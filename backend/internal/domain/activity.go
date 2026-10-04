package domain

import "time"

// ActivityState is how busy the agent is, reported via the agent's CLI hook
// callbacks, not inferred from transcript/JSONL
type ActivityState string

// Activity states. WaitingInput and Blocked are sticky (see IsSticky).
//
// WaitingInput and Blocked both mean "paused on the user" but demand opposite
// automation: waiting_input is an agent at an empty prompt awaiting its next
// INSTRUCTION (safe to message or nudge), while blocked is an agent stopped on
// a pending DECISION — a tool-permission or approval dialog — where a stray
// keystroke could answer the dialog on the user's behalf. Automated senders
// must never inject input into a blocked session. (Not to be confused with the
// PR-stack Blocked flag in the status read model; blocked here predates it —
// the state existed in the original activity model and returns with the
// permission-prompt producers.)
const (
	ActivityActive       ActivityState = "active"
	ActivityIdle         ActivityState = "idle"
	ActivityWaitingInput ActivityState = "waiting_input"
	ActivityBlocked      ActivityState = "blocked"
	ActivityExited       ActivityState = "exited"
)

// IsSticky reports whether an activity state must NOT be aged/demoted by the
// passage of time (a paused agent is still paused until a new signal says so).
func (a ActivityState) IsSticky() bool {
	return a == ActivityWaitingInput || a == ActivityBlocked
}

// NeedsInput reports whether the agent is paused on the user — waiting for the
// next instruction (waiting_input) or blocked on a decision (blocked). Both
// render as the needs_input session status. Distinct from IsSticky: stickiness
// is about time-demotion, NeedsInput about the user being the unblocker.
func (a ActivityState) NeedsInput() bool {
	return a == ActivityWaitingInput || a == ActivityBlocked
}

// WorkInFlight reports whether the agent still has a turn in progress: it is
// either running (active) or paused inside that turn on the user
// (waiting_input/blocked). Idle and exited are the two states in which no turn
// is running.
//
// This is the distinction direct-branch execution ownership is scoped to
// (Checkpoint 8P-E.14A): a task owns its repository+branch while it is working,
// and while a human owes it an answer, and stops owning it when its turn ends.
// It deliberately is NOT the question "is this session alive" — an idle session
// is very much alive and can be given a new turn, which takes the lock again.
func (a ActivityState) WorkInFlight() bool {
	return a == ActivityActive || a.NeedsInput()
}

// Activity captures the persisted activity reading: the state, when that state
// last CHANGED, and when a signal was last HEARD at all.
//
// The two timestamps answer different questions and must not be conflated.
//
// LastActivityAt is a state-TRANSITION clock. It advances only when a signal
// carries a state the row did not already hold, because that is what every
// consumer scoped to a pause depends on: PauseScopeID keys one notification to
// one pause, humanQuestionFact keys one blocked entry to one dialog, and the
// waiting-input telemetry measures how long a single pause lasted. If it were
// refreshed by every repeat those all become "now" and stop identifying the
// episode they were built to identify.
//
// LastSignalAt is a LIVENESS clock. It advances on every signal that provably
// belongs to the session's current launch, including the same-state repeats
// LastActivityAt deliberately ignores — an agent hammering PostToolUse for
// twenty minutes emits scores of them and changes state in none. Before it
// existed, "is this worker alive?" had to be answered from the transition
// clock, and a worker that stayed `active` for twenty minutes was
// indistinguishable from one that had said `active` once and died: run
// wf-1c2cb9bd sat at a LastActivityAt of 17:10:20Z while its worker made 111
// model calls, and the code that reads it (workerNeedsInputCorroborationWindow)
// documented the opposite assumption in its own comment.
//
// It is deliberately coalesced rather than written per signal — see
// lifecycle.livenessCoalesceWindow for the bound and why it is sound.
//
// Zero means no signal has been heard for the current row. Rows written before
// the column existed are backfilled from LastActivityAt, which is the newest
// liveness fact those rows actually carry.
type Activity struct {
	State          ActivityState `json:"state"`
	LastActivityAt time.Time     `json:"lastActivityAt"`
	LastSignalAt   time.Time     `json:"lastSignalAt,omitempty"`
}

// Liveness is the freshest evidence that this session was heard from at all:
// the liveness clock when it has advanced, and otherwise the transition clock
// it was backfilled from. Every "has this session gone silent?" question must
// go through here rather than reading LastActivityAt directly, so a row written
// before LastSignalAt existed — or by a path that only sets the transition —
// still answers with the newest fact it has instead of a zero value.
func (a Activity) Liveness() time.Time {
	if a.LastSignalAt.After(a.LastActivityAt) {
		return a.LastSignalAt
	}
	return a.LastActivityAt
}
