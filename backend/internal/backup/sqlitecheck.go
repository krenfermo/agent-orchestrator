package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	_ "modernc.org/sqlite" // snapshots and checks open SQLite directly
)

// dbFacts are what the checks learned about one database file.
type dbFacts struct {
	Integrity            string
	ForeignKeyViolations int
	GooseVersion         int64
}

// openImmutable opens a standalone database file without locking it, without
// reading or creating a -wal/-shm, and without any possibility of writing. It
// is only correct for a file nothing else is writing: a backup's snapshot, a
// staged copy, or a just-promoted restore.
func openImmutable(path string) (*sql.DB, error) {
	if err := sqlitePathSafe(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&immutable=1")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// inspectDatabase runs integrity_check (or quick_check), foreign_key_check and
// reads the goose version. It never migrates: sqlite.Open is deliberately not
// used, because opening through it would upgrade the very file under check.
func inspectDatabase(ctx context.Context, path string, quick bool) (dbFacts, error) {
	var facts dbFacts
	db, err := openImmutable(path)
	if err != nil {
		return facts, err
	}
	defer func() { _ = db.Close() }()

	pragma := "integrity_check"
	if quick {
		pragma = "quick_check"
	}
	lines, err := pragmaLines(ctx, db, pragma, 10)
	if err != nil {
		return facts, fmt.Errorf("%s: %w", pragma, err)
	}
	facts.Integrity = strings.Join(lines, "; ")

	facts.ForeignKeyViolations, err = countRows(ctx, db, "PRAGMA foreign_key_check")
	if err != nil {
		return facts, fmt.Errorf("foreign_key_check: %w", err)
	}

	facts.GooseVersion, err = readGooseVersion(ctx, db)
	return facts, err
}

// pragmaLines returns up to limit first-column lines a PRAGMA reports.
func pragmaLines(ctx context.Context, db *sql.DB, pragma string, limit int) ([]string, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA "+pragma)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, err
		}
		if len(lines) < limit {
			lines = append(lines, line)
		}
	}
	return lines, rows.Err()
}

// countRows counts the rows a query returns without reading them.
func countRows(ctx context.Context, db *sql.DB, query string) (int, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		n++
	}
	return n, rows.Err()
}

func readGooseVersion(ctx context.Context, db *sql.DB) (int64, error) {
	var tables int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'goose_db_version'`,
	).Scan(&tables); err != nil {
		return 0, fmt.Errorf("read schema: %w", err)
	}
	if tables == 0 {
		return 0, errors.New("no goose_db_version table: not an AO database")
	}
	var v int64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied = 1`,
	).Scan(&v); err != nil {
		return 0, fmt.Errorf("read goose version: %w", err)
	}
	return v, nil
}

// snapshotDatabase writes a consistent, standalone copy of src to dst with
// VACUUM INTO. The statement runs inside ONE read transaction, so the copy is a
// single point in time that includes every committed WAL frame, whatever a live
// daemon writes meanwhile. The source connection is read-only: it never writes
// a page of the source. dst must not exist.
func snapshotDatabase(ctx context.Context, src, dst string) (journalMode string, err error) {
	if err := sqlitePathSafe(src); err != nil {
		return "", err
	}
	db, err := sql.Open("sqlite", "file:"+src+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		return "", fmt.Errorf("read journal mode: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "VACUUM INTO ?", dst); err != nil {
		return "", err
	}
	return journalMode, nil
}

// ProbeExclusive asks SQLite whether any other connection has the database
// open. It takes an exclusive lock with busy_timeout(0) inside a transaction it
// rolls back, so it writes nothing and fails immediately if a daemon, a
// sqlite3 shell or another ao process holds the file -- a fact about the
// database, not about a run-file. A missing database is quiet; mode=rw never
// creates one.
func ProbeExclusive(dbPath string) error {
	if _, err := os.Lstat(dbPath); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := sqlitePathSafe(dbPath); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=rw&_pragma=busy_timeout(0)&_pragma=locking_mode(exclusive)")
	if err != nil {
		return fmt.Errorf("probe database: %w", err)
	}
	defer func() { _ = db.Close() }()
	tx, err := db.Begin()
	if err == nil {
		_, err = tx.Exec("CREATE TABLE IF NOT EXISTS ao_backup_lock_probe_never_created (x INTEGER)")
		if rerr := tx.Rollback(); rerr != nil && err == nil {
			err = rerr
		}
	}
	if err != nil {
		if isBusy(err) {
			return refusedf(CodeDBInUse, "another process has AO's database open (%s); stop it before restoring", dbPath)
		}
		return fmt.Errorf("probe database %s: %w", dbPath, err)
	}
	return nil
}

func isBusy(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "database is locked") || strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "SQLITE_LOCKED") || strings.Contains(msg, "database table is locked")
}
