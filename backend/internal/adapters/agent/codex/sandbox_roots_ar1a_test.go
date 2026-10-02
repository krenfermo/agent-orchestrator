package codex

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// AR-1a (Codex AR1A-INT-01) on the terminal path: a sandboxed Codex launch or
// restore carries `--add-dir` for every explicit extra writable root -- the
// git directories of the workspace's repositories (without them `git add` /
// `git commit` are denied inside workspace-write) and a workspace project's
// child roots. Explicit bypass carries none, and invalid roots refuse.

func gitInit(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func addDirs(argv []string) []string {
	var out []string
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--add-dir" {
			out = append(out, argv[i+1])
		}
	}
	return out
}

func TestSandboxedLaunchAddsTheRepositoryGitDirectory(t *testing.T) {
	ws := canonicalTempDir(t)
	gitInit(t, ws)
	plugin := &Plugin{resolvedBinary: "codex"}
	cmd, err := plugin.GetLaunchCommand(context.Background(), ports.LaunchConfig{WorkspacePath: ws, Permissions: ports.PermissionModeDefault})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := addDirs(cmd), []string{filepath.Join(ws, ".git")}; !slices.Equal(got, want) {
		t.Fatalf("--add-dir = %v, want %v", got, want)
	}
}

func TestSandboxedLaunchOfAWorkspaceProjectAddsChildRootsAndTheirGitDirectories(t *testing.T) {
	ws := canonicalTempDir(t)
	gitInit(t, ws)
	child := filepath.Join(ws, "child")
	gitInit(t, child)
	plugin := &Plugin{resolvedBinary: "codex"}
	cmd, err := plugin.GetLaunchCommand(context.Background(), ports.LaunchConfig{
		WorkspacePath: ws, Permissions: ports.PermissionModeDefault, AdditionalDirectories: []string{child, child},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{child, filepath.Join(ws, ".git"), filepath.Join(child, ".git")}
	if got := addDirs(cmd); !slices.Equal(got, want) {
		t.Fatalf("--add-dir = %v, want %v", got, want)
	}
}

func TestSandboxedRestoreCarriesTheSameRoots(t *testing.T) {
	ws := canonicalTempDir(t)
	gitInit(t, ws)
	plugin := &Plugin{resolvedBinary: "codex"}
	cmd, ok, err := plugin.GetRestoreCommand(context.Background(), ports.RestoreConfig{
		Session:     ports.SessionRef{ID: "s-1", WorkspacePath: ws, Metadata: map[string]string{ports.MetadataKeyAgentSessionID: "thread-1"}},
		Permissions: ports.PermissionModeDefault,
	})
	if err != nil || !ok {
		t.Fatalf("restore: ok=%v err=%v", ok, err)
	}
	if got, want := addDirs(cmd), []string{filepath.Join(ws, ".git")}; !slices.Equal(got, want) {
		t.Fatalf("restore --add-dir = %v, want %v", got, want)
	}
}

func TestExplicitBypassAddsNoRootsAndNoSandbox(t *testing.T) {
	ws := canonicalTempDir(t)
	gitInit(t, ws)
	plugin := &Plugin{resolvedBinary: "codex"}
	cmd, err := plugin.GetLaunchCommand(context.Background(), ports.LaunchConfig{WorkspacePath: ws, Permissions: ports.PermissionModeBypassPermissions})
	if err != nil {
		t.Fatal(err)
	}
	if got := addDirs(cmd); len(got) != 0 {
		t.Fatalf("explicit bypass got --add-dir %v", got)
	}
}

// Fail closed: an invalid root refuses the launch; and the sandboxed default
// never silently becomes full access.
func TestInvalidRootsRefuseTheLaunch(t *testing.T) {
	ws := canonicalTempDir(t)
	outside := canonicalTempDir(t)
	plugin := &Plugin{resolvedBinary: "codex"}
	for name, dirs := range map[string][]string{"outside": {outside}, "relative": {"child"}, "missing": {filepath.Join(ws, "nope")}} {
		t.Run(name, func(t *testing.T) {
			if _, err := plugin.GetLaunchCommand(context.Background(), ports.LaunchConfig{WorkspacePath: ws, Permissions: ports.PermissionModeDefault, AdditionalDirectories: dirs}); err == nil {
				t.Fatalf("launch accepted invalid roots %v", dirs)
			}
		})
	}
	failing := &Plugin{resolvedBinary: "codex", writableRoots: func(context.Context, string, []string) ([]string, error) {
		return nil, errors.New("git broke")
	}}
	if _, err := failing.GetLaunchCommand(context.Background(), ports.LaunchConfig{WorkspacePath: ws, Permissions: ports.PermissionModeDefault}); err == nil {
		t.Fatalf("launch accepted unresolvable roots")
	}
	cmd, err := plugin.GetLaunchCommand(context.Background(), ports.LaunchConfig{WorkspacePath: ws, Permissions: ""})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(cmd, "--dangerously-bypass-approvals-and-sandbox") || !slices.Contains(cmd, "workspace-write") {
		t.Fatalf("default launch is not sandboxed: %v", cmd)
	}
}
