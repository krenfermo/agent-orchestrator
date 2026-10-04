package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// migrate_oidc_identities_test.go — the durable half of P4-A (migration 0151).
//
// The three properties checked here belong to the SCHEMA, not to any Go code:
// a pre-existing session survives the migration with an honest auth_method, a
// federated identity is unique per (issuer, sub) rather than per email, and a
// login flow's client_kind is a closed set.

func migratedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "ao.db")+pragmas)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedUser(t *testing.T, db *sql.DB, id, email string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO users (id, display_name, email, username, password_hash, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'hash', 'active', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		id, id, email, email,
	); err != nil {
		t.Fatalf("seed user %s: %v", id, err)
	}
}

// A session created by a build that predates P4-A is a PASSWORD session, and
// the migration must say so rather than leaving the column empty and letting
// an audit reader guess.
func TestOIDCMigrationBackfillsExistingSessionsAsPassword(t *testing.T) {
	db := migratedDB(t)
	// Stop just before 0151 so the row is genuinely written at the old schema.
	upTo(t, db, 150)
	seedUser(t, db, "u1", "ada@example.com")
	if _, err := db.Exec(`
		INSERT INTO auth_sessions (id, user_id, token_hash, created_at, expires_at, last_seen_at)
		VALUES ('s1', 'u1', 'hash-1', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
	); err != nil {
		t.Fatalf("seed pre-0151 session: %v", err)
	}

	if err := migrate(db); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}

	var method, issuer, subject string
	if err := db.QueryRow(`SELECT auth_method, issuer, subject FROM auth_sessions WHERE id = 's1'`).
		Scan(&method, &issuer, &subject); err != nil {
		t.Fatalf("read migrated session: %v", err)
	}
	if method != "password" {
		t.Errorf("auth_method = %q, want password", method)
	}
	if issuer != "" || subject != "" {
		t.Errorf("a local session carries a federated identity: (%q,%q)", issuer, subject)
	}
}

// The canonical key is (issuer, sub). Two different subjects at one issuer are
// two identities even when they share an email; the same (issuer, sub) twice
// is one identity, enforced by the index rather than by application code.
func TestExternalIdentityUniquenessIsIssuerAndSubject(t *testing.T) {
	db := migratedDB(t)
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seedUser(t, db, "u1", "ada@example.com")
	seedUser(t, db, "u2", "grace@example.com")

	insert := func(id, user, issuer, subject, email string) error {
		_, err := db.Exec(`
			INSERT INTO external_identities (id, user_id, issuer, subject, email, email_verified, display_name, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 1, '', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
			id, user, issuer, subject, email)
		return err
	}

	if err := insert("i1", "u1", "https://idp.example", "sub-1", "shared@example.com"); err != nil {
		t.Fatalf("first identity: %v", err)
	}
	// Same email, different subject: a different person, and allowed.
	if err := insert("i2", "u2", "https://idp.example", "sub-2", "shared@example.com"); err != nil {
		t.Fatalf("second subject with the same email was rejected: %v", err)
	}
	// Same (issuer, sub) again: refused, whatever the email says.
	if err := insert("i3", "u2", "https://idp.example", "sub-1", "other@example.com"); err == nil {
		t.Error("a duplicate (issuer, subject) was accepted")
	}
	// Same subject at a DIFFERENT issuer is a different identity.
	if err := insert("i4", "u2", "https://other-idp.example", "sub-1", "other@example.com"); err != nil {
		t.Errorf("same subject at another issuer was rejected: %v", err)
	}
}

func TestOIDCLoginFlowClientKindIsAClosedSet(t *testing.T) {
	db := migratedDB(t)
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	insert := func(id, kind string) error {
		_, err := db.Exec(`
			INSERT INTO oidc_login_flows (id, nonce, code_verifier, redirect_uri, client_kind, created_at, expires_at)
			VALUES (?, 'n', 'v', 'http://127.0.0.1/cb', ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, id, kind)
		return err
	}
	for _, kind := range []string{"browser", "desktop"} {
		if err := insert("f-"+kind, kind); err != nil {
			t.Errorf("client_kind %q rejected: %v", kind, err)
		}
	}
	if err := insert("f-bogus", "smuggled"); err == nil {
		t.Error("an unknown client_kind was accepted")
	}
}
