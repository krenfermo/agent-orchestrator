package workflow_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// P9 §25 — the ownership readback explains recovery's decision without opening
// the database, and is itself a strict read.
func TestP9Readback_ReportsProofAndDecisionAndWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		proof     domain.WorkerRuntimeProof
		ownership domain.WorkerOwnershipStatus
		decision  workflowcore.WorkerRecoveryAction
		reason    workflowcore.WorkerRecoveryReason
	}{
		{domain.WorkerRuntimeOwned, domain.WorkerOwnershipProven, workflowcore.WorkerRecoveryAdopt, workflowcore.WorkerReasonMatchingRuntime},
		{domain.WorkerRuntimeOwnerMismatch, domain.WorkerOwnershipUnproven, workflowcore.WorkerRecoveryFailClosed, workflowcore.WorkerReasonOwnerMismatch},
		{domain.WorkerRuntimeProvenanceMissing, domain.WorkerOwnershipLegacyUnknown, workflowcore.WorkerRecoveryFailClosed, workflowcore.WorkerReasonLegacyProvenanceMissing},
		{domain.WorkerRuntimeAbsent, domain.WorkerOwnershipUnproven, workflowcore.WorkerRecoveryRelaunch, workflowcore.WorkerReasonRuntimeMissing},
	} {
		t.Run(string(tc.proof), func(t *testing.T) {
			f := newP9Fixture(t)
			f.spawner.crashAfterRuntime[1] = true
			f.startCrashing(f.coord)
			f.runtime.set(f.sid(1), tc.proof)
			// A secret in the session's own prompt must never reach the readback.
			rec, _, _ := f.store.GetSession(f.ctx, f.sid(1))
			rec.Metadata.Prompt = "deploy with sk-live-P9SECRET"
			if err := f.store.UpdateSession(f.ctx, rec); err != nil {
				t.Fatal(err)
			}
			f.boot()
			f.clk.Advance(time.Minute)

			before := len(f.ledger())
			stepBefore := f.workStep()
			var rows []workflowcore.WorkerOwnershipReadback
			for i := 0; i < 3; i++ {
				got, err := f.coord.WorkerOwnershipFor(f.ctx, f.runID)
				if err != nil {
					t.Fatalf("WorkerOwnershipFor: %v", err)
				}
				rows = got
			}
			if after := len(f.ledger()); after != before {
				t.Fatalf("the readback wrote %d checkpoints", after-before)
			}
			f.assertSpawns(1, 1)
			if got := f.workStep(); got.State != stepBefore.State || f.sessionOnStep() != "" {
				t.Fatalf("the readback moved the step: %q -> %q", stepBefore.State, got.State)
			}

			var work *workflowcore.WorkerOwnershipReadback
			for i := range rows {
				if rows[i].StepKind == domain.WorkflowStepWork {
					work = &rows[i]
				}
			}
			if work == nil {
				t.Fatalf("no work step row in %+v", rows)
			}
			if work.SessionID != string(f.sid(1)) || work.Proof != tc.proof || work.Ownership != tc.ownership {
				t.Fatalf("row = %+v, want session %s proof %s ownership %s", *work, f.sid(1), tc.proof, tc.ownership)
			}
			if work.Decision != tc.decision || work.Reason != tc.reason {
				t.Fatalf("decision = %s/%s, want %s/%s (%s)", work.Decision, work.Reason, tc.decision, tc.reason, work.Detail)
			}
			if work.LaunchState != domain.WorkflowOutboxDispatched || work.DispatchGeneration == "" || work.AttemptID == "" {
				t.Fatalf("launch identity missing from the readback: %+v", *work)
			}
			b, _ := json.Marshal(rows)
			if strings.Contains(string(b), "P9SECRET") || strings.Contains(string(b), "sk-live") {
				t.Fatalf("the readback leaked the session prompt: %s", b)
			}
		})
	}
}

func TestP9Readback_StepWithoutASessionIsNotApplicable(t *testing.T) {
	f := newP9Fixture(t)
	rows, err := f.coord.WorkerOwnershipFor(f.ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Ownership != domain.WorkerOwnershipNotApplicable || rows[0].Decision != "" {
		t.Fatalf("rows = %+v, want one not_applicable work row with no decision", rows)
	}
	if _, err := f.coord.WorkerOwnershipFor(f.ctx, "wf-does-not-exist"); err == nil {
		t.Fatal("a missing run returned no error")
	}
}
