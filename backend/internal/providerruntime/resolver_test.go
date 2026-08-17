package providerruntime

import (
	"context"
	"errors"
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
	env, owner, err := r.Resolve(context.Background(), "run-1", domain.HarnessClaudeCode)
	if err != nil || env != nil || owner != "" {
		t.Fatalf("expected no-op, got env=%v owner=%v err=%v", env, owner, err)
	}
}

func TestResolver_UnownedRun_NoOp(t *testing.T) {
	r := &Resolver{Owners: fakeOwners{owner: nil}}
	env, owner, err := r.Resolve(context.Background(), "run-1", domain.HarnessClaudeCode)
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
	env, owner, err := r.Resolve(context.Background(), "run-1", domain.HarnessClaudeCode)
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
	_, owner, err := r.Resolve(context.Background(), "run-1", domain.HarnessClaudeCode)
	if !errors.Is(err, ports.ErrProviderProfileRequired) {
		t.Fatalf("expected ErrProviderProfileRequired, got %v", err)
	}
	if owner != "user-a" {
		t.Fatalf("owner should still be reported even when blocked: %v", owner)
	}
}

func TestResolver_MatchingEnabledProfile_IsolatesEnv(t *testing.T) {
	r := &Resolver{
		Owners: fakeOwners{owner: userPtr("user-a")},
		Profiles: fakeProfiles{profiles: []domain.ProviderProfile{
			{Harness: domain.HarnessClaudeCode, Enabled: true},
		}},
		TrustedLocal: false,
		DataDir:      t.TempDir(),
	}
	env, owner, err := r.Resolve(context.Background(), "run-1", domain.HarnessClaudeCode)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != "user-a" {
		t.Fatalf("unexpected owner: %v", owner)
	}
	if env["HOME"] == "" || env["CLAUDE_CONFIG_DIR"] == "" {
		t.Fatalf("expected isolated env overrides, got %v", env)
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
	_, _, err := r.Resolve(context.Background(), "run-1", domain.HarnessClaudeCode)
	if !errors.Is(err, ports.ErrProviderProfileRequired) {
		t.Fatalf("a disabled profile must not count as a match: %v", err)
	}
}

// TestResolver_TwoUsers_DistinctEnv proves isolation end-to-end through the
// resolver: two different owners with matching profiles never share a HOME.
func TestResolver_TwoUsers_DistinctEnv(t *testing.T) {
	dataDir := t.TempDir()
	profiles := []domain.ProviderProfile{{Harness: domain.HarnessClaudeCode, Enabled: true}}

	rA := &Resolver{Owners: fakeOwners{owner: userPtr("user-a")}, Profiles: fakeProfiles{profiles: profiles}, DataDir: dataDir}
	rB := &Resolver{Owners: fakeOwners{owner: userPtr("user-b")}, Profiles: fakeProfiles{profiles: profiles}, DataDir: dataDir}

	envA, _, err := rA.Resolve(context.Background(), "run-a", domain.HarnessClaudeCode)
	if err != nil {
		t.Fatalf("resolve A: %v", err)
	}
	envB, _, err := rB.Resolve(context.Background(), "run-b", domain.HarnessClaudeCode)
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
