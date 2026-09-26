package daemon

import (
	"os"

	"github.com/aoagents/agent-orchestrator/backend/internal/contextrouter"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// daemonOnlySwitches are the dispatch-context switches the daemon reads once,
// at composition. Every reader (memoryConfig, contextrouter.Enabled) runs while
// RunWithConfig wires the daemon; the resolved values then travel explicitly,
// and what each dispatch received is frozen in its run's policy_snapshot.
//
// They mean nothing to any process the daemon starts afterwards, and their
// values name the arm of a memory A/B (AO_MEMORY_MODE=assisted). The tmux
// runtime already withholds them from agent panes; that covers only children
// started through tmux. Verification commands, provider probes and every
// other exec.Command inherit os.Environ, and a same-user process can read a
// child's environment from the process table (Frente 3 / 3D).
var daemonOnlySwitches = []string{
	projectmemory.ModeEnv,
	projectmemory.ExternalEnv,
	contextrouter.FlagEnv,
}

// withholdDaemonOnlySwitches removes daemonOnlySwitches from this process's
// environment so no child started from here on inherits them. It must run
// after the last composition-time read; running it earlier would silently
// resolve the defaults.
func withholdDaemonOnlySwitches(unsetenv func(string) error) {
	for _, name := range daemonOnlySwitches {
		_ = unsetenv(name)
	}
}

// withholdDaemonOnlySwitchesFromChildren is the production form.
func withholdDaemonOnlySwitchesFromChildren() { withholdDaemonOnlySwitches(os.Unsetenv) }
