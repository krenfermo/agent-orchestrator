package controllers

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
)

// UsageExplorationService is the controller-facing 3C read contract.
// *usage.ExplorationReader satisfies it.
type UsageExplorationService interface {
	WorkflowRun(ctx context.Context, runID string) (domain.RunExploration, error)
}

// ExplorationMetricResponse is one count and how AO came to know it.
type ExplorationMetricResponse struct {
	// Value is null exactly when basis is "unavailable" -- never 0 for "unknown".
	Value  *int64 `json:"value"`
	Basis  string `json:"basis" enum:"observed,derived,unavailable"`
	Method string `json:"method"`
	// LowerBound is true when value is a floor: activity AO saw but could not
	// attribute may add to it. Never compare a lower bound as exact.
	LowerBound bool `json:"lowerBound"`
}

// ExplorationCoverageResponse says whether the 3C extractor parsed every
// transcript of an agent from its first byte. When complete is false, every
// tool figure is unavailable and reason says why.
type ExplorationCoverageResponse struct {
	Complete          bool    `json:"complete"`
	Reason            string  `json:"reason"`
	ExtractorVersions []int64 `json:"extractorVersions"`
}

// ExplorationTurnCountResponse is one turn class and its call count.
type ExplorationTurnCountResponse struct {
	Class string `json:"class"`
	Count int64  `json:"count"`
}

// ExplorationTurnMixResponse counts an agent's provider calls per turn class.
type ExplorationTurnMixResponse struct {
	Basis  string                         `json:"basis" enum:"observed,derived,unavailable"`
	Method string                         `json:"method"`
	Counts []ExplorationTurnCountResponse `json:"counts"`
}

// RunContextSourcesResponse is the context-decorator state frozen into the
// run at creation. recorded=false: the run predates 3C (not "off").
type RunContextSourcesResponse struct {
	Recorded      bool   `json:"recorded"`
	MemoryMode    string `json:"memoryMode"`
	ContextRouter string `json:"contextRouter"`
}

// RunMemoryPackResponse is what one dispatch of the run was handed by project
// memory (identities and sizes only).
type RunMemoryPackResponse struct {
	Role            string `json:"role"`
	TaskRef         string `json:"taskRef"`
	PackDigest      string `json:"packDigest"`
	PolicyVersion   int    `json:"policyVersion"`
	Generation      int64  `json:"generation"`
	IndexedCommit   string `json:"indexedCommit"`
	ItemCount       int    `json:"itemCount"`
	SelectedBytes   int    `json:"selectedBytes"`
	EstimatedTokens int    `json:"estimatedTokens"`
	CreatedAt       string `json:"createdAt"`
}

// ExplorationRatioResponse is one share in [0,1] and how AO came to know it.
type ExplorationRatioResponse struct {
	Value  *float64 `json:"value"`
	Basis  string   `json:"basis" enum:"observed,derived,unavailable"`
	Method string   `json:"method"`
}

// ExplorationFileCountResponse is one project-relative path and its read count.
type ExplorationFileCountResponse struct {
	Path  string `json:"path"`
	Reads int64  `json:"reads"`
}

// ExplorationPathScopeCountResponse counts path-naming observations by scope.
// Only "project" ever named a path; the others are counts only.
type ExplorationPathScopeCountResponse struct {
	Scope string `json:"scope"`
	Count int64  `json:"count"`
}

// AgentExplorationResponse is one agent (usage subject x role x cycle).
type AgentExplorationResponse struct {
	Role        string   `json:"role"`
	Cycle       int64    `json:"cycle"`
	SubjectKind string   `json:"subjectKind"`
	SubjectID   string   `json:"subjectId"`
	Harness     string   `json:"harness"`
	Models      []string `json:"models"`

	ModelCalls  ExplorationMetricResponse `json:"modelCalls"`
	InputTokens ExplorationMetricResponse `json:"inputTokens"`
	// UncachedInputTokens is M1u: neither a cache read nor a cache write.
	UncachedInputTokens  ExplorationMetricResponse `json:"uncachedInputTokens"`
	OutputTokens         ExplorationMetricResponse `json:"outputTokens"`
	CachedInputTokens    ExplorationMetricResponse `json:"cachedInputTokens"`
	CacheWriteTokens     ExplorationMetricResponse `json:"cacheWriteTokens"`
	FirstCallInputTokens ExplorationMetricResponse `json:"firstCallInputTokens"`
	// HarnessTokensFirstCall is an ESTIMATE (basis derived): first-call input
	// minus AO's prompt at ~4 bytes/token.
	HarnessTokensFirstCall ExplorationMetricResponse `json:"harnessTokensFirstCall"`

	ToolCalls       ExplorationMetricResponse `json:"toolCalls"`
	FileReads       ExplorationMetricResponse `json:"fileReads"`
	UniqueFilesRead ExplorationMetricResponse `json:"uniqueFilesRead"`
	RepeatedReads   ExplorationMetricResponse `json:"repeatedReads"`
	Searches        ExplorationMetricResponse `json:"searches"`
	Listings        ExplorationMetricResponse `json:"listings"`
	Commands        ExplorationMetricResponse `json:"commands"`
	ExploreCommands ExplorationMetricResponse `json:"exploreCommands"`
	ExplorationOps  ExplorationMetricResponse `json:"explorationOps"`
	// ExplorationOpsAll is the 3D exploration figure (M3), one method per
	// harness; method names it.
	ExplorationOpsAll    ExplorationMetricResponse `json:"explorationOpsAll"`
	UnattributedCommands ExplorationMetricResponse `json:"unattributedCommands"`
	Edits                ExplorationMetricResponse `json:"edits"`
	ShellEdits           ExplorationMetricResponse `json:"shellEdits"`
	UniqueFilesEdited    ExplorationMetricResponse `json:"uniqueFilesEdited"`
	OpsBeforeFirstEdit   ExplorationMetricResponse `json:"opsBeforeFirstEdit"`
	CallsBeforeFirstEdit ExplorationMetricResponse `json:"callsBeforeFirstEdit"`

	RepoBytesObserved      ExplorationMetricResponse `json:"repoBytesObserved"`
	ExplorationResultBytes ExplorationMetricResponse `json:"explorationResultBytes"`
	AOContextBytes         ExplorationMetricResponse `json:"aoContextBytes"`
	HarnessContextBytes    ExplorationMetricResponse `json:"harnessContextBytes"`
	UnobservedResults      ExplorationMetricResponse `json:"unobservedResults"`

	ExplorationRatio    ExplorationRatioResponse `json:"explorationRatio"`
	AOContextRatio      ExplorationRatioResponse `json:"aoContextRatio"`
	HarnessContextRatio ExplorationRatioResponse `json:"harnessContextRatio"`

	ActiveSpanMs ExplorationMetricResponse `json:"activeSpanMs"`

	PathScopes             []ExplorationPathScopeCountResponse `json:"pathScopes"`
	TopFiles               []ExplorationFileCountResponse      `json:"topFiles"`
	ApproximateAttribution int64                               `json:"approximateAttribution"`
	Sources                int64                               `json:"sources"`
	ToolCoverage           ExplorationCoverageResponse         `json:"toolCoverage"`
	TurnMix                ExplorationTurnMixResponse          `json:"turnMix"`
}

// RunQualitySignalsResponse are the run's outcome facts, for the 3D quality
// gate.
type RunQualitySignalsResponse struct {
	FinalState         string                    `json:"finalState"`
	Completed          bool                      `json:"completed"`
	DurationMs         ExplorationMetricResponse `json:"durationMs"`
	Attempts           ExplorationMetricResponse `json:"attempts"`
	FailedAttempts     ExplorationMetricResponse `json:"failedAttempts"`
	Retries            ExplorationMetricResponse `json:"retries"`
	ProviderFailovers  ExplorationMetricResponse `json:"providerFailovers"`
	VerifyRuns         ExplorationMetricResponse `json:"verifyRuns"`
	VerifyPassed       *bool                     `json:"verifyPassed"`
	ChecksPassed       ExplorationMetricResponse `json:"checksPassed"`
	ChecksFailed       ExplorationMetricResponse `json:"checksFailed"`
	ReviewRuns         ExplorationMetricResponse `json:"reviewRuns"`
	FinalReviewVerdict string                    `json:"finalReviewVerdict"`
	FixCycles          ExplorationMetricResponse `json:"fixCycles"`
}

// WorkflowExplorationResponse is GET /workflows/{workflowId}/exploration.
type WorkflowExplorationResponse struct {
	RunID     string `json:"runId"`
	ProjectID string `json:"projectId"`
	// Recorded is false when AO holds no usage event and no tool observation
	// for the run: "nothing recorded", never "zero exploration".
	Recorded       bool                       `json:"recorded"`
	Agents         []AgentExplorationResponse `json:"agents"`
	Totals         AgentExplorationResponse   `json:"totals"`
	Quality        RunQualitySignalsResponse  `json:"quality"`
	ContextSources RunContextSourcesResponse  `json:"contextSources"`
	MemoryPacks    []RunMemoryPackResponse    `json:"memoryPacks"`
}

// getWorkflowExploration serves GET /workflows/{workflowId}/exploration. A
// strict read: unlike getWorkflowUsage it never calls the workflow service's
// GetRun, which observes and advances steps.
func (c *WorkflowsController) getWorkflowExploration(w http.ResponseWriter, r *http.Request) {
	if c.UsageExploration == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/workflows/{workflowId}/exploration")
		return
	}
	workflowID := chi.URLParam(r, "workflowId")
	if c.scopingEnforced() {
		user, err := identity.Require(r)
		if err != nil {
			envelope.WriteError(w, r, err)
			return
		}
		if !c.runVisible(r.Context(), workflowID, user.ID) {
			envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "WORKFLOW_NOT_FOUND", "workflow run not found", nil)
			return
		}
	}
	view, err := c.UsageExploration.WorkflowRun(r.Context(), workflowID)
	if errors.Is(err, usagesvc.ErrExplorationRunNotFound) {
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "WORKFLOW_NOT_FOUND", "workflow run not found", nil)
		return
	}
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, workflowExplorationResponse(view))
}

func workflowExplorationResponse(v domain.RunExploration) WorkflowExplorationResponse {
	out := WorkflowExplorationResponse{
		RunID:     v.RunID,
		ProjectID: v.ProjectID,
		Recorded:  v.Recorded,
		Agents:    make([]AgentExplorationResponse, 0, len(v.Agents)),
		Totals:    agentExplorationResponse(v.Totals),
		Quality:   runQualityResponse(v.Quality),
		ContextSources: RunContextSourcesResponse{
			Recorded:      v.ContextSources.Recorded(),
			MemoryMode:    v.ContextSources.MemoryMode,
			ContextRouter: v.ContextSources.ContextRouter,
		},
		MemoryPacks: make([]RunMemoryPackResponse, 0, len(v.MemoryPacks)),
	}
	for _, a := range v.Agents {
		out.Agents = append(out.Agents, agentExplorationResponse(a))
	}
	for _, p := range v.MemoryPacks {
		out.MemoryPacks = append(out.MemoryPacks, RunMemoryPackResponse{
			Role:            p.Role,
			TaskRef:         p.TaskRef,
			PackDigest:      p.PackDigest,
			PolicyVersion:   p.PolicyVersion,
			Generation:      p.Generation,
			IndexedCommit:   p.IndexedCommit,
			ItemCount:       p.ItemCount,
			SelectedBytes:   p.SelectedBytes,
			EstimatedTokens: p.EstimatedTokens,
			CreatedAt:       p.CreatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	return out
}

func explorationMetric(m domain.ExplorationMetric) ExplorationMetricResponse {
	basis := m.Basis
	if basis == "" {
		basis = domain.ExplorationUnavailable
	}
	out := ExplorationMetricResponse{Basis: string(basis), Method: m.Method}
	if basis != domain.ExplorationUnavailable && m.Value != nil {
		v := *m.Value
		out.Value = &v
		out.LowerBound = m.LowerBound
	}
	return out
}

func explorationRatio(m domain.ExplorationRatio) ExplorationRatioResponse {
	basis := m.Basis
	if basis == "" {
		basis = domain.ExplorationUnavailable
	}
	out := ExplorationRatioResponse{Basis: string(basis), Method: m.Method}
	if basis != domain.ExplorationUnavailable && m.Value != nil {
		v := *m.Value
		out.Value = &v
	}
	return out
}

func agentExplorationResponse(a domain.AgentExploration) AgentExplorationResponse {
	out := AgentExplorationResponse{
		Role:                   string(a.Role),
		Cycle:                  a.Cycle,
		SubjectKind:            string(a.Subject.Kind),
		SubjectID:              a.Subject.ID,
		Harness:                a.Harness,
		Models:                 append([]string{}, a.Models...),
		ModelCalls:             explorationMetric(a.ModelCalls),
		InputTokens:            explorationMetric(a.InputTokens),
		UncachedInputTokens:    explorationMetric(a.UncachedInputTokens),
		OutputTokens:           explorationMetric(a.OutputTokens),
		CachedInputTokens:      explorationMetric(a.CachedInputTokens),
		CacheWriteTokens:       explorationMetric(a.CacheWriteTokens),
		FirstCallInputTokens:   explorationMetric(a.FirstCallInput),
		HarnessTokensFirstCall: explorationMetric(a.HarnessTokensFirstCall),
		ShellEdits:             explorationMetric(a.ShellEdits),
		UnattributedCommands:   explorationMetric(a.UnattributedCommands),
		ToolCalls:              explorationMetric(a.ToolCalls),
		FileReads:              explorationMetric(a.FileReads),
		UniqueFilesRead:        explorationMetric(a.UniqueFilesRead),
		RepeatedReads:          explorationMetric(a.RepeatedReads),
		Searches:               explorationMetric(a.Searches),
		Listings:               explorationMetric(a.Listings),
		Commands:               explorationMetric(a.Commands),
		ExploreCommands:        explorationMetric(a.ExploreCommands),
		ExplorationOps:         explorationMetric(a.ExplorationOps),
		ExplorationOpsAll:      explorationMetric(a.ExplorationOpsAll),
		Edits:                  explorationMetric(a.Edits),
		UniqueFilesEdited:      explorationMetric(a.UniqueFilesEdited),
		OpsBeforeFirstEdit:     explorationMetric(a.OpsBeforeFirstEdit),
		CallsBeforeFirstEdit:   explorationMetric(a.CallsBeforeFirstEdit),
		RepoBytesObserved:      explorationMetric(a.RepoBytesObserved),
		ExplorationResultBytes: explorationMetric(a.ExplorationResultBytes),
		AOContextBytes:         explorationMetric(a.AOContextBytes),
		HarnessContextBytes:    explorationMetric(a.HarnessContextBytes),
		UnobservedResults:      explorationMetric(a.UnobservedResults),
		ExplorationRatio:       explorationRatio(a.ExplorationRatio),
		AOContextRatio:         explorationRatio(a.AOContextRatio),
		HarnessContextRatio:    explorationRatio(a.HarnessContextRatio),
		ActiveSpanMs:           explorationMetric(a.ActiveSpanMS),
		PathScopes:             []ExplorationPathScopeCountResponse{},
		TopFiles:               []ExplorationFileCountResponse{},
		ApproximateAttribution: a.ApproximateAttribution,
		Sources:                a.Sources,
		ToolCoverage: ExplorationCoverageResponse{
			Complete:          a.ToolCoverage.Complete,
			Reason:            a.ToolCoverage.Reason,
			ExtractorVersions: append([]int64{}, a.ToolCoverage.ExtractorVersions...),
		},
		TurnMix: explorationTurnMix(a.TurnMix),
	}
	for scope, n := range a.PathScopes {
		out.PathScopes = append(out.PathScopes, ExplorationPathScopeCountResponse{Scope: string(scope), Count: n})
	}
	sort.Slice(out.PathScopes, func(i, j int) bool { return out.PathScopes[i].Scope < out.PathScopes[j].Scope })
	for _, f := range a.TopFiles {
		out.TopFiles = append(out.TopFiles, ExplorationFileCountResponse{Path: f.Path, Reads: f.Reads})
	}
	return out
}

func explorationTurnMix(m domain.ExplorationTurnMix) ExplorationTurnMixResponse {
	basis := m.Basis
	if basis == "" {
		basis = domain.ExplorationUnavailable
	}
	out := ExplorationTurnMixResponse{Basis: string(basis), Method: m.Method, Counts: []ExplorationTurnCountResponse{}}
	if basis == domain.ExplorationUnavailable {
		return out
	}
	for class, n := range m.Counts {
		name := string(class)
		if name == "" {
			name = "unclassified"
		}
		out.Counts = append(out.Counts, ExplorationTurnCountResponse{Class: name, Count: n})
	}
	sort.Slice(out.Counts, func(i, j int) bool { return out.Counts[i].Class < out.Counts[j].Class })
	return out
}

func runQualityResponse(q domain.RunQualitySignals) RunQualitySignalsResponse {
	return RunQualitySignalsResponse{
		FinalState:         string(q.FinalState),
		Completed:          q.Completed,
		DurationMs:         explorationMetric(q.DurationMS),
		Attempts:           explorationMetric(q.Attempts),
		FailedAttempts:     explorationMetric(q.FailedAttempts),
		Retries:            explorationMetric(q.Retries),
		ProviderFailovers:  explorationMetric(q.ProviderFailovers),
		VerifyRuns:         explorationMetric(q.VerifyRuns),
		VerifyPassed:       q.VerifyPassed,
		ChecksPassed:       explorationMetric(q.ChecksPassed),
		ChecksFailed:       explorationMetric(q.ChecksFailed),
		ReviewRuns:         explorationMetric(q.ReviewRuns),
		FinalReviewVerdict: string(q.FinalReviewVerdict),
		FixCycles:          explorationMetric(q.FixCycles),
	}
}
