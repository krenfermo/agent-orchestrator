package workflow

import (
	stdctx "context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/integration"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// fakeMaterializer is a controllable WorkspaceFacts fake local to this
// (internal, white-box) test file — distinct from workflow_test's own
// fakeWorkspaceFacts, which external tests can't reach from here.
type fakeMaterializer struct {
	calls       int
	lastInfo    ports.WorkspaceInfo
	lastRef     string
	lastParent  string
	lastExclude []string
	commitSHA   string
	treeSHA     string
	reused      bool
	err         error
}

func (f *fakeMaterializer) ObserveWorkspace(stdctx.Context, ports.WorkspaceInfo) (ports.WorkspaceObservation, error) {
	return ports.WorkspaceObservation{}, nil
}

func (f *fakeMaterializer) MaterializeIntegrationCommit(_ stdctx.Context, info ports.WorkspaceInfo, ref, parentSHA, _ string, excludePatterns []string) (string, string, bool, error) {
	f.calls++
	f.lastInfo = info
	f.lastRef = ref
	f.lastParent = parentSHA
	f.lastExclude = excludePatterns
	if f.err != nil {
		return "", "", false, f.err
	}
	commit := f.commitSHA
	if commit == "" {
		commit = "commit-sha"
	}
	tree := f.treeSHA
	if tree == "" {
		tree = "tree-sha"
	}
	return commit, tree, f.reused, nil
}

// seedTaskWithWorktree creates a real "master" run and a real "execution"
// run whose sole work step carries a completion checkpoint with worktree
// facts, mirroring the shape maybeVerify leaves behind once a task's
// execution run legitimately reaches WorkflowRunCompleted. Returns the
// master run, a domain.WorkflowTask value (not persisted — promoteTaskToIntegration
// only reads its ID/Title), and the RunDetail promoteTaskToIntegration expects.
func seedTaskWithWorktree(t *testing.T, ctx stdctx.Context, store *sqlite.Store, projectID, taskID, worktreePath string) (domain.WorkflowRun, domain.WorkflowTask, RunDetail) {
	t.Helper()
	now := time.Now().UTC()
	sess, err := store.CreateSession(ctx, domain.SessionRecord{ProjectID: domain.ProjectID(projectID), Kind: domain.KindWorker, Harness: domain.HarnessCodex, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}

	masterRun := domain.WorkflowRun{ID: "wf-master-" + taskID, ProjectID: projectID, Objective: "master", State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}", CreatedAt: now, UpdatedAt: now}
	if _, _, err := store.CreateWorkflowRun(ctx, masterRun, nil); err != nil {
		t.Fatalf("seed master run: %v", err)
	}

	childID := "wf-exec-" + taskID
	step := domain.WorkflowStep{ID: "wfs-" + taskID, WorkflowRunID: childID, Kind: domain.WorkflowStepWork, Ordinal: 1, State: domain.WorkflowStepCompleted, ArtifactJSON: "{}", CreatedAt: now, UpdatedAt: now}
	childRun := domain.WorkflowRun{ID: childID, ProjectID: projectID, Objective: "task", State: domain.WorkflowRunCompleted, PolicyVersion: policyVersionV1, PolicySnapshot: "{}", CreatedAt: now, UpdatedAt: now, ParentWorkflowID: &masterRun.ID, PlannedTaskID: &taskID}
	createdRun, createdSteps, err := store.CreateWorkflowRun(ctx, childRun, []domain.WorkflowStep{step})
	if err != nil {
		t.Fatalf("seed child run: %v", err)
	}
	workStepID := createdSteps[0].ID
	if _, err := store.UpdateWorkflowStepSession(ctx, workStepID, string(sess.ID), now); err != nil {
		t.Fatalf("attach session: %v", err)
	}
	sessID := string(sess.ID)
	branch := "feature/" + taskID
	if integrationRepo != "" && projectID == "p" {
		// A real worktree on its own branch with one commit of its own: what
		// the Coordinator has to be able to fast-forward or replay.
		worktreePath = filepath.Join(t.TempDir(), "wt-"+taskID)
		laneGit(t, integrationRepo, "worktree", "add", "-b", branch, worktreePath)
		if err := os.WriteFile(filepath.Join(worktreePath, taskID+".txt"), []byte(taskID+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		laneGit(t, worktreePath, "add", ".")
		laneGit(t, worktreePath, "commit", "-m", taskID)
	}
	if _, err := store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-work-" + taskID, WorkflowRunID: childID, WorkflowStepID: &workStepID,
		ProjectID: projectID, SessionID: &sessID, Branch: branch,
		WorktreePath: worktreePath, DurablePhase: "work_completed", PayloadVersion: "v1",
		RetryState: "{}", CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed work checkpoint: %v", err)
	}

	task := seedPlanTask(t, ctx, store, masterRun.ID, taskID, 1, domain.WorkflowTaskRunning, nil)
	detail := RunDetail{Run: createdRun, Steps: []StepDetail{{Step: createdSteps[0]}}}
	return masterRun, task, detail
}

// seedPlanTask writes a REAL workflow_tasks row.
//
// It used to be a bare struct literal, which was harmless while a task's state
// lived only in memory during a promotion. Migration 0130 made task-level
// attention durable state — a conflict parks the row, reconciliation reads the
// row, a human resume clears the row — so a fixture without a row cannot
// exercise any of it, and would quietly assert nothing.
func seedPlanTask(t *testing.T, ctx stdctx.Context, store *sqlite.Store, runID, taskID string, ordinal int64, state domain.WorkflowTaskState, deps []string) domain.WorkflowTask {
	t.Helper()
	now := time.Now().UTC()
	task := domain.WorkflowTask{
		ID: taskID, WorkflowRunID: runID, PlanStepID: "step-" + taskID, Ordinal: ordinal,
		Title: "Task " + taskID, Description: "seeded task " + taskID,
		AcceptanceCriteriaJSON: "[]", VerifyJSON: "{}", ScopeJSON: "{}",
		State: state, Dependencies: deps, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.InsertWorkflowTasks(ctx, []domain.WorkflowTask{task}); err != nil {
		t.Fatalf("seed plan task %s: %v", taskID, err)
	}
	return task
}

// integrationRepo is the real repository these promotions now run against.
//
// Task 5 made the Integration Coordinator the one route every ready task takes,
// and it answers its questions with git: where the ref points, whether one
// commit contains another, whether a compare-and-set still sees what it read.
// The old fixture handed out paths like "/repos/p/task-1" and a fake
// materializer, which could only ever assert the fixture's own opinion of a
// promotion route that no longer exists.
// It is reset by every fixture constructor rather than left set, because a
// t.TempDir() is deleted when its test ends and a stale handle would send the
// next test's `git worktree add` at a directory that no longer exists.
var integrationRepo string

func newIntegrationCoordinator(t *testing.T, mat *fakeMaterializer) (*Coordinator, *sqlite.Store, stdctx.Context) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	integrationRepo = newInternalTestRepo(t)
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: integrationRepo, RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p-multi", Path: t.TempDir(), RegisteredAt: time.Now().UTC(), Kind: domain.ProjectKindWorkspace}); err != nil {
		t.Fatalf("seed multi-repo project: %v", err)
	}
	coord := New(Deps{Store: store, Projects: store, WorkspaceFacts: mat, IntegrationLocks: newLaneStub(), Clock: func() time.Time { return time.Now().UTC() }})
	return coord, store, ctx
}

// Promotion gate: promoteTaskToIntegration is only ever called by
// reconcileMasterTasks once a task's execution run has already reached
// domain.WorkflowRunCompleted — a state verify.go's maybeVerify only grants
// after review approved-or-skipped, verify passed, and the fingerprint held
// stable (see verify_test.go for that gate's own coverage). This file tests
// promotion itself, not that gate, which is why every fixture here starts
// from an already-Completed execution run.
func TestPromoteTaskToIntegration_Success(t *testing.T) {
	mat := &fakeMaterializer{commitSHA: "sha-1", treeSHA: "tree-1"}
	coord, store, ctx := newIntegrationCoordinator(t, mat)
	master, task, detail := seedTaskWithWorktree(t, ctx, store, "p", "task-1", "/repos/p/task-1")

	if err := coord.promoteTaskToIntegration(ctx, master, task, detail); err != nil {
		t.Fatalf("promoteTaskToIntegration: %v", err)
	}
	// The legacy materializer route is gone: there is one promotion path now.
	if mat.calls != 0 {
		t.Fatalf("MaterializeIntegrationCommit ran %d times; that route no longer exists", mat.calls)
	}
	// It went through the Coordinator, which means it took the lane and left an
	// audit record naming the strategy and bracketing the ref.
	records, err := coord.ListTaskIntegrations(ctx, master.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 {
		t.Fatal("the promotion recorded no integration audit row")
	}
	landed := records[len(records)-1]
	if landed.Outcome != string(integration.OutcomeIntegrated) || landed.Strategy != string(integration.StrategyFastForward) {
		t.Fatalf("record = %+v, want an integrated fast-forward for the first promotion", landed)
	}
	if landed.TargetBeforeSHA != "" {
		t.Fatalf("target-before = %q, want empty: nothing had been integrated yet", landed.TargetBeforeSHA)
	}
	// And the ref really moved, in the repository, to the task's own commit.
	head := laneGit(t, integrationRepo, "rev-parse", masterIntegrationRefName(master.ID))
	if head != landed.TargetAfterSHA {
		t.Fatalf("ref is at %s but the record says %s", head, landed.TargetAfterSHA)
	}

	state, err := coord.getMasterIntegrationState(ctx, master.ID)
	if err != nil {
		t.Fatalf("getMasterIntegrationState: %v", err)
	}
	if state.CurrentSHA != head || len(state.CompletedTaskIDs) != 1 || state.CompletedTaskIDs[0] != "task-1" {
		t.Fatalf("unexpected integration state: %+v (ref head %s)", state, head)
	}

	summary, err := coord.buildIntegrationSummary(ctx, master.ID)
	if err != nil {
		t.Fatalf("buildIntegrationSummary: %v", err)
	}
	if summary.Status != "ok" || summary.TasksIntegrated != 1 || summary.CurrentSHA != head {
		t.Fatalf("unexpected summary: %+v (ref head %s)", summary, head)
	}
}

// Idempotency (§14): a task already recorded as promoted is never
// re-materialized, even if called again (e.g. a restart re-entering
// reconcileMasterTasks for an already-completed task before its state
// transition was durably observed by the caller).
func TestPromoteTaskToIntegration_IdempotentNoOpForAlreadyPromotedTask(t *testing.T) {
	mat := &fakeMaterializer{commitSHA: "sha-1"}
	coord, store, ctx := newIntegrationCoordinator(t, mat)
	master, task, detail := seedTaskWithWorktree(t, ctx, store, "p", "task-1", "/repos/p/task-1")

	if err := coord.promoteTaskToIntegration(ctx, master, task, detail); err != nil {
		t.Fatalf("first promoteTaskToIntegration: %v", err)
	}
	if err := coord.promoteTaskToIntegration(ctx, master, task, detail); err != nil {
		t.Fatalf("second promoteTaskToIntegration: %v", err)
	}
	// Idempotence is now about the promotion itself: the second call must not
	// integrate the task a second time or move the ref again.
	promotions := promotionCheckpoints(t, ctx, store, master.ID, masterIntegrationDurablePhase)
	if len(promotions) != 1 {
		t.Fatalf("promotion checkpoints = %d across two calls, want exactly 1", len(promotions))
	}
	if mat.calls != 0 {
		t.Fatalf("MaterializeIntegrationCommit ran %d times; that route no longer exists", mat.calls)
	}
}

// Chaining (§8/§9 — V1 linearization): task 2's promotion is parented on
// task 1's integration SHA, and masterTaskBaseRef only returns a ref once
// something has actually been promoted.
func TestPromoteTaskToIntegration_ChainsAndDrivesBaseRef(t *testing.T) {
	mat := &fakeMaterializer{}
	coord, store, ctx := newIntegrationCoordinator(t, mat)
	master, task1, detail1 := seedTaskWithWorktree(t, ctx, store, "p", "task-1", "/repos/p/task-1")

	firstRun := domain.WorkflowRun{ID: "run-before-task1", ParentWorkflowID: &master.ID}
	if ref := coord.masterTaskBaseRef(ctx, firstRun); ref != "" {
		t.Fatalf("expected empty base ref before any task is promoted, got %q", ref)
	}

	mat.commitSHA, mat.treeSHA = "sha-1", "tree-1"
	if err := coord.promoteTaskToIntegration(ctx, master, task1, detail1); err != nil {
		t.Fatalf("promote task 1: %v", err)
	}

	secondRun := domain.WorkflowRun{ID: "run-before-task2", ParentWorkflowID: &master.ID}
	if ref := coord.masterTaskBaseRef(ctx, secondRun); ref != masterIntegrationRefName(master.ID) {
		t.Fatalf("base ref after task 1 promoted = %q, want %q", ref, masterIntegrationRefName(master.ID))
	}

	_, task2, detail2 := seedTaskWithWorktree(t, ctx, store, "p", "task-2", "/repos/p/task-2")
	mat.commitSHA, mat.treeSHA = "sha-2", "tree-2"
	if err := coord.promoteTaskToIntegration(ctx, master, task2, detail2); err != nil {
		t.Fatalf("promote task 2: %v", err)
	}
	// Chaining is now visible in the audit: task 2 integrated onto exactly the
	// head task 1 left behind, read under the lane rather than passed in.
	assertChainedOnto(t, ctx, coord, master.ID, 2)

	state, err := coord.getMasterIntegrationState(ctx, master.ID)
	if err != nil {
		t.Fatalf("getMasterIntegrationState: %v", err)
	}
	if len(state.CompletedTaskIDs) != 2 || state.CompletedTaskIDs[0] != "task-1" || state.CompletedTaskIDs[1] != "task-2" {
		t.Fatalf("unexpected completed task order: %v", state.CompletedTaskIDs)
	}
	// The integration state tracks the ref, which the Coordinator moved.
	head := laneGit(t, integrationRepo, "rev-parse", masterIntegrationRefName(master.ID))
	if state.CurrentSHA != head {
		t.Fatalf("current sha = %q, want the ref head %s", state.CurrentSHA, head)
	}
}

// Run task 3 dependency chain — proves the same monotonic base-ref behavior
// holds for a third task, matching the checkpoint brief's E2E A shape at the
// unit level (E2E A itself runs the real gitworktree adapter end-to-end).
func TestPromoteTaskToIntegration_ThreeTaskChain(t *testing.T) {
	mat := &fakeMaterializer{}
	coord, store, ctx := newIntegrationCoordinator(t, mat)
	master, task1, detail1 := seedTaskWithWorktree(t, ctx, store, "p", "task-1", "/repos/p/task-1")
	_, task2, detail2 := seedTaskWithWorktree(t, ctx, store, "p", "task-2", "/repos/p/task-2")
	_, task3, detail3 := seedTaskWithWorktree(t, ctx, store, "p", "task-3", "/repos/p/task-3")

	mat.commitSHA = "sha-1"
	if err := coord.promoteTaskToIntegration(ctx, master, task1, detail1); err != nil {
		t.Fatalf("promote task 1: %v", err)
	}
	mat.commitSHA = "sha-2"
	if err := coord.promoteTaskToIntegration(ctx, master, task2, detail2); err != nil {
		t.Fatalf("promote task 2: %v", err)
	}
	assertChainedOnto(t, ctx, coord, master.ID, 2)
	mat.commitSHA = "sha-3"
	if err := coord.promoteTaskToIntegration(ctx, master, task3, detail3); err != nil {
		t.Fatalf("promote task 3: %v", err)
	}
	assertChainedOnto(t, ctx, coord, master.ID, 3)
}

// assertChainedOnto checks that the nth landed integration started from exactly
// where the previous one finished — V1 linearization, expressed against the
// audit record the Coordinator writes rather than against a fake's parameters.
func assertChainedOnto(t *testing.T, ctx stdctx.Context, coord *Coordinator, masterID string, n int) {
	t.Helper()
	records, err := coord.ListTaskIntegrations(ctx, masterID)
	if err != nil {
		t.Fatal(err)
	}
	var landed []TaskIntegrationRecord
	for _, r := range records {
		if r.Outcome == string(integration.OutcomeIntegrated) {
			landed = append(landed, r)
		}
	}
	if len(landed) != n {
		t.Fatalf("landed integrations = %d, want %d: %+v", len(landed), n, landed)
	}
	prev, cur := landed[n-2], landed[n-1]
	if cur.TargetBeforeSHA != prev.TargetAfterSHA {
		t.Fatalf("integration %d started from %s but %d finished at %s: the chain is broken",
			n, cur.TargetBeforeSHA, n-1, prev.TargetAfterSHA)
	}
}

// §8 scope guard: multi-repo workspace-kind projects are explicitly
// unsupported in 8M.1, never silently degraded.
func TestPromoteTaskToIntegration_RejectsWorkspaceKindProject(t *testing.T) {
	mat := &fakeMaterializer{}
	coord, store, ctx := newIntegrationCoordinator(t, mat)
	master, task, detail := seedTaskWithWorktree(t, ctx, store, "p-multi", "task-1", "/repos/p-multi/task-1")

	err := coord.promoteTaskToIntegration(ctx, master, task, detail)
	if err == nil {
		t.Fatal("expected an error for a multi-repo workspace-kind project")
	}
	if mat.calls != 0 {
		t.Fatalf("expected MaterializeIntegrationCommit to never be called, got %d calls", mat.calls)
	}

	summary, sErr := coord.buildIntegrationSummary(ctx, master.ID)
	if sErr != nil {
		t.Fatalf("buildIntegrationSummary: %v", sErr)
	}
	if summary.Status != "integration_failed" || summary.ErrorClass != string(domain.WorkflowErrorIntegrationFailed) {
		t.Fatalf("unexpected summary after failure: %+v", summary)
	}
}

// Failure surfacing (§15): an integration that cannot be performed is recorded
// distinctly from success, never silently treated as a promotion.
//
// The failure it exercises changed with the route. There is no materializer to
// fail any more; what fails now is an integration whose source has no branch to
// integrate — the Coordinator moves commits, and a task with only a working
// tree has none.
func TestPromoteTaskToIntegration_IntegrationFailureRecordsDistinctCheckpoint(t *testing.T) {
	mat := &fakeMaterializer{}
	coord, store, ctx := newIntegrationCoordinator(t, mat)
	master, task, detail := seedTaskWithWorktree(t, ctx, store, "p", "task-1", "/repos/p/task-1")
	// Strip the branch from the task's durable work record: a worktree with no
	// branch cannot be integrated by any strategy.
	stripTaskBranch(t, ctx, store, detail.Run.ID, "task-1")

	if err := coord.promoteTaskToIntegration(ctx, master, task, detail); err == nil {
		t.Fatal("expected an error when the task has no integrable source")
	}
	state, err := coord.getMasterIntegrationState(ctx, master.ID)
	if err != nil {
		t.Fatalf("getMasterIntegrationState: %v", err)
	}
	if len(state.CompletedTaskIDs) != 0 {
		t.Fatalf("task must not be recorded as completed after a materialization failure: %+v", state)
	}
	if state.LastErrorClass != string(domain.WorkflowErrorIntegrationFailed) {
		t.Fatalf("expected LastErrorClass to be recorded, got %+v", state)
	}
}

// stripTaskBranch re-records a task's work checkpoint without a branch, which is
// what a task that produced only a working tree looks like durably.
func stripTaskBranch(t *testing.T, ctx stdctx.Context, store *sqlite.Store, childRunID, taskID string) {
	t.Helper()
	prior, ok, err := store.GetLatestWorkflowCheckpointByStep(ctx, "wfs-"+taskID)
	if err != nil || !ok {
		t.Fatalf("read work checkpoint: %v (found=%v)", err, ok)
	}
	stepID := "wfs-" + taskID
	if _, err := store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-work-nobranch-" + taskID, WorkflowRunID: childRunID, WorkflowStepID: &stepID,
		ProjectID: prior.ProjectID, SessionID: prior.SessionID,
		WorktreePath: prior.WorktreePath, DurablePhase: "work_completed",
		PayloadVersion: "v1", RetryState: "{}", CreatedAt: time.Now().UTC().Add(time.Second),
	}); err != nil {
		t.Fatalf("seed branchless work checkpoint: %v", err)
	}
}
