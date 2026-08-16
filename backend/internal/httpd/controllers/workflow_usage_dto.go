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

// WorkflowUsageResponse is the full Checkpoint 8J usage section embedded in
// a workflow run detail response.
type WorkflowUsageResponse struct {
	Roles      []RoleUsageResponse            `json:"roles"`
	Metrics    TaskUsefulWorkMetricsResponse  `json:"metrics"`
	Advisory   SessionRefreshAdvisoryResponse `json:"advisory"`
	Checkpoint TaskCheckpointSummaryResponse  `json:"checkpoint"`
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
	}
}

const rfc3339Milli = "2006-01-02T15:04:05.000Z07:00"
