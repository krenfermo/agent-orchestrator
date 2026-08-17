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
	// Decisions is Checkpoint 8K-B pass 3's read-time-derived Decision
	// Resolver telemetry, built from workflow_questions +
	// workflow_question_resolutions. Zero value (all counts 0, no
	// provider/duration) when the run has no questions at all — never
	// omitted, since "zero questions asked" is a real, knowable fact,
	// unlike token counts (see DecisionsUsageView's own doc comment for
	// which of its fields follow the opposite "unknown != 0" rule).
	Decisions DecisionsUsageView
	// Routing is Checkpoint 8L's per-role ExecutionRouter telemetry, one
	// entry per step that has a persisted routing_decision checkpoint. Nil
	// entries are never fabricated: a step with no routing decision yet
	// (e.g. a run created before 8L, or a step kind ExecutionRouter does not
	// route) is simply absent from the slice.
	Routing []RoutingUsageView
}

// RoutingUsageView is one step's Checkpoint 8L routing telemetry, derived
// read-time from that step's routing_decision checkpoint — no new
// instrumentation beyond what routeWorkerDispatch/routeReviewerDispatch
// already persist.
type RoutingUsageView struct {
	Role                    domain.WorkflowRole
	StepKind                domain.WorkflowStepKind
	RoutingDecision         domain.RoutingDecision
	CapacityStateAtDecision map[domain.AgentHarness]domain.CapacityState
	FallbackUsed            bool
}

// DecisionsUsageView is Checkpoint 8K-B pass 3's telemetry section: derived
// entirely read-time from already-persisted workflow_questions and
// workflow_question_resolutions rows, no new write-time instrumentation.
//
// QuestionsAsked/PolicyResolved/TechnicalResolved/HumanRequired/
// ResolverFailed/WaitingForCapacity are plain row counts — always truly
// knowable (0 is a real fact, not a fabrication) so they are never pointers.
// ResolverProvider and ResolverDurationMS follow the OPPOSITE "unknown !=
// 0" rule: nil/"" when no resolution row exists to derive them from, never
// a fabricated empty/zero. ResolverDurationMS sums CompletedAt-CreatedAt
// across every COMPLETE resolution attempt in the run (both auto-resolved
// and requires_human outcomes) that has a CompletedAt.
//
// ReusedDecision is deliberately always nil: whether a captured question
// hit InsertWorkflowQuestion's fingerprint-dedup no-op path (see
// detector.go's DetectResult.Inserted) is a per-call, in-memory fact that
// is NOT persisted anywhere on the workflow_questions row itself, so it
// cannot be reconstructed read-time from the current schema without adding
// a new column — out of scope for this pass (no new migrations). Left nil
// rather than fabricated as 0, consistent with the rest of this view's
// "unknown != 0" fields.
type DecisionsUsageView struct {
	QuestionsAsked     int64
	PolicyResolved     int64
	TechnicalResolved  int64
	HumanRequired      int64
	ResolverFailed     int64
	WaitingForCapacity int64
	ResolverProvider   string
	ResolverDurationMS *int64
	ReusedDecision     *int64
}

// BuildDecisionsUsageView derives Checkpoint 8K-B pass 3's telemetry
// section from a run's already-fetched questions and resolutions — no new
// store reads beyond what the caller already performed for the Questions
// section and this view.
func BuildDecisionsUsageView(qs []domain.WorkflowQuestion, resolutions []domain.WorkflowQuestionResolution) DecisionsUsageView {
	var v DecisionsUsageView
	v.QuestionsAsked = int64(len(qs))
	for _, q := range qs {
		if q.AnswerSource != nil {
			switch *q.AnswerSource {
			case domain.AnswerSourcePolicy:
				v.PolicyResolved++
			case domain.AnswerSourceResolver:
				v.TechnicalResolved++
			}
		}
		if q.State == domain.QuestionStateHumanRequired {
			v.HumanRequired++
		}
		if q.State == domain.QuestionStateResolving && q.ResolvingRunID == nil {
			// Mirrors reconcileDecisionResolvers' own read-time
			// "waiting_for_capacity" derivation (decision_resolver_wiring.go):
			// resolving with no dispatched attempt yet means every usable
			// provider was unavailable on the last reconcile pass.
			v.WaitingForCapacity++
		}
	}

	var totalDuration time.Duration
	var haveDuration bool
	var latestResolverHarness domain.AgentHarness
	var latestCreatedAt time.Time
	for _, r := range resolutions {
		if r.Status == domain.ResolutionStatusFailed {
			v.ResolverFailed++
		}
		if r.CompletedAt != nil {
			totalDuration += r.CompletedAt.Sub(r.CreatedAt)
			haveDuration = true
		}
		if r.ResolverHarness != "" && r.CreatedAt.After(latestCreatedAt) {
			latestCreatedAt = r.CreatedAt
			latestResolverHarness = r.ResolverHarness
		}
	}
	if latestResolverHarness != "" {
		v.ResolverProvider = domain.ProviderForHarness(latestResolverHarness)
	}
	if haveDuration {
		ms := totalDuration.Milliseconds()
		v.ResolverDurationMS = &ms
	}
	return v
}

// BuildWorkflowUsageView is the pure, deterministic entry point: given an
// already-fetched RunDetail (no new store reads beyond the session-usage
// lookup) it derives every Checkpoint 8J read-model field. It never blocks
// on or triggers ingestion — a session with no usage rows yet simply reports
// UsageKnown=false, not zero.
func BuildWorkflowUsageView(ctx context.Context, detail workflowcore.RunDetail, lookup SessionUsageLookup) WorkflowUsageView {
	roles := make([]RoleUsageView, 0, len(detail.Steps))
	var routing []RoutingUsageView
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
		if sd.Routing != nil {
			fallbackUsed := sd.Routing.SelectedHarness != "" && sd.Routing.SelectedHarness != sd.Routing.PreferredHarness
			routing = append(routing, RoutingUsageView{
				Role: role, StepKind: sd.Step.Kind, RoutingDecision: *sd.Routing,
				CapacityStateAtDecision: sd.Routing.CapacityStateAtDecision, FallbackUsed: fallbackUsed,
			})
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
		Routing:    routing,
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
