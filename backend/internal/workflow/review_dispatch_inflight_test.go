package workflow_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// review_dispatch_inflight_test.go -- the reviewer-dispatch race observed in
// the Frente 3 / 3D preflight E2E (runs wf-38cc5f74 and wf-bd9b2bea): a wake
// re-entered the review step while THIS daemon's own dispatch was still
// provisioning the reviewer (memory pack, external context, launch). The
// re-entry found the outbox claim `dispatched` and a review run with only a
// launch intent, probed the reviewer before it existed, concluded "no reviewer
// launch was ever recorded", failed the review run and released the claim --
// and the run stopped on a failed review whose reviewer then started anyway.
//
// The property: a review whose launch is validly in progress in this process
// must not be declared absent. The re-entry is reproduced deterministically by
// calling ContinueRun from inside the launch window.
func TestReviewDispatchInProgressIsNotDeclaredAbsentByAConcurrentPass(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)

	concurrentPassRan := false
	launcher.beforeLaunch = func() {
		// The wake poller's pass, arriving while the dispatch above is between
		// "review run created" and "reviewer launched".
		concurrentPassRan = true
		done := make(chan error, 1)
		go func() { _, err := c.ContinueRun(ctx, created.Run.ID); done <- err }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("concurrent ContinueRun: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("concurrent ContinueRun blocked behind the in-flight dispatch")
		}
	}

	got, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if !concurrentPassRan {
		t.Fatal("the launch window was never reached")
	}
	review := reviewStepFrom(got)
	if review.Step.State != domain.WorkflowStepRunning || review.Step.ReviewRunID == nil {
		t.Fatalf("review step = %q (review run %v), want running with its review run", review.Step.State, review.Step.ReviewRunID)
	}
	rr := reviewRuns.runs[*review.Step.ReviewRunID]
	if rr.Status == domain.ReviewRunFailed {
		t.Fatalf("the in-flight review run was declared absent and failed: %+v", rr)
	}
	if launcher.launchCalls != 1 || reviewRuns.insertCalls != 1 {
		t.Fatalf("launches=%d inserts=%d, want exactly one reviewer", launcher.launchCalls, reviewRuns.insertCalls)
	}
	// And a later pass (the dispatch has finished) still sees one healthy review.
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("later ContinueRun: %v", err)
	}
	if rr := reviewRuns.runs[*review.Step.ReviewRunID]; rr.Status == domain.ReviewRunFailed || launcher.launchCalls != 1 {
		t.Fatalf("after the dispatch finished: status=%q launches=%d", rr.Status, launcher.launchCalls)
	}
}

// Restart safety: the in-flight set is per process. A coordinator that did NOT
// start the dispatch -- a restarted daemon, whose own set is empty -- still
// runs the durable recovery exactly as before: the claim's owner is gone as far
// as it can know, so it probes, finds no reviewer and closes the identity out.
func TestReviewDispatchRecoveryAfterRestartIsUnchanged(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()
	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)

	var firstReviewRun string
	launcher.beforeLaunch = func() {
		for id := range reviewRuns.runs {
			firstReviewRun = id
		}
		// A second process over the same durable state.
		restarted := workflowcore.New(workflowcore.Deps{
			Store: store, Spawner: spawner, SessionFacts: sessionFacts, WorkspaceFacts: workspaceFacts,
			ReviewRuns: reviewRuns, ReviewerLauncher: launcher, Clock: clk.Now,
			NewID: func() string { return "restarted-" + strconv.Itoa(len(reviewRuns.runs)) },
		})
		if _, err := restarted.ContinueRun(ctx, created.Run.ID); err != nil {
			t.Errorf("restarted ContinueRun: %v", err)
		}
	}
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if firstReviewRun == "" {
		t.Fatal("the launch window was never reached")
	}
	if got := reviewRuns.runs[firstReviewRun].Status; got != domain.ReviewRunFailed {
		t.Fatalf("a coordinator that does not own the dispatch must still recover it: first review run status=%q, want failed", got)
	}
}

// The interval Codex found (3D preflight review): the claim is DURABLE -- the
// outbox row reads `dispatched` with this dispatch's generation -- but the
// dispatcher has not yet returned from the claim call. A concurrent pass in
// that interval must treat the dispatch as live: it may not declare the
// reviewer absent, mark the review ambiguous, release the claim or launch a
// second reviewer.
func TestReviewDispatchDurableClaimBeforeLocalRegistrationIsLive(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()
	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)

	reached := false
	var claimedGeneration string
	store.afterOutboxClaim = func() {
		reached = true
		for _, e := range store.outbox {
			if e.CommandType == domain.WorkflowOutboxTriggerReview {
				claimedGeneration = e.DispatchGeneration
			}
		}
		concurrent, err := c.ContinueRun(ctx, created.Run.ID)
		if err != nil {
			t.Errorf("concurrent pass: %v", err)
			return
		}
		if concurrent.Run.State == domain.WorkflowRunNeedsAttention {
			t.Errorf("the concurrent pass declared the live dispatch ambiguous/absent (run needs_attention)")
		}
		for _, e := range store.outbox {
			if e.CommandType == domain.WorkflowOutboxTriggerReview &&
				(e.Status != domain.WorkflowOutboxDispatched || e.DispatchGeneration != claimedGeneration) {
				t.Errorf("the concurrent pass released or re-claimed the live claim: status=%q generation=%q", e.Status, e.DispatchGeneration)
			}
		}
		for id, rr := range reviewRuns.runs {
			if rr.Status == domain.ReviewRunFailed {
				t.Errorf("the concurrent pass failed review run %s", id)
			}
		}
		if launcher.launchCalls != 0 {
			t.Errorf("the concurrent pass launched a reviewer (%d) while the owner had not yet", launcher.launchCalls)
		}
	}
	got, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if !reached {
		t.Fatal("the durable-claim interval was never reached")
	}
	if got.Run.State == domain.WorkflowRunNeedsAttention {
		t.Fatal("run ended needs_attention")
	}
	review := reviewStepFrom(got)
	if review.Step.State != domain.WorkflowStepRunning || review.Step.ReviewRunID == nil {
		t.Fatalf("review step = %q, want running with its review run", review.Step.State)
	}
	if launcher.launchCalls != 1 || reviewRuns.insertCalls != 1 {
		t.Fatalf("launches=%d inserts=%d, want exactly one reviewer", launcher.launchCalls, reviewRuns.insertCalls)
	}
}

// Generation/reclaim integration: while THIS process's dispatch N is between
// its durable claim and its launch, the row is reclaimed by generation N+1
// (another owner). N's local reservation must not protect N+1 -- a concurrent
// pass must treat N+1's claim by its own facts -- and the outcome must never be
// two reviewers.
func TestReviewDispatchReservationDoesNotCoverAReclaimedGeneration(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()
	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)

	store.afterOutboxClaim = func() {
		// Model a valid reclaim: the row now belongs to generation N+1, which
		// this process never reserved.
		for key, e := range store.outbox {
			if e.CommandType == domain.WorkflowOutboxTriggerReview {
				e.DispatchGeneration = "gen-N+1-foreign"
				store.outbox[key] = e
			}
		}
		got, err := c.ContinueRun(ctx, created.Run.ID)
		if err != nil {
			t.Errorf("concurrent pass: %v", err)
		}
		// N's reservation must not have short-circuited the pass: it must
		// have evaluated N+1's claim on its own facts, which here (a claim
		// with no review run) is the durable recovery -- the row no longer
		// reads (dispatched, N+1) or the run was stopped for a person.
		for _, e := range store.outbox {
			if e.CommandType == domain.WorkflowOutboxTriggerReview {
				t.Logf("after the concurrent pass: status=%q generation=%q run=%q", e.Status, e.DispatchGeneration, got.Run.State)
				if e.Status == domain.WorkflowOutboxDispatched && e.DispatchGeneration == "gen-N+1-foreign" &&
					got.Run.State != domain.WorkflowRunNeedsAttention {
					t.Errorf("the pass treated N+1 as covered by N's reservation (nothing evaluated)")
				}
			}
		}
	}
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if launcher.launchCalls > 1 {
		t.Fatalf("launches=%d: a reclaimed generation produced two reviewers", launcher.launchCalls)
	}
}
