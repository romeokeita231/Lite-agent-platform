package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/ids"
	"github.com/romeokeita231/Lite-agent-platform/internal/repository"
)

type Options struct {
	StepTimeout     time.Duration
	MaxStepAttempts int
	PollInterval    time.Duration
}

type Dispatcher struct {
	store    domain.Store
	gateway  domain.ModelGateway
	tools    domain.ToolGateway
	opts     Options
	client   *policyClient
	wg       sync.WaitGroup
	stopped  atomic.Bool
	stopCh   chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

func New(store domain.Store, gateway domain.ModelGateway, tools domain.ToolGateway, opts Options) *Dispatcher {
	return &Dispatcher{
		store:   store,
		gateway: gateway,
		tools:   tools,
		opts:    opts,
		client:  newPolicyClient(opts.StepTimeout),
		stopCh:  make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func (d *Dispatcher) Stop(wait time.Duration) {
	d.stopOnce.Do(func() {
		d.stopped.Store(true)
		close(d.stopCh)
	})
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-d.done:
	case <-timer.C:
	}
}

func (d *Dispatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(d.opts.PollInterval)
	defer ticker.Stop()
	defer close(d.done)
	for {
		d.tick(ctx)
		select {
		case <-ctx.Done():
			d.wg.Wait()
			return
		case <-d.stopCh:
			d.wg.Wait()
			return
		case <-ticker.C:
		}
	}
}

func (d *Dispatcher) tick(ctx context.Context) {
	if d.stopped.Load() {
		return
	}
	if err := d.store.FailExpiredHumanWaits(ctx); err != nil {
		log.Printf("dispatcher: fail expired human waits: %v", err)
	}
	runs, err := d.store.ListRunnableRuns(ctx)
	if err != nil {
		log.Printf("dispatcher: list runnable runs: %v", err)
		return
	}
	for _, run := range runs {
		if ctx.Err() != nil || d.stopped.Load() {
			return
		}
		claimed, err := d.store.ClaimRun(ctx, run.ID)
		if err != nil {
			log.Printf("dispatcher: claim run %s: %v", run.ID, err)
			continue
		}
		if !claimed {
			continue
		}
		d.wg.Add(1)
		go func(run domain.Run) {
			defer d.wg.Done()
			d.drive(ctx, run)
		}(run)
	}
}

func (d *Dispatcher) drive(ctx context.Context, run domain.Run) {
	ctx = repository.WithTenant(ctx, run.TenantID)
	agent, err := d.store.GetAgent(ctx, run.AgentID)
	if err != nil {
		log.Printf("runtime: load agent for run %s: %v", run.ID, err)
		d.fail(ctx, run.ID, domain.ReasonPolicyUnreachable)
		return
	}
	steps, err := d.store.GetSteps(ctx, run.ID)
	if err != nil {
		log.Printf("runtime: load steps for run %s: %v", run.ID, err)
		d.fail(ctx, run.ID, domain.ReasonMaxRetries)
		return
	}
	attempt := run.Attempt
	stepIndex := len(steps)
	for {
		if ctx.Err() != nil || d.stopped.Load() {
			return
		}
		if d.checkCancel(ctx, run.ID) {
			return
		}
		started := time.Now()
		req := buildDecision(agent, run, stepIndex, attempt, steps)
		act, err := d.client.decide(ctx, agent, req)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			failure := asStepFailure(err)
			if !failure.retryable {
				d.fail(ctx, run.ID, failure.reason)
				return
			}
			attempt++
			if attempt >= d.opts.MaxStepAttempts {
				d.fail(ctx, run.ID, failure.reason)
				return
			}
			if !d.retryLater(ctx, run.ID, attempt) {
				return
			}
			continue
		}

		switch domain.StepType(act.Kind) {
		case domain.StepDone:
			finished := time.Now()
			result, err := json.Marshal(struct {
				Output json.RawMessage `json:"output"`
			}{Output: act.Output})
			if err != nil {
				d.fail(ctx, run.ID, domain.ReasonPolicyInvalidAction)
				return
			}
			runID := run.ID
			checkpoint := domain.Checkpoint{
				RunID:     run.ID,
				TenantID:  run.TenantID,
				StepIndex: stepIndex,
				StepType:  domain.StepDone,
				Result:    result,
				CreatedAt: finished,
			}
			span := domain.Span{
				ID:         ids.New(),
				TenantID:   run.TenantID,
				RunID:      &runID,
				StepIndex:  stepIndex,
				Attempt:    attempt,
				Name:       "step.done",
				StartedAt:  started,
				EndedAt:    finished,
				Attributes: json.RawMessage(`{}`),
			}
			if err := d.store.CompleteRunStep(ctx, checkpoint, span, act.Output); err != nil {
				if ctx.Err() != nil {
					return
				}
				attempt++
				if attempt >= d.opts.MaxStepAttempts {
					log.Printf("runtime: complete run %s: %v", run.ID, err)
					d.fail(ctx, run.ID, domain.ReasonMaxRetries)
					return
				}
				if !d.retryLater(ctx, run.ID, attempt) {
					return
				}
				continue
			}
			return
		case domain.StepLLMCall:
			runID := run.ID
			modelResult, callErr := d.gateway.Generate(ctx, domain.ModelRequest{Route: act.Route, Messages: act.Messages})
			if callErr != nil && errors.Is(callErr, domain.ErrModelRouteNotFound) {
				if ctx.Err() != nil {
					return
				}
				d.fail(ctx, run.ID, domain.ReasonPolicyInvalidAction)
				return
			}
			finished := time.Now()
			callLog := domain.ModelCallLog{
				TenantID:       run.TenantID,
				RunID:          &runID,
				RouteLabel:     modelResult.RouteLabel,
				RequestedModel: modelResult.RequestedModel,
				Chain:          modelResult.Chain,
				EffectiveModel: modelResult.Model,
				FallbackFrom:   modelResult.FallbackFrom,
				InputTokens:    modelResult.InputTokens,
				OutputTokens:   modelResult.OutputTokens,
				LatencyMS:      modelResult.LatencyMS,
				Status:         modelResult.Status,
			}
			spans := modelAttemptSpans(run, act.Route, stepIndex, attempt, modelResult.Attempts)
			if callErr != nil {
				if ctx.Err() != nil {
					return
				}
				if len(modelResult.Attempts) > 0 {
					if err := d.store.RecordModelCallAttempts(ctx, spans, callLog); err != nil {
						log.Printf("runtime: record model call attempts for run %s: %v", run.ID, err)
					}
				}
				attempt++
				if attempt >= d.opts.MaxStepAttempts {
					d.fail(ctx, run.ID, domain.ReasonMaxRetries)
					return
				}
				if !d.retryLater(ctx, run.ID, attempt) {
					return
				}
				continue
			}

			var fallbackFrom *string
			if modelResult.FallbackFrom != "" {
				value := modelResult.FallbackFrom
				fallbackFrom = &value
			}
			result, err := json.Marshal(llmCallResult{
				Route:          act.Route,
				RequestedModel: modelResult.RequestedModel,
				Model:          modelResult.Model,
				Chain:          modelResult.Chain,
				FallbackFrom:   fallbackFrom,
				Content:        modelResult.Content,
				Usage: struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				}{InputTokens: modelResult.InputTokens, OutputTokens: modelResult.OutputTokens},
			})
			if err != nil {
				d.fail(ctx, run.ID, domain.ReasonPolicyInvalidAction)
				return
			}
			checkpoint := domain.Checkpoint{
				RunID:     run.ID,
				TenantID:  run.TenantID,
				StepIndex: stepIndex,
				StepType:  domain.StepLLMCall,
				Result:    result,
				CreatedAt: finished,
			}
			if err := d.store.CompleteLLMCallStep(ctx, checkpoint, spans, callLog); err != nil {
				if ctx.Err() != nil {
					return
				}
				attempt++
				if attempt >= d.opts.MaxStepAttempts {
					log.Printf("runtime: complete llm_call run %s: %v", run.ID, err)
					d.fail(ctx, run.ID, domain.ReasonMaxRetries)
					return
				}
				if !d.retryLater(ctx, run.ID, attempt) {
					return
				}
				continue
			}
			steps = append(steps, checkpoint)
			stepIndex++
			attempt = 0
			continue
		case domain.StepToolCall:
			tool, err := d.tools.Resolve(ctx, act.Tool, act.Version)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				d.fail(ctx, run.ID, domain.ReasonToolSchemaViolation)
				return
			}
			if err := d.tools.Validate(tool, act.Args); err != nil {
				if ctx.Err() != nil {
					return
				}
				d.fail(ctx, run.ID, domain.ReasonToolSchemaViolation)
				return
			}
			runID := run.ID
			toolStart := time.Now()
			toolResult, callErr := d.tools.Invoke(ctx, tool, act.Args, fmt.Sprintf("%s:%d", run.ID, stepIndex))
			finished := time.Now()
			if callErr != nil {
				if ctx.Err() != nil {
					return
				}
				attempt++
				if attempt >= d.opts.MaxStepAttempts {
					d.fail(ctx, run.ID, domain.ReasonMaxRetries)
					return
				}
				if !d.retryLater(ctx, run.ID, attempt) {
					return
				}
				continue
			}

			result, err := json.Marshal(toolCallResult{
				Tool:    tool.ToolName,
				Version: tool.Version,
				Args:    act.Args,
				Result:  toolResult,
			})
			if err != nil {
				d.fail(ctx, run.ID, domain.ReasonPolicyInvalidAction)
				return
			}
			attributes, _ := json.Marshal(map[string]any{
				"tool":    tool.ToolName,
				"version": tool.Version,
				"attempt": attempt,
			})
			checkpoint := domain.Checkpoint{
				RunID:     run.ID,
				TenantID:  run.TenantID,
				StepIndex: stepIndex,
				StepType:  domain.StepToolCall,
				Result:    result,
				CreatedAt: finished,
			}
			span := domain.Span{
				ID:         ids.New(),
				TenantID:   run.TenantID,
				RunID:      &runID,
				StepIndex:  stepIndex,
				Attempt:    attempt,
				Name:       "tool." + tool.ToolName,
				StartedAt:  toolStart,
				EndedAt:    finished,
				Attributes: attributes,
			}
			invocation := domain.ToolInvocation{
				ID:        ids.New(),
				TenantID:  run.TenantID,
				RunID:     run.ID,
				ToolName:  tool.ToolName,
				Version:   tool.Version,
				Args:      act.Args,
				Result:    toolResult,
				Status:    "ok",
				CreatedAt: finished,
			}
			if err := d.store.CompleteToolCallStep(ctx, checkpoint, span, invocation); err != nil {
				if ctx.Err() != nil {
					return
				}
				attempt++
				if attempt >= d.opts.MaxStepAttempts {
					log.Printf("runtime: complete tool_call run %s: %v", run.ID, err)
					d.fail(ctx, run.ID, domain.ReasonMaxRetries)
					return
				}
				if !d.retryLater(ctx, run.ID, attempt) {
					return
				}
				continue
			}
			steps = append(steps, checkpoint)
			stepIndex++
			attempt = 0
			continue
		case domain.StepHumanReview:
			pendingStep, err := json.Marshal(humanReviewStep{
				Action:          string(domain.StepHumanReview),
				Prompt:          act.Prompt,
				DeadlineSeconds: act.DeadlineSeconds,
			})
			if err != nil {
				d.fail(ctx, run.ID, domain.ReasonPolicyInvalidAction)
				return
			}
			deadline := time.Now().Add(d.opts.StepTimeout)
			if act.DeadlineSeconds != nil {
				deadline = time.Now().Add(time.Duration(*act.DeadlineSeconds * float64(time.Second)))
			}
			if err := d.store.EnterHumanReview(ctx, run.ID, attempt, pendingStep, act.Prompt, deadline); err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("runtime: enter human review run %s: %v", run.ID, err)
				d.fail(ctx, run.ID, domain.ReasonMaxRetries)
			}
			return
		default:
			d.fail(ctx, run.ID, domain.ReasonPolicyInvalidAction)
			return
		}
	}
}

func modelAttemptSpans(run domain.Run, requested string, stepIndex, attempt int, attempts []domain.ModelAttempt) []domain.Span {
	runID := run.ID
	spans := make([]domain.Span, 0, len(attempts))
	for _, result := range attempts {
		attributes, _ := json.Marshal(map[string]any{
			"requested_model": requested,
			"effective_model": result.Model,
			"status":          result.Status,
			"latency_ms":      result.LatencyMS,
			"attempt":         result.Attempt,
		})
		spans = append(spans, domain.Span{
			ID:         ids.New(),
			TenantID:   run.TenantID,
			RunID:      &runID,
			StepIndex:  stepIndex,
			Attempt:    attempt,
			Name:       "model." + result.Model,
			StartedAt:  result.StartedAt,
			EndedAt:    result.EndedAt,
			Attributes: attributes,
			Error:      result.Error,
		})
	}
	return spans
}

type humanReviewStep struct {
	Action          string   `json:"action"`
	Prompt          string   `json:"prompt"`
	DeadlineSeconds *float64 `json:"deadline_seconds,omitempty"`
}

type toolCallResult struct {
	Tool    string          `json:"tool"`
	Version string          `json:"version"`
	Args    json.RawMessage `json:"args"`
	Result  json.RawMessage `json:"result"`
}

type llmCallResult struct {
	Route          string   `json:"route"`
	RequestedModel string   `json:"requested_model"`
	Model          string   `json:"model"`
	Chain          []string `json:"chain"`
	FallbackFrom   *string  `json:"fallback_from"`
	Content        string   `json:"content"`
	Usage          struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (d *Dispatcher) retryLater(ctx context.Context, runID string, attempt int) bool {
	next := time.Now().Add(backoff(attempt))
	if err := d.store.RescheduleStep(ctx, runID, attempt, next); err != nil {
		log.Printf("runtime: reschedule run %s: %v", runID, err)
		d.fail(ctx, runID, domain.ReasonMaxRetries)
		return false
	}
	return d.waitUntil(ctx, next)
}

func (d *Dispatcher) waitUntil(ctx context.Context, at time.Time) bool {
	wait := time.Until(at)
	if wait <= 0 {
		return ctx.Err() == nil && !d.stopped.Load()
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-d.stopCh:
		return false
	case <-timer.C:
		return true
	}
}

func (d *Dispatcher) checkCancel(ctx context.Context, runID string) bool {
	requested, err := d.store.CancelRequested(ctx, runID)
	if err != nil {
		log.Printf("runtime: check cancel for run %s: %v", runID, err)
		return false
	}
	if !requested {
		return false
	}
	if err := d.store.MarkCancelled(ctx, runID); err != nil {
		log.Printf("runtime: mark run %s cancelled: %v", runID, err)
	}
	return true
}

func (d *Dispatcher) fail(ctx context.Context, runID string, reason domain.TerminalReason) {
	if err := d.store.MarkRunFailed(ctx, runID, reason); err != nil {
		log.Printf("runtime: mark run %s failed: %v", runID, err)
	}
}

func buildDecision(agent domain.Agent, run domain.Run, stepIndex, attempt int, steps []domain.Checkpoint) decisionRequest {
	recent := make([]ledgerStep, 0, len(steps))
	for _, checkpoint := range steps {
		recent = append(recent, ledgerStep{
			StepIndex: checkpoint.StepIndex,
			StepType:  string(checkpoint.StepType),
			Result:    checkpoint.Result,
		})
	}
	input := run.Input
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	return decisionRequest{
		RunID:     run.ID,
		Agent:     agent.Name,
		StepIndex: stepIndex,
		Attempt:   attempt,
		Input:     input,
		Ledger:    ledger{Recent: recent, Summary: "", Dropped: []int{}},
	}
}

func asStepFailure(err error) *stepFailure {
	var failure *stepFailure
	if errors.As(err, &failure) {
		return failure
	}
	return &stepFailure{reason: domain.ReasonPolicyUnreachable, retryable: true, err: err}
}

func backoff(attempt int) time.Duration {
	duration := time.Second << (attempt - 1)
	if duration > 30*time.Second {
		return 30 * time.Second
	}
	return duration
}
