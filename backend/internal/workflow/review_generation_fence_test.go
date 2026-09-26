package workflow_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// review_generation_fence_test.go -- Frente 3 / 3D preflight, P2 from the
// cycle-2 review: a dispatch holding generation N must not launch a reviewer
// once the durable claim has validly passed to N+1. The pre-launch check used
// to ask only whether the row was still `dispatched`, which a reclaimed row is.
// Every interleaving below is driven by hooks at exact protocol points; there
// are no sleeps.

func reviewOutboxEntry(t *testing.T, store *fakeStore) domain.WorkflowOutboxEntry {
	t.Helper()
	for _, e := range store.outbox {
		if e.CommandType == domain.WorkflowOutboxTriggerReview {
			return e
		}
	}
	t.Fatal("no review outbox entry")
	return domain.WorkflowOutboxEntry{}
}

// reclaimAsForeignGeneration models a VALID ownership change: N's claim is
// released through the generation-conditioned release, and generation N+1 --
// a dispatch live somewhere else, not yet bound to the step -- wins the CAS.
func reclaimAsForeignGeneration(t *testing.T, ctx context.Context, store *fakeStore, clk *fakeClock, next string) string {
	t.Helper()
	e := reviewOutboxEntry(t, store)
	released, err := store.ReleaseDispatchedWorkflowOutboxGeneration(ctx, e.ID, "", e.DispatchGeneration)
	if err != nil || !released {
		t.Fatalf("valid release of N (%q): released=%v err=%v", e.DispatchGeneration, released, err)
	}
	claimed, err := store.ClaimWorkflowOutboxDispatch(ctx, e.ID, clk.Now(), next)
	if err != nil || !claimed {
		t.Fatalf("claim by N+1: claimed=%v err=%v", claimed, err)
	}
	return e.DispatchGeneration
}

// assertSuccessorUntouched checks that nothing N did after losing its claim
// landed on N+1's state.
func assertSuccessorUntouched(t *testing.T, store *fakeStore, runID, next string) {
	t.Helper()
	// Read straight from the store: an API read is itself an observation pass,
	// which would evaluate N+1's claim rather than report what N left behind.
	e := reviewOutboxEntry(t, store)
	if e.Status != domain.WorkflowOutboxDispatched || e.DispatchGeneration != next {
		t.Fatalf("N+1's claim was disturbed: status=%q generation=%q, want dispatched/%q", e.Status, e.DispatchGeneration, next)
	}
	run := store.runs[runID]
	if run.State == domain.WorkflowRunNeedsAttention || run.State.Terminal() {
		t.Fatalf("run moved to %q by a fenced dispatch", run.State)
	}
	for _, s := range store.steps[runID] {
		if s.Kind == domain.WorkflowStepReview && s.State != domain.WorkflowStepRunning {
			t.Fatalf("review step moved to %q by a fenced dispatch; N+1 owns it", s.State)
		}
	}
}

// N claims; the claim validly passes to N+1; N continues. N must be fenced --
// zero launches -- and must leave N+1's claim, step and run exactly as they
// were. Both windows are covered: the reclaim landing before N's READY TO
// LAUNCH re-check, and landing after it (the window only the launch fence
// covers).
func TestReviewLaunchStaleGenerationIsFencedAfterValidReclaim(t *testing.T) {
	for _, window := range []string{"before-ready-check", "after-ready-check"} {
		t.Run(window, func(t *testing.T) {
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

			const next = "wfc-generation-N+1"
			var stale string
			endN1 := func() {}
			reclaim := func() {
				stale = reclaimAsForeignGeneration(t, ctx, store, clk, next)
				// N+1 is a dispatch live in this process (the only way a valid
				// reclaim happens under the daemon lock): reserved, mid-flight.
				endN1 = c.ReserveReviewDispatchForTest(reviewOutboxEntry(t, store).ID, next)
			}
			if window == "before-ready-check" {
				store.afterOutboxClaim = reclaim
			} else {
				launcher.beforePreflight = reclaim
			}
			if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
				t.Fatalf("ContinueRun (N): %v", err)
			}
			if stale == "" || stale == next {
				t.Fatalf("the reclaim did not run inside N's dispatch (stale=%q)", stale)
			}
			if launcher.launchCalls != 0 {
				t.Fatalf("stale generation %q launched %d reviewer(s) after the claim passed to %q", stale, launcher.launchCalls, next)
			}
			assertSuccessorUntouched(t, store, created.Run.ID, next)
			// Only N+1 may launch: on this durable state the fence authorizes
			// N+1's generation and refuses N's.
			row := reviewOutboxEntry(t, store)
			asN, asN1 := row, row
			asN.DispatchGeneration = stale
			if owned, err := c.ReviewClaimOwnedForTest(ctx, created.Run.ID, asN); err != nil || owned {
				t.Fatalf("N (%q) still reads as owner: owned=%v err=%v", stale, owned, err)
			}
			if owned, err := c.ReviewClaimOwnedForTest(ctx, created.Run.ID, asN1); err != nil || !owned {
				t.Fatalf("N+1 (%q) does not read as owner: owned=%v err=%v", next, owned, err)
			}
			// The only thing N owned -- its own review run -- is closed out.
			for id, r := range reviewRuns.runs {
				if r.Status == domain.ReviewRunRunning {
					t.Fatalf("N left its review run %s running with no reviewer behind it", id)
				}
			}

			// Restart/recovery: N+1 dies without launching (its process is
			// gone, so its reservation with it). A restarted daemon recovers
			// the orphaned claim through the durable path and ends with
			// exactly one reviewer -- never N's.
			endN1()
			clk.Advance(time.Second)
			seq := 0
			restarted := workflowcore.New(workflowcore.Deps{
				Store: store, Spawner: spawner, SessionFacts: sessionFacts, WorkspaceFacts: workspaceFacts,
				ReviewRuns: reviewRuns, ReviewerLauncher: launcher, Clock: clk.Now,
				NewID: func() string { seq++; return "restarted-" + strconv.Itoa(seq) },
			})
			for i := 0; i < 3 && launcher.launchCalls == 0; i++ {
				clk.Advance(time.Second)
				if _, err := restarted.ContinueRun(ctx, created.Run.ID); err != nil {
					t.Fatalf("restarted coordinator, pass %d: %v", i, err)
				}
			}
			if launcher.launchCalls != 1 {
				t.Fatalf("launches=%d after restart recovery, want exactly one", launcher.launchCalls)
			}
			if launcher.lastReq.RunID == "" || reviewRuns.runs[launcher.lastReq.RunID].Status != domain.ReviewRunRunning {
				t.Fatalf("the recovered launch is not a live review run: %q", launcher.lastReq.RunID)
			}
		})
	}
}

// A stale N whose own launch then FAILS must not charge that failure to N+1:
// the step write around the (conditioned) outbox write is not conditioned on
// the claim, and used to move N+1's running step back to waiting.
func TestReviewLaunchFailureOfASupersededDispatchLeavesTheSuccessorAlone(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{preflightErr: errors.New("reviewer preflight: transient")}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()
	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)

	const next = "wfc-generation-N+1"
	launcher.beforePreflight = func() {
		reclaimAsForeignGeneration(t, ctx, store, clk, next)
		t.Cleanup(c.ReserveReviewDispatchForTest(reviewOutboxEntry(t, store).ID, next))
	}
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("ContinueRun (N): %v", err)
	}
	if launcher.launchCalls != 0 {
		t.Fatalf("launches=%d, want 0", launcher.launchCalls)
	}
	assertSuccessorUntouched(t, store, created.Run.ID, next)
}

// The full interleaving across two coordinators over one durable state (a
// restarted daemon overlapping the old one, which the daemon lock excludes in
// production): N pauses after its READY TO LAUNCH re-check; the second
// coordinator recovers N and dispatches N+1; while N+1 is inside its own
// launch -- claimed, not yet bound -- N resumes. N must be fenced, N+1 must be
// the one and only reviewer, and a further restart must converge without a
// second launch.
func TestReviewLaunchFenceOnlyTheLiveSuccessorLaunches(t *testing.T) {
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

	restart := func(prefix string) *workflowcore.Coordinator {
		seq := 0
		return workflowcore.New(workflowcore.Deps{
			Store: store, Spawner: spawner, SessionFacts: sessionFacts, WorkspaceFacts: workspaceFacts,
			ReviewRuns: reviewRuns, ReviewerLauncher: launcher, Clock: clk.Now,
			NewID: func() string { seq++; return prefix + strconv.Itoa(seq) },
		})
	}

	// Handoffs are strictly sequential: exactly one goroutine touches the
	// fakes at any moment, ordered by these channels.
	nPaused := make(chan string) // N's generation, once N is past its ready check
	nResume := make(chan struct{})
	nDone := make(chan error, 1)
	launcher.beforePreflight = func() {
		nPaused <- reviewOutboxEntry(t, store).DispatchGeneration
		<-nResume
	}
	go func() { _, err := c.ContinueRun(ctx, created.Run.ID); nDone <- err }()
	genN := <-nPaused
	// Real time passes between N's preparation and the second coordinator's
	// work, so N+1's rows are strictly newer than N's -- as created_at orders
	// them in the real store. A frozen clock would make "the newest review run
	// for this target" a coin flip between N's and N+1's.
	clk.Advance(time.Second)

	c2 := restart("second-")
	var genN1, nextReviewRun string
	var nErr error
	launcher.beforeLaunch = func() {
		// N+1 is inside its launch: claimed, review run created, not bound.
		// Now N resumes and runs to completion.
		genN1 = reviewOutboxEntry(t, store).DispatchGeneration
		nextReviewRun = launcher.lastReq.RunID
		close(nResume)
		nErr = <-nDone
	}
	// The second coordinator recovers N (its dispatch is not live THERE) and
	// dispatches N+1; recovery may take more than one pass.
	for i := 0; i < 3 && genN1 == ""; i++ {
		if _, err := c2.ContinueRun(ctx, created.Run.ID); err != nil {
			t.Fatalf("second coordinator, pass %d: %v", i, err)
		}
	}
	if genN1 == "" {
		t.Fatalf("the second coordinator never dispatched N+1 (outbox=%+v)", reviewOutboxEntry(t, store))
	}
	if nErr != nil {
		t.Fatalf("ContinueRun (N): %v", nErr)
	}
	if genN1 == genN {
		t.Fatalf("N+1 did not take a new generation (%q)", genN)
	}
	if launcher.launchCalls != 1 {
		t.Fatalf("launches=%d, want exactly one (N+1's); stale N %q launched after N+1 %q claimed", launcher.launchCalls, genN, genN1)
	}
	if launcher.lastReq.RunID != nextReviewRun {
		t.Fatalf("the launched reviewer is for review run %q, want N+1's %q", launcher.lastReq.RunID, nextReviewRun)
	}

	// N's generation never reached acknowledgement and its own review run is
	// closed out: it stopped at the fence, not after launching.
	for id, r := range reviewRuns.runs {
		if id != nextReviewRun && r.Status == domain.ReviewRunRunning {
			t.Fatalf("stale N left review run %s running", id)
		}
	}
	// Liveness is NOT asserted here. Two coordinators over one data dir have no
	// shared in-flight registry, so N's own trailing observation pass can take
	// N+1's in-flight launch for an abandoned one and park the run for a
	// person. That is the topology the exclusive daemon lock rules out; the
	// property this test holds it to is safety: one reviewer, N+1's.

	// Restart/recovery after the handover: a fresh coordinator converges on
	// the same single reviewer.
	c3 := restart("third-")
	if _, err := c3.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("restarted coordinator: %v", err)
	}
	if launcher.launchCalls != 1 {
		t.Fatalf("launches=%d after restart, want still one", launcher.launchCalls)
	}
}

// Codex's cycle-2 cross-coordinator interleaving, against a launcher shaped
// like production's: N passes every workflow-side check and enters the
// launcher; while N is still assembling the reviewer's context, a second
// coordinator recovers N and dispatches and launches N+1. N's launcher then
// consults the fence immediately before creating the reviewer and creates
// nothing. (What no fence can cover is an overlap INSIDE the runtime's create
// call itself; that needs two daemons on one data dir, which the exclusive
// daemon lock rules out.)
func TestReviewLaunchFenceInsideTheLauncherCoversProvisioning(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{honorFence: true}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()
	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)

	var staleReviewRun, nextReviewRun string
	launcher.beforeLaunch = func() {
		// N is inside the launcher, provisioning. A second coordinator over
		// the same durable state recovers N and dispatches N+1.
		staleReviewRun = launcher.lastReq.RunID
		clk.Advance(time.Second)
		seq := 0
		second := workflowcore.New(workflowcore.Deps{
			Store: store, Spawner: spawner, SessionFacts: sessionFacts, WorkspaceFacts: workspaceFacts,
			ReviewRuns: reviewRuns, ReviewerLauncher: launcher, Clock: clk.Now,
			NewID: func() string { seq++; return "second-" + strconv.Itoa(seq) },
		})
		for i := 0; i < 3 && launcher.launchCalls == 0; i++ {
			if _, err := second.ContinueRun(ctx, created.Run.ID); err != nil {
				t.Fatalf("second coordinator, pass %d: %v", i, err)
			}
		}
		nextReviewRun = launcher.lastReq.RunID
	}
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("ContinueRun (N): %v", err)
	}
	if staleReviewRun == "" || nextReviewRun == "" || staleReviewRun == nextReviewRun {
		t.Fatalf("interleaving did not happen: N=%q N+1=%q", staleReviewRun, nextReviewRun)
	}
	if launcher.launchCalls != 1 {
		t.Fatalf("reviewers created=%d, want exactly one (N+1's); N created one after N+1 took the claim", launcher.launchCalls)
	}
	if r := reviewRuns.runs[staleReviewRun]; r.Status == domain.ReviewRunRunning {
		t.Fatalf("stale N left its review run %s running", staleReviewRun)
	}
}
