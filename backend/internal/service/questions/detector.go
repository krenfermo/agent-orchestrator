package questions

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// CaptureParserVersion is bumped whenever a QuestionPaneParser's marker
// heuristics change in a way that would affect stored evidence metadata.
const CaptureParserVersion = "8k-a.v1"

// PaneCaptureRangeLines is the bounded recent-window size passed to
// ports.Runtime.GetOutput by callers before invoking Detect. Recorded as
// evidence metadata (capture_range_lines), never used to store the pane
// text itself.
const PaneCaptureRangeLines = 120

// Store is the narrow persistence contract Detect needs. Satisfied by
// *store.Store (backend/internal/storage/sqlite/store), which implements
// each of these against the workflow_questions table.
type Store interface {
	InsertWorkflowQuestion(ctx context.Context, q domain.WorkflowQuestion) (domain.WorkflowQuestion, bool, error)
	ListOpenWorkflowQuestionsByRun(ctx context.Context, runID string) ([]domain.WorkflowQuestion, error)
	CountPolicyAnsweredWorkflowQuestionsByStep(ctx context.Context, stepID string) (int64, error)
	AnswerWorkflowQuestion(ctx context.Context, id string, expectedState, newState domain.QuestionState, source domain.AnswerSource, answerText, answerReference string, answeredAt time.Time) (bool, error)
}

// DetectInput bundles the already-resolved facts Detect needs: everything
// here is either already captured by the caller (bounded pane text) or
// already durably stored elsewhere (run/step/policy identity, checkpoint
// branch/worktree). Detect performs no I/O of its own beyond the Store and
// parser calls it is explicitly given.
type DetectInput struct {
	RunID                         domain.WorkflowRunID
	StepID                        *domain.WorkflowStepID
	AttemptID                     *string
	SessionID                     *domain.SessionID
	AskingHarness                 domain.AgentHarness
	AskingRole                    string
	PaneText                      string
	CaptureProvider               string
	PolicyVersionAtCapture        string
	WorkspaceFingerprintAtCapture string
	Branch                        string
	WorktreePath                  string
	MaxAutoAnswered               int
	Now                           time.Time
	NewID                         func() string
}

// DetectResult is what Detect produced.
type DetectResult struct {
	Question domain.WorkflowQuestion
	// Inserted is false when the fingerprint already existed (idempotent
	// dedup no-op): the returned Question is the pre-existing row, and it
	// was NOT reclassified or re-persisted.
	Inserted bool
}

// Detect is Checkpoint 8K-A's single synchronous orchestration step:
// capture (already done by the caller into in.PaneText) -> parse -> print
// -> classify -> persist, as one store transaction (the row is inserted
// with classification already computed). A crash before InsertWorkflowQuestion
// commits just means the next poll re-detects from scratch, safely deduped
// by fingerprint on retry.
func Detect(ctx context.Context, store Store, parser ports.QuestionPaneParser, in DetectInput) (DetectResult, error) {
	var (
		questionText string
		choices      []domain.QuestionChoice
		certainty    domain.QuestionCertainty
	)

	candidate, ok := ports.QuestionCandidate{}, false
	if parser != nil {
		candidate, ok = parser.ParseQuestion(in.PaneText)
	}
	if ok {
		questionText = candidate.QuestionText
		choices = candidate.StructuredChoices
		certainty = candidate.Certainty
	} else {
		// Conservative fallback: state is waiting/blocked but text can't be
		// reconstructed reliably. Never invent text.
		questionText = ""
		certainty = domain.QuestionCertaintyUnknown
	}

	stepIDStr := ""
	if in.StepID != nil {
		stepIDStr = string(*in.StepID)
	}
	fingerprint := Fingerprint(in.RunID, stepIDStr, questionText, choices, in.PolicyVersionAtCapture, in.WorkspaceFingerprintAtCapture)

	classification, reason := Classify(questionText, certainty)

	budgetCtx := ClassifyContext{
		Branch:          in.Branch,
		WorktreePath:    in.WorktreePath,
		MaxAutoAnswered: in.MaxAutoAnswered,
	}
	// Checkpoint 8K-B pass 2 budget-sharing decision: auto_resolvable now
	// also computes PolicyAnsweredCount (previously only policy_resolvable
	// did — a gap that left the auto_resolvable branch of ResolveState's
	// budget check permanently 0-vs-0, i.e. never enforced). The intended
	// design (per ClassifyContext.PolicyAnsweredCount's doc comment, and the
	// recommendation left there in pass 1) is a single shared count across
	// answer_source IN ('policy','resolver'); the backing query
	// (queries/workflow_questions.sql's CountPolicyAnsweredWorkflowQuestionsByStep)
	// still filters strictly answer_source='policy' because widening its
	// WHERE clause reproducibly triggered a sqlc v1.31.1 SQL-codegen
	// corruption bug specific to this query file (see the store-level note
	// on Store.TransitionWorkflowQuestionState for the same bug hit
	// elsewhere in this file). Until that is worked around, both
	// classifications draw against this policy-only count as a
	// conservative stand-in — strictly safer than the prior "auto_resolvable
	// budget never enforced" gap, though not yet a true shared count with
	// resolver-answered questions. Flagged for pass 3.
	if (classification == domain.QuestionClassificationPolicyResolvable || classification == domain.QuestionClassificationAutoResolvable) && stepIDStr != "" {
		count, err := store.CountPolicyAnsweredWorkflowQuestionsByStep(ctx, stepIDStr)
		if err != nil {
			return DetectResult{}, err
		}
		budgetCtx.PolicyAnsweredCount = int(count)
	}
	state := ResolveState(classification, budgetCtx)

	newID := string(in.RunID) + ":question:" + fingerprint[:12]
	if in.NewID != nil {
		newID = in.NewID()
	}

	row := domain.WorkflowQuestion{
		ID:                   domain.WorkflowQuestionID(newID),
		WorkflowRunID:        in.RunID,
		WorkflowStepID:       in.StepID,
		WorkflowAttemptID:    in.AttemptID,
		SessionID:            in.SessionID,
		AskingHarness:        in.AskingHarness,
		AskingRole:           in.AskingRole,
		Fingerprint:          fingerprint,
		QuestionText:         questionText,
		StructuredChoices:    choices,
		CaptureProvider:      in.CaptureProvider,
		CaptureParserVersion: CaptureParserVersion,
		CaptureRangeLines:    PaneCaptureRangeLines,
		Certainty:            certainty,
		Classification:       classification,
		ClassificationReason: reason,
		State:                state,
		CreatedAt:            in.Now,
	}

	saved, inserted, err := store.InsertWorkflowQuestion(ctx, row)
	if err != nil {
		return DetectResult{}, err
	}
	if !inserted {
		// Duplicate fingerprint: never reclassified, never re-persisted.
		return DetectResult{Question: saved, Inserted: false}, nil
	}

	// Policy resolution happens inline, synchronously, right after a fresh
	// policy_resolvable insert lands in "pending" — no second-LLM call, no
	// network call, purely a deterministic read of already-stored facts.
	if saved.State == domain.QuestionStatePending && saved.Classification == domain.QuestionClassificationPolicyResolvable {
		answerText, ok := ResolvePolicyAnswer(saved.QuestionText, budgetCtx)
		if ok {
			answeredOK, aerr := store.AnswerWorkflowQuestion(ctx, string(saved.ID), domain.QuestionStatePending, domain.QuestionStateAnswered, domain.AnswerSourcePolicy, answerText, "", in.Now)
			if aerr != nil {
				return DetectResult{}, aerr
			}
			if answeredOK {
				saved.State = domain.QuestionStateAnswered
				policySrc := domain.AnswerSourcePolicy
				saved.AnswerSource = &policySrc
				saved.AnswerText = answerText
			}
		}
	}

	return DetectResult{Question: saved, Inserted: true}, nil
}
