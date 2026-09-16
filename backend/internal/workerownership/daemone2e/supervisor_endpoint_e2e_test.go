//go:build !windows

package daemone2e

// Supervisor endpoint identity against the real daemon binary (same opt-in as
// the rest of this package: AO_P9_DAEMON_E2E=1).
//
// Connecting to a supervisor endpoint ARMS the daemon's watchdog (it stops 5 s
// after its last client leaves), so these tests dial a live endpoint only at
// the very end of each scenario.

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/daemon/supervisor"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

// shortRunFileDir keeps <dir>/supervise-<16 hex>.sock inside sockaddr_un.
func shortRunFileDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ao-sup-e2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func readRunFile(t *testing.T, path string) *runfile.Info {
	t.Helper()
	info, err := runfile.Read(path)
	if err != nil || info == nil {
		t.Fatalf("run-file %s: %+v, %v", path, info, err)
	}
	return info
}

func isSocket(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}

func assertEndpointOfInstance(t *testing.T, dir string, info *runfile.Info) {
	t.Helper()
	name, err := supervisor.SocketName(info.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, name); info.SupervisorAddress != want {
		t.Fatalf("supervisorAddress = %q, want %q (named after instance %s)", info.SupervisorAddress, want, info.InstanceID)
	}
	if !isSocket(info.SupervisorAddress) {
		t.Fatalf("published supervisor endpoint %s is not a live socket file", info.SupervisorAddress)
	}
}

func TestSupervisorEndpoint_PerInstanceAcrossCrashAndRestart(t *testing.T) {
	s := newScratch(t)
	dir := shortRunFileDir(t)
	s.runFile = filepath.Join(dir, "running.json")

	s.startDaemon()
	a := readRunFile(t, s.runFile)
	assertEndpointOfInstance(t, dir, a)
	if legacy := filepath.Join(dir, "supervise.sock"); isSocket(legacy) {
		t.Fatalf("legacy shared endpoint %s was created", legacy)
	}

	// Headless: no client ever connected, so the watchdog is not armed and the
	// daemon keeps running well past the 5 s grace.
	time.Sleep(7 * time.Second)
	if s.daemon.ProcessState != nil {
		t.Fatal("a headless daemon with no supervisor client stopped itself")
	}

	// Abrupt death leaves A's socket file behind.
	s.crashDaemon()
	if !isSocket(a.SupervisorAddress) {
		t.Fatalf("expected A's endpoint %s to survive SIGKILL as a stale socket", a.SupervisorAddress)
	}

	// B starts on the same run-file: new instance, new endpoint; A's stale
	// socket is removed because the run-file lock proves A is gone.
	s.startDaemon()
	b := readRunFile(t, s.runFile)
	if b.InstanceID == a.InstanceID {
		t.Fatal("restart kept the instance id")
	}
	assertEndpointOfInstance(t, dir, b)
	if b.SupervisorAddress == a.SupervisorAddress {
		t.Fatalf("instances A and B share endpoint %s", a.SupervisorAddress)
	}
	if _, err := os.Lstat(a.SupervisorAddress); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("A's proven-stale endpoint was not cleaned up (err=%v)", err)
	}

	// A client that linked A can never reach B through A's address.
	if conn, err := net.DialTimeout("unix", a.SupervisorAddress, time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("dialing instance A's endpoint reached a live listener after A died")
	}
	conn, err := net.DialTimeout("unix", b.SupervisorAddress, time.Second)
	if err != nil {
		t.Fatalf("B's published endpoint is not dialable: %v", err)
	}
	_ = conn.Close()

	// Graceful stop removes B's endpoint and run-file.
	s.stopDaemon()
	if _, err := os.Lstat(b.SupervisorAddress); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("B's endpoint survived a graceful stop (err=%v)", err)
	}
}

func TestSupervisorEndpoint_TwoDaemonsWithRunFilesInOneDirectoryNeverShare(t *testing.T) {
	dir := shortRunFileDir(t)
	first := newScratch(t)
	first.runFile = filepath.Join(dir, "running.json")
	second := newScratch(t)
	second.runFile = filepath.Join(dir, "sandbox.json")

	first.startDaemon()
	second.startDaemon()
	a := readRunFile(t, first.runFile)
	b := readRunFile(t, second.runFile)
	assertEndpointOfInstance(t, dir, a)
	assertEndpointOfInstance(t, dir, b)
	if a.InstallationID == b.InstallationID {
		t.Fatal("fixture error: the two daemons share an installation")
	}
	if a.SupervisorAddress == b.SupervisorAddress {
		t.Fatalf("two installations share endpoint %s", a.SupervisorAddress)
	}
	// The second start did not take over or remove the first daemon's endpoint.
	for _, addr := range []string{a.SupervisorAddress, b.SupervisorAddress} {
		conn, err := net.DialTimeout("unix", addr, time.Second)
		if err != nil {
			t.Fatalf("endpoint %s not dialable with both daemons running: %v", addr, err)
		}
		defer conn.Close()
	}
}
