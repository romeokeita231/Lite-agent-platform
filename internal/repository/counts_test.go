package repository_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/ids"
	"github.com/romeokeita231/Lite-agent-platform/internal/repository"
	"github.com/romeokeita231/Lite-agent-platform/internal/testutil"
)

func TestCountRunsByStatusIgnoresTenantFilter(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)

	runs := []struct {
		tenant string
		status domain.RunStatus
	}{
		{"tenant-a", domain.RunQueued},
		{"tenant-a", domain.RunCompleted},
		{"tenant-b", domain.RunFailed},
		{"tenant-b", domain.RunFailed},
		{"tenant-b", domain.RunRunning},
	}
	for _, item := range runs {
		ctx := repository.WithTenant(context.Background(), item.tenant)
		if _, err := store.CreateRun(ctx, domain.Run{
			ID:       ids.New(),
			TenantID: item.tenant,
			AgentID:  "agent-" + item.tenant,
			Status:   item.status,
			Input:    json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("create %s run for %s: %v", item.status, item.tenant, err)
		}
	}

	counts, err := store.CountRunsByStatus(context.Background())
	if err != nil {
		t.Fatalf("count runs by status: %v", err)
	}
	want := map[domain.RunStatus]int64{
		domain.RunQueued:    1,
		domain.RunCompleted: 1,
		domain.RunFailed:    2,
		domain.RunRunning:   1,
	}
	for status, expected := range want {
		if got := counts[status]; got != expected {
			t.Fatalf("count for %s = %d, want %d", status, got, expected)
		}
	}
	if counts[domain.RunWaitingHuman] != 0 || counts[domain.RunCancelled] != 0 {
		t.Fatalf("unexpected status counts: %+v", counts)
	}
}
