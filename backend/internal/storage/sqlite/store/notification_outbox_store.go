package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// EnqueueNotificationEmail records that one notification owes an email.
//
// Idempotent on the notification id, which is the primary key: enqueueing the
// same notification twice inserts nothing and reports enqueued=false. That is
// what makes the outbox inherit the notification's own permanent event dedupe
// rather than needing a second one.
func (s *Store) EnqueueNotificationEmail(ctx context.Context, entry domain.EmailOutboxEntry) (bool, error) {
	if entry.NotificationID == "" {
		return false, errors.New("enqueue notification email: notification id is required")
	}
	if entry.MaxAttempts <= 0 {
		return false, errors.New("enqueue notification email: max attempts must be positive")
	}
	recipient := entry.Recipient
	if recipient == "" {
		recipient = domain.NotificationRecipientLocal
	}
	next := entry.NextAttemptAt
	if next.IsZero() {
		next = entry.CreatedAt
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.EnqueueNotificationEmail(ctx, gen.EnqueueNotificationEmailParams{
		NotificationID: entry.NotificationID,
		Recipient:      recipient,
		Subject:        entry.Subject,
		Body:           entry.Body,
		MaxAttempts:    int64(entry.MaxAttempts),
		NextAttemptAt:  next.UTC(),
		CreatedAt:      entry.CreatedAt.UTC(),
	})
	if err != nil {
		return false, fmt.Errorf("enqueue notification email %s: %w", entry.NotificationID, err)
	}
	return rows > 0, nil
}

// GetNotificationEmail reads one outbox entry.
func (s *Store) GetNotificationEmail(ctx context.Context, notificationID string) (domain.EmailOutboxEntry, bool, error) {
	row, err := s.qr.GetNotificationEmailOutboxEntry(ctx, notificationID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.EmailOutboxEntry{}, false, nil
	}
	if err != nil {
		return domain.EmailOutboxEntry{}, false, fmt.Errorf("get notification email %s: %w", notificationID, err)
	}
	return emailOutboxFromGen(row), true, nil
}

// ListDueNotificationEmails returns entries eligible to send now, oldest first.
func (s *Store) ListDueNotificationEmails(ctx context.Context, now time.Time, limit int) ([]domain.EmailOutboxEntry, error) {
	if limit <= 0 {
		limit = 1
	}
	rows, err := s.qr.ListDueNotificationEmails(ctx, gen.ListDueNotificationEmailsParams{
		Now:       now.UTC(),
		PageLimit: int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("list due notification emails: %w", err)
	}
	out := make([]domain.EmailOutboxEntry, 0, len(rows))
	for _, row := range rows {
		out = append(out, emailOutboxFromGen(row))
	}
	return out, nil
}

// ClaimNotificationEmail takes ownership of one due entry, moving it to
// 'sending' and spending an attempt. It reports claimed=false when another
// worker got there first or the entry stopped being due, which is what makes
// two workers on the same batch safe.
func (s *Store) ClaimNotificationEmail(
	ctx context.Context,
	notificationID string,
	now, leaseExpiresAt time.Time,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.ClaimNotificationEmail(ctx, gen.ClaimNotificationEmailParams{
		NotificationID: notificationID,
		Now:            now.UTC(),
		LeaseExpiresAt: nullTime(leaseExpiresAt),
	})
	if err != nil {
		return false, fmt.Errorf("claim notification email %s: %w", notificationID, err)
	}
	return rows > 0, nil
}

// MarkNotificationEmailSent completes a claimed entry.
func (s *Store) MarkNotificationEmailSent(ctx context.Context, notificationID string, now time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.qw.MarkNotificationEmailSent(ctx, gen.MarkNotificationEmailSentParams{
		NotificationID: notificationID,
		Now:            now.UTC(),
	}); err != nil {
		return fmt.Errorf("mark notification email sent %s: %w", notificationID, err)
	}
	return nil
}

// MarkNotificationEmailRetry parks a claimed entry until its backoff deadline.
func (s *Store) MarkNotificationEmailRetry(
	ctx context.Context,
	notificationID, lastError string,
	now, nextAttemptAt time.Time,
) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.qw.MarkNotificationEmailRetry(ctx, gen.MarkNotificationEmailRetryParams{
		NotificationID: notificationID,
		LastError:      truncateOutboxError(lastError),
		NextAttemptAt:  nextAttemptAt.UTC(),
		Now:            now.UTC(),
	}); err != nil {
		return fmt.Errorf("mark notification email retry %s: %w", notificationID, err)
	}
	return nil
}

// MarkNotificationEmailDead gives up on a claimed entry, permanently.
func (s *Store) MarkNotificationEmailDead(
	ctx context.Context,
	notificationID, lastError string,
	now time.Time,
) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.qw.MarkNotificationEmailDead(ctx, gen.MarkNotificationEmailDeadParams{
		NotificationID: notificationID,
		LastError:      truncateOutboxError(lastError),
		Now:            now.UTC(),
	}); err != nil {
		return fmt.Errorf("mark notification email dead %s: %w", notificationID, err)
	}
	return nil
}

// ReclaimExpiredNotificationEmailLeases returns entries whose sending daemon
// died to the retry state. Called on startup and on every worker pass, this is
// what makes a crash mid-send converge rather than strand the email.
func (s *Store) ReclaimExpiredNotificationEmailLeases(ctx context.Context, now time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.ReclaimExpiredEmailOutboxLeases(ctx, now.UTC())
	if err != nil {
		return 0, fmt.Errorf("reclaim expired notification email leases: %w", err)
	}
	return rows, nil
}

// CountNotificationEmailsByState reports how many entries sit in one state.
func (s *Store) CountNotificationEmailsByState(ctx context.Context, state domain.EmailOutboxState) (int64, error) {
	count, err := s.qr.CountNotificationEmailsByState(ctx, string(state))
	if err != nil {
		return 0, fmt.Errorf("count notification emails in %s: %w", state, err)
	}
	return count, nil
}

// outboxErrorLimit bounds what a failure writes back to the row. The column is
// for an operator reading "why is this stuck", not for a provider transcript.
const outboxErrorLimit = 500

func truncateOutboxError(msg string) string {
	msg = domain.SanitizeControlChars(msg)
	if len(msg) > outboxErrorLimit {
		return msg[:outboxErrorLimit] + "..."
	}
	return msg
}

func emailOutboxFromGen(row gen.NotificationEmailOutbox) domain.EmailOutboxEntry {
	return domain.EmailOutboxEntry{
		NotificationID: row.NotificationID,
		Recipient:      row.Recipient,
		State:          domain.EmailOutboxState(row.State),
		Subject:        row.Subject,
		Body:           row.Body,
		AttemptCount:   int(row.AttemptCount),
		MaxAttempts:    int(row.MaxAttempts),
		NextAttemptAt:  row.NextAttemptAt,
		LeaseExpiresAt: timeFromNull(row.LeaseExpiresAt),
		LastError:      row.LastError,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
		CompletedAt:    timeFromNull(row.CompletedAt),
	}
}
