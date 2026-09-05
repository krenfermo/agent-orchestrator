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

// Session-scoped run facts (P4-D). The workflow_* and task_* kinds above are
// raised by the workflow coordinator from a workflow RUN's own durable state.
// These three are raised from facts about a SESSION that the lifecycle reducer
// and the review engine observe directly, and they exist because no run-level
// state change corresponds to them: a session can sit on a permission prompt,
// burn through a repair budget, or have a review pass fail under it without its
// run ever changing state.
//
// They are event-keyed like the run kinds, so the same permanent dedupe applies.
const (
	// NotificationHumanQuestionRequired means the agent came to rest on a
	// pending DECISION -- a tool-permission or approval dialog. Only a person
	// can answer it: automation must not write into a blocked session at all.
	// Authority: sessions.activity_state entering ActivityBlocked.
	NotificationHumanQuestionRequired NotificationType = "human_question_required"
	// NotificationRepairExhausted means AO's automatic nudge loop spent its
	// attempt budget on one problem without fixing it. The attempts themselves
	// are silent -- that is the autonomous policy, not an oversight -- so this
	// is the first and only ping for that repair. Authority: the attempt map
	// persisted in pr.last_nudge_signature.
	//
	// Distinct from the fix/verify budget behind workflow.ReasonFixBudgetExhausted,
	// which is a workflow-run concern already reported as needs_attention.
	NotificationRepairExhausted NotificationType = "repair_exhausted"
	// NotificationIntegrationFailed means an integration AO drives on the
	// session's behalf failed durably. Authority today: a review_run row
	// written with status failed.
	NotificationIntegrationFailed NotificationType = "integration_failed"
)

// Deferred: budget_warning and budget_exhausted.
//
// P4-D lists them as candidates, and they are deliberately NOT defined here.
// The usage budget (domain.UsageBudgetStatus) is a pure read model: it is
// computed on demand by usage.EvaluateWorkflowBudget from ledger rows, and
// nothing anywhere records that a run CROSSED a threshold. Producing a
// notification from it would mean either polling and diffing in memory -- which
// a restart re-announces -- or inventing a durable transition record, which is
// inventing an authority this phase has no mandate to design. When a durable
// crossing fact exists, adding the two types is a migration and two cases here;
// the dedupe, policy and storage idempotency below already generalize to them.

// SessionRunFact reports whether t is one of the session-scoped run facts.
func (t NotificationType) SessionRunFact() bool {
	switch t {
	case NotificationHumanQuestionRequired, NotificationRepairExhausted, NotificationIntegrationFailed:
		return true
	default:
		return false
	}
}

// Valid reports whether t is one of the v1 notification kinds.
func (t NotificationType) Valid() bool {
	switch t {
	case NotificationNeedsInput, NotificationReadyToMerge, NotificationPRMerged, NotificationPRClosedUnmerged,
		NotificationTaskCompleted, NotificationWorkflowCompleted,
		NotificationTaskNeedsAttention, NotificationWorkflowNeedsAttention,
		NotificationTaskFailed, NotificationWorkflowFailed:
		return true
	default:
		return t.SessionRunFact()
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
	return t.Completion() || t.Attention() || t.Failure() || t.SessionRunFact()
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
	// The session-scoped facts fold into the two coarse events a person would
	// have picked anyway: a question and a spent repair budget are both "AO
	// stopped and needs you", and a failed integration is a failure. Nobody
	// wants a third checkbox for the difference.
	case t == NotificationHumanQuestionRequired, t == NotificationRepairExhausted:
		return EmailEventNeedsAttention, true
	case t == NotificationIntegrationFailed:
		return EmailEventFailed, true
	default:
		return "", false
	}
}

// NotificationRecipient names the principal a notification is addressed to.
//
// AO runs single-user / local-trusted, so every notification today is addressed
// to NotificationRecipientLocal -- the one principal operating this machine.
// The type exists so a later multi-principal change (P4-B users/teams, P4-C
// tenants) can introduce real recipients by minting new values and scoping
// reads by recipient, without reshaping the stored model again. It is
// deliberately an opaque string rather than a foreign key: no users or teams
// table exists, and inventing one before there are multiple principals would be
// a guess at its shape.
//
// EXTENSION POINT (P4-B/P4-C): give this a real identity source, scope every
// list and count query by recipient, and require producers to address rows
// explicitly instead of defaulting to the local principal.
type NotificationRecipient string

// NotificationRecipientLocal is the single local principal in local-trusted
// mode. Rows written before recipients existed were backfilled to it.
const NotificationRecipientLocal NotificationRecipient = "local"

// Valid reports whether r names a principal. Local-trusted mode accepts any
// non-empty value; validating against a principal registry is the P4-B step.
func (r NotificationRecipient) Valid() bool { return r != "" }

// NotificationSeverity is how loudly a notification should be surfaced,
// independent of its type, so a delivery channel can filter on it without
// enumerating every notification kind.
type NotificationSeverity string

// Notification severities.
const (
	// NotificationSeverityInfo reports something that happened; nothing is owed.
	NotificationSeverityInfo NotificationSeverity = "info"
	// NotificationSeverityWarning reports something waiting on the user.
	NotificationSeverityWarning NotificationSeverity = "warning"
	// NotificationSeverityCritical reports something actively going wrong.
	NotificationSeverityCritical NotificationSeverity = "critical"
)

// Valid reports whether s is a supported severity.
func (s NotificationSeverity) Valid() bool {
	switch s {
	case NotificationSeverityInfo, NotificationSeverityWarning, NotificationSeverityCritical:
		return true
	default:
		return false
	}
}

// DefaultSeverity is how loudly a kind is surfaced when a producer does not
// say. It matches the backfill in migration 0154.
func (t NotificationType) DefaultSeverity() NotificationSeverity {
	switch {
	case t == NotificationWorkflowFailed || t == NotificationTaskFailed ||
		t == NotificationIntegrationFailed:
		return NotificationSeverityCritical
	case t == NotificationNeedsInput || t == NotificationPRClosedUnmerged ||
		t.Attention() || t == NotificationHumanQuestionRequired ||
		t == NotificationRepairExhausted:
		return NotificationSeverityWarning
	default:
		return NotificationSeverityInfo
	}
}

// NotificationDeliveryState tracks fan-out of a stored notification to outbound
// delivery channels. Persisting the row is itself the in-app delivery, so a row
// with no outbound channel is born delivered; a row that also owes an email is
// written pending and driven forward by the email outbox.
type NotificationDeliveryState string

// Notification delivery states.
const (
	// NotificationDeliveryPending means at least one channel still owes delivery.
	NotificationDeliveryPending NotificationDeliveryState = "pending"
	// NotificationDeliveryDelivered means every owed channel accepted it.
	NotificationDeliveryDelivered NotificationDeliveryState = "delivered"
	// NotificationDeliveryFailed means delivery was attempted and gave up.
	NotificationDeliveryFailed NotificationDeliveryState = "failed"
	// NotificationDeliverySuppressed means a policy dropped delivery on purpose
	// -- email disabled, or an event the user did not select.
	NotificationDeliverySuppressed NotificationDeliveryState = "suppressed"
)

// Valid reports whether d is a supported delivery state.
func (d NotificationDeliveryState) Valid() bool {
	switch d {
	case NotificationDeliveryPending, NotificationDeliveryDelivered,
		NotificationDeliveryFailed, NotificationDeliverySuppressed:
		return true
	default:
		return false
	}
}

// NotificationSource names the producer that emitted a notification. Paired
// with SourceEventID it is the provenance half of the record: it says which
// observation a stored row came from, so a row can be traced back to the fact
// that caused it. Free-form on purpose -- a new producer should not need a
// migration.
type NotificationSource string

// Notification sources that exist today.
const (
	// NotificationSourceLifecycle is the lifecycle reducer. Rows written before
	// provenance existed were backfilled to it.
	NotificationSourceLifecycle NotificationSource = "lifecycle"
	// NotificationSourceWorkflow is the workflow coordinator.
	NotificationSourceWorkflow NotificationSource = "workflow"
	// NotificationSourceReview is the review engine.
	NotificationSourceReview NotificationSource = "review"
)

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
	// TaskID is the optional planned-task anchor. Free-form: a notification
	// outlives the run it reports on, so it must not hard-reference a task row.
	TaskID string
	Type   NotificationType
	Title  string
	Body   string
	Status NotificationStatus
	// Recipient is the principal this row is addressed to. Empty on the way in
	// means the single local principal; storage never stores it empty.
	Recipient NotificationRecipient
	// Severity is how loudly to surface this, independent of Type. Empty on the
	// way in means Type.DefaultSeverity().
	Severity NotificationSeverity
	// DeliveryState tracks fan-out to outbound channels. The in-app row itself
	// is always delivered by virtue of existing.
	DeliveryState NotificationDeliveryState
	// Source and SourceEventID are provenance: which producer wrote this row,
	// and the durable id of the observation it read.
	Source        NotificationSource
	SourceEventID string
	CreatedAt     time.Time
	// ReadAt is when the user acknowledged this notification. It carries the
	// same fact as Status ("read" <=> non-zero) and exists because Status
	// records only that an acknowledgement happened, never when.
	ReadAt time.Time
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
	// Recipient, severity and delivery state are defaulted by the writer rather
	// than demanded of every producer, so an empty value is valid here and only
	// a value that is set-but-wrong is rejected.
	if r.Recipient != "" && !r.Recipient.Valid() {
		return ErrInvalidNotificationRecord
	}
	if r.Severity != "" && !r.Severity.Valid() {
		return ErrInvalidNotificationRecord
	}
	if r.DeliveryState != "" && !r.DeliveryState.Valid() {
		return ErrInvalidNotificationRecord
	}
	return nil
}

// WithDefaults fills the model fields a producer may leave unset. It is the one
// place those defaults live, so the store, the tests and any future producer
// agree on what an unaddressed, unrated, in-app-only notification means.
func (r NotificationRecord) WithDefaults() NotificationRecord {
	if r.Recipient == "" {
		r.Recipient = NotificationRecipientLocal
	}
	if r.Severity == "" {
		r.Severity = r.Type.DefaultSeverity()
	}
	if r.DeliveryState == "" {
		r.DeliveryState = NotificationDeliveryDelivered
	}
	if r.Status == NotificationRead && r.ReadAt.IsZero() {
		r.ReadAt = r.CreatedAt
	}
	return r
}
