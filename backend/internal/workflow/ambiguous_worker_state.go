package workflow

import (
	stdctx "context"
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

// raiseAmbiguousWorkerState collects the evidence, raises the ambiguity and
// records the snapshot on the run's ledger — in that order, before any caller
// is given a value it can persist.
//
// Every ambiguous_worker_state stop in AO starts here. The order matters for
// the same reason recordFixDispatchIntent's does: a stop AO could not first
// write the evidence for is a stop AO does not take, so a daemon that dies
// between the two leaves a readable evidence row and no unexplained stop,
// rather than the reverse.
func (c *Coordinator) raiseAmbiguousWorkerState(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	reason, detail string,
	obs *ports.WorkspaceObservation,
) (AmbiguousWorkerState, error) {
	snap := c.CollectWorkerEvidence(ctx, EvidenceRequest{Run: run, Step: step, Observation: obs})
	raise, err := RaiseAmbiguousWorkerState(snap, reason, detail)
	if err != nil {
		return AmbiguousWorkerState{}, err
	}
	c.recordAmbiguityEvidence(ctx, run, step, raise)
	return raise, nil
}

// recordAmbiguityEvidence writes the snapshot row. A failure to write it is
// logged and does not block the stop: the alternative — leaving a run running
// on a state AO has already decided it cannot read — is worse than an
// unexplained stop, and the stop's own checkpoint still lands.
func (c *Coordinator) recordAmbiguityEvidence(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, raise AmbiguousWorkerState,
) {
	var stepID *string
	if step.ID != "" {
		id := step.ID
		stepID = &id
	}
	var sessionID *string
	if step.SessionID != nil && *step.SessionID != "" {
		sid := *step.SessionID
		sessionID = &sid
	}
	snap := raise.Snapshot()
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: stepID,
		ProjectID:      run.ProjectID,
		SessionID:      sessionID,
		NextAction:     raise.Detail(),
		DurablePhase:   AmbiguousWorkerStateEvidencePhase,
		PayloadVersion: "v1",
		RetryState:     snap.JSON(),
		CreatedAt:      c.clock(),
	}); err != nil && c.log != nil {
		c.log.Warn("workflow: recording ambiguous_worker_state evidence failed",
			"run", run.ID, "step", step.ID, "reason", raise.Reason(), "err", err)
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
