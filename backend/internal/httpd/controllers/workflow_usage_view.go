package controllers

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// SessionUsageLookup is the narrow read contract this file needs to attach
// observed token usage to a role. internal/service/usage.SummaryReader
// already satisfies this signature exactly — no adapter needed.
type SessionUsageLookup interface {
	Get(ctx context.Context, sessionID domain.SessionID) (domain.SessionUsageSummary, error)
}

// RoleUsageView is one role's (planner/worker/reviewer/fix_worker/verify)
// provider/model/duration/usage facts for a single workflow step, built from
// that step's latest attempt. Usage is nil (not zero) whenever no session
// usage telemetry is available for the step's session — Checkpoint 8J's
// "never fabricate zero tokens" rule.
type RoleUsageView struct {
	Role         domain.WorkflowRole
	StepKind     domain.WorkflowStepKind
	Harness      string
	Provider     string
	Model        string
	SessionID    string
	StartedAt    *time.Time
	CompletedAt  *time.Time
	DurationMS   *int64
	Usage        *domain.SessionUsageSummary
	UsageKnown   bool
	VerifyChecks *int64 // set only for the verify role, from its VerifyResult
}

// WorkflowUsageView is the full Checkpoint 8J usage/telemetry read model for
// one workflow run: one RoleUsageView per step present, plus the derived
// task-level metrics, session-refresh advisory, and checkpoint summary.
type WorkflowUsageView struct {
	Roles      []RoleUsageView
	Metrics    domain.TaskUsefulWorkMetrics
	Advisory   domain.SessionRefreshAdvisory
	Checkpoint domain.TaskCheckpointSummary
}

// BuildWorkflowUsageView is the pure, deterministic entry point: given an
// already-fetched RunDetail (no new store reads beyond the session-usage
// lookup) it derives every Checkpoint 8J read-model field. It never blocks
// on or triggers ingestion — a session with no usage rows yet simply reports
// UsageKnown=false, not zero.
func BuildWorkflowUsageView(ctx context.Context, detail workflowcore.RunDetail, lookup SessionUsageLookup) WorkflowUsageView {
	roles := make([]RoleUsageView, 0, len(detail.Steps))
	var attempts, reviewRuns, fixCycles int64
	var reviewsSkipped bool
	var verifyDuration *time.Duration
	var verifyChecks *int64
	var totalDuration time.Duration
	var haveDuration bool

	for _, sd := range detail.Steps {
		role := domain.RoleForStepKind(sd.Step.Kind)
		if role == "" {
			continue // "advance" is bookkeeping, not an executed role
		}
		attempts += int64(len(sd.Attempts))
		switch sd.Step.Kind {
		case domain.WorkflowStepReview:
			// A review step's dispatches aren't tracked through the generic
			// workflow_attempts mechanism (unlike work/fix) — the durable
			// record is the step's ReviewRunID plus the live ReviewSummary
			// fetched from review_run. Count either as one review run, on top
			// of any attempts the step does carry, so this metric reflects a
			// real dispatched review regardless of which mechanism recorded
			// it.
			reviewRuns += int64(len(sd.Attempts))
			if sd.Step.ReviewRunID != nil || sd.Review != nil {
				reviewRuns++
			}
			if sd.Step.State == domain.WorkflowStepCompleted && sd.Review == nil && sd.ReviewPolicy != nil && sd.ReviewPolicy.Decision == workflowcore.ReviewSkipped {
				reviewsSkipped = true
			}
		case domain.WorkflowStepFix:
			fixCycles += int64(len(sd.Attempts))
		}

		rv := RoleUsageView{Role: role, StepKind: sd.Step.Kind, Harness: sd.Step.AssignedHarness, Provider: domain.ProviderForHarness(domain.AgentHarness(sd.Step.AssignedHarness))}
		if sd.Step.SessionID != nil {
			rv.SessionID = *sd.Step.SessionID
		}
		if latest := latestAttempt(sd.Attempts); latest != nil {
			rv.Harness = latest.Harness
			rv.Provider = domain.ProviderForHarness(domain.AgentHarness(latest.Harness))
			rv.Model = latest.Model
			started := latest.StartedAt
			rv.StartedAt = &started
			rv.CompletedAt = latest.FinishedAt
			if latest.FinishedAt != nil {
				d := latest.FinishedAt.Sub(latest.StartedAt)
				ms := d.Milliseconds()
				rv.DurationMS = &ms
				if sd.Step.Kind == domain.WorkflowStepVerify {
					verifyDuration = &d
				}
				totalDuration += d
				haveDuration = true
			}
		}
		if sd.Step.Kind == domain.WorkflowStepVerify {
			if checks := verifyCheckCount(sd); checks != nil {
				verifyChecks = checks
				rv.VerifyChecks = checks
			}
		}
		if rv.SessionID != "" && lookup != nil {
			if summary, err := lookup.Get(ctx, domain.SessionID(rv.SessionID)); err == nil {
				rv.Usage = &summary
				rv.UsageKnown = summary.Totals.InputTokens != nil || summary.Totals.OutputTokens != nil
			}
		}
		roles = append(roles, rv)
	}

	metrics := domain.TaskUsefulWorkMetrics{
		Attempts:         attempts,
		ReviewRuns:       reviewRuns,
		FixCycles:        fixCycles,
		ReviewsSkipped:   reviewsSkipped,
		VerifyDuration:   verifyDuration,
		VerifyCheckCount: verifyChecks,
		TokensCertainty:  domain.MetricUnknown,
	}
	if haveDuration {
		metrics.Duration = &totalDuration
	}
	if in, out, cached, ok := sumKnownTokens(roles); ok {
		metrics.InputTokens = &in
		metrics.OutputTokens = &out
		metrics.CachedTokens = &cached
		metrics.TokensCertainty = domain.MetricActual
	}

	return WorkflowUsageView{
		Roles:      roles,
		Metrics:    metrics,
		Advisory:   BuildSessionRefreshAdvisory(detail, fixCycles),
		Checkpoint: BuildTaskCheckpointSummary(detail),
	}
}

func latestAttempt(attempts []domain.WorkflowAttempt) *domain.WorkflowAttempt {
	var latest *domain.WorkflowAttempt
	for i := range attempts {
		a := &attempts[i]
		if latest == nil || a.AttemptNumber > latest.AttemptNumber {
			latest = a
		}
	}
	return latest
}

func verifyCheckCount(sd workflowcore.StepDetail) *int64 {
	if sd.LatestCheckpoint == nil {
		return nil
	}
	// workflowRunDetailView (workflow.go) already knows how to unmarshal a
	// verify step's VerifyResult from its latest checkpoint's RetryState;
	// reuse the same extraction here rather than re-deriving it.
	result := extractVerifyResult(sd)
	if result == nil {
		return nil
	}
	n := int64(len(result.Checks))
	return &n
}

func sumKnownTokens(roles []RoleUsageView) (input, output, cached int64, ok bool) {
	found := false
	for _, r := range roles {
		if r.Usage == nil {
			continue
		}
		if r.Usage.Totals.InputTokens != nil {
			input += *r.Usage.Totals.InputTokens
			found = true
		}
		if r.Usage.Totals.OutputTokens != nil {
			output += *r.Usage.Totals.OutputTokens
			found = true
		}
		if r.Usage.Totals.CacheReadTokens != nil {
			cached += *r.Usage.Totals.CacheReadTokens
		}
	}
	return input, output, cached, found
}
