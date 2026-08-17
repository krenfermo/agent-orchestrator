package controllers

import (
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// WorkflowQuestionChoiceResponse is one structured choice offered alongside
// a captured question.
type WorkflowQuestionChoiceResponse struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// WorkflowQuestionResponse is one durable captured question on the wire
// (Checkpoint 8K-A). QuestionText is "" when Certainty is "unknown" — the
// frontend renders the fixed fallback string for that case, never invented
// text.
type WorkflowQuestionResponse struct {
	ID                   string                           `json:"id"`
	WorkflowRunID        string                           `json:"workflowRunId"`
	WorkflowStepID       string                           `json:"workflowStepId,omitempty"`
	SessionID            string                           `json:"sessionId,omitempty"`
	AskingHarness        string                           `json:"askingHarness,omitempty"`
	AskingRole           string                           `json:"askingRole,omitempty"`
	QuestionText         string                           `json:"questionText"`
	StructuredChoices    []WorkflowQuestionChoiceResponse `json:"structuredChoices,omitempty"`
	Certainty            string                           `json:"certainty" enum:"actual,inferred,unknown"`
	Classification       string                           `json:"classification" enum:"policy_resolvable,auto_resolvable,human_required,ambiguous"`
	ClassificationReason string                           `json:"classificationReason,omitempty"`
	State                string                           `json:"state" enum:"pending,resolving,answered,human_required,cancelled"`
	CreatedAt            string                           `json:"createdAt"`
	AnsweredAt           *string                          `json:"answeredAt,omitempty"`
	AnswerSource         string                           `json:"answerSource,omitempty" enum:",policy,human,resolver"`
	AnswerText           string                           `json:"answerText,omitempty"`
	Delivered            bool                             `json:"delivered"`
	DeliveredAt          *string                          `json:"deliveredAt,omitempty"`

	// The fields below surface Checkpoint 8K-B's cross-provider Decision
	// Resolver (pass 3), when a resolution attempt exists for this
	// question. ResolverHarness/ResolverProvider/ResolverReasonSummary/
	// ResolverEvidenceReferences describe the attempt itself and are safe
	// to show regardless of outcome. AdvisoryAnswer is set ONLY when the
	// resolver completed with requiresHuman=true (it could not determine a
	// safe answer) — it is the resolver's non-binding advisory, NEVER a
	// delivered decision, and must never be confused with AnswerText
	// (which is only ever set once the question is actually answered).
	ResolverHarness            string   `json:"resolverHarness,omitempty"`
	ResolverProvider           string   `json:"resolverProvider,omitempty"`
	ResolverReasonSummary      string   `json:"resolverReasonSummary,omitempty"`
	ResolverEvidenceReferences []string `json:"resolverEvidenceReferences,omitempty"`
	ResolverAdvisoryAnswer     string   `json:"resolverAdvisoryAnswer,omitempty"`
}

// ListWorkflowQuestionsResponse is the body of
// GET /api/v1/workflows/{workflowId}/questions and, reused as-is, of
// GET /api/v1/questions/pending (Checkpoint 8K-B pass 3's global inbox).
type ListWorkflowQuestionsResponse struct {
	Questions []WorkflowQuestionResponse `json:"questions"`
}

// PendingDecisionsQuery is the query-param container for
// GET /api/v1/questions/pending. State may repeat (?state=a&state=b) to
// filter to more than one of human_required/resolving/waiting_for_capacity;
// omitted entirely, the endpoint defaults to human_required+resolving (see
// ListPendingWorkflowQuestions's doc comment).
type PendingDecisionsQuery struct {
	State []string `query:"state,omitempty" enum:"human_required,resolving,waiting_for_capacity" description:"Filter to one or more of human_required, resolving, waiting_for_capacity. Omit for the default (human_required + resolving)."`
}

// WorkflowQuestionResponseBody is the body of the single-question GET and
// the answer POST.
type WorkflowQuestionResponseBody struct {
	Question WorkflowQuestionResponse `json:"question"`
}

func workflowQuestionResponse(q domain.WorkflowQuestion) WorkflowQuestionResponse {
	out := WorkflowQuestionResponse{
		ID:                   string(q.ID),
		WorkflowRunID:        string(q.WorkflowRunID),
		AskingHarness:        string(q.AskingHarness),
		AskingRole:           q.AskingRole,
		QuestionText:         q.QuestionText,
		Certainty:            string(q.Certainty),
		Classification:       string(q.Classification),
		ClassificationReason: q.ClassificationReason,
		State:                string(q.State),
		CreatedAt:            q.CreatedAt.Format(rfc3339Milli),
		AnswerText:           q.AnswerText,
		Delivered:            q.Delivered,
	}
	if q.WorkflowStepID != nil {
		out.WorkflowStepID = string(*q.WorkflowStepID)
	}
	if q.SessionID != nil {
		out.SessionID = string(*q.SessionID)
	}
	if q.AnswerSource != nil {
		out.AnswerSource = string(*q.AnswerSource)
	}
	if q.AnsweredAt != nil {
		v := q.AnsweredAt.Format(rfc3339Milli)
		out.AnsweredAt = &v
	}
	if q.DeliveredAt != nil {
		v := q.DeliveredAt.Format(rfc3339Milli)
		out.DeliveredAt = &v
	}
	for _, c := range q.StructuredChoices {
		out.StructuredChoices = append(out.StructuredChoices, WorkflowQuestionChoiceResponse{ID: c.ID, Label: c.Label})
	}
	return out
}

// applyResolverEnrichment attaches Checkpoint 8K-B pass 3's resolver fields
// to an already-built WorkflowQuestionResponse from that question's current
// resolution attempt, if any. Never sets ResolverAdvisoryAnswer unless
// resolution.RequiresHuman is true — see WorkflowQuestionResponse's doc
// comment for why that distinction must never blur.
func applyResolverEnrichment(out WorkflowQuestionResponse, resolution domain.WorkflowQuestionResolution, found bool) WorkflowQuestionResponse {
	if !found {
		return out
	}
	out.ResolverHarness = string(resolution.ResolverHarness)
	out.ResolverProvider = string(domain.ProviderForHarness(resolution.ResolverHarness))
	out.ResolverReasonSummary = resolution.ReasonSummary
	out.ResolverEvidenceReferences = resolution.EvidenceReferences
	if resolution.RequiresHuman {
		out.ResolverAdvisoryAnswer = resolution.Answer
	}
	return out
}

// resolverEnrichmentEligible reports whether a question could have a
// resolution worth fetching: only questions that are (or were) in the
// resolving/human_required lifecycle ever get a workflow_question_resolutions
// row (see dispatchDecisionResolver / observeResolutionStep in
// internal/workflow). Skipping the lookup for every other question avoids
// an unnecessary store round-trip on the common case (policy/human answered
// questions, pending questions never routed through the resolver).
func resolverEnrichmentEligible(q domain.WorkflowQuestion) bool {
	return q.ResolvingRunID != nil
}

func workflowQuestionResponses(qs []domain.WorkflowQuestion) []WorkflowQuestionResponse {
	out := make([]WorkflowQuestionResponse, 0, len(qs))
	for _, q := range qs {
		out = append(out, workflowQuestionResponse(q))
	}
	return out
}
