package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

// P9 — daemon discovery that proves which daemon it found.
//
// Two run-file conventions exist on a real machine: the default
// (~/.ao/running.json beside ~/.ao/data) and `ao server --data-dir X`, which
// writes X/running.json. A CLI that reads only one of them reports STOPPED over a
// daemon that is serving the very data dir it means (seen during migration 0170).
// So every status / stop decision looks at BOTH canonical locations for this
// installation, and trusts neither a path nor a PID:
//
//   - a run-file that names ANOTHER data dir is `foreign` -- another
//     installation's daemon, never acted on;
//   - a PID that is not alive is `stale`, and its file is removed only while it
//     still names that exact incarnation (runfile.RemoveIfMatches);
//   - a live PID whose probe does not answer is `unhealthy`; one whose probe
//     answers as a different incarnation or data dir is `running_unverified` --
//     both are RUNNING BUT UNVERIFIED, never stopped and never signalled;
//   - only a probe that answers with the run-file's PID, instance and this data
//     dir is a verified daemon.
//
// Nothing here ever sends a signal or kills a process. The only way `ao stop`
// ends a daemon is the daemon's own /shutdown, asked of a verified one.

const (
	stateUnverified daemonState = "running_unverified"
	stateForeign    daemonState = "foreign"
)

// runFileCandidate is one inspected location, kept for the report.
type runFileCandidate struct {
	RunFile string      `json:"runFile"`
	State   daemonState `json:"state"`
	PID     int         `json:"pid,omitempty"`
	Error   string      `json:"error,omitempty"`

	status daemonStatus
	info   *runfile.Info
}

// candidateRunFiles are the canonical run-file locations for cfg's installation,
// deduplicated, primary first.
func candidateRunFiles(cfg config.Config) []string {
	paths := []string{cfg.RunFilePath}
	if cfg.DataDir != "" {
		alt := filepath.Join(cfg.DataDir, "running.json")
		if filepath.Clean(alt) != filepath.Clean(cfg.RunFilePath) {
			paths = append(paths, alt)
		}
	}
	return paths
}

// discoverDaemon inspects every candidate and decides which daemon, if any, is
// this installation's.
func (c *commandContext) discoverDaemon(ctx context.Context, cfg config.Config) (daemonStatus, []runFileCandidate, error) {
	var cands []runFileCandidate
	for _, path := range candidateRunFiles(cfg) {
		st, info, err := c.inspectRunFile(ctx, cfg, path)
		if err != nil {
			return daemonStatus{}, nil, err
		}
		cands = append(cands, runFileCandidate{RunFile: path, State: st.State, PID: st.PID, Error: st.Error, status: st, info: info})
	}

	var verified []runFileCandidate
	for _, cand := range cands {
		if cand.status.owned && !sameDaemonAs(verified, cand) {
			verified = append(verified, cand)
		}
	}
	if len(verified) > 1 {
		var names []string
		for _, v := range verified {
			names = append(names, fmt.Sprintf("pid %d via %s", v.PID, v.RunFile))
		}
		return daemonStatus{}, cands, fmt.Errorf("two AO daemons claim data dir %s (%s); refusing to guess which one is this installation's",
			cfg.DataDir, strings.Join(names, ", "))
	}

	var chosen runFileCandidate
	switch {
	case len(verified) == 1:
		chosen = verified[0]
	default:
		chosen = cands[0]
		for _, cand := range cands[1:] {
			if statePriority(cand.State) > statePriority(chosen.State) {
				chosen = cand
			}
		}
	}
	out := chosen.status
	if presentCount(cands) > 1 {
		out.Candidates = cands
	}
	return out, cands, nil
}

// statePriority orders what a person must be told first when no candidate is
// verified: something alive outranks something foreign outranks a stale file.
func statePriority(s daemonState) int {
	switch s {
	case stateUnverified:
		return 5
	case stateUnhealthy, stateNotReady:
		return 4
	case stateForeign:
		return 3
	case stateStale:
		return 2
	case stateStopped:
		return 0
	default:
		return 1
	}
}

func presentCount(cands []runFileCandidate) int {
	n := 0
	for _, cand := range cands {
		if cand.State != stateStopped {
			n++
		}
	}
	return n
}

func sameDaemonAs(verified []runFileCandidate, cand runFileCandidate) bool {
	for _, v := range verified {
		if v.info != nil && cand.info != nil && v.info.SameDaemon(*cand.info) {
			return true
		}
	}
	return false
}

// inspectRunFile is the per-location verdict.
func (c *commandContext) inspectRunFile(ctx context.Context, cfg config.Config, path string) (daemonStatus, *runfile.Info, error) {
	st := daemonStatus{State: stateStopped, RunFile: path, DataDir: cfg.DataDir}
	info, err := runfile.Read(path)
	if err != nil {
		return daemonStatus{}, nil, err
	}
	if info == nil {
		return st, nil, nil
	}
	st.PID = info.PID
	st.Port = info.Port
	startedAt := info.StartedAt
	st.StartedAt = &startedAt
	st.Uptime = formatUptime(c.deps.Now().Sub(info.StartedAt))
	st.InstanceID = info.InstanceID
	st.InstallationID = info.InstallationID

	if info.DataDir != "" && !sameDataDir(info.DataDir, cfg.DataDir) {
		st.State = stateForeign
		st.Error = fmt.Sprintf("run-file belongs to data dir %s, not %s", info.DataDir, cfg.DataDir)
		return st, info, nil
	}
	if !c.deps.ProcessAlive(info.PID) {
		st.State = stateStale
		st.Error = "run-file points to a dead process"
		return st, info, nil
	}

	health, err := c.readProbe(ctx, info.Port, "healthz")
	if err != nil {
		st.State = stateUnhealthy
		st.Error = err.Error()
		return st, info, nil
	}
	if err := verifyProbeOwner(health, info.PID, "healthz"); err != nil {
		st.State = stateStale
		st.Error = err.Error()
		return st, info, nil
	}
	if reason := probeIdentityMismatch(health, info, cfg.DataDir); reason != "" {
		st.State = stateUnverified
		if health.DataDir != "" && !sameDataDir(health.DataDir, cfg.DataDir) {
			st.State = stateForeign
		}
		st.Error = "healthz: " + reason
		return st, info, nil
	}
	st.owned = true
	st.Health = health.Status
	if health.Status != "ok" {
		st.State = stateUnhealthy
		return st, info, nil
	}

	ready, err := c.readProbe(ctx, info.Port, "readyz")
	if err != nil {
		st.State = stateNotReady
		st.Error = err.Error()
		return st, info, nil
	}
	if err := verifyProbeOwner(ready, info.PID, "readyz"); err != nil {
		st.State = stateStale
		st.owned = false
		st.Error = err.Error()
		return st, info, nil
	}
	if reason := probeIdentityMismatch(ready, info, cfg.DataDir); reason != "" {
		st.State = stateUnverified
		st.owned = false
		st.Error = "readyz: " + reason
		return st, info, nil
	}
	st.Ready = ready.Status
	if ready.Status == string(stateReady) {
		st.State = stateReady
		return st, info, nil
	}
	st.State = stateNotReady
	return st, info, nil
}

// probeIdentityMismatch compares what the daemon SAYS it is against the run-file
// and the data dir the caller means. A daemon too old to say is not a mismatch
// (it is still verified by PID + service, as before P9); a stated disagreement is.
func probeIdentityMismatch(probe probeResult, info *runfile.Info, dataDir string) string {
	if probe.DataDir != "" && !sameDataDir(probe.DataDir, dataDir) {
		return fmt.Sprintf("the daemon serves data dir %s, not %s", probe.DataDir, dataDir)
	}
	if probe.InstanceID != "" && info.InstanceID != "" && probe.InstanceID != info.InstanceID {
		return fmt.Sprintf("the daemon answering is incarnation %s, but the run-file names %s", probe.InstanceID, info.InstanceID)
	}
	if probe.InstallationID != "" && info.InstallationID != "" && probe.InstallationID != info.InstallationID {
		return fmt.Sprintf("the daemon answering is installation %s, but the run-file names %s", probe.InstallationID, info.InstallationID)
	}
	return ""
}

func sameDataDir(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	ca, cb := filepath.Clean(a), filepath.Clean(b)
	if ca == cb {
		return true
	}
	ea, errA := filepath.EvalSymlinks(ca)
	eb, errB := filepath.EvalSymlinks(cb)
	return errA == nil && errB == nil && ea == eb
}
