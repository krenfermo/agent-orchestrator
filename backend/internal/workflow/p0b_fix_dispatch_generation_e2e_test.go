package workflow_test

// P0-B, fix-cycle dispatch: the crash boundaries, end to end, over the real
// coordinator and the durable rows a restart actually leaves behind.
//
// The unit half of this work is in p0b_fix_generation_internal_test.go — the
// identity, the refusal rules, the recovery disposition table and the CAS
// statements. What is pinned HERE is the property those parts exist to produce:
//
//	however many times AO crashes, restarts and reconciles, the worker session
//	receives one copy of one fix cycle's findings, under one generation, with
//	one attempt row — or AO stops and says exactly what it could not prove.
//
// Every boundary below is expressed as durable state, never as a timing window:
// the rows are rolled back to the instant of the crash and a NEW coordinator is
// built over them, which is what a daemon restart is.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// fixDeliveryRecords returns every promptDeliveryRecord of a given durable
// phase, decoded the way the coordinator itself writes them.
func (f *fixRecoveryFixture) fixDeliveryRecords(phase string) []map[string]any {
	f.t.Helper()
	out := []map[string]any{}
	for _, cp := range f.store.checkpoints[f.runID] {
		if cp.DurablePhase != phase {
			continue
		}
		var rec map[string]any
		if json.Unmarshal([]byte(cp.RetryState), &rec) != nil {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// generationOf reads the dispatch generation token off a delivery record.
func generationOf(rec map[string]any) string {
	gen, ok := rec["generation"].(map[string]any)
	if !ok {
		return ""
	}
	id, _ := gen["id"].(string)
	return id
}

func generationFieldOf(rec map[string]any, field string) any {
	gen, ok := rec["generation"].(map[string]any)
	if !ok {
		return nil
	}
	return gen[field]
}

// fixOutboxEntry returns this run's single fix-step outbox row.
func (f *fixRecoveryFixture) fixOutboxEntry() domain.WorkflowOutboxEntry {
	f.t.Helper()
	var found []domain.WorkflowOutboxEntry
	for _, entry := range f.store.outbox {
		if entry.WorkflowStepID != nil && *entry.WorkflowStepID == f.fixStepID {
			found = append(found, entry)
		}
	}
	if len(found) != 1 {
		f.t.Fatalf("fix outbox entries = %d, want exactly 1", len(found))
	}
	return found[0]
}

// stampFixOutboxGeneration overwrites the claim token on the fix outbox row,
// modelling a row whose ownership does not match the ledger.
func (f *fixRecoveryFixture) stampFixOutboxGeneration(token string) {
	f.t.Helper()
	for key, entry := range f.store.outbox {
		if entry.WorkflowStepID != nil && *entry.WorkflowStepID == f.fixStepID {
			entry.DispatchGeneration = token
			f.store.outbox[key] = entry
		}
	}
}

// fixDeliveryReport is the operator-facing projection for the fix step.
func (f *fixRecoveryFixture) fixDeliveryReport() *workflowcore.FixDeliveryReport {
	f.t.Helper()
	detail, err := f.c.GetRun(context.Background(), f.runID)
	if err != nil {
		f.t.Fatalf("GetRun: %v", err)
	}
	return fixStepFrom(detail).FixDelivery
}

// ---------------------------------------------------------------------------
// a / r: a fresh dispatch, and what it exposes
// ---------------------------------------------------------------------------

// A first fix dispatch delivers once, under a generation that binds every
// dimension requirement 2 names, and the outbox row carries that same claim
// until the acknowledge that ends it.
func TestFreshFixDispatchIsBoundToADurableGeneration(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()

	if f.sender.calls != 1 {
		t.Fatalf("Send calls = %d, want exactly 1", f.sender.calls)
	}
	f.assertOneAttemptOneCycle()

	intents := f.fixDeliveryRecords("fix_dispatch_intent")
	if len(intents) != 1 {
		t.Fatalf("pre-delivery records = %d, want exactly 1", len(intents))
	}
	gen := generationOf(intents[0])
	if gen == "" {
		t.Fatal("the pre-delivery record carries no dispatch generation; recovery would have nothing to adopt")
	}
	// Requirement 2: minted BEFORE the claim and durably binding the whole
	// dispatch. The intent record is written strictly before Send, so a
	// generation visible here is one that existed before any prompt did.
	for _, field := range []string{"workflowRunId", "fixStepId", "reviewRunId", "reviewGeneration", "sessionId", "findingsDigest"} {
		if v, _ := generationFieldOf(intents[0], field).(string); v == "" {
			t.Errorf("the fix generation does not bind %s", field)
		}
	}

	dispatched := f.fixDeliveryRecords("fix_dispatched")
	if len(dispatched) != 1 {
		t.Fatalf("fix_dispatched records = %d, want exactly 1", len(dispatched))
	}
	if got := generationOf(dispatched[0]); got != gen {
		t.Fatalf("the dispatch record names generation %q, the pre-delivery record %q: they must be one identity", got, gen)
	}
	// The attempt row is bound to the generation that opened it, from both
	// sides — requirement 7's "which attempt?".
	attemptID, _ := generationFieldOf(dispatched[0], "fixAttemptId").(string)
	attempts, err := f.store.ListWorkflowAttempts(context.Background(), f.fixStepID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts = %+v err=%v", attempts, err)
	}
	if attemptID != attempts[0].ID {
		t.Fatalf("generation names attempt %q, the step has %q", attemptID, attempts[0].ID)
	}

	// The claim ends with the acknowledge, exactly as it does for the worker
	// path: an acknowledged row is no longer claimable.
	entry := f.fixOutboxEntry()
	if entry.Status != domain.WorkflowOutboxAcknowledged {
		t.Fatalf("outbox status = %q, want acknowledged", entry.Status)
	}
	if entry.DispatchGeneration != "" {
		t.Fatalf("acknowledged row still holds a claim token %q", entry.DispatchGeneration)
	}

	// r: fixDelivery exposes the generation and the review authority it was
	// bound to, so an operator can answer requirement 7 without grepping ~/.ao.
	report := f.fixDeliveryReport()
	if report == nil {
		t.Fatal("no fix delivery report")
	}
	if report.Generation != gen {
		t.Fatalf("fixDelivery.generation = %q, want %q", report.Generation, gen)
	}
	if report.ReviewGeneration == "" {
		t.Fatal("fixDelivery does not expose the review generation the fix was authorized by")
	}
	if report.FindingsDigest == "" || report.SessionID == "" || report.FixAttemptID == "" {
		t.Fatalf("fixDelivery cannot answer which findings/session/attempt: %+v", report)
	}
}

// ---------------------------------------------------------------------------
// b / k: repeated reconcile and repeated restart
// ---------------------------------------------------------------------------

// Requirement 4: the poller re-derives the same cycle every two seconds. It
// must never produce a second prompt, a second attempt or a second generation.
func TestRepeatedReconcileDeliversOneFixPrompt(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()
	f.poll(10)

	if f.sender.calls != 1 {
		t.Fatalf("Send calls after 10 reconcile passes = %d, want exactly 1", f.sender.calls)
	}
	f.assertOneAttemptOneCycle()
	if n := len(f.fixDeliveryRecords("fix_dispatched")); n != 1 {
		t.Fatalf("fix_dispatched records = %d, want exactly 1", n)
	}
}

// Requirement 4's convergence clause, stated as the thing operators actually
// see: the daemon restarting over and over must not accumulate anything.
func TestRepeatedDaemonRestartsAreIdempotent(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()
	sends := f.sender.calls
	gen := generationOf(f.fixDeliveryRecords("fix_dispatched")[0])

	for i := 0; i < 5; i++ {
		f.c = f.restart()
		f.poll(2)
	}

	if f.sender.calls != sends {
		t.Fatalf("Send calls after five restarts = %d, want still %d", f.sender.calls, sends)
	}
	f.assertOneAttemptOneCycle()
	dispatched := f.fixDeliveryRecords("fix_dispatched")
	if len(dispatched) != 1 {
		t.Fatalf("fix_dispatched records = %d, want exactly 1", len(dispatched))
	}
	if got := generationOf(dispatched[0]); got != gen {
		t.Fatalf("the generation changed across restarts: %q -> %q", gen, got)
	}
}

// ---------------------------------------------------------------------------
// e / f: crash before the claim, and after the claim before Send
// ---------------------------------------------------------------------------

// Boundary A: the intent to dispatch exists only as "the review asked for
// changes"; nothing is claimed. A restart here simply dispatches, once.
func TestCrashBeforeClaimDeliversExactlyOnceAfterRestart(t *testing.T) {
	f := newFixRecoveryFixture(t)
	// Drive the review to changes_requested WITHOUT letting the cascade
	// dispatch, by restarting the coordinator first: the durable state is a run
	// whose fix cycle is derivable and whose outbox holds nothing at all.
	f.driveToFixDispatch()
	f.c = f.restart()
	sends := f.sender.calls

	f.poll(3)

	if f.sender.calls != sends {
		t.Fatalf("Send calls = %d, want still %d", f.sender.calls, sends)
	}
	f.assertOneAttemptOneCycle()
}

// Boundary B: the claim is durable and Send was never reached. The absence of
// the pre-delivery record is positive proof of that, so recovery delivers once
// — under the token already on the row, never a freshly minted one.
func TestCrashAfterClaimBeforeSendDeliversOnceUnderTheSameClaim(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()

	// Rewind to the crash: the row is claimed by a generation that wrote no
	// record, and nothing else exists.
	f.store.checkpoints[f.runID] = dropPhases(f.store.checkpoints[f.runID], "fix_dispatch_intent", "fix_dispatched")
	delete(f.store.attempts, f.fixStepID)
	f.stampFixOutboxGeneration("wfg-crashed-claim")
	f.setFixOutboxStatus(domain.WorkflowOutboxDispatched)
	f.sender.calls = 0

	f.c = f.restart()
	f.poll(2)

	if f.sender.calls != 1 {
		t.Fatalf("Send calls = %d, want exactly 1: a provably undelivered cycle is delivered once", f.sender.calls)
	}
	intents := f.fixDeliveryRecords("fix_dispatch_intent")
	if len(intents) != 1 {
		t.Fatalf("pre-delivery records = %d, want exactly 1", len(intents))
	}
	if got := generationOf(intents[0]); got != "wfg-crashed-claim" {
		t.Fatalf("recovery delivered under generation %q, want the claim already on the row: a new token would be a second identity", got)
	}
	f.assertOneAttemptOneCycle()
}

// ---------------------------------------------------------------------------
// g / h / i / j: the windows after Send
// ---------------------------------------------------------------------------

// Boundary C: Send succeeded and nothing after it landed. Proven delivery is
// adopted, never re-sent, and the bookkeeping completes under the SAME
// generation the interrupted pass held.
func TestCrashAfterSendBeforeAckAdoptsTheSameGeneration(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()
	gen := generationOf(f.fixDeliveryRecords("fix_dispatch_intent")[0])
	f.crashAfterSend(deliveryEvidence{receipt: f.sender.lastMsg})

	f.c = f.restart()
	f.poll(2)

	if f.sender.calls != 0 {
		t.Fatalf("Send calls after recovery = %d, want 0: the agent already has this cycle", f.sender.calls)
	}
	f.assertOneAttemptOneCycle()
	dispatched := f.fixDeliveryRecords("fix_dispatched")
	if len(dispatched) != 1 {
		t.Fatalf("fix_dispatched records = %d, want exactly 1", len(dispatched))
	}
	if got := generationOf(dispatched[0]); got != gen {
		t.Fatalf("recovery completed under generation %q, want the interrupted pass's %q", got, gen)
	}
}

// Boundary D: the acknowledge landed and the attempt row did not. The
// acknowledge cleared the claim token, so ownership can only be proven from the
// pre-delivery record — which is exactly what it is for.
func TestCrashAfterAckBeforeAttemptOpenConverges(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()
	gen := generationOf(f.fixDeliveryRecords("fix_dispatch_intent")[0])

	// The crash: acknowledged row, no attempt, no dispatch record.
	delete(f.store.attempts, f.fixStepID)
	f.store.checkpoints[f.runID] = dropPhases(f.store.checkpoints[f.runID], "fix_dispatched")
	f.setFixOutboxStatus(domain.WorkflowOutboxAcknowledged)
	f.mutateSession(func(rec *domain.SessionRecord) { rec.Metadata.LatestUserPrompt = f.sender.lastMsg })
	f.sender.calls = 0

	f.c = f.restart()
	f.poll(2)

	if f.sender.calls != 0 {
		t.Fatalf("Send calls = %d, want 0: the acknowledge proves the delivery happened", f.sender.calls)
	}
	f.assertOneAttemptOneCycle()
	dispatched := f.fixDeliveryRecords("fix_dispatched")
	if len(dispatched) != 1 || generationOf(dispatched[0]) != gen {
		t.Fatalf("fix_dispatched records = %+v, want exactly one under generation %q", dispatched, gen)
	}
}

// Boundary F, and the one the old attempt-count guard could not see at all: the
// attempt row exists and the fix_dispatched record does not. Before this, the
// cycle counted as dispatched and observeFixStep had no checkpoint to observe
// against, so the run sat still forever.
func TestCrashAfterAttemptOpenBeforeDispatchRecordConverges(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()
	gen := generationOf(f.fixDeliveryRecords("fix_dispatch_intent")[0])
	attemptsBefore := f.fixAttempts()

	f.store.checkpoints[f.runID] = dropPhases(f.store.checkpoints[f.runID], "fix_dispatched")
	f.setFixOutboxStatus(domain.WorkflowOutboxAcknowledged)
	f.mutateSession(func(rec *domain.SessionRecord) { rec.Metadata.LatestUserPrompt = f.sender.lastMsg })
	f.sender.calls = 0

	f.c = f.restart()
	f.poll(2)

	if f.sender.calls != 0 {
		t.Fatalf("Send calls = %d, want 0: an attempt row is durable evidence the delivery completed", f.sender.calls)
	}
	if got := f.fixAttempts(); got != attemptsBefore {
		t.Fatalf("fix attempts = %d, want still %d: recovery must not open a second one", got, attemptsBefore)
	}
	dispatched := f.fixDeliveryRecords("fix_dispatched")
	if len(dispatched) != 1 {
		t.Fatalf("fix_dispatched records = %d, want exactly 1: the interrupted bookkeeping was not completed", len(dispatched))
	}
	if got := generationOf(dispatched[0]); got != gen {
		t.Fatalf("the completed record names generation %q, want %q", got, gen)
	}
}

// The worker finishes the cycle before AO ever closed the attempt. Observation
// owns it from there, and no second prompt is ever produced.
func TestFixCompletionBeforeAttemptClosureNeedsNoSecondPrompt(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()
	sends := f.sender.calls

	// The worker committed and went idle: a genuinely new workspace
	// fingerprint, which is the only evidence observation acts on.
	f.workspaceFacts.obs.HeadSHA = "sha-after-fix"
	f.workspaceFacts.obs.Dirty = false
	f.mutateSession(func(rec *domain.SessionRecord) {
		rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: f.clk.Now()}
		rec.TurnCompletedAt = f.clk.Now()
		rec.FirstSignalAt = f.clk.Now()
	})

	f.c = f.restart()
	f.poll(3)

	if f.sender.calls != sends {
		t.Fatalf("Send calls = %d, want still %d", f.sender.calls, sends)
	}
	f.assertOneAttemptOneCycle()
}

// ---------------------------------------------------------------------------
// c: a generation that does not own the delivery
// ---------------------------------------------------------------------------

// Requirement 3, the load-bearing case: the row is claimed by one generation
// and the ledger records another. AO must not send, must not open an attempt,
// must not advance the run — and must say which two disagree.
func TestFixDispatchWhoseClaimDisagreesWithTheLedgerFailsClosed(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()

	f.crashAfterSend(deliveryEvidence{receipt: f.sender.lastMsg})
	f.stampFixOutboxGeneration("wfg-somebody-else")
	f.sender.calls = 0
	attemptsBefore := f.fixAttempts()

	f.c = f.restart()
	f.poll(3)

	if f.sender.calls != 0 {
		t.Fatalf("Send calls = %d, want 0: a dispatch AO cannot account for must never write into a worker session", f.sender.calls)
	}
	if got := f.fixAttempts(); got != attemptsBefore {
		t.Fatalf("fix attempts = %d, want still %d", got, attemptsBefore)
	}
	if n := len(f.fixDeliveryRecords("fix_dispatched")); n != 0 {
		t.Fatalf("fix_dispatched records = %d, want 0: a stale generation must not advance the fix lifecycle", n)
	}
	if f.runState() != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", f.runState())
	}
	if n := f.countCheckpointPhase(workflowcore.ReasonFixGenerationUnprovable); n != 1 {
		t.Fatalf("fix_generation_unprovable stops = %d, want exactly 1", n)
	}
	note := f.latestCheckpointPhaseNextAction(workflowcore.ReasonFixGenerationUnprovable)
	if !strings.Contains(note, "wfg-somebody-else") {
		t.Fatalf("stop note = %q, want it to name the disagreeing claim", note)
	}

	// And it does not become a retry loop: the condition is unchanged, so the
	// ledger records it once however long the run is polled.
	f.poll(5)
	if n := f.countCheckpointPhase(workflowcore.ReasonFixGenerationUnprovable); n != 1 {
		t.Fatalf("fix_generation_unprovable stops after further polls = %d, want still 1", n)
	}
}

// ---------------------------------------------------------------------------
// d / n / p: review authority
// ---------------------------------------------------------------------------

// Requirement 5: a review generation that has moved on makes the fix generation
// derived from it inert. The review run is the same row and still asks for
// changes; only the commit it reviewed changed.
func TestSupersededReviewGenerationCannotSendItsFixCycle(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()

	// The crash window: the pre-delivery record is written and Send may or may
	// not have run. Then the review is re-reviewed against a different commit —
	// the same row, still asking for changes, speaking with a new authority.
	f.crashBeforeAck()
	f.sender.calls = 0
	f.retargetReviewRun("sha-somewhere-else")

	f.c = f.restart()
	f.poll(3)

	if f.sender.calls != 0 {
		t.Fatalf("Send calls = %d, want 0: the fix cycle's review generation was superseded", f.sender.calls)
	}
	if n := f.fixAttempts(); n != 0 {
		t.Fatalf("fix attempts = %d, want 0", n)
	}
	if n := len(f.fixDeliveryRecords("fix_dispatched")); n != 0 {
		t.Fatalf("fix_dispatched records = %d, want 0: a superseded generation must not advance the fix lifecycle", n)
	}
	if n := f.countCheckpointPhase(workflowcore.ReasonFixGenerationUnprovable); n != 1 {
		t.Fatalf("fix_generation_unprovable stops = %d, want exactly 1", n)
	}
	note := f.latestCheckpointPhaseNextAction(workflowcore.ReasonFixGenerationUnprovable)
	if !strings.Contains(note, "not the dispatch this pass derived") {
		t.Fatalf("stop note = %q, want it to say the dispatch on disk is not the one derived now", note)
	}
}

// Requirement 5's approval clause, end to end: an approved review with no
// unanswered verify re-entry authorizes nothing, so the fix cycle derived from
// its earlier changes_requested is inert.
func TestApprovedReviewMakesAStaleFixGenerationInert(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()

	f.crashBeforeAck()
	f.sender.calls = 0
	f.approveReviewRun()

	f.c = f.restart()
	f.poll(3)

	if f.sender.calls != 0 {
		t.Fatalf("Send calls = %d, want 0: an approved review authorizes no fix cycle", f.sender.calls)
	}
	if n := f.fixAttempts(); n != 0 {
		t.Fatalf("fix attempts = %d, want 0", n)
	}
	if n := len(f.fixDeliveryRecords("fix_dispatched")); n != 0 {
		t.Fatalf("fix_dispatched records = %d, want 0: a stale fix generation must not authorize a re-review", n)
	}
}

// ---------------------------------------------------------------------------
// o / q: the verify-driven re-entry, and the >4 KB transport
// ---------------------------------------------------------------------------

// The verify->fix re-entry is a genuinely different dispatch from any
// review-driven cycle: different findings, from a different source, for a
// different cycle number. It must therefore get its own generation, bound to
// the verification payload it actually carries — and the >4 KB transport must
// keep delivering that payload whole, which is requirement 6's "preserve the
// existing transport fix".
//
// This runs on the autonomous fixture rather than the recovery one because the
// re-entry is only reachable through the real lifecycle: work -> review
// approved -> verify fails -> verify_fix_reentry -> fix dispatch.
func TestVerifyDrivenFixReentryGetsItsOwnGenerationAndSurvivesTheLargeTransport(t *testing.T) {
	fx := newAutonomousFixture(t, oneTaskPlan())
	ctx := context.Background()
	seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())

	hugeOutput := strings.Repeat("--- FAIL: TestSomething (0.01s)\n    thing_test.go:118: want 3 rows, got 4\n", 700)
	fx.verifier.result = workflowcore.VerifyCommandExecution{ExitCode: 1, StdoutTail: hugeOutput}

	created, err := fx.coord.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")

	fixSent := false
	driveCycles(t, fx, 40, func(int) {
		if _, childID, ok := activeChildRunID(t, fx, created.Run.ID); ok {
			approveOpenReview(t, fx, childID, domain.VerdictApproved)
		}
		if fx.sender.calls > 0 && strings.Contains(fx.sender.lastMsg, "--- FAIL: TestSomething") && !fixSent {
			fixSent = true
			fx.ws.obs.Changes = append(fx.ws.obs.Changes, ports.WorkspaceChange{Path: "fix.go", Status: " M"})
			fx.verifier.result = workflowcore.VerifyCommandExecution{ExitCode: 0}
		}
	})
	if !fixSent {
		t.Fatal("the verify findings never reached the fix worker")
	}
	// q: the prompt is past the inline transport boundary and arrived whole.
	if len(fx.sender.lastMsg) <= ports.MaxInlinePromptBytes {
		t.Fatalf("fix prompt = %d bytes, want one past the inline transport boundary", len(fx.sender.lastMsg))
	}
	if !strings.Contains(fx.sender.lastMsg, hugeOutput[:400]) {
		t.Fatal("the delivered fix prompt does not carry the verification output")
	}

	// o: that cycle's dispatch record names its own generation, bound to the
	// verification findings rather than to a reviewer's.
	tasks, err := fx.store.ListWorkflowTasks(ctx, created.Run.ID)
	if err != nil || len(tasks) == 0 || tasks[0].ExecutionRunID == nil {
		t.Fatalf("no child run to inspect: tasks=%+v err=%v", tasks, err)
	}
	cps, err := fx.store.ListWorkflowCheckpoints(ctx, *tasks[0].ExecutionRunID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	seen := map[string]bool{}
	verifyDriven := 0
	for _, cp := range cps {
		if cp.DurablePhase != "fix_dispatched" {
			continue
		}
		var rec map[string]any
		if json.Unmarshal([]byte(cp.RetryState), &rec) != nil {
			continue
		}
		gen := generationOf(rec)
		if gen == "" {
			t.Fatal("a fix dispatch was recorded with no generation")
		}
		if seen[gen] {
			t.Fatalf("generation %s was recorded for two different fix dispatches", gen)
		}
		seen[gen] = true
		findings, _ := rec["findings"].(map[string]any)
		if source, _ := findings["source"].(string); source == workflowcore.FixFindingsSourceVerification {
			verifyDriven++
			// The generation binds the payload that actually travelled.
			if digest, _ := generationFieldOf(rec, "findingsDigest").(string); digest == "" || digest != findings["digest"] {
				t.Fatalf("the verify-driven generation binds findings %v, the record carries %v", digest, findings["digest"])
			}
			if rg, _ := generationFieldOf(rec, "reviewGeneration").(string); rg == "" {
				t.Fatal("the verify-driven fix generation is not bound to a review generation")
			}
		}
	}
	if verifyDriven != 1 {
		t.Fatalf("verify-driven fix dispatches = %d, want exactly 1", verifyDriven)
	}
}

// ---------------------------------------------------------------------------
// s: legacy rows
// ---------------------------------------------------------------------------

// Requirement 9: a generation-less delivery that maps deterministically onto one
// cycle recovers safely, without a token being fabricated for it.
func TestLegacyGenerationlessFixDeliveryRecoversSafely(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()
	f.crashAfterSend(deliveryEvidence{receipt: f.sender.lastMsg})
	// The rows as an older daemon left them: no token anywhere.
	f.stripFixGenerations()
	f.stampFixOutboxGeneration("")
	f.sender.calls = 0

	f.c = f.restart()
	f.poll(2)

	if f.sender.calls != 0 {
		t.Fatalf("Send calls = %d, want 0: the delivery was proven, not re-sent", f.sender.calls)
	}
	f.assertOneAttemptOneCycle()
	dispatched := f.fixDeliveryRecords("fix_dispatched")
	if len(dispatched) != 1 {
		t.Fatalf("fix_dispatched records = %d, want exactly 1", len(dispatched))
	}
	if got := generationOf(dispatched[0]); got != "" {
		t.Fatalf("recovery fabricated generation %q for a generation-less delivery", got)
	}
}

// ...and one that does NOT map deterministically fails closed with a named
// condition, rather than guessing which delivery it was.
func TestUnprovableLegacyFixStateFailsClosed(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()
	f.crashAfterSend(deliveryEvidence{receipt: f.sender.lastMsg})
	f.stripFixGenerations()
	f.stampFixOutboxGeneration("")
	// The generation-less record on disk names findings this pass does not
	// derive: AO cannot prove the two are the same delivery.
	f.rewriteFixFindingsDigest("a-digest-from-some-other-payload")
	f.sender.calls = 0

	f.c = f.restart()
	f.poll(3)

	if f.sender.calls != 0 {
		t.Fatalf("Send calls = %d, want 0", f.sender.calls)
	}
	if n := len(f.fixDeliveryRecords("fix_dispatched")); n != 0 {
		t.Fatalf("fix_dispatched records = %d, want 0", n)
	}
	if n := f.countCheckpointPhase(workflowcore.ReasonFixGenerationUnprovable); n != 1 {
		t.Fatalf("fix_generation_unprovable stops = %d, want exactly 1", n)
	}
	if f.runState() != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", f.runState())
	}
}

// ---------------------------------------------------------------------------
// The unsubmitted-prompt path keeps its generation too
// ---------------------------------------------------------------------------

// Requirement 6: a prompt that reached the composer and never left it still
// parks the way it always did — and the record now names the generation, so the
// resume that submits it is bound to the same delivery.
func TestUnsubmittedFixPromptRecordsItsGeneration(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.useReportingSender(ports.PromptLoadedNotSubmitted)
	f.driveToFixDispatch()

	notSubmitted := f.fixDeliveryRecords(workflowcore.ReasonFixPromptNotSubmitted)
	if len(notSubmitted) != 1 {
		t.Fatalf("fix_prompt_not_submitted records = %d, want exactly 1", len(notSubmitted))
	}
	if generationOf(notSubmitted[0]) == "" {
		t.Fatal("the unsubmitted-prompt record carries no generation")
	}
	intents := f.fixDeliveryRecords("fix_dispatch_intent")
	if got := generationOf(notSubmitted[0]); got != generationOf(intents[0]) {
		t.Fatalf("the unsubmitted record names generation %q, the pre-delivery record %q", got, generationOf(intents[0]))
	}
}

// ---------------------------------------------------------------------------
// crash-state helpers
// ---------------------------------------------------------------------------

// dropPhases removes every checkpoint of the given durable phases, modelling
// the rows a crash never got as far as writing.
func dropPhases(cps []domain.WorkflowCheckpoint, phases ...string) []domain.WorkflowCheckpoint {
	drop := map[string]bool{}
	for _, p := range phases {
		drop[p] = true
	}
	kept := cps[:0]
	for _, cp := range cps {
		if drop[cp.DurablePhase] {
			continue
		}
		kept = append(kept, cp)
	}
	return kept
}

// setFixOutboxStatus rewinds the fix outbox row to the status a crash left it
// in. Timestamps are cleared to match, so nothing downstream reads a completion
// that did not happen.
func (f *fixRecoveryFixture) setFixOutboxStatus(status domain.WorkflowOutboxStatus) {
	f.t.Helper()
	for key, entry := range f.store.outbox {
		if entry.WorkflowStepID == nil || *entry.WorkflowStepID != f.fixStepID {
			continue
		}
		entry.Status = status
		if status != domain.WorkflowOutboxAcknowledged {
			entry.AcknowledgedAt = nil
		}
		f.store.outbox[key] = entry
	}
}

// retargetReviewRun moves the review run onto a different commit WITHOUT
// changing its verdict: the same row still asks for changes, but the authority
// it speaks with is a different generation.
func (f *fixRecoveryFixture) retargetReviewRun(targetSHA string) {
	f.t.Helper()
	for id, rr := range f.reviewRuns.runs {
		if rr.Verdict == domain.VerdictChangesRequested {
			rr.TargetSHA = targetSHA
			f.reviewRuns.runs[id] = rr
		}
	}
}

// approveReviewRun turns the changes_requested verdict into an approval, with
// no verify re-entry outstanding — the state in which nothing authorizes a fix.
func (f *fixRecoveryFixture) approveReviewRun() {
	f.t.Helper()
	for id, rr := range f.reviewRuns.runs {
		if rr.Verdict == domain.VerdictChangesRequested {
			rr.Verdict = domain.VerdictApproved
			f.reviewRuns.runs[id] = rr
		}
	}
}

// stripFixGenerations rewrites every fix delivery record as an older daemon
// would have written it: no generation at all.
func (f *fixRecoveryFixture) stripFixGenerations() {
	f.t.Helper()
	f.rewriteFixDeliveryRecords(func(rec map[string]any) { delete(rec, "generation") })
}

// rewriteFixFindingsDigest changes the findings a recorded delivery says it
// carried, so it no longer maps onto the delivery this run would derive now.
func (f *fixRecoveryFixture) rewriteFixFindingsDigest(digest string) {
	f.t.Helper()
	f.rewriteFixDeliveryRecords(func(rec map[string]any) {
		findings, ok := rec["findings"].(map[string]any)
		if !ok {
			findings = map[string]any{}
		}
		findings["digest"] = digest
		rec["findings"] = findings
	})
}

func (f *fixRecoveryFixture) rewriteFixDeliveryRecords(mutate func(map[string]any)) {
	f.t.Helper()
	cps := f.store.checkpoints[f.runID]
	for i, cp := range cps {
		switch cp.DurablePhase {
		case "fix_dispatch_intent", "fix_dispatched":
		default:
			continue
		}
		var rec map[string]any
		if json.Unmarshal([]byte(cp.RetryState), &rec) != nil {
			continue
		}
		mutate(rec)
		raw, err := json.Marshal(rec)
		if err != nil {
			f.t.Fatalf("re-encode delivery record: %v", err)
		}
		cps[i].RetryState = string(raw)
	}
	f.store.checkpoints[f.runID] = cps
}

// crashBeforeAck rewinds to the window between the pre-delivery record and the
// acknowledgement: the record and the claim survive (only acknowledge/fail
// clear the token), the attempt row and the dispatch record do not.
func (f *fixRecoveryFixture) crashBeforeAck() {
	f.t.Helper()
	delete(f.store.attempts, f.fixStepID)
	f.store.checkpoints[f.runID] = dropPhases(f.store.checkpoints[f.runID], "fix_dispatched")
	f.stampFixOutboxGeneration(f.dispatchGenerationFromIntent())
	f.setFixOutboxStatus(domain.WorkflowOutboxDispatched)
}
