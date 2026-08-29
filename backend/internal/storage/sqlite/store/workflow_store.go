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

// ListChildWorkflowRuns lists every child run of a master run, in creation
// order. Cancellation cascades over this: the parent link is the durable
// "belongs to this master" fact, present even when the master's task row was
// never stamped with its execution run id.
func (s *Store) ListChildWorkflowRuns(ctx context.Context, parentRunID string) ([]domain.WorkflowRun, error) {
	rows, err := s.qr.ListChildWorkflowRuns(ctx, sql.NullString{String: parentRunID, Valid: parentRunID != ""})
	if err != nil {
		return nil, fmt.Errorf("list child workflow runs of %s: %w", parentRunID, err)
	}
	out := make([]domain.WorkflowRun, 0, len(rows))
	for _, row := range rows {
		out = append(out, workflowRunFromRow(row))
	}
	return out, nil
}

// ListArchivedWorkflowRuns lists a project's archived top-level runs, newest
// archive first. This is the read behind "Mostrar archivados": archiving hides
// a run from the active Board, it never removes it.
func (s *Store) ListArchivedWorkflowRuns(ctx context.Context, projectID string, limit int) ([]domain.WorkflowRun, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.qr.ListArchivedWorkflowRunsByProject(ctx, gen.ListArchivedWorkflowRunsByProjectParams{
		ProjectID: domain.ProjectID(projectID),
		Limit:     int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("list archived workflow runs for project %s: %w", projectID, err)
	}
	out := make([]domain.WorkflowRun, 0, len(rows))
	for _, row := range rows {
		out = append(out, workflowRunFromRow(row))
	}
	return out, nil
}

// ArchiveWorkflowRun stamps a run's archive marker. Returns false when the run
// was already archived (the original timestamp is kept, so a retried
// cancel-and-archive is a genuine no-op) or is not terminal (a still-live
// workflow can never be hidden from the Board). Never deletes anything.
func (s *Store) ArchiveWorkflowRun(ctx context.Context, id string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.ArchiveWorkflowRun(ctx, gen.ArchiveWorkflowRunParams{
		ArchivedAt: sql.NullTime{Time: now, Valid: true},
		UpdatedAt:  now,
		ID:         id,
	})
	if err != nil {
		return false, fmt.Errorf("archive workflow run %s: %w", id, err)
	}
	return rows > 0, nil
}

// UpdateWorkflowRunState compare-and-swaps a workflow run's state. A false
// result means the expected state no longer matched (already advanced by
// another caller, or already terminal).
// UpdateWorkflowRunPolicySnapshot overwrites a run's policy_snapshot
// (Checkpoint 8P-C: embedding the owner's execution policy right after
// creation). Callers must only ever call this once, immediately after
// CreateWorkflowRun -- routing relies on the snapshot never changing again
// for the lifetime of the run.
func (s *Store) UpdateWorkflowRunPolicySnapshot(ctx context.Context, id, policySnapshot string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.UpdateWorkflowRunPolicySnapshot(ctx, gen.UpdateWorkflowRunPolicySnapshotParams{
		PolicySnapshot: policySnapshot,
		UpdatedAt:      now,
		ID:             id,
	})
	if err != nil {
		return false, fmt.Errorf("update workflow run %s policy snapshot: %w", id, err)
	}
	return rows > 0, nil
}

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

// ReopenFailedWorkflowStep moves a terminally failed step back to `ready` and
// clears its completed_at, so a new attempt can run against the same target.
//
// It reuses the same compare-and-swap UPDATE as UpdateWorkflowStepState with the
// expected state pinned to `failed`, which is the whole safety property: this
// method cannot be pointed at a step in any other state, and a second caller
// matches no row and gets false. See workflow.Store's doc comment for why the
// reopen is a method of its own rather than a widening of the state machine.
func (s *Store) ReopenFailedWorkflowStep(ctx context.Context, stepID string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.UpdateWorkflowStepState(ctx, gen.UpdateWorkflowStepStateParams{
		State:         domain.WorkflowStepReady,
		UpdatedAt:     now,
		CompletedAt:   sql.NullTime{},
		ID:            stepID,
		ExpectedState: domain.WorkflowStepFailed,
	})
	if err != nil {
		return false, fmt.Errorf("reopen failed workflow step %s: %w", stepID, err)
	}
	return rows > 0, nil
}

// ReopenCompletedWorkflowStep moves a COMPLETED step back to `waiting` and
// clears its completed_at, so the step's own dispatch path can start one more
// cycle for it.
//
// Same shape and same safety property as ReopenFailedWorkflowStep: the
// compare-and-swap's expected state is hard-coded (here to `completed`), so the
// method cannot be pointed at a step in any other state and a second caller
// matches no row and gets false. `waiting` rather than `ready` because that is
// the resting state every review-cycle dispatch already starts from. See
// workflow.Store's doc comment for its single caller and why it is a method of
// its own rather than a widening of the state machine.
func (s *Store) ReopenCompletedWorkflowStep(ctx context.Context, stepID string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.UpdateWorkflowStepState(ctx, gen.UpdateWorkflowStepStateParams{
		State:         domain.WorkflowStepWaiting,
		UpdatedAt:     now,
		CompletedAt:   sql.NullTime{},
		ID:            stepID,
		ExpectedState: domain.WorkflowStepCompleted,
	})
	if err != nil {
		return false, fmt.Errorf("reopen completed workflow step %s: %w", stepID, err)
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

// ClaimWorkflowOutboxDispatch takes an entry pending -> dispatched and stamps
// the claim token that identifies the dispatch now owning it.
//
// The token and the status change are one statement, so a claimed row always
// says who claimed it; every ownership-dependent transition off `dispatched`
// then names the token back.
func (s *Store) ClaimWorkflowOutboxDispatch(
	ctx context.Context,
	id string,
	now time.Time,
	dispatchGeneration string,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.ClaimWorkflowOutboxDispatch(ctx, gen.ClaimWorkflowOutboxDispatchParams{
		DispatchedAt:       sql.NullTime{Time: now, Valid: true},
		DispatchGeneration: dispatchGeneration,
		ID:                 id,
	})
	if err != nil {
		return false, fmt.Errorf("claim workflow outbox %s dispatch: %w", id, err)
	}
	return rows > 0, nil
}

// AcknowledgeWorkflowOutboxDispatch completes a dispatch — dispatched ->
// acknowledged — for the EXACT dispatch that holds the claim.
//
// It is the last of the four ownership-dependent transitions off `dispatched`
// (the other three being fail, release and reopen) and it exists for the same
// reason they do: `id + status = dispatched` is satisfied by ANY dispatch of
// this row, so a pass that paused after launching, lost its claim to a
// reconciler's release, and woke up to find the row reclaimed by a second
// dispatch would otherwise acknowledge somebody else's live launch as its own —
// and the step would go RUNNING over a confirmation that belongs to a different
// worker.
//
// The claim token is cleared by the transition: an acknowledged row is no
// longer claimable, and the token described the claim, not the row.
//
// A caller holding an EMPTY generation matches only a row whose generation is
// also empty. That is exactly right for the rows on disk from before the worker
// path claimed with a token: they are unclaimed, so an unclaimed acknowledge is
// the only one that may complete them, and a token-holding pass cannot. It is
// also what lets the ADOPTION path complete a `failed` row: a failed entry
// carries no claim (FailWorkflowOutboxWithGeneration clears it), so the status
// compare-and-swap is the whole arbiter there and the token fence is vacuous by
// construction rather than by exception.
//
// expected is the status the caller holds, not a constant: a launch confirms
// from `dispatched`, while resumeWorkerLaunchAfterFailure adopts an existing
// session from `failed`. Both are real transitions to `acknowledged` and both
// must stay compare-and-swapped against the state their caller actually read.
//
// Hand-written for the same reason ClaimWorkflowAttemptOutcome is: sqlc is not
// part of the build here, and the statement stays readable next to the
// generated ones.
func (s *Store) AcknowledgeWorkflowOutboxDispatch(
	ctx context.Context,
	id string,
	expected domain.WorkflowOutboxStatus,
	now time.Time,
	dispatchGeneration string,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.writeDB.ExecContext(ctx,
		`UPDATE workflow_outbox
		    SET status = 'acknowledged',
		        acknowledged_at = ?,
		        error_class = '',
		        failure_generation = '',
		        dispatch_generation = ''
		  WHERE id = ?
		    AND status = ?
		    AND dispatch_generation = ?`,
		sql.NullTime{Time: now, Valid: true}, id, expected, dispatchGeneration)
	if err != nil {
		return false, fmt.Errorf("acknowledge workflow outbox %s dispatch: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("acknowledge workflow outbox %s dispatch: %w", id, err)
	}
	return rows == 1, nil
}

// FailWorkflowOutboxWithGeneration moves an entry to `failed`, stamps the
// identity of the failure that did it, and PROVES the caller still owns the
// dispatch — all in the SAME statement.
//
// One statement is the requirement, not a convenience. The stamp is what a later
// human resume compare-and-swaps against, so a row that reached `failed` without
// it would be a failure nobody could prove the generation of. And the ownership
// half is why `id + status = dispatched` is not enough: a dispatch that paused
// after recording its launch error can wake to find the row dispatched AGAIN, to
// somebody else, and would otherwise fail that live generation and stamp its own
// failure onto it.
func (s *Store) FailWorkflowOutboxWithGeneration(
	ctx context.Context,
	id string,
	expected domain.WorkflowOutboxStatus,
	now time.Time,
	errorClass, generation, dispatchGeneration string,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.FailWorkflowOutboxWithGeneration(ctx, gen.FailWorkflowOutboxWithGenerationParams{
		FailedAt:           sql.NullTime{Time: now, Valid: true},
		ErrorClass:         errorClass,
		FailureGeneration:  generation,
		ID:                 id,
		ExpectedStatus:     expected,
		DispatchGeneration: dispatchGeneration,
	})
	if err != nil {
		return false, fmt.Errorf("fail workflow outbox %s with generation: %w", id, err)
	}
	return rows > 0, nil
}

// ReleaseDispatchedWorkflowOutboxGeneration gives one dispatch's claim back:
// dispatched -> pending, for the exact token that holds it. A stale release
// changes nothing rather than handing a live dispatch's claim to somebody else.
func (s *Store) ReleaseDispatchedWorkflowOutboxGeneration(
	ctx context.Context,
	id string,
	errorClass, dispatchGeneration string,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.ReleaseDispatchedWorkflowOutboxGeneration(ctx, gen.ReleaseDispatchedWorkflowOutboxGenerationParams{
		ErrorClass:         errorClass,
		ID:                 id,
		DispatchGeneration: dispatchGeneration,
	})
	if err != nil {
		return false, fmt.Errorf("release workflow outbox %s dispatch: %w", id, err)
	}
	return rows > 0, nil
}

// ReopenFailedWorkflowOutboxGeneration moves ONE named failed generation back to
// `pending`.
//
// The generation is in the UPDATE's own WHERE clause, so "is this still the
// failure the person acted on?" is answered by the same statement that changes
// the state. Answering it with a read first and swapping on id + status
// afterwards is the TOCTOU this exists to remove: the row can fail again, under
// the same id and the same status, in between.
//
// false means zero rows matched — already reopened, or replaced by a newer
// failure. Both are idempotent no-ops for the caller, never errors.
func (s *Store) ReopenFailedWorkflowOutboxGeneration(
	ctx context.Context,
	id string,
	errorClass, generation string,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.ReopenFailedWorkflowOutboxGeneration(ctx, gen.ReopenFailedWorkflowOutboxGenerationParams{
		ErrorClass:        errorClass,
		ID:                id,
		FailureGeneration: generation,
	})
	if err != nil {
		return false, fmt.Errorf("reopen workflow outbox %s generation: %w", id, err)
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

// ClaimWorkflowAttemptOutcome concludes an attempt ONLY if nothing has
// concluded it yet, and reports whether this caller is the one that did.
//
// It is the durable arbiter of which of several concurrent verify executions is
// allowed to act. An attempt row with finished_at IS NULL means "in flight",
// and more than one execution can legitimately reach that state — a Continue
// and a Board poll entering the cascade together, a restart resuming an
// attempt whose process is still alive elsewhere. Before this existed they all
// concluded the attempt with an unconditional UPDATE and all went on to act on
// their own result, which is how one failing and one passing verification of
// the same target both produced side effects.
//
// The WHERE clause is the whole mechanism: exactly one UPDATE can match a row
// whose finished_at is NULL, so exactly one caller sees ok=true. Everyone else
// is a loser and must become a no-op. It is a compare-and-swap rather than a
// timestamp comparison deliberately — wall-clock ordering cannot decide a race
// between two writers, and a lease would still have to be arbitrated by
// something like this at the end.
//
// Hand-written rather than generated because sqlc is not part of the build
// here; the query is deliberately trivial so it stays readable next to the
// generated ones.
func (s *Store) ClaimWorkflowAttemptOutcome(
	ctx context.Context,
	attemptID string,
	finishedAt time.Time,
	outcome domain.WorkflowAttemptOutcome,
	errorClass domain.WorkflowErrorClass,
) (bool, error) {
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
	res, err := s.writeDB.ExecContext(ctx,
		`UPDATE workflow_attempts
		    SET finished_at = ?, outcome = ?, error_class = ?
		  WHERE id = ? AND finished_at IS NULL`,
		finishedAtNull, outcomePtr, errorClassPtr, attemptID)
	if err != nil {
		return false, fmt.Errorf("claim workflow attempt %s outcome: %w", attemptID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim workflow attempt %s outcome: %w", attemptID, err)
	}
	return n == 1, nil
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

// StartWorkflowStepForSession moves a step ready -> running ONLY while it
// durably holds the session the caller confirmed.
//
// The session is part of the predicate rather than a thing checked just before
// it. `id = ? AND state = 'ready'` licenses RUNNING by ORDER OF EXECUTION: any
// pass that happens to reach the statement satisfies it, so a step could enter
// RUNNING under a confirmation belonging to a different launch of the same step
// -- which is the precise shape of "running means AO intended to launch" that
// the phased dispatch exists to remove. Naming the session makes RUNNING
// licensed by the confirmation that wrote it and by nothing else.
//
// It reports whether it moved; a false answer means the step is no longer ready,
// or is holding somebody else's session, and the caller must not treat the step
// as running.
//
// Hand-written for the same reason ClaimWorkflowAttemptOutcome is.
func (s *Store) StartWorkflowStepForSession(
	ctx context.Context,
	id, sessionID string,
	now time.Time,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.writeDB.ExecContext(ctx,
		`UPDATE workflow_steps
		    SET state = 'running', updated_at = ?
		  WHERE id = ?
		    AND state = 'ready'
		    AND session_id = ?`,
		now, id, sessionID)
	if err != nil {
		return false, fmt.Errorf("start workflow step %s for session %s: %w", id, sessionID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("start workflow step %s for session %s: %w", id, sessionID, err)
	}
	return n == 1, nil
}

// ClaimOpenWorkflowAttempt returns the step's currently OPEN attempt, creating
// one only if there is none — atomically, under the store's write lock.
//
// It replaces a read-then-decide that lived in the coordinator:
// "attempts[len-1].Outcome == \"\"" is a positional test made outside any
// transaction, so two passes entering dispatch together could both read the same
// open attempt and both believe they held it, or both find none and both create
// one. An attempt row is what claims that work is in flight, so two passes
// holding "the" open attempt is precisely how one launch's failure came to
// conclude another launch's attempt.
//
// Serialising the read and the insert under writeMu makes "the open attempt" a
// single durable fact rather than a coincidence of timing: exactly one caller
// creates, and everyone else is handed the row that caller created. The bool
// reports which of the two happened, so a caller that needs to know whether it
// opened this attempt (rather than joining one) can tell.
//
// It deliberately does NOT reuse an attempt whose outcome is set: Checkpoint
// 8H's rule that a prior provider's failed attempt is never overwritten by its
// fallback's is unchanged.
//
// Hand-written for the same reason ClaimWorkflowAttemptOutcome is.
func (s *Store) ClaimOpenWorkflowAttempt(
	ctx context.Context,
	id, stepID, harness, model string,
	startedAt time.Time,
) (domain.WorkflowAttempt, bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.ListWorkflowAttemptsByStep(ctx, stepID)
	if err != nil {
		return domain.WorkflowAttempt{}, false, fmt.Errorf("list workflow attempts for step %s: %w", stepID, err)
	}
	if len(rows) > 0 {
		latest := workflowAttemptFromRow(rows[len(rows)-1])
		if latest.Outcome == "" {
			return latest, false, nil
		}
	}
	maxAttempt, err := s.qw.GetMaxWorkflowAttemptNumber(ctx, stepID)
	if err != nil {
		return domain.WorkflowAttempt{}, false, fmt.Errorf("get max workflow attempt number for step %s: %w", stepID, err)
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
		return domain.WorkflowAttempt{}, false, fmt.Errorf("insert workflow attempt for step %s: %w", stepID, err)
	}
	return workflowAttemptFromRow(row), true, nil
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
	if isSQLiteUnique(err) {
		// Only a uniqueness-constrained phase can land here — the review
		// authority claim/receipt phases from migration 0135. Surfaced as a
		// sentinel so its one caller can read it as "another reconciler already
		// claimed this replacement" instead of as a storage failure.
		return domain.WorkflowCheckpoint{}, fmt.Errorf(
			"insert workflow checkpoint for run %s phase %s: %w",
			cp.WorkflowRunID, cp.DurablePhase, domain.ErrDuplicateWorkflowCheckpoint)
	}
	if err != nil {
		return domain.WorkflowCheckpoint{}, fmt.Errorf("insert workflow checkpoint for run %s: %w", cp.WorkflowRunID, err)
	}
	return workflowCheckpointFromRow(row), nil
}

// RebindWorkflowStepReviewRunFrom compare-and-swaps a review step's authority
// pointer. expected "" means "currently unset" (the state review-authority
// reconciliation leaves behind when it releases a step).
//
// False is never a failure — it is "somebody else owns this step now", and the
// caller must stop acting as its owner.
func (s *Store) RebindWorkflowStepReviewRunFrom(
	ctx context.Context, stepID, expected, predecessor, next string, now time.Time,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var rows int64
	var err error
	if expected == "" {
		rows, err = s.qw.ClaimWorkflowStepReviewRunIfUnset(ctx, gen.ClaimWorkflowStepReviewRunIfUnsetParams{
			ReviewRunID:            sql.NullString{String: next, Valid: next != ""},
			UpdatedAt:              now,
			ID:                     stepID,
			PredecessorReviewRunID: predecessor,
		})
	} else {
		rows, err = s.qw.RebindWorkflowStepReviewRunFrom(ctx, gen.RebindWorkflowStepReviewRunFromParams{
			NextReviewRunID:        sql.NullString{String: next, Valid: next != ""},
			UpdatedAt:              now,
			ID:                     stepID,
			ExpectedReviewRunID:    sql.NullString{String: expected, Valid: true},
			PredecessorReviewRunID: predecessor,
		})
	}
	if err != nil {
		return false, fmt.Errorf("rebind workflow step %s review run: %w", stepID, err)
	}
	return rows > 0, nil
}

// UpdateWorkflowStepStateIfReviewRun applies a step transition only while the
// named review run is still the step's authority. False means the decision was
// stale — the pointer moved, or the step was not in the expected state.
func (s *Store) UpdateWorkflowStepStateIfReviewRun(
	ctx context.Context, stepID string, expected, next domain.WorkflowStepState,
	reviewRunID string, now time.Time,
) (bool, error) {
	if !domain.ValidWorkflowStepTransition(expected, next) {
		return false, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var completedAt sql.NullTime
	if next.Terminal() {
		completedAt = sql.NullTime{Time: now, Valid: true}
	}
	rows, err := s.qw.UpdateWorkflowStepStateIfReviewRun(ctx, gen.UpdateWorkflowStepStateIfReviewRunParams{
		State:         next,
		UpdatedAt:     now,
		CompletedAt:   completedAt,
		ID:            stepID,
		ExpectedState: expected,
		ReviewRunID:   sql.NullString{String: reviewRunID, Valid: reviewRunID != ""},
	})
	if err != nil {
		return false, fmt.Errorf("update workflow step %s state under review run %s: %w", stepID, reviewRunID, err)
	}
	return rows > 0, nil
}

// ReleaseWorkflowStepReviewRunIfNoLateVerdict clears a review step's authority
// pointer only while the run it names still has no late verdict.
//
// The check and the release are one statement on purpose: a reconciler that
// tested for a late verdict and then cleared the pointer in a second call could
// have the reviewer's verdict land between the two, orphaning a valid
// authoritative verdict and dispatching a replacement over completed work.
// False means the decision was stale — re-read and decide again.
func (s *Store) ReleaseWorkflowStepReviewRunIfNoLateVerdict(
	ctx context.Context, stepID, reviewRunID string, now time.Time,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.ReleaseWorkflowStepReviewRunIfNoLateVerdict(
		ctx, gen.ReleaseWorkflowStepReviewRunIfNoLateVerdictParams{
			UpdatedAt:   now,
			ID:          stepID,
			ReviewRunID: sql.NullString{String: reviewRunID, Valid: reviewRunID != ""},
		})
	if err != nil {
		return false, fmt.Errorf("release workflow step %s review run: %w", stepID, err)
	}
	return rows > 0, nil
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

// ListWorkflowRunIDsByCheckpointPhase returns the runs carrying a checkpoint of
// the given durable phase, terminal runs included.
func (s *Store) ListWorkflowRunIDsByCheckpointPhase(ctx context.Context, phase string) ([]string, error) {
	ids, err := s.qr.ListWorkflowRunIDsByCheckpointPhase(ctx, phase)
	if err != nil {
		return nil, fmt.Errorf("list workflow runs by checkpoint phase %s: %w", phase, err)
	}
	return ids, nil
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
		ArchivedAt:       nullTimeToTimePtr(r.ArchivedAt),
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
		// Migration 0133. An attempt written before it has no deadline and no
		// review target, and maps to a nil pointer and a zero-valued
		// WorkflowReviewTarget rather than to an error.
		DeadlineAt: nullTimeToTimePtr(r.DeadlineAt),
		ReviewTarget: domain.WorkflowReviewTarget{
			ReviewRunID: nullStringToPtr(r.ReviewTargetReviewRunID),
			Fingerprint: r.ReviewTargetFingerprint,
			HeadSHA:     r.ReviewTargetHeadSha,
		},
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

		FailureGeneration:  r.FailureGeneration,
		DispatchGeneration: r.DispatchGeneration,
	}
}

// RecordAgentHealthEvent inserts an append-only health fact for a harness
// (Checkpoint 8H). Never updated; GetAgentHealth derives current state from
// the latest event.
func (s *Store) RecordAgentHealthEvent(ctx context.Context, ev domain.AgentHealthEvent) (domain.AgentHealthEvent, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	row, err := s.qw.InsertAgentHealthEvent(ctx, gen.InsertAgentHealthEventParams{
		ID:                  ev.ID,
		Harness:             string(ev.Harness),
		UserID:              userIDToNullString(ev.UserID),
		ProviderProfileID:   profileIDToNullString(ev.ProviderProfileID),
		State:               string(ev.State),
		Reason:              ev.Reason,
		FailureClass:        string(ev.FailureClass),
		CooldownUntil:       timePtrToNullTime(ev.CooldownUntil),
		ConsecutiveFailures: ev.ConsecutiveFailures,
		CreatedAt:           ev.CreatedAt,
	})
	if err != nil {
		return domain.AgentHealthEvent{}, fmt.Errorf("insert agent health event for harness %s: %w", ev.Harness, err)
	}
	return agentHealthEventFromRow(agentHealthEventRow(row)), nil
}

// GetAgentHealth returns the latest legacy/global recorded health event for
// a harness (ignoring any user/profile scope), ok=false if none has ever
// been recorded (domain.AgentHealthUnknown). Used directly by trusted-local
// mode; service/capacity's precedence rule also calls this as an explicit
// fallback when GetAgentHealthScoped finds nothing.
func (s *Store) GetAgentHealth(ctx context.Context, harness domain.AgentHarness) (domain.AgentHealthEvent, bool, error) {
	row, err := s.qr.GetLatestAgentHealthEvent(ctx, string(harness))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AgentHealthEvent{}, false, nil
	}
	if err != nil {
		return domain.AgentHealthEvent{}, false, fmt.Errorf("get latest agent health event for harness %s: %w", harness, err)
	}
	return agentHealthEventFromRow(agentHealthEventRow(row)), true, nil
}

// GetAgentHealthScoped returns the latest health event recorded for the
// exact (harness, userID, profileID) triple -- never a legacy/global row,
// never another user's or another profile's row. ok=false means no scoped
// event exists yet for this connection (Checkpoint 8P-C).
func (s *Store) GetAgentHealthScoped(ctx context.Context, harness domain.AgentHarness, userID domain.UserID, profileID domain.ProviderProfileID) (domain.AgentHealthEvent, bool, error) {
	row, err := s.qr.GetLatestAgentHealthEventScoped(ctx, gen.GetLatestAgentHealthEventScopedParams{
		Harness:           string(harness),
		UserID:            userIDToNullString(userID),
		ProviderProfileID: profileIDToNullString(profileID),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AgentHealthEvent{}, false, nil
	}
	if err != nil {
		return domain.AgentHealthEvent{}, false, fmt.Errorf("get scoped agent health event for harness %s: %w", harness, err)
	}
	return agentHealthEventFromRow(agentHealthEventRow(row)), true, nil
}

func userIDToNullString(id domain.UserID) sql.NullString {
	if id == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: string(id), Valid: true}
}

func profileIDToNullString(id domain.ProviderProfileID) sql.NullString {
	if id == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: string(id), Valid: true}
}

// agentHealthEventRow is the shape shared by every generated agent_health_events
// row type (Insert/GetLatest/GetLatestScoped each get their own sqlc row
// struct despite selecting the same columns).
type agentHealthEventRow struct {
	ID                  string
	Harness             string
	UserID              sql.NullString
	ProviderProfileID   sql.NullString
	State               string
	Reason              string
	FailureClass        string
	CooldownUntil       sql.NullTime
	ConsecutiveFailures int64
	CreatedAt           time.Time
}

func agentHealthEventFromRow(r agentHealthEventRow) domain.AgentHealthEvent {
	return domain.AgentHealthEvent{
		ID:                  r.ID,
		Harness:             domain.AgentHarness(r.Harness),
		UserID:              domain.UserID(r.UserID.String),
		ProviderProfileID:   domain.ProviderProfileID(r.ProviderProfileID.String),
		State:               domain.AgentHealthState(r.State),
		Reason:              r.Reason,
		FailureClass:        domain.WorkflowErrorClass(r.FailureClass),
		CooldownUntil:       nullTimeToTimePtr(r.CooldownUntil),
		ConsecutiveFailures: r.ConsecutiveFailures,
		CreatedAt:           r.CreatedAt,
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
