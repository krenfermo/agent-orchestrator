package workflow_test

// Checkpoint 8P-E.21 — closing an incident whose cause went away, and the
// derived progress the modal renders.
//
// The distinction under test throughout is between two claims AO can make:
// RESOLVED means it did something attributable and that something verified;
// CLOSED means the condition ended by some other route and AO takes no credit.
// Conflating them would let the Advisor claim every recovery that happened near
// it, which is how a system's own reports stop being worth reading.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// 1. A capacity incident whose provider comes back closes itself.
func TestCapacityIncidentClosesWhenTheRunRecovers(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonCapacityRetryExhausted, "every provider attempt reported no capacity")
	ctx := context.Background()
	inc, err := f.c.OpenIncident(ctx, f.runID)
	if err != nil {
		t.Fatalf("OpenIncident: %v", err)
	}

	// Capacity returns and the run resumes through AO's ordinary path.
	f.unparkRunExternally(t)

	got, err := f.c.LoadIncident(ctx, f.runID, inc.ID)
	if err != nil {
		t.Fatalf("LoadIncident: %v", err)
	}
	if got.State != workflowcore.IncidentClosed {
		t.Fatalf("state = %q, want closed once the run left needs_attention", got.State)
	}
	if got.ClosureCause != "run_no_longer_stopped" {
		t.Fatalf("closure cause = %q, want run_no_longer_stopped", got.ClosureCause)
	}
	if len(got.ClosureEvidence) == 0 {
		t.Fatal("closed with no evidence: AO must be able to say why it stopped asking")
	}
}

// 2. A dirty worktree a person cleans up themselves closes the same way, and
// explicitly does NOT resolve — AO did not clean it.
func TestDirtyWorktreeClosesWithoutClaimingCredit(t *testing.T) {
	f := newAdvisorFixture(t, "dirty_worktree", "the target repository has uncommitted changes")
	ctx := context.Background()
	inc, err := f.c.OpenIncident(ctx, f.runID)
	if err != nil {
		t.Fatalf("OpenIncident: %v", err)
	}
	f.unparkRunExternally(t)

	got, err := f.c.LoadIncident(ctx, f.runID, inc.ID)
	if err != nil {
		t.Fatalf("LoadIncident: %v", err)
	}
	if got.State == workflowcore.IncidentResolved {
		t.Fatal("AO claimed a resolution for a worktree a person cleaned up")
	}
	if got.State != workflowcore.IncidentClosed {
		t.Fatalf("state = %q, want closed", got.State)
	}
	if n := f.countCheckpointPhase("incident_resolved"); n != 0 {
		t.Fatalf("resolved rows = %d, want 0", n)
	}
}

// 3. A restart before the closure is noticed still produces exactly one
// incident_closed row, however many times it is read afterwards.
func TestClosureIsWrittenOnceAcrossRestartsAndPolls(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonCapacityRetryExhausted, "no capacity")
	ctx := context.Background()
	inc, err := f.c.OpenIncident(ctx, f.runID)
	if err != nil {
		t.Fatalf("OpenIncident: %v", err)
	}
	f.unparkRunExternally(t)

	for i := 0; i < 5; i++ {
		if i == 2 {
			f.c = f.newCoordinator() // a daemon restart before anyone noticed
		}
		if _, err := f.c.LoadIncident(ctx, f.runID, inc.ID); err != nil {
			t.Fatalf("LoadIncident %d: %v", i, err)
		}
	}
	if n := f.countCheckpointPhase("incident_closed"); n != 1 {
		t.Fatalf("incident_closed rows = %d, want exactly 1 across a restart and five reads", n)
	}
}

// A condition that has NOT gone away is never closed. This is the guard against
// closing on absent evidence, which is the failure mode that would make the
// whole mechanism untrustworthy.
func TestAnUnchangedConditionIsNeverClosed(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonFixCycleNotStarted, "the worker never started this cycle")
	ctx := context.Background()
	inc, err := f.c.OpenIncident(ctx, f.runID)
	if err != nil {
		t.Fatalf("OpenIncident: %v", err)
	}
	for i := 0; i < 4; i++ {
		f.clk.Advance(2 * time.Hour) // time alone must never close anything
		got, lerr := f.c.LoadIncident(ctx, f.runID, inc.ID)
		if lerr != nil {
			t.Fatalf("LoadIncident: %v", lerr)
		}
		if got.State == workflowcore.IncidentClosed {
			t.Fatal("an unchanged condition was closed; only a positive observation may close an incident")
		}
	}
}

// 4. A successful repair still ends RESOLVED, never CLOSED — even though the
// run it was about has by then left needs_attention, which is exactly the
// condition that closes every other incident.
func TestSuccessfulRepairResolvesRatherThanCloses(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonVerifyInfraFailed, "verification infrastructure failure")
	ctx := context.Background()
	inc := f.approvedRepairDiagnosis(t)
	if _, err := f.c.ExecuteIncidentAction(ctx, f.runID, inc.ID, "morakurt@icloud.com"); err != nil {
		t.Fatalf("ExecuteIncidentAction: %v", err)
	}
	loaded, _ := f.c.LoadIncident(ctx, f.runID, inc.ID)
	f.finishRepairRun(t, loaded.RepairRunID, domain.WorkflowRunCompleted)
	// The source run also recovers, which would close any unowned incident.
	f.unparkRunExternally(t)

	got, err := f.c.LoadIncident(ctx, f.runID, inc.ID)
	if err != nil {
		t.Fatalf("LoadIncident: %v", err)
	}
	if got.State != workflowcore.IncidentResolved {
		t.Fatalf("state = %q, want resolved: AO ran an attributable, verified repair", got.State)
	}
	if n := f.countCheckpointPhase("incident_closed"); n != 0 {
		t.Fatalf("incident_closed rows = %d, want 0 for a repaired incident", n)
	}
}

// 5. The modal's progress crosses the real states, each derived from durable
// state rather than from elapsed time.
func TestProgressCrossesTheRealStates(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonVerifyInfraFailed, "verification infrastructure failure")
	ctx := context.Background()

	inc, err := f.c.OpenIncident(ctx, f.runID)
	if err != nil {
		t.Fatalf("OpenIncident: %v", err)
	}
	if got := f.c.DeriveIncidentStatus(ctx, inc).Progress; got != workflowcore.IncidentProgressAnalyzing {
		t.Fatalf("progress = %q, want analyzing before any diagnosis", got)
	}

	if _, _, err := f.c.RequestIncidentDiagnosis(ctx, f.runID); err != nil {
		t.Fatalf("RequestIncidentDiagnosis: %v", err)
	}
	inc, _ = f.c.LoadIncident(ctx, f.runID, inc.ID)
	st := f.c.DeriveIncidentStatus(ctx, inc)
	if st.Progress != workflowcore.IncidentProgressDiagnosing {
		t.Fatalf("progress = %q, want diagnosing", st.Progress)
	}
	if st.DiagnosticHarness == "" {
		t.Fatal("the modal is not told which provider is investigating")
	}

	if _, err := f.c.SubmitIncidentDiagnosis(ctx, f.runID, workflowcore.IncidentDiagnosisSubmission{
		IncidentID: inc.ID, PackDigest: f.packDigest(t),
		Class: workflowcore.IncidentRepairAO, Summary: "AO resolves the module root wrongly",
		Action: &workflowcore.IncidentActionSpec{Kind: workflowcore.IncidentActionRepairAgent, Reason: "fix it"},
	}); err != nil {
		t.Fatalf("SubmitIncidentDiagnosis: %v", err)
	}
	inc, _ = f.c.LoadIncident(ctx, f.runID, inc.ID)
	if got := f.c.DeriveIncidentStatus(ctx, inc).Progress; got != workflowcore.IncidentProgressAwaitingApproval {
		t.Fatalf("progress = %q, want awaiting_approval", got)
	}

	if _, err := f.c.ExecuteIncidentAction(ctx, f.runID, inc.ID, "morakurt@icloud.com"); err != nil {
		t.Fatalf("ExecuteIncidentAction: %v", err)
	}
	inc, _ = f.c.LoadIncident(ctx, f.runID, inc.ID)
	st = f.c.DeriveIncidentStatus(ctx, inc)
	if st.Progress != workflowcore.IncidentProgressRepairing {
		t.Fatalf("progress = %q, want repairing once the repair run is working", st.Progress)
	}
	if st.RepairRunID == "" {
		t.Fatal("the modal cannot open the repair run: no run id")
	}

	f.driveRepairStepTo(t, st.RepairRunID, domain.WorkflowStepReview, domain.WorkflowStepRunning)
	inc, _ = f.c.LoadIncident(ctx, f.runID, inc.ID)
	if got := f.c.DeriveIncidentStatus(ctx, inc).Progress; got != workflowcore.IncidentProgressReviewing {
		t.Fatalf("progress = %q, want reviewing", got)
	}

	f.driveRepairStepTo(t, st.RepairRunID, domain.WorkflowStepVerify, domain.WorkflowStepRunning)
	inc, _ = f.c.LoadIncident(ctx, f.runID, inc.ID)
	if got := f.c.DeriveIncidentStatus(ctx, inc).Progress; got != workflowcore.IncidentProgressVerifying {
		t.Fatalf("progress = %q, want verifying", got)
	}

	f.finishRepairRun(t, st.RepairRunID, domain.WorkflowRunCompleted)
	inc, _ = f.c.LoadIncident(ctx, f.runID, inc.ID)
	if got := f.c.DeriveIncidentStatus(ctx, inc).Progress; got != workflowcore.IncidentProgressResolved {
		t.Fatalf("progress = %q, want resolved", got)
	}
}

// A capacity wait tells the operator why, in routing's own words, rather than
// leaving them to guess from a spinner.
func TestWaitingForCapacityExplainsItself(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonFixCycleNotStarted, "the worker never started this cycle")
	ctx := context.Background()

	st := f.c.DeriveIncidentStatus(ctx, workflowcore.Incident{
		RunID: f.runID, State: workflowcore.IncidentOpen,
		WaitingForCapacity: true,
		CapacityReasons:    []string{"provider_unavailable", "capacity_probe_indeterminate"},
	})
	if st.Progress != workflowcore.IncidentProgressWaitingCapacity {
		t.Fatalf("progress = %q, want waiting_capacity", st.Progress)
	}
	if len(st.CapacityReasons) != 2 {
		t.Fatalf("capacity reasons = %v, want routing's own codes surfaced", st.CapacityReasons)
	}
}

// 6. Repeated reads never duplicate a state row.
func TestRepeatedReadsDoNotDuplicateIncidentRows(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonFixCycleNotStarted, "the worker never started this cycle")
	ctx := context.Background()
	inc, err := f.c.OpenIncident(ctx, f.runID)
	if err != nil {
		t.Fatalf("OpenIncident: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, _, err := f.c.IncidentPackFor(ctx, f.runID); err != nil {
			t.Fatalf("IncidentPackFor: %v", err)
		}
		if _, err := f.c.LoadIncident(ctx, f.runID, inc.ID); err != nil {
			t.Fatalf("LoadIncident: %v", err)
		}
	}
	if n := f.countCheckpointPhase("incident_opened"); n != 1 {
		t.Fatalf("incident_opened rows after ten polls = %d, want 1", n)
	}
	if n := f.countCheckpointPhase("incident_closed"); n != 0 {
		t.Fatalf("incident_closed rows = %d, want 0 for an unchanged condition", n)
	}
}

// 7. The repair run is navigable and explicitly labelled as AO's own repair,
// so the Board can group it away from the project's ordinary work.
func TestRepairRunIsLabelledAndNavigable(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonVerifyInfraFailed, "verification infrastructure failure")
	ctx := context.Background()
	inc := f.approvedRepairDiagnosis(t)
	if _, err := f.c.ExecuteIncidentAction(ctx, f.runID, inc.ID, "morakurt@icloud.com"); err != nil {
		t.Fatalf("ExecuteIncidentAction: %v", err)
	}
	loaded, _ := f.c.LoadIncident(ctx, f.runID, inc.ID)

	origin, ok := f.c.RepairOriginFor(ctx, loaded.RepairRunID)
	if !ok {
		t.Fatal("the repair run carries no origin label; the Board cannot tell it from ordinary work")
	}
	if origin.Origin != "incident_repair" {
		t.Fatalf("origin = %q, want incident_repair", origin.Origin)
	}
	if origin.IncidentID != inc.ID {
		t.Fatalf("origin incident = %q, want %q", origin.IncidentID, inc.ID)
	}
	if origin.SourceRunID != f.runID {
		t.Fatalf("origin source run = %q, want %q", origin.SourceRunID, f.runID)
	}
	if origin.ApprovedBy == "" {
		t.Fatal("the origin does not record who approved the repair")
	}
}

// 8. A diagnostic agent is never a workflow run. It is auxiliary, read-only
// activity belonging to the incident, and turning it into a run purely to
// display it would put a fake row on the Board.
func TestDiagnosticAgentIsNeverAWorkflowRun(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonFixCycleNotStarted, "the worker never started this cycle")
	ctx := context.Background()

	before, err := f.store.ListWorkflowRuns(ctx, "")
	if err != nil {
		t.Fatalf("ListWorkflowRuns: %v", err)
	}
	if _, _, err := f.c.RequestIncidentDiagnosis(ctx, f.runID); err != nil {
		t.Fatalf("RequestIncidentDiagnosis: %v", err)
	}
	after, err := f.store.ListWorkflowRuns(ctx, "")
	if err != nil {
		t.Fatalf("ListWorkflowRuns: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("workflow runs went from %d to %d: a diagnostic agent became a run", len(before), len(after))
	}
	if f.agents.diagnostics != 1 {
		t.Fatalf("diagnostic launches = %d, want 1 (it runs, it just is not a run)", f.agents.diagnostics)
	}
}

// ---- fixture helpers --------------------------------------------------------

// unparkRunExternally models the condition ending by some route that is not the
// Incident Advisor: a person continuing the run, capacity returning, a child
// recovering. It is the ONLY thing that may close an incident.
func (f *advisorFixture) unparkRunExternally(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	run, ok, err := f.store.GetWorkflowRun(ctx, f.runID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun: %v (found=%v)", err, ok)
	}
	f.clk.Advance(time.Minute)
	if _, err := f.store.UpdateWorkflowRunState(ctx, f.runID, run.State, domain.WorkflowRunRunning, f.clk.Now()); err != nil {
		t.Fatalf("unpark run: %v", err)
	}
}

// driveRepairStepTo moves one of the repair run's steps, standing in for the
// work the run would really be doing.
func (f *advisorFixture) driveRepairStepTo(t *testing.T, repairRunID string, kind domain.WorkflowStepKind, state domain.WorkflowStepState) {
	t.Helper()
	ctx := context.Background()
	steps, err := f.store.ListWorkflowSteps(ctx, repairRunID)
	if err != nil {
		t.Fatalf("ListWorkflowSteps: %v", err)
	}
	f.clk.Advance(time.Second)
	for _, s := range steps {
		if s.Kind != kind {
			continue
		}
		// pending -> running is not a legal transition; go through ready, the
		// same path a real dispatch takes.
		cur := s.State
		for _, next := range []domain.WorkflowStepState{domain.WorkflowStepReady, state} {
			if cur == next {
				continue
			}
			moved, err := f.store.UpdateWorkflowStepState(ctx, s.ID, cur, next, f.clk.Now())
			if err != nil {
				t.Fatalf("move %s step to %s: %v", kind, next, err)
			}
			if !moved {
				t.Fatalf("%s step refused %s -> %s", kind, cur, next)
			}
			cur = next
		}
		return
	}
	t.Fatalf("repair run %s has no %s step", repairRunID, kind)
}

// packDigest returns the digest of the pack this incident's newest diagnosis
// request was built from.
func (f *advisorFixture) packDigest(t *testing.T) string {
	t.Helper()
	for _, cp := range f.store.checkpoints[f.runID] {
		if cp.DurablePhase != "incident_diagnosis_requested" {
			continue
		}
		if i := strings.Index(cp.RetryState, `"packDigest":"`); i >= 0 {
			rest := cp.RetryState[i+len(`"packDigest":"`):]
			if j := strings.Index(rest, `"`); j >= 0 {
				return rest[:j]
			}
		}
	}
	t.Fatal("no diagnosis request recorded a pack digest")
	return ""
}
