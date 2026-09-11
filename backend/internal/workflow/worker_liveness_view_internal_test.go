package workflow

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// PHASE J.1 -- a worker that has been working for twenty minutes must read as
// heard-from-seconds-ago, not as silent-for-twenty-minutes.
//
// This is the projection half of the fix migration 0168 made possible. The
// session row in this test is exactly the wf-1c2cb9bd shape: a transition clock
// frozen at the instant the worker went active, and a liveness clock that has
// been moving ever since.
func TestWorkerLivenessReportsTheSignalClockNotTheTransitionClock(t *testing.T) {
	wentActiveAt := time.Date(2026, 9, 9, 17, 10, 20, 0, time.UTC)
	now := wentActiveAt.Add(20 * time.Minute)
	live := WorkerLiveness{
		Observed:         true,
		LastTransitionAt: wentActiveAt,
		LastSignalAt:     now.Add(-3 * time.Second),
		State:            domain.ActivityActive,
	}

	silent, ok := live.SilentFor(now)
	if !ok {
		t.Fatal("an observed worker reported no silence figure at all")
	}
	if silent != 3*time.Second {
		t.Fatalf("silent for %v, want 3s -- the transition clock was read as liveness again", silent)
	}
	// The transition clock is kept, not discarded: "active since 17:10" is a
	// real fact. It is only a misreport when it is LABELLED as liveness.
	if !live.LastTransitionAt.Equal(wentActiveAt) {
		t.Fatalf("transition clock = %v, want it preserved beside the signal clock", live.LastTransitionAt)
	}
}

// A run with no running agent has no liveness to report, and reporting one
// would be the same misreport inverted: "heard from 0s ago" under a run where
// nothing is running.
func TestUnobservedWorkerLivenessHasNoSilenceFigure(t *testing.T) {
	if _, ok := (WorkerLiveness{}).SilentFor(time.Now()); ok {
		t.Fatal("an unobserved worker produced a silence figure")
	}
	if _, ok := (WorkerLiveness{Observed: true}).SilentFor(time.Now()); ok {
		t.Fatal("an observed worker with no signal clock produced a silence figure")
	}
}

// The agent a person means by "the worker" is the one running NOW. A fix
// cycle's step is live once it starts, and the work step it followed keeps its
// session id long after its own turn ended.
func TestTheRunningStepIsTheOneWhoseLivenessIsReported(t *testing.T) {
	sid := func(s string) *string { return &s }
	steps := []StepDetail{
		{Step: domain.WorkflowStep{ID: "s-work", Ordinal: 2, Kind: domain.WorkflowStepWork,
			State: domain.WorkflowStepCompleted, SessionID: sid("sess-work")}},
		{Step: domain.WorkflowStep{ID: "s-review", Ordinal: 3, Kind: domain.WorkflowStepReview,
			State: domain.WorkflowStepCompleted, SessionID: sid("sess-review")}},
		{Step: domain.WorkflowStep{ID: "s-fix", Ordinal: 4, Kind: domain.WorkflowStepFix,
			State: domain.WorkflowStepRunning, SessionID: sid("sess-fix")}},
	}
	step, session, ok := runningWorkerSession(steps)
	if !ok {
		t.Fatal("a run with a running fix step reported no running agent")
	}
	if session != "sess-fix" || step.ID != "s-fix" {
		t.Fatalf("reported %q/%q, want the RUNNING fix step", step.ID, session)
	}
}

// A running step with no session is not an agent AO can ask. Guessing at the
// last session the run used would produce a clock about the wrong process.
func TestARunningStepWithNoSessionReportsNothing(t *testing.T) {
	steps := []StepDetail{
		{Step: domain.WorkflowStep{ID: "s-verify", Ordinal: 5, Kind: domain.WorkflowStepVerify,
			State: domain.WorkflowStepRunning}},
	}
	if _, _, ok := runningWorkerSession(steps); ok {
		t.Fatal("a running step with no session was treated as an agent to ask")
	}
}
