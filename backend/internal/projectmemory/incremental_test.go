package projectmemory_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// incremental_test.go — the change-set path, which is where the actual
// incrementality lives.
//
// The property every test here defends is the same one: an update must touch
// what the change set names and nothing else. A pass that quietly invalidates
// unrelated memory is indistinguishable, from the outside, from one that
// rescans — and the completion bar for P2-A says a rescan must not be the
// normal path.

func indexed(t *testing.T, f *fixture, root, commit string) projectmemory.IndexOutcome {
	t.Helper()
	out, err := f.idx.Index(f.ctx, projectmemory.IndexRequest{
		ProjectID: testProject, RepoPath: root, Commit: commit, Branch: "main",
	})
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	return out
}

func TestUpdateChangedTouchesOnlyTheChangedPath(t *testing.T) {
	f := newFixture(t)
	root := goRepo(t)
	first := indexed(t, f, root, "c1")

	writeTree(t, root, map[string]string{
		"docs/architecture.md": "# Architecture\n\nThe daemon owns all state, and now also the queue.\n",
	})
	out, err := f.idx.UpdateChanged(f.ctx, projectmemory.UpdateRequest{
		ProjectID: testProject, RepoPath: root, ToCommit: "c2", Branch: "main",
		Changes: []projectmemory.PathChange{
			{Kind: projectmemory.ChangeModified, Path: "docs/architecture.md"},
		},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if out.FellBackToFullIndex {
		t.Fatalf("incremental update fell back to a full index: %s", out.FallbackReason)
	}
	if out.PathsRead != 1 {
		t.Fatalf("read %d paths for a one-file change", out.PathsRead)
	}

	items, err := f.store.ListProjectMemoryItems(f.ctx, testProject, first.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		switch it.Key.Key {
		case "docs/architecture.md":
			if it.State != domain.MemoryStateValid {
				t.Fatalf("the re-derived fact is %s, want valid", it.State)
			}
			if it.SourceCommit != "c2" {
				t.Fatalf("the re-derived fact still carries commit %q", it.SourceCommit)
			}
			if !strings.Contains(it.Content, "also the queue") {
				t.Fatal("the re-derived fact does not carry the new content")
			}
		case "AGENTS.md":
			if it.State != domain.MemoryStateValid {
				t.Fatalf("an untouched fact was moved to %s by an unrelated change", it.State)
			}
		}
	}
}

func TestUpdateChangedHandlesDeletionAndRename(t *testing.T) {
	f := newFixture(t)
	root := goRepo(t)
	first := indexed(t, f, root, "c1")

	if err := os.Remove(filepath.Join(root, "docs", "architecture.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(
		filepath.Join(root, "internal", "store", "query.go"),
		filepath.Join(root, "internal", "store", "queries.go"),
	); err != nil {
		t.Fatal(err)
	}

	out, err := f.idx.UpdateChanged(f.ctx, projectmemory.UpdateRequest{
		ProjectID: testProject, RepoPath: root, ToCommit: "c2",
		Changes: []projectmemory.PathChange{
			{Kind: projectmemory.ChangeDeleted, Path: "docs/architecture.md"},
			{Kind: projectmemory.ChangeRenamed, PreviousPath: "internal/store/query.go", Path: "internal/store/queries.go"},
		},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if out.ItemsInvalidated == 0 {
		t.Fatal("the deletion invalidated nothing")
	}

	ledger, err := f.store.ListProjectMemoryFiles(f.ctx, testProject, first.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, l := range ledger {
		paths[l.Path] = true
	}
	if paths["docs/architecture.md"] {
		t.Error("the deleted path is still in the digest ledger")
	}
	if paths["internal/store/query.go"] {
		t.Error("the renamed-away path is still in the digest ledger")
	}
	if !paths["internal/store/queries.go"] {
		t.Error("the rename destination never entered the ledger")
	}

	// And a following full pass must not re-discover the same deletion.
	second := indexed(t, f, root, "c3")
	if second.FilesRemoved != 0 {
		t.Fatalf("the next full pass rediscovered %d removals the update had already handled", second.FilesRemoved)
	}
}

func TestUpdateChangedRefreshesTheAffectedModuleCensus(t *testing.T) {
	f := newFixture(t)
	root := goRepo(t)
	first := indexed(t, f, root, "c1")

	writeTree(t, root, map[string]string{"internal/store/extra.go": "package store\n\nfunc Extra() {}\n"})
	if _, err := f.idx.UpdateChanged(f.ctx, projectmemory.UpdateRequest{
		ProjectID: testProject, RepoPath: root, ToCommit: "c2",
		Changes: []projectmemory.PathChange{
			{Kind: projectmemory.ChangeAdded, Path: "internal/store/extra.go"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	key := domain.ProjectMemoryKey{
		ProjectID: testProject, RepoID: first.RepoID,
		Type: domain.MemoryTypeModule, Scope: domain.MemoryScopeModule, Key: "internal/store",
	}
	mod, ok, err := f.store.GetProjectMemoryItem(f.ctx, key.ID())
	if err != nil || !ok {
		t.Fatalf("module fact: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(mod.Content, "3 files") {
		t.Fatalf("module census was not refreshed: %q", mod.Summary)
	}
}

func TestUpdateChangedFallsBackToFullIndexWithoutABaseline(t *testing.T) {
	f := newFixture(t)
	root := goRepo(t)

	out, err := f.idx.UpdateChanged(f.ctx, projectmemory.UpdateRequest{
		ProjectID: testProject, RepoPath: root, ToCommit: "c1",
		Changes: []projectmemory.PathChange{{Kind: projectmemory.ChangeModified, Path: "README.md"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.FellBackToFullIndex {
		t.Fatal("an update against an unindexed repository did not fall back to a full pass")
	}
	if out.ItemsWritten == 0 {
		t.Fatal("the fallback full pass wrote nothing")
	}
}

// TestUpdateChangedIsRefusedByANewerGeneration is the second half of the CAS
// story: the store refuses the write, and the pass reports it rather than
// pretending to have applied it.
func TestUpdateChangedRefusesToRunConcurrently(t *testing.T) {
	f := newFixture(t)
	root := goRepo(t)
	first := indexed(t, f, root, "c1")

	// Hold the repository with a claimed pass, as a live indexer would.
	if _, ok, err := f.store.ClaimProjectMemoryIndexPass(f.ctx, testProject, first.RepoID, "c2", "main", f.now()); err != nil || !ok {
		t.Fatalf("hold: ok=%v err=%v", ok, err)
	}
	out, err := f.idx.UpdateChanged(f.ctx, projectmemory.UpdateRequest{
		ProjectID: testProject, RepoPath: root, ToCommit: "c2",
		Changes: []projectmemory.PathChange{{Kind: projectmemory.ChangeModified, Path: "README.md"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Skipped {
		t.Fatal("an update ran while another pass held the repository")
	}
}

func TestUpdateChangedWithAnEmptyChangeSetIsCheapAndAdvancesTheCommit(t *testing.T) {
	f := newFixture(t)
	root := goRepo(t)
	first := indexed(t, f, root, "c1")

	out, err := f.idx.UpdateChanged(f.ctx, projectmemory.UpdateRequest{
		ProjectID: testProject, RepoPath: root, ToCommit: "c2", Branch: "other",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.PathsRead != 0 {
		t.Fatalf("a branch move that changed nothing read %d files", out.PathsRead)
	}
	status, _, err := f.store.GetProjectMemoryStatus(f.ctx, testProject, first.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Index.IndexedCommit != "c2" {
		t.Fatalf("indexed commit = %q, want the branch's new commit", status.Index.IndexedCommit)
	}
	if status.Counts.Stale != 0 || status.Counts.Invalidated != 0 {
		t.Fatalf("a no-op branch move invalidated memory: %+v", status.Counts)
	}
}

// A dependency manifest is an ordinary changed path, and that is the point: the
// facts derived from it — the dependency list and the build/test commands — are
// re-derived from the new manifest rather than left describing the old one.
func TestUpdateChangedRederivesFactsFromAChangedManifest(t *testing.T) {
	f := newFixture(t)
	root := goRepo(t)
	first := indexed(t, f, root, "c1")

	writeTree(t, root, map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.24\n\nrequire (\n" +
			"\tgithub.com/spf13/cobra v1.8.0\n\tgithub.com/stretchr/testify v1.9.0\n" +
			"\tgithub.com/go-chi/chi/v5 v5.1.0\n)\n",
	})
	if _, err := f.idx.UpdateChanged(f.ctx, projectmemory.UpdateRequest{
		ProjectID: testProject, RepoPath: root, ToCommit: "c2",
		Changes: []projectmemory.PathChange{{Kind: projectmemory.ChangeModified, Path: "go.mod"}},
	}); err != nil {
		t.Fatal(err)
	}

	key := domain.ProjectMemoryKey{
		ProjectID: testProject, RepoID: first.RepoID,
		Type: domain.MemoryTypeDependency, Scope: domain.MemoryScopeRepository, Key: "go.mod",
	}
	dep, ok, err := f.store.GetProjectMemoryItem(f.ctx, key.ID())
	if err != nil || !ok {
		t.Fatalf("dependency fact: ok=%v err=%v", ok, err)
	}
	if dep.State != domain.MemoryStateValid {
		t.Fatalf("the re-derived dependency fact is %s", dep.State)
	}
	if !strings.Contains(dep.Content, "go-chi/chi/v5") {
		t.Fatalf("the newly declared dependency is missing:\n%s", dep.Content)
	}
	if !strings.Contains(dep.Summary, "3 go dependencies") {
		t.Fatalf("summary = %q, want the new count", dep.Summary)
	}
	if dep.SourceCommit != "c2" {
		t.Fatalf("the re-derived fact carries commit %q", dep.SourceCommit)
	}
}
