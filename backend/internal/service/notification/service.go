package notification

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

const (
	// DefaultListLimit keeps the first dashboard history page bounded.
	DefaultListLimit = 100
	// MaxListLimit keeps every notification history page bounded.
	MaxListLimit = 100
)

// Manager reads stored notifications for REST controllers.
type Manager struct {
	store Store
	clock func() time.Time
}

// Deps configures a Manager.
type Deps struct {
	Store Store
	// Clock stamps read_at on acknowledgement. Injectable for deterministic
	// tests; defaults to the wall clock in UTC.
	Clock func() time.Time
}

// New constructs a read-only notification Manager.
func New(d Deps) *Manager {
	clock := d.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &Manager{store: d.Store, clock: clock}
}

// UnreadCount returns just the unread badge count. It exists so a client
// polling the badge does not have to fetch a page of history to learn one
// number (P4-D section 16).
func (m *Manager) UnreadCount(ctx context.Context) (int, error) {
	return m.UnreadCountInScope(ctx, nil)
}

// UnreadCountInScope is UnreadCount limited to the caller's visible projects.
// A nil scope is the unscoped count, which is what UnreadCount asks for.
func (m *Manager) UnreadCountInScope(ctx context.Context, scope *ProjectScope) (int, error) {
	if m == nil || m.store == nil {
		return 0, errors.New("notification: store is required")
	}
	var (
		count int64
		err   error
	)
	if scope == nil {
		count, err = m.store.CountUnreadNotifications(ctx)
	} else {
		count, err = m.store.CountUnreadNotificationsInProjects(ctx, scope.ProjectIDs)
	}
	if err != nil {
		return 0, err
	}
	return int(count), nil
}

// ProjectFor reports which project a notification is about, so an
// acknowledgement route can refuse one in a project the caller cannot see
// BEFORE marking it read. Doing it after would leave a foreign row modified by
// a request that was about to be told the row does not exist.
func (m *Manager) ProjectFor(ctx context.Context, id string) (domain.ProjectID, bool, error) {
	if m == nil || m.store == nil {
		return "", false, errors.New("notification: store is required")
	}
	return m.store.GetNotificationProject(ctx, id)
}

// List returns one stable newest-first page of notification history.
func (m *Manager) List(ctx context.Context, filter ListFilter) (ListPage, error) {
	if m == nil || m.store == nil {
		return ListPage{}, errors.New("notification: store is required")
	}
	if filter.Status == "" {
		filter.Status = ListUnread
	}
	if !filter.Status.Valid() {
		return ListPage{}, apierr.Invalid(
			"INVALID_NOTIFICATION_STATUS",
			"Notification status must be unread, unresolved, or all",
			nil,
		)
	}
	limit := normalizeLimit(filter.Limit)
	beforeCreatedAt, beforeID, err := decodeCursor(filter.Cursor)
	if err != nil {
		return ListPage{}, err
	}
	var (
		rows            []domain.NotificationRecord
		unreadCount     int64
		unresolvedCount int64
	)
	if filter.Scope == nil {
		rows, err = m.store.ListNotifications(ctx, filter.Status, beforeCreatedAt, beforeID, limit+1)
		if err != nil {
			return ListPage{}, err
		}
		if unreadCount, err = m.store.CountUnreadNotifications(ctx); err != nil {
			return ListPage{}, err
		}
		if unresolvedCount, err = m.store.CountUnresolvedNotifications(ctx); err != nil {
			return ListPage{}, err
		}
	} else {
		// The counts are read through the same scope as the rows on purpose. A
		// badge that counts what the list refuses to show is a bug report
		// waiting to be filed, and an unscoped count would also disclose HOW
		// MUCH is happening in organizations the caller cannot see.
		projects := filter.Scope.ProjectIDs
		rows, err = m.store.ListNotificationsInProjects(ctx, filter.Status, beforeCreatedAt, beforeID, limit+1, projects)
		if err != nil {
			return ListPage{}, err
		}
		if unreadCount, err = m.store.CountUnreadNotificationsInProjects(ctx, projects); err != nil {
			return ListPage{}, err
		}
		if unresolvedCount, err = m.store.CountUnresolvedNotificationsInProjects(ctx, projects); err != nil {
			return ListPage{}, err
		}
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	out := make([]Notification, 0, len(rows))
	for _, row := range rows {
		out = append(out, notificationFromRecord(row))
	}
	page := ListPage{Notifications: out, UnreadCount: int(unreadCount), UnresolvedCount: int(unresolvedCount)}
	if hasMore {
		page.NextCursor = encodeCursor(rows[len(rows)-1])
	}
	return page, nil
}

// MarkRead marks one unread notification read.
func (m *Manager) MarkRead(ctx context.Context, id string) (Notification, bool, error) {
	if m == nil || m.store == nil {
		return Notification{}, false, errors.New("notification: store is required")
	}
	if id == "" {
		return Notification{}, false, apierr.Invalid("INVALID_NOTIFICATION_ID", "Notification id is required", nil)
	}
	row, ok, err := m.store.MarkNotificationRead(ctx, id, m.now())
	if err != nil {
		return Notification{}, false, err
	}
	if !ok {
		return Notification{}, false, apierr.NotFound("NOTIFICATION_NOT_FOUND", "Unknown unread notification")
	}
	return notificationFromRecord(row), true, nil
}

// MarkAllRead acknowledges notifications as seen. With ids it acknowledges
// exactly those rows — the ones a client actually rendered — which keeps
// anything past the client's last loaded page unread and therefore still
// reachable. With no ids it falls back to acknowledging every unread row, for
// clients that do not paginate.
func (m *Manager) MarkAllRead(ctx context.Context, ids []string) (int64, error) {
	return m.MarkAllReadInScope(ctx, ids, nil)
}

// MarkAllReadInScope is MarkAllRead confined to the caller's visible projects.
// The explicit-ids form is filtered before the write rather than after: a
// client that sends an id it should not have must not acknowledge somebody
// else's notification, even though it would never see the result.
func (m *Manager) MarkAllReadInScope(ctx context.Context, ids []string, scope *ProjectScope) (int64, error) {
	if m == nil || m.store == nil {
		return 0, errors.New("notification: store is required")
	}
	at := m.now()
	if scope == nil {
		if len(ids) == 0 {
			return m.store.MarkAllNotificationsRead(ctx, at)
		}
		return m.store.MarkNotificationsRead(ctx, ids, at)
	}
	if len(ids) == 0 {
		return m.store.MarkAllNotificationsReadInProjects(ctx, at, scope.ProjectIDs)
	}
	visible := make(map[domain.ProjectID]bool, len(scope.ProjectIDs))
	for _, id := range scope.ProjectIDs {
		visible[id] = true
	}
	allowed := make([]string, 0, len(ids))
	for _, id := range ids {
		project, ok, err := m.store.GetNotificationProject(ctx, id)
		if err != nil {
			return 0, err
		}
		if !ok || !visible[project] {
			continue
		}
		allowed = append(allowed, id)
	}
	if len(allowed) == 0 {
		return 0, nil
	}
	return m.store.MarkNotificationsRead(ctx, allowed, at)
}

// now is the acknowledgement clock, normalized to UTC so read_at is stored the
// same way created_at is.
func (m *Manager) now() time.Time {
	if m.clock == nil {
		return time.Now().UTC()
	}
	return m.clock().UTC()
}

func normalizeLimit(limit int) int {
	if limit <= 0 {
		return DefaultListLimit
	}
	if limit > MaxListLimit {
		return MaxListLimit
	}
	return limit
}

func encodeCursor(rec domain.NotificationRecord) string {
	value := rec.CreatedAt.UTC().Format(time.RFC3339Nano) + "\n" + rec.ID
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeCursor(raw string) (time.Time, string, error) {
	if raw == "" {
		return time.Time{}, "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return time.Time{}, "", invalidCursor()
	}
	createdAtRaw, id, ok := strings.Cut(string(decoded), "\n")
	if !ok || id == "" {
		return time.Time{}, "", invalidCursor()
	}
	createdAt, err := time.Parse(time.RFC3339Nano, createdAtRaw)
	if err != nil {
		return time.Time{}, "", invalidCursor()
	}
	return createdAt.UTC(), id, nil
}

func invalidCursor() error {
	return apierr.Invalid("INVALID_NOTIFICATION_CURSOR", "Notification cursor is invalid", nil)
}

func notificationFromRecord(rec domain.NotificationRecord) Notification {
	return Notification{NotificationRecord: rec, Target: targetForRecord(rec)}
}

func targetForRecord(rec domain.NotificationRecord) Target {
	if rec.PRURL != "" {
		return Target{Kind: TargetPR, SessionID: rec.SessionID, PRURL: rec.PRURL}
	}
	// A run-level notification has no session to open; naming the run keeps the
	// target honest instead of handing a client an empty session id.
	if rec.SessionID == "" && rec.WorkflowRunID != "" {
		return Target{Kind: TargetWorkflow, WorkflowRunID: rec.WorkflowRunID}
	}
	return Target{Kind: TargetSession, SessionID: rec.SessionID}
}
