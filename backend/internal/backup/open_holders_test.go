package backup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// sqliteShell is a real sqlite3 process driven line by line: the "manual
// sqlite3 an operator left open" of the independent review.
type sqliteShell struct {
	stdin io.WriteCloser
	lines chan string
	pid   int
	seq   int
}

func startSQLiteShell(t *testing.T) *sqliteShell {
	t.Helper()
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is not installed")
	}
	cmd := exec.Command(bin, "-batch")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := &sqliteShell{stdin: stdin, lines: make(chan string, 1024), pid: cmd.Process.Pid}
	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			s.lines <- sc.Text()
		}
		close(s.lines)
	}()
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); _ = pw.Close(); close(waited) }()
	t.Cleanup(func() {
		_ = stdin.Close()
		select {
		case <-waited:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill() // our own child only
			<-waited
		}
	})
	return s
}

// run sends commands and returns their output once the shell has run them.
func (s *sqliteShell) run(t *testing.T, cmds string) []string {
	t.Helper()
	s.seq++
	marker := fmt.Sprintf("__ao_review_done_%d__", s.seq)
	if _, err := fmt.Fprintf(s.stdin, "%s\n.print %s\n", cmds, marker); err != nil {
		t.Fatal(err)
	}
	var out []string
	timeout := time.After(15 * time.Second)
	for {
		select {
		case l, ok := <-s.lines:
			if !ok || l == marker {
				return out
			}
			out = append(out, l)
		case <-timeout:
			t.Fatalf("sqlite3 did not answer; output so far %q", out)
		}
	}
}

func (s *sqliteShell) quit() {
	_, _ = io.WriteString(s.stdin, ".quit\n")
	_ = s.stdin.Close()
	for range s.lines { //nolint:revive // drain until the shell exits
	}
}

// integrityOf opens the data dir the way the daemon does and reports
// integrity_check plus the project ids, never failing the test itself.
func integrityOf(t *testing.T, dataDir string) (string, []string) {
	t.Helper()
	db := openRW(t, dataDir)
	defer func() { _ = db.Close() }()
	lines, err := pragmaLines(context.Background(), db, "integrity_check", 5)
	if err != nil {
		return "error: " + err.Error(), nil
	}
	rows, err := db.Query(`SELECT id FROM projects ORDER BY id`)
	if err != nil {
		return "error: " + err.Error(), nil
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	return strings.Join(lines, "; "), ids
}

const ghostInsert = "INSERT INTO projects (id, path, registered_at) VALUES ('ghost', '/tmp/ghost', CURRENT_TIMESTAMP);"

// §12 BLOCKER of the independent review: a sqlite3 opened on ao.db and left
// idle holds no SQLite lock, so both probes pass. Before the fix its first
// write, after the restore, created a WAL that attached to the RESTORED
// database and corrupted it ("database disk image is malformed") while the
// restore had reported RESTORED. Now the restore sees the descriptor and
// refuses before touching anything.
func TestRestoreRefusesAProcessThatOpenedTheDatabaseWithoutReading(t *testing.T) {
	f := newFixture(t)
	dbPath := filepath.Join(f.dataDir, DatabaseAsset)
	sh := startSQLiteShell(t)
	sh.run(t, ".open "+dbPath)
	if err := ProbeExclusive(dbPath); err != nil {
		t.Fatalf("precondition: an idle shell holds no SQLite lock, the probe must pass: %v", err)
	}
	before := stripSidecars(managedDigest(t, f.dataDir))

	rep, err := f.restore(t, nil)
	if codeOf(err) != CodeDBInUse || rep.DestinationTouched {
		t.Fatalf("restore with a process holding ao.db open: err=%v rep=%+v", err, rep)
	}
	if !strings.Contains(err.Error(), fmt.Sprint(sh.pid)) {
		t.Fatalf("the refusal does not name the process: %v", err)
	}
	if got := stripSidecars(managedDigest(t, f.dataDir)); got != before {
		t.Fatal("the refused restore changed the data dir")
	}
	assertNoRestoreLeftovers(t, f.dataDir)

	sh.run(t, ghostInsert)
	sh.quit()
	integrity, ids := integrityOf(t, f.dataDir)
	if integrity != "ok" || !slices.Contains(ids, "ghost") || !slices.Contains(ids, "state-b") {
		t.Fatalf("the database is not whole after the shell wrote: integrity=%q ids=%v", integrity, ids)
	}
}

// §12 — a process that opens ao.db inside the window (after the second probe,
// before the renames) holds the file the swap moves into previous/. The check
// after the swap finds it there and rolls back, which gives that process back
// the very file it opened; its later write is consistent, and the restore can
// be retried once it is gone.
func TestRestoreRollsBackWhenAProcessOpenedTheDatabaseInsideTheWindow(t *testing.T) {
	f := newFixture(t)
	dbPath := filepath.Join(f.dataDir, DatabaseAsset)
	var sh *sqliteShell
	rep, err := f.restore(t, func(o *RestoreOptions) {
		o.hooks = &testHooks{atPhase: func(p Phase) error {
			if p == PhaseSwapping {
				sh = startSQLiteShell(t)
				sh.run(t, ".open "+dbPath)
			}
			return nil
		}}
	})
	if classOf(err) != ClassRolledBack || !hasCode(err, CodeDBInUse) || rep.Result != ResultRolledBack {
		t.Fatalf("restore with a process that opened ao.db in the window: err=%v rep=%+v", err, rep)
	}
	if semanticState(t, f.dataDir) != f.stateB {
		t.Fatal("the rollback did not put state B back")
	}
	assertNoRestoreLeftovers(t, f.dataDir)

	sh.run(t, ghostInsert)
	sh.quit()
	integrity, ids := integrityOf(t, f.dataDir)
	if integrity != "ok" || !slices.Contains(ids, "ghost") {
		t.Fatalf("after the rollback the shell's write is not consistent: integrity=%q ids=%v", integrity, ids)
	}
	if _, err := f.restore(t, nil); err != nil || semanticState(t, f.dataDir) != f.stateA {
		t.Fatalf("restore once the process is gone: %v", err)
	}
	integrity, _ = integrityOf(t, f.dataDir)
	if integrity != "ok" {
		t.Fatalf("restored database integrity: %q", integrity)
	}
}

// A process that opens the RESTORED database after the swap, while the
// restore then fails and must roll back: moving that file away would hand the
// process's sidecars to the previous database. The rollback refuses, boot stays
// refused, recover refuses while the process lives and rolls back once it exits.
func TestRollbackRefusesWhileTheRestoredDatabaseIsOpen(t *testing.T) {
	f := newFixture(t)
	dbPath := filepath.Join(f.dataDir, DatabaseAsset)
	var sh *sqliteShell
	rep, err := f.restore(t, func(o *RestoreOptions) {
		o.hooks = &testHooks{
			atPhase: func(p Phase) error {
				if p == PhaseSwapped {
					sh = startSQLiteShell(t)
					sh.run(t, ".open "+dbPath)
				}
				return nil
			},
			finalVerify: func() error { return errors.New("injected verify failure") },
		}
	})
	if classOf(err) != ClassRollbackFailed || rep.Result != ResultRollbackFailed || !hasCode(err, CodeDBInUse) {
		t.Fatalf("err=%v rep=%+v", err, rep)
	}
	if CheckStartup(f.dataDir) == nil {
		t.Fatal("boot allowed over a restore that could not roll back")
	}
	if rr, err := Recover(context.Background(), RecoverOptions{DataDir: f.dataDir, CheckDaemon: noDaemon}); codeOf(err) != CodeDBInUse || rr.Result != RecoverFailed {
		t.Fatalf("recover while the restored database is open: %+v %v", rr, err)
	}
	sh.quit()
	rr, err := Recover(context.Background(), RecoverOptions{DataDir: f.dataDir, CheckDaemon: noDaemon})
	if err != nil || rr.Result != RecoverRolledBack {
		t.Fatalf("recover once the process exited: %+v %v", rr, err)
	}
	if semanticState(t, f.dataDir) != f.stateB {
		t.Fatal("not the previous state")
	}
	if integrity, _ := integrityOf(t, f.dataDir); integrity != "ok" {
		t.Fatalf("integrity after recover: %q", integrity)
	}
	assertNoRestoreLeftovers(t, f.dataDir)
}

// Not being able to see open descriptors refuses: before the swap nothing is
// touched, after it the restore rolls back.
func TestRestoreFailsClosedWhenOpenFilesCannotBeListed(t *testing.T) {
	broken := func([]string) ([]int, error) { return nil, errors.New("injected: lsof unavailable") }
	t.Run("before the swap", func(t *testing.T) {
		f := newFixture(t)
		before := stripSidecars(managedDigest(t, f.dataDir))
		rep, err := f.restore(t, func(o *RestoreOptions) { o.hooks = &testHooks{openHolders: broken} })
		if codeOf(err) != CodeDBInUse || classOf(err) != ClassRefused || rep.DestinationTouched {
			t.Fatalf("err=%v rep=%+v", err, rep)
		}
		if stripSidecars(managedDigest(t, f.dataDir)) != before {
			t.Fatal("data dir changed")
		}
		assertNoRestoreLeftovers(t, f.dataDir)
	})
	t.Run("after the swap", func(t *testing.T) {
		f := newFixture(t)
		before := stripSidecars(managedDigest(t, f.dataDir))
		sep := string(filepath.Separator)
		hooks := &testHooks{openHolders: func(paths []string) ([]int, error) {
			if strings.Contains(paths[0], sep+"previous"+sep) {
				return nil, errors.New("injected: lsof unavailable")
			}
			return nil, nil
		}}
		rep, err := f.restore(t, func(o *RestoreOptions) { o.hooks = hooks })
		if classOf(err) != ClassRolledBack || !hasCode(err, CodeDBInUse) || rep.Result != ResultRolledBack {
			t.Fatalf("err=%v rep=%+v", err, rep)
		}
		if stripSidecars(managedDigest(t, f.dataDir)) != before || semanticState(t, f.dataDir) != f.stateB {
			t.Fatal("the previous state is not back")
		}
		assertNoRestoreLeftovers(t, f.dataDir)
	})
}

// The operating-system check itself: an idle sqlite3 is seen, still seen after
// its file is renamed (that is what the post-swap check relies on), and not
// seen once it exits.
func TestOpenHoldersFollowsTheDescriptorAcrossARename(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows forbids renaming an open database instead")
	}
	dataDir := newInstallation(t, t.TempDir())
	dbPath := filepath.Join(dataDir, DatabaseAsset)
	sh := startSQLiteShell(t)
	sh.run(t, ".open "+dbPath)
	pids, err := openHolders([]string{dbPath})
	if err != nil || !slices.Contains(pids, sh.pid) {
		t.Fatalf("holders of an idle-open database: %v %v (shell %d)", pids, err, sh.pid)
	}
	moved := filepath.Join(dataDir, "moved.db")
	if err := os.Rename(dbPath, moved); err != nil {
		t.Fatal(err)
	}
	pids, err = openHolders([]string{moved})
	if err != nil || !slices.Contains(pids, sh.pid) {
		t.Fatalf("holders after the rename: %v %v", pids, err)
	}
	sh.quit()
	if pids, err := openHolders([]string{moved}); err != nil || len(pids) != 0 {
		t.Fatalf("holders after the shell exited: %v %v", pids, err)
	}
}
