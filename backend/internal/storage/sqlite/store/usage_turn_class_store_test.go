package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// usage_turn_class_store_test.go -- the refinement path, against a real
// database.
//
// A billed message arrives as several transcript records. The event is
// inserted on the first of them, so its class starts as whatever that record
// could imply; the later records of the SAME message are what reveal that the
// turn ran a command or edited a file. The ingest path treats those as
// refinements rather than as the token conflict a differing token vector
// would be, and these tests pin both halves of that: it refines, and it
// refuses to lower.

// singleTurnClass asserts the scope produced exactly one row and returns its
// class. Written against a count and an accessor so the test file does not
// have to import the store package purely to name a row type.
func singleTurnClass(t *testing.T, n int, class func() domain.TurnClass) domain.TurnClass {
	t.Helper()
	if n != 1 {
		t.Fatalf("trajectory events = %d, want exactly 1", n)
	}
	return class()
}

func classEvent(key string, input, output int64, observed time.Time, class domain.TurnClass) domain.ModelUsageEvent {
	ev := attrEvent(key, input, output, observed)
	ev.TurnClass = class
	return ev
}

func TestTurnClassIsStoredAndRefinedUpward(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Unix(1700000000, 0).UTC()
	sess, source := seedAttributionSession(t, s, base)
	openWindow(t, s, window{
		key: "w-worker", session: string(sess.ID), role: domain.WorkflowRoleWorker,
		step: "step-work", opened: base, harness: "claude-code", provider: "anthropic",
	})

	// First record of the message: only a thinking block had been seen, so the
	// honest class is "it talked".
	applyEvents(t, s, source, base.Add(time.Minute), []domain.ModelUsageEvent{
		classEvent("msg-1", 1000, 200, base.Add(time.Minute), domain.TurnMessage),
	})
	events, err := s.ListRunContextTrajectoryEvents(ctx, attrRunID)
	mustNoError(t, err, "list trajectory events")
	if got := singleTurnClass(t, len(events), func() domain.TurnClass { return events[0].TurnClass }); got != domain.TurnMessage {
		t.Fatalf("initial class = %q, want message", got)
	}

	// Second record of the SAME message carries the tool call. Same key, same
	// tokens: a refinement, not a conflict.
	applyEventsAtOffset(t, s, source, base.Add(2*time.Minute), []domain.ModelUsageEvent{
		classEvent("msg-1", 1000, 200, base.Add(time.Minute), domain.TurnCommand),
	}, 1)
	events, err = s.ListRunContextTrajectoryEvents(ctx, attrRunID)
	mustNoError(t, err, "list trajectory events after refinement")
	if len(events) != 1 {
		t.Fatalf("events = %d, want the refinement to land on the same row", len(events))
	}
	if got := singleTurnClass(t, len(events), func() domain.TurnClass { return events[0].TurnClass }); got != domain.TurnCommand {
		t.Fatalf("refined class = %q, want command", got)
	}
}

func TestTurnClassRefinementIsNeverLowered(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Unix(1700000000, 0).UTC()
	sess, source := seedAttributionSession(t, s, base)
	openWindow(t, s, window{
		key: "w-worker", session: string(sess.ID), role: domain.WorkflowRoleWorker,
		step: "step-work", opened: base, harness: "claude-code", provider: "anthropic",
	})

	applyEvents(t, s, source, base.Add(time.Minute), []domain.ModelUsageEvent{
		classEvent("msg-1", 1000, 200, base.Add(time.Minute), domain.TurnEdit),
	})

	// A re-read that could not decode the record must not erase what an
	// earlier read established.
	applyEventsAtOffset(t, s, source, base.Add(2*time.Minute), []domain.ModelUsageEvent{
		classEvent("msg-1", 1000, 200, base.Add(time.Minute), domain.TurnUnclassified),
	}, 1)
	events, err := s.ListRunContextTrajectoryEvents(ctx, attrRunID)
	mustNoError(t, err, "list trajectory events")
	if got := singleTurnClass(t, len(events), func() domain.TurnClass { return events[0].TurnClass }); got != domain.TurnEdit {
		t.Fatalf("class after an unclassified re-report = %q, want the stored edit", got)
	}

	// And a second, different real class means the one message did two kinds
	// of thing -- which is mixed, not a coin flip between them.
	applyEventsAtOffset(t, s, source, base.Add(3*time.Minute), []domain.ModelUsageEvent{
		classEvent("msg-1", 1000, 200, base.Add(time.Minute), domain.TurnCommand),
	}, 2)
	events, err = s.ListRunContextTrajectoryEvents(ctx, attrRunID)
	mustNoError(t, err, "list trajectory events")
	if got := singleTurnClass(t, len(events), func() domain.TurnClass { return events[0].TurnClass }); got != domain.TurnMixed {
		t.Fatalf("class after two real classes = %q, want mixed", got)
	}
}

// A row written before migration 0169 reads back as unclassified, which is an
// absence of information and must never be folded into a real class.
func TestTurnClassDefaultsToUnclassified(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Unix(1700000000, 0).UTC()
	sess, source := seedAttributionSession(t, s, base)
	openWindow(t, s, window{
		key: "w-worker", session: string(sess.ID), role: domain.WorkflowRoleWorker,
		step: "step-work", opened: base, harness: "claude-code", provider: "anthropic",
	})
	applyEvents(t, s, source, base.Add(time.Minute), []domain.ModelUsageEvent{
		attrEvent("msg-1", 1000, 200, base.Add(time.Minute)),
	})
	events, err := s.ListRunContextTrajectoryEvents(ctx, attrRunID)
	mustNoError(t, err, "list trajectory events")
	if got := singleTurnClass(t, len(events), func() domain.TurnClass { return events[0].TurnClass }); got != domain.TurnUnclassified {
		t.Fatalf("class = %q, want unclassified", got)
	}
}
