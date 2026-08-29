package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/integration"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// verify_branch_advanced.go — bounded recovery of ONE shape of
// verify_workspace_changed: the branch grew past the reviewed commit.
//
// verify_workspace_changed is not recoverable, and this file does not make it
// so. It answers a single, provable sub-case of it, and refuses everything
// else — including everything it merely cannot prove.
//
// The sub-case: verification finds a workspace that no longer matches the
// approval, and the reason is that the branch the run was authorized to work
// on has COMMITS ON TOP of the commit the approval was given for. Nothing was
// lost: the approved commit is still reachable from the head, so the reviewed
// work is still on the branch, exactly as it was read. What went stale is the
// reviewer's opinion — and the verification — of a tree that has since moved
// on. AO can ask for both again, bounded, instead of parking a person; it is
// the same distinction directBranchFreshness already draws at integration time
// (task_integration_route.go), applied at the verification boundary.
//
// The other sub-case wearing the same shape is an amend, a rebase or a reset:
// the reviewed commit is NOT reachable from the head any more, so the work AO
// certified is not on the branch and no reviewer has read what is. That is a
// fact only a person can act on, and it parks exactly as it always did.
//
// The transition, when every proof below holds:
//
//	verify_workspace_changed
//	  -> the verification is recorded as stale (superseded, never deleted)
//	  -> ONE fresh, independent review of the CURRENT head
//	  -> a fresh verification of what that review approved
//	  -> ordinary integration
//
// Nothing earlier is reused. Not the stale approval (the review step is
// reopened and a NEW review run is dispatched at the live workspace), and not
// the stale verification (the fresh approval yields a different target key, so
// a new attempt row is created and executed).
//
// The six proofs, all required, every one re-derived at decision time from
// durable facts or from Git itself:
//
//  1. the commit the last approving review was given for is still an ANCESTOR
//     of the current head, according to Git, read now;
//  2. branch and worktree are still the ones AO recorded for this run, and the
//     head is the tip of that branch;
//  3. that commit still EXISTS — an amend/reset/rebase that dropped it, or any
//     Git read that fails for any reason, answers "not recoverable";
//  4. the head moved forward inside AO's own authorized workspace, on AO's own
//     authorized branch, as an append — never a rewrite, never another tree,
//     never a detached or switched HEAD;
//  5. nothing of AO's is touching the tree right now: no running work, fix or
//     verify step, no review in flight, no open question, and this run's own
//     agent has been silent long enough that a delivery cannot still be
//     landing;
//  6. there is no recovery already open for this same approved fingerprint,
//     and this run has not used up its bounded number of them.
//
// What AO does NOT claim: it does not claim to know WHO wrote the commits on
// top. It cannot, and it says so. What proofs 2 and 4 establish is narrower and
// checkable — the movement happened where AO was authorized to work, on the
// branch it was authorized to work on, and it added to the reviewed history
// rather than replacing it. That is precisely the movement a fresh review can
// settle. Anything else gets a person.
const (
	// verifyBranchAdvancedPhase is the durable decision: the branch advanced
	// past the approved commit and ONE fresh review of the current head has
	// been authorized. Written BEFORE anything is reopened, so a daemon that
	// dies mid-transition resumes the decision instead of re-deciding it
	// against a branch that may have moved again.
	verifyBranchAdvancedPhase = "verify_branch_advanced_fresh_review"
	// maxBranchAdvancedRecoveries bounds how many times ONE run may answer a
	// workspace change this way. Three: a branch several tasks share can grow
	// under a task more than once, but a branch that grows faster than this
	// task can be reviewed and verified is a scheduling problem a fourth
	// silent re-review does not fix.
	maxBranchAdvancedRecoveries = 3
	// branchAdvancedSettleWindow is how long this run's own agent must have
	// been silent before the tree may be treated as nobody's work in progress.
	// It mirrors humanFixSettleWindow deliberately: the two answer the same
	// question — "could this agent still be about to write here?" — and they
	// must not disagree about how long that takes.
	branchAdvancedSettleWindow = humanFixSettleWindow
)

// branchAdvancedRecord is the durable payload of the decision. Every field is
// something AO observed or proved, so a person reading the ledger can re-check
// the claim rather than take it.
type branchAdvancedRecord struct {
	// Generation counts this run's branch-advance recoveries, from 1. It is
	// what maxBranchAdvancedRecoveries is applied to.
	Generation int `json:"generation"`
	// ApprovedFingerprint is the workspace identity the stale approval was
	// given for, and ApprovedHeadSHA the commit that approval was read at.
	ApprovedFingerprint string `json:"approvedFingerprint"`
	ApprovedHeadSHA     string `json:"approvedHeadSha"`
	// CurrentFingerprint and HeadSHA are the workspace as verification actually
	// found it: the fresh review's starting target, and the head that contains
	// the approved commit.
	CurrentFingerprint string `json:"currentFingerprint"`
	HeadSHA            string `json:"headSha"`
	// TargetKey is the verification target the stale verification was for. The
	// fresh one will have a different key, which is what makes the new attempt
	// a new attempt rather than a re-run of a decided one.
	TargetKey string `json:"targetKey,omitempty"`
	Branch    string `json:"branch,omitempty"`
	// WorktreePath is the AO-authorized worktree the advance happened in.
	WorktreePath string `json:"worktreePath,omitempty"`
	SessionID    string `json:"sessionId,omitempty"`
	ReviewStepID string `json:"reviewStepId,omitempty"`
	// PriorReviewRunID is the review run whose approval went stale. It is what
	// makes the fresh review provably a DIFFERENT run, and it is how this
	// request closes itself once that new run exists.
	PriorReviewRunID string `json:"priorReviewRunId,omitempty"`
	// Attribution states, in words, exactly what AO proved and what it did not.
	Attribution string    `json:"attribution,omitempty"`
	ObservedAt  time.Time `json:"observedAt"`
}

// freshReviewRecord renders the decision in the vocabulary dispatchReviewStep
// already speaks, so a branch advance is served by the SAME dispatch branch as
// a verify recovery, an integration replay and a criterion amendment, rather
// than by a fourth one of its own.
func (r branchAdvancedRecord) freshReviewRecord() VerifyFreshReviewRecord {
	return VerifyFreshReviewRecord{
		Purpose:             freshReviewPurposeBranchAdvance,
		Generation:          r.Generation,
		TargetKey:           r.TargetKey,
		ApprovedFingerprint: r.ApprovedFingerprint,
		CurrentFingerprint:  r.CurrentFingerprint,
		HeadSHA:             r.HeadSHA,
		Branch:              r.Branch,
		WorktreePath:        r.WorktreePath,
		ReviewStepID:        r.ReviewStepID,
		PriorReviewRunID:    r.PriorReviewRunID,
	}
}

// branchAdvancedDecision is the whole predicate, in one place, and its default
// is refusal.
//
// It returns the record to write when a recovery is authorized, and otherwise a
// refusal precise enough to explain in a log or in front of a person. It never
// mutates anything.
func (c *Coordinator) branchAdvancedDecision(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	steps []domain.WorkflowStep,
	reviewStep domain.WorkflowStep,
	workCP domain.WorkflowCheckpoint,
	obs ports.WorkspaceObservation,
	reviewed, currentFingerprint, targetKey string,
) (branchAdvancedRecord, bool, string) {
	refuse := func(format string, args ...any) (branchAdvancedRecord, bool, string) {
		return branchAdvancedRecord{}, false, fmt.Sprintf(format, args...)
	}
	if reviewed == "" || currentFingerprint == "" || reviewed == currentFingerprint {
		return refuse("there is no workspace change to recover from")
	}

	// Proof 2a — the same AO-managed workspace, on the same branch, that this
	// run's own work step recorded. Not "a workspace with the same contents":
	// the same path, the same branch. An unknown branch (a detached HEAD) is
	// ambiguity, and ambiguity refuses.
	if workCP.WorktreePath == "" || (obs.Path != "" && obs.Path != workCP.WorktreePath) {
		return refuse("the verified worktree is no longer this run's own (%q, expected %q)", obs.Path, workCP.WorktreePath)
	}
	if workCP.Branch == "" || obs.Branch == "" || obs.Branch != workCP.Branch {
		return refuse("the worktree is on branch %q, not the branch %q this run was authorized to work on", obs.Branch, workCP.Branch)
	}

	// There has to be a reviewer to re-ask, and an approval to supersede. A
	// review step that reached "completed" through ReviewPolicy's SKIPPED path
	// has no review run and never had a reviewer; reopening it would rest the
	// run at `waiting` forever.
	if c.reviewRuns == nil || c.reviewerLauncher == nil || reviewStep.ReviewRunID == nil {
		return refuse("no reviewer is available to re-review the current workspace")
	}
	priorReview, found, err := c.reviewRuns.GetReviewRun(ctx, *reviewStep.ReviewRunID)
	if err != nil || !found {
		return refuse("AO could not read the review whose approval went stale")
	}
	// Proof 5a — no reviewer is in flight. A review run without a verdict means
	// a reviewer is reading this tree right now, and re-dispatching would be a
	// second reviewer for one question.
	if priorReview.EffectiveVerdict() != domain.VerdictApproved {
		return refuse("the review of this workspace is %q, not an approval that could have gone stale", priorReview.Verdict)
	}

	// Proof 1/3 — the ancestry, from Git, read now.
	//
	// approvedHeadSHA resolves the commit the approving review was actually
	// given for from the review step's own review_target_observed checkpoint,
	// falling back to the work step's completion commit. No durable record of
	// it means AO cannot prove anything about the head at all, which refuses:
	// an unprovable baseline is not evidence of innocence.
	approvedHead := c.approvedHeadSHA(ctx, run.ID, reviewStep.ID, reviewed, workCP)
	if approvedHead == "" {
		return refuse("AO has no durable record of the commit the approved review was given for")
	}
	if obs.HeadSHA == "" {
		return refuse("AO could not read the worktree's current HEAD")
	}
	if obs.HeadSHA == approvedHead {
		// The head did not move, so whatever changed is uncommitted. That is a
		// different question with a different answer (verify_fresh_review.go's
		// authorized-recovery path), and inventing a second route to it here
		// would be a way around that path's own authorization.
		return refuse("the worktree's HEAD did not move, so the change is not a branch advance")
	}
	// Real git, against the repository as it is now, exactly as
	// verifiedWorkIsStillOnBranch does at integration time. A stub that
	// answered these questions would be asserting AO's opinion of the branch
	// rather than the branch.
	git := integration.NewExecGit("")
	// Proof 2b — the head is the TIP of the authorized branch. This is what
	// separates "the branch grew" from "this worktree is sitting on some other
	// commit that happens to contain the approved one".
	branchTip, exists, err := git.ResolveCommitIfExists(ctx, workCP.WorktreePath, workCP.Branch)
	if err != nil {
		return refuse("AO could not read branch %q to prove what the worktree is on: %s", workCP.Branch, err.Error())
	}
	if !exists || branchTip != obs.HeadSHA {
		return refuse("the worktree's HEAD %s is not the tip of branch %q", shortFingerprint(obs.HeadSHA), workCP.Branch)
	}
	// Proof 3 — the approved commit still exists. An amend, a reset or a rebase
	// that dropped it answers here, and so does any read failure.
	if _, exists, err := git.ResolveCommitIfExists(ctx, workCP.WorktreePath, approvedHead); err != nil || !exists {
		return refuse("the commit the review approved (%s) is no longer in the repository, or could not be read", shortFingerprint(approvedHead))
	}
	// Proof 1 — and it is still reachable from the head. A rebase or a reset
	// that rewrote it into a different commit answers here.
	contained, err := git.IsAncestor(ctx, workCP.WorktreePath, approvedHead, obs.HeadSHA)
	if err != nil {
		return refuse("AO could not prove the approved commit %s is still on the branch: %s", shortFingerprint(approvedHead), err.Error())
	}
	if !contained {
		return refuse("the approved commit %s is no longer reachable from HEAD %s — the history was rewritten, and only a person can say what that means",
			shortFingerprint(approvedHead), shortFingerprint(obs.HeadSHA))
	}

	// Proof 5b — nothing of AO's is moving. Every step that can write to this
	// tree must be at rest, and no question may be open on the run.
	for _, s := range steps {
		switch s.Kind {
		case domain.WorkflowStepWork, domain.WorkflowStepFix:
			if s.State == domain.WorkflowStepRunning || s.State == domain.WorkflowStepReady {
				return refuse("this run's %s step is still %s, so the tree may still be changing", s.Kind, s.State)
			}
			attempts, aerr := c.store.ListWorkflowAttempts(ctx, s.ID)
			if aerr != nil {
				return refuse("AO could not read this run's %s attempts to prove nothing is in flight", s.Kind)
			}
			for _, a := range attempts {
				if a.FinishedAt == nil {
					return refuse("this run's %s step has an attempt that never finished, so the tree may still be changing", s.Kind)
				}
			}
		}
	}
	if open, qerr := c.hasOpenQuestion(ctx, run.ID, nil); qerr != nil || open {
		return refuse("this run has an unresolved question open, so nothing about its workspace is settled")
	}
	// Proof 5c — and this run's own agent has been quiet long enough that a
	// delivery cannot still be landing. Not being able to read the session
	// refuses: "AO does not know what its worker is doing" is the ambiguity
	// this proof exists to exclude.
	sessionID := ""
	if workCP.SessionID != nil {
		sessionID = strings.TrimSpace(*workCP.SessionID)
	}
	if c.sessionFacts == nil || sessionID == "" {
		return refuse("AO cannot tell whether this run's own worker is still writing to the tree")
	}
	sess, sfound, err := c.sessionFacts.GetSession(ctx, domain.SessionID(sessionID))
	if err != nil || !sfound {
		return refuse("AO could not read session %s to prove no worker is writing to the tree", sessionID)
	}
	if agentMayStillBeDelivering(sess, c.clock()) {
		return refuse("this run's agent was active within the last %s, so the tree may still be changing", branchAdvancedSettleWindow)
	}

	// Proof 6 — the bound, and one recovery per approved fingerprint. A second
	// request for the same approval would mean the first one's fresh review has
	// not landed yet, and asking twice is exactly what "bounded" excludes.
	generation, already, lerr := c.branchAdvancedState(ctx, run.ID, reviewed)
	if lerr != nil {
		// A read failure must never look like "not yet recorded": that is how
		// one poll's request becomes fifty.
		return refuse("AO could not read this run's recovery ledger")
	}
	if already {
		return refuse("a fresh review of this workspace has already been authorized for the approval of %s", shortFingerprint(reviewed))
	}
	if generation > maxBranchAdvancedRecoveries {
		return refuse("this run has already recovered from a branch advance %d times", maxBranchAdvancedRecoveries)
	}

	return branchAdvancedRecord{
		Generation:          generation,
		ApprovedFingerprint: reviewed,
		ApprovedHeadSHA:     approvedHead,
		CurrentFingerprint:  currentFingerprint,
		HeadSHA:             obs.HeadSHA,
		TargetKey:           targetKey,
		Branch:              workCP.Branch,
		WorktreePath:        workCP.WorktreePath,
		SessionID:           sessionID,
		ReviewStepID:        reviewStep.ID,
		PriorReviewRunID:    priorReview.ID,
		Attribution: fmt.Sprintf(
			"the branch AO was authorized to work on advanced from %s to %s inside AO's own worktree, and still contains the approved commit; AO does not claim to know who wrote the commits on top, only that they added to the reviewed history rather than replacing it",
			shortFingerprint(approvedHead), shortFingerprint(obs.HeadSHA)),
		ObservedAt: c.clock(),
	}, true, ""
}

// branchAdvancedDrift is the predicate as maybeVerify calls it: the same
// decision, with the run's steps read here rather than threaded through the
// verification path for one caller.
func (c *Coordinator) branchAdvancedDrift(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	reviewStep domain.WorkflowStep,
	workCP domain.WorkflowCheckpoint,
	obs ports.WorkspaceObservation,
	reviewed, currentFingerprint, targetKey string,
) (branchAdvancedRecord, bool, string) {
	steps, err := c.store.ListWorkflowSteps(ctx, run.ID)
	if err != nil {
		return branchAdvancedRecord{}, false, "AO could not read this run's steps to prove nothing is writing to the tree"
	}
	return c.branchAdvancedDecision(ctx, run, steps, reviewStep, workCP, obs, reviewed, currentFingerprint, targetKey)
}

// branchAdvancedState folds the ledger into "which recovery would this be, and
// has this exact approval already had one".
func (c *Coordinator) branchAdvancedState(ctx stdctx.Context, runID, approvedFingerprint string) (generation int, already bool, err error) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return 0, false, err
	}
	n := 0
	for _, cp := range cps {
		if cp.DurablePhase != verifyBranchAdvancedPhase {
			continue
		}
		n++
		var rec branchAdvancedRecord
		if json.Unmarshal([]byte(cp.RetryState), &rec) == nil && rec.ApprovedFingerprint == approvedFingerprint {
			return n, true, nil
		}
	}
	return n + 1, false, nil
}

// recordBranchAdvanced writes the decision. Like the other fresh-review
// checkpoints — and unlike an attention stop — this is NOT best-effort: it is
// the only durable record that this recovery exists, and a review reopened
// without it would be an unbounded, unauditable re-review.
func (c *Coordinator) recordBranchAdvanced(ctx stdctx.Context, run domain.WorkflowRun, verifyStep domain.WorkflowStep, rec branchAdvancedRecord) error {
	payload, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	stepID := verifyStep.ID
	_, err = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:                "wfc-" + c.newID(),
		WorkflowRunID:     run.ID,
		WorkflowStepID:    &stepID,
		ProjectID:         run.ProjectID,
		Branch:            rec.Branch,
		WorktreePath:      rec.WorktreePath,
		HeadSHA:           rec.HeadSHA,
		RetryState:        string(payload),
		FingerprintBefore: rec.ApprovedFingerprint,
		FingerprintAfter:  rec.CurrentFingerprint,
		NextAction: fmt.Sprintf(
			"verify_branch_advanced_fresh_review: branch %s advanced from the approved commit %s to %s, which still contains it (recovery %d of %d) — the verification of %s is stale, and one fresh independent review of %s is due",
			rec.Branch, shortFingerprint(rec.ApprovedHeadSHA), shortFingerprint(rec.HeadSHA),
			rec.Generation, maxBranchAdvancedRecoveries,
			shortFingerprint(rec.ApprovedFingerprint), shortFingerprint(rec.CurrentFingerprint)),
		DurablePhase:   verifyBranchAdvancedPhase,
		PayloadVersion: verifyResultVersion,
		CreatedAt:      c.clock(),
	})
	return err
}

// requestBranchAdvancedFreshReview is the live transition, called from
// maybeVerify's workspace-fingerprint guard when the predicate above allows it.
//
// Everything it writes is idempotent against re-entry from any poll, any
// Continue and any restart: the request checkpoint is written once per approved
// fingerprint (the ledger is the guard, inside the predicate), the superseded
// VerifyResult and the attempt outcome close this attempt exactly once, and
// both step transitions are compare-and-swaps on the state they expect.
func (c *Coordinator) requestBranchAdvancedFreshReview(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	reviewStep, verifyStep domain.WorkflowStep,
	attempt domain.WorkflowAttempt,
	result VerifyResult,
	rec branchAdvancedRecord,
) (domain.WorkflowRun, domain.WorkflowStep, error) {
	// The decision, durably, before anything moves.
	if err := c.recordBranchAdvanced(ctx, run, verifyStep, rec); err != nil {
		return run, verifyStep, err
	}

	// This attempt really did fail, and on the class it failed on. Recording it
	// as anything else would be a lie in the ledger. The flag says the failure
	// is a question AO went on to ask, not the answer to it.
	result.SupersededByFreshReview = true
	if err := c.persistVerifyResult(ctx, run, verifyStep, attempt, result, "verify_branch_advanced_fresh_review"); err != nil {
		return run, verifyStep, err
	}
	now := c.clock()
	if err := c.store.UpdateWorkflowAttemptOutcome(ctx, attempt.ID, now, domain.WorkflowAttemptFailed, result.ErrorClass); err != nil {
		return run, verifyStep, err
	}
	if _, err := c.store.ReopenCompletedWorkflowStep(ctx, reviewStep.ID, now); err != nil {
		return run, verifyStep, err
	}
	// The verify step rests at `waiting`, never `failed`: a terminal verify step
	// would make the re-verification this transition exists to enable
	// structurally impossible.
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
	if c.log != nil {
		c.log.Info("workflow: the branch advanced past the reviewed commit; asking for one fresh review of what is there now",
			"run", run.ID, "generation", rec.Generation, "branch", rec.Branch,
			"approvedHead", shortFingerprint(rec.ApprovedHeadSHA), "head", shortFingerprint(rec.HeadSHA))
	}
	return run, verifyStep, nil
}

// resumeBranchAdvancedVerify is the same transition entered from a run that is
// ALREADY parked on a workspace change — one decided and persisted before this
// mechanism existed, or by a pass whose proofs did not hold at the time.
//
// Its only caller is ContinueRun, for the same reason resumeStaleVerifyFailure's
// is: a terminal verify step is taken out of its terminal state here and
// nowhere else, and a 2s Board poll is not a person. The live path above needs
// no such licence because it never touches a terminal step — it acts on the
// verification that is running right now.
//
// Every proof is re-derived against the repository as it stands NOW, not as it
// stood when the run was parked. Returns whether this call reopened anything.
func (c *Coordinator) resumeBranchAdvancedVerify(ctx stdctx.Context, run domain.WorkflowRun, steps []domain.WorkflowStep) (domain.WorkflowRun, bool, error) {
	if run.State != domain.WorkflowRunNeedsAttention {
		return run, false, nil
	}
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
	if verifyStep.State != domain.WorkflowStepFailed || reviewStep.State != domain.WorkflowStepCompleted {
		return run, false, nil
	}
	// The stop must be one of the verification stops. Every human-owned stop is
	// left exactly where it is.
	reason, _, ok := c.stopReason(ctx, run)
	// verify_approved_head_unprovable joins the two attribution stops here on
	// purpose. It names a run that parked because AO could not locate its
	// approval's commit -- and the classification below is re-derived from
	// scratch, against a daemon that can now RECONSTRUCT that commit from the
	// branch's own history (approved_head_recovery.go). A run parked on a fact
	// AO can now prove must be able to move on its own, without a person; the
	// operator recovery exists for the runs where the fact is still unprovable,
	// not for the ones a newer daemon can simply answer.
	if !ok || (!recoverableVerifyStopReasons[reason] &&
		reason != ReasonVerifyWorkspaceUnattributable &&
		reason != ReasonVerifyApprovedHeadUnprovable) {
		return run, false, nil
	}
	// And the failure must actually be a workspace change.
	result, hasResult, err := c.latestVerifyResult(ctx, run.ID)
	if err != nil {
		return run, false, err
	}
	if !hasResult || result.Passed || result.ErrorClass != domain.WorkflowErrorVerifyWorkspaceChanged {
		return run, false, nil
	}

	workCP, hasWorkCP, err := c.store.GetLatestWorkflowCheckpointByStep(ctx, workStep.ID)
	if err != nil {
		return run, false, err
	}
	if !hasWorkCP || workCP.WorktreePath == "" || workCP.SessionID == nil || *workCP.SessionID == "" || c.workspaceFacts == nil {
		return run, false, nil
	}
	obs, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
		Path:      workCP.WorktreePath,
		Branch:    workCP.Branch,
		SessionID: domain.SessionID(*workCP.SessionID),
		ProjectID: domain.ProjectID(run.ProjectID),
	})
	if err != nil {
		// Unreadable is not "unchanged". The run stays where it is.
		return run, false, nil
	}
	current := WorkspaceFingerprint(obs)
	rec, allowed, refusal := c.branchAdvancedDecision(ctx, run, steps, *reviewStep, workCP, obs,
		result.ReviewedFingerprint, current, result.TargetKey)
	if !allowed {
		if c.log != nil {
			c.log.Debug("workflow: a parked workspace change is not a recoverable branch advance",
				"run", run.ID, "reason", refusal)
		}
		return run, false, nil
	}
	if err := c.recordBranchAdvanced(ctx, run, *verifyStep, rec); err != nil {
		return run, false, err
	}
	// applyFreshReviewReopen is the identical, idempotent state mutation the
	// verify-recovery path uses: review out of `completed`, verify out of
	// `failed`, run out of `needs_attention`, resting at `waiting` on a
	// reviewer. Sharing it is what keeps the two mechanisms converging on one
	// resting state instead of two nearly-identical ones.
	run, reopened, err := c.applyFreshReviewReopen(ctx, run, *reviewStep, *verifyStep, rec.freshReviewRecord())
	if err != nil {
		return run, false, err
	}
	if reopened && c.log != nil {
		c.log.Info("workflow: reopened a parked workspace change as a branch advance",
			"run", run.ID, "generation", rec.Generation, "branch", rec.Branch,
			"approvedHead", shortFingerprint(rec.ApprovedHeadSHA), "head", shortFingerprint(rec.HeadSHA))
	}
	return run, reopened, nil
}

// pendingBranchAdvancedFreshReview is the read dispatchReviewStep makes for a
// review step reopened by a branch advance. It is the fourth reason for one
// resting state, served through pendingFreshReview so the dispatcher keeps one
// branch instead of one per reason.
//
// Outstanding means: a request was recorded for this step, and the step still
// points at the approval that went stale. It is therefore self-closing without
// a second ledger — the moment a new review run is linked, the request is
// answered, and no restart, poll or repeated Continue can produce a second.
func (c *Coordinator) pendingBranchAdvancedFreshReview(ctx stdctx.Context, runID, reviewStepID string) (VerifyFreshReviewRecord, bool) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return VerifyFreshReviewRecord{}, false
	}
	var newest *branchAdvancedRecord
	var newestAt time.Time
	for i := range cps {
		cp := &cps[i]
		if cp.DurablePhase != verifyBranchAdvancedPhase {
			continue
		}
		var rec branchAdvancedRecord
		if json.Unmarshal([]byte(cp.RetryState), &rec) != nil {
			continue
		}
		if rec.ReviewStepID != "" && rec.ReviewStepID != reviewStepID {
			continue
		}
		if newest == nil || !cp.CreatedAt.Before(newestAt) {
			copied := rec
			newest, newestAt = &copied, cp.CreatedAt
		}
	}
	if newest == nil {
		return VerifyFreshReviewRecord{}, false
	}
	// Self-closing: once the step points at a review run other than the stale
	// approval, the fresh review has been dispatched and nothing is owed.
	steps, err := c.store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		return VerifyFreshReviewRecord{}, false
	}
	for _, s := range steps {
		if s.ID != reviewStepID || s.ReviewRunID == nil {
			continue
		}
		if *s.ReviewRunID != newest.PriorReviewRunID {
			return VerifyFreshReviewRecord{}, false
		}
	}
	return newest.freshReviewRecord(), true
}
