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
)

// ListFilter narrows ListRuns. An empty ProjectID means every project.
type ListFilter struct {
	ProjectID string
}

// RunSummary is a workflow run without its step/attempt fan-out, for list views.
type RunSummary = domain.WorkflowRun

// Manager is the workflows surface the HTTP controller depends on.
type Manager interface {
	CreateRun(ctx context.Context, projectID, objective string) (workflowcore.RunDetail, error)
	GetRun(ctx context.Context, runID string) (workflowcore.RunDetail, error)
	ListRuns(ctx context.Context, filter ListFilter) ([]RunSummary, error)
	CancelRun(ctx context.Context, runID string) (workflowcore.RunDetail, error)
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
func (s *Service) CreateRun(ctx context.Context, projectID, objective string) (workflowcore.RunDetail, error) {
	return s.coordinator.CreateRun(ctx, projectID, objective)
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
