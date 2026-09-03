// Package providerpreflight answers, for one provider and one workspace,
// whether an UNATTENDED agent launch can start without asking an operator
// anything.
//
// It exists because of a real failure mode: AO spawned Claude workers into
// sessions that then sat forever on "Yes, I accept" (bypass permissions) and
// "Yes, I trust this folder". From AO's side the spawn succeeded — a process
// existed, a pane existed — and nothing was wrong except that the agent was
// never going to do anything, and nobody was there to press a key.
//
// Two rules govern everything here.
//
// It never answers a prompt. Piping "yes" into whatever a provider happens to
// ask is how an unattended agent accepts something nobody read. Trust and
// permission acceptance are recorded through each provider's OWN supported
// configuration, ahead of time; this reads that configuration to check they
// were.
//
// And what it cannot check is never a refusal. Every field it cannot determine
// comes back Unknown, which the workflow package treats as ready — grounding
// every dispatch on an inconclusive probe would be strictly worse than the
// incident this exists for. The cost of a wrong "unknown" is that AO fails to
// warn; the cost of a wrong "not ready" is that AO stops working.
package providerpreflight

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// Checker implements workflowcore.WorkerPreflight.
type Checker struct {
	// Agents resolves harness adapters, so each provider's OWN binary and auth
	// probes are used rather than a second, divergent idea of "is this provider
	// installed". Optional: without it the binary and auth answers are Unknown
	// rather than negative.
	Agents ports.AgentResolver
	// Probe bounds any subprocess the auth check shells out to. A probe that
	// hangs must not hold a dispatch.
	Probe time.Duration
}

// Preflight reports readiness for one unattended launch.
func (c *Checker) Preflight(ctx context.Context, req workflowcore.WorkerPreflightRequest) (workflowcore.WorkerPreflightResult, error) {
	res := workflowcore.WorkerPreflightResult{
		// Every answer starts at "AO could not check". Each probe below
		// downgrades one of them only when it actually learned something.
		BinaryOK: true, AuthOK: true, AuthUnknown: true,
		TrustOK: true, TrustUnknown: true,
		PermissionModeOK: true, PermissionModeUnknown: true,
	}
	probe := c.Probe
	if probe <= 0 {
		probe = 8 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, probe)
	defer cancel()

	var notes []string
	agent, haveAgent := c.agentFor(req.Harness)

	// 1. The binary. This is the one answer that can be negative with
	//    certainty: a CLI that does not resolve cannot be launched at all.
	if haveAgent {
		if resolver, ok := agent.(ports.AgentBinaryResolver); ok {
			if path, err := resolver.ResolveBinary(ctx); err != nil {
				res.BinaryOK = false
				notes = append(notes, "binary: "+err.Error())
			} else if path != "" {
				notes = append(notes, "binary: "+path)
			}
		}
	}
	if res.BinaryOK && !haveAgent {
		if _, err := exec.LookPath(string(req.Harness)); err == nil {
			notes = append(notes, "binary: found on PATH")
		}
	}

	// 2. Credentials. Only an AFFIRMATIVE "unauthorized" is a refusal; a probe
	//    that cannot tell leaves AuthUnknown set.
	if haveAgent {
		if checker, ok := agent.(ports.AgentAuthChecker); ok {
			switch status, err := checker.AuthStatus(ctx); {
			case err != nil:
				notes = append(notes, "auth: could not be checked ("+err.Error()+")")
			case status == ports.AgentAuthStatusUnauthorized:
				res.AuthOK, res.AuthUnknown = false, false
				notes = append(notes, "auth: the provider reports its credentials are not usable")
			case status == ports.AgentAuthStatusAuthorized:
				res.AuthUnknown = false
				notes = append(notes, "auth: ok")
			}
		}
	}

	// 3. The two interactive startup gates AO does not otherwise cover, read
	//    from the provider's own configuration file — the same one the spawned
	//    process will read, resolved through the same isolated CLAUDE_CONFIG_DIR
	//    the launch will export.
	if isClaudeFamily(req.Harness) {
		cfg, ok := readClaudeConfig(req.RuntimeEnv)
		if ok {
			// The bypass-permissions acceptance is global and is exactly the
			// "Yes, I accept" dialog an unattended session cannot pass. AO does
			// not write it: accepting a permissions posture on a person's behalf
			// is precisely the decision this package refuses to make.
			if accepted, known := cfg.bool("bypassPermissionsModeAccepted"); known {
				res.PermissionModeUnknown = false
				res.PermissionModeOK = accepted
				if !accepted {
					notes = append(notes, "the bypass-permissions posture has never been accepted in this provider's configuration, so the first launch would open its \"Yes, I accept\" dialog")
				}
			}
			// Workspace trust is path-scoped. AO's claude-code adapter records
			// trust for the launch workspace itself in PreLaunch, so the only
			// path worth checking here is one that already exists and will not
			// get that write — an incident or repair workspace.
			//
			// TrustRecordedAtLaunch is the caller saying which it is, and it is
			// checked here rather than left implicit because "will not get that
			// write" was previously an assumption about the caller that the one
			// real caller did not satisfy: a worker dispatch's launch DOES
			// record trust, so refusing it was refusing a condition that was
			// about to stop being true.
			if req.TrustRecordedAtLaunch {
				notes = append(notes,
					"trust: not checked — this launch records the workspace's trust itself before the agent starts")
			} else if req.WorkspacePath != "" && dirExists(req.WorkspacePath) {
				trusted, known := cfg.projectBool(req.WorkspacePath, "hasTrustDialogAccepted")
				if known {
					res.TrustUnknown = false
					res.TrustOK = trusted
					if !trusted {
						notes = append(notes, "no trust record for "+req.WorkspacePath)
					}
				}
			}
		}
	}

	res.Detail = strings.Join(notes, "; ")
	return res, nil
}

func (c *Checker) agentFor(harness domain.AgentHarness) (ports.Agent, bool) {
	if c.Agents == nil || harness == "" {
		return nil, false
	}
	return c.Agents.Agent(harness)
}

// isClaudeFamily reports whether this harness uses Claude Code's ~/.claude.json
// configuration. Deliberately a narrow match: a harness AO does not recognise
// simply produces Unknown answers, which is ready.
func isClaudeFamily(harness domain.AgentHarness) bool {
	return harness == "claude-code"
}

// claudeConfig is the parsed provider configuration, read-only.
type claudeConfig map[string]any

func (c claudeConfig) bool(key string) (value, known bool) {
	v, ok := c[key].(bool)
	return v, ok
}

func (c claudeConfig) projectBool(path, key string) (value, known bool) {
	projects, ok := c["projects"].(map[string]any)
	if !ok {
		return false, false
	}
	entry, ok := projects[path].(map[string]any)
	if !ok {
		// The project has no entry at all, which for a path-scoped trust record
		// means "not trusted" rather than "unknown": the file was readable and
		// simply does not name this directory.
		return false, true
	}
	v, ok := entry[key].(bool)
	if !ok {
		return false, true
	}
	return v, true
}

// readClaudeConfig resolves and parses the configuration file the launch will
// actually read: the isolated per-user CLAUDE_CONFIG_DIR when the runtime env
// carries one (Checkpoint 8P-B's per-user runtime-home isolation), otherwise
// the daemon's own ~/.claude.json. Reading the wrong file would answer about a
// configuration the worker never sees, which is worse than not answering.
func readClaudeConfig(env map[string]string) (claudeConfig, bool) {
	path := ""
	if dir := strings.TrimSpace(env["CLAUDE_CONFIG_DIR"]); dir != "" {
		path = filepath.Join(dir, ".claude.json")
	} else if home, err := os.UserHomeDir(); err == nil {
		path = filepath.Join(home, ".claude.json")
	}
	if path == "" {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	cfg := claudeConfig{}
	if json.Unmarshal(data, &cfg) != nil {
		return nil, false
	}
	return cfg, true
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
