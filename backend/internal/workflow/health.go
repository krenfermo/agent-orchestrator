package workflow

import (
	stdctx "context"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// agentHealth derives a harness's current health from its latest recorded
// event, defaulting to domain.AgentHealthUnknown when nothing has ever been
// recorded (Checkpoint 8H §5).
func (c *Coordinator) agentHealth(ctx stdctx.Context, harness domain.AgentHarness) (domain.AgentHealth, error) {
	ev, ok, err := c.store.GetAgentHealth(ctx, harness)
	if err != nil {
		return domain.AgentHealth{}, err
	}
	if !ok {
		return domain.AgentHealth{Harness: harness, State: domain.AgentHealthUnknown}, nil
	}
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
	return h, nil
}

// recordAgentHealthFailure appends a durable failure fact for a harness
// (Checkpoint 8H §5-6). A rate-limited/capacity/transient class enters
// cooldown; anything else (binary missing, auth, an unclassified default)
// enters unavailable, since a cooldown timer cannot heal those. No
// cooldown_until is ever invented: 8H has no reliable typed reset for
// workflow's TUI worker sessions (only Codex Chat-mode conversations expose
// one today — see the audit), so CooldownUntil stays nil, meaning "unknown
// reset, do not treat as scheduled to clear."
func (c *Coordinator) recordAgentHealthFailure(ctx stdctx.Context, harness domain.AgentHarness, cls ProviderFailureClassification, now time.Time) {
	prevConsecutive := int64(0)
	if ev, ok, err := c.store.GetAgentHealth(ctx, harness); err == nil && ok {
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
		State:               state,
		Reason:              fmt.Sprintf("%s (%s)", cls.Class, cls.Certainty),
		FailureClass:        cls.Class,
		ConsecutiveFailures: prevConsecutive + 1,
		CreatedAt:           now,
	})
}

// recordAgentHealthSuccess resets a harness's health to available after a
// successful dispatch, so a prior failure/cooldown does not permanently block
// future attempts once the provider is demonstrably working again.
func (c *Coordinator) recordAgentHealthSuccess(ctx stdctx.Context, harness domain.AgentHarness, now time.Time) {
	_, _ = c.store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
		ID:        "ahe-" + c.newID(),
		Harness:   harness,
		State:     domain.AgentHealthAvailable,
		Reason:    "dispatch succeeded",
		CreatedAt: now,
	})
}
