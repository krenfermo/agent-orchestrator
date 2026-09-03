package workflow_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// f1_child_policy_inheritance_test.go — the regression suite for F1.
//
// An Autonomous parent's real work all happens on its CHILD runs: worker,
// review, fix and verify steps live there. Before the fix, a parent created
// with autonomy=auto_decide_low_risk / repair=automatic produced children
// durably frozen at the creation defaults ask_always / suggest, so every
// worker question escalated to a person and no repair ever ran unattended
// while every durable row looked healthy. These tests fail without the fix.

// f1Parent builds an owned, autonomous master run whose frozen policy carries
// the given autonomy and repair modes, and returns the coordinator, the store
// and the parent run id.
func f1Parent(t *testing.T, autonomy domain.QuestionAutonomyMode, repair domain.RepairMode) (*workflowcore.Coordinator, *sqlite.Store, string) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	owner, profile := domain.UserID("f1-owner"), domain.ProviderProfileID("f1-profile")
	seedOwnerAndClaudeProfile(t, store, owner, profile, now)

	c := workflowcore.New(workflowcore.Deps{
		Store: store, Projects: store, Planner: &staticPlanner{plan: validMasterPlan()},
		PlannerContextBuilder: staticContext{}, ProviderProfiles: store, ExecutionPolicies: store,
	})
	created, err := c.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalAuto)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}
	if ok, err := store.SetWorkflowRunOwner(ctx, created.Run.ID, owner); err != nil || !ok {
		t.Fatalf("stamp owner: ok=%v err=%v", ok, err)
	}
	// Mirrors the create handler: the per-run autonomy and repair choices are
	// stamped onto the parent immediately after creation.
	autonomousMode := true
	if err := c.ApplyExecutionPolicySnapshot(ctx, created.Run.ID, owner, &autonomousMode); err != nil {
		t.Fatalf("ApplyExecutionPolicySnapshot: %v", err)
	}
	if err := c.ApplyRepairPolicy(ctx, created.Run.ID, repair); err != nil {
		t.Fatalf("ApplyRepairPolicy: %v", err)
	}
	if err := c.ApplyAutonomyPolicy(ctx, created.Run.ID, autonomy); err != nil {
		t.Fatalf("ApplyAutonomyPolicy: %v", err)
	}
	return c, store, created.Run.ID
}

func f1PolicyOf(t *testing.T, store *sqlite.Store, runID string) domain.WorkflowPolicy {
	t.Helper()
	run, ok, err := store.GetWorkflowRun(context.Background(), runID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(%s): ok=%v err=%v", runID, ok, err)
	}
	var p domain.WorkflowPolicy
	if err := json.Unmarshal([]byte(run.PolicySnapshot), &p); err != nil {
		t.Fatalf("decode policy snapshot for %s: %v", runID, err)
	}
	return p
}

// f1FirstChild generates the parent's plan (auto-approved, which dispatches
// the first task) and returns the child run id.
func f1FirstChild(t *testing.T, c *workflowcore.Coordinator, parentID string) string {
	t.Helper()
	detail, err := c.GeneratePlan(context.Background(), parentID)
	if err != nil {
		t.Fatalf("GeneratePlan: %v", err)
	}
	if len(detail.Tasks) == 0 || detail.Tasks[0].ExecutionRunID == nil {
		t.Fatalf("expected a dispatched first task, got %+v", detail.Tasks)
	}
	return *detail.Tasks[0].ExecutionRunID
}

// TestF1_ChildInheritsParentAutonomyAndRepairModes is the core regression:
// every combination of the two frozen parent choices must reach the child.
func TestF1_ChildInheritsParentAutonomyAndRepairModes(t *testing.T) {
	for _, tt := range []struct {
		name     string
		autonomy domain.QuestionAutonomyMode
		repair   domain.RepairMode
	}{
		{"auto_decide_low_risk + automatic", domain.QuestionAutonomyAutoDecideLowRisk, domain.RepairModeAutomatic},
		{"full_autonomy + automatic", domain.QuestionAutonomyFullAutonomy, domain.RepairModeAutomatic},
		{"ask_always + suggest", domain.QuestionAutonomyAskAlways, domain.RepairModeSuggest},
		{"auto_decide_low_risk + disabled", domain.QuestionAutonomyAutoDecideLowRisk, domain.RepairModeDisabled},
		{"full_autonomy + suggest", domain.QuestionAutonomyFullAutonomy, domain.RepairModeSuggest},
		{"ask_always + automatic", domain.QuestionAutonomyAskAlways, domain.RepairModeAutomatic},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, store, parentID := f1Parent(t, tt.autonomy, tt.repair)
			childID := f1FirstChild(t, c, parentID)

			parent, child := f1PolicyOf(t, store, parentID), f1PolicyOf(t, store, childID)
			if got := parent.EffectiveAutonomyPolicy().Mode; got != tt.autonomy {
				t.Fatalf("parent autonomy = %s, want %s (fixture broken)", got, tt.autonomy)
			}
			if got := child.EffectiveAutonomyPolicy().Mode; got != tt.autonomy {
				t.Errorf("child autonomy = %s, want the parent's %s", got, tt.autonomy)
			}
			if got := child.EffectiveRepairPolicy().Mode; got != tt.repair {
				t.Errorf("child repair = %s, want the parent's %s", got, tt.repair)
			}
			if !child.Execution.AutonomousMode {
				t.Error("child autonomousMode = false, want the parent's true")
			}
			if err := domain.RequireInheritedWorkflowPolicy(parent, child); err != nil {
				t.Errorf("RequireInheritedWorkflowPolicy: %v", err)
			}
		})
	}
}

// TestF1_ChildStrategyIsRecomputedNotInherited proves the one field that must
// NOT be copied still is not: a child is never `master`, so copying the
// parent's selection would re-open the fan-out ChildExecutionStrategy closes.
func TestF1_ChildStrategyIsRecomputedNotInherited(t *testing.T) {
	c, store, parentID := f1Parent(t, domain.QuestionAutonomyAutoDecideLowRisk, domain.RepairModeAutomatic)
	childID := f1FirstChild(t, c, parentID)

	child := f1PolicyOf(t, store, childID)
	if child.Strategy.Effective == domain.ExecutionStrategyMaster {
		t.Fatalf("child strategy = master; a child must never be master")
	}
	if child.Strategy.ParentRunID != parentID {
		t.Errorf("child strategy parent = %q, want %q", child.Strategy.ParentRunID, parentID)
	}
}

// TestF1_ChildKeepsFrozenParentValuesAfterSettingsChange is §2: inheritance
// reads the parent RUN's frozen snapshot, never live settings. A user policy
// edited after the parent started must not reach a child created afterwards.
func TestF1_ChildKeepsFrozenParentValuesAfterSettingsChange(t *testing.T) {
	c, store, parentID := f1Parent(t, domain.QuestionAutonomyFullAutonomy, domain.RepairModeAutomatic)
	ctx := context.Background()

	// The owner turns autonomy right down AFTER the parent was frozen.
	if _, err := store.UpsertUserExecutionPolicy(ctx, domain.UserExecutionPolicy{
		UserID: "f1-owner", Version: domain.UserExecutionPolicyVersion, AutonomousMode: false,
		FallbackBehavior:   domain.FallbackUseNextAvailable,
		ReviewIndependence: domain.ReviewIndependenceRequireDifferentProvider,
	}); err != nil {
		t.Fatalf("UpsertUserExecutionPolicy: %v", err)
	}

	childID := f1FirstChild(t, c, parentID)
	parent, child := f1PolicyOf(t, store, parentID), f1PolicyOf(t, store, childID)
	if got := child.EffectiveAutonomyPolicy().Mode; got != domain.QuestionAutonomyFullAutonomy {
		t.Errorf("child autonomy = %s, want the parent's frozen full_autonomy", got)
	}
	if !child.Execution.AutonomousMode {
		t.Error("child autonomousMode = false; the later Settings edit must not reach an in-flight family")
	}
	if err := domain.RequireInheritedWorkflowPolicy(parent, child); err != nil {
		t.Errorf("RequireInheritedWorkflowPolicy: %v", err)
	}
}

// TestF1_InheritanceIsIdempotentAcrossRestart is §5's restart + idempotency
// case: a second coordinator over the same store re-running the inheritance
// (which is exactly what dispatchMasterTask's recovery branch does) must reach
// a byte-identical snapshot.
func TestF1_InheritanceIsIdempotentAcrossRestart(t *testing.T) {
	c, store, parentID := f1Parent(t, domain.QuestionAutonomyAutoDecideLowRisk, domain.RepairModeAutomatic)
	ctx := context.Background()
	childID := f1FirstChild(t, c, parentID)

	before, ok, err := store.GetWorkflowRun(ctx, childID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(child): ok=%v err=%v", ok, err)
	}

	// "Restart": a brand-new coordinator over the same durable state, then the
	// same reconcile that runs on every wake.
	c2 := workflowcore.New(workflowcore.Deps{
		Store: store, Projects: store, Planner: &staticPlanner{plan: validMasterPlan()},
		PlannerContextBuilder: staticContext{}, ProviderProfiles: store, ExecutionPolicies: store,
	})
	if _, err := c2.ContinueRun(ctx, parentID); err != nil {
		t.Fatalf("ContinueRun after restart: %v", err)
	}
	after, ok, err := store.GetWorkflowRun(ctx, childID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(child) after restart: ok=%v err=%v", ok, err)
	}

	beforePolicy, afterPolicy := domain.WorkflowPolicy{}, domain.WorkflowPolicy{}
	if err := json.Unmarshal([]byte(before.PolicySnapshot), &beforePolicy); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(after.PolicySnapshot), &afterPolicy); err != nil {
		t.Fatal(err)
	}
	if beforePolicy.EffectiveAutonomyPolicy().Mode != afterPolicy.EffectiveAutonomyPolicy().Mode {
		t.Errorf("autonomy changed across restart: %s -> %s",
			beforePolicy.EffectiveAutonomyPolicy().Mode, afterPolicy.EffectiveAutonomyPolicy().Mode)
	}
	if beforePolicy.EffectiveRepairPolicy().Mode != afterPolicy.EffectiveRepairPolicy().Mode {
		t.Errorf("repair changed across restart: %s -> %s",
			beforePolicy.EffectiveRepairPolicy().Mode, afterPolicy.EffectiveRepairPolicy().Mode)
	}
	if beforePolicy.Execution.AutonomousMode != afterPolicy.Execution.AutonomousMode {
		t.Errorf("autonomousMode changed across restart: %t -> %t",
			beforePolicy.Execution.AutonomousMode, afterPolicy.Execution.AutonomousMode)
	}
}

// TestF1_LegacyParentFallsBackSafely is §5's legacy case: a parent snapshot
// that predates Repair/Autonomy must leave the child on the conservative
// defaults, never escalate it into unattended autonomy.
func TestF1_LegacyParentFallsBackSafely(t *testing.T) {
	legacy := domain.WorkflowPolicy{Version: "v1", MaxFixCycles: 3}
	child := domain.DefaultWorkflowPolicy()

	got := domain.InheritWorkflowPolicy(legacy, child)
	if mode := got.EffectiveAutonomyPolicy().Mode; mode != domain.QuestionAutonomyAskAlways {
		t.Errorf("autonomy = %s, want the safe default ask_always", mode)
	}
	if mode := got.EffectiveRepairPolicy().Mode; mode != domain.RepairModeSuggest {
		t.Errorf("repair = %s, want the safe default suggest", mode)
	}
	if got.MaxFixCycles != 3 || got.MaxWorkProviderAttempts != child.MaxWorkProviderAttempts {
		t.Errorf("legacy parent overwrote a bounded budget the child had a default for: %+v", got)
	}
	if err := domain.RequireInheritedWorkflowPolicy(legacy, got); err != nil {
		t.Errorf("a legacy parent and its child must agree trivially, got %v", err)
	}
}

// TestF1_RequireInheritedWorkflowPolicyNamesTheDisagreeingField is §3: the
// fail-closed invariant must actually catch the F1 shape (parent says
// auto_decide_low_risk/automatic, child says ask_always/suggest) and say which
// field is wrong rather than failing anonymously.
func TestF1_RequireInheritedWorkflowPolicyNamesTheDisagreeingField(t *testing.T) {
	parent := domain.DefaultWorkflowPolicy()
	parent.Autonomy = domain.QuestionAutonomySnapshot{Version: domain.QuestionAutonomyPolicyVersion, Mode: domain.QuestionAutonomyAutoDecideLowRisk}
	parent.Repair = domain.RepairPolicySnapshot{Version: domain.RepairPolicyVersion, Mode: domain.RepairModeAutomatic, MaxRepairCycles: 2}

	// Exactly the child F1 produced: creation defaults, never inherited.
	child := domain.DefaultWorkflowPolicy()

	err := domain.RequireInheritedWorkflowPolicy(parent, child)
	if err == nil {
		t.Fatal("expected the invariant to refuse a child frozen at ask_always/suggest under an auto_decide_low_risk/automatic parent")
	}
	if !strings.Contains(err.Error(), "autonomy.mode") {
		t.Errorf("error should name the disagreeing field, got %q", err)
	}

	// Repair alone must be caught too.
	childAutonomyOK := domain.DefaultWorkflowPolicy()
	childAutonomyOK.Autonomy = parent.Autonomy
	err = domain.RequireInheritedWorkflowPolicy(parent, childAutonomyOK)
	if err == nil || !strings.Contains(err.Error(), "repair.mode") {
		t.Errorf("expected a repair.mode disagreement, got %v", err)
	}

	// And a correctly inherited child must pass.
	if err := domain.RequireInheritedWorkflowPolicy(parent, domain.InheritWorkflowPolicy(parent, domain.DefaultWorkflowPolicy())); err != nil {
		t.Errorf("a correctly inherited child must pass, got %v", err)
	}
}

// TestF1_UsageCeilingInheritedAndMeasuredOnTheParentFamily is §1's Usage case.
// The ceiling must reach the child (so a long-running child can be stopped
// between the parent's own task boundaries) while the MEASURED scope stays the
// parent's family — otherwise every child holds the whole budget again, which
// is the failure ParentScope exists to prevent.
func TestF1_UsageCeilingInheritedAndMeasuredOnTheParentFamily(t *testing.T) {
	parent := domain.DefaultWorkflowPolicy()
	parent.Usage = domain.UsageBudgetPolicy{Version: domain.UsageBudgetPolicyVersion, WorkflowTokenBudget: 200_000}

	child := domain.InheritWorkflowPolicy(parent, domain.DefaultWorkflowPolicy())
	if got := child.EffectiveUsageBudgetPolicy().WorkflowTokenBudget; got != 200_000 {
		t.Errorf("child token ceiling = %d, want the parent's 200000", got)
	}
	if !child.EffectiveUsageBudgetPolicy().ParentScoped() {
		t.Error("inherited budget must stay parent-scoped")
	}
	if err := domain.RequireInheritedWorkflowPolicy(parent, child); err != nil {
		t.Errorf("RequireInheritedWorkflowPolicy: %v", err)
	}
}
