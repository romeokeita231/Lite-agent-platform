package runtime_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/romeokeita231/Lite-agent-platform/internal/contextwin"
	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/modelgw"
)

func newKeepPolicy(t *testing.T, secret string) *policyServer {
	t.Helper()
	return newPolicyServerFunc(t, secret, func(request policyRequest) string {
		switch request.StepIndex {
		case 0:
			return `{"action":"llm_call","route":"route:default","messages":[{"role":"user","content":"hi"}],"keep":[0]}`
		case 1:
			return `{"action":"llm_call","route":"route:default","messages":[{"role":"user","content":"more"}],"keep":[0]}`
		case 2:
			return `{"action":"human_review","prompt":"approve?","deadline_seconds":60,"keep":[0,1]}`
		default:
			return `{"action":"done","output":{"answer":"done"}}`
		}
	})
}

func TestContextWindowKeepsRequestedStepsAndSurvivesRestart(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	upstream := newFakeUpstream(t, 200)
	policy := newKeepPolicy(t, secret)
	e := newEnvWithGateway(t, modelgw.New(upstream.baseURL(), "", 2*time.Second), 3)

	e.stopDispatcher()
	e.options.ContextRecent = 1
	e.options.ContextMaxKeep = 1
	e.startDispatcher(t)

	agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
	run := e.submitRun(t, "tenant-a", agent.ID)

	e.waitRun(t, "tenant-a", run.ID, domain.RunWaitingHuman, 10*time.Second)

	requests := policy.snapshot()
	if len(requests) != 3 {
		t.Fatalf("policy requests before human input = %d, want 3", len(requests))
	}
	before := requests[2]
	if before.StepIndex != 2 {
		t.Fatalf("step 2 request step_index = %d", before.StepIndex)
	}
	if got := recentIndices(t, before); !equalIndices(got, []int{0, 1}) {
		t.Fatalf("step 2 recent = %v, want [0 1]", got)
	}
	if len(before.Ledger.Dropped) != 0 {
		t.Fatalf("step 2 dropped = %v, want empty", before.Ledger.Dropped)
	}

	e.stopDispatcher()
	time.Sleep(200 * time.Millisecond)
	e.startDispatcher(t)

	status, raw := e.submitHumanInput(t, "tenant-a", run.ID, 2, map[string]any{"approved": true})
	if status != 200 {
		t.Fatalf("human input status = %d body=%s", status, raw)
	}
	e.waitRun(t, "tenant-a", run.ID, domain.RunCompleted, 10*time.Second)

	requests = policy.snapshot()
	if len(requests) != 4 {
		t.Fatalf("policy requests = %d, want 4", len(requests))
	}
	after := requests[3]
	if after.StepIndex != 3 {
		t.Fatalf("step 3 request step_index = %d", after.StepIndex)
	}
	if got := recentIndices(t, after); !equalIndices(got, []int{0, 2}) {
		t.Fatalf("step 3 recent = %v, want [0 2]", got)
	}
	if !equalIndices(after.Ledger.Dropped, []int{1}) {
		t.Fatalf("step 3 dropped = %v, want [1]", after.Ledger.Dropped)
	}
	if after.Ledger.Summary != "1 llm_call 42" {
		t.Fatalf("step 3 summary = %q", after.Ledger.Summary)
	}
	if after.Ledger.Recent[0].StepType != "llm_call" || after.Ledger.Recent[1].StepType != "human_review" {
		t.Fatalf("step 3 recent = %+v", after.Ledger.Recent)
	}

	type keptResult struct {
		Input json.RawMessage `json:"input"`
		Keep  []int           `json:"keep"`
	}
	steps := e.runSteps(t, "tenant-a", run.ID)
	if len(steps) != 4 {
		t.Fatalf("steps = %+v", steps)
	}
	var human keptResult
	if err := json.Unmarshal(steps[2].Result, &human); err != nil {
		t.Fatalf("decode human result %s: %v", steps[2].Result, err)
	}
	if !equalIndices(human.Keep, []int{0, 1}) {
		t.Fatalf("human checkpoint keep = %v, want [0 1]", human.Keep)
	}

	rebuilt := make([]contextwin.Step, 0, len(steps)-1)
	for _, step := range steps[:3] {
		rebuilt = append(rebuilt, contextwin.Step{
			StepIndex: step.StepIndex,
			StepType:  step.StepType,
			Result:    step.Result,
		})
	}
	view := contextwin.Build(rebuilt, contextwin.KeepOf(steps[2].Result), contextwin.Config{Recent: 1, MaxKeep: 1})
	if !equalIndices(indices(view.Recent), recentIndices(t, after)) ||
		!equalIndices(view.Dropped, after.Ledger.Dropped) || view.Summary != after.Ledger.Summary {
		t.Fatalf("rebuilt view = %+v, ledger = %+v", view, after.Ledger)
	}

	var attributes json.RawMessage
	if err := e.env.Pool.QueryRow(context.Background(),
		`select attributes from spans where run_id::text = $1 and step_index = 3`, run.ID,
	).Scan(&attributes); err != nil {
		t.Fatalf("read done span: %v", err)
	}
	var attrs map[string]any
	if err := json.Unmarshal(attributes, &attrs); err != nil {
		t.Fatalf("decode span attributes %s: %v", attributes, err)
	}
	if attrs["context_tokens"] == nil || attrs["context_dropped_steps"] != float64(1) {
		t.Fatalf("done span attributes = %s", attributes)
	}
}

func recentIndices(t *testing.T, request policyRequest) []int {
	t.Helper()
	indices := make([]int, 0, len(request.Ledger.Recent))
	for _, step := range request.Ledger.Recent {
		indices = append(indices, step.StepIndex)
	}
	return indices
}

func indices(steps []contextwin.Step) []int {
	out := make([]int, 0, len(steps))
	for _, step := range steps {
		out = append(out, step.StepIndex)
	}
	return out
}

func equalIndices(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
