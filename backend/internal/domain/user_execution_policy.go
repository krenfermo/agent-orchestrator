package domain

import (
	"sort"
	"time"
)

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

// SyncExecutionPolicyPriorities is Checkpoint 8P-E.13A.5's stale-policy
// repair: it appends every owned, enabled, capable profile that a role's
// priority list does not already mention, and returns the updated policy plus
// whether anything actually changed.
//
// A stored policy row is written once -- when the user first saves Settings,
// or from DefaultUserExecutionPolicy at bootstrap -- against whatever profiles
// existed at that moment, and nothing ever revisited it. A provider connected
// afterwards therefore stayed absent from every priority list forever (in
// ~/.ao/data, reviewer_priority named only the Claude Code profile while an
// enabled, authenticated, reviewer-capable Codex profile sat unreferenced).
// Checkpoint 8P-E.13A.4 made routing survive that at dispatch time; this makes
// the stored configuration itself correct, so Settings shows the truth and
// routing no longer has to compensate.
//
// Guarantees, in order of importance:
//
//   - Existing order is preserved EXACTLY. New entries are only ever
//     appended, never inserted, reordered or removed. [Claude] + a new Codex
//     becomes [Claude, Codex], never [Codex, Claude].
//   - No profile ID is ever duplicated, including a list that already
//     contained duplicates before (they are left as-is; this function only
//     declines to add a third).
//   - Nothing outside the four priority lists is touched. AutonomousMode,
//     FallbackBehavior, ReviewIndependence, ID, Version, UserID and CreatedAt
//     are copied through verbatim; UpdatedAt is the caller's business, and is
//     only worth bumping when changed is true.
//   - Disabled and unauthenticated profiles are never REMOVED (that is a
//     separate policy decision, deliberately out of scope here).
//
// An EMPTY list is deliberately left empty. The schema (migration 0112,
// `DEFAULT '[]'`) has no way to tell "the user cleared this list on purpose"
// from "this list was auto-generated empty before any capable profile
// existed", and inventing entries for the first case would silently overwrite
// an explicit user choice -- the exact failure mode this checkpoint is meant
// to avoid. Extending only lists the user has actually populated also matches
// RouteExecution's own completePriority rule (checkpoint brief §18: an empty
// list means "no preference stated", and waits). See the checkpoint report for
// the one residual case this leaves open.
//
// Eligibility mirrors executionpolicy.validatePriority exactly -- descriptor
// available, and the capability advertised by either the descriptor or the
// profile row -- so a synced list always survives a later Settings PUT
// instead of failing validation. Auth state is deliberately NOT consulted: it
// is a cached probe result that flaps, and routing already filters on it live
// at dispatch time (domain.EligibleProfiles).
func SyncExecutionPolicyPriorities(policy UserExecutionPolicy, profiles []ProviderProfile, descriptors []ProviderAdapterDescriptor) (UserExecutionPolicy, bool) {
	candidates, capsByProfile := syncCandidates(profiles, descriptors)
	changed := false
	for _, list := range []struct {
		current    *[]ProviderProfileID
		capability ProviderCapability
	}{
		{&policy.PlannerPriority, CapabilityPlanner},
		{&policy.WorkerPriority, CapabilityWorker},
		{&policy.ReviewerPriority, CapabilityReviewer},
		{&policy.DecisionResolverPriority, CapabilityDecisionResolver},
	} {
		updated, listChanged := appendMissingForCapability(*list.current, candidates, capsByProfile, list.capability)
		if listChanged {
			*list.current = updated
			changed = true
		}
	}
	return policy, changed
}

// syncCandidates returns the profiles eligible to be appended, in a stable
// order: oldest connection first, ID as tiebreak. ListProviderProfilesByUser
// has no ORDER BY, so without this the appended tail would depend on SQLite's
// unspecified row order and two syncs could disagree. Ordering by CreatedAt
// also gives the natural meaning -- a newly connected provider lands last,
// behind everything the user already had.
// It also returns each candidate's effective capability set: the union of the
// profile row's own capabilities and its adapter descriptor's, matching how
// executionpolicy's PUT validator decides the same question (a profile row
// persisted before a descriptor gained a capability must not be treated as
// incapable, and vice versa).
func syncCandidates(profiles []ProviderProfile, descriptors []ProviderAdapterDescriptor) ([]ProviderProfile, map[ProviderProfileID][]ProviderCapability) {
	byKey := make(map[string]ProviderAdapterDescriptor, len(descriptors))
	for _, d := range descriptors {
		byKey[d.Provider+"|"+string(d.Harness)] = d
	}
	out := make([]ProviderProfile, 0, len(profiles))
	caps := make(map[ProviderProfileID][]ProviderCapability, len(profiles))
	for _, p := range profiles {
		if !p.Enabled {
			continue
		}
		desc, ok := byKey[p.Provider+"|"+string(p.Harness)]
		if !ok || !desc.Available {
			continue
		}
		out = append(out, p)
		caps[p.ID] = append(append([]ProviderCapability{}, p.Capabilities...), desc.Capabilities...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, caps
}

func appendMissingForCapability(current []ProviderProfileID, candidates []ProviderProfile, capsByProfile map[ProviderProfileID][]ProviderCapability, capability ProviderCapability) ([]ProviderProfileID, bool) {
	// An empty list stays empty -- see SyncExecutionPolicyPriorities.
	if len(current) == 0 {
		return current, false
	}
	present := make(map[ProviderProfileID]struct{}, len(current))
	for _, id := range current {
		present[id] = struct{}{}
	}
	updated := current
	changed := false
	for _, p := range candidates {
		if _, ok := present[p.ID]; ok {
			continue
		}
		if !hasCapability(capsByProfile[p.ID], capability) {
			continue
		}
		present[p.ID] = struct{}{}
		// Copy on first append so a caller's original slice is never aliased
		// into the returned policy and mutated underneath them.
		if !changed {
			updated = append(append([]ProviderProfileID{}, current...), p.ID)
		} else {
			updated = append(updated, p.ID)
		}
		changed = true
	}
	return updated, changed
}
