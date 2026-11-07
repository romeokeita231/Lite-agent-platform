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
	EnterHumanReview(ctx context.Context, id string, attempt int, pendingStep json.RawMessage, prompt string, deadline time.Time) error
	SubmitHumanInput(ctx context.Context, checkpoint Checkpoint, span Span) (Run, error)
	FailExpiredHumanWaits(ctx context.Context) error
	CompleteRunStep(ctx context.Context, checkpoint Checkpoint, span Span, output json.RawMessage) error
	CompleteLLMCallStep(ctx context.Context, checkpoint Checkpoint, span Span, log ModelCallLog) error
	RecordModelCallLog(ctx context.Context, log ModelCallLog) error
	CreateTool(ctx context.Context, tool Tool) (Tool, error)
	ListTools(ctx context.Context) ([]Tool, error)
	GetTool(ctx context.Context, toolName, version string) (Tool, error)
	CompleteToolCallStep(ctx context.Context, checkpoint Checkpoint, span Span, invocation ToolInvocation) error
}
