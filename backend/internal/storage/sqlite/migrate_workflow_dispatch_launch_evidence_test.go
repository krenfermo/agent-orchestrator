package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	sqlitestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// seedPre0134DispatchCheckpoint builds a database at exactly the schema 0133
// left behind, holding one dispatch checkpoint written the way a build from
// before 0134 would have written it: the fifteen columns 0133 created, and no
// launch evidence beyond them. It returns the open handle, still at version 133.
func seedPre0134DispatchCheckpoint(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "ao.db")+pragmas)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	upTo(t, db, 133)

	if _, err := db.Exec(
		`INSERT INTO projects (id, path, registered_at) VALUES ('p', '/tmp/p', CURRENT_TIMESTAMP)`,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO workflow_runs (id, project_id, objective, state, policy_version, policy_snapshot,
			created_at, updated_at)
		VALUES ('wf-133', 'p', 'objective', 'running', 'v1', '{}', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
	); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO workflow_steps (id, workflow_run_id, kind, ordinal, state, created_at, updated_at)
		VALUES ('wfs-work', 'wf-133', 'work', 1, 'running', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
	); err != nil {
		t.Fatalf("seed step: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO workflow_dispatch_checkpoints (id, workflow_run_id, workflow_step_id, attempt_id,
			checkpoint_id, phase, idempotency_key, harness, session_id, launch_stage, launch_outcome,
			error_class, evidence_json, detail, created_at)
		VALUES ('wfd-legacy', 'wf-133', 'wfs-work', NULL, NULL, 'worker_dispatched',
			'workflow-step-spawn:wfs-work', 'codex', NULL, 'spawn', 'ambiguous', '', '{}',
			'spawn returned no error and no session', '2026-08-20T00:00:00Z')`,
	); err != nil {
		t.Fatalf("seed legacy dispatch checkpoint: %v", err)
	}
	return db
}

// TestMigration0134LeavesPre0134DispatchRowsWithoutLaunchEvidence is why every
// column 0134 adds is defaulted or nullable.
//
// A dispatch checkpoint written before the launch evidence had columns knows
// nothing about the workspace it was aimed at or the process it produced, and
// it must keep reading back as exactly that: empty strings and, above all, a
// nil LaunchedAt. Borrowing CreatedAt for the launch instant would be the one
// failure this table cannot afford — it would hand a person a launch time for
// a launch nobody recorded.
func TestMigration0134LeavesPre0134DispatchRowsWithoutLaunchEvidence(t *testing.T) {
	db := seedPre0134DispatchCheckpoint(t)
	if err := migrate(db); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}

	ctx := t.Context()
	store := sqlitestore.NewStore(db, db)

	got, ok, err := store.GetLatestWorkflowDispatchCheckpointByStep(ctx, "wfs-work")
	if err != nil {
		t.Fatalf("get latest dispatch checkpoint on legacy row: %v", err)
	}
	if !ok || got.ID != "wfd-legacy" {
		t.Fatalf("latest dispatch checkpoint = %+v ok=%v, want the legacy row", got, ok)
	}
	if got.Phase != domain.DispatchPhaseWorkerDispatched || got.LaunchOutcome != domain.LaunchOutcomeAmbiguous {
		t.Fatalf("legacy dispatch checkpoint lost its own facts: %+v", got)
	}
	for name, value := range map[string]string{
		"Branch":               got.Branch,
		"WorktreePath":         got.WorktreePath,
		"BaseSHA":              got.BaseSHA,
		"WorkspaceFingerprint": got.WorkspaceFingerprint,
		"RuntimeHandleID":      got.RuntimeHandleID,
		"RuntimeLaunchID":      got.RuntimeLaunchID,
		"AgentSessionID":       got.AgentSessionID,
	} {
		if value != "" {
			t.Errorf("%s = %q, want empty for a pre-0134 dispatch row", name, value)
		}
	}
	if got.LaunchedAt != nil {
		t.Errorf("LaunchedAt = %v, want nil for a pre-0134 dispatch row", *got.LaunchedAt)
	}
	if !got.CreatedAt.Equal(time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("CreatedAt = %v, want the legacy write time", got.CreatedAt)
	}

	byRun, err := store.ListWorkflowDispatchCheckpointsByRun(ctx, "wf-133")
	if err != nil {
		t.Fatalf("list dispatch checkpoints on legacy run: %v", err)
	}
	if len(byRun) != 1 || byRun[0].LaunchedAt != nil {
		t.Fatalf("legacy run's dispatch checkpoints = %+v, want one row with no launch instant", byRun)
	}
}

// TestMigration0134RecordsLaunchEvidenceOnUpgradedRuns checks the other half:
// the same upgraded database can carry the whole launch-evidence set on the
// table 0133 created — no second table, no parallel record — and can still
// record a boundary that legitimately has none of it.
func TestMigration0134RecordsLaunchEvidenceOnUpgradedRuns(t *testing.T) {
	db := seedPre0134DispatchCheckpoint(t)
	if err := migrate(db); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}

	ctx := t.Context()
	store := sqlitestore.NewStore(db, db)
	stepID := "wfs-work"
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	launched := now.Add(-90 * time.Second)

	full := domain.WorkflowDispatchCheckpoint{
		ID:                   "wfd-full",
		WorkflowRunID:        "wf-133",
		WorkflowStepID:       &stepID,
		Phase:                domain.DispatchPhaseWorkerDispatched,
		IdempotencyKey:       "workflow-step-spawn:wfs-work",
		Harness:              "claude-code",
		LaunchStage:          domain.LaunchStageSpawn,
		LaunchOutcome:        domain.LaunchOutcomeDispatched,
		Detail:               "tmux session created",
		Branch:               "feat/thing",
		WorktreePath:         "/tmp/wt/feat-thing",
		BaseSHA:              "base0",
		WorkspaceFingerprint: "fp-at-launch",
		RuntimeHandleID:      "tmux:ao-7",
		RuntimeLaunchID:      "launch-3",
		AgentSessionID:       "agent-abc",
		LaunchedAt:           &launched,
		CreatedAt:            now,
	}
	written, err := store.CreateWorkflowDispatchCheckpoint(ctx, full)
	if err != nil {
		t.Fatalf("create dispatch checkpoint with launch evidence: %v", err)
	}
	if written.LaunchedAt == nil || !written.LaunchedAt.Equal(launched) {
		t.Fatalf("LaunchedAt = %v, want %v", written.LaunchedAt, launched)
	}
	if written.WorkflowStepID == nil || *written.WorkflowStepID != stepID {
		t.Fatalf("WorkflowStepID = %v, want %q", written.WorkflowStepID, stepID)
	}
	// The remaining comparison is by value; the pointer and time fields just
	// checked above are aliased so struct equality can carry the rest.
	full.EvidenceJSON = "{}" // defaulted by the store, not by the caller
	written.WorkflowStepID = full.WorkflowStepID
	written.LaunchedAt = full.LaunchedAt
	written.CreatedAt = full.CreatedAt
	if written != full {
		t.Fatalf("dispatch checkpoint read back changed:\n got %+v\nwant %+v", written, full)
	}

	// A launch that never happened records that it never happened: the stage
	// it died at, and no launch instant at all.
	preflight, err := store.CreateWorkflowDispatchCheckpoint(ctx, domain.WorkflowDispatchCheckpoint{
		ID:             "wfd-preflight",
		WorkflowRunID:  "wf-133",
		WorkflowStepID: &stepID,
		Phase:          domain.DispatchPhaseWorkerLaunchError,
		IdempotencyKey: "workflow-step-spawn:wfs-work",
		Harness:        "claude-code",
		LaunchStage:    domain.LaunchStagePreflight,
		LaunchOutcome:  domain.LaunchOutcomeFailed,
		ErrorClass:     domain.WorkflowErrorAuth,
		Detail:         "provider requires an interactive login",
		CreatedAt:      now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("create preflight-failure dispatch checkpoint: %v", err)
	}
	if preflight.LaunchedAt != nil {
		t.Errorf("LaunchedAt = %v, want nil for a launch that never started", *preflight.LaunchedAt)
	}
	if preflight.Branch != "" || preflight.WorktreePath != "" || preflight.RuntimeLaunchID != "" {
		t.Errorf("a refused launch was given workspace/process evidence: %+v", preflight)
	}

	byStep, err := store.ListWorkflowDispatchCheckpointsByStep(ctx, stepID)
	if err != nil {
		t.Fatalf("list dispatch checkpoints by step: %v", err)
	}
	if len(byStep) != 3 {
		t.Fatalf("dispatch checkpoints by step = %d, want the legacy row plus both new ones", len(byStep))
	}
}
