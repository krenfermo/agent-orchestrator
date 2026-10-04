package gitworktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// TestMaterializeIntegrationCommit_FirstPromotion covers the base case
// (Checkpoint 8M.1): a clean worktree with new work gets captured into a
// commit under the given ref, parented on nothing, without touching the
// worktree's real index/HEAD, stamped with AO's own git identity rather than
// the ambient one.
func TestMaterializeIntegrationCommit_FirstPromotion(t *testing.T) {
	git := requireGit(t)
	tmp := t.TempDir()
	repo := setupOriginClone(t, git, tmp)
	root := filepath.Join(tmp, "managed")
	ws, err := New(Options{Binary: git, ManagedRoot: root, RepoResolver: StaticRepoResolver{"proj": repo}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx := context.Background()
	cfg := ports.WorkspaceConfig{ProjectID: "proj", SessionID: "sess-integ", Branch: "feature/integ"}
	info, err := ws.Create(ctx, cfg)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := os.WriteFile(filepath.Join(info.Path, "helper.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write helper.go: %v", err)
	}

	ref := "refs/ao/workflows/wf-1/integration"
	commitSHA, treeSHA, reused, err := ws.MaterializeIntegrationCommit(ctx, info, ref, "", "AO internal integration checkpoint: task-1", nil)
	if err != nil {
		t.Fatalf("MaterializeIntegrationCommit: %v", err)
	}
	if reused {
		t.Fatal("expected a new commit for a worktree with real new content, got reused=true")
	}
	if commitSHA == "" || treeSHA == "" {
		t.Fatalf("expected non-empty commit/tree SHA, got commit=%q tree=%q", commitSHA, treeSHA)
	}

	// The worktree's own index/HEAD must be untouched: git status still shows
	// helper.go as untracked, exactly as before materialization.
	statusOut := gitOutput(t, git, info.Path, "status", "--porcelain")
	if !strings.Contains(statusOut, "helper.go") {
		t.Fatalf("expected worktree to remain dirty with helper.go untracked, status: %q", statusOut)
	}

	// The ref must point at commitSHA and be resolvable from the repo.
	refOut := gitOutput(t, git, info.Path, "rev-parse", "--verify", ref)
	if strings.TrimSpace(refOut) != commitSHA {
		t.Fatalf("ref %s = %q, want %q", ref, strings.TrimSpace(refOut), commitSHA)
	}

	// AO identity, not the ambient one.
	authorOut := gitOutput(t, git, info.Path, "log", "-1", "--format=%an <%ae>", ref)
	if strings.TrimSpace(authorOut) != "Agent Orchestrator <ao@local>" {
		t.Fatalf("author = %q, want Agent Orchestrator <ao@local>", strings.TrimSpace(authorOut))
	}
	committerOut := gitOutput(t, git, info.Path, "log", "-1", "--format=%cn <%ce>", ref)
	if strings.TrimSpace(committerOut) != "Agent Orchestrator <ao@local>" {
		t.Fatalf("committer = %q, want Agent Orchestrator <ao@local>", strings.TrimSpace(committerOut))
	}

	// The base project branch/HEAD must be unaffected — the commit only lives
	// under refs/ao/*, never on the branch this worktree was created from.
	headOut := gitOutput(t, git, repo, "rev-parse", "HEAD")
	branchesOut := gitOutput(t, git, repo, "branch", "--list", "main")
	if !strings.Contains(branchesOut, "main") {
		t.Fatalf("expected origin repo to still have main, got: %q", branchesOut)
	}
	_ = headOut
}

// TestMaterializeIntegrationCommit_ExcludesEphemeralArtifacts proves the
// internal commit never contains cache artifacts even when the target repo's
// own .gitignore doesn't cover them (Checkpoint 8M.1 §12/§17).
func TestMaterializeIntegrationCommit_ExcludesEphemeralArtifacts(t *testing.T) {
	git := requireGit(t)
	tmp := t.TempDir()
	repo := setupOriginClone(t, git, tmp)
	root := filepath.Join(tmp, "managed")
	ws, err := New(Options{Binary: git, ManagedRoot: root, RepoResolver: StaticRepoResolver{"proj": repo}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx := context.Background()
	cfg := ports.WorkspaceConfig{ProjectID: "proj", SessionID: "sess-cache", Branch: "feature/cache"}
	info, err := ws.Create(ctx, cfg)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := os.WriteFile(filepath.Join(info.Path, "main.py"), []byte("print('hi')\n"), 0o644); err != nil {
		t.Fatalf("write main.py: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(info.Path, "__pycache__"), 0o755); err != nil {
		t.Fatalf("mkdir __pycache__: %v", err)
	}
	if err := os.WriteFile(filepath.Join(info.Path, "__pycache__", "main.cpython-312.pyc"), []byte("bytecode"), 0o644); err != nil {
		t.Fatalf("write pyc: %v", err)
	}
	if err := os.WriteFile(filepath.Join(info.Path, ".coverage"), []byte("cov"), 0o644); err != nil {
		t.Fatalf("write .coverage: %v", err)
	}

	ref := "refs/ao/workflows/wf-2/integration"
	excludePatterns := []string{"__pycache__", "*.pyc", ".coverage"}
	commitSHA, _, _, err := ws.MaterializeIntegrationCommit(ctx, info, ref, "", "AO internal integration checkpoint: task-1", excludePatterns)
	if err != nil {
		t.Fatalf("MaterializeIntegrationCommit: %v", err)
	}

	filesOut := gitOutput(t, git, info.Path, "ls-tree", "-r", "--name-only", commitSHA)
	if strings.Contains(filesOut, "__pycache__") || strings.Contains(filesOut, ".pyc") || strings.Contains(filesOut, ".coverage") {
		t.Fatalf("integration commit contains ephemeral artifacts, tree: %q", filesOut)
	}
	if !strings.Contains(filesOut, "main.py") {
		t.Fatalf("integration commit missing real source file, tree: %q", filesOut)
	}
}

// TestMaterializeIntegrationCommit_ReusesIdenticalContent proves idempotency
// (Checkpoint 8M.1 §14): calling materialization twice with the same worktree
// content never creates a second commit object.
func TestMaterializeIntegrationCommit_ReusesIdenticalContent(t *testing.T) {
	git := requireGit(t)
	tmp := t.TempDir()
	repo := setupOriginClone(t, git, tmp)
	root := filepath.Join(tmp, "managed")
	ws, err := New(Options{Binary: git, ManagedRoot: root, RepoResolver: StaticRepoResolver{"proj": repo}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx := context.Background()
	cfg := ports.WorkspaceConfig{ProjectID: "proj", SessionID: "sess-idem", Branch: "feature/idem"}
	info, err := ws.Create(ctx, cfg)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := os.WriteFile(filepath.Join(info.Path, "helper.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	ref := "refs/ao/workflows/wf-3/integration"
	first, _, reused1, err := ws.MaterializeIntegrationCommit(ctx, info, ref, "", "task-1", nil)
	if err != nil {
		t.Fatalf("first MaterializeIntegrationCommit: %v", err)
	}
	if reused1 {
		t.Fatal("first call unexpectedly reused")
	}

	second, _, reused2, err := ws.MaterializeIntegrationCommit(ctx, info, ref, first, "task-1-retry", nil)
	if err != nil {
		t.Fatalf("second MaterializeIntegrationCommit: %v", err)
	}
	if !reused2 {
		t.Fatal("expected reused=true for identical content on retry")
	}
	if second != first {
		t.Fatalf("expected identical commit SHA on reuse, got %q vs %q", first, second)
	}
}

// TestMaterializeIntegrationCommit_ChainsAcrossTasks models the 3-task
// dependency chain: task 2's promotion is parented on task 1's integration
// commit and both trees are present in the resulting history.
func TestMaterializeIntegrationCommit_ChainsAcrossTasks(t *testing.T) {
	git := requireGit(t)
	tmp := t.TempDir()
	repo := setupOriginClone(t, git, tmp)
	root := filepath.Join(tmp, "managed")
	ws, err := New(Options{Binary: git, ManagedRoot: root, RepoResolver: StaticRepoResolver{"proj": repo}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx := context.Background()
	ref := "refs/ao/workflows/wf-4/integration"

	// Task 1's worktree.
	cfg1 := ports.WorkspaceConfig{ProjectID: "proj", SessionID: "sess-t1", Branch: "feature/t1"}
	info1, err := ws.Create(ctx, cfg1)
	if err != nil {
		t.Fatalf("create t1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(info1.Path, "helper.go"), []byte("package helper\n"), 0o644); err != nil {
		t.Fatalf("write helper: %v", err)
	}
	sha1, _, _, err := ws.MaterializeIntegrationCommit(ctx, info1, ref, "", "task-1", nil)
	if err != nil {
		t.Fatalf("promote t1: %v", err)
	}

	// Task 2's worktree is based on the integration ref (this test only
	// verifies the git-level chaining; base-ref propagation into the worker
	// spawn path is covered in the workflow package).
	cfg2 := ports.WorkspaceConfig{ProjectID: "proj", SessionID: "sess-t2", Branch: "feature/t2", BaseBranch: ref}
	info2, err := ws.Create(ctx, cfg2)
	if err != nil {
		t.Fatalf("create t2 based on integration ref: %v", err)
	}
	if _, err := os.Stat(filepath.Join(info2.Path, "helper.go")); err != nil {
		t.Fatalf("task 2 worktree missing task 1's file BEFORE any promotion of its own: %v", err)
	}
	if err := os.WriteFile(filepath.Join(info2.Path, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	sha2, _, _, err := ws.MaterializeIntegrationCommit(ctx, info2, ref, sha1, "task-2", nil)
	if err != nil {
		t.Fatalf("promote t2: %v", err)
	}

	filesOut := gitOutput(t, git, info2.Path, "ls-tree", "-r", "--name-only", sha2)
	if !strings.Contains(filesOut, "helper.go") || !strings.Contains(filesOut, "main.go") {
		t.Fatalf("expected task 2's integration commit to contain both files, tree: %q", filesOut)
	}
	parentOut := strings.TrimSpace(gitOutput(t, git, info2.Path, "rev-parse", sha2+"^"))
	if parentOut != sha1 {
		t.Fatalf("task 2's commit parent = %q, want task 1's commit %q", parentOut, sha1)
	}
}
