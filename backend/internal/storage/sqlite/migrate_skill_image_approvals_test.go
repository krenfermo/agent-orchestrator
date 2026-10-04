package sqlite

import (
	"database/sql"
	"testing"
)

// migrate_skill_image_approvals_test.go -- the durable half of the image trust
// root (migration 0163).
//
// The risk in this migration is not the new table. It is the REBUILD of
// skill_audit: SQLite cannot alter a CHECK in place, so the audit trail is
// copied into a new table and the old one dropped. An installation that already
// holds somebody's install and enable history must come through with every row
// intact, and the widened vocabulary must actually be accepted afterwards.

// seedPreImageTrustAudit writes the audit history a real installation would
// already hold at 0162, plus the project and install rows it hangs off.
func seedPreImageTrustAudit(t *testing.T, db *sql.DB) {
	t.Helper()
	seedProjectRow(t, db, "proj-audit")
	if _, err := db.Exec(`
		INSERT INTO skill_installs (
			skill_id, version, name, description, risk_level, origin_type, origin_ref,
			digest, manifest_json, package_dir, installed_at, installed_by
		) VALUES ('security-audit', '1.2.0', 'Security Audit', '', 'high', 'local', '',
			'sha256:aaaa', '{}', '/pkgs/sa', CURRENT_TIMESTAMP, 'ada')`,
	); err != nil {
		t.Fatalf("seed install: %v", err)
	}
	rows := []struct {
		id, action, project string
	}{
		{"aud-1", "install", ""},
		{"aud-2", "enable", "proj-audit"},
		{"aud-3", "grant_changed", "proj-audit"},
		{"aud-4", "install_rejected", ""},
		{"aud-5", "disable", "proj-audit"},
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
			) VALUES (?, CURRENT_TIMESTAMP, 'ada', ?, 'security-audit', '1.2.0', ?,
				'sha256:aaaa', '["repo.read"]', 'seeded')`,
			r.id, r.action, project,
		); err != nil {
			t.Fatalf("seed audit %s: %v", r.id, err)
		}
	}
}

// The rebuild must not lose history. An audit trail that silently shrinks is
// worse than one that never existed: somebody reading it would conclude the
// events did not happen.
func TestImageTrustMigrationPreservesTheAuditTrail(t *testing.T) {
	db := migratedDB(t)
	upTo(t, db, 162)
	seedPreImageTrustAudit(t, db)

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
		t.Fatalf("the rebuild left %d of %d audit rows", after, before)
	}
	// Every column travels, not just the id. A copy that dropped project_id or
	// capabilities would leave a trail that no longer says who or what.
	var actor, action, digest, capabilities, detail string
	var projectID sql.NullString
	if err := db.QueryRow(`
		SELECT actor, action, project_id, digest, capabilities, detail
		FROM skill_audit WHERE id = 'aud-2'`,
	).Scan(&actor, &action, &projectID, &digest, &capabilities, &detail); err != nil {
		t.Fatalf("read aud-2: %v", err)
	}
	if actor != "ada" || action != "enable" || projectID.String != "proj-audit" ||
		digest != "sha256:aaaa" || capabilities != `["repo.read"]` || detail != "seeded" {
		t.Fatalf("aud-2 came through as %q/%q/%q/%q/%q/%q",
			actor, action, projectID.String, digest, capabilities, detail)
	}
	// The indexes have to come back, or every audit read turns into a scan.
	for _, idx := range []string{"idx_skill_audit_occurred_at", "idx_skill_audit_skill"} {
		var name string
		if err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, idx,
		).Scan(&name); err != nil {
			t.Fatalf("index %s is gone after the rebuild: %v", idx, err)
		}
	}
}

// The point of the rebuild: the new actions are accepted, and the old ones
// still are. A widened CHECK that dropped an existing value would break the
// install path on the next boot.
func TestImageTrustMigrationWidensTheAuditVocabulary(t *testing.T) {
	db := migratedDB(t)
	upTo(t, db, 162)
	seedPreImageTrustAudit(t, db)
	if err := migrate(db); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}

	for _, action := range []string{
		"install", "uninstall", "enable", "disable", "grant_changed", "install_rejected",
		"image_approved", "image_revoked", "run_executed", "run_refused",
	} {
		if _, err := db.Exec(`
			INSERT INTO skill_audit (id, occurred_at, actor, action, skill_id)
			VALUES (?, CURRENT_TIMESTAMP, 'ada', ?, 'security-audit')`,
			"new-"+action, action,
		); err != nil {
			t.Fatalf("action %q was refused after the widening: %v", action, err)
		}
	}
	// And the CHECK still refuses an action nobody defined. A rebuild that
	// dropped the constraint would make the column free text.
	if _, err := db.Exec(`
		INSERT INTO skill_audit (id, occurred_at, actor, action, skill_id)
		VALUES ('bogus', CURRENT_TIMESTAMP, 'ada', 'exfiltrate', 'security-audit')`,
	); err == nil {
		t.Fatal("the audit action CHECK was dropped by the rebuild")
	}
}

// The approvals table enforces the two things the Go validator also enforces,
// as a backstop: one approval per (scope, tool), and a digest-shaped digest.
func TestImageTrustMigrationConstrainsApprovals(t *testing.T) {
	db := migratedDB(t)
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seedProjectRow(t, db, "proj-1")

	insert := func(id, digest string) error {
		_, err := db.Exec(`
			INSERT INTO skill_image_approvals (
				id, tenant_id, project_id, skill_id, version, mode_id, tool,
				reference, digest, approved_by, approved_at, note
			) VALUES (?, 'tenant-1', 'proj-1', 'security-audit', '1.2.0', 'static-code',
				'ao.static-scan/v1', 'alpine', ?, 'ada', CURRENT_TIMESTAMP, 'checked')`,
			id, digest,
		)
		return err
	}
	good := "sha256:" + repeatHex(64)
	if err := insert("app-1", good); err != nil {
		t.Fatalf("insert a valid approval: %v", err)
	}
	// A second approval for the same scope and tool is refused: "which image
	// may this scope run" must have one answer, not an accumulating list.
	if err := insert("app-2", "sha256:"+repeatHex(64)); err == nil {
		t.Fatal("a second approval for the same scope and tool was accepted")
	}
	// A tag is not a digest. The CHECK is a backstop under the Go validator,
	// and it must not be the thing that lets one through.
	for _, bad := range []string{"latest", "alpine:3.19", "sha256:short", ""} {
		if err := insert("app-bad", bad); err == nil {
			t.Fatalf("digest %q was accepted", bad)
		}
	}
}

func repeatHex(n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = "0123456789abcdef"[i%16]
	}
	return string(out)
}
