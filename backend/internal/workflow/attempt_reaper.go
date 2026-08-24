package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// attempt_reaper.go — closing attempts that a restart or a crash abandoned.
//
// A workflow_attempts row with no finished_at means "an attempt is in flight".
// That reading is what makes AO safe: every guard that asks "could something
// still be writing to this tree?" answers yes while such a row exists, and
// refuses to act. verify_branch_advanced.go's proof 5 is one of them.
//
// The reading is only true while the process that opened the row is alive. A
// daemon killed mid-attempt leaves the row exactly as it left it, and nothing
// ever closes it, because the only writer that would have is gone. From then on
// the row is not evidence of work in flight — it is the fossil of a writer that
// no longer exists. Every guard keeps believing it, and the run is stuck behind
// a claim nobody can retract.
//
// This closes those rows, and ONLY those. It is not a timeout and not a
// sweeper: an attempt is reaped only when durable state recorded AFTER it
// started proves the run went on without it, and only when the agent that owned
// it is provably not running. Anything AO cannot prove is left alone, because a
// wrongly-reaped attempt is the one failure mode that matters here — it would
// tell every guard downstream that the tree is quiet while an agent is still
// writing to it, which is exactly the lie the guards exist to prevent.
//
// The four proofs, all required:
//
//  1. the attempt is open — finished_at IS NULL — and old enough that a
//     dispatch in progress cannot be mistaken for an abandoned one;
//  2. its step is not running or ready: a step still in flight OWNS its attempt,
//     and closing that one would be reaping live work, not a fossil;
//  3. there is durable evidence, created strictly after the attempt started and
//     recorded against a DIFFERENT step, that the run progressed past it — AO
//     dispatched a review, ran a verification, moved on. An attempt the run
//     never went past is not abandoned; it is simply unfinished, and it stays;
//  4. the agent that owned it is not running and has been quiet longer than the
//     settle window. A session AO cannot read refuses: "AO does not know what
//     its worker is doing" is precisely the ambiguity this must exclude.
//
// Durability and exactly-once: the reap is written as a checkpoint keyed to the
// attempt id BEFORE the row is closed. A crash between the two leaves the
// checkpoint and an open row, and the next pass finds the checkpoint, skips
// writing a second one, and finishes closing the row. A crash after both leaves
// nothing to do, because proof 1 no longer holds. The ledger therefore carries
// exactly one reap record per attempt no matter how many times this runs.
//
// It spends no budget: fix budget is counted from review runs per session
// (review_progress.go), never from attempt rows, and this creates no attempt
// row and dispatches nothing. The attempt is recorded `cancelled`, which is
// what actually happened to it — not `failed`, which would claim AO observed an
// outcome it never saw.
const (
	// attemptReapedPhase is the durable record of one reaped attempt. One per
	// attempt id, ever.
	attemptReapedPhase = "attempt_reaped_orphaned"
	// attemptReapMinimumAge is how old an open attempt must be before it may be
	// read as abandoned rather than as a dispatch still in progress. It is
	// deliberately far longer than any dispatch takes: the cost of waiting is a
	// run that stays parked a little longer, and the cost of being wrong is
	// reaping an attempt whose agent is still writing.
	attemptReapMinimumAge = 30 * time.Minute
	// attemptReapSettleWindow is how long the owning agent must have been quiet.
	// It mirrors humanFixSettleWindow for the same reason
	// branchAdvancedSettleWindow does: the three answer the same question and
	// must not disagree about how long an agent takes to go quiet.
	attemptReapSettleWindow = humanFixSettleWindow
)

// attemptReapRecord is the durable payload: what was closed, and the evidence
// that closing it was allowed. A person reading the ledger can re-check every
// claim rather than take it.
type attemptReapRecord struct {
	// Reason is the machine-readable class of the close.
	Reason string `json:"reason"`
	// AttemptID and StepID identify the row this record closed, and StepKind
	// says what kind of work it was.
	AttemptID string `json:"attemptId"`
	StepID    string `json:"stepId"`
	StepKind  string `json:"stepKind"`
	// StartedAt is when the abandoned attempt opened.
	StartedAt time.Time `json:"startedAt"`
	// EvidencePhase / EvidenceStepID / EvidenceAt are proof 3: the durable
	// record, on another step, written after this attempt started, that shows
	// the run carried on without it.
	EvidencePhase  string    `json:"evidencePhase"`
	EvidenceStepID string    `json:"evidenceStepId"`
	EvidenceAt     time.Time `json:"evidenceAt"`
	// SessionID is the agent proved not to be running.
	SessionID  string    `json:"sessionId,omitempty"`
	ObservedAt time.Time `json:"observedAt"`
}

// reapOrphanedAttempts closes every attempt on this run that the four proofs
// above show was abandoned, and leaves every other attempt exactly as it is.
//
// It returns how many rows it closed. It is a no-op for a run whose attempts
// are all accounted for, which is nearly every run, so it is cheap to call on
// any path a person drives.
func (c *Coordinator) reapOrphanedAttempts(ctx stdctx.Context, run domain.WorkflowRun, steps []domain.WorkflowStep) (int, error) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, run.ID)
	if err != nil {
		return 0, err
	}
	// Which attempts already carry a reap record, so a resumed reap writes no
	// second one. Read once, for the whole run.
	reaped := map[string]bool{}
	for _, cp := range cps {
		if cp.DurablePhase == attemptReapedPhase && cp.AttemptID != nil {
			reaped[*cp.AttemptID] = true
		}
	}

	now := c.clock()
	closed := 0
	for i := range steps {
		step := steps[i]
		// Proof 2 — a step still in flight owns its attempt.
		if step.State == domain.WorkflowStepRunning || step.State == domain.WorkflowStepReady {
			continue
		}
		attempts, err := c.store.ListWorkflowAttempts(ctx, step.ID)
		if err != nil {
			// Unreadable is not "nothing to do", but it is also not this
			// function's business to fail the caller's whole operation over.
			// Refusing to reap is always safe; it leaves the run exactly as it
			// was.
			return closed, err
		}
		for _, a := range attempts {
			// Proof 1 — open, and old enough not to be a live dispatch.
			if a.FinishedAt != nil {
				continue
			}
			if now.Sub(a.StartedAt) < attemptReapMinimumAge {
				continue
			}
			// Proof 3 — durable evidence the run moved past it.
			evidence, ok := laterProgressEvidence(cps, step.ID, a)
			if !ok {
				continue
			}
			// Proof 4 — the agent that owned it is not running.
			sessionID, quiet := c.attemptOwnerIsQuiet(ctx, cps, step.ID, now)
			if !quiet {
				continue
			}
			rec := attemptReapRecord{
				Reason:         "orphaned_after_restart",
				AttemptID:      a.ID,
				StepID:         step.ID,
				StepKind:       string(step.Kind),
				StartedAt:      a.StartedAt,
				EvidencePhase:  evidence.DurablePhase,
				EvidenceStepID: derefString(evidence.WorkflowStepID),
				EvidenceAt:     evidence.CreatedAt,
				SessionID:      sessionID,
				ObservedAt:     now,
			}
			// The record first, so a crash between the two resumes rather than
			// loses the audit trail — and exactly once, so a resumed reap adds
			// nothing to the ledger.
			if !reaped[a.ID] {
				if err := c.recordAttemptReap(ctx, run, step, rec); err != nil {
					return closed, err
				}
				reaped[a.ID] = true
			}
			// `cancelled`, because that is what happened: the attempt was
			// abandoned before it produced an outcome. No error class — AO
			// never observed one, and inventing one would put a failure AO did
			// not see into the run's error history.
			if err := c.store.UpdateWorkflowAttemptOutcome(ctx, a.ID, now, domain.WorkflowAttemptCancelled, ""); err != nil {
				return closed, err
			}
			closed++
			if c.log != nil {
				c.log.Info("workflow: closed an attempt abandoned by a restart",
					"run", run.ID, "step", step.Kind, "attempt", a.ID,
					"startedAt", a.StartedAt, "evidence", evidence.DurablePhase)
			}
		}
	}
	return closed, nil
}

// laterProgressEvidence is proof 3: the earliest durable checkpoint, recorded
// against a step OTHER than the attempt's own, created strictly after the
// attempt opened.
//
// "Another step" is what makes it evidence. A checkpoint on the attempt's own
// step could have been written BY that attempt, so it proves nothing about
// whether the run went past it. A checkpoint on a different step proves AO was
// somewhere else, doing something else, after this attempt started — and AO
// runs one step of a run at a time, so it cannot have been somewhere else while
// this attempt was still live.
func laterProgressEvidence(cps []domain.WorkflowCheckpoint, stepID string, a domain.WorkflowAttempt) (domain.WorkflowCheckpoint, bool) {
	var best domain.WorkflowCheckpoint
	found := false
	for _, cp := range cps {
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID == stepID {
			continue
		}
		// A checkpoint this attempt itself produced is never evidence against
		// it, whichever step it was filed under.
		if cp.AttemptID != nil && *cp.AttemptID == a.ID {
			continue
		}
		if !cp.CreatedAt.After(a.StartedAt) {
			continue
		}
		if !found || cp.CreatedAt.Before(best.CreatedAt) {
			best, found = cp, true
		}
	}
	return best, found
}

// attemptOwnerIsQuiet is proof 4. It resolves the session that owned the step
// from the step's own checkpoints, falling back to the run's, and answers
// whether that agent is provably not running.
//
// Every unknown answers false. No session recorded, no session facts, an
// unreadable session, a session that is active or was active inside the settle
// window — each of them means AO cannot say the agent is gone, and an attempt
// whose owner AO cannot account for is not reapable.
func (c *Coordinator) attemptOwnerIsQuiet(ctx stdctx.Context, cps []domain.WorkflowCheckpoint, stepID string, now time.Time) (string, bool) {
	if c.sessionFacts == nil {
		return "", false
	}
	// The step's own session first; any session the run recorded otherwise.
	sessionID := ""
	var newestAt time.Time
	for _, cp := range cps {
		if cp.SessionID == nil || strings.TrimSpace(*cp.SessionID) == "" {
			continue
		}
		own := cp.WorkflowStepID != nil && *cp.WorkflowStepID == stepID
		if sessionID == "" || (own && cp.CreatedAt.After(newestAt)) {
			sessionID, newestAt = strings.TrimSpace(*cp.SessionID), cp.CreatedAt
		}
	}
	if sessionID == "" {
		return "", false
	}
	sess, found, err := c.sessionFacts.GetSession(ctx, domain.SessionID(sessionID))
	if err != nil || !found {
		return sessionID, false
	}
	if agentMayStillBeDelivering(sess, now) {
		return sessionID, false
	}
	return sessionID, true
}

// recordAttemptReap writes the durable record. Like the fresh-review
// checkpoints, it is not best-effort: it is the only account of why an attempt
// AO did not observe finishing is nevertheless closed, and closing one without
// it would be an unexplainable edit to the run's history.
func (c *Coordinator) recordAttemptReap(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, rec attemptReapRecord) error {
	payload, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	stepID := step.ID
	attemptID := rec.AttemptID
	sessionID := rec.SessionID
	var sessionPtr *string
	if sessionID != "" {
		sessionPtr = &sessionID
	}
	_, err = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		AttemptID:      &attemptID,
		ProjectID:      run.ProjectID,
		SessionID:      sessionPtr,
		RetryState:     string(payload),
		NextAction: fmt.Sprintf(
			"attempt_reaped_orphaned: the %s attempt started %s never recorded an outcome, and %s on another step at %s proves the run continued without it; its agent is not running, so the attempt is closed as cancelled",
			step.Kind, rec.StartedAt.Format(time.RFC3339), rec.EvidencePhase, rec.EvidenceAt.Format(time.RFC3339)),
		DurablePhase:   attemptReapedPhase,
		PayloadVersion: verifyResultVersion,
		CreatedAt:      c.clock(),
	})
	return err
}

// derefString is the nil-safe read of an optional checkpoint column.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
