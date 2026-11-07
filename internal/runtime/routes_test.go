package runtime_test

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
	"github.com/romeokeita231/Lite-agent-platform/internal/repository"
)

type routedUpstream struct {
	srv      *httptest.Server
	mu       sync.Mutex
	calls    []string
	handlers map[string]http.HandlerFunc
}

func newRoutedUpstream(t *testing.T, handlers map[string]http.HandlerFunc) *routedUpstream {
	t.Helper()
	u := &routedUpstream{handlers: handlers}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		var request struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		u.mu.Lock()
		u.calls = append(u.calls, request.Model)
		handler := u.handlers[request.Model]
		u.mu.Unlock()
		if handler == nil {
			http.Error(w, "unknown model", http.StatusNotFound)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *routedUpstream) baseURL() string { return u.srv.URL + "/v1" }

func (u *routedUpstream) received() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]string, len(u.calls))
	copy(out, u.calls)
	return out
}

func okModel(content string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`, content)
	}
}

func statusModel(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream failure", status)
	}
}

func (e *env) submitRunWithInput(t *testing.T, tenant, agentID string, input map[string]any) domain.Run {
	t.Helper()
	status, raw := e.do(t, http.MethodPost, "/v1/runs", tenant, map[string]any{
		"agent_id": agentID,
		"input":    input,
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

func (e *env) putRoutes(t *testing.T, tenant string, routes map[string][]string) {
	t.Helper()
	ctx := repository.WithTenant(t.Context(), tenant)
	for label, chain := range routes {
		if _, err := e.store.UpsertModelRoute(ctx, domain.ModelRoute{TenantID: tenant, Label: label, Chain: chain}); err != nil {
			t.Fatalf("upsert route %s: %v", label, err)
		}
	}
}

func (e *env) llmSteps(t *testing.T, tenant, runID string) []stepPayload {
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

func TestRouteLabelsUseTheirOwnChains(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	upstream := newRoutedUpstream(t, map[string]http.HandlerFunc{
		"m1": okModel("m1-answer"),
		"m2": okModel("m2-answer"),
		"m3": okModel("m3-answer"),
		"m4": okModel("m4-answer"),
	})
	policy := newPolicyServerFunc(t, secret, func(request policyRequest) string {
		if request.StepIndex == 0 {
			var input struct {
				Route string `json:"route"`
			}
			_ = json.Unmarshal(request.Input, &input)
			return fmt.Sprintf(`{"action":"llm_call","route":%q,"messages":[{"role":"user","content":"hi"}]}`, input.Route)
		}
		return `{"action":"done","output":{"answer":"done"}}`
	})
	e := newEnvWithRoutes(t, upstream.baseURL(), 3, 2*time.Second, 2*time.Second)
	e.putRoutes(t, "tenant-a", map[string][]string{
		"cheap":   {"m1", "m2"},
		"fast":    {"m3"},
		"default": {"m4"},
	})
	agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")

	cases := []struct {
		route  string
		chain  []string
		model  string
		before int
	}{
		{"route:cheap", []string{"m1", "m2"}, "m1", 0},
		{"route:fast", []string{"m3"}, "m3", 1},
		{"route:default", []string{"m4"}, "m4", 2},
	}
	for _, tc := range cases {
		run := e.submitRunWithInput(t, "tenant-a", agent.ID, map[string]any{"route": tc.route})
		e.waitRun(t, "tenant-a", run.ID, domain.RunCompleted, 10*time.Second)

		calls := upstream.received()
		if len(calls) != tc.before+1 || calls[tc.before] != tc.model {
			t.Fatalf("%s upstream calls = %v, want one call to %s", tc.route, calls, tc.model)
		}

		steps := e.llmSteps(t, "tenant-a", run.ID)
		if len(steps) != 2 {
			t.Fatalf("%s steps = %+v", tc.route, steps)
		}
		var llm llmResultPayload
		if err := json.Unmarshal(steps[0].Result, &llm); err != nil {
			t.Fatalf("decode llm result: %v", err)
		}
		if llm.Route != tc.route || llm.RequestedModel != tc.route || llm.Model != tc.model || llm.FallbackFrom != nil {
			t.Fatalf("%s llm result = %+v", tc.route, llm)
		}
		if len(llm.Chain) != len(tc.chain) {
			t.Fatalf("%s chain = %v", tc.route, llm.Chain)
		}
		for i, model := range tc.chain {
			if llm.Chain[i] != model {
				t.Fatalf("%s chain = %v, want %v", tc.route, llm.Chain, tc.chain)
			}
		}

		var requested, effective, fallbackFrom string
		var chain []string
		if err := e.env.Pool.QueryRow(t.Context(),
			`select requested_model, effective_model, fallback_from, chain
			 from model_call_logs where run_id::text = $1`, run.ID,
		).Scan(&requested, &effective, &fallbackFrom, &chain); err != nil {
			t.Fatalf("read model call log: %v", err)
		}
		if requested != tc.route || effective != tc.model || fallbackFrom != "" || len(chain) != len(tc.chain) {
			t.Fatalf("%s model call log = %q %q %q %v", tc.route, requested, effective, fallbackFrom, chain)
		}
	}
}

func TestRouteFallsBackOn429ButNotOn400(t *testing.T) {
	t.Run("retryable_429", func(t *testing.T) {
		const secret = "runtime-secret"
		t.Setenv("POLICY_SECRET_ALPHA", secret)
		upstream := newRoutedUpstream(t, map[string]http.HandlerFunc{
			"m1": statusModel(http.StatusTooManyRequests),
			"m2": okModel("m2-answer"),
		})
		policy := newPolicyServerFunc(t, secret, func(request policyRequest) string {
			if request.StepIndex == 0 {
				return `{"action":"llm_call","route":"route:cheap","messages":[{"role":"user","content":"hi"}]}`
			}
			return `{"action":"done"}`
		})
		e := newEnvWithRoutes(t, upstream.baseURL(), 3, 2*time.Second, 2*time.Second)
		e.putRoutes(t, "tenant-a", map[string][]string{"cheap": {"m1", "m2"}})
		agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
		run := e.submitRun(t, "tenant-a", agent.ID)
		e.waitRun(t, "tenant-a", run.ID, domain.RunCompleted, 10*time.Second)

		calls := upstream.received()
		if len(calls) != 2 || calls[0] != "m1" || calls[1] != "m2" {
			t.Fatalf("upstream calls = %v", calls)
		}
		steps := e.llmSteps(t, "tenant-a", run.ID)
		var llm llmResultPayload
		if err := json.Unmarshal(steps[0].Result, &llm); err != nil {
			t.Fatalf("decode llm result: %v", err)
		}
		if llm.Model != "m2" || llm.FallbackFrom == nil || *llm.FallbackFrom != "m1" {
			t.Fatalf("llm result = %+v", llm)
		}
		var fallbackFrom string
		if err := e.env.Pool.QueryRow(t.Context(),
			`select fallback_from from model_call_logs where run_id::text = $1`, run.ID,
		).Scan(&fallbackFrom); err != nil {
			t.Fatalf("read model call log: %v", err)
		}
		if fallbackFrom != "m1" {
			t.Fatalf("fallback_from = %q", fallbackFrom)
		}
		var spanAttempts int
		if err := e.env.Pool.QueryRow(t.Context(),
			`select count(*) from spans where run_id::text = $1 and step_index = 0`, run.ID,
		).Scan(&spanAttempts); err != nil {
			t.Fatalf("count spans: %v", err)
		}
		if spanAttempts != 2 {
			t.Fatalf("llm span count = %d, want 2", spanAttempts)
		}
	})

	t.Run("non_retryable_400", func(t *testing.T) {
		const secret = "runtime-secret"
		t.Setenv("POLICY_SECRET_ALPHA", secret)
		upstream := newRoutedUpstream(t, map[string]http.HandlerFunc{
			"m1": statusModel(http.StatusBadRequest),
			"m2": okModel("m2-answer"),
		})
		policy := newPolicyServerFunc(t, secret, func(request policyRequest) string {
			return `{"action":"llm_call","route":"route:cheap","messages":[{"role":"user","content":"hi"}]}`
		})
		e := newEnvWithRoutes(t, upstream.baseURL(), 2, 2*time.Second, 2*time.Second)
		e.putRoutes(t, "tenant-a", map[string][]string{"cheap": {"m1", "m2"}})
		agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
		run := e.submitRun(t, "tenant-a", agent.ID)
		failed := e.waitRun(t, "tenant-a", run.ID, domain.RunFailed, 15*time.Second)
		if failed.TerminalReason == nil || *failed.TerminalReason != domain.ReasonMaxRetries {
			t.Fatalf("terminal reason = %v, want max_retries", failed.TerminalReason)
		}
		calls := upstream.received()
		if len(calls) != 2 || calls[0] != "m1" || calls[1] != "m1" {
			t.Fatalf("upstream calls = %v, want m1 twice", calls)
		}
		for _, call := range calls {
			if call == "m2" {
				t.Fatalf("m2 was called after a 400")
			}
		}
		if steps := e.llmSteps(t, "tenant-a", run.ID); len(steps) != 0 {
			t.Fatalf("failed run steps = %+v", steps)
		}
	})
}

func TestRouteChainExhaustedBy5xxCallsEachModelOnce(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	upstream := newRoutedUpstream(t, map[string]http.HandlerFunc{
		"m1": statusModel(http.StatusInternalServerError),
		"m2": statusModel(http.StatusServiceUnavailable),
	})
	policy := newPolicyServerFunc(t, secret, func(request policyRequest) string {
		return `{"action":"llm_call","route":"route:cheap","messages":[{"role":"user","content":"hi"}]}`
	})
	e := newEnvWithRoutes(t, upstream.baseURL(), 1, 2*time.Second, 2*time.Second)
	e.putRoutes(t, "tenant-a", map[string][]string{"cheap": {"m1", "m2"}})
	agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
	run := e.submitRun(t, "tenant-a", agent.ID)
	failed := e.waitRun(t, "tenant-a", run.ID, domain.RunFailed, 10*time.Second)
	if failed.TerminalReason == nil || *failed.TerminalReason != domain.ReasonMaxRetries {
		t.Fatalf("terminal reason = %v, want max_retries", failed.TerminalReason)
	}
	calls := upstream.received()
	if len(calls) != 2 || calls[0] != "m1" || calls[1] != "m2" {
		t.Fatalf("upstream calls = %v, want m1 then m2 once", calls)
	}
}

func TestModelTimeoutExhaustedReportsStepTimeout(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	upstream := newRoutedUpstream(t, map[string]http.HandlerFunc{
		"m1": func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() },
	})
	policy := newPolicyServerFunc(t, secret, func(request policyRequest) string {
		return `{"action":"llm_call","route":"route:cheap","messages":[{"role":"user","content":"hi"}]}`
	})
	e := newEnvWithRoutes(t, upstream.baseURL(), 1, 200*time.Millisecond, 200*time.Millisecond)
	e.putRoutes(t, "tenant-a", map[string][]string{"cheap": {"m1"}})
	agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
	run := e.submitRun(t, "tenant-a", agent.ID)
	failed := e.waitRun(t, "tenant-a", run.ID, domain.RunFailed, 5*time.Second)
	if failed.TerminalReason == nil || *failed.TerminalReason != domain.ReasonStepTimeout {
		t.Fatalf("terminal reason = %v, want step_timeout", failed.TerminalReason)
	}
}

func TestUnknownRouteFailsWithoutCallingUpstream(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	upstream := newRoutedUpstream(t, map[string]http.HandlerFunc{"m1": okModel("m1-answer")})
	policy := newPolicyServerFunc(t, secret, func(request policyRequest) string {
		return `{"action":"llm_call","route":"route:missing","messages":[{"role":"user","content":"hi"}]}`
	})
	e := newEnvWithRoutes(t, upstream.baseURL(), 3, 2*time.Second, 2*time.Second)
	agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
	run := e.submitRun(t, "tenant-a", agent.ID)
	failed := e.waitRun(t, "tenant-a", run.ID, domain.RunFailed, 5*time.Second)
	if failed.TerminalReason == nil || *failed.TerminalReason != domain.ReasonPolicyInvalidAction {
		t.Fatalf("terminal reason = %v, want policy_invalid_action", failed.TerminalReason)
	}
	if calls := upstream.received(); len(calls) != 0 {
		t.Fatalf("upstream calls = %v, want none", calls)
	}
	if got := policy.count(); got != 1 {
		t.Fatalf("policy calls = %d, want 1", got)
	}
}
