//go:build unix

package skillrunner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// hang_test.go — the Colima/virtiofs incident, reproduced without Docker.
//
// fakeDocker is a real executable standing in for the docker CLI. It keeps a
// container table in a state directory and can be told, by flag files, to
// behave the way the stalled runtime did:
//
//	run_hang  `docker run` never returns and ignores SIGTERM (a container
//	          blocked in `cat` on a stalled share, with the CLI attached)
//	down      `docker rm` and `docker ps` never return (the runtime wedged)
//
// Because it is a real process tree driven through execRunner and runBounded,
// what these tests prove is the actual kill, wait and abandon behaviour, not a
// mock's idea of it.

type fakeDocker struct {
	dir string
	bin string
}

const fakeDockerScript = `#!/bin/sh
PATH=/usr/bin:/bin; export PATH
S='%s'
echo "$*" >> "$S/calls"
hang() { trap '' TERM INT HUP; while :; do sleep 1; done; }
drop() { grep -v "^$1|" "$S/containers" > "$S/c.tmp"; mv "$S/c.tmp" "$S/containers"; }
touch "$S/containers"
cmd="$1"; shift
case "$cmd" in
run)
  name=""; owner=""; rid=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --name) name="$2"; shift;;
      --label) case "$2" in
          ao.skillrun.owner=*) owner="${2#ao.skillrun.owner=}";;
          ao.skillrun.id=*) rid="${2#ao.skillrun.id=}";;
        esac; shift;;
    esac
    shift
  done
  echo "$name|$owner|$rid" >> "$S/containers"
  echo $$ > "$S/run_pid"
  if [ -f "$S/run_hang" ]; then hang; fi
  drop "$name"
  exit 0;;
rm)
  [ -f "$S/down" ] && hang
  drop "$2"
  exit 0;;
ps)
  [ -f "$S/down" ] && hang
  filter=""
  while [ $# -gt 0 ]; do case "$1" in --filter) filter="$2"; shift;; esac; shift; done
  case "$filter" in
    name=*) n="${filter#name=^/}"; n="${n%%\$}"; grep "^$n|" "$S/containers" | cut -d'|' -f1;;
    label=ao.skillrun.owner=*) o="${filter#label=ao.skillrun.owner=}"
      awk -F'|' -v o="$o" '$2==o {print $1 "\t" $3}' "$S/containers";;
    label=ao.skillrun.id=*) r="${filter#label=ao.skillrun.id=}"
      awk -F'|' -v r="$r" '$3==r {print $1}' "$S/containers";;
  esac
  exit 0;;
network)
  [ -f "$S/down" ] && hang
  touch "$S/networks"
  sub="$1"; shift
  case "$sub" in
  create)
    name=""; owner=""; rid=""; internal="false"
    while [ $# -gt 0 ]; do
      case "$1" in
        --label) case "$2" in
            ao.skillrun.owner=*) owner="${2#ao.skillrun.owner=}";;
            ao.skillrun.id=*) rid="${2#ao.skillrun.id=}";;
          esac; shift;;
        --internal) internal="true";;
        *) name="$1";;
      esac
      shift
    done
    echo "$name|$owner|$rid|$internal" >> "$S/networks"
    exit 0;;
  rm)
    grep -v "^$1|" "$S/networks" > "$S/n.tmp" 2>/dev/null; mv "$S/n.tmp" "$S/networks"
    exit 0;;
  ls)
    q=""; filter=""
    while [ $# -gt 0 ]; do case "$1" in -q) q=1;; --filter) filter="$2"; shift;; esac; shift; done
    case "$filter" in
      label=ao.skillrun.owner=*) o="${filter#label=ao.skillrun.owner=}"
        if [ -n "$q" ]; then awk -F'|' -v o="$o" '$2==o {print $1}' "$S/networks"
        else awk -F'|' -v o="$o" '$2==o {print $1 "\t" $3}' "$S/networks"; fi;;
      label=ao.skillrun.id=*) r="${filter#label=ao.skillrun.id=}"
        awk -F'|' -v r="$r" '$3==r {print $1}' "$S/networks";;
    esac
    exit 0;;
  inspect)
    n="$1"; fmt=""
    while [ $# -gt 0 ]; do case "$1" in --format) fmt="$2"; shift;; esac; shift; done
    if grep -q "^$n|" "$S/networks"; then
      case "$fmt" in *Containers*) : ;; *) echo "$n";; esac
      exit 0
    fi
    exit 1;;
  connect|disconnect)
    exit 0;;
  esac
  exit 0;;
esac
exit 0
`

func newFakeDocker(t *testing.T) *fakeDocker {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte(fmt.Sprintf(fakeDockerScript, dir)), 0o755); err != nil { //nolint:gosec // test executable.
		t.Fatal(err)
	}
	return &fakeDocker{dir: dir, bin: bin}
}

func (f *fakeDocker) set(t *testing.T, flag string, on bool) {
	t.Helper()
	p := filepath.Join(f.dir, flag)
	if on {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	_ = os.Remove(p)
}

// seed puts a container in the table that AO did not start in this test.
func (f *fakeDocker) seed(t *testing.T, name, owner, runID string) {
	t.Helper()
	fh, err := os.OpenFile(filepath.Join(f.dir, "containers"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	fmt.Fprintf(fh, "%s|%s|%s\n", name, owner, runID)
}

func (f *fakeDocker) containers(t *testing.T) []string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(f.dir, "containers"))
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if name, _, _ := strings.Cut(line, "|"); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// seedNetwork puts a network in the table that AO did not create in this test,
// as a crashed pentest run would leave one behind.
func (f *fakeDocker) seedNetwork(t *testing.T, name, owner, runID string) {
	t.Helper()
	fh, err := os.OpenFile(filepath.Join(f.dir, "networks"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	fmt.Fprintf(fh, "%s|%s|%s|true\n", name, owner, runID)
}

func (f *fakeDocker) networks(t *testing.T) []string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(f.dir, "networks"))
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if name, _, _ := strings.Cut(line, "|"); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func (f *fakeDocker) calls(t *testing.T) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(f.dir, "calls"))
	return string(b)
}

func (f *fakeDocker) runner(owner string) *Runner {
	return (&Runner{runner: execRunner{}, runtime: Runtime{Binary: f.bin}}).WithOwner(owner)
}

// shortLimits shrinks every runtime limit so a hang costs a second or two, and
// restores them after the test. Not smaller: under a loaded host (`go test
// ./... -p 4`) starting the fake CLI alone has taken over 300 ms, and a limit
// shorter than process start-up kills it before it does anything to observe.
func shortLimits(t *testing.T) {
	t.Helper()
	oldProbe, oldGrace := probeTimeout, commandGrace
	probeTimeout, commandGrace = 2*time.Second, time.Second
	t.Cleanup(func() { probeTimeout, commandGrace = oldProbe, oldGrace })
}

func testImage() ApprovedImage {
	img := ApprovedImage{Digest: "sha256:" + strings.Repeat("c", 64)}
	img.Contract.BaseImage = "alpine:3.19"
	return img
}

func testRequest(t *testing.T, runID string) Request {
	t.Helper()
	in := t.TempDir()
	if err := os.WriteFile(filepath.Join(in, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return Request{
		Image: "sha256:" + strings.Repeat("d", 64), Argv: []string{"true"}, InputDir: in, RunID: runID,
		Limits: Limits{Wall: 1500 * time.Millisecond, MemoryBytes: 64 << 20, CPUs: 1, MaxPIDs: 16, MaxOutputBytes: 4096},
	}
}

// finishesWithin fails the test if fn takes longer than limit: "the worker is not
// held" is a statement about time.
func finishesWithin(t *testing.T, limit time.Duration, fn func()) time.Duration {
	t.Helper()
	start := time.Now()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
		return time.Since(start)
	case <-time.After(limit):
		t.Fatalf("still waiting after %s: the caller is held", limit)
		return 0
	}
}

func pidAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// --- runBounded -------------------------------------------------------------

func TestRunBounded_ADeadlineKillsTheWholeProcessGroup(t *testing.T) {
	shortLimits(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child")
	// The CLI ignores SIGTERM and leaves a grandchild holding stdout -- the
	// shape that pins a plain exec.CommandContext + Wait.
	cmd := exec.Command("/bin/sh", "-c", //nolint:noctx // runBounded enforces the context.
		"trap '' TERM; sleep 300 & echo $! > "+pidFile+"; wait")
	var out strings.Builder
	cmd.Stdout = &out

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var err error
	finishesWithin(t, 10*time.Second, func() { err = runBounded(ctx, cmd) })

	if !errors.Is(err, ErrRuntimeTimeout) || errors.Is(err, ErrCommandAbandoned) {
		t.Fatalf("a killed process is a timeout, not an abandonment: %v", err)
	}
	b, _ := os.ReadFile(pidFile)
	child, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if child == 0 {
		t.Fatal("the grandchild never started")
	}
	deadline := time.Now().Add(2 * time.Second)
	for pidAlive(child) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if pidAlive(child) {
		_ = syscall.Kill(child, syscall.SIGKILL)
		t.Fatalf("grandchild %d survived the group kill", child)
	}
}

func TestRunBounded_ACancelIsNotATimeout(t *testing.T) {
	shortLimits(t)
	cmd := exec.Command("/bin/sh", "-c", "trap '' TERM; while :; do sleep 1; done") //nolint:noctx // runBounded enforces the context.
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	var err error
	finishesWithin(t, 10*time.Second, func() { err = runBounded(ctx, cmd) })
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrRuntimeTimeout) {
		t.Fatalf("a cancel must read as the caller's cancel: %v", err)
	}
}

// A process in uninterruptible sleep cannot be killed by anything, and no test
// can put one there on purpose. So the kill is replaced by one that does
// nothing, which is indistinguishable from the caller's side: the process
// does not exit. AO must stop waiting, say so, and count it.
func TestRunBounded_AProcessThatWillNotDieIsAbandonedNotWaitedOn(t *testing.T) {
	shortLimits(t)
	oldKill := killGroup
	killGroup = func(*exec.Cmd) {}
	t.Cleanup(func() { killGroup = oldKill })

	cmd := exec.Command("/bin/sh", "-c", "trap '' TERM; while :; do sleep 1; done") //nolint:noctx // runBounded enforces the context.
	before := AbandonedCommands()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var err error
	took := finishesWithin(t, 10*time.Second, func() { err = runBounded(ctx, cmd) })
	t.Cleanup(func() { killProcessGroup(cmd) })

	if !errors.Is(err, ErrCommandAbandoned) || !errors.Is(err, ErrRuntimeTimeout) {
		t.Fatalf("want an abandoned timeout, got: %v", err)
	}
	if AbandonedCommands() != before+1 {
		t.Fatalf("the abandonment was not counted: %d -> %d", before, AbandonedCommands())
	}
	if took > 500*time.Millisecond+commandGrace+2*time.Second {
		t.Fatalf("abandoning took %s; the bound is the deadline plus %s", took, commandGrace)
	}
	if !pidAlive(cmd.Process.Pid) {
		t.Fatal("the stand-in should still be running: the test did not exercise abandonment")
	}
}

// --- the probe --------------------------------------------------------------

func TestVerifyStagingVisible_AProbeThatNeverFinishesIsRemovedAndReadsAsATimeout(t *testing.T) {
	shortLimits(t)
	fd := newFakeDocker(t)
	fd.set(t, "run_hang", true)
	r := fd.runner("aoi-mine")

	var err error
	finishesWithin(t, 15*time.Second, func() {
		err = r.verifyStagingVisible(context.Background(), t.TempDir(), testImage(), "skr-probe1")
	})
	if !errors.Is(err, ErrRuntimeTimeout) {
		t.Fatalf("a stuck probe must be a runtime timeout: %v", err)
	}
	if errors.Is(err, ErrStagingNotVisible) {
		t.Fatalf("a stuck runtime is not an unshared path; that sends the operator to the wrong fix: %v", err)
	}
	// The incident's orphan: the probe container must be gone, and confirmed.
	if left := fd.containers(t); len(left) != 0 {
		t.Fatalf("probe container(s) left running: %v", left)
	}
	if p := r.PendingCleanup(); len(p) != 0 {
		t.Fatalf("nothing should be pending once the runtime confirmed removal: %+v", p)
	}
	calls := fd.calls(t)
	for _, want := range []string{"--name ao-skillprobe-", "ao.skillrun.owner=aoi-mine",
		"ao.skillrun.id=skr-probe1", "ao.skillrun.kind=probe", "rm -f ao-skillprobe-"} {
		if !strings.Contains(calls, want) {
			t.Fatalf("expected %q in the runtime calls:\n%s", want, calls)
		}
	}
}

// --- the run ----------------------------------------------------------------

func TestRun_AWallClockTimeoutReleasesTheWorkerAndRemovesTheContainer(t *testing.T) {
	shortLimits(t)
	fd := newFakeDocker(t)
	fd.set(t, "run_hang", true)
	r := fd.runner("aoi-mine")

	var res Result
	var err error
	finishesWithin(t, 15*time.Second, func() { res, err = r.Run(context.Background(), testRequest(t, "skr-wall1")) })
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.Cleanup != CleanupConfirmed {
		t.Fatalf("want a timed-out run with a confirmed removal, got TimedOut=%v Cleanup=%q", res.TimedOut, res.Cleanup)
	}
	if left := fd.containers(t); len(left) != 0 {
		t.Fatalf("container left behind: %v", left)
	}
}

func TestRun_ACancelStopsTheRunPromptly(t *testing.T) {
	shortLimits(t)
	fd := newFakeDocker(t)
	fd.set(t, "run_hang", true)
	r := fd.runner("aoi-mine")
	req := testRequest(t, "skr-cancel1")
	req.Limits.Wall = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	var res Result
	finishesWithin(t, 15*time.Second, func() { res, _ = r.Run(ctx, req) })
	if res.TimedOut {
		t.Fatal("a cancel is not a wall-clock timeout")
	}
	if res.Cleanup != CleanupConfirmed || len(fd.containers(t)) != 0 {
		t.Fatalf("a cancelled run's container must be removed and confirmed: %q %v", res.Cleanup, fd.containers(t))
	}
}

// The runtime wedges in the middle of a run: the run times out, and so do the
// removal and the check. AO must not say the container is gone -- and must
// remove it once the runtime answers again.
func TestRun_ACleanupTheRuntimeCannotConfirmIsPendingAndRecoveredLater(t *testing.T) {
	shortLimits(t)
	fd := newFakeDocker(t)
	fd.set(t, "run_hang", true)
	fd.set(t, "down", true)
	r := fd.runner("aoi-mine")

	var res Result
	var err error
	finishesWithin(t, 30*time.Second, func() { res, err = r.Run(context.Background(), testRequest(t, "skr-down1")) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Cleanup != CleanupPending {
		t.Fatalf("AO claimed a removal the runtime never confirmed: %q", res.Cleanup)
	}
	if !r.PendingCleanupFor("skr-down1") {
		t.Fatal("the pending container is not recorded against its run")
	}
	if note := cleanupNote(res); !strings.Contains(note, "recorded for cleanup") {
		t.Fatalf("the error a caller sees must say the container is pending: %q", note)
	}
	if left := fd.containers(t); len(left) != 1 {
		t.Fatalf("the fake runtime should still hold the container: %v", left)
	}

	// Still down: a sweep confirms nothing and says so.
	var rep SweepReport
	finishesWithin(t, 30*time.Second, func() { rep, err = r.SweepOwned(context.Background(), nil) })
	if !errors.Is(err, ErrCleanupPending) && err == nil {
		t.Fatalf("a sweep against a wedged runtime cannot succeed: %v", err)
	}
	if len(rep.Removed) != 0 {
		t.Fatalf("reported removals the runtime never confirmed: %v", rep.Removed)
	}

	// The runtime recovers.
	fd.set(t, "down", false)
	finishesWithin(t, 15*time.Second, func() { rep, err = r.SweepOwned(context.Background(), nil) })
	if err != nil {
		t.Fatalf("sweep after recovery: %v", err)
	}
	if len(rep.Removed) != 1 || len(fd.containers(t)) != 0 || len(r.PendingCleanup()) != 0 {
		t.Fatalf("recovery did not remove the container: removed=%v left=%v pending=%v",
			rep.Removed, fd.containers(t), r.PendingCleanup())
	}
}

// --- ownership --------------------------------------------------------------

func TestSweepOwned_TouchesOnlyThisInstallationsIdleContainers(t *testing.T) {
	shortLimits(t)
	fd := newFakeDocker(t)
	r := fd.runner("aoi-mine")

	fd.seed(t, "someone-elses-db", "", "")                    // no AO label at all
	fd.seed(t, "ao-skillrun-other", "aoi-other", "skr-other") // another AO installation
	fd.seed(t, "ao-skillrun-oldbuild", "", "skr-old")         // an AO build before OwnerLabel
	fd.seed(t, "ao-skillrun-live", "aoi-mine", "skr-live")    // a run executing right now
	fd.seed(t, "ao-skillprobe-busy", "aoi-mine", "")          // a probe in flight here
	fd.seed(t, "ao-skillrun-dead", "aoi-mine", "skr-dead")    // a finished run's leftover
	fd.seed(t, "ao-skillprobe-stale", "aoi-mine", "")         // a probe nobody is waiting on
	r.track("ao-skillprobe-busy", "")

	rep, err := r.SweepOwned(context.Background(), func(id string) bool { return id == "skr-live" })
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(rep.Removed, ",")
	if got != "ao-skillprobe-stale,ao-skillrun-dead" {
		t.Fatalf("removed %q; want exactly this installation's idle leftovers", got)
	}
	left := strings.Join(fd.containers(t), ",")
	for _, keep := range []string{"someone-elses-db", "ao-skillrun-other", "ao-skillrun-oldbuild",
		"ao-skillrun-live", "ao-skillprobe-busy"} {
		if !strings.Contains(left, keep) {
			t.Fatalf("the sweep removed %s, which was not its to remove (left: %s)", keep, left)
		}
	}
	for _, foreign := range []string{"someone-elses-db", "ao-skillrun-other", "ao-skillrun-oldbuild"} {
		if strings.Contains(fd.calls(t), "rm -f "+foreign) {
			t.Fatalf("the sweep issued a removal for %s", foreign)
		}
	}
}

func TestSweepOwned_WithoutAnOwnerIdSweepsNothingByLabel(t *testing.T) {
	shortLimits(t)
	fd := newFakeDocker(t)
	fd.seed(t, "ao-skillrun-x", "", "skr-x")
	rep, err := fd.runner("").SweepOwned(context.Background(), nil)
	if err != nil || len(rep.Removed) != 0 || len(fd.containers(t)) != 1 {
		t.Fatalf("an owner-less runner claimed a container: %+v %v %v", rep, err, fd.containers(t))
	}
}

// --- restart: ReapRun ---------------------------------------------------------

func TestReapRun_ReportsPendingRatherThanRemovedWhenTheRuntimeIsWedged(t *testing.T) {
	shortLimits(t)
	fd := newFakeDocker(t)
	fd.seed(t, "ao-skillrun-crashed", "aoi-mine", "skr-crash1")
	fd.seed(t, "ao-skillrun-neighbour", "aoi-mine", "skr-other")
	r := fd.runner("aoi-mine")

	// Listing works, removal does not: the runtime answers reads and wedges
	// on the removal.
	var rep ReapReport
	var err error
	r.runner = rmHangs{inner: execRunner{}}
	finishesWithin(t, 30*time.Second, func() { rep, err = r.ReapRun(context.Background(), "skr-crash1", "", "") })
	if !errors.Is(err, ErrCleanupPending) {
		t.Fatalf("want ErrCleanupPending, got %v", err)
	}
	if len(rep.ContainersRemoved) != 0 || len(rep.ContainersPending) != 1 {
		t.Fatalf("a removal nobody confirmed was counted: %+v", rep)
	}

	// Recovery through the sweeper, by the pending record.
	r.runner = execRunner{}
	sweep, serr := r.SweepOwned(context.Background(), func(id string) bool { return id == "skr-other" })
	if serr != nil || !strings.Contains(strings.Join(sweep.Removed, ","), "ao-skillrun-crashed") {
		t.Fatalf("the crashed run's container was not recovered: %+v %v", sweep, serr)
	}
	if !strings.Contains(strings.Join(fd.containers(t), ","), "ao-skillrun-neighbour") {
		t.Fatal("another run's container was removed")
	}
}

// A pentest run that crashes mid-flight leaves not only its containers but the
// two networks it created (internal + egress), both labelled with its run id.
// Reaping the run by that id must reclaim the networks too, confirmed gone, and
// must not touch a neighbouring run's network.
func TestReapRun_ReclaimsTheRunsNetworksAndSparesOthers(t *testing.T) {
	shortLimits(t)
	fd := newFakeDocker(t)
	fd.seed(t, "ao-pentest-checker-skr-pen1", "aoi-mine", "skr-pen1")
	fd.seedNetwork(t, "ao-pentest-int-skr-pen1", "aoi-mine", "skr-pen1")
	fd.seedNetwork(t, "ao-pentest-egr-skr-pen1", "aoi-mine", "skr-pen1")
	fd.seedNetwork(t, "ao-pentest-int-skr-other", "aoi-mine", "skr-other")
	r := fd.runner("aoi-mine")

	rep, err := r.ReapRun(context.Background(), "skr-pen1", "", "")
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if len(rep.NetworksRemoved) != 2 || len(rep.NetworksPending) != 0 {
		t.Fatalf("want both of the run's networks reclaimed, got removed=%v pending=%v",
			rep.NetworksRemoved, rep.NetworksPending)
	}
	if len(rep.ContainersRemoved) != 1 {
		t.Fatalf("the run's container was not reclaimed: %+v", rep)
	}
	left := strings.Join(fd.networks(t), ",")
	if left != "ao-pentest-int-skr-other" {
		t.Fatalf("reap touched the wrong networks; left=%q want only the neighbour's", left)
	}
}

// The periodic sweep reclaims this installation's leftover pentest networks by
// its owner label, but must leave a live run's network — whose containers are
// still attached and doing their job — alone, and never touch another
// installation's network.
func TestSweepOwned_ReclaimsIdleOwnedNetworksAndSparesLiveOnes(t *testing.T) {
	shortLimits(t)
	fd := newFakeDocker(t)
	fd.seedNetwork(t, "ao-pentest-int-skr-live", "aoi-mine", "skr-live") // executing here
	fd.seedNetwork(t, "ao-pentest-egr-skr-live", "aoi-mine", "skr-live")
	fd.seedNetwork(t, "ao-pentest-int-skr-dead", "aoi-mine", "skr-dead") // a crashed run's leftover
	fd.seedNetwork(t, "ao-pentest-int-skr-other", "aoi-other", "skr-x")  // another installation
	r := fd.runner("aoi-mine")

	rep, err := r.SweepOwned(context.Background(), func(id string) bool { return id == "skr-live" })
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rep.NetworksRemoved, ","); got != "ao-pentest-int-skr-dead" {
		t.Fatalf("swept %q; want exactly the crashed run's idle network", got)
	}
	left := strings.Join(fd.networks(t), ",")
	for _, keep := range []string{"ao-pentest-int-skr-live", "ao-pentest-egr-skr-live", "ao-pentest-int-skr-other"} {
		if !strings.Contains(left, keep) {
			t.Fatalf("the sweep removed %s, which was not its to remove (left: %s)", keep, left)
		}
	}
}

// rmHangs passes every call through except `rm`, which blocks until its
// context ends -- the runtime answering reads and wedging on writes.
type rmHangs struct{ inner commandRunner }

func (h rmHangs) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	if len(args) > 0 && args[0] == "rm" {
		<-ctx.Done()
		return nil, fmt.Errorf("%w: rm: %w", ErrRuntimeTimeout, ctx.Err())
	}
	return h.inner.Output(ctx, name, args...)
}
