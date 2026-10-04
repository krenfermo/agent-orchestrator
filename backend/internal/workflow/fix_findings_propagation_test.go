package workflow_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// fix_findings_propagation_test.go — the regression suite for "did the fix
// worker actually receive the complete reviewer finding".
//
// The production report these cover: a reviewer returned changes_requested with
// a finding naming docs/worker-lifecycle-audit.md:142-167, AO launched the fix
// step, the fix worker went idle without touching the workspace, and AO stopped
// on ambiguous_worker_state. The finding was visible in the UI, and no durable
// evidence existed to say whether the worker had been given it.
//
// These tests pin the propagation itself (the findings bytes reach the prompt
// the transport is handed) AND the evidence about it (the digest, count and
// embedded flag recorded before delivery), because the second is what makes the
// first diagnosable after the fact rather than only assertable in a test.

// productionFinding reproduces the shape of the finding from the report: a
// single reviewer finding that names a file and a line range.
const productionFinding = `The worker lifecycle audit is wrong about the recovery window.

- docs/worker-lifecycle-audit.md:142-167 claims a dispatched outbox entry is
  unrecoverable after a restart. That has not been true since the pre-delivery
  intent record landed; the paragraph must be rewritten to describe the
  three-verdict classification instead.`

// driveToChangesRequestedBody is driveToChangesRequested with the reviewer's
// findings text under the test's control.
func driveToChangesRequestedBody(t *testing.T, c *workflowcore.Coordinator, store *fakeStore, clk *fakeClock, sessionFacts *fakeSessionFacts, workspaceFacts *fakeWorkspaceFacts, reviewRuns *fakeReviewRuns, runID, body string) workflowcore.RunDetail {
	t.Helper()
	ctx := context.Background()
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, runID)
	got, err := c.ContinueRun(ctx, runID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	reviewRunID := *reviewStepFrom(got).Step.ReviewRunID
	reviewRuns.setStatus(reviewRunID, domain.ReviewRunComplete, domain.VerdictChangesRequested)
	reviewRuns.runs[reviewRunID] = withBody(reviewRuns.runs[reviewRunID], body)
	clk.Advance(time.Second)

	final, err := c.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun after changes_requested: %v", err)
	}
	return final
}

// fixFixture is the full review->fix stack plus the handles a test needs to
// inspect what was delivered.
type fixFixture struct {
	c              *workflowcore.Coordinator
	store          *fakeStore
	clk            *fakeClock
	sessionFacts   *fakeSessionFacts
	workspaceFacts *fakeWorkspaceFacts
	reviewRuns     *fakeReviewRuns
	launcher       *fakeReviewerLauncher
	sender         *fakeMessageSender
	spawner        *fakeSpawner
	runID          string
}

func newFixFixture(t *testing.T) *fixFixture {
	t.Helper()
	sessionFacts := newFakeSessionFacts()
	f := &fixFixture{
		sessionFacts:   sessionFacts,
		spawner:        &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts},
		workspaceFacts: &fakeWorkspaceFacts{},
		reviewRuns:     newFakeReviewRuns(),
		launcher:       &fakeReviewerLauncher{},
		sender:         &fakeMessageSender{},
	}
	f.c, f.store, f.clk = newCoordinatorWithFix(f.spawner, f.sessionFacts, f.workspaceFacts, f.reviewRuns, f.launcher, f.sender)
	created, err := f.c.CreateRun(context.Background(), "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	f.runID = created.Run.ID
	return f
}

// delivery reads the fix step's durable delivery projection back out of GetRun,
// i.e. through exactly the surface the API exposes.
func (f *fixFixture) delivery(t *testing.T) *workflowcore.FixDeliveryReport {
	t.Helper()
	detail, err := f.c.GetRun(context.Background(), f.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	report := fixStepFrom(detail).FixDelivery
	if report == nil {
		t.Fatalf("fix step has no FixDelivery report")
	}
	return report
}

// --- C1: one finding reaches the fix prompt, exactly ------------------------

func TestChangesRequestedWithOneFindingDeliversThatExactFindingToTheFixWorker(t *testing.T) {
	f := newFixFixture(t)
	driveToChangesRequestedBody(t, f.c, f.store, f.clk, f.sessionFacts, f.workspaceFacts, f.reviewRuns, f.runID, productionFinding)

	if f.sender.calls != 1 {
		t.Fatalf("sender calls = %d, want 1", f.sender.calls)
	}
	// The propagation itself: the reviewer's bytes, verbatim, in the message
	// the transport was handed.
	if !strings.Contains(f.sender.lastMsg, productionFinding) {
		t.Fatalf("delivered prompt does not contain the reviewer's findings verbatim.\nprompt:\n%s", f.sender.lastMsg)
	}
	if !strings.Contains(f.sender.lastMsg, "docs/worker-lifecycle-audit.md:142-167") {
		t.Fatalf("delivered prompt lost the finding's file/line reference")
	}

	// The evidence about it, recorded before the send.
	got := f.delivery(t)
	if !got.FindingsEmbedded {
		t.Fatalf("FindingsEmbedded = false, want true — AO recorded that the findings were NOT in the prompt it delivered")
	}
	if want := workflowcore.FindingsDigest(productionFinding); got.FindingsDigest != want {
		t.Fatalf("FindingsDigest = %q, want %q", got.FindingsDigest, want)
	}
	if got.FindingsBytes != len(productionFinding) {
		t.Fatalf("FindingsBytes = %d, want %d", got.FindingsBytes, len(productionFinding))
	}
	if got.FindingsCount != 1 {
		t.Fatalf("FindingsCount = %d, want 1", got.FindingsCount)
	}
	if got.FindingsSource != workflowcore.FixFindingsSourceReview {
		t.Fatalf("FindingsSource = %q, want %q", got.FindingsSource, workflowcore.FixFindingsSourceReview)
	}
	if got.ReviewVerdict != string(domain.VerdictChangesRequested) {
		t.Fatalf("ReviewVerdict = %q, want changes_requested", got.ReviewVerdict)
	}
	if got.State != workflowcore.FixDeliveryStateDispatched {
		t.Fatalf("State = %q, want dispatched", got.State)
	}
}

// --- C2: every required finding reaches the fix prompt ----------------------

func TestChangesRequestedWithMultipleFindingsDeliversAllOfThem(t *testing.T) {
	findings := strings.Join([]string{
		"- docs/worker-lifecycle-audit.md:142-167 describes a recovery window that no longer exists.",
		"- internal/workflow/fix_dispatch.go:210 should name the outbox key it guards on.",
		"- The audit never states which verdict column drives the fix cycle.",
	}, "\n")
	f := newFixFixture(t)
	driveToChangesRequestedBody(t, f.c, f.store, f.clk, f.sessionFacts, f.workspaceFacts, f.reviewRuns, f.runID, findings)

	// Not just "the blob is present": each individual finding must survive,
	// because a truncating builder would still pass a whole-body contains check
	// if the body happened to be short.
	for _, line := range strings.Split(findings, "\n") {
		if !strings.Contains(f.sender.lastMsg, line) {
			t.Fatalf("delivered prompt is missing finding %q", line)
		}
	}
	got := f.delivery(t)
	if !got.FindingsEmbedded {
		t.Fatalf("FindingsEmbedded = false, want true")
	}
	if got.FindingsCount != 3 {
		t.Fatalf("FindingsCount = %d, want 3", got.FindingsCount)
	}
	if got.FindingsDigest != workflowcore.FindingsDigest(findings) {
		t.Fatalf("FindingsDigest does not match the reviewer's body")
	}
}

// --- C3: restart between verdict and dispatch -------------------------------

func TestDaemonRestartBetweenVerdictAndFixDispatchDeliversTheSameDurableFindings(t *testing.T) {
	ctx := context.Background()
	f := newFixFixture(t)

	// Reach a recorded changes_requested verdict WITHOUT letting the cascade
	// dispatch the fix: the window the report describes.
	completeWorkStep(t, f.c, f.store, f.clk, f.sessionFacts, f.workspaceFacts, f.runID)
	got, err := f.c.ContinueRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	reviewRunID := *reviewStepFrom(got).Step.ReviewRunID
	f.reviewRuns.setStatus(reviewRunID, domain.ReviewRunComplete, domain.VerdictChangesRequested)
	f.reviewRuns.runs[reviewRunID] = withBody(f.reviewRuns.runs[reviewRunID], productionFinding)
	f.clk.Advance(time.Second)
	if f.sender.calls != 0 {
		t.Fatalf("fix was dispatched before the restart; this test needs the pre-dispatch window")
	}

	// Restart: a brand-new Coordinator over the same durable store, with its
	// own transport. Nothing in memory carries over.
	sender2 := &fakeMessageSender{}
	var idSeq int
	c2 := workflowcore.New(workflowcore.Deps{
		Store: f.store, Spawner: f.spawner, SessionFacts: f.sessionFacts, WorkspaceFacts: f.workspaceFacts,
		ReviewRuns: f.reviewRuns, ReviewerLauncher: f.launcher, MessageSender: sender2, Clock: f.clk.Now,
		NewID: func() string { idSeq++; return fmt.Sprintf("restart-id%d", idSeq) },
	})
	detail, err := c2.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetRun after restart: %v", err)
	}

	if sender2.calls != 1 {
		t.Fatalf("post-restart sender calls = %d, want 1 (the fix must still be dispatched)", sender2.calls)
	}
	if !strings.Contains(sender2.lastMsg, productionFinding) {
		t.Fatalf("post-restart fix prompt lost the reviewer's findings.\nprompt:\n%s", sender2.lastMsg)
	}
	report := fixStepFrom(detail).FixDelivery
	if report == nil {
		t.Fatalf("no FixDelivery report after restart")
	}
	if report.FindingsDigest != workflowcore.FindingsDigest(productionFinding) {
		t.Fatalf("post-restart FindingsDigest = %q, want the same durable findings", report.FindingsDigest)
	}
	if !report.FindingsEmbedded {
		t.Fatalf("post-restart FindingsEmbedded = false, want true")
	}
	if report.ReviewRunID != reviewRunID {
		t.Fatalf("post-restart ReviewRunID = %q, want %q", report.ReviewRunID, reviewRunID)
	}
}

// --- C4: duplicate reconciliation neither loses, duplicates nor mutates -----

func TestRepeatedReconciliationNeverLosesDuplicatesOrMutatesTheFindings(t *testing.T) {
	ctx := context.Background()
	f := newFixFixture(t)
	driveToChangesRequestedBody(t, f.c, f.store, f.clk, f.sessionFacts, f.workspaceFacts, f.reviewRuns, f.runID, productionFinding)

	first := f.delivery(t)
	wantDigest := workflowcore.FindingsDigest(productionFinding)

	// Poll the cascade repeatedly, exactly as the daemon does.
	for i := 0; i < 6; i++ {
		f.clk.Advance(time.Second)
		if _, err := f.c.GetRun(ctx, f.runID); err != nil {
			t.Fatalf("GetRun poll %d: %v", i, err)
		}
	}

	if f.sender.calls != 1 {
		t.Fatalf("sender calls after repeated reconciliation = %d, want 1 (no duplicate delivery)", f.sender.calls)
	}
	after := f.delivery(t)
	if after.FindingsDigest != wantDigest {
		t.Fatalf("findings digest mutated across reconciliation: %q -> %q", wantDigest, after.FindingsDigest)
	}
	if after.CycleNumber != first.CycleNumber || after.FixAttemptID != first.FixAttemptID {
		t.Fatalf("fix attempt binding mutated across reconciliation: cycle %d/%d attempt %q/%q",
			first.CycleNumber, after.CycleNumber, first.FixAttemptID, after.FixAttemptID)
	}
	if after.FindingsCount != first.FindingsCount || after.FindingsBytes != first.FindingsBytes {
		t.Fatalf("findings count/size mutated across reconciliation")
	}

	// Exactly one dispatched record and one intent record for this cycle —
	// findings neither lost nor duplicated on the ledger.
	cps, err := f.store.ListWorkflowCheckpoints(ctx, f.runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	counts := map[string]int{}
	for _, cp := range cps {
		switch cp.DurablePhase {
		case "fix_dispatch_intent", "fix_dispatched":
			counts[cp.DurablePhase]++
		}
	}
	if counts["fix_dispatch_intent"] != 1 || counts["fix_dispatched"] != 1 {
		t.Fatalf("delivery checkpoints = %v, want exactly one of each", counts)
	}
}

// --- C5: a stale review generation cannot feed a newer fix attempt ----------

func TestSupersededReviewGenerationCannotDispatchAFixCycle(t *testing.T) {
	ctx := context.Background()
	f := newFixFixture(t)

	completeWorkStep(t, f.c, f.store, f.clk, f.sessionFacts, f.workspaceFacts, f.runID)
	got, err := f.c.ContinueRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	reviewRunID := *reviewStepFrom(got).Step.ReviewRunID

	// The stale generation: a reviewer that answered changes_requested AFTER AO
	// had closed its run out and handed authority to a replacement. Its
	// findings are real, and they are evidence, never a decision.
	stale := f.reviewRuns.runs[reviewRunID]
	stale.Status = domain.ReviewRunComplete
	stale.Verdict = ""
	stale.LateVerdict = domain.VerdictChangesRequested
	stale.LateVerdictBody = "stale generation: rewrite docs/worker-lifecycle-audit.md:142-167"
	lateAt := f.clk.Now()
	stale.LateVerdictAt = &lateAt
	stale.SupersededBy = "rr-newer"
	f.reviewRuns.runs[reviewRunID] = stale
	f.clk.Advance(time.Second)

	detail, err := f.c.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if f.sender.calls != 0 {
		t.Fatalf("a superseded review generation dispatched a fix cycle (sender calls = %d, want 0); delivered:\n%s",
			f.sender.calls, f.sender.lastMsg)
	}
	if report := fixStepFrom(detail).FixDelivery; report != nil {
		t.Fatalf("a superseded review generation produced a fix delivery record: %+v", report)
	}
	// And nothing anywhere on the ledger carries the stale findings.
	cps, err := f.store.ListWorkflowCheckpoints(ctx, f.runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	staleDigest := workflowcore.FindingsDigest(stale.LateVerdictBody)
	for _, cp := range cps {
		if strings.Contains(cp.RetryState, staleDigest) {
			t.Fatalf("checkpoint %s (%s) carries the superseded generation's findings digest", cp.ID, cp.DurablePhase)
		}
	}
}

// --- C6: the fix attempt is durably bound to its whole provenance -----------

func TestFixAttemptIsDurablyBoundToWorkflowTaskReviewAndFindings(t *testing.T) {
	ctx := context.Background()
	f := newFixFixture(t)
	detail := driveToChangesRequestedBody(t, f.c, f.store, f.clk, f.sessionFacts, f.workspaceFacts, f.reviewRuns, f.runID, productionFinding)

	reviewStep := reviewStepFrom(detail)
	fixStep := fixStepFrom(detail)
	report := f.delivery(t)

	// workflow + step
	cps, err := f.store.ListWorkflowCheckpoints(ctx, f.runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	bound := false
	for _, cp := range cps {
		if cp.DurablePhase != "fix_dispatched" {
			continue
		}
		if cp.WorkflowRunID != f.runID {
			t.Fatalf("fix_dispatched checkpoint run = %q, want %q", cp.WorkflowRunID, f.runID)
		}
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != fixStep.Step.ID {
			t.Fatalf("fix_dispatched checkpoint is not bound to the fix step")
		}
		if cp.ReviewRunID == nil || *cp.ReviewRunID != *reviewStep.Step.ReviewRunID {
			t.Fatalf("fix_dispatched checkpoint is not bound to the review run")
		}
		if cp.ReviewVerdict != string(domain.VerdictChangesRequested) {
			t.Fatalf("fix_dispatched checkpoint verdict = %q, want changes_requested", cp.ReviewVerdict)
		}
		bound = true
	}
	if !bound {
		t.Fatalf("no fix_dispatched checkpoint was written")
	}

	// review generation
	if report.ReviewRunID != *reviewStep.Step.ReviewRunID {
		t.Fatalf("report ReviewRunID = %q, want %q", report.ReviewRunID, *reviewStep.Step.ReviewRunID)
	}
	if report.ReviewTargetSHA == "" {
		t.Fatalf("report has no review target SHA")
	}
	// fix generation + attempt row
	if report.CycleNumber != 1 {
		t.Fatalf("report CycleNumber = %d, want 1", report.CycleNumber)
	}
	attempts, err := f.store.ListWorkflowAttempts(ctx, fixStep.Step.ID)
	if err != nil {
		t.Fatalf("ListWorkflowAttempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("fix attempts = %d, want 1", len(attempts))
	}
	if report.FixAttemptID != attempts[0].ID {
		t.Fatalf("report FixAttemptID = %q, want %q", report.FixAttemptID, attempts[0].ID)
	}
	// reviewer verdict + finding set
	if report.ReviewVerdict != string(domain.VerdictChangesRequested) {
		t.Fatalf("report ReviewVerdict = %q", report.ReviewVerdict)
	}
	if report.FindingsDigest != workflowcore.FindingsDigest(productionFinding) {
		t.Fatalf("report is not bound to the reviewer's finding set")
	}
	// worker session
	if report.SessionID == "" {
		t.Fatalf("report has no worker session id")
	}
	if report.SessionID != string(f.reviewRuns.runs[*reviewStep.Step.ReviewRunID].SessionID) {
		t.Fatalf("report session %q is not the review run's worker session", report.SessionID)
	}
}

// --- E: the fail-closed protection is unchanged -----------------------------

// TestIdleFixWorkerWithNoChangeStillStopsAmbiguousAndNowExplainsItself
// reproduces the production run end to end: findings delivered, worker takes a
// turn, workspace unchanged. The stop must still happen — this change does not
// suppress it — and the delivery evidence beside it must now answer "was the
// worker given the findings" without touching the filesystem.
func TestIdleFixWorkerWithNoChangeStillStopsAmbiguousAndNowExplainsItself(t *testing.T) {
	ctx := context.Background()
	f := newFixFixture(t)
	driveToChangesRequestedBody(t, f.c, f.store, f.clk, f.sessionFacts, f.workspaceFacts, f.reviewRuns, f.runID, productionFinding)

	// The worker takes its turn and changes nothing: it signals after the
	// dispatch (so fixCycleStarted is satisfied and the pickup grace period is
	// not what stops the run), goes idle, and the workspace fingerprint is
	// identical to the one the cycle was dispatched against.
	detail, err := f.c.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	workSessionID := domain.SessionID(*workStepFrom(detail).Step.SessionID)
	f.clk.Advance(time.Minute)
	f.sessionFacts.put(domain.SessionRecord{
		ID: workSessionID, ProjectID: "proj-1",
		Activity:      domain.Activity{State: domain.ActivityIdle, LastActivityAt: f.clk.Now()},
		FirstSignalAt: f.clk.Now(), TurnCompletedAt: f.clk.Now(),
		Metadata: domain.SessionMetadata{WorkspacePath: "/ws/wf", Branch: "ao/wf"},
	})
	f.clk.Advance(time.Minute)

	detail, err = f.c.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	// Fail-closed: unchanged.
	if detail.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention — the no-change protection must remain intact", detail.Run.State)
	}
	fixSD := fixStepFrom(detail)
	sawAmbiguous := false
	for _, a := range fixSD.Attempts {
		if a.ErrorClass == domain.WorkflowErrorAmbiguousWorkerState {
			sawAmbiguous = true
		}
	}
	if !sawAmbiguous {
		t.Fatalf("fix attempt error class = %v, want ambiguous_worker_state still raised", fixSD.Attempts)
	}

	// Newly diagnosable: the same read that shows the stop shows what the
	// worker was given.
	report := fixSD.FixDelivery
	if report == nil {
		t.Fatalf("an ambiguous fix stop still exposes no delivery evidence")
	}
	if !report.FindingsEmbedded || report.FindingsDigest != workflowcore.FindingsDigest(productionFinding) {
		t.Fatalf("delivery evidence does not prove the findings were delivered: %+v", report)
	}
	if report.SessionID == "" || report.CycleNumber == 0 {
		t.Fatalf("delivery evidence is missing the session/cycle binding: %+v", report)
	}
	if report.TerminalErrorClass != domain.WorkflowErrorAmbiguousWorkerState {
		t.Fatalf("report TerminalErrorClass = %q, want ambiguous_worker_state", report.TerminalErrorClass)
	}
	if report.NextAction == "" {
		t.Fatalf("report carries no next_action to explain the stop")
	}
}

// --- the counting heuristic -------------------------------------------------

func TestCountReviewFindings(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"empty", "", 0},
		{"whitespace only", "  \n\t\n", 0},
		{"prose with no markers counts as one", "The audit paragraph is wrong.", 1},
		{"dash bullets", "- one\n- two\n- three", 3},
		{"star and plus bullets", "* one\n+ two", 2},
		{"ordered list", "1. one\n2) two", 2},
		{"headings", "# One\n\nbody\n\n## Two\n\nbody", 2},
		{"nested notes are not their own findings", "- one\n  - detail\n  - detail\n- two", 2},
		{"a hash without a space is prose", "#3 is not a heading", 1},
		{"production shape", productionFinding, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := workflowcore.CountReviewFindings(tc.body); got != tc.want {
				t.Fatalf("CountReviewFindings(%q) = %d, want %d", tc.body, got, tc.want)
			}
		})
	}
}

// --- the production incident, reproduced at the workflow level --------------

// shreddingMessageSender models the transport defect behind incident
// wf-a816d7fe: tmux `paste-buffer` without -p converted every LF in the prompt
// to a carriage return, and the agent's raw-mode TUI read each CR as Enter. The
// agent therefore received the prompt as a stream of one-line submissions and
// answered only the LAST fragment. The composer really was empty afterwards, so
// the submit probe honestly reported `submitted`.
//
// The fake reproduces the OBSERVABLE consequence — the session records a
// receipt for the tail, not for the prompt AO sent — which is the only part
// workflow can ever see.
type shreddingMessageSender struct {
	facts *fakeSessionFacts
	calls int
	sent  string
}

func (s *shreddingMessageSender) Send(_ context.Context, id domain.SessionID, message string, _ *ports.SpawnAttachment) error {
	s.calls++
	s.sent = message
	lines := strings.Split(message, "\n")
	tail := lines[len(lines)-1]
	if rec, ok, _ := s.facts.GetSession(context.Background(), id); ok {
		rec.Metadata.LatestUserPrompt = tail
		s.facts.put(rec)
	}
	return nil
}

// TestShreddedFixPromptIsVisibleAsAReceiptMismatch is the regression test for
// the production run: AO builds a complete prompt, the transport reports
// success, and the agent receives only its tail.
//
// It pins the two properties that made that incident undiagnosable:
//
//  1. AO's own record still proves what it MEANT to deliver — the findings
//     digest and the embedded flag are computed over the prompt AO built, so
//     "AO never put the findings in the prompt" is ruled out from the ledger.
//  2. ReceiptMatch reports "other" — AO and the agent do not hold the same
//     bytes — which is the fact that distinguishes a transport that destroyed
//     the prompt from a worker that ignored it.
func TestShreddedFixPromptIsVisibleAsAReceiptMismatch(t *testing.T) {
	ctx := context.Background()
	sessionFacts := newFakeSessionFacts()
	sender := &shreddingMessageSender{facts: sessionFacts}
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	_, store, clk := newCoordinatorWithFix(spawner, sessionFacts, workspaceFacts, reviewRuns, &fakeReviewerLauncher{}, nil)
	// The coordinator is rebuilt with the shredding sender: newCoordinatorWithFix
	// takes a *fakeMessageSender specifically, so wire this one directly. Only
	// the store and clock it built are reused.
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store: store, Spawner: spawner, SessionFacts: sessionFacts, WorkspaceFacts: workspaceFacts,
		ReviewRuns: reviewRuns, ReviewerLauncher: &fakeReviewerLauncher{}, MessageSender: sender,
		Clock: clk.Now, NewID: func() string { idSeq++; return fmt.Sprintf("id%d", idSeq) },
	})

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	driveToChangesRequestedBody(t, c, store, clk, sessionFacts, workspaceFacts, reviewRuns, created.Run.ID, productionFinding)

	if sender.calls != 1 {
		t.Fatalf("sender calls = %d, want 1", sender.calls)
	}
	// AO built a complete prompt: that half was never in doubt and the ledger
	// must keep saying so.
	if !strings.Contains(sender.sent, productionFinding) {
		t.Fatalf("AO did not build a complete prompt; this test needs the transport to be the only defect")
	}

	detail, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	report := fixStepFrom(detail).FixDelivery
	if report == nil {
		t.Fatalf("no FixDelivery report")
	}
	if !report.FindingsEmbedded || report.FindingsDigest != workflowcore.FindingsDigest(productionFinding) {
		t.Fatalf("AO's own record no longer proves it built the findings into the prompt: %+v", report)
	}
	if report.ReceiptMatch != "other" {
		t.Fatalf("ReceiptMatch = %q, want \"other\" — a shredded delivery must be visible as "+
			"AO and the agent holding different bytes", report.ReceiptMatch)
	}
}

// TestIntactFixPromptReportsAMatchingReceipt is the positive control: a
// transport that delivers what AO sent reports "match", so "other" above is a
// real signal rather than a field that is always wrong.
func TestIntactFixPromptReportsAMatchingReceipt(t *testing.T) {
	ctx := context.Background()
	sessionFacts := newFakeSessionFacts()
	sender := &recordingMessageSender{facts: sessionFacts}
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	_, store, clk := newCoordinatorWithFix(spawner, sessionFacts, workspaceFacts, reviewRuns, &fakeReviewerLauncher{}, nil)
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store: store, Spawner: spawner, SessionFacts: sessionFacts, WorkspaceFacts: workspaceFacts,
		ReviewRuns: reviewRuns, ReviewerLauncher: &fakeReviewerLauncher{}, MessageSender: sender,
		Clock: clk.Now, NewID: func() string { idSeq++; return fmt.Sprintf("id%d", idSeq) },
	})
	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	driveToChangesRequestedBody(t, c, store, clk, sessionFacts, workspaceFacts, reviewRuns, created.Run.ID, productionFinding)

	detail, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	report := fixStepFrom(detail).FixDelivery
	if report == nil {
		t.Fatalf("no FixDelivery report")
	}
	if report.ReceiptMatch != "match" {
		t.Fatalf("ReceiptMatch = %q, want \"match\" for an intact delivery", report.ReceiptMatch)
	}
}

// recordingMessageSender is an honest transport: the session records exactly
// the bytes it was sent.
type recordingMessageSender struct {
	facts *fakeSessionFacts
	calls int
}

func (s *recordingMessageSender) Send(_ context.Context, id domain.SessionID, message string, _ *ports.SpawnAttachment) error {
	s.calls++
	if rec, ok, _ := s.facts.GetSession(context.Background(), id); ok {
		rec.Metadata.LatestUserPrompt = message
		s.facts.put(rec)
	}
	return nil
}
