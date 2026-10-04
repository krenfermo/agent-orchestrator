package usage

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// assistantRecord builds one transcript record for a billed assistant message
// whose content blocks are exactly those given. Every record of a message
// carries the same usage, which is what a real transcript does and what makes
// the ingest path's identity the message id rather than a record offset.
func assistantRecord(offset int64, msgID string, blocks string) jsonlRecord {
	line := `{"type":"assistant","isSidechain":false,"uuid":"u-` + msgID +
		`","timestamp":"2026-07-01T10:00:00Z","message":{"id":"` + msgID +
		`","model":"claude-x","stop_reason":"tool_use","usage":{"input_tokens":10,` +
		`"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":4},` +
		`"content":` + blocks + `}}`
	return jsonlRecord{Offset: offset, Data: []byte(line)}
}

func classesOf(t *testing.T, result parseResult) []domain.TurnClass {
	t.Helper()
	out := make([]domain.TurnClass, 0, len(result.Events))
	for _, e := range result.Events {
		out = append(out, e.TurnClass)
	}
	return out
}

func TestClassifyTurnByToolName(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	cases := []struct {
		name   string
		blocks string
		want   domain.TurnClass
	}{
		{"bash is a command", `[{"type":"tool_use","name":"Bash","id":"t1"}]`, domain.TurnCommand},
		{"edit mutates", `[{"type":"tool_use","name":"Edit","id":"t1"}]`, domain.TurnEdit},
		{"grep reads", `[{"type":"tool_use","name":"Grep","id":"t1"}]`, domain.TurnRead},
		{"bashoutput is a wait", `[{"type":"tool_use","name":"BashOutput","id":"t1"}]`, domain.TurnWait},
		{"task delegates", `[{"type":"tool_use","name":"Task","id":"t1"}]`, domain.TurnSubagent},
		{"todowrite is plan state", `[{"type":"tool_use","name":"TodoWrite","id":"t1"}]`, domain.TurnPlan},
		{"text only is a message", `[{"type":"text","text":"here is what I found"}]`, domain.TurnMessage},
		{"thinking alone is still a message", `[{"type":"thinking","thinking":"..."}]`, domain.TurnMessage},
		{"two kinds in one call is mixed",
			`[{"type":"tool_use","name":"Read","id":"t1"},{"type":"tool_use","name":"Edit","id":"t2"}]`,
			domain.TurnMixed},
		{"two of the same kind is not mixed",
			`[{"type":"tool_use","name":"Read","id":"t1"},{"type":"tool_use","name":"Grep","id":"t2"}]`,
			domain.TurnRead},
		{"an unrecognised tool votes for nothing",
			`[{"type":"tool_use","name":"SomeFutureTool","id":"t1"}]`, domain.TurnMessage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := usageSource(domain.UsageSourceClaudeMain)
			result := parseRecords(source, []jsonlRecord{assistantRecord(0, "m1", tc.blocks)}, 400, now)
			got := classesOf(t, result)
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("class = %v, want %v", got, tc.want)
			}
		})
	}
}

// A billed message routinely arrives as several records: the thinking block
// first, the tool call after. Classifying from whichever record produced the
// event would have called 109 of the worked example's 193 calls "message".
func TestClassifyTurnAccumulatesAcrossRecordsOfOneMessage(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	source := usageSource(domain.UsageSourceClaudeMain)
	records := []jsonlRecord{
		assistantRecord(0, "m1", `[{"type":"thinking","thinking":"..."}]`),
		assistantRecord(200, "m1", `[{"type":"tool_use","name":"Bash","id":"t1"}]`),
	}
	result := parseRecords(source, records, 400, now)
	got := classesOf(t, result)
	if len(got) != 2 {
		t.Fatalf("events = %d, want one per record (the store dedupes on the message id)", len(got))
	}
	if got[0] != domain.TurnMessage {
		t.Fatalf("first emission = %v, want the honest class of what had been seen", got[0])
	}
	if got[1] != domain.TurnCommand {
		t.Fatalf("refined emission = %v, want command", got[1])
	}
	if result.Events[0].SourceEventKey != result.Events[1].SourceEventKey {
		t.Fatal("both emissions must carry the same key, or the refinement lands on a different row")
	}
}

// The accumulation lives in durable parser state precisely so the tailer's
// batch boundary -- which falls wherever the bytes happened to arrive -- cannot
// split a message and classify it from half of itself.
func TestClassifyTurnSurvivesABatchBoundary(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	source := usageSource(domain.UsageSourceClaudeMain)

	state, err := decodeParserState(source.Source)
	if err != nil {
		t.Fatalf("decode parser state: %v", err)
	}
	first := parseRecordsWithState(source, []jsonlRecord{
		assistantRecord(0, "m1", `[{"type":"thinking","thinking":"..."}]`),
	}, 200, now, state)
	if first.err != nil {
		t.Fatalf("first batch: %v", first.err)
	}

	// The next batch starts from the encoded state, exactly as the ingestor
	// resumes a source from its stored parser_state_json.
	resumed := source
	resumed.Source.ParserStateJSON = first.Cursor.ParserStateJSON
	nextState, err := decodeParserState(resumed.Source)
	if err != nil {
		t.Fatalf("decode resumed parser state: %v", err)
	}
	second := parseRecordsWithState(resumed, []jsonlRecord{
		assistantRecord(200, "m1", `[{"type":"tool_use","name":"Edit","id":"t1"}]`),
	}, 400, now, nextState)
	if second.err != nil {
		t.Fatalf("second batch: %v", second.err)
	}
	if len(second.Events) != 1 || second.Events[0].TurnClass != domain.TurnEdit {
		t.Fatalf("resumed class = %v, want edit", classesOf(t, second))
	}
}

// A new message id closes the previous accumulation. Without this a whole
// transcript would collapse into one ever-broadening "mixed".
func TestClassifyTurnResetsBetweenMessages(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	source := usageSource(domain.UsageSourceClaudeMain)
	records := []jsonlRecord{
		assistantRecord(0, "m1", `[{"type":"tool_use","name":"Edit","id":"t1"}]`),
		assistantRecord(200, "m2", `[{"type":"tool_use","name":"Bash","id":"t2"}]`),
	}
	got := classesOf(t, parseRecords(source, records, 400, now))
	if len(got) != 2 || got[0] != domain.TurnEdit || got[1] != domain.TurnCommand {
		t.Fatalf("classes = %v, want [edit command]", got)
	}
}

// The classifier must be structurally incapable of carrying user content: the
// decoded block type has no field for a command, an argument or a body, so a
// transcript full of them yields parser state with none.
func TestParserStateNeverCarriesCommandText(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	source := usageSource(domain.UsageSourceClaudeMain)
	secret := "rm -rf /very/secret/path --token=hunter2"
	blocks := `[{"type":"thinking","thinking":"` + secret + `"},` +
		`{"type":"tool_use","name":"Bash","id":"t1","input":{"command":"` + secret + `"}},` +
		`{"type":"text","text":"` + secret + `"}]`
	result := parseRecords(source, []jsonlRecord{assistantRecord(0, "m1", blocks)}, 400, now)
	if result.Cursor.ParserStateJSON == "" {
		t.Fatal("expected parser state to be encoded")
	}
	if containsSubstring(result.Cursor.ParserStateJSON, "hunter2") ||
		containsSubstring(result.Cursor.ParserStateJSON, "rm -rf") {
		t.Fatalf("parser state leaked command text: %s", result.Cursor.ParserStateJSON)
	}
	// And what it does carry is the closed vocabulary and nothing else.
	var envelope map[string]any
	if err := json.Unmarshal([]byte(result.Cursor.ParserStateJSON), &envelope); err != nil {
		t.Fatalf("parser state is not an object: %v", err)
	}
	if len(result.Events) != 1 || result.Events[0].TurnClass != domain.TurnCommand {
		t.Fatalf("class = %v, want command", classesOf(t, result))
	}
}

func containsSubstring(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// A Codex rollout exposes no per-call content blocks, so its events are
// UNCLASSIFIED rather than guessed into a class.
func TestCodexEventsStayUnclassified(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	source := usageSource(domain.UsageSourceCodexRollout)
	records := []jsonlRecord{
		{Offset: 0, Data: []byte(`{"type":"turn_context","payload":{"model":"gpt-5.6"}}`)},
		{Offset: 100, Data: codexTokenLine("2026-07-01T10:00:00Z", 100, 60, 10, 20, 5)},
	}
	result := parseRecords(source, records, 300, now)
	if len(result.Events) == 0 {
		t.Fatal("expected codex usage events")
	}
	for _, e := range result.Events {
		if e.TurnClass != domain.TurnUnclassified {
			t.Fatalf("codex class = %q, want unclassified", e.TurnClass)
		}
	}
}
