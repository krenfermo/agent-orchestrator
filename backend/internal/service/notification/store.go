package notification

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Store is the notification service's read persistence surface.
type Store interface {
	ListNotifications(
		ctx context.Context,
		status ListStatus,
		beforeCreatedAt time.Time,
		beforeID string,
		limit int,
	) ([]domain.NotificationRecord, error)
	CountUnreadNotifications(ctx context.Context) (int64, error)
	CountUnresolvedNotifications(ctx context.Context) (int64, error)
	// The acknowledgement time is passed in rather than taken from the store's
	// own clock so read_at and status are written from one source of truth and
	// tests can pin it.
	MarkNotificationRead(ctx context.Context, id string, at time.Time) (domain.NotificationRecord, bool, error)
	MarkAllNotificationsRead(ctx context.Context, at time.Time) (int64, error)
	MarkNotificationsRead(ctx context.Context, ids []string, at time.Time) (int64, error)

	// P4-C organization-scoped reads. Each requires a non-empty project set:
	// the store returns nothing for an empty one rather than everything.
	ListNotificationsInProjects(
		ctx context.Context,
		status ListStatus,
		beforeCreatedAt time.Time,
		beforeID string,
		limit int,
		projects []domain.ProjectID,
	) ([]domain.NotificationRecord, error)
	CountUnreadNotificationsInProjects(ctx context.Context, projects []domain.ProjectID) (int64, error)
	CountUnresolvedNotificationsInProjects(ctx context.Context, projects []domain.ProjectID) (int64, error)
	MarkAllNotificationsReadInProjects(ctx context.Context, at time.Time, projects []domain.ProjectID) (int64, error)
	GetNotificationProject(ctx context.Context, id string) (domain.ProjectID, bool, error)
}
