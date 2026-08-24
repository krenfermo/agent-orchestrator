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
	// WorkflowTaskNeedsAttention means this task is parked on something only a
	// person can decide — today, an integration conflict AO may not resolve by
	// itself. Added by migration 0130.
	//
	// It is deliberately NOT terminal and deliberately NOT "running". A parked
	// task has not ended, so calling it failed would be a lie about work that
	// is very likely fine; but it is also not making progress, and leaving it
	// at "running" is what made the same conflict be retried on every poll,
	// re-rebasing the same worktree and writing the same checkpoint forever.
	// The only exit is ResumeTaskAfterAttention, i.e. a person.
	WorkflowTaskNeedsAttention WorkflowTaskState = "needs_attention"
)

// Terminal reports whether a task can never change state again.
func (s WorkflowTaskState) Terminal() bool {
	return s == WorkflowTaskCompleted || s == WorkflowTaskFailed || s == WorkflowTaskCancelled
}

// Parked reports whether the task is stopped awaiting a human decision. It is
// the middle ground Terminal() does not cover: not finished, and not going to
// move on its own.
func (s WorkflowTaskState) Parked() bool { return s == WorkflowTaskNeedsAttention }

// WorkflowTaskAttention is everything a person needs to act on a parked task,
// stored on the task row itself (workflow_tasks.attention_json).
//
// It lives with the task rather than only on the run's checkpoint ledger
// because it is state, not history: reconciliation reads it to know the task
// is parked and why, the Board renders it, and the resume transition clears
// it. A ledger row can say a conflict happened; only this can say the task is
// still stopped on one.
type WorkflowTaskAttention struct {
	// Reason is the stable machine-checkable code, mirrored into the
	// attention_reason column so it can be filtered without parsing JSON.
	Reason string `json:"reason"`
	// ConflictingFiles are the exact repository-relative paths that collided,
	// in git's order. Empty for a stop that is not a conflict.
	ConflictingFiles []string `json:"conflictingFiles,omitempty"`
	// SourceSHA, BaseSHA and TargetBeforeSHA are the three commits that
	// describe the situation completely: what is trying to land, what it was
	// built on, and where the target actually was.
	SourceSHA       string `json:"sourceSha,omitempty"`
	BaseSHA         string `json:"baseSha,omitempty"`
	TargetBeforeSHA string `json:"targetBeforeSha,omitempty"`
	// IntegrationStrategy is the strategy that was attempted, so a reader can
	// tell "the rebase conflicted" from "no strategy applied at all".
	IntegrationStrategy string `json:"integrationStrategy,omitempty"`
	// RecommendedAction says what a person can actually do about it, in the
	// vocabulary of the thing that went wrong.
	RecommendedAction string `json:"recommendedAction,omitempty"`
	// Detail is the sentence behind Reason.
	Detail string `json:"detail,omitempty"`
	// Attempt counts how many times this task has been integrated and parked.
	// It increments only on a human resume followed by a fresh stop, which is
	// what makes "the resume produced exactly one new attempt" checkable.
	Attempt int `json:"attempt,omitempty"`
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
	// AttentionReason and Attention are populated exactly while State is
	// WorkflowTaskNeedsAttention; the schema's own CHECK refuses a parked task
	// with no reason. Attention is the unmarshalled attention_json.
	AttentionReason string
	Attention       WorkflowTaskAttention
	AttentionAt     *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
	CompletedAt     *time.Time
}

// WorkflowTaskCriterionDisposition is what an amendment did to a criterion.
type WorkflowTaskCriterionDisposition string

const (
	// WorkflowTaskCriterionAmended replaced the criterion with a new one that
	// still has to be met.
	WorkflowTaskCriterionAmended WorkflowTaskCriterionDisposition = "amended"
	// WorkflowTaskCriterionObsolete removed the criterion. Only honest when it
	// described something outside the work itself — a precondition of the
	// environment that stopped holding for reasons the work did not cause.
	WorkflowTaskCriterionObsolete WorkflowTaskCriterionDisposition = "declared_obsolete"
)

// WorkflowTaskCriterionAmendment is one durable, human-approved change to a
// planned task's acceptance criteria (migration 0132).
//
// It exists because a criterion can describe a PRECONDITION OF THE ENVIRONMENT
// rather than a property of the work, and preconditions expire. When one does,
// the reviewer is right to keep blocking and the work is right to be blocked —
// the thing that is wrong is the criterion, and until this existed AO had no
// way to say so short of editing the database by hand or replanning the whole
// objective.
//
// Every field is a guard against the obvious abuse, which is talking a reviewer
// out of a real finding: the original text is kept forever, a reason and at
// least one piece of evidence are required, and a named human must approve it.
// The record is append-only; the task row carries the criteria as they now
// stand, and these say how they got there.
type WorkflowTaskCriterionAmendment struct {
	ID            string
	WorkflowRunID string
	TaskID        string
	// CriterionIndex is the position amended, as indexed at the time. The text
	// below is what actually identifies it: an index stops meaning anything as
	// soon as a later amendment removes an earlier criterion.
	CriterionIndex    int64
	OriginalCriterion string
	AmendedCriterion  string
	Disposition       WorkflowTaskCriterionDisposition
	Reason            string
	Evidence          []string
	ApprovedBy        string
	// SupersededReviewRunID is the review whose verdict this amendment
	// invalidated, when there was one. A verdict reached under a criterion that
	// no longer exists cannot carry over.
	SupersededReviewRunID string
	CreatedAt             time.Time
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
	// WaitingReason is the dispatcher's durable explanation for why this task
	// is currently held. Empty means it has not been evaluated as blocked.
	WaitingReason WorkflowTaskWaitingReason `json:"waitingReason,omitempty"`
	// ExecutionMode is the workspace strategy the plan selected for THIS task,
	// which is not always the project's own setting: a smart-parallel project
	// downgrades a task whose write set conflicts with a sibling's, or whose
	// write set is too uncertain to prove independent, back to a plain
	// isolated worktree.
	//
	// Empty means the plan made no per-task selection and the task simply uses
	// the project's mode. That is the case for every project configured
	// isolated_worktree or direct_branch, where all tasks share one mode and
	// there is nothing to decide -- recording a "selection" there would be
	// recording a decision nobody made, and would change the stored scope of
	// every existing plan. Read it through ResolveTaskExecutionMode, never
	// directly.
	ExecutionMode ExecutionMode `json:"executionMode,omitempty"`
	// ExecutionModeDowngrade explains why ExecutionMode is weaker than the
	// project asked for. Nil when the task got what the project configured.
	ExecutionModeDowngrade *WorkflowTaskExecutionDowngrade `json:"executionModeDowngrade,omitempty"`
	// IntegrationDependencies are the sibling task ids whose work must be
	// integrated before this task's. It is a superset of the task's dependency
	// edges: an earlier sibling this task probably write-conflicts with is
	// included even with no dependency edge between them, because integrating
	// them in an arbitrary order is what produces the collision.
	IntegrationDependencies []string `json:"integrationDependencies"`
	// ObservedWritePaths are the paths this task's execution actually changed,
	// recorded once the task completes. Empty until then.
	ObservedWritePaths []string `json:"observedWritePaths"`
	// SafeWriteOverlaps are this task's explicit waivers: overlaps with a
	// named sibling that the plan declares safe to share despite both tasks
	// writing there. Empty for almost every task -- an overlap that nobody
	// marked safe is a probable write conflict, and that default is what makes
	// the waiver meaningful.
	SafeWriteOverlaps []WorkflowTaskSafeOverlap `json:"safeWriteOverlaps"`
}

type WorkflowTaskWaitingReason string

const (
	WorkflowTaskWaitingDependency WorkflowTaskWaitingReason = "waiting_for_dependencies"
	WorkflowTaskWaitingConflict   WorkflowTaskWaitingReason = "waiting_for_write_conflict"
)

// WorkflowTaskExecutionDowngrade is the durable record of a task being denied
// the execution strategy its project configured.
//
// It exists because the downgrade is a judgement about text -- an estimated
// write set, not an observed one -- and a judgement nobody can audit is one
// nobody can correct. Storing From/To alone would say a task was demoted
// without saying what the classifier saw; Reason is the stable code a later
// build can still match on, Detail is the sentence a person reads, and
// Conflicts names the specific siblings, so the decision can be checked
// against the plan rather than trusted.
type WorkflowTaskExecutionDowngrade struct {
	// PolicyVersion is the selection-policy version that produced this
	// downgrade (workflow.TaskStrategyPolicyVersion). It is separate from the
	// scope's own Version because the rules that estimate a write set and the
	// rules that decide what to do about one can change independently.
	PolicyVersion string `json:"policyVersion"`
	// From is the project's configured mode; To is what the task actually got.
	From ExecutionMode `json:"from"`
	To   ExecutionMode `json:"to"`
	// Serial additionally forbids this task from running at the same time as
	// the siblings in Conflicts. A downgrade to a private worktree removes the
	// physical collision but not the integration one: two tasks writing the
	// same file still have to land in some order, so a write-set conflict
	// demotes all the way to serial while mere uncertainty does not.
	Serial bool `json:"serial,omitempty"`
	// Reason is a stable machine-checkable code (see
	// workflow.TaskStrategyReason).
	Reason string `json:"reason"`
	// Detail is the human-readable sentence behind Reason.
	Detail string `json:"detail"`
	// Conflicts are the sibling task ids whose write sets this task probably
	// collides with. Empty for a downgrade caused by uncertainty alone.
	Conflicts []string `json:"conflicts,omitempty"`
}

// ResolveTaskExecutionMode returns the execution mode a planned task's work
// must actually use: the strategy the plan selected for it, or the project's
// own mode when the plan selected none. Callers must use this rather than
// reading WorkflowTaskScope.ExecutionMode directly, the same convention
// ProjectConfig's Effective* accessors already establish.
func ResolveTaskExecutionMode(project ExecutionMode, scope WorkflowTaskScope) ExecutionMode {
	if scope.ExecutionMode == "" {
		return project.WithDefault()
	}
	return scope.ExecutionMode
}

// WorkflowTaskSafeOverlap is one declaration that a write-set overlap with a
// specific sibling task is not a conflict.
//
// It is deliberately narrow in two ways. It names the sibling it applies to,
// because a blanket "this task never conflicts" waiver is exactly the mistake
// the conflict default exists to catch. And it carries a Reason, because a
// waiver is a claim about the code that a person may later have to audit --
// the reason travels into the stored relationship's detail so the decision
// explains itself without anyone re-deriving why it was waived.
type WorkflowTaskSafeOverlap struct {
	// WithTaskID is the sibling task this waiver applies to.
	WithTaskID string `json:"withTaskId"`
	// Paths narrows the waiver to specific paths; a directory waives
	// everything under it. Empty waives the whole overlap with that sibling.
	Paths []string `json:"paths"`
	// Reason is why sharing these paths is safe (e.g. "append-only registry,
	// both tasks add distinct entries"). Required.
	Reason string `json:"reason"`
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
	// Overlap is where the two tasks' estimated write sets intersect, so the
	// decision can be checked rather than trusted. For a write conflict it is
	// what made it one; for a pair whose overlap was explicitly declared safe
	// it is what was waived; for a pair that simply does not intersect, and
	// for a functional dependency, it is empty.
	Overlap   []string
	CreatedAt time.Time
}
