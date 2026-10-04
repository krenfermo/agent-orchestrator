package workflow_test

// Incident wf-57f90ff2 (2026-08-23), both halves of it.
//
// Durable state at the stop:
//
//	work=completed  review=waiting  fix=waiting  verify=pending  advance=pending
//	run=needs_attention
//	checkpoints: fix_dispatched(18:43:59) -> fix_observed_waiting(18:45:15)
//	             -> fix_no_verifiable_change(18:45:15)
//	             "fix worker idle with no verifiable new change — needs human review"
//
// The session those checkpoints were written about, `agent-orchestrator-29`:
//
//	latest_user_prompt   = "...(fix cycle 2)"   <- the cycle HAD been delivered
//	activity_state       = idle
//	activity_last_at     = 18:35:33Z            <- cycle 1
//	turn_completed_at    = 18:35:34Z            <- cycle 1
//	first_signal_at      = 16:06:01Z            <- the work step
//
// Every activity fact on the row predated the dispatch by eight minutes. AO
// read them as this cycle's outcome seventy-six seconds after dispatching it,
// and stopped the run saying the worker had gone idle without changing
// anything. The worker had never started. Twenty-eight minutes and several
// presses of "Reanudar" later, the run had not written a single checkpoint:
// the resulting shape is a closed dead end that ContinueRun had no entry point
// for (see fix_cycle_resume.go for why each cascade branch no-ops).
//
// These tests pin both halves — the stop AO is no longer allowed to reach, and
// the resume that must now reach the state it left behind — including the
// legacy on-disk reason, which is what the incident's own rows carry.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// ---- half 1: the stop AO may no longer reach --------------------------------

// The incident's exact timing: a cycle dispatched, judged 76 seconds later off
// activity clocks that all predate it. That must produce no verdict at all.
func TestStaleActivityIsNotAVerdictAboutAFreshFixCycle(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()
	f.silentSinceBeforeDispatch()

	f.clk.Advance(76 * time.Second)
	got := f.poll(1)

	if n := f.countCheckpointPhase(workflowcore.ReasonFixNoVerifiableChange); n != 0 {
		t.Fatalf("fix_no_verifiable_change checkpoints = %d, want 0: AO judged a cycle the worker had not started", n)
	}
	if n := f.countCheckpointPhase(workflowcore.ReasonFixCycleNotStarted); n != 0 {
		t.Fatalf("fix_cycle_not_started checkpoints = %d, want 0 this early: the grace window had not elapsed", n)
	}
	if f.runState() == domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q: a worker that has not yet started is not a human decision", f.runState())
	}
	if st := fixStepFrom(got).Step.State; st != domain.WorkflowStepRunning {
		t.Fatalf("fix step state = %q, want running: the cycle is still outstanding", st)
	}
}

// Past the grace window the silence IS a fact, and it gets its own name. The
// distinction is the point: fix_no_verifiable_change asserts the worker ran and
// produced nothing, which AO cannot support here.
func TestUnstartedFixCycleStopsAsNotStartedNotAsNoVerifiableChange(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()
	f.silentSinceBeforeDispatch()

	f.clk.Advance(11 * time.Minute)
	got := f.poll(1)

	if n := f.countCheckpointPhase(workflowcore.ReasonFixNoVerifiableChange); n != 0 {
		t.Fatalf("fix_no_verifiable_change checkpoints = %d, want 0", n)
	}
	if n := f.countCheckpointPhase(workflowcore.ReasonFixCycleNotStarted); n != 1 {
		t.Fatalf("fix_cycle_not_started checkpoints = %d, want exactly 1", n)
	}
	if f.runState() != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", f.runState())
	}
	if st := fixStepFrom(got).Step.State; st != domain.WorkflowStepWaiting {
		t.Fatalf("fix step state = %q, want waiting", st)
	}
	note := f.latestCheckpointPhaseNextAction(workflowcore.ReasonFixCycleNotStarted)
	if !strings.Contains(note, "never started this cycle") {
		t.Fatalf("stop detail = %q, want it to say the worker never started the cycle", note)
	}
	if strings.Contains(note, "no verifiable") {
		t.Fatalf("stop detail = %q, must not claim anything about what the worker produced", note)
	}
}

// The guard must not swallow the real stop. A worker that demonstrably DID work
// on this cycle and produced nothing still reports exactly what it always did.
func TestStartedFixCycleWithNoChangeStillReportsNoVerifiableChange(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()
	dispatchedAt := f.intentCreatedAt()
	f.mutateSession(func(rec *domain.SessionRecord) {
		rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: dispatchedAt.Add(time.Minute)}
		rec.TurnCompletedAt = dispatchedAt.Add(time.Minute)
		rec.FirstSignalAt = dispatchedAt.Add(-time.Hour)
	})

	f.clk.Advance(2 * time.Minute)
	f.poll(1)

	if n := f.countCheckpointPhase(workflowcore.ReasonFixNoVerifiableChange); n != 1 {
		t.Fatalf("fix_no_verifiable_change checkpoints = %d, want exactly 1: the genuine stop was lost", n)
	}
	if n := f.countCheckpointPhase(workflowcore.ReasonFixCycleNotStarted); n != 0 {
		t.Fatalf("fix_cycle_not_started checkpoints = %d, want 0: this worker did start", n)
	}
}

// ---- half 2: Continue must reach the state the stop left behind -------------

// The incident's own durable reason, written by a daemon that predates
// fix_cycle_not_started. Those rows are on disk and must be recoverable without
// a data migration — this is wf-57f90ff2 itself.
func TestContinueRedeliversALegacyNoVerifiableChangeStop(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()
	f.silentSinceBeforeDispatch()
	f.parkAsFixStop(workflowcore.ReasonFixNoVerifiableChange,
		"fix worker idle with no verifiable new change — needs human review")
	f.sender.calls = 0

	got := f.continueRun()

	if f.sender.calls != 1 {
		t.Fatalf("MessageSender.Send calls = %d, want exactly 1: Continue was a no-op again", f.sender.calls)
	}
	if f.runState() == domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want the stop released", f.runState())
	}
	if st := fixStepFrom(got).Step.State; st != domain.WorkflowStepRunning {
		t.Fatalf("fix step state = %q, want running", st)
	}
	if n := f.countCheckpointPhase("fix_cycle_redelivery"); n != 1 {
		t.Fatalf("fix_cycle_redelivery checkpoints = %d, want exactly 1", n)
	}
	if n := f.countCheckpointPhase("attention_cleared"); n != 1 {
		t.Fatalf("attention_cleared checkpoints = %d, want exactly 1", n)
	}
	// The re-delivery is the SAME cycle: no new attempt row, no new fix cycle,
	// no fix budget spent.
	if n := f.fixAttempts(); n != 1 {
		t.Fatalf("fix step attempts = %d, want still 1: a re-delivery opened a new cycle", n)
	}
	if !strings.Contains(f.sender.lastMsg, "fix cycle 1") {
		t.Fatalf("re-delivered prompt does not name the original cycle:\n%s", f.sender.lastMsg)
	}
}

// The same, from the reason the fixed detector now writes.
func TestContinueRedeliversAnUnstartedFixCycle(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()
	f.silentSinceBeforeDispatch()
	f.clk.Advance(11 * time.Minute)
	f.poll(1)
	if f.runState() != domain.WorkflowRunNeedsAttention {
		t.Fatalf("setup: run state = %q, want the run stopped", f.runState())
	}
	f.sender.calls = 0

	f.continueRun()

	if f.sender.calls != 1 {
		t.Fatalf("MessageSender.Send calls = %d, want exactly 1", f.sender.calls)
	}
	if f.runState() == domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want the stop released", f.runState())
	}
	if n := f.fixAttempts(); n != 1 {
		t.Fatalf("fix step attempts = %d, want still 1", n)
	}
}

// Bounded, and bounded durably: the budget is counted from checkpoints, so a
// daemon that restarts mid-recovery does not get a fresh allowance.
func TestFixCycleRedeliveryIsBoundedAndSurvivesRestart(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()
	f.sender.calls = 0

	sends := 0
	for i := 0; i < 6; i++ {
		f.silentSinceBeforeDispatch()
		f.parkAsFixStop(workflowcore.ReasonFixCycleNotStarted, "the worker never started this cycle")
		if i == 3 {
			// A daemon restart over the same rows, mid-recovery.
			f.c = f.restart()
		}
		f.continueRun()
		sends = f.sender.calls
	}

	if sends != 2 {
		t.Fatalf("MessageSender.Send calls = %d, want exactly %d (maxFixCycleRedeliveries)", sends, 2)
	}
	if n := f.countCheckpointPhase("fix_cycle_redelivery"); n != 2 {
		t.Fatalf("fix_cycle_redelivery checkpoints = %d, want exactly 2", n)
	}
	if f.runState() != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention once the budget is spent", f.runState())
	}
	if n := f.fixAttempts(); n != 1 {
		t.Fatalf("fix step attempts = %d, want still 1 across every re-delivery", n)
	}
}

// ---- the four refusals ------------------------------------------------------

// Evidence is re-derived at Continue time, not trusted from the stop: a worker
// that has since woken up is already acting on these findings.
func TestContinueDoesNotRedeliverWhenTheWorkerDidStart(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()
	dispatchedAt := f.intentCreatedAt()
	f.parkAsFixStop(workflowcore.ReasonFixCycleNotStarted, "recorded before the worker woke up")
	f.mutateSession(func(rec *domain.SessionRecord) {
		rec.Activity = domain.Activity{State: domain.ActivityActive, LastActivityAt: dispatchedAt.Add(time.Minute)}
	})
	f.sender.calls = 0

	f.continueRun()

	if f.sender.calls != 0 {
		t.Fatalf("MessageSender.Send calls = %d, want 0: the worker was already on it", f.sender.calls)
	}
	if n := f.countCheckpointPhase("fix_cycle_redelivery"); n != 0 {
		t.Fatalf("fix_cycle_redelivery checkpoints = %d, want 0", n)
	}
}

// Never re-deliver over work that exists. Whose work it is would be a guess.
func TestContinueDoesNotRedeliverOverANewFingerprint(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()
	f.silentSinceBeforeDispatch()
	f.parkAsFixStop(workflowcore.ReasonFixCycleNotStarted, "the worker never started this cycle")
	f.workspaceFacts.obs.HeadSHA = "a-different-head"
	f.sender.calls = 0

	f.continueRun()

	if f.sender.calls != 0 {
		t.Fatalf("MessageSender.Send calls = %d, want 0: the workspace had changed", f.sender.calls)
	}
	if f.runState() != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention: nothing was proven", f.runState())
	}
}

// A dead session cannot be re-delivered to, and pretending otherwise would
// leave the run running with nothing behind it.
func TestContinueDoesNotRedeliverToATerminatedSession(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()
	f.silentSinceBeforeDispatch()
	f.parkAsFixStop(workflowcore.ReasonFixCycleNotStarted, "the worker never started this cycle")
	f.mutateSession(func(rec *domain.SessionRecord) { rec.IsTerminated = true })
	f.sender.calls = 0

	f.continueRun()

	if f.sender.calls != 0 {
		t.Fatalf("MessageSender.Send calls = %d, want 0", f.sender.calls)
	}
	if f.runState() != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", f.runState())
	}
}

// A run stopped for anything else is left exactly where it is.
func TestContinueDoesNotRedeliverForAnUnrelatedStop(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()
	f.silentSinceBeforeDispatch()
	f.parkAsFixStop(workflowcore.ReasonFixBudgetExhausted,
		"the reviewer still requests changes after every allowed fix cycle")
	f.sender.calls = 0

	f.continueRun()

	if f.sender.calls != 0 {
		t.Fatalf("MessageSender.Send calls = %d, want 0: an unrelated stop was resumed", f.sender.calls)
	}
	if n := f.countCheckpointPhase("attention_cleared"); n != 0 {
		t.Fatalf("attention_cleared checkpoints = %d, want 0", n)
	}
}

// A read poll must never re-deliver. Only the explicit human/API Continue may.
func TestPollingNeverRedeliversAFixCycle(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()
	f.silentSinceBeforeDispatch()
	f.parkAsFixStop(workflowcore.ReasonFixCycleNotStarted, "the worker never started this cycle")
	f.sender.calls = 0

	f.poll(10)

	if f.sender.calls != 0 {
		t.Fatalf("MessageSender.Send calls from polling = %d, want 0", f.sender.calls)
	}
	if n := f.countCheckpointPhase("fix_cycle_redelivery"); n != 0 {
		t.Fatalf("fix_cycle_redelivery checkpoints from polling = %d, want 0", n)
	}
}

// ---- fixture helpers --------------------------------------------------------

// silentSinceBeforeDispatch reproduces session agent-orchestrator-29 as it was
// at the moment of the incident: the cycle's prompt is in the row, and every
// activity fact on it belongs to the PREVIOUS cycle.
func (f *fixRecoveryFixture) silentSinceBeforeDispatch() {
	f.t.Helper()
	dispatchedAt := f.intentCreatedAt()
	f.mutateSession(func(rec *domain.SessionRecord) {
		rec.Metadata.LatestUserPrompt = f.sender.lastMsg
		rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: dispatchedAt.Add(-8 * time.Minute)}
		rec.TurnCompletedAt = dispatchedAt.Add(-8 * time.Minute)
		rec.FirstSignalAt = dispatchedAt.Add(-90 * time.Minute)
		rec.IsTerminated = false
	})
}

// parkAsFixStop writes the durable shape a fix stop leaves behind: the step
// resting at waiting, the run in needs_attention, and the reason recorded as
// the run's newest checkpoint — which is where stopReason reads it from.
func (f *fixRecoveryFixture) parkAsFixStop(reason, detail string) {
	f.t.Helper()
	ctx := context.Background()
	steps, err := f.store.ListWorkflowSteps(ctx, f.runID)
	if err != nil {
		f.t.Fatalf("ListWorkflowSteps: %v", err)
	}
	var fixState domain.WorkflowStepState
	for _, s := range steps {
		if s.ID == f.fixStepID {
			fixState = s.State
		}
	}
	f.clk.Advance(time.Second)
	if fixState == domain.WorkflowStepRunning || fixState == domain.WorkflowStepReady {
		if _, err := f.store.UpdateWorkflowStepState(ctx, f.fixStepID, fixState, domain.WorkflowStepWaiting, f.clk.Now()); err != nil {
			f.t.Fatalf("park fix step: %v", err)
		}
	}
	run, ok, err := f.store.GetWorkflowRun(ctx, f.runID)
	if err != nil || !ok {
		f.t.Fatalf("GetWorkflowRun: %v (found=%v)", err, ok)
	}
	if run.State != domain.WorkflowRunNeedsAttention {
		if _, err := f.store.UpdateWorkflowRunState(ctx, f.runID, run.State, domain.WorkflowRunNeedsAttention, f.clk.Now()); err != nil {
			f.t.Fatalf("park run: %v", err)
		}
	}
	f.clk.Advance(time.Second)
	stepID := f.fixStepID
	if _, err := f.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-stop-" + reason + f.clk.Now().Format("150405.000000000"), WorkflowRunID: f.runID,
		WorkflowStepID: &stepID, ProjectID: run.ProjectID,
		NextAction: detail, DurablePhase: reason,
		PayloadVersion: "v1", RetryState: "{}", CreatedAt: f.clk.Now(),
	}); err != nil {
		f.t.Fatalf("CreateWorkflowCheckpoint: %v", err)
	}
}

func (f *fixRecoveryFixture) continueRun() workflowcore.RunDetail {
	f.t.Helper()
	f.clk.Advance(2 * time.Second)
	got, err := f.c.ContinueRun(context.Background(), f.runID)
	if err != nil {
		f.t.Fatalf("ContinueRun: %v", err)
	}
	return got
}

func (f *fixRecoveryFixture) fixAttempts() int {
	f.t.Helper()
	attempts, err := f.store.ListWorkflowAttempts(context.Background(), f.fixStepID)
	if err != nil {
		f.t.Fatalf("ListWorkflowAttempts: %v", err)
	}
	return len(attempts)
}
