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

// provider_attempt_store.go — P1-D §F: durable storage for the provider-attempt
// ledger.
//
// The two properties the schema carries, restated because every method here
// depends on them:
//
//   - UNIQUE(run, step, lifecycle_generation, ordinal) makes the failover
//     budget durable. A restart re-reads the highest ordinal rather than
//     starting from zero, which is what stops an A->B->A loop across boots.
//   - the partial unique index over the authoritative states makes "one live
//     attempt per obligation" a constraint. Two providers cannot hold one
//     placement even if two passes race to create attempts.

func providerAttemptFromRow(r gen.ProviderAttempt) domain.ProviderAttempt {
	return domain.ProviderAttempt{
		ID:                     r.ID,
		WorkflowRunID:          r.WorkflowRunID,
		WorkflowStepID:         r.WorkflowStepID,
		TaskID:                 r.TaskID,
		ProjectID:              r.ProjectID,
		LifecycleGeneration:    r.LifecycleGeneration,
		PlacementGeneration:    r.PlacementGeneration,
		Ordinal:                r.Ordinal,
		Provider:               domain.AgentHarness(r.Provider),
		Profile:                domain.ProviderProfileID(r.ProfileID),
		State:                  domain.ProviderAttemptState(r.State),
		FailureReason:          r.FailureReason,
		FailureClass:           domain.WorkflowErrorClass(r.FailureClass),
		Safety:                 domain.FailoverSafety(r.FailoverSafety),
		MutationEvidenceDigest: r.MutationEvidenceDigest,
		RuntimeSessionID:       r.RuntimeSessionID,
		CapacityClaimID:        r.CapacityClaimID,
		PredecessorAttemptID:   r.PredecessorAttemptID,
		SuccessorAttemptID:     r.SuccessorAttemptID,
		CreatedAt:              r.CreatedAt,
		UpdatedAt:              r.UpdatedAt,
		TerminalAt:             nullTimeToTimePtr(r.TerminalAt),
	}
}

func providerAttemptsFromRows(rows []gen.ProviderAttempt) []domain.ProviderAttempt {
	out := make([]domain.ProviderAttempt, 0, len(rows))
	for _, r := range rows {
		out = append(out, providerAttemptFromRow(r))
	}
	return out
}

// CreateProviderAttempt records one provider attempt before it launches.
//
// It reports whether this call created the row. False means either the exact
// (obligation, ordinal) already exists — the idempotent repeat — or another
// attempt for the same obligation is still authoritative, which is a refusal
// the caller must respect rather than work around.
func (s *Store) CreateProviderAttempt(ctx context.Context, a domain.ProviderAttempt) (bool, error) {
	if a.ID == "" || a.WorkflowRunID == "" || a.Ordinal <= 0 || !a.State.IsKnown() {
		return false, fmt.Errorf("create provider attempt: identity is incomplete")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.CreateProviderAttempt(ctx, gen.CreateProviderAttemptParams{
		ID: a.ID, WorkflowRunID: a.WorkflowRunID, WorkflowStepID: a.WorkflowStepID,
		TaskID: a.TaskID, ProjectID: a.ProjectID,
		LifecycleGeneration: a.LifecycleGeneration, PlacementGeneration: a.PlacementGeneration,
		Ordinal: a.Ordinal, Provider: string(a.Provider), ProfileID: string(a.Profile),
		State: string(a.State), FailureReason: a.FailureReason, FailureClass: string(a.FailureClass),
		FailoverSafety: string(a.Safety), MutationEvidenceDigest: a.MutationEvidenceDigest,
		RuntimeSessionID: a.RuntimeSessionID, CapacityClaimID: a.CapacityClaimID,
		PredecessorAttemptID: a.PredecessorAttemptID, SuccessorAttemptID: a.SuccessorAttemptID,
		CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
	})
	if err != nil {
		return false, fmt.Errorf("create provider attempt: %w", err)
	}
	return n > 0, nil
}

// GetProviderAttempt reads one attempt by its durable id.
func (s *Store) GetProviderAttempt(ctx context.Context, id string) (domain.ProviderAttempt, bool, error) {
	r, err := s.qr.GetProviderAttempt(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ProviderAttempt{}, false, nil
	}
	if err != nil {
		return domain.ProviderAttempt{}, false, fmt.Errorf("get provider attempt: %w", err)
	}
	return providerAttemptFromRow(r), true, nil
}

// GetAuthoritativeProviderAttempt returns the attempt currently entitled to act
// for one obligation, if any.
func (s *Store) GetAuthoritativeProviderAttempt(ctx context.Context, runID, stepID string, lifecycleGeneration int64) (domain.ProviderAttempt, bool, error) {
	r, err := s.qr.GetAuthoritativeProviderAttempt(ctx, gen.GetAuthoritativeProviderAttemptParams{
		WorkflowRunID: runID, WorkflowStepID: stepID, LifecycleGeneration: lifecycleGeneration,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ProviderAttempt{}, false, nil
	}
	if err != nil {
		return domain.ProviderAttempt{}, false, fmt.Errorf("get authoritative provider attempt: %w", err)
	}
	return providerAttemptFromRow(r), true, nil
}

// MaxProviderAttemptOrdinal is the failover budget's durable counter.
func (s *Store) MaxProviderAttemptOrdinal(ctx context.Context, runID, stepID string, lifecycleGeneration int64) (int64, error) {
	n, err := s.qr.MaxProviderAttemptOrdinal(ctx, gen.MaxProviderAttemptOrdinalParams{
		WorkflowRunID: runID, WorkflowStepID: stepID, LifecycleGeneration: lifecycleGeneration,
	})
	if err != nil {
		return 0, fmt.Errorf("max provider attempt ordinal: %w", err)
	}
	return n, nil
}

// TransitionProviderAttempt compare-and-sets one attempt's state on its exact
// identity and expected current state.
func (s *Store) TransitionProviderAttempt(ctx context.Context, id string, expected, next domain.ProviderAttemptState, reason string, class domain.WorkflowErrorClass, safety domain.FailoverSafety, evidence string, now time.Time) (bool, error) {
	if !next.IsKnown() {
		return false, fmt.Errorf("transition provider attempt: unknown state %q", next)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// Same as the placement transition: domain owns what "terminal" means.
	var terminal sql.NullTime
	if next.Terminal() {
		terminal = sql.NullTime{Time: now, Valid: true}
	}
	n, err := s.qw.TransitionProviderAttemptState(ctx, gen.TransitionProviderAttemptStateParams{
		TerminalAt: terminal,
		NextState:  string(next), FailureReason: reason, FailureClass: string(class),
		FailoverSafety: string(safety), MutationEvidenceDigest: evidence, UpdatedAt: now,
		ID: id, ExpectedState: string(expected),
	})
	if err != nil {
		return false, fmt.Errorf("transition provider attempt: %w", err)
	}
	return n > 0, nil
}

// BindProviderAttemptRuntime records the runtime and capacity claim an attempt
// launched under. Refused for an attempt that is no longer authoritative.
func (s *Store) BindProviderAttemptRuntime(ctx context.Context, id, sessionID, capacityClaimID string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.BindProviderAttemptRuntime(ctx, gen.BindProviderAttemptRuntimeParams{
		RuntimeSessionID: sessionID, CapacityClaimID: capacityClaimID, UpdatedAt: now, ID: id,
	})
	if err != nil {
		return false, fmt.Errorf("bind provider attempt runtime: %w", err)
	}
	return n > 0, nil
}

// LinkProviderAttemptSuccessor chains a failover onto its predecessor, once.
func (s *Store) LinkProviderAttemptSuccessor(ctx context.Context, id, successorID string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.LinkProviderAttemptSuccessor(ctx, gen.LinkProviderAttemptSuccessorParams{
		SuccessorAttemptID: successorID, UpdatedAt: now, ID: id,
	})
	if err != nil {
		return false, fmt.Errorf("link provider attempt successor: %w", err)
	}
	return n > 0, nil
}

// ListProviderAttemptsForObligation returns one obligation's whole hop chain,
// in ordinal order.
func (s *Store) ListProviderAttemptsForObligation(ctx context.Context, runID, stepID string, lifecycleGeneration int64) ([]domain.ProviderAttempt, error) {
	rows, err := s.qr.ListProviderAttemptsForObligation(ctx, gen.ListProviderAttemptsForObligationParams{
		WorkflowRunID: runID, WorkflowStepID: stepID, LifecycleGeneration: lifecycleGeneration,
	})
	if err != nil {
		return nil, fmt.Errorf("list provider attempts for obligation: %w", err)
	}
	return providerAttemptsFromRows(rows), nil
}

// ListProviderAttemptsForRun returns a run's whole attempt history.
func (s *Store) ListProviderAttemptsForRun(ctx context.Context, runID string) ([]domain.ProviderAttempt, error) {
	rows, err := s.qr.ListProviderAttemptsForRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list provider attempts for run: %w", err)
	}
	return providerAttemptsFromRows(rows), nil
}

// AbandonProviderAttemptsForRun closes out a terminal run's outstanding
// attempts. Abandoned rather than failed: nothing went wrong with the provider,
// the obligation itself went away.
func (s *Store) AbandonProviderAttemptsForRun(ctx context.Context, runID, reason string, now time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.AbandonProviderAttemptsForRun(ctx, gen.AbandonProviderAttemptsForRunParams{
		FailureReason: reason, UpdatedAt: now, WorkflowRunID: runID,
	})
	if err != nil {
		return 0, fmt.Errorf("abandon provider attempts for run: %w", err)
	}
	return n, nil
}
