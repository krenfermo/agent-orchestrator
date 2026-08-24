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
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/integration"
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

const dbTestBranch = "feat/engineering-control-center"

// dbTestRepo is the REAL repository the direct-branch fixtures run against,
// set per test by newDirectBranchCoordinator.
//
// It used to be the string "/repos/agent-orchestrator", which was fine while
// direct-branch promotion was its own route and never touched git. It is not
// fine now: the route was deleted and direct-branch integration goes through
// the Integration Coordinator, which answers its questions with git — where
// the ref points, whether one commit contains another. A fixture pointing at a
// path that does not exist would only ever exercise "git could not read the
// repository", which is the opposite of what these tests are about.
var dbTestRepo string

// dbCommit adds one commit to dbTestBranch and returns its real SHA. Each label
// is a distinct file, so two labels are always two distinct commits.
func dbCommit(t *testing.T, label string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dbTestRepo, label+".txt"), []byte(label+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	laneGit(t, dbTestRepo, "add", ".")
	laneGit(t, dbTestRepo, "commit", "-m", label)
	return laneGit(t, dbTestRepo, "rev-parse", "HEAD")
}

// newDirectBranchCoordinator seeds a project in direct_branch execution mode
// (the real ProjectConfig field production reads, not a test-only flag), on a
// real repository checked out on the project's configured branch.
//
// facts is filled in by the caller AFTER this returns, because the observation
// a test wants to make is usually about a commit that only exists once the
// repository does.
func newDirectBranchCoordinator(t *testing.T, facts *directBranchFacts) (*Coordinator, *sqlite.Store, stdctx.Context) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// Clearing the shared handle stops a previous test's (already deleted)
	// TempDir from leaking into this one.
	integrationRepo = ""
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o750); err != nil {
		t.Fatal(err)
	}
	laneGit(t, repo, "init", "--initial-branch="+dbTestBranch)
	laneGit(t, repo, "config", "user.email", "ao@example.com")
	laneGit(t, repo, "config", "user.name", "Ao Agents")
	dbTestRepo = repo
	dbCommit(t, "seed")

	store := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	now := time.Now().UTC()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID: "p", Path: repo, RegisteredAt: now,
		Config: domain.ProjectConfig{DefaultBranch: dbTestBranch, ExecutionMode: domain.ExecutionDirectBranch},
	}); err != nil {
		t.Fatalf("seed direct-branch project: %v", err)
	}
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p-worktree", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatalf("seed isolated-worktree project: %v", err)
	}
	coord := New(Deps{Store: store, Projects: store, WorkspaceFacts: facts, IntegrationLocks: newLaneStub(), Clock: func() time.Time { return time.Now().UTC() }})
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
	markChildReadyForIntegration(t, ctx, store, detail)
	refreshed, err := coordDetailFor(ctx, store, detail.Run.ID)
	if err == nil {
		detail = refreshed
	}
	return master, task, detail
}

// coordDetailFor re-reads a child's steps after the fixture has moved them, so
// the RunDetail handed to promotion describes the run as it now is.
func coordDetailFor(ctx stdctx.Context, store *sqlite.Store, runID string) (RunDetail, error) {
	run, ok, err := store.GetWorkflowRun(ctx, runID)
	if err != nil || !ok {
		return RunDetail{}, err
	}
	steps, err := store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	detail := RunDetail{Run: run}
	for _, s := range steps {
		detail.Steps = append(detail.Steps, StepDetail{Step: s})
	}
	return detail, nil
}

// markChildReadyForIntegration completes the child's review and verify steps.
//
// Task 5 made the Integration Coordinator's readiness gate apply to EVERY
// promotion rather than only to the replay path, so a fixture whose child still
// has a pending review is now correctly refused at the ref. These fixtures are
// about what happens to a task that IS ready, so they have to say so.
func markChildReadyForIntegration(t *testing.T, ctx stdctx.Context, store *sqlite.Store, child RunDetail) {
	t.Helper()
	// The fixture's child has only a work step, so completing the RUN is what
	// says "this task passed review and verification" — the same durable fact
	// production relies on (see taskReadiness).
	run, ok, err := store.GetWorkflowRun(ctx, child.Run.ID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun: %v (found=%v)", err, ok)
	}
	if run.State != domain.WorkflowRunCompleted {
		for _, next := range []domain.WorkflowRunState{domain.WorkflowRunRunning, domain.WorkflowRunCompleted} {
			if run.State == next {
				continue
			}
			if _, err := store.UpdateWorkflowRunState(ctx, child.Run.ID, run.State, next, time.Now().UTC()); err != nil {
				t.Fatalf("complete child run: %v", err)
			}
			run.State = next
		}
	}
	steps, err := store.ListWorkflowSteps(ctx, child.Run.ID)
	if err != nil {
		t.Fatalf("ListWorkflowSteps: %v", err)
	}
	for _, step := range steps {
		if step.Kind != domain.WorkflowStepReview && step.Kind != domain.WorkflowStepVerify {
			continue
		}
		cur := step.State
		for _, next := range []domain.WorkflowStepState{
			domain.WorkflowStepReady, domain.WorkflowStepRunning, domain.WorkflowStepCompleted,
		} {
			if cur == next {
				continue
			}
			if _, err := store.UpdateWorkflowStepState(ctx, step.ID, cur, next, time.Now().UTC()); err != nil {
				t.Fatalf("advance %s step to %s: %v", step.Kind, next, err)
			}
			cur = next
		}
	}
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
	facts := &directBranchFacts{}
	coord, store, ctx := newDirectBranchCoordinator(t, facts)
	head := dbCommit(t, "one")
	facts.obs = dbObservation(head)
	master, task, detail := seedDirectBranchTask(t, ctx, store, "p", "task-1", head)

	if err := coord.promoteTaskToIntegration(ctx, master, task, detail); err != nil {
		t.Fatalf("promoteTaskToIntegration: %v", err)
	}
	if facts.materializeCall != 0 {
		t.Fatalf("MaterializeIntegrationCommit calls = %d, want 0 in direct-branch mode", facts.materializeCall)
	}
	// The whole of Finding 2: it went through the Integration Coordinator, and
	// the Coordinator named the strategy a promotion with no git operation
	// actually used.
	records, rerr := coord.ListTaskIntegrations(ctx, master.ID)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(records) == 0 {
		t.Fatal("a direct-branch promotion left no Coordinator audit row")
	}
	if got := records[len(records)-1].Strategy; got != string(integration.StrategyNoOp) {
		t.Fatalf("strategy = %q, want %q", got, integration.StrategyNoOp)
	}

	state, err := coord.getMasterIntegrationState(ctx, master.ID)
	if err != nil {
		t.Fatalf("getMasterIntegrationState: %v", err)
	}
	if len(state.CompletedTaskIDs) != 1 || state.CompletedTaskIDs[0] != "task-1" {
		t.Fatalf("completed task ids = %v, want [task-1]", state.CompletedTaskIDs)
	}
	if state.CurrentSHA != head {
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
//
// Since Task 5's follow-up this refusal is a TASK-level stop rather than a
// generic integration failure, and that is a deliberate change: the branch
// moving under a verified task is exactly a situation one person resolves for
// one task, and parking the objective on it would stop every sibling that has
// nothing to do with this branch.
func TestDirectBranchPromotion_TargetMovedAfterVerifyIsNotSilentlyPromoted(t *testing.T) {
	facts := &directBranchFacts{}
	coord, store, ctx := newDirectBranchCoordinator(t, facts)
	verified := dbCommit(t, "one")
	someoneElse := dbCommit(t, "two")
	facts.obs = dbObservation(someoneElse)
	master, task, detail := seedDirectBranchTask(t, ctx, store, "p", "task-1", verified)

	err := coord.promoteTaskToIntegration(ctx, master, task, detail)
	if err == nil {
		t.Fatal("expected promotion to refuse a branch that moved after verification")
	}
	if !errors.Is(err, errIntegrationTaskConflict) {
		t.Fatalf("error = %v, want a task-level integration conflict", err)
	}
	state, sErr := coord.getMasterIntegrationState(ctx, master.ID)
	if sErr != nil {
		t.Fatalf("getMasterIntegrationState: %v", sErr)
	}
	if len(state.CompletedTaskIDs) != 0 {
		t.Fatalf("nothing may be recorded as promoted: %+v", state)
	}
	// The stop lives on the task, durably, with the reason on the row.
	assertTaskParked(t, ctx, store, master.ID, task.ID, string(integration.ReasonPreconditionFailed))
}

// A (branch identity): the repository is not even on the branch the verified
// result was produced on.
func TestDirectBranchPromotion_DifferentBranchIsRefused(t *testing.T) {
	facts := &directBranchFacts{}
	coord, store, ctx := newDirectBranchCoordinator(t, facts)
	head := dbCommit(t, "one")
	facts.obs = ports.WorkspaceObservation{Branch: "main", HeadSHA: head}
	master, task, detail := seedDirectBranchTask(t, ctx, store, "p", "task-1", head)

	if err := coord.promoteTaskToIntegration(ctx, master, task, detail); err == nil {
		t.Fatal("expected promotion to refuse a repository checked out on another branch")
	}
	if len(promotionCheckpoints(t, ctx, store, master.ID, masterIntegrationDurablePhase)) != 0 {
		t.Fatal("no promotion checkpoint may exist after a refusal")
	}
}

// assertTaskParked is the durable half of every task-level stop: the row itself
// says the task is parked, and says on what.
func assertTaskParked(t *testing.T, ctx stdctx.Context, store *sqlite.Store, runID, taskID, wantReason string) domain.WorkflowTask {
	t.Helper()
	tasks, err := store.ListWorkflowTasks(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.ID != taskID {
			continue
		}
		if task.State != domain.WorkflowTaskNeedsAttention {
			t.Fatalf("task %s state = %q, want needs_attention", taskID, task.State)
		}
		if wantReason != "" && task.AttentionReason != wantReason {
			t.Fatalf("task %s attention reason = %q, want %q", taskID, task.AttentionReason, wantReason)
		}
		if task.Attention.RecommendedAction == "" {
			t.Fatalf("task %s is parked with no recommended action", taskID)
		}
		return task
	}
	t.Fatalf("run %s has no task %s", runID, taskID)
	return domain.WorkflowTask{}
}

// B: the child left no durable proof that its verified result reached the
// branch (its local-commit policy deferred or forbade the commit). Stop safely
// — never complete the task on the child's word alone.
func TestDirectBranchPromotion_WithoutProofStopsSafely(t *testing.T) {
	facts := &directBranchFacts{}
	coord, store, ctx := newDirectBranchCoordinator(t, facts)
	facts.obs = dbObservation(dbCommit(t, "one"))
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
	facts := &directBranchFacts{}
	coord, store, ctx := newDirectBranchCoordinator(t, facts)
	head := dbCommit(t, "one")
	obs := ports.WorkspaceObservation{Path: dbTestRepo, Branch: dbTestBranch, HeadSHA: head}
	facts.obs = obs
	master, task, detail := seedDirectBranchTask(t, ctx, store, "p", "task-1", "")
	seedVerifyPassed(t, ctx, store, detail, "p", WorkspaceFingerprint(obs))

	if err := coord.promoteTaskToIntegration(ctx, master, task, detail); err != nil {
		t.Fatalf("promoteTaskToIntegration: %v", err)
	}
	state, err := coord.getMasterIntegrationState(ctx, master.ID)
	if err != nil {
		t.Fatalf("getMasterIntegrationState: %v", err)
	}
	if state.CurrentSHA != head || len(state.CompletedTaskIDs) != 1 {
		t.Fatalf("unexpected integration state: %+v", state)
	}
}

func TestDirectBranchPromotion_UncommittedWorkIsNotIntegrated(t *testing.T) {
	facts := &directBranchFacts{}
	coord, store, ctx := newDirectBranchCoordinator(t, facts)
	obs := ports.WorkspaceObservation{
		Path: dbTestRepo, Branch: dbTestBranch, HeadSHA: dbCommit(t, "one"), Dirty: true,
		Changes: []ports.WorkspaceChange{{Path: "frontend/src/Board.tsx", Status: " M"}},
	}
	facts.obs = obs
	master, task, detail := seedDirectBranchTask(t, ctx, store, "p", "task-1", "")
	seedVerifyPassed(t, ctx, store, detail, "p", WorkspaceFingerprint(obs))

	if err := coord.promoteTaskToIntegration(ctx, master, task, detail); err == nil {
		t.Fatal("expected an uncommitted verified result to be refused: it is not durably integrated")
	}
}

func TestDirectBranchPromotion_FingerprintDriftIsRefused(t *testing.T) {
	facts := &directBranchFacts{}
	coord, store, ctx := newDirectBranchCoordinator(t, facts)
	facts.obs = ports.WorkspaceObservation{Path: dbTestRepo, Branch: dbTestBranch, HeadSHA: dbCommit(t, "one")}
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
	master, task, detail := seedDirectBranchTask(t, ctx, store, "p", "task-1", dbCommit(t, "one"))

	if err := coord.promoteTaskToIntegration(ctx, master, task, detail); err == nil {
		t.Fatal("expected promotion to refuse when the target branch cannot be observed")
	}
}

// C: isolated-worktree projects are untouched — they still materialize a real
// integration commit, and never take the direct-branch path.
func TestDirectBranchPromotion_IsolatedWorktreeGoesThroughTheCoordinator(t *testing.T) {
	mat := &fakeMaterializer{}
	coord, store, ctx := newIntegrationCoordinator(t, mat)
	master, task, detail := seedTaskWithWorktree(t, ctx, store, "p", "task-1", "/repos/p/task-1")

	if err := coord.promoteTaskToIntegration(ctx, master, task, detail); err != nil {
		t.Fatalf("promoteTaskToIntegration: %v", err)
	}
	// An isolated-worktree project no longer has a materializer route: it takes
	// the same Integration Coordinator every other mode does, which is the
	// whole point of Task 5's single authoritative path.
	if mat.calls != 0 {
		t.Fatalf("MaterializeIntegrationCommit calls = %d; that route no longer exists", mat.calls)
	}
	records, err := coord.ListTaskIntegrations(ctx, master.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 {
		t.Fatal("an isolated-worktree promotion left no integration audit row")
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
	facts := &directBranchFacts{}
	coord, store, ctx := newDirectBranchCoordinator(t, facts)
	facts.obs = dbObservation(dbCommit(t, "one"))
	// No durable proof at all: a condition that is neither a task-level
	// conflict nor anything a retry changes, so it is the right shape for
	// asserting the per-condition dedup.
	master, task, detail := seedDirectBranchTask(t, ctx, store, "p", "task-1", "")

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
	// not a mute button. This one is the readiness gate rather than the missing
	// proof — the child now has durable evidence, but has not finished, and an
	// unfinished child may not reach the branch.
	seedVerifyPassed(t, ctx, store, detail, "p", WorkspaceFingerprint(facts.obs))
	detail.Run.State = domain.WorkflowRunRunning
	if err := coord.promoteTaskToIntegration(ctx, master, task, detail); err == nil {
		t.Fatal("expected the new failure to be reported")
	}
	if got := len(promotionCheckpoints(t, ctx, store, master.ID, masterIntegrationFailureDurablePhase)); got != 2 {
		t.Fatalf("integration failure checkpoints = %d, want 2 (a new, different condition)", got)
	}
}

// The task-level counterpart: a conflict is attempted ONCE, however many times
// reconciliation runs, because the task is parked rather than left running.
func TestDirectBranchConflictIsNotRetriedOnEveryPass(t *testing.T) {
	facts := &directBranchFacts{}
	coord, store, ctx := newDirectBranchCoordinator(t, facts)
	verified := dbCommit(t, "one")
	facts.obs = dbObservation(dbCommit(t, "two"))
	master, task, detail := seedDirectBranchTask(t, ctx, store, "p", "task-1", verified)

	if err := coord.promoteTaskToIntegration(ctx, master, task, detail); !errors.Is(err, errIntegrationTaskConflict) {
		t.Fatalf("first pass err = %v, want a task conflict", err)
	}
	parked := assertTaskParked(t, ctx, store, master.ID, task.ID, string(integration.ReasonPreconditionFailed))
	if parked.Attention.Attempt != 1 {
		t.Fatalf("attempt = %d, want 1", parked.Attention.Attempt)
	}

	// Every subsequent reconcile pass reads the plan, sees a parked task and
	// leaves it alone. This is the retry storm the durable state exists to end.
	for i := 0; i < 100; i++ {
		if err := coord.reconcileMasterTasksOnce(ctx, master); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if got := len(promotionCheckpoints(t, ctx, store, master.ID, taskIntegrationConflictPhase)); got != 1 {
		t.Fatalf("conflict records = %d after 100 reconciles, want exactly 1", got)
	}
	assertTaskParked(t, ctx, store, master.ID, task.ID, string(integration.ReasonPreconditionFailed))
}

// And a human resume produces exactly ONE new attempt — not a retry loop, not
// zero — and is idempotent under repetition and across a restart.
func TestResumingAParkedTaskProducesExactlyOneNewAttempt(t *testing.T) {
	facts := &directBranchFacts{}
	coord, store, ctx := newDirectBranchCoordinator(t, facts)
	verified := dbCommit(t, "one")
	facts.obs = dbObservation(dbCommit(t, "two"))
	master, task, detail := seedDirectBranchTask(t, ctx, store, "p", "task-1", verified)

	if err := coord.promoteTaskToIntegration(ctx, master, task, detail); !errors.Is(err, errIntegrationTaskConflict) {
		t.Fatalf("first pass err = %v, want a task conflict", err)
	}

	// A restart before anyone resumes: the task is still parked, because the
	// stop is state rather than a memory.
	restarted := New(Deps{Store: store, Projects: store, WorkspaceFacts: facts,
		IntegrationLocks: newLaneStub(), Clock: func() time.Time { return time.Now().UTC() }})
	assertTaskParked(t, ctx, store, master.ID, task.ID, string(integration.ReasonPreconditionFailed))

	// The person resumes. Twice, and from two different Coordinators, because
	// a retried request must not become a second attempt.
	if err := restarted.ResumeTaskAfterAttention(ctx, master.ID, task.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := coord.ResumeTaskAfterAttention(ctx, master.ID, task.ID); err != nil {
		t.Fatalf("second resume must be a no-op, got: %v", err)
	}

	// Reconciliation — many passes, as polling would make — turns the ONE
	// resume into exactly one new integration attempt.
	for i := 0; i < 10; i++ {
		if err := coord.reconcileMasterTasksOnce(ctx, master); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}

	// The resume itself is recorded once, and the conflict it produced is the
	// SECOND attempt — one new try, not a loop.
	if got := len(promotionCheckpoints(t, ctx, store, master.ID, taskIntegrationResumedPhase)); got != 1 {
		t.Fatalf("resume records = %d, want exactly 1 across two resumes", got)
	}
	parked := assertTaskParked(t, ctx, store, master.ID, task.ID, string(integration.ReasonPreconditionFailed))
	if parked.Attention.Attempt != 2 {
		t.Fatalf("attempt after one resume = %d, want 2", parked.Attention.Attempt)
	}
	if got := len(promotionCheckpoints(t, ctx, store, master.ID, taskIntegrationConflictPhase)); got != 2 {
		t.Fatalf("conflict records = %d, want exactly 2 (one per human decision)", got)
	}
}

// F (durable-recovery half): promotion reads only the child's durable ledger
// and the live repository, so a brand-new Coordinator over the same store — a
// daemon restarted between the child's verification and the master's promotion
// — converges to the same promotion, exactly once.
func TestDirectBranchPromotion_SurvivesRestartAndStaysIdempotent(t *testing.T) {
	facts := &directBranchFacts{}
	coord, store, ctx := newDirectBranchCoordinator(t, facts)
	head := dbCommit(t, "one")
	facts.obs = dbObservation(head)
	master, task, detail := seedDirectBranchTask(t, ctx, store, "p", "task-1", head)

	restarted := New(Deps{Store: store, Projects: store, WorkspaceFacts: facts, IntegrationLocks: newLaneStub(), Clock: func() time.Time { return time.Now().UTC() }})
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
