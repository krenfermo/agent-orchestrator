package workflow

import (
	stdctx "context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// placement_semantics.go — P3-C §28: direct-branch semantics come from the RUN's
// placement, never from the project's default mode.
//
// THE BUG THIS CLOSES. A run may be placed on the user's own branch explicitly
// (an applied placement override, or a task scope that downgraded to
// direct-branch) inside a project whose default execution mode is isolated.
// Admission already gets this right — it takes the branch lock when
// `placement.Type == domain.PlacementDirectBranch`, a fact about the run — so
// such a run really does hold a per-(repository, branch) lock and really does
// write on somebody's actual branch.
//
// Everything AFTER the launch, though, asked the PROJECT. autonomousLocalCommit
// read domain.ResolveExecutionMode(project) and returned early for any project
// not in direct-branch mode, which meant an explicit direct-branch run in an
// isolated-default project finished with its work uncommitted on a real branch,
// no autonomous commit, and — worse than the missing commit — no
// autonomous_local_commit_deferred checkpoint either, so nothing anywhere said
// so. The lock was later released by the run's terminal transition and the
// changes were left sitting in the developer's checkout, attributable to
// nothing.
//
// That is a correctness defect rather than a presentation one: it is the same
// project-vs-placement conflation P3-A §7 removed from the mutation-provenance
// path, still live in the commit path. The rule this file states once, so no
// third path can reintroduce it:
//
//	direct_branch placement -> branch lock semantics, direct-branch commit and
//	integration semantics. Full stop. The project's default mode answers "how
//	does this project usually work", which is not the question.

// runPlacementIsDirectBranch reports whether a run's EXECUTION PLACEMENT is a
// direct branch.
//
// The frozen record wins whenever there is one. The project's configured mode
// is consulted only as the legacy fallback — a run from before placements were
// durable has no record of its own, and the project's mode is then the only
// answer that exists. That ordering, not the fallback's existence, is what
// makes the semantics placement-derived.
func (c *Coordinator) runPlacementIsDirectBranch(ctx stdctx.Context, run domain.WorkflowRun) bool {
	scope := placementScopeFor(run)
	if c.placementEnabled() {
		if live, found, err := c.placements.GetLiveExecutionPlacement(ctx, scope.runID, scope.taskID, scope.stepID); err == nil && found && live.Type.IsKnown() {
			return live.Type == domain.PlacementDirectBranch
		}
	}
	return c.projectExecutionModeFor(ctx, run, scope).DirectBranch()
}

// runIDPlacementIsDirectBranch is runPlacementIsDirectBranch for a caller that
// holds only an id. A run it cannot read answers false, which is the safe
// direction: the direct-branch answer is the one that authorizes writing on
// somebody's real branch, and AO must never assume that authority.
func (c *Coordinator) runIDPlacementIsDirectBranch(ctx stdctx.Context, runID string) bool {
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil || !ok {
		return false
	}
	return c.runPlacementIsDirectBranch(ctx, run)
}

// directBranchTarget names the repository and branch a run's frozen
// direct-branch placement will write to, or two empty strings when the run has
// no readable placement or is not direct-branch.
//
// It exists because "does this run need a branch lock" and "which branch does
// it need a lock ON" were answered by two different authorities: admission
// derived the first from the run's placement (correctly), while the lock
// manager derived the second from the project's execution mode. For an explicit
// direct-branch placement inside an isolated-default project those disagreed,
// the second produced no target at all, and an acquisition with no targets
// reads as success -- so the run launched onto the user's branch holding no
// lock and having skipped the dirty-worktree gate entirely.
//
// Returning empty for a non-direct-branch or unreadable placement is what keeps
// the manager's project-derived behaviour untouched everywhere else, including
// the workspace-project case where the project's own configuration is the only
// thing that knows about the child repositories.
func (c *Coordinator) directBranchTarget(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep) (string, string) {
	if !c.placementEnabled() {
		return "", ""
	}
	placement, ok, err := c.EnsureExecutionPlacement(ctx, run, step)
	if err != nil || !ok || placement.Type != domain.PlacementDirectBranch {
		return "", ""
	}
	return placement.RepoPath, placement.ExecutionBranch
}

// placementWorkspacePath is the checkout a READ-ONLY agent should be pointed at
// for this run: the isolated worktree when there is one, and the repository
// itself for a direct-branch placement.
//
// It exists because the Decision Resolver looked for a worktree path on the
// step's latest checkpoint and gave up when it found none — and a direct-branch
// run HAS no worktree by definition, so it never found one. The consequence was
// not a bad answer but no answer at all: every autonomous decision on a
// direct-branch run parked forever on "resolver unavailable (no worktree
// recorded yet)", which made the question-autonomy policy inert for exactly the
// placement most single-developer projects use. Found by the P3-C closing smoke.
//
// Read-only and non-freezing on purpose: this is consulted from a read path, and
// a projection that froze a placement as a side effect of being displayed would
// be the mutation-on-read the checkpoint forbids. A run with no live placement
// answers "", and the caller keeps waiting exactly as it did before.
func (c *Coordinator) placementWorkspacePath(ctx stdctx.Context, run domain.WorkflowRun) (string, string) {
	if !c.placementEnabled() {
		return "", ""
	}
	scope := placementScopeFor(run)
	live, found, err := c.placements.GetLiveExecutionPlacement(ctx, scope.runID, scope.taskID, scope.stepID)
	if err != nil || !found || !live.Type.IsKnown() {
		return "", ""
	}
	if live.Type == domain.PlacementDirectBranch {
		return live.RepoPath, live.ExecutionBranch
	}
	return live.WorktreePath, live.ExecutionBranch
}
