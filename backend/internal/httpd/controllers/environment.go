package controllers

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	environmentsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/environment"
)

// EnvironmentStatusProvider is the controller-facing contract for the Setup
// UX Settings surface (Codex/Claude/GitHub/Projects readiness).
type EnvironmentStatusProvider interface {
	Status(ctx context.Context) (environmentsvc.Status, error)
	TestGitHub(ctx context.Context) (environmentsvc.GitHubStatus, error)
}

// EnvironmentController owns the /environment routes.
type EnvironmentController struct {
	Svc EnvironmentStatusProvider
}

// Register mounts the environment routes on the supplied router.
func (c *EnvironmentController) Register(r chi.Router) {
	r.Get("/environment/status", c.status)
	r.Post("/environment/github/test", c.testGitHub)
}

func (c *EnvironmentController) status(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/environment/status")
		return
	}
	status, err := c.Svc.Status(r.Context())
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, status)
}

func (c *EnvironmentController) testGitHub(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/environment/github/test")
		return
	}
	status, err := c.Svc.TestGitHub(r.Context())
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, status)
}
