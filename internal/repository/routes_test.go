package repository_test

import (
	"context"
	"errors"
	"testing"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/repository"
	"github.com/romeokeita231/Lite-agent-platform/internal/testutil"
)

func TestModelRoutesAreTenantScopedAndUpsertable(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)
	ctxA := repository.WithTenant(context.Background(), "tenant-a")
	ctxB := repository.WithTenant(context.Background(), "tenant-b")

	for _, tenantID := range []string{"tenant-a", "tenant-b"} {
		ctx := repository.WithTenant(context.Background(), tenantID)
		if err := store.CreateTenant(ctx, domain.Tenant{ID: tenantID, Name: tenantID}); err != nil {
			t.Fatalf("create tenant %s: %v", tenantID, err)
		}
	}

	route, err := store.UpsertModelRoute(ctxA, domain.ModelRoute{
		TenantID: "tenant-a",
		Label:    "cheap",
		Chain:    []string{"m1", "m2"},
	})
	if err != nil {
		t.Fatalf("upsert route: %v", err)
	}
	if route.Label != "cheap" || len(route.Chain) != 2 || route.Chain[0] != "m1" || route.Chain[1] != "m2" {
		t.Fatalf("upserted route = %+v", route)
	}
	if route.CreatedAt.IsZero() {
		t.Fatalf("route created_at is zero")
	}

	route, err = store.UpsertModelRoute(ctxA, domain.ModelRoute{
		TenantID: "tenant-a",
		Label:    "cheap",
		Chain:    []string{"m3"},
	})
	if err != nil {
		t.Fatalf("update route: %v", err)
	}
	if len(route.Chain) != 1 || route.Chain[0] != "m3" {
		t.Fatalf("updated route = %+v", route)
	}
	stored, err := store.GetModelRoute(ctxA, "cheap")
	if err != nil {
		t.Fatalf("get route: %v", err)
	}
	if len(stored.Chain) != 1 || stored.Chain[0] != "m3" {
		t.Fatalf("stored route = %+v", stored)
	}

	if _, err := store.UpsertModelRoute(ctxB, domain.ModelRoute{TenantID: "tenant-a", Label: "cheap", Chain: []string{"m1"}}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant upsert error = %v, want ErrNotFound", err)
	}
	if _, err := store.GetModelRoute(ctxB, "cheap"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant get error = %v, want ErrNotFound", err)
	}
	if _, err := store.GetModelRoute(context.Background(), "cheap"); !errors.Is(err, domain.ErrNoTenant) {
		t.Fatalf("get without tenant error = %v, want ErrNoTenant", err)
	}

	if _, err := store.UpsertModelRoute(ctxB, domain.ModelRoute{TenantID: "tenant-b", Label: "fast", Chain: []string{"b1"}}); err != nil {
		t.Fatalf("upsert tenant b route: %v", err)
	}
	routes, err := store.ListModelRoutes(ctxA)
	if err != nil {
		t.Fatalf("list routes: %v", err)
	}
	if len(routes) != 1 || routes[0].Label != "cheap" || routes[0].TenantID != "tenant-a" {
		t.Fatalf("tenant a routes = %+v", routes)
	}
	routes, err = store.ListModelRoutes(ctxB)
	if err != nil {
		t.Fatalf("list tenant b routes: %v", err)
	}
	if len(routes) != 1 || routes[0].Label != "fast" || routes[0].TenantID != "tenant-b" {
		t.Fatalf("tenant b routes = %+v", routes)
	}
}
