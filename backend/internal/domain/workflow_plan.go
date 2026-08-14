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
	WorkflowTaskCancelled WorkflowTaskState = "cancelled"
)

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
