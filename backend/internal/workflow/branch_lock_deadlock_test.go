package workflow_test

// Checkpoint 8P-E.13A regression fixture.
//
// Every test in this file is modeled directly on the state found in
// ~/.ao/data/ao.db on 2026-08-21, reconstructed synthetically here — the real
// database is never read or written by tests:
//
//	wf-3220567f  needs_attention, holds feat/engineering-control-center,
//	             worked and fixed for ~19h before its review stopped for a
//	             human decision.
//	wf-40209d5f  master run, needs_attention.
//	wf-507d9a93  its child: plan completed, work ready, review pending, no work
//	             session — queued behind wf-3220567f, and repeatedly recording
//	             "review_dispatch_ambiguous: work step has no recorded
//	             session/checkpoint to review" on every wake.
//
// The two bugs those rows are evidence of:
//
//	A. a branch lock held by a permanently stopped workflow never came back;
//	B. a review was dispatched for work that had not started.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

const contendedBranch = "feat/engineering-control-center"
const contendedRepo = "/repos/agent-orchestrator"

// newQueuedBranchCoordinator is newDirectBranchCoordinator plus a wake
// scheduler, which the queue tests need: "does the queued run get told the
// branch is free" is only answerable if wakes are observable.
func newQueuedBranchCoordinator(t *testing.T, spawner *fakeSpawner, locks *fakeBranchLocks, wakeSched *fakeWakeScheduler) (*workflowcore.Coordinator, *fakeStore) {
	t.Helper()
	store := newFakeStore()
	clk := &fakeClock{t: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)}
	facts := newFakeSessionFacts()
	spawner.facts = facts
	locks.targets["proj"] = []branchTarget{{repoPath: contendedRepo, branch: contendedBranch}}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store:          store,
		Projects:       fakeProjects{"proj": directProject("proj", contendedRepo, contendedBranch)},
		Spawner:        spawner,
		SessionFacts:   facts,
		WorkspaceFacts: &fakeWorkspaceFacts{},
		BranchLocks:    locks,
		WakeScheduler:  wakeSched,
		// The reviewer stack is wired on purpose: dispatchReviewStep no-ops
		// early when it has no launcher, so a fixture without one could not
		// reach — and therefore could not disprove — the false-ambiguity path
		// this file exists to pin down.
		ReviewRuns:       newFakeReviewRuns(),
		ReviewerLauncher: &fakeReviewerLauncher{},
		Clock:          clk.Now,
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("id%d", idSeq)
		},
	})
	return c, store
}

// seedStoppedHolder reproduces wf-3220567f: a run that worked on the branch and
// then stopped for a decision only a human can make. withWorkSession controls
// the one fact that decides whether its lock is protecting anything.
func seedStoppedHolder(t *testing.T, c *workflowcore.Coordinator, store *fakeStore, spawner *fakeSpawner, stopPhase string, withWorkSession bool) string {
	t.Helper()
	ctx := context.Background()
	holder, err := c.CreateRun(ctx, "proj", "the workflow that holds the branch")
	if err != nil {
		t.Fatalf("CreateRun holder: %v", err)
	}
	if withWorkSession {
		if _, err := c.StartRun(ctx, holder.Run.ID); err != nil {
			t.Fatalf("StartRun holder: %v", err)
		}
	} else {
		// No work session ever attached: the run took the branch and stopped
		// before writing anything to it.
		if _, err := c.StartRun(ctx, holder.Run.ID); err != nil {
			t.Fatalf("StartRun holder: %v", err)
		}
		clearWorkSessions(t, store, holder.Run.ID)
		spawner.calls = 0
	}
	run, _, err := store.GetWorkflowRun(ctx, holder.Run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRun holder: %v", err)
	}
	if _, err := store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, time.Now().UTC()); err != nil {
		t.Fatalf("park holder in needs_attention: %v", err)
	}
	if _, err := store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-holder-stop", WorkflowRunID: run.ID, ProjectID: "proj",
		NextAction: "the holder stopped", DurablePhase: stopPhase,
		PayloadVersion: "v1", RetryState: "{}", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("record holder stop: %v", err)
	}
	return holder.Run.ID
}

// clearWorkSessions strips the session linkage a dispatch wrote, modeling a run
// that never got a worker onto the branch.
func clearWorkSessions(t *testing.T, store *fakeStore, runID string) {
	t.Helper()
	ctx := context.Background()
	steps, err := store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		t.Fatalf("ListWorkflowSteps: %v", err)
	}
	for i := range steps {
		steps[i].SessionID = nil
	}
	store.steps[runID] = steps
	store.checkpoints[runID] = nil
}

func hasCheckpointPhase(t *testing.T, store *fakeStore, runID, phase string) bool {
	t.Helper()
	cps, err := store.ListWorkflowCheckpoints(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	for _, cp := range cps {
		if cp.DurablePhase == phase {
			return true
		}
	}
	return false
}

func stepState(t *testing.T, store *fakeStore, runID string, kind domain.WorkflowStepKind) domain.WorkflowStepState {
	t.Helper()
	steps, err := store.ListWorkflowSteps(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListWorkflowSteps: %v", err)
	}
	for _, s := range steps {
		if s.Kind == kind {
			return s.State
		}
	}
	t.Fatalf("run %s has no %s step", runID, kind)
	return ""
}

// BUG B, exactly as it happened: a run whose work step is still `ready` because
// the branch is taken must not enter review dispatch at all. Its review is not
// ambiguous — it is not due.
func TestQueuedRunNeverReportsReviewAmbiguityWhileWorkWaitsForBranch(t *testing.T) {
	locks := newFakeBranchLocks()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: contendedBranch, WorkspacePath: contendedRepo}}}
	wakeSched := newFakeWakeScheduler()
	c, store := newQueuedBranchCoordinator(t, spawner, locks, wakeSched)
	ctx := context.Background()

	holderID := seedStoppedHolder(t, c, store, spawner, workflowcore.ReasonFixBudgetExhausted, true)

	queued, err := c.CreateRun(ctx, "proj", "the queued child")
	if err != nil {
		t.Fatalf("CreateRun queued: %v", err)
	}
	if _, err := c.StartRun(ctx, queued.Run.ID); err != nil {
		t.Fatalf("StartRun queued: %v", err)
	}

	// Three wake-driven resumes, exactly what the poller did against the real
	// database — each one previously appended another false ambiguity.
	for i := 0; i < 3; i++ {
		if _, err := c.ContinueRun(ctx, queued.Run.ID); err != nil {
			t.Fatalf("ContinueRun %d: %v", i, err)
		}
	}

	if hasCheckpointPhase(t, store, queued.Run.ID, "review_dispatch_ambiguous") {
		t.Fatal("a run whose work never started reported an ambiguous review")
	}
	detail, err := c.GetRun(ctx, queued.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if detail.Run.State != domain.WorkflowRunWaiting {
		t.Fatalf("run state = %q, want waiting", detail.Run.State)
	}
	if got := stepState(t, store, queued.Run.ID, domain.WorkflowStepWork); got != domain.WorkflowStepReady {
		t.Fatalf("work step = %q, want ready (never dispatched: the branch is taken)", got)
	}
	if got := stepState(t, store, queued.Run.ID, domain.WorkflowStepReview); got != domain.WorkflowStepPending {
		t.Fatalf("review step = %q, want pending", got)
	}
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail, Questions: detail.Questions})
	if life.Phase != workflowcore.PhaseBlocked {
		t.Fatalf("phase = %q, want blocked", life.Phase)
	}
	if life.Attention == workflowcore.AttentionHuman {
		t.Fatalf("a run queued behind a branch asked for a human decision: %#v", life)
	}
	if !wakeSched.scheduled[queued.Run.ID] {
		t.Fatal("no durable wake left behind: the queued run would never resume")
	}
	if detail.BranchWait == nil || detail.BranchWait.HeldByWorkflowRunID != holderID {
		t.Fatalf("branch wait = %#v, want the real holder named", detail.BranchWait)
	}
}

// BUG A: the lock of a permanently stopped workflow that is protecting nothing
// is reclaimed by the next run that actually needs the branch — within one
// daemon lifetime, with no restart and no timer.
func TestQueuedRunReclaimsBranchFromStoppedHolderWithNothingToProtect(t *testing.T) {
	locks := newFakeBranchLocks()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: contendedBranch, WorkspacePath: contendedRepo}}}
	c, store := newQueuedBranchCoordinator(t, spawner, locks, newFakeWakeScheduler())
	ctx := context.Background()

	holderID := seedStoppedHolder(t, c, store, spawner, workflowcore.ReasonFixBudgetExhausted, false)
	// The real branchlock.Manager decides staleness (see
	// branchlock/retention_test.go); here the fake is told the outcome so this
	// test stays about what the coordinator does with it.
	locks.staleRuns[holderID] = true

	queued, err := c.CreateRun(ctx, "proj", "the queued child")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	detail, err := c.StartRun(ctx, queued.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	if len(locks.recoverCalls) == 0 || locks.recoverCalls[0] != holderID {
		t.Fatalf("recover calls = %v, want the blocking holder %q to have been evaluated", locks.recoverCalls, holderID)
	}
	if spawner.calls != 1 {
		t.Fatalf("spawner calls = %d, want the queued run to have dispatched once the branch was reclaimed", spawner.calls)
	}
	if detail.Run.State != domain.WorkflowRunRunning {
		t.Fatalf("run state = %q, want running", detail.Run.State)
	}
	held, _ := locks.HeldByRun(ctx, queued.Run.ID)
	if len(held) != 1 {
		t.Fatalf("queued run holds %d locks, want 1", len(held))
	}
}

// The other half of the same rule: a stopped holder that DID write to the
// branch keeps it. Releasing that lock would let a second workflow start
// writing on top of uncommitted work nobody has looked at yet.
func TestStoppedHolderProtectingWorkKeepsTheBranch(t *testing.T) {
	locks := newFakeBranchLocks()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: contendedBranch, WorkspacePath: contendedRepo}}}
	c, store := newQueuedBranchCoordinator(t, spawner, locks, newFakeWakeScheduler())
	ctx := context.Background()

	holderID := seedStoppedHolder(t, c, store, spawner, workflowcore.ReasonFixBudgetExhausted, true)
	// staleRuns deliberately left empty: the manager refused to release it.

	queued, _ := c.CreateRun(ctx, "proj", "the queued child")
	detail, err := c.StartRun(ctx, queued.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if spawner.calls != 1 {
		t.Fatalf("spawner calls = %d, want only the holder's original dispatch", spawner.calls)
	}
	if detail.Run.State != domain.WorkflowRunWaiting {
		t.Fatalf("run state = %q, want waiting", detail.Run.State)
	}
	held, _ := locks.HeldByRun(ctx, holderID)
	if len(held) != 1 {
		t.Fatalf("holder holds %d locks, want to still own the branch it wrote to", len(held))
	}
}

// A queued run has to be able to say whether its wait is going anywhere. Both
// answers are facts about the holder, resolved at read time.
func TestBranchWaitReportsWhetherItClearsByItself(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stopPhase  string
		autoResume bool
		mentions   string
	}{
		{
			name:      "human decision over uncommitted work",
			stopPhase: workflowcore.ReasonFixBudgetExhausted,
			mentions:  "human decision",
		},
		{
			name:       "AO is retrying the holder",
			stopPhase:  workflowcore.ReasonPlannerRetryScheduled,
			autoResume: true,
			mentions:   "resumes by itself",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			locks := newFakeBranchLocks()
			spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: contendedBranch, WorkspacePath: contendedRepo}}}
			c, store := newQueuedBranchCoordinator(t, spawner, locks, newFakeWakeScheduler())
			ctx := context.Background()

			seedStoppedHolder(t, c, store, spawner, tc.stopPhase, true)
			queued, _ := c.CreateRun(ctx, "proj", "the queued child")
			if _, err := c.StartRun(ctx, queued.Run.ID); err != nil {
				t.Fatalf("StartRun: %v", err)
			}
			detail, err := c.GetRun(ctx, queued.Run.ID)
			if err != nil {
				t.Fatalf("GetRun: %v", err)
			}
			if detail.BranchWait == nil {
				t.Fatal("no branch wait surfaced for a queued run")
			}
			if detail.BranchWait.HeldByState != string(domain.WorkflowRunNeedsAttention) {
				t.Fatalf("heldByState = %q, want needs_attention", detail.BranchWait.HeldByState)
			}
			if detail.BranchWait.AutoResume != tc.autoResume {
				t.Fatalf("autoResume = %v, want %v (reason: %q)", detail.BranchWait.AutoResume, tc.autoResume, detail.BranchWait.HeldByReason)
			}
			if !strings.Contains(detail.BranchWait.HeldByReason, tc.mentions) {
				t.Fatalf("heldByReason = %q, want it to mention %q", detail.BranchWait.HeldByReason, tc.mentions)
			}
		})
	}
}

// Cancelling the workflow that holds a branch frees it AND tells the queue,
// rather than leaving the next run to sit out the rest of its backoff.
func TestCancellingTheHolderWakesTheQueuedRun(t *testing.T) {
	locks := newFakeBranchLocks()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: contendedBranch, WorkspacePath: contendedRepo}}}
	wakeSched := newFakeWakeScheduler()
	c, store := newQueuedBranchCoordinator(t, spawner, locks, wakeSched)
	ctx := context.Background()

	holderID := seedStoppedHolder(t, c, store, spawner, workflowcore.ReasonFixBudgetExhausted, true)
	queued, _ := c.CreateRun(ctx, "proj", "the queued child")
	if _, err := c.StartRun(ctx, queued.Run.ID); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	if _, err := c.CancelRun(ctx, holderID); err != nil {
		t.Fatalf("CancelRun holder: %v", err)
	}

	if len(locks.releases) == 0 {
		t.Fatal("cancelling the holder did not release its branch lock")
	}
	found := false
	for _, id := range wakeSched.wokenNow {
		if id == queued.Run.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("woken now = %v, want the queued run %q told the branch is free", wakeSched.wokenNow, queued.Run.ID)
	}
	// And the freed branch is genuinely takeable on that resume.
	if _, err := c.ContinueRun(ctx, queued.Run.ID); err != nil {
		t.Fatalf("ContinueRun queued: %v", err)
	}
	if spawner.calls != 2 {
		t.Fatalf("spawner calls = %d, want the queued run to have dispatched after the release", spawner.calls)
	}
}
