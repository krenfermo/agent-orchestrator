package workflow

import (
	"encoding/json"
	"strconv"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// TaskCheckpointSummaryInput is BuildTaskCheckpointSummary's pure input.
// Detail is required; Artifact and Questions are optional enrichments a
// caller supplies when it already has them at hand (never a new store
// round-trip inside the builder itself, keeping it deterministic given its
// inputs).
type TaskCheckpointSummaryInput struct {
	Detail    RunDetail
	Artifact  *PlanArtifact
	Questions []domain.WorkflowQuestion
}

// decodeVerifyResultFromCheckpoint mirrors httpd/controllers'
// extractVerifyResult exactly (same DurablePhase check, same JSON shape) —
// moved here so both the usage-telemetry read model and Checkpoint 8M's
// context-pack builder read the identical durable fact the same way,
// instead of two independent unmarshal paths.
func decodeVerifyResultFromCheckpoint(sd StepDetail) (VerifyResult, bool) {
	if sd.LatestCheckpoint == nil || sd.Step.Kind != domain.WorkflowStepVerify || sd.LatestCheckpoint.DurablePhase != "verify_result" {
		return VerifyResult{}, false
	}
	var result VerifyResult
	if json.Unmarshal([]byte(sd.LatestCheckpoint.RetryState), &result) != nil {
		return VerifyResult{}, false
	}
	return result, true
}

// BuildTaskCheckpointSummary deterministically derives Checkpoint 8J's
// TaskCheckpointSummary from facts already durable in RunDetail, plus two
// optional enrichments this checkpoint (8M) adds real data sources for:
//
//   - RelevantFiles: the plan artifact's own declared Verification.Files
//     paths (what the task expects to touch) when an Artifact is supplied —
//     never invented, never derived from prose.
//   - FilesChanged: the review step's own persisted ReviewPolicy decision
//     facts (ChangedFilePaths), when a review happened — the exact same
//     observed-diff facts ReviewPolicy itself decided on, not a second
//     computation.
//   - Decisions: one line per answered question (from Questions), reusing
//     8K's own QuestionText/AnswerText/AnswerSource facts verbatim — never a
//     free-text summary of the conversation that produced them.
//   - Tests: each Verify check's own Label (the exact command/file check
//     that ran), reusing VerifyResult — not a guess at what "tests exist".
//
// ArchitecturalFacts stays empty: no durable source for it exists anywhere
// in AO's data model today, and this function never fabricates one.
//
// No chain-of-thought, no transcript: every field here is either copied
// verbatim from an existing column/JSON blob or a short deterministic
// derivation of one — the same invariant 8J's original version already
// upheld (checkpoint 8M brief §2/§6).
func BuildTaskCheckpointSummary(in TaskCheckpointSummaryInput) domain.TaskCheckpointSummary {
	detail := in.Detail
	summary := domain.TaskCheckpointSummary{Objective: detail.Run.Objective, NextAction: detail.NextAction}

	matchedTask := false
	for _, task := range detail.Tasks {
		if task.ExecutionRunID != nil && *task.ExecutionRunID == detail.Run.ID {
			summary.Task = task.Title
			var criteria []string
			_ = json.Unmarshal([]byte(task.AcceptanceCriteriaJSON), &criteria)
			summary.AcceptanceCriteria = criteria
			matchedTask = true
			break
		}
	}
	if !matchedTask && in.Artifact != nil {
		// Single-task (non-master-plan) runs have no workflow_tasks row at
		// all — fall back to the plan artifact's own acceptance criteria so
		// the common case (one objective, one run) still yields a non-empty
		// AcceptanceCriteria rather than silently staying empty.
		summary.AcceptanceCriteria = in.Artifact.AcceptanceCriteria
	}
	if in.Artifact != nil {
		relevant := make([]string, 0, len(in.Artifact.Verification.Files))
		for _, fc := range in.Artifact.Verification.Files {
			relevant = append(relevant, fc.Path)
		}
		summary.RelevantFiles = relevant
	}

	var latestCheckpoint *domain.WorkflowCheckpoint
	for _, sd := range detail.Steps {
		if sd.LatestCheckpoint != nil && (latestCheckpoint == nil || sd.LatestCheckpoint.CreatedAt.After(latestCheckpoint.CreatedAt)) {
			latestCheckpoint = sd.LatestCheckpoint
		}
		if sd.Step.Kind == domain.WorkflowStepReview {
			if sd.Review != nil && sd.Review.FindingsSummary != "" {
				summary.LatestReviewFindings = sd.Review.FindingsSummary
			}
			if sd.ReviewPolicy != nil && len(sd.ReviewPolicy.Facts.ChangedFilePaths) > 0 {
				summary.FilesChanged = sd.ReviewPolicy.Facts.ChangedFilePaths
			}
		}
		if sd.Step.Kind == domain.WorkflowStepVerify {
			if result, ok := decodeVerifyResultFromCheckpoint(sd); ok {
				for _, check := range result.Checks {
					summary.Tests = append(summary.Tests, check.Label)
					if !check.Passed {
						summary.ActiveErrors = append(summary.ActiveErrors, check.Label+": "+check.FailureReason)
					}
				}
			}
		}
		for _, a := range sd.Attempts {
			if a.Outcome == domain.WorkflowAttemptFailed && a.ErrorClass != "" {
				summary.ActiveErrors = append(summary.ActiveErrors, string(sd.Step.Kind)+" attempt "+strconv.FormatInt(a.AttemptNumber, 10)+": "+string(a.ErrorClass))
			}
		}
	}
	if latestCheckpoint != nil {
		summary.CurrentFingerprint = latestCheckpoint.FingerprintAfter
		if summary.CurrentFingerprint == "" {
			summary.CurrentFingerprint = latestCheckpoint.FingerprintBefore
		}
	}

	for _, q := range in.Questions {
		if q.AnswerText == "" {
			continue
		}
		source := "human"
		if q.AnswerSource != nil {
			source = string(*q.AnswerSource)
		}
		summary.Decisions = append(summary.Decisions, q.QuestionText+" -> "+q.AnswerText+" ("+source+")")
	}

	return summary
}
