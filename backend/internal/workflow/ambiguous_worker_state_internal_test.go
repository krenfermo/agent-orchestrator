package workflow

// The invariant this file guards: AO may not say "I cannot prove what this
// worker is doing" without first collecting the evidence it CAN prove.
//
// Two kinds of test, because the invariant has two halves. The first three are
// about the value: a raise is impossible to construct without a collected
// snapshot, and the error class simply does not exist on anything else. The
// last one reads the package's own source, because "no new code path assigns
// the constant directly" is a property about the SHAPE of the package, not
// about any particular behaviour — a fourth raise site can be added in an
// afternoon and every behavioural test here would still pass.

import (
	stdctx "context"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// A snapshot that did not come from the collector cannot raise anything.
func TestRaiseAmbiguousWorkerStateRefusesAnUncollectedSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name string
		snap WorkerEvidenceSnapshot
	}{
		{"zero value", WorkerEvidenceSnapshot{}},
		{"hand-built literal", WorkerEvidenceSnapshot{
			Version: EvidenceSnapshotVersion,
			RunID:   "wf-1",
			Sections: []EvidenceSection{{Key: "workflow", Title: "Workflow run", Fields: []EvidenceField{
				{Key: "workflow.state", Label: "state", Value: "needs_attention", Status: EvidenceObserved},
			}}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raised, err := RaiseAmbiguousWorkerState(tc.snap, ReasonWorkerDispatchAmbiguous, "the worker went quiet")
			if err == nil {
				t.Fatal("an uncollected snapshot raised ambiguous_worker_state")
			}
			if !strings.Contains(err.Error(), ErrAmbiguousWithoutEvidence.Error()) {
				t.Fatalf("error = %v, want it to wrap ErrAmbiguousWithoutEvidence", err)
			}
			if got := raised.ErrorClass(); got != "" {
				t.Fatalf("refused raise still carries error class %q", got)
			}
		})
	}
}

// A serialized snapshot read back off the ledger is evidence to show, never
// authority to raise a fresh stop with. Round-tripping must not launder it.
func TestASerializedSnapshotCannotBeLaunderedIntoARaise(t *testing.T) {
	c := newEvidenceOnlyCoordinator()
	snap := c.CollectWorkerEvidence(t.Context(), EvidenceRequest{
		Run: domain.WorkflowRun{ID: "wf-1", ProjectID: "proj-1", State: domain.WorkflowRunNeedsAttention},
	})
	if !snap.Collected() {
		t.Fatal("CollectWorkerEvidence produced an uncollected snapshot")
	}
	decoded, ok := DecodeWorkerEvidenceSnapshot(snap.JSON())
	if !ok {
		t.Fatal("a collected snapshot did not round-trip through JSON")
	}
	if decoded.Collected() {
		t.Fatal("a deserialized snapshot reports itself collected: the gate can be bypassed by writing and reading a row")
	}
	if _, err := RaiseAmbiguousWorkerState(decoded, ReasonWorkerDispatchAmbiguous, "still quiet"); err == nil {
		t.Fatal("a deserialized snapshot raised ambiguous_worker_state")
	}
}

// The guard every durable write passes through refuses a class with no raise
// behind it, whatever the class arrived from.
func TestAssertAmbiguousEvidenceRefusesAClassWithNoRaise(t *testing.T) {
	if err := assertAmbiguousEvidence(domain.WorkflowErrorAmbiguousWorkerState, AmbiguousWorkerState{}); err == nil {
		t.Fatal("ambiguous_worker_state passed the guard with no evidence behind it")
	}
	if err := assertAmbiguousEvidence(domain.WorkflowErrorAgentStartFailed, AmbiguousWorkerState{}); err != nil {
		t.Fatalf("the guard refused an unrelated error class: %v", err)
	}

	c := newEvidenceOnlyCoordinator()
	snap := c.CollectWorkerEvidence(t.Context(), EvidenceRequest{Run: domain.WorkflowRun{ID: "wf-1"}})
	raised, err := RaiseAmbiguousWorkerState(snap, ReasonWorkerDispatchAmbiguous, "the worker went quiet")
	if err != nil {
		t.Fatalf("RaiseAmbiguousWorkerState: %v", err)
	}
	if raised.ErrorClass() != domain.WorkflowErrorAmbiguousWorkerState {
		t.Fatalf("a collected raise yielded error class %q", raised.ErrorClass())
	}
	if err := assertAmbiguousEvidence(raised.ErrorClass(), raised); err != nil {
		t.Fatalf("the guard refused a properly evidenced raise: %v", err)
	}
	// And a raise with no sentence a person can read is refused too: a stop
	// nobody can read is the failure the gate exists to remove.
	if _, err := RaiseAmbiguousWorkerState(snap, ReasonWorkerDispatchAmbiguous, "   "); err == nil {
		t.Fatal("a raise with an empty detail was accepted")
	}
}

// ambiguousClassAllowList names the only non-test files in this package allowed
// to mention the error class at all.
//
//   - ambiguous_worker_state.go IS the gate.
//   - attention.go maps the class to display text; it raises nothing.
//   - failure_classifier.go returns the class as a CLASSIFICATION of a provider
//     failure it could not identify. Whatever consumes that classification and
//     wants to make it durable must still go through the gate, which is exactly
//     what assertAmbiguousEvidence is for.
var ambiguousClassAllowList = map[string]string{
	"ambiguous_worker_state.go": "the gate itself",
	"attention.go":              "display mapping only",
	"failure_classifier.go":     "classification, not a raise",
}

// Every ambiguous_worker_state stop goes through the gate. A new raise site
// that assigns the constant directly fails here, by name.
func TestAmbiguousWorkerStateIsOnlyRaisedThroughTheGate(t *testing.T) {
	const constant = "WorkflowErrorAmbiguousWorkerState"
	for name, body := range workflowSources(t) {
		if !strings.Contains(body, constant) {
			continue
		}
		if _, ok := ambiguousClassAllowList[name]; ok {
			continue
		}
		t.Errorf("%s names %s directly; ambiguous_worker_state may only be produced by "+
			"AmbiguousWorkerState.ErrorClass(), which requires a collected evidence snapshot "+
			"(see raiseAmbiguousWorkerState in ambiguous_worker_state.go)", name, constant)
	}
}

// The collector reads durable rows and nothing else. That is not a style
// preference: a live reading is a fact only this process ever held, so a
// snapshot built on one cannot be shown to anyone after a restart and cannot be
// checked by the person reading the stop it authorized. The two live readings
// AO can take are taken by the caller and persisted BEFORE the raise (see
// ObservedWorkerFacts); the collector must never reach for them itself.
//
// Guarded structurally, because the failure mode is the EXISTENCE of a port
// call in this file, not anything a behavioural test would observe: a probe
// added here would return the right answer in-process every time.
func TestTheEvidenceCollectorTakesNoLiveReadings(t *testing.T) {
	body, ok := workflowSources(t)["evidence_snapshot.go"]
	if !ok {
		t.Fatal("evidence_snapshot.go is gone")
	}
	for _, live := range []struct{ call, why string }{
		{"c.workspaceFacts", "the collector must not observe a worktree; the caller persists its observation first"},
		{"c.workerLiveness", "the collector must not probe a runtime; the caller persists the probe's answer first"},
		{"ObserveWorkspace(", "a git observation taken here would be a fact only this process ever held"},
		{"SessionAlive(", "a liveness probe taken here would be a fact only this process ever held"},
	} {
		if strings.Contains(body, live.call) {
			t.Errorf("evidence_snapshot.go calls %s: %s", live.call, live.why)
		}
	}
}

// newEvidenceOnlyCoordinator is the smallest Coordinator the collector needs:
// a store and a clock, nothing else wired. It is deliberately bare, because the
// collector's contract is that every unwired port becomes an `unavailable`
// field rather than an error or an invented fact.
func newEvidenceOnlyCoordinator() *Coordinator {
	return New(Deps{Store: evidenceNullStore{}})
}

// evidenceNullStore embeds the Store interface so it satisfies it in full while
// implementing only the four reads the collector actually performs. Anything
// else would panic loudly, which is the point: it documents the collector's
// read surface by making any widening of it fail immediately.
type evidenceNullStore struct{ Store }

func (evidenceNullStore) ListWorkflowCheckpoints(stdctx.Context, string) ([]domain.WorkflowCheckpoint, error) {
	return nil, nil
}

func (evidenceNullStore) GetLatestWorkflowAttempt(stdctx.Context, string) (domain.WorkflowAttempt, bool, error) {
	return domain.WorkflowAttempt{}, false, nil
}

func (evidenceNullStore) ListWorkflowSteps(stdctx.Context, string) ([]domain.WorkflowStep, error) {
	return nil, nil
}

func (evidenceNullStore) GetWorkflowRun(stdctx.Context, string) (domain.WorkflowRun, bool, error) {
	return domain.WorkflowRun{}, false, nil
}
