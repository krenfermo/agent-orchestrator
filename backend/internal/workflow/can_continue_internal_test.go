package workflow

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// canContinue is what the Reanudar control is rendered from, so the rule has to
// be exact in both directions: a recoverable stop must offer it, and a terminal
// or nonrecoverable one must not offer a button that provably does nothing.
func TestCanContinueRun(t *testing.T) {
	now := time.Date(2026, 8, 23, 18, 0, 0, 0, time.UTC)
	stopped := func(reason string) RunDetail {
		return RunDetail{
			Run:                   domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunNeedsAttention},
			LatestCheckpointPhase: reason,
			LatestCheckpointAt:    now,
		}
	}

	cases := []struct {
		name string
		in   RunDetail
		want bool
	}{
		{
			name: "recoverable needs_attention offers a resume",
			in:   stopped(ReasonWorkerBlocked),
			want: true,
		},
		{
			name: "a stop AO could not name at all is still recoverable — continuing it is exactly how AO finds out",
			in:   stopped("something_nobody_registered"),
			want: true,
		},
		{
			name: "an exhausted verification recovery is not: the remedy is a fresh run",
			in:   stopped(ReasonVerifyRecoveryExhausted),
			want: false,
		},
		{
			name: "an exhausted planner is not: the remedy is retrying planning",
			in:   stopped(ReasonPlannerExhausted),
			want: false,
		},
		{
			name: "a completed run is never continuable",
			in:   RunDetail{Run: domain.WorkflowRun{State: domain.WorkflowRunCompleted}},
			want: false,
		},
		{
			name: "a cancelled run is never continuable",
			in:   RunDetail{Run: domain.WorkflowRun{State: domain.WorkflowRunCancelled}},
			want: false,
		},
		{
			name: "a healthy run mid-flight is not: AO is already moving it",
			in: RunDetail{
				Run:   domain.WorkflowRun{State: domain.WorkflowRunRunning},
				Steps: []StepDetail{{Step: domain.WorkflowStep{Kind: domain.WorkflowStepWork, State: domain.WorkflowStepRunning}}},
			},
			want: false,
		},
		{
			name: "a completed work step with a review still to dispatch is the one live hand-off",
			in: RunDetail{
				Run: domain.WorkflowRun{State: domain.WorkflowRunWaiting},
				Steps: []StepDetail{
					{Step: domain.WorkflowStep{Kind: domain.WorkflowStepWork, State: domain.WorkflowStepCompleted}},
					{Step: domain.WorkflowStep{Kind: domain.WorkflowStepReview, State: domain.WorkflowStepReady}},
				},
			},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			life := DeriveLifecycle(LifecycleInput{Detail: tc.in})
			if life.CanContinue != tc.want {
				t.Fatalf("canContinue = %v, want %v (phase %q, reason %q)", life.CanContinue, tc.want, life.Phase, life.AttentionReason)
			}
		})
	}
}

// A master run stopped on child_needs_attention is reporting somebody else's
// problem, and both the link and the Continue must name that child.
func TestLifecycle_ChildStopNamesTheExactChild(t *testing.T) {
	detail := RunDetail{
		Run:                   domain.WorkflowRun{ID: "wf-master", State: domain.WorkflowRunNeedsAttention},
		LatestCheckpointPhase: ReasonChildNeedsAttention,
		Plan:                  &domain.WorkflowPlanRecord{Status: domain.WorkflowPlanApproved},
		Tasks: []domain.WorkflowTask{
			{ID: "t1", Ordinal: 1, State: domain.WorkflowTaskCompleted},
			{ID: "t2", Ordinal: 2, State: domain.WorkflowTaskRunning, ExecutionRunID: strPtr("wf-child")},
		},
	}
	life := DeriveLifecycle(LifecycleInput{Detail: detail})
	if life.AttentionWorkflowID != "wf-child" {
		t.Fatalf("attention workflow = %q, want the exact child run", life.AttentionWorkflowID)
	}
	if !life.CanContinue {
		t.Fatalf("a mirrored child stop must stay continuable — the remedy is continuing the child")
	}
}
