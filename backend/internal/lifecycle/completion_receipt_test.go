package lifecycle

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// The durable receipt behind the Completed status. AO has no task entity: a
// task IS a session, and a task that succeeds stays alive and goes idle, which
// is byte-for-byte what a session that never did anything looks like. These
// tests pin the one thing that tells them apart — the agent's own report that
// its turn ended — and pin that nothing else is allowed to write it.

func idleSession(id domain.SessionID) domain.SessionRecord {
	rec := working(id)
	rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: time.Now()}
	rec.FirstSignalAt = time.Now().Add(-time.Minute)
	return rec
}

func TestStopHookStampsTheCompletionReceipt(t *testing.T) {
	m, st, _ := newManager()
	st.sessions["mer-1"] = working("mer-1")

	stoppedAt := time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC)
	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
		Valid: true, State: domain.ActivityIdle, Event: "stop", Timestamp: stoppedAt,
	}); err != nil {
		t.Fatalf("ApplyActivitySignal: %v", err)
	}

	got := st.sessions["mer-1"]
	if !got.TurnCompletedAt.Equal(stoppedAt) {
		t.Fatalf("TurnCompletedAt = %v, want the stop's own timestamp %v", got.TurnCompletedAt, stoppedAt)
	}
	if got.IsTerminated || got.Activity.State != domain.ActivityIdle {
		t.Fatalf("a finished task must stay alive and idle, got %+v", got.Activity)
	}
}

// The usual shape in production: the turn's "active" POST never arrives (hook
// delivery is best-effort), so the Stop lands on a row that already reads idle.
// The same-state early return must not swallow the only proof of completion.
func TestStopOnAnAlreadyIdleRowStillStampsTheReceipt(t *testing.T) {
	m, st, _ := newManager()
	st.sessions["mer-1"] = idleSession("mer-1")

	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
		Valid: true, State: domain.ActivityIdle, Event: "stop",
	}); err != nil {
		t.Fatalf("ApplyActivitySignal: %v", err)
	}

	if st.sessions["mer-1"].TurnCompletedAt.IsZero() {
		t.Fatal("a stop on an already-idle row left no completion receipt")
	}
}

func TestChatTurnCompletionStampsTheReceipt(t *testing.T) {
	m, st, _ := newManager()
	rec := working("mer-1")
	rec.Mode = domain.SessionModeChat
	rec.Metadata.ControllerGeneration = "gen-1"
	st.sessions["mer-1"] = rec

	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
		Valid: true, State: domain.ActivityIdle, Event: "chat.turn.completed",
		ControllerGeneration: "gen-1",
	}); err != nil {
		t.Fatalf("ApplyActivitySignal: %v", err)
	}

	if st.sessions["mer-1"].TurnCompletedAt.IsZero() {
		t.Fatal("a chat turn that completed left no completion receipt")
	}
}

// Quietness is not completion. None of these is an agent reporting that the
// work is done, so none of them may promote a session to Completed.
func TestOnlyAReportedTurnEndStampsTheReceipt(t *testing.T) {
	tests := []struct {
		name   string
		signal ports.ActivitySignal
	}{
		{
			"untagged idle from an old CLI",
			ports.ActivitySignal{Valid: true, State: domain.ActivityIdle},
		},
		{
			"the agent sitting at its prompt between tool calls",
			ports.ActivitySignal{Valid: true, State: domain.ActivityIdle, Event: "notification"},
		},
		{
			"the agent process going away",
			ports.ActivitySignal{Valid: true, State: domain.ActivityExited, Event: "session-end"},
		},
		{
			"the runtime being observed dead",
			ports.ActivitySignal{Valid: true, State: domain.ActivityExited, Event: "process-exited"},
		},
		{
			"a chat controller being torn down",
			ports.ActivitySignal{Valid: true, State: domain.ActivityExited, Event: "chat.controller.stopped"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, st, _ := newManager()
			st.sessions["mer-1"] = working("mer-1")

			if err := m.ApplyActivitySignal(ctx, "mer-1", tt.signal); err != nil {
				t.Fatalf("ApplyActivitySignal: %v", err)
			}
			if got := st.sessions["mer-1"].TurnCompletedAt; !got.IsZero() {
				t.Fatalf("TurnCompletedAt = %v, want no receipt for %q", got, tt.name)
			}
		})
	}
}

// The receipt describes the CURRENT quiet period. Anything that puts work back
// in flight clears it, so a task that was told to do more — or that stopped to
// ask the user something — cannot fall back to Completed if it later goes quiet
// without saying so again.
func TestWorkBackInFlightClearsTheCompletionReceipt(t *testing.T) {
	tests := []struct {
		name   string
		signal ports.ActivitySignal
	}{
		{
			"a new prompt",
			ports.ActivitySignal{Valid: true, State: domain.ActivityActive, Event: "user-prompt-submit"},
		},
		{
			"a tool starting without its prompt hook",
			ports.ActivitySignal{Valid: true, State: domain.ActivityActive, Event: "pre-tool-use", ToolUseID: "t1", ToolName: "Bash"},
		},
		{
			"a question back to the user",
			ports.ActivitySignal{Valid: true, State: domain.ActivityWaitingInput, Event: "notification"},
		},
		{
			"a permission decision",
			ports.ActivitySignal{Valid: true, State: domain.ActivityBlocked, Event: "permission-request", ToolName: "Bash"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, st, _ := newManager()
			rec := idleSession("mer-1")
			rec.TurnCompletedAt = time.Now().Add(-time.Hour)
			st.sessions["mer-1"] = rec

			if err := m.ApplyActivitySignal(ctx, "mer-1", tt.signal); err != nil {
				t.Fatalf("ApplyActivitySignal: %v", err)
			}
			if got := st.sessions["mer-1"].TurnCompletedAt; !got.IsZero() {
				t.Fatalf("TurnCompletedAt = %v, want the receipt cleared by %q", got, tt.name)
			}
		})
	}
}

// Inactivity is not an input. Once stamped, the receipt is only rewritten by a
// later turn — a reaper sweep, a runtime probe or the passage of time cannot
// take a finished task's status away from it.
func TestCompletionReceiptSurvivesQuietTime(t *testing.T) {
	m, st, _ := newManager()
	rec := idleSession("mer-1")
	stamped := time.Now().Add(-48 * time.Hour)
	rec.TurnCompletedAt = stamped
	rec.Activity.LastActivityAt = stamped
	st.sessions["mer-1"] = rec

	if err := m.ApplyRuntimeObservation(ctx, "mer-1", ports.RuntimeFacts{
		Runtime: ports.ProbeAlive, Workload: ports.ProbeAlive,
	}); err != nil {
		t.Fatalf("ApplyRuntimeObservation: %v", err)
	}
	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
		Valid: true, State: domain.ActivityIdle,
	}); err != nil {
		t.Fatalf("ApplyActivitySignal: %v", err)
	}

	if got := st.sessions["mer-1"].TurnCompletedAt; !got.Equal(stamped) {
		t.Fatalf("TurnCompletedAt = %v, want it untouched at %v", got, stamped)
	}
}

// A repeated stop in the same quiet period keeps the original stamp: the
// receipt marks when the work finished, not when AO last heard about it.
func TestRepeatedStopKeepsTheOriginalStamp(t *testing.T) {
	m, st, _ := newManager()
	st.sessions["mer-1"] = working("mer-1")

	first := time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC)
	second := first.Add(5 * time.Minute)
	for _, at := range []time.Time{first, second} {
		if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
			Valid: true, State: domain.ActivityIdle, Event: "stop", Timestamp: at,
		}); err != nil {
			t.Fatalf("ApplyActivitySignal: %v", err)
		}
	}

	if got := st.sessions["mer-1"].TurnCompletedAt; !got.Equal(first) {
		t.Fatalf("TurnCompletedAt = %v, want the first stop %v", got, first)
	}
}

// An orchestrator is not a task. Its turns end constantly and none of them is
// work being delivered, so it keeps reading Idle — waiting for you — rather
// than announcing a completion after every reply.
func TestOrchestratorTurnsLeaveNoCompletionReceipt(t *testing.T) {
	m, st, _ := newManager()
	rec := working("mer-1")
	rec.Kind = domain.KindOrchestrator
	st.sessions["mer-1"] = rec

	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
		Valid: true, State: domain.ActivityIdle, Event: "stop",
	}); err != nil {
		t.Fatalf("ApplyActivitySignal: %v", err)
	}
	if got := st.sessions["mer-1"].TurnCompletedAt; !got.IsZero() {
		t.Fatalf("TurnCompletedAt = %v, want no receipt for an orchestrator", got)
	}
}
