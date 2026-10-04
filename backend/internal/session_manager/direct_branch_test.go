package sessionmanager

import (
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Checkpoint 8P-E.11: in direct-branch mode the configured branch IS the work
// branch. AO must not generate an ao/* branch, and the session prefix -- which
// only ever existed to name generated branches -- must not leak into git.
func TestProjectSpawnBranchUsesTheConfiguredBranchInDirectBranchMode(t *testing.T) {
	project := domain.ProjectRecord{
		ID:   "proj",
		Path: "/repos/agent-orchestrator",
		Config: domain.ProjectConfig{
			DefaultBranch: "feat/engineering-control-center",
			SessionPrefix: "ao",
			ExecutionMode: domain.ExecutionDirectBranch,
		},
	}
	for _, kind := range []domain.SessionKind{domain.KindWorker, domain.KindOrchestrator} {
		got := ProjectSpawnBranch(project, "sess-1", kind, "/home/u/.ao/data")
		if got != "feat/engineering-control-center" {
			t.Fatalf("kind %q branch = %q, want the configured branch", kind, got)
		}
		if strings.HasPrefix(got, "ao/") {
			t.Fatalf("kind %q produced a generated ao/* branch: %q", kind, got)
		}
	}
	if got := ProjectOrchestratorBranch(project, "/home/u/.ao/data"); got != "feat/engineering-control-center" {
		t.Fatalf("orchestrator branch = %q, want the configured branch", got)
	}
}

// Every project that did not opt in keeps the pre-checkpoint generated branch.
func TestProjectSpawnBranchStillGeneratesAOBranchesInWorktreeMode(t *testing.T) {
	project := domain.ProjectRecord{
		ID:     "proj",
		Path:   "/repos/agent-orchestrator",
		Config: domain.ProjectConfig{DefaultBranch: "main", SessionPrefix: "ao"},
	}
	got := ProjectSpawnBranch(project, "sess-1", domain.KindWorker, "/home/u/.ao/data")
	if !strings.HasPrefix(got, "ao/") {
		t.Fatalf("worktree-mode branch = %q, want a generated ao/* branch", got)
	}
	if got != DefaultSpawnBranch("sess-1", domain.KindWorker, "ao", domain.ProjectKindSingleRepo, "/home/u/.ao/data") {
		t.Fatalf("worktree-mode branch %q diverged from the pre-checkpoint helper", got)
	}
}

// A scratch project has no repository at all, so it keeps its empty branch even
// if direct-branch mode is somehow stored on it.
func TestProjectSpawnBranchIsEmptyForScratchProjects(t *testing.T) {
	project := domain.ProjectRecord{
		ID:     "scratch",
		Kind:   domain.ProjectKindScratch,
		Config: domain.ProjectConfig{ExecutionMode: domain.ExecutionDirectBranch, DefaultBranch: "main"},
	}
	if got := ProjectSpawnBranch(project, "sess-1", domain.KindWorker, "/home/u/.ao/data"); got != "" {
		t.Fatalf("scratch branch = %q, want empty", got)
	}
}
