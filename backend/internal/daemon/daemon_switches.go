package daemon

import (
	"log/slog"
	"os"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/contextrouter"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// daemonOnlySwitches are the dispatch-context switches only the daemon reads,
// at composition. The resolved values then travel explicitly, and what each
// dispatch received is frozen in its run's policy_snapshot.
//
// They mean nothing to any process the daemon starts, and their values name
// the arm of a memory A/B (AO_MEMORY_MODE=assisted). The tmux runtime already
// withholds them from agent panes; that covers only children started through
// tmux. Verification commands, provider probes and every other exec.Command
// inherit os.Environ, and a same-user process can read a child's environment
// from the process table (Frente 3 / 3D).
var daemonOnlySwitches = []string{
	projectmemory.ModeEnv,
	projectmemory.ExternalEnv,
	contextrouter.FlagEnv,
}

// resolvedSwitches holds what resolveAndWithholdDaemonOnlySwitches read, for
// the lifetime of one RunWithConfig. Nil fields mean "not resolved": the
// readers then fall back to the environment, which is what tests and any
// caller outside RunWithConfig get.
var resolvedSwitches struct {
	sync.Mutex
	memory *projectmemory.Config
	router *bool
}

// resolveAndWithholdDaemonOnlySwitches reads the switches ONCE, before the
// daemon starts any child, and then removes them from this process's
// environment so that no child ever inherits them. Every later reader
// (memoryConfig, contextRouterEnabled) returns the resolved values. The
// returned func forgets them; RunWithConfig defers it.
func resolveAndWithholdDaemonOnlySwitches(log *slog.Logger) (forget func()) {
	mem := readMemoryConfig(log)
	router := contextrouter.Enabled()
	resolvedSwitches.Lock()
	resolvedSwitches.memory, resolvedSwitches.router = &mem, &router
	resolvedSwitches.Unlock()
	withholdDaemonOnlySwitches(os.Unsetenv)
	return func() {
		resolvedSwitches.Lock()
		resolvedSwitches.memory, resolvedSwitches.router = nil, nil
		resolvedSwitches.Unlock()
	}
}

func withholdDaemonOnlySwitches(unsetenv func(string) error) {
	for _, name := range daemonOnlySwitches {
		_ = unsetenv(name)
	}
}

// contextRouterEnabled is contextrouter.Enabled as resolved at composition.
func contextRouterEnabled() bool {
	resolvedSwitches.Lock()
	defer resolvedSwitches.Unlock()
	if resolvedSwitches.router != nil {
		return *resolvedSwitches.router
	}
	return contextrouter.Enabled()
}

func resolvedMemoryConfig() (projectmemory.Config, bool) {
	resolvedSwitches.Lock()
	defer resolvedSwitches.Unlock()
	if resolvedSwitches.memory != nil {
		return *resolvedSwitches.memory, true
	}
	return projectmemory.Config{}, false
}
