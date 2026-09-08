package domain

import (
	"strings"
	"time"
)

// review_depth.go — how much scrutiny a delivered change gets.
//
// AO already had two independent, durable execution axes: the execution
// strategy (how much orchestration a run gets — task/autonomous/master) and the
// approval policy (who approves and drives). Neither answers a third question
// that was, until now, not representable at all: how much reviewing the change
// a worker delivered should receive.
//
// Before this file there was exactly one answer. Checkpoint 8I's ReviewPolicy
// decides WHETHER a reviewer runs, deterministically and conservatively, but
// when it says yes the reviewer that runs is always the same full independent
// pass. That is the right shape for a migration and a wildly disproportionate
// one for three lines in one package — and because ReviewPolicy defaults to
// REQUIRED, the disproportionate case is the common one.
//
// Review depth is that third axis, and it is deliberately SEPARATE from the
// other two for the same reason "manual" was separated out of the strategy
// vocabulary in P1-A: folding scrutiny into orchestration would make one of
// them unable to move without the other.
//
// The safety property is one line, stated once, in ResolveReviewDepth: the
// effective depth is the HIGHER of what was asked for and what the change's
// deterministic risk tier requires. A request can always make a review deeper
// and can never make it shallower than the risk allows.

// ReviewDepth is how much scrutiny a review pass applies. Exactly three
// members, ordered: none < light < deep.
type ReviewDepth string

const (
	// ReviewDepthNone runs no reviewer process at all. AO's own deterministic
	// Verify step remains the gate. It is reachable only where the risk tier is
	// `low`, which is exactly the set of changes ReviewPolicy already resolves
	// to ReviewSkipped — so this value can never skip a review AO would
	// otherwise have run.
	ReviewDepthNone ReviewDepth = "none"
	// ReviewDepthLight is a bounded review: judge the diff against the task's
	// acceptance criteria and the evidence AO supplies, without re-reading the
	// repository or running a full suite. A light reviewer that finds itself
	// out of its depth escalates rather than approving.
	ReviewDepthLight ReviewDepth = "light"
	// ReviewDepthDeep is the full independent review pass AO has always run.
	// Its prompt is byte-identical to the pre-P5-A one, so nothing that runs
	// deep can have changed.
	ReviewDepthDeep ReviewDepth = "deep"
)

// Valid reports whether d is one of the three canonical depths.
func (d ReviewDepth) Valid() bool {
	switch d {
	case ReviewDepthNone, ReviewDepthLight, ReviewDepthDeep:
		return true
	default:
		return false
	}
}

// rank orders the depths so "at least this deep" is a comparison rather than a
// switch repeated at every call site. An invalid depth ranks below none, so it
// can never satisfy a floor by accident.
func (d ReviewDepth) rank() int {
	switch d {
	case ReviewDepthNone:
		return 0
	case ReviewDepthLight:
		return 1
	case ReviewDepthDeep:
		return 2
	default:
		return -1
	}
}

// AtLeast reports whether d applies at least as much scrutiny as floor.
func (d ReviewDepth) AtLeast(floor ReviewDepth) bool { return d.rank() >= floor.rank() }

// DeeperOf returns whichever of a and b applies more scrutiny. This is the
// clamp, and every non-degradability guarantee in the design reduces to it.
func DeeperOf(a, b ReviewDepth) ReviewDepth {
	if b.rank() > a.rank() {
		return b
	}
	return a
}

// RequestedReviewDepth is what a caller may ASK for: any canonical depth, or
// "auto" to take the default for the run's execution strategy. A separate type
// from ReviewDepth because "auto" is never an answer, only a question.
type RequestedReviewDepth string

const (
	// RequestedReviewDepthUnspecified is an omitted field, which is treated
	// exactly as "auto" by the API layer.
	RequestedReviewDepthUnspecified RequestedReviewDepth = ""
	// RequestedReviewDepthAuto asks for the execution strategy's default.
	RequestedReviewDepthAuto RequestedReviewDepth = "auto"
	// RequestedReviewDepthNone explicitly asks for no reviewer.
	RequestedReviewDepthNone RequestedReviewDepth = "none"
	// RequestedReviewDepthLight explicitly asks for a bounded review.
	RequestedReviewDepthLight RequestedReviewDepth = "light"
	// RequestedReviewDepthDeep explicitly asks for a full review.
	RequestedReviewDepthDeep RequestedReviewDepth = "deep"
)

// Valid reports whether r is an accepted request value. The unspecified zero
// value is not one: a caller that omitted the field is handled by the
// transport layer's mapping to auto, not by pretending it asked for something.
func (r RequestedReviewDepth) Valid() bool {
	switch r {
	case RequestedReviewDepthAuto, RequestedReviewDepthNone,
		RequestedReviewDepthLight, RequestedReviewDepthDeep:
		return true
	default:
		return false
	}
}

// Explicit returns the canonical depth r names, and whether it names one.
// "auto" names none.
func (r RequestedReviewDepth) Explicit() (ReviewDepth, bool) {
	d := ReviewDepth(r)
	return d, d.Valid()
}

// NormalizeRequestedReviewDepth trims and lower-cases a caller-supplied value,
// leaving anything unrecognised unchanged so the caller can reject it rather
// than silently running something the user did not ask for.
func NormalizeRequestedReviewDepth(raw string) RequestedReviewDepth {
	return RequestedReviewDepth(strings.ToLower(strings.TrimSpace(raw)))
}

// ReviewRiskTier is the deterministic risk band a delivered change falls into.
// It is derived — by workflow.ReviewRiskTierFor — from the ReviewPolicyDecision
// AO already computes and persists at review cycle 1. There is no second risk
// evaluation and no model call: a tier is a reading of facts AO already has.
type ReviewRiskTier string

const (
	// ReviewRiskLow is a change AO can already prove safe deterministically:
	// docs-only with verification coverage, or a single file with an exact
	// content/digest check. These are exactly the changes ReviewPolicy resolves
	// to ReviewSkipped.
	ReviewRiskLow ReviewRiskTier = "low"
	// ReviewRiskStandard is ordinary code work: it must be looked at, and a
	// bounded look is enough unless something escalates it.
	ReviewRiskStandard ReviewRiskTier = "standard"
	// ReviewRiskHigh is a change touching security, authentication, payments,
	// migrations, concurrency, infrastructure, public contracts, dependency
	// configuration, or declaring destructive intent. It is never reviewed
	// with less than a full independent pass, whatever anybody selected.
	ReviewRiskHigh ReviewRiskTier = "high"
)

// Valid reports whether t is one of the three tiers.
func (t ReviewRiskTier) Valid() bool {
	switch t {
	case ReviewRiskLow, ReviewRiskStandard, ReviewRiskHigh:
		return true
	default:
		return false
	}
}

// MinimumDepth is the floor this tier imposes. It is the whole
// non-degradability rule, expressed once.
//
// An unrecognised tier floors at deep, not at none: a risk band AO cannot name
// is not a risk band AO may discount.
func (t ReviewRiskTier) MinimumDepth() ReviewDepth {
	switch t {
	case ReviewRiskLow:
		return ReviewDepthNone
	case ReviewRiskStandard:
		return ReviewDepthLight
	case ReviewRiskHigh:
		return ReviewDepthDeep
	default:
		return ReviewDepthDeep
	}
}

// DeeperTier returns whichever of a and b is the higher risk band, so a tier
// computed over several reasons is the maximum rather than the last one seen.
func DeeperTier(a, b ReviewRiskTier) ReviewRiskTier {
	rank := func(t ReviewRiskTier) int {
		switch t {
		case ReviewRiskLow:
			return 0
		case ReviewRiskStandard:
			return 1
		case ReviewRiskHigh:
			return 2
		default:
			return 2
		}
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// ReviewDepthSource records HOW a run's effective depth was arrived at.
type ReviewDepthSource string

const (
	// ReviewDepthExplicit means a person or an API client named the depth and
	// the risk tier allowed it.
	ReviewDepthExplicit ReviewDepthSource = "explicit"
	// ReviewDepthPolicy means the depth came from the execution strategy's
	// default and the risk tier allowed it.
	ReviewDepthPolicy ReviewDepthSource = "policy"
	// ReviewDepthClamped means the requested depth was shallower than the risk
	// tier permits and was raised. The request is still recorded, so the
	// decision reads as "you asked for X, the risk required Y" rather than as
	// a choice nobody made.
	ReviewDepthClamped ReviewDepthSource = "clamped"
	// ReviewDepthEscalated means a later, durable fact raised the depth above
	// what cycle 1 resolved: the reviewer asked for it, or the light loop
	// stopped converging.
	ReviewDepthEscalated ReviewDepthSource = "escalated"
	// ReviewDepthRecovered means the run predates this model and its depth was
	// read as the behaviour it already had (deep). A reading of history, never
	// a claim that somebody chose it.
	ReviewDepthRecovered ReviewDepthSource = "recovered"
)

// Recorded reports whether src names a real decision.
func (src ReviewDepthSource) Recorded() bool {
	switch src {
	case ReviewDepthExplicit, ReviewDepthPolicy, ReviewDepthClamped,
		ReviewDepthEscalated, ReviewDepthRecovered:
		return true
	default:
		return false
	}
}

// ReviewDepthReason is a stable, machine-checkable explanation code, so a
// decision taken today stays explainable after the rules change.
type ReviewDepthReason string

const (
	// ReviewDepthReasonStrategyDefault — the depth is the run's execution
	// strategy's default and nothing raised it.
	ReviewDepthReasonStrategyDefault ReviewDepthReason = "strategy_default"
	// ReviewDepthReasonExplicitRequest — a caller named this depth and the
	// risk tier allowed it.
	ReviewDepthReasonExplicitRequest ReviewDepthReason = "explicit_request"
	// ReviewDepthReasonClampedByRiskTier — the request was shallower than the
	// change's risk tier permits.
	ReviewDepthReasonClampedByRiskTier ReviewDepthReason = "clamped_by_risk_tier"
	// ReviewDepthReasonEscalatedByReviewer — a light reviewer declared the
	// change outside what a bounded review can judge.
	ReviewDepthReasonEscalatedByReviewer ReviewDepthReason = "escalated_by_reviewer"
	// ReviewDepthReasonEscalatedByCycles — the light review/fix loop ran its
	// budget of cycles without converging.
	ReviewDepthReasonEscalatedByCycles ReviewDepthReason = "escalated_by_cycles"
	// ReviewDepthReasonLegacyDefault — a run created before this model, read
	// as the depth it already ran at.
	ReviewDepthReasonLegacyDefault ReviewDepthReason = "legacy_default"
)

// ReviewDepthPolicyVersion is the version of the depth policy implemented by
// ResolveReviewDepth and by the tier mapping in internal/workflow. It is
// stamped into every recorded decision so a decision stays replayable after
// the rules change. Bump it whenever the tiers or the floors change.
const ReviewDepthPolicyVersion = "review-depth/v1"

// ReviewDepthMaxLightCycles is how many changes_requested cycles a light review
// may produce before the loop is judged not to be converging and the next
// cycle is dispatched deep. Two: one round of findings and one round of "the
// findings were not addressed" is information; a third is a bounded reviewer
// being asked a question it cannot answer.
const ReviewDepthMaxLightCycles = 2

// ReviewEscalationMarker is the exact prefix a light reviewer puts on the first
// line of a changes_requested body to say the change is outside what a bounded
// review can honestly judge. It is matched deterministically by AO; the
// reviewer supplies the judgement, AO supplies the rule.
const ReviewEscalationMarker = "AO-ESCALATE:"

// ReviewDepthPolicySnapshot is the frozen review-depth policy embedded in a
// run's WorkflowPolicy, alongside the strategy/repair/autonomy/usage snapshots
// already frozen there. It records what the run ASKED for, not what any
// particular review resolved to: the effective depth depends on facts about
// the delivered change that do not exist until a worker has finished, and
// freezing an answer to a question nobody could ask yet would be inventing it.
type ReviewDepthPolicySnapshot struct {
	Version string `json:"version,omitempty"`
	// Requested is the concrete depth this run asks for. Never "auto": auto is
	// resolved to a strategy default by DefaultReviewDepthPolicy before it is
	// frozen, so no reader has to know what the default was at creation time.
	Requested ReviewDepth `json:"requested,omitempty"`
	// Source is whether a person named Requested or the strategy default
	// supplied it.
	Source ReviewDepthSource `json:"source,omitempty"`
	At     time.Time         `json:"at,omitempty"`
}

// Recorded reports whether this snapshot is a real durable decision, as
// opposed to the zero value a run created before this model carries.
func (s ReviewDepthPolicySnapshot) Recorded() bool {
	return s.Requested.Valid() && s.Version != ""
}

// DefaultRequestedReviewDepth is the depth an execution strategy asks for when
// nobody chose one.
//
//   - task asks for none, because a bounded low-risk change is what the task
//     strategy exists for. The risk tier raises it to light for ordinary code
//     and to deep for anything sensitive, so "none" is only ever effective
//     where ReviewPolicy already skips the reviewer entirely.
//   - autonomous asks for light: ordinary multi-step project work gets a
//     bounded second opinion by default.
//   - master asks for deep. max(deep, anything) is deep, which is why this
//     change cannot alter what any master run does.
//
// An unrecognised strategy asks for deep, for the same reason an unrecognised
// tier floors at deep.
func DefaultRequestedReviewDepth(strategy ExecutionStrategy) ReviewDepth {
	switch strategy {
	case ExecutionStrategyTask:
		return ReviewDepthNone
	case ExecutionStrategyAutonomous:
		return ReviewDepthLight
	case ExecutionStrategyMaster:
		return ReviewDepthDeep
	default:
		return ReviewDepthDeep
	}
}

// DefaultReviewDepthPolicy is the snapshot a run gets when nobody named a
// depth: its strategy's default, recorded as a policy selection rather than
// left as a fallback every reader has to re-derive.
func DefaultReviewDepthPolicy(strategy ExecutionStrategy, now time.Time) ReviewDepthPolicySnapshot {
	return ReviewDepthPolicySnapshot{
		Version:   ReviewDepthPolicyVersion,
		Requested: DefaultRequestedReviewDepth(strategy),
		Source:    ReviewDepthPolicy,
		At:        now,
	}
}

// ReviewDepthDecision is the durable, explainable result of resolving one
// review step's depth: what was asked for, what the risk required, what will
// actually run, and why. Persisted as its own append-only checkpoint next to
// the review_policy_decision it was derived from.
type ReviewDepthDecision struct {
	PolicyVersion string `json:"policyVersion"`
	// Requested is the run's frozen request.
	Requested ReviewDepth `json:"requested"`
	// Effective is what this review cycle actually runs at.
	Effective ReviewDepth `json:"effective"`
	// Source is how Effective was arrived at.
	Source ReviewDepthSource `json:"source"`
	// Reason is the stable explanation code.
	Reason ReviewDepthReason `json:"reason"`
	// RiskTier is the tier the floor came from.
	RiskTier ReviewRiskTier `json:"riskTier"`
	// RiskReasons are the ReviewPolicy reason codes the tier was read from,
	// carried as strings so this type stays in domain without depending on
	// internal/workflow's reason vocabulary. They make the decision replayable
	// rather than merely asserted.
	RiskReasons []string  `json:"riskReasons,omitempty"`
	DecidedAt   time.Time `json:"decidedAt"`
}

// Recorded reports whether this decision is a real one rather than a zero
// value.
func (d ReviewDepthDecision) Recorded() bool {
	return d.Effective.Valid() && d.Source.Recorded()
}

// ResolveReviewDepth is the whole depth policy, and it is a pure function:
// same inputs, same answer, forever.
//
//	effective = max(requested, tier.MinimumDepth())
//
// That single line is the non-degradability guarantee. A caller may always ask
// for MORE scrutiny than the risk requires and it is honoured; a caller may
// never receive less, and when the clamp fires the request is still recorded
// so the decision explains itself instead of looking like somebody's choice.
func ResolveReviewDepth(snapshot ReviewDepthPolicySnapshot, tier ReviewRiskTier, riskReasons []string, now time.Time) ReviewDepthDecision {
	requested := snapshot.Requested
	source := snapshot.Source
	if !requested.Valid() {
		// A snapshot from before this model, or one that recorded nothing
		// readable. The depth those runs already had is deep, and reading them
		// as anything cheaper would silently downgrade work nobody chose to
		// downgrade.
		requested = ReviewDepthDeep
		source = ReviewDepthRecovered
	}
	if !source.Recorded() {
		source = ReviewDepthPolicy
	}

	decision := ReviewDepthDecision{
		PolicyVersion: ReviewDepthPolicyVersion,
		Requested:     requested,
		RiskTier:      tier,
		RiskReasons:   riskReasons,
		DecidedAt:     now,
	}

	floor := tier.MinimumDepth()
	effective := DeeperOf(requested, floor)
	decision.Effective = effective

	switch {
	case effective != requested:
		decision.Source = ReviewDepthClamped
		decision.Reason = ReviewDepthReasonClampedByRiskTier
	case source == ReviewDepthRecovered:
		decision.Source = ReviewDepthRecovered
		decision.Reason = ReviewDepthReasonLegacyDefault
	case source == ReviewDepthExplicit:
		decision.Source = ReviewDepthExplicit
		decision.Reason = ReviewDepthReasonExplicitRequest
	default:
		decision.Source = ReviewDepthPolicy
		decision.Reason = ReviewDepthReasonStrategyDefault
	}
	return decision
}

// EscalateReviewDepth raises a resolved decision to deep and records why. It
// only ever deepens: an escalation applied to a decision that is already deep
// returns it unchanged, so a repeated escalation is idempotent and can never
// rewrite the reason a stricter decision was already taken for.
func EscalateReviewDepth(decision ReviewDepthDecision, reason ReviewDepthReason, now time.Time) ReviewDepthDecision {
	if decision.Effective.AtLeast(ReviewDepthDeep) {
		return decision
	}
	decision.Effective = ReviewDepthDeep
	decision.Source = ReviewDepthEscalated
	decision.Reason = reason
	decision.DecidedAt = now
	return decision
}
