//go:build !windows

package supervisor

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	instA = "aod-11111111-1111-4111-8111-111111111111"
	instB = "aod-22222222-2222-4222-8222-222222222222"
)

// socketTempDir is a temp dir short enough for a Unix socket address
// (sockaddr_un is 104 bytes on macOS); t.TempDir() inherits TMPDIR, which can
// be arbitrarily deep.
func socketTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if len(dir)+len("/supervise-0123456789abcdef.sock")+1 <= 104 {
		return dir
	}
	short, err := os.MkdirTemp("/tmp", "ao-sock")
	if err != nil {
		t.Skipf("TMPDIR path %q leaves no room for a Unix socket address and /tmp is unavailable: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	return short
}

func mustSocketName(t *testing.T, instance string) string {
	t.Helper()
	name, err := SocketName(instance)
	if err != nil {
		t.Fatal(err)
	}
	return name
}

// staleSocket leaves a real socket file with no listener behind, as a daemon
// killed without unlinking would.
func staleSocket(t *testing.T, path string) {
	t.Helper()
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("create socket %s: %v", path, err)
	}
	ln.SetUnlinkOnClose(false)
	_ = ln.Close()
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("stale socket fixture missing: %v", err)
	}
}

func TestListen_endpointIsPerInstanceAndDialable(t *testing.T) {
	t.Parallel()
	dir := socketTempDir(t)
	ln, addr, err := Listen(filepath.Join(dir, "running.json"), instA, nil)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	if want := filepath.Join(dir, mustSocketName(t, instA)); addr != want {
		t.Fatalf("addr = %q, want %q", addr, want)
	}
	conn, err := net.Dial("unix", addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	_ = conn.Close()
}

func TestListen_failsClosedWithoutInstanceIdentity(t *testing.T) {
	t.Parallel()
	dir := socketTempDir(t)
	if ln, _, err := Listen(filepath.Join(dir, "running.json"), "", nil); err == nil {
		_ = ln.Close()
		t.Fatal("Listen without an instance identity succeeded")
	}
}

// Two run-files in one directory, two concurrent daemons: two endpoints, and
// each connection reaches the daemon it was addressed to.
func TestListen_twoDaemonsSameDirectoryNeverShare(t *testing.T) {
	t.Parallel()
	dir := socketTempDir(t)
	lnA, addrA, err := Listen(filepath.Join(dir, "running.json"), instA, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lnA.Close()
	lnB, addrB, err := Listen(filepath.Join(dir, "sandbox.json"), instB, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lnB.Close()
	if addrA == addrB {
		t.Fatalf("both daemons listen on %q", addrA)
	}
	for _, c := range []struct {
		addr string
		ln   net.Listener
	}{{addrA, lnA}, {addrB, lnB}} {
		accepted := make(chan struct{})
		go func(ln net.Listener) {
			if conn, err := ln.Accept(); err == nil {
				_ = conn.Close()
				close(accepted)
			}
		}(c.ln)
		conn, err := net.Dial("unix", c.addr)
		if err != nil {
			t.Fatalf("Dial %s: %v", c.addr, err)
		}
		<-accepted
		_ = conn.Close()
	}
}

// A second daemon starting never removes or takes over a live daemon's endpoint.
func TestListen_newDaemonNeverTouchesALiveEndpoint(t *testing.T) {
	t.Parallel()
	dir := socketTempDir(t)
	lnA, addrA, err := Listen(filepath.Join(dir, "running.json"), instA, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lnA.Close()
	// B even claims A as its predecessor: a live listener is never "stale".
	lnB, _, err := Listen(filepath.Join(dir, "running.json"), instB, &Prior{InstanceID: instA, Address: addrA})
	if err != nil {
		t.Fatal(err)
	}
	defer lnB.Close()
	conn, err := net.Dial("unix", addrA)
	if err != nil {
		t.Fatalf("A's endpoint was disturbed by B's start: %v", err)
	}
	_ = conn.Close()
}

func TestListen_removesOnlyAProvenStalePredecessorSocket(t *testing.T) {
	t.Parallel()
	dir := socketTempDir(t)
	staleA := filepath.Join(dir, mustSocketName(t, instA))
	staleSocket(t, staleA)

	ln, _, err := Listen(filepath.Join(dir, "running.json"), instB, &Prior{InstanceID: instA, Address: staleA})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if _, err := os.Lstat(staleA); !os.IsNotExist(err) {
		t.Fatalf("proven stale predecessor socket still present (err=%v)", err)
	}
}

func TestListen_keepsAnythingNotProvenStale(t *testing.T) {
	t.Parallel()
	dir := socketTempDir(t)
	third := "aod-33333333-3333-4333-8333-333333333333"

	// 1. The recorded address is not the name the recorded instance would use.
	notItsName := filepath.Join(dir, mustSocketName(t, third))
	staleSocket(t, notItsName)
	// 2. The legacy shared name.
	legacy := filepath.Join(dir, "supervise.sock")
	staleSocket(t, legacy)
	// 3. The right name, but a regular file rather than a socket.
	regular := filepath.Join(dir, mustSocketName(t, instA))
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, prior := range []*Prior{
		{InstanceID: instA, Address: notItsName},
		{InstanceID: "", Address: legacy},
		{InstanceID: instA, Address: regular},
		nil,
	} {
		ln, _, err := Listen(filepath.Join(dir, "running.json"), instB, prior)
		if err != nil {
			t.Fatalf("Listen with prior %+v: %v", prior, err)
		}
		_ = ln.Close()
	}
	for _, p := range []string{notItsName, legacy, regular} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s was removed without proof: %v", p, err)
		}
	}
}

func TestListen_pathTooLongFailsClosed(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(socketTempDir(t), strings.Repeat("d", 90))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, addr, err := Listen(filepath.Join(dir, "running.json"), instA, nil)
	if err == nil {
		_ = ln.Close()
		t.Fatalf("Listen accepted a %d-byte socket path %q", len(addr), addr)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "supervise-") {
			t.Fatalf("a socket file %q was created despite the path limit", e.Name())
		}
	}
}

func TestListen_unlinkOnClose(t *testing.T) {
	t.Parallel()
	dir := socketTempDir(t)
	ln, addr, err := Listen(filepath.Join(dir, "running.json"), instA, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	if _, err := os.Stat(addr); !os.IsNotExist(err) {
		t.Fatalf("socket still present after Close (err=%v)", err)
	}
}
