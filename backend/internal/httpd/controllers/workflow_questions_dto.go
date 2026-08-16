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
	AnswerSource         string                           `json:"answerSource,omitempty" enum:",policy,human"`
	AnswerText           string                           `json:"answerText,omitempty"`
	Delivered            bool                             `json:"delivered"`
	DeliveredAt          *string                          `json:"deliveredAt,omitempty"`
}

// ListWorkflowQuestionsResponse is the body of
// GET /api/v1/workflows/{workflowId}/questions.
type ListWorkflowQuestionsResponse struct {
	Questions []WorkflowQuestionResponse `json:"questions"`
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

func workflowQuestionResponses(qs []domain.WorkflowQuestion) []WorkflowQuestionResponse {
	out := make([]WorkflowQuestionResponse, 0, len(qs))
	for _, q := range qs {
		out = append(out, workflowQuestionResponse(q))
	}
	return out
}
