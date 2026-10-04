// Package wakepoller is Checkpoint 8N.1's daemon-level poller: the piece
// that actually calls wake.Scheduler.ClaimDue and turns a claimed wake into
// a real workflow.Coordinator.ContinueRun call. wake.Scheduler deliberately
// knows nothing about the Coordinator (see that package's own doc comment);
// this package is exactly the thing that does, mirroring how
// observe/reaper knows about the Lifecycle Manager while the reaper's own
// probing stays LCM-agnostic.
//
// Poller follows the same Config{Tick, Clock, Logger} + New + Start(ctx)
// <-chan struct{} + exported synchronous-cycle shape as observe/reaper.Reaper
// and cdc.Poller: RunDueOnce is both the production ticker's per-cycle body
// and the primitive tests call directly after advancing a fake clock, so
// both paths run the exact same code (checkpoint item 23: the ticker is only
// a discovery heartbeat, never the retry authority — scheduled_at on each
// wake row is).
package wakepoller

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/workflow"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
)

// DefaultTickInterval matches the checkpoint's "20s wake-discovery heartbeat"
// baseline.
const DefaultTickInterval = 20 * time.Second

// DefaultClaimLimit bounds how many due wakes a single cycle claims, so one
// slow ContinueRun call can't starve every other due run in the same tick.
const DefaultClaimLimit = 10

// claimant identifies this poller instance in claimed_by, purely for
// operational debugging (which process/replica fired a given wake).
const claimant = "wakepoller"

// Scheduler is the narrow wake.Scheduler surface Poller needs.
type Scheduler interface {
	ClaimDue(ctx context.Context, claimant string, limit int) ([]wake.Schedule, error)
	Complete(ctx context.Context, id string) error
	Fail(ctx context.Context, sch wake.Schedule, errMsg string) (wake.FailResult, error)
}

// Resumer is the narrow workflow.Coordinator surface Poller needs: resuming
// a run is idempotent by construction (workflow.go's own doc comment on
// ContinueRun), so RunDueOnce never needs to know which specific reason a
// wake was scheduled for — it just asks the coordinator to re-evaluate.
type Resumer interface {
	ContinueRun(ctx context.Context, runID string) (workflow.RunDetail, error)
	// MarkCapacityRetryExhausted is called when a wake's retry budget runs
	// out with capacity still unavailable (checkpoint item 26): the run
	// moves to an explicit, observable state instead of staying silently
	// parked in Waiting with no further wake ever scheduled.
	MarkCapacityRetryExhausted(ctx context.Context, runID string, reason string) error
}

// RecoveryDispatcher is the P3-C surface a wake scheduled for
// wake.ReasonAutoRecovery is routed to instead of ContinueRun.
//
// The distinction matters and is the reason this is a separate interface
// rather than another Resumer method: an automatic recovery is NOT a resume.
// The run is parked on a condition a resume cannot clear -- that is why it is
// parked -- so calling ContinueRun for it would do nothing, report nothing, and
// leave the wake looking like it fired successfully. Routing by reason lets the
// poller drive the remedy the Advisor actually selected, while every other wake
// keeps the reason-agnostic resume path unchanged.
//
// Optional: a Resumer that does not implement it simply never gets an
// auto-recovery wake routed differently, which degrades to the pre-P3-C
// behaviour rather than to an error.
type RecoveryDispatcher interface {
	DispatchAutomaticRecovery(ctx context.Context, runID string) (workflow.AutomaticRecoveryOutcome, error)
}

// Config holds the externally-tunable knobs for a Poller. Every field is
// optional; zero values fall back to safe defaults, mirroring
// observe/reaper.Config's own convention.
type Config struct {
	// Tick is the interval between production poll cycles. <=0 means
	// DefaultTickInterval.
	Tick time.Duration
	// Clock is unused directly by Poller today (wake.Scheduler owns its own
	// clock for scheduled_at comparisons) but is accepted for symmetry with
	// every other poller in this codebase and reserved for future
	// telemetry timestamps. nil means time.Now.
	Clock func() time.Time
	// ClaimLimit bounds wakes claimed per cycle. <=0 means DefaultClaimLimit.
	ClaimLimit int
	// Logger receives operational diagnostics. nil means slog.Default.
	Logger *slog.Logger
}

// Poller is Checkpoint 8N.1's daemon central poller.
type Poller struct {
	scheduler  Scheduler
	resumer    Resumer
	tick       time.Duration
	clock      func() time.Time
	claimLimit int
	logger     *slog.Logger
}

// New constructs a Poller. scheduler claims/completes/fails wakes; resumer is
// the Coordinator that actually resumes a run once its wake is due.
func New(scheduler Scheduler, resumer Resumer, cfg Config) *Poller {
	p := &Poller{
		scheduler:  scheduler,
		resumer:    resumer,
		tick:       cfg.Tick,
		clock:      cfg.Clock,
		claimLimit: cfg.ClaimLimit,
		logger:     cfg.Logger,
	}
	if p.tick <= 0 {
		p.tick = DefaultTickInterval
	}
	if p.clock == nil {
		p.clock = time.Now
	}
	if p.claimLimit <= 0 {
		p.claimLimit = DefaultClaimLimit
	}
	if p.logger == nil {
		p.logger = slog.Default()
	}
	return p
}

// Start launches the background goroutine and returns a channel that closes
// once the loop has exited. The loop exits on ctx cancellation; the channel
// gives the daemon a clean shutdown hook (wait on it after cancel to confirm
// the poller has stopped before tearing down dependencies) — same shape as
// observe/reaper.Reaper.Start.
func (p *Poller) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go p.loop(ctx, done)
	return done
}

func (p *Poller) loop(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(p.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := p.RunDueOnce(ctx); err != nil {
				p.logger.Error("wakepoller: cycle failed", "err", err)
			}
		}
	}
}

// RunDueOnce claims every currently-due wake (bounded by ClaimLimit) and
// fires each one exactly once via Resumer.ContinueRun, then closes out the
// wake according to what happened. It returns the number of wakes claimed
// this cycle.
//
// Completion semantics, and why a single unconditional Complete call after a
// nil/not-found/terminal ContinueRun result is safe rather than a source of
// silently lost wakes (the exact risk the checkpoint brief calls out):
//
//   - ContinueRun succeeds and the run did NOT re-park on the same scope:
//     the claimed wake row is still status=claimed, so Complete's
//     `WHERE id = ? AND status = 'claimed'` CAS succeeds and closes it out.
//   - ContinueRun succeeds but the run re-parks on the exact same
//     (run, step, reason) scope: before returning, that dispatch path
//     already called wake.Scheduler.Schedule again via the same idempotency
//     key, which finds this exact row (still claimed at that point) and
//     flips it back to pending with a freshly-computed backoff — all inside
//     UpsertWorkflowWakeSchedule, which runs before this function's own
//     Complete call. By the time Complete(id) runs here, the row is no
//     longer claimed, so the CAS affects 0 rows and is a no-op: the fresh
//     pending reschedule is left untouched, never lost.
//   - ContinueRun returns ErrNotFound or ErrAlreadyTerminal: nothing to
//     resume, ever again, for this run — Complete closes the wake out (a
//     terminal/missing run can't legally re-park, so no supersession race
//     is possible here).
//   - ContinueRun returns workflow.ErrUnrecoverable: the failure is
//     deterministic -- AO asked, got an answer it refuses to act on, and no
//     amount of retrying changes it (see that sentinel). Rescheduling would
//     re-drive the run into the identical error on every tick, forever, which
//     is exactly the overnight spin this branch exists to stop. The wake is
//     COMPLETED, not failed: the run is already parked in needs_attention by
//     whoever raised the condition, and a person continuing the run schedules a
//     fresh wake.
//   - ContinueRun returns any other (transient) error: Fail reschedules the
//     wake with the scheduler's own backoff/budget bookkeeping, exactly as
//     it does for any other firing failure.
func (p *Poller) RunDueOnce(ctx context.Context) (int, error) {
	claimed, err := p.scheduler.ClaimDue(ctx, claimant, p.claimLimit)
	if err != nil {
		return 0, err
	}
	for _, sch := range claimed {
		resumeErr := p.fire(ctx, sch)
		switch {
		case resumeErr == nil, errors.Is(resumeErr, workflow.ErrNotFound), errors.Is(resumeErr, workflow.ErrAlreadyTerminal),
			errors.Is(resumeErr, workflow.ErrUnrecoverable):
			if cerr := p.scheduler.Complete(ctx, sch.ID); cerr != nil {
				p.logger.Warn("wakepoller: complete wake failed", "wake", sch.ID, "run", sch.WorkflowRunID, "err", cerr)
				continue
			}
			if errors.Is(resumeErr, workflow.ErrUnrecoverable) {
				// Said plainly, once: this run is not coming back on a timer.
				p.logger.Warn("wakepoller: deterministic failure, wake closed without rescheduling",
					"wake", sch.ID, "run", sch.WorkflowRunID, "reason", sch.Reason, "err", resumeErr)
				continue
			}
			p.logger.Info("wakepoller: wake completed", "wake", sch.ID, "run", sch.WorkflowRunID, "reason", sch.Reason, "resumeErr", resumeErr)
		default:
			res, ferr := p.scheduler.Fail(ctx, sch, resumeErr.Error())
			if ferr != nil {
				p.logger.Warn("wakepoller: fail wake bookkeeping failed", "wake", sch.ID, "run", sch.WorkflowRunID, "err", ferr)
				continue
			}
			if res.BudgetExhausted {
				p.logger.Warn("wakepoller: wake budget exhausted, giving up", "wake", sch.ID, "run", sch.WorkflowRunID, "reason", sch.Reason, "attempts", res.AttemptCount)
				// P3-C: an auto-recovery wake that ran out of retries is NOT a
				// capacity exhaustion, and parking the run under that name
				// would tell a person their provider is at capacity when what
				// actually happened is that AO's own recovery dispatcher kept
				// erroring. The run is already parked on its real stop, which
				// still explains it and still names a remedy; the wake simply
				// stops, and the log line above is what says so.
				if sch.Reason != wake.ReasonAutoRecovery {
					if merr := p.resumer.MarkCapacityRetryExhausted(ctx, string(sch.WorkflowRunID), string(sch.Reason)); merr != nil {
						p.logger.Warn("wakepoller: mark capacity retry exhausted failed", "wake", sch.ID, "run", sch.WorkflowRunID, "err", merr)
					}
				}
			} else {
				p.logger.Info("wakepoller: wake rescheduled after transient error", "wake", sch.ID, "run", sch.WorkflowRunID, "reason", sch.Reason, "nextScheduledAt", res.NextScheduledAt, "err", resumeErr)
			}
		}
	}
	return len(claimed), nil
}

// fire performs the one action a claimed wake calls for.
//
// Every wake but one is reason-agnostic and goes to ContinueRun, exactly as it
// did before P3-C: the run's own evidence gates decide what, if anything, a
// resume discharges. wake.ReasonAutoRecovery is the exception, because the run
// it fires for is parked on a condition a resume provably cannot clear -- see
// RecoveryDispatcher.
//
// The dispatcher's own refusals are NOT errors here. "The budget is spent",
// "another actor took this repair", "the condition is no longer repairable" are
// all outcomes it reports in its result rather than raising, so a wake that
// finds nothing left to do completes cleanly instead of being rescheduled into
// the same answer forever.
func (p *Poller) fire(ctx context.Context, sch wake.Schedule) error {
	if sch.Reason == wake.ReasonAutoRecovery {
		dispatcher, ok := p.resumer.(RecoveryDispatcher)
		if !ok {
			// No dispatcher wired. Fall through to the ordinary resume rather
			// than failing the wake: the pre-P3-C behaviour is a no-op, not a
			// broken one.
			_, err := p.resumer.ContinueRun(ctx, string(sch.WorkflowRunID))
			return err
		}
		out, err := dispatcher.DispatchAutomaticRecovery(ctx, string(sch.WorkflowRunID))
		if err != nil {
			return err
		}
		p.logger.Info("wakepoller: automatic recovery evaluated",
			"wake", sch.ID, "run", sch.WorkflowRunID, "action", out.Action,
			"dispatched", out.Dispatched, "detail", out.Detail)
		return nil
	}
	_, err := p.resumer.ContinueRun(ctx, string(sch.WorkflowRunID))
	return err
}
