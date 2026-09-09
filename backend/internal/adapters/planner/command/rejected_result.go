package command

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
)

// rejected_result.go — what AO keeps about a planner result it refused.
//
// THE GAP. When F2 refuses a result (see consistency.go) the evidence records
// how BIG each half of the envelope was and which signal spoke, and then the
// bytes are gone. So "the provider was billed 18,238 output tokens and handed
// back 682 bytes" is durable, but "682 bytes of WHAT" is not: a schema-shaped
// placeholder, a plan truncated mid-write, an empty answer and a fragment of
// prose are four different provider failures and they all read as one number.
//
// WHAT IS NOT KEPT, and why. Not the bytes, not a prefix of them, not a
// redacted prefix. A rejected result is arbitrary model output derived from
// the user's objective, and no scrubber can promise that an arbitrary span of
// it is safe to store -- so the honest answer is to store none of it rather
// than a prefix that is usually fine. PlannerAttemptEvidence already carries
// that discipline explicitly ("sizes and durations only -- never the prompt,
// the objective text, the documents") and this does not break it.
//
// WHAT IS KEPT instead is enough to tell those four apart without any content:
//
//   - a SHA-256 of the exact bytes, so the same failure seen twice is provably
//     the same failure, and a replay that reproduces it can be matched to it;
//   - a KIND, from parsing the bytes rather than from guessing;
//   - a SHAPE: the plan's own field names -- AO's vocabulary, not the model's
//     -- with the LENGTH of each value instead of the value. `summary:len4`
//     beside `steps:[1x{title:len1}]` is a placeholder, and it says so without
//     quoting a single character the model wrote.
//
// A key the plan schema does not define is never printed: it is emitted as `?`,
// so even a hostile or hallucinated field name cannot ride out in the shape.

// rejectedResultKind classifies why a refused result is refused, from the bytes.
const (
	rejectedKindEmpty              = "empty"
	rejectedKindInvalidJSON        = "invalid_json"
	rejectedKindTruncatedJSON      = "truncated_json"
	rejectedKindPlaceholder        = "placeholder"
	rejectedKindUsageContentGap    = "usage_content_mismatch"
	rejectedKindEnvelopeDisagree   = "envelope_disagreement"
	rejectedKindProviderReportedNo = "provider_reported_failure"
)

const (
	// maxShapeBytes bounds the rendered shape. It rides on the attempt
	// evidence inside an existing checkpoint row, so it must stay small and
	// predictable rather than grow with the plan.
	maxShapeBytes = 512
	// maxShapeSteps bounds how many array elements are described individually.
	maxShapeSteps = 3
	// maxShapeDepth stops a pathological nesting from costing anything.
	maxShapeDepth = 4
	// placeholderScalarLen is the value length at or below which a plan's
	// load-bearing text fields read as filler rather than as content. The
	// incident's placeholder carried summary:"test" (4) and title:"t" (1).
	placeholderScalarLen = 8
)

// planSchemaKeys is the whitelist. Only a field AO's own plan schema defines
// can appear in a shape; everything else is "?".
var planSchemaKeys = map[string]struct{}{
	"version": {}, "objective": {}, "summary": {}, "steps": {},
	"id": {}, "title": {}, "description": {}, "dependencies": {},
	"acceptanceCriteria": {}, "verify": {}, "writeIntent": {},
	"scope": {}, "waivers": {}, "commands": {}, "command": {},
	"paths": {}, "reason": {}, "kind": {},
}

// rejectedResult is the content-free description of a refused planner result.
type rejectedResult struct {
	Hash  string
	Bytes int
	Kind  string
	Shape string
}

// describeRejectedResult builds the evidence for a refused result.
//
// signal is F2's verdict string, used only to distinguish the refusals that
// parse perfectly (a ratio mismatch, an envelope disagreement) from the ones
// the bytes themselves explain.
func describeRejectedResult(raw []byte, signal string) rejectedResult {
	sum := sha256.Sum256(raw)
	out := rejectedResult{
		Hash:  hex.EncodeToString(sum[:]),
		Bytes: len(raw),
	}

	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		out.Kind = rejectedKindEmpty
		return out
	}

	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		// A plan cut off mid-write and a plan that was never JSON are
		// different provider failures: the first ends early, the second is
		// malformed somewhere a decoder can point at.
		if errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(err.Error(), "unexpected end of JSON input") {
			out.Kind = rejectedKindTruncatedJSON
		} else {
			out.Kind = rejectedKindInvalidJSON
		}
		return out
	}

	out.Shape = renderShape(parsed, 0)
	switch {
	case strings.Contains(signal, "is_error=true"):
		out.Kind = rejectedKindProviderReportedNo
	case strings.Contains(signal, "disagree"):
		out.Kind = rejectedKindEnvelopeDisagree
	case looksLikePlaceholder(parsed):
		out.Kind = rejectedKindPlaceholder
	default:
		out.Kind = rejectedKindUsageContentGap
	}
	return out
}

// looksLikePlaceholder reports whether every load-bearing text field in the
// parsed plan is filler-length. It is a description of what was received, never
// a decision: F2 has already refused this result by the time it is asked.
func looksLikePlaceholder(v any) bool {
	obj, ok := v.(map[string]any)
	if !ok {
		return false
	}
	longest := 0
	for _, key := range []string{"summary", "objective"} {
		if s, ok := obj[key].(string); ok && len(s) > longest {
			longest = len(s)
		}
	}
	steps, _ := obj["steps"].([]any)
	for _, step := range steps {
		stepObj, ok := step.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"title", "description"} {
			if s, ok := stepObj[key].(string); ok && len(s) > longest {
				longest = len(s)
			}
		}
	}
	return longest > 0 && longest <= placeholderScalarLen
}

// renderShape describes a value's structure with lengths in place of content.
func renderShape(v any, depth int) string {
	if depth > maxShapeDepth {
		return "..."
	}
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			name := "?"
			if _, known := planSchemaKeys[k]; known {
				name = k
			}
			parts = append(parts, name+":"+renderShape(t[k], depth+1))
		}
		return "{" + truncateShape(strings.Join(parts, ",")) + "}"
	case []any:
		if len(t) == 0 {
			return "[0]"
		}
		shown := t
		if len(shown) > maxShapeSteps {
			shown = shown[:maxShapeSteps]
		}
		parts := make([]string, 0, len(shown))
		for _, item := range shown {
			parts = append(parts, renderShape(item, depth+1))
		}
		return "[" + strconv.Itoa(len(t)) + "x" + truncateShape(strings.Join(parts, ",")) + "]"
	case string:
		return "len" + strconv.Itoa(len(t))
	case float64:
		return "num"
	case bool:
		return "bool"
	case nil:
		return "null"
	default:
		return "?"
	}
}

func truncateShape(s string) string {
	if len(s) <= maxShapeBytes {
		return s
	}
	return s[:maxShapeBytes] + "..."
}

// logArgs renders the evidence for a log line, omitting an absent shape.
func (r rejectedResult) logArgs() []any {
	args := []any{"rejectedResultKind", r.Kind, "rejectedResultBytes", r.Bytes, "rejectedResultHash", r.Hash}
	if r.Shape != "" {
		args = append(args, "rejectedResultShape", r.Shape)
	}
	return args
}
