package workflow

import (
	stdctx "context"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// AgentSwitchRequest is workflow's own minimal mirror of
// session_manager.SwitchAgentConfig (Checkpoint 8H). Workflow deliberately
// does not import session_manager: daemon wiring adapts an AgentSwitcher
// implementation backed by *session_manager.Manager.SwitchAgent, keeping the
// same layering session_manager's Spawner/MessageSender interfaces already
// use in dispatch.go/fix_dispatch.go.
type AgentSwitchRequest struct {
	TargetHarness  domain.AgentHarness
	Note           string
	IdempotencyKey string
}

// AgentSwitcher is the narrow durable-failover write path workflow reuses
// (Checkpoint 8H §2/§9): the existing, tested, generation-fenced Claude<->
// Codex agent-switching saga in session_manager/agent_switching.go. Workflow
// never implements a second switching mechanism; it only ever decides WHEN
// to call this one.
type AgentSwitcher interface {
	SwitchAgent(ctx stdctx.Context, id domain.SessionID, cfg AgentSwitchRequest) (domain.AgentSwitch, error)
}

// workFallbackHarness (Checkpoint 8P-C) picks the next harness in the
// workflow owner's own WorkerPriority list after current, restricted to
// profiles that are eligible (owned, enabled, connected, worker-capable)
// and currently capacity-eligible -- replacing 8L's fixed
// Codex<->Claude-only oppositeHarness table with an arbitrary-length,
// user-ordered walk. domain.FallbackWaitForPreferred never substitutes a
// lower-priority profile here either, matching RouteExecution's own rule.
func (c *Coordinator) workFallbackHarness(ctx stdctx.Context, run domain.WorkflowRun, current domain.AgentHarness) (domain.AgentHarness, bool) {
	owner := c.runOwner(ctx, run.ID)
	snapshot := policyForRun(run).EffectiveExecutionPolicy()
	policy, eligible, _, capacity := c.routingInputsForRole(ctx, owner, domain.WorkflowRoleWorker, snapshot)
	if policy.FallbackBehavior == domain.FallbackWaitForPreferred {
		return "", false
	}
	priority := policy.WorkerPriority
	pastCurrent := false
	for _, id := range priority {
		profile, ok := eligible[id]
		if !ok {
			continue
		}
		if !pastCurrent {
			if profile.Harness == current {
				pastCurrent = true
			}
			continue
		}
		if profile.Harness == current {
			continue
		}
		if capacityEligible(capacity[id]) {
			return profile.Harness, true
		}
	}
	return "", false
}

// effectiveMaxWorkProviderAttempts reads the policy's work-attempt budget,
// falling back to the domain default for policy snapshots created before
// Checkpoint 8H (which decode with the field at its zero value).
func effectiveMaxWorkProviderAttempts(p domain.WorkflowPolicy) int {
	if p.MaxWorkProviderAttempts > 0 {
		return p.MaxWorkProviderAttempts
	}
	return domain.DefaultWorkflowPolicy().MaxWorkProviderAttempts
}

// effectiveMaxReviewProviderAttempts mirrors effectiveMaxWorkProviderAttempts
// for the review role.
func effectiveMaxReviewProviderAttempts(p domain.WorkflowPolicy) int {
	if p.MaxReviewProviderAttempts > 0 {
		return p.MaxReviewProviderAttempts
	}
	return domain.DefaultWorkflowPolicy().MaxReviewProviderAttempts
}

// selectFallbackForWork decides whether a work-step dispatch failure should
// fail over to a different harness (Checkpoint 8H §4/§8): the failure class
// must be failover-eligible, the step must still be within its policy
// attempt budget, a fallback harness must exist for V1's fixed order, that
// fallback harness must not itself be in a durable unavailable/cooldown
// state, and — since Checkpoint 8L made workFallbackHarness bidirectional —
// the fallback harness must not already have a prior attempt recorded on
// this exact step. Without that last guard, two harnesses that are both
// durably "cooldown but no CooldownUntil" (Available()==true per 8H's own
// unknown-reset rule) would ping-pong indefinitely across duplicate failure
// reports instead of terminating after one hop.
func (c *Coordinator) selectFallbackForWork(ctx stdctx.Context, run domain.WorkflowRun, stepID string, harness domain.AgentHarness, attemptNumber int, cls ProviderFailureClassification) (domain.AgentHarness, bool) {
	if !cls.Eligible {
		return "", false
	}
	if attemptNumber >= effectiveMaxWorkProviderAttempts(policyForRun(run)) {
		return "", false
	}
	fallback, ok := c.workFallbackHarness(ctx, run, harness)
	if !ok {
		return "", false
	}
	if attempts, err := c.store.ListWorkflowAttempts(ctx, stepID); err == nil {
		for _, a := range attempts {
			if domain.AgentHarness(a.Harness) == fallback {
				return "", false
			}
		}
	}
	_, owner, profileID, _ := c.resolveRuntimeEnv(ctx, run.ID, fallback)
	if health, err := c.agentHealth(ctx, fallback, healthScope{userID: owner, profileID: profileID}); err == nil && !health.Available(c.clock()) {
		return "", false
	}
	return fallback, true
}

// failLiveWorkAttempt is the conservative terminal path for a live-session
// work-step provider failure that is not eligible for automatic failover, or
// has exhausted its budget, or whose switch attempt itself did not succeed.
// It never guesses success; it marks the attempt failed, waits the step, and
// surfaces needs_attention on the run.
func (c *Coordinator) failLiveWorkAttempt(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, attempt domain.WorkflowAttempt, cls ProviderFailureClassification, reason string, now time.Time) (domain.WorkflowStep, error) {
	if attempt.Outcome == "" {
		if err := c.store.UpdateWorkflowAttemptOutcome(ctx, attempt.ID, now, domain.WorkflowAttemptFailed, cls.Class); err != nil {
			return step, err
		}
	}
	if step.State == domain.WorkflowStepRunning {
		if _, err := c.store.UpdateWorkflowStepState(ctx, step.ID, domain.WorkflowStepRunning, domain.WorkflowStepWaiting, now); err != nil {
			return step, err
		}
		step.State = domain.WorkflowStepWaiting
	}
	if run.State == domain.WorkflowRunRunning || run.State == domain.WorkflowRunWaiting {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, now); err != nil {
			return step, err
		}
	}
	stepID := step.ID
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		NextAction:     fmt.Sprintf("needs_attention: %s (%s)", cls.Class, reason),
		DurablePhase:   "work_provider_failure_needs_attention",
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      now,
	}); err != nil {
		return step, err
	}
	if c.log != nil {
		c.log.Warn("workflow: work step provider failure needs attention", "step", step.ID, "class", cls.Class, "reason", reason)
	}
	return step, nil
}

// ReportWorkStepProviderFailure lets a signal source report that a work
// step's LIVE worker session hit a provider-level failure (Checkpoint 8H).
// Today the only caller is a test/fixture failure-injection hook exercising
// this exact production code path (classifier -> health -> budget ->
// switch); a future checkpoint may wire real mid-session Codex/Claude health
// telemetry to the same entry point. It classifies the failure, records
// agent health, and — only if eligible, within budget, and a healthy
// fallback harness exists — durably switches the session's provider via
// session_manager's existing agent-switching saga, never a second mechanism.
//
// Idempotent by construction: the switch idempotency key is derived purely
// from (stepID, attemptNumber, source harness, target harness), so
// reconciling/reporting the same failure twice — including across a daemon
// restart — resolves to AgentSwitcher.SwitchAgent's own idempotent lookup
// rather than a second switch or a second Claude launch.
func (c *Coordinator) ReportWorkStepProviderFailure(ctx stdctx.Context, runID, stepID string, cause error) (domain.WorkflowStep, error) {
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return domain.WorkflowStep{}, err
	}
	if !ok {
		return domain.WorkflowStep{}, fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	steps, err := c.store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		return domain.WorkflowStep{}, err
	}
	var step domain.WorkflowStep
	found := false
	for _, s := range steps {
		if s.ID == stepID {
			step, found = s, true
			break
		}
	}
	if !found {
		return domain.WorkflowStep{}, fmt.Errorf("%w: workflow step %q", ErrNotFound, stepID)
	}
	if step.Kind != domain.WorkflowStepWork {
		return step, fmt.Errorf("%w: provider failover only applies to work steps", ErrInvalid)
	}
	// Terminal run/step (including cancelled) beats a late-arriving failure
	// signal: cancellation must prevent failover, never race with it.
	if run.State.Terminal() || step.State.Terminal() {
		return step, nil
	}
	if step.SessionID == nil {
		return step, fmt.Errorf("%w: work step %q has no live session to fail over", ErrInvalid, stepID)
	}

	attempts, err := c.store.ListWorkflowAttempts(ctx, step.ID)
	if err != nil {
		return step, err
	}
	if len(attempts) == 0 {
		return step, fmt.Errorf("%w: work step %q has no recorded attempt", ErrInvalid, stepID)
	}
	current := attempts[len(attempts)-1]
	if current.Outcome != "" {
		// The live attempt already reached a terminal outcome by the time
		// this signal arrived (e.g. a duplicate/stale report) — never reopen
		// a finished attempt.
		return step, nil
	}
	currentHarness := domain.AgentHarness(current.Harness)
	if currentHarness == "" {
		currentHarness = domain.HarnessCodex
	}

	classification := classifyProviderFailure(cause)
	now := c.clock()
	_, curOwner, curProfileID, _ := c.resolveRuntimeEnv(ctx, run.ID, currentHarness)
	c.recordAgentHealthFailure(ctx, currentHarness, healthScope{userID: curOwner, profileID: curProfileID}, classification, now)

	fallback, eligible := c.selectFallbackForWork(ctx, run, step.ID, currentHarness, int(current.AttemptNumber), classification)
	if !eligible {
		return c.failLiveWorkAttempt(ctx, run, step, current, classification, "not eligible for automatic failover, or budget/health exhausted", now)
	}
	if c.switcher == nil {
		return c.failLiveWorkAttempt(ctx, run, step, current, classification, "no agent switcher configured", now)
	}

	idemKey := fmt.Sprintf("workflow-step-failover:%s:attempt%d:%s-to-%s", step.ID, current.AttemptNumber, currentHarness, fallback)
	note := fmt.Sprintf("workflow: work step %s provider failover (%s)", step.ID, classification.Class)
	// Checkpoint 8M §11: enrich the switch note with a compact, fact-only
	// SessionContextPack — never the source provider's transcript — reusing
	// 8H's own bounded/idempotent Note->UserNote handoff path unchanged
	// (agent_switching.go already bounds/fingerprints this exact field; no
	// second handoff mechanism is built here).
	decision := DecideSessionLifecycle(SessionLifecycleRequest{
		Role: domain.WorkflowRoleWorker, CurrentSessionID: string(*step.SessionID),
		SessionHealth: domain.SessionHealthRunning, ProviderSwitch: true, Policy: policyForRun(run),
	})
	decision.ToSessionID = string(*step.SessionID)
	var pack *domain.SessionContextPack
	if artifact, aerr := c.planArtifactForRun(ctx, run); aerr == nil {
		facts := BuildTaskCheckpointSummary(TaskCheckpointSummaryInput{Detail: RunDetail{Run: run}, Artifact: &artifact})
		built := BuildSessionContextPack(domain.WorkflowRoleWorker, facts)
		pack = &built
		decision.ContextPackHash = built.ContentHash()
		note = note + "\n\n" + RenderContextPackForRole(built)
	}
	_ = c.persistSessionLifecycleDecision(ctx, run, nil, decision, pack)

	sw, err := c.switcher.SwitchAgent(ctx, domain.SessionID(*step.SessionID), AgentSwitchRequest{
		TargetHarness:  fallback,
		Note:           note,
		IdempotencyKey: idemKey,
	})
	if err != nil {
		// SwitchAgent's own preconditions (blocked session, switch already
		// in progress, ambiguous state) rejected the switch. Never retry
		// blindly; surface needs_attention.
		return c.failLiveWorkAttempt(ctx, run, step, current, classification, "agent switch rejected: "+err.Error(), now)
	}
	if sw.State != domain.AgentSwitchCompleted {
		// Non-terminal: session_manager's own ReconcileAgentSwitches owns
		// healing this saga further on the next boot/reconcile pass.
		// Terminal-but-failed: the saga itself already recorded why; workflow
		// only needs to stop treating the old attempt as still live.
		if sw.State == domain.AgentSwitchFailed {
			return c.failLiveWorkAttempt(ctx, run, step, current, classification, "agent switch saga failed", now)
		}
		return step, nil
	}

	if err := c.store.UpdateWorkflowAttemptOutcome(ctx, current.ID, now, domain.WorkflowAttemptFailed, classification.Class); err != nil {
		return step, err
	}
	if _, err := c.store.CreateWorkflowAttempt(ctx, "wfa-"+c.newID(), step.ID, string(fallback), "", now); err != nil {
		return step, err
	}
	_, fbOwner, fbProfileID, _ := c.resolveRuntimeEnv(ctx, run.ID, fallback)
	c.recordAgentHealthSuccess(ctx, fallback, healthScope{userID: fbOwner, profileID: fbProfileID}, now)

	stepID2 := step.ID
	sid := string(*step.SessionID)
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID2,
		ProjectID:      run.ProjectID,
		SessionID:      &sid,
		NextAction:     fmt.Sprintf("provider_failover_completed: %s -> %s (%s)", currentHarness, fallback, classification.Class),
		DurablePhase:   "work_provider_failover_completed",
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      now,
	}); err != nil {
		return step, err
	}
	return step, nil
}
