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
}
