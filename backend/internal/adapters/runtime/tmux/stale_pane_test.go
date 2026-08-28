package tmux

// A pane that will not name its process.
//
// Production evidence (AO daemon, wf-9405592d): tmux answered `#{pane_pid}`
// with an EMPTY string for session instance $34, and the adapter raised
//
//	tmux runtime: invalid pane pid "" for instance $34
//
// as an error. That error reached boot reconciliation, the wake poller, and an
// ordinary workflow GET -- so one malformed pane fact stopped every unrelated
// run from recovering, spun the wake scheduler all night, and turned
// GET /api/v1/workflows/{id} into a repeating 500.
//
// The rule these tests pin: an unreadable pane pid is an UNTRUSTED OBSERVATION,
// not a failure and not a death certificate. SessionFacts reports the session
// with liveness UNKNOWN, which licenses nothing downstream -- no adoption, no
// termination, no fabricated pid.

import (
	"context"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func panePIDText(s string) *string { return &s }

// (a) THE PRODUCTION SHAPE: the pane exists and answers with an empty pid.
func TestSessionFacts_EmptyPanePIDIsUnknownLivenessNotAnError(t *testing.T) {
	r, st := newScriptedRuntime()
	createReviewer(t, r, st, "rev-empty-pid")
	st.panePIDText = panePIDText("")

	facts, exists, err := r.SessionFacts(context.Background(), ports.RuntimeHandle{ID: "rev-empty-pid"})
	if err != nil {
		t.Fatalf("SessionFacts returned an error for an empty pane pid: %v", err)
	}
	if !exists {
		t.Fatal("an unreadable pane pid was reported as proof the session is gone")
	}
	if !facts.OwnerKnown || facts.Owner != "ao-reviewer:rev-empty-pid" {
		t.Fatalf("ownership was lost with the pane pid: owner=(%q,%t)", facts.Owner, facts.OwnerKnown)
	}
	if facts.WorkloadKnown {
		t.Fatalf("liveness was claimed as KNOWN (%t) from a pane that named no process", facts.WorkloadAlive)
	}
	if facts.WorkloadAlive {
		t.Fatal("a pane with no readable pid was reported alive")
	}
	if facts.InstanceID == "" {
		t.Fatal("the observation lost the incarnation it is about")
	}
}

// (b) A MALFORMED pid is the same class of answer as an empty one: tmux replied,
// with something AO cannot use. It must not be parsed, guessed at, or raised.
func TestSessionFacts_MalformedPanePIDIsUnknownLivenessNotAnError(t *testing.T) {
	for _, raw := range []string{"not-a-pid", "0", "-1", "   "} {
		r, st := newScriptedRuntime()
		createReviewer(t, r, st, "rev-bad-pid")
		st.panePIDText = panePIDText(raw)

		facts, exists, err := r.SessionFacts(context.Background(), ports.RuntimeHandle{ID: "rev-bad-pid"})
		if err != nil {
			t.Fatalf("pane pid %q: SessionFacts errored: %v", raw, err)
		}
		if !exists {
			t.Fatalf("pane pid %q: reported as a session that does not exist", raw)
		}
		if facts.WorkloadKnown {
			t.Fatalf("pane pid %q: liveness reported as known", raw)
		}
	}
}

// A pane pid AO cannot read must never be presented as a pid. Nothing
// downstream may receive a fabricated one.
func TestInstancePanePID_UnreadableNeverFabricatesAPID(t *testing.T) {
	r, st := newScriptedRuntime()
	createReviewer(t, r, st, "rev-no-fab")
	st.panePIDText = panePIDText("")

	pid, status, err := r.instancePanePID(context.Background(), st.sessions["rev-no-fab"].instanceID)
	if err != nil {
		t.Fatalf("instancePanePID: %v", err)
	}
	if status != panePIDUnreadable {
		t.Fatalf("status = %v, want panePIDUnreadable", status)
	}
	if pid != 0 {
		t.Fatalf("pid = %d, want no pid at all", pid)
	}
}

// A genuinely UNREACHABLE server is still an error: that is not a fact about the
// pane, it is AO being unable to ask at all. Degrading it to "unknown liveness"
// would let a dead tmux server read as a live-but-unclassifiable session.
func TestSessionFacts_UnreachableServerRemainsAnError(t *testing.T) {
	r, st := newScriptedRuntime()
	createReviewer(t, r, st, "rev-down")
	st.serverDown = true

	if _, _, err := r.SessionFacts(context.Background(), ports.RuntimeHandle{ID: "rev-down"}); err == nil {
		t.Fatal("an unreachable tmux server was reported as an ordinary observation")
	}
}

// (c) A STALE INSTANCE FROM AN OLDER GENERATION. The handle names an incarnation
// that no longer exists; a different session holds its former name. The
// unreadable-pid path must not turn that into facts about the replacement --
// absence is still proven absence.
func TestSessionFacts_StaleInstanceFromAnOlderGenerationIsAbsent(t *testing.T) {
	r, st := newScriptedRuntime()
	original := createReviewer(t, r, st, "rev-generation")
	staleInstance := original.instanceID

	// The generation turns over: the original is gone, a new session holds the
	// name, and its pane will not report a pid either.
	st.replaceSession("rev-generation", "ao-reviewer:rev-generation")
	st.panePIDText = panePIDText("")

	facts, exists, err := r.SessionFacts(context.Background(),
		ports.RuntimeHandle{ID: "rev-generation", InstanceID: staleInstance})
	if err != nil {
		t.Fatalf("SessionFacts: %v", err)
	}
	if exists {
		t.Fatalf("facts were returned for an incarnation that no longer exists: %+v", facts)
	}
	if facts.InstanceID != "" {
		t.Fatalf("the replacement answered for the stale instance: %+v", facts)
	}
}

// The supervised-process probes keep their own contract: they answer a narrower
// question (is THIS launch still running) and an unreadable pane pid is an
// inconclusive probe there, which their callers already treat as uncertainty.
// Pinned so the SessionFacts change above is not read as a licence to soften it.
func TestSupervisedProbe_InvalidPanePIDStaysInconclusive(t *testing.T) {
	r, fr := newTestRuntime(0)
	fr.outputs = [][]byte{[]byte("\n")}

	_, err := r.IsExactSupervisedProcessAlive(context.Background(),
		ports.RuntimeHandle{ID: "sess-1"}, ports.SupervisedProcessRef{SessionID: "sess-1", LaunchID: "l-1"})
	if err == nil {
		t.Fatal("an unreadable pane pid was answered as a definite liveness verdict")
	}
	if !strings.Contains(err.Error(), "invalid pane pid") {
		t.Fatalf("error = %v, want it to name the unreadable pane pid", err)
	}
}
