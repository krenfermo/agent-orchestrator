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

// verifyUnexecutedTarget is the model value failVerifyWithoutExecution stamps on
// the attempt it records for a verification that never ran. It is not a target
// key and must never be compared with one: such an attempt has no opinion about
// any target, because it never got as far as having one.
const verifyUnexecutedTarget = "invalid-target"

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
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Passed bool   `json:"passed"`
	// ResolvedPath is where a file check was actually evaluated, worktree-
	// relative, when that differs from nothing at all. Label keeps the path as
	// the plan declared it, so the durable record shows both the declaration and
	// the namespace it was read in rather than leaving the two to be guessed
	// apart afterwards (wf-6528a538).
	ResolvedPath  string `json:"resolvedPath,omitempty"`
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
	// ContextResolutions records every working directory AO corrected for this
	// attempt (Checkpoint 8P-E.14), pre-flight or self-healed. Empty when every
	// command ran exactly where the plan said.
	ContextResolutions []VerifyContextResolution `json:"contextResolutions,omitempty"`
	// PathContext is the worktree-relative namespace every relative path check
	// in this attempt was evaluated in — the same directory this attempt's
	// commands ran in, with "." meaning the worktree root. It is recorded even
	// when it is "." so the record states the rule it applied instead of leaving
	// a reader to infer it, and it is what an invariant check reads to prove the
	// commands and the file checks of one spec agreed.
	PathContext string `json:"pathContext,omitempty"`
	// InfraFailure, when set, means this failure was AO's own verification
	// infrastructure rather than the code under test. Its presence alone
	// disqualifies the failure from ever being handed to a fix worker.
	InfraFailure *VerifyInfraFailure `json:"infraFailure,omitempty"`
	// RecoveryGeneration is which stale-verify recovery this attempt belongs to
	// (verify_recovery.go): 0 for every ordinary verification, and N>0 for the
	// Nth attempt a person explicitly reopened after correcting AO's own
	// verification infrastructure. It makes a recovery attempt's result
	// distinguishable from the historical one it re-asked, and it is what the
	// recovery ledger reads to know a generation has been answered.
	RecoveryGeneration int `json:"recoveryGeneration,omitempty"`
	// SupersededByFreshReview marks the one failure that is a question rather
	// than a verdict (Checkpoint 8P-E.14D): a recovery generation discovering that
	// the approval it holds no longer describes the workspace, and going to get a
	// fresh one. The failure itself is recorded honestly — it happened, on the
	// class it happened on — but the recovery ledger must not read it as the
	// generation's answer, or the generation would close on the question it just
	// asked. See verifyRecoveryLedger.executed.
	SupersededByFreshReview bool `json:"supersededByFreshReview,omitempty"`
	// StopReason, when set, is the canonical attention reason (attention.go)
	// finishVerifyFailure records instead of deriving one from the error class.
	// It exists for the failures whose remedy is genuinely different from
	// "inspect the verification output" — today only the unattributable workspace
	// drift 8P-E.14D refuses to absorb, whose question is "who changed this
	// worktree", not "why did the checks fail". Empty everywhere else, which
	// leaves every pre-existing stop naming itself exactly as it did.
	StopReason string `json:"stopReason,omitempty"`
}

func verificationTargetKey(fingerprint string, plan VerificationPlan) string {
	b, _ := json.Marshal(plan)
	sum := sha256.Sum256(append([]byte(fingerprint+"\n"), b...))
	return hex.EncodeToString(sum[:])
}

// verifyAttemptID is the deterministic identity of one verification attempt:
// the same step verifying the same target is the same attempt, however many
// times AO re-enters maybeVerify.
//
// recoveryGeneration widens that identity by exactly one dimension. A recovery
// (verify_recovery.go) re-asks the SAME target with a corrected verifier, so it
// must be a NEW attempt row — the old one is history that stays on disk — while
// still being the same attempt every time this generation is re-entered.
// Generation 0 hashes exactly what it always did, so no existing attempt id
// changes.
//
// fixGeneration widens it by the second and last dimension: how many
// verify-driven fix cycles this run has DELIVERED. A verification that failed,
// sent its findings to a fix worker and got a changed worktree back is history
// for a state that no longer exists, and the next verification is a genuinely
// new question about a genuinely new tree — but it is a question about the SAME
// approved target, because the approval is what makes a verification mean
// anything and a fix does not confer one.
//
// This is what replaced the old answer. maybeVerify used to keep the attempt
// identity fixed and move the TARGET instead, promoting the fingerprint the fix
// delivered to the thing being verified — which made a run completable on a tree
// no reviewer had read. Widening the attempt identity gives the re-verification
// its own row without touching whose authority it runs under: the verification
// re-runs, finds the workspace no longer matches the approval, and asks for a
// review of what is actually there. Generation 0 again hashes exactly what it
// always did.
func verifyAttemptID(stepID, targetKey string, recoveryGeneration, fixGeneration int) string {
	seed := stepID + "\n" + targetKey
	if recoveryGeneration > 0 {
		seed += fmt.Sprintf("\nrecovery=%d", recoveryGeneration)
	}
	if fixGeneration > 0 {
		seed += fmt.Sprintf("\nfix=%d", fixGeneration)
	}
	sum := sha256.Sum256([]byte(seed))
	return "wfa-verify-" + hex.EncodeToString(sum[:12])
}

// verifyFixDeliveries counts the verify-driven fix cycles this run has actually
// DELIVERED: verify_fix_reentry checkpoints that a later fix delivery answered.
//
// Derived from append-only rows, like every other generation counter here, so a
// restart recomputes the same value and the attempt identity is stable across
// one. A re-entry that has been written but not yet answered deliberately does
// not count: the tree has not moved yet, so the verification in flight is still
// the same question.
func (c *Coordinator) verifyFixDeliveries(ctx stdctx.Context, runID string) int {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		// Unreadable: claim no advance rather than invent one. The verification
		// then reuses its existing attempt, which is the conservative direction.
		return 0
	}
	var reentries, deliveries []time.Time
	for _, cp := range cps {
		switch cp.DurablePhase {
		case ReasonVerifyFixReentry:
			reentries = append(reentries, cp.CreatedAt)
		case "fix_observed_" + string(domain.WorkflowStepWaiting):
			if cp.FingerprintAfter != "" {
				deliveries = append(deliveries, cp.CreatedAt)
			}
		}
	}
	n := 0
	for _, r := range reentries {
		for _, d := range deliveries {
			if d.After(r) {
				n++
				break
			}
		}
	}
	return n
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
		if reviewRun.EffectiveVerdict() != domain.VerdictApproved {
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

	// THE REVIEW AUTHORITY INVARIANT.
	//
	// `reviewed` is the fingerprint an approving REVIEW was given for, and from
	// here to the end of this function nothing may replace it with a state no
	// reviewer read. Verification certifies work; a target AO chose for itself
	// is a certificate of nothing.
	//
	// This is where that used to be given away. A verify-driven fix cycle
	// deliberately changes the worktree, so the approval no longer describes it
	// — and the code answered that by promoting the fingerprint THE FIX
	// DELIVERED to the verification target (verifyTargetAfterFix), silently, on
	// no reviewer's authority at all. A run could therefore reach `completed`
	// on a tree whose last mutation nobody had reviewed:
	//
	//	approved(A) -> verify(A) fails -> fix -> HEAD=B -> verify(B) -> completed
	//
	// The remedy is not to verify A either — A is gone. It is to notice that
	// the approval went stale, say so, and go and get a new one: verification
	// proceeds against the approved target, finds the workspace no longer
	// matches it, attributes the difference to this run's own authorized fix
	// worker (workspace_provenance.go), and requests ONE bounded fresh review of
	// what is actually there. Only that review's approval becomes the next
	// verification target — through authorizeFreshReviewTarget below, which is
	// the single audited door a target may advance through.
	//
	//	approved(A) -> verify(A) fails -> fix -> HEAD=B
	//	            -> review(B) -> approved(B) -> verify(B)
	//
	// Same loop, same bounds, one more reviewer verdict — and no path from an
	// approval of A to a verification of B.

	artifact, err := c.planArtifactForRun(ctx, run)
	if err != nil {
		return run, verifyStep, err
	}
	if err := artifact.Verification.validate(); err != nil {
		return c.failVerifyWithoutExecution(ctx, run, verifyStep, domain.WorkflowErrorVerifyAmbiguous, err.Error())
	}
	targetKey := verificationTargetKey(reviewed, artifact.Verification)

	// Checkpoint 8P-E.14C: an open stale-verify recovery (verify_recovery.go) is
	// a person's explicit licence to re-ask a target whose historical answer came
	// from a verifier or an environment that has since been corrected.
	//
	// The licence is deliberately narrow. It authorizes a NEW attempt row for the
	// SAME reviewed target, and nothing else: if the target moved between the
	// authorization and this execution, the recovery is void rather than
	// silently re-using an approval that was given for different work.
	//
	// Checkpoint 8P-E.14D adds the one exception, and only the one: a review AO
	// ITSELF asked for, for this same generation and this same verification
	// target, because the approval it was holding no longer described the
	// workspace (verify_fresh_review.go). That approval is not a target that
	// drifted behind AO's back — it is the answer to a question AO asked on the
	// record — so the recovery's authorized target advances to it, once, durably.
	recovery, recovering := c.currentVerifyRecovery(ctx, run.ID)
	if recovering && recovery.ReviewedFingerprint != "" && recovery.ReviewedFingerprint != reviewed {
		approval, ok, err := c.authorizeFreshReviewTarget(ctx, run, reviewStep, verifyStep, recovery, reviewed, targetKey)
		if err != nil {
			return run, verifyStep, err
		}
		if !ok {
			return c.failVerifyWithoutExecution(ctx, run, verifyStep, domain.WorkflowErrorVerifyAmbiguous,
				"the reviewed target changed after verification recovery was authorized")
		}
		recovery.ReviewedFingerprint = approval.ReviewedFingerprint
		recovery.TargetKey = approval.TargetKey
	}
	recoveryGeneration := 0
	if recovering {
		recoveryGeneration = recovery.Generation
	}
	fixGeneration := c.verifyFixDeliveries(ctx, run.ID)

	latest, hasAttempt, err := c.store.GetLatestWorkflowAttempt(ctx, verifyStep.ID)
	if err != nil {
		return run, verifyStep, err
	}
	// The finished attempt of a superseded generation is history, not a decision
	// about this one. Keyed on the attempt's IDENTITY rather than on timestamps,
	// so re-entering this generation any number of times (a repeat Continue, a
	// poll, a restart) finds its own attempt and never opens a second:
	// verifyAttemptID is a pure function of step, target and the two generations.
	//
	// Only ever applied while a generation is actually open. At recovery 0 and
	// fix 0 the identity is bit-for-bit the pre-generation one, so no attempt
	// written by an older binary can be reopened by this rule.
	if hasAttempt && (recovering || fixGeneration > 0) && latest.Outcome != "" &&
		latest.ID != verifyAttemptID(verifyStep.ID, targetKey, recoveryGeneration, fixGeneration) {
		hasAttempt = false
		latest = domain.WorkflowAttempt{}
	}
	// An attempt that never executed has no target to have changed. Treating it
	// as one is what let a single unexecuted failure block every later
	// verification: its model is a placeholder, it differs from every real
	// target key, and the guard below read that difference as drift.
	if hasAttempt && latest.Model == verifyUnexecutedTarget {
		hasAttempt = false
		latest = domain.WorkflowAttempt{}
	}
	if hasAttempt && latest.Model != targetKey {
		// A target change is normally the ambiguity this guard exists to
		// catch — except when AO itself authorized it by running a
		// verify-driven fix cycle. In that case the prior attempt is simply
		// finished history for a superseded target, and this cycle gets its own
		// attempt row (verifyAttemptID is derived from the target key, so the
		// two can never collide).
		if !c.verifyTargetAdvancedByFix(ctx, run.ID, latest) && !c.verifyTargetAdvancedByReview(ctx, run.ID, latest) {
			return c.failVerifyWithoutExecution(ctx, run, verifyStep, domain.WorkflowErrorVerifyAmbiguous, "verify target changed after an attempt was created")
		}
		hasAttempt = false
		latest = domain.WorkflowAttempt{}
	}
	if hasAttempt && latest.Outcome == "" {
		if cp, found, cpErr := c.store.GetLatestWorkflowCheckpointByStep(ctx, verifyStep.ID); cpErr != nil {
			return run, verifyStep, cpErr
		} else if found && cp.DurablePhase == verifyResultPhase && cp.AttemptID != nil && *cp.AttemptID == latest.ID {
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
		attemptID := verifyAttemptID(verifyStep.ID, targetKey, recoveryGeneration, fixGeneration)
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

	// One execution of this attempt per process. An attempt in flight is
	// indistinguishable from one abandoned by a crash — that ambiguity is what
	// let a Continue and a Board poll run the same checks concurrently and both
	// act on their own answer. Losing the claim means another goroutine is
	// already executing these checks and its decision will settle the run, so
	// this pass simply stands down. Correctness does not rest here: the durable
	// CAS in decideVerify is what actually arbitrates. See verify_authority.go.
	releaseClaim, claimed := c.claimVerifyExecution(latest.ID)
	if !claimed {
		if c.log != nil {
			c.log.Debug("workflow: a verification of this attempt is already executing in this process",
				"run", run.ID, "attempt", latest.ID)
		}
		return run, verifyStep, nil
	}
	defer releaseClaim()

	obs, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{Path: workCP.WorktreePath, Branch: workCP.Branch, SessionID: domain.SessionID(*workCP.SessionID), ProjectID: domain.ProjectID(run.ProjectID)})
	if err != nil {
		return c.finishVerifyFailure(ctx, run, verifyStep, latest, VerifyResult{Version: verifyResultVersion, TargetKey: targetKey, ReviewedFingerprint: reviewed, ErrorClass: domain.WorkflowErrorVerifyEnvironment}, err.Error())
	}
	pre := WorkspaceFingerprint(obs)
	result := VerifyResult{Version: verifyResultVersion, TargetKey: targetKey, ReviewedFingerprint: reviewed, PreFingerprint: pre, Checks: []VerifyCheckResult{}}
	if pre != reviewed {
		result.ErrorClass = domain.WorkflowErrorVerifyWorkspaceChanged
		// Checkpoint 8P-E.14D: inside an explicitly authorized recovery, and ONLY
		// there, a workspace that no longer matches the approval may be a stale
		// approval rather than untrusted code — AO's own verifier was corrected
		// between the two, while the worker's uncommitted work was preserved on
		// purpose. When every fact needed to attribute the difference to this same
		// task holds, AO re-reviews the current workspace once instead of parking
		// forever; when it does not, it stops and says precisely which fact failed.
		// Outside a recovery this branch is not reachable, so an ordinary
		// verify_workspace_changed is bit-for-bit what it always was.
		if recovering {
			led, ledErr := c.verifyRecoveryLedger(ctx, run.ID)
			if ledErr != nil {
				return run, verifyStep, ledErr
			}
			decision := c.attributableWorkspaceDrift(ctx, run, reviewStep, recovery, led, targetKey, workCP, obs)
			if decision.allowed {
				return c.requestFreshReviewForRecovery(ctx, run, reviewStep, verifyStep, latest, result, recovery, workCP, obs)
			}
			if rec, allowed, _ := c.branchAdvancedDrift(ctx, run, reviewStep, workCP, obs, reviewed, pre, targetKey); allowed {
				return c.requestBranchAdvancedFreshReview(ctx, run, reviewStep, verifyStep, latest, result, rec)
			}
			result.StopReason = ReasonVerifyWorkspaceUnattributable
			return c.finishVerifyFailure(ctx, run, verifyStep, latest, result, decision.refusal)
		}
		// The one shape of a workspace change that is not a verdict: the branch
		// AO was authorized to work on grew COMMITS ON TOP of the reviewed one,
		// which still contains it. Nothing was lost, so nothing needs a person —
		// what went stale is the review, and AO can ask for a new one, bounded.
		// Every other shape, and every shape AO cannot prove, falls through to
		// the same stop it always did. See verify_branch_advanced.go.
		if rec, allowed, refusal := c.branchAdvancedDrift(ctx, run, reviewStep, workCP, obs, reviewed, pre, targetKey); allowed {
			return c.requestBranchAdvancedFreshReview(ctx, run, reviewStep, verifyStep, latest, result, rec)
		} else if c.log != nil {
			c.log.Debug("workflow: a workspace change is not a recoverable branch advance", "run", run.ID, "reason", refusal)
		}
		// Checkpoint 8P-E.24 (incident wf-cd5bad10): before this becomes an
		// unexplained stop, ANSWER THE SECOND QUESTION — whose change is this?
		// A difference AO can attribute to this task's own authorized work or
		// fix worker, in this task's own worktree at an unchanged HEAD, is not
		// untrusted code; it is code no reviewer has read YET. The remedy for
		// that is a review, not a person. Every other class — external,
		// another AO task's, a rewritten history, or anything AO cannot prove —
		// stops exactly as it always did, with the provenance recorded so the
		// stop is readable.
		steps, serr := c.store.ListWorkflowSteps(ctx, run.ID)
		if serr != nil {
			return run, verifyStep, serr
		}
		prov := c.classifyWorkspaceDrift(ctx, run, steps, reviewStep, workCP, obs, reviewed, pre, targetKey)
		if prov.Class.Authorized() {
			if generation := c.provenanceFreshReviewGenerations(ctx, run.ID) + 1; generation <= maxProvenanceFreshReviews {
				prov.Generation = generation
				return c.requestProvenanceFreshReview(ctx, run, reviewStep, verifyStep, latest, result, prov)
			}
			prov.Rationale += fmt.Sprintf("; but this run has already used its %d provenance-authorized re-reviews, so it stops instead", maxProvenanceFreshReviews)
		}
		if perr := c.recordWorkspaceProvenance(ctx, run, verifyStep.ID, prov); perr != nil && c.log != nil {
			c.log.Warn("workflow: recording workspace provenance failed", "run", run.ID, "err", perr)
		}
		return c.finishVerifyFailure(ctx, run, verifyStep, latest, result,
			"workspace fingerprint no longer matches the approved review target ("+string(prov.Class)+": "+prov.Rationale+")")
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

	// Checkpoint 8P-E.14: resolve each command's working directory against the
	// project's real module root BEFORE executing it. A Go command asked to run
	// from a directory that is not inside any module is the incident this guards
	// — it fails with exit code 1 and a message about the main module, which is
	// indistinguishable from a broken build unless AO knows where the module is.
	repairsUsed := c.verifyContextRepairCount(ctx, run.ID)
	// effectiveDirs collects where each command ACTUALLY ran, after any
	// pre-flight resolution and any mid-attempt repair. It is what the file
	// checks below derive their namespace from, which is the whole reason the
	// two halves of one spec can no longer disagree — see VerifyPathContext.
	var effectiveDirs []string
	for _, planned := range narrowedPlan.Commands {
		check := planned
		if resolved, resolution, resolveErr := resolveVerifyCommandContext(workCP.WorktreePath, check, false); resolveErr == nil && resolution != nil {
			check = resolved
			result.ContextResolutions = append(result.ContextResolutions, *resolution)
		}
		// A resolution failure (no module root, or several) is deliberately not
		// fatal here: the command still runs exactly as configured, and if it
		// then fails for that reason the classifier below names it precisely
		// instead of AO pre-judging a project layout it does not understand.
		dir, pathErr := secureWorktreePath(workCP.WorktreePath, check.WorkingDirectory)
		if pathErr != nil {
			result.ErrorClass = domain.WorkflowErrorVerifyEnvironment
			return c.finishVerifyFailure(ctx, run, verifyStep, latest, result, pathErr.Error())
		}
		timeout := time.Duration(check.TimeoutSeconds) * time.Second
		if timeout == 0 {
			timeout = 10 * time.Minute
		}
		transientRetries := 0
		for {
			execResult, runErr := c.verifier.Run(ctx, VerifyCommandRequest{Command: check.Command, Args: check.Args, Directory: dir, Timeout: timeout})
			if errors.Is(runErr, stdctx.Canceled) {
				return run, verifyStep, runErr
			}
			exitCode := execResult.ExitCode
			cr := VerifyCheckResult{Kind: "command", Label: commandLabel(check), ExitCode: &exitCode, DurationMS: execResult.DurationMS, StdoutTail: execResult.StdoutTail, StderrTail: execResult.StderrTail}
			var infra *VerifyInfraFailure
			switch {
			case execResult.TimedOut:
				result.ErrorClass = domain.WorkflowErrorVerifyTimeout
				cr.FailureReason = "command timed out"
			case runErr != nil:
				infra = classifyVerifyExecutionFailure(check, normalizeRel(check.WorkingDirectory), execResult, runErr)
				result.ErrorClass = domain.WorkflowErrorVerifyEnvironment
				cr.FailureReason = runErr.Error()
			case exitCode != check.RequiredExitCode:
				infra = classifyVerifyExecutionFailure(check, normalizeRel(check.WorkingDirectory), execResult, nil)
				if infra != nil {
					result.ErrorClass = domain.WorkflowErrorVerifyEnvironment
					cr.FailureReason = infra.Reason()
				} else {
					result.ErrorClass = domain.WorkflowErrorVerifyCommandFailed
					cr.FailureReason = fmt.Sprintf("exit code %d, required %d", exitCode, check.RequiredExitCode)
				}
			default:
				cr.Passed = true
			}
			if cr.Passed {
				result.Checks = append(result.Checks, cr)
				break
			}
			if infra != nil {
				// Self-heal: the command proved it ran outside the module it was
				// meant to verify. When AO can deterministically name the right
				// module root, it records that decision durably and re-runs the
				// same command there — no human, no fix worker.
				if infra.Kind == VerifyInfraWrongModuleRoot && repairsUsed < maxVerifyContextRepairs {
					repaired, resolution, resolveErr := resolveVerifyCommandContext(workCP.WorktreePath, check, true)
					if resolveErr == nil && resolution != nil {
						repairedDir, dirErr := secureWorktreePath(workCP.WorktreePath, repaired.WorkingDirectory)
						if dirErr == nil {
							resolution.Repaired = true
							resolution.Reason = fmt.Sprintf("%s (after: %s)", resolution.Reason, infra.Detail)
							if err := c.persistVerifyContextRepair(ctx, run, verifyStep, latest, *resolution); err != nil {
								return run, verifyStep, err
							}
							result.ContextResolutions = append(result.ContextResolutions, *resolution)
							check, dir, repairsUsed = repaired, repairedDir, repairsUsed+1
							continue
						}
					}
					if resolveErr != nil {
						// Nothing AO may safely guess (no module root at all, or
						// several). Report the layout problem itself rather than
						// the toolchain message it produced.
						infra.Kind = VerifyInfraConfigInvalid
						infra.Repairable = false
						infra.Detail = resolveErr.Error() + "; " + infra.Detail
						cr.FailureReason = infra.Reason()
					}
				}
				if infra.Transient && check.RetrySafe && transientRetries < maxTransientVerifyRetries {
					transientRetries++
					continue
				}
				result.InfraFailure = infra
			}
			result.Checks = append(result.Checks, cr)
			return c.finishVerifyFailure(ctx, run, verifyStep, latest, result, cr.FailureReason)
		}
		effectiveDirs = append(effectiveDirs, normalizeRel(check.WorkingDirectory))
	}
	// One namespace for this spec, derived from where its commands ran and
	// recorded durably, so the VerifyResult states the rule it applied rather
	// than leaving a reader to infer it from two independently-resolved halves.
	pathCtx := verifyPathContextFor(effectiveDirs)
	result.PathContext = pathCtx.Base
	for _, check := range narrowedPlan.Files {
		cr, class := verifyFile(workCP.WorktreePath, pathCtx, check)
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
	// The decision BEFORE the effects. If a concurrent execution of this same
	// attempt already decided, this pass is stale however green it is, and
	// completing the run on it would be the winning half of the race that left
	// wf-04e8309d terminal with a fix still running. See verify_authority.go.
	decision, derr := c.decideVerify(ctx, run, verifyStep, latest, domain.WorkflowAttemptSucceeded, "", result)
	if derr != nil {
		return run, verifyStep, derr
	}
	if !decision.Won {
		return run, verifyStep, nil
	}
	if err := c.persistVerifyResult(ctx, run, verifyStep, latest, result, "done"); err != nil {
		return run, verifyStep, err
	}
	return c.completeVerifiedRun(ctx, run, verifyStep)
}

// verifyTargetAdvancedByFix reports whether a verify-driven fix cycle was
// authorized after the given attempt started — the one legitimate reason the
// verification target may differ from the one that attempt recorded.
// verifyTargetAdvancedByReview is verifyTargetAdvancedByFix's other half: the
// target changed because AO asked a REVIEWER again, not because it ran a fix.
//
// The guard above exists to catch a target that drifted behind AO's back, and
// it knew exactly one legitimate reason for a change — a verify-driven fix. But
// a review AO itself dispatched after the attempt started is just as
// authorized, and just as much on the record: an integration fresh review, a
// stale-approval recovery, an amended acceptance criterion. All three end in a
// new approval of a new target, and all three left the previous attempt as
// finished history for a superseded one.
//
// Without this, that history is read as ambiguity and the verification fails
// with "verify target changed after an attempt was created" — refusing the very
// re-verification AO asked for. The attempt row cannot collide either way:
// verifyAttemptID is derived from the target key.
func (c *Coordinator) verifyTargetAdvancedByReview(ctx stdctx.Context, runID string, attempt domain.WorkflowAttempt) bool {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return false
	}
	for _, cp := range cps {
		if cp.DurablePhase == reviewDispatchedDurablePhase && !cp.CreatedAt.Before(attempt.StartedAt) {
			return true
		}
	}
	return false
}

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
	//
	// The predicate lives in fix_authority.go because the delivery gate applies
	// the identical rule: an approved review authorizes a fix cycle only while
	// its re-entry is unanswered. Two copies of that rule could disagree, and
	// the one that said "authorized" would win by being the one that sends.
	open, _, err := c.unansweredVerifyFixReentry(ctx, run.ID, fixStep.ID)
	if err != nil {
		return fixStep, err
	}
	if !open {
		return fixStep, nil
	}
	attempts, err := c.store.ListWorkflowAttempts(ctx, fixStep.ID)
	if err != nil {
		return fixStep, err
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
	// The findings here are AO's own verification output, not the reviewer's,
	// and the durable evidence says so — see FixFindingsSourceVerification.
	findings := verifyFindingsRef(reviewRun, renderVerifyFindings(result))
	prompt := BuildFixPrompt(FixPromptInput{
		Objective:          run.Objective,
		AcceptanceCriteria: artifact.AcceptanceCriteria,
		EffectiveSpec:      RenderEffectiveSpecification(c.effectiveTaskSpecification(ctx, run, artifact.AcceptanceCriteria)),
		ReviewRunID:        reviewRun.ID,
		Findings:           findings.Body,
		CycleNumber:        cycleNumber,
	})
	return c.dispatchFixStep(ctx, run, workStep, fixStep, reviewRun, cycleNumber, prompt, findings)
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
	// One row per condition, not one per poll.
	//
	// maybeVerify runs on every GetRun, and a verification that cannot execute
	// is usually a standing condition rather than an event: a missing worktree,
	// an unparseable plan, an ambiguous target. Minting a fresh attempt row each
	// time turned that into an unbounded write storm — wf-04e8309d accumulated
	// 35 of them in three minutes — and, worse, each new row became the LATEST
	// attempt, so every guard that reasons about "the attempt that came before"
	// was reasoning about a row created microseconds ago by the previous poll.
	// The condition re-observed is the same condition, and it is already
	// recorded.
	if latest, ok, lerr := c.store.GetLatestWorkflowAttempt(ctx, step.ID); lerr == nil && ok &&
		latest.Model == verifyUnexecutedTarget && latest.Outcome != "" && latest.ErrorClass == class {
		return run, step, nil
	}
	attempt, err := c.store.CreateWorkflowAttempt(ctx, "wfa-verify-"+c.newID(), step.ID, "local-verify", verifyUnexecutedTarget, now)
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

// fixCycleAccounting renders how much fixing this run has actually done, from
// the durable rows rather than from one of the two counters.
//
// The stop line used to read "after %d fix cycles" and pass `used` — the count
// of verify_fix_reentry checkpoints, i.e. VERIFY-driven fix cycles only. For
// wf-a21d98aa that was 0, and it was printed on a run that had completed two
// reviewer-requested fix cycles, delivered by two finished attempts on the fix
// step. Both numbers were individually right and the sentence was wrong, which
// is worse than either: the operator reading it concluded nothing had touched
// the tree since the work step.
//
// So the line names both, and both come from durable rows: the fix step's own
// finished attempts (every cycle actually delivered, whoever asked for it) and
// the verify-driven subset that the fix budget here is spent against.
func (c *Coordinator) fixCycleAccounting(ctx stdctx.Context, runID string, verifyDriven int) string {
	delivered := -1
	if steps, err := c.store.ListWorkflowSteps(ctx, runID); err == nil {
		for _, s := range steps {
			if s.Kind != domain.WorkflowStepFix {
				continue
			}
			attempts, aerr := c.store.ListWorkflowAttempts(ctx, s.ID)
			if aerr != nil {
				break
			}
			delivered = 0
			for _, a := range attempts {
				if a.Outcome == domain.WorkflowAttemptSucceeded {
					delivered++
				}
			}
		}
	}
	if delivered < 0 {
		// Unreadable. Say only what is still provable rather than invent a total.
		return fmt.Sprintf("%d verify-driven fix cycles", verifyDriven)
	}
	return fmt.Sprintf("%d delivered fix cycles (%d of them verify-driven)", delivered, verifyDriven)
}

func (c *Coordinator) finishVerifyFailure(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, attempt domain.WorkflowAttempt, result VerifyResult, reason string) (domain.WorkflowRun, domain.WorkflowStep, error) {
	result.Passed = false
	if len(result.Checks) == 0 && reason != "" {
		result.Checks = []VerifyCheckResult{{Kind: "guard", Label: "verify guard", Passed: false, FailureReason: reason}}
	}
	// The decision BEFORE the effects, for exactly the reason the success path
	// makes it: a failing execution that lost the race must not open a fix
	// cycle against a target another execution already passed.
	decision, derr := c.decideVerify(ctx, run, step, attempt, domain.WorkflowAttemptFailed, result.ErrorClass, result)
	if derr != nil {
		return run, step, derr
	}
	if !decision.Won {
		return run, step, nil
	}
	if err := c.persistVerifyResult(ctx, run, step, attempt, result, "verify_failed"); err != nil {
		return run, step, err
	}
	now := c.clock()

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
	// Checkpoint 8P-E.14: an infrastructure failure is never a code defect, so
	// it never reaches a fix worker — however repairable its error class would
	// otherwise look. This is the guard whose absence turned "AO ran go build
	// from the wrong directory" into "the worker's code is broken".
	if result.InfraFailure == nil && repairableVerifyFailure(result.ErrorClass) && used < budget && c.messageSender != nil {
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
	detail := fmt.Sprintf("verify failed (%s) after %s: %s", result.ErrorClass, c.fixCycleAccounting(ctx, run.ID, used), reason)
	switch {
	case result.StopReason != "":
		// A stop the failing site already named precisely. Its detail is that
		// site's own explanation, which is more specific than anything derivable
		// from the error class here.
		stopReason = result.StopReason
		detail = reason
	case result.InfraFailure != nil:
		// Name the infrastructure failure for what it is, so the user is told to
		// fix the verifier rather than to go read a diff that is not at fault.
		stopReason = infraAttentionReason(result.InfraFailure.Kind)
		detail = result.InfraFailure.Reason()
	case repairableVerifyFailure(result.ErrorClass):
		stopReason = ReasonVerifyBudgetExhausted
	}
	c.recordAttentionStop(ctx, run, &step.ID, stopReason, detail)
	return run, step, nil
}

// verifyContextRepairCount counts this run's durable verify_context_repair
// checkpoints: how many times AO has already corrected a verification working
// directory by itself. Derived from append-only rows for the same reason
// verifyFixCycleCount is — the bound has to survive a restart, or a repair that
// does not help becomes an unbounded loop.
func (c *Coordinator) verifyContextRepairCount(ctx stdctx.Context, runID string) int {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return maxVerifyContextRepairs
	}
	n := 0
	for _, cp := range cps {
		if cp.DurablePhase == verifyContextRepairPhase {
			n++
		}
	}
	return n
}

// persistVerifyContextRepair records a working-directory correction before the
// re-run happens, so the decision is durable even if the daemon dies mid-retry
// and so a person can later read exactly why a command ran somewhere other than
// where the plan said.
func (c *Coordinator) persistVerifyContextRepair(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, attempt domain.WorkflowAttempt, resolution VerifyContextResolution) error {
	payload, err := json.Marshal(resolution)
	if err != nil {
		return err
	}
	sid, aid := step.ID, attempt.ID
	_, err = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &sid,
		AttemptID:      &aid,
		ProjectID:      run.ProjectID,
		RetryState:     string(payload),
		NextAction: fmt.Sprintf("verify: re-running %q from %q (was %q)",
			resolution.Label, resolution.Resolved, resolution.Requested),
		DurablePhase:   verifyContextRepairPhase,
		PayloadVersion: verifyResultVersion,
		CreatedAt:      c.clock(),
	})
	return err
}

func (c *Coordinator) persistVerifyResult(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, attempt domain.WorkflowAttempt, result VerifyResult, next string) error {
	// Checkpoint 8P-E.14C: stamp the open recovery generation onto every result
	// this run persists, here rather than at each of maybeVerify's ~ten
	// construction sites. One place cannot drift from another, and it is also the
	// exact moment the stamp means what it says: a persisted result IS the answer
	// to the generation that was open when it was written, and writing it is what
	// closes that generation (see verifyRecoveryLedger.executed).
	if result.RecoveryGeneration == 0 {
		if rec, ok := c.currentVerifyRecovery(ctx, run.ID); ok {
			result.RecoveryGeneration = rec.Generation
		}
	}
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	sid, aid := step.ID, attempt.ID
	_, err = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{ID: "wfc-" + c.newID(), WorkflowRunID: run.ID, WorkflowStepID: &sid, AttemptID: &aid, ProjectID: run.ProjectID, RetryState: string(b), NextAction: next, DurablePhase: verifyResultPhase, PayloadVersion: verifyResultVersion, FingerprintBefore: result.PreFingerprint, FingerprintAfter: result.PostFingerprint, CreatedAt: c.clock()})
	return err
}

func (c *Coordinator) completeVerifiedRun(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep) (domain.WorkflowRun, domain.WorkflowStep, error) {
	now := c.clock()
	// A run may not go terminal while something that can still change its
	// outcome is running. Completing over a live fix worker claims the work is
	// finished while an agent is still editing it, and strands an advance step
	// that will now never run -- the exact shape wf-04e8309d ended in. The
	// verify step itself is excluded because it is THIS step, mid-completion.
	//
	// Refusing is safe and self-correcting: the cascade re-enters once the other
	// step settles, and this verification's result is already durable.
	if steps, serr := c.store.ListWorkflowSteps(ctx, run.ID); serr == nil {
		var others []domain.WorkflowStep
		for _, s := range steps {
			if s.ID != step.ID {
				others = append(others, s)
			}
		}
		if why, active := runHasActiveWork(others); active {
			if c.log != nil {
				c.log.Info("workflow: a verified run is not completing yet because work is still in flight",
					"run", run.ID, "reason", why)
			}
			return run, step, nil
		}
	}
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
		if _, err := c.completeRun(ctx, run, domain.WorkflowRunRunning); err != nil {
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

// secureVerifyArtifactPath is secureWorktreePath's counterpart for a file an
// artifact check names, and it differs in exactly one deliberate way: it does
// not require the path — or its parent — to exist.
//
// That is the second half of the wf-6528a538 incident. The old code resolved
// the artifact's PARENT through secureWorktreePath, which stats and returns an
// error for a missing directory, so a check whose directory was not there came
// back as verify_environment_error ("stat .../internal/postrunqa: no such file
// or directory") instead of the artifact-missing verdict the check actually
// warranted. Existence is the question a file check asks; it must not also be
// the precondition for asking it.
//
// Containment is still enforced in full, and without depending on existence:
// lexically first, then — for whatever part of the path does exist — through
// EvalSymlinks on the deepest existing ancestor, which is where a symlink could
// smuggle the read outside the worktree. A path with no existing ancestor
// inside the worktree cannot escape it, because there is nothing to follow.
func secureVerifyArtifactPath(root, relative string) (string, error) {
	if relative == "" || relative == "." {
		return "", errors.New("verify file path is required")
	}
	if filepath.IsAbs(relative) {
		return "", errors.New("verify file path must be relative to the worktree")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", err
	}
	candidate := filepath.Clean(filepath.Join(rootAbs, filepath.FromSlash(relative)))
	rel, err := filepath.Rel(rootAbs, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("verify path escapes the worktree")
	}
	// Walk up to the deepest ancestor that exists and prove IT is still inside
	// the worktree once symlinks are resolved. Everything below it does not
	// exist yet, so no link on that part of the path can redirect the read.
	for probe := candidate; ; probe = filepath.Dir(probe) {
		real, evalErr := filepath.EvalSymlinks(probe)
		if evalErr != nil {
			if os.IsNotExist(evalErr) {
				if filepath.Clean(probe) == filepath.Clean(rootAbs) {
					return "", errors.New("verify worktree root does not exist")
				}
				parent := filepath.Dir(probe)
				if parent == probe {
					return "", errors.New("verify path escapes the worktree")
				}
				continue
			}
			return "", evalErr
		}
		realRel, relErr := filepath.Rel(rootReal, real)
		if relErr != nil || realRel == ".." || strings.HasPrefix(realRel, ".."+string(filepath.Separator)) {
			return "", errors.New("artifact symlink escapes the worktree")
		}
		return candidate, nil
	}
}

func verifyFile(root string, pathCtx VerifyPathContext, check VerificationFileCheck) (VerifyCheckResult, domain.WorkflowErrorClass) {
	// One resolution, shared with the commands of this same spec, applied
	// identically to existence, exact-content and sha256 checks — they all read
	// the same bytes from the same resolved path below.
	resolved := pathCtx.ResolvePath(check.Path)
	cr := VerifyCheckResult{Kind: "file", Label: check.Path, ResolvedPath: resolved}
	path, err := secureVerifyArtifactPath(root, resolved)
	if err != nil {
		cr.FailureReason = err.Error()
		return cr, domain.WorkflowErrorVerifyEnvironment
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
