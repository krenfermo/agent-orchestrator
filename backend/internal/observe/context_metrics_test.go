package observe

import (
	"math"
	"testing"

	baseline "github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory"
)

// routedRecord is one dispatch a router selection shaped.
func routedRecord(sent, potential, selected, reused, fresh int64) baseline.EvidenceRecord {
	routing := baseline.RoutingSelected(baseline.RoutingSelection{
		Role: "worker", Tier: "compact",
		PotentialBytes: potential, SelectedBytes: selected, ReusedBytes: reused, NewBytes: fresh,
	})
	return baseline.EvidenceRecord{
		Context: baseline.ContextMetrics{ContextSentBytes: baseline.Measured(sent, "test")},
		Routing: &routing,
	}
}

func TestSummarizeContextSelectionAddsMeasuredSizes(t *testing.T) {
	routing := baseline.RoutingDisabled(500, "the router was off for this dispatch")
	summary := SummarizeContextSelection([]baseline.EvidenceRecord{
		routedRecord(1000, 4000, 1000, 400, 600),
		routedRecord(2000, 6000, 2000, 500, 1500),
		{
			Context: baseline.ContextMetrics{ContextSentBytes: baseline.Measured(500, "test")},
			Routing: &routing,
		},
		// A verify dispatch: no payload, no routing story, and no opinion
		// about either.
		{Context: baseline.ContextMetrics{ContextSentBytes: baseline.Unavailable("this dispatch carries no payload")}},
	})

	if summary.Dispatches != 4 {
		t.Fatalf("dispatches = %d, want 4", summary.Dispatches)
	}
	if summary.Routed != 2 || summary.Unrouted != 1 {
		t.Fatalf("routed=%d unrouted=%d, want 2 and 1", summary.Routed, summary.Unrouted)
	}
	if summary.SentBytes != 3500 {
		t.Fatalf("sentBytes = %d, want 3500 (the unmeasured payload must not count as zero)", summary.SentBytes)
	}
	if summary.PotentialBytes != 10500 || summary.SelectedBytes != 3500 {
		t.Fatalf("potential=%d selected=%d, want 10500 and 3500", summary.PotentialBytes, summary.SelectedBytes)
	}
	if summary.ReusedBytes+summary.NewBytes != summary.SelectedBytes {
		t.Fatalf("reused %d + new %d != selected %d", summary.ReusedBytes, summary.NewBytes, summary.SelectedBytes)
	}
	// The verify dispatch's unavailable payload is the one unmeasured size, so
	// the totals are honestly a lower bound.
	if summary.Complete() || summary.Unmeasured != 1 {
		t.Fatalf("unmeasured = %d (complete=%v), want 1 and false", summary.Unmeasured, summary.Complete())
	}
	if pct, ok := summary.ReductionPercent(); !ok || !near(pct, 200.0/3) {
		t.Fatalf("reduction = %.4f%% (ok=%v), want 66.6667%%", pct, ok)
	}
	if pct, ok := summary.ReusedSharePercent(); !ok || !near(pct, 900.0/3500*100) {
		t.Fatalf("reused share = %.4f%% (ok=%v), want 25.7143%%", pct, ok)
	}
}

// The distinction the whole baseline rests on: a size nobody measured is not a
// measured zero, and no percentage is invented from one.
func TestSummarizeContextSelectionRefusesToInventPercentages(t *testing.T) {
	empty := SummarizeContextSelection(nil)
	if _, ok := empty.ReductionPercent(); ok {
		t.Fatal("an empty summary produced a reduction percentage")
	}
	if _, ok := empty.ReusedSharePercent(); ok {
		t.Fatal("an empty summary produced a reused share")
	}
	if !empty.Complete() {
		t.Fatal("a summary of nothing has nothing unmeasured")
	}

	unmeasured := SummarizeContextSelection([]baseline.EvidenceRecord{{
		Context: baseline.ContextMetrics{ContextSentBytes: baseline.Unavailable("surface carries no payload")},
		Routing: &baseline.RoutingMetrics{
			Enabled:        true,
			PotentialBytes: baseline.Unavailable("not measured"),
			SelectedBytes:  baseline.Unavailable("not measured"),
			ReusedBytes:    baseline.Unavailable("not measured"),
			NewBytes:       baseline.Unavailable("not measured"),
		},
	}})
	if unmeasured.PotentialBytes != 0 || unmeasured.SentBytes != 0 {
		t.Fatalf("unavailable sizes were summed: %+v", unmeasured)
	}
	if unmeasured.Complete() || unmeasured.Unmeasured != 5 {
		t.Fatalf("unmeasured = %d, want 5 (the payload plus four routing sizes)", unmeasured.Unmeasured)
	}
	if _, ok := unmeasured.ReductionPercent(); ok {
		t.Fatal("a summary with no measured potential produced a reduction percentage")
	}
}

// An estimated byte figure is not a measured one, and must not be summed into
// a total this read model presents as measured bytes.
func TestSummarizeContextSelectionSumsOnlyMeasuredBytes(t *testing.T) {
	summary := SummarizeContextSelection([]baseline.EvidenceRecord{{
		Context: baseline.ContextMetrics{ContextSentBytes: baseline.Estimated(1234, "a guess")},
	}})
	if summary.SentBytes != 0 || summary.Unmeasured != 1 {
		t.Fatalf("an estimated byte figure was added to a measured total: %+v", summary)
	}
}

func TestEstimatedTokensUsesTheBaselineHeuristic(t *testing.T) {
	if got, want := EstimatedTokens(4000), baseline.EstimateTokensFromBytes(4000); got != want {
		t.Fatalf("EstimatedTokens(4000) = %d, want %d", got, want)
	}
}

// near compares two percentages without demanding bit-identical float
// arithmetic, which would pin the order of operations inside the summary
// rather than its result.
func near(got, want float64) bool { return math.Abs(got-want) < 1e-9 }
