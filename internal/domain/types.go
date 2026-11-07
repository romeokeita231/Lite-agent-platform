package domain

import (
	"context"
	"encoding/json"
	"time"
)

type Tenant struct {
	ID           string
	Name         string
	APIKeyHash   string
	APIKeyPrefix string
	CreatedAt    time.Time
}

type Agent struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	Name      string    `json:"name"`
	PolicyURL string    `json:"policy_url"`
	SecretEnv string    `json:"secret_env"`
	CreatedAt time.Time `json:"created_at"`
}

type RunStatus string

const (
	RunQueued       RunStatus = "queued"
	RunRunning      RunStatus = "running"
	RunWaitingHuman RunStatus = "waiting_human"
	RunCompleted    RunStatus = "completed"
	RunFailed       RunStatus = "failed"
	RunCancelled    RunStatus = "cancelled"
)

type TerminalReason string

const (
	ReasonStepTimeout         TerminalReason = "step_timeout"
	ReasonPolicyUnreachable   TerminalReason = "policy_unreachable"
	ReasonMaxRetries          TerminalReason = "max_retries"
	ReasonToolSchemaViolation TerminalReason = "tool_schema_violation"
	ReasonPolicyInvalidAction TerminalReason = "policy_invalid_action"
	ReasonCancelledByTenant   TerminalReason = "cancelled_by_tenant"
)

type StepType string

const (
	StepLLMCall     StepType = "llm_call"
	StepToolCall    StepType = "tool_call"
	StepHumanReview StepType = "human_review"
	StepDone        StepType = "done"
)

type Run struct {
	ID             string          `json:"id"`
	TenantID       string          `json:"tenant_id"`
	AgentID        string          `json:"agent_id"`
	Status         RunStatus       `json:"status"`
	Input          json.RawMessage `json:"input"`
	Output         json.RawMessage `json:"output"`
	Attempt        int             `json:"attempt"`
	NextAttemptAt  *time.Time      `json:"next_attempt_at"`
	PendingStep    json.RawMessage `json:"pending_step"`
	HumanPrompt    string          `json:"human_prompt"`
	Deadline       *time.Time      `json:"deadline"`
	TerminalReason *TerminalReason `json:"terminal_reason"`
	EvalID         *string         `json:"eval_id"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	EndedAt        *time.Time      `json:"ended_at"`
}

type Checkpoint struct {
	RunID     string
	TenantID  string
	StepIndex int
	StepType  StepType
	Result    json.RawMessage
	CreatedAt time.Time
}

type Span struct {
	ID         string
	TenantID   string
	RunID      *string
	StepIndex  int
	Attempt    int
	Name       string
	StartedAt  time.Time
	EndedAt    time.Time
	Attributes json.RawMessage
	Error      string
}

type ModelCallLog struct {
	TenantID       string
	RunID          *string
	RouteLabel     string
	RequestedModel string
	Chain          []string
	EffectiveModel string
	FallbackFrom   string
	InputTokens    int
	OutputTokens   int
	LatencyMS      int64
	Status         string
}

type ModelRoute struct {
	TenantID  string
	Label     string
	Chain     []string
	CreatedAt time.Time
}

const (
	ModelStatusOK    = "ok"
	ModelStatusError = "error"
)

type ModelMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ModelRequest struct {
	Route    string
	Messages []ModelMessage
	Extra    map[string]json.RawMessage
}

type ModelAttempt struct {
	Model     string
	Status    string
	Attempt   int
	StartedAt time.Time
	EndedAt   time.Time
	LatencyMS int64
	Error     string
}

type ModelResult struct {
	RouteLabel     string
	RequestedModel string
	Model          string
	Chain          []string
	FallbackFrom   string
	Content        string
	InputTokens    int
	OutputTokens   int
	LatencyMS      int64
	Status         string
	Attempts       []ModelAttempt
}

type ModelGateway interface {
	Generate(ctx context.Context, req ModelRequest) (ModelResult, error)
}

type Tool struct {
	TenantID    string          `json:"tenant_id"`
	ToolName    string          `json:"tool_name"`
	Version     string          `json:"version"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	Endpoint    string          `json:"endpoint"`
	IsDefault   bool            `json:"is_default"`
}

type ToolGateway interface {
	Resolve(ctx context.Context, toolName, version string) (Tool, error)
	Validate(tool Tool, args json.RawMessage) error
	Invoke(ctx context.Context, tool Tool, args json.RawMessage, idempotencyKey string) (json.RawMessage, error)
}

type ToolInvocation struct {
	ID        string
	TenantID  string
	RunID     string
	ToolName  string
	Version   string
	Args      json.RawMessage
	Result    json.RawMessage
	Status    string
	CreatedAt time.Time
}

type EvalReport struct {
	ID            string
	TenantID      string
	Status        string
	CasesSnapshot json.RawMessage
	Results       json.RawMessage
	CreatedAt     time.Time
	EndedAt       *time.Time
}
