package workflow_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
)

// recordingWakeScheduler remembers the reason of every wake scheduled, which is
// what these tests are actually about: a heartbeat must exist, and it must
// never displace a more specific wait.
type recordingWakeScheduler struct {
	reasons []wake.Reason
	next    *wake.Schedule
}

func (f *recordingWakeScheduler) Schedule(_ context.Context, runID domain.WorkflowRunID, _ *domain.WorkflowStepID, reason wake.Reason, _ *time.Time) (wake.Schedule, error) {
	f.reasons = append(f.reasons, reason)
	return wake.Schedule{ID: "wfwk-rec", WorkflowRunID: runID, Reason: reason}, nil
}

func (f *recordingWakeScheduler) WakeNow(_ context.Context, runID domain.WorkflowRunID, _ *domain.WorkflowStepID, reason wake.Reason) (wake.Schedule, error) {
	f.reasons = append(f.reasons, reason)
	return wake.Schedule{ID: "wfwk-rec", WorkflowRunID: runID, Reason: reason}, nil
}

func (f *recordingWakeScheduler) CancelAllForRun(context.Context, domain.WorkflowRunID) (int, error) {
	return 0, nil
}

func (f *recordingWakeScheduler) NextForRun(context.Context, domain.WorkflowRunID) (*wake.Schedule, error) {
	return f.next, nil
}

func (f *recordingWakeScheduler) scheduled(reason wake.Reason) bool {
	for _, r := range f.reasons {
		if r == reason {
			return true
		}
	}
	return false
}

func newHeartbeatCoordinator(sched workflowcore.WakeScheduler) (*workflowcore.Coordinator, *fakeStore, *fakeClock) {
	store := newFakeStore()
	clk := &fakeClock{t: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store:         store,
		WakeScheduler: sched,
		Clock:         clk.Now,
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("hb%d", idSeq)
		},
	})
	return c, store, clk
}

// markAutonomous rewrites a run's frozen policy snapshot so it reads as an
// autonomous run, the same durable fact ApplyExecutionPolicySnapshot writes in
// production.
func markAutonomous(t *testing.T, store *fakeStore, runID string, now time.Time) {
	t.Helper()
	policy := domain.DefaultWorkflowPolicy()
	policy.Execution = domain.ExecutionPolicySnapshot{
		Version:        domain.UserExecutionPolicyVersion,
		AutonomousMode: true,
	}
	snapshot, err := json.Marshal(policy)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	if _, err := store.UpdateWorkflowRunPolicySnapshot(context.Background(), runID, string(snapshot), now); err != nil {
		t.Fatalf("UpdateWorkflowRunPolicySnapshot: %v", err)
	}
}

// The stall this checkpoint exists to fix: before it, only a MASTER run ever
// left behind a headless-progression wake. A standalone autonomous run advanced
// only while the renderer happened to be polling its detail page, so closing
// that page silently froze the run until the next daemon restart — the
// "it says Inactive and never moves" report.
func TestStandaloneAutonomousRunSchedulesHeadlessHeartbeat(t *testing.T) {
	sched := &recordingWakeScheduler{}
	c, store, clk := newHeartbeatCoordinator(sched)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	markAutonomous(t, store, created.Run.ID, clk.Now())

	if _, err := c.GetRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if !sched.scheduled(wake.ReasonAutonomousProgress) {
		t.Fatalf("no autonomous_progress wake scheduled; a standalone autonomous run would only advance while a UI polls it")
	}
}

// A manual run must stay manual: no heartbeat, no self-driving.
func TestManualRunSchedulesNoHeartbeat(t *testing.T) {
	sched := &recordingWakeScheduler{}
	c, _, _ := newHeartbeatCoordinator(sched)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := c.GetRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if sched.scheduled(wake.ReasonAutonomousProgress) {
		t.Fatalf("manual run scheduled an autonomous heartbeat")
	}
}

// The heartbeat must never be added on top of a more specific open wake:
// NextForRun surfaces the soonest one, and a short heartbeat would hide
// "waiting for reviewer capacity, next retry at …" behind a generic label.
func TestHeartbeatDoesNotDisplaceAMoreSpecificWake(t *testing.T) {
	sched := &recordingWakeScheduler{
		next: &wake.Schedule{ID: "wfwk-cap", Reason: wake.ReasonReviewerCapacity},
	}
	c, store, clk := newHeartbeatCoordinator(sched)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	markAutonomous(t, store, created.Run.ID, clk.Now())

	if _, err := c.GetRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if sched.scheduled(wake.ReasonAutonomousProgress) {
		t.Fatalf("heartbeat scheduled on top of an open reviewer_capacity wake, hiding the real wait reason")
	}
}
