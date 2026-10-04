//go:build darwin

package runtimehome

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// keychain_reserved_name_darwin_test.go — the keychain incident, pinned.
//
// A real workflow stopped at planner_auth_unavailable while macOS asked for
// the password of a keychain called "login". The keychain was AO's own, its
// password was a random secret AO had generated, and AO could not open it
// either: `security create-keychain -p PW .../login.keychain-db` succeeds and
// `security unlock-keychain -p PW` on the same file then fails with "The user
// name or passphrase you entered is not correct", because macOS reserves that
// name for the user's real login keychain.
//
// The bug hid for weeks because create-keychain leaves the new keychain
// unlocked for the rest of the securityd session: everything worked until the
// first sleep or reboot, after which the keychain was permanently unopenable
// and every provider launch that reached for a credential raised a dialog no
// unattended run could answer.
//
// So the load-bearing test is not "can AO write a credential" (that passed
// throughout the incident). It is "after the keychain LOCKS, can AO open it
// again with the secret it stored" -- which is the one thing the old name
// could never do.

func TestPrepare_KeychainReopensAfterLock(t *testing.T) {
	securityAvailable(t)
	dataDir := t.TempDir()
	env, err := Prepare(dataDir, domain.UserID("relock-user"))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if env.Keychain.State != KeychainOK {
		t.Fatalf("prepare left the keychain unusable: state=%s detail=%s", env.Keychain.State, env.Keychain.Detail)
	}
	if err := addDummyCredential(t, env, "relock-account", "dummy-not-a-real-token"); err != nil {
		t.Fatalf("seed dummy credential: %v", err)
	}

	// Lock it, exactly as a sleep or a logout would.
	lock := exec.Command("security", "lock-keychain", env.Keychain.Path)
	lock.Env = []string{"HOME=" + env.RuntimeHome, "PATH=/usr/bin:/bin"}
	if out, err := lock.CombinedOutput(); err != nil {
		t.Fatalf("lock-keychain: %v: %s", err, out)
	}

	// The whole contract: AO can reopen it, with no person and no dialog.
	report := InspectKeychain(env.RuntimeHome)
	if report.State != KeychainOK {
		t.Fatalf("a locked AO keychain could not be reopened without interaction: state=%s detail=%s",
			report.State, report.Detail)
	}
	if !findDummyCredential(env, "relock-account") {
		t.Fatal("credential unreadable after the keychain was locked and reopened")
	}
}

// TestPrepare_NeverUsesTheReservedLoginName is the regression guard for the
// name itself. It is asserted rather than left implicit because the failure it
// prevents is invisible in the same process that creates the keychain.
func TestPrepare_NeverUsesTheReservedLoginName(t *testing.T) {
	securityAvailable(t)
	env, err := Prepare(t.TempDir(), domain.UserID("name-user"))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if filepath.Base(env.Keychain.Path) == "login.keychain-db" {
		t.Fatal("AO provisioned its per-user keychain under the reserved name \"login\", which macOS never reopens with AO's own password")
	}
	if _, err := os.Stat(filepath.Join(env.RuntimeHome, "Library", "Keychains", "login.keychain-db")); err == nil {
		t.Fatal("a keychain named \"login\" exists in the runtime home; it will raise an unanswerable unlock dialog")
	}
}

// TestPrepare_QuarantinesLegacyLoginKeychain covers the upgrade path: an
// install that already ran the old code has an unopenable login.keychain-db
// sitting in its runtime home and named in that home's search list. Leaving it
// leaves the dialog, so Prepare moves it aside -- and moves it, never deletes
// it, because AO does not destroy credential material it cannot read.
func TestPrepare_QuarantinesLegacyLoginKeychain(t *testing.T) {
	securityAvailable(t)
	dataDir := t.TempDir()
	userID := domain.UserID("legacy-user")

	// Provision the legacy shape by hand, the way the old code did: the
	// runtime-home layout, the generated secret, and a keychain named "login".
	root := filepath.Join(dataDir, "users", string(userID))
	keychainDir := filepath.Join(root, "runtime-home", "Library", "Keychains")
	if err := os.MkdirAll(keychainDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "providers", "claude-code"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".keychain-secret"), []byte("legacy-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(keychainDir, "login.keychain-db")
	if err := os.WriteFile(legacy, []byte("legacy keychain bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A preflight run against this home must refuse, not probe.
	if report := InspectKeychain(filepath.Join(root, "runtime-home")); report.State != KeychainRequiresInteraction {
		t.Fatalf("legacy login keychain was not reported as requiring interaction: state=%s", report.State)
	}

	env, err := Prepare(dataDir, userID)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !env.Keychain.Repaired {
		t.Fatal("prepare did not report repairing the legacy keychain")
	}
	if _, err := os.Stat(legacy); err == nil {
		t.Fatal("the legacy login keychain is still in place after prepare")
	}
	if env.Keychain.QuarantinedPath == "" {
		t.Fatal("prepare reported a repair but named no quarantine path")
	}
	if _, err := os.Stat(env.Keychain.QuarantinedPath); err != nil {
		t.Fatalf("the legacy keychain was deleted rather than moved aside: %v", err)
	}
	if env.Keychain.State != KeychainOK {
		t.Fatalf("prepare left the replacement keychain unusable: state=%s detail=%s", env.Keychain.State, env.Keychain.Detail)
	}
}

// TestEnsureIsolatedKeychain_RepairsAnUnopenableKeychain covers the general
// case the incident is one instance of: whatever the reason, an AO-owned
// keychain that AO's stored secret does not open is a dialog waiting to
// happen, and must be replaced rather than handed to a launch.
func TestEnsureIsolatedKeychain_RepairsAnUnopenableKeychain(t *testing.T) {
	securityAvailable(t)
	dataDir := t.TempDir()
	userID := domain.UserID("drifted-user")

	env, err := Prepare(dataDir, userID)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// Drift the stored secret away from the keychain's real password, which is
	// indistinguishable from any other way AO could lose it.
	secretPath := filepath.Join(env.Root, ".keychain-secret")
	if err := os.WriteFile(secretPath, []byte("not-the-password-any-more"), 0o600); err != nil {
		t.Fatal(err)
	}

	if report := InspectKeychain(env.RuntimeHome); report.State != KeychainRequiresInteraction {
		t.Fatalf("a keychain AO cannot open was not reported as requiring interaction: state=%s", report.State)
	}

	repaired, err := Prepare(dataDir, userID)
	if err != nil {
		t.Fatalf("prepare after drift: %v", err)
	}
	if !repaired.Keychain.Repaired {
		t.Fatal("prepare did not repair a keychain it could no longer open")
	}
	if repaired.Keychain.State != KeychainOK {
		t.Fatalf("prepare left the keychain unusable after repair: state=%s detail=%s",
			repaired.Keychain.State, repaired.Keychain.Detail)
	}
	if err := addDummyCredential(t, repaired, "post-repair", "dummy-not-a-real-token"); err != nil {
		t.Fatalf("repaired keychain does not accept a credential: %v", err)
	}
	if !findDummyCredential(repaired, "post-repair") {
		t.Fatal("repaired keychain does not return the credential written to it")
	}
}

// TestKeychainReport_CarriesNoSecret is the leakage guard. The report is
// persisted into durable stops and shown to people, so no path through it may
// carry the keychain password -- including the error paths, which are exactly
// where a future security(1) would be most likely to echo one back.
func TestKeychainReport_CarriesNoSecret(t *testing.T) {
	securityAvailable(t)
	dataDir := t.TempDir()
	env, err := Prepare(dataDir, domain.UserID("secret-user"))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	secret, err := os.ReadFile(filepath.Join(env.Root, ".keychain-secret"))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{env.Keychain.Detail, env.Keychain.Path, env.Keychain.QuarantinedPath} {
		if field != "" && len(secret) > 0 && strings.Contains(field, string(secret)) {
			t.Fatalf("keychain report leaked the keychain secret: %q", field)
		}
	}
}
