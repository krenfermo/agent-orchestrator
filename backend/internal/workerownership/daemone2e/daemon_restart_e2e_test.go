// Package daemone2e is P9's daemon restart / crash E2E (§34, §35).
//
// It builds the real `ao` binary and runs the real daemon against a SCRATCH
// installation: a temporary HOME, data dir, database, run-file and free port,
// and a tmux server on a private socket. Nothing touches ~/.ao, port 3002, the
// user's tmux servers, or any agent: the worker is the deterministic POSIX
// fixture, and AO_FAKE_HARNESS=1 guarantees that even an unexpected launch could
// only ever start the LLM-free fake harness.
//
// The launch under test is seeded with the PRODUCTION coordinator: a work step
// is started and the daemon "dies" (runtime.Goexit) right after the worker's
// tmux session and session row exist and before the launch is confirmed -- the
// C3 window. Then the real daemon boots over it.
//
// Opt-in (it builds a binary and runs a daemon): AO_P9_DAEMON_E2E=1.
package daemone2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/runtime/tmux"
	"github.com/aoagents/agent-orchestrator/backend/internal/daemonmeta"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

var socketSeq atomic.Int64

type scratch struct {
	t         *testing.T
	root      string
	home      string
	dataDir   string
	runFile   string
	port      int
	socket    string
	bin       string
	install   string
	daemon    *exec.Cmd
	daemonLog *os.File
	waitErr   chan error
}

func newScratch(t *testing.T) *scratch {
	t.Helper()
	if os.Getenv("AO_P9_DAEMON_E2E") != "1" {
		t.Skip("set AO_P9_DAEMON_E2E=1 to run the P9 daemon restart E2E")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Fatalf("tmux is required: %v", err)
	}
	root := t.TempDir()
	s := &scratch{
		t: t, root: root,
		home:    filepath.Join(root, "home"),
		dataDir: filepath.Join(root, "data"),
		runFile: filepath.Join(root, "running.json"),
		socket:  fmt.Sprintf("ao-p9-e2e-daemon-%d-%d", os.Getpid(), socketSeq.Add(1)),
		port:    freePort(t),
	}
	if s.port == 3002 {
		t.Fatal("refusing to use port 3002")
	}
	for _, d := range []string{s.home, s.dataDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	install, err := daemonmeta.LoadOrCreateInstallationID(s.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	s.install = install
	s.bin = buildAO(t)
	t.Cleanup(s.cleanup)
	return s
}

func (s *scratch) cleanup() {
	if s.daemon != nil && s.daemon.ProcessState == nil {
		_ = s.daemon.Process.Kill() // our own child only
		<-s.waitErr
	}
	if s.daemonLog != nil {
		_ = s.daemonLog.Close()
		if s.t.Failed() {
			if b, err := os.ReadFile(s.daemonLog.Name()); err == nil {
				tail := string(b)
				if len(tail) > 6000 {
					tail = tail[len(tail)-6000:]
				}
				s.t.Logf("daemon log tail:\n%s", tail)
			}
		}
	}
	socketPath := ""
	if out, err := exec.Command("tmux", "-L", s.socket, "display-message", "-p", "#{socket_path}").Output(); err == nil {
		socketPath = strings.TrimSpace(string(out))
	}
	_ = exec.Command("tmux", "-L", s.socket, "kill-server").Run()
	if strings.Contains(socketPath, "ao-p9-e2e-daemon-") {
		_ = os.Remove(socketPath)
	}
	if out, err := exec.Command("tmux", "-L", s.socket, "list-sessions").CombinedOutput(); err == nil {
		s.t.Errorf("tmux sessions survived cleanup on %s: %s", s.socket, out)
	}
}

// TestMain removes the binary built for the E2E, which outlives any one test.
func TestMain(m *testing.M) {
	code := m.Run()
	if builtBin != "" {
		_ = os.RemoveAll(filepath.Dir(builtBin))
	}
	os.Exit(code)
}

var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
)

func buildAO(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		out, err := exec.Command("go", "env", "GOMOD").Output()
		if err != nil {
			buildErr = err
			return
		}
		moduleDir := filepath.Dir(strings.TrimSpace(string(out)))
		dir, err := os.MkdirTemp("", "ao-p9-e2e-bin-")
		if err != nil {
			buildErr = err
			return
		}
		builtBin = filepath.Join(dir, "ao")
		cmd := exec.Command("go", "build", "-o", builtBin, "./cmd/ao")
		cmd.Dir = moduleDir
		if b, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build ao: %w: %s", err, b)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return builtBin
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// env is the scratch daemon's whole environment: nothing inherited that could
// point it at the real installation, a real provider, or a user's credentials.
func (s *scratch) env() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + s.home,
		"TMPDIR=" + s.root,
		"CODEX_HOME=" + filepath.Join(s.home, ".codex"),
		"AO_DATA_DIR=" + s.dataDir,
		"AO_RUN_FILE=" + s.runFile,
		"AO_PORT=" + strconv.Itoa(s.port),
		"AO_TMUX_SOCKET=" + s.socket,
		"AO_FAKE_HARNESS=1",
		"AO_TELEMETRY_REMOTE=off",
	}
}

func (s *scratch) tmuxRuntime() *tmux.Runtime {
	return tmux.New(tmux.Options{Socket: s.socket, ScratchDir: s.root, InstallationID: s.install, DaemonInstanceID: "aod-seeding"})
}

// ---- seeding -----------------------------------------------------------------

type seeded struct {
	runID     string
	stepID    string
	sessionID domain.SessionID
	workDir   string
}

// tmuxSpawner creates the worker exactly as session_manager does -- row first
// for its id, tmux session stamped with the owner token and this installation --
// and then the daemon dies inside the launch.
type tmuxSpawner struct {
	s *scratch
	// runtimeLaunch, when set, is the launch id the RUNTIME is stamped with,
	// while the row records a different one: a runtime that is not this launch's.
	runtimeLaunch string
	out           *seeded
}

func (sp *tmuxSpawner) Spawn(ctx context.Context, cfg ports.SpawnConfig) (domain.SessionRecord, int, int, error) {
	store := sp.s.openStore()
	defer store.Close()
	now := time.Now().UTC()
	rec, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: cfg.ProjectID, Kind: cfg.Kind, Harness: cfg.Harness, IssueID: cfg.IssueID,
		Activity:  domain.Activity{State: domain.ActivityActive, LastActivityAt: now, LastSignalAt: now},
		CreatedAt: now, UpdatedAt: now, FirstSignalAt: now,
	})
	if err != nil {
		return domain.SessionRecord{}, 0, 0, err
	}
	launch := uuid.NewString()
	runtimeLaunch := launch
	if sp.runtimeLaunch != "" {
		runtimeLaunch = sp.runtimeLaunch
	}
	workDir := filepath.Join(sp.s.root, "worktree")
	if err := os.MkdirAll(workDir, 0o750); err != nil {
		return domain.SessionRecord{}, 0, 0, err
	}
	fixture, _ := filepath.Abs(filepath.Join("..", "tmuxe2e", "testdata", "p9worker.sh"))
	handle, err := sp.s.tmuxRuntime().Create(ctx, ports.RuntimeConfig{
		SessionID: rec.ID, WorkspacePath: workDir,
		Argv:  []string{"sh", fixture, workDir, "stay"},
		Env:   map[string]string{"AO_SUPERVISED_PROCESS": "1"},
		Owner: domain.SessionRuntimeOwnerToken(rec.ID, runtimeLaunch),
	})
	if err != nil {
		return domain.SessionRecord{}, 0, 0, err
	}
	rec.Metadata = domain.SessionMetadata{
		Branch: "ao/p9-e2e", WorkspacePath: workDir,
		RuntimeHandleID: handle.ID, RuntimeInstanceID: handle.InstanceID,
		RuntimeLaunchID: launch, RuntimeOwnerToken: domain.SessionRuntimeOwnerToken(rec.ID, launch),
	}
	if err := store.UpdateSession(ctx, rec); err != nil {
		return domain.SessionRecord{}, 0, 0, err
	}
	sp.out.sessionID, sp.out.workDir = rec.ID, workDir
	runtime.Goexit() // the seeding "daemon" dies before confirming the launch
	return rec, 0, 0, nil
}

func (s *scratch) openStore() *sqlite.Store {
	s.t.Helper()
	store, err := sqlite.Open(s.dataDir)
	if err != nil {
		s.t.Fatalf("open scratch store: %v", err)
	}
	return store
}

func (s *scratch) seedCrashedLaunch(runtimeLaunch string) seeded {
	s.t.Helper()
	ctx := context.Background()
	store := s.openStore()
	repo := filepath.Join(s.root, "repo")
	if err := os.MkdirAll(repo, 0o750); err != nil {
		s.t.Fatal(err)
	}
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "proj-p9", Path: repo, RegisteredAt: time.Now().UTC()}); err != nil {
		s.t.Fatal(err)
	}
	out := &seeded{}
	coord := workflowcore.New(workflowcore.Deps{
		Store: store, Projects: store, SessionFacts: store,
		Spawner: &tmuxSpawner{s: s, runtimeLaunch: runtimeLaunch, out: out},
	})
	created, err := coord.CreateRun(ctx, "proj-p9", "P9 daemon restart E2E")
	if err != nil {
		s.t.Fatal(err)
	}
	out.runID = created.Run.ID
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = coord.StartRun(ctx, out.runID)
	}()
	<-done
	for _, st := range created.Steps {
		if st.Step.Kind == domain.WorkflowStepWork {
			out.stepID = st.Step.ID
		}
	}
	if err := store.Close(); err != nil {
		s.t.Fatal(err)
	}
	if out.sessionID == "" {
		s.t.Fatal("the seeded launch never reached the runtime")
	}
	waitFile(s.t, out.workDir, "ready")
	return *out
}

// ---- the daemon ----------------------------------------------------------------

func (s *scratch) startDaemon() {
	s.t.Helper()
	logf, err := os.OpenFile(filepath.Join(s.root, "daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		s.t.Fatal(err)
	}
	s.daemonLog = logf
	cmd := exec.Command(s.bin, "daemon")
	cmd.Env = s.env()
	cmd.Dir = s.root
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		s.t.Fatalf("start daemon: %v", err)
	}
	s.daemon = cmd
	s.waitErr = make(chan error, 1)
	go func() { s.waitErr <- cmd.Wait() }()
	waitFor(s.t, 120*time.Second, "the scratch daemon to be ready", func() bool {
		var body struct {
			Status  string `json:"status"`
			PID     int    `json:"pid"`
			DataDir string `json:"dataDir"`
		}
		if err := s.getJSON("/readyz", &body); err != nil {
			return false
		}
		return body.Status == "ready" && body.PID == cmd.Process.Pid && body.DataDir == s.dataDir
	})
}

// crashDaemon is §35's abrupt death of THIS test's own daemon child.
func (s *scratch) crashDaemon() {
	s.t.Helper()
	if err := s.daemon.Process.Kill(); err != nil {
		s.t.Fatal(err)
	}
	<-s.waitErr
}

// stopDaemon is the canonical path: the real `ao stop`, discovering and
// verifying this daemon and asking it to shut down.
func (s *scratch) stopDaemon() {
	s.t.Helper()
	out := s.ao("stop")
	if !strings.Contains(out, "stopped") {
		s.t.Fatalf("ao stop said: %s", out)
	}
	select {
	case <-s.waitErr:
	case <-time.After(30 * time.Second):
		s.t.Fatal("the daemon did not exit after ao stop")
	}
}

func (s *scratch) ao(args ...string) string {
	s.t.Helper()
	cmd := exec.Command(s.bin, args...)
	cmd.Env = s.env()
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.t.Fatalf("ao %v: %v: %s", args, err, out)
	}
	return string(out)
}

// postJSON issues a body-less POST and reports only transport/status failure.
func (s *scratch) postJSON(path string) error {
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d%s", s.port, path), strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("POST %s: %d", path, resp.StatusCode)
	}
	return nil
}

func (s *scratch) getJSON(path string, v any) error {
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", s.port, path), http.NoBody)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %d %s", path, resp.StatusCode, b)
	}
	return json.Unmarshal(b, v)
}

type ownershipRow struct {
	StepKind         string `json:"stepKind"`
	StepState        string `json:"stepState"`
	SessionID        string `json:"sessionId"`
	Ownership        string `json:"ownership"`
	Proof            string `json:"proof"`
	RecoveryDecision string `json:"recoveryDecision"`
	RecoveryReason   string `json:"recoveryReason"`
	Detail           string `json:"detail"`
}

func (s *scratch) workOwnership(runID string) ownershipRow {
	s.t.Helper()
	var res struct {
		WorkerOwnership []ownershipRow `json:"workerOwnership"`
	}
	if err := s.getJSON("/api/v1/workflows/"+runID+"/recovery?ownership=1", &res); err != nil {
		s.t.Fatalf("recovery readback: %v", err)
	}
	for _, r := range res.WorkerOwnership {
		if r.StepKind == "work" {
			return r
		}
	}
	s.t.Fatalf("no work row in %+v", res.WorkerOwnership)
	return ownershipRow{}
}

func (s *scratch) runState(runID string) string {
	s.t.Helper()
	var res struct {
		Workflow struct {
			Run struct {
				State string `json:"state"`
			} `json:"run"`
		} `json:"workflow"`
	}
	if err := s.getJSON("/api/v1/workflows/"+runID, &res); err != nil {
		s.t.Fatalf("GET workflow: %v", err)
	}
	return res.Workflow.Run.State
}

// durable reads the scratch DB after the daemon exited: sessions, checkpoint
// phases and the work step, without writing.
func (s *scratch) durable(runID, stepID string) (sessions int, phases []string, step domain.WorkflowStep) {
	s.t.Helper()
	ctx := context.Background()
	store, err := sqlite.OpenReadOnly(ctx, s.dataDir)
	if err != nil {
		s.t.Fatalf("open scratch store read-only: %v", err)
	}
	defer store.Close()
	all, err := store.ListAllSessions(ctx)
	if err != nil {
		s.t.Fatal(err)
	}
	cps, err := store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		s.t.Fatal(err)
	}
	for _, cp := range cps {
		phases = append(phases, cp.DurablePhase)
	}
	steps, err := store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		s.t.Fatal(err)
	}
	for _, st := range steps {
		if st.ID == stepID {
			step = st
		}
	}
	return len(all), phases, step
}

// ---- helpers ------------------------------------------------------------------

func waitFor(t *testing.T, deadline time.Duration, what string, cond func() bool) {
	t.Helper()
	stop := time.Now().Add(deadline)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		if time.Now().After(stop) {
			t.Fatalf("timed out after %s waiting for %s", deadline, what)
		}
		<-tick.C
	}
}

func waitFile(t *testing.T, dir, name string) {
	t.Helper()
	waitFor(t, 15*time.Second, name, func() bool {
		b, err := os.ReadFile(filepath.Join(dir, name))
		return err == nil && len(strings.TrimSpace(string(b))) > 0
	})
}

func heartbeat(dir string) int {
	b, _ := os.ReadFile(filepath.Join(dir, "heartbeat"))
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func assertWorkerAlive(t *testing.T, dir, when string) {
	t.Helper()
	n := heartbeat(dir)
	waitFor(t, 15*time.Second, "the worker fixture to keep beating "+when, func() bool { return heartbeat(dir) > n })
	if _, err := os.Stat(filepath.Join(dir, "done")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the worker fixture ended %s", when)
	}
}

func count(phases []string, phase string) int {
	n := 0
	for _, p := range phases {
		if p == phase {
			n++
		}
	}
	return n
}

// ---- the tests ------------------------------------------------------------------

// §34/§35 happy path: a real daemon boots over a crashed launch whose runtime
// proves it is AO's, ADOPTS it, survives a graceful stop, an abrupt kill and two
// more boots -- never starting a second worker, never disturbing the one running.
func TestP9Daemon_AdoptsAProvenWorkerAcrossGracefulStopAndCrash(t *testing.T) {
	s := newScratch(t)
	seed := s.seedCrashedLaunch("")

	// Boot 1: recovery adopts.
	s.startDaemon()
	row := s.workOwnership(seed.runID)
	if row.Ownership != "proven" || row.SessionID != string(seed.sessionID) || row.StepState != "running" {
		t.Fatalf("boot 1: ownership row = %+v, want the seeded session adopted and running", row)
	}
	assertWorkerAlive(t, seed.workDir, "after adoption")

	// Graceful stop through the real `ao stop`.
	s.stopDaemon()
	if _, err := os.Stat(s.runFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run-file still present after a graceful stop: %v", err)
	}
	assertWorkerAlive(t, seed.workDir, "after the daemon stopped")
	sessions1, phases1, step1 := s.durable(seed.runID, seed.stepID)
	if sessions1 != 1 || step1.SessionID == nil || *step1.SessionID != string(seed.sessionID) {
		t.Fatalf("durable after boot 1: sessions=%d step=%+v", sessions1, step1)
	}

	// Boot 2, then an abrupt death of THIS daemon.
	s.startDaemon()
	if row := s.workOwnership(seed.runID); row.Ownership != "proven" || row.StepState != "running" {
		t.Fatalf("boot 2: %+v", row)
	}
	s.crashDaemon()
	if _, err := os.Stat(s.runFile); err != nil {
		t.Fatalf("an abruptly killed daemon cannot have removed its run-file: %v", err)
	}
	if out := s.ao("status"); !strings.Contains(out, "stale") {
		t.Fatalf("ao status after a crash: %s", out)
	}
	assertWorkerAlive(t, seed.workDir, "after the daemon was killed")

	// Boot 3 over the stale run-file.
	s.startDaemon()
	if row := s.workOwnership(seed.runID); row.Ownership != "proven" || row.SessionID != string(seed.sessionID) || row.StepState != "running" {
		t.Fatalf("boot 3: %+v", row)
	}
	if got := s.runState(seed.runID); got != "running" {
		t.Fatalf("boot 3: run state = %q", got)
	}
	s.stopDaemon()

	sessions3, phases3, step3 := s.durable(seed.runID, seed.stepID)
	if sessions3 != 1 {
		t.Fatalf("sessions after three boots = %d, want 1 — a second worker was created", sessions3)
	}
	if step3.SessionID == nil || *step3.SessionID != string(seed.sessionID) || step3.State != domain.WorkflowStepRunning {
		t.Fatalf("step after three boots = %+v", step3)
	}
	if n := count(phases3, "worker_dispatched"); n != 1 {
		t.Fatalf("worker_dispatched markers = %d, want exactly 1; phases=%v", n, phases3)
	}
	for _, bad := range []string{workflowcore.ReasonWorkerOwnershipUnproven, workflowcore.ReasonWorkerDispatchAmbiguous} {
		if count(phases3, bad) != 0 {
			t.Fatalf("a proven worker was stopped as %s; phases=%v", bad, phases3)
		}
	}
	if len(phases3) != len(phases1) {
		t.Logf("ledger after boot 1: %v", phases1)
		t.Fatalf("recovery is not idempotent across boots: %d -> %d rows; added %v", len(phases1), len(phases3), phases3[len(phases1):])
	}
	assertWorkerAlive(t, seed.workDir, "at the end")
}

// The same crashed launch, but the runtime carries ANOTHER launch's ownership
// token: the real daemon must neither adopt it nor launch beside it, must not
// kill it, and must say ownership is unproven -- not that the worker failed.
func TestP9Daemon_RefusesARuntimeThatIsNotThisLaunch(t *testing.T) {
	s := newScratch(t)
	seed := s.seedCrashedLaunch("a-launch-this-row-never-recorded")

	s.startDaemon()
	// Boot reconciliation runs seconds after the crash, inside the launch's
	// 30-second settle window, where only an ADOPTION may be concluded (a launch
	// that young may still be in flight in another pass). Nothing is adopted --
	// the runtime is not this launch's -- and nothing is stopped yet. The stop is
	// taken by the first pass after the window: a person's Continue, repeated
	// until the run reports it, against a deadline.
	if row := s.workOwnership(seed.runID); row.StepState == "running" && row.SessionID != "" && row.Ownership == "proven" {
		t.Fatalf("an unproven runtime was adopted at boot: %+v", row)
	}
	waitFor(t, 120*time.Second, "the refusal to be taken once the settle window has passed", func() bool {
		_ = s.postJSON("/api/v1/workflows/" + seed.runID + "/continue")
		return s.runState(seed.runID) == "needs_attention"
	})
	row := s.workOwnership(seed.runID)
	if row.Ownership != "unproven" || row.Proof != "owner_mismatch" {
		t.Fatalf("ownership row = %+v, want unproven/owner_mismatch", row)
	}
	if row.RecoveryDecision != "fail_closed" || row.RecoveryReason != "owner_mismatch" {
		t.Fatalf("decision = %s/%s, want fail_closed/owner_mismatch", row.RecoveryDecision, row.RecoveryReason)
	}
	if got := s.runState(seed.runID); got != "needs_attention" {
		t.Fatalf("run state = %q, want needs_attention", got)
	}
	assertWorkerAlive(t, seed.workDir, "after a refusal — AO must not kill what it cannot prove it owns")
	s.stopDaemon()

	sessions, phases, step := s.durable(seed.runID, seed.stepID)
	if sessions != 1 {
		t.Fatalf("sessions = %d, want 1 — a worker was launched beside an unproven runtime", sessions)
	}
	if step.SessionID != nil || step.State == domain.WorkflowStepRunning {
		t.Fatalf("the unproven runtime was bound to the step: %+v", step)
	}
	if count(phases, workflowcore.ReasonWorkerOwnershipUnproven) != 1 {
		t.Fatalf("worker_ownership_unproven recorded %d times, want 1; phases=%v", count(phases, workflowcore.ReasonWorkerOwnershipUnproven), phases)
	}

	// A second boot changes nothing.
	s.startDaemon()
	s.stopDaemon()
	sessions2, phases2, _ := s.durable(seed.runID, seed.stepID)
	if sessions2 != 1 || len(phases2) != len(phases) {
		t.Fatalf("second boot: sessions %d, ledger %d -> %d (added %v)", sessions2, len(phases), len(phases2), phases2[len(phases):])
	}
}

// aoFails runs the real `ao` with an overridden environment and returns its
// output and whether it failed, without failing the test itself.
func (s *scratch) aoFails(env []string, args ...string) (string, bool) {
	s.t.Helper()
	cmd := exec.Command(s.bin, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err != nil
}

func withEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return append(out, key+"="+value)
}

// Case 3 (P9 review): a run-file whose PID is ALIVE but whose daemon identity
// does not match what answers on its port. `ao status` must say it is running
// but unverified, `ao stop` must refuse, the process at that PID must receive
// NO signal, the answering daemon must NOT be shut down, the file must be kept
// -- and a second daemon on the same data dir must refuse to start at all.
func TestP9Daemon_LivePIDWithMismatchedIdentityIsNeverSignalledOrTakenOver(t *testing.T) {
	s := newScratch(t)
	s.startDaemon()
	daemonPID := s.daemon.Process.Pid

	// A live process of this test's own, standing in for a reused PID.
	bystander := exec.Command("sleep", "300")
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	bystanderDone := make(chan error, 1)
	go func() { bystanderDone <- bystander.Wait() }()
	t.Cleanup(func() {
		// The Wait goroutine owns ProcessState; only the channel says whether the
		// bystander already exited (reading ProcessState here is a data race).
		select {
		case <-bystanderDone:
		default:
			_ = bystander.Process.Kill() // our own child only
			<-bystanderDone
		}
	})

	forged := filepath.Join(s.root, "forged-running.json")
	body, _ := json.Marshal(map[string]any{
		"pid": bystander.Process.Pid, "port": s.port, "startedAt": time.Now().UTC(),
		"formatVersion": 2, "instanceId": "aod-" + uuid.NewString(), "installationId": s.install, "dataDir": s.dataDir,
	})
	if err := os.WriteFile(forged, body, 0o600); err != nil {
		t.Fatal(err)
	}
	env := withEnv(s.env(), "AO_RUN_FILE", forged)

	if out, _ := s.aoFails(env, "status"); !strings.Contains(out, "unverified") {
		t.Fatalf("ao status over a live, mismatched PID said: %s", out)
	}
	if out, failed := s.aoFails(env, "stop"); !failed {
		t.Fatalf("ao stop acted on a live PID it could not verify: %s", out)
	}
	select {
	case err := <-bystanderDone:
		t.Fatalf("the process at the recorded PID was signalled: %v", err)
	default:
	}
	if err := bystander.Process.Signal(syscallZero()); err != nil {
		t.Fatalf("the bystander is gone: %v", err)
	}
	if _, err := os.Stat(forged); err != nil {
		t.Fatalf("the run-file of a live, unverified PID was removed: %v", err)
	}
	var ready struct {
		Status string `json:"status"`
		PID    int    `json:"pid"`
	}
	if err := s.getJSON("/readyz", &ready); err != nil || ready.PID != daemonPID {
		t.Fatalf("the answering daemon was taken down: %+v err=%v", ready, err)
	}

	// No takeover by a second daemon on the same data dir either.
	second := exec.Command(s.bin, "daemon")
	second.Env = withEnv(withEnv(s.env(), "AO_RUN_FILE", filepath.Join(s.root, "second-running.json")), "AO_PORT", strconv.Itoa(freePort(t)))
	second.Dir = s.root
	out, err := second.CombinedOutput()
	if err == nil {
		t.Fatalf("a second daemon started on a data dir another daemon holds: %s", out)
	}
	if !strings.Contains(string(out), "refusing to start") {
		t.Fatalf("second daemon failed for another reason: %s", out)
	}
	if err := s.getJSON("/readyz", &ready); err != nil || ready.PID != daemonPID {
		t.Fatalf("the first daemon did not survive a refused takeover: %+v err=%v", ready, err)
	}
	s.stopDaemon()
}
