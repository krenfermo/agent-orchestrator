package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// execution_strategy.go — the coordinator's half of P1-A's canonical
// execution-strategy model (see domain/execution_strategy.go for the model
// itself).
//
// Three rules govern everything here:
//
//   - The strategy is written by the SAME statement that creates the run, so
//     there is no window in which a run exists without one. That is the
//     lesson CP3 already taught for the execution-policy freeze, applied
//     up front rather than after an incident.
//   - The strategy is never recomputed. Not on restart, not on resume, not
//     when the selection policy changes, not when a child is created. Every
//     reader resolves it from the run's own frozen snapshot.
//   - A run that predates the model is mapped from durable facts that really
//     exist -- whether it owns a workflow_plans row -- and the mapping is
//     recorded as `recovered`, never as a choice somebody made.

// recordedStrategy returns the selection frozen into run's policy snapshot,
// and whether one is actually there. It never falls back to a default: a
// missing selection is a fact about the run (it predates the model), and
// pretending otherwise is exactly the invented history this model exists to
// prevent.
func recordedStrategy(run domain.WorkflowRun) (domain.ExecutionStrategySelection, bool) {
	sel := policyForRun(run).Strategy
	return sel, sel.Recorded()
}

// RecordedExecutionStrategy is recordedStrategy for callers outside this
// package (the API layer), so nobody has to re-implement decoding a run's
// policy snapshot to answer "what strategy is this".
func RecordedExecutionStrategy(run domain.WorkflowRun) (domain.ExecutionStrategySelection, bool) {
	return recordedStrategy(run)
}

// LegacyExecutionStrategy is the pre-P1-A mapping as a pure function of the
// durable facts it reads, so the coordinator and the API layer cannot drift
// apart about what a legacy run is. hasPlan is whether the run owns a
// workflow_plans row.
//
// See legacyStrategyFor for why each answer is the one it is.
func LegacyExecutionStrategy(run domain.WorkflowRun, hasPlan bool, now time.Time) domain.ExecutionStrategySelection {
	sel := domain.ExecutionStrategySelection{
		Source:        domain.ExecutionStrategyRecovered,
		PolicyVersion: domain.ExecutionStrategyPolicyVersion,
		At:            now,
	}
	switch {
	case run.ParentWorkflowID != nil:
		sel.Effective = domain.ExecutionStrategyTask
		sel.Reason = domain.ExecutionStrategyReasonLegacyPlannedChild
		sel.ParentRunID = *run.ParentWorkflowID
		sel.Depth = domain.ExecutionStrategyMaxChildDepth
	case hasPlan:
		sel.Effective = domain.ExecutionStrategyAutonomous
		sel.Reason = domain.ExecutionStrategyReasonLegacyPlannedRun
	default:
		sel.Effective = domain.ExecutionStrategyTask
		sel.Reason = domain.ExecutionStrategyReasonLegacySingleTaskRun
	}
	return sel
}

// ResolveExecutionStrategy answers "which strategy is this run using" from a
// run and the one extra durable fact the mapping needs. It writes nothing.
func ResolveExecutionStrategy(run domain.WorkflowRun, hasPlan bool, now time.Time) domain.ExecutionStrategySelection {
	if sel, ok := recordedStrategy(run); ok {
		return sel
	}
	return LegacyExecutionStrategy(run, hasPlan, now)
}

// legacyStrategyFor maps a pre-P1-A run onto a strategy from the only durable
// facts that survive: whether the run owns a plan row (it was created through
// the objective/planner path) and whether it is somebody's planned child.
//
// The mapping is deterministic and total, so a legacy run never fails and
// never has to guess:
//
//   - a planned child run -> task, the bounded leaf it has always been;
//   - a run with a plan row -> autonomous, the planner-driven objective it has
//     always been. NOT master: master is the strategy a person picks for a
//     large initiative, and no legacy row records that anybody did;
//   - anything else -> task, the single bounded chain it has always been.
//
// The point of the mapping is that it changes no behaviour whatsoever. Each
// answer names what the run already does.
func (c *Coordinator) legacyStrategyFor(ctx stdctx.Context, run domain.WorkflowRun) domain.ExecutionStrategySelection {
	hasPlan := false
	if c.planStore != nil && run.ParentWorkflowID == nil {
		if _, isMaster, err := c.planStore.GetWorkflowPlan(ctx, run.ID); err == nil && isMaster {
			hasPlan = true
		}
	}
	return LegacyExecutionStrategy(run, hasPlan, c.clock())
}

// EffectiveStrategy answers "which strategy is this run using" for any
// lifecycle component, on any run, at any time. It reads the frozen selection
// when there is one and maps a legacy run when there is not. It writes
// nothing -- healing is ensureRecordedExecutionStrategy's job, on the
// recovery path, so a read can never mutate a run.
func (c *Coordinator) EffectiveStrategy(ctx stdctx.Context, runID string) (domain.ExecutionStrategySelection, error) {
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return domain.ExecutionStrategySelection{}, err
	}
	if !ok {
		return domain.ExecutionStrategySelection{}, fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	return c.strategyForRun(ctx, run), nil
}

// strategyForRun is EffectiveStrategy for a run already in hand.
func (c *Coordinator) strategyForRun(ctx stdctx.Context, run domain.WorkflowRun) domain.ExecutionStrategySelection {
	if sel, ok := recordedStrategy(run); ok {
		return sel
	}
	return c.legacyStrategyFor(ctx, run)
}

// ensureRecordedExecutionStrategy is the legacy half: a run whose snapshot
// carries no selection gets the mapped one written down, once, as
// `recovered`. After that the run is indistinguishable from any other for
// every reader, and the mapping can never drift because it is no longer
// re-derived.
//
// It is deliberately idempotent and deliberately non-fatal in spirit: it only
// ever ADDS a record of what the run already was. A run that already has a
// selection is untouched -- which is what makes "restart must not select a
// different strategy" true by construction rather than by convention.
func (c *Coordinator) ensureRecordedExecutionStrategy(ctx stdctx.Context, run domain.WorkflowRun) (domain.WorkflowRun, error) {
	if _, ok := recordedStrategy(run); ok {
		return run, nil
	}
	// Re-read before writing. The caller's copy of the run comes from a list
	// taken at the top of a whole reconciliation pass, and the policy snapshot
	// is one column mutated by several healers (execution-policy freeze, child
	// inheritance) during that same pass. Marshalling a stale snapshot back
	// would silently undo whichever of them ran first -- exactly the
	// lost-update that would leave a master child unable to prove it inherited
	// its parent's policy.
	fresh, ok, err := c.store.GetWorkflowRun(ctx, run.ID)
	if err != nil || !ok {
		return run, err
	}
	run = fresh
	if _, done := recordedStrategy(run); done {
		return run, nil
	}
	policy := policyForRun(run)
	policy.Strategy = c.legacyStrategyFor(ctx, run)
	snapshotJSON, err := json.Marshal(policy)
	if err != nil {
		return run, err
	}
	if _, err := c.store.UpdateWorkflowRunPolicySnapshot(ctx, run.ID, string(snapshotJSON), c.clock()); err != nil {
		return run, err
	}
	refreshed, ok, err := c.store.GetWorkflowRun(ctx, run.ID)
	if err != nil || !ok {
		return run, err
	}
	return refreshed, nil
}

// withStrategy stamps sel into policy, filling in the policy version so no
// caller can record a decision without saying which rules produced it.
func withStrategy(policy domain.WorkflowPolicy, sel domain.ExecutionStrategySelection) domain.WorkflowPolicy {
	if sel.PolicyVersion == "" {
		sel.PolicyVersion = domain.ExecutionStrategyPolicyVersion
	}
	policy.Strategy = sel
	return policy
}

// defaultTaskStrategy is what CreateRun records when a caller uses the
// pre-P1-A entry point: this run is a single bounded chain, which is a task,
// selected by policy rather than chosen by anybody.
func (c *Coordinator) defaultTaskStrategy() domain.ExecutionStrategySelection {
	return domain.ExecutionStrategySelection{
		Effective:     domain.ExecutionStrategyTask,
		Source:        domain.ExecutionStrategyPolicy,
		PolicyVersion: domain.ExecutionStrategyPolicyVersion,
		Reason:        domain.ExecutionStrategyReasonBoundedWork,
		At:            c.clock(),
	}
}

// defaultObjectiveStrategy is CreateObjectiveRun's equivalent: a
// planner-driven objective is autonomous unless the caller said master.
func (c *Coordinator) defaultObjectiveStrategy() domain.ExecutionStrategySelection {
	return domain.ExecutionStrategySelection{
		Effective:     domain.ExecutionStrategyAutonomous,
		Source:        domain.ExecutionStrategyPolicy,
		PolicyVersion: domain.ExecutionStrategyPolicyVersion,
		Reason:        domain.ExecutionStrategyReasonMultiStepProject,
		At:            c.clock(),
	}
}

// requirePlannedStrategy is the durable enforcement that makes `task` mean
// something: a run frozen as a task may never be pushed through the master
// planner, no matter what a later caller, a stale writer or a resumed wake
// asks for. The refusal survives restart because the strategy does.
//
// Legacy runs are mapped first, so a pre-P1-A objective (autonomous) still
// plans and a pre-P1-A single-task chain still cannot be turned into one.
func (c *Coordinator) requirePlannedStrategy(ctx stdctx.Context, run domain.WorkflowRun) error {
	sel := c.strategyForRun(ctx, run)
	if sel.Effective.Planned() {
		return nil
	}
	return fmt.Errorf("%w: workflow run %s executes under the %q strategy and has no plan to generate",
		ErrInvalid, run.ID, sel.Effective)
}
