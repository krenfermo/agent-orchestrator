package daemon

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// The switches are read once, then no child the daemon starts may inherit
// them -- whichever exec path starts it -- while every composition-time reader
// still sees the operator's policy. Everything else in the env is untouched.
func TestResolveAndWithholdDaemonOnlySwitches(t *testing.T) {
	t.Setenv("AO_MEMORY_MODE", "assisted")
	t.Setenv("AO_MEMORY_EXTERNAL", "off")
	t.Setenv("AO_CONTEXT_ROUTER", "on")
	t.Setenv("AO_MEMORY_BUDGETS", "worker=1024/4")
	t.Setenv("AO_DATA_DIR", "/tmp/ao-switches-test")

	forget := resolveAndWithholdDaemonOnlySwitches(nil)

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

	// Readers after the withhold still get the resolved policy, not defaults.
	cfg := memoryConfig(nil)
	if cfg.Mode != "assisted" || cfg.ExternalContext {
		t.Errorf("memoryConfig after withhold = mode %q external %v, want assisted/false", cfg.Mode, cfg.ExternalContext)
	}
	if !contextRouterEnabled() {
		t.Error("contextRouterEnabled after withhold = false, want the resolved true")
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

	// Once RunWithConfig returns, the readers go back to the environment.
	forget()
	t.Setenv("AO_MEMORY_MODE", "off")
	if got := memoryConfig(nil).Mode; got != "off" {
		t.Errorf("memoryConfig after forget = %q, want the environment's off", got)
	}
}

// The list is exactly the switches whose values can differ between the arms of
// a memory A/B; the tmux runtime withholds the same three.
func TestDaemonOnlySwitchesAreTheArmNamingSwitches(t *testing.T) {
	want := []string{"AO_MEMORY_MODE", "AO_MEMORY_EXTERNAL", "AO_CONTEXT_ROUTER"}
	if !slices.Equal(daemonOnlySwitches, want) {
		t.Fatalf("daemonOnlySwitches = %v, want %v", daemonOnlySwitches, want)
	}
}
