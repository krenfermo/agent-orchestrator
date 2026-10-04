package registry

import "github.com/aoagents/agent-orchestrator/backend/internal/domain"

// ProviderDescriptors returns the provider-neutral descriptor for every
// provider AO knows about (Checkpoint 8P-B). This is static, code-level
// capability description -- it carries no per-user state and is safe to
// serve to any authenticated user via GET /api/v1/providers/registry.
//
// Only capabilities/auth methods a real adapter implementation backs today
// are declared. Claude Code and Codex are the two real, wired providers;
// every other entry (MiniMax) is explicitly marked Available: false so the
// frontend can render it honestly rather than as a working connection.
func ProviderDescriptors() []domain.ProviderAdapterDescriptor {
	return []domain.ProviderAdapterDescriptor{
		{
			Provider:    "anthropic",
			Harness:     domain.HarnessClaudeCode,
			DisplayName: "Claude Code",
			Capabilities: []domain.ProviderCapability{
				domain.CapabilityPlanner,
				domain.CapabilityWorker,
				domain.CapabilityReviewer,
				domain.CapabilityDecisionResolver,
				domain.CapabilityUsageTelemetry,
				domain.CapabilityCapacityTelemetry,
			},
			// AO never implements OAuth itself for CLI-managed providers; it
			// shells out to the CLI's own interactive login under the
			// owning user's runtime-home and then probes local auth state.
			AuthMethods: []domain.ProviderAuthMethod{domain.AuthMethodCLIBootstrap},
			Models:      []string{"sonnet", "opus", "haiku"},
			Available:   true,
		},
		{
			Provider:    "openai",
			Harness:     domain.HarnessCodex,
			DisplayName: "Codex",
			// CapabilityPlanner is deliberately absent (Checkpoint 8P-C §21
			// audit): the only wired Planner implementation
			// (adapters/planner/command.Planner, see daemon/workflow_wiring.go)
			// unconditionally shells out to the `claude` CLI -- there is no
			// per-harness/Codex planner adapter today, so Codex must never be
			// selectable for the planner role until one exists.
			Capabilities: []domain.ProviderCapability{
				domain.CapabilityWorker,
				domain.CapabilityReviewer,
				domain.CapabilityDecisionResolver,
				domain.CapabilityUsageTelemetry,
				domain.CapabilityCapacityTelemetry,
			},
			AuthMethods: []domain.ProviderAuthMethod{domain.AuthMethodCLIBootstrap},
			Models: []string{
				"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna",
				"gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex",
			},
			Available: true,
		},
		{
			// Checkpoint 8P-B audit: no MiniMax adapter exists anywhere in
			// this repo (no entry in adapters/agent/registry.Constructors,
			// no reviewer/planner wiring, no model catalog entry). Listed
			// here only so the frontend can render it honestly as
			// unsupported instead of omitting it silently or fabricating a
			// working connection. To make this real: add a MiniMax
			// adapters/agent plugin (Manifest/GetLaunchCommand/AuthStatus),
			// a reviewer/planner wiring if applicable, and a modelcatalog
			// entry, then flip Available to true with real capabilities.
			Provider:     "minimax",
			Harness:      domain.AgentHarness("minimax"),
			DisplayName:  "MiniMax",
			Capabilities: nil,
			AuthMethods:  []domain.ProviderAuthMethod{domain.AuthMethodUnsupported},
			Models:       nil,
			Available:    false,
			Unavailable:  "no MiniMax adapter is implemented in this codebase yet",
		},
	}
}
