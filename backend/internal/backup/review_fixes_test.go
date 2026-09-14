package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/daemonlock"
)

// Findings of the P10 adversarial review, each pinned by a test.

// F1, as the independent review requires — a verified restore whose completion
// cannot be written to the journal is never reported as RESTORED: a journal
// still reading "swapped" is one recover would undo, so the restore rolls back
// and can be re-run.
func TestRestoreCompletionThatCannotBeJournaledIsNeverReportedRestored(t *testing.T) {
	f := newFixture(t)
	rep, err := f.restore(t, func(o *RestoreOptions) {
		o.hooks = &testHooks{journalWrite: func(p Phase, write func() error) error {
			if p == PhaseComplete {
				return errors.New("injected: cannot write the journal")
			}
			return write()
		}}
	})
	if classOf(err) != ClassRolledBack || rep.Result != ResultRolledBack || rep.RecoverRequired {
		t.Fatalf("err=%v rep=%+v", err, rep)
	}
	if got := semanticState(t, f.dataDir); got != f.stateB {
		t.Fatal("the previous state is not back")
	}
	assertNoRestoreLeftovers(t, f.dataDir)
	if err := CheckStartup(f.dataDir); err != nil {
		t.Fatal(err)
	}
	rr, err := Recover(context.Background(), RecoverOptions{DataDir: f.dataDir, CheckDaemon: noDaemon})
	if err != nil || rr.Result != RecoverNothing {
		t.Fatalf("recover: %+v %v", rr, err)
	}
	if _, err := f.restore(t, nil); err != nil || semanticState(t, f.dataDir) != f.stateA {
		t.Fatalf("re-run: %v", err)
	}
}

// Independent review §4/§6 (HIGH): after the swap the journal can no longer be
// written or removed (reproduced with an immutable journal file). Before the
// fix the rollback deleted its work dir anyway and printed "nothing else needs
// to be done", leaving a "swapped" journal that blocked every boot and a
// recover that could only fail. Now the work dir stays, the report demands
// recover, and recover settles it -- idempotently.
func TestRestoreWhoseJournalCannotBeClearedStaysRecoverable(t *testing.T) {
	f := newFixture(t)
	before := stripSidecars(managedDigest(t, f.dataDir))
	broken := false
	rep, err := f.restore(t, func(o *RestoreOptions) {
		o.hooks = &testHooks{
			journalWrite: func(p Phase, write func() error) error {
				if p == PhaseSwapped {
					broken = true
				}
				if broken {
					return errors.New("injected: journal is immutable")
				}
				return write()
			},
			journalRemove: func(remove func() error) error {
				if broken {
					return errors.New("injected: journal is immutable")
				}
				return remove()
			},
		}
	})
	if classOf(err) != ClassRolledBack || rep.Result != ResultRolledBack || !rep.RecoverRequired {
		t.Fatalf("err=%v rep=%+v", err, rep)
	}
	if !strings.Contains(err.Error(), "ao backup recover") {
		t.Fatalf("the report does not demand recover: %v", err)
	}
	if got := semanticState(t, f.dataDir); got != f.stateB {
		t.Fatal("the previous state is not back")
	}
	j, jerr := readJournal(f.dataDir)
	if jerr != nil || j == nil {
		t.Fatalf("journal: %v", jerr)
	}
	if fi, err := os.Lstat(filepath.Join(f.dataDir, j.WorkDir)); err != nil || !fi.IsDir() {
		t.Fatal("the work dir recover needs was deleted")
	}
	if CheckStartup(f.dataDir) == nil {
		t.Fatal("the boot gate accepts a data dir whose journal still reads unsettled")
	}
	for i := 0; i < 3; i++ {
		rr, err := Recover(context.Background(), RecoverOptions{DataDir: f.dataDir, CheckDaemon: noDaemon})
		if err != nil || (i == 0 && rr.Result != RecoverRolledBack) || (i > 0 && rr.Result != RecoverNothing) {
			t.Fatalf("recover #%d: %+v %v", i+1, rr, err)
		}
		if stripSidecars(managedDigest(t, f.dataDir)) != before {
			t.Fatalf("recover #%d changed the previous files", i+1)
		}
	}
	assertNoRestoreLeftovers(t, f.dataDir)
	if err := CheckStartup(f.dataDir); err != nil {
		t.Fatal(err)
	}
}

func removeDatabase(t *testing.T, dataDir string) {
	t.Helper()
	for _, e := range append([]string{DatabaseAsset}, sqliteSidecars...) {
		if err := os.Remove(filepath.Join(dataDir, e)); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
}

// F2 — with no database there is no pre-restore backup, so a restore must not
// replace anything that exists only in that data dir.
func TestRestoreRefusesToReplaceWhatADatabaselessDataDirAloneHolds(t *testing.T) {
	t.Run("a non-empty skill catalog", func(t *testing.T) {
		f := newFixture(t)
		removeDatabase(t, f.dataDir)
		catalog := filepath.Join(f.dataDir, "skills", "catalog")
		before := treeDigest(t, catalog)
		_, err := f.restore(t, nil)
		if codeOf(err) != CodeNoRollbackPossible || classOf(err) != ClassRefused {
			t.Fatalf("err=%v", err)
		}
		if treeDigest(t, catalog) != before {
			t.Fatal("the only copy of the skill catalog was touched")
		}
		assertNoRestoreLeftovers(t, f.dataDir)
	})
	t.Run("an identity the backup would overwrite", func(t *testing.T) {
		f := newFixture(t)
		removeDatabase(t, f.dataDir)
		if err := os.RemoveAll(filepath.Join(f.dataDir, "skills", "catalog")); err != nil {
			t.Fatal(err)
		}
		other := "aoi-" + uuid.NewString()
		writeFile(t, filepath.Join(f.dataDir, IdentityAsset), other+"\n", 0o600)
		_, err := f.restore(t, func(o *RestoreOptions) { o.Identity = IdentityFromBackup })
		if codeOf(err) != CodeNoRollbackPossible {
			t.Fatalf("err=%v", err)
		}
		if got, _ := readIdentity(f.dataDir); got != other {
			t.Fatalf("the only copy of the identity was replaced: %s", got)
		}
	})
	t.Run("nothing unbacked still restores", func(t *testing.T) {
		f := newFixture(t)
		removeDatabase(t, f.dataDir)
		if err := os.RemoveAll(filepath.Join(f.dataDir, "skills", "catalog")); err != nil {
			t.Fatal(err)
		}
		rep, err := f.restore(t, nil)
		if err != nil || rep.Result != ResultRestored {
			t.Fatalf("err=%v rep=%+v", err, rep)
		}
		if got := semanticState(t, f.dataDir); got != f.stateA {
			t.Fatal("not restored")
		}
	})
}

// F5 — the source backup is locked in its OWN root, whatever --root says.
func TestRestoreLocksTheSourceInItsOwnRoot(t *testing.T) {
	f := newFixture(t)
	held, err := daemonlock.Acquire(lockPath(f.root, f.backupA.BackupID))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()
	before := managedDigest(t, f.dataDir)
	_, err = f.restore(t, func(o *RestoreOptions) { o.Root = filepath.Join(f.parent, "another-root") })
	if codeOf(err) != CodeBackupInUse {
		t.Fatalf("err=%v", err)
	}
	if managedDigest(t, f.dataDir) != before {
		t.Fatal("data dir changed")
	}
}

// F5 — holders never remove lock files (so no two processes can hold one
// backup), and prune sweeps the lock files of backups that no longer exist.
func TestLockFilesStayWithHoldersAndPruneSweepsOrphans(t *testing.T) {
	parent := t.TempDir()
	dataDir := newInstallation(t, parent)
	root := filepath.Join(parent, "backups")
	res := mustCreate(t, dataDir, root)
	if _, err := os.Stat(lockPath(root, res.BackupID)); err != nil {
		t.Fatalf("create removed its lock file: %v", err)
	}
	orphan, _ := NewBackupID(time.Now().Add(-time.Hour))
	writeFile(t, lockPath(root, orphan), "", 0o600)
	running, _ := NewBackupID(time.Now())
	writeFile(t, filepath.Join(root, stagingPrefix+running, DatabaseAsset), "partial", 0o600)
	held, err := daemonlock.Acquire(lockPath(root, running))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()

	rep, err := Prune(context.Background(), PruneOptions{Root: root, Apply: true})
	if err != nil || rep.LocksSwept != 1 {
		t.Fatalf("report %+v err %v", rep, err)
	}
	if _, err := os.Stat(lockPath(root, orphan)); err == nil {
		t.Fatal("an orphan lock file survived")
	}
	for _, id := range []string{res.BackupID, running} {
		if _, err := os.Stat(lockPath(root, id)); err != nil {
			t.Fatalf("the lock file of %s was removed", id)
		}
	}
}

// F6 — recover never moves a file through a symlink inside the work dir.
func TestRecoverRefusesToMoveFilesThroughASymlinkedWorkDir(t *testing.T) {
	f := newFixture(t)
	dbPath := filepath.Join(f.dataDir, DatabaseAsset)
	_, sumBefore, err := hashFile(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := newID("aor-", time.Now())
	work := filepath.Join(f.dataDir, restoreWorkPrefix+id)
	writeFile(t, filepath.Join(work, "staged", DatabaseAsset), "staged", 0o600)
	outside := filepath.Join(f.parent, "outside")
	writeFile(t, filepath.Join(outside, DatabaseAsset), "NOT AN AO DATABASE", 0o600)
	if err := os.Symlink(outside, filepath.Join(work, "previous")); err != nil {
		t.Fatal(err)
	}
	var pre []string
	for _, e := range managedEntries {
		if _, err := os.Lstat(filepath.Join(f.dataDir, filepath.FromSlash(e))); err == nil {
			pre = append(pre, e)
		}
	}
	fi, _ := os.Stat(dbPath)
	if err := writeJournal(f.dataDir, &journal{RestoreID: id, SourceBackupID: f.backupA.BackupID, WorkDir: restoreWorkPrefix + id,
		Phase: PhaseSwapping, Promote: []string{DatabaseAsset, SkillCatalogPath}, PreExisting: pre, PreDatabase: stampOf(fi)}); err != nil {
		t.Fatal(err)
	}

	rr, err := Recover(context.Background(), RecoverOptions{DataDir: f.dataDir, CheckDaemon: noDaemon})
	if classOf(err) != ClassRollbackFailed || rr.Result != RecoverFailed {
		t.Fatalf("recover through a symlinked work dir: %+v %v", rr, err)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, DatabaseAsset)); string(b) != "NOT AN AO DATABASE" {
		t.Fatal("recover moved a file out of the symlink target")
	}
	if _, sum, err := hashFile(context.Background(), dbPath); err != nil || sum != sumBefore {
		t.Fatalf("recover replaced ao.db through a symlink (err %v)", err)
	}
}
