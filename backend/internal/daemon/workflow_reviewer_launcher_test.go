package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// fakeReviewerAdapter is a hand-rolled fake ports.Reviewer that records
// exactly what ReviewCommand was called with, so the test below can assert
// workflowReviewerLauncher reuses the adapter's own command/config
// resolution rather than constructing a permissive one itself (Checkpoint
// 8C, test item #18).
type fakeReviewerAdapter struct {
	lastInvocation ports.ReviewInvocation
	cmd            ports.ReviewCommandSpec
	err            error
	preLaunchInv   ports.ReviewInvocation
	preLaunchCalls int
}

func (f *fakeReviewerAdapter) ReviewCommand(_ context.Context, inv ports.ReviewInvocation) (ports.ReviewCommandSpec, error) {
	f.lastInvocation = inv
	if f.err != nil {
		return ports.ReviewCommandSpec{}, f.err
	}
	return f.cmd, nil
}

func (f *fakeReviewerAdapter) ReviewMessage(_ context.Context, _ ports.ReviewInvocation) (string, error) {
	return "", nil
}

// PreLaunch makes fakeReviewerAdapter satisfy the launcher's optional
// preLaunchReviewer capability so tests can assert on the ReviewInvocation
// (in particular Env) built before a workflow reviewer pane starts
// (Checkpoint 8P-E.3.1).
func (f *fakeReviewerAdapter) PreLaunch(_ context.Context, inv ports.ReviewInvocation) error {
	f.preLaunchInv = inv
	f.preLaunchCalls++
	return nil
}

// Regression (Checkpoint 8M.1 §20): reviewer tool execution (e.g. pytest run
// read-only during review) must default to PYTHONDONTWRITEBYTECODE=1, and a
// reviewer adapter's own env must still be able to override it.
func TestWorkflowReviewerLauncherRuntimeEnvSetsPythonDontWriteBytecode(t *testing.T) {
	l := &workflowReviewerLauncher{dataDir: t.TempDir(), executable: func() (string, error) { return "/usr/local/bin/ao", nil }}
	env := l.runtimeEnv(context.Background(), workflowcore.ReviewerLaunchRequest{ReviewID: "rev-1", Harness: domain.ReviewerClaudeCode}, nil, nil)
	if env["PYTHONDONTWRITEBYTECODE"] != "1" {
		t.Fatalf("expected PYTHONDONTWRITEBYTECODE=1, got %q", env["PYTHONDONTWRITEBYTECODE"])
	}

	overridden := l.runtimeEnv(context.Background(), workflowcore.ReviewerLaunchRequest{ReviewID: "rev-1", Harness: domain.ReviewerClaudeCode}, nil, map[string]string{"PYTHONDONTWRITEBYTECODE": "0"})
	if overridden["PYTHONDONTWRITEBYTECODE"] != "0" {
		t.Fatalf("expected adapter-supplied env to override the default, got %q", overridden["PYTHONDONTWRITEBYTECODE"])
	}
}

// TestWorkflowReviewerLauncherRuntimeEnvAppliesIsolatedRuntimeHomeLast is
// Checkpoint 8P-B.1's reviewer-isolation proof: req.RuntimeEnv must win
// over every other env source (PATH pin/shim included), and a nil
// RuntimeEnv (unresolved owner) must be a pure no-op.
func TestWorkflowReviewerLauncherRuntimeEnvAppliesIsolatedRuntimeHomeLast(t *testing.T) {
	l := &workflowReviewerLauncher{dataDir: t.TempDir(), executable: func() (string, error) { return "/usr/local/bin/ao", nil }}
	req := workflowcore.ReviewerLaunchRequest{
		ReviewID: "rev-1", Harness: domain.ReviewerClaudeCode,
		RuntimeEnv: map[string]string{"HOME": "/ao/users/user-a/runtime-home", "CLAUDE_CONFIG_DIR": "/ao/users/user-a/providers/claude-code"},
	}
	env := l.runtimeEnv(context.Background(), req, nil, map[string]string{"HOME": "/real/host/home"})
	if env["HOME"] != "/ao/users/user-a/runtime-home" {
		t.Fatalf("isolated HOME did not win: %q", env["HOME"])
	}
	if env["CLAUDE_CONFIG_DIR"] != "/ao/users/user-a/providers/claude-code" {
		t.Fatalf("isolated CLAUDE_CONFIG_DIR missing: %q", env["CLAUDE_CONFIG_DIR"])
	}

	noOverride := l.runtimeEnv(context.Background(), workflowcore.ReviewerLaunchRequest{ReviewID: "rev-1", Harness: domain.ReviewerClaudeCode}, nil, nil)
	if _, ok := noOverride["CLAUDE_CONFIG_DIR"]; ok {
		t.Fatalf("nil RuntimeEnv must not fabricate a CLAUDE_CONFIG_DIR: %v", noOverride)
	}
}

type fakeReviewerResolver struct {
	adapter *fakeReviewerAdapter
}

func (f *fakeReviewerResolver) Reviewer(_ domain.ReviewerHarness) (ports.Reviewer, bool) {
	if f.adapter == nil {
		return nil, false
	}
	return f.adapter, true
}

type fakeWorkflowReviewerRuntime struct {
	lastCfg ports.RuntimeConfig
	calls   int
}

func (f *fakeWorkflowReviewerRuntime) Create(_ context.Context, cfg ports.RuntimeConfig) (ports.RuntimeHandle, error) {
	f.calls++
	f.lastCfg = cfg
	return ports.RuntimeHandle{ID: "pane-1"}, nil
}

// TestWorkflowReviewerLauncherReusesAdapterCommandUnmodified asserts that
// Launch calls the real adapter's ReviewCommand (the unmodified
// claudecode.go allowlist/denylist resolution) and passes its Argv/Env
// straight through to the runtime — the launcher never builds its own tool
// configuration.
func TestWorkflowReviewerLauncherReusesAdapterCommandUnmodified(t *testing.T) {
	adapter := &fakeReviewerAdapter{cmd: ports.ReviewCommandSpec{
		Argv: []string{"claude", "--some-real-flag"},
		Env:  map[string]string{"FROM_ADAPTER": "1"},
	}}
	runtime := &fakeWorkflowReviewerRuntime{}
	l := &workflowReviewerLauncher{
		reviewers: &fakeReviewerResolver{adapter: adapter},
		runtime:   runtime,
		dataDir:   t.TempDir(),
	}

	req := workflowcore.ReviewerLaunchRequest{
		Harness:         domain.ReviewerClaudeCode,
		WorkerSessionID: "sess-1",
		ProjectID:       "proj-1",
		ReviewID:        "review-1",
		RunID:           "run-1",
		WorkspacePath:   "/ws/wf",
		Prompt:          "review this worktree via git status/git diff",
	}
	result, err := l.Launch(context.Background(), req)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if result.HandleID != "pane-1" {
		t.Fatalf("handle id = %q, want pane-1", result.HandleID)
	}
	if len(runtime.lastCfg.Argv) != 2 || runtime.lastCfg.Argv[0] != "claude" || runtime.lastCfg.Argv[1] != "--some-real-flag" {
		t.Fatalf("runtime Argv = %v, want the adapter's own argv passed through unmodified", runtime.lastCfg.Argv)
	}
	if runtime.lastCfg.Env["FROM_ADAPTER"] != "1" {
		t.Fatalf("runtime env missing adapter-provided env var: %+v", runtime.lastCfg.Env)
	}
	if adapter.lastInvocation.Prompt == "" {
		t.Fatalf("adapter was not given the workflow-owned prompt")
	}
}

// TestWorkflowReviewerLauncherForwardsRuntimeEnvToPreLaunch is Checkpoint
// 8P-E.3.1's regression: req.RuntimeEnv (Checkpoint 8P-B.1's resolved
// per-user isolated env) must reach the reviewer adapter's PreLaunch, not
// just the runtime pane's own env -- otherwise a Claude reviewer's trust
// record lands in a config file the isolated subprocess never reads (the
// exact bug 8P-E.3 fixed for workers).
func TestWorkflowReviewerLauncherForwardsRuntimeEnvToPreLaunch(t *testing.T) {
	adapter := &fakeReviewerAdapter{cmd: ports.ReviewCommandSpec{Argv: []string{"claude"}}}
	l := &workflowReviewerLauncher{
		reviewers: &fakeReviewerResolver{adapter: adapter},
		runtime:   &fakeWorkflowReviewerRuntime{},
		dataDir:   t.TempDir(),
	}
	isolatedEnv := map[string]string{"CLAUDE_CONFIG_DIR": "/ao/users/user-a/providers/claude-code"}

	if _, err := l.Launch(context.Background(), workflowcore.ReviewerLaunchRequest{
		Harness:         domain.ReviewerClaudeCode,
		WorkerSessionID: "sess-1",
		ProjectID:       "proj-1",
		ReviewID:        "review-1",
		RunID:           "run-1",
		WorkspacePath:   "/ws/wf",
		Prompt:          "review this worktree",
		RuntimeEnv:      isolatedEnv,
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if adapter.preLaunchCalls != 1 {
		t.Fatalf("PreLaunch calls = %d, want 1", adapter.preLaunchCalls)
	}
	if adapter.preLaunchInv.Env["CLAUDE_CONFIG_DIR"] != isolatedEnv["CLAUDE_CONFIG_DIR"] {
		t.Fatalf("PreLaunch Env = %#v, want isolated CLAUDE_CONFIG_DIR forwarded", adapter.preLaunchInv.Env)
	}
}

// -- Checkpoint 8I.2 shim-gap closure --

func launchWithExecutable(t *testing.T, executable func() (string, error), dataDir string) *fakeWorkflowReviewerRuntime {
	t.Helper()
	adapter := &fakeReviewerAdapter{cmd: ports.ReviewCommandSpec{
		Argv: []string{"claude", "--some-real-flag"},
		Env:  map[string]string{"REVIEWER_ADAPTER_PATH": "/reviewer/adapter/bin"},
	}}
	rt := &fakeWorkflowReviewerRuntime{}
	l := &workflowReviewerLauncher{
		reviewers:  &fakeReviewerResolver{adapter: adapter},
		runtime:    rt,
		dataDir:    dataDir,
		executable: executable,
	}
	req := workflowcore.ReviewerLaunchRequest{
		Harness:         domain.ReviewerClaudeCode,
		WorkerSessionID: "sess-1",
		ProjectID:       "proj-1",
		ReviewID:        "review-42",
		RunID:           "run-42",
		WorkspacePath:   "/ws/wf",
		Prompt:          "review this worktree via git status/git diff",
	}
	if _, err := l.Launch(context.Background(), req); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	return rt
}

// TestWorkflowReviewerLauncherPATHPinnedWhenNamedAO covers test item #1: an
// executable literally named "ao" keeps working exactly as before (HookPATH
// pins it, no shim needed).
func TestWorkflowReviewerLauncherPATHPinnedWhenNamedAO(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "ao")
	rt := launchWithExecutable(t, func() (string, error) { return exe, nil }, t.TempDir())

	parts := strings.Split(rt.lastCfg.Env["PATH"], string(os.PathListSeparator))
	if len(parts) < 2 || parts[0] != filepath.Dir(exe) {
		t.Fatalf("reviewer PATH = %q, want daemon dir first", rt.lastCfg.Env["PATH"])
	}
	requirePathContains(t, rt.lastCfg.Env["PATH"], "/usr/bin", "/bin")
}

// TestWorkflowReviewerLauncherCreatesAOShimWhenNotNamedAO covers test items
// #2 and #3: a renamed executable produces a review-submit command that
// resolves `ao` via a real shim AO itself provides — no global `ao` install
// required, and Claude never has to discover an absolute path on its own.
func TestWorkflowReviewerLauncherCreatesAOShimWhenNotNamedAO(t *testing.T) {
	dataDir := t.TempDir()
	exe := filepath.Join(t.TempDir(), "ao-e2e-runtime-test")
	rt := launchWithExecutable(t, func() (string, error) { return exe, nil }, dataDir)

	shimDir := filepath.Join(dataDir, "reviewer-runtime", "bin")
	parts := strings.Split(rt.lastCfg.Env["PATH"], string(os.PathListSeparator))
	if len(parts) < 2 || parts[0] != shimDir {
		t.Fatalf("reviewer PATH = %q, want the AO shim dir first", rt.lastCfg.Env["PATH"])
	}
	requirePathContains(t, rt.lastCfg.Env["PATH"], "/usr/bin", "/bin")

	shimPath := filepath.Join(shimDir, "ao")
	shim, err := os.ReadFile(shimPath)
	if err != nil {
		t.Fatalf("read AO shim: %v", err)
	}
	if !strings.Contains(string(shim), exe) {
		t.Fatalf("AO shim = %q, want it to exec %q", shim, exe)
	}

	// The shim must actually be resolvable the way Claude resolves `ao`:
	// exec.LookPath against the constructed PATH, not a manually-discovered
	// absolute path.
	t.Setenv("PATH", rt.lastCfg.Env["PATH"])
	resolved, err := exec.LookPath("ao")
	if err != nil {
		t.Fatalf("ao not resolvable via PATH built by the launcher: %v", err)
	}
	if resolvedAbs, _ := filepath.Abs(resolved); resolvedAbs != shimPath {
		t.Fatalf("exec.LookPath(\"ao\") = %q, want the AO shim %q", resolved, shimPath)
	}
}

// TestWorkflowReviewerLauncherNoGlobalAORequired covers test item #3
// explicitly: even when nothing named "ao" exists anywhere else on the
// system PATH, the reviewer's own constructed PATH still resolves it via the
// shim alone.
func TestWorkflowReviewerLauncherNoGlobalAORequired(t *testing.T) {
	dataDir := t.TempDir()
	exe := filepath.Join(t.TempDir(), "ao-e2e-runtime-test")
	rt := launchWithExecutable(t, func() (string, error) { return exe, nil }, dataDir)

	// Isolate PATH resolution to exactly what the launcher produced — no
	// /opt/homebrew/bin, no ~/.local/bin, nothing that could coincidentally
	// contain a real `ao` and mask a broken shim.
	t.Setenv("PATH", rt.lastCfg.Env["PATH"])
	if _, err := exec.LookPath("ao"); err != nil {
		t.Fatalf("ao not resolvable from the launcher's own PATH with no global ao install: %v", err)
	}
}

// TestWorkflowReviewerLauncherCommandTargetsCorrectSession covers test item
// #5: the AO_REVIEW_* env AO threads through still carries the exact
// review_run/worker-session identifiers regardless of which PATH-pinning
// branch ran (pinned vs. shimmed).
func TestWorkflowReviewerLauncherCommandTargetsCorrectSession(t *testing.T) {
	for _, tc := range []struct {
		name string
		exe  string
	}{
		{"pinned", "ao"},
		{"renamed", "ao-e2e-runtime-test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exe := filepath.Join(t.TempDir(), tc.exe)
			rt := launchWithExecutable(t, func() (string, error) { return exe, nil }, t.TempDir())
			if rt.lastCfg.Env["AO_REVIEW_SESSION_ID"] != "review-42" {
				t.Fatalf("AO_REVIEW_SESSION_ID = %q, want review-42", rt.lastCfg.Env["AO_REVIEW_SESSION_ID"])
			}
			if rt.lastCfg.Env["AO_REVIEW_WORKER_SESSION_ID"] != "sess-1" {
				t.Fatalf("AO_REVIEW_WORKER_SESSION_ID = %q, want sess-1", rt.lastCfg.Env["AO_REVIEW_WORKER_SESSION_ID"])
			}
			if rt.lastCfg.SessionID != "workflow-review-run-42" {
				t.Fatalf("runtime SessionID = %q, want workflow-review-run-42", rt.lastCfg.SessionID)
			}
		})
	}
}

func requirePathContains(t *testing.T, path string, dirs ...string) {
	t.Helper()
	parts := strings.Split(path, string(os.PathListSeparator))
	for _, want := range dirs {
		found := false
		for _, p := range parts {
			if p == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("PATH = %q, want it to include required dir %q", path, want)
		}
	}
}

// TestEnsureAOShim_SharedHelperUsedByBothLaunchers is a direct regression
// guard for the extraction itself: session_manager.EnsureAOShim is the one
// place shim-script generation lives now, and review.Launcher's Spawn is
// still expected to produce byte-identical shim scripts through it (see
// internal/review/launcher_test.go's own shim tests, which exercise the
// same helper from the other caller — this just pins the helper's own
// output shape so a future edit can't silently diverge the two callers
// again).
func TestEnsureAOShim_SharedHelperUsedByBothLaunchers(t *testing.T) {
	dataDir := t.TempDir()
	exe := filepath.Join(t.TempDir(), "ao-dev-daemon")
	shimDir, err := sessionmanager.EnsureAOShim(dataDir, func() (string, error) { return exe, nil })
	if err != nil {
		t.Fatalf("EnsureAOShim: %v", err)
	}
	if shimDir != filepath.Join(dataDir, "reviewer-runtime", "bin") {
		t.Fatalf("shimDir = %q, want dataDir/reviewer-runtime/bin", shimDir)
	}
	shim, err := os.ReadFile(filepath.Join(shimDir, "ao"))
	if err != nil {
		t.Fatalf("read shim: %v", err)
	}
	if !strings.Contains(string(shim), exe) {
		t.Fatalf("shim = %q, want it to exec %q", shim, exe)
	}
}
