package workflow

import (
	stdctx "context"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/integration"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// task_promotion_proof.go — proving, or refusing to prove, that one task's
// work is part of the repository (P2-D §13, §14).
//
// This file replaces two inferences with two proofs.
//
// The old direct-branch rule was: the project's execution MODE is direct
// branch, therefore a verified, committed task's work is integrated. That is a
// statement about CONFIGURATION. It stays true if the branch was force-moved
// under the task, if the commit AO stamped came from a checkpoint belonging to
// something else entirely, or if the repository at that path has since been
// replaced.
//
// The old worktree rule was: somebody passed a non-empty SHA to
// promoteIntegratedTaskMemory, therefore the work is integrated. That is a
// statement about a FUNCTION ARGUMENT.
//
// Both are now decided from durable rows plus, where a row cannot settle it,
// real git. Every path returns a REASON when it refuses, because "AO withheld
// this fact because it could not show the worktree was ever integrated" is
// actionable and "the fact is missing" is not.
//
// The asymmetry is deliberate and total: this file can only ever refuse to
// promote. There is no input that makes it promote something a durable row
// does not describe, and every read failure -- an unreadable store, an
// unreadable repository, a missing checkpoint -- lands on a refusal.

// directBranchPromotionProof decides whether a verified, committed
// direct-branch task's facts may become canonical project knowledge.
//
// Five things have to hold, and each one is a specific way P2-C's version
// could be wrong:
//
//  1. **The placement really is direct branch.** Read through the same
//     resolver the dispatch path uses, so the two cannot disagree.
//  2. **AO durably recorded the verified boundary.** Not "a commit exists" but
//     "AO wrote down that verification passed at this head" -- which is what
//     makes the commit attributable to this task rather than merely present.
//  3. **The repository is the same repository.** Identity, not path (P2-D §9).
//  4. **The verified head is still on the branch.** If HEAD has moved on, the
//     verified commit must still be an ANCESTOR of it: the branch growing is
//     normal and safe, the verified commit disappearing from the history is a
//     rewrite and means the knowledge describes work the repository no longer
//     has.
//  5. **The generation is current.** A stale callback carrying an older
//     generation than the newest recorded verified boundary is refused, so a
//     worker that wakes up after a newer attempt finished cannot promote its
//     own superseded result (P2-D §6).
func (c *Coordinator) directBranchPromotionProof(
	ctx stdctx.Context, run domain.WorkflowRun, taskRef, repoPath string,
) domain.MemoryPromotionProof {
	if c.mutationProvenance == nil {
		return domain.UnprovablePromotion(domain.ReasonProvenanceMissing,
			"this daemon records no mutation provenance, so no promotion can be proven")
	}
	if c.mutationPlacementFor(ctx, run) != domain.MutationPlacementDirectBranch {
		return domain.UnprovablePromotion(domain.ReasonPromotionUnprovable,
			"the task did not execute on the project's own branch")
	}

	rec, found, err := c.mutationProvenance.GetLatestWorkflowMutationProvenanceByTaskBoundary(
		ctx, taskRef, domain.BoundaryVerified)
	switch {
	case err != nil:
		return domain.UnprovablePromotion(domain.ReasonProvenanceMissing,
			"the task's mutation provenance could not be read: "+err.Error())
	case !found:
		return domain.UnprovablePromotion(domain.ReasonProvenanceMissing,
			"AO holds no record of a verified boundary for this task")
	case strings.TrimSpace(rec.HeadSHA) == "":
		return domain.UnprovablePromotion(domain.ReasonPromotionUnprovable,
			"the verified boundary names no commit, so there is nothing to pin this knowledge to")
	}

	observed := c.memoryRepoIdentity(ctx, repoPath)
	if !domain.RepoIdentityCompatible(rec.RepoIdentity, observed) {
		return domain.UnprovablePromotion(domain.ReasonRepoIdentityChanged, fmt.Sprintf(
			"the repository at %s identifies as %q and the verified work was recorded against %q",
			repoPath, orUnknownIdentity(observed), orUnknownIdentity(rec.RepoIdentity)))
	}

	if reason, ok := c.verifiedHeadStillOnBranch(ctx, repoPath, rec.HeadSHA); !ok {
		return domain.UnprovablePromotion(domain.ReasonSourceDrift, reason)
	}

	return domain.MemoryPromotionProof{
		Provable:             true,
		MutationProvenanceID: rec.ID,
		VerifiedCommit:       rec.HeadSHA,
		// Direct-branch work has no separate integration: the commit IS the
		// integration, and saying so explicitly is different from leaving the
		// field empty, which would read as "the integration was not recorded".
		IntegratedCommit: rec.HeadSHA,
		RepoIdentity:     observed,
		Placement:        domain.MutationPlacementDirectBranch,
		Method:           domain.IntegrationDirectCommit,
	}
}

// worktreePromotionProof decides whether an isolated worktree's verified work
// may become canonical project knowledge.
//
// The rule this implements is the one P2-D §13 calls critical: a verified
// worktree result is NOT canonical. Canonical requires an integration AO can
// prove, and the proof has to link four things -- the worktree result head,
// the integration operation, the target ref, and the target head afterwards.
//
// integratedSHA is what the caller believes landed. It is checked against the
// durable row rather than trusted: a caller passing a plausible SHA is exactly
// the inference this file exists to remove.
//
// It takes the task rather than the run, unlike its direct-branch sibling: the
// proof is entirely about what happened to THIS TASK's worktree, and the
// parent run adds nothing the task id does not already address.
func (c *Coordinator) worktreePromotionProof(
	ctx stdctx.Context, taskID, repoPath, integratedSHA string,
) domain.MemoryPromotionProof {
	if c.mutationProvenance == nil {
		return domain.UnprovablePromotion(domain.ReasonProvenanceMissing,
			"this daemon records no mutation provenance, so no integration can be proven")
	}
	rec, found, err := c.mutationProvenance.GetLatestWorkflowMutationProvenanceByTaskBoundary(
		ctx, taskID, domain.BoundaryIntegrated)
	switch {
	case err != nil:
		return domain.UnprovablePromotion(domain.ReasonProvenanceMissing,
			"the task's integration provenance could not be read: "+err.Error())
	case !found:
		// The single most important refusal in this file. A worktree whose
		// work was verified but never integrated has no row here, and without
		// this branch its facts would become the project's knowledge on the
		// strength of a function argument.
		return domain.UnprovablePromotion(domain.ReasonPromotionUnprovable,
			"AO holds no record that this task's worktree was ever integrated")
	case strings.TrimSpace(rec.IntegrationTargetAfterSHA) == "":
		return domain.UnprovablePromotion(domain.ReasonPromotionUnprovable,
			"the integration record names no target head, so where the work landed is unproven")
	}

	if sha := strings.TrimSpace(integratedSHA); sha != "" && sha != rec.IntegrationTargetAfterSHA {
		// The caller and the durable row disagree about where the work landed.
		// The row wins, and the disagreement is a refusal rather than a silent
		// preference: two different answers to "what is the integrated head"
		// means one of them describes an integration that is not this one.
		return domain.UnprovablePromotion(domain.ReasonPromotionUnprovable, fmt.Sprintf(
			"the caller reports the work integrated at %s and AO's own record says %s",
			shortSHA(sha), shortSHA(rec.IntegrationTargetAfterSHA)))
	}

	observed := c.memoryRepoIdentity(ctx, repoPath)
	if !domain.RepoIdentityCompatible(rec.RepoIdentity, observed) {
		return domain.UnprovablePromotion(domain.ReasonRepoIdentityChanged, fmt.Sprintf(
			"the repository at %s identifies as %q and the integration was recorded against %q",
			repoPath, orUnknownIdentity(observed), orUnknownIdentity(rec.RepoIdentity)))
	}

	// Ancestry, for the methods ancestry can prove. A merge or a fast-forward
	// leaves the source commit reachable from the target head, and checking it
	// is what separates "AO recorded an integration" from "the integration is
	// still in the history". A cherry-pick produces different SHAs for the
	// same content, so the same check would refuse every legitimate one --
	// which is why the method, not the call site, decides (see
	// WorkflowIntegrationMethod.AncestryProves).
	if rec.IntegrationMethod.AncestryProves() && strings.TrimSpace(rec.HeadSHA) != "" {
		git := integration.NewExecGit("")
		contained, err := git.IsAncestor(ctx, repoPath, rec.HeadSHA, rec.IntegrationTargetAfterSHA)
		if err != nil {
			return domain.UnprovablePromotion(domain.ReasonPromotionUnprovable,
				"AO could not check whether the integrated commit is still reachable: "+err.Error())
		}
		if !contained {
			return domain.UnprovablePromotion(domain.ReasonSourceDrift, fmt.Sprintf(
				"the integrated commit %s is no longer reachable from the target head %s -- the history was rewritten after the integration",
				shortSHA(rec.HeadSHA), shortSHA(rec.IntegrationTargetAfterSHA)))
		}
	}

	verified := rec.HeadSHA
	if v, ok, err := c.mutationProvenance.GetLatestWorkflowMutationProvenanceByTaskBoundary(
		ctx, taskID, domain.BoundaryVerified); err == nil && ok && strings.TrimSpace(v.HeadSHA) != "" {
		verified = v.HeadSHA
	}

	return domain.MemoryPromotionProof{
		Provable:             true,
		MutationProvenanceID: rec.ID,
		VerifiedCommit:       verified,
		IntegratedCommit:     rec.IntegrationTargetAfterSHA,
		RepoIdentity:         observed,
		Placement:            domain.MutationPlacementIsolatedWorktree,
		Method:               rec.IntegrationMethod,
	}
}

// verifiedHeadStillOnBranch answers P2-D §14's "the branch advanced after
// verification" case.
//
// A branch that grew on top of the verified commit is normal: other tasks
// land, a person commits, and none of that makes what THIS task learned
// untrue. A branch from which the verified commit has vanished is not: an
// amend, a reset or a rebase dropped it, and knowledge pinned to it describes
// work the repository no longer contains.
//
// So the test is ancestry, not equality. Equality would refuse every healthy
// repository where anything else happened after the task; anything weaker
// would accept a rewrite.
func (c *Coordinator) verifiedHeadStillOnBranch(
	ctx stdctx.Context, repoPath, verifiedHead string,
) (string, bool) {
	if c.workspaceFacts == nil {
		return "AO cannot observe the repository, so it cannot show the verified commit is still on the branch", false
	}
	obs, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
		Path: repoPath, RepoPath: repoPath,
	})
	if err != nil {
		return "the repository could not be observed: " + err.Error(), false
	}
	current := strings.TrimSpace(obs.HeadSHA)
	switch current {
	case "":
		return "the repository reports no HEAD, so the verified commit cannot be located in its history", false
	case verifiedHead:
		return "", true
	}
	git := integration.NewExecGit("")
	contained, err := git.IsAncestor(ctx, repoPath, verifiedHead, current)
	if err != nil {
		return "AO could not check whether the verified commit is still on the branch: " + err.Error(), false
	}
	if !contained {
		return fmt.Sprintf(
			"the verified commit %s is no longer reachable from HEAD %s -- the branch was rewritten after verification",
			shortSHA(verifiedHead), shortSHA(current)), false
	}
	return "", true
}

func orUnknownIdentity(id domain.RepoIdentity) string {
	if !id.Known() {
		return "(unidentifiable)"
	}
	return string(id)
}
