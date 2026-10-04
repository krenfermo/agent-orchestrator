package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/daemonmeta"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// Every test here builds its own scratch installation. None of them opens,
// copies or restores the operator's ~/.ao.

const testSecretKey = "c2NyYXRjaC1rZXktbmV2ZXItcmVhbC0wMTIzNDU2Nzg5YWI="

// newInstallation creates a migrated data dir with an installation identity, a
// secret key and a small skill catalog, and returns its path.
func newInstallation(t *testing.T, parent string) string {
	t.Helper()
	dataDir := filepath.Join(parent, "data")
	store, err := sqlitetest.Open(dataDir)
	if err != nil {
		t.Fatalf("open scratch store: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := daemonmeta.LoadOrCreateInstallationID(dataDir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dataDir, secretKeyFile), testSecretKey, 0o600)
	writeFile(t, filepath.Join(dataDir, "skills", "catalog", "pkg-a", "SKILL.md"), "# skill a\n", 0o644)
	writeFile(t, filepath.Join(dataDir, "skills", "catalog", "pkg-a", "scripts", "run.sh"), "#!/bin/sh\necho a\n", 0o755)
	return dataDir
}

func writeFile(t *testing.T, p, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

// openRW opens the scratch database the way the daemon does (WAL).
func openRW(t *testing.T, dataDir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, DatabaseAsset)+
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	return db
}

// mutate runs statements against the scratch database and closes it.
func mutate(t *testing.T, dataDir string, stmts ...string) {
	t.Helper()
	db := openRW(t, dataDir)
	defer func() { _ = db.Close() }()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func addProject(id string) string {
	return fmt.Sprintf(`INSERT INTO projects (id, path, registered_at) VALUES ('%s', '/tmp/%s', CURRENT_TIMESTAMP)`, id, id)
}

// projectIDs reads project ids from a standalone database file (no WAL).
func projectIDs(t *testing.T, dbPath string) []string {
	t.Helper()
	db, err := openImmutable(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT id FROM projects ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

// liveProjectIDs reads through a normal connection, WAL included.
func liveProjectIDs(t *testing.T, dataDir string) []string {
	t.Helper()
	db := openRW(t, dataDir)
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT id FROM projects ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

// semanticState is what a round trip must preserve: every table's row count,
// the goose version, and the key records.
func semanticState(t *testing.T, dataDir string) string {
	t.Helper()
	db := openRW(t, dataDir)
	defer func() { _ = db.Close() }()
	var b strings.Builder
	for _, tbl := range tableNames(t, db) {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM "` + tbl + `"`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s=%d\n", tbl, n)
	}
	v, err := readGooseVersion(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&b, "goose=%d\nprojects=%v\n", v, liveProjectIDs(t, dataDir))
	return b.String()
}

func tableNames(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return tables
}

// treeDigest fingerprints every file under dir (names, modes, contents), so a
// test can prove an operation left a directory exactly as it was.
func treeDigest(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		info, _ := d.Info()
		if d.IsDir() {
			lines = append(lines, fmt.Sprintf("d %s %o", rel, info.Mode().Perm()))
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, _ = io.Copy(h, f)
		_ = f.Close()
		lines = append(lines, fmt.Sprintf("f %s %o %s", rel, info.Mode().Perm(), hex.EncodeToString(h.Sum(nil))))
		return nil
	})
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// managedDigest fingerprints only what a restore may replace.
func managedDigest(t *testing.T, dataDir string) string {
	t.Helper()
	var b strings.Builder
	for _, e := range managedEntries {
		p := filepath.Join(dataDir, filepath.FromSlash(e))
		fi, err := os.Lstat(p)
		if err != nil {
			fmt.Fprintf(&b, "%s absent\n", e)
			continue
		}
		if fi.IsDir() {
			fmt.Fprintf(&b, "%s dir\n%s\n", e, treeDigest(t, p))
			continue
		}
		f, _ := os.Open(p)
		h := sha256.New()
		_, _ = io.Copy(h, f)
		_ = f.Close()
		fmt.Fprintf(&b, "%s %s\n", e, hex.EncodeToString(h.Sum(nil)))
	}
	return b.String()
}

func mustCreate(t *testing.T, dataDir, root string) *CreateResult {
	t.Helper()
	res, err := Create(context.Background(), CreateOptions{DataDir: dataDir, Root: root, Tool: ToolInfo{Name: "ao-test"}})
	if err != nil {
		t.Fatalf("create backup: %v", err)
	}
	return res
}

func noDaemon(context.Context) error { return nil }

// peakHeapDuring samples the live heap while fn runs and returns the baseline
// and the peak, so a test can prove a large file is streamed, not loaded.
func peakHeapDuring(fn func()) (base, peak uint64) {
	var s runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&s)
	base, peak = s.HeapInuse, s.HeapInuse
	done := make(chan struct{})
	var mu sync.Mutex
	go func() {
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				mu.Lock()
				peak = max(peak, ms.HeapInuse)
				mu.Unlock()
			}
		}
	}()
	fn()
	close(done)
	mu.Lock()
	defer mu.Unlock()
	return base, peak
}

func testHead(t *testing.T) int64 {
	t.Helper()
	h, err := sqlite.MigrationHead()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func codeOf(err error) Code {
	if e, ok := AsError(err); ok {
		return e.Code
	}
	return ""
}

func classOf(err error) Class {
	if e, ok := AsError(err); ok {
		return e.Class
	}
	return ""
}

func hasReason(rep *VerifyReport, code Code) bool {
	for _, r := range rep.Reasons {
		if r.Code == code {
			return true
		}
	}
	return false
}

func countBackups(t *testing.T, root string, kind Kind) int {
	t.Helper()
	rep, err := List("", root)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range rep.Entries {
		if e.State == StateOK && e.Kind == kind {
			n++
		}
	}
	return n
}
