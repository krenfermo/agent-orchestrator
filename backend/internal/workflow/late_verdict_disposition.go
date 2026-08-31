package workflow

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// late_verdict_disposition.go — a verdict that arrives after AO stopped
// listening gets exactly one answer, and gets it once.
//
// THE INCIDENT (wf-c4c84f52, review run 7e528219).
//
//	review_run   status=failed   verdict=''   late_verdict=approved
//	review step  failed
//	outbox       trigger_review  workflow-step-review:wfs-876daf3c:cycle1:codex
//	             pending, dispatched_at NULL
//
// The reviewer's launch failed, the step failed with it, the run parked on
// reviewer_launch_failed — and then the reviewer, which had in fact started,
// answered `approved`. review_authority.go correctly tried to adopt that
// verdict rather than throw a real review away. Adoption routes through
// recordReviewOutcome, which starts its transition from where the step actually
// is, and `failed -> completed` is not a legal edge: terminal states have zero
// outgoing transitions (domain.ValidWorkflowStepTransition). So the transition
// was skipped as a benign race, the completion invariant
// (lateVerdictAdoptionState) was never satisfied, and the next pass tried
// again. Forever, once per reconcile, for three hours.
//
// Two durable consequences, and the second is the expensive one:
//
//   - the ledger grew one review_observed row per pass (fixed separately, in
//     review_progress.go: an observation that changes nothing writes nothing);
//   - the cycle's trigger_review stayed `pending`, and clause (7) of
//     proveRepairQuiescent reads a pending dispatch as "this run can still
//     launch work" — correctly, in general. So a repair that could not write
//     anything held its origin's branch, and only Cancel freed it.
//
// # The rule
//
// A late verdict for a review cycle has exactly two terminal dispositions, and
// no third:
//
//	ADOPTED   AO can apply it through a LEGAL transition, and does. Unchanged:
//	          review_authority.go already owns this, and it still routes through
//	          the same recordReviewOutcome an on-time verdict takes, so the
//	          budget, the cascade and the ledger are identical.
//	REFUSED   AO can PROVE the verdict can never become this step's outcome.
//	          Recorded once, with the fact that proves it, and never retried.
//
// "Retry until something changes" is not a disposition. It is what produced the
// incident.
//
// # What makes a late verdict adoptable
//
// Adoption is a state transition, so adoptability is a question about the STEP,
// answered against the state machine rather than against intent:
//
//	running   adoptable — recordReviewOutcome's own edges apply.
//	waiting   adoptable — liftRestingReviewStep brings it back to running under
//	          the same authority guard first. That lift already existed.
//	terminal  REFUSED. completed/failed/cancelled have zero outgoing edges by
//	          construction, and forcing one would mean a step could be resurrected
//	          by a verdict arriving after it ended — exactly the invariant the
//	          state machine exists to hold.
//	pending   REFUSED. The step names a review run while never having been
//	ready     dispatched: AO cannot account for that, and inventing an outcome
//	          from it would be guessing.
//
// The incident's step is `failed`, so its verdict is REFUSED — and that is the
// honest answer, not a workaround. The review it describes judged a workspace
// state through a reviewer whose launch AO had already given up on and whose
// step it had already failed; the run is parked for a person on exactly that
// failure, and the remedy the disposition offers them is unchanged.
//
// # Why refusal does not lose a real review
//
// Nothing is deleted. review_run.late_verdict stays exactly where it is, the
// refusal names it, and both are readable forever. What refusal ends is only
// AO's attempt to APPLY it to a step that cannot legally receive it.

// lateVerdictRefusedPhase is the durable refusal. One row per (step, review
// run), by derived id, so a refusal is exactly-once under concurrent
// reconciles and across restarts.
const lateVerdictRefusedPhase = "review_late_verdict_refused"

// Refusal reasons. Each names a fact that can be re-derived from durable state,
// never a judgement call.
const (
	// lateVerdictRefusedStepTerminal: the step this verdict would conclude has
	// ended. Terminal states have no outgoing transitions.
	lateVerdictRefusedStepTerminal = "step_terminal"
	// lateVerdictRefusedStepNotDispatched: the step names this review run but
	// has never been dispatched, so there is no review episode to conclude.
	lateVerdictRefusedStepNotDispatched = "step_not_dispatched"
)

// lateVerdictDisposition is the durable record's payload. It carries enough to
// re-identify the exact review episode this decision was about without reading
// anything else back: which run, which step, which cycle, which state it judged,
// and what AO decided.
type lateVerdictDisposition struct {
	Disposition  string    `json:"disposition"`
	Reason       string    `json:"reason"`
	ReviewRunID  string    `json:"reviewRunId"`
	StepID       string    `json:"stepId"`
	Cycle        int       `json:"cycle,omitempty"`
	TargetSHA    string    `json:"targetSha,omitempty"`
	Verdict      string    `json:"verdict"`
	StepState    string    `json:"stepState"`
	ReviewStatus string    `json:"reviewStatus"`
	At           time.Time `json:"at"`
}

// lateVerdictAdoptable answers "can this verdict legally become this step's
// outcome", from the step's state and the state machine alone.
//
// It returns the refusal reason when it cannot, so the caller records WHY
// rather than merely that it declined.
func lateVerdictAdoptable(step domain.WorkflowStep) (string, bool) {
	switch {
	case step.State == domain.WorkflowStepRunning || step.State == domain.WorkflowStepWaiting:
		return "", true
	case step.State.Terminal():
		return lateVerdictRefusedStepTerminal, false
	default:
		return lateVerdictRefusedStepNotDispatched, false
	}
}

// lateVerdictRefusalID is the derived identity of one refusal: this review run,
// this step. Two reconciles racing collide on the primary key and one row
// survives; a restart re-deriving the same decision writes nothing new.
func lateVerdictRefusalID(reviewRunID, stepID string) string {
	return fmt.Sprintf("wfc-lvrefused-%s-%s", reviewRunID, stepID)
}

// lateVerdictAlreadyDisposed reports whether this (step, review run) already has
// a terminal disposition, so reconciliation stops re-deciding it.
//
// A read failure reports "already disposed", which REFUSES to act rather than
// acting on an unreadable ledger — the same direction every other proof in this
// package fails in.
func (c *Coordinator) lateVerdictAlreadyDisposed(
	ctx stdctx.Context, runID, stepID, reviewRunID string,
) bool {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return true
	}
	for _, cp := range cps {
		if cp.DurablePhase != lateVerdictRefusedPhase {
			continue
		}
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID {
			continue
		}
		if cp.ReviewRunID != nil && *cp.ReviewRunID == reviewRunID {
			return true
		}
	}
	return false
}

// refuseLateVerdict records the refusal, once.
//
// It is written BEFORE anything else stops happening, and it is the only thing
// that stops it: the next pass reads this row and does not re-enter adoption.
// That ordering is what turns "AO keeps trying" into "AO decided".
func (c *Coordinator) refuseLateVerdict(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	reviewRun domain.ReviewRun,
	reason string,
) error {
	cycle := 0
	if n, err := c.completedReviewCycles(ctx, reviewRun.SessionID, reviewRun.Harness); err == nil {
		cycle = n
	}
	rec := lateVerdictDisposition{
		Disposition: "refused", Reason: reason,
		ReviewRunID: reviewRun.ID, StepID: step.ID, Cycle: cycle,
		TargetSHA: reviewRun.TargetSHA, Verdict: string(reviewRun.EffectiveVerdict()),
		StepState: string(step.State), ReviewStatus: string(reviewRun.Status),
		At: c.clock(),
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	stepID := step.ID
	reviewRunID := reviewRun.ID
	detail := fmt.Sprintf(
		"review_late_verdict_refused: review run %s answered %s after AO closed it out as %s, but this step is %s and a %s step has no legal transition left — the verdict is recorded as unadoptable and will not be retried",
		reviewRun.ID, reviewRun.EffectiveVerdict(), reviewRun.Status, step.State, step.State)
	if reason == lateVerdictRefusedStepNotDispatched {
		detail = fmt.Sprintf(
			"review_late_verdict_refused: review run %s answered %s, but this step is %s and was never dispatched, so AO cannot account for a review episode to conclude — the verdict is recorded as unadoptable and will not be retried",
			reviewRun.ID, reviewRun.EffectiveVerdict(), step.State)
	}
	_, err = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             lateVerdictRefusalID(reviewRun.ID, step.ID),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		ReviewRunID:    &reviewRunID,
		ReviewVerdict:  string(reviewRun.EffectiveVerdict()),
		HeadSHA:        reviewRun.TargetSHA,
		NextAction:     detail,
		DurablePhase:   lateVerdictRefusedPhase,
		PayloadVersion: "v1",
		RetryState:     string(payload),
		CreatedAt:      c.clock(),
	})
	if errors.Is(err, domain.ErrDuplicateWorkflowCheckpoint) {
		// Another reconciler refused it first. One decision, one row.
		return nil
	}
	if err != nil {
		return err
	}
	if c.log != nil {
		c.log.Info("workflow: a late review verdict was recorded as unadoptable",
			"run", run.ID, "step", step.ID, "reviewRun", reviewRun.ID,
			"verdict", reviewRun.EffectiveVerdict(), "stepState", step.State, "reason", reason)
	}
	return nil
}

// ---------------------------------------------------------------------------
// The dispatch a disposed cycle leaves behind.
// ---------------------------------------------------------------------------

// reviewDispatchRetiredClass is the error class stamped on a review dispatch
// that can never launch. It is deliberately not an acknowledgement: nothing was
// dispatched and nothing was acknowledged, and saying otherwise would put a lie
// in the one row a person reads to find out what happened.
const reviewDispatchRetiredClass = "review_step_terminal"

// retireUnlaunchableReviewDispatches closes review dispatches that cannot
// produce a reviewer.
//
// THE PROOF, and it is a proof rather than a heuristic: dispatchReviewStep's
// own first line refuses a review step whose state is terminal. So for a
// terminal review step there is no path — boot reconciliation, a wake, an
// ordinary read, or a human Continue — that can turn its pending or dispatched
// trigger_review into a reviewer. The row is not a queued launch; it is a
// fossil that reads like one, and clause (7) of the quiescence proof is right
// to refuse while it stands.
//
// Scope is the narrowest that covers it:
//
//   - only entries whose idempotency key names THIS step. A dispatch belonging
//     to another step is another obligation and is never touched.
//   - only while that step is TERMINAL. A live step's pending dispatch is a
//     real queued launch, and retiring one would silently cancel a review
//     somebody is waiting for.
//   - the transition is a compare-and-set on the exact status observed, so two
//     daemons produce one retirement and a stale pass produces none.
//
// It covers both halves of "this cycle has been answered" without needing to
// reason about cycles at all: an ADOPTED verdict drives the step to completed,
// and a REFUSED one leaves it terminal already. Either way the step is terminal
// and no cycle of it can launch. A step that is still live keeps every dispatch
// it has.
func (c *Coordinator) retireUnlaunchableReviewDispatches(
	ctx stdctx.Context, run domain.WorkflowRun, steps []domain.WorkflowStep, out *executionAuthorityRetirement,
) {
	terminalReviewSteps := map[string]domain.WorkflowStep{}
	for _, step := range steps {
		if step.Kind == domain.WorkflowStepReview && step.State.Terminal() {
			terminalReviewSteps[step.ID] = step
		}
	}
	if len(terminalReviewSteps) == 0 {
		return
	}
	entries, err := c.store.ListWorkflowOutboxByRun(ctx, run.ID)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.CommandType != domain.WorkflowOutboxTriggerReview {
			continue
		}
		if entry.Status != domain.WorkflowOutboxPending && entry.Status != domain.WorkflowOutboxDispatched {
			continue
		}
		step, named := terminalReviewSteps[reviewDispatchStepID(entry)]
		if !named {
			// Either it names a step this run does not have, or one that is
			// still live. Both are somebody else's business.
			continue
		}
		retired, uerr := c.store.UpdateWorkflowOutboxStatus(ctx, entry.ID,
			entry.Status, domain.WorkflowOutboxFailed, c.clock(), reviewDispatchRetiredClass)
		switch {
		case uerr != nil:
			out.Refused = append(out.Refused, retiredAuthority{
				Kind: "review_dispatch", ID: entry.IdempotencyKey,
				Detail: "retiring the dispatch failed: " + uerr.Error()})
		case retired:
			out.Retired = append(out.Retired, retiredAuthority{
				Kind: "review_dispatch", ID: entry.IdempotencyKey,
				Detail: fmt.Sprintf(
					"review step %s is %s, so this cycle-%d dispatch can never launch a reviewer (dispatchReviewStep refuses a terminal step); it is closed rather than left looking like a queued launch",
					step.ID, step.State, reviewCycleOf(entry))})
		}
	}
}

// reviewDispatchStepID recovers the step a review dispatch belongs to.
//
// It prefers the entry's own foreign key and falls back to the idempotency
// key's second segment, because both key shapes
// (`workflow-step-review:<step>:cycle…` and
// `workflow-step-review-replacement:<step>:<run>`) carry the step id there and
// a historical row may predate the column being set. An entry that names no
// step at all resolves to "", which matches no step and is therefore never
// retired.
func reviewDispatchStepID(entry domain.WorkflowOutboxEntry) string {
	if entry.WorkflowStepID != nil && *entry.WorkflowStepID != "" {
		return *entry.WorkflowStepID
	}
	parts := strings.Split(entry.IdempotencyKey, ":")
	if len(parts) < 2 {
		return ""
	}
	switch parts[0] {
	case "workflow-step-review", "workflow-step-review-replacement":
		return parts[1]
	default:
		return ""
	}
}
