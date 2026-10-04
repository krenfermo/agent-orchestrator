package workflow

// The DAG half of task-level attention: what a parked task does to the rest of
// the plan, and what it does NOT do.
//
// The reviewer's first finding was that an integration conflict stopped the
// whole objective. That is the opposite of what parallel execution is for: a
// merge problem in task A says nothing about task B, and the only tasks that
// should wait are the ones that depend on A. These tests fix all three of those
// in one plan — A conflicts, B is independent and lands, C depends on A and
// waits — and then check the one case where the objective genuinely must stop:
// when nothing is left that can move.

import (
	stdctx "context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/integration"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// dagFixture is one master run with a three-task plan, in direct-branch mode
// against a real repository.
type dagFixture struct {
	coord  *Coordinator
	store  *sqlite.Store
	ctx    stdctx.Context
	master domain.WorkflowRun
	head   string
}

// newTaskDAGFixture builds A (conflicts), B (integrates) and C (depends on A).
//
// A's and B's evidence differ in kind on purpose, because that is what lets one
// observation of one branch be fresh for one task and stale for the other: B
// committed its verified result and the branch still points at that commit,
// while A had nothing to commit and the workspace no longer fingerprints as the
// state its verification passed on.
func newTaskDAGFixture(t *testing.T) *dagFixture {
	t.Helper()
	facts := &directBranchFacts{}
	coord, store, ctx := newDirectBranchCoordinator(t, facts)
	head := dbCommit(t, "one")
	facts.obs = dbObservation(head)

	now := time.Now().UTC()
	master := domain.WorkflowRun{
		ID: "wf-dag", ProjectID: "p", Objective: "three tasks", State: domain.WorkflowRunRunning,
		PolicyVersion: policyVersionV1, PolicySnapshot: "{}", CreatedAt: now, UpdatedAt: now,
	}
	if _, _, err := store.CreateWorkflowRun(ctx, master, nil); err != nil {
		t.Fatal(err)
	}
	seedPlanTask(t, ctx, store, master.ID, "task-a", 1, domain.WorkflowTaskRunning, nil)
	seedPlanTask(t, ctx, store, master.ID, "task-b", 2, domain.WorkflowTaskRunning, nil)
	seedPlanTask(t, ctx, store, master.ID, "task-c", 3, domain.WorkflowTaskBlocked, []string{"task-a"})

	// A: verified against a workspace state that is gone. Its precondition
	// cannot hold, so its integration is a conflict.
	a := seedDagChild(t, ctx, store, master.ID, "task-a", "")
	seedVerifyPassed(t, ctx, store, a, "p", "a-fingerprint-from-a-state-that-is-gone")
	// B: committed its verified result, and the branch still points at it.
	seedDagChild(t, ctx, store, master.ID, "task-b", head)

	return &dagFixture{coord: coord, store: store, ctx: ctx, master: master, head: head}
}

// seedDagChild creates one task's completed child run, with the durable facts
// promotion reads: a work checkpoint naming the repository and branch, and —
// when commitSHA is set — the autonomous local commit that proves the verified
// result reached the branch.
func seedDagChild(t *testing.T, ctx stdctx.Context, store *sqlite.Store, masterID, taskID, commitSHA string) RunDetail {
	t.Helper()
	return seedDagChildUnder(t, ctx, store, masterID, taskID, commitSHA, "")
}

// seedDagChildUnder is seedDagChild plus an optional plan artifact, for the
// tests that need the child's plan to declare a verification.
func seedDagChildUnder(t *testing.T, ctx stdctx.Context, store *sqlite.Store, masterID, taskID, commitSHA, planArtifact string) RunDetail {
	t.Helper()
	now := time.Now().UTC()
	sess, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessCodex, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	childID := "wf-exec-" + taskID
	steps := []domain.WorkflowStep{}
	if planArtifact != "" {
		steps = append(steps, domain.WorkflowStep{ID: "wfs-plan-" + taskID, WorkflowRunID: childID,
			Kind: domain.WorkflowStepPlan, Ordinal: 0, State: domain.WorkflowStepCompleted,
			ArtifactJSON: planArtifact, CreatedAt: now, UpdatedAt: now})
	}
	steps = append(steps, domain.WorkflowStep{ID: "wfs-" + taskID, WorkflowRunID: childID, Kind: domain.WorkflowStepWork,
		Ordinal: 1, State: domain.WorkflowStepCompleted, ArtifactJSON: "{}", CreatedAt: now, UpdatedAt: now})
	tid := taskID
	child := domain.WorkflowRun{ID: childID, ProjectID: "p", Objective: taskID, State: domain.WorkflowRunCompleted,
		PolicyVersion: policyVersionV1, PolicySnapshot: "{}", CreatedAt: now, UpdatedAt: now,
		ParentWorkflowID: &masterID, PlannedTaskID: &tid}
	createdRun, createdSteps, err := store.CreateWorkflowRun(ctx, child, steps)
	if err != nil {
		t.Fatal(err)
	}
	workStep := createdSteps[len(createdSteps)-1]
	sessID := string(sess.ID)
	if _, err := store.UpdateWorkflowStepSession(ctx, workStep.ID, sessID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-work-" + taskID, WorkflowRunID: childID, WorkflowStepID: &workStep.ID,
		ProjectID: "p", SessionID: &sessID, Branch: dbTestBranch, WorktreePath: dbTestRepo,
		DurablePhase: "work_completed", PayloadVersion: "v1", RetryState: "{}", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if commitSHA != "" {
		if _, err := store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
			ID: "wfc-commit-" + taskID, WorkflowRunID: childID, ProjectID: "p",
			Branch: dbTestBranch, WorktreePath: dbTestRepo, HeadSHA: commitSHA,
			DurablePhase: autonomousLocalCommitPhase, PayloadVersion: "v1", RetryState: "{}",
			CreatedAt: now.Add(time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	detail := RunDetail{Run: createdRun}
	for _, st := range createdSteps {
		detail.Steps = append(detail.Steps, StepDetail{Step: st})
	}
	return detail
}

// seedVerifyPassedOn writes the child's own verify_result checkpoint and
// returns its id, so a test can assert the audit points at THIS row rather than
// merely at some row.
func seedVerifyPassedOn(t *testing.T, ctx stdctx.Context, store *sqlite.Store, child RunDetail, projectID, fingerprint string) string {
	t.Helper()
	seedVerifyPassed(t, ctx, store, child, projectID, fingerprint)
	return "wfc-verify-" + child.Run.ID
}

// The audit records the verification that authorized the integration, with the
// links into the child's own durable evidence — not the bare "verificationRan:
// false" that made a promotion of verified work read as unverified.
func TestAuditCarriesTheChildsVerificationEvidence(t *testing.T) {
	facts := &directBranchFacts{}
	coord, store, ctx := newDirectBranchCoordinator(t, facts)
	head := dbCommit(t, "one")
	facts.obs = dbObservation(head)

	now := time.Now().UTC()
	master := domain.WorkflowRun{ID: "wf-evidence", ProjectID: "p", Objective: "one task",
		State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}",
		CreatedAt: now, UpdatedAt: now}
	if _, _, err := store.CreateWorkflowRun(ctx, master, nil); err != nil {
		t.Fatal(err)
	}
	task := seedPlanTask(t, ctx, store, master.ID, "task-v", 1, domain.WorkflowTaskRunning, nil)

	// A child whose PLAN declares verification, and whose verify step durably
	// recorded a pass. Both halves matter: the plan is how "nothing was asked"
	// is told apart from "nothing answered", and the checkpoint is the evidence.
	artifact, err := MarshalPlanArtifact(BuildPlanArtifact("p", "task", policyVersionV1, VerificationPlan{
		Commands: []VerificationCommandCheck{{Command: "go", Args: []string{"test", "./..."}, RequiredExitCode: 0}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	detail := seedDagChildUnder(t, ctx, store, master.ID, "task-v", head, artifact)
	verifyCP := seedVerifyPassedOn(t, ctx, store, detail, "p", head)

	if err := coord.promoteTaskToIntegration(ctx, master, task, detail); err != nil {
		t.Fatalf("promote: %v", err)
	}
	records, err := coord.ListTaskIntegrations(ctx, master.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 {
		t.Fatal("no audit row")
	}
	rec := records[len(records)-1]
	if !rec.VerificationRan || !rec.VerificationOK {
		t.Fatalf("audit says nothing verified this: %+v", rec)
	}
	if rec.VerificationSource != string(integration.SourceTaskVerification) {
		t.Fatalf("verification source = %q, want %q", rec.VerificationSource, integration.SourceTaskVerification)
	}
	if rec.VerificationRecord != verifyCP {
		t.Fatalf("evidence id = %q, want the child's verify_result checkpoint %q", rec.VerificationRecord, verifyCP)
	}
	if rec.VerifiedFingerprint != head {
		t.Fatalf("verified fingerprint = %q, want the commit that was integrated %q", rec.VerifiedFingerprint, head)
	}
}

func (f *dagFixture) taskState(t *testing.T, id string) domain.WorkflowTask {
	t.Helper()
	tasks, err := f.store.ListWorkflowTasks(f.ctx, f.master.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.ID == id {
			return task
		}
	}
	t.Fatalf("no task %s", id)
	return domain.WorkflowTask{}
}

// A conflicts and is parked; B is independent, keeps going, and integrates; C
// depends on A and waits. All three in one reconcile pass.
func TestParkedTaskStopsOnlyItsOwnDependents(t *testing.T) {
	f := newTaskDAGFixture(t)
	if err := f.coord.reconcileMasterTasksOnce(f.ctx, f.master); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := f.taskState(t, "task-a"); got.State != domain.WorkflowTaskNeedsAttention {
		t.Fatalf("task A state = %q, want needs_attention", got.State)
	}
	if got := f.taskState(t, "task-b"); got.State != domain.WorkflowTaskCompleted {
		t.Fatalf("task B state = %q, want completed — an independent sibling must not be stopped by A's conflict", got.State)
	}
	if got := f.taskState(t, "task-c"); got.State == domain.WorkflowTaskRunning || got.State == domain.WorkflowTaskCompleted {
		t.Fatalf("task C state = %q, want it still waiting on A", got.State)
	}
	if got := f.taskState(t, "task-c"); got.ExecutionRunID != nil {
		t.Fatal("task C was dispatched even though the task it depends on is parked")
	}

	// B really integrated: it has its own audit row and its own promotion.
	records, err := f.coord.ListTaskIntegrations(f.ctx, f.master.ID)
	if err != nil {
		t.Fatal(err)
	}
	landedB := false
	for _, rec := range records {
		if rec.TaskID == "task-b" && rec.Outcome == string(integration.OutcomeIntegrated) {
			landedB = true
		}
	}
	if !landedB {
		t.Fatalf("task B left no landed audit row: %+v", records)
	}
}

// The objective reflects attention only once nothing is left that can move —
// and then it names the task, so the card points at something actionable.
func TestObjectiveReflectsAttentionOnlyWhenTheDAGIsStuck(t *testing.T) {
	f := newTaskDAGFixture(t)
	if err := f.coord.reconcileMasterTasksOnce(f.ctx, f.master); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	run, ok, err := f.store.GetWorkflowRun(f.ctx, f.master.ID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun: %v (found=%v)", err, ok)
	}
	if run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("objective state = %q; with A parked, B done and C blocked on A, nothing can move", run.State)
	}
	stops := 0
	cps, err := f.store.ListWorkflowCheckpoints(f.ctx, f.master.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, cp := range cps {
		if cp.DurablePhase == ReasonTaskParked {
			stops++
		}
	}
	if stops != 1 {
		t.Fatalf("objective-level parked-task stops = %d, want exactly 1", stops)
	}
}

// The objective does NOT reflect attention while a sibling is still working.
// This is the half that makes the previous test meaningful: "stopped" has to be
// a conclusion about the plan, not a reaction to the first conflict.
func TestObjectiveKeepsRunningWhileASiblingCanStillMove(t *testing.T) {
	f := newTaskDAGFixture(t)
	// B has not finished: no child run at all yet, so it is still dispatchable
	// work rather than something waiting on A.
	if _, err := f.store.UpdateWorkflowRunState(f.ctx, "wf-exec-task-b",
		domain.WorkflowRunCompleted, domain.WorkflowRunRunning, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	if err := f.coord.reconcileMasterTasksOnce(f.ctx, f.master); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := f.taskState(t, "task-a"); got.State != domain.WorkflowTaskNeedsAttention {
		t.Fatalf("task A state = %q, want needs_attention", got.State)
	}
	run, ok, err := f.store.GetWorkflowRun(f.ctx, f.master.ID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun: %v (found=%v)", err, ok)
	}
	if run.State != domain.WorkflowRunRunning {
		t.Fatalf("objective state = %q, want running while task B is still working", run.State)
	}
}
