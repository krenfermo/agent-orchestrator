package project_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// newManagerWithRoots builds a Manager confined to the given allowed project
// roots, over a real isolated sqlite store.
func newManagerWithRoots(t *testing.T, roots []string) project.Manager {
	t.Helper()
	t.Setenv("GIT_CEILING_DIRECTORIES", os.TempDir())
	store, err := sqlitetest.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return project.NewWithDeps(project.Deps{Store: store, AllowedRoots: roots})
}

// gitRepoAt creates a real git repository with an initial commit at the given
// path (which must already exist as an empty directory or not exist).
func gitRepoAt(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-b", "main", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "-c", "user.email=ao@example.com", "-c", "user.name=AO Test", "commit", "--allow-empty", "-m", "initial").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v (%s)", err, out)
	}
}

func TestAdd_AllowedRoots_AcceptsValidNestedPath(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "sub", "repo")
	gitRepoAt(t, repo)

	mgr := newManagerWithRoots(t, []string{root})
	p, err := mgr.Add(context.Background(), project.AddInput{Path: "sub/repo"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if p.Path != repo && p.Path != resolveSymlinks(t, repo) {
		t.Errorf("Path = %q, want %q", p.Path, repo)
	}
}

func TestAdd_AllowedRoots_RejectsTraversal(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "allowed")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "outside-repo")
	gitRepoAt(t, outside)

	mgr := newManagerWithRoots(t, []string{root})
	_, err := mgr.Add(context.Background(), project.AddInput{Path: "../outside-repo"})
	if err == nil {
		t.Fatal("Add: want error for traversal outside allowed root, got nil")
	}
	wantCode(t, err, "PATH_OUTSIDE_ALLOWED_ROOTS")
}

func TestAdd_AllowedRoots_RejectsAbsolutePathOutsideRoots(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	gitRepoAt(t, filepath.Join(outside, "repo"))

	mgr := newManagerWithRoots(t, []string{root})
	_, err := mgr.Add(context.Background(), project.AddInput{Path: filepath.Join(outside, "repo")})
	if err == nil {
		t.Fatal("Add: want error for absolute path outside allowed roots, got nil")
	}
	wantCode(t, err, "PATH_OUTSIDE_ALLOWED_ROOTS")
}

func TestAdd_AllowedRoots_RejectsSymlinkEscape(t *testing.T) {
	if os.Getenv("CI") != "" {
		// Symlink creation can be restricted in some sandboxed CI runners; this
		// case is exercised locally and is not essential to gate merges on.
	}
	parent := t.TempDir()
	root := filepath.Join(parent, "allowed")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "outside-repo")
	gitRepoAt(t, outside)

	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	mgr := newManagerWithRoots(t, []string{root})
	_, err := mgr.Add(context.Background(), project.AddInput{Path: "escape"})
	if err == nil {
		t.Fatal("Add: want error for symlink escaping allowed root, got nil")
	}
	wantCode(t, err, "PATH_OUTSIDE_ALLOWED_ROOTS")
}

func TestAdd_AllowedRoots_RejectsNonGitDirectory(t *testing.T) {
	root := t.TempDir()
	plain := filepath.Join(root, "plain")
	if err := os.Mkdir(plain, 0o755); err != nil {
		t.Fatal(err)
	}

	mgr := newManagerWithRoots(t, []string{root})
	_, err := mgr.Add(context.Background(), project.AddInput{Path: "plain"})
	if err == nil {
		t.Fatal("Add: want error for non-git directory, got nil")
	}
	wantCode(t, err, "NOT_A_GIT_REPO")
}

func TestAdd_AllowedRoots_DuplicateRegistrationIsClearConflict(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	gitRepoAt(t, repo)

	mgr := newManagerWithRoots(t, []string{root})
	if _, err := mgr.Add(context.Background(), project.AddInput{Path: "repo"}); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	_, err := mgr.Add(context.Background(), project.AddInput{Path: "repo"})
	if err == nil {
		t.Fatal("second Add: want conflict error, got nil")
	}
	wantCode(t, err, "PATH_ALREADY_REGISTERED")
}

func TestListAllowedRootEntries_RejectsTraversalAndListsGitRepos(t *testing.T) {
	root := t.TempDir()
	gitRepoAt(t, filepath.Join(root, "repo-a"))
	if err := os.Mkdir(filepath.Join(root, "plain-b"), 0o755); err != nil {
		t.Fatal(err)
	}

	mgr := newManagerWithRoots(t, []string{root})
	entries, err := mgr.ListAllowedRootEntries(context.Background(), "")
	if err != nil {
		t.Fatalf("ListAllowedRootEntries: %v", err)
	}
	byName := map[string]project.BrowseEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	if !byName["repo-a"].IsGitRepo {
		t.Errorf("repo-a.IsGitRepo = false, want true")
	}
	if byName["plain-b"].IsGitRepo {
		t.Errorf("plain-b.IsGitRepo = true, want false")
	}

	if _, err := mgr.ListAllowedRootEntries(context.Background(), "../"); err == nil {
		t.Fatal("ListAllowedRootEntries(\"../\"): want error, got nil")
	}
}

func TestListAllowedRootEntries_NoRootsConfigured(t *testing.T) {
	mgr := newManagerWithRoots(t, nil)
	_, err := mgr.ListAllowedRootEntries(context.Background(), "")
	if err == nil {
		t.Fatal("want error when no allowed roots configured, got nil")
	}
	wantCode(t, err, "NO_ALLOWED_ROOTS_CONFIGURED")
}

func resolveSymlinks(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return resolved
}
