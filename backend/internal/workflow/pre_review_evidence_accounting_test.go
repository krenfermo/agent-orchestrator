package workflow_test

import (
	"testing"

	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// pre_review_evidence_accounting_test.go — telling AO's two command executors
// apart in a fixture that only has one counter.
//
// P5-A phase 2 gave AO a second place that legitimately runs a task's planned
// commands: the pre-review evidence pass, at review cycle 1, before any
// reviewer is dispatched. It runs through the SAME VerifyRunner as verification
// — deliberately, because that runner is the only authorized seam to the host —
// so a fake runner's call counter now counts both.
//
// Several existing tests assert "verification executed no commands", and their
// counter was that shared total. The invariant those tests protect is
// unchanged and still holds; what stopped being true is that the shared total
// measured it. This helper restores the measurement by subtracting the
// executions the evidence pass durably recorded as its own.
//
// It is deliberately derived from the durable ledger rather than from a
// snapshot taken at the right moment in the test: a snapshot would have to be
// placed correctly in every test that needs it, and a misplaced one would hide
// exactly the regression these assertions exist to catch.

// verifyRunnerCalls returns how many of totalRunnerCalls belong to VERIFICATION
// rather than to the pre-review evidence pass, by subtracting every execution
// the run's own pre_review_evidence checkpoints account for.
func verifyRunnerCalls(t *testing.T, store *fakeStore, runID string, totalRunnerCalls int) int {
	t.Helper()
	spent := 0
	for _, cp := range checkpointsWithPhase(t, store, runID, "pre_review_evidence") {
		record, ok := workflowcore.DecodePreReviewEvidenceForTest(cp.RetryState)
		if !ok {
			continue
		}
		spent += record.ExecutedCommandCount
	}
	if spent > totalRunnerCalls {
		t.Fatalf("pre-review evidence claims %d executions but the runner only ran %d — the accounting is wrong, not the run",
			spent, totalRunnerCalls)
	}
	return totalRunnerCalls - spent
}
