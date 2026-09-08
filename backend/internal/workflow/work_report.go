package workflow

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// work_report.go — storing what a worker says it did, and refusing to treat it
// as proof of anything.
//
// The report is durable for the same reason the evidence is: a reviewer that
// reads it after a restart must read the same words the policy read, not a
// re-derivation. It is stored on the WORK step, because it is a fact about the
// work, and it is read at review cycle 1 alongside the evidence.
//
// The whole point of the type is the separation. AO holds two accounts of the
// same change — one it produced by running commands, one the worker wrote —
// and this file is careful never to let the second stand in for the first:
//
//   - the report never contributes a pass. `AdmitsAnyFailure` is the only
//     predicate policy reads, and it only ever DEEPENS a review;
//   - a claim contradicted by an observation is recorded as a contradiction,
//     and a contradiction independently blocks any relief;
//   - a run with no report at all is not penalised and not credited. It is
//     simply a run whose reviewer gets less context.

// SubmitWorkReport records a worker's structured declaration for a run's work
// step. It is the durable core of the report contract: normalization, bounds,
// versioning and the append-only write.
//
// It is deliberately permissive about CONTENT and strict about IDENTITY. A
// report whose prose is odd is still recorded — it is a declaration, and
// editing it would make AO the author. A report for a run or step that does not
// exist is refused, because a declaration nobody can attribute is not one.
func (c *Coordinator) SubmitWorkReport(ctx stdctx.Context, runID string, report domain.WorkReport) (domain.WorkReport, error) {
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return domain.WorkReport{}, err
	}
	if !ok {
		return domain.WorkReport{}, fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	steps, err := c.store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		return domain.WorkReport{}, err
	}
	var workStep *domain.WorkflowStep
	for i := range steps {
		if steps[i].Kind == domain.WorkflowStepWork {
			workStep = &steps[i]
			break
		}
	}
	if workStep == nil {
		return domain.WorkReport{}, fmt.Errorf("%w: workflow run %q has no work step to report on", ErrInvalid, runID)
	}

	normalized := report.Normalize(c.clock())
	// AO stamps the fingerprint itself. The worker cannot compute AO's content
	// fingerprint and is never asked to: a reference the subject supplies is a
	// claim, and this one has to be a fact for the relief policy to compare it.
	if cp, hasCP, cpErr := c.store.GetLatestWorkflowCheckpointByStep(ctx, workStep.ID); cpErr == nil && hasCP {
		normalized.Reference.FingerprintAtSubmission = cp.FingerprintAfter
		if normalized.Reference.Branch == "" {
			normalized.Reference.Branch = cp.Branch
		}
	}

	payload, err := json.Marshal(normalized)
	if err != nil {
		return domain.WorkReport{}, err
	}
	// RUN-SCOPED, with no step id, and this is load-bearing rather than
	// incidental.
	//
	// A checkpoint carrying a step id becomes that step's LATEST checkpoint,
	// and dispatchReviewStep reads exactly that row to recover the session,
	// worktree, branch and completion fingerprint it needs. A report attached
	// to the work step therefore displaced those facts the moment it was newer
	// than the work-completion row — which is always, in any run where a real
	// clock advances between finishing and reporting — and the review dispatch
	// then found a checkpoint with no session on it and stopped the run as
	// ambiguous.
	//
	// The report is not a step-lifecycle fact anyway. It is a declaration about
	// the run, read once at review cycle 1, and it belongs at the run level
	// where nothing reads it as a step's state.
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		ProjectID:      run.ProjectID,
		RetryState:     string(payload),
		NextAction:     "work_report",
		DurablePhase:   workReportPhase,
		PayloadVersion: domain.WorkReportVersion,
		CreatedAt:      normalized.SubmittedAt,
	}); err != nil {
		return domain.WorkReport{}, err
	}
	return normalized, nil
}

// workReportForRun re-reads the newest report recorded for a run.
// ok=false means there is none or it cannot be decoded — never an empty report,
// which a caller could mistake for "the worker said nothing was wrong".
//
// A worker may report more than once; every report is kept and the newest is
// the one policy and the reviewer read.
func (c *Coordinator) workReportForRun(ctx stdctx.Context, runID string) (domain.WorkReport, bool) {
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return domain.WorkReport{}, false
	}
	var latest *domain.WorkflowCheckpoint
	for i := range checkpoints {
		cp := &checkpoints[i]
		if cp.DurablePhase != workReportPhase {
			continue
		}
		if latest == nil || cp.CreatedAt.After(latest.CreatedAt) {
			latest = cp
		}
	}
	if latest == nil {
		return domain.WorkReport{}, false
	}
	var report domain.WorkReport
	if err := json.Unmarshal([]byte(latest.RetryState), &report); err != nil || !report.Recorded() {
		return domain.WorkReport{}, false
	}
	return report, true
}

// reportContradictsEvidence reports whether the worker claimed a command passed
// that AO watched fail.
//
// The match is deliberately conservative: it compares normalized command text
// and only fires when AO actually observed a FAILING check for a command the
// worker named as passing. A claim AO has no observation for is not a
// contradiction — it is simply unverified, which is the ordinary state of every
// claim and is handled by not believing it.
func reportContradictsEvidence(report domain.WorkReport, evidence domain.PreReviewEvidence) bool {
	if !report.Recorded() || !evidence.Recorded() {
		return false
	}
	failed := map[string]bool{}
	for _, c := range evidence.Checks {
		if !c.Passed {
			failed[normalizeCommandText(c.Command)] = true
			failed[normalizeCommandText(c.Label)] = true
		}
	}
	if len(failed) == 0 {
		return false
	}
	for _, claim := range report.TestsReported {
		if claim.ClaimedOutcome != domain.WorkReportOutcomeClaimedPassed {
			continue
		}
		if failed[normalizeCommandText(claim.Command)] {
			return true
		}
	}
	return false
}

// normalizeCommandText collapses whitespace and case so "go test ./..." and
// "go  test ./..." compare equal. It does not attempt to understand commands;
// anything cleverer would start guessing at equivalence, and a wrong guess here
// would either invent a contradiction or hide one.
func normalizeCommandText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// ErrWorkReportWindowClosed is returned when a report arrives after AO has
// already decided how deeply to review the change it describes.
//
// It is a refusal rather than a late acceptance because of what the record
// would otherwise say. The depth decision reads the report; once that decision
// is written, a report accepted afterwards would sit in the ledger next to a
// decision it could not have informed, and the only honest reading of that pair
// is the wrong one. A worker that reports late is told so, and the decision
// stays explainable.
var ErrWorkReportWindowClosed = errors.New("workflow: the work-report window is closed")

// WorkReportReceipt is what a caller gets back for an accepted report: which
// run and step it landed on, whether it replaced an earlier one, and the
// normalized report AO actually stored.
type WorkReportReceipt struct {
	WorkflowRunID  string
	WorkflowStepID string
	// Superseded is true when this run already had a report and this one
	// replaced it as the newest. Both stay on the ledger — the write is
	// append-only — so a worker that reports twice leaves a history rather than
	// an edit.
	Superseded bool
	Report     domain.WorkReport
}

// SubmitWorkReportForSession is the transport-facing entry point: a worker
// names ITS OWN session, and AO resolves which run and step that session is
// executing from durable state.
//
// Addressing by session rather than by run id is deliberate, and it is the same
// choice `ao review submit` makes. The session is the thing the agent provably
// is — its credential is bound to exactly one, and AO_SESSION_ID names it in
// every pane — whereas a run id is a string the agent would have to be told and
// could therefore be told wrongly. Resolving the run from the session means a
// worker can only ever report on the work it is actually doing.
//
// Four states are refused, each for its own reason:
//
//   - no non-terminal run has a work step in this session: there is nothing
//     this report could be about. That covers a cancelled run, a finished one,
//     and a pane from a launch generation whose step has since been re-dispatched
//     into a different session;
//   - the work step has not been dispatched into this session at all;
//   - the run's review depth has already been decided (see
//     ErrWorkReportWindowClosed);
//   - the report is unusable as a declaration.
func (c *Coordinator) SubmitWorkReportForSession(ctx stdctx.Context, sessionID string, report domain.WorkReport) (WorkReportReceipt, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return WorkReportReceipt{}, fmt.Errorf("%w: a session id is required", ErrInvalid)
	}
	run, workStep, ok, err := c.runForWorkSession(ctx, sessionID)
	if err != nil {
		return WorkReportReceipt{}, err
	}
	if !ok {
		return WorkReportReceipt{}, fmt.Errorf(
			"%w: no active workflow work step is running in session %q", ErrNotFound, sessionID)
	}
	if c.workReportWindowClosed(ctx, run.ID) {
		return WorkReportReceipt{}, fmt.Errorf(
			"%w: run %s has already decided how deeply to review this change", ErrWorkReportWindowClosed, run.ID)
	}
	_, superseded := c.workReportForRun(ctx, run.ID)
	stored, err := c.SubmitWorkReport(ctx, run.ID, report)
	if err != nil {
		return WorkReportReceipt{}, err
	}
	return WorkReportReceipt{
		WorkflowRunID:  run.ID,
		WorkflowStepID: workStep.ID,
		Superseded:     superseded,
		Report:         stored,
	}, nil
}

// runForWorkSession finds the non-terminal run whose WORK step is currently
// dispatched into sessionID.
//
// It matches on the step's own recorded session rather than on any naming
// convention, so a pane from a superseded launch generation — whose step now
// carries a different session id — resolves to nothing rather than to the run
// it used to belong to. That is the stale-generation refusal, and it falls out
// of the data instead of needing a rule.
//
// Terminal runs are not scanned at all: a report about work that has already
// finished, been cancelled, or failed has nothing left to inform.
func (c *Coordinator) runForWorkSession(ctx stdctx.Context, sessionID string) (domain.WorkflowRun, domain.WorkflowStep, bool, error) {
	runs, err := c.store.ListNonTerminalWorkflowRuns(ctx)
	if err != nil {
		return domain.WorkflowRun{}, domain.WorkflowStep{}, false, err
	}
	for _, run := range runs {
		steps, err := c.store.ListWorkflowSteps(ctx, run.ID)
		if err != nil {
			return domain.WorkflowRun{}, domain.WorkflowStep{}, false, err
		}
		for _, step := range steps {
			if step.Kind != domain.WorkflowStepWork || step.SessionID == nil {
				continue
			}
			if strings.TrimSpace(*step.SessionID) == sessionID {
				return run, step, true, nil
			}
		}
	}
	return domain.WorkflowRun{}, domain.WorkflowStep{}, false, nil
}

// workReportWindowClosed reports whether this run has already resolved a review
// depth, which is the moment the report stopped being able to influence
// anything.
//
// An unreadable checkpoint list closes the window. That is the conservative
// direction here and it is worth saying why, because it is the opposite of the
// usual one: refusing a report costs a worker one advisory message, while
// accepting one AO cannot place in time would put an undated claim next to a
// decision and invite a reader to connect them.
func (c *Coordinator) workReportWindowClosed(ctx stdctx.Context, runID string) bool {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return true
	}
	for _, cp := range cps {
		if cp.DurablePhase == reviewDepthDecisionPhase {
			return true
		}
	}
	return false
}
