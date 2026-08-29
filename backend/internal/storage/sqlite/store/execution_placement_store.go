package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// execution_placement_store.go — P1-D §A: durable storage for the FROZEN
// execution placement.
//
// Every write here is either an idempotent insert or a compare-and-set. There
// is no unconditional UPDATE, and that is the whole safety argument: a pass
// working from a stale read of a placement matches zero rows and learns it is
// stale, rather than overwriting the authority somebody else established.

func executionPlacementFromRow(r gen.ExecutionPlacement) domain.ExecutionPlacement {
	return domain.ExecutionPlacement{
		ID:                  r.ID,
		WorkflowRunID:       r.WorkflowRunID,
		TaskID:              r.TaskID,
		WorkflowStepID:      r.WorkflowStepID,
		ProjectID:           r.ProjectID,
		PlacementGeneration: r.PlacementGeneration,
		LifecycleGeneration: r.LifecycleGeneration,
		Type:                domain.ExecutionPlacementType(r.PlacementType),
		RepoPath:            r.RepoPath,
		BaseBranch:          r.BaseBranch,
		BaseSHA:             r.BaseSha,
		ExecutionBranch:     r.ExecutionBranch,
		WorktreePath:        r.WorktreePath,
		WorktreeRecordID:    r.WorktreeRecordID,
		MergeTarget:         r.MergeTarget,
		OwnerToken:          r.OwnerToken,
		State:               domain.ExecutionPlacementState(r.State),
		Provenance:          domain.PlacementProvenance(r.Provenance),
		WaitingReason:       r.WaitingReason,
		IntegratedSHA:       r.IntegratedSha,
		Detail:              r.Detail,
		CreatedAt:           r.CreatedAt,
		UpdatedAt:           r.UpdatedAt,
		FinalizedAt:         nullTimeToTimePtr(r.FinalizedAt),
	}
}

func executionPlacementsFromRows(rows []gen.ExecutionPlacement) []domain.ExecutionPlacement {
	out := make([]domain.ExecutionPlacement, 0, len(rows))
	for _, r := range rows {
		out = append(out, executionPlacementFromRow(r))
	}
	return out
}

// FreezeExecutionPlacement writes a placement, once.
//
// It reports whether THIS call created the row. A false result is not a
// failure: it means the obligation already has a frozen placement, which is the
// correct outcome for every repeated reconcile, wake and restart. The caller
// then reads the existing record and uses that as the authority — which is
// exactly the behaviour §A requires, expressed as a return value rather than as
// a comment.
//
// The live partial unique index is what makes this safe under a race: two
// passes freezing different placements for the same obligation cannot both
// succeed, so a project whose mode changed between two passes cannot end up
// with a worktree AND a direct-branch claim.
func (s *Store) FreezeExecutionPlacement(ctx context.Context, p domain.ExecutionPlacement) (bool, error) {
	if !p.Valid() {
		return false, fmt.Errorf("freeze execution placement: record is not internally consistent")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.FreezeExecutionPlacement(ctx, gen.FreezeExecutionPlacementParams{
		ID: p.ID, WorkflowRunID: p.WorkflowRunID, TaskID: p.TaskID,
		WorkflowStepID: p.WorkflowStepID, ProjectID: p.ProjectID,
		PlacementGeneration: p.PlacementGeneration, LifecycleGeneration: p.LifecycleGeneration,
		PlacementType: string(p.Type), RepoPath: p.RepoPath, BaseBranch: p.BaseBranch,
		BaseSha: p.BaseSHA, ExecutionBranch: p.ExecutionBranch, WorktreePath: p.WorktreePath,
		WorktreeRecordID: p.WorktreeRecordID, MergeTarget: p.MergeTarget, OwnerToken: p.OwnerToken,
		State: string(p.State), Provenance: string(p.Provenance), WaitingReason: p.WaitingReason,
		IntegratedSha: p.IntegratedSHA, Detail: p.Detail,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	})
	if err != nil {
		// A UNIQUE violation on the live index is a legitimate answer, not an
		// error: another placement is already outstanding for this obligation.
		// Reporting it as "did not create" lets the caller read what IS
		// authoritative instead of failing a dispatch over a race it lost.
		return false, fmt.Errorf("freeze execution placement: %w", err)
	}
	return n > 0, nil
}

// GetLiveExecutionPlacement returns the current authority for one obligation.
func (s *Store) GetLiveExecutionPlacement(ctx context.Context, runID, taskID, stepID string) (domain.ExecutionPlacement, bool, error) {
	r, err := s.qr.GetLiveExecutionPlacement(ctx, gen.GetLiveExecutionPlacementParams{
		WorkflowRunID: runID, TaskID: taskID, WorkflowStepID: stepID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ExecutionPlacement{}, false, nil
	}
	if err != nil {
		return domain.ExecutionPlacement{}, false, fmt.Errorf("get live execution placement: %w", err)
	}
	return executionPlacementFromRow(r), true, nil
}

// GetExecutionPlacement returns one exact generation, terminal rows included.
// Recovery reads THIS rather than the live row: what a stale pass claims to
// hold has to be looked up by the generation it names.
func (s *Store) GetExecutionPlacement(ctx context.Context, runID, taskID, stepID string, generation int64) (domain.ExecutionPlacement, bool, error) {
	r, err := s.qr.GetExecutionPlacementByGeneration(ctx, gen.GetExecutionPlacementByGenerationParams{
		WorkflowRunID: runID, TaskID: taskID, WorkflowStepID: stepID, PlacementGeneration: generation,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ExecutionPlacement{}, false, nil
	}
	if err != nil {
		return domain.ExecutionPlacement{}, false, fmt.Errorf("get execution placement: %w", err)
	}
	return executionPlacementFromRow(r), true, nil
}

// MaxExecutionPlacementGeneration is the newest generation ever recorded for
// one obligation, terminal rows included. It is the staleness test: a caller
// holding a lower generation is stale even when no live row exists.
func (s *Store) MaxExecutionPlacementGeneration(ctx context.Context, runID, taskID, stepID string) (int64, error) {
	n, err := s.qr.MaxExecutionPlacementGeneration(ctx, gen.MaxExecutionPlacementGenerationParams{
		WorkflowRunID: runID, TaskID: taskID, WorkflowStepID: stepID,
	})
	if err != nil {
		return 0, fmt.Errorf("max execution placement generation: %w", err)
	}
	return n, nil
}

// TransitionExecutionPlacement compare-and-sets one placement's state.
func (s *Store) TransitionExecutionPlacement(ctx context.Context, runID, taskID, stepID string, generation int64, expected, next domain.ExecutionPlacementState, waitingReason, detail string, now time.Time) (bool, error) {
	if !next.IsKnown() {
		return false, fmt.Errorf("transition execution placement: unknown state %q", next)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// The state vocabulary in domain is the one definition of "terminal"; the
	// statement stamps whatever this decides rather than carrying a second list.
	var finalized sql.NullTime
	if next.Terminal() {
		finalized = sql.NullTime{Time: now, Valid: true}
	}
	n, err := s.qw.TransitionExecutionPlacementState(ctx, gen.TransitionExecutionPlacementStateParams{
		NextState: string(next), WaitingReason: waitingReason, Detail: detail, UpdatedAt: now,
		FinalizedAt:   finalized,
		WorkflowRunID: runID, TaskID: taskID, WorkflowStepID: stepID,
		PlacementGeneration: generation, ExpectedState: string(expected),
	})
	if err != nil {
		return false, fmt.Errorf("transition execution placement: %w", err)
	}
	return n > 0, nil
}

// RecordExecutionPlacementPreparation fills in the facts that only exist once
// the placement is materialised.
func (s *Store) RecordExecutionPlacementPreparation(ctx context.Context, runID, taskID, stepID string, generation int64, baseSHA, worktreePath, worktreeRecordID string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.RecordExecutionPlacementPreparation(ctx, gen.RecordExecutionPlacementPreparationParams{
		BaseSha: baseSHA, WorktreePath: worktreePath, WorktreeRecordID: worktreeRecordID, UpdatedAt: now,
		WorkflowRunID: runID, TaskID: taskID, WorkflowStepID: stepID, PlacementGeneration: generation,
	})
	if err != nil {
		return false, fmt.Errorf("record execution placement preparation: %w", err)
	}
	return n > 0, nil
}

// MarkExecutionPlacementIntegrated records the commit the work landed at, in
// the same write that moves the state. A placement that claims integration
// without naming a commit matches zero rows: the SQL requires a non-empty SHA.
func (s *Store) MarkExecutionPlacementIntegrated(ctx context.Context, runID, taskID, stepID string, generation int64, integratedSHA string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.MarkExecutionPlacementIntegrated(ctx, gen.MarkExecutionPlacementIntegratedParams{
		IntegratedSha: integratedSHA, UpdatedAt: now,
		WorkflowRunID: runID, TaskID: taskID, WorkflowStepID: stepID, PlacementGeneration: generation,
	})
	if err != nil {
		return false, fmt.Errorf("mark execution placement integrated: %w", err)
	}
	return n > 0, nil
}

// RetireSupersededExecutionPlacements makes every older non-terminal placement
// for one obligation terminal, so a replacement generation can take authority.
func (s *Store) RetireSupersededExecutionPlacements(ctx context.Context, runID, taskID, stepID string, generation int64, detail string, now time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.RetireSupersededExecutionPlacements(ctx, gen.RetireSupersededExecutionPlacementsParams{
		Detail: detail, UpdatedAt: now,
		WorkflowRunID: runID, TaskID: taskID, WorkflowStepID: stepID, PlacementGeneration: generation,
	})
	if err != nil {
		return 0, fmt.Errorf("retire superseded execution placements: %w", err)
	}
	return n, nil
}

// ListExecutionPlacementsForRun returns a run's whole placement history.
func (s *Store) ListExecutionPlacementsForRun(ctx context.Context, runID string) ([]domain.ExecutionPlacement, error) {
	rows, err := s.qr.ListExecutionPlacementsForRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list execution placements for run: %w", err)
	}
	return executionPlacementsFromRows(rows), nil
}

// ListLiveExecutionPlacements returns every outstanding placement, for
// recovery sweeps.
func (s *Store) ListLiveExecutionPlacements(ctx context.Context) ([]domain.ExecutionPlacement, error) {
	rows, err := s.qr.ListLiveExecutionPlacements(ctx)
	if err != nil {
		return nil, fmt.Errorf("list live execution placements: %w", err)
	}
	return executionPlacementsFromRows(rows), nil
}
