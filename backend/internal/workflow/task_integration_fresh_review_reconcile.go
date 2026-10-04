package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// task_integration_fresh_review_reconcile.go — closing an integration
// fresh-review request that was answered but never recorded as answered.
//
// An integration fresh-review request is closed by closeIntegrationFreshReview,
// which runs when the task INTEGRATES. That is the right moment for the happy
// path, and it is deliberately late: a request should not look answered while
// the review it asked for is still in flight.
//
// It is also the only moment. A run whose fresh review completed — approved or
// changes_requested — but which then stopped somewhere between that verdict and
// integration keeps the request open forever, because nothing between those two
// points writes the answer. The request has in fact been answered; the ledger
// simply never says so.
//
// An open request is not inert. pendingFreshReview consults it BEFORE every
// other reason a review step can rest at `waiting`, so a stale one shadows them
// all: dispatchReviewStep reads its fingerprint, finds the step already points
// at a review with a different target, concludes this generation's fresh review
// was dispatched, and dispatches nothing. The run rests forever on a question
// that was answered hours earlier. That is exactly what happened to Task 8: a
// request from 13:11:39, answered by an approved review three seconds later,
// still outstanding when a branch-advance recovery tried to ask its own
// question — and silently shadowed by it.
//
// The fix is to close the request on the evidence, not to reorder the readers.
// Preferring the newest request would let this one win, but it would leave a
// stale request alive in the ledger to shadow something else later, and it
// would make "which request is outstanding" depend on arrival order rather than
// on what actually happened.
//
// A request is answered when AO can prove, from durable state alone, that a
// review answered it. All five, every one required:
//
//  1. the review was created AFTER the request — a verdict that predates the
//     question cannot be its answer, however well its target matches;
//  2. it belongs to the same run's authorized review step and the worker
//     session that step reviews, so a review of some other work can never close
//     this task's request;
//  3. it was dispatched for the fingerprint the request ASKED for — the
//     request's CurrentFingerprint, the workspace it wanted read;
//  4. it reached a terminal verdict that actually answers the question:
//     approved or changes_requested. A cancelled, failed or still-running
//     review answers nothing;
//  5. exactly one review satisfies 1–4. Two candidates mean AO cannot say which
//     one answered, and an ambiguous close is a claim AO has not earned.
//
// What it never does: touch the review, the verdict, or any historical row. It
// writes one checkpoint — the same record the request carried, under the
// answered phase — which is precisely what closeIntegrationFreshReview would
// have written had the run reached integration. The ledger ends up saying what
// it should have said, and says when and on what evidence it was reconciled.
//
// Idempotent and restart-safe by construction: once the answered row exists,
// outstandingIntegrationFreshReview returns nothing outstanding, so every later
// pass finds no request to reconcile and writes nothing. There is no second
// ledger and no counter — the answer is its own guard.

// integrationFreshReviewReconciledDetail names the reconciliation in the
// answered row's next_action, so the ledger distinguishes a request closed by
// an actual integration from one closed by this evidence-based catch-up.
const integrationFreshReviewReconciledDetail = "integration_fresh_review_answered (reconciled)"

// reconcileIntegrationFreshReviewAnswer closes this run's outstanding
// integration fresh-review request when the five proofs above hold, and does
// nothing otherwise. It reports whether it closed one.
func (c *Coordinator) reconcileIntegrationFreshReviewAnswer(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	steps []domain.WorkflowStep,
) (bool, error) {
	if c.reviewRuns == nil {
		return false, nil
	}
	record, requestedAt, outstanding, err := c.outstandingIntegrationFreshReviewAt(ctx, run.ID)
	if err != nil || !outstanding {
		return false, err
	}
	// Proof 2a — the request names a review step, and that step is this run's.
	if strings.TrimSpace(record.ReviewStepID) == "" {
		return false, nil
	}
	var reviewStep, workStep *domain.WorkflowStep
	for i := range steps {
		switch steps[i].Kind {
		case domain.WorkflowStepReview:
			reviewStep = &steps[i]
		case domain.WorkflowStepWork:
			workStep = &steps[i]
		}
	}
	if reviewStep == nil || workStep == nil || reviewStep.ID != record.ReviewStepID {
		return false, nil
	}
	// Proof 3 — the request must say which workspace it wanted read. A request
	// with no asked-for fingerprint cannot be matched against anything, and
	// guessing would be exactly the ambiguity proof 5 excludes.
	wanted := strings.TrimSpace(record.CurrentFingerprint)
	if wanted == "" {
		return false, nil
	}
	// Proof 2b — the worker session whose work that step reviews.
	workCP, hasWorkCP, err := c.store.GetLatestWorkflowCheckpointByStep(ctx, workStep.ID)
	if err != nil {
		return false, err
	}
	if !hasWorkCP || workCP.SessionID == nil || strings.TrimSpace(*workCP.SessionID) == "" {
		return false, nil
	}
	reviews, err := c.reviewRuns.ListReviewRunsBySession(ctx, domain.SessionID(strings.TrimSpace(*workCP.SessionID)))
	if err != nil {
		return false, err
	}

	var answer domain.ReviewRun
	found := 0
	for _, r := range reviews {
		// The stale review the request was raised ABOUT can never be its
		// answer, whatever its timestamps say.
		if r.ID == record.PriorReviewRunID {
			continue
		}
		// Proof 1 — strictly after the request.
		if !r.CreatedAt.After(requestedAt) {
			continue
		}
		// Proof 3 — dispatched for the fingerprint the request asked for.
		if r.TargetSHA != wanted {
			continue
		}
		// Proof 4 — a terminal verdict that answers the question.
		if r.Status != domain.ReviewRunComplete {
			continue
		}
		if v := r.EffectiveVerdict(); v != domain.VerdictApproved && v != domain.VerdictChangesRequested {
			continue
		}
		answer = r
		found++
	}
	// Proof 5 — exactly one. None means the request is genuinely still open and
	// must keep blocking; more than one means AO cannot say which answered it,
	// and refusing is the only honest response.
	if found != 1 {
		return false, nil
	}

	if err := c.recordIntegrationFreshReview(ctx, run, integrationFreshReviewAnsweredPhase, record, fmt.Sprintf(
		"%s: task %s attempt %d was answered by review %s (verdict %s, target %s) at %s, recorded retroactively because the run never reached integration",
		integrationFreshReviewReconciledDetail, record.TaskID, record.Attempt,
		answer.ID, answer.Verdict, shortFingerprint(answer.TargetSHA),
		answer.CreatedAt.Format(time.RFC3339))); err != nil {
		return false, err
	}
	if c.log != nil {
		c.log.Info("workflow: closed an integration fresh-review request that a review had already answered",
			"run", run.ID, "task", record.TaskID, "attempt", record.Attempt,
			"review", answer.ID, "verdict", answer.Verdict)
	}
	return true, nil
}

// outstandingIntegrationFreshReviewAt is outstandingIntegrationFreshReview plus
// the one fact the reconciliation cannot do without: WHEN the request was made.
//
// Proof 1 is a comparison against that instant, and reading it from the
// checkpoint row rather than from the payload is deliberate — the row's
// created_at is written by the store when the request is durable, so it cannot
// disagree with the ledger it is being compared against.
func (c *Coordinator) outstandingIntegrationFreshReviewAt(
	ctx stdctx.Context,
	childRunID string,
) (IntegrationFreshReviewRecord, time.Time, bool, error) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, childRunID)
	if err != nil {
		return IntegrationFreshReviewRecord{}, time.Time{}, false, err
	}
	var requested, answered IntegrationFreshReviewRecord
	var requestedAt time.Time
	var haveRequest, haveAnswer bool
	for _, cp := range cps {
		var rec IntegrationFreshReviewRecord
		switch cp.DurablePhase {
		case integrationFreshReviewRequiredPhase:
			if decodeIntegrationFreshReviewRecord(cp.RetryState, &rec) {
				requested, requestedAt, haveRequest = rec, cp.CreatedAt, true
			}
		case integrationFreshReviewAnsweredPhase:
			if decodeIntegrationFreshReviewRecord(cp.RetryState, &rec) {
				answered, haveAnswer = rec, true
			}
		}
	}
	if !haveRequest {
		return IntegrationFreshReviewRecord{}, time.Time{}, false, nil
	}
	if haveAnswer && answered.Attempt >= requested.Attempt {
		return IntegrationFreshReviewRecord{}, time.Time{}, false, nil
	}
	return requested, requestedAt, true, nil
}

// decodeIntegrationFreshReviewRecord is the one place the ledger's payload is
// unmarshalled for this file, so a malformed row is skipped rather than read as
// a zero-valued request (whose Attempt of 0 would make any answer look
// sufficient).
func decodeIntegrationFreshReviewRecord(payload string, out *IntegrationFreshReviewRecord) bool {
	return json.Unmarshal([]byte(payload), out) == nil
}

// PendingIntegrationFreshReviewForTest exposes pendingIntegrationFreshReview to
// the external test package. It exists so a test can assert the property that
// actually matters — whether a request is still shadowing the review
// dispatcher — rather than inferring it from the ledger's shape.
func (c *Coordinator) PendingIntegrationFreshReviewForTest(ctx stdctx.Context, runID, reviewStepID string) (VerifyFreshReviewRecord, bool) {
	return c.pendingIntegrationFreshReview(ctx, runID, reviewStepID)
}
