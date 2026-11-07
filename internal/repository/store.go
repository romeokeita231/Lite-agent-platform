package repository

import (
	"context"
	"encoding/json"
	"errors"

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
		`insert into runs (id, tenant_id, agent_id, status, input)
		 values ($1::uuid, $2, $3, $4, $5)
		 returning status, input, created_at, updated_at`,
		run.ID, run.TenantID, run.AgentID, string(status), input,
	).Scan(&statusOut, &run.Input, &run.CreatedAt, &run.UpdatedAt)
	if err != nil {
		return domain.Run{}, err
	}
	run.Status = domain.RunStatus(statusOut)
	return run, nil
}

func (s *Store) GetRun(ctx context.Context, id string) (domain.Run, error) {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return domain.Run{}, err
	}
	var run domain.Run
	var status string
	var terminalReason *string
	err = s.pool.QueryRow(ctx,
		`select id::text, tenant_id, agent_id, status, input, output, attempt, next_attempt_at,
		        pending_step, human_prompt, deadline, terminal_reason, eval_id,
		        created_at, updated_at, ended_at
		 from runs where id::text = $1 and tenant_id = $2`,
		id, tenantID,
	).Scan(&run.ID, &run.TenantID, &run.AgentID, &status, &run.Input, &run.Output,
		&run.Attempt, &run.NextAttemptAt, &run.PendingStep, &run.HumanPrompt, &run.Deadline,
		&terminalReason, &run.EvalID, &run.CreatedAt, &run.UpdatedAt, &run.EndedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Run{}, domain.ErrNotFound
	}
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

func uniqueToConflict(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return domain.ErrConflict
	}
	return err
}
