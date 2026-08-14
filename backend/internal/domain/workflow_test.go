package domain

import "testing"

func TestValidWorkflowRunTransition(t *testing.T) {
	cases := []struct {
		from, to WorkflowRunState
		want     bool
	}{
		{WorkflowRunPending, WorkflowRunPending, true},
		{WorkflowRunPending, WorkflowRunRunning, true},
		{WorkflowRunPending, WorkflowRunCancelled, true},
		{WorkflowRunPending, WorkflowRunCompleted, false},
		{WorkflowRunPending, WorkflowRunWaiting, true},
		{WorkflowRunPending, WorkflowRunNeedsAttention, true},
		{WorkflowRunRunning, WorkflowRunWaiting, true},
		{WorkflowRunRunning, WorkflowRunNeedsAttention, true},
		{WorkflowRunRunning, WorkflowRunCompleted, true},
		{WorkflowRunRunning, WorkflowRunFailed, true},
		{WorkflowRunRunning, WorkflowRunCancelled, true},
		{WorkflowRunRunning, WorkflowRunPending, false},
		{WorkflowRunWaiting, WorkflowRunRunning, true},
		{WorkflowRunWaiting, WorkflowRunNeedsAttention, true},
		{WorkflowRunWaiting, WorkflowRunFailed, true},
		{WorkflowRunWaiting, WorkflowRunCancelled, true},
		{WorkflowRunWaiting, WorkflowRunCompleted, false},
		{WorkflowRunNeedsAttention, WorkflowRunRunning, true},
		{WorkflowRunNeedsAttention, WorkflowRunFailed, true},
		{WorkflowRunNeedsAttention, WorkflowRunCancelled, true},
		{WorkflowRunNeedsAttention, WorkflowRunCompleted, false},
		// Terminal states have zero outgoing transitions — not even to themselves.
		{WorkflowRunCompleted, WorkflowRunCompleted, false},
		{WorkflowRunCompleted, WorkflowRunRunning, false},
		{WorkflowRunFailed, WorkflowRunRunning, false},
		{WorkflowRunFailed, WorkflowRunFailed, false},
		{WorkflowRunCancelled, WorkflowRunRunning, false},
		{WorkflowRunCancelled, WorkflowRunCancelled, false},
		// Invalid states are always rejected.
		{WorkflowRunState("bogus"), WorkflowRunRunning, false},
		{WorkflowRunPending, WorkflowRunState("bogus"), false},
	}
	for _, c := range cases {
		if got := ValidWorkflowRunTransition(c.from, c.to); got != c.want {
			t.Errorf("ValidWorkflowRunTransition(%q, %q) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestWorkflowRunStateTerminal(t *testing.T) {
	for _, s := range []WorkflowRunState{WorkflowRunCompleted, WorkflowRunFailed, WorkflowRunCancelled} {
		if !s.Terminal() {
			t.Errorf("%q.Terminal() = false, want true", s)
		}
	}
	for _, s := range []WorkflowRunState{WorkflowRunPending, WorkflowRunRunning, WorkflowRunWaiting, WorkflowRunNeedsAttention} {
		if s.Terminal() {
			t.Errorf("%q.Terminal() = true, want false", s)
		}
	}
}

func TestValidWorkflowStepTransition(t *testing.T) {
	cases := []struct {
		from, to WorkflowStepState
		want     bool
	}{
		{WorkflowStepPending, WorkflowStepPending, true},
		{WorkflowStepPending, WorkflowStepReady, true},
		{WorkflowStepPending, WorkflowStepCancelled, true},
		{WorkflowStepPending, WorkflowStepRunning, false},
		{WorkflowStepReady, WorkflowStepRunning, true},
		{WorkflowStepReady, WorkflowStepCancelled, true},
		{WorkflowStepReady, WorkflowStepPending, false},
		{WorkflowStepRunning, WorkflowStepWaiting, true},
		{WorkflowStepRunning, WorkflowStepCompleted, true},
		{WorkflowStepRunning, WorkflowStepFailed, true},
		{WorkflowStepRunning, WorkflowStepCancelled, true},
		{WorkflowStepRunning, WorkflowStepReady, false},
		{WorkflowStepWaiting, WorkflowStepRunning, true},
		{WorkflowStepWaiting, WorkflowStepFailed, true},
		{WorkflowStepWaiting, WorkflowStepCancelled, true},
		{WorkflowStepWaiting, WorkflowStepCompleted, false},
		// Terminal states have zero outgoing transitions.
		{WorkflowStepCompleted, WorkflowStepCompleted, false},
		{WorkflowStepCompleted, WorkflowStepRunning, false},
		{WorkflowStepFailed, WorkflowStepRunning, false},
		{WorkflowStepCancelled, WorkflowStepRunning, false},
		{WorkflowStepState("bogus"), WorkflowStepReady, false},
	}
	for _, c := range cases {
		if got := ValidWorkflowStepTransition(c.from, c.to); got != c.want {
			t.Errorf("ValidWorkflowStepTransition(%q, %q) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestWorkflowStepStateTerminal(t *testing.T) {
	for _, s := range []WorkflowStepState{WorkflowStepCompleted, WorkflowStepFailed, WorkflowStepCancelled} {
		if !s.Terminal() {
			t.Errorf("%q.Terminal() = false, want true", s)
		}
	}
	for _, s := range []WorkflowStepState{WorkflowStepPending, WorkflowStepReady, WorkflowStepRunning, WorkflowStepWaiting} {
		if s.Terminal() {
			t.Errorf("%q.Terminal() = true, want false", s)
		}
	}
}

func TestWorkflowEnumValid(t *testing.T) {
	if !WorkflowStepKind("plan").Valid() || WorkflowStepKind("bogus").Valid() {
		t.Fatal("WorkflowStepKind.Valid() misbehaves")
	}
	if !WorkflowAttemptOutcome("").Valid() || !WorkflowAttemptOutcome("succeeded").Valid() || WorkflowAttemptOutcome("bogus").Valid() {
		t.Fatal("WorkflowAttemptOutcome.Valid() misbehaves")
	}
	if !WorkflowErrorClass("").Valid() || !WorkflowErrorClass("rate_limited").Valid() || WorkflowErrorClass("bogus").Valid() {
		t.Fatal("WorkflowErrorClass.Valid() misbehaves")
	}
	if !WorkflowOutboxCommandType("spawn_worker_session").Valid() || WorkflowOutboxCommandType("bogus").Valid() {
		t.Fatal("WorkflowOutboxCommandType.Valid() misbehaves")
	}
	if !WorkflowOutboxStatus("pending").Valid() || WorkflowOutboxStatus("bogus").Valid() {
		t.Fatal("WorkflowOutboxStatus.Valid() misbehaves")
	}
}
