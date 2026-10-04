package workflow_test

// Checkpoint 8P-E.13C: prompt delivery for the fix step.
//
// Real incident (child wf-2261767d of master wf-fa49c2e0): work -> review ->
// verify -> verify_failed -> verify_fix_reentry -> fix dispatch, and the fix
// dispatch died with
//
//	dispatch_failed / fix dispatch failed (prompt_delivery_failed):
//	... tmux runtime: send message ...: exit status 1: command too long
//
// leaving the fix step FAILED (terminal, so nothing could re-dispatch it) and
// the child in needs_attention over a transport limit no human can act on.
//
// The transport itself is fixed in the tmux adapter (see its
// prompt_transport_test.go). These tests pin the workflow half: the prompt is
// delivered whole, its size and transport are recorded, and a refusal that
// provably delivered nothing is retried by AO within a bounded budget instead
// of becoming a person's problem.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// refusingMessageSender models the transport contract: the first
// refuseCount sends are refused BEFORE anything reaches the agent
// (ports.ErrPromptUndelivered — exactly what the tmux runtime returns for
// "command too long"), and every send records the exact bytes it was given.
type refusingMessageSender struct {
	refuseCount int
	calls       int
	delivered   []string
}

func (s *refusingMessageSender) Send(_ context.Context, _ domain.SessionID, message string, _ *ports.SpawnAttachment) error {
	s.calls++
	if s.calls <= s.refuseCount {
		return ports.ErrPromptUndelivered
	}
	s.delivered = append(s.delivered, message)
	return nil
}

func checkpointsWithPhase(t *testing.T, store *fakeStore, runID, phase string) []domain.WorkflowCheckpoint {
	t.Helper()
	cps, err := store.ListWorkflowCheckpoints(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	var out []domain.WorkflowCheckpoint
	for _, cp := range cps {
		if cp.DurablePhase == phase {
			out = append(out, cp)
		}
	}
	return out
}

// newFixCoordinatorOver builds a coordinator with the full work/review/fix
// stack over an EXISTING store and clock, so a test can simulate a daemon
// restart by building a second one over the same durable state. idPrefix keeps
// the two coordinators' generated ids distinct, exactly as two daemon processes
// would be.
func newFixCoordinatorOver(store *fakeStore, clk *fakeClock, spawner *fakeSpawner, sessionFacts *fakeSessionFacts, ws *fakeWorkspaceFacts, reviewRuns *fakeReviewRuns, sender workflowcore.MessageSender, idPrefix string) *workflowcore.Coordinator {
	var idSeq int
	return workflowcore.New(workflowcore.Deps{
		Store:            store,
		Spawner:          spawner,
		SessionFacts:     sessionFacts,
		WorkspaceFacts:   ws,
		ReviewRuns:       reviewRuns,
		ReviewerLauncher: &fakeReviewerLauncher{},
		MessageSender:    sender,
		Clock:            clk.Now,
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("%s%d", idPrefix, idSeq)
		},
	})
}

// fixDeliveryFixture bundles the stack these tests drive.
type fixDeliveryFixture struct {
	coord        *workflowcore.Coordinator
	store        *fakeStore
	clk          *fakeClock
	spawner      *fakeSpawner
	sessionFacts *fakeSessionFacts
	ws           *fakeWorkspaceFacts
	reviewRuns   *fakeReviewRuns
	sender       *refusingMessageSender
}

func newFixDeliveryFixture(sender *refusingMessageSender) *fixDeliveryFixture {
	store := newFakeStore()
	clk := &fakeClock{t: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)}
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	ws := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	return &fixDeliveryFixture{
		coord: newFixCoordinatorOver(store, clk, spawner, sessionFacts, ws, reviewRuns, sender, "id"),
		store: store, clk: clk, spawner: spawner, sessionFacts: sessionFacts,
		ws: ws, reviewRuns: reviewRuns, sender: sender,
	}
}

// restart builds a second coordinator over the same durable state.
func (f *fixDeliveryFixture) restart() *workflowcore.Coordinator {
	f.coord = newFixCoordinatorOver(f.store, f.clk, f.spawner, f.sessionFacts, f.ws, f.reviewRuns, f.sender, "rid")
	return f.coord
}

// driveToChangesRequestedWithFindings is driveToChangesRequested with a
// caller-supplied review body, so a test can make the findings — the dominant
// term in a fix prompt's size — as large as the real incident's.
func driveToChangesRequestedWithFindings(t *testing.T, f *fixDeliveryFixture, runID, findings string) workflowcore.RunDetail {
	t.Helper()
	ctx := context.Background()
	completeWorkStep(t, f.coord, f.store, f.clk, f.sessionFacts, f.ws, runID)
	got, err := f.coord.ContinueRun(ctx, runID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	reviewRunID := *reviewStepFrom(got).Step.ReviewRunID
	f.reviewRuns.setStatus(reviewRunID, domain.ReviewRunComplete, domain.VerdictChangesRequested)
	f.reviewRuns.runs[reviewRunID] = withBody(f.reviewRuns.runs[reviewRunID], findings)
	f.clk.Advance(time.Second)
	final, err := f.coord.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun after changes_requested: %v", err)
	}
	return final
}

// hugeFindings is a review body well past the transport's inline budget — the
// shape of a real verify failure pasted into findings.
func hugeFindings() string {
	return strings.Repeat("FAIL: TestSomething (0.02s)\n    board_test.go:118: expected 3, got 4\n", 800)
}

// A + prompt-size observability: a very large fix prompt is delivered in one
// piece, never truncated, and the dispatch checkpoint records what it took.
func TestLargeFixPromptIsDeliveredWholeAndRecorded(t *testing.T) {
	f := newFixDeliveryFixture(&refusingMessageSender{})
	sender, store := f.sender, f.store
	ctx := context.Background()

	created, err := f.coord.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	findings := hugeFindings()
	got := driveToChangesRequestedWithFindings(t, f, created.Run.ID, findings)

	if len(sender.delivered) != 1 {
		t.Fatalf("deliveries = %d, want exactly 1", len(sender.delivered))
	}
	if !strings.Contains(sender.delivered[0], findings) {
		t.Fatal("the reviewer's findings were not delivered verbatim — the prompt must never be silently truncated")
	}
	if n := strings.Count(sender.delivered[0], "FAIL: TestSomething"); n != strings.Count(findings, "FAIL: TestSomething") {
		t.Fatalf("findings occurrences = %d, want %d (no truncation, no duplication)", n, strings.Count(findings, "FAIL: TestSomething"))
	}

	fix := fixStepFrom(got)
	dispatched := checkpointsWithPhase(t, store, created.Run.ID, "fix_dispatched")
	if len(dispatched) != 1 {
		t.Fatalf("fix_dispatched checkpoints = %d, want 1", len(dispatched))
	}
	var rec struct {
		PromptBytes int    `json:"promptBytes"`
		Transport   string `json:"transport"`
		ContextPack bool   `json:"contextPack"`
		CycleNumber int    `json:"cycleNumber"`
	}
	if err := json.Unmarshal([]byte(dispatched[0].RetryState), &rec); err != nil {
		t.Fatalf("decode delivery record: %v", err)
	}
	if rec.PromptBytes != len(sender.delivered[0]) {
		t.Fatalf("recorded promptBytes = %d, want the delivered prompt's %d", rec.PromptBytes, len(sender.delivered[0]))
	}
	if rec.Transport != string(ports.PromptTransportBufferFile) {
		t.Fatalf("recorded transport = %q, want %q for a prompt this size", rec.Transport, ports.PromptTransportBufferFile)
	}
	if rec.CycleNumber != 1 {
		t.Fatalf("recorded cycle = %d, want 1", rec.CycleNumber)
	}
	if fix.Step.State != domain.WorkflowStepRunning {
		t.Fatalf("fix step state = %q, want running", fix.Step.State)
	}
}

// E: a refusal that delivered nothing is retried automatically, exactly once
// per refusal, with byte-identical instructions — and the run never becomes a
// human decision on the way.
func TestRefusedFixPromptIsRetriedIdempotently(t *testing.T) {
	f := newFixDeliveryFixture(&refusingMessageSender{refuseCount: 1})
	sender, store, clk := f.sender, f.store, f.clk
	c := f.coord
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	findings := hugeFindings()
	got := driveToChangesRequestedWithFindings(t, f, created.Run.ID, findings)

	// The refusal itself: nothing terminal, nothing for a human.
	fix := fixStepFrom(got)
	if fix.Step.State == domain.WorkflowStepFailed {
		t.Fatal("a refused prompt failed the fix step; a step that never reached the agent must stay re-dispatchable")
	}
	if got.Run.State == domain.WorkflowRunNeedsAttention {
		t.Fatal("a refused prompt parked the run in needs_attention; 'command too long' is not a decision anyone can make")
	}
	retries := checkpointsWithPhase(t, store, created.Run.ID, "prompt_transport_retry")
	if len(retries) != 1 {
		t.Fatalf("prompt_transport_retry checkpoints = %d, want 1", len(retries))
	}
	if !strings.Contains(retries[0].NextAction, "refused before delivery") {
		t.Fatalf("retry checkpoint next_action = %q, want it to name the refusal", retries[0].NextAction)
	}

	// The next poll re-sends it.
	clk.Advance(time.Second)
	after, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun after refusal: %v", err)
	}
	if sender.calls != 2 {
		t.Fatalf("Send calls = %d, want 2 (one refused, one retried)", sender.calls)
	}
	if len(sender.delivered) != 1 {
		t.Fatalf("deliveries = %d, want exactly 1 — a retry must not duplicate the instructions", len(sender.delivered))
	}
	if !strings.Contains(sender.delivered[0], findings) {
		t.Fatal("the retried prompt lost the findings")
	}
	attempts, err := store.ListWorkflowAttempts(ctx, fixStepFrom(after).Step.ID)
	if err != nil {
		t.Fatalf("ListWorkflowAttempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("fix attempts = %d, want exactly 1 across refusal + retry", len(attempts))
	}

	// And further polls neither re-send nor re-record.
	clk.Advance(time.Second)
	if _, err := c.GetRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("third GetRun: %v", err)
	}
	if sender.calls != 2 {
		t.Fatalf("Send calls after another poll = %d, want still 2", sender.calls)
	}
	if n := len(checkpointsWithPhase(t, store, created.Run.ID, "prompt_transport_retry")); n != 1 {
		t.Fatalf("prompt_transport_retry checkpoints = %d, want still 1", n)
	}
}

// The retry budget is bounded: a transport that never accepts the prompt ends
// in the ordinary, actionable dispatch_failed stop rather than retrying forever.
func TestRefusedFixPromptRetryIsBounded(t *testing.T) {
	f := newFixDeliveryFixture(&refusingMessageSender{refuseCount: 99})
	store, clk, c := f.store, f.clk, f.coord
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	driveToChangesRequestedWithFindings(t, f, created.Run.ID, hugeFindings())

	for i := 0; i < 8; i++ {
		clk.Advance(time.Second)
		if _, err := c.GetRun(ctx, created.Run.ID); err != nil {
			t.Fatalf("GetRun poll %d: %v", i, err)
		}
	}
	retries := checkpointsWithPhase(t, store, created.Run.ID, "prompt_transport_retry")
	if len(retries) > 3 {
		t.Fatalf("prompt_transport_retry checkpoints = %d, want at most the bounded 3", len(retries))
	}
	if len(checkpointsWithPhase(t, store, created.Run.ID, workflowcore.ReasonDispatchFailed)) == 0 {
		t.Fatal("an exhausted retry budget must end in an actionable dispatch_failed stop")
	}
	run, _, err := store.GetWorkflowRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention once the budget is exhausted", run.State)
	}
}

// Restart safety: the retry budget lives in the durable ledger, so a fresh
// Coordinator over the same store continues the same retry sequence instead of
// starting a second, concurrent delivery of the same cycle.
func TestRefusedFixPromptRetrySurvivesRestart(t *testing.T) {
	f := newFixDeliveryFixture(&refusingMessageSender{refuseCount: 1})
	sender, store, clk := f.sender, f.store, f.clk
	ctx := context.Background()

	created, err := f.coord.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	driveToChangesRequestedWithFindings(t, f, created.Run.ID, hugeFindings())

	// "Daemon restart": a brand-new Coordinator over the same store.
	restarted := f.restart()
	clk.Advance(time.Second)
	if _, err := restarted.GetRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("GetRun after restart: %v", err)
	}
	if len(sender.delivered) != 1 {
		t.Fatalf("deliveries after restart = %d, want exactly 1", len(sender.delivered))
	}
	if n := len(checkpointsWithPhase(t, store, created.Run.ID, "prompt_transport_retry")); n != 1 {
		t.Fatalf("prompt_transport_retry checkpoints = %d, want 1 (the pre-restart refusal, not a new one)", n)
	}
}

// Token discipline: the context pack prepended to a fix prompt no longer
// repeats what the fix prompt already carries verbatim. Nothing is dropped from
// the message as a whole — the excluded sections are the ones BuildFixPrompt
// itself renders in full.
func TestFixContextPackOmitsWhatTheFixPromptAlreadyCarries(t *testing.T) {
	pack := workflowcore.BuildSessionContextPack(domain.WorkflowRoleFixWorker, domain.TaskCheckpointSummary{
		Objective:            "ship the thing",
		Task:                 "task 2",
		AcceptanceCriteria:   []string{"tests pass"},
		LatestReviewFindings: "the reviewer's very long findings",
		ActiveErrors:         []string{"TestBoard failing"},
		FilesChanged:         []string{"frontend/src/Board.tsx"},
		NextAction:           "fix",
	})
	full := workflowcore.RenderContextPackForRole(pack)
	trimmed := workflowcore.RenderContextPackForRoleExcluding(pack, []workflowcore.ContextPackField{
		workflowcore.ContextPackObjective,
		workflowcore.ContextPackAcceptanceCriteria,
		workflowcore.ContextPackReviewFindings,
	})

	for _, dup := range []string{"ship the thing", "tests pass", "the reviewer's very long findings"} {
		if !strings.Contains(full, dup) {
			t.Fatalf("fixture problem: %q missing from the full render", dup)
		}
		if strings.Contains(trimmed, dup) {
			t.Fatalf("%q is still duplicated in the context-pack prefix", dup)
		}
	}
	// Everything the fix prompt does NOT carry must survive.
	for _, kept := range []string{"TestBoard failing", "frontend/src/Board.tsx", "task 2"} {
		if !strings.Contains(trimmed, kept) {
			t.Fatalf("%q was dropped from the context pack; de-duplication must never remove context", kept)
		}
	}
	if len(trimmed) >= len(full) {
		t.Fatal("the trimmed render is not smaller than the full one")
	}
}

// I: the incident's own lifecycle, end to end and headless — work -> review
// approved -> verify FAILS with a large output -> verify_fix_reentry -> a large
// fix prompt is delivered to the same worker -> verification runs again and the
// run completes. Before Checkpoint 8P-E.13C the fix dispatch died at
// "command too long" and the run ended in needs_attention with a terminal fix
// step; nothing about the workflow could recover it.
func TestVerifyFailureDeliversLargeFixPromptAndConverges(t *testing.T) {
	fx := newAutonomousFixture(t, oneTaskPlan())
	ctx := context.Background()
	seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())

	// The first verification fails with a real-sized failure dump; every later
	// one passes, exactly as a fix cycle that works would look.
	hugeOutput := strings.Repeat("--- FAIL: TestBoardRendersWorkflowRunID (0.01s)\n    board_test.go:118: want 3 rows, got 4\n", 700)
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
		if fx.sender.calls > 0 && strings.Contains(fx.sender.lastMsg, "--- FAIL: TestBoardRendersWorkflowRunID") {
			if !fixSent {
				fixSent = true
				// The fix worker actually fixes it: a genuinely new workspace
				// fingerprint, and a verification that now passes.
				fx.ws.obs.Changes = append(fx.ws.obs.Changes, ports.WorkspaceChange{Path: "fix.go", Status: " M"})
				fx.verifier.result = workflowcore.VerifyCommandExecution{ExitCode: 0}
			}
		}
	})

	if !fixSent {
		t.Fatal("the verify findings never reached the fix worker — the fix prompt was not delivered")
	}
	if len(fx.sender.lastMsg) <= ports.MaxInlinePromptBytes {
		t.Fatalf("fix prompt = %d bytes; the fixture did not reproduce an oversized prompt", len(fx.sender.lastMsg))
	}
	if !strings.Contains(fx.sender.lastMsg, hugeOutput[:200]) {
		t.Fatal("the delivered fix prompt does not carry the verification output")
	}
	run, _, err := fx.store.GetWorkflowRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if run.State == domain.WorkflowRunNeedsAttention {
		cps, _ := fx.store.ListWorkflowCheckpoints(ctx, created.Run.ID)
		var last string
		for _, cp := range cps {
			if cp.NextAction != "" {
				last = cp.DurablePhase + ": " + cp.NextAction
			}
		}
		t.Fatalf("run stopped for attention after a verify-driven fix: %s", last)
	}
	if fx.verifier.calls < 2 {
		t.Fatalf("verifier calls = %d, want at least 2 (the failure and the re-verification)", fx.verifier.calls)
	}
}
