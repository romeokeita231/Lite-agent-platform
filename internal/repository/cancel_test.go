package repository_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/ids"
	"github.com/romeokeita231/Lite-agent-platform/internal/repository"
	"github.com/romeokeita231/Lite-agent-platform/internal/testutil"
)

func TestRequestCancelMarksNonTerminalRuns(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)
	ctxA := repository.WithTenant(context.Background(), "tenant-a")

	queued := createTestRun(t, store, ctxA, "tenant-a", "agent-a")
	cancelled, err := store.RequestCancel(ctxA, queued.ID)
	if err != nil {
		t.Fatalf("request cancel queued run: %v", err)
	}
	if cancelled.Status != domain.RunQueued || cancelled.TerminalReason != nil || cancelled.EndedAt != nil {
		t.Fatalf("queued run after request = %+v", cancelled)
	}
	requested, err := store.CancelRequested(ctxA, queued.ID)
	if err != nil {
		t.Fatalf("cancel requested: %v", err)
	}
	if !requested {
		t.Fatalf("cancel_requested is false after request")
	}
	var flag bool
	if err := env.Pool.QueryRow(context.Background(),
		`select cancel_requested from runs where id::text = $1`, queued.ID).Scan(&flag); err != nil {
		t.Fatalf("read cancel_requested: %v", err)
	}
	if !flag {
		t.Fatalf("cancel_requested column is false")
	}

	running := createTestRun(t, store, ctxA, "tenant-a", "agent-a")
	if _, err := store.ClaimRun(ctxA, running.ID); err != nil {
		t.Fatalf("claim running run: %v", err)
	}
	updated, err := store.RequestCancel(ctxA, running.ID)
	if err != nil {
		t.Fatalf("request cancel running run: %v", err)
	}
	if updated.Status != domain.RunRunning {
		t.Fatalf("running run after request = %+v", updated)
	}

	if err := store.MarkCancelled(ctxA, queued.ID); err != nil {
		t.Fatalf("mark cancelled: %v", err)
	}
	final, err := store.GetRun(ctxA, queued.ID)
	if err != nil {
		t.Fatalf("get cancelled run: %v", err)
	}
	if final.Status != domain.RunCancelled || final.TerminalReason == nil ||
		*final.TerminalReason != domain.ReasonCancelledByTenant || final.EndedAt == nil {
		t.Fatalf("cancelled run = %+v", final)
	}
}

func TestRequestCancelWaitingHumanRunCancelImmediately(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)
	ctxA := repository.WithTenant(context.Background(), "tenant-a")
	run := createWaitingRun(t, store, env, ctxA, "tenant-a")

	cancelled, err := store.RequestCancel(ctxA, run.ID)
	if err != nil {
		t.Fatalf("request cancel waiting run: %v", err)
	}
	if cancelled.Status != domain.RunCancelled {
		t.Fatalf("waiting run after cancel = %+v", cancelled)
	}
	if cancelled.TerminalReason == nil || *cancelled.TerminalReason != domain.ReasonCancelledByTenant {
		t.Fatalf("terminal reason = %v", cancelled.TerminalReason)
	}
	if cancelled.EndedAt == nil || cancelled.Deadline != nil || cancelled.HumanPrompt != "" ||
		len(cancelled.PendingStep) != 0 || cancelled.Attempt != 0 || cancelled.NextAttemptAt != nil {
		t.Fatalf("cancelled waiting run = %+v", cancelled)
	}

	var prompt string
	var pending, deadline *string
	var flag bool
	if err := env.Pool.QueryRow(context.Background(),
		`select human_prompt, pending_step::text, deadline::text, cancel_requested
		 from runs where id::text = $1`, run.ID,
	).Scan(&prompt, &pending, &deadline, &flag); err != nil {
		t.Fatalf("read cancelled waiting run columns: %v", err)
	}
	if prompt != "" || pending != nil || deadline != nil || !flag {
		t.Fatalf("cancelled waiting run columns = %q %v %v %v", prompt, pending, deadline, flag)
	}

	steps, err := store.GetSteps(ctxA, run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	if len(steps) != 0 {
		t.Fatalf("cancelled waiting run steps = %+v", steps)
	}
}

func TestRequestCancelRejectsTerminalAndForeignRuns(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)
	ctxA := repository.WithTenant(context.Background(), "tenant-a")
	ctxB := repository.WithTenant(context.Background(), "tenant-b")

	for _, status := range []domain.RunStatus{domain.RunCompleted, domain.RunFailed, domain.RunCancelled} {
		run := createTestRun(t, store, ctxA, "tenant-a", "agent-a")
		if _, err := env.Pool.Exec(context.Background(),
			`update runs set status = $2 where id::text = $1`, run.ID, string(status)); err != nil {
			t.Fatalf("seed %s run: %v", status, err)
		}
		if _, err := store.RequestCancel(ctxA, run.ID); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("cancel %s run error = %v, want ErrConflict", status, err)
		}
	}

	run := createTestRun(t, store, ctxA, "tenant-a", "agent-a")
	if _, err := store.RequestCancel(ctxB, run.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant cancel error = %v, want ErrNotFound", err)
	}
	if _, err := store.RequestCancel(ctxA, ids.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing cancel error = %v, want ErrNotFound", err)
	}
	if _, err := store.RequestCancel(context.Background(), run.ID); !errors.Is(err, domain.ErrNoTenant) {
		t.Fatalf("cancel without tenant error = %v, want ErrNoTenant", err)
	}
	if _, err := store.CancelRequested(ctxA, ids.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing cancel requested error = %v, want ErrNotFound", err)
	}
}

func TestCancelRequestedIsTenantScoped(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)
	ctxA := repository.WithTenant(context.Background(), "tenant-a")
	ctxB := repository.WithTenant(context.Background(), "tenant-b")
	run := createTestRun(t, store, ctxA, "tenant-a", "agent-a")

	requested, err := store.CancelRequested(ctxA, run.ID)
	if err != nil {
		t.Fatalf("cancel requested: %v", err)
	}
	if requested {
		t.Fatalf("new run has cancel_requested set")
	}
	if _, err := store.RequestCancel(ctxA, run.ID); err != nil {
		t.Fatalf("request cancel: %v", err)
	}
	requested, err = store.CancelRequested(ctxA, run.ID)
	if err != nil {
		t.Fatalf("cancel requested after request: %v", err)
	}
	if !requested {
		t.Fatalf("cancel_requested is false after request")
	}
	if _, err := store.CancelRequested(ctxB, run.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant cancel requested error = %v, want ErrNotFound", err)
	}
	if _, err := store.CancelRequested(context.Background(), run.ID); !errors.Is(err, domain.ErrNoTenant) {
		t.Fatalf("cancel requested without tenant error = %v, want ErrNoTenant", err)
	}
}

func TestMarkCancelledIsIdempotent(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)
	ctxA := repository.WithTenant(context.Background(), "tenant-a")
	run := createTestRun(t, store, ctxA, "tenant-a", "agent-a")
	if _, err := store.ClaimRun(ctxA, run.ID); err != nil {
		t.Fatalf("claim run: %v", err)
	}

	if err := store.MarkCancelled(ctxA, run.ID); err != nil {
		t.Fatalf("mark cancelled: %v", err)
	}
	first, err := store.GetRun(ctxA, run.ID)
	if err != nil {
		t.Fatalf("get cancelled run: %v", err)
	}
	if first.Status != domain.RunCancelled || first.TerminalReason == nil ||
		*first.TerminalReason != domain.ReasonCancelledByTenant || first.EndedAt == nil {
		t.Fatalf("cancelled run = %+v", first)
	}

	if err := store.MarkCancelled(ctxA, run.ID); err != nil {
		t.Fatalf("second mark cancelled: %v", err)
	}
	second, err := store.GetRun(ctxA, run.ID)
	if err != nil {
		t.Fatalf("get run after duplicate cancel: %v", err)
	}
	if second.Status != first.Status || !second.UpdatedAt.Equal(first.UpdatedAt) ||
		second.EndedAt == nil || !second.EndedAt.Equal(*first.EndedAt) ||
		second.TerminalReason == nil || *second.TerminalReason != *first.TerminalReason {
		t.Fatalf("run changed after duplicate cancel: first=%+v second=%+v", first, second)
	}

	failed := createTestRun(t, store, ctxA, "tenant-a", "agent-a")
	if _, err := env.Pool.Exec(context.Background(),
		`update runs set status = 'failed', terminal_reason = 'max_retries', ended_at = now()
		 where id::text = $1`, failed.ID); err != nil {
		t.Fatalf("seed failed run: %v", err)
	}
	var before time.Time
	if err := env.Pool.QueryRow(context.Background(),
		`select updated_at from runs where id::text = $1`, failed.ID).Scan(&before); err != nil {
		t.Fatalf("read failed updated_at: %v", err)
	}
	if err := store.MarkCancelled(ctxA, failed.ID); err != nil {
		t.Fatalf("mark failed run cancelled: %v", err)
	}
	got, err := store.GetRun(ctxA, failed.ID)
	if err != nil {
		t.Fatalf("get failed run: %v", err)
	}
	if got.Status != domain.RunFailed || got.TerminalReason == nil ||
		*got.TerminalReason != domain.ReasonMaxRetries || !got.UpdatedAt.Equal(before) {
		t.Fatalf("failed run changed: %+v", got)
	}
}
