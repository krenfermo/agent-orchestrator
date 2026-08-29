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

// execution_placement_override_store.go — P1-E §B/§C: durable storage for the
// placement OVERRIDE request and the generation TRANSITION.
//
// Same safety argument as the placement store next to it: every write is either
// an idempotent insert against a unique index or a compare-and-set on the state
// the caller read. There is no unconditional UPDATE, so a pass working from a
// stale read learns it is stale instead of overwriting somebody's decision.

func placementOverrideFromRow(r gen.ExecutionPlacementOverride) domain.ExecutionPlacementOverride {
	return domain.ExecutionPlacementOverride{
		ID:                r.ID,
		WorkflowRunID:     r.WorkflowRunID,
		TaskID:            r.TaskID,
		WorkflowStepID:    r.WorkflowStepID,
		ProjectID:         r.ProjectID,
		Requested:         domain.PlacementOverrideRequest(r.RequestedPlacement),
		RequestedBy:       r.RequestedBy,
		Reason:            r.Reason,
		State:             domain.PlacementOverrideState(r.State),
		AppliedGeneration: r.AppliedGeneration,
		Detail:            r.Detail,
		CreatedAt:         r.CreatedAt,
		UpdatedAt:         r.UpdatedAt,
		ResolvedAt:        nullTimeToTimePtr(r.ResolvedAt),
	}
}

func placementTransitionFromRow(r gen.ExecutionPlacementTransition) domain.ExecutionPlacementTransition {
	return domain.ExecutionPlacementTransition{
		ID:                  r.ID,
		WorkflowRunID:       r.WorkflowRunID,
		TaskID:              r.TaskID,
		WorkflowStepID:      r.WorkflowStepID,
		ProjectID:           r.ProjectID,
		FromGeneration:      r.FromGeneration,
		ToGeneration:        r.ToGeneration,
		FromType:            domain.ExecutionPlacementType(r.FromPlacementType),
		FromRepoPath:        r.FromRepoPath,
		FromExecutionBranch: r.FromExecutionBranch,
		FromWorktreePath:    r.FromWorktreePath,
		FromBaseSHA:         r.FromBaseSha,
		Requested:           domain.PlacementOverrideRequest(r.RequestedPlacement),
		ToType:              domain.ExecutionPlacementType(r.ToPlacementType),
		RequestedBy:         r.RequestedBy,
		Reason:              r.Reason,
		ExpectedState:       domain.ExecutionPlacementState(r.ExpectedState),
		QuiescenceDigest:    r.QuiescenceDigest,
		State:               domain.PlacementTransitionState(r.State),
		RefusalReason:       domain.PlacementTransitionRefusal(r.RefusalReason),
		Detail:              r.Detail,
		CreatedAt:           r.CreatedAt,
		UpdatedAt:           r.UpdatedAt,
	}
}

// RequestExecutionPlacementOverride records what an operator asked for, and
// supersedes whatever was outstanding for the same obligation.
//
// The supersede and the insert are one critical section on purpose. Apart, they
// are a window in which the obligation has no request at all, and a freeze
// landing in that window would silently ignore an override an operator had just
// made — which is the one failure mode a request model must not have.
func (s *Store) RequestExecutionPlacementOverride(ctx context.Context, o domain.ExecutionPlacementOverride) (bool, error) {
	if !o.Valid() {
		return false, fmt.Errorf("request execution placement override: record is not internally consistent")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.qw.SupersedeOutstandingPlacementOverrides(ctx, gen.SupersedeOutstandingPlacementOverridesParams{
		Detail:        "replaced by a newer request",
		UpdatedAt:     o.CreatedAt,
		WorkflowRunID: o.WorkflowRunID, TaskID: o.TaskID, WorkflowStepID: o.WorkflowStepID,
	}); err != nil {
		return false, fmt.Errorf("supersede outstanding placement overrides: %w", err)
	}
	n, err := s.qw.RequestExecutionPlacementOverride(ctx, gen.RequestExecutionPlacementOverrideParams{
		ID: o.ID, WorkflowRunID: o.WorkflowRunID, TaskID: o.TaskID,
		WorkflowStepID: o.WorkflowStepID, ProjectID: o.ProjectID,
		RequestedPlacement: string(o.Requested), RequestedBy: o.RequestedBy, Reason: o.Reason,
		State: string(o.State), AppliedGeneration: o.AppliedGeneration, Detail: o.Detail,
		CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt,
	})
	if err != nil {
		return false, fmt.Errorf("request execution placement override: %w", err)
	}
	return n > 0, nil
}

// GetOutstandingPlacementOverride returns the request the next freeze or
// transition will consume, if there is one.
func (s *Store) GetOutstandingPlacementOverride(ctx context.Context, runID, taskID, stepID string) (domain.ExecutionPlacementOverride, bool, error) {
	r, err := s.qr.GetOutstandingPlacementOverride(ctx, gen.GetOutstandingPlacementOverrideParams{
		WorkflowRunID: runID, TaskID: taskID, WorkflowStepID: stepID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ExecutionPlacementOverride{}, false, nil
	}
	if err != nil {
		return domain.ExecutionPlacementOverride{}, false, fmt.Errorf("get outstanding placement override: %w", err)
	}
	return placementOverrideFromRow(r), true, nil
}

// ResolvePlacementOverride marks a request consumed, naming the generation that
// consumed it. Conditioned on the row still being outstanding, so two passes
// consuming one request cannot both believe they did.
func (s *Store) ResolvePlacementOverride(ctx context.Context, id string, next domain.PlacementOverrideState, generation int64, detail string, now time.Time) (bool, error) {
	if !next.IsKnown() || next == domain.PlacementOverrideRequested {
		return false, fmt.Errorf("resolve placement override: %q is not a resolution", next)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.ResolvePlacementOverride(ctx, gen.ResolvePlacementOverrideParams{
		NextState: string(next), AppliedGeneration: generation, Detail: detail, UpdatedAt: now, ID: id,
	})
	if err != nil {
		return false, fmt.Errorf("resolve placement override: %w", err)
	}
	return n > 0, nil
}

// ListPlacementOverridesForRun returns a run's whole override history.
func (s *Store) ListPlacementOverridesForRun(ctx context.Context, runID string) ([]domain.ExecutionPlacementOverride, error) {
	rows, err := s.qr.ListPlacementOverridesForRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list placement overrides for run: %w", err)
	}
	out := make([]domain.ExecutionPlacementOverride, 0, len(rows))
	for _, r := range rows {
		out = append(out, placementOverrideFromRow(r))
	}
	return out, nil
}

// RecordPlacementTransition writes the intent BEFORE the replacement it
// authorizes, and reports whether THIS call created the row.
//
// A false result for a non-refused transition is the idempotency answer: a
// surviving transition already supersedes this generation, and the caller reads
// it back rather than minting a second one.
func (s *Store) RecordPlacementTransition(ctx context.Context, t domain.ExecutionPlacementTransition) (bool, error) {
	if t.ID == "" || t.WorkflowRunID == "" || !t.State.IsKnown() || !t.Requested.IsKnown() {
		return false, fmt.Errorf("record placement transition: record is not internally consistent")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.RecordPlacementTransition(ctx, gen.RecordPlacementTransitionParams{
		ID: t.ID, WorkflowRunID: t.WorkflowRunID, TaskID: t.TaskID,
		WorkflowStepID: t.WorkflowStepID, ProjectID: t.ProjectID,
		FromGeneration: t.FromGeneration, ToGeneration: t.ToGeneration,
		FromPlacementType: string(t.FromType), FromRepoPath: t.FromRepoPath,
		FromExecutionBranch: t.FromExecutionBranch, FromWorktreePath: t.FromWorktreePath,
		FromBaseSha:        t.FromBaseSHA,
		RequestedPlacement: string(t.Requested), ToPlacementType: string(t.ToType),
		RequestedBy: t.RequestedBy, Reason: t.Reason, ExpectedState: string(t.ExpectedState),
		QuiescenceDigest: t.QuiescenceDigest, State: string(t.State),
		RefusalReason: string(t.RefusalReason), Detail: t.Detail,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	})
	if err != nil {
		return false, fmt.Errorf("record placement transition: %w", err)
	}
	return n > 0, nil
}

// CompletePlacementTransition names the generation the replacement actually
// got. Conditioned on the transition still being `requested`, so a second pass
// cannot re-point a completed transition at a different successor.
func (s *Store) CompletePlacementTransition(ctx context.Context, id string, toGeneration int64, toType domain.ExecutionPlacementType, detail string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.CompletePlacementTransition(ctx, gen.CompletePlacementTransitionParams{
		ToGeneration: toGeneration, ToPlacementType: string(toType),
		Detail: detail, UpdatedAt: now, ID: id,
	})
	if err != nil {
		return false, fmt.Errorf("complete placement transition: %w", err)
	}
	return n > 0, nil
}

// GetSurvivingPlacementTransition returns the transition that already
// supersedes one generation, if any. Refused rows are excluded by the index, so
// a "not yet" never masquerades as a transition that happened.
func (s *Store) GetSurvivingPlacementTransition(ctx context.Context, runID, taskID, stepID string, fromGeneration int64) (domain.ExecutionPlacementTransition, bool, error) {
	r, err := s.qr.GetSurvivingPlacementTransition(ctx, gen.GetSurvivingPlacementTransitionParams{
		WorkflowRunID: runID, TaskID: taskID, WorkflowStepID: stepID, FromGeneration: fromGeneration,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ExecutionPlacementTransition{}, false, nil
	}
	if err != nil {
		return domain.ExecutionPlacementTransition{}, false, fmt.Errorf("get surviving placement transition: %w", err)
	}
	return placementTransitionFromRow(r), true, nil
}

// ListPlacementTransitionsForRun returns a run's whole transition history,
// refusals included: a refusal an operator cannot read afterwards is a refusal
// they will run into again.
func (s *Store) ListPlacementTransitionsForRun(ctx context.Context, runID string) ([]domain.ExecutionPlacementTransition, error) {
	rows, err := s.qr.ListPlacementTransitionsForRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list placement transitions for run: %w", err)
	}
	out := make([]domain.ExecutionPlacementTransition, 0, len(rows))
	for _, r := range rows {
		out = append(out, placementTransitionFromRow(r))
	}
	return out, nil
}
