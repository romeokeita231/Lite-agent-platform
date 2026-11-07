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

	"github.com/romeokeita231/Lite-agent-platform/internal/contextwin"
	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/ids"
	"github.com/romeokeita231/Lite-agent-platform/internal/observability"
)

type Options struct {
	StepTimeout       time.Duration
	MaxStepAttempts   int
	PollInterval      time.Duration
	TenantConcurrency int
	TenantRate        float64
	ContextRecent     int
	ContextMaxKeep    int
	Metrics           observability.Recorder
}

type tenantLimit struct {
	inflight int
	tokens   float64
	last     time.Time
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
	limitMu  sync.Mutex
	limits   map[string]*tenantLimit
}

func New(store domain.Store, gateway domain.ModelGateway, tools domain.ToolGateway, opts Options) *Dispatcher {
	if opts.TenantConcurrency <= 0 {
		opts.TenantConcurrency = 4
	}
	if opts.TenantRate < 0 {
		opts.TenantRate = 0
	}
	if opts.ContextRecent <= 0 {
		opts.ContextRecent = 8
	}
	if opts.ContextMaxKeep <= 0 {
		opts.ContextMaxKeep = 16
	}
	return &Dispatcher{
		store:   store,
		gateway: gateway,
		tools:   tools,
		opts:    opts,
		client:  newPolicyClient(opts.StepTimeout),
		stopCh:  make(chan struct{}),
		done:    make(chan struct{}),
		limits:  make(map[string]*tenantLimit),
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
	d.refreshMetrics(ctx)
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
		if !d.acquire(run.TenantID) {
			continue
		}
		claimed, err := d.store.ClaimRun(domain.WithTenant(ctx, run.TenantID), run.ID)
		if err != nil {
			log.Printf("dispatcher: claim run %s: %v", run.ID, err)
			d.release(run.TenantID)
			continue
		}
		if !claimed {
			d.release(run.TenantID)
			continue
		}
		d.wg.Add(1)
		go func(run domain.Run) {
			defer d.wg.Done()
			d.drive(ctx, run)
		}(run)
	}
}

var runStatuses = []domain.RunStatus{
	domain.RunQueued,
	domain.RunRunning,
	domain.RunWaitingHuman,
	domain.RunCompleted,
	domain.RunFailed,
	domain.RunCancelled,
}

func (d *Dispatcher) refreshMetrics(ctx context.Context) {
	if d.opts.Metrics == nil {
		return
	}
	counts, err := d.store.CountRunsByStatus(ctx)
	if err != nil {
		log.Printf("dispatcher: count runs by status: %v", err)
	} else {
		for _, status := range runStatuses {
			d.opts.Metrics.SetRunStatus(string(status), int(counts[status]))
		}
	}
	d.limitMu.Lock()
	inflight := make([]int, 0, len(d.limits))
	for _, limit := range d.limits {
		inflight = append(inflight, limit.inflight)
	}
	d.limitMu.Unlock()
	for _, count := range inflight {
		d.opts.Metrics.ObserveTenantInflight(count)
	}
}

func (d *Dispatcher) acquire(tenantID string) bool {
	d.limitMu.Lock()
	defer d.limitMu.Unlock()
	limit := d.limits[tenantID]
	if limit == nil {
		limit = &tenantLimit{tokens: 1, last: time.Now()}
		d.limits[tenantID] = limit
	}
	if limit.inflight >= d.opts.TenantConcurrency {
		return false
	}
	if d.opts.TenantRate > 0 {
		now := time.Now()
		limit.tokens += now.Sub(limit.last).Seconds() * d.opts.TenantRate
		if limit.tokens > 1 {
			limit.tokens = 1
		}
		limit.last = now
		if limit.tokens < 1 {
			return false
		}
		limit.tokens--
	}
	limit.inflight++
	return true
}

func (d *Dispatcher) release(tenantID string) {
	d.limitMu.Lock()
	defer d.limitMu.Unlock()
	if limit := d.limits[tenantID]; limit != nil && limit.inflight > 0 {
		limit.inflight--
	}
}

func (d *Dispatcher) drive(ctx context.Context, run domain.Run) {
	ctx = domain.WithTenant(ctx, run.TenantID)
	held := true
	release := func() {
		if held {
			d.release(run.TenantID)
			held = false
		}
	}
	defer release()
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
	reacquire := func() bool {
		if !d.acquireBlocking(ctx, run.TenantID) {
			return false
		}
		held = true
		return true
	}
	retry := func(attempt int) bool {
		if d.opts.Metrics != nil {
			d.opts.Metrics.IncStepRetry()
		}
		next := time.Now().Add(backoff(attempt))
		if err := d.store.RescheduleStep(ctx, run.ID, attempt, next); err != nil {
			log.Printf("runtime: reschedule run %s: %v", run.ID, err)
			d.fail(ctx, run.ID, domain.ReasonMaxRetries)
			return false
		}
		release()
		if !d.waitUntil(ctx, next) {
			return false
		}
		return reacquire()
	}
	retryOrFail := func(runID string, attempt *int, reason domain.TerminalReason, cause error) bool {
		*attempt++
		if *attempt >= d.opts.MaxStepAttempts {
			if cause != nil {
				log.Printf("runtime: run %s: %v", runID, cause)
			}
			d.fail(ctx, runID, reason)
			return false
		}
		return retry(*attempt)
	}
	for {
		if ctx.Err() != nil || d.stopped.Load() {
			return
		}
		if d.checkCancel(ctx, run.ID) {
			return
		}
		started := time.Now()
		view := buildView(steps, d.opts)
		if d.opts.Metrics != nil {
			d.opts.Metrics.ObserveContext(view.Tokens, view.DroppedSteps)
		}
		req := buildDecision(agent, run, stepIndex, attempt, view)
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
			if !retryOrFail(run.ID, &attempt, failure.reason, nil) {
				return
			}
			continue
		}

		switch domain.StepType(act.Kind) {
		case domain.StepDone:
			finished := time.Now()
			result, err := json.Marshal(doneResult{
				Output: act.Output,
				Keep:   act.Keep,
			})
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
				Attributes: contextAttributes(view),
			}
			if err := d.store.CompleteRunStep(ctx, checkpoint, span, act.Output); err != nil {
				if ctx.Err() != nil {
					return
				}
				if !retryOrFail(run.ID, &attempt, domain.ReasonMaxRetries, err) {
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
			spans := modelAttemptSpans(run, act.Route, stepIndex, attempt, modelResult.Attempts, view)
			if callErr != nil {
				if ctx.Err() != nil {
					return
				}
				if len(modelResult.Attempts) > 0 {
					if err := d.store.RecordModelCallAttempts(ctx, spans, callLog); err != nil {
						log.Printf("runtime: record model call attempts for run %s: %v", run.ID, err)
					}
				}
				reason := domain.ReasonMaxRetries
				if errors.Is(callErr, context.DeadlineExceeded) {
					reason = domain.ReasonStepTimeout
				}
				if !retryOrFail(run.ID, &attempt, reason, nil) {
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
				Keep: act.Keep,
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
				if !retryOrFail(run.ID, &attempt, domain.ReasonMaxRetries, err) {
					return
				}
				continue
			}
			steps = append(steps, checkpoint)
			stepIndex++
			attempt = 0
			release()
			if !reacquire() {
				return
			}
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
			idempotencyKey := fmt.Sprintf("%s:%d", run.ID, stepIndex)
			toolResult, callErr := d.tools.Invoke(ctx, tool, act.Args, idempotencyKey)
			finished := time.Now()
			if callErr != nil {
				if ctx.Err() != nil {
					return
				}
				reason := domain.ReasonMaxRetries
				if errors.Is(callErr, context.DeadlineExceeded) {
					reason = domain.ReasonStepTimeout
				}
				if !retryOrFail(run.ID, &attempt, reason, nil) {
					return
				}
				continue
			}

			result, err := json.Marshal(toolCallResult{
				Tool:    tool.ToolName,
				Version: tool.Version,
				Args:    act.Args,
				Result:  toolResult,
				Keep:    act.Keep,
			})
			if err != nil {
				d.fail(ctx, run.ID, domain.ReasonPolicyInvalidAction)
				return
			}
			attributes, _ := json.Marshal(map[string]any{
				"tool":                  tool.ToolName,
				"version":               tool.Version,
				"attempt":               attempt,
				"context_tokens":        view.Tokens,
				"context_dropped_steps": view.DroppedSteps,
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
				ID:             ids.New(),
				TenantID:       run.TenantID,
				RunID:          run.ID,
				ToolName:       tool.ToolName,
				Version:        tool.Version,
				Args:           act.Args,
				Result:         toolResult,
				Status:         domain.ToolStatusOK,
				CreatedAt:      finished,
				StepIndex:      stepIndex,
				IdempotencyKey: idempotencyKey,
				LatencyMS:      finished.Sub(toolStart).Milliseconds(),
			}
			if err := d.store.CompleteToolCallStep(ctx, checkpoint, span, invocation); err != nil {
				if ctx.Err() != nil {
					return
				}
				if !retryOrFail(run.ID, &attempt, domain.ReasonMaxRetries, err) {
					return
				}
				continue
			}
			steps = append(steps, checkpoint)
			stepIndex++
			attempt = 0
			release()
			if !reacquire() {
				return
			}
			continue
		case domain.StepHumanReview:
			pendingStep, err := json.Marshal(humanReviewStep{
				Action:              string(domain.StepHumanReview),
				Prompt:              act.Prompt,
				DeadlineSeconds:     act.DeadlineSeconds,
				Keep:                act.Keep,
				ContextTokens:       view.Tokens,
				ContextDroppedSteps: view.DroppedSteps,
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

func modelAttemptSpans(run domain.Run, requested string, stepIndex, attempt int, attempts []domain.ModelAttempt, view contextwin.View) []domain.Span {
	runID := run.ID
	spans := make([]domain.Span, 0, len(attempts))
	for _, result := range attempts {
		attributes, _ := json.Marshal(map[string]any{
			"requested_model":       requested,
			"effective_model":       result.Model,
			"status":                result.Status,
			"latency_ms":            result.LatencyMS,
			"attempt":               result.Attempt,
			"context_tokens":        view.Tokens,
			"context_dropped_steps": view.DroppedSteps,
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
	Action              string   `json:"action"`
	Prompt              string   `json:"prompt"`
	DeadlineSeconds     *float64 `json:"deadline_seconds,omitempty"`
	Keep                []int    `json:"keep,omitempty"`
	ContextTokens       int      `json:"context_tokens"`
	ContextDroppedSteps int      `json:"context_dropped_steps"`
}

type doneResult struct {
	Output json.RawMessage `json:"output"`
	Keep   []int           `json:"keep,omitempty"`
}

type toolCallResult struct {
	Tool    string          `json:"tool"`
	Version string          `json:"version"`
	Args    json.RawMessage `json:"args"`
	Result  json.RawMessage `json:"result"`
	Keep    []int           `json:"keep,omitempty"`
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
	Keep []int `json:"keep,omitempty"`
}

func (d *Dispatcher) acquireBlocking(ctx context.Context, tenantID string) bool {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil || d.stopped.Load() {
			return false
		}
		if d.acquire(tenantID) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-d.stopCh:
			return false
		case <-ticker.C:
		}
	}
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

func buildView(steps []domain.Checkpoint, opts Options) contextwin.View {
	ledgerSteps := make([]contextwin.Step, 0, len(steps))
	for _, checkpoint := range steps {
		ledgerSteps = append(ledgerSteps, contextwin.Step{
			StepIndex: checkpoint.StepIndex,
			StepType:  string(checkpoint.StepType),
			Result:    checkpoint.Result,
		})
	}
	var keep []int
	if len(steps) > 0 {
		keep = contextwin.KeepOf(steps[len(steps)-1].Result)
	}
	return contextwin.Build(ledgerSteps, keep, contextwin.Config{
		Recent:  opts.ContextRecent,
		MaxKeep: opts.ContextMaxKeep,
	})
}

func contextAttributes(view contextwin.View) json.RawMessage {
	attributes, _ := json.Marshal(map[string]any{
		"context_tokens":        view.Tokens,
		"context_dropped_steps": view.DroppedSteps,
	})
	return attributes
}

func buildDecision(agent domain.Agent, run domain.Run, stepIndex, attempt int, view contextwin.View) decisionRequest {
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
		Ledger:    ledger{Recent: view.Recent, Summary: view.Summary, Dropped: view.Dropped},
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
