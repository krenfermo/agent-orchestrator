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

// SessionInputState answers one question about one session: is the agent
// sitting on a prompt of its own right now.
//
// It exists because "the send call returned nil" is not a receipt (P3-D §4),
// and this path was treating it as one. A provider dialog is MODAL: text typed
// while one is open never reaches the composer, the agent never sees it, and
// the write still succeeds. Marking the answer delivered on that write records
// a fact that is not true, and the run then waits forever for a worker holding
// a decision nobody handed it — which is exactly what the P3-D smoke B run did,
// with the correct answer sitting in the database and the dialog still on the
// screen.
//
// The refusal this restores was supposed to come from sessionguard, which
// refuses a send into a session reading `blocked`. That covers a permission
// prompt and nothing else: a provider's own select dialog reports
// `waiting_input`, so the guard admitted the write. The whole needs-input
// family is what has to be excluded here, not one member of it.
//
// Optional. A nil reader means AO cannot tell, and cannot-tell keeps the
// previous behaviour rather than grounding every delivery.
type SessionInputState interface {
	// AwaitingInput reports whether this session is waiting on something of its
	// own. `known` is false when AO could not read the session at all, which is
	// never read as "no".
	AwaitingInput(ctx context.Context, id domain.SessionID) (awaiting, known bool)
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
	return DeliverAnsweredWithState(ctx, store, sender, nil, runID, now)
}

// DeliverAnsweredWithState is DeliverAnswered plus the one fact that makes its
// receipt honest: whether the target session is sitting on a prompt of its own.
//
// A question whose session is awaiting input is SKIPPED — not failed. Its
// answer stays durable and undelivered, which is precisely the state the
// structured dialog path (workflow/dialog_delivery.go) claims: that path
// presses the selection key and marks the row delivered only after re-observing
// the prompt gone. Leaving the row for it is what lets the two paths compose
// instead of racing, and the text path stops being able to consume an answer
// the worker never received.
func DeliverAnsweredWithState(
	ctx context.Context, store DeliveryStore, sender MessageSender,
	state SessionInputState, runID string, now time.Time,
) (int, error) {
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
		if state != nil {
			if awaiting, known := state.AwaitingInput(ctx, *q.SessionID); known && awaiting {
				// A modal prompt would swallow this write and the write would
				// still succeed. Leave the answer for the path that can prove
				// it landed.
				continue
			}
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
	// Checkpoint 8K-B: a resolver-produced answer gets a slightly more
	// descriptive source label than the bare enum value, per the checkpoint's
	// delivery-format requirement — evidence references live on the
	// workflow_question_resolutions row, not on WorkflowQuestion itself, so
	// they are not included here; pass 3's UI is expected to surface them
	// alongside the resolution row directly instead.
	if source == string(domain.AnswerSourceResolver) {
		source = "technical resolver (cross-provider)"
	}
	// P3-C: an answer AO decided for itself says so, in words, to the worker
	// that has to act on it. The bare enum would tell it as little as it tells
	// a person, and a worker resuming on a decision nobody made needs to know
	// which authority stands behind it.
	if source == string(domain.AnswerSourceAutonomous) {
		source = "AO's own decision, taken automatically under this run's autonomy policy"
	}
	return fmt.Sprintf("Decision:\n%s\n\nSource: %s\n\nContinue the current task using this decision.", q.AnswerText, source)
}
