package controllers_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// workflow_long_objective_test.go — P2-E B6, end to end over HTTP.
//
// The unit tests prove the limit; these prove the CREATE PATH honours it. The
// property under test is that a long, multi-line, multibyte specification
// reaches the service byte-for-byte -- same digest in as out -- because the
// failure this change removes was silent alteration, not rejection.

// longTaskSpecification is the shape a real Task brief has: sections, blank
// lines, a list, non-ASCII, and a fenced block.
func longTaskSpecification(t *testing.T, approxBytes int) string {
	t.Helper()
	block := strings.Join([]string{
		"OBJETIVO",
		"",
		"Añadir un índice de documentación al repositorio.",
		"",
		"ALCANCE",
		"",
		"- Sólo `docs/`",
		"- No modificar código de aplicación",
		"",
		"RESTRICCIONES",
		"",
		"```",
		"no inventar documentos",
		"```",
		"",
		"CRITERIOS DE ACEPTACIÓN",
		"",
		"1. El archivo existe",
		"2. Enlaza únicamente documentos reales",
		"",
		"NO HACER",
		"",
		"No convertir esto en un plan multi-tarea.",
		"",
	}, "\n")
	var b strings.Builder
	for b.Len() < approxBytes {
		b.WriteString(block)
	}
	return b.String()
}

func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestWorkflowCreateAcceptsALongSpecificationVerbatim is the headline: a real
// multi-thousand-character brief survives the round trip unchanged.
func TestWorkflowCreateAcceptsALongSpecificationVerbatim(t *testing.T) {
	for _, size := range []int{4 << 10, 10 << 10, 50 << 10} {
		spec := longTaskSpecification(t, size)
		svc := &fakeWorkflowService{}
		srv := newWorkflowTestServer(t, svc)

		payload, err := json.Marshal(map[string]any{"objective": spec, "strategy": "task"})
		if err != nil {
			t.Fatal(err)
		}
		body, status, _ := doRequest(t, srv, "POST", "/api/v1/projects/proj-1/workflows", string(payload))
		if status != http.StatusCreated {
			t.Fatalf("%d-byte specification: status=%d body=%s", len(spec), status, body)
		}
		want := strings.TrimSpace(spec)
		if svc.createdObjective != want {
			t.Fatalf("%d-byte specification was altered: digest in %s, out %s",
				len(spec), digestOf(want), digestOf(svc.createdObjective))
		}
		if !strings.Contains(svc.createdObjective, "\n\n") {
			t.Fatalf("%d-byte specification lost its blank lines", len(spec))
		}
		if !strings.Contains(svc.createdObjective, "CRITERIOS DE ACEPTACIÓN") {
			t.Fatalf("%d-byte specification lost its multibyte content", len(spec))
		}
		if !strings.Contains(svc.createdObjective, "```") {
			t.Fatalf("%d-byte specification lost its markdown fence", len(spec))
		}
	}
}

// TestWorkflowCreateAtTheLimitIsAccepted pins the boundary from below, so a
// later change that tightens the limit without saying so fails here.
func TestWorkflowCreateAtTheLimitIsAccepted(t *testing.T) {
	spec := strings.Repeat("a", domain.MaxWorkflowObjectiveBytes)
	svc := &fakeWorkflowService{}
	srv := newWorkflowTestServer(t, svc)
	payload, _ := json.Marshal(map[string]any{"objective": spec})
	body, status, _ := doRequest(t, srv, "POST", "/api/v1/projects/proj-1/workflows", string(payload))
	if status != http.StatusCreated {
		t.Fatalf("a specification exactly at the limit was refused: status=%d body=%s", status, body)
	}
	if len(svc.createdObjective) != domain.MaxWorkflowObjectiveBytes {
		t.Fatalf("objective arrived as %d bytes, want %d",
			len(svc.createdObjective), domain.MaxWorkflowObjectiveBytes)
	}
}

// TestWorkflowCreateOverTheLimitIsRefusedWithBothSizes is the other boundary,
// and the one that must never truncate: no run is created, and the message
// names what was sent and what is allowed.
func TestWorkflowCreateOverTheLimitIsRefusedWithBothSizes(t *testing.T) {
	spec := strings.Repeat("a", domain.MaxWorkflowObjectiveBytes+1)
	svc := &fakeWorkflowService{}
	srv := newWorkflowTestServer(t, svc)
	payload, _ := json.Marshal(map[string]any{"objective": spec})
	body, status, _ := doRequest(t, srv, "POST", "/api/v1/projects/proj-1/workflows", string(payload))

	if status != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400: body=%s", status, body)
	}
	if svc.createdObjective != "" {
		t.Fatalf("a run was created from an over-long specification (%d bytes)", len(svc.createdObjective))
	}
	for _, want := range []string{"OBJECTIVE_TOO_LONG", "131072"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("the refusal does not name %q: %s", want, body)
		}
	}
}

// TestWorkflowCreateRejectsAnOversizedBodyWithoutBuffering guards the abuse
// case: a body far past the limit is stopped by MaxBytesReader rather than
// read into memory to find out how big it is.
func TestWorkflowCreateRejectsAnOversizedBodyWithoutBuffering(t *testing.T) {
	spec := strings.Repeat("a", domain.MaxWorkflowObjectiveBytes+128<<10)
	svc := &fakeWorkflowService{}
	srv := newWorkflowTestServer(t, svc)
	payload, _ := json.Marshal(map[string]any{"objective": spec})
	body, status, _ := doRequest(t, srv, "POST", "/api/v1/projects/proj-1/workflows", string(payload))

	if status != http.StatusRequestEntityTooLarge && status != http.StatusBadRequest {
		t.Fatalf("status=%d, want 413 or 400: body=%s", status, body)
	}
	if svc.createdObjective != "" {
		t.Fatal("an oversized body still created a run")
	}
}
