//go:build !darwin

package runtimehome

// ensureIsolatedKeychain is a no-op off macOS: Claude Code has no OS
// keychain to resolve there (per Checkpoint 8P-E.2's investigation, it
// falls back to a credentials file under CLAUDE_CONFIG_DIR/HOME, which the
// existing HOME/XDG_*/CLAUDE_CONFIG_DIR overrides in SubprocessEnv already
// isolate correctly -- see runtimehome.go).
func ensureIsolatedKeychain(env Environment) {}
