package workflow

// P0-B regression: the plan segment's four deferred crash windows.
//
// Every test here drives the real sqlite store and asserts a DURABLE property,
// because every one of these defects is invisible in memory and only shows up
// as a row (or a missing row) that survives a restart.
//
//	CP13/CP14 — ApprovePlan's early exits skipped the transitions its own first
//	            write made necessary.
//	CP30      — the retry budget was recorded AFTER the reset it bounds.
//	CP31/CP32 — the terminal row landed BEFORE the explanation.
//	CP1       — a master run with no plan row was permanently inert.
//	CP7       — planner_ambiguous was permanent; it is now reopenable, by a
//	            person, bounded, under an observed-version compare-and-swap.

import (
	stdctx "context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// newPlanSegmentFixture creates a project and a bare master objective run whose
// plan row is in whatever state the test needs.
func newPlanSegmentFixture(t *testing.T) (*Coordinator, *sqlite.Store, stdctx.Context, string) {
	t.Helper()
	st := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	now := time.Now().UTC()
	if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	c := New(Deps{Store: st, Projects: st, Clock: func() time.Time { return time.Now().UTC() }})
	runID := "wf-plan-seg"
	run := domain.WorkflowRun{ID: runID, ProjectID: "p", Objective: "objective",
		State: domain.WorkflowRunPending, PolicyVersion: policyVersionV1, PolicySnapshot: "{}",
		CreatedAt: now, UpdatedAt: now}
	step := domain.WorkflowStep{ID: "wfs-plan", WorkflowRunID: runID, Kind: domain.WorkflowStepPlan,
		Ordinal: 1, State: domain.WorkflowStepWaiting, ArtifactJSON: "{}", CreatedAt: now, UpdatedAt: now}
	if _, _, err := st.CreateWorkflowRun(ctx, run, []domain.WorkflowStep{step}); err != nil {
		t.Fatal(err)
	}
	return c, st, ctx, runID
}

func phases(t *testing.T, st *sqlite.Store, ctx stdctx.Context, runID string) []string {
	t.Helper()
	cps, err := st.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(cps))
	for _, cp := range cps {
		out = append(out, cp.DurablePhase)
	}
	return out
}

// ---------------------------------------------------------------------------
// CP13 / CP14
// ---------------------------------------------------------------------------

// A crash anywhere inside ApprovePlan's write set used to be permanent: the
// plan read back `approved`, the early exit returned, and W2-W4 never ran
// again. CP14 is the consequential half — a run stuck at `pending` can never
// complete or report a stop, because every branch of reconcileMasterTasksOnce
// that would do either is gated on run.State == running.
func TestApprovePlanConvergesAWriteSetACrashLeftHalfDone(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stepState domain.WorkflowStepState
		runState  domain.WorkflowRunState
	}{
		{"CP13: plan approved, plan step still waiting", domain.WorkflowStepWaiting, domain.WorkflowRunPending},
		{"CP13: plan approved, plan step still running", domain.WorkflowStepRunning, domain.WorkflowRunPending},
		{"CP14: plan approved and step completed, run still pending", domain.WorkflowStepCompleted, domain.WorkflowRunPending},
		{"CP14: run still waiting", domain.WorkflowStepCompleted, domain.WorkflowRunWaiting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, st, ctx, runID := newPlanSegmentFixture(t)
			now := time.Now().UTC()
			if _, err := st.CreateWorkflowPlan(ctx, runID, domain.WorkflowPlanApprovalManual, PlannerContextVersion, now); err != nil {
				t.Fatal(err)
			}
			// W1 landed and nothing after it did.
			approvePlanRowOnly(t, st, ctx, runID, now)
			steps, _ := st.ListWorkflowSteps(ctx, runID)
			if tc.stepState != domain.WorkflowStepWaiting {
				if _, err := st.UpdateWorkflowStepState(ctx, steps[0].ID, domain.WorkflowStepWaiting, domain.WorkflowStepRunning, now); err != nil {
					t.Fatal(err)
				}
				if tc.stepState == domain.WorkflowStepCompleted {
					if _, err := st.UpdateWorkflowStepState(ctx, steps[0].ID, domain.WorkflowStepRunning, domain.WorkflowStepCompleted, now); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.runState == domain.WorkflowRunWaiting {
				if _, err := st.UpdateWorkflowRunState(ctx, runID, domain.WorkflowRunPending, domain.WorkflowRunWaiting, now); err != nil {
					t.Fatal(err)
				}
			}

			// Re-entering ApprovePlan must FINISH the write set, not return over it.
			if _, err := c.ApprovePlan(ctx, runID); err != nil {
				t.Fatalf("ApprovePlan: %v", err)
			}
			steps, _ = st.ListWorkflowSteps(ctx, runID)
			if steps[0].State != domain.WorkflowStepCompleted {
				t.Fatalf("plan step = %q, want completed: the approved plan's own step transition was skipped", steps[0].State)
			}
			run, _, _ := st.GetWorkflowRun(ctx, runID)
			if run.State != domain.WorkflowRunRunning {
				t.Fatalf("run = %q, want running: an objective that never reaches running can never complete or report a stop", run.State)
			}
		})
	}
}

// Nothing re-enters ApprovePlan once a plan is approved, so boot recovery is
// the only place a CP14 crash is ever healed. It must converge BEFORE the task
// reconciliation that depends on run.State == running.
func TestBootRecoveryConvergesAnApprovedPlanWhoseRunNeverReachedRunning(t *testing.T) {
	c, st, ctx, runID := newPlanSegmentFixture(t)
	now := time.Now().UTC()
	if _, err := st.CreateWorkflowPlan(ctx, runID, domain.WorkflowPlanApprovalManual, PlannerContextVersion, now); err != nil {
		t.Fatal(err)
	}
	approvePlanRowOnly(t, st, ctx, runID, now)
	if err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	run, _, _ := st.GetWorkflowRun(ctx, runID)
	if run.State != domain.WorkflowRunRunning {
		t.Fatalf("run = %q, want running after boot recovery", run.State)
	}
	steps, _ := st.ListWorkflowSteps(ctx, runID)
	if steps[0].State != domain.WorkflowStepCompleted {
		t.Fatalf("plan step = %q, want completed after boot recovery", steps[0].State)
	}
}

// A plan that is not validated is still refused. Convergence is for a plan that
// IS approved, never a way into approval.
func TestApprovePlanStillRefusesAPlanThatIsNotValidated(t *testing.T) {
	c, st, ctx, runID := newPlanSegmentFixture(t)
	if _, err := st.CreateWorkflowPlan(ctx, runID, domain.WorkflowPlanApprovalManual, PlannerContextVersion, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ApprovePlan(ctx, runID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid for a pending plan", err)
	}
	run, _, _ := st.GetWorkflowRun(ctx, runID)
	if run.State != domain.WorkflowRunPending {
		t.Fatalf("run = %q, want pending: a refused approval must move nothing", run.State)
	}
}

// ---------------------------------------------------------------------------
// CP30 / CP31 / CP32 — write ORDER
// ---------------------------------------------------------------------------

// The budget row must be durable BEFORE the reset it bounds. Arming first and
// recording second means a crash in that window widens a budget of three by one
// — for every such crash, forever.
func TestPlannerRetryRecordsItsBudgetBeforeArmingTheNextAttempt(t *testing.T) {
	c, st, ctx, runID := newPlanSegmentFixture(t)
	now := time.Now().UTC()
	if _, err := st.CreateWorkflowPlan(ctx, runID, domain.WorkflowPlanApprovalManual, PlannerContextVersion, now); err != nil {
		t.Fatal(err)
	}
	run, _, _ := st.GetWorkflowRun(ctx, runID)
	if _, err := c.retryPlanOrFail(ctx, run, "planner_timeout", errors.New("timed out")); err != nil {
		t.Fatalf("retryPlanOrFail: %v", err)
	}
	cps, err := st.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cps) == 0 || cps[0].DurablePhase != ReasonPlannerRetryScheduled {
		t.Fatalf("first durable row = %v, want the retry budget row first", phases(t, st, ctx, runID))
	}
	// And the budget is now genuinely one unit smaller.
	if got := c.plannerRetryCount(ctx, runID); got != 1 {
		t.Fatalf("plannerRetryCount = %d, want 1", got)
	}
	plan, _, _ := st.GetWorkflowPlan(ctx, runID)
	if plan.Status != domain.WorkflowPlanPending || plan.CommandStatus != domain.WorkflowPlanCommandIdle {
		t.Fatalf("plan = %q/%q, want pending/idle so the retry re-enters the ordinary path", plan.Status, plan.CommandStatus)
	}
}

// failPlan's terminal row is permanent for GeneratePlan. Its explanation must
// therefore land first: a reason with no terminal row is harmless, a terminal
// row with no reason is an unexplained dead end.
func TestFailPlanRecordsTheReasonBeforeTheTerminalRow(t *testing.T) {
	c, st, ctx, runID := newPlanSegmentFixture(t)
	if _, err := st.CreateWorkflowPlan(ctx, runID, domain.WorkflowPlanApprovalManual, PlannerContextVersion, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	run, _, _ := st.GetWorkflowRun(ctx, runID)
	if _, err := c.failPlan(ctx, run, ReasonPlannerExhausted, errors.New("gave up")); err != nil {
		t.Fatalf("failPlan: %v", err)
	}
	cps, _ := st.ListWorkflowCheckpoints(ctx, runID)
	if len(cps) == 0 || cps[0].DurablePhase != ReasonPlannerExhausted {
		t.Fatalf("first durable row = %v, want the reason first", phases(t, st, ctx, runID))
	}
}

// CP32: the same rule, in boot recovery's own ambiguous-planner arm.
func TestBootRecoveryRecordsThePlannerAmbiguityBeforeMarkingThePlanInvalid(t *testing.T) {
	c, st, ctx, runID := newPlanSegmentFixture(t)
	now := time.Now().UTC()
	if _, err := st.CreateWorkflowPlan(ctx, runID, domain.WorkflowPlanApprovalManual, PlannerContextVersion, now); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.StartWorkflowPlanCommand(ctx, runID, "fake", "fake-v1", "{}", now); err != nil || !ok {
		t.Fatalf("StartWorkflowPlanCommand: ok=%v err=%v", ok, err)
	}
	if err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	cps, _ := st.ListWorkflowCheckpoints(ctx, runID)
	if len(cps) == 0 || cps[0].DurablePhase != ReasonPlannerAmbiguous {
		t.Fatalf("first durable row = %v, want the ambiguity reason first", phases(t, st, ctx, runID))
	}
	plan, _, _ := st.GetWorkflowPlan(ctx, runID)
	if plan.Status != domain.WorkflowPlanInvalid || plan.ErrorClass != ReasonPlannerAmbiguous {
		t.Fatalf("plan = %q/%q: the verdict itself must be unchanged and still fail-closed", plan.Status, plan.ErrorClass)
	}
}

// ---------------------------------------------------------------------------
// CP1
// ---------------------------------------------------------------------------

// The window is CLOSED for new objectives: run, plan step and plan row are one
// transaction.
func TestCreateObjectiveRunWritesTheRunAndItsPlanInOneTransaction(t *testing.T) {
	st := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	c := New(Deps{Store: st, Projects: st, Planner: nopPlanner{}, PlannerContextBuilder: nopPlannerContext{}})
	created, err := c.CreateObjectiveRun(ctx, "p", "build it", domain.WorkflowPlanApprovalAuto)
	if err != nil {
		t.Fatal(err)
	}
	plan, master, err := st.GetWorkflowPlan(ctx, created.Run.ID)
	if err != nil || !master {
		t.Fatalf("plan row missing: master=%v err=%v", master, err)
	}
	if plan.ApprovalMode != domain.WorkflowPlanApprovalAuto {
		t.Fatalf("approval mode = %q, want the mode the request asked for", plan.ApprovalMode)
	}
}

// And the rows a pre-fix daemon already left on disk are healed — fail-closed,
// on the one field that is genuinely unrecoverable.
func TestBootRecoveryHealsAnObjectiveRunThatHasNoPlanRow(t *testing.T) {
	c, st, ctx, runID := newPlanSegmentFixture(t)
	if err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	plan, master, err := st.GetWorkflowPlan(ctx, runID)
	if err != nil || !master {
		t.Fatalf("the orphaned objective was not healed: master=%v err=%v", master, err)
	}
	if plan.ApprovalMode != domain.WorkflowPlanApprovalManual {
		t.Fatalf("approval mode = %q, want manual: inferring auto would start an unattended planner nobody asked for", plan.ApprovalMode)
	}
	got := phases(t, st, ctx, runID)
	if !containsPhase(got, ReasonObjectivePlanRecovered) || !containsPhase(got, orphanedObjectivePlanHealedPhase) {
		t.Fatalf("phases = %v, want both the human-readable stop and the healer's own record", got)
	}
	// The reason is durable BEFORE the repair, same ordering rule as CP31/CP32.
	cps, _ := st.ListWorkflowCheckpoints(ctx, runID)
	if cps[0].DurablePhase != ReasonObjectivePlanRecovered {
		t.Fatalf("first durable row = %q, want the substitution to be announced first", cps[0].DurablePhase)
	}
}

// A child run is never an objective, whatever its steps look like.
func TestTheOrphanedObjectiveHealerIgnoresAChildRun(t *testing.T) {
	c, st, ctx, _ := newPlanSegmentFixture(t)
	now := time.Now().UTC()
	parent, task := "wf-plan-seg", "task-1"
	child := domain.WorkflowRun{ID: "wf-child", ProjectID: "p", Objective: "o",
		State: domain.WorkflowRunPending, PolicyVersion: policyVersionV1, PolicySnapshot: "{}",
		CreatedAt: now, UpdatedAt: now, ParentWorkflowID: &parent, PlannedTaskID: &task}
	step := domain.WorkflowStep{ID: "wfs-child-plan", WorkflowRunID: child.ID, Kind: domain.WorkflowStepPlan,
		Ordinal: 1, State: domain.WorkflowStepWaiting, ArtifactJSON: "{}", CreatedAt: now, UpdatedAt: now}
	if _, _, err := st.CreateWorkflowRun(ctx, child, []domain.WorkflowStep{step}); err != nil {
		t.Fatal(err)
	}
	healed, err := c.healOrphanedObjectiveRun(ctx, child, now)
	if err != nil {
		t.Fatal(err)
	}
	if healed {
		t.Fatal("a child run must never be healed into a master objective")
	}
}

// ---------------------------------------------------------------------------
// CP7 — the reopen
// ---------------------------------------------------------------------------

// ambiguousPlanFixture parks a run in exactly the state recovery.go's
// ambiguous-planner arm produces.
func ambiguousPlanFixture(t *testing.T) (*Coordinator, *sqlite.Store, stdctx.Context, string) {
	t.Helper()
	c, st, ctx, runID := newPlanSegmentFixture(t)
	now := time.Now().UTC()
	if _, err := st.CreateWorkflowPlan(ctx, runID, domain.WorkflowPlanApprovalManual, PlannerContextVersion, now); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.StartWorkflowPlanCommand(ctx, runID, "fake", "fake-v1", "{}", now); err != nil || !ok {
		t.Fatalf("StartWorkflowPlanCommand: ok=%v err=%v", ok, err)
	}
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	return c, st, ctx, runID
}

func TestReopenAmbiguousPlanReturnsTheObjectiveToOrdinaryPlanning(t *testing.T) {
	c, st, ctx, runID := ambiguousPlanFixture(t)
	plan, _, _ := st.GetWorkflowPlan(ctx, runID)
	if plan.Status != domain.WorkflowPlanInvalid {
		t.Fatalf("fixture did not reach the ambiguous state: %+v", plan)
	}
	if _, err := c.ReopenAmbiguousPlan(ctx, runID, plan.UpdatedAt); err != nil {
		t.Fatalf("ReopenAmbiguousPlan: %v", err)
	}
	reopened, _, _ := st.GetWorkflowPlan(ctx, runID)
	if reopened.Status != domain.WorkflowPlanPending || reopened.CommandStatus != domain.WorkflowPlanCommandIdle {
		t.Fatalf("plan = %q/%q, want pending/idle — the exact state GeneratePlan falls through to real generation on",
			reopened.Status, reopened.CommandStatus)
	}
	if reopened.ErrorClass != "" {
		t.Fatalf("error class = %q, want cleared", reopened.ErrorClass)
	}
	run, _, _ := st.GetWorkflowRun(ctx, runID)
	if run.State == domain.WorkflowRunNeedsAttention {
		t.Fatal("the run is still parked on the stop the reopen answers")
	}
	// The authorization is on the ledger, and it is durable BEFORE the plan row
	// moved: this reopen re-arms a planner that spends real provider budget.
	cps, _ := st.ListWorkflowCheckpoints(ctx, runID)
	idx := -1
	for i, cp := range cps {
		if cp.DurablePhase == ambiguousPlanReopenPhase {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("phases = %v, want the reopen authorization recorded", phases(t, st, ctx, runID))
	}
}

// The observed-version compare-and-swap is the whole safety property: a reopen
// carrying a version the row no longer has must refuse, never accept.
func TestReopenAmbiguousPlanRefusesAVersionTheRowNoLongerHas(t *testing.T) {
	c, st, ctx, runID := ambiguousPlanFixture(t)
	plan, _, _ := st.GetWorkflowPlan(ctx, runID)
	stale := plan.UpdatedAt

	// Any write to the row moves the version. SetWorkflowPlanApprovalMode is
	// the one that leaves status/command_status/error_class identical, which is
	// exactly why the three state columns alone would NOT be sufficient.
	if _, err := st.SetWorkflowPlanApprovalMode(ctx, runID, domain.WorkflowPlanApprovalAuto, stale.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReopenAmbiguousPlan(ctx, runID, stale); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid: a reopen must never land on a state nobody looked at", err)
	}
	after, _, _ := st.GetWorkflowPlan(ctx, runID)
	if after.Status != domain.WorkflowPlanInvalid {
		t.Fatalf("plan = %q, want the refusal to have moved nothing", after.Status)
	}
	// Re-reading and re-submitting works, which is what makes the refusal a
	// nuisance rather than a dead end.
	if _, err := c.ReopenAmbiguousPlan(ctx, runID, after.UpdatedAt); err != nil {
		t.Fatalf("re-reading and re-submitting must succeed: %v", err)
	}
}

func TestReopenAmbiguousPlanIsBounded(t *testing.T) {
	c, st, ctx, runID := ambiguousPlanFixture(t)
	for i := 0; i < maxAmbiguousPlanReopens; i++ {
		plan, _, _ := st.GetWorkflowPlan(ctx, runID)
		if _, err := c.ReopenAmbiguousPlan(ctx, runID, plan.UpdatedAt); err != nil {
			t.Fatalf("reopen %d: %v", i+1, err)
		}
		// Put it back into the ambiguous state, as a second crossed restart would.
		if err := c.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := st.StartWorkflowPlanCommand(ctx, runID, "fake", "fake-v1", "{}", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if err := c.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	plan, _, _ := st.GetWorkflowPlan(ctx, runID)
	_, err := c.ReopenAmbiguousPlan(ctx, runID, plan.UpdatedAt)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "will not reopen it again") {
		t.Fatalf("err = %v, want a bounded refusal: even a person holding the button must not loop forever", err)
	}
}

func TestReopenAmbiguousPlanRefusesAPlanThatIsNotAmbiguous(t *testing.T) {
	c, st, ctx, runID := newPlanSegmentFixture(t)
	now := time.Now().UTC()
	if _, err := st.CreateWorkflowPlan(ctx, runID, domain.WorkflowPlanApprovalManual, PlannerContextVersion, now); err != nil {
		t.Fatal(err)
	}
	plan, _, _ := st.GetWorkflowPlan(ctx, runID)
	if _, err := c.ReopenAmbiguousPlan(ctx, runID, plan.UpdatedAt); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid: only the ambiguous-terminal state is reopenable", err)
	}
	// And an ambiguous plan reopened with no observed version at all is refused
	// rather than defaulted.
	if _, err := c.ReopenAmbiguousPlan(ctx, runID, time.Time{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid for a missing observed version", err)
	}
}

// Nothing automatic may reach the reopen. This is the property that keeps
// restart -> reopen -> planner -> restart from being an unattended loop.
func TestNothingAutomaticReopensAnAmbiguousPlan(t *testing.T) {
	c, st, ctx, runID := ambiguousPlanFixture(t)
	for i := 0; i < 3; i++ {
		if err := c.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := c.ContinueRun(ctx, runID); err != nil {
			t.Fatalf("ContinueRun: %v", err)
		}
	}
	plan, _, _ := st.GetWorkflowPlan(ctx, runID)
	if plan.Status != domain.WorkflowPlanInvalid || plan.ErrorClass != ReasonPlannerAmbiguous {
		t.Fatalf("plan = %q/%q: an ambiguous planner must stay fail-closed until a person acts",
			plan.Status, plan.ErrorClass)
	}
	if containsPhase(phases(t, st, ctx, runID), ambiguousPlanReopenPhase) {
		t.Fatal("an automatic pass authorized a reopen")
	}
}

func containsPhase(all []string, want string) bool {
	for _, p := range all {
		if p == want {
			return true
		}
	}
	return false
}

// nopPlanner / nopPlannerContext satisfy CreateObjectiveRun's guard without
// generating anything: these tests never call GeneratePlan.
type nopPlanner struct{}

func (nopPlanner) Generate(stdctx.Context, PlannerRequest) (PlannerResponse, error) {
	return PlannerResponse{}, errors.New("not used")
}
func (nopPlanner) Descriptor() (string, string) { return "nop", "nop" }

type nopPlannerContext struct{}

func (nopPlannerContext) Build(_ stdctx.Context, p domain.ProjectRecord) (PlannerContext, error) {
	return PlannerContext{Version: "v1", ProjectID: p.ID, ProjectPath: p.Path}, nil
}

// approvePlanRowOnly drives the plan row alone to `approved` — W1 of
// ApprovePlan's write set — leaving the plan step and the run exactly where a
// crash immediately afterwards would have left them.
func approvePlanRowOnly(t *testing.T, st *sqlite.Store, ctx stdctx.Context, runID string, now time.Time) {
	t.Helper()
	if ok, err := st.StartWorkflowPlanCommand(ctx, runID, "fake", "fake-v1", "{}", now); err != nil || !ok {
		t.Fatalf("StartWorkflowPlanCommand: ok=%v err=%v", ok, err)
	}
	if ok, err := st.FinishWorkflowPlan(ctx, runID, domain.WorkflowPlanValidated, domain.WorkflowPlanCommandResponded, "{}", "h", "", now); err != nil || !ok {
		t.Fatalf("FinishWorkflowPlan: ok=%v err=%v", ok, err)
	}
	if ok, err := st.ApproveWorkflowPlan(ctx, runID, now); err != nil || !ok {
		t.Fatalf("ApproveWorkflowPlan: ok=%v err=%v", ok, err)
	}
}
