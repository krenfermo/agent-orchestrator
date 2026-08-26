package domain

import "time"

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
