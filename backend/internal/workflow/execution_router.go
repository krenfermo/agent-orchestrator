package workflow

import (
	stdctx "context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// RoutingRequest is ExecutionRouter's pure input (Checkpoint 8P-C rewrite of
// 8L's fixed two-provider version): every fact a routing decision may
// consult, gathered by the caller from already-durable state. RouteExecution
// itself performs no IO, so the same request always produces the same
// decision.
//
// The fixed Claude<->Codex universe (knownRoutingHarnesses/oppositeHarness)
// is gone: routing now walks the caller's domain.UserExecutionPolicy
// priority lists over their own EligibleProfiles, so adding a new provider
// only ever requires a real adapter + a ProviderProfile, never a router
// source change (checkpoint brief §19).
type RoutingRequest struct {
	Role domain.WorkflowRole
	// Complexity is recorded on the decision for explainability/telemetry
	// only. It no longer selects a different priority list on its own
	// (checkpoint brief §17: user order must never be silently overridden by
	// a complexity heuristic) -- a future policy may reintroduce
	// complexity-based ordering explicitly, but nothing in V1 does.
	Complexity TaskComplexity
	// CurrentImplementerProvider is the provider family (e.g. "anthropic",
	// "openai") of the profile whose work is being reviewed (reviewer/
	// decision-resolver role only). Comparing providers rather than exact
	// profile IDs matches independence's real intent: a different profile of
	// the SAME provider is not independent.
	CurrentImplementerProvider string
	Policy                     domain.ExecutionPolicySnapshot
	// EligibleProfiles is the caller-precomputed set of the workflow owner's
	// own profiles that are enabled, connected, and advertise the role's
	// required capability (domain.EligibleProfiles) -- router only consults
	// capacity beyond this, never re-derives ownership/capability itself.
	EligibleProfiles map[domain.ProviderProfileID]domain.ProviderProfile
	// IneligibleReasons explains, for a priority-list entry NOT present in
	// EligibleProfiles, exactly why (provider_disabled/profile_not_connected/
	// capability_missing/unsupported_provider) -- used only to produce an
	// accurate reason code when the single most-preferred entry is the one
	// filtered out; entries beyond the first are silently skipped the same
	// way an unavailable one is.
	IneligibleReasons map[domain.ProviderProfileID]domain.RoutingReason
	Capacity          map[domain.ProviderProfileID]domain.CapacityState
}

// capacityEligible reports whether state permits dispatch: available/unknown
// are eligible, limited/cooldown/unavailable are not. Unknown defaults to
// eligible ("elegible conservadoramente si instalado/autenticado salvo fallo
// activo") — RouteExecution has no probe of its own, so an unrecorded
// profile is treated the same way domain.AgentHealth.Available already
// treats AgentHealthUnknown.
func capacityEligible(state domain.CapacityState) bool {
	switch state {
	case domain.CapacityAvailable, domain.CapacityUnknown:
		return true
	default:
		return false
	}
}

func capacityReason(state domain.CapacityState) domain.RoutingReason {
	if state == domain.CapacityCooldown {
		return domain.RoutingReasonProviderCooldown
	}
	return domain.RoutingReasonProviderUnavailable
}

// RouteExecution is Checkpoint 8P-C's pure, deterministic routing decision.
// It never calls an LLM, never performs IO and never mutates state —
// callers gather RoutingRequest's facts first (eligible profiles, capacity
// snapshot, the caller's live/default UserExecutionPolicy) and persist the
// returned RoutingDecision themselves.
func RouteExecution(req RoutingRequest) domain.RoutingDecision {
	decision := domain.RoutingDecision{
		Role:                   req.Role,
		Complexity:             string(req.Complexity),
		PolicyVersion:          domain.UserExecutionPolicyVersion,
		CapacityStateByProfile: req.Capacity,
	}
	priority := req.Policy.PriorityFor(req.Role)
	switch req.Role {
	case domain.WorkflowRolePlanner, domain.WorkflowRoleWorker, domain.WorkflowRoleFixWorker:
		return routeByPriority(decision, priority, req)
	case domain.WorkflowRoleReviewer, domain.WorkflowRoleDecisionResolver:
		return routeCrossProvider(decision, priority, req)
	default:
		decision.Waiting = true
		decision.ReasonCodes = []domain.RoutingReason{domain.RoutingReasonWaitingForCapacity}
		return decision
	}
}

// routeByPriority applies the planner/worker rule: walk the role's priority
// list in order, selecting the first entry that is both eligible (owned,
// enabled, connected, capable) and capacity-eligible.
// domain.FallbackWaitForPreferred stops at the first eligible-but-
// capacity-blocked entry rather than silently walking to the next one
// (checkpoint brief §2: "Codex sólo cuando Claude no sea elegible" is an
// opt-in behavior, not the only one).
func routeByPriority(decision domain.RoutingDecision, priority []domain.ProviderProfileID, req RoutingRequest) domain.RoutingDecision {
	if len(priority) == 0 {
		decision.Waiting = true
		decision.ReasonCodes = []domain.RoutingReason{domain.RoutingReasonWaitingForCapacity}
		return decision
	}
	decision.PreferredProfileID = priority[0]
	if p, ok := req.EligibleProfiles[priority[0]]; ok {
		decision.PreferredHarness = p.Harness
	}
	for i, id := range priority {
		profile, ok := req.EligibleProfiles[id]
		if !ok {
			continue
		}
		if !capacityEligible(req.Capacity[id]) {
			if req.Policy.FallbackBehavior == domain.FallbackWaitForPreferred {
				break
			}
			continue
		}
		decision.SelectedProfileID = id
		decision.SelectedHarness = profile.Harness
		if i == 0 {
			decision.ReasonCodes = []domain.RoutingReason{domain.RoutingReasonUserPreferredProvider}
		} else {
			decision.FallbackProfileOrder = append([]domain.ProviderProfileID{}, priority[1:i+1]...)
			decision.ReasonCodes = []domain.RoutingReason{domain.RoutingReasonPreferredUnavailable, domain.RoutingReasonFallbackSelected}
		}
		return decision
	}
	decision.Waiting = true
	decision.ReasonCodes = append(preferredIneligibilityReasons(priority[0], req), domain.RoutingReasonWaitingForCapacity)
	return decision
}

// preferredIneligibilityReasons explains why the single most-preferred
// priority entry was not selected, for the waiting decision's reason trail.
func preferredIneligibilityReasons(preferred domain.ProviderProfileID, req RoutingRequest) []domain.RoutingReason {
	if reason, ok := req.IneligibleReasons[preferred]; ok {
		return []domain.RoutingReason{reason}
	}
	if _, ok := req.EligibleProfiles[preferred]; ok {
		return []domain.RoutingReason{domain.RoutingReasonPreferredUnavailable, capacityReason(req.Capacity[preferred])}
	}
	return nil
}

// routeCrossProvider applies the reviewer/decision-resolver independence
// rule: prefer the highest-priority profile whose provider differs from
// CurrentImplementerProvider. domain.ReviewIndependenceAllowSameProviderFallback
// only ever applies for non-high-risk complexity — high-risk independence
// can never be relaxed by policy, mirroring 8L's original hard rule
// ("NO usar same-provider reviewer automáticamente para high-risk").
func routeCrossProvider(decision domain.RoutingDecision, priority []domain.ProviderProfileID, req RoutingRequest) domain.RoutingDecision {
	if len(priority) == 0 {
		decision.Waiting = true
		decision.ReasonCodes = []domain.RoutingReason{domain.RoutingReasonWaitingForCapacity}
		return decision
	}
	decision.PreferredProfileID = priority[0]
	if p, ok := req.EligibleProfiles[priority[0]]; ok {
		decision.PreferredHarness = p.Harness
	}
	independent := func(p domain.ProviderProfile) bool { return p.Provider != req.CurrentImplementerProvider }
	if sel, ok := selectFromPriority(priority, req, independent); ok {
		decision.SelectedProfileID = sel.ID
		decision.SelectedHarness = sel.Harness
		decision.ReasonCodes = []domain.RoutingReason{domain.RoutingReasonCrossProviderReview, domain.RoutingReasonReviewIndependenceRequired}
		return decision
	}
	allowSameProvider := req.Policy.ReviewIndependence == domain.ReviewIndependenceAllowSameProviderFallback && req.Complexity != ComplexityHighRisk
	if allowSameProvider {
		any := func(domain.ProviderProfile) bool { return true }
		if sel, ok := selectFromPriority(priority, req, any); ok {
			decision.SelectedProfileID = sel.ID
			decision.SelectedHarness = sel.Harness
			decision.ReasonCodes = []domain.RoutingReason{domain.RoutingReasonReviewIndependenceRequired, domain.RoutingReasonPreferredUnavailable, domain.RoutingReasonSameProviderFallbackAllowed}
			return decision
		}
	}
	decision.Waiting = true
	decision.ReasonCodes = []domain.RoutingReason{domain.RoutingReasonReviewIndependenceRequired, domain.RoutingReasonWaitingForCapacity}
	return decision
}

// selectFromPriority walks priority in order, returning the first entry
// that is eligible, passes allow, and is capacity-eligible.
func selectFromPriority(priority []domain.ProviderProfileID, req RoutingRequest, allow func(domain.ProviderProfile) bool) (domain.ProviderProfile, bool) {
	for _, id := range priority {
		p, ok := req.EligibleProfiles[id]
		if !ok || !allow(p) {
			continue
		}
		if capacityEligible(req.Capacity[id]) {
			return p, true
		}
	}
	return domain.ProviderProfile{}, false
}

// capacitySnapshotForProfiles builds a profile-keyed capacity snapshot
// (Checkpoint 8P-C) from the exact same agentHealth source 8H/8J already
// use, scoped to (userID, profileID) so one user's cooldown/outage can never
// appear in another user's routing decision (see health.go's precedence
// rule). A profile with no recorded scoped event yet reports
// domain.CapacityUnknown — never a fabricated "available".
func (c *Coordinator) capacitySnapshotForProfiles(ctx stdctx.Context, userID domain.UserID, profiles map[domain.ProviderProfileID]domain.ProviderProfile) map[domain.ProviderProfileID]domain.CapacityState {
	snapshot := make(map[domain.ProviderProfileID]domain.CapacityState, len(profiles))
	for id, p := range profiles {
		health, err := c.agentHealth(ctx, p.Harness, healthScope{userID: userID, profileID: id})
		if err != nil {
			snapshot[id] = domain.CapacityUnknown
			continue
		}
		snapshot[id] = domain.CapacityStateFromHealth(health.State)
	}
	return snapshot
}
