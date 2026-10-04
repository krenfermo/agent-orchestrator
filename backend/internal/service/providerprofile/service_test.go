package providerprofile_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/runtimehome"
	authsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/authsvc"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/providerprofile"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type fixedDataDir string

func (d fixedDataDir) DataDir() string { return string(d) }

type fakeProber struct {
	state domain.ProviderAuthState
	err   error
}

func (f fakeProber) Probe(ctx context.Context, harness domain.AgentHarness, env runtimehome.Environment) (domain.ProviderAuthState, error) {
	return f.state, f.err
}

func newService(t *testing.T, state domain.ProviderAuthState) (*providerprofile.Service, domain.UserID) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	authMgr := authsvc.New(store, func() time.Time { return time.Now().UTC() })
	user, err := authMgr.CreateUser(context.Background(), authsvc.CreateUserInput{
		Email: "alice@example.com", Username: "alice", Password: "correct-horse-a",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	svc := &providerprofile.Service{
		Store:   store,
		Prober:  fakeProber{state: state},
		DataDir: fixedDataDir(t.TempDir()),
		Clock:   func() time.Time { return time.Now().UTC() },
	}
	return svc, user.ID
}

func createClaudeProfile(t *testing.T, svc *providerprofile.Service, userID domain.UserID) domain.ProviderProfile {
	t.Helper()
	p, err := svc.Create(context.Background(), userID, providerprofile.CreateInput{
		Provider: "anthropic",
		Harness:  domain.HarnessClaudeCode,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return p
}

// TestTest_NeverClaimsAuthenticatedWithoutARealProbe covers the Checkpoint
// 8P-E.1 hard rule: Test's OK field must track the prober's real, current
// answer -- never a stale/optimistic guess.
func TestTest_NeverClaimsAuthenticatedWithoutARealProbe(t *testing.T) {
	cases := []struct {
		name          string
		probeState    domain.ProviderAuthState
		wantOK        bool
		wantSubstring string
	}{
		{"authenticated", domain.ProviderAuthStateAuthenticated, true, "authenticated and ready"},
		{"unauthenticated", domain.ProviderAuthStateUnauthenticated, false, "not authenticated"},
		{"not installed", domain.ProviderAuthStateNotInstalled, false, "not installed"},
		{"probe error", domain.ProviderAuthStateError, false, "failed"},
		{"unknown", domain.ProviderAuthStateUnknown, false, "could not be determined"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, userID := newService(t, c.probeState)
			profile := createClaudeProfile(t, svc, userID)

			result, err := svc.Test(context.Background(), userID, profile.ID)
			if err != nil {
				t.Fatalf("test: %v", err)
			}
			if result.OK != c.wantOK {
				t.Fatalf("OK = %v, want %v", result.OK, c.wantOK)
			}
			if !strings.Contains(result.Message, c.wantSubstring) {
				t.Fatalf("message %q does not contain %q", result.Message, c.wantSubstring)
			}
			if result.AuthState != c.probeState {
				t.Fatalf("AuthState = %s, want %s", result.AuthState, c.probeState)
			}

			// Persisted state must match what was just reported -- the UI
			// refetches the profile after Test and must see the same state.
			persisted, err := svc.Get(context.Background(), userID, profile.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if persisted.AuthState != c.probeState {
				t.Fatalf("persisted AuthState = %s, want %s", persisted.AuthState, c.probeState)
			}
		})
	}
}

// TestTest_MessageNeverContainsSecretMaterial guards against a future
// message template accidentally interpolating something sensitive.
func TestTest_MessageNeverContainsSecretMaterial(t *testing.T) {
	svc, userID := newService(t, domain.ProviderAuthStateUnauthenticated)
	profile := createClaudeProfile(t, svc, userID)

	result, err := svc.Test(context.Background(), userID, profile.ID)
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	forbidden := []string{"/Users/", "/home/", "token", "secret", "password", "ciphertext"}
	lower := strings.ToLower(result.Message)
	for _, f := range forbidden {
		if strings.Contains(lower, strings.ToLower(f)) {
			t.Fatalf("message leaked forbidden substring %q: %q", f, result.Message)
		}
	}
}

// TestConnect_DoesNotFabricateAuthenticated verifies Connect only ever
// reports authenticated when the real prober says so.
func TestConnect_DoesNotFabricateAuthenticated(t *testing.T) {
	svc, userID := newService(t, domain.ProviderAuthStateUnauthenticated)
	profile := createClaudeProfile(t, svc, userID)

	got, err := svc.Connect(context.Background(), userID, profile.ID)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if got.AuthState == domain.ProviderAuthStateAuthenticated {
		t.Fatal("Connect fabricated an authenticated state the prober never reported")
	}
	if got.AuthState != domain.ProviderAuthStateUnauthenticated {
		t.Fatalf("AuthState = %s, want unauthenticated", got.AuthState)
	}
}

// TestDisconnect_UpdatesStateImmediately covers Checkpoint 8P-E.1 Phase 5:
// Disconnect must leave the store in a state a subsequent Get reflects
// immediately, with no separate re-probe required to see the change.
func TestDisconnect_UpdatesStateImmediately(t *testing.T) {
	svc, userID := newService(t, domain.ProviderAuthStateAuthenticated)
	profile := createClaudeProfile(t, svc, userID)

	if _, err := svc.Connect(context.Background(), userID, profile.ID); err != nil {
		t.Fatalf("connect: %v", err)
	}
	connected, err := svc.Get(context.Background(), userID, profile.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if connected.AuthState != domain.ProviderAuthStateAuthenticated {
		t.Fatalf("precondition: expected authenticated, got %s", connected.AuthState)
	}

	if _, err := svc.Disconnect(context.Background(), userID, profile.ID); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	after, err := svc.Get(context.Background(), userID, profile.ID)
	if err != nil {
		t.Fatalf("get after disconnect: %v", err)
	}
	if after.AuthState != domain.ProviderAuthStateUnauthenticated {
		t.Fatalf("AuthState after disconnect = %s, want unauthenticated", after.AuthState)
	}
}
