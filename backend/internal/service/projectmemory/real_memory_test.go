package projectmemory_test

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/codegraph"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	pm "github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
	svc "github.com/aoagents/agent-orchestrator/backend/internal/service/projectmemory"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// real_memory_test.go -- P4-H section 14: the operational check for DERIVED
// memory, against real checkouts.
//
// It is the P4-H counterpart of real_projects_test.go and follows the same
// rules: skipped unless AO_P4H_REAL_PROJECTS names directories that exist, so
// it never fails CI or a machine without these repositories; the checkouts are
// only ever read; everything is written to a throwaway database.
//
// What it asserts is deliberately thin, and what it RECORDS is the point. The
// brief says quality matters more than count, and a threshold on fact count
// would be a number invented to be met. So the test asserts only the two
// things that are wrong under any reading -- that derivation produced
// something, and that a representative question reaches it -- and logs the
// facts themselves so a person can judge the quality the brief asks about.

func TestRealProjectsDeriveDurableMemory(t *testing.T) {
	raw := os.Getenv("AO_P4H_REAL_PROJECTS")
	if raw == "" {
		t.Skip("set AO_P4H_REAL_PROJECTS to a colon-separated list of checkouts to run the operational check")
	}
	for _, root := range filepath.SplitList(raw) {
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			t.Logf("skipping %s: not a directory", root)
			continue
		}
		t.Run(filepath.Base(root), func(t *testing.T) {
			f := realMemoryFixture(t, root)

			// 1. The graph, exactly as the reconciler drives it.
			graphStarted := time.Now()
			if _, err := f.service.GraphSync(f.ctx, testProject, "", false); err != nil {
				t.Fatalf("graph sync %s: %v", root, err)
			}
			t.Logf("GRAPH %s built in %s", root, time.Since(graphStarted))

			// 2. Memory derivation, exactly as the reconciler drives it.
			memStarted := time.Now()
			out, err := f.service.DeriveMemory(f.ctx, testProject, "", false)
			if err != nil {
				t.Fatalf("memory sync %s: %v", root, err)
			}
			t.Logf("MEMORY %s: kind=%s state=%s items=%d reconfirmed=%d insights=%d graphBacked=%v in %s",
				root, out.Kind, out.State, out.ItemsWritten, out.ItemsReconfirmed,
				out.InsightsDerived, out.InsightsGraphBacked, time.Since(memStarted))
			if out.InsightsSkipReason != "" {
				t.Logf("MEMORY %s: high-level facts degraded: %s", root, out.InsightsSkipReason)
			}
			if out.State != svc.MemoryReady {
				t.Errorf("%s did not reach ready: %s", root, out.State)
			}

			// 3. What was actually derived, by type. This is the census the
			//    brief asks to see, and the reason the test logs rather than
			//    asserts: "did AO learn anything useful about this project"
			//    is a question a person answers by reading these lines.
			items := f.items(t)
			if len(items) == 0 {
				t.Fatalf("%s derived no durable memory at all", root)
			}
			byType := map[string]int{}
			for _, item := range items {
				byType[string(item.Key.Type)]++
			}
			types := make([]string, 0, len(byType))
			for k := range byType {
				types = append(types, k)
			}
			sort.Strings(types)
			for _, k := range types {
				t.Logf("  %-24s %d", k, byType[k])
			}

			// 4. The high-level facts in full. These are what P4-H added, and
			//    reading them is how their quality is judged.
			for _, item := range items {
				if !highLevel[string(item.Key.Type)] {
					continue
				}
				t.Logf("  FACT [%s/%s conf=%.2f] %s",
					item.Key.Type, item.EvidenceClass, item.Confidence, item.Summary)
			}

			// 5. Representative questions. Every one of these returned zero
			//    memory hits on every real project before P4-H -- that is the
			//    finding the phase opened with.
			for _, query := range []string{
				"authentication and permissions",
				"how is this project built and tested",
				"database schema and storage",
				"how is this deployed",
				"where does the application start",
			} {
				res, err := f.service.Search(f.ctx, svc.SearchRequest{
					ProjectID: testProject, Query: query,
				})
				if err != nil {
					t.Fatalf("search %q: %v", query, err)
				}
				t.Logf("QUERY %-38q memory=%d graph=%d", query, res.MemoryHits, res.SymbolHits)
				for i, hit := range res.Hits {
					if i >= 3 || hit.Kind != "memory" {
						continue
					}
					t.Logf("    %s [%s] %s", hit.Kind, hit.MemoryType, hit.Title)
				}
				if res.MemoryHits == 0 {
					t.Errorf("%s: %q returned no durable memory hits", root, query)
				}
			}

			// 6. What a Planner would actually be handed.
			preview, err := f.service.ContextPreview(f.ctx, svc.ContextPreviewRequest{
				ProjectID: testProject, Role: "planner",
			})
			if err != nil {
				t.Fatalf("context preview: %v", err)
			}
			t.Logf("CONTEXT %s planner: candidates=%d(%dB) selected=%d(%dB) tokens~%d dropped=%d staleExcluded=%d",
				root, preview.CandidateItems, preview.CandidateBytes,
				preview.SelectedItems, preview.SelectedBytes, preview.EstimatedTokens,
				preview.DroppedItems, preview.StaleExcluded)
			t.Logf("CONTEXT %s graph half: consideredSymbols=%d consideredEdges=%d selectedSymbols=%d selectedEdges=%d bytes=%d",
				root, preview.Graph.ConsideredSymbols, preview.Graph.ConsideredEdges,
				preview.Graph.SelectedSymbols, preview.Graph.SelectedEdges, preview.Graph.Bytes)

			// Section 15: what the high-level facts cost, and what they answer.
			//
			// The comparison is deliberately stated as SELECTED and AVOIDED
			// rather than as a saving. AO cannot observe what a coding harness
			// reads inside a worktree, so it cannot know what its context
			// prevented anybody from reading; what it can report is what it
			// actually sent and what it actually had to choose from.
			var highLevelBytes, highLevelCount int
			for _, item := range items {
				if !highLevel[string(item.Key.Type)] {
					continue
				}
				highLevelCount++
				highLevelBytes += len(item.Summary) + len(item.Content)
			}
			// A REPRESENTATIVE TASK, which is what section 15 asks for: the
			// same piece of work previewed with a real subject, so the graph
			// half engages and both authorities can be measured against each
			// other on the same request.
			focused, err := f.service.ContextPreview(f.ctx, svc.ContextPreviewRequest{
				ProjectID: testProject, Role: "worker",
				Keywords: []string{"auth", "permission", "authorize"},
			})
			if err != nil {
				t.Fatalf("focused ContextPreview: %v", err)
			}
			t.Logf("TASK %s worker[auth]: memory candidates=%d(%dB) selected=%d(%dB) dropped=%d; "+
				"graph considered %d symbols / %d relations, selected %d / %d (%dB)",
				root, focused.CandidateItems, focused.CandidateBytes,
				focused.SelectedItems, focused.SelectedBytes, focused.DroppedItems,
				focused.Graph.ConsideredSymbols, focused.Graph.ConsideredEdges,
				focused.Graph.SelectedSymbols, focused.Graph.SelectedEdges, focused.Graph.Bytes)
			for _, section := range focused.Sections {
				t.Logf("    section %-34s %d item(s)", section.Title, len(section.Items))
			}

			t.Logf("VALUE %s: %d high-level facts totalling %dB, against a candidate corpus of %d facts / %dB",
				root, highLevelCount, highLevelBytes, preview.CandidateItems, preview.CandidateBytes)
		})
	}
}

// highLevel names the P4-H fact types, so the log can single them out from the
// document- and census-backed facts P2-A already produced.
var highLevel = map[string]bool{
	string(domain.MemoryTypeArchitecture):    true,
	string(domain.MemoryTypeEntryPoint):      true,
	string(domain.MemoryTypeRuntimeSurface):  true,
	string(domain.MemoryTypePersistence):     true,
	string(domain.MemoryTypeAuthModel):       true,
	string(domain.MemoryTypeIntegration):     true,
	string(domain.MemoryTypeTestingSurface):  true,
	string(domain.MemoryTypeConfigSurface):   true,
	string(domain.MemoryTypeDeployment):      true,
	string(domain.MemoryTypeProjectOverview): true,
}

// items reads back what the derivation wrote, through the same API surface the
// Memory tab uses.
func (f *fixture) items(t *testing.T) []domain.ProjectMemoryItem {
	t.Helper()
	res, err := f.service.Inspect(f.ctx, controllers.ProjectMemoryInspectQuery{
		ProjectID: testProject, Limit: 500,
	})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	return res.Items
}

// realMemoryFixture registers an existing checkout against a throwaway
// database, with the code graph wired into project memory so the high-level
// derivation has structural evidence to read.
func realMemoryFixture(t *testing.T, root string) *fixture {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := t.Context()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID: string(testProject), Path: root, RegisteredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("register %s: %v", root, err)
	}
	graph := codegraph.NewIndex(store)
	return &fixture{
		store: store, root: root, ctx: ctx,
		service: svc.New(pm.NewService(store, pm.WithCodeGraph(graph)), store).
			WithGraph(graph).WithExplorer(store),
	}
}
