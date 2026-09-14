package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/daemonmeta"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

// P9 §13–§15 — discovery and stop must PROVE which daemon they found.

type fakeDaemon struct {
	srv       *httptest.Server
	pid       int
	instance  string
	dataDir   string
	alive     atomic.Bool
	mu        sync.Mutex
	probes    int
	shutdowns int
}

func newFakeDaemon(t *testing.T, pid int, instance, dataDir string) *fakeDaemon {
	t.Helper()
	d := &fakeDaemon{pid: pid, instance: instance, dataDir: dataDir}
	d.alive.Store(true)
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		switch r.URL.Path {
		case "/healthz", "/readyz":
			d.probes++
			status := "ok"
			if r.URL.Path == "/readyz" {
				status = "ready"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": status, "service": daemonmeta.ServiceName, "pid": d.pid,
				"instanceId": d.instance, "dataDir": d.dataDir,
			})
		case "/shutdown":
			d.shutdowns++
			d.alive.Store(false)
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *fakeDaemon) port(t *testing.T) int {
	t.Helper()
	u, err := url.Parse(d.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (d *fakeDaemon) counts() (probes, shutdowns int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.probes, d.shutdowns
}

type discoveryEnv struct {
	dataDir  string
	runFile  string // AO_RUN_FILE (the default convention)
	dataFile string // <dataDir>/running.json (the --data-dir convention)
}

func newDiscoveryEnv(t *testing.T) discoveryEnv {
	t.Helper()
	root := t.TempDir()
	env := discoveryEnv{
		dataDir: filepath.Join(root, "data"),
		runFile: filepath.Join(root, "running.json"),
	}
	env.dataFile = filepath.Join(env.dataDir, "running.json")
	t.Setenv("AO_DATA_DIR", env.dataDir)
	t.Setenv("AO_RUN_FILE", env.runFile)
	return env
}

func writeDiscoveryRunFile(t *testing.T, path string, info runfile.Info) runfile.Info {
	t.Helper()
	if info.StartedAt.IsZero() {
		info.StartedAt = time.Date(2026, 9, 13, 20, 0, 0, 0, time.UTC)
	}
	if err := runfile.Write(path, info); err != nil {
		t.Fatal(err)
	}
	return info
}

func discoveryContext(alive func(pid int) bool) *commandContext {
	return &commandContext{deps: Deps{
		HTTPClient:   &http.Client{Timeout: time.Second},
		ProcessAlive: alive,
		Now:          func() time.Time { return time.Date(2026, 9, 13, 21, 0, 0, 0, time.UTC) },
		Sleep:        func(time.Duration) {},
	}.withDefaults()}
}

func TestDiscovery_CanonicalLiveDaemonIsVerified(t *testing.T) {
	env := newDiscoveryEnv(t)
	d := newFakeDaemon(t, 4100, "aod-live", env.dataDir)
	writeDiscoveryRunFile(t, env.runFile, runfile.Info{PID: 4100, Port: d.port(t), InstanceID: "aod-live", DataDir: env.dataDir, FormatVersion: runfile.CurrentFormatVersion})
	st, err := discoveryContext(func(pid int) bool { return pid == 4100 }).inspectDaemon(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.State != stateReady || !st.owned || st.InstanceID != "aod-live" {
		t.Fatalf("status = %+v, want a verified ready daemon", st)
	}
}

// The migration-0170 shape: the default run-file is absent and the daemon wrote
// <data dir>/running.json. It must be found, not reported STOPPED.
func TestDiscovery_DaemonUnderTheDataDirConventionIsFound(t *testing.T) {
	env := newDiscoveryEnv(t)
	d := newFakeDaemon(t, 4200, "aod-datadir", env.dataDir)
	writeDiscoveryRunFile(t, env.dataFile, runfile.Info{PID: 4200, Port: d.port(t), InstanceID: "aod-datadir", DataDir: env.dataDir})
	st, err := discoveryContext(func(pid int) bool { return pid == 4200 }).inspectDaemon(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.State != stateReady || st.RunFile != env.dataFile {
		t.Fatalf("status = %s via %s, want ready via %s", st.State, st.RunFile, env.dataFile)
	}
}

func TestDiscovery_MissingRunFilesAreStopped(t *testing.T) {
	newDiscoveryEnv(t)
	st, err := discoveryContext(func(int) bool { return true }).inspectDaemon(context.Background())
	if err != nil || st.State != stateStopped {
		t.Fatalf("status = %s err=%v, want stopped", st.State, err)
	}
}

func TestStop_StalePIDRemovesOnlyThatStaleFile(t *testing.T) {
	env := newDiscoveryEnv(t)
	writeDiscoveryRunFile(t, env.runFile, runfile.Info{PID: 4300, Port: 1, InstanceID: "aod-dead", DataDir: env.dataDir})
	st, err := discoveryContext(func(int) bool { return false }).stopDaemon(context.Background(), stopOptions{timeout: time.Second})
	if err != nil || st.State != stateStopped {
		t.Fatalf("stop = %s err=%v", st.State, err)
	}
	if info, _ := runfile.Read(env.runFile); info != nil {
		t.Fatal("the stale run-file was not removed")
	}
}

// A reused PID: something is alive at the recorded PID and nothing AO answers on
// the port. RUNNING BUT UNVERIFIED -- never stopped, never signalled, file kept.
func TestStop_ReusedPIDWithNoAOProbeIsNeverActedOn(t *testing.T) {
	env := newDiscoveryEnv(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	port := func() int { u, _ := url.Parse(dead.URL); p, _ := strconv.Atoi(u.Port()); return p }()
	dead.Close()
	writeDiscoveryRunFile(t, env.runFile, runfile.Info{PID: 4400, Port: port, InstanceID: "aod-gone", DataDir: env.dataDir})
	c := discoveryContext(func(int) bool { return true })
	st, err := c.inspectDaemon(context.Background())
	if err != nil || st.State != stateUnhealthy || st.owned {
		t.Fatalf("status = %+v err=%v, want unhealthy and unowned", st, err)
	}
	if _, err := c.stopDaemon(context.Background(), stopOptions{timeout: time.Second}); err == nil {
		t.Fatal("stop acted on a process it could not verify")
	}
	if info, _ := runfile.Read(env.runFile); info == nil {
		t.Fatal("an unverified daemon's run-file was deleted")
	}
}

// A reused PID answered by a DIFFERENT AO daemon: the file is stale for the pid
// it names, and removing it is safe only by exact match.
func TestStop_ReusedPIDAnsweredByAnotherDaemonIsStale(t *testing.T) {
	env := newDiscoveryEnv(t)
	d := newFakeDaemon(t, 9999, "aod-other", env.dataDir)
	writeDiscoveryRunFile(t, env.runFile, runfile.Info{PID: 4500, Port: d.port(t), InstanceID: "aod-old", DataDir: env.dataDir})
	st, err := discoveryContext(func(int) bool { return true }).stopDaemon(context.Background(), stopOptions{timeout: time.Second})
	if err != nil || st.State != stateStopped {
		t.Fatalf("stop = %s err=%v", st.State, err)
	}
	if _, shutdowns := d.counts(); shutdowns != 0 {
		t.Fatal("a daemon the run-file does not name was asked to shut down")
	}
}

func TestStop_WrongInstanceIsUnverifiedAndRefused(t *testing.T) {
	env := newDiscoveryEnv(t)
	d := newFakeDaemon(t, 4600, "aod-answering", env.dataDir)
	writeDiscoveryRunFile(t, env.runFile, runfile.Info{PID: 4600, Port: d.port(t), InstanceID: "aod-recorded", DataDir: env.dataDir})
	c := discoveryContext(func(int) bool { return true })
	st, _ := c.inspectDaemon(context.Background())
	if st.State != stateUnverified {
		t.Fatalf("state = %s, want running_unverified", st.State)
	}
	if _, err := c.stopDaemon(context.Background(), stopOptions{timeout: time.Second}); err == nil || !strings.Contains(err.Error(), "not provably") {
		t.Fatalf("stop err = %v, want a refusal", err)
	}
	if _, shutdowns := d.counts(); shutdowns != 0 {
		t.Fatal("an unverified daemon was asked to shut down")
	}
}

func TestStop_RunFileOfAnotherDataDirIsForeignAndNeverProbed(t *testing.T) {
	env := newDiscoveryEnv(t)
	d := newFakeDaemon(t, 4700, "aod-foreign", "/somewhere/else")
	writeDiscoveryRunFile(t, env.runFile, runfile.Info{PID: 4700, Port: d.port(t), InstanceID: "aod-foreign", DataDir: "/somewhere/else"})
	c := discoveryContext(func(int) bool { return true })
	if _, err := c.stopDaemon(context.Background(), stopOptions{timeout: time.Second}); err == nil {
		t.Fatal("stop acted on another installation's daemon")
	}
	if probes, shutdowns := d.counts(); probes != 0 || shutdowns != 0 {
		t.Fatalf("foreign daemon probed %d / shut down %d times", probes, shutdowns)
	}
	if info, _ := runfile.Read(env.runFile); info == nil {
		t.Fatal("another installation's run-file was deleted")
	}
}

func TestDiscovery_ProbeServingAnotherDataDirIsForeign(t *testing.T) {
	env := newDiscoveryEnv(t)
	d := newFakeDaemon(t, 4800, "aod-x", "/other/data")
	writeDiscoveryRunFile(t, env.runFile, runfile.Info{PID: 4800, Port: d.port(t)}) // legacy file: no identity
	st, _ := discoveryContext(func(int) bool { return true }).inspectDaemon(context.Background())
	if st.State != stateForeign || st.owned {
		t.Fatalf("status = %+v, want foreign", st)
	}
}

func TestDiscovery_TwoVerifiedDaemonsRefuseToGuess(t *testing.T) {
	env := newDiscoveryEnv(t)
	a := newFakeDaemon(t, 4901, "aod-a", env.dataDir)
	b := newFakeDaemon(t, 4902, "aod-b", env.dataDir)
	writeDiscoveryRunFile(t, env.runFile, runfile.Info{PID: 4901, Port: a.port(t), InstanceID: "aod-a", DataDir: env.dataDir})
	writeDiscoveryRunFile(t, env.dataFile, runfile.Info{PID: 4902, Port: b.port(t), InstanceID: "aod-b", DataDir: env.dataDir})
	c := discoveryContext(func(int) bool { return true })
	if _, err := c.inspectDaemon(context.Background()); err == nil || !strings.Contains(err.Error(), "two AO daemons") {
		t.Fatalf("err = %v, want a refusal to guess", err)
	}
	if _, err := c.stopDaemon(context.Background(), stopOptions{timeout: time.Second}); err == nil {
		t.Fatal("stop picked one of two daemons")
	}
	for _, d := range []*fakeDaemon{a, b} {
		if _, shutdowns := d.counts(); shutdowns != 0 {
			t.Fatal("a daemon was shut down while two claimed the installation")
		}
	}
}

func TestDiscovery_BothConventionsNamingOneDaemonAreOneDaemon(t *testing.T) {
	env := newDiscoveryEnv(t)
	d := newFakeDaemon(t, 5000, "aod-one", env.dataDir)
	info := writeDiscoveryRunFile(t, env.runFile, runfile.Info{PID: 5000, Port: d.port(t), InstanceID: "aod-one", DataDir: env.dataDir})
	writeDiscoveryRunFile(t, env.dataFile, info)
	st, err := discoveryContext(func(int) bool { return true }).inspectDaemon(context.Background())
	if err != nil || st.State != stateReady || len(st.Candidates) != 2 {
		t.Fatalf("status = %s err=%v candidates=%d, want ready with both locations reported", st.State, err, len(st.Candidates))
	}
}

// The canonical stop: verified, asked to shut down through its own endpoint,
// and confirmed gone -- the file removed only because it names that incarnation.
func TestStop_VerifiedDaemonShutsDownAndIsVerifiedGone(t *testing.T) {
	env := newDiscoveryEnv(t)
	d := newFakeDaemon(t, 5100, "aod-stop", env.dataDir)
	writeDiscoveryRunFile(t, env.runFile, runfile.Info{PID: 5100, Port: d.port(t), InstanceID: "aod-stop", DataDir: env.dataDir})
	c := discoveryContext(func(pid int) bool { return pid == 5100 && d.alive.Load() })
	st, err := c.stopDaemon(context.Background(), stopOptions{timeout: time.Second})
	if err != nil || st.State != stateStopped {
		t.Fatalf("stop = %s err=%v", st.State, err)
	}
	if _, shutdowns := d.counts(); shutdowns != 1 {
		t.Fatalf("shutdown requests = %d, want exactly 1", shutdowns)
	}
	if info, _ := runfile.Read(env.runFile); info != nil {
		t.Fatal("the stopped daemon's run-file was left behind")
	}
}
