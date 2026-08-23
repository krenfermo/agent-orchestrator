package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func requireGit(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	return path
}

func runGit(t *testing.T, binary, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(binary, append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANGUAGE=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// setupRepo builds a repository in the state a user's actually is: on a
// feature branch, with an uncommitted edit and an untracked file. Every
// operation in this package has to leave all three alone.
func setupRepo(t *testing.T, binary string) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o750); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	runGit(t, binary, repo, "init", "--initial-branch=main")
	runGit(t, binary, repo, "config", "user.email", "ao@example.com")
	runGit(t, binary, repo, "config", "user.name", "Ao Agents")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("seed\n"), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	runGit(t, binary, repo, "add", "README.md")
	runGit(t, binary, repo, "commit", "-m", "seed")
	runGit(t, binary, repo, "checkout", "-b", "feat/user-work")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("seed\nuser edit\n"), 0o600); err != nil {
		t.Fatalf("dirty seed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "scratch.txt"), []byte("untracked\n"), 0o600); err != nil {
		t.Fatalf("untracked: %v", err)
	}
	return repo
}

type treeState struct{ branch, head, status string }

func snapshot(t *testing.T, binary, repo string) treeState {
	t.Helper()
	return treeState{
		branch: runGit(t, binary, repo, "rev-parse", "--abbrev-ref", "HEAD"),
		head:   runGit(t, binary, repo, "rev-parse", "HEAD"),
		status: runGit(t, binary, repo, "status", "--porcelain"),
	}
}

func TestResolveCommit(t *testing.T) {
	binary := requireGit(t)
	repo := setupRepo(t, binary)
	g := NewExecGit(binary)
	ctx := context.Background()

	want := runGit(t, binary, repo, "rev-parse", "refs/heads/main")
	got, err := g.ResolveCommit(ctx, repo, "refs/heads/main")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != want {
		t.Fatalf("resolve = %q, want %q", got, want)
	}
	// A ref that is not there is an error, never an empty string a caller
	// could mistake for a commit and record as a base SHA.
	if sha, err := g.ResolveCommit(ctx, repo, "refs/heads/nope"); err == nil {
		t.Fatalf("resolve missing ref = %q, want an error", sha)
	}
}

// "No such branch" and "this repository is broken" must not collapse into the
// same answer: reporting a broken repo as "branch absent" would make the
// lifecycle manager cut a new branch over work it could not see.
func TestBranchExistsDistinguishesAbsentFromBroken(t *testing.T) {
	binary := requireGit(t)
	repo := setupRepo(t, binary)
	g := NewExecGit(binary)
	ctx := context.Background()

	if ok, err := g.BranchExists(ctx, repo, "main"); err != nil || !ok {
		t.Fatalf("BranchExists(main) = %v, %v; want true", ok, err)
	}
	if ok, err := g.BranchExists(ctx, repo, "ao/nope"); err != nil || ok {
		t.Fatalf("BranchExists(ao/nope) = %v, %v; want false", ok, err)
	}
	notARepo := t.TempDir()
	if ok, err := g.BranchExists(ctx, notARepo, "main"); err == nil || ok {
		t.Fatalf("BranchExists in a non-repo = %v, %v; want an error", ok, err)
	}
}

// The add/remove/prune cycle against real git, with the user's checkout
// snapshotted around every step: these are the operations that could touch it,
// so this is where "they do not" has to be shown.
func TestWorktreeAddRemovePruneLeavePrimaryCheckoutAlone(t *testing.T) {
	binary := requireGit(t)
	repo := setupRepo(t, binary)
	g := NewExecGit(binary)
	ctx := context.Background()
	base := runGit(t, binary, repo, "rev-parse", "refs/heads/main")
	before := snapshot(t, binary, repo)
	path := filepath.Join(t.TempDir(), "wt")

	if err := g.AddWorktreeNewBranch(ctx, repo, path, "ao/t1", base); err != nil {
		t.Fatalf("add new branch: %v", err)
	}
	if got := runGit(t, binary, path, "rev-parse", "--abbrev-ref", "HEAD"); got != "ao/t1" {
		t.Fatalf("worktree branch = %q, want ao/t1", got)
	}
	if got := runGit(t, binary, path, "rev-parse", "HEAD"); got != base {
		t.Fatalf("worktree head = %q, want the requested base %q", got, base)
	}
	if got := snapshot(t, binary, repo); got != before {
		t.Fatalf("primary tree changed by add: %#v, want %#v", got, before)
	}

	// Uncommitted work must block removal: the manager relies on this refusal
	// rather than on checking cleanliness itself.
	wip := filepath.Join(path, "wip.txt")
	if err := os.WriteFile(wip, []byte("uncommitted\n"), 0o600); err != nil {
		t.Fatalf("write wip: %v", err)
	}
	if err := g.RemoveWorktree(ctx, repo, path); err == nil {
		t.Fatal("remove of a dirty worktree succeeded, want git's refusal")
	}
	if _, err := os.Stat(wip); err != nil {
		t.Fatalf("uncommitted work was destroyed: %v", err)
	}
	if err := os.Remove(wip); err != nil {
		t.Fatalf("remove wip: %v", err)
	}
	if err := g.RemoveWorktree(ctx, repo, path); err != nil {
		t.Fatalf("remove clean worktree: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree path after remove = %v, want gone", err)
	}
	// Removing a worktree removes a checkout, never commits: the branch is
	// still there for whoever has to integrate it.
	if ok, err := g.BranchExists(ctx, repo, "ao/t1"); err != nil || !ok {
		t.Fatalf("branch after remove = %v, %v; want it kept", ok, err)
	}
	if got := snapshot(t, binary, repo); got != before {
		t.Fatalf("primary tree changed by remove: %#v, want %#v", got, before)
	}
}

// Prune exists so a registration that outlived its directory can be replaced.
// It must drop the registration and touch nothing on disk -- including the
// user's checkout, which git happily walks past while pruning.
func TestPruneDropsStaleRegistrationWithoutDeletingAnything(t *testing.T) {
	binary := requireGit(t)
	repo := setupRepo(t, binary)
	g := NewExecGit(binary)
	ctx := context.Background()
	base := runGit(t, binary, repo, "rev-parse", "refs/heads/main")
	path := filepath.Join(t.TempDir(), "wt")

	if err := g.AddWorktreeNewBranch(ctx, repo, path, "ao/t1", base); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("remove dir out of band: %v", err)
	}
	before := snapshot(t, binary, repo)

	// The stale registration is what makes a re-add fail, so it has to still
	// be there before pruning, or this proves nothing.
	if !strings.Contains(runGit(t, binary, repo, "worktree", "list"), path) {
		t.Fatal("registration vanished on its own; nothing left to prune")
	}
	if err := g.Prune(ctx, repo); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if strings.Contains(runGit(t, binary, repo, "worktree", "list"), path) {
		t.Fatal("stale registration survived prune")
	}
	if got := snapshot(t, binary, repo); got != before {
		t.Fatalf("primary tree changed by prune: %#v, want %#v", got, before)
	}
	// The path is now free, and re-adding on the branch that still exists is
	// the recovery form the lifecycle manager uses.
	if err := g.AddWorktreeExistingBranch(ctx, repo, path, "ao/t1"); err != nil {
		t.Fatalf("re-add on the existing branch: %v", err)
	}
	if got := runGit(t, binary, path, "rev-parse", "--abbrev-ref", "HEAD"); got != "ao/t1" {
		t.Fatalf("re-added worktree branch = %q, want ao/t1", got)
	}
	if got := snapshot(t, binary, repo); got != before {
		t.Fatalf("primary tree changed by re-add: %#v, want %#v", got, before)
	}
}
