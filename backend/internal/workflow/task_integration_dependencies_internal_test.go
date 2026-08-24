package workflow

// The dependency gate reads what LANDED, and there are two durable records of
// that. Reading only one of them was a silent livelock: a pending dependency is
// deliberately a quiet wait, so a task whose dependency looked un-integrated
// simply never landed, forever, with nothing written anywhere to say why.
//
// That is exactly what master run wf-872e7f57 did. Its seven completed tasks
// were integrated by builds predating the Coordinator's audit ledger, so that
// ledger was empty while the master's own promotion ledger held all seven — and
// task 8, the first task with an integration dependency to reach the new gate,
// waited on a dependency that had in fact landed hours earlier.

import (
	stdctx "context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/integration"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func newDepsFixture(t *testing.T) (*Coordinator, *sqlite.Store, stdctx.Context, domain.WorkflowRun) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	now := time.Now().UTC()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	master := domain.WorkflowRun{ID: "wf-deps", ProjectID: "p", Objective: "o",
		State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}",
		CreatedAt: now, UpdatedAt: now}
	if _, _, err := store.CreateWorkflowRun(ctx, master, nil); err != nil {
		t.Fatal(err)
	}
	coord := New(Deps{Store: store, Projects: store, Clock: func() time.Time { return time.Now().UTC() }})
	return coord, store, ctx, master
}

// seedPromotionAt is seedPromotion with an explicit timestamp, which these
// tests need in order to say which of two landings is the later one.
func seedPromotionAt(t *testing.T, ctx stdctx.Context, store *sqlite.Store, runID, taskID, head string, at time.Time) {
	t.Helper()
	payload, _ := json.Marshal(masterIntegrationPromotionPayload{TaskID: taskID})
	if _, err := store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-promo-" + taskID + "-" + head, WorkflowRunID: runID, ProjectID: "p",
		HeadSHA: head, RetryState: string(payload), DurablePhase: masterIntegrationDurablePhase,
		PayloadVersion: masterIntegrationPayloadVersion, CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
}

// A dependency integrated before the audit ledger existed still counts. Its
// promotion checkpoint is the only record of it, and it is a real one.
func TestDependencyIsSatisfiedByTheMastersOwnPromotionLedger(t *testing.T) {
	coord, store, ctx, master := newDepsFixture(t)
	seedPromotionAt(t, ctx, store, master.ID, "task-7", "sha-seven", time.Now().UTC())

	deps, err := coord.integrationDependencies(ctx, master, domain.WorkflowTask{
		ID: "task-8", WorkflowRunID: master.ID, Dependencies: []string{"task-7"}, ScopeJSON: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("dependencies = %d, want 1", len(deps))
	}
	if deps[0].IntegratedSHA != "sha-seven" {
		t.Fatalf("integrated sha = %q, want the commit the promotion left the ref at", deps[0].IntegratedSHA)
	}
}

// Where both records exist the audit row wins: it is the more precise one, and
// it distinguishes an attempt from a landing.
func TestAuditLedgerOverridesThePromotionLedger(t *testing.T) {
	coord, store, ctx, master := newDepsFixture(t)
	now := time.Now().UTC()
	seedPromotionAt(t, ctx, store, master.ID, "task-7", "sha-old", now)

	ledger := integrationLedger{c: coord, parent: master}
	if err := ledger.RecordIntegration(ctx, integration.Record{
		TaskID: "task-7", Outcome: integration.OutcomeIntegrated,
		TargetBeforeSHA: "sha-old", TargetAfterSHA: "sha-new", Strategy: integration.StrategyFastForward,
	}); err != nil {
		t.Fatal(err)
	}

	deps, err := coord.integrationDependencies(ctx, master, domain.WorkflowTask{
		ID: "task-8", WorkflowRunID: master.ID, Dependencies: []string{"task-7"}, ScopeJSON: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	if deps[0].IntegratedSHA != "sha-new" {
		t.Fatalf("integrated sha = %q, want the audit row's target-after", deps[0].IntegratedSHA)
	}
}

// An attempt that did not land is not a landing, from either record.
func TestAnUnlandedAttemptDoesNotSatisfyADependency(t *testing.T) {
	coord, _, ctx, master := newDepsFixture(t)
	ledger := integrationLedger{c: coord, parent: master}
	if err := ledger.RecordIntegration(ctx, integration.Record{
		TaskID: "task-7", Outcome: integration.OutcomeAttempting,
		TargetBeforeSHA: "sha-old", TargetAfterSHA: "sha-new",
	}); err != nil {
		t.Fatal(err)
	}
	deps, err := coord.integrationDependencies(ctx, master, domain.WorkflowTask{
		ID: "task-8", WorkflowRunID: master.ID, Dependencies: []string{"task-7"}, ScopeJSON: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	if deps[0].IntegratedSHA != "" {
		t.Fatalf("integrated sha = %q, want empty — an attempt is not a landing", deps[0].IntegratedSHA)
	}
}

// The newest landing wins within a source: a task integrated, parked and
// integrated again is on the target at its LAST successful commit.
func TestNewestLandingWins(t *testing.T) {
	coord, store, ctx, master := newDepsFixture(t)
	base := time.Now().UTC()
	seedPromotionAt(t, ctx, store, master.ID, "task-7", "sha-first", base)
	seedPromotionAt(t, ctx, store, master.ID, "task-7", "sha-second", base.Add(time.Minute))

	deps, err := coord.integrationDependencies(ctx, master, domain.WorkflowTask{
		ID: "task-8", WorkflowRunID: master.ID, Dependencies: []string{"task-7"}, ScopeJSON: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	if deps[0].IntegratedSHA != "sha-second" {
		t.Fatalf("integrated sha = %q, want the later promotion", deps[0].IntegratedSHA)
	}
}
