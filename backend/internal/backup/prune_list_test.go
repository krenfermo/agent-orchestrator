package backup

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/daemonlock"
)

// §28/§58 — retention deletes only the right candidates and never: protected
// kinds, the newest backup, a backup in use, an unreadable one, or anything
// that is not an AO backup.
func TestPruneDeletesOnlyCandidates(t *testing.T) {
	parent := t.TempDir()
	dataDir := newInstallation(t, parent)
	root := filepath.Join(parent, "backups")

	var manual []*CreateResult
	for i := 0; i < 5; i++ {
		manual = append(manual, mustCreate(t, dataDir, root))
		if i == 1 {
			if _, err := Create(context.Background(), CreateOptions{DataDir: dataDir, Root: root, Kind: KindPreRestore, Tool: ToolInfo{Name: "ao-test"}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// manual[0] is the oldest. Hold manual[1]'s lock: a restore is reading it.
	inUse, err := daemonlock.Acquire(lockPath(root, manual[1].BackupID))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inUse.Release() }()

	// Things that are not candidates.
	legacy := filepath.Join(root, "pre-0170-20260912-213244")
	writeFile(t, filepath.Join(legacy, "ao.db"), "hand-made copy", 0o644)
	writeFile(t, filepath.Join(root, "ao.db.backup-before-x"), "hand-made copy", 0o644)
	legacyBefore := treeDigest(t, legacy)

	orphanID, _ := NewBackupID(time.Now().Add(-time.Hour))
	writeFile(t, filepath.Join(root, stagingPrefix+orphanID, DatabaseAsset), "partial", 0o600)
	runningID, _ := NewBackupID(time.Now())
	writeFile(t, filepath.Join(root, stagingPrefix+runningID, DatabaseAsset), "partial", 0o600)
	running, err := daemonlock.Acquire(lockPath(root, runningID))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = running.Release() }()
	deletingID, _ := NewBackupID(time.Now().Add(-2 * time.Hour))
	writeFile(t, filepath.Join(root, deletingPrefix+deletingID, DatabaseAsset), "half deleted", 0o600)
	badID, _ := NewBackupID(time.Now().Add(-3 * time.Hour))
	writeFile(t, filepath.Join(root, badID, ManifestName), "{", 0o600)

	// List reports every AO entry with its state and ignores the rest.
	lrep, err := List(dataDir, root)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, e := range lrep.Entries {
		states[e.Name] = e.State
	}
	for name, want := range map[string]string{
		stagingPrefix + orphanID:    StateIncomplete,
		stagingPrefix + runningID:   StateInProgress,
		deletingPrefix + deletingID: StateDeleting,
		badID:                       StateInvalidManifest,
		manual[4].BackupID:          StateOK,
	} {
		if states[name] != want {
			t.Errorf("list: %s state %q, want %q", name, states[name], want)
		}
	}
	if _, listed := states["pre-0170-20260912-213244"]; listed {
		t.Error("list reported a hand-made copy as an AO backup")
	}
	if lrep.LastBackup == nil {
		t.Error("list lost the last backup record")
	}

	// Dry run: decisions only.
	rootBefore := treeDigestExcept(t, root, locksDir)
	dry, err := Prune(context.Background(), PruneOptions{Root: root, KeepLast: 2})
	if err != nil || !dry.DryRun {
		t.Fatalf("dry run: %+v %v", dry, err)
	}
	if treeDigestExcept(t, root, locksDir) != rootBefore {
		t.Fatal("a dry run changed the backup root")
	}

	rep, err := Prune(context.Background(), PruneOptions{Root: root, KeepLast: 2, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	exists := func(name string) bool {
		_, err := os.Lstat(filepath.Join(root, name))
		return err == nil
	}
	for _, keep := range []string{manual[4].BackupID, manual[3].BackupID, manual[1].BackupID, badID, stagingPrefix + runningID, "ao.db.backup-before-x"} {
		if !exists(keep) {
			t.Errorf("%s was deleted", keep)
		}
	}
	for _, gone := range []string{manual[2].BackupID, manual[0].BackupID, stagingPrefix + orphanID, deletingPrefix + deletingID} {
		if exists(gone) {
			t.Errorf("%s was not removed", gone)
		}
	}
	if countBackups(t, root, KindPreRestore) != 1 {
		t.Error("the pre-restore backup was pruned")
	}
	if treeDigest(t, legacy) != legacyBefore {
		t.Error("prune touched a hand-made copy")
	}
	if rep.Deleted != 2 || rep.Errors != 0 {
		t.Errorf("report: deleted=%d errors=%d", rep.Deleted, rep.Errors)
	}
	for _, keep := range []*CreateResult{manual[4], manual[3], manual[1]} {
		if vr, err := Verify(context.Background(), keep.Path, VerifyOptions{Quick: true}); err != nil || vr.Status != StatusValid {
			t.Errorf("surviving backup %s damaged: %+v %v", keep.BackupID, vr, err)
		}
	}
}

func treeDigestExcept(t *testing.T, dir, skip string) string {
	t.Helper()
	var keep []string
	for _, line := range strings.Split(treeDigest(t, dir), "\n") {
		if strings.Contains(line, " "+skip) {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}

// The newest backup survives any policy; --max-age keeps young backups.
func TestPruneNewestAndMaxAge(t *testing.T) {
	parent := t.TempDir()
	dataDir := newInstallation(t, parent)
	root := filepath.Join(parent, "backups")
	for i := 0; i < 3; i++ {
		mustCreate(t, dataDir, root)
	}
	rep, err := Prune(context.Background(), PruneOptions{Root: root, KeepLast: 1, MaxAge: time.Hour, Apply: true})
	if err != nil || rep.Deleted != 0 {
		t.Fatalf("young backups pruned despite --max-age: %+v %v", rep, err)
	}
	future := func() time.Time { return time.Now().Add(48 * time.Hour) }
	rep, err = Prune(context.Background(), PruneOptions{Root: root, KeepLast: 1, MaxAge: time.Hour, Apply: true, Now: future})
	if err != nil || rep.Deleted != 2 {
		t.Fatalf("old backups not pruned: %+v %v", rep, err)
	}
	if countBackups(t, root, KindManual) != 1 {
		t.Fatal("the newest backup did not survive")
	}
	if _, err := Prune(context.Background(), PruneOptions{Root: root, KeepLast: -1}); codeOf(err) != CodeInvalidArgument {
		t.Fatalf("keep -1: %v", err)
	}
}

// §65 — a deletion that fails half-way leaves the other backups intact and a
// .deleting-* leftover (never a partial backup under its real name), which the
// next prune removes.
func TestPruneFailureLeavesOtherBackupsIntact(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions enforced for this user")
	}
	parent := t.TempDir()
	dataDir := newInstallation(t, parent)
	root := filepath.Join(parent, "backups")
	old := mustCreate(t, dataDir, root)
	keep := []*CreateResult{mustCreate(t, dataDir, root), mustCreate(t, dataDir, root)}
	// An undeletable inner directory makes RemoveAll fail after the rename.
	inner := filepath.Join(old.Path, "skills", "catalog")
	if err := os.Chmod(inner, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, deletingPrefix+old.BackupID, "skills", "catalog"), 0o700) })

	rep, err := Prune(context.Background(), PruneOptions{Root: root, KeepLast: 2, Apply: true})
	if err != nil || rep.Errors != 1 {
		t.Fatalf("report %+v err %v", rep, err)
	}
	if _, err := os.Lstat(old.Path); err == nil {
		t.Fatal("a half-deleted backup is still under its valid name")
	}
	for _, k := range keep {
		if vr, err := Verify(context.Background(), k.Path, VerifyOptions{Quick: true}); err != nil || vr.Status != StatusValid {
			t.Fatalf("backup %s damaged by another's failed deletion", k.BackupID)
		}
	}
	_ = os.Chmod(filepath.Join(root, deletingPrefix+old.BackupID, "skills", "catalog"), 0o700)
	rep, err = Prune(context.Background(), PruneOptions{Root: root, KeepLast: 2, Apply: true})
	if err != nil || rep.Errors != 0 {
		t.Fatalf("second prune: %+v %v", rep, err)
	}
	if _, err := os.Lstat(filepath.Join(root, deletingPrefix+old.BackupID)); err == nil {
		t.Fatal("the interrupted deletion was not finished")
	}
}
