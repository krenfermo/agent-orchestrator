package daemon

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// After composition, no child the daemon starts may inherit the arm-naming
// switches -- whichever exec path starts it. Everything else is untouched.
func TestWithholdDaemonOnlySwitchesKeepsThemFromEveryChild(t *testing.T) {
	t.Setenv("AO_MEMORY_MODE", "assisted")
	t.Setenv("AO_MEMORY_EXTERNAL", "off")
	t.Setenv("AO_CONTEXT_ROUTER", "on")
	t.Setenv("AO_MEMORY_BUDGETS", "worker=1024/4")
	t.Setenv("AO_DATA_DIR", "/tmp/ao-switches-test")

	// Resolved before the withhold: composition sees the operator's policy.
	if got := memoryConfig(nil).Mode; got != "assisted" {
		t.Fatalf("composition read memory mode %q, want assisted", got)
	}

	withholdDaemonOnlySwitchesFromChildren()

	for _, name := range []string{"AO_MEMORY_MODE", "AO_MEMORY_EXTERNAL", "AO_CONTEXT_ROUTER"} {
		if v, ok := os.LookupEnv(name); ok {
			t.Errorf("%s still in the daemon environment (%q)", name, v)
		}
	}
	for name, want := range map[string]string{"AO_MEMORY_BUDGETS": "worker=1024/4", "AO_DATA_DIR": "/tmp/ao-switches-test"} {
		if got := os.Getenv(name); got != want {
			t.Errorf("%s = %q, want it untouched (%q)", name, got, want)
		}
	}

	// A plain exec.Command child -- no tmux sanitisation on this path.
	out, err := exec.Command("/usr/bin/env").Output()
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		name, _, _ := strings.Cut(line, "=")
		if slices.Contains(daemonOnlySwitches, name) {
			t.Errorf("child inherited %s", line)
		}
	}
}

// The list is exactly the switches whose values can differ between the arms of
// a memory A/B, and the tmux runtime's own list names the same three.
func TestDaemonOnlySwitchesAreTheArmNamingSwitches(t *testing.T) {
	want := []string{"AO_MEMORY_MODE", "AO_MEMORY_EXTERNAL", "AO_CONTEXT_ROUTER"}
	if !slices.Equal(daemonOnlySwitches, want) {
		t.Fatalf("daemonOnlySwitches = %v, want %v", daemonOnlySwitches, want)
	}
}
