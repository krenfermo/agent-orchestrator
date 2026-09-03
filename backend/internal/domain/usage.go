package domain

import (
	"errors"
	"strings"
	"time"
)

// UsageSourceKind identifies the native artifact shape that produced usage
// facts. It is deliberately narrower than AgentHarness: only certified usage
// sources get persisted in the V1 usage pipeline.
type UsageSourceKind string

// UsageSourceKind values identify certified native usage artifact shapes.
const (
	UsageSourceClaudeMain     UsageSourceKind = "claude_main"
	UsageSourceClaudeSubagent UsageSourceKind = "claude_subagent"
	UsageSourceCodexRollout   UsageSourceKind = "codex_rollout"
)

// UsageBindingState tracks the root native-session binding lifecycle.
type UsageBindingState string

// UsageBindingState values describe root native-session binding lifecycle.
const (
	UsageBindingDiscovering UsageBindingState = "discovering"
	UsageBindingActive      UsageBindingState = "active"
	UsageBindingFinalizing  UsageBindingState = "finalizing"
	UsageBindingComplete    UsageBindingState = "complete"
	UsageBindingPartial     UsageBindingState = "partial"
)

// UsageSourceState tracks one physical JSONL artifact generation.
type UsageSourceState string

// UsageSourceState values describe one physical source artifact lifecycle.
const (
	UsageSourcePending  UsageSourceState = "pending"
	UsageSourceActive   UsageSourceState = "active"
	UsageSourceComplete UsageSourceState = "complete"
	UsageSourceError    UsageSourceState = "error"
)

// Usage error code constants are safe storage/display identifiers for
// transcript discovery and ingestion failures.
const (
	UsageErrorSourceDiscoveryPending      = "source_discovery_pending"
	UsageErrorArtifactPathRejected        = "artifact_path_rejected"
	UsageErrorArtifactMissing             = "artifact_missing"
	UsageErrorArtifactReplaced            = "artifact_replaced"
	UsageErrorSourceReadFailed            = "source_read_failed"
	UsageErrorRecordTooLarge              = "record_too_large"
	UsageErrorMalformedJSONL              = "malformed_jsonl"
	UsageErrorUnsupportedSourceFormat     = "unsupported_source_format"
	UsageErrorSourceEventConflict         = "source_event_conflict"
	UsageErrorNonMonotonicCumulativeUsage = "non_monotonic_cumulative_usage"
	UsageErrorInvalidParserState          = "invalid_parser_state"
	UsageErrorUnresolvedSpawnCall         = "unresolved_spawn_call"
	UsageErrorCodexSourceBudgetExceeded   = "codex_source_budget_exceeded"
)

// Usage ingestion sentinel errors report replay and cursor conflicts.
var (
	ErrUsageSourceOffsetConflict   = errors.New("usage source cursor offset conflict")
	ErrUsageSourceRevisionConflict = errors.New("usage source revision conflict")
	ErrUsageSourceEventConflict    = errors.New("usage source event conflict")
)

// UsageSubjectKind names what a usage binding is attached to.
//
// It exists because three execution roles that spend real provider tokens are
// not AO sessions: a reviewer and a decision resolver run in runtime panes, and
// the planner is a bounded `claude --print` subprocess. Before this, a binding
// had to name a row in `sessions`, so none of them could be metered and every
// run total was silently a lower bound.
type UsageSubjectKind string

// UsageSubjectKind values.
const (
	// UsageSubjectSession is an AO session: a worker, and the repair cycles
	// delivered into it. The only kind that carries a SessionID.
	UsageSubjectSession UsageSubjectKind = "session"
	// UsageSubjectRuntimePane is a reviewer or decision-resolver pane,
	// identified by the DURABLE AUTHORITY that owns it -- a review run id, a
	// question-resolution id -- never by which process started most recently.
	UsageSubjectRuntimePane UsageSubjectKind = "runtime_pane"
	// UsageSubjectPlannerInvocation is one planner subprocess call.
	UsageSubjectPlannerInvocation UsageSubjectKind = "planner_invocation"
	// UsageSubjectProviderAttempt is reserved for binding usage directly to a
	// provider attempt. Nothing emits it yet; it exists so a later surface has
	// a name to write rather than a schema change to make.
	UsageSubjectProviderAttempt UsageSubjectKind = "provider_attempt"
)

// String renders the kind.
func (k UsageSubjectKind) String() string { return string(k) }

// Valid reports whether k is a kind storage will accept.
func (k UsageSubjectKind) Valid() bool {
	switch k {
	case UsageSubjectSession, UsageSubjectRuntimePane,
		UsageSubjectPlannerInvocation, UsageSubjectProviderAttempt:
		return true
	default:
		return false
	}
}

// UsageSubject is what a binding and an attribution window are both keyed on.
// The two must agree exactly, or an event resolves to no window.
type UsageSubject struct {
	Kind UsageSubjectKind
	ID   string
}

// SessionSubject is the subject of an AO session.
func SessionSubject(id SessionID) UsageSubject {
	return UsageSubject{Kind: UsageSubjectSession, ID: string(id)}
}

// RuntimePaneSubject is the subject of a reviewer or resolver pane, named by the
// durable authority that owns it.
func RuntimePaneSubject(authorityID string) UsageSubject {
	return UsageSubject{Kind: UsageSubjectRuntimePane, ID: authorityID}
}

// PlannerInvocationSubject is the subject of one planner subprocess call.
func PlannerInvocationSubject(invocationID string) UsageSubject {
	return UsageSubject{Kind: UsageSubjectPlannerInvocation, ID: invocationID}
}

// Valid reports whether the subject can key a binding.
func (s UsageSubject) Valid() bool { return s.Kind.Valid() && strings.TrimSpace(s.ID) != "" }

// String renders the subject as "<kind>:<id>", the form carried in a pane's
// environment so its hooks can report usage against it.
func (s UsageSubject) String() string { return string(s.Kind) + ":" + s.ID }

// ParseUsageSubject reads the "<kind>:<id>" form back. ok is false for anything
// it does not fully recognize -- an unknown kind is refused rather than coerced
// into a session, which would attach a pane's tokens to somebody else's ledger.
func ParseUsageSubject(raw string) (UsageSubject, bool) {
	raw = strings.TrimSpace(raw)
	kind, id, found := strings.Cut(raw, ":")
	if !found {
		return UsageSubject{}, false
	}
	subject := UsageSubject{Kind: UsageSubjectKind(strings.TrimSpace(kind)), ID: strings.TrimSpace(id)}
	if !subject.Valid() {
		return UsageSubject{}, false
	}
	return subject, true
}

// UsageBindingRecord binds one usage SUBJECT to one native root session/thread.
type UsageBindingRecord struct {
	ID      int64
	Subject UsageSubject
	// SessionID is set only when Subject.Kind is session. Storage enforces the
	// correspondence, so a pane binding can never be half-attached to a session
	// it does not belong to.
	SessionID      SessionID
	Harness        AgentHarness
	NativeRootID   string
	InitialModelID string
	State          UsageBindingState
	LastErrorCode  string
	UpdatedAt      time.Time
}

// UsageSourceRecord tracks one physical JSONL artifact generation and its
// durable read cursor.
type UsageSourceRecord struct {
	ID              int64
	BindingID       int64
	Kind            UsageSourceKind
	NativeSessionID string
	SubagentID      string
	ArtifactPath    string
	FileIdentity    string
	Generation      int64
	ByteOffset      int64
	ParserStateJSON string
	State           UsageSourceState
	FailureCount    int64
	AnomalyCount    int64
	NextRetryAt     *time.Time
	LastErrorCode   string
	UpdatedAt       time.Time
}

// UsageSourceContext is the source row plus immutable binding/subject facts the
// ingestor needs while normalizing parser output.
type UsageSourceContext struct {
	Source  UsageSourceRecord
	Subject UsageSubject
	// SessionID is empty for a non-session subject.
	SessionID      SessionID
	NativeRootID   string
	InitialModelID string
	BindingState   UsageBindingState
}

// UsageTokenMetrics is the normalized token vector stored on every usage event
// and returned in aggregate summaries.
type UsageTokenMetrics struct {
	InputTokens         int64
	UncachedInputTokens int64
	CacheReadTokens     int64
	CacheWriteTokens    int64
	OutputTokens        int64
	ReasoningTokens     *int64
}

// ModelUsageEvent is one append-only normalized usage fact.
type ModelUsageEvent struct {
	ModelID string
	Tokens  UsageTokenMetrics
	// SourceEventKey is the exactly-once identity: derived from the artifact,
	// never from a clock, so a re-read of the same transcript re-derives the
	// same key and the insert is a no-op.
	SourceEventKey string
	// ObservedAt is the provider's own timestamp for this event, when the
	// transcript record carried one. It is what lets P3-E place a token inside
	// the role window that was open when it was spent. Nil means the artifact
	// stated no time: role attribution then falls back to the session's first
	// window and is reported as approximate rather than being guessed.
	ObservedAt *time.Time
}

// UsageModelAggregate is the raw model-level aggregate read from storage before
// the service applies user-facing coverage rules.
type UsageModelAggregate struct {
	Harness             AgentHarness
	ModelID             string
	Tokens              UsageTokenMetrics
	ReasoningEventCount int64
}

// CompactSessionUsage is the token-only dashboard read model.
type CompactSessionUsage struct {
	SessionID   SessionID
	TotalTokens int64
	Incomplete  bool
}

// UsageMetricTotals is the aggregate metric block used by session, harness,
// and model summaries.
type UsageMetricTotals struct {
	InputTokens         *int64
	UncachedInputTokens *int64
	CacheReadTokens     *int64
	CacheWriteTokens    *int64
	OutputTokens        *int64
	ReasoningTokens     *int64
}

// ModelUsageSummary is a per-exact-model aggregate.
type ModelUsageSummary struct {
	ModelID string
	Totals  UsageMetricTotals
}

// HarnessUsageSummary groups model summaries by AO harness.
type HarnessUsageSummary struct {
	Harness AgentHarness
	Totals  UsageMetricTotals
	Models  []ModelUsageSummary
}

// SessionUsageSummary is the read model returned by the session usage service.
type SessionUsageSummary struct {
	SessionID  SessionID
	Incomplete bool
	Totals     UsageMetricTotals
	Harnesses  []HarnessUsageSummary
}

// SourceCursorState is the durable source state to commit after parsing a
// chunk. ApplyUsageChunk writes it atomically with the emitted events.
type SourceCursorState struct {
	ByteOffset      int64
	State           UsageSourceState
	ParserStateJSON string
	FailureCount    int64
	AnomalyCount    int64
	NextRetryAt     *time.Time
	LastErrorCode   string
	UpdatedAt       time.Time
}
