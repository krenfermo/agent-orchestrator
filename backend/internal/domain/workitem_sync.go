package domain

import "strings"

// workitem_sync.go — the status mapping, and the direction rule it enforces
// (P4-E §7).
//
// THE DIRECTION RULE, stated once and enforced by the type system as far as Go
// allows: **AO execution state is authoritative.** External planning state is
// recorded and displayed; it never becomes AO's state. There is exactly one
// function here that produces an external state from an AO state, and there is
// deliberately NO function anywhere that produces an AO run or task state from
// an external one. A reader looking for the reverse mapping should find its
// absence, not a helper somebody could reach for.
//
// What external state IS allowed to influence is narrower and is not a state
// transition at all: whether a linked item looks ready to be worked on. That is
// PlanningReadiness below — advisory, never applied, and read only by surfaces
// that present a suggestion to a person.
//
// MAPPING IS EXPLICIT, NEVER STRING EQUALITY. The brief asks for this directly,
// and the reason is that both vocabularies grow: a new AO run state must fail
// to compile into a mapping rather than silently fall through to whatever
// happens to match, and an external state AO does not recognise must be
// reported as unrecognised rather than bucketed by name.

// WorkItemSyncEvent is an AO execution moment worth telling an external system
// about.
//
// The list is short on purpose (§8: do not spam every low-level lifecycle
// event). Each one is a moment a person watching the external board would want
// to see, and none of them fires more than once per real-world occurrence.
type WorkItemSyncEvent string

// The sync events.
const (
	// WorkItemSyncStarted is "AO has begun executing this work".
	WorkItemSyncStarted WorkItemSyncEvent = "started"
	// WorkItemSyncNeedsAttention is "AO stopped and a person has to decide
	// something". It is the one event that most deserves to reach a planning
	// board, because it is the one that needs a human.
	WorkItemSyncNeedsAttention WorkItemSyncEvent = "needs_attention"
	// WorkItemSyncCompleted is "AO finished this work successfully".
	WorkItemSyncCompleted WorkItemSyncEvent = "completed"
	// WorkItemSyncFailed is "AO stopped without completing and is not going to
	// retry on its own".
	WorkItemSyncFailed WorkItemSyncEvent = "failed"
	// WorkItemSyncCancelled is "a person cancelled this work".
	WorkItemSyncCancelled WorkItemSyncEvent = "cancelled"
)

// ValidWorkItemSyncEvent reports whether e is an event this build emits.
func ValidWorkItemSyncEvent(e WorkItemSyncEvent) bool {
	switch e {
	case WorkItemSyncStarted, WorkItemSyncNeedsAttention, WorkItemSyncCompleted,
		WorkItemSyncFailed, WorkItemSyncCancelled:
		return true
	default:
		return false
	}
}

// WorkItemSyncEventForRun maps an AO workflow run state to the sync event that
// state represents, or reports that the state is not worth reporting.
//
// The switch is total over WorkflowRunState. A state that produces no event
// says so explicitly rather than falling through a default, so adding a run
// state forces a decision here instead of silently producing nothing.
func WorkItemSyncEventForRun(state WorkflowRunState) (WorkItemSyncEvent, bool) {
	switch state {
	case WorkflowRunRunning:
		return WorkItemSyncStarted, true
	case WorkflowRunNeedsAttention:
		return WorkItemSyncNeedsAttention, true
	case WorkflowRunCompleted:
		return WorkItemSyncCompleted, true
	case WorkflowRunFailed:
		return WorkItemSyncFailed, true
	case WorkflowRunCancelled:
		return WorkItemSyncCancelled, true
	case WorkflowRunPending:
		// Queued but not started. Announcing it would move an external item to
		// "in progress" while nothing is happening, which is worse than
		// silence: a planning board that says work has begun when it has not
		// is a board people stop trusting.
		return "", false
	case WorkflowRunWaiting:
		// Waiting on a person or a wake. The item's external state is already
		// whatever the last real event set it to, and "still waiting" is not
		// news.
		return "", false
	default:
		return "", false
	}
}

// WorkItemSyncEventForTask maps an AO planned-task state to its sync event.
func WorkItemSyncEventForTask(state WorkflowTaskState) (WorkItemSyncEvent, bool) {
	switch state {
	case WorkflowTaskRunning:
		return WorkItemSyncStarted, true
	case WorkflowTaskNeedsAttention:
		return WorkItemSyncNeedsAttention, true
	case WorkflowTaskCompleted:
		return WorkItemSyncCompleted, true
	case WorkflowTaskFailed:
		return WorkItemSyncFailed, true
	case WorkflowTaskCancelled:
		return WorkItemSyncCancelled, true
	default:
		return "", false
	}
}

// TargetStateGroup is the external state group an event moves an item to, or
// false when the event should leave the item's state alone.
//
// needs_attention deliberately does NOT move the state. There is no external
// group that means "a human must decide": mapping it to `started` would be a
// lie about progress and mapping it to `cancelled` would be a lie about
// intent. What it produces instead is a comment (see WorkItemSyncEvent's use
// in the sync service), which is the honest way to say something a state
// machine cannot.
func (e WorkItemSyncEvent) TargetStateGroup() (WorkItemStateGroup, bool) {
	switch e {
	case WorkItemSyncStarted:
		return WorkItemStateStarted, true
	case WorkItemSyncCompleted:
		return WorkItemStateCompleted, true
	case WorkItemSyncCancelled:
		return WorkItemStateCancelled, true
	case WorkItemSyncFailed:
		// A failed AO run does not mean the planned work is cancelled — the
		// work is still wanted, and somebody will look at it. Moving the item
		// to `cancelled` would delete it from the board's active plan on
		// AO's say-so, which is exactly the authority P4-E refuses to take.
		return "", false
	case WorkItemSyncNeedsAttention:
		return "", false
	default:
		return "", false
	}
}

// PlanningReadiness is what an external state group SUGGESTS about whether
// work should be picked up. It is advisory in the strongest sense: nothing in
// AO's execution path reads it, and it can never change a run's state.
//
// It exists because §7 permits planning state to influence whether work is
// queued or deferred, and the honest way to permit that is a value a human-
// facing surface can show — "this item is in Backlog, are you sure?" — rather
// than a hook that lets a board stop a run somebody started.
type PlanningReadiness string

// The planning-readiness values.
const (
	// PlanningReady is "the external plan says this is meant to be worked on".
	PlanningReady PlanningReadiness = "ready"
	// PlanningDeferred is "the external plan has this parked".
	PlanningDeferred PlanningReadiness = "deferred"
	// PlanningDone is "the external plan considers this finished or dropped".
	PlanningDone PlanningReadiness = "done"
	// PlanningUnknown is an external state AO does not recognise, or none.
	PlanningUnknown PlanningReadiness = "unknown"
)

// ReadinessOf reports what an external state group suggests about readiness.
func ReadinessOf(g WorkItemStateGroup) PlanningReadiness {
	switch g {
	case WorkItemStateStarted, WorkItemStateUnstarted:
		return PlanningReady
	case WorkItemStateBacklog, WorkItemStateTriage:
		return PlanningDeferred
	case WorkItemStateCompleted, WorkItemStateCancelled:
		return PlanningDone
	default:
		return PlanningUnknown
	}
}

// SyncDedupeKey is the identity of one sync attempt: the same real-world
// moment, however many times it is observed or retried, is one key.
//
// It is what makes the outbox idempotent across retries, restarts and
// duplicate lifecycle callbacks, and it is what AO writes into the provider's
// own comment external-id so a comment is not posted twice even if AO loses
// its record of having posted it.
func SyncDedupeKey(scope WorkItemLinkScope, scopeID string, event WorkItemSyncEvent) string {
	return strings.Join([]string{string(scope), strings.TrimSpace(scopeID), string(event)}, ":")
}
