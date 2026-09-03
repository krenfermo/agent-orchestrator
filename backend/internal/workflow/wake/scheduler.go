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
	"sort"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// Reason is the fixed vocabulary of why a wake was scheduled, mirroring the
// workflow_wake_schedules.reason CHECK enum (migration 0106).
type Reason string

// The reason vocabulary. Keep in sync with the migration's CHECK enum: a
// reason this package can write but the schema rejects is a wake that silently
// never gets scheduled.
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
	// ReasonAutonomousProgress is Checkpoint 8P-D's headless progression
	// heartbeat for a master/objective run under AutonomousMode: scheduled
	// (a) once as a kickoff right after the run's frozen execution policy
	// snapshot is applied, if the plan hasn't been generated yet, and
	// (b) re-scheduled by reconcileMasterTasks after every reconcile pass
	// while the run is non-terminal, not NeedsAttention, and not blocked on
	// a HUMAN_REQUIRED question — so the daemon poller alone keeps driving
	// planning/approval/task-dispatch/review/fix/verify/integration forward
	// without any browser GET.
	ReasonAutonomousProgress Reason = "autonomous_progress"
	// ReasonBranchLock is Checkpoint 8P-E.11's direct-branch execution wait:
	// the run is ready to work but another run owns its repository+branch
	// pair. Unlike every capacity reason above it, the blocker is local and
	// bounded — the owning run will end — so the wake exists to retry the
	// acquisition rather than to probe an external provider. known_reset_at
	// is always nil for it: a lock has no announced release time, and
	// inventing one would be exactly the fabricated timestamp 0106 forbids.
	ReasonBranchLock Reason = "branch_lock"
	// ReasonAutoRecovery is P3-C's automatic-recovery wake: a run has parked on
	// a condition its own frozen repair policy authorizes AO to repair without
	// asking, and this wake is what makes that actually happen while the daemon
	// stays up.
	//
	// It is deliberately a wake rather than an inline launch at the stop site.
	// A stop is sometimes recorded from a read path, and starting a repair
	// agent from there would give a GET a side effect; and a durable wake
	// survives a daemon that dies between the stop and the repair. The poller
	// routes it to Coordinator.DispatchAutomaticRecovery rather than to
	// ContinueRun, because the remedy is not a resume (see wakepoller).
	//
	// known_reset_at is always nil for it: nothing external announces when a
	// repairable condition becomes repairable, and it already is.
	ReasonAutoRecovery Reason = "auto_recovery"
)

// Status is the fixed vocabulary of a wake schedule's lifecycle state,
// mirroring the workflow_wake_schedules.status CHECK enum.
type Status string

// The wake lifecycle. Claimed is the single-flight slot -- exactly one poller
// may move a wake out of pending -- and completed and cancelled are both
// terminal, distinguishing a wake that fired from one that was withdrawn.
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
	GetWorkflowWakeScheduleByIdempotencyKey(ctx context.Context, idempotencyKey string) (store.WorkflowWakeSchedule, bool, error)
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
		// Retry jitter only: this spreads competing wakes apart so they do not
		// all fire on the same tick. Nothing here is a secret, a token or a
		// key, so a cryptographic source would buy nothing.
		//nolint:gosec // G404: jitter for retry spreading, not security.
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
// new row is created. The backoff delay is computed from the existing row's
// own current AttemptCount (read fresh from the store by idempotency key),
// never from a caller-supplied guess — a caller re-parking on the same scope
// has no reliable way to know how many times this exact wake has already
// backed off, and trusting a wrong guess (e.g. always 0) would silently
// prevent unknown-reset backoff from ever growing. A brand-new scope (no
// existing row) starts at attempt 0, same as before.
func (s *Scheduler) Schedule(ctx context.Context, runID domain.WorkflowRunID, stepID *domain.WorkflowStepID, reason Reason, knownResetAt *time.Time) (Schedule, error) {
	if runID == "" {
		return Schedule{}, errors.New("wake: workflow run id is required")
	}
	if reason == "" {
		return Schedule{}, errors.New("wake: reason is required")
	}
	now := s.clock()
	key := idempotencyKey(runID, stepID, reason)
	attempt := 0
	if existing, ok, err := s.store.GetWorkflowWakeScheduleByIdempotencyKey(ctx, key); err != nil {
		return Schedule{}, fmt.Errorf("lookup existing wake schedule for run %s: %w", runID, err)
	} else if ok {
		// A wake that is already PENDING is already the answer to this exact
		// question, and re-parking on it must not disturb it.
		//
		// This is the second half of the wf-57f90ff2 incident. Schedule is called
		// from the routing path, which is re-entered by every ContinueRun — the
		// autonomous heartbeat, the board's own 2s poll, every master reconcile
		// pass. Each call landed on the pending/claimed branch, which increments
		// attempt_count AND overwrites scheduled_at with now+backoff. Two things
		// followed. The counter climbed without bound (the real reviewer_capacity
		// wake reached >200 attempts), and — far worse — the wake's due time was
		// pushed 30 minutes into the future more often than every 30 minutes, so
		// the row was permanently pending and never became due. The retry that
		// was supposed to re-evaluate capacity could not fire at all, which is why
		// hundreds of routing checks produced no probe.
		//
		// Leaving a pending row untouched makes attempt_count mean what its name
		// says (retries actually taken), makes the backoff real, and reduces N
		// re-parks to N-1 no writes at all.
		//
		// The CLAIMED branch still reschedules: the poller has fired that wake and
		// is inside ContinueRun right now, so a re-park during it is a genuine
		// retry and must both back off and count. See wakepoller's own
		// completion-semantics note, which depends on exactly that supersession.
		switch existing.Status {
		case string(StatusPending):
			return fromStoreRow(existing), nil
		case string(StatusClaimed):
			if !isFixedCadence(reason) {
				attempt = int(existing.AttemptCount)
			}
		}
	}
	scheduledAt, _ := computeScheduledAt(now, knownResetAt, attempt, s.effectivePolicy(), s.rng)

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
		IdempotencyKey: key,
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

// WakeNow schedules (or reschedules in place) a wake for this exact scope to
// fire on the next poll instead of after a backoff delay.
//
// Checkpoint 8P-E.13A: a run queued behind a branch lock parks with an
// exponentially backing-off branch_lock wake, because when it parked nobody
// knew when the branch would free. The moment the holder actually releases it,
// that unknown becomes a fact — and re-using Schedule there would still make
// the waiter sit out the rest of a delay of up to MaxBackoffSeconds for no
// reason. WakeNow is that fact expressed: same idempotency key, same row, same
// claim/complete lifecycle, only due immediately.
//
// It deliberately does not reset attempt_count: the store's reschedule
// increments it either way, and the count is a diagnostic of how often this
// scope has parked, not a promise about the next delay.
func (s *Scheduler) WakeNow(ctx context.Context, runID domain.WorkflowRunID, stepID *domain.WorkflowStepID, reason Reason) (Schedule, error) {
	if runID == "" {
		return Schedule{}, errors.New("wake: workflow run id is required")
	}
	if reason == "" {
		return Schedule{}, errors.New("wake: reason is required")
	}
	now := s.clock()
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
		ScheduledAt:    now,
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	if err != nil {
		return Schedule{}, fmt.Errorf("wake now for run %s: %w", runID, err)
	}
	return fromStoreRow(row), nil
}

// isFixedCadence reports whether a reason's wake is a heartbeat rather than a
// retry, and must therefore keep a constant interval instead of backing off.
//
// Checkpoint 8P-E.13 Phase 7 found this the hard way. Every wake shared the
// exponential-backoff path, including ReasonAutonomousProgress — but that wake
// does not represent "a provider rejected us, wait longer before asking
// again". It represents "come back and drive this run forward". Because the
// poller re-schedules it on every cycle while the row is still claimed, its
// attempt_count climbed once per cycle and its interval doubled with it:
// 60s, 120s, 240s … capped at MaxBackoffSeconds (30 minutes).
//
// A real master run in ~/.ao/data showed attempt_count=9 on its heartbeat, i.e.
// an autonomous objective advancing roughly one step per half hour. Nothing
// looked broken — the run was waiting, the wake was pending, every row was
// honest — it was simply too slow to ever finish. A heartbeat that decays is
// indistinguishable from a stalled workflow, which is exactly the report this
// checkpoint exists to answer.
//
// Backoff still applies to this reason through Fail (a wake whose firing
// actually errored), which is the case backoff is genuinely for.
func isFixedCadence(reason Reason) bool {
	return reason == ReasonAutonomousProgress
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

// PendingForRun returns every still-open (pending or claimed) wake for a run,
// soonest-scheduled first.
//
// NextForRun answers "when does this run wake next", which is all a live run
// detail needs. Post-run evidence collection asks the opposite question — which
// wakes did this execution leave behind when it finished — and that needs the
// whole set, including the ones scheduled far out and the ones that are already
// overdue.
func (s *Scheduler) PendingForRun(ctx context.Context, runID domain.WorkflowRunID) ([]Schedule, error) {
	rows, err := s.store.ListPendingWorkflowWakeSchedulesByRun(ctx, string(runID))
	if err != nil {
		return nil, fmt.Errorf("list pending wake schedules for run %s: %w", runID, err)
	}
	out := make([]Schedule, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromStoreRow(r))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ScheduledAt.Before(out[j].ScheduledAt) })
	return out, nil
}

// NextForRun returns the soonest-scheduled still-open (pending or claimed)
// wake for a run, or nil if none. Read-time source for
// RunDetail.NextWakeAt/WaitReason.
func (s *Scheduler) NextForRun(ctx context.Context, runID domain.WorkflowRunID) (*Schedule, error) {
	rows, err := s.PendingForRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := rows[0]
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
