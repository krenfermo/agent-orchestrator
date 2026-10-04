package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
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
	add := func(p string) {
		if p == "" {
			return
		}
		for _, existing := range paths {
			if filepath.Clean(existing) == filepath.Clean(p) {
				return
			}
		}
		paths = append(paths, p)
	}
	if dataDir != "" {
		add(filepath.Join(dataDir, "running.json"))
	}
	// The default convention too: `ao server --data-dir X` names X/running.json,
	// and a daemon on the same data dir published under ~/.ao/running.json would
	// otherwise be invisible to it.
	if def, err := config.DefaultRunFilePath(); err == nil {
		add(def)
	}
	return paths
}

// liveDaemonForDataDir reports a daemon that is SERVING and serves dataDir (or
// is too old to say which, in which case a file this installation's paths lead
// to is taken as its own -- refusing to start is the safe error).
func liveDaemonForDataDir(client *http.Client, runFilePath, dataDir string) (*runfile.Info, string, error) {
	for i, path := range startupRunFileCandidates(runFilePath, dataDir) {
		live, err := runfile.CheckStale(path)
		if err != nil {
			// An unreadable candidate is not proof nothing serves this data
			// dir; a start that cannot read it refuses rather than guesses.
			return nil, "", fmt.Errorf("inspect run-file %s: %w", path, err)
		}
		if live == nil {
			continue
		}
		serving, servedDir := runFileDaemonServing(client, config.LoopbackHost, live)
		if !serving {
			continue
		}
		if i == 0 {
			// The CONFIGURED run-file is the one this daemon is about to write.
			// A live daemon behind it is refused whatever data dir it serves:
			// starting would overwrite its handshake (and exiting would delete
			// it), leaving that daemon undiscoverable.
			return live, path, nil
		}
		if servedDir != "" && !sameDataDirPath(servedDir, dataDir) {
			continue
		}
		return live, path, nil
	}
	return nil, "", nil
}

// sameDataDirPath compares two data dirs after resolving symlinks, so
// /tmp/ao and /private/tmp/ao on macOS are one installation, never two daemons
// on one database.
func sameDataDirPath(a, b string) bool {
	ca, cb := filepath.Clean(a), filepath.Clean(b)
	if ca == cb {
		return true
	}
	ea, errA := filepath.EvalSymlinks(ca)
	eb, errB := filepath.EvalSymlinks(cb)
	return errA == nil && errB == nil && ea == eb
}
