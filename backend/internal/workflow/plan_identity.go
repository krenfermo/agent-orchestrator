package workflow

import (
	stdctx "context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// canonicalTaskID derives a planned task's row id from its own natural key —
// the pair `(workflow_run_id, plan_step_id)` that `workflow_tasks` already
// declares UNIQUE (0101_workflow_master_plan.sql).
//
// This is the fix for CP9(b) in docs/worker-lifecycle-audit.md. Before it,
// `finalizeGeneratedPlan` minted a fresh random id per task on every pass. A
// crash between the task insert (P10) and the relationship insert (P11) left
// boot recovery replaying the finalize with *different* ids: `INSERT OR
// IGNORE` lost every task row to the unique key and silently dropped it, and
// the FK-bound relationship insert that followed then named task ids that did
// not exist. With `foreign_keys(ON)` that insert fails, the error propagates
// to `parkUnreconcilableRun`, and every subsequent boot reproduces it — a
// permanently stuck objective.
//
// Deriving the id from the natural key makes the replay compute the exact
// same ids it computed the first time, so the `INSERT OR IGNORE` is a true
// no-op and the relationship rows reference live tasks. It is pure: no clock,
// no randomness, no IO, so N replays produce one identity.
func canonicalTaskID(workflowRunID, planStepID string) string {
	sum := sha256.Sum256([]byte("workflow-task\x00" + workflowRunID + "\x00" + planStepID))
	return "wft-" + hex.EncodeToString(sum[:])[:24]
}

// verifyRelationshipEndpoints proves, against the durable task rows, that
// every pair verdict about to be written names two tasks that exist.
//
// It exists because `workflow_task_relationships` is FK-bound to
// `workflow_tasks` under `foreign_keys(ON)`: an endpoint that does not
// resolve turns into a raw constraint failure inside a transaction, which
// propagates out of finalizeGeneratedPlan into parkUnreconcilableRun and is
// reproduced identically by every later boot. Refusing here fails closed the
// same way, but says what is wrong instead of leaving a SQLite error as the
// only evidence.
func (c *Coordinator) verifyRelationshipEndpoints(ctx stdctx.Context, runID string, rels []domain.WorkflowTaskRelationship) error {
	if len(rels) == 0 {
		return nil
	}
	tasks, err := c.planStore.ListWorkflowTasks(ctx, runID)
	if err != nil {
		return err
	}
	live := make(map[string]struct{}, len(tasks))
	for _, t := range tasks {
		live[t.ID] = struct{}{}
	}
	for _, rel := range rels {
		if _, ok := live[rel.TaskID]; !ok {
			return fmt.Errorf("%w: task relationship references unpersisted task %s in run %s", ErrInvalid, rel.TaskID, runID)
		}
		if _, ok := live[rel.RelatedTaskID]; !ok {
			return fmt.Errorf("%w: task relationship references unpersisted task %s in run %s", ErrInvalid, rel.RelatedTaskID, runID)
		}
	}
	return nil
}
