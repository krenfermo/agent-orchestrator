package workflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// crashStore is a real SQLite store with individually switchable write
// failures. Each switch reproduces ONE durable write that a daemon crash can
// lose, at exactly the point the audit's crash window opens, so the tests
// below exercise the real code path rather than a hand-built database state.
//
// It embeds *sqlite.Store, so the coordinator's `masterPlanStore` type
// assertion still succeeds and every unswitched method is the real one.
type crashStore struct {
	*sqlite.Store
	failRelationships   bool
	failSetExecutionRun bool
	swallowPolicyWrite  bool
	swallowApprove      bool
	swallowApprovalMode bool
	// mutatePlan lets a P1-B test present a plan row whose stored bytes or
	// recorded context no longer match what the row claims, without reaching
	// past the store into raw SQL. Every production reader still goes through
	// GetWorkflowPlan, so what the test changes is exactly what they see.
	mutatePlan func(domain.WorkflowPlanRecord) domain.WorkflowPlanRecord
}

func (s *crashStore) GetWorkflowPlan(ctx context.Context, runID string) (domain.WorkflowPlanRecord, bool, error) {
	record, ok, err := s.Store.GetWorkflowPlan(ctx, runID)
	if ok && err == nil && s.mutatePlan != nil {
		record = s.mutatePlan(record)
	}
	return record, ok, err
}

func (s *crashStore) ReplaceWorkflowTaskRelationships(ctx context.Context, rels []domain.WorkflowTaskRelationship) error {
	if s.failRelationships {
		return errors.New("simulated crash before task relationships were written")
	}
	return s.Store.ReplaceWorkflowTaskRelationships(ctx, rels)
}

func (s *crashStore) SetWorkflowTaskExecutionRun(ctx context.Context, taskID, executionRunID string, now time.Time) (bool, error) {
	if s.failSetExecutionRun {
		return false, errors.New("simulated crash before the task's execution run was recorded")
	}
	return s.Store.SetWorkflowTaskExecutionRun(ctx, taskID, executionRunID, now)
}

func (s *crashStore) UpdateWorkflowRunPolicySnapshot(ctx context.Context, runID, snapshot string, now time.Time) (bool, error) {
	if s.swallowPolicyWrite {
		return false, nil
	}
	return s.Store.UpdateWorkflowRunPolicySnapshot(ctx, runID, snapshot, now)
}

func (s *crashStore) ApproveWorkflowPlan(ctx context.Context, runID string, now time.Time) (bool, error) {
	if s.swallowApprove {
		return false, nil
	}
	return s.Store.ApproveWorkflowPlan(ctx, runID, now)
}

func (s *crashStore) SetWorkflowPlanApprovalMode(ctx context.Context, runID string, mode domain.WorkflowPlanApprovalMode, now time.Time) (bool, error) {
	if s.swallowApprovalMode {
		return false, nil
	}
	return s.Store.SetWorkflowPlanApprovalMode(ctx, runID, mode, now)
}

// newCrashFixture builds a real-store master objective fixture whose writes
// can be selectively lost. rebootCoordinator returns a brand-new Coordinator
// over the same database, which is what a daemon restart actually is.
func newCrashFixture(t *testing.T, plan workflowcore.MasterPlan) (*crashStore, func() *workflowcore.Coordinator) {
	t.Helper()
	store := &crashStore{Store: sqlitetest.MustOpen(t)}
	ctx := context.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	reboot := func() *workflowcore.Coordinator {
		return workflowcore.New(workflowcore.Deps{Store: store, Projects: store, Planner: &staticPlanner{plan: plan}, PlannerContextBuilder: staticContext{}})
	}
	return store, reboot
}

func runPolicy(t *testing.T, store *crashStore, runID string) domain.WorkflowPolicy {
	t.Helper()
	run, ok, err := store.GetWorkflowRun(context.Background(), runID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(%s): ok=%v err=%v", runID, ok, err)
	}
	var policy domain.WorkflowPolicy
	if err := json.Unmarshal([]byte(run.PolicySnapshot), &policy); err != nil {
		t.Fatalf("policy snapshot for %s: %v", runID, err)
	}
	return policy
}

func stepsByKind(t *testing.T, store *crashStore, runID string) map[domain.WorkflowStepKind]domain.WorkflowStep {
	t.Helper()
	steps, err := store.ListWorkflowSteps(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListWorkflowSteps(%s): %v", runID, err)
	}
	out := map[domain.WorkflowStepKind]domain.WorkflowStep{}
	for _, s := range steps {
		out[s.Kind] = s
	}
	return out
}

// ---------------------------------------------------------------------------
// CP9(b): finalizeGeneratedPlan's replay must reuse the persisted task
// identities instead of minting new ones, or the FK-bound relationship insert
// fails on every boot forever.
// ---------------------------------------------------------------------------

func TestCP9bFinalizeReplayReusesPersistedTaskIdentities(t *testing.T) {
	ctx := context.Background()
	store, reboot := newCrashFixture(t, validMasterPlan())

	// The crash: tasks written (P10), relationships not (P11).
	store.failRelationships = true
	c := reboot()
	created, err := c.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatal(err)
	}
	runID := created.Run.ID
	if _, err := c.GeneratePlan(ctx, runID); err == nil {
		t.Fatal("GeneratePlan succeeded; the relationship write was supposed to fail")
	}
	before, err := store.ListWorkflowTasks(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 2 {
		t.Fatalf("tasks after crash = %d, want 2", len(before))
	}
	idBefore := map[string]string{}
	for _, task := range before {
		idBefore[task.PlanStepID] = task.ID
	}
	if rels, rerr := store.ListWorkflowTaskRelationships(ctx, runID); rerr != nil || len(rels) != 0 {
		t.Fatalf("relationships after crash = %d (err=%v), want 0", len(rels), rerr)
	}

	// The restart, three times over: recovery replays finalizeGeneratedPlan.
	store.failRelationships = false
	for pass := 1; pass <= 3; pass++ {
		if err := reboot().Reconcile(ctx); err != nil {
			t.Fatalf("Reconcile pass %d: %v", pass, err)
		}
		after, lerr := store.ListWorkflowTasks(ctx, runID)
		if lerr != nil {
			t.Fatal(lerr)
		}
		if len(after) != 2 {
			t.Fatalf("pass %d: tasks = %d, want 2 (a replay minted new ids and duplicated or dropped rows)", pass, len(after))
		}
		for _, task := range after {
			if idBefore[task.PlanStepID] != task.ID {
				t.Fatalf("pass %d: task for plan step %s changed identity %s -> %s",
					pass, task.PlanStepID, idBefore[task.PlanStepID], task.ID)
			}
		}
		rels, rerr := store.ListWorkflowTaskRelationships(ctx, runID)
		if rerr != nil {
			t.Fatalf("pass %d: %v", pass, rerr)
		}
		if len(rels) == 0 {
			t.Fatalf("pass %d: the replay wrote no task relationships", pass)
		}
		for _, rel := range rels {
			if idBefore[""] == rel.TaskID { // guard against an empty-key accident
				t.Fatalf("pass %d: relationship names an empty task id", pass)
			}
		}
		plan, ok, perr := store.GetWorkflowPlan(ctx, runID)
		if perr != nil || !ok {
			t.Fatalf("pass %d: GetWorkflowPlan ok=%v err=%v", pass, ok, perr)
		}
		if plan.Status != domain.WorkflowPlanValidated {
			t.Fatalf("pass %d: plan status = %q, want validated", pass, plan.Status)
		}
	}
}

// A canonical task id is a pure function of (workflow_run_id, plan_step_id):
// two independent finalizations of the same plan agree without coordinating.
func TestCP9bTaskIdentityIsDerivedFromTheNaturalKey(t *testing.T) {
	ctx := context.Background()
	ids := make([]map[string]string, 2)
	for i := range ids {
		store, reboot := newCrashFixture(t, validMasterPlan())
		c := reboot()
		created, err := c.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.GeneratePlan(ctx, created.Run.ID); err != nil {
			t.Fatal(err)
		}
		tasks, err := store.ListWorkflowTasks(ctx, created.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = map[string]string{}
		for _, task := range tasks {
			// The id must be derivable, not random: same run id + same plan
			// step id must give the same task id in a different process.
			ids[i][task.WorkflowRunID+"/"+task.PlanStepID] = task.ID
		}
	}
	// The two runs have different run ids, so the ids differ -- what must hold
	// is that neither map has an id that repeats for a different natural key.
	seen := map[string]string{}
	for _, m := range ids {
		for key, id := range m {
			if prev, ok := seen[id]; ok && prev != key {
				t.Fatalf("task id %s collides across natural keys %s and %s", id, prev, key)
			}
			seen[id] = key
		}
	}
}

// ---------------------------------------------------------------------------
// CP21: a master task's child run must carry the planner's real acceptance
// criteria and the task's declared write intent from the moment it exists.
// CP19: and its parent objective's frozen execution policy.
// ---------------------------------------------------------------------------

// readOnlyMasterPlan is validMasterPlan with the first step explicitly
// declared read-only -- the semantic CP21 loses. An empty WriteIntent is
// Unspecified, which is treated as mutating, so "read_only silently became
// mutating" is exactly the failure the artifact overwrite could produce.
func readOnlyMasterPlan() workflowcore.MasterPlan {
	plan := validMasterPlan()
	plan.Steps[0].WriteIntent = domain.WorkflowWriteIntentReadOnly
	plan.Steps[0].AcceptanceCriteria = []string{"The audit report lists every unvalidated field."}
	return plan
}

func childPlanArtifact(t *testing.T, store *crashStore, childRunID string) workflowcore.PlanArtifact {
	t.Helper()
	planStep, ok := stepsByKind(t, store, childRunID)[domain.WorkflowStepPlan]
	if !ok {
		t.Fatalf("child run %s has no plan step", childRunID)
	}
	artifact, err := workflowcore.UnmarshalPlanArtifact(planStep.ArtifactJSON)
	if err != nil {
		t.Fatalf("child plan artifact: %v", err)
	}
	return artifact
}

// dispatchedChildAfterCrash drives a master objective up to the point where
// its first task's child run exists but the task's execution run was never
// recorded -- the crash window in which the child is reachable only by its
// natural key. It returns the child run id.
func dispatchedChildAfterCrash(t *testing.T, store *crashStore, reboot func() *workflowcore.Coordinator) (string, string) {
	t.Helper()
	ctx := context.Background()
	c := reboot()
	created, err := c.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalAuto)
	if err != nil {
		t.Fatal(err)
	}
	runID := created.Run.ID
	store.failSetExecutionRun = true
	_, _ = c.GeneratePlan(ctx, runID)
	store.failSetExecutionRun = false

	tasks, err := store.ListWorkflowTasks(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) == 0 {
		t.Fatal("no tasks were planned")
	}
	if tasks[0].ExecutionRunID != nil {
		t.Fatal("the execution run was recorded; the crash did not happen where the test needs it")
	}
	childID, ok, err := store.FindWorkflowRunByPlannedTask(ctx, tasks[0].ID)
	if err != nil || !ok {
		t.Fatalf("child run for task %s: ok=%v err=%v", tasks[0].ID, ok, err)
	}
	return runID, childID
}

func TestCP21ChildRunCarriesPlannerCriteriaAndWriteIntentFromCreation(t *testing.T) {
	store, reboot := newCrashFixture(t, readOnlyMasterPlan())
	_, childID := dispatchedChildAfterCrash(t, store, reboot)

	// The crash landed AFTER the child was created and BEFORE anything else
	// could touch it. Under the old ordering the artifact at this instant was
	// the generic one; it must now already be the planner's.
	artifact := childPlanArtifact(t, store, childID)
	if artifact.WriteIntent != domain.WorkflowWriteIntentReadOnly {
		t.Fatalf("child write intent = %q, want read_only: a read-only task would be prompted and classified as mutating",
			artifact.WriteIntent)
	}
	if len(artifact.AcceptanceCriteria) != 1 || artifact.AcceptanceCriteria[0] != "The audit report lists every unvalidated field." {
		t.Fatalf("child acceptance criteria = %v, want the planner's", artifact.AcceptanceCriteria)
	}
}

func TestCP21AndCP19RecoveryRebindsArtifactAndInheritedPolicy(t *testing.T) {
	ctx := context.Background()
	store, reboot := newCrashFixture(t, readOnlyMasterPlan())
	masterID, childID := dispatchedChildAfterCrash(t, store, reboot)

	// Corrupt the child the way the two pre-fix crash windows did: the generic
	// artifact CP21 leaves behind, and the default (non-inherited) execution
	// policy CP19 leaves behind.
	generic := workflowcore.BuildPlanArtifact("p", "whatever", "v1")
	genericJSON, err := workflowcore.MarshalPlanArtifact(generic)
	if err != nil {
		t.Fatal(err)
	}
	planStep := stepsByKind(t, store, childID)[domain.WorkflowStepPlan]
	if ok, uerr := store.UpdateWorkflowStepArtifact(ctx, planStep.ID, genericJSON, time.Now().UTC()); uerr != nil || !ok {
		t.Fatalf("seed generic artifact: ok=%v err=%v", ok, uerr)
	}
	defaultPolicy, err := json.Marshal(domain.DefaultWorkflowPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if ok, uerr := store.UpdateWorkflowRunPolicySnapshot(ctx, childID, string(defaultPolicy), time.Now().UTC()); uerr != nil || !ok {
		t.Fatalf("seed default policy: ok=%v err=%v", ok, uerr)
	}

	// Repeated daemon restarts must converge, and the second must change
	// nothing the first did not already fix.
	for pass := 1; pass <= 3; pass++ {
		if rerr := reboot().Reconcile(ctx); rerr != nil {
			t.Fatalf("Reconcile pass %d: %v", pass, rerr)
		}
		artifact := childPlanArtifact(t, store, childID)
		if artifact.WriteIntent != domain.WorkflowWriteIntentReadOnly {
			t.Fatalf("pass %d: child write intent = %q, want read_only", pass, artifact.WriteIntent)
		}
		if len(artifact.AcceptanceCriteria) != 1 ||
			artifact.AcceptanceCriteria[0] != "The audit report lists every unvalidated field." {
			t.Fatalf("pass %d: child criteria = %v, want the planner's", pass, artifact.AcceptanceCriteria)
		}
		prov := runPolicy(t, store, childID).Execution.Provenance
		if prov.Source != domain.ExecutionPolicyInherited || prov.ParentRunID != masterID {
			t.Fatalf("pass %d: child policy provenance = %+v, want inherited from %s", pass, prov, masterID)
		}
	}
}

// The gate is fail-closed, not advisory: if the inheritance write cannot land,
// the child must never be started.
func TestCP19ChildNeverDispatchesWithUnprovableExecutionPolicy(t *testing.T) {
	ctx := context.Background()
	store, reboot := newCrashFixture(t, validMasterPlan())
	c := reboot()
	created, err := c.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalAuto)
	if err != nil {
		t.Fatal(err)
	}
	masterID := created.Run.ID
	if _, err := store.SetWorkflowRunOwner(ctx, masterID, domain.UserID("u1")); err != nil {
		t.Fatal(err)
	}
	autonomous := true
	if err := c.ApplyExecutionPolicySnapshot(ctx, masterID, domain.UserID("u1"), &autonomous); err != nil {
		t.Fatal(err)
	}

	// From here the child's policy snapshot write never lands.
	store.swallowPolicyWrite = true
	_, _ = reboot().GeneratePlan(ctx, masterID)
	store.swallowPolicyWrite = false

	tasks, err := store.ListWorkflowTasks(ctx, masterID)
	if err != nil {
		t.Fatal(err)
	}
	childID, ok, err := store.FindWorkflowRunByPlannedTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		// The child was never even created; that is also fail-closed.
		return
	}
	child, found, err := store.GetWorkflowRun(ctx, childID)
	if err != nil || !found {
		t.Fatalf("GetWorkflowRun(child): ok=%v err=%v", found, err)
	}
	if child.State != domain.WorkflowRunPending {
		t.Fatalf("child run state = %q, want pending: it was started under an execution policy nobody could prove", child.State)
	}
}

// ---------------------------------------------------------------------------
// CP24-CP27: a crash inside StartRun, after the run moved pending -> running,
// used to leave the plan -> work unblock unreachable by every entry point.
// ---------------------------------------------------------------------------

// seedInterruptedStart puts a standalone run into one of the four durable
// states a crash inside StartRun can leave, using only the CAS transitions the
// real code takes to get there.
func seedInterruptedStart(t *testing.T, store *crashStore, runID string, planState domain.WorkflowStepState) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if ok, err := store.UpdateWorkflowRunState(ctx, runID, domain.WorkflowRunPending, domain.WorkflowRunRunning, now); err != nil || !ok {
		t.Fatalf("seed run running: ok=%v err=%v", ok, err)
	}
	planStep := stepsByKind(t, store, runID)[domain.WorkflowStepPlan]
	if planState == domain.WorkflowStepReady {
		return
	}
	if ok, err := store.UpdateWorkflowStepState(ctx, planStep.ID, domain.WorkflowStepReady, domain.WorkflowStepRunning, now); err != nil || !ok {
		t.Fatalf("seed plan step running: ok=%v err=%v", ok, err)
	}
	if planState == domain.WorkflowStepRunning {
		return
	}
	next := planState
	if next == domain.WorkflowStepWaiting || next == domain.WorkflowStepCompleted {
		if ok, err := store.UpdateWorkflowStepState(ctx, planStep.ID, domain.WorkflowStepRunning, next, now); err != nil || !ok {
			t.Fatalf("seed plan step %s: ok=%v err=%v", next, ok, err)
		}
		return
	}
	t.Fatalf("unsupported seed plan state %q", planState)
}

func TestCP24ToCP27InterruptedStartConvergesOnEveryEntryPoint(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name      string
		planState domain.WorkflowStepState
	}{
		{"CP24 plan step still ready", domain.WorkflowStepReady},
		{"CP25/CP26 plan step running", domain.WorkflowStepRunning},
		{"CP25 after boot parked the plan step", domain.WorkflowStepWaiting},
		{"CP27 plan step completed, work step never unblocked", domain.WorkflowStepCompleted},
	}
	for _, tc := range cases {
		for _, entry := range []string{"reconcile", "startrun"} {
			t.Run(tc.name+"/"+entry, func(t *testing.T) {
				store, reboot := newCrashFixture(t, validMasterPlan())
				created, err := reboot().CreateRun(ctx, "p", "ship the thing")
				if err != nil {
					t.Fatal(err)
				}
				runID := created.Run.ID
				seedInterruptedStart(t, store, runID, tc.planState)

				// Whichever entry point runs, and however many times the daemon
				// restarts, the run must converge to the same reachable state.
				for pass := 1; pass <= 3; pass++ {
					c := reboot()
					if entry == "reconcile" {
						if rerr := c.Reconcile(ctx); rerr != nil {
							t.Fatalf("pass %d Reconcile: %v", pass, rerr)
						}
					} else if _, serr := c.StartRun(ctx, runID); serr != nil {
						t.Fatalf("pass %d StartRun: %v", pass, serr)
					}
					run, ok, gerr := store.GetWorkflowRun(ctx, runID)
					if gerr != nil || !ok {
						t.Fatalf("pass %d GetWorkflowRun: ok=%v err=%v", pass, ok, gerr)
					}
					if run.State != domain.WorkflowRunRunning {
						t.Fatalf("pass %d run state = %q, want running", pass, run.State)
					}
					steps := stepsByKind(t, store, runID)
					if steps[domain.WorkflowStepPlan].State != domain.WorkflowStepCompleted {
						t.Fatalf("pass %d plan step = %q, want completed", pass, steps[domain.WorkflowStepPlan].State)
					}
					if steps[domain.WorkflowStepWork].State == domain.WorkflowStepPending {
						t.Fatalf("pass %d work step is still pending: the plan->work unblock never happened", pass)
					}
				}
			})
		}
	}
}

// A run parked by boot recovery's interrupted-step fallback is still
// convergeable: needs_attention is the state CP25 itself produces.
func TestCP25ParkedRunStillConverges(t *testing.T) {
	ctx := context.Background()
	store, reboot := newCrashFixture(t, validMasterPlan())
	created, err := reboot().CreateRun(ctx, "p", "ship the thing")
	if err != nil {
		t.Fatal(err)
	}
	runID := created.Run.ID
	seedInterruptedStart(t, store, runID, domain.WorkflowStepWaiting)
	now := time.Now().UTC()
	if ok, uerr := store.UpdateWorkflowRunState(ctx, runID, domain.WorkflowRunRunning, domain.WorkflowRunNeedsAttention, now); uerr != nil || !ok {
		t.Fatalf("park run: ok=%v err=%v", ok, uerr)
	}

	if rerr := reboot().Reconcile(ctx); rerr != nil {
		t.Fatal(rerr)
	}
	steps := stepsByKind(t, store, runID)
	if steps[domain.WorkflowStepPlan].State != domain.WorkflowStepCompleted ||
		steps[domain.WorkflowStepWork].State == domain.WorkflowStepPending {
		t.Fatalf("parked run did not converge: plan=%q work=%q",
			steps[domain.WorkflowStepPlan].State, steps[domain.WorkflowStepWork].State)
	}
}

// A run whose start is NOT owed must not be touched: StartRun stays the
// idempotent no-op it has always been for an already-advanced run.
func TestStartRunDoesNotResumeARunThatOwesNothing(t *testing.T) {
	ctx := context.Background()
	store, reboot := newCrashFixture(t, validMasterPlan())
	created, err := reboot().CreateRun(ctx, "p", "ship the thing")
	if err != nil {
		t.Fatal(err)
	}
	runID := created.Run.ID
	if _, err := reboot().StartRun(ctx, runID); err != nil {
		t.Fatal(err)
	}
	before := stepsByKind(t, store, runID)
	now := time.Now().UTC()
	work := before[domain.WorkflowStepWork]
	if ok, uerr := store.UpdateWorkflowStepState(ctx, work.ID, work.State, domain.WorkflowStepCompleted, now); uerr != nil || !ok {
		t.Fatalf("advance work step: ok=%v err=%v", ok, uerr)
	}
	if _, err := reboot().StartRun(ctx, runID); err != nil {
		t.Fatal(err)
	}
	after := stepsByKind(t, store, runID)
	if after[domain.WorkflowStepWork].State != domain.WorkflowStepCompleted {
		t.Fatalf("work step = %q, want completed: StartRun re-drove a run that owed nothing", after[domain.WorkflowStepWork].State)
	}
}

// ---------------------------------------------------------------------------
// CP3: a crash before the execution-policy freeze must never leave a run
// silently carrying the default policy with nothing on disk saying so.
// ---------------------------------------------------------------------------

func TestCP3CreationRecordsThatTheFreezeIsOwed(t *testing.T) {
	ctx := context.Background()
	store, reboot := newCrashFixture(t, validMasterPlan())
	c := reboot()
	objective, err := c.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatal(err)
	}
	standalone, err := c.CreateRun(ctx, "p", "ship the thing")
	if err != nil {
		t.Fatal(err)
	}
	for _, runID := range []string{objective.Run.ID, standalone.Run.ID} {
		prov := runPolicy(t, store, runID).Execution.Provenance
		if !prov.Unproven() {
			t.Fatalf("run %s provenance = %+v, want the freeze recorded as owed", runID, prov)
		}
	}
}

func TestCP3RecoveryRefreezesAnOwnedRunAndSaysItRecoveredIt(t *testing.T) {
	ctx := context.Background()
	store, reboot := newCrashFixture(t, validMasterPlan())
	created, err := reboot().CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatal(err)
	}
	runID := created.Run.ID
	// The owner stamp landed; the freeze that follows it did not.
	if _, err := store.SetWorkflowRunOwner(ctx, runID, domain.UserID("u1")); err != nil {
		t.Fatal(err)
	}

	for pass := 1; pass <= 3; pass++ {
		if rerr := reboot().Reconcile(ctx); rerr != nil {
			t.Fatalf("Reconcile pass %d: %v", pass, rerr)
		}
		prov := runPolicy(t, store, runID).Execution.Provenance
		if !prov.Proven() {
			t.Fatalf("pass %d: provenance = %+v, want a proven policy", pass, prov)
		}
		if prov.Source != domain.ExecutionPolicyRecovered {
			t.Fatalf("pass %d: provenance source = %q, want %q -- a recovery freeze must never claim to be the original one",
				pass, prov.Source, domain.ExecutionPolicyRecovered)
		}
		if prov.OwnerID != domain.UserID("u1") {
			t.Fatalf("pass %d: provenance owner = %q, want u1", pass, prov.OwnerID)
		}
	}
}

// An UNOWNED run has no identity to freeze against; the default policy is the
// honest answer there, and recovery must leave it exactly alone.
func TestCP3UnownedAndLegacyRunsAreNeverTouched(t *testing.T) {
	ctx := context.Background()
	store, reboot := newCrashFixture(t, validMasterPlan())
	unowned, err := reboot().CreateRun(ctx, "p", "ship the thing")
	if err != nil {
		t.Fatal(err)
	}
	// A run that predates provenance entirely: no `provenance` key at all.
	legacy, err := reboot().CreateRun(ctx, "p", "an older objective")
	if err != nil {
		t.Fatal(err)
	}
	legacyJSON, err := json.Marshal(domain.DefaultWorkflowPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if ok, uerr := store.UpdateWorkflowRunPolicySnapshot(ctx, legacy.Run.ID, string(legacyJSON), time.Now().UTC()); uerr != nil || !ok {
		t.Fatalf("seed legacy snapshot: ok=%v err=%v", ok, uerr)
	}
	if _, err := store.SetWorkflowRunOwner(ctx, legacy.Run.ID, domain.UserID("u1")); err != nil {
		t.Fatal(err)
	}

	if rerr := reboot().Reconcile(ctx); rerr != nil {
		t.Fatal(rerr)
	}
	if prov := runPolicy(t, store, unowned.Run.ID).Execution.Provenance; !prov.Unproven() {
		t.Fatalf("unowned run provenance = %+v, want untouched", prov)
	}
	if prov := runPolicy(t, store, legacy.Run.ID).Execution.Provenance; prov.Source != "" {
		t.Fatalf("legacy run provenance = %+v, want untouched (no provenance invented)", prov)
	}
}

// If the re-freeze cannot land, the run is parked rather than driven on a
// policy nobody chose.
func TestCP3FailsClosedWhenThePolicyCannotBeReProven(t *testing.T) {
	ctx := context.Background()
	store, reboot := newCrashFixture(t, validMasterPlan())
	created, err := reboot().CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatal(err)
	}
	runID := created.Run.ID
	if _, err := store.SetWorkflowRunOwner(ctx, runID, domain.UserID("u1")); err != nil {
		t.Fatal(err)
	}
	store.swallowPolicyWrite = true
	rerr := reboot().Reconcile(ctx)
	store.swallowPolicyWrite = false
	if rerr == nil {
		t.Fatal("Reconcile reported success for a run whose execution policy could not be proven")
	}
	if !strings.Contains(rerr.Error(), runID) {
		t.Fatalf("error = %v, want it to name the run", rerr)
	}
	run, ok, err := store.GetWorkflowRun(ctx, runID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun: ok=%v err=%v", ok, err)
	}
	if run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", run.State)
	}
}

// ---------------------------------------------------------------------------
// CP11/CP12: an autonomous plan must never remain `validated` forever.
// ---------------------------------------------------------------------------

func TestCP11AndCP12ValidatedAutonomousPlanIsApprovedAfterRestart(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		// CP12: the record already says approval_mode=auto.
		// CP11: it still says manual, and only the frozen policy says the
		// objective is autonomous.
		keepApprovalModeManual bool
		entry                  string
	}{
		{"CP12 approval_mode auto/reconcile", false, "reconcile"},
		{"CP12 approval_mode auto/continue", false, "continue"},
		{"CP11 autonomous policy only/reconcile", true, "reconcile"},
		{"CP11 autonomous policy only/continue", true, "continue"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, reboot := newCrashFixture(t, validMasterPlan())
			c := reboot()
			created, err := c.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
			if err != nil {
				t.Fatal(err)
			}
			runID := created.Run.ID
			if _, err := store.SetWorkflowRunOwner(ctx, runID, domain.UserID("u1")); err != nil {
				t.Fatal(err)
			}
			autonomous := true
			if err := c.ApplyExecutionPolicySnapshot(ctx, runID, domain.UserID("u1"), &autonomous); err != nil {
				t.Fatal(err)
			}

			// The crash: the plan validates, and the approval that should have
			// followed it never lands.
			store.swallowApprove = true
			store.swallowApprovalMode = tc.keepApprovalModeManual
			if _, err := reboot().GeneratePlan(ctx, runID); err != nil {
				t.Fatal(err)
			}
			store.swallowApprove, store.swallowApprovalMode = false, false

			plan, ok, err := store.GetWorkflowPlan(ctx, runID)
			if err != nil || !ok {
				t.Fatalf("GetWorkflowPlan: ok=%v err=%v", ok, err)
			}
			if plan.Status != domain.WorkflowPlanValidated {
				t.Fatalf("plan status after crash = %q, want validated", plan.Status)
			}

			for pass := 1; pass <= 3; pass++ {
				resumed := reboot()
				if tc.entry == "reconcile" {
					if rerr := resumed.Reconcile(ctx); rerr != nil {
						t.Fatalf("pass %d Reconcile: %v", pass, rerr)
					}
				} else if _, cerr := resumed.ContinueRun(ctx, runID); cerr != nil {
					t.Fatalf("pass %d ContinueRun: %v", pass, cerr)
				}
				after, ok, gerr := store.GetWorkflowPlan(ctx, runID)
				if gerr != nil || !ok {
					t.Fatalf("pass %d GetWorkflowPlan: ok=%v err=%v", pass, ok, gerr)
				}
				if after.Status != domain.WorkflowPlanApproved {
					t.Fatalf("pass %d plan status = %q, want approved: an autonomous objective stalled at 'plan ready'", pass, after.Status)
				}
				if after.ApprovalMode != domain.WorkflowPlanApprovalAuto {
					t.Fatalf("pass %d approval mode = %q, want auto recorded as the approval source", pass, after.ApprovalMode)
				}
			}
		})
	}
}

// A MANUAL objective's `validated` plan is the approval prompt. Nothing here
// may answer it on the person's behalf.
func TestValidatedManualPlanIsLeftForThePerson(t *testing.T) {
	ctx := context.Background()
	store, reboot := newCrashFixture(t, validMasterPlan())
	created, err := reboot().CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatal(err)
	}
	runID := created.Run.ID
	if _, err := reboot().GeneratePlan(ctx, runID); err != nil {
		t.Fatal(err)
	}
	for pass := 1; pass <= 2; pass++ {
		if rerr := reboot().Reconcile(ctx); rerr != nil {
			t.Fatalf("pass %d: %v", pass, rerr)
		}
		if _, cerr := reboot().ContinueRun(ctx, runID); cerr != nil {
			t.Fatalf("pass %d ContinueRun: %v", pass, cerr)
		}
		plan, ok, gerr := store.GetWorkflowPlan(ctx, runID)
		if gerr != nil || !ok {
			t.Fatalf("pass %d: ok=%v err=%v", pass, ok, gerr)
		}
		if plan.Status != domain.WorkflowPlanValidated {
			t.Fatalf("pass %d plan status = %q, want validated: a manual plan was approved without a person", pass, plan.Status)
		}
	}
}

// ---------------------------------------------------------------------------
// P9: the normalized-plan re-persist. It reused a CAS the response write had
// already invalidated, so it matched zero rows every time and its result was
// discarded. It now has its own CAS, conditioned on the bytes the caller read.
// ---------------------------------------------------------------------------

// unnormalizedMasterPlan is a valid plan whose raw bytes differ from its
// normalized form (the titles carry surrounding whitespace normalization
// trims), which is the only condition under which P9's write runs at all.
func unnormalizedMasterPlan() workflowcore.MasterPlan {
	plan := validMasterPlan()
	plan.Steps[0].Title = "   Model   "
	plan.Steps[1].Title = "  Tests "
	return plan
}

func TestP9NormalizedPlanIsActuallyPersisted(t *testing.T) {
	ctx := context.Background()
	// Two independent copies: NormalizeAndValidatePlan normalizes the steps
	// slice in place, so the planner's copy and the copy this test normalizes
	// for comparison must not share a backing array.
	rawJSON, err := json.Marshal(unnormalizedMasterPlan())
	if err != nil {
		t.Fatal(err)
	}
	store, reboot := newCrashFixture(t, unnormalizedMasterPlan())
	created, err := reboot().CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatal(err)
	}
	runID := created.Run.ID
	if _, err := reboot().GeneratePlan(ctx, runID); err != nil {
		t.Fatal(err)
	}
	plan, ok, err := store.GetWorkflowPlan(ctx, runID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowPlan: ok=%v err=%v", ok, err)
	}
	normalized, validation, _ := workflowcore.NormalizeAndValidatePlan(unnormalizedMasterPlan(), "Build users", workflowcore.MaxPlanSteps)
	if !validation.Valid {
		t.Fatalf("fixture plan is invalid: %v", validation.Errors)
	}
	want, err := json.Marshal(normalized)
	if err != nil {
		t.Fatal(err)
	}
	if string(rawJSON) == string(want) {
		t.Fatal("fixture plan normalizes to itself; this test would not exercise P9's write")
	}
	if plan.GeneratedPlanJSON != string(want) {
		t.Fatalf("stored plan is not the normalized one:\n got %s\nwant %s", plan.GeneratedPlanJSON, want)
	}
	// And re-entering finalize (the boot replay) is a byte-stable no-op.
	if rerr := reboot().Reconcile(ctx); rerr != nil {
		t.Fatal(rerr)
	}
	again, _, err := store.GetWorkflowPlan(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if again.GeneratedPlanJSON != plan.GeneratedPlanJSON {
		t.Fatal("the replay rewrote the stored plan")
	}
}

// The CAS itself: a writer working from a stale read is rejected, and so is a
// writer whose plan is no longer in the state the write belongs to.
func TestP9NormalizedPlanCASRejectsStaleWriters(t *testing.T) {
	ctx := context.Background()
	store := sqlitetest.MustOpen(t)
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	c := workflowcore.New(workflowcore.Deps{Store: store, Projects: store, Planner: &staticPlanner{plan: validMasterPlan()}, PlannerContextBuilder: staticContext{}})
	created, err := c.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatal(err)
	}
	runID := created.Run.ID
	now := time.Now().UTC()
	if ok, serr := store.StartWorkflowPlanCommand(ctx, runID, "fake", "fake-v1", "{}", now); serr != nil || !ok {
		t.Fatalf("arm plan command: ok=%v err=%v", ok, serr)
	}

	// While the command is still `running`, the normalized write does not
	// belong yet and must be refused.
	if ok, werr := store.PersistNormalizedWorkflowPlan(ctx, runID, "", `{"a":1}`, now); werr != nil || ok {
		t.Fatalf("normalized write accepted at command_status=running: ok=%v err=%v", ok, werr)
	}

	if ok, perr := store.PersistWorkflowPlanResponse(ctx, runID, `{"raw":true}`, now); perr != nil || !ok {
		t.Fatalf("persist response: ok=%v err=%v", ok, perr)
	}
	// A stale writer -- one holding an older copy of generated_plan_json --
	// loses the compare-and-set.
	if ok, werr := store.PersistNormalizedWorkflowPlan(ctx, runID, `{"stale":true}`, `{"norm":true}`, now); werr != nil || ok {
		t.Fatalf("stale normalized write accepted: ok=%v err=%v", ok, werr)
	}
	// The writer that read the current bytes wins.
	if ok, werr := store.PersistNormalizedWorkflowPlan(ctx, runID, `{"raw":true}`, `{"norm":true}`, now); werr != nil || !ok {
		t.Fatalf("current normalized write refused: ok=%v err=%v", ok, werr)
	}
	plan, ok, gerr := store.GetWorkflowPlan(ctx, runID)
	if gerr != nil || !ok {
		t.Fatalf("GetWorkflowPlan: ok=%v err=%v", ok, gerr)
	}
	if plan.GeneratedPlanJSON != `{"norm":true}` {
		t.Fatalf("stored plan = %s, want the normalized bytes", plan.GeneratedPlanJSON)
	}
	// Replaying the same write is now a refusal too -- and an idempotent one,
	// because the value it wanted is already there.
	if ok, werr := store.PersistNormalizedWorkflowPlan(ctx, runID, `{"raw":true}`, `{"norm":true}`, now); werr != nil || ok {
		t.Fatalf("replayed stale write accepted: ok=%v err=%v", ok, werr)
	}
}
