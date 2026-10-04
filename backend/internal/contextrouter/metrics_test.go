package contextrouter

import (
	"context"
	"strings"
	"testing"

	baseline "github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory"
)

// The origin split is what separates "AO sent less" from "AO made the agent
// fetch it itself": graph and memory are served from stores AO already built,
// everything else is content read for this dispatch.
func TestSectionOriginSplitsIndexedFromRead(t *testing.T) {
	for kind, want := range map[SectionKind]Origin{
		SectionTask:     OriginRead,
		SectionDocument: OriginRead,
		SectionDiff:     OriginRead,
		SectionGraph:    OriginIndexed,
		SectionMemory:   OriginIndexed,
	} {
		if got := kind.Origin(); got != want {
			t.Fatalf("%s classified as %q, want %q", kind, got, want)
		}
	}
}

// A selection sizes both what it could have sent and what it sent, and the
// origin halves add up to the whole.
func TestSelectionSizesConsideredAndSelected(t *testing.T) {
	router, err := New(Options{
		Budgets: BudgetSet{
			RolePlanner:  {CompactTokens: 200, ExpandedTokens: 400, HardCapTokens: 600},
			RoleWorker:   {CompactTokens: 200, ExpandedTokens: 400, HardCapTokens: 600},
			RoleReviewer: {CompactTokens: 200, ExpandedTokens: 400, HardCapTokens: 600},
			RoleFix:      {CompactTokens: 200, ExpandedTokens: 400, HardCapTokens: 600},
			RoleVerify:   {CompactTokens: 200, ExpandedTokens: 400, HardCapTokens: 600},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	huge := strings.Repeat("the whole tracker issue, several times over. ", 900)
	selection, err := router.Select(context.Background(), Request{
		Role:      RoleWorker,
		Task:      Task{ID: "t-1", Objective: "bound the retry loop"},
		Project:   Project{ID: "p-1", Root: t.TempDir()},
		Documents: []Document{{Path: "issue context", Content: huge}},
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if selection.Considered.Bytes < len(huge) {
		t.Fatalf("considered %d bytes for a %d-byte document; the pre-retrieval size is what routing is measured against",
			selection.Considered.Bytes, len(huge))
	}
	if selection.Selected.Bytes >= selection.Considered.Bytes {
		t.Fatalf("selected %d of a considered %d; a budgeted selection must send less", selection.Selected.Bytes, selection.Considered.Bytes)
	}
	if selection.Selected.Bytes != selection.EstimatedBytes {
		t.Fatalf("selected size %d disagrees with the selection's own byte total %d", selection.Selected.Bytes, selection.EstimatedBytes)
	}
	if got := selection.Selected.IndexedBytes + selection.Selected.ReadBytes; got != selection.Selected.Bytes {
		t.Fatalf("origin halves sum to %d, want %d", got, selection.Selected.Bytes)
	}
	if selection.Selected.IndexedBytes != 0 {
		t.Fatalf("no graph or memory source was configured, so nothing could be reused (got %d bytes)", selection.Selected.IndexedBytes)
	}
}

// The bridge to the baseline record: a selection converts into a routing block
// whose sizes are the selection's own, labeled the way the baseline requires.
func TestBaselineRoutingCarriesTheSelectionsOwnSizes(t *testing.T) {
	selection := Selection{
		Role:       RoleWorker,
		Tier:       TierCompact,
		Considered: Sizes{Sections: 4, Bytes: 8000},
		Selected:   Sizes{Sections: 2, Bytes: 2000, IndexedBytes: 500, ReadBytes: 1500, Truncated: 1},
		Dropped:    []Dropped{{Kind: SectionMemory, Title: "memory"}},
		Limit:      4000,
		Budget:     Budget{CompactTokens: 4000, ExpandedTokens: 10000, HardCapTokens: 14000},
	}
	routing := selection.BaselineRouting()
	if err := routing.Validate(); err != nil {
		t.Fatalf("the converted routing block is invalid: %v", err)
	}
	if !routing.Enabled || routing.Role != string(RoleWorker) || routing.Tier != string(TierCompact) {
		t.Fatalf("routing block does not describe the selection: %+v", routing)
	}
	for name, got := range map[string]struct {
		metric baseline.Metric
		want   int64
	}{
		"potential": {routing.PotentialBytes, 8000},
		"selected":  {routing.SelectedBytes, 2000},
		"reused":    {routing.ReusedBytes, 500},
		"new":       {routing.NewBytes, 1500},
	} {
		if got.metric.Value == nil || *got.metric.Value != got.want {
			t.Fatalf("%s bytes %v, want %d", name, got.metric.Value, got.want)
		}
	}
	if routing.Dropped != 1 || routing.Truncated != 1 || routing.Sections != 2 {
		t.Fatalf("routing block lost the selection's counts: %+v", routing)
	}
	if pct, ok := routing.ReductionPercent(); !ok || pct != 75 {
		t.Fatalf("reduction %.1f%% (ok=%v), want 75%%", pct, ok)
	}
}
