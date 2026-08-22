package domain

import (
	"errors"
	"time"
)

// NotificationType identifies a user-facing notification kind persisted for the dashboard.
type NotificationType string

const (
	// NotificationNeedsInput means an agent session is waiting for user input.
	NotificationNeedsInput NotificationType = "needs_input"
	// NotificationReadyToMerge means a PR has no known merge blockers.
	NotificationReadyToMerge NotificationType = "ready_to_merge"
	// NotificationPRMerged means a tracked PR was merged.
	NotificationPRMerged NotificationType = "pr_merged"
	// NotificationPRClosedUnmerged means a tracked PR closed without merging.
	NotificationPRClosedUnmerged NotificationType = "pr_closed_unmerged"
	// NotificationTaskCompleted means an ordinary task session reported that it
	// finished the work it was given. It is raised from the durable completion
	// receipt (SessionRecord.TurnCompletedAt) rather than from any generic idle
	// or stop observation — see lifecycle.turnSucceeded.
	NotificationTaskCompleted NotificationType = "task_completed"
	// NotificationWorkflowCompleted means a workflow run reached the terminal
	// WorkflowRunCompleted state. Workflow terminal states have no outgoing
	// transitions, so that state change happens exactly once per run.
	NotificationWorkflowCompleted NotificationType = "workflow_completed"
	// NotificationTaskNeedsAttention means a planned task's run stopped on
	// something only a person can resolve. It is raised from the same durable
	// record that makes the stop explainable in the UI (the attention
	// checkpoint), never from a transient observation.
	NotificationTaskNeedsAttention NotificationType = "task_needs_attention"
	// NotificationWorkflowNeedsAttention is NotificationTaskNeedsAttention for a
	// workflow run that is not a planned task's child.
	NotificationWorkflowNeedsAttention NotificationType = "workflow_needs_attention"
	// NotificationTaskFailed means a planned task's run reached the terminal
	// WorkflowRunFailed state: it ended, and it did not do the work.
	NotificationTaskFailed NotificationType = "task_failed"
	// NotificationWorkflowFailed is NotificationTaskFailed for a workflow run
	// that is not a planned task's child.
	NotificationWorkflowFailed NotificationType = "workflow_failed"
)

// Valid reports whether t is one of the v1 notification kinds.
func (t NotificationType) Valid() bool {
	switch t {
	case NotificationNeedsInput, NotificationReadyToMerge, NotificationPRMerged, NotificationPRClosedUnmerged,
		NotificationTaskCompleted, NotificationWorkflowCompleted,
		NotificationTaskNeedsAttention, NotificationWorkflowNeedsAttention,
		NotificationTaskFailed, NotificationWorkflowFailed:
		return true
	default:
		return false
	}
}

// NeedsResolution reports whether t describes an issue that stays open until
// something changes (an agent waiting on the user, a PR waiting on a merge).
// Terminal facts — a PR that merged or closed — describe something that already
// happened, so they are surfaced once as unseen and never held as unresolved.
func (t NotificationType) NeedsResolution() bool {
	return t == NotificationNeedsInput || t == NotificationReadyToMerge
}

// Completion reports whether t announces that a unit of work finished
// successfully.
func (t NotificationType) Completion() bool {
	return t == NotificationTaskCompleted || t == NotificationWorkflowCompleted
}

// Attention reports whether t announces that a unit of work stopped on
// something only a person can resolve.
func (t NotificationType) Attention() bool {
	return t == NotificationTaskNeedsAttention || t == NotificationWorkflowNeedsAttention
}

// Failure reports whether t announces that a unit of work ended without doing
// the work.
func (t NotificationType) Failure() bool {
	return t == NotificationTaskFailed || t == NotificationWorkflowFailed
}

// EventKeyed reports whether t names a one-off EVENT rather than a condition
// that comes and goes.
//
// The three event families all share the same two consequences, which is why
// they share one predicate: each row MUST carry a DedupeKey (enrich rejects one
// that does not), and that key is deduped permanently rather than only while
// the row is open. Without it a daemon restart, a retry, or a second observer
// re-announces something the user already saw — a finished run, a stop they
// already read, a failure they already handled.
func (t NotificationType) EventKeyed() bool {
	return t.Completion() || t.Attention() || t.Failure()
}

// EmailEvent is the granularity at which a user chooses what AO may email
// about. It is deliberately coarser than NotificationType: nobody wants to
// answer "tasks yes, workflows no" three times, and the two halves of each pair
// always mean the same thing to the person reading the mail.
type EmailEvent string

const (
	// EmailEventCompleted covers task_completed and workflow_completed.
	EmailEventCompleted EmailEvent = "completed"
	// EmailEventNeedsAttention covers task_needs_attention and
	// workflow_needs_attention.
	EmailEventNeedsAttention EmailEvent = "needs_attention"
	// EmailEventFailed covers task_failed and workflow_failed.
	EmailEventFailed EmailEvent = "failed"
)

// EmailEventOf reports which email event a notification type belongs to, and
// false for the families that have no email fan-out at all.
func (t NotificationType) EmailEventOf() (EmailEvent, bool) {
	switch {
	case t.Completion():
		return EmailEventCompleted, true
	case t.Attention():
		return EmailEventNeedsAttention, true
	case t.Failure():
		return EmailEventFailed, true
	default:
		return "", false
	}
}

// NotificationStatus is the seen state for a stored notification. The stored
// values remain "unread"/"read" for wire and schema compatibility; "unread"
// means the user has not opened the notification panel since it arrived.
type NotificationStatus string

const (
	// NotificationUnread marks a notification that has not been acknowledged.
	NotificationUnread NotificationStatus = "unread"
	// NotificationRead marks a notification that has been acknowledged.
	NotificationRead NotificationStatus = "read"
)

// Valid reports whether s is a supported notification read state.
func (s NotificationStatus) Valid() bool {
	switch s {
	case NotificationUnread, NotificationRead:
		return true
	default:
		return false
	}
}

// NotificationListStatus selects which stored notifications are returned.
type NotificationListStatus string

const (
	// NotificationListUnread returns only notifications that still need acknowledgement.
	NotificationListUnread NotificationListStatus = "unread"
	// NotificationListAll returns both read and unread notifications.
	NotificationListAll NotificationListStatus = "all"
	// NotificationListUnresolved returns notifications whose underlying issue is
	// still open, regardless of whether the user has already seen them.
	NotificationListUnresolved NotificationListStatus = "unresolved"
)

// Valid reports whether s is a supported notification list filter.
func (s NotificationListStatus) Valid() bool {
	switch s {
	case NotificationListUnread, NotificationListAll, NotificationListUnresolved:
		return true
	default:
		return false
	}
}

// NotificationRecord is the durable notification persistence shape.
type NotificationRecord struct {
	ID        string
	SessionID SessionID
	ProjectID ProjectID
	PRURL     string
	// WorkflowRunID scopes a run-level notification (workflow_completed) that has
	// no session behind it. Empty for every session-scoped notification.
	WorkflowRunID string
	// DedupeKey names the one real-world EVENT this row reports, so the same
	// event can never produce a second row — not on a retry, not after a daemon
	// restart, not when two callers observe the same transition. Unlike the
	// open-row dedupe index (which only holds while a row is unseen or
	// unresolved), this is permanent, which is what a terminal "it finished"
	// fact needs. Empty keeps the pre-existing open-row dedupe behavior.
	DedupeKey string
	Type      NotificationType
	Title     string
	Body      string
	Status    NotificationStatus
	CreatedAt time.Time
	// ResolvedAt is when the underlying issue went away — the session received
	// its input, or the PR stopped waiting on a merge. Zero means still open.
	// Only AO writes it; there is no user-facing "resolve" action.
	ResolvedAt time.Time
}

// Resolved reports whether the issue behind this notification is closed.
func (r NotificationRecord) Resolved() bool { return !r.ResolvedAt.IsZero() }

// NotificationEventKind distinguishes the live notification stream events.
type NotificationEventKind string

const (
	// NotificationCreated announces a newly persisted notification.
	NotificationCreated NotificationEventKind = "created"
	// NotificationResolved announces that a stored notification's underlying
	// issue went away, so open dashboards can drop it from the unresolved list.
	NotificationResolved NotificationEventKind = "resolved"
)

// NotificationEvent is one live notification-stream message.
type NotificationEvent struct {
	Kind   NotificationEventKind
	Record NotificationRecord
}

var (
	// ErrInvalidNotificationType reports an unknown notification type.
	ErrInvalidNotificationType = errors.New("invalid notification type")
	// ErrInvalidNotificationStatus reports an unknown notification status.
	ErrInvalidNotificationStatus = errors.New("invalid notification status")
	// ErrInvalidNotificationRecord reports a missing required notification field.
	ErrInvalidNotificationRecord = errors.New("invalid notification record")
)

// Validate checks the required fields and enum values for a stored notification.
func (r NotificationRecord) Validate() error {
	if r.ProjectID == "" || r.Title == "" || r.CreatedAt.IsZero() {
		return ErrInvalidNotificationRecord
	}
	// A notification is anchored to either a session or a workflow run. A
	// workflow run is not a session and has no session to borrow, so requiring
	// one here would have forced run-level notifications to invent an owner.
	if r.SessionID == "" && r.WorkflowRunID == "" {
		return ErrInvalidNotificationRecord
	}
	if !r.Type.Valid() {
		return ErrInvalidNotificationType
	}
	if !r.Status.Valid() {
		return ErrInvalidNotificationStatus
	}
	return nil
}
