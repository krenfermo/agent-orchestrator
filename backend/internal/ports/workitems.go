package ports

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// workitems.go — the outbound port for external work-management providers
// (P4-E).
//
// It sits beside Tracker rather than inside it. Tracker is the READ-ONLY
// issue-tracker port whose own contract says richer per-provider behaviour
// belongs behind a separate port; this is that port, and the reasoning is in
// domain/workitem.go. What the two share is deliberate and lives BELOW both:
// domain.NormalizedIssueState, the httpkit HTTP plumbing, and the project-
// scoped authorization model.
//
// EVERY METHOD IS OPTIONAL TO AO. There is no call site in AO's lifecycle that
// cannot proceed when this port is nil or every method fails. That is the
// whole of §13: a planning system being down is a reason to defer a sync, and
// never a reason to stop executing work.

// WorkItems is one configured work-management provider, already scoped to a
// workspace by its construction.
//
// Implementations must be safe for concurrent use and must honour ctx
// cancellation on every call: the sync worker gives each attempt a bounded
// deadline, and a provider that ignores it would be able to hold the worker
// open indefinitely.
type WorkItems interface {
	// Preflight verifies the configured credential actually works and reports
	// what it reached. It is the "test connection" button, and it is the only
	// method a caller may use to decide the integration is healthy.
	Preflight(ctx context.Context) (WorkItemsIdentity, error)

	// ListProjects enumerates the projects in the configured workspace, so a
	// person mapping an AO project can choose from a list rather than paste a
	// UUID.
	ListProjects(ctx context.Context) ([]domain.WorkItemProject, error)

	// ListStates returns the states one project defines. Transitions resolve a
	// concrete state within a group through this, which is what lets AO write
	// to a workspace whose state names it was never told.
	ListStates(ctx context.Context, projectID string) ([]domain.WorkItemState, error)

	// Get reads one work item.
	Get(ctx context.Context, ref domain.WorkItemRef) (domain.WorkItem, error)

	// Resolve finds a work item from the human-readable reference a person
	// typed ("PROJ-123"), or from a provider URL. It exists because that is
	// what a person has in their hand; requiring a UUID would make linking a
	// copy-paste-from-the-API exercise.
	Resolve(ctx context.Context, reference string) (domain.WorkItem, error)

	// FindByExternalID looks up the item AO previously created for one of its
	// own ids. It is the idempotency read that makes "create an item for this
	// run" safe to retry: the provider itself indexes on the pair, so a
	// duplicate create is prevented by the provider rather than by AO
	// remembering.
	FindByExternalID(ctx context.Context, projectID, externalID string) (domain.WorkItem, bool, error)

	// Create makes a new work item. The request carries AO's own external id,
	// so a retried create resolves to the same item rather than a second one.
	Create(ctx context.Context, req WorkItemCreateRequest) (domain.WorkItem, error)

	// Transition moves an item into the given state group, choosing a concrete
	// state within it. It is a no-op returning nil when the item is already in
	// that group — a sync that fires twice must not churn the item's history.
	Transition(ctx context.Context, ref domain.WorkItemRef, group domain.WorkItemStateGroup) error

	// Comment posts a progress note. DedupeKey is written into the provider's
	// own comment external-id where supported, so the same note is never
	// posted twice even across an AO restart that lost its outbox row.
	Comment(ctx context.Context, ref domain.WorkItemRef, body, dedupeKey string) error
}

// WorkItemsIdentity is what a successful preflight reached. It carries nothing
// secret — a workspace name and how many projects are visible — and is what
// the settings surface shows as proof the connection works.
type WorkItemsIdentity struct {
	Provider domain.WorkItemProvider `json:"provider"`
	// Workspace is the workspace slug the credential is scoped to.
	Workspace string `json:"workspace"`
	// WorkspaceName is the human name, when the provider returns one.
	WorkspaceName string `json:"workspaceName,omitempty"`
	// Projects is how many projects the credential can see. It is the cheapest
	// honest evidence that the token has real access rather than merely
	// existing.
	Projects int `json:"projects"`
}

// WorkItemCreateRequest is a new work item, in provider-neutral terms.
type WorkItemCreateRequest struct {
	// ProjectID is the provider-side project the item is created in.
	ProjectID string
	Title     string
	// Description is plain text. The adapter is responsible for whatever
	// escaping its provider needs; callers never pass markup.
	Description string
	// StateGroup is the group the new item should start in. Zero value leaves
	// the provider's own default state, which is the right behaviour when AO
	// has no opinion.
	StateGroup domain.WorkItemStateGroup
	// Labels are applied when the provider already has a label of that name.
	// AO deliberately does not CREATE labels: minting taxonomy in somebody
	// else's planning tool is not a side effect a link should have.
	Labels []string
	// ExternalID is AO's own identity for this item (domain.ExternalIDFor).
	// Required: a created item AO cannot find again is one AO will duplicate.
	ExternalID string
}

// WorkItemsError classifies a provider failure so callers can decide whether
// to retry without matching on message text.
//
// The distinction that matters is Retryable: the sync worker backs off and
// tries again on a transient failure, and stops permanently on a terminal one.
// Getting that wrong in the safe direction (treating terminal as transient)
// produces a queue that never drains; getting it wrong the other way silently
// drops work. So the classification is the adapter's job, made once, where the
// HTTP status is still in scope.
type WorkItemsError struct {
	// Op is the operation that failed, for logs and for the audit row.
	Op string
	// Kind is the failure class (see the WorkItemsErr* constants).
	Kind WorkItemsErrorKind
	// Status is the HTTP status when there was one, else zero.
	Status int
	// Message is a provider-supplied explanation, already stripped of
	// anything that could carry a credential.
	Message string
	// Err is the wrapped cause, if any.
	Err error
}

// WorkItemsErrorKind is the failure class.
type WorkItemsErrorKind string

// The failure classes.
const (
	// WorkItemsErrAuth is a rejected or missing credential. Never retried: a
	// bad token stays bad, and retrying it is how an account gets locked.
	WorkItemsErrAuth WorkItemsErrorKind = "auth"
	// WorkItemsErrNotFound is a workspace, project or item that is not there.
	// Never retried.
	WorkItemsErrNotFound WorkItemsErrorKind = "not_found"
	// WorkItemsErrInvalid is a request the provider refused on its merits.
	// Never retried — the same request will be refused again.
	WorkItemsErrInvalid WorkItemsErrorKind = "invalid"
	// WorkItemsErrRateLimited is a rate limit. Retried, after the provider's
	// own reset hint where it gives one.
	WorkItemsErrRateLimited WorkItemsErrorKind = "rate_limited"
	// WorkItemsErrUnavailable is a network failure, a timeout, or a 5xx.
	// Retried.
	WorkItemsErrUnavailable WorkItemsErrorKind = "unavailable"
	// WorkItemsErrNotConfigured is "there is no provider here at all". It is a
	// first-class kind rather than a nil check at every call site, because
	// "not configured" is the DEFAULT state of this integration and every
	// caller has to handle it.
	WorkItemsErrNotConfigured WorkItemsErrorKind = "not_configured"
)

// Error renders the failure without ever including a credential: the adapter
// builds Message from the provider's own error body, which it truncates and
// which never contains the request's own headers.
func (e *WorkItemsError) Error() string {
	if e == nil {
		return "work items: <nil>"
	}
	msg := "work items: " + e.Op + ": " + string(e.Kind)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// Unwrap exposes the cause to errors.Is/As.
func (e *WorkItemsError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Retryable reports whether trying the same request later could succeed.
//
// A nil error is not retryable because there is nothing to retry, and an error
// that is not a *WorkItemsError is treated as retryable: an unclassified
// failure is most often a transport problem, and the outbox's own attempt
// ceiling bounds the cost of being wrong.
func (e *WorkItemsError) Retryable() bool {
	if e == nil {
		return false
	}
	switch e.Kind {
	case WorkItemsErrRateLimited, WorkItemsErrUnavailable:
		return true
	default:
		return false
	}
}
