package workflow_test

// Checkpoint 8P-E.13B: the full master/child lifecycle in direct-branch
// execution mode, driven exclusively by the daemon poller.
//
// The real incident (master wf-2df5d6dd, task wft-7283a992): task 1's child ran
// plan -> work -> review approved -> verify passed, its result was committed on
// feat/engineering-control-center, and the master then could not promote it —
//
//	master_integration_promotion_failed:
//	directbranch: integration commits: workspace: operation not supported in this execution mode
//
// — forever, re-recorded on every reconcile pass. Task 2 never started, and no
// amount of waiting would have changed that.
//
// These tests drive the real autonomous stack (real sqlite store, real
// wake.Scheduler/wakepoller, real branchlock.Manager) with a WorkspaceFacts fake
// that refuses MaterializeIntegrationCommit exactly the way the real
// directbranch adapter does, so the production failure is reproducible here
// rather than assumed away.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/branchlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wakepoller"
)

const dbBranch = "feat/engineering-control-center"

// directBranchWS is the workspace surface of a direct-branch project: one
// repository, one branch, observable — and structurally incapable of
// materializing an AO-internal integration commit, which is the real adapter's
// deliberate contract (directbranch/workspace.go), not a test shortcut.
type directBranchWS struct {
	// t and repoPath make the commits REAL. Task 5 routed direct-branch
	// integration through the Integration Coordinator, which resolves refs and
	// compares ancestry with git, so a fake "commit-1" would only ever exercise
	// "git could not read this repository" — the opposite of what these tests
	// are about.
	t               *testing.T
	repoPath        string
	obs             ports.WorkspaceObservation
	commits         int
	materializeCall int
	// externalMove, when set, makes an outside actor leave a DIFFERENT commit
	// on the branch after AO's own — the "someone else moved the branch" case.
	externalMove bool
}

func (w *directBranchWS) ObserveWorkspace(context.Context, ports.WorkspaceInfo) (ports.WorkspaceObservation, error) {
	return w.obs, nil
}

func (w *directBranchWS) MaterializeIntegrationCommit(context.Context, ports.WorkspaceInfo, string, string, string, []string) (string, string, bool, error) {
	w.materializeCall++
	return "", "", false, ports.ErrWorkspaceOperationUnsupported
}

// CommitAll is the autonomous local commit: the verified working tree becomes a
// commit on the branch, and HEAD moves to it.
func (w *directBranchWS) CommitAll(_ context.Context, _ ports.WorkspaceInfo, _ string) (string, bool, error) {
	w.commits++
	sha := dbGitCommit(w.t, w.repoPath, fmt.Sprintf("ao commit %d", w.commits))
	w.obs.HeadSHA = sha
	if w.externalMove {
		// Somebody else REWRITES the tip after AO's commit, so the verified
		// commit is not even an ancestor of the branch any more. A commit on
		// top would not do: a branch that merely grew still contains the
		// verified work and is answered by a fresh review, not by stopping.
		dbGit(w.t, w.repoPath, "commit", "--amend", "--allow-empty", "-m", "someone else")
		w.obs.HeadSHA = dbGit(w.t, w.repoPath, "rev-parse", "HEAD")
	}
	return sha, true, nil
}

// dbGit runs one git command in dir and returns its trimmed stdout.
func dbGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func dbGitCommit(t *testing.T, dir, message string) string {
	t.Helper()
	dbGit(t, dir, "commit", "--allow-empty", "-m", message)
	return dbGit(t, dir, "rev-parse", "HEAD")
}

// directBranchSpawner is autoSpawner with the direct-branch session shape: every
// session works in the registered repository itself, on the project's branch.
type directBranchSpawner struct {
	store    *sqlite.Store
	repoPath string
	calls    []ports.SpawnConfig
}

func (s *directBranchSpawner) Spawn(ctx context.Context, cfg ports.SpawnConfig) (domain.SessionRecord, int, int, error) {
	s.calls = append(s.calls, cfg)
	n := len(s.calls)
	now := time.Now().UTC()
	created, err := s.store.CreateSession(ctx, domain.SessionRecord{
		ID: domain.SessionID(fmt.Sprintf("dbsess-%d", n)), ProjectID: cfg.ProjectID, Kind: cfg.Kind,
		Harness: cfg.Harness, IssueID: cfg.IssueID, Activity: domain.Activity{State: domain.ActivityIdle},
		Metadata:  domain.SessionMetadata{Branch: dbBranch, WorkspacePath: s.repoPath},
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return domain.SessionRecord{}, 0, 0, err
	}
	return created, len(cfg.Prompt), 0, nil
}

// testBranchLocks mirrors daemon/workflow_wiring.go's workflowBranchLocks: the
// composition root, not the workflow package, translates between workflow's own
// narrow lock request type and branchlock's.
type testBranchLocks struct{ mgr *branchlock.Manager }

func (w testBranchLocks) Acquire(ctx context.Context, req workflowcore.BranchLockRequest) ([]domain.BranchLock, error) {
	return w.mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: req.ProjectID, RunID: req.RunID, StepID: req.StepID, SessionID: req.SessionID})
}

func (w testBranchLocks) ReleaseRun(ctx context.Context, runID, reason string) (int64, error) {
	return w.mgr.ReleaseRun(ctx, runID, reason)
}

func (w testBranchLocks) HeldByRun(ctx context.Context, runID string) ([]domain.BranchLock, error) {
	return w.mgr.HeldByRun(ctx, runID)
}

func (w testBranchLocks) Renew(ctx context.Context, runID, stepID, sessionID string) {
	w.mgr.Renew(ctx, runID, stepID, sessionID)
}

func (w testBranchLocks) RecoverStale(ctx context.Context, runID string) (int64, error) {
	return w.mgr.RecoverStale(ctx, runID)
}

type directBranchFixture struct {
	*autonomousFixture
	ws       *directBranchWS
	locks    *branchlock.Manager
	repoPath string
	spawner  *directBranchSpawner
}

func newDirectBranchFixture(t *testing.T, plan workflowcore.MasterPlan) *directBranchFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	repo := t.TempDir()
	dbGit(t, repo, "init", "--initial-branch="+dbBranch)
	dbGit(t, repo, "config", "user.email", "ao@example.com")
	dbGit(t, repo, "config", "user.name", "Ao Agents")
	base := dbGitCommit(t, repo, "seed")
	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID: "p", Path: repo, RegisteredAt: time.Now().UTC(),
		Config: domain.ProjectConfig{DefaultBranch: dbBranch, ExecutionMode: domain.ExecutionDirectBranch},
	}); err != nil {
		t.Fatalf("seed direct-branch project: %v", err)
	}
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	ws := &directBranchWS{t: t, repoPath: repo, obs: ports.WorkspaceObservation{
		Path: repo, Branch: dbBranch, HeadSHA: base, Dirty: true,
		Changes: []ports.WorkspaceChange{{Path: "frontend/src/Board.tsx", Status: " M"}},
	}}
	spawner := &directBranchSpawner{store: store, repoPath: repo}
	planner := &staticPlanner{plan: plan}
	wakeSched := wake.New(store, clk.Now, autoIDSeq("wk"), wake.Config{})
	launcher := &fakeReviewerLauncher{}
	verifier := &fakeVerifyRunner{result: workflowcore.VerifyCommandExecution{ExitCode: 0}}
	sender := &fakeMessageSender{}
	locks := branchlock.New(branchlock.Deps{Store: store, OwnerToken: "test-daemon", Clock: clk.Now})
	coord := newDirectBranchAutonomousCoordinator(store, clk, spawner, planner, ws, launcher, verifier, sender, wakeSched, locks, "id")
	fx := &autonomousFixture{
		store: store, clk: clk, wake: wakeSched, coord: coord,
		poller:  wakepoller.New(wakeSched, coord, wakepoller.Config{Clock: clk.Now}),
		planner: planner, launcher: launcher, verifier: verifier, sender: sender,
	}
	return &directBranchFixture{autonomousFixture: fx, ws: ws, locks: locks, repoPath: repo, spawner: spawner}
}

func newDirectBranchAutonomousCoordinator(store *sqlite.Store, clk *fakeClock, spawner *directBranchSpawner, planner *staticPlanner, ws *directBranchWS, launcher *fakeReviewerLauncher, verifier *fakeVerifyRunner, sender *fakeMessageSender, wakeSched *wake.Scheduler, locks *branchlock.Manager, idPrefix string) *workflowcore.Coordinator {
	return workflowcore.New(workflowcore.Deps{
		// Task 5: every ready task now reaches its target through the
		// Integration Coordinator, which takes the lane first. A fixture
		// without one cannot integrate at all.
		IntegrationLocks: newLaneStubExternal(),
		Store:            store, Projects: store,
		Spawner: spawner, SessionFacts: store, WorkspaceFacts: ws,
		ReviewRuns: store, ReviewerLauncher: launcher,
		Verifier: verifier, MessageSender: sender,
		Planner: planner, PlannerContextBuilder: staticContext{},
		QuestionsStore:    store,
		WakeScheduler:     wakeSched,
		ProviderProfiles:  store,
		ExecutionPolicies: store,
		RuntimeIsolation:  &identityRuntimeIsolation{store: store},
		BranchLocks:       testBranchLocks{mgr: locks},
		// The workspace itself is the committer, exactly as production wires the
		// workspace router into both roles.
		WorkspaceCommitter: ws,
		Clock:              clk.Now, NewID: autoIDSeq(idPrefix),
	})
}

func countCheckpoints(t *testing.T, store *sqlite.Store, runID, phase string) int {
	t.Helper()
	cps, err := store.ListWorkflowCheckpoints(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	n := 0
	for _, cp := range cps {
		if cp.DurablePhase == phase {
			n++
		}
	}
	return n
}

// startDirectBranchObjective creates and kicks off the objective run the same
// single way every autonomous test does.
func startDirectBranchObjective(t *testing.T, fx *directBranchFixture) string {
	t.Helper()
	seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())
	created, err := fx.coord.CreateObjectiveRun(context.Background(), "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx.autonomousFixture, created.Run.ID, "user-1")
	return created.Run.ID
}

// The central regression: a direct-branch master converges through task 1 and
// on into task 2 with no human action at all.
func TestDirectBranch_MasterPromotesVerifiedChildAndDispatchesNextTask(t *testing.T) {
	fx := newDirectBranchFixture(t, twoTaskDependentPlan())
	ctx := context.Background()
	masterID := startDirectBranchObjective(t, fx)

	secondTaskStartedEarly := false
	maxLocksHeldAtOnce := 0
	driveCycles(t, fx.autonomousFixture, 40, func(int) {
		taskA, okA := taskByPlanStepID(t, fx.autonomousFixture, masterID, "model")
		taskB, okB := taskByPlanStepID(t, fx.autonomousFixture, masterID, "tests")
		if okB && taskB.ExecutionRunID != nil && okA && taskA.State != domain.WorkflowTaskCompleted {
			secondTaskStartedEarly = true
		}
		// E: one modifying workflow per repo+branch, throughout the whole
		// task 1 -> task 2 transition.
		held, err := fx.store.ListHeldBranchLocks(ctx)
		if err != nil {
			t.Fatalf("ListHeldBranchLocks: %v", err)
		}
		if len(held) > maxLocksHeldAtOnce {
			maxLocksHeldAtOnce = len(held)
		}
		if _, childID, ok := activeChildRunID(t, fx.autonomousFixture, masterID); ok {
			approveOpenReview(t, fx.autonomousFixture, childID, domain.VerdictApproved)
		}
	})

	if n := countCheckpoints(t, fx.store, masterID, "master_integration_promotion_failed"); n != 0 {
		t.Fatalf("master recorded %d integration failures; a verified direct-branch task must be promotable", n)
	}
	if fx.ws.materializeCall != 0 {
		t.Fatalf("MaterializeIntegrationCommit was called %d times in direct-branch mode", fx.ws.materializeCall)
	}
	if secondTaskStartedEarly {
		t.Fatal("task 2 was dispatched before task 1 completed")
	}
	if maxLocksHeldAtOnce > 1 {
		t.Fatalf("branch locks held simultaneously = %d, want at most 1 (one modifying workflow per repo+branch)", maxLocksHeldAtOnce)
	}

	taskA, _ := taskByPlanStepID(t, fx.autonomousFixture, masterID, "model")
	taskB, _ := taskByPlanStepID(t, fx.autonomousFixture, masterID, "tests")
	if taskA.State != domain.WorkflowTaskCompleted {
		t.Fatalf("task 1 state = %q, want completed", taskA.State)
	}
	if taskB.ExecutionRunID == nil {
		t.Fatal("task 2 was never dispatched — the master stalled after task 1")
	}
	if taskB.State != domain.WorkflowTaskCompleted {
		t.Fatalf("task 2 state = %q, want completed", taskB.State)
	}
	run, _, err := fx.store.GetWorkflowRun(ctx, masterID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if run.State != domain.WorkflowRunCompleted {
		t.Fatalf("master state = %q, want completed", run.State)
	}
	if n := countCheckpoints(t, fx.store, masterID, "master_integration_promotion"); n != 2 {
		t.Fatalf("promotion checkpoints = %d, want one per task", n)
	}
	// The lock is released once the objective is over: nothing keeps the user's
	// branch after AO is done with it.
	held, err := fx.store.ListHeldBranchLocks(ctx)
	if err != nil {
		t.Fatalf("ListHeldBranchLocks: %v", err)
	}
	if len(held) != 0 {
		t.Fatalf("branch locks still held after completion: %+v", held)
	}
}

// D: a deterministic integration failure is durably represented once, not
// re-recorded on every coordinator poll. GetRun is the read path the Board polls
// roughly every 2 seconds, and it is what drives reconcileMasterTasks.
func TestDirectBranch_RepeatedPollsAfterIntegrationFailureDoNotSpamCheckpoints(t *testing.T) {
	fx := newDirectBranchFixture(t, twoTaskDependentPlan())
	ctx := context.Background()
	// An outside actor owns the branch tip after AO's commit, so the verified
	// result can never be proven integrated: a permanent, human-owned stop.
	fx.ws.externalMove = true
	masterID := startDirectBranchObjective(t, fx)

	driveCycles(t, fx.autonomousFixture, 12, func(int) {
		if _, childID, ok := activeChildRunID(t, fx.autonomousFixture, masterID); ok {
			approveOpenReview(t, fx.autonomousFixture, childID, domain.VerdictApproved)
		}
	})

	// The refusal belongs to the TASK, so it is recorded there — and the
	// promotion-failure phase, which parks the whole objective, is not used for
	// it at all.
	conflicts := countCheckpoints(t, fx.store, masterID, "task_integration_conflict")
	if conflicts == 0 {
		t.Fatal("expected the moved branch to be refused; the fixture never reached the state under test")
	}
	if n := countCheckpoints(t, fx.store, masterID, "master_integration_promotion_failed"); n != 0 {
		t.Fatalf("a task-level conflict was recorded as %d objective-level integration failures", n)
	}

	// Twenty board polls over the unchanged condition. Each one reconciles the
	// plan, and each one must leave the parked task exactly as it found it:
	// this is the retry storm the durable state exists to end.
	for i := 0; i < 20; i++ {
		if _, err := fx.coord.GetRun(ctx, masterID); err != nil {
			t.Fatalf("GetRun poll %d: %v", i, err)
		}
	}
	after := countCheckpoints(t, fx.store, masterID, "task_integration_conflict")
	if after != conflicts {
		t.Fatalf("conflict checkpoints grew from %d to %d across 20 polls of one unchanged condition", conflicts, after)
	}
	if conflicts > 1 {
		t.Fatalf("conflict checkpoints = %d for a single deterministic conflict", conflicts)
	}

	// The task is emphatically NOT completed — a refused promotion never lets a
	// task count as done — and it is parked, durably, with the detail a person
	// needs rather than only a line in a ledger.
	taskA, _ := taskByPlanStepID(t, fx.autonomousFixture, masterID, "model")
	if taskA.State != domain.WorkflowTaskNeedsAttention {
		t.Fatalf("task state = %q, want needs_attention", taskA.State)
	}
	if taskA.AttentionReason == "" || taskA.Attention.RecommendedAction == "" {
		t.Fatalf("parked task carries no actionable detail: %+v", taskA.Attention)
	}
}

// F: a daemon restart between the child's verification and the master's
// promotion still converges. Promotion reads only durable state (the child's
// checkpoint ledger) plus the live repository, so a fresh Coordinator picks up
// exactly where the old one stopped.
func TestDirectBranch_RestartBetweenVerificationAndPromotionStillConverges(t *testing.T) {
	fx := newDirectBranchFixture(t, twoTaskDependentPlan())
	ctx := context.Background()
	masterID := startDirectBranchObjective(t, fx)

	// Drive only until task 1's child has actually reached completed (verified
	// and committed) — the exact window the incident sat in.
	// Get task 1 dispatched, then drive ONLY its child run to completion. That
	// isolates the exact window the incident sat in: the child is verified and
	// committed, and the master has not yet observed it.
	driveCycles(t, fx.autonomousFixture, 2, nil)
	_, childID, ok := activeChildRunID(t, fx.autonomousFixture, masterID)
	if !ok {
		t.Fatal("task 1's child was never dispatched; the fixture did not reach the state under test")
	}
	childCompleted := false
	for i := 0; i < 40 && !childCompleted; i++ {
		approveOpenReview(t, fx.autonomousFixture, childID, domain.VerdictApproved)
		fx.clk.Advance(90 * time.Second)
		if _, err := fx.coord.ContinueRun(ctx, childID); err != nil {
			t.Fatalf("ContinueRun(child) cycle %d: %v", i, err)
		}
		child, found, err := fx.store.GetWorkflowRun(ctx, childID)
		if err != nil || !found {
			t.Fatalf("GetWorkflowRun(child): %v (found=%v)", err, found)
		}
		childCompleted = child.State == domain.WorkflowRunCompleted
	}
	if !childCompleted {
		t.Fatal("task 1's child never reached completed; the fixture did not reach the state under test")
	}
	taskA, _ := taskByPlanStepID(t, fx.autonomousFixture, masterID, "model")
	if taskA.State == domain.WorkflowTaskCompleted {
		t.Fatal("task 1 was already promoted before the restart; the window under test was missed")
	}

	// Restart: a brand-new Coordinator and poller over the same store.
	restarted := newDirectBranchAutonomousCoordinator(fx.store, fx.clk, fx.spawner, fx.planner, fx.ws, fx.launcher, fx.verifier, fx.sender, fx.wake, fx.locks, "rid")
	if err := restarted.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile after restart: %v", err)
	}
	fx.coord = restarted
	fx.poller = wakepoller.New(fx.wake, restarted, wakepoller.Config{Clock: fx.clk.Now})

	driveCycles(t, fx.autonomousFixture, 40, func(int) {
		if _, childID, ok := activeChildRunID(t, fx.autonomousFixture, masterID); ok {
			approveOpenReview(t, fx.autonomousFixture, childID, domain.VerdictApproved)
		}
	})

	taskA, _ = taskByPlanStepID(t, fx.autonomousFixture, masterID, "model")
	taskB, _ := taskByPlanStepID(t, fx.autonomousFixture, masterID, "tests")
	if taskA.State != domain.WorkflowTaskCompleted {
		t.Fatalf("task 1 state after restart = %q, want completed", taskA.State)
	}
	if taskB.ExecutionRunID == nil {
		t.Fatal("task 2 was never dispatched after the restart")
	}
	if n := countCheckpoints(t, fx.store, masterID, "master_integration_promotion"); n < 1 {
		t.Fatal("no promotion was recorded after the restart")
	}
}
