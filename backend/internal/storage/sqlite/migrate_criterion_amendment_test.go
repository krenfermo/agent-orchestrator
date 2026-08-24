package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigration0132CriterionAmendmentsEnforceTheirGuarantees.
//
// The amendment ledger's value is entirely in what it refuses to store. A row
// without an approving human, without a reason, without evidence, or whose
// disposition contradicts its text would each turn "auditable amendment" into
// "someone quietly changed the rules". The schema is where those are made
// unrepresentable rather than merely discouraged, so this checks the
// constraints rather than the happy path.
func TestMigration0132CriterionAmendmentsEnforceTheirGuarantees(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "ao.db") + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	upTo(t, db, 132)

	if _, err := db.Exec(`INSERT INTO projects (id, path, registered_at) VALUES ('p','/tmp/p',CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO workflow_runs (id, project_id, objective, state, policy_version, policy_snapshot, created_at, updated_at)
		VALUES ('wf','p','o','running','v1','{}',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO workflow_tasks (id, workflow_run_id, plan_step_id, ordinal, title, description,
			acceptance_criteria_json, verify_json, state, created_at, updated_at)
		VALUES ('t','wf','s',1,'title','desc','[]','{}','running',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	ins := func(id, disposition, amended, reason, evidence, approvedBy string) error {
		_, err := db.Exec(`
			INSERT INTO workflow_task_criterion_amendments
				(id, workflow_run_id, task_id, criterion_index, original_criterion,
				 amended_criterion, disposition, reason, evidence_json, approved_by, created_at)
			VALUES (?, 'wf', 't', 0, 'the original', ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
			id, amended, disposition, reason, evidence, approvedBy)
		return err
	}

	// The one that must work.
	if err := ins("a1", "declared_obsolete", "", "committed in 70296042b", `["70296042b"]`, "joaquin"); err != nil {
		t.Fatalf("a well-formed amendment was refused: %v", err)
	}

	for _, tc := range []struct {
		name, id, disposition, amended, reason, evidence, approvedBy string
	}{
		{"no approver", "a2", "declared_obsolete", "", "r", `["e"]`, ""},
		{"no reason", "a3", "declared_obsolete", "", "", `["e"]`, "joaquin"},
		{"empty evidence array", "a4", "declared_obsolete", "", "r", `[]`, "joaquin"},
		{"evidence not json", "a5", "declared_obsolete", "", "r", `nope`, "joaquin"},
		{"obsolete with replacement text", "a6", "declared_obsolete", "new text", "r", `["e"]`, "joaquin"},
		{"amended without replacement text", "a7", "amended", "", "r", `["e"]`, "joaquin"},
		{"unknown disposition", "a8", "rewritten", "x", "r", `["e"]`, "joaquin"},
	} {
		if err := ins(tc.id, tc.disposition, tc.amended, tc.reason, tc.evidence, tc.approvedBy); err == nil {
			t.Errorf("%s: accepted, want refused by the schema", tc.name)
		}
	}

	// The original text is not merely stored, it is required: an amendment that
	// cannot say what it replaced is not auditable.
	if _, err := db.Exec(`
		INSERT INTO workflow_task_criterion_amendments
			(id, workflow_run_id, task_id, criterion_index, original_criterion,
			 amended_criterion, disposition, reason, evidence_json, approved_by, created_at)
		VALUES ('a9','wf','t',0,'','', 'declared_obsolete','r','["e"]','joaquin',CURRENT_TIMESTAMP)`); err == nil {
		t.Error("an amendment with no original text was accepted")
	}

	// It belongs to a real task, and goes away with it.
	if _, err := db.Exec(`
		INSERT INTO workflow_task_criterion_amendments
			(id, workflow_run_id, task_id, criterion_index, original_criterion,
			 amended_criterion, disposition, reason, evidence_json, approved_by, created_at)
		VALUES ('a10','wf','nope',0,'x','', 'declared_obsolete','r','["e"]','joaquin',CURRENT_TIMESTAMP)`); err == nil {
		t.Error("an amendment pointing at no task was accepted")
	}
	if _, err := db.Exec(`DELETE FROM workflow_tasks WHERE id = 't'`); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	var left int
	if err := db.QueryRow(`SELECT count(*) FROM workflow_task_criterion_amendments`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("amendments left after the task was deleted = %d, want 0 (ON DELETE CASCADE)", left)
	}
}
