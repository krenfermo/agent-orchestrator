//go:build unix

package skillrunner

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// hang_live_test.go — the runner-hang fix against the host's real runtime.
//
// A workload that never finishes stands in for the incident's container
// blocked on a stalled share (a real virtiofs stall cannot be produced on
// demand). What is real here: the docker CLI being killed at the wall clock,
// the container outliving it, the confirmed removal, the ownership labels, and
// a sweep that removes this installation's leftover and not a neighbour's.

func liveDocker(t *testing.T, r *Runner, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, r.runtime.Binary, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestLive_AHungWorkloadIsKilledAtItsWallClockAndItsContainerRemoved(t *testing.T) {
	r := liveRunner(t)
	image := pinnedImage(t, r)
	owner := "aoi-livehang-" + randomToken()
	r.WithOwner(owner)
	input, _ := workspace(t)
	runID := "skr-livehang-" + randomToken()

	var res Result
	var err error
	seenLabels := make(chan string, 1)
	go func() {
		// While it runs, the container must be findable by this
		// installation's owner label, with its run id.
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			out, _ := exec.Command(r.runtime.Binary, "ps", "--filter", "label="+OwnerLabel+"="+owner, //nolint:noctx // bounded by the loop.
				"--format", "{{.Names}} {{.Label \""+RunIDLabel+"\"}} {{.Label \""+KindLabel+"\"}}").Output()
			if s := strings.TrimSpace(string(out)); s != "" {
				seenLabels <- s
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		seenLabels <- ""
	}()
	took := finishesWithin(t, 60*time.Second, func() {
		res, err = r.Run(context.Background(), Request{
			Image: image, Argv: []string{"sleep", "600"}, InputDir: input, RunID: runID,
			Limits: Limits{Wall: 4 * time.Second, MemoryBytes: 64 << 20, CPUs: 0.5, MaxPIDs: 16, MaxOutputBytes: 4096},
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	labels := <-seenLabels
	if !strings.Contains(labels, runID) || !strings.HasSuffix(labels, kindRun) || !strings.HasPrefix(labels, "ao-skillrun-") {
		t.Fatalf("the running container did not carry AO's ownership labels: %q", labels)
	}
	if !res.TimedOut || res.Cleanup != CleanupConfirmed {
		t.Fatalf("TimedOut=%v Cleanup=%q; want a timed-out run whose removal the runtime confirmed", res.TimedOut, res.Cleanup)
	}
	if left := liveDocker(t, r, "ps", "-aq", "--filter", "label="+RunIDLabel+"="+runID); left != "" {
		t.Fatalf("the hung container survived: %s", left)
	}
	t.Logf("a 600s workload was stopped and removed in %s (wall clock 4s)", took.Round(time.Millisecond))
}

func TestLive_SweepRemovesThisInstallationsLeftoverAndNothingElse(t *testing.T) {
	r := liveRunner(t)
	image := pinnedImage(t, r)
	owner := "aoi-livesweep-" + randomToken()
	r.WithOwner(owner)

	// A leftover of ours: this installation's label, a finished run's id.
	mine := "ao-skillrun-leftover-" + randomToken()
	liveDocker(t, r, "run", "-d", "--name", mine, "--network", "none",
		"--label", RunLabel+"=1", "--label", OwnerLabel+"="+owner, "--label", RunIDLabel+"=skr-finished",
		image, "sleep", "600")
	// A neighbour: AO's generic label, but another installation's owner id.
	neighbour := "ao-skillrun-neighbour-" + randomToken()
	liveDocker(t, r, "run", "-d", "--name", neighbour, "--network", "none",
		"--label", RunLabel+"=1", "--label", OwnerLabel+"=aoi-someone-else", "--label", RunIDLabel+"=skr-finished",
		image, "sleep", "600")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, r.runtime.Binary, "rm", "-f", mine, neighbour).Run()
	})

	rep, err := r.SweepOwned(context.Background(), func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Removed) != 1 || rep.Removed[0] != mine {
		t.Fatalf("removed %v; want exactly [%s]", rep.Removed, mine)
	}
	if left := liveDocker(t, r, "ps", "-aq", "--filter", "name=^/"+mine+"$"); left != "" {
		t.Fatalf("our leftover is still there: %s", left)
	}
	if left := liveDocker(t, r, "ps", "-q", "--filter", "name=^/"+neighbour+"$"); left == "" {
		t.Fatal("the sweep removed another installation's container")
	}
}
