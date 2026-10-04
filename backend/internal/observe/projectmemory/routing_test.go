package projectmemory

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func baseRecord() EvidenceRecord {
	return EvidenceRecord{
		RecordID:    "pmb-routing-1",
		GeneratedAt: time.Unix(0, 0).UTC(),
		Role:        domain.WorkflowRoleWorker,
	}
}

// The additive-schema promise, asserted on the wire format rather than on the
// struct: a record with no routing block serialises without the key at all, so
// a consumer written before the router keeps reading exactly what it read.
func TestRecordWithoutRoutingCarriesNoRoutingKey(t *testing.T) {
	payload, err := json.Marshal(baseRecord().normalized())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(payload), "\"routing\"") {
		t.Fatalf("a record with no routing story emitted a routing key:\n%s", payload)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"schemaVersion", "recordId", "role", "dispatch", "context", "providerTokens", "tools", "outcomes"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("existing key %q disappeared from the record:\n%s", key, payload)
		}
	}
	if decoded["schemaVersion"] != EvidenceSchemaVersion {
		t.Fatalf("schemaVersion is %v, want %q — the routing block must not version the record", decoded["schemaVersion"], EvidenceSchemaVersion)
	}
}

// Every routing size is held to the same labeling rule as a baseline one:
// bytes measured, tokens estimated, and a violation fails the record.
func TestRoutingMetricsAreLabeledAndValidated(t *testing.T) {
	routing := RoutingSelected(RoutingSelection{
		Role: "worker", Tier: "compact", Sections: 3, Dropped: 1, Truncated: 1,
		PotentialBytes: 4000, SelectedBytes: 1000, ReusedBytes: 400, NewBytes: 600,
		LimitTokens: 4000, HardCapTokens: 14000,
	})
	record := baseRecord()
	record.Routing = &routing
	record = record.normalized()
	if err := record.Validate(); err != nil {
		t.Fatalf("a well-formed routing block was rejected: %v", err)
	}
	for name, m := range map[string]Metric{
		"potentialBytes": routing.PotentialBytes,
		"selectedBytes":  routing.SelectedBytes,
		"reusedBytes":    routing.ReusedBytes,
		"newBytes":       routing.NewBytes,
	} {
		if m.Basis != BasisMeasured {
			t.Fatalf("%s is labeled %q, want measured", name, m.Basis)
		}
	}
	for name, m := range map[string]Metric{
		"potentialTokens": routing.PotentialTokens,
		"selectedTokens":  routing.SelectedTokens,
		"newTokens":       routing.NewTokens,
	} {
		if m.Basis != BasisEstimated {
			t.Fatalf("%s is labeled %q, want estimated — AO does not run the provider's tokenizer", name, m.Basis)
		}
		if m.Method != EstimateMethod {
			t.Fatalf("%s does not name the heuristic behind it: %q", name, m.Method)
		}
	}

	broken := record
	bad := routing
	bad.SelectedBytes = Metric{Basis: BasisMeasured, Method: "no value"}
	broken.Routing = &bad
	if err := broken.normalized().Validate(); !errors.Is(err, ErrMetricInvalid) {
		t.Fatalf("an unlabeled routing metric was accepted: %v", err)
	}
}

// A disabled routing block still says what the dispatch sent, and never
// pretends a measurement it did not make.
func TestRoutingDisabledSizesTheUnroutedPayload(t *testing.T) {
	routing := RoutingDisabled(2048, "")
	if routing.Enabled {
		t.Fatal("RoutingDisabled produced an enabled block")
	}
	if strings.TrimSpace(routing.Reason) == "" {
		t.Fatal("a disabled block must state why")
	}
	if got := *routing.PotentialBytes.Value; got != 2048 {
		t.Fatalf("potential bytes %d, want the whole unrouted payload", got)
	}
	if *routing.SelectedBytes.Value != *routing.PotentialBytes.Value {
		t.Fatal("an unrouted dispatch sent everything it had; potential and selected must agree")
	}
	if *routing.ReusedBytes.Value != 0 {
		t.Fatal("nothing is reused when the router did not run")
	}
	if routing.LimitTokens.Basis != BasisUnavailable || routing.HardCapTokens.Basis != BasisUnavailable {
		t.Fatal("a dispatch no budget applied to must report the budget as unavailable, not as zero")
	}
	record := baseRecord()
	record.Routing = &routing
	if err := record.normalized().Validate(); err != nil {
		t.Fatalf("a disabled routing block was rejected: %v", err)
	}
}

// The reduction figure refuses to invent a percentage it has no basis for.
func TestReductionPercentNeedsAMeasuredPotential(t *testing.T) {
	routing := RoutingSelected(RoutingSelection{PotentialBytes: 1000, SelectedBytes: 250})
	pct, ok := routing.ReductionPercent()
	if !ok || pct != 75 {
		t.Fatalf("reduction %.1f%% (ok=%v), want 75%%", pct, ok)
	}
	empty := RoutingSelected(RoutingSelection{})
	if _, ok := empty.ReductionPercent(); ok {
		t.Fatal("a zero potential produced a reduction percentage")
	}
	unknown := RoutingMetrics{PotentialBytes: Unavailable("not measured"), SelectedBytes: Measured(10, "measured")}
	if _, ok := unknown.ReductionPercent(); ok {
		t.Fatal("an unavailable potential produced a reduction percentage")
	}
}

// The carrier between the two independent dispatch wrappers.
func TestRoutingTravelsThroughContextOntoTheRecord(t *testing.T) {
	routing := RoutingSelected(RoutingSelection{Role: "worker", Tier: "compact", PotentialBytes: 900, SelectedBytes: 300, NewBytes: 300})
	ctx := WithRouting(context.Background(), routing)

	span := NewRecorder(nil).Begin(Dispatch{Role: domain.WorkflowRoleWorker, Observable: Capabilities{ContextPayload: true}})
	span.ObserveContextSent("a routed payload")
	if !span.ObserveRoutingFromContext(ctx) {
		t.Fatal("the routing block did not reach the span")
	}
	record := span.Build(nil)
	if record.Routing == nil || !record.Routing.Enabled {
		t.Fatalf("the record did not carry the routing decision: %+v", record.Routing)
	}
	if *record.Routing.PotentialBytes.Value != 900 {
		t.Fatalf("potential bytes %d, want 900", *record.Routing.PotentialBytes.Value)
	}

	// Without a carrier, the same dispatch records a disabled block sized by
	// what it actually sent -- never a silent absence that would read as "the
	// router shaped this".
	bare := NewRecorder(nil).Begin(Dispatch{Role: domain.WorkflowRoleWorker, Observable: Capabilities{ContextPayload: true}})
	bare.ObserveContextSent("an unrouted payload")
	if span.ObserveRoutingFromContext(context.Background()) {
		t.Fatal("an empty context reported a routing block")
	}
	bareRecord := bare.Build(nil)
	if bareRecord.Routing == nil || bareRecord.Routing.Enabled {
		t.Fatalf("an unrouted dispatch did not record a disabled routing block: %+v", bareRecord.Routing)
	}
	if *bareRecord.Routing.SelectedBytes.Value != int64(len("an unrouted payload")) {
		t.Fatalf("the disabled block does not size the payload that went out: %+v", bareRecord.Routing.SelectedBytes)
	}
}
