package domain

import "time"

// execution_placement.go — P1-D §A/§B: the FROZEN execution placement.
//
// Before this type, "where does this task's work happen" was answered by
// reading the project's execution mode at the moment somebody asked. That is a
// derivation, not a fact, and it has one failure mode that matters: a project
// switched from isolated_worktree to direct_branch while a task is mid-flight
// changes the answer under a run that has already created a worktree, already
// taken a branch lock, and may already have written code. Recovery then
// reconstructs a placement that never existed.
//
// An ExecutionPlacement is the durable answer, written once, before any
// mutation, and read forever after. Selection policy may be computed before the
// freeze; after it, the stored record wins and the project config is not
// consulted again for this task.
//
// It carries its OWN generation, distinct from the task's lifecycle
// generation, because the two supersede for different reasons: a task
// generation advances when the obligation is retried, and a placement
// generation advances only when the physical placement itself is replaced. A
// provider failover advances neither.

// ExecutionPlacementType is the frozen physical placement of one task's work.
//
// Deliberately two values, not three. smart_parallel_worktrees is a SCHEDULING
// permission -- it says independent tasks may run their isolated worktrees
// concurrently -- and it materialises identically to isolated_worktree. Storing
// it as a third placement type would mean a placement record that changes
// meaning when the planner's independence classification changes, which is
// exactly the read-time derivation this record exists to eliminate.
type ExecutionPlacementType string

const (
	// PlacementDirectBranch is work performed in the registered repository
	// itself, on the configured branch, protected by a durable branch lock
	// rather than by physical isolation.
	PlacementDirectBranch ExecutionPlacementType = "direct_branch"
	// PlacementIsolatedWorktree is work performed in an AO-owned git worktree
	// on a generated ao/* branch under the AO data dir.
	PlacementIsolatedWorktree ExecutionPlacementType = "isolated_worktree"
)

// IsKnown reports whether the value is one this build understands. An unknown
// placement type is never coerced to a default: it fails closed, because
// guessing "probably isolated" for a record AO cannot read is how a direct
// branch gets written without a lock.
func (t ExecutionPlacementType) IsKnown() bool {
	return t == PlacementDirectBranch || t == PlacementIsolatedWorktree
}

// Isolated reports whether this placement has an AO-owned worktree.
func (t ExecutionPlacementType) Isolated() bool { return t == PlacementIsolatedWorktree }

// PlacementTypeForExecutionMode maps a project's execution mode onto the
// placement type it materialises.
//
// This is the SELECTION policy, and it is legitimate to run before a freeze.
// It must never be used to answer "what placement does this task have" after
// one -- that question is answered by the stored record.
func PlacementTypeForExecutionMode(mode ExecutionMode) ExecutionPlacementType {
	if mode.DirectBranch() {
		return PlacementDirectBranch
	}
	return PlacementIsolatedWorktree
}

// ExecutionPlacementState is where a frozen placement is in its life.
//
// The vocabulary follows the physical thing rather than the workflow: a
// placement is ready when the checkout and the branch authority exist, active
// while an agent may write to it, and integrated once its work has landed. The
// workflow's own step states are a separate axis and stay where they are.
type ExecutionPlacementState string

const (
	// PlacementSelected is written at freeze time, before anything physical
	// exists. It is the record that authorizes creating the worktree or taking
	// the lock, so a crash immediately afterwards leaves evidence of what was
	// about to be made rather than an orphan.
	PlacementSelected ExecutionPlacementState = "selected"
	// PlacementWaiting means the placement is frozen but cannot yet be
	// prepared -- typically because the branch it needs is held by another run.
	// It is distinct from the workflow being unable to run for capacity
	// reasons; see AdmissionWait.
	PlacementWaiting ExecutionPlacementState = "waiting"
	// PlacementPreparing means AO is materialising it right now (git worktree
	// add, or acquiring the branch lock).
	PlacementPreparing ExecutionPlacementState = "preparing"
	// PlacementReady means the checkout and the branch authority both exist and
	// nothing is running in them yet. This is the only state admission accepts
	// as "placement is ready to launch into", together with active.
	PlacementReady ExecutionPlacementState = "ready"
	// PlacementActive means an agent runtime may be writing to it.
	PlacementActive ExecutionPlacementState = "active"
	// PlacementReviewing means work has stopped and the placement is being read
	// by a reviewer. Still not removable: a review reads the checkout.
	PlacementReviewing ExecutionPlacementState = "reviewing"
	// PlacementIntegrating means the work is being moved onto the merge target.
	PlacementIntegrating ExecutionPlacementState = "integrating"
	// PlacementIntegrated means the work is durably on the merge target. This
	// is the state that authorizes cleanup, and only alongside a recorded
	// IntegratedSHA.
	PlacementIntegrated ExecutionPlacementState = "integrated"
	// PlacementConflict means integration could not complete and a human or a
	// later pass has to resolve it. Never cleaned up.
	PlacementConflict ExecutionPlacementState = "conflict"
	// PlacementPreserved is the durable refusal to clean up: work that never
	// landed, kept on purpose.
	PlacementPreserved ExecutionPlacementState = "preserved"
	// PlacementTerminal means this placement is finished with and its physical
	// leftovers, if any, have been dealt with or handed to a newer generation.
	PlacementTerminal ExecutionPlacementState = "terminal"
)

// IsKnown reports whether the value is one this build understands.
func (s ExecutionPlacementState) IsKnown() bool {
	switch s {
	case PlacementSelected, PlacementWaiting, PlacementPreparing, PlacementReady,
		PlacementActive, PlacementReviewing, PlacementIntegrating, PlacementIntegrated,
		PlacementConflict, PlacementPreserved, PlacementTerminal:
		return true
	default:
		return false
	}
}

// Terminal reports whether nothing further will move this placement. A
// terminal placement is never the current authority for a launch.
func (s ExecutionPlacementState) Terminal() bool {
	switch s {
	case PlacementIntegrated, PlacementPreserved, PlacementTerminal:
		return true
	default:
		return false
	}
}

// PermitsLaunch reports whether a runtime may be launched into this placement.
//
// `selected` is included, and that is a statement about AO's architecture
// rather than a relaxation. AO materialises an isolated checkout as PART of the
// launch -- the spawn path creates the worktree -- so a freshly frozen
// placement is exactly the state a first launch is supposed to find. Requiring
// `ready` first would deadlock every isolated task against a preparation step
// that only the launch performs.
//
// `preparing` is excluded for the opposite reason: it means another pass is
// materialising this placement right now, and a second launch into it is the
// duplicate the state exists to prevent. Every state from `reviewing` onward is
// excluded because the work is over: launching a worker into a placement being
// integrated would write into a checkout whose contents are already being
// merged.
func (s ExecutionPlacementState) PermitsLaunch() bool {
	switch s {
	case PlacementSelected, PlacementReady, PlacementActive:
		return true
	default:
		return false
	}
}

// PlacementProvenance records HOW a placement record came to exist. It is
// stored because the two origins carry different amounts of proof, and a
// reader deciding whether to trust a placement needs to know which it has.
type PlacementProvenance string

const (
	// PlacementFrozenAtSelection is the ordinary case: the placement was
	// computed from project policy and frozen before any mutation.
	PlacementFrozenAtSelection PlacementProvenance = "frozen_at_selection"
	// PlacementRecoveredFromDurableFacts is a legacy run whose placement was
	// reconstructed from durable evidence that PROVES the mode -- an existing
	// TaskWorktreeRecord (isolated) or a held branch lock (direct). Nothing is
	// fabricated: a legacy run with neither is left without a placement and
	// fails closed rather than being assigned a guess.
	PlacementRecoveredFromDurableFacts PlacementProvenance = "recovered_from_durable_facts"
)

// ExecutionPlacement is the frozen, durable execution placement of one task.
//
// Identity is (WorkflowRunID, TaskID, WorkflowStepID, PlacementGeneration).
// The first three name the obligation; the generation names which physical
// placement is currently discharging it.
type ExecutionPlacement struct {
	ID string
	// WorkflowRunID, TaskID and WorkflowStepID place the record. TaskID is
	// empty for a plain task run that has no planned-task row; WorkflowStepID
	// is empty for a placement frozen at the task level.
	WorkflowRunID  string
	TaskID         string
	WorkflowStepID string
	ProjectID      string

	// PlacementGeneration is this placement's OWN generation, independent of
	// the task's lifecycle generation. It advances only when the physical
	// placement is replaced.
	PlacementGeneration int64
	// LifecycleGeneration is the task/step generation the placement was frozen
	// under, recorded so a reader can see which obligation minted it. A
	// provider failover does NOT advance it, which is the distinction §F
	// exists to hold.
	LifecycleGeneration int64

	// Type is the frozen placement. After the freeze this is the authority;
	// project config is not consulted again.
	Type ExecutionPlacementType

	// RepoPath is the repository identity: the user's own checkout the
	// placement was cut from, or the one the direct branch lives in.
	RepoPath string
	// BaseBranch and BaseSHA pin what the work started from. BaseSHA is what
	// makes "what did this task actually change" answerable after the target
	// has moved.
	BaseBranch string
	BaseSHA    string
	// ExecutionBranch is the branch the agent writes to: the ao/* branch for an
	// isolated placement, the configured branch itself for a direct one.
	ExecutionBranch string
	// WorktreePath is the isolated checkout's directory. Always empty for a
	// direct-branch placement -- there is no AO worktree, and recording one
	// would be fabricating an identity.
	WorktreePath string
	// WorktreeRecordID is the durable TaskWorktreeRecord identity (its task id)
	// when one exists, so the placement points at the worktree record rather
	// than re-deriving it from a path.
	WorktreeRecordID string
	// MergeTarget is where the work is ultimately meant to land.
	MergeTarget string

	// OwnerToken names the daemon instance that froze this placement, which is
	// what makes restart reconciliation decidable without guessing from
	// timestamps. It is AO's own local identifier and carries no secret.
	OwnerToken string

	State      ExecutionPlacementState
	Provenance PlacementProvenance
	// WaitingReason explains a `waiting` state in the admission vocabulary.
	WaitingReason string
	// IntegratedSHA is the commit this placement's work landed at, copied from
	// the integration audit record. It is the authorization for cleanup.
	IntegratedSHA string
	// Detail explains a conflict, a preservation, or a fail-closed state.
	Detail string

	CreatedAt   time.Time
	UpdatedAt   time.Time
	FinalizedAt *time.Time
}

// Valid reports whether the record is internally consistent enough to be an
// authority. It is applied before a freeze and after a read, so a row that
// cannot be trusted is refused rather than acted on.
func (p ExecutionPlacement) Valid() bool {
	if p.WorkflowRunID == "" || p.PlacementGeneration <= 0 || !p.Type.IsKnown() || !p.State.IsKnown() {
		return false
	}
	if p.RepoPath == "" || p.ExecutionBranch == "" {
		return false
	}
	// A direct-branch placement naming a worktree is describing something AO
	// did not create, and is refused rather than normalized. The converse is
	// deliberately NOT symmetric: an isolated placement is frozen before its
	// checkout exists, so an empty path at that point is the truth. ReadyValid
	// below is what refuses to call such a placement ready.
	if !p.Type.Isolated() && p.WorktreePath != "" {
		return false
	}
	return true
}

// ReadyValid reports whether this placement may be reported as ready to launch
// into: everything Valid requires, plus the physical facts that only exist once
// it has been materialised. An isolated placement with no worktree path is a
// plan, not a checkout, and admitting a launch into it would be admitting a
// launch into a directory that is not there.
func (p ExecutionPlacement) ReadyValid() bool {
	if !p.Valid() {
		return false
	}
	if p.Type.Isolated() && p.WorktreePath == "" {
		return false
	}
	return true
}

// SamePlacement reports whether two records describe the same physical
// placement, ignoring generation and mutable state. It is what lets a re-freeze
// be recognised as idempotent rather than as a replacement.
func (p ExecutionPlacement) SamePlacement(other ExecutionPlacement) bool {
	return p.Type == other.Type &&
		p.RepoPath == other.RepoPath &&
		p.ExecutionBranch == other.ExecutionBranch &&
		p.WorktreePath == other.WorktreePath &&
		p.MergeTarget == other.MergeTarget
}
