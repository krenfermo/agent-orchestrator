package skillrunner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// reap.go removes what a durable skill run left behind when the daemon that
// owned it stopped mid-run.
//
// A normal run cleans up after itself: RunStaticScan removes its staging with a
// deferred Cleanup and Run force-removes its container. A daemon that is killed
// runs neither, and leaves two things: a container that may still be running,
// and a copy of somebody's source in the staging root. Both are addressed by
// the run id alone -- the container by its RunIDLabel, the staging directory by
// its name -- so reaping one run can never touch another run, or another
// installation's run on the same container runtime.

// ReapReport says what ReapRun found and removed.
type ReapReport struct {
	// ContainersRemoved are containers the runtime CONFIRMED gone.
	ContainersRemoved []string
	// ContainersPending are containers of the run whose removal the runtime
	// did not confirm. They stay recorded, and SweepOwned retries them.
	ContainersPending []string
	// NetworksRemoved are networks the runtime CONFIRMED gone. A pentest run
	// creates an internal and an egress network labelled with its run id;
	// removing its containers does not remove them.
	NetworksRemoved []string
	// NetworksPending are networks of the run whose removal the runtime did
	// not confirm. SweepOwned retries them by this installation's owner label.
	NetworksPending []string
	StagingRemoved  string
}

// ReapRun removes the container(s) labelled with runID and the run's staging
// directory under the staging root for projectPath (or override). It is safe to
// call for a run that left nothing behind.
func (r *Runner) ReapRun(ctx context.Context, runID, projectPath, stagingOverride string) (ReapReport, error) {
	var rep ReapReport
	if !ValidRunID(runID) {
		return rep, fmt.Errorf("skillrunner: run id %q is not a valid run id", runID)
	}
	var pendingErr error
	if r.Available() {
		// Bounded here, not by the caller: the boot reconcile passes a context
		// with no deadline, and a wedged runtime must not hold up the boot.
		listCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		out, err := r.runner.Output(listCtx, r.runtime.Binary, "ps", "-aq", "--no-trunc",
			"--filter", "label="+RunIDLabel+"="+runID)
		cancel()
		if err != nil {
			return rep, fmt.Errorf("skillrunner: list containers of run %s: %w", runID, err)
		}
		for _, id := range strings.Fields(string(out)) {
			if r.removeContainer(ctx, id, runID) == CleanupConfirmed {
				rep.ContainersRemoved = append(rep.ContainersRemoved, id)
			} else {
				rep.ContainersPending = append(rep.ContainersPending, id)
			}
		}

		// Networks come after containers: a pentest run's internal/egress
		// networks are labelled with the same run id but are not removed by
		// removing its containers, so a killed daemon leaks them. Reclaim them
		// by that run id, force-detaching any endpoint left over, confirmed
		// gone rather than assumed. Removing the containers first means the
		// networks are normally empty by the time we get here.
		netListCtx, netCancel := context.WithTimeout(ctx, probeTimeout)
		netOut, netErr := r.runner.Output(netListCtx, r.runtime.Binary, "network", "ls", "-q", "--no-trunc",
			"--filter", "label="+RunIDLabel+"="+runID)
		netCancel()
		if netErr != nil {
			return rep, fmt.Errorf("skillrunner: list networks of run %s: %w", runID, netErr)
		}
		for _, id := range strings.Fields(string(netOut)) {
			if r.removeNetworkConfirmed(ctx, id) == CleanupConfirmed {
				rep.NetworksRemoved = append(rep.NetworksRemoved, id)
			} else {
				rep.NetworksPending = append(rep.NetworksPending, id)
			}
		}

		if leftover := append(append([]string{}, rep.ContainersPending...), rep.NetworksPending...); len(leftover) > 0 {
			pendingErr = fmt.Errorf("%w: run %s: %s", ErrCleanupPending, runID,
				strings.Join(leftover, ", "))
		}
	}
	if strings.TrimSpace(projectPath) == "" && strings.TrimSpace(stagingOverride) == "" {
		return rep, pendingErr
	}
	root, err := StagingRootFor(projectPath, stagingOverride)
	if err != nil {
		return rep, err
	}
	dir := filepath.Join(root, runID)
	if _, err := os.Lstat(dir); err == nil {
		if err := (Staging{Dir: dir, Root: root}).Cleanup(); err != nil {
			return rep, err
		}
		rep.StagingRemoved = dir
	}
	return rep, pendingErr
}
