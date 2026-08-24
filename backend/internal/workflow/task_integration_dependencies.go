package workflow

import (
	stdctx "context"
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
	records, err := c.ListTaskIntegrations(ctx, parent.ID)
	if err != nil {
		return nil, err
	}
	// Newest-integrated wins: a task that was integrated, parked and integrated
	// again has more than one row, and the commit that is on the target is the
	// one the last successful attempt left.
	landed := map[string]string{}
	for _, rec := range records {
		if rec.Outcome != string(integration.OutcomeIntegrated) || rec.TargetAfterSHA == "" {
			continue
		}
		landed[rec.TaskID] = rec.TargetAfterSHA
	}
	out := make([]integration.Dependency, 0, len(required))
	for _, id := range required {
		out = append(out, integration.Dependency{TaskID: id, IntegratedSHA: landed[id]})
	}
	return out, nil
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
