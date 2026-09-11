package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// usage_trajectory_store_test.go -- the SQL behind the context-shape read,
// against a real database.
//
// The fold that turns a series into a shape is tested in
// internal/service/usage/dynamics_test.go with canned rows. What can only be
// tested here is the part the fold TRUSTS: that the rows come back in provider
// order, carry the step that incurred them, and leave out the events AO cannot
// place in time rather than guessing a position for them.

func TestTrajectoryEventsComeBackInProviderOrderWithTheirStep(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Unix(1700000000, 0).UTC()
	sess, source := seedAttributionSession(t, s, base)

	openWindow(t, s, window{
		key: "w-worker", session: string(sess.ID), role: domain.WorkflowRoleWorker,
		step: "step-work", opened: base, harness: "claude-code", provider: "anthropic",
	})
	openWindow(t, s, window{
		key: "w-fix", session: string(sess.ID), role: domain.WorkflowRoleFixWorker,
		step: "step-fix", cycle: 1, opened: base.Add(30 * time.Minute),
		harness: "claude-code", provider: "anthropic",
	})

	// Deliberately applied out of provider order, so the ORDER BY is what puts
	// them right rather than the insertion sequence.
	applyEvents(t, s, source, base.Add(time.Hour), []domain.ModelUsageEvent{
		attrEvent("e2", 2000, 300, base.Add(10*time.Minute)),
		attrEvent("e1", 1000, 200, base.Add(5*time.Minute)),
		attrEvent("e3", 4000, 100, base.Add(40*time.Minute)),
	})

	events, err := s.ListRunContextTrajectoryEvents(ctx, attrRunID)
	mustNoError(t, err, "list trajectory events")
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3", len(events))
	}
	wantContext := []int64{1000, 2000, 4000}
	wantStep := []string{"step-work", "step-work", "step-fix"}
	for i, ev := range events {
		if ev.Tokens.InputTokens != wantContext[i] {
			t.Errorf("event %d context = %d, want %d (series out of order)", i, ev.Tokens.InputTokens, wantContext[i])
		}
		if ev.WorkflowStepID != wantStep[i] {
			t.Errorf("event %d step = %q, want %q", i, ev.WorkflowStepID, wantStep[i])
		}
	}
	// The repair's event must belong to the repair's step and cycle, or a
	// per-step cost would silently charge the base step for the re-work.
	if events[2].Cycle != 1 || events[2].Role != domain.WorkflowRoleFixWorker {
		t.Errorf("last event = cycle %d role %q, want cycle 1 fix_worker", events[2].Cycle, events[2].Role)
	}
}

func TestAnEventWithNoProviderTimestampIsCountedNotPlaced(t *testing.T) {
	// The attribution ledger keeps such an event and folds it into the run's
	// TOTAL under an approximate basis -- that behavior is covered next door and
	// is not changed here. What this pins is that it stays OUT of the series:
	// an event with no time has no position, and inventing one would corrupt
	// whichever end of the trajectory it was pushed onto.
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Unix(1700000000, 0).UTC()
	sess, source := seedAttributionSession(t, s, base)

	openWindow(t, s, window{
		key: "w-worker", session: string(sess.ID), role: domain.WorkflowRoleWorker,
		step: "step-work", opened: base, harness: "claude-code", provider: "anthropic",
	})
	applyEvents(t, s, source, base.Add(time.Hour), []domain.ModelUsageEvent{
		attrEvent("e1", 1000, 200, base.Add(5*time.Minute)),
		attrEvent("e-untimed", 90000, 100, time.Time{}),
		attrEvent("e2", 3000, 300, base.Add(10*time.Minute)),
	})

	events, err := s.ListRunContextTrajectoryEvents(ctx, attrRunID)
	mustNoError(t, err, "list trajectory events")
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2: an unplaceable event must not enter the series", len(events))
	}
	if events[len(events)-1].Tokens.InputTokens != 3000 {
		t.Errorf("series ends at %d, want 3000 -- the 90k untimed event was ordered into the series",
			events[len(events)-1].Tokens.InputTokens)
	}

	unplaceable, err := s.CountRunUnplaceableUsageEvents(ctx, attrRunID)
	mustNoError(t, err, "count unplaceable")
	if unplaceable != 1 {
		t.Errorf("unplaceable = %d, want 1: what was left out has to be countable, or the series would imply completeness", unplaceable)
	}
}

func TestTrajectoryCarriesTheCacheDimensionsSeparately(t *testing.T) {
	// 98.8% of the worked example's bill was cache reads. A read model that
	// folded them into one input figure could not say so, and the whole
	// explanation of the cost would be gone.
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Unix(1700000000, 0).UTC()
	sess, source := seedAttributionSession(t, s, base)
	openWindow(t, s, window{
		key: "w-worker", session: string(sess.ID), role: domain.WorkflowRoleWorker,
		step: "step-work", opened: base, harness: "claude-code", provider: "anthropic",
	})
	ev := domain.ModelUsageEvent{
		ModelID: "claude-opus-5", SourceEventKey: "e1",
		ObservedAt: timePtrUTC(base.Add(time.Minute)),
		Tokens: domain.UsageTokenMetrics{
			InputTokens: 100000, UncachedInputTokens: 1000,
			CacheReadTokens: 98000, CacheWriteTokens: 1000, OutputTokens: 500,
		},
	}
	applyEvents(t, s, source, base.Add(time.Hour), []domain.ModelUsageEvent{ev})

	events, err := s.ListRunContextTrajectoryEvents(ctx, attrRunID)
	mustNoError(t, err, "list trajectory events")
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	got := events[0].Tokens
	if got.CacheReadTokens != 98000 || got.CacheWriteTokens != 1000 || got.UncachedInputTokens != 1000 {
		t.Fatalf("cache dimensions = %+v, want read 98000 / write 1000 / uncached 1000", got)
	}
	if share, dominant := got.CacheReadDominant(); !dominant || share != 98 {
		t.Errorf("cache dominance = %d%% (%t), want 98%% dominant", share, dominant)
	}
}

func TestALegacyRunWithNoWindowsHasNoTrajectory(t *testing.T) {
	s := newTestStore(t)
	events, err := s.ListRunContextTrajectoryEvents(context.Background(), "wf-never-existed")
	mustNoError(t, err, "list trajectory events")
	if len(events) != 0 {
		t.Fatalf("events = %d, want 0", len(events))
	}
}
