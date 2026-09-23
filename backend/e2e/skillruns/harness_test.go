//go:build !windows

// Package skillruns is Frente 2 / 2B's end-to-end test of a durable skill run
// through a REAL `ao daemon`, a REAL SQLite database and a REAL container
// runtime: Project -> security-audit -> activation -> image approval -> Run ->
// Docker -> findings -> persistence -> daemon restart -> history.
//
// Scratch everything: HOME, TMPDIR, data dir (and so the database), run-file,
// port, tmux socket and the project repository live under a private temp root.
// The one exception is the staging root, which must be a path the container
// runtime's VM shares with the host (on colima/Docker Desktop an unshared path
// mounts EMPTY). It is supplied explicitly:
//
//	AO_SKILL_RUN_E2E=1 \
//	AO_SKILL_RUN_E2E_SHARED_ROOT=/a/path/the/runtime/shares \
//	go test ./e2e/skillruns/ -v -count=1 -timeout 20m
//
// ~/.ao is never read or written. The daemon reaches Docker through an explicit
// DOCKER_HOST, because its HOME is scratch and holds no docker context.
package skillruns

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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/testsupport/dockerlock"
)

const (
	gateEnv       = "AO_SKILL_RUN_E2E"
	sharedRootEnv = "AO_SKILL_RUN_E2E_SHARED_ROOT"
	// secretSentinel is planted in the project as a literal password. The scan
	// must FIND it and nothing AO stores may CONTAIN it.
	secretSentinel = "E2E-SECRET-VALUE-9f3a1c7d"
)

var socketSeq atomic.Int64

type scratch struct {
	t          *testing.T
	root       string
	home       string
	dataDir    string
	runFile    string
	binDir     string
	repo       string
	shared     string
	port       int
	socket     string
	aoBin      string
	dockerHost string
	env        []string

	daemon    *exec.Cmd
	daemonLog *os.File
	waitErr   chan error
}

func newScratch(t *testing.T) *scratch {
	t.Helper()
	if os.Getenv(gateEnv) != "1" {
		t.Skipf("set %s=1 to run the skill-run E2E (real daemon, real Docker)", gateEnv)
	}
	sharedBase := os.Getenv(sharedRootEnv)
	if sharedBase == "" || !filepath.IsAbs(sharedBase) {
		t.Fatalf("%s must name an absolute directory the container runtime shares with this host", sharedRootEnv)
	}
	for _, bin := range []string{"docker", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatalf("%s is required: %v", bin, err)
		}
	}
	host := dockerHost(t)
	if err := exec.Command("docker", "image", "inspect", "alpine:3.19").Run(); err != nil {
		t.Fatalf("alpine:3.19 must be present locally (this test never pulls): %v", err)
	}
	// Host-wide: this test starts containers on the same runtime other
	// packages' live tests use, and asserts on host-wide container state.
	dockerlock.Acquire(t)

	raw, err := os.MkdirTemp("/tmp", "aoskr-")
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sharedBase, 0o750); err != nil {
		t.Fatal(err)
	}
	shared, err := os.MkdirTemp(sharedBase, "run-")
	if err != nil {
		t.Fatal(err)
	}
	s := &scratch{
		t:          t,
		root:       root,
		home:       filepath.Join(root, "home"),
		dataDir:    filepath.Join(root, "data"),
		runFile:    filepath.Join(root, "running.json"),
		binDir:     filepath.Join(root, "bin"),
		repo:       filepath.Join(root, "repo"),
		shared:     shared,
		socket:     fmt.Sprintf("aoskr-%d-%d", os.Getpid(), socketSeq.Add(1)),
		port:       freePort(t),
		dockerHost: host,
	}
	home, _ := os.UserHomeDir()
	if strings.HasPrefix(s.dataDir, filepath.Join(home, ".ao")) || strings.HasPrefix(s.shared, filepath.Join(home, ".ao")) {
		t.Fatal("a scratch path resolved under ~/.ao")
	}
	for _, d := range []string{s.home, s.dataDir, s.binDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(s.home, ".gitconfig"), "[user]\n\tname = ao-e2e\n\temail = ao-e2e@example.invalid\n")
	for _, tool := range []string{"docker", "git"} {
		p, _ := exec.LookPath(tool)
		if err := os.Symlink(p, filepath.Join(s.binDir, tool)); err != nil {
			t.Fatal(err)
		}
	}
	s.env = s.baseEnv()
	s.aoBin = buildAO(t)
	t.Cleanup(s.cleanup)
	return s
}

func dockerHost(t *testing.T) string {
	t.Helper()
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return h
	}
	out, err := exec.Command("docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
	if err != nil {
		t.Fatalf("resolve the docker endpoint: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func (s *scratch) baseEnv() []string {
	return []string{
		"PATH=" + s.binDir + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + s.home,
		"TMPDIR=" + s.root,
		"DOCKER_HOST=" + s.dockerHost,
		"AO_DATA_DIR=" + s.dataDir,
		"AO_RUN_FILE=" + s.runFile,
		"AO_PORT=" + strconv.Itoa(s.port),
		"AO_TMUX_SOCKET=" + s.socket,
		"AO_TELEMETRY_REMOTE=off",
		"AO_SKILL_STAGING_ROOT=" + s.shared,
		"AO_BOOTSTRAP_ADMIN_EMAIL=admin@ao-e2e.invalid",
		"AO_BOOTSTRAP_ADMIN_PASSWORD=ao-e2e-scratch-password-1",
	}
}

func (s *scratch) cleanup() {
	s.stopDaemon(syscall.SIGKILL)
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
	_ = os.Remove(filepath.Join("/tmp", fmt.Sprintf("tmux-%d", os.Getuid()), s.socket))
	if os.Getenv("AO_E2E_KEEP_DIR") == "1" {
		s.t.Logf("scratch kept at %s and %s", s.root, s.shared)
		return
	}
	_ = os.RemoveAll(s.root)
	_ = os.RemoveAll(s.shared)
}

func (s *scratch) startDaemon(extraEnv ...string) {
	s.t.Helper()
	logf, err := os.OpenFile(filepath.Join(s.root, "daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		s.t.Fatal(err)
	}
	s.daemonLog = logf
	cmd := exec.Command(s.aoBin, "daemon")
	cmd.Env = append(append([]string{}, s.env...), extraEnv...)
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
		code, b, err := s.do(http.MethodGet, "/readyz", nil)
		if err != nil || code != http.StatusOK || json.Unmarshal(b, &body) != nil {
			return false
		}
		return body.Status == "ready" && body.PID == cmd.Process.Pid && body.DataDir == s.dataDir
	})
}

// stopDaemon stops THIS test's daemon child with sig and waits for it.
func (s *scratch) stopDaemon(sig syscall.Signal) {
	if s.daemon == nil || s.daemon.ProcessState != nil {
		return
	}
	_ = s.daemon.Process.Signal(sig)
	select {
	case <-s.waitErr:
	case <-time.After(30 * time.Second):
		_ = s.daemon.Process.Kill()
		<-s.waitErr
	}
	s.daemon = nil
}

func (s *scratch) daemonLogText() string {
	b, _ := os.ReadFile(filepath.Join(s.root, "daemon.log"))
	return string(b)
}

// initRepo writes a project the static scan has real findings in, including a
// literal password whose VALUE must never reach AO's storage.
func (s *scratch) initRepo(dir string) {
	s.t.Helper()
	writeFile(s.t, filepath.Join(dir, "src", "app.go"), "package src\n\n"+
		"import (\n\t\"crypto/md5\"\n\t\"crypto/tls\"\n)\n\n"+
		"var digest = md5.New()\n\n"+
		"var tlsConfig = &tls.Config{InsecureSkipVerify: true}\n\n"+
		"var password = \""+secretSentinel+"\"\n")
	writeFile(s.t, filepath.Join(dir, "README.md"), "# skill run e2e\n")
	s.git(dir, "init", "-q", "-b", "main")
	s.git(dir, "add", "-A")
	s.git(dir, "commit", "-qm", "init")
}

func (s *scratch) git(dir string, args ...string) {
	s.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(s.baseEnv(), "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		s.t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

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
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

// expect performs a request, requires the status, and decodes into out.
func (s *scratch) expect(method, path string, body any, want int, out any) []byte {
	s.t.Helper()
	code, b, err := s.do(method, path, body)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	if code != want {
		s.t.Fatalf("%s %s: got %d want %d: %s", method, path, code, want, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			s.t.Fatalf("%s %s: decode: %v: %s", method, path, err, b)
		}
	}
	return b
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
		dir, err := os.MkdirTemp("", "aoskr-bin-")
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
	for !cond() {
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

// skillRunContainers lists every container labelled as an AO skill run.
func skillRunContainers(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("docker", "ps", "-aq", "--filter", "label=ao.skillrun=1").Output()
	if err != nil {
		t.Fatalf("docker ps: %v", err)
	}
	return strings.Fields(string(out))
}

func jsonDecode(b []byte, v any) error { return json.Unmarshal(b, v) }
