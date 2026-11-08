package httpapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/httpapi"
	"github.com/romeokeita231/Lite-agent-platform/internal/repository"
	"github.com/romeokeita231/Lite-agent-platform/internal/testutil"
	"github.com/romeokeita231/Lite-agent-platform/internal/toolgw"
)

type alwaysReady struct{}

func (alwaysReady) Ready() bool { return true }

func (alwaysReady) Draining() bool { return false }

type apiEnv struct {
	env *testutil.Env
	srv *httptest.Server
	key map[string]string
}

func newAPI(t *testing.T) *apiEnv {
	t.Helper()
	env := testutil.New(t)
	store := repository.New(env.Pool)
	env2 := &apiEnv{
		env: env,
		key: map[string]string{
			"tenant-a": "key-tenant-a-0123456789",
			"tenant-b": "key-tenant-b-0123456789",
		},
	}
	for tenantID, key := range env2.key {
		ctx := repository.WithTenant(context.Background(), tenantID)
		if err := store.CreateTenant(ctx, domain.Tenant{
			ID:           tenantID,
			Name:         tenantID,
			APIKeyHash:   hashHex(key),
			APIKeyPrefix: key[:8],
		}); err != nil {
			t.Fatalf("create tenant %s: %v", tenantID, err)
		}
	}
	env2.srv = httptest.NewServer(httpapi.NewMux(alwaysReady{}, store, toolgw.New(store, 2*time.Second), alwaysReady{}, nil))
	t.Cleanup(env2.srv.Close)
	return env2
}

type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func (e *apiEnv) do(t *testing.T, method, path, tenant string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if tenant != "" {
		req.Header.Set("Authorization", "Bearer "+e.key[tenant])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("%s %s content-type = %q", method, path, got)
	}
	return resp.StatusCode, raw
}

func decodeError(t *testing.T, raw []byte) errorBody {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode error body %s: %v", raw, err)
	}
	return body
}

func TestAPIAuthentication(t *testing.T) {
	api := newAPI(t)

	if status, raw := api.do(t, http.MethodGet, "/v1/agents", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("missing key status = %d body=%s", status, raw)
	} else if body := decodeError(t, raw); body.Error != "unauthorized" || body.Message == "" {
		t.Fatalf("missing key body = %+v", body)
	}

	req, err := http.NewRequest(http.MethodGet, api.srv.URL+"/v1/agents", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer not-a-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown key status = %d body=%s", resp.StatusCode, raw)
	}
	if body := decodeError(t, raw); body.Error != "unauthorized" {
		t.Fatalf("unknown key body = %+v", body)
	}

	if status, raw := api.do(t, http.MethodGet, "/v1/agents", "tenant-a", nil); status != http.StatusOK {
		t.Fatalf("valid key status = %d body=%s", status, raw)
	}
}

func TestAgentRegistrationAndListAreTenantScoped(t *testing.T) {
	api := newAPI(t)

	status, raw := api.do(t, http.MethodPost, "/v1/agents", "tenant-a", map[string]string{
		"name":       "alpha",
		"policy_url": "http://policy-a",
		"secret_env": "POLICY_SECRET_ALPHA",
	})
	if status != http.StatusCreated {
		t.Fatalf("create agent status = %d body=%s", status, raw)
	}
	var created domain.Agent
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode agent: %v", err)
	}
	if created.ID == "" || created.TenantID != "tenant-a" || created.Name != "alpha" ||
		created.PolicyURL != "http://policy-a" || created.SecretEnv != "POLICY_SECRET_ALPHA" {
		t.Fatalf("created agent = %+v", created)
	}

	status, raw = api.do(t, http.MethodGet, "/v1/agents", "tenant-a", nil)
	if status != http.StatusOK {
		t.Fatalf("list agents a status = %d body=%s", status, raw)
	}
	var agentsA []domain.Agent
	if err := json.Unmarshal(raw, &agentsA); err != nil {
		t.Fatalf("decode agents a: %v", err)
	}
	if len(agentsA) != 1 || agentsA[0].ID != created.ID {
		t.Fatalf("agents a = %+v", agentsA)
	}

	status, raw = api.do(t, http.MethodGet, "/v1/agents", "tenant-b", nil)
	if status != http.StatusOK {
		t.Fatalf("list agents b status = %d body=%s", status, raw)
	}
	var agentsB []domain.Agent
	if err := json.Unmarshal(raw, &agentsB); err != nil {
		t.Fatalf("decode agents b: %v", err)
	}
	if len(agentsB) != 0 {
		t.Fatalf("tenant b sees agents = %+v", agentsB)
	}

	status, raw = api.do(t, http.MethodPost, "/v1/agents", "tenant-a", map[string]string{
		"name":       "alpha",
		"policy_url": "http://policy-a2",
		"secret_env": "POLICY_SECRET_ALPHA",
	})
	if status != http.StatusConflict {
		t.Fatalf("duplicate agent status = %d body=%s", status, raw)
	}
	if body := decodeError(t, raw); body.Error != "conflict" {
		t.Fatalf("duplicate agent body = %+v", body)
	}

	status, raw = api.do(t, http.MethodPost, "/v1/agents", "tenant-a", map[string]string{
		"name": "alpha",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("invalid agent status = %d body=%s", status, raw)
	}
	if body := decodeError(t, raw); body.Error != "bad_request" {
		t.Fatalf("invalid agent body = %+v", body)
	}
}

func TestRunSubmissionAndQueryAreTenantScoped(t *testing.T) {
	api := newAPI(t)

	status, raw := api.do(t, http.MethodPost, "/v1/agents", "tenant-a", map[string]string{
		"name":       "alpha",
		"policy_url": "http://policy-a",
		"secret_env": "POLICY_SECRET_ALPHA",
	})
	if status != http.StatusCreated {
		t.Fatalf("create agent status = %d body=%s", status, raw)
	}
	var agent domain.Agent
	if err := json.Unmarshal(raw, &agent); err != nil {
		t.Fatalf("decode agent: %v", err)
	}

	status, raw = api.do(t, http.MethodPost, "/v1/runs", "tenant-a", map[string]any{
		"agent_id": agent.ID,
		"input":    map[string]string{"order_id": "A1"},
	})
	if status != http.StatusCreated {
		t.Fatalf("create run status = %d body=%s", status, raw)
	}
	var created domain.Run
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if created.ID == "" || created.TenantID != "tenant-a" || created.AgentID != agent.ID {
		t.Fatalf("created run = %+v", created)
	}
	if created.Status != domain.RunQueued || created.Attempt != 0 {
		t.Fatalf("created run status/attempt = %s/%d", created.Status, created.Attempt)
	}
	var input struct {
		OrderID string `json:"order_id"`
	}
	if err := json.Unmarshal(created.Input, &input); err != nil || input.OrderID != "A1" {
		t.Fatalf("created run input = %s (%v)", created.Input, err)
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Fatalf("created run timestamps are zero")
	}

	status, raw = api.do(t, http.MethodGet, "/v1/runs/"+created.ID, "tenant-a", nil)
	if status != http.StatusOK {
		t.Fatalf("get run status = %d body=%s", status, raw)
	}
	var fetched domain.Run
	if err := json.Unmarshal(raw, &fetched); err != nil {
		t.Fatalf("decode fetched run: %v", err)
	}
	if fetched.ID != created.ID || fetched.Status != domain.RunQueued ||
		fetched.TenantID != "tenant-a" || fetched.AgentID != agent.ID {
		t.Fatalf("fetched run = %+v", fetched)
	}
	if fetched.TerminalReason != nil || fetched.EndedAt != nil || !rawNull(fetched.Output) || !rawNull(fetched.PendingStep) {
		t.Fatalf("queued run has terminal fields: %+v", fetched)
	}
	if !bytes.Contains(raw, []byte(`"terminal_reason":null`)) {
		t.Fatalf("fetched run body lacks terminal_reason: %s", raw)
	}

	status, raw = api.do(t, http.MethodGet, "/v1/runs/"+created.ID, "tenant-b", nil)
	if status != http.StatusNotFound {
		t.Fatalf("cross-tenant get status = %d body=%s", status, raw)
	}
	if body := decodeError(t, raw); body.Error != "not_found" {
		t.Fatalf("cross-tenant get body = %+v", body)
	}

	status, raw = api.do(t, http.MethodGet, "/v1/runs/00000000-0000-4000-8000-000000000000", "tenant-a", nil)
	if status != http.StatusNotFound {
		t.Fatalf("missing run status = %d body=%s", status, raw)
	}

	status, raw = api.do(t, http.MethodPost, "/v1/runs", "tenant-a", map[string]any{
		"input": map[string]string{"order_id": "A2"},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("run without agent status = %d body=%s", status, raw)
	}
	if body := decodeError(t, raw); body.Error != "bad_request" {
		t.Fatalf("run without agent body = %+v", body)
	}
}

func rawNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

type httpToolCall struct {
	Args json.RawMessage
	Key  string
}

type httpToolServer struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []httpToolCall
}

func newHTTPToolServer(t *testing.T) *httpToolServer {
	t.Helper()
	f := &httpToolServer{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, httpToolCall{Args: json.RawMessage(body), Key: r.Header.Get("Idempotency-Key")})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *httpToolServer) received() []httpToolCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]httpToolCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func registerTool(t *testing.T, api *apiEnv, tenant, name, version, endpoint string, isDefault bool) domain.Tool {
	t.Helper()
	status, raw := api.do(t, http.MethodPost, "/v1/tools", tenant, map[string]any{
		"tool_name": name,
		"version":   version,
		"schema": map[string]any{
			"type":     "object",
			"required": []string{"order_id"},
			"properties": map[string]any{
				"order_id": map[string]string{"type": "string"},
			},
		},
		"endpoint":   endpoint,
		"is_default": isDefault,
	})
	if status != http.StatusCreated {
		t.Fatalf("register tool status = %d body=%s", status, raw)
	}
	var tool domain.Tool
	if err := json.Unmarshal(raw, &tool); err != nil {
		t.Fatalf("decode tool: %v", err)
	}
	return tool
}

func TestToolRegistrationListAndInvocationAreTenantScoped(t *testing.T) {
	api := newAPI(t)
	toolServer := newHTTPToolServer(t)
	tool := registerTool(t, api, "tenant-a", "lookup", "1", toolServer.srv.URL, true)
	if tool.ToolName != "lookup" || tool.Version != "1" || !tool.IsDefault || tool.Endpoint != toolServer.srv.URL {
		t.Fatalf("registered tool = %+v", tool)
	}

	status, raw := api.do(t, http.MethodGet, "/v1/tools", "tenant-a", nil)
	if status != http.StatusOK {
		t.Fatalf("list tools a status = %d body=%s", status, raw)
	}
	var toolsA []domain.Tool
	if err := json.Unmarshal(raw, &toolsA); err != nil {
		t.Fatalf("decode tools a: %v", err)
	}
	if len(toolsA) != 1 || toolsA[0].ToolName != "lookup" || toolsA[0].Version != "1" {
		t.Fatalf("tools a = %+v", toolsA)
	}

	status, raw = api.do(t, http.MethodGet, "/v1/tools", "tenant-b", nil)
	if status != http.StatusOK {
		t.Fatalf("list tools b status = %d body=%s", status, raw)
	}
	var toolsB []domain.Tool
	if err := json.Unmarshal(raw, &toolsB); err != nil {
		t.Fatalf("decode tools b: %v", err)
	}
	if len(toolsB) != 0 {
		t.Fatalf("tenant b sees tools = %+v", toolsB)
	}

	status, raw = api.do(t, http.MethodPost, "/v1/tools/lookup/invoke", "tenant-a", map[string]any{
		"args": map[string]string{"order_id": "A1"},
	})
	if status != http.StatusOK {
		t.Fatalf("invoke status = %d body=%s", status, raw)
	}
	var invoked struct {
		Tool    string          `json:"tool"`
		Version string          `json:"version"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &invoked); err != nil {
		t.Fatalf("decode invoke response: %v", err)
	}
	if invoked.Tool != "lookup" || invoked.Version != "1" || string(invoked.Result) != `{"ok":true}` {
		t.Fatalf("invoke response = %+v", invoked)
	}
	calls := toolServer.received()
	if len(calls) != 1 {
		t.Fatalf("tool endpoint calls = %d, want 1", len(calls))
	}
	if string(calls[0].Args) != `{"order_id":"A1"}` {
		t.Fatalf("tool endpoint args = %s", calls[0].Args)
	}
	if calls[0].Key != "" {
		t.Fatalf("invoke sent idempotency key %q", calls[0].Key)
	}

	status, raw = api.do(t, http.MethodPost, "/v1/tools/lookup/invoke", "tenant-a", map[string]any{
		"args": map[string]string{"wrong": "x"},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("schema violation status = %d body=%s", status, raw)
	}
	if body := decodeError(t, raw); body.Error != "bad_request" {
		t.Fatalf("schema violation body = %+v", body)
	}
	if calls := toolServer.received(); len(calls) != 1 {
		t.Fatalf("schema violation called endpoint: %d calls", len(calls))
	}

	status, raw = api.do(t, http.MethodPost, "/v1/tools/lookup/invoke", "tenant-a", map[string]any{
		"version": "9",
		"args":    map[string]string{"order_id": "A1"},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("unknown version status = %d body=%s", status, raw)
	}

	status, raw = api.do(t, http.MethodPost, "/v1/tools/lookup/invoke", "tenant-b", map[string]any{
		"args": map[string]string{"order_id": "A1"},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("cross-tenant invoke status = %d body=%s", status, raw)
	}
	if body := decodeError(t, raw); body.Error != "bad_request" {
		t.Fatalf("cross-tenant invoke body = %+v", body)
	}

	status, raw = api.do(t, http.MethodPost, "/v1/tools", "tenant-a", map[string]any{
		"tool_name": "lookup",
		"version":   "1",
		"schema":    map[string]any{"type": "object"},
		"endpoint":  toolServer.srv.URL,
	})
	if status != http.StatusConflict {
		t.Fatalf("duplicate tool status = %d body=%s", status, raw)
	}
	if body := decodeError(t, raw); body.Error != "conflict" {
		t.Fatalf("duplicate tool body = %+v", body)
	}

	status, raw = api.do(t, http.MethodPost, "/v1/tools", "tenant-a", map[string]any{
		"tool_name": "lookup",
		"schema":    map[string]any{"type": "object"},
		"endpoint":  toolServer.srv.URL,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("tool without version status = %d body=%s", status, raw)
	}
}

func TestToolDefaultVersionIsExclusiveAndUsedByInvoke(t *testing.T) {
	api := newAPI(t)
	v1Server := newHTTPToolServer(t)
	v2Server := newHTTPToolServer(t)
	registerTool(t, api, "tenant-a", "lookup", "1", v1Server.srv.URL, true)
	registerTool(t, api, "tenant-a", "lookup", "2", v2Server.srv.URL, true)

	status, raw := api.do(t, http.MethodGet, "/v1/tools", "tenant-a", nil)
	if status != http.StatusOK {
		t.Fatalf("list tools status = %d body=%s", status, raw)
	}
	var tools []domain.Tool
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatalf("decode tools: %v", err)
	}
	if len(tools) != 2 || tools[0].Version != "1" || tools[0].IsDefault ||
		tools[1].Version != "2" || !tools[1].IsDefault {
		t.Fatalf("tools = %+v", tools)
	}

	status, raw = api.do(t, http.MethodPost, "/v1/tools/lookup/invoke", "tenant-a", map[string]any{
		"args": map[string]string{"order_id": "A1"},
	})
	if status != http.StatusOK {
		t.Fatalf("invoke status = %d body=%s", status, raw)
	}
	var invoked struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &invoked); err != nil {
		t.Fatalf("decode invoke: %v", err)
	}
	if invoked.Version != "2" {
		t.Fatalf("invoke version = %q, want 2", invoked.Version)
	}
	if calls := v1Server.received(); len(calls) != 0 {
		t.Fatalf("v1 calls = %d, want 0", len(calls))
	}
	if calls := v2Server.received(); len(calls) != 1 {
		t.Fatalf("v2 calls = %d, want 1", len(calls))
	}

	_, err := api.env.Pool.Exec(context.Background(),
		`insert into tool_registry (tenant_id, tool_name, version, "schema", endpoint, is_default)
		 values ('tenant-a', 'lookup', '3', '{}', 'http://example', true)`)
	if err == nil {
		t.Fatalf("database allowed a second default version for the same tool")
	}
}

func hashHex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
