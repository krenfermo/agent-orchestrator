package projectmemory

import "testing"

// AO_MEMORY_EXTERNAL (Frente 3 / 3D): external context is on by default, and
// an explicit off/false removes it; a malformed value is an error, never a
// silent default.
func TestConfigFromEnvExternalContextSwitch(t *testing.T) {
	for raw, want := range map[string]bool{"": true, "on": true, "true": true, "1": true, "off": false, "OFF": false, "false": false, "0": false} {
		t.Run("value="+raw, func(t *testing.T) {
			t.Setenv(ExternalEnv, raw)
			cfg, err := ConfigFromEnv()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ExternalContext != want {
				t.Fatalf("%s=%q -> ExternalContext=%v, want %v", ExternalEnv, raw, cfg.ExternalContext, want)
			}
		})
	}
	t.Setenv(ExternalEnv, "sometimes")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("a malformed AO_MEMORY_EXTERNAL must be rejected")
	}
	if !DefaultConfig().ExternalContext {
		t.Fatal("external context must stay on by default")
	}
}
