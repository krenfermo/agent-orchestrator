package providerruntime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type fakeOwners struct {
	owner *domain.UserID
	err   error
}

func (f fakeOwners) GetWorkflowRunOwner(context.Context, string) (*domain.UserID, error) {
	return f.owner, f.err
}

type fakeProfiles struct {
	profiles []domain.ProviderProfile
	err      error
}

func (f fakeProfiles) ListProviderProfilesByUser(context.Context, domain.UserID) ([]domain.ProviderProfile, error) {
	return f.profiles, f.err
}

func userPtr(id string) *domain.UserID {
	u := domain.UserID(id)
	return &u
}

func TestResolver_NilOwners_NoOp(t *testing.T) {
	r := &Resolver{}
	env, owner, _, err := r.Resolve(context.Background(), "run-1", domain.HarnessClaudeCode)
	if err != nil || env != nil || owner != "" {
		t.Fatalf("expected no-op, got env=%v owner=%v err=%v", env, owner, err)
	}
}

func TestResolver_UnownedRun_NoOp(t *testing.T) {
	r := &Resolver{Owners: fakeOwners{owner: nil}}
	env, owner, _, err := r.Resolve(context.Background(), "run-1", domain.HarnessClaudeCode)
	if err != nil || env != nil || owner != "" {
		t.Fatalf("expected no-op for unowned run, got env=%v owner=%v err=%v", env, owner, err)
	}
}

func TestResolver_NoMatchingProfile_TrustedLocal_NoOp(t *testing.T) {
	r := &Resolver{
		Owners:       fakeOwners{owner: userPtr("user-a")},
		Profiles:     fakeProfiles{profiles: nil},
		TrustedLocal: true,
		DataDir:      t.TempDir(),
	}
	env, owner, _, err := r.Resolve(context.Background(), "run-1", domain.HarnessClaudeCode)
	if err != nil {
		t.Fatalf("trusted-local with no profile must not block: %v", err)
	}
	if env != nil {
		t.Fatalf("trusted-local with no profile must not override env: %v", env)
	}
	if owner != "user-a" {
		t.Fatalf("owner should still be reported: %v", owner)
	}
}

func TestResolver_NoMatchingProfile_MultiUser_Blocked(t *testing.T) {
	r := &Resolver{
		Owners:       fakeOwners{owner: userPtr("user-a")},
		Profiles:     fakeProfiles{profiles: nil},
		TrustedLocal: false,
		DataDir:      t.TempDir(),
	}
	_, owner, _, err := r.Resolve(context.Background(), "run-1", domain.HarnessClaudeCode)
	if !errors.Is(err, ports.ErrProviderProfileRequired) {
		t.Fatalf("expected ErrProviderProfileRequired, got %v", err)
	}
	if owner != "user-a" {
		t.Fatalf("owner should still be reported even when blocked: %v", owner)
	}
}

// TestResolver_MatchingEnabledProfile_MultiUser_IsolatesEnv is explicitly
// multi-user-only (TrustedLocal=false): isolating HOME/CLAUDE_CONFIG_DIR/
// CODEX_HOME on a matching profile is the multi-user policy. The
// trusted-local desktop counterpart deliberately keeps the host runtime --
// see TestResolver_MatchingEnabledProfile_TrustedLocal_KeepsHostRuntime.
func TestResolver_MatchingEnabledProfile_MultiUser_IsolatesEnv(t *testing.T) {
	r := &Resolver{
		Owners: fakeOwners{owner: userPtr("user-a")},
		Profiles: fakeProfiles{profiles: []domain.ProviderProfile{
			{Harness: domain.HarnessClaudeCode, Enabled: true},
		}},
		TrustedLocal: false,
		DataDir:      t.TempDir(),
	}
	env, owner, _, err := r.Resolve(context.Background(), "run-1", domain.HarnessClaudeCode)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != "user-a" {
		t.Fatalf("unexpected owner: %v", owner)
	}
	if env["HOME"] == "" || env["CLAUDE_CONFIG_DIR"] == "" || env["CODEX_HOME"] == "" {
		t.Fatalf("expected isolated env overrides, got %v", env)
	}
	for _, key := range []string{"HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME"} {
		if !strings.HasPrefix(env[key], r.DataDir) {
			t.Fatalf("multi-user %s must stay inside the AO data dir, got %q", key, env[key])
		}
	}
}

func TestResolver_DisabledProfile_TreatedAsNoMatch(t *testing.T) {
	r := &Resolver{
		Owners: fakeOwners{owner: userPtr("user-a")},
		Profiles: fakeProfiles{profiles: []domain.ProviderProfile{
			{Harness: domain.HarnessClaudeCode, Enabled: false},
		}},
		TrustedLocal: false,
		DataDir:      t.TempDir(),
	}
	_, _, _, err := r.Resolve(context.Background(), "run-1", domain.HarnessClaudeCode)
	if !errors.Is(err, ports.ErrProviderProfileRequired) {
		t.Fatalf("a disabled profile must not count as a match: %v", err)
	}
}

// TestResolver_TwoUsers_DistinctEnv proves isolation end-to-end through the
// resolver: two different owners with matching profiles never share a HOME.
func TestResolver_TwoUsers_DistinctEnv(t *testing.T) {
	dataDir := t.TempDir()
	profiles := []domain.ProviderProfile{{Harness: domain.HarnessClaudeCode, Enabled: true}}

	// Multi-user mode explicitly: this is the isolation guarantee that must
	// never regress, and it is not the trusted-local desktop policy.
	rA := &Resolver{Owners: fakeOwners{owner: userPtr("user-a")}, Profiles: fakeProfiles{profiles: profiles}, DataDir: dataDir, TrustedLocal: false}
	rB := &Resolver{Owners: fakeOwners{owner: userPtr("user-b")}, Profiles: fakeProfiles{profiles: profiles}, DataDir: dataDir, TrustedLocal: false}

	envA, _, _, err := rA.Resolve(context.Background(), "run-a", domain.HarnessClaudeCode)
	if err != nil {
		t.Fatalf("resolve A: %v", err)
	}
	envB, _, _, err := rB.Resolve(context.Background(), "run-b", domain.HarnessClaudeCode)
	if err != nil {
		t.Fatalf("resolve B: %v", err)
	}
	if envA["HOME"] == envB["HOME"] {
		t.Fatalf("user A and B resolved to the same HOME: %s", envA["HOME"])
	}
	if envA["CLAUDE_CONFIG_DIR"] == envB["CLAUDE_CONFIG_DIR"] {
		t.Fatalf("user A and B resolved to the same CLAUDE_CONFIG_DIR")
	}
}

// TestResolver_MatchingEnabledProfile_TrustedLocal_KeepsHostRuntime is the
// regression guard for the trusted-local desktop failure: an enabled
// ProviderProfile used to redirect the launch into the AO per-user isolated
// runtime-home (and, on macOS, an AO-owned login keychain that does not hold
// the host CLI's OAuth credential), so the planner died instantly with
// "exit status 1" on a desktop whose `claude` CLI was authenticated and
// working. Trusted-local must keep the host runtime while still reporting
// the matched profile ID for routing/capacity scoping.
func TestResolver_MatchingEnabledProfile_TrustedLocal_KeepsHostRuntime(t *testing.T) {
	dataDir := t.TempDir()
	r := &Resolver{
		Owners: fakeOwners{owner: userPtr("user-a")},
		Profiles: fakeProfiles{profiles: []domain.ProviderProfile{
			{ID: domain.ProviderProfileID("profile-claude"), Harness: domain.HarnessClaudeCode, Enabled: true},
		}},
		TrustedLocal: true,
		DataDir:      dataDir,
	}
	env, owner, profileID, err := r.Resolve(context.Background(), "run-1", domain.HarnessClaudeCode)
	if err != nil {
		t.Fatalf("trusted-local must not block a matching profile: %v", err)
	}
	if env != nil {
		t.Fatalf("trusted-local must inherit the host CLI runtime, got env override %v", env)
	}
	if owner != "user-a" {
		t.Fatalf("owner should still be reported: %v", owner)
	}
	if profileID != domain.ProviderProfileID("profile-claude") {
		t.Fatalf("matched profile ID must still be reported for routing, got %q", profileID)
	}
	// runtimehome.Prepare must not even be required for the launch: nothing
	// under AO_DATA_DIR/users is created, so no isolated HOME and (on macOS)
	// no AO-owned login.keychain-db is provisioned.
	if _, statErr := os.Stat(filepath.Join(dataDir, "users")); !os.IsNotExist(statErr) {
		t.Fatalf("trusted-local must not prepare a per-user runtime-home under %s (stat err: %v)", dataDir, statErr)
	}
}

// TestResolver_TrustedLocal_NoHostKeychainMutation proves the trusted-local
// path leaves the host macOS keychain configuration exactly as it found it:
// no isolated keychain is created under the data dir, and the host's own
// default keychain / search list are unchanged (the resolver never shells out
// to security(1) at all in this mode).
func TestResolver_TrustedLocal_NoHostKeychainMutation(t *testing.T) {
	dataDir := t.TempDir()
	before := hostKeychainState(t)

	r := &Resolver{
		Owners: fakeOwners{owner: userPtr("user-a")},
		Profiles: fakeProfiles{profiles: []domain.ProviderProfile{
			{ID: domain.ProviderProfileID("profile-claude"), Harness: domain.HarnessClaudeCode, Enabled: true},
		}},
		TrustedLocal: true,
		DataDir:      dataDir,
	}
	if _, _, _, err := r.Resolve(context.Background(), "run-1", domain.HarnessClaudeCode); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got := hostKeychainState(t); got != before {
		t.Fatalf("host keychain state mutated:\nbefore: %s\nafter:  %s", before, got)
	}
	matches, err := filepath.Glob(filepath.Join(dataDir, "users", "*", "runtime-home", "Library", "Keychains", "*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("trusted-local must not create an AO-owned keychain: %v", matches)
	}
}

// hostKeychainState snapshots the host's default keychain and user search
// list on macOS (empty string elsewhere, where there is no keychain to
// mutate).
func hostKeychainState(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "darwin" {
		return ""
	}
	var b strings.Builder
	for _, args := range [][]string{
		{"default-keychain"},
		{"list-keychains", "-d", "user"},
	} {
		out, err := exec.Command("security", args...).Output()
		if err != nil {
			t.Skipf("security %v unavailable in this environment: %v", args, err)
		}
		b.Write(out)
	}
	return b.String()
}

// TestResolver_TrustedLocal_ProfileID_ReportedPerHarness keeps the routing
// metadata contract honest across harnesses: trusted-local returns no env,
// but the profile that matched the requested harness is still the one
// reported (and a harness with no enabled profile still resolves cleanly).
func TestResolver_TrustedLocal_ProfileID_ReportedPerHarness(t *testing.T) {
	r := &Resolver{
		Owners: fakeOwners{owner: userPtr("user-a")},
		Profiles: fakeProfiles{profiles: []domain.ProviderProfile{
			{ID: domain.ProviderProfileID("p-claude"), Harness: domain.HarnessClaudeCode, Enabled: true},
			{ID: domain.ProviderProfileID("p-codex"), Harness: domain.HarnessCodex, Enabled: false},
		}},
		TrustedLocal: true,
		DataDir:      t.TempDir(),
	}
	env, profileID, err := r.ResolveForOwner(context.Background(), domain.UserID("user-a"), domain.HarnessClaudeCode)
	if err != nil || env != nil || profileID != domain.ProviderProfileID("p-claude") {
		t.Fatalf("claude: env=%v profile=%q err=%v", env, profileID, err)
	}
	// Disabled Codex profile is not a match; trusted-local stays permissive.
	env, profileID, err = r.ResolveForOwner(context.Background(), domain.UserID("user-a"), domain.HarnessCodex)
	if err != nil || env != nil || profileID != "" {
		t.Fatalf("codex: env=%v profile=%q err=%v", env, profileID, err)
	}
}
