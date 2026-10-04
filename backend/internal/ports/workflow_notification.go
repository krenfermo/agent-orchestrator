package ports

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// SessionFactKind names a session-level fact AO has already written durably.
//
// These are FACTS, not events on a bus. Nothing in AO publishes them: each one
// is read off the durable authority that already owns it, at the moment that
// authority commits, and handed to the notification producer
// (service/notification). There is deliberately no queue, no subscriber
// registry, and no second write of the fact itself -- a notification row is the
// only thing the producer creates.
//
// SCOPE. These cover facts about a SESSION. Facts about a workflow RUN --
// completed, failed, needs-attention -- are raised by the workflow coordinator
// from the run's own durable state (workflow/completion_notify.go and
// workflow/attention_notify.go) and do not come through here. That split is
// deliberate: a session can sit on a permission prompt, exhaust a nudge budget,
// or have a review fail under it without its run ever changing state, and the
// run can complete or fail without any single session saying so.
type SessionFactKind string

// The session-level facts the producer understands. Each maps 1:1 onto a
// durable authority that exists today; the authority is named on each constant
// so a reader can go check it.
const (
	// SessionFactHumanQuestion: the agent came to rest on a pending decision (a
	// tool-permission or approval dialog) -- any entry into blocked, whether
	// straight from active/idle or by escalating out of waiting_input.
	// Authority: sessions.activity_state.
	SessionFactHumanQuestion SessionFactKind = "human_question"

	// SessionFactRepairAttempted: AO sent another automatic repair nudge for a
	// problem and the attempt budget still has room. This fact exists so the
	// autonomous policy is explicit rather than implied by silence: the
	// producer deliberately records nothing for it. Authority: the persisted
	// reaction-attempt map in pr.last_nudge_signature.
	SessionFactRepairAttempted SessionFactKind = "repair_attempted"

	// SessionFactRepairExhausted: that same attempt budget is spent and the
	// problem is still there. Authority: pr.last_nudge_signature.
	//
	// This is the LIFECYCLE nudge loop, not the workflow fix/verify loop behind
	// workflow.ReasonFixBudgetExhausted -- that one is a run-level stop already
	// reported as needs_attention.
	SessionFactRepairExhausted SessionFactKind = "repair_exhausted"

	// SessionFactIntegrationFailed: an integration AO drives for the session
	// failed durably. Authority today: a review_run row written with status
	// failed.
	SessionFactIntegrationFailed SessionFactKind = "integration_failed"
)

// Deferred: budget warning and exhaustion.
//
// P4-D lists them as candidates. They are deliberately absent because no
// durable authority for them exists: domain.UsageBudgetStatus is computed on
// demand by usage.EvaluateWorkflowBudget from ledger rows, and nothing records
// that a run CROSSED a threshold. A producer built on the read model would
// either poll and diff in memory (which a restart re-announces) or need a new
// durable transition record, which is a design this phase has no mandate to
// invent. When such a fact exists, adding it here is one constant, one policy
// case and one type: the dedupe derivation below already generalizes.

// SessionFact is one durable session-level fact handed to the notification
// producer. It carries the fact's DURABLE IDENTITY, never a rendering of it:
// the producer derives the dedupe key from Kind plus SessionID plus ScopeID, so
// two reads of the same stored fact -- a re-observation, a reconciliation pass,
// a daemon restart part-way through creating the row -- always produce the same
// key and therefore at most one notification.
type SessionFact struct {
	Kind      SessionFactKind
	SessionID domain.SessionID
	ProjectID domain.ProjectID

	// ScopeID identifies WHICH instance of Kind this is, within the session.
	// It must come from durable state and must not contain an observation
	// timestamp, a message body, or anything else that changes when the same
	// fact is read again:
	//
	//   - repair facts pass the persisted reaction key plus its attempt budget;
	//   - the human-question fact passes the stored activity timestamp of the
	//     pause it reports, which is that pause's durable identity;
	//   - integration failures pass the failed review run's id.
	ScopeID string

	// PRURL scopes the fact to a PR when it has one, so the dashboard can link
	// to it. It is already part of ScopeID for repair facts; it is never mixed
	// into the dedupe key on its own.
	PRURL string

	// Detail is human-facing context appended to the notification body. It is
	// deliberately excluded from the dedupe key: the same fact re-read with
	// slightly different wording is still the same fact.
	Detail string

	// SessionDisplayName is an enrichment hint that avoids a store read.
	SessionDisplayName string

	// ObservedAt is when the durable fact was read. It becomes the row's
	// created_at and never feeds the dedupe key.
	ObservedAt time.Time
}

// SessionNotifier records session-level facts as notifications. Lifecycle and
// the review engine hold this narrow interface so they depend on the contract,
// not on the service package that implements it.
type SessionNotifier interface {
	// Record applies the autonomous policy to fact and, when it warrants a
	// notification, creates exactly one -- idempotently.
	Record(ctx context.Context, fact SessionFact) error
}

// RepairScopeID is the durable identity of one automatic repair loop: the
// persisted reaction key (which already names the problem and the PR) plus the
// attempt budget that loop was given. The budget is part of the identity so
// raising the budget starts a genuinely new repair rather than being silenced
// forever by the exhaustion already recorded under the old one.
func RepairScopeID(reactionKey string, maxAttempts int) string {
	return fmt.Sprintf("%s#%d", strings.TrimSpace(reactionKey), maxAttempts)
}

// PauseScopeID is the durable identity of one pause: the activity timestamp
// stored on the session row for it. Re-reading that row -- after a restart, or
// on a repeat signal AO folds into the same state -- yields the same value, so
// one pause maps to one notification. A later, genuinely distinct pause carries
// its own stored timestamp and is free to raise its own.
func PauseScopeID(pausedAt time.Time) string {
	return pausedAt.UTC().Format(time.RFC3339Nano)
}
