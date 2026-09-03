package router_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/directbranch"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/gitworktree"
	workspacerouter "github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/router"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// observe_placement_test.go — P3-A. An observation is a READ of a path the
// caller already holds, and it was routed by the PROJECT's execution mode. A
// direct-branch run in a project configured for isolated worktrees was
// therefore observed by the worktree adapter, whose managed-root guard refuses
// the user's own repository — correctly, for every operation that would delete
// or check out something, and disastrously for this one. The refusal was
// swallowed by the work step watching for that worker's change, which then had
// nothing to conclude on and polled itself forever.

type reproRepos struct{ path string }

func (r reproRepos) RepoPath(_ domain.ProjectID) (string, error) { return r.path, nil }

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=ao", "GIT_AUTHOR_EMAIL=ao@example.test",
		"GIT_COMMITTER_NAME=ao", "GIT_COMMITTER_EMAIL=ao@example.test")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// realPlacementRouter builds the production adapter pair over a real repository
// and a real managed root. The fakes elsewhere in this package cannot show this
// bug: they accept any path, which is precisely the guard that fails here.
func realPlacementRouter(t *testing.T, mode domain.ExecutionMode) (*workspacerouter.Workspace, string) {
	t.Helper()
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	managed := filepath.Join(base, "worktrees")
	for _, dir := range []string{repo, managed} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, repo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-m", "base")

	gw, err := gitworktree.New(gitworktree.Options{ManagedRoot: managed, RepoResolver: reproRepos{repo}})
	if err != nil {
		t.Fatal(err)
	}
	db, err := directbranch.New(directbranch.Options{RepoResolver: reproRepos{repo}})
	if err != nil {
		t.Fatal(err)
	}
	return workspacerouter.New(workspacerouter.Deps{
		Git: gw, DirectBranch: db,
		Projects: projectStore{projects: map[string]domain.ProjectRecord{
			"p1": {ID: "p1", Path: repo, Config: domain.ProjectConfig{ExecutionMode: mode, DefaultBranch: "main"}},
		}},
	}), repo
}

// The blocker: a direct-branch worker's own change, in a project configured for
// isolated worktrees, must be observable.
func TestObserveDirectBranchCheckoutInIsolatedProject(t *testing.T) {
	router, repo := realPlacementRouter(t, domain.ExecutionIsolatedWorktree)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("the worker's change\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	obs, err := router.ObserveWorkspace(context.Background(), ports.WorkspaceInfo{
		Path: repo, Branch: "main", SessionID: "s1", ProjectID: "p1",
	})
	if err != nil {
		t.Fatalf("observing a direct-branch checkout: %v", err)
	}
	if !obs.Dirty || len(obs.Changes) == 0 {
		t.Fatalf("observation reported no change over a dirty repository: %+v", obs)
	}
	if obs.HeadSHA == "" {
		t.Fatal("observation reported no HEAD")
	}
}

// The converse still holds, so the fallback is not a way to observe anything at
// all: a managed worktree in a direct-branch project is observed too.
func TestObserveManagedWorktreeInDirectBranchProject(t *testing.T) {
	router, repo := realPlacementRouter(t, domain.ExecutionDirectBranch)
	created, err := router.Create(context.Background(), ports.WorkspaceConfig{
		ProjectID: "p1", SessionID: "s1", Branch: "ao/s1/root", BaseBranch: "main",
		Placement: domain.PlacementIsolatedWorktree,
	})
	if err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	if created.Path == repo {
		t.Fatalf("an isolated placement was materialised in the repository itself: %q", created.Path)
	}
	if err := os.WriteFile(filepath.Join(created.Path, "a.txt"), []byte("worktree change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	obs, err := router.ObserveWorkspace(context.Background(), ports.WorkspaceInfo{
		Path: created.Path, Branch: created.Branch, SessionID: "s1", ProjectID: "p1",
	})
	if err != nil {
		t.Fatalf("observing a managed worktree: %v", err)
	}
	if !obs.Dirty {
		t.Fatalf("observation reported no change over a dirty worktree: %+v", obs)
	}
}

// And the guard it must not weaken: the fallback is for observation only. A
// destructive operation on a path the worktree adapter does not manage is still
// refused rather than quietly handed to the other adapter.
func TestDestructiveOperationsStillRefuseUnmanagedPaths(t *testing.T) {
	router, repo := realPlacementRouter(t, domain.ExecutionIsolatedWorktree)
	info := ports.WorkspaceInfo{Path: repo, Branch: "main", SessionID: "s1", ProjectID: "p1"}
	if err := router.Destroy(context.Background(), info); err == nil {
		t.Fatal("Destroy accepted a path outside the managed root")
	}
	if err := router.ForceDestroy(context.Background(), info); err == nil {
		t.Fatal("ForceDestroy accepted a path outside the managed root")
	}
}
