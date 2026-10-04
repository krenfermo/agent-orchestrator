package workflow_test

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// p3b_projection_invariants_test.go — P3-B §26/§33.
//
// One table of the real states AO reaches, run through the ONE projection every
// surface renders, checked against the invariants that must hold in all of
// them. It is deliberately broad rather than deep: each individual behaviour
// has its own assertion in presentation_test.go, and what this file exists to
// catch is the cross-cutting contradiction — "completed and needs you",
// "repairing and Repair enabled", "direct branch and integration pending" —
// that appears when one state's handling is changed without the others in view.

func p3bAt(offset time.Duration) time.Time {
	return time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC).Add(offset)
}

// p3bDetail is a single-task run with the step states named, created an hour
// ago so every timeline entry has a real timestamp.
func p3bDetail(state domain.WorkflowRunState, work, review, fix, verify domain.WorkflowStepState) workflowcore.RunDetail {
	return workflowcore.RunDetail{
		Run:   domain.WorkflowRun{ID: "wf-1", ProjectID: "proj", State: state, CreatedAt: p3bAt(-time.Hour), UpdatedAt: p3bAt(-time.Minute)},
		Steps: singleTaskSteps(work, review, fix, verify),
	}
}

func p3bStopped(reason string) workflowcore.RunDetail {
	d := p3bDetail(domain.WorkflowRunNeedsAttention,
		domain.WorkflowStepCompleted, domain.WorkflowStepWaiting,
		domain.WorkflowStepPending, domain.WorkflowStepPending)
	d.StopAuthorityPhase = reason
	d.StopAuthorityAt = p3bAt(-time.Minute)
	d.LatestCheckpointPhase = reason
	d.LatestCheckpointAt = p3bAt(-time.Minute)
	d.CheckpointsFolded = true
	return d
}

// The §33 matrix. Every row is a state AO genuinely reaches, and every row's
// projection has to hold every invariant in CheckPresentationInvariants.
func TestEveryProjectedStateHoldsTheInvariants(t *testing.T) {
	completed := p3bDetail(domain.WorkflowRunCompleted,
		domain.WorkflowStepCompleted, domain.WorkflowStepCompleted,
		domain.WorkflowStepCompleted, domain.WorkflowStepCompleted)
	completed.Run.CompletedAt = ptrTime(p3bAt(-time.Minute))

	cancelled := p3bDetail(domain.WorkflowRunCancelled,
		domain.WorkflowStepCompleted, domain.WorkflowStepPending,
		domain.WorkflowStepPending, domain.WorkflowStepPending)
	cancelled.Run.CancelledAt = ptrTime(p3bAt(-time.Minute))

	repairing := p3bStopped(workflowcore.ReasonFixBudgetExhausted)
	repairing.Repair = workflowcore.RepairLifecycle{Active: true, Attempt: 2, Budget: 3, RunID: "wf-repair-2"}

	repairFailed := p3bStopped(workflowcore.ReasonFixBudgetExhausted)
	repairFailed.Repair = workflowcore.RepairLifecycle{Attempt: 3, Budget: 3, Exhausted: true, RunID: "wf-repair-3"}

	branchWait := p3bDetail(domain.WorkflowRunWaiting,
		domain.WorkflowStepReady, domain.WorkflowStepPending,
		domain.WorkflowStepPending, domain.WorkflowStepPending)
	branchWait.BranchWait = &workflowcore.BranchWait{Branch: "feat/x", RepoPath: "/repo", HeldByWorkflowRunID: "wf-2", AutoResume: true}

	capacityWait := p3bDetail(domain.WorkflowRunWaiting,
		domain.WorkflowStepReady, domain.WorkflowStepPending,
		domain.WorkflowStepPending, domain.WorkflowStepPending)
	capacityWait.WaitReason = "provider_capacity"
	capacityWait.CapacityWait = &workflowcore.CapacityWait{Role: domain.WorkflowRoleWorker, Reason: workflowcore.CapacityWaitProviderCooldown}

	cases := []struct {
		name       string
		detail     workflowcore.RunDetail
		placements []workflowcore.PlacementView
		wantStage  workflowcore.Stage
		wantHuman  bool
	}{
		{"preparing", p3bDetail(domain.WorkflowRunPending,
			domain.WorkflowStepPending, domain.WorkflowStepPending,
			domain.WorkflowStepPending, domain.WorkflowStepPending),
			directBranchPlacement(), workflowcore.StagePreparing, false},
		{"working", p3bDetail(domain.WorkflowRunRunning,
			domain.WorkflowStepRunning, domain.WorkflowStepPending,
			domain.WorkflowStepPending, domain.WorkflowStepPending),
			directBranchPlacement(), workflowcore.StageWorking, false},
		{"reviewing", p3bDetail(domain.WorkflowRunRunning,
			domain.WorkflowStepCompleted, domain.WorkflowStepRunning,
			domain.WorkflowStepPending, domain.WorkflowStepPending),
			directBranchPlacement(), workflowcore.StageReviewing, false},
		{"correcting", p3bDetail(domain.WorkflowRunRunning,
			domain.WorkflowStepCompleted, domain.WorkflowStepCompleted,
			domain.WorkflowStepRunning, domain.WorkflowStepPending),
			directBranchPlacement(), workflowcore.StageCorrecting, false},
		{"verifying", p3bDetail(domain.WorkflowRunRunning,
			domain.WorkflowStepCompleted, domain.WorkflowStepCompleted,
			domain.WorkflowStepCompleted, domain.WorkflowStepRunning),
			directBranchPlacement(), workflowcore.StageVerifying, false},
		{"completed direct branch", completed, directBranchPlacement(), workflowcore.StageCompleted, false},
		{"completed isolated pending integration", completed,
			isolatedPlacement(domain.PlacementReady, ""), workflowcore.StageCompleted, false},
		{"completed isolated integrated", completed,
			isolatedPlacement(domain.PlacementIntegrated, "abc123"), workflowcore.StageCompleted, false},
		{"cancelled", cancelled, directBranchPlacement(), workflowcore.StageCancelled, false},
		{"failed", p3bDetail(domain.WorkflowRunFailed,
			domain.WorkflowStepFailed, domain.WorkflowStepPending,
			domain.WorkflowStepPending, domain.WorkflowStepPending),
			directBranchPlacement(), workflowcore.StageFailed, false},
		{"waiting on capacity", capacityWait, directBranchPlacement(), workflowcore.StageWaiting, false},
		{"waiting on a branch", branchWait, directBranchPlacement(), workflowcore.StageWaiting, false},
		// The canonical auth family. There is deliberately no `auth_required`
		// reason in AO: a stop names WHICH credential failed, and the two that
		// exist both map onto the same "sign in" remedy.
		{"provider auth required", p3bStopped(workflowcore.ReasonProviderAuthRequired),
			directBranchPlacement(), workflowcore.StageNeedsAttention, true},
		{"reviewer auth invalid", p3bStopped(workflowcore.ReasonReviewerAuthInvalid),
			directBranchPlacement(), workflowcore.StageNeedsAttention, true},
		// The canonical stale-plan family is `planner_ambiguous`: the plan
		// cannot be trusted and the two answers are revalidate or regenerate.
		{"plan stale", p3bStopped(workflowcore.ReasonPlannerAmbiguous),
			directBranchPlacement(), workflowcore.StageNeedsAttention, true},
		{"dirty worktree", p3bStopped("dirty_worktree"), directBranchPlacement(), workflowcore.StageNeedsAttention, true},
		{"fix budget exhausted", p3bStopped(workflowcore.ReasonFixBudgetExhausted),
			directBranchPlacement(), workflowcore.StageNeedsAttention, true},
		{"reviewer launch failed", p3bStopped(workflowcore.ReasonReviewerLaunchFailed),
			directBranchPlacement(), workflowcore.StageNeedsAttention, true},
		{"fix produced no verifiable change", p3bStopped(workflowcore.ReasonFixNoVerifiableChange),
			directBranchPlacement(), workflowcore.StageNeedsAttention, true},
		{"automatic repair in flight", repairing, directBranchPlacement(), workflowcore.StageCorrecting, false},
		{"repair exhausted", repairFailed, directBranchPlacement(), workflowcore.StageNeedsAttention, true},
		{"integration conflict", completed, isolatedPlacement(domain.PlacementConflict, ""), workflowcore.StageCompleted, false},
		{"placement unknown", p3bDetail(domain.WorkflowRunRunning,
			domain.WorkflowStepRunning, domain.WorkflowStepPending,
			domain.WorkflowStepPending, domain.WorkflowStepPending),
			nil, workflowcore.StageWorking, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := presentationFor(tc.detail, tc.placements, nil)
			if violations := workflowcore.CheckPresentationInvariants(p); len(violations) > 0 {
				t.Fatalf("projection violates its own invariants: %v (stage=%q summary=%q)", violations, p.Stage, p.SummaryCode)
			}
			if p.Stage != tc.wantStage {
				t.Fatalf("stage = %q, want %q", p.Stage, tc.wantStage)
			}
			if p.RequiresHuman != tc.wantHuman {
				t.Fatalf("requiresHuman = %v, want %v (summary=%q)", p.RequiresHuman, tc.wantHuman, p.SummaryCode)
			}
			if p.LastMeaningfulActivityAt.IsZero() {
				t.Fatal("no meaningful activity timestamp: every run has at least been started")
			}
		})
	}
}

// §15: five distinguishable integration answers, and "not required" is one of
// them rather than the absence of one.
func TestIntegrationStateIsSpecificRatherThanGenericMergePending(t *testing.T) {
	completed := p3bDetail(domain.WorkflowRunCompleted,
		domain.WorkflowStepCompleted, domain.WorkflowStepCompleted,
		domain.WorkflowStepCompleted, domain.WorkflowStepCompleted)
	completed.Run.CompletedAt = ptrTime(p3bAt(-time.Minute))

	for _, tc := range []struct {
		name       string
		placements []workflowcore.PlacementView
		want       workflowcore.IntegrationState
	}{
		{"direct branch has nothing to integrate", directBranchPlacement(), workflowcore.IntegrationNotRequired},
		{"isolated and not yet moved", isolatedPlacement(domain.PlacementReady, ""), workflowcore.IntegrationPending},
		{"isolated mid-integration", isolatedPlacement(domain.PlacementIntegrating, ""), workflowcore.IntegrationInProgress},
		{"isolated integrated", isolatedPlacement(domain.PlacementIntegrated, "abc123"), workflowcore.IntegrationIntegrated},
		{"isolated conflict", isolatedPlacement(domain.PlacementConflict, ""), workflowcore.IntegrationFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := presentationFor(completed, tc.placements, nil)
			if p.Placement.Integration != tc.want {
				t.Fatalf("integration = %q, want %q", p.Placement.Integration, tc.want)
			}
		})
	}
}

// §11: bookkeeping must not make a stalled run look busy. The projection's
// meaningful activity is the newest entry of the BOUNDED timeline, so a
// checkpoint written by a reconcile pass a second ago cannot move it.
func TestMeaningfulActivityIgnoresBookkeeping(t *testing.T) {
	detail := p3bDetail(domain.WorkflowRunNeedsAttention,
		domain.WorkflowStepCompleted, domain.WorkflowStepWaiting,
		domain.WorkflowStepPending, domain.WorkflowStepPending)
	detail.Steps[1].Step.CompletedAt = ptrTime(p3bAt(-6 * time.Hour))
	// A poller touched the run one second ago. Neither of these is something a
	// person would call activity.
	detail.LatestCheckpointPhase = "heartbeat"
	detail.LatestCheckpointAt = p3bAt(-time.Second)
	detail.Run.UpdatedAt = p3bAt(-time.Second)

	p := presentationFor(detail, directBranchPlacement(), nil)
	if p.LastMeaningfulActivityAt.After(p3bAt(-time.Minute)) {
		t.Fatalf("last meaningful activity = %v; a heartbeat made a stalled run look active", p.LastMeaningfulActivityAt)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
