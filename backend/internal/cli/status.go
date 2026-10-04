package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/daemonmeta"
)

const probeTimeout = 2 * time.Second

type statusOptions struct {
	json bool
}

type daemonState string

const (
	stateReady     daemonState = "ready"
	stateStopped   daemonState = "stopped"
	stateStale     daemonState = "stale"
	stateUnhealthy daemonState = "unhealthy"
	stateNotReady  daemonState = "not_ready"
)

type daemonStatus struct {
	State     daemonState `json:"state"`
	PID       int         `json:"pid,omitempty"`
	Port      int         `json:"port,omitempty"`
	StartedAt *time.Time  `json:"startedAt,omitempty"`
	Uptime    string      `json:"uptime,omitempty"`
	RunFile   string      `json:"runFile"`
	DataDir   string      `json:"dataDir"`
	Health    string      `json:"health,omitempty"`
	Ready     string      `json:"ready,omitempty"`
	Error     string      `json:"error,omitempty"`
	// P9 identity of the daemon this status describes, when its run-file names it.
	InstanceID     string `json:"instanceId,omitempty"`
	InstallationID string `json:"installationId,omitempty"`
	// Candidates lists every run-file location that was found for this
	// installation, when more than one was, so a person sees both conventions.
	Candidates []runFileCandidate `json:"candidates,omitempty"`
	owned      bool
}

type probeResult struct {
	Status           string `json:"status"`
	Service          string `json:"service"`
	PID              int    `json:"pid"`
	ExecutablePath   string `json:"executablePath,omitempty"`
	WorkingDirectory string `json:"workingDirectory,omitempty"`
	InstanceID       string `json:"instanceId,omitempty"`
	InstallationID   string `json:"installationId,omitempty"`
	DataDir          string `json:"dataDir,omitempty"`
}

func newStatusCommand(ctx *commandContext) *cobra.Command {
	var opts statusOptions
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show AO daemon status",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := ctx.inspectDaemon(cmd.Context())
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), st)
			}
			return writeStatus(cmd, st)
		},
	}
	cmd.Flags().BoolVar(&opts.json, "json", false, "Output status as JSON")
	return cmd
}

func (c *commandContext) inspectDaemon(ctx context.Context) (daemonStatus, error) {
	cfg, err := config.Load()
	if err != nil {
		return daemonStatus{}, err
	}
	st, _, err := c.discoverDaemon(ctx, cfg)
	return st, err
}

func (c *commandContext) readProbe(ctx context.Context, port int, path string) (probeResult, error) {
	reqCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, fmt.Sprintf("http://%s:%d/%s", config.LoopbackHost, port, path), http.NoBody)
	if err != nil {
		return probeResult{}, err
	}
	resp, err := c.deps.HTTPClient.Do(req)
	if err != nil {
		return probeResult{}, fmt.Errorf("%s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return probeResult{}, fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
	}
	var body probeResult
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return probeResult{}, fmt.Errorf("%s: decode response: %w", path, err)
	}
	if body.Status == "" {
		return probeResult{}, fmt.Errorf("%s: missing status", path)
	}
	return body, nil
}

func verifyProbeOwner(probe probeResult, wantPID int, path string) error {
	if probe.Service != daemonmeta.ServiceName {
		return fmt.Errorf("%s: response is not from AO daemon", path)
	}
	if probe.PID != wantPID {
		return fmt.Errorf("%s: daemon pid %d does not match run-file pid %d", path, probe.PID, wantPID)
	}
	return nil
}

func writeStatus(cmd *cobra.Command, st daemonStatus) error {
	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(out, "AO daemon: %s\n", st.State); err != nil {
		return err
	}
	if st.PID != 0 {
		if _, err := fmt.Fprintf(out, "  pid: %d\n", st.PID); err != nil {
			return err
		}
	}
	if st.Port != 0 {
		if _, err := fmt.Fprintf(out, "  port: %d\n", st.Port); err != nil {
			return err
		}
	}
	if st.StartedAt != nil && !st.StartedAt.IsZero() {
		if _, err := fmt.Fprintf(out, "  started: %s\n", st.StartedAt.Format(time.RFC3339)); err != nil {
			return err
		}
	}
	if st.Uptime != "" {
		if _, err := fmt.Fprintf(out, "  uptime: %s\n", st.Uptime); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(out, "  run file: %s\n", st.RunFile); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "  data dir: %s\n", st.DataDir); err != nil {
		return err
	}
	if st.Health != "" {
		if _, err := fmt.Fprintf(out, "  healthz: %s\n", st.Health); err != nil {
			return err
		}
	}
	if st.Ready != "" {
		if _, err := fmt.Fprintf(out, "  readyz: %s\n", st.Ready); err != nil {
			return err
		}
	}
	if st.InstanceID != "" {
		if _, err := fmt.Fprintf(out, "  instance: %s\n", st.InstanceID); err != nil {
			return err
		}
	}
	if st.Error != "" {
		if _, err := fmt.Fprintf(out, "  error: %s\n", st.Error); err != nil {
			return err
		}
	}
	for _, cand := range st.Candidates {
		line := fmt.Sprintf("  candidate: %s (%s", cand.RunFile, cand.State)
		if cand.PID != 0 {
			line += fmt.Sprintf(", pid %d", cand.PID)
		}
		if _, err := fmt.Fprintln(out, line+")"); err != nil {
			return err
		}
	}
	return nil
}

func formatUptime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}
