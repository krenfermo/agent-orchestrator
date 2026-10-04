package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// WorkflowWakeSchedule is the store-layer domain view of one durable wake-up
// scheduling row (Checkpoint 8N). It lives here rather than in package
// domain because, like domain.WorkflowQuestionResolution's own row shape,
// nothing outside storage/workflow-wake needs to construct one directly —
// the workflow/wake package defines its own narrower Schedule type and maps
// to/from this one at the Store interface boundary.
type WorkflowWakeSchedule struct {
	ID             string
	WorkflowRunID  string
	WorkflowStepID *string
	Reason         string
	Status         string
	IdempotencyKey string
	ScheduledAt    time.Time
	KnownResetAt   *time.Time
	AttemptCount   int64
	ClaimedBy      string
	ClaimedAt      *time.Time
	CompletedAt    *time.Time
	CancelledAt    *time.Time
	LastError      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// UpsertWorkflowWakeSchedule inserts a new wake schedule row, or — if a row
// with the same idempotency_key already exists and is still pending/claimed
// — reschedules that existing row in place (incrementing its attempt_count)
// rather than creating a duplicate. This is the idempotent-upsert flow
// workflow/wake.Scheduler.Schedule relies on; it is implemented here as a
// SELECT-then-branch under writeMu rather than SQLite's
// INSERT ... ON CONFLICT DO UPDATE, matching this store package's existing
// convention of doing read-then-write atomicity in Go when a query needs
// more branching than a single CAS statement can express cleanly (compare
// TransitionResolutionStatus's own simpler single-statement CAS, which this
// is not: an upsert must choose between two entirely different SQL
// statements depending on whether a row already exists).
func (s *Store) UpsertWorkflowWakeSchedule(ctx context.Context, sch WorkflowWakeSchedule) (WorkflowWakeSchedule, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	existing, err := s.qw.GetWorkflowWakeScheduleByIdempotencyKey(ctx, sch.IdempotencyKey)
	if err == nil {
		if existing.Status == "pending" || existing.Status == "claimed" {
			n, rerr := s.qw.RescheduleWorkflowWakeSchedule(ctx, gen.RescheduleWorkflowWakeScheduleParams{
				ScheduledAt:  sch.ScheduledAt,
				KnownResetAt: timePtrToNullTime(sch.KnownResetAt),
				LastError:    sch.LastError,
				UpdatedAt:    sch.UpdatedAt,
				ID:           existing.ID,
			})
			if rerr != nil {
				return WorkflowWakeSchedule{}, fmt.Errorf("reschedule workflow wake schedule %s: %w", existing.ID, rerr)
			}
			if n == 0 {
				// Lost a race against a concurrent completion/cancellation
				// between the read above and this write — fall through and
				// insert a fresh row instead of silently doing nothing.
				return s.insertWorkflowWakeSchedule(ctx, sch)
			}
			row, gerr := s.qw.GetWorkflowWakeSchedule(ctx, existing.ID)
			if gerr != nil {
				return WorkflowWakeSchedule{}, fmt.Errorf("reload rescheduled workflow wake schedule %s: %w", existing.ID, gerr)
			}
			return workflowWakeScheduleFromRow(row), nil
		}
		// Existing row is completed/cancelled: the same scope is waiting
		// again.
		//
		// Checkpoint 8P-E13A.1: this branch used to insert a fresh row, which
		// could never work — idempotency_key is globally UNIQUE (migration
		// 0106), so every attempt failed with a constraint violation and the
		// wake was silently dropped. The effect in the field was severe and
		// invisible: once a run's branch_lock or autonomous_progress wake had
		// completed even once, no later wake for that scope could ever be
		// scheduled again, so the run stopped resuming on its own and only a
		// human clicking Continue moved it. Reviving the row in place is what
		// the unique constraint always implied: one row per scope, reused.
		revived, rerr := s.qw.ReviveWorkflowWakeSchedule(ctx, gen.ReviveWorkflowWakeScheduleParams{
			ScheduledAt:  sch.ScheduledAt,
			KnownResetAt: timePtrToNullTime(sch.KnownResetAt),
			UpdatedAt:    sch.UpdatedAt,
			ID:           existing.ID,
		})
		if rerr != nil {
			return WorkflowWakeSchedule{}, fmt.Errorf("revive workflow wake schedule %s: %w", existing.ID, rerr)
		}
		if revived == 0 {
			// Lost a race against a concurrent reschedule that moved the row
			// back to pending/claimed between the read and this write: that
			// row is already an open wake for this scope, so report it.
			row, gerr := s.qw.GetWorkflowWakeSchedule(ctx, existing.ID)
			if gerr != nil {
				return WorkflowWakeSchedule{}, fmt.Errorf("reload contended workflow wake schedule %s: %w", existing.ID, gerr)
			}
			return workflowWakeScheduleFromRow(row), nil
		}
		row, gerr := s.qw.GetWorkflowWakeSchedule(ctx, existing.ID)
		if gerr != nil {
			return WorkflowWakeSchedule{}, fmt.Errorf("reload revived workflow wake schedule %s: %w", existing.ID, gerr)
		}
		return workflowWakeScheduleFromRow(row), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return WorkflowWakeSchedule{}, fmt.Errorf("lookup workflow wake schedule by idempotency key %s: %w", sch.IdempotencyKey, err)
	}
	return s.insertWorkflowWakeSchedule(ctx, sch)
}

// insertWorkflowWakeSchedule is the plain-insert half of UpsertWorkflowWakeSchedule.
// Callers must already hold s.writeMu.
func (s *Store) insertWorkflowWakeSchedule(ctx context.Context, sch WorkflowWakeSchedule) (WorkflowWakeSchedule, error) {
	row, err := s.qw.InsertWorkflowWakeSchedule(ctx, gen.InsertWorkflowWakeScheduleParams{
		ID:             sch.ID,
		WorkflowRunID:  sch.WorkflowRunID,
		WorkflowStepID: stringPtrToNullString(sch.WorkflowStepID),
		Reason:         sch.Reason,
		Status:         "pending",
		IdempotencyKey: sch.IdempotencyKey,
		ScheduledAt:    sch.ScheduledAt,
		KnownResetAt:   timePtrToNullTime(sch.KnownResetAt),
		AttemptCount:   1,
		LastError:      "",
		CreatedAt:      sch.CreatedAt,
		UpdatedAt:      sch.UpdatedAt,
	})
	if err != nil {
		return WorkflowWakeSchedule{}, fmt.Errorf("insert workflow wake schedule: %w", err)
	}
	return workflowWakeScheduleFromRow(row), nil
}

// GetWorkflowWakeScheduleByIdempotencyKey returns the wake schedule row for
// an exact idempotency key, if one exists. Used by wake.Scheduler.Schedule to
// read a wake's real current AttemptCount before computing its next backoff
// delay, rather than trusting a caller-supplied attempt number that can
// drift from the store's own count.
func (s *Store) GetWorkflowWakeScheduleByIdempotencyKey(ctx context.Context, idempotencyKey string) (WorkflowWakeSchedule, bool, error) {
	row, err := s.qr.GetWorkflowWakeScheduleByIdempotencyKey(ctx, idempotencyKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkflowWakeSchedule{}, false, nil
		}
		return WorkflowWakeSchedule{}, false, fmt.Errorf("get workflow wake schedule by idempotency key %s: %w", idempotencyKey, err)
	}
	return workflowWakeScheduleFromRow(row), true, nil
}

// ListDueWorkflowWakeSchedules returns every pending wake whose scheduled_at
// has passed, plus any claimed wake whose claim lease (claimLeaseCutoff) has
// expired — a prior claimant crashed mid-fire. Sorted by ScheduledAt in Go
// (see workflow_wake_schedules.sql's own note on why this query has no
// ORDER BY) and truncated to limit rows.
func (s *Store) ListDueWorkflowWakeSchedules(ctx context.Context, now, claimLeaseCutoff time.Time, limit int) ([]WorkflowWakeSchedule, error) {
	rows, err := s.qr.ListDueWorkflowWakeSchedules(ctx, gen.ListDueWorkflowWakeSchedulesParams{
		ScheduledAt: now,
		ClaimedAt:   sql.NullTime{Time: claimLeaseCutoff, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("list due workflow wake schedules: %w", err)
	}
	out := make([]WorkflowWakeSchedule, 0, len(rows))
	for _, r := range rows {
		out = append(out, workflowWakeScheduleFromRow(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ScheduledAt.Before(out[j].ScheduledAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ClaimWorkflowWakeSchedule CAS-claims one due wake for a claimant. Returns
// ok=false, no error, if the row was no longer in expectedStatus (another
// claimant/poller already handled it) — the caller must treat that as
// "someone else already handled it", matching TransitionResolutionStatus's
// own convention.
func (s *Store) ClaimWorkflowWakeSchedule(ctx context.Context, id, expectedStatus, claimant string, at time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.ClaimWorkflowWakeSchedule(ctx, gen.ClaimWorkflowWakeScheduleParams{
		ClaimedBy:      sql.NullString{String: claimant, Valid: claimant != ""},
		ClaimedAt:      sql.NullTime{Time: at, Valid: true},
		UpdatedAt:      at,
		ID:             id,
		ExpectedStatus: expectedStatus,
	})
	if err != nil {
		return false, fmt.Errorf("claim workflow wake schedule %s: %w", id, err)
	}
	return n > 0, nil
}

// CompleteWorkflowWakeSchedule marks a claimed wake completed. ok=false, no
// error, if it was not currently claimed.
func (s *Store) CompleteWorkflowWakeSchedule(ctx context.Context, id string, at time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.CompleteWorkflowWakeSchedule(ctx, gen.CompleteWorkflowWakeScheduleParams{
		CompletedAt: sql.NullTime{Time: at, Valid: true},
		UpdatedAt:   at,
		ID:          id,
	})
	if err != nil {
		return false, fmt.Errorf("complete workflow wake schedule %s: %w", id, err)
	}
	return n > 0, nil
}

// RescheduleWorkflowWakeSchedule reschedules an existing wake back to
// pending with a new scheduled_at/known_reset_at and increments its
// attempt_count — used by wake.Scheduler.Fail's backoff-and-retry path.
func (s *Store) RescheduleWorkflowWakeSchedule(ctx context.Context, id string, scheduledAt time.Time, knownResetAt *time.Time, lastError string, at time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.RescheduleWorkflowWakeSchedule(ctx, gen.RescheduleWorkflowWakeScheduleParams{
		ScheduledAt:  scheduledAt,
		KnownResetAt: timePtrToNullTime(knownResetAt),
		LastError:    lastError,
		UpdatedAt:    at,
		ID:           id,
	})
	if err != nil {
		return false, fmt.Errorf("reschedule workflow wake schedule %s: %w", id, err)
	}
	return n > 0, nil
}

// CancelWorkflowWakeSchedule cancels one pending/claimed wake by id.
func (s *Store) CancelWorkflowWakeSchedule(ctx context.Context, id string, at time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.CancelWorkflowWakeSchedule(ctx, gen.CancelWorkflowWakeScheduleParams{
		CancelledAt: sql.NullTime{Time: at, Valid: true},
		UpdatedAt:   at,
		ID:          id,
	})
	if err != nil {
		return false, fmt.Errorf("cancel workflow wake schedule %s: %w", id, err)
	}
	return n > 0, nil
}

// CancelAllWorkflowWakeSchedulesByRun cancels every pending/claimed wake for
// a run (CancelRun's cascade — mirrors CancelOpenWorkflowQuestionsByRun).
// Returns the number of rows cancelled.
func (s *Store) CancelAllWorkflowWakeSchedulesByRun(ctx context.Context, runID string, at time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.CancelAllWorkflowWakeSchedulesByRun(ctx, gen.CancelAllWorkflowWakeSchedulesByRunParams{
		CancelledAt:   sql.NullTime{Time: at, Valid: true},
		UpdatedAt:     at,
		WorkflowRunID: runID,
	})
	if err != nil {
		return 0, fmt.Errorf("cancel workflow wake schedules for run %s: %w", runID, err)
	}
	return n, nil
}

// ListPendingWorkflowWakeSchedulesByRun returns every open (pending or
// claimed) wake for a run, sorted by ScheduledAt ascending in Go (see
// workflow_wake_schedules.sql's own note on why this query has no ORDER BY).
func (s *Store) ListPendingWorkflowWakeSchedulesByRun(ctx context.Context, runID string) ([]WorkflowWakeSchedule, error) {
	rows, err := s.qr.ListPendingWorkflowWakeSchedulesByRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list pending workflow wake schedules for run %s: %w", runID, err)
	}
	out := make([]WorkflowWakeSchedule, 0, len(rows))
	for _, r := range rows {
		out = append(out, workflowWakeScheduleFromRow(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ScheduledAt.Before(out[j].ScheduledAt) })
	return out, nil
}

func nullStringToStringPtr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	v := s.String
	return &v
}

func workflowWakeScheduleFromRow(r gen.WorkflowWakeSchedule) WorkflowWakeSchedule {
	return WorkflowWakeSchedule{
		ID:             r.ID,
		WorkflowRunID:  r.WorkflowRunID,
		WorkflowStepID: nullStringToStringPtr(r.WorkflowStepID),
		Reason:         r.Reason,
		Status:         r.Status,
		IdempotencyKey: r.IdempotencyKey,
		ScheduledAt:    r.ScheduledAt,
		KnownResetAt:   nullTimeToTimePtr(r.KnownResetAt),
		AttemptCount:   r.AttemptCount,
		ClaimedBy:      r.ClaimedBy.String,
		ClaimedAt:      nullTimeToTimePtr(r.ClaimedAt),
		CompletedAt:    nullTimeToTimePtr(r.CompletedAt),
		CancelledAt:    nullTimeToTimePtr(r.CancelledAt),
		LastError:      r.LastError,
		CreatedAt:      r.CreatedAt,
		UpdatedAt:      r.UpdatedAt,
	}
}
