package repository_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/ids"
	"github.com/romeokeita231/Lite-agent-platform/internal/repository"
	"github.com/romeokeita231/Lite-agent-platform/internal/testutil"
)

func TestFailExpiredHumanWaitsIgnoresTenantFilterAndMarksTimeout(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)
	ctxA := repository.WithTenant(context.Background(), "tenant-a")
	ctxB := repository.WithTenant(context.Background(), "tenant-b")

	runA := createTestRun(t, store, ctxA, "tenant-a", "agent-a")
	runB := createTestRun(t, store, ctxB, "tenant-b", "agent-b")
	future := createTestRun(t, store, ctxA, "tenant-a", "agent-a")
	for _, item := range []struct {
		runID    string
		deadline string
	}{
		{runA.ID, "now() - interval '1 minute'"},
		{runB.ID, "now() - interval '2 seconds'"},
		{future.ID, "now() + interval '1 hour'"},
	} {
		if _, err := env.Pool.Exec(context.Background(),
			`update runs set status = 'waiting_human', pending_step = '{"action":"human_review"}',
			        human_prompt = 'approve?', attempt = 1, deadline = `+item.deadline+`
			 where id::text = $1`, item.runID); err != nil {
			t.Fatalf("seed waiting run %s: %v", item.runID, err)
		}
	}

	if err := store.FailExpiredHumanWaits(context.Background()); err != nil {
		t.Fatalf("fail expired human waits: %v", err)
	}

	for _, item := range []struct {
		ctx  context.Context
		run  domain.Run
		want domain.RunStatus
	}{
		{ctxA, runA, domain.RunFailed},
		{ctxB, runB, domain.RunFailed},
		{ctxA, future, domain.RunWaitingHuman},
	} {
		run, err := store.GetRun(item.ctx, item.run.ID)
		if err != nil {
			t.Fatalf("get run %s: %v", item.run.ID, err)
		}
		if run.Status != item.want {
			t.Fatalf("run %s status = %s, want %s", item.run.ID, run.Status, item.want)
		}
		if item.want == domain.RunFailed {
			if run.TerminalReason == nil || *run.TerminalReason != domain.ReasonStepTimeout {
				t.Fatalf("run %s terminal reason = %v, want step_timeout", item.run.ID, run.TerminalReason)
			}
			if run.EndedAt == nil {
				t.Fatalf("run %s has no ended_at", item.run.ID)
			}
		} else if run.TerminalReason != nil || run.EndedAt != nil {
			t.Fatalf("future run %s has terminal fields: %+v", item.run.ID, run)
		}
	}
}

func TestSubmitHumanInputWritesCheckpointSpanAndQueuesRun(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)
	ctxA := repository.WithTenant(context.Background(), "tenant-a")
	ctxB := repository.WithTenant(context.Background(), "tenant-b")
	run := createWaitingRun(t, store, env, ctxA, "tenant-a")

	updated, err := store.SubmitHumanInput(ctxA, humanCheckpoint(run.ID, 0), humanSpan(run.ID, 0, 1))
	if err != nil {
		t.Fatalf("submit human input: %v", err)
	}
	if updated.Status != domain.RunQueued || updated.Attempt != 0 || updated.NextAttemptAt != nil ||
		updated.HumanPrompt != "" || updated.Deadline != nil || len(updated.PendingStep) != 0 {
		t.Fatalf("updated run = %+v", updated)
	}

	steps, err := store.GetSteps(ctxA, run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	if len(steps) != 1 || steps[0].StepIndex != 0 || steps[0].StepType != domain.StepHumanReview {
		t.Fatalf("steps = %+v", steps)
	}
	var result struct {
		Input struct {
			Approved bool `json:"approved"`
		} `json:"input"`
	}
	if err := json.Unmarshal(steps[0].Result, &result); err != nil || !result.Input.Approved {
		t.Fatalf("human result = %s (%v)", steps[0].Result, err)
	}

	var spanName string
	var spanAttempt int
	if err := env.Pool.QueryRow(context.Background(),
		`select name, attempt from spans where run_id::text = $1`, run.ID,
	).Scan(&spanName, &spanAttempt); err != nil {
		t.Fatalf("read span: %v", err)
	}
	if spanName != "step.human_review" || spanAttempt != 1 {
		t.Fatalf("span = %q attempt %d", spanName, spanAttempt)
	}

	if _, err := store.SubmitHumanInput(ctxA, humanCheckpoint(run.ID, 0), humanSpan(run.ID, 0, 1)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate submit error = %v, want ErrConflict", err)
	}

	mismatched := createWaitingRun(t, store, env, ctxA, "tenant-a")
	if _, err := store.SubmitHumanInput(ctxA, humanCheckpoint(mismatched.ID, 1), humanSpan(mismatched.ID, 1, 0)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("mismatched step error = %v, want ErrConflict", err)
	}
	if got, err := store.GetRun(ctxA, mismatched.ID); err != nil || got.Status != domain.RunWaitingHuman {
		t.Fatalf("run after mismatched submit = %+v (%v)", got, err)
	}

	queued := createTestRun(t, store, ctxA, "tenant-a", "agent-a")
	if _, err := store.SubmitHumanInput(ctxA, humanCheckpoint(queued.ID, 0), humanSpan(queued.ID, 0, 0)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("queued run submit error = %v, want ErrConflict", err)
	}

	runB := createWaitingRun(t, store, env, ctxB, "tenant-b")
	if _, err := store.SubmitHumanInput(ctxA, humanCheckpoint(runB.ID, 0), humanSpan(runB.ID, 0, 0)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant submit error = %v, want ErrNotFound", err)
	}
	if _, err := store.SubmitHumanInput(context.Background(), humanCheckpoint(run.ID, 0), humanSpan(run.ID, 0, 0)); !errors.Is(err, domain.ErrNoTenant) {
		t.Fatalf("submit without tenant error = %v, want ErrNoTenant", err)
	}
}

func TestSubmitHumanInputAllowsOnlyOneConcurrentSubmit(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)
	ctxA := repository.WithTenant(context.Background(), "tenant-a")
	run := createWaitingRun(t, store, env, ctxA, "tenant-a")

	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.SubmitHumanInput(ctxA, humanCheckpoint(run.ID, 0), humanSpan(run.ID, 0, 0))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	successes, conflicts := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, domain.ErrConflict):
			conflicts++
		default:
			t.Fatalf("concurrent submit error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent submits = %d success / %d conflict, want 1/1", successes, conflicts)
	}
	var checkpointCount, spanCount int
	if err := env.Pool.QueryRow(context.Background(),
		`select count(*) from checkpoints where run_id::text = $1`, run.ID).Scan(&checkpointCount); err != nil {
		t.Fatalf("count checkpoints: %v", err)
	}
	if err := env.Pool.QueryRow(context.Background(),
		`select count(*) from spans where run_id::text = $1`, run.ID).Scan(&spanCount); err != nil {
		t.Fatalf("count spans: %v", err)
	}
	if checkpointCount != 1 || spanCount != 1 {
		t.Fatalf("checkpoints/spans = %d/%d, want 1/1", checkpointCount, spanCount)
	}
}

func createWaitingRun(t *testing.T, store *repository.Store, env *testutil.Env, ctx context.Context, tenantID string) domain.Run {
	t.Helper()
	run := createTestRun(t, store, ctx, tenantID, "agent-"+tenantID)
	if _, err := env.Pool.Exec(context.Background(),
		`update runs set status = 'waiting_human', pending_step = '{"action":"human_review","prompt":"approve?"}',
		        human_prompt = 'approve?', deadline = now() + interval '1 hour', attempt = 1
		 where id::text = $1`, run.ID); err != nil {
		t.Fatalf("seed waiting run: %v", err)
	}
	return run
}

func humanCheckpoint(runID string, stepIndex int) domain.Checkpoint {
	return domain.Checkpoint{
		RunID:     runID,
		TenantID:  "tenant-a",
		StepIndex: stepIndex,
		StepType:  domain.StepHumanReview,
		Result:    json.RawMessage(`{"input":{"approved":true}}`),
		CreatedAt: time.Now(),
	}
}

func humanSpan(runID string, stepIndex, attempt int) domain.Span {
	id := runID
	return domain.Span{
		ID:         ids.New(),
		TenantID:   "tenant-a",
		RunID:      &id,
		StepIndex:  stepIndex,
		Attempt:    attempt,
		Name:       "step.human_review",
		StartedAt:  time.Now(),
		EndedAt:    time.Now(),
		Attributes: json.RawMessage(`{}`),
	}
}
