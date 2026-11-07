package runtime_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/modelgw"
)

type toolEndpointCall struct {
	Args json.RawMessage
	Key  string
}

type fakeToolEndpoint struct {
	srv      *httptest.Server
	mu       sync.Mutex
	calls    []toolEndpointCall
	failures int
}

func newFakeToolEndpoint(t *testing.T, failures int) *fakeToolEndpoint {
	t.Helper()
	f := &fakeToolEndpoint{failures: failures}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, toolEndpointCall{Args: json.RawMessage(body), Key: r.Header.Get("Idempotency-Key")})
		failed := f.failures > 0
		if failed {
			f.failures--
		}
		f.mu.Unlock()
		if failed {
			http.Error(w, "tool failure", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"found":true}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeToolEndpoint) received() []toolEndpointCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]toolEndpointCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func (e *env) registerTool(t *testing.T, tenant, name, version, endpoint string, isDefault bool) {
	t.Helper()
	status, raw := e.do(t, http.MethodPost, "/v1/tools", tenant, map[string]any{
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
}

type toolResultPayload struct {
	Tool    string          `json:"tool"`
	Version string          `json:"version"`
	Args    json.RawMessage `json:"args"`
	Result  json.RawMessage `json:"result"`
}

func (e *env) runSteps(t *testing.T, tenant, runID string) []stepPayload {
	t.Helper()
	status, raw := e.do(t, http.MethodGet, "/v1/runs/"+runID+"/steps", tenant, nil)
	if status != http.StatusOK {
		t.Fatalf("get steps status = %d body=%s", status, raw)
	}
	var steps []stepPayload
	if err := json.Unmarshal(raw, &steps); err != nil {
		t.Fatalf("decode steps %s: %v", raw, err)
	}
	return steps
}

func TestRunCompletesThroughDefaultToolVersion(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	endpoint := newFakeToolEndpoint(t, 0)
	policy := newPolicyServerFunc(t, secret, func(request policyRequest) string {
		if request.StepIndex == 0 {
			return `{"action":"tool_call","tool":"lookup","args":{"order_id":"A1"}}`
		}
		return `{"action":"done","output":{"answer":"done"}}`
	})
	e := newEnvWithGateway(t, modelgw.New("http://127.0.0.1:1/v1", "", 2*time.Second), 3)
	e.registerTool(t, "tenant-a", "lookup", "1", endpoint.srv.URL, true)

	agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
	run := e.submitRun(t, "tenant-a", agent.ID)

	completed := e.waitRun(t, "tenant-a", run.ID, domain.RunCompleted, 10*time.Second)
	if string(completed.Output) != `{"answer":"done"}` {
		t.Fatalf("completed run output = %s", completed.Output)
	}

	steps := e.runSteps(t, "tenant-a", run.ID)
	if len(steps) != 2 || steps[0].StepType != "tool_call" || steps[1].StepType != "done" {
		t.Fatalf("steps = %+v", steps)
	}
	var toolResult toolResultPayload
	if err := json.Unmarshal(steps[0].Result, &toolResult); err != nil {
		t.Fatalf("decode tool result %s: %v", steps[0].Result, err)
	}
	if toolResult.Tool != "lookup" || toolResult.Version != "1" {
		t.Fatalf("tool result = %+v", toolResult)
	}
	var args struct {
		OrderID string `json:"order_id"`
	}
	if err := json.Unmarshal(toolResult.Args, &args); err != nil || args.OrderID != "A1" {
		t.Fatalf("tool result args = %s (%v)", toolResult.Args, err)
	}
	var result struct {
		Found bool `json:"found"`
	}
	if err := json.Unmarshal(toolResult.Result, &result); err != nil || !result.Found {
		t.Fatalf("tool result payload = %s (%v)", toolResult.Result, err)
	}

	calls := endpoint.received()
	if len(calls) != 1 {
		t.Fatalf("tool endpoint calls = %d, want 1", len(calls))
	}
	if calls[0].Key != run.ID+":0" {
		t.Fatalf("idempotency key = %q, want %q", calls[0].Key, run.ID+":0")
	}
	if !json.Valid(calls[0].Args) || string(calls[0].Args) != `{"order_id":"A1"}` {
		t.Fatalf("tool endpoint args = %s", calls[0].Args)
	}

	var invocationCount int
	var invocationVersion, invocationStatus string
	var invocationArgs, invocationResult json.RawMessage
	if err := e.env.Pool.QueryRow(context.Background(),
		`select version, args, result, status from tool_invocations where run_id::text = $1 and tool_name = 'lookup'`, run.ID,
	).Scan(&invocationVersion, &invocationArgs, &invocationResult, &invocationStatus); err != nil {
		t.Fatalf("read tool invocation: %v", err)
	}
	if invocationVersion != "1" || invocationStatus != "ok" {
		t.Fatalf("tool invocation = version %q status %q", invocationVersion, invocationStatus)
	}
	var invocationPayload struct {
		Found bool `json:"found"`
	}
	if err := json.Unmarshal(invocationResult, &invocationPayload); err != nil || !invocationPayload.Found {
		t.Fatalf("tool invocation result = %s (%v)", invocationResult, err)
	}
	if err := e.env.Pool.QueryRow(context.Background(),
		`select count(*) from tool_invocations where run_id::text = $1`, run.ID).Scan(&invocationCount); err != nil {
		t.Fatalf("count tool invocations: %v", err)
	}
	if invocationCount != 1 {
		t.Fatalf("tool invocation count = %d, want 1", invocationCount)
	}

	var spanName string
	var spanAttempt int
	if err := e.env.Pool.QueryRow(context.Background(),
		`select name, attempt from spans where run_id::text = $1 and step_index = 0`, run.ID).Scan(&spanName, &spanAttempt); err != nil {
		t.Fatalf("read tool span: %v", err)
	}
	if spanName != "tool.lookup" || spanAttempt != 0 {
		t.Fatalf("tool span = %q attempt %d", spanName, spanAttempt)
	}

	requests := policy.snapshot()
	if len(requests) != 2 || len(requests[1].Ledger.Recent) != 1 || requests[1].Ledger.Recent[0].StepType != "tool_call" {
		t.Fatalf("policy requests = %+v", requests)
	}

	e.registerTool(t, "tenant-a", "lookup", "2", endpoint.srv.URL, true)
	pinned := e.runSteps(t, "tenant-a", run.ID)
	var pinnedResult toolResultPayload
	if err := json.Unmarshal(pinned[0].Result, &pinnedResult); err != nil {
		t.Fatalf("decode pinned result %s: %v", pinned[0].Result, err)
	}
	if pinnedResult.Version != "1" {
		t.Fatalf("checkpoint version changed after default swap: %+v", pinnedResult)
	}

	status, raw := e.do(t, http.MethodGet, "/v1/tools", "tenant-a", nil)
	if status != http.StatusOK {
		t.Fatalf("list tools status = %d body=%s", status, raw)
	}
	var tools []domain.Tool
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatalf("decode tools %s: %v", raw, err)
	}
	if len(tools) != 2 || tools[0].Version != "1" || tools[1].Version != "2" ||
		tools[0].IsDefault || !tools[1].IsDefault {
		t.Fatalf("tools = %+v", tools)
	}
}

func TestRunUsesExplicitToolVersion(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	defaultEndpoint := newFakeToolEndpoint(t, 0)
	explicitEndpoint := newFakeToolEndpoint(t, 0)
	policy := newPolicyServerFunc(t, secret, func(request policyRequest) string {
		if request.StepIndex == 0 {
			return `{"action":"tool_call","tool":"lookup","version":"2","args":{"order_id":"A1"}}`
		}
		return `{"action":"done"}`
	})
	e := newEnvWithGateway(t, modelgw.New("http://127.0.0.1:1/v1", "", 2*time.Second), 3)
	e.registerTool(t, "tenant-a", "lookup", "1", defaultEndpoint.srv.URL, true)
	e.registerTool(t, "tenant-a", "lookup", "2", explicitEndpoint.srv.URL, false)

	agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
	run := e.submitRun(t, "tenant-a", agent.ID)
	e.waitRun(t, "tenant-a", run.ID, domain.RunCompleted, 10*time.Second)

	steps := e.runSteps(t, "tenant-a", run.ID)
	var toolResult toolResultPayload
	if err := json.Unmarshal(steps[0].Result, &toolResult); err != nil {
		t.Fatalf("decode tool result %s: %v", steps[0].Result, err)
	}
	if toolResult.Version != "2" {
		t.Fatalf("tool result version = %q, want 2", toolResult.Version)
	}
	if calls := defaultEndpoint.received(); len(calls) != 0 {
		t.Fatalf("default endpoint calls = %d, want 0", len(calls))
	}
	if calls := explicitEndpoint.received(); len(calls) != 1 {
		t.Fatalf("explicit endpoint calls = %d, want 1", len(calls))
	}
}

func TestToolResolutionAndSchemaViolationsFailWithoutCallingEndpoint(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	cases := []struct {
		name       string
		register   bool
		version    string
		args       string
		wantPolicy int
	}{
		{"unknown_tool", false, "", `{"order_id":"A1"}`, 1},
		{"unknown_version", true, `"9"`, `{"order_id":"A1"}`, 1},
		{"schema_violation", true, "", `{"wrong":"x"}`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := newFakeToolEndpoint(t, 0)
			versionField := ""
			if tc.version != "" {
				versionField = `,"version":` + tc.version
			}
			response := `{"action":"tool_call","tool":"lookup"` + versionField + `,"args":` + tc.args + `}`
			policy := newPolicyServer(t, secret, response)
			e := newEnvWithGateway(t, modelgw.New("http://127.0.0.1:1/v1", "", 2*time.Second), 3)
			if tc.register {
				e.registerTool(t, "tenant-a", "lookup", "1", endpoint.srv.URL, true)
			}

			agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
			run := e.submitRun(t, "tenant-a", agent.ID)

			failed := e.waitRun(t, "tenant-a", run.ID, domain.RunFailed, 5*time.Second)
			if failed.TerminalReason == nil || *failed.TerminalReason != domain.ReasonToolSchemaViolation {
				t.Fatalf("terminal reason = %v, want tool_schema_violation", failed.TerminalReason)
			}
			if steps := e.runSteps(t, "tenant-a", run.ID); len(steps) != 0 {
				t.Fatalf("failed run steps = %+v", steps)
			}
			if calls := endpoint.received(); len(calls) != 0 {
				t.Fatalf("tool endpoint calls = %d, want 0", len(calls))
			}
			if got := policy.count(); got != tc.wantPolicy {
				t.Fatalf("policy calls = %d, want %d", got, tc.wantPolicy)
			}
			var invocationCount int
			if err := e.env.Pool.QueryRow(context.Background(),
				`select count(*) from tool_invocations where run_id::text = $1`, run.ID).Scan(&invocationCount); err != nil {
				t.Fatalf("count tool invocations: %v", err)
			}
			if invocationCount != 0 {
				t.Fatalf("tool invocation count = %d, want 0", invocationCount)
			}
		})
	}
}

func TestToolEndpointRetriesReuseIdempotencyKey(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	endpoint := newFakeToolEndpoint(t, 1)
	policy := newPolicyServerFunc(t, secret, func(request policyRequest) string {
		if request.StepIndex == 0 {
			return `{"action":"tool_call","tool":"lookup","args":{"order_id":"A1"}}`
		}
		return `{"action":"done"}`
	})
	e := newEnvWithGateway(t, modelgw.New("http://127.0.0.1:1/v1", "", 2*time.Second), 3)
	e.registerTool(t, "tenant-a", "lookup", "1", endpoint.srv.URL, true)

	agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
	run := e.submitRun(t, "tenant-a", agent.ID)
	e.waitRun(t, "tenant-a", run.ID, domain.RunCompleted, 15*time.Second)

	calls := endpoint.received()
	if len(calls) != 2 {
		t.Fatalf("tool endpoint calls = %d, want 2", len(calls))
	}
	if calls[0].Key != calls[1].Key || calls[0].Key != run.ID+":0" {
		t.Fatalf("idempotency keys = %q / %q, want %q", calls[0].Key, calls[1].Key, run.ID+":0")
	}
	var invocationCount int
	if err := e.env.Pool.QueryRow(context.Background(),
		`select count(*) from tool_invocations where run_id::text = $1`, run.ID).Scan(&invocationCount); err != nil {
		t.Fatalf("count tool invocations: %v", err)
	}
	if invocationCount != 1 {
		t.Fatalf("tool invocation count = %d, want 1", invocationCount)
	}
}

func TestToolEndpointFailureExhaustsRetries(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	endpoint := newFakeToolEndpoint(t, 2)
	policy := newPolicyServer(t, secret, `{"action":"tool_call","tool":"lookup","args":{"order_id":"A1"}}`)
	e := newEnvWithGateway(t, modelgw.New("http://127.0.0.1:1/v1", "", 2*time.Second), 2)
	e.registerTool(t, "tenant-a", "lookup", "1", endpoint.srv.URL, true)

	agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
	run := e.submitRun(t, "tenant-a", agent.ID)

	failed := e.waitRun(t, "tenant-a", run.ID, domain.RunFailed, 10*time.Second)
	if failed.TerminalReason == nil || *failed.TerminalReason != domain.ReasonMaxRetries {
		t.Fatalf("terminal reason = %v, want max_retries", failed.TerminalReason)
	}
	if steps := e.runSteps(t, "tenant-a", run.ID); len(steps) != 0 {
		t.Fatalf("failed run steps = %+v", steps)
	}
	if calls := endpoint.received(); len(calls) != 2 {
		t.Fatalf("tool endpoint calls = %d, want 2", len(calls))
	}
}

func TestRunFailsOnInvalidToolCallAction(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	cases := []struct {
		name     string
		response string
	}{
		{"missing_tool", `{"action":"tool_call","args":{"order_id":"A1"}}`},
		{"tool_not_string", `{"action":"tool_call","tool":7,"args":{"order_id":"A1"}}`},
		{"missing_args", `{"action":"tool_call","tool":"lookup"}`},
		{"args_not_object", `{"action":"tool_call","tool":"lookup","args":[1]}`},
		{"args_null", `{"action":"tool_call","tool":"lookup","args":null}`},
		{"version_not_string", `{"action":"tool_call","tool":"lookup","version":2,"args":{}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := newFakeToolEndpoint(t, 0)
			policy := newPolicyServer(t, secret, tc.response)
			e := newEnvWithGateway(t, modelgw.New("http://127.0.0.1:1/v1", "", 2*time.Second), 3)
			e.registerTool(t, "tenant-a", "lookup", "1", endpoint.srv.URL, true)

			agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
			run := e.submitRun(t, "tenant-a", agent.ID)

			failed := e.waitRun(t, "tenant-a", run.ID, domain.RunFailed, 5*time.Second)
			if failed.TerminalReason == nil || *failed.TerminalReason != domain.ReasonPolicyInvalidAction {
				t.Fatalf("terminal reason = %v, want policy_invalid_action", failed.TerminalReason)
			}
			if calls := endpoint.received(); len(calls) != 0 {
				t.Fatalf("tool endpoint calls = %d, want 0", len(calls))
			}
			if got := policy.count(); got != 1 {
				t.Fatalf("policy calls = %d, want 1", got)
			}
		})
	}
}
