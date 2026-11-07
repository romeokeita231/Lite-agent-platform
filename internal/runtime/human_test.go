package runtime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/modelgw"
)

func newHumanReviewPolicy(t *testing.T, secret string) *policyServer {
	t.Helper()
	return newPolicyServerFunc(t, secret, func(request policyRequest) string {
		if request.StepIndex == 0 {
			return `{"action":"human_review","prompt":"approve?","deadline_seconds":60}`
		}
		return `{"action":"done","output":{"answer":"done"}}`
	})
}

type pendingStepPayload struct {
	Action          string   `json:"action"`
	Prompt          string   `json:"prompt"`
	DeadlineSeconds *float64 `json:"deadline_seconds"`
}

type humanStepResult struct {
	Input json.RawMessage `json:"input"`
}

func (e *env) submitHumanInput(t *testing.T, tenant, runID string, stepIndex int, input any) (int, []byte) {
	t.Helper()
	return e.do(t, http.MethodPost, "/v1/runs/"+runID+"/human-input", tenant, map[string]any{
		"step_index": stepIndex,
		"input":      input,
	})
}

type humanErrorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func decodeConflict(t *testing.T, raw []byte) humanErrorBody {
	t.Helper()
	var body humanErrorBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode error body %s: %v", raw, err)
	}
	return body
}

func TestHumanReviewWaitsForInputThenResumes(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	policy := newHumanReviewPolicy(t, secret)
	e := newEnvWithGateway(t, modelgw.New("http://127.0.0.1:1/v1", "", 2*time.Second), 3)

	agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
	run := e.submitRun(t, "tenant-a", agent.ID)

	waiting := e.waitRun(t, "tenant-a", run.ID, domain.RunWaitingHuman, 5*time.Second)
	if waiting.HumanPrompt != "approve?" {
		t.Fatalf("human prompt = %q", waiting.HumanPrompt)
	}
	if waiting.PendingStep == nil {
		t.Fatalf("waiting run has no pending_step: %+v", waiting)
	}
	var pending pendingStepPayload
	if err := json.Unmarshal(waiting.PendingStep, &pending); err != nil {
		t.Fatalf("decode pending step %s: %v", waiting.PendingStep, err)
	}
	if pending.Action != "human_review" || pending.Prompt != "approve?" ||
		pending.DeadlineSeconds == nil || *pending.DeadlineSeconds != 60 {
		t.Fatalf("pending step = %+v", pending)
	}
	if waiting.Deadline == nil {
		t.Fatalf("waiting run has no deadline: %+v", waiting)
	}
	if remaining := time.Until(*waiting.Deadline); remaining < 50*time.Second || remaining > 65*time.Second {
		t.Fatalf("deadline is %s away, want about 60s", remaining)
	}
	if steps := e.runSteps(t, "tenant-a", run.ID); len(steps) != 0 {
		t.Fatalf("waiting run steps = %+v", steps)
	}

	time.Sleep(300 * time.Millisecond)
	if got := policy.count(); got != 1 {
		t.Fatalf("policy calls while waiting = %d, want 1", got)
	}

	if status, raw := e.submitHumanInput(t, "tenant-b", run.ID, 0, map[string]any{"approved": true}); status != http.StatusNotFound {
		t.Fatalf("cross-tenant human input status = %d body=%s", status, raw)
	}

	status, raw := e.submitHumanInput(t, "tenant-a", run.ID, 0, map[string]any{"approved": true})
	if status != http.StatusOK {
		t.Fatalf("human input status = %d body=%s", status, raw)
	}
	var queued domain.Run
	if err := json.Unmarshal(raw, &queued); err != nil {
		t.Fatalf("decode submitted run %s: %v", raw, err)
	}
	if queued.Status != domain.RunQueued || queued.HumanPrompt != "" ||
		(len(queued.PendingStep) != 0 && string(queued.PendingStep) != "null") || queued.Deadline != nil {
		t.Fatalf("run after human input = %+v", queued)
	}

	completed := e.waitRun(t, "tenant-a", run.ID, domain.RunCompleted, 5*time.Second)
	if string(completed.Output) != `{"answer":"done"}` {
		t.Fatalf("completed run output = %s", completed.Output)
	}

	steps := e.runSteps(t, "tenant-a", run.ID)
	if len(steps) != 2 || steps[0].StepType != "human_review" || steps[1].StepType != "done" {
		t.Fatalf("steps = %+v", steps)
	}
	var result humanStepResult
	if err := json.Unmarshal(steps[0].Result, &result); err != nil {
		t.Fatalf("decode human result %s: %v", steps[0].Result, err)
	}
	var input struct {
		Approved bool `json:"approved"`
	}
	if err := json.Unmarshal(result.Input, &input); err != nil || !input.Approved {
		t.Fatalf("human input result = %s (%v)", result.Input, err)
	}

	requests := policy.snapshot()
	if len(requests) != 2 {
		t.Fatalf("policy calls = %d, want 2", len(requests))
	}
	if requests[1].StepIndex != 1 {
		t.Fatalf("resumed policy step_index = %d, want 1", requests[1].StepIndex)
	}
	if len(requests[1].Ledger.Recent) != 1 || requests[1].Ledger.Recent[0].StepType != "human_review" {
		t.Fatalf("resumed policy ledger = %+v", requests[1].Ledger)
	}
	var ledgerResult humanStepResult
	if err := json.Unmarshal(requests[1].Ledger.Recent[0].Result, &ledgerResult); err != nil {
		t.Fatalf("decode ledger result: %v", err)
	}
	if string(ledgerResult.Input) != string(result.Input) {
		t.Fatalf("ledger input = %s, want %s", ledgerResult.Input, result.Input)
	}

	var spanName string
	var spanAttempt int
	if err := e.env.Pool.QueryRow(context.Background(),
		`select name, attempt from spans where run_id::text = $1 and step_index = 0`, run.ID,
	).Scan(&spanName, &spanAttempt); err != nil {
		t.Fatalf("read human span: %v", err)
	}
	if spanName != "step.human_review" || spanAttempt != 0 {
		t.Fatalf("human span = %q attempt %d", spanName, spanAttempt)
	}
}

func TestHumanInputRejectsMismatchDuplicateAndDoubleClick(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	policy := newHumanReviewPolicy(t, secret)
	e := newEnvWithGateway(t, modelgw.New("http://127.0.0.1:1/v1", "", 2*time.Second), 3)

	agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
	run := e.submitRun(t, "tenant-a", agent.ID)
	e.waitRun(t, "tenant-a", run.ID, domain.RunWaitingHuman, 5*time.Second)

	status, raw := e.submitHumanInput(t, "tenant-a", run.ID, 1, map[string]any{"approved": true})
	if status != http.StatusConflict {
		t.Fatalf("mismatched step status = %d body=%s", status, raw)
	}
	if body := decodeConflict(t, raw); body.Error != "conflict" || body.Message == "" {
		t.Fatalf("mismatched step body = %+v", body)
	}

	type clickResult struct {
		status int
		err    error
	}
	results := make(chan clickResult, 2)
	for i := 0; i < 2; i++ {
		go func() {
			payload, err := json.Marshal(map[string]any{"step_index": 0, "input": map[string]any{"approved": true}})
			if err != nil {
				results <- clickResult{err: err}
				return
			}
			req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/v1/runs/"+run.ID+"/human-input", bytes.NewReader(payload))
			if err != nil {
				results <- clickResult{err: err}
				return
			}
			req.Header.Set("Authorization", "Bearer "+e.keys["tenant-a"])
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				results <- clickResult{err: err}
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
			results <- clickResult{status: resp.StatusCode}
		}()
	}
	okCount, conflictCount := 0, 0
	for i := 0; i < 2; i++ {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent human input: %v", result.err)
		}
		switch result.status {
		case http.StatusOK:
			okCount++
		case http.StatusConflict:
			conflictCount++
		default:
			t.Fatalf("concurrent human input status = %d", result.status)
		}
	}
	if okCount != 1 || conflictCount != 1 {
		t.Fatalf("double click results = %d ok / %d conflict, want 1/1", okCount, conflictCount)
	}

	e.waitRun(t, "tenant-a", run.ID, domain.RunCompleted, 5*time.Second)
	if steps := e.runSteps(t, "tenant-a", run.ID); len(steps) != 2 || steps[0].StepType != "human_review" {
		t.Fatalf("steps after double click = %+v", steps)
	}
	status, raw = e.submitHumanInput(t, "tenant-a", run.ID, 0, map[string]any{"approved": true})
	if status != http.StatusConflict {
		t.Fatalf("duplicate after completion status = %d body=%s", status, raw)
	}
}

func TestHumanReviewExpiresToStepTimeout(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	policy := newHumanReviewPolicy(t, secret)
	e := newEnvWithGateway(t, modelgw.New("http://127.0.0.1:1/v1", "", 2*time.Second), 3)

	agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
	run := e.submitRun(t, "tenant-a", agent.ID)
	e.waitRun(t, "tenant-a", run.ID, domain.RunWaitingHuman, 5*time.Second)

	if _, err := e.env.Pool.Exec(context.Background(),
		`update runs set deadline = now() - interval '1 second' where id::text = $1`, run.ID); err != nil {
		t.Fatalf("expire deadline: %v", err)
	}

	failed := e.waitRun(t, "tenant-a", run.ID, domain.RunFailed, 5*time.Second)
	if failed.TerminalReason == nil || *failed.TerminalReason != domain.ReasonStepTimeout {
		t.Fatalf("terminal reason = %v, want step_timeout", failed.TerminalReason)
	}
	if failed.EndedAt == nil {
		t.Fatalf("expired run has no ended_at: %+v", failed)
	}
	if steps := e.runSteps(t, "tenant-a", run.ID); len(steps) != 0 {
		t.Fatalf("expired run steps = %+v", steps)
	}
	if got := policy.count(); got != 1 {
		t.Fatalf("policy calls = %d, want 1", got)
	}
}

func TestHumanInputResumesAfterDispatcherRestart(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	policy := newHumanReviewPolicy(t, secret)
	e := newEnvWithGateway(t, modelgw.New("http://127.0.0.1:1/v1", "", 2*time.Second), 3)

	agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
	run := e.submitRun(t, "tenant-a", agent.ID)
	e.waitRun(t, "tenant-a", run.ID, domain.RunWaitingHuman, 5*time.Second)

	e.stopDispatcher()
	time.Sleep(300 * time.Millisecond)
	if got := policy.count(); got != 1 {
		t.Fatalf("policy calls with dispatcher stopped = %d, want 1", got)
	}
	e.startDispatcher(t)

	status, raw := e.submitHumanInput(t, "tenant-a", run.ID, 0, map[string]any{"approved": true})
	if status != http.StatusOK {
		t.Fatalf("human input after restart status = %d body=%s", status, raw)
	}
	e.waitRun(t, "tenant-a", run.ID, domain.RunCompleted, 5*time.Second)
	steps := e.runSteps(t, "tenant-a", run.ID)
	if len(steps) != 2 || steps[0].StepType != "human_review" || steps[1].StepType != "done" {
		t.Fatalf("steps after restart = %+v", steps)
	}
}
