package workflow_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// planner_launch_reliability_test.go is the coordinator half of the
// wf-7f8cc736 incident. The adapter half (which provider failure is which)
// lives in adapters/planner/command; this file asserts what AO DOES with each
// answer, because that is the part the incident got wrong: every planner
// failure, whatever its cause, ended as one nonrecoverable
// `planner_start_failed` whose sentence named neither the cause nor a repair.
//
// The distinction under test is §7's: which failures are retried and which are
// not. A missing binary, a rejected credential, an unreadable profile and a
// refused invocation are all PERMANENT — a second identical attempt spends
// provider budget to reach the same stop. A provider that started and died
// without saying why is RETRYABLE, on the same bounded budget a timeout uses.

// launchFailure wraps a sentinel the way the real adapter does, including the
// provider prose the fix exists to preserve.
func launchFailure(sentinel error, detail string) error {
	return fmt.Errorf("%w: provider claude (/usr/local/bin/claude) exited 1; reason: %s", sentinel, detail)
}

func TestGeneratePlan_LaunchFailuresAreClassifiedAndNotAllRetried(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantClass  string
		wantStatus domain.WorkflowPlanStatus
		// wantAction is a phrase the operator-facing remedy must contain, so a
		// registry entry cannot silently regress to generic advice.
		wantAction string
	}{
		{
			name:       "binary missing is permanent and names the installation",
			err:        launchFailure(ports.ErrPlannerBinaryMissing, `"claude" is not on the PATH the planner subprocess would inherit`),
			wantClass:  workflowcore.ReasonPlannerBinaryMissing,
			wantStatus: domain.WorkflowPlanInvalid,
			wantAction: "PATH",
		},
		{
			name:       "rejected credentials are permanent and ask for a sign-in",
			err:        launchFailure(ports.ErrPlannerAuthRequired, "Invalid API key · Please run /login"),
			wantClass:  workflowcore.ReasonPlannerAuthUnavailable,
			wantStatus: domain.WorkflowPlanInvalid,
			wantAction: "Sign that provider in",
		},
		{
			name:       "an unreadable profile is its own fault, not an auth prompt",
			err:        launchFailure(ports.ErrPlannerRuntimeHomeUnreadable, "HOME=/nope: no such file or directory"),
			wantClass:  workflowcore.ReasonPlannerProfileUnreadable,
			wantStatus: domain.WorkflowPlanInvalid,
			wantAction: "profile directory",
		},
		{
			name:       "a refused invocation is a provider version problem",
			err:        launchFailure(ports.ErrPlannerUnsupportedInvocation, "error: unknown option '--json-schema'"),
			wantClass:  workflowcore.ReasonPlannerProviderUnsupported,
			wantStatus: domain.WorkflowPlanInvalid,
			wantAction: "CLI rejected AO's invocation",
		},
		{
			name:       "a provider that died without saying why is retried, not failed",
			err:        launchFailure(ports.ErrPlannerLaunchFailed, "no output (exit status 3)"),
			wantClass:  workflowcore.ReasonPlannerExitedEarly,
			wantStatus: domain.WorkflowPlanPending,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, runID := newFailingMasterFixture(t, tt.err)
			detail, err := c.GeneratePlan(context.Background(), runID)
			if err != nil {
				t.Fatal(err)
			}
			if detail.Plan.ErrorClass != tt.wantClass {
				t.Fatalf("errorClass=%q, want %q", detail.Plan.ErrorClass, tt.wantClass)
			}
			if detail.Plan.Status != tt.wantStatus {
				t.Fatalf("plan status=%q, want %q", detail.Plan.Status, tt.wantStatus)
			}
			if tt.wantAction == "" {
				return
			}
			life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail, Questions: detail.Questions})
			if life.AttentionReason != tt.wantClass {
				t.Fatalf("attention reason=%q, want %q", life.AttentionReason, tt.wantClass)
			}
			if !strings.Contains(life.AttentionAction, tt.wantAction) {
				t.Fatalf("attention action %q does not name the repair (%q)", life.AttentionAction, tt.wantAction)
			}
			if strings.Contains(life.AttentionAction, "Check the planner provider's auth and installation") {
				t.Fatal("a classified launch failure must not fall back to the generic pre-incident advice")
			}
		})
	}
}

// TestGeneratePlan_PermanentLaunchFailureIsNotRetried is the budget half of
// §7: a cause that cannot change must not consume the planner retry budget at
// all. The first call is terminal.
func TestGeneratePlan_PermanentLaunchFailureIsNotRetried(t *testing.T) {
	ctx := context.Background()
	c, runID := newFailingMasterFixture(t, launchFailure(ports.ErrPlannerBinaryMissing, `"claude" is not on PATH`))

	detail, err := c.GeneratePlan(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Plan.Status != domain.WorkflowPlanInvalid {
		t.Fatalf("plan status=%q on the FIRST missing-binary failure, want invalid", detail.Plan.Status)
	}
	if detail.LatestCheckpointPhase == workflowcore.ReasonPlannerRetryScheduled {
		t.Fatal("a missing binary scheduled a planner retry; installing a CLI is not something a retry can do")
	}
	if detail.LatestCheckpointPhase != workflowcore.ReasonPlannerBinaryMissing {
		t.Fatalf("latest checkpoint phase=%q, want %q", detail.LatestCheckpointPhase, workflowcore.ReasonPlannerBinaryMissing)
	}
}

// TestGeneratePlan_TransientLaunchFailureRetriesThenStops is the other half:
// the retryable class is bounded, and its eventual stop is the honest
// exhausted one rather than a permanent launch verdict.
func TestGeneratePlan_TransientLaunchFailureRetriesThenStops(t *testing.T) {
	ctx := context.Background()
	c, runID := newFailingMasterFixture(t, launchFailure(ports.ErrPlannerLaunchFailed, "no output (signal: killed)"))

	var last workflowcore.RunDetail
	retries := 0
	for i := 0; i < 8; i++ {
		detail, err := c.GeneratePlan(ctx, runID)
		if err != nil {
			t.Fatalf("GeneratePlan call %d: %v", i, err)
		}
		last = detail
		if detail.Plan.Status == domain.WorkflowPlanInvalid {
			break
		}
		retries++
	}
	if retries == 0 {
		t.Fatal("a provider that died mid-call was never retried")
	}
	if last.Plan.Status != domain.WorkflowPlanInvalid {
		t.Fatalf("plan status=%q after the budget; retries must be bounded", last.Plan.Status)
	}
	if last.Plan.ErrorClass != workflowcore.ReasonPlannerExhausted {
		t.Fatalf("errorClass=%q, want %q once the budget is spent", last.Plan.ErrorClass, workflowcore.ReasonPlannerExhausted)
	}
}

// TestGeneratePlan_ProviderReasonSurvivesIntoTheDurableStop is the regression
// for the exact evidence gap: the sentence AO persists must carry what the
// provider said, because that string is the only account of the failure that
// outlives the daemon.
func TestGeneratePlan_ProviderReasonSurvivesIntoTheDurableStop(t *testing.T) {
	const reason = "Invalid API key · Please run /login"
	c, runID := newFailingMasterFixture(t, launchFailure(ports.ErrPlannerAuthRequired, reason))
	detail, err := c.GeneratePlan(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail.NextAction, reason) {
		t.Fatalf("the run's next action lost the provider's own reason: %q", detail.NextAction)
	}
	if !strings.Contains(detail.Plan.ValidationJSON, "Invalid API key") {
		t.Fatalf("the plan's validation record lost the provider reason: %s", detail.Plan.ValidationJSON)
	}
}

// TestGeneratePlan_CapacityShapedLaunchFailureStillParks proves the fix did not
// steal capacity handling: a provider that is merely busy must still park and
// wake, not burn the planner's small retry budget or stop for a person.
func TestGeneratePlan_CapacityShapedLaunchFailureStillParks(t *testing.T) {
	c, runID := newFailingMasterFixture(t, launchFailure(ports.ErrPlannerLaunchFailed, "Claude AI usage limit reached; try again later"))
	detail, err := c.GeneratePlan(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Plan.ErrorClass != "planner_capacity" {
		t.Fatalf("errorClass=%q, want planner_capacity", detail.Plan.ErrorClass)
	}
	if detail.Plan.Status != domain.WorkflowPlanPending {
		t.Fatalf("plan status=%q, want pending", detail.Plan.Status)
	}
}

// restartableFixture is newFailingMasterFixture's restart-capable sibling: it
// hands back the store as well, so a second Coordinator can be built over the
// same durable state — which is what a daemon restart IS, from the run's point
// of view.
func restartableFixture(t *testing.T, plannerErr error, objective string) (*workflowcore.Coordinator, *sqlite.Store, string) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	project := domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}
	if err := store.UpsertProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	c := workflowcore.New(workflowcore.Deps{Store: store, Projects: store, Planner: failingPlanner{err: plannerErr}, PlannerContextBuilder: staticContext{}})
	created, err := c.CreateObjectiveRun(ctx, "p", objective, domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatal(err)
	}
	return c, store, created.Run.ID
}

// TestPlannerLaunchRetryBudgetSurvivesADaemonRestart is §11's core: a restart
// must neither reset the budget (an unbounded retry loop, one crash at a time)
// nor block a legitimate next attempt with stale runtime state. The budget is
// derived from append-only checkpoints, so a brand-new Coordinator over the
// same store sees exactly what the old one spent.
func TestPlannerLaunchRetryBudgetSurvivesADaemonRestart(t *testing.T) {
	ctx := context.Background()
	c, store, runID := restartableFixture(t, launchFailure(ports.ErrPlannerLaunchFailed, "no output (signal: killed)"), medusaSizedObjective())

	// One attempt before the "restart".
	detail, err := c.GeneratePlan(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Plan.Status != domain.WorkflowPlanPending {
		t.Fatalf("plan status=%q after one exited-early failure, want pending", detail.Plan.Status)
	}

	// The restart: a fresh Coordinator, no in-memory carry-over.
	restarted := workflowcore.New(workflowcore.Deps{
		Store: store, Projects: store,
		Planner:               failingPlanner{err: launchFailure(ports.ErrPlannerLaunchFailed, "no output (signal: killed)")},
		PlannerContextBuilder: staticContext{},
	})
	last := detail
	for i := 0; i < 8; i++ {
		d, err := restarted.GeneratePlan(ctx, runID)
		if err != nil {
			t.Fatalf("GeneratePlan after restart, call %d: %v", i, err)
		}
		last = d
		if d.Plan.Status == domain.WorkflowPlanInvalid {
			break
		}
	}
	if last.Plan.Status != domain.WorkflowPlanInvalid {
		t.Fatal("the retry budget restarted with the daemon; a crash loop would retry the planner forever")
	}
	if last.Plan.ErrorClass != workflowcore.ReasonPlannerExhausted {
		t.Fatalf("errorClass=%q, want %q", last.Plan.ErrorClass, workflowcore.ReasonPlannerExhausted)
	}
}

// TestRetryPlanningAfterALaunchFailureKeepsTheObjective is §9: an objective
// stopped at planning on a launch failure must be re-plannable once the cause
// is fixed — same run, same specification, no duplicate — and the repaired
// planner must be the one that runs.
func TestRetryPlanningAfterALaunchFailureKeepsTheObjective(t *testing.T) {
	ctx := context.Background()
	// The objective and the repaired planner's plan are a matching pair, so
	// this test measures the retry-planning path and not the plan-plausibility
	// gate that sits after it.
	c, store, runID := restartableFixture(t,
		launchFailure(ports.ErrPlannerBinaryMissing, `"claude" is not on PATH`),
		"Extend the greetings module with a farewell entry point, a shared normalization helper and documentation.")

	stopped, err := c.GeneratePlan(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Plan.Status != domain.WorkflowPlanInvalid {
		t.Fatalf("plan status=%q, want invalid", stopped.Plan.Status)
	}
	objective := stopped.Run.Objective
	revisionBefore := stopped.Plan.Revision

	// The operator installs the CLI and asks AO to plan again. The provider is
	// healthy this time.
	repaired := workflowcore.New(workflowcore.Deps{
		Store: store, Projects: store,
		Planner:               &staticPlanner{plan: f2GateRealPlan()},
		PlannerContextBuilder: staticContext{},
	})
	regenerated, _, err := repaired.RegeneratePlan(ctx, runID)
	if err != nil {
		t.Fatalf("RegeneratePlan: %v", err)
	}
	if regenerated.Run.ID != runID {
		t.Fatalf("retrying planning created a different run: %q", regenerated.Run.ID)
	}
	if regenerated.Run.Objective != objective {
		t.Fatal("retrying planning lost the objective's specification")
	}
	if regenerated.Plan.Revision <= revisionBefore {
		t.Fatalf("revision=%d, want a new revision after %d", regenerated.Plan.Revision, revisionBefore)
	}

	planned, err := repaired.GeneratePlan(ctx, runID)
	if err != nil {
		t.Fatalf("GeneratePlan after repair: %v", err)
	}
	if planned.Plan.Status != domain.WorkflowPlanValidated {
		t.Fatalf("plan status=%q after the cause was repaired, want validated", planned.Plan.Status)
	}
	if planned.Plan.ErrorClass != "" {
		t.Fatalf("the repaired plan still carries errorClass=%q", planned.Plan.ErrorClass)
	}
}

// cancellingPlanner cancels the CALLER's context from inside Generate, then
// returns a real plan. That is exactly what a 60-second REST timeout does to a
// three-minute planner call: the answer arrives after the request that asked
// for it is already dead.
type cancellingPlanner struct {
	plan   workflowcore.MasterPlan
	cancel context.CancelFunc
}

func (p *cancellingPlanner) Generate(context.Context, workflowcore.PlannerRequest) (workflowcore.PlannerResponse, error) {
	p.cancel()
	return workflowcore.PlannerResponse{Plan: p.plan, Provider: "fake", Model: "fake-v1"}, nil
}
func (p *cancellingPlanner) Descriptor() (string, string) { return "fake", "fake-v1" }

// TestGeneratePlan_PersistsThePlanAfterTheCallerGivesUp is the regression for
// the third half of the incident, and the one that wedged the objective for
// good.
//
// The REST group's request timeout is 60s; a planner's budget runs to 12
// minutes. Every real objective's plan/generate therefore outlives its own
// request. When only the subprocess call was detached from the caller, the
// planner produced a real plan and every durable write after it -- the usage
// record, the capacity release, the plan row itself -- ran on a dead context
// and failed. What was left on disk was a plan row stuck at running/running
// holding "{}", a leaked capacity claim, and a run that answered every
// subsequent attempt with "planner command already running".
func TestGeneratePlan_PersistsThePlanAfterTheCallerGivesUp(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	setup := context.Background()
	project := domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}
	if err := store.UpsertProject(setup, project); err != nil {
		t.Fatal(err)
	}
	callerCtx, cancel := context.WithCancel(setup)
	defer cancel()

	c := workflowcore.New(workflowcore.Deps{
		Store: store, Projects: store,
		Planner:               &cancellingPlanner{plan: f2GateRealPlan(), cancel: cancel},
		PlannerContextBuilder: staticContext{},
	})
	created, err := c.CreateObjectiveRun(setup, "p",
		"Extend the greetings module with a farewell entry point, a shared normalization helper and documentation.",
		domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.GeneratePlan(callerCtx, created.Run.ID); err != nil {
		t.Fatalf("GeneratePlan: %v", err)
	}

	// The verdict is read on a LIVE context: what matters is what landed on
	// disk, not what the abandoned request managed to return.
	detail, err := c.GetRun(setup, created.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Plan.Status != domain.WorkflowPlanValidated {
		t.Fatalf("plan status=%q after the caller gave up, want validated; the plan the provider was paid for was discarded",
			detail.Plan.Status)
	}
	if detail.Plan.CommandStatus == domain.WorkflowPlanCommandRunning {
		t.Fatal("the plan command is stuck at running; every later attempt would be refused with \"planner command already running\"")
	}
	if len(detail.Plan.GeneratedPlanJSON) < 2 || detail.Plan.GeneratedPlanJSON == "{}" {
		t.Fatalf("the generated plan was not persisted: %q", detail.Plan.GeneratedPlanJSON)
	}
}
