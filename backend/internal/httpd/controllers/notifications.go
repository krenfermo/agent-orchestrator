package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
	notificationsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/notification"
)

// NotificationService is the controller-facing notification service contract.
type NotificationService interface {
	UnreadCount(ctx context.Context) (int, error)
	List(ctx context.Context, filter notificationsvc.ListFilter) (notificationsvc.ListPage, error)
	MarkRead(ctx context.Context, id string) (notificationsvc.Notification, bool, error)
	MarkAllRead(ctx context.Context, ids []string) (int64, error)

	// P4-C organization-scoped forms. Every read and acknowledgement on these
	// routes goes through one of them once a Guard is wired.
	UnreadCountInScope(ctx context.Context, scope *notificationsvc.ProjectScope) (int, error)
	MarkAllReadInScope(ctx context.Context, ids []string, scope *notificationsvc.ProjectScope) (int64, error)
	ProjectFor(ctx context.Context, id string) (domain.ProjectID, bool, error)
}

// NotificationStream is the live notification stream used by SSE clients.
type NotificationStream interface {
	Subscribe(projectID domain.ProjectID) (<-chan domain.NotificationEvent, func())
}

// NotificationsController owns the /notifications routes.
//
// P4-B left this family deliberately ungated: it was one of the loopback
// desktop surfaces, and narrowing it needed a scope that did not exist yet.
// P4-C is that scope. A notification is ABOUT a project -- every row carries a
// NOT NULL project_id -- so "which notifications may this caller see" is the
// question "which projects may this caller see", already answered, and the
// answer is now applied here rather than left open.
type NotificationsController struct {
	Svc    NotificationService
	Stream NotificationStream
	// Guard is P4-C's authorization gate. A zero Guard leaves the pre-P4-C
	// behavior exactly as it was, which is what every headless and test
	// wiring with no identity layer depends on.
	Guard Guard
}

// scope resolves the caller's visible-project scope for these routes.
//
// ok is false when a response has already been written. A nil scope means "do
// not scope": either authorization is not wired at all, or the caller's
// authority genuinely spans every organization. Everyone else gets the exact
// set of projects they can read, which for a foreign organization is the empty
// set -- and an empty scope shows nothing rather than everything.
func (c *NotificationsController) scope(w http.ResponseWriter, r *http.Request) (*notificationsvc.ProjectScope, bool) {
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
	visible := make([]domain.ProjectID, 0, len(sub.ProjectRoles))
	for _, id := range sub.AccessibleProjectIDs() {
		if sub.CanSeeProject(id) {
			visible = append(visible, id)
		}
	}
	return &notificationsvc.ProjectScope{ProjectIDs: visible}, true
}

// Register mounts bounded notification REST routes on the supplied router.
func (c *NotificationsController) Register(r chi.Router) {
	r.Get("/notifications", c.list)
	r.Get("/notifications/unread-count", c.unreadCount)
	r.Post("/notifications/read-all", c.markAllRead)
	r.Patch("/notifications/{id}", c.markRead)
}

// RegisterStream mounts long-lived notification stream routes on the supplied router.
func (c *NotificationsController) RegisterStream(r chi.Router) {
	r.Get("/notifications/stream", c.stream)
}

// unreadCount serves just the badge number. It exists so a client polling the
// badge does not have to pull a page of history to learn one integer -- the
// count is served by a recipient-scoped index and reads no notification bodies
// at all (P4-D section 16).
func (c *NotificationsController) unreadCount(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/notifications/unread-count")
		return
	}
	scope, ok := c.scope(w, r)
	if !ok {
		return
	}
	count, err := c.Svc.UnreadCountInScope(r.Context(), scope)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, NotificationUnreadCountResponse{UnreadCount: count})
}

func (c *NotificationsController) list(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/notifications")
		return
	}
	filter, err := parseNotificationListFilter(r)
	if err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_QUERY", err.Error(), nil)
		return
	}
	scope, ok := c.scope(w, r)
	if !ok {
		return
	}
	filter.Scope = scope
	page, err := c.Svc.List(r.Context(), filter)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ListNotificationsResponse{
		Notifications:   notificationResponses(page.Notifications),
		NextCursor:      page.NextCursor,
		UnreadCount:     page.UnreadCount,
		UnresolvedCount: page.UnresolvedCount,
	})
}

func (c *NotificationsController) markRead(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "PATCH", "/api/v1/notifications/{id}")
		return
	}
	var req MarkNotificationReadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	if req.Status != string(domain.NotificationRead) {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_NOTIFICATION_STATUS", "Notification status must be read", nil)
		return
	}
	id := chi.URLParam(r, "id")
	if !c.mayAcknowledge(w, r, id) {
		return
	}
	notification, _, err := c.Svc.MarkRead(r.Context(), id)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, NotificationEnvelope{Notification: notificationResponse(notification)})
}

func (c *NotificationsController) markAllRead(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/notifications/read-all")
		return
	}
	// The body is optional: older clients POST nothing and mean "everything".
	var req MarkAllNotificationsReadRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
			return
		}
	}
	scope, ok := c.scope(w, r)
	if !ok {
		return
	}
	updatedCount, err := c.Svc.MarkAllReadInScope(r.Context(), req.IDs, scope)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, MarkAllNotificationsReadResponse{
		Notifications: []NotificationResponse{},
		UpdatedCount:  updatedCount,
	})
}

// mayAcknowledge reports whether the caller may mark one notification read,
// answering BEFORE the write. A notification about a project the caller cannot
// see reports 404 -- the same answer an id that does not exist gets, for the
// same reason project ids answer 404: the route must not become an oracle for
// "did something happen over there".
func (c *NotificationsController) mayAcknowledge(w http.ResponseWriter, r *http.Request, id string) bool {
	if !c.Guard.Enabled() {
		return true
	}
	sub, ok := c.Guard.Subject(r)
	if !ok {
		envelope.WriteError(w, r, identity.Unauthorized())
		return false
	}
	if sub.CrossTenant {
		return true
	}
	project, found, err := c.Svc.ProjectFor(r.Context(), id)
	if err != nil {
		envelope.WriteError(w, r, err)
		return false
	}
	if !found || !sub.CanSeeProject(project) {
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found",
			"NOTIFICATION_NOT_FOUND", "Unknown unread notification", nil)
		return false
	}
	return true
}

func (c *NotificationsController) stream(w http.ResponseWriter, r *http.Request) {
	if c.Stream == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/notifications/stream")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		envelope.WriteAPIError(w, r, http.StatusInternalServerError, "internal", "SSE_UNSUPPORTED", "Streaming is not supported by this server", nil)
		return
	}
	// Authenticated is not authorized. The subscription is per-project when a
	// projectId is given and installation-wide when it is not, so the filter
	// below is what keeps the wide form from becoming a live feed of every
	// organization's activity.
	var visible func(domain.ProjectID) bool
	if c.Guard.Enabled() {
		sub, ok := c.Guard.Subject(r)
		if !ok {
			envelope.WriteError(w, r, identity.Unauthorized())
			return
		}
		requested := domain.ProjectID(r.URL.Query().Get("projectId"))
		if requested != "" && !sub.CrossTenant && !sub.CanSeeProject(requested) {
			envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found",
				"PROJECT_NOT_FOUND", "project not found", nil)
			return
		}
		if !sub.CrossTenant {
			visible = sub.CanSeeProject
		}
	}

	ch, unsubscribe := c.Stream.Subscribe(domain.ProjectID(r.URL.Query().Get("projectId")))
	defer unsubscribe()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			if visible != nil && !visible(event.Record.ProjectID) {
				continue
			}
			if err := writeNotificationSSE(w, flusher, event); err != nil {
				return
			}
		}
	}
}

func writeNotificationSSE(w http.ResponseWriter, flusher http.Flusher, event domain.NotificationEvent) error {
	data, err := json.Marshal(notificationResponseFromRecord(event.Record))
	if err != nil {
		return err
	}
	name := "notification_created"
	if event.Kind == domain.NotificationResolved {
		name = "notification_resolved"
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func parseNotificationListFilter(r *http.Request) (notificationsvc.ListFilter, error) {
	q := r.URL.Query()
	status := notificationsvc.ListStatus(q.Get("status"))
	if status == "" {
		status = notificationsvc.ListUnread
	}
	if !status.Valid() {
		return notificationsvc.ListFilter{}, errNotificationStatusUnsupported
	}
	limit := notificationsvc.DefaultListLimit
	if raw := q.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			return notificationsvc.ListFilter{}, errNotificationLimitInvalid
		}
		limit = parsed
	}
	if limit > notificationsvc.MaxListLimit {
		limit = notificationsvc.MaxListLimit
	}
	return notificationsvc.ListFilter{Status: status, Limit: limit, Cursor: q.Get("cursor")}, nil
}

var (
	errNotificationStatusUnsupported = notificationQueryError("status must be unread, unresolved, or all")
	errNotificationLimitInvalid      = notificationQueryError("limit must be a positive integer")
)

type notificationQueryError string

func (e notificationQueryError) Error() string { return string(e) }

func notificationResponses(in []notificationsvc.Notification) []NotificationResponse {
	out := make([]NotificationResponse, 0, len(in))
	for _, n := range in {
		out = append(out, notificationResponse(n))
	}
	return out
}

func notificationResponse(n notificationsvc.Notification) NotificationResponse {
	return NotificationResponse{
		ID:            n.ID,
		SessionID:     string(n.SessionID),
		ProjectID:     string(n.ProjectID),
		PRURL:         n.PRURL,
		WorkflowRunID: n.WorkflowRunID,
		TaskID:        n.TaskID,
		Type:          string(n.Type),
		Title:         n.Title,
		Body:          n.Body,
		Status:        string(n.Status),
		Severity:      string(n.Severity),
		CreatedAt:     n.CreatedAt,
		ReadAt:        optionalTime(n.ReadAt),
		ResolvedAt:    optionalTime(n.ResolvedAt),
		Source:        string(n.Source),
		Target: NotificationTarget{
			Kind:          string(n.Target.Kind),
			SessionID:     string(n.Target.SessionID),
			PRURL:         n.Target.PRURL,
			WorkflowRunID: n.Target.WorkflowRunID,
		},
	}
}

func notificationResponseFromRecord(rec domain.NotificationRecord) NotificationResponse {
	return NotificationResponse{
		ID:            rec.ID,
		SessionID:     string(rec.SessionID),
		ProjectID:     string(rec.ProjectID),
		PRURL:         rec.PRURL,
		WorkflowRunID: rec.WorkflowRunID,
		TaskID:        rec.TaskID,
		Type:          string(rec.Type),
		Title:         rec.Title,
		Body:          rec.Body,
		Status:        string(rec.Status),
		Severity:      string(rec.Severity),
		CreatedAt:     rec.CreatedAt,
		ReadAt:        optionalTime(rec.ReadAt),
		ResolvedAt:    optionalTime(rec.ResolvedAt),
		Source:        string(rec.Source),
		Target:        notificationTargetFromRecord(rec),
	}
}

func notificationTargetFromRecord(rec domain.NotificationRecord) NotificationTarget {
	if rec.PRURL != "" {
		return NotificationTarget{Kind: "pr", SessionID: string(rec.SessionID), PRURL: rec.PRURL}
	}
	// A run-level notification has no session to open; naming the run keeps the
	// target honest instead of handing a client an empty session id.
	if rec.SessionID == "" && rec.WorkflowRunID != "" {
		return NotificationTarget{Kind: "workflow", WorkflowRunID: rec.WorkflowRunID}
	}
	return NotificationTarget{Kind: "session", SessionID: string(rec.SessionID)}
}
