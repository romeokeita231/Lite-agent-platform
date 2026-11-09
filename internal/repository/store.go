package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
)

var _ domain.Store = (*Store)(nil)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// CreateTenant is tenant-scoped: the tenant ID must match the context tenant.
func (s *Store) CreateTenant(ctx context.Context, tenant domain.Tenant) error {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return err
	}
	if tenant.ID != tenantID {
		return domain.ErrNotFound
	}
	_, err = s.pool.Exec(ctx,
		`insert into tenants (id, name, api_key_hash, api_key_prefix) values ($1, $2, nullif($3, ''), nullif($4, ''))`,
		tenant.ID, tenant.Name, tenant.APIKeyHash, tenant.APIKeyPrefix)
	return err
}

func (s *Store) GetTenant(ctx context.Context, id string) (domain.Tenant, error) {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return domain.Tenant{}, err
	}
	var tenant domain.Tenant
	err = s.pool.QueryRow(ctx,
		`select id, name, coalesce(api_key_hash, ''), coalesce(api_key_prefix, ''), created_at
		 from tenants where id = $1 and id = $2`,
		id, tenantID,
	).Scan(&tenant.ID, &tenant.Name, &tenant.APIKeyHash, &tenant.APIKeyPrefix, &tenant.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Tenant{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Tenant{}, err
	}
	return tenant, nil
}

// LookupTenantByAPIKey is the one method that intentionally ignores the tenant filter.
func (s *Store) LookupTenantByAPIKey(ctx context.Context, apiKeyHash string) (domain.Tenant, error) {
	var tenant domain.Tenant
	err := s.pool.QueryRow(ctx,
		`select id, name, coalesce(api_key_hash, ''), coalesce(api_key_prefix, ''), created_at
		 from tenants where api_key_hash = $1`,
		apiKeyHash,
	).Scan(&tenant.ID, &tenant.Name, &tenant.APIKeyHash, &tenant.APIKeyPrefix, &tenant.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Tenant{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Tenant{}, err
	}
	return tenant, nil
}

func (s *Store) CreateAgent(ctx context.Context, agent domain.Agent) (domain.Agent, error) {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return domain.Agent{}, err
	}
	if agent.TenantID != tenantID {
		return domain.Agent{}, domain.ErrNotFound
	}
	err = s.pool.QueryRow(ctx,
		`insert into agents (id, tenant_id, name, policy_url, secret_env) values ($1, $2, $3, $4, $5)
		 returning created_at`,
		agent.ID, agent.TenantID, agent.Name, agent.PolicyURL, agent.SecretEnv,
	).Scan(&agent.CreatedAt)
	if err != nil {
		return domain.Agent{}, uniqueToConflict(err)
	}
	return agent, nil
}

func (s *Store) ListAgents(ctx context.Context) ([]domain.Agent, error) {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`select id, tenant_id, name, policy_url, secret_env, created_at
		 from agents where tenant_id = $1 order by name`,
		tenantID)
	if err != nil {
		return nil, err
	}
	agents, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Agent, error) {
		var agent domain.Agent
		err := row.Scan(&agent.ID, &agent.TenantID, &agent.Name, &agent.PolicyURL, &agent.SecretEnv, &agent.CreatedAt)
		return agent, err
	})
	if err != nil {
		return nil, err
	}
	if agents == nil {
		agents = []domain.Agent{}
	}
	return agents, nil
}

func (s *Store) CreateRun(ctx context.Context, run domain.Run) (domain.Run, error) {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return domain.Run{}, err
	}
	if run.TenantID != tenantID {
		return domain.Run{}, domain.ErrNotFound
	}
	status := run.Status
	if status == "" {
		status = domain.RunQueued
	}
	input := run.Input
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	var statusOut string
	err = s.pool.QueryRow(ctx,
		`insert into runs (id, tenant_id, agent_id, status, input, eval_id)
		 values ($1::uuid, $2, $3, $4, $5, $6)
		 returning status, input, created_at, updated_at`,
		run.ID, run.TenantID, run.AgentID, string(status), input, run.EvalID,
	).Scan(&statusOut, &run.Input, &run.CreatedAt, &run.UpdatedAt)
	if err != nil {
		return domain.Run{}, err
	}
	run.Status = domain.RunStatus(statusOut)
	return run, nil
}

func (s *Store) ListRuns(ctx context.Context, status domain.RunStatus, agentID string, limit int) ([]domain.Run, error) {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		runColumns+` from runs
		 where tenant_id = $1
		   and ($2 = '' or status = $2)
		   and ($3 = '' or agent_id = $3)
		 order by created_at desc, id desc
		 limit $4`,
		tenantID, string(status), agentID, limit)
	if err != nil {
		return nil, err
	}
	runs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Run, error) {
		return scanRun(row)
	})
	if err != nil {
		return nil, err
	}
	if runs == nil {
		runs = []domain.Run{}
	}
	return runs, nil
}

// CountTenantRunsByStatus scopes counts to the calling tenant.
func (s *Store) CountTenantRunsByStatus(ctx context.Context) (map[domain.RunStatus]int64, error) {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `select status, count(*) from runs where tenant_id = $1 group by status`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[domain.RunStatus]int64)
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		counts[domain.RunStatus(status)] = count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return counts, nil
}

func (s *Store) GetRun(ctx context.Context, id string) (domain.Run, error) {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return domain.Run{}, err
	}
	run, err := scanRun(s.pool.QueryRow(ctx,
		runColumns+` from runs where id::text = $1 and tenant_id = $2`,
		id, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Run{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Run{}, err
	}
	return run, nil
}

func (s *Store) GetAgent(ctx context.Context, id string) (domain.Agent, error) {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return domain.Agent{}, err
	}
	var agent domain.Agent
	err = s.pool.QueryRow(ctx,
		`select id, tenant_id, name, policy_url, secret_env, created_at
		 from agents where id = $1 and tenant_id = $2`,
		id, tenantID,
	).Scan(&agent.ID, &agent.TenantID, &agent.Name, &agent.PolicyURL, &agent.SecretEnv, &agent.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Agent{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Agent{}, err
	}
	return agent, nil
}

// ResetRunningRuns is a startup recovery method that intentionally ignores the tenant filter.
func (s *Store) ResetRunningRuns(ctx context.Context) error {
	_, err := s.pool.Exec(ctx,
		`update runs set status = 'queued', attempt = 0, next_attempt_at = null, updated_at = now()
		 where status = 'running'`)
	return err
}

// CountRunsByStatus is a metrics method that intentionally ignores the tenant filter.
func (s *Store) CountRunsByStatus(ctx context.Context) (map[domain.RunStatus]int64, error) {
	rows, err := s.pool.Query(ctx, `select status, count(*) from runs group by status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[domain.RunStatus]int64)
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		counts[domain.RunStatus(status)] = count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return counts, nil
}

// ListRunnableRuns is a dispatcher method that intentionally ignores the tenant filter.
func (s *Store) ListRunnableRuns(ctx context.Context) ([]domain.Run, error) {
	rows, err := s.pool.Query(ctx,
		`select id::text, tenant_id, agent_id, input, attempt
		 from runs
		 where status = 'queued' and (next_attempt_at is null or next_attempt_at <= now())
		 order by created_at, id`)
	if err != nil {
		return nil, err
	}
	runs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Run, error) {
		var run domain.Run
		err := row.Scan(&run.ID, &run.TenantID, &run.AgentID, &run.Input, &run.Attempt)
		run.Status = domain.RunQueued
		return run, err
	})
	if err != nil {
		return nil, err
	}
	if runs == nil {
		runs = []domain.Run{}
	}
	return runs, nil
}

func (s *Store) ClaimRun(ctx context.Context, id string) (bool, error) {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx,
		`update runs set status = 'running', updated_at = now()
		 where id::text = $1 and tenant_id = $2 and status = 'queued'`,
		id, tenantID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) RescheduleStep(ctx context.Context, id string, attempt int, nextAttemptAt time.Time) error {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`update runs set attempt = $2, next_attempt_at = $3, updated_at = now()
		 where id::text = $1 and tenant_id = $4 and status = 'running'`,
		id, attempt, nextAttemptAt, tenantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrNotFound
	}
	return nil
}

func (s *Store) MarkRunFailed(ctx context.Context, id string, reason domain.TerminalReason) error {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`update runs set status = 'failed', terminal_reason = $2, ended_at = now(), updated_at = now()
		 where id::text = $1 and tenant_id = $3 and status in ('queued', 'running')`,
		id, string(reason), tenantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrNotFound
	}
	return nil
}

func (s *Store) RequestCancel(ctx context.Context, id string) (domain.Run, error) {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return domain.Run{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Run{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	run, err := scanRun(tx.QueryRow(ctx,
		runColumns+` from runs where id::text = $1 and tenant_id = $2 for update`,
		id, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Run{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Run{}, err
	}

	switch {
	case run.Status.Terminal():
		return domain.Run{}, domain.ErrConflict
	case run.Status == domain.RunWaitingHuman:
		tag, err := tx.Exec(ctx,
			`update runs set status = 'cancelled', terminal_reason = $3, cancel_requested = true,
			        pending_step = null, human_prompt = '', deadline = null,
			        attempt = 0, next_attempt_at = null, ended_at = now(), updated_at = now()
			 where id::text = $1 and tenant_id = $2 and status = 'waiting_human'`,
			id, tenantID, string(domain.ReasonCancelledByTenant))
		if err != nil {
			return domain.Run{}, err
		}
		if tag.RowsAffected() != 1 {
			return domain.Run{}, domain.ErrConflict
		}
	default:
		tag, err := tx.Exec(ctx,
			`update runs set cancel_requested = true, updated_at = now()
			 where id::text = $1 and tenant_id = $2`,
			id, tenantID)
		if err != nil {
			return domain.Run{}, err
		}
		if tag.RowsAffected() != 1 {
			return domain.Run{}, domain.ErrNotFound
		}
	}

	run, err = scanRun(tx.QueryRow(ctx,
		runColumns+` from runs where id::text = $1 and tenant_id = $2`,
		id, tenantID))
	if err != nil {
		return domain.Run{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Run{}, err
	}
	return run, nil
}

func (s *Store) CancelRequested(ctx context.Context, id string) (bool, error) {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return false, err
	}
	var requested bool
	err = s.pool.QueryRow(ctx,
		`select cancel_requested from runs where id::text = $1 and tenant_id = $2`,
		id, tenantID,
	).Scan(&requested)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, domain.ErrNotFound
	}
	if err != nil {
		return false, err
	}
	return requested, nil
}

func (s *Store) MarkCancelled(ctx context.Context, id string) error {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`update runs set status = 'cancelled', terminal_reason = $2, cancel_requested = true,
		        ended_at = now(), updated_at = now()
		 where id::text = $1 and tenant_id = $3 and status in ('queued', 'running')`,
		id, string(domain.ReasonCancelledByTenant), tenantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := s.pool.QueryRow(ctx,
			`select exists(select 1 from runs where id::text = $1 and tenant_id = $2)`,
			id, tenantID,
		).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return domain.ErrNotFound
		}
	}
	return nil
}

func (s *Store) EnterHumanReview(ctx context.Context, id string, attempt int, pendingStep json.RawMessage, prompt string, deadline time.Time) error {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`update runs set status = 'waiting_human', pending_step = $2, human_prompt = $3, deadline = $4,
		        attempt = $5, next_attempt_at = null, updated_at = now()
		 where id::text = $1 and tenant_id = $6 and status = 'running'`,
		id, pendingStep, prompt, deadline, attempt, tenantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrNotFound
	}
	return nil
}

func (s *Store) SubmitHumanInput(ctx context.Context, checkpoint domain.Checkpoint, span domain.Span) (domain.Run, error) {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return domain.Run{}, err
	}
	if checkpoint.TenantID != tenantID || span.TenantID != tenantID {
		return domain.Run{}, domain.ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Run{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	if err := tx.QueryRow(ctx,
		`select status from runs where id::text = $1 and tenant_id = $2 for update`,
		checkpoint.RunID, tenantID,
	).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Run{}, domain.ErrNotFound
		}
		return domain.Run{}, err
	}
	var pendingIndex int
	if err := tx.QueryRow(ctx,
		`select coalesce(max(step_index) + 1, 0) from checkpoints
		 where run_id::text = $1 and tenant_id = $2`,
		checkpoint.RunID, tenantID,
	).Scan(&pendingIndex); err != nil {
		return domain.Run{}, err
	}
	if domain.RunStatus(status) != domain.RunWaitingHuman || pendingIndex != checkpoint.StepIndex {
		return domain.Run{}, domain.ErrConflict
	}
	if err := insertCheckpoint(ctx, tx, checkpoint); err != nil {
		return domain.Run{}, uniqueToConflict(err)
	}
	if err := insertSpans(ctx, tx, tenantID, &checkpoint.RunID, []domain.Span{span}); err != nil {
		return domain.Run{}, err
	}
	tag, err := tx.Exec(ctx,
		`update runs set status = 'queued', pending_step = null, human_prompt = '', deadline = null,
		        attempt = 0, next_attempt_at = null, updated_at = now()
		 where id::text = $1 and tenant_id = $2 and status = 'waiting_human'`,
		checkpoint.RunID, tenantID)
	if err != nil {
		return domain.Run{}, err
	}
	if tag.RowsAffected() != 1 {
		return domain.Run{}, domain.ErrConflict
	}
	run, err := scanRun(tx.QueryRow(ctx,
		runColumns+` from runs where id::text = $1 and tenant_id = $2`,
		checkpoint.RunID, tenantID))
	if err != nil {
		return domain.Run{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Run{}, err
	}
	return run, nil
}

// FailExpiredHumanWaits is a dispatcher method that intentionally ignores the tenant filter.
func (s *Store) FailExpiredHumanWaits(ctx context.Context) error {
	_, err := s.pool.Exec(ctx,
		`update runs set status = 'failed', terminal_reason = $1, ended_at = now(), updated_at = now()
		 where status = 'waiting_human' and deadline < now()`,
		string(domain.ReasonStepTimeout))
	return err
}

func (s *Store) GetSteps(ctx context.Context, runID string) ([]domain.Checkpoint, error) {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`select exists(select 1 from runs where id::text = $1 and tenant_id = $2)`,
		runID, tenantID,
	).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, domain.ErrNotFound
	}
	rows, err := s.pool.Query(ctx,
		`select run_id::text, tenant_id, step_index, step_type, result, created_at
		 from checkpoints where tenant_id = $1 and run_id::text = $2
		 order by step_index asc`,
		tenantID, runID)
	if err != nil {
		return nil, err
	}
	steps, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Checkpoint, error) {
		var checkpoint domain.Checkpoint
		var stepType string
		err := row.Scan(&checkpoint.RunID, &checkpoint.TenantID, &checkpoint.StepIndex, &stepType, &checkpoint.Result, &checkpoint.CreatedAt)
		checkpoint.StepType = domain.StepType(stepType)
		return checkpoint, err
	})
	if err != nil {
		return nil, err
	}
	if steps == nil {
		steps = []domain.Checkpoint{}
	}
	return steps, nil
}

func (s *Store) ListSpans(ctx context.Context, runID string) ([]domain.Span, error) {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`select exists(select 1 from runs where id::text = $1 and tenant_id = $2)`,
		runID, tenantID,
	).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, domain.ErrNotFound
	}
	rows, err := s.pool.Query(ctx,
		`select id::text, tenant_id, run_id::text, step_index, attempt, name, started_at, ended_at,
		        coalesce(attributes, '{}'::jsonb), error
		 from spans where tenant_id = $1 and run_id::text = $2
		 order by started_at asc, id asc`,
		tenantID, runID)
	if err != nil {
		return nil, err
	}
	spans, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Span, error) {
		var span domain.Span
		var spanRunID *string
		err := row.Scan(&span.ID, &span.TenantID, &spanRunID, &span.StepIndex, &span.Attempt, &span.Name,
			&span.StartedAt, &span.EndedAt, &span.Attributes, &span.Error)
		span.RunID = spanRunID
		return span, err
	})
	if err != nil {
		return nil, err
	}
	if spans == nil {
		spans = []domain.Span{}
	}
	return spans, nil
}

func (s *Store) CompleteRunStep(ctx context.Context, checkpoint domain.Checkpoint, span domain.Span, output json.RawMessage) error {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return err
	}
	if checkpoint.TenantID != tenantID || span.TenantID != tenantID {
		return domain.ErrNotFound
	}
	if len(output) == 0 {
		output = json.RawMessage("{}")
	}
	runID := checkpoint.RunID
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := insertCheckpoint(ctx, tx, checkpoint); err != nil {
		return err
	}
	if err := insertSpans(ctx, tx, tenantID, &runID, []domain.Span{span}); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx,
		`update runs set status = 'completed', output = $2, attempt = 0, next_attempt_at = null,
		        terminal_reason = null, ended_at = now(), updated_at = now()
		 where id::text = $1 and tenant_id = $3 and status in ('queued', 'running')`,
		runID, output, tenantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrNotFound
	}
	return tx.Commit(ctx)
}

func (s *Store) CompleteLLMCallStep(ctx context.Context, checkpoint domain.Checkpoint, spans []domain.Span, log domain.ModelCallLog) error {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return err
	}
	if checkpoint.TenantID != tenantID || log.TenantID != tenantID {
		return domain.ErrNotFound
	}
	for _, span := range spans {
		if span.TenantID != tenantID {
			return domain.ErrNotFound
		}
	}
	runID := checkpoint.RunID
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := insertCheckpoint(ctx, tx, checkpoint); err != nil {
		return err
	}
	if err := insertSpans(ctx, tx, tenantID, &runID, spans); err != nil {
		return err
	}
	if err := insertModelCallLog(ctx, tx, log); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx,
		`update runs set attempt = 0, next_attempt_at = null, updated_at = now()
		 where id::text = $1 and tenant_id = $2 and status = 'running'`,
		checkpoint.RunID, tenantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrNotFound
	}
	return tx.Commit(ctx)
}

func (s *Store) RecordModelCallAttempts(ctx context.Context, spans []domain.Span, log domain.ModelCallLog) error {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return err
	}
	if log.TenantID != tenantID {
		return domain.ErrNotFound
	}
	for _, span := range spans {
		if span.TenantID != tenantID {
			return domain.ErrNotFound
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := insertSpans(ctx, tx, tenantID, log.RunID, spans); err != nil {
		return err
	}
	if err := insertModelCallLog(ctx, tx, log); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) UpsertModelRoute(ctx context.Context, route domain.ModelRoute) (domain.ModelRoute, error) {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return domain.ModelRoute{}, err
	}
	if route.TenantID != tenantID {
		return domain.ModelRoute{}, domain.ErrNotFound
	}
	if route.Chain == nil {
		route.Chain = []string{}
	}
	chain, err := json.Marshal(route.Chain)
	if err != nil {
		return domain.ModelRoute{}, err
	}
	err = s.pool.QueryRow(ctx,
		`insert into model_routes (tenant_id, label, chain) values ($1, $2, $3)
		 on conflict (tenant_id, label) do update set chain = excluded.chain
		 returning chain, created_at`,
		route.TenantID, route.Label, chain,
	).Scan(&route.Chain, &route.CreatedAt)
	if err != nil {
		return domain.ModelRoute{}, err
	}
	return route, nil
}

func (s *Store) GetModelRoute(ctx context.Context, label string) (domain.ModelRoute, error) {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return domain.ModelRoute{}, err
	}
	var route domain.ModelRoute
	err = s.pool.QueryRow(ctx,
		`select tenant_id, label, chain, created_at
		 from model_routes where tenant_id = $1 and label = $2`,
		tenantID, label,
	).Scan(&route.TenantID, &route.Label, &route.Chain, &route.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ModelRoute{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.ModelRoute{}, err
	}
	return route, nil
}

func (s *Store) ListModelRoutes(ctx context.Context) ([]domain.ModelRoute, error) {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`select tenant_id, label, chain, created_at
		 from model_routes where tenant_id = $1 order by label`,
		tenantID)
	if err != nil {
		return nil, err
	}
	routes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.ModelRoute, error) {
		var route domain.ModelRoute
		err := row.Scan(&route.TenantID, &route.Label, &route.Chain, &route.CreatedAt)
		return route, err
	})
	if err != nil {
		return nil, err
	}
	if routes == nil {
		routes = []domain.ModelRoute{}
	}
	return routes, nil
}

func (s *Store) RecordModelCallLog(ctx context.Context, log domain.ModelCallLog) error {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return err
	}
	if log.TenantID != tenantID {
		return domain.ErrNotFound
	}
	return insertModelCallLog(ctx, s.pool, log)
}

func (s *Store) ModelUsage(ctx context.Context, groupBy string, from, to *time.Time) ([]domain.UsageGroup, error) {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return nil, err
	}
	if groupBy == domain.UsageGroupByRun {
		rows, err := s.pool.Query(ctx,
			`select run_id::text, count(*), coalesce(sum(input_tokens), 0), coalesce(sum(output_tokens), 0)
			 from model_call_logs
			 where tenant_id = $1
			   and ($2::timestamptz is null or created_at >= $2::timestamptz)
			   and ($3::timestamptz is null or created_at <= $3::timestamptz)
			 group by run_id
			 order by run_id nulls first`,
			tenantID, from, to)
		if err != nil {
			return nil, err
		}
		groups, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.UsageGroup, error) {
			var group domain.UsageGroup
			err := row.Scan(&group.RunID, &group.Calls, &group.InputTokens, &group.OutputTokens)
			return group, err
		})
		if err != nil {
			return nil, err
		}
		if groups == nil {
			groups = []domain.UsageGroup{}
		}
		return groups, nil
	}
	rows, err := s.pool.Query(ctx,
		`select effective_model, count(*), coalesce(sum(input_tokens), 0), coalesce(sum(output_tokens), 0)
		 from model_call_logs
		 where tenant_id = $1
		   and ($2::timestamptz is null or created_at >= $2::timestamptz)
		   and ($3::timestamptz is null or created_at <= $3::timestamptz)
		 group by effective_model
		 order by effective_model`,
		tenantID, from, to)
	if err != nil {
		return nil, err
	}
	groups, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.UsageGroup, error) {
		var group domain.UsageGroup
		err := row.Scan(&group.Model, &group.Calls, &group.InputTokens, &group.OutputTokens)
		return group, err
	})
	if err != nil {
		return nil, err
	}
	if groups == nil {
		groups = []domain.UsageGroup{}
	}
	return groups, nil
}

func (s *Store) CreateTool(ctx context.Context, tool domain.Tool) (domain.Tool, error) {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return domain.Tool{}, err
	}
	if tool.TenantID != tenantID {
		return domain.Tool{}, domain.ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Tool{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if tool.IsDefault {
		if _, err := tx.Exec(ctx,
			`update tool_registry set is_default = false
			 where tenant_id = $1 and tool_name = $2 and is_default`,
			tenantID, tool.ToolName); err != nil {
			return domain.Tool{}, err
		}
	}
	if _, err := tx.Exec(ctx,
		`insert into tool_registry (tenant_id, tool_name, version, description, "schema", endpoint, is_default)
		 values ($1, $2, $3, $4, $5, $6, $7)`,
		tenantID, tool.ToolName, tool.Version, tool.Description, tool.Schema, tool.Endpoint, tool.IsDefault); err != nil {
		return domain.Tool{}, uniqueToConflict(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Tool{}, err
	}
	return tool, nil
}

func (s *Store) ListTools(ctx context.Context) ([]domain.Tool, error) {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`select tenant_id, tool_name, version, description, "schema", endpoint, is_default
		 from tool_registry where tenant_id = $1 order by tool_name, version`,
		tenantID)
	if err != nil {
		return nil, err
	}
	tools, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Tool, error) {
		var tool domain.Tool
		err := row.Scan(&tool.TenantID, &tool.ToolName, &tool.Version, &tool.Description,
			&tool.Schema, &tool.Endpoint, &tool.IsDefault)
		return tool, err
	})
	if err != nil {
		return nil, err
	}
	if tools == nil {
		tools = []domain.Tool{}
	}
	return tools, nil
}

func (s *Store) GetTool(ctx context.Context, toolName, version string) (domain.Tool, error) {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return domain.Tool{}, err
	}
	var tool domain.Tool
	err = s.pool.QueryRow(ctx,
		`select tenant_id, tool_name, version, description, "schema", endpoint, is_default
		 from tool_registry
		 where tenant_id = $1 and tool_name = $2
		   and (($3 <> '' and version = $3) or ($3 = '' and is_default))`,
		tenantID, toolName, version,
	).Scan(&tool.TenantID, &tool.ToolName, &tool.Version, &tool.Description,
		&tool.Schema, &tool.Endpoint, &tool.IsDefault)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Tool{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Tool{}, err
	}
	return tool, nil
}

func (s *Store) CompleteToolCallStep(ctx context.Context, checkpoint domain.Checkpoint, span domain.Span, invocation domain.ToolInvocation) error {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return err
	}
	if checkpoint.TenantID != tenantID || span.TenantID != tenantID || invocation.TenantID != tenantID {
		return domain.ErrNotFound
	}
	args := invocation.Args
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	result := invocation.Result
	if len(result) == 0 {
		result = json.RawMessage("null")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := insertCheckpoint(ctx, tx, checkpoint); err != nil {
		return err
	}
	if err := insertSpans(ctx, tx, tenantID, &checkpoint.RunID, []domain.Span{span}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`insert into tool_invocations (id, tenant_id, run_id, tool_name, version, args, result, status, created_at,
		        step_index, idempotency_key, latency_ms)
		 values ($1::uuid, $2, $3::uuid, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		invocation.ID, tenantID, checkpoint.RunID, invocation.ToolName, invocation.Version, args, result, invocation.Status, invocation.CreatedAt,
		invocation.StepIndex, invocation.IdempotencyKey, invocation.LatencyMS,
	); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx,
		`update runs set attempt = 0, next_attempt_at = null, updated_at = now()
		 where id::text = $1 and tenant_id = $2 and status = 'running'`,
		checkpoint.RunID, tenantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrNotFound
	}
	return tx.Commit(ctx)
}

func (s *Store) ListToolInvocations(ctx context.Context, runID string) ([]domain.ToolInvocation, error) {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`select id::text, tenant_id, run_id::text, tool_name, version, args, result, status, created_at,
		        step_index, idempotency_key, latency_ms
		 from tool_invocations
		 where tenant_id = $1 and run_id::text = $2
		 order by created_at asc, id asc`,
		tenantID, runID)
	if err != nil {
		return nil, err
	}
	invocations, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.ToolInvocation, error) {
		var invocation domain.ToolInvocation
		err := row.Scan(&invocation.ID, &invocation.TenantID, &invocation.RunID, &invocation.ToolName,
			&invocation.Version, &invocation.Args, &invocation.Result, &invocation.Status, &invocation.CreatedAt,
			&invocation.StepIndex, &invocation.IdempotencyKey, &invocation.LatencyMS)
		return invocation, err
	})
	if err != nil {
		return nil, err
	}
	if invocations == nil {
		invocations = []domain.ToolInvocation{}
	}
	return invocations, nil
}

func (s *Store) CreateEvalReport(ctx context.Context, report domain.EvalReport) (domain.EvalReport, error) {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return domain.EvalReport{}, err
	}
	if report.TenantID != tenantID {
		return domain.EvalReport{}, domain.ErrNotFound
	}
	results := report.Results
	if len(results) == 0 {
		results = json.RawMessage("[]")
	}
	err = s.pool.QueryRow(ctx,
		`insert into eval_reports (id, tenant_id, status, cases_snapshot, results)
		 values ($1, $2, $3, $4, $5)
		 returning created_at`,
		report.ID, report.TenantID, report.Status, report.CasesSnapshot, results,
	).Scan(&report.CreatedAt)
	if err != nil {
		return domain.EvalReport{}, err
	}
	report.Results = results
	return report, nil
}

func (s *Store) GetEvalReport(ctx context.Context, id string) (domain.EvalReport, error) {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return domain.EvalReport{}, err
	}
	var report domain.EvalReport
	err = s.pool.QueryRow(ctx,
		`select id, tenant_id, status, cases_snapshot, coalesce(results, '[]'::jsonb), created_at, ended_at
		 from eval_reports where id = $1 and tenant_id = $2`,
		id, tenantID,
	).Scan(&report.ID, &report.TenantID, &report.Status, &report.CasesSnapshot, &report.Results,
		&report.CreatedAt, &report.EndedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.EvalReport{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.EvalReport{}, err
	}
	return report, nil
}

func (s *Store) FinishEvalReport(ctx context.Context, id, status string, results json.RawMessage, endedAt time.Time) error {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return err
	}
	if len(results) == 0 {
		results = json.RawMessage("[]")
	}
	tag, err := s.pool.Exec(ctx,
		`update eval_reports set status = $3, results = $4, ended_at = $5
		 where id = $1 and tenant_id = $2`,
		id, tenantID, status, results, endedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrNotFound
	}
	return nil
}

const runColumns = `select id::text, tenant_id, agent_id, status, input, output, attempt, next_attempt_at,
	        pending_step, human_prompt, deadline, terminal_reason, eval_id,
	        created_at, updated_at, ended_at`

func scanRun(row pgx.Row) (domain.Run, error) {
	var run domain.Run
	var status string
	var terminalReason *string
	err := row.Scan(&run.ID, &run.TenantID, &run.AgentID, &status, &run.Input, &run.Output,
		&run.Attempt, &run.NextAttemptAt, &run.PendingStep, &run.HumanPrompt, &run.Deadline,
		&terminalReason, &run.EvalID, &run.CreatedAt, &run.UpdatedAt, &run.EndedAt)
	if err != nil {
		return domain.Run{}, err
	}
	run.Status = domain.RunStatus(status)
	if terminalReason != nil {
		reason := domain.TerminalReason(*terminalReason)
		run.TerminalReason = &reason
	}
	return run, nil
}

type spanInserter interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func insertSpans(ctx context.Context, db spanInserter, tenantID string, runID *string, spans []domain.Span) error {
	for _, span := range spans {
		attributes := span.Attributes
		if len(attributes) == 0 {
			attributes = json.RawMessage("{}")
		}
		if _, err := db.Exec(ctx,
			`insert into spans (id, tenant_id, run_id, step_index, attempt, name, started_at, ended_at, attributes, error)
			 values ($1::uuid, $2, $3::uuid, $4, $5, $6, $7, $8, $9, $10)`,
			span.ID, tenantID, runID, span.StepIndex, span.Attempt, span.Name, span.StartedAt, span.EndedAt, attributes, span.Error,
		); err != nil {
			return err
		}
	}
	return nil
}

func insertCheckpoint(ctx context.Context, db spanInserter, checkpoint domain.Checkpoint) error {
	_, err := db.Exec(ctx,
		`insert into checkpoints (run_id, tenant_id, step_index, step_type, result, created_at)
		 values ($1::uuid, $2, $3, $4, $5, $6)`,
		checkpoint.RunID, checkpoint.TenantID, checkpoint.StepIndex, string(checkpoint.StepType), checkpoint.Result, checkpoint.CreatedAt,
	)
	return err
}

func insertModelCallLog(ctx context.Context, db spanInserter, log domain.ModelCallLog) error {
	chain, err := modelChainJSON(log.Chain)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx,
		`insert into model_call_logs (tenant_id, run_id, route_label, requested_model, chain,
		                             effective_model, fallback_from, input_tokens, output_tokens, latency_ms, status)
		 values ($1, $2::uuid, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		log.TenantID, log.RunID, log.RouteLabel, log.RequestedModel, chain,
		log.EffectiveModel, log.FallbackFrom, log.InputTokens, log.OutputTokens, log.LatencyMS, log.Status,
	)
	return err
}

func modelChainJSON(chain []string) ([]byte, error) {
	if chain == nil {
		chain = []string{}
	}
	return json.Marshal(chain)
}

func uniqueToConflict(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return domain.ErrConflict
	}
	return err
}
