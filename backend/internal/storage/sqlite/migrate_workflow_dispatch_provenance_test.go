package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	sqlitestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// seedPre0133Run builds a database at exactly the schema 0132 left behind,
// holding one workflow run whose verify attempt was written before dispatch
// checkpoints, mutation provenance, deadlines and review targets existed. It
// returns the open handle, still at version 132.
func seedPre0133Run(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "ao.db")+pragmas)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	upTo(t, db, 132)

	if _, err := db.Exec(
		`INSERT INTO projects (id, path, registered_at) VALUES ('p', '/tmp/p', CURRENT_TIMESTAMP)`,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO workflow_runs (id, project_id, objective, state, policy_version, policy_snapshot,
			created_at, updated_at)
		VALUES ('wf-legacy', 'p', 'objective', 'running', 'v1', '{}', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
	); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO workflow_steps (id, workflow_run_id, kind, ordinal, state, created_at, updated_at)
		VALUES ('wfs-verify', 'wf-legacy', 'verify', 1, 'running', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
	); err != nil {
		t.Fatalf("seed step: %v", err)
	}
	// The pre-0133 attempt shape: exactly the ten columns 0102 left, written
	// positionally the way a build from before this migration would have.
	if _, err := db.Exec(`
		INSERT INTO workflow_attempts (id, workflow_step_id, attempt_number, harness, model,
			started_at, finished_at, outcome, error_class, retry_after)
		VALUES ('wfa-legacy', 'wfs-verify', 1, 'claude-code', 'opus', '2026-08-01T00:00:00Z',
			NULL, NULL, NULL, NULL)`,
	); err != nil {
		t.Fatalf("seed legacy attempt: %v", err)
	}
	return db
}

// TestMigration0133LeavesPre0133RowsReadableWithoutProvenance is the whole
// point of making every column of 0133 nullable or defaulted.
//
// A database that has been running since before dispatch checkpoints, mutation
// provenance, verify deadlines and review targets existed holds rows that
// answer none of those questions, and it must keep working: the attempt reads
// back with a nil deadline and an empty review target, and the run reads back
// with zero dispatch-checkpoint and zero provenance rows. What it must NOT do
// is error, and it must not invent a deadline or a provenance record either --
// a mutation nobody observed has no honest provenance, and substituting one
// would defeat the reason the table exists.
func TestMigration0133LeavesPre0133RowsReadableWithoutProvenance(t *testing.T) {
	db := seedPre0133Run(t)
	if err := migrate(db); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}

	ctx := t.Context()
	store := sqlitestore.NewStore(db, db)

	attempts, err := store.ListWorkflowAttempts(ctx, "wfs-verify")
	if err != nil {
		t.Fatalf("list attempts on legacy row: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(attempts))
	}
	got := attempts[0]
	if got.ID != "wfa-legacy" || got.Harness != "claude-code" {
		t.Fatalf("legacy attempt lost its own facts: %+v", got)
	}
	if got.DeadlineAt != nil {
		t.Errorf("DeadlineAt = %v, want nil for a pre-0133 attempt", *got.DeadlineAt)
	}
	if !got.ReviewTarget.Empty() {
		t.Errorf("ReviewTarget = %+v, want empty for a pre-0133 attempt", got.ReviewTarget)
	}
	if window := got.VerifyWindow(); window.OverdueAt(time.Now().UTC().Add(1000 * time.Hour)) {
		t.Error("an attempt with no recorded deadline reported itself overdue")
	}

	latest, ok, err := store.GetLatestWorkflowAttempt(ctx, "wfs-verify")
	if err != nil {
		t.Fatalf("get latest attempt on legacy row: %v", err)
	}
	if !ok || latest.ID != "wfa-legacy" {
		t.Fatalf("latest attempt = %+v ok=%v, want the legacy attempt", latest, ok)
	}
	if latest.DeadlineAt != nil || !latest.ReviewTarget.Empty() {
		t.Errorf("latest attempt carries provenance it was never given: %+v", latest)
	}

	dispatches, err := store.ListWorkflowDispatchCheckpointsByRun(ctx, "wf-legacy")
	if err != nil {
		t.Fatalf("list dispatch checkpoints on legacy run: %v", err)
	}
	if len(dispatches) != 0 {
		t.Errorf("dispatch checkpoints = %d, want 0 for a pre-0133 run", len(dispatches))
	}
	provenance, err := store.ListWorkflowMutationProvenanceByRun(ctx, "wf-legacy")
	if err != nil {
		t.Fatalf("list mutation provenance on legacy run: %v", err)
	}
	if len(provenance) != 0 {
		t.Errorf("mutation provenance = %d, want 0 for a pre-0133 run", len(provenance))
	}

	// "Nothing was ever recorded" must be reported as a miss, never as an
	// attribution: a pre-0133 branch nobody observed is not an unchanged one.
	if _, found, err := store.GetLatestWorkflowMutationProvenanceByBranch(ctx, "wf-legacy", "ao/legacy"); err != nil {
		t.Fatalf("latest provenance by branch on legacy run: %v", err)
	} else if found {
		t.Error("a pre-0133 run reported mutation provenance it never had")
	}
	if _, found, err := store.GetLatestWorkflowDispatchCheckpointByStep(ctx, "wfs-verify"); err != nil {
		t.Fatalf("latest dispatch checkpoint on legacy step: %v", err)
	} else if found {
		t.Error("a pre-0133 step reported a dispatch checkpoint it never had")
	}
}

// TestMigration0133RecordsProvenanceOnUpgradedLegacyRows checks the other half:
// the same upgraded database can carry the new facts for the same legacy run,
// so the migration is an upgrade rather than a fork in the data.
func TestMigration0133RecordsProvenanceOnUpgradedLegacyRows(t *testing.T) {
	db := seedPre0133Run(t)
	if err := migrate(db); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}

	ctx := t.Context()
	store := sqlitestore.NewStore(db, db)
	stepID, attemptID := "wfs-verify", "wfa-legacy"
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	dispatch, err := store.CreateWorkflowDispatchCheckpoint(ctx, domain.WorkflowDispatchCheckpoint{
		ID:             "wfd-1",
		WorkflowRunID:  "wf-legacy",
		WorkflowStepID: &stepID,
		AttemptID:      &attemptID,
		Phase:          domain.DispatchPhaseWorkerDispatched,
		IdempotencyKey: "wf-legacy:wfs-verify:1",
		Harness:        "claude-code",
		LaunchStage:    domain.LaunchStageSpawn,
		LaunchOutcome:  domain.LaunchOutcomeDispatched,
		Detail:         "tmux session created",
		CreatedAt:      now,
	})
	if err != nil {
		t.Fatalf("create dispatch checkpoint: %v", err)
	}
	if dispatch.EvidenceJSON != "{}" {
		t.Errorf("EvidenceJSON = %q, want the {} default", dispatch.EvidenceJSON)
	}
	if !dispatch.LaunchOutcome.Proven() {
		t.Errorf("LaunchOutcome %q did not read back as proven", dispatch.LaunchOutcome)
	}

	observed := now.Add(-2 * time.Minute)
	if _, err := store.RecordWorkflowMutationProvenance(ctx, domain.WorkflowMutationProvenance{
		ID:                "wfm-1",
		WorkflowRunID:     "wf-legacy",
		WorkflowStepID:    &stepID,
		AttemptID:         &attemptID,
		Class:             domain.MutationAuthorizedFix,
		Harness:           "claude-code",
		Branch:            "ao/legacy",
		WorktreePath:      "/tmp/wt",
		BaseSHA:           "base0",
		HeadSHA:           "head1",
		FingerprintBefore: "fp-before",
		FingerprintAfter:  "fp-after",
		Reason:            "reviewer-requested fix cycle 2",
		ObservedAt:        &observed,
		CreatedAt:         now,
	}); err != nil {
		t.Fatalf("record mutation provenance: %v", err)
	}

	who, found, err := store.GetLatestWorkflowMutationProvenanceByBranch(ctx, "wf-legacy", "ao/legacy")
	if err != nil {
		t.Fatalf("latest provenance by branch: %v", err)
	}
	if !found {
		t.Fatal("recorded provenance did not read back")
	}
	if !who.Class.Authorized() || who.HeadSHA != "head1" || who.FingerprintAfter != "fp-after" {
		t.Fatalf("provenance read back wrong: %+v", who)
	}
	if who.ObservedAt == nil || !who.ObservedAt.Equal(observed) {
		t.Errorf("ObservedAt = %v, want %v", who.ObservedAt, observed)
	}

	// An unattributed mutation is UNKNOWN, never a blank that later reads as
	// "no opinion was ever formed".
	blank, err := store.RecordWorkflowMutationProvenance(ctx, domain.WorkflowMutationProvenance{
		ID: "wfm-2", WorkflowRunID: "wf-legacy", Branch: "ao/other", CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("record unclassified provenance: %v", err)
	}
	if blank.Class != domain.MutationUnknown {
		t.Errorf("Class = %q, want UNKNOWN for an unclassified mutation", blank.Class)
	}
	if blank.ObservedAt != nil {
		t.Errorf("ObservedAt = %v, want nil when the writer could not say", *blank.ObservedAt)
	}

	deadline := now.Add(30 * time.Minute)
	if ok, err := store.SetWorkflowAttemptDeadline(ctx, attemptID, &deadline); err != nil || !ok {
		t.Fatalf("set attempt deadline: ok=%v err=%v", ok, err)
	}
	reviewTarget := domain.WorkflowReviewTarget{Fingerprint: "fp-reviewed", HeadSHA: "head1"}
	if ok, err := store.SetWorkflowAttemptReviewTarget(ctx, attemptID, reviewTarget); err != nil || !ok {
		t.Fatalf("pin review target: ok=%v err=%v", ok, err)
	}
	// A re-review must not be able to retarget a verification already in
	// flight; that needs a new attempt row, not an edit of this one.
	if ok, err := store.SetWorkflowAttemptReviewTarget(ctx, attemptID,
		domain.WorkflowReviewTarget{Fingerprint: "fp-someone-else", HeadSHA: "head2"}); err != nil {
		t.Fatalf("retarget review target: %v", err)
	} else if ok {
		t.Error("a second review target overwrote a pinned one")
	}

	upgraded, ok, err := store.GetLatestWorkflowAttempt(ctx, stepID)
	if err != nil || !ok {
		t.Fatalf("get upgraded attempt: ok=%v err=%v", ok, err)
	}
	if upgraded.DeadlineAt == nil || !upgraded.DeadlineAt.Equal(deadline) {
		t.Fatalf("DeadlineAt = %v, want %v", upgraded.DeadlineAt, deadline)
	}
	if upgraded.ReviewTarget != reviewTarget {
		t.Fatalf("ReviewTarget = %+v, want %+v", upgraded.ReviewTarget, reviewTarget)
	}
	if window := upgraded.VerifyWindow(); !window.OverdueAt(deadline.Add(time.Second)) {
		t.Error("an unfinished attempt past its deadline did not report overdue")
	}

	byStep, err := store.ListWorkflowDispatchCheckpointsByStep(ctx, stepID)
	if err != nil {
		t.Fatalf("list dispatch checkpoints by step: %v", err)
	}
	if len(byStep) != 1 || byStep[0].ID != "wfd-1" {
		t.Fatalf("dispatch checkpoints by step = %+v, want just wfd-1", byStep)
	}
	byStepProvenance, err := store.ListWorkflowMutationProvenanceByStep(ctx, stepID)
	if err != nil {
		t.Fatalf("list provenance by step: %v", err)
	}
	if len(byStepProvenance) != 1 || byStepProvenance[0].ID != "wfm-1" {
		t.Fatalf("provenance by step = %+v, want just wfm-1", byStepProvenance)
	}
}
