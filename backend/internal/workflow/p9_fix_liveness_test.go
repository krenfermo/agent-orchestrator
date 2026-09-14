package workflow_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// P9 §18 / §17 — liveness of a fix cycle, and what a clock jump may conclude.
//
// A fix cycle is delivered into the worker's EXISTING session, and the fix step
// row never carries a session of its own. Before P9 nothing watched it once the
// session read `active`: a fix agent whose process died under a still-`active`
// row stayed "running" forever. The opposite error is just as real — a laptop
// that slept for three hours wakes with every clock silent over a worker that
// is perfectly alive — so silence is never the conclusion. Past the window AO
// asks the runtime, and only a PROVEN ending moves the step.

// scriptedRuntimeOwnership is the P9 port with a programmable answer per
// session. Unscripted sessions answer `unavailable`, never a fact.
type scriptedRuntimeOwnership struct {
	mu     sync.Mutex
	proofs map[domain.SessionID]domain.WorkerRuntimeProof
	calls  int
}

func newScriptedRuntimeOwnership() *scriptedRuntimeOwnership {
	return &scriptedRuntimeOwnership{proofs: map[domain.SessionID]domain.WorkerRuntimeProof{}}
}

func (s *scriptedRuntimeOwnership) set(id domain.SessionID, p domain.WorkerRuntimeProof) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.proofs[id] = p
}

func (s *scriptedRuntimeOwnership) ObserveWorkerRuntime(_ context.Context, id domain.SessionID) domain.WorkerRuntimeObservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	p, ok := s.proofs[id]
	if !ok {
		p = domain.WorkerRuntimeUnavailable
	}
	return domain.WorkerRuntimeObservation{SessionID: id, Proof: p, Detail: "scripted " + string(p)}
}

func (s *scriptedRuntimeOwnership) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// p9FixCycle drives a run to a delivered fix cycle with the runtime port wired,
// and returns the dispatch instant of that cycle.
func p9FixCycle(t *testing.T, wire bool) (*fixRecoveryFixture, *scriptedRuntimeOwnership, time.Time) {
	t.Helper()
	f := newFixRecoveryFixture(t)
	rt := newScriptedRuntimeOwnership()
	if wire {
		f.runtimeOwnership = rt
		f.c = f.newCoordinator()
	}
	detail := f.driveToFixDispatch()
	if got := fixStepFrom(detail).Step.State; got != domain.WorkflowStepRunning {
		t.Fatalf("fix step state after dispatch = %q, want running", got)
	}
	var dispatchedAt time.Time
	for _, cp := range f.store.checkpoints[f.runID] {
		if cp.DurablePhase == "fix_dispatched" && cp.CreatedAt.After(dispatchedAt) {
			dispatchedAt = cp.CreatedAt
		}
	}
	if dispatchedAt.IsZero() {
		t.Fatal("no fix_dispatched checkpoint was written")
	}
	return f, rt, dispatchedAt
}

// startedActive puts the worker session in `active`, having visibly begun this
// cycle one second after its dispatch, and last heard from at lastSignal.
func startedActive(f *fixRecoveryFixture, dispatchedAt, lastSignal time.Time) {
	f.mutateSession(func(rec *domain.SessionRecord) {
		rec.Activity.State = domain.ActivityActive
		rec.Activity.LastActivityAt = dispatchedAt.Add(time.Second)
		rec.Activity.LastSignalAt = lastSignal
		rec.IsTerminated = false
	})
}

func (f *fixRecoveryFixture) fixState() domain.WorkflowStepState {
	f.t.Helper()
	return fixStepFrom(f.poll(1)).Step.State
}

// Fix active and heard from: nothing to decide, and the runtime is not even
// asked.
func TestP9Fix_ActiveWithSignalsIsNeverStaleAndNeverProbed(t *testing.T) {
	f, rt, dispatchedAt := p9FixCycle(t, true)
	for i := 0; i < 12; i++ {
		f.clk.Advance(5 * time.Minute)
		startedActive(f, dispatchedAt, f.clk.Now())
		if got := f.fixState(); got != domain.WorkflowStepRunning {
			t.Fatalf("after %d min of signals the fix step is %q, want running", (i+1)*5, got)
		}
	}
	if rt.callCount() != 0 {
		t.Fatalf("a fix cycle that is heard from was probed %d times", rt.callCount())
	}
}

// §17: three hours of silence (a slept laptop) over a runtime that proves it is
// alive and ours. Nothing is concluded and nothing is written.
func TestP9Fix_ClockJumpOverALiveRuntimeConcludesNothing(t *testing.T) {
	f, rt, dispatchedAt := p9FixCycle(t, true)
	startedActive(f, dispatchedAt, dispatchedAt.Add(time.Second))
	rt.set(f.workSessionID, domain.WorkerRuntimeOwned)
	before := len(f.store.checkpoints[f.runID])

	f.clk.Advance(3 * time.Hour)
	for i := 0; i < 5; i++ {
		if got := f.fixState(); got != domain.WorkflowStepRunning {
			t.Fatalf("a live fix agent after a clock jump became %q", got)
		}
	}
	if rt.callCount() == 0 {
		t.Fatal("the runtime was never re-checked after the silence window")
	}
	if after := len(f.store.checkpoints[f.runID]); after != before {
		t.Fatalf("a quiet-but-alive fix cycle wrote %d checkpoints", after-before)
	}
	if f.runState() == domain.WorkflowRunNeedsAttention {
		t.Fatal("a live fix agent parked the run after a clock jump")
	}
}

// Silence over a runtime AO cannot prove anything about is also not a
// conclusion: an unknown probe is never proof a session is dead.
func TestP9Fix_UnprovableRuntimeConcludesNothing(t *testing.T) {
	for _, proof := range []domain.WorkerRuntimeProof{
		domain.WorkerRuntimeUnavailable, domain.WorkerRuntimeOwnerMismatch,
		domain.WorkerRuntimeProvenanceMissing, domain.WorkerRuntimeUnsupported,
	} {
		t.Run(string(proof), func(t *testing.T) {
			f, rt, dispatchedAt := p9FixCycle(t, true)
			startedActive(f, dispatchedAt, dispatchedAt.Add(time.Second))
			rt.set(f.workSessionID, proof)
			f.clk.Advance(time.Hour)
			if got := f.fixState(); got != domain.WorkflowStepRunning {
				t.Fatalf("an unprovable runtime moved the fix step to %q", got)
			}
		})
	}
}

// The fix agent's process is provably gone while its row still reads `active`.
// Before P9 this cycle stayed running forever; now it is concluded exactly like
// a terminated session — on workspace evidence.
func TestP9Fix_ProvenDeadRuntimeIsConcludedOnWorkspaceEvidence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		proof     domain.WorkerRuntimeProof
		changed   bool
		wantState domain.WorkflowStepState
	}{
		{"absent, change delivered", domain.WorkerRuntimeAbsent, true, domain.WorkflowStepWaiting},
		{"workload exited, no change", domain.WorkerRuntimeOwnedExited, false, domain.WorkflowStepFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, rt, dispatchedAt := p9FixCycle(t, true)
			startedActive(f, dispatchedAt, dispatchedAt.Add(time.Second))
			rt.set(f.workSessionID, tc.proof)
			if tc.changed {
				f.workspaceFacts.obs.HeadSHA = "p9-fix-produced-a-commit"
			}
			// Inside the window: not asked, not concluded.
			f.clk.Advance(10 * time.Minute)
			if got := f.fixState(); got != domain.WorkflowStepRunning {
				t.Fatalf("inside the silence window the fix step became %q", got)
			}
			f.clk.Advance(10 * time.Minute)
			if got := f.fixState(); got != tc.wantState {
				t.Fatalf("a provably dead fix runtime left the step %q, want %q", got, tc.wantState)
			}
		})
	}
}

// Without the port (a pre-P9 daemon) the behaviour is exactly the old one: an
// active fix cycle is left alone however long it is silent.
func TestP9Fix_NoPortKeepsThePreP9Behaviour(t *testing.T) {
	f, _, dispatchedAt := p9FixCycle(t, false)
	startedActive(f, dispatchedAt, dispatchedAt.Add(time.Second))
	f.clk.Advance(3 * time.Hour)
	if got := f.fixState(); got != domain.WorkflowStepRunning {
		t.Fatalf("with no runtime port the fix step became %q", got)
	}
}

// An agent that was ALREADY active keeps emitting same-state signals, which move
// only last_signal_at. Such a signal after the dispatch is the agent working, so
// the cycle must not be stopped as never started.
func TestP9Fix_ActiveSignalAfterDispatchCountsAsStarted(t *testing.T) {
	f, _, dispatchedAt := p9FixCycle(t, true)
	f.mutateSession(func(rec *domain.SessionRecord) {
		rec.Activity.State = domain.ActivityActive
		rec.Activity.LastActivityAt = dispatchedAt.Add(-time.Hour) // transition clock predates the cycle
		rec.TurnCompletedAt = time.Time{}
		rec.FirstSignalAt = dispatchedAt.Add(-2 * time.Hour)
		rec.Activity.LastSignalAt = dispatchedAt.Add(30 * time.Second)
	})
	f.clk.Advance(11 * time.Minute)
	if got := f.fixState(); got != domain.WorkflowStepRunning {
		t.Fatalf("an active agent heard from after the dispatch was stopped: %q", got)
	}
	if f.countCheckpointPhase(workflowcore.ReasonFixCycleNotStarted) != 0 {
		t.Fatal("fix_cycle_not_started was recorded over an agent that signalled after the dispatch")
	}
}

// wf-57f90ff2 must stay closed: an IDLE heartbeat after the dispatch is not the
// cycle starting, and signals from the PREVIOUS cycle never count for this one.
func TestP9Fix_IdleHeartbeatOrOldSignalsNeverStartACycle(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(rec *domain.SessionRecord, dispatchedAt time.Time)
	}{
		{"idle heartbeat after dispatch", func(rec *domain.SessionRecord, at time.Time) {
			rec.Activity.State = domain.ActivityIdle
			rec.Activity.LastActivityAt = at.Add(-time.Hour)
			rec.Activity.LastSignalAt = at.Add(time.Minute)
		}},
		{"active, but last heard before this cycle", func(rec *domain.SessionRecord, at time.Time) {
			rec.Activity.State = domain.ActivityActive
			rec.Activity.LastActivityAt = at.Add(-time.Hour)
			rec.Activity.LastSignalAt = at.Add(-time.Minute)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, dispatchedAt := p9FixCycle(t, true)
			f.mutateSession(func(rec *domain.SessionRecord) {
				rec.TurnCompletedAt = time.Time{}
				rec.FirstSignalAt = dispatchedAt.Add(-2 * time.Hour)
				tc.mutate(rec, dispatchedAt)
			})
			f.clk.Advance(11 * time.Minute)
			_ = f.poll(1)
			if f.countCheckpointPhase(workflowcore.ReasonFixCycleNotStarted) == 0 {
				t.Fatalf("a cycle with no evidence of its own was not stopped as not started; state=%q", f.fixState())
			}
		})
	}
}

// P8 debt 11: the run detail's worker clocks now cover a fix cycle, through the
// session named on the cycle's own durable dispatch record.
func TestP9Fix_LivenessViewCoversTheFixCycle(t *testing.T) {
	f, _, dispatchedAt := p9FixCycle(t, true)
	heard := dispatchedAt.Add(42 * time.Second)
	startedActive(f, dispatchedAt, heard)
	detail := f.poll(1)
	wl := detail.WorkerLiveness
	if !wl.Observed {
		t.Fatal("a running fix cycle reported no worker liveness")
	}
	if wl.StepKind != domain.WorkflowStepFix || wl.SessionID != string(f.workSessionID) {
		t.Fatalf("liveness = %+v, want the fix step over session %s", wl, f.workSessionID)
	}
	if !wl.LastSignalAt.Equal(heard) {
		t.Fatalf("last signal = %s, want %s", wl.LastSignalAt, heard)
	}
}
