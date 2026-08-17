package workflow

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func defaultLifecyclePolicyFixture() domain.WorkflowPolicy {
	return domain.DefaultWorkflowPolicy()
}

// Test #1: same task, healthy session, no pressure -> REUSE.
func TestDecideSessionLifecycle_SameTaskHealthyReuses(t *testing.T) {
	d := DecideSessionLifecycle(SessionLifecycleRequest{
		Role: domain.WorkflowRoleWorker, CurrentSessionID: "sess-1",
		SessionHealth: domain.SessionHealthRunning, UsageKnown: true,
		Policy: defaultLifecyclePolicyFixture(),
	})
	if d.Action != domain.LifecycleReuse {
		t.Fatalf("action = %q, want reuse", d.Action)
	}
	if len(d.Reasons) != 1 || d.Reasons[0] != domain.LifecycleReasonSameTaskHealthy {
		t.Fatalf("reasons = %v, want [same_task_healthy]", d.Reasons)
	}
}

// Test #2: task boundary -> NEW_SESSION, even with a perfectly healthy
// current session.
func TestDecideSessionLifecycle_TaskBoundaryForcesNewSession(t *testing.T) {
	d := DecideSessionLifecycle(SessionLifecycleRequest{
		Role: domain.WorkflowRoleWorker, CurrentSessionID: "sess-1",
		SessionHealth: domain.SessionHealthRunning, TaskBoundary: true, UsageKnown: true,
		Policy: defaultLifecyclePolicyFixture(),
	})
	if d.Action != domain.LifecycleNewSession {
		t.Fatalf("action = %q, want new_session", d.Action)
	}
	if len(d.Reasons) != 1 || d.Reasons[0] != domain.LifecycleReasonTaskBoundary {
		t.Fatalf("reasons = %v, want [task_boundary]", d.Reasons)
	}
}

// Test #3: provider switch -> NEW_SESSION.
func TestDecideSessionLifecycle_ProviderSwitchForcesNewSession(t *testing.T) {
	d := DecideSessionLifecycle(SessionLifecycleRequest{
		Role: domain.WorkflowRoleWorker, CurrentSessionID: "sess-1",
		SessionHealth: domain.SessionHealthRunning, ProviderSwitch: true, UsageKnown: true,
		Policy: defaultLifecyclePolicyFixture(),
	})
	if d.Action != domain.LifecycleNewSession {
		t.Fatalf("action = %q, want new_session", d.Action)
	}
	if len(d.Reasons) != 1 || d.Reasons[0] != domain.LifecycleReasonProviderSwitch {
		t.Fatalf("reasons = %v, want [provider_switch]", d.Reasons)
	}
}

// Test #4: fix reuse — a fix cycle count below policy's MaxFixCycles stays
// REUSE (default behavior unchanged).
func TestDecideSessionLifecycle_FixCycleBelowBudgetReuses(t *testing.T) {
	policy := defaultLifecyclePolicyFixture()
	d := DecideSessionLifecycle(SessionLifecycleRequest{
		Role: domain.WorkflowRoleFixWorker, CurrentSessionID: "sess-1",
		SessionHealth: domain.SessionHealthRunning, FixCycleCount: policy.MaxFixCycles - 1, UsageKnown: true,
		Policy: policy,
	})
	if d.Action != domain.LifecycleReuse {
		t.Fatalf("action = %q, want reuse", d.Action)
	}
}

// Test #5: dead/terminated session is never reused.
func TestDecideSessionLifecycle_DeadSessionNeverReused(t *testing.T) {
	d := DecideSessionLifecycle(SessionLifecycleRequest{
		Role: domain.WorkflowRoleWorker, CurrentSessionID: "sess-1",
		SessionHealth: domain.SessionHealthTerminated, UsageKnown: true,
		Policy: defaultLifecyclePolicyFixture(),
	})
	if d.Action != domain.LifecycleNewSession {
		t.Fatalf("action = %q, want new_session", d.Action)
	}
	if len(d.Reasons) != 1 || d.Reasons[0] != domain.LifecycleReasonSessionUnhealthy {
		t.Fatalf("reasons = %v, want [session_unhealthy]", d.Reasons)
	}

	// Stale and Unknown must behave the same way.
	for _, h := range []domain.SessionHealth{domain.SessionHealthStale, domain.SessionHealthUnknown} {
		d := DecideSessionLifecycle(SessionLifecycleRequest{
			Role: domain.WorkflowRoleWorker, CurrentSessionID: "sess-1",
			SessionHealth: h, UsageKnown: true, Policy: defaultLifecyclePolicyFixture(),
		})
		if d.Action != domain.LifecycleNewSession {
			t.Fatalf("health=%q action = %q, want new_session", h, d.Action)
		}
	}
}

// Test #6: unknown usage is conservative — never treated as "no pressure",
// surfaced as an explicit reason alongside whatever action is otherwise
// correct (still reuse here, since nothing else forces a change).
func TestDecideSessionLifecycle_UnknownUsageIsConservativeReason(t *testing.T) {
	d := DecideSessionLifecycle(SessionLifecycleRequest{
		Role: domain.WorkflowRoleWorker, CurrentSessionID: "sess-1",
		SessionHealth: domain.SessionHealthRunning, UsageKnown: false,
		Policy: defaultLifecyclePolicyFixture(),
	})
	if d.Action != domain.LifecycleReuse {
		t.Fatalf("action = %q, want reuse", d.Action)
	}
	found := false
	for _, r := range d.Reasons {
		if r == domain.LifecycleReasonUnknownUsage {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasons = %v, want unknown_usage present", d.Reasons)
	}
}

// Test #7: real (typed, non-inferred) context pressure -> COMPACT, not
// REUSE and not a hard NEW_SESSION.
func TestDecideSessionLifecycle_ActualContextPressureCompacts(t *testing.T) {
	d := DecideSessionLifecycle(SessionLifecycleRequest{
		Role: domain.WorkflowRoleWorker, CurrentSessionID: "sess-1",
		SessionHealth: domain.SessionHealthRunning, ContextPressure: true, UsageKnown: true,
		Policy: defaultLifecyclePolicyFixture(),
	})
	if d.Action != domain.LifecycleCompact {
		t.Fatalf("action = %q, want compact", d.Action)
	}
	if len(d.Reasons) != 1 || d.Reasons[0] != domain.LifecycleReasonContextPressureActual {
		t.Fatalf("reasons = %v, want [context_pressure_actual]", d.Reasons)
	}
}

// Many fix cycles at the run's own configured budget -> COMPACT, reusing
// WorkflowPolicy.MaxFixCycles verbatim, never a new invented number.
func TestDecideSessionLifecycle_ManyFixCyclesAtPolicyBudgetCompacts(t *testing.T) {
	policy := defaultLifecyclePolicyFixture()
	d := DecideSessionLifecycle(SessionLifecycleRequest{
		Role: domain.WorkflowRoleFixWorker, CurrentSessionID: "sess-1",
		SessionHealth: domain.SessionHealthRunning, FixCycleCount: policy.MaxFixCycles, UsageKnown: true,
		Policy: policy,
	})
	if d.Action != domain.LifecycleCompact {
		t.Fatalf("action = %q, want compact", d.Action)
	}
	if len(d.Reasons) != 1 || d.Reasons[0] != domain.LifecycleReasonManyFixCycles {
		t.Fatalf("reasons = %v, want [many_fix_cycles]", d.Reasons)
	}
}

// Many attempts at the run's own configured budget -> COMPACT, reusing
// WorkflowPolicy.MaxWorkProviderAttempts verbatim.
func TestDecideSessionLifecycle_ManyAttemptsAtPolicyBudgetCompacts(t *testing.T) {
	policy := defaultLifecyclePolicyFixture()
	d := DecideSessionLifecycle(SessionLifecycleRequest{
		Role: domain.WorkflowRoleWorker, CurrentSessionID: "sess-1",
		SessionHealth: domain.SessionHealthRunning, AttemptCount: policy.MaxWorkProviderAttempts, UsageKnown: true,
		Policy: policy,
	})
	if d.Action != domain.LifecycleCompact {
		t.Fatalf("action = %q, want compact", d.Action)
	}
	if len(d.Reasons) != 1 || d.Reasons[0] != domain.LifecycleReasonManyAttempts {
		t.Fatalf("reasons = %v, want [many_attempts]", d.Reasons)
	}
}

// No current session at all -> NEW_SESSION trivially.
func TestDecideSessionLifecycle_NoCurrentSessionIsNewSession(t *testing.T) {
	d := DecideSessionLifecycle(SessionLifecycleRequest{
		Role: domain.WorkflowRoleWorker, Policy: defaultLifecyclePolicyFixture(),
	})
	if d.Action != domain.LifecycleNewSession {
		t.Fatalf("action = %q, want new_session", d.Action)
	}
}

// Reason codes are always part of the closed enum, and policy version is
// always stamped.
func TestDecideSessionLifecycle_ReasonCodesClosedEnumAndPolicyVersion(t *testing.T) {
	cases := []SessionLifecycleRequest{
		{Role: domain.WorkflowRoleWorker, CurrentSessionID: "s", SessionHealth: domain.SessionHealthRunning, Policy: defaultLifecyclePolicyFixture()},
		{Role: domain.WorkflowRoleWorker, CurrentSessionID: "s", SessionHealth: domain.SessionHealthTerminated, Policy: defaultLifecyclePolicyFixture()},
		{Role: domain.WorkflowRoleWorker, CurrentSessionID: "s", SessionHealth: domain.SessionHealthRunning, TaskBoundary: true, Policy: defaultLifecyclePolicyFixture()},
		{Role: domain.WorkflowRoleWorker, CurrentSessionID: "s", SessionHealth: domain.SessionHealthRunning, ProviderSwitch: true, Policy: defaultLifecyclePolicyFixture()},
	}
	for _, req := range cases {
		d := DecideSessionLifecycle(req)
		if d.PolicyVersion != domain.SessionLifecyclePolicyVersion {
			t.Fatalf("policy version = %q, want %q", d.PolicyVersion, domain.SessionLifecyclePolicyVersion)
		}
		if len(d.Reasons) == 0 {
			t.Fatalf("decision %+v has no reason codes", d)
		}
		for _, r := range d.Reasons {
			if !r.Valid() {
				t.Fatalf("reason %q not part of closed enum", r)
			}
		}
	}
}
