package tmux

// P1-D SECTION AD — worker runtime ownership, proven against a REAL tmux server.
//
// P1-C could reclaim a finished REVIEWER pane and not a finished WORKER one:
// reviewers attached an ownership token at creation and workers did not, so a
// worker's tmux session was provably AO's only when a capacity claim happened
// to name it. Everything else was reported "unprovable" and left on the machine
// forever. P1-D §C attaches the same kind of token to every worker launch.
//
// These tests prove the property that makes it worth anything: the token names
// the SESSION AND THE LAUNCH, so a stale handle from an earlier launch cannot
// adopt — or destroy — the runtime a later launch created.

import (
	"context"
	"os/exec"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/runtimegc"
)

// createWorkerSession creates a session the way session_manager now does: with
// an ownership token bound to the session id and the launch id.
func createWorkerSession(t *testing.T, r *Runtime, sessionID domain.SessionID, launchID string) ports.RuntimeHandle {
	t.Helper()
	handle, err := r.Create(context.Background(), ports.RuntimeConfig{
		SessionID:     sessionID,
		WorkspacePath: t.TempDir(),
		Argv:          []string{"sh", "-c", "sleep 300"},
		Owner:         domain.SessionRuntimeOwnerToken(sessionID, launchID),
	})
	if err != nil {
		t.Fatalf("create worker session %s: %v", sessionID, err)
	}
	return handle
}

// AD1: a worker session exposes ownership proof, and it names both the session
// and the launch.
func TestRealTmuxWorkerSessionCarriesOwnershipProof(t *testing.T) {
	requireRealTmux(t)
	r, _ := realTmuxServer(t)
	ctx := context.Background()

	const sessionID = domain.SessionID("ao-worker-own-1")
	handle := createWorkerSession(t, r, sessionID, "launch-A")

	facts, exists, err := r.SessionFacts(ctx, handle)
	if err != nil || !exists {
		t.Fatalf("SessionFacts: %v (exists=%v)", err, exists)
	}
	if !facts.OwnerKnown {
		t.Fatal("a worker session created by AO reported no ownership token; P1-C's gap is still open")
	}
	if !domain.RuntimeOwnedBySession(facts.Owner, sessionID, "launch-A") {
		t.Fatalf("token %q does not prove this session/launch", facts.Owner)
	}
	// The proof is strict: the SAME session under a DIFFERENT launch does not
	// match. That is what stops a stale handle adopting a replacement.
	if domain.RuntimeOwnedBySession(facts.Owner, sessionID, "launch-B") {
		t.Fatal("a token from launch A satisfied launch B; the launch fence is not binding")
	}
	if domain.RuntimeOwnedBySession(facts.Owner, "ao-worker-own-2", "launch-A") {
		t.Fatal("a token from one session satisfied another")
	}
}

// AD2/AD3: the exact incarnation survives recreation, and a stale handle
// cannot adopt the replacement.
func TestRealTmuxStaleWorkerHandleCannotAdoptAReplacement(t *testing.T) {
	requireRealTmux(t)
	r, socket := realTmuxServer(t)
	ctx := context.Background()

	// Keepalive, so killing the first incarnation does not tear the server
	// down and restart tmux's `$N` counter.
	createStrangerSession(t, socket, "keepalive")

	const sessionID = domain.SessionID("ao-worker-own-aba")
	first := createWorkerSession(t, r, sessionID, "launch-1")
	if out, err := exec.Command("tmux", "-L", socket, "kill-session", "-t", first.InstanceID).CombinedOutput(); err != nil {
		t.Fatalf("kill first incarnation: %v: %s", err, out)
	}
	second := createWorkerSession(t, r, sessionID, "launch-2")
	if second.InstanceID == first.InstanceID {
		t.Fatalf("tmux reused incarnation %s; the test cannot exercise the ABA", first.InstanceID)
	}

	// The stale handle names the OLD incarnation. Facts addressed to it must
	// not describe the replacement as if it were the same runtime.
	facts, exists, err := r.SessionFacts(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if exists && facts.InstanceID == first.InstanceID {
		t.Fatal("the destroyed incarnation is still reported as present")
	}
	if exists && domain.RuntimeOwnedBySession(facts.Owner, sessionID, "launch-1") {
		t.Fatalf("the replacement satisfied the stale launch's ownership token: %+v", facts)
	}
	// And the replacement proves itself under its OWN launch.
	current, exists, err := r.SessionFacts(ctx, second)
	if err != nil || !exists {
		t.Fatalf("SessionFacts(second): %v (exists=%v)", err, exists)
	}
	if !domain.RuntimeOwnedBySession(current.Owner, sessionID, "launch-2") {
		t.Fatalf("the replacement does not prove its own launch: %+v", current)
	}
}

// AD4/AD5: GC can now reclaim a terminated worker it can prove it created, and
// still leaves a legacy worker (no token) exactly where it is.
func TestRealTmuxGCReclaimsProvenWorkerAndSparesLegacy(t *testing.T) {
	requireRealTmux(t)
	r, socket := realTmuxServer(t)
	ctx := context.Background()

	const provenID = domain.SessionID("ao-worker-proven")
	proven := createWorkerSession(t, r, provenID, "launch-9")

	// A legacy worker: created by AO before P1-D, so it carries no token. Its
	// session row records no incarnation and no token either.
	legacy, err := r.Create(ctx, ports.RuntimeConfig{
		SessionID: "ao-worker-legacy", WorkspacePath: t.TempDir(),
		Argv: []string{"sh", "-c", "sleep 300"},
	})
	if err != nil {
		t.Fatalf("create legacy worker: %v", err)
	}

	sessions := &gcSessions{records: []domain.SessionRecord{
		{
			ID: provenID, IsTerminated: true,
			Metadata: domain.SessionMetadata{
				RuntimeHandleID: proven.ID, RuntimeInstanceID: proven.InstanceID,
				RuntimeOwnerToken: domain.SessionRuntimeOwnerToken(provenID, "launch-9"),
				RuntimeLaunchID:   "launch-9",
			},
		},
		{
			// Legacy: terminated, but nothing AO recorded can address or
			// attribute its runtime.
			ID: "ao-worker-legacy", IsTerminated: true,
			Metadata: domain.SessionMetadata{RuntimeHandleID: legacy.ID},
		},
	}}

	sweeper := &runtimegc.Sweeper{
		Inventory: r, Facts: r, Claims: &gcClaims{},
		Runs: &gcRuns{runs: map[string]domain.WorkflowRun{}}, Sessions: sessions,
	}
	report, err := sweeper.Sweep(ctx, runtimegc.Options{Trigger: "real-tmux-test"})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if instanceExists(t, socket, proven.InstanceID) {
		t.Fatal("a terminated worker AO could prove it created survived the sweep")
	}
	if !instanceExists(t, socket, legacy.InstanceID) {
		t.Fatal("a legacy worker with no ownership proof was destroyed; unprovable must stay untouched")
	}
	if report.Cleaned != 1 {
		t.Fatalf("cleaned %d, want exactly 1: %+v", report.Cleaned, report.Findings)
	}
	if report.SkippedUnprovable != 1 {
		t.Fatalf("skippedUnprovable = %d, want 1 (the legacy worker): %+v", report.SkippedUnprovable, report.Findings)
	}

	// AD6: repeated sweeps stay idempotent and keep reporting the legacy one.
	for i := 0; i < 3; i++ {
		again, aerr := sweeper.Sweep(ctx, runtimegc.Options{Trigger: "real-tmux-test"})
		if aerr != nil {
			t.Fatalf("sweep %d: %v", i+2, aerr)
		}
		if again.Cleaned != 0 {
			t.Fatalf("sweep %d cleaned %d; nothing new existed to clean", i+2, again.Cleaned)
		}
		if again.SkippedUnprovable != 1 {
			t.Fatalf("sweep %d stopped reporting the legacy worker: %+v", i+2, again)
		}
	}
}

// gcSessions is the durable session half for these tests.
type gcSessions struct{ records []domain.SessionRecord }

func (s *gcSessions) ListAllSessions(context.Context) ([]domain.SessionRecord, error) {
	return s.records, nil
}
