package command

import (
	"encoding/json"
	"strings"
	"testing"
)

// The five failures this evidence exists to tell apart, each described from
// its own bytes.
func TestDescribeRejectedResultClassifiesEachFailure(t *testing.T) {
	ratioSignal := "provider was billed 18238 output tokens but the plan AO received accounts for about 170 of them (682 bytes); the real result was lost"

	for _, tc := range []struct {
		name   string
		raw    string
		signal string
		want   string
	}{
		{"empty", "", ratioSignal, rejectedKindEmpty},
		{"whitespace only", "   \n\t", ratioSignal, rejectedKindEmpty},
		{
			"truncated mid-write",
			`{"version":"v1","summary":"a real plan that stops","steps":[{"id":"s1","title":"do`,
			ratioSignal, rejectedKindTruncatedJSON,
		},
		{"not json at all", `I cannot produce a plan for this.`, ratioSignal, rejectedKindInvalidJSON},
		{
			"schema-shaped placeholder",
			`{"version":"v1","objective":"o","summary":"test","steps":[{"id":"s1","title":"t","description":"d","acceptanceCriteria":["a"]}]}`,
			ratioSignal, rejectedKindPlaceholder,
		},
		{
			"real content refused on the ratio",
			`{"version":"v1","objective":"Document the operational runbook for the Plane backlog in full","summary":"A complete and genuinely long summary of the work to be carried out here","steps":[{"id":"s1","title":"Write the runbook document","description":"Create the file and describe every state transition in detail","acceptanceCriteria":["the file exists and describes the four states"]}]}`,
			ratioSignal, rejectedKindUsageContentGap,
		},
		{
			"provider said it failed",
			`{"summary":"x"}`,
			`provider reported the invocation itself failed (is_error=true, subtype="error_during_execution")`,
			rejectedKindProviderReportedNo,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := describeRejectedResult([]byte(tc.raw), tc.signal)
			if got.Kind != tc.want {
				t.Fatalf("kind = %q, want %q (shape %q)", got.Kind, tc.want, got.Shape)
			}
			if got.Bytes != len(tc.raw) {
				t.Fatalf("bytes = %d, want %d", got.Bytes, len(tc.raw))
			}
			if len(got.Hash) != 64 {
				t.Fatalf("hash = %q, want a 64-char sha256", got.Hash)
			}
		})
	}
}

// The guarantee the whole design rests on: nothing the model wrote survives.
func TestRejectedResultNeverCarriesContent(t *testing.T) {
	// Every one of these strings is content that must not reach the evidence.
	secrets := []string{
		"CORPORATE-OBJECTIVE-TEXT",
		"192.168.10.163",
		"sk-live-abcdef0123456789",
		"la contraseña del sistema",
	}
	raw := `{"version":"v1","objective":"CORPORATE-OBJECTIVE-TEXT","summary":"contact 192.168.10.163 with sk-live-abcdef0123456789","steps":[{"id":"s1","title":"la contraseña del sistema","description":"d","acceptanceCriteria":["a"],"unexpectedField":"CORPORATE-OBJECTIVE-TEXT"}]}`

	got := describeRejectedResult([]byte(raw), "some signal")
	rendered := strings.Join([]string{got.Kind, got.Shape, got.Hash}, " ")
	for _, s := range secrets {
		if strings.Contains(rendered, s) {
			t.Fatalf("the evidence carried content: %q appears in %q", s, rendered)
		}
	}
	// A field the plan schema does not define must not be named either: a
	// hallucinated or hostile key is content too.
	if strings.Contains(got.Shape, "unexpectedField") {
		t.Fatalf("an unknown key was named in the shape: %q", got.Shape)
	}
	if !strings.Contains(got.Shape, "?:") {
		t.Fatalf("the unknown key should appear as `?`, got %q", got.Shape)
	}
	// And the shape must still be useful: known field names with lengths.
	for _, want := range []string{"summary:len", "objective:len", "steps:[1x", "title:len"} {
		if !strings.Contains(got.Shape, want) {
			t.Fatalf("shape lost its diagnostic value, %q missing from %q", want, got.Shape)
		}
	}
}

// The MEDUSA incident. 18,238 output tokens billed, 682 bytes received. Before
// this, that was the whole record; the shape is what makes it answerable.
func TestPlaceholderIsDistinguishableFromARealPlanOfTheSameSize(t *testing.T) {
	placeholder := `{"version":"v1","objective":"o","summary":"test","steps":[{"id":"s1","title":"t","description":"d","acceptanceCriteria":["a"]}]}`
	real := `{"version":"v1","objective":"Documentar el runbook operativo de AO para el backlog de Plane con el contrato exacto","summary":"Crear el documento con los cuatro estados, las transiciones y los criterios de exclusion","steps":[{"id":"s1","title":"Escribir el runbook completo","description":"Crear docs/ao-plane-backlog-runbook.md con todas las secciones","acceptanceCriteria":["el archivo existe y describe los cuatro estados"]}]}`

	p := describeRejectedResult([]byte(placeholder), "ratio")
	r := describeRejectedResult([]byte(real), "ratio")
	if p.Kind != rejectedKindPlaceholder {
		t.Fatalf("placeholder classified as %q", p.Kind)
	}
	if r.Kind == rejectedKindPlaceholder {
		t.Fatal("a real plan was classified as a placeholder")
	}
	if p.Hash == r.Hash {
		t.Fatal("two different results hashed the same")
	}
}

// The same failure twice is provably the same failure.
func TestIdenticalResultsHashIdentically(t *testing.T) {
	raw := []byte(`{"summary":"test","steps":[]}`)
	if describeRejectedResult(raw, "s").Hash != describeRejectedResult(raw, "s").Hash {
		t.Fatal("the hash is not stable for identical bytes")
	}
}

// Bounded: it rides inside an existing checkpoint row and must not grow with
// the plan.
func TestShapeStaysBounded(t *testing.T) {
	steps := make([]map[string]any, 200)
	for i := range steps {
		steps[i] = map[string]any{
			"id": "step-with-a-long-identifier", "title": strings.Repeat("x", 400),
			"description": strings.Repeat("y", 4000),
		}
	}
	raw, err := json.Marshal(map[string]any{"version": "v1", "steps": steps})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	got := describeRejectedResult(raw, "ratio")
	if len(got.Shape) > maxShapeBytes*2 {
		t.Fatalf("shape grew to %d bytes; it must stay bounded", len(got.Shape))
	}
	// The count survives even though the elements are not all described.
	if !strings.Contains(got.Shape, "[200x") {
		t.Fatalf("the element count was lost: %q", got.Shape)
	}
}

// An accepted result records nothing: this evidence exists only for refusals.
func TestAcceptedResultRecordsNoRejectionEvidence(t *testing.T) {
	env := plannerEnvelope{StructuredOutput: json.RawMessage(`{"summary":"a genuinely long and real plan summary"}`)}
	if reason := resultConsistency(env, 4096, 1000); reason != "" {
		t.Fatalf("fixture should be accepted, got %q", reason)
	}
}
