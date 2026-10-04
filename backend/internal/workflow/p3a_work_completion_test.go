package workflow_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// p3a_work_completion_test.go — the P3-A blocker.
//
// Both real smokes ended in the same state: the worker had finished, its change
// was on disk, the session read "completed", /continue answered 200, and the
// work step stayed `running` forever. Review never started, verify never
// started, the run never reached a terminal state.
//
// Two independent causes, and the tests below cover both.
//
//  1. A TUI worker does not exit when it finishes. Every "the work is over"
//     fact the work-step evaluator read was about the PROCESS — terminated,
//     exited, gone — and a finished TUI worker satisfies none of them: the pane
//     stays alive and the session goes idle, exactly as it does between two
//     turns. The one fact that distinguishes finished from quiet, the harness's
//     turn-completion receipt, was invisible to workflow. So an idle worker
//     whose workspace could not be observed was evaluated as "look again
//     later", with no bound and nothing that could ever change the answer.
//
//  2. The workspace observation could not be obtained for a direct-branch run
//     at all. See the router package's own test: the observation was routed by
//     the PROJECT's execution mode, and the worktree adapter refuses a path
//     outside its managed root — which is precisely what a direct-branch
//     checkout is.

// tuiWorkerSession is a TUI worker that has finished: a Stop hook arrived, so
// the session is idle carrying a durable turn-completion receipt, and the pane
// is still alive so it is neither terminated nor exited. This is the exact row
// shape both smokes left behind.
func tuiWorkerSession(id domain.SessionID, path string, firstSignal, completedAt time.Time) domain.SessionRecord {
	return domain.SessionRecord{
		ID:              id,
		Activity:        domain.Activity{State: domain.ActivityIdle, LastActivityAt: completedAt},
		Metadata:        domain.SessionMetadata{Branch: "main", WorkspacePath: path},
		FirstSignalAt:   firstSignal,
		TurnCompletedAt: completedAt,
	}
}

type completionFixture struct {
	c        *workflowcore.Coordinator
	store    *fakeStore
	clk      *fakeClock
	facts    *fakeSessionFacts
	ws       *fakeWorkspaceFacts
	reviews  *fakeReviewRuns
	launcher *fakeReviewerLauncher
	runID    string
	stepID   string
	session  domain.SessionID
}

// startWorker creates a run, starts it, and returns the fixture with the work
// step running under a live worker session.
func startWorker(t *testing.T, ws *fakeWorkspaceFacts, workspacePath string) completionFixture {
	t.Helper()
	facts := newFakeSessionFacts()
	spawner := &fakeSpawner{
		rec:   domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "main", WorkspacePath: workspacePath}},
		facts: facts,
	}
	reviews := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, facts, ws, reviews, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	detail, err := c.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	work := workStepFrom(detail)
	if work.Step.State != domain.WorkflowStepRunning {
		t.Fatalf("work step state = %q, want running", work.Step.State)
	}
	return completionFixture{
		c: c, store: store, clk: clk, facts: facts, ws: ws,
		reviews: reviews, launcher: launcher,
		runID: created.Run.ID, stepID: work.Step.ID,
		session: domain.SessionID(*work.Step.SessionID),
	}
}

// finishTurn puts the worker in the finished-TUI shape.
func (f completionFixture) finishTurn(path string) {
	f.clk.Advance(2 * time.Minute)
	f.facts.put(tuiWorkerSession(f.session, path, f.clk.Now().Add(-90*time.Second), f.clk.Now()))
	f.clk.Advance(10 * time.Second)
}

func (f completionFixture) workStep(t *testing.T) domain.WorkflowStep {
	t.Helper()
	got, err := f.c.GetRun(context.Background(), f.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	return workStepFrom(got).Step
}

// A. A TUI worker finishes, its change is on disk, the work step completes and
// the review starts. This is the acceptance case: it is not enough for the row
// to move, the lifecycle has to continue.
func TestTUIWorkerCompletionConcludesWorkAndStartsReview(t *testing.T) {
	ws := &fakeWorkspaceFacts{obs: ports.WorkspaceObservation{HeadSHA: "after", Dirty: true}}
	f := startWorker(t, ws, "/ws/wf")
	f.finishTurn("/ws/wf")

	got, err := f.c.ContinueRun(context.Background(), f.runID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if state := workStepFrom(got).Step.State; state != domain.WorkflowStepCompleted {
		t.Fatalf("work step state = %q, want completed", state)
	}
	review := reviewStepFrom(got)
	if review.Step.State != domain.WorkflowStepRunning {
		t.Fatalf("review step state = %q, want running — completing the row is not the fix, continuing the lifecycle is", review.Step.State)
	}
	if f.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launch calls = %d, want 1", f.launcher.launchCalls)
	}
}

// B/C. The same, for each placement's workspace shape: a direct-branch run
// observes the repository itself on the project's own branch, an isolated run
// observes its worktree on an ao/* branch. The work step's completion authority
// must not depend on which one it is.
func TestTUIWorkerCompletionIsPlacementIndependent(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		obs  ports.WorkspaceObservation
	}{
		{"direct_branch", "/repo", ports.WorkspaceObservation{Path: "/repo", Branch: "main", HeadSHA: "after", Dirty: true}},
		{"isolated_worktree", "/ao/worktrees/p/s1", ports.WorkspaceObservation{Path: "/ao/worktrees/p/s1", Branch: "ao/s1/root", HeadSHA: "after", Dirty: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := startWorker(t, &fakeWorkspaceFacts{obs: tc.obs}, tc.path)
			f.finishTurn(tc.path)
			if state := f.workStep(t).State; state != domain.WorkflowStepCompleted {
				t.Fatalf("work step state = %q, want completed", state)
			}
		})
	}
}

// E. Fail closed. A worker that says it is done and left nothing behind is not
// a completed task: the run stops for a person rather than completing on the
// worker's own word.
func TestTUIWorkerCompletionWithNoOutcomeFailsClosed(t *testing.T) {
	// Observable, and empty: HEAD is where the dispatch left it and nothing is
	// dirty, staged or untracked.
	f := startWorker(t, &fakeWorkspaceFacts{}, "/ws/wf")
	f.finishTurn("/ws/wf")

	got, err := f.c.GetRun(context.Background(), f.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if state := workStepFrom(got).Step.State; state == domain.WorkflowStepCompleted {
		t.Fatal("a worker that produced nothing verifiable completed the work step")
	}
	if got.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", got.Run.State)
	}
	if f.launcher.launchCalls != 0 {
		t.Fatalf("reviewer launch calls = %d, want 0 — nothing was produced to review", f.launcher.launchCalls)
	}
}

// The blocker itself, stated as a property: a finished worker never leaves the
// work step running. Whatever AO can or cannot prove, the step reaches a
// decision in bounded time and /continue converges it.
func TestFinishedTUIWorkerNeverLeavesWorkStepRunning(t *testing.T) {
	for _, tc := range []struct {
		name string
		ws   *fakeWorkspaceFacts
	}{
		{"workspace observable, work present", &fakeWorkspaceFacts{obs: ports.WorkspaceObservation{HeadSHA: "after", Dirty: true}}},
		{"workspace observable, nothing produced", &fakeWorkspaceFacts{}},
		// The direct-branch smoke's exact shape before the router fix: the
		// repository is real, the change is in it, and AO cannot read it.
		{"workspace unreadable", &fakeWorkspaceFacts{err: errors.New(`gitworktree: unsafe workspace path: "/repo" is outside managed root`)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := startWorker(t, tc.ws, "/repo")
			f.finishTurn("/repo")
			if _, err := f.c.ContinueRun(context.Background(), f.runID); err != nil {
				t.Fatalf("ContinueRun: %v", err)
			}
			got, err := f.c.GetRun(context.Background(), f.runID)
			if err != nil {
				t.Fatalf("GetRun: %v", err)
			}
			if state := workStepFrom(got).Step.State; state == domain.WorkflowStepRunning {
				t.Fatalf("work step still running after the worker reported it finished (run=%q)", got.Run.State)
			}
		})
	}
}

// F. Generation/ownership. A completion receipt that PREDATES this attempt's
// dispatch is an older turn's word and may not close the current work step.
func TestStaleTurnCompletionCannotCloseTheCurrentWorkStep(t *testing.T) {
	f := startWorker(t, &fakeWorkspaceFacts{err: errors.New("workspace unreadable")}, "/repo")

	attempts, err := f.store.ListWorkflowAttempts(context.Background(), f.stepID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts = %+v err=%v, want exactly 1", attempts, err)
	}
	dispatchedAt := attempts[0].StartedAt

	f.clk.Advance(5 * time.Minute)
	stale := tuiWorkerSession(f.session, "/repo", dispatchedAt, dispatchedAt.Add(-time.Hour))
	f.facts.put(stale)

	if state := f.workStep(t).State; state != domain.WorkflowStepRunning {
		t.Fatalf("work step state = %q, want still running — a receipt from before this dispatch decided the current attempt", state)
	}
}

// G/H. Exactly once. Two mechanisms observing the same completion — a poll and
// Continue — produce exactly one transition, one completion checkpoint, one
// finalized attempt and one reviewer launch. The completion checkpoint is what
// review dispatch resolves its worktree and target fingerprint from, so a
// second one for the same completion is not merely untidy.
func TestConcurrentObserversConcludeWorkExactlyOnce(t *testing.T) {
	ws := &fakeWorkspaceFacts{obs: ports.WorkspaceObservation{HeadSHA: "after", Dirty: true}}
	f := startWorker(t, ws, "/ws/wf")
	f.finishTurn("/ws/wf")
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		if _, err := f.c.GetRun(ctx, f.runID); err != nil {
			t.Fatalf("GetRun %d: %v", i, err)
		}
		if _, err := f.c.ContinueRun(ctx, f.runID); err != nil {
			t.Fatalf("ContinueRun %d: %v", i, err)
		}
	}

	cps, err := f.store.ListWorkflowCheckpoints(ctx, f.runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	completions := 0
	for _, cp := range cps {
		if cp.WorkflowStepID != nil && *cp.WorkflowStepID == f.stepID && cp.NextAction == "start_review" {
			completions++
		}
	}
	if completions != 1 {
		t.Fatalf("work completion checkpoints = %d, want exactly 1", completions)
	}
	if f.reviews.insertCalls != 1 {
		t.Fatalf("review runs inserted = %d, want exactly 1", f.reviews.insertCalls)
	}
	if f.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches = %d, want exactly 1", f.launcher.launchCalls)
	}
	attempts, err := f.store.ListWorkflowAttempts(ctx, f.stepID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts = %+v err=%v, want exactly 1", attempts, err)
	}
	if attempts[0].Outcome != domain.WorkflowAttemptSucceeded {
		t.Fatalf("attempt outcome = %q, want succeeded", attempts[0].Outcome)
	}
	if attempts[0].FinishedAt == nil {
		t.Fatal("attempt finished_at is NULL after the work step completed")
	}
}

// I. Restart convergence. The worker finished, the daemon died before the work
// step could be closed, and the daemon comes back. The receipt is durable, so
// boot reconciliation reconstructs the same conclusion with no human in it.
func TestDaemonRestartConvergesAFinishedWorker(t *testing.T) {
	ws := &fakeWorkspaceFacts{obs: ports.WorkspaceObservation{HeadSHA: "after", Dirty: true}}
	facts := newFakeSessionFacts()
	spawner := &fakeSpawner{
		rec:   domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "main", WorkspacePath: "/repo"}},
		facts: facts,
	}
	reviews := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	store := newFakeStore()
	store.reviewRuns = reviews
	clk := &fakeClock{t: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
	var idSeq int
	deps := workflowcore.Deps{
		Store: store, Spawner: spawner, SessionFacts: facts, WorkspaceFacts: ws,
		ReviewRuns: reviews, ReviewerLauncher: launcher, Clock: clk.Now,
		NewID: func() string { idSeq++; return fmt.Sprintf("id%d", idSeq) },
	}
	ctx := context.Background()

	c := workflowcore.New(deps)
	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	detail, err := c.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	work := workStepFrom(detail)
	sessionID := domain.SessionID(*work.Step.SessionID)

	// The worker finishes. Nothing observes it: the daemon is gone.
	clk.Advance(3 * time.Minute)
	facts.put(tuiWorkerSession(sessionID, "/repo", clk.Now().Add(-2*time.Minute), clk.Now()))
	clk.Advance(time.Minute)

	// A new daemon, over the same durable state.
	restarted := workflowcore.New(deps)
	if err := restarted.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got, err := restarted.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if state := workStepFrom(got).Step.State; state != domain.WorkflowStepCompleted {
		t.Fatalf("work step state = %q after restart, want completed — convergence must need no human", state)
	}
}

// M. A worker whose runtime can still write has not finished, whatever else is
// true of it. An active session with work evidence already on disk is not a
// completion: the agent is mid-turn and the tree it left is not its result.
func TestActiveWorkerIsNeverConcludedFromWorkspaceEvidence(t *testing.T) {
	ws := &fakeWorkspaceFacts{obs: ports.WorkspaceObservation{HeadSHA: "after", Dirty: true}}
	f := startWorker(t, ws, "/ws/wf")
	f.clk.Advance(2 * time.Minute)
	// Active, and carrying no receipt — which is what the lifecycle guarantees
	// for a turn that is in flight.
	f.facts.put(domain.SessionRecord{
		ID:            f.session,
		Activity:      domain.Activity{State: domain.ActivityActive, LastActivityAt: f.clk.Now()},
		Metadata:      domain.SessionMetadata{Branch: "main", WorkspacePath: "/ws/wf"},
		FirstSignalAt: f.clk.Now().Add(-time.Minute),
	})
	f.clk.Advance(10 * time.Second)
	if state := f.workStep(t).State; state != domain.WorkflowStepRunning {
		t.Fatalf("work step state = %q, want still running — the worker can still write", state)
	}
	if f.launcher.launchCalls != 0 {
		t.Fatalf("reviewer launch calls = %d, want 0", f.launcher.launchCalls)
	}
}

// N. The other half of the completion authority, unchanged and still working: a
// worker whose RUNTIME is gone and whose result is on disk concludes without
// needing any receipt at all.
func TestTerminatedWorkerWithDurableResultStillConcludes(t *testing.T) {
	ws := &fakeWorkspaceFacts{obs: ports.WorkspaceObservation{HeadSHA: "after", Dirty: true}}
	f := startWorker(t, ws, "/ws/wf")
	f.clk.Advance(2 * time.Minute)
	f.facts.put(domain.SessionRecord{
		ID:            f.session,
		IsTerminated:  true,
		Activity:      domain.Activity{State: domain.ActivityExited, LastActivityAt: f.clk.Now()},
		Metadata:      domain.SessionMetadata{Branch: "main", WorkspacePath: "/ws/wf"},
		FirstSignalAt: f.clk.Now().Add(-time.Minute),
	})
	f.clk.Advance(10 * time.Second)
	if state := f.workStep(t).State; state != domain.WorkflowStepCompleted {
		t.Fatalf("work step state = %q, want completed", state)
	}
}
