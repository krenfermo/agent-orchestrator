package branchlock_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/branchlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// fakePreflight reports a fixed answer per repo path. A path with no entry is
// clean.
type fakePreflight struct {
	mu    sync.Mutex
	dirty map[string]bool
	err   error
}

func (f *fakePreflight) PreflightRepository(_ context.Context, repoPath, branch string) (ports.WorkspacePreflight, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return ports.WorkspacePreflight{}, f.err
	}
	out := ports.WorkspacePreflight{RepoPath: repoPath, ConfiguredBranch: branch, CurrentBranch: branch, HeadSHA: "sha-" + repoPath}
	if f.dirty[repoPath] {
		out.Dirty = true
		out.Changes = []ports.WorkspaceChange{{Path: "README.md", Status: " M"}}
	}
	return out, nil
}

func TestTargetsForSingleRepoDirectBranchProject(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	seedProject(t, store, domain.ProjectRecord{
		ID: "proj", Path: "/repos/agent-orchestrator",
		Config: domain.ProjectConfig{
			DefaultBranch: "feat/engineering-control-center",
			ExecutionMode: domain.ExecutionDirectBranch,
		},
	})
	mgr := newManager(t, store, nil, "owner-1")

	targets, err := mgr.Targets(context.Background(), "proj")
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(targets))
	}
	if targets[0].Branch != "feat/engineering-control-center" || targets[0].RepoName != domain.RootWorkspaceRepoName {
		t.Fatalf("target = %#v", targets[0])
	}
}

// A workspace project's repositories keep their own branches: they are separate
// targets and therefore separate locks, never one collapsed project-wide lock.
func TestTargetsForWorkspaceProjectKeepRepositoriesIndependent(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	seedWorkspaceProject(t, store, domain.ProjectRecord{
		ID: "medusa", Path: "/repos/medusa",
		Config: domain.ProjectConfig{DefaultBranch: "main", ExecutionMode: domain.ExecutionDirectBranch},
	},
		domain.WorkspaceRepoRecord{Name: "backend_node", RelativePath: "backend_node", DefaultBranch: "medusa_back_v2", GitStatus: domain.GitStatusReady},
		domain.WorkspaceRepoRecord{Name: "not_a_repo_yet", RelativePath: "pending", GitStatus: domain.GitStatusNeedsInit},
	)
	mgr := newManager(t, store, nil, "owner-1")

	targets, err := mgr.Targets(context.Background(), "medusa")
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	branches := map[string]string{}
	for _, target := range targets {
		branches[target.RepoName] = target.Branch
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %#v, want root + backend_node only (needs_init is skipped)", targets)
	}
	if branches[domain.RootWorkspaceRepoName] != "main" {
		t.Fatalf("root branch = %q, want main", branches[domain.RootWorkspaceRepoName])
	}
	if branches["backend_node"] != "medusa_back_v2" {
		t.Fatalf("child branch = %q, want medusa_back_v2", branches["backend_node"])
	}
	if targets[0].Key == targets[1].Key {
		t.Fatal("root and child collapsed into one lock key")
	}
}

func TestTargetsAreEmptyForIsolatedWorktreeProjects(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	seedProject(t, store, domain.ProjectRecord{
		ID: "proj", Path: "/repos/legacy",
		// No ExecutionMode set at all: this is every project that existed
		// before the checkpoint.
		Config: domain.ProjectConfig{DefaultBranch: "main"},
	})
	mgr := newManager(t, store, nil, "owner-1")

	targets, err := mgr.Targets(context.Background(), "proj")
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("targets = %#v, want none for an isolated-worktree project", targets)
	}
}

func TestTwoRunsCannotWriteTheSameRepoAndBranch(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	seedDirectProject(t, store, "proj", "/repos/ao", "feat/engineering-control-center")
	mgr := newManager(t, store, &fakePreflight{}, "owner-1")
	ctx := context.Background()

	first, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", RunID: "WF-1"})
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first acquired %d locks, want 1", len(first))
	}

	_, err = mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", RunID: "WF-2"})
	if !branchlock.IsConflict(err) {
		t.Fatalf("second acquire err = %v, want a branch-lock conflict", err)
	}
	holder, ok := branchlock.Conflict(err)
	if !ok || holder.WorkflowRunID != "WF-1" {
		t.Fatalf("conflict holder = %#v, want WF-1 named", holder)
	}
	if holder.Branch != "feat/engineering-control-center" {
		t.Fatalf("conflict branch = %q", holder.Branch)
	}
}

func TestWaitingRunAcquiresAfterTheOwnerReleases(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	seedDirectProject(t, store, "proj", "/repos/ao", "main")
	mgr := newManager(t, store, &fakePreflight{}, "owner-1")
	ctx := context.Background()

	if _, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", RunID: "WF-1"}); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", RunID: "WF-2"}); !branchlock.IsConflict(err) {
		t.Fatalf("second acquire err = %v, want conflict", err)
	}
	if n, err := mgr.ReleaseRun(ctx, "WF-1", "completed"); err != nil || n != 1 {
		t.Fatalf("release = (%d, %v), want (1, nil)", n, err)
	}
	locks, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", RunID: "WF-2"})
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if len(locks) != 1 || locks[0].WorkflowRunID != "WF-2" {
		t.Fatalf("locks = %#v, want WF-2 owning one lock", locks)
	}
}

// Re-acquiring is idempotent for the run that already holds the lock, so a
// redispatch or a reconcile pass never deadlocks a run against itself.
func TestReacquireByTheSameRunIsIdempotent(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	seedDirectProject(t, store, "proj", "/repos/ao", "main")
	mgr := newManager(t, store, &fakePreflight{}, "owner-1")
	ctx := context.Background()

	first, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", RunID: "WF-1"})
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	second, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", RunID: "WF-1"})
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	if second[0].ID != first[0].ID {
		t.Fatalf("re-acquire produced a new lock %q, want the existing %q", second[0].ID, first[0].ID)
	}
}

func TestConcurrentAcquireProducesExactlyOneWinner(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	seedDirectProject(t, store, "proj", "/repos/ao", "main")
	mgr := newManager(t, store, &fakePreflight{}, "owner-1")
	ctx := context.Background()

	const runs = 8
	var wg sync.WaitGroup
	results := make([]error, runs)
	start := make(chan struct{})
	for i := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, results[i] = mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", RunID: fmt.Sprintf("WF-%d", i)})
		}()
	}
	close(start)
	wg.Wait()

	winners := 0
	for i, err := range results {
		switch {
		case err == nil:
			winners++
		case branchlock.IsConflict(err):
		default:
			t.Fatalf("run %d got an unexpected error: %v", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}
	held, err := store.ListHeldBranchLocks(ctx)
	if err != nil {
		t.Fatalf("list held: %v", err)
	}
	if len(held) != 1 {
		t.Fatalf("held locks = %d, want 1", len(held))
	}
}

func TestDirtyRepositoryBlocksAcquisitionAndTakesNoLock(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	seedDirectProject(t, store, "proj", "/repos/ao", "main")
	pre := &fakePreflight{dirty: map[string]bool{"/repos/ao": true}}
	mgr := newManager(t, store, pre, "owner-1")
	ctx := context.Background()

	_, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", RunID: "WF-1"})
	if !branchlock.IsDirty(err) {
		t.Fatalf("err = %v, want a dirty-repository refusal", err)
	}
	repos, ok := branchlock.Dirty(err)
	if !ok || len(repos) != 1 || repos[0].RepoPath != "/repos/ao" {
		t.Fatalf("dirty repos = %#v", repos)
	}
	held, err := store.ListHeldBranchLocks(ctx)
	if err != nil {
		t.Fatalf("list held: %v", err)
	}
	if len(held) != 0 {
		t.Fatalf("a blocked acquisition still took %d lock(s)", len(held))
	}
}

// One dirty repository in a workspace project blocks the whole acquisition and
// leaves no partially-owned project behind.
func TestDirtyChildRepositoryRollsBackTheWholeAcquisition(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	seedWorkspaceProject(t, store, domain.ProjectRecord{
		ID: "medusa", Path: "/repos/medusa",
		Config: domain.ProjectConfig{DefaultBranch: "main", ExecutionMode: domain.ExecutionDirectBranch},
	},
		domain.WorkspaceRepoRecord{Name: "backend_node", RelativePath: "backend_node", DefaultBranch: "medusa_back_v2", GitStatus: domain.GitStatusReady},
	)
	pre := &fakePreflight{dirty: map[string]bool{"/repos/medusa/backend_node": true}}
	mgr := newManager(t, store, pre, "owner-1")
	ctx := context.Background()

	if _, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "medusa", RunID: "WF-1"}); !branchlock.IsDirty(err) {
		t.Fatalf("err = %v, want dirty", err)
	}
	held, err := store.ListHeldBranchLocks(ctx)
	if err != nil {
		t.Fatalf("list held: %v", err)
	}
	if len(held) != 0 {
		t.Fatalf("partial acquisition left %d lock(s) held", len(held))
	}
}

func TestReconcileReleasesStaleLocksAndAdoptsLiveOnes(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	seedDirectProject(t, store, "proj", "/repos/ao", "main")
	ctx := context.Background()

	liveRun := seedRun(t, store, "proj", domain.WorkflowRunRunning)
	cancelledRun := seedRun(t, store, "proj", domain.WorkflowRunCancelled)

	// A previous daemon instance held both locks.
	previous := newManager(t, store, &fakePreflight{}, "owner-previous")
	mustAcquireFor(t, previous, "proj", liveRun)
	mustAcquireRaw(t, store, "/repos/other", "main", cancelledRun, "owner-previous")
	mustAcquireRaw(t, store, "/repos/gone", "main", "WF-does-not-exist", "owner-previous")

	current := newManager(t, store, &fakePreflight{}, "owner-current")
	result, err := current.Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Adopted != 1 || result.Released != 2 {
		t.Fatalf("reconcile = %#v, want 1 adopted (live run) and 2 released (terminal + missing)", result)
	}
	held, err := store.ListHeldBranchLocks(ctx)
	if err != nil {
		t.Fatalf("list held: %v", err)
	}
	if len(held) != 1 || held[0].WorkflowRunID != liveRun {
		t.Fatalf("held after reconcile = %#v, want only the live run's lock", held)
	}
	if held[0].OwnerToken != "owner-current" {
		t.Fatalf("owner token = %q, want the lock adopted by this instance", held[0].OwnerToken)
	}
}

// Reconciliation must never free a branch a still-live run is writing: a second
// run must still be refused right after a restart.
func TestReconcileKeepsALiveRunsBranchExclusive(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	seedDirectProject(t, store, "proj", "/repos/ao", "main")
	ctx := context.Background()
	liveRun := seedRun(t, store, "proj", domain.WorkflowRunRunning)

	previous := newManager(t, store, &fakePreflight{}, "owner-previous")
	mustAcquireFor(t, previous, "proj", liveRun)

	current := newManager(t, store, &fakePreflight{}, "owner-current")
	if _, err := current.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := current.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", RunID: "WF-other"}); !branchlock.IsConflict(err) {
		t.Fatalf("post-restart acquire err = %v, want the branch still exclusively held", err)
	}
}

func TestRenewRecordsTheOccupyingSessionWithoutTransferringOwnership(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	seedDirectProject(t, store, "proj", "/repos/ao", "main")
	mgr := newManager(t, store, &fakePreflight{}, "owner-1")
	ctx := context.Background()

	if _, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", RunID: "WF-1"}); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	mgr.Renew(ctx, "WF-1", "step-1", "sess-9")
	held, err := mgr.HeldByRun(ctx, "WF-1")
	if err != nil || len(held) != 1 {
		t.Fatalf("held = (%#v, %v)", held, err)
	}
	if held[0].SessionID != "sess-9" || held[0].WorkflowStepID != "step-1" {
		t.Fatalf("lock scope = %#v, want the live step/session recorded", held[0])
	}
	// A different run's renewal must change nothing.
	mgr.Renew(ctx, "WF-2", "step-x", "sess-x")
	held, _ = mgr.HeldByRun(ctx, "WF-1")
	if held[0].SessionID != "sess-9" {
		t.Fatalf("another run's renewal changed the lock: %#v", held[0])
	}
}

func TestHolderNamesTheOccupyingRun(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	seedDirectProject(t, store, "proj", "/repos/ao", "main")
	mgr := newManager(t, store, &fakePreflight{}, "owner-1")
	ctx := context.Background()

	if _, found, err := mgr.Holder(ctx, "/repos/ao", "main"); err != nil || found {
		t.Fatalf("holder before acquire = (found=%v, %v), want free", found, err)
	}
	if _, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", RunID: "WF-1"}); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	holder, found, err := mgr.Holder(ctx, "/repos/ao", "main")
	if err != nil || !found || holder.WorkflowRunID != "WF-1" {
		t.Fatalf("holder = (%#v, %v, %v)", holder, found, err)
	}
}

func TestPreflightFailureIsSurfacedNotSwallowed(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	seedDirectProject(t, store, "proj", "/repos/ao", "main")
	boom := errors.New("git exploded")
	mgr := newManager(t, store, &fakePreflight{err: boom}, "owner-1")

	_, err := mgr.Acquire(context.Background(), branchlock.AcquireRequest{ProjectID: "proj", RunID: "WF-1"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the probe failure surfaced", err)
	}
}

// ---- helpers ----

func newManager(t *testing.T, store *sqlite.Store, pre branchlock.Preflighter, owner string) *branchlock.Manager {
	t.Helper()
	var n int
	var mu sync.Mutex
	return branchlock.New(branchlock.Deps{
		Store:      store,
		Preflight:  pre,
		OwnerToken: owner,
		NewID: func() string {
			mu.Lock()
			defer mu.Unlock()
			n++
			return fmt.Sprintf("%s-%d", owner, n)
		},
		Clock: func() time.Time { return time.Unix(1700000000, 0).UTC() },
	})
}

func seedProject(t *testing.T, store *sqlite.Store, rec domain.ProjectRecord) {
	t.Helper()
	if rec.RegisteredAt.IsZero() {
		rec.RegisteredAt = time.Now().UTC()
	}
	if err := store.UpsertProject(context.Background(), rec); err != nil {
		t.Fatalf("seed project %s: %v", rec.ID, err)
	}
}

func seedDirectProject(t *testing.T, store *sqlite.Store, id, path, branch string) {
	t.Helper()
	seedProject(t, store, domain.ProjectRecord{
		ID: id, Path: path,
		Config: domain.ProjectConfig{DefaultBranch: branch, ExecutionMode: domain.ExecutionDirectBranch},
	})
}

func seedWorkspaceProject(t *testing.T, store *sqlite.Store, rec domain.ProjectRecord, repos ...domain.WorkspaceRepoRecord) {
	t.Helper()
	if rec.RegisteredAt.IsZero() {
		rec.RegisteredAt = time.Now().UTC()
	}
	rec.Kind = domain.ProjectKindWorkspace
	for i := range repos {
		repos[i].ProjectID = domain.ProjectID(rec.ID)
		if repos[i].RegisteredAt.IsZero() {
			repos[i].RegisteredAt = rec.RegisteredAt
		}
	}
	if err := store.UpsertWorkspaceProject(context.Background(), rec, repos); err != nil {
		t.Fatalf("seed workspace project %s: %v", rec.ID, err)
	}
}

func seedRun(t *testing.T, store *sqlite.Store, projectID string, state domain.WorkflowRunState) string {
	t.Helper()
	id := fmt.Sprintf("WF-%s-%d", state, time.Now().UnixNano())
	now := time.Now().UTC()
	run, _, err := store.CreateWorkflowRun(context.Background(), domain.WorkflowRun{
		ID: id, ProjectID: projectID, Objective: "obj", State: domain.WorkflowRunPending,
		PolicyVersion: "v1", PolicySnapshot: "{}", CreatedAt: now, UpdatedAt: now,
	}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if state != domain.WorkflowRunPending {
		next := state
		if state.Terminal() && state != domain.WorkflowRunCancelled {
			// Pending can only reach a terminal state via running.
			if _, err := store.UpdateWorkflowRunState(context.Background(), run.ID, domain.WorkflowRunPending, domain.WorkflowRunRunning, now); err != nil {
				t.Fatalf("advance run: %v", err)
			}
			if _, err := store.UpdateWorkflowRunState(context.Background(), run.ID, domain.WorkflowRunRunning, next, now); err != nil {
				t.Fatalf("terminate run: %v", err)
			}
			return run.ID
		}
		if _, err := store.UpdateWorkflowRunState(context.Background(), run.ID, domain.WorkflowRunPending, next, now); err != nil {
			t.Fatalf("advance run to %s: %v", next, err)
		}
	}
	return run.ID
}

func mustAcquireFor(t *testing.T, mgr *branchlock.Manager, projectID, runID string) {
	t.Helper()
	if _, err := mgr.Acquire(context.Background(), branchlock.AcquireRequest{ProjectID: domain.ProjectID(projectID), RunID: runID}); err != nil {
		t.Fatalf("acquire for %s: %v", runID, err)
	}
}

// mustAcquireRaw writes a held lock straight through the store, for scenarios
// (a repo outside the seeded project, a run that no longer exists) the manager
// deliberately cannot construct.
func mustAcquireRaw(t *testing.T, store *sqlite.Store, repoPath, branch, runID, owner string) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := store.AcquireBranchLock(context.Background(), domain.BranchLock{
		ID:            "blk-raw-" + runID,
		LockKey:       domain.BranchLockKey(repoPath, branch),
		ProjectID:     "proj",
		RepoPath:      repoPath,
		RepoName:      domain.RootWorkspaceRepoName,
		Branch:        branch,
		WorkflowRunID: runID,
		OwnerToken:    owner,
		AcquiredAt:    now,
	}); err != nil {
		t.Fatalf("raw acquire for %s: %v", runID, err)
	}
}

func TestAcquirePersistsNonDirectOwnershipKinds(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	mgr := newManager(t, store, &fakePreflight{}, "owner-1")
	ctx := context.Background()
	for i, kind := range []domain.BranchLockOwnershipKind{
		domain.BranchLockOwnershipTaskWorkspace,
		domain.BranchLockOwnershipTargetIntegration,
	} {
		locks, err := mgr.Acquire(ctx, branchlock.AcquireRequest{
			ProjectID: "proj", RunID: fmt.Sprintf("run-%d", i), Kind: kind,
			RepoPath: t.TempDir(), Branch: fmt.Sprintf("ao/task-%d", i),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(locks) != 1 || locks[0].OwnershipKind != kind {
			t.Fatalf("kind %q persisted as %+v", kind, locks)
		}
		held, found, err := store.GetHeldBranchLock(ctx, locks[0].LockKey)
		if err != nil || !found || held.OwnershipKind != kind {
			t.Fatalf("reloaded kind=%q found=%v err=%v, want %q", held.OwnershipKind, found, err, kind)
		}
	}
}
