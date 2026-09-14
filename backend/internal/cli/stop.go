package cli

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

const defaultStopTimeout = 10 * time.Second

type stopOptions struct {
	timeout time.Duration
	json    bool
}

func newStopCommand(ctx *commandContext) *cobra.Command {
	opts := stopOptions{timeout: defaultStopTimeout}
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the AO daemon",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := ctx.stopDaemon(cmd.Context(), opts)
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), st)
			}
			if st.State == stateStopped {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "AO daemon stopped")
				return err
			}
			return writeStatus(cmd, st)
		},
	}
	cmd.Flags().DurationVar(&opts.timeout, "timeout", defaultStopTimeout, "How long to wait for daemon shutdown")
	cmd.Flags().BoolVar(&opts.json, "json", false, "Output stop result as JSON")
	return cmd
}

func (c *commandContext) stopDaemon(ctx context.Context, opts stopOptions) (daemonStatus, error) {
	cfg, err := config.Load()
	if err != nil {
		return daemonStatus{}, err
	}
	st, cands, err := c.discoverDaemon(ctx, cfg)
	if err != nil {
		return daemonStatus{}, err
	}
	switch st.State {
	case stateStopped:
		return st, nil
	case stateStale:
		// Every stale location is cleared, and each only while it still names
		// the dead incarnation that was inspected -- a daemon starting right now
		// keeps the run-file it just wrote.
		for _, cand := range cands {
			if cand.State != stateStale || cand.info == nil {
				continue
			}
			if _, err := runfile.RemoveIfMatches(cand.RunFile, *cand.info); err != nil {
				return daemonStatus{}, err
			}
		}
		return daemonStatus{State: stateStopped, RunFile: st.RunFile, DataDir: cfg.DataDir}, nil
	case stateForeign:
		return daemonStatus{}, fmt.Errorf("the run-file at %s belongs to another AO installation (%s); not stopping it", st.RunFile, st.Error)
	case stateUnverified:
		return daemonStatus{}, fmt.Errorf("pid %d is alive but is not provably this installation's daemon: %s", st.PID, st.Error)
	}
	if !st.owned {
		if st.Error != "" {
			return daemonStatus{}, fmt.Errorf("daemon pid %d is alive but ownership could not be verified: %s", st.PID, st.Error)
		}
		return daemonStatus{}, fmt.Errorf("daemon pid %d is alive but ownership could not be verified", st.PID)
	}

	if err := c.requestShutdown(ctx, st.Port); err != nil {
		return daemonStatus{}, fmt.Errorf("request daemon shutdown: %w", err)
	}
	return c.waitForDaemonExit(ctx, daemonIdentity{pid: st.PID, instance: st.InstanceID, port: st.Port}, st.RunFile, cfg.DataDir, opts.timeout)
}

// daemonIdentity is what `ao stop` verified before asking a daemon to exit, and
// what it re-verifies to decide the daemon is gone.
type daemonIdentity struct {
	pid      int
	instance string
	port     int
}

func (c *commandContext) requestShutdown(ctx context.Context, port int) error {
	reqCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, fmt.Sprintf("http://%s:%d/shutdown", config.LoopbackHost, port), http.NoBody)
	if err != nil {
		return err
	}
	resp, err := c.deps.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func (c *commandContext) waitForStopped(ctx context.Context, pid int, runFilePath, dataDir string, timeout time.Duration) (daemonStatus, error) {
	return c.waitForDaemonExit(ctx, daemonIdentity{pid: pid}, runFilePath, dataDir, timeout)
}

// waitForDaemonExit polls until the verified daemon is gone, within timeout.
// "Gone" is the process exiting, or its run-file removed AND its probe no longer
// answering as that incarnation. A run-file is only ever removed while it still
// names the incarnation that was stopped.
func (c *commandContext) waitForDaemonExit(ctx context.Context, id daemonIdentity, runFilePath, dataDir string, timeout time.Duration) (daemonStatus, error) {
	pid := id.pid
	if timeout <= 0 {
		timeout = defaultStopTimeout
	}
	deadline := c.deps.Now().Add(timeout)
	for {
		select {
		case <-ctx.Done():
			return daemonStatus{}, ctx.Err()
		default:
		}

		info, err := runfile.Read(runFilePath)
		if err != nil {
			return daemonStatus{}, err
		}
		alive := c.deps.ProcessAlive(pid)
		if !alive {
			// Only remove the run-file if it still belongs to the process we
			// stopped. A concurrent `ao start` may have already written a new
			// run-file for a different daemon; removing that would corrupt its
			// handshake and make a live daemon look stopped.
			if info != nil && info.PID == pid && (id.instance == "" || info.InstanceID == "" || info.InstanceID == id.instance) {
				if err := runfile.Remove(runFilePath); err != nil {
					return daemonStatus{}, err
				}
			}
			return daemonStatus{State: stateStopped, RunFile: runFilePath, DataDir: dataDir}, nil
		}
		if info == nil {
			// The run-file is the daemon's own liveness marker; it removes it as
			// it shuts down, before the OS process has necessarily exited. Once
			// the marker is gone the daemon has committed to stopping, so treat
			// that as stopped.
			//
			// We still poll for full process exit as a best effort so Windows
			// releases inherited handles such as daemon.log before callers clean
			// up the data directory, but exceeding the timeout is NOT an error:
			// with no desktop client connected the daemon can drain its
			// background workers slower than the stop timeout, and failing here
			// made `ao stop` spuriously report failure (issue #2214).
			if !c.deps.Now().Before(deadline) {
				// P9: the marker is gone and the PID lingers. If the port still
				// answers as the very incarnation that was asked to stop, it did
				// not stop, whatever the file says.
				if id.port > 0 {
					if probe, perr := c.readProbe(ctx, id.port, "healthz"); perr == nil &&
						verifyProbeOwner(probe, pid, "healthz") == nil &&
						(id.instance == "" || probe.InstanceID == "" || probe.InstanceID == id.instance) {
						return daemonStatus{}, fmt.Errorf("daemon pid %d removed its run-file but is still serving on port %d", pid, id.port)
					}
				}
				return daemonStatus{State: stateStopped, RunFile: runFilePath, DataDir: dataDir}, nil
			}
			c.deps.Sleep(100 * time.Millisecond)
			continue
		}
		if !c.deps.Now().Before(deadline) {
			return daemonStatus{}, fmt.Errorf("daemon pid %d did not stop within %s", pid, timeout)
		}
		c.deps.Sleep(100 * time.Millisecond)
	}
}
