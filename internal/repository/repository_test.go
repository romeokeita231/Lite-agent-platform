package repository_test

import (
	"context"
	"errors"
	"testing"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/repository"
	"github.com/romeokeita231/Lite-agent-platform/internal/testutil"
)

func TestTenantSeamFiltersEveryTenantMethod(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)
	ctxA := repository.WithTenant(context.Background(), "tenant-a")
	ctxB := repository.WithTenant(context.Background(), "tenant-b")

	if err := store.CreateTenant(ctxA, domain.Tenant{ID: "tenant-a", Name: "Tenant A"}); err != nil {
		t.Fatalf("create tenant a: %v", err)
	}
	if err := store.CreateTenant(ctxB, domain.Tenant{ID: "tenant-b", Name: "Tenant B"}); err != nil {
		t.Fatalf("create tenant b: %v", err)
	}

	if err := store.CreateTenant(ctxA, domain.Tenant{ID: "tenant-b", Name: "Hijacked"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant create error = %v, want ErrNotFound", err)
	}
	if err := store.CreateTenant(context.Background(), domain.Tenant{ID: "tenant-c", Name: "No Context"}); !errors.Is(err, domain.ErrNoTenant) {
		t.Fatalf("create without tenant error = %v, want ErrNoTenant", err)
	}

	tenantA, err := store.GetTenant(ctxA, "tenant-a")
	if err != nil {
		t.Fatalf("get tenant a: %v", err)
	}
	if tenantA.ID != "tenant-a" || tenantA.Name != "Tenant A" {
		t.Fatalf("tenant a = %+v", tenantA)
	}
	tenantB, err := store.GetTenant(ctxB, "tenant-b")
	if err != nil {
		t.Fatalf("get tenant b: %v", err)
	}
	if tenantB.ID != "tenant-b" || tenantB.Name != "Tenant B" {
		t.Fatalf("tenant b = %+v", tenantB)
	}

	if _, err := store.GetTenant(ctxA, "tenant-b"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant get error = %v, want ErrNotFound", err)
	}
	if _, err := store.GetTenant(context.Background(), "tenant-a"); !errors.Is(err, domain.ErrNoTenant) {
		t.Fatalf("get without tenant error = %v, want ErrNoTenant", err)
	}
}
