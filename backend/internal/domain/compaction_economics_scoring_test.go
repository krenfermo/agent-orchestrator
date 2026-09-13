package domain_test

import (
	"math"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// compaction_economics_scoring_test.go -- scoring the verdicts, and refusing to
// score the ones that cannot be scored.

// canaryOutcome is one of the two REAL compactions, as measured: the boundary's
// own preTokens, the first post-compaction call's context, the calls that
// actually followed, and the summary size from the session's residual.
func canaryOutcome(b, a, delta, remaining int64) domain.CompactionObservedOutcome {
	return domain.CompactionObservedOutcome{
		Compacted:           true,
		ContextBeforeTokens: b,
		ContextAfterTokens:  a,
		StablePrefixTokens:  37_379,
		PromptTokens:        delta,
		SummaryTokens:       8_050,
		SummaryTokensKnown:  true,
		RemainingCalls:      remaining,
		RemainingCallsKnown: true,
		Rates:               opus5Rates(),
		RatesKnown:          true,
		CacheWriteLifetime:  domain.CacheWriteLifetime1h,
	}
}

// TestTheCanaryRetrospectiveConfirmsBothSkips is the canary as a retrospective.
//
// The shadow gate would have said SKIP to both of its compactions. Both
// compactions happened anyway -- the policy asked for them and the gate governs
// nothing -- so both are among the rare SCORABLE skips, and the measurement
// confirms both were losses. This is the only way a skip is ever vindicated by a
// measurement rather than by its own forecast.
//
// The realized figures are the corrected ones: priced at the ONE-HOUR
// cache-write rate the provider actually charged, not the five-minute rate the
// embedded catalog derives.
func TestTheCanaryRetrospectiveConfirmsBothSkips(t *testing.T) {
	cases := []struct {
		name           string
		in             domain.CompactionEconomicsInput
		observed       domain.CompactionObservedOutcome
		wantNetDollars float64
	}{{
		name:           "compact 1",
		in:             historicalInput(85847, 57803, 37379, 1181, 8050, 6),
		observed:       canaryOutcome(85847, 57803, 1181, 6),
		wantNetDollars: -0.324695,
	}, {
		name:           "compact 2",
		in:             historicalInput(67663, 61099, 37379, 1062, 8050, 4),
		observed:       canaryOutcome(67663, 61099, 1062, 4),
		wantNetDollars: -0.431268,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := domain.EvaluateCompactionEconomics(tc.in)
			if record.Verdict != domain.CompactionVerdictSkip {
				t.Fatalf("verdict = %s/%s, want skip", record.Verdict, record.Reason)
			}
			score := domain.ScoreCompactionVerdict(record, tc.observed)
			if !score.Scored {
				t.Fatalf("the compaction happened and every term was measured; it must be scorable")
			}
			if score.Outcome != domain.CompactionScoreSkipVindicated {
				t.Errorf("outcome = %s, want skip_vindicated (the compaction lost money)", score.Outcome)
			}
			gotDollars := float64(score.RealizedNetMicros) / 1_000_000
			if math.Abs(gotDollars-tc.wantNetDollars) > 0.0005 {
				t.Errorf("realized net = $%.6f, want $%.6f", gotDollars, tc.wantNetDollars)
			}
			if score.RealizedNetMicros >= 0 {
				t.Errorf("realized net = %d micros, want a loss", score.RealizedNetMicros)
			}
		})
	}
}

// TestASkipOnACompactionThatNeverHappenedIsNotCorrect is the honesty rule, and
// the single most important assertion in this file.
//
// A SKIP verdict whose compaction did not happen has no observable
// counterfactual. AO never learns what A or S would have been, so the skip can
// never be MEASURED correct -- only predicted correct. Classifying it as a
// success would manufacture a precision no experiment produced.
func TestASkipOnACompactionThatNeverHappenedIsNotCorrect(t *testing.T) {
	record := domain.EvaluateCompactionEconomics(historicalInput(85847, 57803, 37379, 1181, 8050, 6))
	if record.Verdict != domain.CompactionVerdictSkip {
		t.Fatalf("fixture precondition: wanted a skip, got %s", record.Verdict)
	}
	score := domain.ScoreCompactionVerdict(record, domain.CompactionObservedOutcome{Compacted: false})
	if score.Outcome != domain.CompactionScoreUnscoredCounterfactual {
		t.Errorf("outcome = %s, want unscored_counterfactual", score.Outcome)
	}
	if score.Scored {
		t.Error("a skip with no compaction must not be marked scored")
	}
	if score.RealizedNetMicros != 0 {
		t.Errorf("realized net = %d, want 0: there is nothing to have realized", score.RealizedNetMicros)
	}
}

// An UNKNOWN verdict declined to predict, so it can be neither right nor wrong.
// Counting it either way turns a refusal into a data point.
func TestAnUnknownVerdictIsUnscorable(t *testing.T) {
	in := historicalInput(85847, 57803, 37379, 1181, 8050, 6)
	in.RatesKnown = false
	record := domain.EvaluateCompactionEconomics(in)
	if record.Verdict != domain.CompactionVerdictUnknown {
		t.Fatalf("fixture precondition: wanted unknown, got %s", record.Verdict)
	}
	score := domain.ScoreCompactionVerdict(record, canaryOutcome(85847, 57803, 1181, 6))
	if score.Outcome != domain.CompactionScoreUnscorable || score.Scored {
		t.Errorf("outcome = %s scored = %v, want unscorable and unscored", score.Outcome, score.Scored)
	}
}

// A COMPACT verdict on a compaction that lost money is a FALSE POSITIVE, and the
// enforcement criterion requires zero of them. This is the test that proves the
// scorer can actually produce one -- a scoreboard that structurally cannot
// report a false positive would pass the criterion vacuously.
func TestAFalsePositiveIsDetectedAndBlocksEnforcement(t *testing.T) {
	// The profitable shape's verdict, scored against the canary's losing
	// outcome: a COMPACT recommendation that would have lost money.
	record := domain.EvaluateCompactionEconomics(historicalInput(293224, 47809, 26009, 2500, 8050, 19))
	if record.Verdict != domain.CompactionVerdictCompact {
		t.Fatalf("fixture precondition: wanted compact, got %s/%s", record.Verdict, record.Reason)
	}
	score := domain.ScoreCompactionVerdict(record, canaryOutcome(67663, 61099, 1062, 4))
	if score.Outcome != domain.CompactionScoreFalsePositive {
		t.Fatalf("outcome = %s, want false_positive", score.Outcome)
	}
	board := domain.BuildCompactionScoreboard([]domain.CompactionVerdictScore{score})
	ready, answerable := board.EnforcementReady()
	if !answerable {
		t.Error("a scored cohort must be answerable")
	}
	if ready {
		t.Error("enforcement must not be reported ready with a false positive on the board")
	}
}

// TestAnEmptyScoreboardIsNotReadyAndSaysSo. Verdicts accumulate in shadow mode
// while outcomes do not, so a count of predictions can look like progress for
// months. An empty scoreboard has not passed the criterion; it has not taken it.
func TestAnEmptyScoreboardIsNotReadyAndSaysSo(t *testing.T) {
	board := domain.BuildCompactionScoreboard(nil)
	ready, answerable := board.EnforcementReady()
	if answerable {
		t.Error("an empty scoreboard must report the question as unanswerable")
	}
	if ready {
		t.Error("an empty scoreboard must never report enforcement ready")
	}

	// A cohort made entirely of unscored counterfactuals is the same situation
	// with more rows, and must read the same way.
	onlyCounterfactuals := []domain.CompactionVerdictScore{
		{Outcome: domain.CompactionScoreUnscoredCounterfactual},
		{Outcome: domain.CompactionScoreUnscoredCounterfactual},
		{Outcome: domain.CompactionScoreUnscorable},
	}
	board = domain.BuildCompactionScoreboard(onlyCounterfactuals)
	if board.Total != 3 || board.Scored != 0 {
		t.Errorf("total/scored = %d/%d, want 3/0", board.Total, board.Scored)
	}
	if _, answerable := board.EnforcementReady(); answerable {
		t.Error("three unscored rows are still no evidence")
	}
}

// The estimator errors are signed so the direction is legible, and they are
// measurable even when the money is not.
func TestEstimatorErrorsAreSignedAndSurviveAnUnmeasurableCost(t *testing.T) {
	record := domain.EvaluateCompactionEconomics(historicalInput(85847, 57803, 37379, 1181, 8050, 6))
	observed := canaryOutcome(85847, 50000, 1181, 10)
	// The session has not ended, so its residual -- and therefore S -- is not
	// yet measurable. The cost must not be computed from a missing S.
	observed.SummaryTokensKnown = false

	score := domain.ScoreCompactionVerdict(record, observed)
	if score.Scored {
		t.Error("a cost must not be computed while the summary size is unknown")
	}
	if !score.ContextAfterErrorKnown {
		t.Fatal("the context-after error is measurable and must be reported")
	}
	// Estimated 57,803 against an observed 50,000: the estimate was 7,803 too
	// large, which is the conservative direction.
	if score.ContextAfterErrorTokens != 7_803 {
		t.Errorf("context-after error = %d, want +7803", score.ContextAfterErrorTokens)
	}
	if !score.RemainingCallsErrorKnown || score.RemainingCallsErrorCalls != -4 {
		t.Errorf("remaining-calls error = %d (known %v), want -4: the forecast was 6 against an actual 10",
			score.RemainingCallsErrorCalls, score.RemainingCallsErrorKnown)
	}
	if score.SummaryTokensErrorKnown {
		t.Error("the summary error must not be claimed when the summary was never measured")
	}
}

// A realized outcome priced at a different rate card version from the verdict is
// a comparison of two currencies. It is still scored, and it is FLAGGED, so a
// cohort statistic can exclude it.
func TestAMismatchedRateCardIsFlaggedRatherThanSilentlyCompared(t *testing.T) {
	record := domain.EvaluateCompactionEconomics(historicalInput(85847, 57803, 37379, 1181, 8050, 6))
	observed := canaryOutcome(85847, 57803, 1181, 6)
	observed.Rates.Version = "2099-01-01"

	score := domain.ScoreCompactionVerdict(record, observed)
	if !score.Scored {
		t.Fatal("the outcome was fully measured and must still be scored")
	}
	if score.RatesMatchVerdict {
		t.Error("a different rate card version must be flagged as a mismatch")
	}
	board := domain.BuildCompactionScoreboard([]domain.CompactionVerdictScore{score})
	if board.MismatchedRates != 1 {
		t.Errorf("mismatchedRates = %d, want 1", board.MismatchedRates)
	}
}

// --- the cohort fold ---------------------------------------------------------

// TestTheCohortSummaryAnswersTheOperatorsQuestions walks the questions the
// readback exists to answer: how many, of what, why, at what break-even, for
// what estimated saving.
func TestTheCohortSummaryAnswersTheOperatorsQuestions(t *testing.T) {
	records := []domain.CompactionEconomicsRecord{
		domain.EvaluateCompactionEconomics(historicalInput(293224, 47809, 26009, 2500, 8050, 19)), // compact
		domain.EvaluateCompactionEconomics(historicalInput(85847, 57803, 37379, 1181, 8050, 6)),   // skip
		domain.EvaluateCompactionEconomics(historicalInput(67663, 61099, 37379, 1062, 8050, 4)),   // skip
	}
	unpriced := historicalInput(293224, 47809, 26009, 2500, 8050, 19)
	unpriced.RatesKnown = false
	records = append(records, domain.EvaluateCompactionEconomics(unpriced)) // unknown

	got := domain.SummarizeCompactionEconomics(records)
	if got.Evaluations != 4 || got.Compact != 1 || got.Skip != 2 || got.Unknown != 1 {
		t.Errorf("counts = %d eval / %d compact / %d skip / %d unknown, want 4/1/2/1",
			got.Evaluations, got.Compact, got.Skip, got.Unknown)
	}
	if got.ByReason[domain.CompactionReasonPositiveMargin] != 1 {
		t.Errorf("positive-margin count = %d, want 1", got.ByReason[domain.CompactionReasonPositiveMargin])
	}
	if got.ByReason[domain.CompactionReasonPricingUnknown] != 1 {
		t.Errorf("pricing-unknown count = %d, want 1", got.ByReason[domain.CompactionReasonPricingUnknown])
	}
	// Pricing coverage is the number §13's first criterion is measured against.
	coverage, ok := got.PricingCoverage()
	if !ok {
		t.Fatal("coverage must be answerable over a non-empty cohort")
	}
	if math.Abs(coverage-75) > 0.001 {
		t.Errorf("pricing coverage = %.2f%%, want 75%% (3 of 4)", coverage)
	}
	// The P7.2B1 honesty check: every priced row used the one-hour rate.
	if got.CacheWriteLifetime1h != 3 || got.CacheWriteLifetime5m != 0 {
		t.Errorf("lifetimes priced = %d at 1h / %d at 5m, want 3/0",
			got.CacheWriteLifetime1h, got.CacheWriteLifetime5m)
	}
	if got.BreakEvenComputed != 3 {
		t.Errorf("break-even computed on %d rows, want 3", got.BreakEvenComputed)
	}
	if got.CompactNetSavingsMicros <= 0 {
		t.Errorf("compact net savings = %d, want positive", got.CompactNetSavingsMicros)
	}
	// Both measured compactions sit two orders of magnitude below the near-miss
	// floor, so the cohort contains no plausible false negative.
	if got.SkipNearMisses != 0 {
		t.Errorf("near-miss skips = %d, want 0: both canary margins are below 0.1", got.SkipNearMisses)
	}
	if len(got.GateVersions) != 1 || len(got.EstimatorVersions) != 1 {
		t.Errorf("a single-build cohort must carry one gate and one estimator version, got %v / %v",
			got.GateVersions, got.EstimatorVersions)
	}
}

// TestAnEmptyCohortIsEmptyAndNotZeroPercent. An empty cohort has no pricing
// coverage; reporting 0% would read as a pricing failure rather than as an
// absence of evaluations.
func TestAnEmptyCohortIsEmptyAndNotZeroPercent(t *testing.T) {
	got := domain.SummarizeCompactionEconomics(nil)
	if got.Evaluations != 0 {
		t.Errorf("evaluations = %d, want 0", got.Evaluations)
	}
	if _, ok := got.PricingCoverage(); ok {
		t.Error("coverage must be unanswerable over an empty cohort")
	}
	// The maps must be usable without a nil check.
	if got.ByReason == nil || got.ByPricingStatus == nil || got.GateVersions == nil {
		t.Error("an empty summary must still carry initialized maps")
	}
}

// TestTheReplayCostReductionIsNotTheTokenReduction pins the figure §22 exists to
// correct, at the rates P7.2B1 proved.
//
// The turnbench replay of wf-1c2cb9bd cuts cumulative billed input by 39.0%. It
// does NOT cut the bill by 39.0%, because compaction moves tokens from the
// cheapest input rate to the dearest and generates a summary at output prices
// that no call series contains. Derived here from the run's own measured vector
// so the two figures can never again be reported as the same number.
func TestTheReplayCostReductionIsNotTheTokenReduction(t *testing.T) {
	rates := opus5Rates()
	const perMillion = 1_000_000.0
	// The run's measured vector, verified against the real ledger: 386 uncached,
	// 35,787,742 cache read, 294,688 cache creation ALL at the one-hour
	// lifetime, 139,003 output.
	costWithout := 386*rates.InputPerMTok +
		35_787_742*rates.CacheReadPerMTok +
		294_688*rates.CacheWrite1hPerMTok +
		139_003*rates.OutputPerMTok
	if math.Abs(costWithout/perMillion-24.317756) > 0.0005 {
		t.Fatalf("baseline cost = $%.6f, want $24.317756", costWithout/perMillion)
	}

	// The three replay boundaries with their own measured call tails.
	boundaries := []struct {
		b, n int64
	}{{199_892, 29}, {251_148, 24}, {293_224, 19}}
	const p, a, delta, summary int64 = 26_009, 47_809, 2_500, 8_050
	var totalCost, totalSaving float64
	for _, bd := range boundaries {
		in := historicalInput(bd.b, a, p, delta, summary, bd.n)
		// The replay's own call tail, not the estimator's capped forecast: this
		// is what the boundary ACTUALLY had left.
		in.RemainingCalls = domain.EstimatedCalls{Value: bd.n, Known: true, Basis: domain.CompactionBasisObserved, Samples: 1}
		rec := domain.EvaluateCompactionEconomics(in)
		if rec.Verdict != domain.CompactionVerdictCompact {
			t.Fatalf("boundary B=%d: verdict %s/%s, want compact", bd.b, rec.Verdict, rec.Reason)
		}
		totalCost += float64(rec.EstimatedCompactionCostMicros)
		totalSaving += float64(rec.EstimatedFutureSavingsMicros)
	}
	net := totalSaving - totalCost
	reduction := 100 * net / costWithout

	if math.Abs(net/perMillion-5.844951) > 0.002 {
		t.Errorf("net = $%.6f, want $5.844951", net/perMillion)
	}
	// 24.0% of the money against 39.0% of the tokens. THE TWO MUST NEVER BE
	// REPORTED AS ONE NUMBER, and this assertion is what fails if they are.
	if math.Abs(reduction-24.04) > 0.1 {
		t.Errorf("cost reduction = %.2f%%, want 24.04%%", reduction)
	}
	const tokenReduction = 39.0
	if math.Abs(reduction-tokenReduction) < 5 {
		t.Errorf("the cost reduction (%.2f%%) must stay clearly distinct from the token reduction (%.1f%%)",
			reduction, tokenReduction)
	}
}
