// Package tmuxe2e proves P9's worker ownership model against a REAL tmux binary.
//
// Every session lives on a private socket (ao-p9-e2e-<pid>-<n>) that is killed on
// cleanup, and the test asserts nothing of that namespace survives. AO's own
// servers (`ao`, `ao-<hash>`) and the user's default server are never touched.
// Synchronisation is by files the deterministic fixture writes atomically,
// polled against a deadline — never by sleeping for "long enough".
package tmuxe2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/runtime/tmux"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/workerownership"
)

const (
	installationA = "aoi-11111111-1111-4111-8111-111111111111"
	installationB = "aoi-22222222-2222-4222-8222-222222222222"
	deadline      = 15 * time.Second
)

var socketSeq atomic.Int64

// requireTmux skips when tmux is absent, unless AO_P9_REQUIRE_TMUX=1 makes that
// a failure (the P9 gate runs with it set: a missing binary is a blocker, not a
// pass).
func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		if os.Getenv("AO_P9_REQUIRE_TMUX") == "1" {
			t.Fatalf("tmux is required for the P9 real-tmux E2E: %v", err)
		}
		t.Skip("tmux not installed")
	}
}

type server struct {
	t      *testing.T
	socket string
}

// newServer starts an isolated tmux server with a keepalive session, so killing
// a worker never shuts the server down and resets tmux's `$N` counter.
func newServer(t *testing.T) *server {
	t.Helper()
	requireTmux(t)
	s := &server{t: t, socket: fmt.Sprintf("ao-p9-e2e-%d-%d", os.Getpid(), socketSeq.Add(1))}
	socketPath := ""
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", s.socket, "kill-server").Run()
		if socketPath != "" {
			// The server is gone; its socket file is this test's own, nobody else's.
			_ = os.Remove(socketPath)
		}
		// T10: nothing of this namespace may survive the test.
		if out, err := exec.Command("tmux", "-L", s.socket, "list-sessions").CombinedOutput(); err == nil {
			t.Errorf("tmux sessions survived cleanup on %s:\n%s", s.socket, out)
		}
	})
	if out, err := exec.Command("tmux", "-L", s.socket, "new-session", "-d", "-s", "e2e-keepalive", "sh", "-c", "exec cat").CombinedOutput(); err != nil {
		t.Fatalf("start isolated tmux server: %v: %s", err, out)
	}
	if out, err := exec.Command("tmux", "-L", s.socket, "display-message", "-p", "#{socket_path}").Output(); err == nil {
		if p := strings.TrimSpace(string(out)); strings.Contains(p, "ao-p9-e2e-") {
			socketPath = p
		}
	}
	return s
}

// runtime is one DAEMON's view of the shared server: its own Runtime value, its
// own installation and daemon instance identity.
func (s *server) runtime(installation, daemon string) *tmux.Runtime {
	return tmux.New(tmux.Options{Socket: s.socket, ScratchDir: s.t.TempDir(), InstallationID: installation, DaemonInstanceID: daemon})
}

func fixturePath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("testdata", "p9worker.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type worker struct {
	rec    domain.SessionRecord
	handle ports.RuntimeHandle
	dir    string
}

// launch creates a worker the way session_manager does — the ownership token is
// part of the creation command — and returns the DURABLE record AO would store.
func launch(t *testing.T, r *tmux.Runtime, sessionID domain.SessionID, launchID, mode string) worker {
	t.Helper()
	dir := t.TempDir()
	token := domain.SessionRuntimeOwnerToken(sessionID, launchID)
	handle, err := r.Create(context.Background(), ports.RuntimeConfig{
		SessionID: sessionID, WorkspacePath: dir,
		Argv:  []string{"sh", fixturePath(t), dir, mode},
		Env:   map[string]string{"AO_SUPERVISED_PROCESS": "1"},
		Owner: token,
	})
	if err != nil {
		t.Fatalf("create %s: %v", sessionID, err)
	}
	w := worker{handle: handle, dir: dir, rec: domain.SessionRecord{
		ID: sessionID,
		Metadata: domain.SessionMetadata{
			RuntimeHandleID: handle.ID, RuntimeInstanceID: handle.InstanceID,
			RuntimeLaunchID: launchID, RuntimeOwnerToken: token, WorkspacePath: dir,
		},
	}}
	waitFile(t, dir, "ready")
	return w
}

// durable round-trips a record through JSON: what a restarted daemon has is the
// row, never the in-memory handle.
func durable(t *testing.T, rec domain.SessionRecord) domain.SessionRecord {
	t.Helper()
	b, err := json.Marshal(rec.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	out := domain.SessionRecord{ID: rec.ID}
	if err := json.Unmarshal(b, &out.Metadata); err != nil {
		t.Fatal(err)
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	stop := time.Now().Add(deadline)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		if time.Now().After(stop) {
			t.Fatalf("timed out after %s waiting for %s", deadline, what)
		}
		<-tick.C
	}
}

func waitFile(t *testing.T, dir, name string) string {
	t.Helper()
	var content string
	waitFor(t, name+" in "+dir, func() bool {
		b, err := os.ReadFile(filepath.Join(dir, name))
		content = strings.TrimSpace(string(b))
		return err == nil && content != ""
	})
	return content
}

func heartbeat(dir string) int {
	b, _ := os.ReadFile(filepath.Join(dir, "heartbeat"))
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

// waitHeartbeatPast proves the fixture is still running.
func waitHeartbeatPast(t *testing.T, dir string, n int) int {
	t.Helper()
	var got int
	waitFor(t, fmt.Sprintf("heartbeat past %d", n), func() bool {
		got = heartbeat(dir)
		return got > n
	})
	return got
}

func classify(t *testing.T, rec domain.SessionRecord, r *tmux.Runtime, installation string) domain.WorkerRuntimeObservation {
	t.Helper()
	return workerownership.Classify(context.Background(), rec, r, installation)
}

func wantProof(t *testing.T, got domain.WorkerRuntimeObservation, want domain.WorkerRuntimeProof) {
	t.Helper()
	if got.Proof != want {
		t.Fatalf("proof = %q (%s), want %q", got.Proof, got.Detail, want)
	}
}

// T1 — a normal launch is PROVEN owned from the runtime, and its worker runs.
func TestRealTmuxP9_T1_NormalLaunchIsProvenOwned(t *testing.T) {
	s := newServer(t)
	r := s.runtime(installationA, "aod-daemon-1")
	w := launch(t, r, "p9-t1", "launch-1", "stay")
	wantProof(t, classify(t, w.rec, r, installationA), domain.WorkerRuntimeOwned)
	waitHeartbeatPast(t, w.dir, heartbeat(w.dir))
	facts, ok, err := r.SessionFacts(context.Background(), w.handle)
	if err != nil || !ok {
		t.Fatalf("SessionFacts: %v ok=%v", err, ok)
	}
	if !facts.InstallationKnown || facts.Installation != installationA {
		t.Fatalf("installation stamp = %q known=%v, want %q", facts.Installation, facts.InstallationKnown, installationA)
	}
}

// T2 + T3 — a daemon restart: a NEW runtime value, a new daemon instance, only
// the durable row. The live worker is adopted on proof, and adopting it does not
// disturb it.
func TestRealTmuxP9_T2T3_RestartedDaemonAdoptsOnDurableIdentityAlone(t *testing.T) {
	s := newServer(t)
	first := s.runtime(installationA, "aod-daemon-1")
	w := launch(t, first, "p9-t2", "launch-1", "stay")
	row := durable(t, w.rec)
	before := heartbeat(w.dir)

	restarted := s.runtime(installationA, "aod-daemon-2")
	wantProof(t, classify(t, row, restarted, installationA), domain.WorkerRuntimeOwned)
	waitHeartbeatPast(t, w.dir, before)
	if _, err := os.Stat(filepath.Join(w.dir, "done")); err == nil {
		t.Fatal("adoption ended the worker")
	}
}

// T4 — wrong installation, and wrong incarnation: a runtime created by another
// AO installation on a shared socket is not adopted; a name reused by a NEW
// incarnation is not adopted by the record of the old one (I5/I16).
func TestRealTmuxP9_T4_WrongInstallationOrIncarnationIsRejected(t *testing.T) {
	s := newServer(t)
	other := s.runtime(installationB, "aod-other")
	foreign := launch(t, other, "p9-t4-foreign", "launch-1", "stay")
	wantProof(t, classify(t, durable(t, foreign.rec), s.runtime(installationA, "aod-me"), installationA),
		domain.WorkerRuntimeInstallationMismatch)

	r := s.runtime(installationA, "aod-me")
	w := launch(t, r, "p9-t4-reused", "launch-1", "stay")
	oldRow := durable(t, w.rec)
	if err := r.DestroyInstance(context.Background(), w.handle.InstanceID); err != nil {
		t.Fatalf("destroy first incarnation: %v", err)
	}
	replacement := launch(t, r, "p9-t4-reused", "launch-2", "stay")
	if replacement.handle.InstanceID == w.handle.InstanceID {
		t.Fatalf("tmux reused incarnation %s; the ABA cannot be exercised", w.handle.InstanceID)
	}
	got := classify(t, oldRow, r, installationA)
	wantProof(t, got, domain.WorkerRuntimeInstanceMismatch)
	if got.ObservedInstanceID != replacement.handle.InstanceID {
		t.Fatalf("observed incarnation = %q, want the replacement %q", got.ObservedInstanceID, replacement.handle.InstanceID)
	}
	wantProof(t, classify(t, durable(t, replacement.rec), r, installationA), domain.WorkerRuntimeOwned)
}

// T5 — wrong generation. The row of launch 2 does not prove a runtime still
// carrying launch 1's token (I2), and a real Restart (session_manager's restore
// of a live pane) re-stamps the token so the NEW launch proves itself and the old
// one no longer does (RC4).
func TestRealTmuxP9_T5_WrongGenerationIsRejectedAndRestartRestampsOwnership(t *testing.T) {
	s := newServer(t)
	r := s.runtime(installationA, "aod-me")
	w := launch(t, r, "p9-t5", "launch-1", "stay")

	newer := w.rec
	newer.Metadata.RuntimeLaunchID = "launch-2"
	newer.Metadata.RuntimeOwnerToken = domain.SessionRuntimeOwnerToken(w.rec.ID, "launch-2")
	wantProof(t, classify(t, durable(t, newer), r, installationA), domain.WorkerRuntimeOwnerMismatch)

	// Restore of the live pane under launch 2.
	if err := os.WriteFile(filepath.Join(w.dir, "stop"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFile(t, w.dir, "done")
	if err := os.Remove(filepath.Join(w.dir, "stop")); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"ready", "done", "heartbeat"} {
		_ = os.Remove(filepath.Join(w.dir, f))
	}
	if _, err := r.Restart(context.Background(), w.handle, ports.RuntimeConfig{
		SessionID: w.rec.ID, WorkspacePath: w.dir,
		Argv:  []string{"sh", fixturePath(t), w.dir, "stay"},
		Env:   map[string]string{"AO_SUPERVISED_PROCESS": "1"},
		Owner: newer.Metadata.RuntimeOwnerToken,
	}); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	waitFile(t, w.dir, "ready")
	wantProof(t, classify(t, durable(t, newer), r, installationA), domain.WorkerRuntimeOwned)
	wantProof(t, classify(t, durable(t, w.rec), r, installationA), domain.WorkerRuntimeOwnerMismatch)
}

// T7 — stale/dead runtime: a worker that exited is still PROVABLY this launch
// (owned_exited — its pane is kept by the supervised keep-alive), and a destroyed
// incarnation with nothing under its name is absent. Neither is ever "owned".
func TestRealTmuxP9_T7_DeadAndStaleRuntimes(t *testing.T) {
	s := newServer(t)
	r := s.runtime(installationA, "aod-me")
	for _, mode := range []string{"exit0", "exit1"} {
		w := launch(t, r, domain.SessionID("p9-t7-"+mode), "launch-1", mode)
		waitFile(t, w.dir, "done")
		waitFor(t, mode+" workload to read as exited", func() bool {
			return classify(t, w.rec, r, installationA).Proof == domain.WorkerRuntimeOwnedExited
		})
	}
	w := launch(t, r, "p9-t7-killed", "launch-1", "stay")
	row := durable(t, w.rec)
	if err := r.DestroyInstance(context.Background(), w.handle.InstanceID); err != nil {
		t.Fatalf("DestroyInstance: %v", err)
	}
	wantProof(t, classify(t, row, r, installationA), domain.WorkerRuntimeAbsent)

	// A stale row that never recorded provenance is refused before tmux is even asked.
	legacy := row
	legacy.Metadata.RuntimeOwnerToken = ""
	wantProof(t, classify(t, legacy, r, installationA), domain.WorkerRuntimeProvenanceMissing)
}

// A delayed worker keeps running while ownership is proven and re-proven, and
// finishes on its own terms: proof reads never end or disturb a live worker.
func TestRealTmuxP9_DelayedCompletionIsUndisturbedByOwnershipReads(t *testing.T) {
	s := newServer(t)
	r := s.runtime(installationA, "aod-me")
	w := launch(t, r, "p9-delay", "launch-1", "delay")
	for i := 0; i < 5; i++ {
		wantProof(t, classify(t, w.rec, r, installationA), domain.WorkerRuntimeOwned)
	}
	if err := os.WriteFile(filepath.Join(w.dir, "release"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := waitFile(t, w.dir, "done"); got != "ok" {
		t.Fatalf("done = %q, want ok", got)
	}
	waitFor(t, "released workload to read as exited", func() bool {
		return classify(t, w.rec, r, installationA).Proof == domain.WorkerRuntimeOwnedExited
	})
}

// T10 — cleanup is idempotent and exact: destroying an incarnation twice is
// success, the second destroy cannot reach a replacement under the same name,
// and nothing of the test namespace is left (asserted again in cleanup).
func TestRealTmuxP9_T10_CleanupIsIdempotentAndExact(t *testing.T) {
	s := newServer(t)
	r := s.runtime(installationA, "aod-me")
	w := launch(t, r, "p9-t10", "launch-1", "stay")
	ctx := context.Background()
	if err := r.DestroyInstance(ctx, w.handle.InstanceID); err != nil {
		t.Fatalf("first destroy: %v", err)
	}
	replacement := launch(t, r, "p9-t10", "launch-2", "stay")
	if err := r.DestroyInstance(ctx, w.handle.InstanceID); err != nil {
		t.Fatalf("second destroy of a gone incarnation must be success: %v", err)
	}
	wantProof(t, classify(t, replacement.rec, r, installationA), domain.WorkerRuntimeOwned)
	waitHeartbeatPast(t, replacement.dir, heartbeat(replacement.dir))
	if err := r.DestroyInstance(ctx, replacement.handle.InstanceID); err != nil {
		t.Fatalf("destroy replacement: %v", err)
	}
	sessions, err := r.ListSessions(ctx)
	if err != nil && !errors.Is(err, ports.ErrRuntimeUnavailable) {
		t.Fatalf("ListSessions: %v", err)
	}
	for _, sess := range sessions {
		if strings.HasPrefix(sess.ID, "p9-") {
			t.Fatalf("a P9 session survived exact cleanup: %+v", sess)
		}
	}
}

// A machine reboot (or a lost tmux server): nothing answers on this
// installation's private socket at all. That is a proven absence -- the launch
// takes the bounded retry -- never an indefinitely unprovable runtime.
func TestRealTmuxP9_ServerGoneIsAProvenAbsence(t *testing.T) {
	s := newServer(t)
	r := s.runtime(installationA, "aod-me")
	w := launch(t, r, "p9-server-gone", "launch-1", "stay")
	row := durable(t, w.rec)
	if out, err := exec.Command("tmux", "-L", s.socket, "kill-server").CombinedOutput(); err != nil {
		t.Fatalf("kill-server: %v: %s", err, out)
	}
	wantProof(t, classify(t, row, s.runtime(installationA, "aod-after-reboot"), installationA), domain.WorkerRuntimeAbsent)
}
