package project_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
		t.Skip("symlink creation may be restricted on CI runners")
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

func TestListAllowedRootEntries_SingleRootListsChildrenAndGitFlag(t *testing.T) {
	root := t.TempDir()
	gitRepoAt(t, filepath.Join(root, "repo-a"))
	if err := os.Mkdir(filepath.Join(root, "plain-b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}

	mgr := newManagerWithRoots(t, []string{root})
	// A single configured root skips straight to listing its own children --
	// nothing to choose between -- and the result's Path names that root.
	result, err := mgr.ListAllowedRootEntries(context.Background(), "")
	if err != nil {
		t.Fatalf("ListAllowedRootEntries: %v", err)
	}
	if result.Path != root && result.Path != resolveSymlinks(t, root) {
		t.Errorf("top-level Path = %q, want %q", result.Path, root)
	}
	byName := map[string]project.BrowseEntry{}
	for _, e := range result.Entries {
		byName[e.Name] = e
	}
	if !byName["repo-a"].IsGitRepo {
		t.Errorf("repo-a.IsGitRepo = false, want true")
	}
	if byName["plain-b"].IsGitRepo {
		t.Errorf("plain-b.IsGitRepo = true, want false")
	}
	if _, ok := byName[".hidden"]; ok {
		t.Errorf("dotfile directory must be hidden from the listing: %v", byName)
	}
}

// TestListAllowedRootEntries_DrillsIntoSubdirectory is Checkpoint 8P-E.4's
// core navigation regression: an entry's own Path, fed back as the next
// browse call's path, must list that subdirectory's children -- real
// multi-level folder navigation, not just a single flat listing.
func TestListAllowedRootEntries_DrillsIntoSubdirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "parent", "child"), 0o755); err != nil {
		t.Fatal(err)
	}

	mgr := newManagerWithRoots(t, []string{root})
	top, err := mgr.ListAllowedRootEntries(context.Background(), "")
	if err != nil {
		t.Fatalf("top-level ListAllowedRootEntries: %v", err)
	}
	var parentEntry *project.BrowseEntry
	for i := range top.Entries {
		if top.Entries[i].Name == "parent" {
			parentEntry = &top.Entries[i]
		}
	}
	if parentEntry == nil {
		t.Fatalf("top-level entries = %+v, want a %q entry", top.Entries, "parent")
	}

	nested, err := mgr.ListAllowedRootEntries(context.Background(), parentEntry.Path)
	if err != nil {
		t.Fatalf("drill-down ListAllowedRootEntries: %v", err)
	}
	if len(nested.Entries) != 1 || nested.Entries[0].Name != "child" {
		t.Fatalf("nested entries = %+v, want exactly one %q entry", nested.Entries, "child")
	}
}

// TestListAllowedRootEntries_MultipleRootsListedAtTopLevel proves the
// "Allowed locations" list: when more than one root is configured, the
// top-level browse call lists the roots themselves rather than picking one
// implicitly, and each is independently drillable.
func TestListAllowedRootEntries_MultipleRootsListedAtTopLevel(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootA, "in-a"), 0o755); err != nil {
		t.Fatal(err)
	}

	mgr := newManagerWithRoots(t, []string{rootA, rootB})
	top, err := mgr.ListAllowedRootEntries(context.Background(), "")
	if err != nil {
		t.Fatalf("top-level ListAllowedRootEntries: %v", err)
	}
	if top.Path != "" {
		t.Errorf("top-level Path = %q, want empty (virtual root, still choosing between roots)", top.Path)
	}
	if len(top.Entries) != 2 {
		t.Fatalf("top-level entries = %+v, want exactly the 2 configured roots", top.Entries)
	}

	var rootAEntry *project.BrowseEntry
	for i := range top.Entries {
		if top.Entries[i].Path == rootA || top.Entries[i].Path == resolveSymlinks(t, rootA) {
			rootAEntry = &top.Entries[i]
		}
	}
	if rootAEntry == nil {
		t.Fatalf("top-level entries = %+v, want an entry for root %q", top.Entries, rootA)
	}
	inA, err := mgr.ListAllowedRootEntries(context.Background(), rootAEntry.Path)
	if err != nil {
		t.Fatalf("drill into rootA: %v", err)
	}
	if len(inA.Entries) != 1 || inA.Entries[0].Name != "in-a" {
		t.Fatalf("rootA entries = %+v, want exactly one %q entry", inA.Entries, "in-a")
	}
}

func TestListAllowedRootEntries_RejectsRelativeTraversal(t *testing.T) {
	root := t.TempDir()
	mgr := newManagerWithRoots(t, []string{root})

	// A non-empty, non-absolute path (e.g. "../", or any relative fragment)
	// is rejected outright -- the endpoint only ever accepts an absolute
	// path it itself previously returned, never a caller-authored relative
	// fragment to resolve against a root.
	if _, err := mgr.ListAllowedRootEntries(context.Background(), "../"); err == nil {
		t.Fatal("ListAllowedRootEntries(\"../\"): want error, got nil")
	} else {
		wantCode(t, err, "PATH_NOT_ABSOLUTE")
	}
}

func TestListAllowedRootEntries_RejectsAbsoluteTraversalOutsideRoots(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "allowed")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}

	mgr := newManagerWithRoots(t, []string{root})
	// filepath.Clean collapses the "../" before comparison, so this exercises
	// the same containment check as a literal outside path -- both must be
	// rejected, never silently clamped back inside the root.
	_, err := mgr.ListAllowedRootEntries(context.Background(), filepath.Join(root, "..", "outside"))
	if err == nil {
		t.Fatal("want error for absolute path escaping the allowed root via \"..\", got nil")
	}
	wantCode(t, err, "PATH_OUTSIDE_ALLOWED_ROOTS")

	_, err = mgr.ListAllowedRootEntries(context.Background(), outside)
	if err == nil {
		t.Fatal("want error for an absolute path outside the allowed root, got nil")
	}
	wantCode(t, err, "PATH_OUTSIDE_ALLOWED_ROOTS")
}

func TestListAllowedRootEntries_RejectsSymlinkEscape(t *testing.T) {
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
	_, err := mgr.ListAllowedRootEntries(context.Background(), link)
	if err == nil {
		t.Fatal("ListAllowedRootEntries(symlink escaping root): want error, got nil")
	}
	wantCode(t, err, "PATH_OUTSIDE_ALLOWED_ROOTS")
}

// TestListAllowedRootEntries_NoRootsConfiguredFallsBackToHomeDirectory is
// Checkpoint 8P-E.4's explicit behavior change from the pre-8P-E.4
// NO_ALLOWED_ROOTS_CONFIGURED error: a local web install with no
// AO_PROJECT_ROOTS set must still get a working graphical folder browser,
// scoped to the OS user's own home directory -- the same trust level the
// desktop app already assumes for that user, not unrestricted filesystem
// browsing.
func TestListAllowedRootEntries_NoRootsConfiguredFallsBackToHomeDirectory(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no resolvable home directory in this environment: %v", err)
	}
	mgr := newManagerWithRoots(t, nil)

	top, err := mgr.ListAllowedRootEntries(context.Background(), "")
	if err != nil {
		t.Fatalf("ListAllowedRootEntries: %v", err)
	}
	if top.Path != home && top.Path != resolveSymlinks(t, home) {
		t.Errorf("top-level Path = %q, want the home directory %q", top.Path, home)
	}

	// Still bounded: an absolute path outside the home directory is
	// rejected exactly like an outside-root path would be with explicit
	// AO_PROJECT_ROOTS configured.
	outside := t.TempDir()
	if strings.HasPrefix(comparablePathForTest(t, outside), comparablePathForTest(t, home)) {
		t.Skip("test tempdir happens to live under home; cannot exercise the outside-home case here")
	}
	if _, err := mgr.ListAllowedRootEntries(context.Background(), outside); err == nil {
		t.Fatal("want error for a path outside the home-directory fallback root, got nil")
	} else {
		wantCode(t, err, "PATH_OUTSIDE_ALLOWED_ROOTS")
	}
}

func comparablePathForTest(t *testing.T, path string) string {
	t.Helper()
	clean := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		clean = resolved
	}
	return clean
}

func resolveSymlinks(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return resolved
}
