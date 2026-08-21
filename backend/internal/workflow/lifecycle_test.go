package workflow_test

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
)

// step builds one StepDetail at a given state, so each test below reads as the
// durable rows it is actually about.
func step(kind domain.WorkflowStepKind, state domain.WorkflowStepState) workflowcore.StepDetail {
	return workflowcore.StepDetail{Step: domain.WorkflowStep{Kind: kind, State: state}}
}

func singleTaskSteps(work, review, fix, verify domain.WorkflowStepState) []workflowcore.StepDetail {
	return []workflowcore.StepDetail{
		step(domain.WorkflowStepPlan, domain.WorkflowStepCompleted),
		step(domain.WorkflowStepWork, work),
		step(domain.WorkflowStepReview, review),
		step(domain.WorkflowStepFix, fix),
		step(domain.WorkflowStepVerify, verify),
		step(domain.WorkflowStepAdvance, domain.WorkflowStepPending),
	}
}

// The headline regression: the worker session goes idle the moment it finishes,
// and stays idle for the whole review. The Board must read the workflow, not
// the session, and say Reviewing.
func TestReviewInFlightProjectsAsReviewingNotInactive(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run: domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunRunning},
		Steps: singleTaskSteps(
			domain.WorkflowStepCompleted,
			domain.WorkflowStepRunning,
			domain.WorkflowStepPending,
			domain.WorkflowStepPending,
		),
	}
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail})
	if life.Phase != workflowcore.PhaseReviewing {
		t.Fatalf("phase = %q, want reviewing", life.Phase)
	}
	if life.Attention != workflowcore.AttentionNone {
		t.Fatalf("attention = %q, want none: a review in flight is progress, not a problem", life.Attention)
	}
}

// A changes_requested verdict rests the review step at waiting and the run at
// waiting. AO dispatches the fix itself, so this must never read as a request
// for a human decision.
func TestChangesRequestedIsInternalAttentionNotHumanDecision(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run: domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunWaiting},
		Steps: singleTaskSteps(
			domain.WorkflowStepCompleted,
			domain.WorkflowStepWaiting,
			domain.WorkflowStepRunning,
			domain.WorkflowStepPending,
		),
		LatestCheckpointPhase: "review_observed",
	}
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail})
	if life.Phase != workflowcore.PhaseFixing {
		t.Fatalf("phase = %q, want fixing", life.Phase)
	}
	if life.Attention == workflowcore.AttentionHuman {
		t.Fatalf("attention = human_decision, want ao_internal: AO applies this fix itself")
	}
	if life.Attention != workflowcore.AttentionInternal {
		t.Fatalf("attention = %q, want ao_internal", life.Attention)
	}
}

// A capacity wait with a scheduled retry is AO's own problem: it has a wake row
// and will resume on its own.
func TestCapacityWaitIsInternalAttention(t *testing.T) {
	at := time.Date(2026, 8, 20, 14, 2, 0, 0, time.UTC)
	detail := workflowcore.RunDetail{
		Run:        domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunWaiting},
		Steps:      singleTaskSteps(domain.WorkflowStepReady, domain.WorkflowStepPending, domain.WorkflowStepPending, domain.WorkflowStepPending),
		WaitReason: string(wake.ReasonWorkerCapacity),
		NextWakeAt: &at,
	}
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail})
	if life.Phase != workflowcore.PhaseWaitingForCapacity {
		t.Fatalf("phase = %q, want waiting_for_capacity", life.Phase)
	}
	if life.Attention != workflowcore.AttentionInternal {
		t.Fatalf("attention = %q, want ao_internal", life.Attention)
	}
}

// §8.8: a reviewer capacity stall parks the run with no wake row at all. Keyed
// on the wake alone this would read as a plain wait; it is a capacity wait with
// genuinely no retry time to show.
func TestReviewerCapacityStallWithoutWakeStillReadsAsCapacityWait(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run:                   domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunWaiting},
		Steps:                 singleTaskSteps(domain.WorkflowStepCompleted, domain.WorkflowStepWaiting, domain.WorkflowStepPending, domain.WorkflowStepPending),
		LatestCheckpointPhase: "review_capacity_retry",
	}
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail})
	if life.Phase != workflowcore.PhaseWaitingForCapacity {
		t.Fatalf("phase = %q, want waiting_for_capacity", life.Phase)
	}
	if life.NextWakeAt != nil {
		t.Fatalf("nextWakeAt = %v, want nil: there is genuinely no scheduled retry to show", life.NextWakeAt)
	}
}

// A branch queue is a wait on another workflow, never a provider problem.
func TestBranchWaitProjectsAsBlocked(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run:        domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunWaiting},
		Steps:      singleTaskSteps(domain.WorkflowStepReady, domain.WorkflowStepPending, domain.WorkflowStepPending, domain.WorkflowStepPending),
		WaitReason: string(wake.ReasonBranchLock),
	}
	if got := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail}).Phase; got != workflowcore.PhaseBlocked {
		t.Fatalf("phase = %q, want blocked", got)
	}
}

// The one carrier that genuinely means "Te necesita".
func TestHumanRequiredQuestionIsHumanDecision(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run:   domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunWaiting},
		Steps: singleTaskSteps(domain.WorkflowStepRunning, domain.WorkflowStepPending, domain.WorkflowStepPending, domain.WorkflowStepPending),
	}
	questions := []domain.WorkflowQuestion{{
		State:        domain.QuestionStateHumanRequired,
		QuestionText: "Should the restore overwrite the existing gallery?",
	}}
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail, Questions: questions})
	if life.Attention != workflowcore.AttentionHuman {
		t.Fatalf("attention = %q, want human_decision", life.Attention)
	}
	if life.AttentionReason != "question_human_required" {
		t.Fatalf("reason = %q, want question_human_required", life.AttentionReason)
	}
	if life.AttentionAction == "" {
		t.Fatalf("attentionAction is empty; the user needs to see what is being asked")
	}
}

// A dirty worktree is the textbook human decision: no amount of waiting makes
// someone's uncommitted changes go away, and AO knows exactly what to say.
func TestDirtyWorktreeIsHumanDecisionWithAnAction(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run:                   domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunNeedsAttention},
		Steps:                 singleTaskSteps(domain.WorkflowStepReady, domain.WorkflowStepPending, domain.WorkflowStepPending, domain.WorkflowStepPending),
		LatestCheckpointPhase: "dirty_worktree",
	}
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail})
	if life.Phase != workflowcore.PhaseNeedsAttention {
		t.Fatalf("phase = %q, want needs_attention", life.Phase)
	}
	if life.Attention != workflowcore.AttentionHuman {
		t.Fatalf("attention = %q, want human_decision", life.Attention)
	}
	if life.AttentionReason != "dirty_worktree" {
		t.Fatalf("reason = %q, want dirty_worktree", life.AttentionReason)
	}
	if life.AttentionAction == "" {
		t.Fatalf("attentionAction is empty; a dirty worktree has a known remedy")
	}
}

// needs_attention with nothing recorded stays truthful and unhelpful rather
// than acquiring a synthesized reason.
func TestNeedsAttentionWithNoRecordedReasonSynthesizesNothing(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run:   domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunNeedsAttention},
		Steps: singleTaskSteps(domain.WorkflowStepReady, domain.WorkflowStepPending, domain.WorkflowStepPending, domain.WorkflowStepPending),
	}
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail})
	if life.AttentionReason != "" || life.AttentionAction != "" {
		t.Fatalf("reason=%q action=%q, want both empty", life.AttentionReason, life.AttentionAction)
	}
}

// §8.4: a durable terminal state outranks a question that no longer blocks
// anything.
func TestCompletedRunWithLingeringQuestionRendersCompleted(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run:   domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunCompleted},
		Steps: singleTaskSteps(domain.WorkflowStepCompleted, domain.WorkflowStepCompleted, domain.WorkflowStepPending, domain.WorkflowStepCompleted),
	}
	questions := []domain.WorkflowQuestion{{State: domain.QuestionStateHumanRequired, QuestionText: "stale"}}
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail, Questions: questions})
	if life.Phase != workflowcore.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", life.Phase)
	}
	if life.Attention != workflowcore.AttentionNone {
		t.Fatalf("attention = %q, want none on a terminal run", life.Attention)
	}
}

// §8.2: the advance step never runs, so it must not keep a verified run looking
// unfinished.
func TestAdvanceStepIsExcludedFromTheChecklist(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run:   domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunCompleted},
		Steps: singleTaskSteps(domain.WorkflowStepCompleted, domain.WorkflowStepCompleted, domain.WorkflowStepPending, domain.WorkflowStepCompleted),
	}
	progress := workflowcore.DeriveStepProgress(detail)
	if len(progress) != 5 {
		t.Fatalf("checklist length = %d, want 5 (advance excluded)", len(progress))
	}
	for _, p := range progress {
		if p.Kind == domain.WorkflowStepAdvance {
			t.Fatalf("advance step leaked into the checklist")
		}
	}
}

// "Task 2 of 7 — Backend backup API" has to come from facts.
func TestTaskProgressNamesTheRunningTask(t *testing.T) {
	runID := "wf-child-2"
	tasks := []domain.WorkflowTask{
		{Ordinal: 1, Title: "Lifecycle mapping", State: domain.WorkflowTaskCompleted},
		{Ordinal: 2, Title: "Backend projection", State: domain.WorkflowTaskRunning, ExecutionRunID: &runID},
		{Ordinal: 3, Title: "Backend tests", State: domain.WorkflowTaskBlocked},
		{Ordinal: 4, Title: "Frontend Board", State: domain.WorkflowTaskBlocked},
	}
	p := workflowcore.DeriveTaskProgress(tasks)
	if p.Total != 4 || p.Completed != 1 || p.Running != 1 || p.Blocked != 2 {
		t.Fatalf("progress = %+v, want total 4 / completed 1 / running 1 / blocked 2", p)
	}
	if p.CurrentNumber != 2 || p.CurrentTitle != "Backend projection" || p.CurrentRunID != runID {
		t.Fatalf("current task = %d/%q/%q, want 2/Backend projection/%s", p.CurrentNumber, p.CurrentTitle, p.CurrentRunID, runID)
	}
}

// A master run whose only remaining tasks are dependency-blocked is blocked,
// never "waiting for capacity" — the blocker is another task, not a provider.
func TestMasterRunWithOnlyBlockedTasksProjectsAsBlocked(t *testing.T) {
	plan := domain.WorkflowPlanRecord{Status: domain.WorkflowPlanApproved}
	detail := workflowcore.RunDetail{
		Run:  domain.WorkflowRun{ID: "wf-master", State: domain.WorkflowRunRunning},
		Plan: &plan,
		Tasks: []domain.WorkflowTask{
			{Ordinal: 1, State: domain.WorkflowTaskCompleted},
			{Ordinal: 2, State: domain.WorkflowTaskBlocked},
		},
	}
	if got := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail}).Phase; got != workflowcore.PhaseBlocked {
		t.Fatalf("phase = %q, want blocked", got)
	}
}

// A master run still generating its plan is planning, not inactive.
func TestMasterRunGeneratingPlanProjectsAsPlanning(t *testing.T) {
	plan := domain.WorkflowPlanRecord{Status: domain.WorkflowPlanRunning}
	detail := workflowcore.RunDetail{
		Run:  domain.WorkflowRun{ID: "wf-master", State: domain.WorkflowRunPending},
		Plan: &plan,
	}
	if got := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail}).Phase; got != workflowcore.PhasePlanning {
		t.Fatalf("phase = %q, want planning", got)
	}
}

// LastActivityAt is the workflow's own newest durable timestamp.
func TestLastActivityUsesNewestDurableFactNotTheSession(t *testing.T) {
	runUpdated := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	checkpointAt := runUpdated.Add(35 * time.Second)
	detail := workflowcore.RunDetail{
		Run:                domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunRunning, UpdatedAt: runUpdated},
		Steps:              singleTaskSteps(domain.WorkflowStepCompleted, domain.WorkflowStepRunning, domain.WorkflowStepPending, domain.WorkflowStepPending),
		LatestCheckpointAt: checkpointAt,
	}
	if got := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail}).LastActivityAt; !got.Equal(checkpointAt) {
		t.Fatalf("lastActivityAt = %v, want %v", got, checkpointAt)
	}
}
