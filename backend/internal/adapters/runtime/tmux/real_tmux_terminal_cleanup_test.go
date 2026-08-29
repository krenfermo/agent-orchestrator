package tmux

// Terminal runtime reclamation, proven against a REAL tmux server.
//
// The policy is tested in internal/workflow, the proofs in internal/runtimegc,
// and the durable seam in internal/daemon — all three against fakes. This file
// proves the one thing none of them can: that when AO decides a finished
// workflow's runtime may end, a real tmux session with a real agent process in
// it actually goes away, and that the exact-incarnation discipline really
// spares a real replacement that has taken the same name.
//
// Every test runs on its OWN isolated tmux server (Options.Socket). Nothing
// here can see, adopt or kill anything on the operator's default server, which
// is the same rule the rest of the real-tmux suite obeys and for the same
// reason: a test that can reach a person's sessions will eventually kill one.

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/runtimegc"
)

// terminalReclaimRequest is the request a terminal workflow's reclamation
// produces, built from the durable identity a session row holds.
func terminalReclaimRequest(session, launch, instance string) runtimegc.ReclaimRequest {
	return runtimegc.ReclaimRequest{
		SessionID:     session,
		LaunchID:      launch,
		Handle:        session,
		InstanceID:    instance,
		OwnerToken:    domain.SessionRuntimeOwnerToken(domain.SessionID(session), launch),
		WorkflowRunID: "wf-terminal",
		Reason:        "the workflow completed",
	}
}

// The whole §J sequence in one test, in order, because the ordering IS the
// property: a stale cleanup is only dangerous after a replacement exists.
func TestRealTmuxTerminalCleanupEndsExactIncarnationAndSparesReplacement(t *testing.T) {
	requireRealTmux(t)
	r, socket := realTmuxServer(t)
	ctx := context.Background()

	const (
		name    = "agent-orchestrator-terminal-1"
		launch1 = "launch-1"
		launch2 = "launch-2"
	)
	sweeper := gcSweeper(r, &gcClaims{}, &gcRuns{runs: map[string]domain.WorkflowRun{}})

	// A second AO session, untouched throughout. It keeps the tmux SERVER alive
	// across the reclamations below, which is what makes the incarnation
	// comparison meaningful: tmux restarts `$N` from zero when its server exits
	// and is respawned, so on an otherwise-empty server the "replacement" would
	// reuse the destroyed id and this test would prove nothing. A machine
	// actually running AO always has other sessions, so this is also the
	// realistic shape.
	const bystander = "agent-orchestrator-bystander"
	bystanderInstance := createOwnedSession(t, r, bystander, domain.SessionRuntimeOwnerToken(bystander, launch1))

	// 1-2. AO creates a worker runtime carrying the ownership marker its
	// session row records. This is the production Create path: the marker is
	// attached AS PART OF new-session, never written afterwards.
	first := createOwnedSession(t, r, name, domain.SessionRuntimeOwnerToken(name, launch1))
	if !instanceExists(t, socket, first) {
		t.Fatalf("the created incarnation %s is not on the server", first)
	}

	// 3-4. The workflow goes terminal and its runtime is reclaimed. The destroy
	// is addressed to the incarnation, and the incarnation is what disappears.
	f, err := sweeper.ReclaimSessionRuntime(ctx, terminalReclaimRequest(name, launch1, first))
	if err != nil {
		t.Fatal(err)
	}
	if f.Disposition != runtimegc.DispositionCleaned {
		t.Fatalf("disposition = %s (%s), want cleaned", f.Disposition, f.Reason)
	}
	if instanceExists(t, socket, first) {
		t.Fatalf("incarnation %s survived a terminal reclamation", first)
	}

	// 5. The same session NAME is reused by a new launch, which on a real tmux
	// server means a genuinely new `$N`.
	second := createOwnedSession(t, r, name, domain.SessionRuntimeOwnerToken(name, launch2))
	if second == first {
		t.Fatalf("tmux reused incarnation %s; this test proves nothing without a new one", second)
	}

	// 6-7. The stale terminal cleanup replays, still naming the OLD
	// incarnation. The replacement must survive — this is the ABA the whole
	// model exists to exclude, against a real server rather than a fake.
	stale, err := sweeper.ReclaimSessionRuntime(ctx, terminalReclaimRequest(name, launch1, first))
	if err != nil {
		t.Fatal(err)
	}
	if stale.Disposition == runtimegc.DispositionCleaned {
		t.Fatal("a replayed stale cleanup reported destroying something")
	}
	if !instanceExists(t, socket, second) {
		t.Fatalf("the live replacement %s was destroyed by a stale cleanup naming %s", second, first)
	}

	// 8. Repeatedly. A reconciliation loop replays this on every pass, so
	// "converges" has to mean "still converges the tenth time".
	for i := 0; i < 5; i++ {
		if _, err := sweeper.ReclaimSessionRuntime(ctx, terminalReclaimRequest(name, launch1, first)); err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
		if !instanceExists(t, socket, second) {
			t.Fatalf("replay %d destroyed the live replacement", i)
		}
	}

	// And the replacement is itself reclaimable, on its own launch's proof —
	// so sparing it was a refusal about identity, never a permanent immunity.
	final, err := sweeper.ReclaimSessionRuntime(ctx, terminalReclaimRequest(name, launch2, second))
	if err != nil {
		t.Fatal(err)
	}
	if final.Disposition != runtimegc.DispositionCleaned {
		t.Fatalf("the replacement was %s (%s), want cleaned under its own launch's proof",
			final.Disposition, final.Reason)
	}
	if instanceExists(t, socket, second) {
		t.Fatal("the replacement survived its own terminal reclamation")
	}

	// Nothing in any of that touched the session no terminal workflow named.
	if !instanceExists(t, socket, bystanderInstance) {
		t.Fatal("an unrelated AO session was destroyed by another run's terminal cleanup")
	}
}

// A session AO did not create is never ended by a terminal workflow, however
// convincingly its name matches. This is the operator-safety half, against a
// real session made with the tmux CLI directly.
func TestRealTmuxTerminalCleanupNeverEndsAStrangersSession(t *testing.T) {
	requireRealTmux(t)
	r, socket := realTmuxServer(t)
	ctx := context.Background()

	const name = "agent-orchestrator-terminal-2"
	createStrangerSession(t, socket, name)

	sessions, err := r.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("inventory = %+v, want the one stranger session", sessions)
	}
	stranger := sessions[0]
	if stranger.OwnerKnown {
		t.Fatal("a session created outside AO reported an ownership token")
	}

	// A terminal workflow naming that incarnation, with a token AO would have
	// used had it created the session. Refused: the live runtime carries no
	// marker, so the two records disagree and unknown is not owned.
	f, err := gcSweeper(r, &gcClaims{}, &gcRuns{runs: map[string]domain.WorkflowRun{}}).
		ReclaimSessionRuntime(ctx, terminalReclaimRequest(name, "launch-1", stranger.InstanceID))
	if err != nil {
		t.Fatal(err)
	}
	if f.Disposition == runtimegc.DispositionCleaned {
		t.Fatal("a terminal workflow destroyed a session AO did not create")
	}
	if !instanceExists(t, socket, stranger.InstanceID) {
		t.Fatal("the stranger's session was destroyed")
	}
	if f.RecommendedAction == "" {
		t.Fatal("a refusal AO will never revisit carries no operator recommendation")
	}
}

// A legacy AO session — one created before ownership markers existed — is left
// exactly where it is, and is reported so it does not become an orphan nobody
// knows about.
func TestRealTmuxTerminalCleanupLeavesLegacySessionsAlone(t *testing.T) {
	requireRealTmux(t)
	r, socket := realTmuxServer(t)
	ctx := context.Background()

	const name = "agent-orchestrator-legacy-1"
	// No owner: exactly what `show-environment AO_SESSION_OWNER` reported for
	// every one of the twenty-five sessions found in production.
	legacy := createOwnedSession(t, r, name, "")

	f, err := gcSweeper(r, &gcClaims{}, &gcRuns{runs: map[string]domain.WorkflowRun{}}).
		ReclaimSessionRuntime(ctx, runtimegc.ReclaimRequest{
			SessionID: name, LaunchID: "launch-1", Handle: name,
			InstanceID: legacy, OwnerToken: "", // the row records none either
			WorkflowRunID: "wf-old", Reason: "the workflow completed",
		})
	if err != nil {
		t.Fatal(err)
	}
	if f.Disposition != runtimegc.DispositionUnprovable {
		t.Fatalf("disposition = %s, want unprovable", f.Disposition)
	}
	if !instanceExists(t, socket, legacy) {
		t.Fatal("a legacy session with no ownership proof was destroyed")
	}

	// A full sweep reaches the same verdict and reports it.
	report, err := gcSweeper(r, &gcClaims{}, &gcRuns{runs: map[string]domain.WorkflowRun{}}).
		Sweep(ctx, runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Cleaned != 0 {
		t.Fatalf("the sweep reclaimed %d legacy runtimes", report.Cleaned)
	}
	if !instanceExists(t, socket, legacy) {
		t.Fatal("the sweep destroyed a legacy session")
	}
	if report.SkippedUnprovable != 1 {
		t.Fatalf("unprovable = %d, want the legacy session reported", report.SkippedUnprovable)
	}
}
