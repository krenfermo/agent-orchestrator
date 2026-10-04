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
	"github.com/aoagents/agent-orchestrator/backend/internal/service/questions"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// TestDecisions_E2E_RealRouterRealStoreValidResolveDelivers is Checkpoint
// 8K-B pass 2's controller/service-level E2E test: a real
// workflow_question_resolutions row (as dispatchDecisionResolver would have
// minted) is resolved through the REAL HTTP router
// (httptest against httpd.NewRouterWithControl) backed by a REAL
// *questions.ResolverAnswerService over a REAL SQLite store.
func TestDecisions_E2E_RealRouterRealStoreValidResolveDelivers(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	resolverSessionID := domain.SessionID("decision-resolver-wqr-e2e-1")

	saved, err := store.InsertWorkflowQuestionResolution(ctx, domain.WorkflowQuestionResolution{
		ID:                 "wqr-e2e-1",
		WorkflowQuestionID: "q-e2e-1",
		WorkflowRunID:      "wf-e2e-1",
		ResolverHarness:    domain.HarnessCodex,
		ResolverSessionID:  &resolverSessionID,
		Status:             domain.ResolutionStatusRunning,
		CreatedAt:          now,
		UpdatedAt:          now,
	})
	if err != nil {
		t.Fatalf("seed resolution: %v", err)
	}

	svc := &questions.ResolverAnswerService{Store: store, Clock: func() time.Time { return now }}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{Decisions: svc}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)

	path := "/api/v1/sessions/" + string(resolverSessionID) + "/decisions/resolve"
	reqBody := `{"runId":"` + string(saved.ID) + `","answer":"use pkg/foo.Bar","reasonSummary":"only one exists","certainty":"actual","evidenceReferences":["pkg/foo/bar.go"]}`

	body, status, headers := doRequest(t, srv, "POST", path, reqBody)
	assertJSON(t, headers)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var out struct {
		Resolution struct {
			Status string `json:"status"`
			Answer string `json:"answer"`
		} `json:"resolution"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	if out.Resolution.Status != "complete" || out.Resolution.Answer != "use pkg/foo.Bar" {
		t.Fatalf("unexpected resolution in response: %+v", out.Resolution)
	}

	persisted, ok, err := store.GetWorkflowQuestionResolution(ctx, string(saved.ID))
	if err != nil || !ok {
		t.Fatalf("GetWorkflowQuestionResolution: ok=%v err=%v", ok, err)
	}
	if persisted.Status != domain.ResolutionStatusComplete || persisted.Answer != "use pkg/foo.Bar" {
		t.Fatalf("persisted resolution = %+v", persisted)
	}

	// Duplicate identical resubmit: idempotent 200, no error.
	body2, status2, _ := doRequest(t, srv, "POST", path, reqBody)
	if status2 != http.StatusOK {
		t.Fatalf("duplicate identical status=%d body=%s, want 200", status2, body2)
	}

	// Duplicate differing resubmit: 409-shaped.
	differing := `{"runId":"` + string(saved.ID) + `","answer":"different answer","certainty":"actual"}`
	body3, status3, _ := doRequest(t, srv, "POST", path, differing)
	if status3 != http.StatusConflict {
		t.Fatalf("duplicate differing status=%d body=%s, want 409", status3, body3)
	}
}

// TestDecisions_E2E_WrongSessionAndWrongRunRejected covers the two
// cross-identity rejection paths through the real router.
func TestDecisions_E2E_WrongSessionAndWrongRunRejected(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	resolverSessionID := domain.SessionID("decision-resolver-wqr-e2e-2")

	saved, err := store.InsertWorkflowQuestionResolution(ctx, domain.WorkflowQuestionResolution{
		ID:                 "wqr-e2e-2",
		WorkflowQuestionID: "q-e2e-2",
		WorkflowRunID:      "wf-e2e-2",
		ResolverHarness:    domain.HarnessClaudeCode,
		ResolverSessionID:  &resolverSessionID,
		Status:             domain.ResolutionStatusRunning,
		CreatedAt:          now,
		UpdatedAt:          now,
	})
	if err != nil {
		t.Fatalf("seed resolution: %v", err)
	}

	svc := &questions.ResolverAnswerService{Store: store, Clock: func() time.Time { return now }}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{Decisions: svc}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)

	// Wrong session: hits the resolution via a different session id.
	wrongSessionPath := "/api/v1/sessions/some-other-session/decisions/resolve"
	body, status, _ := doRequest(t, srv, "POST", wrongSessionPath, `{"runId":"`+string(saved.ID)+`","answer":"x","certainty":"actual"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("wrong-session status=%d body=%s, want 400", status, body)
	}

	// Wrong run: correct session, bogus run id.
	rightSessionPath := "/api/v1/sessions/" + string(resolverSessionID) + "/decisions/resolve"
	body2, status2, _ := doRequest(t, srv, "POST", rightSessionPath, `{"runId":"wqr-does-not-exist","answer":"x","certainty":"actual"}`)
	if status2 != http.StatusNotFound {
		t.Fatalf("wrong-run status=%d body=%s, want 404", status2, body2)
	}
}
