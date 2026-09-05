package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Completion rows use a permanent, event-scoped dedupe rather than the open-row
// one. "The user already read it" is exactly when the old rule stops protecting
// a terminal fact, and exactly when a restart is most likely to re-observe it.

func completionRecord(id string, session domain.SessionID, project domain.ProjectID, at time.Time) domain.NotificationRecord {
	return domain.NotificationRecord{
		ID:        id,
		SessionID: session,
		ProjectID: project,
		Type:      domain.NotificationTaskCompleted,
		DedupeKey: string(session) + "@" + at.Format(time.RFC3339Nano),
		Title:     "checkout-flow finished",
		Body:      "The task reported that it finished the work it was given.",
		Status:    domain.NotificationUnread,
		CreatedAt: at,
	}
}

func TestNotificationStore_TaskCompletionDedupesOnTheEvent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	sess, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	at := time.Now().UTC().Truncate(time.Second)

	rec := completionRecord("ntf_1", sess.ID, sess.ProjectID, at)
	if _, inserted, err := s.CreateNotification(ctx, rec); err != nil || !inserted {
		t.Fatalf("first insert inserted=%v err=%v", inserted, err)
	}

	dup := rec
	dup.ID = "ntf_2"
	if _, inserted, err := s.CreateNotification(ctx, dup); err != nil || inserted {
		t.Fatalf("same event inserted=%v err=%v, want false nil", inserted, err)
	}

	// The decisive case: once read, the row leaves the OPEN set, so only the
	// permanent event index can still stop a restart from re-announcing it.
	if _, _, err := s.MarkNotificationRead(ctx, "ntf_1", time.Now().UTC()); err != nil {
		t.Fatalf("MarkNotificationRead: %v", err)
	}
	afterRead := rec
	afterRead.ID = "ntf_3"
	if _, inserted, err := s.CreateNotification(ctx, afterRead); err != nil || inserted {
		t.Fatalf("a read completion was re-raised: inserted=%v err=%v", inserted, err)
	}

	rows, err := s.ListNotifications(ctx, domain.NotificationListAll, time.Time{}, "", 10)
	if err != nil {
		t.Fatalf("ListNotifications: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
}

// A later turn is different finished work and gets its own row, even though it
// shares a session and a type with the first.
func TestNotificationStore_ASecondTurnGetsItsOwnRow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	sess, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	first := time.Now().UTC().Truncate(time.Second)

	if _, inserted, err := s.CreateNotification(ctx, completionRecord("ntf_1", sess.ID, sess.ProjectID, first)); err != nil || !inserted {
		t.Fatalf("first insert inserted=%v err=%v", inserted, err)
	}
	second := completionRecord("ntf_2", sess.ID, sess.ProjectID, first.Add(time.Hour))
	if _, inserted, err := s.CreateNotification(ctx, second); err != nil || !inserted {
		t.Fatalf("second turn inserted=%v err=%v, want true nil", inserted, err)
	}

	rows, err := s.ListNotifications(ctx, domain.NotificationListAll, time.Time{}, "", 10)
	if err != nil {
		t.Fatalf("ListNotifications: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
}

// A workflow run is not a session and has none to borrow, so the row is stored
// session-less and anchored to the run.
func TestNotificationStore_WorkflowCompletionIsAnchoredToTheRun(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	now := time.Now().UTC().Truncate(time.Second)

	rec := domain.NotificationRecord{
		ID:            "ntf_wf_1",
		ProjectID:     "mer",
		WorkflowRunID: "wf-1",
		DedupeKey:     "wf-1",
		Type:          domain.NotificationWorkflowCompleted,
		Title:         "ship the thing finished",
		Status:        domain.NotificationUnread,
		CreatedAt:     now,
	}
	created, inserted, err := s.CreateNotification(ctx, rec)
	if err != nil || !inserted {
		t.Fatalf("CreateNotification inserted=%v err=%v", inserted, err)
	}
	if created.SessionID != "" {
		t.Fatalf("SessionID = %q, want empty for a run-level row", created.SessionID)
	}
	if created.WorkflowRunID != "wf-1" {
		t.Fatalf("WorkflowRunID = %q", created.WorkflowRunID)
	}

	dup := rec
	dup.ID = "ntf_wf_2"
	if _, inserted, err := s.CreateNotification(ctx, dup); err != nil || inserted {
		t.Fatalf("the same run completed twice: inserted=%v err=%v", inserted, err)
	}
}

// Two runs finishing are two events, and both deserve a row.
func TestNotificationStore_DifferentRunsEachGetARow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	now := time.Now().UTC().Truncate(time.Second)

	for _, runID := range []string{"wf-1", "wf-2"} {
		rec := domain.NotificationRecord{
			ID: "ntf_" + runID, ProjectID: "mer", WorkflowRunID: runID, DedupeKey: runID,
			Type: domain.NotificationWorkflowCompleted, Title: runID + " finished",
			Status: domain.NotificationUnread, CreatedAt: now,
		}
		if _, inserted, err := s.CreateNotification(ctx, rec); err != nil || !inserted {
			t.Fatalf("%s inserted=%v err=%v", runID, inserted, err)
		}
	}
	rows, err := s.ListNotifications(ctx, domain.NotificationListAll, time.Time{}, "", 10)
	if err != nil {
		t.Fatalf("ListNotifications: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
}

// Completions are terminal facts, so they belong in the unread badge and the
// history but never in the still-actionable unresolved list.
func TestNotificationStore_CompletionsAreNeverUnresolved(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	sess, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if _, _, err := s.CreateNotification(ctx, completionRecord("ntf_1", sess.ID, sess.ProjectID, now)); err != nil {
		t.Fatalf("CreateNotification: %v", err)
	}

	unresolved, err := s.CountUnresolvedNotifications(ctx)
	if err != nil {
		t.Fatalf("CountUnresolvedNotifications: %v", err)
	}
	if unresolved != 0 {
		t.Fatalf("unresolved = %d, want 0", unresolved)
	}
	unread, err := s.CountUnreadNotifications(ctx)
	if err != nil {
		t.Fatalf("CountUnreadNotifications: %v", err)
	}
	if unread != 1 {
		t.Fatalf("unread = %d, want 1", unread)
	}
}

// The four pre-existing types keep their open-row dedupe: a needs-input row the
// user has read and resolved must be raisable again the next time it happens.
func TestNotificationStore_KeylessTypesKeepOpenRowDedupe(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	sess, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	rec := domain.NotificationRecord{
		ID: "ntf_1", SessionID: sess.ID, ProjectID: sess.ProjectID,
		Type: domain.NotificationNeedsInput, Title: "checkout-flow needs input",
		Status: domain.NotificationUnread, CreatedAt: now,
	}
	if _, inserted, err := s.CreateNotification(ctx, rec); err != nil || !inserted {
		t.Fatalf("first insert inserted=%v err=%v", inserted, err)
	}
	if _, err := s.ResolveSessionNotifications(ctx, sess.ID, domain.NotificationNeedsInput, now.Add(time.Minute)); err != nil {
		t.Fatalf("ResolveSessionNotifications: %v", err)
	}
	if _, _, err := s.MarkNotificationRead(ctx, "ntf_1", time.Now().UTC()); err != nil {
		t.Fatalf("MarkNotificationRead: %v", err)
	}

	again := rec
	again.ID = "ntf_2"
	again.CreatedAt = now.Add(2 * time.Minute)
	if _, inserted, err := s.CreateNotification(ctx, again); err != nil || !inserted {
		t.Fatalf("a resolved-and-read needs-input could not be raised again: inserted=%v err=%v", inserted, err)
	}
}
