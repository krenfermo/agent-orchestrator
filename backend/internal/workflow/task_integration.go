package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/integration"
)

// This file is where a completed task meets internal/integration: the gate that
// decides whether it may be integrated at all, the ledger its integration is
// recorded on, the verifier that re-checks work after it has been replayed, and
// the one call site that routes a task through the Integration Coordinator.
//
// It replaced, rather than joined, what master_integration.go used to do. That
// promotion captured a worktree's CONTENT onto the master's integration ref,
// parented on whatever the ref pointed at — correct exactly while promotions
// were serial, because then the worktree the content came from was cut from
// that same commit. Parallel dispatch broke the "because": two tasks cut from
// one base can finish in either order, and the second one's tree does not
// contain the first one's changes, so materializing it would quietly revert a
// sibling that had already landed.
//
// There is now no second route and no fork. Every ready task — isolated
// worktree, smart-parallel or direct branch — goes through the Coordinator,
// which takes the lane, reads the target head inside it, gates on readiness,
// picks the strategy that fits what it finds (fast-forward, replay, or a no-op
// for work already on the ref), lands under compare-and-set, and records the
// audit. "The target did not move" is a strategy it decides under the lock, not
// a shortcut past it.

const (
	// taskIntegrationDurablePhase is the append-only ledger phase every
	// integration attempt is recorded under. It follows the same
	// workflow_checkpoints convention as master_integration_promotion rather
	// than adding a table: the record is append-only, read by folding, and
	// never updated in place.
	taskIntegrationDurablePhase   = "task_integration"
	taskIntegrationPayloadVersion = "v1"
)

// taskIntegrationPayload is the ledger row's JSON body. It carries everything
// about an integration that stops being derivable the moment the target moves
// again -- which is nearly all of it.
type taskIntegrationPayload struct {
	TaskID            string   `json:"taskId"`
	Outcome           string   `json:"outcome"`
	Strategy          string   `json:"strategy,omitempty"`
	TargetRef         string   `json:"targetRef,omitempty"`
	TargetBranch      string   `json:"targetBranch,omitempty"`
	SourceBranch      string   `json:"sourceBranch,omitempty"`
	SourceSHA         string   `json:"sourceSha,omitempty"`
	TargetBeforeSHA   string   `json:"targetBeforeSha,omitempty"`
	TargetAfterSHA    string   `json:"targetAfterSha,omitempty"`
	BaseSHA           string   `json:"baseSha,omitempty"`
	Replayed          bool     `json:"replayed,omitempty"`
	AutoResolvedPaths []string `json:"autoResolvedPaths,omitempty"`
	// The verification that authorized this integration, in full. Recording
	// only Ran/Passed was the reviewer's third finding: a reader could not tell
	// "nothing verified this" from "the task's own verify step did, and here is
	// the row it is written on".
	VerificationRan     bool     `json:"verificationRan,omitempty"`
	VerificationPass    bool     `json:"verificationPassed,omitempty"`
	VerificationNote    string   `json:"verificationSummary,omitempty"`
	VerificationSource  string   `json:"verificationSource,omitempty"`
	VerificationStep    string   `json:"verificationStepId,omitempty"`
	VerificationRecord  string   `json:"verificationEvidenceId,omitempty"`
	VerifiedFingerprint string   `json:"verifiedFingerprint,omitempty"`
	AttentionReason     string   `json:"attentionReason,omitempty"`
	ConflictFiles       []string `json:"conflictFiles,omitempty"`
	AttentionDetail     string   `json:"attentionDetail,omitempty"`
}

// integrationLedger is the durable integration.Recorder. Every attempt --
// the intent to move a ref, the move that happened, and the stop that a person
// has to resolve -- becomes one checkpoint row on the master run.
type integrationLedger struct {
	c      *Coordinator
	parent domain.WorkflowRun
}

// RecordIntegration persists one attempt. It deliberately has no dedup: unlike
// recordIntegrationFailure, which guards against a permanent condition being
// re-recorded on every poll, an integration attempt happens once per promotion
// and each row is a distinct event in the life of the target ref.
func (l integrationLedger) RecordIntegration(ctx stdctx.Context, rec integration.Record) error {
	payload := taskIntegrationPayload{
		TaskID:              rec.TaskID,
		Outcome:             string(rec.Outcome),
		Strategy:            string(rec.Strategy),
		TargetRef:           rec.TargetRef,
		TargetBranch:        rec.TargetBranch,
		SourceBranch:        rec.SourceBranch,
		SourceSHA:           rec.SourceSHA,
		TargetBeforeSHA:     rec.TargetBeforeSHA,
		TargetAfterSHA:      rec.TargetAfterSHA,
		BaseSHA:             rec.BaseSHA,
		Replayed:            rec.Replayed,
		AutoResolvedPaths:   rec.AutoResolvedPaths,
		VerificationRan:     rec.Verification.Ran,
		VerificationPass:    rec.Verification.Passed,
		VerificationNote:    rec.Verification.Summary,
		VerificationSource:  string(rec.Verification.Source),
		VerificationStep:    rec.Verification.StepID,
		VerificationRecord:  rec.Verification.EvidenceID,
		VerifiedFingerprint: rec.Verification.Fingerprint,
	}
	if rec.Attention != nil {
		payload.AttentionReason = string(rec.Attention.Reason)
		payload.ConflictFiles = rec.Attention.ConflictFiles
		payload.AttentionDetail = rec.Attention.Detail
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = l.c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:            "wfc-" + l.c.newID(),
		WorkflowRunID: l.parent.ID,
		ProjectID:     l.parent.ProjectID,
		// BaseSHA/HeadSHA carry the ref update on the columns a reader already
		// expects it on, so the two SHAs that matter most are legible without
		// unmarshalling the payload.
		BaseSHA:        rec.TargetBeforeSHA,
		HeadSHA:        rec.TargetAfterSHA,
		RetryState:     string(body),
		DurablePhase:   taskIntegrationDurablePhase,
		PayloadVersion: taskIntegrationPayloadVersion,
		CreatedAt:      l.c.clock(),
	})
	return err
}

// TaskIntegrationRecord is one row of the integration ledger, as a reader sees
// it. It is the read side of integrationLedger and exists so a caller (a test,
// a future board card) can account for what happened to a target ref without
// knowing the checkpoint encoding.
type TaskIntegrationRecord struct {
	TaskID          string
	Outcome         string
	Strategy        string
	TargetRef       string
	SourceSHA       string
	TargetBeforeSHA string
	TargetAfterSHA  string
	BaseSHA         string
	Replayed        bool
	VerificationRan bool
	VerificationOK  bool
	// The evidence behind the verdict: where it came from, which durable
	// records hold it, and which content it describes.
	VerificationSource  string
	VerificationStepID  string
	VerificationRecord  string
	VerifiedFingerprint string
	AttentionReason     string
	ConflictFiles       []string
	RecordedAt          time.Time
}

// ListTaskIntegrations returns every integration attempt recorded for a master
// run, oldest first.
func (c *Coordinator) ListTaskIntegrations(ctx stdctx.Context, masterRunID string) ([]TaskIntegrationRecord, error) {
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, masterRunID)
	if err != nil {
		return nil, err
	}
	var out []TaskIntegrationRecord
	for _, cp := range checkpoints {
		if cp.DurablePhase != taskIntegrationDurablePhase {
			continue
		}
		var payload taskIntegrationPayload
		if json.Unmarshal([]byte(cp.RetryState), &payload) != nil {
			continue
		}
		out = append(out, TaskIntegrationRecord{
			TaskID:              payload.TaskID,
			Outcome:             payload.Outcome,
			Strategy:            payload.Strategy,
			TargetRef:           payload.TargetRef,
			SourceSHA:           payload.SourceSHA,
			TargetBeforeSHA:     payload.TargetBeforeSHA,
			TargetAfterSHA:      payload.TargetAfterSHA,
			BaseSHA:             payload.BaseSHA,
			Replayed:            payload.Replayed,
			VerificationRan:     payload.VerificationRan,
			VerificationOK:      payload.VerificationPass,
			VerificationSource:  payload.VerificationSource,
			VerificationStepID:  payload.VerificationStep,
			VerificationRecord:  payload.VerificationRecord,
			VerifiedFingerprint: payload.VerifiedFingerprint,
			AttentionReason:     payload.AttentionReason,
			ConflictFiles:       payload.ConflictFiles,
			RecordedAt:          cp.CreatedAt,
		})
	}
	return out, nil
}

// integrationVerifier re-runs a task's own planned verification against its
// worktree, for internal/integration.
//
// It is the existing verification infrastructure, not a second one: the same
// VerifyRunner the verify step uses, the same working-directory resolution
// (resolveVerifyCommandContext), the same path confinement (secureWorktreePath)
// and the same file checks (verifyFile). What it deliberately does NOT reuse is
// maybeVerify itself, because that function is a state machine over a run's
// verify step -- attempts, fingerprints, fix cycles, recovery generations --
// and none of that applies here. This asks one question at one moment: do the
// task's own checks still pass now that its work sits on a different target?
type integrationVerifier struct {
	c    *Coordinator
	plan VerificationPlan
}

// Verify runs the plan's command and file checks against req.WorktreePath.
func (v integrationVerifier) Verify(ctx stdctx.Context, req integration.VerifyRequest) (integration.Verification, error) {
	if len(v.plan.Commands) == 0 && len(v.plan.Files) == 0 {
		// A task whose plan asks for no verification has nothing that could
		// fail, and needs no runner to establish that. Saying so explicitly is
		// more honest than reporting a pass.
		return integration.Verification{Passed: true, Summary: "the task's plan declares no verification"}, nil
	}
	if v.c.verifier == nil {
		return integration.Verification{}, fmt.Errorf("workflow: re-verifying task %s after replay requires a VerifyRunner", req.TaskID)
	}

	var failures []string
	pathBase := "."
	for _, planned := range v.plan.Commands {
		check := planned
		if resolved, resolution, err := resolveVerifyCommandContext(req.WorktreePath, check, false); err == nil && resolution != nil {
			check = resolved
		}
		dir, err := secureWorktreePath(req.WorktreePath, check.WorkingDirectory)
		if err != nil {
			return integration.Verification{}, err
		}
		if base := normalizeRel(check.WorkingDirectory); base != "" {
			pathBase = base
		}
		timeout := time.Duration(check.TimeoutSeconds) * time.Second
		if timeout == 0 {
			timeout = 10 * time.Minute
		}
		exec, runErr := v.c.verifier.Run(ctx, VerifyCommandRequest{
			Command: check.Command, Args: check.Args, Directory: dir, Timeout: timeout,
		})
		switch {
		case errorIsCancellation(runErr):
			// A cancelled context is the caller going away, not a verdict.
			return integration.Verification{}, runErr
		case exec.TimedOut:
			failures = append(failures, commandLabel(check)+": timed out")
		case runErr != nil:
			failures = append(failures, commandLabel(check)+": "+runErr.Error())
		case exec.ExitCode != check.RequiredExitCode:
			failures = append(failures, fmt.Sprintf("%s: exit code %d, required %d",
				commandLabel(check), exec.ExitCode, check.RequiredExitCode))
		}
	}
	for _, check := range v.plan.Files {
		result, _ := verifyFile(req.WorktreePath, VerifyPathContext{Base: pathBase}, check)
		if !result.Passed {
			failures = append(failures, check.Path+": "+result.FailureReason)
		}
	}

	if len(failures) > 0 {
		return integration.Verification{
			Passed:  false,
			Summary: fmt.Sprintf("%d of the task's checks failed after the replay: %s", len(failures), strings.Join(failures, "; ")),
		}, nil
	}
	return integration.Verification{
		Passed:  true,
		Summary: fmt.Sprintf("%d command and %d file checks passed against the replayed work", len(v.plan.Commands), len(v.plan.Files)),
	}, nil
}

func errorIsCancellation(err error) bool {
	return err != nil && (strings.Contains(err.Error(), stdctx.Canceled.Error()) ||
		strings.Contains(err.Error(), stdctx.DeadlineExceeded.Error()))
}

// taskReadiness derives, from the child run's OWN durable step states, whether
// its work may be integrated.
//
// It re-derives rather than trusting "the child reached completed", and that is
// the point of it. A run's state is a summary written by one code path; the
// gate that must hold before a target branch moves is "this specific review
// approved or was policy-skipped, and this specific verification passed or was
// never planned". Deriving it here means the guarantee is enforced at the place
// that can actually move the ref, by reading the same facts a person would.
func taskReadiness(child RunDetail) integration.Readiness {
	out := integration.Readiness{}
	for _, s := range child.Steps {
		switch s.Step.Kind {
		case domain.WorkflowStepReview:
			out.Review = reviewReadiness(s)
		case domain.WorkflowStepVerify:
			out.Verify = verifyReadiness(s)
		}
	}
	// A child run that durably reached `completed` is itself the proof that
	// both gates were passed: maybeVerify only completes a run after its review
	// approved (or was policy-skipped) and its verification passed, and the run
	// state is a durable fact rather than a live read that can come back empty.
	//
	// This is the same reasoning reviewReadiness already applies to a completed
	// review STEP, lifted to the run — and it is what lets the gate be strict
	// about the case that actually matters. "No review step could be read" is
	// ambiguous for a run still in flight and unambiguous for one that finished;
	// refusing the latter would gate on the absence of a record rather than on
	// the absence of approval.
	completed := child.Run.State == domain.WorkflowRunCompleted
	if out.Review == "" {
		if completed {
			out.Review = integration.ReviewApproved
		} else {
			// Neither a verdict nor a finished run: the gate refuses rather
			// than assumes.
			out.Review = integration.ReviewPending
		}
	}
	if out.Verify == "" {
		if completed {
			out.Verify = integration.VerifyPassed
		} else {
			out.Verify = integration.VerifyPending
		}
	}
	return out
}

func reviewReadiness(s StepDetail) integration.ReviewState {
	// A policy-skipped review never created a review run, so the decision
	// checkpoint is the only durable record that the step was legitimately
	// passed rather than left unfinished.
	if s.ReviewPolicy != nil && s.ReviewPolicy.Decision == ReviewSkipped {
		return integration.ReviewSkipped
	}
	if s.Review != nil {
		switch s.Review.Verdict {
		case domain.VerdictApproved:
			return integration.ReviewApproved
		case domain.VerdictChangesRequested:
			return integration.ReviewChangesRequested
		}
	}
	switch s.Step.State {
	case domain.WorkflowStepFailed, domain.WorkflowStepCancelled:
		return integration.ReviewFailed
	case domain.WorkflowStepCompleted:
		// Completed, but neither a verdict nor a skip decision could be read.
		//
		// This is treated as approved, and the asymmetry with the explicit
		// verdicts above is deliberate. StepDetail.Review is a LIVE read of the
		// review run, so it is nil both when there was no review and when the
		// read merely failed this once -- while the step's completion inside a
		// completed run is a durable fact, and one that maybeVerify only ever
		// grants after review approved-or-skipped. Refusing here would turn a
		// transient read failure into a stopped run without adding any
		// guarantee: an unapproved review could not have produced this state.
		//
		// What the explicit branches above add is defence in depth against the
		// opposite error -- a caller reaching this path with a run whose review
		// genuinely did not approve.
		return integration.ReviewApproved
	default:
		// Pending, ready, running or waiting: the review has not answered yet.
		return integration.ReviewPending
	}
}

func verifyReadiness(s StepDetail) integration.VerifyState {
	switch s.Step.State {
	case domain.WorkflowStepCompleted:
		return integration.VerifyPassed
	case domain.WorkflowStepFailed, domain.WorkflowStepCancelled:
		return integration.VerifyFailed
	default:
		// Pending, ready, running or waiting: the verification has not answered
		// yet, and "has not answered" must gate exactly as "failed" does.
		return integration.VerifyPending
	}
}
