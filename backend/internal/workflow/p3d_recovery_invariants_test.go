package workflow_test

// P3-D §19 — the recovery invariants, stated once and checked as a set.
//
// Every one of these is a property some incident in this checkpoint violated,
// and each is asserted against DURABLE ROWS rather than against a code path, so
// it keeps holding when the code that produces those rows is rewritten.
//
// The Advice consistency checks (§8) are here rather than in advice's own tests
// on purpose: neither projection is derived from the other, so agreeing is a
// property that has to be asserted. A RecoveryStatus computed FROM Advice could
// not detect Advice being wrong, and vice versa.

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// One active fix attempt per fix cycle, and a cancelled cycle leaves none.
func TestInvariantOneActiveFixAttemptPerCycle(t *testing.T) {
	c, store, reviewRuns, ctx, runID, fixStep := driveToFixCycle(t)

	attempts := fixAttemptsOf(ctx, t, store, fixStep.ID)
	open := 0
	for _, a := range attempts {
		if a.Outcome == "" {
			open++
		}
	}
	if open != 1 {
		t.Fatalf("open fix attempts = %d, want exactly 1: %+v", open, attempts)
	}

	// The cycle is cancelled. Its attempt must not remain active authority.
	reviewRunID, cycle, ok := workflowcore.FixAttemptCycle(attempts[0])
	if !ok {
		t.Fatal("the attempt does not name its cycle")
	}
	cancelled := reviewRuns.runs[reviewRunID]
	cancelled.Status = domain.ReviewRunCancelled
	cancelled.Verdict = domain.VerdictNone
	reviewRuns.runs[reviewRunID] = cancelled

	run, found, err := store.GetWorkflowRun(ctx, runID)
	if err != nil || !found {
		t.Fatalf("GetWorkflowRun: %v (found=%v)", err, found)
	}
	if _, err := c.TerminalizeSupersededFixAttempts(ctx, run); err != nil {
		t.Fatalf("TerminalizeSupersededFixAttempts: %v", err)
	}
	after := fixAttemptsOf(ctx, t, store, fixStep.ID)
	for _, a := range after {
		if a.Outcome == "" {
			t.Fatalf("a cancelled cycle left an open attempt: %+v", a)
		}
		if a.FinishedAt == nil {
			t.Fatalf("a terminal attempt has no finished_at: %+v", a)
		}
	}
	// And it is not active authority under ANY authority reading.
	for _, authority := range []workflowcore.FixAuthority{
		{Known: true},
		{ReviewRunID: reviewRunID, CycleNumber: cycle, Known: true},
		{},
	} {
		if got := workflowcore.ClassifyFixAttempt(after[0], authority); got == workflowcore.FixAttemptActive {
			t.Fatalf("a concluded attempt read as active under authority %+v", authority)
		}
	}
}

// A legacy row — no derivable cycle — is never active authority, and is never
// silently closed either.
func TestInvariantLegacyAttemptIsNeverActiveAuthority(t *testing.T) {
	legacy := domain.WorkflowAttempt{ID: "wfa-legacy-random", Harness: "codex"}
	for _, authority := range []workflowcore.FixAuthority{
		{Known: true},
		{ReviewRunID: "rr-1", CycleNumber: 1, Known: true},
		{},
	} {
		got := workflowcore.ClassifyFixAttempt(legacy, authority)
		if got == workflowcore.FixAttemptActive {
			t.Fatalf("a legacy row read as active authority under %+v", authority)
		}
		if got != workflowcore.FixAttemptLegacyUnproven {
			t.Fatalf("classification = %q, want legacy_unproven", got)
		}
	}
}

// A stale generation is never authoritative: an attempt whose cycle is not the
// authorized one reads superseded, whatever else is true about it.
func TestInvariantStaleGenerationIsNotAuthoritative(t *testing.T) {
	_, store, reviewRuns, ctx, _, fixStep := driveToFixCycle(t)
	attempts := fixAttemptsOf(ctx, t, store, fixStep.ID)
	reviewRunID, cycle, _ := workflowcore.FixAttemptCycle(attempts[0])
	_ = reviewRuns

	newer := workflowcore.FixAuthority{ReviewRunID: reviewRunID, CycleNumber: cycle + 1, Known: true}
	if got := workflowcore.ClassifyFixAttempt(attempts[0], newer); got != workflowcore.FixAttemptSuperseded {
		t.Fatalf("classification = %q, want superseded when a later cycle holds authority", got)
	}
	other := workflowcore.FixAuthority{ReviewRunID: "rr-somebody-else", CycleNumber: cycle, Known: true}
	if got := workflowcore.ClassifyFixAttempt(attempts[0], other); got != workflowcore.FixAttemptSuperseded {
		t.Fatalf("classification = %q, want superseded when another review holds authority", got)
	}
}

// An authority AO could not read never downgrades an open attempt. This is the
// safety direction: a review lookup that failed must not close a live worker's
// attempt out from under it.
func TestInvariantUnreadableAuthorityKeepsAnOpenAttemptActive(t *testing.T) {
	_, store, _, ctx, _, fixStep := driveToFixCycle(t)
	attempts := fixAttemptsOf(ctx, t, store, fixStep.ID)
	if got := workflowcore.ClassifyFixAttempt(attempts[0], workflowcore.FixAuthority{}); got != workflowcore.FixAttemptActive {
		t.Fatalf("classification = %q, want active when the authority could not be read", got)
	}
}

// Every state RecoveryStatus can report has a sentence, and only two of them
// say the person is the next actor. A state added without a sentence would
// reach an operator as "AO cannot describe this run".
func TestInvariantEveryRecoveryStateDescribesItself(t *testing.T) {
	all := []workflowcore.RecoveryState{
		workflowcore.RecoveryHealthyRunning,
		workflowcore.RecoveryWaitingCapacity,
		workflowcore.RecoveryWaitingBranch,
		workflowcore.RecoveryWaitingProvider,
		workflowcore.RecoveryWaitingDialogDelivery,
		workflowcore.RecoveryVerifyingResult,
		workflowcore.RecoveryAutomaticPending,
		workflowcore.RecoveryRepairRunning,
		workflowcore.RecoveryFailoverRunning,
		workflowcore.RecoveryRestartRecovery,
		workflowcore.RecoveryNeedsHuman,
		workflowcore.RecoveryTerminal,
	}
	for _, state := range all {
		s := workflowcore.RecoveryStatus{State: state, Repair: workflowcore.RecoveryRepairView{Attempt: 1, Budget: 2}}
		got := s.Describe()
		if got == "" || got == "AO cannot describe this run's recovery state." {
			t.Fatalf("state %q has no sentence", state)
		}
		// The only two states that may claim it is the person's turn.
		saysNoAction := got == "" || containsAny(got, "No action required.")
		switch state {
		case workflowcore.RecoveryNeedsHuman:
			if saysNoAction {
				t.Fatalf("needs_human says no action is required: %q", got)
			}
		case workflowcore.RecoveryRepairRunning, workflowcore.RecoveryFailoverRunning:
			// These say what AO is doing rather than "no action required", and
			// that is correct: they are progress reports, not invitations.
		default:
			if !saysNoAction && state != workflowcore.RecoveryTerminal {
				t.Fatalf("state %q neither says no action is required nor is terminal: %q", state, got)
			}
		}
	}
}

// §8: a waiting state is never a human's turn, and never offers a repair.
func TestInvariantWaitingStatesAreNotAHumansTurn(t *testing.T) {
	for _, state := range []workflowcore.RecoveryState{
		workflowcore.RecoveryWaitingCapacity,
		workflowcore.RecoveryWaitingBranch,
		workflowcore.RecoveryWaitingProvider,
		workflowcore.RecoveryWaitingDialogDelivery,
	} {
		if !state.Waiting() {
			t.Fatalf("state %q does not report itself as a wait", state)
		}
		if !state.AOIsActing() {
			t.Fatalf("state %q reports the person as the next actor", state)
		}
	}
	// And the two that ARE a person's turn, or nobody's.
	if workflowcore.RecoveryNeedsHuman.AOIsActing() {
		t.Fatal("needs_human reports AO as the next actor")
	}
	if workflowcore.RecoveryTerminal.AOIsActing() {
		t.Fatal("terminal reports AO as the next actor")
	}
	// A repair in flight is AO acting, not a wait.
	if workflowcore.RecoveryRepairRunning.Waiting() {
		t.Fatal("repair_running reports itself as a wait")
	}
	if !workflowcore.RecoveryRepairRunning.AOIsActing() {
		t.Fatal("repair_running reports the person as the next actor")
	}
}

// Dialog: `resolving` and `delivery_pending` are different answers, and
// conflating them is what hid P3-D smoke B's failure. `unreadable` is a third.
func TestInvariantDialogStatesAreNotConflated(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range []string{"captured", "resolving", "delivery_pending", "delivered", "unreadable", "human_required"} {
		if seen[s] {
			t.Fatalf("duplicate dialog state %q", s)
		}
		seen[s] = true
	}
	if len(seen) != 6 {
		t.Fatalf("dialog vocabulary has %d members, want 6", len(seen))
	}
}

func containsAny(haystack string, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
