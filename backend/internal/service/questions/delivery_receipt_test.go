package questions

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// P3-D smoke B: the answer AO recorded as delivered and the worker never got.
//
// A provider's select dialog is MODAL. Text written into a session showing one
// never reaches the composer — and the write still succeeds, because writing to
// a terminal always does. This path marked the question delivered on exactly
// that success, so a correct, well-evidenced autonomous decision was consumed
// without ever reaching the agent, and the run sat blocked on a prompt whose
// answer was already in the database and already marked handed over.
//
// The refusal was supposed to come from sessionguard, which declines a write
// into a session reading `blocked`. That covers a permission prompt and nothing
// else: a provider's own select dialog reports `waiting_input`, so the guard
// admitted the write. The whole needs-input family has to be excluded, and the
// receipt has to be something other than "the send call returned nil".

type fakeDeliveryStore struct {
	pending []domain.WorkflowQuestion
	marked  []string
}

func (f *fakeDeliveryStore) ListUndeliveredAnsweredWorkflowQuestions(
	_ context.Context, _ string,
) ([]domain.WorkflowQuestion, error) {
	return f.pending, nil
}

func (f *fakeDeliveryStore) MarkWorkflowQuestionDelivered(_ context.Context, id string, _ time.Time) (bool, error) {
	f.marked = append(f.marked, id)
	return true, nil
}

type fakeDeliverySender struct{ sent []domain.SessionID }

func (f *fakeDeliverySender) Send(
	_ context.Context, id domain.SessionID, _ string, _ *ports.SpawnAttachment,
) error {
	f.sent = append(f.sent, id)
	return nil
}

// fakeInputState answers for one session, and can also model the session AO
// could not read at all — which must never be treated as "no dialog".
type fakeInputState struct {
	awaiting bool
	known    bool
}

func (f fakeInputState) AwaitingInput(_ context.Context, _ domain.SessionID) (bool, bool) {
	return f.awaiting, f.known
}

func answeredQuestion(sessionID string) domain.WorkflowQuestion {
	sid := domain.SessionID(sessionID)
	return domain.WorkflowQuestion{
		ID:            domain.WorkflowQuestionID("wfq-1"),
		WorkflowRunID: "wf-1",
		SessionID:     &sid,
		State:         domain.QuestionStateAnswered,
		AnswerText:    "String concatenation",
	}
}

func TestAnAnswerIsNotDeliveredIntoASessionSittingOnAPrompt(t *testing.T) {
	store := &fakeDeliveryStore{pending: []domain.WorkflowQuestion{answeredQuestion("sess-blocked")}}
	sender := &fakeDeliverySender{}

	n, err := DeliverAnsweredWithState(context.Background(), store, sender,
		fakeInputState{awaiting: true, known: true}, "wf-1", time.Now().UTC())
	if err != nil {
		t.Fatalf("DeliverAnsweredWithState: %v", err)
	}
	if n != 0 {
		t.Fatalf("delivered %d answers into a session showing a prompt, want 0", n)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("text was written into a session showing a modal prompt: %v", sender.sent)
	}
	// And, above all, the answer is still OWED. A row marked delivered is one
	// no later sweep and no structured delivery will ever pick up again.
	if len(store.marked) != 0 {
		t.Fatalf("the answer was recorded as delivered without reaching anyone: %v", store.marked)
	}
}

func TestAnAnswerIsDeliveredWhenTheSessionHasAComposer(t *testing.T) {
	store := &fakeDeliveryStore{pending: []domain.WorkflowQuestion{answeredQuestion("sess-idle")}}
	sender := &fakeDeliverySender{}

	n, err := DeliverAnsweredWithState(context.Background(), store, sender,
		fakeInputState{awaiting: false, known: true}, "wf-1", time.Now().UTC())
	if err != nil {
		t.Fatalf("DeliverAnsweredWithState: %v", err)
	}
	if n != 1 || len(sender.sent) != 1 || len(store.marked) != 1 {
		t.Fatalf("an ordinary delivery was blocked: n=%d sent=%v marked=%v", n, sender.sent, store.marked)
	}
}

// Cannot-tell is not a refusal, here as everywhere else in AO: an unreadable
// session keeps the behaviour this path always had rather than grounding every
// delivery on a probe that did not answer.
func TestAnUnreadableSessionDoesNotGroundDelivery(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state SessionInputState
	}{
		{"the probe could not read the session", fakeInputState{known: false}},
		{"no probe is wired at all", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeDeliveryStore{pending: []domain.WorkflowQuestion{answeredQuestion("sess-unknown")}}
			sender := &fakeDeliverySender{}
			n, err := DeliverAnsweredWithState(context.Background(), store, sender, tc.state, "wf-1", time.Now().UTC())
			if err != nil {
				t.Fatalf("DeliverAnsweredWithState: %v", err)
			}
			if n != 1 {
				t.Fatalf("an unreadable session stopped a delivery: n=%d", n)
			}
		})
	}
}
