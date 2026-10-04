package capacityprobe

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// stubAgent implements only what the prober consults. The embedded nil
// ports.Agent satisfies the interface without any of its methods being
// reachable from ProbeCapacity.
type stubAgent struct {
	ports.Agent
	binary     string
	binaryErr  error
	authStatus ports.AgentAuthStatus
	authErr    error
}

func (s stubAgent) ResolveBinary(context.Context) (string, error) {
	return s.binary, s.binaryErr
}

func (s stubAgent) AuthStatus(context.Context) (ports.AgentAuthStatus, error) {
	return s.authStatus, s.authErr
}

// binaryOnlyAgent has a resolvable binary but no auth probe at all.
type binaryOnlyAgent struct {
	ports.Agent
	binary string
}

func (b binaryOnlyAgent) ResolveBinary(context.Context) (string, error) { return b.binary, nil }

// opaqueAgent exposes neither capability: nothing local can be concluded.
type opaqueAgent struct{ ports.Agent }

func proberFor(agent ports.Agent) *Prober {
	return &Prober{agents: map[domain.AgentHarness]ports.Agent{domain.HarnessCodex: agent}}
}

func TestProbeCapacity(t *testing.T) {
	cases := []struct {
		name      string
		agent     ports.Agent
		wantState domain.CapacityState
		wantOK    bool
	}{
		{
			name:      "authenticated cli is available",
			agent:     stubAgent{binary: "/usr/local/bin/codex", authStatus: ports.AgentAuthStatusAuthorized},
			wantState: domain.CapacityAvailable, wantOK: true,
		},
		{
			name:      "unauthenticated cli is unavailable",
			agent:     stubAgent{binary: "/usr/local/bin/codex", authStatus: ports.AgentAuthStatusUnauthorized},
			wantState: domain.CapacityUnavailable, wantOK: true,
		},
		{
			// A stored profile records what was true at connect time. If the
			// binary is gone now, the harness provably cannot accept work,
			// whatever auth_state still claims.
			name:      "missing binary is unavailable regardless of stored auth state",
			agent:     stubAgent{binaryErr: errors.New("not found"), authStatus: ports.AgentAuthStatusAuthorized},
			wantState: domain.CapacityUnavailable, wantOK: true,
		},
		{
			name:      "empty binary path is unavailable",
			agent:     stubAgent{binary: "", authStatus: ports.AgentAuthStatusAuthorized},
			wantState: domain.CapacityUnavailable, wantOK: true,
		},
		{
			name:      "inconclusive auth output stays indeterminate",
			agent:     stubAgent{binary: "/usr/local/bin/codex", authStatus: ports.AgentAuthStatusUnknown},
			wantState: domain.CapacityUnknown, wantOK: false,
		},
		{
			name:      "auth probe error stays indeterminate",
			agent:     stubAgent{binary: "/usr/local/bin/codex", authErr: errors.New("timeout")},
			wantState: domain.CapacityUnknown, wantOK: false,
		},
		{
			name:      "binary present with no auth probe is available",
			agent:     binaryOnlyAgent{binary: "/usr/local/bin/codex"},
			wantState: domain.CapacityAvailable, wantOK: true,
		},
		{
			name:      "adapter with no local evidence stays indeterminate",
			agent:     opaqueAgent{},
			wantState: domain.CapacityUnknown, wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, reason, ok, _ := proberFor(tc.agent).ProbeCapacity(context.Background(), domain.HarnessCodex)
			if state != tc.wantState || ok != tc.wantOK {
				t.Fatalf("state=%q ok=%v, want state=%q ok=%v (reason %q)", state, ok, tc.wantState, tc.wantOK, reason)
			}
			if reason == "" {
				t.Fatalf("probe returned no reason: capacity evidence must always be explainable")
			}
		})
	}
}

// An unknown harness is indeterminate, never "unavailable": the prober has no
// evidence either way, and absence of evidence must not park a workflow.
func TestProbeCapacity_UnknownHarnessIsIndeterminate(t *testing.T) {
	state, _, ok, err := New().ProbeCapacity(context.Background(), domain.AgentHarness("nonexistent-harness"))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if state != domain.CapacityUnknown || ok {
		t.Fatalf("state=%q ok=%v, want unknown/false", state, ok)
	}
}

// The real registry must expose both harnesses routing actually uses, or an
// active probe silently degrades to the old reactive behavior.
func TestNew_CoversRoutedHarnesses(t *testing.T) {
	p := New()
	for _, h := range []domain.AgentHarness{domain.HarnessClaudeCode, domain.HarnessCodex} {
		if _, ok := p.agents[h]; !ok {
			t.Fatalf("harness %q has no adapter in the prober", h)
		}
	}
}
