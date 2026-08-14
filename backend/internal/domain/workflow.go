package domain

import "time"

// WorkflowRunID identifies one durable workflow run.
type WorkflowRunID string

// WorkflowStepID identifies one durable workflow step within a run.
type WorkflowStepID string

// WorkflowRunState is the durable progress of a workflow run. It is an
// operational fact, not a derived display status.
type WorkflowRunState string

const (
	// WorkflowRunPending is the initial state before any step has started.
	WorkflowRunPending WorkflowRunState = "pending"
	// WorkflowRunRunning means at least one step is actively executing.
	WorkflowRunRunning WorkflowRunState = "running"
	// WorkflowRunWaiting means the run is paused awaiting some external input
	// or condition.
	WorkflowRunWaiting WorkflowRunState = "waiting"
	// WorkflowRunNeedsAttention means the run requires human or future-tooling
	// intervention before it can continue (e.g. after interrupted recovery).
	WorkflowRunNeedsAttention WorkflowRunState = "needs_attention"
	// WorkflowRunCompleted means the run finished successfully.
	WorkflowRunCompleted WorkflowRunState = "completed"
	// WorkflowRunFailed means the run ended without completing.
	WorkflowRunFailed WorkflowRunState = "failed"
	// WorkflowRunCancelled means the run was cancelled before completing.
	WorkflowRunCancelled WorkflowRunState = "cancelled"
)

// Valid reports whether a workflow run state is persistable.
func (s WorkflowRunState) Valid() bool {
	switch s {
	case WorkflowRunPending, WorkflowRunRunning, WorkflowRunWaiting,
		WorkflowRunNeedsAttention, WorkflowRunCompleted, WorkflowRunFailed,
		WorkflowRunCancelled:
		return true
	default:
		return false
	}
}

// Terminal reports whether no more progress is expected for the run.
func (s WorkflowRunState) Terminal() bool {
	return s == WorkflowRunCompleted || s == WorkflowRunFailed || s == WorkflowRunCancelled
}

// ValidWorkflowRunTransition is the persistence-level state-machine contract
// for workflow runs. Keeping it in domain prevents alternate store callers
// from bypassing the coordinator's ordering guarantees. Terminal states have
// zero outgoing transitions — not even to themselves — so a completed or
// cancelled run can never be mutated again.
func ValidWorkflowRunTransition(from, to WorkflowRunState) bool {
	if !from.Valid() || !to.Valid() || from.Terminal() {
		return false
	}
	if from == to {
		return true // metadata amendment within one durable state
	}
	switch from {
	case WorkflowRunPending:
		return to == WorkflowRunRunning || to == WorkflowRunCancelled
	case WorkflowRunRunning:
		return to == WorkflowRunWaiting || to == WorkflowRunNeedsAttention ||
			to == WorkflowRunCompleted || to == WorkflowRunFailed || to == WorkflowRunCancelled
	case WorkflowRunWaiting:
		return to == WorkflowRunRunning || to == WorkflowRunNeedsAttention ||
			to == WorkflowRunFailed || to == WorkflowRunCancelled
	case WorkflowRunNeedsAttention:
		return to == WorkflowRunRunning || to == WorkflowRunFailed || to == WorkflowRunCancelled
	default:
		return false
	}
}

// WorkflowStepKind is the fixed vocabulary of steps a workflow run can contain.
type WorkflowStepKind string

const (
	// WorkflowStepPlan is the planning step.
	WorkflowStepPlan WorkflowStepKind = "plan"
	// WorkflowStepWork is the implementation step.
	WorkflowStepWork WorkflowStepKind = "work"
	// WorkflowStepReview is the review step.
	WorkflowStepReview WorkflowStepKind = "review"
	// WorkflowStepFix is the fix-up step following review feedback.
	WorkflowStepFix WorkflowStepKind = "fix"
	// WorkflowStepVerify is the verification step.
	WorkflowStepVerify WorkflowStepKind = "verify"
	// WorkflowStepAdvance is the final advancement/merge step.
	WorkflowStepAdvance WorkflowStepKind = "advance"
)

// Valid reports whether a step kind is persistable.
func (k WorkflowStepKind) Valid() bool {
	switch k {
	case WorkflowStepPlan, WorkflowStepWork, WorkflowStepReview,
		WorkflowStepFix, WorkflowStepVerify, WorkflowStepAdvance:
		return true
	default:
		return false
	}
}

// WorkflowStepState is the durable progress of one workflow step.
type WorkflowStepState string

const (
	// WorkflowStepPending means the step is blocked on a dependency.
	WorkflowStepPending WorkflowStepState = "pending"
	// WorkflowStepReady means the step has no unmet dependency and could start.
	WorkflowStepReady WorkflowStepState = "ready"
	// WorkflowStepRunning means the step is actively executing.
	WorkflowStepRunning WorkflowStepState = "running"
	// WorkflowStepWaiting means the step is paused, including after an
	// interrupted (crash-recovered) run.
	WorkflowStepWaiting WorkflowStepState = "waiting"
	// WorkflowStepCompleted means the step finished successfully.
	WorkflowStepCompleted WorkflowStepState = "completed"
	// WorkflowStepFailed means the step ended without completing.
	WorkflowStepFailed WorkflowStepState = "failed"
	// WorkflowStepCancelled means the step was cancelled before completing.
	WorkflowStepCancelled WorkflowStepState = "cancelled"
)

// Valid reports whether a step state is persistable.
func (s WorkflowStepState) Valid() bool {
	switch s {
	case WorkflowStepPending, WorkflowStepReady, WorkflowStepRunning,
		WorkflowStepWaiting, WorkflowStepCompleted, WorkflowStepFailed,
		WorkflowStepCancelled:
		return true
	default:
		return false
	}
}

// Terminal reports whether no more progress is expected for the step.
func (s WorkflowStepState) Terminal() bool {
	return s == WorkflowStepCompleted || s == WorkflowStepFailed || s == WorkflowStepCancelled
}

// ValidWorkflowStepTransition is the persistence-level state-machine contract
// for workflow steps. Terminal states have zero outgoing transitions.
func ValidWorkflowStepTransition(from, to WorkflowStepState) bool {
	if !from.Valid() || !to.Valid() || from.Terminal() {
		return false
	}
	if from == to {
		return true // metadata amendment within one durable state
	}
	switch from {
	case WorkflowStepPending:
		return to == WorkflowStepReady || to == WorkflowStepCancelled
	case WorkflowStepReady:
		return to == WorkflowStepRunning || to == WorkflowStepCancelled
	case WorkflowStepRunning:
		return to == WorkflowStepWaiting || to == WorkflowStepCompleted ||
			to == WorkflowStepFailed || to == WorkflowStepCancelled
	case WorkflowStepWaiting:
		return to == WorkflowStepRunning || to == WorkflowStepFailed || to == WorkflowStepCancelled
	default:
		return false
	}
}

// WorkflowAttemptOutcome is the terminal result of one step attempt.
type WorkflowAttemptOutcome string

const (
	// WorkflowAttemptSucceeded means the attempt completed successfully.
	WorkflowAttemptSucceeded WorkflowAttemptOutcome = "succeeded"
	// WorkflowAttemptFailed means the attempt ended in failure.
	WorkflowAttemptFailed WorkflowAttemptOutcome = "failed"
	// WorkflowAttemptCancelled means the attempt was cancelled before finishing.
	WorkflowAttemptCancelled WorkflowAttemptOutcome = "cancelled"
)

// Valid reports whether an attempt outcome is persistable. The empty value is
// valid and means the attempt has not finished yet.
func (o WorkflowAttemptOutcome) Valid() bool {
	switch o {
	case "", WorkflowAttemptSucceeded, WorkflowAttemptFailed, WorkflowAttemptCancelled:
		return true
	default:
		return false
	}
}

// WorkflowErrorClass classifies why a workflow attempt did not succeed.
type WorkflowErrorClass string

const (
	// WorkflowErrorRateLimited means the harness hit a provider rate limit.
	WorkflowErrorRateLimited WorkflowErrorClass = "rate_limited"
	// WorkflowErrorAuth means the harness failed to authenticate.
	WorkflowErrorAuth WorkflowErrorClass = "auth"
	// WorkflowErrorTransient means a retryable infrastructure error occurred.
	WorkflowErrorTransient WorkflowErrorClass = "transient"
	// WorkflowErrorTool means a tool invocation failed.
	WorkflowErrorTool WorkflowErrorClass = "tool"
	// WorkflowErrorTestFailed means an automated test run failed.
	WorkflowErrorTestFailed WorkflowErrorClass = "test_failed"
	// WorkflowErrorReviewChangesRequested means a review pass requested changes.
	WorkflowErrorReviewChangesRequested WorkflowErrorClass = "review_changes_requested"
	// WorkflowErrorSessionCreateFailed means Spawn failed before or at session
	// row creation (Checkpoint 8B).
	WorkflowErrorSessionCreateFailed WorkflowErrorClass = "session_create_failed"
	// WorkflowErrorAgentStartFailed means the session row was created but the
	// agent process failed to launch (Checkpoint 8B).
	WorkflowErrorAgentStartFailed WorkflowErrorClass = "agent_start_failed"
	// WorkflowErrorPromptDeliveryFailed means the initial task prompt failed to
	// deliver to the spawned worker (Checkpoint 8B).
	WorkflowErrorPromptDeliveryFailed WorkflowErrorClass = "prompt_delivery_failed"
	// WorkflowErrorRuntimeFailed means the underlying terminal/runtime failed
	// independent of the agent process itself (Checkpoint 8B).
	WorkflowErrorRuntimeFailed WorkflowErrorClass = "runtime_failed"
	// WorkflowErrorWorkerTerminatedUnexpectedly means the worker session ended
	// with no evidence of committed work (Checkpoint 8B).
	WorkflowErrorWorkerTerminatedUnexpectedly WorkflowErrorClass = "worker_terminated_unexpectedly"
	// WorkflowErrorAmbiguousWorkerState means AO could not durably prove
	// whether a dispatch/worker attempt succeeded or failed, so it surfaces the
	// ambiguity rather than guessing (Checkpoint 8B; "nunca asumir éxito").
	WorkflowErrorAmbiguousWorkerState WorkflowErrorClass = "ambiguous_worker_state"
)

// Valid reports whether an error class is persistable. The empty value is
// valid and means no error has been recorded.
func (c WorkflowErrorClass) Valid() bool {
	switch c {
	case "", WorkflowErrorRateLimited, WorkflowErrorAuth, WorkflowErrorTransient,
		WorkflowErrorTool, WorkflowErrorTestFailed, WorkflowErrorReviewChangesRequested,
		WorkflowErrorSessionCreateFailed, WorkflowErrorAgentStartFailed,
		WorkflowErrorPromptDeliveryFailed, WorkflowErrorRuntimeFailed,
		WorkflowErrorWorkerTerminatedUnexpectedly, WorkflowErrorAmbiguousWorkerState:
		return true
	default:
		return false
	}
}

// WorkflowOutboxCommandType is the fixed vocabulary of durable idempotent
// commands the workflow outbox can stage. Nothing dispatches these yet.
type WorkflowOutboxCommandType string

const (
	// WorkflowOutboxSpawnWorkerSession stages a worker-session spawn command.
	WorkflowOutboxSpawnWorkerSession WorkflowOutboxCommandType = "spawn_worker_session"
	// WorkflowOutboxTriggerReview stages a review-trigger command.
	WorkflowOutboxTriggerReview WorkflowOutboxCommandType = "trigger_review"
	// WorkflowOutboxSendMessage stages a send-message command.
	WorkflowOutboxSendMessage WorkflowOutboxCommandType = "send_message"
	// WorkflowOutboxCancelSession stages a cancel-session command.
	WorkflowOutboxCancelSession WorkflowOutboxCommandType = "cancel_session"
)

// Valid reports whether an outbox command type is persistable.
func (t WorkflowOutboxCommandType) Valid() bool {
	switch t {
	case WorkflowOutboxSpawnWorkerSession, WorkflowOutboxTriggerReview,
		WorkflowOutboxSendMessage, WorkflowOutboxCancelSession:
		return true
	default:
		return false
	}
}

// WorkflowOutboxStatus is the durable dispatch progress of one outbox entry.
type WorkflowOutboxStatus string

const (
	// WorkflowOutboxPending means the entry has not been dispatched yet.
	WorkflowOutboxPending WorkflowOutboxStatus = "pending"
	// WorkflowOutboxDispatched means the entry was sent to its target.
	WorkflowOutboxDispatched WorkflowOutboxStatus = "dispatched"
	// WorkflowOutboxAcknowledged means the entry's dispatch was confirmed.
	WorkflowOutboxAcknowledged WorkflowOutboxStatus = "acknowledged"
	// WorkflowOutboxFailed means the entry's dispatch failed.
	WorkflowOutboxFailed WorkflowOutboxStatus = "failed"
)

// Valid reports whether an outbox status is persistable.
func (s WorkflowOutboxStatus) Valid() bool {
	switch s {
	case WorkflowOutboxPending, WorkflowOutboxDispatched, WorkflowOutboxAcknowledged, WorkflowOutboxFailed:
		return true
	default:
		return false
	}
}

// WorkflowRun is one durable workflow run: an objective decomposed into an
// ordered chain of steps.
type WorkflowRun struct {
	ID             string
	ProjectID      string
	Objective      string
	State          WorkflowRunState
	PolicyVersion  string
	PolicySnapshot string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	CompletedAt    *time.Time
	CancelledAt    *time.Time
}

// WorkflowStep is one step in a workflow run's linear chain.
type WorkflowStep struct {
	ID                       string
	WorkflowRunID            string
	Kind                     WorkflowStepKind
	Ordinal                  int64
	DependsOnStepID          *string
	State                    WorkflowStepState
	AssignedHarness          string
	SessionID                *string
	ReviewRunID              *string
	ExpectedArtifactsVersion string
	// ArtifactJSON is a generic small JSON slot (Checkpoint 8B). The plan step
	// stores its structured PlanArtifact here; other step kinds may use it
	// later. Defaults to "{}".
	ArtifactJSON string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	CompletedAt  *time.Time
}

// WorkflowAttempt is one execution attempt of a workflow step.
type WorkflowAttempt struct {
	ID             string
	WorkflowStepID string
	AttemptNumber  int64
	Harness        string
	Model          string
	StartedAt      time.Time
	FinishedAt     *time.Time
	Outcome        WorkflowAttemptOutcome
	ErrorClass     WorkflowErrorClass
	RetryAfter     *time.Time
}

// WorkflowCheckpoint is one append-only durable checkpoint recorded while a
// workflow step attempt progresses. Never updated; a new row is inserted to
// advance.
type WorkflowCheckpoint struct {
	ID             string
	WorkflowRunID  string
	WorkflowStepID *string
	AttemptID      *string
	ProjectID      string
	SessionID      *string
	Branch         string
	WorktreePath   string
	BaseSHA        string
	HeadSHA        string
	ReviewRunID    *string
	ReviewVerdict  string
	RetryState     string
	NextAction     string
	DurablePhase   string
	PayloadVersion string
	CreatedAt      time.Time
}

// WorkflowOutboxEntry is one durable idempotent-command staging row. Nothing
// dispatches these yet in this checkpoint.
type WorkflowOutboxEntry struct {
	ID             string
	WorkflowRunID  string
	WorkflowStepID *string
	IdempotencyKey string
	CommandType    WorkflowOutboxCommandType
	Payload        string
	Status         WorkflowOutboxStatus
	CreatedAt      time.Time
	DispatchedAt   *time.Time
	AcknowledgedAt *time.Time
	FailedAt       *time.Time
	ErrorClass     string
}
