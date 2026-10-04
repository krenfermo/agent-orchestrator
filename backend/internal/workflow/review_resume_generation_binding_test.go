package workflow_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// review_resume_generation_binding_test.go — a human resume may act on the
// failed outbox generation it OBSERVED, and on no other.
//
// The hole these close: `reviewer_launch_error` carried no outbox correlation,
// so the resume resolved "the newest launch error on this step" and treated it
// as the failure being resumed. One outbox row is reused across retries
// (failed -> pending -> failed under one idempotency key), so that reading
// cannot tell two failures of the same claim apart:
//
//	ContinueRun A observes failure F1 and is delayed.
//	ContinueRun B resumes F1, opens epoch 2, reopens the entry, dispatches,
//	and the launch fails again -> failure F2, entry failed once more.
//	ContinueRun A finally runs. The entry is failed, and "newest error on the
//	step" is F2 — so A recorded a reset for F2, won the failed->pending swap,
//	and opened epoch 3.
//
// A stale duplicate human action, opening a fresh budget epoch against a
// failure no person ever looked at.

// --- fixture helpers -------------------------------------------------------

// resetEpochs lists the epoch each durable reset opened.
func (f *budgetFixture) resetEpochs() []int {
	f.t.Helper()
	var out []int
	for _, cp := range f.checkpoints() {
		if cp.DurablePhase != "reviewer_launch_human_retry" {
			continue
		}
		var rec struct {
			Epoch int `json:"epoch"`
		}
		if err := json.Unmarshal([]byte(cp.RetryState), &rec); err != nil {
			f.t.Fatalf("reset record %s is undecodable: %v", cp.ID, err)
		}
		out = append(out, rec.Epoch)
	}
	return out
}

// launchErrorRecord is the decoded correlation a launch-failure record carries.
type launchErrorRecord struct {
	ID             string
	OutboxID       string `json:"outboxId"`
	IdempotencyKey string `json:"idempotencyKey"`
	StepID         string `json:"stepId"`
	Cycle          int    `json:"cycle"`
	Epoch          int    `json:"epoch"`
	Attempt        int    `json:"attempt"`
}

func (f *budgetFixture) launchErrorRecords() []launchErrorRecord {
	f.t.Helper()
	var out []launchErrorRecord
	for _, cp := range f.checkpoints() {
		if cp.DurablePhase != "reviewer_launch_error" {
			continue
		}
		var rec launchErrorRecord
		if err := json.Unmarshal([]byte(cp.RetryState), &rec); err != nil {
			f.t.Fatalf("launch-failure record %s is undecodable: %v", cp.ID, err)
		}
		rec.ID = cp.ID
		out = append(out, rec)
	}
	return out
}

// seedLaunchError places a launch-failure record on the ledger by hand, so a
// test can state exactly which outbox generation it belongs to.
func (f *budgetFixture) seedLaunchError(id, outboxID, key string, cycle, epoch, attempt int) {
	f.t.Helper()
	step := f.reviewStepValue()
	if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
		ID: id, WorkflowRunID: f.runID, WorkflowStepID: &step.ID, ProjectID: "proj-1",
		DurablePhase: "reviewer_launch_error", PayloadVersion: "v1",
		RetryState: fmt.Sprintf(
			`{"cycle":%d,"attempt":%d,"class":"transient","certainty":"inferred","retryable":false,`+
				`"stage":"launch","harness":"codex","targetSha":"sha","error":"seeded",`+
				`"outboxId":%q,"idempotencyKey":%q,"stepId":%q,"epoch":%d}`,
			cycle, attempt, outboxID, key, step.ID, epoch),
		CreatedAt: f.clk.Now(),
	}); err != nil {
		f.t.Fatalf("seed launch error: %v", err)
	}
}

// observe captures the failed generation a caller is looking at right now.
func (f *budgetFixture) observe(entry domain.WorkflowOutboxEntry) workflowcore.ReviewLaunchGenerationForTest {
	f.t.Helper()
	run, _, _ := f.store.GetWorkflowRun(f.ctx, f.runID)
	obs, ok := f.c.ObserveFailedReviewLaunchGenerationForTest(f.ctx, run, f.reviewStepValue(), entry)
	if !ok {
		f.t.Fatal("no failed launch generation could be observed for the entry")
	}
	return obs
}

// resumeFrom runs a resume that was DELAYED after its observation.
func (f *budgetFixture) resumeFrom(entry domain.WorkflowOutboxEntry, obs workflowcore.ReviewLaunchGenerationForTest) bool {
	f.t.Helper()
	run, _, _ := f.store.GetWorkflowRun(f.ctx, f.runID)
	return f.c.ResumeReviewLaunchFromGenerationForTest(f.ctx, run, f.reviewStepValue(), entry, obs)
}

// burnAFreshEpoch resumes the current failure and spends the whole epoch it
// opens, ending in a NEW durable failure of the same claim.
func (f *budgetFixture) burnAFreshEpoch() {
	f.t.Helper()
	for i := 0; i < workflowcore.MaxReviewerLaunchAttemptsForTest; i++ {
		f.failLaunch()
	}
	if got := f.store.outbox[f.reviewOutboxKey()].Status; got != domain.WorkflowOutboxFailed {
		f.t.Fatalf("precondition: outbox = %q after a spent epoch, want failed", got)
	}
}

// --- 1. THE BLOCKER: the exact Codex race ----------------------------------

func TestResumeBinding_StaleResumeCannotUpgradeItselfToALaterFailure(t *testing.T) {
	f := newBudgetFixture(t)
	stale := f.failedEntry()

	// A looks at the run and is then delayed. F1 is what it saw.
	obsA := f.observe(stale)
	f1 := f.latestLaunchErrorID()
	if obsA.RecordIDForTest() != f1 {
		t.Fatalf("observation names %q, want the current failure %q", obsA.RecordIDForTest(), f1)
	}

	// B resumes F1, opens epoch 2, and spends it — ending in a NEW failure F2 of
	// the same claim, on the same row, at the same step.
	f.burnAFreshEpoch()
	f2 := f.latestLaunchErrorID()
	if f2 == f1 {
		t.Fatal("precondition: the second cycle produced no new launch failure")
	}
	if got := f.resetEpochs(); len(got) != 1 || got[0] != 2 {
		t.Fatalf("reset epochs after B = %v, want exactly [2]", got)
	}
	entry := f.store.outbox[f.reviewOutboxKey()]

	// A finally runs, still holding the F1 snapshot. The row IS failed — but it
	// is failed as F2, a generation nobody asked A about.
	if f.resumeFrom(stale, obsA) {
		t.Fatal("a resume that observed F1 reported that it reopened the launch; " +
			"it upgraded itself to the later failure F2")
	}

	// Nothing moved: no epoch, no reset, no budget, no reopen.
	if got := f.resetEpochs(); len(got) != 1 || got[0] != 2 {
		t.Fatalf("reset epochs = %v after the stale resume, want the [2] B opened", got)
	}
	if got := len(f.resetGenerations()); got != 1 {
		t.Fatalf("%d reset generations after the stale resume, want 1", got)
	}
	if got := f.store.outbox[f.reviewOutboxKey()].Status; got != domain.WorkflowOutboxFailed {
		t.Fatalf("outbox = %q after the stale resume, want it left failed as F2", got)
	}

	// And no budget was handed back: epoch 2 is still spent.
	run, _, _ := f.store.GetWorkflowRun(f.ctx, f.runID)
	step := f.reviewStepValue()
	remaining, _, berr := f.c.ReviewLaunchBudgetRemainsForTest(f.ctx, run, step, entry)
	if berr != nil {
		t.Fatalf("budget gate: %v", berr)
	}
	if remaining {
		t.Fatal("the stale resume handed the budget back; epoch 2 was spent")
	}
	if _, err := f.c.AllocateReviewLaunchAttemptForTest(f.ctx, run, step, entry, 1); err == nil {
		t.Fatal("an attempt was allocated over the spent epoch the stale resume did not own")
	}
}

// --- 2. the reset is correlated to the EXACT claim and failure --------------

func TestResumeBinding_ResetNamesTheExactOutboxGeneration(t *testing.T) {
	f := newBudgetFixture(t)
	stale := f.failedEntry()
	f1 := f.latestLaunchErrorID()

	// The failure record itself carries the correlation, durably.
	errs := f.launchErrorRecords()
	if len(errs) == 0 {
		t.Fatal("no launch-failure records were written")
	}
	last := errs[len(errs)-1]
	switch {
	case last.ID != f1:
		t.Fatalf("newest failure record = %q, want %q", last.ID, f1)
	case last.OutboxID != stale.ID:
		t.Fatalf("failure record names outbox %q, want %q", last.OutboxID, stale.ID)
	case last.IdempotencyKey != stale.IdempotencyKey:
		t.Fatalf("failure record names claim %q, want %q", last.IdempotencyKey, stale.IdempotencyKey)
	case last.StepID != f.reviewStepValue().ID:
		t.Fatalf("failure record names step %q, want %q", last.StepID, f.reviewStepValue().ID)
	case last.Cycle <= 0 || last.Attempt <= 0 || last.Epoch <= 0:
		t.Fatalf("failure record carries an incomplete generation: %+v", last)
	}

	if !f.c.ResumeReviewLaunchAfterFailureForTest(
		f.ctx, mustRun(t, f), f.reviewStepValue(), stale) {
		t.Fatal("the resume did not reopen the failed launch")
	}

	want := stale.IdempotencyKey + "|" + f1
	gens := f.resetGenerations()
	if len(gens) != 1 || gens[0] != want {
		t.Fatalf("reset generations = %v, want exactly [%s] — the claim key and the "+
			"exact failure record, not the step's newest error", gens, want)
	}
	// And the reset names the row it was won on.
	for _, cp := range f.checkpoints() {
		if cp.DurablePhase != "reviewer_launch_human_retry" {
			continue
		}
		var rec struct {
			OutboxID       string `json:"outboxId"`
			IdempotencyKey string `json:"idempotencyKey"`
		}
		if err := json.Unmarshal([]byte(cp.RetryState), &rec); err != nil {
			t.Fatalf("reset record %s is undecodable: %v", cp.ID, err)
		}
		if rec.OutboxID != stale.ID || rec.IdempotencyKey != stale.IdempotencyKey {
			t.Fatalf("reset names outbox %q/claim %q, want %q/%q",
				rec.OutboxID, rec.IdempotencyKey, stale.ID, stale.IdempotencyKey)
		}
	}
}

func mustRun(t *testing.T, f *budgetFixture) domain.WorkflowRun {
	t.Helper()
	run, _, _ := f.store.GetWorkflowRun(f.ctx, f.runID)
	return run
}

// --- 3. another generation's failure on the same step is not selectable -----

func TestResumeBinding_AnotherGenerationsFailureIsNeverSelected(t *testing.T) {
	f := newBudgetFixture(t)
	stale := f.failedEntry()
	mine := f.latestLaunchErrorID()

	// A launch failure for a DIFFERENT outbox row and claim on the SAME step,
	// written after mine — exactly what "newest error on this step" would pick.
	f.seedLaunchError("wfc-foreign-error", "wfo-someone-else", "workflow-step-review:other:cycle9:codex", 9, 7, 1)

	obs := f.observe(stale)
	if obs.RecordIDForTest() != mine {
		t.Fatalf("observation resolved to %q, want this row's own failure %q; a foreign "+
			"generation's record was selected because it was newest",
			obs.RecordIDForTest(), mine)
	}

	// And the resume it drives claims this row's generation, not the foreign one.
	if !f.resumeFrom(stale, obs) {
		t.Fatal("the resume did not reopen the failed launch")
	}
	want := stale.IdempotencyKey + "|" + mine
	if gens := f.resetGenerations(); len(gens) != 1 || gens[0] != want {
		t.Fatalf("reset generations = %v, want [%s]", gens, want)
	}
}

// --- 4. a delayed duplicate arriving after F2 writes nothing at all ---------

func TestResumeBinding_DelayedDuplicateAfterANewFailureWritesNothing(t *testing.T) {
	f := newBudgetFixture(t)
	stale := f.failedEntry()
	obsA := f.observe(stale)

	f.burnAFreshEpoch()

	before := len(f.checkpoints())
	launches := f.launcher.launchCalls
	for i := 0; i < 4; i++ {
		if f.resumeFrom(stale, obsA) {
			t.Fatalf("delayed duplicate %d reported a reopen", i)
		}
	}
	if got := len(f.checkpoints()); got != before {
		t.Fatalf("%d checkpoints written by repeated stale resumes, want 0", got-before)
	}
	if got := f.launcher.launchCalls - launches; got != 0 {
		t.Fatalf("%d reviewer launches from stale resumes, want 0", got)
	}
	if got := f.store.outbox[f.reviewOutboxKey()].Status; got != domain.WorkflowOutboxFailed {
		t.Fatalf("outbox = %q, want failed", got)
	}
}

// --- 5. restart: the correlation survives, and a fresh look still works -----

func TestResumeBinding_RestartPreservesTheCorrelation(t *testing.T) {
	f := newBudgetFixture(t)
	stale := f.failedEntry()
	obsA := f.observe(stale)

	f.burnAFreshEpoch()
	f2 := f.latestLaunchErrorID()

	// "Restart": every read below comes from the durable ledger alone. A resume
	// carrying the pre-restart observation still binds to the generation it saw,
	// and still refuses to be promoted to the newest failure.
	if f.resumeFrom(stale, obsA) {
		t.Fatal("after a restart, the stale observation was substituted with the step's newest failure")
	}

	// A person looking at the run NOW observes F2, and may resume that — the
	// binding refuses stale actions, not human ones.
	current := f.store.outbox[f.reviewOutboxKey()]
	obsB := f.observe(current)
	if obsB.RecordIDForTest() != f2 {
		t.Fatalf("a fresh observation names %q, want the current failure %q", obsB.RecordIDForTest(), f2)
	}
	if !f.resumeFrom(current, obsB) {
		t.Fatal("a resume of the CURRENT failed generation was refused")
	}
	if got := f.resetEpochs(); len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("reset epochs = %v, want [2 3]: one per failure a person actually resumed", got)
	}
	if got := f.store.outbox[f.reviewOutboxKey()].Status; got != domain.WorkflowOutboxPending {
		t.Fatalf("outbox = %q after resuming the current generation, want pending", got)
	}
}

// ---- the failed->pending swap is conditioned on the generation, in SQL -----
//
// The Go binding above refuses a resume whose observed failure is no longer
// current. It cannot refuse what happens BETWEEN that check and the write: the
// row can fail again, under the same id and the same status, in the window. So
// the generation is part of the swap itself. These drive the coordinator with a
// competing resume running inside exactly that window.

// reviewGeneration is the stamp a failure of this claim writes onto the row.
func (f *budgetFixture) reviewGeneration(recordID string) string {
	f.t.Helper()
	return f.reviewOutboxKey() + "|" + recordID
}

// 1. THE BLOCKER, at the swap. A validates F1 and is suspended on the CAS; B
// finishes the resume, dispatches, and fails again as F2; A's swap then runs.
func TestResumeCAS_StaleSwapCannotReopenALaterGeneration(t *testing.T) {
	f := newBudgetFixture(t)
	stale := f.failedEntry()
	f1 := f.latestLaunchErrorID()
	if got := f.store.outbox[f.reviewOutboxKey()].FailureGeneration; got != f.reviewGeneration(f1) {
		t.Fatalf("row generation = %q, want F1 %q", got, f.reviewGeneration(f1))
	}

	afterB := -1
	f.store.beforeOutboxCAS = func(_ string, _, next domain.WorkflowOutboxStatus) {
		if next != domain.WorkflowOutboxPending {
			return
		}
		// B runs here: it completes the reopen A claimed, dispatches, and spends
		// the epoch — leaving the row failed as a DIFFERENT generation.
		f.burnAFreshEpoch()
		afterB = f.launcher.launchCalls
	}
	resumed := f.c.ResumeReviewLaunchAfterFailureForTest(f.ctx, mustRun(t, f), f.reviewStepValue(), stale)
	f.store.beforeOutboxCAS = nil

	if afterB < 0 {
		t.Fatal("the competing resume never ran; the race was not exercised")
	}
	if resumed {
		t.Fatal("a swap naming F1 reported that it reopened the entry; it moved F2")
	}

	f2 := f.latestLaunchErrorID()
	if f2 == f1 {
		t.Fatal("precondition: the competing resume produced no new failure")
	}
	entry := f.store.outbox[f.reviewOutboxKey()]
	switch {
	case entry.Status != domain.WorkflowOutboxFailed:
		t.Fatalf("outbox = %q after the stale swap, want it left failed as F2", entry.Status)
	case entry.FailureGeneration != f.reviewGeneration(f2):
		t.Fatalf("row generation = %q, want F2 %q", entry.FailureGeneration, f.reviewGeneration(f2))
	}
	if got := f.launcher.launchCalls; got != afterB {
		t.Fatalf("%d reviewer launches after the stale swap, want 0", got-afterB)
	}
	// One failed generation, one epoch: A claimed F1 before F2 existed, and
	// nothing opened a second.
	if gens := f.resetGenerations(); len(gens) != 1 || gens[0] != f.reviewGeneration(f1) {
		t.Fatalf("reset generations = %v, want exactly [%s]", gens, f.reviewGeneration(f1))
	}
	if got := f.resetEpochs(); len(got) != 1 || got[0] != 2 {
		t.Fatalf("reset epochs = %v, want [2]", got)
	}
}

// 2 + 3. The ordinary resume swaps once, clears the stamp, and repeats no-op.
func TestResumeCAS_MatchingGenerationSwapsExactlyOnce(t *testing.T) {
	f := newBudgetFixture(t)
	stale := f.failedEntry()

	if !f.c.ResumeReviewLaunchAfterFailureForTest(f.ctx, mustRun(t, f), f.reviewStepValue(), stale) {
		t.Fatal("the matching generation did not reopen the entry")
	}
	entry := f.store.outbox[f.reviewOutboxKey()]
	switch {
	case entry.Status != domain.WorkflowOutboxPending:
		t.Fatalf("outbox = %q, want pending", entry.Status)
	case entry.FailureGeneration != "":
		t.Fatalf("the reopened row still carries generation %q", entry.FailureGeneration)
	}

	for i := 0; i < 3; i++ {
		if f.c.ResumeReviewLaunchAfterFailureForTest(f.ctx, mustRun(t, f), f.reviewStepValue(), stale) {
			t.Fatalf("duplicate resume %d reported a reopen", i)
		}
	}
	if got := len(f.resetGenerations()); got != 1 {
		t.Fatalf("%d resets after duplicate resumes, want 1", got)
	}
}

// 4. Restart: a persisted F1 observation cannot move a row that is now F2, and
// the refusal is the store's, not a Go-side check that ran earlier.
func TestResumeCAS_PersistedF1CannotMutateF2AfterRestart(t *testing.T) {
	f := newBudgetFixture(t)
	_ = f.failedEntry()
	staleGeneration := f.reviewGeneration(f.latestLaunchErrorID())

	f.burnAFreshEpoch()
	entry := f.store.outbox[f.reviewOutboxKey()]
	if entry.FailureGeneration == staleGeneration {
		t.Fatal("precondition: the row still carries F1")
	}

	moved, err := f.store.ReopenFailedWorkflowOutboxGeneration(f.ctx, entry.ID, "transient", staleGeneration)
	if err != nil {
		t.Fatalf("stale reopen: %v", err)
	}
	if moved {
		t.Fatal("a persisted F1 resume moved the row after F2 replaced it")
	}
	if got := f.store.outbox[f.reviewOutboxKey()]; got.Status != domain.WorkflowOutboxFailed {
		t.Fatalf("outbox = %q, want failed", got.Status)
	}
}

// 5. A launch failure belonging to another generation on the same step cannot
// satisfy the swap, however new it is.
func TestResumeCAS_ForeignLaunchErrorCannotSatisfyTheSwap(t *testing.T) {
	f := newBudgetFixture(t)
	entry := f.failedEntry()
	f.seedLaunchError("wfc-foreign-cas", "wfo-someone-else", "workflow-step-review:other:cycle9:codex", 9, 7, 1)

	for _, generation := range []string{
		f.reviewGeneration("wfc-foreign-cas"),
		"workflow-step-review:other:cycle9:codex|wfc-foreign-cas",
	} {
		moved, err := f.store.ReopenFailedWorkflowOutboxGeneration(f.ctx, entry.ID, "transient", generation)
		if err != nil {
			t.Fatalf("reopen with %q: %v", generation, err)
		}
		if moved {
			t.Fatalf("generation %q satisfied the swap", generation)
		}
	}
	if got := f.store.outbox[f.reviewOutboxKey()].Status; got != domain.WorkflowOutboxFailed {
		t.Fatalf("outbox = %q, want failed", got)
	}
}

// ---- the claim token: a dispatch may only act on a row it still owns -------
//
// One step earlier than the reopen. A dispatch writes its abandon evidence and
// its reviewer_launch_error F1, then pauses. Recovery releases the claim; a
// second dispatch takes the row. The row is `dispatched` in both worlds, so a
// fail predicate of id + status matched — and the stale dispatch failed a live
// generation, stamping F1 onto a launch that never failed. A human resume of F1
// could then reopen it and start a third dispatch over a running reviewer.

// armOutboxHook installs an outbox-CAS hook that survives unrelated CAS calls:
// the fake clears the hook before invoking it, so a filtered hook has to re-arm
// itself or it is spent on the first transition that is not the one wanted.
func (f *budgetFixture) armOutboxHook(want domain.WorkflowOutboxStatus, act func()) {
	f.t.Helper()
	var arm func()
	arm = func() {
		f.store.beforeOutboxCAS = func(_ string, _, next domain.WorkflowOutboxStatus) {
			if next != want {
				arm()
				return
			}
			act()
		}
	}
	arm()
}

// 1 + 5. THE BLOCKER: D1 pauses at its fail CAS; recovery releases G1 and D2
// claims G2; D1 then tries to fail the row.
func TestDispatchCAS_StaleDispatchCannotFailAReclaimedRow(t *testing.T) {
	f := newBudgetFixture(t)
	key := f.reviewOutboxKey()

	// Spend the budget down so the next failure is the PERMANENT one — the
	// branch that fails the outbox rather than releasing it for a retry.
	for i := 0; i < workflowcore.MaxReviewerLaunchAttemptsForTest-1; i++ {
		f.failLaunch()
	}

	stolen := false
	f.armOutboxHook(domain.WorkflowOutboxFailed, func() {
		// D1 is suspended here, with its abandon evidence and its F1 record
		// already durable. Recovery releases its claim, and D2 takes the row.
		entry := f.store.outbox[key]
		if entry.DispatchGeneration == "" {
			f.t.Fatal("the dispatched row names no claim holder")
		}
		if ok, err := f.store.ReleaseDispatchedWorkflowOutboxGeneration(
			f.ctx, entry.ID, "", entry.DispatchGeneration); err != nil || !ok {
			f.t.Fatalf("recovery release: ok=%v err=%v", ok, err)
		}
		if ok, err := f.store.ClaimWorkflowOutboxDispatch(
			f.ctx, entry.ID, f.clk.Now(), "wfc-authz-d2"); err != nil || !ok {
			f.t.Fatalf("D2 claim: ok=%v err=%v", ok, err)
		}
		stolen = true
	})
	f.failLaunch()
	f.store.beforeOutboxCAS = nil

	if !stolen {
		t.Fatal("the row was never reclaimed; the race was not exercised")
	}
	entry := f.store.outbox[key]
	switch {
	case entry.Status != domain.WorkflowOutboxDispatched:
		t.Fatalf("outbox = %q after the stale fail, want D2 left dispatched", entry.Status)
	case entry.DispatchGeneration != "wfc-authz-d2":
		t.Fatalf("claim token = %q, want D2 still holding it", entry.DispatchGeneration)
	case entry.FailureGeneration != "":
		t.Fatalf("F1 was stamped onto D2's live generation: %q", entry.FailureGeneration)
	}

	// And F1 can reopen nothing: it was never stamped by a winning fail, so
	// there is no failed generation for a human resume to act on.
	if f.c.ResumeReviewLaunchAfterFailureForTest(f.ctx, mustRun(t, f), f.reviewStepValue(), entry) {
		t.Fatal("a human resume reopened a row that no failure ever won")
	}
	if got := len(f.resetGenerations()); got != 0 {
		t.Fatalf("%d budget resets over an unfailed row, want 0", got)
	}
}

// 2 + 3. The owning dispatch fails its own row exactly once, and the stamp is
// its own F1; a duplicate fail from the same (now released) token no-ops.
func TestDispatchCAS_OwningDispatchFailsItsOwnRowOnce(t *testing.T) {
	f := newBudgetFixture(t)
	key := f.reviewOutboxKey()

	held := ""
	f.armOutboxHook(domain.WorkflowOutboxFailed, func() {
		held = f.store.outbox[key].DispatchGeneration
	})
	for i := 0; i < workflowcore.MaxReviewerLaunchAttemptsForTest; i++ {
		f.failLaunch()
	}
	f.store.beforeOutboxCAS = nil

	if held == "" {
		t.Fatal("the failing dispatch held no claim token")
	}
	entry := f.store.outbox[key]
	f1 := f.reviewGeneration(f.latestLaunchErrorID())
	switch {
	case entry.Status != domain.WorkflowOutboxFailed:
		t.Fatalf("outbox = %q, want failed", entry.Status)
	case entry.FailureGeneration != f1:
		t.Fatalf("failure generation = %q, want F1 %q", entry.FailureGeneration, f1)
	case entry.DispatchGeneration != "":
		t.Fatalf("the failed row still names a claim holder: %q", entry.DispatchGeneration)
	}

	for i := 0; i < 3; i++ {
		failed, err := f.store.FailWorkflowOutboxWithGeneration(
			f.ctx, entry.ID, domain.WorkflowOutboxDispatched, f.clk.Now(), "transient", f1, held)
		if err != nil || failed {
			t.Fatalf("duplicate fail %d: ok=%v err=%v", i, failed, err)
		}
	}
}

// 4. Restart: a persisted G1 token cannot fail the row a later G2 now holds.
func TestDispatchCAS_PersistedG1CannotFailG2AfterRestart(t *testing.T) {
	f := newBudgetFixture(t)
	key := f.reviewOutboxKey()

	held := ""
	f.armOutboxHook(domain.WorkflowOutboxFailed, func() {
		held = f.store.outbox[key].DispatchGeneration
	})
	for i := 0; i < workflowcore.MaxReviewerLaunchAttemptsForTest; i++ {
		f.failLaunch()
	}
	f.store.beforeOutboxCAS = nil
	if held == "" {
		t.Fatal("no claim token was ever held")
	}

	// A human resumes, and a NEW dispatch claims the row: G2 owns it now.
	if !f.c.ResumeReviewLaunchAfterFailureForTest(f.ctx, mustRun(t, f), f.reviewStepValue(), f.store.outbox[key]) {
		t.Fatal("the resume did not reopen the entry")
	}
	if ok, err := f.store.ClaimWorkflowOutboxDispatch(
		f.ctx, f.store.outbox[key].ID, f.clk.Now(), "wfc-authz-g2"); err != nil || !ok {
		t.Fatalf("G2 claim: ok=%v err=%v", ok, err)
	}

	// The persisted G1 token, replayed after a restart, moves nothing.
	failed, err := f.store.FailWorkflowOutboxWithGeneration(
		f.ctx, f.store.outbox[key].ID, domain.WorkflowOutboxDispatched, f.clk.Now(),
		"transient", "stale|wfc-launch-error-1", held)
	if err != nil {
		t.Fatalf("stale fail: %v", err)
	}
	if failed {
		t.Fatal("a persisted G1 token failed the row G2 now owns")
	}
	entry := f.store.outbox[key]
	switch {
	case entry.Status != domain.WorkflowOutboxDispatched:
		t.Fatalf("outbox = %q, want G2 left dispatched", entry.Status)
	case entry.DispatchGeneration != "wfc-authz-g2":
		t.Fatalf("claim token = %q, want G2", entry.DispatchGeneration)
	}
}
