package questions

import (
	"context"
	"errors"
	"log/slog"
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
	// ListPendingWorkflowQuestions backs Checkpoint 8K-B pass 3's global
	// "Pending Decisions" inbox: every open/in-flight question across ALL
	// runs, optionally filtered to the given states.
	ListPendingWorkflowQuestions(ctx context.Context, states []string) ([]domain.WorkflowQuestion, error)
	// GetCurrentResolutionForQuestion backs pass 3's per-question resolver
	// enrichment (resolver harness/advisory), following the question's
	// resolving_run_id pointer. Returns ok=false, no error, when the
	// question has never had a resolution attempt.
	GetCurrentResolutionForQuestion(ctx context.Context, questionID string) (domain.WorkflowQuestionResolution, bool, error)
	// ListWorkflowQuestionResolutionsByRun backs pass 3's Decisions
	// telemetry section: every resolution attempt ever recorded for a run.
	ListWorkflowQuestionResolutionsByRun(ctx context.Context, runID string) ([]domain.WorkflowQuestionResolution, error)
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
	// Inputs reports whether a target session is sitting on a prompt of its
	// own, so the immediate text delivery below cannot consume an answer a
	// modal dialog would swallow. Optional; nil means AO cannot tell, which
	// preserves the previous behaviour. See questions.SessionInputState.
	Inputs SessionInputState
	Clock  func() time.Time
	// Logger receives the one thing this service now swallows: a text delivery
	// that was refused after the answer was already recorded. Optional.
	Logger *slog.Logger
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

// ListPending returns every open/in-flight question across all runs
// (Checkpoint 8K-B pass 3's global inbox), optionally filtered to states.
func (s *AnswerService) ListPending(ctx context.Context, states []string) ([]domain.WorkflowQuestion, error) {
	return s.Store.ListPendingWorkflowQuestions(ctx, states)
}

// GetResolution returns the current Decision Resolver attempt for a
// question, if any (Checkpoint 8K-B pass 3's per-question resolver
// enrichment). ok=false, no error, when the question has never had one.
func (s *AnswerService) GetResolution(ctx context.Context, questionID string) (domain.WorkflowQuestionResolution, bool, error) {
	return s.Store.GetCurrentResolutionForQuestion(ctx, questionID)
}

// ListResolutionsByRun returns every Decision Resolver attempt ever
// recorded for a run (Checkpoint 8K-B pass 3's telemetry source).
func (s *AnswerService) ListResolutionsByRun(ctx context.Context, runID string) ([]domain.WorkflowQuestionResolution, error) {
	return s.Store.ListWorkflowQuestionResolutionsByRun(ctx, runID)
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
		// Best-effort, and deliberately so (P3-C).
		//
		// The answer is already durable at this point. Delivering it is a
		// separate obligation that the reconcile sweep retries, and for a
		// worker blocked on a select dialog this text path is REFUSED on
		// purpose -- sessionguard will not let a paste answer a prompt whose
		// Enter confirms a highlighted row. The structured path is what carries
		// such an answer over.
		//
		// Returning that refusal to the caller reported a 500 for an answer
		// that had been recorded perfectly well, which tells a person their
		// decision failed when it did not, and invites them to submit it again.
		if _, err := DeliverAnsweredWithState(ctx, s.Store, s.Sender, s.Inputs, runID, now); err != nil && s.Logger != nil {
			s.Logger.Warn("questions: the answer was recorded but could not be delivered as text; it stays pending for the structured delivery sweep",
				"run", runID, "question", questionID, "err", err)
		}
	}

	updated, err := s.Get(ctx, runID, questionID)
	if err != nil {
		return domain.WorkflowQuestion{}, err
	}
	return updated, nil
}
