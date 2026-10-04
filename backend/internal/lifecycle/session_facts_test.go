package lifecycle

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// The session-level facts lifecycle reports to the notification producer.
//
// The case this file exists for is the FIRST one below. A permission-request
// hook moves a session from active or idle straight into blocked; it never
// passes through waiting_input. An earlier draft only reported the
// waiting_input -> blocked escalation, so the common pending-decision path
// produced no human-question fact at all -- the notification for the one state
// automation is forbidden to resolve was the one that never fired.

type fakeSessionFactSink struct {
	facts []ports.SessionFact
	err   error
}

func (f *fakeSessionFactSink) Record(_ context.Context, fact ports.SessionFact) error {
	f.facts = append(f.facts, fact)
	return f.err
}

func (f *fakeSessionFactSink) ofKind(kind ports.SessionFactKind) []ports.SessionFact {
	var out []ports.SessionFact
	for _, fact := range f.facts {
		if fact.Kind == kind {
			out = append(out, fact)
		}
	}
	return out
}

func newFactManager() (*Manager, *fakeStore, *fakeSessionFactSink) {
	st := newFakeStore()
	facts := &fakeSessionFactSink{}
	return New(st, nil, WithSessionFactNotifier(facts)), st, facts
}

func blockedSignal(at time.Time) ports.ActivitySignal {
	return ports.ActivitySignal{
		Valid: true, State: domain.ActivityBlocked, Event: "permission-request", Timestamp: at,
	}
}

// The regression this whole fact exists for: active -> blocked, the shape a
// tool-permission prompt actually produces.
func TestHumanQuestionFactOnDirectEntryFromActive(t *testing.T) {
	m, st, facts := newFactManager()
	rec := working("mer-1")
	rec.DisplayName = "checkout-flow"
	st.sessions["mer-1"] = rec

	pausedAt := time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)
	if err := m.ApplyActivitySignal(ctx, "mer-1", blockedSignal(pausedAt)); err != nil {
		t.Fatalf("ApplyActivitySignal: %v", err)
	}

	got := facts.ofKind(ports.SessionFactHumanQuestion)
	if len(got) != 1 {
		t.Fatalf("human-question facts = %d, want 1 (%+v)", len(got), facts.facts)
	}
	fact := got[0]
	if fact.SessionID != "mer-1" || fact.ProjectID != "mer" {
		t.Fatalf("fact is not addressed to the session: %+v", fact)
	}
	if fact.SessionDisplayName != "checkout-flow" {
		t.Fatalf("SessionDisplayName = %q, want the enrichment hint", fact.SessionDisplayName)
	}
	// The pause's durable identity is the stored activity timestamp, so a
	// re-read of the same row recomputes the same scope.
	stored := st.sessions["mer-1"].Activity.LastActivityAt
	if want := ports.PauseScopeID(stored); fact.ScopeID != want {
		t.Fatalf("ScopeID = %q, want the stored pause timestamp %q", fact.ScopeID, want)
	}
}

// The other entry into blocked: an escalation out of waiting_input. It was the
// only path the earlier draft covered, and it must keep working.
func TestHumanQuestionFactOnEscalationFromWaitingInput(t *testing.T) {
	m, st, facts := newFactManager()
	rec := working("mer-1")
	rec.Activity.State = domain.ActivityWaitingInput
	st.sessions["mer-1"] = rec

	if err := m.ApplyActivitySignal(ctx, "mer-1", blockedSignal(time.Now().UTC())); err != nil {
		t.Fatalf("ApplyActivitySignal: %v", err)
	}
	if got := facts.ofKind(ports.SessionFactHumanQuestion); len(got) != 1 {
		t.Fatalf("human-question facts = %d, want 1", len(got))
	}
}

// Idle -> blocked is the same permission-prompt shape as active -> blocked; a
// session that had gone quiet still asks.
func TestHumanQuestionFactOnDirectEntryFromIdle(t *testing.T) {
	m, st, facts := newFactManager()
	st.sessions["mer-1"] = idleSession("mer-1")

	if err := m.ApplyActivitySignal(ctx, "mer-1", blockedSignal(time.Now().UTC())); err != nil {
		t.Fatalf("ApplyActivitySignal: %v", err)
	}
	if got := facts.ofKind(ports.SessionFactHumanQuestion); len(got) != 1 {
		t.Fatalf("human-question facts = %d, want 1", len(got))
	}
}

// One pause, one fact. A session already blocked that reports blocked again is
// not a new question, and the dedupe key would collapse it anyway -- but not
// raising it at all is what keeps the producer honest about "entry".
func TestHumanQuestionFactSkipsBlockedToBlocked(t *testing.T) {
	m, st, facts := newFactManager()
	rec := working("mer-1")
	rec.Activity.State = domain.ActivityBlocked
	st.sessions["mer-1"] = rec

	if err := m.ApplyActivitySignal(ctx, "mer-1", blockedSignal(time.Now().UTC())); err != nil {
		t.Fatalf("ApplyActivitySignal: %v", err)
	}
	if got := facts.ofKind(ports.SessionFactHumanQuestion); len(got) != 0 {
		t.Fatalf("a blocked -> blocked repeat raised %d facts, want 0", len(got))
	}
}

// Replay safety at the source: re-applying the same signal re-reads the same
// stored pause timestamp, so the scope id is identical and the producer's
// dedupe collapses the two to one notification.
func TestHumanQuestionFactScopeIsStableAcrossReplay(t *testing.T) {
	pausedAt := time.Date(2026, 9, 4, 11, 30, 0, 0, time.UTC)

	scopeFor := func() string {
		m, st, facts := newFactManager()
		st.sessions["mer-1"] = working("mer-1")
		if err := m.ApplyActivitySignal(ctx, "mer-1", blockedSignal(pausedAt)); err != nil {
			t.Fatalf("ApplyActivitySignal: %v", err)
		}
		got := facts.ofKind(ports.SessionFactHumanQuestion)
		if len(got) != 1 {
			t.Fatalf("human-question facts = %d, want 1", len(got))
		}
		return got[0].ScopeID
	}

	if first, second := scopeFor(), scopeFor(); first != second {
		t.Fatalf("scope id is not stable across a replay: %q vs %q", first, second)
	}
}

// A terminated session is not waiting on anyone.
func TestHumanQuestionFactSkipsTerminatedSession(t *testing.T) {
	m, st, facts := newFactManager()
	rec := working("mer-1")
	rec.IsTerminated = true
	st.sessions["mer-1"] = rec

	_ = m.ApplyActivitySignal(ctx, "mer-1", blockedSignal(time.Now().UTC()))
	if got := facts.ofKind(ports.SessionFactHumanQuestion); len(got) != 0 {
		t.Fatalf("a terminated session raised %d facts, want 0", len(got))
	}
}

// The legacy needs-input intent is a separate axis and must not change: it
// still fires exactly once on family ENTRY, so the new fact does not turn one
// pause into two user-visible pings on the notification side either.
func TestHumanQuestionFactDoesNotDuplicateTheNeedsInputIntent(t *testing.T) {
	st := newFakeStore()
	sink := &fakeNotificationSink{}
	facts := &fakeSessionFactSink{}
	m := New(st, nil, WithNotificationSink(sink), WithSessionFactNotifier(facts))
	st.sessions["mer-1"] = working("mer-1")

	if err := m.ApplyActivitySignal(ctx, "mer-1", blockedSignal(time.Now().UTC())); err != nil {
		t.Fatalf("ApplyActivitySignal: %v", err)
	}

	needsInput := 0
	for _, intent := range sink.intents {
		if intent.Type == domain.NotificationNeedsInput {
			needsInput++
		}
	}
	if needsInput != 1 {
		t.Fatalf("needs-input intents = %d, want exactly 1", needsInput)
	}
	if got := facts.ofKind(ports.SessionFactHumanQuestion); len(got) != 1 {
		t.Fatalf("human-question facts = %d, want 1", len(got))
	}
}

// A sink that errors must not fail the lifecycle write that observed the fact:
// the durable state is already committed, and a notification is never worth
// losing it.
func TestSessionFactSinkFailureDoesNotFailTheWrite(t *testing.T) {
	st := newFakeStore()
	facts := &fakeSessionFactSink{err: context.DeadlineExceeded}
	m := New(st, nil, WithSessionFactNotifier(facts))
	st.sessions["mer-1"] = working("mer-1")

	if err := m.ApplyActivitySignal(ctx, "mer-1", blockedSignal(time.Now().UTC())); err != nil {
		t.Fatalf("ApplyActivitySignal returned %v, want nil despite the sink failing", err)
	}
	if st.sessions["mer-1"].Activity.State != domain.ActivityBlocked {
		t.Fatal("the durable transition was lost when the notification sink failed")
	}
}
