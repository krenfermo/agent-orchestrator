package sqlite

import (
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// migrate_workflow_attempt_error_class_drift_test.go guards the attempt
// error_class CHECK against the Go vocabulary it has to hold.
//
// 0171 exists because the two drifted: seven classes AO writes to
// workflow_attempts were rejected by the CHECK on every real database, and no
// test noticed, because every workflow test drives a fake store with no CHECK
// at all. A per-migration test cannot catch the NEXT drift either -- it only
// knows the classes that existed when it was written. So the drift test below
// reads the vocabulary from the Go source and asks the fully migrated schema
// about every entry.

// seedAttemptParents creates the project/run/step an attempt row needs.
func seedAttemptParents(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, stmt := range []string{
		`INSERT INTO projects (id, path, registered_at) VALUES ('p', '/tmp/p', CURRENT_TIMESTAMP)`,
		`INSERT INTO workflow_runs (id, project_id, objective, state, policy_version, policy_snapshot, created_at, updated_at)
		 VALUES ('wf-1', 'p', 'objective', 'running', 'v1', '{}', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO workflow_steps (id, workflow_run_id, kind, state, ordinal, created_at, updated_at)
		 VALUES ('st-1', 'wf-1', 'work', 'running', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}
}

var driftNewClasses = []string{
	"provider_auth_required", "provider_workspace_trust_required", "provider_preflight_failed",
	"provider_auth_interactive", "deliverable_not_observable", "integration_failed", "superseded",
}

func TestMigration0171PreservesAttemptsAndAcceptsTheDriftedClasses(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "ao.db") + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	upTo(t, db, 170)
	seedAttemptParents(t, db)
	if _, err := db.Exec(`
		INSERT INTO workflow_attempts (
			id, workflow_step_id, attempt_number, harness, model, started_at, finished_at,
			outcome, error_class, retry_after, deadline_at, review_target_fingerprint, review_target_head_sha
		) VALUES (
			'at-1', 'st-1', 1, 'claude-code', 'opus', '2026-01-01 00:00:00', '2026-01-01 00:05:00',
			'failed', 'invalid_placement', '2026-01-01 00:06:00', '2026-01-01 00:30:00', 'fp-abc', 'sha-def'
		)`); err != nil {
		t.Fatalf("seed attempt: %v", err)
	}
	// Before 0171 the drifted classes really are rejected: this is the defect,
	// reproduced, not assumed.
	if _, err := db.Exec(`INSERT INTO workflow_attempts (id, workflow_step_id, attempt_number, started_at, outcome, error_class)
		VALUES ('at-pre', 'st-1', 99, CURRENT_TIMESTAMP, 'failed', 'deliverable_not_observable')`); err == nil {
		t.Fatal("the pre-0171 schema accepted deliverable_not_observable; this test no longer models the defect")
	}

	upTo(t, db, 171)

	var (
		harness, model, outcome, errClass, fingerprint, headSHA string
		finished, retryAfter, deadline                          sql.NullString
	)
	if err := db.QueryRow(`
		SELECT harness, model, finished_at, outcome, error_class, retry_after, deadline_at,
		       review_target_fingerprint, review_target_head_sha
		FROM workflow_attempts WHERE id = 'at-1'`).
		Scan(&harness, &model, &finished, &outcome, &errClass, &retryAfter, &deadline, &fingerprint, &headSHA); err != nil {
		t.Fatalf("read attempt after 0171: %v", err)
	}
	if harness != "claude-code" || model != "opus" || outcome != "failed" || errClass != "invalid_placement" ||
		fingerprint != "fp-abc" || headSHA != "sha-def" || !finished.Valid || !retryAfter.Valid || !deadline.Valid {
		t.Fatalf("attempt mis-mapped by the rebuild: harness=%q model=%q outcome=%q class=%q fp=%q sha=%q finished=%v retry=%v deadline=%v",
			harness, model, outcome, errClass, fingerprint, headSHA, finished, retryAfter, deadline)
	}

	for i, class := range driftNewClasses {
		if _, err := db.Exec(`INSERT INTO workflow_attempts (id, workflow_step_id, attempt_number, started_at, outcome, error_class)
			VALUES (?, 'st-1', ?, CURRENT_TIMESTAMP, 'failed', ?)`, "at-new-"+class, 10+i, class); err != nil {
			t.Fatalf("0171 still rejects %q: %v", class, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO workflow_attempts (id, workflow_step_id, attempt_number, started_at, outcome, error_class)
		VALUES ('at-bogus', 'st-1', 50, CURRENT_TIMESTAMP, 'failed', 'not_a_class')`); err == nil {
		t.Fatal("0171 dropped the CHECK instead of widening it: an unknown class was accepted")
	}

	// Down neutralises exactly the classes 0160's list cannot hold and keeps
	// every row and every other class.
	if err := downTo(t, db, 170); err != nil {
		t.Fatalf("down to 170: %v", err)
	}
	var total, nulled int
	if err := db.QueryRow(`SELECT COUNT(*), SUM(CASE WHEN error_class IS NULL THEN 1 ELSE 0 END) FROM workflow_attempts`).Scan(&total, &nulled); err != nil {
		t.Fatalf("count after down: %v", err)
	}
	if total != 1+len(driftNewClasses) || nulled != len(driftNewClasses) {
		t.Fatalf("after down: %d rows (%d NULL class), want %d rows with exactly the %d new-class rows neutralised",
			total, nulled, 1+len(driftNewClasses), len(driftNewClasses))
	}
	if err := db.QueryRow(`SELECT error_class FROM workflow_attempts WHERE id = 'at-1'`).Scan(&errClass); err != nil || errClass != "invalid_placement" {
		t.Fatalf("down touched a class 0160 can hold: %q (%v)", errClass, err)
	}
}

// goErrorClassRe matches a declared attempt error class in Go source.
var goErrorClassRe = regexp.MustCompile(`WorkflowErrorClass\s*=\s*"([a-z_]+)"`)

// declaredErrorClasses reads every WorkflowErrorClass constant AO declares, in
// the two packages that declare them.
func declaredErrorClasses(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, dir := range []string{"../../domain", "../../workflow"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range goErrorClassRe.FindAllStringSubmatch(string(b), -1) {
				seen[m[1]] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func TestAttemptErrorClassCheckAcceptsEveryDeclaredClass(t *testing.T) {
	classes := declaredErrorClasses(t)
	// A scan that silently found nothing would pass vacuously.
	if len(classes) < 30 {
		t.Fatalf("found only %d declared error classes (%v); the source scan is broken", len(classes), classes)
	}
	db := openMigratedTestDB(t)
	seedAttemptParents(t, db)
	var rejected []string
	for i, class := range classes {
		if _, err := db.Exec(`INSERT INTO workflow_attempts (id, workflow_step_id, attempt_number, started_at, outcome, error_class)
			VALUES (?, 'st-1', ?, CURRENT_TIMESTAMP, 'failed', ?)`, "at-"+class, i+1, class); err != nil {
			rejected = append(rejected, class)
		}
	}
	if len(rejected) > 0 {
		t.Fatalf("the workflow_attempts.error_class CHECK rejects %d class(es) AO declares: %v\n"+
			"A class AO can write must be accepted by the schema, or recording it fails the whole operation on a real database. "+
			"Widen the CHECK with a new migration (0171's park-and-restore recipe).", len(rejected), rejected)
	}
}
