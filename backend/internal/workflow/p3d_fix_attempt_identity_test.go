package workflow_test

// P3-D §7/§10/§28 — the fix attempt row nobody could attribute or close.
//
// Deferral A, stated as a test. The fix lane identified a cycle by counting
// attempt rows, so:
//
//   - a row could not say which cycle it belonged to (nothing to read), and
//   - a cycle whose review was cancelled left its row open forever, with a NULL
//     outcome and a NULL error class, telling every guard downstream that a
//     writer might still be live in the tree.
//
// These tests assert the two halves of the replacement: the identity is durable
// and recoverable FROM THE ROW, and a cycle that is provably over closes.

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// driveToFixCycle produces the real state a dispatched fix cycle leaves: a
// review that requested changes, and the fix attempt minted for it.
func driveToFixCycle(t *testing.T) (
	*workflowcore.Coordinator, *fakeStore, *fakeReviewRuns, context.Context, string, domain.WorkflowStep,
) {
	t.Helper()
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{
		Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	c, store, clk := newCoordinatorWithFix(
		spawner, sessionFacts, workspaceFacts, reviewRuns, &fakeReviewerLauncher{}, &fakeMessageSender{})
	ctx := context.Background()
	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	got := driveToChangesRequested(t, c, store, clk, sessionFacts, workspaceFacts, reviewRuns, created.Run.ID)
	return c, store, reviewRuns, ctx, created.Run.ID, fixStepFrom(got).Step
}

func fixAttemptsOf(ctx context.Context, t *testing.T, store *fakeStore, stepID string) []domain.WorkflowAttempt {
	t.Helper()
	attempts, err := store.ListWorkflowAttempts(ctx, stepID)
	if err != nil {
		t.Fatalf("ListWorkflowAttempts: %v", err)
	}
	return attempts
}

// The identity is on the row, and it names the cycle that minted it. This is
// what a count could never do.
func TestAFixAttemptNamesItsOwnCycle(t *testing.T) {
	_, store, reviewRuns, ctx, _, fixStep := driveToFixCycle(t)

	attempts := fixAttemptsOf(ctx, t, store, fixStep.ID)
	if len(attempts) != 1 {
		t.Fatalf("fix attempts = %d, want exactly 1", len(attempts))
	}
	reviewRunID, cycle, ok := workflowcore.FixAttemptCycle(attempts[0])
	if !ok {
		t.Fatalf("attempt %+v carries no recoverable cycle identity", attempts[0])
	}
	if cycle != 1 {
		t.Fatalf("cycle = %d, want 1", cycle)
	}
	if _, found := reviewRuns.runs[reviewRunID]; !found {
		t.Fatalf("the attempt names review run %q, which does not exist", reviewRunID)
	}
	// And while that cycle holds authority the row reads as the active one.
	if got := workflowcore.ClassifyFixAttempt(attempts[0], workflowcore.FixAuthority{
		ReviewRunID: reviewRunID, CycleNumber: cycle, Known: true,
	}); got != workflowcore.FixAttemptActive {
		t.Fatalf("classification = %q, want active", got)
	}
}

// §28, exactly: the review cycle is cancelled after its fix attempt was minted.
// The attempt must be terminalized, and the reaper must see no active orphan.
func TestACancelledReviewCycleLeavesNoActiveFixAttempt(t *testing.T) {
	c, store, reviewRuns, ctx, runID, fixStep := driveToFixCycle(t)

	attempts := fixAttemptsOf(ctx, t, store, fixStep.ID)
	if len(attempts) != 1 || attempts[0].Outcome != "" {
		t.Fatalf("precondition: want exactly one open fix attempt, got %+v", attempts)
	}
	reviewRunID, cycle, ok := workflowcore.FixAttemptCycle(attempts[0])
	if !ok {
		t.Fatal("precondition: the attempt must name its cycle")
	}

	// The review that authorized this cycle is cancelled, and it never
	// recorded a verdict. Its findings are no longer an authority, so the fix
	// cycle they opened can never be finished by anyone.
	cancelled := reviewRuns.runs[reviewRunID]
	cancelled.Status = domain.ReviewRunCancelled
	cancelled.Verdict = domain.VerdictNone
	reviewRuns.runs[reviewRunID] = cancelled

	run, ok2, err := store.GetWorkflowRun(ctx, runID)
	if err != nil || !ok2 {
		t.Fatalf("GetWorkflowRun: %v (found=%v)", err, ok2)
	}
	closed, err := c.TerminalizeSupersededFixAttempts(ctx, run)
	if err != nil {
		t.Fatalf("TerminalizeSupersededFixAttempts: %v", err)
	}
	if closed != 1 {
		t.Fatalf("closed %d superseded fix attempts, want 1", closed)
	}

	after := fixAttemptsOf(ctx, t, store, fixStep.ID)
	if after[0].Outcome != domain.WorkflowAttemptCancelled {
		t.Fatalf("outcome = %q, want cancelled", after[0].Outcome)
	}
	if after[0].ErrorClass != domain.WorkflowErrorSuperseded {
		t.Fatalf("error class = %q, want superseded", after[0].ErrorClass)
	}
	if after[0].FinishedAt == nil {
		t.Fatal("the attempt is still open after being terminalized")
	}
	// No orphan active attempt remains, whatever authority is asked about.
	if got := workflowcore.ClassifyFixAttempt(after[0], workflowcore.FixAuthority{
		ReviewRunID: reviewRunID, CycleNumber: cycle, Known: true,
	}); got != workflowcore.FixAttemptConcluded {
		t.Fatalf("classification = %q, want concluded", got)
	}
	// And it is explained on the ledger, not merely closed.
	if _, found := latestCheckpointOfPhase(t, store, runID, "fix_attempt_superseded"); !found {
		t.Fatalf("the closure carries no durable record; phases = %v", ledgerPhases(t, store, runID))
	}

	// Exactly once: a second sweep closes nothing and writes nothing more.
	before := len(ledgerPhases(t, store, runID))
	again, err := c.TerminalizeSupersededFixAttempts(ctx, run)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if again != 0 {
		t.Fatalf("second sweep closed %d attempts, want 0", again)
	}
	if now := len(ledgerPhases(t, store, runID)); now != before {
		t.Fatalf("ledger rows %d -> %d on a repeated sweep", before, now)
	}
}

// The safety half. A review run AO cannot read supersedes nothing: an
// unresolvable authority must never close an attempt whose fix worker may still
// be writing.
func TestAnUnreadableReviewAuthorityClosesNothing(t *testing.T) {
	c, store, reviewRuns, ctx, runID, fixStep := driveToFixCycle(t)

	attempts := fixAttemptsOf(ctx, t, store, fixStep.ID)
	reviewRunID, _, _ := workflowcore.FixAttemptCycle(attempts[0])
	delete(reviewRuns.runs, reviewRunID)

	run, _, err := store.GetWorkflowRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	closed, err := c.TerminalizeSupersededFixAttempts(ctx, run)
	if err != nil {
		t.Fatalf("TerminalizeSupersededFixAttempts: %v", err)
	}
	if closed != 0 {
		t.Fatalf("closed %d attempts on an authority AO could not attribute, want 0", closed)
	}
	if after := fixAttemptsOf(ctx, t, store, fixStep.ID); after[0].Outcome != "" {
		t.Fatalf("outcome = %q, want the attempt left untouched", after[0].Outcome)
	}
}
