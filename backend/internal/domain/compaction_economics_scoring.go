package domain

// compaction_economics_scoring.go -- scoring a shadow verdict against what
// actually happened, and being honest about which verdicts can never be scored.
//
// A cohort of predictions is not evidence. It becomes evidence when a prediction
// meets an outcome, and the point of this file is that only SOME of them ever
// can -- which is a property of the world and not a gap to close with a
// statistic.
//
// THE ASYMMETRY, STATED BEFORE ANYTHING ELSE.
//
// A COMPACT verdict on a compaction that then happened is fully scorable: AO
// observes the boundary's own preTokens, the first post-compaction call's
// context, the calls that followed, and the session's residual once it ends. The
// realized profit or loss is arithmetic on measurements, and a COMPACT verdict
// that lost money is a FALSE POSITIVE with no room to argue.
//
// A SKIP verdict on a compaction that then did not happen has NO OBSERVABLE
// COUNTERFACTUAL. AO never learns what A or S would have been. "The skip was
// correct" is therefore never measured, only predicted, and false negatives are
// structurally unobservable without an A/B. This file refuses to call those
// skips correct: they are classified unscored_counterfactual, and the margin
// recorded on each one is a BOUND on how wrong they could be, not a measurement
// of how wrong they were.
//
// There is one lucky exception and it is the most valuable row in the cohort: a
// SKIP verdict on a compaction that happened ANYWAY, because the policy asked for
// it and the shadow gate governs nothing. That is a scorable skip -- exactly the
// shape the two measured compactions have -- and it is the only way a skip is
// ever vindicated by a measurement rather than by its own forecast.
//
// NO LOOK-AHEAD, BY CONSTRUCTION. The scorer takes a RECORD that was already
// written and an OUTCOME observed afterwards, as two separate arguments, and
// produces a third value. It cannot write back into the record and the record
// cannot have been computed from the outcome. The gate and the scorer are
// different functions taking different inputs, and that separation is the only
// thing that keeps the cohort's accuracy from being an artefact.

// CompactionScoreOutcome is what a verdict turned out to be worth. Closed enum.
type CompactionScoreOutcome string

// The outcomes.
const (
	// CompactionScoreTruePositive is a COMPACT verdict on a compaction that
	// happened and made money.
	CompactionScoreTruePositive CompactionScoreOutcome = "true_positive"
	// CompactionScoreFalsePositive is a COMPACT verdict on a compaction that
	// happened and lost money. This is the one the enforcement criterion
	// requires zero of.
	CompactionScoreFalsePositive CompactionScoreOutcome = "false_positive"
	// CompactionScoreSkipVindicated is a SKIP verdict on a compaction that
	// happened anyway and lost money. The only kind of skip a MEASUREMENT ever
	// confirms.
	CompactionScoreSkipVindicated CompactionScoreOutcome = "skip_vindicated"
	// CompactionScoreSkipRefuted is a SKIP verdict on a compaction that happened
	// anyway and made money: a measured false negative, and the only way one is
	// ever visible.
	CompactionScoreSkipRefuted CompactionScoreOutcome = "skip_refuted"
	// CompactionScoreUnscoredCounterfactual is a SKIP on a compaction that did
	// not happen. NOT a correct skip. It is the outcome that exists so nobody
	// can quietly count these as successes and report a precision no experiment
	// produced.
	CompactionScoreUnscoredCounterfactual CompactionScoreOutcome = "unscored_counterfactual"
	// CompactionScoreUnscorable is an UNKNOWN verdict, or an outcome missing a
	// measurement the score needs. A verdict that declined to predict cannot be
	// right or wrong.
	CompactionScoreUnscorable CompactionScoreOutcome = "unscorable"
)

// Valid reports whether an outcome is part of the closed enum.
func (o CompactionScoreOutcome) Valid() bool {
	switch o {
	case CompactionScoreTruePositive, CompactionScoreFalsePositive,
		CompactionScoreSkipVindicated, CompactionScoreSkipRefuted,
		CompactionScoreUnscoredCounterfactual, CompactionScoreUnscorable:
		return true
	default:
		return false
	}
}

// CompactionObservedOutcome is what was measured AFTER the verdict was recorded.
//
// Every field is an observation, and Compacted is what decides whether there is
// anything to observe at all. It is assembled by a caller that reads boundaries
// and calls whose timestamps are LATER than the verdict's -- the opposite filter
// to the one the gate uses, and deliberately a different function.
type CompactionObservedOutcome struct {
	// Compacted is whether a compaction actually happened for this decision.
	// False means the conversation was left alone, whatever the verdict said.
	Compacted bool
	// ContextBeforeTokens is the boundary's own preTokens: the conversation the
	// summarizer actually read.
	ContextBeforeTokens int64
	// ContextAfterTokens is the FIRST post-compaction call's context, which is
	// what the gate's A was predicting.
	ContextAfterTokens int64
	// SummaryTokens is what the harness actually generated, from the session's
	// residual once the session ended. Zero means the session has not finished
	// and S is not yet measurable -- which makes the score incomplete rather
	// than zero-cost.
	SummaryTokens      int64
	SummaryTokensKnown bool
	// RemainingCalls is how many calls the session actually made after the first
	// post-compaction one.
	RemainingCalls      int64
	RemainingCallsKnown bool
	// PromptTokens is the delta actually sent, if it differed from the estimate.
	PromptTokens int64
	// StablePrefixTokens is the prefix actually observed on the post-compaction
	// call. Measured identically on both of the compactions AO has.
	StablePrefixTokens int64
	// Rates are the rates to price the realized outcome with. They should be the
	// SAME card version the verdict recorded; a caller pricing a realized
	// outcome at a different card is comparing two currencies and the score says
	// so through RatesMatchVerdict.
	Rates      ModelRateView
	RatesKnown bool
	// CacheWriteLifetime is the lifetime the rewrite was actually created at.
	CacheWriteLifetime CacheWriteLifetime
}

// CompactionVerdictScore is one verdict measured against one outcome.
type CompactionVerdictScore struct {
	Outcome CompactionScoreOutcome
	// Scored is whether a realized profit-or-loss was computed at all. False for
	// every unscored_counterfactual and unscorable row, which is most of a
	// shadow cohort and must be visible as such.
	Scored bool

	// RealizedNetMicros is the measured profit or loss of the compaction that
	// happened: what the calls after it saved, minus what it cost.
	RealizedNetMicros    int64
	RealizedCostMicros   int64
	RealizedSavingMicros int64

	// The estimator errors, each signed so the DIRECTION is legible: positive
	// means the estimate was larger than the measurement.
	ContextAfterErrorTokens  int64
	SummaryTokensErrorTokens int64
	RemainingCallsErrorCalls int64
	ContextAfterErrorKnown   bool
	SummaryTokensErrorKnown  bool
	RemainingCallsErrorKnown bool
	// RatesMatchVerdict is false when the outcome was priced at a different rate
	// card version from the verdict. The score is still computed; it is simply
	// not a comparison of like with like, and a cohort statistic must exclude it.
	RatesMatchVerdict bool
}

// ScoreCompactionVerdict measures one recorded verdict against one observed
// outcome.
//
// Pure, and deliberately unable to alter the record it is given.
func ScoreCompactionVerdict(record CompactionEconomicsRecord, observed CompactionObservedOutcome) CompactionVerdictScore {
	score := CompactionVerdictScore{
		Outcome:           CompactionScoreUnscorable,
		RatesMatchVerdict: observed.RatesKnown && observed.Rates.Version == record.PricingVersion,
	}

	// An UNKNOWN verdict declined to predict. It cannot be right or wrong, and
	// counting it either way would turn a refusal into a data point.
	if record.Verdict == CompactionVerdictUnknown {
		return score
	}
	// A SKIP with no compaction is the structurally unobservable case.
	if !observed.Compacted {
		if record.Verdict == CompactionVerdictSkip {
			score.Outcome = CompactionScoreUnscoredCounterfactual
		}
		// A COMPACT verdict whose compaction never happened is equally
		// unobservable -- in shadow mode that is EVERY compact verdict on a run
		// with the knob off -- and stays unscorable rather than being counted as
		// a miss.
		return score
	}

	// From here a compaction actually happened, so the estimator errors are
	// measurable whether or not the money is.
	if observed.ContextAfterTokens > 0 && record.ContextAfterBasis != CompactionBasisNone {
		score.ContextAfterErrorTokens = record.EstimatedContextAfterTokens - observed.ContextAfterTokens
		score.ContextAfterErrorKnown = true
	}
	if observed.SummaryTokensKnown && record.SummaryBasis != CompactionBasisNone {
		score.SummaryTokensErrorTokens = record.EstimatedSummaryTokens - observed.SummaryTokens
		score.SummaryTokensErrorKnown = true
	}
	if observed.RemainingCallsKnown && record.RemainingCallsBasis != CompactionBasisNone {
		score.RemainingCallsErrorCalls = record.EstimatedRemainingCalls - observed.RemainingCalls
		score.RemainingCallsErrorKnown = true
	}

	// The money needs every measured term. A session that has not ended has no
	// residual, so S is unknown, so the realized cost is unknown -- and an
	// unknown cost must not be read as a cheap one.
	if !observed.RatesKnown || !observed.SummaryTokensKnown || !observed.RemainingCallsKnown {
		return score
	}
	cacheWriteRate, ok := observed.Rates.CacheWriteRateFor(observed.CacheWriteLifetime)
	if !ok || observed.Rates.CacheReadPerMTok <= 0 || observed.Rates.OutputPerMTok <= 0 {
		return score
	}
	rewritten := observed.ContextAfterTokens - observed.StablePrefixTokens - observed.PromptTokens
	if rewritten < 0 {
		return score
	}
	// The same four terms the forward model uses, on measurements instead of
	// estimates. Using a different decomposition here would make the score
	// incomparable with the prediction it is scoring.
	cost := microsPerMillionTokens(observed.ContextBeforeTokens, observed.Rates.CacheReadPerMTok) +
		microsPerMillionTokens(observed.SummaryTokens, observed.Rates.OutputPerMTok) +
		microsPerMillionTokens(rewritten, cacheWriteRate) -
		microsPerMillionTokens(observed.ContextBeforeTokens-observed.StablePrefixTokens, observed.Rates.CacheReadPerMTok)
	reduction := (observed.ContextBeforeTokens + observed.PromptTokens) - observed.ContextAfterTokens
	saving := microsPerMillionTokens(reduction, observed.Rates.CacheReadPerMTok) * float64(observed.RemainingCalls)
	if !allFinite(cost, saving) {
		return score
	}
	score.RealizedCostMicros = roundMicros(cost)
	score.RealizedSavingMicros = roundMicros(saving)
	score.RealizedNetMicros = score.RealizedSavingMicros - score.RealizedCostMicros
	score.Scored = true

	profitable := score.RealizedNetMicros > 0
	switch {
	case record.Verdict == CompactionVerdictCompact && profitable:
		score.Outcome = CompactionScoreTruePositive
	case record.Verdict == CompactionVerdictCompact:
		score.Outcome = CompactionScoreFalsePositive
	case profitable:
		score.Outcome = CompactionScoreSkipRefuted
	default:
		score.Outcome = CompactionScoreSkipVindicated
	}
	return score
}

// CompactionScoreboard counts outcomes across a cohort.
type CompactionScoreboard struct {
	Total                  int
	Scored                 int
	TruePositives          int
	FalsePositives         int
	SkipsVindicated        int
	SkipsRefuted           int
	UnscoredCounterfactual int
	Unscorable             int
	// MismatchedRates counts scored rows priced at a different rate card version
	// from the verdict. They are excluded from ScoredSessionsComparable.
	MismatchedRates int
	// RealizedNetMicros sums only the rows that were actually scored.
	RealizedNetMicros int64
}

// BuildCompactionScoreboard folds scores into counts.
func BuildCompactionScoreboard(scores []CompactionVerdictScore) CompactionScoreboard {
	var out CompactionScoreboard
	for _, s := range scores {
		out.Total++
		if s.Scored {
			out.Scored++
			out.RealizedNetMicros += s.RealizedNetMicros
			if !s.RatesMatchVerdict {
				out.MismatchedRates++
			}
		}
		switch s.Outcome {
		case CompactionScoreTruePositive:
			out.TruePositives++
		case CompactionScoreFalsePositive:
			out.FalsePositives++
		case CompactionScoreSkipVindicated:
			out.SkipsVindicated++
		case CompactionScoreSkipRefuted:
			out.SkipsRefuted++
		case CompactionScoreUnscoredCounterfactual:
			out.UnscoredCounterfactual++
		default:
			out.Unscorable++
		}
	}
	return out
}

// EnforcementReady reports whether a scored cohort meets the ZERO-FALSE-POSITIVE
// half of the enforcement criterion, and whether the question is answerable.
//
// Two returns, again: a cohort with no scored compactions has not passed this
// test, it has not taken it. Reporting "ready" for an empty scoreboard is
// exactly the mistake a shadow phase exists to prevent -- verdicts accumulate
// while outcomes do not, and a count of predictions can look like progress for
// months.
//
// It deliberately does NOT check the sample-size conditions (at least 5 scored
// compactions across 3 sessions and 2 projects). Those are about the cohort's
// shape, which this type does not see; this answers only "did any recommendation
// lose money".
func (s CompactionScoreboard) EnforcementReady() (bool, bool) {
	if s.Scored == 0 {
		return false, false
	}
	return s.FalsePositives == 0, true
}
