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

// CreateWorkflowRun inserts a workflow run and its initial steps in one
// transaction. Checkpoint 8A's fixed seeding policy always supplies the same
// six-step linear chain, but the store stays policy-agnostic: it persists
// whatever steps the caller built.
func (s *Store) CreateWorkflowRun(
	ctx context.Context,
	run domain.WorkflowRun,
	steps []domain.WorkflowStep,
) (domain.WorkflowRun, []domain.WorkflowStep, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	var (
		insertedRun   domain.WorkflowRun
		insertedSteps = make([]domain.WorkflowStep, 0, len(steps))
	)
	err := s.inTx(ctx, "create workflow run", func(q *gen.Queries) error {
		row, err := q.InsertWorkflowRun(ctx, gen.InsertWorkflowRunParams{
			ID:             run.ID,
			ProjectID:      domain.ProjectID(run.ProjectID),
			Objective:      run.Objective,
			State:          run.State,
			PolicyVersion:  run.PolicyVersion,
			PolicySnapshot: run.PolicySnapshot,
			CreatedAt:      run.CreatedAt,
			UpdatedAt:      run.UpdatedAt,
		})
		if err != nil {
			return fmt.Errorf("insert workflow run: %w", err)
		}
		insertedRun = workflowRunFromRow(row)
		for _, step := range steps {
			stepRow, err := q.InsertWorkflowStep(ctx, gen.InsertWorkflowStepParams{
				ID:               step.ID,
				WorkflowRunID:    step.WorkflowRunID,
				Kind:             step.Kind,
				Ordinal:          step.Ordinal,
				DependsOnStepID:  stringPtrToNullString(step.DependsOnStepID),
				State:            step.State,
				CreatedAt:        step.CreatedAt,
				UpdatedAt:        step.UpdatedAt,
			})
			if err != nil {
				return fmt.Errorf("insert workflow step %d: %w", step.Ordinal, err)
			}
			insertedSteps = append(insertedSteps, workflowStepFromRow(stepRow))
		}
		return nil
	})
	if err != nil {
		return domain.WorkflowRun{}, nil, err
	}
	return insertedRun, insertedSteps, nil
}

// GetWorkflowRun reads one workflow run by id.
func (s *Store) GetWorkflowRun(ctx context.Context, id string) (domain.WorkflowRun, bool, error) {
	row, err := s.qr.GetWorkflowRun(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WorkflowRun{}, false, nil
	}
	if err != nil {
		return domain.WorkflowRun{}, false, fmt.Errorf("get workflow run %s: %w", id, err)
	}
	return workflowRunFromRow(row), true, nil
}

// ListWorkflowRuns lists workflow runs, optionally filtered by project id.
func (s *Store) ListWorkflowRuns(ctx context.Context, projectID string) ([]domain.WorkflowRun, error) {
	if projectID != "" {
		rows, err := s.qr.ListWorkflowRunsByProject(ctx, domain.ProjectID(projectID))
		if err != nil {
			return nil, fmt.Errorf("list workflow runs for project %s: %w", projectID, err)
		}
		out := make([]domain.WorkflowRun, 0, len(rows))
		for _, row := range rows {
			out = append(out, workflowRunFromRow(row))
		}
		return out, nil
	}
	rows, err := s.qr.ListWorkflowRuns(ctx)
	if err != nil {
		return nil, fmt.Errorf("list workflow runs: %w", err)
	}
	out := make([]domain.WorkflowRun, 0, len(rows))
	for _, row := range rows {
		out = append(out, workflowRunFromRow(row))
	}
	return out, nil
}

// ListNonTerminalWorkflowRuns lists every workflow run not yet in a terminal
// state. Used by boot recovery.
func (s *Store) ListNonTerminalWorkflowRuns(ctx context.Context) ([]domain.WorkflowRun, error) {
	rows, err := s.qr.ListNonTerminalWorkflowRuns(ctx)
	if err != nil {
		return nil, fmt.Errorf("list non-terminal workflow runs: %w", err)
	}
	out := make([]domain.WorkflowRun, 0, len(rows))
	for _, row := range rows {
		out = append(out, workflowRunFromRow(row))
	}
	return out, nil
}

// UpdateWorkflowRunState compare-and-swaps a workflow run's state. A false
// result means the expected state no longer matched (already advanced by
// another caller, or already terminal).
func (s *Store) UpdateWorkflowRunState(
	ctx context.Context,
	id string,
	expected, next domain.WorkflowRunState,
	now time.Time,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var completedAt, cancelledAt sql.NullTime
	if next == domain.WorkflowRunCompleted {
		completedAt = sql.NullTime{Time: now, Valid: true}
	}
	if next == domain.WorkflowRunCancelled {
		cancelledAt = sql.NullTime{Time: now, Valid: true}
	}
	rows, err := s.qw.UpdateWorkflowRunState(ctx, gen.UpdateWorkflowRunStateParams{
		State:         next,
		UpdatedAt:     now,
		CompletedAt:   completedAt,
		CancelledAt:   cancelledAt,
		ID:            id,
		ExpectedState: expected,
	})
	if err != nil {
		return false, fmt.Errorf("update workflow run %s state: %w", id, err)
	}
	return rows > 0, nil
}

// GetWorkflowStep reads one workflow step by id.
func (s *Store) GetWorkflowStep(ctx context.Context, id string) (domain.WorkflowStep, bool, error) {
	row, err := s.qr.GetWorkflowStep(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WorkflowStep{}, false, nil
	}
	if err != nil {
		return domain.WorkflowStep{}, false, fmt.Errorf("get workflow step %s: %w", id, err)
	}
	return workflowStepFromRow(row), true, nil
}

// ListWorkflowSteps lists a run's steps in ordinal order.
func (s *Store) ListWorkflowSteps(ctx context.Context, runID string) ([]domain.WorkflowStep, error) {
	rows, err := s.qr.ListWorkflowStepsByRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list workflow steps for run %s: %w", runID, err)
	}
	out := make([]domain.WorkflowStep, 0, len(rows))
	for _, row := range rows {
		out = append(out, workflowStepFromRow(row))
	}
	return out, nil
}

// UpdateWorkflowStepState compare-and-swaps a workflow step's state.
func (s *Store) UpdateWorkflowStepState(
	ctx context.Context,
	id string,
	expected, next domain.WorkflowStepState,
	now time.Time,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var completedAt sql.NullTime
	if next.Terminal() {
		completedAt = sql.NullTime{Time: now, Valid: true}
	}
	rows, err := s.qw.UpdateWorkflowStepState(ctx, gen.UpdateWorkflowStepStateParams{
		State:         next,
		UpdatedAt:     now,
		CompletedAt:   completedAt,
		ID:            id,
		ExpectedState: expected,
	})
	if err != nil {
		return false, fmt.Errorf("update workflow step %s state: %w", id, err)
	}
	return rows > 0, nil
}

// CreateWorkflowAttempt inserts a new attempt for a step, assigning it the
// next sequential attempt_number for that step. writeMu serialises the
// read-then-insert against concurrent writers, mirroring the store's other
// next-num-then-insert methods (see store.go).
func (s *Store) CreateWorkflowAttempt(
	ctx context.Context,
	id, stepID, harness, model string,
	startedAt time.Time,
) (domain.WorkflowAttempt, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	maxAttempt, err := s.qw.GetMaxWorkflowAttemptNumber(ctx, stepID)
	if err != nil {
		return domain.WorkflowAttempt{}, fmt.Errorf("get max workflow attempt number for step %s: %w", stepID, err)
	}
	row, err := s.qw.InsertWorkflowAttempt(ctx, gen.InsertWorkflowAttemptParams{
		ID:            id,
		WorkflowStepID: stepID,
		AttemptNumber: maxAttempt + 1,
		Harness:       harness,
		Model:         model,
		StartedAt:     startedAt,
	})
	if err != nil {
		return domain.WorkflowAttempt{}, fmt.Errorf("insert workflow attempt for step %s: %w", stepID, err)
	}
	return workflowAttemptFromRow(row), nil
}

// ListWorkflowAttempts lists a step's attempts in attempt-number order.
func (s *Store) ListWorkflowAttempts(ctx context.Context, stepID string) ([]domain.WorkflowAttempt, error) {
	rows, err := s.qr.ListWorkflowAttemptsByStep(ctx, stepID)
	if err != nil {
		return nil, fmt.Errorf("list workflow attempts for step %s: %w", stepID, err)
	}
	out := make([]domain.WorkflowAttempt, 0, len(rows))
	for _, row := range rows {
		out = append(out, workflowAttemptFromRow(row))
	}
	return out, nil
}

// GetLatestWorkflowAttempt returns a step's most recent attempt, ok=false if none.
func (s *Store) GetLatestWorkflowAttempt(ctx context.Context, stepID string) (domain.WorkflowAttempt, bool, error) {
	row, err := s.qr.GetLatestWorkflowAttemptByStep(ctx, stepID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WorkflowAttempt{}, false, nil
	}
	if err != nil {
		return domain.WorkflowAttempt{}, false, fmt.Errorf("get latest workflow attempt for step %s: %w", stepID, err)
	}
	return workflowAttemptFromRow(row), true, nil
}

// CreateWorkflowCheckpoint inserts an append-only checkpoint row. There is no
// update path: advancing means inserting a new row.
func (s *Store) CreateWorkflowCheckpoint(ctx context.Context, cp domain.WorkflowCheckpoint) (domain.WorkflowCheckpoint, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	row, err := s.qw.InsertWorkflowCheckpoint(ctx, gen.InsertWorkflowCheckpointParams{
		ID:             cp.ID,
		WorkflowRunID:  cp.WorkflowRunID,
		WorkflowStepID: stringPtrToNullString(cp.WorkflowStepID),
		AttemptID:      stringPtrToNullString(cp.AttemptID),
		ProjectID:      domain.ProjectID(cp.ProjectID),
		SessionID:      stringPtrToNullString(cp.SessionID),
		Branch:         cp.Branch,
		WorktreePath:   cp.WorktreePath,
		BaseSha:        cp.BaseSHA,
		HeadSha:        cp.HeadSHA,
		ReviewRunID:    stringPtrToNullString(cp.ReviewRunID),
		ReviewVerdict:  cp.ReviewVerdict,
		RetryState:     cp.RetryState,
		NextAction:     cp.NextAction,
		DurablePhase:   cp.DurablePhase,
		PayloadVersion: cp.PayloadVersion,
		CreatedAt:      cp.CreatedAt,
	})
	if err != nil {
		return domain.WorkflowCheckpoint{}, fmt.Errorf("insert workflow checkpoint for run %s: %w", cp.WorkflowRunID, err)
	}
	return workflowCheckpointFromRow(row), nil
}

// ListWorkflowCheckpoints lists a run's checkpoints oldest first.
func (s *Store) ListWorkflowCheckpoints(ctx context.Context, runID string) ([]domain.WorkflowCheckpoint, error) {
	rows, err := s.qr.ListWorkflowCheckpointsByRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list workflow checkpoints for run %s: %w", runID, err)
	}
	out := make([]domain.WorkflowCheckpoint, 0, len(rows))
	for _, row := range rows {
		out = append(out, workflowCheckpointFromRow(row))
	}
	return out, nil
}

// GetLatestWorkflowCheckpointByStep returns a step's most recent checkpoint, ok=false if none.
func (s *Store) GetLatestWorkflowCheckpointByStep(ctx context.Context, stepID string) (domain.WorkflowCheckpoint, bool, error) {
	row, err := s.qr.GetLatestWorkflowCheckpointByStep(ctx, sql.NullString{String: stepID, Valid: stepID != ""})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WorkflowCheckpoint{}, false, nil
	}
	if err != nil {
		return domain.WorkflowCheckpoint{}, false, fmt.Errorf("get latest workflow checkpoint for step %s: %w", stepID, err)
	}
	return workflowCheckpointFromRow(row), true, nil
}

// EnqueueWorkflowOutboxEntry idempotently inserts an outbox entry keyed by
// idempotency_key. created=false returns the existing row for a repeated key
// instead of leaking a SQLite constraint as an internal error, mirroring
// CreateSessionInterfaceTransition.
func (s *Store) EnqueueWorkflowOutboxEntry(ctx context.Context, entry domain.WorkflowOutboxEntry) (domain.WorkflowOutboxEntry, bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.InsertWorkflowOutboxEntry(ctx, gen.InsertWorkflowOutboxEntryParams{
		ID:              entry.ID,
		WorkflowRunID:   entry.WorkflowRunID,
		WorkflowStepID:  stringPtrToNullString(entry.WorkflowStepID),
		IdempotencyKey:  entry.IdempotencyKey,
		CommandType:     entry.CommandType,
		Payload:         entry.Payload,
		CreatedAt:       entry.CreatedAt,
	})
	if err != nil {
		return domain.WorkflowOutboxEntry{}, false, fmt.Errorf("insert workflow outbox entry: %w", err)
	}
	existing, lookupErr := s.qw.GetWorkflowOutboxByIdempotencyKey(ctx, entry.IdempotencyKey)
	if lookupErr != nil {
		return domain.WorkflowOutboxEntry{}, false, fmt.Errorf("read workflow outbox entry %s: %w", entry.IdempotencyKey, lookupErr)
	}
	return workflowOutboxFromRow(existing), rows > 0, nil
}

// ListWorkflowOutboxByRun lists a run's outbox entries oldest first.
func (s *Store) ListWorkflowOutboxByRun(ctx context.Context, runID string) ([]domain.WorkflowOutboxEntry, error) {
	rows, err := s.qr.ListWorkflowOutboxByRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list workflow outbox for run %s: %w", runID, err)
	}
	out := make([]domain.WorkflowOutboxEntry, 0, len(rows))
	for _, row := range rows {
		out = append(out, workflowOutboxFromRow(row))
	}
	return out, nil
}

func workflowRunFromRow(r gen.WorkflowRun) domain.WorkflowRun {
	return domain.WorkflowRun{
		ID:             r.ID,
		ProjectID:      string(r.ProjectID),
		Objective:      r.Objective,
		State:          r.State,
		PolicyVersion:  r.PolicyVersion,
		PolicySnapshot: r.PolicySnapshot,
		CreatedAt:      r.CreatedAt,
		UpdatedAt:      r.UpdatedAt,
		CompletedAt:    nullTimeToTimePtr(r.CompletedAt),
		CancelledAt:    nullTimeToTimePtr(r.CancelledAt),
	}
}

func workflowStepFromRow(r gen.WorkflowStep) domain.WorkflowStep {
	return domain.WorkflowStep{
		ID:                       r.ID,
		WorkflowRunID:            r.WorkflowRunID,
		Kind:                     r.Kind,
		Ordinal:                  r.Ordinal,
		DependsOnStepID:          nullStringToPtr(r.DependsOnStepID),
		State:                    r.State,
		AssignedHarness:          r.AssignedHarness,
		SessionID:                nullStringToPtr(r.SessionID),
		ReviewRunID:              nullStringToPtr(r.ReviewRunID),
		ExpectedArtifactsVersion: r.ExpectedArtifactsVersion,
		CreatedAt:                r.CreatedAt,
		UpdatedAt:                r.UpdatedAt,
		CompletedAt:              nullTimeToTimePtr(r.CompletedAt),
	}
}

func workflowAttemptFromRow(r gen.WorkflowAttempt) domain.WorkflowAttempt {
	var outcome domain.WorkflowAttemptOutcome
	if r.Outcome != nil {
		outcome = *r.Outcome
	}
	var errClass domain.WorkflowErrorClass
	if r.ErrorClass != nil {
		errClass = *r.ErrorClass
	}
	return domain.WorkflowAttempt{
		ID:             r.ID,
		WorkflowStepID: r.WorkflowStepID,
		AttemptNumber:  r.AttemptNumber,
		Harness:        r.Harness,
		Model:          r.Model,
		StartedAt:      r.StartedAt,
		FinishedAt:     nullTimeToTimePtr(r.FinishedAt),
		Outcome:        outcome,
		ErrorClass:     errClass,
		RetryAfter:     nullTimeToTimePtr(r.RetryAfter),
	}
}

func workflowCheckpointFromRow(r gen.WorkflowCheckpoint) domain.WorkflowCheckpoint {
	return domain.WorkflowCheckpoint{
		ID:             r.ID,
		WorkflowRunID:  r.WorkflowRunID,
		WorkflowStepID: nullStringToPtr(r.WorkflowStepID),
		AttemptID:      nullStringToPtr(r.AttemptID),
		ProjectID:      string(r.ProjectID),
		SessionID:      nullStringToPtr(r.SessionID),
		Branch:         r.Branch,
		WorktreePath:   r.WorktreePath,
		BaseSHA:        r.BaseSha,
		HeadSHA:        r.HeadSha,
		ReviewRunID:    nullStringToPtr(r.ReviewRunID),
		ReviewVerdict:  r.ReviewVerdict,
		RetryState:     r.RetryState,
		NextAction:     r.NextAction,
		DurablePhase:   r.DurablePhase,
		PayloadVersion: r.PayloadVersion,
		CreatedAt:      r.CreatedAt,
	}
}

func workflowOutboxFromRow(r gen.WorkflowOutbox) domain.WorkflowOutboxEntry {
	return domain.WorkflowOutboxEntry{
		ID:             r.ID,
		WorkflowRunID:  r.WorkflowRunID,
		WorkflowStepID: nullStringToPtr(r.WorkflowStepID),
		IdempotencyKey: r.IdempotencyKey,
		CommandType:    r.CommandType,
		Payload:        r.Payload,
		Status:         r.Status,
		CreatedAt:      r.CreatedAt,
		DispatchedAt:   nullTimeToTimePtr(r.DispatchedAt),
		AcknowledgedAt: nullTimeToTimePtr(r.AcknowledgedAt),
		FailedAt:       nullTimeToTimePtr(r.FailedAt),
		ErrorClass:     r.ErrorClass,
	}
}

func stringPtrToNullString(s *string) sql.NullString {
	if s == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *s, Valid: true}
}

func nullStringToPtr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	v := s.String
	return &v
}
