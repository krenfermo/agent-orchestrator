package workflow

import (
	"reflect"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func availableCapacity() map[domain.AgentHarness]domain.CapacityState {
	return map[domain.AgentHarness]domain.CapacityState{
		domain.HarnessCodex:      domain.CapacityAvailable,
		domain.HarnessClaudeCode: domain.CapacityAvailable,
	}
}

// Test #1: trivial complexity routes the worker to Codex.
func TestRouteExecution_TrivialWorkerPrefersCodex(t *testing.T) {
	d := RouteExecution(RoutingRequest{
		Role:       domain.WorkflowRoleWorker,
		Complexity: ComplexityTrivial,
		Capacity:   availableCapacity(),
		Policy:     domain.DefaultRoutingPolicy(),
	})
	if d.SelectedHarness != domain.HarnessCodex {
		t.Fatalf("selected = %q, want codex", d.SelectedHarness)
	}
	if d.Waiting {
		t.Fatalf("waiting = true, want false")
	}
	if len(d.ReasonCodes) == 0 || d.ReasonCodes[0] != domain.RoutingReasonPreferredForComplexity {
		t.Fatalf("reasons = %v, want preferred_for_complexity first", d.ReasonCodes)
	}
}

// Test #2: normal complexity routes the worker to Claude Code.
func TestRouteExecution_NormalWorkerPrefersClaude(t *testing.T) {
	d := RouteExecution(RoutingRequest{
		Role:       domain.WorkflowRoleWorker,
		Complexity: ComplexityNormal,
		Capacity:   availableCapacity(),
		Policy:     domain.DefaultRoutingPolicy(),
	})
	if d.SelectedHarness != domain.HarnessClaudeCode {
		t.Fatalf("selected = %q, want claude-code", d.SelectedHarness)
	}
}

// Test #3: high-risk complexity routes the worker to Claude Code.
func TestRouteExecution_HighRiskWorkerPrefersClaude(t *testing.T) {
	d := RouteExecution(RoutingRequest{
		Role:       domain.WorkflowRoleWorker,
		Complexity: ComplexityHighRisk,
		Capacity:   availableCapacity(),
		Policy:     domain.DefaultRoutingPolicy(),
	})
	if d.SelectedHarness != domain.HarnessClaudeCode {
		t.Fatalf("selected = %q, want claude-code", d.SelectedHarness)
	}
}

// Test #4: preferred harness unavailable falls back to the opposite one.
func TestRouteExecution_PreferredUnavailableFallsBack(t *testing.T) {
	capacity := availableCapacity()
	capacity[domain.HarnessClaudeCode] = domain.CapacityCooldown
	d := RouteExecution(RoutingRequest{
		Role:       domain.WorkflowRoleWorker,
		Complexity: ComplexityNormal,
		Capacity:   capacity,
		Policy:     domain.DefaultRoutingPolicy(),
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

// Test #5: both harnesses unavailable waits for capacity, never fails.
func TestRouteExecution_BothUnavailableWaits(t *testing.T) {
	capacity := map[domain.AgentHarness]domain.CapacityState{
		domain.HarnessCodex:      domain.CapacityCooldown,
		domain.HarnessClaudeCode: domain.CapacityUnavailable,
	}
	d := RouteExecution(RoutingRequest{
		Role:       domain.WorkflowRoleWorker,
		Complexity: ComplexityNormal,
		Capacity:   capacity,
		Policy:     domain.DefaultRoutingPolicy(),
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
		Role:               domain.WorkflowRoleReviewer,
		Complexity:         ComplexityNormal,
		CurrentImplementer: domain.HarnessClaudeCode,
		Capacity:           availableCapacity(),
		Policy:             domain.DefaultRoutingPolicy(),
	})
	if d.SelectedHarness != domain.HarnessCodex {
		t.Fatalf("selected = %q, want codex", d.SelectedHarness)
	}
}

// Test #7: Codex worker -> Claude Code reviewer (cross-provider independence).
func TestRouteExecution_ReviewerCodexWorkerToClaudeReviewer(t *testing.T) {
	d := RouteExecution(RoutingRequest{
		Role:               domain.WorkflowRoleReviewer,
		Complexity:         ComplexityNormal,
		CurrentImplementer: domain.HarnessCodex,
		Capacity:           availableCapacity(),
		Policy:             domain.DefaultRoutingPolicy(),
	})
	if d.SelectedHarness != domain.HarnessClaudeCode {
		t.Fatalf("selected = %q, want claude-code", d.SelectedHarness)
	}
}

// Test #8: high-risk reviewer independence can never fall back to the same
// provider, even when the policy opt-in for normal complexity is enabled and
// the opposite provider is unavailable — it must wait instead.
func TestRouteExecution_HighRiskReviewerNeverSameProviderFallback(t *testing.T) {
	capacity := availableCapacity()
	capacity[domain.HarnessCodex] = domain.CapacityCooldown // opposite of claude-code implementer
	policy := domain.DefaultRoutingPolicy()
	policy.AllowSameProviderReviewFallbackForNormal = true // must not apply to high-risk
	d := RouteExecution(RoutingRequest{
		Role:               domain.WorkflowRoleReviewer,
		Complexity:         ComplexityHighRisk,
		CurrentImplementer: domain.HarnessClaudeCode,
		Capacity:           capacity,
		Policy:             policy,
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
	capacity[domain.HarnessCodex] = domain.CapacityCooldown

	// Default policy: no opt-in -> waits.
	d := RouteExecution(RoutingRequest{
		Role:               domain.WorkflowRoleReviewer,
		Complexity:         ComplexityNormal,
		CurrentImplementer: domain.HarnessClaudeCode,
		Capacity:           capacity,
		Policy:             domain.DefaultRoutingPolicy(),
	})
	if !d.Waiting {
		t.Fatalf("waiting = false, want true without opt-in")
	}

	// Opt-in enabled: falls back to claude-code (the asking/implementing
	// provider itself).
	policy := domain.DefaultRoutingPolicy()
	policy.AllowSameProviderReviewFallbackForNormal = true
	d2 := RouteExecution(RoutingRequest{
		Role:               domain.WorkflowRoleReviewer,
		Complexity:         ComplexityNormal,
		CurrentImplementer: domain.HarnessClaudeCode,
		Capacity:           capacity,
		Policy:             policy,
	})
	if d2.Waiting || d2.SelectedHarness != domain.HarnessClaudeCode {
		t.Fatalf("decision = %+v, want same-provider fallback to claude-code", d2)
	}
}

// Test #9 (review skipped creates no reviewer) is covered by the existing
// Checkpoint 8I suite (review_policy_integration_test.go); no routing
// decision is ever made for a step ReviewPolicy skips outright, since
// dispatchReviewStep short-circuits to applyReviewPolicySkip before
// reviewerHarnessForStep is ever called.

// Test #10: planner policy prefers the configured harness and waits (never
// guesses a second implementation) when it is unavailable.
func TestRouteExecution_PlannerPolicy(t *testing.T) {
	d := RouteExecution(RoutingRequest{
		Role:     domain.WorkflowRolePlanner,
		Capacity: availableCapacity(),
		Policy:   domain.DefaultRoutingPolicy(),
	})
	if d.SelectedHarness != domain.HarnessClaudeCode {
		t.Fatalf("selected = %q, want claude-code", d.SelectedHarness)
	}
	if len(d.FallbackOrder) != 0 {
		t.Fatalf("fallback order = %v, want none for planner", d.FallbackOrder)
	}

	capacity := availableCapacity()
	capacity[domain.HarnessClaudeCode] = domain.CapacityUnavailable
	d2 := RouteExecution(RoutingRequest{
		Role:     domain.WorkflowRolePlanner,
		Capacity: capacity,
		Policy:   domain.DefaultRoutingPolicy(),
	})
	if !d2.Waiting {
		t.Fatalf("waiting = false, want true: no second planner implementation exists")
	}
}

// Test #13: reason codes are always part of the closed enum.
func TestRouteExecution_ReasonCodesAreClosedEnum(t *testing.T) {
	cases := []RoutingRequest{
		{Role: domain.WorkflowRoleWorker, Complexity: ComplexityTrivial, Capacity: availableCapacity(), Policy: domain.DefaultRoutingPolicy()},
		{Role: domain.WorkflowRoleWorker, Complexity: ComplexityNormal, Capacity: map[domain.AgentHarness]domain.CapacityState{domain.HarnessCodex: domain.CapacityCooldown, domain.HarnessClaudeCode: domain.CapacityCooldown}, Policy: domain.DefaultRoutingPolicy()},
		{Role: domain.WorkflowRoleReviewer, Complexity: ComplexityHighRisk, CurrentImplementer: domain.HarnessCodex, Capacity: availableCapacity(), Policy: domain.DefaultRoutingPolicy()},
		{Role: domain.WorkflowRolePlanner, Capacity: availableCapacity(), Policy: domain.DefaultRoutingPolicy()},
	}
	for _, req := range cases {
		d := RouteExecution(req)
		if d.PolicyVersion != domain.RoutingPolicyVersion {
			t.Fatalf("policy version = %q, want %q", d.PolicyVersion, domain.RoutingPolicyVersion)
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
		Role:       domain.WorkflowRoleWorker,
		Complexity: ComplexityTrivial,
		Capacity:   capacity,
		Policy:     domain.DefaultRoutingPolicy(),
	})
	if !reflect.DeepEqual(d.CapacityStateAtDecision, capacity) {
		t.Fatalf("capacity snapshot = %v, want %v", d.CapacityStateAtDecision, capacity)
	}
}

// Unknown capacity defaults to eligible, matching domain.AgentHealth's own
// unknown-is-available semantics.
func TestRouteExecution_UnknownCapacityIsEligible(t *testing.T) {
	d := RouteExecution(RoutingRequest{
		Role:       domain.WorkflowRoleWorker,
		Complexity: ComplexityTrivial,
		Capacity:   map[domain.AgentHarness]domain.CapacityState{domain.HarnessCodex: domain.CapacityUnknown},
		Policy:     domain.DefaultRoutingPolicy(),
	})
	if d.Waiting || d.SelectedHarness != domain.HarnessCodex {
		t.Fatalf("decision = %+v, want unknown capacity treated as eligible", d)
	}
}
