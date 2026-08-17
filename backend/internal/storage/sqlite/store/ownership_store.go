package store

import (
	"context"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// GetProjectOwner returns the project's owner_user_id, which is nil for a
// project that predates ownership or was created while no user was resolved.
func (s *Store) GetProjectOwner(ctx context.Context, id domain.ProjectID) (*domain.UserID, error) {
	owner, err := s.qr.GetProjectOwner(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get project owner %s: %w", id, err)
	}
	return owner, nil
}

// SetProjectOwner stamps a project's owner_user_id. Returns false if the
// project id doesn't exist.
func (s *Store) SetProjectOwner(ctx context.Context, id domain.ProjectID, owner domain.UserID) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.SetProjectOwner(ctx, gen.SetProjectOwnerParams{OwnerUserID: &owner, ID: id})
	if err != nil {
		return false, fmt.Errorf("set project owner %s: %w", id, err)
	}
	return n > 0, nil
}

// BackfillProjectOwners stamps owner on every project row whose
// owner_user_id is still NULL. Used once at bootstrap-admin creation so no
// pre-existing project is silently orphaned. Returns the number of rows
// updated.
func (s *Store) BackfillProjectOwners(ctx context.Context, owner domain.UserID) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.BackfillProjectOwners(ctx, &owner)
	if err != nil {
		return 0, fmt.Errorf("backfill project owners: %w", err)
	}
	return n, nil
}

// ListProjectIDsByOwner returns every project id owned by the given user.
func (s *Store) ListProjectIDsByOwner(ctx context.Context, owner domain.UserID) ([]domain.ProjectID, error) {
	ids, err := s.qr.ListProjectIDsByOwner(ctx, &owner)
	if err != nil {
		return nil, fmt.Errorf("list project ids by owner: %w", err)
	}
	return ids, nil
}

// GetWorkflowRunOwner returns the run's user_id, which is nil for a run that
// predates ownership or was created while no user was resolved.
func (s *Store) GetWorkflowRunOwner(ctx context.Context, id string) (*domain.UserID, error) {
	owner, err := s.qr.GetWorkflowRunOwner(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get workflow run owner %s: %w", id, err)
	}
	return owner, nil
}

// SetWorkflowRunOwner stamps a workflow run's user_id. Returns false if the
// run id doesn't exist.
func (s *Store) SetWorkflowRunOwner(ctx context.Context, id string, owner domain.UserID) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.SetWorkflowRunOwner(ctx, gen.SetWorkflowRunOwnerParams{UserID: &owner, ID: id})
	if err != nil {
		return false, fmt.Errorf("set workflow run owner %s: %w", id, err)
	}
	return n > 0, nil
}

// BackfillWorkflowRunOwners stamps owner on every workflow_runs row whose
// user_id is still NULL. Returns the number of rows updated.
func (s *Store) BackfillWorkflowRunOwners(ctx context.Context, owner domain.UserID) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.BackfillWorkflowRunOwners(ctx, &owner)
	if err != nil {
		return 0, fmt.Errorf("backfill workflow run owners: %w", err)
	}
	return n, nil
}

// ListWorkflowRunIDsByOwner returns every workflow run id owned by the given user.
func (s *Store) ListWorkflowRunIDsByOwner(ctx context.Context, owner domain.UserID) ([]string, error) {
	ids, err := s.qr.ListWorkflowRunIDsByOwner(ctx, &owner)
	if err != nil {
		return nil, fmt.Errorf("list workflow run ids by owner: %w", err)
	}
	return ids, nil
}
