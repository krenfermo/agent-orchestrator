// Command backend is a compatibility wrapper for the Agent Orchestrator daemon.
// The user-facing CLI lives at cmd/ao; keep this wrapper so existing `go run .`
// development workflows continue to start the daemon while scripts migrate.
//
// PRODUCTION SAFETY (Frente 3 incident, 2026-09-25). This wrapper used to
// ignore its arguments and start a daemon on the DEFAULT data dir. A binary
// built from here and probed with `--version` therefore booted on ~/.ao/data --
// the operator's real database -- and applied an unmerged migration to it. The
// wrapper now fails closed: it accepts no arguments at all, and it never falls
// back to the default data dir or run-file. Both must be named explicitly, so
// a development daemon can only ever touch the state someone chose for it.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/daemon"
)

// errMisuse marks an invocation refused before anything was started.
var errMisuse = errors.New("refused")

// requiredExplicitEnv are the locations this wrapper refuses to default.
var requiredExplicitEnv = []string{"AO_DATA_DIR", "AO_RUN_FILE"}

func main() {
	if err := run(os.Args[1:], os.LookupEnv, daemon.Run); err != nil {
		fmt.Fprintln(os.Stderr, "ao backend daemon: "+err.Error())
		if errors.Is(err, errMisuse) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// run validates the invocation and only then calls start. Nothing is opened,
// created or migrated before validation passes.
func run(args []string, lookupEnv func(string) (string, bool), start func() error) error {
	if len(args) > 0 {
		return fmt.Errorf("%w: this development daemon wrapper takes no arguments (got %q). "+
			"It is not the ao CLI; use `go run ./cmd/ao <command>` (e.g. `go run ./cmd/ao version`)", errMisuse, args)
	}
	var missing []string
	for _, name := range requiredExplicitEnv {
		if v, ok := lookupEnv(name); !ok || strings.TrimSpace(v) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s must be set explicitly. This development wrapper never falls back to the "+
			"default ~/.ao/data or ~/.ao/running.json, which hold real data. Example: "+
			"AO_DATA_DIR=$HOME/.ao/dev/data AO_RUN_FILE=$HOME/.ao/dev/running.json go run .",
			errMisuse, strings.Join(missing, " and "))
	}
	return start()
}
