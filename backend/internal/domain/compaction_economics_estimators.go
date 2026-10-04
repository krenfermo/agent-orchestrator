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
//
// v2 (P7.2B2.1) is S's rule, not a constant: UNKNOWN below three independent
// sessions instead of the most expensive observation, the MAXIMUM above it
// instead of the mean, the look-ahead filter on the observation's time, and an
// EXACT (harness, model) cohort. A v1 verdict and a v2 verdict can disagree
// about S on identical evidence, so they must never share a label.
const CompactionEstimatorsVersion = "compaction-estimators/v2"

// compactionReattachFactor scales a compacted conversation's LAST KNOWN post size
// up to what the first call after the NEXT compaction actually pays for.
//
// The harness re-attaches material beyond the summary -- files, the preserved
// segment -- so the post-compaction context is reliably larger than
// prefix + postTokens + prompt.
//
// THE DERIVATION MUST BE OUT OF SAMPLE, AND THAT IS WHY THIS CONSTANT IS NOT 1.19.
//
// The tempting derivation divides each boundary's own measured A by its own
// reported postTokens:
//
//	compact 1  57,803 / (37,379 + 10,956 + 1,181) = 1.1673
//	compact 2  61,099 / (37,379 + 13,240 + 1,062) = 1.1822
//
// Both of those use the boundary's OWN post size, which is the one number a
// prediction about that boundary cannot have: it is produced BY the compaction
// being predicted. A factor fitted that way is fitted in sample, and it is
// optimistic out of sample -- which is the direction that compacts.
//
// The corpus supports exactly ONE honest prediction: compact 2's A from compact
// 1's post size, which is what the estimator actually has in hand.
//
//	61,099 / (37,379 + 10,956 + 1,062) = 61,099 / 49,397 = 1.2369
//
// 1.25 is that, rounded up. It over-estimates the one prediction it can be
// checked against by 1.06%, and it would have over-estimated it by 4.9% at 1.19
// in the wrong direction. ONE out-of-sample point from one fixture session is a
// very thin anchor; it is the conservative end of what exists, and it should be
// re-derived the moment there is a second.
const compactionReattachFactor = 1.25

// minSummaryPriorSessions is how many INDEPENDENT sessions the summary prior
// requires before it will produce a figure at all.
//
// Below it the answer is UNKNOWN, not a cheaper statistic. One or two
// observations are a point estimate wearing a statistic's clothing, and the
// whole purpose of the shadow phase is to find out what the distribution is
// rather than to assume one -- a prior built from a single fixture session would
// be exactly the fabricated precision this design keeps refusing.
//
// INDEPENDENT means distinct sessions. Three boundaries of one session are one
// sample: they share a harness, a project, a task shape and a CLAUDE.md, and the
// quantity being estimated is how much a summarizer generates, which those
// things move together.
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
	SessionID   string
	Compactions int
	// Harness and ModelID are the economic cohort this session belongs to,
	// exactly as AO recorded them: the binding's harness and the model the
	// session was running when it compacted, in the ledger's own spelling. Empty
	// means the session cannot be placed in ONE cohort -- its boundaries named
	// different models, or none -- and such an observation is never used.
	Harness string
	ModelID string
	// UnattributedOutputTokens is the harness's own output total minus what AO's
	// ledger accounts for. It INCLUDES reasoning, which is most of what a
	// summarization turn generates and none of what its text contains -- the
	// reason a figure derived from the summary's characters understated it
	// roughly twofold.
	UnattributedOutputTokens int64
	// ObservedAt is when this session's evidence became observable. A prior may
	// only use observations that predate the decision it informs; an observation
	// with no timestamp cannot be ordered against one and is inadmissible rather
	// than assumed old.
	ObservedAt *time.Time
}

// CompactionSummaryEstimatorInput is what estimating S is allowed to see.
type CompactionSummaryEstimatorInput struct {
	// ExcludeSessionID is the session being judged. S can NEVER come from it:
	// the figure is only measurable from a harness's end-of-session rollup, and
	// a running session has no rollup. This is a structural property of the
	// signal, not a precaution.
	ExcludeSessionID string
	Observations     []CompactionSummarySessionObservation
	// Harness and ModelID are the cohort being estimated FOR: the judged
	// session's harness and model. Only observations whose identity matches both
	// EXACTLY are admissible. No normalization and no alias resolution: a
	// summarizer on another model or another harness generates a different
	// amount, and mixing cohorts to reach the minimum sooner would be a prior
	// that is optimistic for whichever cohort is the more expensive one. Either
	// one empty, or the parser's "unknown" model placeholder, is an unidentified
	// cohort and the answer is UNKNOWN.
	Harness string
	ModelID string
	// DecisionAt is when the verdict is being taken. Observations at or after it
	// are the future and are dropped, the same strict comparison the
	// context-after estimator uses. A zero DecisionAt disables the filter, which
	// is only correct for a caller that has already frozen its input -- every
	// production path sets it.
	DecisionAt time.Time
}

// EstimateSummaryTokens predicts how many tokens the harness will GENERATE
// producing the summary -- the term that is priced at output rates and is
// therefore the largest single cost of compacting.
//
// It is structurally a cross-session prior over ONE exact (harness, model)
// cohort. Below minSummaryPriorSessions independent sessions it returns UNKNOWN,
// and at or above it the most expensive per-compaction figure -- never a
// constant. There is no default
// summary size in this file, and that is on purpose: the design that guessed one
// guessed 4,600 tokens by counting the characters of the summary text, and the
// measured figure is roughly twice that because most of what a summarizer
// generates is thinking, which the text does not contain.
func EstimateSummaryTokens(in CompactionSummaryEstimatorInput) EstimatedTokens {
	if !summaryCohortIdentified(in.Harness, in.ModelID) {
		return EstimatedTokens{Basis: CompactionBasisNone}
	}
	// Deduplicated by session, so a session that contributed several sources
	// counts once. The quantity is per-compaction, so a session that compacted
	// three times contributes one sample of its own average -- never three.
	perSession := map[string]int64{}
	for _, o := range in.Observations {
		// THE COHORT, exact. Checked first so an observation from another model
		// or harness cannot reach the sample count at all.
		if o.Harness != in.Harness || o.ModelID != in.ModelID {
			continue
		}
		if o.SessionID == "" {
			// An observation AO cannot attribute to a session cannot be counted
			// as an independent one.
			continue
		}
		if o.SessionID == in.ExcludeSessionID {
			continue
		}
		if o.Compactions <= 0 || o.UnattributedOutputTokens <= 0 {
			continue
		}
		// The look-ahead filter, strict and applied here as well as in the
		// caller: an observation without a timestamp cannot be ordered against
		// the decision, and one at or after it is the future.
		if !in.DecisionAt.IsZero() {
			if o.ObservedAt == nil || !o.ObservedAt.Before(in.DecisionAt) {
				continue
			}
		}
		perCompaction := o.UnattributedOutputTokens / int64(o.Compactions)
		if existing, seen := perSession[o.SessionID]; !seen || perCompaction > existing {
			perSession[o.SessionID] = perCompaction
		}
	}
	// BELOW THE MINIMUM THE ANSWER IS UNKNOWN, not a cheaper statistic.
	if len(perSession) < minSummaryPriorSessions {
		return EstimatedTokens{Basis: CompactionBasisNone, Samples: len(perSession)}
	}
	// THE MAXIMUM, not the mean. The estimate enters the cost of compacting, so
	// underestimating it makes compaction look cheaper than it is -- the one
	// direction this whole design refuses. The maximum never understates any
	// session AO has actually observed, and with a cohort this small it is the
	// most conservative choice that is not absurd. A percentile becomes the
	// right answer when there are enough sessions for one to mean something;
	// that change bumps CompactionEstimatorsVersion.
	var value int64
	for _, v := range perSession {
		if v > value {
			value = v
		}
	}
	return EstimatedTokens{
		Value:   value,
		Known:   true,
		Basis:   CompactionBasisCrossSessionPrior,
		Samples: len(perSession),
	}
}

// summaryCohortIdentified reports whether a (harness, model) pair names a cohort
// at all. The parser records the literal "unknown" when a transcript never
// named its model, and two sessions that both said nothing are not thereby the
// same economic population.
func summaryCohortIdentified(harness, modelID string) bool {
	return harness != "" && modelID != "" && modelID != "unknown"
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

// MinSummaryPriorSessions exposes the minimum independent-session count so a
// readback can report "N of M needed" without carrying its own copy of M. A
// second copy is how a screen starts disagreeing with the rule it describes.
func MinSummaryPriorSessions() int { return minSummaryPriorSessions }
