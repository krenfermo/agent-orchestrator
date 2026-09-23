package sqlite

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigration0172SkillRunInvariantsLiveInTheSchema proves the SkillRun
// contract is enforced by the database itself, not only by the one service
// that usually writes it: a second in-flight run, a terminal run without a
// finish time, a success without a report or digest, and a failure without a
// reason are all impossible to write.
func TestMigration0172SkillRunInvariantsLiveInTheSchema(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "ao.db") + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	upTo(t, db, 172)
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
	mustExec(`INSERT INTO projects (id, path, registered_at) VALUES ('p', '/tmp/p', CURRENT_TIMESTAMP)`)
	insertRun := `INSERT INTO skill_runs (id, project_id, skill_id, skill_version, mode_id, state,
		owner_instance, created_at, updated_at) VALUES (?, 'p', 'security-audit', '0.1.0', 'static-code', ?,
		'aod-1', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`

	mustExec(insertRun, "r1", "queued")
	// Single flight: a second queued/running run for the same (project, skill, mode).
	mustFail("UNIQUE", insertRun, "r2", "queued")
	// A terminal state without finished_at is not a terminal row.
	mustFail("CHECK", insertRun, "r3", "failed")
	// running -> succeeded without a report or digest.
	mustExec(`UPDATE skill_runs SET state='running', started_at=CURRENT_TIMESTAMP WHERE id='r1'`)
	mustFail("CHECK", `UPDATE skill_runs SET state='succeeded', finished_at=CURRENT_TIMESTAMP WHERE id='r1'`)
	mustFail("CHECK", `UPDATE skill_runs SET state='succeeded', finished_at=CURRENT_TIMESTAMP,
		report_json='{}' WHERE id='r1'`)
	// failed without a reason.
	mustFail("CHECK", `UPDATE skill_runs SET state='failed', finished_at=CURRENT_TIMESTAMP WHERE id='r1'`)
	// A well-formed success is accepted, with findings.
	mustExec(`UPDATE skill_runs SET state='succeeded', finished_at=CURRENT_TIMESTAMP,
		report_json='{}', report_sha256='44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a' WHERE id='r1'`)
	mustExec(`INSERT INTO skill_run_findings (run_id, ordinal, rule_id, severity) VALUES ('r1', 0, 'AOSS-001', 'high')`)
	// Once terminal, a new run for the same (project, skill, mode) is allowed.
	mustExec(insertRun, "r4", "queued")
	// The same idempotency key on the same project is one run.
	mustExec(`UPDATE skill_runs SET idempotency_key='k1' WHERE id='r1'`)
	mustFail("UNIQUE", `UPDATE skill_runs SET idempotency_key='k1' WHERE id='r4'`)
	// Deleting the project removes its runs and their findings.
	mustExec(`DELETE FROM skill_runs WHERE id='r4'`)
	mustExec(`DELETE FROM projects WHERE id='p'`)
	var runs, findings int
	_ = db.QueryRow(`SELECT count(*) FROM skill_runs`).Scan(&runs)
	_ = db.QueryRow(`SELECT count(*) FROM skill_run_findings`).Scan(&findings)
	if runs != 0 || findings != 0 {
		t.Fatalf("project delete left %d run(s) and %d finding(s)", runs, findings)
	}

	// Down removes both tables.
	if err := downTo(t, db, 171); err != nil {
		t.Fatalf("down to 171: %v", err)
	}
	var n int
	_ = db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name IN ('skill_runs','skill_run_findings')`).Scan(&n)
	if n != 0 {
		t.Fatalf("down left %d table(s)", n)
	}
}
