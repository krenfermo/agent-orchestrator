package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/daemonlock"
	"github.com/google/uuid"
)

// fixture is a scratch installation with a backup of state A, then mutated to
// state B (a project removed, two added, one skill package installed).
type fixture struct {
	parent, dataDir, root string
	backupA               *CreateResult
	stateA, stateB        string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	parent := t.TempDir()
	dataDir := newInstallation(t, parent)
	root := filepath.Join(parent, "backups")
	mutate(t, dataDir, addProject("state-a"))
	stateA := semanticState(t, dataDir)
	backupA := mustCreate(t, dataDir, root)
	mutate(t, dataDir, `DELETE FROM projects WHERE id = 'state-a'`, addProject("state-b"), addProject("state-b2"))
	writeFile(t, filepath.Join(dataDir, "skills", "catalog", "pkg-b", "SKILL.md"), "# skill b\n", 0o644)
	stateB := semanticState(t, dataDir)
	if stateA == stateB {
		t.Fatal("fixture: states A and B are indistinguishable")
	}
	return &fixture{parent: parent, dataDir: dataDir, root: root, backupA: backupA, stateA: stateA, stateB: stateB}
}

func (f *fixture) restore(t *testing.T, mod func(*RestoreOptions)) (*RestoreReport, error) {
	t.Helper()
	opts := RestoreOptions{DataDir: f.dataDir, Source: f.backupA.Path, Root: f.root, CheckDaemon: noDaemon, Tool: ToolInfo{Name: "ao-test"}}
	if mod != nil {
		mod(&opts)
	}
	return Restore(context.Background(), opts)
}

// assertNoRestoreLeftovers: no journal, no work dir.
func assertNoRestoreLeftovers(t *testing.T, dataDir string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(dataDir, journalName)); err == nil {
		t.Error("a restore journal was left behind")
	}
	entries, _ := os.ReadDir(dataDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), restoreWorkPrefix) {
			t.Errorf("restore work dir %s was left behind", e.Name())
		}
	}
}

// hasCode walks the whole error chain.
func hasCode(err error, code Code) bool {
	for err != nil {
		var e *Error
		if !errors.As(err, &e) {
			return false
		}
		if e.Code == code {
			return true
		}
		err = e.Err
	}
	return false
}

// §23/§24/§L — A → backup → B → restore A: B disappears, A returns, semantically.
func TestRestoreRoundTripReturnsStateA(t *testing.T) {
	f := newFixture(t)
	idBefore, _ := os.ReadFile(filepath.Join(f.dataDir, IdentityAsset))
	keyBefore, _ := os.ReadFile(filepath.Join(f.dataDir, secretKeyFile))
	writeFile(t, filepath.Join(f.dataDir, "prompts", "s1", "system.md"), "not a restore's business", 0o600)

	rep, err := f.restore(t, nil)
	if err != nil {
		t.Fatalf("restore: %v (%+v)", err, rep)
	}
	if rep.Result != ResultRestored || !rep.DestinationTouched {
		t.Fatalf("report: %+v", rep)
	}
	for _, s := range sqliteSidecars {
		if _, err := os.Lstat(filepath.Join(f.dataDir, s)); err == nil {
			t.Fatalf("%s is present right after the restore", s)
		}
	}
	facts, err := inspectDatabase(context.Background(), filepath.Join(f.dataDir, DatabaseAsset), false)
	if err != nil || facts.Integrity != "ok" || facts.ForeignKeyViolations != 0 || facts.GooseVersion != testHead(t) {
		t.Fatalf("restored database: %+v err=%v", facts, err)
	}
	if got := semanticState(t, f.dataDir); got != f.stateA {
		t.Fatalf("restored state differs from A:\n got: %s\nwant: %s", got, f.stateA)
	}
	ids := liveProjectIDs(t, f.dataDir)
	if !slices.Contains(ids, "state-a") || slices.Contains(ids, "state-b") {
		t.Fatalf("projects after restore: %v", ids)
	}
	if _, err := os.Stat(filepath.Join(f.dataDir, "skills", "catalog", "pkg-b")); err == nil {
		t.Fatal("a skill package installed after the backup survived the restore")
	}
	if _, err := os.Stat(filepath.Join(f.dataDir, "skills", "catalog", "pkg-a", "scripts", "run.sh")); err != nil {
		t.Fatalf("a backed-up skill file was not restored: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(f.dataDir, "prompts", "s1", "system.md")); string(b) != "not a restore's business" {
		t.Fatal("the restore touched a file outside its managed entries")
	}
	if idAfter, _ := os.ReadFile(filepath.Join(f.dataDir, IdentityAsset)); string(idAfter) != string(idBefore) {
		t.Fatal("installation identity changed")
	}
	if keyAfter, _ := os.ReadFile(filepath.Join(f.dataDir, secretKeyFile)); string(keyAfter) != string(keyBefore) {
		t.Fatal("secret key changed")
	}
	for _, p := range []string{DatabaseAsset, IdentityAsset} {
		if fi, _ := os.Stat(filepath.Join(f.dataDir, p)); fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %o after restore, want 0600", p, fi.Mode().Perm())
		}
	}
	assertNoRestoreLeftovers(t, f.dataDir)

	// The pre-restore backup is kept, valid, and holds state B.
	if rep.RollbackBackupPath == "" {
		t.Fatal("no pre-restore backup recorded")
	}
	vr, err := Verify(context.Background(), rep.RollbackBackupPath, VerifyOptions{})
	if err != nil || vr.Status != StatusValid || vr.Kind != KindPreRestore {
		t.Fatalf("pre-restore backup: %+v %v", vr, err)
	}
	if ids := projectIDs(t, filepath.Join(rep.RollbackBackupPath, DatabaseAsset)); !slices.Contains(ids, "state-b") {
		t.Fatalf("the pre-restore backup does not hold state B: %v", ids)
	}

	// Rolling back by hand = restoring the pre-restore backup.
	f.backupA = &CreateResult{Path: rep.RollbackBackupPath}
	if _, err := f.restore(t, nil); err != nil {
		t.Fatalf("manual rollback: %v", err)
	}
	if got := semanticState(t, f.dataDir); got != f.stateB {
		t.Fatalf("manual rollback did not return state B:\n got: %s\nwant: %s", got, f.stateB)
	}
}

// §52/§I — a destination whose WAL/SHM belong to state B ends with none, and
// the database shows only A.
func TestRestoreLeavesNoStaleWALOrSHM(t *testing.T) {
	f := newFixture(t)
	// Build a destination whose uncheckpointed WAL holds a B-only row, as a
	// crashed daemon would leave it: copy the files while a writer holds them.
	dest := filepath.Join(f.parent, "dest")
	writer := openRW(t, f.dataDir)
	if _, err := writer.Exec(`PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(addProject("state-b-in-wal")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{DatabaseAsset, DatabaseAsset + "-wal", DatabaseAsset + "-shm", IdentityAsset, secretKeyFile} {
		b, err := os.ReadFile(filepath.Join(f.dataDir, name))
		if err != nil {
			t.Fatalf("copy %s: %v", name, err)
		}
		writeFile(t, filepath.Join(dest, name), string(b), 0o600)
	}
	_ = writer.Close()
	if fi, err := os.Stat(filepath.Join(dest, DatabaseAsset+"-wal")); err != nil || fi.Size() == 0 {
		t.Fatal("precondition: the destination must carry a non-empty stale WAL")
	}

	rep, err := Restore(context.Background(), RestoreOptions{DataDir: dest, Source: f.backupA.Path, Root: filepath.Join(f.parent, "dest-backups"),
		CheckDaemon: noDaemon, Tool: ToolInfo{Name: "ao-test"}})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	for _, s := range sqliteSidecars {
		if _, err := os.Lstat(filepath.Join(dest, s)); err == nil {
			t.Fatalf("stale %s survived the restore", s)
		}
	}
	// The main file alone holds exactly A; opened normally, still exactly A.
	if ids := projectIDs(t, filepath.Join(dest, DatabaseAsset)); !slices.Equal(ids, []string{"state-a"}) {
		t.Fatalf("restored main file holds %v", ids)
	}
	if ids := liveProjectIDs(t, dest); !slices.Equal(ids, []string{"state-a"}) {
		t.Fatalf("restored database opened normally holds %v: a stale WAL attached", ids)
	}
	if !hasWarning(rep, CodeDataDirDiffers) {
		t.Error("restoring into another data dir must warn about stored paths")
	}
	// The destination's WAL-only row was preserved by the pre-restore backup.
	if ids := projectIDs(t, filepath.Join(rep.RollbackBackupPath, DatabaseAsset)); !slices.Contains(ids, "state-b-in-wal") {
		t.Fatalf("the pre-restore backup lost the destination's WAL-only row: %v", ids)
	}
}

func hasWarning(rep *RestoreReport, code Code) bool {
	for _, w := range rep.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

// §14/§53/§54 at the library boundary — a failed daemon check refuses before
// anything is written; no pre-restore backup, no journal.
func TestRestoreRefusesWhenAODaemonIsNotProvenStopped(t *testing.T) {
	for _, code := range []Code{CodeDaemonActive, CodeDaemonUnverified, CodeDaemonAmbiguous} {
		t.Run(string(code), func(t *testing.T) {
			f := newFixture(t)
			before := managedDigest(t, f.dataDir)
			rep, err := f.restore(t, func(o *RestoreOptions) {
				o.CheckDaemon = func(context.Context) error { return refusedf(code, "test daemon") }
			})
			if codeOf(err) != code || classOf(err) != ClassRefused || rep.Result != ResultRefused || rep.DestinationTouched {
				t.Fatalf("err=%v rep=%+v", err, rep)
			}
			if managedDigest(t, f.dataDir) != before {
				t.Fatal("a refused restore changed the data dir")
			}
			if countBackups(t, f.root, KindPreRestore) != 0 {
				t.Fatal("a refused restore took a pre-restore backup")
			}
			assertNoRestoreLeftovers(t, f.dataDir)
		})
	}
	t.Run("no check at all", func(t *testing.T) {
		f := newFixture(t)
		_, err := f.restore(t, func(o *RestoreOptions) { o.CheckDaemon = nil })
		if codeOf(err) != CodeInvalidArgument {
			t.Fatalf("err=%v", err)
		}
	})
}

// §15/§F — the P9 data-dir lock and SQLite's own view both refuse.
func TestRestoreRefusesWhenTheDataDirOrDatabaseIsHeld(t *testing.T) {
	t.Run("daemon.lock held", func(t *testing.T) {
		f := newFixture(t)
		lock, err := daemonlock.Acquire(filepath.Join(f.dataDir, daemonLockFile))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lock.Release() }()
		before := managedDigest(t, f.dataDir)
		_, err = f.restore(t, nil)
		if codeOf(err) != CodeDataDirLocked {
			t.Fatalf("err=%v", err)
		}
		if managedDigest(t, f.dataDir) != before {
			t.Fatal("data dir changed")
		}
		assertNoRestoreLeftovers(t, f.dataDir)
	})
	t.Run("another connection has the database open", func(t *testing.T) {
		f := newFixture(t)
		holder := openRW(t, f.dataDir)
		defer func() { _ = holder.Close() }()
		if err := holder.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(new(int)); err != nil {
			t.Fatal(err)
		}
		before := stripSidecars(managedDigest(t, f.dataDir))
		_, err := f.restore(t, nil)
		if codeOf(err) != CodeDBInUse {
			t.Fatalf("err=%v", err)
		}
		if stripSidecars(managedDigest(t, f.dataDir)) != before {
			t.Fatal("data dir changed")
		}
		assertNoRestoreLeftovers(t, f.dataDir)
	})
}

// §19 — identity policy, end to end.
func TestRestoreInstallationIdentityPolicy(t *testing.T) {
	other := "aoi-" + uuid.NewString()
	setup := func(t *testing.T) (*fixture, string) {
		f := newFixture(t)
		backupID := f.backupA.Manifest.Source.InstallationID
		writeFile(t, filepath.Join(f.dataDir, IdentityAsset), other+"\n", 0o600)
		return f, backupID
	}
	t.Run("strict refuses a mismatch", func(t *testing.T) {
		f, _ := setup(t)
		before := managedDigest(t, f.dataDir)
		_, err := f.restore(t, nil)
		if codeOf(err) != CodeInstallationMismatch {
			t.Fatalf("err=%v", err)
		}
		if managedDigest(t, f.dataDir) != stripSidecarsKeep(before, managedDigest(t, f.dataDir)) {
			t.Fatal("data dir changed")
		}
	})
	t.Run("backup identity adopted", func(t *testing.T) {
		f, backupID := setup(t)
		rep, err := f.restore(t, func(o *RestoreOptions) { o.Identity = IdentityFromBackup })
		if err != nil || rep.IdentityAction != "replaced_with_backup" {
			t.Fatalf("err=%v rep=%+v", err, rep)
		}
		if got, _ := readIdentity(f.dataDir); got != backupID {
			t.Fatalf("identity %s, want the backup's %s", got, backupID)
		}
	})
	t.Run("destination identity kept", func(t *testing.T) {
		f, _ := setup(t)
		rep, err := f.restore(t, func(o *RestoreOptions) { o.Identity = IdentityKeepDestination })
		if err != nil || rep.IdentityAction != "kept_destination" {
			t.Fatalf("err=%v rep=%+v", err, rep)
		}
		if got, _ := readIdentity(f.dataDir); got != other {
			t.Fatalf("identity %s, want the destination's %s", got, other)
		}
		if got := semanticState(t, f.dataDir); got != f.stateA {
			t.Fatal("the database was not restored")
		}
	})
	t.Run("fresh destination receives the backup identity", func(t *testing.T) {
		f, backupID := setup(t)
		if err := os.Remove(filepath.Join(f.dataDir, IdentityAsset)); err != nil {
			t.Fatal(err)
		}
		if _, err := f.restore(t, nil); err != nil {
			t.Fatal(err)
		}
		if got, _ := readIdentity(f.dataDir); got != backupID {
			t.Fatalf("identity %s, want %s", got, backupID)
		}
	})
}

// stripSidecarsKeep compares ignoring sidecars only when the probe created one.
func stripSidecarsKeep(before, after string) string {
	if stripSidecars(before) == stripSidecars(after) {
		return after
	}
	return before
}

// §19b — a database is never silently put next to a key that cannot open it.
func TestRestoreSecretKeyPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, dataDir string)
	}{
		{"different key", func(t *testing.T, d string) {
			writeFile(t, filepath.Join(d, secretKeyFile), "ZGlmZmVyZW50LWtleS1kaWZmZXJlbnQta2V5LWRpZmZlcmU=", 0o600)
		}},
		{"missing key", func(t *testing.T, d string) { _ = os.Remove(filepath.Join(d, secretKeyFile)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.change(t, f.dataDir)
			before := managedDigest(t, f.dataDir)
			_, err := f.restore(t, nil)
			if codeOf(err) != CodeSecretKeyMismatch {
				t.Fatalf("err=%v", err)
			}
			if stripSidecars(managedDigest(t, f.dataDir)) != stripSidecars(before) {
				t.Fatal("data dir changed")
			}
			rep, err := f.restore(t, func(o *RestoreOptions) { o.AllowSecretKeyMismatch = true })
			if err != nil || !hasWarning(rep, CodeSecretKeyMismatch) {
				t.Fatalf("explicit override: err=%v rep=%+v", err, rep)
			}
		})
	}
}

// §40/§73 — adversarial paths.
func TestRestoreRefusesUnsafePaths(t *testing.T) {
	t.Run("source is a symlink", func(t *testing.T) {
		f := newFixture(t)
		link := filepath.Join(f.parent, "link")
		if err := os.Symlink(f.backupA.Path, link); err != nil {
			t.Fatal(err)
		}
		_, err := f.restore(t, func(o *RestoreOptions) { o.Source = link })
		if codeOf(err) != CodeUnsafePath {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("destination entry is a symlink", func(t *testing.T) {
		f := newFixture(t)
		outside := filepath.Join(f.parent, "outside-id")
		writeFile(t, outside, "aoi-"+uuid.NewString()+"\n", 0o600)
		_ = os.Remove(filepath.Join(f.dataDir, IdentityAsset))
		if err := os.Symlink(outside, filepath.Join(f.dataDir, IdentityAsset)); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(outside)
		_, err := f.restore(t, nil)
		if codeOf(err) != CodeUnsafePath {
			t.Fatalf("err=%v", err)
		}
		if after, _ := os.ReadFile(outside); string(after) != string(before) {
			t.Fatal("the restore wrote through a symlink")
		}
	})
	t.Run("source inside the destination", func(t *testing.T) {
		f := newFixture(t)
		inside := filepath.Join(f.dataDir, "old-backups", filepath.Base(f.backupA.Path))
		copyTree(t, f.backupA.Path, inside)
		_, err := f.restore(t, func(o *RestoreOptions) { o.Source = inside })
		if codeOf(err) != CodeSourceInsideDestination {
			t.Fatalf("err=%v", err)
		}
		if _, err := os.Stat(filepath.Join(inside, DatabaseAsset)); err != nil {
			t.Fatal("the source inside the destination was damaged")
		}
	})
	t.Run("source and destination are the same path", func(t *testing.T) {
		f := newFixture(t)
		_, err := f.restore(t, func(o *RestoreOptions) { o.Source = f.dataDir })
		if codeOf(err) != CodeSourceInsideDestination {
			t.Fatalf("err=%v", err)
		}
	})
}

// §56 — an older schema restores as-is; migrating is the next start's job.
func TestRestoreOlderSchemaIsNotMigrated(t *testing.T) {
	parent := t.TempDir()
	dataDir := newInstallation(t, parent)
	head := testHead(t)
	mutate(t, dataDir, addProject("old"), `DELETE FROM goose_db_version WHERE version_id = `+itoa(head))
	res := mustCreate(t, dataDir, filepath.Join(parent, "b"))
	vr, err := Verify(context.Background(), res.Path, VerifyOptions{})
	if err != nil || vr.Status != StatusValid || vr.Compatibility != CompatUpgradeRequired || !vr.Restorable() {
		t.Fatalf("verify: %+v %v", vr, err)
	}
	mutate(t, dataDir, addProject("newer"))
	rep, err := Restore(context.Background(), RestoreOptions{DataDir: dataDir, Source: res.Path, Root: filepath.Join(parent, "b"), CheckDaemon: noDaemon, Tool: ToolInfo{Name: "ao-test"}})
	if err != nil || !hasWarning(rep, CodeUpgradeRequired) {
		t.Fatalf("restore: err=%v rep=%+v", err, rep)
	}
	facts, err := inspectDatabase(context.Background(), filepath.Join(dataDir, DatabaseAsset), true)
	if err != nil || facts.GooseVersion != head-1 {
		t.Fatalf("restored goose %d (err %v), want %d: the restore must not migrate", facts.GooseVersion, err, head-1)
	}
}

// §57 — a newer schema is intact but never restored by this binary.
func TestRestoreNewerSchemaFailsClosed(t *testing.T) {
	f := newFixture(t)
	head := testHead(t)
	mutate(t, f.dataDir, `INSERT INTO goose_db_version (version_id, is_applied) VALUES (`+itoa(head+1)+`, 1)`)
	res := mustCreate(t, f.dataDir, f.root)
	vr, err := Verify(context.Background(), res.Path, VerifyOptions{})
	if err != nil || vr.Status != StatusValid || vr.Compatibility != CompatNewerThanBinary || vr.Restorable() || !hasReason(vr, CodeNewerThanBinary) {
		t.Fatalf("verify: %+v %v", vr, err)
	}
	mutate(t, f.dataDir, addProject("after"))
	before := managedDigest(t, f.dataDir)
	_, err = f.restore(t, func(o *RestoreOptions) { o.Source = res.Path })
	if codeOf(err) != CodeNewerThanBinary || classOf(err) != ClassIncompatible {
		t.Fatalf("err=%v", err)
	}
	if managedDigest(t, f.dataDir) != before {
		t.Fatal("data dir changed")
	}
}

// §26 — failures before the swap never touch the destination.
func TestRestoreFailuresBeforeTheSwapLeaveTheDestinationUntouched(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hooks func(f *fixture) *testHooks
		code  Code
	}{
		{"pre-restore backup fails", func(*fixture) *testHooks {
			return &testHooks{createRollback: func(context.Context, CreateOptions) (*CreateResult, error) {
				return nil, errors.New("injected: disk full while backing up")
			}}
		}, CodeRollbackBackupFailed},
		{"staging write fails", func(*fixture) *testHooks {
			return &testHooks{beforeStagingCopy: func() error { return syscall.ENOSPC }}
		}, CodeStagingFailed},
		{"not enough space", func(*fixture) *testHooks {
			return &testHooks{freeSpace: func(string) (uint64, error) { return 1024, nil }}
		}, CodeInsufficientSpace},
		{"injected failure after staging", func(*fixture) *testHooks {
			return &testHooks{atPhase: func(p Phase) error {
				if p == PhaseStaged {
					return errors.New("injected")
				}
				return nil
			}}
		}, CodeStagingFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			before := stripSidecars(managedDigest(t, f.dataDir))
			rep, err := f.restore(t, func(o *RestoreOptions) { o.hooks = tc.hooks(f) })
			if !hasCode(err, tc.code) {
				t.Fatalf("err=%v, want %s", err, tc.code)
			}
			if rep.DestinationTouched || rep.Result == ResultRestored {
				t.Fatalf("report: %+v", rep)
			}
			if got := stripSidecars(managedDigest(t, f.dataDir)); got != before {
				t.Fatalf("destination changed:\n%s\n---\n%s", before, got)
			}
			if got := semanticState(t, f.dataDir); got != f.stateB {
				t.Fatal("destination state changed")
			}
			assertNoRestoreLeftovers(t, f.dataDir)
			if err := CheckStartup(f.dataDir); err != nil {
				t.Fatalf("the daemon would refuse to boot: %v", err)
			}
		})
	}
}

// §27/§55/§K — the swap happened, verification failed: the previous state is back.
func TestRestoreAutoRollbackAfterTheSwap(t *testing.T) {
	failIn := func(sub string) func(string, string) error {
		return func(oldpath, newpath string) error {
			if strings.Contains(oldpath, sub) {
				return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EXDEV}
			}
			return os.Rename(oldpath, newpath)
		}
	}
	sep := string(filepath.Separator)
	for _, tc := range []struct {
		name  string
		hooks *testHooks
		code  Code
	}{
		{"final verification fails", &testHooks{finalVerify: func() error { return errors.New("injected verify failure") }}, CodeRestoreVerifyFailed},
		{"promotion crosses a device", &testHooks{rename: failIn(sep + "staged" + sep)}, CodeCrossDevice},
		{"failure right after the swap", &testHooks{atPhase: func(p Phase) error {
			if p == PhaseSwapped {
				return errors.New("injected")
			}
			return nil
		}}, CodeRestoreVerifyFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			before := stripSidecars(managedDigest(t, f.dataDir))
			rep, err := f.restore(t, func(o *RestoreOptions) { o.hooks = tc.hooks })
			if classOf(err) != ClassRolledBack || rep.Result != ResultRolledBack || !hasCode(err, tc.code) {
				t.Fatalf("err=%v rep=%+v", err, rep)
			}
			if got := stripSidecars(managedDigest(t, f.dataDir)); got != before {
				t.Fatalf("rollback did not restore the previous files:\n%s\n---\n%s", before, got)
			}
			if got := semanticState(t, f.dataDir); got != f.stateB {
				t.Fatalf("rollback did not restore the previous state")
			}
			assertNoRestoreLeftovers(t, f.dataDir)
			if err := CheckStartup(f.dataDir); err != nil {
				t.Fatalf("boot refused after a clean rollback: %v", err)
			}
		})
	}
}

// §27 — when the rollback fails too, it says so, the daemon refuses to boot,
// and recover puts everything back.
func TestRestoreRollbackFailureIsExplicitAndRecoverable(t *testing.T) {
	f := newFixture(t)
	before := stripSidecars(managedDigest(t, f.dataDir))
	sep := string(filepath.Separator)
	hooks := &testHooks{
		finalVerify: func() error { return errors.New("injected verify failure") },
		rename: func(oldpath, newpath string) error {
			if strings.Contains(oldpath, sep+"previous"+sep) {
				return errors.New("injected: cannot move the previous state back")
			}
			return os.Rename(oldpath, newpath)
		},
	}
	rep, err := f.restore(t, func(o *RestoreOptions) { o.hooks = hooks })
	if classOf(err) != ClassRollbackFailed || rep.Result != ResultRollbackFailed {
		t.Fatalf("err=%v rep=%+v", err, rep)
	}
	if !strings.Contains(err.Error(), "ao backup recover") || !strings.Contains(err.Error(), rep.RollbackBackupPath) {
		t.Fatalf("the failure does not tell the operator what to do: %v", err)
	}
	j, jerr := readJournal(f.dataDir)
	if jerr != nil || j == nil || j.Phase != PhaseRollbackFailed {
		t.Fatalf("journal: %+v %v", j, jerr)
	}
	if err := CheckStartup(f.dataDir); err == nil {
		t.Fatal("the daemon would boot over a failed rollback")
	}
	if _, err := f.restore(t, nil); codeOf(err) != CodeRestoreInterrupted {
		t.Fatalf("a new restore over an unresolved one: %v", err)
	}

	rr, err := Recover(context.Background(), RecoverOptions{DataDir: f.dataDir, CheckDaemon: noDaemon})
	if err != nil || rr.Result != RecoverRolledBack {
		t.Fatalf("recover: %+v %v", rr, err)
	}
	if got := stripSidecars(managedDigest(t, f.dataDir)); got != before {
		t.Fatalf("recover did not restore the previous files:\n%s\n---\n%s", before, got)
	}
	if got := semanticState(t, f.dataDir); got != f.stateB {
		t.Fatal("recover did not restore the previous state")
	}
	assertNoRestoreLeftovers(t, f.dataDir)
	if err := CheckStartup(f.dataDir); err != nil {
		t.Fatalf("boot still refused: %v", err)
	}
	if _, err := f.restore(t, nil); err != nil {
		t.Fatalf("restore after recover: %v", err)
	}
}

// §38 — Ctrl+C before the swap aborts cleanly; once the swap began it is ignored.
func TestRestoreCancellation(t *testing.T) {
	for _, tc := range []struct {
		phase   Phase
		touched bool
	}{{PhaseRollbackReady, false}, {PhaseStaged, false}, {PhaseSwapping, true}} {
		t.Run(string(tc.phase), func(t *testing.T) {
			f := newFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rep, err := Restore(ctx, RestoreOptions{DataDir: f.dataDir, Source: f.backupA.Path, Root: f.root, CheckDaemon: noDaemon,
				Tool: ToolInfo{Name: "ao-test"}, hooks: &testHooks{atPhase: func(p Phase) error {
					if p == tc.phase {
						cancel()
					}
					return nil
				}}})
			if !tc.touched {
				if codeOf(err) != CodeCanceled || rep.DestinationTouched {
					t.Fatalf("err=%v rep=%+v", err, rep)
				}
				if got := semanticState(t, f.dataDir); got != f.stateB {
					t.Fatal("a canceled restore changed the data dir")
				}
			} else {
				if err != nil || rep.Result != ResultRestored {
					t.Fatalf("a cancel inside the critical section interrupted it: err=%v rep=%+v", err, rep)
				}
				if got := semanticState(t, f.dataDir); got != f.stateA {
					t.Fatal("the critical section did not finish")
				}
			}
			assertNoRestoreLeftovers(t, f.dataDir)
		})
	}
}

func TestRestoreIntoAnEmptyDataDir(t *testing.T) {
	f := newFixture(t)
	dest := filepath.Join(f.parent, "fresh", "data")
	rep, err := Restore(context.Background(), RestoreOptions{DataDir: dest, Source: f.backupA.Path, Root: filepath.Join(f.parent, "fresh", "backups"),
		CheckDaemon: noDaemon, Tool: ToolInfo{Name: "ao-test"}, AllowSecretKeyMismatch: true})
	if err != nil || rep.RollbackBackupPath != "" {
		t.Fatalf("err=%v rep=%+v", err, rep)
	}
	if got := semanticState(t, dest); got != f.stateA {
		t.Fatal("fresh restore does not hold state A")
	}
	if got, _ := readIdentity(dest); got != f.backupA.Manifest.Source.InstallationID {
		t.Fatalf("identity %q", got)
	}
}

// A restore journal that is not resolved blocks the next restore, whatever its phase.
func TestRestoreRefusesOverAnUnresolvedJournal(t *testing.T) {
	f := newFixture(t)
	id, _ := newID("aor-", f.backupA.Manifest.CreatedAt)
	if err := writeJournal(f.dataDir, &journal{RestoreID: id, WorkDir: restoreWorkPrefix + id, Phase: PhaseStaged}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.restore(t, nil); codeOf(err) != CodeRestoreInterrupted {
		t.Fatalf("err=%v", err)
	}
	if err := CheckStartup(f.dataDir); err != nil {
		t.Fatalf("a pre-swap journal must not block boot: %v", err)
	}
}

// A crafted journal can never make recovery move anything outside the managed entries.
func TestJournalRejectsPathsOutsideTheManagedEntries(t *testing.T) {
	id, _ := newID("aor-", testTime())
	for name, j := range map[string]*journal{
		"traversal work dir": {Format: journalFormat, RestoreID: id, WorkDir: "../../elsewhere", Phase: PhaseSwapping},
		"foreign promote":    {Format: journalFormat, RestoreID: id, WorkDir: restoreWorkPrefix + id, Phase: PhaseSwapping, Promote: []string{"secret.key"}},
		"foreign entry":      {Format: journalFormat, RestoreID: id, WorkDir: restoreWorkPrefix + id, Phase: PhaseSwapping, PreExisting: []string{"../x"}},
		"unknown phase":      {Format: journalFormat, RestoreID: id, WorkDir: restoreWorkPrefix + id, Phase: "done-ish"},
	} {
		if err := j.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDecideIdentity(t *testing.T) {
	const b, d = "aoi-11111111-1111-4111-8111-111111111111", "aoi-22222222-2222-4222-8222-222222222222"
	for _, tc := range []struct {
		backup, dest string
		policy       IdentityPolicy
		promote      bool
		refused      bool
	}{
		{"", d, IdentityStrict, false, false},
		{b, "", IdentityStrict, true, false},
		{b, b, IdentityStrict, true, false},
		{b, d, IdentityStrict, false, true},
		{b, d, IdentityFromBackup, true, false},
		{b, d, IdentityKeepDestination, false, false},
	} {
		promote, _, err := decideIdentity(tc.backup, tc.dest, tc.policy)
		if (err != nil) != tc.refused || promote != tc.promote {
			t.Errorf("%+v: promote=%t err=%v", tc, promote, err)
		}
		if tc.refused && codeOf(err) != CodeInstallationMismatch {
			t.Errorf("%+v: code=%s", tc, codeOf(err))
		}
	}
}
