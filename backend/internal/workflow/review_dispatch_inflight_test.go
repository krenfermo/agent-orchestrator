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
