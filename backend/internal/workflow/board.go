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

	// Presentation is P3-B's whole point: the Board renders the SAME projection
	// the run detail page renders, derived by the same DerivePresentation over
	// the same durable facts. A card and the page it opens cannot disagree
	// about the stage, whose turn it is, what AO is doing, where it is working,
	// what may be pressed, or whether anything is left to integrate — because
	// there is one derivation and this is it.
	Presentation Presentation
	// Strategy is the run's recorded execution strategy ("task", "autonomous",
	// "master"), empty for a legacy run whose mapping has not been reconciled.
	Strategy string
	// Repairs are the automatic repairs of THIS run, projected inline (§6).
	// A repair run is an ordinary top-level run in the project; nesting it here
	// is what stops one incident from reading as three workflows.
	Repairs []BoardRepair
	// RepairOfRunID/RepairGeneration are set only on an entry that IS a repair
	// and could not be nested because its origin is not on this board. They
	// keep it labelled as a repair rather than passing it off as ordinary work.
	RepairOfRunID    string
	RepairGeneration int
	// RepairBudget is policy.MaxRepairCycles for this run — the M in "attempt N
	// of M" its nested repairs are numbered against.
	RepairBudget int
	// ObjectiveTitle/ObjectiveSummary/ObjectiveTruncated are §17's bounded
	// presentation of an objective that may be a 128 KiB specification. Nothing
	// is truncated in storage; the detail endpoint still returns it in full.
	ObjectiveTitle     string
	ObjectiveSummary   string
	ObjectiveTruncated bool
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
	// Presentation is the child run's own human projection, derived by the same
	// DerivePresentation the parent's is. It is what §5's aggregation reads:
	// the parent's headline is a function of its children's stated stages, not
	// of whichever child's row was written last. Zero value for a task that has
	// never been dispatched, which has no run to project.
	Presentation Presentation
	// HasRun reports that Presentation was derived from a real child run, so a
	// zero-valued projection is distinguishable from a genuinely `preparing`
	// one.
	HasRun bool
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
	view, err := c.ProjectBoardView(ctx, projectID, BoardQuery{Retention: retention})
	if err != nil {
		return nil, err
	}
	return view.Entries, nil
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
		RepairBudget:  detail.Repair.Budget,
	}
	entry.ActivePhase = entry.Lifecycle.Phase
	entry.SessionID, entry.Harness, entry.Model = activeAssignment(detail)
	if sel, ok := RecordedExecutionStrategy(run); ok {
		entry.Strategy = string(sel.Effective)
	}
	entry.ObjectiveTitle, entry.ObjectiveSummary, entry.ObjectiveTruncated = boardObjective(run.Objective)

	if detail.Plan == nil {
		entry.Presentation = c.presentationFor(ctx, detail, entry.Lifecycle)
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
				child.Presentation = c.presentationFor(ctx, childDetail, childLife)
				child.HasRun = true
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
	entry.Presentation = c.presentationFor(ctx, detail, entry.Lifecycle)
	entry.Presentation = aggregateParentPresentation(entry)
	return entry, nil
}

// aggregateParentPresentation is §5: the canonical parent status.
//
// The rule it replaces was "whatever the running child is doing", with the
// parent's own row as the fallback — which is a timestamp race dressed as a
// policy: two children moving at once gave a parent whose headline depended on
// which write landed second. This is an explicit AUTHORITY ordering instead,
// and every clause names a durable fact rather than a recency:
//
//  1. a child that needs a person outranks everything. The objective cannot
//     finish, and the parent must say so — with the child's own summary, so
//     the card names the incident instead of saying "needs attention".
//  2. a child AO is repairing by itself is the next most specific true thing.
//     The parent reads `correcting` and, per §5, asks nobody anything.
//  3. otherwise the parent shows the work actually in flight, taking the
//     LOWEST-ordinal executing child so two concurrent tasks produce one stable
//     answer rather than alternating ones.
//  4. a parent with children but none of the above keeps its own projection —
//     waiting, completed, cancelled or failed as its own lifecycle says.
//
// It never invents a stage for the parent that no child and no parent fact
// supports, and it never turns a terminal parent into a live one: a completed
// objective whose child rows are stale stays completed.
func aggregateParentPresentation(entry BoardEntry) Presentation {
	p := entry.Presentation
	if len(entry.ChildTasks) == 0 || p.Stage.Terminal() {
		return p
	}
	var human, repairing, working *BoardChildTask
	for i := range entry.ChildTasks {
		child := &entry.ChildTasks[i]
		if !child.HasRun {
			continue
		}
		cp := child.Presentation
		switch {
		case cp.RequiresHuman:
			if human == nil {
				human = child
			}
		case cp.AutomaticActionActive && !cp.Stage.Terminal():
			if repairing == nil {
				repairing = child
			}
		case !cp.Stage.Terminal() && cp.Stage != StageWaiting:
			if working == nil {
				working = child
			}
		}
	}
	switch {
	case human != nil:
		p.Stage = StageNeedsAttention
		p.RequiresHuman = true
		p.AutomaticActionActive = false
		p.SummaryCode = human.Presentation.SummaryCode
		// The remedy for an objective is not to act on the objective: it is to
		// open the task that stopped. The action is ADDED rather than assumed,
		// because a recommendation AO does not also offer is a dead end.
		p.RecommendedAction = ActionViewBlockingWorkflow
		p.Actions = withAction(p.Actions, Action{ID: ActionViewBlockingWorkflow, Primary: true, Enabled: true})
	case repairing != nil:
		p.Stage = StageCorrecting
		p.RequiresHuman = false
		p.AutomaticActionActive = true
		p.SummaryCode = repairing.Presentation.SummaryCode
	case working != nil:
		p.Stage = working.Presentation.Stage
		p.SummaryCode = working.Presentation.SummaryCode
	}
	// The parent's meaningful activity is the newest meaningful thing anything
	// under it did. A master run's own row barely moves; its children are where
	// the work happens, and a board that sorted masters by the parent row would
	// sink a busy objective below an idle one.
	for i := range entry.ChildTasks {
		if at := entry.ChildTasks[i].Presentation.LastMeaningfulActivityAt; at.After(p.LastMeaningfulActivityAt) {
			p.LastMeaningfulActivityAt = at
		}
	}
	return p
}

// withAction adds an offer if it is not already present, and promotes an
// existing disabled copy rather than duplicating it.
func withAction(actions []Action, add Action) []Action {
	for i := range actions {
		if actions[i].ID != add.ID {
			continue
		}
		actions[i] = add
		return actions
	}
	return append([]Action{add}, actions...)
}

// presentationFor derives the ONE human projection for a run, from the same
// inputs the run detail endpoint feeds DerivePresentation.
//
// The placement and override reads are the only extra queries a Board card
// costs over the pre-P3-B projection, and they are the reads that make "where
// is AO working" and "is there anything to integrate" answerable from a card
// instead of only from the page. Both are optional exactly as they are on the
// detail path: a deployment with no placement authority gets a presentation
// reporting `unknown`, never an invented placement.
func (c *Coordinator) presentationFor(ctx stdctx.Context, detail RunDetail, life Lifecycle) Presentation {
	placements, _ := c.ListPlacements(ctx, detail.Run.ID)
	overrides, _ := c.ListPlacementOverrides(ctx, detail.Run.ID)
	admission, _ := c.AdmissionState(ctx, detail.Run.ID)
	return DerivePresentation(PresentationInput{
		Detail: detail, Lifecycle: life, Placements: placements,
		Overrides: overrides, Admission: admission, Now: c.clock(),
	})
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
				// The verdict a person reads on the Board is the effective one; an
				// adopted late verdict is not a blank review.
				sd.Review = &ReviewSummary{Harness: rr.Harness, Verdict: rr.EffectiveVerdict(), Target: rr.TargetSHA}
			}
		}
		detail.Steps = append(detail.Steps, sd)
	}
	detail.Repair = RepairLifecycle{Budget: policyForRun(run).EffectiveRepairPolicy().MaxRepairCycles}
	if cps, cerr := c.store.ListWorkflowCheckpoints(ctx, run.ID); cerr == nil {
		// One fold, shared by every projection, so the Board, the API, the CLI
		// and the reconciler can never disagree about why a run is stopped.
		// See checkpoint_authority.go.
		applyCheckpointAuthority(&detail, cps)
		if run.State == domain.WorkflowRunWaiting {
			detail.BranchWait = branchWaitFromCheckpoints(cps)
			c.enrichBranchWait(ctx, detail.BranchWait)
		}
		// The repair projection is the run detail page's own — the same
		// repairLifecycleFor, so a card and the page cannot disagree about
		// whether AO is repairing something. It is asked for ONLY when the
		// ledger already carries a repair dispatch: a run that never had one
		// gets the identical zero projection for the price of a slice scan
		// instead of a quiescence proof, which is what keeps a board of runs
		// that have never failed from paying for the machinery of one that
		// did.
		if ledgerHasRepairDispatch(cps) {
			detail.Repair = c.repairLifecycleFor(ctx, run)
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

// ledgerHasRepairDispatch reports whether this run has ever dispatched a
// repair. It is the cheap precondition of the full repair projection: no
// dispatch row means no generation, no repair run and nothing to prove.
func ledgerHasRepairDispatch(cps []domain.WorkflowCheckpoint) bool {
	for _, cp := range cps {
		if cp.DurablePhase == repairDispatchPhase {
			return true
		}
	}
	return false
}
