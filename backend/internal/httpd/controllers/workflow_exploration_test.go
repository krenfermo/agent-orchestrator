package controllers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
)

type fakeExploration struct {
	view domain.RunExploration
	err  error
}

func (f fakeExploration) WorkflowRun(context.Context, string) (domain.RunExploration, error) {
	return f.view, f.err
}

func explorationRequest(t *testing.T, svc controllers.UsageExplorationService, path string) (int, []byte) {
	t.Helper()
	r := chi.NewRouter()
	(&controllers.WorkflowsController{UsageExploration: svc}).Register(r)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.Bytes()
}

func TestWorkflowExplorationRoute(t *testing.T) {
	status, _ := explorationRequest(t, nil, "/workflows/wf-1/exploration")
	if status != http.StatusNotImplemented {
		t.Fatalf("nil service status = %d, want 501", status)
	}
	status, _ = explorationRequest(t, fakeExploration{err: usagesvc.ErrExplorationRunNotFound}, "/workflows/nope/exploration")
	if status != http.StatusNotFound {
		t.Fatalf("unknown run status = %d, want 404", status)
	}

	reads := int64(7)
	view := domain.RunExploration{
		RunID: "wf-1", ProjectID: "p1", Recorded: true,
		Agents: []domain.AgentExploration{{
			Role: domain.WorkflowRoleWorker, Subject: domain.SessionSubject("s1"), Harness: "codex",
			FileReads:  domain.ExplorationMetric{Basis: domain.ExplorationUnavailable, Method: "codex exposes no structured read tool"},
			ToolCalls:  domain.ExplorationMetric{Value: &reads, Basis: domain.ExplorationObserved, Method: "tool calls"},
			PathScopes: map[domain.ToolPathScope]int64{domain.ToolPathSecret: 2},
		}},
	}
	status, body := explorationRequest(t, fakeExploration{view: view}, "/workflows/wf-1/exploration")
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s", status, body)
	}
	var res map[string]any
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}
	agent := res["agents"].([]any)[0].(map[string]any)
	fileReads := agent["fileReads"].(map[string]any)
	if v, present := fileReads["value"]; !present || v != nil || fileReads["basis"] != "unavailable" {
		t.Fatalf("unavailable metric must serialize value:null, got %v", fileReads)
	}
	if agent["toolCalls"].(map[string]any)["value"].(float64) != 7 {
		t.Fatalf("observed metric lost its value: %v", agent["toolCalls"])
	}
	// A metric nobody set is unavailable, never an implicit 0.
	if s := agent["searches"].(map[string]any); s["value"] != nil || s["basis"] != "unavailable" {
		t.Fatalf("zero-valued metric must read as unavailable, got %v", s)
	}
	if !strings.Contains(string(body), `"scope":"secret","count":2`) {
		t.Fatalf("path scopes missing: %s", body)
	}
}
