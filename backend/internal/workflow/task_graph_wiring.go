package workflow

import (
	stdctx "context"
	"encoding/json"
	"path"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// repoRootsFromContextManifest reads the repository-structure signal out of
// the planner context manifest already persisted with the plan
// (workflow_plans.context_manifest_json). The manifest lists the documents the
// planner was shown by repository-relative path, so their directories are
// directories the repository demonstrably has -- a fact, not a guess, and one
// that costs no extra IO because it was captured when the planner ran.
//
// Returns nil for a manifest that is empty, unparseable, or names no
// directories; the classifier is fine with no roots and simply admits fewer
// ambiguous two-segment tokens.
func repoRootsFromContextManifest(raw string) []string {
	if strings.TrimSpace(raw) == "" || raw == "{}" {
		return nil
	}
	var manifest PlannerContext
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		return nil
	}
	roots := map[string]bool{}
	for _, doc := range manifest.Documents {
		p := strings.TrimPrefix(strings.TrimSpace(doc.Path), "./")
		if i := strings.Index(p, "/"); i > 0 {
			roots[p[:i]] = true
		}
	}
	return sortedKeys(roots)
}

// taskScopeInputs joins the accepted plan's steps to the task rows they became,
// so the classifier speaks in task ids (the durable identity everything else
// uses) while still seeing the plan's own text and scope declarations.
//
// observed, when non-nil, supplies the paths a task's execution actually wrote,
// keyed by task id. It is empty at plan time and populated on re-classification
// after a task completes.
func taskScopeInputs(plan MasterPlan, tasks []domain.WorkflowTask, idByPlan map[string]string, observed map[string][]string) []TaskScopeInput {
	ordinalByID := make(map[string]int64, len(tasks))
	depsByID := make(map[string][]string, len(tasks))
	for _, t := range tasks {
		ordinalByID[t.ID] = t.Ordinal
		depsByID[t.ID] = t.Dependencies
	}
	out := make([]TaskScopeInput, 0, len(plan.Steps))
	for _, s := range plan.Steps {
		id, ok := idByPlan[s.ID]
		if !ok {
			continue
		}
		out = append(out, TaskScopeInput{
			TaskID:             id,
			PlanStepID:         s.ID,
			Ordinal:            ordinalByID[id],
			Title:              s.Title,
			Description:        s.Description,
			AcceptanceCriteria: s.AcceptanceCriteria,
			Verify:             s.Verify,
			Dependencies:       depsByID[id],
			DeclaredFiles:      s.Files,
			DeclaredPackages:   s.Packages,
			ObservedWritePaths: observed[id],
			SafeWriteOverlaps:  safeOverlapsForStep(s, idByPlan),
		})
	}
	return out
}

// safeOverlapsForStep resolves a step's waivers from plan step ids into the
// task ids everything downstream speaks in. A waiver naming a step that did
// not become a task is dropped: it can no longer refer to anything, and a
// waiver that resolves to nothing must not silently widen into one that
// waives everything.
func safeOverlapsForStep(s PlannedStep, idByPlan map[string]string) []domain.WorkflowTaskSafeOverlap {
	if len(s.SafeWriteOverlaps) == 0 {
		return nil
	}
	out := make([]domain.WorkflowTaskSafeOverlap, 0, len(s.SafeWriteOverlaps))
	for _, w := range s.SafeWriteOverlaps {
		with, ok := idByPlan[w.With]
		if !ok {
			continue
		}
		out = append(out, domain.WorkflowTaskSafeOverlap{WithTaskID: with, Paths: w.Paths, Reason: w.Reason})
	}
	return out
}

// recordObservedTaskWriteSet replaces a completed task's ESTIMATED write set
// with what its execution actually wrote, then re-classifies the plan's pairs
// against the corrected picture.
//
// This is the "prior task outputs" input to the classifier, and it is the only
// one that is not a guess: the paths come from the child run's own verification
// record, which already observed the worktree. A plan's later tasks are
// therefore scheduled against evidence for the tasks that have run and against
// estimates only for the ones that have not.
//
// Best-effort by design. A missing or scope-less verify result simply leaves
// the estimate in place, and no failure here may block a task that has already
// passed review, verification, and integration from being marked complete.
func (c *Coordinator) recordObservedTaskWriteSet(ctx stdctx.Context, task domain.WorkflowTask, childRunID string) {
	if c.planStore == nil {
		return
	}
	result, ok, err := c.latestVerifyResult(ctx, childRunID)
	if err != nil || !ok || result.Scope == nil || len(result.Scope.ChangedFiles) == 0 {
		return
	}
	scope, err := UnmarshalTaskScope(task.ScopeJSON)
	if err != nil {
		return
	}
	observed := normalizeStrings(result.Scope.ChangedFiles)
	if len(observed) == 0 {
		return
	}
	scope.Version = TaskGraphPolicyVersion
	scope.Source = domain.WorkflowTaskScopeObserved
	scope.ObservedWritePaths = observed
	// The observed set REPLACES the estimated write set rather than joining it:
	// an estimate the run has already disproved is not evidence, and leaving a
	// guessed path in the write set would keep reporting conflicts against a
	// file this task demonstrably never touched.
	scope.WritePaths = observed
	scope.ReadPaths = subtractStrings(scope.ReadPaths, observed)
	scope.Files, scope.Packages, scope.Components = derivePathFacets(append(append([]string{}, scope.WritePaths...), scope.ReadPaths...), nil)

	raw, err := MarshalTaskScope(scope)
	if err != nil {
		return
	}
	if _, err := c.planStore.UpdateWorkflowTaskScope(ctx, task.ID, raw, c.clock()); err != nil {
		return
	}
	c.reclassifyTaskRelationships(ctx, task.WorkflowRunID)
}

// reclassifyTaskRelationships re-labels every task pair of a plan from the
// write sets currently persisted on the task rows, and writes back both the
// pair verdicts and the per-task scheduling facts they imply.
//
// It deliberately does NOT re-estimate any scope from plan text: the plan text
// has not changed, and re-deriving it would throw away an observed write set
// that a completed task earned. Best-effort for the same reason
// recordObservedTaskWriteSet is.
func (c *Coordinator) reclassifyTaskRelationships(ctx stdctx.Context, runID string) {
	if c.planStore == nil {
		return
	}
	tasks, err := c.planStore.ListWorkflowTasks(ctx, runID)
	if err != nil || len(tasks) < 2 {
		return
	}
	scopes := make(map[string]domain.WorkflowTaskScope, len(tasks))
	pairs := make([]TaskRelationInput, 0, len(tasks))
	for _, t := range tasks {
		scope, err := UnmarshalTaskScope(t.ScopeJSON)
		if err != nil {
			return
		}
		scopes[t.ID] = scope
		pairs = append(pairs, TaskRelationInput{TaskID: t.ID, Ordinal: t.Ordinal, Dependencies: t.Dependencies, WritePaths: scope.WritePaths, SafeWriteOverlaps: scope.SafeWriteOverlaps})
	}
	rels, scheduling := ClassifyTaskRelations(runID, pairs)
	now := c.clock()
	for i := range rels {
		rels[i].CreatedAt = now
	}
	if err := c.planStore.ReplaceWorkflowTaskRelationships(ctx, rels); err != nil {
		return
	}
	// Re-select the execution strategies too, against the corrected picture: a
	// write set the run has now OBSERVED can reveal a conflict the estimate
	// missed -- which must downgrade both sides -- or clear one it invented,
	// which must let them back up. Leaving the plan-time verdict in place would
	// keep scheduling later tasks against text that evidence has since
	// replaced. For any project except a smart-parallel one this changes
	// nothing, exactly as it changes nothing at plan time.
	updated := TaskGraph{Scopes: make(map[string]domain.WorkflowTaskScope, len(tasks)), Relationships: rels}
	for id, scope := range scopes {
		if sched, ok := scheduling[id]; ok {
			scope.ExecutionStrategy = sched.ExecutionStrategy
			scope.IntegrationDependencies = sched.IntegrationDependencies
		}
		updated.Scopes[id] = scope
	}
	updated = ApplyTaskExecutionStrategies(c.runExecutionMode(ctx, runID), updated)
	for _, t := range tasks {
		next, ok := updated.Scopes[t.ID]
		if !ok {
			continue
		}
		raw, err := MarshalTaskScope(next)
		if err != nil {
			continue
		}
		if before, err := MarshalTaskScope(scopes[t.ID]); err == nil && before == raw {
			continue
		}
		_, _ = c.planStore.UpdateWorkflowTaskScope(ctx, t.ID, raw, now)
	}
}

// projectExecutionMode resolves a project's configured execution mode for the
// strategy selector. It is best-effort on purpose: a project that cannot be
// read must fall back to the default rather than fail a plan, and the default
// selects nothing, so an unreadable project can never accidentally grant
// concurrent worktrees.
func (c *Coordinator) projectExecutionMode(ctx stdctx.Context, projectID string) domain.ExecutionMode {
	if c.projects == nil || projectID == "" {
		return domain.ExecutionIsolatedWorktree
	}
	project, ok, err := c.projects.GetProject(ctx, projectID)
	if err != nil || !ok {
		return domain.ExecutionIsolatedWorktree
	}
	return domain.ResolveExecutionMode(project.Kind, project.Config)
}

// runExecutionMode is projectExecutionMode for a caller that only holds a run
// id, which is the case on the re-classification path.
func (c *Coordinator) runExecutionMode(ctx stdctx.Context, runID string) domain.ExecutionMode {
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil || !ok {
		return domain.ExecutionIsolatedWorktree
	}
	return c.projectExecutionMode(ctx, run.ProjectID)
}

// derivePathFacets splits a path set into the explicit files, the packages
// (directories) and the coarse components it covers -- the same derivation
// estimateTaskScope does, reused when a scope is rebuilt from observed paths.
func derivePathFacets(paths, roots []string) (files, packages, components []string) {
	fileSet, pkgSet, componentSet := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, p := range paths {
		if isFilePath(p) {
			fileSet[p] = true
			if d := path.Dir(p); d != "." && d != "/" {
				pkgSet[d] = true
			}
		} else {
			pkgSet[p] = true
		}
		if cmp := componentFor(p, roots); cmp != "" {
			componentSet[cmp] = true
		}
	}
	return sortedKeys(fileSet), sortedKeys(pkgSet), sortedKeys(componentSet)
}

func subtractStrings(in, remove []string) []string {
	drop := map[string]bool{}
	for _, r := range remove {
		drop[r] = true
	}
	out := []string{}
	for _, s := range in {
		if !drop[s] {
			out = append(out, s)
		}
	}
	return out
}

// TaskGraphSnapshot is a master run's persisted task-graph, as scheduling and
// integration read it: one scope per task and one verdict per task pair,
// exactly as the classifier last decided them. Nothing here is recomputed --
// that is the whole point of persisting the decision.
type TaskGraphSnapshot struct {
	// Scopes is keyed by task id. A task planned before the scope model
	// existed has the zero scope, so callers must tolerate empty write sets.
	Scopes map[string]domain.WorkflowTaskScope
	// Relationships holds one entry per unordered pair, canonically ordered
	// (TaskID < RelatedTaskID). Use RelationFor to look a pair up without
	// having to know which way round it was stored.
	Relationships []domain.WorkflowTaskRelationship
}

// LoadTaskGraph reads a run's persisted scopes and pair verdicts.
func (c *Coordinator) LoadTaskGraph(ctx stdctx.Context, runID string) (TaskGraphSnapshot, error) {
	out := TaskGraphSnapshot{Scopes: map[string]domain.WorkflowTaskScope{}, Relationships: []domain.WorkflowTaskRelationship{}}
	if c.planStore == nil {
		return out, nil
	}
	tasks, err := c.planStore.ListWorkflowTasks(ctx, runID)
	if err != nil {
		return TaskGraphSnapshot{}, err
	}
	for _, t := range tasks {
		scope, err := UnmarshalTaskScope(t.ScopeJSON)
		if err != nil {
			return TaskGraphSnapshot{}, err
		}
		out.Scopes[t.ID] = scope
	}
	rels, err := c.planStore.ListWorkflowTaskRelationships(ctx, runID)
	if err != nil {
		return TaskGraphSnapshot{}, err
	}
	out.Relationships = rels
	return out, nil
}

// RelationFor returns the stored verdict for one pair in either order.
func (g TaskGraphSnapshot) RelationFor(a, b string) (domain.WorkflowTaskRelationship, bool) {
	if a > b {
		a, b = b, a
	}
	for _, rel := range g.Relationships {
		if rel.TaskID == a && rel.RelatedTaskID == b {
			return rel, true
		}
	}
	return domain.WorkflowTaskRelationship{}, false
}

// ConflictsFor returns the task ids a given task probably write-conflicts
// with -- the set a scheduler must not run it alongside.
func (g TaskGraphSnapshot) ConflictsFor(taskID string) []string {
	out := []string{}
	for _, rel := range g.Relationships {
		if rel.Relation != domain.WorkflowTaskRelationWriteConflict {
			continue
		}
		switch taskID {
		case rel.TaskID:
			out = append(out, rel.RelatedTaskID)
		case rel.RelatedTaskID:
			out = append(out, rel.TaskID)
		}
	}
	return normalizeStrings(out)
}
