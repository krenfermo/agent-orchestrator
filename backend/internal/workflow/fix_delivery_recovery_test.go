package workflow_test

// The incident, wf-6528a538 (2026-08-22):
//
//	work=completed  review=waiting  fix=waiting  verify=pending
//	run=needs_attention
//	checkpoint fix_dispatch_ambiguous
//	  "ambiguous_fix_delivery: cannot confirm message delivery after restart"
//	...written again, identically, roughly every two seconds. No pending wake.
//
// The daemon had restarted somewhere between the fix outbox entry reaching
// `dispatched` and MessageSender.Send returning. dispatchFixStep had no fact
// either way, so it parked the run and wrote a checkpoint — and because the
// cycle never got its attempt row, the next poll re-derived exactly the same
// dispatch and did it all again. A healthy autonomous fix became permanent
// human intervention purely because AO restarted.
//
// These tests pin the whole decision table, at the exact dangerous point: the
// durable state is rolled back to the crash window (or built as it, for the
// crashed-before-Send case) and recovery is then driven through the ordinary
// GetRun poll and through a genuinely restarted Coordinator over the same rows.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// ---- A: proven not delivered ------------------------------------------------

// Case A: the crash landed BEFORE Send. The pre-delivery record AO always
// writes first is absent, which is positive proof that nothing reached the
// agent — so recovery must deliver, exactly once, rather than escalate.
func TestFixDeliveryProvenNotSentIsDeliveredOnceAfterRestart(t *testing.T) {
	f := newFixRecoveryFixture(t)

	// The durable state of a crash between "outbox -> dispatched" and Send:
	// the entry exists and is dispatched, and nothing else was written.
	f.seedDispatchedOutboxEntry(1, 0)
	got := f.driveToFixDispatch()

	if f.sender.calls != 1 {
		t.Fatalf("MessageSender.Send calls = %d, want exactly 1: a provable non-delivery must be re-sent", f.sender.calls)
	}
	fix := fixStepFrom(got)
	if fix.Step.State != domain.WorkflowStepRunning {
		t.Fatalf("fix step state = %q, want running after the recovered delivery", fix.Step.State)
	}
	if f.runState() == domain.WorkflowRunNeedsAttention {
		t.Fatal("a provable non-delivery was escalated to a human instead of being re-sent")
	}
	if n := f.countCheckpointPhase(workflowcore.ReasonFixDispatchAmbiguous); n != 0 {
		t.Fatalf("ambiguity checkpoints = %d, want 0: nothing here was ambiguous", n)
	}
	if n := f.countCheckpointPhase("fix_dispatch_intent"); n != 1 {
		t.Fatalf("dispatch intent records = %d, want exactly 1", n)
	}
	f.assertOneAttemptOneCycle()

	// And the poll that follows must not send it again.
	f.poll(3)
	if f.sender.calls != 1 {
		t.Fatalf("MessageSender.Send calls after further polls = %d, want still 1", f.sender.calls)
	}
}

// ---- B: proven delivered ----------------------------------------------------

// Case B: the crash landed AFTER Send but before the bookkeeping. The session
// carries the receipt of this exact cycle's prompt, so recovery must adopt the
// cycle and resume observation — never send it a second time.
func TestFixDeliveryProvenDeliveredIsNotResentAfterRestart(t *testing.T) {
	for _, harness := range []domain.AgentHarness{domain.HarnessClaudeCode, domain.HarnessCodex} {
		t.Run(string(harness), func(t *testing.T) {
			f := newFixRecoveryFixture(t)
			f.driveToFixDispatch()
			f.setWorkerHarness(harness)
			delivered := f.sender.lastMsg

			f.crashAfterSend(deliveryEvidence{receipt: delivered})

			got := f.poll(1)
			if f.sender.calls != 0 {
				t.Fatalf("MessageSender.Send calls after restart = %d, want 0: a delivered prompt must never be re-sent", f.sender.calls)
			}
			if f.runState() == domain.WorkflowRunNeedsAttention {
				t.Fatal("a provably delivered fix was escalated to a human")
			}
			fix := fixStepFrom(got)
			if fix.Step.State != domain.WorkflowStepRunning {
				t.Fatalf("fix step state = %q, want running so observeFixStep resumes", fix.Step.State)
			}
			if n := f.countCheckpointPhase("fix_dispatched"); n != 1 {
				t.Fatalf("fix_dispatched checkpoints = %d, want exactly 1: the interrupted bookkeeping was not completed", n)
			}
			if n := f.countCheckpointPhase(workflowcore.ReasonFixDispatchAmbiguous); n != 0 {
				t.Fatalf("ambiguity checkpoints = %d, want 0", n)
			}
			f.assertOneAttemptOneCycle()
		})
	}
}

// ---- C: the worker began the expected turn ----------------------------------

// Case C: no prompt receipt survived, but the session reported activity that
// postdates the dispatch. The agent got something and started working after AO
// asked it to — enough to resume observation automatically.
func TestFixDeliveryResumesWhenWorkerStartedTheExpectedTurn(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()

	f.crashAfterSend(deliveryEvidence{activeAfterDispatch: true})

	got := f.poll(1)
	if f.sender.calls != 0 {
		t.Fatalf("MessageSender.Send calls = %d, want 0: the worker had already begun the turn", f.sender.calls)
	}
	if f.runState() == domain.WorkflowRunNeedsAttention {
		t.Fatal("AO escalated a fix whose worker was demonstrably already working on it")
	}
	if fixStepFrom(got).Step.State != domain.WorkflowStepRunning {
		t.Fatalf("fix step state = %q, want running", fixStepFrom(got).Step.State)
	}
	f.assertOneAttemptOneCycle()
}

// A reported turn COMPLETION after the dispatch is the same proof from the
// other end: TurnCompletedAt is cleared when a turn starts, so a completion
// stamped after the dispatch can only belong to the turn AO asked for.
func TestFixDeliveryResumesWhenWorkerCompletedATurnAfterDispatch(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()

	f.crashAfterSend(deliveryEvidence{turnCompletedAfterDispatch: true})

	f.poll(1)
	if f.sender.calls != 0 {
		t.Fatalf("MessageSender.Send calls = %d, want 0", f.sender.calls)
	}
	if f.runState() == domain.WorkflowRunNeedsAttention {
		t.Fatal("AO escalated a fix whose worker had already reported finishing the turn")
	}
}

// Activity that PREDATES the dispatch proves nothing, and must not be mistaken
// for a response to it.
func TestFixDeliveryDoesNotTreatPreDispatchActivityAsProof(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()

	f.crashAfterSend(deliveryEvidence{activeBeforeDispatch: true})

	f.poll(1)
	if f.sender.calls != 0 {
		t.Fatalf("MessageSender.Send calls = %d, want 0: delivery was not disproven either", f.sender.calls)
	}
	if f.runState() != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention: stale activity was accepted as proof of delivery", f.runState())
	}
}

// ---- D/E/F: true ambiguity, escalated once ----------------------------------

// Cases D, E and F together: genuinely unprovable delivery escalates exactly
// once, stays escalated across 50 reconciles and across a second restart, and
// never sends anything.
func TestUnprovableFixDeliveryEscalatesOnceAndStaysQuiet(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()

	// No receipt, no activity after the dispatch: nothing AO can prove.
	f.crashAfterSend(deliveryEvidence{})

	// D. One transition into needs_attention, with a precise account.
	f.poll(1)
	if f.runState() != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention for a genuinely unprovable delivery", f.runState())
	}
	if f.sender.calls != 0 {
		t.Fatalf("MessageSender.Send calls = %d, want 0: an unprovable delivery must never be duplicated", f.sender.calls)
	}
	detail, err := f.c.GetRun(context.Background(), f.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	verdict := workflowcore.ClassifyAttention(detail, detail.Questions, workflowcore.PhaseNeedsAttention)
	if verdict.Reason != workflowcore.ReasonFixDispatchAmbiguous {
		t.Fatalf("attention reason = %q, want %q", verdict.Reason, workflowcore.ReasonFixDispatchAmbiguous)
	}
	if verdict.Attention != workflowcore.AttentionHuman || verdict.Action == "" {
		t.Fatalf("an exhausted-evidence stop must name a concrete human action: %#v", verdict)
	}
	note := f.latestCheckpointPhaseNextAction(workflowcore.ReasonFixDispatchAmbiguous)
	for _, want := range []string{"fix cycle 1", "receipt", "turn boundary"} {
		if !strings.Contains(note, want) {
			t.Fatalf("escalation detail %q does not say what was inconclusive (missing %q)", note, want)
		}
	}

	// E. 50 more reconciles over the same unchanged condition.
	f.poll(50)
	if n := f.countCheckpointPhase(workflowcore.ReasonFixDispatchAmbiguous); n != 1 {
		t.Fatalf("ambiguity checkpoints after 51 reconciles = %d, want exactly 1", n)
	}
	if f.sender.calls != 0 {
		t.Fatalf("MessageSender.Send calls = %d, want still 0", f.sender.calls)
	}
	f.assertOneCycleNoDuplicates()

	// F. A second restart: a brand-new Coordinator over the same durable rows.
	restarted := f.restart()
	for i := 0; i < 5; i++ {
		f.clk.Advance(2 * time.Second)
		if _, err := restarted.GetRun(context.Background(), f.runID); err != nil {
			t.Fatalf("GetRun after second restart: %v", err)
		}
	}
	if n := f.countCheckpointPhase(workflowcore.ReasonFixDispatchAmbiguous); n != 1 {
		t.Fatalf("ambiguity checkpoints after a second restart = %d, want still exactly 1", n)
	}
	if f.sender.calls != 0 {
		t.Fatalf("MessageSender.Send calls after a second restart = %d, want still 0", f.sender.calls)
	}
	f.assertOneCycleNoDuplicates()
}

// A material change in the evidence is a new fact and IS recorded — dedup must
// silence repetition, not the truth.
func TestChangedFixDeliveryEvidenceIsRecordedAgain(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()
	f.crashAfterSend(deliveryEvidence{})

	f.poll(5)
	if n := f.countCheckpointPhase(workflowcore.ReasonFixDispatchAmbiguous); n != 1 {
		t.Fatalf("ambiguity checkpoints = %d, want 1", n)
	}

	// The worker session is torn down: still unprovable, but a different
	// situation, and one the user is told about differently.
	f.mutateSession(func(rec *domain.SessionRecord) { rec.IsTerminated = true })
	f.poll(5)
	if n := f.countCheckpointPhase(workflowcore.ReasonFixDispatchAmbiguous); n != 2 {
		t.Fatalf("ambiguity checkpoints after the session was terminated = %d, want 2 (the condition changed)", n)
	}
	if note := f.latestCheckpointPhaseNextAction(workflowcore.ReasonFixDispatchAmbiguous); !strings.Contains(note, "terminated") {
		t.Fatalf("the new escalation does not mention the terminated session: %q", note)
	}
}

// ---- G: recovery releases the run, and the parent mirror with it ------------

// Case G: an escalated ambiguity that later becomes provable must release the
// run by itself. The child returning to an active, non-human-owned lifecycle is
// exactly the input the master reconciliation reads, so the parent's mirrored
// child_needs_attention clears through the existing path (proven end to end by
// TestParentReconcilesStaleChildAttentionWhenTheChildResumes).
func TestProvenFixDeliveryReleasesTheEscalatedRun(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()
	delivered := f.sender.lastMsg
	f.crashAfterSend(deliveryEvidence{})

	f.poll(1)
	if f.runState() != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention before the evidence arrives", f.runState())
	}

	// The receipt lands late — the session row is read again and now carries
	// this cycle's prompt.
	f.mutateSession(func(rec *domain.SessionRecord) { rec.Metadata.LatestUserPrompt = delivered })

	got := f.poll(1)
	if f.runState() == domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q: proof arrived and the stop was not released", f.runState())
	}
	if f.sender.calls != 0 {
		t.Fatalf("MessageSender.Send calls = %d, want 0", f.sender.calls)
	}
	if fixStepFrom(got).Step.State != domain.WorkflowStepRunning {
		t.Fatalf("fix step state = %q, want running", fixStepFrom(got).Step.State)
	}
	if n := f.countCheckpointPhase("attention_cleared"); n != 1 {
		t.Fatalf("attention_cleared checkpoints = %d, want exactly 1", n)
	}

	// The child is no longer billing a human for anything — which is what the
	// parent's reconcileMirroredChildStop tests when it decides whether its
	// mirror is still true.
	detail, err := f.c.GetRun(context.Background(), f.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail, Questions: detail.Questions})
	if life.Attention == workflowcore.AttentionHuman {
		t.Fatalf("the recovered child still asks for a human decision: %#v", life)
	}
	f.assertOneCycleNoDuplicates()
}

// A run stopped for a DIFFERENT reason is never released by this recovery.
func TestProvenFixDeliveryDoesNotClearAnUnrelatedStop(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.driveToFixDispatch()
	delivered := f.sender.lastMsg
	f.crashAfterSend(deliveryEvidence{receipt: delivered})

	// The run is parked on something else entirely, recorded after the crash.
	f.parkOnUnrelatedStop(workflowcore.ReasonFixBudgetExhausted,
		"the reviewer still requests changes after every allowed fix cycle")

	f.poll(3)
	if f.runState() != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention: an unrelated stop was cleared", f.runState())
	}
	if n := f.countCheckpointPhase("attention_cleared"); n != 0 {
		t.Fatalf("attention_cleared checkpoints = %d, want 0", n)
	}
}

// ---- fixture ---------------------------------------------------------------

type fixRecoveryFixture struct {
	t              *testing.T
	c              *workflowcore.Coordinator
	store          *fakeStore
	clk            *fakeClock
	sessionFacts   *fakeSessionFacts
	workspaceFacts *fakeWorkspaceFacts
	reviewRuns     *fakeReviewRuns
	launcher       *fakeReviewerLauncher
	spawner        *fakeSpawner
	sender         *fakeMessageSender
	runID          string
	fixStepID      string
	workSessionID  domain.SessionID
	idSeq          int
}

func newFixRecoveryFixture(t *testing.T) *fixRecoveryFixture {
	t.Helper()
	f := &fixRecoveryFixture{
		t:              t,
		sessionFacts:   newFakeSessionFacts(),
		workspaceFacts: &fakeWorkspaceFacts{},
		reviewRuns:     newFakeReviewRuns(),
		launcher:       &fakeReviewerLauncher{},
		sender:         &fakeMessageSender{},
	}
	f.spawner = &fakeSpawner{
		rec:   domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}},
		facts: f.sessionFacts,
	}
	f.store = newFakeStore()
	f.clk = &fakeClock{t: time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)}
	f.c = f.newCoordinator()

	created, err := f.c.CreateRun(context.Background(), "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	f.runID = created.Run.ID
	return f
}

// newCoordinator builds a Coordinator over this fixture's durable store. Called
// again by restart() to model a daemon that came back over the same rows.
func (f *fixRecoveryFixture) newCoordinator() *workflowcore.Coordinator {
	return workflowcore.New(workflowcore.Deps{
		Store:            f.store,
		Spawner:          f.spawner,
		SessionFacts:     f.sessionFacts,
		WorkspaceFacts:   f.workspaceFacts,
		ReviewRuns:       f.reviewRuns,
		ReviewerLauncher: f.launcher,
		MessageSender:    f.sender,
		Clock:            f.clk.Now,
		NewID: func() string {
			f.idSeq++
			return fmt.Sprintf("id%d", f.idSeq)
		},
	})
}

func (f *fixRecoveryFixture) restart() *workflowcore.Coordinator {
	f.t.Helper()
	return f.newCoordinator()
}

// driveToFixDispatch takes the run through work completion, cycle-1 review and
// a changes_requested verdict — the point at which the fix cycle dispatches.
func (f *fixRecoveryFixture) driveToFixDispatch() workflowcore.RunDetail {
	f.t.Helper()
	got := driveToChangesRequested(f.t, f.c, f.store, f.clk, f.sessionFacts, f.workspaceFacts, f.reviewRuns, f.runID)
	fix := fixStepFrom(got)
	f.fixStepID = fix.Step.ID
	if sid := workStepFrom(got).Step.SessionID; sid != nil {
		f.workSessionID = domain.SessionID(*sid)
	}
	return got
}

// seedDispatchedOutboxEntry writes the durable state of a daemon that died
// between claiming this cycle's outbox entry and calling Send: the entry is
// `dispatched` and NOTHING else was written — no pre-delivery record, no
// attempt, no receipt. It must be seeded before the dispatch is first reached,
// so the fix step id is derived the way dispatchFixStep itself derives the key.
func (f *fixRecoveryFixture) seedDispatchedOutboxEntry(cycle, transportAttempt int) {
	f.t.Helper()
	detail, err := f.c.GetRun(context.Background(), f.runID)
	if err != nil {
		f.t.Fatalf("GetRun: %v", err)
	}
	stepID := fixStepFrom(detail).Step.ID
	key := "workflow-step-fix:" + stepID + ":cycle" + fmt.Sprint(cycle)
	if transportAttempt > 0 {
		key += ":transport" + fmt.Sprint(transportAttempt)
	}
	at := f.clk.Now()
	f.store.outbox[key] = domain.WorkflowOutboxEntry{
		ID:             "wfo-crashed",
		WorkflowRunID:  f.runID,
		WorkflowStepID: &stepID,
		IdempotencyKey: key,
		CommandType:    domain.WorkflowOutboxSendMessage,
		Payload:        "{}",
		Status:         domain.WorkflowOutboxDispatched,
		CreatedAt:      at,
		DispatchedAt:   &at,
	}
}

// deliveryEvidence is what the worker session will say about the interrupted
// delivery once the daemon comes back.
type deliveryEvidence struct {
	// receipt is the prompt text the session recorded as its last user prompt.
	receipt string
	// activeAfterDispatch marks the session actively working at a moment that
	// postdates the dispatch intent.
	activeAfterDispatch bool
	// activeBeforeDispatch marks activity that PREDATES it — deliberately not
	// proof of anything.
	activeBeforeDispatch bool
	// turnCompletedAfterDispatch marks a reported turn boundary after it.
	turnCompletedAfterDispatch bool
}

// crashAfterSend rolls the durable state back to the exact dangerous point:
// Send has run (or may have run), and the daemon died before any of the
// bookkeeping that follows it. The pre-delivery record and the outbox entry
// survive — they were written first — while the attempt row, the acknowledgement
// and the fix_dispatched checkpoint do not.
//
// The Send that produced this state is then un-counted, so every assertion
// below about Send calls is about what RECOVERY did, not about the original
// dispatch.
func (f *fixRecoveryFixture) crashAfterSend(ev deliveryEvidence) {
	f.t.Helper()
	if f.fixStepID == "" {
		f.t.Fatal("crashAfterSend before the fix cycle was ever dispatched")
	}
	dispatchedAt := f.intentCreatedAt()

	delete(f.store.attempts, f.fixStepID)
	for key, entry := range f.store.outbox {
		if entry.WorkflowStepID != nil && *entry.WorkflowStepID == f.fixStepID {
			entry.Status = domain.WorkflowOutboxDispatched
			entry.AcknowledgedAt = nil
			f.store.outbox[key] = entry
		}
	}
	kept := f.store.checkpoints[f.runID][:0]
	for _, cp := range f.store.checkpoints[f.runID] {
		if cp.DurablePhase == "fix_dispatched" {
			continue
		}
		kept = append(kept, cp)
	}
	f.store.checkpoints[f.runID] = kept

	f.mutateSession(func(rec *domain.SessionRecord) {
		rec.Metadata.LatestUserPrompt = ev.receipt
		rec.Activity = domain.Activity{State: domain.ActivityIdle}
		rec.TurnCompletedAt = time.Time{}
		switch {
		case ev.activeAfterDispatch:
			rec.Activity = domain.Activity{State: domain.ActivityActive, LastActivityAt: dispatchedAt.Add(time.Second)}
		case ev.activeBeforeDispatch:
			rec.Activity = domain.Activity{State: domain.ActivityActive, LastActivityAt: dispatchedAt.Add(-time.Minute)}
		}
		if ev.turnCompletedAfterDispatch {
			rec.TurnCompletedAt = dispatchedAt.Add(time.Second)
		}
	})

	f.sender.calls = 0
	f.clk.Advance(time.Minute)
}

// intentCreatedAt is the timestamp of the pre-delivery record the dispatch
// wrote — the instant everything else is judged against.
func (f *fixRecoveryFixture) intentCreatedAt() time.Time {
	f.t.Helper()
	for _, cp := range f.store.checkpoints[f.runID] {
		if cp.DurablePhase == "fix_dispatch_intent" {
			return cp.CreatedAt
		}
	}
	f.t.Fatal("no fix_dispatch_intent checkpoint: the dispatch never recorded what it was about to deliver")
	return time.Time{}
}

func (f *fixRecoveryFixture) mutateSession(mutate func(*domain.SessionRecord)) {
	f.t.Helper()
	rec, ok, err := f.sessionFacts.GetSession(context.Background(), f.workSessionID)
	if err != nil || !ok {
		f.t.Fatalf("GetSession(%s): %v (found=%v)", f.workSessionID, err, ok)
	}
	mutate(&rec)
	f.sessionFacts.put(rec)
}

func (f *fixRecoveryFixture) setWorkerHarness(h domain.AgentHarness) {
	f.t.Helper()
	f.mutateSession(func(rec *domain.SessionRecord) { rec.Harness = h })
}

// parkOnUnrelatedStop moves the run to needs_attention for a reason that has
// nothing to do with delivery, and records it the way any stop site would.
func (f *fixRecoveryFixture) parkOnUnrelatedStop(reason, detail string) {
	f.t.Helper()
	ctx := context.Background()
	run, ok, err := f.store.GetWorkflowRun(ctx, f.runID)
	if err != nil || !ok {
		f.t.Fatalf("GetWorkflowRun: %v (found=%v)", err, ok)
	}
	if run.State != domain.WorkflowRunNeedsAttention {
		if _, err := f.store.UpdateWorkflowRunState(ctx, f.runID, run.State, domain.WorkflowRunNeedsAttention, f.clk.Now()); err != nil {
			f.t.Fatalf("park run: %v", err)
		}
	}
	f.clk.Advance(time.Second)
	if _, err := f.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-unrelated", WorkflowRunID: f.runID, ProjectID: run.ProjectID,
		NextAction: detail, DurablePhase: reason,
		PayloadVersion: "v1", RetryState: "{}", CreatedAt: f.clk.Now(),
	}); err != nil {
		f.t.Fatalf("CreateWorkflowCheckpoint: %v", err)
	}
}

// poll runs n GetRun calls, the frontend's own 2s reconcile.
func (f *fixRecoveryFixture) poll(n int) workflowcore.RunDetail {
	f.t.Helper()
	var detail workflowcore.RunDetail
	for i := 0; i < n; i++ {
		f.clk.Advance(2 * time.Second)
		got, err := f.c.GetRun(context.Background(), f.runID)
		if err != nil {
			f.t.Fatalf("GetRun poll %d: %v", i, err)
		}
		detail = got
	}
	return detail
}

func (f *fixRecoveryFixture) runState() domain.WorkflowRunState {
	f.t.Helper()
	run, ok, err := f.store.GetWorkflowRun(context.Background(), f.runID)
	if err != nil || !ok {
		f.t.Fatalf("GetWorkflowRun: %v (found=%v)", err, ok)
	}
	return run.State
}

func (f *fixRecoveryFixture) countCheckpointPhase(phase string) int {
	f.t.Helper()
	n := 0
	for _, cp := range f.store.checkpoints[f.runID] {
		if cp.DurablePhase == phase {
			n++
		}
	}
	return n
}

func (f *fixRecoveryFixture) latestCheckpointPhaseNextAction(phase string) string {
	f.t.Helper()
	note := ""
	for _, cp := range f.store.checkpoints[f.runID] {
		if cp.DurablePhase == phase {
			note = cp.NextAction
		}
	}
	return note
}

// assertOneAttemptOneCycle: recovery created the cycle's single attempt row and
// no second one, whichever branch it took.
func (f *fixRecoveryFixture) assertOneAttemptOneCycle() {
	f.t.Helper()
	attempts, err := f.store.ListWorkflowAttempts(context.Background(), f.fixStepID)
	if err != nil {
		f.t.Fatalf("ListWorkflowAttempts: %v", err)
	}
	if len(attempts) != 1 {
		f.t.Fatalf("fix step attempts = %d, want exactly 1", len(attempts))
	}
	f.assertOneCycleNoDuplicates()
}

// assertOneCycleNoDuplicates: one outbox entry for the cycle, at most one
// attempt, at most one pre-delivery record, and no second fix cycle — the
// idempotency the whole recovery rests on.
func (f *fixRecoveryFixture) assertOneCycleNoDuplicates() {
	f.t.Helper()
	entries := 0
	for _, entry := range f.store.outbox {
		if entry.WorkflowStepID != nil && *entry.WorkflowStepID == f.fixStepID {
			entries++
		}
	}
	if entries != 1 {
		f.t.Fatalf("fix outbox entries = %d, want exactly 1 (one logical dispatch identity)", entries)
	}
	attempts, err := f.store.ListWorkflowAttempts(context.Background(), f.fixStepID)
	if err != nil {
		f.t.Fatalf("ListWorkflowAttempts: %v", err)
	}
	if len(attempts) > 1 {
		f.t.Fatalf("fix step attempts = %d, want at most 1", len(attempts))
	}
	if n := f.countCheckpointPhase("fix_dispatch_intent"); n > 1 {
		f.t.Fatalf("dispatch intent records = %d, want at most 1: a delivery was re-attempted", n)
	}
	if n := f.countCheckpointPhase("prompt_transport_retry"); n != 0 {
		f.t.Fatalf("transport retries = %d, want 0: a restart must not consume the retry budget", n)
	}
}
