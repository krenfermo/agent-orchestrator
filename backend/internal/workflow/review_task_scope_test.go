package workflow_test

// Regression for the review-scope failure observed in real AO usage.
//
// A Master Workflow plan decomposed "Post-Run QA evidence" into five tasks.
// Task 1 was "Build the Post-Run QA evidence collector", and its acceptance
// criteria required only the collector package and its unit tests. Wiring that
// collector into the Task and Workflow lifecycles was explicitly assigned to
// tasks 4 and 5.
//
// The reviewer rejected task 1 anyway, every single time, because the collector
// was not wired into the lifecycle — work task 1 was never asked to do. The run
// went review -> fix -> review -> fix -> review -> fix ->
// fix_budget_exhausted -> needs_attention over a task that was correct.
//
// The reviewer could not have known better: it was handed run.Objective under
// the heading "objective of the overall run", the literal text "Acceptance
// criteria: (none recorded)", and no mention that a plan with later tasks
// existed at all.
//
// These tests pin the contract that fixes it: the reviewer of a child task is
// told this task's acceptance criteria, what earlier tasks already delivered,
// and which tasks own the work that is deliberately still missing — and is told
// that the last of those is never a reason to request changes.

import (
	"context"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// postRunQAPlan is the reported plan: a self-contained collector first, its
// lifecycle wiring last, and nothing in task 1 that mentions either lifecycle.
func postRunQAPlan() workflowcore.MasterPlan {
	verify := workflowcore.VerificationPlan{
		Commands: []workflowcore.VerificationCommandCheck{{
			Command: "go", Args: []string{"test", "./..."},
			TimeoutSeconds: 60, RequiredExitCode: 0, RetrySafe: true,
		}},
		Files: []workflowcore.VerificationFileCheck{},
	}
	step := func(id, title, desc string, deps, criteria []string) workflowcore.PlannedStep {
		return workflowcore.PlannedStep{
			ID: id, Title: title, Description: desc,
			Dependencies: deps, AcceptanceCriteria: criteria, Verify: verify,
		}
	}
	return workflowcore.MasterPlan{
		Version:   "v1",
		Objective: "Post-Run QA evidence",
		Summary:   "collector, storage, reporting, then lifecycle wiring",
		Steps: []workflowcore.PlannedStep{
			step("collector", "Build the Post-Run QA evidence collector",
				"Add the evidence collector package.", []string{},
				[]string{
					"A collector package exists that gathers post-run QA evidence.",
					"The collector has unit tests covering its public surface.",
				}),
			step("storage", "Persist collected evidence",
				"Store evidence rows durably.", []string{"collector"},
				[]string{"Evidence is persisted and readable back."}),
			step("report", "Render the evidence report",
				"Turn stored evidence into a report.", []string{"storage"},
				[]string{"A report renders from stored evidence."}),
			step("task-wiring", "Wire the evidence collector into the Task lifecycle",
				"Call the collector when a Task finishes.", []string{"report"},
				[]string{"A finished Task produces evidence via the collector."}),
			step("workflow-wiring", "Wire the evidence collector into the Workflow lifecycle",
				"Call the collector when a Workflow finishes.", []string{"task-wiring"},
				[]string{"A finished Workflow produces evidence via the collector."}),
		},
	}
}

// TestChildTaskReviewIsScopedToItsOwnTaskNotTheWholePlan drives the real
// autonomous stack over the reported plan and inspects what the reviewer of
// task 1 is actually handed.
func TestChildTaskReviewIsScopedToItsOwnTaskNotTheWholePlan(t *testing.T) {
	fx := newAutonomousFixture(t, postRunQAPlan())
	ctx := context.Background()
	seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())

	created, err := fx.coord.CreateObjectiveRun(ctx, "p", "Post-Run QA evidence", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")

	firstReviewPrompt := ""
	driveCycles(t, fx, 12, func(int) {
		if firstReviewPrompt == "" && fx.launcher.launchCalls == 1 {
			firstReviewPrompt = fx.launcher.lastPrompt
		}
	})
	if firstReviewPrompt == "" {
		firstReviewPrompt = fx.launcher.lastPrompt
	}
	if firstReviewPrompt == "" {
		t.Fatal("no reviewer was ever launched; the fixture did not reach the state under test")
	}

	// 1. The task's own acceptance criteria reach the reviewer. This alone is
	//    what "(none recorded)" used to replace.
	for _, want := range []string{
		"A collector package exists that gathers post-run QA evidence.",
		"The collector has unit tests covering its public surface.",
	} {
		if !strings.Contains(firstReviewPrompt, want) {
			t.Fatalf("review prompt is missing task 1's acceptance criterion %q\n---\n%s", want, firstReviewPrompt)
		}
	}
	if strings.Contains(firstReviewPrompt, "(none recorded)") {
		t.Fatalf("review prompt still tells the reviewer no acceptance criteria exist\n---\n%s", firstReviewPrompt)
	}

	// 2. The tasks that own the lifecycle wiring are named as future scope, so
	//    their absence reads as "not yet", not as a defect in task 1.
	for _, want := range []string{
		"Wire the evidence collector into the Task lifecycle",
		"Wire the evidence collector into the Workflow lifecycle",
	} {
		if !strings.Contains(firstReviewPrompt, want) {
			t.Fatalf("review prompt never names the later task %q that owns the wiring\n---\n%s", want, firstReviewPrompt)
		}
	}
	if !strings.Contains(firstReviewPrompt, "its absence\nis never a defect in this task") {
		t.Fatalf("review prompt does not say that missing future-task work is not a defect here\n---\n%s", firstReviewPrompt)
	}
	if !strings.Contains(firstReviewPrompt, "do NOT return\nchanges_requested for it") {
		t.Fatalf("review prompt does not forbid changes_requested for future-scope findings\n---\n%s", firstReviewPrompt)
	}

	// 3. Review quality is not weakened: the bar for a genuine rejection is
	//    still stated, in the same prompt.
	if !strings.Contains(firstReviewPrompt, "changes_requested is for this task's OWN acceptance criteria being unmet") {
		t.Fatalf("review prompt no longer states when changes_requested IS correct\n---\n%s", firstReviewPrompt)
	}
}

// TestTaskOnePassesWhileLifecycleWiringIsStillAssignedToLaterTasks is the
// end-to-end half: a reviewer that honors the scope contract approves task 1,
// and task 1 completes with the lifecycle wiring still unbuilt and still owned
// by tasks 4 and 5 — the outcome that previously ended in
// fix_budget_exhausted -> needs_attention.
func TestTaskOnePassesWhileLifecycleWiringIsStillAssignedToLaterTasks(t *testing.T) {
	fx := newAutonomousFixture(t, postRunQAPlan())
	ctx := context.Background()
	seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())

	created, err := fx.coord.CreateObjectiveRun(ctx, "p", "Post-Run QA evidence", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")

	var collectorTaskID string

	// Approve only the FIRST task's review, then stop feeding verdicts: the
	// claim under test is about task 1 alone, and leaving the rest unreviewed
	// keeps the later tasks provably unbuilt.
	driveCycles(t, fx, 14, func(int) {
		if collectorTaskID == "" {
			if task, found := taskByPlanStepID(t, fx, created.Run.ID, "collector"); found {
				collectorTaskID = task.ID
			}
		}
		taskID, childID, running := activeChildRunID(t, fx, created.Run.ID)
		if running && taskID == collectorTaskID {
			approveOpenReview(t, fx, childID, domain.VerdictApproved)
		}
	})

	task, found := taskByPlanStepID(t, fx, created.Run.ID, "collector")
	if !found {
		t.Fatal("the collector task was never created from the plan")
	}
	if task.State != domain.WorkflowTaskCompleted {
		t.Fatalf("task 1 state = %q, want completed: it satisfied its own acceptance criteria", task.State)
	}

	// The wiring tasks are still exactly where the plan put them.
	for _, planStepID := range []string{"task-wiring", "workflow-wiring"} {
		later, found := taskByPlanStepID(t, fx, created.Run.ID, planStepID)
		if !found {
			t.Fatalf("planned task %q is missing", planStepID)
		}
		if later.State == domain.WorkflowTaskCompleted {
			t.Fatalf("task %q completed; the test no longer proves task 1 passed with the wiring outstanding", planStepID)
		}
	}

	// And nothing along the way blamed task 1 for that outstanding work.
	if realHasCheckpointPhase(t, fx, created.Run.ID, workflowcore.ReasonFixBudgetExhausted) {
		t.Fatal("the master run recorded fix_budget_exhausted for a correctly scoped task")
	}
	if child := task.ExecutionRunID; child != nil {
		if realHasCheckpointPhase(t, fx, *child, workflowcore.ReasonFixBudgetExhausted) {
			t.Fatal("task 1's child run recorded fix_budget_exhausted for work assigned to tasks 4 and 5")
		}
		run, _, err := fx.store.GetWorkflowRun(ctx, *child)
		if err != nil {
			t.Fatalf("GetWorkflowRun(child): %v", err)
		}
		if run.State == domain.WorkflowRunNeedsAttention {
			t.Fatalf("task 1's child parked in needs_attention after being approved")
		}
	}
}

// TestReviewPromptOmitsPlanScopeForAStandaloneRun: a run with no master plan
// has no siblings, and must not be handed an empty "future tasks" section to
// reason about.
func TestReviewPromptOmitsPlanScopeForAStandaloneRun(t *testing.T) {
	prompt := workflowcore.BuildReviewPrompt(workflowcore.ReviewPromptInput{
		Objective:          "Fix the flaky test",
		AcceptanceCriteria: []string{"The test passes ten times in a row."},
		WorkerSessionID:    "sess-1", Branch: "ao/x", WorktreePath: "/tmp/x",
		BaseSHA: "base", HeadSHA: "head", ReviewRunID: "rr-1",
	})
	if strings.Contains(prompt, "Scope of this review") {
		t.Fatalf("standalone run was handed a plan-scope section it has no plan for\n---\n%s", prompt)
	}
	if !strings.Contains(prompt, "The test passes ten times in a row.") {
		t.Fatalf("standalone run lost its acceptance criteria\n---\n%s", prompt)
	}
}
