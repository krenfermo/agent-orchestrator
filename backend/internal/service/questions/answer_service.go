package questions

import (
	"context"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Sentinel errors the controller layer maps to HTTP status codes. Kept in
// this package (not the controller) so the validation rules — the actual
// business logic of "can this question be answered right now" — live next
// to the rest of Checkpoint 8K-A's domain logic, not in transport code.
var (
	ErrNotFound        = errors.New("questions: not found")
	ErrWrongRun        = errors.New("questions: question does not belong to this workflow run")
	ErrNotAnswerable   = errors.New("questions: question is not awaiting a human answer")
	ErrRunCancelled    = errors.New("questions: workflow run is cancelled")
	ErrInvalidChoice   = errors.New("questions: choiceId does not match any of the question's structured choices")
	ErrAmbiguousAnswer = errors.New("questions: exactly one of choiceId or customText must be set")
)

// APIStore is the persistence surface the human-answer API needs, layered
// on top of Store/DeliveryStore. Satisfied by *store.Store.
type APIStore interface {
	Store
	DeliveryStore
	GetWorkflowQuestion(ctx context.Context, id string) (domain.WorkflowQuestion, bool, error)
	ListWorkflowQuestionsByRun(ctx context.Context, runID string) ([]domain.WorkflowQuestion, error)
}

// RunLookup is the narrow run-read contract AnswerService needs to reject
// answering a question on an already-cancelled run. Satisfied by
// *store.Store (it already implements workflow.Store.GetWorkflowRun).
type RunLookup interface {
	GetWorkflowRun(ctx context.Context, id string) (domain.WorkflowRun, bool, error)
}

// AnswerService implements Checkpoint 8K-A's human-answer use case: the
// controller-facing surface for GET (list/detail) and POST .../answer. It
// is deliberately thin — validation plus a single AnswerWorkflowQuestion
// compare-and-swap plus an immediate delivery attempt via the real Send
// path, no new persistence beyond what the store package already exposes.
type AnswerService struct {
	Store  APIStore
	Runs   RunLookup
	Sender MessageSender
	Clock  func() time.Time
}

func (s *AnswerService) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now().UTC()
}

// ListByRun returns every question (any state) recorded for a run, oldest
// first — the run-detail embedding and the list endpoint both use this.
func (s *AnswerService) ListByRun(ctx context.Context, runID string) ([]domain.WorkflowQuestion, error) {
	return s.Store.ListWorkflowQuestionsByRun(ctx, runID)
}

// Get fetches a single question and verifies it belongs to runID.
func (s *AnswerService) Get(ctx context.Context, runID, questionID string) (domain.WorkflowQuestion, error) {
	q, ok, err := s.Store.GetWorkflowQuestion(ctx, questionID)
	if err != nil {
		return domain.WorkflowQuestion{}, err
	}
	if !ok {
		return domain.WorkflowQuestion{}, ErrNotFound
	}
	if string(q.WorkflowRunID) != runID {
		return domain.WorkflowQuestion{}, ErrWrongRun
	}
	return q, nil
}

// Answer validates and persists a human answer, then attempts immediate
// delivery via the real Send path. Exactly one of choiceID/customText must
// be set; a choiceID must match one of the question's structured choices.
// A question not currently human_required (including one already answered
// or cancelled) is rejected with ErrNotAnswerable — never a silent
// overwrite. A cancelled run also rejects with ErrRunCancelled.
func (s *AnswerService) Answer(ctx context.Context, runID, questionID string, choiceID, customText *string) (domain.WorkflowQuestion, error) {
	hasChoice := choiceID != nil && *choiceID != ""
	hasCustom := customText != nil && *customText != ""
	if hasChoice == hasCustom {
		return domain.WorkflowQuestion{}, ErrAmbiguousAnswer
	}

	q, err := s.Get(ctx, runID, questionID)
	if err != nil {
		return domain.WorkflowQuestion{}, err
	}
	if q.State != domain.QuestionStateHumanRequired {
		return domain.WorkflowQuestion{}, ErrNotAnswerable
	}

	if s.Runs != nil {
		if run, ok, err := s.Runs.GetWorkflowRun(ctx, runID); err != nil {
			return domain.WorkflowQuestion{}, err
		} else if ok && run.State == domain.WorkflowRunCancelled {
			return domain.WorkflowQuestion{}, ErrRunCancelled
		}
	}

	answerText := ""
	answerRef := ""
	if hasChoice {
		matched := false
		for _, c := range q.StructuredChoices {
			if c.ID == *choiceID {
				matched = true
				answerText = c.Label
				answerRef = c.ID
				break
			}
		}
		if !matched {
			return domain.WorkflowQuestion{}, ErrInvalidChoice
		}
	} else {
		answerText = *customText
	}

	now := s.now()
	ok, err := s.Store.AnswerWorkflowQuestion(ctx, questionID, domain.QuestionStateHumanRequired, domain.QuestionStateAnswered, domain.AnswerSourceHuman, answerText, answerRef, now)
	if err != nil {
		return domain.WorkflowQuestion{}, err
	}
	if !ok {
		// Lost a race with a concurrent answer/cancel between Get and here.
		return domain.WorkflowQuestion{}, ErrNotAnswerable
	}

	if s.Sender != nil {
		if _, err := DeliverAnswered(ctx, s.Store, s.Sender, runID, now); err != nil {
			return domain.WorkflowQuestion{}, err
		}
	}

	updated, err := s.Get(ctx, runID, questionID)
	if err != nil {
		return domain.WorkflowQuestion{}, err
	}
	return updated, nil
}
