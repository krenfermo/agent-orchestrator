package turnbench

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// cost.go -- P7.1: the money half, kept deliberately apart from the token half.
//
// THE DEFECT THIS FILE EXISTS TO CLOSE. Measure() answers "how many tokens",
// and the answer for wf-1c2cb9bd's replay was a 39% reduction in cumulative
// input. That figure was then read -- in a document, by people, including the
// people who wrote it -- as a 39% saving. It is not one, and it cannot be one,
// for two reasons that are both arithmetic:
//
//  1. Billed input is not one rate. A cache read, a cache write and an uncached
//     read are priced apart by a factor of twelve and of ten respectively, and
//     compaction moves tokens from the cheapest of the three to the dearest.
//     Fewer tokens at a higher blended rate is a smaller bill only sometimes.
//  2. A compaction GENERATES a summary, at output prices, and that turn is
//     invisible to the call series. A replay that folds only the calls has
//     omitted the single largest term in what compacting costs.
//
// So cost lives here, behind a rate card, behind a split the token fold does
// not require, and behind a Known flag that is false by default. You cannot
// reach a cost figure from this package by accident, and you cannot reach one
// at all from a series that does not carry what pricing a series needs.
//
// Nothing here decides anything. It prices a recorded or replayed shape.

// CostUnknownReason names why a series could not be priced. Closed vocabulary:
// a reader has to be able to tell "no rate for this model" from "this recording
// predates the split" without parsing prose.
type CostUnknownReason string

// The reasons a cost cannot be produced.
const (
	// CostReasonNone means the cost was produced.
	CostReasonNone CostUnknownReason = ""
	// CostReasonNoPricer is a caller that supplied no rate card.
	CostReasonNoPricer CostUnknownReason = "no_pricer"
	// CostReasonNoModel is a series that does not say which model it ran on.
	CostReasonNoModel CostUnknownReason = "no_model"
	// CostReasonUnpricedModel is a model no rate card covers. The tokens are
	// still reported in full -- P3-E's own rule.
	CostReasonUnpricedModel CostUnknownReason = "unpriced_model"
	// CostReasonSplitUnknown is a series whose calls do not partition their
	// context into uncached / cache read / cache write. Every recording made
	// before P7.1 is in this state, and none of them can be priced: a total of
	// billed input carries no information about which rate applied to it.
	CostReasonSplitUnknown CostUnknownReason = "split_unknown"
	// CostReasonSummaryOutputUnknown is a series that compacted without anyone
	// measuring what the summarization generated. The calls could be priced;
	// the compactions could not, so the total is refused rather than reported
	// short.
	CostReasonSummaryOutputUnknown CostUnknownReason = "summary_output_unknown"
	// CostReasonUnpricedUnattributedModel is a measured residual whose model
	// no rate card covers. It is a DIFFERENT failure from an unpriced series:
	// the calls priced fine and the gap did not, which is what a session rolled
	// up as "claude-opus-5[1m]" against a catalog that knows "claude-opus-5"
	// looks like. Naming it separately is what makes it fixable -- one row in
	// an operator rate card -- instead of looking like a broken recording.
	CostReasonUnpricedUnattributedModel CostUnknownReason = "unpriced_unattributed_model"
)

// CompactionCostBasis says where the compaction half of a cost came from.
type CompactionCostBasis string

// The three bases.
const (
	// CompactionBasisNone is a series with no compactions.
	CompactionBasisNone CompactionCostBasis = "none"
	// CompactionBasisMeasured is the harness's own residual: what it charged
	// itself minus what AO attributed. The strongest evidence available.
	CompactionBasisMeasured CompactionCostBasis = "measured_residual"
	// CompactionBasisModelled is the read-plus-generate model, used when a
	// replay has no residual to measure because the run never happened.
	CompactionBasisModelled CompactionCostBasis = "modelled"
)

// CostMetrics is what one series costs, and the provenance of the claim.
type CostMetrics struct {
	// Known is false when Reason says why. Every token field remains valid
	// either way: tokens are reported, cost is refused -- never the reverse.
	Known  bool
	Reason CostUnknownReason

	// CallCost is the cost of the calls in the series. CompactionCost is the
	// cost of the summarization turns beside them: what they re-read, at cache
	// read prices, plus what they generated, at output prices.
	//
	// They are separate because the whole finding of P7.1 is that the second
	// one exists and was never counted. A reader who sees only a total cannot
	// tell whether it was.
	CallCost       domain.UsageCost
	CompactionCost domain.UsageCost
	TotalCost      domain.UsageCost

	// CallTokens and CompactionTokens are the vectors those costs were
	// computed from.
	CallTokens       domain.UsageTokenTotals
	CompactionTokens domain.UsageTokenTotals
	// CompactionBasis says whether the compaction half was measured from the
	// harness's own rollup or modelled from the boundaries. A reader comparing
	// two scenarios must be able to see that one side is evidence and the
	// other is arithmetic.
	CompactionBasis CompactionCostBasis
}

// CostOf prices a series.
//
// pricer may be nil and the model may be unknown; the result then says so
// through Reason and carries no money figure at all. There is deliberately no
// partial cost and no zero: a cost this package cannot compute is not a cheap
// one.
//
// The compaction turn is modelled as: read the whole pre-compaction
// conversation at the CACHE READ rate, and generate the summary at the OUTPUT
// rate. The read is priced as cached because a conversation the harness has
// just been talking to is in its cache; that is the CHEAPEST of the three input
// rates, so the model understates the compaction rather than inflating it. What
// the first post-compaction call then has to re-write is not charged here -- it
// is already in that call's own CacheWriteTokens, and charging it twice would
// be the mirror of the error this file exists to fix.
func CostOf(s Series, pricer domain.ModelTokenPricer, metrics Metrics) CostMetrics {
	out := CostMetrics{
		CallTokens: metrics.Tokens,
	}
	switch {
	case pricer == nil:
		out.Reason = CostReasonNoPricer
		return out
	case s.ModelID == "":
		out.Reason = CostReasonNoModel
		return out
	case !metrics.SplitKnown:
		out.Reason = CostReasonSplitUnknown
		return out
	}
	out.CallCost = pricer.Cost(s.ModelID, metrics.Tokens)
	if !out.CallCost.Known {
		out.Reason = CostReasonUnpricedModel
		out.CallCost = domain.UsageCost{}
		return out
	}
	switch {
	case s.Unattributed != nil:
		// Measured beats modelled. The residual is what the harness says it
		// spent and AO cannot account for; it is priced as the harness named
		// the model, never as the series named it.
		out.CompactionBasis = CompactionBasisMeasured
		out.CompactionTokens = s.Unattributed.Tokens
		out.CompactionCost = pricer.Cost(s.Unattributed.ModelID, s.Unattributed.Tokens)
		if !out.CompactionCost.Known {
			out.Reason = CostReasonUnpricedUnattributedModel
			out.CallCost = domain.UsageCost{}
			out.CompactionCost = domain.UsageCost{}
			return out
		}
	case metrics.Compactions == 0:
		out.CompactionBasis = CompactionBasisNone
	case !metrics.SummaryOutputKnown:
		out.Reason = CostReasonSummaryOutputUnknown
		out.CallCost = domain.UsageCost{}
		return out
	default:
		out.CompactionBasis = CompactionBasisModelled
		out.CompactionTokens = domain.UsageTokenTotals{
			InputTokens:     metrics.CompactionPreTokens,
			CacheReadTokens: metrics.CompactionPreTokens,
			OutputTokens:    metrics.SummaryOutputTokens,
			EventCount:      metrics.Compactions,
		}
		out.CompactionCost = pricer.Cost(s.ModelID, out.CompactionTokens)
	}
	out.TotalCost = out.CallCost.Add(out.CompactionCost)
	out.Known = true
	out.Reason = CostReasonNone
	return out
}

// Scenario is one side of a before/after: the series, what it measures, and
// what it costs.
type Scenario struct {
	Series  Series
	Metrics Metrics
	Cost    CostMetrics
}

// MeasureScenario folds and prices one series in one call.
func MeasureScenario(s Series, pricer domain.ModelTokenPricer) Scenario {
	metrics := Measure(s)
	return Scenario{Series: s, Metrics: metrics, Cost: CostOf(s, pricer, metrics)}
}

// Savings is the whole point of the file: the two reductions, side by side,
// each with its own Known flag, and NEITHER of them reachable as "the"
// reduction.
//
// A caller that wants one number has to choose which number it means, in a
// field name that says so, and handle the case where that one is not available
// while the other is -- which is the normal case, not an edge one.
type Savings struct {
	// TokenReductionPercent is how much smaller the billed INPUT got. It is
	// the figure `docs/p7-turn-economy.md` §0 reports, and it is not money.
	TokenReductionPercent float64
	TokenReductionKnown   bool
	// CostReductionPercent is how much smaller the BILL got, compactions
	// included. CostReductionUnknownReason says why it is missing when it is.
	CostReductionPercent       float64
	CostReductionKnown         bool
	CostReductionUnknownReason CostUnknownReason
}

// CompareScenarios produces both reductions from a before/after pair.
//
// The two are computed from different quantities on purpose and are expected to
// disagree: on the one real pair AO has, the token reduction is roughly twice
// the cost reduction. A test pins that they are not equal, so a future change
// that quietly makes one a synonym for the other fails here rather than in a
// bill.
func CompareScenarios(before, after Scenario) Savings {
	out := Savings{}
	if before.Metrics.CumulativeInput > 0 {
		out.TokenReductionPercent = ReductionPercent(before.Metrics.CumulativeInput, after.Metrics.CumulativeInput)
		out.TokenReductionKnown = true
	}
	switch {
	case !before.Cost.Known:
		out.CostReductionUnknownReason = before.Cost.Reason
	case !after.Cost.Known:
		out.CostReductionUnknownReason = after.Cost.Reason
	case before.Cost.TotalCost.Amount <= 0:
		out.CostReductionUnknownReason = CostReasonSplitUnknown
	default:
		out.CostReductionPercent = 100 * (before.Cost.TotalCost.Amount - after.Cost.TotalCost.Amount) / before.Cost.TotalCost.Amount
		out.CostReductionKnown = true
	}
	return out
}

// --- rendering --------------------------------------------------------------

// Report renders a before/after pair as a plain-text table.
//
// It exists so the two halves cannot be quoted apart by accident: every line of
// TOKENS and every line of COST appear under their own heading, the compaction
// rows sit between them, and the two reductions are printed on adjacent lines
// with their own labels. A reader who copies one number out of this table takes
// the label with it.
//
// Deterministic and content-free: numbers, the model id, and the closed
// vocabulary of reasons and bases.
func Report(before, after Scenario) string {
	var b strings.Builder
	row := func(label string, l, r string) {
		fmt.Fprintf(&b, "%-28s %18s %18s\n", label, l, r)
	}
	num := func(v int64) string { return strconv.FormatInt(v, 10) }
	money := func(c domain.UsageCost, known bool) string {
		if !known || !c.Known {
			return "unknown"
		}
		return fmt.Sprintf("%.4f %s", c.Amount, c.Currency)
	}

	fmt.Fprintf(&b, "scenario: %s -> %s\n", before.Series.Source, after.Series.Source)
	fmt.Fprintf(&b, "model:    %s / %s\n\n", before.Series.ModelID, after.Series.ModelID)
	row("", "ANTES", "DESPUES")
	b.WriteString("-- TOKENS ------------------------------------------------------------\n")
	row("calls", num(before.Metrics.Calls), num(after.Metrics.Calls))
	row("cumulative input", num(before.Metrics.CumulativeInput), num(after.Metrics.CumulativeInput))
	row("uncached input", num(before.Metrics.Tokens.UncachedInputTokens), num(after.Metrics.Tokens.UncachedInputTokens))
	row("cache read", num(before.Metrics.Tokens.CacheReadTokens), num(after.Metrics.Tokens.CacheReadTokens))
	row("cache write", num(before.Metrics.Tokens.CacheWriteTokens), num(after.Metrics.Tokens.CacheWriteTokens))
	row("output", num(before.Metrics.Tokens.OutputTokens), num(after.Metrics.Tokens.OutputTokens))
	b.WriteString("-- COMPACTIONS -------------------------------------------------------\n")
	row("count", num(before.Metrics.Compactions), num(after.Metrics.Compactions))
	row("pre tokens", num(before.Metrics.CompactionPreTokens), num(after.Metrics.CompactionPreTokens))
	row("post tokens", num(before.Metrics.CompactionPostTokens), num(after.Metrics.CompactionPostTokens))
	row("summary output tokens",
		knownNum(before.Metrics.SummaryOutputTokens, before.Metrics.SummaryOutputKnown),
		knownNum(after.Metrics.SummaryOutputTokens, after.Metrics.SummaryOutputKnown))
	row("duration ms", num(before.Metrics.CompactionDurationMs), num(after.Metrics.CompactionDurationMs))
	row("cost basis", string(before.Cost.CompactionBasis), string(after.Cost.CompactionBasis))
	b.WriteString("-- COST --------------------------------------------------------------\n")
	row("calls", money(before.Cost.CallCost, before.Cost.Known), money(after.Cost.CallCost, after.Cost.Known))
	row("compactions", money(before.Cost.CompactionCost, before.Cost.Known), money(after.Cost.CompactionCost, after.Cost.Known))
	row("total", money(before.Cost.TotalCost, before.Cost.Known), money(after.Cost.TotalCost, after.Cost.Known))
	if before.Cost.Reason != CostReasonNone || after.Cost.Reason != CostReasonNone {
		row("unknown because", string(before.Cost.Reason), string(after.Cost.Reason))
	}
	b.WriteString("-- SAVINGS (never one number) ----------------------------------------\n")
	savings := CompareScenarios(before, after)
	if savings.TokenReductionKnown {
		fmt.Fprintf(&b, "TOKEN SAVINGS  %.1f%%\n", savings.TokenReductionPercent)
	} else {
		b.WriteString("TOKEN SAVINGS  unknown\n")
	}
	if savings.CostReductionKnown {
		fmt.Fprintf(&b, "COST SAVINGS   %.1f%%\n", savings.CostReductionPercent)
	} else {
		fmt.Fprintf(&b, "COST SAVINGS   unknown (%s)\n", savings.CostReductionUnknownReason)
	}
	return b.String()
}

func knownNum(v int64, known bool) string {
	if !known {
		return "unknown"
	}
	return strconv.FormatInt(v, 10)
}
