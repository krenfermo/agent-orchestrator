package sessionmanager

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type fixedBrowserCapability string

func (f fixedBrowserCapability) Issue(_ domain.SessionID) (string, string, error) {
	return string(f), "verifier-1", nil
}

func TestSpawnEnvProjectVarsCannotOverrideInternal(t *testing.T) {
	env := spawnEnv("mer-1", "mer", "issue-9", "/data", "/data/running.json", map[string]string{
		"FOO":        "bar",
		EnvSessionID: "hacked", // a project must not override AO-internal vars
		EnvProjectID: "hacked",
	})
	if env["FOO"] != "bar" {
		t.Fatalf("FOO = %q, want bar", env["FOO"])
	}
	if env[EnvSessionID] != "mer-1" {
		t.Fatalf("AO_SESSION_ID = %q, want mer-1 (internal wins)", env[EnvSessionID])
	}
	if env[EnvProjectID] != "mer" {
		t.Fatalf("AO_PROJECT_ID = %q, want mer (internal wins)", env[EnvProjectID])
	}
}

// TestSpawnEnvExportsRunFilePath is 8P-D.2's regression test for the exact
// sequence that produced a false-early verify_workspace_changed in a real
// autonomous run: the daemon was started with an AO_RUN_FILE override that
// didn't live under AO_DATA_DIR, spawnEnv exported only AO_DATA_DIR, so every
// `ao hooks claude-code ...` callback from inside the worker session hit
// "AO daemon is not running" (resolveRunFilePath falls back to the default
// ~/.ao/running.json, which the running daemon never wrote) — activity state
// never left its idle default, and the coordinator's git-status fallback
// (evaluateWorkStepProgress) called the work step complete while Claude Code
// was still mid-task, freezing an incomplete reviewed fingerprint. Exporting
// AO_RUN_FILE removes the guesswork the hook subprocess otherwise has to do.
func TestSpawnEnvExportsRunFilePath(t *testing.T) {
	env := spawnEnv("mer-1", "mer", "issue-9", "/data", "/private/tmp/ao-manual-test/data/running.json", nil)
	if got := env[EnvRunFile]; got != "/private/tmp/ao-manual-test/data/running.json" {
		t.Fatalf("AO_RUN_FILE = %q, want the daemon's resolved run-file path", got)
	}
}

// TestSpawnEnvOmitsRunFileWhenUnset preserves pre-8P-D.2 behavior when the
// daemon wiring leaves RunFilePath unset (e.g. an older test double): no
// empty AO_RUN_FILE is exported, so a spawned agent falls back to its own
// default resolution instead of being pointed at an empty path.
func TestSpawnEnvOmitsRunFileWhenUnset(t *testing.T) {
	env := spawnEnv("mer-1", "mer", "issue-9", "/data", "", nil)
	if _, ok := env[EnvRunFile]; ok {
		t.Fatalf("AO_RUN_FILE should be absent when runFilePath is empty, got %q", env[EnvRunFile])
	}
}

func TestRuntimeEnvInjectsBrowserCapability(t *testing.T) {
	manager := &Manager{
		dataDir:             "/data",
		runFilePath:         "/data/running.json",
		browserCapabilities: fixedBrowserCapability("capability-1"),
		executable:          func() (string, error) { return filepath.Join("/opt", "aod", "ao"), nil },
		logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	env, verifier, err := manager.launchRuntimeEnv("mer-1", "mer", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if env[EnvBrowserCapability] != "capability-1" {
		t.Fatalf("%s = %q", EnvBrowserCapability, env[EnvBrowserCapability])
	}
	if verifier != "verifier-1" {
		t.Fatalf("verifier = %q", verifier)
	}
	if env[EnvRunFile] != "/data/running.json" {
		t.Fatalf("%s = %q, want the manager's runFilePath threaded through runtimeEnv", EnvRunFile, env[EnvRunFile])
	}
}

func TestRuntimeEnvClearsDaemonBrowserRuntimeSecrets(t *testing.T) {
	manager := &Manager{
		dataDir:    "/data",
		executable: func() (string, error) { return filepath.Join("/opt", "aod", "ao"), nil },
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	env := manager.runtimeEnv("mer-1", "mer", "", map[string]string{
		EnvBrowserRuntimeToken:      "runtime-secret",
		EnvBrowserRuntimeTokenStdin: "1",
	})
	if env[EnvBrowserRuntimeToken] != "" || env[EnvBrowserRuntimeTokenStdin] != "" {
		t.Fatalf("daemon browser runtime credentials leaked to worker: token=%q stdin=%q", env[EnvBrowserRuntimeToken], env[EnvBrowserRuntimeTokenStdin])
	}
}

// TestRuntimeEnvAppliesAOShimForRenamedBinary is Checkpoint 8M §16's
// regression test for the gap 8L.1's real E2E discovered: a worker session
// whose daemon binary is not literally named "ao" (HookPATH's pin only
// works for that exact name) used to leave PATH degraded — every worker
// hook callback (`ao hooks ...`) resolved to "command not found". Mirrors
// the exact fallback workflow_reviewer_launcher.go/decision_resolver_launcher.go
// already apply for their own launches.
func TestRuntimeEnvAppliesAOShimForRenamedBinary(t *testing.T) {
	dataDir := t.TempDir()
	renamed := filepath.Join(dataDir, "renamed-daemon-binary")
	manager := &Manager{
		dataDir:    dataDir,
		executable: func() (string, error) { return renamed, nil },
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	env := manager.runtimeEnv("mer-1", "mer", "", nil)

	shimDir := filepath.Join(dataDir, "reviewer-runtime", "bin")
	if !strings.Contains(env["PATH"], shimDir) {
		t.Fatalf("PATH = %q, want it to include the AO shim directory %q", env["PATH"], shimDir)
	}
	shimPath := filepath.Join(shimDir, "ao")
	if runtime.GOOS == "windows" {
		shimPath += ".cmd"
	}
	if _, err := os.Stat(shimPath); err != nil {
		t.Fatalf("AO shim not created at %q: %v", shimPath, err)
	}
}

func TestHookPATH(t *testing.T) {
	sep := string(os.PathListSeparator)
	daemonExe := filepath.Join("/opt", "aod", "ao")
	daemonDir := filepath.Dir(daemonExe)
	exeOK := func() (string, error) { return daemonExe, nil }

	cases := []struct {
		name       string
		executable func() (string, error)
		daemonPATH string
		projectEnv map[string]string
		want       string
		wantPinned bool
		wantErr    bool
	}{
		{
			name:       "prepends daemon dir to inherited PATH",
			executable: exeOK,
			daemonPATH: "/usr/bin" + sep + "/bin",
			want:       daemonDir + sep + "/usr/bin" + sep + "/bin",
			wantPinned: true,
		},
		{
			name:       "project PATH override is the base",
			executable: exeOK,
			daemonPATH: "/usr/bin",
			projectEnv: map[string]string{"PATH": "/proj/bin"},
			want:       daemonDir + sep + "/proj/bin" + sep + "/usr/bin" + sep + "/bin",
			wantPinned: true,
		},
		{
			// Required system dirs are appended even when the base PATH was
			// empty, so a caller building PATH from scratch never ends up with
			// only the daemon's own directory (Checkpoint 8I.2).
			name:       "empty base PATH still yields required system dirs",
			executable: exeOK,
			want:       daemonDir + sep + "/usr/bin" + sep + "/bin",
			wantPinned: true,
		},
		{
			name:       "unresolvable executable fails",
			executable: func() (string, error) { return "", errors.New("no exe") },
			daemonPATH: "/usr/bin",
			wantErr:    true,
		},
		{
			// A daemon binary not named "ao" cannot anchor `ao` resolution by
			// having its directory prepended, so the pin is reported as not
			// applied (pinned=false) — but PATH itself must stay fully usable
			// (base PATH plus required system dirs), never collapse to nothing.
			// This is the exact fragility Checkpoint 8I.1 hit: HookPATH used to
			// hard-error here, and callers degraded PATH down to a single
			// narrow directory, breaking the reviewer agent CLI's own auth
			// lookup even though credentials were fine.
			name:       "executable not named ao is not pinned but PATH stays usable",
			executable: func() (string, error) { return filepath.Join("/opt", "aod", "ao-daemon"), nil },
			daemonPATH: "/usr/bin",
			want:       filepath.Join("/opt", "aod") + sep + "/usr/bin" + sep + "/bin",
			wantPinned: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(key string) string {
				if key == "PATH" {
					return tc.daemonPATH
				}
				return ""
			}
			got, pinned, err := HookPATH(tc.executable, getenv, tc.projectEnv)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("HookPATH = %q, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("HookPATH: %v", err)
			}
			if got != tc.want {
				t.Fatalf("HookPATH = %q, want %q", got, tc.want)
			}
			if pinned != tc.wantPinned {
				t.Fatalf("HookPATH pinned = %v, want %v", pinned, tc.wantPinned)
			}
		})
	}
}

// TestHookPATH_PreservesInheritedAgentDirs is a proxy for "real agent CLIs
// (Claude Code, Codex) stay resolvable": HookPATH must never drop a
// directory that was already on the inherited/base PATH, regardless of
// whether the daemon executable happens to be named "ao". A typical dev
// machine has those installed under something like ~/.local/bin or
// /opt/homebrew/bin, which is exactly the kind of entry Checkpoint 8I.1's
// PATH-collapse bug silently lost.
func TestHookPATH_PreservesInheritedAgentDirs(t *testing.T) {
	claudeDir := "/Users/dev/.local/bin"
	codexDir := "/opt/homebrew/bin"
	inherited := claudeDir + string(os.PathListSeparator) + codexDir
	getenv := func(key string) string {
		if key == "PATH" {
			return inherited
		}
		return ""
	}

	for _, tc := range []struct {
		name       string
		executable func() (string, error)
	}{
		{"named ao", func() (string, error) { return filepath.Join("/opt", "aod", "ao"), nil }},
		{"not named ao", func() (string, error) { return filepath.Join("/opt", "aod", "ao-daemon"), nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := HookPATH(tc.executable, getenv, nil)
			if err != nil {
				t.Fatalf("HookPATH: %v", err)
			}
			for _, dir := range []string{claudeDir, codexDir} {
				if !strings.Contains(got, dir) {
					t.Fatalf("HookPATH = %q, want it to still contain agent install dir %q", got, dir)
				}
			}
		})
	}
}

func TestEffectiveHarnessAndAgentConfig(t *testing.T) {
	cfg := domain.ProjectConfig{
		AgentConfig:  domain.AgentConfig{Model: "base", Mode: "low", Permissions: domain.PermissionModeAuto},
		Worker:       domain.RoleOverride{Harness: domain.HarnessCodex, AgentConfig: domain.AgentConfig{Model: "worker", Mode: "high"}},
		Orchestrator: domain.RoleOverride{Harness: domain.HarnessClaudeCode},
	}

	// Explicit harness always wins.
	if h := effectiveHarness(domain.HarnessAider, domain.KindWorker, cfg); h != domain.HarnessAider {
		t.Fatalf("explicit harness = %q, want aider", h)
	}
	// Empty harness falls back to the role override per kind.
	if h := effectiveHarness("", domain.KindWorker, cfg); h != domain.HarnessCodex {
		t.Fatalf("worker harness = %q, want codex", h)
	}
	if h := effectiveHarness("", domain.KindOrchestrator, cfg); h != domain.HarnessClaudeCode {
		t.Fatalf("orchestrator harness = %q, want claude-code", h)
	}

	// Role override merges over the base agent config (set fields win; unset keep base).
	got := effectiveAgentConfig(domain.KindWorker, cfg)
	if got.Model != "worker" || got.Mode != "high" || got.Permissions != domain.PermissionModeAuto {
		t.Fatalf("merged worker config = %#v, want model=worker mode=high permissions=auto", got)
	}
	// Orchestrator has no agent-config override, so the base config is used as-is.
	if got := effectiveAgentConfig(domain.KindOrchestrator, cfg); got.Model != "base" {
		t.Fatalf("orchestrator config = %#v, want base", got)
	}
}

func TestApplySymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows symlink creation requires a host privilege outside this unit test")
	}
	project := t.TempDir()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, ".env"), []byte("X=1"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A present source is linked; a missing source is skipped, not an error.
	if err := applySymlinks(project, workspace, []string{".env", "missing.txt"}); err != nil {
		t.Fatalf("applySymlinks: %v", err)
	}
	target := filepath.Join(workspace, ".env")
	if data, err := os.ReadFile(target); err != nil || string(data) != "X=1" {
		t.Fatalf("symlinked .env = %q err=%v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(workspace, "missing.txt")); !os.IsNotExist(err) {
		t.Fatal("missing source should not have been linked")
	}
}

func TestApplySymlinksRejectsParentTraversal(t *testing.T) {
	project := t.TempDir()
	workspace := t.TempDir()
	// A "..", "/" or "../" segment escapes the project tree and must be refused
	// before any stat/link runs, so a project config cannot link in arbitrary
	// host files.
	for _, bad := range []string{"../escape", "/etc/passwd", "a/../../b", ".."} {
		if err := applySymlinks(project, workspace, []string{bad}); err == nil {
			t.Fatalf("applySymlinks(%q) accepted an unsafe path", bad)
		}
	}
}

func TestRunPostCreate(t *testing.T) {
	workspace := t.TempDir()
	if err := runPostCreate(context.Background(), workspace, []string{"echo hi > out.txt"}); err != nil {
		t.Fatalf("runPostCreate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "out.txt")); err != nil {
		t.Fatalf("post-create command did not run in workspace: %v", err)
	}
	// A failing command surfaces an error.
	if err := runPostCreate(context.Background(), workspace, []string{"exit 3"}); err == nil {
		t.Fatal("expected error from failing post-create command")
	}
}
