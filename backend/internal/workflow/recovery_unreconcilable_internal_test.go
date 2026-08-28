package workflow

import (
	"strings"
	"testing"
)

// The stop AO records for a run it could not reconcile must be one a PERSON is
// told to act on. Registering it as self-remediable would say AO retries its way
// out of an answer that is deterministic by definition -- which is exactly the
// forever-retry this reason exists to end.
func TestRecoveryUnreconcilableIsAHumanActionableStop(t *testing.T) {
	d, ok := attentionDispositions[ReasonRecoveryUnreconcilable]
	if !ok {
		t.Fatalf("%q is not registered in the attention dispositions table", ReasonRecoveryUnreconcilable)
	}
	if d.SelfRemediable {
		t.Fatal("an unreconcilable run was classified as something AO will fix by itself")
	}
	if strings.TrimSpace(d.HumanAction) == "" {
		t.Fatal("the stop carries no action for the person it is escalated to")
	}
}
