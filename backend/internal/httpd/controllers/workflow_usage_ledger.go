package controllers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// UsageLedgerService is the controller-facing P3-E read contract.
// *usage.LedgerReader satisfies it exactly.
type UsageLedgerService interface {
	WorkflowRun(ctx context.Context, runID string, opts usagesvc.RunUsageOptions) (domain.WorkflowUsageLedger, error)
	Project(ctx context.Context, projectID string, period domain.UsagePeriod, now time.Time, budget domain.UsageBudgetPolicy) (domain.ProjectUsageSummary, error)
	CompactForProject(ctx context.Context, projectID string) (map[string]domain.CompactRunUsage, error)
}

// UsageContextService is the AO-ASSEMBLED context read contract, kept separate
// from UsageLedgerService because the two measure different things and must
// never be folded into one reader that could accidentally add them.
// *usage.ContextReader satisfies it.
type UsageContextService interface {
	WorkflowRun(ctx context.Context, runID string) (domain.ContextCompositionView, error)
}

// runUsageContext builds the run's assembled-context view. A failure costs the
// section and nothing else: a run whose evidence tree is unreadable must still
// report the tokens it spent.
func (c *WorkflowsController) runUsageContext(ctx context.Context, runID string) *WorkflowContextResponse {
	if c.UsageContext == nil {
		return nil
	}
	view, err := c.UsageContext.WorkflowRun(ctx, runID)
	if err != nil || !view.Recorded {
		return nil
	}
	out := workflowContextResponse(view)
	return &out
}

// runUsageLedger builds one run's canonical usage ledger, reading the budget
// from the run's own FROZEN policy snapshot rather than from live Settings —
// so what a run was allowed to spend cannot change under it, and a restart
// reaches the same verdict.
func (c *WorkflowsController) runUsageLedger(ctx context.Context, run domain.WorkflowRun) (domain.WorkflowUsageLedger, bool) {
	if c.UsageLedger == nil {
		return domain.WorkflowUsageLedger{}, false
	}
	// Decoded by exactly the rule the ENFORCING side uses
	// (workflow.policyForRun): a snapshot is honoured only when it carries a
	// fix budget, which is what proves it was written by CreateRun rather than
	// being a partial or hand-edited row. The two must agree, or the run page
	// would show a ceiling the dispatch gate does not apply -- a budget the
	// user believes in and AO ignores is worse than no budget at all.
	var policy domain.WorkflowPolicy
	if run.PolicySnapshot != "" && run.PolicySnapshot != "{}" {
		var decoded domain.WorkflowPolicy
		if err := json.Unmarshal([]byte(run.PolicySnapshot), &decoded); err == nil && decoded.MaxFixCycles > 0 {
			policy = decoded
		}
	}
	ledger, err := c.UsageLedger.WorkflowRun(ctx, run.ID, usagesvc.RunUsageOptions{
		Budget: policy.EffectiveUsageBudgetPolicy(),
		// A parent's ledger folds its children because that is the only total
		// its budget can honestly be measured against (P3-E §16).
		IncludeFamily: true,
	})
	if err != nil {
		return domain.WorkflowUsageLedger{}, false
	}
	return ledger, true
}

// getWorkflowUsage serves GET /workflows/{workflowId}/usage.
func (c *WorkflowsController) getWorkflowUsage(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil || c.UsageLedger == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/workflows/{workflowId}/usage")
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
	detail, err := c.Svc.GetRun(r.Context(), workflowID)
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	ledger, ok := c.runUsageLedger(r.Context(), detail.Run)
	if !ok {
		apispec.NotImplemented(w, r, "GET", "/api/v1/workflows/{workflowId}/usage")
		return
	}
	response := workflowUsageLedgerResponse(ledger)
	response.Context = c.runUsageContext(r.Context(), workflowID)
	response.Dynamics = c.runUsageDynamics(r.Context(), detail)
	envelope.WriteJSON(w, http.StatusOK, response)
}

// getProjectUsage serves GET /projects/{projectId}/usage?range=today|7d|30d|all.
func (c *WorkflowsController) getProjectUsage(w http.ResponseWriter, r *http.Request) {
	if c.UsageLedger == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{projectId}/usage")
		return
	}
	projectID := chi.URLParam(r, "projectId")
	period := domain.UsagePeriod(r.URL.Query().Get("range"))
	if period == "" {
		period = domain.UsagePeriodWeek
	}
	if !period.Valid() {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "invalid", "INVALID_RANGE",
			"range must be one of today, 7d, 30d, all", nil)
		return
	}
	summary, err := c.UsageLedger.Project(r.Context(), projectID, period, time.Now().UTC(), domain.UsageBudgetPolicy{})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, projectUsageResponse(summary))
}

// UsageDynamicsService is the controller-facing read contract for a run's
// context SHAPE. *usage.DynamicsReader satisfies it.
//
// A third service rather than a widened ledger, for the same reason
// UsageContextService is a second one: what a provider reported spending, what
// AO assembled, and how the conversation moved are three different quantities,
// and one reader that could return all three is one refactor away from adding
// two of them together.
type UsageDynamicsService interface {
	WorkflowRun(ctx context.Context, runID string, opts usagesvc.DynamicsOptions) (domain.RunContextDynamics, error)
}

// runUsageDynamics builds the run's context-shape block.
//
// A failure costs the block and nothing else: a run whose trajectory cannot be
// read must still report the tokens it spent. The progress evidence is taken
// from the detail the caller already has -- no extra store read -- and is
// passed as evidence rather than as a verdict, so an unknown term leaves the
// growth-without-progress conjunction unfired instead of firing it.
func (c *WorkflowsController) runUsageDynamics(ctx context.Context, detail workflowsvc.RunDetail) *WorkflowUsageDynamicsResponse {
	if c.UsageDynamics == nil {
		return nil
	}
	now := time.Now().UTC()
	opts := usagesvc.DynamicsOptions{
		Strategy:     runExecutionStrategy(detail.Run),
		Budget:       runFrozenUsagePolicy(detail.Run),
		RunStartedAt: detail.Run.CreatedAt,
		RunEndedAt:   detail.Run.CompletedAt,
		Now:          now,
		Progress:     progressEvidenceFor(detail),
	}
	view, err := c.UsageDynamics.WorkflowRun(ctx, detail.Run.ID, opts)
	if err != nil {
		return nil
	}
	out := workflowUsageDynamicsResponse(view)
	return &out
}

// runFrozenUsagePolicy decodes the run's frozen budget by exactly the rule the
// ENFORCING side uses (workflow.policyForRun): a snapshot counts only when it
// carries a fix budget, which is what proves CreateRun wrote it. The advisory
// thresholds must come from the same snapshot the ceilings do, or a run would
// warn against numbers its own policy never recorded.
func runFrozenUsagePolicy(run domain.WorkflowRun) domain.UsageBudgetPolicy {
	var policy domain.WorkflowPolicy
	if run.PolicySnapshot != "" && run.PolicySnapshot != "{}" {
		var decoded domain.WorkflowPolicy
		if err := json.Unmarshal([]byte(run.PolicySnapshot), &decoded); err == nil && decoded.MaxFixCycles > 0 {
			policy = decoded
		}
	}
	return policy.EffectiveUsageBudgetPolicy()
}

// runExecutionStrategy reads the run's frozen strategy, or empty when the
// snapshot records none. Empty is deliberately NOT mapped to "task" here:
// UsageBudgetProfileFor answers an unknown strategy with the middle profile,
// because measuring a run of unknown shape against the tightest expectations
// would warn constantly and teach a person to ignore the warnings.
func runExecutionStrategy(run domain.WorkflowRun) domain.ExecutionStrategy {
	if run.PolicySnapshot == "" || run.PolicySnapshot == "{}" {
		return ""
	}
	var decoded domain.WorkflowPolicy
	if err := json.Unmarshal([]byte(run.PolicySnapshot), &decoded); err != nil {
		return ""
	}
	if !decoded.Strategy.Effective.Valid() {
		return ""
	}
	return decoded.Strategy.Effective
}

// progressEvidenceFor assembles what this read already knows about whether the
// run is getting anywhere.
//
// Known is true only when there IS a running agent to judge. A run between
// steps has no dispatch to measure progress from, and calling that "no
// progress" would fire the advisory on every gap between a worker finishing
// and a review starting.
func progressEvidenceFor(detail workflowsvc.RunDetail) usagesvc.ProgressEvidence {
	live := detail.WorkerLiveness
	if !live.Observed {
		return usagesvc.ProgressEvidence{}
	}
	out := usagesvc.ProgressEvidence{
		Known:              true,
		WorkerLastSignalAt: live.LastSignalAt,
	}
	for _, sd := range detail.Steps {
		if sd.Step.ID != live.StepID {
			continue
		}
		// The running step's own dispatch instant: its newest attempt's start,
		// falling back to when the step itself began.
		out.DispatchedAt = sd.Step.UpdatedAt
		for _, att := range sd.Attempts {
			if att.StartedAt.After(out.DispatchedAt) {
				out.DispatchedAt = att.StartedAt
			}
		}
		if sd.LatestCheckpoint != nil {
			out.LastDurableProgressAt = sd.LatestCheckpoint.CreatedAt
		}
		break
	}
	// The run's own newest durable act counts too: a checkpoint written at run
	// level (a placement, an adoption, an observation) is progress even when
	// the running step has not written one of its own.
	if detail.LatestCheckpointAt.After(out.LastDurableProgressAt) {
		out.LastDurableProgressAt = detail.LatestCheckpointAt
	}
	return out
}
