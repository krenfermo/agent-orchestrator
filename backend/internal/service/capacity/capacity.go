// Package capacity exposes Checkpoint 8J's read-only capacity/quota view.
// It creates no new source of truth: every snapshot is derived from the
// existing Checkpoint 8H agent_health_events record for a harness. No
// browser automation, no scraping, no reading of auth/credential files —
// only the durable health facts workflow dispatch already records when it
// succeeds or fails against a harness.
package capacity

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Store is the narrow read contract this reader needs — satisfied by
// *store.Store's existing GetAgentHealth (Checkpoint 8H), no new method.
type Store interface {
	GetAgentHealth(ctx context.Context, harness domain.AgentHarness) (domain.AgentHealthEvent, bool, error)
}

// KnownHarnesses lists the harnesses workflow dispatch actually uses today
// (dispatch.go's worker harness, and the reviewer registry's Claude Code
// adapter). Not an exhaustive harness catalog — only ones 8H can have
// recorded health for.
var KnownHarnesses = []domain.AgentHarness{
	domain.AgentHarness("codex"),
	domain.AgentHarness("claude-code"),
}

// Reader derives CapacitySnapshot read models from stored agent health.
type Reader struct{ store Store }

// NewReader constructs a capacity reader.
func NewReader(store Store) *Reader { return &Reader{store: store} }

// List returns one snapshot per KnownHarnesses entry, in order. A harness
// with no recorded health event yet reports CapacityUnknown/MetricUnknown —
// never a fabricated "available".
func (r *Reader) List(ctx context.Context) ([]domain.CapacitySnapshot, error) {
	if r == nil || r.store == nil {
		return nil, nil
	}
	out := make([]domain.CapacitySnapshot, 0, len(KnownHarnesses))
	for _, h := range KnownHarnesses {
		ev, ok, err := r.store.GetAgentHealth(ctx, h)
		if err != nil {
			return nil, err
		}
		out = append(out, snapshotFrom(h, ev, ok))
	}
	return out, nil
}

func snapshotFrom(h domain.AgentHarness, ev domain.AgentHealthEvent, ok bool) domain.CapacitySnapshot {
	if !ok {
		return domain.CapacitySnapshot{
			Provider: domain.ProviderForHarness(h), Harness: h,
			State: domain.CapacityUnknown, Certainty: domain.MetricUnknown,
			Reason: "no agent health event recorded yet",
		}
	}
	detected := ev.CreatedAt
	return domain.CapacitySnapshot{
		Provider:   domain.ProviderForHarness(h),
		Harness:    h,
		State:      capacityStateFrom(ev.State),
		DetectedAt: &detected,
		// ResetAt stays nil whenever 8H itself never recorded one (see
		// workflow/health.go: no reset timestamp is invented there either).
		ResetAt:   ev.CooldownUntil,
		Reason:    ev.Reason,
		Certainty: domain.MetricActual,
	}
}

func capacityStateFrom(s domain.AgentHealthState) domain.CapacityState {
	switch s {
	case domain.AgentHealthAvailable:
		return domain.CapacityAvailable
	case domain.AgentHealthCooldown:
		return domain.CapacityCooldown
	case domain.AgentHealthUnavailable:
		return domain.CapacityUnavailable
	default:
		return domain.CapacityUnknown
	}
}
