package workflow_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// P0-D section D — repeated restart / ABA.
//
// The crash-boundary suites (P0-A's p0a_crash_recovery_test.go, P0-B's
// generation tests, the recovery_boundaries_* files) each prove ONE restart at
// ONE boundary converges. That is the property those tests exist for, and it is
// not the property this file is about.
//
// What an unattended 24-hour daemon actually does is restart repeatedly against
// records that are not changing — a crash loop, a supervisor flapping, an
// operator restarting while a run is parked. The failure mode that hides there
// is ABA: a recovery pass that is individually idempotent but leaves a trace
// each time, so the fifth restart is not the same as the first. That shows up
// as a ledger that grows without new facts, a generation that drifts, or a
// second launch/prompt on some restart other than the one every test drives.
//
// So every test here restarts the SAME durable records repeatedly (5 is the
// stated P0-D floor) and asserts the invariants across the whole sequence
// rather than after a single pass:
//
//   - no additional Spawn, launch, InsertReviewRun or Send, ever
//   - attempt identity and count are byte-stable from restart 1 to restart 5
//   - the checkpoint ledger stops growing: a restart that learns nothing new
//     must write nothing at all
//
// The ledger assertion is the one that would actually catch a slow leak, and it
// is deliberately "no growth after the first settled restart" rather than a
// bound: a recovery that appends one row per restart forever is exactly the
// shape of the overnight incidents this whole P0 exists for.
const p0dRestarts = 5

// p0dCounters is the whole observable side-effect surface of a restart: what AO
// launched, prompted or created. If none of these move across five restarts,
// nothing was duplicated.
type p0dCounters struct {
	spawns      int
	launches    int
	reviewRuns  int
	sends       int
	checkpoints int
	attempts    int
}

func (f *fixRecoveryFixture) p0dSnapshot(t *testing.T, stepIDs ...string) p0dCounters {
	t.Helper()
	ctx := context.Background()
	c := p0dCounters{
		spawns:     f.spawner.calls,
		launches:   f.launcher.launchCalls,
		reviewRuns: f.reviewRuns.insertCalls,
		sends:      f.sender.calls,
	}
	cps, err := f.store.ListWorkflowCheckpoints(ctx, f.runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	c.checkpoints = len(cps)
	for _, id := range stepIDs {
		at, err := f.store.ListWorkflowAttempts(ctx, id)
		if err != nil {
			t.Fatalf("ListWorkflowAttempts(%s): %v", id, err)
		}
		c.attempts += len(at)
	}
	return c
}

// p0dRestartLoop restarts the daemon p0dRestarts times over the same records,
// reconciling and re-reading on each pass, and returns the counters observed
// after every restart. Time advances between restarts so that anything keyed on
// "has enough time passed" gets its chance to fire more than once — a bound
// that only holds because the clock never moved is not a bound.
func (f *fixRecoveryFixture) p0dRestartLoop(t *testing.T, stepIDs ...string) []p0dCounters {
	t.Helper()
	ctx := context.Background()
	out := make([]p0dCounters, 0, p0dRestarts)
	for i := 0; i < p0dRestarts; i++ {
		c := f.restart()
		if err := c.Reconcile(ctx); err != nil {
			t.Fatalf("restart %d Reconcile: %v", i+1, err)
		}
		if _, err := c.GetRun(ctx, f.runID); err != nil {
			t.Fatalf("restart %d GetRun: %v", i+1, err)
		}
		f.clk.Advance(37 * time.Second)
		out = append(out, f.p0dSnapshot(t, stepIDs...))
	}
	return out
}

// assertStable is the ABA assertion: every counter identical across the whole
// sequence. It names the first restart that moved, because "it drifted on the
// fourth" is a materially different bug from "it drifted on the first".
func assertStable(t *testing.T, what string, seq []p0dCounters) {
	t.Helper()
	for i := 1; i < len(seq); i++ {
		if seq[i] != seq[0] {
			t.Fatalf("%s: restart %d differs from restart 1\n  restart 1: %+v\n  restart %d: %+v",
				what, i+1, seq[0], i+1, seq[i])
		}
	}
}

// State 1: worker running. Five restarts over a live, evidenced worker must
// adopt it every time and never spawn a second.
func TestP0D_WorkerRunningSurvivesFiveRestartsWithoutDuplicating(t *testing.T) {
	f := newFixRecoveryFixture(t)
	ctx := context.Background()

	detail, err := f.c.StartRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	work := workStepFrom(detail)
	sessionID := *work.Step.SessionID
	// A worker that is demonstrably alive and working: adoption is licensed,
	// and a replacement is not.
	f.sessionFacts.put(domain.SessionRecord{
		ID: domain.SessionID(sessionID), ProjectID: "proj-1",
		Activity: domain.Activity{State: domain.ActivityActive, LastActivityAt: f.clk.Now()},
		Metadata: domain.SessionMetadata{WorkspacePath: "/ws/wf", Branch: "ao/wf"},
	})
	if f.spawner.calls != 1 {
		t.Fatalf("spawns before restarts = %d, want 1", f.spawner.calls)
	}

	seq := f.p0dRestartLoop(t, work.Step.ID)
	assertStable(t, "worker-running", seq)
	if seq[0].spawns != 1 {
		t.Fatalf("spawns across %d restarts = %d, want still exactly 1", p0dRestarts, seq[0].spawns)
	}
	if seq[0].attempts != 1 {
		t.Fatalf("work attempts across %d restarts = %d, want still exactly 1", p0dRestarts, seq[0].attempts)
	}

	// The session bound to the step must still be the same incarnation: an
	// adoption that rebinds to a new id would keep the counters flat while
	// silently handing the run to a different worker.
	after, err := f.c.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got := workStepFrom(after).Step.SessionID; got == nil || *got != sessionID {
		t.Fatalf("work session after %d restarts = %v, want the original %s", p0dRestarts, got, sessionID)
	}
}

// State 2: fix running. The fix prompt is the one AO must never send twice —
// a duplicate is a second copy of the reviewer's findings in the composer.
func TestP0D_FixRunningSurvivesFiveRestartsWithoutResending(t *testing.T) {
	f := newFixRecoveryFixture(t)
	ctx := context.Background()

	got := f.driveToFixDispatch()
	fix := fixStepFrom(got)
	if f.sender.calls != 1 {
		t.Fatalf("sends after fix dispatch = %d, want 1", f.sender.calls)
	}

	seq := f.p0dRestartLoop(t, fix.Step.ID)
	assertStable(t, "fix-running", seq)
	if seq[0].sends != 1 {
		t.Fatalf("fix prompt sends across %d restarts = %d, want still exactly 1", p0dRestarts, seq[0].sends)
	}
	if seq[0].attempts != 1 {
		t.Fatalf("fix attempts across %d restarts = %d, want still exactly 1", p0dRestarts, seq[0].attempts)
	}

	after, err := f.c.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if s := fixStepFrom(after).Step.State; s != domain.WorkflowStepRunning {
		t.Fatalf("fix step state after %d restarts = %q, want still running", p0dRestarts, s)
	}
}

// State 3: review running. A reviewer that is alive must be adopted, never
// relaunched, and never given a second review run.
func TestP0D_ReviewRunningSurvivesFiveRestartsWithoutRelaunching(t *testing.T) {
	f := newFixRecoveryFixture(t)
	ctx := context.Background()

	completeWorkStep(t, f.c, f.store, f.clk, f.sessionFacts, f.workspaceFacts, f.runID)
	got, err := f.c.ContinueRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	review := reviewStepFrom(got)
	if review.Step.ReviewRunID == nil {
		t.Fatal("no review run after ContinueRun")
	}
	reviewRunID := *review.Step.ReviewRunID
	if f.launcher.launchCalls != 1 || f.reviewRuns.insertCalls != 1 {
		t.Fatalf("launches=%d inserts=%d after dispatch, want 1/1", f.launcher.launchCalls, f.reviewRuns.insertCalls)
	}

	seq := f.p0dRestartLoop(t, review.Step.ID)
	assertStable(t, "review-running", seq)
	if seq[0].launches != 1 {
		t.Fatalf("reviewer launches across %d restarts = %d, want still exactly 1", p0dRestarts, seq[0].launches)
	}
	if seq[0].reviewRuns != 1 {
		t.Fatalf("review runs across %d restarts = %d, want still exactly 1", p0dRestarts, seq[0].reviewRuns)
	}

	// Same review run, not a replacement: the verdict AO eventually reads must
	// belong to the reviewer it actually launched.
	after, err := f.c.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if id := reviewStepFrom(after).Step.ReviewRunID; id == nil || *id != reviewRunID {
		t.Fatalf("review run after %d restarts = %v, want the original %s", p0dRestarts, id, reviewRunID)
	}
}

// State 4: completed but not finalized. The work step is done and the verdict
// is in; the run has not been driven to its terminal state. Restarts here must
// not re-dispatch the work that already completed.
func TestP0D_CompletedNotFinalizedSurvivesFiveRestarts(t *testing.T) {
	f := newFixRecoveryFixture(t)
	ctx := context.Background()

	completeWorkStep(t, f.c, f.store, f.clk, f.sessionFacts, f.workspaceFacts, f.runID)
	got, err := f.c.ContinueRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	review := reviewStepFrom(got)
	f.reviewRuns.setStatus(*review.Step.ReviewRunID, domain.ReviewRunComplete, domain.VerdictApproved)
	f.clk.Advance(time.Second)
	settled, err := f.c.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetRun after approval: %v", err)
	}
	work := workStepFrom(settled)
	if work.Step.State != domain.WorkflowStepCompleted {
		t.Fatalf("work step = %q, want completed", work.Step.State)
	}

	seq := f.p0dRestartLoop(t, work.Step.ID, review.Step.ID)
	assertStable(t, "completed-not-finalized", seq)
	if seq[0].spawns != 1 {
		t.Fatalf("spawns across %d restarts = %d, want still exactly 1", p0dRestarts, seq[0].spawns)
	}
	if seq[0].launches != 1 {
		t.Fatalf("launches across %d restarts = %d, want still exactly 1", p0dRestarts, seq[0].launches)
	}
	if seq[0].sends != 0 {
		t.Fatalf("sends across %d restarts = %d: an approved run owes no fix prompt", p0dRestarts, seq[0].sends)
	}
}

// State 5: stale runtime. The worker's session is gone from SessionFacts
// entirely — the daemon came back to a host where the pane did not survive.
// Repeated restarts must reach one settled answer and stay there, and in
// particular must not spawn a replacement on some later pass having declined
// to on the first.
func TestP0D_StaleRuntimeSurvivesFiveRestartsWithoutRespawning(t *testing.T) {
	f := newFixRecoveryFixture(t)
	ctx := context.Background()

	detail, err := f.c.StartRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	work := workStepFrom(detail)
	// The runtime is simply not there any more. Not terminated-with-evidence,
	// not idle: absent, which is the state a rebooted host presents.
	delete(f.sessionFacts.byID, domain.SessionID(*work.Step.SessionID))
	f.spawner.calls = 0

	seq := f.p0dRestartLoop(t, work.Step.ID)
	assertStable(t, "stale-runtime", seq)
	if seq[0].spawns != 0 {
		t.Fatalf("spawns across %d restarts of a stale runtime = %d, want 0: "+
			"a vanished session is not authority to start a second worker", p0dRestarts, seq[0].spawns)
	}

	after, err := f.c.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if s := after.Run.State; s == domain.WorkflowRunCompleted {
		t.Fatal("a run whose worker vanished must not converge on completed")
	}
}
