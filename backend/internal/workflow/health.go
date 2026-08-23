package workflow

import (
	stdctx "context"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// healthScope (Checkpoint 8P-C) is the (user, provider profile) a health
// fact is recorded/read against. The zero value means "legacy/global": an
// unowned run, or an owned run whose runtimeIsolation resolution found no
// matching profile (e.g. trusted-local with no profile configured) --
// exactly today's pre-8P-C behavior. Every dispatch/failover call site
// resolves this once (from resolveRuntimeEnv, which already computes it for
// runtime env isolation) and threads it down, rather than re-deriving it
// per health call.
type healthScope struct {
	userID    domain.UserID
	profileID domain.ProviderProfileID
}

func (s healthScope) scoped() bool { return s.userID != "" && s.profileID != "" }

// agentHealth derives a harness's current health (Checkpoint 8H), preferring
// the most specific scoped fact for scope over a legacy/global one
// (Checkpoint 8P-C's precedence rule): a scoped row for this exact
// user+profile+harness always wins; a legacy/global row is only ever
// consulted when no scoped row exists yet for that connection. Defaults to
// domain.AgentHealthUnknown when nothing has ever been recorded either way.
func (c *Coordinator) agentHealth(ctx stdctx.Context, harness domain.AgentHarness, scope healthScope) (domain.AgentHealth, error) {
	if scope.scoped() {
		ev, ok, err := c.store.GetAgentHealthScoped(ctx, harness, scope.userID, scope.profileID)
		if err != nil {
			return domain.AgentHealth{}, err
		}
		if ok {
			return agentHealthFromEvent(harness, ev), nil
		}
		// No scoped fact yet for this connection: fall back to legacy/global
		// only as a compatibility read, never letting it be treated as more
		// authoritative than a scoped row would be once one exists.
	}
	ev, ok, err := c.store.GetAgentHealth(ctx, harness)
	if err != nil {
		return domain.AgentHealth{}, err
	}
	if !ok {
		return domain.AgentHealth{Harness: harness, State: domain.AgentHealthUnknown}, nil
	}
	return agentHealthFromEvent(harness, ev), nil
}

// agentHealthFromEvent derives the read-time health of a harness from its
// latest durable event, applying domain.AgentHealthPolicyForFailure to the
// recorded failure class rather than trusting the stored state verbatim.
//
// Deriving rather than trusting is what heals the rows already on disk. A
// failure recorded before this policy existed persisted state=unavailable with
// cooldown_until=NULL for ANY class that was not rate/capacity/transient --
// including agent_start_failed, the conservative default for a failure nothing
// distinguished. Read verbatim, such a row blocks the provider forever and can
// only be superseded by a successful dispatch that routing will never allow.
// Re-read under the policy, the same row becomes what it always meant: a
// transient failure whose bounded cooldown expired long ago, eligible for a
// probe. No migration, no manual row insertion, no special case for one
// workflow.
//
// A row with no failure class at all (a success, or a probe conclusion) is not
// a failure and keeps its recorded state; an unavailable one is marked
// probe-recoverable, because a probe is exactly what produced it.
func agentHealthFromEvent(harness domain.AgentHarness, ev domain.AgentHealthEvent) domain.AgentHealth {
	h := domain.AgentHealth{
		Harness:             harness,
		State:               ev.State,
		Reason:              ev.Reason,
		FailureClass:        ev.FailureClass,
		CooldownUntil:       ev.CooldownUntil,
		ConsecutiveFailures: ev.ConsecutiveFailures,
		ObservedAt:          ev.CreatedAt,
	}
	if ev.State == domain.AgentHealthAvailable {
		t := ev.CreatedAt
		h.LastSuccessAt = &t
	} else {
		t := ev.CreatedAt
		h.LastFailureAt = &t
	}
	switch {
	case ev.State == domain.AgentHealthAvailable, ev.State == domain.AgentHealthUnknown:
		h.Recovery = domain.AgentHealthRecoveryNone
	case ev.FailureClass != "":
		policy := domain.AgentHealthPolicyForFailure(ev.FailureClass)
		h.State = policy.State
		h.Recovery = policy.Recovery
		if h.State == domain.AgentHealthCooldown && h.CooldownUntil == nil {
			until := ev.CreatedAt.Add(policy.CooldownFor(ev.ConsecutiveFailures))
			h.CooldownUntil = &until
		}
	case ev.State == domain.AgentHealthUnavailable:
		h.Recovery = domain.AgentHealthRecoveryProbe
	default:
		h.Recovery = domain.AgentHealthRecoveryCooldown
	}
	return h
}

// recordAgentHealthFailure appends a durable failure fact for a harness
// (Checkpoint 8H §5-6), scoped to scope's user+profile when known
// (Checkpoint 8P-C) so a failure observed for one user's connection never
// counts against another user's -- or another profile's -- consecutive
// failure streak.
//
// The state and its expiry both come from domain.AgentHealthPolicyForFailure,
// so the row on disk already carries the semantics rather than depending on the
// reader to supply them (which is what makes a cooldown survive a daemon
// restart honestly rather than restarting its clock).
//
// cooldown_until is still never FABRICATED for a class the policy does not
// time-box, and a provider-reported reset always wins over AO's own backoff:
// cls.ResetAt is populated only from a typed provider signal (see
// classifyProviderFailure), never from parsed prose.
func (c *Coordinator) recordAgentHealthFailure(ctx stdctx.Context, harness domain.AgentHarness, scope healthScope, cls ProviderFailureClassification, now time.Time) {
	prevConsecutive := int64(0)
	if ev, ok, err := c.agentHealthEventForScope(ctx, harness, scope); err == nil && ok {
		prevConsecutive = ev.ConsecutiveFailures
	}
	consecutive := prevConsecutive + 1
	policy := domain.AgentHealthPolicyForFailure(cls.Class)
	var cooldownUntil *time.Time
	if policy.State == domain.AgentHealthCooldown {
		if cls.ResetAt != nil && cls.ResetAt.After(now) {
			reset := *cls.ResetAt
			cooldownUntil = &reset
		} else {
			until := now.Add(policy.CooldownFor(consecutive))
			cooldownUntil = &until
		}
	}
	_, _ = c.store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
		ID:                  "ahe-" + c.newID(),
		Harness:             harness,
		UserID:              scope.userID,
		ProviderProfileID:   scope.profileID,
		State:               policy.State,
		Reason:              fmt.Sprintf("%s (%s)", cls.Class, cls.Certainty),
		FailureClass:        cls.Class,
		CooldownUntil:       cooldownUntil,
		ConsecutiveFailures: consecutive,
		CreatedAt:           now,
	})
}

// recordAgentHealthSuccess resets a harness's health to available after a
// successful dispatch, scoped to scope the same way recordAgentHealthFailure
// is, so a prior failure/cooldown on another user's connection is never
// what gets cleared by this user's success.
func (c *Coordinator) recordAgentHealthSuccess(ctx stdctx.Context, harness domain.AgentHarness, scope healthScope, now time.Time) {
	_, _ = c.store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
		ID:                "ahe-" + c.newID(),
		Harness:           harness,
		UserID:            scope.userID,
		ProviderProfileID: scope.profileID,
		State:             domain.AgentHealthAvailable,
		Reason:            "dispatch succeeded",
		// Explicit, not incidental: a success is the strongest evidence AO can
		// have, and it resets the consecutive-failure streak that drives the
		// next failure's cooldown backoff.
		ConsecutiveFailures: 0,
		CreatedAt:           now,
	})
}

// agentHealthEventForScope reads the latest raw event for scope (or
// legacy/global if scope is unscoped), used internally to seed
// ConsecutiveFailures -- never exported, since every other reader wants the
// derived domain.AgentHealth from agentHealth instead.
func (c *Coordinator) agentHealthEventForScope(ctx stdctx.Context, harness domain.AgentHarness, scope healthScope) (domain.AgentHealthEvent, bool, error) {
	if scope.scoped() {
		return c.store.GetAgentHealthScoped(ctx, harness, scope.userID, scope.profileID)
	}
	return c.store.GetAgentHealth(ctx, harness)
}
