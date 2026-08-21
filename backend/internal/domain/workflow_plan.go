package domain

import "time"

type WorkflowPlanStatus string

const (
	WorkflowPlanPending   WorkflowPlanStatus = "pending"
	WorkflowPlanRunning   WorkflowPlanStatus = "running"
	WorkflowPlanValidated WorkflowPlanStatus = "validated"
	WorkflowPlanApproved  WorkflowPlanStatus = "approved"
	WorkflowPlanInvalid   WorkflowPlanStatus = "invalid"
	WorkflowPlanRejected  WorkflowPlanStatus = "rejected"
)

type WorkflowPlanApprovalMode string

const (
	WorkflowPlanApprovalManual WorkflowPlanApprovalMode = "manual"
	WorkflowPlanApprovalAuto   WorkflowPlanApprovalMode = "auto"
)

type WorkflowPlanCommandStatus string

const (
	WorkflowPlanCommandIdle      WorkflowPlanCommandStatus = "idle"
	WorkflowPlanCommandPending   WorkflowPlanCommandStatus = "pending"
	WorkflowPlanCommandRunning   WorkflowPlanCommandStatus = "running"
	WorkflowPlanCommandResponded WorkflowPlanCommandStatus = "responded"
	WorkflowPlanCommandCompleted WorkflowPlanCommandStatus = "completed"
	WorkflowPlanCommandFailed    WorkflowPlanCommandStatus = "failed"
)

type WorkflowPlanRecord struct {
	WorkflowRunID        string
	Status               WorkflowPlanStatus
	ApprovalMode         WorkflowPlanApprovalMode
	Provider             string
	Model                string
	PromptContextVersion string
	ContextManifestJSON  string
	GeneratedPlanJSON    string
	ValidationJSON       string
	PlanHash             string
	CommandStatus        WorkflowPlanCommandStatus
	ErrorClass           string
	CreatedAt            time.Time
	UpdatedAt            time.Time
	GeneratedAt          *time.Time
	ApprovedAt           *time.Time
	RejectedAt           *time.Time
}

type WorkflowTaskState string

const (
	WorkflowTaskBlocked   WorkflowTaskState = "blocked"
	WorkflowTaskEligible  WorkflowTaskState = "eligible"
	WorkflowTaskRunning   WorkflowTaskState = "running"
	WorkflowTaskCompleted WorkflowTaskState = "completed"
	// WorkflowTaskFailed means this task's child run ended in a state it can
	// never leave (failed or cancelled). Added by Checkpoint 8P-E.13 (migration
	// 0119): before it existed, a task whose child had failed stayed at
	// "running" forever, because "running" was the only state left that wasn't
	// a lie — and a task permanently stuck at "running" is exactly what made a
	// master run's Board card unreadable.
	WorkflowTaskFailed    WorkflowTaskState = "failed"
	WorkflowTaskCancelled WorkflowTaskState = "cancelled"
)

// Terminal reports whether a task can never change state again.
func (s WorkflowTaskState) Terminal() bool {
	return s == WorkflowTaskCompleted || s == WorkflowTaskFailed || s == WorkflowTaskCancelled
}

type WorkflowTask struct {
	ID                     string
	WorkflowRunID          string
	PlanStepID             string
	Ordinal                int64
	Title                  string
	Description            string
	AcceptanceCriteriaJSON string
	VerifyJSON             string
	State                  WorkflowTaskState
	ExecutionRunID         *string
	Dependencies           []string
	CreatedAt              time.Time
	UpdatedAt              time.Time
	CompletedAt            *time.Time
}
