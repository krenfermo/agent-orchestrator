package workflow

import (
	stdctx "context"
	"os"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// historical_placement.go — the read-only half of the placement authority.
//
// A placement answers two different questions, and P3-A's smokes found that AO
// had conflated them:
//
//	ACTIVE       "where may work be launched right now"
//	HISTORICAL   "where did this run's work actually happen"
//
// The first is retired the instant a run reaches a terminal state, and that is
// correct: nothing will ever launch into it again, and a live placement for a
// finished run keeps the obligation's live index occupied forever. The second
// must survive, because the moment a run finishes is exactly when somebody
// wants to see what is in the repository, where AO worked, and what is left to
// commit. Asking the LIVE index that question is what made
// GET /pending-changes answer "AO has no frozen placement for this run" about a
// run that had just finished working in a repository AO can name precisely.
//
// Nothing here resurrects anything. recallPlacement performs no writes, takes
// no lock, starts no runtime and creates no worktree; it reads rows that were
// already durable and reports whether what it found is still live. Every
// MUTATING caller must consult that flag and refuse a recollection — see
// commitAuthorityFor in pending_changes.go.

// placementRecall is one placement plus the one fact that decides what a caller
// may do with it.
type placementRecall struct {
	domain.ExecutionPlacement
	// Live reports that this record is still the run's live execution
	// authority. False means it is HISTORY: safe to read, describe and probe,
	// never sufficient to authorise a write.
	Live bool
}

// recallPlacement returns the placement a read-only surface may describe for
// this run: the live one while there is one, and otherwise the newest
// generation recorded for the run's own obligation whatever state it retired
// into.
//
// Newest generation, not newest row: a replaced placement leaves the superseded
// generation behind, and describing that one would name a worktree the work was
// moved out of. This is the same rule ListPlacements uses for `current`, and
// deliberately so — two answers to "which placement is this run's" is how a
// person ends up looking at the wrong repository.
func (c *Coordinator) recallPlacement(ctx stdctx.Context, run domain.WorkflowRun) (placementRecall, bool) {
	if !c.placementEnabled() {
		return placementRecall{}, false
	}
	scope := placementScopeFor(run)
	if live, found, err := c.placements.GetLiveExecutionPlacement(ctx, scope.runID, scope.taskID, scope.stepID); err == nil && found {
		return c.enrichRecall(ctx, placementRecall{ExecutionPlacement: live, Live: true}), true
	}
	records, err := c.placements.ListExecutionPlacementsForRun(ctx, run.ID)
	if err != nil {
		return placementRecall{}, false
	}
	var newest domain.ExecutionPlacement
	found := false
	for _, r := range records {
		if r.WorkflowRunID != scope.runID || r.TaskID != scope.taskID || r.WorkflowStepID != scope.stepID {
			continue
		}
		if !found || r.PlacementGeneration > newest.PlacementGeneration {
			newest, found = r, true
		}
	}
	if !found {
		return placementRecall{}, false
	}
	return c.enrichRecall(ctx, placementRecall{ExecutionPlacement: newest, Live: false}), true
}

// enrichRecall fills in the isolated worktree path when the placement row does
// not carry one.
//
// It has to, because a plain task run frequently does not have one: the
// worktree identity is adopted onto the placement from a task-worktree record,
// and a run with no planned-task decomposition has no such record — so the
// column stays empty for a run whose worktree AO created, used, reviewed and
// verified. The work step's own checkpoints carry that path on every write
// (observeWorkStep forwards it deliberately), so the fact is durable; it was
// simply being read from the one place that did not have it.
//
// Direct-branch placements are left alone: they have no AO worktree, and
// inventing one would be fabricating an identity.
func (c *Coordinator) enrichRecall(ctx stdctx.Context, recall placementRecall) placementRecall {
	if !recall.Type.Isolated() || strings.TrimSpace(recall.WorktreePath) != "" {
		return recall
	}
	recall.WorktreePath = c.recordedWorktreePath(ctx, recall.WorkflowRunID)
	return recall
}

// recordedWorktreePath is the newest worktree path this run durably recorded on
// a checkpoint, or empty when it recorded none.
func (c *Coordinator) recordedWorktreePath(ctx stdctx.Context, runID string) string {
	if c.store == nil || runID == "" {
		return ""
	}
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return ""
	}
	for i := len(checkpoints) - 1; i >= 0; i-- {
		if path := strings.TrimSpace(checkpoints[i].WorktreePath); path != "" {
			return path
		}
	}
	return ""
}

// directoryExists reports whether a path is present as a directory.
//
// Used only to tell "the worktree is gone" from "the worktree cannot be read",
// which are different answers to a person: the first is the ordinary end of an
// integrated task's life, and the second is a problem. AO never recreates one
// to answer a read.
func directoryExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
