package usage

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// compaction_forward_test.go -- P7.2B2.1 §5: the forward observation path, end to
// end, on a transcript shaped like a real compacting session.
//
// P7.2B2's review found that the real corpus holds ZERO compaction observations,
// and the obvious worry was that the parser might be at fault. It is not. The
// existing unit tests cover each record in isolation; what was missing -- and what
// this file supplies -- is the whole shape in one pass, from offset zero, with the
// cache-creation lifetimes and the harness's reasoning figure present, followed by
// a resume from the persisted offset.
//
// NO LLM, no network, no file. The fixture is the shape taken from the real canary
// transcript, and every content-bearing field is kept in it ON PURPOSE so the
// privacy assertion has something real to fail on.

// claudeTurnWithLifetimes is an assistant record whose cache creation is split by
// the lifetime the provider created it at -- the field migration 0170 and the
// P7.2B1 backfill exist for. The `text` and `thinking` blocks are content and
// must never survive into an observation.
func claudeTurnWithLifetimes(id, model string, read, create5m, create1h, output int64) string {
	return fmt.Sprintf(
		`{"type":"assistant","isSidechain":false,"uuid":"%s","timestamp":"2026-09-11T20:54:29.736Z",`+
			`"cwd":"/Users/someone/secret-project","gitBranch":"ao/wf-1",`+
			`"message":{"id":"%s","model":"%s","stop_reason":"end_turn",`+
			`"content":[{"type":"thinking","thinking":"SECRET REASONING about the password"},`+
			`{"type":"text","text":"SECRET ANSWER naming /etc/shadow"}],`+
			`"usage":{"input_tokens":12,"cache_read_input_tokens":%d,`+
			`"cache_creation_input_tokens":%d,`+
			`"cache_creation":{"ephemeral_5m_input_tokens":%d,"ephemeral_1h_input_tokens":%d},`+
			`"output_tokens":%d}}}`,
		id, id, model, read, create5m+create1h, create5m, create1h, output)
}

// compactingSessionFixture is the whole shape: two ordinary turns, the boundary
// the harness writes when it replaces the conversation, the first turn after it
// (which rewrites the new prefix, so its cache creation is large), another
// ordinary turn, and finally the once-per-session rollup the harness appends when
// the session ends.
func compactingSessionFixture() []string {
	return []string{
		claudeTurnWithLifetimes("turn-1", "claude-opus-5", 30_000, 0, 4_000, 150),
		claudeTurnWithLifetimes("turn-2", "claude-opus-5", 34_000, 0, 2_000, 200),
		compactBoundaryRecord,
		// The first call after a compaction rewrites the prefix: a large
		// one-hour creation, which is the single biggest cache write in a
		// session and the term the economic gate is most sensitive to.
		claudeTurnWithLifetimes("turn-3", "claude-opus-5", 37_379, 0, 20_422, 300),
		claudeTurnWithLifetimes("turn-4", "claude-opus-5", 58_000, 500, 1_500, 250),
		costStateRecord,
	}
}

// TestANewSessionProducesEveryObservationTheEconomicGateNeeds is the answer to
// "would a session created today produce the evidence?" -- demonstrated rather
// than assumed.
func TestANewSessionProducesEveryObservationTheEconomicGateNeeds(t *testing.T) {
	lines := compactingSessionFixture()
	result, state := parseClaudeRecords(t, lines...)

	// --- the boundary -------------------------------------------------------
	if state.CompactionCount != 1 || len(state.Compactions) != 1 {
		t.Fatalf("compactions = %d observed / %d detailed, want exactly 1 of each",
			state.CompactionCount, len(state.Compactions))
	}
	boundary := state.Compactions[0]
	if boundary.PreTokens != 85847 {
		t.Errorf("preTokens = %d, want 85847", boundary.PreTokens)
	}
	if boundary.PostTokens != 10956 {
		t.Errorf("postTokens = %d, want 10956", boundary.PostTokens)
	}
	if boundary.DurationMs != 95073 {
		t.Errorf("durationMs = %d, want 95073", boundary.DurationMs)
	}
	if boundary.Trigger != string(domain.CompactionTriggerManual) {
		t.Errorf("trigger = %q, want manual", boundary.Trigger)
	}
	if boundary.Timestamp == "" {
		t.Error("a boundary with no timestamp cannot be ordered against a decision, and the estimator will refuse it")
	}
	// Identity: the model the session was running when it compacted, carried
	// from the turns that preceded the boundary.
	if boundary.ModelID != "claude-opus-5" {
		t.Errorf("boundary model = %q, want claude-opus-5", boundary.ModelID)
	}
	if boundary.UUID == "" {
		t.Error("a boundary with no uuid has no idempotency key")
	}

	// --- the harness rollup, which is the ONLY source of a summary cost -----
	if state.HarnessTotals == nil {
		t.Fatal("no harness rollup observed; without it the summary cost is unknowable")
	}
	var opus *harnessModelTotalsV1
	for i := range state.HarnessTotals.Models {
		if state.HarnessTotals.Models[i].ModelID == "claude-opus-5[1m]" {
			opus = &state.HarnessTotals.Models[i]
		}
	}
	if opus == nil {
		t.Fatalf("the rollup did not carry the session's own model; got %+v", state.HarnessTotals.Models)
	}
	if opus.OutputTokens != 47453 {
		t.Errorf("rollup output = %d, want 47453", opus.OutputTokens)
	}
	// REASONING IS THE HALF THE DESIGN'S FIRST ESTIMATE MISSED. Counting the
	// characters of a summary's text understated it roughly twofold, because most
	// of what a summarizer generates is thinking, which the text does not contain.
	if opus.ThinkingTokens != 6326 {
		t.Errorf("rollup thinking = %d, want 6326: reasoning is most of a summarization turn", opus.ThinkingTokens)
	}
	if opus.CacheWriteTokens != 115415 || opus.CacheReadTokens != 2083095 {
		t.Errorf("the rollup must carry the full pricing-relevant vector, got %+v", opus)
	}

	// --- the cache-creation lifetimes --------------------------------------
	// 4,000 + 2,000 + 20,422 + 1,500 at one hour; 500 at five minutes; nothing
	// unknown. The split is what lets the gate price the rewrite at the rate the
	// provider actually charges.
	if state.CacheCreation1h != 27_922 {
		t.Errorf("1h creation = %d, want 27922", state.CacheCreation1h)
	}
	if state.CacheCreation5m != 500 {
		t.Errorf("5m creation = %d, want 500", state.CacheCreation5m)
	}
	if state.CacheCreationUnknown != 0 {
		t.Errorf("unknown-lifetime creation = %d, want 0: every record reported its split", state.CacheCreationUnknown)
	}
	if state.CacheCreationTotal != state.CacheCreation5m+state.CacheCreation1h {
		t.Errorf("the split must partition the reported total: %d vs %d+%d",
			state.CacheCreationTotal, state.CacheCreation5m, state.CacheCreation1h)
	}

	// --- the ledger side is untouched by either observation ----------------
	// Four assistant turns, four events. The boundary and the rollup bill
	// nothing: they are observations, and a transcript containing them must
	// produce exactly the events it would have produced without them.
	if len(result.Events) != 4 {
		t.Fatalf("usage events = %d, want 4 (one per assistant turn; observations bill nothing)", len(result.Events))
	}
	for _, ev := range result.Events {
		if ev.Tokens.CacheCreation.Total() != ev.Tokens.CacheWriteTokens {
			t.Errorf("event %s: the per-event split must partition its own cache write", ev.SourceEventKey)
		}
	}

	// --- privacy ------------------------------------------------------------
	assertObservationsCarryNoContent(t, state)
}

// TestObservationsSurviveAParserResumeFromThePersistedOffset is the resume half of
// §5: the parser stops mid-session, its state is persisted, and the next batch is
// parsed on top of it. Observations from the first batch must survive, and the
// second batch must not double-count the boundary it has already seen.
func TestObservationsSurviveAParserResumeFromThePersistedOffset(t *testing.T) {
	lines := compactingSessionFixture()

	// Batch one stops immediately AFTER the boundary -- the worst place to be
	// interrupted, because the boundary is now in durable state and the turns
	// that follow it are not yet parsed.
	firstBatch, secondBatch := lines[:3], lines[3:]

	_, stateAfterFirst := parseClaudeRecords(t, firstBatch...)
	if stateAfterFirst.CompactionCount != 1 {
		t.Fatalf("batch one: compactions = %d, want 1", stateAfterFirst.CompactionCount)
	}
	if stateAfterFirst.HarnessTotals != nil {
		t.Error("batch one contains no rollup; a session still running has none, and that is not a zero")
	}

	// Resume: the persisted state becomes the source's state, exactly as the
	// store hands it back.
	resumed := resumeClaudeRecords(t, stateAfterFirst, secondBatch...)

	if resumed.CompactionCount != 1 || len(resumed.Compactions) != 1 {
		t.Errorf("after resume: compactions = %d/%d, want 1/1 -- the boundary must not be re-counted",
			resumed.CompactionCount, len(resumed.Compactions))
	}
	if resumed.Compactions[0].PreTokens != 85847 || resumed.Compactions[0].PostTokens != 10956 {
		t.Errorf("the resumed boundary lost its figures: %+v", resumed.Compactions[0])
	}
	if resumed.HarnessTotals == nil {
		t.Fatal("the rollup arrived in batch two and must now be observed")
	}
	// The lifetimes accumulate across batches rather than restarting.
	if resumed.CacheCreation1h != 27_922 || resumed.CacheCreation5m != 500 {
		t.Errorf("lifetimes after resume = %d/%d, want 27922/500",
			resumed.CacheCreation1h, resumed.CacheCreation5m)
	}
	assertObservationsCarryNoContent(t, resumed)
}

// TestReprocessingTheWholeSessionIsIdempotent is the property a historical
// recovery depends on: parsing the same transcript again, from offset zero, on top
// of state that already holds its observations, changes nothing.
func TestReprocessingTheWholeSessionIsIdempotent(t *testing.T) {
	lines := compactingSessionFixture()
	_, once := parseClaudeRecords(t, lines...)
	twice := resumeClaudeRecords(t, once, lines...)

	if twice.CompactionCount != once.CompactionCount {
		t.Errorf("compaction count moved from %d to %d on a re-read", once.CompactionCount, twice.CompactionCount)
	}
	if len(twice.Compactions) != len(once.Compactions) {
		t.Errorf("boundary list grew from %d to %d on a re-read", len(once.Compactions), len(twice.Compactions))
	}
	// The rollup is CUMULATIVE for the whole session, so the last one wins and
	// two are never summed. Re-reading must therefore leave it identical, not
	// doubled.
	if twice.HarnessTotals == nil || once.HarnessTotals == nil {
		t.Fatal("both passes must observe the rollup")
	}
	before, _ := json.Marshal(once.HarnessTotals)
	after, _ := json.Marshal(twice.HarnessTotals)
	if string(before) != string(after) {
		t.Errorf("the rollup changed on a re-read:\n before %s\n after  %s", before, after)
	}
}

// resumeClaudeRecords parses more records on top of an existing durable state,
// the way the tailer does after a restart.
func resumeClaudeRecords(t *testing.T, prior *claudeParserStateV1, lines ...string) *claudeParserStateV1 {
	t.Helper()
	envelope := parserStateEnvelope{Version: 1, SourceKind: domain.UsageSourceClaudeMain, Claude: prior}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal prior state: %v", err)
	}
	source := usageSource(domain.UsageSourceClaudeMain)
	source.Source.ParserStateJSON = string(encoded)
	records := make([]jsonlRecord, 0, len(lines))
	for i, line := range lines {
		records = append(records, jsonlRecord{Offset: int64(10_000 + i*100), Data: []byte(line)})
	}
	result := parseRecords(source, records, int64(10_000+len(lines)*100), time.Unix(1700000100, 0).UTC())
	return parserStateFromResult(t, result, domain.UsageSourceClaudeMain).Claude
}

// assertObservationsCarryNoContent walks the serialized observation state and
// fails on anything the fixture deliberately planted in the records.
func assertObservationsCarryNoContent(t *testing.T, state *claudeParserStateV1) {
	t.Helper()
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	for _, forbidden := range []string{
		"SECRET REASONING", "SECRET ANSWER", "/etc/shadow", "password",
		"secret-project", "Conversation compacted", "preservedSegment",
		"preservedMessages", "add-batch-loading", "ao/wf-1", "headUuid",
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("observation state contains %q:\n%s", forbidden, encoded)
		}
	}
}
