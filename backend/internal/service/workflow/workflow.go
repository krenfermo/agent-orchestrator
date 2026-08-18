// Package workflow is the daemon's HTTP-facing workflow service boundary.
// The core coordinator lives in internal/workflow; this layer is the thin
// contract the API controller depends on.
package workflow

import (
	"context"

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

// ContinueRun dispatches the review step's real Claude reviewer once the
// work step has completed (idempotently); a no-op otherwise.
func (s *Service) ContinueRun(ctx context.Context, runID string) (workflowcore.RunDetail, error) {
	return s.coordinator.ContinueRun(ctx, runID)
}
