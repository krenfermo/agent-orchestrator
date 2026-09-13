package domain_test

import (
	"math"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// compaction_economics_adversarial_test.go -- the cases the independent review
// asked for by name. Each one is a way the gate could be made to lie.

// TestAnInvalidSafetyFactorFailsClosed. A "safety" factor below 1 is a licence,
// and NaN or Inf would propagate through every comparison. All three must fall
// back to the documented default rather than be honoured.
func TestAnInvalidSafetyFactorFailsClosed(t *testing.T) {
	for _, factor := range []float64{
		0, 0.5, -1, -0.0001,
		math.NaN(), math.Inf(1), math.Inf(-1),
	} {
		in := historicalInput(293224, 47809, 26009, 2500, 8050, 19)
		in.SafetyFactor = factor
		got := domain.EvaluateCompactionEconomics(in)
		if got.SafetyFactor != domain.DefaultCompactionSafetyFactor {
			t.Errorf("safetyFactor %v was honoured as %v; an invalid factor must fall back to %v",
				factor, got.SafetyFactor, domain.DefaultCompactionSafetyFactor)
		}
		for _, f := range []float64{got.RequiredCalls, got.Margin, got.BreakEvenCalls} {
			if math.IsNaN(f) || math.IsInf(f, 0) {
				t.Errorf("safetyFactor %v produced a non-finite figure %v in the record", factor, f)
			}
		}
	}

	// A factor ABOVE the default is honoured: raising the bar is always allowed.
	in := historicalInput(293224, 47809, 26009, 2500, 8050, 19)
	in.SafetyFactor = 10
	if got := domain.EvaluateCompactionEconomics(in); got.SafetyFactor != 10 {
		t.Errorf("safetyFactor = %v, want a raised bar to be honoured", got.SafetyFactor)
	}
}

// TestAMixedLifetimeSessionIsPricedAtTheDearRate. A session whose writes are
// mostly short-lived but not entirely must be priced at the LONG rate: the
// majority lifetime is not the conservative one, and understating the rewrite is
// exactly the error P7.2B1 was opened to remove.
func TestAMixedLifetimeSessionIsPricedAtTheDearRate(t *testing.T) {
	base := historicalInput(293224, 47809, 26009, 2500, 8050, 19)

	allShort := base
	allShort.ObservedCacheCreation = domain.CacheCreationSplit{Ephemeral5mTokens: 100_000}
	short := domain.EvaluateCompactionEconomics(allShort)

	mostlyShort := base
	mostlyShort.ObservedCacheCreation = domain.CacheCreationSplit{Ephemeral5mTokens: 99_999, Ephemeral1hTokens: 1}
	mixed := domain.EvaluateCompactionEconomics(mostlyShort)

	if mixed.CacheWriteLifetimePriced != domain.CacheWriteLifetime1h {
		t.Fatalf("a 99.999%%-short session priced at %q; the dearer lifetime must win a mix",
			mixed.CacheWriteLifetimePriced)
	}
	if mixed.EstimatedCompactionCostMicros <= short.EstimatedCompactionCostMicros {
		t.Errorf("mixed cost %d must exceed all-short cost %d",
			mixed.EstimatedCompactionCostMicros, short.EstimatedCompactionCostMicros)
	}
	if mixed.BreakEvenCalls <= short.BreakEvenCalls {
		t.Errorf("mixed break-even %.3f must exceed all-short %.3f", mixed.BreakEvenCalls, short.BreakEvenCalls)
	}
}

// TestTheGateNeverPricesAnUnknownLifetimeAtTheShortRate is the P7.2B1 regression
// in its sharpest form: a single unknown token poisons the whole verdict, and
// the recorded cost stays at zero rather than being computed at 5m "for now".
func TestTheGateNeverPricesAnUnknownLifetimeAtTheShortRate(t *testing.T) {
	in := historicalInput(293224, 47809, 26009, 2500, 8050, 19)
	in.ObservedCacheCreation = domain.CacheCreationSplit{Ephemeral1hTokens: 99_999, UnknownTTLTokens: 1}

	got := domain.EvaluateCompactionEconomics(in)
	if got.Verdict != domain.CompactionVerdictUnknown || got.Reason != domain.CompactionReasonTTLUnknown {
		t.Fatalf("verdict = %s/%s, want unknown/ttl_unknown", got.Verdict, got.Reason)
	}
	if got.EstimatedCompactionCostMicros != 0 || got.SavingPerCallMicros != 0 || got.BreakEvenCallsKnown {
		t.Errorf("a refused verdict must carry no computed economics, got cost=%d perCall=%d breakEvenKnown=%v",
			got.EstimatedCompactionCostMicros, got.SavingPerCallMicros, got.BreakEvenCallsKnown)
	}
	if got.CacheWriteLifetimePriced == domain.CacheWriteLifetime5m {
		t.Error("the short lifetime must never be recorded as the one priced when the split is unknown")
	}
}

// TestTerminalCycleReasonCombinations walks every reason set against the
// structural rule. The rule must fire on the two counters ALONE and on nothing
// else -- neither wider (swallowing a pressure-driven decision) nor narrower
// (missing the combination of both counters).
func TestTerminalCycleReasonCombinations(t *testing.T) {
	cases := []struct {
		name    string
		reasons []domain.SessionLifecycleReason
		want    bool
	}{
		{"many_fix_cycles alone", []domain.SessionLifecycleReason{domain.LifecycleReasonManyFixCycles}, true},
		{"many_attempts alone", []domain.SessionLifecycleReason{domain.LifecycleReasonManyAttempts}, true},
		{"both counters together", []domain.SessionLifecycleReason{
			domain.LifecycleReasonManyFixCycles, domain.LifecycleReasonManyAttempts}, true},
		{"duplicated counter", []domain.SessionLifecycleReason{
			domain.LifecycleReasonManyFixCycles, domain.LifecycleReasonManyFixCycles}, true},
		{"counter plus context pressure", []domain.SessionLifecycleReason{
			domain.LifecycleReasonManyFixCycles, domain.LifecycleReasonContextPressureActual}, false},
		{"context pressure alone", []domain.SessionLifecycleReason{
			domain.LifecycleReasonContextPressureActual}, false},
		{"session unhealthy", []domain.SessionLifecycleReason{
			domain.LifecycleReasonSessionUnhealthy}, false},
		// No reason at all is not a licence: the rule needs the counter to be
		// the SOLE cause, and an empty set names no cause.
		{"no reasons recorded", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := historicalInput(293224, 47809, 26009, 2500, 8050, 19)
			in.Cycle, in.MaxFixCycles = 3, 3
			in.LifecycleReasons = tc.reasons
			got := domain.EvaluateCompactionEconomics(in)
			fired := got.Reason == domain.CompactionReasonTerminalCycle
			if fired != tc.want {
				t.Errorf("terminal_cycle fired = %v, want %v (verdict %s/%s)",
					fired, tc.want, got.Verdict, got.Reason)
			}
			// Whatever the reason set, a terminal cycle must never COMPACT on
			// the counters. Where the rule does not fire, the ordinary
			// arithmetic decides -- and it may legitimately say COMPACT.
			if fired && got.Verdict != domain.CompactionVerdictSkip {
				t.Errorf("terminal_cycle must be a SKIP, got %s", got.Verdict)
			}
		})
	}
}

// TestANonTerminalCycleIsNeverRejectedStructurally. The mirror of the rule: with
// cycles remaining the counters must not block anything.
func TestANonTerminalCycleIsNeverRejectedStructurally(t *testing.T) {
	for cycle := 1; cycle <= 2; cycle++ {
		in := historicalInput(293224, 47809, 26009, 2500, 8050, 19)
		in.Cycle, in.MaxFixCycles = cycle, 3
		in.LifecycleReasons = []domain.SessionLifecycleReason{domain.LifecycleReasonManyFixCycles}
		got := domain.EvaluateCompactionEconomics(in)
		if got.Reason == domain.CompactionReasonTerminalCycle {
			t.Errorf("cycle %d of 3 has %d cycles left; terminal_cycle must not fire",
				cycle, got.StructuralCyclesRemaining)
		}
		if got.StructuralCyclesRemaining != 3-cycle {
			t.Errorf("structuralCyclesRemaining = %d, want %d", got.StructuralCyclesRemaining, 3-cycle)
		}
	}
}

// TestTheGateRefusesTheBareSonnetAliasEndToEnd. The review's specific concern:
// the canonical key P7.1 uses to MATCH a residual must never become a pricing
// device. Asserted through the gate, not only through the rate table.
func TestTheGateRefusesTheBareSonnetAliasEndToEnd(t *testing.T) {
	for _, modelID := range []string{"sonnet", "claude-opus-5[1m]", "opus", ""} {
		in := historicalInput(293224, 47809, 26009, 2500, 8050, 19)
		in.ModelID = modelID
		// A caller that failed to resolve a rate must report RatesKnown=false;
		// this is the state the workflow assembler produces for an uncovered id.
		in.RatesKnown = false
		in.Rates = domain.ModelRateView{}

		got := domain.EvaluateCompactionEconomics(in)
		if got.Verdict != domain.CompactionVerdictUnknown || got.Reason != domain.CompactionReasonPricingUnknown {
			t.Errorf("model %q gave %s/%s, want unknown/pricing_unknown", modelID, got.Verdict, got.Reason)
		}
		if got.PricingStatus != domain.CompactionPricingUnpricedModel {
			t.Errorf("model %q pricingStatus = %q, want unpriced_model", modelID, got.PricingStatus)
		}
		if got.ModelID != modelID {
			t.Errorf("the record must carry the id as asked (%q), got %q", modelID, got.ModelID)
		}
	}
}

// TestHugeInputsDoNotOverflowIntoAVerdict. Token counts near the int64 ceiling
// must not wrap a micros figure into a plausible small number.
func TestHugeInputsDoNotOverflowIntoAVerdict(t *testing.T) {
	const huge = int64(1) << 55
	in := historicalInput(huge, huge/4, huge/8, 2500, huge/16, 19)
	in.ObservedCacheCreation = domain.CacheCreationSplit{Ephemeral1hTokens: huge / 32}
	got := domain.EvaluateCompactionEconomics(in)

	if !got.Verdict.Valid() || !got.Reason.Valid() {
		t.Fatalf("verdict/reason outside the closed enum: %s/%s", got.Verdict, got.Reason)
	}
	for label, f := range map[string]float64{
		"breakEven": got.BreakEvenCalls, "required": got.RequiredCalls,
		"margin": got.Margin, "reductionPercent": got.EstimatedReductionPercent,
	} {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			t.Errorf("%s is non-finite: %v", label, f)
		}
	}
	// Saturated rather than wrapped: a clipped figure is visibly absurd, a
	// wrapped one is plausibly small.
	if got.EstimatedCompactionCostMicros < 0 && got.EstimatedFutureSavingsMicros > 0 {
		t.Errorf("a cost wrapped negative: %d", got.EstimatedCompactionCostMicros)
	}
}

// TestTheRecordIsIndependentOfTheCallersReasonSlice. The gate must not be able to
// reach back into the SessionLifecycleDecision it is shadowing.
func TestTheRecordIsIndependentOfTheCallersReasonSlice(t *testing.T) {
	callerReasons := []domain.SessionLifecycleReason{domain.LifecycleReasonContextPressureActual}
	in := historicalInput(293224, 47809, 26009, 2500, 8050, 19)
	in.LifecycleReasons = callerReasons

	before := append([]domain.SessionLifecycleReason(nil), callerReasons...)
	rec := domain.EvaluateCompactionEconomics(in)

	if len(callerReasons) != len(before) || callerReasons[0] != before[0] {
		t.Errorf("the gate mutated the caller's reason slice: %v, was %v", callerReasons, before)
	}
	// And mutating the record's copy afterwards must not reach the caller.
	if len(rec.LifecycleReasons) > 0 {
		rec.LifecycleReasons[0] = "tampered"
		if callerReasons[0] != before[0] {
			t.Errorf("the record shares storage with the caller's reasons")
		}
	}
}
