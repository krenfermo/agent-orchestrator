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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
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
	calls   []ports.SpawnConfig
}

func (s *autoSpawner) Spawn(ctx context.Context, cfg ports.SpawnConfig) (domain.SessionRecord, int, int, error) {
	s.calls = append(s.calls, cfg)
	n := len(s.calls)
	wsPath := filepath.Join(s.baseDir, fmt.Sprintf("task-%d", n))
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		return domain.SessionRecord{}, 0, 0, err
	}
	rec := domain.SessionRecord{
		ID:        domain.SessionID(fmt.Sprintf("asess-%d", n)),
		ProjectID: cfg.ProjectID,
		Kind:      cfg.Kind,
		Harness:   cfg.Harness,
		IssueID:   cfg.IssueID,
		Activity:  domain.Activity{State: domain.ActivityIdle},
		Metadata:  domain.SessionMetadata{Branch: fmt.Sprintf("ao/task-%d", n), WorkspacePath: wsPath},
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
	if err != nil || owner == nil {
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
		PasswordHash: "x", Status: domain.UserStatusActive, CreatedAt: now, UpdatedAt: now,
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
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	spawner := &autoSpawner{store: store, baseDir: t.TempDir()}
	planner := &staticPlanner{plan: plan}
	wakeSched := wake.New(store, clk.Now, autoIDSeq("wk"), wake.Config{})
	ws := &fakeWorkspaceFacts{obs: ports.WorkspaceObservation{Dirty: true}}
	launcher := &fakeReviewerLauncher{}
	verifier := &fakeVerifyRunner{result: workflowcore.VerifyCommandExecution{ExitCode: 0}}
	sender := &fakeMessageSender{}
	coord := newAutonomousCoordinator(store, clk, spawner, planner, ws, launcher, verifier, sender, wakeSched)
	poller := wakepoller.New(wakeSched, coord, wakepoller.Config{Clock: clk.Now})
	return &autonomousFixture{
		store: store, clk: clk, wake: wakeSched, poller: poller, coord: coord,
		spawner: spawner, planner: planner, ws: ws, launcher: launcher, verifier: verifier, sender: sender,
	}
}

func newAutonomousCoordinator(store *sqlite.Store, clk *fakeClock, spawner *autoSpawner, planner *staticPlanner, ws *fakeWorkspaceFacts, launcher *fakeReviewerLauncher, verifier *fakeVerifyRunner, sender *fakeMessageSender, wakeSched *wake.Scheduler) *workflowcore.Coordinator {
	return workflowcore.New(workflowcore.Deps{
		Store: store, Projects: store,
		Spawner: spawner, SessionFacts: store, WorkspaceFacts: ws,
		ReviewRuns: store, ReviewerLauncher: launcher,
		Verifier: verifier, MessageSender: sender,
		Planner: planner, PlannerContextBuilder: staticContext{},
		QuestionsStore:    store,
		WakeScheduler:     wakeSched,
		ProviderProfiles:  store,
		ExecutionPolicies: store,
		RuntimeIsolation:  &identityRuntimeIsolation{store: store},
		Clock:             clk.Now, NewID: autoIDSeq("id"),
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
