package codexsandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func realDir(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// newRepo creates a repository with one commit and returns its path.
func newRepo(t *testing.T, parent, name string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "init", "-q")
	run(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	return dir
}

// testResolver protects a fake home so tests never depend on the real one.
func testResolver(t *testing.T) Resolver {
	return Resolver{Home: filepath.Join(t.TempDir(), "home")}
}

func TestAPlainWorkspaceNeedsNoExtraRoots(t *testing.T) {
	ws := t.TempDir()
	roots, err := testResolver(t).WritableRoots(context.Background(), ws, nil)
	if err != nil || len(roots) != 0 {
		t.Fatalf("roots=%v err=%v, want none", roots, err)
	}
	if roots, err := testResolver(t).WritableRoots(context.Background(), "", nil); err != nil || roots != nil {
		t.Fatalf("empty workspace: roots=%v err=%v", roots, err)
	}
}

// Codex protects a .git inside a writable root, so a direct repository's own
// .git must be an explicit root for `git commit` to work.
func TestADirectRepositoryGetsItsGitDirectory(t *testing.T) {
	repo := newRepo(t, t.TempDir(), "repo")
	roots, err := testResolver(t).WritableRoots(context.Background(), repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{realDir(t, filepath.Join(repo, ".git"))}; !slices.Equal(roots, want) {
		t.Fatalf("roots=%v, want %v", roots, want)
	}
}

// An AO worktree's index and objects live in the repository's common git
// directory, outside the worktree.
func TestAWorktreeGetsItsRepositorysCommonGitDirectory(t *testing.T) {
	base := t.TempDir()
	repo := newRepo(t, base, "main")
	wt := filepath.Join(base, "wt")
	run(t, repo, "worktree", "add", "-q", wt, "-b", "feature")
	roots, err := testResolver(t).WritableRoots(context.Background(), wt, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{realDir(t, filepath.Join(repo, ".git"))}; !slices.Equal(roots, want) {
		t.Fatalf("roots=%v, want %v", roots, want)
	}
}

// A workspace project: child repositories nested in the workspace are explicit
// roots, and each brings its own git directory; duplicates collapse.
func TestAWorkspaceWithChildRepositoriesAndDuplicates(t *testing.T) {
	ws := newRepo(t, t.TempDir(), "ws")
	childA := newRepo(t, ws, "a")
	childB := newRepo(t, ws, "b")
	roots, err := testResolver(t).WritableRoots(context.Background(), ws, []string{childA, childB, childA, " " + childB + " ", ws})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		realDir(t, childA), realDir(t, childB),
		realDir(t, filepath.Join(ws, ".git")), realDir(t, filepath.Join(childA, ".git")), realDir(t, filepath.Join(childB, ".git")),
	}
	if !slices.Equal(roots, want) {
		t.Fatalf("roots=%v\nwant %v", roots, want)
	}
}

// Fail closed: nothing is ever widened beyond the workspace and its own repos.
func TestInvalidRootsAreRefused(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	file := filepath.Join(ws, "f")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(ws, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}
	for name, dirs := range map[string][]string{
		"relative":         {"child"},
		"empty":            {"  "},
		"missing":          {filepath.Join(ws, "missing")},
		"a file":           {file},
		"outside":          {outside},
		"symlink escape":   {escape},
		"parent traversal": {filepath.Join(ws, "..")},
		"filesystem root":  {"/"},
	} {
		t.Run(name, func(t *testing.T) {
			if roots, err := testResolver(t).WritableRoots(context.Background(), ws, dirs); !errors.Is(err, ErrInvalidRoot) {
				t.Fatalf("roots=%v err=%v, want ErrInvalidRoot", roots, err)
			}
		})
	}
	if _, err := testResolver(t).WritableRoots(context.Background(), filepath.Join(ws, "missing"), nil); !errors.Is(err, ErrInvalidRoot) {
		t.Fatalf("a missing workspace err=%v, want ErrInvalidRoot", err)
	}
}

// A git directory that would expose the home directory or AO's own state is
// refused, whatever git reports.
func TestAGitDirectoryExposingProtectedStateIsRefused(t *testing.T) {
	ws := t.TempDir()
	home := t.TempDir()
	aoData := filepath.Join(home, ".ao")
	if err := os.MkdirAll(aoData, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, common := range map[string]string{"home": home, "ao state": aoData} {
		t.Run(name, func(t *testing.T) {
			r := Resolver{Home: home, GitCommonDir: func(context.Context, string) (string, bool, error) { return common, true, nil }}
			if roots, err := r.WritableRoots(context.Background(), ws, nil); !errors.Is(err, ErrInvalidRoot) {
				t.Fatalf("roots=%v err=%v, want ErrInvalidRoot", roots, err)
			}
		})
	}
	failing := Resolver{Home: home, GitCommonDir: func(context.Context, string) (string, bool, error) {
		return "", false, errors.New("git broke")
	}}
	if _, err := failing.WritableRoots(context.Background(), ws, nil); !errors.Is(err, ErrInvalidRoot) {
		t.Fatalf("a failing git query err=%v, want ErrInvalidRoot (never 'not a repository')", err)
	}
}

// Codex AR1A-FIN-03: git, not AO, decides where a git directory is. It may not
// be the home directory's own .git, nor lie inside AO's state -- except AO's
// scratch area, where AO keeps the scratch project's repository.
func TestGitDirectoryPolicy(t *testing.T) {
	ws := t.TempDir()
	home := t.TempDir()
	data := filepath.Join(home, ".ao", "data")
	for _, d := range []string{filepath.Join(home, ".git"), filepath.Join(data, "other", ".git"), filepath.Join(data, "scratch", "default", ".git"), filepath.Join(home, ".ao", "x", ".git"), filepath.Join(home, "code", "repo", ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name   string
		gitDir string
		ok     bool
	}{
		{"home's own .git", filepath.Join(home, ".git"), false},
		{"inside AO data", filepath.Join(data, "other", ".git"), false},
		{"inside ~/.ao", filepath.Join(home, ".ao", "x", ".git"), false},
		{"AO scratch repository", filepath.Join(data, "scratch", "default", ".git"), true},
		{"an ordinary repository under home", filepath.Join(home, "code", "repo", ".git"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Resolver{Home: home, AODataDir: data, GitCommonDir: func(context.Context, string) (string, bool, error) { return tc.gitDir, true, nil }}
			roots, err := r.WritableRoots(context.Background(), ws, nil)
			if tc.ok && err != nil {
				t.Fatalf("refused %q: %v", tc.gitDir, err)
			}
			if !tc.ok && !errors.Is(err, ErrInvalidRoot) {
				t.Fatalf("accepted %q as a writable root: %v", tc.gitDir, roots)
			}
		})
	}
}
