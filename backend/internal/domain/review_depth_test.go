package domain_test

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestReviewDepthOrdering(t *testing.T) {
	t.Parallel()
	if !domain.ReviewDepthDeep.AtLeast(domain.ReviewDepthLight) {
		t.Error("deep must be at least as deep as light")
	}
	if domain.ReviewDepthNone.AtLeast(domain.ReviewDepthLight) {
		t.Error("none must not satisfy a light floor")
	}
	// An unrecognised depth must never satisfy any floor: a value AO cannot
	// rank is not a value it may treat as sufficient scrutiny.
	if domain.ReviewDepth("thorough-ish").AtLeast(domain.ReviewDepthNone) {
		t.Error("an unknown depth must not satisfy even the none floor")
	}
	if got := domain.DeeperOf(domain.ReviewDepthLight, domain.ReviewDepthNone); got != domain.ReviewDepthLight {
		t.Errorf("DeeperOf(light, none) = %q, want light", got)
	}
}

func TestReviewRiskTierFloors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		tier domain.ReviewRiskTier
		want domain.ReviewDepth
	}{
		{domain.ReviewRiskLow, domain.ReviewDepthNone},
		{domain.ReviewRiskStandard, domain.ReviewDepthLight},
		{domain.ReviewRiskHigh, domain.ReviewDepthDeep},
		// An unrecognised tier floors at deep, not at none.
		{domain.ReviewRiskTier("catastrophic"), domain.ReviewDepthDeep},
	} {
		if got := tc.tier.MinimumDepth(); got != tc.want {
			t.Errorf("%q.MinimumDepth() = %q, want %q", tc.tier, got, tc.want)
		}
	}
}

// TestResolveReviewDepthNeverDegrades is the load-bearing property: whatever a
// caller asks for, the effective depth is never shallower than the risk tier's
// floor, for every combination of the two.
func TestResolveReviewDepthNeverDegrades(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	depths := []domain.ReviewDepth{domain.ReviewDepthNone, domain.ReviewDepthLight, domain.ReviewDepthDeep}
	tiers := []domain.ReviewRiskTier{domain.ReviewRiskLow, domain.ReviewRiskStandard, domain.ReviewRiskHigh}
	for _, requested := range depths {
		for _, tier := range tiers {
			snapshot := domain.ReviewDepthPolicySnapshot{
				Version: domain.ReviewDepthPolicyVersion, Requested: requested,
				Source: domain.ReviewDepthExplicit, At: now,
			}
			got := domain.ResolveReviewDepth(snapshot, tier, nil, now)
			if !got.Effective.AtLeast(tier.MinimumDepth()) {
				t.Errorf("requested=%q tier=%q resolved to %q, which is below the %q floor",
					requested, tier, got.Effective, tier.MinimumDepth())
			}
			if !got.Effective.AtLeast(requested) {
				t.Errorf("requested=%q tier=%q resolved to %q, which is shallower than the request",
					requested, tier, got.Effective)
			}
			if got.Requested != requested {
				t.Errorf("the request must survive the clamp: got %q, want %q", got.Requested, requested)
			}
			if got.PolicyVersion != domain.ReviewDepthPolicyVersion {
				t.Errorf("every decision must name the policy that produced it, got %q", got.PolicyVersion)
			}
		}
	}
}

// A high-risk change is reviewed in full even when the run explicitly asked for
// no reviewer at all, and the decision says the clamp is why.
func TestResolveReviewDepthHighRiskIsNonDegradable(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	got := domain.ResolveReviewDepth(domain.ReviewDepthPolicySnapshot{
		Version: domain.ReviewDepthPolicyVersion, Requested: domain.ReviewDepthNone,
		Source: domain.ReviewDepthExplicit, At: now,
	}, domain.ReviewRiskHigh, []string{"auth_or_security_path"}, now)

	if got.Effective != domain.ReviewDepthDeep {
		t.Fatalf("effective depth = %q, want deep", got.Effective)
	}
	if got.Source != domain.ReviewDepthClamped {
		t.Errorf("source = %q, want clamped", got.Source)
	}
	if got.Reason != domain.ReviewDepthReasonClampedByRiskTier {
		t.Errorf("reason = %q, want clamped_by_risk_tier", got.Reason)
	}
	if len(got.RiskReasons) != 1 || got.RiskReasons[0] != "auth_or_security_path" {
		t.Errorf("the clamp must carry the reasons it rested on, got %v", got.RiskReasons)
	}
}

// A run whose snapshot predates the model is read as the depth it already ran
// at (deep), never as something cheaper.
func TestResolveReviewDepthLegacySnapshotReadsAsDeep(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	got := domain.ResolveReviewDepth(domain.ReviewDepthPolicySnapshot{}, domain.ReviewRiskLow, nil, now)
	if got.Effective != domain.ReviewDepthDeep {
		t.Fatalf("a pre-model snapshot resolved to %q, want deep", got.Effective)
	}
	if got.Source != domain.ReviewDepthRecovered || got.Reason != domain.ReviewDepthReasonLegacyDefault {
		t.Errorf("source/reason = %q/%q, want recovered/legacy_default", got.Source, got.Reason)
	}
}

func TestEffectiveReviewDepthPolicyFallsBackToDeep(t *testing.T) {
	t.Parallel()
	// A policy snapshot written before P5-A carries no depth at all.
	var policy domain.WorkflowPolicy
	got := policy.EffectiveReviewDepthPolicy()
	if got.Requested != domain.ReviewDepthDeep {
		t.Fatalf("legacy policy requested depth = %q, want deep", got.Requested)
	}
	if got.Source != domain.ReviewDepthRecovered {
		t.Errorf("source = %q, want recovered", got.Source)
	}
	if got.Version != domain.ReviewDepthPolicyVersion {
		t.Errorf("version = %q, want %q", got.Version, domain.ReviewDepthPolicyVersion)
	}
}

// The per-strategy defaults are the three levels the design promises, and
// master's is the one that makes "master is unaffected" true.
func TestDefaultRequestedReviewDepthPerStrategy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		strategy domain.ExecutionStrategy
		want     domain.ReviewDepth
	}{
		{domain.ExecutionStrategyTask, domain.ReviewDepthNone},
		{domain.ExecutionStrategyAutonomous, domain.ReviewDepthLight},
		{domain.ExecutionStrategyMaster, domain.ReviewDepthDeep},
		{domain.ExecutionStrategy("something-new"), domain.ReviewDepthDeep},
	} {
		if got := domain.DefaultRequestedReviewDepth(tc.strategy); got != tc.want {
			t.Errorf("default for %q = %q, want %q", tc.strategy, got, tc.want)
		}
	}
}

// Whatever the risk tier, a master run resolves to deep — the property that
// makes this change incapable of altering an existing master run's behaviour.
func TestMasterAlwaysResolvesDeep(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	snapshot := domain.DefaultReviewDepthPolicy(domain.ExecutionStrategyMaster, now)
	for _, tier := range []domain.ReviewRiskTier{domain.ReviewRiskLow, domain.ReviewRiskStandard, domain.ReviewRiskHigh} {
		if got := domain.ResolveReviewDepth(snapshot, tier, nil, now); got.Effective != domain.ReviewDepthDeep {
			t.Errorf("master at tier %q resolved to %q, want deep", tier, got.Effective)
		}
	}
}

func TestEscalateReviewDepthOnlyDeepens(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	light := domain.ReviewDepthDecision{
		PolicyVersion: domain.ReviewDepthPolicyVersion,
		Requested:     domain.ReviewDepthLight, Effective: domain.ReviewDepthLight,
		Source: domain.ReviewDepthPolicy, Reason: domain.ReviewDepthReasonStrategyDefault,
		RiskTier: domain.ReviewRiskStandard, DecidedAt: now,
	}
	escalated := domain.EscalateReviewDepth(light, domain.ReviewDepthReasonEscalatedByReviewer, now)
	if escalated.Effective != domain.ReviewDepthDeep {
		t.Fatalf("escalated depth = %q, want deep", escalated.Effective)
	}
	if escalated.Source != domain.ReviewDepthEscalated || escalated.Reason != domain.ReviewDepthReasonEscalatedByReviewer {
		t.Errorf("source/reason = %q/%q", escalated.Source, escalated.Reason)
	}

	// Escalating an already-deep decision is a no-op: it must not rewrite the
	// reason a stricter decision was already taken for.
	clamped := domain.ReviewDepthDecision{
		Effective: domain.ReviewDepthDeep, Source: domain.ReviewDepthClamped,
		Reason: domain.ReviewDepthReasonClampedByRiskTier, RiskTier: domain.ReviewRiskHigh,
	}
	again := domain.EscalateReviewDepth(clamped, domain.ReviewDepthReasonEscalatedByCycles, now)
	if again.Reason != domain.ReviewDepthReasonClampedByRiskTier || again.Source != domain.ReviewDepthClamped {
		t.Errorf("escalating a deep decision rewrote it: %+v", again)
	}
}

func TestNormalizeRequestedReviewDepth(t *testing.T) {
	t.Parallel()
	if got := domain.NormalizeRequestedReviewDepth("  LIGHT "); got != domain.RequestedReviewDepthLight {
		t.Errorf("normalize = %q, want light", got)
	}
	// Unrecognised input comes back unchanged so the caller rejects it rather
	// than silently running something nobody asked for.
	if got := domain.NormalizeRequestedReviewDepth("shallow"); got.Valid() {
		t.Errorf("%q must not normalize to a valid depth", got)
	}
}
