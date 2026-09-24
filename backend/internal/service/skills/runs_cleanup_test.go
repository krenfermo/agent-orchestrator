package skills_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/service/skills"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillrunner"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// runs_cleanup_test.go — the service side of the runner-hang fix: a stuck
// runtime ends a run in the right terminal state, never holds the boot or the
// shutdown, and a container whose removal is not confirmed is said to be so
// and retried.

// sweepingReaper is a reaper that is also a ContainerSweeper.
type sweepingReaper struct {
	mu        sync.Mutex
	reaped    []string
	reapErr   error
	pending   map[string]bool
	sweeps    atomic.Int32
	liveSeen  map[string]bool
	blockOnce bool
}

func (r *sweepingReaper) ReapRun(_ context.Context, runID, _, _ string) (skillrunner.ReapReport, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reaped = append(r.reaped, runID)
	return skillrunner.ReapReport{}, r.reapErr
}

func (r *sweepingReaper) SweepOwned(ctx context.Context, live func(string) bool) (skillrunner.SweepReport, error) {
	r.mu.Lock()
	block := r.blockOnce
	r.blockOnce = false
	for id := range r.liveSeen {
		r.liveSeen[id] = live(id)
	}
	r.mu.Unlock()
	// Counted after live() was consulted, so a test that sees the count also
	// sees the answers.
	r.sweeps.Add(1)
	if block {
		// A wedged runtime: the sweep returns only when its context ends.
		<-ctx.Done()
		return skillrunner.SweepReport{}, ctx.Err()
	}
	return skillrunner.SweepReport{}, nil
}

func (r *sweepingReaper) PendingCleanupFor(runID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pending[runID]
}

func newSweepRig(t *testing.T, block bool, reaper *sweepingReaper) runRig {
	t.Helper()
	exec := newBlockingExecutor(block)
	f, _, auth, scope := newRunFixture(t, exec.recordingExecutor, true)
	svc := skills.New(f.store, f.dataDir,
		skills.WithSkillExecutor(exec, auth, f.store, "", ""),
		skills.WithDurableRuns(f.store, "aod-sweep", reaper, nil))
	t.Cleanup(func() { svc.CloseRuns(5 * time.Second) })
	return runRig{f: f, svc: svc, auth: auth, scope: scope, exec: exec}
}

// A stuck runtime is a FAILURE with its own code, not a refusal: nothing an
// operator must change in policy, and worth retrying once the runtime answers.
func TestStartRun_AStuckRuntimeEndsFailedWithATypedCode(t *testing.T) {
	cases := map[string]struct {
		err  error
		code string
	}{
		"probe or inspect timed out": {fmt.Errorf("probe: %w", skillrunner.ErrRuntimeTimeout), skills.RunErrRuntimeTimeout},
		"CLI abandoned":              {fmt.Errorf("rm: %w", skillrunner.ErrCommandAbandoned), skills.RunErrRuntimeTimeout},
		"wall clock used up":         {fmt.Errorf("scan: %w", skillrunner.ErrWallClockExceeded), skills.RunErrTimedOut},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRunRig(t, false, true, "aod-owner-1")
			r.exec.err = tc.err
			run := r.start(t, "")
			d := r.waitTerminal(t, run.ID)
			if d.State != store.SkillRunFailed || d.ErrorCode != tc.code {
				t.Fatalf("state=%q code=%q; want failed %s", d.State, d.ErrorCode, tc.code)
			}
		})
	}
}

// A cancel whose container the runtime did not confirm removed must not say
// it was removed.
func TestCancelRun_SaysWhenTheContainerIsOnlyPendingRemoval(t *testing.T) {
	reaper := &sweepingReaper{pending: map[string]bool{}}
	r := newSweepRig(t, true, reaper)
	run := r.start(t, "")
	<-r.exec.started
	reaper.mu.Lock()
	reaper.pending[run.ID] = true
	reaper.mu.Unlock()
	if _, err := r.svc.CancelRun(context.Background(), r.scope.ProjectID, run.ID); err != nil {
		t.Fatal(err)
	}
	d := r.waitTerminal(t, run.ID)
	if d.State != store.SkillRunCancelled {
		t.Fatalf("state %q", d.State)
	}
	if strings.Contains(d.ErrorMessage, "stopped and removed") || !strings.Contains(d.ErrorMessage, "recorded for cleanup") {
		t.Fatalf("the message claims a removal nobody confirmed: %q", d.ErrorMessage)
	}
}

// SIGTERM/restart: a new daemon reconciles two runs a dead one left, over a
// runtime that times out. The first reap reports the timeout; AO does not
// wait on the runtime again for the second, and both runs still end.
func TestReconcileRuns_AStuckRuntimeDoesNotMultiplyTheBootWait(t *testing.T) {
	dead := newRunRig(t, true, true, "aod-dead")
	first := dead.start(t, "")
	<-dead.exec.started
	rec, ok, err := dead.f.store.GetSkillRun(context.Background(), first.ID)
	if err != nil || !ok {
		t.Fatalf("read run: %v", err)
	}
	rec.ID = "skr-second00000000000000000"
	rec.ModeID = "secret-scan"
	rec.IdempotencyKey = ""
	if _, _, err := dead.f.store.CreateSkillRun(context.Background(), rec); err != nil {
		t.Fatalf("seed second run: %v", err)
	}

	reaper := &sweepingReaper{reapErr: fmt.Errorf("list: %w", skillrunner.ErrRuntimeTimeout)}
	next := skills.New(dead.f.store, dead.f.dataDir,
		skills.WithSkillExecutor(newBlockingExecutor(false), dead.auth, dead.f.store, "", ""),
		skills.WithDurableRuns(dead.f.store, "aod-next", reaper, nil))
	t.Cleanup(func() { next.CloseRuns(time.Second) })
	n, err := next.ReconcileRuns(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("ReconcileRuns: n=%d err=%v", n, err)
	}
	if len(reaper.reaped) != 1 {
		t.Fatalf("asked a stuck runtime %d times; want once, then leave it to the sweeper", len(reaper.reaped))
	}
	for _, id := range []string{first.ID, rec.ID} {
		d, err := next.GetRun(context.Background(), dead.scope.ProjectID, id)
		if err != nil || d.State != store.SkillRunFailed || d.ErrorCode != skills.RunErrInterrupted {
			t.Fatalf("%s: err=%v state=%q code=%q", id, err, d.State, d.ErrorCode)
		}
	}
	close(dead.exec.release)
}

// The sweeper runs once immediately (off the boot path), leaves a live run's
// containers alone, and a wedged sweep cannot hold up shutdown.
func TestContainerSweeper_RunsAtOnceSparesLiveRunsAndStopsOnShutdown(t *testing.T) {
	reaper := &sweepingReaper{liveSeen: map[string]bool{}, blockOnce: false}
	r := newSweepRig(t, true, reaper)
	run := r.start(t, "")
	<-r.exec.started
	reaper.mu.Lock()
	reaper.liveSeen[run.ID] = false
	reaper.liveSeen["skr-finished"] = true
	reaper.blockOnce = true
	reaper.mu.Unlock()

	r.svc.StartContainerSweeper(time.Hour)
	deadline := time.Now().Add(5 * time.Second)
	for reaper.sweeps.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first sweep did not run at once")
		}
		time.Sleep(10 * time.Millisecond)
	}
	reaper.mu.Lock()
	live, finished := reaper.liveSeen[run.ID], reaper.liveSeen["skr-finished"]
	reaper.mu.Unlock()
	if !live || finished {
		t.Fatalf("live(): running run=%v (want true), finished run=%v (want false)", live, finished)
	}

	// The first sweep is blocked on a wedged runtime. Shutdown must not wait
	// for it.
	start := time.Now()
	r.svc.CloseRuns(5 * time.Second)
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("shutdown waited %s on a wedged sweep", took)
	}
}
