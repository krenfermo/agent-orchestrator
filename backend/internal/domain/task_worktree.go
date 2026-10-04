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
	// TaskWorktreeIntegrated means the task's work is durably on its target
	// ref and the integration is durably recorded, but the worktree and the
	// ao/* branch are still there.
	//
	// It exists for exactly one crash window: between the moment an
	// integration becomes a fact and the moment its leftovers are cleaned up.
	// Without a state for it, a restart in that window is indistinguishable
	// from a restart before the integration -- the worktree is present, the
	// branch is present, the task looks ready -- and the only way to tell is
	// to integrate again, which is the duplicate this whole design exists to
	// prevent. Written AFTER the audit record and BEFORE the first removal, so
	// the worst a crash can leave behind is a row that says "this landed at
	// <sha>; finish tidying up".
	TaskWorktreeIntegrated TaskWorktreeState = "integrated"
	// TaskWorktreeReleased means the worktree directory is gone. The branch is
	// NOT necessarily: releasing a worktree removes the checkout, never the
	// commits. A released row that also has BranchDeleted set is the only
	// shape where AO claims the ao/* branch is gone too, and it is only ever
	// reached for work that provably landed.
	TaskWorktreeReleased TaskWorktreeState = "released"
	// TaskWorktreePreserved means the task failed, was cancelled, or was
	// abandoned with work that never reached its target, and its directory and
	// branch are deliberately being kept.
	//
	// It is the explicit opposite of released: a durable "do not clean this
	// up". Cleanup is not merely skipped for such a task, it is refused, and
	// the state is what a later pass reads instead of re-deciding from
	// whatever is on disk. The agent's commits are the only copy of work
	// somebody may still want, and a tidy-up that deletes them is the one
	// unrecoverable mistake in this package.
	TaskWorktreePreserved TaskWorktreeState = "preserved"
	// TaskWorktreeFailed means creation or teardown failed and the row is the
	// record of what was attempted. It is terminal for the manager; a human or
	// a later run decides what to do with whatever is on disk.
	TaskWorktreeFailed TaskWorktreeState = "failed"
)

// IsKnown reports whether the value is one this build understands.
func (s TaskWorktreeState) IsKnown() bool {
	switch s {
	case TaskWorktreeCreating, TaskWorktreeActive, TaskWorktreeIntegrated,
		TaskWorktreeReleased, TaskWorktreePreserved, TaskWorktreeFailed:
		return true
	default:
		return false
	}
}

// Terminal reports whether the manager is done with this record: nothing it
// does later can move a released or preserved row, and a failed one waits for
// a person.
func (s TaskWorktreeState) Terminal() bool {
	switch s {
	case TaskWorktreeReleased, TaskWorktreePreserved, TaskWorktreeFailed:
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
	// IntegratedSHA is the commit this task's work actually reached its target
	// at, copied from the integration's own audit record.
	//
	// It is the authorization for every destructive step that follows. The
	// branch is only deleted when its tip is provably reachable from this
	// commit, so "the work is safe to throw the checkout away" is a proof
	// against a recorded fact rather than an inference from a state name.
	// Empty for every record that has not integrated.
	IntegratedSHA string
	// BranchDeleted reports that the ao/* branch is gone -- deleted by AO's own
	// cleanup, or already absent when cleanup went to look.
	//
	// It is recorded rather than re-derived because absence alone is ambiguous
	// at read time: a branch may be missing because cleanup finished, or
	// because it was never created. Only a row that says so distinguishes "this
	// task is done with" from "this task still has work sitting on a branch",
	// and the second is the one a reconcile pass has to finish.
	BranchDeleted bool
	// Detail explains a Failed state, or says why a Preserved one is being
	// kept. Empty otherwise.
	Detail     string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	ReleasedAt *time.Time
}
