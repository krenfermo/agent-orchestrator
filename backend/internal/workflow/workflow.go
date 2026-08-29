// Package workflow is the durable foundation for Checkpoint 8A (Workflow
// Durable Foundation): creating a workflow run and its initial steps,
// reading/listing/cancelling runs, and reconciling (read-mostly) on daemon
// boot. It deliberately does not launch any agent, execute any step, call
// Session Manager or Review Engine, or auto-advance anything — that is all
// out of scope for this checkpoint.
package workflow

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/integration"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
	"github.com/aoagents/agent-orchestrator/backend/internal/workspace"
)

// ErrInvalid and ErrNotFound let the transport layer map failures to 422/404.
var (
	ErrInvalid  = errors.New("workflow: invalid input")
	ErrNotFound = errors.New("workflow: not found")
	// ErrAlreadyTerminal means a cancel was requested on a run that is already
	// completed, failed, or cancelled. Cancelling a terminal run is a no-op
	// error, not a silent success: it must never flip a completed run back
	// toward running.
	ErrAlreadyTerminal = errors.New("workflow: run is already terminal")
	ErrPlanLocked      = errors.New("workflow: plan is already accepted or generation is in progress")
	// ErrUnrecoverable marks a failure that WILL NOT change on retry: AO asked,
	// got a deterministic answer it refuses to act on, and nothing AO can do by
	// itself makes the next answer different. Today that is the fail-closed
	// runtime-ownership class -- a session AO can neither prove it owns nor
	// prove is gone.
	//
	// It exists for the wake scheduler. Every failure used to be "transient",
	// so a run whose runtime was permanently unclassifiable was re-driven on
	// every wake forever, each attempt producing the identical error and the
	// identical reschedule. Marking the class lets the poller close the wake out
	// and leave the run parked for a person instead of spinning on it all night.
	//
	// It never licenses an action. It only says retrying is pointless.
	ErrUnrecoverable = errors.New("workflow: unrecoverable without a person")
)

// workflowStepPolicyV1 is the fixed Checkpoint 8A seeding policy: a strict
// linear chain of six steps. Later checkpoints may introduce other policy
// versions; this one is the only one implemented here.
var workflowStepPolicyV1 = []domain.WorkflowStepKind{
	domain.WorkflowStepPlan,
	domain.WorkflowStepWork,
	domain.WorkflowStepReview,
	domain.WorkflowStepFix,
	domain.WorkflowStepVerify,
	domain.WorkflowStepAdvance,
}

const policyVersionV1 = "v1"

// Store is the persistence surface the coordinator needs. *sqlite.Store
// satisfies it in production; tests use a fake.
type Store interface {
	CreateWorkflowRun(ctx stdctx.Context, run domain.WorkflowRun, steps []domain.WorkflowStep) (domain.WorkflowRun, []domain.WorkflowStep, error)
	GetWorkflowRun(ctx stdctx.Context, id string) (domain.WorkflowRun, bool, error)
	ListWorkflowRuns(ctx stdctx.Context, projectID string) ([]domain.WorkflowRun, error)
	ListNonTerminalWorkflowRuns(ctx stdctx.Context) ([]domain.WorkflowRun, error)
	// ListWorkflowRunIDsByCheckpointPhase finds runs carrying a given durable
	// phase, TERMINAL ONES INCLUDED. It is what lets boot recovery find external
	// obligations that outlived the workflow that created them.
	ListWorkflowRunIDsByCheckpointPhase(ctx stdctx.Context, phase string) ([]string, error)
	UpdateWorkflowRunState(ctx stdctx.Context, id string, expected, next domain.WorkflowRunState, now time.Time) (bool, error)
	ListWorkflowSteps(ctx stdctx.Context, runID string) ([]domain.WorkflowStep, error)
	UpdateWorkflowStepState(ctx stdctx.Context, id string, expected, next domain.WorkflowStepState, now time.Time) (bool, error)
	ListWorkflowAttempts(ctx stdctx.Context, stepID string) ([]domain.WorkflowAttempt, error)

	// ReopenFailedWorkflowStep moves a terminally FAILED step back to `ready` so
	// a new attempt can be created for it. It is the one and only write in AO
	// that leaves a terminal step state, and it is a separate, narrowly named
	// method rather than a widened UpdateWorkflowStepState precisely so that
	// stays true and greppable: ValidWorkflowStepTransition keeps its "terminal
	// states have zero outgoing transitions" contract unchanged, and this method
	// can never target a step in any other state because the compare-and-swap is
	// hard-coded to expect `failed`.
	//
	// Its only caller is verify_recovery.go's resumeStaleVerifyFailure, which is
	// itself only reachable from an explicit human Continue on a run stopped by a
	// recoverable verification infrastructure failure. Reports false when the
	// step was not (or is no longer) failed, which is what makes repeated calls
	// idempotent rather than racy.
	ReopenFailedWorkflowStep(ctx stdctx.Context, stepID string, now time.Time) (bool, error)

	// ReopenCompletedWorkflowStep moves a COMPLETED step back to `waiting` so its
	// own dispatch path can run one more cycle for it. It is the second — and
	// last — write in AO that leaves a terminal step state, and it is narrow for
	// exactly the same reasons ReopenFailedWorkflowStep is: the compare-and-swap
	// is hard-coded to expect `completed`, so it can never target a step in any
	// other state, and ValidWorkflowStepTransition keeps its "terminal states have
	// zero outgoing transitions" contract unchanged.
	//
	// Its only caller is verify_recovery.go's requestFreshReviewForRecovery, which
	// reopens the REVIEW step of a run already inside an explicitly authorized
	// verification recovery whose approval no longer describes the workspace. It
	// reports false when the step was not (or is no longer) completed, which is
	// what makes repeated calls idempotent rather than racy.
	ReopenCompletedWorkflowStep(ctx stdctx.Context, stepID string, now time.Time) (bool, error)

	// The methods below back Checkpoint 8B's work-step dispatch/observation.
	UpdateWorkflowStepArtifact(ctx stdctx.Context, stepID, artifactJSON string, now time.Time) (bool, error)
	UpdateWorkflowStepSession(ctx stdctx.Context, stepID, sessionID string, now time.Time) (bool, error)
	// ReleaseWorkflowStepReviewRunIfNoLateVerdict clears a review step's
	// authority pointer only while the run it names still has no late verdict —
	// check and release in ONE statement, so a verdict landing in between cannot
	// be orphaned. False means the decision was stale, never that it failed.
	// See workflow/review_authority.go.
	ReleaseWorkflowStepReviewRunIfNoLateVerdict(ctx stdctx.Context, stepID, reviewRunID string, now time.Time) (bool, error)
	// RebindWorkflowStepReviewRunFrom compare-and-swaps the authority pointer.
	// expected "" means "currently unset"; predecessor names the run being
	// replaced (which is not the same thing once reconciliation has released the
	// pointer). It refuses when the step has already RESOLVED, or when the
	// predecessor has produced a verdict — the guard that makes adoption and
	// replacement mutually exclusive rather than merely ordered. False means
	// another owner won, or the step is no longer replaceable.
	RebindWorkflowStepReviewRunFrom(ctx stdctx.Context, stepID, expected, predecessor, next string, now time.Time) (bool, error)
	// UpdateWorkflowStepStateIfReviewRun applies a step transition only while
	// the named review run is still the step's authority — the primitive that
	// makes late-verdict adoption safe against a concurrent replacement.
	UpdateWorkflowStepStateIfReviewRun(ctx stdctx.Context, stepID string, expected, next domain.WorkflowStepState, reviewRunID string, now time.Time) (bool, error)
	CreateWorkflowAttempt(ctx stdctx.Context, id, stepID, harness, model string, startedAt time.Time) (domain.WorkflowAttempt, error)
	GetLatestWorkflowAttempt(ctx stdctx.Context, stepID string) (domain.WorkflowAttempt, bool, error)
	UpdateWorkflowAttemptOutcome(ctx stdctx.Context, attemptID string, finishedAt time.Time, outcome domain.WorkflowAttemptOutcome, errorClass domain.WorkflowErrorClass) error
	// ClaimWorkflowAttemptOutcome is the conditional form: it concludes the
	// attempt only if nothing has concluded it yet, and says whether this
	// caller won. It is what decides which of several concurrent verify
	// executions of one target is allowed to have side effects.
	ClaimWorkflowAttemptOutcome(ctx stdctx.Context, attemptID string, finishedAt time.Time, outcome domain.WorkflowAttemptOutcome, errorClass domain.WorkflowErrorClass) (bool, error)
	CreateWorkflowCheckpoint(ctx stdctx.Context, cp domain.WorkflowCheckpoint) (domain.WorkflowCheckpoint, error)
	ListWorkflowCheckpoints(ctx stdctx.Context, runID string) ([]domain.WorkflowCheckpoint, error)
	GetLatestWorkflowCheckpointByStep(ctx stdctx.Context, stepID string) (domain.WorkflowCheckpoint, bool, error)
	EnqueueWorkflowOutboxEntry(ctx stdctx.Context, entry domain.WorkflowOutboxEntry) (domain.WorkflowOutboxEntry, bool, error)
	UpdateWorkflowOutboxStatus(ctx stdctx.Context, id string, expected, next domain.WorkflowOutboxStatus, now time.Time, errorClass string) (bool, error)
	// FailWorkflowOutboxWithGeneration moves an entry to `failed` and stamps,
	// in the same statement, the identity of the failure that did it. A row is
	// reused across retries, so `failed` alone does not say WHICH failure — the
	// stamp is what a later human resume is allowed to act on.
	// ClaimWorkflowOutboxDispatch takes an entry pending -> dispatched and
	// stamps the token identifying the dispatch that now owns it. The row is
	// reclaimable, so "dispatched" alone never says WHOSE dispatch it is.
	ClaimWorkflowOutboxDispatch(ctx stdctx.Context, id string, now time.Time, dispatchGeneration string) (bool, error)
	// FailWorkflowOutboxWithGeneration moves an entry to `failed`, stamps the
	// identity of the failure that did it, and proves the caller still owns the
	// dispatch — one statement. A dispatch that paused after recording its
	// launch error can wake to find the row dispatched again to somebody else;
	// without the ownership half it would fail that live generation and stamp
	// its own failure onto it.
	FailWorkflowOutboxWithGeneration(ctx stdctx.Context, id string, expected domain.WorkflowOutboxStatus, now time.Time, errorClass, generation, dispatchGeneration string) (bool, error)
	// ReleaseDispatchedWorkflowOutboxGeneration gives one dispatch's claim
	// back: dispatched -> pending for the exact token holding it. False means
	// the caller no longer owns the row — an idempotent no-op, never an error.
	ReleaseDispatchedWorkflowOutboxGeneration(ctx stdctx.Context, id string, errorClass, dispatchGeneration string) (bool, error)
	// ReopenFailedWorkflowOutboxGeneration moves ONE named failed generation
	// back to `pending`. The generation is part of the UPDATE's own predicate,
	// so a resume cannot be handed a row that failed again after it looked.
	// False means zero rows matched: already reopened, or superseded. Both are
	// idempotent no-ops, never errors.
	ReopenFailedWorkflowOutboxGeneration(ctx stdctx.Context, id string, errorClass, generation string) (bool, error)
	// AcknowledgeWorkflowOutboxDispatch completes a dispatch --
	// dispatched -> acknowledged -- for the exact dispatch that holds the
	// claim, and is the fourth ownership-dependent transition off
	// `dispatched`. Without it, RUNNING could be licensed by a confirmation
	// that belongs to a different launch of the same step. An empty
	// generation matches only an unclaimed row, which is what completes the
	// entries written before the worker path claimed with a token.
	AcknowledgeWorkflowOutboxDispatch(ctx stdctx.Context, id string, expected domain.WorkflowOutboxStatus, now time.Time, dispatchGeneration string) (bool, error)
	// ClaimOpenWorkflowAttempt returns the step's open attempt, creating one
	// atomically when there is none. It replaces the positional
	// "attempts[len-1].Outcome == \"\"" read that let two concurrent passes
	// both believe they held one open attempt.
	ClaimOpenWorkflowAttempt(ctx stdctx.Context, id, stepID, harness, model string, startedAt time.Time) (domain.WorkflowAttempt, bool, error)
	// StartWorkflowStepForSession moves a work step ready -> running only
	// while it durably holds the session the caller just confirmed, so
	// RUNNING is licensed by that confirmation rather than by order of
	// execution.
	StartWorkflowStepForSession(ctx stdctx.Context, id, sessionID string, now time.Time) (bool, error)
	// ListWorkflowOutboxByRun is what lets a superseded dispatch be retired
	// rather than left to be re-adopted. See supersedeReviewDispatch.
	ListWorkflowOutboxByRun(ctx stdctx.Context, runID string) ([]domain.WorkflowOutboxEntry, error)

	// SetWorkflowStepReviewRun backs Checkpoint 8C/8D's review-step dispatch
	// (review_dispatch.go). Checkpoint 8D redefines workflow_steps.
	// review_run_id as "current/most-recent" (mutable across review cycles),
	// so this is an unconditional set, not write-once.
	SetWorkflowStepReviewRun(ctx stdctx.Context, stepID, reviewRunID string, now time.Time) (bool, error)

	// RecordAgentHealthEvent and GetAgentHealth back Checkpoint 8H's minimal
	// durable agent health (failure_classifier.go, health.go, failover.go).
	// Append-only: RecordAgentHealthEvent never updates a prior row;
	// GetAgentHealth derives current health from the latest one.
	RecordAgentHealthEvent(ctx stdctx.Context, ev domain.AgentHealthEvent) (domain.AgentHealthEvent, error)
	GetAgentHealth(ctx stdctx.Context, harness domain.AgentHarness) (domain.AgentHealthEvent, bool, error)
	// GetAgentHealthScoped is Checkpoint 8P-C's per-(user,profile) health
	// read -- see healthScope's doc comment for the precedence rule against
	// GetAgentHealth's legacy/global rows.
	GetAgentHealthScoped(ctx stdctx.Context, harness domain.AgentHarness, userID domain.UserID, profileID domain.ProviderProfileID) (domain.AgentHealthEvent, bool, error)

	// GetWorkflowRunOwner backs Checkpoint 8P-C's routing (RouteExecution
	// needs the run owner before it knows which harness it will even pick,
	// so it can't wait for resolveRuntimeEnv's harness-scoped lookup).
	// Satisfied by the same store.Store method providerruntime.Resolver and
	// httpd's ownership scoping already use -- no second ownership lookup
	// implementation. Returns (nil, nil) for an unowned/pre-8P-A run.
	GetWorkflowRunOwner(ctx stdctx.Context, id string) (*domain.UserID, error)

	// SetWorkflowRunOwner backs Checkpoint 8P-C.1's durable child-run
	// ownership propagation (stampChildOwnership below). Idempotent:
	// re-stamping the same owner on recovery is always safe.
	SetWorkflowRunOwner(ctx stdctx.Context, id string, owner domain.UserID) (bool, error)

	// UpdateWorkflowRunPolicySnapshot backs Checkpoint 8P-C's run-creation
	// execution-policy embedding (ApplyExecutionPolicySnapshot below).
	UpdateWorkflowRunPolicySnapshot(ctx stdctx.Context, id, policySnapshot string, now time.Time) (bool, error)
}

type masterPlanStore interface {
	CreateWorkflowPlan(ctx stdctx.Context, runID string, mode domain.WorkflowPlanApprovalMode, contextVersion string, now time.Time) (domain.WorkflowPlanRecord, error)
	GetWorkflowPlan(ctx stdctx.Context, runID string) (domain.WorkflowPlanRecord, bool, error)
	StartWorkflowPlanCommand(ctx stdctx.Context, runID, provider, model, manifest string, now time.Time) (bool, error)
	PersistWorkflowPlanResponse(ctx stdctx.Context, runID, planJSON string, now time.Time) (bool, error)
	// PersistNormalizedWorkflowPlan re-persists the normalized plan under a
	// CAS conditioned on the bytes the caller read (P9). false means the row
	// moved on and this writer is stale.
	PersistNormalizedWorkflowPlan(ctx stdctx.Context, runID, expected, normalized string, now time.Time) (bool, error)
	FinishWorkflowPlan(ctx stdctx.Context, runID string, status domain.WorkflowPlanStatus, command domain.WorkflowPlanCommandStatus, validationJSON, hash, errorClass string, now time.Time) (bool, error)
	InsertWorkflowTasks(ctx stdctx.Context, tasks []domain.WorkflowTask) error
	ListWorkflowTasks(ctx stdctx.Context, runID string) ([]domain.WorkflowTask, error)
	// UpdateWorkflowTaskScope / ReplaceWorkflowTaskRelationships /
	// ListWorkflowTaskRelationships back the durable task write-set and
	// conflict model (see task_graph.go): the classification is persisted with
	// the plan so scheduling and integration read a decision instead of
	// re-deriving one from text that may since have changed.
	UpdateWorkflowTaskScope(ctx stdctx.Context, taskID, scopeJSON string, now time.Time) (bool, error)
	ReplaceWorkflowTaskRelationships(ctx stdctx.Context, rels []domain.WorkflowTaskRelationship) error
	ListWorkflowTaskRelationships(ctx stdctx.Context, runID string) ([]domain.WorkflowTaskRelationship, error)
	UpdateWorkflowTaskState(ctx stdctx.Context, id string, expected, next domain.WorkflowTaskState, now time.Time) (bool, error)
	// ParkWorkflowTaskForAttention / ResumeWorkflowTaskFromAttention are the
	// only two transitions in and out of the durable task-level parked state
	// (migration 0130). Both are conditional on the state they expect, which is
	// what makes parking race-free and resuming idempotent.
	ParkWorkflowTaskForAttention(ctx stdctx.Context, id string, expected domain.WorkflowTaskState, expectedAttempt int, reason string, attention domain.WorkflowTaskAttention, now time.Time) (bool, error)
	ResumeWorkflowTaskFromAttention(ctx stdctx.Context, id string, next domain.WorkflowTaskState, expectedAttempt int, now time.Time) (bool, error)
	// AmendWorkflowTaskCriterion / ListWorkflowTaskCriterionAmendments back the
	// Plan / Acceptance Criteria Amendment mechanism (migration 0132): a
	// human-approved, append-only change to a criterion that has stopped
	// describing reality. The write is one transaction over the ledger row and
	// the task's criteria, because an amendment nobody can account for and an
	// explanation for a change that never happened are both worse than nothing.
	AmendWorkflowTaskCriterion(ctx stdctx.Context, amendment domain.WorkflowTaskCriterionAmendment, criteria []string, now time.Time) error
	ListWorkflowTaskCriterionAmendments(ctx stdctx.Context, runID string) ([]domain.WorkflowTaskCriterionAmendment, error)
	SetWorkflowTaskExecutionRun(ctx stdctx.Context, taskID, executionRunID string, now time.Time) (bool, error)
	FindWorkflowRunByPlannedTask(ctx stdctx.Context, taskID string) (string, bool, error)
	ApproveWorkflowPlan(ctx stdctx.Context, runID string, now time.Time) (bool, error)
	RejectWorkflowPlan(ctx stdctx.Context, runID string, now time.Time) (bool, error)
	SetWorkflowPlanApprovalMode(ctx stdctx.Context, runID string, mode domain.WorkflowPlanApprovalMode, now time.Time) (bool, error)
}

// WakeScheduler is the narrow interface Checkpoint 8N's wake.Scheduler
// satisfies. Coordinator depends on this interface (not *wake.Scheduler
// directly) so unit tests can substitute a fake in-memory implementation,
// mirroring every other optional Deps field's own narrow-interface
// convention. Optional: a nil WakeScheduler means a capacity wait is
// recorded (run/step move to Waiting) but no wake is ever scheduled — same
// nil-safe-optional convention as every other Deps field here.
type WakeScheduler interface {
	Schedule(ctx stdctx.Context, runID domain.WorkflowRunID, stepID *domain.WorkflowStepID, reason wake.Reason, knownResetAt *time.Time) (wake.Schedule, error)
	CancelAllForRun(ctx stdctx.Context, runID domain.WorkflowRunID) (int, error)
	NextForRun(ctx stdctx.Context, runID domain.WorkflowRunID) (*wake.Schedule, error)
	// WakeNow makes a scope due immediately instead of after a backoff delay,
	// for the case where the thing it was waiting for demonstrably happened
	// (Checkpoint 8P-E.13A: a branch lock was released).
	WakeNow(ctx stdctx.Context, runID domain.WorkflowRunID, stepID *domain.WorkflowStepID, reason wake.Reason) (wake.Schedule, error)
}

// Projects resolves the project a run belongs to.
type Projects interface {
	GetProject(ctx stdctx.Context, id string) (domain.ProjectRecord, bool, error)
}

// Deps wires the coordinator.
type Deps struct {
	Store    Store
	Projects Projects

	// Sessions and ReviewRuns back Reconcile's best-effort integrity check
	// (see recovery.go). Both optional: a nil dependency simply skips its check.
	Sessions   Sessions
	ReviewRuns ReviewRuns
	// Logger receives recovery diagnostics. Optional.
	Logger *slog.Logger

	// Spawner, SessionFacts, and WorkspaceFacts back Checkpoint 8B's work-step
	// dispatch/observation (see dispatch.go, worker_progress.go). All optional:
	// a nil Spawner means dispatchWorkStep is a no-op (useful for tests that
	// only exercise the durable foundation); nil SessionFacts/WorkspaceFacts
	// similarly skip observation.
	//
	// One qualification on SessionFacts, since the phased dispatch landed: it
	// is the default read-back the dispatch confirmation proves session
	// ownership through (dispatch_state_machine.go). A Spawner wired WITHOUT
	// either SessionFacts or an explicit SessionOwnership can still launch
	// workers, but can never prove one, so every launch lands in the durable
	// `worker_launch_unconfirmed` state instead of reaching RUNNING. That is
	// the honest outcome rather than a silent one -- the record names the
	// missing read path -- but a deployment that dispatches work wants both.
	Spawner        Spawner
	SessionFacts   SessionFacts
	WorkspaceFacts WorkspaceFacts

	// WorkerLauncher and SessionOwnership override the phased dispatch state
	// machine's launch boundary (dispatch_state_machine.go). Both optional and
	// both default to adapters over Spawner/SessionFacts, so production wiring
	// is unchanged by their existence; they are here so that a launch, and the
	// ownership proof read back from it, can each be faked independently.
	WorkerLauncher   WorkerLauncher
	SessionOwnership SessionOwnership

	// ReviewerLauncher backs Checkpoint 8C's review-step dispatch
	// (review_dispatch.go). Optional: a nil ReviewerLauncher means
	// dispatchReviewStep is a no-op, same convention as a nil Spawner for
	// dispatchWorkStep. ReviewRuns (above) doubles as 8C's review read/write
	// path in addition to its original 8A recovery-integrity-check role.
	ReviewerLauncher ReviewerLauncher

	// MessageSender backs Checkpoint 8D's fix-step dispatch (fix_dispatch.go):
	// delivering fix findings to the SAME worker session, never a new Spawn.
	// Optional: a nil MessageSender means dispatchFixStep is a no-op.
	IncidentAgents        IncidentAgentLauncher
	SelfRepairProjectID   string
	MessageSender         MessageSender
	Verifier              VerifyRunner
	Planner               Planner
	PlannerContextBuilder PlannerContextBuilder

	// Switcher backs Checkpoint 8H's live-session Codex->Claude failover
	// (failover.go): the durable, generation-fenced session_manager agent-
	// switching saga, reused unmodified — workflow only decides WHEN to call
	// it. Optional: a nil Switcher means a live-session provider failure can
	// still be classified/recorded (RecordAgentHealthEvent) but cannot
	// durably switch the session, and is surfaced as needs_attention instead.
	Switcher AgentSwitcher

	// QuestionsStore and PaneReader back Checkpoint 8K-A's durable question
	// detection/classification/policy-resolution (questions_wiring.go).
	// Both optional: a nil QuestionsStore means detection, delivery,
	// dispatch guards, and cancel-cascade are all no-ops (same convention
	// as every other optional dependency here); a nil PaneReader with a
	// non-nil QuestionsStore still delivers already-answered questions
	// (restart recovery) but never attempts new detection.
	QuestionsStore QuestionsStore
	PaneReader     PaneReader

	// DecisionResolverLauncher backs Checkpoint 8K-B pass 2's cross-provider
	// Decision Resolver dispatch (decision_resolver_wiring.go). Optional: a
	// nil launcher means dispatchDecisionResolver always reports
	// "waiting_for_capacity: resolver unavailable" and never launches a
	// session — same nil-safe-optional convention as every other dependency
	// here (e.g. ReviewerLauncher).
	DecisionResolverLauncher DecisionResolverLauncher

	// Notifications receives the "this workflow run finished" intent when a run
	// durably enters WorkflowRunCompleted. Optional: nil raises no
	// notifications and changes nothing else.
	Notifications NotificationSink

	// WakeScheduler backs Checkpoint 8N's durable wake-up scheduler: when a
	// run/step enters a capacity wait, Coordinator asks WakeScheduler to
	// persist a wake so a daemon-level poller can resume it automatically.
	// Optional: nil means capacity waits are recorded exactly as before 8N,
	// just with no automatic wake scheduled (a human must still intervene).
	WakeScheduler WakeScheduler

	// Clock and NewID are injectable for deterministic tests.
	Clock func() time.Time
	NewID func() string

	// RuntimeIsolation backs Checkpoint 8P-B.1's per-user provider
	// credential isolation: every dispatch site (worker, reviewer, planner,
	// decision resolver) resolves the workflow owner's isolated runtime
	// env through this single dependency before launching. Optional: nil
	// preserves pre-8P-B.1 behavior exactly.
	RuntimeIsolation RuntimeIsolation

	// ProviderProfiles and ExecutionPolicies back Checkpoint 8P-C's
	// user-configurable routing: RouteExecution walks the owner's own
	// profiles under their own priority policy instead of a fixed
	// Claude<->Codex table. Both optional: nil makes every routing decision
	// resolve to domain.DefaultUserExecutionPolicy with no owned profiles,
	// i.e. always waiting -- never a silent hardcoded fallback.
	ProviderProfiles  ProviderProfiles
	ExecutionPolicies ExecutionPolicies
	// BranchLocks and WorkspaceCommitter back Checkpoint 8P-E.11's
	// direct-branch execution mode: the durable per-repository+branch
	// execution lock, and the autonomous local commit that concludes a run
	// whose project policy allows it. Both optional: nil means no run ever
	// takes a branch lock and no run ever commits on its own -- exactly the
	// pre-8P-E.11 behavior every isolated-worktree project keeps.
	BranchLocks BranchLocks
	// IntegrationLocks is the target-integration lane (internal/integration).
	// Wired in internal/daemon to integration.NewBranchLocker over the same
	// branch-lock manager BranchLocks uses, so an integration and a direct
	// writer of one branch exclude each other.
	IntegrationLocks   integration.Locker
	WorkspaceCommitter WorkspaceCommitter

	// TrustedLocal mirrors config.Config.TrustedLocalMode (Checkpoint 8P-C):
	// when true, a workflow owner with zero configured ProviderProfile rows
	// falls back to legacy/global compatibility profiles instead of waiting
	// forever -- the same desktop-upgrade compatibility
	// providerruntime.Resolver.TrustedLocal already grants runtime-env
	// resolution. Multi-user mode (false) never applies this: zero owned
	// profiles there correctly waits (checkpoint brief §18).
	TrustedLocal bool

	// CapacityProber backs Checkpoint 8P-E.13A.4's active capacity probe (see
	// CapacityProber). Optional.
	CapacityProber CapacityProber

	// TaskWorkspaces is the AO-owned worktree lifecycle (internal/workspace):
	// what records a task's work as landed, what cleans up after it, what
	// preserves it when it did not land, and what reconciles all three after a
	// restart. Optional: nil leaves every AO worktree and ao/* branch exactly
	// where it is, which is the pre-cleanup behavior -- untidy, never unsafe.
	TaskWorkspaces TaskWorkspaces

	// TaskWorktreeRecords is the READ side of the same worktree records
	// TaskWorkspaces writes. It is a separate dependency rather than two more
	// methods on that port because the two have opposite blast radii: one
	// removes directories and deletes branches, the other answers "which
	// checkout and which ao/* branch hold this task's work" for a board card.
	// Optional: nil leaves the worktree/branch fields of the planner projection
	// empty, which is the honest answer when nothing can be read.
	TaskWorktreeRecords TaskWorktreeRecords

	// WorkerLiveness is the optional runtime liveness probe the missing-first-
	// signal reconciliation consults (worker_signal_reconcile.go). Optional: nil
	// simply removes one independent fact, and its absence is never read as a
	// dead worker.
	WorkerLiveness WorkerLivenessProbe

	// WorkerPreflight is the optional pre-dispatch provider readiness check
	// (provider_preflight.go). Optional: nil keeps the pre-8P-E.24 behavior of
	// spawning straight into whatever the provider does.
	WorkerPreflight WorkerPreflight

	// CommitHistory lists a worktree's reachable commits, and is what lets AO
	// reconstruct the commit an approved review target was read at for runs
	// whose ledger predates that pin (approved_head_recovery.go). Optional: nil
	// uses a plain `git rev-list` reader, and a repository that cannot be read
	// produces no candidates, which fails closed.
	CommitHistory CommitHistory
}

// TaskWorktreeRecords lists the AO worktree records belonging to one master
// run. Satisfied by *storage/sqlite/store.Store.
type TaskWorktreeRecords interface {
	ListTaskWorktreesByRun(ctx stdctx.Context, runID string) ([]domain.TaskWorktreeRecord, error)
}

// TaskWorkspaces is the workflow side's view of the worktree lifecycle
// manager, satisfied by *internal/workspace.Manager.
//
// It is a port rather than a direct dependency for the reason every other one
// here is: the coordinator only ever asks it four questions, and naming them
// is what keeps "the workflow engine can delete a branch" from being true in
// any broader sense than these four calls.
type TaskWorkspaces interface {
	// MarkIntegrated records that a task's work is durably on its target ref,
	// at the given commit. It is what authorizes cleanup, and it is called only
	// after the promotion is itself durably recorded.
	MarkIntegrated(ctx stdctx.Context, taskID, integratedSHA string) (domain.TaskWorktreeRecord, error)
	// Cleanup removes an integrated task's worktree and deletes its ao/* branch
	// when it can prove the branch holds nothing the target does not.
	Cleanup(ctx stdctx.Context, taskID string) (workspace.CleanupResult, error)
	// Preserve marks a failed or cancelled task's worktree as evidence to keep.
	Preserve(ctx stdctx.Context, taskID, reason string) (domain.TaskWorktreeRecord, bool, error)
	// Reconcile matches every unfinished record against the repository and
	// finishes whatever a restart interrupted.
	Reconcile(ctx stdctx.Context) (workspace.ReconcileReport, error)
}

// ProviderProfiles lists a user's owned provider profiles (Checkpoint
// 8P-C). Satisfied by *storage/sqlite/store.Store.
type ProviderProfiles interface {
	ListProviderProfilesByUser(ctx stdctx.Context, userID domain.UserID) ([]domain.ProviderProfile, error)
}

// ExecutionPolicies reads a user's stored routing policy (Checkpoint
// 8P-C). Satisfied by *storage/sqlite/store.Store.
type ExecutionPolicies interface {
	GetUserExecutionPolicyByUser(ctx stdctx.Context, userID domain.UserID) (domain.UserExecutionPolicy, bool, error)
}

// Coordinator is the core workflow durable-foundation engine.
type Coordinator struct {
	store    Store
	projects Projects
	clock    func() time.Time
	newID    func() string

	// sessions, reviewRuns, and log back Reconcile's best-effort integrity
	// check (see recovery.go). All optional.
	sessions   Sessions
	reviewRuns ReviewRuns
	log        *slog.Logger

	// spawner, sessionFacts, and workspaceFacts back Checkpoint 8B's work-step
	// dispatch/observation. All optional.
	spawner        Spawner
	sessionFacts   SessionFacts
	workspaceFacts WorkspaceFacts

	// workerLauncher and sessionOwnership are the phased dispatch state
	// machine's launch boundary (dispatch_state_machine.go). Both optional:
	// nil workerLauncher adapts the Spawner above, nil sessionOwnership adapts
	// SessionFacts. They exist as separate, injectable interfaces so a test can
	// drive launch success, launch failure and evidence-free success without a
	// process or a timer.
	workerLauncher   WorkerLauncher
	sessionOwnership SessionOwnership

	// reviewerLauncher backs Checkpoint 8C's review-step dispatch. Optional.
	reviewerLauncher ReviewerLauncher

	// incidentAgents backs Checkpoint 8P-E.18's Incident Advisor: the isolated
	// Diagnostic and Repair agents. Optional — without it a stopped run can
	// still be inspected and explained, it simply cannot be investigated
	// automatically.
	incidentAgents IncidentAgentLauncher

	// selfRepairProjectID names the project holding AO's OWN source, the only
	// repository an incident repair may be launched into. Empty means self
	// repair is unavailable, which is a refusal rather than a fallback: guessing
	// which checkout is AO's own is exactly the guess that would land a repair
	// in the user's working tree. See incident_repair.go.
	selfRepairProjectID string

	// messageSender backs Checkpoint 8D's fix-step dispatch. Optional.
	messageSender MessageSender
	verifier      VerifyRunner
	planStore     masterPlanStore
	// verifyClaimsState holds the per-process claims that stop one daemon from
	// executing the same verify attempt twice concurrently. See
	// verify_authority.go.
	verifyClaimsState
	planner               Planner
	plannerContextBuilder PlannerContextBuilder

	// switcher backs Checkpoint 8H's live-session failover. Optional.
	switcher AgentSwitcher

	// questionsStore and paneReader back Checkpoint 8K-A's durable question
	// handling. Both optional.
	questionsStore QuestionsStore
	paneReader     PaneReader

	// decisionResolverLauncher backs Checkpoint 8K-B pass 2's resolver
	// dispatch. Optional.
	decisionResolverLauncher DecisionResolverLauncher

	// notifications receives run-completion notification intents. Optional.
	notifications NotificationSink

	// wakeScheduler backs Checkpoint 8N's durable wake-up scheduler.
	// Optional.
	wakeScheduler WakeScheduler

	// runtimeIsolation backs Checkpoint 8P-B.1's per-user provider
	// credential isolation. Optional.
	runtimeIsolation RuntimeIsolation

	// providerProfiles and executionPolicies back Checkpoint 8P-C's
	// user-configurable routing. Both optional.
	providerProfiles  ProviderProfiles
	executionPolicies ExecutionPolicies
	trustedLocal      bool

	// integrationLocks is the target-integration lane every task's work passes
	// through on its way onto a target ref (internal/integration). It is
	// separate from branchLocks because the two answer different questions:
	// branchLocks is "may this run write this project's branches at all", held
	// for the length of a run, while this is "may this task move this target
	// right now", held for the length of one integration. Optional: a
	// deployment without it keeps the serial promotion path and refuses, loudly,
	// only where a lane would actually have been needed.
	integrationLocks integration.Locker
	// branchLocks and workspaceCommitter back Checkpoint 8P-E.11's
	// direct-branch execution mode. Both optional.
	branchLocks        BranchLocks
	workspaceCommitter WorkspaceCommitter

	// capacityProber backs Checkpoint 8P-E.13A.4's ACTIVE capacity probe.
	// Optional: nil keeps the pre-8P-E.13A.4 purely reactive behavior, where a
	// never-dispatched profile stays domain.CapacityUnknown.
	capacityProber CapacityProber
	probeGate      *capacityProbeGate

	// taskWorkspaces is the AO-owned worktree lifecycle: cleanup after an
	// integration, preservation after a failure, and the boot pass that
	// finishes either one after a restart. Optional.
	taskWorkspaces TaskWorkspaces
	// taskWorktreeRecords reads those same records back for the planner
	// projection the API and the Board render. Optional.
	taskWorktreeRecords TaskWorktreeRecords

	// workerLiveness backs the missing-first-signal reconciliation's optional
	// runtime probe; workerPreflight backs the pre-dispatch provider readiness
	// check. Both optional.
	workerLiveness  WorkerLivenessProbe
	workerPreflight WorkerPreflight

	// commitHistory backs the approved-head reconstruction. Optional; see
	// commitHistoryOrDefault.
	commitHistory CommitHistory

	// plannerExec owns the execution contexts in-flight planner calls run on,
	// so a minutes-long plan generation outlives the request or poller tick
	// that entered it without outliving the daemon or the run. See
	// planner_execution_context.go.
	plannerExec plannerExecution
}

// New wires a Coordinator from its dependencies, defaulting the clock and id source.
func New(d Deps) *Coordinator {
	clock := d.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	newID := d.NewID
	if newID == nil {
		newID = uuid.NewString
	}
	return &Coordinator{
		store:                    d.Store,
		projects:                 d.Projects,
		sessions:                 d.Sessions,
		reviewRuns:               d.ReviewRuns,
		log:                      d.Logger,
		branchLocks:              d.BranchLocks,
		integrationLocks:         d.IntegrationLocks,
		workspaceCommitter:       d.WorkspaceCommitter,
		spawner:                  d.Spawner,
		sessionFacts:             d.SessionFacts,
		workspaceFacts:           d.WorkspaceFacts,
		workerLauncher:           d.WorkerLauncher,
		sessionOwnership:         d.SessionOwnership,
		reviewerLauncher:         d.ReviewerLauncher,
		incidentAgents:           d.IncidentAgents,
		selfRepairProjectID:      d.SelfRepairProjectID,
		messageSender:            d.MessageSender,
		verifier:                 d.Verifier,
		planStore:                func() masterPlanStore { s, _ := d.Store.(masterPlanStore); return s }(),
		planner:                  d.Planner,
		plannerContextBuilder:    d.PlannerContextBuilder,
		switcher:                 d.Switcher,
		questionsStore:           d.QuestionsStore,
		paneReader:               d.PaneReader,
		decisionResolverLauncher: d.DecisionResolverLauncher,
		notifications:            d.Notifications,
		wakeScheduler:            d.WakeScheduler,
		runtimeIsolation:         d.RuntimeIsolation,
		providerProfiles:         d.ProviderProfiles,
		executionPolicies:        d.ExecutionPolicies,
		trustedLocal:             d.TrustedLocal,
		capacityProber:           d.CapacityProber,
		taskWorkspaces:           d.TaskWorkspaces,
		taskWorktreeRecords:      d.TaskWorktreeRecords,
		workerLiveness:           d.WorkerLiveness,
		workerPreflight:          d.WorkerPreflight,
		commitHistory:            d.CommitHistory,
		probeGate:                &capacityProbeGate{attempts: make(map[capacityProbeKey]time.Time)},
		clock:                    clock,
		newID:                    newID,
	}
}

// RunDetail is a workflow run plus its steps and each step's attempts.
type RunDetail struct {
	Run   domain.WorkflowRun
	Steps []StepDetail
	// NextAction mirrors the run's most recent checkpoint's NextAction across
	// all its steps (e.g. "start_review" once the work step completes).
	// Empty when no checkpoint has recorded one yet.
	NextAction string
	Plan       *domain.WorkflowPlanRecord
	Tasks      []domain.WorkflowTask
	// SessionLifecycle is Checkpoint 8M's audit trail of every
	// session_lifecycle_decision checkpoint recorded for this run (task-
	// boundary/provider-switch/fix-compact decisions), in the same order
	// ListWorkflowCheckpoints returns them. Run-level, not per-step — see
	// session_context_pack.go's persistSessionLifecycleDecision doc comment
	// for why these are deliberately not tied to a WorkflowStepID.
	SessionLifecycle []SessionLifecycleAuditEntry
	// IntegrationState is Checkpoint 8M.1's live-derived git integration
	// summary — populated only for master runs (Plan != nil). Nil for a
	// non-master run or a master run whose plan hasn't been approved yet.
	IntegrationState *MasterIntegrationSummary
	// NextWakeAt/WaitReason are Checkpoint 8N's read-time surfacing of the
	// soonest still-open durable wake for this run (see
	// wake.Scheduler.NextForRun) — populated only when wakeScheduler is
	// configured and a wake is currently pending/claimed for this run. Both
	// zero-value when not applicable, never a fabricated estimate.
	NextWakeAt *time.Time
	WaitReason string
	// WakeAttemptCount mirrors the same wake row's AttemptCount — how many
	// times this exact capacity wait has already retried (checkpoint brief
	// §15's "Attempt: N"). 0 when no wake is open.
	WakeAttemptCount int64
	// BranchWait is Checkpoint 8P-E.11's structured waiting_for_branch state:
	// which branch this direct-branch run is queued on and which workflow
	// currently owns it. Populated only while the run is actually Waiting and
	// its most recent waiting_for_branch checkpoint recorded a holder, so the
	// board shows a real wait or nothing at all — never a fabricated
	// "inactive".
	BranchWait *BranchWait
	// LatestCheckpointPhase/LatestCheckpointAt are the durable_phase and
	// created_at of the newest workflow_checkpoints row for this run
	// (Checkpoint 8P-E.12). They are the machine-readable half of the
	// needs_attention/wait reason pair the mapping document describes, and the
	// timestamp the Board reports as "Last activity" — deliberately the
	// workflow's own last durable act, never the worker session's activity
	// state, because an idle worker during a review is not an idle workflow.
	LatestCheckpointPhase string
	LatestCheckpointAt    time.Time
	// Questions is the run's durable question list in creation order. Empty
	// means the run has never asked anything; it is what DeriveLifecycle reads
	// to decide whether a stop is genuinely the user's to resolve.
	Questions []domain.WorkflowQuestion
	// CapacityWait is the normalized provider-capacity wait projection (see
	// capacity_wait.go). Non-nil only while the run's newest routing decision is
	// actually a wait, so the UI renders a real, explainable wait or nothing.
	CapacityWait *CapacityWait
	// TaskPlanner is the per-task planner projection (see
	// task_planner_view.go): execution strategy, dependencies, waiting reason,
	// dispatch wave, probable write scope, AO worktree/branch and integration
	// state, one entry per element of Tasks and in the same order. Empty for a
	// run that has no planned tasks.
	TaskPlanner []TaskPlannerView
}

// SessionLifecycleAuditEntry is one durable session-lifecycle decision plus
// the context pack it produced (if any) and when it was recorded.
type SessionLifecycleAuditEntry struct {
	Decision    domain.SessionLifecycleDecision
	ContextPack *domain.SessionContextPack
	CreatedAt   time.Time
}

// StepDetail is one workflow step plus its recorded attempts and latest
// checkpoint, if any.
type StepDetail struct {
	Step             domain.WorkflowStep
	Attempts         []domain.WorkflowAttempt
	LatestCheckpoint *domain.WorkflowCheckpoint
	// Review is populated for a review step whose ReviewRunID is set,
	// fetched live from ReviewRuns.GetReviewRun (Checkpoint 8C). Nil when
	// not applicable or the review run could not be read. The findings body
	// is truncated here, not persisted: workflow_checkpoints only ever store
	// the review_run_id reference, never a copy of the body.
	Review *ReviewSummary
	// ReviewPolicy is populated for a review step whose review_policy_decision
	// checkpoint exists (Checkpoint 8I) — for REQUIRED decisions this sits
	// alongside Review; for SKIPPED decisions Review stays nil (no
	// review_run was ever created) and this is the only durable record of
	// why. Read live at GetRun time, mirroring Review's own read pattern.
	ReviewPolicy *ReviewPolicyDecision
	// Routing is Checkpoint 8L's ExecutionRouter decision for this step's
	// role (worker/reviewer/planner), populated whenever a routing_decision
	// checkpoint exists for it — read live, mirroring ReviewPolicy's own
	// pattern.
	Routing *domain.RoutingDecision
	// FixDelivery is the read-time projection of the newest fix-cycle delivery
	// recorded for a fix step: which review verdict authorized it, which
	// findings travelled (digest/count/size and whether they were embedded in
	// the delivered prompt), which attempt and session it is bound to, and what
	// the transport could prove. Nil for a non-fix step or a fix step that has
	// never been dispatched. See fix_delivery_report.go.
	FixDelivery *FixDeliveryReport
}

// annotateFixDeliveryReceipt fills in FixDeliveryReport.ReceiptMatch by asking
// the worker session what prompt it last recorded receiving and comparing it
// against the digest AO wrote down before delivering.
//
// Read-only and best-effort by construction: it writes nothing, and every
// failure to obtain an answer leaves the field empty rather than guessing. The
// comparison is exact because both sides are bounded by the same
// domain.BoundLatestUserPrompt — see promptReceiptDigest.
func (c *Coordinator) annotateFixDeliveryReceipt(ctx stdctx.Context, report *FixDeliveryReport) {
	if report == nil || c.sessionFacts == nil || report.SessionID == "" || report.PromptReceipt == "" {
		return
	}
	sess, found, err := c.sessionFacts.GetSession(ctx, domain.SessionID(report.SessionID))
	if err != nil || !found {
		return
	}
	switch {
	case sess.Metadata.LatestUserPrompt == "":
		report.ReceiptMatch = "none"
	case promptReceiptDigest(sess.Metadata.LatestUserPrompt) == report.PromptReceipt:
		report.ReceiptMatch = "match"
	default:
		report.ReceiptMatch = "other"
	}
}

// ReviewSummary is a read-time-only projection of a review step's review_run
// facts, for the API layer to surface without importing review internals.
type ReviewSummary struct {
	Harness         domain.ReviewerHarness
	Verdict         domain.ReviewVerdict
	Target          string
	FindingsSummary string
}

// reviewFindingsSummaryMaxLen bounds the truncated findings preview surfaced
// in GetRun's response (Checkpoint 8C §7).
const reviewFindingsSummaryMaxLen = 500

// CreateRun creates a new workflow run in state pending and seeds its initial
// steps under the fixed v1 policy: a strict linear chain of six steps
// (plan, work, review, fix, verify, advance). Every step starts pending
// except the first, which starts ready since nothing blocks it — no
// automatic execution happens; a future checkpoint decides when to actually
// run it.
func (c *Coordinator) CreateRun(ctx stdctx.Context, projectID, objective string, verification ...VerificationPlan) (RunDetail, error) {
	return c.createSingleTaskRun(ctx, projectID, objective, nil, nil, c.defaultTaskStrategy(), verification...)
}

// TaskRunRequest is P1-A's create-a-task entry point: everything a bounded
// TASK run needs, and nothing a planned objective would.
//
// It is a struct rather than more parameters on CreateRun because a task is
// now a thing a caller deliberately asks for, with its own acceptance
// criteria, its own declared write intent and its own frozen strategy --
// while CreateRun keeps its signature for the ~50 existing call sites that
// mean "a single bounded chain" and nothing more.
type TaskRunRequest struct {
	ProjectID string
	Objective string
	// Strategy is the frozen selection to record. An unrecorded selection
	// falls back to defaultTaskStrategy rather than being invented.
	Strategy domain.ExecutionStrategySelection
	// AcceptanceCriteria is what "done" means for this task. Optional; the
	// generic artifact's criteria are used when it is empty.
	AcceptanceCriteria []string
	// WriteIntent declares whether the task is expected to change the
	// workspace. Unspecified is treated as mutating everywhere, exactly as
	// it is for a planned task -- a read-only task must SAY so, which is what
	// lets a correctly-unchanged workspace complete instead of parking as
	// ambiguous_worker_state.
	WriteIntent  domain.WorkflowWriteIntent
	Verification VerificationPlan
}

// CreateTaskRun creates a bounded TASK run: the ordinary durable
// plan/work/review/fix/verify/advance chain, with no objective planner, no
// decomposition and no hierarchy -- and with its strategy, acceptance
// criteria and write intent all bound by the SAME creation transaction, so
// no crash can separate a task from what it was asked to do (the CP21
// argument, applied to a standalone task).
func (c *Coordinator) CreateTaskRun(ctx stdctx.Context, req TaskRunRequest) (RunDetail, error) {
	strategy := req.Strategy
	if !strategy.Recorded() {
		strategy = c.defaultTaskStrategy()
	}
	if strategy.Effective != domain.ExecutionStrategyTask {
		return RunDetail{}, fmt.Errorf("%w: CreateTaskRun cannot create a %q run", ErrInvalid, strategy.Effective)
	}
	var overlay *plannedTaskArtifact
	if len(req.AcceptanceCriteria) > 0 || req.WriteIntent != domain.WorkflowWriteIntentUnspecified {
		criteria := req.AcceptanceCriteria
		if len(criteria) == 0 {
			// applyTo replaces the criteria wholesale (CP21 needs it to, for a
			// planned task), so a caller who declared only a write intent must
			// not silently lose the default criteria along the way.
			criteria = BuildPlanArtifact(req.ProjectID, req.Objective, policyVersionV1).AcceptanceCriteria
		}
		overlay = &plannedTaskArtifact{AcceptanceCriteria: criteria, WriteIntent: req.WriteIntent}
	}
	return c.createRunWithPlanArtifact(ctx, req.ProjectID, req.Objective, nil, nil, overlay, strategy, req.Verification)
}

func (c *Coordinator) createSingleTaskRun(ctx stdctx.Context, projectID, objective string, parentWorkflowID, plannedTaskID *string, strategy domain.ExecutionStrategySelection, verification ...VerificationPlan) (RunDetail, error) {
	return c.createRunWithPlanArtifact(ctx, projectID, objective, parentWorkflowID, plannedTaskID, nil, strategy, verification...)
}

// plannedTaskArtifact carries the parts of a master plan's task that must be
// bound into its child run's plan step *at creation*, not one write later.
//
// This is CP21 in docs/worker-lifecycle-audit.md, the highest-severity window
// in the plan segment. dispatchMasterTask used to create the child with
// BuildPlanArtifact's generic boilerplate and then overwrite the artifact
// with the planner's real criteria and write intent as a separate statement.
// A crash in between left a child that looked complete and correct -- owned,
// policy-frozen, plan step present -- and whose artifact was simply the wrong
// one. Recovery goes straight to StartRun, which builds the worker prompt
// from that artifact, so the worker ran against generic acceptance criteria,
// and an empty WriteIntent (Unspecified) meant a task the plan declared
// read-only was verified and classified as mutating. Nothing downstream could
// tell: a plausible artifact is indistinguishable from the right one.
//
// Passing it through creation removes the window instead of guarding it: the
// artifact is part of the same CreateWorkflowRun transaction as the run and
// its steps, so no crash can separate them.
type plannedTaskArtifact struct {
	AcceptanceCriteria []string
	WriteIntent        domain.WorkflowWriteIntent
}

func (o *plannedTaskArtifact) applyTo(artifact *PlanArtifact) {
	if o == nil {
		return
	}
	artifact.AcceptanceCriteria = o.AcceptanceCriteria
	artifact.WriteIntent = o.WriteIntent
}

// matches reports whether artifact already carries exactly this task's
// semantics -- used by recovery to tell a correctly-bound child from one
// created before this fix (or by a crash) with the generic artifact.
func (o *plannedTaskArtifact) matches(artifact PlanArtifact) bool {
	if o == nil {
		return true
	}
	if artifact.WriteIntent != o.WriteIntent {
		return false
	}
	if len(artifact.AcceptanceCriteria) != len(o.AcceptanceCriteria) {
		return false
	}
	for i := range o.AcceptanceCriteria {
		if artifact.AcceptanceCriteria[i] != o.AcceptanceCriteria[i] {
			return false
		}
	}
	return true
}

func (c *Coordinator) createRunWithPlanArtifact(ctx stdctx.Context, projectID, objective string, parentWorkflowID, plannedTaskID *string, overlay *plannedTaskArtifact, strategy domain.ExecutionStrategySelection, verification ...VerificationPlan) (RunDetail, error) {
	if projectID == "" {
		return RunDetail{}, fmt.Errorf("%w: project id is required", ErrInvalid)
	}
	if objective == "" {
		return RunDetail{}, fmt.Errorf("%w: objective is required", ErrInvalid)
	}
	if c.projects != nil {
		if _, ok, err := c.projects.GetProject(ctx, projectID); err != nil {
			return RunDetail{}, err
		} else if !ok {
			return RunDetail{}, fmt.Errorf("%w: project %q", ErrNotFound, projectID)
		}
	}

	now := c.clock()
	runID := "wf-" + c.newID()
	// CP3: creation records that the execution-policy freeze is still OWED,
	// in the same statement that creates the run. Without that marker a run
	// whose freeze was lost to a crash is byte-identical to a run that
	// legitimately carries the default policy, and the silent downgrade of an
	// autonomous objective to manual is unrecoverable because it is
	// undetectable. See domain.ExecutionPolicyProvenance.
	// P1-A: the execution strategy is frozen HERE, in the same marshalled
	// snapshot as the freeze-owed marker, so a run can never durably exist
	// without one. Nothing downstream recomputes it.
	policySnapshot, err := json.Marshal(withStrategy(unfrozenExecutionPolicy(domain.DefaultWorkflowPolicy(), now), strategy))
	if err != nil {
		return RunDetail{}, fmt.Errorf("marshal default workflow policy: %w", err)
	}
	run := domain.WorkflowRun{
		ID:               runID,
		ProjectID:        projectID,
		Objective:        objective,
		State:            domain.WorkflowRunPending,
		PolicyVersion:    policyVersionV1,
		PolicySnapshot:   string(policySnapshot),
		CreatedAt:        now,
		UpdatedAt:        now,
		ParentWorkflowID: parentWorkflowID,
		PlannedTaskID:    plannedTaskID,
	}

	steps := make([]domain.WorkflowStep, 0, len(workflowStepPolicyV1))
	planArtifact := BuildPlanArtifact(projectID, objective, policyVersionV1, verification...)
	// CP21: the planned task's real criteria and write intent are bound here,
	// inside the same transaction that writes the run and its six steps --
	// never as a follow-up UPDATE a crash can lose.
	overlay.applyTo(&planArtifact)
	planArtifactJSON, err := MarshalPlanArtifact(planArtifact)
	if err != nil {
		return RunDetail{}, err
	}
	var prevID *string
	for i, kind := range workflowStepPolicyV1 {
		state := domain.WorkflowStepPending
		if i == 0 {
			state = domain.WorkflowStepReady
		}
		stepID := "wfs-" + c.newID()
		steps = append(steps, domain.WorkflowStep{
			ID:              stepID,
			WorkflowRunID:   runID,
			Kind:            kind,
			Ordinal:         int64(i + 1),
			DependsOnStepID: prevID,
			State:           state,
			CreatedAt:       now,
			UpdatedAt:       now,
		})
		if kind == domain.WorkflowStepPlan {
			steps[len(steps)-1].ArtifactJSON = planArtifactJSON
		}
		id := stepID
		prevID = &id
	}

	createdRun, createdSteps, err := c.store.CreateWorkflowRun(ctx, run, steps)
	if err != nil {
		return RunDetail{}, err
	}
	detail := RunDetail{Run: createdRun}
	for _, step := range createdSteps {
		detail.Steps = append(detail.Steps, StepDetail{Step: step})
	}
	return detail, nil
}

// GetRun reads one workflow run with its steps and each step's attempts.
// While the run is non-terminal and a work step is running, it opportunistically
// observes that step's real progress (session + throttled workspace facts) and
// persists any resulting transition before building the response — this is
// this codebase's existing "derive status at read time" philosophy (compare
// service/session/status.go's deriveStatus), not a background scheduler.
func (c *Coordinator) GetRun(ctx stdctx.Context, runID string) (RunDetail, error) {
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	if !ok {
		return RunDetail{}, fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	steps, err := c.store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	if c.planStore != nil {
		if plan, isMaster, planErr := c.planStore.GetWorkflowPlan(ctx, runID); planErr != nil {
			return RunDetail{}, planErr
		} else if isMaster {
			return c.getMasterRun(ctx, run, steps, plan)
		}
	}

	detail := RunDetail{Run: run}
	for i, step := range steps {
		if !run.State.Terminal() && step.Kind == domain.WorkflowStepWork && step.State == domain.WorkflowStepRunning {
			updated, err := c.observeWorkStep(ctx, run, step)
			if err != nil {
				return RunDetail{}, err
			}
			steps[i] = updated
			if refreshed, ok2, rerr := c.store.GetWorkflowRun(ctx, runID); rerr == nil && ok2 {
				run = refreshed
				detail.Run = run
			}
		}
	}

	// Checkpoint 8K-A: durable question detection + restart-recovery
	// delivery sweep, read-time-derived exactly like observeWorkStep above —
	// runs before the review<->fix cascade so a freshly detected/answered
	// question's dispatch guards (dispatch.go, fix_dispatch.go,
	// review_dispatch.go) see up-to-date state within this same call.
	waitingForCapacity, err := c.reconcileQuestions(ctx, run, steps)
	if err != nil {
		return RunDetail{}, err
	}

	// Checkpoint 8D: the automatic review<->fix progression cascade —
	// opportunistically observes a running review/fix step and dispatches
	// the next eligible cycle within this same call (see advanceReviewFixCycle's
	// doc comment). includeCycle1Unblock=false: the very first review
	// dispatch (work-just-completed, review step still "pending") stays
	// ContinueRun's explicit job, exactly as Checkpoint 8C originally scoped
	// it (see TestReviewStepUntouchedAfterWorkCompletion).
	updatedRun, err := c.advanceReviewFixCycle(ctx, run, steps, false)
	if err != nil {
		return RunDetail{}, err
	}
	run = updatedRun
	detail.Run = run

	for _, step := range steps {
		attempts, err := c.store.ListWorkflowAttempts(ctx, step.ID)
		if err != nil {
			return RunDetail{}, err
		}
		var cpPtr *domain.WorkflowCheckpoint
		if cp, hasCP, cperr := c.store.GetLatestWorkflowCheckpointByStep(ctx, step.ID); cperr == nil && hasCP {
			cp := cp
			cpPtr = &cp
		}
		var reviewSummary *ReviewSummary
		var reviewPolicy *ReviewPolicyDecision
		if step.Kind == domain.WorkflowStepReview {
			if step.ReviewRunID != nil && c.reviewRuns != nil {
				if rr, found, rrErr := c.reviewRuns.GetReviewRun(ctx, *step.ReviewRunID); rrErr == nil && found {
					// The read model shows the EFFECTIVE authoritative outcome. An
					// adopted late verdict is the review's real result; rendering
					// the raw column would show the API, the Board and every
					// checkpoint/context summary a blank verdict with no findings
					// for a review that had in fact concluded.
					body := rr.EffectiveBody()
					if len(body) > reviewFindingsSummaryMaxLen {
						body = body[:reviewFindingsSummaryMaxLen]
					}
					reviewSummary = &ReviewSummary{
						Harness:         rr.Harness,
						Verdict:         rr.EffectiveVerdict(),
						Target:          rr.TargetSHA,
						FindingsSummary: body,
					}
				}
			}
			if decision, ok := c.reviewPolicyDecisionForStep(ctx, runID, step.ID); ok {
				reviewPolicy = &decision
			}
		}
		var routing *domain.RoutingDecision
		if decision, ok := c.routingDecisionForStep(ctx, runID, step.ID); ok {
			routing = &decision
		}
		detail.Steps = append(detail.Steps, StepDetail{Step: step, Attempts: attempts, LatestCheckpoint: cpPtr, Review: reviewSummary, ReviewPolicy: reviewPolicy, Routing: routing})
	}

	if checkpoints, cperr := c.store.ListWorkflowCheckpoints(ctx, runID); cperr == nil {
		// The fix-delivery projection, derived from the same rows the loop
		// below already reads. Done here rather than per step so the ledger is
		// listed once, exactly like every other run-level derivation.
		for i := range detail.Steps {
			if detail.Steps[i].Step.Kind != domain.WorkflowStepFix {
				continue
			}
			detail.Steps[i].FixDelivery = BuildFixDeliveryReport(
				detail.Steps[i].Step, detail.Steps[i].Attempts, checkpoints)
			c.annotateFixDeliveryReceipt(ctx, detail.Steps[i].FixDelivery)
		}
		for _, cp := range checkpoints {
			// Checkpoint 8P-E.18: incident-ledger rows describe a stop, they are
			// never one. See isBookkeepingPhase.
			if isBookkeepingPhase(cp.DurablePhase) {
				continue
			}
			if cp.NextAction != "" {
				detail.NextAction = cp.NextAction
			}
			if !cp.CreatedAt.Before(detail.LatestCheckpointAt) {
				detail.LatestCheckpointPhase = cp.DurablePhase
				detail.LatestCheckpointAt = cp.CreatedAt
			}
			if cp.DurablePhase == sessionLifecycleDurablePhase {
				if rec, ok := decodeSessionLifecycleRecord(cp.RetryState); ok {
					detail.SessionLifecycle = append(detail.SessionLifecycle, SessionLifecycleAuditEntry{
						Decision: rec.Decision, ContextPack: rec.ContextPack, CreatedAt: cp.CreatedAt,
					})
				}
			}
		}
	}

	// Checkpoint 8K-A: an open question always wins over any checkpoint's
	// recorded NextAction — the run is not truly progressing toward that
	// next_action until the question is resolved. Derived at read time
	// only, never persisted: the moment the question is answered+delivered
	// it drops out of ListOpenWorkflowQuestionsByRun and this override
	// disappears on its own on the very next GetRun call, no second manual
	// step required.
	if c.questionsStore != nil {
		if all, qerr := c.questionsStore.ListWorkflowQuestionsByRun(ctx, runID); qerr == nil {
			detail.Questions = all
		}
		if open, oerr := c.questionsStore.ListOpenWorkflowQuestionsByRun(ctx, runID); oerr == nil && len(open) > 0 {
			detail.NextAction = nextActionForOpenQuestion(open[0])
		} else if waitingForCapacity != "" {
			// Checkpoint 8K-B: a resolving question stuck on provider
			// capacity only wins the NextAction slot when no higher-priority
			// pending/human_required question override already applies —
			// "an open question always wins" (8K-A's own framing) extends
			// naturally to "an open question always wins over a capacity
			// wait" here.
			detail.NextAction = waitingForCapacity
		}
	}

	// Checkpoint 8N: surface the soonest still-open durable wake for this
	// run, read live exactly like Routing/ReviewPolicy above — never
	// persisted onto RunDetail beyond this one call.
	if c.wakeScheduler != nil {
		if next, werr := c.wakeScheduler.NextForRun(ctx, domain.WorkflowRunID(runID)); werr == nil && next != nil {
			at := next.ScheduledAt
			detail.NextWakeAt = &at
			detail.WaitReason = string(next.Reason)
			detail.WakeAttemptCount = next.AttemptCount
		}
	}

	// The normalized capacity-wait projection, read the same live way. Derived
	// after the wake lookup above because it reports that wake's next attempt
	// and attempt count as part of one coherent answer.
	detail.CapacityWait = c.deriveCapacityWait(ctx, detail)

	// Checkpoint 8P-E.11: surface the structured branch wait, read the same
	// live way. Guarded on the run actually being in Waiting so a checkpoint
	// left over from an earlier, already-resolved wait can never be shown as
	// a current one.
	if detail.Run.State == domain.WorkflowRunWaiting {
		if cps, cperr := c.store.ListWorkflowCheckpoints(ctx, runID); cperr == nil {
			detail.BranchWait = branchWaitFromCheckpoints(cps)
			c.enrichBranchWait(ctx, detail.BranchWait)
		}
	}

	// Checkpoint 8P-E.12: a standalone autonomous run needs the same headless
	// heartbeat a master run has had since 8P-D. Without this, the only thing
	// driving a single-task run's review->fix->verify cascade was the
	// renderer's 2s poll, so closing the workflow page silently stalled the
	// run until the next daemon restart — the "it says Inactive and nothing
	// ever happens" report this checkpoint exists to fix. A child run of a
	// master is deliberately excluded: its parent's own heartbeat already
	// drives it through reconcileMasterTasks, and a second wake would just
	// double-drive the same cascade.
	if detail.Run.ParentWorkflowID == nil {
		c.maybeScheduleAutonomousHeartbeat(ctx, runID)
	}

	return detail, nil
}

// StartRun transitions a pending run to running: executes the deterministic
// plan step synchronously to completion, unblocks the work step (a one-off
// hardcoded "plan just finished, unblock work" edge — not a generic
// dependency-resolution engine), and dispatches its Codex worker spawn
// idempotently. Calling it again on a non-pending, non-terminal run is a
// belt-and-suspenders idempotent no-op that just returns the current detail;
// dispatchWorkStep's own outbox-level guards are the deeper idempotency layer.
func (c *Coordinator) StartRun(ctx stdctx.Context, runID string) (RunDetail, error) {
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	if !ok {
		return RunDetail{}, fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	if run.State.Terminal() {
		return RunDetail{}, fmt.Errorf("%w: workflow run %q is already %s", ErrAlreadyTerminal, runID, run.State)
	}

	steps, err := c.store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	var planStep, workStep *domain.WorkflowStep
	for i := range steps {
		switch steps[i].Kind {
		case domain.WorkflowStepPlan:
			planStep = &steps[i]
		case domain.WorkflowStepWork:
			workStep = &steps[i]
		}
	}

	// CP24-CP27 (docs/worker-lifecycle-audit.md): StartRun used to refuse
	// re-entry on `run.State != pending` -- the state its OWN first write
	// produces. Every one of the four writes that follow that transition is
	// therefore a crash window with no way back: the run sits `running` with
	// a `ready`/`running`/`completed` plan step and a `pending` work step,
	// forever. Boot recovery cannot see it either, because nothing is
	// contradicting anything; the state is merely unreachable. CP27 is the
	// purest form: a durably completed producer whose consumer was never
	// unblocked.
	//
	// Re-entry is now conditioned on the OBLIGATION rather than on the
	// caller's history: if the plan->work unblock this call owes is still
	// outstanding, StartRun re-enters and finishes it, however many times it
	// takes. Every write below is a compare-and-set against the state this
	// call actually observed, so a second concurrent caller loses its CAS and
	// is rejected rather than double-driving the run.
	resuming := false
	if run.State != domain.WorkflowRunPending {
		if planStep == nil || workStep == nil || !planUnblockOwed(*planStep, *workStep) || !startResumableRunState(run.State) {
			return c.GetRun(ctx, runID)
		}
		resuming = true
	}
	if planStep == nil || workStep == nil {
		return RunDetail{}, fmt.Errorf("%w: workflow run %q is missing its plan/work step", ErrInvalid, runID)
	}

	now := c.clock()
	if run.State != domain.WorkflowRunRunning {
		moved, err := c.store.UpdateWorkflowRunState(ctx, runID, run.State, domain.WorkflowRunRunning, now)
		if err != nil {
			return RunDetail{}, err
		}
		if !moved {
			// Someone else moved this run between the read above and here.
			// Losing the CAS means this writer is stale: it must not carry on
			// driving a run from a state that no longer exists.
			return c.GetRun(ctx, runID)
		}
	}
	if resuming {
		c.recordStartResumed(ctx, run, *planStep, *workStep)
	}
	run.State = domain.WorkflowRunRunning

	artifact, err := UnmarshalPlanArtifact(planStep.ArtifactJSON)
	if err != nil {
		return RunDetail{}, err
	}
	if artifact.Objective == "" {
		artifact = BuildPlanArtifact(run.ProjectID, run.Objective, run.PolicyVersion)
	}
	artifactJSON, err := MarshalPlanArtifact(artifact)
	if err != nil {
		return RunDetail{}, err
	}

	planState := planStep.State
	if planState == domain.WorkflowStepReady {
		moved, err := c.store.UpdateWorkflowStepState(ctx, planStep.ID, domain.WorkflowStepReady, domain.WorkflowStepRunning, now)
		if err != nil {
			return RunDetail{}, err
		}
		if moved {
			planState = domain.WorkflowStepRunning
		}
	}
	if _, err := c.store.UpdateWorkflowStepArtifact(ctx, planStep.ID, artifactJSON, now); err != nil {
		return RunDetail{}, err
	}
	// CP25/CP26: the plan step is not always `running` on re-entry. Boot
	// recovery's generic interrupted-step fallback moves a `running` non-work
	// step to `waiting`, so a resumed StartRun can find it there -- and the
	// old unconditional running->completed CAS silently matched zero rows,
	// leaving the plan step parked and the work step about to be dispatched
	// underneath it. Complete it from wherever it actually is, and treat a
	// lost CAS as a stale writer rather than as success.
	if planState != domain.WorkflowStepCompleted {
		moved, err := c.store.UpdateWorkflowStepState(ctx, planStep.ID, planState, domain.WorkflowStepCompleted, now)
		if err != nil {
			return RunDetail{}, err
		}
		if !moved {
			current, ok, gerr := c.getWorkflowStep(ctx, runID, planStep.ID)
			if gerr != nil {
				return RunDetail{}, gerr
			}
			if !ok || current.State != domain.WorkflowStepCompleted {
				return RunDetail{}, fmt.Errorf("%w: workflow run %q plan step could not be completed from %s", ErrInvalid, runID, planState)
			}
		}
	}

	if workStep.State == domain.WorkflowStepPending {
		moved, err := c.store.UpdateWorkflowStepState(ctx, workStep.ID, domain.WorkflowStepPending, domain.WorkflowStepReady, now)
		if err != nil {
			return RunDetail{}, err
		}
		if !moved {
			// Another caller unblocked it first; re-read rather than
			// dispatching against a state this call never observed.
			current, ok, gerr := c.getWorkflowStep(ctx, runID, workStep.ID)
			if gerr != nil {
				return RunDetail{}, gerr
			}
			if !ok {
				return c.GetRun(ctx, runID)
			}
			workStep = &current
		} else {
			workStep.State = domain.WorkflowStepReady
		}
	}

	prompt := BuildWorkStepPromptWithSpec(artifact,
		RenderEffectiveSpecification(c.effectiveTaskSpecification(ctx, run, artifact.AcceptanceCriteria)))
	if _, err := c.dispatchWorkStep(ctx, run, *workStep, prompt, false); err != nil {
		return RunDetail{}, err
	}

	return c.GetRun(ctx, runID)
}

// ContinueRun is Checkpoint 8C's explicit unblock-and-dispatch entry point,
// named generically (not "startReview") so it stays reusable without a
// breaking API change. Checkpoint 8D widens what it drives (the full
// review<->fix cascade, see advanceReviewFixCycle) but its contract is
// unchanged: any state with nothing eligible to dispatch is an idempotent
// no-op that just returns the current detail, mirroring StartRun's shape.
//
// 8C->8D root-cause fix (double-continue bug): manual E2E testing of 8C
// found that the FIRST POST /continue call sometimes left the review step
// "pending" with reviewRunId: null, requiring a second identical call to
// actually dispatch. Root cause: ContinueRun read the work step's state
// straight from storage without ever opportunistically observing it first
// (unlike GetRun, which always does before deciding anything) — so if the
// real Codex worker had already finished but no prior GetRun poll had yet
// observed that fact, the work step was still durably "running" in the DB,
// dispatchReviewStep's own `workStep.State != Completed` guard correctly
// no-op'd, and the review step stayed "pending" until some later GetRun call
// happened to observe completion first. The fix: ContinueRun now calls
// observeWorkStep itself, within this same call, before evaluating anything
// downstream — exactly mirroring GetRun's existing pattern — so a single
// call reliably dispatches whenever the underlying state is genuinely
// eligible, without depending on interleaving with the frontend's separate
// 2s poll. See TestContinueRunSingleCallDispatchesReviewFromCompletedWork
// for the regression test.
func (c *Coordinator) ContinueRun(ctx stdctx.Context, runID string) (RunDetail, error) {
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	if !ok {
		return RunDetail{}, fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	if run.State.Terminal() {
		// A terminal run is normally nothing to continue. The exception is a run
		// left incoherent by a concurrent verification that lost its decision
		// and acted anyway: terminal, with a step the loser started still in
		// flight. That is not a run to continue, it is a run to repair, and this
		// is the person-driven entry point where repairing it belongs. It never
		// completes, cancels or re-decides anything -- see
		// verify_race_reconcile.go. For every coherent terminal run it is a
		// no-op and the refusal below is unchanged.
		terminalSteps, serr := c.store.ListWorkflowSteps(ctx, runID)
		if serr != nil {
			return RunDetail{}, serr
		}
		// A completed child is exactly where a re-baseline belongs: the review
		// and the verification it rests on have both finished, and integration
		// has not run yet. It appends a fact and moves nothing, so it is safe
		// on a terminal run in a way a state transition would not be.
		if _, berr := c.reconcileVerifiedIntegrationBaseline(ctx, run, terminalSteps); berr != nil {
			return RunDetail{}, berr
		}
		repaired, rerr := c.reconcileVerifyRace(ctx, run, terminalSteps)
		if rerr != nil {
			return RunDetail{}, rerr
		}
		if repaired {
			return c.GetRun(ctx, runID)
		}
		return RunDetail{}, fmt.Errorf("%w: workflow run %q is already %s", ErrAlreadyTerminal, runID, run.State)
	}

	// Checkpoint 8N.1/8P-D: a master/objective run never has its own
	// work/review step (only a single "plan" step) — the lookup below would
	// wrongly reject it as ErrInvalid at any plan stage past Pending. A run
	// parked mid-planning by parkPlanForCapacity (plan.Status reset to
	// Pending, exactly the state GeneratePlan's own status switch falls
	// through to actual generation on) retries through the exact same
	// GeneratePlan entry point a human-driven retry uses. Any other plan
	// stage (Validated-awaiting-approval, Approved-with-tasks-in-flight)
	// delegates entirely to GetRun, whose getMasterRun/reconcileMasterTasks
	// path already knows how to advance task dispatch/review/fix/verify/
	// integration — this is what makes ContinueRun (the wakepoller's only
	// entry point, see wakepoller.Resumer) a valid headless resume call for a
	// master run at any stage, not just the Pending one.
	if c.planStore != nil {
		if plan, isMaster, perr := c.planStore.GetWorkflowPlan(ctx, runID); perr == nil && isMaster {
			if plan.Status == domain.WorkflowPlanPending {
				return c.GeneratePlan(ctx, runID)
			}
			// CP11/CP12: this is the wake poller's only entry point, so a
			// validated-but-never-approved autonomous objective has to be
			// resolvable from here too -- not only at boot. Without it the
			// heartbeat wakes, finds a non-pending plan, delegates to GetRun,
			// and GetRun reconciles nothing because the plan is not approved.
			if aerr := c.resumeValidatedPlan(ctx, run, plan); aerr != nil {
				return RunDetail{}, aerr
			}
			return c.GetRun(ctx, runID)
		}
	}

	// P1-A: an autonomous `task` run is created pending, with no planner to
	// kick it off, so this -- the wake poller's only entry point, and the
	// Continue button's -- is where it starts. StartRun is idempotent, so a
	// repeated wake cannot start a second worker, and every other run shape
	// falls straight through unchanged.
	if run.State == domain.WorkflowRunPending && run.ParentWorkflowID == nil {
		if sel, ok := recordedStrategy(run); ok && sel.Effective == domain.ExecutionStrategyTask &&
			policyForRun(run).Execution.AutonomousMode {
			return c.StartRun(ctx, runID)
		}
	}

	steps, err := c.store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	var workStep, reviewStep *domain.WorkflowStep
	for i := range steps {
		switch steps[i].Kind {
		case domain.WorkflowStepWork:
			workStep = &steps[i]
		case domain.WorkflowStepReview:
			reviewStep = &steps[i]
		}
	}
	if workStep == nil || reviewStep == nil {
		return RunDetail{}, fmt.Errorf("%w: workflow run %q is missing its work/review step", ErrInvalid, runID)
	}

	// The wake-triggered half of crash/restart reconciliation (the other is boot
	// recovery -- see dispatch_reconcile.go). A wake is the first thing that
	// reaches a run after the daemon that was launching for it died, so it is
	// the first opportunity to resolve the contradiction that crash left: an
	// attempt open over a launch that never happened, a worker launched and
	// never confirmed, a step RUNNING over an execution that is gone.
	//
	// It runs BEFORE every resume below, and before dispatch, because all of
	// those act on the step's CURRENT state — and reconciliation is what makes
	// that state true. It never launches anything and it never touches a live,
	// evidenced worker.
	reconciled, reconciledRun, rerr := c.ReconcileWorkStepDispatch(ctx, run, *workStep)
	if rerr != nil {
		return RunDetail{}, rerr
	}
	run = reconciledRun
	if reconciled.Action.Resolved() {
		refreshedStep, ok, serr := c.getWorkflowStep(ctx, runID, workStep.ID)
		if serr != nil {
			return RunDetail{}, serr
		}
		if ok {
			*workStep = refreshedStep
		}
	}

	// Checkpoint 8N.1: a work step parked at Ready (never dispatched — the
	// capacity-wait case, see markRunWaitingForCapacity) must be re-entered
	// into dispatchWorkStep here, exactly like Reconcile already does at boot
	// (recovery.go). Before this fix, ContinueRun only ever observed an
	// already-Running work step, so a wake firing for ReasonWorkerCapacity
	// resumed nothing: the durable wake fired, claimed, and completed, but
	// the parked step was never actually redispatched until the next daemon
	// restart's Reconcile pass happened to sweep it up.
	// A work step that is durably FAILED because its worker never launched is
	// not a verdict about the work — no worker ran, nothing was produced, and
	// the failure is AO's own dispatch. This is the one place that may reopen
	// it, for the same reason resumeStaleVerifyFailure lives here and not in
	// GetRun: THIS call is a person saying "start it". It is a no-op for every
	// step without exactly that durable evidence (including one whose spawn
	// outcome is merely ambiguous, which adoptOrMarkAmbiguous owns), and when it
	// does apply the step comes back at `ready` so the ordinary dispatch below
	// starts exactly one worker in this same call.
	if run, *workStep, _, err = c.resumeWorkerLaunchAfterFailure(ctx, run, *workStep); err != nil {
		return RunDetail{}, err
	}

	// Checkpoint 8P-E.15: the same person, on the same button, for the other
	// stop AO could reach without evidence — a work step parked on
	// "worker awaiting input" that no observed question actually supports. A
	// no-op for every run except that exact durable shape, and never a verdict
	// of its own: it returns the step to `running` so the ordinary fact-based
	// observation below re-derives what the worker is really doing.
	if run, *workStep, _, err = c.resumeFalseWorkerBlocked(ctx, run, *workStep); err != nil {
		return RunDetail{}, err
	}

	// Checkpoint 8P-E.24: the same person, on the same button, for the state
	// both overnight incidents actually ended in — AO lost track of a worker,
	// the worker's output was real, and it was committed by hand because there
	// was nowhere else for it to go. resumeAdoptedTaskCommit proves the commit
	// is this task's own (descends from its dispatch base, on its own branch, in
	// its own worktree, nothing else writing) and adopts it as the work step's
	// result, so review and verification run against it normally. It never marks
	// a task complete, and it is a no-op for every run without exactly that
	// durable evidence. Crucially it runs BEFORE the dispatch below, so an
	// adoptable changeset can never have a second writer started over it.
	// See work_adoption.go.
	var adopted bool
	if run, adopted, err = c.resumeAdoptedTaskCommit(ctx, run, steps); err != nil {
		return RunDetail{}, err
	}
	if adopted {
		if steps, err = c.store.ListWorkflowSteps(ctx, runID); err != nil {
			return RunDetail{}, err
		}
		for i := range steps {
			if steps[i].Kind == domain.WorkflowStepWork {
				workStep = &steps[i]
			}
		}
	}

	if workStep.State == domain.WorkflowStepReady || workStep.State == domain.WorkflowStepRunning {
		prompt := promptForRun(run, steps)
		updated, err := c.dispatchWorkStep(ctx, run, *workStep, prompt, true)
		if err != nil {
			return RunDetail{}, err
		}
		*workStep = updated
		if refreshed, ok2, rerr := c.store.GetWorkflowRun(ctx, runID); rerr == nil && ok2 {
			run = refreshed
		}
		if updated.State == domain.WorkflowStepRunning {
			observed, err := c.observeWorkStep(ctx, run, updated)
			if err != nil {
				return RunDetail{}, err
			}
			*workStep = observed
			if refreshed, ok2, rerr := c.store.GetWorkflowRun(ctx, runID); rerr == nil && ok2 {
				run = refreshed
			}
		}
	}

	// Checkpoint 8P-E.16: the same person, on the same button, for the one stop
	// that had no entry point at all — a fix cycle parked because its worker
	// never started it. resumeUnstartedFixCycle re-derives that evidence now and
	// re-delivers the same findings to the same session when, and only when, it
	// still holds. A no-op for every other run, including one stopped on the
	// same reason whose worker did in fact start. See fix_cycle_resume.go.
	var resumedFix bool
	if run, resumedFix, err = c.resumeUnstartedFixCycle(ctx, run); err != nil {
		return RunDetail{}, err
	}
	if resumedFix {
		// The fix step is running again and the run is no longer parked; the
		// cascade below must see that, not the snapshot read at entry.
		if steps, err = c.store.ListWorkflowSteps(ctx, runID); err != nil {
			return RunDetail{}, err
		}
	}

	// Checkpoint 8P-E.22: the same person, on the same button, for the stop
	// whose own advice is "apply the changes yourself" — and which then had
	// nowhere to put them. resumeHumanAppliedFix observes that the workspace
	// changed after the budget ran out and re-opens an INDEPENDENT review of
	// what is actually there, without raising the budget, consuming a fix cycle
	// or skipping the reviewer. See human_applied_fix.go.
	var humanFixed bool
	if run, humanFixed, err = c.resumeHumanAppliedFix(ctx, run); err != nil {
		return RunDetail{}, err
	}
	if humanFixed {
		if steps, err = c.store.ListWorkflowSteps(ctx, runID); err != nil {
			return RunDetail{}, err
		}
	}

	// Before any of the resume paths below decide anything: close the attempts a
	// restart or a crash abandoned. An attempt row with no outcome means "work
	// may be in flight", and every guard downstream believes it — so a fossil
	// left by a dead daemon silently refuses every recovery there is, forever,
	// with no way to tell that from a genuine refusal. Reaping is proof-bound
	// and closes nothing AO cannot show was abandoned (see attempt_reaper.go);
	// for the overwhelming majority of runs it is a no-op. It happens here, on
	// the person's own button, rather than in a poll, for the same reason the
	// resumes do.
	if reaped, rerr := c.reapOrphanedAttempts(ctx, run, steps); rerr != nil {
		return RunDetail{}, rerr
	} else if reaped > 0 {
		if steps, err = c.store.ListWorkflowSteps(ctx, runID); err != nil {
			return RunDetail{}, err
		}
	}

	// The same re-baseline for a run that is still live, so a task whose branch
	// moved does not have to reach a terminal state first.
	if _, rerr := c.reconcileVerifiedIntegrationBaseline(ctx, run, steps); rerr != nil {
		return RunDetail{}, rerr
	}

	// And the ledger's other fossil: an integration fresh-review request that a
	// review actually answered, but which nothing ever recorded as answered
	// because the run stopped before it integrated. pendingFreshReview consults
	// integration requests before every other reason a review step can rest, so
	// a stale one shadows them all and the run waits forever on a question that
	// was settled hours earlier. Closing it needs proof that a specific review
	// answered it (see task_integration_fresh_review_reconcile.go); a request
	// genuinely still open keeps blocking, exactly as it should.
	if _, rerr := c.reconcileIntegrationFreshReviewAnswer(ctx, run, steps); rerr != nil {
		return RunDetail{}, rerr
	}

	// Checkpoint 8P-E.14C: this is the one place in AO where a terminal
	// verification failure can be reopened, and it is here rather than in GetRun
	// or the cascade because THIS call is the person saying "I corrected the
	// thing that was broken". resumeStaleVerifyFailure decides on the run's own
	// durable evidence whether that licence applies; when it does not, it is a
	// no-op and everything below behaves exactly as it always did.
	var reopened bool
	if run, reopened, err = c.resumeStaleVerifyFailure(ctx, run, steps); err != nil {
		return RunDetail{}, err
	}
	// Checkpoint 8P-E.14D: the same person, on the same button, for the state one
	// step further along — a workspace mismatch an OLDER daemon already decided
	// and persisted inside an authorized recovery generation. resumeStaleVerify-
	// Failure cannot see it (its error-class guard is deliberately narrow, and
	// widening it would make every workspace change recoverable), so this is a
	// second, equally narrow entry point rather than a relaxation of the first.
	// Only reached when the first one did nothing, and a no-op for every run
	// without the exact durable evidence it requires.
	if !reopened {
		if run, reopened, rerr = c.resumeWorkspaceChangedVerifyRecovery(ctx, run, steps); rerr != nil {
			return RunDetail{}, rerr
		}
	}
	// And the third entry point, equally narrow: a run parked on a workspace
	// change whose cause is that the branch grew COMMITS ON TOP of the reviewed
	// commit, which still contains it. Neither of the two above can see it —
	// the first refuses the class, the second requires an authorized recovery
	// generation this run never had — and both refusals are correct, because
	// what makes this one safe is a Git ancestry proof neither of them makes.
	// A no-op for every run without exactly that durable evidence, re-derived
	// against the repository as it stands now. See verify_branch_advanced.go.
	if !reopened {
		if run, reopened, rerr = c.resumeBranchAdvancedVerify(ctx, run, steps); rerr != nil {
			return RunDetail{}, rerr
		}
	}
	// And the fourth, equally narrow: a run parked on a workspace change whose
	// difference is this task's OWN authorized work or fix output, uncommitted,
	// in this task's own worktree at an unchanged HEAD — code no reviewer has
	// read yet rather than code AO has any reason to distrust. None of the three
	// above can see it: the first refuses the class, the second requires a
	// recovery generation this run never had, and the third requires the HEAD to
	// have MOVED. What makes this one safe is an attribution proof none of them
	// makes, and it still ends in a fresh independent review — never in
	// verifying an approval that does not describe the tree. See
	// workspace_provenance.go.
	if !reopened {
		if run, reopened, rerr = c.resumeProvenanceWorkspaceChange(ctx, run, steps); rerr != nil {
			return RunDetail{}, rerr
		}
	}
	if reopened {
		// The verify step is no longer terminal and the run is no longer parked;
		// the cascade below must see that, not the snapshot read at entry.
		refreshed, err := c.store.ListWorkflowSteps(ctx, runID)
		if err != nil {
			return RunDetail{}, err
		}
		steps = refreshed
	}

	if _, err := c.advanceReviewFixCycle(ctx, run, steps, true); err != nil {
		return RunDetail{}, err
	}

	return c.GetRun(ctx, runID)
}

// policyForRun decodes a run's durable policy_snapshot into a
// domain.WorkflowPolicy, falling back to domain.DefaultWorkflowPolicy() if
// the snapshot is empty/unparseable (defensive: every run created via
// CreateRun always has a valid snapshot; this only guards pre-8D or
// hand-edited rows). Checkpoint 8D's fix-budget enforcement (fix_dispatch.go)
// always reads the policy back through this function — never a bare
// constant scattered in dispatch code.
func policyForRun(run domain.WorkflowRun) domain.WorkflowPolicy {
	var p domain.WorkflowPolicy
	if run.PolicySnapshot != "" && run.PolicySnapshot != "{}" {
		if err := json.Unmarshal([]byte(run.PolicySnapshot), &p); err == nil && p.MaxFixCycles > 0 {
			return p
		}
	}
	return domain.DefaultWorkflowPolicy()
}

// ListRuns lists workflow run summaries, optionally filtered by project id.
func (c *Coordinator) ListRuns(ctx stdctx.Context, projectID string) ([]domain.WorkflowRun, error) {
	return c.store.ListWorkflowRuns(ctx, projectID)
}

// CancelRun transitions a run to cancelled and cascades cancellation to every
// non-terminal step. Cancelling an already-terminal run is a no-op error
// (ErrAlreadyTerminal), never a silent success, and never mutates the run.
func (c *Coordinator) CancelRun(ctx stdctx.Context, runID string) (RunDetail, error) {
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	if !ok {
		return RunDetail{}, fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	if run.State.Terminal() {
		return RunDetail{}, fmt.Errorf("%w: workflow run %q is already %s", ErrAlreadyTerminal, runID, run.State)
	}

	now := c.clock()
	moved, err := c.store.UpdateWorkflowRunState(ctx, runID, run.State, domain.WorkflowRunCancelled, now)
	if err != nil {
		return RunDetail{}, err
	}
	if !moved {
		return RunDetail{}, fmt.Errorf("%w: workflow run %q changed while cancelling", ErrInvalid, runID)
	}

	// A cancelled run stops paying for work: any planner subprocess still
	// running for it is killed here. It runs on an AO-owned context rather
	// than the caller's precisely so nothing ELSE can kill it, which makes
	// this the one place that must.
	c.cancelInFlightPlanner(runID)

	// Checkpoint 8P-E13A.1: the branch is freed the instant the run is durably
	// cancelled, before any of the fallible bookkeeping below, and on a defer
	// so it happens even if that bookkeeping returns an error.
	//
	// It used to be the LAST statement in this function. Everything between the
	// state transition and it — cancelling steps, writing the left-running
	// session checkpoint, cancelling questions and resolutions, cancelling
	// wakes — can return an error, and each of those early returns produced the
	// one state AO must never be in: a run durably `cancelled` while its branch
	// lock is still `held`, with nothing left running that would ever free it.
	// Ordering is the fix, not error handling: releasing the lock cannot fail
	// the cancellation, and the cancellation must not be able to fail the
	// release. releaseBranchLocks is itself best-effort and idempotent (the
	// release SQL is CAS'd on state='held'), so running it early costs nothing
	// and a second pass would be a no-op. The work already done stays exactly
	// where it is in the repository: releasing the lock gives up ownership, it
	// never reverts anything.
	c.releaseBranchLocks(ctx, runID, "workflow run cancelled")

	steps, err := c.store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	for _, step := range steps {
		if step.State.Terminal() {
			continue
		}
		if _, err := c.store.UpdateWorkflowStepState(ctx, step.ID, step.State, domain.WorkflowStepCancelled, now); err != nil {
			return RunDetail{}, err
		}
		// Checkpoint 8B semantic: cancelling a run never stops a worker
		// session. No kill/stop port is wired here by construction. The left-
		// running session is only recorded as an informational trail so a
		// human knows to stop it manually if it should not continue.
		if step.Kind == domain.WorkflowStepWork && step.SessionID != nil {
			stepID := step.ID
			sessionID := *step.SessionID
			if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
				ID:             "wfc-" + c.newID(),
				WorkflowRunID:  runID,
				WorkflowStepID: &stepID,
				ProjectID:      run.ProjectID,
				SessionID:      &sessionID,
				NextAction: fmt.Sprintf(
					"worker session %s left running — AO does not auto-stop it on workflow cancellation; stop it manually (e.g. via the session Kill action) if it should not continue",
					sessionID,
				),
				DurablePhase:   "worker_left_running_on_cancel",
				PayloadVersion: "v1",
				RetryState:     "{}",
				CreatedAt:      now,
			}); err != nil {
				return RunDetail{}, err
			}
		}
	}

	// Checkpoint 8K-A: cancelling a run also cancels any open questions —
	// no resolver call, no delivery attempt follows. Cancelled is terminal
	// for a question just like for a step; the delivery sweep only ever
	// considers state=answered, so a cancelled question is naturally
	// excluded without any extra guard there.
	if c.questionsStore != nil {
		// Checkpoint 8K-B: also force any still-resolving question to
		// cancelled and stop trusting its in-flight resolution attempt —
		// CancelOpenWorkflowQuestionsByRun only ever touched
		// pending/human_required, so state=resolving needs its own pass
		// here. No resolver-launch, no delivery follows either transition.
		all, err := c.questionsStore.ListWorkflowQuestionsByRun(ctx, runID)
		if err != nil {
			return RunDetail{}, err
		}
		for _, q := range all {
			if q.State != domain.QuestionStateResolving {
				continue
			}
			if _, err := c.questionsStore.CancelRunningResolutionsByQuestion(ctx, string(q.ID), now); err != nil {
				return RunDetail{}, err
			}
			if _, err := c.questionsStore.TransitionWorkflowQuestionState(ctx, string(q.ID), domain.QuestionStateResolving, domain.QuestionStateCancelled, "workflow run cancelled", now); err != nil {
				return RunDetail{}, err
			}
		}
		if _, err := c.questionsStore.CancelOpenWorkflowQuestionsByRun(ctx, runID); err != nil {
			return RunDetail{}, err
		}
	}

	// Checkpoint 8N: cancelling a run must also cancel any durable wake still
	// pending/claimed for it — otherwise a wake scheduled before the cancel
	// would later fire and redispatch a run that no longer exists in an
	// active state. Best-effort like scheduleCapacityWake's own writes: a nil
	// wakeScheduler or a cancellation error never fails CancelRun itself, it
	// just means a stray wake might still be sitting in the table (the
	// poller's ContinueRun call on it is idempotent and will hit
	// ErrAlreadyTerminal, so at worst it is a wasted claim, never a wrong
	// dispatch).
	if c.wakeScheduler != nil {
		if _, werr := c.wakeScheduler.CancelAllForRun(ctx, domain.WorkflowRunID(runID)); werr != nil && c.log != nil {
			c.log.Warn("workflow: cancel wake schedules failed", "run", runID, "err", werr)
		}
	}

	// Checkpoint 8P-E.13A: a cancelled child immediately mirrors onto its
	// parent's task row instead of waiting for the parent's next reconcile
	// pass. The pass may never come — a master parked in needs_attention BECAUSE
	// of this child stops its own heartbeat — which is how "cancel the stuck
	// task" used to leave a master run showing a task that was still running.
	// Best-effort: the reconcile path still handles it, this only makes it
	// immediate.
	c.syncCancelledTask(ctx, run)

	return c.GetRun(ctx, runID)
}
