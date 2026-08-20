package authsvc_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	authsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/authsvc"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func newTestManager(t *testing.T) authsvc.Manager {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	return authsvc.New(store, func() time.Time { return time.Now().UTC() })
}

func TestSetupRequired(t *testing.T) {
	ctx := context.Background()
	mgr := newTestManager(t)

	required, err := mgr.SetupRequired(ctx)
	if err != nil {
		t.Fatalf("SetupRequired (empty): %v", err)
	}
	if !required {
		t.Fatal("expected setup required with zero users")
	}

	if _, err := mgr.RegisterFirstUser(ctx, authsvc.CreateUserInput{
		DisplayName: "Owner",
		Email:       "owner@example.com",
		Username:    "owner@example.com",
		Password:    "supersecret1",
	}); err != nil {
		t.Fatalf("RegisterFirstUser: %v", err)
	}

	required, err = mgr.SetupRequired(ctx)
	if err != nil {
		t.Fatalf("SetupRequired (non-empty): %v", err)
	}
	if required {
		t.Fatal("expected setup not required after first user")
	}
}

func TestRegisterFirstUser_Success(t *testing.T) {
	ctx := context.Background()
	mgr := newTestManager(t)

	u, err := mgr.RegisterFirstUser(ctx, authsvc.CreateUserInput{
		DisplayName: "Owner",
		Email:       "owner@example.com",
		Username:    "owner@example.com",
		Password:    "supersecret1",
	})
	if err != nil {
		t.Fatalf("RegisterFirstUser: %v", err)
	}
	if u.Role != domain.UserRoleOwner {
		t.Fatalf("expected owner role, got %q", u.Role)
	}
	if u.Username != "owner@example.com" {
		t.Fatalf("expected username to default to email, got %q", u.Username)
	}
}

func TestRegisterFirstUser_SecondCallRejected(t *testing.T) {
	ctx := context.Background()
	mgr := newTestManager(t)

	if _, err := mgr.RegisterFirstUser(ctx, authsvc.CreateUserInput{
		DisplayName: "Owner",
		Email:       "owner@example.com",
		Username:    "owner@example.com",
		Password:    "supersecret1",
	}); err != nil {
		t.Fatalf("first RegisterFirstUser: %v", err)
	}

	_, err := mgr.RegisterFirstUser(ctx, authsvc.CreateUserInput{
		DisplayName: "Someone Else",
		Email:       "someone@example.com",
		Username:    "someone@example.com",
		Password:    "supersecret2",
	})
	if err == nil {
		t.Fatal("expected second RegisterFirstUser to fail")
	}
}

// TestRegisterFirstUser_Concurrent proves the ux_users_single_owner unique
// index (not application-level locking) is what makes concurrent first-run
// signup safe: exactly one of N simultaneous callers must succeed.
func TestRegisterFirstUser_Concurrent(t *testing.T) {
	ctx := context.Background()
	mgr := newTestManager(t)

	const n = 8
	var wg sync.WaitGroup
	successes := make(chan domain.User, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			email := emailFor(i)
			u, err := mgr.RegisterFirstUser(ctx, authsvc.CreateUserInput{
				DisplayName: "Racer",
				Email:       email,
				Username:    email,
				Password:    "supersecret1",
			})
			if err == nil {
				successes <- u
			}
		}()
	}
	wg.Wait()
	close(successes)

	count := 0
	for range successes {
		count++
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 successful RegisterFirstUser, got %d", count)
	}

	required, err := mgr.SetupRequired(ctx)
	if err != nil {
		t.Fatalf("SetupRequired: %v", err)
	}
	if required {
		t.Fatal("expected setup not required after concurrent registration race")
	}
}

func emailFor(i int) string {
	letters := "abcdefghijklmnopqrstuvwxyz"
	return string(letters[i%len(letters)]) + "@example.com"
}

func TestResetPassword(t *testing.T) {
	ctx := context.Background()
	mgr := newTestManager(t)

	u, err := mgr.RegisterFirstUser(ctx, authsvc.CreateUserInput{
		DisplayName: "Owner",
		Email:       "owner@example.com",
		Username:    "owner@example.com",
		Password:    "supersecret1",
	})
	if err != nil {
		t.Fatalf("RegisterFirstUser: %v", err)
	}
	rawToken, _, err := mgr.CreateSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := mgr.ResetPassword(ctx, "owner@example.com", "brandnewpass1"); err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}

	// Old password must no longer authenticate.
	if _, err := mgr.Authenticate(ctx, "owner@example.com", "supersecret1", "test"); err == nil {
		t.Fatal("expected old password to be rejected after reset")
	}
	// New password must authenticate.
	if _, err := mgr.Authenticate(ctx, "owner@example.com", "brandnewpass1", "test2"); err != nil {
		t.Fatalf("expected new password to authenticate: %v", err)
	}
	// The pre-reset session must have been revoked.
	if _, err := mgr.ResolveSession(ctx, rawToken); err == nil {
		t.Fatal("expected pre-reset session to be revoked")
	}
}

// TestBootstrap_AssignsOwnerRole covers the env-var bootstrap path
// (AO_BOOTSTRAP_ADMIN_EMAIL/PASSWORD, wired at daemon startup): it must
// promote the created admin to domain.UserRoleOwner via the same
// ux_users_single_owner-guarded path RegisterFirstUser uses, so the two
// bootstrap routes can never both succeed.
func TestBootstrap_AssignsOwnerRole(t *testing.T) {
	ctx := context.Background()
	mgr := newTestManager(t)

	result, err := mgr.Bootstrap(ctx, "admin@example.com", "supersecret1")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if !result.Created {
		t.Fatal("expected Bootstrap to create the admin")
	}

	u, err := mgr.Authenticate(ctx, "admin@example.com", "supersecret1", "test")
	if err != nil {
		t.Fatalf("Authenticate bootstrap admin: %v", err)
	}
	if u.Role != domain.UserRoleOwner {
		t.Fatalf("expected bootstrap admin to have owner role, got %q", u.Role)
	}

	// RegisterFirstUser must now be rejected: Bootstrap already claimed the
	// single-owner slot.
	if _, err := mgr.RegisterFirstUser(ctx, authsvc.CreateUserInput{
		DisplayName: "Someone Else",
		Email:       "someone@example.com",
		Username:    "someone@example.com",
		Password:    "supersecret2",
	}); err == nil {
		t.Fatal("expected RegisterFirstUser to be rejected after env-var Bootstrap")
	}
}

// TestEnsureOwnerExists_PromotesSolePreExistingUser covers the 8P-E.8.1
// migration-recovery path: a user created via CreateUser (pre-8P-E.8 shape,
// no role opinion) with the pre-role-column backfill default of 'member'
// must be promoted to owner once, and EnsureOwnerExists must not act again
// or on installations with zero or multiple users.
func TestEnsureOwnerExists_PromotesSolePreExistingUser(t *testing.T) {
	ctx := context.Background()
	mgr := newTestManager(t)

	// Zero users: no-op.
	promoted, err := mgr.EnsureOwnerExists(ctx)
	if err != nil {
		t.Fatalf("EnsureOwnerExists (zero users): %v", err)
	}
	if promoted {
		t.Fatal("expected no-op with zero users")
	}

	u, err := mgr.CreateUser(ctx, authsvc.CreateUserInput{
		DisplayName: "Legacy User",
		Email:       "legacy@example.com",
		Username:    "legacy@example.com",
		Password:    "supersecret1",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if u.Role != domain.UserRoleMember {
		t.Fatalf("expected default role member, got %q", u.Role)
	}

	promoted, err = mgr.EnsureOwnerExists(ctx)
	if err != nil {
		t.Fatalf("EnsureOwnerExists (sole user): %v", err)
	}
	if !promoted {
		t.Fatal("expected the sole user to be promoted to owner")
	}

	got, err := mgr.Authenticate(ctx, "legacy@example.com", "supersecret1", "test")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.Role != domain.UserRoleOwner {
		t.Fatalf("expected promoted role owner, got %q", got.Role)
	}

	// Second call is a no-op (an owner now exists).
	promoted, err = mgr.EnsureOwnerExists(ctx)
	if err != nil {
		t.Fatalf("EnsureOwnerExists (already owned): %v", err)
	}
	if promoted {
		t.Fatal("expected no-op once an owner exists")
	}
}

// TestEnsureOwnerExists_MultipleUsersNoOp proves a multi-user installation
// with no owner is left alone -- that ambiguity needs a human decision, not
// an automatic promotion of an arbitrary user.
func TestEnsureOwnerExists_MultipleUsersNoOp(t *testing.T) {
	ctx := context.Background()
	mgr := newTestManager(t)

	if _, err := mgr.CreateUser(ctx, authsvc.CreateUserInput{
		DisplayName: "A", Email: "a@example.com", Username: "a@example.com", Password: "supersecret1",
	}); err != nil {
		t.Fatalf("CreateUser a: %v", err)
	}
	if _, err := mgr.CreateUser(ctx, authsvc.CreateUserInput{
		DisplayName: "B", Email: "b@example.com", Username: "b@example.com", Password: "supersecret2",
	}); err != nil {
		t.Fatalf("CreateUser b: %v", err)
	}

	promoted, err := mgr.EnsureOwnerExists(ctx)
	if err != nil {
		t.Fatalf("EnsureOwnerExists (two users): %v", err)
	}
	if promoted {
		t.Fatal("expected no-op with two users and no owner")
	}
}

func TestResetPassword_UnknownEmail(t *testing.T) {
	ctx := context.Background()
	mgr := newTestManager(t)

	if err := mgr.ResetPassword(ctx, "nobody@example.com", "brandnewpass1"); err == nil {
		t.Fatal("expected error for unknown email")
	}
}
