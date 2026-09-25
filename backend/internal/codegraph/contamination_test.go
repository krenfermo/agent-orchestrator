package codegraph

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// contamination_test.go — Frente 3 / 3B permanent regression for the pattern
// observed in production on MEDUSA: 4,265 of 17,242 served symbols (24.7%)
// came from `.claude/worktrees/roc-capacity-fe/`, a linked git worktree an
// agent had checked out inside the project. The code graph's walk did not
// skip `.claude/`, so the copy was indexed as the project's own code.
//
// Before 3B, the same two fixtures below indexed the worktree's symbols (shown
// against the baseline commit, see docs/frente3/3b-implementation.md). The
// graph is now built from the project's TRACKED files in a git repository,
// and from a walk that refuses nested checkouts otherwise.

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func assertNoWorktreeSymbols(t *testing.T, ids []string) {
	t.Helper()
	for _, id := range ids {
		if strings.Contains(id, ".claude/") || strings.Contains(id, "LeakedFromWorktree") {
			t.Fatalf("a symbol from an agent worktree was indexed into the project: %q (all: %v)", id, ids)
		}
	}
}

// The MEDUSA shape exactly: a git project, `.claude/` ignored, and a real
// linked worktree under `.claude/worktrees/` whose branch carries extra code.
func TestLinkedAgentWorktreeIsNeverIndexedIntoTheProject(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "src/app.ts", "export function appEntry() { return 1 }\n")
	writeFile(t, root, ".gitignore", ".claude/\n")
	gitCmd(t, root, "init", "-q", "-b", "main")
	gitCmd(t, root, "add", ".")
	gitCmd(t, root, "commit", "-q", "-m", "init")
	gitCmd(t, root, "worktree", "add", "-q", ".claude/worktrees/roc-capacity-fe", "-b", "agent/roc")
	wt := filepath.Join(root, ".claude", "worktrees", "roc-capacity-fe")
	writeFile(t, wt, "src/capacity.ts", "export function LeakedFromWorktree() { return 2 }\n")
	gitCmd(t, wt, "add", ".")
	gitCmd(t, wt, "commit", "-q", "-m", "agent work")

	indexer := newIndexer(t)
	if _, err := indexer.Index(context.Background(), IndexRequest{ProjectRoot: root}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	ids := symbolIDs(t, indexer, root)
	assertNoWorktreeSymbols(t, ids)
	found := false
	for _, id := range ids {
		if id == "src/app.ts#function:appEntry" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the project's own tracked symbol is missing: %v", ids)
	}

	// An incremental update whose diff names a worktree path (a crafted or
	// confused diff) must not bring it in either.
	if _, err := indexer.IncrementalUpdate(context.Background(), UpdateRequest{
		ProjectRoot: root,
		Diff:        Diff{Changes: []FileChange{{Status: ChangeAdded, Path: ".claude/worktrees/roc-capacity-fe/src/capacity.ts"}}},
	}); err != nil {
		t.Fatalf("IncrementalUpdate naming an excluded path should skip it, got %v", err)
	}
	assertNoWorktreeSymbols(t, symbolIDs(t, indexer, root))
}

// The same shape without git: a project directory that is not a repository,
// holding a copy with its own `.git` FILE (how a linked worktree looks).
func TestNestedCheckoutIsNeverIndexedWithoutGit(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plain")
	writeFile(t, root, "src/app.ts", "export function appEntry() { return 1 }\n")
	writeFile(t, root, ".claude/worktrees/foo/.git", "gitdir: /elsewhere\n")
	writeFile(t, root, ".claude/worktrees/foo/src/capacity.ts", "export function LeakedFromWorktree() {}\n")
	// A nested checkout under an innocent name: caught by its .git, not its name.
	writeFile(t, root, "tools/copy/.git", "gitdir: /elsewhere\n")
	writeFile(t, root, "tools/copy/src/capacity.ts", "export function LeakedFromWorktree() {}\n")

	indexer := newIndexer(t)
	if _, err := indexer.Index(context.Background(), IndexRequest{ProjectRoot: root}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	ids := symbolIDs(t, indexer, root)
	assertNoWorktreeSymbols(t, ids)
	if len(ids) != 1 || ids[0] != "src/app.ts#function:appEntry" {
		t.Fatalf("symbols = %v, want only the project's own", ids)
	}
}

// Untracked files in a git project are not the project's code either.
func TestUntrackedFilesAreNotIndexedInAGitProject(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "main.go", "package main\n\nfunc Tracked() {}\n")
	gitCmd(t, root, "init", "-q", "-b", "main")
	gitCmd(t, root, "add", ".")
	gitCmd(t, root, "commit", "-q", "-m", "init")
	writeFile(t, root, "scratch.go", "package main\n\nfunc UntrackedScratch() {}\n")

	indexer := newIndexer(t)
	if _, err := indexer.Index(context.Background(), IndexRequest{ProjectRoot: root}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	for _, id := range symbolIDs(t, indexer, root) {
		if strings.Contains(id, "UntrackedScratch") {
			t.Fatalf("untracked file indexed: %s", id)
		}
	}
}
