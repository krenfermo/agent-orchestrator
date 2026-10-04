package domain

import "time"

// repair_artifact.go — what a repair is a repair OF.
//
// THE INCIDENT (wf-f951be84 -> wf-3af3c533). A task produced a 2,444-line
// implementation on its own branch, a reviewer returned changes_requested with
// two concrete findings, and the fix worker terminated after 77 seconds having
// changed nothing. AO did the right thing next and launched a repair — and the
// repair run was created as an ordinary top-level task run, so it was given a
// fresh worktree cut from the project's default branch. That checkout was
// byte-identical to origin/main. It did not contain the file either finding was
// about, or any of the work under review.
//
// The repair worker therefore could not have repaired anything, correctly
// changed nothing, and AO parked the run on `worker_turn_produced_nothing` —
// a sentence that blames the worker for a workspace AO chose.
//
// THE MISSING CONCEPT. A repair is not a new independent task. It is bounded
// work against an artifact that already exists, and the identity of that
// artifact — which run, which task, which branch, which commit, which review's
// findings — is not something to infer later from project configuration. It is
// authority, and authority is frozen before anything is created from it.
//
// This type is that authority. It is resolved once, before the repair run
// exists, from durable facts only; it is written to the ledger before the
// repair run is started; and every later question ("what should this checkout
// be cut from", "did the worker see the right code", "where does the repaired
// candidate go back to") is answered by reading it rather than by deriving a
// fresh answer that could differ.
//
// WHAT IS DELIBERATELY NOT HERE. No prompt text, no provider output, no paths
// outside the project, and no findings body — only a digest and a count of the
// findings, so "the same findings travelled" is checkable without this record
// becoming a second copy of them.

// RepairArtifactRefusal is the closed set of reasons AO will not repair.
//
// Each one is a statement about AO's own ability to establish the artifact, and
// none of them is a statement about the worker. That distinction is the whole
// point: `worker_turn_produced_nothing` must never be the label on a repair
// that was pointed at the wrong tree.
type RepairArtifactRefusal string

const (
	// RepairArtifactUnavailable means AO could not establish which artifact is
	// under repair at all: no placement, no branch, no committed head, or a
	// repository it cannot read. It is the fail-closed default.
	RepairArtifactUnavailable RepairArtifactRefusal = "repair_artifact_unavailable"
	// RepairArtifactUncommitted means the artifact exists but only as
	// uncommitted state in the origin's own worktree. A second checkout cannot
	// be cut from work that is not a commit, and cutting one from the last
	// commit would hand the repairer a tree missing the very change under
	// review.
	RepairArtifactUncommitted RepairArtifactRefusal = "repair_artifact_uncommitted"
	// RepairWorkspaceMismatch means the repair's checkout was materialised and
	// does NOT contain the artifact it was pinned to. It is an AO error about
	// AO's own workspace, recorded as such.
	RepairWorkspaceMismatch RepairArtifactRefusal = "repair_workspace_mismatch"
)

// RepairArtifactSource says how the base commit was established, so a reader
// can tell a live observation from a reconstruction.
type RepairArtifactSource string

const (
	// RepairArtifactObserved means AO read the origin's own worktree and took
	// its committed head.
	RepairArtifactObserved RepairArtifactSource = "observed_worktree"
	// RepairArtifactReconstructed means the origin's worktree is gone and the
	// base came from the durable ledger instead — the commit AO itself recorded
	// for that task. The branch still exists in the repository, so the commit
	// is still reachable and a checkout can still be cut from it.
	RepairArtifactReconstructed RepairArtifactSource = "reconstructed_from_ledger"
	// RepairArtifactSharedCheckout means the origin runs on the user's own
	// checkout (direct_branch). There is nothing to cut: the artifact is the
	// working tree itself, and the branch lock is what moves. See
	// workflow/repair_branch_cession.go.
	RepairArtifactSharedCheckout RepairArtifactSource = "shared_checkout"
	// RepairArtifactNone means the origin has provably never executed
	// anything: no session, no attempt, no worktree, no placement. There is no
	// artifact to be authority over, so the repair legitimately starts from the
	// project's own default branch -- and says so, rather than arriving there
	// by omission. It is the ONLY case in which that base is correct.
	RepairArtifactNone RepairArtifactSource = "no_artifact"
)

// RepairArtifactAuthority is the frozen identity of the thing being repaired.
//
// It travels three ways, all of them durable and all of them written before the
// repair run starts: on the RepairIntent recorded against the origin, on the
// repair run's own origin marker, and (as the base ref) into the launch that
// materialises the repair's checkout.
type RepairArtifactAuthority struct {
	// Resolved is the load-bearing field. False means AO could not establish
	// the artifact, and no repair may be created — never that a default was
	// substituted. Refusal names which refusal.
	Resolved bool                  `json:"resolved"`
	Refusal  RepairArtifactRefusal `json:"refusal,omitempty"`
	// Detail is AO's own sentence about a refusal, for the ledger and the
	// operator. Empty when Resolved.
	Detail string `json:"detail,omitempty"`
	// HasArtifact reports that the origin has actually produced something. It
	// is the difference between "there is nothing to be authority over" (a run
	// that never executed) and "there is, and AO established it". A resolved
	// authority with HasArtifact false is the only shape allowed to be cut from
	// the project default branch.
	HasArtifact bool `json:"hasArtifact,omitempty"`

	// ---- origin identity --------------------------------------------------

	// OriginRunID is the run whose artifact is under repair — the run that
	// actually owns the work, which for a master objective is the affected
	// CHILD and not the objective.
	OriginRunID string `json:"originRunId,omitempty"`
	// OriginTaskID is the planned task the origin run executes, when it has
	// one. It is what places the artifact in its master's plan, and it is the
	// key the worktree records and execution placements are scoped by.
	OriginTaskID string `json:"originTaskId,omitempty"`
	// OriginStepID is the work step whose output the artifact is.
	OriginStepID string `json:"originStepId,omitempty"`
	// OriginSessionID is the worker session that produced it, when AO still
	// holds one.
	OriginSessionID string `json:"originSessionId,omitempty"`
	// OriginProjectID is the project the artifact belongs to. A repair may
	// never be created against any other.
	OriginProjectID string `json:"originProjectId,omitempty"`

	// ---- the review cycle that asked for the repair -----------------------

	// ReviewRunID and ReviewVerdict identify the review whose findings
	// triggered this repair, and what it concluded.
	ReviewRunID   string `json:"reviewRunId,omitempty"`
	ReviewVerdict string `json:"reviewVerdict,omitempty"`
	// ReviewTargetKey is the target the review recorded for itself. It is NOT
	// necessarily a commit: for AO's own workflow reviews it is the workspace
	// FINGERPRINT the reviewer was pointed at (review_dispatch.go), and only a
	// PR review records a commit there. It is carried for provenance and it is
	// never compared against BaseSHA -- doing so would report drift on every
	// repair AO has ever launched.
	ReviewTargetKey string `json:"reviewTargetKey,omitempty"`
	// FindingsDigest/Bytes/Count are the same evidence a fix dispatch records
	// (workflow/fix_findings_evidence.go), so "the repair was given the same
	// findings the fix cycle was" is checkable rather than assumed.
	FindingsDigest string `json:"findingsDigest,omitempty"`
	FindingsBytes  int    `json:"findingsBytes,omitempty"`
	FindingsCount  int    `json:"findingsCount,omitempty"`

	// ---- the fix cycle that failed ----------------------------------------

	// FixStepID is the fix step whose worker did not deliver, when the repair
	// follows a failed fix rather than a failed verification.
	FixStepID string `json:"fixStepId,omitempty"`
	// FixCycle is that cycle's number, so a retry can be told from a new cycle.
	FixCycle int `json:"fixCycle,omitempty"`

	// ---- workspace authority ----------------------------------------------

	// Placement is the origin's FROZEN execution placement type. It decides
	// which repair workspace policy applies: an isolated artifact is cut fresh
	// from BaseSHA, a direct-branch one is the user's checkout and moves by
	// branch-lock cession instead.
	Placement ExecutionPlacementType `json:"placement,omitempty"`
	// PlacementGeneration pins which generation of that placement the artifact
	// belongs to, so a placement transition under a repair is detectable.
	PlacementGeneration int64 `json:"placementGeneration,omitempty"`
	// RepoPath is the repository the artifact lives in — the user's own
	// checkout the origin worktree was cut from.
	RepoPath string `json:"repoPath,omitempty"`
	// OriginBranch is the branch carrying the artifact: the ao/* execution
	// branch for an isolated placement, the configured branch for a direct one.
	//
	// It is recorded for provenance and for the promotion path, NEVER as the
	// base a repair checkout is cut from. A branch name is a moving target;
	// BaseSHA is not. See BaseRef.
	OriginBranch string `json:"originBranch,omitempty"`
	// OriginWorktreePath and WorktreeRecordID name the origin's own checkout,
	// so promotion knows where the repaired candidate has to land and cleanup
	// knows which record it belongs to. Empty for direct_branch.
	OriginWorktreePath string `json:"originWorktreePath,omitempty"`
	WorktreeRecordID   string `json:"worktreeRecordId,omitempty"`
	// MergeTarget is where the origin's work was always meant to land. A repair
	// inherits it unchanged: a repair changes where work HAPPENS, never what it
	// is for.
	MergeTarget string `json:"mergeTarget,omitempty"`

	// ---- the commit itself ------------------------------------------------

	// BaseSHA is THE artifact: the exact commit a repair checkout is cut from.
	// It is a full object id and never a name.
	BaseSHA string `json:"baseSha,omitempty"`
	// Source says how BaseSHA was established.
	Source RepairArtifactSource `json:"source,omitempty"`
	// ChangedFiles is a bounded list of the paths the artifact changes relative
	// to its own base, for the repair's context pack. Evidence, never a fence.
	ChangedFiles []string `json:"changedFiles,omitempty"`

	// ---- fencing ----------------------------------------------------------

	// Generation is the repair generation this authority was frozen for. An
	// authority carrying a generation the origin has moved past may not
	// materialise a checkout or promote a candidate.
	Generation int       `json:"generation,omitempty"`
	At         time.Time `json:"at,omitempty"`
}

// BaseRef is what a repair checkout must be cut from, as a ref the workspace
// layer can resolve.
//
// It is the object id and nothing else, deliberately. Handing a branch name
// down would reintroduce the whole defect one level lower: the branch can move,
// be deleted, or be checked out elsewhere between the freeze and the cut, and
// every one of those would silently produce a repair against a different tree.
// Empty when the authority does not pin one, which is the direct-branch case
// and is never a licence to fall back to the project default.
func (a RepairArtifactAuthority) BaseRef() string {
	if !a.Resolved || !a.HasArtifact || a.Placement == PlacementDirectBranch {
		return ""
	}
	return a.BaseSHA
}

// PinsCheckout reports whether this authority requires a checkout cut from a
// specific commit — the case where dispatching without the pin would be the
// original defect.
func (a RepairArtifactAuthority) PinsCheckout() bool {
	return a.Resolved && a.HasArtifact && a.Placement != PlacementDirectBranch && a.BaseSHA != ""
}

// Valid reports whether the record is internally consistent enough to be an
// authority, applied after every read so a row that cannot be trusted is
// refused rather than acted on.
func (a RepairArtifactAuthority) Valid() bool {
	if !a.Resolved || a.OriginRunID == "" || a.OriginProjectID == "" {
		return false
	}
	if !a.HasArtifact {
		return true
	}
	if a.Placement == PlacementDirectBranch {
		return a.RepoPath != "" && a.OriginBranch != ""
	}
	return a.RepoPath != "" && a.BaseSHA != ""
}

// Promotable reports whether a successful repair of this artifact has a
// candidate to promote back into the origin's own lifecycle -- an isolated
// checkout with a known origin worktree and a known base. A direct-branch
// repair writes into the origin's own checkout and needs no promotion; a run
// with no artifact has nothing to promote onto.
func (a RepairArtifactAuthority) Promotable() bool {
	return a.PinsCheckout() && a.OriginWorktreePath != "" && a.OriginBranch != ""
}
