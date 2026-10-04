package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/hooksjson"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/hookutil"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// These tests cover the reported failure directly: Claude Code runs a Stop hook
// through a bare `/bin/sh -c`, which reads no ~/.zshrc, ~/.bashrc, profile, or
// alias and therefore resolves commands only through the PATH it inherits. When
// the hook was a bare `ao hooks claude-code stop` and AO was not on that PATH,
// every turn ended with "Failed with non-blocking status code: /bin/sh: ao:
// command not found".

// fakeAOBinary writes a stand-in AO binary that records its argv, and returns
// its path plus the record file.
func fakeAOBinary(t *testing.T, dir string) (exe, record string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	exe = filepath.Join(dir, "ao-daemon")
	record = filepath.Join(dir, "argv.txt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + hookutil.ShellQuote(record) + "\nexit 0\n"
	if err := os.WriteFile(exe, []byte(script), 0o700); err != nil { // #nosec G306 -- test fixture must be executable
		t.Fatal(err)
	}
	return exe, record
}

// pinClaudeHookLauncher points the installed hook commands at a fake AO binary
// for the duration of one test.
func pinClaudeHookLauncher(t *testing.T, exe string) {
	t.Helper()
	original := claudeHooks.Launcher
	claudeHooks.Launcher = func(dataDir string) (string, error) {
		return hookutil.EnsureLauncherFor(dataDir, func() (string, error) { return exe, nil })
	}
	t.Cleanup(func() { claudeHooks.Launcher = original })
}

func installedClaudeHookCommand(t *testing.T, workspace, event string) string {
	t.Helper()
	data, err := os.ReadFile(claudeSettingsPath(workspace)) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Hooks map[string][]hooksjson.MatcherGroup `json:"hooks"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("parse settings.local.json: %v\n%s", err, data)
	}
	for _, group := range config.Hooks[event] {
		for _, hook := range group.Hooks {
			if claudeHooks.Matches(hook.Command, claudeHookCommandPrefix+"stop") {
				return hook.Command
			}
		}
	}
	t.Fatalf("no AO %s hook installed:\n%s", event, data)
	return ""
}

func TestStopHookRunsWhenAOIsAbsentFromShellPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("/bin/sh hook execution is Unix-only")
	}
	root := t.TempDir()
	exe, record := fakeAOBinary(t, filepath.Join(root, "install"))
	pinClaudeHookLauncher(t, exe)

	workspace := filepath.Join(root, "workspace")
	dataDir := filepath.Join(root, "data")
	if err := (&Plugin{}).GetAgentHooks(context.Background(), ports.WorkspaceHookConfig{
		WorkspacePath: workspace,
		DataDir:       dataDir,
	}); err != nil {
		t.Fatalf("GetAgentHooks: %v", err)
	}

	command := installedClaudeHookCommand(t, workspace, "Stop")
	if strings.HasPrefix(command, "ao ") {
		t.Fatalf("Stop hook still invokes a bare `ao`: %q", command)
	}

	// Exactly how Claude Code invokes it, with an environment that cannot
	// resolve `ao` by name.
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Env = []string{"PATH="}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Stop hook failed with no AO on PATH: %v\n%s", err, out)
	}
	got, err := os.ReadFile(record) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatalf("Stop hook never reached the AO binary: %v", err)
	}
	if strings.TrimSpace(string(got)) != "hooks claude-code stop" {
		t.Fatalf("AO received %q, want %q", strings.TrimSpace(string(got)), "hooks claude-code stop")
	}
}

func TestStopHookHandlesWorkspaceAndDataPathsWithSpaces(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("/bin/sh hook execution is Unix-only")
	}
	root := t.TempDir()
	exe, record := fakeAOBinary(t, filepath.Join(root, "Agent Orchestrator.app", "Contents", "MacOS"))
	pinClaudeHookLauncher(t, exe)

	workspace := filepath.Join(root, "my projects", "agent orchestrator")
	dataDir := filepath.Join(root, "ao data")
	if err := (&Plugin{}).GetAgentHooks(context.Background(), ports.WorkspaceHookConfig{
		WorkspacePath: workspace,
		DataDir:       dataDir,
	}); err != nil {
		t.Fatalf("GetAgentHooks: %v", err)
	}

	command := installedClaudeHookCommand(t, workspace, "Stop")
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Env = []string{"PATH="}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Stop hook failed for paths with spaces: %v\ncommand: %s\n%s", err, command, out)
	}
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("Stop hook never reached the AO binary: %v", err)
	}
}

func TestInstalledHookCommandIsDeterministic(t *testing.T) {
	root := t.TempDir()
	exe, _ := fakeAOBinary(t, filepath.Join(root, "install"))
	pinClaudeHookLauncher(t, exe)

	// Nothing about the caller's shell may influence the resolved command.
	t.Setenv("PATH", "")
	t.Setenv("SHELL", "/nonexistent/shell")

	workspace := filepath.Join(root, "workspace")
	dataDir := filepath.Join(root, "data")
	want := hookutil.ShellQuote(filepath.Join(dataDir, hookutil.LauncherDirName, hookutil.LauncherName())) +
		" hooks claude-code stop"

	for i := 0; i < 3; i++ {
		if err := (&Plugin{}).GetAgentHooks(context.Background(), ports.WorkspaceHookConfig{
			WorkspacePath: workspace,
			DataDir:       dataDir,
		}); err != nil {
			t.Fatalf("GetAgentHooks #%d: %v", i, err)
		}
		got := installedClaudeHookCommand(t, workspace, "Stop")
		if got != want {
			t.Fatalf("Stop command #%d = %q, want %q", i, got, want)
		}
	}
}

// TestInstallReplacesLegacyBareAOHook covers the upgrade path: a workspace
// written by an older AO carries the broken bare-`ao` command, and reinstalling
// must replace it in place rather than leave it alongside the working one.
func TestInstallReplacesLegacyBareAOHook(t *testing.T) {
	root := t.TempDir()
	exe, _ := fakeAOBinary(t, filepath.Join(root, "install"))
	pinClaudeHookLauncher(t, exe)

	workspace := filepath.Join(root, "workspace")
	settings := claudeSettingsPath(workspace)
	if err := os.MkdirAll(filepath.Dir(settings), 0o750); err != nil {
		t.Fatal(err)
	}
	legacy := `{"hooks":{"Stop":[{"hooks":[` +
		`{"type":"command","command":"ao hooks claude-code stop","timeout":30},` +
		`{"type":"command","command":"my own stop hook","timeout":5}]}]}}`
	if err := os.WriteFile(settings, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := (&Plugin{}).GetAgentHooks(context.Background(), ports.WorkspaceHookConfig{
		WorkspacePath: workspace,
		DataDir:       filepath.Join(root, "data"),
	}); err != nil {
		t.Fatalf("GetAgentHooks: %v", err)
	}

	data, err := os.ReadFile(settings) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if strings.Contains(body, `"ao hooks claude-code stop"`) {
		t.Fatalf("legacy bare-`ao` Stop hook survived the install:\n%s", body)
	}
	if n := strings.Count(body, "hooks claude-code stop"); n != 1 {
		t.Fatalf("AO Stop hook appears %d times, want exactly 1:\n%s", n, body)
	}
	if !strings.Contains(body, "my own stop hook") {
		t.Fatalf("user Stop hook dropped:\n%s", body)
	}

	// The legacy form must still be recognized for uninstall.
	if err := (&Plugin{}).UninstallHooks(context.Background(), workspace); err != nil {
		t.Fatalf("UninstallHooks: %v", err)
	}
	data, err = os.ReadFile(settings) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "hooks claude-code") {
		t.Fatalf("AO hooks survived uninstall:\n%s", data)
	}
	if !strings.Contains(string(data), "my own stop hook") {
		t.Fatalf("user Stop hook dropped by uninstall:\n%s", data)
	}
}
