package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// main_test.go -- the production-safety guardrail of the development daemon
// wrapper (Frente 3 incident, 2026-09-25): an invalid invocation, or one that
// names no data dir, must be refused BEFORE a daemon starts, so it can never
// open, create or migrate the operator's real database.

func env(vars map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := vars[k]
		return v, ok
	}
}

func TestWrapperRefusesBeforeStarting(t *testing.T) {
	explicit := map[string]string{"AO_DATA_DIR": "/scratch/data", "AO_RUN_FILE": "/scratch/running.json"}
	for name, tc := range map[string]struct {
		args []string
		env  map[string]string
	}{
		"version flag, the incident":      {[]string{"--version"}, explicit},
		"a CLI subcommand":                {[]string{"server", "--data-dir", "/scratch/data"}, explicit},
		"any positional argument":         {[]string{"daemon"}, explicit},
		"no data dir":                     {nil, map[string]string{"AO_RUN_FILE": "/scratch/running.json"}},
		"no run file":                     {nil, map[string]string{"AO_DATA_DIR": "/scratch/data"}},
		"blank data dir":                  {nil, map[string]string{"AO_DATA_DIR": "  ", "AO_RUN_FILE": "/scratch/running.json"}},
		"nothing set (the default homes)": {nil, map[string]string{}},
	} {
		t.Run(name, func(t *testing.T) {
			started := false
			err := run(tc.args, env(tc.env), func() error { started = true; return nil })
			if !errors.Is(err, errMisuse) {
				t.Fatalf("err = %v, want a misuse refusal", err)
			}
			if started {
				t.Fatal("the daemon was started despite the refusal")
			}
		})
	}
}

func TestWrapperStartsWithExplicitLocations(t *testing.T) {
	started := false
	err := run(nil, env(map[string]string{"AO_DATA_DIR": "/scratch/data", "AO_RUN_FILE": "/scratch/running.json"}),
		func() error { started = true; return nil })
	if err != nil || !started {
		t.Fatalf("explicit invocation: err=%v started=%v, want the daemon started", err, started)
	}
}

// TestWrapperProcessNeverTouchesDefaultDataDir runs the REAL main() in a child
// process whose HOME is a temporary directory, exactly as the incident did
// (`<binary> --version`, no AO_* environment). It must exit 2 quickly and leave
// no ~/.ao behind. If the guardrail ever regressed, the daemon it starts is
// confined to the temporary HOME and killed by the timeout -- the test can
// fail, but it can never reach the operator's real ~/.ao.
func TestWrapperProcessNeverTouchesDefaultDataDir(t *testing.T) {
	if os.Getenv("AO_WRAPPER_TEST_MAIN") == "1" {
		os.Args = append([]string{"ao-backend"}, filepath.SplitList(os.Getenv("AO_WRAPPER_TEST_ARGS"))...)
		if os.Getenv("AO_WRAPPER_TEST_ARGS") == "" {
			os.Args = []string{"ao-backend"}
		}
		main()
		return
	}
	for name, args := range map[string]string{"with --version": "--version", "bare": ""} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWrapperProcessNeverTouchesDefaultDataDir$")
			cmd.Env = []string{
				"HOME=" + home,
				"PATH=" + os.Getenv("PATH"),
				"AO_WRAPPER_TEST_MAIN=1",
				"AO_WRAPPER_TEST_ARGS=" + args,
			}
			out, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
				t.Fatalf("child err = %v (output %s), want exit code 2", err, out)
			}
			if ctx.Err() != nil {
				t.Fatal("the child ran until the timeout: a daemon started")
			}
			if _, statErr := os.Stat(filepath.Join(home, ".ao")); !os.IsNotExist(statErr) {
				t.Fatalf("the refused invocation created %s/.ao (stat err %v)", home, statErr)
			}
		})
	}
}
