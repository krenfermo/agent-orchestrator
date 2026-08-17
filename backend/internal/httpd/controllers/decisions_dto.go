package controllers

import (
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ResolverSessionIDParam is the {resolverSessionId} path parameter for the
// Decision Resolver callback route.
type ResolverSessionIDParam struct {
	ResolverSessionID string `path:"resolverSessionId" description:"Decision Resolver session identifier baked into the resolver's launch prompt."`
}

// ResolveDecisionRequest is the body of
// POST /api/v1/sessions/{resolverSessionId}/decisions/resolve. Exactly one
// of Answer or RequiresHuman must be set. No transcript/chain-of-thought
// field exists here, deliberately — see ao decision resolve's CLI docs.
type ResolveDecisionRequest struct {
	RunID              string   `json:"runId" description:"Resolution run id (workflow_question_resolutions.id) this result is for."`
	Answer             string   `json:"answer,omitempty" description:"The resolver's answer. Required unless requiresHuman is true."`
	ReasonSummary      string   `json:"reasonSummary,omitempty" description:"Short reason summary, capped."`
	EvidenceReferences []string `json:"evidenceReferences,omitempty" description:"Bounded list of evidence references (file paths / line ranges), never a transcript. Max 10, 500 chars each."`
	Certainty          string   `json:"certainty,omitempty" enum:",actual,inferred,unknown" description:"Required when answer is set."`
	RequiresHuman      bool     `json:"requiresHuman,omitempty" description:"True when the resolver could not determine a safe answer."`
}

// ResolveDecisionResponse is the body of a successful decision resolve call.
type ResolveDecisionResponse struct {
	Resolution WorkflowQuestionResolutionResponse `json:"resolution"`
}

// WorkflowQuestionResolutionResponse is one Decision Resolver attempt on the
// wire (Checkpoint 8K-B). Never carries a transcript/chain-of-thought field.
type WorkflowQuestionResolutionResponse struct {
	ID                 string   `json:"id"`
	WorkflowQuestionID string   `json:"workflowQuestionId"`
	WorkflowRunID      string   `json:"workflowRunId"`
	ResolverHarness    string   `json:"resolverHarness,omitempty"`
	ResolverSessionID  string   `json:"resolverSessionId,omitempty"`
	Status             string   `json:"status" enum:"pending,running,complete,failed,cancelled"`
	Answer             string   `json:"answer,omitempty"`
	ReasonSummary      string   `json:"reasonSummary,omitempty"`
	EvidenceReferences []string `json:"evidenceReferences,omitempty"`
	Certainty          string   `json:"certainty,omitempty" enum:",actual,inferred,unknown"`
	RequiresHuman      bool     `json:"requiresHuman"`
	CreatedAt          string   `json:"createdAt"`
	UpdatedAt          string   `json:"updatedAt"`
	CompletedAt        *string  `json:"completedAt,omitempty"`
}

func workflowQuestionResolutionResponse(r domain.WorkflowQuestionResolution) WorkflowQuestionResolutionResponse {
	out := WorkflowQuestionResolutionResponse{
		ID:                 string(r.ID),
		WorkflowQuestionID: string(r.WorkflowQuestionID),
		WorkflowRunID:      string(r.WorkflowRunID),
		ResolverHarness:    string(r.ResolverHarness),
		Status:             string(r.Status),
		Answer:             r.Answer,
		ReasonSummary:      r.ReasonSummary,
		EvidenceReferences: r.EvidenceReferences,
		RequiresHuman:      r.RequiresHuman,
		CreatedAt:          r.CreatedAt.Format(rfc3339Milli),
		UpdatedAt:          r.UpdatedAt.Format(rfc3339Milli),
	}
	if r.ResolverSessionID != nil {
		out.ResolverSessionID = string(*r.ResolverSessionID)
	}
	if r.Certainty != nil {
		out.Certainty = string(*r.Certainty)
	}
	if r.CompletedAt != nil {
		v := r.CompletedAt.Format(rfc3339Milli)
		out.CompletedAt = &v
	}
	return out
}
