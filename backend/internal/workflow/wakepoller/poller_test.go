package wakepoller_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wakepoller"
)

// fakeClock is a manually-advanced clock shared between the wake.Scheduler
// under test and the assertions driving it, so every test in this file
// drives real time forward explicitly instead of sleeping.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time    { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// fakeResumer is a scripted double for wakepoller.Resumer: each test controls
// exactly what ContinueRun returns per call via next, and records every runID
// it was invoked with so tests can assert "exactly once" claims.
type fakeResumer struct {
	next  func(callN int) error
	calls []string
}

func (f *fakeResumer) ContinueRun(_ context.Context, runID string) (workflowcore.RunDetail, error) {
	f.calls = append(f.calls, runID)
	var err error
	if f.next != nil {
		err = f.next(len(f.calls))
	}
	return workflowcore.RunDetail{}, err
}

func newIDSeq() func() string {
	n := 0
	return func() string {
		n++
		return "seq" + strconv.Itoa(n)
	}
}

func TestPoller_KnownReset_NoEarlyClaim_ExactDueClaim(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	sched := wake.New(store, clk.now, newIDSeq(), wake.Config{})
	resumer := &fakeResumer{}
	poller := wakepoller.New(sched, resumer, wakepoller.Config{})
	ctx := context.Background()

	reset := clk.t.Add(60 * time.Minute)
	if _, err := sched.Schedule(ctx, "wf-1", nil, wake.ReasonWorkerCapacity, &reset); err != nil {
		t.Fatalf("schedule: %v", err)
	}

	// Just before the reset + safety delay: zero claims.
	clk.advance(59*time.Minute + 59*time.Second)
	n, err := poller.RunDueOnce(ctx)
	if err != nil {
		t.Fatalf("RunDueOnce before due: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 claims before due, got %d", n)
	}
	if len(resumer.calls) != 0 {
		t.Fatalf("resumer must not be invoked before due, got %v", resumer.calls)
	}

	// Past the reset + default 30s safety delay: exactly one claim, exactly
	// one resume.
	clk.advance(2 * time.Minute)
	n, err = poller.RunDueOnce(ctx)
	if err != nil {
		t.Fatalf("RunDueOnce at due: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 claim once due, got %d", n)
	}
	if len(resumer.calls) != 1 || resumer.calls[0] != "wf-1" {
		t.Fatalf("expected exactly one resume of wf-1, got %v", resumer.calls)
	}
}

func TestPoller_UnknownResetBackoffGrows_ThenSucceeds(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	sched := wake.New(store, clk.now, newIDSeq(), wake.Config{})
	errCapacity := errors.New("provider still at capacity")
	callCount := 0
	resumer := &fakeResumer{next: func(int) error {
		callCount++
		if callCount <= 2 {
			return errCapacity
		}
		return nil
	}}
	poller := wakepoller.New(sched, resumer, wakepoller.Config{})
	ctx := context.Background()

	if _, err := sched.Schedule(ctx, "wf-1", nil, wake.ReasonWorkerCapacity, nil); err != nil {
		t.Fatalf("schedule: %v", err)
	}

	var lastDelay time.Duration
	for cycle := 0; cycle < 2; cycle++ {
		next, err := sched.NextForRun(ctx, "wf-1")
		if err != nil || next == nil {
			t.Fatalf("cycle %d: expected an open wake, got %+v err=%v", cycle, next, err)
		}
		delay := next.ScheduledAt.Sub(clk.now())
		if cycle > 0 && delay <= lastDelay {
			t.Fatalf("cycle %d: backoff did not grow: prev=%v next=%v", cycle, lastDelay, delay)
		}
		lastDelay = delay

		clk.advance(delay + time.Second)
		n, err := poller.RunDueOnce(ctx)
		if err != nil {
			t.Fatalf("cycle %d: RunDueOnce: %v", cycle, err)
		}
		if n != 1 {
			t.Fatalf("cycle %d: expected exactly 1 claim, got %d", cycle, n)
		}
	}

	// Third cycle: resumer now succeeds, wake must be completed with no
	// further reschedule.
	next, err := sched.NextForRun(ctx, "wf-1")
	if err != nil || next == nil {
		t.Fatalf("expected an open wake before final cycle, got %+v err=%v", next, err)
	}
	clk.advance(next.ScheduledAt.Sub(clk.now()) + time.Second)
	if _, err := poller.RunDueOnce(ctx); err != nil {
		t.Fatalf("final RunDueOnce: %v", err)
	}
	if callCount != 3 {
		t.Fatalf("expected exactly 3 resume attempts, got %d", callCount)
	}
	final, err := sched.NextForRun(ctx, "wf-1")
	if err != nil {
		t.Fatalf("final NextForRun: %v", err)
	}
	if final != nil {
		t.Fatalf("expected wake completed (no open wake left), got %+v", final)
	}

	// No further claims even after advancing far into the future.
	clk.advance(24 * time.Hour)
	n, err := poller.RunDueOnce(ctx)
	if err != nil {
		t.Fatalf("RunDueOnce after completion: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 claims after completion, got %d", n)
	}
}

func TestPoller_RestartLeaseRecovery(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	ctx := context.Background()

	// Instance "A": schedules and claims a wake but crashes before firing it
	// (simulated by simply never calling ContinueRun/Complete/Fail on it —
	// same effect as a daemon dying mid-cycle).
	schedA := wake.New(store, clk.now, newIDSeq(), wake.Config{})
	if _, err := schedA.Schedule(ctx, "wf-1", nil, wake.ReasonReviewerCapacity, nil); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	next, err := schedA.NextForRun(ctx, "wf-1")
	if err != nil || next == nil {
		t.Fatalf("expected open wake, got %+v err=%v", next, err)
	}
	clk.advance(next.ScheduledAt.Sub(clk.now()) + time.Second)
	claimed, err := schedA.ClaimDue(ctx, "instance-a", 10)
	if err != nil {
		t.Fatalf("instance A claim: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("expected instance A to claim exactly 1 wake, got %d", len(claimed))
	}
	// Instance A never completes/fails it — it's gone.

	// Instance "B": a fresh Scheduler/Poller pair over the same store (a
	// restarted daemon has no in-memory carryover, only what is durable).
	// Before the claim lease expires, B must not re-claim it.
	schedB := wake.New(store, clk.now, newIDSeq(), wake.Config{})
	resumerB := &fakeResumer{}
	pollerB := wakepoller.New(schedB, resumerB, wakepoller.Config{})

	clk.advance(1 * time.Minute)
	n, err := pollerB.RunDueOnce(ctx)
	if err != nil {
		t.Fatalf("instance B early RunDueOnce: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected instance B to not reclaim while A's lease is fresh, got %d", n)
	}

	// Past the claim lease: instance B reclaims it and fires exactly once.
	clk.advance(6 * time.Minute)
	n, err = pollerB.RunDueOnce(ctx)
	if err != nil {
		t.Fatalf("instance B RunDueOnce after lease expiry: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected instance B to reclaim exactly 1 wake after lease expiry, got %d", n)
	}
	if len(resumerB.calls) != 1 || resumerB.calls[0] != "wf-1" {
		t.Fatalf("expected exactly one resume of wf-1 by instance B, got %v", resumerB.calls)
	}

	// No zombie: a further cycle claims nothing more.
	n, err = pollerB.RunDueOnce(ctx)
	if err != nil {
		t.Fatalf("instance B final RunDueOnce: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected no further claims, got %d", n)
	}
}

func TestPoller_CancelPreventsFutureClaim(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	sched := wake.New(store, clk.now, newIDSeq(), wake.Config{})
	resumer := &fakeResumer{}
	poller := wakepoller.New(sched, resumer, wakepoller.Config{})
	ctx := context.Background()

	if _, err := sched.Schedule(ctx, "wf-1", nil, wake.ReasonWorkerCapacity, nil); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if _, err := sched.CancelAllForRun(ctx, "wf-1"); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	clk.advance(24 * time.Hour)
	n, err := poller.RunDueOnce(ctx)
	if err != nil {
		t.Fatalf("RunDueOnce after cancel: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 claims for a cancelled wake, got %d", n)
	}
	if len(resumer.calls) != 0 {
		t.Fatalf("resumer must never be invoked for a cancelled wake, got %v", resumer.calls)
	}
}

func TestPoller_TerminalRunCompletesNoReschedule(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	sched := wake.New(store, clk.now, newIDSeq(), wake.Config{})
	resumer := &fakeResumer{next: func(int) error { return workflowcore.ErrAlreadyTerminal }}
	poller := wakepoller.New(sched, resumer, wakepoller.Config{})
	ctx := context.Background()

	if _, err := sched.Schedule(ctx, "wf-1", nil, wake.ReasonWorkerCapacity, nil); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	next, _ := sched.NextForRun(ctx, "wf-1")
	clk.advance(next.ScheduledAt.Sub(clk.now()) + time.Second)

	n, err := poller.RunDueOnce(ctx)
	if err != nil {
		t.Fatalf("RunDueOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 claim, got %d", n)
	}
	if len(resumer.calls) != 1 {
		t.Fatalf("expected exactly 1 resume attempt, got %d", len(resumer.calls))
	}

	final, err := sched.NextForRun(ctx, "wf-1")
	if err != nil {
		t.Fatalf("NextForRun: %v", err)
	}
	if final != nil {
		t.Fatalf("expected the wake completed (terminal run), not rescheduled, got %+v", final)
	}

	clk.advance(24 * time.Hour)
	n, err = poller.RunDueOnce(ctx)
	if err != nil {
		t.Fatalf("RunDueOnce after terminal completion: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected no further claims for a terminal run, got %d", n)
	}
}

// TestPoller_SequentialCallsNeverDoubleClaim proves the CAS-backed ClaimDue a
// second poller cycle (or a second poller instance racing the same due row)
// can never re-claim a wake that a prior cycle already claimed and is still
// within its lease.
func TestPoller_SequentialCallsNeverDoubleClaim(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	sched := wake.New(store, clk.now, newIDSeq(), wake.Config{})
	ctx := context.Background()

	if _, err := sched.Schedule(ctx, "wf-1", nil, wake.ReasonWorkerCapacity, nil); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	next, _ := sched.NextForRun(ctx, "wf-1")
	clk.advance(next.ScheduledAt.Sub(clk.now()) + time.Second)

	// "Instance A" claims the row directly via the scheduler (mid-fire,
	// before it would call Complete/Fail) to simulate instance B's poller
	// cycle racing an in-flight claim rather than an idle one.
	claimedA, err := sched.ClaimDue(ctx, "instance-a", 10)
	if err != nil {
		t.Fatalf("instance A claim: %v", err)
	}
	if len(claimedA) != 1 {
		t.Fatalf("expected instance A to claim exactly 1, got %d", len(claimedA))
	}

	pollerB := wakepoller.New(sched, &fakeResumer{}, wakepoller.Config{})
	nB, err := pollerB.RunDueOnce(ctx)
	if err != nil {
		t.Fatalf("instance B RunDueOnce: %v", err)
	}
	if nB != 0 {
		t.Fatalf("expected instance B to claim 0 while A's lease is fresh, got %d", nB)
	}
}
