package workflow

import (
	stdctx "context"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// review_authority_provenance.go — what a review run must PROVE before it may
// (re)take a review step's authority pointer.
//
// THE INCIDENT (wf-0aadfcde, task 4 of wf-2f06ba48). A verification recovery had
// asked for one fresh review, because the approval AO was holding (e7cac283)
// described a fingerprint the workspace no longer had. The fresh review
// (e8fe44e6) was dispatched, stalled, and was cancelled; its replacement
// (f4f02254) was never created. What the ledger then recorded was not a stop:
//
//	workflow_steps  review  review_run_id -> e7cac283      (the STALE approval)
//	review_run      e7cac283  superseded_by = e8fe44e6
//	review_run      e8fe44e6  superseded_by = e7cac283     (a CYCLE)
//
// Authority went back to an approval that had already been superseded, for a
// fingerprint a fresh review had been explicitly authorized to replace — and the
// two runs ended up naming each other as successor, so "which review speaks for
// this step" had no answer a walk of the chain could give. Verification then ran
// against that stale approval's target, failed, and could never be re-asked
// (verify_stalled_attempt.go is the other half of the same incident).
//
// Two facts were never checked, and this file adds exactly those two:
//
//  1. a run that a replacement ALREADY took authority over is history. Binding
//     it again re-arms a decision AO durably recorded as replaced.
//  2. the "predecessor won" restore — recordReviewDispatchSuccess's answer to
//     losing the authority CAS — restored the pointer unconditionally. It is a
//     correct move for exactly one situation (the predecessor produced a late
//     verdict for THIS target while its replacement was launching) and for no
//     other, because the whole point of restoring is to keep that verdict
//     adoptable. A predecessor with no verdict, or with a verdict given for a
//     different target, has nothing to keep adoptable.
//
// Neither refusal may be silent. The claim CAS
// (ClaimWorkflowStepReviewRunIfUnset) refuses any claim while the predecessor
// holds a late verdict, so a refused restore leaves a pointer nothing can ever
// take: quietly returning would be an unbounded retry with no progress, which is
// the same failure mode as the incident it fixes. Both refusals therefore record
// a durable checkpoint and park the run for a person.

// reviewAuthorityStalePhase is the durable refusal: a review run was offered as
// this step's authority and could not take it, with the fact that proves it.
const reviewAuthorityStalePhase = "review_authority_stale"

// supersessionChainLimit bounds the walk of superseded_by links. The chain is
// write-once per row and a replacement is bound one at a time, so a real chain
// is short; the bound exists so a corrupt or cyclic chain terminates the walk
// instead of the walk.
const supersessionChainLimit = 32

// reviewRunSuperseded reports the run that already took authority over this one,
// or "" when nothing has. An unreadable run reports "" together with the error:
// refusing a bind on a read failure would strand dispatch on a transient fault.
func (c *Coordinator) reviewRunSuperseded(ctx stdctx.Context, id string) (string, error) {
	if c.reviewRuns == nil || id == "" {
		return "", nil
	}
	run, ok, err := c.reviewRuns.GetReviewRun(ctx, id)
	if err != nil || !ok {
		return "", err
	}
	return run.SupersededBy, nil
}

// supersessionWouldCycle reports whether naming `replacement` the successor of
// `predecessor` would close a loop — that is, whether `predecessor` is already
// reachable by following `replacement`'s own superseded_by links.
//
// superseded_by is write-once per row, which makes each individual link
// trustworthy and says nothing at all about the graph they form: the storage
// guard only refuses a self-link (`id != superseded_by`). A two-node cycle is
// therefore perfectly writable, and once written "which review replaced which"
// is unanswerable forever, because every walk of the chain returns to where it
// started.
//
// A read failure answers TRUE. Refusing to write a link AO cannot prove is safe
// is the conservative direction: the link is bookkeeping about history, and a
// missing one leaves the authority pointer — the actual decision — untouched.
func (c *Coordinator) supersessionWouldCycle(ctx stdctx.Context, predecessor, replacement string) bool {
	if c.reviewRuns == nil || predecessor == "" || replacement == "" {
		return false
	}
	if predecessor == replacement {
		return true
	}
	seen := map[string]bool{replacement: true}
	at := replacement
	for i := 0; i < supersessionChainLimit; i++ {
		next, err := c.reviewRunSuperseded(ctx, at)
		if err != nil {
			return true
		}
		if next == "" {
			return false
		}
		if next == predecessor {
			return true
		}
		if seen[next] {
			// The chain already loops on itself. Adding a link to it can only
			// make an unanswerable graph worse.
			return true
		}
		seen[next] = true
		at = next
	}
	// Longer than any real chain: treat as unprovable rather than as proven safe.
	return true
}

// predecessorMayRetakeAuthority is the guard on recordReviewDispatchSuccess's
// "the predecessor won" restore.
//
// The restore exists for one situation and is justified only by it: review
// authority reconciliation released the pointer so a replacement could be bound,
// the predecessor then produced a LATE VERDICT (which is what makes the
// replacement's claim CAS fail), and that verdict is adoptable only while the
// step still points at the run that produced it. Restoring keeps a real
// reviewer's real answer usable.
//
// Everything that justification rests on is a fact AO can check, so it checks
// all of them:
//
//	the run is READABLE          — a pointer AO cannot resolve is not provenance
//	it is NOT superseded         — a replaced run's answer is evidence, not authority
//	it HAS an effective verdict  — otherwise it "won" nothing and restoring it
//	                               simply re-parks the step on a silent run
//	its target IS the live one   — a verdict given for a different fingerprint
//	                               certifies work this step is no longer about
//
// The last is the one the incident turned on: the approval being restored had
// been given for a fingerprint a fresh review was explicitly authorized to
// replace. It is compared against the target of the replacement dispatch that
// just lost the bind, which is the target this step is currently asking about.
func (c *Coordinator) predecessorMayRetakeAuthority(
	ctx stdctx.Context, predecessorID string, replacement domain.ReviewRun,
) (bool, string) {
	if c.reviewRuns == nil {
		return false, "AO has no review store to read the replaced review from"
	}
	pred, ok, err := c.reviewRuns.GetReviewRun(ctx, predecessorID)
	if err != nil {
		return false, fmt.Sprintf("the review it replaced (%s) could not be read", predecessorID)
	}
	if !ok {
		return false, fmt.Sprintf("the review it replaced (%s) does not exist", predecessorID)
	}
	if pred.SupersededBy != "" {
		return false, fmt.Sprintf("review run %s was already superseded by %s, so it is history and not authority",
			pred.ID, pred.SupersededBy)
	}
	if !pred.HasEffectiveVerdict() {
		return false, fmt.Sprintf("review run %s produced no verdict, so there is nothing for it to have won", pred.ID)
	}
	if pred.TargetSHA != replacement.TargetSHA {
		return false, fmt.Sprintf(
			"review run %s answered %s for target %s, and this review step is asking about %s",
			pred.ID, pred.EffectiveVerdict(), shortFingerprint(pred.TargetSHA),
			shortFingerprint(replacement.TargetSHA))
	}
	return true, ""
}

// refuseStaleReviewAuthority records one durable refusal and stops the run.
//
// It parks rather than retries on purpose. Every shape that reaches it leaves
// the authority pointer in a state the ordinary bounded dispatch cannot resolve
// by trying again — a superseded run offered as authority, or a released pointer
// the claim CAS will refuse for as long as the predecessor holds its late
// verdict. "Try again next wake" against either of those is the silent loop this
// change exists to end.
func (c *Coordinator) refuseStaleReviewAuthority(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	reviewStep domain.WorkflowStep,
	reviewRunID string,
	reason string,
) (domain.WorkflowStep, error) {
	now := c.clock()
	stepID := reviewStep.ID
	rid := reviewRunID
	detail := "review_authority_stale: " + reason
	// ONE refusal per (step, review run), however many passes re-derive it.
	// Dispatch runs from the read path, so without this the ledger grows a row
	// per Board poll describing one unchanged decision — the wf-c4c84f52 shape.
	// Guarded twice on purpose: the derived id is the storage-level guarantee
	// under concurrency, and the read below is what makes the gate hold for
	// stores whose uniqueness is not the primary key.
	fresh := !c.reviewAuthorityAlreadyRefused(ctx, run.ID, stepID, reviewRunID)
	if fresh {
		_, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
			ID:             reviewAuthorityRefusalID(stepID, reviewRunID),
			WorkflowRunID:  run.ID,
			WorkflowStepID: &stepID,
			ProjectID:      run.ProjectID,
			ReviewRunID:    &rid,
			NextAction:     detail,
			DurablePhase:   reviewAuthorityStalePhase,
			PayloadVersion: "v1",
			RetryState:     "{}",
			CreatedAt:      now,
		})
		if errors.Is(err, domain.ErrDuplicateWorkflowCheckpoint) {
			// Another pass refused it first. One decision, one row.
			fresh = false
		} else if err != nil {
			return reviewStep, err
		}
	}
	if reviewStep.State == domain.WorkflowStepRunning || reviewStep.State == domain.WorkflowStepReady {
		from := reviewStep.State
		if _, err := c.store.UpdateWorkflowStepState(ctx, reviewStep.ID, from, domain.WorkflowStepWaiting, now); err != nil {
			return reviewStep, err
		}
		reviewStep.State = domain.WorkflowStepWaiting
	}
	if run.State == domain.WorkflowRunRunning || run.State == domain.WorkflowRunWaiting {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State,
			domain.WorkflowRunNeedsAttention, now); err != nil {
			return reviewStep, err
		}
		run.State = domain.WorkflowRunNeedsAttention
	}
	if fresh {
		c.recordAttentionStopOnce(ctx, run, &reviewStep.ID, ReasonReviewAuthorityStale, detail)
		if c.log != nil {
			c.log.Warn("workflow: a review run was refused this step's authority",
				"run", run.ID, "step", reviewStep.ID, "reviewRun", reviewRunID, "reason", reason)
		}
	}
	return reviewStep, nil
}

// reviewAuthorityRefused reports whether this review step is resting on a
// refusal AO has already recorded, and so must not be dispatched again by
// itself.
//
// The gate is SELF-CLOSING, which is what keeps it from becoming a permanent
// block nobody can lift: it holds only while the step's authority pointer is
// still released. The moment anything binds a review to this step — a person's
// Continue, a recovery, a replacement that finally wins — the refusal is history
// and the ordinary dispatch resumes with no further bookkeeping.
//
// A read failure reports FALSE. Failing to read the ledger is never a reason to
// stop dispatching work; the refusal will simply be re-derived (and re-gated) by
// recordReviewDispatchSuccess, which is where it is decided in the first place.
func (c *Coordinator) reviewAuthorityRefused(
	ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep,
) bool {
	if reviewStep.ReviewRunID != nil && *reviewStep.ReviewRunID != "" {
		return false
	}
	cps, err := c.store.ListWorkflowCheckpoints(ctx, run.ID)
	if err != nil {
		return false
	}
	for _, cp := range cps {
		if cp.DurablePhase != reviewAuthorityStalePhase {
			continue
		}
		if cp.WorkflowStepID != nil && *cp.WorkflowStepID == reviewStep.ID {
			return true
		}
	}
	return false
}

// reviewAuthorityRefusalID is the derived identity of one refusal: this step,
// this review run. Two passes racing collide on the primary key and one row
// survives; a restart re-deriving the same decision writes nothing new.
func reviewAuthorityRefusalID(stepID, reviewRunID string) string {
	return fmt.Sprintf("wfc-authstale-%s-%s", stepID, reviewRunID)
}

// reviewAuthorityAlreadyRefused reports whether this (step, review run) already
// carries a refusal.
//
// A read failure reports TRUE, which declines to write rather than writing on an
// unreadable ledger — the same direction lateVerdictAlreadyDisposed fails in,
// and for the same reason: the run's own state is what stops it, and a second
// explanation adds nothing a first one did not already say.
func (c *Coordinator) reviewAuthorityAlreadyRefused(
	ctx stdctx.Context, runID, stepID, reviewRunID string,
) bool {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return true
	}
	for _, cp := range cps {
		if cp.DurablePhase != reviewAuthorityStalePhase {
			continue
		}
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID {
			continue
		}
		if cp.ReviewRunID != nil && *cp.ReviewRunID == reviewRunID {
			return true
		}
	}
	return false
}
