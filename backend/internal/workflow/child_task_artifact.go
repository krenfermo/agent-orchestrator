package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// plannedTaskArtifactFor derives the exact plan-step semantics a master
// task's child run must carry: the planner's accepted acceptance criteria and
// the task's declared write intent.
//
// The write intent is read from the task's own durable scope rather than
// re-read from the plan JSON, so a re-dispatch after a restart resolves the
// same declaration the first dispatch did. A scope that will not parse yields
// Unspecified, which is treated as mutating -- the conservative answer, and
// the same one every pre-existing task gets.
func plannedTaskArtifactFor(task domain.WorkflowTask, criteria []string) *plannedTaskArtifact {
	overlay := &plannedTaskArtifact{AcceptanceCriteria: criteria}
	if scope, err := UnmarshalTaskScope(task.ScopeJSON); err == nil {
		overlay.WriteIntent = domain.NormalizeWorkflowWriteIntent(string(scope.WriteIntent))
	}
	return overlay
}

// healPlannedTaskArtifact re-binds a child run's plan-step artifact to its
// task's real semantics when they do not already match.
//
// With CP21's fix the artifact is written inside the child's creation
// transaction, so this never fires for a child created by this code. It stays
// because two populations still exist and both are indistinguishable from a
// correct child by inspection alone: a child created before the fix, and a
// child created by a daemon that crashed inside the old two-write window. For
// both, the durable task row is the authority on what the child was supposed
// to be prompted against, and re-deriving from it invents nothing.
//
// It is idempotent by construction: it compares first and writes only on a
// real divergence, so repeated boot reconciliations after the first are
// no-ops.
func (c *Coordinator) healPlannedTaskArtifact(ctx stdctx.Context, childRunID string, task domain.WorkflowTask) error {
	var criteria []string
	if err := unmarshalJSONIfPresent(task.AcceptanceCriteriaJSON, &criteria); err != nil {
		// A criteria blob that will not parse is not something to guess at.
		// Leave the child exactly as it is rather than substituting anything.
		//nolint:nilerr // unparseable criteria heals nothing; it is not a heal failure.
		return nil
	}
	overlay := plannedTaskArtifactFor(task, criteria)

	steps, err := c.store.ListWorkflowSteps(ctx, childRunID)
	if err != nil {
		return err
	}
	var planStep *domain.WorkflowStep
	for i := range steps {
		if steps[i].Kind == domain.WorkflowStepPlan {
			planStep = &steps[i]
			break
		}
	}
	if planStep == nil {
		return fmt.Errorf("%w: child run %s has no plan step", ErrInvalid, childRunID)
	}
	artifact, err := UnmarshalPlanArtifact(planStep.ArtifactJSON)
	if err != nil {
		return err
	}
	if overlay.matches(artifact) {
		return nil
	}
	overlay.applyTo(&artifact)
	raw, err := MarshalPlanArtifact(artifact)
	if err != nil {
		return err
	}
	if _, err := c.store.UpdateWorkflowStepArtifact(ctx, planStep.ID, raw, c.clock()); err != nil {
		return err
	}
	c.recordPlanArtifactHealed(ctx, childRunID, task)
	return nil
}

// unmarshalJSONIfPresent decodes raw into v, treating an empty blob as an
// empty value rather than an error.
func unmarshalJSONIfPresent(raw string, v any) error {
	if raw == "" || raw == "null" {
		return nil
	}
	return json.Unmarshal([]byte(raw), v)
}

// recordPlanArtifactHealed appends the durable evidence that a child run's
// plan artifact was re-bound to its task's real semantics, so the repair is
// visible rather than silent. Best-effort: it records a fact, it never gates
// a transition.
func (c *Coordinator) recordPlanArtifactHealed(ctx stdctx.Context, childRunID string, task domain.WorkflowTask) {
	run, ok, err := c.store.GetWorkflowRun(ctx, childRunID)
	if err != nil || !ok {
		return
	}
	_, _ = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  childRunID,
		ProjectID:      run.ProjectID,
		NextAction:     fmt.Sprintf("plan artifact re-bound to planned task %s (%s): the child carried the generic artifact instead of the plan's criteria/write intent", task.ID, task.PlanStepID),
		DurablePhase:   "plan_artifact_healed",
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      c.clock(),
	})
}
