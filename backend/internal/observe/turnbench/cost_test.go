package turnbench

import (
	"math"
	"os"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/usage/pricing"
)

func loadFixture(t *testing.T, name string) Series {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	defer f.Close()
	s, err := LoadSeries(f)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return s
}

// testRates is a rate card with exactly the rows a test names, so an assertion
// cannot be made accidentally true or false by the embedded catalog changing.
type testRates map[string]struct{ in, out, read, write5m, write1h float64 }

func (r testRates) Cost(modelID string, tokens domain.UsageTokenTotals) domain.UsageCost {
	rate, ok := r[modelID]
	if !ok {
		return domain.UsageCost{Known: false, Basis: domain.CostUnknown, UnpricedModels: []string{modelID}}
	}
	const perMillion = 1_000_000.0
	return domain.UsageCost{
		Known: true, Basis: domain.CostCalculated, Currency: "USD",
		Amount: float64(tokens.UncachedInputTokens)*rate.in/perMillion +
			float64(tokens.CacheReadTokens)*rate.read/perMillion +
			cacheCost(rate, tokens)/perMillion +
			float64(tokens.OutputTokens)*rate.out/perMillion,
		TTLAssumedTokens: assumedTTL(tokens),
		PricingSource:    "test", PricingVersion: "test",
	}
}

// opusRates prices the base model only -- which is what production does.
func opusRates() testRates {
	return testRates{"claude-opus-5": {in: 5, out: 25, read: 0.5, write5m: 6.25, write1h: 10}}
}

// cacheCost prices cache creation by lifetime; a lifetime nothing reported is
// priced at the short rate and disclosed through TTLAssumedTokens, which is the
// same compromise the real rate table makes.
func cacheCost(rate struct{ in, out, read, write5m, write1h float64 }, tokens domain.UsageTokenTotals) float64 {
	split := tokens.CacheCreation
	if split.Total() == 0 {
		split = domain.CacheCreationSplit{UnknownTTLTokens: tokens.CacheWriteTokens}
	}
	return float64(split.Ephemeral5mTokens)*rate.write5m +
		float64(split.Ephemeral1hTokens)*rate.write1h +
		float64(split.UnknownTTLTokens)*rate.write5m
}

func assumedTTL(tokens domain.UsageTokenTotals) int64 {
	if tokens.CacheCreation.Total() == 0 {
		return tokens.CacheWriteTokens
	}
	return tokens.CacheCreation.UnknownTTLTokens
}

// opusAndVariantRates additionally covers the long-context spelling the harness
// rolls a session up under. The equal rate is a TEST assumption, stated here
// and nowhere else: the embedded catalog deliberately does not make it.
func opusAndVariantRates() testRates {
	r := opusRates()
	r["claude-opus-5[1m]"] = r["claude-opus-5"]
	return r
}

func near(t *testing.T, label string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("%s = %v, want %v (+/- %v)", label, got, want, tol)
	}
}

func TestSplitFixtureMatchesTheRecordedLedger(t *testing.T) {
	m := Measure(loadFixture(t, "wf-1c2cb9bd-split.json"))
	if m.Calls != 193 {
		t.Fatalf("calls = %d, want 193", m.Calls)
	}
	if !m.SplitKnown {
		t.Fatal("the split fixture must carry a consistent split")
	}
	if m.Tokens.InputTokens != 36_082_816 || m.Tokens.UncachedInputTokens != 386 ||
		m.Tokens.CacheReadTokens != 35_787_742 || m.Tokens.CacheWriteTokens != 294_688 ||
		m.Tokens.OutputTokens != 139_003 {
		t.Fatalf("tokens = %+v", m.Tokens)
	}
	if m.Compactions != 0 {
		t.Fatalf("compactions = %d, want 0 -- this run never compacted", m.Compactions)
	}
}

func TestTheOriginalRecordingStillMeasuresAndStillCannotBePriced(t *testing.T) {
	s := loadFixture(t, "wf-1c2cb9bd.json")
	m := Measure(s)
	// Every token answer the original fixture could give, it still gives.
	if m.Calls != 193 || m.CumulativeInput != 36_082_816 || m.OutputTokens != 139_003 {
		t.Fatalf("the untouched fixture must still measure: %+v", m)
	}
	if m.SplitKnown {
		t.Fatal("a recording with no billed split must not claim one")
	}
	// The recording names no model either, so it is refused twice over. Naming
	// one isolates the reason this test is about.
	if got := CostOf(s, opusRates(), m); got.Known {
		t.Fatalf("an unsplit, unmodelled recording must not price: %+v", got)
	}
	s.ModelID = "claude-opus-5"
	cost := CostOf(s, opusRates(), m)
	if cost.Known || cost.Reason != CostReasonSplitUnknown {
		t.Fatalf("cost = %+v, want refused as split_unknown", cost)
	}
	// And refusing is the whole point: a sum of billed input carries no
	// information about which of three rates applied to it.
	if cost.CallCost.Known {
		t.Fatal("no partial money figure may escape a refusal")
	}
}

func TestTokenSavingsAreNotCostSavings(t *testing.T) {
	before := loadFixture(t, "wf-1c2cb9bd-split.json")
	// Both sides are compared on their CALLS, which is the only thing the
	// replay changes. The recorded side's measured residual is dropped rather
	// than set against the other side's modelled compactions: it is noise on
	// this run (121 output tokens across 193 calls), the harness reports it
	// with no cache lifetime so it cannot be priced exactly anyway, and
	// weighing a measured figure against a modelled one would be the same
	// category error this file exists to prevent.
	before.Unattributed = nil
	after := Apply(before, Policy{
		CompactAtSegmentBoundary: true,
		PostCompactContextTokens: 57_404,
		// 26,009 of the post-compaction conversation is the standing prefix
		// this session started from and keeps cached; the rest is written.
		PostCompactCacheWriteTokens: 57_404 - 26_009,
		// The rewritten prefix is created at the same lifetime every write in
		// this session was: the long one. Stated, because a replay that does
		// not state it is refused rather than priced at the cheap rate.
		PostCompactCacheLifetime: "1h",
		// The canary's MEASURED output residual per compaction: 16,953 over
		// two compactions. It is evidence from another session, used here as a
		// stated assumption, which is why the replayed side reports its
		// compaction basis as modelled rather than measured.
		CompactionSummaryOutputTokens: 8_477,
		CompactionSummaryOutputKnown:  true,
	})
	after.Unattributed = nil

	rates := opusAndVariantRates()
	beforeScenario := MeasureScenario(before, rates)
	afterScenario := MeasureScenario(after, rates)
	if !beforeScenario.Cost.Known || !afterScenario.Cost.Known {
		t.Fatalf("both sides must price: %v / %v", beforeScenario.Cost.Reason, afterScenario.Cost.Reason)
	}
	if beforeScenario.Cost.CompactionBasis != CompactionBasisNone {
		t.Fatalf("the recorded side never compacted, got %q", beforeScenario.Cost.CompactionBasis)
	}
	if afterScenario.Cost.CompactionBasis != CompactionBasisModelled {
		t.Fatalf("the replayed side can only be modelled, got %q", afterScenario.Cost.CompactionBasis)
	}
	if afterScenario.Metrics.Compactions != 3 {
		t.Fatalf("compactions = %d, want one per repair boundary", afterScenario.Metrics.Compactions)
	}

	savings := CompareScenarios(beforeScenario, afterScenario)
	if !savings.TokenReductionKnown || !savings.CostReductionKnown {
		t.Fatalf("both reductions must be available here: %+v", savings)
	}
	// THE ASSERTION THIS FILE EXISTS FOR. The token reduction is the figure
	// docs/p7-turn-economy.md reports; the cost reduction is the one a bill
	// would show, and it is materially smaller. If a future change ever makes
	// these two equal, it has either stopped charging for the summaries or
	// started pricing every input token at one rate -- and both are the bug.
	near(t, "token reduction", savings.TokenReductionPercent, 38.9, 1.0)
	if savings.CostReductionPercent >= savings.TokenReductionPercent-5 {
		t.Fatalf("cost reduction %.1f%% must be materially below token reduction %.1f%%",
			savings.CostReductionPercent, savings.TokenReductionPercent)
	}
	// At the lifetime these writes were actually created with, the money
	// saving is smaller still than P7.1 reported: every compaction rewrites a
	// prefix at the dearest rate in the card.
	near(t, "cost reduction", savings.CostReductionPercent, 22.0, 7.0)
}

func TestTheCanaryFixtureCarriesItsMeasuredCompactions(t *testing.T) {
	s := loadFixture(t, "wf-66f0ee54.json")
	m := Measure(s)
	if m.Calls != 28 || m.CumulativeInput != 1_829_810 || m.OutputTokens != 30_500 {
		t.Fatalf("canary metrics = %+v", m)
	}
	if m.Compactions != 2 {
		t.Fatalf("compactions = %d, want 2", m.Compactions)
	}
	if m.CompactionPreTokens != 153_510 || m.CompactionPostTokens != 24_196 {
		t.Fatalf("pre/post = %d/%d", m.CompactionPreTokens, m.CompactionPostTokens)
	}
	if m.CompactionDurationMs != 182_466 {
		t.Fatalf("duration = %d ms, want 182,466 -- three minutes of wall clock", m.CompactionDurationMs)
	}
	// Nobody measured what each compaction generated, and the fixture says so
	// rather than implying zero.
	if m.SummaryOutputKnown || m.SummaryOutputTokens != 0 {
		t.Fatalf("per-boundary summary output is not reported by the harness: %+v", m)
	}
}

func TestTheRealResidualIsUnpricedInProduction(t *testing.T) {
	s := loadFixture(t, "wf-66f0ee54.json")
	scenario := MeasureScenario(s, pricing.Embedded())
	if scenario.Cost.Known {
		t.Fatal("the embedded catalog covers no claude-opus-5[1m]; the total must be refused")
	}
	if scenario.Cost.Reason != CostReasonUnpricedUnattributedModel {
		t.Fatalf("reason = %q, want the residual named as the unpriced part", scenario.Cost.Reason)
	}
	// The tokens survive the refusal in full -- P3-E's rule, unchanged.
	if scenario.Metrics.Tokens.CacheReadTokens != 1_719_470 {
		t.Fatalf("tokens = %+v", scenario.Metrics.Tokens)
	}
}

func TestTheCanarysCallsPriceAtTheLifetimeTheyWereCreatedAt(t *testing.T) {
	s := loadFixture(t, "wf-66f0ee54.json")
	m := Measure(s)
	if !m.CacheTTLKnown {
		t.Fatal("the canary's calls all carry a lifetime")
	}
	// 110,284 tokens of cache creation, every one of them at the LONG
	// lifetime. Priced at the short one the calls came to $2.3118; priced at
	// the one they were actually created at they come to $2.7254 -- 17.9%
	// more, on a figure AO reported as complete.
	if m.Tokens.CacheCreation.Ephemeral1hTokens != 110_284 || m.Tokens.CacheCreation.Ephemeral5mTokens != 0 {
		t.Fatalf("split = %+v", m.Tokens.CacheCreation)
	}
	cost := CostOf(s, opusAndVariantRates(), m)
	// The scenario TOTAL is still refused, and for a reason worth pinning: the
	// harness's own end-of-session rollup reports no lifetime at all, so the
	// residual beside the calls cannot be priced without assuming one. The
	// calls are exact; the gap is not priceable; the benchmark says so rather
	// than adding them.
	if cost.Known || cost.Reason != CostReasonCacheTTLUnknown {
		t.Fatalf("cost = %+v, want refused as cache_ttl_unknown", cost)
	}
	// Without the unpriceable residual, the calls themselves price exactly.
	callsOnly := s
	callsOnly.Unattributed = nil
	callsOnly.Compactions = nil
	onlyCost := CostOf(callsOnly, opusAndVariantRates(), Measure(callsOnly))
	if !onlyCost.Known {
		t.Fatalf("the calls alone must price: %q", onlyCost.Reason)
	}
	near(t, "call cost at the true lifetime", onlyCost.CallCost.Amount, 2.7254, 0.001)
}

func TestACompactionWhoseOutputNobodyMeasuredRefusesTheTotal(t *testing.T) {
	s := loadFixture(t, "wf-1c2cb9bd-split.json")
	s.Unattributed = nil
	s.Compactions = []Compaction{{PreTokens: 200_000, PostTokens: 50_000}}
	cost := CostOf(s, opusRates(), Measure(s))
	if cost.Known || cost.Reason != CostReasonSummaryOutputUnknown {
		t.Fatalf("cost = %+v, want refused", cost)
	}
	if cost.CallCost.Known {
		t.Fatal("the call half must not be reported as a total when the other half is missing")
	}
}

func TestAnUnpricedSeriesModelIsNamedAsSuch(t *testing.T) {
	s := loadFixture(t, "wf-1c2cb9bd-split.json")
	s.ModelID = "gpt-5.6-sol"
	cost := CostOf(s, opusRates(), Measure(s))
	if cost.Known || cost.Reason != CostReasonUnpricedModel {
		t.Fatalf("cost = %+v, want unpriced_model", cost)
	}
}

func TestASeriesWithNoModelOrNoPricerIsRefusedDistinctly(t *testing.T) {
	s := loadFixture(t, "wf-1c2cb9bd-split.json")
	if got := CostOf(s, nil, Measure(s)); got.Reason != CostReasonNoPricer {
		t.Fatalf("reason = %q, want no_pricer", got.Reason)
	}
	s.ModelID = ""
	if got := CostOf(s, opusRates(), Measure(s)); got.Reason != CostReasonNoModel {
		t.Fatalf("reason = %q, want no_model", got.Reason)
	}
}

func TestReplayKeepsEveryCallsSplitConsistent(t *testing.T) {
	before := loadFixture(t, "wf-1c2cb9bd-split.json")
	after := Apply(before, Policy{
		CompactAtSegmentBoundary:      true,
		PostCompactContextTokens:      57_404,
		PostCompactCacheWriteTokens:   31_395,
		PostCompactCacheLifetime:      "1h",
		CompactionSummaryOutputTokens: 8_477,
		CompactionSummaryOutputKnown:  true,
	})
	m := Measure(after)
	if !m.SplitKnown {
		t.Fatal("a replayed series must stay partitioned")
	}
	for i, call := range after.Calls {
		if call.UncachedInputTokens+call.CacheReadTokens+call.CacheWriteTokens != call.ContextTokens {
			t.Fatalf("call %d does not add up: %+v", i, call)
		}
		if call.CacheReadTokens < 0 || call.CacheWriteTokens < 0 {
			t.Fatalf("call %d has a negative dimension: %+v", i, call)
		}
	}
	// The written half GROWS across a replay, because every boundary rewrites a
	// prefix the unreplayed run had cached. That is the mechanism by which a
	// token saving shrinks into a smaller money one.
	if m.Tokens.CacheWriteTokens <= Measure(before).Tokens.CacheWriteTokens {
		t.Fatalf("replayed cache writes = %d, want more than the recorded %d",
			m.Tokens.CacheWriteTokens, Measure(before).Tokens.CacheWriteTokens)
	}
}

func TestReplayingASeriesWithoutASplitLeavesItWithoutOne(t *testing.T) {
	before := loadFixture(t, "wf-1c2cb9bd.json")
	after := Apply(before, Policy{
		CompactAtSegmentBoundary:      true,
		PostCompactContextTokens:      57_404,
		PostCompactCacheWriteTokens:   31_395,
		PostCompactCacheLifetime:      "1h",
		CompactionSummaryOutputTokens: 8_477,
		CompactionSummaryOutputKnown:  true,
	})
	m := Measure(after)
	if m.SplitKnown {
		t.Fatal("a replay must not invent a partition the recording never had")
	}
	// The token answer is still the one P7 published.
	if ReductionPercent(Measure(before).CumulativeInput, m.CumulativeInput) < 30 {
		t.Fatal("the token reduction must survive: this is P7's own 30% gate")
	}
}

// TestScenarioReportsAreRenderable is the demonstration §3 asks for: the two
// historical scenarios, rendered, with TOKEN SAVINGS and COST SAVINGS on
// separate labelled lines. Run with -v to read them.
func TestScenarioReportsAreRenderable(t *testing.T) {
	rates := opusAndVariantRates()

	// wf-1c2cb9bd: the measured run and its replay, compared on their calls.
	// The recorded side's residual is dropped for the reason
	// TestTokenSavingsAreNotCostSavings gives.
	before := loadFixture(t, "wf-1c2cb9bd-split.json")
	before.Unattributed = nil
	after := Apply(before, Policy{
		CompactAtSegmentBoundary:      true,
		PostCompactContextTokens:      57_404,
		PostCompactCacheWriteTokens:   57_404 - 26_009,
		PostCompactCacheLifetime:      "1h",
		CompactionSummaryOutputTokens: 8_477,
		CompactionSummaryOutputKnown:  true,
	})
	after.Unattributed = nil
	after.Source = before.Source + " (replayed with compaction)"
	replay := Report(MeasureScenario(before, rates), MeasureScenario(after, rates))
	t.Log("\n" + replay)
	for _, want := range []string{"TOKEN SAVINGS", "COST SAVINGS", "created 1h", "modelled"} {
		if !contains(replay, want) {
			t.Fatalf("report is missing %q", want)
		}
	}

	// wf-66f0ee54: the canary, against production pricing and against a rate
	// card that covers the variant. Only the second produces money.
	canary := loadFixture(t, "wf-66f0ee54.json")
	t.Log("\n" + Report(MeasureScenario(canary, pricing.Embedded()), MeasureScenario(canary, rates)))
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
