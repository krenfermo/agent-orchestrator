package controllers

// RoleUsageResponse is one role's provider/model/duration/usage facts for a
// workflow step. Usage is null, never a zeroed object, when no session
// telemetry is available yet (Checkpoint 8J §16: "Si tokens unknown: mostrar
// 'Unknown' NO 0").
type RoleUsageResponse struct {
	Role         string                `json:"role"`
	StepKind     string                `json:"stepKind"`
	Harness      string                `json:"harness,omitempty"`
	Provider     string                `json:"provider,omitempty"`
	Model        string                `json:"model,omitempty"`
	SessionID    string                `json:"sessionId,omitempty"`
	StartedAt    *string               `json:"startedAt,omitempty"`
	CompletedAt  *string               `json:"completedAt,omitempty"`
	DurationMS   *int64                `json:"durationMs,omitempty"`
	Usage        *SessionUsageResponse `json:"usage"`
	VerifyChecks *int64                `json:"verifyChecks,omitempty"`
}

// TaskUsefulWorkMetricsResponse mirrors domain.TaskUsefulWorkMetrics for the
// wire. Token fields stay null unless tokensCertainty is "actual".
type TaskUsefulWorkMetricsResponse struct {
	Attempts         int64  `json:"attempts"`
	DurationMS       *int64 `json:"durationMs,omitempty"`
	ReviewRuns       int64  `json:"reviewRuns"`
	FixCycles        int64  `json:"fixCycles"`
	ReviewsSkipped   bool   `json:"reviewsSkipped"`
	VerifyDurationMS *int64 `json:"verifyDurationMs,omitempty"`
	VerifyCheckCount *int64 `json:"verifyCheckCount,omitempty"`
	InputTokens      *int64 `json:"inputTokens"`
	OutputTokens     *int64 `json:"outputTokens"`
	CachedTokens     *int64 `json:"cachedTokens"`
	TokensCertainty  string `json:"tokensCertainty"`
}

// SessionRefreshAdvisoryResponse is the advisory-only recommendation. Never
// acted on by AO itself.
type SessionRefreshAdvisoryResponse struct {
	Recommendation string   `json:"recommendation"`
	Reason         string   `json:"reason"`
	Signals        []string `json:"signals,omitempty"`
}

// TaskCheckpointSummaryResponse is the durable-facts object — no transcript,
// no chain-of-thought.
type TaskCheckpointSummaryResponse struct {
	Objective            string   `json:"objective"`
	Task                 string   `json:"task,omitempty"`
	AcceptanceCriteria   []string `json:"acceptanceCriteria,omitempty"`
	RelevantFiles        []string `json:"relevantFiles,omitempty"`
	FilesChanged         []string `json:"filesChanged,omitempty"`
	ArchitecturalFacts   []string `json:"architecturalFacts,omitempty"`
	Decisions            []string `json:"decisions,omitempty"`
	Tests                []string `json:"tests,omitempty"`
	LatestReviewFindings string   `json:"latestReviewFindings,omitempty"`
	ActiveErrors         []string `json:"activeErrors,omitempty"`
	CurrentFingerprint   string   `json:"currentFingerprint,omitempty"`
	NextAction           string   `json:"nextAction,omitempty"`
}

// DecisionsUsageResponse is Checkpoint 8K-B pass 3's Decision Resolver
// telemetry section. See DecisionsUsageView's doc comment for which fields
// are plain counts (always present, 0 is a real fact) versus which follow
// the "unknown != 0" rule (omitted/null when not observable).
type DecisionsUsageResponse struct {
	QuestionsAsked     int64  `json:"questionsAsked"`
	PolicyResolved     int64  `json:"policyResolved"`
	TechnicalResolved  int64  `json:"technicalResolved"`
	HumanRequired      int64  `json:"humanRequired"`
	ResolverFailed     int64  `json:"resolverFailed"`
	WaitingForCapacity int64  `json:"waitingForCapacity"`
	ResolverProvider   string `json:"resolverProvider,omitempty"`
	ResolverDurationMS *int64 `json:"resolverDurationMs,omitempty"`
	// ReusedDecision is always null on the wire in this pass: see
	// DecisionsUsageView's doc comment for why it is not yet observable
	// read-time without a new column (deferred, no new migration this pass).
	ReusedDecision *int64 `json:"reusedDecision,omitempty"`
}

// RoutingUsageResponse is one step's Checkpoint 8L ExecutionRouter decision
// on the wire — reason codes, policy version, preferred/selected harness and
// the capacity snapshot consulted, so the frontend can compare e.g.
// Claude-worker/Codex-reviewer vs Codex-worker/Claude-reviewer runs without
// any cost claim.
type RoutingUsageResponse struct {
	Role                    string            `json:"role"`
	StepKind                string            `json:"stepKind"`
	PreferredHarness        string            `json:"preferredHarness,omitempty"`
	SelectedHarness         string            `json:"selectedHarness,omitempty"`
	FallbackOrder           []string          `json:"fallbackOrder,omitempty"`
	FallbackUsed            bool              `json:"fallbackUsed"`
	ReasonCodes             []string          `json:"reasonCodes"`
	PolicyVersion           string            `json:"policyVersion"`
	Waiting                 bool              `json:"waiting"`
	CapacityStateAtDecision map[string]string `json:"capacityStateAtDecision,omitempty"`
}

// SessionLifecycleDecisionResponse is one Checkpoint 8M lifecycle decision
// on the wire — action/reasons/policy version plus the session ids and
// optional context-pack hash/role, mirroring RoutingUsageResponse's shape.
// Never includes the context pack's own content (only its hash): the pack
// itself is facts derived from data the frontend already has access to via
// Checkpoint/Decisions/Routing, not a new blob to ship over the wire.
type SessionLifecycleDecisionResponse struct {
	Action          string   `json:"action"`
	Reasons         []string `json:"reasons"`
	PolicyVersion   string   `json:"policyVersion"`
	Role            string   `json:"role,omitempty"`
	FromSessionID   string   `json:"fromSessionId,omitempty"`
	ToSessionID     string   `json:"toSessionId,omitempty"`
	ContextPackHash string   `json:"contextPackHash,omitempty"`
	CreatedAt       string   `json:"createdAt,omitempty"`
}

// SessionLifecycleUsageResponse is Checkpoint 8M's telemetry section.
type SessionLifecycleUsageResponse struct {
	SessionsCreated     int64                              `json:"sessionsCreated"`
	SessionsReused      int64                              `json:"sessionsReused"`
	SessionsCompacted   int64                              `json:"sessionsCompacted"`
	ContextPacksCreated int64                              `json:"contextPacksCreated"`
	SessionSwitches     int64                              `json:"sessionSwitches"`
	Decisions           []SessionLifecycleDecisionResponse `json:"decisions,omitempty"`
}

// WorkflowUsageResponse is the full Checkpoint 8J usage section embedded in
// a workflow run detail response.
type WorkflowUsageResponse struct {
	Roles            []RoleUsageResponse            `json:"roles"`
	Metrics          TaskUsefulWorkMetricsResponse  `json:"metrics"`
	Advisory         SessionRefreshAdvisoryResponse `json:"advisory"`
	Checkpoint       TaskCheckpointSummaryResponse  `json:"checkpoint"`
	Decisions        DecisionsUsageResponse         `json:"decisions"`
	Routing          []RoutingUsageResponse         `json:"routing,omitempty"`
	SessionLifecycle SessionLifecycleUsageResponse  `json:"sessionLifecycle"`
}

func workflowUsageResponse(v WorkflowUsageView) WorkflowUsageResponse {
	roles := make([]RoleUsageResponse, 0, len(v.Roles))
	for _, r := range v.Roles {
		rr := RoleUsageResponse{
			Role: string(r.Role), StepKind: string(r.StepKind), Harness: r.Harness,
			Provider: r.Provider, Model: r.Model, SessionID: r.SessionID,
			DurationMS: r.DurationMS, VerifyChecks: r.VerifyChecks,
		}
		if r.StartedAt != nil {
			s := r.StartedAt.Format(rfc3339Milli)
			rr.StartedAt = &s
		}
		if r.CompletedAt != nil {
			s := r.CompletedAt.Format(rfc3339Milli)
			rr.CompletedAt = &s
		}
		if r.Usage != nil {
			u := sessionUsageResponse(*r.Usage)
			rr.Usage = &u
		}
		roles = append(roles, rr)
	}
	m := v.Metrics
	metrics := TaskUsefulWorkMetricsResponse{
		Attempts: m.Attempts, ReviewRuns: m.ReviewRuns, FixCycles: m.FixCycles,
		ReviewsSkipped: m.ReviewsSkipped, VerifyCheckCount: m.VerifyCheckCount,
		InputTokens: m.InputTokens, OutputTokens: m.OutputTokens, CachedTokens: m.CachedTokens,
		TokensCertainty: string(m.TokensCertainty),
	}
	if m.Duration != nil {
		ms := m.Duration.Milliseconds()
		metrics.DurationMS = &ms
	}
	if m.VerifyDuration != nil {
		ms := m.VerifyDuration.Milliseconds()
		metrics.VerifyDurationMS = &ms
	}
	return WorkflowUsageResponse{
		Roles:   roles,
		Metrics: metrics,
		Advisory: SessionRefreshAdvisoryResponse{
			Recommendation: string(v.Advisory.Recommendation), Reason: v.Advisory.Reason, Signals: v.Advisory.Signals,
		},
		Checkpoint: TaskCheckpointSummaryResponse{
			Objective: v.Checkpoint.Objective, Task: v.Checkpoint.Task,
			AcceptanceCriteria: v.Checkpoint.AcceptanceCriteria, RelevantFiles: v.Checkpoint.RelevantFiles,
			FilesChanged: v.Checkpoint.FilesChanged, ArchitecturalFacts: v.Checkpoint.ArchitecturalFacts,
			Decisions: v.Checkpoint.Decisions, Tests: v.Checkpoint.Tests,
			LatestReviewFindings: v.Checkpoint.LatestReviewFindings, ActiveErrors: v.Checkpoint.ActiveErrors,
			CurrentFingerprint: v.Checkpoint.CurrentFingerprint, NextAction: v.Checkpoint.NextAction,
		},
		Decisions: DecisionsUsageResponse{
			QuestionsAsked: v.Decisions.QuestionsAsked, PolicyResolved: v.Decisions.PolicyResolved,
			TechnicalResolved: v.Decisions.TechnicalResolved, HumanRequired: v.Decisions.HumanRequired,
			ResolverFailed: v.Decisions.ResolverFailed, WaitingForCapacity: v.Decisions.WaitingForCapacity,
			ResolverProvider: v.Decisions.ResolverProvider, ResolverDurationMS: v.Decisions.ResolverDurationMS,
			ReusedDecision: v.Decisions.ReusedDecision,
		},
		Routing:          routingUsageResponses(v.Routing),
		SessionLifecycle: sessionLifecycleUsageResponse(v.SessionLifecycle),
	}
}

func sessionLifecycleUsageResponse(v SessionLifecycleUsageView) SessionLifecycleUsageResponse {
	out := SessionLifecycleUsageResponse{
		SessionsCreated: v.SessionsCreated, SessionsReused: v.SessionsReused,
		SessionsCompacted: v.SessionsCompacted, ContextPacksCreated: v.ContextPacksCreated,
		SessionSwitches: v.SessionSwitches,
	}
	for _, e := range v.Decisions {
		d := e.Decision
		reasons := make([]string, len(d.Reasons))
		for i, r := range d.Reasons {
			reasons[i] = string(r)
		}
		var createdAt string
		if !e.CreatedAt.IsZero() {
			createdAt = e.CreatedAt.Format(rfc3339Milli)
		}
		out.Decisions = append(out.Decisions, SessionLifecycleDecisionResponse{
			Action: string(d.Action), Reasons: reasons, PolicyVersion: d.PolicyVersion,
			Role: string(d.Role), FromSessionID: d.FromSessionID, ToSessionID: d.ToSessionID,
			ContextPackHash: d.ContextPackHash, CreatedAt: createdAt,
		})
	}
	return out
}

func routingUsageResponses(rs []RoutingUsageView) []RoutingUsageResponse {
	if len(rs) == 0 {
		return nil
	}
	out := make([]RoutingUsageResponse, 0, len(rs))
	for _, r := range rs {
		d := r.RoutingDecision
		reasons := make([]string, len(d.ReasonCodes))
		for i, rc := range d.ReasonCodes {
			reasons[i] = string(rc)
		}
		fallback := make([]string, len(d.FallbackOrder))
		for i, h := range d.FallbackOrder {
			fallback[i] = string(h)
		}
		var capacity map[string]string
		if len(r.CapacityStateAtDecision) > 0 {
			capacity = make(map[string]string, len(r.CapacityStateAtDecision))
			for h, s := range r.CapacityStateAtDecision {
				capacity[string(h)] = string(s)
			}
		}
		out = append(out, RoutingUsageResponse{
			Role: string(r.Role), StepKind: string(r.StepKind),
			PreferredHarness: string(d.PreferredHarness), SelectedHarness: string(d.SelectedHarness),
			FallbackOrder: fallback, FallbackUsed: r.FallbackUsed,
			ReasonCodes: reasons, PolicyVersion: d.PolicyVersion, Waiting: d.Waiting,
			CapacityStateAtDecision: capacity,
		})
	}
	return out
}

const rfc3339Milli = "2006-01-02T15:04:05.000Z07:00"
