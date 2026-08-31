package workflow

import (
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// checkpoint_authority.go — which checkpoint gets to say why a run is stopped.
//
// THE INCIDENT (wf-c4c84f52). The run stopped at 02:30:32 on
// reviewer_launch_failed, a human-owned reason with a durable row of its own.
// It then accumulated 302 `review_observed` rows, the newest at 05:57 — the
// same approved verdict, re-observed on every pass, changing nothing. Every
// reader took "the newest checkpoint" to mean "what happened to this run", so
// the newest of those observations became the run's phase, it is not a
// canonical reason, and the run's stop resolved to `unclassified_stop`.
//
// The consequences were not cosmetic. proveRepairQuiescent's clause (2) needs a
// readable, human-owned stop; an unreadable one is a refusal, correctly. So a
// repair that WAS parked for a person read as one AO could not classify, its
// origin's branch stayed with it, and blk-dc8e9a89 sat on
// feat/engineering-control-center with no way out but Cancel.
//
// # The principle
//
// A run's stop reason is not "the most recent thing that happened". It is the
// most recent thing that STOPPED IT. Observing, projecting, auditing or
// re-reading a state that already exists creates no transition and must not
// overwrite the reason a stop site deliberately wrote down.
//
// # The four authorities
//
// Every durable_phase falls into exactly one, and the classification is derived
// from the registries that already exist rather than from a second list that
// could drift from them:
//
//	OBSERVATION   records what AO saw or did without changing what the run owes:
//	              review_observed, the launch intent/attempt/claim/confirm
//	              trail, routing and policy decisions, and everything
//	              isBookkeepingPhase already excludes (the incident ledger, the
//	              repair ledger, cession rows, the retirement summary). It never
//	              defines a stop and never clears one.
//	STOP          a phase in attentionDispositions: a stop site wrote it to say
//	              why this run is parked, and the registry says what a person or
//	              AO does about it. This is the ONLY authority that names a stop.
//	STOP_CLEARED  attention_cleared: AO proved forward progress and un-parked the
//	              run (clearResolvedStop). It does not name a stop; it ENDS the
//	              authority of every stop older than itself.
//	LIFECYCLE     everything else — real transitions of the run's own progress.
//	              They are what LatestCheckpointPhase reports and what the
//	              lifecycle derivation reads; they do not name stops.
//
// # Two rules, and why each is exactly this shape
//
// RECENCY AMONG STOPS. The newest STOP row wins. A run that stops, resumes and
// stops again is explained by its latest stop, which is what recency already
// gave us and what nothing here changes.
//
// A CLEAR MUST BE STRICTLY NEWER TO INVALIDATE. Ties go to the stop. This is
// not a tie-breaking nicety, it is the wf-c4c84f52 case: that run wrote
// review_dispatched, review_launch_confirmed, review_launch_abandoned AND
// reviewer_launch_failed inside the same second, and resolving "which came
// last" among them would come down to the lexicographic order of their row ids.
// Authority is not a property of a UUID. Within one instant the stop is the
// answer, because a run that dispatched and failed to launch in the same second
// is a stopped run.
//
// # Why attention_cleared is the only invalidator
//
// It is tempting to also treat a later successful dispatch as ending an older
// stop. It is unnecessary, and it would be unsafe. Unnecessary because the
// successful-dispatch sites already call clearResolvedStop, which writes
// exactly this row (dispatch.go, review_dispatch.go, worker_progress.go).
// Unsafe because those trails are written in the same burst as the failure they
// precede — adding them would reintroduce the id-ordering coin flip this file
// exists to remove, on the exact rows that produced the incident.
//
// clearResolvedStop deliberately refuses to clear a HUMAN-owned stop, so a
// human-owned reason stays authoritative until the run records a new one. That
// is the intended reading: a person's decision is outstanding until something
// says otherwise, and "something" is a row, not the passage of time.
//
// # Legacy
//
// Nothing is migrated and no row is rewritten. The 302 observations stay
// exactly where they are; they simply stop being mistaken for a stop. Callers
// that build a RunDetail by hand, without folding a ledger, keep the older
// single-phase behaviour (see resolveAttentionReason).

// checkpointAuthority is what one durable_phase is allowed to decide.
type checkpointAuthority uint8

const (
	// authorityObservation: records something; decides nothing.
	authorityObservation checkpointAuthority = iota
	// authorityLifecycle: a real transition of the run's own progress.
	authorityLifecycle
	// authorityStop: names why the run is parked.
	authorityStop
	// authorityStopCleared: ends the authority of every older stop.
	authorityStopCleared
)

// classifyCheckpointPhase resolves one durable_phase to its authority.
//
// The order of the tests is the policy, and it is deliberate:
//
//	bookkeeping FIRST, so a phase that is both an incident-ledger row and a
//	  registered reason (incident_diagnosis_capacity_wait is exactly this) keeps
//	  the behaviour it has always had — it describes an investigation, and it
//	  has never been allowed to become the run's stop;
//	then the clear, which outranks the stop registry it is not in;
//	then the stop registry, which is the single source of truth for what counts
//	  as a reason — a reason added there is classified here with no second edit;
//	then lifecycle, the default for a phase this code does not know.
//
// An unknown phase is LIFECYCLE, never STOP: AO must never invent a stop reason
// it cannot explain, and the caller that wants one asks the registry.
func classifyCheckpointPhase(phase string) checkpointAuthority {
	if phase == "" {
		return authorityObservation
	}
	if isBookkeepingPhase(phase) {
		return authorityObservation
	}
	if phase == attentionClearedPhase {
		return authorityStopCleared
	}
	if _, ok := attentionDispositions[phase]; ok {
		return authorityStop
	}
	if observationOnlyPhases[phase] {
		return authorityObservation
	}
	return authorityLifecycle
}

// observationOnlyPhases are the phases that are NOT bookkeeping in the
// incident-ledger sense — they are part of the run's own story and belong in
// its timeline — but that still decide nothing: they record that AO looked, or
// that it wrote down an intention it also recorded the outcome of.
//
// Membership is a claim with teeth, so the bar is: can this row, on its own,
// change what the run owes? If it can, it is not here.
//
// review_observed is the case this file was written for. It re-states a verdict
// AO has already read; when it does move a step, the move is the step row's, and
// a run that ends up parked afterwards writes its own stop.
//
// The launch trail (intent -> attempt -> claimed -> confirmed) is here for a
// sharper reason than "it is chatter": those four rows and the launch's FAILURE
// are written inside the same second, and the failure is the one that says what
// happened. Ranking them by row id is exactly the bug.
var observationOnlyPhases = map[string]bool{
	reviewObservedPhase:           true,
	"review_launch_intent":        true,
	"review_launch_attempt":       true,
	"review_launch_claimed":       true,
	"review_launch_confirmed":     true,
	"review_launch_abandoned":     true,
	"review_dispatch_authorized":  true,
	"review_target_observed":      true,
	"review_target_head_observed": true,
	"review_policy_decision":      true,
	"routing_decision":            true,
	"session_lifecycle_decision":  true,
	"worker_observation_evidence": true,
	"fix_dispatch_intent":         true,
	"fix_observed_waiting":        true,
	"review_cancel_intent":        true,
	"review_cancel_confirmed":     true,
}

// checkpointAuthorityFold is one pass over a run's ledger, producing every
// derived value the readers used to compute for themselves.
//
// It exists because four call sites (the run detail, the Board projection, the
// master projection and stopReason) each ran their own copy of the same loop.
// They agreed, which is why nobody noticed; they would not have stayed
// agreeing, and "the screen says reviewer_launch_failed while the reconciler
// says unclassified_stop" is precisely the failure mode that costs a branch
// lock.
type checkpointAuthorityFold struct {
	// NextAction is the newest non-bookkeeping next_action, unchanged.
	NextAction string
	// LatestPhase/LatestAt are the newest non-bookkeeping checkpoint, unchanged:
	// this is the run's timeline, and the lifecycle derivation reads it.
	// SawLatest distinguishes "no such row" from "a row with an empty phase",
	// which the Board still reports as last activity.
	LatestPhase string
	LatestAt    time.Time
	SawLatest   bool
	// StopPhase/StopAt are the authoritative stop: the newest STOP row that no
	// STOP_CLEARED row is strictly newer than. Empty when the run has never
	// recorded a stop, or when its last one was cleared.
	StopPhase string
	StopAt    time.Time
}

// foldCheckpointAuthority is the single implementation of "what does this
// ledger say". It is order-independent: it compares timestamps rather than
// relying on the order rows come back in, and it resolves an exact tie between
// a stop and anything else in favour of the stop.
func foldCheckpointAuthority(cps []domain.WorkflowCheckpoint) checkpointAuthorityFold {
	var out checkpointAuthorityFold
	var clearedAt time.Time
	var haveStop, haveCleared bool
	for _, cp := range cps {
		authority := classifyCheckpointPhase(cp.DurablePhase)
		if !isBookkeepingPhase(cp.DurablePhase) {
			// The timeline, computed exactly as it always was: bookkeeping is
			// excluded, everything else counts. An observation still belongs to
			// the run's story and still shows up here — what changes below is
			// only that it no longer gets to name the stop.
			if cp.NextAction != "" {
				out.NextAction = cp.NextAction
			}
			if !out.SawLatest || !cp.CreatedAt.Before(out.LatestAt) {
				out.LatestPhase = cp.DurablePhase
				out.LatestAt = cp.CreatedAt
				out.SawLatest = true
			}
		}
		switch authority {
		case authorityStop:
			if !haveStop || !cp.CreatedAt.Before(out.StopAt) {
				out.StopPhase = cp.DurablePhase
				out.StopAt = cp.CreatedAt
				haveStop = true
			}
		case authorityStopCleared:
			if !haveCleared || cp.CreatedAt.After(clearedAt) {
				clearedAt = cp.CreatedAt
				haveCleared = true
			}
		}
	}
	// STRICTLY newer: a clear written in the same instant as a stop does not
	// erase it. See this file's header.
	if haveStop && haveCleared && clearedAt.After(out.StopAt) {
		out.StopPhase = ""
		out.StopAt = time.Time{}
	}
	return out
}

// applyCheckpointAuthority writes one fold onto a RunDetail. Every projection
// uses it, so every projection answers "why is this run stopped" identically.
func applyCheckpointAuthority(detail *RunDetail, cps []domain.WorkflowCheckpoint) {
	fold := foldCheckpointAuthority(cps)
	if fold.NextAction != "" {
		detail.NextAction = fold.NextAction
	}
	if fold.SawLatest {
		detail.LatestCheckpointPhase = fold.LatestPhase
		detail.LatestCheckpointAt = fold.LatestAt
	}
	detail.StopAuthorityPhase = fold.StopPhase
	detail.StopAuthorityAt = fold.StopAt
	detail.CheckpointsFolded = true
}
