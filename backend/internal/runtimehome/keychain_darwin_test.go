//go:build darwin

package runtimehome

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// securityAvailable skips keychain tests on a machine/CI runner where the
// security(1) CLI isn't usable (e.g. no Keychain services at all) rather
// than failing the whole suite on an environment problem unrelated to the
// code under test.
func securityAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("security"); err != nil {
		t.Skip("security(1) not available on this runner")
	}
}

func addDummyCredential(t *testing.T, env Environment, account, value string) error {
	t.Helper()
	cmd := exec.Command("security", "add-generic-password", "-a", account, "-s", "Claude Code-credentials", "-w", value)
	cmd.Env = []string{"HOME=" + env.RuntimeHome, "PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("add-generic-password failed: %s", out)
	}
	return err
}

func findDummyCredential(env Environment, account string) bool {
	cmd := exec.Command("security", "find-generic-password", "-a", account, "-s", "Claude Code-credentials")
	cmd.Env = []string{"HOME=" + env.RuntimeHome, "PATH=/usr/bin:/bin"}
	return cmd.Run() == nil
}

// TestEnsureIsolatedKeychain_PersistsAcrossRepeatedPrepare is Checkpoint
// 8P-E.2's root-cause regression: on macOS, Claude Code stores OAuth
// credentials via the OS keychain, which it resolves through the
// subprocess's $HOME rather than CLAUDE_CONFIG_DIR. Before this checkpoint,
// a HOME pointed at an AO-isolated runtime-home had no Library/Keychains,
// so the keychain search list was empty and any credential write was
// silently dropped -- "Login successful" followed immediately by
// loggedIn:false. This asserts a credential written under one Prepare()
// call is still readable after a fresh Prepare() call for the same user
// (simulating an AO daemon restart), using a dummy value that is never a
// real credential.
func TestEnsureIsolatedKeychain_PersistsAcrossRepeatedPrepare(t *testing.T) {
	securityAvailable(t)
	dataDir := t.TempDir()
	userID := domain.UserID("persist-user")

	envFirst, err := Prepare(dataDir, userID)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := addDummyCredential(t, envFirst, "probe-account", "dummy-not-a-real-token"); err != nil {
		t.Fatalf("seed dummy credential: %v", err)
	}

	// Re-running Prepare (as every probe/session launch does) must not
	// recreate or relock the keychain out from under an existing login.
	envSecond, err := Prepare(dataDir, userID)
	if err != nil {
		t.Fatalf("prepare (restart): %v", err)
	}
	if envSecond.RuntimeHome != envFirst.RuntimeHome {
		t.Fatalf("runtime home changed across Prepare calls for the same user")
	}
	if !findDummyCredential(envSecond, "probe-account") {
		t.Fatal("credential did not persist across a second Prepare() call (simulated daemon restart)")
	}
}

// TestEnsureIsolatedKeychain_IsolatesUsers proves a credential written to
// user A's isolated keychain is never visible when reading through user
// B's isolated environment, and vice versa -- the multiuser guarantee
// Checkpoint 8P-B established must hold for the keychain layer added here
// too.
func TestEnsureIsolatedKeychain_IsolatesUsers(t *testing.T) {
	securityAvailable(t)
	dataDir := t.TempDir()

	envA, err := Prepare(dataDir, domain.UserID("kc-user-a"))
	if err != nil {
		t.Fatalf("prepare A: %v", err)
	}
	envB, err := Prepare(dataDir, domain.UserID("kc-user-b"))
	if err != nil {
		t.Fatalf("prepare B: %v", err)
	}

	if err := addDummyCredential(t, envA, "shared-account-name", "dummy-a-not-a-real-token"); err != nil {
		t.Fatalf("seed A: %v", err)
	}

	if !findDummyCredential(envA, "shared-account-name") {
		t.Fatal("user A cannot read back its own credential")
	}
	if findDummyCredential(envB, "shared-account-name") {
		t.Fatal("user B's isolated keychain unexpectedly exposes user A's credential")
	}
}

// TestEnsureIsolatedKeychain_DoesNotTouchHostKeychainSearchList guards
// against the fix regressing into the "just widen the search list"
// mistake: preparing a runtime-home must never change the real host
// account's own keychain search list or default keychain, since those are
// process-wide OS state Prepare() has no business mutating.
func TestEnsureIsolatedKeychain_DoesNotTouchHostKeychainSearchList(t *testing.T) {
	securityAvailable(t)
	before, err := exec.Command("security", "list-keychains").CombinedOutput()
	if err != nil {
		t.Skip("cannot read host keychain search list on this runner")
	}

	dataDir := t.TempDir()
	if _, err := Prepare(dataDir, domain.UserID("host-guard-user")); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	after, err := exec.Command("security", "list-keychains").CombinedOutput()
	if err != nil {
		t.Fatalf("list-keychains after prepare: %v", err)
	}
	if strings.TrimSpace(string(before)) != strings.TrimSpace(string(after)) {
		t.Fatalf("Prepare() mutated the host's real keychain search list:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
