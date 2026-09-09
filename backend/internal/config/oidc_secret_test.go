package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The sentinel is a fixed non-secret string. Nothing in this file reads, logs
// or compares a real client secret.
const secretSentinel = "sentinel-not-a-real-secret"

func writeSecretFile(t *testing.T, value string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oidc-secret")
	if err := os.WriteFile(path, []byte(value), mode); err != nil {
		t.Fatalf("write secret fixture: %v", err)
	}
	// WriteFile is subject to umask, so the mode is set explicitly.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod secret fixture: %v", err)
	}
	return path
}

func TestClientSecretFileIsReadAndKeptOutOfTheEnvironment(t *testing.T) {
	path := writeSecretFile(t, secretSentinel+"\n", 0o600)
	t.Setenv("AO_OIDC_CLIENT_SECRET_FILE", path)

	got, err := resolveClientSecret()
	if err != nil {
		t.Fatalf("resolveClientSecret: %v", err)
	}
	// The trailing newline an editor adds is not part of the secret.
	if got != secretSentinel {
		t.Fatalf("secret was not read back verbatim (len %d, want %d)", len(got), len(secretSentinel))
	}
	// The whole point: it never entered the environment, so there is nothing
	// for a process listing or a child process to pick up.
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "AO_OIDC_CLIENT_SECRET=") {
			t.Fatal("the secret file path put AO_OIDC_CLIENT_SECRET into the environment")
		}
	}
}

func TestInlineClientSecretIsScrubbedFromTheEnvironment(t *testing.T) {
	t.Setenv("AO_OIDC_CLIENT_SECRET", secretSentinel)

	got, err := resolveClientSecret()
	if err != nil {
		t.Fatalf("resolveClientSecret: %v", err)
	}
	if got != secretSentinel {
		t.Fatal("the legacy inline variable must still configure the client")
	}
	if v, ok := os.LookupEnv("AO_OIDC_CLIENT_SECRET"); ok {
		t.Fatalf("AO_OIDC_CLIENT_SECRET survived the load as %d bytes; it must be unset", len(v))
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "AO_OIDC_CLIENT_SECRET=") {
			t.Fatal("AO_OIDC_CLIENT_SECRET is still in os.Environ after the load")
		}
	}
}

// The guarantee that actually matters for this incident: the daemon spawns
// agents, reviewers, planners and verify commands with a copy of os.Environ().
// This asserts the property on a REAL child process rather than on the slice,
// because the slice is only evidence about what a child would receive.
func TestSpawnedChildDoesNotInheritTheClientSecret(t *testing.T) {
	t.Setenv("AO_OIDC_CLIENT_SECRET", secretSentinel)
	if _, err := resolveClientSecret(); err != nil {
		t.Fatalf("resolveClientSecret: %v", err)
	}

	// `env` prints the environment the child actually received.
	out, err := exec.Command("/usr/bin/env").Output()
	if err != nil {
		t.Skipf("cannot run /usr/bin/env on this platform: %v", err)
	}
	if strings.Contains(string(out), secretSentinel) {
		t.Fatal("a spawned child inherited the client secret from the daemon's environment")
	}
	if strings.Contains(string(out), "AO_OIDC_CLIENT_SECRET=") {
		t.Fatal("a spawned child inherited AO_OIDC_CLIENT_SECRET")
	}
}

func TestClientSecretFileRejectsPermissiveModes(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o660} {
		path := writeSecretFile(t, secretSentinel, mode)
		t.Setenv("AO_OIDC_CLIENT_SECRET_FILE", path)

		_, err := resolveClientSecret()
		if err == nil {
			t.Fatalf("mode %04o was accepted; a secret readable beyond its owner must be refused", mode)
		}
		if strings.Contains(err.Error(), secretSentinel) {
			t.Fatal("the error message quoted the secret")
		}
	}
}

func TestClientSecretRefusesAmbiguousConfiguration(t *testing.T) {
	path := writeSecretFile(t, secretSentinel, 0o600)
	t.Setenv("AO_OIDC_CLIENT_SECRET_FILE", path)
	t.Setenv("AO_OIDC_CLIENT_SECRET", "a-different-sentinel")

	_, err := resolveClientSecret()
	if err == nil {
		t.Fatal("both variables set was accepted; a rotation must be able to say which secret is live")
	}
	// Even when refusing, the inline variable is gone from the environment.
	if _, ok := os.LookupEnv("AO_OIDC_CLIENT_SECRET"); ok {
		t.Fatal("the inline variable survived a failed load")
	}
}

func TestClientSecretFileErrorsNeverQuoteTheSecret(t *testing.T) {
	empty := writeSecretFile(t, "   \n", 0o600)
	t.Setenv("AO_OIDC_CLIENT_SECRET_FILE", empty)
	_, err := resolveClientSecret()
	if err == nil {
		t.Fatal("an empty secret file must be refused")
	}

	missing := filepath.Join(t.TempDir(), "absent")
	t.Setenv("AO_OIDC_CLIENT_SECRET_FILE", missing)
	if _, err := resolveClientSecret(); err == nil {
		t.Fatal("a missing secret file must be refused")
	}
}

// A public (PKCE-only) client is legitimate and must stay configurable.
func TestNoClientSecretConfiguredStaysValid(t *testing.T) {
	t.Setenv("AO_OIDC_CLIENT_SECRET_FILE", "")
	os.Unsetenv("AO_OIDC_CLIENT_SECRET")

	got, err := resolveClientSecret()
	if err != nil {
		t.Fatalf("a public client must not be an error: %v", err)
	}
	if got != "" {
		t.Fatal("no secret was configured, so none must be reported")
	}
}
