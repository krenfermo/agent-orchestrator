package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Independent review §22: restoring precisely BECAUSE the current database is
// broken. Such a database cannot produce a VALID pre-restore backup, and before
// the fix a restore over it was refused for ever (the SQLite probe itself could
// not open it). Now: refused by default with a clear code, and possible with
// --preserve-broken-state, which keeps every byte of the broken state first.

var damageKinds = []string{"page-damage", "not-a-database", "foreign-key-violation"}

func damageDatabase(t *testing.T, dataDir, kind string) {
	t.Helper()
	dbPath := filepath.Join(dataDir, DatabaseAsset)
	switch kind {
	case "page-damage":
		fh, err := os.OpenFile(dbPath, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		junk := make([]byte, 4096)
		for i := range junk {
			junk[i] = byte(i*7 + 3)
		}
		fi, _ := fh.Stat()
		for off := int64(4096 * 2); off+4096 < fi.Size(); off += 4096 * 5 {
			if _, err := fh.WriteAt(junk, off); err != nil {
				t.Fatal(err)
			}
		}
		_ = fh.Close()
	case "not-a-database":
		writeFile(t, dbPath, strings.Repeat("garbage!", 4096), 0o600)
	case "foreign-key-violation":
		mutate(t, dataDir, `PRAGMA foreign_keys=OFF`,
			`INSERT INTO workflow_runs (id, project_id, objective, state, policy_version, policy_snapshot, created_at, updated_at)
			 VALUES ('orphan', 'no-such-project', 'o', 'running', 'v1', '{}', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`)
	default:
		t.Fatalf("unknown damage %q", kind)
	}
}

func fileSum(t *testing.T, p string) string {
	t.Helper()
	_, sum, err := hashFile(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

func forensicCopies(t *testing.T, root string) []string {
	t.Helper()
	entries, _ := os.ReadDir(root)
	var out []string
	for _, e := range entries {
		if strings.Contains(e.Name(), forensicPrefix) {
			out = append(out, filepath.Join(root, e.Name()))
		}
	}
	return out
}

func TestRestoreRefusesADamagedDestinationByDefault(t *testing.T) {
	for _, kind := range damageKinds {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			damageDatabase(t, f.dataDir, kind)
			sum := fileSum(t, filepath.Join(f.dataDir, DatabaseAsset))
			before := stripSidecars(managedDigest(t, f.dataDir))

			rep, err := f.restore(t, nil)
			if codeOf(err) != CodeDestinationDamaged || classOf(err) != ClassRefused || rep.DestinationTouched {
				t.Fatalf("err=%v rep=%+v", err, rep)
			}
			if !strings.Contains(err.Error(), "--preserve-broken-state") {
				t.Fatalf("the refusal does not say how to proceed: %v", err)
			}
			if fileSum(t, filepath.Join(f.dataDir, DatabaseAsset)) != sum || stripSidecars(managedDigest(t, f.dataDir)) != before {
				t.Fatal("the refused restore changed the damaged database")
			}
			assertNoRestoreLeftovers(t, f.dataDir)
			if len(forensicCopies(t, f.root)) != 0 {
				t.Fatal("a refused restore left a forensic copy")
			}
		})
	}
}

func TestRestoreWithPreserveBrokenStateKeepsAForensicCopy(t *testing.T) {
	for _, kind := range damageKinds {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			damageDatabase(t, f.dataDir, kind)
			dbSum := fileSum(t, filepath.Join(f.dataDir, DatabaseAsset))
			identity, _ := os.ReadFile(filepath.Join(f.dataDir, IdentityAsset))
			skillSum := fileSum(t, filepath.Join(f.dataDir, "skills", "catalog", "pkg-b", "SKILL.md"))

			rep, err := f.restore(t, func(o *RestoreOptions) { o.PreserveBrokenState = true })
			if err != nil || rep.Result != ResultRestored || rep.RollbackKind != RollbackForensicCopy || !hasWarning(rep, CodeDestinationDamaged) {
				t.Fatalf("err=%v rep=%+v", err, rep)
			}
			if semanticState(t, f.dataDir) != f.stateA {
				t.Fatal("state A was not restored")
			}
			if integrity, _ := integrityOf(t, f.dataDir); integrity != "ok" {
				t.Fatalf("restored integrity %q", integrity)
			}
			assertNoRestoreLeftovers(t, f.dataDir)

			copyDir := rep.RollbackBackupPath
			resolvedRoot, _ := filepath.EvalSymlinks(f.root)
			if filepath.Dir(copyDir) != resolvedRoot || !strings.HasPrefix(filepath.Base(copyDir), forensicPrefix) {
				t.Fatalf("forensic copy at %s", copyDir)
			}
			if fileSum(t, filepath.Join(copyDir, DatabaseAsset)) != dbSum {
				t.Fatal("the forensic copy of ao.db is not the damaged file byte for byte")
			}
			if got, _ := os.ReadFile(filepath.Join(copyDir, IdentityAsset)); string(got) != string(identity) {
				t.Fatal("the forensic copy lost the installation identity")
			}
			if fileSum(t, filepath.Join(copyDir, "skills", "catalog", "pkg-b", "SKILL.md")) != skillSum {
				t.Fatal("the forensic copy lost a skill file")
			}
			raw, err := os.ReadFile(filepath.Join(copyDir, ForensicManifestName))
			if err != nil {
				t.Fatal(err)
			}
			var m forensicManifest
			if err := json.Unmarshal(raw, &m); err != nil || m.Format != forensicFormat || m.RestoreID != rep.RestoreID || len(m.Files) == 0 {
				t.Fatalf("forensic manifest %+v (%v)", m, err)
			}
			if strings.Contains(string(raw), f.parent) {
				t.Fatal("the forensic manifest names an absolute path")
			}

			// Evidence, not a backup: verify says so, list does not show it, and
			// retention never deletes it.
			if vr, err := Verify(context.Background(), copyDir, VerifyOptions{}); err != nil || vr.Status == StatusValid {
				t.Fatalf("verify of a forensic copy: %+v %v", vr, err)
			}
			lr, err := List(f.dataDir, f.root)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range lr.Entries {
				if strings.HasPrefix(e.Name, forensicPrefix) {
					t.Fatal("list shows the forensic copy as a backup")
				}
			}
			if _, err := Prune(context.Background(), PruneOptions{Root: f.root, KeepLast: 1, Apply: true}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(copyDir, DatabaseAsset)); err != nil {
				t.Fatal("prune removed the forensic copy")
			}
		})
	}
}

// The flag is consent for ONE thing -- a damaged database -- not a --force: a
// pre-restore backup that fails for lack of space still refuses.
func TestPreserveBrokenStateDoesNotOverrideAnOrdinaryPreRestoreFailure(t *testing.T) {
	f := newFixture(t)
	before := stripSidecars(managedDigest(t, f.dataDir))
	rep, err := f.restore(t, func(o *RestoreOptions) {
		o.PreserveBrokenState = true
		o.hooks = &testHooks{createRollback: func(context.Context, CreateOptions) (*CreateResult, error) {
			return nil, failedf(CodeSnapshotFailed, syscall.ENOSPC, "snapshot")
		}}
	})
	if codeOf(err) != CodeRollbackBackupFailed || rep.DestinationTouched || rep.RollbackKind != "" {
		t.Fatalf("err=%v rep=%+v", err, rep)
	}
	if stripSidecars(managedDigest(t, f.dataDir)) != before || len(forensicCopies(t, f.root)) != 0 {
		t.Fatal("an ordinary pre-restore failure was papered over")
	}
	assertNoRestoreLeftovers(t, f.dataDir)
}

// A crash in the middle of a restore over a damaged database: recover must put
// the damaged original back byte for byte, without being able to open it.
func TestRecoverPutsADamagedOriginalBackByteForByte(t *testing.T) {
	for _, mode := range []string{"after-rename:2", "phase:" + string(PhaseSwapped)} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("AO_P10_CRASH_PRESERVE", "1")
			f := newFixture(t)
			damageDatabase(t, f.dataDir, "not-a-database")
			sum := fileSum(t, filepath.Join(f.dataDir, DatabaseAsset))
			crashChild(t, mode, f.dataDir, f.root, f.backupA.Path)
			if CheckStartup(f.dataDir) == nil {
				t.Fatal("boot allowed over a crashed swap")
			}
			rr, err := Recover(context.Background(), RecoverOptions{DataDir: f.dataDir, CheckDaemon: noDaemon})
			if err != nil || rr.Result != RecoverRolledBack {
				t.Fatalf("recover: %+v %v", rr, err)
			}
			if fileSum(t, filepath.Join(f.dataDir, DatabaseAsset)) != sum {
				t.Fatal("recover did not put the damaged original back byte for byte")
			}
			assertNoRestoreLeftovers(t, f.dataDir)
			if rr, err := Recover(context.Background(), RecoverOptions{DataDir: f.dataDir, CheckDaemon: noDaemon}); err != nil || rr.Result != RecoverNothing {
				t.Fatalf("second recover: %+v %v", rr, err)
			}
		})
	}
}
