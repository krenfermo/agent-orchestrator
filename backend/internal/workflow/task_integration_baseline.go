package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// task_integration_baseline.go — what "the verified commit" means after a
// branch has moved.
//
// directBranchFreshness asks whether the branch tip is still the thing that was
// verified, and it answered with the commit the task's own WORK produced
// (autonomous_local_commit). That is the right answer exactly once: at the
// moment the work was verified and nothing had touched the branch since.
//
// It stops being the right answer the moment anything else lands. A branch that
// several tasks share, or that a person commits to, moves past the task's own
// commit, and from then on the comparison fails forever — no matter how many
// fresh reviews and verifications pass, because none of them moves that commit.
// wf-04e8309d proved it: a fresh review approved HEAD 3260247c4 and a fresh
// verification passed against it, and integration still compared against
// cdfde8dd9 and called the review stale for the fourth time.
//
// So a task gets a second, distinct fact: the commit an independent review AND
// a verification both actually passed against. It never replaces the work
// commit -- both are kept, because they answer different questions. "What did
// this task write?" is cdfde8dd9 forever. "What was last certified fit to
// integrate?" is whatever review and verify last agreed on.
//
// The authority is deliberately NOT "the verified commit is an ancestor of the
// head". Ancestry alone says the work survived, not that anybody looked at what
// is there now — that is precisely the reasoning verify_branch_advanced.go uses
// to ask for a review, not to skip one. A baseline is established only when all
// four hold:
//
//  1. an independent review APPROVED a specific head X — not merely approved,
//     but approved the workspace whose recorded target resolves to X;
//  2. a verification PASSED against that same X, and passed on the fingerprint
//     the review approved, with nothing having changed underneath it while it
//     ran;
//  3. the branch and worktree are still the ones this run was authorized to
//     work on;
//  4. nothing moved afterwards: the head observed now is still X, and the
//     workspace still hashes to the fingerprint that was reviewed and verified.
//
// Any of them missing leaves the baseline exactly where it was, which is the
// safe direction: the worst case is the ordinary stale-review stop the task
// would have had anyway.

// integrationBaselineVerifiedPhase is the durable baseline. Append-only, on the
// child run, beside the review and verification it rests on.
const integrationBaselineVerifiedPhase = "integration_baseline_verified"

// VerifiedIntegrationBaseline is the commit a fresh review and a fresh
// verification both passed against, and the evidence for that claim.
type VerifiedIntegrationBaseline struct {
	TaskID string `json:"taskId,omitempty"`
	// OriginalWorkCommit is what the task itself wrote. Kept forever and never
	// overwritten: it is the answer to "what did this task do", which a
	// baseline does not change.
	OriginalWorkCommit string `json:"originalWorkCommit,omitempty"`
	// VerifiedIntegrationCommit is X: the head a review approved and a
	// verification passed against.
	VerifiedIntegrationCommit string `json:"verifiedIntegrationCommit"`
	// Fingerprint is the workspace identity both agreed on, which is what ties
	// the review and the verification to each other rather than merely to the
	// same commit.
	Fingerprint string `json:"fingerprint"`
	// ReviewRunID is the approving review, and VerifyResultAt when the
	// verification that passed was recorded — the two halves of the authority.
	ReviewRunID    string    `json:"reviewRunId"`
	VerifyResultAt time.Time `json:"verifyResultAt"`
	Branch         string    `json:"branch,omitempty"`
	WorktreePath   string    `json:"worktreePath,omitempty"`
	ObservedAt     time.Time `json:"observedAt"`
}

// reconcileVerifiedIntegrationBaseline establishes the baseline when the four
// proofs hold, and does nothing otherwise. It reports whether it wrote one.
//
// Idempotent by the fact it records: a baseline already naming this commit is
// this same conclusion, so a repeat pass returns without writing. A LATER
// commit earning its own baseline appends a new record — the newest wins, and
// the older ones stay as history.
func (c *Coordinator) reconcileVerifiedIntegrationBaseline(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	steps []domain.WorkflowStep,
) (bool, error) {
	if c.reviewRuns == nil || c.workspaceFacts == nil {
		return false, nil
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
	if workStep == nil || reviewStep == nil || verifyStep == nil || reviewStep.ReviewRunID == nil {
		return false, nil
	}
	workCP, ok, err := c.store.GetLatestWorkflowCheckpointByStep(ctx, workStep.ID)
	if err != nil || !ok || workCP.WorktreePath == "" {
		return false, err
	}
	// The session only labels the observation; the worktree and branch are what
	// the fingerprint is actually taken from.
	workSession := ""
	if workCP.SessionID != nil {
		workSession = *workCP.SessionID
	}

	// Proof 1 — an independent review approved, and the workspace it approved.
	review, found, err := c.reviewRuns.GetReviewRun(ctx, *reviewStep.ReviewRunID)
	if err != nil {
		return false, err
	}
	// EffectiveVerdict already implies the review concluded, and it is strictly
	// more precise than the old status+verdict pair: a running or failed run has
	// no effective verdict, and an adopted late approval is an approval.
	if !found || review.EffectiveVerdict() != domain.VerdictApproved {
		return false, nil
	}
	approvedFingerprint := review.TargetSHA
	if approvedFingerprint == "" {
		return false, nil
	}
	// ...and the commit that fingerprint was observed at. The review's own
	// target checkpoint is the only record that ties a reviewed fingerprint to
	// a head, which is what makes "approved X" a checkable claim.
	cps, err := c.store.ListWorkflowCheckpoints(ctx, run.ID)
	if err != nil {
		return false, err
	}
	approvedHead := ""
	var approvedAt time.Time
	for _, cp := range cps {
		if cp.DurablePhase != reviewTargetDurablePhase || cp.HeadSHA == "" {
			continue
		}
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != reviewStep.ID {
			continue
		}
		if cp.FingerprintAfter != approvedFingerprint {
			continue
		}
		if approvedHead == "" || !cp.CreatedAt.Before(approvedAt) {
			approvedHead, approvedAt = cp.HeadSHA, cp.CreatedAt
		}
	}
	if approvedHead == "" {
		return false, nil
	}

	// Proof 2 — a verification PASSED against that same workspace. Both halves
	// matter: passed, and passed on what the reviewer read. A verification of a
	// different fingerprint certifies a different tree, and one that saw the
	// workspace move while it ran certifies nothing at all.
	verified := false
	var verifiedAt time.Time
	for _, cp := range cps {
		if cp.DurablePhase != verifyResultPhase {
			continue
		}
		var result VerifyResult
		if json.Unmarshal([]byte(cp.RetryState), &result) != nil || !result.Passed {
			continue
		}
		if result.ReviewedFingerprint != approvedFingerprint || result.PreFingerprint != approvedFingerprint {
			continue
		}
		if result.PostFingerprint != "" && result.PostFingerprint != approvedFingerprint {
			continue
		}
		if !cp.CreatedAt.Before(approvedAt) {
			verified, verifiedAt = true, cp.CreatedAt
		}
	}
	if !verified {
		return false, nil
	}

	// Proof 3/4 — still the authorized workspace, and nothing moved after.
	obs, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
		Path:      workCP.WorktreePath,
		Branch:    workCP.Branch,
		SessionID: domain.SessionID(workSession),
		ProjectID: domain.ProjectID(run.ProjectID),
	})
	if err != nil {
		return false, nil
	}
	if obs.Path != "" && obs.Path != workCP.WorktreePath {
		return false, nil
	}
	if workCP.Branch != "" && obs.Branch != "" && obs.Branch != workCP.Branch {
		return false, nil
	}
	if obs.HeadSHA != approvedHead {
		// The branch moved again after the verification. That is the ordinary
		// stale-review situation, and it is the fresh-review path's to answer,
		// not this one's: baselining a head nobody has verified since would be
		// the exact unverified promotion this file exists to prevent.
		return false, nil
	}
	if WorkspaceFingerprint(obs) != approvedFingerprint {
		return false, nil
	}

	// Idempotence: a baseline already naming this commit is this same
	// conclusion reached before.
	if existing, ok := c.verifiedIntegrationBaseline(ctx, run.ID); ok && existing.VerifiedIntegrationCommit == approvedHead {
		return false, nil
	}

	rec := VerifiedIntegrationBaseline{
		OriginalWorkCommit:        originalWorkCommit(cps),
		VerifiedIntegrationCommit: approvedHead,
		Fingerprint:               approvedFingerprint,
		ReviewRunID:               review.ID,
		VerifyResultAt:            verifiedAt,
		Branch:                    workCP.Branch,
		WorktreePath:              workCP.WorktreePath,
		ObservedAt:                c.clock(),
	}
	if run.PlannedTaskID != nil {
		rec.TaskID = *run.PlannedTaskID
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		return false, err
	}
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:            "wfc-" + c.newID(),
		WorkflowRunID: run.ID,
		ProjectID:     run.ProjectID,
		Branch:        rec.Branch,
		WorktreePath:  rec.WorktreePath,
		HeadSHA:       rec.VerifiedIntegrationCommit,
		RetryState:    string(payload),
		NextAction: fmt.Sprintf(
			"%s: review %s approved %s and a verification passed against it; %s is now this task's verified integration baseline (its original work commit %s is unchanged)",
			integrationBaselineVerifiedPhase, rec.ReviewRunID, shortSHA(rec.VerifiedIntegrationCommit),
			shortSHA(rec.VerifiedIntegrationCommit), shortSHA(rec.OriginalWorkCommit)),
		DurablePhase:      integrationBaselineVerifiedPhase,
		PayloadVersion:    "v1",
		FingerprintBefore: rec.Fingerprint,
		CreatedAt:         rec.ObservedAt,
	}); err != nil {
		return false, err
	}
	if c.log != nil {
		c.log.Info("workflow: a fresh review and verification re-baselined this task's verified commit",
			"run", run.ID, "baseline", rec.VerifiedIntegrationCommit,
			"originalWorkCommit", rec.OriginalWorkCommit, "review", rec.ReviewRunID)
	}
	return true, nil
}

// verifiedIntegrationBaseline returns the newest baseline recorded for this
// child run. Derived from the append-only ledger, so it survives restarts with
// no state to keep in step.
func (c *Coordinator) verifiedIntegrationBaseline(ctx stdctx.Context, childRunID string) (VerifiedIntegrationBaseline, bool) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, childRunID)
	if err != nil {
		return VerifiedIntegrationBaseline{}, false
	}
	var newest VerifiedIntegrationBaseline
	var newestAt time.Time
	found := false
	for _, cp := range cps {
		if cp.DurablePhase != integrationBaselineVerifiedPhase {
			continue
		}
		var rec VerifiedIntegrationBaseline
		if json.Unmarshal([]byte(cp.RetryState), &rec) != nil || rec.VerifiedIntegrationCommit == "" {
			continue
		}
		if !found || !cp.CreatedAt.Before(newestAt) {
			newest, newestAt, found = rec, cp.CreatedAt, true
		}
	}
	return newest, found
}

// originalWorkCommit is the commit the task's own work produced, read from the
// same checkpoint directBranchPromotionEvidence reads. Recorded on the baseline
// so both facts travel together and neither can be mistaken for the other.
func originalWorkCommit(cps []domain.WorkflowCheckpoint) string {
	out := ""
	for _, cp := range cps {
		if cp.DurablePhase == autonomousLocalCommitPhase && cp.HeadSHA != "" {
			out = cp.HeadSHA
		}
	}
	return out
}
