package workflow

import (
	"reflect"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

const (
	profClaude domain.ProviderProfileID = "prof-claude"
	profCodex  domain.ProviderProfileID = "prof-codex"
)

func fixtureProfiles() map[domain.ProviderProfileID]domain.ProviderProfile {
	return map[domain.ProviderProfileID]domain.ProviderProfile{
		profClaude: {ID: profClaude, Provider: "anthropic", Harness: domain.HarnessClaudeCode, Enabled: true},
		profCodex:  {ID: profCodex, Provider: "openai", Harness: domain.HarnessCodex, Enabled: true},
	}
}

func availableCapacity() map[domain.ProviderProfileID]domain.CapacityState {
	return map[domain.ProviderProfileID]domain.CapacityState{
		profClaude: domain.CapacityAvailable,
		profCodex:  domain.CapacityAvailable,
	}
}

func workerPolicy(order ...domain.ProviderProfileID) domain.ExecutionPolicySnapshot {
	return domain.ExecutionPolicySnapshot{
		Version:            domain.UserExecutionPolicyVersion,
		PlannerPriority:    order,
		WorkerPriority:     order,
		ReviewerPriority:   []domain.ProviderProfileID{profCodex, profClaude},
		FallbackBehavior:   domain.FallbackUseNextAvailable,
		ReviewIndependence: domain.ReviewIndependenceRequireDifferentProvider,
	}
}

// Test #1: user's worker priority order wins regardless of task complexity —
// checkpoint brief §17's core proof that the old hardcoded
// trivial-prefers-Codex rule is gone.
func TestRouteExecution_UserPriorityWinsForAnyComplexity(t *testing.T) {
	for _, complexity := range []TaskComplexity{ComplexityTrivial, ComplexityNormal, ComplexityHighRisk} {
		d := RouteExecution(RoutingRequest{
			Role:             domain.WorkflowRoleWorker,
			Complexity:       complexity,
			Policy:           workerPolicy(profClaude, profCodex),
			EligibleProfiles: fixtureProfiles(),
			Capacity:         availableCapacity(),
		})
		if d.SelectedHarness != domain.HarnessClaudeCode {
			t.Fatalf("complexity=%s: selected = %q, want claude-code (user's first priority)", complexity, d.SelectedHarness)
		}
		if d.Waiting {
			t.Fatalf("complexity=%s: waiting = true, want false", complexity)
		}
		if d.SelectedProfileID != profClaude {
			t.Fatalf("complexity=%s: selected profile = %q, want %q", complexity, d.SelectedProfileID, profClaude)
		}
	}
}

// Test #2: the opposite order (Codex first) is honored too — proves order
// comes purely from policy, not a hardcoded harness preference.
func TestRouteExecution_UserPriorityCodexFirst(t *testing.T) {
	d := RouteExecution(RoutingRequest{
		Role:             domain.WorkflowRoleWorker,
		Complexity:       ComplexityNormal,
		Policy:           workerPolicy(profCodex, profClaude),
		EligibleProfiles: fixtureProfiles(),
		Capacity:         availableCapacity(),
	})
	if d.SelectedHarness != domain.HarnessCodex {
		t.Fatalf("selected = %q, want codex", d.SelectedHarness)
	}
	if len(d.ReasonCodes) == 0 || d.ReasonCodes[0] != domain.RoutingReasonUserPreferredProvider {
		t.Fatalf("reasons = %v, want user_preferred_provider first", d.ReasonCodes)
	}
}

// Test #4: preferred profile unavailable falls back to the next eligible
// entry under FallbackUseNextAvailable.
func TestRouteExecution_PreferredUnavailableFallsBack(t *testing.T) {
	capacity := availableCapacity()
	capacity[profClaude] = domain.CapacityCooldown
	d := RouteExecution(RoutingRequest{
		Role:             domain.WorkflowRoleWorker,
		Complexity:       ComplexityNormal,
		Policy:           workerPolicy(profClaude, profCodex),
		EligibleProfiles: fixtureProfiles(),
		Capacity:         capacity,
	})
	if d.SelectedHarness != domain.HarnessCodex {
		t.Fatalf("selected = %q, want codex fallback", d.SelectedHarness)
	}
	if d.Waiting {
		t.Fatalf("waiting = true, want false (fallback eligible)")
	}
	found := false
	for _, r := range d.ReasonCodes {
		if r == domain.RoutingReasonFallbackSelected {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasons = %v, want fallback_selected present", d.ReasonCodes)
	}
}

// Test #4b: FallbackWaitForPreferred never substitutes the next entry.
func TestRouteExecution_WaitForPreferredNeverSubstitutes(t *testing.T) {
	capacity := availableCapacity()
	capacity[profClaude] = domain.CapacityCooldown
	policy := workerPolicy(profClaude, profCodex)
	policy.FallbackBehavior = domain.FallbackWaitForPreferred
	d := RouteExecution(RoutingRequest{
		Role:             domain.WorkflowRoleWorker,
		Complexity:       ComplexityNormal,
		Policy:           policy,
		EligibleProfiles: fixtureProfiles(),
		Capacity:         capacity,
	})
	if !d.Waiting {
		t.Fatalf("waiting = false, want true: wait_for_preferred must never fall back")
	}
	if d.SelectedHarness != "" {
		t.Fatalf("selected = %q, want empty while waiting", d.SelectedHarness)
	}
}

// Test #5: every priority entry unavailable waits for capacity, never fails.
func TestRouteExecution_BothUnavailableWaits(t *testing.T) {
	capacity := map[domain.ProviderProfileID]domain.CapacityState{
		profClaude: domain.CapacityCooldown,
		profCodex:  domain.CapacityUnavailable,
	}
	d := RouteExecution(RoutingRequest{
		Role:             domain.WorkflowRoleWorker,
		Complexity:       ComplexityNormal,
		Policy:           workerPolicy(profClaude, profCodex),
		EligibleProfiles: fixtureProfiles(),
		Capacity:         capacity,
	})
	if !d.Waiting {
		t.Fatalf("waiting = false, want true")
	}
	if d.SelectedHarness != "" {
		t.Fatalf("selected = %q, want empty while waiting", d.SelectedHarness)
	}
	found := false
	for _, r := range d.ReasonCodes {
		if r == domain.RoutingReasonWaitingForCapacity {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasons = %v, want waiting_for_capacity present", d.ReasonCodes)
	}
}

// Test #6: Claude worker -> Codex reviewer (cross-provider independence).
func TestRouteExecution_ReviewerClaudeWorkerToCodexReviewer(t *testing.T) {
	d := RouteExecution(RoutingRequest{
		Role:                       domain.WorkflowRoleReviewer,
		Complexity:                 ComplexityNormal,
		CurrentImplementerProvider: "anthropic",
		Policy:                     workerPolicy(profClaude, profCodex),
		EligibleProfiles:           fixtureProfiles(),
		Capacity:                   availableCapacity(),
	})
	if d.SelectedHarness != domain.HarnessCodex {
		t.Fatalf("selected = %q, want codex", d.SelectedHarness)
	}
}

// Test #7: Codex worker -> Claude Code reviewer (cross-provider independence).
func TestRouteExecution_ReviewerCodexWorkerToClaudeReviewer(t *testing.T) {
	policy := workerPolicy(profClaude, profCodex)
	policy.ReviewerPriority = []domain.ProviderProfileID{profClaude, profCodex}
	d := RouteExecution(RoutingRequest{
		Role:                       domain.WorkflowRoleReviewer,
		Complexity:                 ComplexityNormal,
		CurrentImplementerProvider: "openai",
		Policy:                     policy,
		EligibleProfiles:           fixtureProfiles(),
		Capacity:                   availableCapacity(),
	})
	if d.SelectedHarness != domain.HarnessClaudeCode {
		t.Fatalf("selected = %q, want claude-code", d.SelectedHarness)
	}
}

// Test #8: high-risk reviewer independence can never fall back to the same
// provider, even when the policy opt-in is enabled and the only independent
// candidate is unavailable — it must wait instead.
func TestRouteExecution_HighRiskReviewerNeverSameProviderFallback(t *testing.T) {
	capacity := availableCapacity()
	capacity[profCodex] = domain.CapacityCooldown // the only independent-of-Claude candidate
	policy := workerPolicy(profClaude, profCodex)
	policy.ReviewIndependence = domain.ReviewIndependenceAllowSameProviderFallback // must not apply to high-risk
	d := RouteExecution(RoutingRequest{
		Role:                       domain.WorkflowRoleReviewer,
		Complexity:                 ComplexityHighRisk,
		CurrentImplementerProvider: "anthropic",
		Policy:                     policy,
		EligibleProfiles:           fixtureProfiles(),
		Capacity:                   capacity,
	})
	if !d.Waiting {
		t.Fatalf("waiting = false, want true (high-risk must never same-provider fallback)")
	}
	if d.SelectedHarness != "" {
		t.Fatalf("selected = %q, want empty", d.SelectedHarness)
	}
}

// Test: normal-complexity reviewer MAY fall back to the same provider, but
// only when the policy explicitly opts in.
func TestRouteExecution_NormalReviewerSameProviderFallbackOnlyWhenAllowed(t *testing.T) {
	capacity := availableCapacity()
	capacity[profCodex] = domain.CapacityCooldown

	// Default policy: no opt-in -> waits.
	d := RouteExecution(RoutingRequest{
		Role:                       domain.WorkflowRoleReviewer,
		Complexity:                 ComplexityNormal,
		CurrentImplementerProvider: "anthropic",
		Policy:                     workerPolicy(profClaude, profCodex),
		EligibleProfiles:           fixtureProfiles(),
		Capacity:                   capacity,
	})
	if !d.Waiting {
		t.Fatalf("waiting = false, want true without opt-in")
	}

	// Opt-in enabled, and the implementer's own profile (Claude) is present
	// in ReviewerPriority: falls back to it.
	policy := workerPolicy(profClaude, profCodex)
	policy.ReviewerPriority = []domain.ProviderProfileID{profCodex, profClaude}
	policy.ReviewIndependence = domain.ReviewIndependenceAllowSameProviderFallback
	d2 := RouteExecution(RoutingRequest{
		Role:                       domain.WorkflowRoleReviewer,
		Complexity:                 ComplexityNormal,
		CurrentImplementerProvider: "anthropic",
		Policy:                     policy,
		EligibleProfiles:           fixtureProfiles(),
		Capacity:                   capacity,
	})
	if d2.Waiting || d2.SelectedHarness != domain.HarnessClaudeCode {
		t.Fatalf("decision = %+v, want same-provider fallback to claude-code", d2)
	}
}

// Test #10: planner has no fallback wired for Codex in the real registry
// (only Claude Code advertises CapabilityPlanner — see the registry audit),
// so an ineligible/unavailable planner profile always waits.
func TestRouteExecution_PlannerPolicy(t *testing.T) {
	d := RouteExecution(RoutingRequest{
		Role:             domain.WorkflowRolePlanner,
		Policy:           workerPolicy(profClaude),
		EligibleProfiles: fixtureProfiles(),
		Capacity:         availableCapacity(),
	})
	if d.SelectedHarness != domain.HarnessClaudeCode {
		t.Fatalf("selected = %q, want claude-code", d.SelectedHarness)
	}
	if len(d.FallbackProfileOrder) != 0 {
		t.Fatalf("fallback order = %v, want none for a single-entry priority list", d.FallbackProfileOrder)
	}

	capacity := availableCapacity()
	capacity[profClaude] = domain.CapacityUnavailable
	d2 := RouteExecution(RoutingRequest{
		Role:             domain.WorkflowRolePlanner,
		Policy:           workerPolicy(profClaude),
		EligibleProfiles: fixtureProfiles(),
		Capacity:         capacity,
	})
	if !d2.Waiting {
		t.Fatalf("waiting = false, want true: no second eligible planner profile")
	}
}

// Test #13: reason codes are always part of the closed enum.
func TestRouteExecution_ReasonCodesAreClosedEnum(t *testing.T) {
	cases := []RoutingRequest{
		{Role: domain.WorkflowRoleWorker, Complexity: ComplexityTrivial, Policy: workerPolicy(profClaude, profCodex), EligibleProfiles: fixtureProfiles(), Capacity: availableCapacity()},
		{Role: domain.WorkflowRoleWorker, Complexity: ComplexityNormal, Policy: workerPolicy(profClaude, profCodex), EligibleProfiles: fixtureProfiles(), Capacity: map[domain.ProviderProfileID]domain.CapacityState{profClaude: domain.CapacityCooldown, profCodex: domain.CapacityCooldown}},
		{Role: domain.WorkflowRoleReviewer, Complexity: ComplexityHighRisk, CurrentImplementerProvider: "openai", Policy: workerPolicy(profClaude, profCodex), EligibleProfiles: fixtureProfiles(), Capacity: availableCapacity()},
		{Role: domain.WorkflowRolePlanner, Policy: workerPolicy(profClaude), EligibleProfiles: fixtureProfiles(), Capacity: availableCapacity()},
	}
	for _, req := range cases {
		d := RouteExecution(req)
		if d.PolicyVersion != domain.UserExecutionPolicyVersion {
			t.Fatalf("policy version = %q, want %q", d.PolicyVersion, domain.UserExecutionPolicyVersion)
		}
		if len(d.ReasonCodes) == 0 {
			t.Fatalf("decision %+v has no reason codes", d)
		}
		for _, r := range d.ReasonCodes {
			if !r.Valid() {
				t.Fatalf("reason %q is not part of the closed enum", r)
			}
		}
	}
}

// Test #14: the capacity snapshot consulted is recorded verbatim on the
// decision for later explainability.
func TestRouteExecution_CapacitySnapshotRecorded(t *testing.T) {
	capacity := availableCapacity()
	d := RouteExecution(RoutingRequest{
		Role:             domain.WorkflowRoleWorker,
		Complexity:       ComplexityTrivial,
		Policy:           workerPolicy(profClaude, profCodex),
		EligibleProfiles: fixtureProfiles(),
		Capacity:         capacity,
	})
	if !reflect.DeepEqual(d.CapacityStateByProfile, capacity) {
		t.Fatalf("capacity snapshot = %v, want %v", d.CapacityStateByProfile, capacity)
	}
}

// Unknown capacity defaults to eligible, matching domain.AgentHealth's own
// unknown-is-available semantics.
func TestRouteExecution_UnknownCapacityIsEligible(t *testing.T) {
	d := RouteExecution(RoutingRequest{
		Role:             domain.WorkflowRoleWorker,
		Complexity:       ComplexityTrivial,
		Policy:           workerPolicy(profClaude, profCodex),
		EligibleProfiles: fixtureProfiles(),
		Capacity:         map[domain.ProviderProfileID]domain.CapacityState{profClaude: domain.CapacityUnknown, profCodex: domain.CapacityUnknown},
	})
	if d.Waiting || d.SelectedHarness != domain.HarnessClaudeCode {
		t.Fatalf("decision = %+v, want unknown capacity treated as eligible", d)
	}
}

// Test: an empty priority list (user has no owned profiles at all, or none
// configured for this role) always waits — never guesses a hardcoded
// provider (checkpoint brief §18: no profile = never selected).
func TestRouteExecution_EmptyPriorityWaits(t *testing.T) {
	d := RouteExecution(RoutingRequest{
		Role:             domain.WorkflowRoleWorker,
		Policy:           domain.ExecutionPolicySnapshot{Version: domain.UserExecutionPolicyVersion, FallbackBehavior: domain.FallbackUseNextAvailable, ReviewIndependence: domain.ReviewIndependenceRequireDifferentProvider},
		EligibleProfiles: fixtureProfiles(),
		Capacity:         availableCapacity(),
	})
	if !d.Waiting || d.SelectedHarness != "" {
		t.Fatalf("decision = %+v, want waiting with no selection for an empty priority list", d)
	}
}

// Test: a priority entry not present in EligibleProfiles (disabled/
// unconnected/missing capability/unsupported provider) is never selected,
// even when it is the user's most-preferred entry.
func TestRouteExecution_IneligibleProfileNeverSelected(t *testing.T) {
	profiles := fixtureProfiles()
	delete(profiles, profClaude) // simulate disabled/unconnected/filtered out
	d := RouteExecution(RoutingRequest{
		Role:              domain.WorkflowRoleWorker,
		Policy:            workerPolicy(profClaude, profCodex),
		EligibleProfiles:  profiles,
		IneligibleReasons: map[domain.ProviderProfileID]domain.RoutingReason{profClaude: domain.RoutingReasonProviderDisabled},
		Capacity:          availableCapacity(),
	})
	if d.SelectedHarness != domain.HarnessCodex {
		t.Fatalf("selected = %q, want codex fallback past the ineligible preferred profile", d.SelectedHarness)
	}
}
