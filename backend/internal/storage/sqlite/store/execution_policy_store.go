package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// UpsertUserExecutionPolicy inserts or replaces the caller's single
// execution policy row (Checkpoint 8P-C). p.UserID must already be
// server-side resolved identity, never client input.
func (s *Store) UpsertUserExecutionPolicy(ctx context.Context, p domain.UserExecutionPolicy) (domain.UserExecutionPolicy, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	planner, err := marshalProfileIDs(p.PlannerPriority)
	if err != nil {
		return domain.UserExecutionPolicy{}, err
	}
	worker, err := marshalProfileIDs(p.WorkerPriority)
	if err != nil {
		return domain.UserExecutionPolicy{}, err
	}
	reviewer, err := marshalProfileIDs(p.ReviewerPriority)
	if err != nil {
		return domain.UserExecutionPolicy{}, err
	}
	resolver, err := marshalProfileIDs(p.DecisionResolverPriority)
	if err != nil {
		return domain.UserExecutionPolicy{}, err
	}
	row, err := s.qw.UpsertUserExecutionPolicy(ctx, gen.UpsertUserExecutionPolicyParams{
		ID:                       string(p.ID),
		UserID:                   string(p.UserID),
		Version:                  p.Version,
		AutonomousMode:           boolToInt64(p.AutonomousMode),
		PlannerPriority:          planner,
		WorkerPriority:           worker,
		ReviewerPriority:         reviewer,
		DecisionResolverPriority: resolver,
		FallbackBehavior:         string(p.FallbackBehavior),
		ReviewIndependence:       string(p.ReviewIndependence),
		CreatedAt:                p.CreatedAt,
		UpdatedAt:                p.UpdatedAt,
	})
	if err != nil {
		return domain.UserExecutionPolicy{}, fmt.Errorf("upsert user execution policy: %w", err)
	}
	return userExecutionPolicyFromRow(row)
}

// GetUserExecutionPolicyByUser returns userID's stored policy, or
// (zero, false, nil) if none has been saved yet -- callers fall back to
// domain.DefaultUserExecutionPolicy in that case.
func (s *Store) GetUserExecutionPolicyByUser(ctx context.Context, userID domain.UserID) (domain.UserExecutionPolicy, bool, error) {
	row, err := s.qr.GetUserExecutionPolicyByUser(ctx, string(userID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.UserExecutionPolicy{}, false, nil
		}
		return domain.UserExecutionPolicy{}, false, fmt.Errorf("get user execution policy: %w", err)
	}
	p, err := userExecutionPolicyFromRow(row)
	if err != nil {
		return domain.UserExecutionPolicy{}, false, err
	}
	return p, true, nil
}

func marshalProfileIDs(ids []domain.ProviderProfileID) (string, error) {
	if ids == nil {
		ids = []domain.ProviderProfileID{}
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return "", fmt.Errorf("marshal provider profile priority: %w", err)
	}
	return string(b), nil
}

func unmarshalProfileIDs(raw string) ([]domain.ProviderProfileID, error) {
	if raw == "" {
		return []domain.ProviderProfileID{}, nil
	}
	var ids []domain.ProviderProfileID
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil, fmt.Errorf("unmarshal provider profile priority: %w", err)
	}
	return ids, nil
}

func userExecutionPolicyFromRow(row gen.UserExecutionPolicy) (domain.UserExecutionPolicy, error) {
	planner, err := unmarshalProfileIDs(row.PlannerPriority)
	if err != nil {
		return domain.UserExecutionPolicy{}, err
	}
	worker, err := unmarshalProfileIDs(row.WorkerPriority)
	if err != nil {
		return domain.UserExecutionPolicy{}, err
	}
	reviewer, err := unmarshalProfileIDs(row.ReviewerPriority)
	if err != nil {
		return domain.UserExecutionPolicy{}, err
	}
	resolver, err := unmarshalProfileIDs(row.DecisionResolverPriority)
	if err != nil {
		return domain.UserExecutionPolicy{}, err
	}
	return domain.UserExecutionPolicy{
		ID:                       domain.UserExecutionPolicyID(row.ID),
		UserID:                   domain.UserID(row.UserID),
		Version:                  row.Version,
		AutonomousMode:           row.AutonomousMode != 0,
		PlannerPriority:          planner,
		WorkerPriority:           worker,
		ReviewerPriority:         reviewer,
		DecisionResolverPriority: resolver,
		FallbackBehavior:         domain.FallbackBehavior(row.FallbackBehavior),
		ReviewIndependence:       domain.ReviewIndependence(row.ReviewIndependence),
		CreatedAt:                row.CreatedAt,
		UpdatedAt:                row.UpdatedAt,
	}, nil
}
