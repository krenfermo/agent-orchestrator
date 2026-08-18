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

func agentHealthFromEvent(harness domain.AgentHarness, ev domain.AgentHealthEvent) domain.AgentHealth {
	h := domain.AgentHealth{
		Harness:             harness,
		State:               ev.State,
		Reason:              ev.Reason,
		FailureClass:        ev.FailureClass,
		CooldownUntil:       ev.CooldownUntil,
		ConsecutiveFailures: ev.ConsecutiveFailures,
	}
	if ev.State == domain.AgentHealthAvailable {
		t := ev.CreatedAt
		h.LastSuccessAt = &t
	} else {
		t := ev.CreatedAt
		h.LastFailureAt = &t
	}
	return h
}

// recordAgentHealthFailure appends a durable failure fact for a harness
// (Checkpoint 8H §5-6), scoped to scope's user+profile when known
// (Checkpoint 8P-C) so a failure observed for one user's connection never
// counts against another user's -- or another profile's -- consecutive
// failure streak. A rate-limited/capacity/transient class enters cooldown;
// anything else (binary missing, auth, an unclassified default) enters
// unavailable, since a cooldown timer cannot heal those. No cooldown_until
// is ever invented: 8H has no reliable typed reset for workflow's TUI worker
// sessions (only Codex Chat-mode conversations expose one today — see the
// audit), so CooldownUntil stays nil, meaning "unknown reset, do not treat
// as scheduled to clear."
func (c *Coordinator) recordAgentHealthFailure(ctx stdctx.Context, harness domain.AgentHarness, scope healthScope, cls ProviderFailureClassification, now time.Time) {
	prevConsecutive := int64(0)
	if ev, ok, err := c.agentHealthEventForScope(ctx, harness, scope); err == nil && ok {
		prevConsecutive = ev.ConsecutiveFailures
	}
	state := domain.AgentHealthUnavailable
	switch cls.Class {
	case domain.WorkflowErrorRateLimited, domain.WorkflowErrorCapacityExhausted, domain.WorkflowErrorTransient:
		state = domain.AgentHealthCooldown
	}
	_, _ = c.store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
		ID:                  "ahe-" + c.newID(),
		Harness:             harness,
		UserID:              scope.userID,
		ProviderProfileID:   scope.profileID,
		State:               state,
		Reason:              fmt.Sprintf("%s (%s)", cls.Class, cls.Certainty),
		FailureClass:        cls.Class,
		ConsecutiveFailures: prevConsecutive + 1,
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
		CreatedAt:         now,
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
