package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/daemonmeta"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

// staleProbeTimeout bounds the startup ownership probe so a run-file pointing at
// an unreachable port cannot stall daemon startup.
const staleProbeTimeout = 2 * time.Second

// runFileOwnerServing reports whether an AO daemon matching info is actually
// serving on the recorded loopback port.
//
// runfile.CheckStale only confirms the recorded PID is alive, which is not
// enough to conclude a predecessor still owns the port. On Windows the desktop
// supervisor can only TerminateProcess the daemon (no POSIX signal reaches it),
// so the daemon's graceful shutdown never runs and running.json is never
// removed; the leaked file then survives into the next launch. Because Windows
// reuses PIDs aggressively, the recorded PID routinely belongs to an unrelated
// process, making the PID-only check report "alive" for a daemon that is long
// gone — which is what made the daemon refuse to start (issue #256).
//
// Probing /healthz and matching both the service name and the PID is the ground
// truth that a real predecessor is still listening. When it is not, the
// run-file is stale and the caller should overwrite it instead of refusing.
func runFileOwnerServing(client *http.Client, host string, info *runfile.Info) bool {
	serving, _ := runFileDaemonServing(client, host, info)
	return serving
}

// runFileDaemonServing is runFileOwnerServing that also reports the data dir the
// serving daemon says it owns ("" for a pre-P9 daemon that does not say).
func runFileDaemonServing(client *http.Client, host string, info *runfile.Info) (bool, string) {
	if info == nil || info.Port <= 0 {
		return false, ""
	}

	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://%s:%d/healthz", host, info.Port), http.NoBody)
	if err != nil {
		return false, ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, ""
	}

	var body struct {
		Service string `json:"service"`
		PID     int    `json:"pid"`
		DataDir string `json:"dataDir"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, ""
	}
	return body.Service == daemonmeta.ServiceName && body.PID == info.PID, body.DataDir
}

// startupRunFileCandidates are the run-file locations a daemon for this data
// dir may have published (P9): the configured one, and <data dir>/running.json,
// which `ao server --data-dir` writes.
func startupRunFileCandidates(runFilePath, dataDir string) []string {
	paths := []string{runFilePath}
	if dataDir != "" {
		if alt := filepath.Join(dataDir, "running.json"); filepath.Clean(alt) != filepath.Clean(runFilePath) {
			paths = append(paths, alt)
		}
	}
	return paths
}

// liveDaemonForDataDir reports a daemon that is SERVING and serves dataDir (or
// is too old to say which, in which case a file this installation's paths lead
// to is taken as its own -- refusing to start is the safe error).
func liveDaemonForDataDir(client *http.Client, host, runFilePath, dataDir string) (*runfile.Info, string, error) {
	for i, path := range startupRunFileCandidates(runFilePath, dataDir) {
		live, err := runfile.CheckStale(path)
		if err != nil {
			if i == 0 {
				return nil, "", err
			}
			continue
		}
		if live == nil {
			continue
		}
		serving, servedDir := runFileDaemonServing(client, host, live)
		if !serving {
			continue
		}
		if servedDir != "" && filepath.Clean(servedDir) != filepath.Clean(dataDir) {
			continue
		}
		return live, path, nil
	}
	return nil, "", nil
}
