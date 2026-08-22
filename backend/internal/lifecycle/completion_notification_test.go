package lifecycle

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// The "your task finished" notification. Its trigger has to be the completion
// receipt being stamped — the same durable fact the Completed status reads —
// and nothing weaker: a session that merely goes quiet, an orchestrator ending
// one of its endless turns, or a restart re-reading a receipt that was already
// there must all stay silent.

func completionIntents(sink *fakeNotificationSink) []ports.NotificationIntent {
	var out []ports.NotificationIntent
	for _, intent := range sink.intents {
		if intent.Type == domain.NotificationTaskCompleted {
			out = append(out, intent)
		}
	}
	return out
}

func newNotifyingManager() (*Manager, *fakeStore, *fakeNotificationSink) {
	st := newFakeStore()
	sink := &fakeNotificationSink{}
	return New(st, nil, WithNotificationSink(sink)), st, sink
}

func TestStopHookNotifiesTaskCompleted(t *testing.T) {
	m, st, sink := newNotifyingManager()
	rec := working("mer-1")
	rec.DisplayName = "checkout-flow"
	st.sessions["mer-1"] = rec

	stoppedAt := time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC)
	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
		Valid: true, State: domain.ActivityIdle, Event: "stop", Timestamp: stoppedAt,
	}); err != nil {
		t.Fatalf("ApplyActivitySignal: %v", err)
	}

	got := completionIntents(sink)
	if len(got) != 1 {
		t.Fatalf("task-completed intents = %d, want 1 (%+v)", len(got), sink.intents)
	}
	intent := got[0]
	if intent.SessionID != "mer-1" || intent.ProjectID != "mer" || intent.SessionDisplayName != "checkout-flow" {
		t.Fatalf("intent = %+v", intent)
	}
	if !intent.CreatedAt.Equal(stoppedAt) {
		t.Fatalf("CreatedAt = %v, want the receipt's own timestamp %v", intent.CreatedAt, stoppedAt)
	}
	// The key names one finished turn, so a replay of that turn is a duplicate
	// while a later turn is not.
	if want := "mer-1@" + stoppedAt.Format(time.RFC3339Nano); intent.DedupeKey != want {
		t.Fatalf("DedupeKey = %q, want %q", intent.DedupeKey, want)
	}
}

// The usual production shape: the turn's "active" POST never arrived, so the
// Stop lands on a row that already reads idle and takes the same-state write
// path. That path must notify too — it is where most completions arrive.
func TestStopOnAnAlreadyIdleRowNotifiesTaskCompleted(t *testing.T) {
	m, st, sink := newNotifyingManager()
	st.sessions["mer-1"] = idleSession("mer-1")

	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
		Valid: true, State: domain.ActivityIdle, Event: "stop",
	}); err != nil {
		t.Fatalf("ApplyActivitySignal: %v", err)
	}
	if got := completionIntents(sink); len(got) != 1 {
		t.Fatalf("task-completed intents = %d, want 1", len(got))
	}
}

// The receipt is only news the first time. A harness that re-delivers its Stop
// hook, or a daemon that restarts and re-observes an unchanged idle row, must
// not announce the same finished work twice.
func TestRepeatedStopNotifiesTaskCompletedOnlyOnce(t *testing.T) {
	m, st, sink := newNotifyingManager()
	st.sessions["mer-1"] = working("mer-1")

	stoppedAt := time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC)
	for range 3 {
		if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
			Valid: true, State: domain.ActivityIdle, Event: "stop", Timestamp: stoppedAt,
		}); err != nil {
			t.Fatalf("ApplyActivitySignal: %v", err)
		}
	}
	if got := completionIntents(sink); len(got) != 1 {
		t.Fatalf("task-completed intents = %d, want 1", len(got))
	}
}

// A second turn is a second piece of finished work and deserves its own
// notification — with its own key, so the store cannot mistake it for a replay.
func TestASecondTurnNotifiesAgainWithItsOwnKey(t *testing.T) {
	m, st, sink := newNotifyingManager()
	st.sessions["mer-1"] = working("mer-1")

	first := time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC)
	second := first.Add(time.Hour)
	signals := []ports.ActivitySignal{
		{Valid: true, State: domain.ActivityIdle, Event: "stop", Timestamp: first},
		{Valid: true, State: domain.ActivityActive, Event: "user-prompt-submit", Timestamp: first.Add(time.Minute)},
		{Valid: true, State: domain.ActivityIdle, Event: "stop", Timestamp: second},
	}
	for _, s := range signals {
		if err := m.ApplyActivitySignal(ctx, "mer-1", s); err != nil {
			t.Fatalf("ApplyActivitySignal(%s): %v", s.Event, err)
		}
	}

	got := completionIntents(sink)
	if len(got) != 2 {
		t.Fatalf("task-completed intents = %d, want 2", len(got))
	}
	if got[0].DedupeKey == got[1].DedupeKey {
		t.Fatalf("two different finished turns shared a dedupe key: %q", got[0].DedupeKey)
	}
}

// Teardown is not success. session-end, process-exited and the chat
// controller stopping all mean the agent went away, which says nothing about
// whether the work got done — and none of them stamps a receipt.
func TestTeardownEventsDoNotNotifyTaskCompleted(t *testing.T) {
	for _, event := range []string{"session-end", "process-exited", "chat.controller.stopped"} {
		t.Run(event, func(t *testing.T) {
			m, st, sink := newNotifyingManager()
			st.sessions["mer-1"] = working("mer-1")

			if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
				Valid: true, State: domain.ActivityExited, Event: event,
			}); err != nil {
				t.Fatalf("ApplyActivitySignal: %v", err)
			}
			if got := completionIntents(sink); len(got) != 0 {
				t.Fatalf("%s notified completion: %+v", event, got)
			}
		})
	}
}

// An untagged idle signal is an old CLI or a runtime probe saying the session
// looks quiet. That is not the agent reporting that it finished.
func TestGoingQuietWithoutReportingDoesNotNotify(t *testing.T) {
	m, st, sink := newNotifyingManager()
	st.sessions["mer-1"] = working("mer-1")

	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
		Valid: true, State: domain.ActivityIdle,
	}); err != nil {
		t.Fatalf("ApplyActivitySignal: %v", err)
	}
	if got := completionIntents(sink); len(got) != 0 {
		t.Fatalf("a quiet session notified completion: %+v", got)
	}
}

// An orchestrator is a standing conversation partner, not a unit of work: its
// turns end all day long and none of them is a task being finished.
func TestOrchestratorTurnsDoNotNotifyTaskCompleted(t *testing.T) {
	m, st, sink := newNotifyingManager()
	rec := working("mer-1")
	rec.Kind = domain.KindOrchestrator
	st.sessions["mer-1"] = rec

	if err := m.ApplyActivitySignal(ctx, "mer-1", ports.ActivitySignal{
		Valid: true, State: domain.ActivityIdle, Event: "stop",
	}); err != nil {
		t.Fatalf("ApplyActivitySignal: %v", err)
	}
	if got := completionIntents(sink); len(got) != 0 {
		t.Fatalf("an orchestrator turn notified completion: %+v", got)
	}
}
