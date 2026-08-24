package controllers

import (
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// This file is the wire shape of the per-task planner projection: what the plan
// decided about a task (strategy, dependencies, dispatch wave, probable write
// scope) and what has happened to it since (which AO worktree and branch hold
// its work, and where that work is on its way to the target branch).
//
// It is shared verbatim by the run-detail task list and the Board's task rows.
// One shape rather than two, because the two surfaces answer the same question
// at different zoom levels, and a Board card that could say "Waiting for
// conflict" while the detail page called the same task something else would be
// a bug nobody could see from either page alone.

// WorkflowTaskPlannerView is one task's planner-level account.
type WorkflowTaskPlannerView struct {
	// Status is the planner-level label. Empty is the normal case, not a gap: a
	// task nobody is waiting on, that is not integrating and is not running
	// beside anything, has no planner-level thing to say, and the caller keeps
	// rendering its ordinary state or phase.
	Status string `json:"status,omitempty" enum:"running_in_parallel,waiting_for_dependency,waiting_for_conflict,ready_to_integrate,integrating,conflict,integrated"`
	// ExecutionStrategy is how the plan said this task may be scheduled against
	// its siblings.
	ExecutionStrategy string `json:"executionStrategy,omitempty" enum:"parallel,sequential,serialized"`
	// ExecutionMode is the workspace this task was given, already resolved
	// through the project's own setting — never the raw per-task override, which
	// is empty for every project where all tasks share one mode.
	ExecutionMode string `json:"executionMode,omitempty" enum:"direct_branch,isolated_worktree,smart_parallel_worktrees"`
	// Downgrade is present exactly when the task got a weaker mode than the
	// project asked for, and says why.
	Downgrade *WorkflowTaskDowngradeView `json:"downgrade,omitempty"`
	// Dependencies are the tasks this one's DISPATCH waits on;
	// IntegrationDependencies are the ones its INTEGRATION waits on, which is a
	// superset — a probable write conflict orders two tasks at the target even
	// with no dependency edge between them. Both are plan-step ids, matching
	// WorkflowTaskView.dependencies.
	Dependencies            []string `json:"dependencies"`
	IntegrationDependencies []string `json:"integrationDependencies"`
	// WaitingReason is the dispatcher's own durable explanation for a held
	// task. Empty when the task has not been evaluated as blocked.
	WaitingReason string `json:"waitingReason,omitempty" enum:"waiting_for_dependencies,waiting_for_write_conflict"`
	// ParallelGroup is the 1-based dispatch wave: siblings sharing a group may
	// run at the same time. ParallelGroupSize is how many tasks are in it, so a
	// group of one reads as "nothing to run alongside" rather than as a missing
	// value.
	ParallelGroup     int                               `json:"parallelGroup"`
	ParallelGroupSize int                               `json:"parallelGroupSize"`
	WriteScope        WorkflowTaskWriteScopeView        `json:"writeScope"`
	Worktree          *WorkflowTaskWorktreeView         `json:"worktree,omitempty"`
	Integration       *WorkflowTaskIntegrationStateView `json:"integration,omitempty"`
}

// WorkflowTaskDowngradeView explains a task denied the execution mode its
// project configured.
type WorkflowTaskDowngradeView struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Serial additionally forbids running at the same time as Conflicts.
	Serial bool `json:"serial,omitempty"`
	// Reason is the stable code; Detail is the sentence behind it.
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Conflicts names the siblings the decision was made about, as plan-step
	// ids, so it can be checked against the plan rather than trusted.
	Conflicts []string `json:"conflicts,omitempty"`
}

// WorkflowTaskWriteScopeView is a task's probable write footprint.
type WorkflowTaskWriteScopeView struct {
	// Source is the difference between a scheduling hint and evidence:
	// "estimated" paths are inferred from plan text, "observed" ones are what
	// the task's execution actually wrote.
	Source     string   `json:"source,omitempty" enum:"estimated,observed"`
	WritePaths []string `json:"writePaths"`
	ReadPaths  []string `json:"readPaths"`
	Packages   []string `json:"packages"`
	Components []string `json:"components"`
	Files      []string `json:"files"`
}

// WorkflowTaskWorktreeView is the AO-owned checkout and branch holding a task's
// work. Absent for a direct-branch task, which has neither.
type WorkflowTaskWorktreeView struct {
	Path         string `json:"path,omitempty"`
	Branch       string `json:"branch,omitempty"`
	TargetBranch string `json:"targetBranch,omitempty"`
	BaseSHA      string `json:"baseSha,omitempty"`
	State        string `json:"state,omitempty" enum:"creating,active,integrated,released,preserved,failed"`
	// IntegratedSHA is the commit this task's work reached its target at, and
	// the authorization for every teardown that follows. Empty until it lands.
	IntegratedSHA string `json:"integratedSha,omitempty"`
	BranchDeleted bool   `json:"branchDeleted,omitempty"`
}

// WorkflowTaskIntegrationStateView is the newest integration ledger row for one
// task — where its work is on its way to the target, and what moved.
type WorkflowTaskIntegrationStateView struct {
	Outcome         string `json:"outcome,omitempty" enum:"attempting,integrated,needs_attention"`
	Strategy        string `json:"strategy,omitempty" enum:"fast_forward,rebase_fast_forward,cherry_pick,merge_commit,no_op"`
	SourceSHA       string `json:"sourceSha,omitempty"`
	TargetBeforeSHA string `json:"targetBeforeSha,omitempty"`
	TargetAfterSHA  string `json:"targetAfterSha,omitempty"`
	BaseSHA         string `json:"baseSha,omitempty"`
	// Replayed reports that the work had to be moved onto a target that had
	// advanced while the task ran.
	Replayed bool `json:"replayed,omitempty"`
	// ConflictFiles are the exact repository-relative paths git reported as
	// unmerged, in git's order. Empty for an outcome that is not a conflict.
	ConflictFiles   []string `json:"conflictFiles,omitempty"`
	AttentionReason string   `json:"attentionReason,omitempty"`
}

// workflowTaskPlannerView converts one projected task.
//
// planIDByTask translates task ids into the plan-step ids this API speaks in
// everywhere else (see WorkflowTaskView.ID). A dependency whose task id has no
// plan step is dropped rather than emitted raw: half-translated ids in one list
// are worse than a shorter list, because a caller cannot tell which kind it is
// holding.
func workflowTaskPlannerView(v *workflowcore.TaskPlannerView, planIDByTask map[string]string) *WorkflowTaskPlannerView {
	if v == nil {
		return nil
	}
	out := &WorkflowTaskPlannerView{
		Status:                  string(v.Status),
		ExecutionStrategy:       string(v.ExecutionStrategy),
		ExecutionMode:           string(v.ExecutionMode),
		Dependencies:            planStepIDs(v.Dependencies, planIDByTask),
		IntegrationDependencies: planStepIDs(v.IntegrationDependencies, planIDByTask),
		WaitingReason:           string(v.WaitingReason),
		ParallelGroup:           v.ParallelGroup,
		ParallelGroupSize:       v.ParallelGroupSize,
		WriteScope: WorkflowTaskWriteScopeView{
			Source:     string(v.WriteScope.Source),
			WritePaths: nonNilStrings(v.WriteScope.WritePaths),
			ReadPaths:  nonNilStrings(v.WriteScope.ReadPaths),
			Packages:   nonNilStrings(v.WriteScope.Packages),
			Components: nonNilStrings(v.WriteScope.Components),
			Files:      nonNilStrings(v.WriteScope.Files),
		},
	}
	if d := v.Downgrade; d != nil {
		out.Downgrade = &WorkflowTaskDowngradeView{
			From:      string(d.From),
			To:        string(d.To),
			Serial:    d.Serial,
			Reason:    d.Reason,
			Detail:    d.Detail,
			Conflicts: planStepIDs(d.Conflicts, planIDByTask),
		}
	}
	if w := v.Worktree; w != nil {
		out.Worktree = &WorkflowTaskWorktreeView{
			Path:          w.Path,
			Branch:        w.Branch,
			TargetBranch:  w.TargetBranch,
			BaseSHA:       w.BaseSHA,
			State:         string(w.State),
			IntegratedSHA: w.IntegratedSHA,
			BranchDeleted: w.BranchDeleted,
		}
	}
	if i := v.Integration; i != nil {
		out.Integration = &WorkflowTaskIntegrationStateView{
			Outcome:         i.Outcome,
			Strategy:        i.Strategy,
			SourceSHA:       i.SourceSHA,
			TargetBeforeSHA: i.TargetBeforeSHA,
			TargetAfterSHA:  i.TargetAfterSHA,
			BaseSHA:         i.BaseSHA,
			Replayed:        i.Replayed,
			ConflictFiles:   i.ConflictFiles,
			AttentionReason: i.AttentionReason,
		}
	}
	return out
}

// workflowTaskPlannerViews indexes a run's projection by task id, so each task
// row can pick up its own without a nested scan.
func workflowTaskPlannerViews(views []workflowcore.TaskPlannerView) map[string]*workflowcore.TaskPlannerView {
	out := make(map[string]*workflowcore.TaskPlannerView, len(views))
	for i := range views {
		out[views[i].TaskID] = &views[i]
	}
	return out
}

func planStepIDs(taskIDs []string, planIDByTask map[string]string) []string {
	out := make([]string, 0, len(taskIDs))
	for _, id := range taskIDs {
		if planID := planIDByTask[id]; planID != "" {
			out = append(out, planID)
		}
	}
	return out
}

// nonNilStrings keeps a JSON array an array. An omitted list and an empty one
// mean different things to a client that renders "writes: none" from one and
// "unknown" from the other.
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
