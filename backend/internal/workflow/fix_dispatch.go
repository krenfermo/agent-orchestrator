package workflow

import (
	stdctx "context"
	"strconv"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// MessageSender is the narrow existing-session messaging path workflow
// reuses to deliver fix findings to the SAME Codex worker, never a new one.
// Satisfied by *session_manager.Manager (the same Manager.Send that backs
// `ao send` and the orchestrator's delegated-task title-refinement message).
//
// Critical, research-confirmed fact (from Checkpoint 8B's investigation):
// Manager.Send's public API has NO idempotency key (the private variant with
// a clientMessageID is only used by the interface-transition outbox
// internally). So workflow must never rely on Send itself for idempotency —
// the same outbox-guarded dispatch pattern as dispatch.go/review_dispatch.go
// applies: enqueue a workflow_outbox entry first, only call Send from the
// "pending" branch, and treat a recovered "dispatched" entry as ambiguous
// rather than ever calling Send a second time for the same cycle.
type MessageSender interface {
	Send(ctx stdctx.Context, id domain.SessionID, message string, attachment *ports.SpawnAttachment) error
}

// fixStepOutboxIdempotencyKey is the deterministic, cycle-specific
// idempotency key for a fix step's send-message command. Cycle-specific for
// the same reason review dispatch is (design decision 2): a second fix cycle
// for the same step must get its own outbox row / single-flight guard.
func fixStepOutboxIdempotencyKey(stepID string, cycleNumber int) string {
	return "workflow-step-fix:" + stepID + ":cycle" + strconv.Itoa(cycleNumber)
}

// dispatchFixStep is Checkpoint 8D's single idempotent dispatch algorithm for
// delivering one cycle of fix findings to the existing worker session. Safe
// to call repeatedly — from GetRun's cascade, from Reconcile, and from a
// future ContinueRun nudge — without ever sending the same cycle's findings
// twice: the primary, cheapest guard is that this cycle's workflow_attempt
// already exists (mirrors dispatchWorkStep's SessionID guard / dispatchReviewStep's
// ReviewRunID guard, adapted to a step that is dispatched repeatedly across
// cycles rather than once).
func (c *Coordinator) dispatchFixStep(ctx stdctx.Context, run domain.WorkflowRun, workStep, fixStep domain.WorkflowStep, reviewRun domain.ReviewRun, cycleNumber int, prompt string) (domain.WorkflowStep, error) {
	if run.State.Terminal() || fixStep.State.Terminal() {
		return fixStep, nil
	}
	// Checkpoint 8K-A: never send fix findings into a session paused on an
	// unresolved question.
	if open, err := c.hasOpenQuestion(ctx, run.ID, &fixStep.ID); err != nil {
		return fixStep, err
	} else if open {
		return fixStep, nil
	}
	if c.messageSender == nil {
		return fixStep, nil
	}
	attempts, err := c.store.ListWorkflowAttempts(ctx, fixStep.ID)
	if err != nil {
		return fixStep, err
	}
	if int64(len(attempts)) >= int64(cycleNumber) {
		// This cycle already has its attempt row: either fully dispatched
		// and progressing, or ambiguous and awaiting attention. Either way,
		// never re-enter dispatch for it.
		return fixStep, nil
	}

	now := c.clock()
	entry, _, err := c.store.EnqueueWorkflowOutboxEntry(ctx, domain.WorkflowOutboxEntry{
		ID:             "wfo-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &fixStep.ID,
		IdempotencyKey: fixStepOutboxIdempotencyKey(fixStep.ID, cycleNumber),
		CommandType:    domain.WorkflowOutboxSendMessage,
		Payload:        fixPayloadJSON(fixStep.ID, reviewRun.ID, cycleNumber),
		CreatedAt:      now,
	})
	if err != nil {
		return fixStep, err
	}

	switch entry.Status {
	case domain.WorkflowOutboxPending:
		return c.dispatchFixFromPending(ctx, run, workStep, fixStep, entry, reviewRun, cycleNumber, prompt)
	case domain.WorkflowOutboxDispatched, domain.WorkflowOutboxAcknowledged:
		// Boundary B: a previous attempt got at least as far as "about to
		// call Send," but Send has no idempotency key and no reliable
		// positive-delivery fact is available here in general, so recovery
		// must NOT call Send again ("nunca asumir éxito"). Surface ambiguity.
		return c.markFixAmbiguous(ctx, run, fixStep, entry,
			"ambiguous_fix_delivery: cannot confirm message delivery after restart")
	case domain.WorkflowOutboxFailed:
		return fixStep, nil
	default:
		return fixStep, nil
	}
}

func (c *Coordinator) dispatchFixFromPending(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	workStep, fixStep domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry,
	reviewRun domain.ReviewRun,
	cycleNumber int,
	prompt string,
) (domain.WorkflowStep, error) {
	now := c.clock()
	if _, err := c.store.UpdateWorkflowOutboxStatus(ctx, entry.ID, domain.WorkflowOutboxPending, domain.WorkflowOutboxDispatched, now, ""); err != nil {
		return fixStep, err
	}
	entry.Status = domain.WorkflowOutboxDispatched

	from := fixStep.State
	if from == domain.WorkflowStepPending {
		if _, err := c.store.UpdateWorkflowStepState(ctx, fixStep.ID, domain.WorkflowStepPending, domain.WorkflowStepReady, now); err != nil {
			return fixStep, err
		}
		from = domain.WorkflowStepReady
		fixStep.State = domain.WorkflowStepReady
	}
	// ready->running (cycle 1) and waiting->running (cycle N+1) are both
	// valid transitions.
	if from == domain.WorkflowStepReady || from == domain.WorkflowStepWaiting {
		if _, err := c.store.UpdateWorkflowStepState(ctx, fixStep.ID, from, domain.WorkflowStepRunning, now); err != nil {
			return fixStep, err
		}
		fixStep.State = domain.WorkflowStepRunning
	}

	if err := c.messageSender.Send(ctx, reviewRun.SessionID, prompt, nil); err != nil {
		return c.recordFixDispatchFailure(ctx, run, fixStep, entry, domain.WorkflowErrorPromptDeliveryFailed, err)
	}
	return c.recordFixDispatchSuccess(ctx, run, workStep, fixStep, entry, reviewRun, cycleNumber)
}

func (c *Coordinator) recordFixDispatchSuccess(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	workStep, fixStep domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry,
	reviewRun domain.ReviewRun,
	cycleNumber int,
) (domain.WorkflowStep, error) {
	now := c.clock()

	attempts, err := c.store.ListWorkflowAttempts(ctx, fixStep.ID)
	if err != nil {
		return fixStep, err
	}
	if int64(len(attempts)) < int64(cycleNumber) {
		if _, err := c.store.CreateWorkflowAttempt(ctx, "wfa-"+c.newID(), fixStep.ID, "codex", "", now); err != nil {
			return fixStep, err
		}
	}

	if entry.Status != domain.WorkflowOutboxAcknowledged {
		if _, err := c.store.UpdateWorkflowOutboxStatus(ctx, entry.ID, entry.Status, domain.WorkflowOutboxAcknowledged, now, ""); err != nil {
			return fixStep, err
		}
	}

	rid := reviewRun.ID
	sid := string(reviewRun.SessionID)
	stepID := fixStep.ID
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:                "wfc-" + c.newID(),
		WorkflowRunID:     run.ID,
		WorkflowStepID:    &stepID,
		ProjectID:         run.ProjectID,
		SessionID:         &sid,
		ReviewRunID:       &rid,
		FingerprintBefore: reviewRun.TargetSHA,
		NextAction:        "fix_dispatched: awaiting a genuinely new workspace fingerprint",
		DurablePhase:      "fix_dispatched",
		PayloadVersion:    "v1",
		RetryState:        "{}",
		CreatedAt:         now,
	}); err != nil {
		return fixStep, err
	}
	return fixStep, nil
}

func (c *Coordinator) recordFixDispatchFailure(ctx stdctx.Context, run domain.WorkflowRun, fixStep domain.WorkflowStep, entry domain.WorkflowOutboxEntry, errClass domain.WorkflowErrorClass, cause error) (domain.WorkflowStep, error) {
	now := c.clock()
	if _, err := c.store.UpdateWorkflowOutboxStatus(ctx, entry.ID, entry.Status, domain.WorkflowOutboxFailed, now, string(errClass)); err != nil {
		return fixStep, err
	}
	if fixStep.State == domain.WorkflowStepRunning {
		if _, err := c.store.UpdateWorkflowStepState(ctx, fixStep.ID, domain.WorkflowStepRunning, domain.WorkflowStepFailed, now); err != nil {
			return fixStep, err
		}
		fixStep.State = domain.WorkflowStepFailed
	}
	if run.State == domain.WorkflowRunRunning || run.State == domain.WorkflowRunWaiting {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, now); err != nil {
			return fixStep, err
		}
	}
	if c.log != nil {
		c.log.Warn("workflow: fix step dispatch failed", "step", fixStep.ID, "err", cause)
	}
	return fixStep, nil
}

// markFixAmbiguous handles a retry/recovery call that found the fix
// outbox entry already dispatched (or, defensively, acknowledged), with no
// reliable fact proving Send actually happened. Never calls Send again.
func (c *Coordinator) markFixAmbiguous(ctx stdctx.Context, run domain.WorkflowRun, fixStep domain.WorkflowStep, entry domain.WorkflowOutboxEntry, nextAction string) (domain.WorkflowStep, error) {
	now := c.clock()
	if fixStep.State == domain.WorkflowStepRunning || fixStep.State == domain.WorkflowStepReady {
		from := fixStep.State
		if _, err := c.store.UpdateWorkflowStepState(ctx, fixStep.ID, from, domain.WorkflowStepWaiting, now); err != nil {
			return fixStep, err
		}
		fixStep.State = domain.WorkflowStepWaiting
	}
	if run.State == domain.WorkflowRunRunning || run.State == domain.WorkflowRunWaiting {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, now); err != nil {
			return fixStep, err
		}
	}
	stepID := fixStep.ID
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		NextAction:     nextAction,
		DurablePhase:   "fix_dispatch_ambiguous",
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      now,
	}); err != nil {
		return fixStep, err
	}
	return fixStep, nil
}

func fixPayloadJSON(fixStepID, reviewRunID string, cycleNumber int) string {
	return `{"fixStepId":"` + fixStepID + `","reviewRunId":"` + reviewRunID + `","cycle":` + strconv.Itoa(cycleNumber) + `}`
}
