// Package capacityprobe implements Checkpoint 8P-E.13A.4's active capacity
// probe: the adapter behind workflow.CapacityProber.
//
// It answers the question "can this harness accept work right now?" from cheap
// LOCAL evidence only — is the CLI resolvable on this machine, and does its own
// auth/status subcommand report usable credentials. It never launches an agent
// session, never sends a prompt, never spends quota, and never touches a vendor
// web surface (the same self-imposed limits Checkpoint 8J's capacity reader
// documents).
//
// That bounds what a probe may claim, deliberately:
//
//   - CLI missing            -> unavailable  (a hard, locally provable fact)
//   - CLI says unauthorized  -> unavailable  (likewise)
//   - CLI says authorized    -> available    (reachable and usable NOW)
//   - anything else          -> indeterminate; the caller keeps "unknown"
//
// "available" here means reachable and usable, NOT "quota is clear" — no local
// command can prove that. Quota exhaustion keeps arriving through the reactive
// path (workflow/health.go's recordAgentHealthFailure on a real dispatch
// failure), and because agent health reads newest-first, that later, stronger
// evidence always supersedes a probe's optimistic reading.
package capacityprobe

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// probeTimeout bounds one whole ProbeCapacity call. The underlying adapters
// already bound their own subprocesses (3s each), but a harness that resolves
// its binary slowly must not be able to hold up a dispatch path.
const probeTimeout = 8 * time.Second

// Prober probes harnesses through the shipped agent adapters.
type Prober struct {
	// agents maps harness -> adapter, taken from the same registry routing's
	// ProviderDescriptors come from, so a harness can never be probeable but
	// unroutable (or the reverse).
	agents map[domain.AgentHarness]ports.Agent
}

// New builds a Prober over every shipped harnessed adapter.
func New() *Prober {
	agents := make(map[domain.AgentHarness]ports.Agent)
	for _, ha := range registry.Harnessed() {
		agents[ha.Harness] = ha.Agent
	}
	return &Prober{agents: agents}
}

// ProbeCapacity implements workflow.CapacityProber.
//
// ok=false means "could not determine" and the caller must leave the state
// unknown; it is not an error condition, and no durable fact may be written
// from it. A harness with no adapter, or an adapter that exposes neither a
// binary resolver nor an auth check, is always indeterminate rather than being
// declared unavailable on absence of evidence.
func (p *Prober) ProbeCapacity(ctx context.Context, harness domain.AgentHarness) (domain.CapacityState, string, bool, error) {
	if p == nil {
		return domain.CapacityUnknown, "no prober configured", false, nil
	}
	agent, ok := p.agents[harness]
	if !ok {
		return domain.CapacityUnknown, "no adapter for harness " + string(harness), false, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	// A resolvable binary is a precondition for everything else: without it
	// the harness provably cannot accept work, whatever its stored auth_state
	// claims (a stored profile records what was true at connect time, not what
	// is true on this machine now).
	resolver, hasResolver := agent.(ports.AgentBinaryResolver)
	if hasResolver {
		path, err := resolver.ResolveBinary(probeCtx)
		if err != nil || path == "" {
			return domain.CapacityUnavailable, "cli not resolvable: " + string(harness), true, nil
		}
	}

	checker, hasChecker := agent.(ports.AgentAuthChecker)
	if !hasChecker {
		if hasResolver {
			// Binary present, no way to check credentials: reachable, and
			// nothing local contradicts usability.
			return domain.CapacityAvailable, "cli present (no auth probe available)", true, nil
		}
		return domain.CapacityUnknown, "adapter exposes no capacity evidence", false, nil
	}

	status, err := checker.AuthStatus(probeCtx)
	if err != nil {
		return domain.CapacityUnknown, "auth probe failed: " + err.Error(), false, err
	}
	switch status {
	case ports.AgentAuthStatusAuthorized:
		return domain.CapacityAvailable, "cli authenticated", true, nil
	case ports.AgentAuthStatusUnauthorized:
		return domain.CapacityUnavailable, "cli reports not authenticated", true, nil
	default:
		return domain.CapacityUnknown, "auth probe inconclusive", false, nil
	}
}
