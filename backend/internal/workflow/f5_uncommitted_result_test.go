package workflow

import (
	stdctx "context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/directbranch"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/gitworktree"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// f5_uncommitted_result_test.go — the regression suite for F5.
//
// THE INCIDENT, in one line: a three-task objective reported
// `tasksIntegrated: 3, status: ok` and completed while the third task's entire
// deliverable sat as dirty, untracked state in a worktree the lifecycle then
// retired. Two of three workers had run `git commit` on their own; the third
// had not, and nothing asks them to.
//
// These tests use the same fixture the integration e2e suite does, and the
// distinction they turn on is the one e2eCommitWork's own comment already
// names: a task that writes files without committing them is not a task that
// produced an integratable result.

// TestF5_UncommittedTaskResultIsNotIntegrated is the core regression.
//
// A task whose worker wrote real files and never committed them must not be
// reported integrated. Before the fix, promotion resolved the task's branch
// head, found it identical to the target, took the fast-forward path and
// recorded base == head as a successful integration — the exact shape the
// incident left in the ledger.
func TestF5_UncommittedTaskResultIsNotIntegrated(t *testing.T) {
	coord, store, ws, repo, ctx := e2eFixture(t)
	f5WireCommitter(t, coord, repo)
	now := time.Now().UTC()
	master := domain.WorkflowRun{ID: "wf-master-f5", ProjectID: "p", Objective: "uncommitted result", State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}", CreatedAt: now, UpdatedAt: now}
	if _, _, err := store.CreateWorkflowRun(ctx, master, nil); err != nil {
		t.Fatalf("seed master: %v", err)
	}

	// Task 1 behaves like the two healthy workers: it commits.
	task1, detail1, info1 := e2eDispatchTask(t, ctx, coord, store, ws, master, "f5-task-1", "", map[string]string{
		"helper.py": "def helper():\n    return 42\n",
	})
	e2eCommitWork(t, info1.Path, "f5-task-1")
	if err := coord.promoteTaskToIntegration(ctx, master, task1, detail1); err != nil {
		t.Fatalf("promote committed task: %v", err)
	}
	targetBefore := e2eOutput(t, git(t), "-C", repo, "rev-parse", masterIntegrationRefName(master.ID))

	// Task 2 is the incident: real work, never committed.
	baseRef := coord.masterTaskBaseRef(ctx, domain.WorkflowRun{ID: "probe", ParentWorkflowID: &master.ID})
	task2, detail2, info2 := e2eDispatchTask(t, ctx, coord, store, ws, master, "f5-task-2", baseRef, map[string]string{
		"farewell.py": "def farewell(name):\n    return f'Goodbye, {name}.'\n",
	})
	if _, err := os.Stat(filepath.Join(info2.Path, "farewell.py")); err != nil {
		t.Fatalf("fixture broken: the worker's file is not in its worktree: %v", err)
	}
	// Deliberately NO e2eCommitWork here. That is the whole test.

	err := coord.promoteTaskToIntegration(ctx, master, task2, detail2)
	if err == nil {
		t.Fatal("an uncommitted task result was reported as integrated; the deliverable exists only as dirty worktree state")
	}
	if !strings.Contains(err.Error(), "uncommitted") {
		t.Errorf("the refusal should name the uncommitted result, got %v", err)
	}

	// §9/§10: the target must not have moved, and the task must not be counted.
	targetAfter := e2eOutput(t, git(t), "-C", repo, "rev-parse", masterIntegrationRefName(master.ID))
	if targetAfter != targetBefore {
		t.Errorf("the target ref moved for a task with nothing to integrate: %s -> %s", targetBefore, targetAfter)
	}
	state, serr := coord.getMasterIntegrationState(ctx, master.ID)
	if serr != nil {
		t.Fatal(serr)
	}
	for _, id := range state.CompletedTaskIDs {
		if id == task2.ID {
			t.Fatal("tasksIntegrated counted a task whose work was never committed")
		}
	}
	if len(state.CompletedTaskIDs) != 1 {
		t.Errorf("expected only the committed task to count, got %+v", state.CompletedTaskIDs)
	}

	// And the work is still there to be rescued — never cleaned away.
	if _, err := os.Stat(filepath.Join(info2.Path, "farewell.py")); err != nil {
		t.Errorf("the unintegrated deliverable was lost from its worktree: %v", err)
	}
}

// TestF5_CommittedTaskResultStillIntegrates is the other half: the gate must
// refuse uncommitted work without refusing work that IS committed.
func TestF5_CommittedTaskResultStillIntegrates(t *testing.T) {
	coord, store, ws, repo, ctx := e2eFixture(t)
	f5WireCommitter(t, coord, repo)
	now := time.Now().UTC()
	master := domain.WorkflowRun{ID: "wf-master-f5-ok", ProjectID: "p", Objective: "committed result", State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}", CreatedAt: now, UpdatedAt: now}
	if _, _, err := store.CreateWorkflowRun(ctx, master, nil); err != nil {
		t.Fatalf("seed master: %v", err)
	}
	task, detail, info := e2eDispatchTask(t, ctx, coord, store, ws, master, "f5-ok-1", "", map[string]string{
		"farewell.py": "def farewell(name):\n    return f'Goodbye, {name}.'\n",
	})
	e2eCommitWork(t, info.Path, "f5-ok-1")
	if err := coord.promoteTaskToIntegration(ctx, master, task, detail); err != nil {
		t.Fatalf("a committed task must still integrate: %v", err)
	}
	// §30: the deliverable is physically on the target, not merely recorded.
	tree := e2eOutput(t, git(t), "-C", repo, "ls-tree", "-r", "--name-only", masterIntegrationRefName(master.ID))
	if !strings.Contains(tree, "farewell.py") {
		t.Fatalf("the target ref does not contain the task's deliverable; tree=%q", tree)
	}
}

// TestF5_EphemeralArtifactsDoNotBlockIntegration keeps the gate from parking
// real work over a __pycache__ directory: only paths that are actually part of
// a deliverable count as an uncommitted result.
func TestF5_EphemeralArtifactsDoNotBlockIntegration(t *testing.T) {
	coord, store, ws, repo, ctx := e2eFixture(t)
	f5WireCommitter(t, coord, repo)
	now := time.Now().UTC()
	master := domain.WorkflowRun{ID: "wf-master-f5-eph", ProjectID: "p", Objective: "ephemeral", State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}", CreatedAt: now, UpdatedAt: now}
	if _, _, err := store.CreateWorkflowRun(ctx, master, nil); err != nil {
		t.Fatalf("seed master: %v", err)
	}
	task, detail, info := e2eDispatchTask(t, ctx, coord, store, ws, master, "f5-eph-1", "", map[string]string{
		"helper.py": "def helper():\n    return 1\n",
	})
	e2eCommitWork(t, info.Path, "f5-eph-1")
	// A build artifact appears after the commit, exactly as a test run leaves one.
	cache := filepath.Join(info.Path, "__pycache__")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "helper.cpython-312.pyc"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := coord.promoteTaskToIntegration(ctx, master, task, detail); err != nil {
		t.Fatalf("an ephemeral artifact must not block a committed result: %v", err)
	}
	tree := e2eOutput(t, git(t), "-C", repo, "ls-tree", "-r", "--name-only", masterIntegrationRefName(master.ID))
	if !strings.Contains(tree, "helper.py") {
		t.Fatalf("the deliverable is missing from the target; tree=%q", tree)
	}
}

// f5CommitFixture wires the coordinator the way the daemon does for an
// isolated task: a real worktree, a real committer, and the run's own frozen
// isolated placement as the ownership proof.
func f5CommitFixture(t *testing.T, files map[string]string) (*Coordinator, domain.WorkflowRun, domain.WorkflowStep, string) {
	t.Helper()
	coord, store, ws, repo, ctx := e2eFixture(t)
	now := time.Now().UTC()
	master := domain.WorkflowRun{ID: "wf-master-f5c", ProjectID: "p", Objective: "commit boundary", State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}", CreatedAt: now, UpdatedAt: now}
	if _, _, err := store.CreateWorkflowRun(ctx, master, nil); err != nil {
		t.Fatalf("seed master: %v", err)
	}
	_, detail, info := e2eDispatchTask(t, ctx, coord, store, ws, master, "f5c-task", "", files)

	f5WireCommitter(t, coord, repo)
	coord.placements = store
	if froze, err := store.FreezeExecutionPlacement(ctx, domain.ExecutionPlacement{
		ID: "ep-f5c", WorkflowRunID: detail.Run.ID, TaskID: "f5c-task", ProjectID: "p",
		Type: domain.PlacementIsolatedWorktree, RepoPath: repo,
		ExecutionBranch: info.Branch, WorktreePath: info.Path,
		BaseBranch: "main", MergeTarget: "main",
		State: domain.PlacementActive, Provenance: domain.PlacementFrozenAtSelection,
		PlacementGeneration: 1, LifecycleGeneration: 1,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed placement: %v", err)
	} else if !froze {
		t.Fatalf("placement freeze did not land")
	}
	steps, _ := store.ListWorkflowSteps(ctx, detail.Run.ID)
	return coord, detail.Run, steps[0], info.Path
}

// TestF5_IsolatedCommitCapturesTheVerifiedResultExactlyOnce is §3/§6/§17-B:
// the boundary commits an isolated task's pending work, and re-entering it
// (a restart between the commit and its checkpoint) commits nothing more.
func TestF5_IsolatedCommitCapturesTheVerifiedResultExactlyOnce(t *testing.T) {
	coord, run, step, worktree := f5CommitFixture(t, map[string]string{
		"farewell.py": "def farewell(name):\n    return f'Goodbye, {name}.'\n",
	})
	ctx := stdctx.Background()

	if err := coord.autonomousLocalCommit(ctx, run, step); err != nil {
		t.Fatalf("the boundary must commit an isolated task's verified result: %v", err)
	}
	head := strings.TrimSpace(e2eOutput(t, git(t), "-C", worktree, "rev-parse", "HEAD"))
	if status := e2eOutput(t, git(t), "-C", worktree, "status", "--porcelain"); strings.TrimSpace(status) != "" {
		t.Fatalf("the worktree is still dirty after the commit: %q", status)
	}
	tree := e2eOutput(t, git(t), "-C", worktree, "ls-tree", "-r", "--name-only", "HEAD")
	if !strings.Contains(tree, "farewell.py") {
		t.Fatalf("the commit does not contain the deliverable; tree=%q", tree)
	}

	// §6: a second pass (restart, heartbeat, reconciler) must not commit again.
	if err := coord.autonomousLocalCommit(ctx, run, step); err != nil {
		t.Fatalf("re-entering the boundary must be a no-op, got %v", err)
	}
	if again := strings.TrimSpace(e2eOutput(t, git(t), "-C", worktree, "rev-parse", "HEAD")); again != head {
		t.Fatalf("a second pass created another commit: %s -> %s", head, again)
	}
}

// TestF5_IsolatedCommitCreatesNoEmptyCommit is §16: a task that changed
// nothing must not get an empty commit manufactured for it.
func TestF5_IsolatedCommitCreatesNoEmptyCommit(t *testing.T) {
	coord, run, step, worktree := f5CommitFixture(t, nil)
	ctx := stdctx.Background()
	before := strings.TrimSpace(e2eOutput(t, git(t), "-C", worktree, "rev-parse", "HEAD"))

	if err := coord.autonomousLocalCommit(ctx, run, step); err != nil {
		t.Fatalf("a clean worktree is not an error: %v", err)
	}
	if after := strings.TrimSpace(e2eOutput(t, git(t), "-C", worktree, "rev-parse", "HEAD")); after != before {
		t.Fatalf("an empty commit was created for a task with no changes: %s -> %s", before, after)
	}
}

// TestF5_IsolatedCommitLeavesAlreadyCommittedWorkAlone is §19's "worker already
// committed" row: the boundary adds nothing on top of a worker's own commit.
func TestF5_IsolatedCommitLeavesAlreadyCommittedWorkAlone(t *testing.T) {
	coord, run, step, worktree := f5CommitFixture(t, map[string]string{
		"helper.py": "def helper():\n    return 1\n",
	})
	ctx := stdctx.Background()
	e2eCommitWork(t, worktree, "worker-own-commit")
	before := strings.TrimSpace(e2eOutput(t, git(t), "-C", worktree, "rev-parse", "HEAD"))

	if err := coord.autonomousLocalCommit(ctx, run, step); err != nil {
		t.Fatalf("an already-committed result is not an error: %v", err)
	}
	if after := strings.TrimSpace(e2eOutput(t, git(t), "-C", worktree, "rev-parse", "HEAD")); after != before {
		t.Fatalf("the boundary committed on top of the worker's own commit: %s -> %s", before, after)
	}
}

// f5WireCommitter gives a fixture the committer the daemon always wires, which
// is what makes it subject to the F5 capture contract at all.
func f5WireCommitter(t *testing.T, coord *Coordinator, repo string) {
	t.Helper()
	committer, err := directbranch.New(directbranch.Options{
		Binary: e2eGit(t), RepoResolver: gitworktree.StaticRepoResolver{"p": repo},
	})
	if err != nil {
		t.Fatalf("new directbranch committer: %v", err)
	}
	coord.workspaceCommitter = committer
}
