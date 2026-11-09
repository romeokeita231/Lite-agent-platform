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
	env2.srv = httptest.NewServer(httpapi.NewMux(alwaysReady{}, store, toolgw.New(store, 2*time.Second), alwaysReady{}))
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

func createAgentForTenant(t *testing.T, api *apiEnv, tenant, name string) domain.Agent {
	t.Helper()
	status, raw := api.do(t, http.MethodPost, "/v1/agents", tenant, map[string]string{
		"name":       name,
		"policy_url": "http://policy-" + name,
		"secret_env": "POLICY_SECRET_" + name,
	})
	if status != http.StatusCreated {
		t.Fatalf("create agent %s status = %d body=%s", name, status, raw)
	}
	var agent domain.Agent
	if err := json.Unmarshal(raw, &agent); err != nil {
		t.Fatalf("decode agent %s: %v", name, err)
	}
	return agent
}

func createRunForAgent(t *testing.T, api *apiEnv, tenant, agentID string) domain.Run {
	t.Helper()
	status, raw := api.do(t, http.MethodPost, "/v1/runs", tenant, map[string]any{
		"agent_id": agentID,
		"input":    map[string]string{"order_id": "A1"},
	})
	if status != http.StatusCreated {
		t.Fatalf("create run for %s status = %d body=%s", tenant, status, raw)
	}
	var run domain.Run
	if err := json.Unmarshal(raw, &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	return run
}

type runListBody struct {
	Runs   []domain.Run     `json:"runs"`
	Counts map[string]int64 `json:"counts"`
}

func decodeRunList(t *testing.T, raw []byte) runListBody {
	t.Helper()
	var body runListBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode run list %s: %v", raw, err)
	}
	return body
}

func TestRunListIsTenantScopedFilteredAndCounted(t *testing.T) {
	api := newAPI(t)
	agentA1 := createAgentForTenant(t, api, "tenant-a", "alpha")
	agentA2 := createAgentForTenant(t, api, "tenant-a", "beta")
	agentB := createAgentForTenant(t, api, "tenant-b", "gamma")

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seed := []struct {
		tenant string
		agent  string
		status domain.RunStatus
		offset time.Duration
	}{
		{"tenant-a", agentA1.ID, domain.RunQueued, time.Minute},
		{"tenant-a", agentA1.ID, domain.RunCompleted, 2 * time.Minute},
		{"tenant-a", agentA2.ID, domain.RunRunning, 3 * time.Minute},
		{"tenant-b", agentB.ID, domain.RunFailed, 4 * time.Minute},
	}
	created := make([]domain.Run, len(seed))
	for i, item := range seed {
		created[i] = createRunForAgent(t, api, item.tenant, item.agent)
		if _, err := api.env.Pool.Exec(context.Background(),
			`update runs set status = $2, created_at = $3, updated_at = $3 where id::text = $1`,
			created[i].ID, string(item.status), base.Add(item.offset)); err != nil {
			t.Fatalf("stamp run %d: %v", i, err)
		}
	}

	status, raw := api.do(t, http.MethodGet, "/v1/runs", "tenant-a", nil)
	if status != http.StatusOK {
		t.Fatalf("list runs status = %d body=%s", status, raw)
	}
	body := decodeRunList(t, raw)
	if len(body.Runs) != 3 {
		t.Fatalf("tenant a runs = %+v", body.Runs)
	}
	wantOrder := []string{created[2].ID, created[1].ID, created[0].ID}
	for i, id := range wantOrder {
		if body.Runs[i].ID != id || body.Runs[i].TenantID != "tenant-a" {
			t.Fatalf("run %d = %+v, want id %s", i, body.Runs[i], id)
		}
	}
	wantCounts := map[string]int64{
		"queued":        1,
		"completed":     1,
		"running":       1,
		"failed":        0,
		"waiting_human": 0,
		"cancelled":     0,
	}
	for name, expected := range wantCounts {
		if body.Counts[name] != expected {
			t.Fatalf("counts[%s] = %d, want %d (counts=%+v)", name, body.Counts[name], expected, body.Counts)
		}
	}
	if len(body.Counts) != len(wantCounts) {
		t.Fatalf("counts keys = %+v", body.Counts)
	}
	if !bytes.Contains(raw, []byte(`"output":null`)) {
		t.Fatalf("run list lacks run fields: %s", raw)
	}

	status, raw = api.do(t, http.MethodGet, "/v1/runs?status=completed", "tenant-a", nil)
	if status != http.StatusOK {
		t.Fatalf("list completed status = %d body=%s", status, raw)
	}
	body = decodeRunList(t, raw)
	if len(body.Runs) != 1 || body.Runs[0].ID != created[1].ID {
		t.Fatalf("completed runs = %+v", body.Runs)
	}
	if body.Counts["queued"] != 1 || body.Counts["running"] != 1 {
		t.Fatalf("status filter changed counts = %+v", body.Counts)
	}

	status, raw = api.do(t, http.MethodGet, "/v1/runs?agent_id="+agentA1.ID, "tenant-a", nil)
	if status != http.StatusOK {
		t.Fatalf("list agent runs status = %d body=%s", status, raw)
	}
	body = decodeRunList(t, raw)
	if len(body.Runs) != 2 || body.Runs[0].ID != created[1].ID || body.Runs[1].ID != created[0].ID {
		t.Fatalf("agent runs = %+v", body.Runs)
	}

	status, raw = api.do(t, http.MethodGet, "/v1/runs?status=queued&agent_id="+agentA1.ID, "tenant-a", nil)
	if status != http.StatusOK {
		t.Fatalf("list filtered runs status = %d body=%s", status, raw)
	}
	body = decodeRunList(t, raw)
	if len(body.Runs) != 1 || body.Runs[0].ID != created[0].ID {
		t.Fatalf("filtered runs = %+v", body.Runs)
	}

	status, raw = api.do(t, http.MethodGet, "/v1/runs?limit=2", "tenant-a", nil)
	if status != http.StatusOK {
		t.Fatalf("list limited status = %d body=%s", status, raw)
	}
	body = decodeRunList(t, raw)
	if len(body.Runs) != 2 || body.Runs[0].ID != created[2].ID || body.Runs[1].ID != created[1].ID {
		t.Fatalf("limited runs = %+v", body.Runs)
	}

	status, raw = api.do(t, http.MethodGet, "/v1/runs?status=cancelled", "tenant-a", nil)
	if status != http.StatusOK {
		t.Fatalf("list empty status = %d body=%s", status, raw)
	}
	if body = decodeRunList(t, raw); len(body.Runs) != 0 || body.Runs == nil {
		t.Fatalf("empty runs = %#v", body.Runs)
	}

	status, raw = api.do(t, http.MethodGet, "/v1/runs", "tenant-b", nil)
	if status != http.StatusOK {
		t.Fatalf("list tenant b status = %d body=%s", status, raw)
	}
	body = decodeRunList(t, raw)
	if len(body.Runs) != 1 || body.Runs[0].ID != created[3].ID {
		t.Fatalf("tenant b runs = %+v", body.Runs)
	}
	if body.Counts["failed"] != 1 || body.Counts["queued"] != 0 || body.Counts["completed"] != 0 {
		t.Fatalf("tenant b counts = %+v", body.Counts)
	}

	for _, query := range []string{
		"?status=done",
		"?status=RUNNING",
		"?limit=0",
		"?limit=-1",
		"?limit=101",
		"?limit=abc",
	} {
		status, raw := api.do(t, http.MethodGet, "/v1/runs"+query, "tenant-a", nil)
		if status != http.StatusBadRequest {
			t.Fatalf("%s status = %d body=%s", query, status, raw)
		}
		if errBody := decodeError(t, raw); errBody.Error != "bad_request" {
			t.Fatalf("%s body = %+v", query, errBody)
		}
	}

	if status, raw := api.do(t, http.MethodGet, "/v1/runs", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list status = %d body=%s", status, raw)
	} else if errBody := decodeError(t, raw); errBody.Error != "unauthorized" {
		t.Fatalf("unauthenticated list body = %+v", errBody)
	}
}

func hashHex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
