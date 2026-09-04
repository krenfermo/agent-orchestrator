package workflow_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// f6_reviewer_runtime_test.go — the regression suite for F6.
//
// THE INCIDENT. A controlled daemon restart landed 1.3s after a reviewer was
// launched. On startup the runtime GC classified that live reviewer as
// `unreferenced_owned_session` and destroyed it: the reviewer's capacity claim
// was held but UNBOUND (empty runtime_instance_id), and GC protects a runtime
// only when a held claim names its incarnation. The claim could never acquire
// that name — bindRuntimesToClaims resolves a runtime through the step's
// `sessions` row, and a reviewer runs in an AO runtime that has none.
//
// AO then never noticed. reviewerSessionStalled requires the session's
// LastActivityAt to be AFTER the dispatch — an anti-race guard so a worker's
// leftover idle state is not read as an instant stall — and a reviewer killed
// before its first hook never advances that timestamp. So review_run stayed
// `running` with no runtime, 0 attempts and no verdict; RecoveryStatus said "A
// review is in flight"; Advice said "AO is working. You do not need to do
// anything." The run would have waited out the full 30-minute staleness
// threshold and then summoned a person for a condition AO created itself.

// f6ReviewInFlight drives a run to a launched, running reviewer and returns the
// pieces the tests then manipulate.
func f6ReviewInFlight(t *testing.T) (*workflowcore.Coordinator, *fakeStore, *fakeClock, *fakeReviewerLauncher, string) {
	t.Helper()
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
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	detail, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got := reviewStepFrom(detail).Step.State; got != domain.WorkflowStepRunning {
		t.Fatalf("review step = %q, want running (fixture broken)", got)
	}
	return c, store, clk, launcher, created.Run.ID
}

// TestF6_ConfirmedReviewerWithNoRuntimeIsRecovered is the core regression: a
// reviewer that is provably gone must be treated as a stall and recovered
// automatically, not waited out for thirty minutes and handed to a person.
func TestF6_ConfirmedReviewerWithNoRuntimeIsRecovered(t *testing.T) {
	c, _, clk, launcher, runID := f6ReviewInFlight(t)
	ctx := context.Background()

	// The GC destroys the reviewer: its name no longer holds the incarnation
	// the confirmation recorded, which is exactly what the runtime reports.
	launcher.externalLive = map[string]bool{}
	launcher.instances = map[string]string{}

	// Past the anti-race grace, and nowhere near the 30-minute threshold: the
	// only thing that can act here is proof the runtime is gone.
	clk.Advance(2 * time.Minute)

	detail, err := c.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got := reviewStepFrom(detail).Step.State; got == domain.WorkflowStepRunning {
		t.Fatal("the review step is still `running` with no reviewer runtime; AO would wait out the staleness threshold and then ask a person")
	}
}

// TestF6_UncertainReviewerPresenceNeverTriggersRecovery is the other half. Only
// PROVEN absence may act: `unknown` means the probe could not tell, and acting
// on it would risk launching a second reviewer over a live one.
func TestF6_UncertainReviewerPresenceNeverTriggersRecovery(t *testing.T) {
	c, _, clk, launcher, runID := f6ReviewInFlight(t)
	ctx := context.Background()

	launcher.probeUnknown = true
	clk.Advance(2 * time.Minute)

	detail, err := c.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got := reviewStepFrom(detail).Step.State; got != domain.WorkflowStepRunning {
		t.Fatalf("review step = %q; an unprovable probe must leave a possibly-live reviewer alone", got)
	}
}

// TestF6_LiveReviewerIsLeftAlone keeps the new probe from disturbing the
// ordinary case: a reviewer that is still there stays there.
func TestF6_LiveReviewerIsLeftAlone(t *testing.T) {
	c, _, clk, _, runID := f6ReviewInFlight(t)
	ctx := context.Background()

	clk.Advance(2 * time.Minute)

	detail, err := c.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got := reviewStepFrom(detail).Step.State; got != domain.WorkflowStepRunning {
		t.Fatalf("review step = %q, want running: a live reviewer must not be recovered out from under itself", got)
	}
}
