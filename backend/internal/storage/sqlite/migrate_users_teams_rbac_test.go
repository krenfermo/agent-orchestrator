package sqlite

import (
	"database/sql"
	"fmt"
	"testing"
)

// migrate_users_teams_rbac_test.go — the durable half of P4-B (migration 0152).
//
// The properties checked here belong to the SCHEMA and would survive a rewrite
// of every Go file above it: existing accounts and the single-owner invariant
// come through the users rebuild intact, a grant subject holds one role per
// project, a team's rows disappear with the team, and the role vocabularies are
// closed sets rather than free text.

// A user row written by a pre-0152 build must arrive unchanged -- same id, same
// role, same everything. This is the whole risk of rebuilding a table to widen
// a CHECK constraint, so it is the first thing tested.
func TestRBACMigrationPreservesExistingAccounts(t *testing.T) {
	db := migratedDB(t)
	upTo(t, db, 151)
	seedUser(t, db, "u1", "ada@example.com")
	if _, err := db.Exec(`UPDATE users SET role = 'owner' WHERE id = 'u1'`); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	seedUser(t, db, "u2", "grace@example.com")
	if _, err := db.Exec(`
		INSERT INTO auth_sessions (id, user_id, token_hash, created_at, expires_at, last_seen_at)
		VALUES ('s1', 'u1', 'hash-1', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	if err := migrate(db); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}

	var role, status, email string
	if err := db.QueryRow(`SELECT role, status, email FROM users WHERE id = 'u1'`).Scan(&role, &status, &email); err != nil {
		t.Fatalf("read migrated owner: %v", err)
	}
	if role != "owner" || status != "active" || email != "ada@example.com" {
		t.Fatalf("owner row changed across the rebuild: role=%q status=%q email=%q", role, status, email)
	}
	var members int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'member'`).Scan(&members); err != nil {
		t.Fatalf("count members: %v", err)
	}
	if members != 1 {
		t.Fatalf("members after migration = %d, want 1", members)
	}
	// The session still resolves to its user: the rebuild must not have
	// orphaned the rows that reference users.
	var sessionUser string
	if err := db.QueryRow(`SELECT user_id FROM auth_sessions WHERE id = 's1'`).Scan(&sessionUser); err != nil {
		t.Fatalf("read session after rebuild: %v", err)
	}
	if sessionUser != "u1" {
		t.Fatalf("session user = %q, want u1", sessionUser)
	}
}

// The single-owner index is the concurrency-safety mechanism 8P-E.8 introduced
// and P4-B's ownership transfer still depends on. Rebuilding the table must
// recreate it, not quietly drop it.
func TestRBACMigrationKeepsTheSingleOwnerInvariant(t *testing.T) {
	db := migratedDB(t)
	if err := migrate(db); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	seedUser(t, db, "u1", "ada@example.com")
	seedUser(t, db, "u2", "grace@example.com")
	if _, err := db.Exec(`UPDATE users SET role = 'owner' WHERE id = 'u1'`); err != nil {
		t.Fatalf("promote first owner: %v", err)
	}
	if _, err := db.Exec(`UPDATE users SET role = 'owner' WHERE id = 'u2'`); err == nil {
		t.Fatal("a second owner was accepted; ux_users_single_owner is missing after the rebuild")
	}
}

// The widened role vocabulary must accept exactly the four roles this build
// knows and nothing else. An open column would let a typo become an
// unauthorized account that the evaluator then has to guess about.
func TestRBACMigrationRoleVocabularyIsClosed(t *testing.T) {
	db := migratedDB(t)
	if err := migrate(db); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	for _, role := range []string{"owner", "admin", "member", "viewer"} {
		seedUser(t, db, "u-"+role, role+"@example.com")
		if _, err := db.Exec(`UPDATE users SET role = ? WHERE id = ?`, role, "u-"+role); err != nil {
			t.Fatalf("role %q must be accepted: %v", role, err)
		}
		// Put it back so the single-owner index does not reject the next loop.
		if _, err := db.Exec(`UPDATE users SET role = 'member' WHERE id = ?`, "u-"+role); err != nil {
			t.Fatalf("reset role: %v", err)
		}
	}
	seedUser(t, db, "u-bad", "bad@example.com")
	if _, err := db.Exec(`UPDATE users SET role = 'superuser' WHERE id = 'u-bad'`); err == nil {
		t.Fatal("an unknown role was accepted; the CHECK constraint is missing")
	}
}

// One subject holds at most one role per project. Without this index a
// re-grant becomes a second row and an access list can show two contradictory
// answers for the same person.
func TestRBACMigrationProjectGrantsAreUniquePerSubject(t *testing.T) {
	db := migratedDB(t)
	if err := migrate(db); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	seedProjectRow(t, db, "p1")
	insert := func() error {
		_, err := db.Exec(`
			INSERT INTO project_grants (id, project_id, subject_kind, subject_id, role, created_at, updated_at, created_by)
			VALUES (?, 'p1', 'user', 'u1', 'member', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, '')`,
			randomRowID(),
		)
		return err
	}
	if err := insert(); err != nil {
		t.Fatalf("first grant: %v", err)
	}
	if err := insert(); err == nil {
		t.Fatal("a duplicate grant was accepted; ux_project_grants_subject is missing")
	}

	if _, err := db.Exec(`
		INSERT INTO project_grants (id, project_id, subject_kind, subject_id, role, created_at, updated_at, created_by)
		VALUES ('g-bad', 'p1', 'group', 'x', 'member', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, '')`,
	); err == nil {
		t.Fatal("an unknown subject kind was accepted; the CHECK constraint is missing")
	}
	if _, err := db.Exec(`
		INSERT INTO project_grants (id, project_id, subject_kind, subject_id, role, created_at, updated_at, created_by)
		VALUES ('g-bad2', 'p1', 'user', 'u2', 'owner', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, '')`,
	); err == nil {
		t.Fatal("a project grant claimed the installation-wide owner role; the CHECK constraint is missing")
	}
}

// A project's grants go with the project, and a team's memberships go with the
// team. Both are cascades in the schema rather than cleanup code that a future
// delete path could forget to call.
func TestRBACMigrationCascadesGrantsAndMemberships(t *testing.T) {
	db := migratedDB(t)
	if err := migrate(db); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	seedProjectRow(t, db, "p1")
	seedUser(t, db, "u1", "ada@example.com")
	if _, err := db.Exec(`
		INSERT INTO teams (id, name, slug, description, status, created_at, updated_at)
		VALUES ('t1', 'Platform', 'platform', '', 'active', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
	); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO team_memberships (id, team_id, user_id, role, created_at, updated_at)
		VALUES ('m1', 't1', 'u1', 'member', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
	); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO project_grants (id, project_id, subject_kind, subject_id, role, created_at, updated_at, created_by)
		VALUES ('g1', 'p1', 'team', 't1', 'member', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, '')`,
	); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	if _, err := db.Exec(`DELETE FROM teams WHERE id = 't1'`); err != nil {
		t.Fatalf("delete team: %v", err)
	}
	var memberships int
	if err := db.QueryRow(`SELECT COUNT(*) FROM team_memberships WHERE team_id = 't1'`).Scan(&memberships); err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	if memberships != 0 {
		t.Fatalf("memberships after team deletion = %d, want 0", memberships)
	}

	if _, err := db.Exec(`DELETE FROM projects WHERE id = 'p1'`); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	var grants int
	if err := db.QueryRow(`SELECT COUNT(*) FROM project_grants WHERE project_id = 'p1'`).Scan(&grants); err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if grants != 0 {
		t.Fatalf("grants after project deletion = %d, want 0", grants)
	}
}

// A team's slug is what stops "Platform" and "platform " becoming two teams
// that read identically in every member list.
func TestRBACMigrationTeamSlugIsUnique(t *testing.T) {
	db := migratedDB(t)
	if err := migrate(db); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	insert := func(id string) error {
		_, err := db.Exec(`
			INSERT INTO teams (id, name, slug, description, status, created_at, updated_at)
			VALUES (?, 'Platform', 'platform', '', 'active', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, id)
		return err
	}
	if err := insert("t1"); err != nil {
		t.Fatalf("first team: %v", err)
	}
	if err := insert("t2"); err == nil {
		t.Fatal("a duplicate slug was accepted; ux_teams_slug is missing")
	}
}

// seedProjectRow writes the minimum projects row the grant cascade needs. It
// is deliberately raw SQL rather than the store: this file tests the schema,
// and going through Go code would test the Go code too.
func seedProjectRow(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO projects (id, path, repo_origin_url, display_name, registered_at, config, kind)
		VALUES (?, ?, '', ?, CURRENT_TIMESTAMP, '{}', 'single_repo')`,
		id, "/tmp/"+id, id,
	); err != nil {
		t.Fatalf("seed project %s: %v", id, err)
	}
}

var grantRowSeq int

// randomRowID keeps two inserts in the same test from colliding on the primary
// key, so the failure a duplicate-grant test observes is the UNIQUE index it is
// actually about.
func randomRowID() string {
	grantRowSeq++
	return fmt.Sprintf("g-%d", grantRowSeq)
}
