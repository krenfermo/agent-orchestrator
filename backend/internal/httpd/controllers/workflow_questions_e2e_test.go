package controllers_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/questions"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// fakeE2ESender is a hand-rolled fake for questions.MessageSender: the
// production Send *interface* is exercised for real (AnswerService calls
// it through the same code path session_manager.Manager.Send would be
// wired into — see daemon/daemon.go's `Sender: rawSessionMgr`), but the
// actual tmux write is faked here rather than driving a real terminal
// process, matching this codebase's existing fakeMessageSender pattern for
// workflow dispatch tests (no network/process calls in this package's
// tests, per AGENTS.md).
type fakeE2ESender struct {
	calls   int
	lastID  domain.SessionID
	lastMsg string
}

func (f *fakeE2ESender) Send(_ context.Context, id domain.SessionID, message string, _ *ports.SpawnAttachment) error {
	f.calls++
	f.lastID = id
	f.lastMsg = message
	return nil
}

// TestWorkflowQuestions_E2E_HumanRequired_RealRouterRealStoreRealDelivery
// is Checkpoint 8K-A's E2E B: a genuinely unspecified functional question
// is captured as human_required, then answered through the REAL HTTP
// router (httptest against httpd.NewRouterWithControl, not a direct
// service call) backed by a REAL *questions.AnswerService over a REAL
// SQLite store — only the final tmux/session write is faked (see
// fakeE2ESender above). Asserts the answer is delivered exactly once via
// the real Send interface.
func TestWorkflowQuestions_E2E_HumanRequired_RealRouterRealStoreRealDelivery(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	sessionID := domain.SessionID("sess-e2e-1")

	// Simulates what the real detector would have persisted after parsing
	// a captured pane for a genuinely ambiguous, unspecified business
	// question ("should the cooldown be 2s or 8s") — human_required,
	// classification=ambiguous, per Classify's documented behavior.
	saved, _, err := store.InsertWorkflowQuestion(ctx, domain.WorkflowQuestion{
		ID:             "q-e2e-1",
		WorkflowRunID:  "wf-e2e-1",
		SessionID:      &sessionID,
		Fingerprint:    "fp-e2e-1",
		QuestionText:   "Should the retry cooldown be 2s or 8s?",
		Certainty:      domain.QuestionCertaintyInferred,
		Classification: domain.QuestionClassificationAmbiguous,
		State:          domain.QuestionStateHumanRequired,
		CreatedAt:      now,
	})
	if err != nil {
		t.Fatalf("seed question: %v", err)
	}

	sender := &fakeE2ESender{}
	svc := &questions.AnswerService{Store: store, Runs: store, Sender: sender, Clock: func() time.Time { return now }}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{Questions: svc}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)

	body, status, headers := doRequest(t, srv, "POST", "/api/v1/workflows/wf-e2e-1/questions/"+string(saved.ID)+"/answer", `{"customText":"Use 8 seconds."}`)
	assertJSON(t, headers)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}

	var out struct {
		Question struct {
			State      string `json:"state"`
			AnswerText string `json:"answerText"`
			Delivered  bool   `json:"delivered"`
		} `json:"question"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	if out.Question.State != "answered" || out.Question.AnswerText != "Use 8 seconds." {
		t.Fatalf("unexpected question in response: %+v", out.Question)
	}
	if !out.Question.Delivered {
		t.Fatalf("expected delivered=true in the HTTP response")
	}

	if sender.calls != 1 {
		t.Fatalf("real Send interface calls = %d, want exactly 1", sender.calls)
	}
	if sender.lastID != sessionID {
		t.Fatalf("delivered to session %v, want %v", sender.lastID, sessionID)
	}

	// Confirm the persisted row (not just the HTTP response) also reflects
	// exactly-once delivery, and that a second identical HTTP request is
	// correctly rejected as a double-answer rather than double-delivering.
	persisted, ok, err := store.GetWorkflowQuestion(ctx, string(saved.ID))
	if err != nil || !ok {
		t.Fatalf("GetWorkflowQuestion: ok=%v err=%v", ok, err)
	}
	if !persisted.Delivered || persisted.State != domain.QuestionStateAnswered {
		t.Fatalf("persisted question = %+v, want delivered+answered", persisted)
	}

	body2, status2, _ := doRequest(t, srv, "POST", "/api/v1/workflows/wf-e2e-1/questions/"+string(saved.ID)+"/answer", `{"customText":"second attempt"}`)
	if status2 != http.StatusConflict {
		t.Fatalf("second answer status=%d body=%s, want 409", status2, body2)
	}
	if sender.calls != 1 {
		t.Fatalf("Send calls after rejected double-answer = %d, want still 1", sender.calls)
	}
}
