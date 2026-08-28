package tmux

// A REAL tmux server, a real pane, and the exact runtime fact that stalled
// wf-9405592d overnight: `#{pane_pid}` coming back empty for a live session.
//
// The unit tests above script tmux; this one does not. It creates an actual
// session, proves AO classifies it correctly while the pane answers normally,
// then blanks ONLY the pane-pid answer (everything else still goes to the real
// server) and proves the observation degrades to "liveness unknown" instead of
// failing. Finally it kills the session for real and proves a handle naming the
// dead incarnation reports proven absence rather than facts about its successor.

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// blankPanePIDRunner is the real command runner with one answer removed: the
// pane-pid probe returns an empty string, exactly as tmux does for a pane it
// cannot report on. Every other command reaches the real server unchanged.
type blankPanePIDRunner struct {
	inner  runner
	blank  bool
	blanks int
}

func (b *blankPanePIDRunner) Run(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	if b.blank && len(args) > 0 && args[len(args)-1] == "#{pane_pid}" {
		b.blanks++
		return []byte("\n"), nil
	}
	return b.inner.Run(ctx, env, name, args...)
}

func TestSessionFactsIntegration_EmptyPanePIDDoesNotFailTheObservation(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	ctx := context.Background()
	// Short, fixed ids: tmux truncates long session names, and a test that
	// cannot name its own leftovers cannot clean them up either.
	const id = "ao-stalepane"
	const keepAlive = "ao-stalepane-keep"
	r := New(Options{Timeout: 10 * time.Second})
	gate := &blankPanePIDRunner{inner: r.runner}
	r.runner = gate

	// Idempotent from whatever a previous interrupted run left behind.
	_ = r.Destroy(ctx, ports.RuntimeHandle{ID: id})
	_ = r.Destroy(ctx, ports.RuntimeHandle{ID: keepAlive})
	t.Cleanup(func() {
		gate.blank = false
		_ = r.Destroy(context.Background(), ports.RuntimeHandle{ID: id})
		_ = r.Destroy(context.Background(), ports.RuntimeHandle{ID: keepAlive})
	})

	h, err := r.Create(ctx, ports.RuntimeConfig{
		SessionID:     domain.SessionID(id),
		WorkspacePath: t.TempDir(),
		Argv:          []string{"sh", "-c", "sleep 30"},
		Owner:         "ao-reviewer:" + id,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if h.InstanceID == "" {
		t.Fatal("the real server reported no session instance")
	}

	// 1. THE HEALTHY BASELINE, from the real server.
	facts, exists, err := r.SessionFacts(ctx, h)
	if err != nil || !exists {
		t.Fatalf("SessionFacts on a live session = (%+v, %t, %v)", facts, exists, err)
	}
	if !facts.OwnerKnown || facts.Owner != "ao-reviewer:"+id {
		t.Fatalf("owner = (%q, %t), want the token Create attached", facts.Owner, facts.OwnerKnown)
	}
	if !facts.WorkloadKnown || !facts.WorkloadAlive {
		t.Fatalf("workload = (alive=%t, known=%t), want a live workload", facts.WorkloadAlive, facts.WorkloadKnown)
	}

	// 2. THE PRODUCTION FAULT: the same live session, whose pane will no longer
	//    name its process.
	gate.blank = true
	stale, exists, err := r.SessionFacts(ctx, h)
	if err != nil {
		t.Fatalf("an empty pane pid from a real session failed the observation: %v", err)
	}
	if gate.blanks == 0 {
		t.Fatal("the pane-pid probe was never issued; this test proved nothing")
	}
	if !exists {
		t.Fatal("a live tmux session was reported gone because its pane pid was unreadable")
	}
	if stale.InstanceID != h.InstanceID {
		t.Fatalf("instance = %q, want the incarnation AO created (%q)", stale.InstanceID, h.InstanceID)
	}
	if !stale.OwnerKnown || stale.Owner != "ao-reviewer:"+id {
		t.Fatalf("ownership was lost with the pane pid: (%q, %t)", stale.Owner, stale.OwnerKnown)
	}
	if stale.WorkloadKnown {
		t.Fatalf("liveness was claimed known (alive=%t) from a pane that named no process", stale.WorkloadAlive)
	}
	gate.blank = false

	// 3. A GENERATION THAT IS ACTUALLY OVER. The session is destroyed for real;
	//    a handle naming the dead incarnation must report proven absence.
	//
	//    A second session keeps the server up: tearing down the LAST session
	//    stops tmux entirely, and "no server" is unreachability, not absence --
	//    a distinction the adapter is right to keep and this test must not blur.
	if _, kerr := r.Create(ctx, ports.RuntimeConfig{
		SessionID:     domain.SessionID(keepAlive),
		WorkspacePath: t.TempDir(),
		Argv:          []string{"sh", "-c", "sleep 30"},
		Owner:         "ao-reviewer:" + keepAlive,
	}); kerr != nil {
		t.Fatalf("Create keep-alive: %v", kerr)
	}
	if derr := r.DestroyInstance(ctx, h.InstanceID); derr != nil {
		t.Fatalf("DestroyInstance: %v", derr)
	}
	gone, exists, err := r.SessionFacts(ctx, h)
	if err != nil {
		t.Fatalf("SessionFacts on a destroyed instance: %v", err)
	}
	if exists {
		t.Fatalf("facts were returned for a destroyed incarnation: %+v", gone)
	}
}
