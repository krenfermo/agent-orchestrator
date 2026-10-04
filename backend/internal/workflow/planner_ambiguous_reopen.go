package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// planner_ambiguous_reopen.go — CP7's fail-closed way back out.
//
// A planner command in flight across a daemon restart cannot be adopted. The
// worker path can adopt a launch it never confirmed because that launch is
// findable by natural key, its liveness is provable through the runtime, and it
// is fenced by a runtime launch id. The planner has none of that: no intent
// record naming the subprocess, no runtime identity on the plan row, no natural
// key, no adoption path. So AO cannot prove whether the discarded planner
// produced a plan, and guessing would put a fabricated plan under a real
// objective.
//
// recovery.go's verdict therefore stands unchanged — `planner_ambiguous` is
// correct. What this file changes is that the verdict is no longer PERMANENT.
// It is a statement about ONE crossed restart, not about the objective, yet it
// landed as WorkflowPlanInvalid, which GeneratePlan's status switch refuses
// forever. An objective died of a daemon restart.
//
// The reopen is:
//
//   - HUMAN-INITIATED ONLY. Nothing here is reachable from reconcileRun, from
//     any wake reason, from ContinueRun or from the autonomous heartbeat. That
//     is load-bearing: restart -> reopen -> planner -> restart is an unbounded
//     loop spending provider budget with nobody watching, and the only thing
//     standing between this mechanism and that loop is that a person has to ask
//     for each attempt.
//   - BOUNDED, the same way plannerRetryCount bounds the retry budget: it counts
//     the run's own durable planner_ambiguous stops and refuses past a small
//     bound, so even a human holding the button cannot loop forever.
//   - An OBSERVED-VERSION compare-and-swap, NOT a generation fence. See
//     Store.ReopenAmbiguousWorkflowPlan for exactly why a generation is
//     impossible here and must not be claimed in any comment, commit message or
//     UI string.
//   - ORDERED reason-first, like CP30/CP31/CP32: the human-readable checkpoint
//     is durable before the plan row moves.

// ambiguousPlanReopenPhase records one human-authorized reopen.
const ambiguousPlanReopenPhase = "planner_ambiguous_reopened"

// maxAmbiguousPlanReopens bounds how many times ONE objective may be reopened
// out of planner_ambiguous. Two: a single crossed restart is bad luck and worth
// one more attempt; an objective whose planner keeps being caught mid-flight
// twice more has a scheduling or stability problem a third identical attempt
// will not discover.
const maxAmbiguousPlanReopens = 2

// ambiguousPlanReopener is the optional store capability this needs. Asserted
// at the call site rather than added to masterPlanStore so that a store or test
// double without it refuses the reopen with a readable error instead of failing
// to compile.
type ambiguousPlanReopener interface {
	ReopenAmbiguousWorkflowPlan(ctx stdctx.Context, runID string, observedUpdatedAt time.Time, validationJSON string, now time.Time) (bool, error)
}

// ReopenAmbiguousPlan reopens planning for an objective whose planner state
// could not be recovered across a daemon restart.
//
// observedUpdatedAt is the plan row's version as the caller's own view was
// rendered from. It is REQUIRED and it is the whole safety property: it says
// "this is the row I looked at", and any write to the row since — a second
// ambiguity, an approval-mode change, anything — makes this call match zero
// rows and refuse. Every false answer is a refusal, never an accept: a reopen
// that should have succeeded and did not is a nuisance the person fixes by
// re-reading and re-submitting, while a reopen that lands on a state nobody
// looked at is the failure the predicate exists to prevent.
//
// It identifies the ROW, not the planner run. Nothing here tells anyone whether
// the discarded planner produced a plan; the reopen only decides which observed
// state is being reopened. Recovering the planner's own work would need the
// launch identity CP7 lacks, and this does not invent one.
func (c *Coordinator) ReopenAmbiguousPlan(ctx stdctx.Context, runID string, observedUpdatedAt time.Time) (RunDetail, error) {
	if c.planStore == nil {
		return RunDetail{}, fmt.Errorf("%w: planner is unavailable", ErrInvalid)
	}
	reopener, ok := c.planStore.(ambiguousPlanReopener)
	if !ok {
		return RunDetail{}, fmt.Errorf("%w: this store cannot reopen an ambiguous plan", ErrInvalid)
	}
	if observedUpdatedAt.IsZero() {
		return RunDetail{}, fmt.Errorf(
			"%w: reopening an ambiguous plan requires the plan version you observed, so AO can refuse a reopen of a state nobody looked at", ErrInvalid)
	}
	run, found, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	if !found {
		return RunDetail{}, fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	if run.State.Terminal() {
		return RunDetail{}, fmt.Errorf("%w: workflow run %q is already %s", ErrAlreadyTerminal, runID, run.State)
	}
	plan, isMaster, err := c.planStore.GetWorkflowPlan(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	if !isMaster {
		return RunDetail{}, fmt.Errorf("%w: workflow run %q is not a master objective", ErrInvalid, runID)
	}
	if plan.Status != domain.WorkflowPlanInvalid ||
		plan.CommandStatus != domain.WorkflowPlanCommandFailed ||
		plan.ErrorClass != ReasonPlannerAmbiguous {
		return RunDetail{}, fmt.Errorf(
			"%w: workflow run %q's plan is not in the ambiguous-planner state (status %q, command %q, class %q)",
			ErrInvalid, runID, plan.Status, plan.CommandStatus, plan.ErrorClass)
	}

	reopens := c.ambiguousPlanReopenCount(ctx, runID)
	if reopens >= maxAmbiguousPlanReopens {
		return RunDetail{}, fmt.Errorf(
			"%w: planning for this objective has already been reopened %d times after an ambiguous planner and AO will not reopen it again — create a new objective, or investigate why the planner keeps being interrupted",
			ErrInvalid, maxAmbiguousPlanReopens)
	}

	// The stop this reopen answers is read BEFORE anything is written. The
	// reopen's own checkpoint becomes the run's newest durable row the moment it
	// lands, and the run's stop reason is derived from the newest row -- so
	// reading afterwards would find "planner_ambiguous_reopened" and conclude
	// the run was never parked on the thing being reopened.
	parkedOn := ""
	if run.State == domain.WorkflowRunNeedsAttention {
		if reason, _, ok := c.stopReason(ctx, run); ok {
			parkedOn = reason
		}
	}

	// The reason row FIRST. Same ordering rule CP30/CP31/CP32 exist to enforce:
	// a state change whose explanation never landed is an unexplained state, and
	// this one re-arms a planner that spends real provider budget.
	now := c.clock()
	state, _ := json.Marshal(struct {
		Reopen            int       `json:"reopen"`
		Max               int       `json:"max"`
		ObservedUpdatedAt time.Time `json:"observedUpdatedAt"`
		PreviousStatus    string    `json:"previousStatus"`
		PreviousCommand   string    `json:"previousCommandStatus"`
		PreviousClass     string    `json:"previousErrorClass"`
	}{
		Reopen: reopens + 1, Max: maxAmbiguousPlanReopens, ObservedUpdatedAt: observedUpdatedAt,
		PreviousStatus: string(plan.Status), PreviousCommand: string(plan.CommandStatus), PreviousClass: plan.ErrorClass,
	})
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:            "wfc-" + c.newID(),
		WorkflowRunID: runID,
		ProjectID:     run.ProjectID,
		NextAction: fmt.Sprintf(
			"planner_ambiguous_reopened: a person reopened planning (%d of %d) for an objective whose planner was in flight across a restart; "+
				"AO still cannot say whether that planner produced a plan and adopts nothing from it — planning starts over",
			reopens+1, maxAmbiguousPlanReopens),
		DurablePhase:   ambiguousPlanReopenPhase,
		PayloadVersion: "v1",
		RetryState:     string(state),
		CreatedAt:      now,
	}); err != nil {
		return RunDetail{}, err
	}

	validation, _ := json.Marshal(PlanValidation{Valid: false, Errors: []string{
		"the previous planner command was interrupted by a daemon restart and its result was discarded; planning was reopened by a person",
	}})
	moved, err := reopener.ReopenAmbiguousWorkflowPlan(ctx, runID, observedUpdatedAt, string(validation), now)
	if err != nil {
		return RunDetail{}, err
	}
	if !moved {
		// The version the person observed is not the version on disk. Refuse,
		// and say what to do about it -- the same `moved == false` convention
		// ApprovePlan and retryPlanOrFail already use, surfaced rather than
		// swallowed because a human is waiting on the answer.
		return RunDetail{}, fmt.Errorf(
			"%w: this plan changed since you read it, so AO refused to reopen a state you did not see — re-read the objective and try again", ErrInvalid)
	}
	// The run leaves needs_attention only for the stop this reopen answers.
	if parkedOn == ReasonPlannerAmbiguous {
		_ = c.unparkRun(ctx, run, parkedOn, "a person reopened planning after an ambiguous planner")
	}
	if c.log != nil {
		c.log.Warn("workflow: a person reopened planning for an objective whose planner state was ambiguous",
			"run", runID, "reopen", reopens+1, "max", maxAmbiguousPlanReopens)
	}
	return c.GetRun(ctx, runID)
}

// ambiguousPlanReopenCount counts this run's durable reopens. Derived from
// append-only rows for the same reason plannerRetryCount is: the bound has to
// survive a restart, or the reopen a person can press becomes unbounded.
func (c *Coordinator) ambiguousPlanReopenCount(ctx stdctx.Context, runID string) int {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		// Unreadable budget is a spent budget.
		return maxAmbiguousPlanReopens
	}
	n := 0
	for _, cp := range cps {
		if cp.DurablePhase == ambiguousPlanReopenPhase {
			n++
		}
	}
	return n
}
