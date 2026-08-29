package workflow

import (
	stdctx "context"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Sessions resolves a referenced session for the recovery integrity check.
// Optional: a nil Sessions dependency simply skips the check.
type Sessions interface {
	GetSession(ctx stdctx.Context, id domain.SessionID) (domain.SessionRecord, bool, error)
}

// ReviewRuns is declared in review_dispatch.go (Checkpoint 8C widened it
// beyond this file's original read-only recovery-integrity-check need to
// also cover review creation/dedupe; checkStepIntegrity below only ever
// calls its GetReviewRun method). Optional: a nil ReviewRuns dependency
// simply skips both the integrity check here and review dispatch.

// Reconcile is workflow's own durable self-state repair, run once at daemon
// boot after storage opens and before the daemon starts serving. It never
// calls Review Engine or spawns a reviewer — but for the work step kind,
// Checkpoint 8B does let it drive the SAME idempotent dispatch/observation
// machinery StartRun and GetRun use, because that is precisely what proves a
// crash mid-dispatch (recovery boundaries A/B/ambiguous) heals correctly:
// re-entering dispatchWorkStep after a restart is safe by construction (see
// dispatch.go), never calls Spawner.Spawn a second time for an
// already-attempted step, and never assumes success it cannot prove.
//
// Rule: every non-terminal run is loaded with its steps. For any step kind
// OTHER than work, 8A's original generic rule still applies unmodified: a
// step found running is moved to waiting (no independent fact source exists
// for those kinds in this checkpoint, since nothing dispatches them yet), and
// if the parent run was running/waiting it moves to needs_attention.
//
// For a work step found ready or running, Checkpoint 8B replaces that blind
// rule with: (1) dispatchWorkStep, which is a no-op if the step already has a
// session, otherwise resumes exactly where the outbox left off (pending ->
// call Spawn once; dispatched -> adopt via natural key or surface ambiguity;
// never re-dispatch a step that never got past pending==the run itself never
// started, since a work step stays "pending" — not ready/running — until
// StartRun unblocks it); then (2), if the step is now running with a session,
// observeWorkStep evaluates its live progress. A still-alive, still-active
// worker session is NOT necessarily "interrupted" by the daemon restart
// (session_manager.reconcileLive already adopts a still-alive tmux process as
// a no-op), so a work step may correctly stay running after Reconcile — only
// real evidence (terminated session, idle-with-no-change, etc.) moves it.
//
// This remains idempotent: a step already waiting/completed/failed is left
// alone by its respective rule, and a run already needs_attention is left
// alone, so running Reconcile twice in a row is a no-op the second time.
func (c *Coordinator) Reconcile(ctx stdctx.Context) error {
	// Before any run is advanced: match every AO-owned worktree record against
	// what is actually in the repository, and finish whatever a restart cut in
	// half -- a worktree whose directory appeared but whose state write did
	// not, a registration that outlived its directory, or an integration whose
	// cleanup never ran. Everything below reads worktrees and branches, and it
	// has to read them in the state the durable records describe.
	//
	// It is the same ordering reason branch locks are reconciled before this
	// function is called at all (see internal/daemon).
	c.reconcileTaskWorktrees(ctx)

	// External-session obligations first, and for EVERY run — including terminal
	// ones, which every other path below deliberately skips.
	//
	// A terminal run is the end of AO's interest in a review, not the end of its
	// responsibility for a process it started. A cancellation that won the race
	// against an in-flight launch leaves a reviewer running with nothing that
	// would ever look for it again, because "the run is over" is exactly the
	// condition under which recovery stops looking. See
	// ReconcileOrphanedReviewers.
	c.reconcileOrphanedReviewersForAllRuns(ctx)

	runs, err := c.store.ListNonTerminalWorkflowRuns(ctx)
	if err != nil {
		return err
	}
	now := c.clock()
	// PER-RUN ISOLATION.
	//
	// Every failure below used to abort the whole pass: the first run whose
	// runtime AO could not read stopped reconciliation for every OTHER run on
	// the machine, and boot logged one error and moved on with the rest of the
	// fleet unrecovered. That is how a single stale tmux pane held an entire
	// overnight queue still.
	//
	// A failure is now scoped to the run that produced it. The run is parked in
	// needs_attention with a named reason so it stops being re-driven blindly,
	// the loop continues, and the collected errors are still returned so boot
	// reports what it could not repair. Nothing here weakens a decision: an
	// error is still never read as success.
	var failures []error
	for _, run := range runs {
		if rerr := c.reconcileRun(ctx, run, now); rerr != nil {
			failures = append(failures, fmt.Errorf("workflow run %s: %w", run.ID, rerr))
			if c.log != nil {
				c.log.Error("workflow: boot reconciliation failed for this run; every other run still reconciles",
					"run", run.ID, "err", rerr)
			}
			c.parkUnreconcilableRun(ctx, run, rerr, now)
		}
	}
	return errors.Join(failures...)
}

// reconcileRun is Reconcile's per-run body, extracted so one run's failure is
// one run's failure. Returning early from it is the same "this run is done for
// this pass" the loop's `continue` used to mean.
func (c *Coordinator) reconcileRun(ctx stdctx.Context, run domain.WorkflowRun, now time.Time) error {
	// CP3: before ANY decision is taken from this run's policy, make sure the
	// policy is one somebody actually chose. A run whose creation recorded
	// that the execution-policy freeze was owed, and whose freeze never
	// landed, is re-frozen here from its owner's stored policy; a run that
	// cannot be re-proven is refused, and the caller parks it. Legacy and
	// unowned runs are untouched. Everything below -- the autonomous kickoff,
	// the validated-plan resume, child inheritance -- reads
	// policyForRun(run), and reading a substituted default there is exactly
	// how an autonomous objective silently became a manual one.
	healed, perr := c.ensureFrozenExecutionPolicy(ctx, run)
	if perr != nil {
		return perr
	}
	run = healed

	// P1-A: the same argument, for the same reason, about execution strategy.
	// A run created before the strategy model has one recorded here, mapped
	// from the durable facts it does have (planned child / owns a plan row /
	// neither) and stamped `recovered` so nothing later mistakes the mapping
	// for somebody's choice. A run that already carries a selection is
	// untouched, which is what makes "a restart cannot select a different
	// strategy" true by construction.
	strategyHealed, serr := c.ensureRecordedExecutionStrategy(ctx, run)
	if serr != nil {
		return serr
	}
	run = strategyHealed

	// P1-B: close the repair loop before anything else reads this run's state.
	// A repair run that finished while the daemon was down has to be folded in
	// (exactly once -- reconcileRepairOutcome is idempotent over its own
	// ledger rows) before reconciliation decides what the origin run still
	// owes. maybeAutoRepair is the ONLY automatic launch, and it is gated on
	// the run's own frozen policy saying `automatic`, on an explicitly
	// repairable condition, and on an unspent budget.
	c.reconcileRepairOutcome(ctx, run)
	c.maybeAutoRepair(ctx, run)

	// P1-C: return whatever runtime capacity this run no longer legitimately
	// holds -- everything, if it is terminal; a superseded generation's claim,
	// if the lifecycle moved past it. Per-run and best-effort on purpose: a run
	// whose capacity state cannot be reasoned about must not stop every other
	// run from being scheduled.
	c.reconcileCapacityForRun(ctx, run)
	// P1-D: the same pass, for the same reason. A placement whose generation
	// was superseded by a crashed replacement, or one belonging to a run that
	// is over, must stop being an authority — and boot is where a stale one is
	// most likely to be found.
	c.reconcilePlacementsForRun(ctx, run)
	// P1-F: and the runtimes a terminal run still owns. Boot is where a run
	// that finished while the daemon was down is found, and its agent process
	// has been running ever since with nothing to do. Idempotent: a session
	// already terminated is skipped, an absent runtime resolves to "absent".
	c.reclaimTerminalRuntimesForRun(ctx, run.ID, run.State)

	if c.planStore != nil {
		if plan, master, planErr := c.planStore.GetWorkflowPlan(ctx, run.ID); planErr != nil {
			return planErr
		} else if master {
			switch {
			case plan.Status == domain.WorkflowPlanPending:
				// Checkpoint 8P-D: heal the narrow crash window between a
				// master run's creation and maybeKickoffAutonomousPlanning's
				// wake write -- if that write never landed (daemon died
				// mid-request) an autonomous objective would otherwise sit
				// at Pending forever with no wake and no browser ever
				// opening it. Schedule (idempotently) re-ensures the
				// kickoff wake exists; a no-op for manual-mode runs or ones
				// that already have it.
				c.maybeKickoffAutonomousPlanning(ctx, run, policyForRun(run).Execution)
			case plan.Status == domain.WorkflowPlanRunning && plan.CommandStatus == domain.WorkflowPlanCommandResponded:
				if _, err := c.finalizeGeneratedPlan(ctx, run, plan); err != nil {
					return err
				}
			case plan.Status == domain.WorkflowPlanRunning:
				// CP32, the plan-segment twin of CP31: the explanation is
				// durable BEFORE the terminal row. FinishWorkflowPlan(invalid,
				// failed) is permanent for GeneratePlan, so a crash between
				// these two writes in the old order left a permanently invalid
				// plan under a run still saying `pending`, with nothing on the
				// ledger anyone could read. The reason row is harmless if the
				// terminal row never lands; the terminal row is not harmless
				// without the reason.
				//
				// CP7: the verdict itself is unchanged and stays fail-closed --
				// AO cannot prove whether the discarded planner produced a
				// plan, and guessing would put a fabricated plan under a real
				// objective. What is no longer PERMANENT is the state: see
				// ReopenAmbiguousWorkflowPlan, the human-only, bounded,
				// observed-version reopen that gives this run a way back.
				validation := `{"valid":false,"errors":["planner state is ambiguous after daemon restart"]}`
				c.recordAttentionStop(ctx, run, nil, ReasonPlannerAmbiguous,
					"the planner command was in flight when the daemon restarted, and AO cannot prove whether it produced a plan; a person may reopen planning for this objective")
				_, _ = c.planStore.FinishWorkflowPlan(ctx, run.ID, domain.WorkflowPlanInvalid, domain.WorkflowPlanCommandFailed, validation, "", "planner_ambiguous", now)
				if run.State == domain.WorkflowRunPending || run.State == domain.WorkflowRunWaiting || run.State == domain.WorkflowRunRunning {
					_, _ = c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, now)
				}
			case plan.Status == domain.WorkflowPlanValidated:
				// CP11/CP12: a validated plan that should have auto-approved
				// had NO resolver at all. Boot recovery's switch had no
				// `validated` case, getMasterRun reconciles only once
				// approved, and ContinueRun delegated to GetRun, which does
				// the same nothing -- so an autonomous objective stalled
				// silently at "plan ready" and never dispatched a task. For a
				// MANUAL run the same state is correct (it is the approval
				// prompt), which is why the stall was invisible; that case is
				// still left exactly alone here.
				if err := c.resumeValidatedPlan(ctx, run, plan); err != nil {
					return err
				}
			case plan.Status == domain.WorkflowPlanApproved:
				// CP13/CP14: an approved plan whose plan step or run row was
				// left behind by a crash inside ApprovePlan's write set is
				// healed here, and ONLY here -- nothing re-enters ApprovePlan
				// once a plan is approved. CP14 is the consequential half: an
				// objective whose run row never reached `running` dispatches
				// tasks forever and can never complete or report a stop,
				// because every branch of reconcileMasterTasksOnce that parks
				// or completes it is gated on run.State == running. So the
				// convergence runs BEFORE the task reconciliation that depends
				// on it, and the run is re-read afterwards.
				c.convergeApprovedPlan(ctx, run)
				if refreshed, ok, gerr := c.store.GetWorkflowRun(ctx, run.ID); gerr == nil && ok {
					run = refreshed
				}
				if err := c.reconcileMasterTasks(ctx, run); err != nil {
					return err
				}
			}
			return nil
		}
		// CP1: a run with a `plan` step and NO workflow_plans row. See
		// healOrphanedObjectiveRun -- it is a master objective nothing can
		// recognise as one, so it is not resumable, not completable and not
		// explicable until this heals or parks it.
		if healed, herr := c.healOrphanedObjectiveRun(ctx, run, now); herr != nil {
			return herr
		} else if healed {
			return nil
		}
	}
	// CP24-CP27: finish an interrupted StartRun before the generic per-step
	// rules below get a chance to park the run for a crash that is fully
	// re-derivable. See resumeInterruptedStart.
	resumed, rerr := c.resumeInterruptedStart(ctx, run)
	if rerr != nil {
		return rerr
	}
	run = resumed

	steps, err := c.store.ListWorkflowSteps(ctx, run.ID)
	if err != nil {
		return err
	}

	interrupted := false
	for _, step := range steps {
		c.checkStepIntegrity(ctx, step)

		if step.Kind == domain.WorkflowStepWork {
			// Before anything is dispatched: read the durable launch
			// evidence under this step and resolve whatever a crash left
			// contradicting it -- an attempt open over a launch that never
			// happened, a worker launched and never confirmed, a step
			// RUNNING over an execution that is gone. Dispatch is
			// idempotent, but it cannot answer those questions: it asks
			// "has this step got a session" and re-enters from the outbox,
			// which is not the same thing as asking which phase of the
			// launch actually completed. See dispatch_reconcile.go.
			//
			// It runs FIRST for the same reason the intent record is
			// written before the launcher is invoked: a live worker AO has
			// not yet recognised must be adopted before anything gets the
			// chance to start a second one over it.
			reconciled, reconciledRun, rerr := c.ReconcileWorkStepDispatch(ctx, run, step)
			if rerr != nil {
				return rerr
			}
			run = reconciledRun
			if reconciled.Action.Resolved() {
				// The step may have moved (adopted -> running, stopped ->
				// waiting), and dispatch below must see where it actually
				// is rather than where this loop found it.
				refreshed, ok, serr := c.getWorkflowStep(ctx, run.ID, step.ID)
				if serr != nil {
					return serr
				}
				if !ok {
					continue
				}
				step = refreshed
			}
			if step.State != domain.WorkflowStepReady && step.State != domain.WorkflowStepRunning {
				continue
			}
			prompt := promptForRun(run, steps)
			updated, err := c.dispatchWorkStep(ctx, run, step, prompt, false)
			if err != nil {
				return err
			}
			if refreshed, ok, rerr := c.store.GetWorkflowRun(ctx, run.ID); rerr == nil && ok {
				run = refreshed
			}
			if updated.State == domain.WorkflowStepRunning {
				if _, err := c.observeWorkStep(ctx, run, updated); err != nil {
					return err
				}
			}
			// dispatchWorkStep/observeWorkStep persist their own state/run
			// transitions (or leave them unchanged if the worker is still
			// legitimately active); the work step never participates in
			// the generic "interrupted -> needs_attention" fallback below.
			continue
		}

		if step.Kind == domain.WorkflowStepReview || step.Kind == domain.WorkflowStepFix {
			// Checkpoint 8C/8D: recovery re-enters the exact same
			// idempotent dispatch/observe cascade ContinueRun/GetRun use
			// (advanceReviewFixCycle), handled once per run below (not
			// per step) — skip the generic per-step fallback for both
			// kinds here.
			continue
		}

		if step.State != domain.WorkflowStepRunning {
			continue
		}
		interrupted = true
		if _, err := c.store.UpdateWorkflowStepState(ctx, step.ID, domain.WorkflowStepRunning, domain.WorkflowStepWaiting, now); err != nil {
			return err
		}
	}

	// Re-read the run and steps: the work-step handling above may already
	// have moved the run (e.g. to needs_attention or waiting) and the
	// work step itself to completed — the review/fix cascade below must
	// see that fresh state, not the pre-loop snapshot.
	if refreshedRun, ok, rerr := c.store.GetWorkflowRun(ctx, run.ID); rerr == nil && ok {
		run = refreshedRun
	}
	if refreshedSteps, rerr := c.store.ListWorkflowSteps(ctx, run.ID); rerr == nil {
		steps = refreshedSteps
	}
	// Checkpoint 8D: the same automatic review<->fix cascade
	// GetRun/ContinueRun drive, re-entered at boot so a crash mid-cycle
	// resumes without any HTTP traffic at all. includeCycle1Unblock=false,
	// mirroring 8C's original exclusion: a review step that has never
	// been explicitly unblocked (no ContinueRun call yet, still
	// "pending") stays untouched by boot recovery.
	if !run.State.Terminal() {
		updatedRun, err := c.advanceReviewFixCycle(ctx, run, steps, false)
		if err != nil {
			return err
		}
		run = updatedRun
	}

	if !interrupted {
		return nil
	}
	// Re-read the run again: a work step's observeWorkStep call above, or
	// the cascade just run, may already have moved it.
	current, ok, err := c.store.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		return err
	}
	if !ok || (current.State != domain.WorkflowRunRunning && current.State != domain.WorkflowRunWaiting) {
		return nil
	}
	if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, current.State, domain.WorkflowRunNeedsAttention, now); err != nil {
		return err
	}
	c.recordAttentionStop(ctx, current, nil, ReasonRecoveryInterrupted,
		"a step was mid-execution when the daemon restarted and has no independent fact source to recover from")
	return nil
}

// parkUnreconcilableRun moves ONE run AO could not reconcile into
// needs_attention with an actionable reason.
//
// It is what turns "this run failed reconciliation again" from an invisible
// forever-retry into a stop a person can see and act on. It is deliberately
// conservative: only a run AO is actively driving (pending/waiting/running) is
// parked, terminal runs are left exactly as they are, and the park is recorded
// once per distinct detail so repeated boots do not grow the ledger.
func (c *Coordinator) parkUnreconcilableRun(
	ctx stdctx.Context, run domain.WorkflowRun, cause error, now time.Time,
) {
	switch run.State {
	case domain.WorkflowRunPending, domain.WorkflowRunWaiting, domain.WorkflowRunRunning:
	default:
		return
	}
	if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, now); err != nil {
		return
	}
	refreshed := run
	if r, ok, gerr := c.store.GetWorkflowRun(ctx, run.ID); gerr == nil && ok {
		refreshed = r
	}
	c.recordAttentionStopOnce(ctx, refreshed, nil, ReasonRecoveryUnreconcilable,
		"AO could not reconcile this run's durable state against its runtime: "+cause.Error())
}

// checkStepIntegrity best-effort verifies that a step's optional session/
// review-run references still resolve. A dangling reference is only logged —
// it must never fail daemon startup.
func (c *Coordinator) checkStepIntegrity(ctx stdctx.Context, step domain.WorkflowStep) {
	if c.log == nil {
		return
	}
	if step.SessionID != nil && c.sessions != nil {
		if _, ok, err := c.sessions.GetSession(ctx, domain.SessionID(*step.SessionID)); err != nil {
			c.log.Warn("workflow recovery: session lookup failed", "step", step.ID, "session", *step.SessionID, "err", err)
		} else if !ok {
			c.log.Warn("workflow recovery: step references missing session", "step", step.ID, "session", *step.SessionID)
		}
	}
	if step.ReviewRunID != nil && c.reviewRuns != nil {
		if _, ok, err := c.reviewRuns.GetReviewRun(ctx, *step.ReviewRunID); err != nil {
			c.log.Warn("workflow recovery: review run lookup failed", "step", step.ID, "reviewRun", *step.ReviewRunID, "err", err)
		} else if !ok {
			c.log.Warn("workflow recovery: step references missing review run", "step", step.ID, "reviewRun", *step.ReviewRunID)
		}
	}
}
