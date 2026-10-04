package workflow

import (
	stdctx "context"
	"fmt"
	"strconv"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
)

// incident_launch.go — Checkpoint 8P-E.19, the production launch path for the
// Incident Advisor's Diagnostic Agent.
//
// Three things this file exists to get right, none of which the fake launcher
// in the tests could have forced:
//
//  1. "The launcher ran" is not "a diagnosis arrived". A pane that started and
//     an answer that came back are different facts, and conflating them is how
//     an incident would sit in `diagnosing` forever after an agent died.
//     incidentDiagnosisTimeout bounds the wait, and only a real submission
//     moves the incident on.
//
//  2. Exactly one live launch per incident generation, whatever happens — a
//     2s poll, a double click, a daemon restart mid-launch. The guard is the
//     outbox's UNIQUE(idempotency_key), the same single-flight primitive every
//     other dispatch in AO uses, keyed by incident and generation rather than
//     by step and cycle.
//
//  3. No second provider-selection system. The harness comes from
//     RouteExecution through the same routingInputsForRole path reviewer and
//     decision-resolver dispatch already use, so health, capacity, profile
//     eligibility and the user's own priority list all apply unchanged, and a
//     capacity shortage produces the ordinary self-remediable wait rather than
//     a new failure mode.

const (
	// incidentDiagnosisTimeout bounds how long a launched Diagnostic Agent has
	// to submit before its generation is considered spent. It is generous: the
	// agent has to read a 48 KB pack and reason about it, and the cost of being
	// early here is a duplicate launch, which is exactly what this file is for.
	incidentDiagnosisTimeout = 15 * time.Minute

	// incidentDiagnosisWaitPhase is the durable phase of a diagnosis AO wants
	// to run and currently cannot, because no provider has capacity. It is a
	// canonical SELF-REMEDIABLE attention reason: nobody has a decision to make
	// about "Claude is rate limited", and AO retries it on its own wake.
	incidentDiagnosisWaitPhase = ReasonIncidentDiagnosisCapacityWait
)

// incidentDiagnoseIdempotencyKey is the single-flight identity of one launch.
//
// Keyed by incident and generation, NOT by run: a run may accumulate several
// incidents over its life, and each one's investigation is its own command.
func incidentDiagnoseIdempotencyKey(incidentID string, generation int) string {
	return "incident-diagnose:" + incidentID + ":gen" + strconv.Itoa(generation)
}

// IncidentLaunchOutcome is what RequestIncidentDiagnosis actually did, so the
// API can tell a person "a provider is busy, AO will retry" apart from "your
// agent is running" apart from "one was already running".
type IncidentLaunchOutcome string

const (
	// IncidentLaunched means a Diagnostic Agent pane was created by this call.
	IncidentLaunched IncidentLaunchOutcome = "launched"
	// IncidentAlreadyRunning means this generation was already launched (by a poll,
	// a double click, or a pass before a restart). Nothing new was started.
	IncidentAlreadyRunning IncidentLaunchOutcome = "already_running"
	// IncidentWaitingForCapacity means routing found no eligible provider right now.
	// A durable wake is scheduled; nobody has a decision to make.
	IncidentWaitingForCapacity IncidentLaunchOutcome = "waiting_for_capacity"
)

// diagnosisGeneration folds the ledger into "which investigation are we on, and
// is one still outstanding".
//
// A generation is outstanding while its request row exists, no diagnosis has
// landed for it, and it is inside incidentDiagnosisTimeout. Once it times out
// the NEXT generation becomes launchable, which is what stops a dead agent from
// wedging an incident permanently — while still costing a diagnosis attempt, so
// a provider that keeps dying cannot loop.
func (c *Coordinator) diagnosisGeneration(inc Incident, now time.Time) (generation int, outstanding bool) {
	generation = inc.Diagnoses
	if generation == 0 {
		return 1, false
	}
	if inc.Diagnosis != nil && inc.Diagnosis.Attempt >= generation {
		// This generation has already answered; the next one is free.
		return generation + 1, false
	}
	if inc.FailedGeneration >= generation {
		// Its launch is durably known to have failed, so nothing is running and
		// waiting for the timeout would strand the incident.
		return generation + 1, false
	}
	if now.Sub(inc.UpdatedAt) < incidentDiagnosisTimeout {
		return generation, true
	}
	return generation + 1, false
}

// selectIncidentDiagnosticProvider chooses the harness through AO's ordinary
// routing.
//
// It routes as domain.WorkflowRoleDecisionResolver deliberately, rather than
// inventing a role. That role already means "an isolated advisory agent AO asks
// a question of", it is already routed CROSS-PROVIDER — so the diagnostician
// prefers a different provider from the one whose work stopped, which is the
// same independence argument the reviewer rests on — and it already has a
// priority list an operator can configure (decisionResolverPriority). A new
// role would have duplicated all three and agreed with none of them.
func (c *Coordinator) selectIncidentDiagnosticProvider(ctx stdctx.Context, run domain.WorkflowRun, incumbent domain.AgentHarness) domain.RoutingDecision {
	owner := c.runOwner(ctx, run.ID)
	snapshot := policyForRun(run).EffectiveExecutionPolicy()
	policy, eligible, ineligible, capacity := c.routingInputsForRole(ctx, owner, domain.WorkflowRoleDecisionResolver, snapshot)

	decision := RouteExecution(RoutingRequest{
		Role:                       domain.WorkflowRoleDecisionResolver,
		CurrentImplementerProvider: domain.ProviderForHarness(incumbent),
		Policy:                     policy,
		EligibleProfiles:           eligible,
		IneligibleReasons:          ineligible,
		Capacity:                   capacity,
	})
	// Deliberately NOT persistRoutingDecision: that writes a `routing_decision`
	// checkpoint, which would become the newest row on a run that is currently
	// STOPPED and therefore rewrite its derived stop reason — the same class of
	// bug as the incident ledger itself (see isIncidentLedgerPhase). The
	// decision is audited inside the incident's own row instead, where it
	// belongs and where it cannot leak.
	return decision
}

// recordIncidentCapacityWait parks the investigation on the ordinary
// self-remediable capacity path.
//
// The run's own state is NOT touched: it is already in needs_attention for its
// own reason, and an investigation that cannot start is a fact about the
// investigation, not a new fact about the run. The wake is what makes it
// self-remediable — AO comes back to it without anyone clicking again.
func (c *Coordinator) recordIncidentCapacityWait(ctx stdctx.Context, run domain.WorkflowRun, inc Incident, decision domain.RoutingDecision) {
	rec := IncidentRecord{
		IncidentID: inc.ID, Signature: inc.Signature,
		StopReason: inc.StopReason, StopDetail: inc.StopDetail,
		Note:           fmt.Sprintf("no provider currently has capacity for a diagnostic agent (%v)", decision.ReasonCodes),
		RoutingReasons: routingReasonStrings(decision.ReasonCodes),
	}
	_ = c.writeIncidentRow(ctx, run, incidentDiagnosisWaitPhase,
		"incident_diagnosis_capacity_wait: "+rec.Note, rec)
	c.scheduleWake(ctx, run, nil, wake.ReasonReviewerCapacity, "")
	if c.log != nil {
		c.log.Info("workflow: incident diagnosis is waiting for provider capacity",
			"run", run.ID, "incident", inc.ID, "reasons", decision.ReasonCodes)
	}
}

// incumbentHarnessFor returns the harness whose work this run's stop came from,
// so cross-provider routing has something to route away from.
func (c *Coordinator) incumbentHarnessFor(ctx stdctx.Context, runID string) domain.AgentHarness {
	steps, err := c.store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		return ""
	}
	for _, s := range steps {
		if s.Kind != domain.WorkflowStepWork {
			continue
		}
		attempts, aerr := c.store.ListWorkflowAttempts(ctx, s.ID)
		if aerr != nil || len(attempts) == 0 {
			return ""
		}
		return domain.AgentHarness(attempts[len(attempts)-1].Harness)
	}
	return ""
}

// claimIncidentLaunch takes the single-flight slot for one generation.
//
// The outbox row is the claim. Its UNIQUE(idempotency_key) is what makes a
// concurrent poll, a double click and a restarted daemon all converge on one
// launch instead of three — the same guard dispatchFixStep and
// dispatchReviewStep rest on, keyed by incident and generation instead of step
// and cycle. A row that comes back anything other than `pending` means someone
// else already got as far as launching this generation, and this pass must not.
func (c *Coordinator) claimIncidentLaunch(ctx stdctx.Context, run domain.WorkflowRun, inc Incident, generation int) (domain.WorkflowOutboxEntry, bool, error) {
	return c.claimIncidentOutboxSlot(ctx, run,
		incidentDiagnoseIdempotencyKey(inc.ID, generation),
		fmt.Sprintf(`{"incidentId":%q,"generation":%d}`, inc.ID, generation))
}

// claimIncidentOutboxSlot is the single-flight claim both incident spawns take:
// enqueue the idempotency key, and win only if the row that comes back is the
// one this pass just created. A row in any other status means another pass
// already got as far as launching this generation, and this one must not.
//
// The two callers differ only in the key they claim and the payload they carry,
// which is exactly why they share this: a second copy of the claim protocol is
// a second place for "pending means mine" to drift.
func (c *Coordinator) claimIncidentOutboxSlot(
	ctx stdctx.Context, run domain.WorkflowRun, idempotencyKey, payload string,
) (domain.WorkflowOutboxEntry, bool, error) {
	entry, _, err := c.store.EnqueueWorkflowOutboxEntry(ctx, domain.WorkflowOutboxEntry{
		ID:             "wfo-" + c.newID(),
		WorkflowRunID:  run.ID,
		IdempotencyKey: idempotencyKey,
		// spawn_worker_session is what this genuinely is — a session is being
		// spawned — and reusing it keeps the command vocabulary (and its CHECK
		// constraint) unchanged rather than requiring a schema migration for a
		// synonym.
		CommandType: domain.WorkflowOutboxSpawnWorkerSession,
		Payload:     payload,
		CreatedAt:   c.clock(),
	})
	if err != nil {
		return domain.WorkflowOutboxEntry{}, false, err
	}
	if entry.Status != domain.WorkflowOutboxPending {
		return entry, false, nil
	}
	if _, err := c.store.UpdateWorkflowOutboxStatus(ctx, entry.ID,
		domain.WorkflowOutboxPending, domain.WorkflowOutboxDispatched, c.clock(), ""); err != nil {
		return entry, false, err
	}
	return entry, true, nil
}

// releaseIncidentLaunch marks a claim failed so the NEXT generation can be
// claimed after a launch that never produced a pane. Without it, a transient
// spawn failure would burn the generation's key and leave the incident unable
// to be investigated at all.
func (c *Coordinator) releaseIncidentLaunch(ctx stdctx.Context, run domain.WorkflowRun, inc Incident, generation int, entry domain.WorkflowOutboxEntry, cause error) {
	_, _ = c.store.UpdateWorkflowOutboxStatus(ctx, entry.ID,
		domain.WorkflowOutboxDispatched, domain.WorkflowOutboxFailed, c.clock(), string(domain.WorkflowErrorAgentStartFailed))
	_ = c.writeIncidentRow(ctx, run, incidentLaunchFailedPhase,
		fmt.Sprintf("incident_diagnosis_launch_failed: generation %d never produced an agent: %v", generation, cause),
		IncidentRecord{
			IncidentID: inc.ID, Signature: inc.Signature, StopReason: inc.StopReason,
			DiagnosisAttempt: generation, Note: cause.Error(),
		})
	if c.log != nil {
		c.log.Warn("workflow: incident diagnostic launch failed", "entry", entry.ID, "err", cause)
	}
}

// routingReasonStrings renders a routing decision's reason codes for the
// incident's durable record, so "why was no provider available" is answerable
// from the ledger rather than only from a log line.
func routingReasonStrings(codes []domain.RoutingReason) []string {
	if len(codes) == 0 {
		return nil
	}
	out := make([]string, 0, len(codes))
	for _, c := range codes {
		out = append(out, string(c))
	}
	return out
}
