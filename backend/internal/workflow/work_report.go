package workflow

import (
	stdctx "context"
	"encoding/json"
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
	stepID := workStep.ID
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
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

// workReportForStep re-reads the newest report recorded for a work step.
// ok=false means there is none or it cannot be decoded — never an empty report,
// which a caller could mistake for "the worker said nothing was wrong".
func (c *Coordinator) workReportForStep(ctx stdctx.Context, runID, workStepID string) (domain.WorkReport, bool) {
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return domain.WorkReport{}, false
	}
	var latest *domain.WorkflowCheckpoint
	for i := range checkpoints {
		cp := &checkpoints[i]
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != workStepID || cp.DurablePhase != workReportPhase {
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

// workStepForRun returns a run's work step. A small helper rather than an
// inline loop because three call sites in phase 2 need the same lookup and each
// one must treat "not found" as a refusal rather than as an empty step.
func (c *Coordinator) workStepForRun(ctx stdctx.Context, runID string) (domain.WorkflowStep, bool) {
	steps, err := c.store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		return domain.WorkflowStep{}, false
	}
	for i := range steps {
		if steps[i].Kind == domain.WorkflowStepWork {
			return steps[i], true
		}
	}
	return domain.WorkflowStep{}, false
}
