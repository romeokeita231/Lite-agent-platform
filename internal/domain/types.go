package domain

import (
	"encoding/json"
	"time"
)

type Tenant struct {
	ID        string
	Name      string
	CreatedAt time.Time
}

type Agent struct {
	ID        string
	TenantID  string
	Name      string
	PolicyURL string
	SecretEnv string
	CreatedAt time.Time
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
	ID             string
	TenantID       string
	AgentID        string
	Status         RunStatus
	Input          json.RawMessage
	Output         json.RawMessage
	Attempt        int
	NextAttemptAt  *time.Time
	PendingStep    json.RawMessage
	HumanPrompt    string
	Deadline       *time.Time
	TerminalReason *TerminalReason
	EvalID         *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	EndedAt        *time.Time
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
	TenantID string
	Label    string
	Chain    []string
}

type Tool struct {
	TenantID    string
	ToolName    string
	Version     string
	Description string
	Schema      json.RawMessage
	Endpoint    string
	IsDefault   bool
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
