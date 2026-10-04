package usage_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	baseline "github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
)

// context_test.go — P3-E §7/§8/§9 and the memory-metrics matrix of §37.
//
// The claim under test throughout is the one the checkpoint refuses to let AO
// overstate: these are AO-ASSEMBLED sizes, and a "saving" may only be reported
// where a comparable baseline exists.

type fakeEvidenceSource struct {
	records []baseline.EvidenceRecord
	skipped int
	err     error
}

func (f *fakeEvidenceSource) ListForRun(context.Context, string) ([]baseline.EvidenceRecord, int, error) {
	return f.records, f.skipped, f.err
}

func measuredBytes(n int64) baseline.Metric { return baseline.Measured(n, "test") }

func record(role domain.WorkflowRole, sentBytes int64, memory *baseline.MemoryMetrics, routing *baseline.RoutingMetrics) baseline.EvidenceRecord {
	return baseline.EvidenceRecord{
		RecordID: string(role), GeneratedAt: time.Unix(1700000000, 0).UTC(), Role: role,
		Context: baseline.ContextMetrics{ContextSentBytes: measuredBytes(sentBytes)},
		Memory:  memory, Routing: routing,
	}
}

func TestContext_NoEvidenceIsAnAbsenceNotZero(t *testing.T) {
	reader := usagesvc.NewContextReader(&fakeEvidenceSource{})
	view, err := reader.WorkflowRun(context.Background(), "wf-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if view.Recorded {
		t.Fatal("a run with no evidence must report recorded=false so the UI says 'no context data recorded'")
	}
	if view.EstimateMethod == "" {
		t.Fatal("the estimator must name itself even when nothing was measured")
	}
}

func TestContext_NilSourceIsAnOrdinaryConfiguration(t *testing.T) {
	// The baseline harness switched off is a normal deployment, not a failure.
	view, err := usagesvc.NewContextReader(nil).WorkflowRun(context.Background(), "wf-1")
	if err != nil {
		t.Fatalf("a daemon without the evidence harness must still serve: %v", err)
	}
	if view.Recorded {
		t.Fatal("nothing recorded")
	}
}

func TestContext_SumsMeasuredBytesAndCountsUnmeasurable(t *testing.T) {
	// A dispatch that could not measure its payload contributes NOTHING and is
	// counted instead, so a partial total is visibly a lower bound rather than
	// reading as complete.
	unmeasured := record(domain.WorkflowRoleReviewer, 0, nil, nil)
	unmeasured.Context.ContextSentBytes = baseline.Unavailable("reviewer launch carries no payload")

	reader := usagesvc.NewContextReader(&fakeEvidenceSource{records: []baseline.EvidenceRecord{
		record(domain.WorkflowRoleWorker, 8_000, nil, nil),
		record(domain.WorkflowRoleWorker, 4_000, nil, nil),
		unmeasured,
	}})
	view, err := reader.WorkflowRun(context.Background(), "wf-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if view.AssembledBytes != 12_000 {
		t.Fatalf("assembled = %d, want 12000", view.AssembledBytes)
	}
	if view.Unmeasured != 1 {
		t.Fatalf("unmeasured = %d, want 1", view.Unmeasured)
	}
	if view.EstimatedAssembledTokens != 3_000 {
		t.Fatalf("estimated tokens = %d, want 3000 (12000 bytes / 4)", view.EstimatedAssembledTokens)
	}
}

func TestContext_ClaimsNoSavingWithoutAComparableBaseline(t *testing.T) {
	// A memory pack that ADDS 6 KB and replaces nothing has avoided nothing.
	// Reporting the pack's own size as a saving is the exact dishonesty P3-E §9
	// forbids.
	reader := usagesvc.NewContextReader(&fakeEvidenceSource{records: []baseline.EvidenceRecord{
		record(domain.WorkflowRoleWorker, 20_000, &baseline.MemoryMetrics{
			Mode: "assisted", PackItems: 5, PackBytes: 6_000, EstimatedPackTokens: 1_500,
			LegacyBytes: 12_000, TaskBytes: 2_000, ContextBytes: 20_000, DedupeSavedBytes: 0,
		}, nil),
	}})
	view, err := reader.WorkflowRun(context.Background(), "wf-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if view.AvoidedComparable {
		t.Fatal("no dedupe and no routing baseline: nothing supports a saving claim")
	}
	if view.AvoidedAssembledBytes != 0 {
		t.Fatalf("avoided = %d, want 0", view.AvoidedAssembledBytes)
	}
}

func TestContext_ReportsAvoidedOnlyFromDedupeAndRouting(t *testing.T) {
	reader := usagesvc.NewContextReader(&fakeEvidenceSource{records: []baseline.EvidenceRecord{
		record(domain.WorkflowRoleWorker, 20_000, &baseline.MemoryMetrics{
			Mode: "preferred", PackItems: 5, PackBytes: 6_000,
			LegacyBytes: 12_000, TaskBytes: 2_000, DedupeSavedBytes: 9_000,
		}, &baseline.RoutingMetrics{
			Enabled:        true,
			PotentialBytes: measuredBytes(30_000),
			SelectedBytes:  measuredBytes(20_000),
			ReusedBytes:    measuredBytes(6_000),
		}),
	}})
	view, err := reader.WorkflowRun(context.Background(), "wf-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if !view.AvoidedComparable {
		t.Fatal("both a dedupe proof and a router baseline exist")
	}
	// 9k replaced by memory + 10k of router candidates assembled and not sent.
	if view.AvoidedAssembledBytes != 19_000 {
		t.Fatalf("avoided = %d, want 19000", view.AvoidedAssembledBytes)
	}
	if view.EstimatedAvoidedTokens != 4_750 {
		t.Fatalf("estimated avoided tokens = %d, want 4750", view.EstimatedAvoidedTokens)
	}
}

func TestContext_MemoryModesAreReportedAsRecorded(t *testing.T) {
	for _, mode := range []string{"off", "assisted", "preferred"} {
		reader := usagesvc.NewContextReader(&fakeEvidenceSource{records: []baseline.EvidenceRecord{
			record(domain.WorkflowRoleWorker, 1_000, &baseline.MemoryMetrics{Mode: mode, Generation: 4, IndexedCommit: "abc123"}, nil),
		}})
		view, err := reader.WorkflowRun(context.Background(), "wf-1")
		if err != nil {
			t.Fatalf("WorkflowRun: %v", err)
		}
		if view.Memory.Mode != mode {
			t.Fatalf("mode = %q, want %q", view.Memory.Mode, mode)
		}
		if view.Memory.Generation != 4 || view.Memory.IndexedCommit != "abc123" {
			t.Fatalf("memory provenance = %+v", view.Memory)
		}
	}
}

func TestContext_WarmNoOpSyncIsVisibleAsANoOp(t *testing.T) {
	// The pair that proves a warm project's normal path costs nothing: a sync
	// that read no files, counted as a no-op rather than as work.
	reader := usagesvc.NewContextReader(&fakeEvidenceSource{records: []baseline.EvidenceRecord{
		record(domain.WorkflowRoleWorker, 1_000, &baseline.MemoryMetrics{
			Mode: "preferred", SyncPerformed: true, SyncKind: "none", SyncFilesRead: 0,
		}, nil),
		record(domain.WorkflowRoleWorker, 1_000, &baseline.MemoryMetrics{
			Mode: "preferred", SyncPerformed: true, SyncKind: "incremental", SyncFilesRead: 1,
		}, nil),
	}})
	view, err := reader.WorkflowRun(context.Background(), "wf-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if view.Memory.NoOpSyncs != 1 {
		t.Fatalf("noOpSyncs = %d, want 1", view.Memory.NoOpSyncs)
	}
	if view.Memory.IncrementalSyncs != 1 {
		t.Fatalf("incrementalSyncs = %d, want 1", view.Memory.IncrementalSyncs)
	}
	if view.Memory.SyncFilesRead != 1 {
		t.Fatalf("syncFilesRead = %d, want 1 — the warm path read nothing, the incremental one read one", view.Memory.SyncFilesRead)
	}
}

func TestContext_SharedKnowledgeReportsBothHalves(t *testing.T) {
	// "Did this task reuse a sibling's finding" cannot be answered by one
	// number: a task next to an earlier one shows candidates it took, an
	// unrelated one candidates it excluded, and both must be visible.
	reader := usagesvc.NewContextReader(&fakeEvidenceSource{records: []baseline.EvidenceRecord{
		record(domain.WorkflowRoleWorker, 5_000, &baseline.MemoryMetrics{
			Mode: "preferred", PackBytes: 3_000, KnowledgeBytes: 1_200,
			SharedCandidates: 9, SharedSelected: 4,
			SharedIrrelevantExcluded: 3, SharedUnauthorizedExcluded: 1, SupersededExcluded: 1,
			TaskLocalItems: 2, WorkflowLocalItems: 1, CanonicalItems: 1,
		}, nil),
	}})
	view, err := reader.WorkflowRun(context.Background(), "wf-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if view.Memory.SharedCandidates != 9 || view.Memory.SharedSelected != 4 || view.Memory.SharedExcluded != 5 {
		t.Fatalf("shared knowledge = %+v, want 9 candidates / 4 selected / 5 excluded", view.Memory)
	}
	if view.Memory.TaskLocalItems != 2 || view.Memory.WorkflowLocalItems != 1 || view.Memory.CanonicalItems != 1 {
		t.Fatalf("scope breakdown = %+v", view.Memory)
	}
	var sharedBytes int64
	for _, line := range view.BySource {
		if line.Source == domain.ContextSourceSharedKnowledge {
			sharedBytes = line.Bytes
		}
	}
	if sharedBytes != 1_200 {
		t.Fatalf("shared knowledge bytes = %d, want 1200", sharedBytes)
	}
}

func TestContext_UnattributedBytesLandInOtherRatherThanInflatingASource(t *testing.T) {
	// A dispatch with no memory block still sent something. Spreading that
	// remainder across the named sources would inflate whichever bucket the
	// spread favoured; it goes to "other" instead.
	reader := usagesvc.NewContextReader(&fakeEvidenceSource{records: []baseline.EvidenceRecord{
		record(domain.WorkflowRoleWorker, 10_000, nil, nil),
	}})
	view, err := reader.WorkflowRun(context.Background(), "wf-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if len(view.BySource) != 1 || view.BySource[0].Source != domain.ContextSourceOther {
		t.Fatalf("bySource = %+v, want everything in 'other'", view.BySource)
	}
	if view.BySource[0].Bytes != 10_000 {
		t.Fatalf("other = %d, want 10000", view.BySource[0].Bytes)
	}
}

func TestContext_RoleBreakdownSumsToTheTotal(t *testing.T) {
	reader := usagesvc.NewContextReader(&fakeEvidenceSource{records: []baseline.EvidenceRecord{
		record(domain.WorkflowRolePlanner, 3_000, nil, nil),
		record(domain.WorkflowRoleWorker, 9_000, nil, nil),
		record(domain.WorkflowRoleFixWorker, 4_000, nil, nil),
	}})
	view, err := reader.WorkflowRun(context.Background(), "wf-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	var sum int64
	for _, line := range view.ByRole {
		sum += line.AssembledBytes
	}
	if sum != view.AssembledBytes {
		t.Fatalf("role breakdown sums to %d, total is %d — two views of one number must agree", sum, view.AssembledBytes)
	}
	if len(view.ByRole) != 3 {
		t.Fatalf("roles = %d, want 3", len(view.ByRole))
	}
}
