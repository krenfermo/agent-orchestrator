package controllers

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/runtimegc"
)

// scheduler.go — P1-C's observability surface (§O/§P).
//
// Two questions, two routes: "why is my work not running" and "what did GC do
// about the runtimes AO left behind". Both are read-only apart from the
// explicit, operator-invoked sweep, and neither exposes a runtime token, a
// prompt, a credential or a provider response. What it does expose — claim
// ids, execution kinds, run/step ids, generations, tmux session names — is
// AO's own local vocabulary, and it is exactly what makes a held slot
// correlatable with something an operator can see.

// SchedulerService is the capacity surface the controller depends on.
type SchedulerService interface {
	SchedulerSnapshot(ctx context.Context) (domain.SchedulerSnapshot, error)
}

// RuntimeGCService performs a sweep.
type RuntimeGCService interface {
	Sweep(ctx context.Context, opts runtimegc.Options) (runtimegc.Report, error)
}

// CapacityUsageView is one execution kind's meter, or the global one.
type CapacityUsageView struct {
	// Kind is empty for the global row.
	Kind   string `json:"kind,omitempty" enum:"planner,worker,reviewer,repair"`
	Limit  int    `json:"limit"`
	Held   int    `json:"held"`
	Queued int    `json:"queued"`
}

// CapacityClaimView is one durable claim.
type CapacityClaimView struct {
	ID   string `json:"id"`
	Kind string `json:"kind" enum:"planner,worker,reviewer,repair"`
	// State is queued or held; released claims are history and are not listed.
	State          string `json:"state" enum:"queued,held,released"`
	WorkflowRunID  string `json:"workflowRunId"`
	WorkflowStepID string `json:"workflowStepId,omitempty"`
	TaskID         string `json:"taskId,omitempty"`
	// LifecycleGeneration is the dispatch generation the claim is fenced to.
	LifecycleGeneration int64 `json:"lifecycleGeneration"`
	// Priority and EnqueuedAt are the scheduling order, so a queued run's
	// position is explainable rather than mysterious.
	Priority   int64      `json:"priority"`
	EnqueuedAt time.Time  `json:"enqueuedAt"`
	HeldAt     *time.Time `json:"heldAt,omitempty"`
	ProjectID  string     `json:"projectId,omitempty"`
	// RuntimeHandle is AO's own local name for the runtime the claim paid for.
	RuntimeHandle string `json:"runtimeHandle,omitempty"`
}

// SchedulerStatusResponse is the body of GET /api/v1/capacity.
type SchedulerStatusResponse struct {
	// Global is the machine-wide meter; PerKind breaks it down.
	Global  CapacityUsageView   `json:"global"`
	PerKind []CapacityUsageView `json:"perKind"`
	// PerWorkflowLimit is the fairness bound: how many slots one workflow may
	// hold at once.
	PerWorkflowLimit int `json:"perWorkflowLimit"`
	// Held is what currently occupies the machine; Queued is the front of the
	// queue in scheduling order.
	Held       []CapacityClaimView `json:"held"`
	Queued     []CapacityClaimView `json:"queued"`
	ObservedAt time.Time           `json:"observedAt"`
}

// RuntimeGCFindingView is one runtime the sweep looked at.
type RuntimeGCFindingView struct {
	Handle     string `json:"handle,omitempty"`
	InstanceID string `json:"instanceId,omitempty"`
	// Class is why it was a candidate; Disposition is what happened; Reason
	// always says why it was or was not destroyed.
	Class         string `json:"class,omitempty"`
	Disposition   string `json:"disposition" enum:"cleaned,candidate,live,unprovable,foreign,absent,error"`
	Reason        string `json:"reason,omitempty"`
	WorkflowRunID string `json:"workflowRunId,omitempty"`
	Error         string `json:"error,omitempty"`
	// OwnershipProven and RecommendedAction are the operator half of a
	// finding: whether AO could prove the runtime is its own, and what a
	// person should do about it. They matter most for the ones AO will never
	// touch — a legacy session AO cannot prove is permanent until somebody
	// acts, and a report that does not say so is how it stays that way.
	OwnershipProven   bool   `json:"ownershipProven"`
	RecommendedAction string `json:"recommendedAction,omitempty"`
}

// RuntimeGCReportResponse is the body of POST /api/v1/runtime/gc.
type RuntimeGCReportResponse struct {
	DryRun     bool      `json:"dryRun"`
	Trigger    string    `json:"trigger,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	// Counters are the summary: what was found, cleaned, and why the rest was
	// left alone.
	Candidates        int `json:"candidates"`
	Cleaned           int `json:"cleaned"`
	SkippedLive       int `json:"skippedLive"`
	SkippedUnprovable int `json:"skippedUnprovable"`
	SkippedForeign    int `json:"skippedForeign"`
	Absent            int `json:"absent"`
	Errors            int `json:"errors"`

	Findings []RuntimeGCFindingView `json:"findings"`
}

// RuntimeGCRequest is the body of POST /api/v1/runtime/gc.
type RuntimeGCRequest struct {
	// DryRun classifies everything and destroys nothing. It runs the identical
	// predicates a real sweep does, so it is a true preview.
	DryRun bool `json:"dryRun,omitempty"`
}

// SchedulerController owns the capacity and runtime-GC routes.
type SchedulerController struct {
	Scheduler SchedulerService
	GC        RuntimeGCService
}

// Register mounts the routes.
func (c *SchedulerController) Register(r chi.Router) {
	// Deliberately under /runtime, not /capacity: /api/v1/capacity is already
	// Checkpoint 8J's PROVIDER capacity view (rate limits and quota), and the
	// two answer different questions. Sharing a path would have made "your
	// Claude subscription is throttled" and "this machine is full" the same
	// endpoint.
	r.Get("/runtime/capacity", c.status)
	r.Post("/runtime/gc", c.gc)
}

func capacityClaimViews(claims []domain.CapacityClaim) []CapacityClaimView {
	out := make([]CapacityClaimView, 0, len(claims))
	for _, claim := range claims {
		out = append(out, CapacityClaimView{
			ID: claim.ID, Kind: string(claim.Kind), State: string(claim.State),
			WorkflowRunID: claim.WorkflowRunID, WorkflowStepID: claim.WorkflowStepID,
			TaskID: claim.TaskID, LifecycleGeneration: claim.LifecycleGeneration,
			Priority: claim.Priority, EnqueuedAt: claim.EnqueuedAt, HeldAt: claim.HeldAt,
			ProjectID: claim.ProjectID, RuntimeHandle: claim.RuntimeHandle,
		})
	}
	return out
}

func (c *SchedulerController) status(w http.ResponseWriter, r *http.Request) {
	if c.Scheduler == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/runtime/capacity")
		return
	}
	snap, err := c.Scheduler.SchedulerSnapshot(r.Context())
	if err != nil {
		envelope.WriteAPIError(w, r, http.StatusInternalServerError, "internal", "CAPACITY_UNAVAILABLE", err.Error(), nil)
		return
	}
	out := SchedulerStatusResponse{
		Global: CapacityUsageView{
			Limit: snap.Global.Limit, Held: snap.Global.Held, Queued: snap.Global.Queued,
		},
		PerWorkflowLimit: snap.Limits.Normalize().PerWorkflow,
		Held:             capacityClaimViews(snap.HeldClaims),
		Queued:           capacityClaimViews(snap.QueuedFirst),
		ObservedAt:       snap.ObservedAt,
	}
	for _, usage := range snap.PerKind {
		out.PerKind = append(out.PerKind, CapacityUsageView{
			Kind: string(usage.Kind), Limit: usage.Limit, Held: usage.Held, Queued: usage.Queued,
		})
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *SchedulerController) gc(w http.ResponseWriter, r *http.Request) {
	if c.GC == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/runtime/gc")
		return
	}
	var in RuntimeGCRequest
	// An absent or unparseable body is a dry run, not a destructive sweep.
	// Defaulting the other way would make a malformed request delete things.
	if err := decodeJSON(r, &in); err != nil {
		in = RuntimeGCRequest{DryRun: true}
	}
	report, err := c.GC.Sweep(r.Context(), runtimegc.Options{DryRun: in.DryRun, Trigger: "operator"})
	if err != nil {
		envelope.WriteAPIError(w, r, http.StatusInternalServerError, "internal", "RUNTIME_GC_FAILED", err.Error(), nil)
		return
	}
	out := RuntimeGCReportResponse{
		DryRun: report.DryRun, Trigger: report.Trigger,
		StartedAt: report.StartedAt, FinishedAt: report.FinishedAt,
		Candidates: report.Candidates, Cleaned: report.Cleaned,
		SkippedLive: report.SkippedLive, SkippedUnprovable: report.SkippedUnprovable,
		SkippedForeign: report.SkippedForeign, Absent: report.Absent, Errors: report.Errors,
		Findings: make([]RuntimeGCFindingView, 0, len(report.Findings)),
	}
	for _, f := range report.Findings {
		out.Findings = append(out.Findings, RuntimeGCFindingView{
			Handle: f.Handle, InstanceID: f.InstanceID, Class: string(f.Class),
			Disposition: string(f.Disposition), Reason: f.Reason,
			WorkflowRunID: f.WorkflowRunID, Error: f.Err,
			OwnershipProven: f.OwnershipProven, RecommendedAction: f.RecommendedAction,
		})
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}
