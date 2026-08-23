package workflow

import (
	stdctx "context"
	"sort"

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
	priority, listed := completePriority(req.Policy.PriorityFor(req.Role), req.EligibleProfiles)
	switch req.Role {
	case domain.WorkflowRolePlanner, domain.WorkflowRoleWorker, domain.WorkflowRoleFixWorker:
		decision = routeByPriority(decision, priority, listed, req)
	case domain.WorkflowRoleReviewer, domain.WorkflowRoleDecisionResolver:
		decision = routeCrossProvider(decision, priority, listed, req)
	default:
		decision.Waiting = true
		decision.ReasonCodes = []domain.RoutingReason{domain.RoutingReasonWaitingForCapacity}
	}
	// A wait taken while some eligible profile's capacity is still unknown is
	// a materially different situation from a wait taken against fully-known
	// capacity, and the operator needs to see which one they are in
	// (requirement §4: expose the real reason). The caller's active probe has
	// already run by the time RouteExecution sees this snapshot, so an unknown
	// here means the probe genuinely could not conclude.
	if decision.Waiting && hasUnknownEligibleCapacity(req) {
		decision.ReasonCodes = append(decision.ReasonCodes, domain.RoutingReasonCapacityProbeIndeterminate)
	}
	return decision
}

func hasUnknownEligibleCapacity(req RoutingRequest) bool {
	for id := range req.EligibleProfiles {
		if req.Capacity[id] == domain.CapacityUnknown {
			return true
		}
	}
	return false
}

// routeByPriority applies the planner/worker rule: walk the role's priority
// list in order, selecting the first entry that is both eligible (owned,
// enabled, connected, capable) and capacity-eligible.
// domain.FallbackWaitForPreferred stops at the first eligible-but-
// capacity-blocked entry rather than silently walking to the next one
// (checkpoint brief §2: "Codex sólo cuando Claude no sea elegible" is an
// opt-in behavior, not the only one).
// completePriority returns stored followed by every eligible profile stored
// never mentioned, and the number of leading entries that came from stored
// itself (Checkpoint 8P-E.13A.4).
//
// A UserExecutionPolicy priority list is written once — at connect time, or
// whenever Settings is saved — against whatever profiles existed then. Every
// profile connected afterwards is absent from it. Before this, routing walked
// the stored list and nothing else, so such a profile could never be selected
// no matter how eligible, enabled, authenticated and capacity-available it
// was: for reviewer/decision-resolver roles, where independence forbids
// falling back to the implementer's own provider on high-risk work, that is a
// permanent deadlock (see ~/.ao/data wf-35fd1af0, whose reviewer_priority
// listed only the Claude Code profile while an authenticated, reviewer-capable
// Codex profile sat unreferenced).
//
// The user's explicit order is never reordered and never demoted — unlisted
// profiles are strictly a last-resort tail, sorted by ID so the same inputs
// always produce the same decision (RouteExecution must stay deterministic).
//
// A completely EMPTY stored list is left empty and still waits, preserving
// checkpoint brief §18's "no profile = never selected": that case means the
// user has expressed no preference for this role at all, which is materially
// different from having expressed one that simply predates a later-connected
// profile. Completion only ever extends an order the user actually stated.
func completePriority(stored []domain.ProviderProfileID, eligible map[domain.ProviderProfileID]domain.ProviderProfile) ([]domain.ProviderProfileID, int) {
	if len(stored) == 0 {
		return nil, 0
	}
	seen := make(map[domain.ProviderProfileID]struct{}, len(stored))
	out := make([]domain.ProviderProfileID, 0, len(stored)+len(eligible))
	for _, id := range stored {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	listed := len(out)
	tail := make([]domain.ProviderProfileID, 0, len(eligible))
	for id := range eligible {
		if _, ok := seen[id]; ok {
			continue
		}
		tail = append(tail, id)
	}
	sort.Slice(tail, func(i, j int) bool { return tail[i] < tail[j] })
	return append(out, tail...), listed
}

func routeByPriority(decision domain.RoutingDecision, priority []domain.ProviderProfileID, listed int, req RoutingRequest) domain.RoutingDecision {
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
			// FallbackWaitForPreferred parks on a user-LISTED entry only: an
			// unlisted tail entry was never a stated preference, so a blocked
			// one must not stop the walk (Checkpoint 8P-E.13A.4).
			if req.Policy.FallbackBehavior == domain.FallbackWaitForPreferred && i < listed {
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
		if i >= listed {
			decision.ReasonCodes = append(decision.ReasonCodes, domain.RoutingReasonPolicyPriorityCompleted)
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
func routeCrossProvider(decision domain.RoutingDecision, priority []domain.ProviderProfileID, listed int, req RoutingRequest) domain.RoutingDecision {
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
	if sel, idx, ok := selectFromPriority(priority, req, independent); ok {
		decision.SelectedProfileID = sel.ID
		decision.SelectedHarness = sel.Harness
		decision.ReasonCodes = []domain.RoutingReason{domain.RoutingReasonCrossProviderReview, domain.RoutingReasonReviewIndependenceRequired}
		if idx >= listed {
			decision.ReasonCodes = append(decision.ReasonCodes, domain.RoutingReasonPolicyPriorityCompleted)
		}
		return decision
	}
	allowSameProvider := req.Policy.ReviewIndependence == domain.ReviewIndependenceAllowSameProviderFallback && req.Complexity != ComplexityHighRisk
	if allowSameProvider {
		any := func(domain.ProviderProfile) bool { return true }
		if sel, idx, ok := selectFromPriority(priority, req, any); ok {
			decision.SelectedProfileID = sel.ID
			decision.SelectedHarness = sel.Harness
			decision.ReasonCodes = []domain.RoutingReason{domain.RoutingReasonReviewIndependenceRequired, domain.RoutingReasonPreferredUnavailable, domain.RoutingReasonSameProviderFallbackAllowed}
			if idx >= listed {
				decision.ReasonCodes = append(decision.ReasonCodes, domain.RoutingReasonPolicyPriorityCompleted)
			}
			return decision
		}
	}
	decision.Waiting = true
	decision.ReasonCodes = []domain.RoutingReason{domain.RoutingReasonReviewIndependenceRequired, domain.RoutingReasonWaitingForCapacity}
	return decision
}

// selectFromPriority walks priority in order, returning the first entry
// that is eligible, passes allow, and is capacity-eligible, along with its
// index in priority (so the caller can tell a user-listed selection from one
// taken out of completePriority's unlisted tail).
func selectFromPriority(priority []domain.ProviderProfileID, req RoutingRequest, allow func(domain.ProviderProfile) bool) (domain.ProviderProfile, int, bool) {
	for i, id := range priority {
		p, ok := req.EligibleProfiles[id]
		if !ok || !allow(p) {
			continue
		}
		if capacityEligible(req.Capacity[id]) {
			return p, i, true
		}
	}
	return domain.ProviderProfile{}, -1, false
}

// capacitySnapshotForProfiles builds a profile-keyed capacity snapshot
// (Checkpoint 8P-C) from the exact same agentHealth source 8H/8J already
// use, scoped to (userID, profileID) so one user's cooldown/outage can never
// appear in another user's routing decision (see health.go's precedence
// rule).
//
// Checkpoint 8P-E.13A.4: a profile with no recorded scoped event yet no
// longer just reports domain.CapacityUnknown and stops there. Recorded health
// is written only as a side effect of a real dispatch, so "never dispatched
// to" and "cannot accept work" were indistinguishable, and the only way out
// was for a human to make AO run that provider once. Unknown now triggers one
// bounded, throttled, local probe (probeCapacityForProfile) whose conclusion
// is persisted as a durable health fact. An inconclusive probe still leaves
// the state unknown — never a fabricated "available", and never a downgrade to
// "unavailable" on no evidence.
func (c *Coordinator) capacitySnapshotForProfiles(ctx stdctx.Context, userID domain.UserID, profiles map[domain.ProviderProfileID]domain.ProviderProfile) map[domain.ProviderProfileID]domain.CapacityState {
	now := c.clock()
	snapshot := make(map[domain.ProviderProfileID]domain.CapacityState, len(profiles))
	for id, p := range profiles {
		scope := healthScope{userID: userID, profileID: id}
		observed := domain.AgentHealth{Harness: p.Harness, State: domain.AgentHealthUnknown}
		if h, err := c.agentHealth(ctx, p.Harness, scope); err == nil {
			observed = h
		}
		// EffectiveState, not State: a cooldown whose window has run out is a
		// past failure, not current evidence, and reporting it as still-blocking
		// is the whole of the self-locking bug.
		state := domain.CapacityStateFromHealth(observed.EffectiveState(now))
		if observed.ProbeEligible(now) {
			// A probe of an already-observed provider is a RECOVERY probe: it
			// re-evaluates a stale verdict rather than discovering an unobserved
			// one, and must not be gated on the telemetry capability (see
			// probeCapacityForProfile).
			if probed, ok := c.probeCapacityForProfile(ctx, scope, p, observed.State != domain.AgentHealthUnknown); ok {
				state = probed
			}
		}
		snapshot[id] = state
	}
	return snapshot
}
