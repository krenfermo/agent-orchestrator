package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Independent review §4 — every journal write a restore makes fails in turn:
// once or from then on (a failing disk, removals included), with nothing
// written or with the write landing and still reporting failure. Whatever the
// restore reports, it must be what the durable state says:
//
//   - RESTORED only when no journal on disk can read the restore as unfinished;
//   - a report that does not demand recover leaves a data dir AO boots on;
//   - the state is exactly A after RESTORED and exactly B otherwise, before and
//     after recover, and recover is idempotent.
func TestRestoreJournalFailureMatrix(t *testing.T) {
	phases := []Phase{PhasePreparing, PhaseRollbackReady, PhaseStaged, PhaseSwapping, PhaseSwapped, PhaseComplete, PhaseRollingBack, PhaseRolledBack}
	for _, at := range phases {
		for _, sticky := range []bool{false, true} {
			for _, landed := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/sticky=%t/landed=%t", at, sticky, landed), func(t *testing.T) {
					journalFailureCase(t, at, sticky, landed)
				})
			}
		}
	}
}

func journalFailureCase(t *testing.T, at Phase, sticky, landed bool) {
	f := newFixture(t)
	before := stripSidecars(managedDigest(t, f.dataDir))
	injected := errors.New("injected journal failure")
	failing, fired := false, false
	hooks := &testHooks{
		journalWrite: func(p Phase, write func() error) error {
			if p == at && !fired {
				failing, fired = true, true
			} else if !sticky {
				failing = false
			}
			if !failing {
				return write()
			}
			if landed {
				_ = write()
			}
			return injected
		},
		journalRemove: func(remove func() error) error {
			// "rolled_back" is only ever written when the journal cannot be removed.
			if (sticky && failing) || at == PhaseRolledBack {
				return injected
			}
			return remove()
		},
	}
	if at == PhaseRollingBack || at == PhaseRolledBack {
		hooks.finalVerify = func() error { return errors.New("injected verify failure") }
	}
	rep, err := f.restore(t, func(o *RestoreOptions) { o.hooks = hooks })
	if !fired {
		t.Fatalf("phase %s was never recorded (err=%v)", at, err)
	}

	want := f.stateB
	switch {
	case err == nil:
		want = f.stateA
		if rep.Result != ResultRestored {
			t.Fatalf("no error but result %s", rep.Result)
		}
		j, jerr := readJournal(f.dataDir)
		if jerr != nil || (j != nil && (j.Phase != PhaseComplete || j.unsettled(f.dataDir))) {
			t.Fatalf("RESTORED while the journal on disk reads %+v (%v)", j, jerr)
		}
	case classOf(err) == ClassRolledBack:
		if stripSidecars(managedDigest(t, f.dataDir)) != before {
			t.Fatal("rolled back but the previous files are not back")
		}
	case classOf(err) == ClassRollbackFailed:
		t.Fatalf("a journal failure alone made the rollback fail: %v", err)
	default:
		if rep.DestinationTouched || stripSidecars(managedDigest(t, f.dataDir)) != before {
			t.Fatalf("failed before the swap but the data dir changed: %v", err)
		}
	}
	if got := semanticState(t, f.dataDir); got != want {
		t.Fatalf("reported %s (err=%v) but the data dir holds the other state", rep.Result, err)
	}
	if !rep.RecoverRequired {
		if berr := CheckStartup(f.dataDir); berr != nil {
			t.Fatalf("the report does not demand recover, yet boot is refused: %v (restore err=%v)", berr, err)
		}
	}

	for i := 1; i <= 3; i++ {
		rr, rerr := Recover(context.Background(), RecoverOptions{DataDir: f.dataDir, CheckDaemon: noDaemon})
		if rerr != nil || (i > 1 && rr.Result != RecoverNothing) {
			t.Fatalf("recover #%d: %+v %v", i, rr, rerr)
		}
		if got := semanticState(t, f.dataDir); got != want {
			t.Fatalf("recover #%d changed the reported state (%s)", i, rr.Result)
		}
	}
	assertNoRestoreLeftovers(t, f.dataDir)
	if err := CheckStartup(f.dataDir); err != nil {
		t.Fatal(err)
	}
}

// The deepest case of the matrix, spelled out: the "complete" record reaches
// the disk while its write reports failure, and nothing after it can be
// written or removed. The rollback cannot overwrite that "complete", so recover
// must not believe it -- believing it would delete previous/, the only copy of
// the pre-swap files. failed/ contradicts it: boot is refused and recover rolls
// back.
func TestACompletionRecordContradictedByARollbackIsNotTrusted(t *testing.T) {
	f := newFixture(t)
	before := stripSidecars(managedDigest(t, f.dataDir))
	injected := errors.New("injected journal failure")
	broken := false
	rep, err := f.restore(t, func(o *RestoreOptions) {
		o.hooks = &testHooks{
			journalWrite: func(p Phase, write func() error) error {
				switch {
				case p == PhaseComplete:
					_ = write()
					broken = true
					return injected
				case broken:
					return injected
				}
				return write()
			},
			journalRemove: func(remove func() error) error {
				if broken {
					return injected
				}
				return remove()
			},
		}
	})
	if classOf(err) != ClassRolledBack || !rep.RecoverRequired {
		t.Fatalf("err=%v rep=%+v", err, rep)
	}
	j, _ := readJournal(f.dataDir)
	if j == nil || j.Phase != PhaseComplete {
		t.Fatalf("precondition: the journal on disk should still read complete: %+v", j)
	}
	if _, err := os.Lstat(filepath.Join(f.dataDir, j.WorkDir, "failed")); err != nil {
		t.Fatal("precondition: failed/ should exist")
	}
	if CheckStartup(f.dataDir) == nil {
		t.Fatal("the boot gate trusts a completion the rollback contradicted")
	}
	rr, err := Recover(context.Background(), RecoverOptions{DataDir: f.dataDir, CheckDaemon: noDaemon})
	if err != nil || rr.Result != RecoverRolledBack {
		t.Fatalf("recover: %+v %v", rr, err)
	}
	if stripSidecars(managedDigest(t, f.dataDir)) != before || semanticState(t, f.dataDir) != f.stateB {
		t.Fatal("recover did not keep the pre-swap state")
	}
	assertNoRestoreLeftovers(t, f.dataDir)
}
