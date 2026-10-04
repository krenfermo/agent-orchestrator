package workflow

import (
	"fmt"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// TaskStrategyPolicyVersion is the version of the selection policy below,
// stamped into every downgrade record so a decision made today stays
// explainable after the policy changes -- the same contract
// TaskGraphPolicyVersion already carries for the classifier.
const TaskStrategyPolicyVersion = "v1"

// TaskStrategyReason is a stable, machine-checkable code explaining why a task
// did not get the execution strategy its project configured. It is persisted
// in domain.WorkflowTaskExecutionDowngrade.Reason.
type TaskStrategyReason string

const (
	// TaskStrategyReasonWriteConflict means the classifier found at least one
	// sibling this task probably write-conflicts with. The overlap is a fact
	// about the plan, not a doubt about it, so the task is demoted all the way
	// to serial execution against those siblings.
	TaskStrategyReasonWriteConflict TaskStrategyReason = "probable_write_conflict"
	// TaskStrategyReasonUnknownWriteSet means the classifier could not name a
	// single path this task writes. Nothing here says the task conflicts --
	// that is exactly the problem. Independence cannot be shown, so parallel
	// worktrees are not granted.
	TaskStrategyReasonUnknownWriteSet TaskStrategyReason = "unknown_write_set"
	// TaskStrategyReasonCoarseWriteSet means the write set is directory-scoped:
	// the task is known to write somewhere under a directory but not which
	// file. Two such tasks can look disjoint and still collide on a file
	// neither of them named.
	TaskStrategyReasonCoarseWriteSet TaskStrategyReason = "directory_scoped_write_set"
)

// TaskExecutionPlacement is the plan-time answer, for one task, to "which
// working tree may this task's work happen in, and may it happen at the same
// time as its siblings' work".
type TaskExecutionPlacement struct {
	TaskID string
	// Mode is the selected workspace strategy. It is only ever weaker than the
	// project's configured mode, never stronger.
	Mode domain.ExecutionMode
	// Serial reports that the task must not run concurrently with the siblings
	// in Downgrade.Conflicts.
	Serial bool
	// Downgrade is nil when the task got exactly what the project configured.
	Downgrade *domain.WorkflowTaskExecutionDowngrade
}

// SelectTaskExecutionStrategies assigns every planned task an execution
// strategy from the project's setting plus the task graph the classifier
// already produced. Pure and deterministic: no IO, no model call, and no
// re-derivation of any write set -- it reads the persisted decision, which is
// the whole reason that decision is persisted.
//
// The rules, and why they are these rules:
//
//   - Any mode other than smart_parallel_worktrees selects NOTHING. A project
//     on isolated_worktree or direct_branch runs every task the one way it is
//     configured, so there is no per-task choice to make and the returned
//     placement carries no downgrade and no scope mutation. This is what keeps
//     direct-branch planning byte-for-byte identical to the behavior before
//     this selector existed.
//
//   - Under smart_parallel_worktrees a task keeps the mode only when the
//     classifier can affirmatively show it is safe: no probable write conflict
//     with any sibling, AND a write set specific enough to have proven that.
//     The burden of proof points that way deliberately -- being wrong about a
//     conflict costs a serialized task, while being wrong about independence
//     costs two agents editing one file in two worktrees and an integration
//     that cannot be untangled afterwards.
//
//   - A probable write conflict downgrades to isolated_worktree AND serial.
//     A private worktree removes the physical collision but not the
//     integration one: both tasks still have to land in some order.
//
//   - Uncertainty alone downgrades to isolated_worktree without serializing.
//     The task may still run alongside anything the DAG allows; it simply does
//     not get the concurrent-worktree grant that assumes a known write set.
//
// A dependency edge is NOT a downgrade. The DAG already orders those tasks and
// the existing ExecutionStrategy (sequential) records it; demoting them a
// second time would serialize a plan for an ordering it already has.
func SelectTaskExecutionStrategies(mode domain.ExecutionMode, graph TaskGraph) map[string]TaskExecutionPlacement {
	effective := mode.WithDefault()
	out := make(map[string]TaskExecutionPlacement, len(graph.Scopes))
	if !effective.SmartParallel() {
		for id := range graph.Scopes {
			out[id] = TaskExecutionPlacement{TaskID: id, Mode: effective}
		}
		return out
	}

	conflicts := conflictPartnersByTask(graph.Relationships)
	for id, scope := range graph.Scopes {
		out[id] = selectTaskExecutionStrategy(id, scope, conflicts[id], effective)
	}
	return out
}

// ApplyTaskExecutionStrategies runs the selection and writes each verdict onto
// the scope that owns it, returning the updated graph. It is the form the plan
// path uses, so a task row is never written with a scope that lacks the
// strategy chosen for it.
func ApplyTaskExecutionStrategies(mode domain.ExecutionMode, graph TaskGraph) TaskGraph {
	placements := SelectTaskExecutionStrategies(mode, graph)
	for id, placement := range placements {
		scope, ok := graph.Scopes[id]
		if !ok {
			continue
		}
		graph.Scopes[id] = placement.applyTo(scope)
	}
	return graph
}

// applyTo stamps a placement onto a scope. A placement that made no per-task
// selection CLEARS both fields rather than leaving whatever was there: a
// project moved off smart_parallel_worktrees must stop carrying downgrades
// that its current setting can no longer produce.
func (p TaskExecutionPlacement) applyTo(scope domain.WorkflowTaskScope) domain.WorkflowTaskScope {
	if p.Downgrade == nil && !p.Mode.SmartParallel() {
		scope.ExecutionMode = ""
		scope.ExecutionModeDowngrade = nil
		return scope
	}
	scope.ExecutionMode = p.Mode
	scope.ExecutionModeDowngrade = p.Downgrade
	return scope
}

func selectTaskExecutionStrategy(id string, scope domain.WorkflowTaskScope, partners []string, project domain.ExecutionMode) TaskExecutionPlacement {
	if len(partners) > 0 {
		return TaskExecutionPlacement{
			TaskID: id,
			Mode:   domain.ExecutionIsolatedWorktree,
			Serial: true,
			Downgrade: &domain.WorkflowTaskExecutionDowngrade{
				PolicyVersion: TaskStrategyPolicyVersion,
				From:          project,
				To:            domain.ExecutionIsolatedWorktree,
				Serial:        true,
				Reason:        string(TaskStrategyReasonWriteConflict),
				Detail:        fmt.Sprintf("write set probably collides with %s, so this task runs in its own worktree and never alongside %s", joinTaskIDs(partners), pluralThem(partners)),
				Conflicts:     partners,
			},
		}
	}
	if reason, detail, uncertain := assessWriteSetUncertainty(scope); uncertain {
		return TaskExecutionPlacement{
			TaskID: id,
			Mode:   domain.ExecutionIsolatedWorktree,
			Downgrade: &domain.WorkflowTaskExecutionDowngrade{
				PolicyVersion: TaskStrategyPolicyVersion,
				From:          project,
				To:            domain.ExecutionIsolatedWorktree,
				Reason:        string(reason),
				Detail:        detail,
			},
		}
	}
	return TaskExecutionPlacement{TaskID: id, Mode: domain.ExecutionSmartParallelWorktrees}
}

// assessWriteSetUncertainty reports whether a task's write set is too vague to
// grant a concurrent worktree on.
//
// An OBSERVED write set is never uncertain: those paths came from a run that
// already happened, so there is nothing left to guess. An estimated one is
// uncertain when it names nothing at all, or when any entry is a directory --
// a directory in a write set is precisely the classifier saying "somewhere in
// here, file unknown", and two tasks pointing at the same unknown file inside
// different directories look independent right up until they are not.
func assessWriteSetUncertainty(scope domain.WorkflowTaskScope) (TaskStrategyReason, string, bool) {
	if scope.Source == domain.WorkflowTaskScopeObserved && len(scope.ObservedWritePaths) > 0 {
		return "", "", false
	}
	if len(scope.WritePaths) == 0 {
		return TaskStrategyReasonUnknownWriteSet,
			"no write path could be estimated for this task, so its independence from its siblings cannot be shown",
			true
	}
	coarse := []string{}
	for _, p := range scope.WritePaths {
		if !isFilePath(p) {
			coarse = append(coarse, p)
		}
	}
	if len(coarse) > 0 {
		sort.Strings(coarse)
		return TaskStrategyReasonCoarseWriteSet,
			fmt.Sprintf("write set is directory-scoped (%s), so the specific files are unknown and an overlap with a sibling cannot be ruled out", strings.Join(coarse, ", ")),
			true
	}
	return "", "", false
}

// conflictPartnersByTask indexes the stored pair verdicts by task, so the
// selector reads the classifier's decision instead of recomputing one.
func conflictPartnersByTask(rels []domain.WorkflowTaskRelationship) map[string][]string {
	out := map[string][]string{}
	for _, rel := range rels {
		if rel.Relation != domain.WorkflowTaskRelationWriteConflict {
			continue
		}
		out[rel.TaskID] = append(out[rel.TaskID], rel.RelatedTaskID)
		out[rel.RelatedTaskID] = append(out[rel.RelatedTaskID], rel.TaskID)
	}
	for id := range out {
		out[id] = normalizeStrings(out[id])
	}
	return out
}

func joinTaskIDs(ids []string) string {
	return strings.Join(ids, ", ")
}

func pluralThem(ids []string) string {
	if len(ids) == 1 {
		return "it"
	}
	return "them"
}
