package workflow

// Checkpoint 8P-E.13B: the direct-branch promotion rule and its safety
// invariants, white-box against the real sqlite store.
//
// The incident these cover: wf-2df5d6dd's task 1 passed review AND
// verification, its result was committed on feat/engineering-control-center by
// the child's own autonomous local commit, and the master still could not
// promote it — promoteTaskToIntegration called MaterializeIntegrationCommit,
// which the direct-branch adapter refuses by design, so every reconcile pass
// recorded master_integration_promotion_failed and task 2 never started.

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// directBranchFacts mirrors the real directbranch.Workspace's contract at the
// WorkspaceFacts boundary: it can observe the repository, and it refuses
// MaterializeIntegrationCommit with the exact sentinel the adapter returns
// (ports.ErrWorkspaceOperationUnsupported). Any promotion path that reaches
// that method in direct-branch mode therefore fails here exactly as it failed
// in production.
type directBranchFacts struct {
	obs             ports.WorkspaceObservation
	observeErr      error
	observed        []ports.WorkspaceInfo
	materializeCall int
}

func (f *directBranchFacts) ObserveWorkspace(_ stdctx.Context, info ports.WorkspaceInfo) (ports.WorkspaceObservation, error) {
	f.observed = append(f.observed, info)
	if f.observeErr != nil {
		return ports.WorkspaceObservation{}, f.observeErr
	}
	obs := f.obs
	if obs.Path == "" {
		obs.Path = info.Path
	}
	return obs, nil
}

func (f *directBranchFacts) MaterializeIntegrationCommit(stdctx.Context, ports.WorkspaceInfo, string, string, string, []string) (string, string, bool, error) {
	f.materializeCall++
	return "", "", false, ports.ErrWorkspaceOperationUnsupported
}

const (
	dbTestRepo   = "/repos/agent-orchestrator"
	dbTestBranch = "feat/engineering-control-center"
)

// newDirectBranchCoordinator seeds a project in direct_branch execution mode
// (the real ProjectConfig field production reads, not a test-only flag).
func newDirectBranchCoordinator(t *testing.T, facts *directBranchFacts) (*Coordinator, *sqlite.Store, stdctx.Context) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	now := time.Now().UTC()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID: "p", Path: t.TempDir(), RegisteredAt: now,
		Config: domain.ProjectConfig{DefaultBranch: dbTestBranch, ExecutionMode: domain.ExecutionDirectBranch},
	}); err != nil {
		t.Fatalf("seed direct-branch project: %v", err)
	}
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p-worktree", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatalf("seed isolated-worktree project: %v", err)
	}
	coord := New(Deps{Store: store, Projects: store, WorkspaceFacts: facts, Clock: func() time.Time { return time.Now().UTC() }})
	return coord, store, ctx
}

// seedDirectBranchTask builds the durable shape a completed direct-branch child
// leaves behind: a work step whose checkpoint carries the repository path and
// branch, and — when commitSHA is non-empty — the autonomous_local_commit
// checkpoint completeVerifiedRun writes while the branch lock is still held.
func seedDirectBranchTask(t *testing.T, ctx stdctx.Context, store *sqlite.Store, projectID, taskID, commitSHA string) (domain.WorkflowRun, domain.WorkflowTask, RunDetail) {
	t.Helper()
	master, task, detail := seedTaskWithWorktree(t, ctx, store, projectID, taskID, dbTestRepo)
	// seedTaskWithWorktree names the branch after the task; direct-branch runs
	// all share the project's configured branch instead, so the work step's
	// newest checkpoint (the one promotion reads its repository facts from)
	// has to carry that branch and the same session.
	workStepID := detail.Steps[0].Step.ID
	prior, ok, err := store.GetLatestWorkflowCheckpointByStep(ctx, workStepID)
	if err != nil || !ok {
		t.Fatalf("read seeded work checkpoint: %v (found=%v)", err, ok)
	}
	if _, err := store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-work-branch-" + taskID, WorkflowRunID: detail.Run.ID,
		WorkflowStepID: &workStepID, ProjectID: projectID, SessionID: prior.SessionID,
		Branch: dbTestBranch, WorktreePath: dbTestRepo, DurablePhase: "worker_observed_completed",
		PayloadVersion: "v1", RetryState: "{}", CreatedAt: time.Now().UTC().Add(time.Second),
	}); err != nil {
		t.Fatalf("seed work branch checkpoint: %v", err)
	}
	if commitSHA != "" {
		if _, err := store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
			ID: "wfc-commit-" + taskID, WorkflowRunID: detail.Run.ID, ProjectID: projectID,
			Branch: dbTestBranch, WorktreePath: dbTestRepo, HeadSHA: commitSHA,
			NextAction:   "local_commit_created: " + commitSHA + " on " + dbTestBranch,
			DurablePhase: autonomousLocalCommitPhase, PayloadVersion: "v1", RetryState: "{}",
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed autonomous local commit checkpoint: %v", err)
		}
	}
	return master, task, detail
}

func seedVerifyPassed(t *testing.T, ctx stdctx.Context, store *sqlite.Store, child RunDetail, projectID, fingerprint string) {
	t.Helper()
	payload, _ := json.Marshal(VerifyResult{Version: verifyResultVersion, Passed: true, PostFingerprint: fingerprint})
	if _, err := store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-verify-" + child.Run.ID, WorkflowRunID: child.Run.ID, ProjectID: projectID,
		RetryState: string(payload), DurablePhase: verifyResultPhase, PayloadVersion: verifyResultVersion,
		FingerprintAfter: fingerprint, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed verify result checkpoint: %v", err)
	}
}

func promotionCheckpoints(t *testing.T, ctx stdctx.Context, store *sqlite.Store, runID, phase string) []domain.WorkflowCheckpoint {
	t.Helper()
	cps, err := store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	var out []domain.WorkflowCheckpoint
	for _, cp := range cps {
		if cp.DurablePhase == phase {
			out = append(out, cp)
		}
	}
	return out
}

// The regression itself: a verified, committed direct-branch task is promoted
// without ever touching the unsupported worktree-style integration operation.
func TestDirectBranchPromotion_ProvenCommitIsPromotedWithoutMaterialize(t *testing.T) {
	facts := &directBranchFacts{obs: ports.WorkspaceObservation{Branch: dbTestBranch, HeadSHA: "commit-1"}}
	coord, store, ctx := newDirectBranchCoordinator(t, facts)
	master, task, detail := seedDirectBranchTask(t, ctx, store, "p", "task-1", "commit-1")

	if err := coord.promoteTaskToIntegration(ctx, master, task, detail); err != nil {
		t.Fatalf("promoteTaskToIntegration: %v", err)
	}
	if facts.materializeCall != 0 {
		t.Fatalf("MaterializeIntegrationCommit calls = %d, want 0 in direct-branch mode", facts.materializeCall)
	}

	state, err := coord.getMasterIntegrationState(ctx, master.ID)
	if err != nil {
		t.Fatalf("getMasterIntegrationState: %v", err)
	}
	if len(state.CompletedTaskIDs) != 1 || state.CompletedTaskIDs[0] != "task-1" {
		t.Fatalf("completed task ids = %v, want [task-1]", state.CompletedTaskIDs)
	}
	if state.CurrentSHA != "commit-1" {
		t.Fatalf("current sha = %q, want the verified commit on the target branch", state.CurrentSHA)
	}
	if state.Mode != masterIntegrationModeDirectBranch {
		t.Fatalf("state mode = %q, want %q", state.Mode, masterIntegrationModeDirectBranch)
	}
	// A direct-branch master must never hand a child a refs/ao/* base ref: the
	// ref does not exist, and the code is already on the branch.
	if ref := coord.masterTaskBaseRef(ctx, domain.WorkflowRun{ID: "probe", ParentWorkflowID: &master.ID}); ref != "" {
		t.Fatalf("base ref after a direct-branch promotion = %q, want empty", ref)
	}
	summary, err := coord.buildIntegrationSummary(ctx, master.ID)
	if err != nil {
		t.Fatalf("buildIntegrationSummary: %v", err)
	}
	if summary.Status != "ok" || summary.TasksIntegrated != 1 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
}

// A: the target branch moved after approval/verification. The reviewed result
// is no longer what the branch holds, so nothing may be promoted.
func TestDirectBranchPromotion_TargetMovedAfterVerifyIsNotSilentlyPromoted(t *testing.T) {
	facts := &directBranchFacts{obs: ports.WorkspaceObservation{Branch: dbTestBranch, HeadSHA: "someone-elses-commit"}}
	coord, store, ctx := newDirectBranchCoordinator(t, facts)
	master, task, detail := seedDirectBranchTask(t, ctx, store, "p", "task-1", "commit-1")

	err := coord.promoteTaskToIntegration(ctx, master, task, detail)
	if err == nil {
		t.Fatal("expected promotion to refuse a branch that moved after verification")
	}
	if !errors.Is(err, errIntegrationFailed) {
		t.Fatalf("error = %v, want an integration failure", err)
	}
	state, sErr := coord.getMasterIntegrationState(ctx, master.ID)
	if sErr != nil {
		t.Fatalf("getMasterIntegrationState: %v", sErr)
	}
	if len(state.CompletedTaskIDs) != 0 {
		t.Fatalf("nothing may be recorded as promoted: %+v", state)
	}
	if state.LastErrorClass != string(domain.WorkflowErrorIntegrationFailed) {
		t.Fatalf("expected a recorded integration failure, got %+v", state)
	}
}

// A (branch identity): the repository is not even on the branch the verified
// result was produced on.
func TestDirectBranchPromotion_DifferentBranchIsRefused(t *testing.T) {
	facts := &directBranchFacts{obs: ports.WorkspaceObservation{Branch: "main", HeadSHA: "commit-1"}}
	coord, store, ctx := newDirectBranchCoordinator(t, facts)
	master, task, detail := seedDirectBranchTask(t, ctx, store, "p", "task-1", "commit-1")

	if err := coord.promoteTaskToIntegration(ctx, master, task, detail); err == nil {
		t.Fatal("expected promotion to refuse a repository checked out on another branch")
	}
	if len(promotionCheckpoints(t, ctx, store, master.ID, masterIntegrationDurablePhase)) != 0 {
		t.Fatal("no promotion checkpoint may exist after a refusal")
	}
}

// B: the child left no durable proof that its verified result reached the
// branch (its local-commit policy deferred or forbade the commit). Stop safely
// — never complete the task on the child's word alone.
func TestDirectBranchPromotion_WithoutProofStopsSafely(t *testing.T) {
	facts := &directBranchFacts{obs: ports.WorkspaceObservation{Branch: dbTestBranch, HeadSHA: "commit-1"}}
	coord, store, ctx := newDirectBranchCoordinator(t, facts)
	master, task, detail := seedDirectBranchTask(t, ctx, store, "p", "task-1", "")

	err := coord.promoteTaskToIntegration(ctx, master, task, detail)
	if err == nil {
		t.Fatal("expected promotion to refuse a task with no durable integration proof")
	}
	state, sErr := coord.getMasterIntegrationState(ctx, master.ID)
	if sErr != nil {
		t.Fatalf("getMasterIntegrationState: %v", sErr)
	}
	if len(state.CompletedTaskIDs) != 0 {
		t.Fatalf("nothing may be recorded as promoted: %+v", state)
	}
}

// B (already-clean shape): the verified workspace had nothing left to commit,
// so the verified state IS the branch state — provable by fingerprint, and only
// while the repository holds no uncommitted work.
func TestDirectBranchPromotion_AlreadyCleanVerifiedWorkspaceIsPromoted(t *testing.T) {
	obs := ports.WorkspaceObservation{Path: dbTestRepo, Branch: dbTestBranch, HeadSHA: "commit-0"}
	facts := &directBranchFacts{obs: obs}
	coord, store, ctx := newDirectBranchCoordinator(t, facts)
	master, task, detail := seedDirectBranchTask(t, ctx, store, "p", "task-1", "")
	seedVerifyPassed(t, ctx, store, detail, "p", WorkspaceFingerprint(obs))

	if err := coord.promoteTaskToIntegration(ctx, master, task, detail); err != nil {
		t.Fatalf("promoteTaskToIntegration: %v", err)
	}
	state, err := coord.getMasterIntegrationState(ctx, master.ID)
	if err != nil {
		t.Fatalf("getMasterIntegrationState: %v", err)
	}
	if state.CurrentSHA != "commit-0" || len(state.CompletedTaskIDs) != 1 {
		t.Fatalf("unexpected integration state: %+v", state)
	}
}

func TestDirectBranchPromotion_UncommittedWorkIsNotIntegrated(t *testing.T) {
	obs := ports.WorkspaceObservation{
		Path: dbTestRepo, Branch: dbTestBranch, HeadSHA: "commit-0", Dirty: true,
		Changes: []ports.WorkspaceChange{{Path: "frontend/src/Board.tsx", Status: " M"}},
	}
	facts := &directBranchFacts{obs: obs}
	coord, store, ctx := newDirectBranchCoordinator(t, facts)
	master, task, detail := seedDirectBranchTask(t, ctx, store, "p", "task-1", "")
	seedVerifyPassed(t, ctx, store, detail, "p", WorkspaceFingerprint(obs))

	if err := coord.promoteTaskToIntegration(ctx, master, task, detail); err == nil {
		t.Fatal("expected an uncommitted verified result to be refused: it is not durably integrated")
	}
}

func TestDirectBranchPromotion_FingerprintDriftIsRefused(t *testing.T) {
	facts := &directBranchFacts{obs: ports.WorkspaceObservation{Path: dbTestRepo, Branch: dbTestBranch, HeadSHA: "commit-0"}}
	coord, store, ctx := newDirectBranchCoordinator(t, facts)
	master, task, detail := seedDirectBranchTask(t, ctx, store, "p", "task-1", "")
	seedVerifyPassed(t, ctx, store, detail, "p", "a-fingerprint-from-a-state-that-is-gone")

	if err := coord.promoteTaskToIntegration(ctx, master, task, detail); err == nil {
		t.Fatal("expected a workspace that no longer matches the verified fingerprint to be refused")
	}
}

// An unobservable repository is a stop, never a pass.
func TestDirectBranchPromotion_UnobservableRepositoryStopsSafely(t *testing.T) {
	facts := &directBranchFacts{observeErr: errors.New("not a git repository")}
	coord, store, ctx := newDirectBranchCoordinator(t, facts)
	master, task, detail := seedDirectBranchTask(t, ctx, store, "p", "task-1", "commit-1")

	if err := coord.promoteTaskToIntegration(ctx, master, task, detail); err == nil {
		t.Fatal("expected promotion to refuse when the target branch cannot be observed")
	}
}

// C: isolated-worktree projects are untouched — they still materialize a real
// integration commit, and never take the direct-branch path.
func TestDirectBranchPromotion_IsolatedWorktreeStillMaterializes(t *testing.T) {
	mat := &fakeMaterializer{commitSHA: "sha-1", treeSHA: "tree-1"}
	store := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	coord := New(Deps{Store: store, Projects: store, WorkspaceFacts: mat, Clock: func() time.Time { return time.Now().UTC() }})
	master, task, detail := seedTaskWithWorktree(t, ctx, store, "p", "task-1", "/repos/p/task-1")

	if err := coord.promoteTaskToIntegration(ctx, master, task, detail); err != nil {
		t.Fatalf("promoteTaskToIntegration: %v", err)
	}
	if mat.calls != 1 {
		t.Fatalf("MaterializeIntegrationCommit calls = %d, want 1 for an isolated-worktree project", mat.calls)
	}
	state, err := coord.getMasterIntegrationState(ctx, master.ID)
	if err != nil {
		t.Fatalf("getMasterIntegrationState: %v", err)
	}
	if state.Mode != "" {
		t.Fatalf("isolated-worktree promotions must not be marked direct-branch, got %q", state.Mode)
	}
	if ref := coord.masterTaskBaseRef(ctx, domain.WorkflowRun{ID: "probe", ParentWorkflowID: &master.ID}); ref != masterIntegrationRefName(master.ID) {
		t.Fatalf("isolated-worktree base ref = %q, want the integration ref", ref)
	}
}

// D (promotion level): an unchanged deterministic failure is recorded once, not
// once per reconcile pass. reconcileMasterTasks runs on every GetRun, so before
// this the incident produced an identical checkpoint roughly every 2 seconds.
func TestDirectBranchPromotion_RepeatedIdenticalFailureRecordsOneCheckpoint(t *testing.T) {
	facts := &directBranchFacts{obs: ports.WorkspaceObservation{Branch: dbTestBranch, HeadSHA: "someone-elses-commit"}}
	coord, store, ctx := newDirectBranchCoordinator(t, facts)
	master, task, detail := seedDirectBranchTask(t, ctx, store, "p", "task-1", "commit-1")

	for i := 0; i < 5; i++ {
		if err := coord.promoteTaskToIntegration(ctx, master, task, detail); err == nil {
			t.Fatalf("pass %d: expected the promotion to keep failing", i)
		}
	}
	failures := promotionCheckpoints(t, ctx, store, master.ID, masterIntegrationFailureDurablePhase)
	if len(failures) != 1 {
		t.Fatalf("integration failure checkpoints = %d, want exactly 1 for one unchanged condition", len(failures))
	}

	// A genuinely different failure is still recorded: dedup is per condition,
	// not a mute button.
	facts.obs.Branch = "main"
	if err := coord.promoteTaskToIntegration(ctx, master, task, detail); err == nil {
		t.Fatal("expected the new failure to be reported")
	}
	if got := len(promotionCheckpoints(t, ctx, store, master.ID, masterIntegrationFailureDurablePhase)); got != 2 {
		t.Fatalf("integration failure checkpoints = %d, want 2 (a new, different condition)", got)
	}
}

// F (durable-recovery half): promotion reads only the child's durable ledger
// and the live repository, so a brand-new Coordinator over the same store — a
// daemon restarted between the child's verification and the master's promotion
// — converges to the same promotion, exactly once.
func TestDirectBranchPromotion_SurvivesRestartAndStaysIdempotent(t *testing.T) {
	facts := &directBranchFacts{obs: ports.WorkspaceObservation{Branch: dbTestBranch, HeadSHA: "commit-1"}}
	coord, store, ctx := newDirectBranchCoordinator(t, facts)
	master, task, detail := seedDirectBranchTask(t, ctx, store, "p", "task-1", "commit-1")

	restarted := New(Deps{Store: store, Projects: store, WorkspaceFacts: facts, Clock: func() time.Time { return time.Now().UTC() }})
	if err := restarted.promoteTaskToIntegration(ctx, master, task, detail); err != nil {
		t.Fatalf("promotion after restart: %v", err)
	}
	if err := coord.promoteTaskToIntegration(ctx, master, task, detail); err != nil {
		t.Fatalf("second promotion of the same task: %v", err)
	}
	if got := len(promotionCheckpoints(t, ctx, store, master.ID, masterIntegrationDurablePhase)); got != 1 {
		t.Fatalf("promotion checkpoints = %d, want exactly 1 (idempotent by task id)", got)
	}
}
