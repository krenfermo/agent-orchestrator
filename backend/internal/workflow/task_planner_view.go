package workflow

import (
	stdctx "context"
	"sort"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/integration"
)

// This file is the read side of everything the planner decided about a task and
// everything that has since happened to it: which strategy it was given, what it
// waits on, which dispatch wave it belongs to, what it probably writes, which
// AO-owned worktree and branch hold its work, and where that work is on its way
// to the target branch.
//
// It is a projection, not a state machine. Every field is folded out of durable
// rows that already exist -- the task's scope JSON, the persisted pair verdicts,
// the worktree records, and the integration ledger -- so nothing here can invent
// a status a later reader could not check against the same rows. That is why
// DeriveTaskPlannerViews is a pure function over plain data: the loader below
// reads, and the derivation decides, and the two never mix.

// TaskPlannerStatus is the planner-level vocabulary the Board renders for one
// planned task.
//
// It is deliberately NARROWER than the task state plus the child run's phase,
// and it does not replace either. A task's state says what the plan thinks of
// it and the child's phase says what the worker is doing; neither can say "this
// one is waiting because a sibling writes the same files", "this one is queued
// behind the integration lane", or "this one's work is already on the target".
// Those are the three questions parallel execution creates and the only ones
// this vocabulary answers.
//
// The empty value is the normal case, not a gap: a task nobody is waiting on,
// that is not integrating and is not running beside anything, has no
// planner-level thing to say, and the Board keeps rendering its ordinary state
// or phase for it.
type TaskPlannerStatus string

const (
	// TaskPlannerRunningInParallel means this task is running AND at least one
	// sibling is running at the same time. It is the observed fact, not the
	// plan's intention: a task that merely *may* run in parallel but happens to
	// be the only one in flight is not running in parallel, and labelling it so
	// would be the one claim on this list a user could disprove by looking.
	TaskPlannerRunningInParallel TaskPlannerStatus = "running_in_parallel"
	// TaskPlannerWaitingForDependency means the dispatcher held this task
	// because a task it depends on has not completed.
	TaskPlannerWaitingForDependency TaskPlannerStatus = "waiting_for_dependency"
	// TaskPlannerWaitingForConflict means the dispatcher held this task because
	// a sibling it probably write-conflicts with is currently running. It is
	// distinct from a dependency wait because the remedy is different: nothing
	// is missing, two tasks simply may not touch the same region at once.
	TaskPlannerWaitingForConflict TaskPlannerStatus = "waiting_for_conflict"
	// TaskPlannerReadyToIntegrate means the task's execution run finished and
	// its work has not reached the target yet -- it is queued behind the
	// integration lane, behind a dependency that has not landed, or behind one
	// more review cycle its rebase asked for.
	TaskPlannerReadyToIntegrate TaskPlannerStatus = "ready_to_integrate"
	// TaskPlannerIntegrating means the integration ledger holds an intent for
	// this task with no result after it: the Coordinator is inside the lane
	// right now, or a restart interrupted it there.
	TaskPlannerIntegrating TaskPlannerStatus = "integrating"
	// TaskPlannerConflict means the task is parked on something only a person
	// can resolve -- in practice an integration conflict, with the conflicting
	// files and the three SHAs recorded alongside it.
	TaskPlannerConflict TaskPlannerStatus = "conflict"
	// TaskPlannerIntegrated means the task's work is durably on its target.
	TaskPlannerIntegrated TaskPlannerStatus = "integrated"
)

// TaskPlannerStatuses is the whole vocabulary, in the order a plan moves
// through it. It exists so the API enum and the renderer's label table are
// derived from one list rather than three hand-kept copies.
var TaskPlannerStatuses = []TaskPlannerStatus{
	TaskPlannerRunningInParallel,
	TaskPlannerWaitingForDependency,
	TaskPlannerWaitingForConflict,
	TaskPlannerReadyToIntegrate,
	TaskPlannerIntegrating,
	TaskPlannerConflict,
	TaskPlannerIntegrated,
}

// TaskWriteScopeView is the probable write scope of one task, as the classifier
// last estimated or observed it.
type TaskWriteScopeView struct {
	// Source says whether the paths below are a guess from plan text or what
	// the task's execution actually wrote. It is the difference between a
	// scheduling hint and evidence, and reading the paths without it is how an
	// estimate gets quoted as a fact.
	Source     domain.WorkflowTaskScopeSource
	WritePaths []string
	ReadPaths  []string
	Packages   []string
	Components []string
	Files      []string
}

// TaskWorktreeView is the AO-owned checkout and branch a task's work lives in.
// Nil for a direct-branch task, which has neither.
type TaskWorktreeView struct {
	Path          string
	Branch        string
	TargetBranch  string
	BaseSHA       string
	State         domain.TaskWorktreeState
	IntegratedSHA string
	BranchDeleted bool
}

// TaskIntegrationView is the newest integration ledger row for one task.
//
// Newest rather than all of them: the Board answers "where is this task's work
// now", and every earlier attempt is history the run detail's own ledger
// already carries in full.
type TaskIntegrationView struct {
	Outcome         string
	Strategy        string
	SourceSHA       string
	TargetBeforeSHA string
	TargetAfterSHA  string
	BaseSHA         string
	Replayed        bool
	ConflictFiles   []string
	AttentionReason string
}

// TaskPlannerView is one task's whole planner-level account.
type TaskPlannerView struct {
	TaskID  string
	Ordinal int64
	Status  TaskPlannerStatus
	// ExecutionStrategy is how the plan said this task may be scheduled against
	// its siblings; ExecutionMode is the workspace it was given, already
	// resolved through the project's own setting (see
	// domain.ResolveTaskExecutionMode) so a reader never has to combine the two
	// itself. Downgrade is non-nil exactly when the task got less than the
	// project asked for, and says why.
	ExecutionStrategy domain.WorkflowTaskExecutionStrategy
	ExecutionMode     domain.ExecutionMode
	Downgrade         *domain.WorkflowTaskExecutionDowngrade
	// Dependencies are the task ids this task's dispatch waits on;
	// IntegrationDependencies are the ones its *integration* waits on, which is
	// a superset -- a probable write conflict orders two tasks at the target
	// even with no dependency edge between them.
	Dependencies            []string
	IntegrationDependencies []string
	WaitingReason           domain.WorkflowTaskWaitingReason
	// ParallelGroup is the dispatch wave this task belongs to, 1-based (see
	// AssignParallelGroups). Siblings sharing a group may run at the same time;
	// Size is how many tasks are in it, so a group of one reads as "nothing to
	// run alongside" rather than as a missing value.
	ParallelGroup     int
	ParallelGroupSize int
	WriteScope        TaskWriteScopeView
	Worktree          *TaskWorktreeView
	Integration       *TaskIntegrationView
}

// TaskPlannerInput is everything DeriveTaskPlannerViews is allowed to know. It
// is plain data on purpose: the same rows always produce the same views, and
// the derivation can be tested without a store.
type TaskPlannerInput struct {
	// ProjectMode is the project's configured execution mode, used only to
	// resolve a task that recorded no per-task selection of its own.
	ProjectMode domain.ExecutionMode
	Tasks       []domain.WorkflowTask
	Graph       TaskGraphSnapshot
	// Worktrees are the AO worktree records for these tasks, in any order.
	Worktrees []domain.TaskWorktreeRecord
	// Integrations is the run's integration ledger, oldest first, exactly as
	// ListTaskIntegrations returns it.
	Integrations []TaskIntegrationRecord
	// ChildRunCompleted marks the tasks whose execution run has durably
	// completed. It is what separates "still working" from "finished and
	// waiting for the lane", and it is supplied by the caller because both call
	// sites already hold the child runs -- re-reading them here would be a
	// second query for a fact the reader has in hand.
	ChildRunCompleted map[string]bool
}

// DeriveTaskPlannerViews projects one master run's tasks onto the planner
// vocabulary. Pure and deterministic: no IO, no clock, no randomness.
func DeriveTaskPlannerViews(in TaskPlannerInput) []TaskPlannerView {
	groups := AssignParallelGroups(in.Tasks, in.Graph)
	groupSize := map[int]int{}
	for _, g := range groups {
		groupSize[g]++
	}

	worktreeByTask := make(map[string]domain.TaskWorktreeRecord, len(in.Worktrees))
	for _, rec := range in.Worktrees {
		worktreeByTask[rec.TaskID] = rec
	}
	// Oldest first, so the last row wins and the map ends up holding the newest
	// attempt per task.
	latestIntegration := make(map[string]TaskIntegrationRecord, len(in.Integrations))
	for _, rec := range in.Integrations {
		if rec.TaskID == "" {
			continue
		}
		latestIntegration[rec.TaskID] = rec
	}

	runningTasks := 0
	for _, t := range in.Tasks {
		if t.State == domain.WorkflowTaskRunning {
			runningTasks++
		}
	}

	out := make([]TaskPlannerView, 0, len(in.Tasks))
	for _, task := range in.Tasks {
		scope, err := UnmarshalTaskScope(task.ScopeJSON)
		if err != nil {
			// A scope that will not parse is not a reason to hide the task. The
			// zero scope is exactly what a task planned before the scope model
			// existed already has, and every reader below tolerates it.
			scope = domain.WorkflowTaskScope{}
		}
		view := TaskPlannerView{
			TaskID:                  task.ID,
			Ordinal:                 task.Ordinal,
			ExecutionStrategy:       scope.ExecutionStrategy,
			ExecutionMode:           domain.ResolveTaskExecutionMode(in.ProjectMode, scope),
			Downgrade:               scope.ExecutionModeDowngrade,
			Dependencies:            append([]string(nil), task.Dependencies...),
			IntegrationDependencies: append([]string(nil), scope.IntegrationDependencies...),
			WaitingReason:           scope.WaitingReason,
			ParallelGroup:           groups[task.ID],
			ParallelGroupSize:       groupSize[groups[task.ID]],
			WriteScope: TaskWriteScopeView{
				Source:     scope.Source,
				WritePaths: append([]string(nil), scope.WritePaths...),
				ReadPaths:  append([]string(nil), scope.ReadPaths...),
				Packages:   append([]string(nil), scope.Packages...),
				Components: append([]string(nil), scope.Components...),
				Files:      append([]string(nil), scope.Files...),
			},
		}
		if len(scope.ObservedWritePaths) > 0 {
			// What the task actually wrote replaces what it was estimated to
			// write. Keeping the estimate alongside evidence would leave a
			// reader to decide which one is true, which is the one decision this
			// projection exists to have already made.
			view.WriteScope.WritePaths = append([]string(nil), scope.ObservedWritePaths...)
		}
		if rec, ok := worktreeByTask[task.ID]; ok {
			view.Worktree = &TaskWorktreeView{
				Path:          rec.Path,
				Branch:        rec.Branch,
				TargetBranch:  rec.TargetBranch,
				BaseSHA:       rec.BaseSHA,
				State:         rec.State,
				IntegratedSHA: rec.IntegratedSHA,
				BranchDeleted: rec.BranchDeleted,
			}
		}
		if rec, ok := latestIntegration[task.ID]; ok {
			view.Integration = &TaskIntegrationView{
				Outcome:         rec.Outcome,
				Strategy:        rec.Strategy,
				SourceSHA:       rec.SourceSHA,
				TargetBeforeSHA: rec.TargetBeforeSHA,
				TargetAfterSHA:  rec.TargetAfterSHA,
				BaseSHA:         rec.BaseSHA,
				Replayed:        rec.Replayed,
				ConflictFiles:   append([]string(nil), rec.ConflictFiles...),
				AttentionReason: rec.AttentionReason,
			}
		}
		view.Status = derivePlannerStatus(task, scope, view.Integration, in.ChildRunCompleted[task.ID], runningTasks)
		out = append(out, view)
	}
	return out
}

// derivePlannerStatus answers, for one task, the single most specific true
// thing on the planner vocabulary.
//
// The order below is precedence, and it runs from the most durable fact to the
// most transient one. A task whose work is on the target is integrated no
// matter what its ledger's earlier rows say; a parked task is a conflict even
// though it is also, technically, still waiting for the lane; and "running in
// parallel" comes last because it is the only entry that is merely a
// description of the moment rather than a position in the task's life.
func derivePlannerStatus(
	task domain.WorkflowTask,
	scope domain.WorkflowTaskScope,
	integ *TaskIntegrationView,
	childCompleted bool,
	runningTasks int,
) TaskPlannerStatus {
	switch {
	// A completed task's work is on the target by construction: the promotion
	// is recorded before the state transition, precisely so a crash between the
	// two can never leave a task counted as done without its code having
	// landed. The ledger row is the corroboration, not the source.
	case task.State == domain.WorkflowTaskCompleted:
		return TaskPlannerIntegrated
	case integ != nil && integ.Outcome == string(integration.OutcomeIntegrated):
		return TaskPlannerIntegrated
	case task.State.Parked():
		return TaskPlannerConflict
	case integ != nil && integ.Outcome == string(integration.OutcomeNeedsAttention):
		return TaskPlannerConflict
	// An intent with no result after it. The Coordinator is in the lane now, or
	// a restart left it there -- and those two are indistinguishable from
	// outside the lane, which is why they share one label.
	case integ != nil && integ.Outcome == string(integration.OutcomeAttempting):
		return TaskPlannerIntegrating
	// The execution run finished and the work has not reached the target. It is
	// queued behind the lane, behind a dependency that has not landed, or behind
	// the extra review cycle a rebase asked for.
	case childCompleted && !task.State.Terminal():
		return TaskPlannerReadyToIntegrate
	case scope.WaitingReason == domain.WorkflowTaskWaitingDependency:
		return TaskPlannerWaitingForDependency
	case scope.WaitingReason == domain.WorkflowTaskWaitingConflict:
		return TaskPlannerWaitingForConflict
	case task.State == domain.WorkflowTaskRunning && runningTasks > 1:
		return TaskPlannerRunningInParallel
	}
	return ""
}

// AssignParallelGroups partitions a plan's tasks into ordered dispatch waves.
//
// A wave is the answer to "what could start together", and it is decided by
// exactly the two rules the dispatcher itself applies (see
// reconcileMasterTasksOnce): a task may not start before anything it depends on
// has completed, and it may not run beside a sibling it probably
// write-conflicts with. Everything else -- how many lanes are free, what order
// the poller happens to visit tasks in -- is scheduling weather, and a group
// number that changed with it would be useless to read.
//
// The result is 1-based so that "group 0" never has to mean both "the first
// wave" and "no group was computed".
//
// Deterministic: tasks are walked in ordinal order and each one takes the
// lowest wave both rules allow, so the same plan always produces the same
// grouping. A dependency edge pointing at a task that is not in the plan is
// ignored rather than treated as unsatisfiable -- it cannot order anything --
// and a dependency cycle, which the planner rejects but a hand-edited row could
// still contain, degrades to the first wave for the tasks inside it instead of
// looping.
func AssignParallelGroups(tasks []domain.WorkflowTask, graph TaskGraphSnapshot) map[string]int {
	groups := make(map[string]int, len(tasks))
	if len(tasks) == 0 {
		return groups
	}
	byID := make(map[string]domain.WorkflowTask, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}

	ordered := append([]domain.WorkflowTask(nil), tasks...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Ordinal != ordered[j].Ordinal {
			return ordered[i].Ordinal < ordered[j].Ordinal
		}
		return ordered[i].ID < ordered[j].ID
	})

	depth := make(map[string]int, len(tasks))
	visiting := map[string]bool{}
	var depthOf func(id string) int
	depthOf = func(id string) int {
		if d, ok := depth[id]; ok {
			return d
		}
		if visiting[id] {
			// A cycle. It cannot be ordered, so it is not allowed to order
			// anything either.
			return 0
		}
		visiting[id] = true
		best := 0
		for _, dep := range byID[id].Dependencies {
			if _, known := byID[dep]; !known {
				continue
			}
			if d := depthOf(dep) + 1; d > best {
				best = d
			}
		}
		delete(visiting, id)
		depth[id] = best
		return best
	}

	// Only tasks already placed can push this one along: the earlier ordinal
	// keeps the earlier wave, which is what makes the assignment stable under
	// re-derivation.
	for _, task := range ordered {
		wave := depthOf(task.ID)
		conflicts := graph.ConflictsFor(task.ID)
		for {
			clash := false
			for _, other := range conflicts {
				if g, ok := groups[other]; ok && g == wave+1 {
					clash = true
					break
				}
			}
			if !clash {
				break
			}
			wave++
		}
		groups[task.ID] = wave + 1
	}
	return groups
}

// loadTaskPlannerViews reads the durable rows DeriveTaskPlannerViews needs and
// projects them.
//
// Every read below is optional in the same way the dependency behind it is: a
// coordinator with no plan store, no worktree record reader or no project
// reader still produces views, with the parts it cannot see left empty rather
// than guessed at.
func (c *Coordinator) loadTaskPlannerViews(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	tasks []domain.WorkflowTask,
	childCompleted map[string]bool,
) []TaskPlannerView {
	if len(tasks) == 0 {
		return nil
	}
	in := TaskPlannerInput{
		ProjectMode:       c.projectExecutionMode(ctx, run.ProjectID),
		Tasks:             tasks,
		ChildRunCompleted: childCompleted,
	}
	if graph, err := c.LoadTaskGraph(ctx, run.ID); err == nil {
		in.Graph = graph
	}
	if c.taskWorktreeRecords != nil {
		if recs, err := c.taskWorktreeRecords.ListTaskWorktreesByRun(ctx, run.ID); err == nil {
			in.Worktrees = recs
		}
	}
	if recs, err := c.ListTaskIntegrations(ctx, run.ID); err == nil {
		in.Integrations = recs
	}
	return DeriveTaskPlannerViews(in)
}

// completedChildRuns reports which of these tasks have a durably completed
// execution run. It is the one fact the planner projection cannot fold out of
// the task rows themselves, and it is deliberately scoped to tasks that are
// still in flight: a completed or failed task's child state changes nothing
// about the label it gets.
func (c *Coordinator) completedChildRuns(ctx stdctx.Context, tasks []domain.WorkflowTask) map[string]bool {
	out := map[string]bool{}
	for _, task := range tasks {
		if task.ExecutionRunID == nil || task.State.Terminal() {
			continue
		}
		child, ok, err := c.store.GetWorkflowRun(ctx, *task.ExecutionRunID)
		if err != nil || !ok {
			continue
		}
		if child.State == domain.WorkflowRunCompleted {
			out[task.ID] = true
		}
	}
	return out
}
