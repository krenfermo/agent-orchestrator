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
	ContainersRemoved []string
	StagingRemoved    string
}

// ReapRun removes the container(s) labelled with runID and the run's staging
// directory under the staging root for projectPath (or override). It is safe to
// call for a run that left nothing behind.
func (r *Runner) ReapRun(ctx context.Context, runID, projectPath, stagingOverride string) (ReapReport, error) {
	var rep ReapReport
	if !ValidRunID(runID) {
		return rep, fmt.Errorf("skillrunner: run id %q is not a valid run id", runID)
	}
	if r.Available() {
		out, err := r.runner.Output(ctx, r.runtime.Binary, "ps", "-aq",
			"--filter", "label="+RunIDLabel+"="+runID)
		if err != nil {
			return rep, fmt.Errorf("skillrunner: list containers of run %s: %w", runID, err)
		}
		for _, id := range strings.Fields(string(out)) {
			r.forceRemove(ctx, id)
			rep.ContainersRemoved = append(rep.ContainersRemoved, id)
		}
	}
	if strings.TrimSpace(projectPath) == "" && strings.TrimSpace(stagingOverride) == "" {
		return rep, nil
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
	return rep, nil
}
