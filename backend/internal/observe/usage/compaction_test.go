package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/usage/pricing"
)

// The two records below are the shapes taken from the real canary transcript
// (session ao-canary-fixture-6), with every content-bearing field kept in the
// fixture ON PURPOSE -- `content`, `preservedSegment`, `preservedMessages`,
// `slug`, `cwd`, `gitBranch` -- so the leak test has something real to fail on.

const compactBoundaryRecord = `{"parentUuid":null,"logicalParentUuid":"prev","isSidechain":false,` +
	`"type":"system","subtype":"compact_boundary","content":"Conversation compacted",` +
	`"level":"info","uuid":"boundary-1","timestamp":"2026-09-11T21:04:02.783Z",` +
	`"cwd":"/Users/someone/secret-project","gitBranch":"ao/wf-1/wf-1","slug":"add-batch-loading",` +
	`"compactMetadata":{"trigger":"manual","preTokens":85847,"postTokens":10956,` +
	`"cumulativeDroppedTokens":74891,"durationMs":95073,` +
	`"preservedSegment":{"headUuid":"a","anchorUuid":"b","tailUuid":"c"},` +
	`"preservedMessages":{"anchorUuid":"b","uuids":["a","b","c"]}}}`

const costStateRecord = `{"type":"cost-state","sessionId":"0faee1b7","totalCostUSD":3.4077755,` +
	`"totalAPIDuration":532012,"totalToolDuration":11021,"totalLinesAdded":0,` +
	`"modelUsage":{"claude-haiku-4-5-20251001":{"inputTokens":4758,"outputTokens":33,` +
	`"thinkingTokens":0,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"costUSD":0.004923},` +
	`"claude-opus-5[1m]":{"inputTokens":6116,"outputTokens":47453,"thinkingTokens":6326,` +
	`"cacheReadInputTokens":2083095,"cacheCreationInputTokens":115415,"costUSD":3.4028525}},` +
	`"hasUnknownModelCost":false}`

func claudeAssistantRecord(id, model string, input, output int64) string {
	return fmt.Sprintf(
		`{"type":"assistant","isSidechain":false,"uuid":"%s","timestamp":"2026-09-11T20:54:29.736Z",`+
			`"message":{"id":"%s","model":"%s","stop_reason":"tool_use","usage":`+
			`{"input_tokens":0,"cache_creation_input_tokens":%d,"cache_read_input_tokens":0,"output_tokens":%d}}}`,
		id, id, model, input, output)
}

func parseClaudeRecords(t *testing.T, lines ...string) (parseResult, *claudeParserStateV1) {
	t.Helper()
	source := usageSource(domain.UsageSourceClaudeMain)
	records := make([]jsonlRecord, 0, len(lines))
	for i, line := range lines {
		records = append(records, jsonlRecord{Offset: int64(i * 100), Data: []byte(line)})
	}
	result := parseRecords(source, records, int64(len(lines)*100), time.Unix(1700000000, 0).UTC())
	state := parserStateFromResult(t, result, domain.UsageSourceClaudeMain)
	return result, state.Claude
}

func TestCompactBoundaryIsObservedAndBillsNothing(t *testing.T) {
	result, state := parseClaudeRecords(t,
		claudeAssistantRecord("msg-1", "claude-opus-5", 45942, 191),
		compactBoundaryRecord,
	)
	// The observation must not become a usage event: AO did not see the
	// provider bill this turn and must not pretend it did.
	if len(result.Events) != 1 {
		t.Fatalf("events = %d, want only the assistant call", len(result.Events))
	}
	if state.CompactionCount != 1 || len(state.Compactions) != 1 {
		t.Fatalf("count/list = %d/%d, want 1/1", state.CompactionCount, len(state.Compactions))
	}
	got := state.Compactions[0]
	if got.UUID != "boundary-1" || got.Trigger != "manual" {
		t.Fatalf("observation = %+v", got)
	}
	if got.PreTokens != 85847 || got.PostTokens != 10956 ||
		got.CumulativeDroppedTokens != 74891 || got.DurationMs != 95073 {
		t.Fatalf("figures = %+v", got)
	}
	// The model is carried from the calls the session had already made, which
	// is the only place the boundary record itself does not name it.
	if got.ModelID != "claude-opus-5" {
		t.Fatalf("model = %q, want the session's own", got.ModelID)
	}
}

func TestCompactBoundaryWithoutMetadataIsStillCounted(t *testing.T) {
	_, state := parseClaudeRecords(t,
		`{"type":"system","subtype":"compact_boundary","uuid":"bare","timestamp":"2026-09-11T21:04:02.783Z"}`,
	)
	if state.CompactionCount != 1 {
		t.Fatalf("count = %d, want the compaction counted", state.CompactionCount)
	}
	got := state.Compactions[0]
	if got.Trigger != string(domain.CompactionTriggerUnknown) {
		t.Fatalf("trigger = %q, want unknown", got.Trigger)
	}
	if got.PreTokens != 0 || got.PostTokens != 0 {
		t.Fatalf("absent figures must stay zero, got %+v", got)
	}
}

func TestCompactBoundariesAreIdempotentOnReRead(t *testing.T) {
	// A source re-read from offset zero -- an artifact replaced, a recovery --
	// sees the same boundary twice and must count it once.
	_, state := parseClaudeRecords(t, compactBoundaryRecord, compactBoundaryRecord)
	if state.CompactionCount != 1 || len(state.Compactions) != 1 {
		t.Fatalf("count/list = %d/%d, want 1/1", state.CompactionCount, len(state.Compactions))
	}
}

func TestMultipleCompactionsAreKeptInOrder(t *testing.T) {
	second := strings.Replace(compactBoundaryRecord, `"uuid":"boundary-1"`, `"uuid":"boundary-2"`, 1)
	second = strings.Replace(second, `"preTokens":85847`, `"preTokens":67663`, 1)
	_, state := parseClaudeRecords(t, compactBoundaryRecord, second)
	if state.CompactionCount != 2 || len(state.Compactions) != 2 {
		t.Fatalf("count/list = %d/%d, want 2/2", state.CompactionCount, len(state.Compactions))
	}
	if state.Compactions[0].PreTokens != 85847 || state.Compactions[1].PreTokens != 67663 {
		t.Fatalf("order lost: %+v", state.Compactions)
	}
}

func TestATranscriptWithNoCompactionObservesNothing(t *testing.T) {
	result, state := parseClaudeRecords(t,
		claudeAssistantRecord("msg-1", "claude-opus-5", 100, 10),
		claudeAssistantRecord("msg-2", "claude-opus-5", 120, 12),
	)
	if len(result.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(result.Events))
	}
	if state.CompactionCount != 0 || len(state.Compactions) != 0 || state.HarnessTotals != nil {
		t.Fatalf("a transcript that never compacted must observe nothing, got %+v", state)
	}
}

func TestTheBoundaryListIsBoundedButTheCountIsNot(t *testing.T) {
	lines := make([]string, 0, maxCompactionObservations+5)
	for i := 0; i < maxCompactionObservations+5; i++ {
		lines = append(lines, strings.Replace(compactBoundaryRecord,
			`"uuid":"boundary-1"`, fmt.Sprintf(`"uuid":"boundary-%d"`, i), 1))
	}
	_, state := parseClaudeRecords(t, lines...)
	if len(state.Compactions) != maxCompactionObservations {
		t.Fatalf("list = %d, want it capped at %d", len(state.Compactions), maxCompactionObservations)
	}
	if state.CompactionCount != maxCompactionObservations+5 {
		t.Fatalf("count = %d, want every boundary counted", state.CompactionCount)
	}
	if state.CompactionsDropped != 5 {
		t.Fatalf("dropped = %d, want 5", state.CompactionsDropped)
	}
}

func TestHarnessRollupIsReadAndTheLastOneWins(t *testing.T) {
	smaller := strings.Replace(costStateRecord, `"totalCostUSD":3.4077755`, `"totalCostUSD":1.0`, 1)
	_, state := parseClaudeRecords(t, smaller, costStateRecord)
	if state.HarnessTotals == nil {
		t.Fatal("rollup not read")
	}
	if state.HarnessTotals.TotalCostUSD != 3.4077755 {
		t.Fatalf("cost = %v, want the last rollup (cumulative, never summed)", state.HarnessTotals.TotalCostUSD)
	}
	if len(state.HarnessTotals.Models) != 2 {
		t.Fatalf("models = %d, want 2", len(state.HarnessTotals.Models))
	}
	// Sorted by id, so haiku comes first and the assertion cannot depend on Go
	// map iteration order.
	opus := state.HarnessTotals.Models[1]
	if opus.ModelID != "claude-opus-5[1m]" {
		t.Fatalf("model id = %q, want the harness's own spelling kept verbatim", opus.ModelID)
	}
	if opus.CacheReadTokens != 2083095 || opus.OutputTokens != 47453 || opus.ThinkingTokens != 6326 {
		t.Fatalf("opus totals = %+v", opus)
	}
}

func TestAnEmptyRollupIsNotAnObservedZero(t *testing.T) {
	_, state := parseClaudeRecords(t, `{"type":"cost-state","totalCostUSD":0,"modelUsage":{}}`)
	if state.HarnessTotals != nil {
		t.Fatal("a rollup naming no model must not be stored as an observed one")
	}
}

func TestNegativeHarnessFiguresAreFlooredNotCarried(t *testing.T) {
	_, state := parseClaudeRecords(t,
		strings.Replace(compactBoundaryRecord, `"preTokens":85847`, `"preTokens":-5`, 1),
		`{"type":"cost-state","totalCostUSD":1,"modelUsage":{"m":{"inputTokens":-9,"outputTokens":4}}}`,
	)
	if state.Compactions[0].PreTokens != 0 {
		t.Fatalf("negative preTokens = %d, want floored", state.Compactions[0].PreTokens)
	}
	if state.HarnessTotals.Models[0].InputTokens != 0 {
		t.Fatalf("negative input = %d, want floored", state.HarnessTotals.Models[0].InputTokens)
	}
}

// TestCompactionObservationsCarryNoContent is the leak test. Both fixture
// records are full of text a compaction touches -- the boundary's `content`,
// its preserved-message uuids, a slug, a cwd, a git branch, and a model rollup
// whose keys are the only free text that reaches state at all. None of it may
// appear in the durable parser state, which is what actually gets written to
// the database.
func TestCompactionObservationsCarryNoContent(t *testing.T) {
	source := usageSource(domain.UsageSourceClaudeMain)
	result := parseRecords(source, []jsonlRecord{
		{Offset: 0, Data: []byte(compactBoundaryRecord)},
		{Offset: 900, Data: []byte(costStateRecord)},
	}, 1800, time.Unix(1700000000, 0).UTC())
	if result.err != nil {
		t.Fatalf("parse: %v", result.err)
	}
	persisted := result.Cursor.ParserStateJSON
	for _, forbidden := range []string{
		"Conversation compacted", "secret-project", "add-batch-loading",
		"preservedSegment", "preservedMessages", "headUuid", "anchorUuid",
		"ao/wf-1/wf-1", "totalAPIDuration", "totalLinesAdded",
	} {
		if strings.Contains(persisted, forbidden) {
			t.Fatalf("parser state leaked %q:\n%s", forbidden, persisted)
		}
	}
	// And what it DOES carry is numbers.
	var envelope map[string]any
	if err := json.Unmarshal([]byte(persisted), &envelope); err != nil {
		t.Fatalf("state is not an object: %v", err)
	}
}

func TestObservationsSurviveTheRoundTripIntoDomainTypes(t *testing.T) {
	source := usageSource(domain.UsageSourceClaudeMain)
	result := parseRecords(source, []jsonlRecord{
		{Offset: 0, Data: []byte(claudeAssistantRecord("msg-1", "claude-opus-5", 100, 10))},
		{Offset: 400, Data: []byte(compactBoundaryRecord)},
		{Offset: 900, Data: []byte(costStateRecord)},
	}, 1800, time.Unix(1700000000, 0).UTC())
	if result.err != nil {
		t.Fatalf("parse: %v", result.err)
	}
	record := domain.UsageSourceRecord{
		ID: 7, Kind: domain.UsageSourceClaudeMain,
		ByteOffset:      result.Cursor.ByteOffset,
		ParserStateJSON: result.Cursor.ParserStateJSON,
		State:           domain.UsageSourceActive,
	}
	got := ExtractCompactionObservations(record, "ao-canary-fixture-6", domain.HarnessClaudeCode)
	if got.Empty() {
		t.Fatal("observations were written and must read back")
	}
	if len(got.Boundaries) != 1 || got.Count != 1 {
		t.Fatalf("boundaries = %d, count = %d", len(got.Boundaries), got.Count)
	}
	boundary := got.Boundaries[0]
	if boundary.SessionID != "ao-canary-fixture-6" || boundary.Harness != domain.HarnessClaudeCode {
		t.Fatalf("identity not filled from the binding: %+v", boundary)
	}
	if boundary.Trigger != domain.CompactionTriggerManual || boundary.ReductionTokens() != 74891 {
		t.Fatalf("boundary = %+v", boundary)
	}
	if boundary.ObservedAt == nil || boundary.ObservedAt.UTC().Format(time.RFC3339) != "2026-09-11T21:04:02Z" {
		t.Fatalf("observedAt = %v", boundary.ObservedAt)
	}
	if !got.Harness.Observed || len(got.Harness.Models) != 2 {
		t.Fatalf("rollup = %+v", got.Harness)
	}
	// InputTokens is defined the same way model_usage_events defines it:
	// uncached plus reads plus writes.
	opus := got.Harness.Models[1]
	if opus.Tokens.InputTokens != 6116+2083095+115415 {
		t.Fatalf("input = %d, want the three dimensions folded", opus.Tokens.InputTokens)
	}
}

func TestStateFromABuildWithoutObservationsReadsBackEmpty(t *testing.T) {
	// A source written before P7.1: valid state, no compaction fields at all.
	legacy := `{"version":1,"source_kind":"claude_main","claude":{"model_id":"claude-opus-5"}}`
	got := ExtractCompactionObservations(domain.UsageSourceRecord{
		ID: 1, Kind: domain.UsageSourceClaudeMain, ParserStateJSON: legacy, State: domain.UsageSourceActive,
	}, "s", domain.HarnessClaudeCode)
	if !got.Empty() {
		t.Fatalf("a pre-P7.1 state must read back empty, got %+v", got)
	}
}

func TestUnreadableStateDoesNotFailTheReport(t *testing.T) {
	for _, state := range []string{"", "not json", `{"version":99}`, `{"version":1,"source_kind":"codex_rollout"}`} {
		got := ExtractCompactionObservations(domain.UsageSourceRecord{
			ID: 1, Kind: domain.UsageSourceClaudeMain, ParserStateJSON: state, State: domain.UsageSourceActive,
		}, "s", domain.HarnessClaudeCode)
		if !got.Empty() {
			t.Fatalf("state %q must degrade to empty, got %+v", state, got)
		}
	}
}

// --- the read model ---------------------------------------------------------

type fakeCompactionStore struct {
	bindings   []domain.UsageBindingRecord
	sources    map[int64][]domain.UsageSourceRecord
	aggregates []domain.UsageModelAggregate
}

func (f fakeCompactionStore) ListUsageBindingsForSession(context.Context, domain.SessionID) ([]domain.UsageBindingRecord, error) {
	return f.bindings, nil
}

func (f fakeCompactionStore) ListUsageSourcesForBinding(_ context.Context, id int64) ([]domain.UsageSourceRecord, error) {
	return f.sources[id], nil
}

func (f fakeCompactionStore) ListUsageModelAggregates(context.Context, domain.SessionID) ([]domain.UsageModelAggregate, error) {
	return f.aggregates, nil
}

func TestSessionCompactionAccountingFoldsTheStoreRowsThatAlreadyExist(t *testing.T) {
	source := usageSource(domain.UsageSourceClaudeMain)
	result := parseRecords(source, []jsonlRecord{
		{Offset: 0, Data: []byte(compactBoundaryRecord)},
		{Offset: 900, Data: []byte(costStateRecord)},
	}, 1800, time.Unix(1700000000, 0).UTC())
	if result.err != nil {
		t.Fatalf("parse: %v", result.err)
	}
	store := fakeCompactionStore{
		bindings: []domain.UsageBindingRecord{{ID: 42, Harness: domain.HarnessClaudeCode}},
		sources: map[int64][]domain.UsageSourceRecord{42: {{
			ID: 7, BindingID: 42, Kind: domain.UsageSourceClaudeMain,
			ByteOffset:      result.Cursor.ByteOffset,
			ParserStateJSON: result.Cursor.ParserStateJSON,
			State:           domain.UsageSourceActive,
		}}},
		aggregates: []domain.UsageModelAggregate{{
			Harness: domain.HarnessClaudeCode, ModelID: "claude-opus-5",
			Tokens: domain.UsageTokenMetrics{
				InputTokens: 1_829_810, UncachedInputTokens: 56,
				CacheReadTokens: 1_719_470, CacheWriteTokens: 110_284, OutputTokens: 30_500,
			},
		}},
	}

	reader := NewCompactionReader(store, pricing.Embedded())
	got, err := reader.SessionCompactionAccounting(context.Background(), "ao-canary-fixture-6")
	if err != nil {
		t.Fatalf("accounting: %v", err)
	}
	if got.Compactions != 1 {
		t.Fatalf("compactions = %d, want 1", got.Compactions)
	}
	if got.Unattributed.OutputTokens != 16_953+33 {
		t.Fatalf("unattributed output = %d", got.Unattributed.OutputTokens)
	}
	if !got.AttributedCost.Known {
		t.Fatal("claude-opus-5 is in the embedded catalog and must price")
	}
	// The production catalog has no row for the long-context variant, so the
	// opus residual -- which is all of the compaction spend -- is reported in
	// tokens with no cost. The haiku line beside it DOES price, so the residual
	// cost is a PARTIAL figure that names what it is missing rather than a
	// complete-looking small one. That distinction is the whole reason
	// UsageCost carries UnpricedModels.
	var opus domain.UnattributedModelLine
	for _, line := range got.UnattributedByModel {
		if line.ModelID == "claude-opus-5[1m]" {
			opus = line
		}
	}
	if opus.ModelID == "" {
		t.Fatal("the opus residual must be reported")
	}
	if opus.Cost.Known {
		t.Fatal("the embedded catalog must not price claude-opus-5[1m]")
	}
	if opus.Tokens.OutputTokens != 16_953 {
		t.Fatalf("opus residual output = %d, want 16953", opus.Tokens.OutputTokens)
	}
	var named bool
	for _, model := range got.UnattributedCost.UnpricedModels {
		if model == "claude-opus-5[1m]" {
			named = true
		}
	}
	if !named {
		t.Fatalf("a partial residual cost must name what it could not price, got %v",
			got.UnattributedCost.UnpricedModels)
	}
}

func TestAccountingWithoutAPricerStillReportsEveryToken(t *testing.T) {
	store := fakeCompactionStore{
		bindings: []domain.UsageBindingRecord{{ID: 1, Harness: domain.HarnessClaudeCode}},
		aggregates: []domain.UsageModelAggregate{{
			ModelID: "claude-opus-5",
			Tokens:  domain.UsageTokenMetrics{InputTokens: 100, OutputTokens: 10},
		}},
	}
	got, err := NewCompactionReader(store, nil).SessionCompactionAccounting(context.Background(), "s")
	if err != nil {
		t.Fatalf("accounting: %v", err)
	}
	if got.Attributed.InputTokens != 100 || got.Attributed.OutputTokens != 10 {
		t.Fatalf("tokens = %+v", got.Attributed)
	}
	if got.AttributedCost.Known {
		t.Fatal("no rate card means no cost, never a zero one")
	}
}
