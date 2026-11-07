package domain

import (
	"context"
	"encoding/json"
	"time"
)

type Store interface {
	CreateTenant(ctx context.Context, tenant Tenant) error
	GetTenant(ctx context.Context, id string) (Tenant, error)
	LookupTenantByAPIKey(ctx context.Context, apiKeyHash string) (Tenant, error)
	CreateAgent(ctx context.Context, agent Agent) (Agent, error)
	ListAgents(ctx context.Context) ([]Agent, error)
	GetAgent(ctx context.Context, id string) (Agent, error)
	CreateRun(ctx context.Context, run Run) (Run, error)
	GetRun(ctx context.Context, id string) (Run, error)
	GetSteps(ctx context.Context, runID string) ([]Checkpoint, error)
	ListRunnableRuns(ctx context.Context) ([]Run, error)
	ClaimRun(ctx context.Context, id string) (bool, error)
	RescheduleStep(ctx context.Context, id string, attempt int, nextAttemptAt time.Time) error
	MarkRunFailed(ctx context.Context, id string, reason TerminalReason) error
	CompleteRunStep(ctx context.Context, checkpoint Checkpoint, span Span, output json.RawMessage) error
	CompleteLLMCallStep(ctx context.Context, checkpoint Checkpoint, span Span, log ModelCallLog) error
	RecordModelCallLog(ctx context.Context, log ModelCallLog) error
}
