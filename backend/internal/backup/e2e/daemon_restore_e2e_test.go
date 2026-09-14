// Package backupe2e is P10's backup/restore E2E against the REAL `ao` binary and
// the REAL daemon (§53, §54, §79).
//
// Everything runs in a SCRATCH installation: temporary HOME, data dir, database,
// run-file, backup root, free port and a private tmux socket. Nothing touches
// ~/.ao, port 3002, the user's tmux servers or any agent; AO_FAKE_HARNESS=1
// guarantees that even an unexpected launch could only start the LLM-free fake.
// The only processes this test ever kills are its own children.
//
// Opt-in (it builds a binary and runs a daemon): AO_P10_E2E=1.
package backupe2e

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/aoagents/agent-orchestrator/backend/internal/daemonmeta"
)

var socketSeq atomic.Int64

type scratch struct {
	t       *testing.T
	root    string
	home    string
	dataDir string
	runFile string
	backups string
	port    int
	socket  string
	bin     string
	daemon  *exec.Cmd
	waitErr chan error
	logPath string
}

func newScratch(t *testing.T) *scratch {
	t.Helper()
	if os.Getenv("AO_P10_E2E") != "1" {
		t.Skip("set AO_P10_E2E=1 to run the P10 daemon backup/restore E2E")
	}
	root := t.TempDir()
	s := &scratch{
		t: t, root: root,
		home:    filepath.Join(root, "home"),
		dataDir: filepath.Join(root, "data"),
		runFile: filepath.Join(root, "running.json"),
		backups: filepath.Join(root, "backups"),
		socket:  fmt.Sprintf("ao-p10-e2e-%d-%d", os.Getpid(), socketSeq.Add(1)),
		port:    freePort(t),
		logPath: filepath.Join(root, "daemon.log"),
	}
	if s.port == 3002 {
		t.Fatal("refusing to use port 3002")
	}
	for _, d := range []string{s.home, s.dataDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	s.bin = buildAO(t)
	t.Cleanup(s.cleanup)
	return s
}

func (s *scratch) cleanup() {
	if s.daemon != nil {
		select {
		case <-s.waitErr:
		default:
			_ = s.daemon.Process.Kill() // our own child only
			<-s.waitErr
		}
	}
	if s.t.Failed() {
		if b, err := os.ReadFile(s.logPath); err == nil {
			tail := string(b)
			if len(tail) > 6000 {
				tail = tail[len(tail)-6000:]
			}
			s.t.Logf("daemon log tail:\n%s", tail)
		}
	}
	_ = exec.Command("tmux", "-L", s.socket, "kill-server").Run()
}

func (s *scratch) env() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + s.home,
		"TMPDIR=" + s.root,
		"CODEX_HOME=" + filepath.Join(s.home, ".codex"),
		"AO_DATA_DIR=" + s.dataDir,
		"AO_RUN_FILE=" + s.runFile,
		"AO_BACKUP_DIR=" + s.backups,
		"AO_PORT=" + strconv.Itoa(s.port),
		"AO_TMUX_SOCKET=" + s.socket,
		"AO_FAKE_HARNESS=1",
		"AO_TELEMETRY_REMOTE=off",
	}
}

var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if builtBin != "" {
		_ = os.RemoveAll(filepath.Dir(builtBin))
	}
	os.Exit(code)
}

func buildAO(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		out, err := exec.Command("go", "env", "GOMOD").Output()
		if err != nil {
			buildErr = err
			return
		}
		moduleDir := filepath.Dir(strings.TrimSpace(string(out)))
		dir, err := os.MkdirTemp("", "ao-p10-e2e-bin-")
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

func (s *scratch) startDaemon() {
	s.t.Helper()
	logf, err := os.OpenFile(s.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		s.t.Fatal(err)
	}
	cmd := exec.Command(s.bin, "daemon")
	cmd.Env = s.env()
	cmd.Dir = s.root
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		s.t.Fatalf("start daemon: %v", err)
	}
	s.daemon = cmd
	s.waitErr = make(chan error, 1)
	go func() {
		s.waitErr <- cmd.Wait()
		_ = logf.Close()
	}()
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if pid, ok := s.readyPID(); ok && pid == cmd.Process.Pid {
			return
		}
		select {
		case err := <-s.waitErr:
			s.waitErr <- err
			s.t.Fatalf("daemon exited before ready: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
	}
	s.t.Fatal("daemon never became ready")
}

func (s *scratch) readyPID() (int, bool) {
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(fmt.Sprintf("http://127.0.0.1:%d/readyz", s.port))
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	var body struct {
		Status  string `json:"status"`
		PID     int    `json:"pid"`
		DataDir string `json:"dataDir"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil || body.Status != "ready" {
		return 0, false
	}
	return body.PID, true
}

func (s *scratch) stopDaemon() {
	s.t.Helper()
	if out, code := s.ao("stop"); code != 0 {
		s.t.Fatalf("ao stop (exit %d): %s", code, out)
	}
	select {
	case <-s.waitErr:
		s.waitErr <- nil
	case <-time.After(60 * time.Second):
		s.t.Fatal("the daemon did not exit after ao stop")
	}
}

// ao runs the real binary and returns combined output and the exit code.
func (s *scratch) ao(args ...string) (string, int) {
	s.t.Helper()
	cmd := exec.Command(s.bin, args...)
	cmd.Env = s.env()
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exit):
		return string(out), exit.ExitCode()
	default:
		s.t.Fatalf("run ao %v: %v", args, err)
		return "", -1
	}
}

func (s *scratch) jsonOut(out string, v any) {
	s.t.Helper()
	// Progress goes to stderr, which CombinedOutput interleaves: take the JSON object.
	start := strings.Index(out, "{")
	end := strings.LastIndex(out, "}")
	if start < 0 || end < start {
		s.t.Fatalf("no JSON in output: %s", out)
	}
	if err := json.Unmarshal([]byte(out[start:end+1]), v); err != nil {
		s.t.Fatalf("parse JSON: %v\n%s", err, out)
	}
}

func (s *scratch) projectIDs() []string {
	s.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(s.dataDir, "ao.db")+"?mode=ro&immutable=1")
	if err != nil {
		s.t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id FROM projects ORDER BY id`)
	if err != nil {
		s.t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			s.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		s.t.Fatal(err)
	}
	return ids
}

func (s *scratch) insertProject(id string) {
	s.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(s.dataDir, "ao.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		s.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO projects (id, path, registered_at) VALUES (?, ?, CURRENT_TIMESTAMP)`, id, "/tmp/"+id); err != nil {
		s.t.Fatal(err)
	}
}

type createOut struct {
	Path     string `json:"path"`
	BackupID string `json:"backupId"`
}

type restoreOut struct {
	Result             string `json:"result"`
	RollbackBackupPath string `json:"rollbackBackupPath"`
	Reason             *struct {
		Code string `json:"code"`
	} `json:"reason"`
}

// §6/§53 — backup is online; restore refuses a live daemon and never stops
// it; after `ao stop` the same restore passes and the restored data dir boots.
func TestP10_OnlineBackupRestoreRefusedWhileRunningThenRestoredAfterStop(t *testing.T) {
	s := newScratch(t)
	s.startDaemon()
	daemonPID := s.daemon.Process.Pid

	out, code := s.ao("backup", "create", "--json", "--note", "p10-e2e")
	if code != 0 {
		t.Fatalf("online backup (exit %d): %s", code, out)
	}
	var created createOut
	s.jsonOut(out, &created)
	if out, code := s.ao("backup", "verify", created.Path); code != 0 || !strings.Contains(out, "VALID") {
		t.Fatalf("verify online backup (exit %d): %s", code, out)
	}

	out, code = s.ao("restore", created.Path, "--yes", "--json")
	if code != 3 || !strings.Contains(out, "daemon_active") {
		t.Fatalf("restore over a live daemon: exit %d: %s", code, out)
	}
	if pid, ok := s.readyPID(); !ok || pid != daemonPID {
		t.Fatalf("the refused restore disturbed the daemon (ready=%t pid=%d)", ok, pid)
	}
	if _, err := os.Lstat(filepath.Join(s.dataDir, ".ao-restore-journal.json")); err == nil {
		t.Fatal("a refused restore left a journal")
	}

	s.stopDaemon()
	s.insertProject("created-after-backup")
	if !slices.Contains(s.projectIDs(), "created-after-backup") {
		t.Fatal("precondition: post-backup row not written")
	}

	out, code = s.ao("restore", created.Path, "--yes", "--json")
	if code != 0 {
		t.Fatalf("restore after ao stop (exit %d): %s", code, out)
	}
	var restored restoreOut
	s.jsonOut(out, &restored)
	if restored.Result != "RESTORED" || restored.RollbackBackupPath == "" {
		t.Fatalf("restore report: %+v", restored)
	}
	if slices.Contains(s.projectIDs(), "created-after-backup") {
		t.Fatal("state created after the backup survived the restore")
	}
	for _, side := range []string{"ao.db-wal", "ao.db-shm"} {
		if _, err := os.Lstat(filepath.Join(s.dataDir, side)); err == nil {
			t.Fatalf("%s present after restore", side)
		}
	}

	// The restored data dir is a data dir AO boots on.
	s.startDaemon()
	if out, code := s.ao("status"); code != 0 || !strings.Contains(out, "ready") {
		t.Fatalf("status after restore (exit %d): %s", code, out)
	}
	s.stopDaemon()
	if out, code := s.ao("backup", "list"); code != 0 || !strings.Contains(out, "Last restore: RESTORED") {
		t.Fatalf("list (exit %d): %s", code, out)
	}
}

// §54 — a live PID recorded in the run-file whose probe answers as another
// process: restore refuses, the process gets no signal, the file stays.
func TestP10_RestoreRefusesAnUnverifiedLivePID(t *testing.T) {
	s := newScratch(t)
	s.startDaemon()
	out, code := s.ao("backup", "create", "--json")
	if code != 0 {
		t.Fatalf("backup (exit %d): %s", code, out)
	}
	var created createOut
	s.jsonOut(out, &created)
	install := readInstallation(t, s.dataDir)
	s.stopDaemon()

	bystander := exec.Command("sleep", "300")
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- bystander.Wait() }()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			_ = bystander.Process.Kill() // our own child only
			<-done
		}
	})
	impostor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "service": daemonmeta.ServiceName, "pid": 1})
	}))
	defer impostor.Close()
	port, _ := strconv.Atoi(impostor.URL[strings.LastIndex(impostor.URL, ":")+1:])
	forged, _ := json.Marshal(map[string]any{
		"pid": bystander.Process.Pid, "port": port, "startedAt": time.Now().UTC(), "formatVersion": 2,
		"instanceId": "aod-forged", "installationId": install, "dataDir": s.dataDir,
	})
	if err := os.WriteFile(s.runFile, forged, 0o600); err != nil {
		t.Fatal(err)
	}

	out, code = s.ao("restore", created.Path, "--yes")
	if code != 3 || !strings.Contains(out, "daemon_unverified") {
		t.Fatalf("restore over an unverified PID: exit %d: %s", code, out)
	}
	select {
	case err := <-done:
		t.Fatalf("the unverified process was signalled: %v", err)
	default:
	}
	if err := bystander.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("the unverified process is gone: %v", err)
	}
	if b, err := os.ReadFile(s.runFile); err != nil || string(b) != string(forged) {
		t.Fatal("the run-file of an unverified PID was changed or removed")
	}

	// Once the operator has dealt with it (our own child, our own test file):
	_ = bystander.Process.Kill()
	<-done
	done <- nil
	if err := os.Remove(s.runFile); err != nil {
		t.Fatal(err)
	}
	if out, code := s.ao("restore", created.Path, "--yes"); code != 0 {
		t.Fatalf("restore after the process is gone (exit %d): %s", code, out)
	}
}

// §79 — a daemon refuses to boot over a restore interrupted mid-swap, and boots
// once `ao backup recover` has resolved it.
func TestP10_DaemonRefusesToBootOverAnInterruptedRestore(t *testing.T) {
	s := newScratch(t)
	s.startDaemon()
	s.stopDaemon()

	// The on-disk state of a restore killed right after it recorded "swapping":
	// journal + staged copies, nothing moved yet.
	restoreID := "aor-" + time.Now().UTC().Format("20060102T150405.000000000Z") + "-0badc0de"
	work := filepath.Join(s.dataDir, ".ao-restore-"+restoreID)
	for _, p := range []string{"staged/ao.db", "staged/skills/catalog/.keep"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(work, p)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, p), []byte("staged"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var pre []string
	for _, e := range []string{"ao.db-wal", "ao.db-shm", "ao.db-journal", "ao.db", "installation_id", "skills/catalog"} {
		if _, err := os.Lstat(filepath.Join(s.dataDir, e)); err == nil {
			pre = append(pre, e)
		}
	}
	dbInfo, err := os.Stat(filepath.Join(s.dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	journal, _ := json.Marshal(map[string]any{
		"format": "ao.restore-journal/v1", "restoreId": restoreID, "sourceBackupId": "aob-20260914T000000.000000000Z-00000000",
		"sourcePath": "/nowhere", "workDir": ".ao-restore-" + restoreID, "phase": "swapping",
		"promote": []string{"ao.db", "skills/catalog"}, "preExisting": pre,
		"preDatabase": map[string]any{"size": dbInfo.Size(), "modTimeUnixNano": dbInfo.ModTime().UnixNano()},
		"updatedAt":   time.Now().UTC(),
	})
	if err := os.WriteFile(filepath.Join(s.dataDir, ".ao-restore-journal.json"), journal, 0o600); err != nil {
		t.Fatal(err)
	}

	boot := exec.Command(s.bin, "daemon")
	boot.Env = s.env()
	boot.Dir = s.root
	bootOut, err := boot.CombinedOutput()
	if err == nil || !strings.Contains(string(bootOut), "ao backup recover") {
		t.Fatalf("daemon booted over an interrupted restore (err=%v): %s", err, bootOut)
	}
	if _, ok := s.readyPID(); ok {
		t.Fatal("something is serving the port")
	}

	out, code := s.ao("backup", "recover", "--json")
	if code != 0 || !strings.Contains(out, "RECOVERED_ROLLED_BACK") {
		t.Fatalf("recover (exit %d): %s", code, out)
	}
	s.startDaemon()
	s.stopDaemon()
}

func readInstallation(t *testing.T, dataDir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dataDir, daemonmeta.InstallationIDFile))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}
