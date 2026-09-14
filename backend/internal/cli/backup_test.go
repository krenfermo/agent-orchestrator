package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/backup"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/daemonlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/daemonmeta"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// P10 CLI: every test runs against a scratch data dir from setConfigEnv; none
// touches ~/.ao.

func newBackupCLIEnv(t *testing.T) testConfig {
	t.Helper()
	cfg := setConfigEnv(t)
	store, err := sqlitetest.Open(cfg.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := daemonmeta.LoadOrCreateInstallationID(cfg.dataDir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AO_BACKUP_DIR", filepath.Join(t.TempDir(), "backups"))
	return cfg
}

func cliCreateBackup(t *testing.T) backup.CreateResult {
	t.Helper()
	out, _, err := executeCLI(t, Deps{}, "backup", "create", "--json", "--note", "cli test")
	if err != nil {
		t.Fatalf("ao backup create: %v", err)
	}
	var res backup.CreateResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("parse create output: %v\n%s", err, out)
	}
	return res
}

func fileSHA(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func setManifestFormat(t *testing.T, dir, format string) {
	t.Helper()
	p := filepath.Join(dir, backup.ManifestName)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	raw["format"] = format
	out, _ := json.Marshal(raw)
	if err := os.WriteFile(p, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBackupCLI_CreateVerifyListPrune(t *testing.T) {
	newBackupCLIEnv(t)
	res := cliCreateBackup(t)
	if !backup.ValidBackupID(res.BackupID) || filepath.Base(filepath.Dir(res.Path)) != "backups" {
		t.Fatalf("create result: %+v", res)
	}

	out, _, err := executeCLI(t, Deps{}, "backup", "verify", res.Path, "--json")
	if ExitCode(err) != 0 {
		t.Fatalf("verify exit %d: %v", ExitCode(err), err)
	}
	var rep backup.VerifyReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil || rep.Status != backup.StatusValid {
		t.Fatalf("verify report %+v (%v)", rep, err)
	}
	out, _, err = executeCLI(t, Deps{}, "backup", "verify", res.Path)
	if err != nil || !strings.Contains(out, "VALID") || !strings.Contains(out, "integrity, not who produced") {
		t.Fatalf("verify text: %v\n%s", err, out)
	}

	out, _, err = executeCLI(t, Deps{}, "backup", "list")
	if err != nil || !strings.Contains(out, res.BackupID) || !strings.Contains(out, "Last backup:") {
		t.Fatalf("list: %v\n%s", err, out)
	}

	out, _, err = executeCLI(t, Deps{}, "backup", "prune")
	if err != nil || !strings.Contains(out, "Dry run") {
		t.Fatalf("prune: %v\n%s", err, out)
	}
	for _, args := range [][]string{
		{"backup", "prune", "--keep", "-1"},
		{"backup", "prune", "--max-age", "soon"},
		{"backup", "verify"},
		{"restore"},
		{"backup", "restore", res.Path, "--identity", "mine", "--yes"},
	} {
		if _, _, err := executeCLI(t, Deps{}, args...); ExitCode(err) != 2 {
			t.Errorf("%v: exit %d (%v), want 2", args, ExitCode(err), err)
		}
	}
}

func TestBackupCLI_VerifyExitCodes(t *testing.T) {
	newBackupCLIEnv(t)
	unsupported := cliCreateBackup(t)
	setManifestFormat(t, unsupported.Path, "ao.backup/v9")
	if _, _, err := executeCLI(t, Deps{}, "backup", "verify", unsupported.Path); ExitCode(err) != exitBackupIncompatible {
		t.Fatalf("unsupported manifest: exit %d (%v)", ExitCode(err), err)
	}

	corrupt := cliCreateBackup(t)
	db := filepath.Join(corrupt.Path, backup.DatabaseAsset)
	b, _ := os.ReadFile(db)
	b[len(b)/2] ^= 0xFF
	if err := os.WriteFile(db, b, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := executeCLI(t, Deps{}, "backup", "verify", corrupt.Path)
	if ExitCode(err) != exitBackupInvalid || !strings.Contains(err.Error(), string(backup.CodeHashMismatch)) {
		t.Fatalf("corrupt database: exit %d (%v)", ExitCode(err), err)
	}
	// And restore from it refuses with the same class.
	_, _, err = executeCLI(t, Deps{}, "restore", corrupt.Path, "--yes")
	if ExitCode(err) != exitBackupInvalid {
		t.Fatalf("restore from corrupt backup: exit %d (%v)", ExitCode(err), err)
	}
}

// restoreRefusal runs `ao restore` in a scenario that must refuse, and proves
// nothing was done: exit 3 with the code, no shutdown, the run-file kept, the
// database unchanged, no journal.
func restoreRefusal(t *testing.T, cfg testConfig, deps Deps, source string, code backup.Code) {
	t.Helper()
	dbBefore := fileSHA(t, filepath.Join(cfg.dataDir, backup.DatabaseAsset))
	_, _, err := executeCLI(t, deps, "restore", source, "--yes")
	if ExitCode(err) != exitBackupRefused || !strings.Contains(err.Error(), string(code)) {
		t.Fatalf("exit %d (%v), want %d with %s", ExitCode(err), err, exitBackupRefused, code)
	}
	if got := fileSHA(t, filepath.Join(cfg.dataDir, backup.DatabaseAsset)); got != dbBefore {
		t.Fatal("a refused restore changed the database")
	}
	if _, err := os.Lstat(filepath.Join(cfg.dataDir, ".ao-restore-journal.json")); err == nil {
		t.Fatal("a refused restore left a journal")
	}
}

// §53 at the CLI boundary, with P9's discovery: a verified live daemon refuses
// the restore and receives nothing.
func TestRestoreCLI_RefusesALiveDaemonAndNeverSignalsIt(t *testing.T) {
	for _, convention := range []string{"AO_RUN_FILE", "data dir"} {
		t.Run(convention, func(t *testing.T) {
			cfg := newBackupCLIEnv(t)
			res := cliCreateBackup(t)
			d := newFakeDaemon(t, 4242, "aod-live", cfg.dataDir)
			path := cfg.runFile
			if convention == "data dir" {
				path = filepath.Join(cfg.dataDir, "running.json")
			}
			info := writeDiscoveryRunFile(t, path, runfile.Info{PID: 4242, Port: d.port(t), InstanceID: "aod-live",
				DataDir: cfg.dataDir, FormatVersion: runfile.CurrentFormatVersion})
			deps := Deps{ProcessAlive: func(pid int) bool { return pid == 4242 }, HTTPClient: &http.Client{Timeout: time.Second}}

			restoreRefusal(t, cfg, deps, res.Path, backup.CodeDaemonActive)
			if _, shutdowns := d.counts(); shutdowns != 0 {
				t.Fatal("the restore asked the daemon to shut down")
			}
			kept, err := runfile.Read(path)
			if err != nil || kept == nil || !kept.SameDaemon(info) {
				t.Fatalf("the live daemon's run-file was changed: %+v %v", kept, err)
			}
		})
	}
}

// §54 — a live PID the probe cannot verify (reused PID / identity mismatch) or
// cannot reach refuses; nothing is signalled or removed.
func TestRestoreCLI_RefusesAnUnverifiedOrUnhealthyProcess(t *testing.T) {
	t.Run("unverified", func(t *testing.T) {
		cfg := newBackupCLIEnv(t)
		res := cliCreateBackup(t)
		other := newFakeDaemon(t, 9999, "aod-other", cfg.dataDir) // answers as a different process
		writeDiscoveryRunFile(t, cfg.runFile, runfile.Info{PID: 4242, Port: other.port(t), InstanceID: "aod-recorded",
			DataDir: cfg.dataDir, FormatVersion: runfile.CurrentFormatVersion})
		deps := Deps{ProcessAlive: func(pid int) bool { return pid == 4242 }, HTTPClient: &http.Client{Timeout: time.Second}}
		restoreRefusal(t, cfg, deps, res.Path, backup.CodeDaemonUnverified)
		if _, shutdowns := other.counts(); shutdowns != 0 {
			t.Fatal("the restore signalled an unverified daemon")
		}
		if _, err := os.Stat(cfg.runFile); err != nil {
			t.Fatal("the run-file of an unverified live PID was removed")
		}
	})
	t.Run("unhealthy", func(t *testing.T) {
		cfg := newBackupCLIEnv(t)
		res := cliCreateBackup(t)
		closed := httptest.NewServer(http.NotFoundHandler())
		port := serverPort(t, closed.URL)
		closed.Close()
		writeDiscoveryRunFile(t, cfg.runFile, runfile.Info{PID: 4242, Port: port, DataDir: cfg.dataDir, FormatVersion: runfile.CurrentFormatVersion})
		deps := Deps{ProcessAlive: func(pid int) bool { return pid == 4242 }, HTTPClient: &http.Client{Timeout: time.Second}}
		restoreRefusal(t, cfg, deps, res.Path, backup.CodeDaemonActive)
		if _, err := os.Stat(cfg.runFile); err != nil {
			t.Fatal("the run-file of a live PID was removed")
		}
	})
	t.Run("daemon.lock held with no run-file at all", func(t *testing.T) {
		cfg := newBackupCLIEnv(t)
		res := cliCreateBackup(t)
		lock, err := daemonlock.Acquire(filepath.Join(cfg.dataDir, "daemon.lock"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lock.Release() }()
		restoreRefusal(t, cfg, Deps{}, res.Path, backup.CodeDataDirLocked)
	})
}

// A stale run-file (dead PID) does not block a restore, and is left for the
// daemon's own start to overwrite.
func TestRestoreCLI_StaleRunFileAllowsRestoreAndIsKept(t *testing.T) {
	cfg := newBackupCLIEnv(t)
	res := cliCreateBackup(t)
	writeDiscoveryRunFile(t, cfg.runFile, runfile.Info{PID: 4242, Port: 1, DataDir: cfg.dataDir, FormatVersion: runfile.CurrentFormatVersion})
	deps := Deps{ProcessAlive: func(int) bool { return false }}
	out, _, err := executeCLI(t, deps, "restore", res.Path, "--yes", "--json")
	if err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	var rep backup.RestoreReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil || rep.Result != backup.ResultRestored || rep.RollbackBackupPath == "" {
		t.Fatalf("report %+v (%v)", rep, err)
	}
	if _, err := os.Stat(cfg.runFile); err != nil {
		t.Fatal("restore removed a run-file")
	}
	out, _, err = executeCLI(t, Deps{}, "backup", "list")
	if err != nil || !strings.Contains(out, "Last restore: RESTORED") {
		t.Fatalf("list after restore: %v\n%s", err, out)
	}
}

// Independent review §22 at the CLI boundary: a damaged database refuses with
// destination_damaged (exit 3), and --preserve-broken-state restores over it,
// reporting the forensic copy.
func TestRestoreCLI_DamagedDestinationNeedsPreserveBrokenState(t *testing.T) {
	cfg := newBackupCLIEnv(t)
	res := cliCreateBackup(t)
	if err := os.WriteFile(filepath.Join(cfg.dataDir, backup.DatabaseAsset), []byte(strings.Repeat("garbage!", 4096)), 0o600); err != nil {
		t.Fatal(err)
	}
	restoreRefusal(t, cfg, Deps{}, res.Path, backup.CodeDestinationDamaged)

	out, _, err := executeCLI(t, Deps{}, "restore", res.Path, "--yes", "--json", "--preserve-broken-state")
	if err != nil {
		t.Fatalf("restore --preserve-broken-state: %v\n%s", err, out)
	}
	var rep backup.RestoreReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil || rep.Result != backup.ResultRestored || rep.RollbackKind != backup.RollbackForensicCopy {
		t.Fatalf("report %+v (%v)", rep, err)
	}
	if _, err := os.Stat(filepath.Join(rep.RollbackBackupPath, backup.DatabaseAsset)); err != nil {
		t.Fatalf("no forensic copy of the damaged database: %v", err)
	}
}

func TestRestoreCLI_NeedsConfirmationOutsideATerminal(t *testing.T) {
	cfg := newBackupCLIEnv(t)
	res := cliCreateBackup(t)
	before := fileSHA(t, filepath.Join(cfg.dataDir, backup.DatabaseAsset))
	_, _, err := executeCLI(t, Deps{In: strings.NewReader("y\n")}, "restore", res.Path)
	if ExitCode(err) != 2 || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("exit %d (%v)", ExitCode(err), err)
	}
	if fileSHA(t, filepath.Join(cfg.dataDir, backup.DatabaseAsset)) != before {
		t.Fatal("an unconfirmed restore changed the database")
	}
}

func TestBackupCLI_RecoverWithNothingToRecover(t *testing.T) {
	newBackupCLIEnv(t)
	out, _, err := executeCLI(t, Deps{}, "backup", "recover")
	if err != nil || !strings.Contains(out, backup.RecoverNothing) {
		t.Fatalf("recover: %v\n%s", err, out)
	}
}

// §32 — the offline writers (import, usage backfill) hold daemon.lock too.
func TestHoldDataDirOfflineRefusesAHeldDataDir(t *testing.T) {
	dir := t.TempDir()
	newTestDB(t, dir)
	cfg := config.Config{DataDir: dir, RunFilePath: filepath.Join(dir, "running.json")}
	lock, err := daemonlock.Acquire(filepath.Join(dir, "daemon.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holdDataDirOffline(cfg, "importing"); ExitCode(err) != 2 || !strings.Contains(err.Error(), "holds data dir") {
		t.Fatalf("held lock: %v", err)
	}
	_ = lock.Release()
	release, err := holdDataDirOffline(cfg, "importing")
	if err != nil {
		t.Fatalf("free data dir: %v", err)
	}
	// While held by the offline command, no daemon (or second writer) can take it.
	if _, err := daemonlock.Acquire(filepath.Join(dir, "daemon.lock")); !errors.Is(err, daemonlock.ErrHeld) {
		t.Fatalf("daemon.lock not held during the offline command: %v", err)
	}
	release()
	l, err := daemonlock.Acquire(filepath.Join(dir, "daemon.lock"))
	if err != nil {
		t.Fatalf("daemon.lock not released: %v", err)
	}
	_ = l.Release()
}

func TestBackupExitCodes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{nil, 0},
		{&backup.Error{Code: backup.CodeDaemonActive, Class: backup.ClassRefused}, exitBackupRefused},
		{&backup.Error{Code: backup.CodeInvalidArgument, Class: backup.ClassRefused}, 2},
		{&backup.Error{Code: backup.CodeBackupInvalid, Class: backup.ClassInvalid}, exitBackupInvalid},
		{&backup.Error{Code: backup.CodeNewerThanBinary, Class: backup.ClassIncompatible}, exitBackupIncompatible},
		{&backup.Error{Code: backup.CodeRestoreVerifyFailed, Class: backup.ClassRolledBack}, exitRestoreRolledBack},
		{&backup.Error{Code: backup.CodeRollbackFailed, Class: backup.ClassRollbackFailed}, exitRestoreRollbackFails},
		{&backup.Error{Code: backup.CodeIO, Class: backup.ClassFailed}, 1},
		{errors.New("plain"), 1},
	} {
		if got := ExitCode(backupExit(tc.err)); got != tc.want {
			t.Errorf("%v: exit %d, want %d", tc.err, got, tc.want)
		}
	}
}
