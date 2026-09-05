package httpd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/cdc"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
)

const (
	eventsReplayBatch = 512
	eventsLiveBuffer  = 1024
	eventAfterHeader  = "X-AO-Event-After"
)

type cdcSubscriber interface {
	Subscribe(func(cdc.Event)) (unsubscribe func())
}

// EventsController owns the client-facing CDC stream. Durable replay comes from
// change_log through Source; Broadcaster remains a live-only pub/sub seam.
//
// P4-B left this route ungated as one of the loopback desktop surfaces. P4-C
// closes it, because an unfiltered CDC stream is the most complete leak an
// installation has: every session created, every PR updated, every check
// recorded, in every organization, live. Every change_log row carries a
// project_id, so the filter is the same question the rest of the API already
// asks -- "may this caller read this project" -- applied per event.
type EventsController struct {
	Source cdc.Source
	Live   cdcSubscriber
	// Guard is P4-C's authorization gate. A zero Guard streams everything,
	// which is what every pre-P4-C wiring did and what the headless and test
	// configurations still expect.
	Guard controllers.Guard
}

// Register mounts the CDC SSE stream route.
func (c *EventsController) Register(r chi.Router) {
	r.Get("/events", c.stream)
}

func (c *EventsController) stream(w http.ResponseWriter, r *http.Request) {
	if c.Source == nil || c.Live == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/events")
		return
	}

	after, err := parseEventsAfter(r)
	if err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_AFTER",
			"after must be a non-negative integer", nil)
		return
	}
	latestSeq, err := c.Source.LatestSeq(r.Context())
	if err != nil {
		envelope.WriteAPIError(w, r, http.StatusInternalServerError, "internal", "EVENT_CURSOR_FAILED",
			"Could not inspect the event cursor", nil)
		return
	}
	if after > latestSeq {
		after = 0
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		envelope.WriteAPIError(w, r, http.StatusInternalServerError, "internal", "SSE_UNSUPPORTED",
			"Streaming is not supported by this server", nil)
		return
	}

	// Resolve the caller's reach ONCE, before the stream opens. A stream is
	// long-lived, so re-resolving per event would turn one authorization
	// question into thousands; resolving it here means a permission change
	// takes effect on the client's next reconnect, which is the same contract
	// every other long-lived surface in AO has.
	visible, ok := c.visibility(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	live := make(chan cdc.Event, eventsLiveBuffer)
	unsubscribe := c.Live.Subscribe(func(e cdc.Event) {
		select {
		case live <- e:
		default:
			// Never block the broadcaster. Closing the stream is safer than
			// silently dropping a live event; the client replays on reconnect.
			cancel()
		}
	})
	defer unsubscribe()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	h.Set(eventAfterHeader, strconv.FormatInt(after, 10))
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sentSeq := after
	if err := c.replay(ctx, w, flusher, &sentSeq, visible); err != nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case e := <-live:
			if err := writeSSEEvent(w, flusher, e, &sentSeq, visible); err != nil {
				return
			}
		}
	}
}

// visibility resolves the per-event filter for one stream. A nil filter means
// "send everything": authorization is not wired, or the caller's authority
// spans every organization. ok is false when a response has already been
// written.
func (c *EventsController) visibility(w http.ResponseWriter, r *http.Request) (func(domain.ProjectID) bool, bool) {
	if !c.Guard.Enabled() {
		return nil, true
	}
	sub, ok := c.Guard.Subject(r)
	if !ok {
		envelope.WriteError(w, r, identity.Unauthorized())
		return nil, false
	}
	if sub.CrossTenant {
		return nil, true
	}
	return sub.CanSeeProject, true
}

func (c *EventsController) replay(
	ctx context.Context,
	w http.ResponseWriter,
	flusher http.Flusher,
	sentSeq *int64,
	visible func(domain.ProjectID) bool,
) error {
	for {
		events, err := c.Source.EventsAfter(ctx, *sentSeq, eventsReplayBatch)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}
		for _, e := range events {
			if err := writeSSEEvent(w, flusher, e, sentSeq, visible); err != nil {
				return err
			}
		}
		if len(events) < eventsReplayBatch {
			return nil
		}
	}
}

func parseEventsAfter(r *http.Request) (int64, error) {
	raw := r.URL.Query().Get("after")
	if raw == "" {
		raw = r.Header.Get("Last-Event-ID")
	}
	if raw == "" {
		return 0, nil
	}
	seq, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seq < 0 {
		return 0, fmt.Errorf("invalid after: %q", raw)
	}
	return seq, nil
}

func writeSSEEvent(
	w http.ResponseWriter,
	flusher http.Flusher,
	e cdc.Event,
	sentSeq *int64,
	visible func(domain.ProjectID) bool,
) error {
	if e.Seq <= *sentSeq {
		return nil
	}
	if visible != nil && !visible(domain.ProjectID(e.ProjectID)) {
		// Advance the cursor without writing. Skipping the event but NOT the
		// sequence number would make replay ask for the same batch forever,
		// and would also tell the client, by the gap in ids it never receives,
		// exactly how much is happening where it cannot look.
		*sentSeq = e.Seq
		return nil
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, sseEventName(e.Type), data); err != nil {
		return err
	}
	*sentSeq = e.Seq
	flusher.Flush()
	return nil
}

func sseEventName(t cdc.EventType) string {
	return strings.NewReplacer("\r", "_", "\n", "_").Replace(string(t))
}
