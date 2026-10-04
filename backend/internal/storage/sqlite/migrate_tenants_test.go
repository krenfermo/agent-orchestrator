package sqlite

import (
	"database/sql"
	"testing"
)

// migrate_tenants_test.go -- the durable half of P4-C (migration 0156).
//
// Every test here seeds a database at 0155 (the real pre-P4-C schema, with
// P4-B accounts, teams and grants and P4-D notifications already in it), then
// migrates to head. That ordering is the point: the risk in this migration is
// not whether it produces the right shape on an empty database, it is whether
// an installation that already has somebody's work in it comes through with
// every project, owner, grant and permission exactly as it was.

// seedPreP4CInstallation writes the state a real single-user installation
// would already hold at 0155: an owner, a colleague, two projects the owner
// registered, a team with a member and a grant, and a notification.
func seedPreP4CInstallation(t *testing.T, db *sql.DB) {
	t.Helper()
	seedUser(t, db, "owner-1", "ada@example.com")
	if _, err := db.Exec(`UPDATE users SET role = 'owner' WHERE id = 'owner-1'`); err != nil {
		t.Fatalf("promote owner: %v", err)
	}
	seedUser(t, db, "member-1", "grace@example.com")
	seedUser(t, db, "viewer-1", "linus@example.com")
	if _, err := db.Exec(`UPDATE users SET role = 'viewer' WHERE id = 'viewer-1'`); err != nil {
		t.Fatalf("demote viewer: %v", err)
	}
	seedProjectRow(t, db, "proj-one")
	seedProjectRow(t, db, "proj-two")
	if _, err := db.Exec(`UPDATE projects SET owner_user_id = 'owner-1'`); err != nil {
		t.Fatalf("stamp project owners: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO teams (id, name, slug, description, status, created_at, updated_at)
		VALUES ('team-1', 'Platform', 'platform', '', 'active', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
	); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO team_memberships (id, team_id, user_id, role, created_at, updated_at)
		VALUES ('tm-1', 'team-1', 'member-1', 'member', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
	); err != nil {
		t.Fatalf("seed team membership: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO project_grants (id, project_id, subject_kind, subject_id, role, created_at, updated_at, created_by)
		VALUES ('g-1', 'proj-two', 'team', 'team-1', 'member', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, 'owner-1')`,
	); err != nil {
		t.Fatalf("seed grant: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO notifications (id, project_id, workflow_run_id, type, title, created_at)
		VALUES ('n-1', 'proj-one', 'run-1', 'needs_input', 'Needs input', CURRENT_TIMESTAMP)`,
	); err != nil {
		t.Fatalf("seed notification: %v", err)
	}
}

// The headline promise of Step 3: no project loss, no ownership loss, no
// permission widening. Everything an installation had at 0155 is still there
// at head, and now has an organization.
func TestTenantMigrationPreservesAnExistingInstallation(t *testing.T) {
	db := migratedDB(t)
	upTo(t, db, 155)
	seedPreP4CInstallation(t, db)

	if err := migrate(db); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}

	var projects int
	if err := db.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(&projects); err != nil {
		t.Fatalf("count projects: %v", err)
	}
	if projects != 2 {
		t.Fatalf("projects after migration = %d, want 2", projects)
	}

	// Ownership survives, and every project is now in the default organization.
	rows, err := db.Query(`SELECT id, owner_user_id, tenant_id FROM projects ORDER BY id`)
	if err != nil {
		t.Fatalf("read projects: %v", err)
	}
	defer rows.Close()
	seen := map[string][2]string{}
	for rows.Next() {
		var id, owner, tenant string
		if err := rows.Scan(&id, &owner, &tenant); err != nil {
			t.Fatalf("scan project: %v", err)
		}
		seen[id] = [2]string{owner, tenant}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate projects: %v", err)
	}
	for _, id := range []string{"proj-one", "proj-two"} {
		got, ok := seen[id]
		if !ok {
			t.Fatalf("project %s disappeared across the migration", id)
		}
		if got[0] != "owner-1" {
			t.Fatalf("project %s owner = %q, want owner-1", id, got[0])
		}
		if got[1] != "tnt_default" {
			t.Fatalf("project %s tenant = %q, want tnt_default", id, got[1])
		}
	}

	// The grant is untouched: P4-C adds no column to project_grants and moves
	// no row. A grant that stopped working after the migration would be a
	// permission LOSS, which is just as much a bug as a widening.
	var grantRole, grantSubject string
	if err := db.QueryRow(
		`SELECT role, subject_id FROM project_grants WHERE id = 'g-1'`,
	).Scan(&grantRole, &grantSubject); err != nil {
		t.Fatalf("read grant: %v", err)
	}
	if grantRole != "member" || grantSubject != "team-1" {
		t.Fatalf("grant changed: role=%q subject=%q", grantRole, grantSubject)
	}

	// The team came with its organization, and its membership survived.
	var teamTenant string
	if err := db.QueryRow(`SELECT tenant_id FROM teams WHERE id = 'team-1'`).Scan(&teamTenant); err != nil {
		t.Fatalf("read team tenant: %v", err)
	}
	if teamTenant != "tnt_default" {
		t.Fatalf("team tenant = %q, want tnt_default", teamTenant)
	}
	var teamMembers int
	if err := db.QueryRow(`SELECT COUNT(*) FROM team_memberships WHERE team_id = 'team-1'`).Scan(&teamMembers); err != nil {
		t.Fatalf("count team members: %v", err)
	}
	if teamMembers != 1 {
		t.Fatalf("team memberships = %d, want 1", teamMembers)
	}

	// P4-D's notification is still attached to its project.
	var notificationProject string
	if err := db.QueryRow(`SELECT project_id FROM notifications WHERE id = 'n-1'`).Scan(&notificationProject); err != nil {
		t.Fatalf("read notification: %v", err)
	}
	if notificationProject != "proj-one" {
		t.Fatalf("notification project = %q, want proj-one", notificationProject)
	}
}

// The backfill maps each account's INSTALLATION role onto the same-named
// organization role. An installation viewer must arrive as an organization
// VIEWER, not a member: a migration that promotes anyone is a permission
// widening, and this is the row where that would happen silently.
func TestTenantMigrationBackfillsMembershipWithoutWidening(t *testing.T) {
	db := migratedDB(t)
	upTo(t, db, 155)
	seedPreP4CInstallation(t, db)

	if err := migrate(db); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}

	want := map[string]string{
		"owner-1":  "owner",
		"member-1": "member",
		"viewer-1": "viewer",
	}
	for user, wantRole := range want {
		var tenant, role string
		if err := db.QueryRow(
			`SELECT tenant_id, role FROM tenant_memberships WHERE user_id = ?`, user,
		).Scan(&tenant, &role); err != nil {
			t.Fatalf("read membership for %s: %v", user, err)
		}
		if tenant != "tnt_default" {
			t.Fatalf("%s joined %q, want tnt_default", user, tenant)
		}
		if role != wantRole {
			t.Fatalf("%s has organization role %q, want %q", user, role, wantRole)
		}
	}

	var memberships int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tenant_memberships`).Scan(&memberships); err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	if memberships != 3 {
		t.Fatalf("memberships = %d, want exactly one per account (3)", memberships)
	}
}

// A fresh database and a migrated one must converge on the same shape, so a
// test that passes on one is evidence about the other. The default
// organization exists either way, with no accounts and no projects to attach.
func TestTenantMigrationOnAFreshDatabase(t *testing.T) {
	db := migratedDB(t)
	if err := migrate(db); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}

	var id, slug, status string
	if err := db.QueryRow(
		`SELECT id, slug, status FROM tenants WHERE id = 'tnt_default'`,
	).Scan(&id, &slug, &status); err != nil {
		t.Fatalf("read default tenant: %v", err)
	}
	if slug != "default" || status != "active" {
		t.Fatalf("default tenant = slug %q status %q", slug, status)
	}
	var tenants, memberships int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tenants`).Scan(&tenants); err != nil {
		t.Fatalf("count tenants: %v", err)
	}
	if tenants != 1 {
		t.Fatalf("a fresh installation has %d organizations, want exactly 1", tenants)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM tenant_memberships`).Scan(&memberships); err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	if memberships != 0 {
		t.Fatalf("a fresh installation has %d memberships, want 0", memberships)
	}
}

// A project inserted without naming an organization lands in the default one
// rather than in none. This is the column default, and it is what makes "no
// tenant-less project after P4-C" a property of the schema rather than a rule
// every write path has to remember.
func TestTenantMigrationGivesEveryNewProjectAHome(t *testing.T) {
	db := migratedDB(t)
	if err := migrate(db); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	seedProjectRow(t, db, "proj-new")

	var tenant string
	if err := db.QueryRow(`SELECT tenant_id FROM projects WHERE id = 'proj-new'`).Scan(&tenant); err != nil {
		t.Fatalf("read new project tenant: %v", err)
	}
	if tenant != "tnt_default" {
		t.Fatalf("a project registered without an organization landed in %q, want tnt_default", tenant)
	}
}

// The role vocabulary is a closed set, so a hand-edited row cannot invent an
// organization role the evaluator has never heard of.
func TestTenantMigrationClosesTheRoleVocabularies(t *testing.T) {
	db := migratedDB(t)
	if err := migrate(db); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	seedUser(t, db, "u1", "ada@example.com")

	if _, err := db.Exec(`
		INSERT INTO tenant_memberships (id, tenant_id, user_id, role, created_at, updated_at)
		VALUES ('m1', 'tnt_default', 'u1', 'superuser', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
	); err == nil {
		t.Fatal("the schema accepted an organization role outside the closed set")
	}
	if _, err := db.Exec(`
		INSERT INTO tenants (id, name, slug, status, created_at, updated_at)
		VALUES ('t2', 'Other', 'other', 'deleted', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
	); err == nil {
		t.Fatal("the schema accepted an organization status outside the closed set")
	}
	// One membership per (organization, account), so re-adding somebody
	// changes their role rather than creating a second row that disagrees.
	if _, err := db.Exec(`
		INSERT INTO tenant_memberships (id, tenant_id, user_id, role, created_at, updated_at)
		VALUES ('m2', 'tnt_default', 'u1', 'member', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
	); err != nil {
		t.Fatalf("seed first membership: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO tenant_memberships (id, tenant_id, user_id, role, created_at, updated_at)
		VALUES ('m3', 'tnt_default', 'u1', 'admin', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
	); err == nil {
		t.Fatal("the schema accepted two memberships for one account in one organization")
	}
}

// The Down migration is a real rollback, not a one-way door: it removes only
// what the Up added, and every row that existed before comes back untouched.
// It is exercised against post-Up data with real projects, grants and
// memberships in it, because that is the only version of the rollback anybody
// will ever run.
func TestTenantMigrationRollsBackAgainstRealData(t *testing.T) {
	db := migratedDB(t)
	upTo(t, db, 155)
	seedPreP4CInstallation(t, db)
	if err := migrate(db); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}

	if err := downTo(t, db, 155); err != nil {
		t.Fatalf("roll back 0156: %v", err)
	}

	// The organization tables are gone.
	for _, table := range []string{"tenants", "tenant_memberships"} {
		var n int
		err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n)
		if err != nil {
			t.Fatalf("inspect schema for %s: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("%s survived the rollback", table)
		}
	}
	// And so is the column, on both tables that gained one.
	if _, err := db.Exec(`SELECT tenant_id FROM projects LIMIT 1`); err == nil {
		t.Fatal("projects.tenant_id survived the rollback")
	}
	if _, err := db.Exec(`SELECT tenant_id FROM teams LIMIT 1`); err == nil {
		t.Fatal("teams.tenant_id survived the rollback")
	}

	// Everything that predates P4-C is exactly as it was. A rolled-back
	// installation is a working single-organization installation, which is
	// precisely what it was before.
	var projects, grants, teams, memberships, users int
	for _, probe := range []struct {
		query string
		into  *int
		want  int
		what  string
	}{
		{`SELECT COUNT(*) FROM projects`, &projects, 2, "projects"},
		{`SELECT COUNT(*) FROM project_grants`, &grants, 1, "project grants"},
		{`SELECT COUNT(*) FROM teams`, &teams, 1, "teams"},
		{`SELECT COUNT(*) FROM team_memberships`, &memberships, 1, "team memberships"},
		{`SELECT COUNT(*) FROM users`, &users, 3, "users"},
	} {
		if err := db.QueryRow(probe.query).Scan(probe.into); err != nil {
			t.Fatalf("count %s after rollback: %v", probe.what, err)
		}
		if *probe.into != probe.want {
			t.Fatalf("%s after rollback = %d, want %d", probe.what, *probe.into, probe.want)
		}
	}
	var owner string
	if err := db.QueryRow(`SELECT owner_user_id FROM projects WHERE id = 'proj-one'`).Scan(&owner); err != nil {
		t.Fatalf("read owner after rollback: %v", err)
	}
	if owner != "owner-1" {
		t.Fatalf("project owner after rollback = %q, want owner-1", owner)
	}

	// And it migrates forward again, which is what makes the rollback usable
	// rather than a trapdoor.
	if err := migrate(db); err != nil {
		t.Fatalf("re-migrate after rollback: %v", err)
	}
	var tenant string
	if err := db.QueryRow(`SELECT tenant_id FROM projects WHERE id = 'proj-one'`).Scan(&tenant); err != nil {
		t.Fatalf("read tenant after re-migration: %v", err)
	}
	if tenant != "tnt_default" {
		t.Fatalf("tenant after re-migration = %q, want tnt_default", tenant)
	}
}
