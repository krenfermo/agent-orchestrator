package workflow

import (
	stdctx "context"
	"encoding/json"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// routingDecisionDurablePhase is the checkpoint DurablePhase Checkpoint 8L
// adds. Routing decisions are persisted the exact same way Checkpoint 8I's
// ReviewPolicyDecision already is (review_policy_dispatch.go's
// persistReviewPolicyDecision) — a checkpoint row, not a second migration/
// table — so a step's full decision trail (routing, then review policy,
// then review, then verify) reads as one ordered checkpoint stream
// (checkpoint brief §14: "Use checkpoint/artifact pattern if appropriate").
const routingDecisionDurablePhase = "routing_decision"

// persistRoutingDecision durably records a domain.RoutingDecision so a work/
// review/planner/decision-resolver attempt can always explain later exactly
// which policy version, complexity estimate and capacity snapshot produced
// its provider choice (checkpoint brief §14/§15) — never recomputed against
// a future policy version.
func (c *Coordinator) persistRoutingDecision(ctx stdctx.Context, run domain.WorkflowRun, stepID *string, decision domain.RoutingDecision) error {
	payload, err := json.Marshal(decision)
	if err != nil {
		return err
	}
	nextAction := "routing_selected: " + string(decision.SelectedHarness)
	if decision.Waiting {
		nextAction = "waiting_for_capacity: role=" + string(decision.Role)
	}
	_, err = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: stepID,
		ProjectID:      run.ProjectID,
		RetryState:     string(payload),
		NextAction:     nextAction,
		DurablePhase:   routingDecisionDurablePhase,
		PayloadVersion: domain.RoutingPolicyVersion,
		CreatedAt:      c.clock(),
	})
	return err
}

// decodeRoutingDecision unmarshals a routing_decision checkpoint's
// RetryState back into a domain.RoutingDecision. Returns ok=false on any
// unmarshal error rather than guessing (mirrors decodeReviewPolicyDecision).
func decodeRoutingDecision(retryState string) (domain.RoutingDecision, bool) {
	var decision domain.RoutingDecision
	if retryState == "" {
		return decision, false
	}
	if err := json.Unmarshal([]byte(retryState), &decision); err != nil {
		return decision, false
	}
	return decision, decision.Role != ""
}

// DecodeRoutingDecisionForTest exposes decodeRoutingDecision to the external
// workflow_test package (mirrors DecodeReviewPolicyDecisionForTest).
func DecodeRoutingDecisionForTest(retryState string) (domain.RoutingDecision, bool) {
	return decodeRoutingDecision(retryState)
}

// routingDecisionForStep reads back the latest routing_decision checkpoint
// for a step, mirroring reviewPolicyDecisionForStep's own read pattern
// (verify_scope_policy.go) exactly — a step's routing choice is surfaced to
// callers (GetRun's StepDetail, usage telemetry) the same read-time way
// every other durable decision in this package already is.
func (c *Coordinator) routingDecisionForStep(ctx stdctx.Context, runID, stepID string) (domain.RoutingDecision, bool) {
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return domain.RoutingDecision{}, false
	}
	var latest *domain.WorkflowCheckpoint
	for i := range checkpoints {
		cp := &checkpoints[i]
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID || cp.DurablePhase != routingDecisionDurablePhase {
			continue
		}
		if latest == nil || cp.CreatedAt.After(latest.CreatedAt) {
			latest = cp
		}
	}
	if latest == nil {
		return domain.RoutingDecision{}, false
	}
	return decodeRoutingDecision(latest.RetryState)
}

// reviewerHarnessFromAgentHarness bridges ExecutionRouter's AgentHarness
// vocabulary (worker/routing) to domain.ReviewerHarness (the reviewer
// registry's own vocabulary, ports.ReviewerResolver). The two vocabularies
// share ids for every harness that serves both roles (Checkpoint 8P-C: no
// longer a fixed two-entry table), falling back to
// domain.FallbackReviewerHarness only for a selected harness the reviewer
// registry doesn't recognize at all -- never a zero value.
func reviewerHarnessFromAgentHarness(h domain.AgentHarness) domain.ReviewerHarness {
	rh := domain.ReviewerHarness(h)
	if rh.IsKnown() {
		return rh
	}
	return domain.FallbackReviewerHarness
}

// routeWorkerDispatch is dispatch.go's single entry point into
// ExecutionRouter for the initial worker-harness choice (checkpoint brief
// §7). It estimates complexity from the step's own PlanArtifact (pre-dispatch,
// see worker_routing.go), snapshots capacity from the same agentHealth
// source 8H/8J already use, evaluates the pure RouteExecution function, and
// persists the decision as a checkpoint before returning the selected
// harness. Waiting=true means "no eligible worker right now" — the caller
// must not spawn and must not treat this as a failure.
func (c *Coordinator) routeWorkerDispatch(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, priorAttempts int) (domain.RoutingDecision, error) {
	artifact, err := c.planArtifactForRun(ctx, run)
	if err != nil {
		artifact = BuildPlanArtifact(run.ProjectID, run.Objective, run.PolicyVersion)
	}
	complexity := EstimateWorkerComplexity(artifact, priorAttempts)
	owner := c.runOwner(ctx, run.ID)
	snapshot := policyForRun(run).EffectiveExecutionPolicy()
	policy, eligible, ineligible, capacity := c.routingInputsForRole(ctx, owner, domain.WorkflowRoleWorker, snapshot)

	decision := RouteExecution(RoutingRequest{
		Role:              domain.WorkflowRoleWorker,
		Complexity:        complexity,
		Policy:            policy,
		EligibleProfiles:  eligible,
		IneligibleReasons: ineligible,
		Capacity:          capacity,
	})
	stepID := step.ID
	if perr := c.persistRoutingDecision(ctx, run, &stepID, decision); perr != nil {
		return decision, perr
	}
	return decision, nil
}

// reviewerHarnessForStep resolves the reviewer harness for a review step's
// session (checkpoint brief §8/§9). Once decided for a session's first
// review cycle it is reused stably across every later fix/re-review cycle
// (reviewer independence must not flip mid-step) — read back from any
// existing review_run row for the session rather than re-routed, so a
// capacity change between cycle 1 and cycle 2 can never silently swap the
// reviewer. Only the very first cycle (no review_run rows yet) calls
// ExecutionRouter fresh. ok=false means "wait for capacity," never a
// failure — the caller must not dispatch and must not treat this as an
// error.
func (c *Coordinator) reviewerHarnessForStep(ctx stdctx.Context, run domain.WorkflowRun, workStep, reviewStep domain.WorkflowStep, sessionID domain.SessionID, workCP domain.WorkflowCheckpoint) (domain.ReviewerHarness, bool, error) {
	if c.reviewRuns != nil {
		runs, err := c.reviewRuns.ListReviewRunsBySession(ctx, sessionID)
		if err != nil {
			return "", false, err
		}
		if len(runs) > 0 {
			return runs[0].Harness, true, nil
		}
	}

	// The LAST recorded attempt row is always the current/relevant
	// implementer, whether it is still live (Outcome=="") or already
	// observed complete (Outcome=="succeeded") — a failed attempt is always
	// followed by a new row for the harness that actually took over (see
	// failover.go's ReportWorkStepProviderFailure, which reads attempts the
	// exact same way), never overwritten in place.
	implementer := domain.HarnessCodex
	if attempts, err := c.store.ListWorkflowAttempts(ctx, workStep.ID); err == nil && len(attempts) > 0 {
		if h := attempts[len(attempts)-1].Harness; h != "" {
			implementer = domain.AgentHarness(h)
		}
	}
	artifact, err := c.planArtifactForRun(ctx, run)
	if err != nil {
		return "", false, err
	}
	facts, err := c.computeReviewRiskFacts(ctx, run, workStep, artifact, workCP)
	if err != nil {
		return "", false, err
	}
	complexity := EvaluateReviewPolicy(facts).Complexity

	decision, err := c.routeReviewerDispatch(ctx, run, reviewStep, implementer, complexity)
	if err != nil {
		return "", false, err
	}
	if decision.Waiting {
		return "", false, nil
	}
	return reviewerHarnessFromAgentHarness(decision.SelectedHarness), true, nil
}

// routeReviewerDispatch is review_dispatch.go's entry point into
// ExecutionRouter for reviewer selection (checkpoint brief §8/§9):
// cross-provider independent from the worker's actual implementing harness.
func (c *Coordinator) routeReviewerDispatch(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, implementer domain.AgentHarness, complexity TaskComplexity) (domain.RoutingDecision, error) {
	owner := c.runOwner(ctx, run.ID)
	snapshot := policyForRun(run).EffectiveExecutionPolicy()
	policy, eligible, ineligible, capacity := c.routingInputsForRole(ctx, owner, domain.WorkflowRoleReviewer, snapshot)

	decision := RouteExecution(RoutingRequest{
		Role:                       domain.WorkflowRoleReviewer,
		Complexity:                 complexity,
		CurrentImplementerProvider: domain.ProviderForHarness(implementer),
		Policy:                     policy,
		EligibleProfiles:           eligible,
		IneligibleReasons:          ineligible,
		Capacity:                   capacity,
	})
	stepID := step.ID
	if err := c.persistRoutingDecision(ctx, run, &stepID, decision); err != nil {
		return decision, err
	}
	return decision, nil
}
