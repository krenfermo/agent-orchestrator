package domain

import "math"

// compaction_economics.go -- P7.2B2. The shadow economic gate's arithmetic.
//
// A compaction is not a discount on the next call. It is a PURCHASE: an
// output-priced summary plus a cache-write-priced rewrite now, in exchange for a
// cache-read-priced discount on every call that comes after. Whether the
// purchase pays depends on how many calls come after -- the one variable the
// lifecycle policy never looks at.
//
// THIS FILE DECIDES NOTHING. It computes a verdict, and in P7.2B2 nothing reads
// it: SessionLifecyclePolicy is untouched, maybeCompactBeforeFix is untouched,
// and a compaction that happens today still happens, byte for byte. The verdict
// exists to be recorded, counted and scored against real work, so that a later
// decision to let it govern anything can be made from evidence instead of from
// this comment. See workflow/compaction_economics.go for the call site that
// throws the answer away on purpose.
//
// PURE. No IO, no clock, no store, no randomness. The input is a frozen struct
// the caller assembles; the same input always produces the same record. That is
// what makes every verdict in the cohort recomputable at a different safety
// factor months later, and it is what makes the look-ahead guard testable: a
// function that cannot reach the database cannot accidentally read the boundary
// belonging to the very compaction it is judging.
//
// FAIL CLOSED, AND IN ONE DIRECTION ONLY. Every missing input produces UNKNOWN
// or SKIP; there is no input whose absence makes compaction look better. Every
// estimate that can be biased is biased toward a LARGER A, a LARGER S, a SMALLER
// N and a SMALLER D -- all four push away from compacting.

// CompactionEconomicsGateVersion identifies the arithmetic. A record carries it
// so a cohort computed by two builds is never silently pooled.
const CompactionEconomicsGateVersion = "compaction-economics/v1"

// CompactionEconomicsPayloadVersion is the durable payload's own version,
// separate from the gate's because a field can be added to the record without
// the formula changing.
const CompactionEconomicsPayloadVersion = "compaction-economics-record/v1"

// DefaultCompactionSafetyFactor is how much better than break-even the forecast
// must be before the arithmetic would recommend compacting.
//
// 1.5, and it is NOT a tuned number -- 1.0 produces the same verdict on every
// historical point AO has. It is carried here, as a named constant with a
// version beside it, because the alternative is a bare 1.5 in an expression and
// a cohort nobody can re-score. The justification is the estimator's own worst
// case: the largest overprediction the remaining-calls estimator has ever made
// is +25%, so 1.25 covers exactly that and nothing else, and A and S are both
// one-observation priors capable of being optimistic at the same time.
//
// It is an INPUT, not a law: CompactionEconomicsInput carries its own factor and
// every record records which one it used.
const DefaultCompactionSafetyFactor = 1.5

// DefaultCompactionMinReductionFraction is the smallest context reduction worth
// buying, as a fraction of the conversation being compacted.
//
// 0.25, anchored on a measurement: the second of the two real compactions AO has
// observed reduced its conversation by 11.1% and lost money, because most of
// what it was asked to compact was the PREVIOUS summary plus the fix prompt AO
// had just prepended. A summarizer cannot compress a summary. This floor
// rejects that case before any forecast is consulted.
const DefaultCompactionMinReductionFraction = 0.25

// CompactionEconomicVerdict is what the arithmetic would recommend. Exactly
// three states, and UNKNOWN is one of them rather than an error: a gate that can
// price nothing yet must say so, not guess.
type CompactionEconomicVerdict string

// The three verdicts.
const (
	CompactionVerdictCompact CompactionEconomicVerdict = "compact"
	CompactionVerdictSkip    CompactionEconomicVerdict = "skip"
	CompactionVerdictUnknown CompactionEconomicVerdict = "unknown"
)

// Valid reports whether a verdict is part of the closed enum.
func (v CompactionEconomicVerdict) Valid() bool {
	switch v {
	case CompactionVerdictCompact, CompactionVerdictSkip, CompactionVerdictUnknown:
		return true
	default:
		return false
	}
}

// CompactionEconomicReason is why. Closed enum, deliberately separate from
// SessionLifecycleReason: the shadow gate must not be able to put a value into
// an enum the lifecycle decision validates, because that is the first step
// toward the two sharing a code path.
type CompactionEconomicReason string

// The closed reason set. Keep in sync with Valid.
const (
	// The one reason a COMPACT verdict can carry.
	CompactionReasonPositiveMargin CompactionEconomicReason = "economic_positive_margin"

	// SKIP: the arithmetic ran and came out against compacting.
	//
	// The three are not interchangeable and the distinction is the point of
	// recording a margin at all. Negative means there is no benefit to be had
	// at any call count. InsufficientFutureCalls means the benefit exists but
	// the forecast does not reach break-even. BelowSafetyMargin means it
	// reaches break-even and not the headroom -- the only one of the three that
	// a later, better-calibrated safety factor could turn into a COMPACT, which
	// is exactly the near-miss cohort worth watching.
	CompactionReasonNegative                CompactionEconomicReason = "economic_negative"
	CompactionReasonInsufficientFutureCalls CompactionEconomicReason = "insufficient_future_calls"
	CompactionReasonBelowSafetyMargin       CompactionEconomicReason = "below_safety_margin"
	// CompactionReasonReductionTooSmall is the structural floor of §
	// DefaultCompactionMinReductionFraction: a reduction this small is not
	// worth buying whatever the call forecast says.
	CompactionReasonReductionTooSmall CompactionEconomicReason = "reduction_too_small"
	// CompactionReasonTerminalCycle is the structural rule: the cycle being
	// dispatched is the last one that will ever run, so there are no future
	// cycles into which a compaction could amortize.
	CompactionReasonTerminalCycle CompactionEconomicReason = "terminal_cycle"

	// UNKNOWN: something the arithmetic needs is not knowable here.
	CompactionReasonPricingUnknown         CompactionEconomicReason = "pricing_unknown"
	CompactionReasonTTLAssumed             CompactionEconomicReason = "ttl_assumed"
	CompactionReasonTTLUnknown             CompactionEconomicReason = "ttl_unknown"
	CompactionReasonTTLRateUnknown         CompactionEconomicReason = "ttl_rate_unknown"
	CompactionReasonContextAfterUnknown    CompactionEconomicReason = "context_after_unknown"
	CompactionReasonSummaryCostUnknown     CompactionEconomicReason = "summary_cost_unknown"
	CompactionReasonRemainingCallsUnknown  CompactionEconomicReason = "remaining_calls_unknown"
	CompactionReasonUsageIncomplete        CompactionEconomicReason = "usage_incomplete"
	CompactionReasonInconsistentAccounting CompactionEconomicReason = "inconsistent_accounting"
	// CompactionReasonEvaluationError is the catch-all for an overflow or a
	// non-finite intermediate. It exists so that a defect in this file produces
	// a recorded UNKNOWN rather than a verdict computed from a NaN.
	CompactionReasonEvaluationError CompactionEconomicReason = "evaluation_error"
)

// Valid reports whether a reason is part of the closed enum.
func (r CompactionEconomicReason) Valid() bool {
	switch r {
	case CompactionReasonPositiveMargin,
		CompactionReasonNegative, CompactionReasonInsufficientFutureCalls,
		CompactionReasonBelowSafetyMargin, CompactionReasonReductionTooSmall,
		CompactionReasonTerminalCycle,
		CompactionReasonPricingUnknown, CompactionReasonTTLAssumed,
		CompactionReasonTTLUnknown, CompactionReasonTTLRateUnknown,
		CompactionReasonContextAfterUnknown, CompactionReasonSummaryCostUnknown,
		CompactionReasonRemainingCallsUnknown, CompactionReasonUsageIncomplete,
		CompactionReasonInconsistentAccounting, CompactionReasonEvaluationError:
		return true
	default:
		return false
	}
}

// CompactionPricingStatus is whether the model could be priced at all, recorded
// on every verdict so pricing coverage over a cohort is a count rather than an
// inference.
type CompactionPricingStatus string

// The pricing states.
const (
	CompactionPricingPriced        CompactionPricingStatus = "priced"
	CompactionPricingUnpricedModel CompactionPricingStatus = "unpriced_model"
	// CompactionPricingIncompleteRates is a model the card names but whose
	// rates do not cover what the arithmetic needs -- a missing one-hour
	// cache-write rate being the case that actually occurs.
	CompactionPricingIncompleteRates CompactionPricingStatus = "incomplete_rates"
	// CompactionPricingAssumedTTL is a model whose rates are complete but whose
	// observed spend was priced on an ASSUMED cache lifetime. The currency is
	// wrong, so the arithmetic is refused even though a rate exists.
	CompactionPricingAssumedTTL CompactionPricingStatus = "assumed_ttl"
)

// CompactionEconomicsInput is everything the gate is allowed to see.
//
// It is FROZEN BY CONSTRUCTION: a plain struct of numbers assembled by a caller
// that has already decided which observations are admissible. The gate cannot
// widen it, cannot re-read anything, and therefore cannot reach the one
// observation that would invalidate the whole cohort -- the boundary produced by
// the compaction this verdict is about. See CompactionContextAfterEstimatorInput
// for where that filter actually lives.
type CompactionEconomicsInput struct {
	// --- identity, carried through to the record -------------------------
	WorkflowRunID string
	StepID        string
	Cycle         int
	SessionID     string
	Provider      string
	Harness       string
	// ModelID is the id the SESSION reported, in the harness's own spelling.
	// Never canonicalized: "claude-opus-5[1m]" is looked up as itself and a
	// card that does not cover it leaves the verdict UNKNOWN.
	ModelID string

	// --- frozen policy ---------------------------------------------------
	MaxFixCycles int
	Strategy     string
	// ContextThresholdTokens is the run's own context-pressure precondition.
	// Recorded, never re-applied: the lifecycle decision already compared it,
	// and comparing it twice would let the shadow gate disagree with the
	// decision it is shadowing.
	ContextThresholdTokens int64
	// LifecycleReasons is why the lifecycle decision said COMPACT. Read ONLY by
	// the terminal-cycle rule, which needs to know whether a fix-cycle counter
	// was the sole cause.
	LifecycleReasons     []SessionLifecycleReason
	SafetyFactor         float64
	MinReductionFraction float64

	// --- observed --------------------------------------------------------
	UsageObservable bool
	// ContextBeforeTokens is B: the conversation the compaction will read.
	ContextBeforeTokens int64
	// StablePrefixTokens is P: the part of it that survives, and is therefore
	// still a cache READ rather than a rewrite, on the first call after.
	StablePrefixTokens int64
	// PromptTokensKnown distinguishes "the prompt is empty" from "AO does not
	// know how big it is". Delta shrinks D, so an unknown one is taken as zero,
	// which is the conservative direction.
	PromptTokens      int64
	PromptTokensKnown bool

	// ObservedCacheCreation is this session's cache creation so far, split by
	// the lifetime each entry was created with. It answers one question and one
	// only: which cache-write rate will the rewrite this compaction causes be
	// billed at. An unknown contribution refuses the whole verdict.
	ObservedCacheCreation CacheCreationSplit
	// ObservedCostTTLAssumedTokens and ObservedCostTTLUnknownTokens are the
	// disclosures UsageCost already carries. Non-zero means AO's own figure for
	// this session is in the wrong currency, so no comparison built on it can
	// be trusted -- P7.2B1 exists because of exactly this.
	ObservedCostTTLAssumedTokens int64
	ObservedCostTTLUnknownTokens int64

	// --- rates -----------------------------------------------------------
	Rates      ModelRateView
	RatesKnown bool

	// --- estimates -------------------------------------------------------
	ContextAfter   EstimatedTokens
	SummaryTokens  EstimatedTokens
	RemainingCalls EstimatedCalls
}

// EstimatedTokens is a token figure that may not be knowable, with the provenance
// of the estimate that produced it.
//
// Basis and Samples are not decoration. A verdict computed from a one-session
// prior has to be re-identifiable as such later; that is the difference between
// a cohort that can be recalibrated and one that has to be thrown away.
type EstimatedTokens struct {
	Value   int64
	Known   bool
	Basis   CompactionEstimatorBasis
	Samples int
}

// EstimatedCalls is the same shape for a call count.
type EstimatedCalls struct {
	Value   int64
	Known   bool
	Basis   CompactionEstimatorBasis
	Samples int
	// CapApplied records that a conservative ceiling lowered the raw estimate.
	CapApplied bool
}

// CompactionEstimatorBasis names where an estimate came from. Closed enum: an
// open string here would be the one place content could enter a payload that is
// otherwise all numbers and ids.
type CompactionEstimatorBasis string

// The bases. "none" is what an unknown estimate carries.
const (
	CompactionBasisNone CompactionEstimatorBasis = "none"
	// CompactionBasisObserved is not an estimate at all -- the figure was read.
	CompactionBasisObserved CompactionEstimatorBasis = "observed"
	// CompactionBasisSessionPriorBoundary is this session's OWN earlier
	// compaction, which is the strongest prior available and the only one that
	// is about the conversation actually being judged.
	CompactionBasisSessionPriorBoundary CompactionEstimatorBasis = "session_prior_boundary"
	// CompactionBasisModelPercentile is a high percentile over completed
	// sessions of the same harness and model.
	CompactionBasisModelPercentile CompactionEstimatorBasis = "model_percentile"
	// CompactionBasisCrossSessionPrior is a prior derived from other sessions'
	// end-of-session rollups, which is the only way S is ever knowable.
	CompactionBasisCrossSessionPrior CompactionEstimatorBasis = "cross_session_prior"
	// CompactionBasisPriorCycleDiscounted is the remaining-calls estimator's
	// first-repair-cycle form: a fraction of the base cycle's call count.
	CompactionBasisPriorCycleDiscounted CompactionEstimatorBasis = "prior_cycle_discounted"
	// CompactionBasisPriorCycleMinDiscounted is its later-cycle form: a
	// fraction of the smallest repair cycle this run has already run.
	CompactionBasisPriorCycleMinDiscounted CompactionEstimatorBasis = "prior_cycle_min_discounted"
)

// Valid reports whether a basis is part of the closed enum.
func (b CompactionEstimatorBasis) Valid() bool {
	switch b {
	case CompactionBasisNone, CompactionBasisObserved, CompactionBasisSessionPriorBoundary,
		CompactionBasisModelPercentile, CompactionBasisCrossSessionPrior,
		CompactionBasisPriorCycleDiscounted, CompactionBasisPriorCycleMinDiscounted:
		return true
	default:
		return false
	}
}

// CompactionEconomicsRecord is the verdict and every number behind it.
//
// PRIVACY IS A PROPERTY OF THE TYPE, not of the code that fills it. There is no
// field here that can hold a prompt, a summary, a finding, a command, a path, a
// working directory or a secret, because every field is an int64, a float64, a
// bool, an identifier or a closed-enum value. A leak would require adding a
// field, which is a review a reviewer can actually perform.
//
// Money is in integer MICROS. A float in a durable payload makes two records
// from different builds incomparable, and the comparison is the entire point of
// writing them down.
type CompactionEconomicsRecord struct {
	GateVersion      string `json:"gateVersion"`
	PayloadVersion   string `json:"payloadVersion"`
	EstimatorVersion string `json:"estimatorVersion"`

	WorkflowRunID string `json:"run,omitempty"`
	StepID        string `json:"step,omitempty"`
	Cycle         int    `json:"cycle"`
	SessionID     string `json:"session,omitempty"`
	Provider      string `json:"provider,omitempty"`
	Harness       string `json:"harness,omitempty"`
	ModelID       string `json:"model,omitempty"`

	PricingStatus  CompactionPricingStatus `json:"pricingStatus"`
	PricingSource  string                  `json:"pricingSource,omitempty"`
	PricingVersion string                  `json:"pricingVersion,omitempty"`
	Currency       string                  `json:"currency,omitempty"`
	// CacheWriteLifetimePriced is which of the two cache-creation rates the
	// rewrite was priced at, and it is the single most load-bearing field in
	// the record: P7.2B1 exists because pricing a long-lived write at the
	// short-lived rate understates it by 60%.
	CacheWriteLifetimePriced CacheWriteLifetime `json:"cacheWriteLifetimePriced,omitempty"`

	ContextBeforeTokens    int64 `json:"contextBefore"`
	ContextThresholdTokens int64 `json:"contextThreshold"`
	StablePrefixTokens     int64 `json:"stablePrefixTokens"`
	PromptTokens           int64 `json:"promptTokens"`

	EstimatedContextAfterTokens     int64                    `json:"estimatedContextAfter"`
	EstimatedContextReductionTokens int64                    `json:"estimatedContextReduction"`
	EstimatedReductionPercent       float64                  `json:"estimatedReductionPercent"`
	ContextAfterBasis               CompactionEstimatorBasis `json:"contextAfterBasis"`
	ContextAfterSamples             int                      `json:"contextAfterSamples"`

	EstimatedSummaryTokens int64                    `json:"estimatedSummaryTokens"`
	SummaryBasis           CompactionEstimatorBasis `json:"summaryEstimatorBasis"`
	SummarySamples         int                      `json:"summarySampleCount"`

	EstimatedRemainingCalls  int64                    `json:"estimatedRemainingCalls"`
	RemainingCallsBasis      CompactionEstimatorBasis `json:"remainingCallsEstimatorSource"`
	RemainingCallsSamples    int                      `json:"remainingCallsSampleCount"`
	RemainingCallsCapApplied bool                     `json:"remainingCallsCapApplied,omitempty"`

	MaxFixCycles              int      `json:"maxFixCycles"`
	StructuralCyclesRemaining int      `json:"structuralCyclesRemaining"`
	Strategy                  string   `json:"strategy,omitempty"`
	LifecycleReasons          []string `json:"lifecycleReasons,omitempty"`

	// BreakEvenCalls is N*: how many calls after the first must still be made
	// for the purchase to pay for itself. Known=false when the arithmetic did
	// not get far enough to compute it.
	BreakEvenCalls      float64 `json:"breakEvenCalls"`
	BreakEvenCallsKnown bool    `json:"breakEvenCallsKnown"`
	SafetyFactor        float64 `json:"safetyFactor"`
	RequiredCalls       float64 `json:"requiredCalls"`
	// Margin is EstimatedRemainingCalls / RequiredCalls. It is what bounds the
	// unobservable false negatives: a cohort of skips clustered near zero
	// contains no plausible missed saving, and one clustered near 0.9 does.
	Margin float64 `json:"margin"`

	EstimatedCompactionCostMicros int64 `json:"estimatedCompactionCostMicros"`
	EstimatedFutureSavingsMicros  int64 `json:"estimatedFutureSavingsMicros"`
	EstimatedNetSavingsMicros     int64 `json:"estimatedNetSavingsMicros"`
	SavingPerCallMicros           int64 `json:"savingPerCallMicros"`

	Verdict CompactionEconomicVerdict `json:"verdict"`
	Reason  CompactionEconomicReason  `json:"reason"`
	// WouldHaveActed is always FALSE in P7.2B2. It exists so the field does not
	// have to be added later, and so no reader can mistake this cohort for
	// enforcement.
	WouldHaveActed bool `json:"wouldHaveActed"`
}

// microsPerMillionTokens converts a $/MTok rate and a token count into integer
// micros of currency: tokens * rate / 1e6 dollars, * 1e6 micros.
//
// The two factors of a million cancel exactly, which is a small gift: the
// conversion is tokens * rate, rounded once, and there is no intermediate
// division to lose precision in.
func microsPerMillionTokens(tokens int64, ratePerMTok float64) float64 {
	return float64(tokens) * ratePerMTok
}

// EvaluateCompactionEconomics computes the shadow verdict.
//
// The order is cheapest-and-most-certain first, with one deliberate exception:
// pricing and cache-lifetime admissibility are checked BEFORE the structural
// terminal-cycle rule, so that every record carries a real PricingStatus and
// pricing coverage over a cohort is a count instead of a guess. The
// terminal-cycle rule loses nothing by running second -- it can only ever
// produce SKIP, and a post-condition at the bottom of this function makes it
// impossible for a terminal cycle to reach COMPACT by any path.
func EvaluateCompactionEconomics(in CompactionEconomicsInput) CompactionEconomicsRecord {
	rec := CompactionEconomicsRecord{
		GateVersion:      CompactionEconomicsGateVersion,
		PayloadVersion:   CompactionEconomicsPayloadVersion,
		EstimatorVersion: CompactionEstimatorsVersion,

		WorkflowRunID: in.WorkflowRunID,
		StepID:        in.StepID,
		Cycle:         in.Cycle,
		SessionID:     in.SessionID,
		Provider:      in.Provider,
		Harness:       in.Harness,
		ModelID:       in.ModelID,

		ContextBeforeTokens:    in.ContextBeforeTokens,
		ContextThresholdTokens: in.ContextThresholdTokens,
		StablePrefixTokens:     in.StablePrefixTokens,

		MaxFixCycles:              in.MaxFixCycles,
		StructuralCyclesRemaining: structuralCyclesRemaining(in.Cycle, in.MaxFixCycles),
		Strategy:                  in.Strategy,

		ContextAfterBasis:   CompactionBasisNone,
		SummaryBasis:        CompactionBasisNone,
		RemainingCallsBasis: CompactionBasisNone,

		SafetyFactor:   effectiveSafetyFactor(in.SafetyFactor),
		WouldHaveActed: false,
	}
	for _, r := range in.LifecycleReasons {
		rec.LifecycleReasons = append(rec.LifecycleReasons, string(r))
	}
	if in.PromptTokensKnown && in.PromptTokens > 0 {
		rec.PromptTokens = in.PromptTokens
	}
	// Estimates are recorded whether or not they are reached, so a record shows
	// what was available as well as what blocked it.
	if in.ContextAfter.Known {
		rec.EstimatedContextAfterTokens = in.ContextAfter.Value
		rec.ContextAfterBasis = in.ContextAfter.Basis
		rec.ContextAfterSamples = in.ContextAfter.Samples
	}
	if in.SummaryTokens.Known {
		rec.EstimatedSummaryTokens = in.SummaryTokens.Value
		rec.SummaryBasis = in.SummaryTokens.Basis
		rec.SummarySamples = in.SummaryTokens.Samples
	}
	if in.RemainingCalls.Known {
		rec.EstimatedRemainingCalls = in.RemainingCalls.Value
		rec.RemainingCallsBasis = in.RemainingCalls.Basis
		rec.RemainingCallsSamples = in.RemainingCalls.Samples
		rec.RemainingCallsCapApplied = in.RemainingCalls.CapApplied
	}

	// --- 1. is the conversation observable at all -------------------------
	if !in.UsageObservable || in.ContextBeforeTokens <= 0 {
		return unknown(rec, CompactionReasonUsageIncomplete)
	}
	// A prefix larger than the conversation it is a prefix of is not a small
	// error to clamp: it means two reads disagree, and an arithmetic built on
	// them would produce a confident wrong answer.
	if in.StablePrefixTokens < 0 || in.StablePrefixTokens > in.ContextBeforeTokens {
		return unknown(rec, CompactionReasonInconsistentAccounting)
	}

	// --- 2. pricing, and whether it is in the right currency --------------
	if !in.RatesKnown {
		rec.PricingStatus = CompactionPricingUnpricedModel
		return unknown(rec, CompactionReasonPricingUnknown)
	}
	rec.PricingSource = in.Rates.Source
	rec.PricingVersion = in.Rates.Version
	rec.Currency = in.Rates.Currency
	if !in.Rates.Complete() {
		rec.PricingStatus = CompactionPricingIncompleteRates
		return unknown(rec, CompactionReasonPricingUnknown)
	}
	// AO's own figure for this session was computed on an ASSUMED lifetime.
	// Every comparison below would inherit that assumption, so it is refused
	// here rather than disclosed at the bottom.
	if in.ObservedCostTTLAssumedTokens > 0 {
		rec.PricingStatus = CompactionPricingAssumedTTL
		return unknown(rec, CompactionReasonTTLAssumed)
	}
	if in.ObservedCostTTLUnknownTokens > 0 {
		rec.PricingStatus = CompactionPricingIncompleteRates
		return unknown(rec, CompactionReasonTTLUnknown)
	}
	// Which lifetime will the rewrite be created at? Read from what this
	// session's writes have actually been, never assumed.
	lifetime := DominantCacheWriteLifetime(in.ObservedCacheCreation)
	if lifetime == CacheWriteLifetimeUnknown {
		rec.PricingStatus = CompactionPricingIncompleteRates
		return unknown(rec, CompactionReasonTTLUnknown)
	}
	cacheWriteRate, ok := in.Rates.CacheWriteRateFor(lifetime)
	if !ok {
		rec.PricingStatus = CompactionPricingIncompleteRates
		rec.CacheWriteLifetimePriced = lifetime
		return unknown(rec, CompactionReasonTTLRateUnknown)
	}
	rec.PricingStatus = CompactionPricingPriced
	rec.CacheWriteLifetimePriced = lifetime

	// --- 3. the structural rule that needs no estimate --------------------
	if isTerminalCycleForCompaction(in) {
		return skip(rec, CompactionReasonTerminalCycle)
	}

	// --- 4. the estimates -------------------------------------------------
	if !in.ContextAfter.Known {
		return unknown(rec, CompactionReasonContextAfterUnknown)
	}
	if !in.SummaryTokens.Known {
		return unknown(rec, CompactionReasonSummaryCostUnknown)
	}
	if in.ContextAfter.Value < 0 || in.SummaryTokens.Value < 0 {
		return unknown(rec, CompactionReasonInconsistentAccounting)
	}

	// --- 5. is there a reduction worth buying -----------------------------
	// D = (B + delta) - A. Delta belongs on the left because AO is about to
	// send it either way: the do-nothing arm pays for it on top of B, and the
	// compacting arm pays for it on top of A.
	reduction := (in.ContextBeforeTokens + rec.PromptTokens) - in.ContextAfter.Value
	rec.EstimatedContextAfterTokens = in.ContextAfter.Value
	rec.EstimatedContextReductionTokens = reduction
	if in.ContextBeforeTokens > 0 {
		rec.EstimatedReductionPercent = 100 * float64(reduction) / float64(in.ContextBeforeTokens)
	}
	if reduction <= 0 {
		return skip(rec, CompactionReasonNegative)
	}

	// --- 6. the money -----------------------------------------------------
	//
	// cost = B*Cr                  the /compact turn re-reads the conversation
	//      + S*Co                  it GENERATES the summary, at output prices
	//      + (A - P - delta)*Cw    the first call after rewrites the new prefix
	//      - (B - P)*Cr            ...but that call no longer reads B
	//
	// The last term is a credit, not a saving to count twice: it is the FIRST
	// call's share, which is why the benefit below counts calls after it.
	rewritten := in.ContextAfter.Value - in.StablePrefixTokens - rec.PromptTokens
	if rewritten < 0 {
		// A post-compaction conversation smaller than the prefix that survives
		// it plus the prompt about to be sent is not a cheap rewrite; it is two
		// reads that cannot both be right.
		return unknown(rec, CompactionReasonInconsistentAccounting)
	}
	costMicros := microsPerMillionTokens(in.ContextBeforeTokens, in.Rates.CacheReadPerMTok) +
		microsPerMillionTokens(in.SummaryTokens.Value, in.Rates.OutputPerMTok) +
		microsPerMillionTokens(rewritten, cacheWriteRate) -
		microsPerMillionTokens(in.ContextBeforeTokens-in.StablePrefixTokens, in.Rates.CacheReadPerMTok)
	savingPerCallMicros := microsPerMillionTokens(reduction, in.Rates.CacheReadPerMTok)
	if !allFinite(costMicros, savingPerCallMicros) {
		return unknown(rec, CompactionReasonEvaluationError)
	}
	rec.EstimatedCompactionCostMicros = roundMicros(costMicros)
	rec.SavingPerCallMicros = roundMicros(savingPerCallMicros)

	// A conversation whose reduction is worth nothing per call can never
	// amortize anything, and dividing by it is how a gate produces an infinite
	// saving. Both are the same guard.
	if savingPerCallMicros <= 0 {
		return skip(rec, CompactionReasonNegative)
	}
	if costMicros <= 0 {
		// Free or better than free. Honest, and it happens when the credit for
		// no longer reading B exceeds everything the compaction costs -- but it
		// is also exactly the shape a bad rate card would produce, so it does
		// not short-circuit the call forecast below.
		rec.BreakEvenCalls, rec.BreakEvenCallsKnown = 0, true
	} else {
		rec.BreakEvenCalls, rec.BreakEvenCallsKnown = costMicros/savingPerCallMicros, true
	}
	rec.RequiredCalls = rec.BreakEvenCalls * rec.SafetyFactor
	if !allFinite(rec.BreakEvenCalls, rec.RequiredCalls) {
		return unknown(rec, CompactionReasonEvaluationError)
	}

	// The structural floor runs HERE rather than before the money, so that a
	// record rejected on it still carries the break-even it was rejected at.
	// That is the difference between an audit that says "the reduction was too
	// small" and one that can answer, months later, "it would have needed 117
	// calls and had 4".
	minFraction := in.MinReductionFraction
	if minFraction <= 0 {
		minFraction = DefaultCompactionMinReductionFraction
	}
	if float64(reduction) < minFraction*float64(in.ContextBeforeTokens) {
		return skip(rec, CompactionReasonReductionTooSmall)
	}

	// --- 7. how many calls are left ---------------------------------------
	if !in.RemainingCalls.Known {
		return unknown(rec, CompactionReasonRemainingCallsUnknown)
	}
	if in.RemainingCalls.Value < 0 {
		return unknown(rec, CompactionReasonInconsistentAccounting)
	}
	n := float64(in.RemainingCalls.Value)
	// Multiplied from the ROUNDED per-call figure, and the net subtracted from
	// the rounded cost, so the four money fields in the record are consistent
	// with each other. A reader who multiplies savingPerCallMicros by the call
	// count must get the figure the record states, or the record is not an audit
	// of anything.
	rec.EstimatedFutureSavingsMicros = rec.SavingPerCallMicros * in.RemainingCalls.Value
	rec.EstimatedNetSavingsMicros = rec.EstimatedFutureSavingsMicros - rec.EstimatedCompactionCostMicros
	if rec.RequiredCalls > 0 {
		rec.Margin = n / rec.RequiredCalls
	} else if n > 0 {
		// Break-even at zero calls with calls available: the margin is not
		// infinite in any useful sense, and 1 is the smallest value that
		// carries "at or above what was required".
		rec.Margin = 1
	}
	if in.RemainingCalls.Value == 0 {
		return skip(rec, CompactionReasonInsufficientFutureCalls)
	}
	if n < rec.BreakEvenCalls {
		return skip(rec, CompactionReasonInsufficientFutureCalls)
	}
	if n < rec.RequiredCalls {
		return skip(rec, CompactionReasonBelowSafetyMargin)
	}

	// --- 8. the only path to COMPACT --------------------------------------
	rec.Verdict, rec.Reason = CompactionVerdictCompact, CompactionReasonPositiveMargin
	// Post-condition, deliberately redundant with step 3. A terminal cycle must
	// never reach COMPACT by ANY path, including one a future edit introduces
	// above this line. If the two ever disagree, the structural rule wins.
	if isTerminalCycleForCompaction(in) {
		return skip(rec, CompactionReasonTerminalCycle)
	}
	return rec
}

// structuralCyclesRemaining is how many repair cycles will still run AFTER the
// one being dispatched.
//
// cycleCount is the number of the cycle about to be dispatched -- 1 for the
// first repair -- so with MaxFixCycles 3, dispatching cycle 3 leaves zero.
// Floored at zero: a run already past its ceiling has none left, not a negative
// number of them.
func structuralCyclesRemaining(cycle, maxFixCycles int) int {
	if maxFixCycles <= 0 || cycle <= 0 {
		return 0
	}
	if remaining := maxFixCycles - cycle; remaining > 0 {
		return remaining
	}
	return 0
}

// isTerminalCycleForCompaction reports whether this is the last cycle that will
// ever run AND the only thing that asked for a compaction was a counter reaching
// its ceiling.
//
// Both halves are load-bearing. The first is arithmetic on frozen policy and
// needs no estimate, which is why it can run before anything is priced. The
// second is what keeps the rule narrow: a terminal cycle with genuine context
// pressure and a long tail of calls inside it can still be economically
// positive, and this rule must not pretend otherwise. It fires only when
// many_fix_cycles or many_attempts is the SOLE reason -- the two branches that
// reach COMPACT precisely when the fewest calls remain, which is the shape that
// bought the one measured compaction that lost the most money.
func isTerminalCycleForCompaction(in CompactionEconomicsInput) bool {
	if structuralCyclesRemaining(in.Cycle, in.MaxFixCycles) > 0 {
		return false
	}
	if len(in.LifecycleReasons) == 0 {
		return false
	}
	for _, r := range in.LifecycleReasons {
		if r != LifecycleReasonManyFixCycles && r != LifecycleReasonManyAttempts {
			return false
		}
	}
	return true
}

// effectiveSafetyFactor refuses a factor below 1: a "safety" factor that lowered
// the bar would be a licence, and one that is absent is not a licence either.
func effectiveSafetyFactor(f float64) float64 {
	if !isFinite(f) || f < 1 {
		return DefaultCompactionSafetyFactor
	}
	return f
}

func skip(rec CompactionEconomicsRecord, reason CompactionEconomicReason) CompactionEconomicsRecord {
	rec.Verdict, rec.Reason = CompactionVerdictSkip, reason
	return rec
}

func unknown(rec CompactionEconomicsRecord, reason CompactionEconomicReason) CompactionEconomicsRecord {
	rec.Verdict, rec.Reason = CompactionVerdictUnknown, reason
	return rec
}

// roundMicros rounds a micros figure to the nearest integer, saturating rather
// than wrapping. A payload that wrapped int64 would be worse than one that
// clipped, because a clipped value is visibly absurd and a wrapped one is
// plausibly small.
func roundMicros(v float64) int64 {
	if !isFinite(v) {
		return 0
	}
	r := math.Round(v)
	if r > math.MaxInt64/2 {
		return math.MaxInt64 / 2
	}
	if r < math.MinInt64/2 {
		return math.MinInt64 / 2
	}
	return int64(r)
}

func isFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func allFinite(vs ...float64) bool {
	for _, v := range vs {
		if !isFinite(v) {
			return false
		}
	}
	return true
}

// --- reading the cohort back -------------------------------------------------

// CompactionEconomicsSummary folds a set of shadow verdicts into the questions
// an operator actually has about them.
//
// The questions are not rhetorical: how many evaluations, how many of each
// verdict, why, what break-even was computed, what saving was estimated. A
// cohort nobody can ask those of is a table, not evidence.
//
// PURE. It folds records that were already written; it reads nothing and
// recomputes nothing. A verdict computed by an older gate version is counted
// under its own version rather than pooled, because pooling two arithmetics is
// how a cohort stops meaning anything.
type CompactionEconomicsSummary struct {
	Evaluations int
	Compact     int
	Skip        int
	Unknown     int
	// ByReason counts each closed reason. Sorted output is the caller's job;
	// the map is the fold.
	ByReason map[CompactionEconomicReason]int
	// ByPricingStatus is what §13's "pricing is honest for the sessions being
	// judged" criterion is measured from.
	ByPricingStatus map[CompactionPricingStatus]int
	// GateVersions and EstimatorVersions are every version present. More than
	// one of either means the cohort spans a change and must be split before
	// any rate is computed over it.
	GateVersions      map[string]int
	EstimatorVersions map[string]int

	// PricedEvaluations is how many verdicts had a complete, honest rate. The
	// share of it is the pricing-coverage figure.
	PricedEvaluations int
	// CacheWriteLifetime1h and 5m count which cache-creation rate was used,
	// which is the P7.2B1 honesty check: a cohort priced mostly at 5m on Claude
	// sessions is a cohort measured in the wrong currency.
	CacheWriteLifetime1h int
	CacheWriteLifetime5m int

	// BreakEvenCalls* summarise N* over the verdicts that got far enough to
	// compute one.
	BreakEvenComputed int
	BreakEvenMin      float64
	BreakEvenMax      float64
	BreakEvenMean     float64

	// NetSavingsMicros sums the estimated net over COMPACT verdicts only. It is
	// an ESTIMATE of money not spent on compactions that did not happen at this
	// gate's recommendation, which in shadow mode is a modelled figure and never
	// a realized one.
	CompactNetSavingsMicros int64
	// SkipNearMisses counts skips whose margin exceeded nearMissMarginFloor: the
	// cohort that would become COMPACT under a lower safety factor, and the one
	// §13's fourth criterion is about. A cohort full of these means the factor
	// is wrong rather than that the gate is safe.
	SkipNearMisses int
	// TerminalCycleSkips is how many verdicts the structural rule rejected
	// without needing any estimate at all.
	TerminalCycleSkips int
}

// NearMissMarginFloor is the margin above which a SKIP is worth looking at
// again.
//
// 0.8: a skip at 0.9 of the required call count is a decision the safety factor
// made rather than the arithmetic, and a cohort of them is the signal to
// recalibrate the factor. Both measured compactions sit at 0.03-0.09, two orders
// of magnitude below it, which is why the observed population contains no
// plausible false negative.
const NearMissMarginFloor = 0.8

// SummarizeCompactionEconomics folds the records. A nil or empty slice returns a
// zero summary with initialized maps, so a caller can print it without a nil
// check and an empty cohort reads as empty rather than as absent.
func SummarizeCompactionEconomics(records []CompactionEconomicsRecord) CompactionEconomicsSummary {
	out := CompactionEconomicsSummary{
		ByReason:          map[CompactionEconomicReason]int{},
		ByPricingStatus:   map[CompactionPricingStatus]int{},
		GateVersions:      map[string]int{},
		EstimatorVersions: map[string]int{},
	}
	var breakEvenSum float64
	for _, r := range records {
		out.Evaluations++
		switch r.Verdict {
		case CompactionVerdictCompact:
			out.Compact++
			out.CompactNetSavingsMicros += r.EstimatedNetSavingsMicros
		case CompactionVerdictSkip:
			out.Skip++
			if r.Margin > NearMissMarginFloor {
				out.SkipNearMisses++
			}
			if r.Reason == CompactionReasonTerminalCycle {
				out.TerminalCycleSkips++
			}
		default:
			out.Unknown++
		}
		out.ByReason[r.Reason]++
		out.ByPricingStatus[r.PricingStatus]++
		if r.GateVersion != "" {
			out.GateVersions[r.GateVersion]++
		}
		if r.EstimatorVersion != "" {
			out.EstimatorVersions[r.EstimatorVersion]++
		}
		if r.PricingStatus == CompactionPricingPriced {
			out.PricedEvaluations++
		}
		switch r.CacheWriteLifetimePriced {
		case CacheWriteLifetime1h:
			out.CacheWriteLifetime1h++
		case CacheWriteLifetime5m:
			out.CacheWriteLifetime5m++
		}
		if r.BreakEvenCallsKnown {
			if out.BreakEvenComputed == 0 || r.BreakEvenCalls < out.BreakEvenMin {
				out.BreakEvenMin = r.BreakEvenCalls
			}
			if r.BreakEvenCalls > out.BreakEvenMax {
				out.BreakEvenMax = r.BreakEvenCalls
			}
			out.BreakEvenComputed++
			breakEvenSum += r.BreakEvenCalls
		}
	}
	if out.BreakEvenComputed > 0 {
		out.BreakEvenMean = breakEvenSum / float64(out.BreakEvenComputed)
	}
	return out
}

// PricingCoverage is the share of evaluations that had an honest rate, and
// whether the question is answerable at all.
//
// Two returns rather than one, for the reason this codebase keeps repeating: an
// empty cohort has no coverage, and reporting 0% for it would read as a pricing
// failure rather than as an absence of evaluations. §13's first criterion asks
// for 90%; this is the number it is measured against.
func (s CompactionEconomicsSummary) PricingCoverage() (float64, bool) {
	if s.Evaluations == 0 {
		return 0, false
	}
	return 100 * float64(s.PricedEvaluations) / float64(s.Evaluations), true
}
