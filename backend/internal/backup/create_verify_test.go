package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// §51 — the property that makes this a backup rather than a copy of ao.db:
// rows that live ONLY in the uncheckpointed WAL are in the snapshot, and the
// snapshot stands alone without any -wal.
func TestCreateCapturesUncheckpointedWALWithoutCopyingIt(t *testing.T) {
	dataDir := newInstallation(t, t.TempDir())
	root := filepath.Join(t.TempDir(), "backups")

	writer := openRW(t, dataDir)
	defer func() { _ = writer.Close() }()
	if _, err := writer.Exec(`PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(addProject("only-in-wal")); err != nil {
		t.Fatal(err)
	}
	wal, err := os.Stat(filepath.Join(dataDir, DatabaseAsset+"-wal"))
	if err != nil || wal.Size() == 0 {
		t.Fatalf("precondition: the write must be sitting in a non-empty WAL (%v)", err)
	}
	// Proof the row is NOT in the main file: a raw copy of ao.db alone lacks it.
	raw, err := os.ReadFile(filepath.Join(dataDir, DatabaseAsset))
	if err != nil {
		t.Fatal(err)
	}
	rawCopy := filepath.Join(t.TempDir(), "raw.db")
	if err := os.WriteFile(rawCopy, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(projectIDs(t, rawCopy), "only-in-wal") {
		t.Fatal("precondition: the row was already checkpointed into ao.db")
	}

	res := mustCreate(t, dataDir, root) // the writer is still open: an online backup

	if !slices.Contains(projectIDs(t, filepath.Join(res.Path, DatabaseAsset)), "only-in-wal") {
		t.Fatal("the backup lost a committed transaction that lived only in the WAL")
	}
	for _, s := range sqliteSidecars {
		if _, err := os.Lstat(filepath.Join(res.Path, s)); err == nil {
			t.Fatalf("the backup contains %s; a snapshot must stand alone", s)
		}
	}
	rep, err := Verify(context.Background(), res.Path, VerifyOptions{})
	if err != nil || rep.Status != StatusValid || rep.Compatibility != CompatCompatible {
		t.Fatalf("verify: %+v err=%v", rep, err)
	}
	if res.Manifest.Source.JournalMode != "wal" {
		t.Fatalf("journal mode observed %q, want wal", res.Manifest.Source.JournalMode)
	}
}

// §35 — a backup taken while a writer keeps inserting is a consistent point in
// time: integrity ok, no FK violations, and a prefix of the writes.
func TestCreateUnderConcurrentWritesIsConsistent(t *testing.T) {
	dataDir := newInstallation(t, t.TempDir())
	root := filepath.Join(t.TempDir(), "backups")
	mutate(t, dataDir, addProject("p"))

	writer := openRW(t, dataDir)
	defer func() { _ = writer.Close() }()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var written int
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := writer.Exec(
				`INSERT INTO workflow_runs (id, project_id, objective, state, policy_version, policy_snapshot, created_at, updated_at)
				 VALUES (?, 'p', 'o', 'running', 'v1', '{}', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, fmt.Sprintf("wf-%06d", i)); err == nil {
				written = i + 1
			}
		}
	}()
	time.Sleep(30 * time.Millisecond)
	res, err := Create(context.Background(), CreateOptions{DataDir: dataDir, Root: root, Tool: ToolInfo{Name: "ao-test"}})
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("create under concurrent writes: %v", err)
	}
	rep, err := Verify(context.Background(), res.Path, VerifyOptions{})
	if err != nil || rep.Status != StatusValid {
		t.Fatalf("verify: %+v err=%v", rep, err)
	}
	db, _ := openImmutable(filepath.Join(res.Path, DatabaseAsset))
	defer func() { _ = db.Close() }()
	var runs int
	var maxID string
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(id), '') FROM workflow_runs`).Scan(&runs, &maxID); err != nil {
		t.Fatal(err)
	}
	if runs == 0 || runs > written {
		t.Fatalf("snapshot has %d runs, writer committed %d", runs, written)
	}
	// A prefix, not a torn mixture: ids are dense from 0.
	if maxID != fmt.Sprintf("wf-%06d", runs-1) {
		t.Fatalf("snapshot is not a prefix of the writes: %d runs, max id %s", runs, maxID)
	}
}

// §34 — concurrent creates are allowed and never corrupt each other.
func TestConcurrentCreatesNeverCollide(t *testing.T) {
	dataDir := newInstallation(t, t.TempDir())
	root := filepath.Join(t.TempDir(), "backups")
	const n = 4
	results := make([]*CreateResult, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = Create(context.Background(), CreateOptions{DataDir: dataDir, Root: root, Tool: ToolInfo{Name: "ao-test"}})
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("create %d: %v", i, errs[i])
		}
		if seen[results[i].Path] {
			t.Fatalf("two creates produced %s", results[i].Path)
		}
		seen[results[i].Path] = true
		if rep, err := Verify(context.Background(), results[i].Path, VerifyOptions{Quick: true}); err != nil || rep.Status != StatusValid {
			t.Fatalf("backup %d invalid: %+v %v", i, rep, err)
		}
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), stagingPrefix) {
			t.Fatalf("a staging dir survived: %s", e.Name())
		}
	}
}

// §38/§65 — a canceled or failed create keeps nothing that looks like a
// backup, and never changes the source.
func TestCreateFailureOrCancelKeepsNothingAndTouchesNoSource(t *testing.T) {
	for _, mode := range []string{"cancel", "fail"} {
		t.Run(mode, func(t *testing.T) {
			dataDir := newInstallation(t, t.TempDir())
			root := filepath.Join(t.TempDir(), "backups")
			mutate(t, dataDir, addProject("a"))
			before := managedDigest(t, dataDir)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hooks := &testHooks{afterSnapshot: func(string) error {
				if mode == "cancel" {
					cancel()
					return nil
				}
				return errors.New("injected failure after the snapshot")
			}}
			_, err := Create(ctx, CreateOptions{DataDir: dataDir, Root: root, Tool: ToolInfo{Name: "ao-test"}, hooks: hooks})
			if err == nil {
				t.Fatal("create succeeded")
			}
			if mode == "cancel" && codeOf(err) != CodeCanceled {
				t.Fatalf("code=%s err=%v", codeOf(err), err)
			}
			entries, _ := os.ReadDir(root)
			for _, e := range entries {
				if ValidBackupID(e.Name()) || strings.HasPrefix(e.Name(), stagingPrefix) {
					t.Fatalf("%s survived a %s", e.Name(), mode)
				}
			}
			// Sidecars may exist (a read-only reader creates them) but every
			// managed file's content is unchanged.
			after := managedDigest(t, dataDir)
			if stripSidecars(before) != stripSidecars(after) {
				t.Fatalf("the source changed:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

func stripSidecars(digest string) string {
	var keep []string
	for _, line := range strings.Split(digest, "\n") {
		if strings.HasPrefix(line, DatabaseAsset+"-") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}

// §72 — a backup root inside the data dir, or one that is a file, is refused
// before anything is created.
func TestCreateRefusesADangerousRoot(t *testing.T) {
	dataDir := newInstallation(t, t.TempDir())
	for name, root := range map[string]string{
		"data dir itself":  dataDir,
		"inside data dir":  filepath.Join(dataDir, "backups"),
		"the database":     filepath.Join(dataDir, DatabaseAsset),
		"nested new dir":   filepath.Join(dataDir, "a", "b"),
		"a file elsewhere": filepath.Join(dataDir, secretKeyFile),
	} {
		_, err := Create(context.Background(), CreateOptions{DataDir: dataDir, Root: root, Tool: ToolInfo{Name: "ao-test"}})
		if codeOf(err) != CodeInvalidBackupRoot {
			t.Errorf("%s: err=%v, want %s", name, err, CodeInvalidBackupRoot)
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, "a")); err == nil {
		t.Fatal("a refused root was created anyway")
	}
}

// §40 — nothing is followed through a symlink.
func TestCreateRefusesSymlinksInTheSource(t *testing.T) {
	t.Run("skill catalog", func(t *testing.T) {
		dataDir := newInstallation(t, t.TempDir())
		outside := filepath.Join(t.TempDir(), "outside.txt")
		writeFile(t, outside, "not ours", 0o600)
		if err := os.Symlink(outside, filepath.Join(dataDir, "skills", "catalog", "pkg-a", "link")); err != nil {
			t.Fatal(err)
		}
		_, err := Create(context.Background(), CreateOptions{DataDir: dataDir, Root: filepath.Join(t.TempDir(), "b"), Tool: ToolInfo{Name: "ao-test"}})
		if codeOf(err) != CodeUnsafePath {
			t.Fatalf("err=%v, want %s", err, CodeUnsafePath)
		}
	})
	t.Run("database", func(t *testing.T) {
		dataDir := newInstallation(t, t.TempDir())
		real := filepath.Join(t.TempDir(), "real.db")
		if err := os.Rename(filepath.Join(dataDir, DatabaseAsset), real); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(dataDir, DatabaseAsset)); err != nil {
			t.Fatal(err)
		}
		_, err := Create(context.Background(), CreateOptions{DataDir: dataDir, Root: filepath.Join(t.TempDir(), "b"), Tool: ToolInfo{Name: "ao-test"}})
		if codeOf(err) != CodeUnsafePath {
			t.Fatalf("err=%v, want %s", err, CodeUnsafePath)
		}
	})
}

// §39 — permissions are owner-only and never relaxed.
func TestCreatePermissions(t *testing.T) {
	dataDir := newInstallation(t, t.TempDir())
	res := mustCreate(t, dataDir, filepath.Join(t.TempDir(), "backups"))
	check := func(rel string, want os.FileMode) {
		fi, err := os.Stat(filepath.Join(res.Path, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode %o, want %o", rel, got, want)
		}
	}
	check(".", 0o700)
	check(ManifestName, 0o600)
	check(DatabaseAsset, 0o600)
	check(IdentityAsset, 0o600)
	check("skills/catalog/pkg-a/SKILL.md", 0o600)
	check("skills/catalog/pkg-a/scripts/run.sh", 0o700)
	_ = filepath.WalkDir(res.Path, func(p string, d fs.DirEntry, err error) error {
		info, _ := d.Info()
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is readable or writable by group/other: %o", p, info.Mode().Perm())
		}
		return nil
	})
}

// §42 — no secret, credential, prompt or absolute path reaches the backup, its
// manifest, or the verify report.
func TestBackupCarriesNoSecrets(t *testing.T) {
	parent := t.TempDir()
	dataDir := newInstallation(t, parent)
	secrets := []string{
		"sk-ant-SECRET-PROVIDER-TOKEN-123", "ghp_SECRETCLITOKEN456", "PROMPT-TEXT-THAT-MUST-STAY", "OIDC_CLIENT_SECRET_789",
	}
	writeFile(t, filepath.Join(dataDir, "cli-credentials.json"), `{"token":"`+secrets[1]+`"}`, 0o600)
	writeFile(t, filepath.Join(dataDir, "users", "u1", "claude", "credentials.json"), secrets[0], 0o600)
	writeFile(t, filepath.Join(dataDir, "prompts", "s1", "system.md"), secrets[2], 0o600)
	writeFile(t, filepath.Join(dataDir, "agent-credentials", "a1"), secrets[3], 0o600)
	t.Setenv("AO_OIDC_CLIENT_SECRET", secrets[3])

	res := mustCreate(t, dataDir, filepath.Join(parent, "backups"))
	rep, err := Verify(context.Background(), res.Path, VerifyOptions{})
	if err != nil || rep.Status != StatusValid {
		t.Fatalf("verify: %+v %v", rep, err)
	}
	reportJSON, _ := json.Marshal(rep)
	resultJSON, _ := json.Marshal(res)
	blobs := map[string][]byte{"verify report": reportJSON, "create result": resultJSON}
	_ = filepath.WalkDir(res.Path, func(p string, d fs.DirEntry, err error) error {
		if !d.IsDir() {
			b, _ := os.ReadFile(p)
			blobs[p] = b
		}
		return nil
	})
	manifest := blobs[filepath.Join(res.Path, ManifestName)]
	for name, blob := range blobs {
		for _, s := range append(secrets, testSecretKey) {
			if bytes.Contains(blob, []byte(s)) {
				t.Errorf("%s contains secret %q", name, s)
			}
		}
	}
	if bytes.Contains(manifest, []byte(dataDir)) || bytes.Contains(manifest, []byte(parent)) {
		t.Error("the manifest names an absolute path")
	}
	if res.Manifest.Source.SecretKeyFingerprint == "" {
		t.Error("the secret key fingerprint is missing")
	}
	for _, a := range res.Manifest.Assets {
		if a.Role != RoleDatabase && a.Role != RoleInstallationIdentity && a.Role != RoleSkillPackage {
			t.Errorf("unexpected asset %+v", a)
		}
	}
}

// A backup of a data dir mid-restore would preserve a possibly mixed state.
func TestCreateRefusesADataDirWithAnInterruptedRestore(t *testing.T) {
	dataDir := newInstallation(t, t.TempDir())
	id, _ := newID("aor-", time.Now())
	if err := writeJournal(dataDir, &journal{RestoreID: id, WorkDir: restoreWorkPrefix + id, Phase: PhaseSwapping}); err != nil {
		t.Fatal(err)
	}
	_, err := Create(context.Background(), CreateOptions{DataDir: dataDir, Root: filepath.Join(t.TempDir(), "b"), Tool: ToolInfo{Name: "ao-test"}})
	if codeOf(err) != CodeRestoreInterrupted {
		t.Fatalf("err=%v", err)
	}
}

// §59 — verify streams: hashing a large asset never holds it in memory.
func TestVerifyHashesByStreaming(t *testing.T) {
	dataDir := newInstallation(t, t.TempDir())
	// ~32 MiB of rows so the database is much larger than the copy buffer.
	stmts := []string{addProject("big")}
	blob := strings.Repeat("x", 4000)
	for i := 0; i < 8000; i++ {
		stmts = append(stmts, fmt.Sprintf(
			`INSERT INTO workflow_runs (id, project_id, objective, state, policy_version, policy_snapshot, created_at, updated_at)
			 VALUES ('r%d', 'big', '%s', 'running', 'v1', '{}', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, i, blob))
	}
	mutate(t, dataDir, stmts...)
	res := mustCreate(t, dataDir, filepath.Join(t.TempDir(), "b"))
	var db Asset
	for _, a := range res.Manifest.Assets {
		if a.Role == RoleDatabase {
			db = a
		}
	}
	if db.Size < 30<<20 {
		t.Fatalf("fixture too small: %d", db.Size)
	}
	var rep *VerifyReport
	var err error
	base, peak := peakHeapDuring(func() {
		rep, err = Verify(context.Background(), res.Path, VerifyOptions{Quick: true})
	})
	if err != nil || rep.Status != StatusValid {
		t.Fatalf("verify: %+v %v", rep, err)
	}
	t.Logf("verify of a %d-byte database: heap baseline %d, peak %d", db.Size, base, peak)
	if peak > base+uint64(db.Size)/2 {
		t.Fatalf("heap peaked %d bytes above baseline verifying a %d-byte database: not streaming", peak-base, db.Size)
	}
}
