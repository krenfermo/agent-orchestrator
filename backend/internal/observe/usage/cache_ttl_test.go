package usage

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// cache_ttl_test.go -- the lifetime a cache entry was created with.
//
// Creating a longer-lived cache entry costs more. The provider reports which
// one it made, on every message; AO folded both into one number and priced all
// of it at the cheap rate. These tests pin the decoding, the arithmetic that
// must never double count, and the one rule that matters more than either:
// a lifetime nothing reported is UNKNOWN, never five minutes and never an hour.

func usageRecord(id string, unc, read, write, out int64, creation string) string {
	return fmt.Sprintf(
		`{"type":"assistant","isSidechain":false,"uuid":"%s","timestamp":"2026-09-11T20:54:29.736Z",`+
			`"message":{"id":"%s","model":"claude-opus-5","stop_reason":"tool_use","usage":`+
			`{"input_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,`+
			`"output_tokens":%d%s}}}`,
		id, id, unc, read, write, out, creation)
}

func creationBlock(five, hour int64) string {
	return fmt.Sprintf(`,"cache_creation":{"ephemeral_5m_input_tokens":%d,"ephemeral_1h_input_tokens":%d}`, five, hour)
}

func TestCacheCreationLifetimeIsDecoded(t *testing.T) {
	for _, tc := range []struct {
		name       string
		creation   string
		write      int64
		want       domain.CacheCreationSplit
		wantAnomly bool
	}{
		{
			name: "only short lived", creation: creationBlock(900, 0), write: 900,
			want: domain.CacheCreationSplit{Ephemeral5mTokens: 900},
		},
		{
			name: "only long lived", creation: creationBlock(0, 2326), write: 2326,
			want: domain.CacheCreationSplit{Ephemeral1hTokens: 2326},
		},
		{
			name: "both", creation: creationBlock(400, 600), write: 1000,
			want: domain.CacheCreationSplit{Ephemeral5mTokens: 400, Ephemeral1hTokens: 600},
		},
		{
			name: "no creation at all", creation: creationBlock(0, 0), write: 0,
			want: domain.CacheCreationSplit{},
		},
		{
			// Legacy: a harness that predates the vocabulary. The total is
			// real and stays; the lifetime is absent and must not be invented.
			name: "legacy, total only", creation: "", write: 5240,
			want: domain.CacheCreationSplit{},
		},
		{
			// The split and the total disagree. Neither is trusted over the
			// other, nothing is apportioned, and the whole figure becomes
			// lifetime-unknown.
			name: "split disagrees with the total", creation: creationBlock(100, 100), write: 900,
			want: domain.CacheCreationSplit{UnknownTTLTokens: 900}, wantAnomly: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := usageSource(domain.UsageSourceClaudeMain)
			result := parseRecords(source, []jsonlRecord{
				{Data: []byte(usageRecord("m1", 2, 1000, tc.write, 50, tc.creation))},
			}, 500, time.Unix(1700000000, 0).UTC())
			if result.err != nil {
				t.Fatalf("parse: %v", result.err)
			}
			if len(result.Events) != 1 {
				t.Fatalf("events = %d, want 1", len(result.Events))
			}
			got := result.Events[0].Tokens
			if got.CacheCreation != tc.want {
				t.Fatalf("split = %+v, want %+v", got.CacheCreation, tc.want)
			}
			// The total is untouched in every case: the split is a division of
			// it and never a replacement for it.
			if got.CacheWriteTokens != tc.write {
				t.Fatalf("total = %d, want %d", got.CacheWriteTokens, tc.write)
			}
			anomaly := result.Cursor.AnomalyCount > 0
			if anomaly != tc.wantAnomly {
				t.Fatalf("anomaly = %v (code %q), want %v", anomaly, result.Cursor.LastErrorCode, tc.wantAnomly)
			}
			if tc.wantAnomly && result.Cursor.LastErrorCode != domain.UsageErrorCacheTTLInconsistent {
				t.Fatalf("error code = %q", result.Cursor.LastErrorCode)
			}
		})
	}
}

func TestTheSplitIsNeverAddedToTheTotal(t *testing.T) {
	source := usageSource(domain.UsageSourceClaudeMain)
	result := parseRecords(source, []jsonlRecord{
		{Data: []byte(usageRecord("m1", 2, 1000, 1000, 50, creationBlock(400, 600)))},
	}, 500, time.Unix(1700000000, 0).UTC())
	got := result.Events[0].Tokens
	// input_tokens is uncached + read + creation, counted ONCE.
	if got.InputTokens != 2+1000+1000 {
		t.Fatalf("input = %d, want 2002 -- the split must not be added a second time", got.InputTokens)
	}
	if got.CacheCreation.Total() != got.CacheWriteTokens {
		t.Fatalf("split total %d != write total %d", got.CacheCreation.Total(), got.CacheWriteTokens)
	}
}

func TestSessionLifetimeAggregateCountsEachMessageOnce(t *testing.T) {
	// One billed message, three transcript records -- the ordinary shape, and
	// the one that would triple-count a per-record accumulation.
	one := usageRecord("m1", 2, 1000, 900, 50, creationBlock(0, 900))
	_, state := parseClaudeRecords(t, one, one, one,
		usageRecord("m2", 2, 1900, 300, 20, creationBlock(300, 0)))
	if state.CacheCreation1h != 900 || state.CacheCreation5m != 300 {
		t.Fatalf("aggregate = 5m %d / 1h %d, want 300 / 900",
			state.CacheCreation5m, state.CacheCreation1h)
	}
	if state.CacheCreationTotal != 1200 || state.CacheCreationMessages != 2 {
		t.Fatalf("total = %d over %d messages, want 1200 over 2",
			state.CacheCreationTotal, state.CacheCreationMessages)
	}
	if state.CacheCreationUnknown != 0 {
		t.Fatalf("unknown = %d, want 0", state.CacheCreationUnknown)
	}
}

func TestLegacyMessagesAggregateAsLifetimeUnknown(t *testing.T) {
	_, state := parseClaudeRecords(t,
		usageRecord("m1", 2, 1000, 900, 50, ""),
		usageRecord("m2", 2, 1900, 300, 20, creationBlock(0, 300)))
	if state.CacheCreationUnknown != 900 {
		t.Fatalf("unknown = %d, want the legacy message's 900", state.CacheCreationUnknown)
	}
	if state.CacheCreation1h != 300 {
		t.Fatalf("1h = %d, want 300", state.CacheCreation1h)
	}
	if state.CacheCreationTotal != 1200 {
		t.Fatalf("total = %d, want 1200", state.CacheCreationTotal)
	}
}

func TestATruncatedRecordContributesNoLifetime(t *testing.T) {
	full := usageRecord("m1", 2, 1000, 900, 50, creationBlock(0, 900))
	result, state := parseClaudeRecords(t, full[:len(full)-25])
	if state.CacheCreationTotal != 0 || state.CacheCreation1h != 0 {
		t.Fatalf("a truncated record contributed %+v", state)
	}
	if result.Cursor.AnomalyCount == 0 {
		t.Fatal("a malformed record must raise an anomaly")
	}
}

func TestLifetimeObservationsReadBackAndCarryNoContent(t *testing.T) {
	source := usageSource(domain.UsageSourceClaudeMain)
	result := parseRecords(source, []jsonlRecord{
		{Data: []byte(usageRecord("m1", 2, 1000, 900, 50, creationBlock(0, 900)))},
		{Data: []byte(usageRecord("m2", 2, 1900, 300, 20, creationBlock(300, 0)))},
	}, 900, time.Unix(1700000000, 0).UTC())
	if result.err != nil {
		t.Fatalf("parse: %v", result.err)
	}
	record := domain.UsageSourceRecord{
		ID: 7, Kind: domain.UsageSourceClaudeMain,
		ByteOffset:      result.Cursor.ByteOffset,
		ParserStateJSON: result.Cursor.ParserStateJSON,
		State:           domain.UsageSourceActive,
	}
	got := ExtractCompactionObservations(record, "s", domain.HarnessClaudeCode)
	if got.CacheCreation.Ephemeral1hTokens != 900 || got.CacheCreation.Ephemeral5mTokens != 300 {
		t.Fatalf("read back %+v", got.CacheCreation)
	}
	if got.CacheCreationTotal != 1200 {
		t.Fatalf("total read back as %d", got.CacheCreationTotal)
	}
	// The state is numbers. Nothing from a message can reach it.
	for _, forbidden := range []string{"claude-opus-5\",\"stop", "tool_use", "ephemeral"} {
		if strings.Contains(result.Cursor.ParserStateJSON, forbidden) {
			t.Fatalf("parser state leaked %q: %s", forbidden, result.Cursor.ParserStateJSON)
		}
	}
}

func TestStateWithoutLifetimeFieldsStillDecodes(t *testing.T) {
	// A source written before this checkpoint: valid state, no lifetime keys.
	legacy := `{"version":1,"source_kind":"claude_main","claude":{"model_id":"claude-opus-5","compaction_count":2}}`
	got := ExtractCompactionObservations(domain.UsageSourceRecord{
		ID: 1, Kind: domain.UsageSourceClaudeMain, ParserStateJSON: legacy, State: domain.UsageSourceActive,
	}, "s", domain.HarnessClaudeCode)
	if got.Count != 2 {
		t.Fatalf("pre-existing state must still decode, got count %d", got.Count)
	}
	if got.CacheCreation.Total() != 0 {
		t.Fatalf("absent lifetime fields must read as nothing, got %+v", got.CacheCreation)
	}
}
