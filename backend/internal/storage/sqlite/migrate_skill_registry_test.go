package sqlite

import (
	"database/sql"
	"testing"
)

// migrate_skill_registry_test.go -- the registry / marketplace foundation
// (migration 0164).
//
// Three things carry risk here and each has a test below:
//
//  1. skill_audit is REBUILT again, for the second time, to widen its CHECK.
//     An installation that already holds an install/enable/image history must
//     come through with every row intact.
//  2. skill_install_origins hangs off skill_installs by a composite foreign
//     key. If that reference is wrong, an install either cannot record its
//     provenance or can record provenance for a package that does not exist.
//  3. A registry's tenant is NULLABLE, and NULL has to mean
//     "installation-wide" rather than "the default organization". Those are
//     different facts, and conflating them makes every registry either private
//     to one tenant or visible to all with no way to choose.

// seedPreRegistryAudit writes the history a real installation already holds at
// 0163 -- including the phase 8 image actions, which the 0163 rebuild added and
// this one must not drop.
func seedPreRegistryAudit(t *testing.T, db *sql.DB) {
	t.Helper()
	seedProjectRow(t, db, "proj-registry")
	if _, err := db.Exec(`
		INSERT INTO skill_installs (
			skill_id, version, name, description, risk_level, origin_type, origin_ref,
			digest, manifest_json, package_dir, installed_at, installed_by
		) VALUES ('security-audit', '0.1.0', 'Security Audit', '', 'high', 'local', '',
			'` + repeatHex(64) + `', '{}', '/pkgs/sa', CURRENT_TIMESTAMP, 'ada')`,
	); err != nil {
		t.Fatalf("seed install: %v", err)
	}
	rows := []struct{ id, action, project string }{
		{"aud-1", "install", ""},
		{"aud-2", "enable", "proj-registry"},
		{"aud-3", "install_rejected", ""},
		{"aud-4", "image_approved", "proj-registry"},
		{"aud-5", "run_refused", "proj-registry"},
	}
	for _, r := range rows {
		var project any
		if r.project != "" {
			project = r.project
		}
		if _, err := db.Exec(`
			INSERT INTO skill_audit (
				id, occurred_at, actor, action, skill_id, version, project_id,
				digest, capabilities, detail
			) VALUES (?, CURRENT_TIMESTAMP, 'ada', ?, 'security-audit', '0.1.0', ?,
				'`+repeatHex(64)+`', '["repo.read"]', 'seeded')`,
			r.id, r.action, project,
		); err != nil {
			t.Fatalf("seed audit %s: %v", r.id, err)
		}
	}
}

func TestRegistryMigrationPreservesTheAuditTrail(t *testing.T) {
	db := migratedDB(t)
	upTo(t, db, 163)
	seedPreRegistryAudit(t, db)

	var before int
	if err := db.QueryRow(`SELECT count(*) FROM skill_audit`).Scan(&before); err != nil {
		t.Fatalf("count before: %v", err)
	}
	if before != 5 {
		t.Fatalf("seeded %d audit rows, want 5", before)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	var after int
	if err := db.QueryRow(`SELECT count(*) FROM skill_audit`).Scan(&after); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != before {
		t.Fatalf("the second rebuild left %d of %d audit rows", after, before)
	}
	// The phase 8 action added by the PREVIOUS rebuild has to survive this
	// one. A copy that dropped it would erase the image trail specifically.
	var action, detail string
	var projectID sql.NullString
	if err := db.QueryRow(
		`SELECT action, project_id, detail FROM skill_audit WHERE id = 'aud-4'`,
	).Scan(&action, &projectID, &detail); err != nil {
		t.Fatalf("read aud-4: %v", err)
	}
	if action != "image_approved" || projectID.String != "proj-registry" || detail != "seeded" {
		t.Fatalf("aud-4 came through as %q/%q/%q", action, projectID.String, detail)
	}
	for _, idx := range []string{"idx_skill_audit_occurred_at", "idx_skill_audit_skill"} {
		var name string
		if err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, idx,
		).Scan(&name); err != nil {
			t.Fatalf("index %s is gone after the rebuild: %v", idx, err)
		}
	}
}

func TestRegistryMigrationWidensTheAuditVocabulary(t *testing.T) {
	db := migratedDB(t)
	upTo(t, db, 163)
	seedPreRegistryAudit(t, db)
	if err := migrate(db); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}

	for _, action := range []string{
		// Everything that already existed.
		"install", "uninstall", "enable", "disable", "grant_changed", "install_rejected",
		"image_approved", "image_revoked", "run_executed", "run_refused",
		// Phase 10.
		"registry_added", "registry_updated", "registry_removed",
		"install_refused", "update_available", "update_installed", "release_revoked_seen",
	} {
		if _, err := db.Exec(`
			INSERT INTO skill_audit (id, occurred_at, actor, action, skill_id)
			VALUES (?, CURRENT_TIMESTAMP, 'ada', ?, 'security-audit')`,
			"new-"+action, action,
		); err != nil {
			t.Fatalf("action %q was refused after the widening: %v", action, err)
		}
	}
	// Not added, deliberately: a search query is text a person typed and can
	// name an internal package or a vulnerability they are hunting. The CHECK
	// is what makes that decision enforceable rather than a convention.
	for _, action := range []string{"skill_searched", "skill_release_viewed", "exfiltrate"} {
		if _, err := db.Exec(`
			INSERT INTO skill_audit (id, occurred_at, actor, action, skill_id)
			VALUES (?, CURRENT_TIMESTAMP, 'ada', ?, 'security-audit')`,
			"bogus-"+action, action,
		); err == nil {
			t.Fatalf("action %q was accepted; the CHECK is not constraining the column", action)
		}
	}
}

// An origin row describes an install. It must be impossible to record
// provenance for a package that is not installed, and uninstalling must take
// the provenance of THAT install with it.
func TestInstallOriginsHangOffTheInstall(t *testing.T) {
	db := migratedDB(t)
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("enable foreign keys: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO skill_installs (
			skill_id, version, name, description, risk_level, origin_type, origin_ref,
			digest, manifest_json, package_dir, installed_at, installed_by
		) VALUES ('security-audit', '0.1.0', 'Security Audit', '', 'high', 'local', '',
			'` + repeatHex(64) + `', '{}', '/pkgs/sa', CURRENT_TIMESTAMP, 'ada')`,
	); err != nil {
		t.Fatalf("seed install: %v", err)
	}

	insertOrigin := func(skill, version string) error {
		_, err := db.Exec(`
			INSERT INTO skill_install_origins (
				skill_id, version, registry_id, registry_type, publisher,
				manifest_digest, artifact_digest, trust_state, trust_policy,
				compatibility_verdict, installed_at, installed_by
			) VALUES (?, ?, 'company-private', 'local', 'acme',
				?, ?, 'verified', 'digest', 'compatible', CURRENT_TIMESTAMP, 'ada')`,
			skill, version, repeatHex(64), repeatHex(64))
		return err
	}
	if err := insertOrigin("security-audit", "0.1.0"); err != nil {
		t.Fatalf("insert origin for an installed version: %v", err)
	}
	if err := insertOrigin("security-audit", "9.9.9"); err == nil {
		t.Fatal("recorded provenance for a version that is not installed")
	}
	// The trust vocabulary is constrained, so 'looks-fine' cannot become a
	// state somebody reads as reassuring.
	if _, err := db.Exec(`
		UPDATE skill_install_origins SET trust_state = 'looks-fine'
		WHERE skill_id = 'security-audit'`,
	); err == nil {
		t.Fatal("the trust_state CHECK accepted a value nobody defined")
	}
	// And 'unknown' compatibility is a legal, recordable outcome: the check
	// not running must be storable, or it would have to be written as a pass.
	if _, err := db.Exec(`
		UPDATE skill_install_origins SET compatibility_verdict = 'unknown'
		WHERE skill_id = 'security-audit'`,
	); err != nil {
		t.Fatalf("an unknown compatibility verdict must be recordable: %v", err)
	}

	if _, err := db.Exec(
		`DELETE FROM skill_installs WHERE skill_id = 'security-audit' AND version = '0.1.0'`,
	); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	var remaining int
	if err := db.QueryRow(`SELECT count(*) FROM skill_install_origins`).Scan(&remaining); err != nil {
		t.Fatalf("count origins: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("%d origin rows outlived their install", remaining)
	}
}

// NULL is "installation-wide", and it has to be storable as such.
func TestRegistryTenantIsNullableAndConstrained(t *testing.T) {
	db := migratedDB(t)
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("enable foreign keys: %v", err)
	}
	insert := func(id, typ, policy string, tenant any) error {
		_, err := db.Exec(`
			INSERT INTO skill_registries (
				id, display_name, type, location, enabled, trust_policy,
				priority, tenant_id, created_at, updated_at
			) VALUES (?, 'Registry', ?, '/srv/registry', 1, ?, 100, ?,
				CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
			id, typ, policy, tenant)
		return err
	}
	if err := insert("installation-wide", "local", "digest", nil); err != nil {
		t.Fatalf("an installation-wide registry must be storable: %v", err)
	}
	var tenantID sql.NullString
	if err := db.QueryRow(
		`SELECT tenant_id FROM skill_registries WHERE id = 'installation-wide'`,
	).Scan(&tenantID); err != nil {
		t.Fatalf("read tenant: %v", err)
	}
	if tenantID.Valid {
		t.Fatalf("an installation-wide registry acquired tenant %q", tenantID.String)
	}
	// The default tenant exists from 0156, so a tenant-scoped registry has
	// something real to point at -- and a made-up tenant does not.
	if err := insert("acme-private", "local", "digest", "tnt_default"); err != nil {
		t.Fatalf("a tenant-scoped registry must be storable: %v", err)
	}
	if err := insert("ghost", "local", "digest", "tnt_nonexistent"); err == nil {
		t.Fatal("a registry pointing at a tenant that does not exist was accepted")
	}
	// The two vocabularies the Go validator also enforces, as backstops.
	if err := insert("bad-type", "ftp", "digest", nil); err == nil {
		t.Fatal("registry type 'ftp' was accepted")
	}
	if err := insert("bad-policy", "local", "vibes", nil); err == nil {
		t.Fatal("trust policy 'vibes' was accepted")
	}
	// https and git are DECLARED in the CHECK on purpose: the column accepts
	// them so a future implementation needs no migration, and the Go validator
	// is what refuses to configure one today.
	if err := insert("future-https", "https", "digest", nil); err != nil {
		t.Fatalf("the column must accept a declared-but-unimplemented type: %v", err)
	}
}
