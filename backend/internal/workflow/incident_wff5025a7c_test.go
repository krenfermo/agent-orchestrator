package workflow_test

// incident_wff5025a7c_test.go — the fossil-authority regression, reproduced.
//
// THE DURABLE STATE, from ~/.ao/data (repair generation 2 of wf-724a1e97):
//
//	run       wf-f5025a7c   needs_attention   fix_no_verifiable_change
//	steps     plan:completed  work:completed(agent-orchestrator-54)
//	          review:waiting  fix:waiting  verify:pending  advance:pending
//	capacity  reviewer:held
//	          cap:reviewer:workflow-step-review:wfs-79a90a65:cycle1:codex:gen1
//	          — the slot for review run 3bf56007, which CONCLUDED
//	            (changes_requested) at 01:51:18
//	outbox    spawn_worker_session:dispatched
//	          workflow-repair:wf-f5025a7c:475174af8f1f8e1ab2f56e1175dfb0f3:gen1
//	          — a repair LAUNCH claim whose run wf-c4c84f52 exists
//	session   agent-orchestrator-54  idle  is_terminated=0  instance $58
//
// The quiescence proof refused this, correctly: two of those rows say "an
// execution may be running". Both were fossils. Nothing existed to close them,
// so the origin stayed shut behind a repair that had finished hours earlier.
//
// What is pinned here is that the CLOSING is proof-bound. The negatives are the
// substance: a reviewer still running keeps its slot, an unproven launch keeps
// its row, and neither is ever tidied away to make a fold possible.

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// The real identifiers, so the rows this test writes read against the ones it
// reproduces.
const (
	incidentReviewCycle1Verdict = "the repair's own reviewer asked for changes"
	incidentRepairEvidence      = "475174af8f1f8e1ab2f56e1175dfb0f3"
)

// fossilCase is the quiescence fixture plus the three authorities the real
// repair run was still holding.
type fossilCase struct {
	*quiescenceCase
	repairReviewStepID string
	repairReviewRunID  string
	repairWorkSession  domain.SessionID
	nestedRepairRunID  string
	reviewerClaimKey   string
	launchClaimKey     string
}

// newFossilCase seeds them. Every row is written through the same store method
// the production path writes it with, so the negatives below can remove one
// fact without the fixture quietly repairing it.
// fossilOptions varies ONE fact at a time, so a negative removes evidence
// rather than being handed a differently-shaped fixture.
type fossilOptions struct {
	// reviewerStillRunning leaves the repair's review run with no verdict and
	// status `running`: a reviewer that may be working right now.
	reviewerStillRunning bool
	// launchProof selects what the ledger says about the launch claim:
	// "completed" (an intent naming a run that exists), "none" (no intent at
	// all), "missing_run" (an intent naming a run that does not exist).
	launchProof string
}

func newFossilCase(t *testing.T) *fossilCase {
	t.Helper()
	return newFossilCaseWith(t, fossilOptions{launchProof: "completed"})
}

func newFossilCaseWith(t *testing.T, opts fossilOptions) *fossilCase {
	t.Helper()
	q := newQuiescenceCase(t)
	f := &fossilCase{quiescenceCase: q}

	f.repairReviewStepID = refreshStep(t, q, q.repairRunID, domain.WorkflowStepReview).ID
	f.repairWorkSession = attachSessionToRepairWorkStep(t, q)
	// The worker went idle after its fix cycle and has said nothing since: not
	// terminated, runtime preserved, and provably not delivering.
	touchSession(t, q, f.repairWorkSession, domain.ActivityIdle, q.clk.Now().Add(-time.Hour))
	seedRuntimeIncarnation(t, q, f.repairWorkSession, "$58")

	f.repairReviewRunID = f.seedRepairReview(t, opts.reviewerStillRunning)
	f.reviewerClaimKey = f.seedReviewerCapacityClaim(t, 1)
	f.nestedRepairRunID, f.launchClaimKey = f.seedLaunchClaim(t, 1, opts.launchProof)
	return f
}

// seedRepairReview gives the repair's review step its review run: concluded
// with changes_requested (the exact shape 3bf56007 has), or still running with
// no verdict when the caller is testing a live reviewer.
func (f *fossilCase) seedRepairReview(t *testing.T, stillRunning bool) string {
	t.Helper()
	q := f.quiescenceCase
	harness := domain.ReviewerHarness("codex")
	if err := q.store.UpsertReview(q.ctx, domain.Review{
		ID: "rev-fossil", SessionID: f.repairWorkSession, ProjectID: "agent-orchestrator",
		Harness: harness, CreatedAt: q.clk.Now(), UpdatedAt: q.clk.Now(),
	}); err != nil {
		t.Fatalf("UpsertReview: %v", err)
	}
	id := "rr-fossil-cycle1"
	run := domain.ReviewRun{
		ID: id, ReviewID: "rev-fossil", SessionID: f.repairWorkSession, Harness: harness,
		TargetSHA: "1d9063aa9b4886ff3ca6ee7f2495730101264b15b4842503e6bdb429ef35fba5",
		Status:    domain.ReviewRunComplete, Verdict: domain.VerdictChangesRequested,
		Body: incidentReviewCycle1Verdict, CreatedAt: q.clk.Now(),
	}
	if stillRunning {
		run.Status = domain.ReviewRunRunning
		run.Verdict = ""
		run.Body = ""
	}
	if err := q.store.InsertReviewRun(q.ctx, run); err != nil {
		t.Fatalf("InsertReviewRun: %v", err)
	}
	if _, err := q.store.SetWorkflowStepReviewRun(q.ctx, f.repairReviewStepID, id, q.clk.Now()); err != nil {
		t.Fatalf("bind the repair's review run: %v", err)
	}
	return id
}

// seedReviewerCapacityClaim holds the slot that review paid for.
func (f *fossilCase) seedReviewerCapacityClaim(t *testing.T, cycle int64) string {
	t.Helper()
	q := f.quiescenceCase
	key := fmt.Sprintf("cap:reviewer:workflow-step-review:%s:cycle%d:codex:gen%d",
		f.repairReviewStepID, cycle, cycle)
	if enqueued, err := q.store.EnqueueCapacityClaim(q.ctx, domain.CapacityClaim{
		ID: fmt.Sprintf("cap-fossil-reviewer-%d", cycle), Kind: domain.ExecutionKindReviewer,
		State: domain.CapacityClaimQueued, WorkflowRunID: q.repairRunID,
		WorkflowStepID: f.repairReviewStepID, DispatchKey: key, ProjectID: "agent-orchestrator",
		LifecycleGeneration: cycle, Priority: domain.PriorityForKind(domain.ExecutionKindReviewer),
		EnqueuedAt: q.clk.Now(), UpdatedAt: q.clk.Now(),
	}); err != nil {
		t.Fatalf("EnqueueCapacityClaim: %v", err)
	} else if !enqueued {
		t.Fatal("the fixture could not enqueue the reviewer claim")
	}
	granted, err := q.store.AcquireCapacity(q.ctx, key, cycle, domain.CapacityLimits{}.Normalize(),
		domain.ExecutionKindReviewer, q.clk.Now())
	if err != nil || !granted {
		t.Fatalf("AcquireCapacity(reviewer): granted=%v err=%v", granted, err)
	}
	return key
}

// seedLaunchClaim reproduces the single-flight repair-launch claim, and
// whatever the ledger is supposed to say about whether its launch completed.
func (f *fossilCase) seedLaunchClaim(t *testing.T, generation int, proof string) (string, string) {
	t.Helper()
	q := f.quiescenceCase
	nested, err := q.c.CreateRun(q.ctx, "agent-orchestrator", "Repair a stopped AO workflow task (nested)")
	if err != nil {
		t.Fatalf("CreateRun(nested repair): %v", err)
	}
	key := fmt.Sprintf("workflow-repair:%s:%s:gen%d", q.repairRunID, incidentRepairEvidence, generation)
	if _, _, err := q.store.EnqueueWorkflowOutboxEntry(q.ctx, domain.WorkflowOutboxEntry{
		ID: fmt.Sprintf("wfo-fossil-launch-%d", generation), WorkflowRunID: q.repairRunID,
		CommandType:    domain.WorkflowOutboxSpawnWorkerSession,
		IdempotencyKey: key,
		Payload:        fmt.Sprintf(`{"repairIntentId":"wfr-fossil","generation":%d}`, generation),
		Status:         domain.WorkflowOutboxPending, CreatedAt: q.clk.Now(),
	}); err != nil {
		t.Fatalf("EnqueueWorkflowOutboxEntry: %v", err)
	}
	if _, err := q.store.UpdateWorkflowOutboxStatus(q.ctx, fmt.Sprintf("wfo-fossil-launch-%d", generation),
		domain.WorkflowOutboxPending, domain.WorkflowOutboxDispatched, q.clk.Now(), ""); err != nil {
		t.Fatalf("claim the launch slot: %v", err)
	}
	switch proof {
	case "completed":
		f.recordNestedRepairIntent(t, generation, nested.Run.ID)
	case "missing_run":
		f.recordNestedRepairIntent(t, generation, "wf-does-not-exist")
	case "none":
		// Deliberately nothing: the daemon died between the claim and the
		// launch, and AO cannot tell that from a launch that finished.
	default:
		t.Fatalf("unknown launch proof %q", proof)
	}
	return nested.Run.ID, key
}

// recordNestedRepairIntent writes the repair-dispatch record the launch writes
// after CreateTaskRun returns — the proof the effect happened.
func (f *fossilCase) recordNestedRepairIntent(t *testing.T, generation int, repairRunID string) {
	t.Helper()
	q := f.quiescenceCase
	intent := domain.RepairIntent{
		ID: "wfr-fossil", WorkflowRunID: q.repairRunID, TargetRunID: q.repairRunID,
		ConditionReason: workflowcore.ReasonFixNoVerifiableChange,
		EvidenceDigest:  incidentRepairEvidence, Generation: generation,
		ProjectID: "agent-orchestrator", RepairRunID: repairRunID, At: q.clk.Now(),
	}
	payload, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.store.CreateWorkflowCheckpoint(q.ctx, domain.WorkflowCheckpoint{
		ID: fmt.Sprintf("wfc-fossil-intent-%d", generation), WorkflowRunID: q.repairRunID,
		ProjectID: "agent-orchestrator", DurablePhase: "workflow_repair_dispatched",
		NextAction:     fmt.Sprintf("repair generation %d dispatched as run %s", generation, repairRunID),
		PayloadVersion: "v1", RetryState: string(payload), CreatedAt: q.clk.Now(),
	}); err != nil {
		t.Fatalf("record the nested repair intent: %v", err)
	}
}

// seedRuntimeIncarnation gives a session the exact incarnation the real one has.
func seedRuntimeIncarnation(t *testing.T, q *quiescenceCase, id domain.SessionID, instance string) {
	t.Helper()
	rec, found, err := q.store.GetSession(q.ctx, id)
	if err != nil || !found {
		t.Fatalf("GetSession(%s): %v (found=%v)", id, err, found)
	}
	rec.Metadata.RuntimeHandleID = string(id)
	rec.Metadata.RuntimeInstanceID = instance
	rec.Metadata.RuntimeOwnerToken = "ao-session:" + string(id) + ":3c4814df"
	rec.Metadata.RuntimeLaunchID = "3c4814df"
	rec.UpdatedAt = q.clk.Now()
	if err := q.store.UpdateSession(q.ctx, rec); err != nil {
		t.Fatalf("UpdateSession(%s): %v", id, err)
	}
}

func (f *fossilCase) capacityState(t *testing.T, key string) domain.CapacityClaimState {
	t.Helper()
	claim, found, err := f.store.GetCapacityClaim(f.ctx, key)
	if err != nil || !found {
		t.Fatalf("GetCapacityClaim(%s): %v (found=%v)", key, err, found)
	}
	return claim.State
}

func (f *fossilCase) outboxState(t *testing.T, key string) domain.WorkflowOutboxStatus {
	t.Helper()
	entries, err := f.store.ListWorkflowOutboxByRun(f.ctx, f.repairRunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IdempotencyKey == key {
			return e.Status
		}
	}
	t.Fatalf("no outbox entry with key %q", key)
	return ""
}

// ---------------------------------------------------------------------------
// The real case, end to end.
// ---------------------------------------------------------------------------

func TestIncidentWFF5025A7C_FossilAuthoritiesAreRetiredAndTheOriginConverges(t *testing.T) {
	f := newFossilCase(t)
	f.arriveHeadB(t)
	launchesBefore := f.rl.launchCalls

	f.reconcileOnly(t)

	// Only what was PROVABLY finished was closed.
	if got := f.capacityState(t, f.reviewerClaimKey); got != domain.CapacityClaimReleased {
		t.Fatalf("reviewer capacity claim = %q, want released: the review it paid for concluded", got)
	}
	if got := f.outboxState(t, f.launchClaimKey); got != domain.WorkflowOutboxAcknowledged {
		t.Fatalf("launch claim = %q, want acknowledged: its launch demonstrably completed", got)
	}
	// The session is untouched: preserving a runtime for a person to inspect is
	// not the same thing as holding write authority.
	rec, found, err := f.store.GetSession(f.ctx, f.repairWorkSession)
	if err != nil || !found {
		t.Fatalf("GetSession: %v (found=%v)", err, found)
	}
	if rec.IsTerminated {
		t.Fatal("the worker session was terminated; a parked run's runtime is preserved for inspection")
	}
	if rec.Metadata.RuntimeInstanceID != "$58" {
		t.Fatalf("runtime incarnation = %q, want it untouched", rec.Metadata.RuntimeInstanceID)
	}
	if n := f.countPhaseOn(t, f.repairRunID, "execution_authority_retired"); n != 1 {
		t.Fatalf("execution_authority_retired rows on the repair = %d, want exactly 1", n)
	}

	// And then the whole point: quiescence, fold, adoption, one fresh review.
	if n := f.countPhase("workflow_repair_quiescent"); n != 1 {
		t.Fatalf("workflow_repair_quiescent = %d, want exactly 1\nphases: %v", n, f.phases())
	}
	if n := f.countPhase("human_applied_fix_observed"); n != 1 {
		t.Fatalf("human_applied_fix_observed = %d, want exactly 1\nphases: %v", n, f.phases())
	}
	if got := f.rl.launchCalls - launchesBefore; got != 1 {
		t.Fatalf("reviewer launches = %d, want exactly 1 fresh authoritative review of B", got)
	}
	fresh := *reviewStepFrom(f.detail()).Step.ReviewRunID
	if fresh == f.reviewOfA {
		t.Fatalf("the origin is still bound to review %s, which judged the old head", f.reviewOfA)
	}
	// No repair generation was bought to get here, and no second review.
	if n := f.countPhase("workflow_repair_dispatched"); n != 2 {
		t.Fatalf("origin repair dispatches = %d, want still 2", n)
	}
}

// Every boundary, restarted: after the capacity release and before the outbox
// reconciliation, after the outbox and before the fold, after the branch return
// and before the fold checkpoint. Each restart re-derives and converges, and the
// totals never move.
func TestIncidentWFF5025A7C_RetirementAndFoldSurviveRestartsExactlyOnce(t *testing.T) {
	f := newFossilCase(t)
	f.arriveHeadB(t)
	launchesBefore := f.rl.launchCalls

	for i := 0; i < 5; i++ {
		f.restart()
		f.reconcileOnly(t)
		f.detail()
	}

	if n := f.countPhaseOn(t, f.repairRunID, "execution_authority_retired"); n != 1 {
		t.Fatalf("execution_authority_retired rows = %d, want exactly 1 across restarts", n)
	}
	if n := f.countPhase("workflow_repair_quiescent"); n != 1 {
		t.Fatalf("workflow_repair_quiescent = %d, want exactly 1 across restarts", n)
	}
	if n := f.countPhase("human_applied_fix_observed"); n != 1 {
		t.Fatalf("human_applied_fix_observed = %d, want exactly 1 across restarts", n)
	}
	if got := f.rl.launchCalls - launchesBefore; got != 1 {
		t.Fatalf("reviewer launches = %d, want exactly 1 across restarts", got)
	}
	if got := f.capacityState(t, f.reviewerClaimKey); got != domain.CapacityClaimReleased {
		t.Fatalf("reviewer claim = %q after restarts, want released once", got)
	}
	if got := f.outboxState(t, f.launchClaimKey); got != domain.WorkflowOutboxAcknowledged {
		t.Fatalf("launch claim = %q after restarts, want acknowledged once", got)
	}
}

func TestIncidentWFF5025A7C_ConcurrentReconcilesRetireOnce(t *testing.T) {
	f := newFossilCase(t)
	f.arriveHeadB(t)
	launchesBefore := f.rl.launchCalls

	second := f.newCoordinatorOverSameStore()
	f.reconcileOnly(t)
	if err := second.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile (second daemon): %v", err)
	}

	if n := f.countPhaseOn(t, f.repairRunID, "execution_authority_retired"); n != 1 {
		t.Fatalf("execution_authority_retired rows = %d, want exactly 1", n)
	}
	if n := f.countPhase("workflow_repair_quiescent"); n != 1 {
		t.Fatalf("workflow_repair_quiescent = %d, want exactly 1", n)
	}
	if got := f.rl.launchCalls - launchesBefore; got != 1 {
		t.Fatalf("reviewer launches = %d, want exactly 1", got)
	}
}

// ---------------------------------------------------------------------------
// Negatives: nothing is closed that AO cannot prove is finished.
// ---------------------------------------------------------------------------

// A reviewer that is still running IS a live execution. Its slot is real.
func TestAReviewerStillRunningKeepsItsCapacitySlot(t *testing.T) {
	f := newFossilCaseWith(t, fossilOptions{reviewerStillRunning: true, launchProof: "completed"})
	f.arriveHeadB(t)

	f.reconcileOnly(t)

	if got := f.capacityState(t, f.reviewerClaimKey); got != domain.CapacityClaimHeld {
		t.Fatalf("reviewer claim = %q, want still held: the reviewer has not concluded", got)
	}
	assertRepairStillBlocks(t, f.quiescenceCase, "a reviewer that is still running")
}

// A launch claim whose effect cannot be shown is left exactly where it is, and
// is never marked failed — that would free its key for a second launch.
func TestAnUnprovenLaunchClaimIsNeverAcknowledged(t *testing.T) {
	// No intent record at all: the daemon may have died between the claim and
	// the launch, so AO cannot tell a finished launch from an interrupted one.
	f := newFossilCaseWith(t, fossilOptions{launchProof: "none"})
	f.arriveHeadB(t)

	f.reconcileOnly(t)

	if got := f.outboxState(t, f.launchClaimKey); got != domain.WorkflowOutboxDispatched {
		t.Fatalf("launch claim = %q, want still dispatched: its effect is unproven", got)
	}
	assertRepairStillBlocks(t, f.quiescenceCase, "an unproven launch claim")
}

// The proof must name a run that EXISTS. An intent pointing at nothing is not
// evidence that a launch completed.
func TestALaunchClaimWhoseRunDoesNotExistIsNeverAcknowledged(t *testing.T) {
	f := newFossilCaseWith(t, fossilOptions{launchProof: "missing_run"})
	f.arriveHeadB(t)

	f.reconcileOnly(t)

	if got := f.outboxState(t, f.launchClaimKey); got != domain.WorkflowOutboxDispatched {
		t.Fatalf("launch claim = %q, want still dispatched: the run it names does not exist", got)
	}
}

// A claim for a review cycle the step is not on describes a dispatch this rule
// cannot see. It is capacity_scheduler.go's business, never this one's.
func TestAReviewerClaimForAnotherCycleIsNotRetiredHere(t *testing.T) {
	f := newFossilCase(t)
	// A second claim, for a cycle this step has never reached.
	future := f.seedReviewerCapacityClaim(t, 7)
	f.arriveHeadB(t)

	f.reconcileOnly(t)

	if got := f.capacityState(t, future); got != domain.CapacityClaimHeld {
		t.Fatalf("out-of-cycle reviewer claim = %q, want untouched by this rule", got)
	}
}

// An ordinary worker dispatch left at `dispatched` is a real crash boundary its
// own recovery owns. This rule must never acknowledge one.
func TestAnOrdinaryDispatchedWorkerCommandIsNeverAcknowledgedHere(t *testing.T) {
	f := newFossilCase(t)
	step := repairWorkStep(t, f.quiescenceCase)
	stepID := step.ID
	if _, _, err := f.store.EnqueueWorkflowOutboxEntry(f.ctx, domain.WorkflowOutboxEntry{
		ID: "wfo-ordinary", WorkflowRunID: f.repairRunID, WorkflowStepID: &stepID,
		CommandType: domain.WorkflowOutboxSpawnWorkerSession,
		// An ordinary worker-dispatch key. It deliberately carries a `:genN`
		// suffix so the ONLY thing standing between it and an acknowledgement
		// is the launch-claim prefix guard: a test that also relied on the key
		// having no generation would pass for a reason it does not name.
		IdempotencyKey: "workflow-step-spawn:" + stepID + ":gen2",
		Payload:        "{}", Status: domain.WorkflowOutboxPending, CreatedAt: f.clk.Now(),
	}); err != nil {
		t.Fatalf("EnqueueWorkflowOutboxEntry: %v", err)
	}
	if _, err := f.store.UpdateWorkflowOutboxStatus(f.ctx, "wfo-ordinary",
		domain.WorkflowOutboxPending, domain.WorkflowOutboxDispatched, f.clk.Now(), ""); err != nil {
		t.Fatalf("dispatch the ordinary command: %v", err)
	}
	f.arriveHeadB(t)

	f.reconcileOnly(t)

	if got := f.outboxState(t, "workflow-step-spawn:"+stepID+":gen2"); got != domain.WorkflowOutboxDispatched {
		t.Fatalf("ordinary worker dispatch = %q, want untouched by the launch-claim rule", got)
	}
	assertRepairStillBlocks(t, f.quiescenceCase, "an ordinary dispatched worker command")
}

// A run that is still moving has no fossil authority by definition, and nothing
// of its is closed.
func TestARunningRepairHasNoAuthorityRetired(t *testing.T) {
	f := newFossilCase(t)
	step := refreshStep(t, f.quiescenceCase, f.repairRunID, domain.WorkflowStepVerify)
	if _, err := f.store.UpdateWorkflowStepState(f.ctx, step.ID, step.State, domain.WorkflowStepReady, f.clk.Now()); err != nil {
		t.Fatalf("make a step ready: %v", err)
	}
	f.arriveHeadB(t)

	f.reconcileOnly(t)

	if got := f.capacityState(t, f.reviewerClaimKey); got != domain.CapacityClaimHeld {
		t.Fatalf("reviewer claim = %q, want held: this run still has a step authorized to execute", got)
	}
	if got := f.outboxState(t, f.launchClaimKey); got != domain.WorkflowOutboxDispatched {
		t.Fatalf("launch claim = %q, want untouched", got)
	}
	if n := f.countPhaseOn(t, f.repairRunID, "execution_authority_retired"); n != 0 {
		t.Fatalf("execution_authority_retired rows = %d, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

func (f *fossilCase) countPhaseOn(t *testing.T, runID, phase string) int {
	t.Helper()
	cps, err := f.store.ListWorkflowCheckpoints(f.ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, cp := range cps {
		if cp.DurablePhase == phase {
			n++
		}
	}
	return n
}
