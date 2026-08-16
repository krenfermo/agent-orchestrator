package questions

import (
	"context"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// DeliveryStore is the narrow persistence contract the delivery sweep
// needs. Satisfied by *store.Store.
type DeliveryStore interface {
	ListUndeliveredAnsweredWorkflowQuestions(ctx context.Context, runID string) ([]domain.WorkflowQuestion, error)
	MarkWorkflowQuestionDelivered(ctx context.Context, id string, deliveredAt time.Time) (bool, error)
}

// MessageSender is the canonical existing-session messaging path
// (*session_manager.Manager.Send) questions delivery reuses to hand an
// answer back to the paused worker session. Deliberately the same shape as
// workflowcore.MessageSender, redeclared locally rather than imported to
// avoid an import cycle (workflow already imports this package for its
// dispatch guards).
type MessageSender interface {
	Send(ctx context.Context, id domain.SessionID, message string, attachment *ports.SpawnAttachment) error
}

// DeliverAnswered is the poll-driven sweep: every question row with
// state=answered AND delivered=0 for a run gets delivered via the
// canonical session-messaging mechanism, then delivered=1 is persisted.
// Called both immediately after an answer is computed (for
// responsiveness) and again at the top of every GetRun/Reconcile pass (for
// restart recovery) — the delivered flag makes both call sites safely
// idempotent, so redundant calls are expected and required.
func DeliverAnswered(ctx context.Context, store DeliveryStore, sender MessageSender, runID string, now time.Time) (int, error) {
	if store == nil || sender == nil {
		return 0, nil
	}
	pending, err := store.ListUndeliveredAnsweredWorkflowQuestions(ctx, runID)
	if err != nil {
		return 0, err
	}

	delivered := 0
	for _, q := range pending {
		if q.SessionID == nil || *q.SessionID == "" {
			// No session to deliver to (should not normally happen for an
			// answered question); leave it for a later sweep once/if a
			// session is associated rather than losing the answer.
			continue
		}
		msg := deliveryMessage(q)
		if err := sender.Send(ctx, *q.SessionID, msg, nil); err != nil {
			return delivered, fmt.Errorf("deliver answer for question %s: %w", q.ID, err)
		}
		ok, err := store.MarkWorkflowQuestionDelivered(ctx, string(q.ID), now)
		if err != nil {
			return delivered, fmt.Errorf("mark question %s delivered: %w", q.ID, err)
		}
		if ok {
			delivered++
		}
	}
	return delivered, nil
}

func deliveryMessage(q domain.WorkflowQuestion) string {
	source := "human"
	if q.AnswerSource != nil {
		source = string(*q.AnswerSource)
	}
	return fmt.Sprintf("Decision:\n%s\n\nSource: %s\n\nContinue the current task using this decision.", q.AnswerText, source)
}
