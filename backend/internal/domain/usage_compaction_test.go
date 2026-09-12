package domain_test

import (
	"math"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// The two sessions below are the whole observed population of P7.1, and they
// are deliberately a matched pair: one compacted twice, one never compacted at
// all, and everything else about them is the same shape. Every figure comes
// from AO's own ledger and the harness's own transcript -- see
// docs/p7-compaction-observability.md for the provenance table.

// canaryAttributed is what the usage pipeline recorded for session
// ao-canary-fixture-6 of run wf-66f0ee54: 28 events.
func canaryAttributed() []domain.ModelUsageLine {
	return []domain.ModelUsageLine{{
		Harness: "claude-code",
		ModelID: "claude-opus-5",
		Tokens: domain.UsageTokenTotals{
			InputTokens: 1_829_810, UncachedInputTokens: 56,
			CacheReadTokens: 1_719_470, CacheWriteTokens: 110_284,
			OutputTokens: 30_500, EventCount: 28,
		},
	}}
}

// canaryHarness is that same session's own end-of-session cost-state rollup.
func canaryHarness() domain.HarnessSessionTotals {
	return domain.HarnessSessionTotals{
		Observed:        true,
		ReportedCostUSD: 3.4077755,
		Models: []domain.HarnessModelTotals{
			{
				ModelID: "claude-haiku-4-5-20251001",
				Tokens: domain.UsageTokenTotals{
					InputTokens: 4_758, UncachedInputTokens: 4_758, OutputTokens: 33,
				},
				ReportedCostUSD: 0.004923,
			},
			{
				ModelID: "claude-opus-5[1m]",
				Tokens: domain.UsageTokenTotals{
					InputTokens: 6_116 + 2_083_095 + 115_415, UncachedInputTokens: 6_116,
					CacheReadTokens: 2_083_095, CacheWriteTokens: 115_415,
					OutputTokens: 47_453,
				},
				ThinkingTokens:  6_326,
				ReportedCostUSD: 3.4028525,
			},
		},
	}
}

func canaryBoundaries() []domain.CompactionBoundary {
	return []domain.CompactionBoundary{
		{RecordUUID: "b1", SessionID: "ao-canary-fixture-6", ModelID: "claude-opus-5",
			Trigger: domain.CompactionTriggerManual, PreTokens: 85_847, PostTokens: 10_956,
			CumulativeDroppedTokens: 74_891, DurationMs: 95_073},
		{RecordUUID: "b2", SessionID: "ao-canary-fixture-6", ModelID: "claude-opus-5",
			Trigger: domain.CompactionTriggerManual, PreTokens: 67_663, PostTokens: 13_240,
			CumulativeDroppedTokens: 129_314, DurationMs: 87_393},
	}
}

// controlAttributed / controlHarness are the 193-call session of wf-1c2cb9bd,
// which never compacted.
func controlAttributed() []domain.ModelUsageLine {
	return []domain.ModelUsageLine{{
		Harness: "claude-code",
		ModelID: "claude-opus-5",
		Tokens: domain.UsageTokenTotals{
			InputTokens: 36_082_816, UncachedInputTokens: 386,
			CacheReadTokens: 35_787_742, CacheWriteTokens: 294_688,
			OutputTokens: 139_003, EventCount: 193,
		},
	}}
}

func controlHarness() domain.HarnessSessionTotals {
	return domain.HarnessSessionTotals{
		Observed:        true,
		ReportedCostUSD: 24.812496,
		Models: []domain.HarnessModelTotals{{
			ModelID: "claude-opus-5[1m]",
			Tokens: domain.UsageTokenTotals{
				InputTokens: 1_488 + 36_634_616 + 300_278, UncachedInputTokens: 1_488,
				CacheReadTokens: 36_634_616, CacheWriteTokens: 300_278,
				OutputTokens: 139_124,
			},
			ThinkingTokens:  38_205,
			ReportedCostUSD: 24.805628,
		}},
	}
}

// fakePricer is a rate card with exactly the rows a test names. It exists so a
// pricing assertion cannot be made accidentally true by the embedded catalog
// gaining or losing a model.
type fakePricer map[string]struct{ in, out, read, write float64 }

func (f fakePricer) Cost(modelID string, tokens domain.UsageTokenTotals) domain.UsageCost {
	rate, ok := f[modelID]
	if !ok {
		return domain.UsageCost{Known: false, Basis: domain.CostUnknown, UnpricedModels: []string{modelID}}
	}
	const perMillion = 1_000_000.0
	return domain.UsageCost{
		Known: true, Basis: domain.CostCalculated, Currency: "USD",
		Amount: float64(tokens.UncachedInputTokens)*rate.in/perMillion +
			float64(tokens.CacheReadTokens)*rate.read/perMillion +
			float64(tokens.CacheWriteTokens)*rate.write/perMillion +
			float64(tokens.OutputTokens)*rate.out/perMillion,
		PricingSource: "test", PricingVersion: "test",
	}
}

func opusOnly() fakePricer {
	return fakePricer{"claude-opus-5": {in: 5, out: 25, read: 0.5, write: 6.25}}
}

func opusAndLongContext() fakePricer {
	f := opusOnly()
	// Same rates as the base model. This is a TEST assumption and nothing
	// else: the embedded catalog deliberately does not make it, because a
	// long-context variant is not guaranteed to cost what the base model
	// costs. It is here to exercise the priced path, not to claim a rate.
	f["claude-opus-5[1m]"] = f["claude-opus-5"]
	f["claude-haiku-4-5-20251001"] = struct{ in, out, read, write float64 }{in: 1, out: 5, read: 0.1, write: 1.25}
	return f
}

func closeTo(t *testing.T, label string, got, want, tolerance float64) {
	t.Helper()
	if math.Abs(got-want) > tolerance {
		t.Fatalf("%s = %v, want %v (+/- %v)", label, got, want, tolerance)
	}
}

func TestCanaryAccountingReproducesTheMeasuredSession(t *testing.T) {
	got := domain.BuildCompactionAccounting(domain.CompactionAccountingInput{
		SessionID:  "ao-canary-fixture-6",
		Attributed: canaryAttributed(),
		Harness:    canaryHarness(),
		Boundaries: canaryBoundaries(),
	}, opusOnly())

	if got.Compactions != 2 {
		t.Fatalf("compactions = %d, want 2", got.Compactions)
	}
	if got.PreTokensTotal != 153_510 || got.PostTokensTotal != 24_196 {
		t.Fatalf("pre/post totals = %d/%d, want 153510/24196", got.PreTokensTotal, got.PostTokensTotal)
	}
	if got.ReductionTokensTotal != 129_314 {
		t.Fatalf("reduction total = %d, want 129314", got.ReductionTokensTotal)
	}
	if got.DurationMsTotal != 182_466 {
		t.Fatalf("duration total = %d ms, want 182466", got.DurationMsTotal)
	}
	// The residual: what the harness charged itself minus what AO holds.
	// 16,953 of it is the opus session; the other 33 is the haiku the harness
	// used to title the conversation, which AO attributed nothing for at all.
	if got.Unattributed.OutputTokens != 16_986 {
		t.Fatalf("unattributed output = %d, want 16986", got.Unattributed.OutputTokens)
	}
	if got.Unattributed.CacheReadTokens != 363_625 {
		t.Fatalf("unattributed cache read = %d, want 363625", got.Unattributed.CacheReadTokens)
	}
	if got.UnattributedNegative {
		t.Fatalf("residual went negative on a session where the harness reports more than AO attributed")
	}
	// The haiku the harness used for its own title generation is a model AO
	// attributed nothing for at all, which is a different statement from a
	// shortfall and is reported as one.
	var haiku domain.UnattributedModelLine
	for _, line := range got.UnattributedByModel {
		if line.ModelID == "claude-haiku-4-5-20251001" {
			haiku = line
		}
	}
	if haiku.ModelID == "" || haiku.Matched {
		t.Fatalf("haiku line = %+v, want an unmatched model", haiku)
	}

	share, ok := got.UnattributedOutputShare()
	if !ok {
		t.Fatal("output share should be computable when the harness reported")
	}
	closeTo(t, "unattributed output share", share, 35.8, 0.2)

	costShare, ok := got.AttributedCostShare()
	if !ok {
		t.Fatal("cost share should be computable")
	}
	// AO's ledger accounts for roughly two thirds of what the harness says the
	// session cost. That is the defect P7.1 measures.
	closeTo(t, "attributed cost share", costShare, 67.8, 0.5)
}

func TestTheControlSessionShowsWhatTheResidualLooksLikeWithoutCompaction(t *testing.T) {
	got := domain.BuildCompactionAccounting(domain.CompactionAccountingInput{
		SessionID:  "medusa-12",
		Attributed: controlAttributed(),
		Harness:    controlHarness(),
	}, opusOnly())

	if got.Compactions != 0 {
		t.Fatalf("compactions = %d, want 0 on the control", got.Compactions)
	}
	if got.Unattributed.OutputTokens != 121 {
		t.Fatalf("unattributed output = %d, want 121", got.Unattributed.OutputTokens)
	}
	share, ok := got.UnattributedOutputShare()
	if !ok {
		t.Fatal("output share should be computable")
	}
	// 0.09% here against 35.7% on the canary. The OUTPUT half of the residual
	// is what separates a session that compacted from one that did not, and
	// that separation is the whole evidential basis of this checkpoint.
	closeTo(t, "control output share", share, 0.087, 0.01)

	costShare, ok := got.AttributedCostShare()
	if !ok {
		t.Fatal("cost share should be computable")
	}
	closeTo(t, "control cost share", costShare, 93.6, 0.5)
}

func TestAnUnpricedResidualModelKeepsItsTokensAndLosesOnlyItsCost(t *testing.T) {
	got := domain.BuildCompactionAccounting(domain.CompactionAccountingInput{
		SessionID:  "ao-canary-fixture-6",
		Attributed: canaryAttributed(),
		Harness:    canaryHarness(),
		Boundaries: canaryBoundaries(),
	}, opusOnly())

	if got.Unattributed.OutputTokens != 16_986 {
		t.Fatalf("tokens must survive an unpriced model, got %d", got.Unattributed.OutputTokens)
	}
	if got.UnattributedCost.Known {
		t.Fatal("claude-opus-5[1m] is not in this rate card; its residual must not be priced")
	}
	var named bool
	for _, model := range got.UnattributedCost.UnpricedModels {
		if model == "claude-opus-5[1m]" {
			named = true
		}
	}
	if !named {
		t.Fatalf("the unpriced model must be named, got %v", got.UnattributedCost.UnpricedModels)
	}
	if !got.AttributedCost.Known {
		t.Fatal("the attributed side priced fine and must stay priced")
	}
}

func TestARateCardThatCoversTheVariantPricesTheResidual(t *testing.T) {
	got := domain.BuildCompactionAccounting(domain.CompactionAccountingInput{
		SessionID:  "ao-canary-fixture-6",
		Attributed: canaryAttributed(),
		Harness:    canaryHarness(),
		Boundaries: canaryBoundaries(),
	}, opusAndLongContext())

	if !got.UnattributedCost.Known {
		t.Fatal("a rate card covering the variant must price the residual")
	}
	// Opus: 6,060 uncached + 363,625 read + 5,131 written + 16,953 generated,
	// which is $0.6680 -- and $0.4238 of that is the generated half alone.
	// Haiku adds the $0.0049 the harness itself reports for the title.
	closeTo(t, "unattributed cost", got.UnattributedCost.Amount, 0.6729, 0.001)
	closeTo(t, "attributed cost", got.AttributedCost.Amount, 2.3118, 0.001)
}

func TestNoHarnessRollupMeansNoResidualRatherThanAZeroOne(t *testing.T) {
	got := domain.BuildCompactionAccounting(domain.CompactionAccountingInput{
		SessionID:  "ao-canary-fixture-6",
		Attributed: canaryAttributed(),
		Boundaries: canaryBoundaries(),
	}, opusOnly())

	if got.HarnessObserved {
		t.Fatal("harness must not read as observed when nothing reported")
	}
	if len(got.UnattributedByModel) != 0 || got.Unattributed != (domain.UsageTokenTotals{}) {
		t.Fatalf("no rollup must produce no residual, got %+v", got.Unattributed)
	}
	if _, ok := got.UnattributedOutputShare(); ok {
		t.Fatal("an output share without a rollup is a number about nothing")
	}
	// The boundaries are still observations and still counted: AO saw the
	// conversation get replaced whether or not the harness ever rolled up.
	if got.Compactions != 2 {
		t.Fatalf("compactions = %d, want 2", got.Compactions)
	}
}

func TestAttributingMoreThanTheHarnessAdmitsIsFlaggedNotCredited(t *testing.T) {
	harness := canaryHarness()
	harness.Models = []domain.HarnessModelTotals{{
		ModelID: "claude-opus-5",
		Tokens:  domain.UsageTokenTotals{InputTokens: 10, OutputTokens: 10},
	}}
	got := domain.BuildCompactionAccounting(domain.CompactionAccountingInput{
		SessionID:  "s",
		Attributed: canaryAttributed(),
		Harness:    harness,
	}, opusOnly())

	if !got.UnattributedNegative {
		t.Fatal("a residual that would go negative must be flagged")
	}
	if got.Unattributed.OutputTokens != 0 || got.Unattributed.InputTokens != 0 {
		t.Fatalf("a negative residual floors at zero, got %+v", got.Unattributed)
	}
}

func TestCompactionTriggerIsAClosedVocabulary(t *testing.T) {
	for raw, want := range map[string]domain.CompactionTrigger{
		"manual":    domain.CompactionTriggerManual,
		" MANUAL ":  domain.CompactionTriggerManual,
		"auto":      domain.CompactionTriggerAuto,
		"automatic": domain.CompactionTriggerAuto,
		"":          domain.CompactionTriggerUnknown,
		// A harness word AO does not know must not travel as itself.
		"because the user said {{secret}}": domain.CompactionTriggerUnknown,
	} {
		if got := domain.NormalizeCompactionTrigger(raw); got != want {
			t.Fatalf("normalize(%q) = %q, want %q", raw, got, want)
		}
		if !domain.NormalizeCompactionTrigger(raw).Valid() {
			t.Fatalf("normalize(%q) produced an invalid trigger", raw)
		}
	}
	if domain.CompactionTrigger("something-else").Valid() {
		t.Fatal("an unlisted trigger must not validate")
	}
}

func TestABoundaryThatDidNotReduceAnythingReportsZeroNotANegative(t *testing.T) {
	grew := domain.CompactionBoundary{PreTokens: 100, PostTokens: 140}
	if got := grew.ReductionTokens(); got != 0 {
		t.Fatalf("reduction = %d, want 0", got)
	}
	if pct, ok := grew.ReductionPercent(); !ok || pct != 0 {
		t.Fatalf("reduction percent = %v/%v, want 0/true", pct, ok)
	}
	unknown := domain.CompactionBoundary{}
	if _, ok := unknown.ReductionPercent(); ok {
		t.Fatal("a boundary with no pre-tokens has no percentage, not a 0% one")
	}
}

func TestModelVariantsAreMatchedForTheResidualAndNeverForThePrice(t *testing.T) {
	// The harness rolls up as claude-opus-5[1m]; AO attributed claude-opus-5.
	// The two must be recognised as the same model when computing the gap --
	// otherwise the whole attributed total reads as unattributed -- and must
	// NOT be recognised as the same model when looking up a rate.
	got := domain.BuildCompactionAccounting(domain.CompactionAccountingInput{
		SessionID:  "s",
		Attributed: canaryAttributed(),
		Harness:    canaryHarness(),
	}, opusOnly())

	var opus domain.UnattributedModelLine
	for _, line := range got.UnattributedByModel {
		if line.ModelID == "claude-opus-5[1m]" {
			opus = line
		}
	}
	if !opus.Matched {
		t.Fatal("claude-opus-5[1m] must match the attributed claude-opus-5")
	}
	if opus.Tokens.CacheReadTokens != 363_625 {
		t.Fatalf("matched residual = %d, want the gap and not the whole rollup", opus.Tokens.CacheReadTokens)
	}
	if opus.Cost.Known {
		t.Fatal("matching for the gap must not become matching for the rate")
	}
}

// TestAttributedTokensAreCreditedExactlyOnce is the regression for the defect
// the P7.1 integration review found.
//
// Two harness buckets can canonicalise to the same model -- a session that ran
// "claude-opus-5" and rolled part of itself up as "claude-opus-5[1m]" is
// exactly that shape. Crediting the attributed tokens against both of them
// subtracted them twice and made the residual SMALLER than it is, which is the
// one direction of error this whole file exists to remove: it under-reports the
// spend AO cannot see, on the feature whose entire purpose is to stop
// under-reporting it.
func TestAttributedTokensAreCreditedExactlyOnce(t *testing.T) {
	got := domain.BuildCompactionAccounting(domain.CompactionAccountingInput{
		SessionID: "s",
		Attributed: []domain.ModelUsageLine{{
			ModelID: "claude-opus-5",
			Tokens:  domain.UsageTokenTotals{InputTokens: 500, OutputTokens: 50, EventCount: 3},
		}},
		Harness: domain.HarnessSessionTotals{Observed: true, Models: []domain.HarnessModelTotals{
			{ModelID: "claude-opus-5", Tokens: domain.UsageTokenTotals{InputTokens: 1000, OutputTokens: 100}},
			{ModelID: "claude-opus-5[1m]", Tokens: domain.UsageTokenTotals{InputTokens: 2000, OutputTokens: 200}},
		}},
	}, nil)

	// The harness reported 3,000 in and 300 out; AO holds 500 and 50.
	if got.Unattributed.InputTokens != 2500 || got.Unattributed.OutputTokens != 250 {
		t.Fatalf("residual = %d/%d, want 2500/250 -- the credit was applied more than once",
			got.Unattributed.InputTokens, got.Unattributed.OutputTokens)
	}
	// The first bucket absorbs the credit; the second gets none of it.
	if got.UnattributedByModel[0].Tokens.InputTokens != 500 {
		t.Fatalf("first bucket = %d, want 1000-500", got.UnattributedByModel[0].Tokens.InputTokens)
	}
	if got.UnattributedByModel[1].Tokens.InputTokens != 2000 {
		t.Fatalf("second bucket = %d, want its whole total", got.UnattributedByModel[1].Tokens.InputTokens)
	}
	// Everything AO attributed was absorbed, so nothing is flagged.
	if got.UnattributedNegative {
		t.Fatal("all attributed tokens were absorbed; nothing should be flagged")
	}
}

// TestAModelTheRollupNeverMentionsIsFlagged pins the other half of the same
// bookkeeping: credit no bucket could absorb means AO holds events the harness
// does not admit to. It is a statement about the pipeline, never a discount.
func TestAModelTheRollupNeverMentionsIsFlagged(t *testing.T) {
	got := domain.BuildCompactionAccounting(domain.CompactionAccountingInput{
		SessionID: "s",
		Attributed: []domain.ModelUsageLine{
			{ModelID: "claude-opus-5", Tokens: domain.UsageTokenTotals{InputTokens: 100}},
			{ModelID: "claude-sonnet-5", Tokens: domain.UsageTokenTotals{InputTokens: 40}},
		},
		Harness: domain.HarnessSessionTotals{Observed: true, Models: []domain.HarnessModelTotals{
			{ModelID: "claude-opus-5", Tokens: domain.UsageTokenTotals{InputTokens: 300}},
		}},
	}, nil)
	if !got.UnattributedNegative {
		t.Fatal("an attributed model absent from the rollup must be flagged")
	}
	if got.Unattributed.InputTokens != 200 {
		t.Fatalf("residual = %d, want 300-100 with sonnet's 40 not netted off anything",
			got.Unattributed.InputTokens)
	}
}
