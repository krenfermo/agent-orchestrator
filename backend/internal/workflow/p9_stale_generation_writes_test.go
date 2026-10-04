package workflow_test

// P9 integration review — stale-generation writes.
//
// Every test here takes a pass (generation 1) that has READ the state it will
// act on, lets another pass (generation 2) conclude that same state first, at
// the very last moment before generation 1's write, and then proves generation 1
// wrote NOTHING: no successor attempt, no park, no completion, no checkpoint.
// The interleave is deterministic -- a store decorator runs generation 2 inside
// generation 1's compare-and-swap call, before delegating -- so no sleep and no
// scheduler luck decides the outcome; run it under -race as well.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// gen2WinsFirst runs win once, immediately before the first armed
// ClaimWorkflowAttemptOutcome reaches the store.
type gen2WinsFirst struct {
	*fakeStore
	armed bool
	fired bool
	win   func(ctx context.Context, attemptID string)
}

func (s *gen2WinsFirst) ClaimWorkflowAttemptOutcome(ctx context.Context, attemptID string, finishedAt time.Time, outcome domain.WorkflowAttemptOutcome, errorClass domain.WorkflowErrorClass) (bool, error) {
	if s.armed && !s.fired {
		s.fired = true
		s.win(ctx, attemptID)
	}
	return s.fakeStore.ClaimWorkflowAttemptOutcome(ctx, attemptID, finishedAt, outcome, errorClass)
}

func newStaleGenerationFailoverFixture(t *testing.T, switcher *fakeSwitcher) (*workflowcore.Coordinator, *gen2WinsFirst, string, string) {
	t.Helper()
	base := newFakeStore()
	store := &gen2WinsFirst{fakeStore: base}
	facts := newFakeSessionFacts()
	spawner := &harnessAwareSpawner{facts: facts}
	clk := &fakeClock{t: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store: store, Spawner: spawner, SessionFacts: facts, Switcher: switcher, Clock: clk.Now,
		NewID: func() string { idSeq++; return fmt.Sprintf("sg%d", idSeq) },
	})
	ctx := context.Background()
	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatal(err)
	}
	detail, err := c.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return c, store, created.Run.ID, workStepFrom(detail).Step.ID
}

func phaseCount(t *testing.T, s *fakeStore, runID, phase string) int {
	t.Helper()
	cps, err := s.ListWorkflowCheckpoints(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, cp := range cps {
		if phase == "" || cp.DurablePhase == phase {
			n++
		}
	}
	return n
}

// Failover success path: generation 2 concludes the predecessor and opens its
// own successor while generation 1 is between its idempotent switch and its
// claim. Generation 1 must open no successor and record no completion.
func TestP9StaleGeneration_FailoverLosesThePredecessorAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	switcher := newFakeSwitcher(domain.HarnessClaudeCode)
	c, store, runID, stepID := newStaleGenerationFailoverFixture(t, switcher)
	before, _ := store.ListWorkflowAttempts(ctx, stepID)
	if len(before) == 0 {
		t.Fatal("no attempt to fail over")
	}
	store.win = func(ctx context.Context, attemptID string) {
		if ok, err := store.fakeStore.ClaimWorkflowAttemptOutcome(ctx, attemptID, time.Now().UTC(), domain.WorkflowAttemptFailed, domain.WorkflowErrorRateLimited); err != nil || !ok {
			t.Errorf("generation 2 could not conclude the predecessor: ok=%v err=%v", ok, err)
		}
		if _, created, err := store.ClaimOpenWorkflowAttempt(ctx, "wfa-gen2", stepID, string(domain.HarnessClaudeCode), "", time.Now().UTC()); err != nil || !created {
			t.Errorf("generation 2 could not open its successor: created=%v err=%v", created, err)
		}
	}
	store.armed = true
	checkpointsBefore := phaseCount(t, store.fakeStore, runID, "")

	if _, err := c.ReportWorkStepProviderFailure(ctx, runID, stepID, errRateLimited); err != nil {
		t.Fatalf("generation 1 report: %v", err)
	}
	if !store.fired {
		t.Fatal("the interleave never ran: generation 1 did not reach its claim, so the test proves nothing")
	}
	after, _ := store.ListWorkflowAttempts(ctx, stepID)
	if len(after) != len(before)+1 || after[len(after)-1].ID != "wfa-gen2" {
		ids := []string{}
		for _, a := range after {
			ids = append(ids, a.ID)
		}
		t.Fatalf("attempts = %v, want exactly the original plus generation 2's successor", ids)
	}
	if n := phaseCount(t, store.fakeStore, runID, "work_provider_failover_completed"); n != 0 {
		t.Fatalf("generation 1 recorded a failover completion it did not win (%d)", n)
	}
	// The lifecycle decision is persisted before the switch, idempotently; the
	// claim is what generation 1 lost, and nothing after it may be written.
	if got := phaseCount(t, store.fakeStore, runID, ""); got-checkpointsBefore > 1 {
		t.Fatalf("generation 1 wrote %d checkpoints after losing its claim", got-checkpointsBefore)
	}
	if st := p9StepState(t, store.fakeStore, runID, stepID); st != domain.WorkflowStepRunning {
		t.Fatalf("step = %q, want still running under generation 2", st)
	}
}

// failLiveWorkAttempt: generation 1 decided to park (the switch was rejected),
// but generation 2 concluded the attempt first. Generation 1 must not move the
// step to waiting, park the run, or record a failure checkpoint.
func TestP9StaleGeneration_FailLiveWorkAttemptLosesAndDoesNotPark(t *testing.T) {
	ctx := context.Background()
	switcher := newFakeSwitcher(domain.HarnessClaudeCode)
	switcher.err = errors.New("switch already in progress")
	c, store, runID, stepID := newStaleGenerationFailoverFixture(t, switcher)
	store.win = func(ctx context.Context, attemptID string) {
		if ok, err := store.fakeStore.ClaimWorkflowAttemptOutcome(ctx, attemptID, time.Now().UTC(), domain.WorkflowAttemptSucceeded, ""); err != nil || !ok {
			t.Errorf("generation 2 could not conclude the attempt: ok=%v err=%v", ok, err)
		}
	}
	store.armed = true
	runBefore, _, _ := store.GetWorkflowRun(ctx, runID)
	checkpointsBefore := phaseCount(t, store.fakeStore, runID, "")

	if _, err := c.ReportWorkStepProviderFailure(ctx, runID, stepID, errRateLimited); err != nil {
		t.Fatalf("generation 1 report: %v", err)
	}
	if !store.fired {
		t.Fatal("the interleave never ran: generation 1 did not reach its claim, so the test proves nothing")
	}
	if st := p9StepState(t, store.fakeStore, runID, stepID); st != domain.WorkflowStepRunning {
		t.Fatalf("step = %q, want untouched (running): a stale pass parked it", st)
	}
	runAfter, _, _ := store.GetWorkflowRun(ctx, runID)
	if runAfter.State != runBefore.State {
		t.Fatalf("run moved %s -> %s on a stale pass", runBefore.State, runAfter.State)
	}
	latest, _, _ := store.GetLatestWorkflowAttempt(ctx, stepID)
	if latest.Outcome != domain.WorkflowAttemptSucceeded {
		t.Fatalf("generation 2's outcome was overwritten: %q", latest.Outcome)
	}
	if got := phaseCount(t, store.fakeStore, runID, ""); got-checkpointsBefore > 1 {
		t.Fatalf("a stale pass wrote %d checkpoints", got-checkpointsBefore)
	}
}

func p9StepState(t *testing.T, s *fakeStore, runID, stepID string) domain.WorkflowStepState {
	t.Helper()
	steps, err := s.ListWorkflowSteps(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range steps {
		if st.ID == stepID {
			return st.State
		}
	}
	t.Fatalf("step %s not found", stepID)
	return ""
}

// reopenLost makes the adoption's first step CAS lose, as if another actor --
// a reopened launch, a concurrent Continue -- had moved the step since it was
// read.
type reopenLost struct {
	*fakeStore
	lost bool
}

func (s *reopenLost) ReopenFailedWorkflowStep(ctx context.Context, stepID string, now time.Time) (bool, error) {
	if s.lost {
		return false, nil
	}
	return s.fakeStore.ReopenFailedWorkflowStep(ctx, stepID, now)
}

// Work adoption: a lost step CAS stops the adoption where it lost. The step is
// not carried to completed, no result checkpoint is written, and the attempt
// is not closed by a pass that no longer owns the step.
func TestP9StaleGeneration_AdoptionThatLosesTheStepCASCompletesNothing(t *testing.T) {
	ctx := context.Background()
	fx := newAdoptionFixture(t)
	fx.parkOnLostWorker(t)
	fx.landWorkByHand(t, "backend/internal/codegraph/native.go", "package codegraph\n")

	lossy := &reopenLost{fakeStore: fx.store, lost: true}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store: lossy, Spawner: fx.spawner, SessionFacts: fx.facts, WorkspaceFacts: fx.workspace,
		ReviewRuns: fx.reviewRuns, ReviewerLauncher: &fakeReviewerLauncher{}, Verifier: fx.verifier,
		Clock: fx.clk.Now, NewID: func() string { idSeq++; return "lost" + itoa(idSeq) },
	})
	attemptBefore, _, _ := fx.store.GetLatestWorkflowAttempt(ctx, fx.stepID)
	resultsBefore := phaseCount(t, fx.store, fx.runID, "worker_observed_worker_result_available")

	if _, err := c.ContinueRun(ctx, fx.runID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if st := p9StepState(t, fx.store, fx.runID, fx.stepID); st == domain.WorkflowStepCompleted {
		t.Fatal("an adoption that lost its step CAS still completed the step")
	}
	if got := phaseCount(t, fx.store, fx.runID, "worker_observed_worker_result_available"); got != resultsBefore {
		t.Fatalf("a result checkpoint was written by a lost adoption (%d -> %d)", resultsBefore, got)
	}
	attemptAfter, _, _ := fx.store.GetLatestWorkflowAttempt(ctx, fx.stepID)
	if attemptAfter.ID != attemptBefore.ID || attemptAfter.Outcome != attemptBefore.Outcome {
		t.Fatalf("the attempt was touched by a lost adoption: %+v -> %+v", attemptBefore, attemptAfter)
	}
	if fx.spawner.calls != 1 {
		t.Fatalf("spawner calls = %d, want 1", fx.spawner.calls)
	}
}

// Work adoption under an authorized launch: a pending or dispatched spawn
// command for the step is a generation that may be starting right now. The
// adoption must refuse -- no adoption record, no completion, no attempt closed.
func TestP9StaleGeneration_AdoptionRefusesWhileAWorkerLaunchIsAuthorized(t *testing.T) {
	ctx := context.Background()
	for _, status := range []domain.WorkflowOutboxStatus{domain.WorkflowOutboxPending, domain.WorkflowOutboxDispatched} {
		t.Run(string(status), func(t *testing.T) {
			fx := newAdoptionFixture(t)
			fx.parkOnLostWorker(t)
			fx.landWorkByHand(t, "backend/internal/codegraph/native.go", "package codegraph\n")
			flipped := 0
			for key, e := range fx.store.outbox {
				if e.WorkflowRunID == fx.runID && e.WorkflowStepID != nil && *e.WorkflowStepID == fx.stepID {
					e.Status = status
					fx.store.outbox[key] = e
					flipped++
				}
			}
			if flipped == 0 {
				t.Fatal("no dispatch command found for the work step; the test would prove nothing")
			}
			attemptBefore, _, _ := fx.store.GetLatestWorkflowAttempt(ctx, fx.stepID)

			if _, err := fx.c.ContinueRun(ctx, fx.runID); err != nil {
				t.Fatalf("ContinueRun: %v", err)
			}
			if _, ok := findCheckpoint(fx.store, fx.runID, "work_commit_adopted"); ok {
				t.Fatal("a commit was adopted while a worker launch was authorized for the step")
			}
			if st := p9StepState(t, fx.store, fx.runID, fx.stepID); st == domain.WorkflowStepCompleted {
				t.Fatal("the step was completed under an authorized launch")
			}
			attemptAfter, _, _ := fx.store.GetLatestWorkflowAttempt(ctx, fx.stepID)
			if attemptAfter.ID != attemptBefore.ID || attemptAfter.Outcome != attemptBefore.Outcome {
				t.Fatalf("the attempt was touched: %+v -> %+v", attemptBefore, attemptAfter)
			}
		})
	}
}
