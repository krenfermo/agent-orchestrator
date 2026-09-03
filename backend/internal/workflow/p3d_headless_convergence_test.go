package workflow_test

// P3-D §1/§3 — the headless worker that finished, and the reconciliation that
// called it a phantom.
//
// The incident this file reproduces was observed on a real run: a Claude Code
// worker was dispatched, it changed the repository, its turn ended, its process
// exited — and the run stopped on `worker_dispatch_ambiguous` instead of
// reaching review. It reproduced with and without a question dialog, which is
// what ruled the P3-C delivery path out as the cause.
//
// The cause is a race between two components that read the same worker through
// different eyes:
//
//   - the session ROW is marked terminated by the lifecycle reaper, and only
//     once the runtime probe says dead AND the row has had no activity for the
//     recent-activity window (lifecycle/runtime.go). A worker that JUST finished
//     has activity seconds old, so for that whole window the row still reads as
//     a live worker;
//   - the workflow's own liveness probe answers immediately, and truthfully:
//     nothing is running.
//
// Those two facts together are, verbatim, ownedExecution.PhantomRunning() — the
// contradiction dispatch reconciliation exists to close. So a wake landing in
// that window classifies a NORMAL, SUCCESSFUL finish as a phantom, closes the
// attempt, and stops the run with the evidence. The window is not a corner: the
// autonomous heartbeat fires on exactly the signal (a completed turn) that
// opens it.
//
// The distinguishing fact reconciliation was not consulting is the turn
// receipt. A session row carrying a TurnCompletedAt at or after this dispatch is
// a durable statement that the provider ran this dispatch's turn to the end —
// which is the difference between "the execution vanished under a worker that
// was still working" (a phantom) and "the execution exited because it was
// finished" (a completion, and work observation's question).

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// headlessFixture is dispatchReconcileFixture with the one thing that fixture
// hard-codes made controllable: the workspace observation. The incident is
// about a worker that LEFT A CHANGE BEHIND, so a fixture that can only report
// an empty repository cannot express it.
type headlessFixture struct {
	*dispatchReconcileFixture
	workspace *fakeWorkspaceFacts
}

func newHeadlessFixture(t *testing.T) *headlessFixture {
	t.Helper()
	store := newFakeStore()
	facts := newFakeSessionFacts()
	registry := newFakeOwnershipRegistry()
	wakes := newFakeWakeScheduler()
	clk := &fakeClock{t: time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)}
	workspace := &fakeWorkspaceFacts{}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store:            store,
		WorkerLauncher:   registry,
		SessionOwnership: registry,
		WorkerLiveness:   registry,
		SessionFacts:     facts,
		WorkspaceFacts:   workspace,
		WakeScheduler:    wakes,
		Clock:            clk.Now,
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("id%d", idSeq)
		},
	})
	base := &dispatchReconcileFixture{t: t, ctx: context.Background(), c: c, store: store,
		facts: facts, registry: registry, wakes: wakes, clk: clk}
	created, err := c.CreateRun(base.ctx, "proj-1", "let a headless worker finish")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	base.runID = created.Run.ID
	base.stepID = workStepIDOf(t, store, created.Run.ID)
	return &headlessFixture{dispatchReconcileFixture: base, workspace: workspace}
}

// seedFinishedHeadlessWorker writes the exact durable state a headless provider
// leaves behind between the end of its turn and the reaper's next pass:
//
//   - a confirmed dispatch whose session is attached to a RUNNING step;
//   - a session row that is NOT terminated and whose activity is seconds old,
//     because the reaper's recent-activity window has not elapsed;
//   - a turn-completion receipt at or after the dispatch;
//   - a runtime probe answering, correctly, that nothing is running;
//   - a repository with the worker's change in it.
func (f *headlessFixture) seedFinishedHeadlessWorker(id domain.SessionID) {
	f.t.Helper()
	f.seedRunState(domain.WorkflowRunRunning)
	f.seedStepState(domain.WorkflowStepReady)
	f.seedStepState(domain.WorkflowStepRunning)
	f.seedOutbox(domain.WorkflowOutboxAcknowledged)
	attempt := f.seedOpenAttempt("claude-code")
	f.seedBoundary(domain.DispatchPhaseWorkerDispatched, domain.LaunchStageConfirm,
		domain.LaunchOutcomeDispatched, attempt.ID, string(id))
	f.seedStepSession(id)

	turnEnded := f.clk.Now().Add(2 * time.Second)
	f.facts.put(domain.SessionRecord{
		ID: id, ProjectID: "proj-1", Harness: domain.HarnessClaudeCode,
		Kind: domain.KindWorker, IssueID: f.issueID(),
		// Not terminated, and recently active: the reaper has not run yet.
		IsTerminated:    false,
		Activity:        domain.Activity{State: domain.ActivityIdle, LastActivityAt: turnEnded},
		FirstSignalAt:   f.clk.Now().Add(time.Second),
		TurnCompletedAt: turnEnded,
		Metadata:        domain.SessionMetadata{Branch: "feat/headless", WorkspacePath: "/tmp/wt/" + string(id)},
	})
	// The process is gone, and the probe says so. This is the truth, not a
	// contradiction: the provider exited because it finished.
	f.registry.register(&fakeOwnedWorker{
		sessionID: id, dispatchKey: f.dispatchKey(), live: false, livenessKnown: true,
	})
	// And the change it made is on disk.
	f.workspace.obs = ports.WorkspaceObservation{
		HeadSHA: "sha-after-the-worker-wrote", Dirty: true,
		Changes: []ports.WorkspaceChange{{Path: "internal/thing.go", Status: "M"}},
	}
	f.clk.t = f.clk.t.Add(10 * time.Second)
}

// A headless worker that finished its turn and left a verified change is not a
// phantom, and reconciliation may not stop the run over it.
func TestAFinishedHeadlessWorkerIsNotAPhantom(t *testing.T) {
	f := newHeadlessFixture(t)
	f.seedFinishedHeadlessWorker("sess-headless")

	got := f.reconcile()
	if got.Action == workflowcore.DispatchReconcileNeedsAttention {
		t.Fatalf("reconciliation stopped a finished headless worker: %s", got.Detail)
	}
	if got.Contradiction == workflowcore.ContradictionStaleRunning {
		t.Fatalf("a completed turn was classified stale_running: %s", got.Detail)
	}
	if f.hasCheckpointPhase(workflowcore.ReasonWorkerDispatchAmbiguous) {
		t.Fatalf("the run was parked ambiguous; phases = %v", f.checkpointPhases())
	}
}

// ...and the run converges on its own: work completed, then review.
func TestAFinishedHeadlessWorkerConvergesToReview(t *testing.T) {
	f := newHeadlessFixture(t)
	f.seedFinishedHeadlessWorker("sess-headless")

	if _, _, err := f.c.ReconcileWorkStepDispatch(f.ctx, f.run(), f.step()); err != nil {
		t.Fatalf("ReconcileWorkStepDispatch: %v", err)
	}
	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := f.step().State; got != domain.WorkflowStepCompleted {
		t.Fatalf("work step state = %q, want completed; phases = %v", got, f.checkpointPhases())
	}
	if got := f.run().State; got == domain.WorkflowRunNeedsAttention {
		t.Fatalf("run needs attention after a conclusive headless completion; phases = %v", f.checkpointPhases())
	}
}
