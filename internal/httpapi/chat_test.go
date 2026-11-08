package httpapi_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/httpapi"
	"github.com/romeokeita231/Lite-agent-platform/internal/modelgw"
	"github.com/romeokeita231/Lite-agent-platform/internal/repository"
	"github.com/romeokeita231/Lite-agent-platform/internal/testutil"
	"github.com/romeokeita231/Lite-agent-platform/internal/toolgw"
)

type chatUpstream struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []map[string]json.RawMessage
}

func newChatUpstream(t *testing.T) *chatUpstream {
	t.Helper()
	u := &chatUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		var call map[string]json.RawMessage
		if err := json.Unmarshal(body, &call); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		u.mu.Lock()
		u.calls = append(u.calls, call)
		u.mu.Unlock()
		var model string
		_ = json.Unmarshal(call["model"], &model)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`, model+"-answer")
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *chatUpstream) baseURL() string { return u.srv.URL + "/v1" }

func (u *chatUpstream) received() []map[string]json.RawMessage {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]map[string]json.RawMessage, len(u.calls))
	copy(out, u.calls)
	return out
}

func newModelAPI(t *testing.T, upstreamURL string, budget time.Duration) *apiEnv {
	t.Helper()
	base := testutil.New(t)
	store := repository.New(base.Pool)
	e := &apiEnv{
		env: base,
		key: map[string]string{
			"tenant-a": "key-tenant-a-0123456789",
			"tenant-b": "key-tenant-b-0123456789",
		},
	}
	for tenantID, key := range e.key {
		ctx := repository.WithTenant(t.Context(), tenantID)
		if err := store.CreateTenant(ctx, domain.Tenant{
			ID:           tenantID,
			Name:         tenantID,
			APIKeyHash:   hashHex(key),
			APIKeyPrefix: key[:8],
		}); err != nil {
			t.Fatalf("create tenant %s: %v", tenantID, err)
		}
	}
	gateway := modelgw.NewWithStore(store, upstreamURL, "", budget)
	e.srv = httptest.NewServer(httpapi.NewMux(alwaysReady{}, store, toolgw.New(store, 2*time.Second), alwaysReady{}, nil, gateway))
	t.Cleanup(e.srv.Close)
	return e
}

func putRoute(t *testing.T, e *apiEnv, tenant, label string, chain []string) {
	t.Helper()
	ctx := repository.WithTenant(t.Context(), tenant)
	if _, err := repository.New(e.env.Pool).UpsertModelRoute(ctx, domain.ModelRoute{
		TenantID: tenant,
		Label:    label,
		Chain:    chain,
	}); err != nil {
		t.Fatalf("upsert route %s: %v", label, err)
	}
}

func TestStandaloneChatCompletionUsesRouteAndWritesDetachedTrace(t *testing.T) {
	upstream := newChatUpstream(t)
	api := newModelAPI(t, upstream.baseURL(), 2*time.Second)
	putRoute(t, api, "tenant-a", "default", []string{"m1"})

	status, raw := api.do(t, http.MethodPost, "/v1/chat/completions", "tenant-a", map[string]any{
		"model":       "route:default",
		"messages":    []map[string]string{{"role": "user", "content": "hi"}},
		"temperature": 0.2,
	})
	if status != http.StatusOK {
		t.Fatalf("chat status = %d body=%s", status, raw)
	}
	var response struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
		Lite struct {
			EffectiveModel string   `json:"effective_model"`
			Chain          []string `json:"chain"`
			FallbackFrom   *string  `json:"fallback_from"`
			Usage          struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"lite"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("decode chat response %s: %v", raw, err)
	}
	if response.ID == "" || response.Object != "chat.completion" || response.Created == 0 || response.Model != "m1" {
		t.Fatalf("chat response = %+v", response)
	}
	if len(response.Choices) != 1 || response.Choices[0].Index != 0 ||
		response.Choices[0].Message.Role != "assistant" || response.Choices[0].Message.Content != "m1-answer" ||
		response.Choices[0].FinishReason != "stop" {
		t.Fatalf("chat choices = %+v", response.Choices)
	}
	if response.Usage.PromptTokens != 10 || response.Usage.CompletionTokens != 5 || response.Usage.TotalTokens != 15 {
		t.Fatalf("chat usage = %+v", response.Usage)
	}
	if response.Lite.EffectiveModel != "m1" || len(response.Lite.Chain) != 1 || response.Lite.Chain[0] != "m1" ||
		response.Lite.FallbackFrom != nil || response.Lite.Usage.InputTokens != 10 || response.Lite.Usage.OutputTokens != 5 {
		t.Fatalf("chat lite = %+v", response.Lite)
	}

	calls := upstream.received()
	if len(calls) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(calls))
	}
	var sentModel string
	_ = json.Unmarshal(calls[0]["model"], &sentModel)
	if sentModel != "m1" {
		t.Fatalf("upstream model = %q", sentModel)
	}
	if _, ok := calls[0]["temperature"]; !ok {
		t.Fatalf("upstream request lost temperature: %v", calls[0])
	}

	var spanCount int
	if err := api.env.Pool.QueryRow(t.Context(),
		`select count(*) from spans where tenant_id = 'tenant-a' and name = 'model.m1'`).Scan(&spanCount); err != nil {
		t.Fatalf("count spans: %v", err)
	}
	if spanCount != 1 {
		t.Fatalf("standalone span count = %d, want 1", spanCount)
	}
	var runIDNull bool
	if err := api.env.Pool.QueryRow(t.Context(),
		`select bool_and(run_id is null) from spans where tenant_id = 'tenant-a'`,
	).Scan(&runIDNull); err != nil {
		t.Fatalf("check span run_id: %v", err)
	}
	if !runIDNull {
		t.Fatalf("standalone spans are not all run_id null")
	}

	var logCount int
	if err := api.env.Pool.QueryRow(t.Context(),
		`select count(*) from model_call_logs where tenant_id = 'tenant-a' and run_id is null`,
	).Scan(&logCount); err != nil {
		t.Fatalf("count standalone model call logs: %v", err)
	}
	if logCount != 1 {
		t.Fatalf("standalone model call log count = %d, want 1", logCount)
	}
	var requested, effective string
	var chain []string
	if err := api.env.Pool.QueryRow(t.Context(),
		`select requested_model, effective_model, chain
		 from model_call_logs where tenant_id = 'tenant-a' and run_id is null`,
	).Scan(&requested, &effective, &chain); err != nil {
		t.Fatalf("read standalone model call log: %v", err)
	}
	if requested != "route:default" || effective != "m1" || len(chain) != 1 || chain[0] != "m1" {
		t.Fatalf("standalone log = %q %q %v", requested, effective, chain)
	}
}

func TestStandaloneChatRejectsUnknownModelRoute(t *testing.T) {
	upstream := newChatUpstream(t)
	api := newModelAPI(t, upstream.baseURL(), 2*time.Second)
	status, raw := api.do(t, http.MethodPost, "/v1/chat/completions", "tenant-a", map[string]any{
		"model":    "route:missing",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("unknown route status = %d body=%s", status, raw)
	}
	if body := decodeError(t, raw); body.Error != "bad_request" {
		t.Fatalf("unknown route body = %+v", body)
	}
	if calls := upstream.received(); len(calls) != 0 {
		t.Fatalf("upstream calls = %d, want 0", len(calls))
	}
	var spanCount int
	if err := api.env.Pool.QueryRow(t.Context(), `select count(*) from spans`).Scan(&spanCount); err != nil {
		t.Fatalf("count spans: %v", err)
	}
	if spanCount != 0 {
		t.Fatalf("spans after unknown route = %d", spanCount)
	}
}

func TestStandaloneChatFailureWritesDetachedTrace(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream failure", http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)
	api := newModelAPI(t, upstream.URL+"/v1", 2*time.Second)
	putRoute(t, api, "tenant-a", "default", []string{"m1", "m2"})

	status, raw := api.do(t, http.MethodPost, "/v1/chat/completions", "tenant-a", map[string]any{
		"model":    "route:default",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if status != http.StatusBadGateway {
		t.Fatalf("failed chat status = %d body=%s", status, raw)
	}

	var logCount int
	if err := api.env.Pool.QueryRow(t.Context(),
		`select count(*) from model_call_logs where tenant_id = 'tenant-a' and run_id is null and status = 'error'`,
	).Scan(&logCount); err != nil {
		t.Fatalf("count failed model call logs: %v", err)
	}
	if logCount != 1 {
		t.Fatalf("failed model call log count = %d, want 1", logCount)
	}

	var spanCount int
	var allDetached bool
	if err := api.env.Pool.QueryRow(t.Context(),
		`select count(*), bool_and(run_id is null) from spans where tenant_id = 'tenant-a' and name in ('model.m1', 'model.m2')`,
	).Scan(&spanCount, &allDetached); err != nil {
		t.Fatalf("count failed spans: %v", err)
	}
	if spanCount != 2 || !allDetached {
		t.Fatalf("failed spans = %d detached=%v, want 2 detached", spanCount, allDetached)
	}
}

func TestModelsEndpointListsTenantRoutes(t *testing.T) {
	api := newModelAPI(t, "http://127.0.0.1:1/v1", 2*time.Second)
	putRoute(t, api, "tenant-a", "cheap", []string{"m1", "m2"})
	putRoute(t, api, "tenant-a", "fast", []string{"m3"})
	putRoute(t, api, "tenant-b", "default", []string{"b1"})

	status, raw := api.do(t, http.MethodGet, "/v1/models", "tenant-a", nil)
	if status != http.StatusOK {
		t.Fatalf("models status = %d body=%s", status, raw)
	}
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode models %s: %v", raw, err)
	}
	if list.Object != "list" {
		t.Fatalf("models object = %q", list.Object)
	}
	ids := map[string]bool{}
	for _, entry := range list.Data {
		ids[entry.ID] = true
		if entry.Object != "model" || entry.Created == 0 || entry.OwnedBy != "platform" {
			t.Fatalf("model entry = %+v", entry)
		}
	}
	want := map[string]bool{"m1": true, "m2": true, "m3": true, "route:cheap": true, "route:fast": true}
	if len(ids) != len(want) {
		t.Fatalf("model ids = %v, want %v", ids, want)
	}
	for id := range want {
		if !ids[id] {
			t.Fatalf("models missing %q: %v", id, ids)
		}
	}
	for _, forbidden := range []string{"b1", "route:default"} {
		if ids[forbidden] {
			t.Fatalf("tenant a sees %q", forbidden)
		}
	}

	status, raw = api.do(t, http.MethodGet, "/v1/models", "tenant-b", nil)
	if status != http.StatusOK {
		t.Fatalf("models b status = %d body=%s", status, raw)
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode models b: %v", err)
	}
	if len(list.Data) != 2 {
		t.Fatalf("tenant b models = %+v", list.Data)
	}
}
