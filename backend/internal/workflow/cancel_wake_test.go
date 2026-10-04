package workflow_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
)

// fakeWakeScheduler is a minimal hand-rolled double for workflowcore.WakeScheduler,
// tracking only what CancelRun's cascade needs to assert against: which runs
// had CancelAllForRun called on them.
type fakeWakeScheduler struct {
	scheduled       map[string]bool // runID -> has an open (uncancelled) wake
	cancelledRunIDs []string
	// wokenNow records the runs told to resume immediately, which is how the
	// branch-queue tests assert a released branch actually wakes its queue.
	wokenNow []string
	// reasons records every scheduled wake's reason, so a test can assert that
	// a stop AO says it will retry actually scheduled the retry.
	reasons []wake.Reason
}

func newFakeWakeScheduler() *fakeWakeScheduler {
	return &fakeWakeScheduler{scheduled: map[string]bool{}}
}

func (f *fakeWakeScheduler) Schedule(_ context.Context, runID domain.WorkflowRunID, _ *domain.WorkflowStepID, reason wake.Reason, _ *time.Time) (wake.Schedule, error) {
	f.scheduled[string(runID)] = true
	f.reasons = append(f.reasons, reason)
	return wake.Schedule{ID: "wfwk-fake", WorkflowRunID: runID}, nil
}

func (f *fakeWakeScheduler) WakeNow(_ context.Context, runID domain.WorkflowRunID, _ *domain.WorkflowStepID, _ wake.Reason) (wake.Schedule, error) {
	f.scheduled[string(runID)] = true
	f.wokenNow = append(f.wokenNow, string(runID))
	return wake.Schedule{ID: "wfwk-fake", WorkflowRunID: runID}, nil
}

func (f *fakeWakeScheduler) CancelAllForRun(_ context.Context, runID domain.WorkflowRunID) (int, error) {
	if !f.scheduled[string(runID)] {
		return 0, nil
	}
	f.scheduled[string(runID)] = false
	f.cancelledRunIDs = append(f.cancelledRunIDs, string(runID))
	return 1, nil
}

func (f *fakeWakeScheduler) NextForRun(_ context.Context, runID domain.WorkflowRunID) (*wake.Schedule, error) {
	if !f.scheduled[string(runID)] {
		return nil, nil
	}
	sch := wake.Schedule{ID: "wfwk-fake", WorkflowRunID: runID}
	return &sch, nil
}

// TestCancelRunCancelsOpenWakeSchedule proves Checkpoint 8N.1's fix: a run
// with a pending durable wake must have that wake cancelled as part of
// CancelRun's cascade, otherwise the daemon poller would later fire it and
// try to redispatch a cancelled run (test-matrix item 10/25).
func TestCancelRunCancelsOpenWakeSchedule(t *testing.T) {
	store := newFakeStore()
	wakeSched := newFakeWakeScheduler()
	clock := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store:         store,
		WakeScheduler: wakeSched,
		Clock:         func() time.Time { return clock },
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("id%d", idSeq)
		},
	})

	ctx := context.Background()
	detail, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := detail.Run.ID

	if _, err := wakeSched.Schedule(ctx, domain.WorkflowRunID(runID), nil, wake.ReasonWorkerCapacity, nil); err != nil {
		t.Fatalf("seed wake: %v", err)
	}
	next, err := wakeSched.NextForRun(ctx, domain.WorkflowRunID(runID))
	if err != nil || next == nil {
		t.Fatalf("expected a seeded open wake before cancel, got %+v err=%v", next, err)
	}

	if _, err := c.CancelRun(ctx, runID); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}

	if len(wakeSched.cancelledRunIDs) != 1 || wakeSched.cancelledRunIDs[0] != runID {
		t.Fatalf("expected CancelAllForRun called once for %q, got %v", runID, wakeSched.cancelledRunIDs)
	}
	next, err = wakeSched.NextForRun(ctx, domain.WorkflowRunID(runID))
	if err != nil {
		t.Fatalf("NextForRun after cancel: %v", err)
	}
	if next != nil {
		t.Fatalf("expected no open wake left after CancelRun, got %+v", next)
	}
}
