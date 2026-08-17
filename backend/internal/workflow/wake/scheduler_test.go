package wake

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// fakeStore is an in-memory implementation of Store for unit testing
// Scheduler without a real database.
type fakeStore struct {
	rows map[string]store.WorkflowWakeSchedule
	seq  int
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[string]store.WorkflowWakeSchedule{}}
}

func (f *fakeStore) UpsertWorkflowWakeSchedule(_ context.Context, sch store.WorkflowWakeSchedule) (store.WorkflowWakeSchedule, error) {
	for _, r := range f.rows {
		if r.IdempotencyKey == sch.IdempotencyKey && (r.Status == "pending" || r.Status == "claimed") {
			r.ScheduledAt = sch.ScheduledAt
			r.KnownResetAt = sch.KnownResetAt
			r.AttemptCount++
			r.Status = "pending"
			r.UpdatedAt = sch.UpdatedAt
			f.rows[r.ID] = r
			return r, nil
		}
	}
	sch.Status = "pending"
	sch.AttemptCount = 1
	f.rows[sch.ID] = sch
	return sch, nil
}

func (f *fakeStore) ListDueWorkflowWakeSchedules(_ context.Context, now, claimLeaseCutoff time.Time, limit int) ([]store.WorkflowWakeSchedule, error) {
	var out []store.WorkflowWakeSchedule
	for _, r := range f.rows {
		if r.Status == "pending" && !r.ScheduledAt.After(now) {
			out = append(out, r)
		} else if r.Status == "claimed" && r.ClaimedAt != nil && !r.ClaimedAt.After(claimLeaseCutoff) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeStore) ClaimWorkflowWakeSchedule(_ context.Context, id, expectedStatus, claimant string, at time.Time) (bool, error) {
	r, ok := f.rows[id]
	if !ok || r.Status != expectedStatus {
		return false, nil
	}
	r.Status = "claimed"
	r.ClaimedBy = claimant
	claimedAt := at
	r.ClaimedAt = &claimedAt
	f.rows[id] = r
	return true, nil
}

func (f *fakeStore) CompleteWorkflowWakeSchedule(_ context.Context, id string, at time.Time) (bool, error) {
	r, ok := f.rows[id]
	if !ok || r.Status != "claimed" {
		return false, nil
	}
	r.Status = "completed"
	r.CompletedAt = &at
	f.rows[id] = r
	return true, nil
}

func (f *fakeStore) RescheduleWorkflowWakeSchedule(_ context.Context, id string, scheduledAt time.Time, knownResetAt *time.Time, lastError string, at time.Time) (bool, error) {
	r, ok := f.rows[id]
	if !ok || (r.Status != "pending" && r.Status != "claimed") {
		return false, nil
	}
	r.Status = "pending"
	r.ScheduledAt = scheduledAt
	r.KnownResetAt = knownResetAt
	r.LastError = lastError
	r.AttemptCount++
	r.ClaimedBy = ""
	r.ClaimedAt = nil
	r.UpdatedAt = at
	f.rows[id] = r
	return true, nil
}

func (f *fakeStore) CancelWorkflowWakeSchedule(_ context.Context, id string, at time.Time) (bool, error) {
	r, ok := f.rows[id]
	if !ok || (r.Status != "pending" && r.Status != "claimed") {
		return false, nil
	}
	r.Status = "cancelled"
	r.CancelledAt = &at
	f.rows[id] = r
	return true, nil
}

func (f *fakeStore) CancelAllWorkflowWakeSchedulesByRun(_ context.Context, runID string, at time.Time) (int64, error) {
	var n int64
	for id, r := range f.rows {
		if r.WorkflowRunID != runID || (r.Status != "pending" && r.Status != "claimed") {
			continue
		}
		r.Status = "cancelled"
		r.CancelledAt = &at
		f.rows[id] = r
		n++
	}
	return n, nil
}

func (f *fakeStore) ListPendingWorkflowWakeSchedulesByRun(_ context.Context, runID string) ([]store.WorkflowWakeSchedule, error) {
	var out []store.WorkflowWakeSchedule
	for _, r := range f.rows {
		if r.WorkflowRunID == runID && (r.Status == "pending" || r.Status == "claimed") {
			out = append(out, r)
		}
	}
	return out, nil
}

func newTestScheduler(fs *fakeStore, now time.Time) *Scheduler {
	seq := 0
	newID := func() string {
		seq++
		return "id" + string(rune('a'+seq))
	}
	clock := func() time.Time { return now }
	return New(fs, clock, newID, Config{Policy: domain.DefaultWakePolicy(), Rand: rand.New(rand.NewSource(1))})
}

func TestComputeScheduledAt_KnownResetBranch(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reset := now.Add(10 * time.Minute)
	cfg := domain.DefaultWakePolicy()
	got, isKnown := computeScheduledAt(now, &reset, 0, cfg, rand.New(rand.NewSource(1)))
	if !isKnown {
		t.Fatalf("expected known-reset branch")
	}
	want := reset.Add(time.Duration(cfg.KnownResetSafetyDelaySeconds) * time.Second)
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestComputeScheduledAt_UnknownResetExponentialBackoffCapped(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg := domain.DefaultWakePolicy()
	rng := rand.New(rand.NewSource(42))

	got0, isKnown := computeScheduledAt(now, nil, 0, cfg, rng)
	if isKnown {
		t.Fatalf("expected unknown-reset branch at attempt 0")
	}
	minDelay := time.Duration(cfg.InitialBackoffSeconds) * time.Second
	maxDelay := minDelay + time.Duration(cfg.JitterSeconds)*time.Second
	if got0.Before(now.Add(minDelay)) || got0.After(now.Add(maxDelay)) {
		t.Fatalf("attempt 0 delay out of range: %v", got0.Sub(now))
	}

	// A high attempt count must be capped at MaxBackoffSeconds + jitter, not
	// grow unbounded.
	gotHigh, _ := computeScheduledAt(now, nil, 20, cfg, rng)
	capDelay := time.Duration(cfg.MaxBackoffSeconds)*time.Second + time.Duration(cfg.JitterSeconds)*time.Second
	if gotHigh.After(now.Add(capDelay)) {
		t.Fatalf("attempt 20 delay exceeded cap: %v > %v", gotHigh.Sub(now), capDelay)
	}
}

func TestComputeScheduledAt_PastResetFallsBackToBackoff(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	past := now.Add(-time.Minute)
	cfg := domain.DefaultWakePolicy()
	_, isKnown := computeScheduledAt(now, &past, 0, cfg, rand.New(rand.NewSource(1)))
	if isKnown {
		t.Fatalf("a knownResetAt already in the past must not be treated as known-reset")
	}
}

func TestScheduler_IdempotentUpsert(t *testing.T) {
	fs := newFakeStore()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sched := newTestScheduler(fs, now)
	ctx := context.Background()
	runID := domain.WorkflowRunID("wf-1")
	stepID := domain.WorkflowStepID("wfs-1")

	first, err := sched.Schedule(ctx, runID, &stepID, ReasonWorkerCapacity, nil, 0)
	if err != nil {
		t.Fatalf("first schedule: %v", err)
	}
	if len(fs.rows) != 1 {
		t.Fatalf("expected 1 row after first schedule, got %d", len(fs.rows))
	}

	second, err := sched.Schedule(ctx, runID, &stepID, ReasonWorkerCapacity, nil, 1)
	if err != nil {
		t.Fatalf("second schedule: %v", err)
	}
	if len(fs.rows) != 1 {
		t.Fatalf("expected 1 row after second schedule (idempotent upsert), got %d", len(fs.rows))
	}
	if second.ID != first.ID {
		t.Fatalf("expected same row id across idempotent schedules, got %s vs %s", first.ID, second.ID)
	}
	if second.AttemptCount != 2 {
		t.Fatalf("expected attempt_count to increment to 2, got %d", second.AttemptCount)
	}
}

func TestScheduler_ClaimDue_OnlyDueRows(t *testing.T) {
	fs := newFakeStore()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sched := newTestScheduler(fs, now)
	ctx := context.Background()

	// Due now: force scheduled_at to now (Schedule's own backoff always adds
	// InitialBackoffSeconds, so simulate "time has passed" by rewriting the
	// row directly, same as advancing a fake clock past scheduled_at would).
	due, err := sched.Schedule(ctx, "wf-1", nil, ReasonReviewerCapacity, nil, 0)
	if err != nil {
		t.Fatalf("schedule due: %v", err)
	}
	dueRow := fs.rows[due.ID]
	dueRow.ScheduledAt = now
	fs.rows[due.ID] = dueRow

	// Not due yet: schedule far in the future by using a known reset.
	future := now.Add(time.Hour)
	if _, err := sched.Schedule(ctx, "wf-2", nil, ReasonWorkerCapacity, &future, 0); err != nil {
		t.Fatalf("schedule future: %v", err)
	}

	claimed, err := sched.ClaimDue(ctx, "claimant-1", 25)
	if err != nil {
		t.Fatalf("claim due: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("expected exactly 1 due wake claimed, got %d", len(claimed))
	}
	if claimed[0].WorkflowRunID != "wf-1" {
		t.Fatalf("claimed wrong wake: %+v", claimed[0])
	}

	// A second ClaimDue call must not re-claim the same (now-claimed, lease
	// still fresh) row.
	claimedAgain, err := sched.ClaimDue(ctx, "claimant-2", 25)
	if err != nil {
		t.Fatalf("second claim due: %v", err)
	}
	if len(claimedAgain) != 0 {
		t.Fatalf("expected no rows re-claimed while lease is fresh, got %d", len(claimedAgain))
	}
}

func TestScheduler_FailReschedulesUntilBudgetExhausted(t *testing.T) {
	fs := newFakeStore()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sched := newTestScheduler(fs, now)
	cfg := domain.DefaultWakePolicy()
	cfg.MaxAttempts = 2
	sched.cfg.Policy = cfg
	ctx := context.Background()

	sch, err := sched.Schedule(ctx, "wf-1", nil, ReasonWorkerCapacity, nil, 0)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}

	res, err := sched.Fail(ctx, sch, "boom")
	if err != nil {
		t.Fatalf("fail 1: %v", err)
	}
	if !res.Rescheduled || res.BudgetExhausted {
		t.Fatalf("expected reschedule on first failure, got %+v", res)
	}

	sch.AttemptCount = 2
	res2, err := sched.Fail(ctx, sch, "boom again")
	if err != nil {
		t.Fatalf("fail 2: %v", err)
	}
	if !res2.BudgetExhausted {
		t.Fatalf("expected budget exhausted at MaxAttempts, got %+v", res2)
	}
	row := fs.rows[sch.ID]
	if row.Status != "cancelled" {
		t.Fatalf("expected wake cancelled after budget exhausted, got status %s", row.Status)
	}
}

func TestScheduler_CancelAllForRun(t *testing.T) {
	fs := newFakeStore()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sched := newTestScheduler(fs, now)
	ctx := context.Background()

	if _, err := sched.Schedule(ctx, "wf-1", nil, ReasonWorkerCapacity, nil, 0); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if _, err := sched.Schedule(ctx, "wf-1", nil, ReasonReviewerCapacity, nil, 0); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if _, err := sched.Schedule(ctx, "wf-2", nil, ReasonWorkerCapacity, nil, 0); err != nil {
		t.Fatalf("schedule: %v", err)
	}

	n, err := sched.CancelAllForRun(ctx, "wf-1")
	if err != nil {
		t.Fatalf("cancel all: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 wakes cancelled, got %d", n)
	}
	next, err := sched.NextForRun(ctx, "wf-1")
	if err != nil {
		t.Fatalf("next for run: %v", err)
	}
	if next != nil {
		t.Fatalf("expected no open wake left for wf-1, got %+v", next)
	}
	next2, err := sched.NextForRun(ctx, "wf-2")
	if err != nil {
		t.Fatalf("next for run wf-2: %v", err)
	}
	if next2 == nil {
		t.Fatalf("expected wf-2's wake to remain open")
	}
}
