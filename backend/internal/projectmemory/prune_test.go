package projectmemory_test

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// prune_test.go — P2-E A8.
//
// The prune removes memory, which makes its REFUSALS the interesting half.
// Every test here is a way it could remove the wrong thing: the project's own
// repository, a workspace something is still using, a directory AO cannot
// identify, or the last memory a project has.

// seedRepoMemory indexes one repository so there is something to prune.
func seedRepoMemory(t *testing.T, f *fixture, svc *projectmemory.Service, repo string) string {
	t.Helper()
	syncer := projectmemory.NewSyncer(svc, assistedConfig())
	if out := syncer.EnsureFresh(f.ctx, testProject, repo); !out.Healthy() {
		t.Fatalf("seed %s: %+v", repo, out)
	}
	return domain.ProjectMemoryRepoID(mustCanonical(t, repo))
}

// seedWorktreeMemory reproduces the pre-P2-E damage: a canonical memory filed
// under a worktree path. It is written directly through the store because the
// production path can no longer create one -- which is the point of the fix,
// and means the only way to test the cleanup is to recreate the wreckage.
func seedWorktreeMemory(t *testing.T, f *fixture, worktree string) string {
	t.Helper()
	repoID := domain.ProjectMemoryRepoID(mustCanonical(t, worktree))
	if err := f.store.EnsureProjectMemoryRepo(
		f.ctx, testProject, repoID, mustCanonical(t, worktree), f.now()); err != nil {
		t.Fatal(err)
	}
	item := domain.ProjectMemoryItem{
		Key: domain.ProjectMemoryKey{
			ProjectID: testProject, RepoID: repoID,
			Type: domain.MemoryTypeFileSummary, Scope: domain.MemoryScopeFile, Key: "internal/store/store.go",
		},
		Summary:        "store.go as the unintegrated branch has it",
		ProvenanceKind: domain.ProvenanceRepoDerivation,
		SourcePaths:    []string{"internal/store/store.go"},
		SourceCommit:   "deadbeef",
	}.Normalized()
	if _, err := f.store.PutProjectMemoryItem(f.ctx, item, f.now()); err != nil {
		t.Fatal(err)
	}
	return repoID
}

// TestPruneRemovesWorktreeMemoryAndKeepsTheRepository is the whole operation in
// one test: the wreckage goes, the repository stays.
func TestPruneRemovesWorktreeMemoryAndKeepsTheRepository(t *testing.T) {
	f := newFixture(t)
	svc := projectmemory.NewService(f.store)
	repo, worktree := gitWorktreeFixture(t)
	rootID := seedRepoMemory(t, f, svc, repo)
	wtID := seedWorktreeMemory(t, f, worktree)

	req := projectmemory.PruneRequest{
		ProjectID: testProject, ProjectRoot: repo, RegisteredRepos: []string{repo},
	}

	// Dry run first: it reports and changes nothing.
	dry, err := svc.Prune(f.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !dry.Prunable() {
		t.Fatalf("the worktree memory was not found: %+v", dry.Candidates)
	}
	for _, c := range dry.Candidates {
		switch c.RepoID {
		case rootID:
			if c.Prunable {
				t.Fatal("the project's own repository was marked prunable")
			}
		case wtID:
			if !c.Prunable {
				t.Fatalf("the worktree memory was kept: %s", c.Reason)
			}
			if c.Purged {
				t.Fatal("a dry run purged")
			}
		}
	}
	if _, found, _ := f.store.GetProjectMemoryIndexState(f.ctx, testProject, wtID); !found {
		t.Fatal("a dry run removed the index row")
	}

	// Apply.
	req.Apply = true
	applied, err := svc.Prune(f.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if applied.PurgedItems == 0 {
		t.Fatal("apply purged nothing")
	}
	if _, found, _ := f.store.GetProjectMemoryIndexState(f.ctx, testProject, wtID); found {
		t.Fatal("the worktree memory survived the prune")
	}
	if _, found, _ := f.store.GetProjectMemoryIndexState(f.ctx, testProject, rootID); !found {
		t.Fatal("the prune removed the repository's own memory")
	}
	items, err := f.store.ListProjectMemoryItems(f.ctx, testProject, rootID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 {
		t.Fatal("the repository's facts went with the worktree's")
	}
}

// TestPruneRefusesABusyWorkspace is proof 5: a live execution's workspace is
// never pruned, however obviously it is a worktree.
func TestPruneRefusesABusyWorkspace(t *testing.T) {
	f := newFixture(t)
	svc := projectmemory.NewService(f.store)
	repo, worktree := gitWorktreeFixture(t)
	seedRepoMemory(t, f, svc, repo)
	wtID := seedWorktreeMemory(t, f, worktree)

	report, err := svc.Prune(f.ctx, projectmemory.PruneRequest{
		ProjectID: testProject, ProjectRoot: repo, RegisteredRepos: []string{repo},
		BusyWorkspaces: []string{worktree}, Apply: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range report.Candidates {
		if c.RepoID == wtID {
			if c.Prunable || c.Purged {
				t.Fatal("a workspace a live execution is using was pruned")
			}
			if c.Reason == "" {
				t.Fatal("the refusal was silent")
			}
		}
	}
	if _, found, _ := f.store.GetProjectMemoryIndexState(f.ctx, testProject, wtID); !found {
		t.Fatal("the busy workspace's memory was removed")
	}
}

// TestPruneRefusesWhenNothingCanonicalWouldSurvive is proof 4. A project whose
// ONLY memory was built wrongly still keeps it: a repair may leave a project
// with less memory, never with none, because "none" is not something an
// operator can tell apart from a broken install.
func TestPruneRefusesWhenNothingCanonicalWouldSurvive(t *testing.T) {
	f := newFixture(t)
	svc := projectmemory.NewService(f.store)
	repo, worktree := gitWorktreeFixture(t)
	wtID := seedWorktreeMemory(t, f, worktree)

	report, err := svc.Prune(f.ctx, projectmemory.PruneRequest{
		ProjectID: testProject, ProjectRoot: repo, RegisteredRepos: []string{repo}, Apply: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Prunable() || report.PurgedItems != 0 {
		t.Fatal("the project's only memory was pruned away")
	}
	if _, found, _ := f.store.GetProjectMemoryIndexState(f.ctx, testProject, wtID); !found {
		t.Fatal("the only memory the project had was removed")
	}
}

// TestPruneRefusesAnUnidentifiableDirectory is the fail-closed default: a real
// directory that git does not call a linked worktree and the project does not
// list is something AO cannot explain, so it is left alone.
func TestPruneRefusesAnUnidentifiableDirectory(t *testing.T) {
	f := newFixture(t)
	svc := projectmemory.NewService(f.store)
	repo, _ := gitWorktreeFixture(t)
	seedRepoMemory(t, f, svc, repo)

	stranger := t.TempDir()
	strangerID := domain.ProjectMemoryRepoID(mustCanonical(t, stranger))
	if err := f.store.EnsureProjectMemoryRepo(
		f.ctx, testProject, strangerID, mustCanonical(t, stranger), f.now()); err != nil {
		t.Fatal(err)
	}

	report, err := svc.Prune(f.ctx, projectmemory.PruneRequest{
		ProjectID: testProject, ProjectRoot: repo, RegisteredRepos: []string{repo}, Apply: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range report.Candidates {
		if c.RepoID == strangerID && (c.Prunable || c.Purged) {
			t.Fatalf("an unidentifiable directory was pruned: %s", c.Reason)
		}
	}
	if _, found, _ := f.store.GetProjectMemoryIndexState(f.ctx, testProject, strangerID); !found {
		t.Fatal("an unidentifiable directory's memory was removed")
	}
}
