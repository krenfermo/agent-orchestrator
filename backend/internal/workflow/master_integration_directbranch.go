package workflow

import (
	"encoding/json"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Checkpoint 8P-E.13B: master task promotion in direct-branch execution mode.
//
// 8M.1 (master_integration.go) gave a master run exactly one way to carry task
// N's verified code to task N+1: capture the child's worktree into an AO-owned
// commit under refs/ao/workflows/<master>/integration, and base the next
// child's *new worktree* on that ref. That is the correct and only possible
// answer in isolated-worktree mode, where every child works in a throwaway tree
// on a generated ao/* branch and nothing it produces exists anywhere else.
//
// In direct-branch mode that whole mechanism is answering a question the mode
// does not ask. Every child of the master works in the SAME registered
// repository, on the SAME configured branch, and completeVerifiedRun already
// commits the verified result there (autonomousLocalCommit, branch_execution.go)
// while the branch lock is still held. Task N's code is therefore already,
// durably, on the exact branch task N+1 will open — there is no second worktree
// to propagate into, which is precisely why the directbranch adapter refuses
// MaterializeIntegrationCommit with ErrWorkspaceOperationUnsupported rather than
// synthesizing a fictional refs/ao/* head.
//
// Before this file, promoteTaskToIntegration called that unsupported operation
// unconditionally, so a child that had genuinely passed review AND verification
// could never be promoted: every reconcile pass recorded the same
// master_integration_promotion_failed and the master never reached task 2.
//
// The rule here is deliberately not "direct branch needs no promotion, complete
// the task": that would complete a task on the strength of the child's own
// state alone, with no evidence about the branch everyone else is about to
// build on. Promotion still happens, still writes the same
// master_integration_promotion checkpoint the isolated-worktree path writes, and
// still advances the same integration state — it just proves integration
// instead of performing it. See directBranchPromotionEvidence for what must be
// proven; anything short of proof stops safely as an integration failure.
const masterIntegrationModeDirectBranch = "direct_branch"

// directBranchEvidenceKind is how the verified result reached the target
// branch — the two mutually exclusive shapes a completed direct-branch child
// can durably leave behind.
type directBranchEvidenceKind int

const (
	// directBranchEvidenceCommitted: AO itself committed the verified result on
	// the target branch (GitPolicy.LocalCommit == automatic). The commit SHA is
	// the promotion's identity, and proving integration means proving the branch
	// still points at it.
	directBranchEvidenceCommitted directBranchEvidenceKind = iota
	// directBranchEvidenceAlreadyClean: the verified workspace held nothing to
	// commit, so the verified state IS the committed branch state. Proving
	// integration means proving the workspace still fingerprints identically to
	// the state verify passed on, and that it holds no uncommitted work (an
	// uncommitted result is not a durably integrated one, whatever it verified
	// as).
	directBranchEvidenceAlreadyClean
)

// directBranchEvidence is everything the child durably recorded about where its
// verified result lives. Every field is copied from a checkpoint the child
// wrote at the time — nothing here is inferred at promotion time, because the
// whole point is to compare the recorded past against the observed present.
type directBranchEvidence struct {
	Kind        directBranchEvidenceKind
	RepoPath    string
	Branch      string
	CommitSHA   string
	Fingerprint string
}

// The promotion function that used to live here is GONE, deliberately.
//
// It was the second integration route: it observed the target branch and
// recorded a promotion, outside any lane, with no readiness gate and no audit
// row. Task 5's reviewer found it, and the fix is not to harden it — a second
// road that has been made as safe as the first is still a second road, and the
// two drifted apart once already. Direct-branch integration now goes through
// integrateReadyTask into the Integration Coordinator exactly like every other
// mode, expressing what is different about it as inputs (NoReplay, a
// Precondition, StrategyNoOp) rather than as a separate implementation. What
// remains in this file is only the evidence reading that route needs, which was
// never the part in question.

// directBranchPromotionEvidence reads the child's own append-only checkpoint
// ledger for the newest record of where its verified result ended up.
//
// autonomous_local_commit wins whenever present: it is written by
// commitHeldRepositories immediately after verification passed and while the
// branch lock is still held, so its HeadSHA is the verified result, committed,
// on the locked branch — the strongest statement available anywhere in the
// system.
//
// Otherwise the fallback is the verify step's own result checkpoint, which
// carries the fingerprint verification actually ran against. That only counts
// as evidence together with the caller's cleanliness check: it proves "the
// branch state is exactly what was verified", which is integration only when
// there is nothing uncommitted left over.
//
// ok=false means the child proved neither, which is a stop, never a pass.
func directBranchPromotionEvidence(checkpoints []domain.WorkflowCheckpoint, workCP domain.WorkflowCheckpoint) (directBranchEvidence, bool) {
	var commit, verify *domain.WorkflowCheckpoint
	for i := range checkpoints {
		cp := &checkpoints[i]
		switch cp.DurablePhase {
		case autonomousLocalCommitPhase:
			if cp.HeadSHA != "" {
				commit = cp
			}
		case verifyResultPhase:
			var result VerifyResult
			if json.Unmarshal([]byte(cp.RetryState), &result) == nil && result.Passed {
				verify = cp
			}
		}
	}
	if commit != nil {
		return directBranchEvidence{
			Kind:      directBranchEvidenceCommitted,
			RepoPath:  commit.WorktreePath,
			Branch:    commit.Branch,
			CommitSHA: commit.HeadSHA,
		}, commit.WorktreePath != ""
	}
	if verify == nil || workCP.WorktreePath == "" {
		return directBranchEvidence{}, false
	}
	fingerprint := verify.FingerprintAfter
	if fingerprint == "" {
		fingerprint = verify.FingerprintBefore
	}
	if fingerprint == "" {
		return directBranchEvidence{}, false
	}
	return directBranchEvidence{
		Kind:        directBranchEvidenceAlreadyClean,
		RepoPath:    workCP.WorktreePath,
		Branch:      workCP.Branch,
		Fingerprint: fingerprint,
	}, true
}

// hasNonEphemeralChanges reports whether the observation holds any change that
// matters, applying exactly the ephemeral-artifact policy WorkspaceFingerprint
// itself applies — a leftover __pycache__ entry must not be read as "the
// verified result is uncommitted".
func hasNonEphemeralChanges(obs ports.WorkspaceObservation) bool {
	for _, ch := range obs.Changes {
		if !IsEphemeralArtifactPath(ch.Path) {
			return true
		}
	}
	return false
}

// Durable phases this file reads. verifyResultPhase is also the phase verify.go
// WRITES every VerifyResult under, and the one verify_recovery.go's ledger reads
// back to know whether a recovery generation has been answered; naming them here
// makes the cross-file dependency explicit rather than a repeated literal.
const (
	autonomousLocalCommitPhase = "autonomous_local_commit"
	verifyResultPhase          = "verify_result"
)
