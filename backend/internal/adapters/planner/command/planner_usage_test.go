package command

import (
	"testing"
)

// planner_usage_test.go — the planner spends real provider tokens, and this is
// the only place they are ever stated.
//
// AO invokes `claude --print --output-format json ... --no-session-persistence`.
// The last flag is why the transcript-based pipeline can never see this call:
// there is no transcript. The envelope below is the whole record.

func TestPlannerEnvelope_ReadsUsageFromTheResponse(t *testing.T) {
	body := []byte(`{"result":"{}","usage":{"input_tokens":800,"output_tokens":120,` +
		`"cache_creation_input_tokens":300,"cache_read_input_tokens":4000}}`)
	env, err := extractEnvelope(body)
	if err != nil {
		t.Fatalf("extractEnvelope: %v", err)
	}
	usage, model, ok := plannerUsageFromEnvelope(env, "sonnet")
	if !ok {
		t.Fatal("the envelope reported usage and it must be read")
	}
	if model != "sonnet" {
		t.Fatalf("model = %q, want the requested model when the envelope names none", model)
	}
	// InputTokens is the TOTAL input with the cached halves inside it — the same
	// convention the transcript parser uses, so a planner call and a worker turn
	// are summable without double counting the cache.
	if usage.InputTokens != 5100 {
		t.Fatalf("input = %d, want 5100 (800 + 300 + 4000)", usage.InputTokens)
	}
	if usage.UncachedInputTokens != 800 || usage.CacheWriteTokens != 300 || usage.CacheReadTokens != 4000 {
		t.Fatalf("dimensions = %+v", usage)
	}
	if usage.OutputTokens != 120 {
		t.Fatalf("output = %d, want 120", usage.OutputTokens)
	}
}

func TestPlannerEnvelope_PrefersThePerModelBlock(t *testing.T) {
	// The aggregate block does not say WHICH model was billed; the per-model
	// block does. When the CLI sends both, the one that names the model wins.
	body := []byte(`{"result":"{}","usage":{"input_tokens":1,"output_tokens":1},` +
		`"modelUsage":{"claude-sonnet-5":{"input_tokens":900,"output_tokens":90}}}`)
	env, err := extractEnvelope(body)
	if err != nil {
		t.Fatalf("extractEnvelope: %v", err)
	}
	usage, model, ok := plannerUsageFromEnvelope(env, "sonnet")
	if !ok {
		t.Fatal("usage must be read")
	}
	if model != "claude-sonnet-5" {
		t.Fatalf("model = %q, want the model the provider actually billed", model)
	}
	if usage.InputTokens != 900 || usage.OutputTokens != 90 {
		t.Fatalf("usage = %+v, want the per-model figures", usage)
	}
}

func TestPlannerEnvelope_NoUsageIsUnknownNotZero(t *testing.T) {
	// A CLI that reports nothing must leave the planner's spend UNKNOWN. A
	// zeroed vector would be stored and rendered as "this planner call was
	// free", which is never true — it always spends something.
	for _, body := range []string{
		`{"result":"{}"}`,
		`{"result":"{}","usage":{"input_tokens":0,"output_tokens":0}}`,
		`{"result":"{}","usage":{"input_tokens":-5,"output_tokens":10}}`,
	} {
		env, err := extractEnvelope([]byte(body))
		if err != nil {
			t.Fatalf("extractEnvelope(%s): %v", body, err)
		}
		if _, _, ok := plannerUsageFromEnvelope(env, "sonnet"); ok {
			t.Fatalf("%s reported no usable usage and must yield unknown", body)
		}
	}
}

func TestPlannerEnvelope_UsageDoesNotBreakPlanParsing(t *testing.T) {
	// The usage fields are additive: an envelope carrying them must still parse
	// its plan exactly as before, and one without them must still work.
	body := []byte(`{"structured_output":{"version":"v1"},"usage":{"input_tokens":10,"output_tokens":2}}`)
	env, err := extractEnvelope(body)
	if err != nil {
		t.Fatalf("extractEnvelope: %v", err)
	}
	if len(env.StructuredOutput) == 0 {
		t.Fatal("the structured plan must survive alongside the usage block")
	}
	legacy, err := extractEnvelope([]byte(`{"structured_output":{"version":"v1"}}`))
	if err != nil {
		t.Fatalf("legacy envelope: %v", err)
	}
	if legacy.Usage != nil {
		t.Fatal("an envelope with no usage block must report none rather than a zero one")
	}
}
