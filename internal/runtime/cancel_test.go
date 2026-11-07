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
	"github.com/romeokeita231/Lite-agent-platform/internal/modelgw"
)

func (e *env) cancelRun(t *testing.T, tenant, runID string) (int, []byte) {
	t.Helper()
	return e.do(t, http.MethodPost, "/v1/runs/"+runID+"/cancel", tenant, nil)
}

func (e *env) doUnauthenticated(t *testing.T, method, path string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
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

type blockingUpstream struct {
	srv     *httptest.Server
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingUpstream(t *testing.T) *blockingUpstream {
	t.Helper()
	b := &blockingUpstream{started: make(chan struct{}), release: make(chan struct{})}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		b.once.Do(func() { close(b.started) })
		select {
		case <-b.release:
		case <-r.Context().Done():
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"42"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func TestCancelDuringInflightLLMCallKeepsCheckpointAndStopsLedger(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	upstream := newBlockingUpstream(t)
	policy := newPolicyServerFunc(t, secret, func(request policyRequest) string {
		if request.StepIndex == 0 {
			return `{"action":"llm_call","route":"route:default","messages":[{"role":"user","content":"hi"}]}`
		}
		return `{"action":"done","output":{"answer":"done"}}`
	})
	e := newEnvWithOptions(t, modelgw.New(upstream.srv.URL+"/v1", "", 10*time.Second), 3, 3*time.Second)

	agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
	run := e.submitRun(t, "tenant-a", agent.ID)

	select {
	case <-upstream.started:
	case <-time.After(5 * time.Second):
		t.Fatalf("model upstream was never called")
	}

	status, raw := e.cancelRun(t, "tenant-a", run.ID)
	if status != http.StatusOK {
		t.Fatalf("cancel status = %d body=%s", status, raw)
	}
	var requested domain.Run
	if err := json.Unmarshal(raw, &requested); err != nil {
		t.Fatalf("decode cancelled run %s: %v", raw, err)
	}
	if requested.Status != domain.RunRunning {
		t.Fatalf("run during inflight cancel = %+v", requested)
	}

	close(upstream.release)

	cancelled := e.waitRun(t, "tenant-a", run.ID, domain.RunCancelled, 10*time.Second)
	if cancelled.TerminalReason == nil || *cancelled.TerminalReason != domain.ReasonCancelledByTenant {
		t.Fatalf("terminal reason = %v, want cancelled_by_tenant", cancelled.TerminalReason)
	}
	if cancelled.EndedAt == nil {
		t.Fatalf("cancelled run has no ended_at: %+v", cancelled)
	}
	if cancelled.Output != nil && string(cancelled.Output) != "null" {
		t.Fatalf("cancelled run output = %s", cancelled.Output)
	}

	steps := e.runSteps(t, "tenant-a", run.ID)
	if len(steps) != 1 || steps[0].StepType != "llm_call" || steps[0].StepIndex != 0 {
		t.Fatalf("steps after cancel = %+v", steps)
	}
	if got := policy.count(); got != 1 {
		t.Fatalf("policy calls = %d, want 1", got)
	}
	time.Sleep(300 * time.Millisecond)
	if got := policy.count(); got != 1 {
		t.Fatalf("policy calls after cancel = %d, want 1", got)
	}
	if steps := e.runSteps(t, "tenant-a", run.ID); len(steps) != 1 {
		t.Fatalf("steps grew after cancel: %+v", steps)
	}
}

func TestCancelQueuedRunSkipsPolicy(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	policy := newPolicyServer(t, secret, `{"action":"done","output":{"answer":"done"}}`)
	e := newEnvWithGateway(t, modelgw.New("http://127.0.0.1:1/v1", "", 2*time.Second), 3)

	agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
	e.stopDispatcher()
	run := e.submitRun(t, "tenant-a", agent.ID)

	status, raw := e.cancelRun(t, "tenant-a", run.ID)
	if status != http.StatusOK {
		t.Fatalf("cancel status = %d body=%s", status, raw)
	}
	var requested domain.Run
	if err := json.Unmarshal(raw, &requested); err != nil {
		t.Fatalf("decode cancelled run %s: %v", raw, err)
	}
	if requested.Status != domain.RunQueued {
		t.Fatalf("queued cancel result = %+v", requested)
	}

	e.startDispatcher(t)
	cancelled := e.waitRun(t, "tenant-a", run.ID, domain.RunCancelled, 5*time.Second)
	if cancelled.TerminalReason == nil || *cancelled.TerminalReason != domain.ReasonCancelledByTenant {
		t.Fatalf("terminal reason = %v, want cancelled_by_tenant", cancelled.TerminalReason)
	}
	if cancelled.Output != nil && string(cancelled.Output) != "null" {
		t.Fatalf("cancelled run output = %s", cancelled.Output)
	}
	time.Sleep(200 * time.Millisecond)
	if got := policy.count(); got != 0 {
		t.Fatalf("policy calls = %d, want 0", got)
	}
	if steps := e.runSteps(t, "tenant-a", run.ID); len(steps) != 0 {
		t.Fatalf("cancelled queued run steps = %+v", steps)
	}
}

func TestCancelWaitingHumanRunIsImmediate(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	policy := newHumanReviewPolicy(t, secret)
	e := newEnvWithGateway(t, modelgw.New("http://127.0.0.1:1/v1", "", 2*time.Second), 3)

	agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
	run := e.submitRun(t, "tenant-a", agent.ID)
	e.waitRun(t, "tenant-a", run.ID, domain.RunWaitingHuman, 5*time.Second)

	status, raw := e.cancelRun(t, "tenant-a", run.ID)
	if status != http.StatusOK {
		t.Fatalf("cancel status = %d body=%s", status, raw)
	}
	var cancelled domain.Run
	if err := json.Unmarshal(raw, &cancelled); err != nil {
		t.Fatalf("decode cancelled run %s: %v", raw, err)
	}
	if cancelled.Status != domain.RunCancelled || cancelled.EndedAt == nil ||
		cancelled.TerminalReason == nil || *cancelled.TerminalReason != domain.ReasonCancelledByTenant {
		t.Fatalf("cancelled waiting run = %+v", cancelled)
	}
	if cancelled.HumanPrompt != "" || cancelled.Deadline != nil ||
		(len(cancelled.PendingStep) != 0 && string(cancelled.PendingStep) != "null") {
		t.Fatalf("cancelled waiting run kept human fields: %+v", cancelled)
	}

	status, raw = e.submitHumanInput(t, "tenant-a", run.ID, 0, map[string]any{"approved": true})
	if status != http.StatusConflict {
		t.Fatalf("human input after cancel status = %d body=%s", status, raw)
	}
	if body := decodeConflict(t, raw); body.Error != "conflict" {
		t.Fatalf("human input after cancel body = %+v", body)
	}

	time.Sleep(200 * time.Millisecond)
	if got := policy.count(); got != 1 {
		t.Fatalf("policy calls = %d, want 1", got)
	}
	if steps := e.runSteps(t, "tenant-a", run.ID); len(steps) != 0 {
		t.Fatalf("cancelled waiting run steps = %+v", steps)
	}
}

func TestCancelTerminalRunsReturnsConflict(t *testing.T) {
	testCases := []struct {
		name    string
		setup   func(t *testing.T, secret string) (*env, domain.Run)
		waitFor domain.RunStatus
	}{
		{
			name: "completed",
			setup: func(t *testing.T, secret string) (*env, domain.Run) {
				t.Setenv("POLICY_SECRET_ALPHA", secret)
				policy := newPolicyServer(t, secret, `{"action":"done","output":{"answer":"done"}}`)
				e := newEnvWithGateway(t, modelgw.New("http://127.0.0.1:1/v1", "", 2*time.Second), 3)
				agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
				return e, e.submitRun(t, "tenant-a", agent.ID)
			},
			waitFor: domain.RunCompleted,
		},
		{
			name: "failed",
			setup: func(t *testing.T, secret string) (*env, domain.Run) {
				t.Setenv("POLICY_SECRET_ALPHA", secret)
				policy := newPolicyServer(t, secret, `{"action":"bogus"}`)
				e := newEnvWithGateway(t, modelgw.New("http://127.0.0.1:1/v1", "", 2*time.Second), 3)
				agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
				return e, e.submitRun(t, "tenant-a", agent.ID)
			},
			waitFor: domain.RunFailed,
		},
		{
			name: "cancelled",
			setup: func(t *testing.T, secret string) (*env, domain.Run) {
				t.Setenv("POLICY_SECRET_ALPHA", secret)
				policy := newHumanReviewPolicy(t, secret)
				e := newEnvWithGateway(t, modelgw.New("http://127.0.0.1:1/v1", "", 2*time.Second), 3)
				agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
				run := e.submitRun(t, "tenant-a", agent.ID)
				e.waitRun(t, "tenant-a", run.ID, domain.RunWaitingHuman, 5*time.Second)
				if status, raw := e.cancelRun(t, "tenant-a", run.ID); status != http.StatusOK {
					t.Fatalf("first cancel status = %d body=%s", status, raw)
				}
				return e, run
			},
			waitFor: domain.RunCancelled,
		},
	}
	const secret = "runtime-secret"
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			e, run := tc.setup(t, secret)
			e.waitRun(t, "tenant-a", run.ID, tc.waitFor, 10*time.Second)
			status, raw := e.cancelRun(t, "tenant-a", run.ID)
			if status != http.StatusConflict {
				t.Fatalf("cancel %s run status = %d body=%s", tc.name, status, raw)
			}
			if body := decodeConflict(t, raw); body.Error != "conflict" || body.Message == "" {
				t.Fatalf("cancel %s run body = %+v", tc.name, body)
			}
		})
	}
}

func TestCancelRunNotFoundAndUnauthenticated(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	policy := newHumanReviewPolicy(t, secret)
	e := newEnvWithGateway(t, modelgw.New("http://127.0.0.1:1/v1", "", 2*time.Second), 3)

	agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
	run := e.submitRun(t, "tenant-a", agent.ID)
	e.waitRun(t, "tenant-a", run.ID, domain.RunWaitingHuman, 5*time.Second)

	status, raw := e.cancelRun(t, "tenant-b", run.ID)
	if status != http.StatusNotFound {
		t.Fatalf("cross-tenant cancel status = %d body=%s", status, raw)
	}
	if body := decodeConflict(t, raw); body.Error != "not_found" {
		t.Fatalf("cross-tenant cancel body = %+v", body)
	}

	status, raw = e.cancelRun(t, "tenant-a", "00000000-0000-4000-8000-000000000000")
	if status != http.StatusNotFound {
		t.Fatalf("missing cancel status = %d body=%s", status, raw)
	}

	status, raw = e.doUnauthenticated(t, http.MethodPost, "/v1/runs/"+run.ID+"/cancel")
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated cancel status = %d body=%s", status, raw)
	}
	if body := decodeConflict(t, raw); body.Error != "unauthorized" {
		t.Fatalf("unauthenticated cancel body = %+v", body)
	}

	status, raw = e.cancelRun(t, "tenant-a", run.ID)
	if status != http.StatusOK {
		t.Fatalf("owner cancel status = %d body=%s", status, raw)
	}
	var cancelled domain.Run
	if err := json.Unmarshal(raw, &cancelled); err != nil {
		t.Fatalf("decode owner cancel %s: %v", raw, err)
	}
	if cancelled.Status != domain.RunCancelled {
		t.Fatalf("owner cancel result = %+v", cancelled)
	}
}
