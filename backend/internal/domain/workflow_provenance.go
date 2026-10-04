package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// workflow_provenance.go — the durable vocabulary for three facts that used to
// exist only as free text inside a checkpoint's retry_state JSON: what happened
// at a dispatch boundary, who changed a workspace, and what a verify attempt is
// actually verifying. See migration 0133 for why each one got columns.

// WorkflowDispatchPhase names a boundary AO crosses when it launches something.
//
// The values mirror the durable_phase strings the coordinator already writes to
// workflow_checkpoints, so a dispatch checkpoint and the checkpoint it was
// recorded alongside say the same word for the same event. The type is
// deliberately open: an unrecognised phase read from an older or newer build
// keeps its own name instead of collapsing to a neighbouring one.
type WorkflowDispatchPhase string

const (
	// DispatchPhaseWorkerLaunchIntent is the boundary AO records BEFORE it
	// invokes any launcher: this run/step/attempt is about to be launched,
	// under this outbox command, aimed at this workspace. It is written first
	// precisely because it is the only record that can exist if the process
	// dies during the launch — an intent with no matching confirmation is how
	// a restart knows a launch was started and never proven.
	DispatchPhaseWorkerLaunchIntent WorkflowDispatchPhase = "worker_launch_intent"
	// DispatchPhaseWorkerDispatched is a worker launch AO can prove completed.
	DispatchPhaseWorkerDispatched WorkflowDispatchPhase = "worker_dispatched"
	// DispatchPhaseWorkerLaunchUnconfirmed is the launch the launcher reported
	// as successful and AO could not durably confirm, because the confirmation
	// write itself failed.
	//
	// It is a phase of its own rather than either neighbour because collapsing
	// it into one of them is wrong in both directions: read as success, AO
	// would treat an unrecorded worker as running; read as failure, AO would
	// retry a launch that may well have started an agent, and put two of them
	// on one worktree.
	DispatchPhaseWorkerLaunchUnconfirmed WorkflowDispatchPhase = "worker_launch_unconfirmed"
	// DispatchPhaseWorkerLaunchError is a worker launch that failed, carrying
	// the classification and the runtime's own words.
	DispatchPhaseWorkerLaunchError WorkflowDispatchPhase = "worker_launch_error"
	// DispatchPhaseWorkerDispatchReconciled is the boundary crash/restart
	// reconciliation records when it RESOLVES a contradiction between an
	// attempt/step that says it is in flight and the launch evidence under it.
	//
	// It is written into the same append-only dispatch history as every other
	// boundary, and for the same reason: the resolution of a launch is part of
	// that launch's story. It is also what makes reconciliation idempotent —
	// while it is the newest boundary for a step, the contradiction it names has
	// already been answered, so a duplicate wake finds nothing left to do.
	DispatchPhaseWorkerDispatchReconciled WorkflowDispatchPhase = "worker_dispatch_reconciled"
	// DispatchPhaseWorkerLaunchHumanRetry is a person reopening a durably
	// failed worker dispatch.
	DispatchPhaseWorkerLaunchHumanRetry WorkflowDispatchPhase = "worker_launch_human_retry"
	// DispatchPhaseReviewDispatched is a reviewer launch AO can prove completed.
	DispatchPhaseReviewDispatched WorkflowDispatchPhase = "review_dispatched"
	// DispatchPhaseReviewLaunchError is a reviewer launch that failed.
	DispatchPhaseReviewLaunchError WorkflowDispatchPhase = "reviewer_launch_error"
)

// WorkflowLaunchStage says how far a launch got before it stopped. It is
// recorded because "the owner's provider env could not be resolved" and "the
// agent process would not start" are different problems that can carry the same
// error class.
type WorkflowLaunchStage string

const (
	// LaunchStageIntent is the stage at which nothing has been invoked yet:
	// the intent record is being written. A failure here is the one failure
	// that is certain to have created nothing at all, because nothing had been
	// called.
	LaunchStageIntent WorkflowLaunchStage = "intent"
	// LaunchStagePreflight is a launch refused BEFORE anything was spawned. It
	// is the earliest stage there is at which a launcher was consulted, and the
	// last one at which AO can be certain nothing was created.
	LaunchStagePreflight WorkflowLaunchStage = "preflight"
	// LaunchStageRuntimeEnv is a failure resolving the runtime environment.
	LaunchStageRuntimeEnv WorkflowLaunchStage = "runtime_env"
	// LaunchStageSpawn is a failure in the spawn itself.
	LaunchStageSpawn WorkflowLaunchStage = "spawn"
	// LaunchStageConfirm is the stage AFTER the launcher answered: AO is
	// recording what it observed. A failure here says nothing about whether the
	// agent started — only that AO could not write down that it had.
	LaunchStageConfirm WorkflowLaunchStage = "confirm"
)

// WorkflowLaunchOutcome is what a dispatch boundary concluded.
type WorkflowLaunchOutcome string

const (
	// LaunchOutcomeIntended means the launch has been recorded and not yet
	// attempted. It concludes nothing, and it is the one outcome that must
	// never be read as progress: an intent is a statement about AO, not about
	// any agent.
	LaunchOutcomeIntended WorkflowLaunchOutcome = "intended"
	// LaunchOutcomeDispatched means AO can prove the launch completed.
	LaunchOutcomeDispatched WorkflowLaunchOutcome = "dispatched"
	// LaunchOutcomeUnconfirmed means the launcher reported success and AO could
	// not durably record it. Unlike LaunchOutcomeAmbiguous, AO does hold the
	// launch evidence — a session identity, and whatever ownership proof came
	// with it; what it could not do is persist the confirmation.
	LaunchOutcomeUnconfirmed WorkflowLaunchOutcome = "unconfirmed"
	// LaunchOutcomeFailed means AO can prove the launch did not complete.
	LaunchOutcomeFailed WorkflowLaunchOutcome = "failed"
	// LaunchOutcomeAmbiguous means AO cannot prove either way.
	//
	// It is a first-class value rather than a missing one because it is the
	// only outcome that forbids a retry: relaunching something that may already
	// be running is how one step becomes two agents on one worktree.
	LaunchOutcomeAmbiguous WorkflowLaunchOutcome = "ambiguous"
)

// Proven reports whether this outcome is one AO observed rather than inferred.
// An ambiguous outcome is not proven and must never be retried as if it were.
func (o WorkflowLaunchOutcome) Proven() bool {
	return o == LaunchOutcomeDispatched || o == LaunchOutcomeFailed
}

// LicensesRunning reports whether this outcome is the one — and it is exactly
// one — that permits an attempt and its step to be treated as RUNNING.
//
// Intended, unconfirmed, ambiguous and failed all mean the same thing to this
// question: AO has not proven an agent is running, so nothing may say it is.
func (o WorkflowLaunchOutcome) LicensesRunning() bool {
	return o == LaunchOutcomeDispatched
}

// WorkflowDispatchCheckpoint is one durable record of a dispatch boundary:
// which run/step/attempt was being launched, under which outbox command, and
// what the launch evidence was.
//
// Append-only. Advancing means inserting another row; a row is never updated,
// so the sequence of rows for one step is the launch history of that step.
type WorkflowDispatchCheckpoint struct {
	ID             string
	WorkflowRunID  string
	WorkflowStepID *string
	AttemptID      *string
	// CheckpointID links to the workflow_checkpoints row written alongside this
	// dispatch, when there was one. Nil when the checkpoint write is what
	// failed, which is itself worth being able to see.
	CheckpointID *string
	Phase        WorkflowDispatchPhase
	// IdempotencyKey is the outbox key the dispatch was made under — the tie
	// between a launch and the exact command that asked for it.
	IdempotencyKey string
	Harness        string
	SessionID      *string
	LaunchStage    WorkflowLaunchStage
	LaunchOutcome  WorkflowLaunchOutcome
	ErrorClass     WorkflowErrorClass
	// EvidenceJSON is the runtime's own words plus whatever else was observed
	// at the boundary. Defaults to "{}" so a read never decodes an empty string.
	EvidenceJSON string
	Detail       string

	// Branch and WorktreePath say where the launch was aimed. They are what
	// joins a dispatch to the WorkflowMutationProvenance rows for the same
	// tree: without them, "this boundary" and "this change" are two facts
	// about the same workspace that cannot be put next to each other.
	Branch       string
	WorktreePath string
	// BaseSHA is the commit the launch was authorized against
	// (SessionMetadata.DiffBaseSHA), which is what keeps a later diff honest
	// once the target branch has moved.
	BaseSHA string
	// WorkspaceFingerprint is the tree as it stood at the boundary, so a later
	// "the worker changed nothing" is decided against what the workspace
	// actually looked like when the worker started.
	WorkspaceFingerprint string

	// RuntimeHandleID, RuntimeLaunchID and AgentSessionID are the process and
	// session ownership evidence for the launch: the runtime instance AO
	// created, the launch generation that fences it
	// (ports.SupervisedProcessRef), and the harness's own session identity.
	//
	// The launch id is the one that decides a retry. SessionID survives a
	// daemon restart while the process behind it does not, so "a session row
	// exists" is not "the process AO started is alive"; only the generation
	// fence separates those, and getting it wrong is how one step becomes two
	// agents on one worktree.
	RuntimeHandleID string
	RuntimeLaunchID string
	AgentSessionID  string

	// LaunchedAt is when the launch itself happened, which is not CreatedAt.
	// Nil whenever the writer cannot honestly say — a preflight or runtime_env
	// failure describes a launch that never happened at all, and every row
	// written before migration 0134 has no recorded launch instant. It is
	// never defaulted to CreatedAt: that would turn "the row was written then"
	// into "the process started then".
	LaunchedAt *time.Time
	// CreatedAt is when this record was written.
	CreatedAt time.Time
}

// WorkflowMutationClass names whose change a workspace mutation is.
//
// It is the durable form of workflow.WorkspaceProvenanceClass, and carries the
// same seven values; the workflow package holds the classification logic and
// this package holds the vocabulary the storage layer persists, because domain
// cannot import workflow. The two must stay in step — a value added there and
// not here would be persisted as an unknown string.
type WorkflowMutationClass string

const (
	// MutationAuthorizedWork means this run's own work step produced it, in
	// this run's own worktree, on this run's own branch.
	MutationAuthorizedWork WorkflowMutationClass = "AUTHORIZED_WORK"
	// MutationAuthorizedFix means this run's own fix step produced it, under a
	// fix cycle AO itself dispatched.
	MutationAuthorizedFix WorkflowMutationClass = "AUTHORIZED_FIX"
	// MutationPreexisting means the difference was already there when the task
	// was dispatched.
	MutationPreexisting WorkflowMutationClass = "PREEXISTING"
	// MutationOtherAOTask means another AO task owns this worktree or branch.
	MutationOtherAOTask WorkflowMutationClass = "OTHER_AO_TASK"
	// MutationExternal means the worktree or branch is not one this run was
	// authorized to work in at all.
	MutationExternal WorkflowMutationClass = "EXTERNAL"
	// MutationConflicting means the history AO certified is no longer
	// reachable — an amend, a reset or a rebase dropped it.
	MutationConflicting WorkflowMutationClass = "CONFLICTING"
	// MutationUnknown means AO cannot attribute the change. The default, and
	// the honest answer whenever any required fact could not be read.
	MutationUnknown WorkflowMutationClass = "UNKNOWN"
)

// Authorized reports whether this class names a change AO itself caused through
// its own dispatched agents. It is the only class family that may lead to a
// fresh review instead of a stop.
func (c WorkflowMutationClass) Authorized() bool {
	return c == MutationAuthorizedWork || c == MutationAuthorizedFix
}

// IsKnown reports whether the value is one this build understands. A value read
// from a newer build is preserved as-is and reported unknown rather than being
// silently rewritten to UNKNOWN.
func (c WorkflowMutationClass) IsKnown() bool {
	switch c {
	case MutationAuthorizedWork, MutationAuthorizedFix, MutationPreexisting,
		MutationOtherAOTask, MutationExternal, MutationConflicting, MutationUnknown:
		return true
	default:
		return false
	}
}

// WorkflowMutationProvenance is one durable record of an AO-owned mutation of a
// workspace: who made it, where, from what, to what, and why.
//
// The record exists because "the fingerprint moved" is equally true of the
// authorized fix worker's own output and of a stranger editing the directory,
// and verification has to tell those apart. Every field is something AO
// observed or holds a durable record of; nothing here is an agent's word for
// anything.
//
// Append-only, and never backfilled: a run that predates migration 0133 has no
// provenance rows at all, and reads back that way rather than being given an
// invented history.
type WorkflowMutationProvenance struct {
	ID             string
	WorkflowRunID  string
	WorkflowStepID *string
	AttemptID      *string
	// TaskID is the planned task the mutation belongs to, empty when the run
	// has no task decomposition.
	TaskID string
	Class  WorkflowMutationClass
	// Harness and SessionID identify the agent AO dispatched, so an authorized
	// change can be attributed to the exact session that was authorized.
	Harness      string
	SessionID    *string
	Branch       string
	WorktreePath string
	// BaseSHA is the commit the work was authorized against; HeadSHA is where
	// the branch actually ended up. Both are needed for a later diff to stay
	// honest once the target branch has moved.
	BaseSHA string
	HeadSHA string
	// FingerprintBefore and FingerprintAfter are the workspace fingerprints
	// either side of the mutation.
	FingerprintBefore string
	FingerprintAfter  string
	// Reason says why this mutation happened: the dispatch that caused it, the
	// reviewer finding it answers, the human action that authorized it.
	Reason string
	// EvidenceJSON carries whatever else was observed. Defaults to "{}".
	EvidenceJSON string
	// ObservedAt is when the mutation itself was observed, which is not always
	// when the row was written. Nil when the writer could not honestly say —
	// substituting its own clock would turn a gap in knowledge into a claim.
	ObservedAt *time.Time
	CreatedAt  time.Time

	// --- P2-D: what a memory promotion has to be able to prove -----------
	//
	// Everything above was enough for the verification path, which only ever
	// asked "whose change is this" about a workspace it was already standing
	// in. A memory promotion asks a harder question from further away — "did
	// this task's work reach the repository's own history, and is that still
	// the same repository" — and the fields below are what make it answerable
	// from a row rather than from an inference.

	// ProjectID, RepoIdentity and RepoPath say WHICH repository. A branch name
	// is not a repository: two projects can both have `main`, and one project
	// can be re-checked-out at a path another used to occupy. RepoIdentity is
	// the durable signal (see RepoIdentity); RepoPath is explanatory.
	ProjectID    string
	RepoIdentity RepoIdentity
	RepoPath     string

	// Placement is how the work was placed, and Boundary is which semantic
	// moment this row records. Together they decide which promotion proof
	// applies: an isolated worktree's `work_result` licenses nothing, and its
	// `integrated` row licenses everything.
	Placement WorkflowMutationPlacement
	Boundary  WorkflowMutationBoundary

	// Generation fences the writer. A stale worker, reviewer or repair
	// callback that wakes up after a newer generation has recorded the same
	// boundary must not append a row that later reads as the current one.
	Generation int64

	// Where an integration landed. HeadSHA (above) stays what it always was:
	// where the SOURCE of the mutation ended up. These three are the other
	// end, and a promotion that cannot name IntegrationTargetAfterSHA has not
	// proven integration.
	IntegrationTargetRef       string
	IntegrationTargetBeforeSHA string
	IntegrationTargetAfterSHA  string
	// IntegrationMethod is how the source reached the target. It matters
	// because cherry-pick produces a different SHA for the same content, so
	// "is the source commit reachable from the target" is the right ancestry
	// question for merge and fast-forward and the wrong one for cherry-pick.
	IntegrationMethod WorkflowIntegrationMethod

	// IdempotencyKey is derived from the facts of the boundary itself, so a
	// duplicate callback and a restart-after-crash derive the SAME key and
	// produce ONE row. Empty when the writer honestly could not derive one,
	// and empty keys do not collide with each other.
	IdempotencyKey string
}

// WorkflowMutationPlacement is where a mutation's work was placed. The two
// values have entirely different promotion proofs, which is why a row that
// does not say which it was can supply neither.
type WorkflowMutationPlacement string

// Workflow mutation placements.
const (
	// MutationPlacementDirectBranch is work committed straight onto the branch
	// the repository's own checkout is on.
	MutationPlacementDirectBranch WorkflowMutationPlacement = "direct_branch"
	// MutationPlacementIsolatedWorktree is work committed to a branch of its
	// own in an AO-owned worktree. Nothing about it is in the repository's
	// history until an integration boundary says so.
	MutationPlacementIsolatedWorktree WorkflowMutationPlacement = "isolated_worktree"
)

// Valid reports whether the placement is one this build writes.
func (p WorkflowMutationPlacement) Valid() bool {
	return p == MutationPlacementDirectBranch || p == MutationPlacementIsolatedWorktree
}

// WorkflowMutationBoundary names the semantic moment a provenance row records.
//
// AO records BOUNDARIES, not write syscalls (P2-D §5). A boundary is a moment
// at which what some durable ref contains changed in a way a later reader must
// be able to attribute — and there are only five of them, because there are
// only five moments at which the answer to "may this become project
// knowledge" can change.
type WorkflowMutationBoundary string

// Workflow mutation boundaries.
const (
	// BoundaryDispatch is the authority a mutation starts from: the tree and
	// head AO handed an agent. It proves what was there BEFORE, which is what
	// makes a later difference attributable at all.
	BoundaryDispatch WorkflowMutationBoundary = "dispatch"
	// BoundaryWorkResult is what a worker produced, observed after it finished.
	BoundaryWorkResult WorkflowMutationBoundary = "work_result"
	// BoundaryRepairResult is what a repair agent produced on top of a worker's
	// result. It is a separate boundary and not another work_result because
	// attribution must not collapse into the original worker (P2-D §16).
	BoundaryRepairResult WorkflowMutationBoundary = "repair_result"
	// BoundaryVerified is the head verification actually passed on. It is the
	// strongest thing AO can say about a branch, and it is still not
	// integration.
	BoundaryVerified WorkflowMutationBoundary = "verified"
	// BoundaryIntegrated is the moment the work became part of a target ref's
	// history. It is the ONLY boundary that licenses canonical project
	// knowledge for isolated-worktree work.
	BoundaryIntegrated WorkflowMutationBoundary = "integrated"
)

// Valid reports whether the boundary is one this build writes.
func (b WorkflowMutationBoundary) Valid() bool {
	switch b {
	case BoundaryDispatch, BoundaryWorkResult, BoundaryRepairResult, BoundaryVerified, BoundaryIntegrated:
		return true
	default:
		return false
	}
}

// WorkflowIntegrationMethod is how a source branch's work reached its target.
type WorkflowIntegrationMethod string

// Workflow integration methods.
const (
	// IntegrationMerge created a merge commit. The source head is an ancestor
	// of the target head.
	IntegrationMerge WorkflowIntegrationMethod = "merge"
	// IntegrationFastForward moved the target ref to the source head. The
	// source head IS the target head.
	IntegrationFastForward WorkflowIntegrationMethod = "fast_forward"
	// IntegrationCherryPick replayed the source's changes onto the target,
	// producing DIFFERENT commit SHAs for the same content. Ancestry cannot
	// prove it, so a cherry-pick promotion needs the recorded target SHAs and
	// nothing weaker — "the files look the same" is explicitly not proof
	// (P2-D §13).
	IntegrationCherryPick WorkflowIntegrationMethod = "cherry_pick"
	// IntegrationDirectCommit is direct-branch work: there was no separate
	// integration, because the commit landed on the target ref itself.
	IntegrationDirectCommit WorkflowIntegrationMethod = "direct_commit"
)

// Valid reports whether the method is one this build writes.
func (m WorkflowIntegrationMethod) Valid() bool {
	switch m {
	case IntegrationMerge, IntegrationFastForward, IntegrationCherryPick, IntegrationDirectCommit:
		return true
	default:
		return false
	}
}

// AncestryProves reports whether, for this method, the source head being an
// ancestor of the integrated target head is the correct proof of integration.
//
// It is false for cherry-pick alone, and that single exception is the reason
// this is a method rather than an if-statement at the call site: a caller that
// forgot it would silently refuse every legitimate cherry-pick promotion, or
// — worse, if it inverted the check — accept every illegitimate one.
func (m WorkflowIntegrationMethod) AncestryProves() bool {
	return m == IntegrationMerge || m == IntegrationFastForward || m == IntegrationDirectCommit
}

// MutationIdempotencyKey derives the key that makes a boundary record
// exactly-once across duplicate callbacks and restarts.
//
// Every part is a fact of the BOUNDARY, never of the writing: no clock, no
// row id, no attempt counter. Two writers describing the same moment
// therefore derive the same key and the unique index collapses them to one
// row; two writers describing different moments cannot collide however close
// together they run.
func MutationIdempotencyKey(
	runID, taskID string, boundary WorkflowMutationBoundary, generation int64, headSHA, targetSHA string,
) string {
	h := sha256.New()
	for _, part := range []string{
		strings.TrimSpace(runID),
		strings.TrimSpace(taskID),
		string(boundary),
		fmt.Sprintf("%d", generation),
		strings.ToLower(strings.TrimSpace(headSHA)),
		strings.ToLower(strings.TrimSpace(targetSHA)),
	} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return "wmp_" + hex.EncodeToString(h.Sum(nil))[:32]
}

// WorkflowReviewTarget is the reviewed artifact a verify attempt is judging.
//
// Verification is only meaningful against the exact thing a reviewer approved.
// Before this was durable, the linkage lived in the review step's MUTABLE
// review_run_id plus a fingerprint buried in a checkpoint's JSON, so a
// re-review could move the target out from under an in-flight verification and
// nothing recorded that it had.
type WorkflowReviewTarget struct {
	// ReviewRunID is the review run whose verdict is being verified against.
	ReviewRunID *string
	// Fingerprint is the workspace fingerprint that review approved.
	Fingerprint string
	// HeadSHA is the commit that review read.
	HeadSHA string
}

// Empty reports whether no review target was ever recorded. True for every
// attempt written before migration 0133 and for every attempt kind that has no
// review target — which is why it is the zero value and not an error.
func (t WorkflowReviewTarget) Empty() bool {
	return t.ReviewRunID == nil && t.Fingerprint == "" && t.HeadSHA == ""
}

// WorkflowVerifyWindow is the timing envelope of one verify attempt: when it
// started, when it concluded, and when it had to conclude by.
//
// DeadlineAt is nil when none was recorded, and is never derived from a default
// duration: "no deadline was recorded" and "the deadline was the default" are
// different facts, and only the first is true of a legacy row.
type WorkflowVerifyWindow struct {
	StartedAt  time.Time
	FinishedAt *time.Time
	DeadlineAt *time.Time
}

// OverdueAt reports whether the window had blown its deadline at the given
// instant. An attempt with no recorded deadline is never overdue.
func (w WorkflowVerifyWindow) OverdueAt(now time.Time) bool {
	if w.DeadlineAt == nil {
		return false
	}
	if w.FinishedAt != nil {
		return w.FinishedAt.After(*w.DeadlineAt)
	}
	return now.After(*w.DeadlineAt)
}
