package workflow

import (
	stdctx "context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/integration"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// mutation_provenance_internal_test.go — P2-D §29, at the writer.
//
// The durable half (exactly-once, the partial unique index, legacy rows) is in
// storage/sqlite/migrate_memory_provenance_test.go, against real SQLite. What
// is left for this file is the WRITER's own behaviour: the generation fence,
// the fact that a boundary is derived from what AO observed rather than from
// what a caller passed, and the two refusals that keep an unfinished task from
// producing evidence it did not earn.

func provenanceCoordinator(prov *fakeMutationProvenance) *Coordinator {
	return &Coordinator{
		mutationProvenance: prov,
		clock:              func() time.Time { return time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC) },
		newID:              func() string { return "fixed" },
	}
}

// TestStaleGenerationBoundaryWriteIsRefused is P2-D §6 at the writer.
//
// A worker, reviewer or repair callback that wakes up after a newer generation
// has recorded the same boundary must not append a row that a later
// "newest wins" read would treat as current. The refusal is not an error --
// a stale callback is a normal event, and reporting it as a failure would make
// several best-effort paths log noise about something that worked.
func TestStaleGenerationBoundaryWriteIsRefused(t *testing.T) {
	prov := &fakeMutationProvenance{}
	c := provenanceCoordinator(prov)
	run := domain.WorkflowRun{ID: "wf-1", ProjectID: "proj-1"}

	base := mutationBoundary{
		run: run, taskID: "task-1", boundary: domain.BoundaryVerified,
		placement: domain.MutationPlacementDirectBranch,
		headSHA:   "sha-new", generation: 3,
	}
	if ok := c.recordMutationBoundary(stdctx.Background(), base); !ok {
		t.Fatal("the current-generation write was refused")
	}

	stale := base
	stale.generation = 1
	stale.headSHA = "sha-old"
	if ok := c.recordMutationBoundary(stdctx.Background(), stale); ok {
		t.Fatal("a callback carrying an older generation recorded a boundary")
	}
	if len(prov.rows) != 1 {
		t.Fatalf("%d rows, want 1: the stale write appended anyway", len(prov.rows))
	}
	if prov.rows[0].HeadSHA != "sha-new" {
		t.Fatalf("the surviving row is the stale one: %+v", prov.rows[0])
	}

	// An EQUAL generation is the same attempt, not a stale one -- a duplicate
	// callback about the current attempt must be collapsed by the idempotency
	// key rather than refused by the fence.
	same := base
	if ok := c.recordMutationBoundary(stdctx.Background(), same); !ok {
		t.Fatal("a duplicate callback at the same generation was refused by the generation fence")
	}
	if len(prov.rows) != 1 {
		t.Fatalf("%d rows after a duplicate at the same generation, want 1", len(prov.rows))
	}
}

// TestBoundaryIdempotencyKeySeparatesRealMoments is the other half: collapsing
// must not swallow a genuinely different moment.
//
// A re-integration onto a moved target is a different boundary from the first
// one, and it has to have its own row or the "newest wins" read would keep
// pointing a promotion at the superseded integration.
func TestBoundaryIdempotencyKeySeparatesRealMoments(t *testing.T) {
	run := domain.WorkflowRun{ID: "wf-1"}
	first := mutationBoundary{
		run: run, taskID: "task-1", boundary: domain.BoundaryIntegrated,
		generation: 1, headSHA: "src", integrationTargetAfterSHA: "tgt-1",
	}
	second := first
	second.integrationTargetAfterSHA = "tgt-2"
	second.generation = 2

	if first.idempotencyKey() == second.idempotencyKey() {
		t.Fatal("two integrations onto different targets derive the same idempotency key")
	}
	repeat := first
	if repeat.idempotencyKey() != first.idempotencyKey() {
		t.Fatal("the same boundary described twice derives two different keys")
	}
}

// TestNoProvenanceStoreRecordsNothingAndPanicsNever is the "memory is off"
// shape, which every other best-effort path in this package also has.
//
// A daemon without the provenance store wired records nothing, and the
// consequence is a promotion that cannot be proven — LESS canonical knowledge,
// never wrong knowledge.
func TestNoProvenanceStoreRecordsNothingAndPanicsNever(t *testing.T) {
	c := &Coordinator{clock: time.Now, newID: func() string { return "x" }}
	if ok := c.recordMutationBoundary(stdctx.Background(), mutationBoundary{
		run: domain.WorkflowRun{ID: "wf-1"}, boundary: domain.BoundaryVerified,
	}); ok {
		t.Fatal("a coordinator with no provenance store reported that it recorded a boundary")
	}
}

// TestUnknownBoundaryIsNotRecorded keeps the vocabulary closed at the writer.
//
// A boundary this build cannot name is not evidence of anything, and writing it
// would put a row in an append-only table that no reader can interpret.
func TestUnknownBoundaryIsNotRecorded(t *testing.T) {
	prov := &fakeMutationProvenance{}
	c := provenanceCoordinator(prov)
	if ok := c.recordMutationBoundary(stdctx.Background(), mutationBoundary{
		run: domain.WorkflowRun{ID: "wf-1"}, taskID: "task-1", boundary: "teleported",
	}); ok {
		t.Fatal("an unrecognised boundary was recorded")
	}
	if len(prov.rows) != 0 {
		t.Fatalf("%d rows written for an unrecognised boundary", len(prov.rows))
	}
}

// TestIntegrationBoundaryRefusesToRecordAnUnprovenTarget is the "cancellation
// does not fabricate an integration" case.
//
// An integration that cannot name where the target ended up has proven
// nothing, and a row saying so would be WORSE than no row: a promotion would
// find it, see boundary = integrated, and read the absent SHA as an oversight
// rather than as the refusal it is.
func TestIntegrationBoundaryRefusesToRecordAnUnprovenTarget(t *testing.T) {
	prov := &fakeMutationProvenance{}
	c := provenanceCoordinator(prov)
	c.recordIntegratedBoundary(stdctx.Background(),
		domain.WorkflowRun{ID: "wf-1", ProjectID: "proj-1"},
		domain.WorkflowTask{ID: "task-1"},
		RunDetail{},
		domain.WorkflowCheckpoint{},
		integration.Record{SourceBranch: "ao/task-1", SourceSHA: "src"},
	)
	if len(prov.rows) != 0 {
		t.Fatalf("an integration with no target head was recorded as evidence: %+v", prov.rows)
	}
}

// TestIntegrationMethodMapsAncestryCorrectly pins the one mapping a promotion
// proof depends on.
//
// Cherry-pick is the single strategy whose source is legitimately unreachable
// from the target, so it is the single method for which ancestry is the WRONG
// proof. An unrecognised strategy must land on the same side as cherry-pick:
// proven by the recorded target SHAs, never by an ancestry check that may not
// apply.
func TestIntegrationMethodMapsAncestryCorrectly(t *testing.T) {
	for _, tc := range []struct {
		strategy      integration.Strategy
		method        domain.WorkflowIntegrationMethod
		ancestryProof bool
	}{
		{integration.StrategyFastForward, domain.IntegrationFastForward, true},
		{integration.StrategyRebaseFastForward, domain.IntegrationFastForward, true},
		{integration.StrategyMergeCommit, domain.IntegrationMerge, true},
		{integration.StrategyNoOp, domain.IntegrationDirectCommit, true},
		{integration.StrategyCherryPick, domain.IntegrationCherryPick, false},
		{integration.Strategy("something_new"), "", false},
	} {
		got := integrationMethodOf(tc.strategy)
		if got != tc.method {
			t.Fatalf("strategy %q mapped to %q, want %q", tc.strategy, got, tc.method)
		}
		if got.AncestryProves() != tc.ancestryProof {
			t.Fatalf("strategy %q: ancestry proves = %v, want %v",
				tc.strategy, got.AncestryProves(), tc.ancestryProof)
		}
	}
}

// TestRepairResultIsAttributedToTheRepairNotTheOriginalWorker is P2-D §16.
//
// When a fix step ran, the verified head is the REPAIR's output. Attributing it
// to the original worker would make the repair invisible to anything that later
// asks who produced the change project memory is about to promote — which is
// exactly the attribution P2-D requires to survive.
//
// The verified boundary is written either way, because it answers a different
// question: not "who produced this head" but "what did verification pass on",
// and memory pins its VerifiedCommit to that and to nothing else.
func TestRepairResultIsAttributedToTheRepairNotTheOriginalWorker(t *testing.T) {
	for _, tc := range []struct {
		name         string
		fixState     domain.WorkflowStepState
		wantBoundary domain.WorkflowMutationBoundary
		wantClass    domain.WorkflowMutationClass
	}{
		{"no fix ran", domain.WorkflowStepWaiting, domain.BoundaryWorkResult, domain.MutationAuthorizedWork},
		{"a fix ran", domain.WorkflowStepCompleted, domain.BoundaryRepairResult, domain.MutationAuthorizedFix},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := sqlitetest.MustOpen(t)
			ctx := stdctx.Background()
			base := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
			repo := t.TempDir()
			if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: repo, RegisteredAt: base}); err != nil {
				t.Fatal(err)
			}
			run := domain.WorkflowRun{
				ID: "wf-1", ProjectID: "p", Objective: "do the thing",
				State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}",
				CreatedAt: base, UpdatedAt: base,
			}
			steps := []domain.WorkflowStep{
				{ID: "wfs-work", WorkflowRunID: run.ID, Kind: domain.WorkflowStepWork, Ordinal: 1,
					State: domain.WorkflowStepCompleted, ArtifactJSON: "{}", CreatedAt: base, UpdatedAt: base},
				{ID: "wfs-fix", WorkflowRunID: run.ID, Kind: domain.WorkflowStepFix, Ordinal: 2,
					State: tc.fixState, ArtifactJSON: "{}", CreatedAt: base, UpdatedAt: base},
				{ID: "wfs-verify", WorkflowRunID: run.ID, Kind: domain.WorkflowStepVerify, Ordinal: 3,
					State: domain.WorkflowStepRunning, ArtifactJSON: "{}", CreatedAt: base, UpdatedAt: base},
			}
			if _, _, err := store.CreateWorkflowRun(ctx, run, steps); err != nil {
				t.Fatal(err)
			}

			prov := &fakeMutationProvenance{}
			c := New(Deps{
				Store: store, Projects: store, MutationProvenance: prov,
				Clock: func() time.Time { return base },
			})
			c.recordVerifiedBoundary(ctx, run, steps[2], "sha-verified")

			byBoundary := map[domain.WorkflowMutationBoundary]domain.WorkflowMutationProvenance{}
			for _, row := range prov.rows {
				byBoundary[row.Boundary] = row
			}
			author, ok := byBoundary[tc.wantBoundary]
			if !ok {
				t.Fatalf("no %s boundary was recorded; got %v", tc.wantBoundary, boundaryKinds(prov.rows))
			}
			if author.Class != tc.wantClass {
				t.Fatalf("class = %q, want %q", author.Class, tc.wantClass)
			}
			if tc.wantBoundary == domain.BoundaryRepairResult {
				if _, leaked := byBoundary[domain.BoundaryWorkResult]; leaked {
					t.Fatal("a repaired result was ALSO attributed to the original worker")
				}
			}
			verified, ok := byBoundary[domain.BoundaryVerified]
			if !ok {
				t.Fatalf("no verified boundary was recorded; got %v", boundaryKinds(prov.rows))
			}
			if verified.HeadSHA != "sha-verified" {
				t.Fatalf("verified head = %q, want the head verification passed on", verified.HeadSHA)
			}
		})
	}
}

func boundaryKinds(rows []domain.WorkflowMutationProvenance) []domain.WorkflowMutationBoundary {
	out := make([]domain.WorkflowMutationBoundary, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Boundary)
	}
	return out
}
