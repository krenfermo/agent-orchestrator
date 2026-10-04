package workflow_test

// P0-B regression, at the coordinator: a real work dispatch takes the outbox
// row under a CLAIM, and the token that claim carries is the id of the durable
// intent record naming the launch.
//
// The two halves have to be one identity or the fence is decorative: the token
// must be reconstructable from the dispatch history, and the history must be
// findable from the token.

import (
	"encoding/json"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestAWorkDispatchClaimsTheOutboxRowUnderItsOwnIntentRecordsID(t *testing.T) {
	spawn := &launchSpawner{}
	f := newLaunchFixture(t, spawn)
	f.start()

	// A confirmed launch ends with the row ACKNOWLEDGED, and an acknowledged
	// row carries no claim: the token described the claim, not the row.
	entries, err := f.store.ListWorkflowOutboxByRun(f.ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	var entry domain.WorkflowOutboxEntry
	for _, e := range entries {
		if e.CommandType == domain.WorkflowOutboxSpawnWorkerSession {
			entry = e
		}
	}
	if entry.ID == "" {
		t.Fatal("no worker dispatch was enqueued")
	}
	if entry.Status != domain.WorkflowOutboxAcknowledged {
		t.Fatalf("outbox = %q, want acknowledged after a confirmed launch", entry.Status)
	}
	if entry.DispatchGeneration != "" {
		t.Fatalf("dispatch generation = %q, want cleared by the acknowledge", entry.DispatchGeneration)
	}

	// The intent record exists, and the confirmation names the same attempt
	// generation the intent was written under.
	records, err := f.store.ListWorkflowDispatchCheckpointsByStep(f.ctx, f.workID)
	if err != nil {
		t.Fatal(err)
	}
	var intentID, confirmedGeneration string
	for _, rec := range records {
		switch rec.LaunchOutcome {
		case domain.LaunchOutcomeIntended:
			if intentID == "" {
				intentID = rec.ID
			}
		case domain.LaunchOutcomeDispatched:
			confirmedGeneration = dispatchEvidence(t, rec.EvidenceJSON)["attemptGeneration"]
		}
	}
	if intentID == "" {
		t.Fatalf("no intent record was written; records = %d", len(records))
	}
	if confirmedGeneration != intentID {
		t.Fatalf("confirmation generation = %q, want the intent record's own id %q — the token and the record it names must be one identity",
			confirmedGeneration, intentID)
	}
	// And the step really is running, over a launch that was proven.
	step := f.workStep()
	if step.State != domain.WorkflowStepRunning || step.SessionID == nil {
		t.Fatalf("work step = %q session=%v, want running with its session", step.State, step.SessionID)
	}
}

// dispatchEvidence decodes a dispatch record's evidence map.
func dispatchEvidence(t *testing.T, raw string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if raw == "" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode dispatch evidence %q: %v", raw, err)
	}
	return out
}
