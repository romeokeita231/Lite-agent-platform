package repository_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/ids"
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

func TestLookupTenantByAPIKeyIgnoresTenantFilter(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)
	keyA := "key-a-0123456789"
	keyB := "key-b-0123456789"
	ctxA := repository.WithTenant(context.Background(), "tenant-a")
	ctxB := repository.WithTenant(context.Background(), "tenant-b")

	if err := store.CreateTenant(ctxA, domain.Tenant{
		ID:           "tenant-a",
		Name:         "Tenant A",
		APIKeyHash:   hashHex(keyA),
		APIKeyPrefix: keyA[:8],
	}); err != nil {
		t.Fatalf("create tenant a: %v", err)
	}
	if err := store.CreateTenant(ctxB, domain.Tenant{
		ID:           "tenant-b",
		Name:         "Tenant B",
		APIKeyHash:   hashHex(keyB),
		APIKeyPrefix: keyB[:8],
	}); err != nil {
		t.Fatalf("create tenant b: %v", err)
	}

	tenant, err := store.LookupTenantByAPIKey(context.Background(), hashHex(keyA))
	if err != nil {
		t.Fatalf("lookup key a: %v", err)
	}
	if tenant.ID != "tenant-a" || tenant.APIKeyPrefix != keyA[:8] {
		t.Fatalf("lookup key a = %+v", tenant)
	}
	tenant, err = store.LookupTenantByAPIKey(context.Background(), hashHex(keyB))
	if err != nil {
		t.Fatalf("lookup key b: %v", err)
	}
	if tenant.ID != "tenant-b" || tenant.APIKeyPrefix != keyB[:8] {
		t.Fatalf("lookup key b = %+v", tenant)
	}
	if _, err := store.LookupTenantByAPIKey(context.Background(), hashHex("unknown")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("lookup unknown key error = %v, want ErrNotFound", err)
	}

	var storedHash, storedPrefix string
	if err := env.Pool.QueryRow(context.Background(),
		`select api_key_hash, api_key_prefix from tenants where id = 'tenant-a'`,
	).Scan(&storedHash, &storedPrefix); err != nil {
		t.Fatalf("read stored key columns: %v", err)
	}
	if storedHash == keyA {
		t.Fatalf("plaintext key stored in api_key_hash")
	}
	if storedHash != hashHex(keyA) || storedPrefix != keyA[:8] {
		t.Fatalf("stored hash/prefix = %q/%q", storedHash, storedPrefix)
	}
}

func TestAgentsAreTenantScoped(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)
	ctxA := repository.WithTenant(context.Background(), "tenant-a")
	ctxB := repository.WithTenant(context.Background(), "tenant-b")

	if _, err := store.CreateAgent(ctxA, domain.Agent{
		ID:        "agent-a",
		TenantID:  "tenant-a",
		Name:      "alpha",
		PolicyURL: "http://policy-a",
		SecretEnv: "POLICY_SECRET_ALPHA",
	}); err != nil {
		t.Fatalf("create agent a: %v", err)
	}
	if _, err := store.CreateAgent(ctxB, domain.Agent{
		ID:        "agent-b",
		TenantID:  "tenant-b",
		Name:      "beta",
		PolicyURL: "http://policy-b",
		SecretEnv: "POLICY_SECRET_BETA",
	}); err != nil {
		t.Fatalf("create agent b: %v", err)
	}

	agentsA, err := store.ListAgents(ctxA)
	if err != nil {
		t.Fatalf("list agents a: %v", err)
	}
	if len(agentsA) != 1 || agentsA[0].ID != "agent-a" || agentsA[0].TenantID != "tenant-a" {
		t.Fatalf("agents a = %+v", agentsA)
	}
	agentsB, err := store.ListAgents(ctxB)
	if err != nil {
		t.Fatalf("list agents b: %v", err)
	}
	if len(agentsB) != 1 || agentsB[0].ID != "agent-b" || agentsB[0].TenantID != "tenant-b" {
		t.Fatalf("agents b = %+v", agentsB)
	}

	if _, err := store.CreateAgent(ctxA, domain.Agent{
		ID:        "agent-hijack",
		TenantID:  "tenant-b",
		Name:      "hijack",
		PolicyURL: "http://policy-b",
		SecretEnv: "POLICY_SECRET_BETA",
	}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant create agent error = %v, want ErrNotFound", err)
	}
	if _, err := store.CreateAgent(context.Background(), domain.Agent{
		ID:        "agent-none",
		TenantID:  "tenant-a",
		Name:      "none",
		PolicyURL: "http://policy-a",
		SecretEnv: "POLICY_SECRET_NONE",
	}); !errors.Is(err, domain.ErrNoTenant) {
		t.Fatalf("create agent without tenant error = %v, want ErrNoTenant", err)
	}
	if _, err := store.CreateAgent(ctxA, domain.Agent{
		ID:        "agent-duplicate",
		TenantID:  "tenant-a",
		Name:      "alpha",
		PolicyURL: "http://policy-a2",
		SecretEnv: "POLICY_SECRET_ALPHA",
	}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate agent error = %v, want ErrConflict", err)
	}
	if _, err := store.ListAgents(context.Background()); !errors.Is(err, domain.ErrNoTenant) {
		t.Fatalf("list agents without tenant error = %v, want ErrNoTenant", err)
	}
}

func TestRunsAreTenantScoped(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)
	ctxA := repository.WithTenant(context.Background(), "tenant-a")
	ctxB := repository.WithTenant(context.Background(), "tenant-b")

	runID := ids.New()
	created, err := store.CreateRun(ctxA, domain.Run{
		ID:       runID,
		TenantID: "tenant-a",
		AgentID:  "agent-a",
		Status:   domain.RunQueued,
		Input:    json.RawMessage(`{"order_id":"A1"}`),
	})
	if err != nil {
		t.Fatalf("create run a: %v", err)
	}
	if created.Status != domain.RunQueued || created.ID != runID || created.TenantID != "tenant-a" {
		t.Fatalf("created run = %+v", created)
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Fatalf("created run timestamps are zero: %+v", created)
	}

	got, err := store.GetRun(ctxA, runID)
	if err != nil {
		t.Fatalf("get run a: %v", err)
	}
	var input struct {
		OrderID string `json:"order_id"`
	}
	if err := json.Unmarshal(got.Input, &input); err != nil {
		t.Fatalf("decode run input: %v", err)
	}
	if got.Status != domain.RunQueued || got.AgentID != "agent-a" || input.OrderID != "A1" {
		t.Fatalf("run a = %+v input=%s", got, got.Input)
	}
	if got.TerminalReason != nil || got.Output != nil || got.EndedAt != nil {
		t.Fatalf("new run has terminal fields: %+v", got)
	}

	if _, err := store.GetRun(ctxB, runID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant get run error = %v, want ErrNotFound", err)
	}
	if _, err := store.GetRun(ctxA, ids.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing run error = %v, want ErrNotFound", err)
	}
	if _, err := store.CreateRun(ctxA, domain.Run{
		ID:       ids.New(),
		TenantID: "tenant-b",
		AgentID:  "agent-b",
		Status:   domain.RunQueued,
		Input:    json.RawMessage(`{}`),
	}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant create run error = %v, want ErrNotFound", err)
	}
	if _, err := store.CreateRun(context.Background(), domain.Run{
		ID:       ids.New(),
		TenantID: "tenant-a",
		AgentID:  "agent-a",
		Status:   domain.RunQueued,
		Input:    json.RawMessage(`{}`),
	}); !errors.Is(err, domain.ErrNoTenant) {
		t.Fatalf("create run without tenant error = %v, want ErrNoTenant", err)
	}
	if _, err := store.GetRun(context.Background(), runID); !errors.Is(err, domain.ErrNoTenant) {
		t.Fatalf("get run without tenant error = %v, want ErrNoTenant", err)
	}
}

func hashHex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
