package review

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// A review pass that fails durably is not something AO retries, so it is one of
// the few session-level facts worth a person's attention. The notification is
// made FROM the failed review_run row, after it is written, and keyed on that
// row's id -- so re-reading it can never raise a second one.

type recordingFacts struct {
	facts []ports.SessionFact
	err   error
}

func (r *recordingFacts) Record(_ context.Context, fact ports.SessionFact) error {
	r.facts = append(r.facts, fact)
	return r.err
}

func newEngineWithFacts(store Store, launcher Launcher, facts ports.SessionNotifier) *Engine {
	ids := 0
	return New(Deps{
		Store: store, Sessions: fakeSessions{rec: liveWorker(), ok: true},
		PRs: prAt("sha1"), Projects: fakeProjects{}, Launcher: launcher,
		SessionFacts: facts,
		Clock:        func() time.Time { return time.Unix(0, 0).UTC() },
		NewID:        func() string { ids++; return "id-" + string(rune('0'+ids)) },
	})
}

func TestFailedReviewRunRaisesIntegrationFailed(t *testing.T) {
	store := &fakeStore{}
	facts := &recordingFacts{}
	launcher := &fakeLauncher{spawnErr: fmt.Errorf("claude: %w", ports.ErrAgentBinaryNotFound)}
	eng := newEngineWithFacts(store, launcher, facts)

	if _, err := eng.Trigger(context.Background(), "mer-1", ""); !errors.Is(err, ports.ErrAgentBinaryNotFound) {
		t.Fatalf("err = %v, want ports.ErrAgentBinaryNotFound", err)
	}
	if len(facts.facts) != 1 {
		t.Fatalf("facts = %d, want 1 (%+v)", len(facts.facts), facts.facts)
	}
	fact := facts.facts[0]
	if fact.Kind != ports.SessionFactIntegrationFailed {
		t.Fatalf("Kind = %q, want integration_failed", fact.Kind)
	}
	if fact.SessionID != "mer-1" {
		t.Fatalf("SessionID = %q, want the worker", fact.SessionID)
	}
	// The failed run's id is the failure's durable identity.
	if fact.ScopeID != store.runs[0].ID {
		t.Fatalf("ScopeID = %q, want the failed review run id %q", fact.ScopeID, store.runs[0].ID)
	}
	if !strings.Contains(fact.Detail, ports.ErrAgentBinaryNotFound.Error()) {
		t.Fatalf("Detail = %q, want the cause", fact.Detail)
	}
}

// P4-D section 9: a notification carries a concise summary, never a transcript
// or raw provider output.
func TestIntegrationFailureDetailIsBounded(t *testing.T) {
	store := &fakeStore{}
	facts := &recordingFacts{}
	launcher := &fakeLauncher{spawnErr: errors.New(strings.Repeat("provider noise ", 500))}
	eng := newEngineWithFacts(store, launcher, facts)

	_, _ = eng.Trigger(context.Background(), "mer-1", "")
	if len(facts.facts) != 1 {
		t.Fatalf("facts = %d, want 1", len(facts.facts))
	}
	if got := len(facts.facts[0].Detail); got > integrationDetailLimit+3 {
		t.Fatalf("Detail length = %d, want it bounded to %d", got, integrationDetailLimit)
	}
}

// A successful trigger is not an integration failure.
func TestSuccessfulReviewRaisesNoFact(t *testing.T) {
	store := &fakeStore{}
	facts := &recordingFacts{}
	eng := newEngineWithFacts(store, &fakeLauncher{handle: "review-mer-1"}, facts)

	if _, err := eng.Trigger(context.Background(), "mer-1", ""); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if len(facts.facts) != 0 {
		t.Fatalf("a successful review raised %d facts, want 0", len(facts.facts))
	}
}

// The run is already recorded failed; a notification sink that is down must not
// turn that into a different trigger error.
func TestFactSinkFailureDoesNotChangeTheTriggerError(t *testing.T) {
	store := &fakeStore{}
	facts := &recordingFacts{err: errors.New("notification store is down")}
	launcher := &fakeLauncher{spawnErr: fmt.Errorf("claude: %w", ports.ErrAgentBinaryNotFound)}
	eng := newEngineWithFacts(store, launcher, facts)

	if _, err := eng.Trigger(context.Background(), "mer-1", ""); !errors.Is(err, ports.ErrAgentBinaryNotFound) {
		t.Fatalf("err = %v, want the launch cause unchanged", err)
	}
	if len(store.runs) != 1 || store.runs[0].Status != "failed" {
		t.Fatalf("the failed run was lost when the notification sink failed: %+v", store.runs)
	}
}

// Without a sink the engine still records the failure; only the notification is
// skipped.
func TestNoFactSinkStillRecordsTheFailedRun(t *testing.T) {
	store := &fakeStore{}
	launcher := &fakeLauncher{spawnErr: fmt.Errorf("claude: %w", ports.ErrAgentBinaryNotFound)}
	eng := newEngineWithFacts(store, launcher, nil)

	if _, err := eng.Trigger(context.Background(), "mer-1", ""); !errors.Is(err, ports.ErrAgentBinaryNotFound) {
		t.Fatalf("err = %v, want ports.ErrAgentBinaryNotFound", err)
	}
	if len(store.runs) != 1 {
		t.Fatalf("runs = %d, want the failure still recorded", len(store.runs))
	}
}
