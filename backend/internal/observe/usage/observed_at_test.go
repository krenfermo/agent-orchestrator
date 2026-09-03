package usage

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// observed_at_test.go — P3-E reads the provider's own event time out of the
// transcript so a token can be placed inside the role window that was open when
// it was spent.
//
// Two rules are load-bearing and both are asserted here. The timestamp must
// NEVER enter SourceEventKey — an identity that moved with a clock would stop
// being exactly-once the moment a transcript was re-read — and an absent or
// unparseable timestamp must yield nil rather than "now", because a fabricated
// instant silently files a token under the wrong role.

func TestParseClaude_ReadsTheProviderEventTime(t *testing.T) {
	source := usageSource(domain.UsageSourceClaudeMain)
	records := []jsonlRecord{{Offset: 0, Data: []byte(
		`{"type":"assistant","uuid":"u1","timestamp":"2026-07-01T10:01:02.500Z","message":{"id":"m1","model":"claude-opus-5","stop_reason":"end_turn","usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":4}}}`)}}
	result := parseRecords(source, records, 500, time.Unix(1700000000, 0).UTC())
	if len(result.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(result.Events))
	}
	got := result.Events[0].ObservedAt
	if got == nil {
		t.Fatal("ObservedAt is nil; the record carried a timestamp")
	}
	want := time.Date(2026, 7, 1, 10, 1, 2, 500_000_000, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("ObservedAt = %s, want %s", got, want)
	}
}

func TestParseClaude_NoTimestampStaysNilRatherThanNow(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	source := usageSource(domain.UsageSourceClaudeMain)
	records := []jsonlRecord{{Offset: 0, Data: []byte(
		`{"type":"assistant","uuid":"u1","message":{"id":"m1","model":"claude-opus-5","stop_reason":"end_turn","usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":4}}}`)}}
	result := parseRecords(source, records, 500, now)
	if len(result.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(result.Events))
	}
	if result.Events[0].ObservedAt != nil {
		t.Fatalf("ObservedAt = %v, want nil — an invented instant files a token under the wrong role",
			result.Events[0].ObservedAt)
	}
}

func TestParseClaude_UnparseableTimestampStaysNil(t *testing.T) {
	source := usageSource(domain.UsageSourceClaudeMain)
	records := []jsonlRecord{{Offset: 0, Data: []byte(
		`{"type":"assistant","uuid":"u1","timestamp":"not-a-time","message":{"id":"m1","model":"claude-opus-5","stop_reason":"end_turn","usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":4}}}`)}}
	result := parseRecords(source, records, 500, time.Unix(1700000000, 0).UTC())
	if len(result.Events) != 1 || result.Events[0].ObservedAt != nil {
		t.Fatalf("an unparseable timestamp must not become a time, got %+v", result.Events)
	}
	// And the event is still emitted: the tokens are real whatever the clock said.
	if result.Events[0].Tokens.OutputTokens != 4 {
		t.Fatalf("tokens = %+v, want the event kept", result.Events[0].Tokens)
	}
}

func TestParseCodex_ReadsTheEnvelopeTimestamp(t *testing.T) {
	source := usageSource(domain.UsageSourceCodexRollout)
	records := []jsonlRecord{
		{Offset: 0, Data: []byte(`{"type":"turn_context","payload":{"model":"gpt-5.6"}}`)},
		{Offset: 100, Data: codexTokenLine("2026-07-01T10:00:02Z", 160, 90, 10, 35, 8)},
	}
	result := parseRecords(source, records, 500, time.Unix(1700000000, 0).UTC())
	if len(result.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(result.Events))
	}
	got := result.Events[0].ObservedAt
	if got == nil || !got.Equal(time.Date(2026, 7, 1, 10, 0, 2, 0, time.UTC)) {
		t.Fatalf("ObservedAt = %v, want 2026-07-01T10:00:02Z", got)
	}
}

func TestObservedAtIsNotPartOfTheEventIdentity(t *testing.T) {
	// The exactly-once guarantee lives in SourceEventKey. If the timestamp
	// leaked into it, a transcript whose clock rendered differently on a
	// re-read would produce a SECOND row for the same spend.
	source := usageSource(domain.UsageSourceClaudeMain)
	withTime := parseRecords(source, []jsonlRecord{{Offset: 0, Data: []byte(
		`{"type":"assistant","uuid":"u1","timestamp":"2026-07-01T10:01:02Z","message":{"id":"m1","model":"claude-opus-5","stop_reason":"end_turn","usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":4}}}`)}},
		500, time.Unix(1700000000, 0).UTC())
	withOtherTime := parseRecords(source, []jsonlRecord{{Offset: 0, Data: []byte(
		`{"type":"assistant","uuid":"u1","timestamp":"2026-07-01T23:59:59Z","message":{"id":"m1","model":"claude-opus-5","stop_reason":"end_turn","usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":4}}}`)}},
		500, time.Unix(1700000000, 0).UTC())

	if withTime.Events[0].SourceEventKey != withOtherTime.Events[0].SourceEventKey {
		t.Fatal("SourceEventKey moved with the timestamp; the ledger would double count on a re-read")
	}
}
