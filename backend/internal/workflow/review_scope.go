package workflow

import (
	stdctx "context"
	"sort"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ReviewTaskScope is the boundary one child task's review must respect.
//
// It exists because of a real failure mode. A master plan decomposed an
// objective into several tasks; task 1 built a self-contained package and task
// 4/5 were the ones assigned to wire that package into the Task/Workflow
// lifecycle. The reviewer was handed only run.Objective (labelled "objective of
// the overall run") and literally "Acceptance criteria: (none recorded)", so it
// judged task 1 against the whole plan, requested changes for the wiring that
// task 4/5 owned, and did so again after every fix — review, fix, review, fix,
// until the fix budget was exhausted and the run parked in needs_attention over
// work that was never task 1's to do.
//
// Nothing about the plan was ambiguous; the reviewer simply could not see it.
// This type carries the three facts that make the boundary decidable:
//
//   - AcceptanceCriteria: what THIS task must satisfy (from the run's own plan
//     artifact, the same source BuildFixPrompt already uses).
//   - AvailableDependencies: the tasks already delivered, so the reviewer knows
//     what it is entitled to assume exists.
//   - FuturePlannedTasks: the tasks that own the rest of the work, so their
//     absence reads as "not yet", not as "missing".
type ReviewTaskScope struct {
	AcceptanceCriteria    []string
	AvailableDependencies []string
	FuturePlannedTasks    []string
}

// reviewScopeForRun resolves the review boundary for the run under review.
//
// Best-effort in the same way every other read on this path is: a scope that
// cannot be resolved degrades to "no plan context", which is exactly the
// pre-existing behavior, never to a failed review dispatch. A run with no
// master-plan parent (a standalone objective) has no siblings and therefore no
// future scope — its acceptance criteria alone are the boundary.
func (c *Coordinator) reviewScopeForRun(ctx stdctx.Context, run domain.WorkflowRun) ReviewTaskScope {
	scope := ReviewTaskScope{}
	if artifact, err := c.planArtifactForRun(ctx, run); err == nil {
		scope.AcceptanceCriteria = artifact.AcceptanceCriteria
	}
	if run.ParentWorkflowID == nil || run.PlannedTaskID == nil || c.planStore == nil {
		return scope
	}
	tasks, err := c.planStore.ListWorkflowTasks(ctx, *run.ParentWorkflowID)
	if err != nil {
		return scope
	}
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].Ordinal < tasks[j].Ordinal })

	var self *domain.WorkflowTask
	byID := make(map[string]domain.WorkflowTask, len(tasks))
	for i := range tasks {
		byID[tasks[i].ID] = tasks[i]
		if tasks[i].ID == *run.PlannedTaskID {
			self = &tasks[i]
		}
	}
	if self == nil {
		return scope
	}
	for _, depID := range self.Dependencies {
		dep, ok := byID[depID]
		if !ok {
			continue
		}
		scope.AvailableDependencies = append(scope.AvailableDependencies, taskScopeLabel(dep))
	}
	// "Future" is deliberately every task of the plan that is not this one and
	// has not completed — not merely the ones with a higher ordinal. A task that
	// runs in parallel, or one this task does not depend on, is just as much
	// somebody else's work, and its absence is just as much not a defect here.
	for _, t := range tasks {
		if t.ID == self.ID || t.State == domain.WorkflowTaskCompleted {
			continue
		}
		scope.FuturePlannedTasks = append(scope.FuturePlannedTasks, taskScopeLabel(t))
	}
	return scope
}

// taskScopeLabel names a sibling task in one line the reviewer can match
// against what it sees in the worktree: its ordinal, its title, and the first
// line of its description (which is where a planner puts the "what", the rest
// being detail the reviewer of a different task does not need).
func taskScopeLabel(t domain.WorkflowTask) string {
	label := "Task " + strconv.FormatInt(t.Ordinal, 10) + ": " + strings.TrimSpace(t.Title)
	if first := firstLine(t.Description); first != "" {
		label += " — " + first
	}
	return label
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
