package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// main_test.go -- the production guardrail, exercised through the REAL ao CLI
// main() in a child process. Every child gets a brand-new environment whose
// HOME is a temporary directory, so the "default data dir" is
// <tmp>/.ao/data and nothing here can reach the operator's real ~/.ao. The
// guard under test: `ao daemon` / `ao server` may use the default data dir
// only with the desktop launch contract (AO_OWNER=app|persistent plus
// AO_APP_RUN_ID) or an explicit data dir.

const childMarker = "AO_CMD_TEST_CHILD"

func TestMain(m *testing.M) {
	if os.Getenv(childMarker) == "1" {
		os.Args = append([]string{"ao"}, strings.Fields(os.Getenv("AO_CMD_TEST_ARGS"))...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type child struct {
	home string
	env  []string
}

func newChild(t *testing.T, extra ...string) child {
	t.Helper()
	home := t.TempDir()
	env := []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + t.TempDir(),
		childMarker + "=1",
	}
	return child{home: home, env: append(env, extra...)}
}

func (c child) run(t *testing.T, timeout time.Duration, args ...string) (string, int, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	cmd.Env = append(append([]string{}, c.env...), "AO_CMD_TEST_ARGS="+strings.Join(args, " "))
	out, err := cmd.CombinedOutput()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	}
	return string(out), code, ctx.Err() != nil
}

func (c child) assertNoDefaultState(t *testing.T) {
	t.Helper()
	for _, p := range []string{filepath.Join(c.home, ".ao", "data"), filepath.Join(c.home, ".ao", "running.json")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("a refused invocation created %s", p)
		}
	}
}

// Requirements 1 and 5: no owner contract, no explicit dir => fail closed,
// and nothing is created under the (temporary) default location.
func TestDaemonAndServerRefuseTheImplicitDefaultDataDir(t *testing.T) {
	web := t.TempDir()
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("<html></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		extra []string
		args  []string
	}{
		"daemon, bare environment":            {nil, []string{"daemon"}},
		"server, bare environment":            {nil, []string{"server", "--port", "3990", "--web-root", web}},
		"daemon, owner without run id":        {[]string{"AO_OWNER=app"}, []string{"daemon"}},
		"daemon, arbitrary owner and run id":  {[]string{"AO_OWNER=manual", "AO_APP_RUN_ID=x"}, []string{"daemon"}},
		"daemon, run id without owner":        {[]string{"AO_APP_RUN_ID=apprun-x"}, []string{"daemon"}},
		"daemon, AO_DATA_DIR set but empty":   {[]string{"AO_DATA_DIR="}, []string{"daemon"}},
		"server, empty --data-dir is default": {nil, []string{"server", "--data-dir=", "--port", "3991", "--web-root", web}},
	} {
		t.Run(name, func(t *testing.T) {
			c := newChild(t, tc.extra...)
			out, code, timedOut := c.run(t, 30*time.Second, tc.args...)
			if timedOut {
				t.Fatalf("the child ran until the timeout: a daemon started\n%s", out)
			}
			if code == 0 || !strings.Contains(out, "refusing to use the default AO data dir") {
				t.Fatalf("exit=%d, want a refusal; output:\n%s", code, out)
			}
			c.assertNoDefaultState(t)
		})
	}
}

// Offline writers open the store with sqlite.Open, which migrates it: the
// same guard applies, and a refusal opens nothing. (The desktop app never
// runs them, so only an explicit AO_DATA_DIR authorizes them in practice.)
func TestOfflineWritersRefuseTheImplicitDefaultDataDir(t *testing.T) {
	for name, args := range map[string][]string{
		"import":             {"import", "--yes", "--from", "/nonexistent-legacy-root"},
		"usage backfill ttl": {"usage", "backfill-cache-ttl"},
	} {
		t.Run(name, func(t *testing.T) {
			c := newChild(t)
			out, code, timedOut := c.run(t, 30*time.Second, args...)
			if timedOut || code == 0 || !strings.Contains(out, "refusing to use the default AO data dir") {
				t.Fatalf("exit=%d timedOut=%v, want a refusal; output:\n%s", code, timedOut, out)
			}
			c.assertNoDefaultState(t)
		})
	}
}

// Requirement 4: an invalid argument or flag never starts a daemon.
func TestInvalidInvocationsNeverStartADaemon(t *testing.T) {
	for name, args := range map[string][]string{
		"--version":           {"--version"},
		"unknown root flag":   {"--definitely-not-a-flag"},
		"unknown command":     {"serve"},
		"daemon with a flag":  {"daemon", "--data-dir", "/tmp/x"},
		"daemon with an arg":  {"daemon", "extra"},
		"server unknown flag": {"server", "--datadir", "/tmp/x"},
	} {
		t.Run(name, func(t *testing.T) {
			// Even WITH the desktop contract, a malformed invocation must not start.
			c := newChild(t, "AO_OWNER=app", "AO_APP_RUN_ID=apprun-test")
			out, _, timedOut := c.run(t, 30*time.Second, args...)
			if timedOut {
				t.Fatalf("the child ran until the timeout: a daemon started\n%s", out)
			}
			c.assertNoDefaultState(t)
		})
	}
}

// Requirements 2 and 3: an explicit scratch dir, or the desktop contract,
// still starts the daemon exactly as before. The daemon runs inside the
// temporary HOME and is shut down over its own loopback API.
func TestAuthorizedStartsStillWork(t *testing.T) {
	for name, extra := range map[string]func(home string) []string{
		"explicit AO_DATA_DIR (scratch)": func(home string) []string {
			return []string{"AO_DATA_DIR=" + filepath.Join(home, "scratch", "data"), "AO_RUN_FILE=" + filepath.Join(home, "scratch", "running.json")}
		},
		"desktop contract app":        func(string) []string { return []string{"AO_OWNER=app", "AO_APP_RUN_ID=apprun-test"} },
		"desktop contract persistent": func(string) []string { return []string{"AO_OWNER=persistent", "AO_APP_RUN_ID=apprun-test"} },
	} {
		t.Run(name, func(t *testing.T) {
			// A running daemon keeps writing into its HOME until it has fully
			// exited, so this HOME is removed best-effort after the wait below
			// instead of by t.TempDir's strict cleanup.
			home, err := os.MkdirTemp("", "ao-guard-home-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(home) })
			c := child{home: home, env: []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "TMPDIR=" + filepath.Join(home, "tmp"), childMarker + "=1"}}
			if err := os.MkdirAll(filepath.Join(home, "tmp"), 0o700); err != nil {
				t.Fatal(err)
			}
			c.env = append(c.env, extra(c.home)...)
			port := freePort(t)
			c.env = append(c.env, "AO_PORT="+strconv.Itoa(port))
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
			cmd.Env = append(append([]string{}, c.env...), "AO_CMD_TEST_ARGS=daemon")
			var out strings.Builder
			cmd.Stdout, cmd.Stderr = &out, &out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan struct{})
			go func() { _ = cmd.Wait(); close(exited) }()
			defer func() {
				select {
				case <-exited:
				case <-time.After(20 * time.Second):
					_ = cmd.Process.Kill()
					<-exited
				}
			}()
			base := "http://127.0.0.1:" + strconv.Itoa(port)
			deadline := time.Now().Add(45 * time.Second)
			for {
				if resp, err := http.Get(base + "/healthz"); err == nil {
					_ = resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						break
					}
				}
				if time.Now().After(deadline) {
					t.Fatalf("authorized daemon never became healthy:\n%s", out.String())
				}
				time.Sleep(200 * time.Millisecond)
			}
			if resp, err := http.Post(base+"/shutdown", "application/json", strings.NewReader("{}")); err == nil {
				_ = resp.Body.Close()
			}
			if strings.Contains(out.String(), "refusing to use the default AO data dir") {
				t.Fatalf("authorized start was refused:\n%s", out.String())
			}
		})
	}
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
