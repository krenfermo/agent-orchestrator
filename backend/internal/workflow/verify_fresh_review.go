package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Fresh review inside an authorized verify recovery — Checkpoint 8P-E.14D.
//
// The incident this file exists for is the SECOND half of wf-6528a538, the one
// verify_recovery.go's own recovery uncovered:
//
//	verify_recovery_requested   generation 1   (a person pressed Continue)
//	verify_reopened             generation 1
//	verify_result               passed=false   verify_workspace_changed
//	                            reviewedFingerprint 5f8f9dc4…
//	                            preFingerprint      1b9f3f81…
//
// AO refused to verify, and it was right to: the approval it was holding was
// given for 5f8f9dc4… and the worktree is 1b9f3f81…. Reusing that approval would
// have been AO certifying work no reviewer ever read.
//
// But refusing was also the end of the road, and it should not have been. The
// approval is stale for a reason AO itself caused: AO's verifier was corrected
// and the daemon restarted BETWEEN the approval and the recovery, while the
// worker's uncommitted task changes were deliberately preserved. The honest move
// is neither "trust the old approval" nor "park forever" — it is to go and ask
// the reviewer about the workspace as it ACTUALLY stands, and then verify that.
//
// So this file adds exactly one transition, and reuses the existing review, fix
// and verify machinery for everything that follows it:
//
//	verify (in an authorized recovery) sees pre != reviewed
//	  -> can AO attribute the difference to this same task? (attributable, below)
//	     no  -> needs_attention, ReasonVerifyWorkspaceUnattributable
//	     yes -> verify_fresh_review_required, review step reopened to `waiting`
//	            -> dispatchReviewStep dispatches ONE more cycle at the live target
//	            -> approved         -> verify_fresh_review_approved, verify runs
//	            -> changes_requested-> the ordinary fix/review loop, unchanged
//	            -> launch failed    -> review_launch_recovery.go, unchanged
//
// What this file deliberately does NOT do:
//
//   - It never re-reviews on a generic workspace mismatch. Outside an authorized
//     recovery, verify_workspace_changed is bit-for-bit what it was.
//   - It never absorbs a change it cannot attribute. Branch, HEAD and worktree
//     path must all be provably unchanged since the approval; anything else is a
//     person's edit or a merge, and gets a person.
//   - It never redispatches the worker. The work step is not the thing that went
//     stale; the approval is.
//   - It never runs twice. One fresh review per recovery generation, bounded by
//     the same append-only-checkpoint counting every other bound here uses.

const (
	// verifyFreshReviewRequiredPhase is the durable decision: this recovery
	// generation's approval no longer describes the workspace, AO attributed the
	// difference to this task's own uncommitted work, and one fresh review has
	// been authorized. Written BEFORE the review step is reopened, so a daemon
	// that dies mid-transition resumes the same decision rather than re-deciding
	// it against a workspace that may have moved again.
	verifyFreshReviewRequiredPhase = "verify_fresh_review_required"
	// verifyFreshReviewApprovedPhase is the durable answer: the fresh review
	// approved a fingerprint, and it is the target this generation's verification
	// is now authorized for. Split from the request for the same reason
	// verify_reopened is split from verify_recovery_requested — so the ledger can
	// tell "asked" from "answered" without comparing timestamps.
	verifyFreshReviewApprovedPhase = "verify_fresh_review_approved"
	// maxFreshReviewsPerRecovery bounds how many times ONE recovery generation may
	// re-ask the reviewer. One. A second would mean the workspace moved again
	// during the fresh review itself, which is no longer "AO's own upgrade left
	// the approval behind" — it is a workspace nobody can hold still, and that is
	// a person's problem, not a retry.
	maxFreshReviewsPerRecovery = 1
)

// VerifyFreshReviewRecord is the durable payload of both fresh-review
// checkpoints. It names the generation it belongs to and pins every fact the
// decision was made from, so a restart re-reads the decision instead of
// re-deriving it from a workspace that has since moved.
type VerifyFreshReviewRecord struct {
	// Generation is the verify recovery generation this fresh review serves. It
	// is the join key for the whole lifecycle: request, dispatch and approval all
	// carry it, and the ledger answers every "already done?" question with it.
	Generation int `json:"generation"`
	// TargetKey is the verification target key (fingerprint + verification plan)
	// the recovery was authorized for. A fresh review is only ever consumed by a
	// verification whose recovery record carries this same key, which is what
	// stops one task's approval from being reused for another's target.
	TargetKey string `json:"targetKey,omitempty"`
	// ApprovedFingerprint is F1: the reviewed fingerprint AO was holding and
	// refused to reuse. Preserved verbatim — the historical approval is never
	// rewritten, only superseded.
	ApprovedFingerprint string `json:"approvedFingerprint,omitempty"`
	// CurrentFingerprint is F2: the workspace as verification actually found it.
	// It is the fresh review's starting target; dispatch re-observes and pins the
	// live value per cycle exactly as every other review cycle does.
	CurrentFingerprint string `json:"currentFingerprint,omitempty"`
	// HeadSHA, Branch and WorktreePath are the workspace identity that had to be
	// unchanged for the difference to be attributable at all. Recorded so a
	// person reading the ledger can see precisely what AO checked.
	HeadSHA      string `json:"headSha,omitempty"`
	Branch       string `json:"branch,omitempty"`
	WorktreePath string `json:"worktreePath,omitempty"`
	// ReviewStepID is the step reopened for the fresh cycle.
	ReviewStepID string `json:"reviewStepId,omitempty"`
	// PriorReviewRunID is the review_run whose approval went stale. Kept so the
	// fresh review is provably a DIFFERENT run, never an edit of the old one.
	PriorReviewRunID string `json:"priorReviewRunId,omitempty"`

	// The two fields below are set only on the approval checkpoint.
	//
	// ReviewRunID is the fresh review's own run, and ReviewedFingerprint the
	// fingerprint it actually approved — which is what verification then runs
	// against, and what the resulting VerifyResult records as its reviewed target.
	ReviewRunID         string `json:"reviewRunId,omitempty"`
	ReviewedFingerprint string `json:"reviewedFingerprint,omitempty"`
}

// freshReviewDecision is the outcome of the safety predicate: either a fresh
// review is authorized, or it is refused with a reason precise enough to put in
// front of a person.
type freshReviewDecision struct {
	allowed bool
	// refusal is the human-readable "why not", empty when allowed. It becomes the
	// attention stop's detail verbatim.
	refusal string
}

// attributableWorkspaceDrift is the whole safety predicate, in one place.
//
// It answers a single question: is the difference between the approval AO holds
// and the workspace in front of it attributable to THIS task's own uncommitted
// work inside an authorized recovery — or is it something AO has no business
// absorbing?
//
// Every condition is a durable fact, and every one of them is required. There is
// deliberately no "probably fine" branch: the default is refusal, because the
// cost of a wrong yes is AO certifying code a reviewer approved a different
// version of.
func (c *Coordinator) attributableWorkspaceDrift(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	reviewStep domain.WorkflowStep,
	recovery VerifyRecoveryRecord,
	led verifyRecoveryLedger,
	targetKey string,
	workCP domain.WorkflowCheckpoint,
	obs ports.WorkspaceObservation,
) freshReviewDecision {
	// 1. An explicitly authorized recovery, for a failure class that was AO's own
	//    infrastructure. resumeStaleVerifyFailure already proved both before it
	//    ever wrote the generation; re-reading the recorded class here means this
	//    predicate stands on its own rather than on its caller's memory.
	if recovery.Generation == 0 || !recoverableVerifyErrorClass(recovery.ErrorClass) {
		return freshReviewDecision{refusal: "the workspace changed outside an authorized verification recovery"}
	}

	// 2. Same task, same verification plan. targetKey is a hash of the reviewed
	//    fingerprint AND the plan, so this catches a plan edit as surely as it
	//    catches a target swap — and the recovery record's key was written from
	//    the failure that was reopened, not from anything observed since.
	if recovery.TargetKey == "" || recovery.TargetKey != targetKey {
		return freshReviewDecision{refusal: fmt.Sprintf(
			"the verification target key changed since the recovery was authorized (%s -> %s)",
			shortFingerprint(recovery.TargetKey), shortFingerprint(targetKey))}
	}

	// 3. One fresh review per generation. Reaching here a second time means the
	//    workspace also moved during the fresh review, which is not the condition
	//    this transition exists for.
	if led.freshReviewRequested {
		return freshReviewDecision{refusal: fmt.Sprintf(
			"the workspace changed again after this recovery already obtained a fresh review of %s",
			shortFingerprint(led.freshReview.CurrentFingerprint))}
	}

	// 4. There has to be a reviewer to re-ask. A review step that reached
	//    "completed" through ReviewPolicy's SKIPPED path has no review_run and
	//    never had a reviewer; reopening it would rest the run at `waiting`
	//    forever, since dispatchReviewStep has nothing to dispatch.
	if reviewStep.ReviewRunID == nil || c.reviewRuns == nil || c.reviewerLauncher == nil {
		return freshReviewDecision{refusal: "no reviewer is available to re-review the current workspace"}
	}

	// 5. The same AO-managed task workspace, still. Not "a workspace with the
	//    same contents" — the same path, on the same branch, that the work step
	//    itself recorded.
	if workCP.WorktreePath == "" || (obs.Path != "" && obs.Path != workCP.WorktreePath) {
		return freshReviewDecision{refusal: fmt.Sprintf(
			"the verified worktree is no longer this run's own (%q, expected %q)", obs.Path, workCP.WorktreePath)}
	}
	if workCP.Branch != "" && obs.Branch != "" && obs.Branch != workCP.Branch {
		return freshReviewDecision{refusal: fmt.Sprintf(
			"the worktree is on branch %q, not the branch %q this run worked on", obs.Branch, workCP.Branch)}
	}

	// 6. The commit history did not move. This is the condition that separates
	//    "the worker's uncommitted task changes, preserved across AO's own
	//    upgrade" from everything else. Workers are told never to commit, so the
	//    ONLY drift this recovery is willing to absorb is uncommitted
	//    working-tree drift at an unchanged HEAD. A new commit, a pull, a rebase
	//    or a branch switch all move HEAD, and all of them are somebody else's
	//    edit arriving in this worktree — a person decides what to do about that,
	//    not AO.
	//
	//    A HEAD AO cannot prove is treated exactly like a HEAD that moved. An
	//    unprovable baseline is not evidence of innocence.
	approvedHead := c.approvedHeadSHA(ctx, run.ID, reviewStep.ID, recovery.ReviewedFingerprint, workCP)
	if approvedHead == "" {
		return freshReviewDecision{refusal: "AO has no durable record of the commit the approved review was given for"}
	}
	if obs.HeadSHA == "" || obs.HeadSHA != approvedHead {
		return freshReviewDecision{refusal: fmt.Sprintf(
			"the worktree's HEAD moved since review approved it (%s -> %s), so the change is not this task's uncommitted work",
			shortFingerprint(approvedHead), shortFingerprint(obs.HeadSHA))}
	}

	return freshReviewDecision{allowed: true}
}

// approvedHeadSHA resolves the commit the approved review was actually given
// for.
//
// First choice is the review step's own review_target_observed checkpoint for
// that exact fingerprint — the record dispatchReviewStep writes naming what the
// reviewer was about to read. Second is the work step's completion checkpoint,
// which is the same commit whenever review followed work without the repository
// moving in between. Returns "" when neither exists, which the predicate above
// treats as a refusal.
func (c *Coordinator) approvedHeadSHA(ctx stdctx.Context, runID, reviewStepID, reviewedFingerprint string, workCP domain.WorkflowCheckpoint) string {
	if reviewedFingerprint != "" {
		if cps, err := c.store.ListWorkflowCheckpoints(ctx, runID); err == nil {
			for _, cp := range cps {
				if cp.DurablePhase != reviewTargetDurablePhase || cp.HeadSHA == "" {
					continue
				}
				if cp.WorkflowStepID == nil || *cp.WorkflowStepID != reviewStepID {
					continue
				}
				if cp.FingerprintAfter == reviewedFingerprint {
					return cp.HeadSHA
				}
			}
		}
	}
	return workCP.HeadSHA
}

// requestFreshReviewForRecovery is the transition itself: it turns a refused
// verification into an authorized, bounded re-review of the current workspace.
//
// It is called from exactly one place — maybeVerify's workspace-fingerprint
// guard, and only when a recovery generation is open. Everything it writes is
// idempotent against re-entry from any poll, any Continue and any restart:
//
//   - the request checkpoint is written once per generation (the ledger's own
//     freshReviewRequested is the guard, checked inside the predicate);
//   - the superseded VerifyResult and the attempt outcome are this attempt's own
//     honest record, written exactly once because the attempt is closed by them;
//   - the review reopen is a compare-and-swap on state='completed';
//   - the verify step and run transitions are compare-and-swaps too.
//
// It returns the run and verify step as they now stand. The caller returns
// immediately afterwards: the next cascade pass dispatches the review.
func (c *Coordinator) requestFreshReviewForRecovery(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	reviewStep, verifyStep domain.WorkflowStep,
	attempt domain.WorkflowAttempt,
	result VerifyResult,
	recovery VerifyRecoveryRecord,
	workCP domain.WorkflowCheckpoint,
	obs ports.WorkspaceObservation,
) (domain.WorkflowRun, domain.WorkflowStep, error) {
	record := VerifyFreshReviewRecord{
		Generation:          recovery.Generation,
		TargetKey:           result.TargetKey,
		ApprovedFingerprint: result.ReviewedFingerprint,
		CurrentFingerprint:  result.PreFingerprint,
		HeadSHA:             obs.HeadSHA,
		Branch:              workCP.Branch,
		WorktreePath:        workCP.WorktreePath,
		ReviewStepID:        reviewStep.ID,
	}
	if reviewStep.ReviewRunID != nil {
		record.PriorReviewRunID = *reviewStep.ReviewRunID
	}

	// The decision, durably, first. Not best-effort: a review reopened without a
	// record of why would be an unbounded, unauditable re-review.
	if err := c.recordFreshReview(ctx, run, verifyStep, verifyFreshReviewRequiredPhase, record, fmt.Sprintf(
		"verify: recovery generation %d holds an approval for %s but the workspace is %s; re-reviewing the current workspace once",
		record.Generation, shortFingerprint(record.ApprovedFingerprint), shortFingerprint(record.CurrentFingerprint),
	)); err != nil {
		return run, verifyStep, err
	}

	// This attempt really did fail, and on the class it failed on. Recording it
	// as anything else would be a lie in the ledger. The flag is what tells the
	// recovery ledger this failure is a question, not the generation's answer.
	result.SupersededByFreshReview = true
	if err := c.persistVerifyResult(ctx, run, verifyStep, attempt, result, "verify_fresh_review_required"); err != nil {
		return run, verifyStep, err
	}
	now := c.clock()
	if err := c.store.UpdateWorkflowAttemptOutcome(ctx, attempt.ID, now, domain.WorkflowAttemptFailed, result.ErrorClass); err != nil {
		return run, verifyStep, err
	}

	// Reopen the review step. `waiting` is the resting state every review cycle
	// already dispatches from, so no new dispatch path is needed — only a new
	// reason to be in it (see dispatchReviewStep's fresh-review branch).
	if _, err := c.store.ReopenCompletedWorkflowStep(ctx, reviewStep.ID, now); err != nil {
		return run, verifyStep, err
	}
	// The verify step rests at `waiting`, never `failed`: a terminal verify step
	// would make the re-verification this transition exists to enable
	// structurally impossible — the same reasoning finishVerifyFailure's fix
	// re-entry branch already follows.
	if verifyStep.State == domain.WorkflowStepRunning {
		if _, err := c.store.UpdateWorkflowStepState(ctx, verifyStep.ID, domain.WorkflowStepRunning, domain.WorkflowStepWaiting, now); err != nil {
			return run, verifyStep, err
		}
		verifyStep.State = domain.WorkflowStepWaiting
	}
	if run.State == domain.WorkflowRunRunning {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, domain.WorkflowRunRunning, domain.WorkflowRunWaiting, now); err != nil {
			return run, verifyStep, err
		}
		run.State = domain.WorkflowRunWaiting
	}
	return run, verifyStep, nil
}

// pendingFreshReview returns the fresh review this run still owes a reviewer:
// one authorized for the currently open recovery generation, not yet answered.
// dispatchReviewStep consults it to know that a review step resting at `waiting`
// with an APPROVED review_run underneath it is nonetheless due for one more
// cycle — a resting state no other branch there can recognize.
func (c *Coordinator) pendingFreshReview(ctx stdctx.Context, runID, reviewStepID string) (VerifyFreshReviewRecord, bool) {
	led, err := c.verifyRecoveryLedger(ctx, runID)
	if err != nil || led.generation == 0 || led.executed {
		return VerifyFreshReviewRecord{}, false
	}
	if !led.freshReviewRequested || led.freshReviewApproved {
		return VerifyFreshReviewRecord{}, false
	}
	if led.freshReview.ReviewStepID != "" && led.freshReview.ReviewStepID != reviewStepID {
		return VerifyFreshReviewRecord{}, false
	}
	return led.freshReview, true
}

// authorizeFreshReviewTarget is the read maybeVerify's recovery guard makes when
// the approved fingerprint it found is NOT the one the recovery was authorized
// for. Exactly one thing makes that legitimate: AO itself asked for this review,
// for this generation, against this same verification target.
//
// On success it records the approval once — the durable statement of "generation
// N is now verifying F2, which review approved, instead of F1, which it did" —
// and reports the record. On failure the caller refuses the verification, which
// is the pre-existing behaviour for every other way a target can move.
func (c *Coordinator) authorizeFreshReviewTarget(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	reviewStep, verifyStep domain.WorkflowStep,
	recovery VerifyRecoveryRecord,
	reviewed, targetKey string,
) (VerifyFreshReviewRecord, bool, error) {
	led, err := c.verifyRecoveryLedger(ctx, run.ID)
	if err != nil {
		return VerifyFreshReviewRecord{}, false, err
	}
	if !led.freshReviewRequested || led.freshReview.Generation != recovery.Generation {
		return VerifyFreshReviewRecord{}, false, nil
	}
	// The fresh review was authorized for one verification target and one review
	// step. A verification of anything else does not get to consume it.
	if led.freshReview.TargetKey != recovery.TargetKey ||
		led.freshReview.ApprovedFingerprint != recovery.ReviewedFingerprint ||
		(led.freshReview.ReviewStepID != "" && led.freshReview.ReviewStepID != reviewStep.ID) {
		return VerifyFreshReviewRecord{}, false, nil
	}
	if led.freshReviewApproved {
		// Already recorded. Idempotent by identity, not by timestamp: the same
		// approved fingerprint resolves the same way on every poll and restart, a
		// different one is a target that moved again and is refused.
		if led.freshApproval.ReviewedFingerprint != reviewed {
			return VerifyFreshReviewRecord{}, false, nil
		}
		return led.freshApproval, true, nil
	}

	approval := led.freshReview
	approval.ReviewedFingerprint = reviewed
	approval.TargetKey = targetKey
	if reviewStep.ReviewRunID != nil {
		approval.ReviewRunID = *reviewStep.ReviewRunID
	}
	if approval.ReviewRunID != "" && approval.ReviewRunID == approval.PriorReviewRunID {
		// The step still points at the approval that went stale, so no fresh
		// review has actually landed yet. Nothing to authorize.
		return VerifyFreshReviewRecord{}, false, nil
	}
	if err := c.recordFreshReview(ctx, run, verifyStep, verifyFreshReviewApprovedPhase, approval, fmt.Sprintf(
		"verify: recovery generation %d verifies %s, freshly approved by review %s (superseding the stale approval of %s)",
		approval.Generation, shortFingerprint(reviewed), approval.ReviewRunID, shortFingerprint(approval.ApprovedFingerprint),
	)); err != nil {
		return VerifyFreshReviewRecord{}, false, err
	}
	return approval, true, nil
}

// recordFreshReview writes one fresh-review checkpoint. Like
// recordVerifyRecovery — and unlike recordAttentionStop — it is NOT best-effort:
// these two rows are the entire durable lifecycle of this transition, and a
// review reopened or an approval consumed without one would be exactly the kind
// of untracked state this mechanism exists to avoid.
func (c *Coordinator) recordFreshReview(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, phase string, record VerifyFreshReviewRecord, detail string) error {
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	stepID := step.ID
	_, err = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:                "wfc-" + c.newID(),
		WorkflowRunID:     run.ID,
		WorkflowStepID:    &stepID,
		ProjectID:         run.ProjectID,
		Branch:            record.Branch,
		WorktreePath:      record.WorktreePath,
		HeadSHA:           record.HeadSHA,
		RetryState:        string(payload),
		FingerprintBefore: record.ApprovedFingerprint,
		FingerprintAfter:  record.CurrentFingerprint,
		NextAction:        detail,
		DurablePhase:      phase,
		PayloadVersion:    verifyResultVersion,
		CreatedAt:         c.clock(),
	})
	return err
}

// shortFingerprint renders a fingerprint/SHA for a human-readable detail line
// without dropping the fact that it is a hash. Empty stays empty rather than
// becoming a misleading "".
func shortFingerprint(v string) string {
	if v == "" {
		return "(none)"
	}
	if len(v) <= 12 {
		return v
	}
	return v[:12] + "…"
}
