package sessionmanager_test

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
)

// placement_branch_test.go — P3-A §7, the half the adapter routing does not
// cover.
//
// Routing the workspace ADAPTER by the frozen placement is necessary and not
// sufficient: a spawn also names the branch that adapter checks out. Deriving
// the adapter from the placement and the branch from project configuration
// produced exactly one failure, and the P3-A smoke walked straight into it — a
// direct-branch placement handed the generated `ao/<project>/<session>` name,
// which does not exist in the user's repository, so every launch failed with
// "branch is not fetched" and retried forever.
//
// These tests pin both questions being answered by the same authority.

func placementProject() domain.ProjectRecord {
	return domain.ProjectRecord{
		ID:   "p1",
		Path: "/repo",
		Kind: domain.ProjectKindSingleRepo,
		Config: domain.ProjectConfig{
			// The project says isolated worktrees; the placement will say
			// otherwise, which is the whole point.
			ExecutionMode: domain.ExecutionIsolatedWorktree,
			DefaultBranch: "feat/x",
		},
	}
}

func TestDirectBranchPlacementSpawnsOnTheConfiguredBranch(t *testing.T) {
	got := sessionmanager.SpawnBranchForPlacement(
		placementProject(), "s-1", domain.KindWorker, t.TempDir(), domain.PlacementDirectBranch,
	)
	if got != "feat/x" {
		t.Fatalf("branch = %q, want the repository's own branch feat/x", got)
	}
}

func TestIsolatedPlacementSpawnsOnAGeneratedBranch(t *testing.T) {
	project := placementProject()
	project.Config.ExecutionMode = domain.ExecutionDirectBranch
	got := sessionmanager.SpawnBranchForPlacement(
		project, "s-1", domain.KindWorker, t.TempDir(), domain.PlacementIsolatedWorktree,
	)
	if got == "feat/x" || got == "" {
		t.Fatalf("branch = %q, want a generated AO branch rather than the project's own", got)
	}
}

// No placement is the pre-P3-A path, and it must be unchanged: the project's
// execution mode still decides, because that is the only authority such a spawn
// has.
func TestNoPlacementKeepsTheProjectsOwnBranchRule(t *testing.T) {
	dir := t.TempDir()
	project := placementProject()
	if got, want := sessionmanager.SpawnBranchForPlacement(project, "s-1", domain.KindWorker, dir, ""),
		sessionmanager.ProjectSpawnBranch(project, "s-1", domain.KindWorker, dir); got != want {
		t.Fatalf("branch = %q, want the unchanged project rule %q", got, want)
	}
	project.Config.ExecutionMode = domain.ExecutionDirectBranch
	if got := sessionmanager.SpawnBranchForPlacement(project, "s-1", domain.KindWorker, dir, ""); got != "feat/x" {
		t.Fatalf("branch = %q, want feat/x for a direct-branch project with no placement", got)
	}
}
