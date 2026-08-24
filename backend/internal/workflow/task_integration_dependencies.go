package workflow

import (
	stdctx "context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/integration"
)

// What a task's plan says it depends on, turned into what the Integration
// Coordinator can prove.
//
// The plan already knows this. s1's classifier writes
// WorkflowTaskScope.IntegrationDependencies for every planned task -- a
// superset of the DAG's own edges, because two tasks that probably write the
// same region have to land in an order even with no edge between them -- and
// nothing read it. The scheduler used it to decide when B may START; nothing
// used it to decide when B may LAND, which is the half that actually matters
// once B is allowed to start speculatively.
//
// This file is that half. It reads the plan's answer and the master's own
// integration ledger, and hands the Coordinator a list of (task, the commit its
// integration left on the target). The Coordinator proves the second claim
// against the ref; nothing here is trusted as evidence that anything landed.

// integrationDependencies returns the siblings whose work must be on the target
// before this task's may land, each paired with the commit its own integration
// left there (empty when it has not integrated).
func (c *Coordinator) integrationDependencies(ctx stdctx.Context, parent domain.WorkflowRun, task domain.WorkflowTask) ([]integration.Dependency, error) {
	required := requiredIntegrationDependencies(task)
	if len(required) == 0 {
		return nil, nil
	}
	landed, err := c.landedTaskCommits(ctx, parent.ID)
	if err != nil {
		return nil, err
	}
	out := make([]integration.Dependency, 0, len(required))
	for _, id := range required {
		out = append(out, integration.Dependency{TaskID: id, IntegratedSHA: landed[id]})
	}
	return out, nil
}

// landedTaskCommits answers "which commit did each task's integration leave the
// target ref at", from BOTH durable records of it.
//
// Reading only the Coordinator's audit ledger was a bug, and a quiet one. That
// ledger is written by internal/integration, which every integration has gone
// through only since Task 5; the master's own promotion ledger
// (master_integration_promotion) is older, is written on every successful
// promotion whatever route it took, and is therefore the ONLY record for a task
// integrated by an earlier build. On master run wf-872e7f57 that was all seven
// completed tasks: the audit ledger was empty, every dependency looked
// un-integrated, and task 8 — the first task with an integration dependency to
// reach the new gate — could never land. Nothing was wrong and nothing was
// recorded, because a pending dependency is deliberately a silent wait.
//
// So both are folded, promotions first and audit rows second. The audit row is
// the better record where it exists (it names the strategy and both target
// SHAs, and distinguishes an attempt from a landing), so it wins; the promotion
// checkpoint is the floor that keeps history readable. Within each source the
// newest wins, because a task that was integrated, parked and integrated again
// is on the target at the commit its LAST successful attempt left.
func (c *Coordinator) landedTaskCommits(ctx stdctx.Context, masterRunID string) (map[string]string, error) {
	landed := map[string]string{}
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, masterRunID)
	if err != nil {
		return nil, err
	}
	for _, cp := range checkpoints {
		if cp.DurablePhase != masterIntegrationDurablePhase || cp.HeadSHA == "" {
			continue
		}
		var payload masterIntegrationPromotionPayload
		if json.Unmarshal([]byte(cp.RetryState), &payload) != nil || payload.TaskID == "" {
			continue
		}
		landed[payload.TaskID] = cp.HeadSHA
	}
	records, err := c.ListTaskIntegrations(ctx, masterRunID)
	if err != nil {
		return nil, err
	}
	for _, rec := range records {
		if rec.Outcome != string(integration.OutcomeIntegrated) || rec.TargetAfterSHA == "" {
			continue
		}
		landed[rec.TaskID] = rec.TargetAfterSHA
	}
	return landed, nil
}

// requiredIntegrationDependencies is the union of the two things a plan can say
// about ordering: the task's dependency edges, and the integration
// dependencies its scope records.
//
// The union rather than either alone. An edge is a functional dependency and is
// always an integration dependency too -- B builds on A's code, so B's work is
// meaningless on a target without it. A scope entry may exist with no edge at
// all, for two independent tasks the classifier expects to write the same
// region; those have no functional order but do have an integration one.
//
// A scope that cannot be parsed contributes nothing rather than failing the
// integration: the edges are still a correct, if smaller, answer, and refusing
// to integrate over a malformed JSON blob would turn a classifier bug into a
// stopped objective.
func requiredIntegrationDependencies(task domain.WorkflowTask) []string {
	seen := map[string]bool{}
	add := func(ids []string) {
		for _, id := range ids {
			id = strings.TrimSpace(id)
			if id == "" || id == task.ID {
				continue
			}
			seen[id] = true
		}
	}
	add(task.Dependencies)
	if scope, err := UnmarshalTaskScope(task.ScopeJSON); err == nil {
		add(scope.IntegrationDependencies)
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
