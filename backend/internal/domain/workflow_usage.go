package domain

import "time"

// WorkflowRole distinguishes the functional role an attempt/session played
// in a workflow, independent of harness. A Claude worker and a Claude
// reviewer share a harness but are different roles — usage/efficiency
// questions ("is Claude better as a worker or a reviewer") are meaningless
// without this distinction (Checkpoint 8J).
type WorkflowRole string

// WorkflowRole values. Verify is included even though it never invokes an
// LLM: it is still measurable as deterministic execution duration/check
// count, and callers must not skip it when reasoning about a task's full
// role lineup.
const (
	WorkflowRolePlanner   WorkflowRole = "planner"
	WorkflowRoleWorker    WorkflowRole = "worker"
	WorkflowRoleReviewer  WorkflowRole = "reviewer"
	WorkflowRoleFixWorker WorkflowRole = "fix_worker"
	WorkflowRoleVerify    WorkflowRole = "verify"
)

// RoleForStepKind maps a workflow step kind to the role that ran it. "advance"
// has no associated role (it is bookkeeping, not execution) and maps to "".
func RoleForStepKind(kind WorkflowStepKind) WorkflowRole {
	switch kind {
	case WorkflowStepPlan:
		return WorkflowRolePlanner
	case WorkflowStepWork:
		return WorkflowRoleWorker
	case WorkflowStepReview:
		return WorkflowRoleReviewer
	case WorkflowStepFix:
		return WorkflowRoleFixWorker
	case WorkflowStepVerify:
		return WorkflowRoleVerify
	default:
		return ""
	}
}

// UsageChannel classifies how a provider invocation is billed, when that is
// actually knowable. Never inferred from "binary happens to be named
// claude" — only a real signal (a future auth/billing probe) may set this to
// anything other than unknown. Kept as a first-class type now so a future
// signal source has somewhere to write without a schema change.
type UsageChannel string

// UsageChannel values.
const (
	UsageChannelSubscription UsageChannel = "subscription"
	UsageChannelMetered      UsageChannel = "metered"
	UsageChannelUnknown      UsageChannel = "unknown"
)

// MetricCertainty marks whether a reported metric is a real observed value,
// a derived/estimated one, or genuinely not known — the "actual/estimated/
// unknown" vocabulary Checkpoint 8J requires on every metric. Mirrors
// workflow.ClassificationCertainty's actual/inferred/unknown values (that
// package's own equivalent for failure classification) so the codebase uses
// one certainty vocabulary rather than two different ones for the same idea.
type MetricCertainty string

// MetricCertainty values.
const (
	MetricActual   MetricCertainty = "actual"
	MetricInferred MetricCertainty = "inferred"
	MetricUnknown  MetricCertainty = "unknown"
)

// ProviderForHarness maps a harness to the LLM vendor that runs it — a
// static, deterministic naming fact (not a commercial/quota assumption).
// Returns "" for a harness with no known single-vendor mapping.
func ProviderForHarness(h AgentHarness) string {
	switch h {
	case AgentHarness("claude-code"):
		return "anthropic"
	case AgentHarness("codex"):
		return "openai"
	default:
		return ""
	}
}

// CapacityState is a read-time snapshot of one harness's dispatch capacity,
// derived from the existing Checkpoint 8H agent_health_events record (never
// a new source of truth — this type adds no storage of its own). Model is
// optional: 8H's health events are scoped to harness only, not per-model.
type CapacityState string

// CapacityState values (Checkpoint 8J §8). Distinct from AgentHealthState's
// vocabulary only in name ("limited" vs "cooldown") to match the checkpoint's
// requested wording exactly; the semantics are identical to AgentHealthState.
const (
	CapacityAvailable   CapacityState = "available"
	CapacityLimited     CapacityState = "limited"
	CapacityCooldown    CapacityState = "cooldown"
	CapacityUnavailable CapacityState = "unavailable"
	CapacityUnknown     CapacityState = "unknown"
)

// CapacitySnapshot is the read-model exposed to Settings/dashboard consumers.
// DetectedAt/ResetAt/Reason are populated only when a real agent_health_events
// row exists; ResetAt stays nil whenever 8H itself never recorded a reset
// (see workflow/health.go's own doc comment: no reset timestamp is invented).
type CapacitySnapshot struct {
	Provider   string
	Harness    AgentHarness
	Model      *string
	State      CapacityState
	DetectedAt *time.Time
	ResetAt    *time.Time
	Reason     string
	Certainty  MetricCertainty
}

// SessionRefreshRecommendation is the advisory-only session-refresh signal
// Checkpoint 8J introduces. It is never acted on automatically — nothing in
// this checkpoint restarts, compacts, or replaces a session; the coordinator
// keeps making its existing session-reuse decisions unchanged. This purely
// reports a recommendation for a human or a later checkpoint to act on.
type SessionRefreshRecommendation string

// SessionRefreshRecommendation values.
const (
	RefreshReuse               SessionRefreshRecommendation = "REUSE"
	RefreshConsiderCompaction  SessionRefreshRecommendation = "CONSIDER_COMPACTION"
	RefreshRecommendNewSession SessionRefreshRecommendation = "RECOMMEND_NEW_SESSION"
	RefreshUnknown             SessionRefreshRecommendation = "UNKNOWN"
)

// SessionRefreshAdvisory is the full advisory output: the recommendation
// plus the observable signals that produced it, so a reader can judge the
// recommendation rather than trust it blindly.
type SessionRefreshAdvisory struct {
	Recommendation SessionRefreshRecommendation
	Reason         string
	Signals        []string
}

// TaskCheckpointSummary is the durable-facts object Checkpoint 8J's data
// model prepares for a future checkpoint to use when opening a clean
// session. It carries fact-shaped fields only — no chain-of-thought, no
// transcript — deterministically derived from data AO already persists
// (workflow_tasks, workflow_checkpoints, review findings, verify results).
// Nothing in 8J opens a new session automatically from this; it is a read
// model only.
type TaskCheckpointSummary struct {
	Objective            string
	Task                 string
	AcceptanceCriteria   []string
	RelevantFiles        []string
	FilesChanged         []string
	ArchitecturalFacts   []string
	Decisions            []string
	Tests                []string
	LatestReviewFindings string
	ActiveErrors         []string
	CurrentFingerprint   string
	NextAction           string
}

// TaskUsefulWorkMetrics are the derived efficiency signals Checkpoint 8J
// requires, computed only from data already recorded (attempts, checkpoints,
// review runs) — never a cost figure, since no reliable billing signal
// exists yet (see UsageChannel).
type TaskUsefulWorkMetrics struct {
	Attempts         int64
	Duration         *time.Duration
	ReviewRuns       int64
	FixCycles        int64
	ReviewsSkipped   bool
	VerifyDuration   *time.Duration
	VerifyCheckCount *int64
	// Token fields are populated only when session usage telemetry is
	// actually available (Certainty != MetricUnknown); nil, not zero, when
	// not known — Checkpoint 8J's "no fabricated zero tokens" requirement.
	InputTokens     *int64
	OutputTokens    *int64
	CachedTokens    *int64
	TokensCertainty MetricCertainty
}
