package workflow_test

// repair_artifact_authority_test.go — the wf-3af3c533 regression, and the
// artifact-authority contract around it.
//
// THE INCIDENT, as a shape a test can reproduce. A task run writes real code on
// its own branch, in its own worktree, and commits it. A reviewer returns
// changes_requested. The fix worker terminates without changing anything. AO
// launches a repair — and the repair's checkout is cut from the project's
// default branch, where none of that code exists. The repairer changes nothing,
// correctly, and AO blames it.
//
// EVERY TEST HERE IS A PROPERTY OF THE REPAIR'S WORKSPACE, not of a mock. The
// fixture cuts real git worktrees off a real repository, the task branch really
// does carry a file that main does not, and the assertion that matters is made
// against the repair worktree's own filesystem: is the code under review in it?
// Before the fix that answer is no, for every one of these tests.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// repairableChild drives a master objective until its first task has a child
// run with a real, committed worktree, then parks that child on a repairable
// technical condition.
//
// Nothing is seeded except the stop itself: the worktree, the branch, the
// commit, the placement and the ledger are all produced by the ordinary
// dispatch path, which is the whole point — an artifact AO invented for a test
// would prove nothing about an artifact AO has to find in production.
func repairableChild(t *testing.T) (*autonomousFixture, context.Context, string, string) {
	t.Helper()
	fx, ctx, masterID := startAutonomousObjective(t, twoTaskDependentPlan())
	_, childID := dispatchedChild(t, fx, masterID)
	parkOnHumanDecision(t, fx, childID)
	makeChildStopRepairable(t, fx, childID)
	freezeRepairMode(t, fx, childID, domain.RepairModeSuggest)
	// Room to actually run the repair. The origin's own reviewer still holds a
	// slot, and the point of these tests is the repair's WORKSPACE, not the
	// scheduler -- p1c_capacity_test.go owns that question.
	fx.withCapacityLimits(roomyLimits())
	return fx, ctx, masterID, childID
}

func roomyLimits() domain.CapacityLimits {
	return domain.CapacityLimits{Global: 20, PerWorkflow: 20, PerKind: map[domain.ExecutionKind]int{
		domain.ExecutionKindWorker: 20, domain.ExecutionKindReviewer: 20,
		domain.ExecutionKindPlanner: 20, domain.ExecutionKindRepair: 20,
	}}
}

// originWorktree is the checkout the child task actually wrote in, read from
// the durable dispatch record rather than guessed from the fixture's layout.
func originWorktree(t *testing.T, fx *autonomousFixture, childID string) string {
	t.Helper()
	cps, err := fx.store.ListWorkflowCheckpoints(context.Background(), childID)
	if err != nil {
		t.Fatal(err)
	}
	path := ""
	for _, cp := range cps {
		if cp.WorktreePath != "" {
			path = cp.WorktreePath
		}
	}
	if path == "" {
		t.Fatal("the child task recorded no worktree; the fixture never reached the state under test")
	}
	return path
}

func gitHead(t *testing.T, dir string) string {
	t.Helper()
	out, err := autoGit(dir, "rev-parse", "--verify", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD in %s: %v: %s", dir, err, out)
	}
	return strings.TrimSpace(out)
}

// repairWorkspacePath is the worktree the repair's worker was actually spawned
// into, taken from the spawner's own record of the launch.
func repairWorkspacePath(t *testing.T, fx *autonomousFixture, repairRunID string) string {
	t.Helper()
	cps, err := fx.store.ListWorkflowCheckpoints(context.Background(), repairRunID)
	if err != nil {
		t.Fatal(err)
	}
	path := ""
	for _, cp := range cps {
		if cp.WorktreePath != "" {
			path = cp.WorktreePath
		}
	}
	return path
}

// driveRepairWorkerLaunch runs poller cycles until the repair run has actually
// spawned its worker, so the assertions below are about a checkout that exists.
func driveRepairWorkerLaunch(t *testing.T, fx *autonomousFixture, repairRunID string) string {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 12 && repairWorkspacePath(t, fx, repairRunID) == ""; i++ {
		// A repair run is an ordinary bounded task run, and an ordinary task run
		// advances through the same read/continue path a person or the poller
		// enters. Both are driven here so the launch happens the way production
		// makes it happen rather than by reaching into dispatch.
		if _, err := fx.coord.ContinueRun(ctx, repairRunID); err != nil {
			t.Fatalf("ContinueRun(repair): %v", err)
		}
		if _, err := fx.coord.GetRun(ctx, repairRunID); err != nil {
			t.Fatalf("GetRun(repair): %v", err)
		}
		driveCycles(t, fx, 1, nil)
	}
	path := repairWorkspacePath(t, fx, repairRunID)
	if path == "" {
		t.Fatal("the repair run never launched a worker, so there is no checkout to judge")
	}
	return path
}

// ---------------------------------------------------------------------------
// A, C, D — the incident itself.
// ---------------------------------------------------------------------------

// A repair must be able to see the code it was created to repair. The task
// branch carries a file that does not exist on main (C); the repair's checkout
// must be cut from that branch's exact committed head (D); and the file must be
// in it (A).
//
// This is the regression. Before the fix the repair run was created top-level,
// its base ref resolved to nothing, and the session manager cut its worktree
// from the project's default branch — so `task-1.txt` was absent and the
// repairer had nothing to work on.
func TestRepairChecksOutTheArtifactUnderReview(t *testing.T) {
	fx, ctx, _, childID := repairableChild(t)
	origin := originWorktree(t, fx, childID)
	artifactSHA := gitHead(t, origin)

	// The precondition that makes this test mean anything: the work really is
	// on the task branch and really is absent from the project's default one.
	taskFile := "task-1.txt"
	if _, err := os.Stat(filepath.Join(origin, taskFile)); err != nil {
		t.Fatalf("the task worktree does not carry %s, so this test cannot detect the defect: %v", taskFile, err)
	}
	if out, err := autoGit(fx.spawner.repoPath, "cat-file", "-e", "main:"+taskFile); err == nil {
		t.Fatalf("%s exists on main, so a main-based checkout would look correct: %s", taskFile, out)
	}

	intent, err := fx.coord.LaunchRepair(ctx, childID, "operator")
	if err != nil {
		t.Fatalf("LaunchRepair: %v", err)
	}

	// D — the authority pins the exact commit, not a branch name and not main.
	if !intent.Origin.Resolved || !intent.Origin.HasArtifact {
		t.Fatalf("the repair was created without establishing its artifact: %+v", intent.Origin)
	}
	if intent.Origin.BaseSHA != artifactSHA {
		t.Fatalf("repair base = %q, want the task's own head %q", intent.Origin.BaseSHA, artifactSHA)
	}
	if intent.Origin.OriginRunID != childID {
		t.Fatalf("repair origin run = %q, want the task run %q", intent.Origin.OriginRunID, childID)
	}

	// A/C — and the checkout the worker actually got contains the work.
	repairPath := driveRepairWorkerLaunch(t, fx, intent.RepairRunID)
	assertContainsCommit(t, repairPath, artifactSHA)
	if _, err := os.Stat(filepath.Join(repairPath, taskFile)); err != nil {
		t.Fatalf("the repair's checkout does not contain %s — it cannot repair code it cannot see: %v", taskFile, err)
	}
}

// assertContainsCommit is the property that actually matters about a repair's
// checkout: the artifact under review is reachable from it. Equality is too
// strong -- the worker committing on top of the artifact is the repair doing
// its job -- and "the branch looks right" is too weak, which is what the
// incident's main-based checkout looked like.
func assertContainsCommit(t *testing.T, worktree, commit string) {
	t.Helper()
	if out, err := autoGit(worktree, "merge-base", "--is-ancestor", commit, "HEAD"); err != nil {
		t.Fatalf("checkout %s (at %s) does not contain the artifact %s: %v: %s",
			worktree, gitHead(t, worktree), commit, err, out)
	}
}

// The repair run carries its authority durably, on its own ledger, before it
// starts — which is what makes the answer survive a restart and what every
// later reader consults instead of project configuration.
func TestRepairRunRecordsItsArtifactAuthorityBeforeStarting(t *testing.T) {
	fx, ctx, _, childID := repairableChild(t)
	artifactSHA := gitHead(t, originWorktree(t, fx, childID))

	intent, err := fx.coord.LaunchRepair(ctx, childID, "operator")
	if err != nil {
		t.Fatalf("LaunchRepair: %v", err)
	}
	cps, err := fx.store.ListWorkflowCheckpoints(ctx, intent.RepairRunID)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, cp := range cps {
		if cp.DurablePhase != "workflow_repair_run_origin" {
			continue
		}
		found++
		var body struct {
			OriginRunID string                         `json:"originRunId"`
			Generation  int                            `json:"generation"`
			Origin      domain.RepairArtifactAuthority `json:"origin"`
		}
		if err := json.Unmarshal([]byte(cp.RetryState), &body); err != nil {
			t.Fatalf("origin marker is unreadable: %v", err)
		}
		// The pre-existing fields every other reader parses must keep working.
		if body.OriginRunID == "" || body.Generation != 1 {
			t.Fatalf("origin marker lost its legacy shape: %+v", body)
		}
		if body.Origin.BaseSHA != artifactSHA {
			t.Fatalf("durable repair base = %q, want %q", body.Origin.BaseSHA, artifactSHA)
		}
		if body.Origin.OriginRunID != childID || !body.Origin.Valid() {
			t.Fatalf("durable repair authority does not name a valid artifact: %+v", body.Origin)
		}
	}
	if found != 1 {
		t.Fatalf("%d origin markers on the repair run, want exactly 1", found)
	}
}

// ---------------------------------------------------------------------------
// G, H — restart, and duplicate wakes.
// ---------------------------------------------------------------------------

// G: a daemon restart between the repair run's creation and its worktree's
// creation must not change what that worktree is cut from. The authority is on
// disk; a rebuilt coordinator reads the same answer.
//
// H: driving many poller cycles over one repairable failure produces ONE repair
// run and ONE checkout, not one per tick.
func TestRepairAuthoritySurvivesRestartAndDuplicateWakes(t *testing.T) {
	fx, ctx, _, childID := repairableChild(t)
	artifactSHA := gitHead(t, originWorktree(t, fx, childID))

	intent, err := fx.coord.LaunchRepair(ctx, childID, "operator")
	if err != nil {
		t.Fatalf("LaunchRepair: %v", err)
	}
	// The restart: every in-memory decision is gone and the coordinator is
	// rebuilt over the same durable store.
	fx.withCapacityLimits(roomyLimits())

	repairPath := driveRepairWorkerLaunch(t, fx, intent.RepairRunID)
	assertContainsCommit(t, repairPath, artifactSHA)

	// H: many more passes, still one repair and one worktree for it.
	driveCycles(t, fx, 10, nil)
	if again, lerr := fx.coord.LaunchRepair(ctx, childID, "operator"); lerr != nil {
		t.Fatalf("re-asking for the same repair errored: %v", lerr)
	} else if again.RepairRunID != intent.RepairRunID {
		t.Fatalf("the same failure produced two repair runs (%s, %s)", intent.RepairRunID, again.RepairRunID)
	}
	launches := 0
	for _, cfg := range fx.spawner.calls {
		if strings.Contains(string(cfg.IssueID), "workflow-step:") && cfg.BaseRef == artifactSHA {
			launches++
		}
	}
	if launches != 1 {
		t.Fatalf("%d worker launches were pinned to the artifact, want exactly 1", launches)
	}
}

// ---------------------------------------------------------------------------
// F — fail closed.
// ---------------------------------------------------------------------------

// An artifact that exists only as uncommitted work in the origin's own worktree
// cannot be handed to a second checkout, and AO refuses rather than cutting one
// from the last commit — which would give the repairer a tree missing exactly
// the change under review.
func TestRepairRefusesAnUncommittedArtifact(t *testing.T) {
	fx, ctx, _, childID := repairableChild(t)

	// The worker's real correction is still dirty in its worktree.
	fx.ws.obs.Changes = append(fx.ws.obs.Changes, ports.WorkspaceChange{Path: "manager.go", Status: " M"})

	plan, err := fx.coord.PlanRepair(ctx, childID)
	if err != nil {
		t.Fatalf("PlanRepair: %v", err)
	}
	if plan.Eligibility != domain.RepairArtifactUnprovable {
		t.Fatalf("eligibility = %q, want artifact_unprovable for an uncommitted artifact (%s)", plan.Eligibility, plan.Reason)
	}
	if plan.Artifact.Refusal != domain.RepairArtifactUncommitted {
		t.Fatalf("refusal = %q, want repair_artifact_uncommitted", plan.Artifact.Refusal)
	}
	if _, lerr := fx.coord.LaunchRepair(ctx, childID, "operator"); lerr == nil {
		t.Fatal("a repair was created for an artifact AO cannot cut a checkout from")
	}
	// And the refusal is a durable, named stop rather than an absence.
	if got := autoLedgerPhases(t, fx, childID)[workflowcore.ReasonRepairArtifactUncommitted]; got == 0 {
		t.Fatal("the refusal left nothing on the ledger saying why AO would not repair")
	}
	// Nothing was created: no repair run, no worktree, no spent generation.
	if got := autoLedgerPhases(t, fx, childID)["workflow_repair_dispatched"]; got != 0 {
		t.Fatalf("%d repair dispatches recorded for a refused repair, want 0", got)
	}
}

// The same fail-closed answer when AO cannot establish a committed head at all:
// no worktree to read and nothing on the ledger that names a commit.
func TestRepairRefusesWhenNoCommittedHeadCanBeEstablished(t *testing.T) {
	fx, ctx, _, childID := repairableChild(t)

	// The worktree is gone AND the fixture reports no head for it, which is the
	// state a cleaned-up, never-committed task is in.
	if err := os.RemoveAll(originWorktree(t, fx, childID)); err != nil {
		t.Fatal(err)
	}
	plan, err := fx.coord.PlanRepair(ctx, childID)
	if err != nil {
		t.Fatalf("PlanRepair: %v", err)
	}
	if plan.Eligibility != domain.RepairArtifactUnprovable ||
		plan.Artifact.Refusal != domain.RepairArtifactUnavailable {
		t.Fatalf("eligibility = %q refusal = %q, want artifact_unprovable / repair_artifact_unavailable (%s)",
			plan.Eligibility, plan.Artifact.Refusal, plan.Reason)
	}
	if plan.Reason == "" {
		t.Fatal("a fail-closed refusal must still say what could not be established")
	}
}

// ---------------------------------------------------------------------------
// E — the worktree is gone, the commit is not.
// ---------------------------------------------------------------------------

// A task whose checkout has been cleaned up still has its work: the branch is
// there and AO wrote the commit down. The repair is reconstructed from the
// ledger rather than refused, and it is still cut from that exact commit.
func TestRepairReconstructsFromTheLedgerWhenTheWorktreeIsGone(t *testing.T) {
	fx, ctx, _, childID := repairableChild(t)
	origin := originWorktree(t, fx, childID)
	artifactSHA := gitHead(t, origin)

	// The commit AO records for a task at its own commit boundary. It is
	// written through the store because that is where the isolated commit path
	// writes it, and it is the ONLY durable trace left once the checkout is
	// removed.
	if _, err := fx.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-committed-" + childID, WorkflowRunID: childID, ProjectID: "p",
		HeadSHA:      artifactSHA,
		NextAction:   "local_commit_created: " + artifactSHA,
		DurablePhase: "autonomous_local_commit", PayloadVersion: "v1", RetryState: "{}",
		CreatedAt: fx.clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(origin); err != nil {
		t.Fatal(err)
	}

	plan, err := fx.coord.PlanRepair(ctx, childID)
	if err != nil {
		t.Fatalf("PlanRepair: %v", err)
	}
	if !plan.Eligibility.Allowed() {
		t.Fatalf("eligibility = %q, want eligible: the commit still exists (%s)", plan.Eligibility, plan.Reason)
	}
	if plan.Artifact.BaseSHA != artifactSHA {
		t.Fatalf("reconstructed base = %q, want %q", plan.Artifact.BaseSHA, artifactSHA)
	}
	if plan.Artifact.Source != domain.RepairArtifactReconstructed {
		t.Fatalf("source = %q, want reconstructed_from_ledger", plan.Artifact.Source)
	}
}

// ---------------------------------------------------------------------------
// B — the failed fix attempt's retry stays on the same artifact.
// ---------------------------------------------------------------------------

// A repair whose own run ends without repairing does not send the next
// generation somewhere else. The origin's artifact is resolved again from the
// same durable facts, so generation 2 is cut from exactly what generation 1
// was.
func TestASecondRepairGenerationTargetsTheSameArtifact(t *testing.T) {
	fx, ctx, _, childID := repairableChild(t)
	artifactSHA := gitHead(t, originWorktree(t, fx, childID))

	first, err := fx.coord.LaunchRepair(ctx, childID, "operator")
	if err != nil {
		t.Fatalf("first repair: %v", err)
	}
	if first.Origin.BaseSHA != artifactSHA {
		t.Fatalf("generation 1 base = %q, want %q", first.Origin.BaseSHA, artifactSHA)
	}
	// The repair run ends without repairing, which is what a terminated fix
	// worker's repair looks like from the origin's side.
	if _, err := fx.coord.CancelRun(ctx, first.RepairRunID); err != nil {
		t.Fatalf("end the first repair: %v", err)
	}
	if err := fx.coord.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	second, err := fx.coord.LaunchRepair(ctx, childID, "operator")
	if err != nil {
		t.Fatalf("second repair: %v", err)
	}
	if second.RepairRunID == first.RepairRunID {
		t.Fatal("the second generation reused the first repair's run")
	}
	if second.Origin.BaseSHA != artifactSHA {
		t.Fatalf("generation 2 base = %q, want the same artifact %q", second.Origin.BaseSHA, artifactSHA)
	}
	if second.Origin.OriginRunID != first.Origin.OriginRunID {
		t.Fatalf("generation 2 repairs %q, generation 1 repaired %q", second.Origin.OriginRunID, first.Origin.OriginRunID)
	}
}

// ---------------------------------------------------------------------------
// J, K, L — the repaired candidate becomes the task's artifact.
// ---------------------------------------------------------------------------

// A repair that completes with a commit of its own moves the origin task's own
// branch onto it, so review, verification and integration afterwards all read
// the repaired candidate. Without this the origin resumes onto the code the
// reviewer already rejected and the repair's commit is stranded.
func TestSuccessfulRepairPromotesItsCandidateOntoTheOriginBranch(t *testing.T) {
	fx, ctx, _, childID := repairableChild(t)
	origin := originWorktree(t, fx, childID)
	artifactSHA := gitHead(t, origin)

	intent, err := fx.coord.LaunchRepair(ctx, childID, "operator")
	if err != nil {
		t.Fatalf("LaunchRepair: %v", err)
	}
	repairPath := driveRepairWorkerLaunch(t, fx, intent.RepairRunID)

	// The repair agent's own work: a commit on the repair's branch, on top of
	// the artifact it was cut from.
	if err := os.WriteFile(filepath.Join(repairPath, "task-1.txt"), []byte("repaired\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, gerr := autoGit(repairPath, "commit", "-am", "repair the finding"); gerr != nil {
		t.Fatalf("commit the repair: %v: %s", gerr, out)
	}
	repaired := gitHead(t, repairPath)
	if repaired == artifactSHA {
		t.Fatal("the repair produced no commit; there is nothing to promote")
	}
	// AO's own record of that commit, written at the isolated commit boundary.
	if _, err := fx.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-repaired-" + intent.RepairRunID, WorkflowRunID: intent.RepairRunID, ProjectID: "p",
		HeadSHA: repaired, WorktreePath: repairPath,
		NextAction:   "local_commit_created: " + repaired,
		DurablePhase: "autonomous_local_commit", PayloadVersion: "v1", RetryState: "{}",
		CreatedAt: fx.clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// The repair run reaches completed through the domain's own transitions.
	run, ok, err := fx.store.GetWorkflowRun(ctx, intent.RepairRunID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(repair): %v (found=%v)", err, ok)
	}
	if run.State == domain.WorkflowRunPending {
		mustTransitionRun(t, fx, intent.RepairRunID, domain.WorkflowRunPending, domain.WorkflowRunRunning)
		run.State = domain.WorkflowRunRunning
	}
	mustTransitionRun(t, fx, intent.RepairRunID, run.State, domain.WorkflowRunCompleted)

	for pass := 0; pass < 3; pass++ {
		if err := fx.coord.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// J — the origin task's own checkout now holds the repaired candidate.
	if got := gitHead(t, origin); got != repaired {
		t.Fatalf("the origin task is still at %q; the repaired candidate %q was never promoted", got, repaired)
	}
	// K/L — and it is the artifact, so everything downstream reads it.
	body, err := os.ReadFile(filepath.Join(origin, "task-1.txt"))
	if err != nil || strings.TrimSpace(string(body)) != "repaired" {
		t.Fatalf("the origin task's tree does not carry the repair (%q, %v)", string(body), err)
	}
	phases := autoLedgerPhases(t, fx, childID)
	if phases["repair_candidate_promoted"] != 1 {
		t.Fatalf("repair_candidate_promoted = %d, want exactly 1; phases=%v",
			phases["repair_candidate_promoted"], phases)
	}
	// Exactly once, however many passes run.
	if err := fx.coord.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := autoLedgerPhases(t, fx, childID)["repair_candidate_promoted"]; got != 1 {
		t.Fatalf("repair_candidate_promoted = %d after another pass, want 1", got)
	}

	// K/L, at the level this change is responsible for: AO's own notion of what
	// this task's artifact IS has moved to the repaired commit. Everything
	// downstream — the next review's target, verification, integration — reads
	// the task's checkout, and the task's checkout is now the repaired one.
	after, err := fx.coord.PlanRepair(ctx, childID)
	if err != nil {
		t.Fatalf("PlanRepair after promotion: %v", err)
	}
	if after.Artifact.BaseSHA != repaired {
		t.Fatalf("after promotion AO still calls %q this task's artifact, want the repaired %q",
			after.Artifact.BaseSHA, repaired)
	}
}

// A promotion that cannot be made safely is refused, named, and does not lose
// the repaired commit: the origin parks with the branch and commit on the
// ledger rather than having somebody's uncommitted work overwritten.
func TestPromotionIsRefusedWhenTheOriginCheckoutHasMoved(t *testing.T) {
	fx, ctx, _, childID := repairableChild(t)
	origin := originWorktree(t, fx, childID)

	intent, err := fx.coord.LaunchRepair(ctx, childID, "operator")
	if err != nil {
		t.Fatalf("LaunchRepair: %v", err)
	}
	repairPath := driveRepairWorkerLaunch(t, fx, intent.RepairRunID)
	if err := os.WriteFile(filepath.Join(repairPath, "task-1.txt"), []byte("repaired\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, gerr := autoGit(repairPath, "commit", "-am", "repair"); gerr != nil {
		t.Fatalf("commit the repair: %v: %s", gerr, out)
	}
	repaired := gitHead(t, repairPath)
	if _, err := fx.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-repaired-" + intent.RepairRunID, WorkflowRunID: intent.RepairRunID, ProjectID: "p",
		HeadSHA: repaired, NextAction: "local_commit_created: " + repaired,
		DurablePhase: "autonomous_local_commit", PayloadVersion: "v1", RetryState: "{}",
		CreatedAt: fx.clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Somebody's uncommitted work in the origin's checkout. Fast-forwarding
	// over it would destroy the only copy.
	if err := os.WriteFile(filepath.Join(origin, "local-edit.txt"), []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	run, _, _ := fx.store.GetWorkflowRun(ctx, intent.RepairRunID)
	if run.State == domain.WorkflowRunPending {
		mustTransitionRun(t, fx, intent.RepairRunID, domain.WorkflowRunPending, domain.WorkflowRunRunning)
		run.State = domain.WorkflowRunRunning
	}
	mustTransitionRun(t, fx, intent.RepairRunID, run.State, domain.WorkflowRunCompleted)
	if err := fx.coord.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(origin, "local-edit.txt")); err != nil {
		t.Fatalf("the origin's uncommitted work was destroyed by a promotion: %v", err)
	}
	phases := autoLedgerPhases(t, fx, childID)
	if phases[workflowcore.ReasonRepairPromotionBlocked] == 0 {
		t.Fatalf("a refused promotion left no named stop; phases=%v", phases)
	}
	// The repaired commit is not lost: the stop names it.
	cps, err := fx.store.ListWorkflowCheckpoints(ctx, childID)
	if err != nil {
		t.Fatal(err)
	}
	named := false
	for _, cp := range cps {
		if cp.DurablePhase == workflowcore.ReasonRepairPromotionBlocked && strings.Contains(cp.NextAction, repaired[:12]) {
			named = true
		}
	}
	if !named {
		t.Fatal("the refused promotion did not name the repaired commit, so the work would be unfindable")
	}
}

// ---------------------------------------------------------------------------
// I — a superseded generation may not move anything.
// ---------------------------------------------------------------------------

// A repair generation the origin has already moved past must not promote a
// candidate onto its branch. It is the same stale-writer fence every other
// authority in this package applies, on the one operation that mutates a
// checkout somebody else now owns.
func TestSupersededRepairGenerationPromotesNothing(t *testing.T) {
	fx, ctx, _, childID := repairableChild(t)
	origin := originWorktree(t, fx, childID)
	artifactSHA := gitHead(t, origin)

	first, err := fx.coord.LaunchRepair(ctx, childID, "operator")
	if err != nil {
		t.Fatalf("first repair: %v", err)
	}
	firstPath := driveRepairWorkerLaunch(t, fx, first.RepairRunID)
	if err := os.WriteFile(filepath.Join(firstPath, "task-1.txt"), []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, gerr := autoGit(firstPath, "commit", "-am", "stale repair"); gerr != nil {
		t.Fatalf("commit: %v: %s", gerr, out)
	}
	staleSHA := gitHead(t, firstPath)

	// The origin moves on: generation 1 ends without repairing and generation 2
	// is minted.
	if _, err := fx.coord.CancelRun(ctx, first.RepairRunID); err != nil {
		t.Fatal(err)
	}
	if err := fx.coord.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.coord.LaunchRepair(ctx, childID, "operator"); err != nil {
		t.Fatalf("second repair: %v", err)
	}

	// Generation 1's commit arrives late. It must move nothing.
	if _, err := fx.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-stale-" + first.RepairRunID, WorkflowRunID: first.RepairRunID, ProjectID: "p",
		HeadSHA: staleSHA, NextAction: "local_commit_created: " + staleSHA,
		DurablePhase: "autonomous_local_commit", PayloadVersion: "v1", RetryState: "{}",
		CreatedAt: fx.clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 3; pass++ {
		if err := fx.coord.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := gitHead(t, origin); got != artifactSHA {
		t.Fatalf("a superseded repair generation moved the origin branch to %q (was %q)", got, artifactSHA)
	}
}

// ---------------------------------------------------------------------------
// §6 — the findings travel with the repair.
// ---------------------------------------------------------------------------

// The repair's objective must carry the concrete evidence: which branch and
// commit it is on, and what the reviewer actually said. The incident's repair
// prompt carried none of it and told the agent to read changes that were not
// there.
func TestRepairObjectiveCarriesTheArtifactAndTheFindings(t *testing.T) {
	fx, ctx, _, childID := repairableChild(t)
	artifactSHA := gitHead(t, originWorktree(t, fx, childID))

	intent, err := fx.coord.LaunchRepair(ctx, childID, "operator")
	if err != nil {
		t.Fatalf("LaunchRepair: %v", err)
	}
	repairRun, ok, err := fx.store.GetWorkflowRun(ctx, intent.RepairRunID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(repair): %v (found=%v)", err, ok)
	}
	for _, want := range []string{"THE CODE UNDER REPAIR", artifactSHA, childID} {
		if !strings.Contains(repairRun.Objective, want) {
			t.Fatalf("the repair objective does not name %q:\n%s", want, repairRun.Objective)
		}
	}
}

// ---------------------------------------------------------------------------
// §12 — worker_turn_produced_nothing keeps its meaning.
// ---------------------------------------------------------------------------

// The guard stays. What changes is that AO may only reach for it once it can
// show the worker held the artifact. A repair whose checkout provably did not
// is AO's own workspace error, and the run must say so instead of asking a
// person to judge whether an empty turn was correct for code the agent could
// not see.
func TestEmptyRepairTurnIsNotBlamedOnTheWorkerWhenTheWorkspaceWasWrong(t *testing.T) {
	fx, ctx, _, childID := repairableChild(t)
	intent, err := fx.coord.LaunchRepair(ctx, childID, "operator")
	if err != nil {
		t.Fatalf("LaunchRepair: %v", err)
	}
	produced := workflowcore.WorkStepDecision{
		AttentionReason: workflowcore.ReasonWorkerTurnProducedNothing,
		NextAction:      "worker reported its turn finished, but AO can see no change in its workspace",
	}

	// With the checkout proven, the judgement is the worker's and stands.
	driveRepairWorkerLaunch(t, fx, intent.RepairRunID)
	repairRun, _, err := fx.store.GetWorkflowRun(ctx, intent.RepairRunID)
	if err != nil {
		t.Fatal(err)
	}
	if got := fx.coord.ReclassifyRepairWorkspaceStopForTest(ctx, repairRun, produced); got.AttentionReason != workflowcore.ReasonWorkerTurnProducedNothing {
		t.Fatalf("a proven repair checkout changed the reason to %q; the guard must survive", got.AttentionReason)
	}

	// Disproven, and the same decision becomes AO's error rather than the
	// worker's. The row is the one confirmation writes; writing it here is how
	// a test reaches the state a mis-cut checkout produces.
	if _, err := fx.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-mismatch-" + intent.RepairRunID, WorkflowRunID: intent.RepairRunID, ProjectID: "p",
		NextAction:   "repair checkout does not contain the artifact under repair",
		DurablePhase: "repair_workspace_mismatch", PayloadVersion: "v1", RetryState: "{}",
		CreatedAt: fx.clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got := fx.coord.ReclassifyRepairWorkspaceStopForTest(ctx, repairRun, produced)
	if got.AttentionReason != workflowcore.ReasonRepairWorkspaceMismatch {
		t.Fatalf("reason = %q, want repair_workspace_mismatch: the worker was blamed for AO's workspace", got.AttentionReason)
	}
	if !strings.Contains(got.NextAction, "does not hold the artifact") {
		t.Fatalf("the reclassified sentence does not say what was actually wrong: %q", got.NextAction)
	}
	// And it is a real stop with a real remedy, not an unnamed one.
	if _, ok := workflowcore.AttentionDispositionForTest(workflowcore.ReasonRepairWorkspaceMismatch); !ok {
		t.Fatal("repair_workspace_mismatch is not a registered stop, so nothing can explain it to a person")
	}
}

// ---------------------------------------------------------------------------
// §4 — no silent main-based repair checkout, ever.
// ---------------------------------------------------------------------------

// A repair run whose origin marker carries no artifact authority is exactly
// what every repair created before this change looks like. Its base ref must be
// a refusal, not an empty string: an empty base ref is what the session manager
// resolves to the project's default branch, so "AO could not say" and "AO said
// main" were the same call, and that is the incident.
func TestALegacyRepairRunIsRefusedRatherThanCutFromMain(t *testing.T) {
	fx, ctx, _, childID := repairableChild(t)

	// A repair run recorded the pre-artifact-authority way: the marker names
	// its origin and its generation, and nothing else.
	created, err := fx.coord.CreateRun(ctx, "p", "legacy repair objective")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-legacy-origin", WorkflowRunID: created.Run.ID, ProjectID: "p",
		NextAction:   "repair run for " + childID + ", generation 1",
		DurablePhase: "workflow_repair_run_origin", PayloadVersion: "v1",
		RetryState: `{"originRunId":"` + childID + `","generation":1}`,
		CreatedAt:  fx.clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	base, refusal, pinned := fx.coord.RepairBaseRefForTest(ctx, created.Run)
	if pinned {
		t.Fatalf("a legacy repair run reported a pinned base %q; it has no recorded artifact", base)
	}
	if refusal.Refusal != domain.RepairArtifactUnavailable {
		t.Fatalf("refusal = %q, want repair_artifact_unavailable: an unreadable authority must refuse, not default to main",
			refusal.Refusal)
	}
	if refusal.Detail == "" {
		t.Fatal("the refusal says nothing about why AO will not launch")
	}

	// And the dispatch actually refuses: the run parks, and no worker is
	// launched into a checkout AO cannot vouch for.
	spawnsBefore := len(fx.spawner.calls)
	if _, err := fx.coord.StartRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := fx.coord.ContinueRun(ctx, created.Run.ID); err != nil {
			t.Fatalf("ContinueRun: %v", err)
		}
	}
	if len(fx.spawner.calls) != spawnsBefore {
		t.Fatalf("a worker was launched for an unvouched repair checkout (%d -> %d)", spawnsBefore, len(fx.spawner.calls))
	}
	run, _, err := fx.store.GetWorkflowRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("the refused repair run is %s, want needs_attention", run.State)
	}
	phases := map[string]int{}
	cps, err := fx.store.ListWorkflowCheckpoints(ctx, created.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, cp := range cps {
		phases[cp.DurablePhase]++
	}
	if phases[workflowcore.ReasonRepairArtifactUnavailable] == 0 {
		t.Fatalf("the refusal left no named stop; phases=%v", phases)
	}
}
