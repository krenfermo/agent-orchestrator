package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// swapScene lays out a data dir with every managed entry (including all three
// SQLite sidecars) and a staged restore beside it.
func swapScene(t *testing.T) (dataDir, workDir string, promote []string, j *journal) {
	t.Helper()
	dataDir = t.TempDir()
	for name, content := range map[string]string{
		DatabaseAsset: "OLD-DB", DatabaseAsset + "-wal": "OLD-WAL", DatabaseAsset + "-shm": "OLD-SHM",
		DatabaseAsset + "-journal": "OLD-JOURNAL", IdentityAsset: "OLD-ID", "skills/catalog/old/SKILL.md": "old",
		"prompts/untouched.md": "keep",
	} {
		writeFile(t, filepath.Join(dataDir, filepath.FromSlash(name)), content, 0o600)
	}
	id, _ := newID("aor-", testTime())
	workDir = filepath.Join(dataDir, restoreWorkPrefix+id)
	staged := filepath.Join(workDir, "staged")
	for name, content := range map[string]string{
		DatabaseAsset: "NEW-DB", IdentityAsset: "NEW-ID", "skills/catalog/new/SKILL.md": "new",
	} {
		writeFile(t, filepath.Join(staged, filepath.FromSlash(name)), content, 0o600)
	}
	promote = []string{DatabaseAsset, IdentityAsset, SkillCatalogPath}
	j = &journal{Format: journalFormat, RestoreID: id, WorkDir: filepath.Base(workDir), Phase: PhaseSwapping, Promote: promote}
	for _, e := range managedEntries {
		if _, err := os.Lstat(filepath.Join(dataDir, filepath.FromSlash(e))); err == nil {
			j.PreExisting = append(j.PreExisting, e)
		}
	}
	return dataDir, workDir, promote, j
}

func dataDirView(t *testing.T, dataDir string) string {
	t.Helper()
	var b strings.Builder
	for _, name := range append(managedEntries, "prompts/untouched.md", "skills/catalog/old/SKILL.md", "skills/catalog/new/SKILL.md") {
		if name == SkillCatalogPath {
			continue
		}
		c, err := os.ReadFile(filepath.Join(dataDir, filepath.FromSlash(name)))
		if err != nil {
			b.WriteString(name + "=<absent>\n")
		} else {
			b.WriteString(name + "=" + string(c) + "\n")
		}
	}
	return b.String()
}

// §18/§I — every sidecar goes aside before promotion; rollback is exact and
// idempotent, and a sidecar the restored database grew goes to failed/.
func TestSwapAndRollbackAreExactAndIdempotent(t *testing.T) {
	dataDir, workDir, promote, j := swapScene(t)
	before := dataDirView(t, dataDir)
	if err := swapIn(dataDir, workDir, promote, nil); err != nil {
		t.Fatal(err)
	}
	after := dataDirView(t, dataDir)
	for _, want := range []string{"ao.db=NEW-DB", "ao.db-wal=<absent>", "ao.db-shm=<absent>", "ao.db-journal=<absent>",
		"installation_id=NEW-ID", "skills/catalog/new/SKILL.md=new", "skills/catalog/old/SKILL.md=<absent>", "prompts/untouched.md=keep"} {
		if !strings.Contains(after, want+"\n") {
			t.Fatalf("after swap, missing %q:\n%s", want, after)
		}
	}
	// The restored database is opened and grows its own WAL, then something fails.
	writeFile(t, filepath.Join(dataDir, DatabaseAsset+"-wal"), "NEW-WAL", 0o600)
	for i := 0; i < 2; i++ {
		if err := rollbackEntries(dataDir, workDir, promote, j.PreExisting, nil); err != nil {
			t.Fatalf("rollback %d: %v", i, err)
		}
		if got := dataDirView(t, dataDir); got != before {
			t.Fatalf("rollback %d not exact:\n%s\nwant:\n%s", i, got, before)
		}
	}
	if err := checkRolledBack(dataDir, j, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(workDir, "failed", DatabaseAsset+"-wal")); string(b) != "NEW-WAL" {
		t.Fatal("the restored database's WAL was not moved out of the way")
	}
}

// A crash can stop the swap after any single rename. From every such point,
// rollback returns exactly the pre-swap data dir.
func TestRollbackFromEveryPartialSwap(t *testing.T) {
	for stopAfter := 0; stopAfter <= 9; stopAfter++ {
		dataDir, workDir, promote, j := swapScene(t)
		before := dataDirView(t, dataDir)
		var n atomic.Int32
		errStop := errors.New("crash")
		h := &testHooks{rename: func(oldpath, newpath string) error {
			if int(n.Add(1)) > stopAfter {
				return errStop
			}
			return os.Rename(oldpath, newpath)
		}}
		err := swapIn(dataDir, workDir, promote, h)
		if stopAfter < 9 && !errors.Is(err, errStop) {
			t.Fatalf("stopAfter=%d: swap did not stop: %v", stopAfter, err)
		}
		if err := rollbackEntries(dataDir, workDir, promote, j.PreExisting, nil); err != nil {
			t.Fatalf("stopAfter=%d: rollback: %v", stopAfter, err)
		}
		if got := dataDirView(t, dataDir); got != before {
			t.Fatalf("stopAfter=%d: rollback not exact:\n%s\nwant:\n%s", stopAfter, got, before)
		}
		if err := checkRolledBack(dataDir, j, nil); err != nil {
			t.Fatalf("stopAfter=%d: %v", stopAfter, err)
		}
	}
}

// ---- Real crashes: the restore or create runs in a child process that dies
// with os.Exit at an exact point, holding locks the kernel then releases. ----

const crashExitCode = 137

// TestCrashHelperProcess is the child. It does nothing unless re-executed by a
// crash test.
func TestCrashHelperProcess(t *testing.T) {
	mode := os.Getenv("AO_P10_CRASH_MODE")
	if mode == "" {
		t.Skip("crash helper: only runs as a child of a crash test")
	}
	dataDir, root, source := os.Getenv("AO_P10_CRASH_DATA"), os.Getenv("AO_P10_CRASH_ROOT"), os.Getenv("AO_P10_CRASH_SRC")
	die := func() { os.Exit(crashExitCode) }
	sep := string(filepath.Separator)
	var n atomic.Int32
	h := &testHooks{}
	switch {
	case strings.HasPrefix(mode, "phase:"):
		want := Phase(strings.TrimPrefix(mode, "phase:"))
		h.atPhase = func(p Phase) error {
			if p == want {
				die()
			}
			return nil
		}
	case mode == "mid-aside":
		h.rename = func(oldpath, newpath string) error {
			if strings.Contains(newpath, sep+"previous"+sep) && n.Add(1) == 2 {
				die()
			}
			return os.Rename(oldpath, newpath)
		}
	case mode == "mid-promote":
		h.rename = func(oldpath, newpath string) error {
			if strings.Contains(oldpath, sep+"staged"+sep) && n.Add(1) == 2 {
				die()
			}
			return os.Rename(oldpath, newpath)
		}
	case strings.HasPrefix(mode, "after-rename:"):
		// Dies right AFTER the n-th rename of the swap itself.
		want, _ := strconv.Atoi(strings.TrimPrefix(mode, "after-rename:"))
		h.rename = func(oldpath, newpath string) error {
			err := os.Rename(oldpath, newpath)
			if err == nil && (strings.Contains(oldpath, restoreWorkPrefix) || strings.Contains(newpath, restoreWorkPrefix)) && int(n.Add(1)) == want {
				die()
			}
			return err
		}
	case mode == "after-rollback":
		// Verification fails, the rollback puts everything back, and the process
		// dies before the journal is cleared.
		h.finalVerify = func() error { return errors.New("injected verify failure") }
		h.journalRemove = func(func() error) error { die(); return nil }
	case mode == "before-complete":
		// Dies after the final verification passed, before "complete" is written.
		h.journalWrite = func(p Phase, write func() error) error {
			if p == PhaseComplete {
				die()
			}
			return write()
		}
	case mode == "create-after-snapshot":
		_, _ = Create(context.Background(), CreateOptions{DataDir: dataDir, Root: root, Tool: ToolInfo{Name: "ao-test"},
			hooks: &testHooks{afterSnapshot: func(string) error { die(); return nil }}})
		t.Fatal("create did not crash")
	}
	_, err := Restore(context.Background(), RestoreOptions{DataDir: dataDir, Source: source, Root: root, CheckDaemon: noDaemon,
		Tool: ToolInfo{Name: "ao-test"}, hooks: h})
	t.Fatalf("restore did not crash: %v", err)
}

func crashChild(t *testing.T, mode, dataDir, root, source string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), "AO_P10_CRASH_MODE="+mode, "AO_P10_CRASH_DATA="+dataDir, "AO_P10_CRASH_ROOT="+root, "AO_P10_CRASH_SRC="+source)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != crashExitCode {
		t.Fatalf("child did not crash as planned (err=%v): %s", err, out)
	}
}

// §78/§79 — a crash before the swap leaves the data dir unchanged and bootable;
// a crash during it blocks boot until recover rolls it back; after recovery a
// restore works again.
func TestCrashDuringRestoreIsAlwaysResolvable(t *testing.T) {
	for _, tc := range []struct {
		mode     string
		critical bool
		result   string
	}{
		{"phase:" + string(PhaseRollbackReady), false, RecoverNoSwap},
		{"phase:" + string(PhaseStaged), false, RecoverNoSwap},
		{"phase:" + string(PhaseSwapping), true, RecoverRolledBack},
		{"mid-aside", true, RecoverRolledBack},
		{"mid-promote", true, RecoverRolledBack},
		{"phase:" + string(PhaseSwapped), true, RecoverRolledBack},
		// Review finding: recover's SQLite probe used to delete the pre-existing
		// empty -shm the finished rollback had put back, so this recover failed
		// for ever ("ao.db-shm present=false").
		{"after-rollback", true, RecoverRolledBack},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			f := newFixture(t)
			before := stripSidecars(managedDigest(t, f.dataDir))
			crashChild(t, tc.mode, f.dataDir, f.root, f.backupA.Path)

			j, err := readJournal(f.dataDir)
			if err != nil || j == nil {
				t.Fatalf("no journal after a crash: %v", err)
			}
			bootErr := CheckStartup(f.dataDir)
			if (bootErr != nil) != tc.critical {
				t.Fatalf("phase %s: boot refusal=%v, want refusal=%t", j.Phase, bootErr, tc.critical)
			}
			if _, err := f.restore(t, nil); codeOf(err) != CodeRestoreInterrupted {
				t.Fatalf("a restore over a crashed one: %v", err)
			}
			if _, err := Create(context.Background(), CreateOptions{DataDir: f.dataDir, Root: f.root, Tool: ToolInfo{Name: "ao-test"}}); tc.critical && codeOf(err) != CodeRestoreInterrupted {
				t.Fatalf("a backup of a possibly mixed data dir: %v", err)
			}

			rr, err := Recover(context.Background(), RecoverOptions{DataDir: f.dataDir, CheckDaemon: noDaemon})
			if err != nil || rr.Result != tc.result {
				t.Fatalf("recover: %+v %v", rr, err)
			}
			if got := stripSidecars(managedDigest(t, f.dataDir)); got != before {
				t.Fatalf("after recover the data dir differs from before the crash:\n%s\n---\n%s", before, got)
			}
			if got := semanticState(t, f.dataDir); got != f.stateB {
				t.Fatal("after recover the state is not the pre-restore state")
			}
			assertNoRestoreLeftovers(t, f.dataDir)
			if err := CheckStartup(f.dataDir); err != nil {
				t.Fatalf("boot still refused after recover: %v", err)
			}
			if _, err := f.restore(t, nil); err != nil {
				t.Fatalf("restore after recover: %v", err)
			}
			if got := semanticState(t, f.dataDir); got != f.stateA {
				t.Fatal("the retried restore did not return state A")
			}
		})
	}
}

// Independent review §5/§7/§37 — a real process death right after EVERY single
// rename of the swap (sidecar, database, identity and catalog set aside; each
// promotion), and after the final verification but before "complete": the boot
// gate refuses, the first recover returns exactly the pre-restore data dir, and
// the second and third find nothing to do and move nothing.
func TestCrashAfterEveryRenameIsRecoveredIdempotently(t *testing.T) {
	modes := []string{"before-complete"}
	for i := 1; i <= 7; i++ {
		modes = append(modes, fmt.Sprintf("after-rename:%d", i))
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			before := stripSidecars(managedDigest(t, f.dataDir))
			crashChild(t, mode, f.dataDir, f.root, f.backupA.Path)

			j, err := readJournal(f.dataDir)
			if err != nil || j == nil || !j.unsettled(f.dataDir) {
				t.Fatalf("journal after the crash: %+v %v", j, err)
			}
			if CheckStartup(f.dataDir) == nil {
				t.Fatal("the daemon would boot over a crashed swap")
			}
			for i := 1; i <= 3; i++ {
				rr, err := Recover(context.Background(), RecoverOptions{DataDir: f.dataDir, CheckDaemon: noDaemon})
				want := RecoverNothing
				if i == 1 {
					want = RecoverRolledBack
				}
				if err != nil || rr.Result != want {
					t.Fatalf("recover #%d: %+v %v", i, rr, err)
				}
				if got := stripSidecars(managedDigest(t, f.dataDir)); got != before {
					t.Fatalf("after recover #%d the data dir is not the pre-restore one:\n%s\n---\n%s", i, before, got)
				}
			}
			if semanticState(t, f.dataDir) != f.stateB {
				t.Fatal("not the pre-restore state")
			}
			assertNoRestoreLeftovers(t, f.dataDir)
			if err := CheckStartup(f.dataDir); err != nil {
				t.Fatal(err)
			}
			if _, err := f.restore(t, nil); err != nil || semanticState(t, f.dataDir) != f.stateA {
				t.Fatalf("restore after recover: %v", err)
			}
		})
	}
}

// §77 — a create killed mid-way never shows up as a valid backup, and prune
// cleans it once its owner is provably gone.
func TestCrashDuringCreateNeverLooksValid(t *testing.T) {
	parent := t.TempDir()
	dataDir := newInstallation(t, parent)
	root := filepath.Join(parent, "backups")
	crashChild(t, "create-after-snapshot", dataDir, root, "")

	rep, err := List(dataDir, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Entries) != 1 || rep.Entries[0].State != StateIncomplete {
		t.Fatalf("list after a crashed create: %+v", rep.Entries)
	}
	vr, err := Verify(context.Background(), rep.Entries[0].Path, VerifyOptions{})
	if err != nil || vr.Status != StatusInvalid || !hasReason(vr, CodeStagingIncomplete) {
		t.Fatalf("verify of the leftover: %+v %v", vr, err)
	}
	pr, err := Prune(context.Background(), PruneOptions{Root: root, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(rep.Entries[0].Path); err == nil {
		t.Fatalf("prune kept the orphaned staging: %+v", pr)
	}
	if res := mustCreate(t, dataDir, root); res == nil {
		t.Fatal("create after cleanup")
	}
}
