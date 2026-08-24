package workflow

import (
	stdctx "context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// BoardEntry is one row of the project Board: a top-level workflow run
// projected onto the lifecycle vocabulary, with everything the Board needs to
// say what AO is doing right now without the user opening a database.
//
// It is deliberately built from a read-only pass over durable rows. GetRun
// drives the review/fix/verify cascade as a side effect of reading, which is
// correct for the run detail page but wrong for a board that polls every run in
// a project every couple of seconds. Progression is the wake poller's job (see
// maybeScheduleAutonomousHeartbeat); the Board only reports.
type BoardEntry struct {
	Run       domain.WorkflowRun
	Lifecycle Lifecycle
	// Steps is the Plan/Work/Review/Fix/Verify checklist for a single-task run,
	// or for a master run's currently running child task.
	Steps []StepProgress
	// Tasks summarizes a master run's fan-out. Zero Total for a single-task
	// run, which has no workflow_tasks rows at all.
	Tasks TaskProgress
	// ChildTasks lists a master run's planned tasks in ordinal order, each with
	// its own derived phase when it has a child run to derive one from.
	ChildTasks []BoardChildTask
	// ActivePhase is the phase the Board shows as the headline. For a master
	// run with a running task it is that child's phase (Reviewing, Fixing, …)
	// rather than the parent's flat "running", because "what is AO doing" is
	// answered by the child.
	ActivePhase Phase
	// SessionID, Harness and Model describe the worker/reviewer currently
	// attached to the active step. Empty when nothing is dispatched.
	SessionID string
	Harness   string
	Model     string
	// ExecutionMode is "autonomous" or "manual", from the run's frozen policy
	// snapshot.
	ExecutionMode string
	// ErrorClass is the typed cause on the newest attempt that recorded one.
	ErrorClass domain.WorkflowErrorClass
	// BranchWait names the repository+branch this run is queued on, who owns it
	// and whether the wait clears by itself (Checkpoint 8P-E.13A). Nil unless
	// the run — or, for a master, its running child — is genuinely queued.
	BranchWait *BranchWait
	// ReviewCycles is how many review runs this run's review step has been
	// through — the cheap, always-available half of Checkpoint 8P-E.12 §9's
	// token/cost observability. Token totals stay on the run detail endpoint,
	// where the existing usage reader already computes them with per-field
	// certainty.
	ReviewCycles int
}

// BoardChildTask is one planned task of a master run.
type BoardChildTask struct {
	TaskID string
	// PlanStepID is the id this task is known by outside the daemon (the API's
	// WorkflowTaskView.ID). It travels with TaskID so a reader can translate
	// the planner projection's dependency task ids without a second lookup.
	PlanStepID string
	Ordinal    int64
	Title      string
	State      domain.WorkflowTaskState
	// WaitReason is present for an undispatched blocked task and distinguishes
	// dependency ordering from a write-set lane conflict.
	WaitReason string
	RunID      string
	// Phase is derived from the child run when one exists. Empty for a task
	// that has never been dispatched — which is a fact ("not started"), not an
	// unknown.
	Phase Phase
	Steps []StepProgress
	// Planner is the task's planner-level projection (see
	// task_planner_view.go). Nil only when the projection could not be built at
	// all; a task with nothing planner-level to say carries a view with an
	// empty Status rather than no view.
	Planner *TaskPlannerView
}

// ProjectBoard projects every top-level workflow run of a project.
//
// Child runs of a master are omitted from the top level: they appear nested
// under their parent as BoardChildTask entries, so a seven-task objective reads
// as one workflow with seven tasks rather than eight unrelated cards.
//
// Terminal runs are included only when they finished within retention, so the
// Board shows a run reaching "Completed" instead of having it vanish the moment
// it succeeds.
//
// Archived runs are excluded outright — see ProjectBoardHistory for the view
// that reads them back.
func (c *Coordinator) ProjectBoard(ctx stdctx.Context, projectID string, retention time.Duration) ([]BoardEntry, error) {
	runs, err := c.store.ListWorkflowRuns(ctx, projectID)
	if err != nil {
		return nil, err
	}
	cutoff := c.clock().Add(-retention)
	entries := make([]BoardEntry, 0, len(runs))
	for _, run := range runs {
		if run.ParentWorkflowID != nil {
			continue
		}
		// An archived run is history by explicit human decision. Unlike the
		// retention rule below it never comes back, and unlike a state filter
		// it is not derived from anything the workflow itself did.
		if run.Archived() {
			continue
		}
		if run.State.Terminal() && !terminalWithin(run, cutoff) {
			continue
		}
		entry, err := c.boardEntry(ctx, run)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func terminalWithin(run domain.WorkflowRun, cutoff time.Time) bool {
	stamp := run.UpdatedAt
	if run.CompletedAt != nil {
		stamp = *run.CompletedAt
	} else if run.CancelledAt != nil {
		stamp = *run.CancelledAt
	}
	return stamp.After(cutoff)
}

func (c *Coordinator) boardEntry(ctx stdctx.Context, run domain.WorkflowRun) (BoardEntry, error) {
	detail, err := c.readOnlyDetail(ctx, run)
	if err != nil {
		return BoardEntry{}, err
	}
	entry := BoardEntry{
		Run:           run,
		Lifecycle:     DeriveLifecycle(LifecycleInput{Detail: detail, Questions: detail.Questions}),
		Steps:         DeriveStepProgress(detail),
		Tasks:         DeriveTaskProgress(detail.Tasks),
		ExecutionMode: executionModeLabel(run),
		ErrorClass:    latestErrorClass(detail),
		ReviewCycles:  reviewCycleCount(detail),
		BranchWait:    detail.BranchWait,
	}
	entry.ActivePhase = entry.Lifecycle.Phase
	entry.SessionID, entry.Harness, entry.Model = activeAssignment(detail)

	if detail.Plan == nil {
		return entry, nil
	}

	// Which tasks have a finished execution run, collected from the child runs
	// this loop already reads. It is the one fact the planner projection cannot
	// fold out of the task rows, and gathering it here costs nothing: reading
	// the child runs a second time inside the projection would double every
	// board poll's query count for an answer already in hand.
	childCompleted := map[string]bool{}
	for _, task := range detail.Tasks {
		child := BoardChildTask{TaskID: task.ID, PlanStepID: task.PlanStepID, Ordinal: task.Ordinal, Title: task.Title, State: task.State}
		if scope, err := UnmarshalTaskScope(task.ScopeJSON); err == nil {
			child.WaitReason = string(scope.WaitingReason)
		}
		if task.ExecutionRunID != nil {
			child.RunID = *task.ExecutionRunID
			childRun, ok, cerr := c.store.GetWorkflowRun(ctx, child.RunID)
			if cerr != nil {
				return BoardEntry{}, cerr
			}
			if ok {
				if childRun.State == domain.WorkflowRunCompleted {
					childCompleted[task.ID] = true
				}
				childDetail, derr := c.readOnlyDetail(ctx, childRun)
				if derr != nil {
					return BoardEntry{}, derr
				}
				childLife := DeriveLifecycle(LifecycleInput{Detail: childDetail, Questions: childDetail.Questions})
				child.Phase = childLife.Phase
				child.Steps = DeriveStepProgress(childDetail)
				if task.State == domain.WorkflowTaskRunning {
					// The parent's headline is whatever its running child is
					// actually doing. A parent that says "running" while its
					// child is mid-review is the vaguer of two true answers.
					entry.ActivePhase = childLife.Phase
					entry.Steps = child.Steps
					entry.ReviewCycles = reviewCycleCount(childDetail)
					// A master run has no branch of its own: the branch its
					// running child is queued on IS what the objective is
					// waiting for, and saying so is the difference between
					// "Blocked" and "Blocked on feat/x, held by workflow Y".
					entry.BranchWait = childDetail.BranchWait
					if entry.Lifecycle.LastActivityAt.Before(childLife.LastActivityAt) {
						entry.Lifecycle.LastActivityAt = childLife.LastActivityAt
					}
					// A child stopped on something only a human can resolve is
					// the parent's stop too — otherwise a master run would show
					// a generic "needs attention" with no actionable reason.
					if childLife.Attention == AttentionHuman && entry.Lifecycle.Attention != AttentionHuman {
						entry.Lifecycle.Attention = childLife.Attention
						entry.Lifecycle.AttentionReason = childLife.AttentionReason
						entry.Lifecycle.AttentionAction = childLife.AttentionAction
					}
					if entry.ErrorClass == "" {
						entry.ErrorClass = latestErrorClass(childDetail)
					}
					entry.SessionID, entry.Harness, entry.Model = activeAssignment(childDetail)
				}
			}
		}
		entry.ChildTasks = append(entry.ChildTasks, child)
	}

	// One projection for the whole plan, then attached per task: the dispatch
	// wave and the "is anything else running" half of the status are properties
	// of the plan as a whole, so they cannot be decided one card at a time.
	planner := c.loadTaskPlannerViews(ctx, run, detail.Tasks, childCompleted)
	byTask := make(map[string]*TaskPlannerView, len(planner))
	for i := range planner {
		byTask[planner[i].TaskID] = &planner[i]
	}
	for i := range entry.ChildTasks {
		entry.ChildTasks[i].Planner = byTask[entry.ChildTasks[i].TaskID]
	}
	return entry, nil
}

// readOnlyDetail assembles the same durable facts GetRun returns, without any
// of GetRun's observation/dispatch side effects.
func (c *Coordinator) readOnlyDetail(ctx stdctx.Context, run domain.WorkflowRun) (RunDetail, error) {
	steps, err := c.store.ListWorkflowSteps(ctx, run.ID)
	if err != nil {
		return RunDetail{}, err
	}
	detail := RunDetail{Run: run}
	for _, step := range steps {
		attempts, aerr := c.store.ListWorkflowAttempts(ctx, step.ID)
		if aerr != nil {
			return RunDetail{}, aerr
		}
		sd := StepDetail{Step: step, Attempts: attempts}
		if cp, ok, cerr := c.store.GetLatestWorkflowCheckpointByStep(ctx, step.ID); cerr == nil && ok {
			cp := cp
			sd.LatestCheckpoint = &cp
		}
		if step.Kind == domain.WorkflowStepReview && step.ReviewRunID != nil && c.reviewRuns != nil {
			if rr, found, rerr := c.reviewRuns.GetReviewRun(ctx, *step.ReviewRunID); rerr == nil && found {
				sd.Review = &ReviewSummary{Harness: rr.Harness, Verdict: rr.Verdict, Target: rr.TargetSHA}
			}
		}
		detail.Steps = append(detail.Steps, sd)
	}
	if cps, cerr := c.store.ListWorkflowCheckpoints(ctx, run.ID); cerr == nil {
		for _, cp := range cps {
			// Checkpoint 8P-E.18: incident-ledger rows describe a stop, they are
			// never one. See isIncidentLedgerPhase.
			if isIncidentLedgerPhase(cp.DurablePhase) {
				continue
			}
			if cp.NextAction != "" {
				detail.NextAction = cp.NextAction
			}
			if !cp.CreatedAt.Before(detail.LatestCheckpointAt) {
				detail.LatestCheckpointPhase = cp.DurablePhase
				detail.LatestCheckpointAt = cp.CreatedAt
			}
		}
		if run.State == domain.WorkflowRunWaiting {
			detail.BranchWait = branchWaitFromCheckpoints(cps)
			c.enrichBranchWait(ctx, detail.BranchWait)
		}
	}
	if c.planStore != nil {
		if plan, isMaster, perr := c.planStore.GetWorkflowPlan(ctx, run.ID); perr == nil && isMaster {
			plan := plan
			detail.Plan = &plan
			if tasks, terr := c.planStore.ListWorkflowTasks(ctx, run.ID); terr == nil {
				detail.Tasks = tasks
			}
		}
	}
	if c.questionsStore != nil {
		if qs, qerr := c.questionsStore.ListWorkflowQuestionsByRun(ctx, run.ID); qerr == nil {
			detail.Questions = qs
		}
	}
	if c.wakeScheduler != nil {
		if next, werr := c.wakeScheduler.NextForRun(ctx, domain.WorkflowRunID(run.ID)); werr == nil && next != nil {
			at := next.ScheduledAt
			detail.NextWakeAt = &at
			detail.WaitReason = string(next.Reason)
			detail.WakeAttemptCount = next.AttemptCount
		}
	}
	return detail, nil
}

// activeAssignment names the session/harness/model attached to the first
// non-terminal step that has one. It never reports a session from a step that
// has already finished: a completed work step's session is history, not the
// thing AO is currently using.
func activeAssignment(d RunDetail) (sessionID, harness, model string) {
	for _, s := range d.Steps {
		if s.Step.State.Terminal() || s.Step.Kind == domain.WorkflowStepAdvance {
			continue
		}
		if s.Step.SessionID != nil {
			sessionID = *s.Step.SessionID
		}
		harness = s.Step.AssignedHarness
		if len(s.Attempts) > 0 {
			last := s.Attempts[len(s.Attempts)-1]
			if last.Harness != "" {
				harness = last.Harness
			}
			model = last.Model
		}
		if sessionID != "" || harness != "" {
			return sessionID, harness, model
		}
	}
	return sessionID, harness, model
}

func executionModeLabel(run domain.WorkflowRun) string {
	if policyForRun(run).Execution.AutonomousMode {
		return "autonomous"
	}
	return "manual"
}

// reviewCycleCount counts recorded attempts on the review step — one per review
// cycle actually dispatched.
func reviewCycleCount(d RunDetail) int {
	for _, s := range d.Steps {
		if s.Step.Kind == domain.WorkflowStepReview {
			return len(s.Attempts)
		}
	}
	return 0
}
