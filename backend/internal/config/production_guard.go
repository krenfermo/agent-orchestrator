package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// production_guard.go -- Frente 3 incident (2026-09-25), human decision
// "option 1": the DEFAULT data dir (~/.ao/data) holds the operator's real
// state, and an experimental build launched by hand must never fall into it
// silently. Opening it to run the daemon or an offline writer (anything that
// can migrate the database) is allowed only when
//
//	A) the process was launched by the desktop app, proven by the launch
//	   contract Electron already follows (frontend/src/main.ts daemonEnv):
//	   AO_OWNER is exactly "app" or "persistent" AND AO_APP_RUN_ID is set; or
//	B) the data dir was named explicitly (AO_DATA_DIR, or --data-dir).
//
// AO_OWNER is not a secret and this is not authentication: it is a
// fail-closed check that the default was reached on purpose. Any other
// AO_OWNER value -- including a merely non-empty one -- does not count.

// ErrDefaultDataDirUnauthorized refuses a start on the default data dir.
var ErrDefaultDataDirUnauthorized = errors.New("refusing to use the default AO data dir")

// desktopOwners are the AO_OWNER values the desktop app sets for a daemon it
// launches ("app": app-owned; "persistent": keep-alive).
var desktopOwners = map[string]bool{"app": true, "persistent": true}

// DesktopLaunchContract reports whether lookupEnv carries the desktop app's
// launch contract: AO_OWNER exactly "app"/"persistent" and a non-empty
// AO_APP_RUN_ID. Both are set together by the one Electron code path that
// spawns the daemon.
func DesktopLaunchContract(lookupEnv func(string) (string, bool)) bool {
	owner, _ := lookupEnv("AO_OWNER")
	runID, _ := lookupEnv("AO_APP_RUN_ID")
	return desktopOwners[owner] && strings.TrimSpace(runID) != ""
}

// DefaultDataDir is where the data dir resolves when nothing names one.
func DefaultDataDir() (string, error) {
	stateDir, err := defaultStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(stateDir, "data"), nil
}

// AuthorizeDefaultDataDir returns nil when cfg may open its data dir, and
// ErrDefaultDataDirUnauthorized when it would fall into the default data dir
// without either the desktop launch contract or an explicit choice. It
// touches nothing on disk, so it runs before anything is created or migrated.
func AuthorizeDefaultDataDir(cfg Config, lookupEnv func(string) (string, bool)) error {
	if cfg.DataDirExplicit {
		return nil
	}
	def, err := DefaultDataDir()
	if err != nil {
		return err
	}
	if filepath.Clean(cfg.DataDir) != filepath.Clean(def) {
		// Not the default (only reachable through an explicit choice).
		return nil
	}
	if DesktopLaunchContract(lookupEnv) {
		return nil
	}
	return fmt.Errorf("%w %s: it holds your real AO state and this process was neither launched by the desktop app "+
		"(AO_OWNER=app|persistent with AO_APP_RUN_ID) nor given an explicit data dir; set AO_DATA_DIR "+
		"(or pass --data-dir to `ao server`) to choose one on purpose", ErrDefaultDataDirUnauthorized, def)
}
