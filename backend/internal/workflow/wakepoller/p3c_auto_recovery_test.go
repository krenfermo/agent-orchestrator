package wakepoller_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wakepoller"
)

// p3c_auto_recovery_test.go — P3-C §16: an automatic recovery is not a resume,
// and the poller must not treat it as one.
//
// The distinction is the whole point. A run parked on a repairable condition is
// parked precisely because a resume cannot clear it, so routing its wake to
// ContinueRun would do nothing, report nothing, and leave the wake looking like
// it had fired successfully — which is how "automatic repair" stayed a button
// somebody had to press.

// dispatchingResumer is a fakeResumer that also implements
// wakepoller.RecoveryDispatcher.
type dispatchingResumer struct {
	fakeResumer
	dispatched []string
	err        error
}

func (d *dispatchingResumer) DispatchAutomaticRecovery(_ context.Context, runID string) (workflowcore.AutomaticRecoveryOutcome, error) {
	d.dispatched = append(d.dispatched, runID)
	if d.err != nil {
		return workflowcore.AutomaticRecoveryOutcome{}, d.err
	}
	return workflowcore.AutomaticRecoveryOutcome{
		RunID: runID, Action: workflowcore.AutoActionLaunchRepair, Dispatched: true,
	}, nil
}

func TestAutoRecoveryWakeGoesToTheDispatcherNotToContinueRun(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	clk := &fakeClock{t: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	sched := wake.New(store, clk.now, newIDSeq(), wake.Config{})
	resumer := &dispatchingResumer{}
	poller := wakepoller.New(sched, resumer, wakepoller.Config{})
	ctx := context.Background()

	if _, err := sched.Schedule(ctx, "wf-1", nil, wake.ReasonAutoRecovery, nil); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	clk.advance(2 * time.Hour)
	if _, err := poller.RunDueOnce(ctx); err != nil {
		t.Fatalf("RunDueOnce: %v", err)
	}
	if len(resumer.dispatched) != 1 || resumer.dispatched[0] != "wf-1" {
		t.Fatalf("dispatcher calls = %v, want exactly [wf-1]", resumer.dispatched)
	}
	if len(resumer.calls) != 0 {
		t.Fatalf("an auto-recovery wake was resumed instead of dispatched: %v", resumer.calls)
	}
}

// Every other wake keeps the reason-agnostic resume path, unchanged.
func TestOtherWakesStillGoToContinueRun(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	clk := &fakeClock{t: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	sched := wake.New(store, clk.now, newIDSeq(), wake.Config{})
	resumer := &dispatchingResumer{}
	poller := wakepoller.New(sched, resumer, wakepoller.Config{})
	ctx := context.Background()

	if _, err := sched.Schedule(ctx, "wf-1", nil, wake.ReasonBranchLock, nil); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	clk.advance(2 * time.Hour)
	if _, err := poller.RunDueOnce(ctx); err != nil {
		t.Fatalf("RunDueOnce: %v", err)
	}
	if len(resumer.calls) != 1 {
		t.Fatalf("ContinueRun calls = %v, want exactly one", resumer.calls)
	}
	if len(resumer.dispatched) != 0 {
		t.Fatalf("a branch-lock wake was routed to the recovery dispatcher: %v", resumer.dispatched)
	}
}

// A Resumer with no dispatcher degrades to the pre-P3-C behaviour rather than
// failing the wake: nothing happens, which is what happened before.
func TestAutoRecoveryWakeDegradesWhenNoDispatcherIsWired(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	clk := &fakeClock{t: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	sched := wake.New(store, clk.now, newIDSeq(), wake.Config{})
	resumer := &fakeResumer{}
	poller := wakepoller.New(sched, resumer, wakepoller.Config{})
	ctx := context.Background()

	if _, err := sched.Schedule(ctx, "wf-1", nil, wake.ReasonAutoRecovery, nil); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	clk.advance(2 * time.Hour)
	if _, err := poller.RunDueOnce(ctx); err != nil {
		t.Fatalf("RunDueOnce: %v", err)
	}
	if len(resumer.calls) != 1 {
		t.Fatalf("ContinueRun calls = %v, want the degraded single resume", resumer.calls)
	}
}

// §18/§32: an auto-recovery wake that runs out of retries must not park the run
// as a CAPACITY exhaustion. The run is already parked on its real stop; telling
// a person their provider is at capacity would be a fabricated explanation for
// something that went wrong inside AO's own dispatcher.
func TestAnExhaustedAutoRecoveryWakeIsNotReportedAsCapacityExhaustion(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	clk := &fakeClock{t: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	sched := wake.New(store, clk.now, newIDSeq(), wake.Config{})
	resumer := &dispatchingResumer{err: errors.New("the dispatcher is broken")}
	poller := wakepoller.New(sched, resumer, wakepoller.Config{})
	ctx := context.Background()

	if _, err := sched.Schedule(ctx, "wf-1", nil, wake.ReasonAutoRecovery, nil); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	// Drive well past the wake budget.
	for i := 0; i < 12; i++ {
		clk.advance(2 * time.Hour)
		if _, err := poller.RunDueOnce(ctx); err != nil {
			t.Fatalf("RunDueOnce: %v", err)
		}
	}
	if len(resumer.exhaustedCalls) != 0 {
		t.Fatalf("an auto-recovery wake parked the run as a capacity exhaustion: %v (reasons %v)",
			resumer.exhaustedCalls, resumer.exhaustedReasons)
	}
	if len(resumer.dispatched) == 0 {
		t.Fatal("the dispatcher was never called, so the budget was never exercised")
	}
}
