package controllers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// workflow_incident.go — Checkpoint 8P-E.18's HTTP surface for "¿Qué hago?".
//
// Four routes, and the split between them is the authorization model, not a
// REST aesthetic:
//
//	GET  /workflows/{id}/incident            what happened, and what AO proposes
//	POST /workflows/{id}/incident/diagnose   investigate it (isolated agent)
//	POST /workflows/{id}/incident/diagnosis  the agent's answer (submit-only)
//	POST /workflows/{id}/incident/execute    do the proposed thing (authorized)
//
// The view is deliberately complete. Everything the modal renders — the
// classification, the risk, whether an approval is required, what AO says the
// action will actually do — is computed here from the durable record, so the
// frontend never re-derives a decision that belongs to the backend. A UI that
// had to infer "does this need approval" from a class string would eventually
// infer it differently from the executor, and the executor is the one holding
// the permission.

// IncidentView is the whole modal payload.
type IncidentView struct {
	ID         string `json:"id"`
	RunID      string `json:"runId"`
	State      string `json:"state"`
	StopReason string `json:"stopReason"`
	StopDetail string `json:"stopDetail,omitempty"`
	// Stale means the run moved since this incident was diagnosed. The UI must
	// offer a fresh look rather than any action.
	Stale bool `json:"stale"`

	// Budgets, so the modal can say "1 of 2 investigations used" instead of
	// discovering the limit by being refused.
	Diagnoses    int `json:"diagnosesUsed"`
	MaxDiagnoses int `json:"maxDiagnoses"`
	Repairs      int `json:"repairsUsed"`
	MaxRepairs   int `json:"maxRepairs"`

	CanDiagnose bool `json:"canDiagnose"`
	CanExecute  bool `json:"canExecute"`
	// LaunchOutcome is what the request that produced this view actually did:
	// launched / already_running / waiting_for_capacity. It is empty for a plain
	// read. The modal needs it to tell "your agent is starting" apart from "one
	// was already running" apart from "every provider is busy, AO will retry" —
	// three situations that look identical from the incident's state alone.
	LaunchOutcome string `json:"launchOutcome,omitempty"`

	// Progress is the single derived value the modal renders. Everything the UI
	// shows about motion comes from here, so it never simulates progress with a
	// timer: a progress that advanced on a clock rather than on a fact would
	// eventually claim a repair was verified while it was still building.
	Progress string `json:"progress"`

	// Diagnosis is the investigation as an observable background job. It is
	// durable and independent of this modal: closing the UI does not affect it,
	// and reopening re-derives every field below from the ledger and the agent
	// session. See workflow/incident_diagnosis_job.go.
	DiagnosisJob IncidentDiagnosisJobView `json:"diagnosisJob"`
	// DiagnosticHarness / CapacityReasons / NextEvaluationAt explain the two
	// states a person would otherwise read as an unexplained spinner.
	DiagnosticHarness string   `json:"diagnosticHarness,omitempty"`
	CapacityReasons   []string `json:"capacityReasons,omitempty"`
	NextEvaluationAt  string   `json:"nextEvaluationAt,omitempty"`
	// ClosureCause / ClosureEvidence say why AO stopped asking, for an incident
	// whose condition ended without a repair.
	ClosureCause    string   `json:"closureCause,omitempty"`
	ClosureEvidence []string `json:"closureEvidence,omitempty"`

	// Repair is the linked repair run and its audit trail, present once a
	// class-B repair has been approved and dispatched. The modal needs it to say
	// "a repair is running and will be independently reviewed and verified"
	// rather than implying the incident is already fixed.
	Repair *IncidentRepairView `json:"repair,omitempty"`

	Diagnosis *IncidentDiagnosisView `json:"diagnosis,omitempty"`
	Pack      *IncidentPackView      `json:"contextPack,omitempty"`
}

// IncidentDiagnosisView is the agent's answer plus AO's own reading of it.
type IncidentDiagnosisView struct {
	Class        string   `json:"classification"`
	Summary      string   `json:"summary"`
	WhatHappened string   `json:"whatHappened,omitempty"`
	WhatIsStuck  string   `json:"whatIsStuck,omitempty"`
	WhyStopped   string   `json:"whyAOStopped,omitempty"`
	Evidence     []string `json:"evidence,omitempty"`
	Missing      []string `json:"missingEvidence,omitempty"`
	Risk         string   `json:"risk,omitempty"`

	Options []IncidentOptionView `json:"options,omitempty"`
	Action  *IncidentActionView  `json:"proposedAction,omitempty"`
}

// IncidentOptionView is one concrete choice for a human decision.
type IncidentOptionView struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Detail      string `json:"detail"`
	Consequence string `json:"consequence,omitempty"`
}

// IncidentActionView is the proposed action as the modal must present it.
//
// Rationale and Describe are two different things and are deliberately kept
// apart: Rationale is the agent's argument for the action, while Describe is
// AO's own statement of what will happen. A proposer does not get to narrate
// the mechanism it is asking to trigger.
type IncidentActionView struct {
	Kind          string `json:"kind"`
	Rationale     string `json:"rationale,omitempty"`
	Describe      string `json:"describe"`
	Risk          string `json:"risk"`
	NeedsApproval bool   `json:"needsApproval"`
	Destructive   bool   `json:"endsWork"`
	WritesCode    bool   `json:"writesCode"`
	Executable    bool   `json:"executable"`
	RefusalReason string `json:"refusalReason,omitempty"`
}

// IncidentPackView is the evidence accounting, never the evidence itself. The
// modal shows what AO looked at and what it cost, not 48 KB of diff.
type IncidentPackView struct {
	Digest          string   `json:"digest"`
	Bytes           int      `json:"bytes"`
	MaxBytes        int      `json:"maxBytes"`
	EstimatedTokens int      `json:"estimatedTokens"`
	Sections        []string `json:"sections"`
	Dropped         []string `json:"droppedSections,omitempty"`
}

// IncidentRepairView is the durable answer to "who authorised this, what is
// carrying it out, and how much repair budget is left".
type IncidentRepairView struct {
	RunID      string `json:"runId"`
	ApprovedBy string `json:"approvedBy,omitempty"`
	Generation int    `json:"generation,omitempty"`
	MaxRepairs int    `json:"maxRepairs"`
	// The outcome facts, once the repair run has produced them.
	ReviewerHarness string `json:"reviewerHarness,omitempty"`
	VerifyResult    string `json:"verifyResult,omitempty"`
	FinalSHA        string `json:"finalSha,omitempty"`
}

// IncidentResponse is the envelope every incident route returns.
type IncidentResponse struct {
	Incident IncidentView `json:"incident"`
}

// incidentAdvisor resolves the optional capability, so a Svc without it answers
// 501 rather than panicking.
func (c *WorkflowsController) incidentAdvisor() (workflowsvc.IncidentAdvisor, bool) {
	if c.Svc == nil {
		return nil, false
	}
	adv, ok := c.Svc.(workflowsvc.IncidentAdvisor)
	return adv, ok
}

func (c *WorkflowsController) getIncident(w http.ResponseWriter, r *http.Request) {
	adv, ok := c.incidentAdvisor()
	if !ok {
		apispec.NotImplemented(w, r, "GET", "/api/v1/workflows/{workflowId}/incident")
		return
	}
	runID := chi.URLParam(r, "workflowId")
	inc, pack, err := adv.IncidentPackFor(r.Context(), runID)
	if err != nil {
		writeIncidentError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, IncidentResponse{Incident: incidentView(inc, &pack, adv.DeriveIncidentStatus(r.Context(), inc))})
}

func (c *WorkflowsController) diagnoseIncident(w http.ResponseWriter, r *http.Request) {
	adv, ok := c.incidentAdvisor()
	if !ok {
		apispec.NotImplemented(w, r, "POST", "/api/v1/workflows/{workflowId}/incident/diagnose")
		return
	}
	inc, pack, err := adv.RequestIncidentDiagnosis(r.Context(), chi.URLParam(r, "workflowId"))
	if err != nil {
		writeIncidentError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusAccepted, IncidentResponse{Incident: incidentView(inc, &pack, adv.DeriveIncidentStatus(r.Context(), inc))})
}

// submitIncidentDiagnosis is the Diagnostic Agent's callback. It is a separate
// route from execute for the reason the whole feature rests on: the process
// that produces a proposal must not be able to carry it out, and here that is
// enforced by the two capabilities living behind two different handlers.
func (c *WorkflowsController) submitIncidentDiagnosis(w http.ResponseWriter, r *http.Request) {
	adv, ok := c.incidentAdvisor()
	if !ok {
		apispec.NotImplemented(w, r, "POST", "/api/v1/workflows/{workflowId}/incident/diagnosis")
		return
	}
	var sub workflowcore.IncidentDiagnosisSubmission
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256*1024)).Decode(&sub); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INCIDENT_DIAGNOSIS_INVALID",
			"The diagnosis payload could not be read as JSON.", nil)
		return
	}
	inc, err := adv.SubmitIncidentDiagnosis(r.Context(), chi.URLParam(r, "workflowId"), sub)
	if err != nil {
		writeIncidentError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, IncidentResponse{Incident: incidentView(inc, nil, adv.DeriveIncidentStatus(r.Context(), inc))})
}

// IncidentDiagnosisSubmissionBody documents the Diagnostic Agent's callback
// payload for the API contract. The handler decodes into the workflow package's
// own type; this mirror exists so the schema is generated from a controller
// type like every other body in the spec.
type IncidentDiagnosisSubmissionBody struct {
	IncidentID   string                  `json:"incidentId"`
	PackDigest   string                  `json:"packDigest"`
	Class        string                  `json:"classification"`
	Summary      string                  `json:"summary"`
	WhatHappened string                  `json:"whatHappened,omitempty"`
	WhatIsStuck  string                  `json:"whatIsStuck,omitempty"`
	WhyStopped   string                  `json:"whyAOStopped,omitempty"`
	Evidence     []string                `json:"evidence,omitempty"`
	Missing      []string                `json:"missingEvidence,omitempty"`
	Risk         string                  `json:"risk,omitempty"`
	Options      []IncidentOptionView    `json:"options,omitempty"`
	Action       *IncidentActionSpecBody `json:"proposedAction,omitempty"`
}

// IncidentActionSpecBody is one proposed action inside a submission.
type IncidentActionSpecBody struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// ExecuteIncidentRequest is the execute route's body.
type ExecuteIncidentRequest struct {
	IncidentID string `json:"incidentId"`
	// Approve is the explicit human yes. It is a separate field from a bare
	// POST precisely so an accidental re-submit of the diagnose call can never
	// be read as an approval.
	Approve bool `json:"approve"`
}

func (c *WorkflowsController) executeIncidentAction(w http.ResponseWriter, r *http.Request) {
	adv, ok := c.incidentAdvisor()
	if !ok {
		apispec.NotImplemented(w, r, "POST", "/api/v1/workflows/{workflowId}/incident/execute")
		return
	}
	var req ExecuteIncidentRequest
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024)).Decode(&req)
	}
	approvedBy := ""
	if req.Approve {
		// The approval is attributed to the authenticated caller, never to a
		// name supplied in the body: an agent posting JSON must not be able to
		// sign a human's approval.
		if user, ok := identity.FromContext(r.Context()); ok && strings.TrimSpace(string(user.ID)) != "" {
			approvedBy = string(user.ID)
		} else {
			// A loopback CLI/desktop caller has no identity middleware in front
			// of it. The approval is still a human act performed on this host;
			// recording it under a fixed local label is honest, where recording
			// a name from the request body would not be.
			approvedBy = "local-operator"
		}
	}
	inc, err := adv.ExecuteIncidentAction(r.Context(), chi.URLParam(r, "workflowId"), req.IncidentID, approvedBy)
	if err != nil {
		writeIncidentError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, IncidentResponse{Incident: incidentView(inc, nil, adv.DeriveIncidentStatus(r.Context(), inc))})
}

// incidentView renders the durable incident for the modal, computing every
// decision the UI would otherwise have to guess at.
func incidentView(inc workflowcore.Incident, pack *workflowcore.IncidentContextPack, status workflowcore.IncidentStatus) IncidentView {
	v := IncidentView{
		ID: inc.ID, RunID: inc.RunID, State: string(inc.State),
		StopReason: inc.StopReason, StopDetail: inc.StopDetail, Stale: inc.Stale,
		Diagnoses: inc.Diagnoses, MaxDiagnoses: workflowcore.MaxIncidentDiagnoses,
		Repairs: inc.Repairs, MaxRepairs: workflowcore.MaxIncidentRepairs,
		CanDiagnose: inc.CanDiagnose(), CanExecute: inc.CanExecute(),
		LaunchOutcome:     string(inc.LaunchOutcome),
		Progress:          string(status.Progress),
		DiagnosticHarness: status.DiagnosticHarness,
		DiagnosisJob:      diagnosisJobView(status.Diagnosis),
		CapacityReasons:   status.CapacityReasons,
		ClosureCause:      inc.ClosureCause,
		ClosureEvidence:   inc.ClosureEvidence,
	}
	if status.NextEvaluationAt != nil {
		v.NextEvaluationAt = status.NextEvaluationAt.UTC().Format(time.RFC3339)
	}
	if pack != nil {
		pv := IncidentPackView{
			Digest: pack.Digest, Bytes: pack.Bytes, MaxBytes: pack.MaxBytes,
			EstimatedTokens: pack.EstimatedTokens, Dropped: pack.DroppedSections,
		}
		for _, s := range pack.Sections {
			if !s.Dropped {
				pv.Sections = append(pv.Sections, s.Title)
			}
		}
		v.Pack = &pv
	}
	if inc.RepairRunID != "" {
		v.Repair = &IncidentRepairView{
			RunID: inc.RepairRunID, ApprovedBy: inc.ApprovedBy,
			Generation: inc.Repairs, MaxRepairs: workflowcore.MaxIncidentRepairs,
			ReviewerHarness: status.ReviewerHarness,
			VerifyResult:    status.VerifyResult,
			FinalSHA:        status.FinalSHA,
		}
	}
	if inc.Diagnosis == nil {
		return v
	}
	d := inc.Diagnosis
	dv := &IncidentDiagnosisView{
		Class: string(d.Class), Summary: d.Summary,
		WhatHappened: d.WhatHappened, WhatIsStuck: d.WhatIsStuck, WhyStopped: d.WhyStopped,
		Evidence: d.Evidence, Missing: d.Missing, Risk: d.Risk,
	}
	for _, o := range d.Options {
		dv.Options = append(dv.Options, IncidentOptionView{
			ID: o.ID, Label: o.Label, Detail: o.Detail, Consequence: o.Consequence,
		})
	}
	if d.Action != nil {
		describe, risk, needs, endsWork, writesCode, executable, refusal := workflowcore.DescribeIncidentAction(d.Class, d.Action.Kind)
		dv.Action = &IncidentActionView{
			Kind: string(d.Action.Kind), Rationale: d.Action.Reason,
			Describe: describe, Risk: risk, NeedsApproval: needs,
			Destructive: endsWork, WritesCode: writesCode,
			Executable:    executable && inc.CanExecute() && !inc.Stale,
			RefusalReason: refusal,
		}
	}
	v.Diagnosis = dv
	return v
}

func writeIncidentError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, workflowcore.ErrIncidentUnavailable):
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict", "INCIDENT_UNAVAILABLE", err.Error(), nil)
	case errors.Is(err, workflowcore.ErrIncidentStale):
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict", "INCIDENT_STALE", err.Error(), nil)
	case errors.Is(err, workflowsvc.ErrNotFound), errors.Is(err, workflowcore.ErrNotFound):
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "INCIDENT_NOT_FOUND", err.Error(), nil)
	default:
		envelope.WriteAPIError(w, r, http.StatusUnprocessableEntity, "unprocessable", "INCIDENT_REFUSED", err.Error(), nil)
	}
}

// IncidentDiagnosisJobView is the investigation's own status, separate from the
// incident's. A person watching an investigation needs to know it is running,
// which agent and provider are running it, since when, and — the field the
// whole thing exists for — whether it is actually blocked on a prompt.
type IncidentDiagnosisJobView struct {
	State   string `json:"state" enum:"queued,starting,running,waiting_for_provider,waiting_for_user,completed,failed"`
	Attempt int    `json:"attempt,omitempty"`
	Max     int    `json:"max,omitempty"`

	SessionID string `json:"sessionId,omitempty"`
	Harness   string `json:"harness,omitempty"`

	StartedAt      string `json:"startedAt,omitempty"`
	ElapsedSeconds int    `json:"elapsedSeconds,omitempty"`
	LastActivityAt string `json:"lastActivityAt,omitempty"`
	LastSignalAt   string `json:"lastSignalAt,omitempty"`

	// BlockingInteraction is empty when AO does not know. The UI must render
	// that as "unknown", never as "nothing is blocking it".
	BlockingInteraction string `json:"blockingInteraction,omitempty"`
}

func diagnosisJobView(job workflowcore.IncidentDiagnosisJob) IncidentDiagnosisJobView {
	v := IncidentDiagnosisJobView{
		State: string(job.State), Attempt: job.Attempt, Max: job.Max,
		SessionID: job.SessionID, Harness: job.Harness,
		ElapsedSeconds:      job.ElapsedSeconds,
		BlockingInteraction: job.BlockingInteraction,
	}
	stamp := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format(time.RFC3339)
	}
	v.StartedAt, v.LastActivityAt, v.LastSignalAt = stamp(job.StartedAt), stamp(job.LastActivityAt), stamp(job.LastSignalAt)
	return v
}
