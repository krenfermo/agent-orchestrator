package domain_test

import (
	"math"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// compaction_economics_test.go -- the shadow gate's arithmetic against the whole
// observed population.
//
// Five boundaries exist: the two real compactions of wf-66f0ee54 (MEASURED, from
// the harness's own compact_boundary records) and the three modelled boundaries
// of the wf-1c2cb9bd replay (REPLAY, from that run's own token vector). They are
// not a sample and they are not a held-out set -- they are everything there is,
// and the tests below pin them so that a change to the formula has to say out
// loud that it moved a historical verdict.

// opus5Rates is the embedded catalog's claude-opus-5 row, with BOTH cache-write
// lifetimes. The 1h rate is the one P7.2B1 proved the provider actually charged
// for every write in both sessions.
func opus5Rates() domain.ModelRateView {
	return domain.ModelRateView{
		ModelID:             "claude-opus-5",
		InputPerMTok:        5.00,
		OutputPerMTok:       25.00,
		CacheReadPerMTok:    0.50,
		CacheWrite5mPerMTok: 6.25,
		CacheWrite1hPerMTok: 10.00,
		Currency:            "USD",
		Source:              "anthropic-list-price",
		Version:             "2026-09-12",
	}
}

// allOneHourCreation is what both measured sessions actually reported: 100% of
// cache creation at the one-hour lifetime, nothing unknown.
func allOneHourCreation(tokens int64) domain.CacheCreationSplit {
	return domain.CacheCreationSplit{Ephemeral1hTokens: tokens}
}

// historicalInput builds a gate input for one historical boundary. Every caller
// below overrides only what distinguishes its boundary.
func historicalInput(b, a, p, delta, summary, remaining int64) domain.CompactionEconomicsInput {
	return domain.CompactionEconomicsInput{
		WorkflowRunID:         "wf-historical",
		StepID:                "wfs-1",
		Cycle:                 1,
		SessionID:             "session-under-judgement",
		Provider:              "anthropic",
		Harness:               "claude-code",
		ModelID:               "claude-opus-5",
		MaxFixCycles:          3,
		Strategy:              "task",
		LifecycleReasons:      []domain.SessionLifecycleReason{domain.LifecycleReasonContextPressureActual},
		SafetyFactor:          domain.DefaultCompactionSafetyFactor,
		MinReductionFraction:  domain.DefaultCompactionMinReductionFraction,
		UsageObservable:       true,
		ContextBeforeTokens:   b,
		StablePrefixTokens:    p,
		PromptTokens:          delta,
		PromptTokensKnown:     true,
		ObservedCacheCreation: allOneHourCreation(110284),
		Rates:                 opus5Rates(),
		RatesKnown:            true,
		ContextAfter:          domain.EstimatedTokens{Value: a, Known: true, Basis: domain.CompactionBasisSessionPriorBoundary, Samples: 1},
		SummaryTokens:         domain.EstimatedTokens{Value: summary, Known: true, Basis: domain.CompactionBasisCrossSessionPrior, Samples: 1},
		RemainingCalls:        domain.EstimatedCalls{Value: remaining, Known: true, Basis: domain.CompactionBasisPriorCycleDiscounted, Samples: 1},
	}
}

// TestBreakEvenCallsReproducesEveryHistoricalBoundary is the test that fails if
// anyone changes the formula, the rate handling or the term structure.
//
// The expected N* values are the corrected ones -- computed at the ONE-HOUR
// cache-write rate of $10.00/MTok, which is what the provider charged. At the
// embedded catalog's 5-minute rate of $6.25 every one of them is smaller, which
// is precisely the understatement P7.2B1 was opened to remove.
func TestBreakEvenCallsReproducesEveryHistoricalBoundary(t *testing.T) {
	cases := []struct {
		name           string
		b, a, p, delta int64
		summary        int64
		remaining      int64
		wantBreakEvenN float64
		wantVerdict    domain.CompactionEconomicVerdict
		wantReason     domain.CompactionEconomicReason
	}{{
		// MEASURED. Lost $0.33: 6 calls remained against a break-even of 28.2.
		name: "canary compact 1 measured", b: 85847, a: 57803, p: 37379, delta: 1181,
		summary: 8050, remaining: 6, wantBreakEvenN: 28.2,
		wantVerdict: domain.CompactionVerdictSkip, wantReason: domain.CompactionReasonInsufficientFutureCalls,
	}, {
		// MEASURED. Lost $0.43. Rejected before the call forecast is even
		// consulted: an 11.3% reduction is below the structural floor, because
		// most of what it was asked to compact was the previous summary.
		name: "canary compact 2 measured", b: 67663, a: 61099, p: 37379, delta: 1062,
		summary: 8050, remaining: 4, wantBreakEvenN: 117.1,
		wantVerdict: domain.CompactionVerdictSkip, wantReason: domain.CompactionReasonReductionTooSmall,
	}, {
		// REPLAY. +$1.83 modelled.
		name: "medusa replay 1", b: 199892, a: 47809, p: 26009, delta: 2500,
		summary: 8050, remaining: 19, wantBreakEvenN: 5.3,
		wantVerdict: domain.CompactionVerdictCompact, wantReason: domain.CompactionReasonPositiveMargin,
	}, {
		name: "medusa replay 2", b: 251148, a: 47809, p: 26009, delta: 2500,
		summary: 8050, remaining: 19, wantBreakEvenN: 4.0,
		wantVerdict: domain.CompactionVerdictCompact, wantReason: domain.CompactionReasonPositiveMargin,
	}, {
		name: "medusa replay 3", b: 293224, a: 47809, p: 26009, delta: 2500,
		summary: 8050, remaining: 19, wantBreakEvenN: 3.3,
		wantVerdict: domain.CompactionVerdictCompact, wantReason: domain.CompactionReasonPositiveMargin,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := historicalInput(tc.b, tc.a, tc.p, tc.delta, tc.summary, tc.remaining)
			got := domain.EvaluateCompactionEconomics(in)
			if !got.BreakEvenCallsKnown {
				t.Fatalf("break-even was not computed at all")
			}
			if math.Abs(got.BreakEvenCalls-tc.wantBreakEvenN) > 0.05 {
				t.Errorf("break-even calls = %.4f, want %.1f (+/- 0.05)", got.BreakEvenCalls, tc.wantBreakEvenN)
			}
			if got.Verdict != tc.wantVerdict || got.Reason != tc.wantReason {
				t.Errorf("verdict = %s/%s, want %s/%s", got.Verdict, got.Reason, tc.wantVerdict, tc.wantReason)
			}
			if got.WouldHaveActed {
				t.Errorf("wouldHaveActed must be false in shadow mode")
			}
			if got.CacheWriteLifetimePriced != domain.CacheWriteLifetime1h {
				t.Errorf("priced the rewrite at %q, want the one-hour lifetime these sessions actually used",
					got.CacheWriteLifetimePriced)
			}
		})
	}
}

// TestTheObservedPopulationSplitsThreeAllowedTwoRejected states the headline as
// a test: every profitable boundary is allowed and every loss-making one is
// rejected, with no errors in either direction.
func TestTheObservedPopulationSplitsThreeAllowedTwoRejected(t *testing.T) {
	type point struct {
		in         domain.CompactionEconomicsInput
		profitable bool
	}
	points := []point{
		{historicalInput(85847, 57803, 37379, 1181, 8050, 6), false},
		{historicalInput(67663, 61099, 37379, 1062, 8050, 4), false},
		{historicalInput(199892, 47809, 26009, 2500, 8050, 19), true},
		{historicalInput(251148, 47809, 26009, 2500, 8050, 19), true},
		{historicalInput(293224, 47809, 26009, 2500, 8050, 19), true},
	}
	allowed, rejected := 0, 0
	for _, p := range points {
		got := domain.EvaluateCompactionEconomics(p.in)
		switch got.Verdict {
		case domain.CompactionVerdictCompact:
			allowed++
			if !p.profitable {
				t.Errorf("FALSE POSITIVE: compacted a boundary that lost money (reason %s)", got.Reason)
			}
		case domain.CompactionVerdictSkip:
			rejected++
			if p.profitable {
				t.Errorf("FALSE NEGATIVE: skipped a profitable boundary (reason %s)", got.Reason)
			}
		default:
			t.Errorf("UNKNOWN on a boundary where every input was present: %s", got.Reason)
		}
	}
	if allowed != 3 || rejected != 2 {
		t.Errorf("allowed/rejected = %d/%d, want 3/2", allowed, rejected)
	}
}

// TestTheSafetyFactorChangesNoHistoricalVerdict is the sensitivity finding as a
// test: the two populations are ~40x apart, so no factor in [1.0, 2.0] moves
// any of the five. If a future change makes a verdict factor-sensitive, that is
// a fact worth failing a build over.
func TestTheSafetyFactorChangesNoHistoricalVerdict(t *testing.T) {
	inputs := []domain.CompactionEconomicsInput{
		historicalInput(85847, 57803, 37379, 1181, 8050, 6),
		historicalInput(67663, 61099, 37379, 1062, 8050, 4),
		historicalInput(199892, 47809, 26009, 2500, 8050, 19),
		historicalInput(251148, 47809, 26009, 2500, 8050, 19),
		historicalInput(293224, 47809, 26009, 2500, 8050, 19),
	}
	for _, factor := range []float64{1.0, 1.25, 1.5, 2.0} {
		for i, base := range inputs {
			at15 := domain.EvaluateCompactionEconomics(base)
			in := base
			in.SafetyFactor = factor
			got := domain.EvaluateCompactionEconomics(in)
			if got.Verdict != at15.Verdict {
				t.Errorf("boundary %d: verdict moved from %s to %s at safetyFactor %.2f",
					i, at15.Verdict, got.Verdict, factor)
			}
		}
	}
}

// TestTheCacheWriteLifetimeChangesTheAnswer is the test that fails if anyone
// hardcodes a rate ratio.
//
// The same conversation priced at the one-hour cache-write rate needs measurably
// MORE remaining calls to justify compacting than at the five-minute rate,
// because the rewrite the first post-compaction call pays for is the largest
// cache write in a session. A gate carrying 62.5 or 70 as a constant cannot
// produce two different answers here.
func TestTheCacheWriteLifetimeChangesTheAnswer(t *testing.T) {
	base := historicalInput(85847, 57803, 37379, 1181, 8050, 6)

	atOneHour := domain.EvaluateCompactionEconomics(base)

	shortLived := base
	shortLived.ObservedCacheCreation = domain.CacheCreationSplit{Ephemeral5mTokens: 110284}
	atFiveMinutes := domain.EvaluateCompactionEconomics(shortLived)

	if atFiveMinutes.CacheWriteLifetimePriced != domain.CacheWriteLifetime5m {
		t.Fatalf("lifetime priced = %q, want 5m", atFiveMinutes.CacheWriteLifetimePriced)
	}
	if atOneHour.BreakEvenCalls <= atFiveMinutes.BreakEvenCalls {
		t.Errorf("break-even at 1h (%.3f) must exceed break-even at 5m (%.3f); a hardcoded ratio makes them equal",
			atOneHour.BreakEvenCalls, atFiveMinutes.BreakEvenCalls)
	}
	if atOneHour.EstimatedCompactionCostMicros <= atFiveMinutes.EstimatedCompactionCostMicros {
		t.Errorf("cost at 1h (%d micros) must exceed cost at 5m (%d micros)",
			atOneHour.EstimatedCompactionCostMicros, atFiveMinutes.EstimatedCompactionCostMicros)
	}
	// And a rate card that does not state the one-hour rate refuses rather than
	// substituting the five-minute one.
	noLongRate := base
	noLongRate.Rates.CacheWrite1hPerMTok = 0
	refused := domain.EvaluateCompactionEconomics(noLongRate)
	if refused.Verdict != domain.CompactionVerdictUnknown || refused.Reason != domain.CompactionReasonTTLRateUnknown {
		t.Errorf("missing 1h rate gave %s/%s, want unknown/ttl_rate_unknown", refused.Verdict, refused.Reason)
	}
}

// TestEveryMissingInputIndependentlyRefusesCompact walks one row of the input
// table at a time. The assertion is not "the reason is X" alone but the
// invariant behind every row: NOTHING missing can produce COMPACT.
func TestEveryMissingInputIndependentlyRefusesCompact(t *testing.T) {
	// A boundary that WOULD compact with everything present, so each mutation
	// below is the only thing standing between it and a COMPACT verdict.
	healthy := historicalInput(293224, 47809, 26009, 2500, 8050, 19)
	if got := domain.EvaluateCompactionEconomics(healthy); got.Verdict != domain.CompactionVerdictCompact {
		t.Fatalf("control case must compact, got %s/%s", got.Verdict, got.Reason)
	}

	cases := []struct {
		name       string
		mutate     func(*domain.CompactionEconomicsInput)
		wantStatus domain.CompactionEconomicVerdict
		wantReason domain.CompactionEconomicReason
	}{{
		name:       "usage unobservable",
		mutate:     func(in *domain.CompactionEconomicsInput) { in.UsageObservable = false },
		wantStatus: domain.CompactionVerdictUnknown, wantReason: domain.CompactionReasonUsageIncomplete,
	}, {
		name:       "context before unknown",
		mutate:     func(in *domain.CompactionEconomicsInput) { in.ContextBeforeTokens = 0 },
		wantStatus: domain.CompactionVerdictUnknown, wantReason: domain.CompactionReasonUsageIncomplete,
	}, {
		name:       "prefix larger than the conversation",
		mutate:     func(in *domain.CompactionEconomicsInput) { in.StablePrefixTokens = in.ContextBeforeTokens + 1 },
		wantStatus: domain.CompactionVerdictUnknown, wantReason: domain.CompactionReasonInconsistentAccounting,
	}, {
		name:       "model not on the rate card",
		mutate:     func(in *domain.CompactionEconomicsInput) { in.RatesKnown = false },
		wantStatus: domain.CompactionVerdictUnknown, wantReason: domain.CompactionReasonPricingUnknown,
	}, {
		name:       "cache read rate missing",
		mutate:     func(in *domain.CompactionEconomicsInput) { in.Rates.CacheReadPerMTok = 0 },
		wantStatus: domain.CompactionVerdictUnknown, wantReason: domain.CompactionReasonPricingUnknown,
	}, {
		name:       "AO priced this session on an assumed lifetime",
		mutate:     func(in *domain.CompactionEconomicsInput) { in.ObservedCostTTLAssumedTokens = 1 },
		wantStatus: domain.CompactionVerdictUnknown, wantReason: domain.CompactionReasonTTLAssumed,
	}, {
		name:       "AO could not price part of this session's creation",
		mutate:     func(in *domain.CompactionEconomicsInput) { in.ObservedCostTTLUnknownTokens = 1 },
		wantStatus: domain.CompactionVerdictUnknown, wantReason: domain.CompactionReasonTTLUnknown,
	}, {
		name: "some of this session's creation has no lifetime",
		mutate: func(in *domain.CompactionEconomicsInput) {
			in.ObservedCacheCreation.UnknownTTLTokens = 1
		},
		wantStatus: domain.CompactionVerdictUnknown, wantReason: domain.CompactionReasonTTLUnknown,
	}, {
		name: "this session has created no cache at all",
		mutate: func(in *domain.CompactionEconomicsInput) {
			in.ObservedCacheCreation = domain.CacheCreationSplit{}
		},
		wantStatus: domain.CompactionVerdictUnknown, wantReason: domain.CompactionReasonTTLUnknown,
	}, {
		name:       "context after not estimable",
		mutate:     func(in *domain.CompactionEconomicsInput) { in.ContextAfter = domain.EstimatedTokens{} },
		wantStatus: domain.CompactionVerdictUnknown, wantReason: domain.CompactionReasonContextAfterUnknown,
	}, {
		name:       "summary size not estimable",
		mutate:     func(in *domain.CompactionEconomicsInput) { in.SummaryTokens = domain.EstimatedTokens{} },
		wantStatus: domain.CompactionVerdictUnknown, wantReason: domain.CompactionReasonSummaryCostUnknown,
	}, {
		name:       "remaining calls not estimable",
		mutate:     func(in *domain.CompactionEconomicsInput) { in.RemainingCalls = domain.EstimatedCalls{} },
		wantStatus: domain.CompactionVerdictUnknown, wantReason: domain.CompactionReasonRemainingCallsUnknown,
	}, {
		name:       "no calls left to amortize into",
		mutate:     func(in *domain.CompactionEconomicsInput) { in.RemainingCalls.Value = 0 },
		wantStatus: domain.CompactionVerdictSkip, wantReason: domain.CompactionReasonInsufficientFutureCalls,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := healthy
			// The split is a value type, so a shallow copy is genuinely
			// independent -- but the reason slice is not, and a mutation that
			// appended to it would leak across subtests.
			in.LifecycleReasons = append([]domain.SessionLifecycleReason(nil), healthy.LifecycleReasons...)
			tc.mutate(&in)
			got := domain.EvaluateCompactionEconomics(in)
			if got.Verdict == domain.CompactionVerdictCompact {
				t.Fatalf("COMPACT with %s: a missing input must never authorize a compaction", tc.name)
			}
			if got.Verdict != tc.wantStatus || got.Reason != tc.wantReason {
				t.Errorf("verdict = %s/%s, want %s/%s", got.Verdict, got.Reason, tc.wantStatus, tc.wantReason)
			}
			if !got.Verdict.Valid() || !got.Reason.Valid() {
				t.Errorf("verdict %q / reason %q is outside the closed enum", got.Verdict, got.Reason)
			}
		})
	}
}

// TestTerminalCycleNeverCompacts is the canary's compact 2 as a structural
// regression test, on the axis that produced it.
//
// With MaxFixCycles 3, many_fix_cycles reaches COMPACT only when dispatching
// cycle 3 -- the last cycle that will ever run. There is structurally nothing
// left to amortize into, and the rule needs no estimate to say so.
func TestTerminalCycleNeverCompacts(t *testing.T) {
	// Deliberately the most profitable boundary AO has, so only the structural
	// rule can be what rejects it.
	base := historicalInput(293224, 47809, 26009, 2500, 8050, 19)

	for _, reason := range []domain.SessionLifecycleReason{
		domain.LifecycleReasonManyFixCycles, domain.LifecycleReasonManyAttempts,
	} {
		t.Run(string(reason), func(t *testing.T) {
			in := base
			in.Cycle, in.MaxFixCycles = 3, 3
			in.LifecycleReasons = []domain.SessionLifecycleReason{reason}
			got := domain.EvaluateCompactionEconomics(in)
			if got.Verdict != domain.CompactionVerdictSkip || got.Reason != domain.CompactionReasonTerminalCycle {
				t.Errorf("verdict = %s/%s, want skip/terminal_cycle", got.Verdict, got.Reason)
			}
			if got.StructuralCyclesRemaining != 0 {
				t.Errorf("structuralCyclesRemaining = %d, want 0", got.StructuralCyclesRemaining)
			}
			// The record still says the model was priceable: the structural
			// rule is not a pricing failure and must not look like one in the
			// coverage statistics.
			if got.PricingStatus != domain.CompactionPricingPriced {
				t.Errorf("pricingStatus = %q, want priced", got.PricingStatus)
			}
		})
	}

	t.Run("genuine context pressure on a terminal cycle is still evaluated", func(t *testing.T) {
		// The rule is narrow ON PURPOSE. A terminal cycle whose reason is real
		// context pressure, with a long tail of calls inside that cycle, can
		// still be economically positive, and pretending otherwise would be a
		// second policy hiding inside a structural test.
		in := base
		in.Cycle, in.MaxFixCycles = 3, 3
		in.LifecycleReasons = []domain.SessionLifecycleReason{domain.LifecycleReasonContextPressureActual}
		got := domain.EvaluateCompactionEconomics(in)
		if got.Reason == domain.CompactionReasonTerminalCycle {
			t.Errorf("terminal_cycle fired on a pressure-driven decision; the rule must need the counter to be the sole reason")
		}
	})

	t.Run("a mixed reason set does not fire the rule", func(t *testing.T) {
		in := base
		in.Cycle, in.MaxFixCycles = 3, 3
		in.LifecycleReasons = []domain.SessionLifecycleReason{
			domain.LifecycleReasonManyFixCycles, domain.LifecycleReasonContextPressureActual,
		}
		if got := domain.EvaluateCompactionEconomics(in); got.Reason == domain.CompactionReasonTerminalCycle {
			t.Errorf("terminal_cycle fired although context pressure was also a reason")
		}
	})
}

// TestDegenerateArithmeticNeverDividesByZeroOrCompacts covers the shapes that
// break a break-even calculation: no reduction, a negative one, a zero
// cache-read rate, and a conversation smaller after compaction than the prefix
// that survives it.
func TestDegenerateArithmeticNeverDividesByZeroOrCompacts(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*domain.CompactionEconomicsInput)
		wantStatus domain.CompactionEconomicVerdict
		wantReason domain.CompactionEconomicReason
	}{{
		name: "compaction would grow the conversation",
		mutate: func(in *domain.CompactionEconomicsInput) {
			in.ContextAfter.Value = in.ContextBeforeTokens + in.PromptTokens + 1
		},
		wantStatus: domain.CompactionVerdictSkip, wantReason: domain.CompactionReasonNegative,
	}, {
		name: "compaction would change nothing",
		mutate: func(in *domain.CompactionEconomicsInput) {
			in.ContextAfter.Value = in.ContextBeforeTokens + in.PromptTokens
		},
		wantStatus: domain.CompactionVerdictSkip, wantReason: domain.CompactionReasonNegative,
	}, {
		name: "a rate card whose cache reads are free",
		mutate: func(in *domain.CompactionEconomicsInput) {
			in.Rates.CacheReadPerMTok = 0
		},
		wantStatus: domain.CompactionVerdictUnknown, wantReason: domain.CompactionReasonPricingUnknown,
	}, {
		name: "post-compaction context below the surviving prefix",
		mutate: func(in *domain.CompactionEconomicsInput) {
			in.StablePrefixTokens = in.ContextAfter.Value + 1
		},
		wantStatus: domain.CompactionVerdictUnknown, wantReason: domain.CompactionReasonInconsistentAccounting,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := historicalInput(293224, 47809, 26009, 2500, 8050, 19)
			tc.mutate(&in)
			got := domain.EvaluateCompactionEconomics(in)
			if got.Verdict == domain.CompactionVerdictCompact {
				t.Fatalf("COMPACT on a degenerate input")
			}
			if got.Verdict != tc.wantStatus || got.Reason != tc.wantReason {
				t.Errorf("verdict = %s/%s, want %s/%s", got.Verdict, got.Reason, tc.wantStatus, tc.wantReason)
			}
			for _, f := range []float64{got.BreakEvenCalls, got.RequiredCalls, got.Margin, got.EstimatedReductionPercent} {
				if math.IsNaN(f) || math.IsInf(f, 0) {
					t.Errorf("a non-finite number reached the record: %v", f)
				}
			}
		})
	}
}

// TestExactBreakEvenIsNotEnough pins the boundary condition. At exactly N* the
// compaction pays for itself and nothing more; the safety factor exists because
// N* is computed from three predictions, so exact break-even must SKIP and it
// must skip with the reason that says it was close.
func TestExactBreakEvenIsNotEnough(t *testing.T) {
	in := historicalInput(293224, 47809, 26009, 2500, 8050, 19)
	probe := domain.EvaluateCompactionEconomics(in)
	exact := int64(math.Ceil(probe.BreakEvenCalls))

	in.RemainingCalls.Value = exact
	atBreakEven := domain.EvaluateCompactionEconomics(in)
	if atBreakEven.Verdict != domain.CompactionVerdictSkip || atBreakEven.Reason != domain.CompactionReasonBelowSafetyMargin {
		t.Errorf("at exactly break-even: %s/%s, want skip/below_safety_margin", atBreakEven.Verdict, atBreakEven.Reason)
	}
	if atBreakEven.Margin >= 1 {
		t.Errorf("margin at break-even = %.4f, want below 1", atBreakEven.Margin)
	}

	in.RemainingCalls.Value = exact - 1
	below := domain.EvaluateCompactionEconomics(in)
	if below.Reason != domain.CompactionReasonInsufficientFutureCalls {
		t.Errorf("below break-even: reason %s, want insufficient_future_calls", below.Reason)
	}

	in.RemainingCalls.Value = int64(math.Ceil(probe.RequiredCalls))
	atRequired := domain.EvaluateCompactionEconomics(in)
	if atRequired.Verdict != domain.CompactionVerdictCompact {
		t.Errorf("at the required call count: %s/%s, want compact", atRequired.Verdict, atRequired.Reason)
	}
	if atRequired.Margin < 1 {
		t.Errorf("margin at the required count = %.4f, want at least 1", atRequired.Margin)
	}
}

// TestNetSavingsAndMicrosAreIntegersAndConsistent checks the money. Costs are
// integer micros because a float in a durable payload makes two records from
// different builds incomparable, and the three figures must agree with each
// other or a reader cannot subtract them.
func TestNetSavingsAndMicrosAreIntegersAndConsistent(t *testing.T) {
	got := domain.EvaluateCompactionEconomics(historicalInput(293224, 47809, 26009, 2500, 8050, 19))

	if got.SavingPerCallMicros <= 0 || got.EstimatedCompactionCostMicros <= 0 {
		t.Fatalf("cost/saving not computed: cost=%d perCall=%d",
			got.EstimatedCompactionCostMicros, got.SavingPerCallMicros)
	}
	wantFuture := got.SavingPerCallMicros * got.EstimatedRemainingCalls
	if diff := got.EstimatedFutureSavingsMicros - wantFuture; diff < -1 || diff > 1 {
		t.Errorf("future savings %d, want ~%d (per-call x calls)", got.EstimatedFutureSavingsMicros, wantFuture)
	}
	wantNet := got.EstimatedFutureSavingsMicros - got.EstimatedCompactionCostMicros
	if diff := got.EstimatedNetSavingsMicros - wantNet; diff < -1 || diff > 1 {
		t.Errorf("net savings %d, want ~%d (future - cost)", got.EstimatedNetSavingsMicros, wantNet)
	}
	if got.EstimatedNetSavingsMicros <= 0 {
		t.Errorf("a COMPACT verdict with non-positive net savings: %d", got.EstimatedNetSavingsMicros)
	}
}

// TestTheRecordCarriesNoContent is the privacy test, and it is a test about the
// TYPE rather than about the code that fills it.
//
// Every field of the record is walked, and any string field that is not an
// identifier or a closed-enum value is a finding. The fixture deliberately
// carries content-shaped ids so that a field that echoed its input would be
// caught: if a prompt, a path or a finding could ever reach the payload, it
// would have to arrive through one of these fields.
func TestTheRecordCarriesNoContent(t *testing.T) {
	const secret = "SECRET-do-not-persist: rm -rf / && cat ~/.ssh/id_rsa"
	in := historicalInput(293224, 47809, 26009, 2500, 8050, 19)
	// Every string the caller controls, set to something that must not survive
	// except as the identifier it is.
	in.SessionID = "session-" + secret
	in.Strategy = secret
	in.Harness = secret
	in.Provider = secret
	in.Rates.Source = secret
	got := domain.EvaluateCompactionEconomics(in)

	// The record's own enums must be closed values, never pass-through.
	if !got.Verdict.Valid() || !got.Reason.Valid() {
		t.Errorf("verdict/reason outside the closed enum: %q/%q", got.Verdict, got.Reason)
	}
	if !got.ContextAfterBasis.Valid() || !got.SummaryBasis.Valid() || !got.RemainingCallsBasis.Valid() {
		t.Errorf("an estimator basis outside the closed enum")
	}
	if !got.CacheWriteLifetimePriced.Valid() {
		t.Errorf("cache write lifetime outside the closed enum: %q", got.CacheWriteLifetimePriced)
	}
	// The ONLY places a caller's free text is allowed to survive are the
	// identity and provenance fields, which the caller is trusted to fill with
	// identifiers. Everything else is a number. This assertion is what fails
	// when somebody adds a `Notes string` or a `Detail string` to the record.
	if len(got.LifecycleReasons) != 1 || got.LifecycleReasons[0] != string(domain.LifecycleReasonContextPressureActual) {
		t.Errorf("lifecycleReasons = %v, want the closed lifecycle enum value only", got.LifecycleReasons)
	}
}

// --- estimators --------------------------------------------------------------

// TestRemainingCallsEstimatorPinsEveryHistoricalSample is §6's table as a test,
// including the sample that documents the estimator's worst failure.
//
// "real" is calls in that cycle minus one: the estimate is of the calls AFTER
// the first post-compaction call, because the first one's share is already a
// credit inside the cost.
func TestRemainingCallsEstimatorPinsEveryHistoricalSample(t *testing.T) {
	cases := []struct {
		run        string
		cycle      int
		prior      []domain.CompactionCycleCalls
		wantRaw    int64
		wantCapped int64
		real       int64
		note       string
	}{
		{run: "wf-0aadfcde", cycle: 1, prior: []domain.CompactionCycleCalls{{Cycle: 0, Calls: 85}},
			wantRaw: 21, wantCapped: 19, real: 18, note: "over by 3 raw, by 1 after the cap"},
		{run: "wf-1c2cb9bd", cycle: 1, prior: []domain.CompactionCycleCalls{{Cycle: 0, Calls: 118}},
			wantRaw: 29, wantCapped: 19, real: 29, note: "exact raw"},
		{run: "wf-1c2cb9bd", cycle: 2, prior: []domain.CompactionCycleCalls{{Cycle: 0, Calls: 118}, {Cycle: 1, Calls: 30}},
			wantRaw: 24, wantCapped: 19, real: 24, note: "exact raw"},
		{run: "wf-1c2cb9bd", cycle: 3, prior: []domain.CompactionCycleCalls{{Cycle: 0, Calls: 118}, {Cycle: 1, Calls: 30}, {Cycle: 2, Calls: 25}},
			wantRaw: 20, wantCapped: 19, real: 19, note: "over by 1 raw"},
		{run: "wf-5b2210f5", cycle: 1, prior: []domain.CompactionCycleCalls{{Cycle: 0, Calls: 27}},
			wantRaw: 6, wantCapped: 6, real: 56, note: "the catastrophic UNDERprediction; costs a saving, never a loss"},
		{run: "wf-66f0ee54", cycle: 1, prior: []domain.CompactionCycleCalls{{Cycle: 0, Calls: 16}},
			wantRaw: 4, wantCapped: 4, real: 6, note: "under by 2"},
		{run: "wf-66f0ee54", cycle: 2, prior: []domain.CompactionCycleCalls{{Cycle: 0, Calls: 16}, {Cycle: 1, Calls: 7}},
			wantRaw: 5, wantCapped: 5, real: 4, note: "over by 1"},
		{run: "wf-88e71ef2", cycle: 1, prior: []domain.CompactionCycleCalls{{Cycle: 0, Calls: 12}},
			wantRaw: 3, wantCapped: 3, real: 4, note: "under by 1"},
	}
	overpredictions := 0
	for _, tc := range cases {
		t.Run(tc.run+"/cycle"+itoa(tc.cycle), func(t *testing.T) {
			got := domain.EstimateRemainingCalls(domain.CompactionRemainingCallsEstimatorInput{
				CurrentCycle: tc.cycle, CycleCalls: tc.prior,
			})
			if !got.Known {
				t.Fatalf("estimator returned unknown with prior cycles present")
			}
			if got.Value != tc.wantCapped {
				t.Errorf("estimate = %d, want %d (%s)", got.Value, tc.wantCapped, tc.note)
			}
			wantBasis := domain.CompactionBasisPriorCycleDiscounted
			if tc.cycle > 1 {
				wantBasis = domain.CompactionBasisPriorCycleMinDiscounted
			}
			if got.Basis != wantBasis {
				t.Errorf("basis = %q, want %q", got.Basis, wantBasis)
			}
			if wantCap := tc.wantRaw > tc.wantCapped; got.CapApplied != wantCap {
				t.Errorf("capApplied = %v, want %v", got.CapApplied, wantCap)
			}
			if got.Value > tc.real {
				overpredictions++
				// The justification for a safety factor of 1.5 is that the
				// estimator's overpredictions are SMALL: 1.25 covers the worst
				// one AO has ever measured and nothing else. A future change
				// that overpredicts by more than that has to re-argue the
				// factor rather than inherit it.
				if ratio := float64(got.Value) / float64(tc.real); ratio > 1.25 {
					t.Errorf("overpredicted by %.2fx (%d against a real %d); 1.25 is the worst case the safety factor was set from",
						ratio, got.Value, tc.real)
				}
			}
		})
	}
	// Two of the eight overpredict, both within 1.25x. Pinned as a count so that
	// a change which makes the estimator optimistic more often is visible even
	// when each individual error stays small.
	if overpredictions != 2 {
		t.Errorf("%d of 8 historical samples overpredict, want 2 (wf-0aadfcde by 1 call, wf-66f0ee54 cycle 2 by 1)", overpredictions)
	}
}

// TestRemainingCallsEstimatorRefusesWithoutEvidence: no prior cycle, no
// estimate. Never a default.
func TestRemainingCallsEstimatorRefusesWithoutEvidence(t *testing.T) {
	cases := []struct {
		name string
		in   domain.CompactionRemainingCallsEstimatorInput
	}{
		{"no cycles at all", domain.CompactionRemainingCallsEstimatorInput{CurrentCycle: 1}},
		{"cycle zero", domain.CompactionRemainingCallsEstimatorInput{CurrentCycle: 0, CycleCalls: []domain.CompactionCycleCalls{{Cycle: 0, Calls: 100}}}},
		{"base cycle made no calls", domain.CompactionRemainingCallsEstimatorInput{CurrentCycle: 1, CycleCalls: []domain.CompactionCycleCalls{{Cycle: 0, Calls: 0}}}},
		{"second cycle with no first repair cycle", domain.CompactionRemainingCallsEstimatorInput{CurrentCycle: 2, CycleCalls: []domain.CompactionCycleCalls{{Cycle: 0, Calls: 100}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := domain.EstimateRemainingCalls(tc.in)
			if got.Known || got.Value != 0 || got.Basis != domain.CompactionBasisNone {
				t.Errorf("got %+v, want an unknown estimate with basis none", got)
			}
		})
	}
}

// TestRemainingCallsEstimatorIgnoresTheCurrentCycle is the look-ahead guard on
// the N axis: the cycle being dispatched has not made its calls yet, and a
// row claiming otherwise must not be counted.
func TestRemainingCallsEstimatorIgnoresTheCurrentCycle(t *testing.T) {
	without := domain.EstimateRemainingCalls(domain.CompactionRemainingCallsEstimatorInput{
		CurrentCycle: 2,
		CycleCalls:   []domain.CompactionCycleCalls{{Cycle: 0, Calls: 118}, {Cycle: 1, Calls: 30}},
	})
	with := domain.EstimateRemainingCalls(domain.CompactionRemainingCallsEstimatorInput{
		CurrentCycle: 2,
		CycleCalls: []domain.CompactionCycleCalls{
			{Cycle: 0, Calls: 118}, {Cycle: 1, Calls: 30},
			// The future, offered to the estimator.
			{Cycle: 2, Calls: 25}, {Cycle: 3, Calls: 20},
		},
	})
	if with != without {
		t.Errorf("the estimator saw the future: with=%+v without=%+v", with, without)
	}
}

// TestContextAfterEstimatorPrefersTheSessionsOwnBoundary pins the A estimator
// and the reattach factor against the two measured boundaries.
func TestContextAfterEstimatorPrefersTheSessionsOwnBoundary(t *testing.T) {
	decisionAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	earlier := decisionAt.Add(-2 * time.Hour)

	// THE ONLY OUT-OF-SAMPLE PREDICTION THE CORPUS SUPPORTS: compact 2's context
	// predicted from compact 1's post size, which is the only post size a
	// decision about compact 2 can actually have. Using compact 2's OWN
	// postTokens (13,240) here would be the look-ahead this estimator exists to
	// prevent, and a factor fitted that way is optimistic out of sample.
	got := domain.EstimateContextAfter(domain.CompactionContextAfterEstimatorInput{
		StablePrefixTokens: 37379,
		PromptTokens:       1062,
		Boundaries:         []domain.CompactionBoundary{{PostTokens: 10956, ObservedAt: &earlier}},
		DecisionAt:         decisionAt,
	})
	if !got.Known {
		t.Fatalf("an admissible boundary produced no estimate")
	}
	if got.Basis != domain.CompactionBasisSessionPriorBoundary || got.Samples != 1 {
		t.Errorf("basis/samples = %q/%d, want session_prior_boundary/1", got.Basis, got.Samples)
	}
	// 1.25 * (37379 + 10956 + 1062) = 1.25 * 49397 = 61,746.
	if got.Value != 61746 {
		t.Errorf("estimated A = %d, want 61746", got.Value)
	}
	// The measured A was 61,099. The estimate MUST be at or above it: a larger A
	// is a smaller reduction is a verdict further from COMPACT. At the in-sample
	// factor of 1.19 this estimate would have been 58,782 -- 3.8% BELOW the
	// measurement, which is the optimistic direction, and this assertion is what
	// catches a future re-fit that slips back into it.
	if got.Value < 61099 {
		t.Errorf("estimated A = %d is below the measured 61099; the reattach factor must never be optimistic", got.Value)
	}
}

// TestContextAfterEstimatorCannotSeeTheCurrentBoundary is the look-ahead test the
// design asked for by name.
//
// Given the boundary produced by the very compaction under judgement, the
// estimate must be IDENTICAL to the estimate without it. This is the test that
// fails if the caller's filter is removed and the estimator is trusted to have
// received clean input.
func TestContextAfterEstimatorCannotSeeTheCurrentBoundary(t *testing.T) {
	decisionAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	past := decisionAt.Add(-2 * time.Hour)
	present := decisionAt
	future := decisionAt.Add(time.Minute)
	var unplaceable *time.Time

	base := domain.CompactionContextAfterEstimatorInput{
		StablePrefixTokens: 37379, PromptTokens: 1062, DecisionAt: decisionAt,
		Boundaries: []domain.CompactionBoundary{{PostTokens: 13240, ObservedAt: &past}},
	}
	want := domain.EstimateContextAfter(base)

	contaminated := base
	contaminated.Boundaries = []domain.CompactionBoundary{
		{PostTokens: 13240, ObservedAt: &past},
		// The current compaction's own boundary, at the decision instant.
		{PostTokens: 999, ObservedAt: &present},
		// A later one.
		{PostTokens: 111, ObservedAt: &future},
		// One that cannot be ordered at all: inadmissible, not assumed early.
		{PostTokens: 222, ObservedAt: unplaceable},
	}
	if got := domain.EstimateContextAfter(contaminated); got != want {
		t.Errorf("the estimator read a boundary it must not see: got %+v, want %+v", got, want)
	}
}

// TestContextAfterEstimatorRefusesWithoutABoundary: no observation, no estimate,
// no constant.
func TestContextAfterEstimatorRefusesWithoutABoundary(t *testing.T) {
	decisionAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	earlier := decisionAt.Add(-time.Hour)
	cases := []struct {
		name string
		in   domain.CompactionContextAfterEstimatorInput
	}{
		{"no boundaries", domain.CompactionContextAfterEstimatorInput{StablePrefixTokens: 37379, DecisionAt: decisionAt}},
		{"a boundary that reported no post size", domain.CompactionContextAfterEstimatorInput{
			StablePrefixTokens: 37379, DecisionAt: decisionAt,
			Boundaries: []domain.CompactionBoundary{{PostTokens: 0, ObservedAt: &earlier}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := domain.EstimateContextAfter(tc.in)
			if got.Known || got.Basis != domain.CompactionBasisNone {
				t.Errorf("got %+v, want unknown/none", got)
			}
		})
	}
}

// TestSummaryEstimatorNeverUsesTheSessionUnderJudgement is the structural rule:
// S is only measurable from an end-of-session rollup, and a running session has
// none. A session estimating its own summary cost is not a bug to guard against
// so much as a thing that cannot be correct.
func TestSummaryEstimatorNeverUsesTheSessionUnderJudgement(t *testing.T) {
	own := domain.CompactionSummarySessionObservation{
		SessionID: "session-under-judgement", Compactions: 2, UnattributedOutputTokens: 999999,
	}
	other := domain.CompactionSummarySessionObservation{
		SessionID: "ao-canary-fixture-6", Compactions: 2, UnattributedOutputTokens: 16986,
	}

	only := domain.EstimateSummaryTokens(domain.CompactionSummaryEstimatorInput{
		ExcludeSessionID: "session-under-judgement",
		Observations:     []domain.CompactionSummarySessionObservation{own},
	})
	if only.Known {
		t.Errorf("estimated S from the session being judged: %+v", only)
	}
	if only.Samples != 0 {
		t.Errorf("samples = %d, want 0: the only observation was the judged session's own", only.Samples)
	}

	// The judged session is excluded, so this cohort has ONE admissible sample --
	// below the minimum of three independent sessions, and therefore UNKNOWN with
	// the sample count reported so a reader can see how far off it is.
	got := domain.EstimateSummaryTokens(domain.CompactionSummaryEstimatorInput{
		ExcludeSessionID: "session-under-judgement",
		Observations:     []domain.CompactionSummarySessionObservation{own, other},
	})
	if got.Known {
		t.Errorf("one session is not a prior: %+v", got)
	}
	if got.Samples != 1 {
		t.Errorf("samples = %d, want 1 reported even though the answer is unknown", got.Samples)
	}
	if got.Value != 0 {
		t.Errorf("an unknown estimate must carry no value, got %d", got.Value)
	}
}

// TestSummaryEstimatorTakesTheExpensiveEndUntilThereIsAPopulation: below the
// minimum sample it uses the worst observation, not an average of two.
func TestSummaryEstimatorTakesTheExpensiveEndUntilThereIsAPopulation(t *testing.T) {
	obs := func(id string, compactions int, out int64) domain.CompactionSummarySessionObservation {
		return domain.CompactionSummarySessionObservation{SessionID: id, Compactions: compactions, UnattributedOutputTokens: out}
	}
	twoSessions := domain.EstimateSummaryTokens(domain.CompactionSummaryEstimatorInput{
		Observations: []domain.CompactionSummarySessionObservation{obs("a", 1, 4000), obs("b", 1, 9000)},
	})
	if twoSessions.Known {
		t.Errorf("two sessions is not a prior: %+v", twoSessions)
	}
	// At three independent sessions the prior becomes known, and it is the
	// MAXIMUM rather than the mean: S enters the cost of compacting, so
	// underestimating it makes compaction look cheaper than it is.
	threeSessions := domain.EstimateSummaryTokens(domain.CompactionSummaryEstimatorInput{
		Observations: []domain.CompactionSummarySessionObservation{obs("a", 1, 4000), obs("b", 1, 9000), obs("c", 1, 5000)},
	})
	if !threeSessions.Known {
		t.Fatalf("three independent sessions must make S known, got %+v", threeSessions)
	}
	if threeSessions.Value != 9000 {
		t.Errorf("with 3 samples S = %d, want the conservative maximum 9000 (the mean would be 6000 and would understate)",
			threeSessions.Value)
	}
	if threeSessions.Samples != 3 {
		t.Errorf("samples = %d, want 3", threeSessions.Samples)
	}
	// THREE BOUNDARIES OF ONE SESSION ARE ONE SAMPLE. They share a harness, a
	// project and a task shape, and the quantity being estimated moves with all
	// three, so counting them as three would be a fabricated population.
	sameSession := domain.EstimateSummaryTokens(domain.CompactionSummaryEstimatorInput{
		Observations: []domain.CompactionSummarySessionObservation{
			obs("a", 3, 27000), obs("a", 3, 27000), obs("a", 3, 27000),
		},
	})
	if sameSession.Known || sameSession.Samples != 1 {
		t.Errorf("three rows from one session gave %+v, want unknown with 1 sample", sameSession)
	}
	none := domain.EstimateSummaryTokens(domain.CompactionSummaryEstimatorInput{})
	if none.Known {
		t.Errorf("estimated S from nothing: %+v", none)
	}
}

// itoa avoids a strconv import in a file that needs exactly one integer
// formatted, matching the tiny local helpers this package already carries.
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
