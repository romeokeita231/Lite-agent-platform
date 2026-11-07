package runtime_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
)

func newLimitedEnv(t *testing.T, concurrency int, rate float64) *env {
	t.Helper()
	e := newEnv(t)
	e.stopDispatcher()
	e.options.TenantConcurrency = concurrency
	e.options.TenantRate = rate
	e.startDispatcher(t)
	return e
}

func (e *env) getRun(t *testing.T, tenant, id string) domain.Run {
	t.Helper()
	status, raw := e.do(t, http.MethodGet, "/v1/runs/"+id, tenant, nil)
	if status != http.StatusOK {
		t.Fatalf("get run %s status = %d body=%s", id, status, raw)
	}
	var run domain.Run
	if err := json.Unmarshal(raw, &run); err != nil {
		t.Fatalf("decode run %s: %v", raw, err)
	}
	return run
}

func (e *env) dbRunStatus(t *testing.T, id string) domain.RunStatus {
	t.Helper()
	var status string
	if err := e.env.Pool.QueryRow(context.Background(),
		`select status from runs where id::text = $1`, id).Scan(&status); err != nil {
		t.Fatalf("read run %s status: %v", id, err)
	}
	return domain.RunStatus(status)
}

func TestTenantConcurrencyKeepsOverflowQueued(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	policy := newPolicyServerFunc(t, secret, func(policyRequest) string {
		time.Sleep(300 * time.Millisecond)
		return `{"action":"done","output":{"answer":"done"}}`
	})
	e := newLimitedEnv(t, 1, 0)

	agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
	first := e.submitRun(t, "tenant-a", agent.ID)
	e.waitRun(t, "tenant-a", first.ID, domain.RunRunning, 3*time.Second)
	second := e.submitRun(t, "tenant-a", agent.ID)

	deadline := time.Now().Add(5 * time.Second)
	for {
		if e.getRun(t, "tenant-a", first.ID).Status != domain.RunRunning {
			break
		}
		if got := e.dbRunStatus(t, second.ID); got != domain.RunQueued {
			t.Fatalf("second run db status = %s, want queued", got)
		}
		if got := e.getRun(t, "tenant-a", second.ID).Status; got != domain.RunQueued {
			t.Fatalf("second run api status = %s, want queued", got)
		}
		if time.Now().After(deadline) {
			t.Fatalf("first run never left running")
		}
		time.Sleep(10 * time.Millisecond)
	}

	e.waitRun(t, "tenant-a", first.ID, domain.RunCompleted, 5*time.Second)
	e.waitRun(t, "tenant-a", second.ID, domain.RunRunning, 3*time.Second)
	completed := e.waitRun(t, "tenant-a", second.ID, domain.RunCompleted, 5*time.Second)
	if string(completed.Output) != `{"answer":"done"}` {
		t.Fatalf("second run output = %s", completed.Output)
	}
}

func TestTenantConcurrencyIsolatesTenants(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	policy := newPolicyServerFunc(t, secret, func(policyRequest) string {
		time.Sleep(600 * time.Millisecond)
		return `{"action":"done","output":{"answer":"done"}}`
	})
	e := newLimitedEnv(t, 1, 0)

	agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
	first := e.submitRun(t, "tenant-a", agent.ID)
	e.waitRun(t, "tenant-a", first.ID, domain.RunRunning, 3*time.Second)
	second := e.submitRun(t, "tenant-a", agent.ID)
	agentB := e.createAgent(t, "tenant-b", "beta", policy.srv.URL, "POLICY_SECRET_ALPHA")
	b := e.submitRun(t, "tenant-b", agentB.ID)

	start := time.Now()
	e.waitRun(t, "tenant-b", b.ID, domain.RunCompleted, 4*time.Second)
	elapsed := time.Since(start)
	if elapsed > 900*time.Millisecond {
		t.Fatalf("tenant-b run took %s while tenant-a was saturated", elapsed)
	}
	if got := e.getRun(t, "tenant-a", second.ID).Status; got == domain.RunCompleted {
		t.Fatalf("tenant-b finished only after tenant-a's queued run completed")
	}
	e.waitRun(t, "tenant-a", second.ID, domain.RunCompleted, 5*time.Second)
}

func TestTenantRateDelaysSameTenantRuns(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	policy := newPolicyServerFunc(t, secret, func(policyRequest) string {
		time.Sleep(50 * time.Millisecond)
		return `{"action":"done","output":{"answer":"done"}}`
	})
	e := newLimitedEnv(t, 4, 1)

	agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
	first := e.submitRun(t, "tenant-a", agent.ID)
	e.waitRun(t, "tenant-a", first.ID, domain.RunRunning, 3*time.Second)
	firstStarted := time.Now()
	second := e.submitRun(t, "tenant-a", agent.ID)

	deadline := time.Now().Add(5 * time.Second)
	var secondStarted time.Time
	for {
		if got := e.getRun(t, "tenant-a", second.ID).Status; got != domain.RunQueued {
			secondStarted = time.Now()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("second run never left queued")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if delta := secondStarted.Sub(firstStarted); delta < 800*time.Millisecond {
		t.Fatalf("second run started %s after first, want about 1s", delta)
	}
	e.waitRun(t, "tenant-a", first.ID, domain.RunCompleted, 5*time.Second)
	e.waitRun(t, "tenant-a", second.ID, domain.RunCompleted, 5*time.Second)
}

func TestWaitingHumanReleasesTenantSlot(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	policy := newPolicyServerFunc(t, secret, func(request policyRequest) string {
		if request.StepIndex == 0 {
			return `{"action":"human_review","prompt":"approve?","deadline_seconds":60}`
		}
		return `{"action":"done","output":{"answer":"done"}}`
	})
	e := newLimitedEnv(t, 1, 0)

	agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
	first := e.submitRun(t, "tenant-a", agent.ID)
	e.waitRun(t, "tenant-a", first.ID, domain.RunWaitingHuman, 5*time.Second)

	second := e.submitRun(t, "tenant-a", agent.ID)
	e.waitRun(t, "tenant-a", second.ID, domain.RunWaitingHuman, 5*time.Second)
	if got := e.getRun(t, "tenant-a", first.ID).Status; got != domain.RunWaitingHuman {
		t.Fatalf("first run status = %s while second ran, want waiting_human", got)
	}

	e.submitHumanInput(t, "tenant-a", second.ID, 0, map[string]any{"approved": true})
	e.waitRun(t, "tenant-a", second.ID, domain.RunCompleted, 5*time.Second)
	if got := e.getRun(t, "tenant-a", first.ID).Status; got != domain.RunWaitingHuman {
		t.Fatalf("first run status = %s after second completed, want waiting_human", got)
	}

	e.submitHumanInput(t, "tenant-a", first.ID, 0, map[string]any{"approved": true})
	e.waitRun(t, "tenant-a", first.ID, domain.RunCompleted, 5*time.Second)
}

func TestDispatcherRestartResetsTenantSlots(t *testing.T) {
	const secret = "runtime-secret"
	t.Setenv("POLICY_SECRET_ALPHA", secret)
	policy := newPolicyServerFunc(t, secret, func(policyRequest) string {
		time.Sleep(300 * time.Millisecond)
		return `{"action":"done","output":{"answer":"done"}}`
	})
	e := newLimitedEnv(t, 1, 0)

	agent := e.createAgent(t, "tenant-a", "alpha", policy.srv.URL, "POLICY_SECRET_ALPHA")
	first := e.submitRun(t, "tenant-a", agent.ID)
	e.waitRun(t, "tenant-a", first.ID, domain.RunRunning, 3*time.Second)
	second := e.submitRun(t, "tenant-a", agent.ID)
	if got := e.dbRunStatus(t, second.ID); got != domain.RunQueued {
		t.Fatalf("second run db status = %s, want queued", got)
	}

	e.stopDispatcher()
	if err := e.store.ResetRunningRuns(context.Background()); err != nil {
		t.Fatalf("reset running runs: %v", err)
	}
	e.startDispatcher(t)

	e.waitRun(t, "tenant-a", first.ID, domain.RunCompleted, 8*time.Second)
	e.waitRun(t, "tenant-a", second.ID, domain.RunCompleted, 8*time.Second)
}
