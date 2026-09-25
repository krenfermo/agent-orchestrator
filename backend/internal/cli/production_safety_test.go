package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// production_safety_test.go -- Frente 3 incident (2026-09-25): a probe of a
// freshly built binary with `--version` booted a daemon on ~/.ao/data. The
// real CLI must answer every probe, typo and bare invocation WITHOUT starting
// a daemon: only the explicit daemon/server/start subcommands may. HOME and
// the AO_* locations point at a temporary directory, so even a regression
// here could never reach the operator's real state.
func TestProbesAndMisuseNeverStartADaemon(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AO_DATA_DIR", filepath.Join(home, "data"))
	t.Setenv("AO_RUN_FILE", filepath.Join(home, "running.json"))
	for name, tc := range map[string]struct {
		args    []string
		wantErr bool
		wantOut string
	}{
		"--version":       {[]string{"--version"}, false, "ao version"},
		"version":         {[]string{"version"}, false, ""},
		"--help":          {[]string{"--help"}, false, "Usage:"},
		"bare":            {nil, false, "Usage:"},
		"unknown flag":    {[]string{"--data-dirx", "/tmp/x"}, true, ""},
		"unknown command": {[]string{"serve"}, true, ""},
	} {
		t.Run(name, func(t *testing.T) {
			out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return false }}, tc.args...)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v (stderr %q), wantErr %v", err, errOut, tc.wantErr)
			}
			if tc.wantOut != "" && !strings.Contains(out, tc.wantOut) {
				t.Fatalf("output %q missing %q", out, tc.wantOut)
			}
			for _, p := range []string{filepath.Join(home, "data"), filepath.Join(home, ".ao"), filepath.Join(home, "running.json")} {
				if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
					t.Fatalf("%v created %s: a daemon or its state was started", tc.args, p)
				}
			}
		})
	}
}
