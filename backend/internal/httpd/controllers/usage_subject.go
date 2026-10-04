package controllers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
)

// usage_subject.go — the loopback callback a runtime pane uses to report its own
// token spend.
//
// It is deliberately the narrowest route in the daemon: it accepts a usage
// subject, a harness and a provider conversation id, and it can do exactly one
// thing — bind that conversation to that subject for metering. It cannot change
// a session's activity, cannot end anything, and cannot be addressed at a
// session at all (a session subject is refused: sessions report through
// /sessions/{id}/activity, which validates a launch id and an activity state
// this route has no business touching).

// UsageSubjectHookRecorder is the narrow write this controller needs.
// *usage.Collector satisfies it.
type UsageSubjectHookRecorder interface {
	RecordSubjectHook(ctx context.Context, signal usagesvc.SubjectHookSignal) error
}

// SubjectUsageHookRequest is one pane's usage callback.
type SubjectUsageHookRequest struct {
	// Subject is "<kind>:<id>" — the durable authority the pane belongs to (a
	// review run, a question resolution), never a process or a pane name.
	Subject string `json:"subject" description:"The usage subject this pane's tokens belong to, as \"<kind>:<id>\"."`
	Agent   string `json:"agent,omitempty" description:"The harness that invoked the hook."`
	Harness string `json:"harness,omitempty" description:"The provider harness whose transcript this is."`
	Event   string `json:"event,omitempty" description:"The native hook event name."`
	// NativeSessionID is the harness's own conversation id, reported by the
	// pane's own hook. Without it nothing is recorded: AO does not adopt a
	// transcript on the strength of it looking recent.
	NativeSessionID string `json:"nativeSessionId,omitempty" description:"The harness's own conversation id, as the pane's hook reported it."`
	TranscriptPath  string `json:"transcriptPath,omitempty" description:"The provider transcript path, when the hook carries one."`
	ModelID         string `json:"modelId,omitempty" description:"The model the pane reported."`
}

// SubjectUsageHookResponse acknowledges the callback.
type SubjectUsageHookResponse struct {
	OK bool `json:"ok"`
}

// maxSubjectUsageHookBytes bounds the callback body. It carries ids and one
// path; anything larger is not a usage report.
const maxSubjectUsageHookBytes = 8 << 10

// UsageSubjectController owns POST /usage/subject-hook.
type UsageSubjectController struct {
	Usage UsageSubjectHookRecorder
}

// Register mounts the route.
func (c *UsageSubjectController) Register(r chi.Router) {
	r.Post("/usage/subject-hook", c.record)
}

func (c *UsageSubjectController) record(w http.ResponseWriter, r *http.Request) {
	var in SubjectUsageHookRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSubjectUsageHookBytes)).Decode(&in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "invalid", "INVALID_BODY", "malformed request body", nil)
		return
	}
	subject, ok := domain.ParseUsageSubject(in.Subject)
	if !ok {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "invalid", "INVALID_USAGE_SUBJECT",
			"subject must be \"<kind>:<id>\" with a known kind", nil)
		return
	}
	if subject.Kind == domain.UsageSubjectSession {
		// A session reports through its own activity route, which checks a
		// launch id and an activity state. Accepting one here would be a second,
		// weaker door into the same ledger.
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "invalid", "INVALID_USAGE_SUBJECT",
			"a session subject must report through the session activity route", nil)
		return
	}
	if c.Usage == nil {
		envelope.WriteJSON(w, http.StatusOK, SubjectUsageHookResponse{OK: true})
		return
	}

	harness := strings.TrimSpace(in.Harness)
	if harness == "" {
		harness = strings.TrimSpace(in.Agent)
	}
	signal := usagesvc.SubjectHookSignal{
		Subject:         subject,
		Harness:         domain.AgentHarness(capActivityMeta(domain.SanitizeControlChars(harness))),
		Event:           capActivityMeta(domain.SanitizeControlChars(in.Event)),
		NativeSessionID: capActivityMeta(domain.SanitizeControlChars(strings.TrimSpace(in.NativeSessionID))),
		ModelID:         capActivityMeta(domain.SanitizeControlChars(strings.TrimSpace(in.ModelID))),
		TranscriptPath:  capUsagePath(domain.SanitizeControlChars(strings.TrimSpace(in.TranscriptPath))),
	}
	if err := c.Usage.RecordSubjectHook(r.Context(), signal); err != nil {
		// Best-effort, exactly like the session usage hook: a usage report that
		// fails must never break the pane that sent it.
		slog.Default().Warn("subject usage hook processing failed",
			"subject", subject.String(), "event", signal.Event, "err", err)
	}
	envelope.WriteJSON(w, http.StatusOK, SubjectUsageHookResponse{OK: true})
}
