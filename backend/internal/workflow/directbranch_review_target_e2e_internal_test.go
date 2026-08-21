package workflow

import (
	stdctx "context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/directbranch"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type e2eRepoResolver struct{ path string }

func (r e2eRepoResolver) RepoPath(domain.ProjectID) (string, error) { return r.path, nil }

// e2eDirectBranchRepo builds a real, disposable repository checked out on a
// feature branch — the direct_branch execution mode's actual shape: AO works
// in the user's own repo, on the user's own branch, with no worktree of its
// own.
func e2eDirectBranchRepo(t *testing.T) (*directbranch.Workspace, ports.WorkspaceInfo, string) {
	t.Helper()
	git := e2eGit(t)
	repo := t.TempDir()
	e2eRun(t, git, "init", repo)
	e2eRun(t, git, "-C", repo, "config", "user.email", "ao@example.com")
	e2eRun(t, git, "-C", repo, "config", "user.name", "Ao Agents")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	e2eRun(t, git, "-C", repo, "add", "README.md")
	e2eRun(t, git, "-C", repo, "commit", "-m", "seed")
	e2eRun(t, git, "-C", repo, "checkout", "-b", "feat/engineering-control-center")

	ws, err := directbranch.New(directbranch.Options{Binary: git, RepoResolver: e2eRepoResolver{path: repo}})
	if err != nil {
		t.Fatalf("directbranch.New: %v", err)
	}
	return ws, ports.WorkspaceInfo{Path: repo, Branch: "feat/engineering-control-center", SessionID: "sess-e2e", ProjectID: "p"}, repo
}

// TestE2E_DirectBranch_CommitBetweenWorkAndReviewMovesTheFingerprint is the
// fingerprint-level proof of Checkpoint 8P-E.13A.3's root cause, against the
// REAL direct-branch adapter and real git — the exact state transition
// wf-507d9a93 hit in ~/.ao/data.
//
// It pins three facts at once:
//   - committing the worker's own delivered bytes on the same branch DOES
//     change the canonical fingerprint (HEAD moves, the dirty entry
//     disappears), so this is not something fingerprint hygiene may ignore;
//   - therefore the target a review is dispatched against must be observed at
//     dispatch time, or verify compares the live workspace against a state
//     nobody reviewed;
//   - once observed at dispatch, verify's later observation of an untouched
//     workspace matches it exactly.
func TestE2E_DirectBranch_CommitBetweenWorkAndReviewMovesTheFingerprint(t *testing.T) {
	ws, info, repo := e2eDirectBranchRepo(t)
	ctx := stdctx.Background()

	// Work step: the worker edits a frontend file and, per AO's worker
	// contract, never commits.
	if err := os.MkdirAll(filepath.Join(repo, "frontend", "src"), 0o755); err != nil {
		t.Fatalf("mkdir frontend/src: %v", err)
	}
	board := filepath.Join(repo, "frontend", "src", "Board.tsx")
	if err := os.WriteFile(board, []byte("export const Board = () => null;\n"), 0o644); err != nil {
		t.Fatalf("write Board.tsx: %v", err)
	}
	obsWork, err := ws.ObserveWorkspace(ctx, info)
	if err != nil {
		t.Fatalf("ObserveWorkspace at work completion: %v", err)
	}
	fpWork := WorkspaceFingerprint(obsWork)

	// The run parks (needs_attention / branch lock / reviewer capacity) and a
	// concurrent actor commits those same bytes on the same branch.
	e2eRun(t, e2eGit(t), "-C", repo, "add", "frontend/src/Board.tsx")
	e2eRun(t, e2eGit(t), "-C", repo, "commit", "-m", "commit the worker's change")

	obsAtDispatch, err := ws.ObserveWorkspace(ctx, info)
	if err != nil {
		t.Fatalf("ObserveWorkspace at review dispatch: %v", err)
	}
	fpAtDispatch := WorkspaceFingerprint(obsAtDispatch)
	if fpAtDispatch == fpWork {
		t.Fatal("expected the canonical fingerprint to move when the worker's changes are committed — HEAD and the dirty set both changed")
	}

	// The reviewer reads THIS state and approves it. Nothing changes after.
	obsAtVerify, err := ws.ObserveWorkspace(ctx, info)
	if err != nil {
		t.Fatalf("ObserveWorkspace at verify: %v", err)
	}
	if got := WorkspaceFingerprint(obsAtVerify); got != fpAtDispatch {
		t.Fatalf("verify observed %s, want the reviewed state %s — an untouched workspace must fingerprint identically", got, fpAtDispatch)
	}

	// And a real post-approval edit still moves it, so verify_workspace_changed
	// remains reachable for exactly the case it exists to catch.
	if err := os.WriteFile(board, []byte("export const Board = () => 'unreviewed';\n"), 0o644); err != nil {
		t.Fatalf("rewrite Board.tsx: %v", err)
	}
	obsTampered, err := ws.ObserveWorkspace(ctx, info)
	if err != nil {
		t.Fatalf("ObserveWorkspace after tampering: %v", err)
	}
	if WorkspaceFingerprint(obsTampered) == fpAtDispatch {
		t.Fatal("expected a real post-approval edit to change the fingerprint")
	}
}
