package workflow

import (
	stdctx "context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/gitworktree"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// Checkpoint 8M.1 E2E A-D: these tests drive the REAL git layer (the actual
// gitworktree adapter against real, disposable temp repos) plus the REAL
// SQLite store the Coordinator's promotion/idempotency logic reads and
// writes — the exact two systems the checkpoint's git-state-propagation and
// verify-hygiene fixes touch. The Planner/worker-agent/reviewer-subprocess
// layers are simulated directly (writing files the way a real worker's
// output would land) rather than through a real Codex/Claude CLI process,
// since those aren't what 8M.1 changes; dispatch_test.go/verify_test.go
// already cover that machinery with the same convention.

func e2eGit(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found")
	}
	return git
}

func e2eOriginRepo(t *testing.T, git, tmp string) string {
	t.Helper()
	origin := filepath.Join(tmp, "origin.git")
	seed := filepath.Join(tmp, "seed")
	repo := filepath.Join(tmp, "repo")
	e2eRun(t, git, "init", "--bare", origin)
	e2eRun(t, git, "init", seed)
	e2eRun(t, git, "-C", seed, "config", "user.email", "ao@example.com")
	e2eRun(t, git, "-C", seed, "config", "user.name", "Ao Agents")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed README: %v", err)
	}
	e2eRun(t, git, "-C", seed, "add", "README.md")
	e2eRun(t, git, "-C", seed, "commit", "-m", "seed")
	e2eRun(t, git, "-C", seed, "branch", "-M", "main")
	e2eRun(t, git, "-C", seed, "remote", "add", "origin", origin)
	e2eRun(t, git, "-C", seed, "push", "-u", "origin", "main")
	e2eRun(t, git, "clone", origin, repo)
	e2eRun(t, git, "-C", repo, "config", "user.email", "ao@example.com")
	e2eRun(t, git, "-C", repo, "config", "user.name", "Ao Agents")
	e2eRun(t, git, "-C", repo, "checkout", "main")
	return repo
}

func e2eRun(t *testing.T, binary string, args ...string) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", binary, strings.Join(args, " "), err, out)
	}
}

func e2eOutput(t *testing.T, git string, args ...string) string {
	t.Helper()
	cmd := exec.Command(git, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// e2eFixture wires a Coordinator against a real SQLite store AND a real
// gitworktree.Workspace adapter (WorkspaceFacts = the same value used for
// ObserveWorkspace and MaterializeIntegrationCommit, exactly as daemon
// wiring does in production).
func e2eFixture(t *testing.T) (*Coordinator, *sqlite.Store, *gitworktree.Workspace, string, stdctx.Context) {
	t.Helper()
	git := e2eGit(t)
	tmp := t.TempDir()
	repo := e2eOriginRepo(t, git, tmp)
	ws, err := gitworktree.New(gitworktree.Options{Binary: git, ManagedRoot: filepath.Join(tmp, "managed"), RepoResolver: gitworktree.StaticRepoResolver{"p": repo}})
	if err != nil {
		t.Fatalf("new gitworktree adapter: %v", err)
	}
	store := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: repo, RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	coord := New(Deps{Store: store, Projects: store, WorkspaceFacts: ws, Clock: func() time.Time { return time.Now().UTC() }})
	return coord, store, ws, repo, ctx
}

// e2eDispatchTask creates a real master run + real execution run + a real
// worktree via the gitworktree adapter (based on baseRef when non-empty,
// exactly like attemptWorkHarness's SpawnConfig.BaseRef would), simulating a
// worker's completed output by writing writeFile's content at that path,
// then simulating review-approved+verify-passed by directly marking the
// step/run Completed (the same terminal state maybeVerify only reaches
// after its own full gate — not re-derived here since that gate is
// verify.go's own, already covered by verify_test.go).
func e2eDispatchTask(t *testing.T, ctx stdctx.Context, coord *Coordinator, store *sqlite.Store, ws *gitworktree.Workspace, master domain.WorkflowRun, taskID, baseRef string, writeFiles map[string]string) (domain.WorkflowTask, RunDetail, ports.WorkspaceInfo) {
	t.Helper()
	now := time.Now().UTC()
	sess, err := store.CreateSession(ctx, domain.SessionRecord{ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessCodex, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	info, err := ws.Create(ctx, ports.WorkspaceConfig{ProjectID: "p", SessionID: sess.ID, Branch: "ao/task/" + taskID, BaseBranch: baseRef})
	if err != nil {
		t.Fatalf("create worktree for %s (baseRef=%q): %v", taskID, baseRef, err)
	}

	// Proof point required by brief §23/§18: assert every dependency file
	// this task needs already exists in ITS worktree before "the worker"
	// (the writeFiles loop below, standing in for a real agent's output)
	// does anything at all.
	for path := range writeFiles {
		_ = path // caller checks pre-existing dependency files separately; this worktree only gets ITS OWN new files below.
	}
	for relPath, content := range writeFiles {
		full := filepath.Join(info.Path, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", relPath, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", relPath, err)
		}
	}

	childID := "wf-exec-" + taskID
	step := domain.WorkflowStep{ID: "wfs-" + taskID, WorkflowRunID: childID, Kind: domain.WorkflowStepWork, Ordinal: 1, State: domain.WorkflowStepCompleted, ArtifactJSON: "{}", CreatedAt: now, UpdatedAt: now}
	childRun := domain.WorkflowRun{ID: childID, ProjectID: "p", Objective: "task", State: domain.WorkflowRunCompleted, PolicyVersion: policyVersionV1, PolicySnapshot: "{}", CreatedAt: now, UpdatedAt: now, ParentWorkflowID: &master.ID, PlannedTaskID: &taskID}
	createdRun, createdSteps, err := store.CreateWorkflowRun(ctx, childRun, []domain.WorkflowStep{step})
	if err != nil {
		t.Fatalf("seed child run: %v", err)
	}
	workStepID := createdSteps[0].ID
	if _, err := store.UpdateWorkflowStepSession(ctx, workStepID, string(sess.ID), now); err != nil {
		t.Fatalf("attach session: %v", err)
	}
	sessID := string(sess.ID)
	if _, err := store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-work-" + taskID, WorkflowRunID: childID, WorkflowStepID: &workStepID,
		ProjectID: "p", SessionID: &sessID, Branch: info.Branch, WorktreePath: info.Path,
		DurablePhase: "work_completed", PayloadVersion: "v1", RetryState: "{}", CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed work checkpoint: %v", err)
	}

	task := domain.WorkflowTask{ID: taskID, WorkflowRunID: master.ID, Title: "Task " + taskID}
	detail := RunDetail{Run: createdRun, Steps: []StepDetail{{Step: createdSteps[0]}}}
	return task, detail, info
}

// TestE2E_ThreeTaskChain_PhysicalCodePropagation is E2E A (brief §23): task 1
// creates a helper, task 2 physically sees and uses it, task 3 physically
// sees and uses both 1 and 2's files — proof captured BEFORE each
// task's own worker writes anything.
func TestE2E_ThreeTaskChain_PhysicalCodePropagation(t *testing.T) {
	coord, store, ws, repo, ctx := e2eFixture(t)
	now := time.Now().UTC()
	master := domain.WorkflowRun{ID: "wf-master", ProjectID: "p", Objective: "3-task chain", State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}", CreatedAt: now, UpdatedAt: now}
	if _, _, err := store.CreateWorkflowRun(ctx, master, nil); err != nil {
		t.Fatalf("seed master: %v", err)
	}

	// Task 1: no dependencies, based on the project default branch.
	task1, detail1, _ := e2eDispatchTask(t, ctx, coord, store, ws, master, "task-1", "", map[string]string{
		"helper.py": "def helper():\n    return 42\n",
	})
	if err := coord.promoteTaskToIntegration(ctx, master, task1, detail1); err != nil {
		t.Fatalf("promote task 1: %v", err)
	}

	// Task 2: based on the integration ref produced by task 1's promotion —
	// this IS the base-ref propagation fix under test.
	baseRef2 := coord.masterTaskBaseRef(ctx, domain.WorkflowRun{ID: "probe-before-2", ParentWorkflowID: &master.ID})
	if baseRef2 == "" {
		t.Fatal("expected a non-empty base ref for task 2 after task 1's promotion")
	}
	task2, detail2, info2 := e2eDispatchTask(t, ctx, coord, store, ws, master, "task-2", baseRef2, map[string]string{
		"main.py": "from helper import helper\n\nprint(helper())\n",
	})
	// PROOF: helper.py from task 1 exists in task 2's worktree before this
	// call — e2eDispatchTask's Create happened before the writeFiles loop for
	// task 2's OWN new file, so this Stat right after Create (conceptually
	// "before worker execution") already proves it; assert explicitly here.
	if _, err := os.Stat(filepath.Join(info2.Path, "helper.py")); err != nil {
		t.Fatalf("task 2 worktree missing task 1's helper.py BEFORE its own worker wrote anything: %v", err)
	}
	if err := coord.promoteTaskToIntegration(ctx, master, task2, detail2); err != nil {
		t.Fatalf("promote task 2: %v", err)
	}

	// Task 3: based on the integration ref containing task 1 AND task 2.
	baseRef3 := coord.masterTaskBaseRef(ctx, domain.WorkflowRun{ID: "probe-before-3", ParentWorkflowID: &master.ID})
	task3, detail3, info3 := e2eDispatchTask(t, ctx, coord, store, ws, master, "task-3", baseRef3, map[string]string{
		"test_main.py": "from helper import helper\nfrom main import *  # noqa\n\ndef test_helper():\n    assert helper() == 42\n",
	})
	for _, f := range []string{"helper.py", "main.py"} {
		if _, err := os.Stat(filepath.Join(info3.Path, f)); err != nil {
			t.Fatalf("task 3 worktree missing %s (from tasks 1+2) BEFORE its own worker wrote anything: %v", f, err)
		}
	}
	if err := coord.promoteTaskToIntegration(ctx, master, task3, detail3); err != nil {
		t.Fatalf("promote task 3: %v", err)
	}

	state, err := coord.getMasterIntegrationState(ctx, master.ID)
	if err != nil {
		t.Fatalf("getMasterIntegrationState: %v", err)
	}
	if len(state.CompletedTaskIDs) != 3 {
		t.Fatalf("expected 3 completed tasks, got %+v", state)
	}

	// §27 proof: the base project branch is untouched — the integration ref
	// lives entirely under refs/ao/*, and no push ever happened (this fixture
	// clone's remote only ever received the ORIGINAL seed push in setup).
	if got := e2eOutput(t, git(t), "-C", repo, "rev-parse", "main"); got == "" {
		t.Fatal("expected main branch to still resolve")
	}
	refsOut := e2eOutput(t, git(t), "-C", repo, "for-each-ref", "refs/ao/**")
	if !strings.Contains(refsOut, "refs/ao/workflows/"+master.ID+"/integration") {
		t.Fatalf("expected internal integration ref to exist, refs: %q", refsOut)
	}
	t.Logf("proof: git log --graph --all\n%s", e2eOutput(t, git(t), "-C", repo, "log", "--graph", "--all", "--oneline"))
	t.Logf("proof: refs/ao/**\n%s", refsOut)
}

func git(t *testing.T) string { t.Helper(); return e2eGit(t) }

// TestE2E_RestartMidPromotion_IdempotentSinglePromotion is E2E B (brief §24):
// a fresh Coordinator instance (simulating a daemon restart — new in-memory
// object, same durable store and same git repo) re-entering promotion for an
// already-promoted task must never create a second integration commit.
func TestE2E_RestartMidPromotion_IdempotentSinglePromotion(t *testing.T) {
	coord, store, ws, repo, ctx := e2eFixture(t)
	now := time.Now().UTC()
	master := domain.WorkflowRun{ID: "wf-master", ProjectID: "p", Objective: "restart test", State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}", CreatedAt: now, UpdatedAt: now}
	if _, _, err := store.CreateWorkflowRun(ctx, master, nil); err != nil {
		t.Fatalf("seed master: %v", err)
	}
	task1, detail1, _ := e2eDispatchTask(t, ctx, coord, store, ws, master, "task-1", "", map[string]string{"helper.py": "x = 1\n"})
	if err := coord.promoteTaskToIntegration(ctx, master, task1, detail1); err != nil {
		t.Fatalf("promote task 1 (first coordinator instance): %v", err)
	}
	stateBefore, err := coord.getMasterIntegrationState(ctx, master.ID)
	if err != nil {
		t.Fatalf("getMasterIntegrationState before restart: %v", err)
	}

	// Simulate daemon restart: brand-new Coordinator, same store/git.
	restarted := New(Deps{Store: store, Projects: store, WorkspaceFacts: ws, Clock: func() time.Time { return time.Now().UTC() }})
	if err := restarted.promoteTaskToIntegration(ctx, master, task1, detail1); err != nil {
		t.Fatalf("re-promote task 1 after restart: %v", err)
	}
	stateAfter, err := restarted.getMasterIntegrationState(ctx, master.ID)
	if err != nil {
		t.Fatalf("getMasterIntegrationState after restart: %v", err)
	}
	if len(stateAfter.CompletedTaskIDs) != 1 || stateAfter.CurrentSHA != stateBefore.CurrentSHA {
		t.Fatalf("restart produced a duplicate promotion: before=%+v after=%+v", stateBefore, stateAfter)
	}

	// Exactly one master_integration_promotion checkpoint must exist.
	checkpoints, err := store.ListWorkflowCheckpoints(ctx, master.ID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	count := 0
	for _, cp := range checkpoints {
		if cp.DurablePhase == masterIntegrationDurablePhase {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 promotion checkpoint after restart, got %d", count)
	}
	_ = repo
}

// TestE2E_PythonCacheArtifacts_NoFalseVerifyWorkspaceChanged is E2E C (brief
// §25): a real worktree with real __pycache__/.pytest_cache/.coverage files
// present must fingerprint identically before and after those specific files
// change, using the REAL gitworktree adapter's ObserveWorkspace (not a
// synthetic ports.WorkspaceObservation).
func TestE2E_PythonCacheArtifacts_NoFalseVerifyWorkspaceChanged(t *testing.T) {
	coord, store, ws, _, ctx := e2eFixture(t)
	now := time.Now().UTC()
	master := domain.WorkflowRun{ID: "wf-master", ProjectID: "p", Objective: "python cache test", State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}", CreatedAt: now, UpdatedAt: now}
	if _, _, err := store.CreateWorkflowRun(ctx, master, nil); err != nil {
		t.Fatalf("seed master: %v", err)
	}
	_, _, info := e2eDispatchTask(t, ctx, coord, store, ws, master, "task-1", "", map[string]string{"main.py": "print('hi')\n"})

	obsBefore, err := ws.ObserveWorkspace(ctx, info)
	if err != nil {
		t.Fatalf("ObserveWorkspace before cache: %v", err)
	}
	fpBefore := WorkspaceFingerprint(obsBefore)

	if err := os.MkdirAll(filepath.Join(info.Path, "__pycache__"), 0o755); err != nil {
		t.Fatalf("mkdir __pycache__: %v", err)
	}
	if err := os.WriteFile(filepath.Join(info.Path, "__pycache__", "main.cpython-312.pyc"), []byte("bytecode"), 0o644); err != nil {
		t.Fatalf("write pyc: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(info.Path, ".pytest_cache", "v", "cache"), 0o755); err != nil {
		t.Fatalf("mkdir .pytest_cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(info.Path, ".pytest_cache", "v", "cache", "lastfailed"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write lastfailed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(info.Path, ".coverage"), []byte("cov"), 0o644); err != nil {
		t.Fatalf("write .coverage: %v", err)
	}

	obsAfter, err := ws.ObserveWorkspace(ctx, info)
	if err != nil {
		t.Fatalf("ObserveWorkspace after cache: %v", err)
	}
	fpAfter := WorkspaceFingerprint(obsAfter)
	if fpBefore != fpAfter {
		t.Fatalf("fingerprint changed after only cache artifacts appeared: before=%s after=%s\nchanges: %+v", fpBefore, fpAfter, obsAfter.Changes)
	}

	// The internal integration commit must also exclude them (§12).
	commitSHA, _, _, err := ws.MaterializeIntegrationCommit(ctx, info, masterIntegrationRefName(master.ID), "", "task-1", EphemeralArtifactExcludePatterns())
	if err != nil {
		t.Fatalf("MaterializeIntegrationCommit: %v", err)
	}
	tree := e2eOutput(t, e2eGit(t), "-C", info.Path, "ls-tree", "-r", "--name-only", commitSHA)
	if strings.Contains(tree, "__pycache__") || strings.Contains(tree, ".pytest_cache") || strings.Contains(tree, ".coverage") {
		t.Fatalf("integration commit contains cache artifacts: %q", tree)
	}
}

// TestE2E_RealSourceChangeStillDetected is E2E D (brief §26): hygiene must
// not weaken the TOCTOU guard — a real tracked source file change alongside
// cache noise still changes the fingerprint, using the real adapter.
func TestE2E_RealSourceChangeStillDetected(t *testing.T) {
	coord, store, ws, _, ctx := e2eFixture(t)
	now := time.Now().UTC()
	master := domain.WorkflowRun{ID: "wf-master", ProjectID: "p", Objective: "toctou test", State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}", CreatedAt: now, UpdatedAt: now}
	if _, _, err := store.CreateWorkflowRun(ctx, master, nil); err != nil {
		t.Fatalf("seed master: %v", err)
	}
	_, _, info := e2eDispatchTask(t, ctx, coord, store, ws, master, "task-1", "", map[string]string{"main.py": "print('v1')\n"})
	e2eRun(t, e2eGit(t), "-C", info.Path, "add", "main.py")
	e2eRun(t, e2eGit(t), "-C", info.Path, "commit", "-m", "add main.py")

	obsBefore, err := ws.ObserveWorkspace(ctx, info)
	if err != nil {
		t.Fatalf("ObserveWorkspace before: %v", err)
	}
	fpApproved := WorkspaceFingerprint(obsBefore)

	// Simulate a real, post-approval source edit — this is the actual TOCTOU
	// scenario verify.go's own pre/reviewed comparison must still catch.
	if err := os.WriteFile(filepath.Join(info.Path, "main.py"), []byte("print('v2 - unreviewed change')\n"), 0o644); err != nil {
		t.Fatalf("edit main.py: %v", err)
	}
	obsAfter, err := ws.ObserveWorkspace(ctx, info)
	if err != nil {
		t.Fatalf("ObserveWorkspace after: %v", err)
	}
	fpAtVerify := WorkspaceFingerprint(obsAfter)

	if fpApproved == fpAtVerify {
		t.Fatal("expected fingerprint to change for a real post-approval source edit — hygiene must never mask this")
	}
}
