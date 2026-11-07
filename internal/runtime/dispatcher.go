package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
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
	store  domain.Store
	opts   Options
	client *policyClient
	wg     sync.WaitGroup
}

func New(store domain.Store, opts Options) *Dispatcher {
	return &Dispatcher{store: store, opts: opts, client: newPolicyClient(opts.StepTimeout)}
}

func (d *Dispatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(d.opts.PollInterval)
	defer ticker.Stop()
	for {
		d.tick(ctx)
		select {
		case <-ctx.Done():
			d.wg.Wait()
			return
		case <-ticker.C:
		}
	}
}

func (d *Dispatcher) tick(ctx context.Context) {
	runs, err := d.store.ListRunnableRuns(ctx)
	if err != nil {
		log.Printf("dispatcher: list runnable runs: %v", err)
		return
	}
	for _, run := range runs {
		if ctx.Err() != nil {
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
		if ctx.Err() != nil {
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
	}
}

func (d *Dispatcher) retryLater(ctx context.Context, runID string, attempt int) bool {
	next := time.Now().Add(backoff(attempt))
	if err := d.store.RescheduleStep(ctx, runID, attempt, next); err != nil {
		log.Printf("runtime: reschedule run %s: %v", runID, err)
		d.fail(ctx, runID, domain.ReasonMaxRetries)
		return false
	}
	return waitUntil(ctx, next)
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

func waitUntil(ctx context.Context, at time.Time) bool {
	wait := time.Until(at)
	if wait <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
