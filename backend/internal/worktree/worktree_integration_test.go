package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func requireGit(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	return path
}

func git(t *testing.T, binary, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(binary, append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANGUAGE=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// setupRepo builds a repository shaped like the one a user actually has open:
// on a feature branch, with an uncommitted edit and an untracked file sitting
// in the working tree. Both are what the manager must leave exactly as it
// found them.
func setupRepo(t *testing.T, binary string) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o750); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	git(t, binary, repo, "init", "--initial-branch=main")
	git(t, binary, repo, "config", "user.email", "ao@example.com")
	git(t, binary, repo, "config", "user.name", "Ao Agents")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("seed\n"), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	git(t, binary, repo, "add", "README.md")
	git(t, binary, repo, "commit", "-m", "seed")
	git(t, binary, repo, "checkout", "-b", "feat/user-work")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("seed\nuser edit\n"), 0o600); err != nil {
		t.Fatalf("dirty seed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "scratch.txt"), []byte("untracked\n"), 0o600); err != nil {
		t.Fatalf("untracked: %v", err)
	}
	return repo
}

type primaryTreeState struct {
	branch string
	head   string
	status string
	readme string
}

func snapshotPrimary(t *testing.T, binary, repo string) primaryTreeState {
	t.Helper()
	readme, err := os.ReadFile(filepath.Join(repo, "README.md"))
	if err != nil {
		t.Fatalf("read primary README: %v", err)
	}
	return primaryTreeState{
		branch: git(t, binary, repo, "rev-parse", "--abbrev-ref", "HEAD"),
		head:   git(t, binary, repo, "rev-parse", "HEAD"),
		status: git(t, binary, repo, "status", "--porcelain"),
		readme: string(readme),
	}
}

func newIntegrationManager(t *testing.T, binary string) (*Manager, *fakeStore) {
	t.Helper()
	store := newFakeStore()
	m, err := New(Options{
		Root:  filepath.Join(t.TempDir(), "ao-worktrees"),
		Git:   NewExecGit(binary),
		Store: store,
		Now:   func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	return m, store
}

// The load-bearing test for the whole package: creating and tearing down an
// AO worktree must be invisible to the checkout the user is sitting in. Not
// just "the files are still there" -- the same branch, the same HEAD, the same
// staged/unstaged/untracked status, and the same uncommitted content.
func TestIntegrationPrimaryCheckoutUntouchedByCreateAndTeardown(t *testing.T) {
	binary := requireGit(t)
	repo := setupRepo(t, binary)
	m, _ := newIntegrationManager(t, binary)
	ctx := context.Background()

	before := snapshotPrimary(t, binary, repo)

	lease, err := m.Ensure(ctx, Request{
		ProjectID: "proj", WorkflowRunID: "wf-1", TaskID: "t1",
		RepoPath: repo, TargetBranch: "main", Mode: domain.ExecutionIsolatedWorktree,
	})
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if got := snapshotPrimary(t, binary, repo); got != before {
		t.Fatalf("primary checkout changed by worktree creation:\n got %#v\nwant %#v", got, before)
	}

	// Work happens in the AO worktree, on the AO branch -- and still does not
	// reach the primary tree.
	if err := os.WriteFile(filepath.Join(lease.Path, "task.txt"), []byte("agent work\n"), 0o600); err != nil {
		t.Fatalf("write in worktree: %v", err)
	}
	git(t, binary, lease.Path, "add", "task.txt")
	git(t, binary, lease.Path, "commit", "-m", "task work")
	taskCommit := git(t, binary, lease.Path, "rev-parse", "HEAD")
	if got := snapshotPrimary(t, binary, repo); got != before {
		t.Fatalf("primary checkout changed by work inside the AO worktree:\n got %#v\nwant %#v", got, before)
	}

	rec, released, err := m.Release(ctx, "t1")
	if err != nil || !released {
		t.Fatalf("release = %v, %v", released, err)
	}
	if _, err := os.Stat(lease.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree path after release: %v, want gone", err)
	}
	if got := snapshotPrimary(t, binary, repo); got != before {
		t.Fatalf("primary checkout changed by teardown:\n got %#v\nwant %#v", got, before)
	}
	// The branch outlives the directory, so the task's commit is still
	// reachable from the record.
	if got := git(t, binary, repo, "rev-parse", "refs/heads/"+rec.Branch); got != taskCommit {
		t.Fatalf("branch %s = %s after release, want the task commit %s", rec.Branch, got, taskCommit)
	}
}

// The durable record has to describe the worktree that actually exists, not a
// plausible one: real branch, real base commit, real path.
func TestIntegrationRecordMatchesRealGitState(t *testing.T) {
	binary := requireGit(t)
	repo := setupRepo(t, binary)
	m, store := newIntegrationManager(t, binary)
	mainHead := git(t, binary, repo, "rev-parse", "refs/heads/main")

	lease, err := m.Ensure(context.Background(), Request{
		ProjectID: "proj", WorkflowRunID: "wf-1", TaskID: "t1",
		RepoPath: repo, TargetBranch: "main", Mode: domain.ExecutionSmartParallelWorktrees,
	})
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(lease.Path, "README.md")); err != nil {
		t.Fatalf("worktree not materialised: %v", err)
	}
	if got := git(t, binary, lease.Path, "rev-parse", "--abbrev-ref", "HEAD"); got != "ao/wf-1/t1" {
		t.Fatalf("worktree branch = %q, want ao/wf-1/t1", got)
	}
	if lease.Record.BaseSHA != mainHead {
		t.Fatalf("base sha = %q, want main head %q", lease.Record.BaseSHA, mainHead)
	}
	if got := git(t, binary, repo, "rev-parse", "refs/heads/ao/wf-1/t1"); got != mainHead {
		t.Fatalf("ao branch head = %s, want %s", got, mainHead)
	}
	// The worktree lives under the managed root, never in the repository.
	if !strings.HasPrefix(lease.Path, m.root) {
		t.Fatalf("path %q is outside the managed root %q", lease.Path, m.root)
	}
	stored := store.rows["t1"]
	if stored.Path != lease.Path || stored.Branch != "ao/wf-1/t1" || stored.State != domain.TaskWorktreeActive {
		t.Fatalf("stored record = %#v", stored)
	}
	// The AO branch must carry no upstream: an isolated task branch is never
	// something AO can push by accident.
	out, err := exec.Command(binary, "-C", repo, "config", "--get", "branch.ao/wf-1/t1.remote").Output()
	if err == nil && strings.TrimSpace(string(out)) != "" {
		t.Fatalf("ao branch tracks %q, want no upstream", strings.TrimSpace(string(out)))
	}
}

// A direct-branch task must not produce a directory, a branch, or a record --
// against real git, not just against a fake that was told not to be called.
func TestIntegrationDirectBranchLeavesRepoWorktreeFree(t *testing.T) {
	binary := requireGit(t)
	repo := setupRepo(t, binary)
	m, store := newIntegrationManager(t, binary)
	before := snapshotPrimary(t, binary, repo)

	lease, err := m.Ensure(context.Background(), Request{
		ProjectID: "proj", WorkflowRunID: "wf-1", TaskID: "t1",
		RepoPath: repo, TargetBranch: "feat/user-work", Mode: domain.ExecutionDirectBranch,
	})
	if err != nil {
		t.Fatalf("ensure direct branch: %v", err)
	}
	if lease.Isolated || lease.Path != repo {
		t.Fatalf("lease = %#v, want the primary repo and no isolation", lease)
	}
	if got := len(store.rows); got != 0 {
		t.Fatalf("stored records = %d, want none", got)
	}
	// `git worktree list` shows exactly one entry -- the primary checkout.
	if lines := strings.Split(git(t, binary, repo, "worktree", "list"), "\n"); len(lines) != 1 {
		t.Fatalf("worktree list = %v, want only the primary checkout", lines)
	}
	if branches := git(t, binary, repo, "branch", "--list", "ao/*"); branches != "" {
		t.Fatalf("ao branches = %q, want none", branches)
	}
	if _, err := os.Stat(m.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed root stat = %v, want never created", err)
	}
	if got := snapshotPrimary(t, binary, repo); got != before {
		t.Fatalf("primary checkout changed:\n got %#v\nwant %#v", got, before)
	}
}

// Real git refuses to remove a worktree with uncommitted work; the manager
// must surface that refusal and preserve the directory rather than force it.
func TestIntegrationReleaseRefusesDirtyWorktree(t *testing.T) {
	binary := requireGit(t)
	repo := setupRepo(t, binary)
	m, store := newIntegrationManager(t, binary)
	ctx := context.Background()

	lease, err := m.Ensure(ctx, Request{
		ProjectID: "proj", WorkflowRunID: "wf-1", TaskID: "t1",
		RepoPath: repo, TargetBranch: "main", Mode: domain.ExecutionIsolatedWorktree,
	})
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	wip := filepath.Join(lease.Path, "wip.txt")
	if err := os.WriteFile(wip, []byte("uncommitted\n"), 0o600); err != nil {
		t.Fatalf("write wip: %v", err)
	}

	if _, released, err := m.Release(ctx, "t1"); err == nil || released {
		t.Fatalf("release = %v, %v; want git's refusal", released, err)
	}
	if _, err := os.Stat(wip); err != nil {
		t.Fatalf("uncommitted work was destroyed: %v", err)
	}
	if got := store.rows["t1"].State; got != domain.TaskWorktreeFailed {
		t.Fatalf("state = %q, want failed", got)
	}
}
