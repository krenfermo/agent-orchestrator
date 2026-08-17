package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// fakeResolverAgent is a hand-rolled fake ports.Agent that records what
// LaunchConfig it was called with and returns a fixed argv, mirroring
// workflow_reviewer_launcher_test.go's fakeReviewerAdapter pattern — this
// proves decisionResolverLauncher builds and passes through the tool
// allowlist/sandbox args itself, without needing a real claude/codex binary
// on PATH in CI.
type fakeResolverAgent struct {
	lastCfg ports.LaunchConfig
	argv    []string
	err     error
}

func (f *fakeResolverAgent) GetConfigSpec(context.Context) (ports.ConfigSpec, error) {
	return ports.ConfigSpec{}, nil
}

func (f *fakeResolverAgent) GetLaunchCommand(_ context.Context, cfg ports.LaunchConfig) ([]string, error) {
	f.lastCfg = cfg
	if f.err != nil {
		return nil, f.err
	}
	return f.argv, nil
}

func (f *fakeResolverAgent) GetPromptDeliveryStrategy(context.Context, ports.LaunchConfig) (ports.PromptDeliveryStrategy, error) {
	return ports.PromptDeliveryInCommand, nil
}
func (f *fakeResolverAgent) GetAgentHooks(context.Context, ports.WorkspaceHookConfig) error {
	return nil
}
func (f *fakeResolverAgent) GetRestoreCommand(context.Context, ports.RestoreConfig) ([]string, bool, error) {
	return nil, false, nil
}
func (f *fakeResolverAgent) SessionInfo(context.Context, ports.SessionRef) (ports.SessionInfo, bool, error) {
	return ports.SessionInfo{}, false, nil
}

type fakeResolverAgentResolver struct {
	byHarness map[domain.AgentHarness]*fakeResolverAgent
}

func (f *fakeResolverAgentResolver) Agent(harness domain.AgentHarness) (ports.Agent, bool) {
	a, ok := f.byHarness[harness]
	if !ok {
		return nil, false
	}
	return a, true
}

type fakeResolverRuntime struct {
	lastCfg ports.RuntimeConfig
	calls   int
}

func (f *fakeResolverRuntime) Create(_ context.Context, cfg ports.RuntimeConfig) (ports.RuntimeHandle, error) {
	f.calls++
	f.lastCfg = cfg
	return ports.RuntimeHandle{ID: "resolver-pane-1"}, nil
}

// TestDecisionResolverLauncher_ClaudeToolAllowlist asserts the real
// enforcement mechanism for a Claude Code resolver session: the exact tool
// allowlist/denylist this launcher builds is what reaches the agent
// adapter's GetLaunchCommand (the same mechanism that, for a real Claude
// Code launch, becomes --allowedTools/--disallowedTools flags — see
// claudecode.Plugin.GetLaunchCommand, which forwards these fields
// unmodified into agentruntime.BuildLaunchCommand).
func TestDecisionResolverLauncher_ClaudeToolAllowlist(t *testing.T) {
	claude := &fakeResolverAgent{argv: []string{"claude", "--some-flag"}}
	l := &decisionResolverLauncher{
		agents:  &fakeResolverAgentResolver{byHarness: map[domain.AgentHarness]*fakeResolverAgent{domain.HarnessClaudeCode: claude}},
		runtime: &fakeResolverRuntime{},
		dataDir: t.TempDir(),
	}

	req := workflowcore.DecisionResolverLaunchRequest{
		Harness:           domain.HarnessClaudeCode,
		ResolverSessionID: "decision-resolver-wqr-1",
		WorkspacePath:     "/ws/wf",
		Prompt:            "resolve this question read-only",
	}
	if _, err := l.Launch(context.Background(), req); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	cfg := claude.lastCfg
	if cfg.Permissions != ports.PermissionModeAuto {
		t.Fatalf("Permissions = %v, want PermissionModeAuto (never bypassPermissions)", cfg.Permissions)
	}
	assertContainsExactlyOnce(t, cfg.AllowedTools, "Bash(ao decision resolve:*)")
	for _, denied := range []string{"Edit", "Write", "NotebookEdit", "Bash(git push:*)", "Bash(git commit:*)"} {
		assertContainsExactlyOnce(t, cfg.DisallowedTools, denied)
	}
	for _, forbidden := range []string{"Edit", "Write", "Bash(git push:*)", "Bash(git commit:*)"} {
		for _, allowed := range cfg.AllowedTools {
			if allowed == forbidden {
				t.Fatalf("AllowedTools unexpectedly includes a write-shaped tool: %q (full list %v)", forbidden, cfg.AllowedTools)
			}
		}
	}
	// The resolver's own review-submission command must be the ONLY
	// write-shaped allowlist entry — no `ao review submit`, no gh, no
	// unrestricted Bash.
	for _, allowed := range cfg.AllowedTools {
		if allowed == "Bash(ao review submit:*)" || allowed == "Bash(gh:*)" || allowed == "Bash" {
			t.Fatalf("AllowedTools unexpectedly includes reviewer/gh/unrestricted-bash entry: %q", allowed)
		}
	}
}

// TestDecisionResolverLauncher_CodexReadOnlySandbox asserts the real
// OS-level read-only sandbox flag reaches the built argv for a Codex
// resolver session, mirroring adapters/reviewer/codex.codexReadOnlyArgs.
func TestDecisionResolverLauncher_CodexReadOnlySandbox(t *testing.T) {
	codexAgent := &fakeResolverAgent{argv: []string{"codex", "exec", "--", "the prompt"}}
	l := &decisionResolverLauncher{
		agents:  &fakeResolverAgentResolver{byHarness: map[domain.AgentHarness]*fakeResolverAgent{domain.HarnessCodex: codexAgent}},
		runtime: &fakeResolverRuntime{},
		dataDir: t.TempDir(),
	}

	req := workflowcore.DecisionResolverLaunchRequest{
		Harness:           domain.HarnessCodex,
		ResolverSessionID: "decision-resolver-wqr-2",
		WorkspacePath:     "/ws/wf",
		Prompt:            "resolve this question read-only",
	}
	result, err := l.Launch(context.Background(), req)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if result.HandleID != "resolver-pane-1" {
		t.Fatalf("HandleID = %q", result.HandleID)
	}

	runtime := l.runtime.(*fakeResolverRuntime)
	argv := runtime.lastCfg.Argv
	found := false
	for i, a := range argv {
		if a == "--sandbox" && i+1 < len(argv) && argv[i+1] == "read-only" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("codex resolver argv = %v, want it to include --sandbox read-only", argv)
	}
	// The extra flags must land BEFORE the "--" prompt delimiter, exactly
	// like the reviewer adapter's own insertBeforePrompt.
	ddIdx, sandboxIdx := -1, -1
	for i, a := range argv {
		if a == "--" && ddIdx == -1 {
			ddIdx = i
		}
		if a == "--sandbox" {
			sandboxIdx = i
		}
	}
	if ddIdx == -1 || sandboxIdx == -1 || sandboxIdx > ddIdx {
		t.Fatalf("argv = %v, want --sandbox before the -- prompt delimiter", argv)
	}
}

// TestDecisionResolverLauncher_RuntimeSessionIDMatchesResolverIdentity
// asserts the runtime pane is created under the SAME deterministic
// ResolverSessionID baked into the prompt by the coordinator — the resolver
// never derives its own identity.
func TestDecisionResolverLauncher_RuntimeSessionIDMatchesResolverIdentity(t *testing.T) {
	claude := &fakeResolverAgent{argv: []string{"claude"}}
	rt := &fakeResolverRuntime{}
	l := &decisionResolverLauncher{
		agents:  &fakeResolverAgentResolver{byHarness: map[domain.AgentHarness]*fakeResolverAgent{domain.HarnessClaudeCode: claude}},
		runtime: rt,
		dataDir: t.TempDir(),
	}
	req := workflowcore.DecisionResolverLaunchRequest{
		Harness:           domain.HarnessClaudeCode,
		ResolverSessionID: "decision-resolver-wqr-3",
		WorkspacePath:     "/ws/wf",
		Prompt:            "resolve",
	}
	if _, err := l.Launch(context.Background(), req); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if rt.lastCfg.SessionID != domain.SessionID("decision-resolver-wqr-3") {
		t.Fatalf("runtime SessionID = %q, want the deterministic resolver session id", rt.lastCfg.SessionID)
	}
}

// TestDecisionResolverLauncher_ShimsAOWhenNotNamedAO covers the same
// PATH/shim-pinning contract workflowReviewerLauncher already proves,
// confirming EnsureAOShim also covers the resolver's spawned session so a
// bare `ao decision resolve` in the prompt resolves.
func TestDecisionResolverLauncher_ShimsAOWhenNotNamedAO(t *testing.T) {
	claude := &fakeResolverAgent{argv: []string{"claude"}}
	rt := &fakeResolverRuntime{}
	dataDir := t.TempDir()
	exe := filepath.Join(t.TempDir(), "ao-e2e-runtime-test")
	l := &decisionResolverLauncher{
		agents:     &fakeResolverAgentResolver{byHarness: map[domain.AgentHarness]*fakeResolverAgent{domain.HarnessClaudeCode: claude}},
		runtime:    rt,
		dataDir:    dataDir,
		executable: func() (string, error) { return exe, nil },
	}
	req := workflowcore.DecisionResolverLaunchRequest{
		Harness:           domain.HarnessClaudeCode,
		ResolverSessionID: "decision-resolver-wqr-4",
		WorkspacePath:     "/ws/wf",
		Prompt:            "resolve",
	}
	if _, err := l.Launch(context.Background(), req); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	shimDir := filepath.Join(dataDir, "reviewer-runtime", "bin")
	path := rt.lastCfg.Env["PATH"]
	if len(path) == 0 {
		t.Fatalf("empty PATH")
	}
	if path[:len(shimDir)] != shimDir {
		t.Fatalf("PATH = %q, want the AO shim dir first (%q)", path, shimDir)
	}
}

func assertContainsExactlyOnce(t *testing.T, list []string, want string) {
	t.Helper()
	count := 0
	for _, v := range list {
		if v == want {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("list %v contains %q %d times, want exactly 1", list, want, count)
	}
}
