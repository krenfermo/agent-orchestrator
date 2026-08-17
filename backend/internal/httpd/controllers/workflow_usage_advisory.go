package controllers

import (
	"encoding/json"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// extractVerifyResult mirrors workflowRunDetailView's own inline extraction
// (workflow.go) so both call sites read the exact same durable fact the same
// way, rather than maintaining two slightly different unmarshal paths.
func extractVerifyResult(sd workflowcore.StepDetail) *workflowcore.VerifyResult {
	if sd.LatestCheckpoint == nil || sd.Step.Kind != domain.WorkflowStepVerify || sd.LatestCheckpoint.DurablePhase != "verify_result" {
		return nil
	}
	var result workflowcore.VerifyResult
	if json.Unmarshal([]byte(sd.LatestCheckpoint.RetryState), &result) != nil {
		return nil
	}
	return &result
}

// BuildSessionRefreshAdvisory computes Checkpoint 8J's advisory-only
// session-refresh recommendation from observable facts already on the
// fetched RunDetail — no new instrumentation, no thresholds tied to a
// commercial plan. Every signal and its source is listed in Signals so the
// recommendation is auditable, not a black box.
//
// This never triggers any action: nothing reads this advisory to restart,
// compact, or replace a session. It is a read-model only.
func BuildSessionRefreshAdvisory(detail workflowcore.RunDetail, fixCycleAttempts int64) domain.SessionRefreshAdvisory {
	budget := int64(domain.DefaultWorkflowPolicy().MaxFixCycles)
	var signals []string

	workStep, hasWork := stepByKind(detail, domain.WorkflowStepWork)
	if !hasWork {
		return domain.SessionRefreshAdvisory{Recommendation: domain.RefreshUnknown, Reason: "no work step recorded yet", Signals: nil}
	}

	// Signal: task boundary. Each planned task already gets its own fresh
	// Codex/Claude session by construction (one Spawner.Spawn per work
	// step) — a session is never reused across two different planned
	// tasks. This is a structural fact worth surfacing, not a judgment.
	signals = append(signals, "session created for this task's own work step (session-per-task is the current, unoptimized default)")

	// Signal: fix-cycle load relative to the same budget the coordinator
	// itself enforces (domain.DefaultWorkflowPolicy().MaxFixCycles) — not
	// an invented number.
	if fixCycleAttempts > 0 {
		signals = append(signals, factSignal("fix cycles observed on this task", fixCycleAttempts, budget))
	}

	// Signal: attempt count on the work step itself (independent of fix
	// cycles — a work step can retry on transient/rate-limit failures).
	workAttempts := int64(len(workStep.Attempts))
	if workAttempts > 1 {
		signals = append(signals, factSignal("work-step attempts", workAttempts, 0))
	}

	switch {
	case fixCycleAttempts >= budget && budget > 0:
		return domain.SessionRefreshAdvisory{
			Recommendation: domain.RefreshRecommendNewSession,
			Reason:         "fix-cycle budget reached on the live session; further reuse risks compounding an unresolved review disagreement, not accumulating useful context",
			Signals:        signals,
		}
	case fixCycleAttempts > 0:
		return domain.SessionRefreshAdvisory{
			Recommendation: domain.RefreshConsiderCompaction,
			Reason:         "at least one fix cycle has run on this session; context has grown beyond the original task framing but is not yet at the fix-cycle budget",
			Signals:        signals,
		}
	default:
		return domain.SessionRefreshAdvisory{
			Recommendation: domain.RefreshReuse,
			Reason:         "fresh session for this task, no fix cycles yet — no signal suggests refreshing",
			Signals:        signals,
		}
	}
}

func factSignal(label string, value, budget int64) string {
	if budget > 0 {
		return label + ": " + itoa(value) + " of " + itoa(budget) + " budgeted"
	}
	return label + ": " + itoa(value)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func stepByKind(detail workflowcore.RunDetail, kind domain.WorkflowStepKind) (workflowcore.StepDetail, bool) {
	for _, sd := range detail.Steps {
		if sd.Step.Kind == kind {
			return sd, true
		}
	}
	return workflowcore.StepDetail{}, false
}

// BuildTaskCheckpointSummary deterministically derives Checkpoint 8J's
// TaskCheckpointSummary from facts already durable in RunDetail — the
// run's objective, its master-plan task record (when present), the latest
// review's findings, the latest checkpoint's fingerprint/branch/next-action,
// and verify's error class when present. No chain-of-thought, no transcript
// content: every field here is either copied verbatim from an existing
// column or a short deterministic derivation of one.
func BuildTaskCheckpointSummary(detail workflowcore.RunDetail) domain.TaskCheckpointSummary {
	return workflowcore.BuildTaskCheckpointSummary(workflowcore.TaskCheckpointSummaryInput{Detail: detail})
}
