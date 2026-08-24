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

// resumeWorkspaceChangedVerifyRecovery is the historical half of 8P-E.14D: the
// same transition, entered from a run whose workspace mismatch was already
// DECIDED and persisted by a daemon that predates the transition.
//
// wf-6528a538 is one state further along than requestFreshReviewForRecovery can
// reach. Its generation-1 recovery ran under the old binary, discovered the
// mismatch, and recorded the only ending that binary had:
//
//	verify step    failed          (terminal)
//	run            needs_attention  attentionReason = verify_unrepairable
//	verify_result  passed=false     verify_workspace_changed
//	               recoveryGeneration = 1
//	               reviewedFingerprint 5f8f9dc4…  preFingerprint 1b9f3f81…
//
// Installing the new code changed nothing for it, and POST /continue returned
// 200 having done precisely nothing. The reason is a single line in
// resumeStaleVerifyFailure's guard 2: it reads the run's newest VerifyResult and
// requires recoverableVerifyErrorClass(result.ErrorClass), which is true only for
// verify_environment_error and verify_ambiguous. The newest result is now the
// generation-1 workspace_changed one, so the guard returns false and every later
// step — the reopen, the cascade, maybeVerify's own drift branch — is never
// reached. (maybeVerify could not have helped anyway: it returns immediately on
// a terminal verify step, and the cascade only calls it for a review step that
// is `completed`, which this one still is.)
//
// This function is the narrow, evidence-only migration of that state into the
// SAME machinery. It never widens guard 2, and it never makes
// verify_workspace_changed recoverable in general: everything it does is gated
// on durable proof that this particular mismatch happened INSIDE an authorized
// recovery generation that originated from an eligible infrastructure failure,
// plus the identical attributableWorkspaceDrift predicate re-evaluated against
// the workspace as it stands right now.
//
// Like resumeStaleVerifyFailure, its only caller is ContinueRun. Returns the
// (possibly updated) run and whether the fresh review was opened by this call.
func (c *Coordinator) resumeWorkspaceChangedVerifyRecovery(ctx stdctx.Context, run domain.WorkflowRun, steps []domain.WorkflowStep) (domain.WorkflowRun, bool, error) {
	var workStep, reviewStep, verifyStep *domain.WorkflowStep
	for i := range steps {
		switch steps[i].Kind {
		case domain.WorkflowStepWork:
			workStep = &steps[i]
		case domain.WorkflowStepReview:
			reviewStep = &steps[i]
		case domain.WorkflowStepVerify:
			verifyStep = &steps[i]
		}
	}
	if workStep == nil || reviewStep == nil || verifyStep == nil {
		return run, false, nil
	}

	led, err := c.verifyRecoveryLedger(ctx, run.ID)
	if err != nil {
		return run, false, err
	}
	// Proof 1 — an authorized recovery generation exists at all, and was applied.
	// A run that has never been reopened by a person has no lineage this
	// transition could belong to.
	if led.generation == 0 || !led.reopened {
		return run, false, nil
	}

	// Resume path: the request is already durable but its state mutation did not
	// finish (a daemon that died between the two, or a Continue that raced one).
	// Re-applying is pure compare-and-swap, so it costs nothing when the
	// mutation did in fact complete, and it is the only thing that keeps a crash
	// in that window from being a permanent dead end — the decision has been
	// made and must not be re-decided against a workspace that may have moved.
	if led.freshReviewRequested {
		if led.freshReviewApproved {
			return run, false, nil
		}
		return c.applyFreshReviewReopen(ctx, run, *reviewStep, *verifyStep, led.freshReview)
	}

	// From here this is a first-time decision, and every proof must hold.
	if run.State != domain.WorkflowRunNeedsAttention || verifyStep.State != domain.WorkflowStepFailed {
		return run, false, nil
	}

	// Proof 2 — the stop is one this transition is allowed to reopen. The
	// historical rows say verify_unrepairable (the flat reason the old
	// finishVerifyFailure recorded for a workspace change); the new code's own
	// refusal says verify_workspace_unattributable, and that is included on
	// purpose — a person who has since restored the branch or the commit and
	// presses Continue is entitled to have the predicate re-evaluated rather
	// than being held to a verdict about a worktree that no longer exists. Every
	// other stop, including every human-owned one, is left exactly where it is.
	reason, _, ok := c.stopReason(ctx, run)
	if !ok || (!recoverableVerifyStopReasons[reason] && reason != ReasonVerifyWorkspaceUnattributable) {
		return run, false, nil
	}

	// Proof 3 — the failure actually is a workspace change produced BY a recovery
	// generation. RecoveryGeneration is stamped by persistVerifyResult at the
	// moment the result is written, so it cannot be back-dated onto an ordinary
	// verification.
	result, hasResult, err := c.latestVerifyResult(ctx, run.ID)
	if err != nil {
		return run, false, err
	}
	if !hasResult || result.Passed ||
		result.ErrorClass != domain.WorkflowErrorVerifyWorkspaceChanged ||
		result.RecoveryGeneration == 0 {
		return run, false, nil
	}

	// Proof 4 — it is THIS generation's result, and the generation it belongs to
	// originated from an eligible infrastructure/environment failure, on the same
	// task target. Both halves of the identity are checked, not one: the
	// generation record's target is what a person authorized, and the result's is
	// what actually ran.
	if result.RecoveryGeneration != led.generation ||
		!recoverableVerifyErrorClass(led.record.ErrorClass) ||
		led.record.TargetKey == "" || led.record.TargetKey != result.TargetKey ||
		led.record.ReviewedFingerprint != result.ReviewedFingerprint {
		return run, false, nil
	}

	// Proof 5/6 — the same attributableWorkspaceDrift predicate as the live path,
	// re-evaluated against the workspace as it stands NOW, not as it stood when
	// the old binary gave up on it.
	workCP, hasWorkCP, err := c.store.GetLatestWorkflowCheckpointByStep(ctx, workStep.ID)
	if err != nil {
		return run, false, err
	}
	if !hasWorkCP || workCP.WorktreePath == "" || workCP.SessionID == nil || *workCP.SessionID == "" ||
		c.workspaceFacts == nil {
		c.recordAttentionStopOnce(ctx, run, &verifyStep.ID, ReasonVerifyWorkspaceUnattributable,
			"verification was reopened, but AO has no workspace facts for this run to compare the approval against")
		return run, false, nil
	}
	artifact, err := c.planArtifactForRun(ctx, run)
	if err != nil {
		return run, false, err
	}
	obs, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
		Path:      workCP.WorktreePath,
		Branch:    workCP.Branch,
		SessionID: domain.SessionID(*workCP.SessionID),
		ProjectID: domain.ProjectID(run.ProjectID),
	})
	if err != nil {
		// Unreadable is not "unchanged". Leave the run stopped, say why, and let
		// the next Continue try again once the host answers.
		c.recordAttentionStopOnce(ctx, run, &verifyStep.ID, ReasonVerifyWorkspaceUnattributable,
			"verification was reopened, but the worktree could not be observed: "+err.Error())
		return run, false, nil
	}
	current := WorkspaceFingerprint(obs)
	if current == result.ReviewedFingerprint {
		// No drift left to re-review — the worktree matches the approval again.
		// Deliberately NOT handled here: this transition exists to obtain a
		// review of a CHANGED workspace, and inventing a re-verification for an
		// unchanged one would be a second, unasked-for recovery path.
		c.recordAttentionStopOnce(ctx, run, &verifyStep.ID, ReasonVerifyWorkspaceUnattributable,
			"the worktree now matches the approved review again; there is nothing to re-review, and the previous verification's verdict still stands")
		return run, false, nil
	}
	targetKey := verificationTargetKey(result.ReviewedFingerprint, artifact.Verification)
	recovery := led.record
	decision := c.attributableWorkspaceDrift(ctx, run, *reviewStep, recovery, led, targetKey, workCP, obs)
	if !decision.allowed {
		c.recordAttentionStopOnce(ctx, run, &verifyStep.ID, ReasonVerifyWorkspaceUnattributable, decision.refusal)
		return run, false, nil
	}

	// Proof 7 (the bound) is attributableWorkspaceDrift's own check on
	// led.freshReviewRequested, already false to have reached here.
	record := VerifyFreshReviewRecord{
		Generation:          led.generation,
		TargetKey:           targetKey,
		ApprovedFingerprint: result.ReviewedFingerprint,
		CurrentFingerprint:  current,
		HeadSHA:             obs.HeadSHA,
		Branch:              workCP.Branch,
		WorktreePath:        workCP.WorktreePath,
		ReviewStepID:        reviewStep.ID,
	}
	if reviewStep.ReviewRunID != nil {
		record.PriorReviewRunID = *reviewStep.ReviewRunID
	}
	// Durable before mutation, exactly as the live path does — a review reopened
	// without this row would be an unbounded, unauditable re-review.
	if err := c.recordFreshReview(ctx, run, *verifyStep, verifyFreshReviewRequiredPhase, record, fmt.Sprintf(
		"verify: recovery generation %d recorded a workspace change under an older daemon; re-reviewing the current workspace %s once, in place of the stale approval of %s",
		record.Generation, shortFingerprint(record.CurrentFingerprint), shortFingerprint(record.ApprovedFingerprint),
	)); err != nil {
		return run, false, err
	}
	return c.applyFreshReviewReopen(ctx, run, *reviewStep, *verifyStep, record)
}

// applyFreshReviewReopen is the idempotent state mutation both fresh-review
// entry points share: review out of `completed`, verify out of `failed`, run out
// of `needs_attention`, and finally to rest at `waiting` on a reviewer.
//
// Every write is a compare-and-swap on the exact state it expects, so
// re-entering this from a repeated Continue, a poll or a restart in any order
// converges on the same place and can never produce a second of anything.
func (c *Coordinator) applyFreshReviewReopen(ctx stdctx.Context, run domain.WorkflowRun, reviewStep, verifyStep domain.WorkflowStep, record VerifyFreshReviewRecord) (domain.WorkflowRun, bool, error) {
	now := c.clock()
	if _, err := c.store.ReopenCompletedWorkflowStep(ctx, reviewStep.ID, now); err != nil {
		return run, false, err
	}
	// The verify step goes back to `ready`, not `waiting`: `failed` is where the
	// old binary left it, and ReopenFailedWorkflowStep is the one write in AO
	// that leaves that state. maybeVerify accepts either, and will not run at all
	// until the review step is `completed` again.
	if _, err := c.store.ReopenFailedWorkflowStep(ctx, verifyStep.ID, now); err != nil {
		return run, false, err
	}
	if moved, err := c.store.UpdateWorkflowRunState(ctx, run.ID,
		domain.WorkflowRunNeedsAttention, domain.WorkflowRunRunning, now); err != nil {
		return run, false, err
	} else if moved {
		run.State = domain.WorkflowRunRunning
	}
	if run.State == domain.WorkflowRunRunning {
		if moved, err := c.store.UpdateWorkflowRunState(ctx, run.ID,
			domain.WorkflowRunRunning, domain.WorkflowRunWaiting, now); err != nil {
			return run, false, err
		} else if moved {
			run.State = domain.WorkflowRunWaiting
		}
	}
	return run, true, nil
}

// pendingFreshReview returns the fresh review this run still owes a reviewer:
// one authorized for the currently open recovery generation, not yet answered.
// dispatchReviewStep consults it to know that a review step resting at `waiting`
// with an APPROVED review_run underneath it is nonetheless due for one more
// cycle — a resting state no other branch there can recognize.
func (c *Coordinator) pendingFreshReview(ctx stdctx.Context, runID, reviewStepID string) (VerifyFreshReviewRecord, bool) {
	led, err := c.verifyRecoveryLedger(ctx, runID)
	if err != nil || led.generation == 0 || led.executed {
		// No verify recovery is asking for one. An INTEGRATION may be: a replay
		// onto a moved target can leave a task's approval describing a change
		// that is no longer the change (task_integration_fresh_review.go). The
		// two are different reasons for the same resting state, and the
		// dispatcher below serves both identically — which is the point of
		// routing the second through this read rather than through a second
		// branch of its own.
		if rec, ok := c.pendingIntegrationFreshReview(ctx, runID, reviewStepID); ok {
			return rec, true
		}
		// And an AMENDMENT may be: a criterion that stopped describing reality
		// was changed by a person, so the verdict reached under it no longer
		// counts (task_criterion_amendment.go). A third reason for the same
		// resting state, served here for the same reason the second is —
		// the dispatcher should not grow a branch per reason.
		return c.pendingAmendmentFreshReview(ctx, runID, reviewStepID)
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
