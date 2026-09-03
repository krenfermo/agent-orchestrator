package branchlock_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/branchlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// p3c_explicit_placement_test.go — P3-C §28, the half the closing smoke found.
//
// THE DEFECT. Manager.Targets derives its direct-branch targets from the
// PROJECT's execution mode. A run whose direct-branch placement was chosen
// explicitly — an applied placement override, or a task scope that downgraded —
// inside a project whose default is isolated therefore produced NO targets. An
// acquisition with no targets returns (nil, nil), which every caller reads as
// success, so:
//
//   - the dirty-worktree preflight never ran;
//   - no lock row was written;
//   - admission recorded an empty branch authority and admitted the launch;
//   - a real worker started on the user's own branch, on top of their
//     uncommitted work, owning nothing.
//
// It was found by running the P3-C closing smoke against a live daemon: a real
// Claude worker was dispatched into a dirty scratch repository on `main` with
// the branch_locks table empty.

// seedIsolatedProject is the shape the defect needs: a real project whose
// DEFAULT mode is the isolated worktree, which is what makes Targets return
// nothing for it.
func seedIsolatedProject(t *testing.T, store interface {
	UpsertProject(context.Context, domain.ProjectRecord) error
}, id, path, branch string,
) {
	t.Helper()
	if err := store.UpsertProject(context.Background(), domain.ProjectRecord{
		ID: id, Path: path,
		Config: domain.ProjectConfig{DefaultBranch: branch, ExecutionMode: domain.ExecutionIsolatedWorktree},
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

// An explicit direct-branch target in an isolated-default project takes a REAL
// lock. Before the fix this returned no locks and no error.
func TestExplicitDirectBranchTargetIsLockedInAnIsolatedProject(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	seedIsolatedProject(t, store, "proj", "/repos/ao", "main")
	mgr := newManager(t, store, &fakePreflight{}, "owner-1")
	ctx := context.Background()

	locks, err := mgr.Acquire(ctx, branchlock.AcquireRequest{
		ProjectID: "proj", RunID: "WF-1",
		RepoPath: "/repos/ao", Branch: "main",
	})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if len(locks) != 1 {
		t.Fatalf("%d locks taken, want exactly 1 — an explicit direct-branch run owns nothing", len(locks))
	}
	if locks[0].RepoPath != "/repos/ao" || locks[0].Branch != "main" {
		t.Fatalf("lock = %#v, want the placement's own repository and branch", locks[0])
	}
	holder, found, err := mgr.Holder(ctx, "/repos/ao", "main")
	if err != nil || !found || holder.WorkflowRunID != "WF-1" {
		t.Fatalf("holder = (%#v, %v, %v), want the run that acquired it", holder, found, err)
	}
}

// And the gate that matters most: a dirty repository refuses the acquisition
// instead of admitting a launch onto somebody's uncommitted work.
func TestExplicitDirectBranchTargetStillRefusesADirtyRepository(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	seedIsolatedProject(t, store, "proj", "/repos/ao", "main")
	mgr := newManager(t, store, &fakePreflight{dirty: map[string]bool{"/repos/ao": true}}, "owner-1")

	_, err := mgr.Acquire(context.Background(), branchlock.AcquireRequest{
		ProjectID: "proj", RunID: "WF-1",
		RepoPath: "/repos/ao", Branch: "main",
	})
	if !errors.Is(err, branchlock.ErrDirtyRepository) {
		t.Fatalf("err = %v, want the dirty-repository refusal", err)
	}
	if _, found, herr := mgr.Holder(context.Background(), "/repos/ao", "main"); herr != nil || found {
		t.Fatalf("a refused acquisition left a lock behind (found=%v, %v)", found, herr)
	}
}

// Two explicit direct-branch runs on one branch still contend: the override
// produces a real target, so exclusivity is the ordinary partial unique index
// rather than a special case.
func TestTwoExplicitDirectBranchRunsContendForOneBranch(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	seedIsolatedProject(t, store, "proj", "/repos/ao", "main")
	mgr := newManager(t, store, &fakePreflight{}, "owner-1")
	ctx := context.Background()
	req := branchlock.AcquireRequest{ProjectID: "proj", RunID: "WF-1", RepoPath: "/repos/ao", Branch: "main"}

	if _, err := mgr.Acquire(ctx, req); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	second := req
	second.RunID = "WF-2"
	if _, err := mgr.Acquire(ctx, second); err == nil {
		t.Fatal("a second run took a branch the first already owns")
	}
}

// The narrowness of the override is the other half of the property. A project
// that IS in direct-branch mode keeps deriving its full target set from its own
// configuration, so a workspace project still locks its root AND every child
// repository rather than the single repo a caller happened to name.
func TestAProjectInDirectBranchModeStillDerivesItsOwnTargets(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	seedWorkspaceProject(t, store,
		domain.ProjectRecord{
			ID: "ws", Path: "/repos/ws", Kind: domain.ProjectKindWorkspace,
			Config: domain.ProjectConfig{DefaultBranch: "main", ExecutionMode: domain.ExecutionDirectBranch},
		},
		domain.WorkspaceRepoRecord{ProjectID: "ws", Name: "api", RelativePath: "api", DefaultBranch: "main"},
	)
	mgr := newManager(t, store, &fakePreflight{}, "owner-1")

	// The caller names ONE repository. The project's own configuration names
	// two, and that is what must be locked.
	locks, err := mgr.Acquire(context.Background(), branchlock.AcquireRequest{
		ProjectID: "ws", RunID: "WF-1", RepoPath: "/repos/ws", Branch: "main",
	})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if len(locks) != 2 {
		t.Fatalf("%d locks taken, want 2 — the override narrowed a workspace project's own target set", len(locks))
	}
}
