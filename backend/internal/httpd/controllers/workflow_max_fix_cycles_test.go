package controllers

import (
	"encoding/json"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// P8: the run page states "fix cycle N / max". The max must be the budget the
// coordinator actually enforces for THIS run, and a snapshot the response
// cannot read must project nothing rather than the package default.
func TestMaxFixCyclesForRunReadsTheFrozenBudget(t *testing.T) {
	policy := domain.DefaultWorkflowPolicy()
	policy.MaxFixCycles = 5
	snapshot, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	got := maxFixCyclesForRun(domain.WorkflowRun{PolicySnapshot: string(snapshot)})
	if got == nil || *got != 5 {
		t.Fatalf("maxFixCycles = %v, want 5 from the frozen snapshot", got)
	}
}

func TestMaxFixCyclesForRunProjectsNothingItCannotRead(t *testing.T) {
	zero := domain.DefaultWorkflowPolicy()
	zero.MaxFixCycles = 0
	zeroSnapshot, err := json.Marshal(zero)
	if err != nil {
		t.Fatal(err)
	}
	for name, snapshot := range map[string]string{
		"empty":        "",
		"empty object": "{}",
		"unparseable":  "{not json",
		"no budget":    string(zeroSnapshot),
	} {
		if got := maxFixCyclesForRun(domain.WorkflowRun{PolicySnapshot: snapshot}); got != nil {
			t.Fatalf("%s: maxFixCycles = %d, want absent rather than a guessed default", name, *got)
		}
	}
}
