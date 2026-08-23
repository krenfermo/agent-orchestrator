package domain

import "fmt"

// ExecutionMode is how a project materialises the git working tree an
// autonomous workflow's sessions operate in (Checkpoint 8P-E.11).
//
// Before this checkpoint AO had exactly one, implicit mode: every session got
// its own `git worktree` under the data dir on a generated `ao/*` branch. That
// architecture is preserved verbatim as ExecutionIsolatedWorktree and remains
// the default for every existing and newly registered project. ExecutionDirect
// Branch is the opt-in alternative for single-developer workflows where the
// isolation is pure overhead: AO works in the registered repository itself, on
// the branch the project (or, for a workspace project, each registered
// repository) already configures.
//
// ExecutionSmartParallelWorktrees is a third, opt-in setting layered on top of
// the isolated-worktree architecture: the workspace is materialised exactly the
// way isolated_worktree materialises it, and the only thing that changes is a
// plan-time decision about which of a master plan's tasks may occupy their own
// worktree concurrently. It is deliberately NOT a new workspace adapter --
// every consumer that branches on execution mode asks "is this direct branch?",
// so a smart-parallel project takes the same worktree path an isolated project
// always took.
type ExecutionMode string

const (
	// ExecutionIsolatedWorktree is the pre-8P-E.11 behavior: one throwaway
	// git worktree per session, on a generated ao/* branch, under the AO data
	// dir. Cleanup removes the worktree and the branch is never pushed by AO.
	ExecutionIsolatedWorktree ExecutionMode = "isolated_worktree"
	// ExecutionDirectBranch works directly inside the registered repository
	// path, on the configured base branch. No ao/* branch and no git worktree
	// are ever created, so there is nothing to clean up; concurrency is
	// protected by a durable per-repository+branch execution lock instead of
	// by physical isolation.
	ExecutionDirectBranch ExecutionMode = "direct_branch"
	// ExecutionSmartParallelWorktrees materialises workspaces exactly like
	// ExecutionIsolatedWorktree and additionally lets the planner run a
	// master plan's independent tasks in their own worktrees at the same
	// time. Which tasks actually get that is not a property of the setting:
	// it is decided per task when the plan is accepted, from the task DAG and
	// the estimated write sets, and a task the classifier cannot prove
	// independent is downgraded back to a plain isolated worktree (see
	// workflow.SelectTaskExecutionStrategies). Selecting this mode therefore
	// grants permission to parallelise, never a guarantee that anything will.
	ExecutionSmartParallelWorktrees ExecutionMode = "smart_parallel_worktrees"
)

// WithDefault returns ExecutionIsolatedWorktree when the stored value is empty
// (every project registered before Checkpoint 8P-E.11), so an existing install
// never silently changes execution mode on upgrade.
func (m ExecutionMode) WithDefault() ExecutionMode {
	if m == "" {
		return ExecutionIsolatedWorktree
	}
	return m
}

// IsKnown reports whether the value is one this build understands. The empty
// value is known and means "unset — use the default".
func (m ExecutionMode) IsKnown() bool {
	switch m {
	case "", ExecutionIsolatedWorktree, ExecutionDirectBranch, ExecutionSmartParallelWorktrees:
		return true
	default:
		return false
	}
}

// DirectBranch reports whether the effective mode works in the registered
// repository rather than in a per-session worktree.
func (m ExecutionMode) DirectBranch() bool {
	return m.WithDefault() == ExecutionDirectBranch
}

// SmartParallel reports whether the effective mode lets the planner assign
// independent tasks their own concurrent worktrees. It is the ONLY question
// that distinguishes smart_parallel_worktrees from isolated_worktree: every
// other consumer asks DirectBranch and must keep treating the two identically.
func (m ExecutionMode) SmartParallel() bool {
	return m.WithDefault() == ExecutionSmartParallelWorktrees
}

// GitActionPolicy is the autonomy granted to AO for one class of git write
// (Checkpoint 8P-E.11). It answers a single question — "may an autonomous
// workflow do this on its own?" — and is deliberately a three-value vocabulary
// rather than a bool so "ask me" is representable without overloading "never".
type GitActionPolicy string

const (
	// GitActionAutomatic lets an autonomous workflow perform the action as
	// part of normal completion, without pausing for a human.
	GitActionAutomatic GitActionPolicy = "automatic"
	// GitActionRequireApproval performs the action only after an explicit
	// human decision. The workflow parks rather than proceeding.
	GitActionRequireApproval GitActionPolicy = "require_approval"
	// GitActionNever forbids the action entirely. AO does not perform it and
	// does not ask; the work simply stops short of it.
	GitActionNever GitActionPolicy = "never"
)

// IsKnown reports whether the value is one this build understands. The empty
// value is known and means "unset — use the default for this action".
func (p GitActionPolicy) IsKnown() bool {
	switch p {
	case "", GitActionAutomatic, GitActionRequireApproval, GitActionNever:
		return true
	default:
		return false
	}
}

// WithDefault returns def when p is unset.
func (p GitActionPolicy) WithDefault(def GitActionPolicy) GitActionPolicy {
	if p == "" {
		return def
	}
	return p
}

// GitPolicy is the per-project autonomous git policy: what an unattended
// workflow may do to the repository on its own (Checkpoint 8P-E.11).
//
// The defaults are deliberately asymmetric between local and remote effects. A
// local commit is reversible, invisible outside the machine, and is part of
// normal autonomous completion — pausing a whole workflow to ask permission for
// one is exactly the interruption this checkpoint removes. A push or a merge
// changes state other people can see, so neither is ever automatic by default.
type GitPolicy struct {
	// LocalCommit governs `git commit` in the working repository. Defaults to
	// automatic.
	LocalCommit GitActionPolicy `json:"localCommit,omitempty" enum:"automatic,require_approval,never"`
	// Push governs publishing commits to a remote. Defaults to never.
	Push GitActionPolicy `json:"push,omitempty" enum:"automatic,require_approval,never"`
	// Merge governs merging the work branch into another branch. Defaults to
	// never, and is not applicable at all in direct-branch mode (there is no
	// separate work branch to merge from) — see EffectiveMerge.
	Merge GitActionPolicy `json:"merge,omitempty" enum:"automatic,require_approval,never"`
}

// DefaultGitPolicy is the policy a project has when it configures none.
func DefaultGitPolicy() GitPolicy {
	return GitPolicy{
		LocalCommit: GitActionAutomatic,
		Push:        GitActionNever,
		Merge:       GitActionNever,
	}
}

// WithDefaults fills only the actions the project left unset.
func (g GitPolicy) WithDefaults() GitPolicy {
	def := DefaultGitPolicy()
	g.LocalCommit = g.LocalCommit.WithDefault(def.LocalCommit)
	g.Push = g.Push.WithDefault(def.Push)
	g.Merge = g.Merge.WithDefault(def.Merge)
	return g
}

// EffectiveMerge resolves the merge policy against the execution mode. Direct
// branch has no AO-created work branch, so there is nothing AO could merge:
// the answer is a hard never regardless of what is configured, rather than a
// setting that silently does nothing.
func (g GitPolicy) EffectiveMerge(mode ExecutionMode) GitActionPolicy {
	if mode.DirectBranch() {
		return GitActionNever
	}
	return g.WithDefaults().Merge
}

// Validate rejects values outside the typed vocabulary.
func (g GitPolicy) Validate() error {
	for name, value := range map[string]GitActionPolicy{
		"localCommit": g.LocalCommit,
		"push":        g.Push,
		"merge":       g.Merge,
	} {
		if !value.IsKnown() {
			return fmt.Errorf("git.%s: unknown policy %q", name, value)
		}
	}
	return nil
}
