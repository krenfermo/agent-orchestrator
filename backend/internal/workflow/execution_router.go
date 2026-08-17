package workflow

import (
	stdctx "context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// knownRoutingHarnesses is V1's fixed two-provider universe (checkpoint
// brief §3/§11: "no un tercer proveedor"). ExecutionRouter never considers a
// harness outside this set.
var knownRoutingHarnesses = []domain.AgentHarness{domain.HarnessCodex, domain.HarnessClaudeCode}

// oppositeHarness is the single shared cross-provider mapping table
// (checkpoint brief §4/§11: "No dupliques provider-selection logic"):
// Codex<->Claude Code, no third provider. Worker fallback (failover.go),
// reviewer routing and the 8K-B decision resolver all consult this one
// function rather than each keeping its own hardcoded table.
func oppositeHarness(h domain.AgentHarness) (domain.AgentHarness, bool) {
	switch h {
	case domain.HarnessClaudeCode:
		return domain.HarnessCodex, true
	case domain.HarnessCodex:
		return domain.HarnessClaudeCode, true
	default:
		return "", false
	}
}

// reviewerHarnessFromAgentHarness bridges ExecutionRouter's AgentHarness
// vocabulary (worker/routing) to domain.ReviewerHarness (the reviewer
// registry's own vocabulary, ports.ReviewerResolver) for the two harnesses
// ExecutionRouter V1 ever selects as reviewer. Falls back to Claude Code —
// the only reviewer 8C originally supported — for anything else, never a
// zero value.
func reviewerHarnessFromAgentHarness(h domain.AgentHarness) domain.ReviewerHarness {
	if h == domain.HarnessCodex {
		return domain.ReviewerCodex
	}
	return domain.ReviewerClaudeCode
}

// RoutingRequest is ExecutionRouter's pure input (checkpoint brief §4):
// every fact a routing decision may consult, gathered by the caller from
// already-durable state. RouteExecution itself performs no IO, so the same
// request always produces the same decision.
type RoutingRequest struct {
	Role domain.WorkflowRole
	// Complexity is consulted for the worker role (which preference tier
	// applies) and the reviewer role (whether same-provider fallback may
	// ever be considered — never for high-risk). Ignored for planner.
	Complexity TaskComplexity
	// CurrentImplementer is the harness whose work is being reviewed
	// (reviewer/decision-resolver role); empty for planner/worker.
	CurrentImplementer       domain.AgentHarness
	PreviousAttempts         int
	PreviousProviderFailures []domain.AgentHarness
	// Capacity is a snapshot of every known harness's CapacityState, taken
	// once by the caller (via the same agentHealth/capacity.Reader source
	// 8H/8J already use) so RouteExecution stays pure/deterministic.
	Capacity map[domain.AgentHarness]domain.CapacityState
	Policy   domain.RoutingPolicy
}

// capacityEligible reports whether state permits dispatch (checkpoint brief
// §12): available/unknown are eligible, limited/cooldown/unavailable are
// not. Unknown defaults to eligible ("elegible conservadoramente si
// instalado/autenticado salvo fallo activo") — RouteExecution has no probe
// of its own, so an unrecorded harness is treated the same way
// domain.AgentHealth.Available already treats AgentHealthUnknown.
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

// RouteExecution is Checkpoint 8L's pure, deterministic V1 routing
// decision. It never calls an LLM, never performs IO and never mutates
// state — callers gather RoutingRequest's facts first (complexity estimate,
// capacity snapshot, policy) and persist the returned RoutingDecision
// themselves.
func RouteExecution(req RoutingRequest) domain.RoutingDecision {
	policy := req.Policy
	if policy.Version == "" {
		policy = domain.DefaultRoutingPolicy()
	}
	decision := domain.RoutingDecision{
		Role:                    req.Role,
		Complexity:              string(req.Complexity),
		PolicyVersion:           domain.RoutingPolicyVersion,
		CapacityStateAtDecision: req.Capacity,
	}

	switch req.Role {
	case domain.WorkflowRolePlanner:
		return routePlanner(decision, policy.PlannerPreference, req.Capacity)
	case domain.WorkflowRoleWorker, domain.WorkflowRoleFixWorker:
		preferred := policy.NormalWorkerPreference
		switch req.Complexity {
		case ComplexityTrivial:
			preferred = policy.TrivialWorkerPreference
		case ComplexityHighRisk:
			preferred = policy.HighRiskWorkerPreference
		}
		return routeWorker(decision, preferred, req.Capacity)
	case domain.WorkflowRoleReviewer, domain.WorkflowRoleDecisionResolver:
		return routeCrossProvider(decision, req.CurrentImplementer, req.Complexity, req.Capacity, policy)
	default:
		decision.Waiting = true
		decision.ReasonCodes = []domain.RoutingReason{domain.RoutingReasonWaitingForCapacity}
		return decision
	}
}

// routePlanner has no fallback: only one planner adapter is wired today
// (checkpoint brief §10 — "Si no existe soporte seguro: waiting_for_capacity"),
// so an unavailable preferred planner harness always waits, never guesses a
// second implementation.
func routePlanner(decision domain.RoutingDecision, preferred domain.AgentHarness, capacity map[domain.AgentHarness]domain.CapacityState) domain.RoutingDecision {
	decision.PreferredHarness = preferred
	if capacityEligible(capacity[preferred]) {
		decision.SelectedHarness = preferred
		decision.ReasonCodes = []domain.RoutingReason{domain.RoutingReasonPlannerPolicy}
		return decision
	}
	decision.Waiting = true
	decision.ReasonCodes = []domain.RoutingReason{
		domain.RoutingReasonPlannerPolicy,
		domain.RoutingReasonPreferredUnavailable,
		capacityReason(capacity[preferred]),
		domain.RoutingReasonWaitingForCapacity,
	}
	return decision
}

// routeWorker applies checkpoint brief §7's worker fallback rule: preferred
// harness by complexity tier, falling back to the opposite provider (V1's
// only other known harness) when the preferred one is not capacity-eligible,
// and waiting_for_capacity — never needs_attention — when neither is.
func routeWorker(decision domain.RoutingDecision, preferred domain.AgentHarness, capacity map[domain.AgentHarness]domain.CapacityState) domain.RoutingDecision {
	decision.PreferredHarness = preferred
	fallback, hasFallback := oppositeHarness(preferred)
	if hasFallback {
		decision.FallbackOrder = []domain.AgentHarness{fallback}
	}
	if capacityEligible(capacity[preferred]) {
		decision.SelectedHarness = preferred
		decision.ReasonCodes = []domain.RoutingReason{domain.RoutingReasonPreferredForComplexity}
		return decision
	}
	reasons := []domain.RoutingReason{domain.RoutingReasonPreferredUnavailable, capacityReason(capacity[preferred])}
	if hasFallback && capacityEligible(capacity[fallback]) {
		decision.SelectedHarness = fallback
		decision.ReasonCodes = append(reasons, domain.RoutingReasonFallbackSelected)
		return decision
	}
	decision.Waiting = true
	decision.ReasonCodes = append(reasons, domain.RoutingReasonWaitingForCapacity)
	return decision
}

// routeCrossProvider applies checkpoint brief §8/§11's independence rule for
// both the reviewer role and the 8K-B decision-resolver role: prefer the
// opposite provider from whoever implemented/asked. A same-provider
// fallback is only ever considered when the policy explicitly allows it AND
// the task is not high-risk (checkpoint brief §8: "NO usar same-provider
// reviewer automáticamente para high-risk" is a hard rule policy cannot
// relax). Absent that opt-in, an unavailable opposite provider waits.
func routeCrossProvider(decision domain.RoutingDecision, implementer domain.AgentHarness, complexity TaskComplexity, capacity map[domain.AgentHarness]domain.CapacityState, policy domain.RoutingPolicy) domain.RoutingDecision {
	preferred, ok := oppositeHarness(implementer)
	if !ok {
		decision.Waiting = true
		decision.ReasonCodes = []domain.RoutingReason{domain.RoutingReasonWaitingForCapacity}
		return decision
	}
	decision.PreferredHarness = preferred
	if capacityEligible(capacity[preferred]) {
		decision.SelectedHarness = preferred
		decision.ReasonCodes = []domain.RoutingReason{domain.RoutingReasonCrossProviderReview, domain.RoutingReasonReviewIndependenceRequired}
		return decision
	}
	reasons := []domain.RoutingReason{
		domain.RoutingReasonReviewIndependenceRequired,
		domain.RoutingReasonPreferredUnavailable,
		capacityReason(capacity[preferred]),
	}
	allowSameProvider := policy.AllowSameProviderReviewFallbackForNormal && complexity != ComplexityHighRisk
	if allowSameProvider && capacityEligible(capacity[implementer]) {
		decision.FallbackOrder = []domain.AgentHarness{implementer}
		decision.SelectedHarness = implementer
		decision.ReasonCodes = append(reasons, domain.RoutingReasonSameProviderFallbackAllowed)
		return decision
	}
	decision.Waiting = true
	decision.ReasonCodes = append(reasons, domain.RoutingReasonWaitingForCapacity)
	return decision
}

// routingCapacitySnapshot builds RoutingRequest.Capacity from the exact same
// agentHealth source 8H/8K-B already query (never a second capacity query
// path) for every V1 known harness.
func (c *Coordinator) routingCapacitySnapshot(ctx stdctx.Context) map[domain.AgentHarness]domain.CapacityState {
	snapshot := make(map[domain.AgentHarness]domain.CapacityState, len(knownRoutingHarnesses))
	for _, h := range knownRoutingHarnesses {
		health, err := c.agentHealth(ctx, h)
		if err != nil {
			snapshot[h] = domain.CapacityUnknown
			continue
		}
		snapshot[h] = domain.CapacityStateFromHealth(health.State)
	}
	return snapshot
}
