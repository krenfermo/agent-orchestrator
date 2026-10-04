package workflow_test

import (
	"reflect"
	"strconv"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// A PROVEN-superseded holder releases nothing and spends nothing -- even where
// the release path's own budget gate would otherwise act.
//
// The state: generation N+1 validly holds the claim and is the cycle's final
// permitted attempt (the budget is fully spent -- by N+1, legitimately). A
// stale dispatch N, still carrying its old entry, reaches the release choke
// point. The generation-conditioned outbox write would refuse N, but the
// budget gate runs BEFORE it: without the superseded-holder guard it reads
// the exhausted cycle, refuses, and parks the review as ambiguous -- on N+1's
// own history, while N+1 is the live owner. The guard must make N a no-op:
// N+1's claim, step and run stay exactly as they were.
func TestReviewReleaseBySupersededHolderSpendsNothingOnTheSuccessorsBudget(t *testing.T) {
	f := newBudgetFixture(t)
	steps, _ := f.store.ListWorkflowSteps(f.ctx, f.runID)
	var reviewStep domain.WorkflowStep
	for _, st := range steps {
		if st.Kind == domain.WorkflowStepReview {
			reviewStep = st
		}
	}
	stepID := reviewStep.ID
	key := "workflow-step-review:" + stepID + ":cycle1:codex"

	// N+1 owns the durable claim.
	const next, stale = "wfc-gen-next", "wfc-gen-stale"
	e := f.store.outbox[key]
	e.ID, e.WorkflowRunID, e.WorkflowStepID = "wfo-g4", f.runID, &stepID
	e.IdempotencyKey, e.CommandType = key, domain.WorkflowOutboxTriggerReview
	e.Status, e.Payload, e.DispatchGeneration = domain.WorkflowOutboxDispatched, "{}", next
	f.store.outbox[key] = e

	// The cycle's budget is fully spent, the last attempt being N+1's.
	if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
		ID: "wfc-g4-claim", WorkflowRunID: f.runID, WorkflowStepID: &stepID,
		ProjectID: "proj-1", DurablePhase: "review_launch_claimed", PayloadVersion: "v1",
		RetryState: `{"idempotencyKey":"` + key + `"}`, CreatedAt: f.clk.Now(),
	}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
			ID: "wfc-g4-" + strconv.Itoa(attempt), WorkflowRunID: f.runID,
			WorkflowStepID: &stepID, ProjectID: "proj-1",
			DurablePhase:   "review_launch_attempt",
			PayloadVersion: "v1",
			RetryState: `{"idempotencyKey":"` + key + `","cycle":1,"epoch":1,"attempt":` +
				strconv.Itoa(attempt) + `}`,
			CreatedAt: f.clk.Now(),
		}); err != nil {
			t.Fatalf("seed attempt %d: %v", attempt, err)
		}
	}

	// The fixture is meaningful only if the budget gate WOULD refuse here:
	// that is the path the guard has to keep N off.
	run, _, _ := f.store.GetWorkflowRun(f.ctx, f.runID)
	if ok, _, err := f.c.ReviewLaunchBudgetRemainsForTest(f.ctx, run, reviewStep, e); err != nil || ok {
		t.Fatalf("fixture broken: budget gate ok=%v err=%v; it must refuse this exhausted cycle", ok, err)
	}

	stepsBefore := append([]domain.WorkflowStep(nil), f.store.steps[f.runID]...)
	runBefore := f.store.runs[f.runID]
	checkpointsBefore := f.countPhase("review_launch_attempt")

	staleEntry := e
	staleEntry.DispatchGeneration = stale
	if _, err := f.c.ReleaseReviewDispatchClaimForTest(f.ctx, run, reviewStep, staleEntry,
		"a stale dispatch closing out after its claim passed on"); err != nil {
		t.Fatalf("release by the stale holder: %v", err)
	}

	got := f.store.outbox[key]
	if got.Status != domain.WorkflowOutboxDispatched || got.DispatchGeneration != next {
		t.Fatalf("N+1's claim was disturbed: status=%q generation=%q, want dispatched/%q",
			got.Status, got.DispatchGeneration, next)
	}
	if !reflect.DeepEqual(f.store.steps[f.runID], stepsBefore) {
		t.Fatalf("a superseded holder moved the successor's steps:\nbefore=%+v\nafter=%+v", stepsBefore, f.store.steps[f.runID])
	}
	if !reflect.DeepEqual(f.store.runs[f.runID], runBefore) {
		t.Fatalf("a superseded holder moved the run: before=%q after=%q", runBefore.State, f.store.runs[f.runID].State)
	}
	if n := f.countPhase("review_launch_attempt"); n != checkpointsBefore {
		t.Fatalf("attempt records changed %d -> %d: a superseded holder spent budget", checkpointsBefore, n)
	}
}
