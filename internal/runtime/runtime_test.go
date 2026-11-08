package runtime_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/romeokeita231/Lite-agent-platform/internal/observability"
	"github.com/romeokeita231/Lite-agent-platform/internal/repository"
	agentruntime "github.com/romeokeita231/Lite-agent-platform/internal/runtime"
	"github.com/romeokeita231/Lite-agent-platform/internal/testutil"
	"github.com/romeokeita231/Lite-agent-platform/internal/toolgw"
)

type alwaysReady struct{}

func (alwaysReady) Ready() bool { return true }

func (alwaysReady) Draining() bool { return false }

type ledgerStepPayload struct {
	StepIndex int             `json:"step_index"`
	StepType  string          `json:"step_type"`
	Result    json.RawMessage `json:"result"`
}

type policyRequest struct {
	RunID     string          `json:"run_id"`
	Agent     string          `json:"agent"`
	StepIndex int             `json:"step_index"`
	Attempt   int             `json:"attempt"`
	Input     json.RawMessage `json:"input"`
	Ledger    struct {
		Recent  []ledgerStepPayload `json:"recent"`
		Summary string              `json:"summary"`
		Dropped []int               `json:"dropped"`
	} `json:"ledger"`
}

type policyServer struct {
	srv      *httptest.Server
	mu       sync.Mutex
	requests []policyRequest
}

type policyResponder func(policyRequest) string

func newPolicyServer(t *testing.T, secret, response string) *policyServer {
	t.Helper()
	return newPolicyServerFunc(t, secret, func(policyRequest) string { return response })
}

func newPolicyServerFunc(t *testing.T, secret string, respond policyResponder) *policyServer {
	t.Helper()
	p := &policyServer{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "unreadable body", http.StatusUnauthorized)
			return
		}
		timestamp := r.Header.Get("X-Timestamp")
		expected := sign(secret, r.Method, r.URL.Path, body, timestamp)
		if !hmac.Equal([]byte(expected), []byte(r.Header.Get("X-Signature"))) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		var request policyRequest
		if err := json.Unmarshal(body, &request); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		p.mu.Lock()
		p.requests = append(p.requests, request)
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(respond(request)))
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *policyServer) last(t *testing.T) policyRequest {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.requests) == 0 {
		t.Fatalf("policy received no requests")
	}
	return p.requests[len(p.requests)-1]
}

func (p *policyServer) snapshot() []policyRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]policyRequest, len(p.requests))
	copy(out, p.requests)
	return out
}

func (p *policyServer) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

type env struct {
	env            *testutil.Env
	srv            *httptest.Server
	keys           map[string]string
	store          *repository.Store
	gateway        domain.ModelGateway
	tools          domain.ToolGateway
	metrics        *observability.Metrics
	options        agentruntime.Options
	stopDispatcher func()
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWithGateway(t, modelgw.New("http://127.0.0.1:1/v1", "", 2*time.Second), 3)
}

func newEnvWithGateway(t *testing.T, gateway domain.ModelGateway, maxAttempts int) *env {
	t.Helper()
	return newEnvWithOptions(t, gateway, maxAttempts, 2*time.Second)
}

func newEnvWithOptions(t *testing.T, gateway domain.ModelGateway, maxAttempts int, stepTimeout time.Duration) *env {
	t.Helper()
	base := testutil.New(t)
	store := repository.New(base.Pool)
	return newEnvFromStore(t, base, store, gateway, maxAttempts, stepTimeout)
}

func newEnvWithRoutes(t *testing.T, baseURL string, maxAttempts int, stepTimeout, budget time.Duration) *env {
	t.Helper()
	base := testutil.New(t)
	store := repository.New(base.Pool)
	return newEnvFromStore(t, base, store, modelgw.NewWithStore(store, baseURL, "", budget), maxAttempts, stepTimeout)
}

func newEnvFromStore(t *testing.T, base *testutil.Env, store *repository.Store, gateway domain.ModelGateway, maxAttempts int, stepTimeout time.Duration) *env {
	t.Helper()
	return newEnvWithRuntimeOptions(t, base, store, gateway, agentruntime.Options{
		StepTimeout:     stepTimeout,
		MaxStepAttempts: maxAttempts,
		PollInterval:    10 * time.Millisecond,
	})
}

func newEnvWithRuntimeOptions(t *testing.T, base *testutil.Env, store *repository.Store, gateway domain.ModelGateway, options agentruntime.Options) *env {
	t.Helper()
	if options.StepTimeout == 0 {
		options.StepTimeout = 2 * time.Second
	}
	if options.MaxStepAttempts == 0 {
		options.MaxStepAttempts = 3
	}
	if options.PollInterval == 0 {
		options.PollInterval = 10 * time.Millisecond
	}
	tools := toolgw.New(store, options.StepTimeout)
	metrics := observability.NewMetrics()
	tools.SetMetrics(metrics)
	if client, ok := gateway.(interface{ SetMetrics(observability.Recorder) }); ok {
		client.SetMetrics(metrics)
	}
	options.Metrics = metrics
	e := &env{
		env: base,
		keys: map[string]string{
			"tenant-a": "key-tenant-a-0123456789",
			"tenant-b": "key-tenant-b-0123456789",
		},
		store:   store,
		gateway: gateway,
		tools:   tools,
		metrics: metrics,
		options: options,
	}
	for tenantID, key := range e.keys {
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
	e.srv = httptest.NewServer(httpapi.NewMux(alwaysReady{}, store, tools, alwaysReady{}, metrics.Handler(), gateway))
	t.Cleanup(e.srv.Close)

	e.startDispatcher(t)
	return e
}

func (e *env) startDispatcher(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	dispatcher := agentruntime.New(e.store, e.gateway, e.tools, e.options)
	done := make(chan struct{})
	go func() {
		dispatcher.Run(ctx)
		close(done)
	}()
	stop := func() {
		cancel()
		<-done
	}
	e.stopDispatcher = stop
	t.Cleanup(stop)
}

func (e *env) do(t *testing.T, method, path, tenant string, body any) (int, []byte) {
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
	req.Header.Set("Authorization", "Bearer "+e.keys[tenant])
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

func (e *env) createAgent(t *testing.T, tenant, name, policyURL, secretEnv string) domain.Agent {
	t.Helper()
	status, raw := e.do(t, http.MethodPost, "/v1/agents", tenant, map[string]string{
		"name":       name,
		"policy_url": policyURL,
		"secret_env": secretEnv,
	})
	if status != http.StatusCreated {
		t.Fatalf("create agent status = %d body=%s", status, raw)
	}
	var agent domain.Agent
	if err := json.Unmarshal(raw, &agent); err != nil {
		t.Fatalf("decode agent: %v", err)
	}
	return agent
}

func (e *env) submitRun(t *testing.T, tenant, agentID string) domain.Run {
	t.Helper()
	status, raw := e.do(t, http.MethodPost, "/v1/runs", tenant, map[string]any{
		"agent_id": agentID,
		"input":    map[string]string{"order_id": "A1"},
	})
	if status != http.StatusCreated {
		t.Fatalf("create run status = %d body=%s", status, raw)
	}
	var run domain.Run
	if err := json.Unmarshal(raw, &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	return run
}

func (e *env) waitRun(t *testing.T, tenant, id string, want domain.RunStatus, timeout time.Duration) domain.Run {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last domain.Run
	for time.Now().Before(deadline) {
		status, raw := e.do(t, http.MethodGet, "/v1/runs/"+id, tenant, nil)
		if status != http.StatusOK {
			t.Fatalf("get run %s status = %d body=%s", id, status, raw)
		}
		if err := json.Unmarshal(raw, &last); err != nil {
			t.Fatalf("decode run: %v", err)
		}
		if last.Status == want {
			return last
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s status = %s, want %s", id, last.Status, want)
	return last
}

func (e *env) watchRun(t *testing.T, tenant, id string, timeout time.Duration, observe func(domain.Run)) domain.Run {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last domain.Run
	for time.Now().Before(deadline) {
		status, raw := e.do(t, http.MethodGet, "/v1/runs/"+id, tenant, nil)
		if status != http.StatusOK {
			t.Fatalf("get run %s status = %d body=%s", id, status, raw)
		}
		if err := json.Unmarshal(raw, &last); err != nil {
			t.Fatalf("decode run: %v", err)
		}
		observe(last)
		switch last.Status {
		case domain.RunCompleted, domain.RunFailed, domain.RunCancelled:
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach a terminal state: %+v", id, last)
	return last
}

type stepPayload struct {
	StepIndex int             `json:"step_index"`
	StepType  string          `json:"step_type"`
	Result    json.RawMessage `json:"result"`
	CreatedAt time.Time       `json:"created_at"`
}

func TestRunReachesDoneThroughSignedPolicy(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	policy := newPolicyServer(t, secret, `{"action":"done","output":{"answer":"done"}}`)
	e := newEnv(t)

	agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
	run := e.submitRun(t, "tenant-a", agent.ID)

	completed := e.waitRun(t, "tenant-a", run.ID, domain.RunCompleted, 5*time.Second)
	if completed.EndedAt == nil {
		t.Fatalf("completed run has no ended_at: %+v", completed)
	}
	if string(completed.Output) != `{"answer":"done"}` {
		t.Fatalf("completed run output = %s", completed.Output)
	}

	status, raw := e.do(t, http.MethodGet, "/v1/runs/"+run.ID+"/steps", "tenant-a", nil)
	if status != http.StatusOK {
		t.Fatalf("get steps status = %d body=%s", status, raw)
	}
	var steps []stepPayload
	if err := json.Unmarshal(raw, &steps); err != nil {
		t.Fatalf("decode steps %s: %v", raw, err)
	}
	if len(steps) != 1 || steps[0].StepIndex != 0 || steps[0].StepType != "done" {
		t.Fatalf("steps = %+v", steps)
	}
	if string(steps[0].Result) != `{"output":{"answer":"done"}}` {
		t.Fatalf("step result = %s", steps[0].Result)
	}
	if steps[0].CreatedAt.IsZero() {
		t.Fatalf("step created_at is zero")
	}

	status, raw = e.do(t, http.MethodGet, "/v1/runs/"+run.ID+"/steps", "tenant-b", nil)
	if status != http.StatusNotFound {
		t.Fatalf("cross-tenant steps status = %d body=%s", status, raw)
	}

	request := policy.last(t)
	if request.RunID != run.ID || request.Agent != "alpha" || request.StepIndex != 0 || request.Attempt != 0 {
		t.Fatalf("policy request = %+v", request)
	}
	var input struct {
		OrderID string `json:"order_id"`
	}
	if err := json.Unmarshal(request.Input, &input); err != nil || input.OrderID != "A1" {
		t.Fatalf("policy request input = %s (%v)", request.Input, err)
	}
	if len(request.Ledger.Recent) != 0 || request.Ledger.Summary != "" || len(request.Ledger.Dropped) != 0 {
		t.Fatalf("policy ledger = %+v", request.Ledger)
	}
}

func TestRunFailsOnInvalidPolicyAction(t *testing.T) {
	cases := []struct {
		name     string
		response string
	}{
		{"unknown_action", `{"action":"bogus"}`},
		{"missing_action", `{}`},
		{"wrong_output_type", `{"action":"done","output":"nope"}`},
		{"human_review_missing_prompt", `{"action":"human_review"}`},
		{"human_review_empty_prompt", `{"action":"human_review","prompt":""}`},
		{"human_review_non_string_prompt", `{"action":"human_review","prompt":7}`},
		{"human_review_deadline_not_number", `{"action":"human_review","prompt":"approve?","deadline_seconds":"soon"}`},
		{"human_review_deadline_zero", `{"action":"human_review","prompt":"approve?","deadline_seconds":0}`},
		{"human_review_deadline_negative", `{"action":"human_review","prompt":"approve?","deadline_seconds":-5}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const secret = "runtime-secret"
			t.Setenv("POLICY_SECRET_ALPHA", secret)
			policy := newPolicyServer(t, secret, tc.response)
			e := newEnv(t)

			agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
			run := e.submitRun(t, "tenant-a", agent.ID)

			failed := e.waitRun(t, "tenant-a", run.ID, domain.RunFailed, 5*time.Second)
			if failed.TerminalReason == nil || *failed.TerminalReason != domain.ReasonPolicyInvalidAction {
				t.Fatalf("terminal reason = %v, want policy_invalid_action", failed.TerminalReason)
			}

			status, raw := e.do(t, http.MethodGet, "/v1/runs/"+run.ID+"/steps", "tenant-a", nil)
			if status != http.StatusOK {
				t.Fatalf("get steps status = %d body=%s", status, raw)
			}
			var steps []stepPayload
			if err := json.Unmarshal(raw, &steps); err != nil {
				t.Fatalf("decode steps %s: %v", raw, err)
			}
			if len(steps) != 0 {
				t.Fatalf("failed run steps = %+v", steps)
			}
			if got := policy.count(); got != 1 {
				t.Fatalf("policy calls = %d, want 1", got)
			}
		})
	}
}

type upstreamCall struct {
	Path     string
	Model    string                `json:"model"`
	Messages []domain.ModelMessage `json:"messages"`
}

type fakeUpstream struct {
	srv    *httptest.Server
	mu     sync.Mutex
	calls  []upstreamCall
	status int
}

func newFakeUpstream(t *testing.T, status int) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{status: status}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		var call upstreamCall
		if err := json.Unmarshal(body, &call); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		call.Path = r.URL.Path
		f.mu.Lock()
		f.calls = append(f.calls, call)
		f.mu.Unlock()
		if f.status < 200 || f.status >= 300 {
			http.Error(w, "upstream failure", f.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"42"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) baseURL() string {
	return f.srv.URL + "/v1"
}

func (f *fakeUpstream) received() []upstreamCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]upstreamCall, len(f.calls))
	copy(out, f.calls)
	return out
}

type llmResultPayload struct {
	Route          string   `json:"route"`
	RequestedModel string   `json:"requested_model"`
	Model          string   `json:"model"`
	Chain          []string `json:"chain"`
	FallbackFrom   *string  `json:"fallback_from"`
	Content        string   `json:"content"`
	Usage          struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func TestRunCompletesThroughLLMCall(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	upstream := newFakeUpstream(t, http.StatusOK)
	policy := newPolicyServerFunc(t, secret, func(request policyRequest) string {
		if request.StepIndex == 0 {
			return `{"action":"llm_call","route":"route:default","messages":[{"role":"user","content":"hi"}]}`
		}
		return `{"action":"done","output":{"answer":"done"}}`
	})
	e := newEnvWithGateway(t, modelgw.New(upstream.baseURL(), "", 2*time.Second), 3)

	agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
	run := e.submitRun(t, "tenant-a", agent.ID)

	completed := e.waitRun(t, "tenant-a", run.ID, domain.RunCompleted, 10*time.Second)
	if string(completed.Output) != `{"answer":"done"}` {
		t.Fatalf("completed run output = %s", completed.Output)
	}

	status, raw := e.do(t, http.MethodGet, "/v1/runs/"+run.ID+"/steps", "tenant-a", nil)
	if status != http.StatusOK {
		t.Fatalf("get steps status = %d body=%s", status, raw)
	}
	var steps []stepPayload
	if err := json.Unmarshal(raw, &steps); err != nil {
		t.Fatalf("decode steps %s: %v", raw, err)
	}
	if len(steps) != 2 || steps[0].StepIndex != 0 || steps[0].StepType != "llm_call" ||
		steps[1].StepIndex != 1 || steps[1].StepType != "done" {
		t.Fatalf("steps = %+v", steps)
	}
	var llm llmResultPayload
	if err := json.Unmarshal(steps[0].Result, &llm); err != nil {
		t.Fatalf("decode llm result %s: %v", steps[0].Result, err)
	}
	if llm.Route != "route:default" || llm.RequestedModel != "route:default" || llm.Model != "route:default" {
		t.Fatalf("llm result models = %+v", llm)
	}
	if len(llm.Chain) != 1 || llm.Chain[0] != "route:default" {
		t.Fatalf("llm chain = %+v", llm.Chain)
	}
	if llm.FallbackFrom != nil {
		t.Fatalf("llm fallback_from = %v", *llm.FallbackFrom)
	}
	if llm.Content != "42" || llm.Usage.InputTokens != 10 || llm.Usage.OutputTokens != 5 {
		t.Fatalf("llm content/usage = %+v", llm)
	}

	second := policy.last(t)
	if second.StepIndex != 1 || len(second.Ledger.Recent) != 1 || second.Ledger.Recent[0].StepType != "llm_call" {
		t.Fatalf("second policy request = %+v", second)
	}

	calls := upstream.received()
	if len(calls) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(calls))
	}
	if calls[0].Path != "/v1/chat/completions" || calls[0].Model != "route:default" {
		t.Fatalf("upstream call = %+v", calls[0])
	}
	if len(calls[0].Messages) != 1 || calls[0].Messages[0].Role != "user" || calls[0].Messages[0].Content != "hi" {
		t.Fatalf("upstream messages = %+v", calls[0].Messages)
	}

	var spanCount int
	if err := e.env.Pool.QueryRow(context.Background(),
		`select count(*) from spans where run_id::text = $1`, run.ID).Scan(&spanCount); err != nil {
		t.Fatalf("count spans: %v", err)
	}
	if spanCount != 2 {
		t.Fatalf("span count = %d, want 2", spanCount)
	}
	var spanName string
	var attributes json.RawMessage
	if err := e.env.Pool.QueryRow(context.Background(),
		`select name, attributes from spans where run_id::text = $1 and step_index = 0`, run.ID).Scan(&spanName, &attributes); err != nil {
		t.Fatalf("read llm span: %v", err)
	}
	if spanName != "model.route:default" {
		t.Fatalf("llm span name = %q", spanName)
	}
	var attrs map[string]any
	if err := json.Unmarshal(attributes, &attrs); err != nil {
		t.Fatalf("decode span attributes %s: %v", attributes, err)
	}
	for _, key := range []string{"requested_model", "effective_model", "status", "latency_ms", "attempt"} {
		if _, ok := attrs[key]; !ok {
			t.Fatalf("span attributes missing %q: %s", key, attributes)
		}
	}

	var logCount int
	if err := e.env.Pool.QueryRow(context.Background(),
		`select count(*) from model_call_logs where run_id::text = $1`, run.ID).Scan(&logCount); err != nil {
		t.Fatalf("count model call logs: %v", err)
	}
	if logCount != 1 {
		t.Fatalf("model call log count = %d, want 1", logCount)
	}
	var routeLabel, requested, effective, callStatus string
	var inputTokens, outputTokens int
	var latencyMS int64
	if err := e.env.Pool.QueryRow(context.Background(),
		`select route_label, requested_model, effective_model, input_tokens, output_tokens, latency_ms, status
		 from model_call_logs where run_id::text = $1`, run.ID,
	).Scan(&routeLabel, &requested, &effective, &inputTokens, &outputTokens, &latencyMS, &callStatus); err != nil {
		t.Fatalf("read model call log: %v", err)
	}
	if routeLabel != "route:default" || requested != "route:default" || effective != "route:default" {
		t.Fatalf("model call log models = %q/%q/%q", routeLabel, requested, effective)
	}
	if inputTokens != 10 || outputTokens != 5 || latencyMS < 0 || callStatus != domain.ModelStatusOK {
		t.Fatalf("model call log = tokens %d/%d latency %d status %q", inputTokens, outputTokens, latencyMS, callStatus)
	}
}

func TestRunFailsWhenUpstreamReturns5xx(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	upstream := newFakeUpstream(t, http.StatusInternalServerError)
	policy := newPolicyServer(t, secret,
		`{"action":"llm_call","route":"route:default","messages":[{"role":"user","content":"hi"}]}`)
	e := newEnvWithGateway(t, modelgw.New(upstream.baseURL(), "", 2*time.Second), 2)

	agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
	run := e.submitRun(t, "tenant-a", agent.ID)

	failed := e.waitRun(t, "tenant-a", run.ID, domain.RunFailed, 15*time.Second)
	if failed.TerminalReason == nil || *failed.TerminalReason != domain.ReasonMaxRetries {
		t.Fatalf("terminal reason = %v, want max_retries", failed.TerminalReason)
	}

	status, raw := e.do(t, http.MethodGet, "/v1/runs/"+run.ID+"/steps", "tenant-a", nil)
	if status != http.StatusOK {
		t.Fatalf("get steps status = %d body=%s", status, raw)
	}
	var steps []stepPayload
	if err := json.Unmarshal(raw, &steps); err != nil {
		t.Fatalf("decode steps %s: %v", raw, err)
	}
	if len(steps) != 0 {
		t.Fatalf("failed run steps = %+v", steps)
	}

	if calls := upstream.received(); len(calls) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(calls))
	}

	rows, err := e.env.Pool.Query(context.Background(),
		`select requested_model, input_tokens, output_tokens, latency_ms, status
		 from model_call_logs where run_id::text = $1 order by created_at`, run.ID)
	if err != nil {
		t.Fatalf("query model call logs: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var requested, callStatus string
		var inputTokens, outputTokens int
		var latencyMS int64
		if err := rows.Scan(&requested, &inputTokens, &outputTokens, &latencyMS, &callStatus); err != nil {
			t.Fatalf("scan model call log: %v", err)
		}
		count++
		if requested != "route:default" || inputTokens != 0 || outputTokens != 0 || latencyMS < 0 || callStatus != domain.ModelStatusError {
			t.Fatalf("failed model call log = %q %d/%d %d %q", requested, inputTokens, outputTokens, latencyMS, callStatus)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate model call logs: %v", err)
	}
	if count != 2 {
		t.Fatalf("failed model call log count = %d, want 2", count)
	}
}

func TestRunFailsOnInvalidLLMCallAction(t *testing.T) {
	cases := []struct {
		name     string
		response string
	}{
		{"missing_route", `{"action":"llm_call","messages":[{"role":"user","content":"hi"}]}`},
		{"empty_messages", `{"action":"llm_call","route":"route:default","messages":[]}`},
		{"messages_not_array", `{"action":"llm_call","route":"route:default","messages":"hi"}`},
		{"message_missing_content", `{"action":"llm_call","route":"route:default","messages":[{"role":"user"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const secret = "runtime-secret"
			t.Setenv("POLICY_SECRET_ALPHA", secret)
			upstream := newFakeUpstream(t, http.StatusOK)
			policy := newPolicyServer(t, secret, tc.response)
			e := newEnvWithGateway(t, modelgw.New(upstream.baseURL(), "", 2*time.Second), 3)

			agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
			run := e.submitRun(t, "tenant-a", agent.ID)

			failed := e.waitRun(t, "tenant-a", run.ID, domain.RunFailed, 5*time.Second)
			if failed.TerminalReason == nil || *failed.TerminalReason != domain.ReasonPolicyInvalidAction {
				t.Fatalf("terminal reason = %v, want policy_invalid_action", failed.TerminalReason)
			}

			status, raw := e.do(t, http.MethodGet, "/v1/runs/"+run.ID+"/steps", "tenant-a", nil)
			if status != http.StatusOK {
				t.Fatalf("get steps status = %d body=%s", status, raw)
			}
			var steps []stepPayload
			if err := json.Unmarshal(raw, &steps); err != nil {
				t.Fatalf("decode steps %s: %v", raw, err)
			}
			if len(steps) != 0 {
				t.Fatalf("failed run steps = %+v", steps)
			}
			if calls := upstream.received(); len(calls) != 0 {
				t.Fatalf("upstream calls = %d, want 0", len(calls))
			}
			var logCount int
			if err := e.env.Pool.QueryRow(context.Background(),
				`select count(*) from model_call_logs where run_id::text = $1`, run.ID).Scan(&logCount); err != nil {
				t.Fatalf("count model call logs: %v", err)
			}
			if logCount != 0 {
				t.Fatalf("model call log count = %d, want 0", logCount)
			}
		})
	}
}

func TestRunRetriesWithBackoffThenFailsPolicyUnreachable(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	unreachable := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	policyURL := unreachable.URL
	unreachable.Close()
	e := newEnvWithOptions(t, modelgw.New("http://127.0.0.1:1/v1", "", time.Second), 3, time.Second)

	agent := e.createAgent(t, "tenant-a", "alpha", policyURL, "POLICY_SECRET_ALPHA")
	run := e.submitRun(t, "tenant-a", agent.ID)

	statuses := map[domain.RunStatus]bool{}
	seen := map[int]bool{}
	failed := e.watchRun(t, "tenant-a", run.ID, 10*time.Second, func(r domain.Run) {
		statuses[r.Status] = true
		if r.Attempt >= 1 && !seen[1] {
			seen[1] = true
			if r.NextAttemptAt == nil {
				t.Fatalf("attempt 1 has no next_attempt_at: %+v", r)
			}
			if wait := time.Until(*r.NextAttemptAt); wait < 500*time.Millisecond || wait > 1200*time.Millisecond {
				t.Fatalf("first backoff = %s, want about 1s", wait)
			}
		}
		if r.Attempt >= 2 && !seen[2] {
			seen[2] = true
			if r.NextAttemptAt == nil {
				t.Fatalf("attempt 2 has no next_attempt_at: %+v", r)
			}
			if wait := time.Until(*r.NextAttemptAt); wait < 1500*time.Millisecond || wait > 2300*time.Millisecond {
				t.Fatalf("second backoff = %s, want about 2s", wait)
			}
		}
	})
	if failed.TerminalReason == nil || *failed.TerminalReason != domain.ReasonPolicyUnreachable {
		t.Fatalf("terminal reason = %v, want policy_unreachable", failed.TerminalReason)
	}
	if failed.EndedAt == nil {
		t.Fatalf("failed run has no ended_at: %+v", failed)
	}
	if !statuses[domain.RunRunning] {
		t.Fatalf("run never observed running: %v", statuses)
	}
	for status := range statuses {
		if status != domain.RunQueued && status != domain.RunRunning && status != domain.RunFailed {
			t.Fatalf("unexpected run status %q", status)
		}
	}
	if !seen[1] || !seen[2] {
		t.Fatalf("retry attempts not observed: %v", seen)
	}
}

func TestDecisionTimeoutRetriesThenFailsMaxRetries(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	policy := newPolicyServerFunc(t, secret, func(policyRequest) string {
		time.Sleep(400 * time.Millisecond)
		return `{"action":"done"}`
	})
	e := newEnvWithOptions(t, modelgw.New("http://127.0.0.1:1/v1", "", time.Second), 2, 200*time.Millisecond)

	agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
	run := e.submitRun(t, "tenant-a", agent.ID)

	statuses := map[domain.RunStatus]bool{}
	failed := e.watchRun(t, "tenant-a", run.ID, 5*time.Second, func(r domain.Run) {
		statuses[r.Status] = true
	})
	if failed.TerminalReason == nil || *failed.TerminalReason != domain.ReasonMaxRetries {
		t.Fatalf("terminal reason = %v, want max_retries", failed.TerminalReason)
	}
	for status := range statuses {
		if status != domain.RunQueued && status != domain.RunRunning && status != domain.RunFailed {
			t.Fatalf("unexpected run status %q", status)
		}
	}
	if got := policy.count(); got != 2 {
		t.Fatalf("policy calls = %d, want 2", got)
	}
}

func TestAttemptResetsWhenNewStepBegins(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	upstream := newFakeUpstream(t, http.StatusOK)
	policy := newPolicyServerFunc(t, secret, func(request policyRequest) string {
		if request.StepIndex == 0 {
			return `{"action":"llm_call","route":"route:default","messages":[{"role":"user","content":"hi"}]}`
		}
		return `{"action":"done","output":{}}`
	})
	e := newEnvWithGateway(t, modelgw.New(upstream.baseURL(), "", 2*time.Second), 3)

	agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
	run := e.submitRun(t, "tenant-a", agent.ID)
	e.waitRun(t, "tenant-a", run.ID, domain.RunCompleted, 10*time.Second)

	requests := policy.snapshot()
	if len(requests) != 2 {
		t.Fatalf("policy requests = %d, want 2", len(requests))
	}
	if requests[0].StepIndex != 0 || requests[0].Attempt != 0 {
		t.Fatalf("first policy request = %+v", requests[0])
	}
	if requests[1].StepIndex != 1 || requests[1].Attempt != 0 {
		t.Fatalf("second policy request = %+v", requests[1])
	}
}

func hashHex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func sign(secret, method, path string, body []byte, timestamp string) string {
	sum := sha256.Sum256(body)
	payload := method + "\n" + path + "\n" + hex.EncodeToString(sum[:]) + "\n" + timestamp
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}
