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
	// ScopeJSON is the task's marshalled WorkflowTaskScope (see below). "{}"
	// for a task planned before the scope model existed, or for a run whose
	// classifier produced nothing; readers must tolerate the zero value.
	ScopeJSON      string
	State          WorkflowTaskState
	ExecutionRunID *string
	Dependencies   []string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	CompletedAt    *time.Time
}

// WorkflowTaskExecutionStrategy is how a planned task may be executed
// relative to its siblings in the same plan. It is derived once, when the
// plan is accepted, from the task DAG plus the estimated write sets, and is
// persisted with the task so a scheduler reads a decision instead of
// re-deriving one.
type WorkflowTaskExecutionStrategy string

const (
	// WorkflowTaskExecutionParallel means the task has no dependency edge and
	// no probable write conflict with any sibling: it may start immediately
	// and run alongside anything else.
	WorkflowTaskExecutionParallel WorkflowTaskExecutionStrategy = "parallel"
	// WorkflowTaskExecutionSequential means the task is ordered only by
	// dependency edges. Once its dependencies are complete it is free to run
	// alongside any non-conflicting sibling.
	WorkflowTaskExecutionSequential WorkflowTaskExecutionStrategy = "sequential"
	// WorkflowTaskExecutionSerialized means the task shares an estimated
	// write region with at least one sibling it does not depend on, so
	// running the two concurrently would probably collide. It must not
	// overlap with the partners named in its relationships.
	WorkflowTaskExecutionSerialized WorkflowTaskExecutionStrategy = "serialized"
)

// WorkflowTaskScopeSource records how much of a task's scope is a guess.
type WorkflowTaskScopeSource string

const (
	// WorkflowTaskScopeEstimated means the scope was derived only from the
	// plan text (objective, description, acceptance criteria, verify checks)
	// and the repository structure.
	WorkflowTaskScopeEstimated WorkflowTaskScopeSource = "estimated"
	// WorkflowTaskScopeObserved means the task has already run and the write
	// set below includes the paths its execution actually touched.
	WorkflowTaskScopeObserved WorkflowTaskScopeSource = "observed"
)

// WorkflowTaskScope is one planned task's durable read/write footprint. It is
// stored as JSON on the task row (workflow_tasks.scope_json) because it is
// only ever read and written as a whole, with the task that owns it.
//
// Every slice is non-nil after Normalize, sorted, and de-duplicated, so two
// runs that estimate the same scope serialize to byte-identical JSON.
type WorkflowTaskScope struct {
	// Version is the classifier policy version that produced this scope, so a
	// scope written today stays explainable when the policy changes.
	Version string                  `json:"version"`
	Source  WorkflowTaskScopeSource `json:"source"`
	// ReadPaths and WritePaths are the estimated read and write scope. A path
	// is either a file (it carries an extension) or a directory standing for
	// everything under it.
	ReadPaths  []string `json:"readPaths"`
	WritePaths []string `json:"writePaths"`
	// Packages are the likely packages/directories touched; Components are the
	// coarser top-level components those packages belong to.
	Packages   []string `json:"packages"`
	Components []string `json:"components"`
	// Files are the explicitly named files, when the plan named any. Empty
	// when the task only ever spoke in terms of packages.
	Files []string `json:"files"`
	// Symbols are the explicitly named code symbols, when the plan named any.
	Symbols []string `json:"symbols"`
	// ExecutionStrategy is how this task may be scheduled against its
	// siblings.
	ExecutionStrategy WorkflowTaskExecutionStrategy `json:"executionStrategy"`
	// IntegrationDependencies are the sibling task ids whose work must be
	// integrated before this task's. It is a superset of the task's dependency
	// edges: an earlier sibling this task probably write-conflicts with is
	// included even with no dependency edge between them, because integrating
	// them in an arbitrary order is what produces the collision.
	IntegrationDependencies []string `json:"integrationDependencies"`
	// ObservedWritePaths are the paths this task's execution actually changed,
	// recorded once the task completes. Empty until then.
	ObservedWritePaths []string `json:"observedWritePaths"`
}

// WorkflowTaskRelation is the classification of one unordered pair of planned
// tasks.
type WorkflowTaskRelation string

const (
	// WorkflowTaskRelationDependency means one task of the pair must complete
	// before the other can start; the DAG already orders them.
	WorkflowTaskRelationDependency WorkflowTaskRelation = "functional_dependency"
	// WorkflowTaskRelationWriteConflict means neither task depends on the
	// other, yet their estimated write sets overlap: running or integrating
	// them concurrently would probably collide.
	WorkflowTaskRelationWriteConflict WorkflowTaskRelation = "probable_write_conflict"
	// WorkflowTaskRelationIndependent means the pair has neither a dependency
	// edge nor an overlapping write set.
	WorkflowTaskRelationIndependent WorkflowTaskRelation = "independent"
)

// WorkflowTaskRelationship is the durable decision for one unordered task
// pair, stored so scheduling and integration read a classification instead of
// recomputing one. TaskID is always the lexicographically smaller of the two
// ids, which is what makes the pair storable exactly once.
type WorkflowTaskRelationship struct {
	WorkflowRunID string
	TaskID        string
	RelatedTaskID string
	Relation      WorkflowTaskRelation
	// Reason is a stable machine-checkable code (see workflow.TaskRelationReason).
	Reason string
	// Detail is the human-readable sentence behind Reason.
	Detail string
	// Overlap is the specific set of paths that made this a write conflict, so
	// the decision can be checked rather than trusted. Empty otherwise.
	Overlap   []string
	CreatedAt time.Time
}
