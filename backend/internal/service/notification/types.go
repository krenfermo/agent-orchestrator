// Package notification exposes read-only notification DTOs for REST controllers.
package notification

import "github.com/aoagents/agent-orchestrator/backend/internal/domain"

// TargetKind describes what a dashboard should navigate to for a notification.
type TargetKind string

const (
	// TargetSession navigates to a session detail view.
	TargetSession TargetKind = "session"
	// TargetPR navigates to a pull request view.
	TargetPR TargetKind = "pr"
	// TargetWorkflow navigates to a workflow run view. Used by run-level
	// notifications, which have no session to open.
	TargetWorkflow TargetKind = "workflow"
)

// Target is the service-facing navigation metadata for a notification.
type Target struct {
	Kind          TargetKind
	SessionID     domain.SessionID
	PRURL         string
	WorkflowRunID string
}

// Notification is the dashboard-facing service DTO assembled from a stored row.
type Notification struct {
	domain.NotificationRecord
	Target Target
}

// ListStatus selects which stored notifications are returned.
type ListStatus = domain.NotificationListStatus

const (
	// ListUnread returns only notifications that still need acknowledgement.
	ListUnread = domain.NotificationListUnread
	// ListAll returns both read and unread notifications.
	ListAll = domain.NotificationListAll
	// ListUnresolved returns notifications whose underlying issue is still open.
	ListUnresolved = domain.NotificationListUnresolved
)

// ProjectScope limits a read to notifications about a set of projects.
//
// The nil/empty distinction is the whole point of the type, and it is the
// opposite of what a bare slice would give you:
//
//   - a nil *ProjectScope means "do not scope" -- the caller's authority spans
//     every organization, which is what an installation owner or administrator
//     has and what every pre-P4-C wiring has;
//   - a non-nil scope with NO project ids means "nothing is visible", and must
//     return an empty page rather than everything.
//
// A plain []domain.ProjectID cannot express that difference: nil and empty are
// the same value, and the safe reading of one is the dangerous reading of the
// other.
type ProjectScope struct {
	ProjectIDs []domain.ProjectID
}

// ListFilter controls paginated notification history.
type ListFilter struct {
	Status ListStatus
	Limit  int
	Cursor string
	// Scope limits the page to the caller's visible projects (P4-C). Nil
	// means unscoped -- see ProjectScope.
	Scope *ProjectScope
}

// ListPage is one newest-first page of notification history.
type ListPage struct {
	Notifications []Notification
	NextCursor    string
	// UnreadCount is the unseen badge count; UnresolvedCount is how many issues
	// are still open. They are independent: a notification the user has already
	// seen stays counted as unresolved until AO closes it.
	UnreadCount     int
	UnresolvedCount int
}
