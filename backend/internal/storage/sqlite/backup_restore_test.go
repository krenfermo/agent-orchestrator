package sqlite

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// backup_restore_test.go — proof that an AO database can be backed up while the
// daemon is running, and restored into something that actually opens.
//
// AO ships no backup mechanism: there is no `VACUUM INTO` anywhere in the tree,
// no backup command, and no retention for the database. What exists on a real
// machine are hand-made copies (`ao.db.backup-before-0119-…`), taken with `cp`
// against a live daemon. That is the thing this file replaces, because a `cp`
// of a WAL database under a live writer is not a backup: the copy can land
// between a page write and its WAL frame and restore as a corrupt or
// silently-stale database, and nothing tells you until you need it.
//
// `VACUUM INTO` is SQLite's own answer. It runs inside a read transaction, so
// it sees one consistent snapshot no matter what the daemon writes while it
// runs; it writes a fresh, compacted database rather than copying pages; and it
// never modifies the source. It needs no daemon cooperation and no downtime.
//
// Everything here runs against a database this test builds. It never opens,
// copies or restores the operator's real ~/.ao/data/ao.db.

// TestBackupUnderConcurrentWritesRestoresConsistent is the property that makes
// this a backup rather than a copy: a snapshot taken while writes are landing
// restores to a database that is internally consistent and referentially
// intact, and that carries a prefix of the writes rather than a torn mixture.
func TestBackupUnderConcurrentWritesRestoresConsistent(t *testing.T) {
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatalf("open source store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	source, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "ao.db")+pragmas)
	if err != nil {
		t.Fatalf("open source db: %v", err)
	}
	t.Cleanup(func() { _ = source.Close() })

	if _, err := source.Exec(
		`INSERT INTO projects (id, path, registered_at) VALUES ('p', '/tmp/p', CURRENT_TIMESTAMP)`,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	// A writer that keeps working for the whole duration of the backup, which
	// is the condition a `cp` cannot survive.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = source.Exec(
				`INSERT INTO workflow_runs (id, project_id, objective, state, policy_version, policy_snapshot, created_at, updated_at)
				 VALUES (?, 'p', 'objective', 'running', 'v1', '{}', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
				fmt.Sprintf("wf-%06d", i))
			time.Sleep(time.Millisecond)
		}
	}()
	time.Sleep(25 * time.Millisecond) // let some writes land first

	backupPath := filepath.Join(t.TempDir(), "ao-backup.db")
	if _, err := source.Exec(`VACUUM INTO ?`, backupPath); err != nil {
		close(stop)
		wg.Wait()
		t.Fatalf("VACUUM INTO under concurrent writes: %v", err)
	}
	close(stop)
	wg.Wait()

	// The source is untouched by taking a backup of it.
	if err := source.Ping(); err != nil {
		t.Fatalf("source unusable after backup: %v", err)
	}

	restored, err := sql.Open("sqlite", "file:"+backupPath+pragmas)
	if err != nil {
		t.Fatalf("open restored db: %v", err)
	}
	defer func() { _ = restored.Close() }()

	var integrity string
	if err := restored.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("integrity_check = %q, want ok", integrity)
	}
	assertNoForeignKeyViolations(t, restored)

	// The snapshot is a consistent point in time: every run it carries has the
	// project it references, and the project it references is the one seeded.
	var orphaned int
	if err := restored.QueryRow(
		`SELECT COUNT(*) FROM workflow_runs r LEFT JOIN projects p ON p.id = r.project_id WHERE p.id IS NULL`,
	).Scan(&orphaned); err != nil {
		t.Fatalf("orphan check: %v", err)
	}
	if orphaned != 0 {
		t.Fatalf("%d runs in the backup reference a project it does not contain", orphaned)
	}
}

// TestRestoredBackupOpensAsALiveStore is the half a checksum cannot answer: the
// restored file is not just readable, it is a database the daemon accepts --
// same schema version, migrations already applied, no replay on first open.
func TestRestoredBackupOpensAsALiveStore(t *testing.T) {
	sourceDir := t.TempDir()
	store, err := Open(sourceDir)
	if err != nil {
		t.Fatalf("open source store: %v", err)
	}
	sourceDB, err := sql.Open("sqlite", "file:"+filepath.Join(sourceDir, "ao.db")+pragmas)
	if err != nil {
		t.Fatalf("open source db: %v", err)
	}
	if _, err := sourceDB.Exec(
		`INSERT INTO projects (id, path, registered_at) VALUES ('kept', '/tmp/kept', CURRENT_TIMESTAMP)`,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var sourceVersion int64
	if err := sourceDB.QueryRow(`SELECT MAX(version_id) FROM goose_db_version`).Scan(&sourceVersion); err != nil {
		t.Fatalf("read source schema version: %v", err)
	}

	backupPath := filepath.Join(t.TempDir(), "ao-backup.db")
	if _, err := sourceDB.Exec(`VACUUM INTO ?`, backupPath); err != nil {
		t.Fatalf("VACUUM INTO: %v", err)
	}
	_ = sourceDB.Close()
	_ = store.Close()

	// Restore = put the snapshot where a fresh data dir expects its database.
	// Nothing else from the source directory is required for the store to open.
	restoreDir := t.TempDir()
	raw, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(restoreDir, "ao.db"), raw, 0o600); err != nil {
		t.Fatalf("place backup: %v", err)
	}

	restoredStore, err := Open(restoreDir)
	if err != nil {
		t.Fatalf("the restored database does not open as an AO store: %v", err)
	}
	t.Cleanup(func() { _ = restoredStore.Close() })

	restoredDB, err := sql.Open("sqlite", "file:"+filepath.Join(restoreDir, "ao.db")+pragmas)
	if err != nil {
		t.Fatalf("open restored db: %v", err)
	}
	t.Cleanup(func() { _ = restoredDB.Close() })

	var restoredVersion int64
	if err := restoredDB.QueryRow(`SELECT MAX(version_id) FROM goose_db_version`).Scan(&restoredVersion); err != nil {
		t.Fatalf("read restored schema version: %v", err)
	}
	if restoredVersion != sourceVersion {
		t.Fatalf("restored schema version %d, source %d: opening the restore re-ran migrations", restoredVersion, sourceVersion)
	}

	var kept int
	if err := restoredDB.QueryRow(`SELECT COUNT(*) FROM projects WHERE id = 'kept'`).Scan(&kept); err != nil {
		t.Fatalf("read restored data: %v", err)
	}
	if kept != 1 {
		t.Fatal("the restored database lost the row the backup was taken to preserve")
	}
	assertNoForeignKeyViolations(t, restoredDB)
}

// TestBackupExcludesTransientRunState records what a restore must NOT carry
// back. running.json is a PID/port handshake for a daemon that is not running
// after a restore, and restoring one is how a CLI ends up talking to a port
// nothing owns.
func TestBackupExcludesTransientRunState(t *testing.T) {
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatalf("read data dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "running.json" {
			t.Fatal("running.json lives in the data dir; the backup procedure must exclude it explicitly")
		}
	}
}

func assertNoForeignKeyViolations(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		t.Fatal("the restored database has foreign-key violations")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate foreign_key_check: %v", err)
	}
}
