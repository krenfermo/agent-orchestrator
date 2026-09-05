package notification

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// The autonomous policy and the idempotency key.
//
// Two rules are load-bearing here and both are about not spending a person's
// attention. AO retrying by itself is silent; AO out of moves is not. And the
// key that names a fact is derived only from durable identity, so the same fact
// observed twice -- a reconcile, a restart, a second observer -- is one
// notification, forever.

type recordingNotifier struct {
	intents []ports.NotificationIntent
	err     error
}

func (r *recordingNotifier) Notify(_ context.Context, intent ports.NotificationIntent) error {
	r.intents = append(r.intents, intent)
	return r.err
}

func newFactNotifier() (*SessionFactNotifier, *recordingNotifier) {
	n := &recordingNotifier{}
	return NewSessionFactNotifier(SessionFactDeps{
		Notifier: n,
		Clock:    func() time.Time { return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC) },
	}), n
}

func baseFact(kind ports.SessionFactKind) ports.SessionFact {
	return ports.SessionFact{
		Kind:      kind,
		SessionID: "mer-1",
		ProjectID: "mer",
		ScopeID:   "scope-1",
	}
}

// The silent half of the policy. AO nudging an agent to fix CI is AO working,
// and it happens repeatedly per PR; interrupting anyone for it is how a
// notification channel gets muted.
func TestRepairAttemptNotifiesNobody(t *testing.T) {
	notifier, sink := newFactNotifier()
	if err := notifier.Record(context.Background(), baseFact(ports.SessionFactRepairAttempted)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(sink.intents) != 0 {
		t.Fatalf("a repair attempt raised %d notifications, want 0", len(sink.intents))
	}
}

// The loud half. The budget is spent, the problem survived it, nothing further
// happens without a person.
func TestRepairExhaustionNotifies(t *testing.T) {
	notifier, sink := newFactNotifier()
	fact := baseFact(ports.SessionFactRepairExhausted)
	fact.Detail = "AO tried 3 times."
	if err := notifier.Record(context.Background(), fact); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(sink.intents) != 1 {
		t.Fatalf("intents = %d, want 1", len(sink.intents))
	}
	got := sink.intents[0]
	if got.Type != domain.NotificationRepairExhausted {
		t.Fatalf("Type = %q, want repair_exhausted", got.Type)
	}
	if got.Detail != "AO tried 3 times." {
		t.Fatalf("Detail = %q, want the fact's detail carried through", got.Detail)
	}
	if got.Source != domain.NotificationSourceLifecycle {
		t.Fatalf("Source = %q, want lifecycle provenance", got.Source)
	}
	if got.SourceEventID != got.DedupeKey {
		t.Fatalf("SourceEventID = %q, want it to equal the dedupe key %q", got.SourceEventID, got.DedupeKey)
	}
}

func TestEachNotifyingKindMapsToItsType(t *testing.T) {
	for _, tc := range []struct {
		kind ports.SessionFactKind
		want domain.NotificationType
	}{
		{ports.SessionFactHumanQuestion, domain.NotificationHumanQuestionRequired},
		{ports.SessionFactRepairExhausted, domain.NotificationRepairExhausted},
		{ports.SessionFactIntegrationFailed, domain.NotificationIntegrationFailed},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			notifier, sink := newFactNotifier()
			if err := notifier.Record(context.Background(), baseFact(tc.kind)); err != nil {
				t.Fatalf("Record: %v", err)
			}
			if len(sink.intents) != 1 {
				t.Fatalf("intents = %d, want 1", len(sink.intents))
			}
			if got := sink.intents[0].Type; got != tc.want {
				t.Fatalf("Type = %q, want %q", got, tc.want)
			}
			if sink.intents[0].DedupeKey == "" {
				t.Fatal("a run-scoped notification was raised with no dedupe key")
			}
		})
	}
}

// The dedupe key is a pure function of durable identity. Nothing that varies
// between two reads of the same stored fact may enter it -- not the observation
// time, not the wording -- because that is exactly what makes a restart replay
// to the same row.
func TestDedupeKeyIgnoresObservationTimeAndWording(t *testing.T) {
	notifier, sink := newFactNotifier()
	ctx := context.Background()

	first := baseFact(ports.SessionFactHumanQuestion)
	first.ObservedAt = time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)
	first.Detail = "seen on the first pass"

	second := baseFact(ports.SessionFactHumanQuestion)
	second.ObservedAt = time.Date(2026, 9, 4, 17, 45, 0, 0, time.UTC)
	second.Detail = "seen again after a restart, worded differently"

	for _, fact := range []ports.SessionFact{first, second} {
		if err := notifier.Record(ctx, fact); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if len(sink.intents) != 2 {
		t.Fatalf("intents = %d, want 2 (storage does the collapsing)", len(sink.intents))
	}
	if a, b := sink.intents[0].DedupeKey, sink.intents[1].DedupeKey; a != b {
		t.Fatalf("dedupe keys differ across a replay: %q vs %q", a, b)
	}
}

// Two genuinely different instances of one kind are two notifications: a later
// pause is a new question, and a second repair loop is a new problem.
func TestDistinctScopesAreDistinctNotifications(t *testing.T) {
	notifier, sink := newFactNotifier()
	ctx := context.Background()
	for _, scope := range []string{"pause-1", "pause-2"} {
		fact := baseFact(ports.SessionFactHumanQuestion)
		fact.ScopeID = scope
		if err := notifier.Record(ctx, fact); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if a, b := sink.intents[0].DedupeKey, sink.intents[1].DedupeKey; a == b {
		t.Fatalf("two distinct pauses shared one dedupe key %q", a)
	}
}

// Two sessions stopping for the same reason are two notifications.
func TestDedupeKeyIsScopedToTheSession(t *testing.T) {
	notifier, sink := newFactNotifier()
	ctx := context.Background()
	for _, id := range []domain.SessionID{"mer-1", "mer-2"} {
		fact := baseFact(ports.SessionFactRepairExhausted)
		fact.SessionID = id
		if err := notifier.Record(ctx, fact); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if a, b := sink.intents[0].DedupeKey, sink.intents[1].DedupeKey; a == b {
		t.Fatalf("two sessions shared one dedupe key %q", a)
	}
}

// A fact with no durable scope cannot be made idempotent, so it is rejected
// rather than written under a key that would collide or duplicate.
func TestFactWithoutScopeIsRejected(t *testing.T) {
	notifier, sink := newFactNotifier()
	fact := baseFact(ports.SessionFactHumanQuestion)
	fact.ScopeID = "  "
	err := notifier.Record(context.Background(), fact)
	if !errors.Is(err, ErrInvalidSessionFact) {
		t.Fatalf("Record error = %v, want ErrInvalidSessionFact", err)
	}
	if len(sink.intents) != 0 {
		t.Fatalf("a scopeless fact still wrote %d notifications", len(sink.intents))
	}
}

func TestFactWithoutSessionIsRejected(t *testing.T) {
	notifier, _ := newFactNotifier()
	fact := baseFact(ports.SessionFactHumanQuestion)
	fact.SessionID = ""
	if err := notifier.Record(context.Background(), fact); !errors.Is(err, ErrInvalidSessionFact) {
		t.Fatalf("Record error = %v, want ErrInvalidSessionFact", err)
	}
}

// A fact that arrives without an observation time still gets a created_at.
func TestObservationTimeDefaultsToTheClock(t *testing.T) {
	notifier, sink := newFactNotifier()
	if err := notifier.Record(context.Background(), baseFact(ports.SessionFactHumanQuestion)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if sink.intents[0].CreatedAt.IsZero() {
		t.Fatal("CreatedAt is zero; the injected clock should have filled it")
	}
}

// The repair scope carries the attempt budget, so raising the budget starts a
// genuinely new repair instead of being silenced forever by the exhaustion
// already recorded under the old one.
func TestRepairScopeIDSeparatesBudgets(t *testing.T) {
	if a, b := ports.RepairScopeID("ci-failure:pr-1", 3), ports.RepairScopeID("ci-failure:pr-1", 5); a == b {
		t.Fatalf("two attempt budgets shared one scope id %q", a)
	}
}
