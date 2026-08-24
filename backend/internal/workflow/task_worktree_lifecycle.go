package workflow

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/workspace"
)

// This file is the workflow side of an AO worktree's end: what happens to the
// directory and the ao/* branch once a task's work has landed, once it has
// failed, and once a restart has interrupted either.
//
// The ordering here is the whole design, and it is one rule applied three
// times: NOTHING is torn down until the fact that authorizes tearing it down is
// itself durable.
//
//	Integration Coordinator lands the ref  ->  audit row (integration ledger)
//	  ->  promotion checkpoint (this run's ledger)
//	  ->  worktree record marked integrated
//	  ->  worktree removed, ao/* branch deleted, record released
//
// Every arrow is a crash window, and each one is safe in a different way:
//
//   - Before the promotion checkpoint, the task is not in CompletedTaskIDs, so
//     the next pass integrates it again -- and integrating it again is a no-op,
//     because the target already contains the source and the Coordinator
//     resolves that to a fast-forward onto the commit it is already at.
//     Crucially the branch is still there for it to read, which is exactly why
//     cleanup may not run before this point.
//
//   - Between the promotion checkpoint and the worktree record, the task looks
//     completed and its worktree looks live. promoteTaskToIntegration's
//     early return -- the branch that used to just `return nil` -- finishes the
//     cleanup instead of skipping it, so the window closes on the next pass
//     rather than leaking a directory and a branch forever.
//
//   - Inside the cleanup itself, the worktree record's own states carry it:
//     internal/workspace writes `released` before deleting the branch, so a
//     restart resumes at whichever half is left.
//
// A failed or cancelled task takes the opposite road and takes it explicitly:
// its worktree is PRESERVED, which is a durable refusal to clean up, not an
// omission that a later tidy-up pass might reverse.

const (
	// taskWorktreeCleanupPhase is the durable record that an integrated task's
	// leftovers were dealt with: what was removed, what was deleted, and -- the
	// row worth having -- what was deliberately kept and why.
	taskWorktreeCleanupPhase = "task_worktree_cleanup"
	// taskWorktreePreservedPhase is the durable record that a task's worktree
	// is being kept as evidence rather than cleaned up.
	taskWorktreePreservedPhase   = "task_worktree_preserved"
	taskWorktreeLifecycleVersion = "v1"
)

// taskWorktreeCleanupPayload is the ledger row's JSON body.
type taskWorktreeCleanupPayload struct {
	TaskID string `json:"taskId"`
	// IntegratedSHA is the commit the work landed at -- the fact that
	// authorized every removal below it.
	IntegratedSHA   string `json:"integratedSha,omitempty"`
	WorktreePath    string `json:"worktreePath,omitempty"`
	Branch          string `json:"branch,omitempty"`
	WorktreeRemoved bool   `json:"worktreeRemoved,omitempty"`
	BranchDeleted   bool   `json:"branchDeleted,omitempty"`
	// BranchKept names the work AO refused to throw away. Empty when the branch
	// is gone; the most important field in the row when it is not.
	BranchKept string `json:"branchKept,omitempty"`
	// Reason explains a preserved worktree.
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
}

// finishTaskWorktree closes out an integrated task's AO worktree: it records
// the integration on the worktree record, then removes the directory and
// deletes the ao/* branch when the record can prove that is safe.
//
// It is called only after the promotion is durably recorded, and it is
// idempotent, so calling it again on the next pass -- which is exactly what a
// restart in the middle of it produces -- finishes whatever is left rather than
// redoing what is done.
//
// It is best-effort with respect to the run: a cleanup that fails must never
// turn a completed integration into a failure. The work is on the target; a
// directory that outlived it is untidy, and calling that an error would stop a
// plan over housekeeping. What it does instead is leave the record saying
// exactly what is outstanding, so boot reconciliation picks it up.
func (c *Coordinator) finishTaskWorktree(ctx stdctx.Context, parent domain.WorkflowRun, task domain.WorkflowTask, integratedSHA string) {
	if c.taskWorkspaces == nil || integratedSHA == "" {
		return
	}
	rec, err := c.taskWorkspaces.MarkIntegrated(ctx, task.ID, integratedSHA)
	if err != nil {
		if errors.Is(err, workspace.ErrNoRecord) {
			// A direct-branch task, or one that ran before AO owned its
			// worktree. There is nothing of AO's to clean up.
			return
		}
		c.recordTaskWorktreeLifecycle(ctx, parent, taskWorktreeCleanupPhase, taskWorktreeCleanupPayload{
			TaskID: task.ID, IntegratedSHA: integratedSHA, Error: err.Error(),
		}, fmt.Sprintf("task_worktree_cleanup_deferred: task %d (%s) integrated at %s but its worktree record could not be updated (%v)",
			task.Ordinal, task.Title, shortSHA(integratedSHA), err))
		return
	}

	result, err := c.taskWorkspaces.Cleanup(ctx, task.ID)
	payload := taskWorktreeCleanupPayload{
		TaskID:          task.ID,
		IntegratedSHA:   rec.IntegratedSHA,
		WorktreePath:    rec.Path,
		Branch:          rec.Branch,
		WorktreeRemoved: result.WorktreeRemoved,
		BranchDeleted:   result.BranchDeleted,
		BranchKept:      result.BranchKept,
	}
	next := fmt.Sprintf("task_worktree_cleaned_up: task %d (%s) integrated at %s; worktree %s, branch %s",
		task.Ordinal, task.Title, shortSHA(rec.IntegratedSHA),
		removedOrKept(result.WorktreeRemoved), branchOutcome(result))
	if err != nil {
		payload.Error = err.Error()
		next = fmt.Sprintf("task_worktree_cleanup_deferred: task %d (%s) is integrated at %s and its worktree could not be cleaned up yet (%v) — nothing was lost and a later pass retries",
			task.Ordinal, task.Title, shortSHA(rec.IntegratedSHA), err)
		if c.log != nil {
			c.log.Warn("workflow: task worktree cleanup deferred",
				"run", parent.ID, "task", task.ID, "path", rec.Path, "branch", rec.Branch, "err", err)
		}
	}
	c.recordTaskWorktreeLifecycle(ctx, parent, taskWorktreeCleanupPhase, payload, next)
}

// preserveTaskWorktree keeps a failed or cancelled task's worktree and branch,
// durably and on purpose.
//
// It is the one place the answer to "may this be tidied up" is decided for work
// that never integrated, and the answer is always no. The agent's commits on
// that ao/* branch are the only copy of whatever it did, and a task ending
// badly is precisely when somebody is most likely to want to read them.
func (c *Coordinator) preserveTaskWorktree(ctx stdctx.Context, parent domain.WorkflowRun, task domain.WorkflowTask, reason string) {
	if c.taskWorkspaces == nil {
		return
	}
	rec, found, err := c.taskWorkspaces.Preserve(ctx, task.ID, reason)
	if err != nil {
		if c.log != nil {
			c.log.Warn("workflow: could not preserve task worktree", "run", parent.ID, "task", task.ID, "err", err)
		}
		return
	}
	if !found || rec.State != domain.TaskWorktreePreserved {
		// No AO worktree (direct branch), or one already past the point where
		// preserving it would mean anything. Either way nothing was decided
		// here, so nothing is recorded.
		return
	}
	c.recordTaskWorktreeLifecycle(ctx, parent, taskWorktreePreservedPhase, taskWorktreeCleanupPayload{
		TaskID: task.ID, WorktreePath: rec.Path, Branch: rec.Branch, Reason: reason,
	}, fmt.Sprintf("task_worktree_preserved: task %d (%s) did not integrate, so its worktree (%s) and branch (%s) are being kept — %s",
		task.Ordinal, task.Title, rec.Path, rec.Branch, reason))
}

// reconcileTaskWorktrees is the boot pass over every AO worktree record the
// lifecycle manager is not finished with.
//
// It runs at daemon start, before any run is advanced, for the same reason
// branch locks are reconciled before workflow runs are: everything that follows
// reads worktrees and branches, and it has to read them in a state that matches
// what is durably recorded rather than whatever a crash left behind.
//
// Failures never stop startup. The report carries per-record outcomes and the
// worst case is that one repository's worktrees stay as they are until the next
// boot -- which is the same "untidy, never unsafe" trade every branch of this
// file makes.
func (c *Coordinator) reconcileTaskWorktrees(ctx stdctx.Context) {
	if c.taskWorkspaces == nil {
		return
	}
	report, err := c.taskWorkspaces.Reconcile(ctx)
	if err != nil {
		if c.log != nil {
			c.log.Error("workflow recovery: could not reconcile AO worktrees", "err", err)
		}
		return
	}
	if c.log == nil {
		return
	}
	for _, entry := range report.Entries {
		switch entry.Action {
		case workspace.ReconcileBlocked:
			c.log.Warn("workflow recovery: AO worktree could not be reconciled",
				"task", entry.TaskID, "path", entry.Record.Path, "detail", entry.Detail)
		case workspace.ReconcileAdopted:
			// The common and quiet case: the record and the directory agree and
			// nothing was done. Logging every one of them at boot would bury
			// the entries that describe an actual repair.
		default:
			c.log.Info("workflow recovery: AO worktree reconciled",
				"task", entry.TaskID, "action", string(entry.Action), "detail", entry.Detail)
		}
	}
}

// promotedHeadSHA is the commit a task's promotion left the integration ref at,
// read back from the run's own ledger.
//
// It exists for the one case that has to heal without redoing anything: a task
// whose promotion IS durably recorded but whose worktree cleanup never ran. The
// ref may have moved several times since, so the current head is the wrong
// answer and the ledger row is the only place the right one survives.
func (c *Coordinator) promotedHeadSHA(ctx stdctx.Context, masterRunID, taskID string) string {
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, masterRunID)
	if err != nil {
		return ""
	}
	head := ""
	for _, cp := range checkpoints {
		if cp.DurablePhase != masterIntegrationDurablePhase {
			continue
		}
		var payload masterIntegrationPromotionPayload
		if json.Unmarshal([]byte(cp.RetryState), &payload) != nil || payload.TaskID != taskID {
			continue
		}
		head = cp.HeadSHA
	}
	return head
}

func (c *Coordinator) recordTaskWorktreeLifecycle(
	ctx stdctx.Context,
	parent domain.WorkflowRun,
	phase string,
	payload taskWorktreeCleanupPayload,
	nextAction string,
) {
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  parent.ID,
		ProjectID:      parent.ProjectID,
		WorktreePath:   payload.WorktreePath,
		Branch:         payload.Branch,
		HeadSHA:        payload.IntegratedSHA,
		NextAction:     nextAction,
		RetryState:     string(body),
		DurablePhase:   phase,
		PayloadVersion: taskWorktreeLifecycleVersion,
		CreatedAt:      c.clock(),
	})
}

// TaskWorktreeCleanupRecord is one cleanup or preservation, as a reader sees
// it. It is the read side of the two ledger phases above, so a test or a board
// card can account for what happened to a task's directory and branch without
// knowing the checkpoint encoding.
type TaskWorktreeCleanupRecord struct {
	TaskID          string
	Preserved       bool
	IntegratedSHA   string
	WorktreePath    string
	Branch          string
	WorktreeRemoved bool
	BranchDeleted   bool
	BranchKept      string
	Reason          string
	Error           string
}

// ListTaskWorktreeCleanups returns every worktree cleanup and preservation
// recorded for a master run, oldest first.
func (c *Coordinator) ListTaskWorktreeCleanups(ctx stdctx.Context, masterRunID string) ([]TaskWorktreeCleanupRecord, error) {
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, masterRunID)
	if err != nil {
		return nil, err
	}
	var out []TaskWorktreeCleanupRecord
	for _, cp := range checkpoints {
		if cp.DurablePhase != taskWorktreeCleanupPhase && cp.DurablePhase != taskWorktreePreservedPhase {
			continue
		}
		var payload taskWorktreeCleanupPayload
		if json.Unmarshal([]byte(cp.RetryState), &payload) != nil {
			continue
		}
		out = append(out, TaskWorktreeCleanupRecord{
			TaskID:          payload.TaskID,
			Preserved:       cp.DurablePhase == taskWorktreePreservedPhase,
			IntegratedSHA:   payload.IntegratedSHA,
			WorktreePath:    payload.WorktreePath,
			Branch:          payload.Branch,
			WorktreeRemoved: payload.WorktreeRemoved,
			BranchDeleted:   payload.BranchDeleted,
			BranchKept:      payload.BranchKept,
			Reason:          payload.Reason,
			Error:           payload.Error,
		})
	}
	return out, nil
}

func removedOrKept(removed bool) string {
	if removed {
		return "removed"
	}
	return "was already gone"
}

func branchOutcome(result workspace.CleanupResult) string {
	if result.BranchDeleted {
		return "deleted"
	}
	if result.BranchKept != "" {
		return "kept (" + result.BranchKept + ")"
	}
	return "unchanged"
}
