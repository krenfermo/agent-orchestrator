package workflow_test

// repair_quiescence_test.go — the last route by which a stale review could hold
// a newer head shut, closed and pinned.
//
// THE CASE (wf-724a1e97 / wf-f5025a7c). The task run is parked on
// fix_budget_exhausted. Its repair generation 2 is parked in needs_attention on
// a human-owned stop: it can launch nothing, write nothing and move nothing
// until a person acts. Under "in flight means non-terminal" that parked repair
// held the task run shut, so 247d3bc5f — the commit that answers the standing
// finding — could only be adopted if somebody pressed Continue.
//
// The positive test drives the daemon's own reconcile and touches nothing else.
// The six negatives are the point of the exercise: quiescence is a PROOF, and a
// proof that only ever succeeds is indistinguishable from an assumption. Each
// negative removes exactly one fact and requires the answer to flip.

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
)

// quiescenceCase is the incident's exact durable shape: an origin parked on
// fix_budget_exhausted with its budget really spent, and a repair generation
// parked for a person.
type quiescenceCase struct {
	*incidentE2E
	repairRunID  string
	repairStepID string
	generation   int
	reviewOfA    string
}

// newQuiescenceCase builds it. The repair is seeded through the same durable
// rows LaunchRepair writes — a workflow_repair_dispatched checkpoint carrying a
// domain.RepairIntent that names a real repair run — because those rows ARE the
// evidence the proof reads, and constructing them directly is what lets each
// negative below remove one fact and nothing else.
func newQuiescenceCase(t *testing.T) *quiescenceCase {
	t.Helper()
	f := newIncidentE2E(t)
	f.completeWork()
	if _, err := f.c.ContinueRun(f.ctx, f.runID); err != nil {
		t.Fatalf("ContinueRun (review handoff): %v", err)
	}

	// Three fix cycles, then the reviewer asks for changes once more: the
	// budget is genuinely spent and the run is genuinely parked.
	for cycle := 1; cycle <= 3; cycle++ {
		f.detail()
		f.answerReview(domain.VerdictChangesRequested, "still not right")
		f.detail()
		f.moveHead(fmt.Sprintf("cycle%02dsha0000000000000000000000000000000", cycle)[:40])
		f.detail()
	}
	f.moveHead(incidentHeadHumanFix)
	f.detail()
	reviewOfA := f.answerReview(domain.VerdictChangesRequested, "the audit doc still claims harness reads are observed")
	f.detail()
	if f.detail().Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("fixture precondition: the origin is not parked")
	}

	qc := &quiescenceCase{incidentE2E: f, reviewOfA: reviewOfA, generation: 2}
	qc.seedRepairGeneration(t, 1, domain.WorkflowRunCompleted)
	qc.seedRepairGeneration(t, 2, domain.WorkflowRunNeedsAttention)
	return qc
}

// seedRepairGeneration creates a real repair run in the given resting state and
// records the dispatch intent that binds it to the origin.
func (q *quiescenceCase) seedRepairGeneration(t *testing.T, generation int, state domain.WorkflowRunState) {
	t.Helper()
	ctx := q.ctx
	created, err := q.c.CreateRun(ctx, "agent-orchestrator",
		fmt.Sprintf("Repair a stopped AO workflow task (generation %d)", generation))
	if err != nil {
		t.Fatalf("CreateRun(repair %d): %v", generation, err)
	}
	repairID := created.Run.ID

	// Mark it as a repair agent's own run, exactly as LaunchRepair does before
	// starting one.
	if _, err := q.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: fmt.Sprintf("wfc-repair-origin-%d", generation), WorkflowRunID: repairID,
		ProjectID: created.Run.ProjectID, DurablePhase: "workflow_repair_run_origin",
		NextAction:     fmt.Sprintf("repair run for %s, generation %d", q.runID, generation),
		PayloadVersion: "v1", RetryState: "{}", CreatedAt: q.clk.Now(),
	}); err != nil {
		t.Fatalf("seed repair origin marker: %v", err)
	}

	switch state {
	case domain.WorkflowRunCompleted:
		moveRunRow(t, q, repairID, domain.WorkflowRunRunning)
		moveRunRow(t, q, repairID, domain.WorkflowRunCompleted)
	case domain.WorkflowRunNeedsAttention:
		moveRunRow(t, q, repairID, domain.WorkflowRunNeedsAttention)
		// The resting shape of a repair that DID its work and then stopped:
		// work completed, review and fix at waiting, nothing ready or running.
		// Seeded rather than assumed, because a repair whose work step is still
		// pending is one boot reconciliation away from launching a worker — and
		// that state is correctly NOT quiescent, which is a different test.
		for _, step := range listSteps(t, q, repairID) {
			switch step.Kind {
			case domain.WorkflowStepPlan:
				moveStepRow(t, q, step, domain.WorkflowStepRunning)
				moveStepRow(t, q, refreshStep(t, q, repairID, domain.WorkflowStepPlan), domain.WorkflowStepCompleted)
			case domain.WorkflowStepWork:
				moveStepRow(t, q, step, domain.WorkflowStepReady)
				moveStepRow(t, q, refreshStep(t, q, repairID, domain.WorkflowStepWork), domain.WorkflowStepRunning)
				moveStepRow(t, q, refreshStep(t, q, repairID, domain.WorkflowStepWork), domain.WorkflowStepCompleted)
			case domain.WorkflowStepReview, domain.WorkflowStepFix:
				moveStepRow(t, q, step, domain.WorkflowStepWaiting)
			}
		}
		// A human-owned stop.
		if _, err := q.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
			ID: fmt.Sprintf("wfc-repair-stop-%d", generation), WorkflowRunID: repairID,
			ProjectID:      created.Run.ProjectID,
			DurablePhase:   workflowcore.ReasonFixBudgetExhausted,
			NextAction:     "the repair's own review/fix budget is spent; the next step is a person's",
			PayloadVersion: "v1", RetryState: "{}", CreatedAt: q.clk.Now(),
		}); err != nil {
			t.Fatalf("seed repair stop: %v", err)
		}
		q.repairRunID = repairID
		for _, step := range listSteps(t, q, repairID) {
			if step.Kind == domain.WorkflowStepWork {
				q.repairStepID = step.ID
			}
		}
	}

	intent := domain.RepairIntent{
		ID:              fmt.Sprintf("wfr-quiescence-%d", generation),
		WorkflowRunID:   q.runID,
		TargetRunID:     q.runID,
		ConditionReason: workflowcore.ReasonFixBudgetExhausted,
		EvidenceDigest:  "digest-quiescence",
		Generation:      generation,
		ProjectID:       "agent-orchestrator",
		RepairRunID:     repairID,
		AuthorizedBy:    "operator",
		At:              q.clk.Now(),
	}
	payload, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	origin, _, err := q.store.GetWorkflowRun(ctx, q.runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: fmt.Sprintf("wfc-repair-dispatch-%d", generation), WorkflowRunID: q.runID,
		ProjectID:      origin.ProjectID,
		DurablePhase:   "workflow_repair_dispatched",
		NextAction:     fmt.Sprintf("repair generation %d dispatched as run %s", generation, repairID),
		PayloadVersion: "v1", RetryState: string(payload), CreatedAt: q.clk.Now(),
	}); err != nil {
		t.Fatalf("seed repair dispatch: %v", err)
	}
	q.clk.Advance(time.Second)
}

func moveRunRow(t *testing.T, q *quiescenceCase, runID string, to domain.WorkflowRunState) {
	t.Helper()
	run, ok, err := q.store.GetWorkflowRun(q.ctx, runID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(%s): %v (found=%v)", runID, err, ok)
	}
	if run.State == to {
		return
	}
	if _, err := q.store.UpdateWorkflowRunState(q.ctx, runID, run.State, to, q.clk.Now()); err != nil {
		t.Fatalf("UpdateWorkflowRunState(%s: %s -> %s): %v", runID, run.State, to, err)
	}
}

func listSteps(t *testing.T, q *quiescenceCase, runID string) []domain.WorkflowStep {
	t.Helper()
	steps, err := q.store.ListWorkflowSteps(q.ctx, runID)
	if err != nil {
		t.Fatalf("ListWorkflowSteps(%s): %v", runID, err)
	}
	return steps
}

// arriveHeadB is the resolving commit landing while everything of AO's is quiet.
func (q *quiescenceCase) arriveHeadB(t *testing.T) {
	t.Helper()
	q.goQuiet()
	q.moveHead(incidentHeadResolving)
}

// reconcileOnly is the daemon's own boot pass. No Continue, no GetRun from a
// browser, nothing a person does.
func (q *quiescenceCase) reconcileOnly(t *testing.T) {
	t.Helper()
	if err := q.c.Reconcile(q.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func (q *quiescenceCase) repairState(t *testing.T) workflowcore.RepairLifecycle {
	t.Helper()
	detail, err := q.c.GetRun(q.ctx, q.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	return detail.Repair
}

// ---------------------------------------------------------------------------
// The positive case.
// ---------------------------------------------------------------------------

func TestQuiescentRepairLetsTheOriginAdoptANewHeadWithoutAnybodyPressingContinue(t *testing.T) {
	q := newQuiescenceCase(t)
	q.arriveHeadB(t)
	launchesBefore := q.rl.launchCalls

	q.reconcileOnly(t)

	// The repair was folded as QUIESCENT, and deliberately not as resolved.
	if q.countPhase("workflow_repair_quiescent") != 1 {
		t.Fatalf("workflow_repair_quiescent rows = %d, want exactly 1\nphases: %v",
			q.countPhase("workflow_repair_quiescent"), q.phases())
	}
	// Generation 2 is QUIESCENT, never resolved: it did not repair anything and
	// a person still owns it. (Generation 1 legitimately IS resolved — it
	// completed, and reconcileRepairOutcome folds a real outcome.)
	for gen, outcome := range repairOutcomesFor(t, q, "workflow_repair_resolved") {
		if gen == q.generation {
			t.Fatalf("repair generation %d was recorded resolved (%q); a quiescent repair is not a resolved one", gen, outcome)
		}
	}
	// No repair generation was spent to get here.
	if n := q.countPhase("workflow_repair_dispatched"); n != 2 {
		t.Fatalf("repair dispatches = %d, want still exactly 2: a fold must not buy a generation", n)
	}

	// The origin adopted head B and asked exactly one fresh authoritative review
	// about it.
	if n := q.countPhase("human_applied_fix_observed"); n != 1 {
		t.Fatalf("human_applied_fix_observed = %d, want exactly 1\nphases: %v", n, q.phases())
	}
	if got := q.rl.launchCalls - launchesBefore; got != 1 {
		t.Fatalf("reviewer launches = %d, want exactly 1", got)
	}
	fresh := *reviewStepFrom(q.detail()).Step.ReviewRunID
	if fresh == q.reviewOfA {
		t.Fatalf("the origin is still bound to review %s, which judged the old head", q.reviewOfA)
	}
	prev, found, err := q.store.GetReviewRun(q.ctx, q.reviewOfA)
	if err != nil || !found {
		t.Fatalf("GetReviewRun(previous): %v (found=%v)", err, found)
	}
	if prev.SupersededBy != fresh {
		t.Fatalf("previous review superseded_by = %q, want %q", prev.SupersededBy, fresh)
	}

	// The projection tells the three states apart.
	state := q.repairState(t)
	if state.Active {
		t.Fatalf("repair still reports active: %+v", state)
	}
	if !state.Quiescent {
		t.Fatalf("repair does not report quiescent: %+v", state)
	}
	if state.Attempt != q.generation {
		t.Fatalf("repair attempt = %d, want %d", state.Attempt, q.generation)
	}

	// No branch-lock leak: nothing this run ceded is still out.
	assertNoCededBranchOutstanding(t, q)
}

// Every boundary, restarted, still produces exactly one of everything.
func TestQuiescentRepairFoldAndFreshReviewSurviveRestartsExactlyOnce(t *testing.T) {
	q := newQuiescenceCase(t)
	q.arriveHeadB(t)
	launchesBefore := q.rl.launchCalls

	// Restart before the fold, between the fold and the review, and after it.
	for i := 0; i < 4; i++ {
		q.restart()
		q.reconcileOnly(t)
		q.detail()
	}

	if n := q.countPhase("workflow_repair_quiescent"); n != 1 {
		t.Fatalf("workflow_repair_quiescent rows = %d, want exactly 1 across restarts", n)
	}
	if n := q.countPhase("human_applied_fix_observed"); n != 1 {
		t.Fatalf("human_applied_fix_observed = %d, want exactly 1 across restarts", n)
	}
	if got := q.rl.launchCalls - launchesBefore; got != 1 {
		t.Fatalf("reviewer launches = %d, want exactly 1 across restarts", got)
	}
	if n := q.countPhase("workflow_repair_dispatched"); n != 2 {
		t.Fatalf("repair dispatches = %d, want still 2: a restart must not buy a generation", n)
	}
}

// Two reconciles racing produce one transition, because the fold is idempotent
// over its own ledger row and the adoption is idempotent over the fingerprint.
func TestTwoConcurrentReconcilesProduceOneQuiescenceAndOneReview(t *testing.T) {
	q := newQuiescenceCase(t)
	q.arriveHeadB(t)
	launchesBefore := q.rl.launchCalls

	second := q.newCoordinatorOverSameStore()
	q.reconcileOnly(t)
	if err := second.Reconcile(q.ctx); err != nil {
		t.Fatalf("Reconcile (second daemon): %v", err)
	}

	if n := q.countPhase("workflow_repair_quiescent"); n != 1 {
		t.Fatalf("workflow_repair_quiescent rows = %d, want exactly 1", n)
	}
	if n := q.countPhase("human_applied_fix_observed"); n != 1 {
		t.Fatalf("human_applied_fix_observed = %d, want exactly 1", n)
	}
	if got := q.rl.launchCalls - launchesBefore; got != 1 {
		t.Fatalf("reviewer launches = %d, want exactly 1", got)
	}
}

// ---------------------------------------------------------------------------
// The negatives. Each removes ONE fact and requires the answer to flip.
// ---------------------------------------------------------------------------

// (5) a session of the repair that is still a live writer.
func TestRepairWithALiveWriterIsNotQuiescent(t *testing.T) {
	q := newQuiescenceCase(t)
	q.arriveHeadB(t)
	// The repair owns a session that spoke a moment ago.
	sess := attachSessionToRepairWorkStep(t, q)
	touchSession(t, q, sess, domain.ActivityActive, q.clk.Now())

	q.reconcileOnly(t)

	assertRepairStillBlocks(t, q, "a live writer")
}

// (4) a HELD capacity claim representing a live mutating execution.
func TestRepairHoldingAMutatingCapacityClaimIsNotQuiescent(t *testing.T) {
	q := newQuiescenceCase(t)
	q.arriveHeadB(t)
	seedHeldCapacityClaim(t, q, domain.ExecutionKindWorker)

	q.reconcileOnly(t)

	assertRepairStillBlocks(t, q, "a held worker slot")
}

// (7) a dispatch still queued in the repair's own outbox.
func TestRepairWithAPendingDispatchIsNotQuiescent(t *testing.T) {
	q := newQuiescenceCase(t)
	q.arriveHeadB(t)
	seedPendingOutboxEntry(t, q)

	q.reconcileOnly(t)

	assertRepairStillBlocks(t, q, "a pending dispatch")
}

// (2) an automatic transition AO still owes the repair.
func TestRepairWithAScheduledWakeIsNotQuiescent(t *testing.T) {
	q := newQuiescenceCase(t)
	q.arriveHeadB(t)
	if _, err := q.wake.Schedule(q.ctx, domain.WorkflowRunID(q.repairRunID), nil,
		wake.ReasonTransientRetry, nil); err != nil {
		t.Fatalf("Schedule wake: %v", err)
	}

	q.reconcileOnly(t)

	assertRepairStillBlocks(t, q, "a pending automatic transition")
}

// (3) a step of the repair authorized to execute. Also the ambiguity case: the
// repair's run row says needs_attention while its own work step says running,
// and AO must believe the more dangerous of the two.
func TestRepairWithAnExecutingStepIsNotQuiescent(t *testing.T) {
	q := newQuiescenceCase(t)
	q.arriveHeadB(t)
	step := repairWorkStep(t, q)
	if _, err := q.store.UpdateWorkflowStepState(q.ctx, step.ID, step.State, domain.WorkflowStepReady, q.clk.Now()); err != nil {
		t.Fatalf("make the repair's work step ready: %v", err)
	}

	q.reconcileOnly(t)

	assertRepairStillBlocks(t, q, "a step authorized to execute")
}

// (8) a stale generation cannot be what a fold acts on, and cannot touch the
// parent. Generation 1 completed long ago; only generation 2 is current, and a
// fold must never be recorded against 1.
func TestAStaleRepairGenerationCannotFoldOrTouchTheParent(t *testing.T) {
	q := newQuiescenceCase(t)
	q.arriveHeadB(t)

	q.reconcileOnly(t)

	quiescent := map[int]string{}
	cps, err := q.store.ListWorkflowCheckpoints(q.ctx, q.runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, cp := range cps {
		if cp.DurablePhase != "workflow_repair_quiescent" {
			continue
		}
		var body struct {
			Generation  int    `json:"generation"`
			RepairRunID string `json:"repairRunId"`
		}
		if err := json.Unmarshal([]byte(cp.RetryState), &body); err != nil {
			t.Fatalf("decode quiescence record: %v", err)
		}
		quiescent[body.Generation] = body.RepairRunID
	}
	if _, stale := quiescent[1]; stale {
		t.Fatalf("generation 1 was folded as quiescent; only the current generation may be: %v", quiescent)
	}
	if got := quiescent[2]; got != q.repairRunID {
		t.Fatalf("quiescence for generation 2 names repair run %q, want %q", got, q.repairRunID)
	}
}

// Provenance ambiguity is still refused after a fold: the repair no longer
// blocks, and the ORIGIN's own rules still do. A fold clears a repair, never a
// proof the origin owes.
func TestAFoldedRepairDoesNotWeakenTheOriginsOwnProvenanceRefusal(t *testing.T) {
	q := newQuiescenceCase(t)
	q.moveHead(incidentHeadResolving)
	// Deliberately NOT quiet: the origin's own worker may still be delivering,
	// so the change's provenance is ambiguous.
	touchSession(t, q, originWorkSession(t, q), domain.ActivityActive, q.clk.Now())
	launchesBefore := q.rl.launchCalls

	q.reconcileOnly(t)

	if n := q.countPhase("workflow_repair_quiescent"); n != 1 {
		t.Fatalf("workflow_repair_quiescent rows = %d, want 1: the repair itself is quiescent", n)
	}
	if n := q.countPhase("human_applied_fix_observed"); n != 0 {
		t.Fatalf("human_applied_fix_observed = %d, want 0 for ambiguous provenance", n)
	}
	if got := q.rl.launchCalls - launchesBefore; got != 0 {
		t.Fatalf("reviewer launches = %d, want 0", got)
	}
	if st := q.detail().Run.State; st != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", st)
	}
}

// ---------------------------------------------------------------------------
// Assertions and seeding used by the cases above.
// ---------------------------------------------------------------------------

func assertRepairStillBlocks(t *testing.T, q *quiescenceCase, what string) {
	t.Helper()
	if n := q.countPhase("workflow_repair_quiescent"); n != 0 {
		t.Fatalf("the repair was folded quiescent despite %s\nphases: %v", what, q.phases())
	}
	if n := q.countPhase("human_applied_fix_observed"); n != 0 {
		t.Fatalf("the origin adopted a new head while the repair still had %s", what)
	}
	state := q.repairState(t)
	if !state.Active {
		t.Fatalf("repair does not report active despite %s: %+v", what, state)
	}
	if state.Quiescent {
		t.Fatalf("repair reports quiescent despite %s: %+v", what, state)
	}
	if state.QuiescenceReason == "" {
		t.Fatalf("AO refused quiescence without saying why (%s)", what)
	}
}

func assertNoCededBranchOutstanding(t *testing.T, q *quiescenceCase) {
	t.Helper()
	cps, err := q.store.ListWorkflowCheckpoints(q.ctx, q.runID)
	if err != nil {
		t.Fatal(err)
	}
	outstanding := map[string]bool{}
	for _, cp := range cps {
		var rec struct {
			LockID string `json:"lockId"`
		}
		switch cp.DurablePhase {
		case "branch_lock_ceded_to_repair":
			if json.Unmarshal([]byte(cp.RetryState), &rec) == nil && rec.LockID != "" {
				outstanding[rec.LockID] = true
			}
		case "branch_lock_returned_from_repair":
			if json.Unmarshal([]byte(cp.RetryState), &rec) == nil {
				delete(outstanding, rec.LockID)
			}
		}
	}
	if len(outstanding) != 0 {
		t.Fatalf("branch locks still ceded to a folded repair: %v", outstanding)
	}
}

func repairWorkStep(t *testing.T, q *quiescenceCase) domain.WorkflowStep {
	t.Helper()
	for _, step := range listSteps(t, q, q.repairRunID) {
		if step.Kind == domain.WorkflowStepWork {
			return step
		}
	}
	t.Fatalf("repair run %s has no work step", q.repairRunID)
	return domain.WorkflowStep{}
}

// attachSessionToRepairWorkStep gives the repair a session it owns a runtime
// through, which is what clause (5) reads.
func attachSessionToRepairWorkStep(t *testing.T, q *quiescenceCase) domain.SessionID {
	t.Helper()
	now := q.clk.Now()
	rec, err := q.store.CreateSession(q.ctx, domain.SessionRecord{
		ProjectID: "agent-orchestrator", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := q.store.UpdateWorkflowStepSession(q.ctx, repairWorkStep(t, q).ID, string(rec.ID), now); err != nil {
		t.Fatalf("attach session to the repair's work step: %v", err)
	}
	return rec.ID
}

func originWorkSession(t *testing.T, q *quiescenceCase) domain.SessionID {
	t.Helper()
	for _, step := range listSteps(t, q, q.runID) {
		if step.Kind == domain.WorkflowStepWork && step.SessionID != nil {
			return domain.SessionID(*step.SessionID)
		}
	}
	t.Fatal("the origin has no worker session")
	return ""
}

func touchSession(t *testing.T, q *quiescenceCase, id domain.SessionID, state domain.ActivityState, at time.Time) {
	t.Helper()
	rec, found, err := q.store.GetSession(q.ctx, id)
	if err != nil || !found {
		t.Fatalf("GetSession(%s): %v (found=%v)", id, err, found)
	}
	rec.Activity = domain.Activity{State: state, LastActivityAt: at}
	rec.TurnCompletedAt = at
	rec.UpdatedAt = at
	if err := q.store.UpdateSession(q.ctx, rec); err != nil {
		t.Fatalf("UpdateSession(%s): %v", id, err)
	}
}

func seedHeldCapacityClaim(t *testing.T, q *quiescenceCase, kind domain.ExecutionKind) {
	t.Helper()
	// Placed on a step that has NOT finished, at that step's current dispatch
	// generation. A claim on a completed step, or on no step at all, is a stale
	// claim the ordinary capacity reconcile correctly releases — so seeding one
	// of those would build a negative that dissolves before it is asked.
	step := refreshStep(t, q, q.repairRunID, domain.WorkflowStepVerify)
	key := "cap:" + string(kind) + ":quiescence-test:gen0"
	if enqueued, err := q.store.EnqueueCapacityClaim(q.ctx, domain.CapacityClaim{
		ID: "cc-quiescence", Kind: kind, State: domain.CapacityClaimQueued,
		WorkflowRunID: q.repairRunID, WorkflowStepID: step.ID,
		DispatchKey: key, ProjectID: "agent-orchestrator",
		LifecycleGeneration: 0, Priority: domain.PriorityForKind(kind),
		EnqueuedAt: q.clk.Now(), UpdatedAt: q.clk.Now(),
	}); err != nil {
		t.Fatalf("EnqueueCapacityClaim: %v", err)
	} else if !enqueued {
		t.Fatal("the fixture could not enqueue a capacity claim")
	}
	granted, err := q.store.AcquireCapacity(q.ctx, key, 0, domain.CapacityLimits{}.Normalize(), kind, q.clk.Now())
	if err != nil {
		t.Fatalf("AcquireCapacity: %v", err)
	}
	if !granted {
		t.Fatal("the fixture could not grant a capacity claim, so the negative it sets up is not represented")
	}
}

func seedPendingOutboxEntry(t *testing.T, q *quiescenceCase) {
	t.Helper()
	step := repairWorkStep(t, q)
	stepID := step.ID
	if _, _, err := q.store.EnqueueWorkflowOutboxEntry(q.ctx, domain.WorkflowOutboxEntry{
		ID: "wob-quiescence", WorkflowRunID: q.repairRunID, WorkflowStepID: &stepID,
		CommandType: domain.WorkflowOutboxSpawnWorkerSession, IdempotencyKey: "quiescence-pending",
		Payload: "{}", Status: domain.WorkflowOutboxPending, CreatedAt: q.clk.Now(),
	}); err != nil {
		t.Fatalf("EnqueueWorkflowOutboxEntry: %v", err)
	}
}

// moveStepRow walks a step to a state through the transition guard, so the
// fixture can only build states the schema itself permits.
func moveStepRow(t *testing.T, q *quiescenceCase, step domain.WorkflowStep, to domain.WorkflowStepState) {
	t.Helper()
	if step.State == to {
		return
	}
	if _, err := q.store.UpdateWorkflowStepState(q.ctx, step.ID, step.State, to, q.clk.Now()); err != nil {
		t.Fatalf("move %s step %s -> %s: %v", step.Kind, step.State, to, err)
	}
}

func refreshStep(t *testing.T, q *quiescenceCase, runID string, kind domain.WorkflowStepKind) domain.WorkflowStep {
	t.Helper()
	for _, step := range listSteps(t, q, runID) {
		if step.Kind == kind {
			return step
		}
	}
	t.Fatalf("run %s has no %s step", runID, kind)
	return domain.WorkflowStep{}
}

// repairOutcomesFor folds one ledger phase into generation -> outcome.
func repairOutcomesFor(t *testing.T, q *quiescenceCase, phase string) map[int]string {
	t.Helper()
	out := map[int]string{}
	cps, err := q.store.ListWorkflowCheckpoints(q.ctx, q.runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, cp := range cps {
		if cp.DurablePhase != phase {
			continue
		}
		var body struct {
			Generation int    `json:"generation"`
			Outcome    string `json:"outcome"`
		}
		if json.Unmarshal([]byte(cp.RetryState), &body) == nil && body.Generation > 0 {
			out[body.Generation] = body.Outcome
		}
	}
	return out
}
