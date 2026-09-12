package domain

import (
	"sort"
	"strings"
	"time"
)

// usage_compaction.go -- P7.1: what a compaction costs, and the part of it the
// ledger never saw.
//
// P7 shipped the act: AO can ask a session to replace its conversation with a
// summary of itself. The economics of doing so were then argued from
// `model_usage_events`, and that argument had a hole in it. On the one run that
// has ever compacted (wf-66f0ee54, session ao-canary-fixture-6) the ledger
// holds 28 events, and NOT ONE of them is the summarization turn. The harness
// wrote the evidence into its own transcript and AO walked past it:
//
//	system / compact_boundary
//	  trigger manual   preTokens 85,847   postTokens 10,956   durationMs 95,073
//	  trigger manual   preTokens 67,663   postTokens 13,240   durationMs 87,393
//
// A compaction is a model call like any other: it re-reads the whole
// conversation and it GENERATES a summary, at output prices. None of that
// reaches an assistant record, so none of it reaches the ledger, so every cost
// comparison built on the ledger alone understates a compacting run and only a
// compacting run. That is the specific defect this file exists to close.
//
// NOTHING HERE IS A SECOND TELEMETRY, and nothing here is a decision. Every
// figure below is read from records the usage pipeline already tails, folded by
// pure functions, and priced -- when it can be priced at all -- by the same
// rate card every other cost in AO comes from. No threshold is compared, no
// lifecycle action is chosen, and no compaction is caused or prevented by any
// line of it.
//
// WHAT IS DELIBERATELY NOT HERE. The summary's text, the conversation it
// summarised, the prompt that asked for it, and any per-compaction split of the
// unattributed tokens. The harness reports its own spend once per session and
// not once per compaction, so a per-compaction output figure would be a
// division AO has no basis for. The session-level residual is reported as a
// session-level residual.

// CompactionTrigger is who asked for the compaction, as the harness reports it.
// Closed enum: a transcript that says anything else is recorded as Unknown
// rather than passed through, because an open string field on an observation is
// a place for content to hide.
type CompactionTrigger string

// The trigger values AO recognises.
const (
	// CompactionTriggerManual is a compaction someone or something asked for
	// explicitly -- which is what AO's own /compact directive produces.
	CompactionTriggerManual CompactionTrigger = "manual"
	// CompactionTriggerAuto is the harness compacting on its own initiative
	// because the conversation reached its own ceiling.
	CompactionTriggerAuto CompactionTrigger = "auto"
	// CompactionTriggerUnknown is an observed compaction whose trigger the
	// transcript did not name, or named in a word AO does not know. The
	// compaction still happened; only its cause is unknown.
	CompactionTriggerUnknown CompactionTrigger = "unknown"
)

// NormalizeCompactionTrigger maps a harness's own word onto the closed enum.
// Anything unrecognised becomes Unknown -- never the harness's raw string.
func NormalizeCompactionTrigger(raw string) CompactionTrigger {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "manual":
		return CompactionTriggerManual
	case "auto", "automatic":
		return CompactionTriggerAuto
	default:
		return CompactionTriggerUnknown
	}
}

// Valid reports whether a trigger is part of the closed enum.
func (t CompactionTrigger) Valid() bool {
	switch t {
	case CompactionTriggerManual, CompactionTriggerAuto, CompactionTriggerUnknown:
		return true
	default:
		return false
	}
}

// CompactionBoundary is one observed conversation replacement.
//
// Every field is a number, a timestamp, an identifier or a closed-enum value.
// There is no field for the summary, for the preserved messages, or for
// anything the conversation contained -- the same omission-is-the-design rule
// claudeContentBlock follows in the parser.
//
// PreTokens and PostTokens are the HARNESS's own accounting of its
// conversation, not a provider's billed input. They are the size of the thing
// before and after, which is what a reduction is measured in; what the
// compaction actually cost is a different question and lives in
// CompactionAccounting.
type CompactionBoundary struct {
	// RecordUUID is the transcript record's own id. It is the idempotency key:
	// a source re-read from offset zero (an artifact replaced, a recovery)
	// must recognise a boundary it has already observed instead of counting it
	// twice.
	RecordUUID string
	SessionID  string
	Harness    AgentHarness
	// ModelID is the model the session was running when it compacted, as the
	// transcript's own assistant records name it. Empty when AO has not seen
	// the session name a model yet.
	ModelID string
	Trigger CompactionTrigger
	// ObservedAt is the harness's timestamp for the boundary. Nil when the
	// record carried none, in which case the boundary is still counted and
	// still real -- it simply cannot be placed against a workflow step.
	ObservedAt *time.Time

	PreTokens  int64
	PostTokens int64
	// CumulativeDroppedTokens is the harness's running total of conversation
	// it has discarded across every compaction of this session. Carried
	// verbatim; it is cumulative, so it must never be summed across
	// boundaries.
	CumulativeDroppedTokens int64
	// DurationMs is how long the harness took to produce the summary. Not a
	// cost in money, and deliberately not converted into one: it is the cost
	// in wall clock, which a run under a deadline pays and a run under a
	// budget does not.
	DurationMs int64
}

// ReductionTokens is how much smaller the conversation got, floored at zero.
// A compaction that grew the conversation did not reduce it by a negative
// amount; it failed to reduce it.
func (b CompactionBoundary) ReductionTokens() int64 {
	if b.PostTokens >= b.PreTokens {
		return 0
	}
	return b.PreTokens - b.PostTokens
}

// ReductionPercent is the same figure relative to what was there, and whether
// it is computable at all. A boundary with no PreTokens is an observation the
// harness left incomplete, not a 0% reduction.
func (b CompactionBoundary) ReductionPercent() (float64, bool) {
	if b.PreTokens <= 0 {
		return 0, false
	}
	return 100 * float64(b.ReductionTokens()) / float64(b.PreTokens), true
}

// HarnessModelTotals is what the harness says ONE model cost it over a whole
// session, read from its own end-of-session rollup.
//
// This is the only signal that bounds the unattributed spend, and it exists
// because the alternative -- estimating a summary's size from the length of the
// summary -- would mean reading the summary. It is not read to replace the
// ledger: the ledger is per call, per role, per cycle, and this is one row per
// model per session. It is read so the ledger can be checked against something.
type HarnessModelTotals struct {
	// ModelID is the harness's own spelling, carried verbatim. It is routinely
	// NOT the spelling the same harness puts on its assistant records -- a
	// session whose calls say "claude-opus-5" can roll up as
	// "claude-opus-5[1m]" -- and flattening the two would hide a real pricing
	// difference behind a cosmetic one.
	ModelID string
	Tokens  UsageTokenTotals
	// ThinkingTokens is reported by the harness separately and is a SUBSET of
	// OutputTokens, not an addition to it. Kept because it is most of what a
	// summarization turn generates.
	ThinkingTokens int64
	// ReportedCostUSD is the harness's own money figure for this model.
	//
	// It is NOT AO's cost and must never be folded into a ledger total, an
	// advisory or a budget: AO does not know how the harness priced it, the
	// number is a list-price computation rather than a bill, and AO's own
	// UsageCost carries provenance this figure has none of. It is carried for
	// exactly one purpose, which is to let a reader see that AO's calculated
	// figure and the harness's own figure disagree, and by how much.
	ReportedCostUSD float64
}

// HarnessSessionTotals is the harness's whole self-reported rollup for one
// session.
//
// Observed=false is "the harness has not written its rollup yet", which is the
// normal state of a session that is still running. It must never be read as a
// session that cost nothing -- the same unknown-is-not-zero rule
// SessionContextReading is built around.
type HarnessSessionTotals struct {
	Observed bool
	Models   []HarnessModelTotals
	// ReportedCostUSD is the harness's total across every model. Same caveats
	// as HarnessModelTotals.ReportedCostUSD, and it is the harness's own sum
	// rather than one AO re-added, so a model AO failed to decode cannot make
	// it quietly smaller.
	ReportedCostUSD float64
	// AnyUnknownModelCost is the harness saying it could not price part of its
	// own usage. When true, ReportedCostUSD is itself partial.
	AnyUnknownModelCost bool
}

// Totals folds every model's tokens into one vector.
func (h HarnessSessionTotals) Totals() UsageTokenTotals {
	var out UsageTokenTotals
	for _, m := range h.Models {
		out = out.Add(m.Tokens)
	}
	return out
}

// canonicalModelKey strips a trailing bracketed annotation from a model id, so
// "claude-opus-5[1m]" and "claude-opus-5" can be recognised as the same model
// WHEN MATCHING AO's events against a harness rollup.
//
// It is used for matching and never for pricing. The two spellings do not
// necessarily cost the same -- a long-context variant is exactly the kind of
// thing that does not -- so the rate lookup always uses the id as reported.
// Collapsing them for pricing would be inventing a price, which the pricing
// package exists to forbid.
func canonicalModelKey(id string) string {
	trimmed := strings.ToLower(strings.TrimSpace(id))
	if i := strings.IndexByte(trimmed, '['); i > 0 {
		return trimmed[:i]
	}
	return trimmed
}

// CompactionAccountingInput is the pure fold's input: what AO attributed, what
// the harness says, and the boundaries observed in between.
type CompactionAccountingInput struct {
	SessionID string
	// Attributed is AO's own ledger for this session, per model, exactly as the
	// usage pipeline recorded it.
	Attributed []ModelUsageLine
	Harness    HarnessSessionTotals
	Boundaries []CompactionBoundary
}

// ModelTokenPricer prices one model's token vector. *pricing.Table satisfies
// it; this package takes the interface so the domain does not depend on the
// rate card's implementation, matching workflow.UsagePricer.
type ModelTokenPricer interface {
	Cost(modelID string, tokens UsageTokenTotals) UsageCost
}

// UnattributedModelLine is one model's gap between what the harness charged
// itself and what AO's ledger accounts for.
type UnattributedModelLine struct {
	// ModelID is the HARNESS's spelling, because that is what the residual
	// would have to be priced as.
	ModelID string
	Tokens  UsageTokenTotals
	Cost    UsageCost
	// Matched is false when no attributed model in this session shares the
	// model's canonical key -- an entire model's spend that the ledger never
	// saw at all, rather than a shortfall on one it did.
	Matched bool
}

// CompactionAccounting is the answer to "what did compacting this session
// actually cost, and how much of it does AO's ledger contain".
//
// The headline is Unattributed: the tokens the harness charged itself for and
// AO holds no event for. On a session that never compacted, that residual is
// noise -- retries, a title generated on a small model. On a session that
// compacted, the OUTPUT half of it is the summaries, and it is an order of
// magnitude larger. The type reports both halves separately for exactly that
// reason.
type CompactionAccounting struct {
	SessionID   string
	Boundaries  []CompactionBoundary
	Compactions int
	// PreTokensTotal and PostTokensTotal sum the boundaries' own figures.
	// ReductionTokensTotal is the sum of the per-boundary reductions, each
	// floored at zero before summing.
	PreTokensTotal       int64
	PostTokensTotal      int64
	ReductionTokensTotal int64
	DurationMsTotal      int64

	// Attributed is what AO's ledger holds for the session.
	Attributed     UsageTokenTotals
	AttributedCost UsageCost
	// Harness is the self-reported rollup, and HarnessObserved says whether
	// there was one at all. Every field below is meaningless without it.
	Harness         HarnessSessionTotals
	HarnessObserved bool

	// Unattributed is Harness minus Attributed, per model, floored at zero on
	// every dimension. A negative would mean AO attributed more than the
	// harness admits to, which is a signal about the pipeline rather than a
	// credit, so it is reported through UnattributedNegative instead of being
	// summed into a smaller number.
	Unattributed         UsageTokenTotals
	UnattributedByModel  []UnattributedModelLine
	UnattributedCost     UsageCost
	UnattributedNegative bool
}

// UnattributedOutputShare is the fraction of the harness's output tokens that
// AO holds no event for, and whether it is computable.
//
// This is the discriminating number. Summarization is almost entirely output,
// so on a session with no compaction this sits near zero however noisy the
// input side is, and on a session that compacted it does not.
func (a CompactionAccounting) UnattributedOutputShare() (float64, bool) {
	if !a.HarnessObserved {
		return 0, false
	}
	total := a.Harness.Totals().OutputTokens
	if total <= 0 {
		return 0, false
	}
	return 100 * float64(a.Unattributed.OutputTokens) / float64(total), true
}

// AttributedCostShare is what fraction of the harness's own money figure AO's
// calculated cost accounts for, and whether the comparison can be made.
//
// It compares two numbers computed by different parties from different rate
// cards, so it is a SHARE and never a difference to be spent: the point is the
// gap's size, not its value. On the one compacting session AO has, the share is
// 68%; on the 193-call session that never compacted it is 94%.
func (a CompactionAccounting) AttributedCostShare() (float64, bool) {
	if !a.HarnessObserved || a.Harness.ReportedCostUSD <= 0 || !a.AttributedCost.Known {
		return 0, false
	}
	return 100 * a.AttributedCost.Amount / a.Harness.ReportedCostUSD, true
}

// BuildCompactionAccounting folds the observations into the accounting. Pure:
// no IO, no clock, and the same input always produces the same result. The
// pricer may be nil, in which case every cost is Unknown and no token figure
// changes.
func BuildCompactionAccounting(in CompactionAccountingInput, pricer ModelTokenPricer) CompactionAccounting {
	out := CompactionAccounting{
		SessionID:       in.SessionID,
		Boundaries:      append([]CompactionBoundary(nil), in.Boundaries...),
		Compactions:     len(in.Boundaries),
		Harness:         in.Harness,
		HarnessObserved: in.Harness.Observed,
	}
	for _, b := range in.Boundaries {
		out.PreTokensTotal += b.PreTokens
		out.PostTokensTotal += b.PostTokens
		out.ReductionTokensTotal += b.ReductionTokens()
		out.DurationMsTotal += b.DurationMs
	}

	attributedByKey := map[string]UsageTokenTotals{}
	for _, line := range in.Attributed {
		out.Attributed = out.Attributed.Add(line.Tokens)
		key := canonicalModelKey(line.ModelID)
		attributedByKey[key] = attributedByKey[key].Add(line.Tokens)
		if pricer != nil {
			out.AttributedCost = out.AttributedCost.Add(pricer.Cost(line.ModelID, line.Tokens))
		}
	}
	if !in.Harness.Observed {
		return out
	}

	// The attributed side is CONSUMED as it is matched, never re-applied.
	//
	// Two harness buckets can share one canonical model -- a session that ran
	// "claude-opus-5" and rolled part of itself up as "claude-opus-5[1m]" is
	// exactly that shape -- and crediting the same attributed tokens against
	// both of them would subtract them twice. The residual would come out
	// SMALLER than it is, which is the one direction of error this whole file
	// exists to remove: it would under-report the spend AO cannot see.
	for _, model := range in.Harness.Models {
		key := canonicalModelKey(model.ModelID)
		credit, matched := attributedByKey[key]
		residual, used := consumeCredit(model.Tokens, credit)
		// Write the remainder back so the next bucket sharing this key can
		// only claim what is left. The entry is updated, never deleted, so
		// `matched` keeps meaning "some attributed line named this model".
		attributedByKey[key] = subtractExact(credit, used)
		if residual == (UsageTokenTotals{}) {
			continue
		}
		line := UnattributedModelLine{ModelID: model.ModelID, Tokens: residual, Matched: matched}
		// Priced as the HARNESS names the model, never as the attributed one.
		// A rate card that does not cover "claude-opus-5[1m]" leaves this
		// unknown, and that is the correct answer rather than the base model's
		// rate wearing a long-context model's token count.
		if pricer != nil {
			line.Cost = pricer.Cost(model.ModelID, residual)
			out.UnattributedCost = out.UnattributedCost.Add(line.Cost)
		}
		out.Unattributed = out.Unattributed.Add(residual)
		out.UnattributedByModel = append(out.UnattributedByModel, line)
	}
	// Credit nobody could absorb means AO holds events the harness does not
	// admit to. That is a statement about the pipeline, not a discount, so it
	// is flagged rather than netted off against anything.
	for _, leftover := range attributedByKey {
		if billedDimensions(leftover) != (UsageTokenTotals{}) {
			out.UnattributedNegative = true
			break
		}
	}
	sort.SliceStable(out.UnattributedByModel, func(i, j int) bool {
		return out.UnattributedByModel[i].ModelID < out.UnattributedByModel[j].ModelID
	})
	return out
}

// consumeCredit subtracts as much of credit from have as have can absorb,
// returning what is left of have and how much of the credit was actually used.
//
// Per dimension, because the dimensions are priced apart and a shortfall on one
// must not be paid for out of another.
//
// EventCount and the reasoning fields are deliberately not carried into a
// residual: a count of events AO does not have is zero events, and a reasoning
// figure the harness reports separately is not comparable to one folded out of
// per-call usage.
func consumeCredit(have, credit UsageTokenTotals) (residual, used UsageTokenTotals) {
	take := func(h, c int64) (int64, int64) {
		if c > h {
			c = h
		}
		if c < 0 {
			c = 0
		}
		return h - c, c
	}
	residual.InputTokens, used.InputTokens = take(have.InputTokens, credit.InputTokens)
	residual.UncachedInputTokens, used.UncachedInputTokens = take(have.UncachedInputTokens, credit.UncachedInputTokens)
	residual.CacheReadTokens, used.CacheReadTokens = take(have.CacheReadTokens, credit.CacheReadTokens)
	residual.CacheWriteTokens, used.CacheWriteTokens = take(have.CacheWriteTokens, credit.CacheWriteTokens)
	residual.OutputTokens, used.OutputTokens = take(have.OutputTokens, credit.OutputTokens)
	return residual, used
}

// billedDimensions keeps only the five dimensions a residual is made of, so a
// leftover event count -- which is bookkeeping, not spend -- cannot be mistaken
// for tokens AO attributed and the harness did not report.
func billedDimensions(t UsageTokenTotals) UsageTokenTotals {
	return UsageTokenTotals{
		InputTokens:         t.InputTokens,
		UncachedInputTokens: t.UncachedInputTokens,
		CacheReadTokens:     t.CacheReadTokens,
		CacheWriteTokens:    t.CacheWriteTokens,
		OutputTokens:        t.OutputTokens,
	}
}

// subtractExact is a - b on the five billed dimensions, with no floor, used
// only where b is known to be no larger than a.
func subtractExact(a, b UsageTokenTotals) UsageTokenTotals {
	return UsageTokenTotals{
		InputTokens:         a.InputTokens - b.InputTokens,
		UncachedInputTokens: a.UncachedInputTokens - b.UncachedInputTokens,
		CacheReadTokens:     a.CacheReadTokens - b.CacheReadTokens,
		CacheWriteTokens:    a.CacheWriteTokens - b.CacheWriteTokens,
		OutputTokens:        a.OutputTokens - b.OutputTokens,
	}
}
