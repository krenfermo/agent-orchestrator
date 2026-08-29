package tmux

// P1-D §U — provider failover against a REAL tmux server.
//
// §I says a safe failover keeps the same frozen placement: provider B inherits
// provider A's checkout, because the authority over the worktree never moved,
// only the attempt did. That is the right rule and it has a sharp edge — the
// two providers are, at some moment, both associated with one obligation, and
// the only thing separating them is the ownership token on the runtime.
//
// These tests prove that separation holds against a real tmux server rather
// than against a fake that returns whatever it was told:
//
//   - a replacement provider's session cannot be adopted by the stale attempt's
//     handle, even though both name the same AO session id;
//   - the ownership token stays EXACT across the hop — it fences on the launch,
//     not merely on the session, which is what makes "same session, new
//     provider" distinguishable from "same session, old provider";
//   - the stale provider's own runtime, once terminated, is reclaimable and its
//     reclamation does not touch the replacement.

import (
	"context"
	"os/exec"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/runtimegc"
)

// U1: after a failover the replacement runtime proves itself under the NEW
// launch, and the stale attempt's token does not satisfy it.
//
// The session id is deliberately the same on both sides. A failover keeps the
// obligation, and in AO's model it can keep the session too — so an ownership
// check that fenced only on the session id would say "yes, this is mine" to the
// stale attempt and let it act on the replacement's runtime.
func TestRealTmuxReplacementProviderRuntimeRejectsTheStaleAttemptsToken(t *testing.T) {
	requireRealTmux(t)
	r, socket := realTmuxServer(t)
	ctx := context.Background()

	// Keepalive, so tearing down the first provider's runtime does not stop the
	// server and restart tmux's `$N` counter — which would hide the ABA.
	createStrangerSession(t, socket, "keepalive")

	const sessionID = domain.SessionID("ao-failover-session")
	const attemptA = "provider-attempt-1"
	const attemptB = "provider-attempt-2"

	// Provider A launches, then fails. Its runtime is torn down.
	runtimeA := createWorkerSession(t, r, sessionID, attemptA)
	if out, err := exec.Command("tmux", "-L", socket, "kill-session", "-t", runtimeA.InstanceID).CombinedOutput(); err != nil {
		t.Fatalf("tear down provider A's runtime: %v: %s", err, out)
	}

	// Provider B takes the SAME obligation and the SAME AO session, under its
	// own launch identity.
	runtimeB := createWorkerSession(t, r, sessionID, attemptB)
	if runtimeB.InstanceID == runtimeA.InstanceID {
		t.Fatalf("tmux reused incarnation %s; the test cannot exercise the failover ABA", runtimeA.InstanceID)
	}

	factsB, exists, err := r.SessionFacts(ctx, runtimeB)
	if err != nil || !exists {
		t.Fatalf("SessionFacts(B): %v (exists=%v)", err, exists)
	}
	// The token is EXACT: it proves B's launch and refuses A's, even though the
	// session id is identical on both.
	if !domain.RuntimeOwnedBySession(factsB.Owner, sessionID, attemptB) {
		t.Fatalf("the replacement provider's runtime does not prove its own attempt: %+v", factsB)
	}
	if domain.RuntimeOwnedBySession(factsB.Owner, sessionID, attemptA) {
		t.Fatal("the stale provider attempt's token satisfied the replacement's runtime; a failover would hand A authority over B")
	}

	// And the stale HANDLE — the one A still holds — does not describe B.
	factsA, existsA, err := r.SessionFacts(ctx, runtimeA)
	if err != nil {
		t.Fatal(err)
	}
	if existsA && factsA.InstanceID == runtimeA.InstanceID {
		t.Fatal("provider A's destroyed incarnation is still reported as present")
	}
	if existsA && domain.RuntimeOwnedBySession(factsA.Owner, sessionID, attemptA) {
		t.Fatalf("provider A adopted the replacement runtime through its stale handle: %+v", factsA)
	}
}

// U2: reclaiming the stale provider's runtime does not touch the replacement's.
//
// This is the operational half of the same property. GC walks durable records,
// and after a failover there are two of them for one AO session: A's, which is
// terminated and provable, and B's, which is live. A sweep that addressed them
// by session id rather than by incarnation would kill the working provider.
func TestRealTmuxSweepingAStaleProviderRuntimeSparesTheReplacement(t *testing.T) {
	requireRealTmux(t)
	r, socket := realTmuxServer(t)
	ctx := context.Background()

	createStrangerSession(t, socket, "keepalive")

	const staleSession = domain.SessionID("ao-failover-stale")
	const liveSession = domain.SessionID("ao-failover-live")

	stale := createWorkerSession(t, r, staleSession, "attempt-1")
	live := createWorkerSession(t, r, liveSession, "attempt-2")

	sessions := &gcSessions{records: []domain.SessionRecord{
		{
			// Provider A's runtime: the attempt failed over, so the session is
			// terminated and AO can prove it created this exact incarnation.
			ID: staleSession, IsTerminated: true,
			Metadata: domain.SessionMetadata{
				RuntimeHandleID: stale.ID, RuntimeInstanceID: stale.InstanceID,
				RuntimeOwnerToken: domain.SessionRuntimeOwnerToken(staleSession, "attempt-1"),
				RuntimeLaunchID:   "attempt-1",
			},
		},
		{
			// Provider B's runtime: the successor, still running. Nothing about
			// its predecessor's reclamation may reach it.
			ID: liveSession, IsTerminated: false,
			Metadata: domain.SessionMetadata{
				RuntimeHandleID: live.ID, RuntimeInstanceID: live.InstanceID,
				RuntimeOwnerToken: domain.SessionRuntimeOwnerToken(liveSession, "attempt-2"),
				RuntimeLaunchID:   "attempt-2",
			},
		},
	}}

	sweeper := &runtimegc.Sweeper{
		Inventory: r, Facts: r, Claims: &gcClaims{},
		Runs: &gcRuns{runs: map[string]domain.WorkflowRun{}}, Sessions: sessions,
	}
	report, err := sweeper.Sweep(ctx, runtimegc.Options{Trigger: "real-tmux-failover-test"})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if instanceExists(t, socket, stale.InstanceID) {
		t.Fatal("the stale provider's terminated runtime survived the sweep")
	}
	if !instanceExists(t, socket, live.InstanceID) {
		t.Fatal("the sweep destroyed the replacement provider's live runtime")
	}
	if report.Cleaned != 1 {
		t.Fatalf("cleaned %d, want exactly the stale provider's runtime: %+v", report.Cleaned, report.Findings)
	}

	// Repeating it is inert, which is what makes a sweep safe to run on a timer
	// while a failover is in flight.
	for i := 0; i < 2; i++ {
		again, aerr := sweeper.Sweep(ctx, runtimegc.Options{Trigger: "real-tmux-failover-test"})
		if aerr != nil {
			t.Fatalf("sweep %d: %v", i+2, aerr)
		}
		if again.Cleaned != 0 {
			t.Fatalf("sweep %d cleaned %d; nothing new existed to clean", i+2, again.Cleaned)
		}
		if !instanceExists(t, socket, live.InstanceID) {
			t.Fatalf("sweep %d destroyed the replacement provider's runtime", i+2)
		}
	}
}

// U3: the incarnation-exact teardown refuses a stale incarnation, and the
// replacement survives.
//
// A word on what is NOT asserted here, because the distinction is load-bearing
// rather than a hedge. `Runtime.Destroy` is addressed by session NAME, and it
// is meant to be: it is "end the session called X", and lifecycle uses it where
// the name is precisely the identity intended. It is therefore ABA-unsafe by
// construction, and running it against a stale handle after a replacement has
// taken the same name does kill the replacement.
//
// That is why nothing on the failover or GC path uses it. Reclamation is
// `DestroyInstance`, which names tmux's immutable `$N` incarnation, and that is
// the operation this test pins: a stale provider attempt, acting on the
// incarnation it remembers, reaches nothing.
func TestRealTmuxIncarnationExactTeardownSparesTheReplacement(t *testing.T) {
	requireRealTmux(t)
	r, socket := realTmuxServer(t)
	ctx := context.Background()

	createStrangerSession(t, socket, "keepalive")

	const sessionID = domain.SessionID("ao-failover-destroy")
	stale := createWorkerSession(t, r, sessionID, "attempt-1")
	if out, err := exec.Command("tmux", "-L", socket, "kill-session", "-t", stale.InstanceID).CombinedOutput(); err != nil {
		t.Fatalf("tear down the first runtime: %v: %s", err, out)
	}
	replacement := createWorkerSession(t, r, sessionID, "attempt-2")
	if replacement.InstanceID == stale.InstanceID {
		t.Fatalf("tmux reused incarnation %s; the test cannot exercise the ABA", stale.InstanceID)
	}

	// The stale attempt, unaware it has been superseded, cleans up after
	// itself the only way anything on the failover path is allowed to: by the
	// exact incarnation it launched.
	if err := r.DestroyInstance(ctx, stale.InstanceID); err != nil {
		t.Fatalf("destroying an already-gone incarnation must be a no-op success: %v", err)
	}

	if !instanceExists(t, socket, replacement.InstanceID) {
		t.Fatal("a stale provider attempt destroyed the replacement's runtime")
	}
	facts, exists, err := r.SessionFacts(ctx, replacement)
	if err != nil || !exists {
		t.Fatalf("the replacement is gone or unreadable: %v (exists=%v)", err, exists)
	}
	if !domain.RuntimeOwnedBySession(facts.Owner, sessionID, "attempt-2") {
		t.Fatalf("the replacement no longer proves its own attempt: %+v", facts)
	}
}
