package controllers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// work_report.go — the transport for a worker's structured declaration
// (P5-A phase 2B).
//
// It is addressed by SESSION, on purpose and by precedent. `ao review submit`
// records a reviewer's verdict at /sessions/{sessionId}/reviews/submit under
// session-ownership scoping, and a worker's report is the same shape of act:
// an AO-launched agent writing a durable fact about the session it is running
// in. So it reuses the same middleware, the same permission (session write, the
// worker role's existing ceiling), and the same URL family — and adds no new
// permission, no new ownership model and no new table.
//
// Nothing about this route can make a review shallower. It records a
// declaration; the depth decision is taken from evidence AO gathered itself,
// and the declaration's only influence is to DEEPEN (see
// domain.EvaluateReviewEvidenceRelief).

// WorkReportsController serves the worker-report route.
type WorkReportsController struct {
	// Svc is the workflow service. The work-report capability is
	// type-asserted off it, so a build without one answers 501 rather than
	// failing to start.
	Svc any
	// Ownership, TrustedLocal and Guard are the same session-scoping trio
	// ReviewsController uses; see its comment for why they travel together.
	Ownership    SessionOwnershipStore
	TrustedLocal bool
	Guard        Guard
}

func (c *WorkReportsController) scoping() SessionScoping {
	return SessionScoping{Ownership: c.Ownership, TrustedLocal: c.TrustedLocal, Guard: c.Guard}
}

// Register mounts the work-report route.
func (c *WorkReportsController) Register(r chi.Router) {
	r.With(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if !AuthorizeSessionAccess(w, req, c.scoping(), sessionID(req)) {
				return
			}
			next.ServeHTTP(w, req)
		})
	}).Post("/sessions/{sessionId}/work-report", c.submit)
}

// SubmitWorkReportTestClaim is one command the worker says it ran, and what it
// says happened. The field is named claimedOutcome, never outcome: the wire
// format carries the same refusal the domain type does.
type SubmitWorkReportTestClaim struct {
	Command string `json:"command"`
	// ClaimedOutcome is the worker's CLAIM. AO never reads
	// "claimed_passed" as a pass; it verifies independently and only ever
	// acts on "claimed_failed", which can make a review deeper.
	ClaimedOutcome string `json:"claimedOutcome,omitempty" enum:"claimed_passed,claimed_failed,claimed_skipped"`
	Note           string `json:"note,omitempty"`
}

// SubmitWorkReportCriterion is the worker's claim about one acceptance
// criterion.
type SubmitWorkReportCriterion struct {
	Criterion string `json:"criterion"`
	Addressed bool   `json:"addressed"`
	Note      string `json:"note,omitempty"`
}

// SubmitWorkReportRequest is the body of POST
// /api/v1/sessions/{sessionId}/work-report.
//
// Every field is a DECLARATION by the agent under review. None of it is
// evidence, none of it is verified, and the type names say so.
type SubmitWorkReportRequest struct {
	Summary string `json:"summary,omitempty"`
	// ClaimedChangedPaths is the worker's own list. AO has its own, observed
	// from the worktree, and prefers it; this exists so a reviewer can see the
	// two disagree.
	ClaimedChangedPaths []string                    `json:"claimedChangedPaths,omitempty"`
	Criteria            []SubmitWorkReportCriterion `json:"criteria,omitempty"`
	TestsReported       []SubmitWorkReportTestClaim `json:"testsReported,omitempty"`
	Limitations         []string                    `json:"limitations,omitempty"`
	Risks               []string                    `json:"risks,omitempty"`
	FollowUp            []string                    `json:"followUp,omitempty"`
	Commit              string                      `json:"commit,omitempty"`
}

// WorkReportResponse is what an accepted report reports back: where it landed,
// whether it replaced an earlier one, and how AO bounded it.
type WorkReportResponse struct {
	WorkflowRunID  string `json:"workflowRunId"`
	WorkflowStepID string `json:"workflowStepId"`
	// Superseded is true when this run already had a report. Both remain on the
	// ledger; the newest is the one policy and the reviewer read.
	Superseded bool   `json:"superseded,omitempty"`
	Version    string `json:"version"`
	// Truncated names the fields AO shortened, so a bounded report says it is
	// bounded rather than looking complete.
	Truncated []string `json:"truncated,omitempty"`
}

func (c *WorkReportsController) submit(w http.ResponseWriter, r *http.Request) {
	svc, ok := c.Svc.(workflowsvc.WorkReportManager)
	if !ok || c.Svc == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/sessions/{sessionId}/work-report")
		return
	}
	session := strings.TrimSpace(chi.URLParam(r, "sessionId"))
	if session == "" {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "WORK_REPORT_SESSION_REQUIRED",
			"Session id is required", nil)
		return
	}
	var in SubmitWorkReportRequest
	if err := decodeJSON(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}

	receipt, err := svc.SubmitWorkReportForSession(r.Context(), session, workReportFromRequest(in))
	switch {
	case err == nil:
	case errors.Is(err, workflowcore.ErrWorkReportWindowClosed):
		// 409: the request was well formed and arrived too late. A conflict
		// rather than a 400, because nothing about it was wrong except when it
		// happened, and a worker retrying identically would fail identically.
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict", "WORK_REPORT_WINDOW_CLOSED",
			"This run has already decided how deeply to review the change, so a report can no longer inform it", nil)
		return
	case errors.Is(err, workflowcore.ErrNotFound):
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "WORK_REPORT_NO_ACTIVE_STEP",
			"No active workflow work step is running in this session", nil)
		return
	case errors.Is(err, workflowcore.ErrInvalid):
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "WORK_REPORT_INVALID", err.Error(), nil)
		return
	default:
		envelope.WriteAPIError(w, r, http.StatusInternalServerError, "internal", "WORK_REPORT_FAILED",
			"Could not record the work report", nil)
		return
	}

	envelope.WriteJSON(w, http.StatusOK, WorkReportResponse{
		WorkflowRunID:  receipt.WorkflowRunID,
		WorkflowStepID: receipt.WorkflowStepID,
		Superseded:     receipt.Superseded,
		Version:        receipt.Report.Version,
		Truncated:      receipt.Report.Truncated,
	})
}

// workReportFromRequest maps the wire shape onto the domain declaration.
//
// It does not validate the claims and it must not: a report is what the worker
// said, and correcting it would make AO the author of a statement it is about
// to hold the worker to. Bounding and normalization happen in
// domain.WorkReport.Normalize, which truncates rather than rejects — an
// unrecognised outcome becomes "unstated" there, never a pass.
func workReportFromRequest(in SubmitWorkReportRequest) domain.WorkReport {
	out := domain.WorkReport{
		Summary:             in.Summary,
		ClaimedChangedPaths: in.ClaimedChangedPaths,
		Limitations:         in.Limitations,
		Risks:               in.Risks,
		FollowUp:            in.FollowUp,
		Reference:           domain.WorkReportReference{Commit: strings.TrimSpace(in.Commit)},
	}
	for _, c := range in.Criteria {
		out.Criteria = append(out.Criteria, domain.WorkReportCriterion{
			Criterion: c.Criterion, Addressed: c.Addressed, Note: c.Note,
		})
	}
	for _, t := range in.TestsReported {
		out.TestsReported = append(out.TestsReported, domain.WorkReportTestClaim{
			Command:        t.Command,
			ClaimedOutcome: domain.WorkReportClaimedOutcome(strings.TrimSpace(strings.ToLower(t.ClaimedOutcome))),
			Note:           t.Note,
		})
	}
	return out
}
