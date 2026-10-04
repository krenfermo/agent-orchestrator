package workflow

import (
	stdctx "context"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// P9 §25 — ownership readback.
//
// "Why did AO adopt / refuse / relaunch this worker?" must be answerable without
// opening the database. WorkerOwnershipFor reports, per worker step, the same
// facts and the same decision recovery acts on -- by calling the very
// decideWorkerAdoption recovery calls, so the readback cannot drift from the
// behaviour it explains.
//
// It is a STRICT read: store reads and one runtime identity read-back per
// session. It never goes through GetRun (whose read path reconciles), never
// writes a checkpoint, and never launches, adopts or stops anything. Identities
// and closed codes only: no prompt, no command, no environment, no token.

// WorkerOwnershipReadback is one worker step's ownership, as recovery sees it.
type WorkerOwnershipReadback struct {
	StepID    string
	StepKind  domain.WorkflowStepKind
	StepState domain.WorkflowStepState
	SessionID string

	Ownership              domain.WorkerOwnershipStatus
	Proof                  domain.WorkerRuntimeProof
	RuntimeInstanceID      string
	ObservedInstanceID     string
	ObservedInstallationID string

	LaunchState        domain.WorkflowOutboxStatus
	DispatchGeneration string
	DispatchPhase      WorkerDispatchPhase
	AttemptID          string
	LastSignalAt       time.Time

	// Decision/Reason are what recovery would decide about this session NOW.
	// Empty for a step with no session, and for a fix cycle (recovery never
	// adopts or relaunches a fix -- it rides in the worker's own session).
	Decision WorkerRecoveryAction
	Reason   WorkerRecoveryReason
	Detail   string
}

// WorkerOwnershipFor is the readback for one run. See the file comment.
func (c *Coordinator) WorkerOwnershipFor(ctx stdctx.Context, runID string) ([]WorkerOwnershipReadback, error) {
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: workflow run %s", ErrNotFound, runID)
	}
	steps, err := c.store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := make([]WorkerOwnershipReadback, 0, 2)
	for _, step := range steps {
		switch {
		case step.Kind == domain.WorkflowStepWork:
			out = append(out, c.workStepOwnership(ctx, run, step))
		case step.Kind == domain.WorkflowStepFix && step.State == domain.WorkflowStepRunning:
			out = append(out, c.fixStepOwnership(ctx, run, step))
		}
	}
	return out, nil
}

func (c *Coordinator) workStepOwnership(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep) WorkerOwnershipReadback {
	rb := WorkerOwnershipReadback{StepID: step.ID, StepKind: step.Kind, StepState: step.State,
		Ownership: domain.WorkerOwnershipNotApplicable}
	status := c.WorkerDispatchStatusForStep(ctx, run.ID, step.ID)
	rb.DispatchPhase, rb.AttemptID = status.Phase, status.AttemptID
	entry, found, err := c.findDispatchOutboxEntry(ctx, run, step)
	if err == nil && found {
		rb.LaunchState, rb.DispatchGeneration = entry.Status, entry.DispatchGeneration
	}

	sessionID := ""
	switch {
	case step.SessionID != nil && *step.SessionID != "":
		sessionID = *step.SessionID
	case status.SessionID != "":
		sessionID = status.SessionID
	case c.sessionFacts != nil:
		if rec, ok, ferr := c.sessionFacts.FindSessionByProjectAndIssueID(
			ctx, domain.ProjectID(run.ProjectID), workStepIssueID(step.ID)); ferr == nil && ok {
			sessionID = string(rec.ID)
		}
	}
	if sessionID == "" {
		rb.Detail = "no worker session exists for this step"
		return rb
	}
	rb.SessionID = sessionID
	rec, readable := c.readableSessionRecord(ctx, domain.SessionID(sessionID))
	if !readable {
		rb.Ownership = domain.WorkerOwnershipUnproven
		rb.Detail = "the session row could not be read"
		return rb
	}
	rb.LastSignalAt = rec.Activity.Liveness()
	// The read-only form: asking "who owns this" never starts or extends the
	// unreadable-runtime grace recovery decides stops on.
	obs := c.readWorkerRuntime(ctx, rec.ID)
	fillRuntimeProof(&rb, obs)
	decision := decideWorkerAdoption(c.workerAdoptionFactsFor(ctx, run, step, entry, rec, obs))
	if decision.Action != WorkerRecoveryAdopt && entry.DispatchedAt != nil &&
		c.clock().Sub(*entry.DispatchedAt) < dispatchReconcileSettleWindow {
		// Recovery concludes nothing but an adoption inside the settle window,
		// and the readback says so rather than showing a stop that is not due.
		decision = WorkerRecoveryDecision{Action: WorkerRecoveryWait, Reason: WorkerReasonSettleWindow,
			Detail: "the launch is younger than its settle window; only an adoption may be concluded yet"}
	}
	rb.Decision, rb.Reason, rb.Detail = decision.Action, decision.Reason, decision.Detail
	return rb
}

func (c *Coordinator) fixStepOwnership(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep) WorkerOwnershipReadback {
	rb := WorkerOwnershipReadback{StepID: step.ID, StepKind: step.Kind, StepState: step.State,
		Ownership: domain.WorkerOwnershipNotApplicable}
	sessionID := c.DurableSessionForStep(ctx, run.ID, step)
	if sessionID == "" {
		rb.Detail = "this fix cycle names no worker session"
		return rb
	}
	rb.SessionID = string(sessionID)
	rec, readable := c.readableSessionRecord(ctx, sessionID)
	if !readable {
		rb.Ownership = domain.WorkerOwnershipUnproven
		rb.Detail = "the session row could not be read"
		return rb
	}
	rb.LastSignalAt = rec.Activity.Liveness()
	fillRuntimeProof(&rb, c.readWorkerRuntime(ctx, rec.ID))
	rb.Detail = "fix cycles run inside the worker's own session; recovery never adopts or relaunches them. " + rb.Detail
	return rb
}

func fillRuntimeProof(rb *WorkerOwnershipReadback, obs *domain.WorkerRuntimeObservation) {
	if obs == nil {
		rb.Ownership = domain.WorkerOwnershipUnproven
		rb.Detail = "this daemon has no runtime ownership reader wired"
		return
	}
	rb.Proof = obs.Proof
	rb.Ownership = obs.Proof.OwnershipStatus()
	rb.RuntimeInstanceID = obs.RuntimeInstanceID
	rb.ObservedInstanceID = obs.ObservedInstanceID
	rb.ObservedInstallationID = obs.ObservedInstallationID
	rb.Detail = obs.Detail
}
