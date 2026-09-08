package workflow

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// pre_review_evidence.go — AO runs the task's own checks itself, once, before
// anybody decides how hard to review it.
//
// The gap this closes is structural. Review runs before Verify, so until now
// the only facts available when AO chose a review depth were the paths the
// change touched. "Does it work" was not among them, and could not be: nothing
// had run it. So `none` was unreachable for ordinary code, and every ordinary
// code change got a reviewer whether or not one was needed.
//
// The fix is not a new engine. Everything here is a reuse:
//
//   - the RUNTIME is VerifyRunner — the same, and the only, seam through which
//     verification is allowed to touch the host (verify.go). There is no second
//     executor, no shelling out from this file, and nothing here can run a
//     command Verify would not run;
//   - the SCOPE is ComputeVerifyScope + NarrowVerificationPlan, so a three-line
//     change does not run a repository suite, and a change ReviewPolicy flagged
//     as sensitive still does;
//   - the IDENTITY is verificationTargetKey over (fingerprint, narrowed plan) —
//     the same function Verify keys its attempts with, which is what lets the
//     two compare results without either trusting the other's bookkeeping;
//   - the DURABILITY is an append-only checkpoint on the review step, exactly
//     like review_policy_decision and review_depth_decision.
//
// Three properties are load-bearing, and each is a refusal:
//
//   - ONE execution per (step, target). The record is keyed by target and read
//     back before anything runs, so a restart, a re-entry, or a second
//     coordinator observing the same run re-reads the answer instead of
//     re-running the suite.
//   - The tree is fingerprinted BEFORE and AFTER. A worktree that moved while
//     the checks ran makes them describe a tree that no longer exists, and the
//     record says `unattributed` however green it was.
//   - Nothing here can produce an approval. It produces evidence. The most a
//     perfect result can buy is one step of review depth, and Verify still runs
//     afterwards on its own authority.

const (
	// preReviewEvidencePhase marks the durable record of one evidence pass.
	// Written once per (review step, target), before the depth decision that
	// reads it.
	preReviewEvidencePhase = "pre_review_evidence"
	// reviewEvidenceReliefPhase marks the durable record of whether that
	// evidence was allowed to lower this change's review floor, and why. It is
	// separate from the evidence itself because the evidence is a measurement
	// and the relief is a policy decision about it — and a policy that changes
	// must not appear to rewrite a measurement that did not.
	reviewEvidenceReliefPhase = "review_evidence_relief"
	// workReportPhase marks a worker's structured declaration. Append-only: a
	// worker that reports twice leaves both, and the newest is read.
	workReportPhase = "work_report"
	// reviewSkippedByEvidencePhase marks a review step completed WITHOUT a
	// reviewer because AO's own evidence and the deterministic policy allowed
	// it. It is deliberately NOT reviewPolicySkippedPhase: that phase means
	// "ReviewPolicy decided no reviewer was needed from the paths alone", and
	// conflating the two would make a decision taken on evidence indexable as
	// one taken without any.
	reviewSkippedByEvidencePhase = "review_skipped_by_evidence"
)

// preReviewEvidenceMaxCommands bounds how many commands one evidence pass will
// execute. A plan with more than this is not a bounded proportional check; it
// is a suite, and a suite belongs to Verify, which has the budget and the
// attempt accounting for it.
const preReviewEvidenceMaxCommands = 12

// preReviewEvidenceDefaultTimeout bounds one command that declared no timeout.
// Deliberately shorter than Verify's 10 minutes: this pass exists to be cheap,
// and a check that needs longer than this is one Verify should own.
const preReviewEvidenceDefaultTimeout = 3 * time.Minute

// collectPreReviewEvidence runs, records and returns the evidence for one
// review step, or re-reads it when it already exists.
//
// It is called at review cycle 1 only, from dispatchReviewStep, after
// EvaluateReviewPolicy and before the depth decision — the single point where
// AO already decides everything about a review exactly once.
//
// It never returns an error for a check that FAILED: a failing check is a
// successful measurement and the caller must see it. It returns an error only
// when the run itself should stop (a cancelled context, a store that will not
// write), because a record AO cannot persist is a decision AO cannot explain.
func (c *Coordinator) collectPreReviewEvidence(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	reviewStep domain.WorkflowStep,
	workCP domain.WorkflowCheckpoint,
	policyDecision ReviewPolicyDecision,
	artifact PlanArtifact,
) (domain.PreReviewEvidence, error) {
	started := c.clock()

	// The worker's declaration, read ONCE here so that every record this
	// function persists carries the same two facts about it: whether one
	// existed, and whether it contradicted what AO observed.
	//
	// It is stamped by this function rather than by the relief policy that
	// reads it, because this function is the only writer of an evidence row. A
	// contradiction computed after the row was written would never reach disk,
	// and a reader after a restart would see a report AO had already caught
	// lying and no record that it had.
	var report domain.WorkReport
	if workCP.WorkflowStepID != nil {
		if r, ok := c.workReportForStep(ctx, run.ID, *workCP.WorkflowStepID); ok {
			report = r
		}
	}
	// finish stamps those facts and writes the row. Every return path below
	// goes through it, so no status can accidentally omit them.
	finish := func(record domain.PreReviewEvidence) (domain.PreReviewEvidence, error) {
		record.ReportRecorded = report.Recorded()
		record.ReportContradicted = reportContradictsEvidence(report, record)
		if record.DecidedAt.IsZero() {
			record.DecidedAt = c.clock()
		}
		if record.DurationMS == 0 {
			record.DurationMS = record.DecidedAt.Sub(started).Milliseconds()
		}
		return c.persistPreReviewEvidence(ctx, run, reviewStep, record)
	}

	// A coordinator with no verification runtime cannot observe anything. That
	// is `unavailable`, recorded honestly, and it is the end of it: an absent
	// runtime must never read as "nothing to check".
	if c.verifier == nil || c.workspaceFacts == nil {
		return finish(domain.PreReviewEvidence{
			Version:   domain.PreReviewEvidenceVersion,
			Status:    domain.PreReviewEvidenceUnavailable,
			Note:      "no verification runtime is wired into this coordinator",
			StartedAt: started,
		})
	}
	if workCP.WorktreePath == "" || workCP.SessionID == nil {
		return finish(domain.PreReviewEvidence{
			Version:   domain.PreReviewEvidenceVersion,
			Status:    domain.PreReviewEvidenceUnavailable,
			Note:      "the work step recorded no worktree/session to run checks in",
			StartedAt: started,
		})
	}

	// The tree, before anything runs. This is the fingerprint the whole record
	// is about, and the one the relief policy compares against the review
	// target.
	obs, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
		Path:      workCP.WorktreePath,
		Branch:    workCP.Branch,
		SessionID: domain.SessionID(*workCP.SessionID),
		ProjectID: domain.ProjectID(run.ProjectID),
	})
	if err != nil {
		return finish(domain.PreReviewEvidence{
			Version:   domain.PreReviewEvidenceVersion,
			Status:    domain.PreReviewEvidenceUnavailable,
			Note:      "could not observe the worktree: " + err.Error(),
			StartedAt: started,
		})
	}
	fingerprint := WorkspaceFingerprint(obs)

	// A task that declares no verification at all is a distinct fact from one
	// whose plan AO refuses to run, and it is checked first so it says so. It
	// is not a failure and not a refusal: there was simply nothing to run, and
	// a change nobody can check automatically is exactly one a person should
	// look at.
	if len(artifact.Verification.Commands) == 0 && len(artifact.Verification.Files) == 0 {
		return finish(domain.PreReviewEvidence{
			Version:     domain.PreReviewEvidenceVersion,
			Status:      domain.PreReviewEvidenceNotPlanned,
			Note:        "this task declares no verification at all, so AO has nothing of its own to run",
			Fingerprint: fingerprint,
			StartedAt:   started,
		})
	}

	// The SAME gate Verify applies before it executes anything: the shared 8E
	// safety policy on every command, plus the workspace-escape and timeout
	// bounds. The production VerifyRunner validates each request again on its
	// own, so this is defence in depth rather than the only check — but a plan
	// AO would refuse to verify is a plan it must refuse to run early, and
	// saying so here makes the refusal explicit instead of a runtime error
	// surfacing later as an unexplained infrastructure failure.
	if err := artifact.Verification.validate(); err != nil {
		return finish(domain.PreReviewEvidence{
			Version:     domain.PreReviewEvidenceVersion,
			Status:      domain.PreReviewEvidenceUnavailable,
			Note:        "this task's verification plan is not one AO may run: " + err.Error(),
			Fingerprint: fingerprint,
			StartedAt:   started,
		})
	}

	// The same narrowing Verify applies, from the same inputs, so the plan that
	// runs here is the plan that would run there. Deriving it from the decision
	// AO just persisted rather than re-deriving risk keeps the "one risk
	// evaluation" rule of review_policy.go §6.
	scopeDecision := ComputeVerifyScope(policyDecision.Reasons, c.isFinalIntegrationTask(ctx, run), workspaceChangedPaths(obs))
	narrowedPlan, transforms := NarrowVerificationPlan(artifact.Verification, scopeDecision)
	targetKey := verificationTargetKey(fingerprint, narrowedPlan)

	record := domain.PreReviewEvidence{
		Version:            domain.PreReviewEvidenceVersion,
		Fingerprint:        fingerprint,
		TargetKey:          targetKey,
		PlanCommandCount:   len(narrowedPlan.Commands),
		PlanFileCheckCount: len(narrowedPlan.Files),
		StartedAt:          started,
		Scope: &domain.VerifyScopeSummary{
			PolicyVersion: scopeDecision.PolicyVersion,
			Scope:         string(scopeDecision.Scope),
			Reasons:       scopeReasonStrings(scopeDecision),
			Transforms:    transforms,
		},
	}

	// ALREADY ANSWERED? A record for this exact target is this exact question,
	// already measured. Re-running it would cost the suite again and could
	// return a different answer for the same tree, which is the one thing a
	// durable record exists to prevent.
	if existing, ok := c.preReviewEvidenceForTarget(ctx, run.ID, reviewStep.ID, targetKey); ok {
		return existing, nil
	}

	if len(narrowedPlan.Commands) == 0 {
		record.Status = domain.PreReviewEvidenceNotPlanned
		record.Note = "this task declares no executable verification, so AO has nothing of its own to run"
		return finish(record)
	}
	if len(narrowedPlan.Commands) > preReviewEvidenceMaxCommands {
		record.Status = domain.PreReviewEvidenceUnavailable
		record.Note = fmt.Sprintf("this task plans %d commands, more than the %d a bounded pre-review pass runs; Verify owns a suite this size",
			len(narrowedPlan.Commands), preReviewEvidenceMaxCommands)
		return finish(record)
	}

	status := domain.PreReviewEvidenceObserved
	for _, planned := range narrowedPlan.Commands {
		check := planned
		if resolved, resolution, resolveErr := resolveVerifyCommandContext(workCP.WorktreePath, check, false); resolveErr == nil && resolution != nil {
			check = resolved
		}
		dir, pathErr := secureWorktreePath(workCP.WorktreePath, check.WorkingDirectory)
		if pathErr != nil {
			record.Status = domain.PreReviewEvidenceUnavailable
			record.Note = "could not resolve a command's working directory: " + pathErr.Error()
			record.DecidedAt = c.clock()
			record.DurationMS = record.DecidedAt.Sub(started).Milliseconds()
			return c.persistPreReviewEvidence(ctx, run, reviewStep, record)
		}
		timeout := time.Duration(check.TimeoutSeconds) * time.Second
		if timeout <= 0 || timeout > preReviewEvidenceDefaultTimeout {
			timeout = preReviewEvidenceDefaultTimeout
		}

		exec, runErr := c.verifier.Run(ctx, VerifyCommandRequest{
			Command: check.Command, Args: check.Args, Directory: dir, Timeout: timeout,
		})
		// A cancelled run is not a measurement. Persist nothing and let the
		// caller unwind: a record written here would outlive the cancellation
		// and be read later as an answer nobody finished asking.
		if errors.Is(runErr, stdctx.Canceled) || errors.Is(ctx.Err(), stdctx.Canceled) {
			return domain.PreReviewEvidence{}, runErr
		}

		exitCode := exec.ExitCode
		cr := domain.PreReviewCheckRecord{
			Kind:             "command",
			Label:            commandLabel(check),
			Command:          strings.TrimSpace(check.Command + " " + strings.Join(check.Args, " ")),
			Directory:        normalizeRel(check.WorkingDirectory),
			ExitCode:         &exitCode,
			RequiredExitCode: check.RequiredExitCode,
			DurationMS:       exec.DurationMS,
			StdoutTail:       exec.StdoutTail,
			StderrTail:       exec.StderrTail,
			TimedOut:         exec.TimedOut,
		}
		record.ExecutedCommandCount++
		switch {
		case exec.TimedOut:
			cr.FailureReason = "command timed out"
			status = domain.DeeperEvidenceFailure(status, domain.PreReviewEvidenceTimedOut)
		case runErr != nil:
			cr.FailureReason = runErr.Error()
			// The runtime could not run the command. That is AO's own
			// infrastructure, not a verdict about the code, and it is recorded
			// as unavailable rather than as a failure the worker caused.
			status = domain.DeeperEvidenceFailure(status, domain.PreReviewEvidenceUnavailable)
		case exitCode != check.RequiredExitCode:
			cr.FailureReason = fmt.Sprintf("exit code %d, required %d", exitCode, check.RequiredExitCode)
			status = domain.DeeperEvidenceFailure(status, domain.PreReviewEvidenceFailed)
		default:
			cr.Passed = true
		}
		record.Checks = append(record.Checks, cr)
	}

	// The tree, after everything ran. A change here means the checks measured
	// something that is no longer in front of the reviewer.
	record.FingerprintAfter = fingerprint
	if after, afterErr := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
		Path:      workCP.WorktreePath,
		Branch:    workCP.Branch,
		SessionID: domain.SessionID(*workCP.SessionID),
		ProjectID: domain.ProjectID(run.ProjectID),
	}); afterErr == nil {
		record.FingerprintAfter = WorkspaceFingerprint(after)
		if record.FingerprintAfter != fingerprint {
			status = domain.PreReviewEvidenceUnattributed
			record.Note = "the worktree changed while AO was running the checks, so their results are not about the tree under review"
		}
	} else {
		// AO cannot prove the tree held still. That is not a pass.
		status = domain.PreReviewEvidenceUnattributed
		record.FingerprintAfter = ""
		record.Note = "could not re-observe the worktree after the checks, so AO cannot attribute their results to the reviewed tree"
	}

	record.Status = status
	return finish(record)
}

// scopeReasonStrings flattens the scope decision's reason vocabulary so the
// durable record stays free of internal/workflow types.
func scopeReasonStrings(d VerifyScopeDecision) []string {
	if len(d.Reasons) == 0 {
		return nil
	}
	out := make([]string, 0, len(d.Reasons))
	for _, r := range d.Reasons {
		out = append(out, string(r))
	}
	return out
}

// persistPreReviewEvidence writes one evidence record as its own checkpoint,
// following persistReviewPolicyDecision exactly.
func (c *Coordinator) persistPreReviewEvidence(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	reviewStep domain.WorkflowStep,
	record domain.PreReviewEvidence,
) (domain.PreReviewEvidence, error) {
	if record.DecidedAt.IsZero() {
		record.DecidedAt = c.clock()
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return record, err
	}
	stepID := reviewStep.ID
	_, err = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		RetryState:     string(payload),
		NextAction:     "pre_review_evidence: " + string(record.Status),
		DurablePhase:   preReviewEvidencePhase,
		PayloadVersion: domain.PreReviewEvidenceVersion,
		CreatedAt:      record.DecidedAt,
	})
	return record, err
}

// preReviewEvidenceForTarget re-reads the evidence recorded for one review step
// AND one exact target. The target match is what makes this a cache rather than
// a guess: a record for a different tree or a different plan answers a
// different question and is ignored.
func (c *Coordinator) preReviewEvidenceForTarget(ctx stdctx.Context, runID, reviewStepID, targetKey string) (domain.PreReviewEvidence, bool) {
	record, ok := c.preReviewEvidenceForStep(ctx, runID, reviewStepID)
	if !ok || record.TargetKey != targetKey || targetKey == "" {
		return domain.PreReviewEvidence{}, false
	}
	return record, true
}

// preReviewEvidenceForStep returns the newest evidence record for a review
// step, whatever target it was taken at. Returns ok=false when there is none or
// when it cannot be decoded — never a zero record that a caller might read as
// "nothing to see".
func (c *Coordinator) preReviewEvidenceForStep(ctx stdctx.Context, runID, reviewStepID string) (domain.PreReviewEvidence, bool) {
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return domain.PreReviewEvidence{}, false
	}
	var latest *domain.WorkflowCheckpoint
	for i := range checkpoints {
		cp := &checkpoints[i]
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != reviewStepID || cp.DurablePhase != preReviewEvidencePhase {
			continue
		}
		if latest == nil || cp.CreatedAt.After(latest.CreatedAt) {
			latest = cp
		}
	}
	if latest == nil {
		return domain.PreReviewEvidence{}, false
	}
	var record domain.PreReviewEvidence
	if err := json.Unmarshal([]byte(latest.RetryState), &record); err != nil || !record.Recorded() {
		return domain.PreReviewEvidence{}, false
	}
	return record, true
}

// DecodePreReviewEvidenceForTest exposes the decode path to the external
// workflow_test package, mirroring DecodeReviewPolicyDecisionForTest.
func DecodePreReviewEvidenceForTest(retryState string) (domain.PreReviewEvidence, bool) {
	var record domain.PreReviewEvidence
	if err := json.Unmarshal([]byte(retryState), &record); err != nil || !record.Recorded() {
		return domain.PreReviewEvidence{}, false
	}
	return record, true
}

// reusablePreReviewEvidence returns the pre-review command results a
// verification of (fingerprint, narrowedPlan) may replay instead of executing.
//
// Everything about it is a refusal except the one case it exists for:
//
//   - the evidence must be for the IDENTICAL target — same tree, same narrowed
//     plan — proven by verificationTargetKey, the same function Verify keys its
//     own attempts with. A fix cycle moves the fingerprint and the map is empty;
//   - the evidence must be `observed`. A pass AO could not attribute, a
//     timeout, or a run on an unavailable runtime replays nothing;
//   - only PASSING checks are replayed. A failure is deliberately re-run by
//     Verify, because a failing verification is the entry point to infra
//     classification, transient retries and the fix cycle, and short-circuiting
//     into that machinery with a borrowed result would skip all three.
//
// The caller has already proven, through the review-authority check, that the
// fingerprint it passes is the approved target. This function therefore never
// widens what Verify is allowed to certify; it only avoids paying twice for the
// same answer about it.
func (c *Coordinator) reusablePreReviewEvidence(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	reviewStep domain.WorkflowStep,
	fingerprint string,
	narrowedPlan VerificationPlan,
) map[string]VerifyCheckResult {
	if fingerprint == "" || len(narrowedPlan.Commands) == 0 {
		return nil
	}
	record, ok := c.preReviewEvidenceForTarget(ctx, run.ID, reviewStep.ID, verificationTargetKey(fingerprint, narrowedPlan))
	if !ok || !record.Status.Observed() {
		return nil
	}
	out := make(map[string]VerifyCheckResult, len(record.Checks))
	for _, rec := range record.Checks {
		if !rec.Passed || rec.Label == "" {
			continue
		}
		exit := 0
		if rec.ExitCode != nil {
			exit = *rec.ExitCode
		}
		out[rec.Label] = VerifyCheckResult{
			Kind:       rec.Kind,
			Label:      rec.Label,
			Passed:     true,
			ExitCode:   &exit,
			DurationMS: rec.DurationMS,
			StdoutTail: rec.StdoutTail,
			StderrTail: rec.StderrTail,
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
