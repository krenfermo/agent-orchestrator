package cli

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
)

// usage_backfill_test.go -- the guard, which is the whole safety of an offline
// command that rewrites the ledger.
//
// The run-file layer alone is NOT sufficient and these tests exist because the
// insufficiency is real: `ao server --data-dir X` writes its run file to
// X/running.json, so a daemon launched that way is invisible to a check that
// only consults AO_RUN_FILE or ~/.ao/running.json. A live daemon was observed
// on this machine in exactly that shape.

func writeRunFile(t *testing.T, path string, pid int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := `{"pid": ` + itoa(pid) + `, "port": 3999, "startedAt": "2026-09-12T00:00:00Z", "appRunId": "apprun-test"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write run file: %v", err)
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	digits := ""
	for v > 0 {
		digits = string(rune('0'+v%10)) + digits
		v /= 10
	}
	return digits
}

func newTestDB(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "ao.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (x INTEGER)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return path
}

func TestTheGuardSeesADaemonWhoseRunFileIsInTheDataDir(t *testing.T) {
	dir := t.TempDir()
	newTestDB(t, dir)
	// The daemon's own location when it was launched with --data-dir. The
	// configured run-file path points somewhere else entirely and is absent --
	// the exact shape observed in production.
	writeRunFile(t, filepath.Join(dir, "running.json"), os.Getpid())

	err := assertNoLiveDaemon(config.Config{
		DataDir:     dir,
		RunFilePath: filepath.Join(t.TempDir(), "elsewhere", "running.json"),
	})
	if err == nil {
		t.Fatal("a live daemon whose run file sits in the data dir must be seen")
	}
	if !strings.Contains(err.Error(), "daemon is running") {
		t.Fatalf("error = %v, want the daemon named", err)
	}
}

func TestTheGuardIgnoresAStaleRunFile(t *testing.T) {
	dir := t.TempDir()
	newTestDB(t, dir)
	writeRunFile(t, filepath.Join(dir, "running.json"), 999999) // a pid nobody has

	if err := assertNoLiveDaemon(config.Config{
		DataDir: dir, RunFilePath: filepath.Join(dir, "running.json"),
	}); err != nil {
		t.Fatalf("a stale run file must not block maintenance: %v", err)
	}
}

func TestTheGuardRefusesWhenAnotherConnectionHoldsTheDatabase(t *testing.T) {
	dir := t.TempDir()
	path := newTestDB(t, dir)
	// No run file anywhere -- the case a file-based check cannot catch at all.
	holder, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("holder: %v", err)
	}
	defer holder.Close()
	if _, err := holder.Exec(`INSERT INTO t (x) VALUES (1)`); err != nil {
		t.Fatalf("holder write: %v", err)
	}
	var n int
	if err := holder.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("holder read: %v", err)
	}

	err = assertNoLiveDaemon(config.Config{
		DataDir: dir, RunFilePath: filepath.Join(dir, "running.json"),
	})
	if err == nil {
		t.Fatal("a database another connection holds must not be opened for maintenance")
	}
	if !strings.Contains(err.Error(), "another process has AO's database open") {
		t.Fatalf("error = %v, want the database named", err)
	}

	// And once the holder lets go, maintenance is allowed again.
	if err := holder.Close(); err != nil {
		t.Fatalf("close holder: %v", err)
	}
	if err := assertNoLiveDaemon(config.Config{
		DataDir: dir, RunFilePath: filepath.Join(dir, "running.json"),
	}); err != nil {
		t.Fatalf("a quiet database must be allowed: %v", err)
	}
}

func TestTheGuardLeavesTheDatabaseUsable(t *testing.T) {
	// The probe takes an exclusive lock; it must give it back.
	dir := t.TempDir()
	path := newTestDB(t, dir)
	for i := 0; i < 3; i++ {
		if err := assertNoLiveDaemon(config.Config{DataDir: dir, RunFilePath: filepath.Join(dir, "running.json")}); err != nil {
			t.Fatalf("probe %d: %v", i, err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO t (x) VALUES (2)`); err != nil {
		t.Fatalf("the probe left the database locked: %v", err)
	}
}

func TestTheProbeCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	path := newTestDB(t, dir)
	if err := assertNoLiveDaemon(config.Config{DataDir: dir, RunFilePath: filepath.Join(dir, "running.json")}); err != nil {
		t.Fatalf("probe: %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE '%probe%'`).Scan(&n); err != nil {
		t.Fatalf("inspect schema: %v", err)
	}
	if n != 0 {
		t.Fatalf("the probe left %d object(s) behind", n)
	}
}
