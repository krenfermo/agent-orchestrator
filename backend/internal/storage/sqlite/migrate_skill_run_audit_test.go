package sqlite

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigration0173AuditStateAndParentLiveInTheSchema proves 0173's contract
// is enforced by the database: 'partial' is terminal, carries a report and its
// digest, and always a reason; a run is never its own parent; every 0172 row
// and its findings survive the rebuild; and Down turns a partial back into a
// failed run rather than losing the row.
func TestMigration0173AuditStateAndParentLiveInTheSchema(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "ao.db") + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	mustFail := func(want, q string, args ...any) {
		t.Helper()
		_, err := db.Exec(q, args...)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("want an error containing %q, got %v\n%s", want, err, q)
		}
	}
	const sha = "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"

	// A 0172 database with a succeeded run and a finding.
	upTo(t, db, 172)
	mustExec(`INSERT INTO projects (id, path, registered_at) VALUES ('p', '/tmp/p', CURRENT_TIMESTAMP)`)
	mustExec(`INSERT INTO skill_runs (id, project_id, skill_id, skill_version, mode_id, state, owner_instance,
		created_at, updated_at, started_at, finished_at, report_json, report_sha256)
		VALUES ('old', 'p', 'security-audit', '0.3.0', 'static-code', 'succeeded', 'aod-1',
		CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, '{}', ?)`, sha)
	mustExec(`INSERT INTO skill_run_findings (run_id, ordinal, rule_id, severity) VALUES ('old', 0, 'AOSS-001', 'high')`)

	upTo(t, db, 173)
	var findings int
	_ = db.QueryRow(`SELECT count(*) FROM skill_run_findings WHERE run_id = 'old'`).Scan(&findings)
	if findings != 1 {
		t.Fatalf("the rebuild lost %d finding(s)", 1-findings)
	}
	var parent sql.NullString
	_ = db.QueryRow(`SELECT parent_run_id FROM skill_runs WHERE id = 'old'`).Scan(&parent)
	if parent.Valid {
		t.Fatal("a pre-0173 run acquired a parent")
	}

	insert := `INSERT INTO skill_runs (id, project_id, skill_id, skill_version, mode_id, state, owner_instance,
		created_at, updated_at, parent_run_id) VALUES (?, 'p', 'security-audit', '0.4.0', ?, 'queued', 'aod-1',
		CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, ?)`
	mustExec(insert, "audit", "full-audit", nil)
	mustExec(insert, "child", "static-code", "audit")
	mustFail("CHECK", insert, "self", "secret-scan", "self")

	mustExec(`UPDATE skill_runs SET state='running', started_at=CURRENT_TIMESTAMP WHERE id='audit'`)
	// partial without a report, without a digest, or without a reason.
	mustFail("CHECK", `UPDATE skill_runs SET state='partial', finished_at=CURRENT_TIMESTAMP,
		error_code='SKILL_AUDIT_PARTIAL' WHERE id='audit'`)
	mustFail("CHECK", `UPDATE skill_runs SET state='partial', finished_at=CURRENT_TIMESTAMP,
		report_json='{}', error_code='SKILL_AUDIT_PARTIAL' WHERE id='audit'`)
	mustFail("CHECK", `UPDATE skill_runs SET state='partial', finished_at=CURRENT_TIMESTAMP,
		report_json='{}', report_sha256=? WHERE id='audit'`, sha)
	// partial without finished_at is not terminal.
	mustFail("CHECK", `UPDATE skill_runs SET state='partial', report_json='{}', report_sha256=?,
		error_code='SKILL_AUDIT_PARTIAL' WHERE id='audit'`, sha)
	mustExec(`UPDATE skill_runs SET state='partial', finished_at=CURRENT_TIMESTAMP, report_json='{}',
		report_sha256=?, error_code='SKILL_AUDIT_PARTIAL' WHERE id='audit'`, sha)
	// Once partial (terminal), a new full-audit is allowed: single flight holds
	// only for queued/running.
	mustExec(insert, "audit2", "full-audit", nil)

	// Down: the partial audit survives as failed, keeping its reason.
	if err := downTo(t, db, 172); err != nil {
		t.Fatalf("down to 172: %v", err)
	}
	var state, code string
	var report sql.NullString
	if err := db.QueryRow(`SELECT state, error_code, report_json FROM skill_runs WHERE id='audit'`).Scan(&state, &code, &report); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || code != "SKILL_AUDIT_PARTIAL" || report.Valid {
		t.Fatalf("down turned partial into %s/%s report=%v", state, code, report.Valid)
	}
	_ = db.QueryRow(`SELECT count(*) FROM skill_run_findings WHERE run_id = 'old'`).Scan(&findings)
	if findings != 1 {
		t.Fatal("down lost findings")
	}
}
