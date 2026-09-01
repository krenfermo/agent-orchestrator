package projectmemory_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// worktree_isolation_test.go — P2-E A7.
//
// The claim, in one sentence: **a repository has canonical memory and a
// worktree never does.** The P2-D production gate found the opposite — one
// caller passing a workspace path minted a second canonical repo_id per
// reviewed task, full-indexed an unintegrated branch into it, and those facts
// then dominated what the router returned for the whole project.
//
// These tests use REAL git worktrees rather than a stub, because the guard's
// signal is a git fact (`--git-dir` differs from `--git-common-dir` in a linked
// worktree) and a stub would let the guard pass while production still failed.

// gitWorktreeFixture builds a real repository with a real linked worktree and
// returns both paths.
func gitWorktreeFixture(t *testing.T) (repo, worktree string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	base := t.TempDir()
	repo = filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o750); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_EDITOR=true",
			"GIT_AUTHOR_NAME=ao", "GIT_AUTHOR_EMAIL=ao@example.test",
			"GIT_COMMITTER_NAME=ao", "GIT_COMMITTER_EMAIL=ao@example.test")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	run(repo, "init", "-q", "-b", "main")
	writeTree(t, repo, map[string]string{
		"go.mod":                  "module example.com/app\n\ngo 1.24\n",
		"README.md":               "# App\n\nA small service.\n",
		"internal/store/store.go": "package store\n\nfunc Open() {}\n",
	})
	run(repo, "add", ".")
	run(repo, "commit", "-q", "-m", "initial")

	worktree = filepath.Join(base, "wt")
	run(repo, "worktree", "add", "-q", "-b", "ao/task-1", worktree)
	return repo, worktree
}

// TestLinkedWorktreeIsRecognisedAndRepositoryIsNot pins the signal the whole
// guard rests on, against real git.
func TestLinkedWorktreeIsRecognisedAndRepositoryIsNot(t *testing.T) {
	repo, worktree := gitWorktreeFixture(t)
	ctx := context.Background()

	if _, linked := projectmemory.LinkedWorktreeOf(ctx, repo); linked {
		t.Fatal("a repository's own working tree was classified as a linked worktree")
	}
	parent, linked := projectmemory.LinkedWorktreeOf(ctx, worktree)
	if !linked {
		t.Fatal("a real linked worktree was not recognised")
	}
	if !projectmemory.SameRepoPath(parent, repo) {
		t.Fatalf("worktree parent = %q, want the repository %q", parent, repo)
	}

	// A directory that is not a git checkout at all is not a worktree, and
	// must stay indexable -- AO supports projects that are not repositories.
	if _, linked := projectmemory.LinkedWorktreeOf(ctx, t.TempDir()); linked {
		t.Fatal("a plain directory was classified as a linked worktree")
	}
}

// TestWorktreeNeverBecomesASecondCanonicalRepository is the headline A7 test:
// one repository, two tasks, still exactly one canonical memory.
func TestWorktreeNeverBecomesASecondCanonicalRepository(t *testing.T) {
	f := newFixture(t)
	repo, worktree := gitWorktreeFixture(t)
	svc := projectmemory.NewService(f.store)
	syncer := projectmemory.NewSyncer(svc, assistedConfig())

	// The repository itself indexes normally.
	if fresh := syncer.EnsureFresh(f.ctx, testProject, repo); !fresh.Healthy() {
		t.Fatalf("the repository did not index: %+v", fresh)
	}
	indexCount := func() int {
		states, err := f.store.ListProjectMemoryIndexStates(f.ctx, testProject)
		if err != nil {
			t.Fatal(err)
		}
		return len(states)
	}
	if got := indexCount(); got != 1 {
		t.Fatalf("%d index rows after indexing one repository, want 1", got)
	}

	// Now the case that produced the blocker: something asks for freshness
	// against the task's worktree. It must be refused, and must leave no trace.
	fresh := syncer.EnsureFresh(f.ctx, testProject, worktree)
	if fresh.Kind != projectmemory.SyncSkipped {
		t.Fatalf("a worktree was indexed as a repository: %+v", fresh)
	}
	if !strings.Contains(fresh.Reason, "linked worktree") {
		t.Fatalf("the refusal does not say why: %q", fresh.Reason)
	}
	if got := indexCount(); got != 1 {
		t.Fatalf("%d index rows after a worktree freshness check, want 1", got)
	}

	// A second task's worktree changes nothing either.
	second := filepath.Join(t.TempDir(), "wt2")
	cmd := exec.Command("git", "-C", repo, "worktree", "add", "-q", "-b", "ao/task-2", second)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("second worktree: %v: %s", err, out)
	}
	syncer.EnsureFresh(f.ctx, testProject, second)
	if got := indexCount(); got != 1 {
		t.Fatalf("%d index rows after two task worktrees, want 1", got)
	}
	if n := syncer.Stats().WorktreeRefused; n != 2 {
		t.Fatalf("WorktreeRefused = %d, want 2", n)
	}

	// And no canonical repo_derivation fact exists that was derived from
	// either branch -- the concrete harm the gate measured.
	items, err := f.store.ListProjectMemoryItemsForProject(f.ctx, testProject)
	if err != nil {
		t.Fatal(err)
	}
	repoID := domain.ProjectMemoryRepoID(mustCanonical(t, repo))
	for _, item := range items {
		if item.Key.RepoID != repoID {
			t.Fatalf("a fact was filed under a non-repository memory %q: %s", item.Key.RepoID, item.Summary)
		}
	}
	if len(items) == 0 {
		t.Fatal("the repository produced no facts, so this test proves nothing")
	}
}

// TestDirectBranchProjectStillIndexes is the regression guard on the guard: a
// project whose execution mode is direct branch works in its own root, and the
// worktree refusal must not touch it.
func TestDirectBranchProjectStillIndexes(t *testing.T) {
	f := newFixture(t)
	repo, _ := gitWorktreeFixture(t)
	svc := projectmemory.NewService(f.store)
	syncer := projectmemory.NewSyncer(svc, assistedConfig())

	first := syncer.EnsureFresh(f.ctx, testProject, repo)
	if !first.Healthy() || first.Kind == projectmemory.SyncSkipped {
		t.Fatalf("direct-branch indexing was refused: %+v", first)
	}
	// And the warm path still works on the second call.
	second := syncer.EnsureFresh(f.ctx, testProject, repo)
	if second.Kind != projectmemory.SyncNone || second.FilesRead != 0 {
		t.Fatalf("the warm path regressed: %+v", second)
	}
}

// TestMultiRepoProjectKeepsOneIndexPerRealRepository is A6: the rule is
// repository != worktree, NOT project == one repository.
func TestMultiRepoProjectKeepsOneIndexPerRealRepository(t *testing.T) {
	f := newFixture(t)
	repoA, worktreeA := gitWorktreeFixture(t)
	repoB, _ := gitWorktreeFixture(t)
	svc := projectmemory.NewService(f.store)
	syncer := projectmemory.NewSyncer(svc, assistedConfig())

	for _, repo := range []string{repoA, repoB} {
		if fresh := syncer.EnsureFresh(f.ctx, testProject, repo); !fresh.Healthy() {
			t.Fatalf("repository %s did not index: %+v", repo, fresh)
		}
	}
	syncer.EnsureFresh(f.ctx, testProject, worktreeA)

	states, err := f.store.ListProjectMemoryIndexStates(f.ctx, testProject)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 {
		paths := make([]string, 0, len(states))
		for _, st := range states {
			paths = append(paths, st.RepoPath)
		}
		t.Fatalf("%d index rows, want one per REAL repository (2): %v", len(states), paths)
	}
}

// TestWorkspaceRewrittenFactsAreWithheldFromTheirOwnReader is A4.
//
// A canonical file summary describes the file as the repository has it. Once
// the reader's own task has rewritten that file in its worktree, serving the
// summary would describe a version nobody is looking at -- so it is withheld
// and the reader falls back to the working tree. A fact derived from several
// files of which the task touched one is NOT withheld: it is still
// substantially true and still the cheapest thing the reader has.
func TestWorkspaceRewrittenFactsAreWithheldFromTheirOwnReader(t *testing.T) {
	f, svc, root, _ := packService(t)

	withoutExclusion := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleReviewer,
		ChangedPaths: []string{"cmd/app/main.go"},
	})
	names := packItemKeys(withoutExclusion)
	if !containsPath(names, "cmd/app/main.go") {
		t.Fatalf("the fixture did not select a fact about the changed file: %v", names)
	}

	withExclusion := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleReviewer,
		ChangedPaths:     []string{"cmd/app/main.go"},
		TaskChangedPaths: []string{"cmd/app/main.go"},
	})
	if withExclusion.Stats.WorkspaceRewrittenExcluded == 0 {
		t.Fatalf("nothing was withheld for a file the task rewrote: %+v", withExclusion.Stats)
	}
	for _, section := range withExclusion.Sections {
		for _, sel := range section.Items {
			if len(sel.Item.SourcePaths) == 1 && sel.Item.SourcePaths[0] == "cmd/app/main.go" {
				t.Fatalf("a canonical summary of a rewritten file was still served: %s", sel.Item.Summary)
			}
		}
	}
	// The module fact, derived from that file AND others, survives.
	if withExclusion.Stats.SelectedItems == 0 {
		t.Fatal("excluding one file's summary emptied the pack")
	}
}

func packItemKeys(pack projectmemory.ContextPack) []string {
	var out []string
	for _, section := range pack.Sections {
		for _, sel := range section.Items {
			out = append(out, sel.Item.SourcePaths...)
		}
	}
	return out
}

func mustCanonical(t *testing.T, p string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("resolve %s: %v", p, err)
	}
	return resolved
}

// assistedConfig is DefaultConfig with memory switched on. The default is off,
// which is correct for production and useless for a test of what memory does.
func assistedConfig() projectmemory.Config {
	cfg := projectmemory.DefaultConfig()
	cfg.Mode = projectmemory.ModeAssisted
	return cfg
}
