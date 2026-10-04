package controllers_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// sessions_long_task_test.go — the Task half of the long-specification work.
//
// The workflow create path was already raised to
// domain.MaxWorkflowObjectiveBytes; the Task path was not, so a specification
// pasted into the new-task composer came back as TASK_TOO_LONG at 4096 bytes.
// These tests pin the property that actually matters: a real multi-thousand-
// word brief reaches the service BYTE FOR BYTE. The failure being removed was
// a refusal, and the failure that must never replace it is silent truncation,
// so every accept case compares digests rather than lengths.

// rbacSpecification is the shape of the brief that provoked this work: a full
// RBAC specification with sections, blank lines, lists, non-ASCII and fenced
// blocks, grown to roughly approxBytes.
func rbacSpecification(t *testing.T, approxBytes int) string {
	t.Helper()
	block := strings.Join([]string{
		"OBJETIVO",
		"",
		"Implementar control de acceso basado en roles (RBAC) en MEDUSA.",
		"",
		"ALCANCE",
		"",
		"- Modelo de roles, permisos y asignaciones",
		"- Middleware de autorización en cada ruta protegida",
		"- Migración de datos para usuarios existentes",
		"",
		"RESTRICCIONES",
		"",
		"```sql",
		"-- Añadir siempre migración nueva; nunca editar una ya fusionada",
		"CREATE TABLE role_assignments (...);",
		"```",
		"",
		"CRITERIOS DE ACEPTACIÓN",
		"",
		"1. Un usuario sin rol no accede a ninguna ruta protegida",
		"2. La comprobación de permisos está cubierta por pruebas",
		"3. La migración es reversible",
		"",
		"NO HACER",
		"",
		"No introducir un proveedor de identidad externo.",
		"",
	}, "\n")
	var b strings.Builder
	for b.Len() < approxBytes {
		b.WriteString(block)
	}
	return b.String()
}

func specDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func delegateBody(t *testing.T, brief string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"projectId": "ao", "brief": brief})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

// TestDelegateAcceptsALongSpecificationVerbatim is the headline: the brief the
// composer could not send before now arrives unaltered.
func TestDelegateAcceptsALongSpecificationVerbatim(t *testing.T) {
	for _, size := range []int{4 << 10, 16 << 10, 64 << 10, domain.MaxWorkflowObjectiveBytes - 1024} {
		spec := rbacSpecification(t, size)
		svc := newFakeSessionService()
		srv := newSessionTestServer(t, svc)

		body, status, _ := doRequest(t, srv, "POST", "/api/v1/orchestrators/delegate", delegateBody(t, spec))
		if status != http.StatusAccepted {
			t.Fatalf("%d-byte specification: status=%d body=%s", len(spec), status, body)
		}
		got := svc.delegationInput.Brief
		if got != spec {
			t.Fatalf("%d-byte specification was altered: digest in %s, out %s (in %d bytes, out %d bytes)",
				len(spec), specDigest(spec), specDigest(got), len(spec), len(got))
		}
		if !strings.Contains(got, "\n\n") {
			t.Fatalf("%d-byte specification lost its blank lines", len(spec))
		}
		if !strings.Contains(got, "CRITERIOS DE ACEPTACIÓN") {
			t.Fatalf("%d-byte specification lost its multibyte content", len(spec))
		}
		if !strings.Contains(got, "```sql") {
			t.Fatalf("%d-byte specification lost its markdown fence", len(spec))
		}
	}
}

// TestDelegateAtTheLimitIsAccepted pins the boundary from below, so a later
// change that quietly tightens the limit fails here.
func TestDelegateAtTheLimitIsAccepted(t *testing.T) {
	spec := strings.Repeat("a", domain.MaxWorkflowObjectiveBytes)
	svc := newFakeSessionService()
	srv := newSessionTestServer(t, svc)

	body, status, _ := doRequest(t, srv, "POST", "/api/v1/orchestrators/delegate", delegateBody(t, spec))
	if status != http.StatusAccepted {
		t.Fatalf("a specification exactly at the limit was refused: status=%d body=%s", status, body)
	}
	if len(svc.delegationInput.Brief) != domain.MaxWorkflowObjectiveBytes {
		t.Fatalf("brief arrived as %d bytes, want %d",
			len(svc.delegationInput.Brief), domain.MaxWorkflowObjectiveBytes)
	}
}

// TestDelegateOverTheLimitIsRefusedWithBothSizes is the other boundary, and
// the one that must never truncate: no worker is spawned, and the refusal
// names what was sent and what is allowed.
func TestDelegateOverTheLimitIsRefusedWithBothSizes(t *testing.T) {
	spec := strings.Repeat("a", domain.MaxWorkflowObjectiveBytes+1)
	svc := newFakeSessionService()
	srv := newSessionTestServer(t, svc)

	body, status, _ := doRequest(t, srv, "POST", "/api/v1/orchestrators/delegate", delegateBody(t, spec))
	assertErrorCode(t, body, status, http.StatusBadRequest, "TASK_TOO_LONG")
	if svc.delegationInput.Brief != "" {
		t.Fatalf("a worker was delegated from an over-long specification (%d bytes)", len(svc.delegationInput.Brief))
	}
	for _, want := range []string{"131073", "131072"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("the refusal does not name %q: %s", want, body)
		}
	}
}

// TestDelegateCountsBytesNotRunes is why the limit is stated in bytes: a
// Spanish or Japanese specification costs more per character, and a
// rune-counted limit would let it through at up to three times the bytes the
// request body and the provider payload actually carry.
func TestDelegateCountsBytesNotRunes(t *testing.T) {
	unit := "日" // three bytes per rune
	spec := strings.Repeat(unit, domain.MaxWorkflowObjectiveBytes/len(unit)+1)
	if utf8.RuneCountInString(spec) >= domain.MaxWorkflowObjectiveBytes {
		t.Fatalf("fixture is not the case under test: %d runes", utf8.RuneCountInString(spec))
	}
	svc := newFakeSessionService()
	srv := newSessionTestServer(t, svc)

	body, status, _ := doRequest(t, srv, "POST", "/api/v1/orchestrators/delegate", delegateBody(t, spec))
	assertErrorCode(t, body, status, http.StatusBadRequest, "TASK_TOO_LONG")
}

// TestDelegateAcceptsAMarkdownSpecificationAttachment covers the escape hatch
// for a specification past even the raised ceiling: it travels as a file, and
// the daemon accepts it as Markdown alongside the brief that points at it.
func TestDelegateAcceptsAMarkdownSpecificationAttachment(t *testing.T) {
	svc := newFakeSessionService()
	srv := newSessionTestServer(t, svc)

	payload, err := json.Marshal(map[string]any{
		"projectId": "ao",
		"brief":     "La especificación completa está adjunta. Léela antes de empezar.",
		"attachments": []map[string]any{
			{"mimeType": "text/markdown", "data": "IyBFc3BlY2lmaWNhY2nDs24K"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, status, _ := doRequest(t, srv, "POST", "/api/v1/orchestrators/delegate", string(payload))
	if status != http.StatusAccepted {
		t.Fatalf("markdown specification attachment refused: status=%d body=%s", status, body)
	}
	if len(svc.delegationInput.Attachments) != 1 {
		t.Fatalf("attachments = %#v, want one", svc.delegationInput.Attachments)
	}
	got := svc.delegationInput.Attachments[0]
	if got.Ext != ".md" {
		t.Fatalf("markdown attachment stored as %q, want .md", got.Ext)
	}
	if string(got.Data) != "# Especificación\n" {
		t.Fatalf("attachment bytes = %q, want the decoded markdown", got.Data)
	}
}

// TestDelegateStillRefusesAnUnsupportedAttachmentType is the guard on the
// previous test: widening what a brief may carry must not widen what a file
// may be. SVG is XML that can carry active content and stays blocked.
func TestDelegateStillRefusesAnUnsupportedAttachmentType(t *testing.T) {
	svc := newFakeSessionService()
	srv := newSessionTestServer(t, svc)

	body, status, _ := doRequest(t, srv, "POST", "/api/v1/orchestrators/delegate",
		`{"projectId":"ao","brief":"spec attached","attachments":[{"mimeType":"image/svg+xml","data":"PHN2Zy8+"}]}`)
	assertErrorCode(t, body, status, http.StatusBadRequest, "UNSUPPORTED_ATTACHMENT_TYPE")
	if len(svc.delegationInput.Attachments) != 0 {
		t.Fatal("a blocked attachment type still reached the service")
	}
}

// TestSpawnAcceptsALongSpecificationVerbatim covers the other entry point:
// `ao spawn --prompt` and the LAN/mobile spawn route share the ceiling, and
// the response's promptBytes is the daemon's own measurement of what it
// received -- so an equal count is proof nothing was trimmed on the way in.
func TestSpawnAcceptsALongSpecificationVerbatim(t *testing.T) {
	spec := rbacSpecification(t, 64<<10)
	svc := newFakeSessionService()
	srv := newSessionTestServer(t, svc)

	payload, err := json.Marshal(map[string]any{"projectId": "ao", "kind": "worker", "prompt": spec})
	if err != nil {
		t.Fatal(err)
	}
	body, status, _ := doRequest(t, srv, "POST", "/api/v1/sessions", string(payload))
	if status != http.StatusCreated {
		t.Fatalf("%d-byte prompt: status=%d body=%s", len(spec), status, body)
	}
	var got struct {
		PromptBytes int `json:"promptBytes"`
	}
	mustJSON(t, body, &got)
	if got.PromptBytes != len(spec) {
		t.Fatalf("prompt arrived as %d bytes, want %d — it was altered in transit",
			got.PromptBytes, len(spec))
	}
}

// TestSpawnOverTheLimitIsRefused keeps the two entry points honest about the
// same ceiling: a prompt one byte over is refused, under its own code.
func TestSpawnOverTheLimitIsRefused(t *testing.T) {
	svc := newFakeSessionService()
	srv := newSessionTestServer(t, svc)

	payload, err := json.Marshal(map[string]any{
		"projectId": "ao",
		"prompt":    strings.Repeat("a", domain.MaxWorkflowObjectiveBytes+1),
	})
	if err != nil {
		t.Fatal(err)
	}
	body, status, _ := doRequest(t, srv, "POST", "/api/v1/sessions", string(payload))
	assertErrorCode(t, body, status, http.StatusBadRequest, "PROMPT_TOO_LONG")
	if len(svc.sessions) != 1 {
		t.Fatalf("an over-long prompt still created a session: %#v", svc.sessions)
	}
}

// TestSendKeepsItsOwnSmallerLimit is the guard on the whole change: raising
// what a TASK may be must not raise what a follow-up TURN may be.
//
// A task's specification is a document -- written once, delivered at spawn,
// bounded at domain.MaxWorkflowObjectiveBytes. A message to a session that is
// already running is a turn in a conversation, and it keeps the 4096-byte cap
// it has always had: a specification pasted turn by turn into a live agent is
// exactly the shape the attachment route exists to replace. So a message the
// task path would now accept is still refused here, under MESSAGE_TOO_LONG.
func TestSendKeepsItsOwnSmallerLimit(t *testing.T) {
	svc := newFakeSessionService()
	srv := newSessionTestServer(t, svc)

	// Comfortably inside the raised task ceiling and comfortably past the
	// message cap: if the two constants were ever collapsed into one, this is
	// the request that would start being accepted.
	message := strings.Repeat("a", 4096+1)
	if len(message) >= domain.MaxWorkflowObjectiveBytes {
		t.Fatalf("fixture is not the case under test: %d bytes", len(message))
	}
	payload, err := json.Marshal(map[string]any{"message": message})
	if err != nil {
		t.Fatal(err)
	}

	body, status, _ := doRequest(t, srv, "POST", "/api/v1/sessions/ao-1/send", string(payload))
	assertErrorCode(t, body, status, http.StatusBadRequest, "MESSAGE_TOO_LONG")
	if svc.sent != "" {
		t.Fatalf("an over-long follow-up still reached the session: %q", svc.sent)
	}

	// And the ordinary turn still goes through untouched.
	ok, status, _ := doRequest(t, srv, "POST", "/api/v1/sessions/ao-1/send", `{"message":"sigue con el punto 3"}`)
	if status != http.StatusOK {
		t.Fatalf("a normal follow-up was refused: status=%d body=%s", status, ok)
	}
	if svc.sent != "sigue con el punto 3" {
		t.Fatalf("the follow-up arrived as %q", svc.sent)
	}
}
