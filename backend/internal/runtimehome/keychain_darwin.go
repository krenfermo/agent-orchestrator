//go:build darwin

package runtimehome

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
)

// ensureIsolatedKeychain provisions (idempotently) a per-user macOS login
// keychain rooted at env.RuntimeHome, so Claude Code's own OAuth token
// storage -- which on macOS resolves via the process's $HOME (macOS
// resolves the default keychain and search list from
// $HOME/Library/Keychains and $HOME/Library/Preferences, not from
// CLAUDE_CONFIG_DIR or any other env var Claude Code exposes) -- lands in
// this user's isolated runtime-home instead of either silently failing to
// persist (Checkpoint 8P-E.2: a HOME with no Library/Keychains resolves to
// an empty keychain search list, so `security add-generic-password` has
// nowhere to write and the write is dropped) or falling back onto the
// daemon host's own real login keychain, which would leak one AO user's
// provider credentials into another's isolated environment and violate
// Checkpoint 8P-B.
//
// Best-effort by design: every operation here is scoped to env.RuntimeHome
// via an explicit HOME override on the security(1) subprocess, so a
// failure never mutates the daemon host's real keychain search list, and
// never fails Prepare() itself -- it just leaves auth persistence in
// today's broken state, which the existing CLI auth probe already reports
// accurately.
func ensureIsolatedKeychain(env Environment) {
	keychainDir := filepath.Join(env.RuntimeHome, "Library", "Keychains")
	keychainPath := filepath.Join(keychainDir, "login.keychain-db")
	secretPath := filepath.Join(env.Root, ".keychain-secret")

	if err := os.MkdirAll(keychainDir, 0o700); err != nil {
		return
	}

	password, err := loadOrCreateKeychainSecret(secretPath)
	if err != nil {
		return
	}

	runSecurity := func(args ...string) error {
		cmd := exec.Command("security", args...)
		cmd.Env = []string{"HOME=" + env.RuntimeHome, "PATH=/usr/bin:/bin"}
		return cmd.Run()
	}

	if _, statErr := os.Stat(keychainPath); errors.Is(statErr, os.ErrNotExist) {
		if err := runSecurity("create-keychain", "-p", password, keychainPath); err != nil {
			return
		}
		// No idle timeout, no lock-on-sleep: AO's daemon must be able to
		// launch provider subprocesses unattended, long after the OAuth
		// login that first populated this keychain.
		_ = runSecurity("set-keychain-settings", keychainPath)
	}
	_ = runSecurity("unlock-keychain", "-p", password, keychainPath)
	_ = runSecurity("default-keychain", "-s", keychainPath)
	_ = runSecurity("list-keychains", "-d", "user", "-s", keychainPath, "/Library/Keychains/System.keychain")
}

// loadOrCreateKeychainSecret returns the password guarding this user's
// isolated login keychain, generating and persisting one on first use. This
// secret only ever unlocks an AO-created, per-user keychain holding that
// same user's own provider CLI tokens -- it is not the user's Anthropic
// credential itself and is never logged or returned to any caller.
func loadOrCreateKeychainSecret(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		return string(b), nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	password := base64.RawURLEncoding.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(password), 0o600); err != nil {
		return "", err
	}
	return password, nil
}
