package projectmemory_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/codegraph"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	svc "github.com/aoagents/agent-orchestrator/backend/internal/service/projectmemory"
)

// real_projects_test.go -- P4-G section 18: the operational check, against real
// checkouts rather than fixtures.
//
// It is SKIPPED unless AO_P4G_REAL_PROJECTS names directories that exist, so it
// never fails CI or a machine that does not have these repositories. That is
// the honest arrangement for a test whose subject is somebody's working copy:
// its value is the measurement it records when it can run, and a hard
// dependency on local paths would make the suite lie about its own coverage
// everywhere else.
//
// It reads the checkouts and writes only to a temporary database. Nothing here
// touches the real ~/.ao data directory or modifies a single source file.

func TestRealProjectsIndexAndAnswer(t *testing.T) {
	raw := os.Getenv("AO_P4G_REAL_PROJECTS")
	if raw == "" {
		t.Skip("set AO_P4G_REAL_PROJECTS to a colon-separated list of checkouts to run the operational check")
	}
	for _, root := range filepath.SplitList(raw) {
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			t.Logf("skipping %s: not a directory", root)
			continue
		}
		t.Run(filepath.Base(root), func(t *testing.T) {
			f := realProjectFixture(t, root)

			started := time.Now()
			out, err := f.service.GraphSync(f.ctx, testProject, "", false)
			if err != nil {
				t.Fatalf("index %s: %v", root, err)
			}
			elapsed := time.Since(started)
			t.Logf("INDEXED %s: files=%d symbols=%d relations=%d parsed=%d kind=%s in %s",
				root, out.Files, out.Symbols, out.Edges, out.FilesParsed, out.Kind, elapsed)

			if out.Files == 0 || out.Symbols == 0 {
				t.Fatalf("%s indexed nothing: %+v", root, out)
			}

			overview, err := f.service.Intelligence(f.ctx, testProject)
			if err != nil {
				t.Fatalf("Intelligence: %v", err)
			}
			if len(overview.Repos) == 0 || overview.Repos[0].State != "ready" {
				t.Fatalf("%s did not reach ready: %+v", root, overview.Repos)
			}

			// The architecture the build derived, which is what a dispatch is
			// given and what the Architecture tab renders.
			arch, rendered, err := f.service.Architecture(f.ctx, testProject, "")
			if err != nil {
				t.Fatalf("Architecture: %v", err)
			}
			t.Logf("ARCHITECTURE %s: %d structured groups, %d bytes rendered",
				root, len(arch), len(rendered))

			// A representative question. Usefulness is recorded rather than
			// asserted: what a good answer looks like differs per repository,
			// and a threshold here would be a number invented to be met.
			res, err := f.service.Search(f.ctx, svc.SearchRequest{
				ProjectID: testProject, Query: "service handler controller repository",
			})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			t.Logf("QUERY %s: %d hits (%d memory, %d graph), truncated=%v",
				root, len(res.Hits), res.MemoryHits, res.SymbolHits, res.Truncated)
			for i, hit := range res.Hits {
				if i >= 5 {
					break
				}
				t.Logf("  hit[%d] %s %s %s:%d", i, hit.Kind, hit.Title, hit.Path, hit.Line)
			}

			// A bounded walk from the first symbol found, which is the Graph
			// tab's whole interaction.
			if len(res.Hits) > 0 {
				sub, err := f.service.Subgraph(f.ctx, svc.SubgraphRequest{
					ProjectID: testProject, Symbol: res.Hits[0].Title, Depth: 2,
				})
				if err != nil {
					t.Fatalf("Subgraph: %v", err)
				}
				t.Logf("SUBGRAPH %s from %q: %d nodes %d relations truncated=%v",
					root, res.Hits[0].Title, len(sub.Nodes), len(sub.Edges), sub.Truncated)
				if len(sub.Nodes) > svc.MaxSubgraphNodes {
					t.Fatalf("the node ceiling did not bind on a real project: %d", len(sub.Nodes))
				}
				if len(sub.Edges) > svc.MaxSubgraphEdges {
					t.Fatalf("the edge ceiling did not bind on a real project: %d", len(sub.Edges))
				}
			}
		})
	}
}

// realProjectFixture registers an existing checkout against a throwaway
// database. The checkout is only ever read.
func realProjectFixture(t *testing.T, root string) *fixture {
	t.Helper()
	f := newFixture(t)
	if err := f.store.UpsertProject(f.ctx, domain.ProjectRecord{
		ID: string(testProject), Path: root, RegisteredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("register %s: %v", root, err)
	}
	f.root = root
	f.service = f.service.WithGraph(codegraph.NewIndex(f.store)).WithExplorer(f.store)
	return f
}
