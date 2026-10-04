package workflow

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ambiguous_worker_state.go — the one gate every ambiguous_worker_state stop
// passes through.
//
// `ambiguous_worker_state` is AO admitting it cannot prove what a worker is
// doing. That admission is worth making, and it was worth almost nothing in
// practice, because the class was a bare constant any decision could assign.
// The result was a stop that recorded a conclusion and no evidence: a person
// reading it learned that AO had given up, and nothing about which of the
// facts AO already holds had been consulted.
//
// So the class stopped being a constant anyone can write down. It is now a
// value that can only be produced from a COLLECTED evidence snapshot:
//
//	AmbiguousWorkerState{}.ErrorClass()        == ""   (nothing, ever)
//	RaiseAmbiguousWorkerState(zero, …)         -> ErrAmbiguousWithoutEvidence
//	c.raiseAmbiguousWorkerState(ctx, …)        -> collects, records, returns it
//
// The enforcement is structural rather than conventional: `collected` is
// unexported and is set only by CollectWorkerEvidence, so no caller in this
// package — and no caller outside it — can construct the raise by hand, and a
// snapshot deserialized off the ledger cannot be laundered back into one.
// TestAmbiguousWorkerStateIsOnlyRaisedThroughTheGate reads the package's own
// source to keep it that way.

// ErrAmbiguousWithoutEvidence is the refusal: something tried to raise
// ambiguous_worker_state without first collecting the evidence AO is required
// to stand on.
var ErrAmbiguousWithoutEvidence = errors.New(
	"workflow: ambiguous_worker_state cannot be raised without a collected evidence snapshot")

// AmbiguousWorkerStateEvidencePhase is the durable record of one raise: the
// whole snapshot, serialized, written at the moment the stop is taken.
//
// It is a separate row from the stop's own checkpoint on purpose. The stop says
// what AO decided and stays short enough to read on a Board; this says what AO
// was looking at when it decided, and is what the Incident Advisor reads back
// after a restart instead of re-deriving a later, different situation.
const AmbiguousWorkerStateEvidencePhase = "ambiguous_worker_state_evidence"

// AmbiguousWorkerState is a raised ambiguity, with the evidence under it.
//
// The zero value is not a raise, and says so in every accessor. That is the
// property the whole file rests on.
type AmbiguousWorkerState struct {
	snapshot WorkerEvidenceSnapshot
	reason   string
	detail   string
}

// RaiseAmbiguousWorkerState is the only constructor.
//
// It refuses a snapshot that did not come from CollectWorkerEvidence in this
// process, and it refuses a raise with no human sentence attached: a stop
// nobody can read is the failure this file exists to remove, and an empty
// detail reproduces it exactly.
func RaiseAmbiguousWorkerState(snap WorkerEvidenceSnapshot, reason, detail string) (AmbiguousWorkerState, error) {
	if !snap.Collected() {
		return AmbiguousWorkerState{}, fmt.Errorf("%w (reason %q)", ErrAmbiguousWithoutEvidence, reason)
	}
	if strings.TrimSpace(detail) == "" {
		return AmbiguousWorkerState{}, fmt.Errorf(
			"workflow: an ambiguous_worker_state raise must say what AO could not prove (reason %q)", reason)
	}
	return AmbiguousWorkerState{snapshot: snap, reason: reason, detail: strings.TrimSpace(detail)}, nil
}

// Raised reports whether this value is a real raise.
func (a AmbiguousWorkerState) Raised() bool { return a.snapshot.Collected() }

// ErrorClass is the ONLY way domain.WorkflowErrorAmbiguousWorkerState enters a
// decision. An unraised value yields the empty class, which is what makes
// "forgot to collect evidence" impossible to persist rather than merely
// discouraged.
func (a AmbiguousWorkerState) ErrorClass() domain.WorkflowErrorClass {
	if !a.Raised() {
		return ""
	}
	return domain.WorkflowErrorAmbiguousWorkerState
}

// Snapshot returns the evidence this raise stands on.
func (a AmbiguousWorkerState) Snapshot() WorkerEvidenceSnapshot { return a.snapshot }

// Reason is the canonical attention reason recorded alongside the stop.
func (a AmbiguousWorkerState) Reason() string { return a.reason }

// Detail is the human sentence.
func (a AmbiguousWorkerState) Detail() string { return a.detail }

// assertAmbiguousEvidence is the invariant, callable from any write path: a
// durable write carrying ambiguous_worker_state must have a raise behind it.
//
// It is belt-and-braces next to ErrorClass()'s own refusal, and it is what
// catches a class that arrived from somewhere else entirely — a stored row, an
// HTTP DTO, a classifier — on its way into a durable transition.
func assertAmbiguousEvidence(class domain.WorkflowErrorClass, raise AmbiguousWorkerState) error {
	if class != domain.WorkflowErrorAmbiguousWorkerState {
		return nil
	}
	if !raise.Raised() {
		return ErrAmbiguousWithoutEvidence
	}
	return nil
}

// raiseAmbiguousWorkerState is the whole gate: persist what was observed,
// collect the evidence, raise the ambiguity, record the snapshot — and refuse
// the raise if any of it could not be made durable.
//
// The order is the contract. `observed` is what the CALLER already paid for (a
// git observation, a runtime liveness answer), and it is written down BEFORE
// the snapshot is built, because the collector reads only durable rows: an
// observation that never reached the ledger is one nobody can see after a
// restart, and a stop authorized on it would be unreviewable.
//
// And every failure here fails the raise. A raise whose evidence could not be
// written is precisely the stop this whole mechanism exists to abolish — a
// conclusion with nothing under it — so it is not taken at all. The step is
// left exactly as it was and the next poll tries again; an ambiguity that is
// still true in three seconds is still true, while an unevidenced
// ambiguous_worker_state on the ledger is permanent.
func (c *Coordinator) raiseAmbiguousWorkerState(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	reason, detail string,
	observed *ObservedWorkerFacts,
) (AmbiguousWorkerState, error) {
	// The row about to be written becomes "the latest checkpoint for this
	// step". Read what that currently is, so everything below can carry it
	// forward rather than replace it. See ambiguityCheckpoint.
	var carry domain.WorkflowCheckpoint
	if step.ID != "" {
		if cp, ok, cerr := c.store.GetLatestWorkflowCheckpointByStep(ctx, step.ID); cerr == nil && ok {
			carry = cp
		}
	}
	if observed != nil && !observed.Empty() {
		if err := c.recordObservedWorkerFacts(ctx, run, step, *observed, carry); err != nil {
			return AmbiguousWorkerState{}, fmt.Errorf(
				"workflow: refusing to raise ambiguous_worker_state for step %s — its observation could not be recorded: %w",
				step.ID, err)
		}
	}
	snap := c.CollectWorkerEvidence(ctx, EvidenceRequest{Run: run, Step: step})
	raise, err := RaiseAmbiguousWorkerState(snap, reason, detail)
	if err != nil {
		return AmbiguousWorkerState{}, err
	}
	if err := c.recordAmbiguityEvidence(ctx, run, step, raise, carry); err != nil {
		return AmbiguousWorkerState{}, fmt.Errorf(
			"workflow: refusing to raise ambiguous_worker_state for step %s — its evidence snapshot could not be recorded: %w",
			step.ID, err)
	}
	return raise, nil
}

// recordObservedWorkerFacts persists one live reading so the collector never
// has to take one. Append-only, and it carries the step's identity forward for
// the same reason every other checkpoint here does.
func (c *Coordinator) recordObservedWorkerFacts(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep,
	observed ObservedWorkerFacts, carry domain.WorkflowCheckpoint,
) error {
	if observed.ObservedAt.IsZero() {
		observed.ObservedAt = c.clock()
	}
	payload, err := json.Marshal(observed)
	if err != nil {
		return err
	}
	cp := c.ambiguityCheckpoint(run, step, carry, WorkerObservationEvidencePhase,
		"recorded workspace/liveness observation", string(payload))
	// The observation's own readings win over the carried ones where it has
	// them: this row IS the newer fact about the worktree.
	if observed.WorkspaceKnown {
		cp.Branch = firstNonEmpty(observed.Branch, cp.Branch)
		cp.WorktreePath = firstNonEmpty(observed.WorktreePath, cp.WorktreePath)
		cp.HeadSHA = firstNonEmpty(observed.HeadSHA, cp.HeadSHA)
	}
	_, err = c.store.CreateWorkflowCheckpoint(ctx, cp)
	return err
}

// observedWorkerFactsFor assembles what a caller holds into the durable form,
// taking the one liveness reading AO can take here — outside the collector, and
// only on the path that is about to persist it.
//
// Returns nil when there is nothing to record, which leaves the corresponding
// snapshot fields `unavailable`. That is the honest answer: AO would rather say
// "no reading was taken" than have a collector invent one.
func (c *Coordinator) observedWorkerFactsFor(
	ctx stdctx.Context,
	sessionID domain.SessionID,
	obs *ports.WorkspaceObservation,
) *ObservedWorkerFacts {
	facts := ObservedWorkerFacts{ObservedAt: c.clock(), SessionID: string(sessionID)}
	if obs != nil {
		workspace := NewObservedWorkspaceFacts(*obs)
		workspace.ObservedAt, workspace.SessionID = facts.ObservedAt, facts.SessionID
		facts = workspace
	}
	if c.workerLiveness != nil && sessionID != "" {
		if alive, known, err := c.workerLiveness.SessionAlive(ctx, sessionID); err == nil && known {
			facts.LivenessAlive, facts.LivenessKnown = alive, true
		}
	}
	if facts.Empty() {
		return nil
	}
	return &facts
}

// recordAmbiguityEvidence writes the snapshot row, carrying the step's whole
// workspace identity forward from the checkpoint it is about to supersede.
//
// The carry is not tidiness, it is the whole correctness of this row. A
// checkpoint written against a step BECOMES "the latest checkpoint for this
// step", which is what some twenty readers resolve the worktree, the branch,
// the dispatch base, the worker session and the delivered fingerprint from —
// the same rule observeWorkStep's own checkpoint and
// recordFirstSignalReconciliation both state, and for the same reason. An
// evidence row that dropped them would not merely fail to help; it would
// DESTROY already-correct work:
//
//	observeFixStep    latestCP.SessionID == nil     -> returns early, forever
//	dispatchReviewStep  fixCP.FingerprintAfter == "" -> cycle N+1 never dispatches
//	dispatchReviewStep  workCP.WorktreePath == ""    -> "workspace path is required"
//
// A fix worker's genuinely delivered change would still be sitting in the
// worktree, with AO having thrown away the record that says it landed and
// disabled the one observer that could ever record it again.
//
// So this row is strictly ADDITIVE: same identity, plus the evidence. The only
// field it originates is the snapshot in RetryState.
//
// A failure to write it FAILS THE RAISE. It used to be logged and swallowed,
// which quietly reintroduced the exact stop this mechanism abolishes: the
// caller would go on to persist ambiguous_worker_state, and after a restart the
// Incident Advisor would find a stop with no stop-time snapshot under it — the
// unevidenced dead end, now with a mechanism that claims to prevent it.
func (c *Coordinator) recordAmbiguityEvidence(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep,
	raise AmbiguousWorkerState, carry domain.WorkflowCheckpoint,
) error {
	_, err := c.store.CreateWorkflowCheckpoint(ctx, c.ambiguityCheckpoint(
		run, step, carry, AmbiguousWorkerStateEvidencePhase, raise.Detail(), raise.Snapshot().JSON()))
	if err != nil && c.log != nil {
		c.log.Warn("workflow: recording ambiguous_worker_state evidence failed; the raise is refused",
			"run", run.ID, "step", step.ID, "reason", raise.Reason(), "err", err)
	}
	return err
}

// ambiguityCheckpoint builds a gate row, carrying the step's identity forward
// from the checkpoint it is about to supersede. Both gate rows go through it,
// so neither can drift out of step with the carry rule.
func (c *Coordinator) ambiguityCheckpoint(
	run domain.WorkflowRun, step domain.WorkflowStep, carry domain.WorkflowCheckpoint,
	phase, nextAction, payload string,
) domain.WorkflowCheckpoint {
	var stepID *string
	if step.ID != "" {
		id := step.ID
		stepID = &id
	}
	// The carried session id wins over the step row's: the fix step never gets
	// a session_id column of its own (only work dispatch writes one), so its
	// worker session lives ONLY on its checkpoints. Preferring the step row
	// here would silently drop it for every fix-step ambiguity there is.
	sessionID := carry.SessionID
	if sessionID == nil || *sessionID == "" {
		if step.SessionID != nil && *step.SessionID != "" {
			sid := *step.SessionID
			sessionID = &sid
		}
	}
	return domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: stepID,
		ProjectID:      run.ProjectID,
		SessionID:      sessionID,
		// Carried, every one of them. See the doc comment above.
		Branch:            carry.Branch,
		WorktreePath:      carry.WorktreePath,
		BaseSHA:           carry.BaseSHA,
		HeadSHA:           carry.HeadSHA,
		ReviewRunID:       carry.ReviewRunID,
		ReviewVerdict:     carry.ReviewVerdict,
		FingerprintBefore: carry.FingerprintBefore,
		FingerprintAfter:  carry.FingerprintAfter,
		NextAction:        nextAction,
		DurablePhase:      phase,
		PayloadVersion:    "v1",
		RetryState:        payload,
		CreatedAt:         c.clock(),
	}
}

// latestAmbiguityEvidence reads the newest recorded snapshot back for a run.
// The result is display evidence, never authority: DecodeWorkerEvidenceSnapshot
// deliberately does not restore the collected marker.
func (c *Coordinator) latestAmbiguityEvidence(cps []domain.WorkflowCheckpoint) (WorkerEvidenceSnapshot, bool) {
	var newest domain.WorkflowCheckpoint
	found := false
	for _, cp := range cps {
		if cp.DurablePhase != AmbiguousWorkerStateEvidencePhase {
			continue
		}
		if !found || !cp.CreatedAt.Before(newest.CreatedAt) {
			newest, found = cp, true
		}
	}
	if !found {
		return WorkerEvidenceSnapshot{}, false
	}
	return DecodeWorkerEvidenceSnapshot(newest.RetryState)
}
