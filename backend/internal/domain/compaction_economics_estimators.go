package domain

import "time"

// compaction_economics_estimators.go -- the three predictions, each pure, each
// carrying its own provenance.
//
// The gate's observed inputs -- B, P, delta, the rates, the cache lifetimes --
// are read. These three are PREDICTED, and that difference is the whole reason
// every one of them returns a basis and a sample count beside its value. A
// verdict computed from a one-session prior has to be re-identifiable as such
// later, or the cohort cannot be recalibrated and can only be discarded.
//
// THE LOOK-AHEAD TRAP, AND WHY THE FILTER IS IN HERE AND NOT ONLY IN THE CALLER.
// P7.1 records a boundary's postTokens for every compaction, including the very
// one a verdict is about. An estimator that read the current cycle's boundary
// would "predict" A from the answer and produce a cohort whose accuracy is an
// artefact. The caller is expected to pass only admissible observations -- and
// this file filters again anyway, on the observation's own timestamp, because a
// guard that exists in one place is a guard one refactor can remove. A boundary
// with no timestamp at all is INADMISSIBLE rather than assumed old: unplaceable
// is not early.

// CompactionEstimatorsVersion identifies the three estimators together. Bumping
// it is how a cohort computed with recalibrated constants is kept apart from one
// computed with these.
const CompactionEstimatorsVersion = "compaction-estimators/v1"

// compactionReattachFactor scales a compacted conversation's own reported size
// up to what the FIRST call after it actually pays for.
//
// The harness re-attaches material beyond the summary -- files, the preserved
// segment -- so the post-compaction context is reliably larger than
// prefix + postTokens + prompt. Measured on the only two boundaries AO has:
//
//	compact 1  57,803 / (37,379 + 10,956 + 1,181) = 1.1673
//	compact 2  61,099 / (37,379 + 13,240 + 1,062) = 1.1822
//
// 1.19 is the LARGER of the two, rounded up. Both are then over-estimated
// (+1.9% and +0.7%), which is the conservative direction: a larger A means a
// smaller reduction means a verdict further from COMPACT. Two observations from
// one fixture session is a thin anchor and this constant should be re-derived
// the moment there is a third.
const compactionReattachFactor = 1.19

// minSummaryPriorSessions is how many completed sessions the summary prior wants
// before it will use their mean.
//
// Below it the estimator uses the single most expensive observation instead of
// an average, because an average over one or two sessions is a point estimate
// wearing a statistic's clothing, and the expensive end is the direction that
// skips.
const minSummaryPriorSessions = 3

// remainingCallsFirstCycleRatio and remainingCallsLaterCycleRatio are the
// remaining-calls estimator's two constants.
//
// Anchored on the LOWER end of the observed ratios rather than their mean:
// across every fix cycle in AO's ledger the first-repair ratio ranges
// 0.25..2.11 and the later-cycle ratio 0.71..0.83, and taking the bottom of
// each is what makes the estimator's error profile skew toward
// UNDERprediction. That matters because the two directions are not
// symmetrical: underpredicting costs a saving nobody notices, and
// overpredicting authorizes a compaction that never amortizes.
const (
	remainingCallsFirstCycleRatio = 0.25
	remainingCallsLaterCycleRatio = 0.80
)

// remainingCallsCeiling caps the estimate at the median repair cycle AO has
// actually observed.
//
// p50 of the eight fix-cycle sizes in the ledger -- 5, 5, 7, 19, 20, 25, 30, 57
// -- is 19.5, floored to 19. The cap ONLY EVER LOWERS an estimate, which is why
// it is safe to carry as a constant rather than as a query: getting it wrong in
// the high direction does nothing, and the alternative is a corpus scan on every
// fix cycle to compute a number that moves once a month.
//
// It is derived from Task-shaped runs only. No Autonomous or Master run has a
// repair cycle in the ledger at all, and those are the runs with the most to
// save, so this constant is the one most likely to be wrong for them -- wrong in
// the safe direction.
const remainingCallsCeiling = 19

// CompactionContextAfterEstimatorInput is what estimating A is allowed to see.
type CompactionContextAfterEstimatorInput struct {
	// StablePrefixTokens is P, observed.
	StablePrefixTokens int64
	// PromptTokens is delta, the prompt AO is about to send.
	PromptTokens int64
	// Boundaries are compactions already observed for THIS session. Only those
	// strictly before DecisionAt are admissible.
	Boundaries []CompactionBoundary
	// DecisionAt is when the decision is being taken. The filter is strict:
	// a boundary observed at exactly this instant is the current one.
	DecisionAt time.Time
}

// EstimateContextAfter predicts the context of the FIRST call after a
// compaction.
//
// Priority: this session's own most recent admissible boundary, which is the
// strongest prior available because it is about the conversation being judged.
// There is deliberately NO fallback constant. A cross-session percentile would
// be the next best thing and it is not implemented here: the population is one
// session, so a "p75" over it would be a single observation with a statistic's
// name on it, and the honest answer is UNKNOWN. That is also why this estimator
// is cheap -- it reads one session's own observations and never the corpus.
func EstimateContextAfter(in CompactionContextAfterEstimatorInput) EstimatedTokens {
	admissible := admissibleBoundaries(in.Boundaries, in.DecisionAt)
	if len(admissible) == 0 {
		return EstimatedTokens{Basis: CompactionBasisNone}
	}
	// The most recent one: a session that has compacted twice tells AO most
	// about its third compaction through its second.
	latest := admissible[0]
	for _, b := range admissible[1:] {
		if b.ObservedAt.After(*latest.ObservedAt) {
			latest = b
		}
	}
	if latest.PostTokens <= 0 {
		return EstimatedTokens{Basis: CompactionBasisNone}
	}
	base := in.StablePrefixTokens + latest.PostTokens + maxInt64(in.PromptTokens, 0)
	value := int64(float64(base)*compactionReattachFactor + 0.5)
	return EstimatedTokens{
		Value:   value,
		Known:   true,
		Basis:   CompactionBasisSessionPriorBoundary,
		Samples: len(admissible),
	}
}

// admissibleBoundaries keeps only the observations a decision at DecisionAt is
// allowed to have seen.
//
// Two rejections, both deliberate: a boundary with no timestamp cannot be
// ordered against the decision, and a boundary at or after the decision instant
// is the future. Neither is clamped into admissibility.
func admissibleBoundaries(in []CompactionBoundary, decisionAt time.Time) []CompactionBoundary {
	out := make([]CompactionBoundary, 0, len(in))
	for _, b := range in {
		if b.ObservedAt == nil {
			continue
		}
		if !b.ObservedAt.Before(decisionAt) {
			continue
		}
		out = append(out, b)
	}
	return out
}

// CompactionSummarySessionObservation is one COMPLETED session's evidence about
// what a compaction generates.
//
// UnattributedOutputTokens is the harness's own output total minus what AO's
// ledger accounts for. On a session that never compacted this is noise near
// zero; on one that compacted it is the summaries, and it is an order of
// magnitude larger. It is an UPPER BOUND on the summarization rather than a
// measurement of it, which is exactly why it is the figure a fail-closed gate
// wants.
type CompactionSummarySessionObservation struct {
	SessionID                string
	Compactions              int
	UnattributedOutputTokens int64
}

// CompactionSummaryEstimatorInput is what estimating S is allowed to see.
type CompactionSummaryEstimatorInput struct {
	// ExcludeSessionID is the session being judged. S can NEVER come from it:
	// the figure is only measurable from a harness's end-of-session rollup, and
	// a running session has no rollup. This is a structural property of the
	// signal, not a precaution.
	ExcludeSessionID string
	Observations     []CompactionSummarySessionObservation
}

// EstimateSummaryTokens predicts how many tokens the harness will GENERATE
// producing the summary -- the term that is priced at output rates and is
// therefore the largest single cost of compacting.
//
// It is structurally a cross-session prior. Below minSummaryPriorSessions it
// takes the most expensive observation rather than a mean, and with no
// observation it returns UNKNOWN rather than a constant. There is no default
// summary size in this file, and that is on purpose: the design that guessed one
// guessed 4,600 tokens by counting the characters of the summary text, and the
// measured figure is roughly twice that because most of what a summarizer
// generates is thinking, which the text does not contain.
func EstimateSummaryTokens(in CompactionSummaryEstimatorInput) EstimatedTokens {
	var perCompaction []int64
	for _, o := range in.Observations {
		if o.SessionID != "" && o.SessionID == in.ExcludeSessionID {
			continue
		}
		if o.Compactions <= 0 || o.UnattributedOutputTokens <= 0 {
			continue
		}
		perCompaction = append(perCompaction, o.UnattributedOutputTokens/int64(o.Compactions))
	}
	if len(perCompaction) == 0 {
		return EstimatedTokens{Basis: CompactionBasisNone}
	}
	value := perCompaction[0]
	if len(perCompaction) >= minSummaryPriorSessions {
		var sum int64
		for _, v := range perCompaction {
			sum += v
		}
		value = sum / int64(len(perCompaction))
	} else {
		for _, v := range perCompaction[1:] {
			if v > value {
				value = v
			}
		}
	}
	return EstimatedTokens{
		Value:   value,
		Known:   true,
		Basis:   CompactionBasisCrossSessionPrior,
		Samples: len(perCompaction),
	}
}

// CompactionCycleCalls is how many provider calls one already-completed cycle of
// this run made.
type CompactionCycleCalls struct {
	Cycle int
	Calls int64
}

// CompactionRemainingCallsEstimatorInput is what estimating N is allowed to see.
type CompactionRemainingCallsEstimatorInput struct {
	// CurrentCycle is the cycle being dispatched: 1 for the first repair. Only
	// cycles STRICTLY BELOW it are admissible -- the current one has not
	// happened yet, and counting it would be reading the answer.
	CurrentCycle int
	CycleCalls   []CompactionCycleCalls
}

// EstimateRemainingCalls predicts how many calls the session will still make
// after the first post-compaction one.
//
// Two forms, both within-run and both deterministic:
//
//	cycle 1   floor(0.25 * calls in cycle 0)
//	cycle k>1 floor(0.80 * min(calls in cycles 1..k-1))
//
// then capped at remainingCallsCeiling. Validated against every fix cycle in
// AO's ledger -- 8 samples across 5 runs -- where it is exact twice, over by at
// most 3 calls (+25%), and under by as much as 50 on the one run whose repair
// phase was twice its base phase. The asymmetry is the point: overprediction
// authorizes a compaction that will not amortize, underprediction only misses a
// saving.
//
// WHAT IT DELIBERATELY DOES NOT COUNT. Calls in LATER fix cycles, which a
// compaction's benefit really does reach if no second compaction happens, and
// the cycles the policy would still allow. Both would raise the estimate; both
// would also be claiming a benefit the next COMPACT decision can annul. Counting
// only the current cycle understates, on purpose.
func EstimateRemainingCalls(in CompactionRemainingCallsEstimatorInput) EstimatedCalls {
	if in.CurrentCycle <= 0 {
		return EstimatedCalls{Basis: CompactionBasisNone}
	}
	byCycle := map[int]int64{}
	for _, c := range in.CycleCalls {
		if c.Cycle < 0 || c.Cycle >= in.CurrentCycle || c.Calls <= 0 {
			continue
		}
		// A cycle reported twice is summed, not replaced: two windows for one
		// cycle are two parts of the same cycle's work.
		byCycle[c.Cycle] += c.Calls
	}
	var raw float64
	var basis CompactionEstimatorBasis
	samples := 0
	if in.CurrentCycle == 1 {
		base, ok := byCycle[0]
		if !ok {
			return EstimatedCalls{Basis: CompactionBasisNone}
		}
		raw, basis, samples = float64(base)*remainingCallsFirstCycleRatio, CompactionBasisPriorCycleDiscounted, 1
	} else {
		var smallest int64
		for cycle, calls := range byCycle {
			if cycle == 0 {
				continue
			}
			if smallest == 0 || calls < smallest {
				smallest = calls
			}
			samples++
		}
		if smallest == 0 {
			return EstimatedCalls{Basis: CompactionBasisNone}
		}
		raw, basis = float64(smallest)*remainingCallsLaterCycleRatio, CompactionBasisPriorCycleMinDiscounted
	}
	value := int64(raw)
	capped := false
	if value > remainingCallsCeiling {
		value, capped = remainingCallsCeiling, true
	}
	if value < 0 {
		value = 0
	}
	return EstimatedCalls{
		Value:      value,
		Known:      true,
		Basis:      basis,
		Samples:    samples,
		CapApplied: capped,
	}
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
