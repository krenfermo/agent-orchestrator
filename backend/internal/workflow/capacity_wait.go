package workflow

import (
	stdctx "context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// This file is the normalized waiting/capacity projection.
//
// It exists because the frontend was being asked to reconstruct provider policy
// from raw strings: a `waitReason` of "reviewer_capacity", a `nextAction` of
// "waiting_for_capacity: role=reviewer", and a routing decision's reason-code
// array were the only evidence available, and none of them says the one thing a
// person actually wants to know — which provider is blocked, why, since when,
// and what AO is going to do about it. Answering that in React means encoding
// health policy in two places, which guarantees the two will disagree.
//
// Everything here is derived at read time from durable rows. No field is
// estimated: an unknown value stays nil rather than being guessed.

// CapacityWaitReason is the closed vocabulary of why a run is parked on
// provider capacity. It is a normalization of health facts, not a restatement
// of the wake reason (which says which ROLE is blocked, not why).
type CapacityWaitReason string

const (
	// CapacityWaitProviderHealthStale is the wf-57f90ff2 case, named: the
	// blocking observation is a past failure whose bounded window has expired,
	// so AO no longer has current evidence and is re-probing the provider.
	CapacityWaitProviderHealthStale CapacityWaitReason = "provider_health_stale"
	// CapacityWaitProviderCooldown is a live, unexpired cooldown.
	CapacityWaitProviderCooldown CapacityWaitReason = "provider_cooldown"
	// CapacityWaitProviderUnavailable is a provider blocked by a condition time
	// cannot clear (auth, a missing CLI).
	CapacityWaitProviderUnavailable CapacityWaitReason = "provider_unavailable"
	// CapacityWaitNoEligibleProvider means nothing owned by this run's owner is
	// enabled, connected and capable for the role at all -- a configuration
	// gap, not a capacity one.
	CapacityWaitNoEligibleProvider CapacityWaitReason = "no_eligible_provider"
)

// CapacityWait is the whole projection for one run.
type CapacityWait struct {
	// Role is the role that could not be dispatched (reviewer, worker, ...).
	Role domain.WorkflowRole
	// Reason is the normalized cause. Always set when CapacityWait is non-nil.
	Reason CapacityWaitReason
	// IndependenceRequired records that the wait is additionally constrained by
	// review independence: the implementer's own provider is not a legal
	// substitute however available it is. Without this the UI would suggest
	// "just use the other provider", which is precisely what AO must not do.
	IndependenceRequired bool
	// NextAttemptAt/KnownResetAt/Attempt mirror the run's soonest open wake.
	// KnownResetAt is non-nil only when a provider actually reported a reset.
	NextAttemptAt *time.Time
	KnownResetAt  *time.Time
	Attempt       int64
	// Probing is true when at least one blocked provider is currently eligible
	// for a capacity probe, i.e. AO is actively re-evaluating rather than
	// waiting out a clock.
	Probing   bool
	Providers []CapacityWaitProvider
}

// CapacityWaitProvider is one candidate provider's capacity evidence.
type CapacityWaitProvider struct {
	ProfileID   domain.ProviderProfileID
	Provider    string
	Harness     domain.AgentHarness
	DisplayName string
	// Capacity is the state routing actually used for this profile.
	Capacity domain.CapacityState
	// HealthState/HealthReason/FailureClass/ObservedAt describe the durable
	// observation behind it. ObservedAt is what "provider health age" is
	// computed from; nil means nothing has ever been observed.
	HealthState  domain.AgentHealthState
	HealthReason string
	FailureClass domain.WorkflowErrorClass
	ObservedAt   *time.Time
	// Recovery is how this provider's state can clear (cooldown/probe/manual).
	Recovery      domain.AgentHealthRecovery
	CooldownUntil *time.Time
	// ProbeEligible reports that AO may re-test this provider now.
	ProbeEligible bool
}

// deriveCapacityWait projects a run's current provider-capacity wait, or nil
// when the run is not waiting on one.
//
// Deliberately read-only: it never probes. Probing belongs to the routing path
// (capacityEvidenceForProfiles), which the wake and the reconcile cascade drive;
// a projection that spawned subprocesses would make every board poll a probe.
func (c *Coordinator) deriveCapacityWait(ctx stdctx.Context, detail RunDetail) *CapacityWait {
	decision, ok := c.waitingRoutingDecision(ctx, detail.Run.ID)
	if !ok {
		return nil
	}
	wait := &CapacityWait{
		Role:          decision.Role,
		Reason:        CapacityWaitNoEligibleProvider,
		NextAttemptAt: detail.NextWakeAt,
		Attempt:       detail.WakeAttemptCount,
	}
	for _, code := range decision.ReasonCodes {
		if code == domain.RoutingReasonReviewIndependenceRequired {
			wait.IndependenceRequired = true
		}
	}

	owner := c.runOwner(ctx, detail.Run.ID)
	capability, hasCapability := domain.RequiredCapability(decision.Role)
	if !hasCapability {
		return wait
	}
	eligible, _ := domain.EligibleProfiles(c.resolvedProfiles(ctx, owner), registry.ProviderDescriptors(), capability)
	now := c.clock()
	for id, profile := range eligible {
		health, err := c.agentHealth(ctx, profile.Harness, healthScope{userID: owner, profileID: id})
		if err != nil {
			continue
		}
		state, known := decision.CapacityStateByProfile[id]
		if !known {
			state = domain.CapacityStateFromHealth(health.EffectiveState(now))
		}
		entry := CapacityWaitProvider{
			ProfileID: id, Provider: profile.Provider, Harness: profile.Harness,
			DisplayName: profile.DisplayName, Capacity: state,
			HealthState: health.State, HealthReason: health.Reason, FailureClass: health.FailureClass,
			Recovery: health.Recovery, CooldownUntil: health.CooldownUntil,
			ProbeEligible: health.ProbeEligible(now),
		}
		if !health.ObservedAt.IsZero() {
			at := health.ObservedAt
			entry.ObservedAt = &at
		}
		if entry.ProbeEligible && entry.Capacity != domain.CapacityAvailable {
			wait.Probing = true
		}
		if health.CooldownUntil != nil && wait.KnownResetAt == nil && health.FailureClass == domain.WorkflowErrorRateLimited {
			reset := *health.CooldownUntil
			wait.KnownResetAt = &reset
		}
		wait.Providers = append(wait.Providers, entry)
	}
	sortCapacityWaitProviders(wait.Providers)
	wait.Reason = capacityWaitReason(wait.Providers)
	return wait
}

// capacityWaitReason picks the single most actionable cause across the
// candidates. Stale beats cooldown beats unavailable: a provider AO is actively
// re-probing is the most truthful thing to report, because it is the one thing
// that is about to change.
func capacityWaitReason(providers []CapacityWaitProvider) CapacityWaitReason {
	if len(providers) == 0 {
		return CapacityWaitNoEligibleProvider
	}
	reason := CapacityWaitProviderUnavailable
	for _, p := range providers {
		switch {
		case p.ProbeEligible && p.Capacity != domain.CapacityAvailable:
			return CapacityWaitProviderHealthStale
		case p.Capacity == domain.CapacityCooldown || p.Capacity == domain.CapacityLimited:
			reason = CapacityWaitProviderCooldown
		}
	}
	return reason
}

func sortCapacityWaitProviders(providers []CapacityWaitProvider) {
	for i := 1; i < len(providers); i++ {
		for j := i; j > 0 && providers[j].ProfileID < providers[j-1].ProfileID; j-- {
			providers[j], providers[j-1] = providers[j-1], providers[j]
		}
	}
}

// waitingRoutingDecision returns the newest persisted routing decision for a
// run when that decision is a wait. Routing decisions are the durable record of
// exactly which providers were considered and what capacity they reported, so
// the projection explains the decision AO actually took rather than recomputing
// a fresh one that might differ.
func (c *Coordinator) waitingRoutingDecision(ctx stdctx.Context, runID string) (domain.RoutingDecision, bool) {
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return domain.RoutingDecision{}, false
	}
	var latest *domain.WorkflowCheckpoint
	for i := range checkpoints {
		cp := &checkpoints[i]
		if cp.DurablePhase != routingDecisionDurablePhase {
			continue
		}
		if latest == nil || cp.CreatedAt.After(latest.CreatedAt) {
			latest = cp
		}
	}
	if latest == nil {
		return domain.RoutingDecision{}, false
	}
	decision, ok := decodeRoutingDecision(latest.RetryState)
	if !ok || !decision.Waiting {
		return domain.RoutingDecision{}, false
	}
	return decision, true
}
