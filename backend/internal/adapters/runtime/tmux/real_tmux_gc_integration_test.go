package tmux

// P1-C SECTION U — Runtime GC proven against a REAL tmux server.
//
// The GC package's own tests use a scripted runtime, which proves the decision
// logic. This file proves the two things a fake cannot: that AO's inventory
// really enumerates a real tmux server with real incarnations, and that a
// destroy addressed to `$N` really spares a different session that has taken
// the same NAME.
//
// Every test runs on its OWN isolated tmux server (Options.Socket), so it can
// never see, adopt or kill anything on the operator's default server, and two
// of them running at once cannot collide. That is the same discipline P0-C's
// real-tmux suite established, and for the same reason: a test that can reach
// a person's real sessions is a test that will eventually kill one.

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/runtimegc"
)

// gcClaims/gcRuns are the durable halves, as small as the sweeper needs.
type gcClaims struct {
	outstanding []domain.CapacityClaim
	held        []domain.CapacityClaim
}

func (c *gcClaims) ListOutstandingCapacityClaims(context.Context) ([]domain.CapacityClaim, error) {
	return c.outstanding, nil
}
func (c *gcClaims) ListHeldCapacityClaims(context.Context) ([]domain.CapacityClaim, error) {
	return c.held, nil
}
func (c *gcClaims) ListCapacityClaimsForRun(_ context.Context, runID string) ([]domain.CapacityClaim, error) {
	var out []domain.CapacityClaim
	for _, claim := range c.outstanding {
		if claim.WorkflowRunID == runID {
			out = append(out, claim)
		}
	}
	return out, nil
}

type gcRuns struct{ runs map[string]domain.WorkflowRun }

func (r *gcRuns) GetWorkflowRun(_ context.Context, id string) (domain.WorkflowRun, bool, error) {
	run, ok := r.runs[id]
	return run, ok, nil
}
func (r *gcRuns) ListWorkflowRuns(context.Context, string) ([]domain.WorkflowRun, error) {
	out := make([]domain.WorkflowRun, 0, len(r.runs))
	for _, run := range r.runs {
		out = append(out, run)
	}
	return out, nil
}

// createOwnedSession creates a long-lived AO-owned session and returns its
// incarnation.
func createOwnedSession(t *testing.T, r *Runtime, name, owner string) string {
	t.Helper()
	handle, err := r.Create(context.Background(), ports.RuntimeConfig{
		SessionID:     domain.SessionID(name),
		WorkspacePath: t.TempDir(),
		Argv:          []string{"sh", "-c", "sleep 300"},
		Owner:         owner,
	})
	if err != nil {
		t.Fatalf("create session %s: %v", name, err)
	}
	if handle.InstanceID == "" {
		t.Fatalf("create session %s returned no incarnation", name)
	}
	return handle.InstanceID
}

// createStrangerSession creates a session AO did NOT create — a person's own
// window, made with the tmux CLI directly and carrying no ownership token.
func createStrangerSession(t *testing.T, socket, name string) {
	t.Helper()
	out, err := exec.Command("tmux", "-L", socket, "new-session", "-d", "-s", name, "sleep 300").CombinedOutput()
	if err != nil {
		t.Fatalf("create stranger session %s: %v: %s", name, err, out)
	}
}

func gcSweeper(r *Runtime, claims *gcClaims, runs *gcRuns) *runtimegc.Sweeper {
	return &runtimegc.Sweeper{Inventory: r, Facts: r, Claims: claims, Runs: runs}
}

func instanceExists(t *testing.T, socket, instance string) bool {
	t.Helper()
	out, err := exec.Command("tmux", "-L", socket, "list-sessions", "-F", "#{session_id}").CombinedOutput()
	if err != nil {
		if strings.Contains(strings.ToLower(string(out)), "no server running") {
			return false
		}
		t.Fatalf("list-sessions on %s: %v: %s", socket, err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == instance {
			return true
		}
	}
	return false
}

// U1 + U3 + U5: an AO-owned session is reclaimed; a session AO did not create
// is untouched; and both verdicts come from the ownership token rather than
// from the name.
func TestRealTmuxGCReclaimsOwnedAndSparesStrangers(t *testing.T) {
	requireRealTmux(t)
	r, socket := realTmuxServer(t)
	ctx := context.Background()

	ownedInstance := createOwnedSession(t, r, "ao-reviewer-gc-1", "ao-reviewer:rv-gc-1")
	createStrangerSession(t, socket, "someones-own-window")

	// AO's inventory must see both — including the one it may not touch,
	// because a sweep that cannot see an unprovable session cannot report it.
	sessions, err := r.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("inventory found %d sessions, want 2: %+v", len(sessions), sessions)
	}
	var sawOwned, sawStranger bool
	for _, s := range sessions {
		switch s.ID {
		case "ao-reviewer-gc-1":
			sawOwned = true
			if !s.OwnerKnown || s.Owner != "ao-reviewer:rv-gc-1" {
				t.Fatalf("AO's own session did not report its ownership token: %+v", s)
			}
			if s.InstanceID != ownedInstance {
				t.Fatalf("inventory reported incarnation %s, want %s", s.InstanceID, ownedInstance)
			}
		case "someones-own-window":
			sawStranger = true
			if s.OwnerKnown {
				t.Fatalf("a session AO did not create reported an ownership token: %+v", s)
			}
		}
	}
	if !sawOwned || !sawStranger {
		t.Fatalf("inventory missed a session: %+v", sessions)
	}

	report, err := gcSweeper(r, &gcClaims{}, &gcRuns{runs: map[string]domain.WorkflowRun{}}).
		Sweep(ctx, runtimegc.Options{Trigger: "real-tmux-test"})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if report.Cleaned != 1 || report.SkippedUnprovable != 1 {
		t.Fatalf("report = %+v, want 1 cleaned and 1 unprovable", report)
	}
	if instanceExists(t, socket, ownedInstance) {
		t.Fatal("AO's own finished session survived the sweep")
	}
	if names := liveSessionNames(t, socket); len(names) != 1 || names[0] != "someones-own-window" {
		t.Fatalf("live sessions after the sweep = %v, want only the stranger's", names)
	}
}

// U2: a live session with a claim paying for it is preserved.
func TestRealTmuxGCPreservesALiveClaimedSession(t *testing.T) {
	requireRealTmux(t)
	r, socket := realTmuxServer(t)
	ctx := context.Background()

	instance := createOwnedSession(t, r, "ao-worker-gc-2", "ao-worker:w-gc-2")
	claim := domain.CapacityClaim{
		ID: "cap-live", Kind: domain.ExecutionKindWorker, State: domain.CapacityClaimHeld,
		WorkflowRunID: "wf-live", RuntimeHandle: "ao-worker-gc-2", RuntimeInstanceID: instance,
		DispatchKey: "k-live", LifecycleGeneration: 1,
	}
	claims := &gcClaims{held: []domain.CapacityClaim{claim}, outstanding: []domain.CapacityClaim{claim}}
	runs := &gcRuns{runs: map[string]domain.WorkflowRun{"wf-live": {ID: "wf-live", State: domain.WorkflowRunRunning}}}

	report, err := gcSweeper(r, claims, runs).Sweep(ctx, runtimegc.Options{Trigger: "real-tmux-test"})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if report.Cleaned != 0 || report.SkippedLive != 1 {
		t.Fatalf("report = %+v, want 0 cleaned and 1 live", report)
	}
	if !instanceExists(t, socket, instance) {
		t.Fatal("a live session a held capacity claim was paying for was destroyed")
	}
}

// U4: session-name ABA, against real tmux.
//
// The sweep is handed a candidate naming an incarnation that has already gone,
// while a DIFFERENT, live session now answers to the same name. A destroy
// addressed to the name would kill the replacement; addressed to `$N` it kills
// nothing.
func TestRealTmuxGCRefusesToDestroyAReusedSessionName(t *testing.T) {
	requireRealTmux(t)
	r, socket := realTmuxServer(t)
	ctx := context.Background()

	// A keepalive session, so killing the first incarnation below does not
	// tear the whole tmux server down. tmux restarts its `$N` counter with the
	// server, and a reused id is precisely the ABA this test must NOT model —
	// it has to be two genuinely different incarnations.
	createStrangerSession(t, socket, "keepalive")

	const name = "ao-reviewer-gc-aba"
	first := createOwnedSession(t, r, name, "ao-reviewer:rv-aba")
	// The original ends and its name is surrendered.
	if out, err := exec.Command("tmux", "-L", socket, "kill-session", "-t", first).CombinedOutput(); err != nil {
		t.Fatalf("kill first incarnation: %v: %s", err, out)
	}
	// A brand-new session claims the same name.
	second := createOwnedSession(t, r, name, "ao-reviewer:rv-aba")
	if second == first {
		t.Fatalf("tmux reused the incarnation id %s; the test cannot exercise the ABA", first)
	}

	// A durable claim from the FIRST incarnation, whose run has since ended:
	// exactly the stale candidate a crashed daemon leaves behind.
	stale := domain.CapacityClaim{
		ID: "cap-stale", Kind: domain.ExecutionKindReviewer, State: domain.CapacityClaimHeld,
		WorkflowRunID: "wf-stale", RuntimeHandle: name, RuntimeInstanceID: first,
		DispatchKey: "k-stale", LifecycleGeneration: 1,
	}
	// ...and the live claim belonging to the replacement.
	live := domain.CapacityClaim{
		ID: "cap-second", Kind: domain.ExecutionKindReviewer, State: domain.CapacityClaimHeld,
		WorkflowRunID: "wf-second", RuntimeHandle: name, RuntimeInstanceID: second,
		DispatchKey: "k-second", LifecycleGeneration: 1,
	}
	claims := &gcClaims{
		held:        []domain.CapacityClaim{stale, live},
		outstanding: []domain.CapacityClaim{stale, live},
	}
	runs := &gcRuns{runs: map[string]domain.WorkflowRun{
		"wf-stale":  {ID: "wf-stale", State: domain.WorkflowRunCompleted},
		"wf-second": {ID: "wf-second", State: domain.WorkflowRunRunning},
	}}

	report, err := gcSweeper(r, claims, runs).Sweep(ctx, runtimegc.Options{Trigger: "real-tmux-test"})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !instanceExists(t, socket, second) {
		t.Fatalf("the session that took the name (%s) was destroyed by a stale candidate for %s", second, first)
	}
	var staleFinding runtimegc.Finding
	for _, f := range report.Findings {
		if f.InstanceID == first {
			staleFinding = f
		}
	}
	// Either verdict is correct and both are safe: tmux may report the
	// incarnation as simply gone (absent), or report the name now answering
	// for a different one (foreign). What must never happen is `cleaned`.
	switch staleFinding.Disposition {
	case runtimegc.DispositionAbsent, runtimegc.DispositionForeign:
	default:
		t.Fatalf("the stale candidate was %s, want absent or foreign: %+v", staleFinding.Disposition, staleFinding)
	}
}

// U6: repeated sweeps against a real server converge and stay idempotent.
func TestRealTmuxGCIsIdempotentAcrossRepeatedSweeps(t *testing.T) {
	requireRealTmux(t)
	r, socket := realTmuxServer(t)
	ctx := context.Background()

	orphan := createOwnedSession(t, r, "ao-reviewer-gc-idem", "ao-reviewer:rv-idem")
	createStrangerSession(t, socket, "not-aos-session")
	s := gcSweeper(r, &gcClaims{}, &gcRuns{runs: map[string]domain.WorkflowRun{}})

	first, err := s.Sweep(ctx, runtimegc.Options{Trigger: "real-tmux-test"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Cleaned != 1 {
		t.Fatalf("first sweep cleaned %d, want 1: %+v", first.Cleaned, first.Findings)
	}
	for i := 0; i < 3; i++ {
		again, aerr := s.Sweep(ctx, runtimegc.Options{Trigger: "real-tmux-test"})
		if aerr != nil {
			t.Fatalf("sweep %d: %v", i+2, aerr)
		}
		if again.Cleaned != 0 {
			t.Fatalf("sweep %d cleaned %d; nothing new existed to clean", i+2, again.Cleaned)
		}
		if again.SkippedUnprovable != 1 {
			t.Fatalf("sweep %d stopped reporting the session AO cannot prove it owns: %+v", i+2, again)
		}
	}
	if instanceExists(t, socket, orphan) {
		t.Fatal("the orphan survived")
	}
	if names := liveSessionNames(t, socket); len(names) != 1 || names[0] != "not-aos-session" {
		t.Fatalf("live sessions = %v, want only the stranger's", names)
	}
}

// A dry run against a real server destroys nothing at all.
func TestRealTmuxGCDryRunDestroysNothing(t *testing.T) {
	requireRealTmux(t)
	r, socket := realTmuxServer(t)
	ctx := context.Background()

	orphan := createOwnedSession(t, r, "ao-reviewer-gc-dry", "ao-reviewer:rv-dry")
	report, err := gcSweeper(r, &gcClaims{}, &gcRuns{runs: map[string]domain.WorkflowRun{}}).
		Sweep(ctx, runtimegc.Options{DryRun: true, Trigger: "real-tmux-test"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Cleaned != 0 || report.Candidates != 1 {
		t.Fatalf("dry run report = %+v, want 1 candidate and 0 cleaned", report)
	}
	if !instanceExists(t, socket, orphan) {
		t.Fatal("a dry run destroyed a real session")
	}
	_ = time.Now
}
