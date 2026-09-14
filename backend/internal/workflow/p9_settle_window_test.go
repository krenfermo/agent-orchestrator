package workflow_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// Finding 5 of the independent review: a concurrent pass (a GetRun, a
// reconcile) can see a launch that is still IN FLIGHT -- dispatched command, a
// session row with its workspace -- before the launching pass confirmed it. On a
// runtime that cannot prove ownership (conpty), the decision is fail_closed, and
// taking it inside the settle window would park a fresh launch. Only an adoption
// may be concluded inside the window; after it, the stop is taken.
func TestP9_InFlightLaunchIsNotStoppedInsideTheSettleWindow(t *testing.T) {
	store := newFakeStore()
	clk := &fakeClock{t: time.Date(2026, 9, 13, 22, 0, 0, 0, time.UTC)}
	sessionFacts := newFakeSessionFacts()
	rt := newScriptedRuntimeOwnership()
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store: store, Spawner: &fakeSpawner{}, SessionFacts: sessionFacts, WorkspaceFacts: &fakeWorkspaceFacts{},
		WorkerRuntimeOwnership: rt, Clock: clk.Now, MonotonicClock: clk.Now,
		NewID: func() string { idSeq++; return fmt.Sprintf("id%d", idSeq) },
	})
	ctx := context.Background()
	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatal(err)
	}
	var workStepID string
	for _, sd := range created.Steps {
		if sd.Step.Kind == domain.WorkflowStepWork {
			workStepID = sd.Step.ID
		}
	}
	sessionFacts.put(domain.SessionRecord{
		ID: "in-flight-1", ProjectID: "proj-1", IssueID: domain.IssueID("workflow-step:" + workStepID),
		Metadata:  domain.SessionMetadata{WorkspacePath: "/ws/wf", Branch: "ao/wf"},
		CreatedAt: clk.Now(),
	})
	rt.set("in-flight-1", domain.WorkerRuntimeUnsupported)
	dispatchedAt := clk.Now()
	outboxKey := "workflow-step-spawn:" + workStepID
	store.outbox[outboxKey] = domain.WorkflowOutboxEntry{
		ID: "wfo-in-flight", WorkflowRunID: created.Run.ID, WorkflowStepID: &workStepID,
		IdempotencyKey: outboxKey, CommandType: domain.WorkflowOutboxSpawnWorkerSession,
		Status: domain.WorkflowOutboxDispatched, Payload: "{}", DispatchedAt: &dispatchedAt,
	}
	if _, err := store.UpdateWorkflowRunState(ctx, created.Run.ID, domain.WorkflowRunPending, domain.WorkflowRunRunning, clk.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateWorkflowStepState(ctx, workStepID, domain.WorkflowStepPending, domain.WorkflowStepReady, clk.Now()); err != nil {
		t.Fatal(err)
	}

	clk.Advance(5 * time.Second)
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if run, _, _ := store.GetWorkflowRun(ctx, created.Run.ID); run.State == domain.WorkflowRunNeedsAttention {
		t.Fatal("a launch still inside its settle window was parked as ownership unproven")
	}

	clk.Advance(time.Minute)
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	run, _, _ := store.GetWorkflowRun(ctx, created.Run.ID)
	if run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("after the settle window, an unprovable runtime must fail closed; run=%q", run.State)
	}
}
