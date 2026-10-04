//go:build !windows

// Package workflowcycle is Frente 1's end-to-end test of the whole workflow
// cycle -- workflow -> worker -> review -> verify -> completion -- through a
// REAL `ao daemon` whose agents run inside a REAL tmux server.
//
// Everything is scratch: HOME, TMPDIR, CODEX_HOME, the data dir (and so the
// database and the worktrees), the run-file, the port, the tmux socket and the
// git repository are all created under t.TempDir(). ~/.ao, the user's tmux
// servers and port 3002 are never touched.
//
// No model is called. The daemon's PATH holds exactly one agent source: a
// private bin directory where testdata/agent-shim.sh is installed as `claude`
// and `codex`. The real CLIs on this machine are not reachable -- the PATH
// carries no directory that holds them, and HOME/CODEX_HOME point at empty
// scratch directories -- so an unexpected launch could only ever start the
// shim. The shim acts as an agent through the surfaces a real agent uses (hook
// commands, `ao review submit`, the workspace), and nothing else: AO's own code
// is the production daemon, unmodified.
//
// Opt-in, because it builds the binary and runs a daemon:
//
//	AO_WORKFLOW_TMUX_E2E=1 go test ./e2e/workflowcycle/ -v -count=1 -timeout 15m
package workflowcycle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const gateEnv = "AO_WORKFLOW_TMUX_E2E"

var socketSeq atomic.Int64

type scratch struct {
	t        *testing.T
	root     string
	home     string
	dataDir  string
	runFile  string
	binDir   string
	traceDir string
	repo     string
	port     int
	socket   string
	aoBin    string

	daemon    *exec.Cmd
	daemonLog *os.File
	waitErr   chan error

	instanceID     string
	installationID string
}

func newScratch(t *testing.T) *scratch {
	t.Helper()
	if os.Getenv(gateEnv) != "1" {
		t.Skipf("set %s=1 to run the workflow-cycle E2E (real daemon, real tmux)", gateEnv)
	}
	for _, bin := range []string{"tmux", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			// The gate is set: a missing prerequisite is a failure, not a pass.
			t.Fatalf("%s is required by the workflow-cycle E2E: %v", bin, err)
		}
	}
	// Not t.TempDir(): its path under $TMPDIR is long enough to push the
	// daemon's unix sockets past the 103-byte sun_path limit.
	raw, err := os.MkdirTemp("/tmp", "aoce-")
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// AO_E2E_KEEP_DIR=1 keeps the scratch installation (daemon log, DB,
		// traces) for diagnosis. It is still under /tmp, never under ~/.ao.
		if os.Getenv("AO_E2E_KEEP_DIR") == "1" {
			t.Logf("scratch installation kept at %s", root)
			return
		}
		_ = os.RemoveAll(root)
	})
	s := &scratch{
		t:        t,
		root:     root,
		home:     filepath.Join(root, "home"),
		dataDir:  filepath.Join(root, "data"),
		runFile:  filepath.Join(root, "running.json"),
		binDir:   filepath.Join(root, "bin"),
		traceDir: filepath.Join(root, "trace"),
		repo:     filepath.Join(root, "repo"),
		socket:   fmt.Sprintf("ao-cycle-e2e-%d-%d", os.Getpid(), socketSeq.Add(1)),
		port:     freePort(t),
	}
	if s.port == 3001 || s.port == 3002 {
		t.Fatalf("refusing to use a well-known AO port (%d)", s.port)
	}
	home, _ := os.UserHomeDir()
	if home != "" && strings.HasPrefix(s.dataDir, filepath.Join(home, ".ao")) {
		t.Fatalf("scratch data dir %s is under ~/.ao", s.dataDir)
	}
	for _, d := range []string{s.home, s.dataDir, s.binDir, s.traceDir, filepath.Join(s.home, ".codex")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	// A deterministic git identity for anything run under the scratch HOME.
	writeFile(t, filepath.Join(s.home, ".gitconfig"), "[user]\n\tname = ao-e2e\n\temail = ao-e2e@example.invalid\n[init]\n\tdefaultBranch = main\n")
	s.installBin()
	s.aoBin = buildAO(t)
	t.Cleanup(s.cleanup)
	return s
}

// installBin builds the daemon's whole PATH: the agent shim under both CLI
// names, and links to the few system tools AO and the shim shell out to.
func (s *scratch) installBin() {
	s.t.Helper()
	shim, err := filepath.Abs(filepath.Join("testdata", "agent-shim.sh"))
	if err != nil {
		s.t.Fatal(err)
	}
	body, err := os.ReadFile(shim)
	if err != nil {
		s.t.Fatal(err)
	}
	for _, name := range []string{"claude", "codex"} {
		if err := os.WriteFile(filepath.Join(s.binDir, name), body, 0o755); err != nil {
			s.t.Fatal(err)
		}
	}
	for _, tool := range []string{"tmux", "git"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			s.t.Fatal(err)
		}
		if err := os.Symlink(p, filepath.Join(s.binDir, tool)); err != nil {
			s.t.Fatal(err)
		}
	}
}

// env is the scratch daemon's entire environment. Nothing is inherited.
func (s *scratch) env() []string {
	return []string{
		// The private bin first, then only system directories. No directory
		// that holds a real claude or codex is on this PATH.
		"PATH=" + s.binDir + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + s.home,
		"TMPDIR=" + s.root,
		"CODEX_HOME=" + filepath.Join(s.home, ".codex"),
		"AO_DATA_DIR=" + s.dataDir,
		"AO_RUN_FILE=" + s.runFile,
		"AO_PORT=" + strconv.Itoa(s.port),
		"AO_TMUX_SOCKET=" + s.socket,
		"AO_TELEMETRY_REMOTE=off",
		// Satisfies the Claude credential probe from the environment, so it
		// never reaches for a keychain. It is not a key and no model is called.
		"ANTHROPIC_API_KEY=ao-e2e-not-a-key",
		"AO_E2E_TRACE_DIR=" + s.traceDir,
		// Trusted-local mode resolves every request to the bootstrap admin, which
		// a fresh installation must be told to create.
		"AO_BOOTSTRAP_ADMIN_EMAIL=admin@ao-e2e.invalid",
		"AO_BOOTSTRAP_ADMIN_PASSWORD=ao-e2e-scratch-password-1",
		// Deliberately NO locale variables: that is the environment a daemon
		// started by launchd or by the desktop app actually gets, and it is the
		// one that exposed the tmux inventory defect fixed alongside this test.
	}
}

func (s *scratch) cleanup() {
	if s.daemon != nil && s.daemon.ProcessState == nil {
		_ = s.daemon.Process.Kill() // our own child only
		select {
		case <-s.waitErr:
		case <-time.After(10 * time.Second):
		}
	}
	if s.daemonLog != nil {
		_ = s.daemonLog.Close()
		if s.t.Failed() {
			if b, err := os.ReadFile(s.daemonLog.Name()); err == nil {
				tail := string(b)
				if len(tail) > 12000 {
					tail = tail[len(tail)-12000:]
				}
				s.t.Logf("daemon log tail:\n%s", tail)
			}
		}
	}
	_ = exec.Command("tmux", "-L", s.socket, "kill-server").Run()
	// The server is gone; remove its socket file too. Derived rather than
	// asked for, because a server that already exited (the usual case once GC
	// reclaimed the last session) can no longer report its own path.
	tmuxDir := os.Getenv("TMUX_TMPDIR")
	if tmuxDir == "" {
		tmuxDir = "/tmp"
	}
	if strings.HasPrefix(s.socket, "ao-cycle-e2e-") {
		_ = os.Remove(filepath.Join(tmuxDir, fmt.Sprintf("tmux-%d", os.Getuid()), s.socket))
	}
	if out, err := exec.Command("tmux", "-L", s.socket, "list-sessions").CombinedOutput(); err == nil {
		s.t.Errorf("tmux sessions survived cleanup on %s: %s", s.socket, out)
	}
}

func (s *scratch) startDaemon() {
	s.t.Helper()
	logf, err := os.OpenFile(filepath.Join(s.root, "daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		s.t.Fatal(err)
	}
	s.daemonLog = logf
	cmd := exec.Command(s.aoBin, "daemon")
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
			Status         string `json:"status"`
			PID            int    `json:"pid"`
			DataDir        string `json:"dataDir"`
			InstanceID     string `json:"instanceId"`
			InstallationID string `json:"installationId"`
		}
		if code, err := s.getJSON("/readyz", &body); err != nil || code != http.StatusOK {
			return false
		}
		if body.Status != "ready" || body.PID != cmd.Process.Pid || body.DataDir != s.dataDir {
			return false
		}
		s.instanceID, s.installationID = body.InstanceID, body.InstallationID
		return true
	})
	if s.instanceID == "" || s.installationID == "" {
		s.t.Fatalf("readyz carried no daemon identity (instance %q, installation %q)", s.instanceID, s.installationID)
	}
}

// ---- repository ----------------------------------------------------------------

// initRepo creates the project repository: a tracked source file, an ignored
// out/ directory, and a single commit on main.
func (s *scratch) initRepo() {
	s.t.Helper()
	writeFile(s.t, filepath.Join(s.repo, ".gitignore"), "out/\n")
	writeFile(s.t, filepath.Join(s.repo, "src", "main.go"), "package src\n")
	writeFile(s.t, filepath.Join(s.repo, "README.md"), "# cycle e2e\n")
	s.git(s.repo, "init", "-q", "-b", "main")
	s.git(s.repo, "add", "-A")
	s.git(s.repo, "commit", "-qm", "init")
}

func (s *scratch) git(dir string, args ...string) string {
	s.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(s.env(), "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// ---- HTTP ------------------------------------------------------------------------

func (s *scratch) do(method, path string, body any) (int, []byte, error) {
	var rd io.Reader = http.NoBody
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", s.port, path), rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

func (s *scratch) getJSON(path string, v any) (int, error) {
	code, b, err := s.do(http.MethodGet, path, nil)
	if err != nil {
		return code, err
	}
	if code != http.StatusOK {
		return code, fmt.Errorf("GET %s: %d %s", path, code, b)
	}
	return code, json.Unmarshal(b, v)
}

// mustDo performs a request that must succeed and returns the decoded body.
func (s *scratch) mustDo(method, path string, body any) map[string]any {
	s.t.Helper()
	code, b, err := s.do(method, path, body)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	if code >= 300 {
		s.t.Fatalf("%s %s: %d %s", method, path, code, b)
	}
	out := map[string]any{}
	if len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &out); err != nil {
			s.t.Fatalf("%s %s: decode: %v: %s", method, path, err, b)
		}
	}
	return out
}

// ---- tmux evidence ---------------------------------------------------------------

type tmuxSession struct {
	Name         string
	StartCommand string
	PanePID      string
	Env          map[string]string
}

// tmuxSessions lists every session on the scratch socket, with the AO stamps
// tmux holds in each session's environment. An absent server is zero sessions.
func (s *scratch) tmuxSessions() []tmuxSession {
	out, err := exec.Command("tmux", "-L", s.socket, "list-sessions", "-F", "#{session_name}\t#{pane_pid}\t#{pane_start_command}").Output()
	if err != nil {
		return nil
	}
	var sessions []tmuxSession
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		for len(parts) < 3 {
			parts = append(parts, "")
		}
		ts := tmuxSession{Name: parts[0], PanePID: parts[1], StartCommand: parts[2], Env: map[string]string{}}
		for _, key := range []string{"AO_SESSION_OWNER", "AO_INSTALLATION_ID", "AO_DAEMON_INSTANCE_ID"} {
			if v, err := exec.Command("tmux", "-L", s.socket, "show-environment", "-t", ts.Name, key).Output(); err == nil {
				if _, val, ok := strings.Cut(strings.TrimSpace(string(v)), "="); ok {
					ts.Env[key] = val
				}
			}
		}
		sessions = append(sessions, ts)
	}
	return sessions
}

// tmuxWatcher samples the socket until stopped, so a session that lived only
// briefly (a reviewer) is still observed.
type tmuxWatcher struct {
	mu   sync.Mutex
	seen map[string]tmuxSession
	stop chan struct{}
	done chan struct{}
}

func (s *scratch) watchTmux() *tmuxWatcher {
	w := &tmuxWatcher{seen: map[string]tmuxSession{}, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		tick := time.NewTicker(150 * time.Millisecond)
		defer tick.Stop()
		for {
			for _, ts := range s.tmuxSessions() {
				w.mu.Lock()
				if prev, ok := w.seen[ts.Name]; !ok || len(prev.Env) < len(ts.Env) {
					w.seen[ts.Name] = ts
				}
				w.mu.Unlock()
			}
			select {
			case <-w.stop:
				return
			case <-tick.C:
			}
		}
	}()
	return w
}

func (w *tmuxWatcher) Stop() map[string]tmuxSession {
	close(w.stop)
	<-w.done
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]tmuxSession, len(w.seen))
	for k, v := range w.seen {
		out[k] = v
	}
	return out
}

// ---- agent traces ----------------------------------------------------------------

type agentTrace map[string]string

// traces reads every session trace the shim wrote for a role.
func (s *scratch) traces(role string) []agentTrace {
	s.t.Helper()
	matches, _ := filepath.Glob(filepath.Join(s.traceDir, role+"-*.trace"))
	sort.Strings(matches)
	var out []agentTrace
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			s.t.Fatal(err)
		}
		tr := agentTrace{"file": m}
		for _, line := range strings.Split(string(b), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				tr[k] = v
			}
		}
		out = append(out, tr)
	}
	return out
}

// ---- helpers ---------------------------------------------------------------------

var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
)

// buildAO compiles the binary under test once per test process: the daemon's
// own boot path and wiring are part of what is under test.
func buildAO(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		out, err := exec.Command("go", "env", "GOMOD").Output()
		if err != nil {
			buildErr = err
			return
		}
		moduleDir := filepath.Dir(strings.TrimSpace(string(out)))
		dir, err := os.MkdirTemp("", "ao-cycle-e2e-bin-")
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

// TestMain removes the binary built for the E2E, which outlives any one test.
func TestMain(m *testing.M) {
	code := m.Run()
	if builtBin != "" {
		_ = os.RemoveAll(filepath.Dir(builtBin))
	}
	os.Exit(code)
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

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// dig walks a decoded JSON document by keys and array indexes.
func dig(v any, path ...any) any {
	for _, p := range path {
		switch key := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[key]
		case int:
			a, ok := v.([]any)
			if !ok || key >= len(a) {
				return nil
			}
			v = a[key]
		}
	}
	return v
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
