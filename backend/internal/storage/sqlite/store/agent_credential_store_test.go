package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func seedCredentialOwner(t *testing.T, st interface {
	InsertUser(context.Context, domain.User) (domain.User, error)
}) domain.UserID {
	t.Helper()
	now := time.Now().UTC()
	u, err := st.InsertUser(context.Background(), domain.User{
		ID: "user-owner", DisplayName: "Ada", Email: "ada@example.com", Username: "ada",
		PasswordHash: "x", Role: domain.UserRoleOwner, Status: domain.UserStatusActive,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return u.ID
}

func credential(owner domain.UserID, id, hash, reviewRun string, now time.Time) domain.AgentCredential {
	return domain.AgentCredential{
		ID: id, TokenHash: hash, Role: domain.AgentRoleReviewer, UserID: owner,
		ProjectID: "proj-1", SessionID: "agent-orchestrator-59",
		WorkflowRunID: "wf-98ab416c", WorkflowStepID: "wfs-04b67615", ReviewRunID: reviewRun,
		RuntimeHandle: "workflow-review-" + reviewRun, RuntimeInstanceID: "$26", Generation: 1,
		Permissions: domain.AgentRoleCeiling(domain.AgentRoleReviewer),
		CreatedAt:   now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now,
	}
}

// The row round-trips with its bindings and its permission set intact: a grant
// AO cannot read back exactly is a grant it cannot enforce exactly.
func TestAgentCredentialRoundTrips(t *testing.T) {
	st := sqlitetest.MustOpen(t)
	ctx := context.Background()
	owner := seedCredentialOwner(t, st)
	now := time.Now().UTC().Truncate(time.Second)

	want := credential(owner, "agc-1", "hash-1", "bf660d26", now)
	if _, err := st.InsertAgentCredential(ctx, want); err != nil {
		t.Fatalf("InsertAgentCredential: %v", err)
	}
	got, ok, err := st.GetAgentCredentialByTokenHash(ctx, "hash-1")
	if err != nil || !ok {
		t.Fatalf("GetAgentCredentialByTokenHash: %v (ok=%v)", err, ok)
	}
	if got.Role != want.Role || got.UserID != want.UserID || got.ProjectID != want.ProjectID ||
		got.SessionID != want.SessionID || got.WorkflowRunID != want.WorkflowRunID ||
		got.WorkflowStepID != want.WorkflowStepID || got.ReviewRunID != want.ReviewRunID ||
		got.RuntimeInstanceID != want.RuntimeInstanceID || got.Generation != want.Generation {
		t.Fatalf("credential did not round-trip:\n got %+v\nwant %+v", got, want)
	}
	if len(got.Permissions) != len(want.Permissions) {
		t.Fatalf("permissions = %v, want %v", got.Permissions, want.Permissions)
	}
	for i, p := range want.Permissions {
		if got.Permissions[i] != p {
			t.Fatalf("permissions[%d] = %q, want %q", i, got.Permissions[i], p)
		}
	}
	if got.RevokedAt != nil || !got.Active(now) {
		t.Fatalf("a fresh credential is not active: %+v", got)
	}

	if _, ok, err := st.GetAgentCredentialByTokenHash(ctx, "not-a-hash"); err != nil || ok {
		t.Fatalf("an unknown hash resolved (ok=%v, err=%v)", ok, err)
	}
}

// Revocation is aimed at ONE review run, and it leaves the launch record
// behind: the ambiguous-review recovery reads that record as evidence that a
// reviewer was once able to answer, so a revocation must not erase it.
func TestRevokingOneReviewRunLeavesTheOthersAndTheRecord(t *testing.T) {
	st := sqlitetest.MustOpen(t)
	ctx := context.Background()
	owner := seedCredentialOwner(t, st)
	now := time.Now().UTC().Truncate(time.Second)

	for _, c := range []domain.AgentCredential{
		credential(owner, "agc-1", "hash-1", "review-a", now),
		credential(owner, "agc-2", "hash-2", "review-b", now),
	} {
		if _, err := st.InsertAgentCredential(ctx, c); err != nil {
			t.Fatalf("InsertAgentCredential: %v", err)
		}
	}

	n, err := st.RevokeAgentCredentialsForReviewRun(ctx, "review-a", now.Add(time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("RevokeAgentCredentialsForReviewRun = %d, %v; want 1, nil", n, err)
	}
	revoked, _, _ := st.GetAgentCredentialByTokenHash(ctx, "hash-1")
	if revoked.RevokedAt == nil || revoked.Active(now.Add(2*time.Minute)) {
		t.Fatalf("the credential was not revoked: %+v", revoked)
	}
	untouched, _, _ := st.GetAgentCredentialByTokenHash(ctx, "hash-2")
	if untouched.RevokedAt != nil {
		t.Fatalf("revoking one review run revoked another's credential")
	}
	// The record survives.
	all, err := st.ListAgentCredentialsForReviewRun(ctx, "review-a")
	if err != nil || len(all) != 1 {
		t.Fatalf("ListAgentCredentialsForReviewRun = %d rows, %v; want 1, nil", len(all), err)
	}
	// A second revoke is a no-op rather than an error, so a replayed close-out
	// is safe.
	if n, err := st.RevokeAgentCredentialsForReviewRun(ctx, "review-a", now.Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("second revoke = %d, %v; want 0, nil", n, err)
	}
}

// A session's own termination path revokes everything bound to it.
func TestRevokingASessionRevokesItsAgents(t *testing.T) {
	st := sqlitetest.MustOpen(t)
	ctx := context.Background()
	owner := seedCredentialOwner(t, st)
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := st.InsertAgentCredential(ctx, credential(owner, "agc-1", "hash-1", "review-a", now)); err != nil {
		t.Fatalf("InsertAgentCredential: %v", err)
	}
	n, err := st.RevokeAgentCredentialsForSession(ctx, "agent-orchestrator-59", now.Add(time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("RevokeAgentCredentialsForSession = %d, %v; want 1, nil", n, err)
	}
	if got, _, _ := st.GetAgentCredentialByTokenHash(ctx, "hash-1"); got.RevokedAt == nil {
		t.Fatalf("the session's agent credential was not revoked")
	}
}

// Last-seen is recorded for a live credential and refused for a revoked one --
// a revoked credential must not look like it is still in use.
func TestTouchingAgentCredentialLastSeen(t *testing.T) {
	st := sqlitetest.MustOpen(t)
	ctx := context.Background()
	owner := seedCredentialOwner(t, st)
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := st.InsertAgentCredential(ctx, credential(owner, "agc-1", "hash-1", "review-a", now)); err != nil {
		t.Fatalf("InsertAgentCredential: %v", err)
	}
	later := now.Add(5 * time.Minute)
	if ok, err := st.TouchAgentCredentialLastSeen(ctx, "agc-1", later); err != nil || !ok {
		t.Fatalf("TouchAgentCredentialLastSeen = %v, %v; want true, nil", ok, err)
	}
	got, _, _ := st.GetAgentCredentialByTokenHash(ctx, "hash-1")
	if !got.LastSeenAt.Equal(later) {
		t.Fatalf("last seen = %v, want %v", got.LastSeenAt, later)
	}
	if _, err := st.RevokeAgentCredentialsForReviewRun(ctx, "review-a", later); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if ok, err := st.TouchAgentCredentialLastSeen(ctx, "agc-1", later.Add(time.Minute)); err != nil || ok {
		t.Fatalf("a revoked credential was touched (ok=%v, err=%v)", ok, err)
	}
}
