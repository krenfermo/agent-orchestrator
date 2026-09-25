package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// agent_exploration_store.go -- Frente 3 / 3C persistence of agent tool
// observations (migration 0175). Metadata only; see the migration.

// applyAgentToolFacts writes one chunk's observations and completes the
// results it carries. Called inside ApplyUsageChunk's transaction. The
// observations are written first so a result in the same chunk as its call
// finds the row it completes.
func applyAgentToolFacts(
	ctx context.Context,
	q *gen.Queries,
	bindingID, sourceID int64,
	facts domain.AgentToolFacts,
	recordedAt time.Time,
) error {
	for _, obs := range facts.Observations {
		var path sql.NullString
		if obs.PathScope == domain.ToolPathProject {
			path = sql.NullString{String: obs.Path, Valid: true}
		}
		if err := q.InsertAgentToolObservation(ctx, gen.InsertAgentToolObservationParams{
			BindingID:      bindingID,
			UsageSourceID:  sql.NullInt64{Int64: sourceID, Valid: sourceID > 0},
			ObservationKey: obs.Key,
			EventKey:       obs.EventKey,
			Ordinal:        obs.Ordinal,
			ObservedAt:     ptrTimeToNullTime(obs.ObservedAt),
			Origin:         string(obs.Origin),
			Op:             string(obs.Op),
			ToolName:       obs.ToolName,
			PathScope:      string(obs.PathScope),
			Path:           path,
			ResultBytes:    ptrInt64ToNull(obs.ResultBytes),
			RecordedAt:     recordedAt,
		}); err != nil {
			return fmt.Errorf("insert tool observation: %w", err)
		}
	}
	for _, res := range facts.Results {
		if res.Key == "" || res.ResultBytes < 0 {
			continue
		}
		isError := int64(0)
		if res.IsError {
			isError = 1
		}
		if _, err := q.CompleteAgentToolObservation(ctx, gen.CompleteAgentToolObservationParams{
			ResultBytes:    sql.NullInt64{Int64: res.ResultBytes, Valid: true},
			ResultItems:    ptrInt64ToNull(res.ResultItems),
			ResultError:    sql.NullInt64{Int64: isError, Valid: true},
			BindingID:      bindingID,
			ObservationKey: res.Key,
		}); err != nil {
			return fmt.Errorf("complete tool observation: %w", err)
		}
	}
	return nil
}

// RunToolObservation is one stored observation, resolved to the role window
// of the run it was read for.
type RunToolObservation struct {
	Role             domain.WorkflowRole
	Cycle            int64
	ProjectID        string
	Subject          domain.UsageSubject
	Harness          string
	Key              string
	EventKey         string
	Ordinal          int64
	ObservedAt       *time.Time
	Origin           domain.ToolObservationOrigin
	Op               domain.ToolOp
	ToolName         string
	PathScope        domain.ToolPathScope
	Path             string
	ResultBytes      *int64
	ResultItems      *int64
	ResultError      *bool
	AttributionBasis domain.AttributionBasis
}

// ListRunToolObservations returns every tool observation of one run's
// subjects, in transcript order per subject.
func (s *Store) ListRunToolObservations(ctx context.Context, runID string) ([]RunToolObservation, error) {
	rows, err := s.qr.ListRunToolObservations(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list tool observations for run %s: %w", runID, err)
	}
	out := make([]RunToolObservation, 0, len(rows))
	for _, r := range rows {
		obs := RunToolObservation{
			Role:             domain.WorkflowRole(r.Role),
			Cycle:            r.Cycle,
			ProjectID:        r.ProjectID,
			Subject:          domain.UsageSubject{Kind: domain.UsageSubjectKind(r.SubjectKind), ID: r.SubjectID},
			Harness:          r.Harness,
			Key:              r.ObservationKey,
			EventKey:         r.EventKey,
			Ordinal:          r.Ordinal,
			ObservedAt:       nullTimePtr(r.ObservedAt),
			Origin:           domain.ToolObservationOrigin(r.Origin),
			Op:               domain.ToolOp(r.Op),
			ToolName:         r.ToolName,
			PathScope:        domain.ToolPathScope(r.PathScope),
			ResultBytes:      nullInt64Ptr(r.ResultBytes),
			ResultItems:      nullInt64Ptr(r.ResultItems),
			AttributionBasis: domain.AttributionBasis(r.AttributionBasis),
		}
		// A value outside the closed vocabularies can only come from a row a
		// future binary wrote; it is narrowed rather than passed through, so a
		// read model can never count a class it does not know.
		if !obs.Origin.Valid() {
			obs.Origin = domain.OriginUnknown
		}
		if !obs.Op.Valid() {
			obs.Op = domain.ToolOpOther
		}
		if !obs.PathScope.Valid() {
			obs.PathScope = domain.ToolPathUnresolved
		}
		if obs.PathScope == domain.ToolPathProject && r.Path.Valid {
			obs.Path = r.Path.String
		}
		if r.ResultError.Valid {
			isError := r.ResultError.Int64 != 0
			obs.ResultError = &isError
		}
		out = append(out, obs)
	}
	return out, nil
}

// RunExplorationCall is one provider call of a run's subjects.
type RunExplorationCall struct {
	Role       domain.WorkflowRole
	Cycle      int64
	ProjectID  string
	Subject    domain.UsageSubject
	Harness    string
	ModelID    string
	Tokens     domain.UsageTokenTotals
	TurnClass  domain.TurnClass
	ObservedAt *time.Time
}

// ListRunExplorationCalls returns every provider call of one run's subjects,
// including calls that carry no observed time.
func (s *Store) ListRunExplorationCalls(ctx context.Context, runID string) ([]RunExplorationCall, error) {
	rows, err := s.qr.ListRunExplorationCalls(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list exploration calls for run %s: %w", runID, err)
	}
	out := make([]RunExplorationCall, 0, len(rows))
	for _, r := range rows {
		out = append(out, RunExplorationCall{
			Role:      domain.WorkflowRole(r.Role),
			Cycle:     r.Cycle,
			ProjectID: r.ProjectID,
			Subject:   domain.UsageSubject{Kind: domain.UsageSubjectKind(r.SubjectKind), ID: r.SubjectID},
			Harness:   r.Harness,
			ModelID:   r.ModelID,
			Tokens: domain.UsageTokenTotals{
				InputTokens:         r.InputTokens,
				UncachedInputTokens: r.UncachedInputTokens,
				CacheReadTokens:     r.CacheReadTokens,
				CacheWriteTokens:    r.CacheWriteTokens,
				OutputTokens:        r.OutputTokens,
				EventCount:          1,
			},
			TurnClass:  turnClassOrUnclassified(r.TurnClass),
			ObservedAt: nullTimePtr(r.ObservedAt),
		})
	}
	return out, nil
}

func nullInt64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	out := v.Int64
	return &out
}
