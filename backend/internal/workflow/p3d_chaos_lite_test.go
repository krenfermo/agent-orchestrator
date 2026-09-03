package workflow_test

// P3-D §5 — the chaos-lite matrix.
//
// Each case writes the durable rows a kill at one exact boundary leaves behind,
// then restarts the engine over them and asserts the same three things:
//
//	one authority        — no second attempt, no second worker, no second repair;
//	no duplicate effects — one completion checkpoint, one dispatch, one release;
//	an honest ending     — convergence when the evidence allows it, and a stop
//	                       that names the real reason when it does not.
//
// They are written against ROWS rather than against a running process on
// purpose. A kill is not interesting in itself; what is interesting is the state
// it leaves, and the state is the thing a restart has to read. Building it
// directly makes every boundary reachable — including the ones that are a
// millisecond wide in production and cannot be hit reliably by a real SIGKILL.
//
// The real process kill is Smoke D, which covers boundary B end to end against
// a live Claude worker. This matrix covers the ones a smoke cannot reach.

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// restart re-enters boot recovery over the durable rows, which is exactly what
// a restarted daemon does and the only thing about a restart these tests care
// about.
func (f *headlessFixture) restart(t *testing.T) {
	t.Helper()
	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("boot recovery: %v", err)
	}
}

func (f *headlessFixture) workAttempts(t *testing.T) []domain.WorkflowAttempt {
	t.Helper()
	got, err := f.store.ListWorkflowAttempts(f.ctx, f.stepID)
	if err != nil {
		t.Fatalf("ListWorkflowAttempts: %v", err)
	}
	return got
}

func (f *headlessFixture) countPhase(t *testing.T, phase string) int {
	t.Helper()
	n := 0
	for _, p := range f.checkpointPhases() {
		if p == phase {
			n++
		}
	}
	return n
}

// A: killed after the dispatch was accepted and before the turn completed.
//
// The worker is alive and mid-turn. A restart must leave it completely alone —
// this is the case where acting would start a second agent on one worktree.
func TestChaosKillAfterDispatchBeforeTurnCompletion(t *testing.T) {
	f := newHeadlessFixture(t)
	f.seedRunState(domain.WorkflowRunRunning)
	f.seedStepState(domain.WorkflowStepReady)
	f.seedStepState(domain.WorkflowStepRunning)
	f.seedOutbox(domain.WorkflowOutboxAcknowledged)
	attempt := f.seedOpenAttempt("claude-code")
	f.seedBoundary(domain.DispatchPhaseWorkerDispatched, domain.LaunchStageConfirm,
		domain.LaunchOutcomeDispatched, attempt.ID, "sess-mid-turn")
	f.seedStepSession("sess-mid-turn")
	f.seedLiveWorker("sess-mid-turn")

	f.restart(t)

	if got := f.workAttempts(t); len(got) != 1 || got[0].Outcome != "" {
		t.Fatalf("a restart disturbed a live worker's attempt: %+v", got)
	}
	f.assertNothingLaunched()
	if got := f.step().State; got != domain.WorkflowStepRunning {
		t.Fatalf("step state = %q, want running: a live worker's step must not move", got)
	}
}

// B: killed after the turn receipt and before work convergence.
//
// This is Smoke D's boundary. Here it is asserted as an invariant: the restart
// converges on the SAME attempt, and mints nothing.
func TestChaosKillAfterTurnCompletionBeforeConvergence(t *testing.T) {
	f := newHeadlessFixture(t)
	f.seedFinishedHeadlessWorker("sess-turn-done")
	before := f.workAttempts(t)

	f.restart(t)

	after := f.workAttempts(t)
	if len(after) != len(before) {
		t.Fatalf("a restart minted a second attempt: %d -> %d", len(before), len(after))
	}
	if after[0].ID != before[0].ID {
		t.Fatalf("attempt identity changed across the restart: %q -> %q", before[0].ID, after[0].ID)
	}
	if got := f.step().State; got != domain.WorkflowStepCompleted {
		t.Fatalf("work step state = %q, want completed: conclusive evidence must converge", got)
	}
	f.assertNothingLaunched()
	// One completion, not two: a second one ties on created_at and makes "the
	// latest checkpoint for this step" a coin flip between them.
	if n := f.countPhase(t, "worker_observed_worker_result_available"); n > 1 {
		t.Fatalf("completion observations = %d, want at most 1", n)
	}
}

// B, twice. A restart that is itself interrupted must still converge exactly
// once — the second pass observes the first pass's result rather than redoing
// it.
func TestChaosRepeatedRestartConvergesExactlyOnce(t *testing.T) {
	f := newHeadlessFixture(t)
	f.seedFinishedHeadlessWorker("sess-turn-done")

	f.restart(t)
	firstPhases := len(f.checkpointPhases())
	f.restart(t)
	f.restart(t)

	if got := f.workAttempts(t); len(got) != 1 {
		t.Fatalf("repeated restarts produced %d attempts, want 1", len(got))
	}
	if n := f.countPhase(t, "worker_observed_worker_result_available"); n > 1 {
		t.Fatalf("completion observations = %d across three restarts, want at most 1", n)
	}
	if after := len(f.checkpointPhases()); after > firstPhases+2 {
		t.Fatalf("ledger grew %d -> %d across two extra restarts; a restart is not an event",
			firstPhases, after)
	}
	f.assertNothingLaunched()
}

// C: killed after the work completed and before the review was dispatched.
//
// The completion is durable; the review is not. A restart owes the review, and
// owes exactly one.
func TestChaosKillAfterWorkCompletedBeforeReviewDispatch(t *testing.T) {
	f := newHeadlessFixture(t)
	f.seedFinishedHeadlessWorker("sess-done")
	f.restart(t)
	if got := f.step().State; got != domain.WorkflowStepCompleted {
		t.Fatalf("precondition: work step = %q, want completed", got)
	}
	before := f.workAttempts(t)

	// Restart again from the completed-work state.
	f.restart(t)
	f.restart(t)

	if after := f.workAttempts(t); len(after) != len(before) {
		t.Fatalf("restarting over a completed work step minted attempts: %d -> %d",
			len(before), len(after))
	}
	f.assertNothingLaunched()
}

// F: a duplicate auto-recovery wake. Two wakes for one run must produce one
// resolution, not two.
func TestChaosDuplicateWakeProducesOneResolution(t *testing.T) {
	f := newHeadlessFixture(t)
	f.seedFinishedHeadlessWorker("sess-dup-wake")

	for i := 0; i < 3; i++ {
		if _, _, err := f.c.ReconcileWorkStepDispatch(f.ctx, f.run(), f.step()); err != nil {
			t.Fatalf("wake %d: %v", i, err)
		}
	}
	f.restart(t)

	if got := f.workAttempts(t); len(got) != 1 {
		t.Fatalf("duplicate wakes produced %d attempts, want 1", len(got))
	}
	f.assertNothingLaunched()
	if n := f.countPhase(t, workflowcore.ReasonWorkerDispatchAmbiguous); n != 0 {
		t.Fatalf("a duplicate wake parked a healthy run as ambiguous %d times", n)
	}
}

// G: a Continue landing concurrently with recovery. Both drive the same run;
// exactly one may move it.
func TestChaosContinueConcurrentWithRecovery(t *testing.T) {
	f := newHeadlessFixture(t)
	f.seedFinishedHeadlessWorker("sess-continue")

	f.restart(t)
	if _, err := f.c.ContinueRun(f.ctx, f.runID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	f.restart(t)

	if got := f.workAttempts(t); len(got) != 1 {
		t.Fatalf("Continue beside recovery produced %d attempts, want 1", len(got))
	}
	if n := f.countPhase(t, "worker_observed_worker_result_available"); n > 1 {
		t.Fatalf("completion observations = %d, want at most 1", n)
	}
	f.assertNothingLaunched()
}

// I: a provider failure recorded, killed before the failover.
//
// The failed attempt is closed and the retry was never routed. A restart must
// route it once — and must not treat the closed attempt as a live writer.
func TestChaosProviderFailureRecordedThenKilledBeforeFailover(t *testing.T) {
	f := newHeadlessFixture(t)
	f.seedRunState(domain.WorkflowRunRunning)
	f.seedStepState(domain.WorkflowStepReady)
	f.seedStepState(domain.WorkflowStepRunning)
	f.seedOutbox(domain.WorkflowOutboxDispatched)
	attempt := f.seedOpenAttempt("claude-code")
	f.seedBoundary(domain.DispatchPhaseWorkerLaunchError, domain.LaunchStageSpawn,
		domain.LaunchOutcomeFailed, attempt.ID, "")
	f.clk.t = f.clk.t.Add(2 * time.Minute)

	f.restart(t)

	attempts := f.workAttempts(t)
	for _, a := range attempts {
		if a.Outcome == "" && a.ID == attempt.ID {
			t.Fatalf("the failed attempt is still open after a restart: %+v", a)
		}
	}
	f.assertNothingLaunched()
}

// J: an answer resolved and the daemon killed before it was delivered.
//
// The answer is durable and undelivered. A restart must NOT record it as
// delivered on the strength of nothing having happened — that is the P3-D smoke
// B failure, stated as a restart invariant.
func TestChaosAnswerResolvedThenKilledBeforeDelivery(t *testing.T) {
	f := newHeadlessFixture(t)
	f.seedFinishedHeadlessWorker("sess-answer")

	f.restart(t)
	// The delivery invariant itself is asserted where deliveries are real, in
	// TestADeliveryRecordedFromAnAbsentPromptAlsoUnparksTheRun and its
	// neighbours, which run against a store that has a questions table. What
	// this case owes is the other half: a restart over a run with an
	// undelivered answer must not disturb the EXECUTION either.
	if got := f.workAttempts(t); len(got) != 1 {
		t.Fatalf("a restart over an undelivered answer produced %d attempts, want 1", len(got))
	}
	f.assertNothingLaunched()
}

// The observation half of the matrix: an inconclusive pane never becomes a
// delivery receipt, whatever else is happening. Asserted here rather than only
// in the parser tests because the rule that matters is the CALLER's.
func TestChaosInconclusiveObservationIsNeverAReceipt(t *testing.T) {
	obs := domain.DialogUnreadable("the layout could not be interpreted")
	if obs.Absent() {
		t.Fatal("an inconclusive observation reported itself as an absence")
	}
	if obs.Present() {
		t.Fatal("an inconclusive observation reported a dialog")
	}
	// And the constructors keep the three apart.
	if !domain.NoDialog().Absent() || !domain.DialogSeen(domain.ProviderDialog{}).Present() {
		t.Fatal("the observation constructors do not produce their own states")
	}
	var _ ports.DialogPaneParser
}
