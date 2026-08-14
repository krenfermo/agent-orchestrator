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
			ID:               run.ID,
			ProjectID:        domain.ProjectID(run.ProjectID),
			Objective:        run.Objective,
			State:            run.State,
			PolicyVersion:    run.PolicyVersion,
			PolicySnapshot:   run.PolicySnapshot,
			CreatedAt:        run.CreatedAt,
			UpdatedAt:        run.UpdatedAt,
			ParentWorkflowID: stringPtrToNullString(run.ParentWorkflowID),
			PlannedTaskID:    stringPtrToNullString(run.PlannedTaskID),
		})
		if err != nil {
			return fmt.Errorf("insert workflow run: %w", err)
		}
		insertedRun = workflowRunFromRow(row)
		for _, step := range steps {
			artifactJSON := step.ArtifactJSON
			if artifactJSON == "" {
				artifactJSON = "{}"
			}
			stepRow, err := q.InsertWorkflowStep(ctx, gen.InsertWorkflowStepParams{
				ID:              step.ID,
				WorkflowRunID:   step.WorkflowRunID,
				Kind:            step.Kind,
				Ordinal:         step.Ordinal,
				DependsOnStepID: stringPtrToNullString(step.DependsOnStepID),
				State:           step.State,
				CreatedAt:       step.CreatedAt,
				UpdatedAt:       step.UpdatedAt,
				ArtifactJson:    artifactJSON,
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

// UpdateWorkflowStepArtifact persists a step's deterministic artifact JSON
// (e.g. the plan step's PlanArtifact). Not a state transition.
func (s *Store) UpdateWorkflowStepArtifact(ctx context.Context, stepID, artifactJSON string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.UpdateWorkflowStepArtifact(ctx, gen.UpdateWorkflowStepArtifactParams{
		ArtifactJson: artifactJSON,
		UpdatedAt:    now,
		ID:           stepID,
	})
	if err != nil {
		return false, fmt.Errorf("update workflow step %s artifact: %w", stepID, err)
	}
	return rows > 0, nil
}

// UpdateWorkflowStepSession backfills a step's session_id once a spawn is
// durably known to have happened. Guarded on session_id currently NULL: a
// second caller can never clobber an already-associated session, making this
// safe to call redundantly from recovery/observation.
func (s *Store) UpdateWorkflowStepSession(ctx context.Context, stepID, sessionID string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.UpdateWorkflowStepSession(ctx, gen.UpdateWorkflowStepSessionParams{
		SessionID: sql.NullString{String: sessionID, Valid: sessionID != ""},
		UpdatedAt: now,
		ID:        stepID,
	})
	if err != nil {
		return false, fmt.Errorf("update workflow step %s session: %w", stepID, err)
	}
	return rows > 0, nil
}

// SetWorkflowStepReviewRun unconditionally sets the review step's
// review_run_id to the current/most-recent review_run for that step
// (Checkpoint 8D: this column is no longer write-once — it cycles across
// review->fix->re-review iterations). The primary anti-duplication guard
// against creating two review_runs for the same cycle is the outbox
// idempotency key (cycle-specific), not a CAS on this column.
func (s *Store) SetWorkflowStepReviewRun(ctx context.Context, stepID, reviewRunID string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.SetWorkflowStepReviewRun(ctx, gen.SetWorkflowStepReviewRunParams{
		ReviewRunID: sql.NullString{String: reviewRunID, Valid: reviewRunID != ""},
		UpdatedAt:   now,
		ID:          stepID,
	})
	if err != nil {
		return false, fmt.Errorf("set workflow step %s review run: %w", stepID, err)
	}
	return rows > 0, nil
}

// UpdateWorkflowOutboxStatus compare-and-swaps an outbox entry's dispatch
// status. Only the timestamp column matching next is set; the others are
// left NULL (a status can only move forward, never back, so a NULL retains
// correctness for columns not yet reached).
func (s *Store) UpdateWorkflowOutboxStatus(
	ctx context.Context,
	id string,
	expected, next domain.WorkflowOutboxStatus,
	now time.Time,
	errorClass string,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var dispatchedAt, acknowledgedAt, failedAt sql.NullTime
	switch next {
	case domain.WorkflowOutboxDispatched:
		dispatchedAt = sql.NullTime{Time: now, Valid: true}
	case domain.WorkflowOutboxAcknowledged:
		acknowledgedAt = sql.NullTime{Time: now, Valid: true}
	case domain.WorkflowOutboxFailed:
		failedAt = sql.NullTime{Time: now, Valid: true}
	}
	rows, err := s.qw.UpdateWorkflowOutboxStatus(ctx, gen.UpdateWorkflowOutboxStatusParams{
		Status:         next,
		DispatchedAt:   dispatchedAt,
		AcknowledgedAt: acknowledgedAt,
		FailedAt:       failedAt,
		ErrorClass:     errorClass,
		ID:             id,
		ExpectedStatus: expected,
	})
	if err != nil {
		return false, fmt.Errorf("update workflow outbox %s status: %w", id, err)
	}
	return rows > 0, nil
}

// UpdateWorkflowAttemptOutcome updates an existing attempt row's terminal
// facts (finished_at/outcome/error_class). Used both to conclude a dispatch
// attempt and to later refine an already-recorded attempt's error_class
// (e.g. to ambiguous_worker_state) without creating a second attempt row.
func (s *Store) UpdateWorkflowAttemptOutcome(
	ctx context.Context,
	attemptID string,
	finishedAt time.Time,
	outcome domain.WorkflowAttemptOutcome,
	errorClass domain.WorkflowErrorClass,
) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var finishedAtNull sql.NullTime
	if !finishedAt.IsZero() {
		finishedAtNull = sql.NullTime{Time: finishedAt, Valid: true}
	}
	var outcomePtr *domain.WorkflowAttemptOutcome
	if outcome != "" {
		outcomePtr = &outcome
	}
	var errorClassPtr *domain.WorkflowErrorClass
	if errorClass != "" {
		errorClassPtr = &errorClass
	}
	if _, err := s.qw.UpdateWorkflowAttemptOutcome(ctx, gen.UpdateWorkflowAttemptOutcomeParams{
		FinishedAt: finishedAtNull,
		Outcome:    outcomePtr,
		ErrorClass: errorClassPtr,
		ID:         attemptID,
	}); err != nil {
		return fmt.Errorf("update workflow attempt %s outcome: %w", attemptID, err)
	}
	return nil
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
		ID:             id,
		WorkflowStepID: stepID,
		AttemptNumber:  maxAttempt + 1,
		Harness:        harness,
		Model:          model,
		StartedAt:      startedAt,
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
		ID:                cp.ID,
		WorkflowRunID:     cp.WorkflowRunID,
		WorkflowStepID:    stringPtrToNullString(cp.WorkflowStepID),
		AttemptID:         stringPtrToNullString(cp.AttemptID),
		ProjectID:         domain.ProjectID(cp.ProjectID),
		SessionID:         stringPtrToNullString(cp.SessionID),
		Branch:            cp.Branch,
		WorktreePath:      cp.WorktreePath,
		BaseSha:           cp.BaseSHA,
		HeadSha:           cp.HeadSHA,
		ReviewRunID:       stringPtrToNullString(cp.ReviewRunID),
		ReviewVerdict:     cp.ReviewVerdict,
		RetryState:        cp.RetryState,
		NextAction:        cp.NextAction,
		DurablePhase:      cp.DurablePhase,
		PayloadVersion:    cp.PayloadVersion,
		CreatedAt:         cp.CreatedAt,
		FingerprintBefore: cp.FingerprintBefore,
		FingerprintAfter:  cp.FingerprintAfter,
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
		ID:             entry.ID,
		WorkflowRunID:  entry.WorkflowRunID,
		WorkflowStepID: stringPtrToNullString(entry.WorkflowStepID),
		IdempotencyKey: entry.IdempotencyKey,
		CommandType:    entry.CommandType,
		Payload:        entry.Payload,
		CreatedAt:      entry.CreatedAt,
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
		ID:               r.ID,
		ProjectID:        string(r.ProjectID),
		Objective:        r.Objective,
		State:            r.State,
		PolicyVersion:    r.PolicyVersion,
		PolicySnapshot:   r.PolicySnapshot,
		CreatedAt:        r.CreatedAt,
		UpdatedAt:        r.UpdatedAt,
		CompletedAt:      nullTimeToTimePtr(r.CompletedAt),
		CancelledAt:      nullTimeToTimePtr(r.CancelledAt),
		ParentWorkflowID: nullStringToPtr(r.ParentWorkflowID),
		PlannedTaskID:    nullStringToPtr(r.PlannedTaskID),
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
		ArtifactJSON:             r.ArtifactJson,
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
		ID:                r.ID,
		WorkflowRunID:     r.WorkflowRunID,
		WorkflowStepID:    nullStringToPtr(r.WorkflowStepID),
		AttemptID:         nullStringToPtr(r.AttemptID),
		ProjectID:         string(r.ProjectID),
		SessionID:         nullStringToPtr(r.SessionID),
		Branch:            r.Branch,
		WorktreePath:      r.WorktreePath,
		BaseSHA:           r.BaseSha,
		HeadSHA:           r.HeadSha,
		ReviewRunID:       nullStringToPtr(r.ReviewRunID),
		ReviewVerdict:     r.ReviewVerdict,
		RetryState:        r.RetryState,
		NextAction:        r.NextAction,
		DurablePhase:      r.DurablePhase,
		PayloadVersion:    r.PayloadVersion,
		FingerprintBefore: r.FingerprintBefore,
		FingerprintAfter:  r.FingerprintAfter,
		CreatedAt:         r.CreatedAt,
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
