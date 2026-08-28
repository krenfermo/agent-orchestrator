package workflow

import (
	stdctx "context"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// planUnblockOwed reports whether a run still owes the plan->work unblock
// StartRun performs -- the CP24-CP27 dead end in
// docs/worker-lifecycle-audit.md.
//
// The obligation is stated as a fact about the durable rows, not as a fact
// about who called what: the work step has never been unblocked (still
// `pending`), and the plan step is in a state from which completing it is the
// right answer. That is deliberately re-derivable by ANY entry point --
// StartRun, boot recovery, a later dispatch pass -- which is exactly what the
// old `run.State != pending` early-exit made impossible.
//
// A plan step that failed or was cancelled is not covered: the unblock is not
// owed there, and nothing here may resurrect a run that stopped for a real
// reason.
func planUnblockOwed(planStep, workStep domain.WorkflowStep) bool {
	if workStep.State != domain.WorkflowStepPending {
		return false
	}
	switch planStep.State {
	case domain.WorkflowStepReady, domain.WorkflowStepRunning,
		domain.WorkflowStepWaiting, domain.WorkflowStepCompleted:
		return true
	}
	return false
}

// startResumableRunState reports whether a non-pending run may have its
// interrupted start finished.
//
// needs_attention is included, and that is the point rather than an
// oversight: CP25/CP26's own boot recovery is what parks the run there, with
// `recovery_interrupted`, while leaving the unblock undone. Excluding it
// would leave precisely the runs the audit names stuck forever. The
// combination is not ambiguous -- a run cannot reach "non-pending, work step
// still pending, plan step not terminal" any other way -- and the resume is
// recorded as durable evidence before anything moves.
func startResumableRunState(state domain.WorkflowRunState) bool {
	switch state {
	case domain.WorkflowRunRunning, domain.WorkflowRunWaiting, domain.WorkflowRunNeedsAttention:
		return true
	}
	return false
}

// recordStartResumed appends the evidence that an interrupted StartRun was
// re-entered and from which observed state, so the repair is inspectable
// instead of looking like the run simply started twice. Best-effort: it
// records a fact and gates nothing.
func (c *Coordinator) recordStartResumed(ctx stdctx.Context, run domain.WorkflowRun, planStep, workStep domain.WorkflowStep) {
	_, _ = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:            "wfc-" + c.newID(),
		WorkflowRunID: run.ID,
		ProjectID:     run.ProjectID,
		NextAction: fmt.Sprintf(
			"resuming an interrupted start: run %s, plan step %s, work step %s — the plan→work unblock never completed",
			run.State, planStep.State, workStep.State),
		DurablePhase:   "start_run_resumed",
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      c.clock(),
	})
}

// resumeInterruptedStart is boot recovery's half of CP24-CP27: before the
// generic per-step rules run, finish any plan->work unblock a crash left
// outstanding.
//
// It runs FIRST for an ordering reason. The generic fallback would otherwise
// see the plan step `running`, move it to `waiting` and park the run
// needs_attention — a correct-looking stop for a run that simply needed its
// own start finished. Running the resume before that fallback means the
// common case never reaches a person at all, and repeated boots converge:
// after the first pass the obligation is discharged and this is a no-op.
func (c *Coordinator) resumeInterruptedStart(ctx stdctx.Context, run domain.WorkflowRun) (domain.WorkflowRun, error) {
	if run.State.Terminal() || run.State == domain.WorkflowRunPending {
		return run, nil
	}
	steps, err := c.store.ListWorkflowSteps(ctx, run.ID)
	if err != nil {
		return run, err
	}
	var planStep, workStep *domain.WorkflowStep
	for i := range steps {
		switch steps[i].Kind {
		case domain.WorkflowStepPlan:
			planStep = &steps[i]
		case domain.WorkflowStepWork:
			workStep = &steps[i]
		}
	}
	if planStep == nil || workStep == nil || !planUnblockOwed(*planStep, *workStep) {
		return run, nil
	}
	if _, err := c.StartRun(ctx, run.ID); err != nil {
		return run, err
	}
	refreshed, ok, err := c.store.GetWorkflowRun(ctx, run.ID)
	if err != nil || !ok {
		return run, err
	}
	return refreshed, nil
}
