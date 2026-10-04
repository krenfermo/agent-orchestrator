package lifecycle

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// The defect, reproduced at the reducer: an agent that stays `active` reports in
// over and over and changes state in none of those reports. Before the liveness
// clock existed every one of them was folded away, so twenty minutes of work
// left behind exactly the same row the first second did.
func TestLiveness_RepeatedSameStateSignalsAdvanceLivenessNotTransition(t *testing.T) {
	m, st, _ := newManager()
	entered := time.Date(2026, 9, 9, 17, 10, 20, 0, time.UTC)
	rec := working("mer-1")
	rec.FirstSignalAt = entered
	rec.Activity = domain.Activity{State: domain.ActivityActive, LastActivityAt: entered, LastSignalAt: entered}
	st.sessions["mer-1"] = rec

	// Twenty minutes of PostToolUse callbacks, one per minute, all `active`.
	for i := 1; i <= 20; i++ {
		at := entered.Add(time.Duration(i) * time.Minute)
		if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
			Valid: true, State: domain.ActivityActive, Event: "post-tool-use", Timestamp: at,
		}); err != nil {
			t.Fatalf("signal %d: %v", i, err)
		}
	}

	got := st.sessions["mer-1"].Activity
	if !got.LastActivityAt.Equal(entered) {
		t.Fatalf("transition clock moved on same-state repeats: %v, want %v", got.LastActivityAt, entered)
	}
	want := entered.Add(20 * time.Minute)
	if !got.LastSignalAt.Equal(want) {
		t.Fatalf("liveness clock = %v, want %v", got.LastSignalAt, want)
	}
	if got.State != domain.ActivityActive {
		t.Fatalf("state changed: %q", got.State)
	}
	if elapsed := want.Sub(got.Liveness()); elapsed != 0 {
		t.Fatalf("Liveness() reports %v of silence for a session heard from at %v", elapsed, want)
	}
}

// The write bound. A same-state repeat whose only news is the clock is coalesced,
// so an agent making hundreds of tool calls cannot make hundreds of writes.
func TestLiveness_SameStateRepeatsAreCoalesced(t *testing.T) {
	m, st, _ := newManager()
	entered := time.Date(2026, 9, 9, 17, 10, 20, 0, time.UTC)
	rec := working("mer-1")
	rec.FirstSignalAt = entered
	rec.Activity = domain.Activity{State: domain.ActivityActive, LastActivityAt: entered, LastSignalAt: entered}
	st.sessions["mer-1"] = rec

	inside := entered.Add(livenessCoalesceWindow - time.Second)
	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
		Valid: true, State: domain.ActivityActive, Event: "post-tool-use", Timestamp: inside,
	}); err != nil {
		t.Fatal(err)
	}
	if got := st.sessions["mer-1"].Activity.LastSignalAt; !got.Equal(entered) {
		t.Fatalf("a repeat inside the coalescing window must not write: liveness = %v, want %v", got, entered)
	}

	atEdge := entered.Add(livenessCoalesceWindow)
	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
		Valid: true, State: domain.ActivityActive, Event: "post-tool-use", Timestamp: atEdge,
	}); err != nil {
		t.Fatal(err)
	}
	if got := st.sessions["mer-1"].Activity.LastSignalAt; !got.Equal(atEdge) {
		t.Fatalf("a repeat at the window edge must write: liveness = %v, want %v", got, atEdge)
	}
}

// Hook deliveries are best-effort and can arrive out of order. An older callback
// overtaking a newer one must never make a live session look staler than AO has
// already proven it to be.
func TestLiveness_NeverRegressesOnAnOutOfOrderSignal(t *testing.T) {
	m, st, _ := newManager()
	entered := time.Date(2026, 9, 9, 17, 10, 20, 0, time.UTC)
	newest := entered.Add(10 * time.Minute)
	rec := working("mer-1")
	rec.FirstSignalAt = entered
	rec.Activity = domain.Activity{State: domain.ActivityActive, LastActivityAt: entered, LastSignalAt: newest}
	st.sessions["mer-1"] = rec

	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
		Valid: true, State: domain.ActivityActive, Event: "post-tool-use",
		Timestamp: entered.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if got := st.sessions["mer-1"].Activity.LastSignalAt; !got.Equal(newest) {
		t.Fatalf("liveness regressed to %v, want it held at %v", got, newest)
	}
}

// A real transition still sets both clocks: entering a state IS being heard from.
func TestLiveness_StateTransitionSetsBothClocks(t *testing.T) {
	m, st, _ := newManager()
	entered := time.Date(2026, 9, 9, 17, 10, 20, 0, time.UTC)
	rec := working("mer-1")
	rec.FirstSignalAt = entered
	rec.Activity = domain.Activity{State: domain.ActivityActive, LastActivityAt: entered, LastSignalAt: entered}
	st.sessions["mer-1"] = rec

	at := entered.Add(5 * time.Minute)
	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
		Valid: true, State: domain.ActivityIdle, Event: "stop", Timestamp: at,
	}); err != nil {
		t.Fatal(err)
	}
	got := st.sessions["mer-1"].Activity
	if !got.LastActivityAt.Equal(at) || !got.LastSignalAt.Equal(at) {
		t.Fatalf("transition must set both clocks: %+v, want both at %v", got, at)
	}
}

// The pause-scoped semantics the transition clock exists for are untouched. A
// session that enters waiting_input and is then hammered with same-state
// repeats keeps ONE pause instant, so the notification, the human-question fact
// and the waiting-input telemetry all continue to name the same episode.
func TestLiveness_PauseInstantSurvivesSameStateRepeats(t *testing.T) {
	m, st, _ := newManager()
	entered := time.Date(2026, 9, 9, 17, 10, 20, 0, time.UTC)
	rec := working("mer-1")
	rec.FirstSignalAt = entered
	rec.Activity = domain.Activity{State: domain.ActivityActive, LastActivityAt: entered, LastSignalAt: entered}
	st.sessions["mer-1"] = rec

	paused := entered.Add(time.Minute)
	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
		Valid: true, State: domain.ActivityWaitingInput, Timestamp: paused,
	}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5; i++ {
		if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
			Valid: true, State: domain.ActivityWaitingInput,
			Timestamp: paused.Add(time.Duration(i) * livenessCoalesceWindow),
		}); err != nil {
			t.Fatal(err)
		}
	}
	got := st.sessions["mer-1"].Activity
	if !got.LastActivityAt.Equal(paused) {
		t.Fatalf("the pause instant moved: %v, want %v", got.LastActivityAt, paused)
	}
	if got.State != domain.ActivityWaitingInput {
		t.Fatalf("state = %q, want waiting_input", got.State)
	}
	if !got.LastSignalAt.After(paused) {
		t.Fatalf("liveness did not advance during the pause: %+v", got)
	}
}

// The completion receipt is written from a reported turn boundary and nothing
// about liveness may reach it: a session emitting signals is not a session that
// said it was done, and one that said so keeps the receipt while it goes quiet.
func TestLiveness_DoesNotDisturbTheCompletionReceipt(t *testing.T) {
	m, st, _ := newManager()
	entered := time.Date(2026, 9, 9, 17, 10, 20, 0, time.UTC)
	rec := working("mer-1")
	rec.FirstSignalAt = entered
	rec.Activity = domain.Activity{State: domain.ActivityActive, LastActivityAt: entered, LastSignalAt: entered}
	st.sessions["mer-1"] = rec

	// Signals while working: no receipt may appear.
	for i := 1; i <= 3; i++ {
		if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
			Valid: true, State: domain.ActivityActive, Event: "post-tool-use",
			Timestamp: entered.Add(time.Duration(i) * livenessCoalesceWindow),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if got := st.sessions["mer-1"]; !got.TurnCompletedAt.IsZero() {
		t.Fatalf("liveness stamped a completion receipt: %v", got.TurnCompletedAt)
	}

	// The reported boundary still stamps it.
	done := entered.Add(10 * time.Minute)
	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
		Valid: true, State: domain.ActivityIdle, Event: "stop", Timestamp: done,
	}); err != nil {
		t.Fatal(err)
	}
	stamped := st.sessions["mer-1"].TurnCompletedAt
	if stamped.IsZero() {
		t.Fatal("reported turn boundary did not stamp the receipt")
	}
	// ...and a later liveness-only repeat does not move or clear it.
	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
		Valid: true, State: domain.ActivityIdle, Timestamp: done.Add(2 * livenessCoalesceWindow),
	}); err != nil {
		t.Fatal(err)
	}
	if got := st.sessions["mer-1"].TurnCompletedAt; !got.Equal(stamped) {
		t.Fatalf("receipt moved on a liveness-only write: %v -> %v", stamped, got)
	}
}
