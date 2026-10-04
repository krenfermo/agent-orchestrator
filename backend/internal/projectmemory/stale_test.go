package projectmemory

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeGit answers the two reachability questions from a fixed picture of a
// repository, so the staleness rules can be tested without a checkout.
type fakeGit struct {
	head string
	// known maps a revision to the commit it resolves to. A revision that is
	// absent resolves with an error, which is git's answer for a commit that
	// was rewritten away.
	known map[string]string
	// reachable lists the commits reachable from head.
	reachable map[string]bool
	// ancestryErr, when set, is returned by IsAncestor: an ancestry question
	// AO could not answer must never be reported as a clean verdict.
	ancestryErr error
}

func (f fakeGit) ResolveCommit(_ context.Context, _, rev string) (string, error) {
	if rev == "HEAD" {
		return f.head, nil
	}
	if sha, ok := f.known[rev]; ok {
		return sha, nil
	}
	return "", fmt.Errorf("unknown revision %q", rev)
}

func (f fakeGit) IsAncestor(_ context.Context, _, ancestor, _ string) (bool, error) {
	if f.ancestryErr != nil {
		return false, f.ancestryErr
	}
	return f.reachable[ancestor], nil
}

func staleCheckFor(t *testing.T, git CommitResolver, repo string) StaleCheck {
	t.Helper()
	return StaleCheck{Repo: repo, Git: git, Hash: HashFile}
}

func TestEvaluateCommitReachability(t *testing.T) {
	repo := t.TempDir()
	git := fakeGit{
		head:      "head000000000000000000000000000000000000",
		known:     map[string]string{"live0000000000000000000000000000000000000": "live0000000000000000000000000000000000000", "orphan00000000000000000000000000000000000": "orphan00000000000000000000000000000000000"},
		reachable: map[string]bool{"live0000000000000000000000000000000000000": true},
	}
	check := staleCheckFor(t, git, repo)
	ctx := context.Background()

	fresh, err := check.Evaluate(ctx, MemoryItem{SourceCommit: "live0000000000000000000000000000000000000"})
	if err != nil {
		t.Fatalf("Evaluate (reachable): %v", err)
	}
	if fresh.Stale {
		t.Fatalf("a commit reachable from HEAD must stay fresh: %+v", fresh)
	}

	orphaned, err := check.Evaluate(ctx, MemoryItem{SourceCommit: "orphan00000000000000000000000000000000000"})
	if err != nil {
		t.Fatalf("Evaluate (unreachable): %v", err)
	}
	if !orphaned.Stale || !strings.Contains(orphaned.Reason, "no longer reachable from HEAD") {
		t.Fatalf("an unreachable commit must go stale with a reason: %+v", orphaned)
	}

	rewritten, err := check.Evaluate(ctx, MemoryItem{SourceCommit: "gone0000000000000000000000000000000000000"})
	if err != nil {
		t.Fatalf("Evaluate (rewritten): %v", err)
	}
	if !rewritten.Stale || !strings.Contains(rewritten.Reason, "not present") {
		t.Fatalf("a commit the repository cannot name must go stale: %+v", rewritten)
	}

	broken := staleCheckFor(t, fakeGit{head: git.head, known: git.known, ancestryErr: fmt.Errorf("boom")}, repo)
	if _, err := broken.Evaluate(ctx, MemoryItem{SourceCommit: "live0000000000000000000000000000000000000"}); err == nil {
		t.Fatal("an ancestry question git could not answer must be an error, not a verdict")
	}
}

func TestEvaluateFileHash(t *testing.T) {
	repo := t.TempDir()
	rel := filepath.Join("backend", "internal", "thing.go")
	full := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte("package thing\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	sum, err := HashFile(full)
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}

	git := fakeGit{head: "head", known: map[string]string{"c1": "c1"}, reachable: map[string]bool{"c1": true}}
	check := staleCheckFor(t, git, repo)
	item := MemoryItem{
		SourceCommit: "c1",
		Source:       Source{Kind: SourceBaselineEvidence, Path: filepath.ToSlash(rel), FileHash: sum},
	}
	ctx := context.Background()

	verdict, err := check.Evaluate(ctx, item)
	if err != nil {
		t.Fatalf("Evaluate (unchanged file): %v", err)
	}
	if verdict.Stale {
		t.Fatalf("an unchanged file must stay fresh: %+v", verdict)
	}

	if err := os.WriteFile(full, []byte("package thing\n\n// edited\n"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	verdict, err = check.Evaluate(ctx, item)
	if err != nil {
		t.Fatalf("Evaluate (changed file): %v", err)
	}
	if !verdict.Stale || !strings.Contains(verdict.Reason, "changed") {
		t.Fatalf("a changed file must invalidate the item: %+v", verdict)
	}

	if err := os.Remove(full); err != nil {
		t.Fatalf("remove: %v", err)
	}
	verdict, err = check.Evaluate(ctx, item)
	if err != nil {
		t.Fatalf("Evaluate (deleted file): %v", err)
	}
	if !verdict.Stale || !strings.Contains(verdict.Reason, "no longer exists") {
		t.Fatalf("a deleted file must invalidate the item: %+v", verdict)
	}

	noHash := MemoryItem{SourceCommit: "c1", Source: Source{Kind: SourceManual, Path: filepath.ToSlash(rel)}}
	verdict, err = check.Evaluate(ctx, noHash)
	if err != nil {
		t.Fatalf("Evaluate (no recorded hash): %v", err)
	}
	if verdict.Stale {
		t.Fatalf("with no recorded hash there is nothing to compare, got %+v", verdict)
	}
}

func TestRefreshStalenessPersistsVerdictsWithoutTouchingUpdatedAt(t *testing.T) {
	repo := t.TempDir()
	store, err := NewStore(t.TempDir(), WithClock(fixedClock(time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC))))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ctx := context.Background()

	item := sampleItem("proj-a", "note-1", "derived at c1")
	item.SourceCommit = "c1"
	created, err := store.Upsert(ctx, item)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	stored := created.Items[0]

	reachable := fakeGit{head: "head", known: map[string]string{"c1": "c1"}, reachable: map[string]bool{"c1": true}}
	fresh, err := store.RefreshStaleness(ctx, "proj-a", staleCheckFor(t, reachable, repo))
	if err != nil {
		t.Fatalf("RefreshStaleness (fresh): %v", err)
	}
	if fresh.MarkedStale != 0 || fresh.Path != "" {
		t.Fatalf("nothing changed, so nothing should have been written: %+v", fresh)
	}

	orphaned := fakeGit{head: "head", known: map[string]string{"c1": "c1"}, reachable: map[string]bool{}}
	marked, err := store.RefreshStaleness(ctx, "proj-a", staleCheckFor(t, orphaned, repo))
	if err != nil {
		t.Fatalf("RefreshStaleness (orphaned): %v", err)
	}
	if marked.MarkedStale != 1 || marked.Path == "" {
		t.Fatalf("the orphaned item should have been marked and persisted: %+v", marked)
	}

	items, err := store.List("proj-a")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 || !items[0].Stale || items[0].StaleReason == "" {
		t.Fatalf("staleness was not persisted: %+v", items)
	}
	if !items[0].UpdatedAt.Equal(stored.UpdatedAt) {
		t.Fatalf("a staleness sweep must not move UpdatedAt: %s -> %s", stored.UpdatedAt, items[0].UpdatedAt)
	}

	cleared, err := store.RefreshStaleness(ctx, "proj-a", staleCheckFor(t, reachable, repo))
	if err != nil {
		t.Fatalf("RefreshStaleness (cleared): %v", err)
	}
	if cleared.Cleared != 1 {
		t.Fatalf("a commit that is reachable again must clear staleness: %+v", cleared)
	}
	items, err = store.List("proj-a")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if items[0].Stale || items[0].StaleReason != "" {
		t.Fatalf("stale annotation survived a passing check: %+v", items[0])
	}
}

func TestStaleCheckRequiresAnAbsoluteRepo(t *testing.T) {
	check := StaleCheck{Repo: "relative/repo", Git: fakeGit{head: "head"}}
	if _, err := check.Evaluate(context.Background(), MemoryItem{}); err == nil {
		t.Fatal("a relative repository path must be rejected")
	}
}

// TestStalenessAgainstRealGit exercises the same rules through AO's real
// read-only git runner, so the fake above cannot drift from git's behaviour.
func TestStalenessAgainstRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=ao", "GIT_AUTHOR_EMAIL=ao@example.test",
			"GIT_COMMITTER_NAME=ao", "GIT_COMMITTER_EMAIL=ao@example.test",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	run("init", "-q", "-b", "main")
	write("doc.md", "first\n")
	run("add", "doc.md")
	run("commit", "-q", "-m", "first")
	base := run("rev-parse", "HEAD")

	write("doc.md", "second\n")
	run("add", "doc.md")
	run("commit", "-q", "-m", "second")
	second := run("rev-parse", "HEAD")

	hash, err := HashFile(filepath.Join(repo, "doc.md"))
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}
	item := MemoryItem{
		Project:      "proj-a",
		Type:         TypeNote,
		Content:      "doc.md says second",
		Source:       Source{Kind: SourceManual, Path: "doc.md", FileHash: hash},
		SourceCommit: second,
		Confidence:   0.9,
	}

	check := NewGitCheck(repo)
	ctx := context.Background()
	verdict, err := check.Evaluate(ctx, item)
	if err != nil {
		t.Fatalf("Evaluate (current commit): %v", err)
	}
	if verdict.Stale {
		t.Fatalf("an item derived at HEAD must be fresh: %+v", verdict)
	}

	// Drop the commit the item was derived at: the branch is reset back, so
	// the commit still exists as an object but is no longer reachable.
	run("reset", "-q", "--hard", base)
	verdict, err = check.Evaluate(ctx, item)
	if err != nil {
		t.Fatalf("Evaluate (after reset): %v", err)
	}
	if !verdict.Stale || !strings.Contains(verdict.Reason, "no longer reachable from HEAD") {
		t.Fatalf("after the branch was reset the item must be stale: %+v", verdict)
	}

	// Restore reachability and change only the file: the other invalidation
	// path has to fire on its own.
	run("reset", "-q", "--hard", second)
	write("doc.md", "third\n")
	verdict, err = check.Evaluate(ctx, item)
	if err != nil {
		t.Fatalf("Evaluate (after edit): %v", err)
	}
	if !verdict.Stale || !strings.Contains(verdict.Reason, "changed") {
		t.Fatalf("an edited source file must invalidate the item: %+v", verdict)
	}
}
