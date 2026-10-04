package tmux

// P0-C SECTION C — the lifecycle proven against a REAL tmux server.
//
// Everything else in this package either scripts tmux (fakeRunner) or exercises
// one narrow real behaviour. This file is the end-to-end evidence the P0-C bar
// asks for: an actual `tmux` binary, actual sessions, actual panes, and the
// exact adoption/rejection decisions AO's recovery paths are built on.
//
// Every test here runs on its OWN isolated tmux server (Options.Socket), so it
// can never see, adopt, or kill anything on the operator's default server, and
// two of these tests running at once cannot collide.
//
// Skips are honest: they happen only when the host genuinely has no tmux, and
// they say so.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// requireRealTmux skips with a stated reason when the host has no tmux, and
// otherwise reports the version under test so the evidence names what it ran
// against.
func requireRealTmux(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skipf("skipping real-tmux E2E: no tmux binary on PATH (%v)", err)
	}
	out, verr := exec.Command(bin, "-V").CombinedOutput()
	if verr != nil {
		t.Skipf("skipping real-tmux E2E: %s -V failed (%v: %s)", bin, verr, out)
	}
	version := strings.TrimSpace(string(out))
	t.Logf("real tmux under test: %s (%s)", version, bin)
	return version
}

// realTmuxServer returns a Runtime pinned to a private tmux server for this
// test, plus the socket name, and tears the whole server down afterwards.
func realTmuxServer(t *testing.T) (*Runtime, string) {
	t.Helper()
	// tmux socket names live in a filesystem path, so keep them short and safe.
	socket := fmt.Sprintf("ao-p0c-%d", time.Now().UnixNano()%1_000_000_000)
	_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })
	return New(Options{Timeout: 10 * time.Second, Socket: socket, ScratchDir: t.TempDir()}), socket
}

// liveSessionNames lists what actually exists on the server right now. Used to
// prove no duplicate was created, which a per-handle probe cannot show.
func liveSessionNames(t *testing.T, socket string) []string {
	t.Helper()
	out, err := exec.Command("tmux", "-L", socket, "list-sessions", "-F", "#{session_name}").CombinedOutput()
	if err != nil {
		// No server means no sessions; every other failure is a real problem.
		if serverUnreachableOutput(string(out)) || sessionMissingOutput(string(out)) {
			return nil
		}
		t.Fatalf("list-sessions on %s: %v: %s", socket, err, out)
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names
}

func createRealSession(t *testing.T, r *Runtime, id, owner string) ports.RuntimeHandle {
	t.Helper()
	h, err := r.Create(context.Background(), ports.RuntimeConfig{
		SessionID:     domain.SessionID(id),
		WorkspacePath: t.TempDir(),
		// A workload that outlives the assertions, so "alive" is a real fact
		// and not a race with a command that already exited.
		Argv:  []string{"sh", "-c", "sleep 120"},
		Owner: owner,
	})
	if err != nil {
		t.Fatalf("Create(%s): %v", id, err)
	}
	if !isSessionInstanceID(h.InstanceID) {
		t.Fatalf("Create(%s) returned no session instance id: %+v", id, h)
	}
	return h
}

// C1 + C2: an AO-owned session exists on a real server, and SessionFacts
// reports its ownership, its incarnation and its liveness from that server.
func TestRealTmux_OwnedSessionIsObservableThroughSessionFacts(t *testing.T) {
	requireRealTmux(t)
	r, _ := realTmuxServer(t)
	ctx := context.Background()

	const id = "p0c-owned"
	owner := "ao-worker:" + id
	h := createRealSession(t, r, id, owner)

	facts, exists, err := r.SessionFacts(ctx, h)
	if err != nil || !exists {
		t.Fatalf("SessionFacts = (%+v, %t, %v), want facts about a live owned session", facts, exists, err)
	}
	if facts.InstanceID != h.InstanceID {
		t.Fatalf("instance = %q, want the incarnation Create returned (%q)", facts.InstanceID, h.InstanceID)
	}
	if !facts.OwnerKnown || facts.Owner != owner {
		t.Fatalf("owner = (%q, known=%t), want %q attached at creation", facts.Owner, facts.OwnerKnown, owner)
	}
	if !facts.WorkloadKnown || !facts.WorkloadAlive {
		t.Fatalf("workload = (alive=%t, known=%t), want a proven-live workload", facts.WorkloadAlive, facts.WorkloadKnown)
	}
}

// C3 + C11 + C12: a coordinator/runtime rebuilt from nothing but the socket
// recovers the SAME owned incarnation; repeating the reconciliation is
// idempotent, and repeated recreation of the runtime never launches or adopts a
// second session.
func TestRealTmux_RecreatedRuntimeRecoversTheSameOwnedSessionIdempotently(t *testing.T) {
	requireRealTmux(t)
	r, socket := realTmuxServer(t)
	ctx := context.Background()

	const id = "p0c-recover"
	owner := "ao-reviewer:" + id
	h := createRealSession(t, r, id, owner)

	before := liveSessionNames(t, socket)
	if len(before) != 1 {
		t.Fatalf("sessions after one create = %v, want exactly one", before)
	}

	// Five independent "daemon boots": a brand-new Runtime value each time,
	// sharing nothing with its predecessor except the socket name (which a
	// restarted daemon re-derives from its stable data dir).
	for boot := 1; boot <= 5; boot++ {
		fresh := New(Options{Timeout: 10 * time.Second, Socket: socket, ScratchDir: t.TempDir()})

		// Reconcile twice within the boot: the second pass must observe exactly
		// what the first did and change nothing.
		var first ports.SessionFacts
		for pass := 1; pass <= 2; pass++ {
			facts, exists, err := fresh.SessionFacts(ctx, h)
			if err != nil || !exists {
				t.Fatalf("boot %d pass %d: SessionFacts = (%+v, %t, %v), want the recovered session", boot, pass, facts, exists, err)
			}
			if facts.InstanceID != h.InstanceID {
				t.Fatalf("boot %d pass %d: instance = %q, want the pre-restart incarnation %q", boot, pass, facts.InstanceID, h.InstanceID)
			}
			if !facts.OwnerKnown || facts.Owner != owner {
				t.Fatalf("boot %d pass %d: ownership was lost across the restart: (%q, %t)", boot, pass, facts.Owner, facts.OwnerKnown)
			}
			if pass == 1 {
				first = facts
			} else if facts != first {
				t.Fatalf("boot %d: reconciliation is not idempotent: %+v then %+v", boot, first, facts)
			}
		}

		// And the server itself must still hold exactly the one session: a
		// recovery that relaunched instead of adopting would show up here and
		// nowhere else.
		if got := liveSessionNames(t, socket); len(got) != 1 || got[0] != before[0] {
			t.Fatalf("boot %d: sessions = %v, want the single pre-existing %v", boot, got, before)
		}
	}
}

// C4 + C5 + C9: destroying the real session is proven absence for the handle
// that named it, and a session that later takes the SAME NAME is a different
// incarnation that the old handle must never adopt.
//
// This is the ABA the instance id exists to close: name-based recovery cannot
// tell "my session is still here" from "somebody else has my session's name".
func TestRealTmux_StaleInstanceIsNeverAdoptedEvenUnderTheSameName(t *testing.T) {
	requireRealTmux(t)
	r, socket := realTmuxServer(t)
	ctx := context.Background()

	const id = "p0c-aba"
	// A second session keeps the server up across the destroy: tearing down the
	// last session stops tmux, and "no server" is unreachability, not absence.
	keepAlive := createRealSession(t, r, "p0c-aba-keep", "ao-keep")
	_ = keepAlive

	old := createRealSession(t, r, id, "ao-worker:generation-1")

	if err := r.DestroyInstance(ctx, old.InstanceID); err != nil {
		t.Fatalf("DestroyInstance: %v", err)
	}

	// PROVEN ABSENCE for the destroyed incarnation.
	gone, exists, err := r.SessionFacts(ctx, old)
	if err != nil {
		t.Fatalf("SessionFacts on a destroyed incarnation: %v", err)
	}
	if exists {
		t.Fatalf("facts were returned for a destroyed incarnation: %+v", gone)
	}

	// THE ABA: the same NAME comes back, as a genuinely different session with a
	// different owner — a later generation, or an unrelated stranger.
	replacement := createRealSession(t, r, id, "ao-worker:generation-2")
	if replacement.InstanceID == old.InstanceID {
		t.Fatalf("tmux reused the incarnation id %q; this test cannot prove anything", old.InstanceID)
	}
	if names := liveSessionNames(t, socket); len(names) != 2 {
		t.Fatalf("sessions = %v, want the keep-alive and the replacement", names)
	}

	// The OLD handle still names the OLD incarnation, and must still report
	// absence — never the replacement's facts.
	stale, exists, err := r.SessionFacts(ctx, old)
	if err != nil {
		t.Fatalf("SessionFacts on the stale handle after the name was reused: %v", err)
	}
	if exists {
		t.Fatalf("the stale handle adopted the session that took its name: %+v", stale)
	}

	// The replacement, addressed by its own incarnation, is fully observable.
	fresh, exists, err := r.SessionFacts(ctx, replacement)
	if err != nil || !exists {
		t.Fatalf("SessionFacts on the replacement = (%+v, %t, %v)", fresh, exists, err)
	}
	if fresh.Owner != "ao-worker:generation-2" {
		t.Fatalf("replacement owner = %q, want the new generation's token", fresh.Owner)
	}

	// And a destroy issued by the stale generation cannot reach the newer one.
	_ = r.DestroyInstance(ctx, old.InstanceID)
	if _, exists, err := r.SessionFacts(ctx, replacement); err != nil || !exists {
		t.Fatalf("a stale generation's destroy killed the newer session (%t, %v)", exists, err)
	}
}

// C6: a session AO did not create carries no ownership token, so it is
// observably unowned and can never be mistaken for an AO-owned one. The same
// read is what rejects a session owned by a DIFFERENT token.
func TestRealTmux_WrongAndMissingOwnerAreRejected(t *testing.T) {
	requireRealTmux(t)
	r, socket := realTmuxServer(t)
	ctx := context.Background()

	// (a) A stranger's session, created straight on AO's socket without going
	// through AO at all — the shape of a session AO must never adopt.
	stranger := SessionName("p0c-stranger")
	if out, err := exec.Command("tmux", "-L", socket, "new-session", "-d", "-s", stranger, "sh", "-c", "sleep 120").CombinedOutput(); err != nil {
		t.Fatalf("seed stranger session: %v: %s", err, out)
	}
	strangerInstance, ok, err := r.resolveInstance(ctx, "p0c-stranger")
	if err != nil || !ok {
		t.Fatalf("resolveInstance(stranger) = (%q, %t, %v)", strangerInstance, ok, err)
	}
	facts, exists, err := r.SessionFacts(ctx, ports.RuntimeHandle{ID: "p0c-stranger", InstanceID: strangerInstance})
	if err != nil || !exists {
		t.Fatalf("SessionFacts(stranger) = (%+v, %t, %v)", facts, exists, err)
	}
	if facts.OwnerKnown || facts.Owner != "" {
		t.Fatalf("an unowned session reported ownership: (%q, known=%t)", facts.Owner, facts.OwnerKnown)
	}

	// (b) An AO session owned by somebody else. Ownership is a fact read off the
	// live incarnation, so the mismatch is decidable rather than assumed.
	mine := createRealSession(t, r, "p0c-owner-a", "ao-worker:A")
	got, exists, err := r.SessionFacts(ctx, mine)
	if err != nil || !exists {
		t.Fatalf("SessionFacts(owner-a) = (%+v, %t, %v)", got, exists, err)
	}
	if !got.OwnerKnown {
		t.Fatal("an AO-created session did not report its ownership token")
	}
	if got.Owner == "ao-worker:B" {
		t.Fatalf("owner = %q, but this session belongs to A", got.Owner)
	}
	// The decision a caller holding B's expectation makes:
	if adoptable := got.OwnerKnown && got.Owner == "ao-worker:B"; adoptable {
		t.Fatal("a caller expecting owner B would have adopted a session owned by A")
	}
}

// panePIDRewritingRunner answers the pane-pid probe with a fixed string and
// sends everything else to the real server unchanged.
type panePIDRewritingRunner struct {
	inner  runner
	answer string
	active bool
	hits   int
}

func (p *panePIDRewritingRunner) Run(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	if p.active && len(args) > 0 && args[len(args)-1] == "#{pane_pid}" {
		p.hits++
		return []byte(p.answer), nil
	}
	return p.inner.Run(ctx, env, name, args...)
}

// C7 + C8: an empty pane pid and a malformed one are both UNKNOWN liveness on a
// session that provably still exists — never an error, and never death.
func TestRealTmux_EmptyOrMalformedPanePIDIsUnknownLivenessNotAnError(t *testing.T) {
	requireRealTmux(t)
	r, _ := realTmuxServer(t)
	ctx := context.Background()

	gate := &panePIDRewritingRunner{inner: r.runner}
	r.runner = gate

	const id = "p0c-panepid"
	owner := "ao-worker:" + id
	h := createRealSession(t, r, id, owner)

	for _, tc := range []struct {
		name   string
		answer string
	}{
		{"empty", "\n"},
		{"whitespace only", "   \n"},
		{"non-numeric", "not-a-pid\n"},
		{"negative", "-1\n"},
		{"zero", "0\n"},
		{"float", "1234.5\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate.answer, gate.active, gate.hits = tc.answer, true, 0
			defer func() { gate.active = false }()

			facts, exists, err := r.SessionFacts(ctx, h)
			if err != nil {
				t.Fatalf("a %s pane pid failed the observation: %v", tc.name, err)
			}
			if gate.hits == 0 {
				t.Fatal("the pane-pid probe was never issued; this case proved nothing")
			}
			if !exists {
				t.Fatalf("a live session was reported gone because its pane pid was %s", tc.name)
			}
			if facts.InstanceID != h.InstanceID {
				t.Fatalf("instance = %q, want %q", facts.InstanceID, h.InstanceID)
			}
			if !facts.OwnerKnown || facts.Owner != owner {
				t.Fatalf("ownership was lost with the pane pid: (%q, %t)", facts.Owner, facts.OwnerKnown)
			}
			if facts.WorkloadKnown {
				t.Fatalf("liveness was claimed KNOWN (alive=%t) from a pane that named no usable process", facts.WorkloadAlive)
			}
			if facts.WorkloadAlive {
				t.Fatal("liveness was claimed ALIVE from an unusable pane pid")
			}
		})
	}
}

// C10: one broken real session does not affect an unrelated one. Both are real,
// on the same real server; killing/destroying one must leave the other's facts
// exactly as they were.
func TestRealTmux_OneBadSessionDoesNotAffectAnUnrelatedOne(t *testing.T) {
	requireRealTmux(t)
	r, socket := realTmuxServer(t)
	ctx := context.Background()

	good := createRealSession(t, r, "p0c-good", "ao-worker:good")
	bad := createRealSession(t, r, "p0c-bad", "ao-worker:bad")

	baseline, exists, err := r.SessionFacts(ctx, good)
	if err != nil || !exists {
		t.Fatalf("baseline SessionFacts(good) = (%+v, %t, %v)", baseline, exists, err)
	}

	// Break the bad one for real: kill its pane's workload out from under it,
	// then destroy the incarnation entirely.
	if out, kerr := exec.Command("tmux", "-L", socket, "kill-session", "-t", "="+SessionName("p0c-bad")).CombinedOutput(); kerr != nil {
		t.Fatalf("kill the bad session: %v: %s", kerr, out)
	}

	if _, exists, err := r.SessionFacts(ctx, bad); err != nil {
		t.Fatalf("SessionFacts(bad) after the kill: %v", err)
	} else if exists {
		t.Fatal("the killed session still reported facts")
	}

	after, exists, err := r.SessionFacts(ctx, good)
	if err != nil || !exists {
		t.Fatalf("the unrelated session was affected: (%+v, %t, %v)", after, exists, err)
	}
	if after != baseline {
		t.Fatalf("unrelated session facts changed: %+v -> %+v", baseline, after)
	}
}
