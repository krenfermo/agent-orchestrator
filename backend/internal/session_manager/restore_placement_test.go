package sessionmanager_test

import (
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
)

// restore_placement_test.go — P5: a restart must not re-place a session.
//
// A spawn carries its run's frozen placement into the workspace router; a
// restore did not, and the empty placement means "route by project
// configuration". The gap between a spawn and a restore is a daemon restart,
// which is the likeliest moment for a person to have changed that
// configuration -- so an isolated session in a project since switched to
// direct-branch was re-attached to the operator's own checkout, on the
// operator's own branch, while the worktree holding its work sat unreferenced.
//
// The placement is not guessed here. It is read back from the two paths written
// when the session was created: a direct-branch session's workspace IS the
// registered repository, an AO worktree's is not.

func TestRestorePlacementRecognisesAnIsolatedWorktree(t *testing.T) {
	got := sessionmanager.PlacementFromSessionFacts(domain.SessionMetadata{
		WorkspacePath:     "/home/u/.ao/worktrees/ao-p1-sess-1",
		WorkspaceRepoPath: "/repos/agent-orchestrator",
	}, "/repos/agent-orchestrator")
	if got != domain.PlacementIsolatedWorktree {
		t.Fatalf("placement = %q, want isolated_worktree: the workspace is not the repository", got)
	}
}

func TestRestorePlacementRecognisesADirectBranchCheckout(t *testing.T) {
	got := sessionmanager.PlacementFromSessionFacts(domain.SessionMetadata{
		WorkspacePath:     "/repos/agent-orchestrator",
		WorkspaceRepoPath: "/repos/agent-orchestrator",
	}, "/repos/agent-orchestrator")
	if got != domain.PlacementDirectBranch {
		t.Fatalf("placement = %q, want direct_branch: the workspace IS the repository", got)
	}
}

// Paths are compared as paths, not as bytes: a trailing separator or a `.`
// segment is the same directory, and reading it as a different one would put a
// direct-branch session back through the worktree adapter.
func TestRestorePlacementComparesPathsNotBytes(t *testing.T) {
	repo := filepath.Join("/repos", "agent-orchestrator")
	got := sessionmanager.PlacementFromSessionFacts(domain.SessionMetadata{
		WorkspacePath:     repo + string(filepath.Separator),
		WorkspaceRepoPath: filepath.Join(repo, "."),
	}, repo)
	if got != domain.PlacementDirectBranch {
		t.Fatalf("placement = %q, want direct_branch for two spellings of one directory", got)
	}
}

// A session written before WorkspaceRepoPath was reliably stored is not left
// undecided: the PROJECT's own registered path answers the same question
// against the same workspace, from a durable row rather than a guess. This is
// the shape every session created before the worktree adapter reported its
// repository has on disk.
func TestRestorePlacementFallsBackToTheProjectsRegisteredPath(t *testing.T) {
	isolated := sessionmanager.PlacementFromSessionFacts(
		domain.SessionMetadata{WorkspacePath: "/home/u/.ao/worktrees/ao-p1-sess-1"},
		"/repos/agent-orchestrator",
	)
	if isolated != domain.PlacementIsolatedWorktree {
		t.Fatalf("placement = %q, want isolated_worktree from the project path alone", isolated)
	}
	direct := sessionmanager.PlacementFromSessionFacts(
		domain.SessionMetadata{WorkspacePath: "/repos/agent-orchestrator"},
		"/repos/agent-orchestrator",
	)
	if direct != domain.PlacementDirectBranch {
		t.Fatalf("placement = %q, want direct_branch from the project path alone", direct)
	}
}

// With NO comparison available at all, the honest answer is the empty
// placement -- the router's "decide by project" value -- rather than an
// invented one.
func TestRestorePlacementIsEmptyWhenNothingCanBeCompared(t *testing.T) {
	for name, tc := range map[string]struct {
		meta        domain.SessionMetadata
		projectPath string
	}{
		"no workspace path":           {domain.SessionMetadata{WorkspaceRepoPath: "/repos/x"}, "/repos/x"},
		"no repo path and no project": {domain.SessionMetadata{WorkspacePath: "/home/u/.ao/worktrees/x"}, ""},
		"nothing recorded":            {domain.SessionMetadata{}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := sessionmanager.PlacementFromSessionFacts(tc.meta, tc.projectPath); got != "" {
				t.Fatalf("placement = %q, want the empty placement", got)
			}
		})
	}
}
