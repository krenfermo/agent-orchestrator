package domain_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// explicitCompaction builds the policy a run gets from an explicit per-run
// opt-in, which is the only thing the create API can produce.
func explicitCompaction(enabled bool) domain.WorkflowPolicy {
	p := domain.DefaultWorkflowPolicy()
	p.SessionCompactionEnabled = enabled
	p.CompactionProvenance = domain.SessionCompactionProvenance{
		Version: domain.SessionCompactionPolicyVersion,
		Source:  domain.SessionCompactionExplicit,
		At:      time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC),
	}
	return p
}

// PHASE E, case 1. The default policy every run is seeded with has compaction
// off AND says nobody chose. Both halves matter: the second is what stops the
// default from propagating to a child as though it were a decision.
func TestDefaultWorkflowPolicyHasCompactionOffAndUnrecorded(t *testing.T) {
	t.Parallel()
	p := domain.DefaultWorkflowPolicy()
	if p.SessionCompactionEnabled {
		t.Fatal("DefaultWorkflowPolicy enables session compaction")
	}
	if p.CompactionProvenance.Recorded() {
		t.Fatalf("DefaultWorkflowPolicy claims a recorded compaction choice: %+v", p.CompactionProvenance)
	}
	enabled, recorded := p.SessionCompactionRequested()
	if enabled || recorded {
		t.Fatalf("SessionCompactionRequested() = (%t, %t), want (false, false)", enabled, recorded)
	}
}

// A recorded FALSE is a decision, not an absence. This is the distinction the
// whole provenance record exists for, so it is pinned directly.
func TestSessionCompactionRequestedSeparatesRefusalFromSilence(t *testing.T) {
	t.Parallel()
	enabled, recorded := explicitCompaction(false).SessionCompactionRequested()
	if enabled || !recorded {
		t.Fatalf("explicit false = (%t, %t), want (false, true)", enabled, recorded)
	}
	enabled, recorded = explicitCompaction(true).SessionCompactionRequested()
	if !enabled || !recorded {
		t.Fatalf("explicit true = (%t, %t), want (true, true)", enabled, recorded)
	}
}

// PHASE E, case 11. A raw `true` that reached a snapshot without a provenance
// record -- a hand-edited row, a corrupted blob, a future binary's field AO
// cannot read -- still runs (the flag is the flag, and nothing here changes
// what the run itself does), but it is NOT inheritable and does not read as a
// request. Validation is what gates propagation, not the bare value.
func TestUnrecordedCompactionIsNotTreatedAsARequest(t *testing.T) {
	t.Parallel()
	corrupt := domain.DefaultWorkflowPolicy()
	corrupt.SessionCompactionEnabled = true // no provenance: nobody recorded this

	if enabled, recorded := corrupt.SessionCompactionRequested(); enabled || recorded {
		t.Fatalf("unrecorded true read as a request: (%t, %t)", enabled, recorded)
	}
	child := domain.InheritWorkflowPolicy(corrupt, domain.DefaultWorkflowPolicy())
	if child.SessionCompactionEnabled {
		t.Fatal("an unrecorded parent flag was inherited by a child")
	}
	if child.CompactionProvenance.Recorded() {
		t.Fatalf("a child invented a provenance record: %+v", child.CompactionProvenance)
	}
	// And it is not refused either: a legacy/unreadable parent is history, not
	// a defect, so dispatch must not start failing because of it.
	if err := domain.RequireInheritedWorkflowPolicy(corrupt, child); err != nil {
		t.Fatalf("RequireInheritedWorkflowPolicy refused a legacy parent: %v", err)
	}
	// An unknown future source is treated exactly the same way.
	future := corrupt
	future.CompactionProvenance = domain.SessionCompactionProvenance{Source: "quantum"}
	if _, recorded := future.SessionCompactionRequested(); recorded {
		t.Fatal("an unrecognised provenance source read as recorded")
	}
}

// PHASE B / PHASE E, case 5. A recorded choice travels to the child, in BOTH
// directions, and the child's record says it was inherited.
func TestInheritWorkflowPolicyCarriesARecordedCompactionChoice(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{true, false} {
		parent := explicitCompaction(enabled)
		parent.CompactionProvenance.RequestedBy = "operator-1"
		child := domain.InheritWorkflowPolicy(parent, domain.DefaultWorkflowPolicy())

		if child.SessionCompactionEnabled != enabled {
			t.Fatalf("child compaction = %t, want the parent's %t", child.SessionCompactionEnabled, enabled)
		}
		if child.CompactionProvenance.Source != domain.SessionCompactionInherited {
			t.Fatalf("child source = %q, want inherited", child.CompactionProvenance.Source)
		}
		// The audit record keeps WHO asked and WHEN -- the child is carrying
		// that person's decision, not making a new one of its own.
		if child.CompactionProvenance.RequestedBy != "operator-1" {
			t.Fatalf("child lost the requester: %+v", child.CompactionProvenance)
		}
		if !child.CompactionProvenance.At.Equal(parent.CompactionProvenance.At) {
			t.Fatalf("child rewrote the decision time: %+v", child.CompactionProvenance)
		}
		// ParentRunID is the caller's to stamp; inheritance must not invent one.
		if child.CompactionProvenance.ParentRunID != "" {
			t.Fatalf("pure inheritance invented a parent id: %+v", child.CompactionProvenance)
		}
		if err := domain.RequireInheritedWorkflowPolicy(parent, child); err != nil {
			t.Fatalf("a correctly inherited child was refused: %v", err)
		}
	}
}

// The parent's contract wins over whatever the child was seeded with. A child
// is where an objective's fix cycles actually run, so a child that kept its own
// default would make the parent's opt-in decorative -- exactly the F1 failure
// this function was written to close, restated for compaction.
func TestInheritWorkflowPolicyOverwritesTheChildSeedValue(t *testing.T) {
	t.Parallel()
	parent := explicitCompaction(true)
	seeded := explicitCompaction(false) // a child that somehow recorded the opposite
	child := domain.InheritWorkflowPolicy(parent, seeded)
	if !child.SessionCompactionEnabled {
		t.Fatal("the child's own value survived its parent's contract")
	}

	// And the gate proves it: a child left at the opposite value is refused
	// rather than silently dispatched under a contract it is not honouring.
	stale := seeded
	if err := domain.RequireInheritedWorkflowPolicy(parent, stale); err == nil {
		t.Fatal("RequireInheritedWorkflowPolicy accepted a child that disagrees with its parent")
	}
	// So is a child that recorded nothing at all under a parent that did.
	if err := domain.RequireInheritedWorkflowPolicy(parent, domain.DefaultWorkflowPolicy()); err == nil {
		t.Fatal("RequireInheritedWorkflowPolicy accepted a child with no record under an opted-in parent")
	}
}

// PHASE E, case 12. Both new fields survive the policy_snapshot round-trip
// unchanged, which is the only reason any of this is durable.
func TestPolicySnapshotRoundTripKeepsCompactionAndContextThreshold(t *testing.T) {
	t.Parallel()
	before := explicitCompaction(true)
	before.CompactionProvenance.RequestedBy = "operator-1"
	before.CompactionProvenance.ParentRunID = "wf-parent"
	usage := before.EffectiveUsageBudgetPolicy()
	usage.WorkflowContextPerCallWarnTokens = 70_000
	before.Usage = usage

	blob, err := json.Marshal(before)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var after domain.WorkflowPolicy
	if err := json.Unmarshal(blob, &after); err != nil {
		t.Fatalf("unmarshal %s: %v", blob, err)
	}

	if !after.SessionCompactionEnabled {
		t.Fatalf("compaction flag lost in the round trip: %s", blob)
	}
	if after.CompactionProvenance != before.CompactionProvenance {
		t.Fatalf("provenance changed: %+v -> %+v", before.CompactionProvenance, after.CompactionProvenance)
	}
	if got := after.EffectiveUsageBudgetPolicy().WorkflowContextPerCallWarnTokens; got != 70_000 {
		t.Fatalf("context threshold = %d, want 70000 (blob %s)", got, blob)
	}
	// And it is readable under the key an operator inspecting the row would
	// look for, rather than only through Go's own struct tags.
	var raw map[string]any
	if err := json.Unmarshal(blob, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if raw["sessionCompactionEnabled"] != true {
		t.Fatalf("snapshot key sessionCompactionEnabled = %v: %s", raw["sessionCompactionEnabled"], blob)
	}
	if _, ok := raw["compactionProvenance"]; !ok {
		t.Fatalf("snapshot carries no compactionProvenance: %s", blob)
	}

	// A snapshot written before any of this existed decodes to the safe state.
	var legacy domain.WorkflowPolicy
	if err := json.Unmarshal([]byte(`{"version":"v1","maxFixCycles":3}`), &legacy); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if legacy.SessionCompactionEnabled || legacy.CompactionProvenance.Recorded() {
		t.Fatalf("a pre-P7 snapshot decoded as an opt-in: %+v", legacy)
	}
	if got := domain.UsageBudgetProfileFor(domain.ExecutionStrategyTask).
		WithOverrides(legacy.EffectiveUsageBudgetPolicy()).ContextPerCallTokens; got != 150_000 {
		t.Fatalf("a pre-P7 snapshot's task threshold = %d, want the 150000 default", got)
	}
}
