package repository_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/ids"
	"github.com/romeokeita231/Lite-agent-platform/internal/repository"
	"github.com/romeokeita231/Lite-agent-platform/internal/testutil"
)

func TestListRunsIsTenantScopedAndFiltered(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)
	ctxA := repository.WithTenant(context.Background(), "tenant-a")
	ctxB := repository.WithTenant(context.Background(), "tenant-b")
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	seed := []struct {
		tenant string
		agent  string
		status domain.RunStatus
		offset time.Duration
	}{
		{"tenant-a", "agent-a1", domain.RunQueued, time.Hour},
		{"tenant-a", "agent-a1", domain.RunCompleted, 2 * time.Hour},
		{"tenant-a", "agent-a2", domain.RunRunning, 3 * time.Hour},
		{"tenant-b", "agent-b1", domain.RunFailed, 4 * time.Hour},
	}
	created := make([]domain.Run, len(seed))
	for i, item := range seed {
		ctx := repository.WithTenant(context.Background(), item.tenant)
		run, err := store.CreateRun(ctx, domain.Run{
			ID:       ids.New(),
			TenantID: item.tenant,
			AgentID:  item.agent,
			Status:   item.status,
			Input:    json.RawMessage(`{}`),
		})
		if err != nil {
			t.Fatalf("create run %d: %v", i, err)
		}
		if _, err := env.Pool.Exec(context.Background(),
			`update runs set created_at = $2, updated_at = $2 where id::text = $1`,
			run.ID, base.Add(item.offset)); err != nil {
			t.Fatalf("stamp run %d: %v", i, err)
		}
		created[i] = run
	}

	runs, err := store.ListRuns(ctxA, "", "", 20)
	if err != nil {
		t.Fatalf("list runs a: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("tenant a runs = %d, want 3", len(runs))
	}
	wantOrder := []string{created[2].ID, created[1].ID, created[0].ID}
	for i, id := range wantOrder {
		if runs[i].ID != id || runs[i].TenantID != "tenant-a" {
			t.Fatalf("run %d = %+v, want id %s", i, runs[i], id)
		}
	}
	if runs[0].Status != domain.RunRunning || runs[1].Status != domain.RunCompleted || runs[2].Status != domain.RunQueued {
		t.Fatalf("run statuses = %s/%s/%s", runs[0].Status, runs[1].Status, runs[2].Status)
	}

	byStatus, err := store.ListRuns(ctxA, domain.RunCompleted, "", 20)
	if err != nil {
		t.Fatalf("list completed runs: %v", err)
	}
	if len(byStatus) != 1 || byStatus[0].ID != created[1].ID {
		t.Fatalf("completed runs = %+v", byStatus)
	}

	byAgent, err := store.ListRuns(ctxA, "", "agent-a1", 20)
	if err != nil {
		t.Fatalf("list agent runs: %v", err)
	}
	if len(byAgent) != 2 || byAgent[0].ID != created[1].ID || byAgent[1].ID != created[0].ID {
		t.Fatalf("agent runs = %+v", byAgent)
	}

	combined, err := store.ListRuns(ctxA, domain.RunQueued, "agent-a1", 20)
	if err != nil {
		t.Fatalf("list filtered runs: %v", err)
	}
	if len(combined) != 1 || combined[0].ID != created[0].ID {
		t.Fatalf("filtered runs = %+v", combined)
	}

	limited, err := store.ListRuns(ctxA, "", "", 2)
	if err != nil {
		t.Fatalf("list limited runs: %v", err)
	}
	if len(limited) != 2 || limited[0].ID != created[2].ID || limited[1].ID != created[1].ID {
		t.Fatalf("limited runs = %+v", limited)
	}

	empty, err := store.ListRuns(ctxA, domain.RunCancelled, "", 20)
	if err != nil {
		t.Fatalf("list cancelled runs: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("cancelled runs = %+v", empty)
	}

	runsB, err := store.ListRuns(ctxB, "", "", 20)
	if err != nil {
		t.Fatalf("list runs b: %v", err)
	}
	if len(runsB) != 1 || runsB[0].ID != created[3].ID || runsB[0].TenantID != "tenant-b" {
		t.Fatalf("tenant b runs = %+v", runsB)
	}

	if _, err := store.ListRuns(context.Background(), "", "", 20); !errors.Is(err, domain.ErrNoTenant) {
		t.Fatalf("list runs without tenant error = %v, want ErrNoTenant", err)
	}
}

func TestTenantRunCountsAreTenantScoped(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)
	ctxA := repository.WithTenant(context.Background(), "tenant-a")
	ctxB := repository.WithTenant(context.Background(), "tenant-b")

	seed := []struct {
		tenant string
		status domain.RunStatus
	}{
		{"tenant-a", domain.RunQueued},
		{"tenant-a", domain.RunCompleted},
		{"tenant-a", domain.RunRunning},
		{"tenant-b", domain.RunFailed},
		{"tenant-b", domain.RunFailed},
		{"tenant-b", domain.RunQueued},
	}
	for i, item := range seed {
		ctx := repository.WithTenant(context.Background(), item.tenant)
		if _, err := store.CreateRun(ctx, domain.Run{
			ID:       ids.New(),
			TenantID: item.tenant,
			AgentID:  "agent-" + item.tenant,
			Status:   item.status,
			Input:    json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("create run %d: %v", i, err)
		}
	}

	countsA, err := store.CountTenantRunsByStatus(ctxA)
	if err != nil {
		t.Fatalf("count tenant a runs: %v", err)
	}
	wantA := map[domain.RunStatus]int64{
		domain.RunQueued:    1,
		domain.RunCompleted: 1,
		domain.RunRunning:   1,
	}
	for status, expected := range wantA {
		if countsA[status] != expected {
			t.Fatalf("tenant a count for %s = %d, want %d", status, countsA[status], expected)
		}
	}
	if countsA[domain.RunFailed] != 0 || countsA[domain.RunWaitingHuman] != 0 || countsA[domain.RunCancelled] != 0 {
		t.Fatalf("tenant a leaked foreign counts: %+v", countsA)
	}

	countsB, err := store.CountTenantRunsByStatus(ctxB)
	if err != nil {
		t.Fatalf("count tenant b runs: %v", err)
	}
	wantB := map[domain.RunStatus]int64{
		domain.RunFailed: 2,
		domain.RunQueued: 1,
	}
	for status, expected := range wantB {
		if countsB[status] != expected {
			t.Fatalf("tenant b count for %s = %d, want %d", status, countsB[status], expected)
		}
	}
	if countsB[domain.RunCompleted] != 0 || countsB[domain.RunRunning] != 0 {
		t.Fatalf("tenant b leaked foreign counts: %+v", countsB)
	}

	if _, err := store.CountTenantRunsByStatus(context.Background()); !errors.Is(err, domain.ErrNoTenant) {
		t.Fatalf("count runs without tenant error = %v, want ErrNoTenant", err)
	}
}
