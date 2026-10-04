package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// The head is what a fresh database is migrated to: if these ever disagree, the
// backup compatibility verdicts built on MigrationHead are wrong.
func TestMigrationHeadIsTheVersionAFreshDatabaseReaches(t *testing.T) {
	head, err := MigrationHead()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ao.db")+pragmas)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var applied int64
	if err := db.QueryRow(`SELECT MAX(version_id) FROM goose_db_version WHERE is_applied = 1`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != head {
		t.Fatalf("MigrationHead()=%d, a fresh database reaches %d", head, applied)
	}
}
