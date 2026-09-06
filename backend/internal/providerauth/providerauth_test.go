package providerauth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// The secret a test would notice leaking. Every case that carries a credential
// uses this exact value so the leakage assertion can be shared.
const fakeCredential = "sk-ant-not-a-real-key-000000"

func hostHome(t *testing.T) string {
	t.Helper()
	// A directory that exists and is treated as the daemon user's own, so an
	// isolated home is anything else. Making it explicit keeps the suite off
	// the machine's real home.
	return t.TempDir()
}

func TestProbe_EnvironmentCredentialIsUnattended(t *testing.T) {
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		t.Run(name, func(t *testing.T) {
			c := Probe(context.Background(), Request{
				Harness:  domain.HarnessClaudeCode,
				Env:      map[string]string{name: fakeCredential, "HOME": t.TempDir()},
				HostHome: hostHome(t),
			})
			if c.Mode != ModeEnvironment || c.Status != StatusAvailable {
				t.Fatalf("got mode=%s status=%s, want environment/available", c.Mode, c.Status)
			}
			if !c.Unattended() {
				t.Fatal("an environment credential must be unattended")
			}
			if c.Source != name {
				t.Fatalf("source = %q, want the variable name %q", c.Source, name)
			}
			assertNoSecret(t, c)
		})
	}
}

// An environment credential must win over a broken keychain. This is the
// property that makes the unattended mode a real escape hatch rather than a
// preference: a machine whose AO keychain is unopenable still launches.
func TestProbe_EnvironmentCredentialOutranksAnUnopenableKeychain(t *testing.T) {
	env := isolatedEnvWithLegacyKeychain(t)
	env["ANTHROPIC_API_KEY"] = fakeCredential

	c := Probe(context.Background(), Request{
		Harness:  domain.HarnessClaudeCode,
		Env:      env,
		HostHome: hostHome(t),
	})
	if c.Mode != ModeEnvironment || !c.Unattended() {
		t.Fatalf("got mode=%s status=%s, want the environment credential to be used", c.Mode, c.Status)
	}
}

func TestProbe_HelperIsUnattendedWhenExecutable(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "get-key.sh")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\necho key\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSettings(t, dir, helper+" --profile ao")

	c := Probe(context.Background(), Request{
		Harness:  domain.HarnessClaudeCode,
		Env:      map[string]string{"CLAUDE_CONFIG_DIR": dir, "HOME": t.TempDir()},
		HostHome: hostHome(t),
	})
	if c.Mode != ModeHelper || c.Status != StatusAvailable {
		t.Fatalf("got mode=%s status=%s (%s), want helper/available", c.Mode, c.Status, c.Reason)
	}
	if c.Source != helper {
		t.Fatalf("source = %q, want the helper program %q", c.Source, helper)
	}
}

// A configured helper that cannot run is an affirmative refusal, not a silent
// fall-through to the keychain: the operator said where credentials come from,
// and quietly reaching somewhere else is how a launch ends up at a dialog.
func TestProbe_ConfiguredHelperThatCannotRunIsUnavailable(t *testing.T) {
	dir := t.TempDir()
	writeSettings(t, dir, filepath.Join(dir, "missing-helper"))

	c := Probe(context.Background(), Request{
		Harness:  domain.HarnessClaudeCode,
		Env:      map[string]string{"CLAUDE_CONFIG_DIR": dir, "HOME": t.TempDir()},
		HostHome: hostHome(t),
	})
	if c.Mode != ModeHelper || c.Status != StatusUnavailable {
		t.Fatalf("got mode=%s status=%s, want helper/unavailable", c.Mode, c.Status)
	}
	if c.Unattended() {
		t.Fatal("a helper that cannot run must not be reported as unattended")
	}
}

// The incident, at the contract level: a launch whose HOME resolves an AO
// keychain that cannot be opened must be classified as requiring interaction,
// which is what refuses the dispatch instead of hanging it.
func TestProbe_UnopenableKeychainRequiresInteraction(t *testing.T) {
	c := Probe(context.Background(), Request{
		Harness:  domain.HarnessClaudeCode,
		Env:      isolatedEnvWithLegacyKeychain(t),
		HostHome: hostHome(t),
	})
	if c.Mode != ModeKeychain || c.Status != StatusRequiresInteraction {
		t.Fatalf("got mode=%s status=%s (%s), want keychain/requires_interaction", c.Mode, c.Status, c.Reason)
	}
	if c.Unattended() {
		t.Fatal("a launch that would raise an unlock dialog must not be reported as unattended")
	}
	if !strings.Contains(c.Reason, "keychain") {
		t.Fatalf("reason does not name the keychain: %q", c.Reason)
	}
}

// An isolated runtime home with no credential at all is unavailable rather
// than requires_interaction: nothing would prompt, there is simply nothing to
// authenticate with, and the two send a person to different places.
func TestProbe_IsolatedHomeWithNoCredentialIsUnavailable(t *testing.T) {
	home := t.TempDir()
	c := Probe(context.Background(), Request{
		Harness:  domain.HarnessClaudeCode,
		Env:      map[string]string{"HOME": home, "CLAUDE_CONFIG_DIR": t.TempDir()},
		HostHome: hostHome(t),
	})
	if c.Status != StatusUnavailable {
		t.Fatalf("got status=%s (%s), want unavailable", c.Status, c.Reason)
	}
}

// The host's own home is never refused. AO does not inspect the desktop user's
// real credential store, and a wrong refusal there grounds a working install.
func TestProbe_HostHomeIsNeverRefused(t *testing.T) {
	host := hostHome(t)
	c := Probe(context.Background(), Request{
		Harness:  domain.HarnessClaudeCode,
		Env:      map[string]string{"HOME": host},
		HostHome: host,
	})
	if !c.Unattended() {
		t.Fatalf("the host's own home was refused: status=%s (%s)", c.Status, c.Reason)
	}
	if c.Status != StatusUnknown {
		t.Fatalf("got status=%s, want unknown for a store AO does not inspect", c.Status)
	}
}

// A harness AO has no credential model for is never refused either.
func TestProbe_UnknownHarnessIsNeverRefused(t *testing.T) {
	c := Probe(context.Background(), Request{
		Harness:  domain.HarnessCodex,
		Env:      isolatedEnvWithLegacyKeychain(t),
		HostHome: hostHome(t),
	})
	if !c.Unattended() || c.Status != StatusUnknown {
		t.Fatalf("got status=%s, want unknown (ready) for a harness with no credential model", c.Status)
	}
}

func TestProbe_CloudProviderIsRecognisedAndNotRefused(t *testing.T) {
	for _, name := range []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX"} {
		t.Run(name, func(t *testing.T) {
			env := isolatedEnvWithLegacyKeychain(t)
			env[name] = "1"
			c := Probe(context.Background(), Request{
				Harness:  domain.HarnessClaudeCode,
				Env:      env,
				HostHome: hostHome(t),
			})
			if c.Mode != ModeCloudProvider || !c.Unattended() {
				t.Fatalf("got mode=%s status=%s, want a cloud provider chain AO does not refuse", c.Mode, c.Status)
			}
		})
	}
}

// A pinned mode is a requirement, not a preference: an operator who declares
// "credentials come from the environment" gets a refusal when they do not,
// rather than a launch that quietly reaches for a keychain instead.
func TestProbe_PinnedModeDoesNotFallThrough(t *testing.T) {
	env := isolatedEnvWithLegacyKeychain(t)
	c := Probe(context.Background(), Request{
		Harness:  domain.HarnessClaudeCode,
		Env:      env,
		Required: ModeEnvironment,
		HostHome: hostHome(t),
	})
	if c.Mode != ModeEnvironment || c.Status != StatusUnavailable {
		t.Fatalf("got mode=%s status=%s, want the pinned mode reported unavailable", c.Mode, c.Status)
	}

	env["ANTHROPIC_API_KEY"] = fakeCredential
	if c := Probe(context.Background(), Request{
		Harness:  domain.HarnessClaudeCode,
		Env:      env,
		Required: ModeEnvironment,
		HostHome: hostHome(t),
	}); !c.Unattended() {
		t.Fatalf("a satisfied pinned mode was refused: %s (%s)", c.Status, c.Reason)
	}
}

// The Claude binary's own path is versioned and changes on every update. The
// contract must not depend on it: nothing here reads or resolves the binary,
// so an update cannot change the answer.
func TestProbe_IsIndependentOfTheProviderBinaryPath(t *testing.T) {
	env := map[string]string{
		"HOME":              t.TempDir(),
		"ANTHROPIC_API_KEY": fakeCredential,
		"PATH":              "/nonexistent/versions/2.1.261/bin",
	}
	before := Probe(context.Background(), Request{Harness: domain.HarnessClaudeCode, Env: env, HostHome: hostHome(t)})
	env["PATH"] = "/nonexistent/versions/9.9.999/bin"
	after := Probe(context.Background(), Request{Harness: domain.HarnessClaudeCode, Env: env, HostHome: hostHome(t)})
	if before != after {
		t.Fatalf("the auth contract changed when the provider binary path did: %+v vs %+v", before, after)
	}
}

// Nothing the contract carries may be a credential. Contract fields are
// persisted into durable stops and rendered to people.
func TestProbe_NeverCarriesACredential(t *testing.T) {
	dir := t.TempDir()
	// A credentials file whose CONTENT is the secret: the keychain path must
	// report that it exists without ever reading it.
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"),
		[]byte(`{"claudeAiOauth":{"accessToken":"`+fakeCredential+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []Request{
		{Harness: domain.HarnessClaudeCode, Env: map[string]string{"ANTHROPIC_API_KEY": fakeCredential, "HOME": t.TempDir()}},
		{Harness: domain.HarnessClaudeCode, Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": fakeCredential, "HOME": t.TempDir()}},
		{Harness: domain.HarnessClaudeCode, Env: map[string]string{"HOME": t.TempDir(), "CLAUDE_CONFIG_DIR": dir}},
		{Harness: domain.HarnessClaudeCode, Env: isolatedEnvWithLegacyKeychain(t)},
	}
	for _, req := range cases {
		req.HostHome = hostHome(t)
		assertNoSecret(t, Probe(context.Background(), req))
	}
}

func TestParseMode(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    Mode
		wantErr bool
	}{
		{raw: "", want: ""},
		{raw: "auto", want: ""},
		{raw: "AUTO", want: ""},
		{raw: "environment", want: ModeEnvironment},
		{raw: " helper ", want: ModeHelper},
		{raw: "cloud_provider", want: ModeCloudProvider},
		{raw: "keychain", want: ModeKeychain},
		{raw: "oauth", wantErr: true},
	} {
		got, err := ParseMode(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("ParseMode(%q) accepted an invalid mode", tc.raw)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ParseMode(%q): %v", tc.raw, err)
		}
		if got != tc.want {
			t.Fatalf("ParseMode(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// isolatedEnvWithLegacyKeychain builds the exact launch environment the
// incident produced: an AO-substituted HOME holding the legacy, unopenable
// "login" keychain, plus the isolated config dir beside it.
func isolatedEnvWithLegacyKeychain(t *testing.T) map[string]string {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "runtime-home")
	keychains := filepath.Join(home, "Library", "Keychains")
	if err := os.MkdirAll(keychains, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keychains, "login.keychain-db"), []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(root, "providers", "claude-code")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return map[string]string{"HOME": home, "CLAUDE_CONFIG_DIR": configDir}
}

func writeSettings(t *testing.T, dir, helper string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"apiKeyHelper": helper})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertNoSecret(t *testing.T, c Contract) {
	t.Helper()
	for label, field := range map[string]string{"Source": c.Source, "Reason": c.Reason} {
		if strings.Contains(field, fakeCredential) {
			t.Fatalf("Contract.%s leaked the credential: %q", label, field)
		}
	}
}
