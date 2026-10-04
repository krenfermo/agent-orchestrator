package workflow_test

// Incident wf-57f90ff2 (2026-08-23), workflow half of the transport fix.
//
// Delivery used to mean "MessageSender.Send returned nil", which in turn meant
// "the tmux commands exited 0". For a pane in copy-mode that was true of a
// prompt the agent never received: the paste queued, the Enter was swallowed by
// the mode, and the 15 KB fix prompt sat in Codex's composer as an unsubmitted
// draft while AO recorded a delivered fix cycle and then stopped the run over
// work the worker had never been asked to do. A later re-delivery pasted a
// SECOND copy of the same prompt into the same draft.
//
// These tests pin what "delivered" is now allowed to mean, and — the property
// that matters most once a draft exists — that AO submits what is already there
// rather than ever sending the prompt twice.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// ---- 1: load + submit succeeds -------------------------------------------

func TestFixPromptSubmittedIsAnOrdinaryDeliveredCycle(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.useReportingSender(ports.PromptSubmitted)
	got := f.driveToFixDispatch()

	if fixStepFrom(got).Step.State != domain.WorkflowStepRunning {
		t.Fatalf("fix step state = %q, want running", fixStepFrom(got).Step.State)
	}
	if f.runState() == domain.WorkflowRunNeedsAttention {
		t.Fatal("a submitted fix prompt was escalated to a human")
	}
	if n := f.countCheckpointPhase(workflowcore.ReasonFixPromptNotSubmitted); n != 0 {
		t.Fatalf("not-submitted checkpoints = %d, want 0", n)
	}
	if n := f.countCheckpointPhase("fix_dispatched"); n != 1 {
		t.Fatalf("fix_dispatched checkpoints = %d, want exactly 1", n)
	}
	if f.reporting.submitCalls != 0 {
		t.Fatalf("submit-only retries = %d, want 0: nothing needed resubmitting", f.reporting.submitCalls)
	}
}

// ---- 2: composer holds the text and the submit never lands ----------------

func TestFixPromptLeftInComposerIsNotADeliveredCycle(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.useReportingSender(ports.PromptLoadedNotSubmitted)
	got := f.driveToFixDispatch()

	if n := f.countCheckpointPhase("fix_dispatched"); n != 0 {
		t.Fatalf("fix_dispatched checkpoints = %d, want 0: a draft is not a delivery", n)
	}
	if n := f.countCheckpointPhase(workflowcore.ReasonFixPromptNotSubmitted); n != 1 {
		t.Fatalf("fix_prompt_not_submitted checkpoints = %d, want exactly 1", n)
	}
	if f.runState() != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", f.runState())
	}
	if st := fixStepFrom(got).Step.State; st != domain.WorkflowStepWaiting {
		t.Fatalf("fix step state = %q, want waiting (non-terminal, still submittable)", st)
	}
	// The cycle is not counted as delivered, so no fix budget was spent.
	if n := f.fixAttempts(); n != 0 {
		t.Fatalf("fix attempts = %d, want 0: an unsubmitted prompt must not consume a cycle", n)
	}
	// Bounded submit-only retries were spent, and NOT a single re-send.
	if f.reporting.submitCalls != 2 {
		t.Fatalf("submit-only retries = %d, want 2", f.reporting.submitCalls)
	}
	if f.sender.calls != 1 {
		t.Fatalf("prompt sends = %d, want exactly 1: the prompt must never be re-sent", f.sender.calls)
	}
}

// A submit-only retry that works mid-way is an ordinary delivery: the agent has
// the turn, and no prompt was ever sent twice.
func TestFixPromptSubmittedByRetryIsDelivered(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.useReportingSender(ports.PromptLoadedNotSubmitted)
	f.reporting.submitResults = []ports.PromptSubmission{ports.PromptSubmitted}
	f.driveToFixDispatch()

	if n := f.countCheckpointPhase("fix_dispatched"); n != 1 {
		t.Fatalf("fix_dispatched checkpoints = %d, want 1", n)
	}
	if f.runState() == domain.WorkflowRunNeedsAttention {
		t.Fatal("a prompt submitted on retry was escalated to a human")
	}
	if f.sender.calls != 1 {
		t.Fatalf("prompt sends = %d, want exactly 1", f.sender.calls)
	}
}

// ---- 3: ambiguous ---------------------------------------------------------

// AO could not read the composer. Missing evidence is not evidence of failure:
// the delivery keeps its ordinary shape, and the workflow's own dispatch-relative
// liveness gate decides what the worker did with it.
func TestAmbiguousSubmissionKeepsTheOrdinaryDeliveryShape(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.useReportingSender(ports.PromptSubmissionAmbiguous)
	f.driveToFixDispatch()

	if n := f.countCheckpointPhase("fix_dispatched"); n != 1 {
		t.Fatalf("fix_dispatched checkpoints = %d, want 1", n)
	}
	if n := f.countCheckpointPhase(workflowcore.ReasonFixPromptNotSubmitted); n != 0 {
		t.Fatalf("not-submitted checkpoints = %d, want 0: ambiguity is not a verdict", n)
	}
	if f.reporting.submitCalls != 0 {
		t.Fatalf("submit-only retries = %d, want 0", f.reporting.submitCalls)
	}
}

// A sender with no composer visibility at all behaves exactly as it did before
// any of this existed.
func TestSenderWithoutSubmissionReportingIsUnchanged(t *testing.T) {
	f := newFixRecoveryFixture(t) // plain fakeMessageSender
	f.driveToFixDispatch()

	if n := f.countCheckpointPhase("fix_dispatched"); n != 1 {
		t.Fatalf("fix_dispatched checkpoints = %d, want 1", n)
	}
	if n := f.countCheckpointPhase(workflowcore.ReasonFixPromptNotSubmitted); n != 0 {
		t.Fatalf("not-submitted checkpoints = %d, want 0", n)
	}
}

// ---- 4: the retry never duplicates a prompt -------------------------------

// The incident's own sequel: a stop is resumed while the prompt is STILL in the
// composer. AO must submit what is there, never paste a second copy.
func TestResumeSubmitsThePendingPromptInsteadOfResendingIt(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.useReportingSender(ports.PromptLoadedNotSubmitted)
	f.driveToFixDispatch()
	sendsAfterDispatch := f.sender.calls

	// The composer still holds AO's own prompt; the next submit will land.
	f.reporting.composer = ports.PromptLoadedNotSubmitted
	f.reporting.submitResults = []ports.PromptSubmission{ports.PromptSubmitted}
	f.reporting.submitCalls = 0

	got := f.continueRun()

	if f.sender.calls != sendsAfterDispatch {
		t.Fatalf("prompt sends = %d, want still %d: the resume re-sent a prompt that was already loaded",
			f.sender.calls, sendsAfterDispatch)
	}
	if f.reporting.submitCalls == 0 {
		t.Fatal("the resume never submitted the pending prompt")
	}
	if n := f.countCheckpointPhase("fix_cycle_redelivery"); n != 0 {
		t.Fatalf("re-delivery checkpoints = %d, want 0: nothing needed re-delivering", n)
	}
	if st := fixStepFrom(got).Step.State; st != domain.WorkflowStepRunning {
		t.Fatalf("fix step state = %q, want running after the submit landed", st)
	}
	if f.runState() == domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want the stop released", f.runState())
	}
	if n := f.fixAttempts(); n > 1 {
		t.Fatalf("fix attempts = %d, want at most 1: submitting a pending prompt must not open a cycle", n)
	}
}

// ---- 5: restart between paste and submit ----------------------------------

// The daemon dies after the prompt reached the composer and before it was
// submitted. Over the same rows, a restarted coordinator must submit what is
// there — never re-send, which would leave two copies in one draft.
func TestRestartBetweenLoadAndSubmitSubmitsWithoutResending(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.useReportingSender(ports.PromptLoadedNotSubmitted)
	f.driveToFixDispatch()
	sends := f.sender.calls

	f.c = f.restart()
	f.reporting.composer = ports.PromptLoadedNotSubmitted
	f.reporting.submitResults = []ports.PromptSubmission{ports.PromptSubmitted}
	f.reporting.submitCalls = 0

	f.continueRun()

	if f.sender.calls != sends {
		t.Fatalf("prompt sends across the restart = %d, want still %d", f.sender.calls, sends)
	}
	if f.reporting.submitCalls == 0 {
		t.Fatal("the restarted coordinator did not submit the pending prompt")
	}
	if f.runState() == domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want the stop released after the submit", f.runState())
	}
}

// ---- 6: a human's draft is never touched ----------------------------------

// AO may only act on a pending draft it can attribute to itself. Without a
// dispatch record naming this session and cycle there is no attribution, so AO
// must neither submit the draft (it could be a half-typed human message) nor
// paste over it.
func TestUnattributablePendingInputIsNeverSubmittedOrOverwritten(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.useReportingSender(ports.PromptSubmitted)
	f.driveToFixDispatch()
	f.silentSinceBeforeDispatch()
	f.parkAsFixStop(workflowcore.ReasonFixCycleNotStarted, "the worker never started this cycle")

	// Someone is typing in the session. AO can see a draft but this run has no
	// claim on it: drop the durable dispatch record that would attribute it.
	f.reporting.composer = ports.PromptLoadedNotSubmitted
	f.dropFixDispatchRecords()
	f.sender.calls = 0
	f.reporting.submitCalls = 0

	f.continueRun()

	if f.sender.calls != 0 {
		t.Fatalf("prompt sends = %d, want 0: AO pasted over input it could not attribute", f.sender.calls)
	}
	if f.reporting.submitCalls != 0 {
		t.Fatalf("submits = %d, want 0: AO submitted a draft it could not attribute to itself", f.reporting.submitCalls)
	}
	if f.runState() != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", f.runState())
	}
}

// A composer AO cannot read at all is the same refusal, for the same reason.
func TestUnreadableComposerBlocksAnyDelivery(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.useReportingSender(ports.PromptSubmitted)
	f.driveToFixDispatch()
	f.silentSinceBeforeDispatch()
	f.parkAsFixStop(workflowcore.ReasonFixCycleNotStarted, "the worker never started this cycle")
	f.reporting.composer = ports.PromptSubmissionAmbiguous
	f.sender.calls = 0

	f.continueRun()

	if f.sender.calls != 0 {
		t.Fatalf("prompt sends = %d, want 0 while the composer is unreadable", f.sender.calls)
	}
	if n := f.countCheckpointPhase("fix_cycle_redelivery"); n != 0 {
		t.Fatalf("re-delivery checkpoints = %d, want 0", n)
	}
}

// ---- 7: idempotence on the same prompt receipt ----------------------------

// Repeated Continues against an unchanged pending draft submit it, and never
// accumulate sends, cycles or attempts.
func TestRepeatedResumesNeverAccumulatePrompts(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.useReportingSender(ports.PromptLoadedNotSubmitted)
	f.driveToFixDispatch()
	sends := f.sender.calls
	receipt := f.sender.lastMsg

	for i := 0; i < 4; i++ {
		f.reporting.composer = ports.PromptLoadedNotSubmitted
		f.parkAsFixStop(workflowcore.ReasonFixPromptNotSubmitted, "still in the composer")
		f.continueRun()
	}

	if f.sender.calls != sends {
		t.Fatalf("prompt sends after four resumes = %d, want still %d", f.sender.calls, sends)
	}
	if f.sender.lastMsg != receipt {
		t.Fatal("the prompt text changed across resumes: this is no longer the same cycle")
	}
	if n := f.fixAttempts(); n > 1 {
		t.Fatalf("fix attempts = %d, want at most 1", n)
	}
	if n := f.countCheckpointPhase("fix_cycle_redelivery"); n != 0 {
		t.Fatalf("re-delivery checkpoints = %d, want 0", n)
	}
}

// ---- 8: a turn boundary after the submit is what proves the cycle started --

// The transport's "submitted" is about the composer. What proves the CYCLE
// began is the agent's own signal postdating the dispatch — the same rule that
// stops a stale idle from being read as a verdict.
func TestActivityAfterSubmitIsWhatProvesTheCycleStarted(t *testing.T) {
	f := newFixRecoveryFixture(t)
	f.useReportingSender(ports.PromptSubmitted)
	f.driveToFixDispatch()
	dispatchedAt := f.intentCreatedAt()

	// Submitted, but the agent has still said nothing: not yet a verdict.
	f.silentSinceBeforeDispatch()
	f.clk.Advance(76 * time.Second)
	f.poll(1)
	if f.runState() == domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q: a submitted prompt with no signal yet is not a stop", f.runState())
	}

	// The agent reports a turn boundary after the dispatch: the cycle is
	// genuinely running, and the workspace change resolves it.
	f.mutateSession(func(rec *domain.SessionRecord) {
		rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: dispatchedAt.Add(2 * time.Minute)}
		rec.TurnCompletedAt = dispatchedAt.Add(2 * time.Minute)
	})
	f.workspaceFacts.obs.HeadSHA = "worker-produced-a-change"
	f.clk.Advance(time.Minute)
	got := f.poll(1)

	if st := fixStepFrom(got).Step.State; st != domain.WorkflowStepWaiting {
		t.Fatalf("fix step state = %q, want waiting (the cycle delivered a change)", st)
	}
	if n := f.countCheckpointPhase(workflowcore.ReasonFixCycleNotStarted); n != 0 {
		t.Fatalf("not-started checkpoints = %d, want 0: the worker demonstrably started", n)
	}
	if n := f.countCheckpointPhase(workflowcore.ReasonFixNoVerifiableChange); n != 0 {
		t.Fatalf("no-verifiable-change checkpoints = %d, want 0: there was a change", n)
	}
}

// ---- fixture --------------------------------------------------------------

// reportingSender is a fakeMessageSender that also answers the composer
// questions, so a test can model each of the four delivery outcomes.
type reportingSender struct {
	*fakeMessageSender
	// submission is what a fresh send reports.
	submission ports.PromptSubmission
	// submitResults are the successive verdicts of submit-only retries; the
	// last one repeats once the script is exhausted.
	submitResults []ports.PromptSubmission
	submitCalls   int
	// composer is what ComposerState answers. Defaults to "empty".
	composer ports.PromptSubmission
}

func (s *reportingSender) SendReportingSubmission(ctx context.Context, id domain.SessionID, message string, att *ports.SpawnAttachment) (ports.PromptSubmission, error) {
	if err := s.Send(ctx, id, message, att); err != nil {
		return ports.PromptSubmissionUnset, err
	}
	if strings.TrimSpace(message) == "" {
		return ports.PromptSubmissionUnset, nil
	}
	return s.submission, nil
}

func (s *reportingSender) SubmitPending(_ context.Context, _ domain.SessionID) (ports.PromptSubmission, error) {
	s.submitCalls++
	switch len(s.submitResults) {
	case 0:
		return s.submission, nil
	case 1:
		return s.submitResults[0], nil
	default:
		v := s.submitResults[0]
		s.submitResults = s.submitResults[1:]
		return v, nil
	}
}

func (s *reportingSender) ComposerState(_ context.Context, _ domain.SessionID) ports.PromptSubmission {
	if s.composer == "" {
		return ports.PromptSubmitted // empty composer
	}
	return s.composer
}

// useReportingSender swaps in a sender with composer visibility. Must be called
// before the run is driven, so the coordinator is built around it.
func (f *fixRecoveryFixture) useReportingSender(submission ports.PromptSubmission) {
	f.t.Helper()
	f.reporting = &reportingSender{fakeMessageSender: f.sender, submission: submission}
	f.senderOverride = f.reporting
	f.c = f.newCoordinator()
}

// dropFixDispatchRecords removes the checkpoints that would let AO attribute a
// pending composer draft to itself, modelling a draft this run has no claim on.
func (f *fixRecoveryFixture) dropFixDispatchRecords() {
	f.t.Helper()
	kept := f.store.checkpoints[f.runID][:0]
	for _, cp := range f.store.checkpoints[f.runID] {
		switch cp.DurablePhase {
		case "fix_dispatched", "fix_dispatch_intent", "fix_cycle_redelivery":
			continue
		}
		kept = append(kept, cp)
	}
	f.store.checkpoints[f.runID] = kept
}
