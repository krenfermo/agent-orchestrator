package projectmemory_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// incremental_equivalence_test.go — Frente 3 / 3B incremental correctness for
// project memory: after an incremental update that modifies a manifest
// (dropping a dependency), deletes a document, renames a source file and adds
// one, the SERVABLE facts equal those of a full index of the same tree. A
// valid fact the full index would not produce is a ghost; a dependency the
// manifest no longer declares is a stale dependency.

func validFacts(t *testing.T, f *fixture, repoID string) []string {
	t.Helper()
	items, err := f.store.ListProjectMemoryItems(f.ctx, testProject, repoID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, it := range items {
		if it.State != domain.MemoryStateValid || it.Origin != domain.OriginCanonical {
			continue
		}
		out = append(out, string(it.Key.Type)+":"+it.Key.Key+" => "+it.Summary+" | "+it.Content)
	}
	sort.Strings(out)
	return out
}

func TestMemoryIncrementalUpdateEqualsAFullIndex(t *testing.T) {
	inc := newFixture(t)
	root := goRepo(t)
	first := indexed(t, inc, root, "c1")

	writeTree(t, root, map[string]string{
		"go.mod":                  "module example.com/app\n\ngo 1.24\n\nrequire github.com/spf13/cobra v1.8.0\n",
		"internal/store/extra.go": "package store\n\nfunc Extra() {}\n",
		"CONTRIBUTING.md":         "# Contributing\n\nOpen a PR.\n",
	})
	if err := os.Remove(filepath.Join(root, "docs", "architecture.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "internal", "store", "query.go"), filepath.Join(root, "internal", "store", "q2.go")); err != nil {
		t.Fatal(err)
	}
	out, err := inc.idx.UpdateChanged(inc.ctx, projectmemory.UpdateRequest{
		ProjectID: testProject, RepoPath: root, ToCommit: "c2", Branch: "main",
		Changes: []projectmemory.PathChange{
			{Kind: projectmemory.ChangeModified, Path: "go.mod"},
			{Kind: projectmemory.ChangeAdded, Path: "internal/store/extra.go"},
			{Kind: projectmemory.ChangeAdded, Path: "CONTRIBUTING.md"},
			{Kind: projectmemory.ChangeDeleted, Path: "docs/architecture.md"},
			{Kind: projectmemory.ChangeRenamed, PreviousPath: "internal/store/query.go", Path: "internal/store/q2.go"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.FellBackToFullIndex {
		t.Fatalf("fell back to a full index: %s", out.FallbackReason)
	}
	incFacts := validFacts(t, inc, first.RepoID)

	full := newFixture(t)
	fullOut := indexed(t, full, root, "c2")
	fullFacts := validFacts(t, full, fullOut.RepoID)

	joined := strings.Join(incFacts, "\n")
	if strings.Contains(joined, "testify") {
		t.Fatalf("a dependency the manifest no longer declares is still a valid fact:\n%s", joined)
	}
	if strings.Contains(joined, "docs/architecture.md =>") || strings.Contains(joined, "query.go") {
		t.Fatalf("a fact about a deleted or renamed-away path is still valid:\n%s", joined)
	}
	if joined != strings.Join(fullFacts, "\n") {
		t.Fatalf("valid facts differ from a full index of the same tree\nincremental:\n%s\n\nfull:\n%s", joined, strings.Join(fullFacts, "\n"))
	}
}
