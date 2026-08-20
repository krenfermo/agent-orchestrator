package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	workeragent "github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/claudecode"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// captureAgent is a stub ports.Agent that records the LaunchConfig the reviewer
// builds, so the test asserts the reviewer's tool policy without needing the
// real claude binary on PATH.
type captureAgent struct {
	got        ports.LaunchConfig
	gotRestore ports.RestoreConfig
	hooks      []ports.WorkspaceHookConfig
	prelaunch  []ports.LaunchConfig
}

func (a *captureAgent) GetConfigSpec(context.Context) (ports.ConfigSpec, error) {
	return ports.ConfigSpec{}, nil
}
func (a *captureAgent) GetLaunchCommand(_ context.Context, cfg ports.LaunchConfig) ([]string, error) {
	a.got = cfg
	return []string{"claude"}, nil
}
func (a *captureAgent) GetPromptDeliveryStrategy(context.Context, ports.LaunchConfig) (ports.PromptDeliveryStrategy, error) {
	return ports.PromptDeliveryInCommand, nil
}
func (a *captureAgent) GetAgentHooks(_ context.Context, cfg ports.WorkspaceHookConfig) error {
	a.hooks = append(a.hooks, cfg)
	return nil
}
func (a *captureAgent) PreLaunch(_ context.Context, cfg ports.LaunchConfig) error {
	a.prelaunch = append(a.prelaunch, cfg)
	return nil
}
func (a *captureAgent) GetRestoreCommand(_ context.Context, cfg ports.RestoreConfig) ([]string, bool, error) {
	a.gotRestore = cfg
	id := cfg.Session.Metadata[ports.MetadataKeyAgentSessionID]
	if id == "" {
		id = cfg.Session.ID
	}
	if id == "" {
		return nil, false, nil
	}
	return []string{"claude", "--resume", id}, true, nil
}
func (a *captureAgent) SessionInfo(context.Context, ports.SessionRef) (ports.SessionInfo, bool, error) {
	return ports.SessionInfo{}, false, nil
}

func TestReviewCommandLaunchesReadOnlyOffBypass(t *testing.T) {
	agent := &captureAgent{}
	r := &Reviewer{agent: agent}

	spec, err := r.ReviewCommand(context.Background(), ports.ReviewInvocation{
		ReviewerID:    "review-w1",
		WorkspacePath: "/ws/w1",
		Prompt:        "review it",
		SystemPrompt:  "you are a reviewer",
	})
	if err != nil {
		t.Fatalf("ReviewCommand: %v", err)
	}

	// The allowlist is what enforces read-only, so it must launch in an
	// explicit non-bypass mode: bypassPermissions ignores allow/deny rules
	// entirely, and an empty mode would defer to a user's defaultMode.
	if agent.got.Permissions != ports.PermissionModeAuto {
		t.Fatalf("reviewer must launch in auto permission mode; got %q", agent.got.Permissions)
	}
	if agent.got.SessionID == "" {
		t.Fatal("reviewer must pin the persisted Claude session id")
	}
	if spec.AgentSessionID != agent.got.SessionID {
		t.Fatalf("persisted agent session id = %q, launched session id = %q", spec.AgentSessionID, agent.got.SessionID)
	}
	if !contains(agent.got.AllowedTools, "Read") || !contains(agent.got.AllowedTools, "Bash(ao review submit:*)") {
		t.Fatalf("allowlist missing read-only review tools: %#v", agent.got.AllowedTools)
	}
	for _, denied := range []string{"Edit", "Write", "Bash(git push:*)", "Bash(git commit:*)"} {
		if !contains(agent.got.DisallowedTools, denied) {
			t.Fatalf("disallow list missing %q: %#v", denied, agent.got.DisallowedTools)
		}
	}
}

func TestPreLaunchInstallsSelectedReviewerHooksAndTrustsWorkspace(t *testing.T) {
	agent := &captureAgent{}
	r := &Reviewer{agent: agent}

	if err := r.PreLaunch(context.Background(), ports.ReviewInvocation{
		ReviewerID:    "review-w1",
		WorkspacePath: "/ws/w1",
	}); err != nil {
		t.Fatalf("PreLaunch: %v", err)
	}
	if len(agent.hooks) != 1 || agent.hooks[0].WorkspacePath != "/ws/w1" {
		t.Fatalf("hooks = %#v, want selected reviewer hook install for /ws/w1", agent.hooks)
	}
	if len(agent.prelaunch) != 1 || agent.prelaunch[0].WorkspacePath != "/ws/w1" || agent.prelaunch[0].SessionID == "" {
		t.Fatalf("prelaunch = %#v, want trusted workspace with pinned session id", agent.prelaunch)
	}
}

// TestPreLaunchForwardsIsolatedEnvToWorkerAdapter is Checkpoint 8P-E.3.1's
// regression: the reviewer's PreLaunch must forward ReviewInvocation.Env
// through to the underlying worker adapter's own PreLaunch unchanged, since
// that Env (when set) is what makes the worker adapter's
// resolveClaudeConfigPath (Checkpoint 8P-E.3) resolve the isolated
// CLAUDE_CONFIG_DIR instead of the daemon's own home directory.
func TestPreLaunchForwardsIsolatedEnvToWorkerAdapter(t *testing.T) {
	agent := &captureAgent{}
	r := &Reviewer{agent: agent}
	isolatedEnv := map[string]string{"CLAUDE_CONFIG_DIR": "/isolated/users/u1/providers/claude-code"}

	if err := r.PreLaunch(context.Background(), ports.ReviewInvocation{
		ReviewerID:    "review-w1",
		WorkspacePath: "/ws/w1",
		Env:           isolatedEnv,
	}); err != nil {
		t.Fatalf("PreLaunch: %v", err)
	}
	if len(agent.prelaunch) != 1 {
		t.Fatalf("prelaunch calls = %d, want 1", len(agent.prelaunch))
	}
	if agent.prelaunch[0].Env["CLAUDE_CONFIG_DIR"] != isolatedEnv["CLAUDE_CONFIG_DIR"] {
		t.Fatalf("worker adapter PreLaunch Env = %#v, want isolated CLAUDE_CONFIG_DIR forwarded", agent.prelaunch[0].Env)
	}
}

// TestPreLaunchWithIsolatedEnvTrustsWorkspaceWithoutMutatingHostConfig is
// Checkpoint 8P-E.3.1's end-to-end regression, exercising the real reviewer
// wrapper over the real worker adapter (no captureAgent stub): with an
// isolated CLAUDE_CONFIG_DIR set, the trust record must land there, and the
// daemon host's own ~/.claude.json (simulated via $HOME) must never be
// created or touched. Reproduces, for reviewers, the exact worker-side proof
// Checkpoint 8P-E.3 already established.
func TestPreLaunchWithIsolatedEnvTrustsWorkspaceWithoutMutatingHostConfig(t *testing.T) {
	hostHome := t.TempDir()
	t.Setenv("HOME", hostHome)
	isolatedConfigDir := filepath.Join(t.TempDir(), "providers", "claude-code")
	// Production always has this directory in place before PreLaunch runs
	// (runtimehome.Prepare creates it as part of the isolated runtime-home);
	// replicate that here rather than relying on PreLaunch to create it.
	if err := os.MkdirAll(isolatedConfigDir, 0o700); err != nil {
		t.Fatalf("mkdir isolated config dir: %v", err)
	}
	workspace := filepath.Join(t.TempDir(), "worktree")

	r := &Reviewer{agent: workeragent.New()}
	if err := r.PreLaunch(context.Background(), ports.ReviewInvocation{
		ReviewerID:    "review-w1",
		WorkspacePath: workspace,
		Env:           map[string]string{"CLAUDE_CONFIG_DIR": isolatedConfigDir},
	}); err != nil {
		t.Fatalf("PreLaunch: %v", err)
	}

	hostConfigPath := filepath.Join(hostHome, ".claude.json")
	if _, err := os.Stat(hostConfigPath); !os.IsNotExist(err) {
		t.Fatalf("host ~/.claude.json must not be created/mutated, stat err = %v", err)
	}

	isolatedConfigPath := filepath.Join(isolatedConfigDir, ".claude.json")
	raw, err := os.ReadFile(isolatedConfigPath)
	if err != nil {
		t.Fatalf("read isolated config: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("unmarshal isolated config: %v", err)
	}
	projects, _ := root["projects"].(map[string]any)
	entry, _ := projects[workspace].(map[string]any)
	if entry["hasTrustDialogAccepted"] != true {
		t.Fatalf("isolated config trust entry = %#v, want hasTrustDialogAccepted=true for %s", entry, workspace)
	}
}

func TestAllowlistCoversPromptRequiredPipedCommands(t *testing.T) {
	agent := &captureAgent{}
	r := &Reviewer{agent: agent}

	if _, err := r.ReviewCommand(context.Background(), ports.ReviewInvocation{
		ReviewerID:    "review-w1",
		WorkspacePath: "/ws/w1",
		Prompt:        "review it",
		SystemPrompt:  "you are a reviewer",
	}); err != nil {
		t.Fatalf("ReviewCommand: %v", err)
	}

	if !contains(agent.got.AllowedTools, "Bash(printf:*)") {
		t.Fatalf("allowlist missing printf for piped review commands: %#v", agent.got.AllowedTools)
	}

	for _, cmd := range []string{
		"printf '%s' '{ \"event\": \"COMMENT\", \"body\": \"x\" }' | gh api --method POST repos/o/r/pulls/1/reviews --input - --jq '.id'",
		"printf '%s' '{ \"reviews\": [] }' | ao review submit --session sess-1 --reviews -",
	} {
		if !compoundCommandCovered(agent.got.AllowedTools, cmd) {
			t.Fatalf("allowlist does not cover prompt-required command %q with tools %#v", cmd, agent.got.AllowedTools)
		}
	}

	disallowed := "printf x | rm -rf /"
	if compoundCommandCovered(agent.got.AllowedTools, disallowed) {
		t.Fatalf("allowlist unexpectedly covers disallowed command %q with tools %#v", disallowed, agent.got.AllowedTools)
	}
}

func TestReviewCommandUsesHiddenSystemPromptFile(t *testing.T) {
	agent := &captureAgent{}
	r := &Reviewer{agent: agent}

	if _, err := r.ReviewCommand(context.Background(), ports.ReviewInvocation{
		Prompt:           "Start the AO review task.",
		SystemPromptFile: "/ao/prompts/reviewer/system.md",
	}); err != nil {
		t.Fatalf("ReviewCommand: %v", err)
	}
	if agent.got.Prompt != "Start the AO review task." || agent.got.SystemPrompt != "" || agent.got.SystemPromptFile != "/ao/prompts/reviewer/system.md" {
		t.Fatalf("launch config = %+v", agent.got)
	}
}

func TestReviewRestoreCommandUsesNativeSessionIDAndReadOnlyPolicy(t *testing.T) {
	agent := &captureAgent{}
	r := &Reviewer{agent: agent}

	got, ok, err := r.ReviewRestoreCommand(context.Background(), ports.ReviewInvocation{
		ReviewerID:       "review-w1",
		AgentSessionID:   "claude-native-1",
		WorkspacePath:    "/ws/w1",
		SystemPromptFile: "/ao/prompts/reviewer/system.md",
	})
	if err != nil {
		t.Fatalf("ReviewRestoreCommand: %v", err)
	}
	if !ok {
		t.Fatal("ReviewRestoreCommand ok = false, want true")
	}
	if strings.Join(got.Argv, " ") != "claude --resume claude-native-1" {
		t.Fatalf("argv = %#v", got.Argv)
	}
	if agent.gotRestore.Session.Metadata[ports.MetadataKeyAgentSessionID] != "claude-native-1" {
		t.Fatalf("restore metadata = %#v", agent.gotRestore.Session.Metadata)
	}
	if agent.gotRestore.Permissions != ports.PermissionModeAuto {
		t.Fatalf("restore permissions = %q, want auto", agent.gotRestore.Permissions)
	}
	if !contains(agent.gotRestore.AllowedTools, "Read") || !contains(agent.gotRestore.DisallowedTools, "Write") {
		t.Fatalf("restore tool policy allowed=%#v disallowed=%#v", agent.gotRestore.AllowedTools, agent.gotRestore.DisallowedTools)
	}
}

func TestReviewRestoreCommandAllowsAdapterFallbackWithoutNativeSessionID(t *testing.T) {
	agent := &captureAgent{}
	r := &Reviewer{agent: agent}

	got, ok, err := r.ReviewRestoreCommand(context.Background(), ports.ReviewInvocation{
		ReviewerID:       "review-w1",
		WorkspacePath:    "/ws/w1",
		SystemPromptFile: "/ao/prompts/reviewer/system.md",
	})
	if err != nil {
		t.Fatalf("ReviewRestoreCommand: %v", err)
	}
	if !ok {
		t.Fatal("ReviewRestoreCommand ok = false, want true")
	}
	if strings.Join(got.Argv, " ") != "claude --resume review-w1" {
		t.Fatalf("argv = %#v", got.Argv)
	}
	if agent.gotRestore.Session.ID != "review-w1" {
		t.Fatalf("restore session id = %q, want review-w1", agent.gotRestore.Session.ID)
	}
	if _, ok := agent.gotRestore.Session.Metadata[ports.MetadataKeyAgentSessionID]; ok {
		t.Fatalf("restore metadata should not invent native id: %#v", agent.gotRestore.Session.Metadata)
	}
}

func TestReviewCancelSendsDoubleEscapeInput(t *testing.T) {
	spec, err := (&Reviewer{}).ReviewCancel(context.Background())
	if err != nil {
		t.Fatalf("ReviewCancel: %v", err)
	}
	if spec.Mode != ports.ReviewCancelInput {
		t.Fatalf("cancel mode = %q, want %q", spec.Mode, ports.ReviewCancelInput)
	}
	if len(spec.Inputs) != 2 || spec.Inputs[0] != "\x1b" || spec.Inputs[1] != "\x1b" {
		t.Fatalf("inputs = %#v, want double escape", spec.Inputs)
	}
	if spec.InputDelay != 150*time.Millisecond {
		t.Fatalf("input delay = %s, want 150ms", spec.InputDelay)
	}
}

func compoundCommandCovered(allowedTools []string, cmd string) bool {
	for _, segment := range splitPipedCommand(cmd) {
		if !bashSegmentCovered(allowedTools, segment) {
			return false
		}
	}
	return true
}

func splitPipedCommand(cmd string) []string {
	rawSegments := strings.Split(cmd, "|")
	segments := make([]string, 0, len(rawSegments))
	for _, segment := range rawSegments {
		if trimmed := strings.TrimSpace(segment); trimmed != "" {
			segments = append(segments, trimmed)
		}
	}
	return segments
}

func bashSegmentCovered(allowedTools []string, segment string) bool {
	for _, tool := range allowedTools {
		cmd, ok := strings.CutPrefix(tool, "Bash(")
		if !ok {
			continue
		}
		cmd, ok = strings.CutSuffix(cmd, ":*)")
		if !ok {
			continue
		}
		if strings.HasPrefix(segment, cmd) {
			return true
		}
	}
	return false
}

func contains(values []string, needle string) bool {
	for _, v := range values {
		if v == needle {
			return true
		}
	}
	return false
}
