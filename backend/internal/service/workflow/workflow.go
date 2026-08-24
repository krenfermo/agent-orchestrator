// Package workflow is the daemon's HTTP-facing workflow service boundary.
// The core coordinator lives in internal/workflow; this layer is the thin
// contract the API controller depends on.
package workflow

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// ErrInvalid, ErrNotFound, and ErrAlreadyTerminal re-export the coordinator
// sentinels so the HTTP controller maps service failures to the right status
// without importing the core package.
var (
	ErrInvalid         = workflowcore.ErrInvalid
	ErrNotFound        = workflowcore.ErrNotFound
	ErrAlreadyTerminal = workflowcore.ErrAlreadyTerminal
	ErrPlanLocked      = workflowcore.ErrPlanLocked
	// ErrArchiveUnsupported surfaces a store that cannot archive.
	ErrArchiveUnsupported = workflowcore.ErrArchiveUnsupported
)

// ListFilter narrows ListRuns. An empty ProjectID means every project.
type ListFilter struct {
	ProjectID string
}

// RunSummary is a workflow run without its step/attempt fan-out, for list views.
type RunSummary = domain.WorkflowRun

// Manager is the workflows surface the HTTP controller depends on.
type Manager interface {
	CreateRun(ctx context.Context, projectID, objective string, verification workflowcore.VerificationPlan) (workflowcore.RunDetail, error)
	GetRun(ctx context.Context, runID string) (workflowcore.RunDetail, error)
	ListRuns(ctx context.Context, filter ListFilter) ([]RunSummary, error)
	CancelRun(ctx context.Context, runID string) (workflowcore.RunDetail, error)
	StartRun(ctx context.Context, runID string) (workflowcore.RunDetail, error)
	// ContinueRun is Checkpoint 8C's generic unblock-and-dispatch entry
	// point. Today it dispatches the review step once the work step has
	// completed; a future checkpoint can extend it (fix/verify automation)
	// without a breaking API change.
	ContinueRun(ctx context.Context, runID string) (workflowcore.RunDetail, error)
	// ResumeTask releases one task parked in needs_attention (migration 0130)
	// after a person has dealt with what parked it. Idempotent.
	ResumeTask(ctx context.Context, runID, taskID string) (workflowcore.RunDetail, error)
	// AmendTaskCriterion is the Plan / Acceptance Criteria Amendment action
	// (migration 0132): a human-approved change to a criterion that stopped
	// describing reality, followed by a fresh independent review.
	AmendTaskCriterion(ctx context.Context, req TaskCriterionAmendment) (domain.WorkflowTaskCriterionAmendment, workflowcore.RunDetail, error)
	// ResumeAmendedTaskReview re-applies an existing amendment's consequences
	// when its fresh review never opened. It creates no second amendment.
	ResumeAmendedTaskReview(ctx context.Context, runID, taskID string) (workflowcore.RunDetail, error)
}

type PlannerManager interface {
	Manager
	CreateObjectiveRun(ctx context.Context, projectID, objective string, mode domain.WorkflowPlanApprovalMode) (workflowcore.RunDetail, error)
	GeneratePlan(ctx context.Context, runID string) (workflowcore.RunDetail, error)
	ApprovePlan(ctx context.Context, runID string) (workflowcore.RunDetail, error)
	RejectPlan(ctx context.Context, runID string) (workflowcore.RunDetail, error)
}

// ExecutionPolicyApplier is Checkpoint 8P-C's optional post-creation step:
// embed the caller's execution policy into a just-created run's policy
// snapshot. Optional (type-asserted by the controller, mirroring
// PlannerManager) so a Manager implementation/test double that predates
// 8P-C keeps compiling unchanged.
type ExecutionPolicyApplier interface {
	ApplyExecutionPolicySnapshot(ctx context.Context, runID string, userID domain.UserID, autonomousOverride *bool) error
}

// BoardReader is Checkpoint 8P-E.12's project Board projection. Optional
// (type-asserted by the controller, mirroring PlannerManager) so a Manager
// implementation or test double that predates it keeps compiling unchanged.
type BoardReader interface {
	ProjectBoard(ctx context.Context, projectID string, retention time.Duration) ([]workflowcore.BoardEntry, error)
}

// RunArchiver is the cancel-and-archive surface. Optional (type-asserted by
// the controller, mirroring PlannerManager/BoardReader) so a Manager
// implementation or test double that predates archiving keeps compiling.
type RunArchiver interface {
	CancelAndArchiveRun(ctx context.Context, runID string) (workflowcore.RunDetail, error)
	ProjectBoardHistory(ctx context.Context, projectID string, limit int) ([]workflowcore.BoardEntry, error)
}

// BoardHistoryLimit caps one page of the archived view. Archiving is a manual,
// per-workflow action, so this is a sanity bound on the response size rather
// than a paging scheme.
const BoardHistoryLimit = 100

// BoardTerminalRetention is how long a finished run stays on the Board. A run
// that vanishes the instant it succeeds is indistinguishable from one that
// never ran, so completion is worth showing for a while.
const BoardTerminalRetention = 30 * time.Minute

// Service is the API-facing workflow service. It delegates to the core coordinator.
type Service struct {
	coordinator *workflowcore.Coordinator
}

var _ Manager = (*Service)(nil)

// New wraps a core workflow coordinator as the API-facing service.
func New(coordinator *workflowcore.Coordinator) *Service {
	return &Service{coordinator: coordinator}
}

// CreateRun creates a new workflow run and seeds its initial steps.
func (s *Service) CreateRun(ctx context.Context, projectID, objective string, verification workflowcore.VerificationPlan) (workflowcore.RunDetail, error) {
	return s.coordinator.CreateRun(ctx, projectID, objective, verification)
}

func (s *Service) CreateObjectiveRun(ctx context.Context, projectID, objective string, mode domain.WorkflowPlanApprovalMode) (workflowcore.RunDetail, error) {
	return s.coordinator.CreateObjectiveRun(ctx, projectID, objective, mode)
}

// ApplyExecutionPolicySnapshot implements ExecutionPolicyApplier.
func (s *Service) ApplyExecutionPolicySnapshot(ctx context.Context, runID string, userID domain.UserID, autonomousOverride *bool) error {
	return s.coordinator.ApplyExecutionPolicySnapshot(ctx, runID, userID, autonomousOverride)
}
func (s *Service) GeneratePlan(ctx context.Context, runID string) (workflowcore.RunDetail, error) {
	return s.coordinator.GeneratePlan(ctx, runID)
}
func (s *Service) ApprovePlan(ctx context.Context, runID string) (workflowcore.RunDetail, error) {
	return s.coordinator.ApprovePlan(ctx, runID)
}
func (s *Service) RejectPlan(ctx context.Context, runID string) (workflowcore.RunDetail, error) {
	return s.coordinator.RejectPlan(ctx, runID)
}

// GetRun returns one workflow run with its steps and attempts.
func (s *Service) GetRun(ctx context.Context, runID string) (workflowcore.RunDetail, error) {
	return s.coordinator.GetRun(ctx, runID)
}

// ListRuns lists workflow run summaries, optionally filtered by project id.
func (s *Service) ListRuns(ctx context.Context, filter ListFilter) ([]RunSummary, error) {
	return s.coordinator.ListRuns(ctx, filter.ProjectID)
}

// CancelRun cancels a workflow run and cascades cancellation to its non-terminal steps.
func (s *Service) CancelRun(ctx context.Context, runID string) (workflowcore.RunDetail, error) {
	return s.coordinator.CancelRun(ctx, runID)
}

// StartRun transitions a pending run to running, runs its plan step, and
// dispatches its work step's Codex worker (idempotently).
func (s *Service) StartRun(ctx context.Context, runID string) (workflowcore.RunDetail, error) {
	return s.coordinator.StartRun(ctx, runID)
}

// ProjectBoard implements BoardReader.
func (s *Service) ProjectBoard(ctx context.Context, projectID string, retention time.Duration) ([]workflowcore.BoardEntry, error) {
	return s.coordinator.ProjectBoard(ctx, projectID, retention)
}

// CancelAndArchiveRun implements RunArchiver: it cancels the run and its
// non-terminal children through the canonical cancellation lifecycle, then
// marks the run archived so the active Board stops showing it. Deletes nothing.
func (s *Service) CancelAndArchiveRun(ctx context.Context, runID string) (workflowcore.RunDetail, error) {
	return s.coordinator.CancelAndArchiveRun(ctx, runID)
}

// ProjectBoardHistory implements RunArchiver: the archived ("Mostrar
// archivados") projection of a project's workflows.
func (s *Service) ProjectBoardHistory(ctx context.Context, projectID string, limit int) ([]workflowcore.BoardEntry, error) {
	return s.coordinator.ProjectBoardHistory(ctx, projectID, limit)
}

// ContinueRun dispatches the review step's real Claude reviewer once the
// work step has completed (idempotently); a no-op otherwise.
func (s *Service) ContinueRun(ctx context.Context, runID string) (workflowcore.RunDetail, error) {
	return s.coordinator.ContinueRun(ctx, runID)
}

// TaskCriterionAmendment is one human-approved amendment, at the service
// boundary. It mirrors workflowcore.TaskCriterionAmendmentRequest rather than
// importing that vocabulary into the transport layer.
type TaskCriterionAmendment struct {
	RunID             string
	TaskID            string
	CriterionIndex    int
	OriginalCriterion string
	AmendedCriterion  string
	Reason            string
	Evidence          []string
	ApprovedBy        string
}

// AmendTaskCriterion records the amendment, applies it, and returns the run
// with its review re-opened. It never approves the work.
func (s *Service) AmendTaskCriterion(ctx context.Context, req TaskCriterionAmendment) (domain.WorkflowTaskCriterionAmendment, workflowcore.RunDetail, error) {
	amendment, err := s.coordinator.AmendTaskAcceptanceCriterion(ctx, workflowcore.TaskCriterionAmendmentRequest{
		RunID: req.RunID, TaskID: req.TaskID, CriterionIndex: req.CriterionIndex,
		OriginalCriterion: req.OriginalCriterion, AmendedCriterion: req.AmendedCriterion,
		Reason: req.Reason, Evidence: req.Evidence, ApprovedBy: req.ApprovedBy,
	})
	if err != nil {
		return domain.WorkflowTaskCriterionAmendment{}, workflowcore.RunDetail{}, err
	}
	detail, err := s.coordinator.GetRun(ctx, req.RunID)
	return amendment, detail, err
}

// ResumeAmendedTaskReview finishes an amendment whose fresh review never got
// opened. Idempotent: a task already moving is left alone.
func (s *Service) ResumeAmendedTaskReview(ctx context.Context, runID, taskID string) (workflowcore.RunDetail, error) {
	if err := s.coordinator.ResumeAmendedTaskReview(ctx, runID, taskID); err != nil {
		return workflowcore.RunDetail{}, err
	}
	return s.coordinator.GetRun(ctx, runID)
}

// ResumeTask releases one task parked in needs_attention after a person has
// dealt with what parked it, and returns the run as it then is.
//
// Idempotent: resuming a task that is not parked changes nothing and is not an
// error, so a repeated request cannot produce a second integration attempt.
func (s *Service) ResumeTask(ctx context.Context, runID, taskID string) (workflowcore.RunDetail, error) {
	if err := s.coordinator.ResumeTaskAfterAttention(ctx, runID, taskID); err != nil {
		return workflowcore.RunDetail{}, err
	}
	return s.coordinator.GetRun(ctx, runID)
}
