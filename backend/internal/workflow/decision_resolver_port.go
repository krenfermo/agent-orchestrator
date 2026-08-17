package workflow

import (
	stdctx "context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// DecisionResolverLaunchRequest is workflow's request to spawn a read-only
// resolver session for one workflow_question_resolutions attempt
// (Checkpoint 8K-B, pass 2). Mirrors ReviewerLaunchRequest's shape closely —
// same loopback-HTTP/no-ambient-identity design: the resolver session id and
// the resolution run id are both baked into Prompt by the caller
// (BuildDecisionResolverPrompt), never derived by the resolver itself.
type DecisionResolverLaunchRequest struct {
	Harness         domain.AgentHarness
	AskingSessionID domain.SessionID
	ProjectID       domain.ProjectID
	ResolutionID    string // the workflow_question_resolutions.id row
	// ResolverSessionID is the deterministic identity minted by the
	// coordinator BEFORE launch and already baked into Prompt (see
	// BuildDecisionResolverPrompt) — the resolver never derives its own
	// identity, it only echoes this back as the `ao decision resolve`
	// positional arg. The launcher must use this exact value as the runtime
	// pane's own session identity (mirroring workflowReviewerLauncher's
	// handleID pattern), not invent a different one.
	ResolverSessionID domain.SessionID
	WorkspacePath     string
	Prompt            string
	SystemPrompt      string
}

// DecisionResolverLaunchResult is the runtime handle created for a resolver
// launch.
type DecisionResolverLaunchResult struct {
	HandleID string
}

// DecisionResolverLauncher is workflow's narrow resolver-launch port
// (Checkpoint 8K-B, pass 2), mirroring ReviewerLauncher's shape. The
// concrete implementation (wired from internal/daemon) spawns a genuinely
// read-only session — Codex under `--sandbox read-only`, Claude Code under a
// read-only tool allowlist whose only write-shaped action is the
// `ao decision resolve` shell invocation — reusing the exact mechanisms the
// reviewer adapters already use, never a new or more permissive one.
type DecisionResolverLauncher interface {
	// Preflight checks whether the resolver harness can actually be launched
	// (binary on PATH, etc.) without starting a pane, mirroring
	// ReviewerLauncher.Preflight.
	Preflight(ctx stdctx.Context, harness domain.AgentHarness, workspacePath string) error
	// Launch starts a fresh, read-only resolver pane for one resolution
	// attempt.
	Launch(ctx stdctx.Context, req DecisionResolverLaunchRequest) (DecisionResolverLaunchResult, error)
}
