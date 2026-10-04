package projectmemory_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// boundary_test.go — Frente 3 / 3B: project memory's repository boundary.
//
// Before 3B the incremental path read through os.Stat + os.ReadFile (which
// follow symlinks), the full walk opened `.env` and key files to hash them, and
// the walk took whatever the directory held. These tests pin the new contract:
// memory derives from the project's eligible files, read without ever
// following a link, and never opens a secret.

const outsideCanary = "OUTSIDE-SECRET-CANARY-3B"

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func assertNoCanary(t *testing.T, f *fixture, repoID string, canaries ...string) {
	t.Helper()
	items, err := f.store.ListProjectMemoryItems(f.ctx, testProject, repoID)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		for _, c := range canaries {
			if strings.Contains(it.Content, c) || strings.Contains(it.Summary, c) {
				t.Fatalf("item %s carries canary %q", it.Key, c)
			}
		}
	}
}

// The exact G-B3 scenario from 3A: a committed CLAUDE.md that is a symlink to a
// file outside the checkout, named in an incremental diff. It must not be read.
func TestIncrementalUpdateNeverFollowsASymlinkedInstructionFile(t *testing.T) {
	f := newFixture(t)
	root := goRepo(t)
	first := indexed(t, f, root, "c1")

	outside := t.TempDir()
	secret := filepath.Join(outside, "credentials")
	if err := os.WriteFile(secret, []byte("aws_secret_access_key = "+outsideCanary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, shape := range []struct{ name, link, target string }{
		{"file", "CLAUDE.md", secret},
		{"relative-escape", "AGENTS.md", filepath.Join("..", filepath.Base(outside), "credentials")},
	} {
		t.Run(shape.name, func(t *testing.T) {
			link := filepath.Join(root, shape.link)
			_ = os.Remove(link)
			if err := os.Symlink(shape.target, link); err != nil {
				t.Fatal(err)
			}
			out, err := f.idx.UpdateChanged(f.ctx, projectmemory.UpdateRequest{
				ProjectID: testProject, RepoPath: root, ToCommit: "c2-" + shape.name, Branch: "main",
				Changes: []projectmemory.PathChange{{Kind: projectmemory.ChangeModified, Path: shape.link}},
			})
			if err != nil {
				t.Fatalf("update: %v", err)
			}
			if out.FellBackToFullIndex {
				t.Fatalf("fell back to a full index: %s", out.FallbackReason)
			}
			assertNoCanary(t, f, first.RepoID, outsideCanary)
			if _, found, err := f.store.GetProjectMemoryFile(f.ctx, testProject, first.RepoID, shape.link); err != nil || found {
				t.Fatalf("a symlinked path is recorded in the ledger (found=%v err=%v)", found, err)
			}
		})
	}
	// A symlinked DIRECTORY on the way to the file is refused too.
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.idx.UpdateChanged(f.ctx, projectmemory.UpdateRequest{
		ProjectID: testProject, RepoPath: root, ToCommit: "c3", Branch: "main",
		Changes: []projectmemory.PathChange{{Kind: projectmemory.ChangeAdded, Path: "linked/credentials"}},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	assertNoCanary(t, f, first.RepoID, outsideCanary)
}

// A full pass never OPENS a secret file -- not even to hash it into the ledger.
func TestFullIndexNeverRecordsOrReadsSecretFiles(t *testing.T) {
	f := newFixture(t)
	root := goRepo(t)
	writeTree(t, root, map[string]string{
		".env":              "DATABASE_URL=postgres://u:" + outsideCanary + "@db/app\n",
		".env.example":      "DATABASE_URL=\n",
		"deploy/server.pem": "-----BEGIN PRIVATE KEY-----\n" + outsideCanary + "\n",
		"secrets/prod.yaml": "token: " + outsideCanary + "\n",
		".aws/credentials":  "aws_secret_access_key=" + outsideCanary + "\n",
	})
	out := indexed(t, f, root, "c1")

	files, err := f.store.ListProjectMemoryFiles(f.ctx, testProject, out.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range files {
		for _, denied := range []string{".env", "server.pem", "secrets/", ".aws/"} {
			if strings.Contains(rec.Path, denied) {
				t.Fatalf("secret path %s was opened and recorded in the ledger", rec.Path)
			}
		}
	}
	assertNoCanary(t, f, out.RepoID, outsideCanary)
}

// The MEDUSA pattern for memory: a linked worktree under .claude/worktrees in a
// git project contributes nothing, and neither does untracked scratch.
func TestMemoryIndexesOnlyTheProjectsTrackedFiles(t *testing.T) {
	f := newFixture(t)
	root := goRepo(t)
	writeTree(t, root, map[string]string{".gitignore": ".claude/\n"})
	gitRun(t, root, "init", "-q", "-b", "main")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-q", "-m", "init")
	gitRun(t, root, "worktree", "add", "-q", ".claude/worktrees/roc-capacity-fe", "-b", "agent/roc")
	wt := filepath.Join(root, ".claude", "worktrees", "roc-capacity-fe")
	writeTree(t, wt, map[string]string{"docs/architecture.md": "# Architecture\n\nWORKTREE-ONLY-CANARY\n"})
	writeTree(t, root, map[string]string{"NOTES.md": "# Notes\n\nUNTRACKED-CANARY\n"})

	out := indexed(t, f, root, "c1")
	files, err := f.store.ListProjectMemoryFiles(f.ctx, testProject, out.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range files {
		if strings.HasPrefix(rec.Path, ".claude/") || rec.Path == "NOTES.md" {
			t.Fatalf("ineligible path in the memory ledger: %s", rec.Path)
		}
	}
	assertNoCanary(t, f, out.RepoID, "WORKTREE-ONLY-CANARY", "UNTRACKED-CANARY")
	items, err := f.store.ListProjectMemoryItems(f.ctx, testProject, out.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		for _, p := range it.SourcePaths {
			if strings.Contains(p, ".claude/") {
				t.Fatalf("item %s is sourced from an agent worktree: %v", it.Key, it.SourcePaths)
			}
		}
	}
}

// Drift: a source file that has been replaced by a symlink is not evidence any
// more. The item is invalidated (fail closed), and the link is never followed.
func TestDriftInvalidatesAFactWhoseSourceBecameASymlink(t *testing.T) {
	f := newFixture(t)
	root := goRepo(t)
	out := indexed(t, f, root, "c1")

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "doc.md"), []byte("# Architecture\n\nThe daemon owns all state.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	arch := filepath.Join(root, "docs", "architecture.md")
	if err := os.Remove(arch); err != nil {
		t.Fatal(err)
	}
	// Same bytes, reached through a link: a content-only check would confirm it.
	if err := os.Symlink(filepath.Join(outside, "doc.md"), arch); err != nil {
		t.Fatal(err)
	}
	report, err := projectmemory.NewDetector(f.store).Check(f.ctx, projectmemory.DriftRequest{
		ProjectID: testProject, RepoPath: root, Commit: "c2", Apply: true,
	})
	if err != nil {
		t.Fatalf("drift: %v", err)
	}
	_ = report
	items, err := f.store.ListProjectMemoryItems(f.ctx, testProject, out.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	checked := false
	for _, it := range items {
		for _, p := range it.SourcePaths {
			if p == "docs/architecture.md" {
				checked = true
				if it.State == domain.MemoryStateValid {
					t.Fatalf("item %s is still valid although its source is now a symlink", it.Key)
				}
			}
		}
	}
	if !checked {
		t.Fatal("fixture broken: no item is sourced from docs/architecture.md")
	}
}
