package router_test

import (
	"context"
	"testing"

	workspacerouter "github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/router"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// placement_test.go — P3-A §7. The router used to answer "worktree or branch?"
// by reading the PROJECT's execution mode, which is mutable and describes how a
// project usually works. A run's frozen placement describes where THIS work was
// decided to happen, and when the two disagree the second is the only one that
// cannot change under a run already in flight.
//
// The bug those tests close is a user experience one with a physical
// consequence: somebody picks "current branch" for one task of an
// isolated-worktree project, and AO silently cuts a worktree anyway.

func placementRouter(mode domain.ExecutionMode) (*workspacerouter.Workspace, *recordingWorkspace, *recordingWorkspace) {
	git := &recordingWorkspace{path: "/data/ao/worktrees/s1"}
	direct := &recordingWorkspace{path: "/repo"}
	return workspacerouter.New(workspacerouter.Deps{
		Git:          git,
		DirectBranch: direct,
		Projects: projectStore{projects: map[string]domain.ProjectRecord{
			"p1": {ID: "p1", Path: "/repo", Config: domain.ProjectConfig{ExecutionMode: mode, DefaultBranch: "main"}},
		}},
	}), git, direct
}

func TestFrozenDirectBranchPlacementBeatsIsolatedProjectConfig(t *testing.T) {
	router, git, direct := placementRouter(domain.ExecutionIsolatedWorktree)
	if _, err := router.Create(context.Background(), ports.WorkspaceConfig{
		ProjectID: "p1", SessionID: "s1", Branch: "feat/x",
		Placement: domain.PlacementDirectBranch,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if git.createCalls != 0 {
		t.Fatal("an explicitly placed direct-branch run was given a git worktree")
	}
	if direct.createCalls != 1 {
		t.Fatalf("direct-branch adapter createCalls = %d, want 1", direct.createCalls)
	}
}

// The converse, so the override is a real authority rather than a one-way
// escape hatch: a frozen isolated placement is materialised as a worktree even
// on a project configured for direct-branch execution.
func TestFrozenIsolatedPlacementBeatsDirectBranchProjectConfig(t *testing.T) {
	router, git, direct := placementRouter(domain.ExecutionDirectBranch)
	if _, err := router.Create(context.Background(), ports.WorkspaceConfig{
		ProjectID: "p1", SessionID: "s1", Branch: "ao/wf-1/t1",
		Placement: domain.PlacementIsolatedWorktree,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if direct.createCalls != 0 {
		t.Fatal("a frozen isolated placement was materialised on the project's branch")
	}
	if git.createCalls != 1 {
		t.Fatalf("git adapter createCalls = %d, want 1", git.createCalls)
	}
}

// A spawn with no placement is an ordinary session, and it keeps the exact
// project-config routing it has always had.
func TestNoPlacementKeepsProjectConfigRouting(t *testing.T) {
	router, git, direct := placementRouter(domain.ExecutionDirectBranch)
	if _, err := router.Create(context.Background(), ports.WorkspaceConfig{
		ProjectID: "p1", SessionID: "s1", Branch: "main",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if git.createCalls != 0 || direct.createCalls != 1 {
		t.Fatalf("unplaced session routed to git=%d direct=%d, want the project's own mode", git.createCalls, direct.createCalls)
	}
}

// An unconfigured direct-branch adapter is an error, never a fallback to the
// worktree the caller opted out of — the same refusal 8P-E.11 made for the
// project-config path, now made for the placement one.
func TestDirectBranchPlacementWithoutAdapterRefusesRatherThanFallsBack(t *testing.T) {
	git := &recordingWorkspace{}
	router := workspacerouter.New(workspacerouter.Deps{
		Git: git,
		Projects: projectStore{projects: map[string]domain.ProjectRecord{
			"p1": {ID: "p1", Path: "/repo", Config: domain.ProjectConfig{ExecutionMode: domain.ExecutionIsolatedWorktree}},
		}},
	})
	if _, err := router.Create(context.Background(), ports.WorkspaceConfig{
		ProjectID: "p1", SessionID: "s1", Placement: domain.PlacementDirectBranch,
	}); err == nil {
		t.Fatal("a direct-branch placement with no adapter silently fell back")
	}
	if git.createCalls != 0 {
		t.Fatal("a worktree was created for a run that asked for its own branch")
	}
}

// Restore takes the same authority as Create: a run recovered after a restart
// must come back in the placement it was frozen into, not in the one the
// project happens to be configured for now.
func TestRestoreHonoursFrozenPlacement(t *testing.T) {
	router, git, direct := placementRouter(domain.ExecutionIsolatedWorktree)
	if _, err := router.Restore(context.Background(), ports.WorkspaceConfig{
		ProjectID: "p1", SessionID: "s1", Path: "/repo",
		Placement: domain.PlacementDirectBranch,
	}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if git.restoreCalls != 0 || direct.restoreCalls != 1 {
		t.Fatalf("restore routed to git=%d direct=%d, want the frozen placement", git.restoreCalls, direct.restoreCalls)
	}
}

// TestCommitAllRoutesToDirectBranchRegardlessOfProjectMode — P3-C §28.
//
// The commit-and-continue flow and the autonomous local commit both target a
// direct-branch repository, and both have already proven that from the RUN's
// frozen placement before they get here. Re-asking the PROJECT could only
// produce a different answer, and it did: an explicit direct-branch placement
// inside an isolated-default project routed to the worktree adapter, which is
// not a committer, so the one stop the flow exists to clear answered 500.
func TestCommitAllRoutesToDirectBranchRegardlessOfProjectMode(t *testing.T) {
	for _, mode := range []domain.ExecutionMode{domain.ExecutionIsolatedWorktree, domain.ExecutionDirectBranch} {
		t.Run(string(mode), func(t *testing.T) {
			w, git, direct := placementRouter(mode)
			sha, committed, err := w.CommitAll(context.Background(), ports.WorkspaceInfo{
				ProjectID: "p1", Path: "/repo", RepoPath: "/repo", Branch: "main",
			}, "save the local work")
			if err != nil {
				t.Fatalf("CommitAll under project mode %s: %v", mode, err)
			}
			if !committed || sha == "" {
				t.Fatalf("commit reported nothing: sha=%q committed=%v", sha, committed)
			}
			if direct.commits != 1 {
				t.Fatalf("the direct-branch adapter committed %d times, want 1", direct.commits)
			}
			if git.commits != 0 {
				t.Fatalf("the worktree adapter was asked to commit %d times", git.commits)
			}
		})
	}
}
