package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Stale-verify recovery — Checkpoint 8P-E.14C.
//
// The incident this file exists for (wf-6528a538): a run whose verification
// failed because AO's OWN verifier was wrong — it resolved a file check against
// the worktree root while running that same spec's commands inside the module in
// backend/ — recorded the only outcome the pre-Phase-5 verify path had for a
// failure no fix cycle can repair:
//
//	workflow_run   state = needs_attention
//	verify step    state = failed          (terminal: zero outgoing transitions)
//	latest attempt outcome = failed, error_class = verify_environment_error
//
// The verifier defect was then fixed and the daemon restarted. The run did
// nothing. Not because anything went wrong on restart, but because all three
// durable facts above are, individually, exactly what they say:
//
//  1. maybeVerify returns immediately on `verifyStep.State.Terminal()`, so the
//     corrected verifier is never given the target to re-evaluate;
//  2. the finished attempt row (outcome != "") is maybeVerify's own "this target
//     has already been decided" record, so even a non-terminal step would not
//     have re-executed; and
//  3. needs_attention is not a state the forward transitions leave by
//     themselves, and ContinueRun had nothing to say about a verify step at all.
//
// So the historical verdict — evidence about what happened under the OLD
// verifier, in the OLD environment — became a permanent verdict about the code.
// That is the confusion this file removes. A terminal verify failure whose cause
// was AO's verification infrastructure rather than the work under test is
// evidence, not a sentence: when a person explicitly presses Continue AFTER
// correcting the thing that failed, AO must be able to ask the question again.
//
// The rules, in the same spirit as attention.go's vocabulary:
//
//   - Only an explicit human Continue reopens anything. GetRun/Board polling
//     never does, however often it re-derives the same state (see the deliberate
//     absence of any call from cascade.go/board.go — the ONLY caller of
//     resumeStaleVerifyFailure is ContinueRun).
//   - Only a specifically recoverable failure class reopens. A failing test, an
//     exhausted budget, a changed workspace and every human-owned stop AO knows
//     about are untouched, by name and by class (both are checked).
//   - Every reopen is a numbered generation, recorded durably before anything is
//     mutated, bounded at maxVerifyRecoveryAttempts, and carried into the new
//     attempt's identity and the new VerifyResult — so a repeat Continue, a
//     restart, or both, can never produce a second concurrent attempt for one
//     generation, and the resulting record says which attempt it was.
//   - The old VerifyResult and the old attempt row are never deleted or
//     rewritten. They stay exactly where they are, as the history of what the
//     old verifier concluded.

const (
	// verifyRecoveryRequestedPhase is the durable authorization: a person
	// pressed Continue on a run stopped by a recoverable verification
	// infrastructure failure. Written BEFORE any state is mutated, so a daemon
	// that dies mid-reopen still knows on the next pass that the reopen was
	// authorized and which generation it belongs to.
	verifyRecoveryRequestedPhase = "verify_recovery_requested"
	// verifyReopenedPhase is the durable effect: the verify step is back out of
	// its terminal state and the run is out of needs_attention. Split from the
	// request so the ledger can tell "authorized" from "applied" without
	// depending on checkpoint timestamp ordering.
	verifyReopenedPhase = "verify_reopened"
	// maxVerifyRecoveryAttempts bounds how many times one run may reopen a
	// terminal verification failure, however many times Continue is pressed. The
	// bound exists for the case the person's diagnosis is wrong: without it,
	// "correct the environment and continue" on a condition that was never the
	// environment is an unbounded loop with a human in it.
	maxVerifyRecoveryAttempts = 3
)

// recoverableVerifyStopReasons are the canonical attention reasons (attention.go)
// that name a verification failure AO's own infrastructure, environment or
// configuration caused. Every one of them is a stop whose HumanAction already
// says "fix this, then continue this run" — this file is what makes that
// sentence true.
//
// ReasonVerifyUnrepairable is in the set because it is the flat reason
// finishVerifyFailure records when no VerifyInfraFailure was attached — the
// exact shape wf-6528a538 wrote — but membership alone is never enough: the
// recorded VerifyResult's error class is checked independently below, which is
// what keeps a genuinely unrepairable CODE failure (a changed workspace, a
// failing command past its budget) out of this path.
var recoverableVerifyStopReasons = map[string]bool{
	ReasonVerifyConfigInvalid:   true,
	ReasonVerifyToolUnavailable: true,
	ReasonVerifyInfraFailed:     true,
	ReasonVerifyUnrepairable:    true,
}

// recoverableVerifyErrorClass reports whether a failed verification's class can
// become a different answer once AO or its environment is corrected.
//
// verify_environment_error means the checks could not be run or read at all, and
// verify_ambiguous means AO could not say what happened — both are statements
// about AO, not about the code, and both are exactly what a corrected verifier
// or a repaired host changes.
//
// Everything else is deliberately absent. verify_command_failed, verify_timeout
// and the two artifact classes are verdicts ABOUT the work, and they already
// have a mechanism (the verify->fix cycle, then verify_budget_exhausted).
// verify_workspace_changed is evidence the reviewed target itself moved, which
// is the one thing recovery must never paper over.
func recoverableVerifyErrorClass(class domain.WorkflowErrorClass) bool {
	switch class {
	case domain.WorkflowErrorVerifyEnvironment, domain.WorkflowErrorVerifyAmbiguous:
		return true
	default:
		return false
	}
}

// VerifyRecoveryRecord is the durable payload of both recovery checkpoints. It
// names the generation, and pins the target the recovery was authorized for so
// a later execution can prove it is still verifying the same reviewed work.
type VerifyRecoveryRecord struct {
	// Generation counts from 1. It is the identity of one bounded recovery
	// attempt: it is hashed into the new verify attempt's id and copied onto the
	// VerifyResult the attempt produces.
	Generation int `json:"generation"`
	// TargetKey and ReviewedFingerprint are the verification target as it stood
	// when the recovery was authorized (see VerifyResult's own fields).
	TargetKey           string `json:"targetKey,omitempty"`
	ReviewedFingerprint string `json:"reviewedFingerprint,omitempty"`
	// StopReason and ErrorClass are the failure this recovery reopened, kept so
	// the audit trail explains itself without a join back to the old result.
	StopReason string                    `json:"stopReason,omitempty"`
	ErrorClass domain.WorkflowErrorClass `json:"errorClass,omitempty"`
}

// verifyRecoveryLedger is the whole recovery state of one run, derived from its
// append-only checkpoints. Deriving rather than storing a column is the same
// choice verifyTargetAfterFix makes, and for the same reason: the rows already
// exist, and a restart re-derives the identical answer with no migration.
//
// Nothing here compares timestamps. "Has this generation been executed?" is
// answered by looking for a VerifyResult stamped with that generation, not by
// asking which checkpoint is newer — two checkpoints written in the same clock
// tick are ordinary (the tests' clock does not advance inside one call), and a
// recovery mechanism that could be confused by a tie would be a worse bug than
// the one it fixes.
type verifyRecoveryLedger struct {
	// requests is how many recovery generations this run has ever authorized:
	// the bound maxVerifyRecoveryAttempts applies to it.
	requests int
	// generation is the newest authorized generation, 0 when none.
	generation int
	// record is the newest request's payload.
	record VerifyRecoveryRecord
	// reopened means the state mutation for `generation` is already recorded.
	reopened bool
	// executed means a VerifyResult stamped with `generation` already exists,
	// i.e. the corrected verifier has already answered for this generation.
	executed bool
}

func (c *Coordinator) verifyRecoveryLedger(ctx stdctx.Context, runID string) (verifyRecoveryLedger, error) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return verifyRecoveryLedger{}, err
	}
	var led verifyRecoveryLedger
	for _, cp := range cps {
		if cp.DurablePhase != verifyRecoveryRequestedPhase {
			continue
		}
		led.requests++
		var rec VerifyRecoveryRecord
		if json.Unmarshal([]byte(cp.RetryState), &rec) == nil && rec.Generation > led.generation {
			led.generation, led.record = rec.Generation, rec
		}
	}
	if led.generation == 0 {
		return led, nil
	}
	for _, cp := range cps {
		switch cp.DurablePhase {
		case verifyReopenedPhase:
			var rec VerifyRecoveryRecord
			if json.Unmarshal([]byte(cp.RetryState), &rec) == nil && rec.Generation == led.generation {
				led.reopened = true
			}
		case verifyResultPhase:
			var res VerifyResult
			if json.Unmarshal([]byte(cp.RetryState), &res) == nil && res.RecoveryGeneration == led.generation {
				led.executed = true
			}
		}
	}
	return led, nil
}

// currentVerifyRecovery returns the recovery generation a verification attempt
// executing right now belongs to, and its authorized target. Generation 0 with
// ok=false means "no recovery has ever been authorized for this run", which is
// the state of every run that has never hit this path — hence every behaviour in
// verify.go outside an open recovery is bit-for-bit what it was.
func (c *Coordinator) currentVerifyRecovery(ctx stdctx.Context, runID string) (VerifyRecoveryRecord, bool) {
	led, err := c.verifyRecoveryLedger(ctx, runID)
	if err != nil || led.generation == 0 || led.executed {
		return VerifyRecoveryRecord{}, false
	}
	return led.record, true
}

// resumeStaleVerifyFailure is the authoritative recovery transition, and the
// ONLY function in AO that takes a workflow step out of a terminal state.
//
// Its single caller is ContinueRun. That is not an implementation detail, it is
// invariant 1: a Board poll, a GetRun, a wake-driven observation and a boot
// Reconcile all re-derive this same durable state constantly, and none of them
// is a person saying "I fixed it". Explicit Continue is the authorization.
//
// Returns the (possibly updated) run and whether a verification attempt was
// reopened by this call.
//
// Every write below is idempotent against being re-entered, in any order, across
// any number of restarts:
//
//   - the request checkpoint is written once per generation, and a generation is
//     only opened when the previous one has been executed;
//   - the step reopen is a compare-and-swap on state='failed', so the second
//     caller matches no row;
//   - the un-park is a compare-and-swap on needs_attention;
//   - the reopened checkpoint is written once per generation.
func (c *Coordinator) resumeStaleVerifyFailure(ctx stdctx.Context, run domain.WorkflowRun, steps []domain.WorkflowStep) (domain.WorkflowRun, bool, error) {
	if run.State != domain.WorkflowRunNeedsAttention {
		return run, false, nil
	}
	var verifyStep *domain.WorkflowStep
	for i := range steps {
		if steps[i].Kind == domain.WorkflowStepVerify {
			verifyStep = &steps[i]
		}
	}
	// Only a terminally FAILED verify step is stale in the sense this function
	// means. A verify step still waiting, running or completed is being handled
	// by the ordinary cascade, and a cancelled one belongs to a cancelled run.
	if verifyStep == nil || verifyStep.State != domain.WorkflowStepFailed {
		return run, false, nil
	}

	// Guard 1 — the NAME of the stop. A run stopped for any reason outside the
	// recoverable set (an exhausted fix budget, a dirty worktree, a child that
	// needs a decision, an ambiguous review) is left exactly where it is, which
	// is what keeps this mechanism from becoming "Continue clears every stop".
	reason, _, ok := c.stopReason(ctx, run)
	if !ok || !recoverableVerifyStopReasons[reason] {
		return run, false, nil
	}

	// Guard 2 — the CLASS of the failure, read from the run's own durable
	// VerifyResult. Independent of guard 1 on purpose: ReasonVerifyUnrepairable
	// is recorded for both "AO could not run the checks" and "the checks ran and
	// said no", and only the first may ever be reopened.
	result, hasResult, err := c.latestVerifyResult(ctx, run.ID)
	if err != nil {
		return run, false, err
	}
	if !hasResult || result.Passed || !recoverableVerifyErrorClass(result.ErrorClass) {
		return run, false, nil
	}

	led, err := c.verifyRecoveryLedger(ctx, run.ID)
	if err != nil {
		return run, false, err
	}
	generation := led.generation
	record := led.record
	if generation == 0 || led.executed {
		// Guard 3 — the BOUND. Every previous generation has already been asked
		// and answered; this would be a new one, and there is a limit.
		if led.requests >= maxVerifyRecoveryAttempts {
			c.recordAttentionStopOnce(ctx, run, &verifyStep.ID, ReasonVerifyRecoveryExhausted,
				fmt.Sprintf("verification was reopened %d times after an infrastructure failure (%s) and failed again every time",
					led.requests, result.ErrorClass))
			return run, false, nil
		}
		generation = led.requests + 1
		record = VerifyRecoveryRecord{
			Generation:          generation,
			TargetKey:           result.TargetKey,
			ReviewedFingerprint: result.ReviewedFingerprint,
			StopReason:          reason,
			ErrorClass:          result.ErrorClass,
		}
		if err := c.recordVerifyRecovery(ctx, run, *verifyStep, verifyRecoveryRequestedPhase, record,
			fmt.Sprintf("verify: recovery generation %d authorized by an explicit Continue after %s (%s)",
				generation, reason, result.ErrorClass)); err != nil {
			return run, false, err
		}
		led.reopened = false
	}

	// From here on everything is the idempotent application of `record`. A
	// daemon that died between the request above and these writes re-enters this
	// exact block on the next Continue, with the same generation.
	if _, err := c.store.ReopenFailedWorkflowStep(ctx, verifyStep.ID, c.clock()); err != nil {
		return run, false, err
	}
	if moved, err := c.store.UpdateWorkflowRunState(ctx, run.ID,
		domain.WorkflowRunNeedsAttention, domain.WorkflowRunRunning, c.clock()); err != nil {
		return run, false, err
	} else if moved {
		run.State = domain.WorkflowRunRunning
	}
	if !led.reopened {
		if err := c.recordVerifyRecovery(ctx, run, *verifyStep, verifyReopenedPhase, record,
			fmt.Sprintf("verify: reopened for recovery generation %d against the same reviewed target", generation)); err != nil {
			return run, false, err
		}
	}
	return run, true, nil
}

// recordVerifyRecovery writes one recovery checkpoint. Unlike recordAttentionStop
// this is NOT best-effort: the checkpoint is the only durable record that this
// generation exists, and a reopen whose generation was never written down would
// be an unbounded, unauditable retry.
func (c *Coordinator) recordVerifyRecovery(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, phase string, record VerifyRecoveryRecord, detail string) error {
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	stepID := step.ID
	_, err = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:                "wfc-" + c.newID(),
		WorkflowRunID:     run.ID,
		WorkflowStepID:    &stepID,
		ProjectID:         run.ProjectID,
		RetryState:        string(payload),
		FingerprintBefore: record.ReviewedFingerprint,
		NextAction:        detail,
		DurablePhase:      phase,
		PayloadVersion:    verifyResultVersion,
		CreatedAt:         c.clock(),
	})
	return err
}

// latestVerifyResult reads back the newest durable VerifyResult of a run.
//
// "Newest" is by recovery generation first and timestamp second: a recovery
// attempt's result always supersedes the pre-recovery one it was authorized to
// re-ask, even when a test clock records both at the same instant.
func (c *Coordinator) latestVerifyResult(ctx stdctx.Context, runID string) (VerifyResult, bool, error) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return VerifyResult{}, false, err
	}
	var best VerifyResult
	var bestCP domain.WorkflowCheckpoint
	found := false
	for _, cp := range cps {
		if cp.DurablePhase != verifyResultPhase {
			continue
		}
		var res VerifyResult
		if json.Unmarshal([]byte(cp.RetryState), &res) != nil {
			continue
		}
		if !found ||
			res.RecoveryGeneration > best.RecoveryGeneration ||
			(res.RecoveryGeneration == best.RecoveryGeneration && !cp.CreatedAt.Before(bestCP.CreatedAt)) {
			best, bestCP, found = res, cp, true
		}
	}
	return best, found, nil
}
