package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// verify_race_reconcile.go — repairing a run left incoherent by a verify race.
//
// verify_authority.go stops the race happening again. It cannot repair the runs
// it already happened to, and those runs are in a state no supported transition
// produces: terminal, with a step still running that a losing verification
// started.
//
// wf-04e8309d is the case. One execution passed, won the right to complete the
// run, and completed it. A concurrent execution of the same attempt failed on a
// flaky command, and — because nothing arbitrated the two — also acted: it
// re-opened the fix step and dispatched fix cycle 5 into a run that was by then
// finished. The result reads as impossible: run completed, fix running.
//
// What this does NOT do: decide which verification was right. That was already
// decided, durably, by whichever execution concluded the attempt — and the
// run's own state is the consequence of that decision. Re-litigating it here
// would be inventing a second authority, which is the disease rather than the
// cure. This only stands down the effects the LOSER produced, so the run's
// state matches the decision that actually won.
//
// It never completes a run, never cancels one, never raises a budget and never
// touches a verdict. It moves a step the loser started back out of flight,
// closes the attempt the loser opened, and records what it did.

// verifyRaceReconciledPhase is the durable record of one repair. Bookkeeping
// about a run rather than a step of it, so it is excluded from every fold into
// derived state (see isBookkeepingPhase).
const verifyRaceReconciledPhase = "verify_race_reconciled"

// verifyRaceRepairRecord is the durable account: what was found, what decided
// the run, and what was stood down.
type verifyRaceRepairRecord struct {
	RunState string `json:"runState"`
	// DecidedByAttempt is the verify attempt whose conclusion the run's state
	// reflects, and DecidedOutcome what it concluded.
	DecidedByAttempt string `json:"decidedByAttempt,omitempty"`
	DecidedOutcome   string `json:"decidedOutcome,omitempty"`
	// StoodDownSteps names the steps taken out of flight, and ClosedAttempts
	// the attempt rows closed with them.
	StoodDownSteps  []string  `json:"stoodDownSteps,omitempty"`
	ClosedAttempts  []string  `json:"closedAttempts,omitempty"`
	SupersededCount int       `json:"supersededVerifyResults"`
	ObservedAt      time.Time `json:"observedAt"`
}

// reconcileVerifyRace repairs a terminal run that still has a step in flight.
//
// It reports whether it changed anything. For every coherent run — which is all
// of them once verify_authority.go is in place — it is a no-op after one cheap
// state check.
func (c *Coordinator) reconcileVerifyRace(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	steps []domain.WorkflowStep,
) (bool, error) {
	// Only a terminal run can be incoherent in this particular way. A
	// non-terminal run with a running step is simply a run doing work.
	if !run.State.Terminal() {
		return false, nil
	}
	why, active := runHasActiveWork(steps)
	if !active {
		return false, nil
	}

	// Which decision the run's state actually reflects. The winner is the
	// verify attempt that carries a conclusion; it is read rather than chosen,
	// because choosing would make this a second authority.
	decidedBy, decidedOutcome := "", ""
	for _, s := range steps {
		if s.Kind != domain.WorkflowStepVerify {
			continue
		}
		attempts, err := c.store.ListWorkflowAttempts(ctx, s.ID)
		if err != nil {
			return false, err
		}
		for _, a := range attempts {
			if a.FinishedAt != nil && a.Outcome != "" {
				decidedBy, decidedOutcome = a.ID, string(a.Outcome)
			}
		}
	}
	// Without a concluded verification AO cannot say what completed this run,
	// and standing anything down would be a guess. A person owns that.
	if decidedBy == "" {
		if c.log != nil {
			c.log.Warn("workflow: a terminal run has work in flight but no concluded verification to explain it",
				"run", run.ID, "reason", why)
		}
		return false, nil
	}

	now := c.clock()
	rec := verifyRaceRepairRecord{
		RunState:         string(run.State),
		DecidedByAttempt: decidedBy,
		DecidedOutcome:   decidedOutcome,
		ObservedAt:       now,
	}
	for _, cp := range c.checkpointsWithPhase(ctx, run.ID, verifySupersededPhase) {
		_ = cp
		rec.SupersededCount++
	}

	// Stand down every step the losing execution left in flight, and close the
	// attempts it opened. `waiting`, never `failed`: the step did nothing wrong
	// and a terminal step would be a second lie on top of the first.
	changed := false
	for _, s := range steps {
		switch s.Kind {
		case domain.WorkflowStepWork, domain.WorkflowStepFix, domain.WorkflowStepReview, domain.WorkflowStepVerify:
		default:
			continue
		}
		if s.State != domain.WorkflowStepRunning && s.State != domain.WorkflowStepReady {
			continue
		}
		attempts, err := c.store.ListWorkflowAttempts(ctx, s.ID)
		if err != nil {
			return changed, err
		}
		for _, a := range attempts {
			if a.FinishedAt != nil {
				continue
			}
			ok, cerr := c.store.ClaimWorkflowAttemptOutcome(ctx, a.ID, now, domain.WorkflowAttemptCancelled, "")
			if cerr != nil {
				return changed, cerr
			}
			if ok {
				rec.ClosedAttempts = append(rec.ClosedAttempts, a.ID)
			}
		}
		moved, err := c.store.UpdateWorkflowStepState(ctx, s.ID, s.State, domain.WorkflowStepWaiting, now)
		if err != nil {
			return changed, err
		}
		if moved {
			rec.StoodDownSteps = append(rec.StoodDownSteps, string(s.Kind))
			changed = true
		}
	}
	if !changed && len(rec.ClosedAttempts) == 0 {
		return false, nil
	}

	payload, err := json.Marshal(rec)
	if err != nil {
		return changed, err
	}
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:            "wfc-" + c.newID(),
		WorkflowRunID: run.ID,
		ProjectID:     run.ProjectID,
		RetryState:    string(payload),
		NextAction: fmt.Sprintf(
			"%s: this run is %s because verify attempt %s concluded %s; %v had been left in flight by a concurrent verification that lost that decision and have been stood down",
			verifyRaceReconciledPhase, run.State, decidedBy, decidedOutcome, rec.StoodDownSteps),
		DurablePhase:   verifyRaceReconciledPhase,
		PayloadVersion: verifyResultVersion,
		CreatedAt:      now,
	}); err != nil {
		return changed, err
	}
	if c.log != nil {
		c.log.Info("workflow: repaired a run left incoherent by a concurrent verification",
			"run", run.ID, "decidedBy", decidedBy, "outcome", decidedOutcome, "stoodDown", rec.StoodDownSteps)
	}
	return true, nil
}

// checkpointsWithPhase is a small read helper used for the repair's accounting.
func (c *Coordinator) checkpointsWithPhase(ctx stdctx.Context, runID, phase string) []domain.WorkflowCheckpoint {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return nil
	}
	var out []domain.WorkflowCheckpoint
	for _, cp := range cps {
		if cp.DurablePhase == phase {
			out = append(out, cp)
		}
	}
	return out
}
