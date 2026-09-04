package rbac_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	authsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/authsvc"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/rbac"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// These tests run against the REAL SQLite store, not a fake. The invariants
// under test -- one owner, always; a transfer that is atomic under concurrent
// callers -- are enforced by a partial unique index and a transaction, and a
// fake that reimplemented them would only be testing itself.

type recordingAudit struct {
	mu     sync.Mutex
	events []rbac.Event
}

func (a *recordingAudit) Record(_ context.Context, ev rbac.Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, ev)
}

func (a *recordingAudit) names() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.events))
	for _, e := range a.events {
		out = append(out, e.Name)
	}
	return out
}

type creator struct{ mgr authsvc.Manager }

func (c creator) CreateUser(ctx context.Context, in rbac.CreateUserInput) (domain.User, error) {
	return c.mgr.CreateUser(ctx, authsvc.CreateUserInput{
		DisplayName: in.DisplayName, Email: in.Email, Username: in.Username,
		Password: in.Password, Role: in.Role,
	})
}

type fixture struct {
	store *store.Store
	auth  authsvc.Manager
	svc   *rbac.Service
	audit *recordingAudit
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st := sqlitetest.MustOpen(t)
	auth := authsvc.New(st, nil)
	audit := &recordingAudit{}
	return &fixture{store: st, auth: auth, svc: rbac.New(st, creator{auth}, audit, nil), audit: audit}
}

func (f *fixture) mustUser(t *testing.T, name string, role domain.UserRole) domain.User {
	t.Helper()
	u, err := f.auth.CreateUser(context.Background(), authsvc.CreateUserInput{
		DisplayName: name, Email: name + "@example.test", Username: name,
		Password: "correct-horse-battery", Role: role,
	})
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return u
}

func actorOf(u domain.User) domain.Principal {
	return domain.Principal{User: u, AuthMethod: domain.AuthMethodPassword}
}

func codeOf(err error) string {
	var e *apierr.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// TestOwnerCannotBeDemoted is the last-owner lockout guard. Ownership moves by
// transfer; there is no sequence of single-role edits that reaches zero owners.
func TestOwnerCannotBeDemoted(t *testing.T) {
	f := newFixture(t)
	owner := f.mustUser(t, "owner", domain.UserRoleOwner)

	_, err := f.svc.SetUserRole(context.Background(), actorOf(owner), owner.ID, domain.UserRoleAdmin)
	if codeOf(err) != rbac.CodeLastOwner {
		t.Fatalf("demoting the owner: got %v, want %s", err, rbac.CodeLastOwner)
	}

	owners, err := f.store.CountOwners(context.Background())
	if err != nil || owners != 1 {
		t.Fatalf("owners = %d (err %v), want 1", owners, err)
	}
}

// TestOwnerCannotBeDisabled: disabling the owner is the same lockout by
// another route, and is refused by the same invariant.
func TestOwnerCannotBeDisabled(t *testing.T) {
	f := newFixture(t)
	owner := f.mustUser(t, "owner", domain.UserRoleOwner)
	admin := f.mustUser(t, "admin", domain.UserRoleAdmin)

	_, err := f.svc.SetUserStatus(context.Background(), actorOf(admin), owner.ID, domain.UserStatusDisabled)
	if codeOf(err) != rbac.CodeLastOwner {
		t.Fatalf("disabling the owner: got %v, want %s", err, rbac.CodeLastOwner)
	}
}

// TestAccountCannotDisableItself stops the other self-inflicted lockout: an
// administrator who disables their own account.
func TestAccountCannotDisableItself(t *testing.T) {
	f := newFixture(t)
	_ = f.mustUser(t, "owner", domain.UserRoleOwner)
	admin := f.mustUser(t, "admin", domain.UserRoleAdmin)

	_, err := f.svc.SetUserStatus(context.Background(), actorOf(admin), admin.ID, domain.UserStatusDisabled)
	if codeOf(err) != rbac.CodeSelfDisable {
		t.Fatalf("self-disable: got %v, want %s", err, rbac.CodeSelfDisable)
	}
}

// TestOnlyTheOwnerTransfersOwnership: an administrator manages accounts, but
// an administrator who could also seize the installation would make the owner
// role decorative.
func TestOnlyTheOwnerTransfersOwnership(t *testing.T) {
	f := newFixture(t)
	owner := f.mustUser(t, "owner", domain.UserRoleOwner)
	admin := f.mustUser(t, "admin", domain.UserRoleAdmin)
	member := f.mustUser(t, "member", domain.UserRoleMember)

	if _, err := f.svc.SetUserRole(context.Background(), actorOf(admin), member.ID, domain.UserRoleOwner); codeOf(err) != rbac.CodeOwnerProtected {
		t.Fatalf("admin transferring ownership: got %v, want %s", err, rbac.CodeOwnerProtected)
	}

	promoted, err := f.svc.SetUserRole(context.Background(), actorOf(owner), member.ID, domain.UserRoleOwner)
	if err != nil {
		t.Fatalf("owner transferring ownership: %v", err)
	}
	if promoted.Role != domain.UserRoleOwner {
		t.Fatalf("target role = %s, want owner", promoted.Role)
	}
	previous, err := f.svc.GetUser(context.Background(), owner.ID)
	if err != nil {
		t.Fatalf("reload previous owner: %v", err)
	}
	if previous.Role != domain.UserRoleAdmin {
		t.Fatalf("previous owner role = %s, want admin", previous.Role)
	}
}

// TestConcurrentOwnershipTransfersLeaveExactlyOneOwner is section 29. Two
// simultaneous transfers race; the unique index picks a winner and the loser's
// demotion rolls back with it, so the installation is never ownerless and
// never doubly owned.
func TestConcurrentOwnershipTransfersLeaveExactlyOneOwner(t *testing.T) {
	f := newFixture(t)
	owner := f.mustUser(t, "owner", domain.UserRoleOwner)
	first := f.mustUser(t, "first", domain.UserRoleAdmin)
	second := f.mustUser(t, "second", domain.UserRoleAdmin)

	var wg sync.WaitGroup
	results := make([]error, 2)
	targets := []domain.UserID{first.ID, second.ID}
	start := make(chan struct{})
	for i := range targets {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, results[i] = f.svc.SetUserRole(context.Background(), actorOf(owner), targets[i], domain.UserRoleOwner)
		}(i)
	}
	close(start)
	wg.Wait()

	owners, err := f.store.CountOwners(context.Background())
	if err != nil {
		t.Fatalf("count owners: %v", err)
	}
	if owners != 1 {
		t.Fatalf("owners after concurrent transfers = %d, want exactly 1 (errors: %v)", owners, results)
	}
}

// TestOwnerlessInstallationCanAppointOne is the recovery direction of the same
// invariant: when no owner exists, ownership can be assigned without a
// transfer -- otherwise a legacy multi-user install could never get one.
func TestOwnerlessInstallationCanAppointOne(t *testing.T) {
	f := newFixture(t)
	admin := f.mustUser(t, "admin", domain.UserRoleAdmin)
	member := f.mustUser(t, "member", domain.UserRoleMember)

	if _, err := f.svc.SetUserRole(context.Background(), actorOf(admin), member.ID, domain.UserRoleOwner); err != nil {
		t.Fatalf("appointing an owner on an ownerless installation: %v", err)
	}
	owners, err := f.store.CountOwners(context.Background())
	if err != nil || owners != 1 {
		t.Fatalf("owners = %d (err %v), want 1", owners, err)
	}
}

// TestOwnershipIsNotAssignableAtCreation: an owner is transferred to, never
// minted. Allowing creation with role=owner would be a second, unguarded way
// to reach two owners.
func TestOwnershipIsNotAssignableAtCreation(t *testing.T) {
	f := newFixture(t)
	owner := f.mustUser(t, "owner", domain.UserRoleOwner)

	_, err := f.svc.CreateUser(context.Background(), actorOf(owner), rbac.CreateUserInput{
		DisplayName: "second", Email: "second@example.test", Username: "second",
		Password: "correct-horse-battery", Role: domain.UserRoleOwner,
	})
	if codeOf(err) != rbac.CodeOwnerProtected {
		t.Fatalf("creating an owner: got %v, want %s", err, rbac.CodeOwnerProtected)
	}
}

// TestTeamLifecycleAndMembership covers the durable team object end to end,
// including that deleting a team also deletes the project grants made to it --
// a grant whose subject no longer exists is access nobody can see or revoke.
func TestTeamLifecycleAndMembership(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	owner := f.mustUser(t, "owner", domain.UserRoleOwner)
	member := f.mustUser(t, "member", domain.UserRoleMember)
	seedProject(t, f.store, "p1")

	team, err := f.svc.CreateTeam(ctx, actorOf(owner), "Platform Team", "owns the runtime")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if team.Slug != "platform-team" {
		t.Fatalf("slug = %q, want platform-team", team.Slug)
	}
	if _, err := f.svc.CreateTeam(ctx, actorOf(owner), "platform team", ""); codeOf(err) != rbac.CodeTeamExists {
		t.Fatalf("duplicate team name: got %v, want %s", err, rbac.CodeTeamExists)
	}

	if _, err := f.svc.AddTeamMember(ctx, actorOf(owner), team.ID, member.ID, domain.TeamRoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	members, err := f.svc.ListTeamMembers(ctx, team.ID)
	if err != nil || len(members) != 1 {
		t.Fatalf("members = %v (err %v), want 1", members, err)
	}

	if _, err := f.svc.GrantProjectAccess(ctx, actorOf(owner), "p1", domain.GrantSubjectTeam, string(team.ID), domain.ProjectRoleMember); err != nil {
		t.Fatalf("grant team access: %v", err)
	}

	if err := f.svc.DeleteTeam(ctx, actorOf(owner), team.ID); err != nil {
		t.Fatalf("delete team: %v", err)
	}
	grants, err := f.store.ListProjectGrants(ctx, "p1")
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants) != 0 {
		t.Fatalf("grants after team deletion = %v, want none", grants)
	}
}

// TestArchivedTeamKeepsItsRowsButStopsConferringAccess: archiving is the safe
// form of deletion, and reactivating restores exactly what was there.
func TestArchivedTeamKeepsItsRowsButStopsConferringAccess(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	owner := f.mustUser(t, "owner", domain.UserRoleOwner)
	member := f.mustUser(t, "member", domain.UserRoleMember)
	seedProject(t, f.store, "p1")

	team, err := f.svc.CreateTeam(ctx, actorOf(owner), "Platform", "")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if _, err := f.svc.AddTeamMember(ctx, actorOf(owner), team.ID, member.ID, domain.TeamRoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if _, err := f.svc.GrantProjectAccess(ctx, actorOf(owner), "p1", domain.GrantSubjectTeam, string(team.ID), domain.ProjectRoleMember); err != nil {
		t.Fatalf("grant: %v", err)
	}

	active, err := f.store.ListActiveTeamIDsForUser(ctx, member.ID)
	if err != nil || len(active) != 1 {
		t.Fatalf("active teams = %v (err %v), want 1", active, err)
	}

	if _, err := f.svc.ArchiveTeam(ctx, actorOf(owner), team.ID, true); err != nil {
		t.Fatalf("archive: %v", err)
	}
	active, err = f.store.ListActiveTeamIDsForUser(ctx, member.ID)
	if err != nil || len(active) != 0 {
		t.Fatalf("active teams after archive = %v (err %v), want none", active, err)
	}
	members, err := f.svc.ListTeamMembers(ctx, team.ID)
	if err != nil || len(members) != 1 {
		t.Fatalf("archiving destroyed the membership: %v (err %v)", members, err)
	}

	if _, err := f.svc.ArchiveTeam(ctx, actorOf(owner), team.ID, false); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	active, err = f.store.ListActiveTeamIDsForUser(ctx, member.ID)
	if err != nil || len(active) != 1 {
		t.Fatalf("active teams after reactivation = %v (err %v), want 1", active, err)
	}
}

// TestGrantToUnknownSubjectIsRefused stops the row that turns into a security
// incident years later: an access rule pointing at an id nobody can resolve.
func TestGrantToUnknownSubjectIsRefused(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	owner := f.mustUser(t, "owner", domain.UserRoleOwner)
	seedProject(t, f.store, "p1")

	if _, err := f.svc.GrantProjectAccess(ctx, actorOf(owner), "p1", domain.GrantSubjectUser, "ghost", domain.ProjectRoleMember); codeOf(err) != rbac.CodeUserNotFound {
		t.Fatalf("grant to unknown user: got %v, want %s", err, rbac.CodeUserNotFound)
	}
	if _, err := f.svc.GrantProjectAccess(ctx, actorOf(owner), "p1", domain.GrantSubjectTeam, "ghost", domain.ProjectRoleMember); codeOf(err) != rbac.CodeTeamNotFound {
		t.Fatalf("grant to unknown team: got %v, want %s", err, rbac.CodeTeamNotFound)
	}
}

// TestRegrantingChangesTheRoleInPlace: the unique index makes re-granting an
// upsert, so an access list can never hold two contradictory rows for the same
// subject.
func TestRegrantingChangesTheRoleInPlace(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	owner := f.mustUser(t, "owner", domain.UserRoleOwner)
	member := f.mustUser(t, "member", domain.UserRoleMember)
	seedProject(t, f.store, "p1")

	for _, role := range []domain.ProjectRole{domain.ProjectRoleViewer, domain.ProjectRoleAdmin} {
		if _, err := f.svc.GrantProjectAccess(ctx, actorOf(owner), "p1", domain.GrantSubjectUser, string(member.ID), role); err != nil {
			t.Fatalf("grant %s: %v", role, err)
		}
	}
	access, err := f.svc.ListProjectAccess(ctx, "p1")
	if err != nil {
		t.Fatalf("list access: %v", err)
	}
	if len(access.Grants) != 1 || access.Grants[0].Role != domain.ProjectRoleAdmin {
		t.Fatalf("grants = %v, want exactly one admin grant", access.Grants)
	}
}

// TestAuthorizationChangesAreAudited is section 22: every meaningful change to
// who can do what leaves a record naming the actor and the target.
func TestAuthorizationChangesAreAudited(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	owner := f.mustUser(t, "owner", domain.UserRoleOwner)
	member := f.mustUser(t, "member", domain.UserRoleMember)
	seedProject(t, f.store, "p1")

	if _, err := f.svc.CreateUser(ctx, actorOf(owner), rbac.CreateUserInput{
		DisplayName: "new", Email: "new@example.test", Username: "new",
		Password: "correct-horse-battery", Role: domain.UserRoleMember,
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := f.svc.SetUserRole(ctx, actorOf(owner), member.ID, domain.UserRoleAdmin); err != nil {
		t.Fatalf("set role: %v", err)
	}
	if _, err := f.svc.SetUserStatus(ctx, actorOf(owner), member.ID, domain.UserStatusDisabled); err != nil {
		t.Fatalf("disable: %v", err)
	}
	team, err := f.svc.CreateTeam(ctx, actorOf(owner), "Platform", "")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if _, err := f.svc.AddTeamMember(ctx, actorOf(owner), team.ID, member.ID, domain.TeamRoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if err := f.svc.RemoveTeamMember(ctx, actorOf(owner), team.ID, member.ID); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	if _, err := f.svc.GrantProjectAccess(ctx, actorOf(owner), "p1", domain.GrantSubjectUser, string(member.ID), domain.ProjectRoleViewer); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := f.svc.RevokeProjectAccess(ctx, actorOf(owner), "p1", domain.GrantSubjectUser, string(member.ID)); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	want := []string{
		rbac.EventUserCreated, rbac.EventUserRoleChanged, rbac.EventUserDisabled,
		rbac.EventTeamCreated, rbac.EventTeamMemberAdded, rbac.EventTeamMemberRemoved,
		rbac.EventProjectAccessGranted, rbac.EventProjectAccessRevoked,
	}
	got := f.audit.names()
	for _, name := range want {
		found := false
		for _, g := range got {
			if g == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no audit event %q; got %v", name, got)
		}
	}

	f.audit.mu.Lock()
	defer f.audit.mu.Unlock()
	for _, ev := range f.audit.events {
		if ev.Actor.User.ID != owner.ID {
			t.Errorf("audit event %s did not record the acting principal", ev.Name)
		}
		if ev.TargetID == "" {
			t.Errorf("audit event %s did not record a target", ev.Name)
		}
	}
}

func seedProject(t *testing.T, st *store.Store, id domain.ProjectID) {
	t.Helper()
	if err := st.UpsertProject(context.Background(), domain.ProjectRecord{
		ID:           string(id),
		Path:         "/tmp/" + string(id),
		RegisteredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed project %s: %v", id, err)
	}
}
