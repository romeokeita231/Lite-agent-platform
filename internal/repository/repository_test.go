package repository_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

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

func TestDispatcherRunAccessIgnoresTenantFilter(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)
	ctxA := repository.WithTenant(context.Background(), "tenant-a")
	ctxB := repository.WithTenant(context.Background(), "tenant-b")

	runA := createTestRun(t, store, ctxA, "tenant-a", "agent-a")
	runB := createTestRun(t, store, ctxB, "tenant-b", "agent-b")
	future := createTestRun(t, store, ctxA, "tenant-a", "agent-a")
	if _, err := env.Pool.Exec(context.Background(),
		`update runs set next_attempt_at = now() + interval '1 hour' where id::text = $1`, future.ID); err != nil {
		t.Fatalf("delay run: %v", err)
	}

	runs, err := store.ListRunnableRuns(context.Background())
	if err != nil {
		t.Fatalf("list runnable runs: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runnable runs = %d, want 2", len(runs))
	}
	seen := map[string]bool{}
	for _, run := range runs {
		seen[run.ID] = true
	}
	if !seen[runA.ID] || !seen[runB.ID] || seen[future.ID] {
		t.Fatalf("runnable runs = %+v", runs)
	}

	claimed, err := store.ClaimRun(context.Background(), runA.ID)
	if err != nil {
		t.Fatalf("claim run a: %v", err)
	}
	if !claimed {
		t.Fatalf("first claim of run a failed")
	}
	claimed, err = store.ClaimRun(context.Background(), runA.ID)
	if err != nil {
		t.Fatalf("second claim run a: %v", err)
	}
	if claimed {
		t.Fatalf("second claim of run a succeeded")
	}

	runs, err = store.ListRunnableRuns(context.Background())
	if err != nil {
		t.Fatalf("list runnable runs after claim: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != runB.ID {
		t.Fatalf("runnable runs after claim = %+v", runs)
	}
}

func TestResetRunningRunsIgnoresTenantFilter(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)
	ctxA := repository.WithTenant(context.Background(), "tenant-a")
	ctxB := repository.WithTenant(context.Background(), "tenant-b")

	runA := createTestRun(t, store, ctxA, "tenant-a", "agent-a")
	runB := createTestRun(t, store, ctxB, "tenant-b", "agent-b")
	finished := createTestRun(t, store, ctxA, "tenant-a", "agent-a")

	for _, id := range []string{runA.ID, runB.ID, finished.ID} {
		claimed, err := store.ClaimRun(context.Background(), id)
		if err != nil || !claimed {
			t.Fatalf("claim run %s: claimed=%v err=%v", id, claimed, err)
		}
	}
	if err := store.RescheduleStep(context.Background(), runA.ID, 3, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("reschedule run a: %v", err)
	}
	if _, err := env.Pool.Exec(context.Background(),
		`update runs set cancel_requested = true where id::text = $1`, runB.ID); err != nil {
		t.Fatalf("mark run b cancelled: %v", err)
	}
	if err := store.MarkRunFailed(ctxA, finished.ID, domain.ReasonMaxRetries); err != nil {
		t.Fatalf("fail finished run: %v", err)
	}
	before, err := store.GetRun(ctxA, runA.ID)
	if err != nil {
		t.Fatalf("get run a before reset: %v", err)
	}

	if err := store.ResetRunningRuns(context.Background()); err != nil {
		t.Fatalf("reset running runs: %v", err)
	}

	cases := []struct {
		ctx    context.Context
		id     string
		tenant string
	}{
		{ctxA, runA.ID, "tenant-a"},
		{ctxB, runB.ID, "tenant-b"},
	}
	for _, tc := range cases {
		run, err := store.GetRun(tc.ctx, tc.id)
		if err != nil {
			t.Fatalf("get run %s after reset: %v", tc.id, err)
		}
		if run.Status != domain.RunQueued || run.Attempt != 0 || run.NextAttemptAt != nil {
			t.Fatalf("run %s after reset = %+v", tc.tenant, run)
		}
		if !run.UpdatedAt.After(before.UpdatedAt) {
			t.Fatalf("run %s updated_at did not advance: %s", tc.tenant, run.UpdatedAt)
		}
	}

	var cancelB bool
	if err := env.Pool.QueryRow(context.Background(),
		`select cancel_requested from runs where id::text = $1`, runB.ID).Scan(&cancelB); err != nil {
		t.Fatalf("read cancel_requested: %v", err)
	}
	if !cancelB {
		t.Fatalf("reset cleared cancel_requested on run b")
	}

	finishedRun, err := store.GetRun(ctxA, finished.ID)
	if err != nil {
		t.Fatalf("get finished run: %v", err)
	}
	if finishedRun.Status != domain.RunFailed || finishedRun.EndedAt == nil {
		t.Fatalf("reset touched a non-running run: %+v", finishedRun)
	}

	runnable, err := store.ListRunnableRuns(context.Background())
	if err != nil {
		t.Fatalf("list runnable runs: %v", err)
	}
	seen := map[string]bool{}
	for _, run := range runnable {
		seen[run.ID] = true
	}
	if !seen[runA.ID] || !seen[runB.ID] || seen[finished.ID] {
		t.Fatalf("runnable runs after reset = %+v", runnable)
	}
}

func TestCompleteRunStepWritesCheckpointSpanAndCompletion(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)
	ctxA := repository.WithTenant(context.Background(), "tenant-a")
	run := createTestRun(t, store, ctxA, "tenant-a", "agent-a")
	if _, err := store.ClaimRun(context.Background(), run.ID); err != nil {
		t.Fatalf("claim run: %v", err)
	}

	output := json.RawMessage(`{"answer":"done"}`)
	checkpoint := domain.Checkpoint{
		RunID:     run.ID,
		TenantID:  "tenant-a",
		StepIndex: 0,
		StepType:  domain.StepDone,
		Result:    json.RawMessage(`{"output":{"answer":"done"}}`),
		CreatedAt: time.Now(),
	}
	runID := run.ID
	span := domain.Span{
		ID:         ids.New(),
		TenantID:   "tenant-a",
		RunID:      &runID,
		StepIndex:  0,
		Attempt:    0,
		Name:       "step.done",
		StartedAt:  time.Now(),
		EndedAt:    time.Now(),
		Attributes: json.RawMessage(`{}`),
	}
	if err := store.CompleteRunStep(ctxA, checkpoint, span, output); err != nil {
		t.Fatalf("complete run step: %v", err)
	}

	fetched, err := store.GetRun(ctxA, run.ID)
	if err != nil {
		t.Fatalf("get completed run: %v", err)
	}
	if fetched.Status != domain.RunCompleted || fetched.EndedAt == nil || fetched.Attempt != 0 {
		t.Fatalf("completed run = %+v", fetched)
	}
	var gotOutput struct {
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal(fetched.Output, &gotOutput); err != nil || gotOutput.Answer != "done" {
		t.Fatalf("completed run output = %s (%v)", fetched.Output, err)
	}

	steps, err := store.GetSteps(ctxA, run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	if len(steps) != 1 || steps[0].StepIndex != 0 || steps[0].StepType != domain.StepDone {
		t.Fatalf("steps = %+v", steps)
	}

	var spanCount int
	if err := env.Pool.QueryRow(context.Background(), `select count(*) from spans where run_id::text = $1`, run.ID).Scan(&spanCount); err != nil {
		t.Fatalf("count spans: %v", err)
	}
	if spanCount != 1 {
		t.Fatalf("span count = %d, want 1", spanCount)
	}
}

func TestCompleteRunStepRollsBackWhenSpanCannotBeWritten(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)
	ctxA := repository.WithTenant(context.Background(), "tenant-a")
	run := createTestRun(t, store, ctxA, "tenant-a", "agent-a")
	if _, err := store.ClaimRun(context.Background(), run.ID); err != nil {
		t.Fatalf("claim run: %v", err)
	}

	spanID := ids.New()
	if _, err := env.Pool.Exec(context.Background(),
		`insert into spans (id, tenant_id, run_id, step_index, attempt, name, started_at, ended_at)
		 values ($1::uuid, 'tenant-a', $2::uuid, 0, 0, 'existing', now(), now())`,
		spanID, run.ID); err != nil {
		t.Fatalf("seed span: %v", err)
	}

	runID := run.ID
	err := store.CompleteRunStep(ctxA, domain.Checkpoint{
		RunID:     run.ID,
		TenantID:  "tenant-a",
		StepIndex: 0,
		StepType:  domain.StepDone,
		Result:    json.RawMessage(`{"output":{}}`),
		CreatedAt: time.Now(),
	}, domain.Span{
		ID:        spanID,
		TenantID:  "tenant-a",
		RunID:     &runID,
		StepIndex: 0,
		Name:      "step.done",
		StartedAt: time.Now(),
		EndedAt:   time.Now(),
	}, json.RawMessage(`{}`))
	if err == nil {
		t.Fatalf("complete run step with duplicate span id succeeded")
	}

	var checkpointCount int
	if err := env.Pool.QueryRow(context.Background(), `select count(*) from checkpoints where run_id::text = $1`, run.ID).Scan(&checkpointCount); err != nil {
		t.Fatalf("count checkpoints: %v", err)
	}
	if checkpointCount != 0 {
		t.Fatalf("checkpoint count = %d after rollback, want 0", checkpointCount)
	}
	fetched, err := store.GetRun(ctxA, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if fetched.Status != domain.RunRunning || fetched.EndedAt != nil {
		t.Fatalf("run after rollback = %+v", fetched)
	}
}

func TestGetStepsOrderingAndTenantScope(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)
	ctxA := repository.WithTenant(context.Background(), "tenant-a")
	ctxB := repository.WithTenant(context.Background(), "tenant-b")
	run := createTestRun(t, store, ctxA, "tenant-a", "agent-a")

	for _, index := range []int{1, 0} {
		if _, err := env.Pool.Exec(context.Background(),
			`insert into checkpoints (run_id, tenant_id, step_index, step_type, result)
			 values ($1::uuid, 'tenant-a', $2, 'done', $3)`,
			run.ID, index, json.RawMessage(`{"output":{}}`)); err != nil {
			t.Fatalf("seed checkpoint %d: %v", index, err)
		}
	}

	steps, err := store.GetSteps(ctxA, run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	if len(steps) != 2 || steps[0].StepIndex != 0 || steps[1].StepIndex != 1 {
		t.Fatalf("steps order = %+v", steps)
	}

	if _, err := store.GetSteps(ctxB, run.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant get steps error = %v, want ErrNotFound", err)
	}
	if _, err := store.GetSteps(ctxA, ids.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing get steps error = %v, want ErrNotFound", err)
	}
	if _, err := store.GetSteps(context.Background(), run.ID); !errors.Is(err, domain.ErrNoTenant) {
		t.Fatalf("get steps without tenant error = %v, want ErrNoTenant", err)
	}
}

func TestListSpansOrderingAndTenantScope(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)
	ctxA := repository.WithTenant(context.Background(), "tenant-a")
	ctxB := repository.WithTenant(context.Background(), "tenant-b")
	run := createTestRun(t, store, ctxA, "tenant-a", "agent-a")
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	seed := []struct {
		id    string
		start time.Time
	}{
		{"00000000-0000-4000-8000-000000000001", base},
		{"00000000-0000-4000-8000-000000000002", base},
		{"00000000-0000-4000-8000-000000000003", base.Add(-time.Second)},
	}
	for i, span := range seed {
		if _, err := env.Pool.Exec(context.Background(),
			`insert into spans (id, tenant_id, run_id, step_index, attempt, name, started_at, ended_at, attributes, error)
			 values ($1::uuid, 'tenant-a', $2::uuid, 0, $3, $4, $5, $5, '{}', '')`,
			span.id, run.ID, i, "span"+span.id, span.start); err != nil {
			t.Fatalf("seed span %s: %v", span.id, err)
		}
	}
	if _, err := env.Pool.Exec(context.Background(),
		`insert into spans (id, tenant_id, run_id, step_index, attempt, name, started_at, ended_at)
		 values ($1::uuid, 'tenant-b', $2::uuid, 0, 0, 'foreign', $3, $3)`,
		ids.New(), run.ID, base); err != nil {
		t.Fatalf("seed foreign span: %v", err)
	}

	spans, err := store.ListSpans(ctxA, run.ID)
	if err != nil {
		t.Fatalf("list spans: %v", err)
	}
	if len(spans) != 3 {
		t.Fatalf("spans = %+v", spans)
	}
	want := []string{
		"00000000-0000-4000-8000-000000000003",
		"00000000-0000-4000-8000-000000000001",
		"00000000-0000-4000-8000-000000000002",
	}
	for i, id := range want {
		if spans[i].ID != id || spans[i].RunID == nil || *spans[i].RunID != run.ID {
			t.Fatalf("span %d = %+v, want id %s", i, spans[i], id)
		}
	}
	if _, err := store.ListSpans(ctxB, run.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant list spans error = %v, want ErrNotFound", err)
	}
	if _, err := store.ListSpans(ctxA, ids.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing list spans error = %v, want ErrNotFound", err)
	}
	if _, err := store.ListSpans(context.Background(), run.ID); !errors.Is(err, domain.ErrNoTenant) {
		t.Fatalf("list spans without tenant error = %v, want ErrNoTenant", err)
	}
}

func TestCompleteLLMCallStepWritesArtifactsAndResetsAttempt(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)
	ctxA := repository.WithTenant(context.Background(), "tenant-a")
	ctxB := repository.WithTenant(context.Background(), "tenant-b")
	run := createTestRun(t, store, ctxA, "tenant-a", "agent-a")
	if _, err := store.ClaimRun(context.Background(), run.ID); err != nil {
		t.Fatalf("claim run: %v", err)
	}
	if err := store.RescheduleStep(context.Background(), run.ID, 2, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("reschedule run: %v", err)
	}

	runID := run.ID
	checkpoint := domain.Checkpoint{
		RunID:     run.ID,
		TenantID:  "tenant-a",
		StepIndex: 0,
		StepType:  domain.StepLLMCall,
		Result:    json.RawMessage(`{"route":"route:default","content":"hi"}`),
		CreatedAt: time.Now(),
	}
	span := domain.Span{
		ID:         ids.New(),
		TenantID:   "tenant-a",
		RunID:      &runID,
		StepIndex:  0,
		Attempt:    1,
		Name:       "model.route:default",
		StartedAt:  time.Now(),
		EndedAt:    time.Now(),
		Attributes: json.RawMessage(`{"status":"ok"}`),
	}
	log := domain.ModelCallLog{
		TenantID:       "tenant-a",
		RunID:          &runID,
		RouteLabel:     "route:default",
		RequestedModel: "route:default",
		Chain:          []string{"route:default"},
		EffectiveModel: "route:default",
		InputTokens:    10,
		OutputTokens:   5,
		LatencyMS:      7,
		Status:         domain.ModelStatusOK,
	}
	if err := store.CompleteLLMCallStep(ctxA, checkpoint, []domain.Span{span}, log); err != nil {
		t.Fatalf("complete llm call step: %v", err)
	}

	fetched, err := store.GetRun(ctxA, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if fetched.Status != domain.RunRunning || fetched.Attempt != 0 || fetched.NextAttemptAt != nil || fetched.EndedAt != nil {
		t.Fatalf("run after llm call = %+v", fetched)
	}

	steps, err := store.GetSteps(ctxA, run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	if len(steps) != 1 || steps[0].StepIndex != 0 || steps[0].StepType != domain.StepLLMCall {
		t.Fatalf("steps = %+v", steps)
	}

	var spanName string
	if err := env.Pool.QueryRow(context.Background(),
		`select name from spans where run_id::text = $1`, run.ID).Scan(&spanName); err != nil {
		t.Fatalf("read span: %v", err)
	}
	if spanName != "model.route:default" {
		t.Fatalf("span name = %q", spanName)
	}

	var chain []string
	var inputTokens, outputTokens int
	var logStatus string
	if err := env.Pool.QueryRow(context.Background(),
		`select chain, input_tokens, output_tokens, status from model_call_logs where run_id::text = $1`,
		run.ID).Scan(&chain, &inputTokens, &outputTokens, &logStatus); err != nil {
		t.Fatalf("read model call log: %v", err)
	}
	if len(chain) != 1 || chain[0] != "route:default" || inputTokens != 10 || outputTokens != 5 || logStatus != domain.ModelStatusOK {
		t.Fatalf("model call log = chain %v tokens %d/%d status %q", chain, inputTokens, outputTokens, logStatus)
	}

	if err := store.CompleteLLMCallStep(ctxB, checkpoint, []domain.Span{span}, log); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant complete error = %v, want ErrNotFound", err)
	}
	if err := store.CompleteLLMCallStep(context.Background(), checkpoint, []domain.Span{span}, log); !errors.Is(err, domain.ErrNoTenant) {
		t.Fatalf("complete without tenant error = %v, want ErrNoTenant", err)
	}
	if err := store.RecordModelCallLog(ctxB, log); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant record error = %v, want ErrNotFound", err)
	}
	if err := store.RecordModelCallLog(context.Background(), log); !errors.Is(err, domain.ErrNoTenant) {
		t.Fatalf("record without tenant error = %v, want ErrNoTenant", err)
	}
	failureLog := log
	failureLog.InputTokens = 0
	failureLog.OutputTokens = 0
	failureLog.Status = domain.ModelStatusError
	if err := store.RecordModelCallLog(ctxA, failureLog); err != nil {
		t.Fatalf("record failure log: %v", err)
	}
	var logCount int
	if err := env.Pool.QueryRow(context.Background(),
		`select count(*) from model_call_logs where run_id::text = $1`, run.ID).Scan(&logCount); err != nil {
		t.Fatalf("count model call logs: %v", err)
	}
	if logCount != 2 {
		t.Fatalf("model call log count = %d, want 2", logCount)
	}
}

func TestMarkRunFailedIsIdempotent(t *testing.T) {
	env := testutil.New(t)
	store := repository.New(env.Pool)
	ctxA := repository.WithTenant(context.Background(), "tenant-a")
	run := createTestRun(t, store, ctxA, "tenant-a", "agent-a")
	if _, err := store.ClaimRun(context.Background(), run.ID); err != nil {
		t.Fatalf("claim run: %v", err)
	}
	if err := store.MarkRunFailed(context.Background(), run.ID, domain.ReasonPolicyUnreachable); err != nil {
		t.Fatalf("mark run failed: %v", err)
	}
	first, err := store.GetRun(ctxA, run.ID)
	if err != nil {
		t.Fatalf("get failed run: %v", err)
	}
	if first.Status != domain.RunFailed || first.TerminalReason == nil ||
		*first.TerminalReason != domain.ReasonPolicyUnreachable || first.EndedAt == nil {
		t.Fatalf("failed run = %+v", first)
	}

	if err := store.MarkRunFailed(context.Background(), run.ID, domain.ReasonMaxRetries); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("second mark run failed error = %v, want ErrNotFound", err)
	}
	second, err := store.GetRun(ctxA, run.ID)
	if err != nil {
		t.Fatalf("get run after duplicate failure: %v", err)
	}
	if second.Status != first.Status || second.Attempt != first.Attempt ||
		second.TerminalReason == nil || *second.TerminalReason != *first.TerminalReason ||
		!second.UpdatedAt.Equal(first.UpdatedAt) || second.EndedAt == nil ||
		!second.EndedAt.Equal(*first.EndedAt) || second.NextAttemptAt != nil {
		t.Fatalf("run changed after duplicate failure: first=%+v second=%+v", first, second)
	}
	if string(second.Output) != string(first.Output) || string(second.Input) != string(first.Input) {
		t.Fatalf("run payload changed after duplicate failure: first=%+v second=%+v", first, second)
	}
}

func createTestRun(t *testing.T, store *repository.Store, ctx context.Context, tenantID, agentID string) domain.Run {
	t.Helper()
	run, err := store.CreateRun(ctx, domain.Run{
		ID:       ids.New(),
		TenantID: tenantID,
		AgentID:  agentID,
		Status:   domain.RunQueued,
		Input:    json.RawMessage(`{"order_id":"A1"}`),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return run
}

func hashHex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
