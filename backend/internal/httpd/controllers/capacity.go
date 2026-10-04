package controllers

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

// CapacityService is the controller-facing capacity read contract
// (Checkpoint 8J §8/17).
type CapacityService interface {
	List(ctx context.Context) ([]domain.CapacitySnapshot, error)
}

// CapacityController owns the read-only /capacity route.
type CapacityController struct {
	Svc CapacityService
}

// Register mounts capacity routes on the supplied router.
func (c *CapacityController) Register(r chi.Router) {
	r.Get("/capacity", c.list)
}

func (c *CapacityController) list(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/capacity")
		return
	}
	items, err := c.Svc.List(r.Context())
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	out := make([]CapacitySnapshotResponse, 0, len(items))
	for _, item := range items {
		out = append(out, capacitySnapshotResponse(item))
	}
	envelope.WriteJSON(w, http.StatusOK, ListCapacityResponse{Capacity: out})
}

// CapacitySnapshotResponse is one harness's capacity snapshot on the wire.
// DetectedAt/ResetAt are null, never a fabricated timestamp, when 8H never
// recorded one.
type CapacitySnapshotResponse struct {
	Provider   string  `json:"provider,omitempty"`
	Harness    string  `json:"harness"`
	Model      *string `json:"model,omitempty"`
	State      string  `json:"state" enum:"available,limited,cooldown,unavailable,unknown"`
	DetectedAt *string `json:"detectedAt"`
	ResetAt    *string `json:"resetAt"`
	Reason     string  `json:"reason,omitempty"`
	Certainty  string  `json:"certainty" enum:"actual,inferred,unknown"`
}

// ListCapacityResponse is the body of GET /api/v1/capacity.
type ListCapacityResponse struct {
	Capacity []CapacitySnapshotResponse `json:"capacity"`
}

func capacitySnapshotResponse(s domain.CapacitySnapshot) CapacitySnapshotResponse {
	out := CapacitySnapshotResponse{
		Provider: s.Provider, Harness: string(s.Harness), Model: s.Model,
		State: string(s.State), Reason: s.Reason, Certainty: string(s.Certainty),
	}
	if s.DetectedAt != nil {
		v := s.DetectedAt.Format(rfc3339Milli)
		out.DetectedAt = &v
	}
	if s.ResetAt != nil {
		v := s.ResetAt.Format(rfc3339Milli)
		out.ResetAt = &v
	}
	return out
}
