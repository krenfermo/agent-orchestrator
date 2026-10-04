package notification

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// SessionFactNotifier turns durable session-level facts into notification rows.
//
// It is a translator, not an event bus. Every fact it receives has already been
// written by the authority that owns it -- sessions.activity_state behind a
// pending decision, the persisted reaction-attempt map behind a repair, a
// failed review_run row behind an integration failure. This type reads that
// fact, decides whether it is worth a person's attention, and writes one
// notification through the single existing write path (internal/notify). It
// never stores the fact a second time, never queues it, and never publishes it
// anywhere else.
//
// # Autonomous policy
//
// AO repairs its own runs. It nudges an agent to fix failing CI, to address
// review feedback, to rebase a conflict, and it retries each of those on a
// budget. None of that is the user's problem while it is working, so none of it
// notifies. See policyFor for the rule and both of its sides.
//
// # Idempotency
//
// Every notification written here carries a dedupe key derived from the fact's
// durable identity (see dedupeKeyFor). Storage holds a permanent unique index
// over (type, dedupe_key), so a re-observation, a reconciliation sweep, or a
// daemon that crashed between writing the row and publishing it can all replay
// the same fact and get the same single row back. Nothing here compares
// timestamps or message text to detect a duplicate.
type SessionFactNotifier struct {
	notifier Notifier
	clock    func() time.Time
}

// Notifier is the existing write-side notification producer
// (internal/notify.Manager). Routing through it is deliberate: it owns the one
// notifications table, the row validation, the email fan-out and the live
// dashboard stream, so a session-scoped notification cannot end up in a second
// store or miss the stream.
type Notifier interface {
	Notify(ctx context.Context, intent ports.NotificationIntent) error
}

// SessionFactDeps configures a SessionFactNotifier.
type SessionFactDeps struct {
	Notifier Notifier
	// Clock stamps facts that arrive without an observation time. Injectable
	// for deterministic tests.
	Clock func() time.Time
}

// NewSessionFactNotifier builds the session-level notification producer.
func NewSessionFactNotifier(d SessionFactDeps) *SessionFactNotifier {
	clock := d.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &SessionFactNotifier{notifier: d.Notifier, clock: clock}
}

// ErrInvalidSessionFact reports a fact that cannot be addressed to a session.
var ErrInvalidSessionFact = errors.New("notification: invalid session fact")

// Record applies the autonomous policy to one durable session-level fact and,
// when the policy says a person should hear about it, creates exactly one
// notification for it.
//
// Recording the same fact again is a no-op: the dedupe key is a function of the
// fact's durable identity, and storage rejects a second row for it.
func (n *SessionFactNotifier) Record(ctx context.Context, fact ports.SessionFact) error {
	if n == nil || n.notifier == nil {
		return errors.New("notification: session fact notifier is not configured")
	}
	p := policyFor(fact.Kind)
	if !p.notifies {
		// Not a defect and not a dropped notification: the fact is one AO is
		// still handling on its own. Silence here IS the policy.
		return nil
	}
	if fact.SessionID == "" || fact.ProjectID == "" {
		return fmt.Errorf("%w: %s has no session to address", ErrInvalidSessionFact, fact.Kind)
	}
	key, err := dedupeKeyFor(fact)
	if err != nil {
		return err
	}
	observedAt := fact.ObservedAt
	if observedAt.IsZero() {
		observedAt = n.clock()
	}
	return n.notifier.Notify(ctx, ports.NotificationIntent{
		Type:      p.typ,
		SessionID: fact.SessionID,
		ProjectID: fact.ProjectID,
		PRURL:     strings.TrimSpace(fact.PRURL),
		CreatedAt: observedAt.UTC(),
		DedupeKey: key,
		// Provenance: the key IS the source event's durable identity, so
		// storing it as the source event id costs nothing and makes the row
		// traceable back to the fact without re-deriving anything.
		Source:             p.source,
		SourceEventID:      key,
		Detail:             strings.TrimSpace(fact.Detail),
		SessionDisplayName: fact.SessionDisplayName,
	})
}

// policy is what the autonomous policy decided about one fact kind.
type policy struct {
	notifies bool
	typ      domain.NotificationType
	source   domain.NotificationSource
}

// policyFor is the autonomous policy, stated in one place.
//
// The rule: while AO still has an autonomous move left, it makes it silently. A
// notification is a claim on someone's attention, and a claim is only justified
// once AO cannot make progress on its own.
//
// The two sides of that rule:
//
//   - A repair ATTEMPT notifies nothing. AO nudging an agent to fix CI or to
//     address review feedback is AO working, not AO stuck, and it happens
//     repeatedly per PR. SessionFactRepairAttempted exists precisely so this is
//     written down rather than left as an absent branch.
//   - A repair EXHAUSTION notifies. The attempt budget is spent, the problem
//     survived it, and nothing further happens without a person.
//
// The other two kinds follow the same logic from the other direction: a pending
// decision is a state automation is forbidden to resolve, and a durably failed
// integration is not something AO retries. Both are, by construction, states AO
// cannot leave by itself -- which is exactly what makes them worth a ping.
func policyFor(kind ports.SessionFactKind) policy {
	switch kind {
	case ports.SessionFactHumanQuestion:
		return policy{
			notifies: true,
			typ:      domain.NotificationHumanQuestionRequired,
			source:   domain.NotificationSourceLifecycle,
		}
	case ports.SessionFactRepairExhausted:
		return policy{
			notifies: true,
			typ:      domain.NotificationRepairExhausted,
			source:   domain.NotificationSourceLifecycle,
		}
	case ports.SessionFactIntegrationFailed:
		return policy{
			notifies: true,
			typ:      domain.NotificationIntegrationFailed,
			source:   domain.NotificationSourceReview,
		}
	case ports.SessionFactRepairAttempted:
		// AO is repairing. Deliberately silent -- see the doc comment above.
		return policy{}
	default:
		return policy{}
	}
}

// dedupeKeyFor derives the storage idempotency key from the fact's durable
// identity: the kind, the session it belongs to, and the durable id of which
// instance of that kind this is.
//
// Nothing that varies between two reads of the same stored fact goes in. Not
// the observation time, not the notification's wording, not a counter held only
// in memory. That is the whole point: a reconciliation pass re-reading the same
// rows, or a daemon restarting after the row was written but before it was
// published, recomputes the identical key, and storage's unique index turns the
// second attempt into a no-op.
//
// Every notifying kind needs a scope id, because a session can legitimately
// produce more than one of each: two distinct pauses, two repair loops on
// different problems, two failed review runs.
func dedupeKeyFor(fact ports.SessionFact) (string, error) {
	scope := strings.TrimSpace(fact.ScopeID)
	if scope == "" {
		return "", fmt.Errorf("%w: %s needs a durable scope id", ErrInvalidSessionFact, fact.Kind)
	}
	return "sf:" + string(fact.Kind) + ":" + string(fact.SessionID) + ":" + scope, nil
}
