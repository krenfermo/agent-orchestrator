// Package wake is Checkpoint 8N's durable, restart-safe wake-up scheduler:
// when a workflow run/step parks in WorkflowRunWaiting/WorkflowStepWaiting
// on provider capacity, Scheduler.Schedule persists a wake (near a known
// reset time if one exists, else bounded exponential backoff with jitter),
// and a lightweight daemon-level poller (outside this package — see
// backend/internal/daemon/workflow_wiring.go) fires due wakes by claiming
// them and calling the coordinator's existing idempotent ContinueRun/GetRun.
//
// This package deliberately knows nothing about the workflow.Coordinator,
// session dispatch, or HTTP — it only knows how to compute and persist
// "when should this scope be retried" and hand back due rows to whatever
// caller wants to fire them. It is unit-testable in isolation against a
// fake Store.
package wake

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// Reason is the fixed vocabulary of why a wake was scheduled, mirroring the
// workflow_wake_schedules.reason CHECK enum (migration 0106).
type Reason string

const (
	ReasonCapacityReset            Reason = "capacity_reset"
	ReasonCapacityProbe            Reason = "capacity_probe"
	ReasonTransientRetry           Reason = "transient_retry"
	ReasonQuestionResolverCapacity Reason = "question_resolver_capacity"
	ReasonReviewerCapacity         Reason = "reviewer_capacity"
	ReasonWorkerCapacity           Reason = "worker_capacity"
	// ReasonPlannerCapacity is defined for forward-compatibility with a
	// future checkpoint's planner-capacity wake producer. Checkpoint 8N
	// explicitly does not wire any code path that produces this reason
	// (master_coordinator.go's GeneratePlan/planner error handling is out
	// of scope) — the value exists so the enum is complete, not because
	// anything schedules it today.
	ReasonPlannerCapacity Reason = "planner_capacity"
)

// Status is the fixed vocabulary of a wake schedule's lifecycle state,
// mirroring the workflow_wake_schedules.status CHECK enum.
type Status string

const (
	StatusPending   Status = "pending"
	StatusClaimed   Status = "claimed"
	StatusCompleted Status = "completed"
	StatusCancelled Status = "cancelled"
)

// Schedule is the wake package's own domain-ish view of one durable wake
// row, mirroring store.WorkflowWakeSchedule but with this package's typed
// Reason/Status vocabulary and domain run/step ids.
type Schedule struct {
	ID             string
	WorkflowRunID  domain.WorkflowRunID
	WorkflowStepID *domain.WorkflowStepID
	Reason         Reason
	Status         Status
	IdempotencyKey string
	ScheduledAt    time.Time
	KnownResetAt   *time.Time
	AttemptCount   int64
	LastError      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Store is the narrow persistence surface Scheduler needs. Satisfied by
// *sqlite.Store's Checkpoint 8N methods (workflow_wake_store.go); tests use
// a fake.
type Store interface {
	UpsertWorkflowWakeSchedule(ctx context.Context, sch store.WorkflowWakeSchedule) (store.WorkflowWakeSchedule, error)
	ListDueWorkflowWakeSchedules(ctx context.Context, now, claimLeaseCutoff time.Time, limit int) ([]store.WorkflowWakeSchedule, error)
	ClaimWorkflowWakeSchedule(ctx context.Context, id, expectedStatus, claimant string, at time.Time) (bool, error)
	CompleteWorkflowWakeSchedule(ctx context.Context, id string, at time.Time) (bool, error)
	RescheduleWorkflowWakeSchedule(ctx context.Context, id string, scheduledAt time.Time, knownResetAt *time.Time, lastError string, at time.Time) (bool, error)
	CancelWorkflowWakeSchedule(ctx context.Context, id string, at time.Time) (bool, error)
	CancelAllWorkflowWakeSchedulesByRun(ctx context.Context, runID string, at time.Time) (int64, error)
	ListPendingWorkflowWakeSchedulesByRun(ctx context.Context, runID string) ([]store.WorkflowWakeSchedule, error)
}

// claimLeaseDuration bounds how long a claimed-but-not-yet-completed wake is
// trusted to still be in flight before the next poll treats it as
// abandoned (the claimant crashed) and re-claims it. Generous relative to
// the daemon's own poll tick so a normal in-flight ContinueRun call is never
// raced by a second poller.
const claimLeaseDuration = 5 * time.Minute

// Config wraps the effective domain.WakePolicy plus an optional seeded
// rand.Rand for deterministic jitter in tests.
type Config struct {
	Policy domain.WakePolicy
	Rand   *rand.Rand
}

// Scheduler is Checkpoint 8N's durable wake-up scheduler.
type Scheduler struct {
	store Store
	clock func() time.Time
	newID func() string
	cfg   Config
	rng   *rand.Rand
}

// New constructs a Scheduler. clock and newID are injectable for
// deterministic tests, mirroring workflow.Coordinator's own convention.
func New(st Store, clock func() time.Time, newID func() string, cfg Config) *Scheduler {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	rng := cfg.Rand
	if rng == nil {
		rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return &Scheduler{store: st, clock: clock, newID: newID, cfg: cfg, rng: rng}
}

// wakeScopeKey derives the stepScope portion of an idempotency key: the
// step id if known, else a role-ish string derived from reason so a
// step-less wait (e.g. question_resolver_capacity, which is scoped to a
// run/question rather than a workflow step) still gets a stable key.
func wakeScopeKey(stepID *domain.WorkflowStepID, reason Reason) string {
	if stepID != nil && *stepID != "" {
		return string(*stepID)
	}
	return "role:" + string(reason)
}

// idempotencyKey builds the deterministic key a wake for this exact scope
// always maps to: "wfwake:" + runID + ":" + stepScope + ":" + reason.
func idempotencyKey(runID domain.WorkflowRunID, stepID *domain.WorkflowStepID, reason Reason) string {
	return "wfwake:" + string(runID) + ":" + wakeScopeKey(stepID, reason) + ":" + string(reason)
}

// Schedule idempotently upserts a wake for (runID, stepID, reason): if a
// wake for this exact scope is already pending/claimed, it is rescheduled
// in place (attempt_count increments) rather than duplicated; otherwise a
// new row is created. priorAttempts is accepted for API-compatibility with
// callers that already know an attempt count, but the store layer's own
// upsert is the source of truth for AttemptCount (see
// store.UpsertWorkflowWakeSchedule's doc comment) — it is not used to
// compute the returned Schedule's AttemptCount, avoiding a race between two
// callers each independently guessing the next attempt number.
func (s *Scheduler) Schedule(ctx context.Context, runID domain.WorkflowRunID, stepID *domain.WorkflowStepID, reason Reason, knownResetAt *time.Time, priorAttempts int) (Schedule, error) {
	if runID == "" {
		return Schedule{}, errors.New("wake: workflow run id is required")
	}
	if reason == "" {
		return Schedule{}, errors.New("wake: reason is required")
	}
	now := s.clock()
	scheduledAt, _ := computeScheduledAt(now, knownResetAt, priorAttempts, s.effectivePolicy(), s.rng)

	var stepIDStr *string
	if stepID != nil {
		v := string(*stepID)
		stepIDStr = &v
	}

	row, err := s.store.UpsertWorkflowWakeSchedule(ctx, store.WorkflowWakeSchedule{
		ID:             "wfwk-" + s.newID(),
		WorkflowRunID:  string(runID),
		WorkflowStepID: stepIDStr,
		Reason:         string(reason),
		IdempotencyKey: idempotencyKey(runID, stepID, reason),
		ScheduledAt:    scheduledAt,
		KnownResetAt:   knownResetAt,
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	if err != nil {
		return Schedule{}, fmt.Errorf("schedule wake for run %s: %w", runID, err)
	}
	return fromStoreRow(row), nil
}

// ClaimDue claims up to limit due wakes (pending and past scheduled_at, or
// claimed with an expired lease) for claimant, and returns the ones this
// call actually won the CAS race for. Never returns more than limit.
func (s *Scheduler) ClaimDue(ctx context.Context, claimant string, limit int) ([]Schedule, error) {
	now := s.clock()
	leaseCutoff := now.Add(-claimLeaseDuration)
	due, err := s.store.ListDueWorkflowWakeSchedules(ctx, now, leaseCutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("list due wake schedules: %w", err)
	}
	claimed := make([]Schedule, 0, len(due))
	for _, row := range due {
		ok, err := s.store.ClaimWorkflowWakeSchedule(ctx, row.ID, row.Status, claimant, now)
		if err != nil {
			return nil, fmt.Errorf("claim wake schedule %s: %w", row.ID, err)
		}
		if !ok {
			// Lost the CAS race to a concurrent claimant — not an error,
			// just skip it.
			continue
		}
		row.Status = "claimed"
		claimed = append(claimed, fromStoreRow(row))
		if limit > 0 && len(claimed) >= limit {
			break
		}
	}
	return claimed, nil
}

// Complete marks a claimed wake completed.
func (s *Scheduler) Complete(ctx context.Context, id string) error {
	_, err := s.store.CompleteWorkflowWakeSchedule(ctx, id, s.clock())
	if err != nil {
		return fmt.Errorf("complete wake schedule %s: %w", id, err)
	}
	return nil
}

// FailResult reports what Fail did with a wake after a firing attempt
// errored: either it was rescheduled with backoff, or it was cancelled
// because MaxAttempts was exhausted.
type FailResult struct {
	Rescheduled     bool
	BudgetExhausted bool
	NextScheduledAt time.Time
	AttemptCount    int64
}

// Fail records a firing failure for a wake and either reschedules it with
// backoff (recomputed from the wake's own AttemptCount) or, past
// cfg.Policy.MaxAttempts, cancels it instead of rescheduling forever.
func (s *Scheduler) Fail(ctx context.Context, sch Schedule, errMsg string) (FailResult, error) {
	now := s.clock()
	policy := s.effectivePolicy()
	nextAttempt := int(sch.AttemptCount)
	if nextAttempt >= policy.MaxAttempts {
		if _, err := s.store.CancelWorkflowWakeSchedule(ctx, sch.ID, now); err != nil {
			return FailResult{}, fmt.Errorf("cancel exhausted wake schedule %s: %w", sch.ID, err)
		}
		return FailResult{BudgetExhausted: true, AttemptCount: sch.AttemptCount}, nil
	}
	scheduledAt, _ := computeScheduledAt(now, sch.KnownResetAt, nextAttempt, policy, s.rng)
	if _, err := s.store.RescheduleWorkflowWakeSchedule(ctx, sch.ID, scheduledAt, sch.KnownResetAt, errMsg, now); err != nil {
		return FailResult{}, fmt.Errorf("reschedule failed wake schedule %s: %w", sch.ID, err)
	}
	return FailResult{Rescheduled: true, NextScheduledAt: scheduledAt, AttemptCount: sch.AttemptCount + 1}, nil
}

// CancelAllForRun cancels every pending/claimed wake for a run (CancelRun's
// cascade). Returns the number of wakes cancelled.
func (s *Scheduler) CancelAllForRun(ctx context.Context, runID domain.WorkflowRunID) (int, error) {
	n, err := s.store.CancelAllWorkflowWakeSchedulesByRun(ctx, string(runID), s.clock())
	if err != nil {
		return 0, fmt.Errorf("cancel all wake schedules for run %s: %w", runID, err)
	}
	return int(n), nil
}

// NextForRun returns the soonest-scheduled still-open (pending or claimed)
// wake for a run, or nil if none. Read-time source for
// RunDetail.NextWakeAt/WaitReason.
func (s *Scheduler) NextForRun(ctx context.Context, runID domain.WorkflowRunID) (*Schedule, error) {
	rows, err := s.store.ListPendingWorkflowWakeSchedulesByRun(ctx, string(runID))
	if err != nil {
		return nil, fmt.Errorf("list pending wake schedules for run %s: %w", runID, err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	best := rows[0]
	for _, r := range rows[1:] {
		if r.ScheduledAt.Before(best.ScheduledAt) {
			best = r
		}
	}
	out := fromStoreRow(best)
	return &out, nil
}

func (s *Scheduler) effectivePolicy() domain.WakePolicy {
	if s.cfg.Policy.Version != "" {
		return s.cfg.Policy
	}
	return domain.DefaultWakePolicy()
}

// computeScheduledAt is the pure backoff/known-reset computation at the
// heart of this package, kept standalone and directly unit-testable.
//
// Known-reset branch: if knownResetAt is non-nil and still in the future,
// the wake is scheduled at knownResetAt plus a small fixed safety delay —
// never before the provider's own reported reset. isKnownReset=true.
//
// Unknown-reset branch (knownResetAt nil, or already in the past): bounded
// exponential backoff — min(InitialBackoffSeconds * BackoffMultiplier^attempt,
// MaxBackoffSeconds) — plus additive jitter in [0, JitterSeconds]. This is
// the checkpoint's only "blind poll"-shaped mechanism, and even then it is
// capped by MaxBackoffSeconds, never unconditional or the primary
// mechanism when a real reset time is known.
func computeScheduledAt(now time.Time, knownResetAt *time.Time, attempt int, cfg domain.WakePolicy, rng *rand.Rand) (scheduledAt time.Time, isKnownReset bool) {
	if knownResetAt != nil && knownResetAt.After(now) {
		safety := time.Duration(cfg.KnownResetSafetyDelaySeconds) * time.Second
		return knownResetAt.Add(safety), true
	}

	if attempt < 0 {
		attempt = 0
	}
	initial := float64(cfg.InitialBackoffSeconds)
	mult := cfg.BackoffMultiplier
	if mult <= 0 {
		mult = 1
	}
	backoffSeconds := initial * math.Pow(mult, float64(attempt))
	maxBackoff := float64(cfg.MaxBackoffSeconds)
	if maxBackoff > 0 && backoffSeconds > maxBackoff {
		backoffSeconds = maxBackoff
	}
	if backoffSeconds < 0 {
		backoffSeconds = 0
	}

	jitter := 0
	if cfg.JitterSeconds > 0 && rng != nil {
		jitter = rng.Intn(cfg.JitterSeconds + 1)
	}

	total := time.Duration(backoffSeconds)*time.Second + time.Duration(jitter)*time.Second
	return now.Add(total), false
}

func fromStoreRow(r store.WorkflowWakeSchedule) Schedule {
	var stepID *domain.WorkflowStepID
	if r.WorkflowStepID != nil {
		v := domain.WorkflowStepID(*r.WorkflowStepID)
		stepID = &v
	}
	return Schedule{
		ID:             r.ID,
		WorkflowRunID:  domain.WorkflowRunID(r.WorkflowRunID),
		WorkflowStepID: stepID,
		Reason:         Reason(r.Reason),
		Status:         Status(r.Status),
		IdempotencyKey: r.IdempotencyKey,
		ScheduledAt:    r.ScheduledAt,
		KnownResetAt:   r.KnownResetAt,
		AttemptCount:   r.AttemptCount,
		LastError:      r.LastError,
		CreatedAt:      r.CreatedAt,
		UpdatedAt:      r.UpdatedAt,
	}
}
