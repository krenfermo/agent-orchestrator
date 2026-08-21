package workflow

import (
	stdctx "context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const verifyResultVersion = "v1"

type VerifyCommandRequest struct {
	Command   string
	Args      []string
	Directory string
	Timeout   time.Duration
}

type VerifyCommandExecution struct {
	ExitCode   int    `json:"exitCode"`
	DurationMS int64  `json:"durationMs"`
	StdoutTail string `json:"stdoutTail,omitempty"`
	StderrTail string `json:"stderrTail,omitempty"`
	TimedOut   bool   `json:"timedOut,omitempty"`
}

type VerifyRunner interface {
	Run(ctx stdctx.Context, req VerifyCommandRequest) (VerifyCommandExecution, error)
}

type VerifyCheckResult struct {
	Kind          string `json:"kind"`
	Label         string `json:"label"`
	Passed        bool   `json:"passed"`
	ExitCode      *int   `json:"exitCode,omitempty"`
	DurationMS    int64  `json:"durationMs,omitempty"`
	StdoutTail    string `json:"stdoutTail,omitempty"`
	StderrTail    string `json:"stderrTail,omitempty"`
	FailureReason string `json:"failureReason,omitempty"`
}

type VerifyResult struct {
	Version             string                    `json:"version"`
	TargetKey           string                    `json:"targetKey"`
	ReviewedFingerprint string                    `json:"reviewedFingerprint"`
	PreFingerprint      string                    `json:"preFingerprint"`
	PostFingerprint     string                    `json:"postFingerprint"`
	Checks              []VerifyCheckResult       `json:"checks"`
	Passed              bool                      `json:"passed"`
	ErrorClass          domain.WorkflowErrorClass `json:"errorClass,omitempty"`
	// Scope is Checkpoint 8I's VerifyScopePolicy decision for this attempt —
	// always recorded, even when the scope resolved to repository (i.e. no
	// narrowing applied), so a run can later explain exactly which commands
	// actually executed and why.
	Scope *VerifyScopeDecision `json:"scope,omitempty"`
	// ScopeAppliedTransforms lists each command rewrite VerifyScopePolicy
	// actually applied (e.g. "go test ./... -> go test ./internal/foo/...").
	// Empty when scope is repository or no recognizable command qualified.
	ScopeAppliedTransforms []string `json:"scopeAppliedTransforms,omitempty"`
}

func verificationTargetKey(fingerprint string, plan VerificationPlan) string {
	b, _ := json.Marshal(plan)
	sum := sha256.Sum256(append([]byte(fingerprint+"\n"), b...))
	return hex.EncodeToString(sum[:])
}

func verifyAttemptID(stepID, targetKey string) string {
	sum := sha256.Sum256([]byte(stepID + "\n" + targetKey))
	return "wfa-verify-" + hex.EncodeToString(sum[:12])
}

func (p VerificationPlan) allCommandsRetrySafe() bool {
	for _, check := range p.Commands {
		if !check.RetrySafe {
			return false
		}
	}
	return true
}

func (p VerificationPlan) validate() error {
	if len(p.Commands) == 0 && len(p.Files) == 0 {
		return fmt.Errorf("%w: verify requires at least one structured check", ErrInvalid)
	}
	for _, check := range p.Commands {
		if strings.TrimSpace(check.Command) == "" {
			return fmt.Errorf("%w: verify command is required", ErrInvalid)
		}
		workingDirectory := strings.TrimSpace(check.WorkingDirectory)
		if workingDirectory != "" {
			clean := filepath.Clean(workingDirectory)
			if filepath.IsAbs(workingDirectory) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
				return fmt.Errorf("%w: verify working directory must stay inside the workspace", ErrInvalid)
			}
		}
		if check.TimeoutSeconds < 0 || check.TimeoutSeconds > 3600 {
			return fmt.Errorf("%w: verify timeout must be between 0 and 3600 seconds", ErrInvalid)
		}
		if err := ValidateVerifyCommand(check.Command, check.Args); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	}
	for _, check := range p.Files {
		if strings.TrimSpace(check.Path) == "" {
			return fmt.Errorf("%w: verify file path is required", ErrInvalid)
		}
		clean := filepath.Clean(check.Path)
		if filepath.IsAbs(check.Path) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%w: verify file path must stay inside the workspace", ErrInvalid)
		}
		if check.SHA256 != "" {
			if len(check.SHA256) != 64 {
				return fmt.Errorf("%w: verify file sha256 must contain 64 hex characters", ErrInvalid)
			}
			if _, err := hex.DecodeString(check.SHA256); err != nil {
				return fmt.Errorf("%w: invalid verify file sha256", ErrInvalid)
			}
		}
	}
	return nil
}

// ValidateVerifyCommand is the shared 8E safety policy used both before a
// generated plan is accepted and immediately before process execution.
func ValidateVerifyCommand(name string, args []string) error {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(name)))
	switch base {
	case "sh", "bash", "zsh", "fish", "cmd", "cmd.exe", "powershell", "pwsh", "rm", "rmdir", "del", "deploy", "terraform", "kubectl", "helm":
		return fmt.Errorf("verify command %q is not allowed", base)
	}
	if base == "git" {
		for _, arg := range args {
			switch strings.ToLower(arg) {
			case "push", "merge", "reset", "clean", "checkout", "switch", "commit", "rebase":
				return fmt.Errorf("verify git subcommand %q is not allowed", arg)
			}
		}
	}
	return nil
}

// maybeVerify executes the approved fingerprint's structured, local checks.
// The durable started checkpoint precedes process execution. An unfinished
// attempt is reused after restart and is only re-executed when every command
// was explicitly declared retry-safe.
func (c *Coordinator) maybeVerify(ctx stdctx.Context, run domain.WorkflowRun, workStep, reviewStep, verifyStep domain.WorkflowStep) (domain.WorkflowRun, domain.WorkflowStep, error) {
	if run.State.Terminal() || reviewStep.State != domain.WorkflowStepCompleted || verifyStep.State.Terminal() || c.verifier == nil || c.workspaceFacts == nil || c.reviewRuns == nil {
		return run, verifyStep, nil
	}
	// Checkpoint 8K-A: never start verification while this step (or the
	// run) has an unresolved question open.
	if open, err := c.hasOpenQuestion(ctx, run.ID, &verifyStep.ID); err != nil {
		return run, verifyStep, err
	} else if open {
		return run, verifyStep, nil
	}
	workCP, hasCP, err := c.store.GetLatestWorkflowCheckpointByStep(ctx, workStep.ID)
	if err != nil {
		return run, verifyStep, err
	}
	if !hasCP || workCP.WorktreePath == "" || workCP.SessionID == nil {
		return c.failVerifyWithoutExecution(ctx, run, verifyStep, domain.WorkflowErrorVerifyEnvironment, "verify worktree/session facts are missing")
	}

	// Checkpoint 8I: a review step can reach "completed" two ways —
	// approved by a real Claude reviewer (reviewRun.Verdict ==
	// VerdictApproved), or advanced directly by ReviewPolicy's SKIPPED path
	// (applyReviewPolicySkip, review_dispatch.go), which never creates a
	// review_run at all. Verify must treat both as equally eligible to run
	// — "Reviewed: No — policy skipped" still means the work is ready to be
	// verified, it only means Claude never looked at it — but must derive
	// "reviewed" from a different source in the SKIPPED case: the work
	// step's own completion fingerprint, the same value dispatchReviewStep
	// would have used as target_sha had a reviewer actually run.
	var reviewed string
	if reviewStep.ReviewRunID != nil {
		reviewRun, ok, err := c.reviewRuns.GetReviewRun(ctx, *reviewStep.ReviewRunID)
		if err != nil || !ok {
			return run, verifyStep, err
		}
		if reviewRun.Verdict != domain.VerdictApproved {
			return run, verifyStep, nil
		}
		reviewed = reviewRun.TargetSHA
	} else {
		decision, ok := c.reviewPolicyDecisionForStep(ctx, run.ID, reviewStep.ID)
		if !ok || decision.Decision != ReviewSkipped {
			// No review_run AND no recorded SKIPPED decision: an ambiguous
			// state Verify must not guess about (mirrors every other
			// "cannot durably prove" branch in this package).
			return run, verifyStep, nil
		}
		reviewed = workCP.FingerprintAfter
		if reviewed == "" {
			reviewed = workCP.HeadSHA
		}
	}

	// Checkpoint 8P-E.13 Phase 5: after a verify-driven fix cycle, the approved
	// review's target SHA is no longer the fingerprint that must be verified —
	// the fix deliberately changed the worktree. The new target is the
	// fingerprint the fix actually delivered, taken from the fix step's own
	// observation checkpoint. Without this the very next verification would
	// fail with verify_workspace_changed, which is how "fix it and try again"
	// would have quietly become an infinite, always-failing loop.
	if delivered, ok := c.verifyTargetAfterFix(ctx, run.ID); ok {
		reviewed = delivered
	}

	artifact, err := c.planArtifactForRun(ctx, run)
	if err != nil {
		return run, verifyStep, err
	}
	if err := artifact.Verification.validate(); err != nil {
		return c.failVerifyWithoutExecution(ctx, run, verifyStep, domain.WorkflowErrorVerifyAmbiguous, err.Error())
	}
	targetKey := verificationTargetKey(reviewed, artifact.Verification)

	latest, hasAttempt, err := c.store.GetLatestWorkflowAttempt(ctx, verifyStep.ID)
	if err != nil {
		return run, verifyStep, err
	}
	if hasAttempt && latest.Model != targetKey {
		// A target change is normally the ambiguity this guard exists to
		// catch — except when AO itself authorized it by running a
		// verify-driven fix cycle. In that case the prior attempt is simply
		// finished history for a superseded target, and this cycle gets its own
		// attempt row (verifyAttemptID is derived from the target key, so the
		// two can never collide).
		if !c.verifyTargetAdvancedByFix(ctx, run.ID, latest) {
			return c.failVerifyWithoutExecution(ctx, run, verifyStep, domain.WorkflowErrorVerifyAmbiguous, "verify target changed after an attempt was created")
		}
		hasAttempt = false
		latest = domain.WorkflowAttempt{}
	}
	if hasAttempt && latest.Outcome == "" {
		if cp, found, cpErr := c.store.GetLatestWorkflowCheckpointByStep(ctx, verifyStep.ID); cpErr != nil {
			return run, verifyStep, cpErr
		} else if found && cp.DurablePhase == "verify_result" && cp.AttemptID != nil && *cp.AttemptID == latest.ID {
			var recovered VerifyResult
			if json.Unmarshal([]byte(cp.RetryState), &recovered) != nil {
				return c.finishVerifyFailure(ctx, run, verifyStep, latest, VerifyResult{Version: verifyResultVersion, TargetKey: targetKey, ErrorClass: domain.WorkflowErrorVerifyAmbiguous}, "persisted verify result is unreadable")
			}
			if recovered.Passed {
				if err := c.store.UpdateWorkflowAttemptOutcome(ctx, latest.ID, c.clock(), domain.WorkflowAttemptSucceeded, ""); err != nil {
					return run, verifyStep, err
				}
				return c.completeVerifiedRun(ctx, run, verifyStep)
			}
			return c.finishVerifyFailure(ctx, run, verifyStep, latest, recovered, "recovered persisted verify failure")
		}
	}
	if hasAttempt && latest.Outcome != "" {
		if latest.Outcome == domain.WorkflowAttemptSucceeded {
			return c.completeVerifiedRun(ctx, run, verifyStep)
		}
		return run, verifyStep, nil
	}
	if hasAttempt && !artifact.Verification.allCommandsRetrySafe() {
		return c.finishVerifyFailure(ctx, run, verifyStep, latest, VerifyResult{Version: verifyResultVersion, TargetKey: targetKey, ReviewedFingerprint: reviewed, ErrorClass: domain.WorkflowErrorVerifyAmbiguous}, "interrupted verify contains a command not declared retry-safe")
	}
	if hasAttempt && verifyStep.State == domain.WorkflowStepWaiting {
		if _, err := c.store.UpdateWorkflowStepState(ctx, verifyStep.ID, domain.WorkflowStepWaiting, domain.WorkflowStepRunning, c.clock()); err != nil {
			return run, verifyStep, err
		}
		verifyStep.State = domain.WorkflowStepRunning
	}
	if run.State == domain.WorkflowRunWaiting {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, domain.WorkflowRunWaiting, domain.WorkflowRunRunning, c.clock()); err != nil {
			return run, verifyStep, err
		}
		run.State = domain.WorkflowRunRunning
	}

	now := c.clock()
	if !hasAttempt {
		attemptID := verifyAttemptID(verifyStep.ID, targetKey)
		latest, err = c.store.CreateWorkflowAttempt(ctx, attemptID, verifyStep.ID, "local-verify", targetKey, now)
		if err != nil {
			return run, verifyStep, err
		}
		if verifyStep.State == domain.WorkflowStepPending {
			if _, err = c.store.UpdateWorkflowStepState(ctx, verifyStep.ID, domain.WorkflowStepPending, domain.WorkflowStepReady, now); err != nil {
				return run, verifyStep, err
			}
			verifyStep.State = domain.WorkflowStepReady
		}
		if verifyStep.State == domain.WorkflowStepReady || verifyStep.State == domain.WorkflowStepWaiting {
			if _, err = c.store.UpdateWorkflowStepState(ctx, verifyStep.ID, verifyStep.State, domain.WorkflowStepRunning, now); err != nil {
				return run, verifyStep, err
			}
			verifyStep.State = domain.WorkflowStepRunning
		}
		stepID, attemptID := verifyStep.ID, latest.ID
		reviewVerdictLabel := ""
		if reviewStep.ReviewRunID != nil {
			reviewVerdictLabel = string(domain.VerdictApproved)
		}
		stateJSON, _ := json.Marshal(map[string]any{"targetKey": targetKey, "verification": artifact.Verification})
		_, err = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{ID: "wfc-" + c.newID(), WorkflowRunID: run.ID, WorkflowStepID: &stepID, AttemptID: &attemptID, ProjectID: run.ProjectID, SessionID: workCP.SessionID, Branch: workCP.Branch, WorktreePath: workCP.WorktreePath, ReviewRunID: reviewStep.ReviewRunID, ReviewVerdict: reviewVerdictLabel, RetryState: string(stateJSON), NextAction: "verify", DurablePhase: "verify_started", PayloadVersion: verifyResultVersion, FingerprintBefore: reviewed, CreatedAt: now})
		if err != nil {
			return run, verifyStep, err
		}
	}

	obs, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{Path: workCP.WorktreePath, Branch: workCP.Branch, SessionID: domain.SessionID(*workCP.SessionID), ProjectID: domain.ProjectID(run.ProjectID)})
	if err != nil {
		return c.finishVerifyFailure(ctx, run, verifyStep, latest, VerifyResult{Version: verifyResultVersion, TargetKey: targetKey, ReviewedFingerprint: reviewed, ErrorClass: domain.WorkflowErrorVerifyEnvironment}, err.Error())
	}
	pre := WorkspaceFingerprint(obs)
	result := VerifyResult{Version: verifyResultVersion, TargetKey: targetKey, ReviewedFingerprint: reviewed, PreFingerprint: pre, Checks: []VerifyCheckResult{}}
	if pre != reviewed {
		result.ErrorClass = domain.WorkflowErrorVerifyWorkspaceChanged
		return c.finishVerifyFailure(ctx, run, verifyStep, latest, result, "workspace fingerprint no longer matches the approved review target")
	}

	// Checkpoint 8I: VerifyScopePolicy narrows the Planner's commands (when
	// safely recognizable) BEFORE execution. reviewReasons/finalIntegration
	// come from data already durable elsewhere (the review step's own
	// policy-decision checkpoint, and the plan's dependency graph) — never a
	// new LLM call. A lookup failure defaults conservatively to repository
	// scope (i.e. the Planner's commands run completely unmodified).
	reviewReasons, _ := c.reviewPolicyReasonsForStep(ctx, run.ID, reviewStep.ID)
	finalIntegration := c.isFinalIntegrationTask(ctx, run)
	scopeDecision := ComputeVerifyScope(reviewReasons, finalIntegration, workspaceChangedPaths(obs))
	narrowedPlan, applied := NarrowVerificationPlan(artifact.Verification, scopeDecision)
	result.Scope = &scopeDecision
	result.ScopeAppliedTransforms = applied

	for _, check := range narrowedPlan.Commands {
		dir, pathErr := secureWorktreePath(workCP.WorktreePath, check.WorkingDirectory)
		if pathErr != nil {
			result.ErrorClass = domain.WorkflowErrorVerifyEnvironment
			return c.finishVerifyFailure(ctx, run, verifyStep, latest, result, pathErr.Error())
		}
		timeout := time.Duration(check.TimeoutSeconds) * time.Second
		if timeout == 0 {
			timeout = 10 * time.Minute
		}
		execResult, runErr := c.verifier.Run(ctx, VerifyCommandRequest{Command: check.Command, Args: check.Args, Directory: dir, Timeout: timeout})
		if errors.Is(runErr, stdctx.Canceled) {
			return run, verifyStep, runErr
		}
		exitCode := execResult.ExitCode
		cr := VerifyCheckResult{Kind: "command", Label: strings.Join(append([]string{check.Command}, check.Args...), " "), ExitCode: &exitCode, DurationMS: execResult.DurationMS, StdoutTail: execResult.StdoutTail, StderrTail: execResult.StderrTail}
		switch {
		case execResult.TimedOut:
			result.ErrorClass = domain.WorkflowErrorVerifyTimeout
			cr.FailureReason = "command timed out"
		case runErr != nil:
			result.ErrorClass = domain.WorkflowErrorVerifyEnvironment
			cr.FailureReason = runErr.Error()
		case exitCode != check.RequiredExitCode:
			result.ErrorClass = domain.WorkflowErrorVerifyCommandFailed
			cr.FailureReason = fmt.Sprintf("exit code %d, required %d", exitCode, check.RequiredExitCode)
		default:
			cr.Passed = true
		}
		result.Checks = append(result.Checks, cr)
		if !cr.Passed {
			return c.finishVerifyFailure(ctx, run, verifyStep, latest, result, cr.FailureReason)
		}
	}
	for _, check := range artifact.Verification.Files {
		cr, class := verifyFile(workCP.WorktreePath, check)
		result.Checks = append(result.Checks, cr)
		if !cr.Passed {
			result.ErrorClass = class
			return c.finishVerifyFailure(ctx, run, verifyStep, latest, result, cr.FailureReason)
		}
	}
	postObs, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{Path: workCP.WorktreePath, Branch: workCP.Branch, SessionID: domain.SessionID(*workCP.SessionID), ProjectID: domain.ProjectID(run.ProjectID)})
	if err != nil {
		result.ErrorClass = domain.WorkflowErrorVerifyEnvironment
		return c.finishVerifyFailure(ctx, run, verifyStep, latest, result, err.Error())
	}
	result.PostFingerprint = WorkspaceFingerprint(postObs)
	if result.PostFingerprint != pre {
		result.ErrorClass = domain.WorkflowErrorVerifyWorkspaceChanged
		return c.finishVerifyFailure(ctx, run, verifyStep, latest, result, "workspace changed while verification was running")
	}
	result.Passed = true
	if err := c.persistVerifyResult(ctx, run, verifyStep, latest, result, "done"); err != nil {
		return run, verifyStep, err
	}
	if err := c.store.UpdateWorkflowAttemptOutcome(ctx, latest.ID, c.clock(), domain.WorkflowAttemptSucceeded, ""); err != nil {
		return run, verifyStep, err
	}
	return c.completeVerifiedRun(ctx, run, verifyStep)
}

// verifyTargetAfterFix returns the workspace fingerprint a verify-driven fix
// cycle delivered, when the run's durable timeline shows one: a
// verify_fix_reentry checkpoint followed by a fix observation that recorded a
// new fingerprint. Returns ok=false when no verify-driven fix has happened, so
// the ordinary path (verify against the approved review's target) is untouched.
//
// Reading the timeline rather than storing a "current verify target" column is
// deliberate: both checkpoints already exist, they are append-only, and
// deriving from them means a restart in the middle of this cycle recovers the
// same answer without a migration.
func (c *Coordinator) verifyTargetAfterFix(ctx stdctx.Context, runID string) (string, bool) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return "", false
	}
	var reentryAt time.Time
	var delivered string
	var deliveredAt time.Time
	for _, cp := range cps {
		switch cp.DurablePhase {
		case ReasonVerifyFixReentry:
			if cp.CreatedAt.After(reentryAt) {
				reentryAt = cp.CreatedAt
			}
		case "fix_observed_" + string(domain.WorkflowStepWaiting):
			if cp.FingerprintAfter != "" && cp.CreatedAt.After(deliveredAt) {
				delivered, deliveredAt = cp.FingerprintAfter, cp.CreatedAt
			}
		}
	}
	if reentryAt.IsZero() || delivered == "" || !deliveredAt.After(reentryAt) {
		return "", false
	}
	return delivered, true
}

// verifyTargetAdvancedByFix reports whether a verify-driven fix cycle was
// authorized after the given attempt started — the one legitimate reason the
// verification target may differ from the one that attempt recorded.
func (c *Coordinator) verifyTargetAdvancedByFix(ctx stdctx.Context, runID string, attempt domain.WorkflowAttempt) bool {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return false
	}
	for _, cp := range cps {
		if cp.DurablePhase == ReasonVerifyFixReentry && !cp.CreatedAt.Before(attempt.StartedAt) {
			return true
		}
	}
	return false
}

// maybeDispatchVerifyFix is the dispatch half of Phase 5's verify->fix
// re-entry: when the run's newest verify record is a verify_fix_reentry and the
// fix step is resting, it sends the failed verification's findings to the same
// worker session the review/fix loop already uses.
//
// It reuses dispatchFixStep unmodified — same outbox idempotency key shape,
// same attempt-count guard, same session — so a verify-driven fix is not a
// second, parallel fix mechanism that could drift from the review-driven one.
// The cycle number is simply the next fix attempt number, which keeps both
// kinds of fix cycle on one honest counter.
func (c *Coordinator) maybeDispatchVerifyFix(ctx stdctx.Context, run domain.WorkflowRun, workStep, fixStep, reviewStep, verifyStep domain.WorkflowStep) (domain.WorkflowStep, error) {
	// A terminally failed verify step is the unrepairable branch's outcome: the
	// run has already stopped for a reason no fix cycle addresses, and there is
	// nothing left to re-verify even if a fix landed.
	if run.State.Terminal() || fixStep.State.Terminal() || verifyStep.State.Terminal() ||
		c.reviewRuns == nil || reviewStep.ReviewRunID == nil {
		return fixStep, nil
	}
	if fixStep.State != domain.WorkflowStepWaiting && fixStep.State != domain.WorkflowStepPending {
		return fixStep, nil
	}
	cps, err := c.store.ListWorkflowCheckpoints(ctx, run.ID)
	if err != nil {
		return fixStep, err
	}
	var cp domain.WorkflowCheckpoint
	var found bool
	for _, candidate := range cps {
		if candidate.DurablePhase == ReasonVerifyFixReentry && (!found || candidate.CreatedAt.After(cp.CreatedAt)) {
			cp, found = candidate, true
		}
	}
	if !found {
		return fixStep, nil
	}

	// Idempotency: dispatch at most one fix per re-entry. Keyed on the fix
	// step's own attempt rows rather than on "is this the newest checkpoint",
	// so a later checkpoint written by any other observer cannot mask a
	// re-entry that has not been answered yet — and a re-entry that HAS been
	// answered can never be answered twice however often this is re-entered.
	attempts, err := c.store.ListWorkflowAttempts(ctx, fixStep.ID)
	if err != nil {
		return fixStep, err
	}
	for _, a := range attempts {
		if !a.StartedAt.Before(cp.CreatedAt) {
			return fixStep, nil
		}
	}

	reviewRun, ok, err := c.reviewRuns.GetReviewRun(ctx, *reviewStep.ReviewRunID)
	if err != nil || !ok {
		return fixStep, err
	}

	var result VerifyResult
	_ = json.Unmarshal([]byte(cp.RetryState), &result)
	cycleNumber := len(attempts) + 1

	artifact, err := c.planArtifactForRun(ctx, run)
	if err != nil {
		return fixStep, err
	}
	prompt := BuildFixPrompt(FixPromptInput{
		Objective:          run.Objective,
		AcceptanceCriteria: artifact.AcceptanceCriteria,
		ReviewRunID:        reviewRun.ID,
		Findings:           renderVerifyFindings(result),
		CycleNumber:        cycleNumber,
	})
	return c.dispatchFixStep(ctx, run, workStep, fixStep, reviewRun, cycleNumber, prompt)
}

// renderVerifyFindings turns a failed VerifyResult into the findings text a fix
// worker receives. It reports only what actually ran and what it printed —
// never a summary AO invented about why the check failed.
func renderVerifyFindings(result VerifyResult) string {
	var b strings.Builder
	b.WriteString("Local verification failed after your work was approved by review.\n")
	b.WriteString("Fix the cause so verification passes. Do not change the verification commands.\n")
	for _, check := range result.Checks {
		if check.Passed {
			continue
		}
		fmt.Fprintf(&b, "\n- %s (%s)\n  reason: %s\n", check.Label, check.Kind, check.FailureReason)
		if check.ExitCode != nil {
			fmt.Fprintf(&b, "  exit code: %d\n", *check.ExitCode)
		}
		if check.StdoutTail != "" {
			fmt.Fprintf(&b, "  stdout tail:\n%s\n", check.StdoutTail)
		}
		if check.StderrTail != "" {
			fmt.Fprintf(&b, "  stderr tail:\n%s\n", check.StderrTail)
		}
	}
	return b.String()
}

func (c *Coordinator) failVerifyWithoutExecution(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, class domain.WorkflowErrorClass, reason string) (domain.WorkflowRun, domain.WorkflowStep, error) {
	now := c.clock()
	attempt, err := c.store.CreateWorkflowAttempt(ctx, "wfa-verify-"+c.newID(), step.ID, "local-verify", "invalid-target", now)
	if err != nil {
		return run, step, err
	}
	if step.State == domain.WorkflowStepPending {
		_, _ = c.store.UpdateWorkflowStepState(ctx, step.ID, domain.WorkflowStepPending, domain.WorkflowStepReady, now)
		step.State = domain.WorkflowStepReady
	}
	if step.State == domain.WorkflowStepReady {
		_, _ = c.store.UpdateWorkflowStepState(ctx, step.ID, domain.WorkflowStepReady, domain.WorkflowStepRunning, now)
		step.State = domain.WorkflowStepRunning
	}
	return c.finishVerifyFailure(ctx, run, step, attempt, VerifyResult{Version: verifyResultVersion, ErrorClass: class}, reason)
}

// repairableVerifyFailure reports whether a verification failure is the kind a
// fix cycle could plausibly repair — i.e. the checks ran and something in the
// code was wrong.
//
// The excluded classes are excluded on evidence, not caution:
// verify_environment_error means the checks could not run at all,
// verify_workspace_changed means the thing under test moved while AO was
// testing it, and verify_ambiguous means AO cannot say what happened. Sending a
// worker to "fix" any of those would be asking it to repair something no diff
// can address.
func repairableVerifyFailure(class domain.WorkflowErrorClass) bool {
	switch class {
	case domain.WorkflowErrorVerifyCommandFailed,
		domain.WorkflowErrorVerifyTimeout,
		domain.WorkflowErrorVerifyArtifactMissing,
		domain.WorkflowErrorVerifyArtifactMismatch:
		return true
	default:
		return false
	}
}

// verifyFixCycleCount counts this run's durable verify_fix_reentry
// checkpoints: how many times a failed verification has already been handed
// back to a fix worker. Derived from append-only rows, so the bound survives
// restarts, and it is the loop protection Phase 5 requires — without it a fix
// that never satisfies the verification command would cycle forever.
func (c *Coordinator) verifyFixCycleCount(ctx stdctx.Context, runID string) int {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		// Unreadable budget: treat as exhausted. Conservative in the direction
		// that stops rather than the direction that loops.
		return 1 << 30
	}
	n := 0
	for _, cp := range cps {
		if cp.DurablePhase == ReasonVerifyFixReentry {
			n++
		}
	}
	return n
}

func (c *Coordinator) finishVerifyFailure(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, attempt domain.WorkflowAttempt, result VerifyResult, reason string) (domain.WorkflowRun, domain.WorkflowStep, error) {
	result.Passed = false
	if len(result.Checks) == 0 && reason != "" {
		result.Checks = []VerifyCheckResult{{Kind: "guard", Label: "verify guard", Passed: false, FailureReason: reason}}
	}
	if err := c.persistVerifyResult(ctx, run, step, attempt, result, "verify_failed"); err != nil {
		return run, step, err
	}
	now := c.clock()
	_ = c.store.UpdateWorkflowAttemptOutcome(ctx, attempt.ID, now, domain.WorkflowAttemptFailed, result.ErrorClass)

	// Checkpoint 8P-E.13 Phase 5 — the debt 8P-E.12 left explicit. A failed
	// verification used to be the end of the road: the verify step went to
	// "failed" (terminal, zero outgoing transitions) and the run went to
	// needs_attention, so a run whose only problem was a failing test could
	// never continue no matter how much budget it had left. When the failure is
	// repairable and budget remains, the run instead hands the verify findings
	// back to the fix worker and re-verifies afterwards.
	//
	// The verify step deliberately rests at "waiting", not "failed": "failed" is
	// terminal for a step, and a terminal verify step would make re-verification
	// structurally impossible — the exact trap this branch exists to avoid.
	budget := policyForRun(run).MaxFixCycles
	used := c.verifyFixCycleCount(ctx, run.ID)
	if repairableVerifyFailure(result.ErrorClass) && used < budget && c.messageSender != nil {
		if step.State == domain.WorkflowStepRunning {
			if _, err := c.store.UpdateWorkflowStepState(ctx, step.ID, domain.WorkflowStepRunning, domain.WorkflowStepWaiting, now); err != nil {
				return run, step, err
			}
			step.State = domain.WorkflowStepWaiting
		}
		if run.State == domain.WorkflowRunRunning {
			if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, domain.WorkflowRunRunning, domain.WorkflowRunWaiting, now); err != nil {
				return run, step, err
			}
			run.State = domain.WorkflowRunWaiting
		}
		stepID, attemptID := step.ID, attempt.ID
		payload, _ := json.Marshal(result)
		if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
			ID:             "wfc-" + c.newID(),
			WorkflowRunID:  run.ID,
			WorkflowStepID: &stepID,
			AttemptID:      &attemptID,
			ProjectID:      run.ProjectID,
			// RetryState carries the full VerifyResult so the fix prompt is
			// built from the durable record of what actually failed, not from a
			// value that only existed in this call's memory.
			RetryState:        string(payload),
			FingerprintBefore: result.PreFingerprint,
			NextAction: fmt.Sprintf("fix: verification failed (%s) — handing findings back to the fix worker (cycle %d of %d)",
				result.ErrorClass, used+1, budget),
			DurablePhase:   ReasonVerifyFixReentry,
			PayloadVersion: verifyResultVersion,
			CreatedAt:      now,
		}); err != nil {
			return run, step, err
		}
		return run, step, nil
	}

	if step.State == domain.WorkflowStepRunning {
		if _, err := c.store.UpdateWorkflowStepState(ctx, step.ID, domain.WorkflowStepRunning, domain.WorkflowStepFailed, now); err != nil {
			return run, step, err
		}
		step.State = domain.WorkflowStepFailed
	}
	if run.State == domain.WorkflowRunWaiting {
		_, _ = c.store.UpdateWorkflowRunState(ctx, run.ID, domain.WorkflowRunWaiting, domain.WorkflowRunRunning, now)
		run.State = domain.WorkflowRunRunning
	}
	if run.State == domain.WorkflowRunRunning {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, domain.WorkflowRunRunning, domain.WorkflowRunNeedsAttention, now); err != nil {
			return run, step, err
		}
		run.State = domain.WorkflowRunNeedsAttention
	}
	// Name the stop in the canonical vocabulary, distinguishing "we tried
	// everything the budget allows" from "no fix cycle could have helped" —
	// two different situations with two different remedies.
	stopReason := ReasonVerifyUnrepairable
	if repairableVerifyFailure(result.ErrorClass) {
		stopReason = ReasonVerifyBudgetExhausted
	}
	c.recordAttentionStop(ctx, run, &step.ID, stopReason, fmt.Sprintf("verify failed (%s) after %d fix cycles: %s", result.ErrorClass, used, reason))
	return run, step, nil
}

func (c *Coordinator) persistVerifyResult(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, attempt domain.WorkflowAttempt, result VerifyResult, next string) error {
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	sid, aid := step.ID, attempt.ID
	_, err = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{ID: "wfc-" + c.newID(), WorkflowRunID: run.ID, WorkflowStepID: &sid, AttemptID: &aid, ProjectID: run.ProjectID, RetryState: string(b), NextAction: next, DurablePhase: "verify_result", PayloadVersion: verifyResultVersion, FingerprintBefore: result.PreFingerprint, FingerprintAfter: result.PostFingerprint, CreatedAt: c.clock()})
	return err
}

func (c *Coordinator) completeVerifiedRun(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep) (domain.WorkflowRun, domain.WorkflowStep, error) {
	now := c.clock()
	// Checkpoint 8P-E.11: the autonomous local commit happens here, between
	// "verified" and "completed", and while the branch lock is still held --
	// the only window in which the work is known-good and the repository is
	// still provably this run's to write. A commit failure fails the run
	// rather than completing it: reporting a run as completed while its work
	// sits uncommitted would be exactly the kind of untruthful state this
	// codebase refuses everywhere else.
	if err := c.autonomousLocalCommit(ctx, run, step); err != nil {
		if c.log != nil {
			c.log.Warn("workflow: autonomous local commit failed", "run", run.ID, "err", err)
		}
		return c.failRunOnCommitError(ctx, run, step, err)
	}
	defer c.releaseBranchLocks(ctx, run.ID, "workflow run completed")
	if step.State == domain.WorkflowStepRunning {
		if _, err := c.store.UpdateWorkflowStepState(ctx, step.ID, domain.WorkflowStepRunning, domain.WorkflowStepCompleted, now); err != nil {
			return run, step, err
		}
		step.State = domain.WorkflowStepCompleted
	}
	if run.State == domain.WorkflowRunWaiting {
		_, _ = c.store.UpdateWorkflowRunState(ctx, run.ID, domain.WorkflowRunWaiting, domain.WorkflowRunRunning, now)
		run.State = domain.WorkflowRunRunning
	}
	if run.State == domain.WorkflowRunRunning {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, domain.WorkflowRunRunning, domain.WorkflowRunCompleted, now); err != nil {
			return run, step, err
		}
		run.State = domain.WorkflowRunCompleted
		run.CompletedAt = &now
	}
	return run, step, nil
}

func secureWorktreePath(root, relative string) (string, error) {
	if relative == "" {
		relative = "."
	}
	if filepath.IsAbs(relative) {
		return "", errors.New("verify working directory must be relative to the worktree")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", err
	}
	candidate := filepath.Clean(filepath.Join(rootAbs, relative))
	rel, err := filepath.Rel(rootAbs, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("verify path escapes the worktree")
	}
	info, err := os.Stat(candidate)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("verify working directory is not a directory")
	}
	real, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	realRel, err := filepath.Rel(rootReal, real)
	if err != nil || realRel == ".." || strings.HasPrefix(realRel, ".."+string(filepath.Separator)) {
		return "", errors.New("verify path symlink escapes the worktree")
	}
	return real, nil
}

func verifyFile(root string, check VerificationFileCheck) (VerifyCheckResult, domain.WorkflowErrorClass) {
	cr := VerifyCheckResult{Kind: "file", Label: check.Path}
	parent := filepath.Dir(check.Path)
	dir, err := secureWorktreePath(root, parent)
	if err != nil {
		cr.FailureReason = err.Error()
		return cr, domain.WorkflowErrorVerifyEnvironment
	}
	path := filepath.Join(dir, filepath.Base(check.Path))
	if real, evalErr := filepath.EvalSymlinks(path); evalErr == nil {
		rootReal, rootErr := filepath.EvalSymlinks(root)
		rel, relErr := filepath.Rel(rootReal, real)
		if rootErr != nil || relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			cr.FailureReason = "artifact symlink escapes the worktree"
			return cr, domain.WorkflowErrorVerifyEnvironment
		}
	}
	data, err := os.ReadFile(path)
	if !check.Exists {
		if os.IsNotExist(err) {
			cr.Passed = true
			return cr, ""
		}
		if err == nil {
			cr.FailureReason = "artifact exists but must be absent"
			return cr, domain.WorkflowErrorVerifyArtifactMismatch
		}
	}
	if os.IsNotExist(err) {
		cr.FailureReason = "required artifact is missing"
		return cr, domain.WorkflowErrorVerifyArtifactMissing
	}
	if err != nil {
		cr.FailureReason = err.Error()
		return cr, domain.WorkflowErrorVerifyEnvironment
	}
	if check.ExactContent != nil && string(data) != *check.ExactContent {
		cr.FailureReason = "artifact exact content does not match"
		return cr, domain.WorkflowErrorVerifyArtifactMismatch
	}
	if check.SHA256 != "" {
		sum := sha256.Sum256(data)
		if !strings.EqualFold(hex.EncodeToString(sum[:]), check.SHA256) {
			cr.FailureReason = "artifact sha256 does not match"
			return cr, domain.WorkflowErrorVerifyArtifactMismatch
		}
	}
	cr.Passed = true
	return cr, ""
}
