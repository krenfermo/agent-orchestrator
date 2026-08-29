package workflow

import (
	"strconv"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// plan_revision.go — P1-B §D's durable plan generation, in the two derived
// values that keep it off the existing UNIQUE constraints.
//
// workflow_tasks carries UNIQUE(workflow_run_id, plan_step_id) and
// UNIQUE(workflow_run_id, ordinal) from migration 0101. Widening either one
// means rebuilding a table that three ON DELETE CASCADE children reference,
// and migrations 0119 and 0130 both learned against a real ~/.ao/data/ao.db
// what that costs when it goes wrong.
//
// So a later revision's rows are made distinct BY CONSTRUCTION instead: its
// plan step ids are namespaced and its ordinals are offset past every ordinal
// revision N-1 could have produced. Revision 1 -- every row already on disk --
// keeps its exact historical spelling, so canonicalTaskID computes the same
// identity it always did and CP9(b)'s replay guarantee is untouched.

// planRevisionOf reads a plan record's revision, treating the zero a
// pre-0139 store returns as revision 1.
func planRevisionOf(record domain.WorkflowPlanRecord) int64 {
	if record.Revision <= 0 {
		return 1
	}
	return record.Revision
}

// taskRevisionOf reads a task row's revision the same way.
func taskRevisionOf(task domain.WorkflowTask) int64 {
	if task.PlanRevision <= 0 {
		return 1
	}
	return task.PlanRevision
}

// planStepIDForRevision namespaces a planner step id to its revision.
//
// Revision 1 is returned unchanged. That is not a convenience: it is what
// makes this a pure ADD COLUMN migration, because every existing task row's
// plan_step_id already reads as revision 1's spelling and every existing
// canonicalTaskID already hashes it.
func planStepIDForRevision(revision int64, planStepID string) string {
	if revision <= 1 {
		return planStepID
	}
	return "r" + strconv.FormatInt(revision, 10) + ":" + planStepID
}

// ordinalForRevision offsets a revision's ordinals past every ordinal an
// earlier revision could have used. Ordering within a revision is preserved
// exactly, and no reader sees the offset: ListWorkflowTasks returns one
// revision at a time and orders by this same column.
func ordinalForRevision(revision int64, index int) int64 {
	if revision <= 1 {
		return int64(index + 1)
	}
	return (revision-1)*int64(MaxPlanSteps) + int64(index+1)
}

// canonicalTaskIDAtRevision is canonicalTaskID for one plan revision. It needs
// no new hashing rule: the revision is already carried by the namespaced step
// id, so revision 1 hashes byte-identically to what it always did and every
// later revision is distinct.
func canonicalTaskIDAtRevision(workflowRunID string, revision int64, planStepID string) string {
	return canonicalTaskID(workflowRunID, planStepIDForRevision(revision, planStepID))
}
