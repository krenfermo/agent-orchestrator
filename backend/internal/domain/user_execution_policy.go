package domain

import "time"

// UserExecutionPolicyID identifies one user's execution policy row. There is
// at most one live policy per user (upsert semantics), but the ID lets a
// workflow snapshot reference the exact version consulted at creation time.
type UserExecutionPolicyID string

// UserExecutionPolicyVersion is the fixed Checkpoint 8P-C policy shape
// version, persisted alongside every snapshot the same way
// RoutingPolicyVersion already is -- a decision remains explainable against
// the rules that actually produced it.
const UserExecutionPolicyVersion = "v1"

// FallbackBehavior controls how RouteExecution walks a role's priority list
// past an ineligible/unavailable entry (checkpoint brief §2).
type FallbackBehavior string

const (
	// FallbackUseNextAvailable walks past a preferred-but-unavailable profile
	// to the next eligible one in priority order.
	FallbackUseNextAvailable FallbackBehavior = "use_next_available"
	// FallbackWaitForPreferred never substitutes a lower-priority profile:
	// an unavailable most-preferred entry means wait, not fallback.
	FallbackWaitForPreferred FallbackBehavior = "wait_for_preferred"
)

// Valid reports whether a fallback behavior is part of the closed V1 enum.
func (b FallbackBehavior) Valid() bool {
	switch b {
	case FallbackUseNextAvailable, FallbackWaitForPreferred:
		return true
	default:
		return false
	}
}

// ReviewIndependence controls whether the reviewer/decision-resolver role
// may ever select the same provider that implemented/asked (checkpoint
// brief §3).
type ReviewIndependence string

const (
	// ReviewIndependenceRequireDifferentProvider never selects a
	// reviewer/resolver profile sharing a provider with the implementer;
	// absent any independent eligible profile, routing waits.
	ReviewIndependenceRequireDifferentProvider ReviewIndependence = "require_different_provider"
	// ReviewIndependenceAllowSameProviderFallback allows the implementer's
	// own provider as a last resort, still ordered by the role's priority
	// list -- never forced first.
	ReviewIndependenceAllowSameProviderFallback ReviewIndependence = "allow_same_provider_fallback"
)

// Valid reports whether a review independence mode is part of the closed V1
// enum.
func (i ReviewIndependence) Valid() bool {
	switch i {
	case ReviewIndependenceRequireDifferentProvider, ReviewIndependenceAllowSameProviderFallback:
		return true
	default:
		return false
	}
}

// UserExecutionPolicy is one user's configurable routing preference
// (Checkpoint 8P-C): which of their own ProviderProfiles to prefer, in what
// order, per role, plus fallback/independence behavior. It replaces the
// fixed Claude<->Codex RoutingPolicy -- priority lists reference
// ProviderProfileID (an owned, capability-bearing connection), never a bare
// harness/provider literal, so a future provider only needs a profile to
// become selectable, never a router source change.
type UserExecutionPolicy struct {
	ID      UserExecutionPolicyID
	UserID  UserID
	Version string
	// AutonomousMode is stored only as of 8P-C; it does not yet change
	// Create Workflow / Generate Plan / auto-approval behavior (8P-D).
	AutonomousMode           bool
	PlannerPriority          []ProviderProfileID
	WorkerPriority           []ProviderProfileID
	ReviewerPriority         []ProviderProfileID
	DecisionResolverPriority []ProviderProfileID
	FallbackBehavior         FallbackBehavior
	ReviewIndependence       ReviewIndependence
	CreatedAt                time.Time
	UpdatedAt                time.Time
}

// PriorityFor returns the priority list for role, or nil for a role this
// policy has no opinion on (e.g. WorkflowRoleVerify never routes through a
// provider).
func (p UserExecutionPolicy) PriorityFor(role WorkflowRole) []ProviderProfileID {
	switch role {
	case WorkflowRolePlanner:
		return p.PlannerPriority
	case WorkflowRoleWorker, WorkflowRoleFixWorker:
		return p.WorkerPriority
	case WorkflowRoleReviewer:
		return p.ReviewerPriority
	case WorkflowRoleDecisionResolver:
		return p.DecisionResolverPriority
	default:
		return nil
	}
}

// DefaultUserExecutionPolicy is the documented compatibility/bootstrap
// builder used when a user (including the trusted-local bootstrap admin)
// has no stored policy yet (checkpoint brief §7/§8). It never invents a
// profile: only profiles the user actually owns are ever placed in a
// priority list, filtered to those whose provider is supported
// (descriptor.Available) and, where practical, that advertise the relevant
// capability. Worker/Planner prefer Claude Code before Codex (today's
// "Claude Max 5x" usage pattern); Reviewer/DecisionResolver prefer Codex
// before Claude Code, so a Claude worker's default reviewer is independent
// without any policy configuration.
func DefaultUserExecutionPolicy(userID UserID, profiles []ProviderProfile) UserExecutionPolicy {
	byHarness := make(map[AgentHarness]ProviderProfileID, len(profiles))
	for _, p := range profiles {
		if _, exists := byHarness[p.Harness]; !exists {
			byHarness[p.Harness] = p.ID
		}
	}
	ordered := func(harnesses ...AgentHarness) []ProviderProfileID {
		var out []ProviderProfileID
		for _, h := range harnesses {
			if id, ok := byHarness[h]; ok {
				out = append(out, id)
			}
		}
		return out
	}
	workerFirst := ordered(HarnessClaudeCode, HarnessCodex)
	reviewFirst := ordered(HarnessCodex, HarnessClaudeCode)
	return UserExecutionPolicy{
		UserID:                   userID,
		Version:                  UserExecutionPolicyVersion,
		PlannerPriority:          workerFirst,
		WorkerPriority:           workerFirst,
		ReviewerPriority:         reviewFirst,
		DecisionResolverPriority: reviewFirst,
		FallbackBehavior:         FallbackUseNextAvailable,
		ReviewIndependence:       ReviewIndependenceRequireDifferentProvider,
	}
}
