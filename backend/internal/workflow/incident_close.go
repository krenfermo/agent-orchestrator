package workflow

import (
	stdctx "context"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// incident_close.go — Checkpoint 8P-E.21.
//
// An incident outlives its cause. Someone commits their dirty worktree, a
// provider's capacity comes back, a person clicks Reanudar, a child recovers —
// and the "¿Qué hago?" entry keeps advertising a problem that stopped existing.
// That is not merely untidy: an operator who learns that AO's incidents are
// often stale stops reading them, which costs more than never having had them.
//
// # RESOLVED and CLOSED are different claims
//
// RESOLVED means the Advisor did something attributable and that something was
// verified: it continued the run, or an approved repair passed an independent
// review and deterministic checks. It is a claim about AO's own agency.
//
// CLOSED means the condition went away by some other route. AO takes no credit
// and asserts no repair — it records what it observed and stops asking for
// attention. Conflating the two would let the Advisor claim every recovery that
// happened near it, which is exactly the kind of self-congratulatory telemetry
// that makes a system's own reports untrustworthy.
//
// # Closing requires positive evidence, never a timeout
//
// The only thing that may close an incident is a READ of the run's live durable
// state showing the condition gone. Not elapsed time, not a missing checkpoint,
// not an agent's opinion. If AO cannot read the run, the incident stays open —
// failing to read is never evidence that a problem has ended.
//
// # Never closes what it might be responsible for
//
// An incident that dispatched a repair, or executed any action, is out of
// scope here regardless of what the run now looks like. Its outcome belongs to
// reconcileIncidentRepair (reviewed + verified => RESOLVED) or to the action's
// own record. Closing such an incident would erase the audit trail of a repair
// that really ran.

// incidentClosureCause names why an incident stopped being about anything.
// Each value corresponds to a distinct, observable change in durable state.
type incidentClosureCause string

const (
	// closureRunNoLongerStopped: the run left needs_attention entirely — it is
	// running again, or it finished, or it was cancelled.
	closureRunNoLongerStopped incidentClosureCause = "run_no_longer_stopped"
	// closureStopChanged: the run is still stopped, but on a different
	// condition. The original incident is over; a new one describes the new
	// stop, and conflating them would attach an old diagnosis to a new problem.
	closureStopChanged incidentClosureCause = "stop_condition_changed"
)

// reconcileIncidentClosure closes an incident whose cause is observably gone.
//
// Returns the incident, updated in place when it closed. Idempotent by
// construction: the transition into `closed` is only valid from a non-terminal
// state, so a second pass over an already-closed incident writes nothing, and
// the write happens before the returned value changes so a crash in between
// leaves the ledger — not the in-memory value — as the source of truth.
func (c *Coordinator) reconcileIncidentClosure(ctx stdctx.Context, run domain.WorkflowRun, inc Incident) Incident {
	if inc.State.Terminal() {
		return inc
	}
	// Anything the Advisor actually did makes this incident's outcome its own
	// business. A dispatched repair resolves or refuses through its run; an
	// executed action already recorded its result.
	if inc.RepairRunID != "" || inc.Executions > 0 {
		return inc
	}

	cause, evidence, ok := c.observeIncidentClosure(ctx, run, inc)
	if !ok {
		return inc
	}
	rec := IncidentRecord{
		IncidentID: inc.ID, Signature: inc.Signature,
		StopReason: inc.StopReason, StopDetail: inc.StopDetail,
		ClosureCause: string(cause), Evidence: evidence,
		Note: "closed without a repair executed by the Incident Advisor",
	}
	if err := c.writeIncidentRow(ctx, run, incidentClosedPhase,
		fmt.Sprintf("incident_closed (%s): %s", cause, strings.Join(evidence, "; ")), rec); err != nil {
		// The ledger is the truth. A failed write means the incident is still
		// open, and saying otherwise in memory would make the next read
		// disagree with the next restart.
		return inc
	}
	if c.log != nil {
		c.log.Info("workflow: incident closed without a repair",
			"run", run.ID, "incident", inc.ID, "cause", cause, "evidence", evidence)
	}
	inc.State = IncidentClosed
	inc.ClosureCause = string(cause)
	inc.ClosureEvidence = evidence
	return inc
}

// observeIncidentClosure reads the run's live durable state and answers whether
// this incident's condition is observably gone, with the minimum evidence that
// justifies saying so.
//
// Every branch here is a positive observation. There is deliberately no branch
// for "we could not tell" that closes anything.
func (c *Coordinator) observeIncidentClosure(ctx stdctx.Context, run domain.WorkflowRun, inc Incident) (incidentClosureCause, []string, bool) {
	if run.State != domain.WorkflowRunNeedsAttention {
		// The run is running, waiting, completed or cancelled. Whatever was
		// blocking it is not blocking it now, and AO did not do it.
		return closureRunNoLongerStopped, []string{
			fmt.Sprintf("the run left needs_attention and is now %s", run.State),
			fmt.Sprintf("the incident was opened for %q", inc.StopReason),
		}, true
	}

	// Still stopped. It only closes if it is stopped on something ELSE — read
	// from the same durable carriers ClassifyAttention uses, so the incident
	// and the Board can never disagree about what the run is waiting on.
	reason, _, ok := c.stopReason(ctx, run)
	if !ok || reason == "" {
		// AO cannot currently name the stop. That is missing evidence, not
		// evidence of recovery.
		return "", nil, false
	}
	if reason == inc.StopReason {
		steps, err := c.store.ListWorkflowSteps(ctx, run.ID)
		if err != nil {
			return "", nil, false
		}
		if incidentSignature(reason, c.stopDetailFor(ctx, run, reason), steps) == inc.Signature {
			// Same reason, same shape: nothing has changed.
			return "", nil, false
		}
		return closureStopChanged, []string{
			fmt.Sprintf("the run is still stopped on %q but its steps have moved since this incident was opened", reason),
		}, true
	}
	return closureStopChanged, []string{
		fmt.Sprintf("the run is now stopped on %q, not on %q", reason, inc.StopReason),
	}, true
}
