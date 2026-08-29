package workflow_test

// Checkpoint 8P-D shared fixture: "true autonomous workflow execution" --
// a master/objective run under AutonomousMode must be driven end to end
// (plan generation, auto-approval, task dispatch, review, fix, verify,
// integration promotion, next task, completion) purely by the daemon's
// wakepoller, never by a browser GET. This file wires the heaviest fixture
// every autonomous_*_test.go file below shares, following the exact
// real-store-plus-fakes convention wake_integration_test.go established:
// real *sqlite.Store-backed Spawner/SessionFacts/ReviewRuns/ProviderProfiles/
// ExecutionPolicies/QuestionsStore (so FK constraints hold and the real
// wake.Scheduler/production read paths are exercised verbatim), fake
// WorkspaceFacts/ReviewerLauncher/Verifier/MessageSender (no real git/agent
// process -- git-plumbing-specific behavior is master_integration_e2e_internal_test.go's
// job, not this checkpoint's).

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/notify"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wakepoller"
)

// autoSpawner is wakeTestSpawner (wake_integration_test.go) extended to
// record each call's full SpawnConfig (not just the harness), needed by the
// fallback/isolation tests below to prove which provider/profile actually
// got selected for which owner, AND to create a REAL directory on disk for
// each session's workspace: Verify's own guard (verify.go) does a real
// os.Lstat on the checkpoint's WorktreePath before ever calling the
// injected VerifyRunner, so a fake nonexistent path fails verification
// every time even with a fake Verifier wired.
type autoSpawner struct {
	store   *sqlite.Store
	baseDir string
	// repoPath is the real git repository each spawned task gets a real
	// worktree of. Task 5 routed every integration through the Integration
	// Coordinator, which reads refs, computes ancestry and moves branches with
	// git — so a task's worktree has to be one, and a fixture that handed out
	// bare directories was describing a promotion path that no longer exists.
	repoPath string
	calls    []ports.SpawnConfig
	// failWith, when set, is returned instead of starting a session — the
	// worker-side counterpart of fakeReviewerLauncher.launchErr, so a test can
	// park a child on a real worker-launch failure produced by the real
	// dispatch path rather than by seeding rows.
	failWith error
}

func (s *autoSpawner) Spawn(ctx context.Context, cfg ports.SpawnConfig) (domain.SessionRecord, int, int, error) {
	s.calls = append(s.calls, cfg)
	if s.failWith != nil {
		return domain.SessionRecord{}, 0, 0, s.failWith
	}
	n := len(s.calls)
	wsPath := filepath.Join(s.baseDir, fmt.Sprintf("task-%d", n))
	branch := fmt.Sprintf("ao/task-%d", n)
	if s.repoPath != "" {
		// A real worktree on its own branch, with one commit of its own — the
		// shape the Coordinator has to be able to fast-forward or replay.
		if out, err := autoGit(s.repoPath, "worktree", "add", "-b", branch, wsPath); err != nil {
			return domain.SessionRecord{}, 0, 0, fmt.Errorf("worktree add: %w: %s", err, out)
		}
		file := filepath.Join(wsPath, fmt.Sprintf("task-%d.txt", n))
		if err := os.WriteFile(file, []byte(fmt.Sprintf("work of task %d\n", n)), 0o600); err != nil {
			return domain.SessionRecord{}, 0, 0, err
		}
		if out, err := autoGit(wsPath, "add", "."); err != nil {
			return domain.SessionRecord{}, 0, 0, fmt.Errorf("git add: %w: %s", err, out)
		}
		if out, err := autoGit(wsPath, "commit", "-m", fmt.Sprintf("task %d", n)); err != nil {
			return domain.SessionRecord{}, 0, 0, fmt.Errorf("git commit: %w: %s", err, out)
		}
	} else if err := os.MkdirAll(wsPath, 0o755); err != nil {
		return domain.SessionRecord{}, 0, 0, err
	}
	rec := domain.SessionRecord{
		ID:        domain.SessionID(fmt.Sprintf("asess-%d", n)),
		ProjectID: cfg.ProjectID,
		Kind:      cfg.Kind,
		Harness:   cfg.Harness,
		IssueID:   cfg.IssueID,
		Activity:  domain.Activity{State: domain.ActivityIdle},
		Metadata:  domain.SessionMetadata{Branch: branch, WorkspacePath: wsPath},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	created, err := s.store.CreateSession(ctx, rec)
	if err != nil {
		return domain.SessionRecord{}, 0, 0, err
	}
	return created, len(cfg.Prompt), 0, nil
}

// identityRuntimeIsolation is a minimal, store-backed
// workflowcore.RuntimeIsolation: no env override, but a real per-run owner
// lookup (via the same GetWorkflowRunOwner call runOwner itself uses) --
// needed because attemptWorkHarness's SpawnConfig.Owner comes ONLY from
// RuntimeIsolation.Resolve (resolveRuntimeEnv returns owner="" unconditionally
// when RuntimeIsolation is nil, regardless of the DB owner stamp), and the
// isolation tests below need SpawnConfig.Owner to reflect each run's REAL
// owner, not a single fixed value like runtime_isolation_test.go's
// single-user fakeRuntimeIsolation.
type identityRuntimeIsolation struct{ store *sqlite.Store }

func (r *identityRuntimeIsolation) Resolve(ctx context.Context, runID string, _ domain.AgentHarness) (map[string]string, domain.UserID, domain.ProviderProfileID, error) {
	owner, err := r.store.GetWorkflowRunOwner(ctx, runID)
	if err != nil {
		// A fixture cannot invent an owner, and a read failure is not one.
		// Reporting no owner is what production does with an unowned run.
		return nil, "", "", nil //nolint:nilerr // an unreadable owner reads as no owner, exactly as an unowned run does
	}
	if owner == nil {
		return nil, "", "", nil
	}
	return nil, *owner, "", nil
}

func autoIDSeq(prefix string) func() string {
	n := 0
	return func() string {
		n++
		return fmt.Sprintf("%s%d", prefix, n)
	}
}

// autonomousFixture bundles a full autonomous-master-run stack.
type autonomousFixture struct {
	store    *sqlite.Store
	clk      *fakeClock
	wake     *wake.Scheduler
	poller   *wakepoller.Poller
	coord    *workflowcore.Coordinator
	spawner  *autoSpawner
	planner  *staticPlanner
	ws       *fakeWorkspaceFacts
	launcher *fakeReviewerLauncher
	verifier *fakeVerifyRunner
	sender   *fakeMessageSender
	// emails records every notification the REAL notify.Manager decided to
	// send. The manager is wired for real (over the same sqlite store) rather
	// than faked, because the property these tests care about — one
	// notification per real event, however many times a poll re-derives it —
	// lives in the store's durable dedupe index, not in the sink.
	emails *autoEmailer
	// newID is this fixture's id sequence. It is held here, not created per
	// coordinator, so rebuilding the coordinator (withCapacityLimits) keeps
	// minting fresh ids instead of restarting the sequence and colliding with
	// rows the store already holds.
	newID func() string
}

// autoEmailer is attention_notify_internal_test.go's recordingEmailer, for the
// external test package.
type autoEmailer struct {
	mu   sync.Mutex
	sent []domain.NotificationRecord
}

func (e *autoEmailer) EmailNotification(_ context.Context, rec domain.NotificationRecord) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sent = append(e.sent, rec)
	return nil
}

func (e *autoEmailer) countOfType(typ domain.NotificationType) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, rec := range e.sent {
		if rec.Type == typ {
			n++
		}
	}
	return n
}

// seedUser seeds a real User + two owned ProviderProfiles (claude-code,
// codex) plus a stored UserExecutionPolicy, mirroring
// capacity_scope_internal_test.go's seedUserAndProfile, extended with a
// second profile and the policy row so ProviderProfiles/ExecutionPolicies
// can be wired directly to the real store rather than a hand-rolled fake.
func seedUser(t *testing.T, store *sqlite.Store, userID domain.UserID, claudeProfile, codexProfile domain.ProviderProfileID, autonomous bool, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.InsertUser(ctx, domain.User{
		ID: userID, DisplayName: string(userID), Email: string(userID) + "@example.com", Username: string(userID),
		PasswordHash: "x", Status: domain.UserStatusActive, Role: domain.UserRoleMember, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed user %s: %v", userID, err)
	}
	// Capabilities must mirror registry.ProviderDescriptors() exactly --
	// domain.EligibleProfiles filters out any profile missing the role's
	// required capability regardless of health/cooldown state, so an empty
	// Capabilities list here would make every profile permanently
	// ineligible and every dispatch wait forever, masquerading as a
	// capacity wait.
	if _, err := store.InsertProviderProfile(ctx, domain.ProviderProfile{
		ID: claudeProfile, UserID: userID, Provider: "anthropic", Harness: domain.HarnessClaudeCode, DisplayName: "Claude",
		Enabled: true, AuthState: domain.ProviderAuthStateAuthenticated, AuthMethod: domain.AuthMethodCLIBootstrap,
		Capabilities: []domain.ProviderCapability{
			domain.CapabilityPlanner, domain.CapabilityWorker, domain.CapabilityReviewer,
			domain.CapabilityDecisionResolver, domain.CapabilityUsageTelemetry, domain.CapabilityCapacityTelemetry,
		},
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed claude profile: %v", err)
	}
	if _, err := store.InsertProviderProfile(ctx, domain.ProviderProfile{
		ID: codexProfile, UserID: userID, Provider: "openai", Harness: domain.HarnessCodex, DisplayName: "Codex",
		Enabled: true, AuthState: domain.ProviderAuthStateAuthenticated, AuthMethod: domain.AuthMethodCLIBootstrap,
		Capabilities: []domain.ProviderCapability{
			domain.CapabilityWorker, domain.CapabilityReviewer,
			domain.CapabilityDecisionResolver, domain.CapabilityUsageTelemetry, domain.CapabilityCapacityTelemetry,
		},
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed codex profile: %v", err)
	}
	if _, err := store.UpsertUserExecutionPolicy(ctx, domain.UserExecutionPolicy{
		ID:     domain.UserExecutionPolicyID("policy-" + string(userID)),
		UserID: userID, Version: domain.UserExecutionPolicyVersion, AutonomousMode: autonomous,
		PlannerPriority:          []domain.ProviderProfileID{claudeProfile},
		WorkerPriority:           []domain.ProviderProfileID{claudeProfile, codexProfile},
		ReviewerPriority:         []domain.ProviderProfileID{codexProfile, claudeProfile},
		DecisionResolverPriority: []domain.ProviderProfileID{codexProfile, claudeProfile},
		FallbackBehavior:         domain.FallbackUseNextAvailable,
		ReviewIndependence:       domain.ReviewIndependenceRequireDifferentProvider,
		CreatedAt:                now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed execution policy: %v", err)
	}
}

// newAutonomousFixture wires a Coordinator + real wake.Scheduler +
// wakepoller.Poller over a fresh real sqlite store, with project "p"
// already seeded and plan as the (single) Planner's static response.
func newAutonomousFixture(t *testing.T, plan workflowcore.MasterPlan) *autonomousFixture {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	repoPath := newAutoTestRepo(t)
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: repoPath, RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	spawner := &autoSpawner{store: store, baseDir: t.TempDir(), repoPath: repoPath}
	planner := &staticPlanner{plan: plan}
	wakeSched := wake.New(store, clk.Now, autoIDSeq("wk"), wake.Config{})
	ws := &fakeWorkspaceFacts{obs: ports.WorkspaceObservation{Dirty: true}}
	launcher := &fakeReviewerLauncher{}
	verifier := &fakeVerifyRunner{result: workflowcore.VerifyCommandExecution{ExitCode: 0}}
	sender := &fakeMessageSender{}
	emails := &autoEmailer{}
	newID := autoIDSeq("id")
	coord := newAutonomousCoordinator(store, clk, spawner, planner, ws, launcher, verifier, sender, wakeSched, emails, newID)
	poller := wakepoller.New(wakeSched, coord, wakepoller.Config{Clock: clk.Now})
	return &autonomousFixture{
		store: store, clk: clk, wake: wakeSched, poller: poller, coord: coord,
		spawner: spawner, planner: planner, ws: ws, launcher: launcher, verifier: verifier, sender: sender,
		emails: emails, newID: newID,
	}
}

// withCapacityLimits rebuilds this fixture's coordinator under tighter
// scheduler bounds, over the SAME durable store. It is how a capacity test
// makes the machine "full" without spawning six real runtimes: the limits are
// configuration, and the durable claims are the same ones production writes.
func (fx *autonomousFixture) withCapacityLimits(limits domain.CapacityLimits) *workflowcore.Coordinator {
	coord := newAutonomousCoordinator(fx.store, fx.clk, fx.spawner, fx.planner, fx.ws,
		fx.launcher, fx.verifier, fx.sender, fx.wake, fx.emails, fx.newID, limits)
	fx.coord = coord
	// The poller drives the coordinator it was built with, so it has to be
	// rebuilt too -- otherwise the daemon's own driving loop keeps running
	// under the OLD limits and the new ones apply to nothing the test does.
	fx.poller = wakepoller.New(fx.wake, coord, wakepoller.Config{Clock: fx.clk.Now})
	return coord
}

func newAutonomousCoordinator(store *sqlite.Store, clk *fakeClock, spawner *autoSpawner, planner *staticPlanner, ws *fakeWorkspaceFacts, launcher *fakeReviewerLauncher, verifier *fakeVerifyRunner, sender *fakeMessageSender, wakeSched *wake.Scheduler, emails *autoEmailer, newID func() string, limits ...domain.CapacityLimits) *workflowcore.Coordinator {
	capacityLimits := domain.CapacityLimits{}
	if len(limits) > 0 {
		capacityLimits = limits[0]
	}
	if newID == nil {
		newID = autoIDSeq("id")
	}
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
		// P1-C: the real capacity scheduler, under the real default limits.
		// Wiring it into the shared autonomous fixture is deliberate: it means
		// every autonomous test in this package now runs through admission
		// control, so a scheduler that wrongly refused (or wrongly granted)
		// would break the whole suite rather than only its own tests.
		Capacity: store, CapacityLimits: capacityLimits,
		Notifications: notify.New(notify.Deps{
			Store:   store,
			Emailer: emails,
			Logger:  slog.Default(),
			Clock:   clk.Now,
		}),
		Clock: clk.Now, NewID: newID,
	})
}

// stampOwnerAndApplyPolicy mirrors internal/httpd/controllers/workflow.go's
// stampOwner exactly: SetWorkflowRunOwner THEN ApplyExecutionPolicySnapshot,
// using the same resolved identity for both -- this is the ONE manual
// "kickoff" step every autonomous test makes; a run's owner must be stamped
// for routingInputsForRole/resolvedProfiles to ever resolve this user's
// owned ProviderProfiles (an unowned run with ProviderProfiles wired and
// TrustedLocal=false resolves zero profiles by design -- checkpoint brief
// §18 "no profile = never selected" -- which would otherwise look
// indistinguishable from a genuine capacity wait).
func stampOwnerAndApplyPolicy(t *testing.T, fx *autonomousFixture, runID string, userID domain.UserID) {
	t.Helper()
	ctx := context.Background()
	if _, err := fx.store.SetWorkflowRunOwner(ctx, runID, userID); err != nil {
		t.Fatalf("SetWorkflowRunOwner: %v", err)
	}
	if err := fx.coord.ApplyExecutionPolicySnapshot(ctx, runID, userID, nil); err != nil {
		t.Fatalf("ApplyExecutionPolicySnapshot: %v", err)
	}
}

// driveCycles advances the fake clock and calls RunDueOnce cycles times,
// invoking before(i) (if non-nil) right before each RunDueOnce call -- the
// hook tests use to simulate out-of-band facts landing (e.g. `ao review
// submit`, an agent health recovery event) between poll cycles, exactly the
// way wake_integration_test.go's tests inject them between StartRun and the
// next RunDueOnce.
func driveCycles(t *testing.T, fx *autonomousFixture, cycles int, before func(i int)) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < cycles; i++ {
		if before != nil {
			before(i)
		}
		fx.clk.Advance(90 * time.Second)
		if _, err := fx.poller.RunDueOnce(ctx); err != nil {
			t.Fatalf("RunDueOnce cycle %d: %v", i, err)
		}
	}
}

// taskByPlanStepID finds a master run's task by its PlanStepID (the
// PlannedStep.ID from the plan, e.g. "model"/"tests" -- NOT
// domain.WorkflowTask.ID, which is a coordinator-generated id).
func taskByPlanStepID(t *testing.T, fx *autonomousFixture, masterID, planStepID string) (domain.WorkflowTask, bool) {
	t.Helper()
	tasks, err := fx.store.ListWorkflowTasks(context.Background(), masterID)
	if err != nil {
		t.Fatalf("ListWorkflowTasks: %v", err)
	}
	for _, tk := range tasks {
		if tk.PlanStepID == planStepID {
			return tk, true
		}
	}
	return domain.WorkflowTask{}, false
}

// activeChildRunID returns the ExecutionRunID of whichever task is
// currently domain.WorkflowTaskRunning under masterID, if any.
func activeChildRunID(t *testing.T, fx *autonomousFixture, masterID string) (taskID, childID string, ok bool) {
	t.Helper()
	tasks, err := fx.store.ListWorkflowTasks(context.Background(), masterID)
	if err != nil {
		t.Fatalf("ListWorkflowTasks: %v", err)
	}
	for _, tk := range tasks {
		if tk.State == domain.WorkflowTaskRunning && tk.ExecutionRunID != nil {
			return tk.ID, *tk.ExecutionRunID, true
		}
	}
	return "", "", false
}

// approveOpenReview finds childRunID's review step, and if it has a review
// run currently domain.ReviewRunRunning, resolves it with verdict --
// simulating the real `ao review submit` CLI call landing out of band,
// exactly like fakeReviewRuns.setStatus does for the in-memory fixtures.
// Returns true if it found and resolved an open review.
func approveOpenReview(t *testing.T, fx *autonomousFixture, childRunID string, verdict domain.ReviewVerdict) bool {
	t.Helper()
	ctx := context.Background()
	steps, err := fx.store.ListWorkflowSteps(ctx, childRunID)
	if err != nil {
		t.Fatalf("ListWorkflowSteps: %v", err)
	}
	for _, s := range steps {
		if s.Kind != domain.WorkflowStepReview || s.ReviewRunID == nil {
			continue
		}
		rr, ok, err := fx.store.GetReviewRun(ctx, *s.ReviewRunID)
		if err != nil {
			t.Fatalf("GetReviewRun: %v", err)
		}
		if !ok || rr.Status != domain.ReviewRunRunning {
			continue
		}
		if _, err := fx.store.UpdateReviewRunResult(ctx, rr.ID, domain.ReviewRunComplete, verdict, "", "", false); err != nil {
			t.Fatalf("UpdateReviewRunResult: %v", err)
		}
		return true
	}
	return false
}

// twoTaskDependentPlan is validMasterPlan (master_plan_test.go): task
// "tests" depends on task "model" -- reused here as the fixed 2-task
// dependency chain the progression tests drive end to end.
func twoTaskDependentPlan() workflowcore.MasterPlan {
	return validMasterPlan()
}

// oneTaskPlan is a minimal single-step master plan for tests that only need
// one child task's full lifecycle.
func oneTaskPlan() workflowcore.MasterPlan {
	return workflowcore.MasterPlan{Version: "v1", Objective: "Build users", Summary: "one step", Steps: []workflowcore.PlannedStep{
		{ID: "only", Title: "Only", Description: "Do the one thing.", Dependencies: []string{}, AcceptanceCriteria: []string{"It works."}, Verify: workflowcore.VerificationPlan{Commands: []workflowcore.VerificationCommandCheck{{Command: "go", Args: []string{"test", "./..."}, TimeoutSeconds: 60, RequiredExitCode: 0, RetrySafe: true}}, Files: []workflowcore.VerificationFileCheck{}}},
	}}
}

// invalidCyclePlan is validMasterPlan mutated to contain a dependency
// cycle, matching TestMasterPlanValidationRejectsUnknownDependencyCycleAndUnsafeCommand's
// "cycle" case.
func invalidCyclePlan() workflowcore.MasterPlan {
	p := validMasterPlan()
	p.Steps[0].Dependencies = []string{"tests"}
	return p
}

// autoGit runs one git command for the autonomous fixtures.
func autoGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANGUAGE=", "GIT_EDITOR=true",
		"GIT_AUTHOR_NAME=Ao", "GIT_AUTHOR_EMAIL=ao@example.com",
		"GIT_COMMITTER_NAME=Ao", "GIT_COMMITTER_EMAIL=ao@example.com")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// newAutoTestRepo creates the real repository the autonomous fixtures now need.
//
// It is real git rather than a stub because the behaviour under test genuinely
// depends on git: which commit a ref points at, whether one commit contains
// another, whether a fast-forward applies, and whether a compare-and-set still
// sees the value it read. A fake that answered those questions would be
// asserting the fixture's opinion rather than the system's.
func newAutoTestRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "--initial-branch=main"},
		{"config", "user.email", "ao@example.com"},
		{"config", "user.name", "Ao Agents"},
	} {
		if out, err := autoGit(repo, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := autoGit(repo, "add", "."); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	if out, err := autoGit(repo, "commit", "-m", "seed"); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
	return repo
}
