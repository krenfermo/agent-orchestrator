package store_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/usage/pricing"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// applyTTLEvents seeds a claude session and writes the events through the real
// ingest path, so what is asserted below is what production stores.
func applyTTLEvents(t *testing.T, events ...domain.ModelUsageEvent) (*sqlite.Store, domain.SessionID) {
	t.Helper()
	s := newTestStore(t)
	sess := seedUsageSession(t, s, domain.HarnessClaudeCode)
	now := time.Unix(1700000000, 0).UTC()
	source := seedUsageSource(t, s, sess, now)
	err := s.ApplyUsageChunk(context.Background(), source.ID, 0, source.UpdatedAt, domain.SourceCursorState{
		ByteOffset:      100,
		State:           domain.UsageSourceActive,
		ParserStateJSON: `{"version":1,"source_kind":"claude_main","claude":{"model_id":"claude-opus-5"}}`,
		UpdatedAt:       now,
	}, events)
	if err != nil {
		t.Fatalf("apply chunk: %v", err)
	}
	return s, sess.ID
}

// usage_cache_ttl_store_test.go -- the lifetime survives the round trip.
//
// P7.2B1 decoded which cache lifetime each call created and had nowhere to put
// it, so the ledger kept pricing a long-lived write at the short-lived rate and
// disclosed an assumption instead. Migration 0170 gave it a column pair. These
// tests pin the one property that makes the pair worth having: what the parser
// decoded is what the store keeps, what the reader returns, and what pricing
// charges -- and a row that never carried a lifetime is still readable and is
// never mistaken for one that said "short".

// ttlEvent builds one event with an explicit lifetime split.
func ttlEvent(key string, read, five, hour, out int64, at time.Time) domain.ModelUsageEvent {
	return ttlEventWithUncached(key, 0, read, five, hour, out, at)
}

func ttlEventWithUncached(key string, unc, read, five, hour, out int64, at time.Time) domain.ModelUsageEvent {
	write := five + hour
	return domain.ModelUsageEvent{
		ModelID: "claude-opus-5",
		Tokens: domain.UsageTokenMetrics{
			InputTokens:         unc + read + write,
			UncachedInputTokens: unc,
			CacheReadTokens:     read,
			CacheWriteTokens:    write,
			OutputTokens:        out,
			CacheCreation: domain.CacheCreationSplit{
				Ephemeral5mTokens: five, Ephemeral1hTokens: hour,
			},
		},
		SourceEventKey: key,
		ObservedAt:     &at,
	}
}

// legacyEvent builds one event whose lifetime nothing reported.
func legacyEvent(key string, read, write, out int64, at time.Time) domain.ModelUsageEvent {
	return domain.ModelUsageEvent{
		ModelID: "claude-opus-5",
		Tokens: domain.UsageTokenMetrics{
			InputTokens: read + write, CacheReadTokens: read,
			CacheWriteTokens: write, OutputTokens: out,
		},
		SourceEventKey: key,
		ObservedAt:     &at,
	}
}

func TestCacheLifetimeSurvivesTheRoundTrip(t *testing.T) {
	at := time.Date(2026, 9, 11, 20, 54, 29, 0, time.UTC)
	st, session := applyTTLEvents(t,
		ttlEvent("e-long", 82702, 0, 2193, 950, at),
		ttlEvent("e-short", 85000, 900, 0, 40, at.Add(time.Minute)),
		ttlEvent("e-both", 86000, 400, 600, 30, at.Add(2*time.Minute)),
		legacyEvent("e-legacy", 87000, 5240, 20, at.Add(3*time.Minute)),
	)
	got, err := st.ListUsageModelAggregates(context.Background(), session)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("aggregates = %d, want 1", len(got))
	}
	split := got[0].Tokens.CacheCreation
	// 900 + 400 short-lived, 2193 + 600 long-lived, and the legacy row's 5,240
	// reported as what it is: a quantity whose lifetime was never observed.
	if split.Ephemeral5mTokens != 1300 || split.Ephemeral1hTokens != 2793 {
		t.Fatalf("split = %+v, want 1300 / 2793", split)
	}
	if split.UnknownTTLTokens != 5240 {
		t.Fatalf("unknown = %d, want the legacy row's 5240", split.UnknownTTLTokens)
	}
	if split.Total() != got[0].Tokens.CacheWriteTokens {
		t.Fatalf("the split (%d) must account for the whole total (%d)",
			split.Total(), got[0].Tokens.CacheWriteTokens)
	}
}

func TestALegacyRowIsNeverReadAsShortLived(t *testing.T) {
	at := time.Date(2026, 9, 11, 20, 54, 29, 0, time.UTC)
	st, session := applyTTLEvents(t, legacyEvent("only-legacy", 1000, 5240, 20, at))
	got, err := st.ListUsageModelAggregates(context.Background(), session)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	split := got[0].Tokens.CacheCreation
	if split.Ephemeral5mTokens != 0 || split.Ephemeral1hTokens != 0 {
		t.Fatalf("a lifetime nobody reported must not become a lifetime, got %+v", split)
	}
	if split.Known() {
		t.Fatal("the split must report itself as incomplete")
	}
	// And pricing must say so rather than presenting the figure as exact.
	cost := pricing.Embedded().Cost("claude-opus-5", domain.UsageTokenTotals{
		CacheReadTokens: 1000, CacheWriteTokens: 5240, OutputTokens: 20,
		CacheCreation: split,
	})
	if cost.TTLAssumedTokens != 5240 {
		t.Fatalf("assumed = %d, want the whole legacy quantity disclosed", cost.TTLAssumedTokens)
	}
}

func TestAnInconsistentSplitIsNotPersisted(t *testing.T) {
	at := time.Date(2026, 9, 11, 20, 54, 29, 0, time.UTC)
	// A split that does not add up to its own total. The parser already raised
	// the anomaly; the store must not put the contradiction in the ledger where
	// every later sum would inherit it.
	bad := ttlEvent("inconsistent", 1000, 100, 100, 10, at)
	bad.Tokens.CacheWriteTokens = 900
	bad.Tokens.InputTokens = 1900
	st, session := applyTTLEvents(t, bad)
	got, err := st.ListUsageModelAggregates(context.Background(), session)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	split := got[0].Tokens.CacheCreation
	if split.Ephemeral5mTokens != 0 || split.Ephemeral1hTokens != 0 {
		t.Fatalf("a contradictory split must not be stored, got %+v", split)
	}
	if split.UnknownTTLTokens != 900 {
		t.Fatalf("unknown = %d, want the whole total 900", split.UnknownTTLTokens)
	}
}

// TestTheMeasuredSessionsReconcileThroughTheDurablePath replays the two known
// runs' real token vectors through the write path and prices what comes back
// out of the reader. Every figure is measured: the tokens from AO's own ledger,
// the lifetime from the transcripts' cache_creation blocks, the target from the
// harness's own end-of-session rollup.
func TestTheMeasuredSessionsReconcileThroughTheDurablePath(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		unc, read, five, hour, out int64
		harnessReported            float64
		wantAssumed, wantExact     float64
	}{
		{
			name: "wf-1c2cb9bd / medusa-12, 193 calls, never compacted",
			unc:  386, read: 35_787_742, five: 0, hour: 294_688, out: 139_003,
			harnessReported: 24.805628,
			wantAssumed:     23.2127, wantExact: 24.3178,
		},
		{
			name: "wf-66f0ee54 / ao-canary-fixture-6, 28 calls, compacted twice",
			unc:  56, read: 1_719_470, five: 0, hour: 110_284, out: 30_500,
			harnessReported: 3.4028525,
			wantAssumed:     2.3118, wantExact: 2.7254,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Date(2026, 9, 11, 20, 54, 29, 0, time.UTC)
			st, session := applyTTLEvents(t,
				ttlEventWithUncached("measured", tc.unc, tc.read, tc.five, tc.hour, tc.out, at))
			got, err := st.ListUsageModelAggregates(context.Background(), session)
			if err != nil {
				t.Fatalf("aggregate: %v", err)
			}
			split := got[0].Tokens.CacheCreation
			if split.Ephemeral1hTokens != tc.hour || split.Ephemeral5mTokens != tc.five {
				t.Fatalf("lifetime lost in the round trip: %+v", split)
			}
			if !split.Known() {
				t.Fatal("nothing about this vector is unknown")
			}

			table := pricing.Embedded()
			vector := domain.UsageTokenTotals{
				UncachedInputTokens: got[0].Tokens.UncachedInputTokens,
				CacheReadTokens:     got[0].Tokens.CacheReadTokens,
				CacheWriteTokens:    got[0].Tokens.CacheWriteTokens,
				OutputTokens:        got[0].Tokens.OutputTokens,
				CacheCreation:       split,
			}
			exact := table.Cost("claude-opus-5", vector)
			if exact.TTLAssumedTokens != 0 {
				t.Fatalf("a durable lifetime leaves nothing assumed, got %d", exact.TTLAssumedTokens)
			}
			if math.Abs(exact.Amount-tc.wantExact) > 0.001 {
				t.Fatalf("exact = %.4f, want %.4f", exact.Amount, tc.wantExact)
			}
			// What the same vector cost before the lifetime was durable.
			blind := vector
			blind.CacheCreation = domain.CacheCreationSplit{}
			assumed := table.Cost("claude-opus-5", blind)
			if assumed.TTLAssumedTokens != tc.hour+tc.five {
				t.Fatalf("the pre-0170 figure assumes the whole quantity, got %d", assumed.TTLAssumedTokens)
			}
			if math.Abs(assumed.Amount-tc.wantAssumed) > 0.001 {
				t.Fatalf("assumed = %.4f, want %.4f", assumed.Amount, tc.wantAssumed)
			}
			// The lifetime moves AO's figure toward the harness's own, and
			// stops short of it: what remains is spend outside the calls,
			// which is P7.1's subject and not this migration's.
			if exact.Amount <= assumed.Amount {
				t.Fatal("the true lifetime costs more, not less")
			}
			if exact.Amount >= tc.harnessReported {
				t.Fatalf("exact (%.4f) must stay under the harness figure (%.4f)",
					exact.Amount, tc.harnessReported)
			}
			t.Logf("explained: %.1f%% -> %.1f%%",
				100*assumed.Amount/tc.harnessReported, 100*exact.Amount/tc.harnessReported)
		})
	}
}
