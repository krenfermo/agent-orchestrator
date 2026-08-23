package domain

import "time"

// TaskWorktreeState is where one AO-owned task worktree is in its lifecycle.
//
// The state is durable and not derived, unlike session status: a worktree
// exists on disk or it does not, and the only witness to a creation that
// crashed halfway is the row that was written before the git call. Deriving
// this from the filesystem would mean a directory that never appeared is
// indistinguishable from one that was never asked for.
type TaskWorktreeState string

const (
	// TaskWorktreeCreating is written BEFORE git is asked to add the worktree,
	// so a crash between the two leaves evidence of the attempt (path, branch,
	// base) instead of an orphan directory nothing in AO knows about.
	TaskWorktreeCreating TaskWorktreeState = "creating"
	// TaskWorktreeActive means the worktree exists on disk and the task's work
	// belongs in it.
	TaskWorktreeActive TaskWorktreeState = "active"
	// TaskWorktreeReleased means the worktree directory is gone. The branch is
	// NOT: releasing a worktree removes the checkout, never the commits, so a
	// released row still names the branch that holds the task's work.
	TaskWorktreeReleased TaskWorktreeState = "released"
	// TaskWorktreeFailed means creation or teardown failed and the row is the
	// record of what was attempted. It is terminal for the manager; a human or
	// a later run decides what to do with whatever is on disk.
	TaskWorktreeFailed TaskWorktreeState = "failed"
)

// IsKnown reports whether the value is one this build understands.
func (s TaskWorktreeState) IsKnown() bool {
	switch s {
	case TaskWorktreeCreating, TaskWorktreeActive, TaskWorktreeReleased, TaskWorktreeFailed:
		return true
	default:
		return false
	}
}

// TaskWorktreeDependency is the commit one dependency task's work sat at when
// this worktree was cut.
//
// It is stored rather than resolved on demand because it is a fact about a
// moment that does not come back: the dependency's branch moves on, gets
// rebased, or is deleted after integration, and "what was this task actually
// built on top of" then has no answer. Integration and post-run QA both need
// that answer to explain a conflict.
type TaskWorktreeDependency struct {
	// TaskID is the dependency task.
	TaskID string `json:"taskId"`
	// SHA is the commit its work was at. Empty only when the dependency had no
	// resolvable ref at all, which is itself worth recording.
	SHA string `json:"sha"`
}

// TaskWorktreeRecord is the durable identity of one AO-owned worktree created
// for one planned task.
//
// Every field answers a question that cannot be re-derived later:
//
//   - Path and Branch say what AO created, so cleanup can find it without
//     guessing at a naming convention that may since have changed.
//   - TargetBranch says where the work is meant to land. It is NOT the same as
//     Branch: Branch is the throwaway ao/* branch the agent commits to, and
//     losing the target means an integration step has to re-infer it from
//     project config that may have been edited in between.
//   - BaseSHA pins the commit the worktree was cut from, which is what makes a
//     later diff against "what this task actually changed" honest even after
//     the target branch has moved.
//   - Dependencies pin the same for every task this one builds on.
//   - TaskID and WorkflowRunID tie the worktree back to the plan, so an
//     abandoned directory on disk is attributable rather than mysterious.
type TaskWorktreeRecord struct {
	WorkflowRunID string
	TaskID        string
	ProjectID     ProjectID
	// RepoPath is the primary repository the worktree was cut from — the
	// user's own checkout. It is recorded so teardown runs `git worktree
	// remove` against the same repo that registered the worktree, and never
	// has to search for it.
	RepoPath string
	// Path is the absolute worktree directory, under the AO data dir.
	Path string
	// Branch is the AO-owned throwaway branch, always ao/*-prefixed.
	Branch string
	// TargetBranch is the branch the task's work is ultimately for.
	TargetBranch string
	// BaseSHA is the commit Branch was created at.
	BaseSHA string
	// Dependencies are the dependency task commits this worktree was cut
	// against, sorted by TaskID.
	Dependencies []TaskWorktreeDependency
	// ExecutionMode is the mode that produced this worktree
	// (isolated_worktree or smart_parallel_worktrees). A direct_branch task
	// never has a record at all.
	ExecutionMode ExecutionMode
	State         TaskWorktreeState
	// Detail explains a Failed state. Empty otherwise.
	Detail     string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	ReleasedAt *time.Time
}
