package domain

// RoutingPolicyVersion is the fixed Checkpoint 8L policy version. Persisted
// alongside every RoutingDecision so a past decision remains explainable
// against the rules that actually produced it, even after a future
// checkpoint changes the defaults ("no recalcules historia con policy
// futura" — the same rule ReviewPolicyVersion already follows).
const RoutingPolicyVersion = "v1"

// RoutingReason is a closed, stable, machine-checkable code explaining why
// ExecutionRouter reached a decision. Never free text (checkpoint brief
// §15): reasons must remain comparable across runs and testable.
type RoutingReason string

// The reason codes. Each is written onto the RoutingDecision it explains, and
// stays valid for that decision forever -- a later checkpoint may add codes but
// must not repurpose one.
const (
	RoutingReasonPreferredForComplexity      RoutingReason = "preferred_for_complexity"
	RoutingReasonCrossProviderReview         RoutingReason = "cross_provider_review"
	RoutingReasonPreferredUnavailable        RoutingReason = "preferred_unavailable"
	RoutingReasonFallbackSelected            RoutingReason = "fallback_selected"
	RoutingReasonProviderCooldown            RoutingReason = "provider_cooldown"
	RoutingReasonProviderUnavailable         RoutingReason = "provider_unavailable"
	RoutingReasonReviewIndependenceRequired  RoutingReason = "review_independence_required"
	RoutingReasonOnlyEligibleProvider        RoutingReason = "only_eligible_provider"
	RoutingReasonWaitingForCapacity          RoutingReason = "waiting_for_capacity"
	RoutingReasonPlannerPolicy               RoutingReason = "planner_policy"
	RoutingReasonSameProviderFallbackAllowed RoutingReason = "same_provider_fallback_allowed"

	// Checkpoint 8P-C's UserExecutionPolicy-driven reason codes (checkpoint
	// brief §23).
	RoutingReasonUserPreferredProvider RoutingReason = "user_preferred_provider"
	RoutingReasonProviderDisabled      RoutingReason = "provider_disabled"
	RoutingReasonProfileNotConnected   RoutingReason = "profile_not_connected"
	RoutingReasonCapabilityMissing     RoutingReason = "capability_missing"
	RoutingReasonUnsupportedProvider   RoutingReason = "unsupported_provider"

	// RoutingReasonPolicyPriorityCompleted (Checkpoint 8P-E.13A.4) marks a
	// decision that selected a profile the user's stored priority list never
	// mentioned. A priority list is a PREFERENCE ORDER, not an allowlist: a
	// profile connected after the policy row was written is otherwise
	// invisible to routing forever, which is exactly how an enabled,
	// authenticated, reviewer-capable Codex profile could leave a high-risk
	// independent review deadlocked in waiting_for_capacity. Unlisted
	// eligible profiles are therefore appended after every listed one (never
	// reordered ahead of a user preference) and this code records that the
	// selection came from that tail.
	RoutingReasonPolicyPriorityCompleted RoutingReason = "policy_priority_completed"
	// RoutingReasonCapacityProbeIndeterminate marks a decision taken while at
	// least one eligible profile's active capacity probe could not conclude
	// (probe unavailable, CLI answered nothing recognizable, or the probe was
	// throttled after a recent indeterminate attempt).
	RoutingReasonCapacityProbeIndeterminate RoutingReason = "capacity_probe_indeterminate"
)

// Valid reports whether a reason code is part of the closed V1 enum —
// callers must never persist/display an arbitrary string as a primary
// reason (checkpoint brief §15).
func (r RoutingReason) Valid() bool {
	switch r {
	case RoutingReasonPreferredForComplexity,
		RoutingReasonCrossProviderReview,
		RoutingReasonPreferredUnavailable,
		RoutingReasonFallbackSelected,
		RoutingReasonProviderCooldown,
		RoutingReasonProviderUnavailable,
		RoutingReasonReviewIndependenceRequired,
		RoutingReasonOnlyEligibleProvider,
		RoutingReasonWaitingForCapacity,
		RoutingReasonPlannerPolicy,
		RoutingReasonSameProviderFallbackAllowed,
		RoutingReasonUserPreferredProvider,
		RoutingReasonProviderDisabled,
		RoutingReasonProfileNotConnected,
		RoutingReasonCapabilityMissing,
		RoutingReasonUnsupportedProvider,
		RoutingReasonPolicyPriorityCompleted,
		RoutingReasonCapacityProbeIndeterminate:
		return true
	default:
		return false
	}
}

// RoutingPolicy is the small, explicit set of V1 routing knobs (checkpoint
// brief §21): which harness is preferred per role/complexity, and whether
// cross-provider review independence may relax to a same-provider fallback.
// It intentionally does not encode Claude Max 5x, any commercial quota, or a
// price multiplier — only a configurable preference (checkpoint brief §6).
type RoutingPolicy struct {
	Version string `json:"version"`
	// PlannerPreference is the harness preferred for the planner role. V1
	// keeps this simple (checkpoint brief §10): Claude by default, but read
	// from policy rather than hardcoded so a future checkpoint can change it
	// without touching call sites.
	PlannerPreference AgentHarness `json:"plannerPreference"`
	// TrivialWorkerPreference/NormalWorkerPreference/HighRiskWorkerPreference
	// are the worker-role harness preference per TaskComplexity tier
	// (checkpoint brief §7).
	TrivialWorkerPreference  AgentHarness `json:"trivialWorkerPreference"`
	NormalWorkerPreference   AgentHarness `json:"normalWorkerPreference"`
	HighRiskWorkerPreference AgentHarness `json:"highRiskWorkerPreference"`
	// CrossProviderReviewRequired forces the reviewer role to prefer the
	// opposite provider from the implementer (checkpoint brief §8). V1
	// default true. AllowSameProviderReviewFallbackForNormal is the single
	// escape hatch, and only for normal complexity — high-risk cross-
	// provider independence can never be relaxed by policy (checkpoint
	// brief §8: "NO usar same-provider reviewer automáticamente para
	// high-risk").
	CrossProviderReviewRequired              bool `json:"crossProviderReviewRequired"`
	AllowSameProviderReviewFallbackForNormal bool `json:"allowSameProviderReviewFallbackForNormal"`
}

// DefaultRoutingPolicy is Checkpoint 8L's fixed V1 default: trivial tasks
// prefer Codex, normal/high-risk tasks prefer Claude Code, the planner
// prefers Claude Code, and reviewer independence is required by default
// (checkpoint brief §7/§8). This encodes today's "Claude Max 5x" usage
// pattern only as a configurable preference, never as a hardcoded quota
// assumption (checkpoint brief §6).
func DefaultRoutingPolicy() RoutingPolicy {
	return RoutingPolicy{
		Version:                     RoutingPolicyVersion,
		PlannerPreference:           HarnessClaudeCode,
		TrivialWorkerPreference:     HarnessCodex,
		NormalWorkerPreference:      HarnessClaudeCode,
		HighRiskWorkerPreference:    HarnessClaudeCode,
		CrossProviderReviewRequired: true,
	}
}

// RoutingDecision is the durable, explainable result of one ExecutionRouter
// evaluation (checkpoint brief §4/§14). Persisted verbatim — policy version,
// reason codes and the capacity snapshot consulted — never reduced to a bare
// harness string, so an attempt can always answer "why this provider" after
// the fact.
type RoutingDecision struct {
	Role             WorkflowRole    `json:"role"`
	Complexity       string          `json:"complexity,omitempty"`
	PreferredHarness AgentHarness    `json:"preferredHarness"`
	SelectedHarness  AgentHarness    `json:"selectedHarness,omitempty"`
	FallbackOrder    []AgentHarness  `json:"fallbackOrder,omitempty"`
	ReasonCodes      []RoutingReason `json:"reasonCodes"`
	PolicyVersion    string          `json:"policyVersion"`
	Waiting          bool            `json:"waiting"`
	// PreferredProfileID/SelectedProfileID (Checkpoint 8P-C) are the exact
	// ProviderProfile the decision preferred/selected, not just its harness
	// -- required so capacity/usage facts and the frontend routing
	// explanation (checkpoint brief §24) can point at the specific
	// connection used, not merely "which provider family".
	PreferredProfileID      ProviderProfileID                   `json:"preferredProfileId,omitempty"`
	SelectedProfileID       ProviderProfileID                   `json:"selectedProfileId,omitempty"`
	FallbackProfileOrder    []ProviderProfileID                 `json:"fallbackProfileOrder,omitempty"`
	CapacityStateAtDecision map[AgentHarness]CapacityState      `json:"capacityStateAtDecision,omitempty"`
	CapacityStateByProfile  map[ProviderProfileID]CapacityState `json:"capacityStateByProfile,omitempty"`
}
