package projectmemory

import (
	"strings"
	"testing"
)

func TestExternalEvidenceLabelsItselfAsLiveState(t *testing.T) {
	e := clampExternal(ExternalEvidence{Source: "github", Rendered: "Branch: feat/x\n"})
	rendered := e.Render()
	if !strings.Contains(rendered, "EXTERNAL CONTEXT (github)") {
		t.Fatalf("external block is not labelled as external:\n%s", rendered)
	}
	// An agent that reads a check result as a durable project fact will act on
	// it long after it stopped being true, so the block says what it is.
	if !strings.Contains(rendered, "not durable project knowledge") {
		t.Fatalf("external block does not disclaim durability:\n%s", rendered)
	}
	if e.Bytes != len(rendered) {
		t.Fatalf("Bytes = %d; want the rendered length %d", e.Bytes, len(rendered))
	}
	if e.EstimatedTokens != EstimateTokens(e.Bytes) {
		t.Fatalf("EstimatedTokens = %d; want %d", e.EstimatedTokens, EstimateTokens(e.Bytes))
	}
}

// The ceiling is AO's, enforced here rather than trusted to a provider that
// reads from a service AO does not control.
func TestClampExternalEnforcesTheCeiling(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 2000; i++ {
		b.WriteString("a line of external state that goes on and on\n")
	}
	e := clampExternal(ExternalEvidence{Source: "github", Rendered: b.String()})
	if !e.Truncated {
		t.Fatal("an oversized contribution was not truncated")
	}
	if len(e.Rendered) > MaxExternalContextBytes {
		t.Fatalf("rendered %d bytes; ceiling is %d", len(e.Rendered), MaxExternalContextBytes)
	}
	// Cut on a line boundary: half a fact is worse than one fewer fact.
	if strings.HasSuffix(e.Rendered, "a line of external state that goes on and o") {
		t.Fatal("truncation cut mid-line")
	}
	if !strings.Contains(e.Render(), "truncated") {
		t.Fatal("a truncated block does not say so")
	}
}

func TestClampExternalOnEmptyContribution(t *testing.T) {
	e := clampExternal(ExternalEvidence{Source: "github", Reason: "GitHub is unreachable.", Degraded: true})
	if !e.Empty() || e.Bytes != 0 || e.Render() != "" {
		t.Fatalf("an empty contribution measured as %+v", e)
	}
}

// A provisioning with no durable memory but real external state still has
// something to send. Treating it as "nothing attached" would drop it.
func TestProvisionedAttachedCountsExternalEvidence(t *testing.T) {
	p := Provisioned{External: clampExternal(ExternalEvidence{Source: "github", Rendered: "Branch: feat/x"})}
	if !p.Attached() {
		t.Fatal("external-only provisioning reported as unattached")
	}
	if !strings.Contains(p.Render(), "Branch: feat/x") {
		t.Fatalf("external-only render lost the content:\n%s", p.Render())
	}
}

func TestProvisionedRenderKeepsMemoryAndExternalSeparate(t *testing.T) {
	p := Provisioned{External: clampExternal(ExternalEvidence{Source: "github", Rendered: "Branch: feat/x"})}
	rendered := p.Render()
	// Memory is empty here, so the external block must stand alone rather than
	// being prefixed by an empty memory pack.
	if strings.HasPrefix(rendered, "\n") {
		t.Fatalf("render starts with an empty memory section:\n%q", rendered)
	}
}
