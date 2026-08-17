package workflow

import (
	stdctx "context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
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
	if _, err := store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-work-" + taskID, WorkflowRunID: childID, WorkflowStepID: &workStepID,
		ProjectID: projectID, SessionID: &sessID, Branch: "feature/" + taskID,
		WorktreePath: worktreePath, DurablePhase: "work_completed", PayloadVersion: "v1",
		RetryState: "{}", CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed work checkpoint: %v", err)
	}

	task := domain.WorkflowTask{ID: taskID, WorkflowRunID: masterRun.ID, Title: "Task " + taskID}
	detail := RunDetail{Run: createdRun, Steps: []StepDetail{{Step: createdSteps[0]}}}
	return masterRun, task, detail
}

func newIntegrationCoordinator(t *testing.T, mat *fakeMaterializer) (*Coordinator, *sqlite.Store, stdctx.Context) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p-multi", Path: t.TempDir(), RegisteredAt: time.Now().UTC(), Kind: domain.ProjectKindWorkspace}); err != nil {
		t.Fatalf("seed multi-repo project: %v", err)
	}
	coord := New(Deps{Store: store, Projects: store, WorkspaceFacts: mat, Clock: func() time.Time { return time.Now().UTC() }})
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
	if mat.calls != 1 {
		t.Fatalf("expected MaterializeIntegrationCommit to be called once, got %d", mat.calls)
	}
	if mat.lastRef != masterIntegrationRefName(master.ID) {
		t.Fatalf("ref = %q, want %q", mat.lastRef, masterIntegrationRefName(master.ID))
	}
	if mat.lastParent != "" {
		t.Fatalf("expected empty parent SHA for the first promotion, got %q", mat.lastParent)
	}
	if len(mat.lastExclude) == 0 {
		t.Fatal("expected ephemeral-artifact exclude patterns to be passed through")
	}

	state, err := coord.getMasterIntegrationState(ctx, master.ID)
	if err != nil {
		t.Fatalf("getMasterIntegrationState: %v", err)
	}
	if state.CurrentSHA != "sha-1" || len(state.CompletedTaskIDs) != 1 || state.CompletedTaskIDs[0] != "task-1" {
		t.Fatalf("unexpected integration state: %+v", state)
	}

	summary, err := coord.buildIntegrationSummary(ctx, master.ID)
	if err != nil {
		t.Fatalf("buildIntegrationSummary: %v", err)
	}
	if summary.Status != "ok" || summary.TasksIntegrated != 1 || summary.CurrentSHA != "sha-1" {
		t.Fatalf("unexpected summary: %+v", summary)
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
	if mat.calls != 1 {
		t.Fatalf("expected exactly one MaterializeIntegrationCommit call across two promotions of the same task, got %d", mat.calls)
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
	if mat.lastParent != "sha-1" {
		t.Fatalf("task 2's promotion parent = %q, want task 1's commit sha-1", mat.lastParent)
	}

	state, err := coord.getMasterIntegrationState(ctx, master.ID)
	if err != nil {
		t.Fatalf("getMasterIntegrationState: %v", err)
	}
	if len(state.CompletedTaskIDs) != 2 || state.CompletedTaskIDs[0] != "task-1" || state.CompletedTaskIDs[1] != "task-2" {
		t.Fatalf("unexpected completed task order: %v", state.CompletedTaskIDs)
	}
	if state.CurrentSHA != "sha-2" {
		t.Fatalf("current sha = %q, want sha-2", state.CurrentSHA)
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
	if mat.lastParent != "sha-1" {
		t.Fatalf("task 2 parent = %q, want sha-1", mat.lastParent)
	}
	mat.commitSHA = "sha-3"
	if err := coord.promoteTaskToIntegration(ctx, master, task3, detail3); err != nil {
		t.Fatalf("promote task 3: %v", err)
	}
	if mat.lastParent != "sha-2" {
		t.Fatalf("task 3 parent = %q, want sha-2 (all previously completed tasks, V1 linearization)", mat.lastParent)
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

// Failure surfacing (§15): a git-level materialization failure is recorded
// distinctly from success, never silently treated as a promotion.
func TestPromoteTaskToIntegration_MaterializeFailureRecordsDistinctCheckpoint(t *testing.T) {
	mat := &fakeMaterializer{err: simpleTestErr("git plumbing failed")}
	coord, store, ctx := newIntegrationCoordinator(t, mat)
	master, task, detail := seedTaskWithWorktree(t, ctx, store, "p", "task-1", "/repos/p/task-1")

	if err := coord.promoteTaskToIntegration(ctx, master, task, detail); err == nil {
		t.Fatal("expected an error when MaterializeIntegrationCommit fails")
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

type simpleTestErr string

func (e simpleTestErr) Error() string { return string(e) }
