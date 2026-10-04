package domain_test

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// PHASE E, case 6. Every strategy keeps the profile figure it has always had
// when no override is recorded, and the Task figure is still the 150,000 the
// measured run anchored it on.
func TestContextPerCallDefaultsAreUnchangedWithoutAnOverride(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		strategy domain.ExecutionStrategy
		want     int64
	}{
		{domain.ExecutionStrategyTask, 150_000},
		{domain.ExecutionStrategyAutonomous, 200_000},
		{domain.ExecutionStrategyMaster, 250_000},
		{"", 200_000}, // unrecognised resolves to the autonomous profile
	} {
		profile := domain.UsageBudgetProfileFor(tt.strategy).
			WithOverrides(domain.WorkflowPolicy{}.EffectiveUsageBudgetPolicy())
		if profile.ContextPerCallTokens != tt.want {
			t.Fatalf("%q default = %d, want %d", tt.strategy, profile.ContextPerCallTokens, tt.want)
		}
	}
}

// PHASE E, case 7. An in-range override replaces the profile figure, and only
// that figure: the other three advisory expectations and every hard ceiling are
// untouched by it.
func TestContextPerCallOverrideAppliesWithoutDisturbingTheRestOfTheProfile(t *testing.T) {
	t.Parallel()
	base := domain.UsageBudgetProfileFor(domain.ExecutionStrategyTask)
	policy := domain.UsageBudgetPolicy{WorkflowContextPerCallWarnTokens: 70_000}
	got := base.WithOverrides(policy)

	if got.ContextPerCallTokens != 70_000 {
		t.Fatalf("ContextPerCallTokens = %d, want 70000", got.ContextPerCallTokens)
	}
	if got.WallClock != base.WallClock || got.ProviderCalls != base.ProviderCalls ||
		got.ContextGrowthTokens != base.ContextGrowthTokens || got.CostUSD != base.CostUSD {
		t.Fatalf("a context-per-call override moved another expectation: %+v vs %+v", got, base)
	}
}

// PHASE E, cases 8 and 11. An out-of-range value is not trusted and not
// clamped: the profile default stands. This is the defence that stops a
// corrupted, hand-edited or future-binary snapshot from putting every session
// permanently under context pressure -- a decision that would change what AO
// DOES, not merely what it warns about.
func TestContextPerCallOverrideIgnoresOutOfRangeValues(t *testing.T) {
	t.Parallel()
	base := domain.UsageBudgetProfileFor(domain.ExecutionStrategyTask)
	for _, bad := range []int64{
		0, // the "no override" sentinel
		1,
		-70_000,
		domain.MinContextPerCallWarnTokens - 1,
		domain.MaxContextPerCallWarnTokens + 1,
		1 << 62,
	} {
		got := base.WithOverrides(domain.UsageBudgetPolicy{WorkflowContextPerCallWarnTokens: bad})
		if got.ContextPerCallTokens != base.ContextPerCallTokens {
			t.Fatalf("override %d changed the threshold to %d; want the %d default to stand",
				bad, got.ContextPerCallTokens, base.ContextPerCallTokens)
		}
		if domain.ValidContextPerCallWarnTokens(bad) {
			t.Fatalf("ValidContextPerCallWarnTokens(%d) = true", bad)
		}
	}
	// The bounds themselves are inclusive, and the lab target sits inside them.
	for _, ok := range []int64{
		domain.MinContextPerCallWarnTokens,
		60_000, 70_000, 80_000,
		150_000,
		domain.MaxContextPerCallWarnTokens,
	} {
		if !domain.ValidContextPerCallWarnTokens(ok) {
			t.Fatalf("ValidContextPerCallWarnTokens(%d) = false", ok)
		}
	}
}

// PHASE E, case 9. The advisory override is not a ceiling and must never be
// mistaken for one: it leaves the hard budget state exactly where it was, and
// a policy carrying only this field is still an unconfigured budget.
func TestContextPerCallOverrideIsNotABudgetCeiling(t *testing.T) {
	t.Parallel()
	policy := domain.UsageBudgetPolicy{WorkflowContextPerCallWarnTokens: 60_000}
	if policy.Configured() {
		t.Fatal("an advisory context threshold made the budget read as configured")
	}
	if policy.WorkflowTokenBudget != 0 || policy.WorkflowCostBudgetUSD != 0 ||
		policy.ProjectDailyTokenBudget != 0 || policy.ProjectDailyCostBudgetUSD != 0 {
		t.Fatalf("a context threshold set a hard ceiling: %+v", policy)
	}
	if policy.EffectiveWarnPercent() != domain.DefaultUsageWarnPercent {
		t.Fatalf("warn percent = %d, want the %d default", policy.EffectiveWarnPercent(), domain.DefaultUsageWarnPercent)
	}
}

// PHASE B, the Usage half. A parent's frozen threshold reaches its children --
// it lives inside Usage, which already inherits, and a child running a fix
// cycle against a different pressure threshold than its parent's contract
// would make the experiment's two arms incomparable.
func TestContextPerCallOverrideIsInheritedWithTheUsagePolicy(t *testing.T) {
	t.Parallel()
	parent := domain.DefaultWorkflowPolicy()
	usage := parent.EffectiveUsageBudgetPolicy()
	usage.WorkflowContextPerCallWarnTokens = 70_000
	parent.Usage = usage

	child := domain.InheritWorkflowPolicy(parent, domain.DefaultWorkflowPolicy())
	if got := child.Usage.WorkflowContextPerCallWarnTokens; got != 70_000 {
		t.Fatalf("child threshold = %d, want the parent's 70000", got)
	}
	if err := domain.RequireInheritedWorkflowPolicy(parent, child); err != nil {
		t.Fatalf("a correctly inherited child was refused: %v", err)
	}
}
