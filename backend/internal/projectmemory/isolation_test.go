package projectmemory_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/codegraph"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// isolation_test.go — Frente 3 / 3B: project A can never retrieve project B's
// nodes, facts, context, paths or summaries, even when the two share file
// names, symbol names and branch names in one database -- the ordinary
// multi-project installation.

const (
	projA = domain.ProjectID("proj-a")
	projB = domain.ProjectID("proj-b")
)

// twinRepo builds a repository whose SHAPE is identical for every project
// (same paths, same symbols, same branch); only the marker differs.
func twinRepo(t *testing.T, marker string) string {
	t.Helper()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"go.mod":                 "module example.com/app\n\ngo 1.24\n",
		"AGENTS.md":              "# AGENTS.md\n\n## Coding conventions\n\nConvention of " + marker + ".\n",
		"README.md":              "# App\n\nThis is the " + marker + " service.\n",
		"docs/architecture.md":   "# Architecture\n\nOwned by " + marker + ".\n",
		"internal/auth/login.go": "package auth\n\n// Login authenticates a user of " + marker + ".\nfunc Login() {}\n",
		"cmd/app/main.go":        "// Command app is the " + marker + " entry point.\npackage main\n\nfunc main() {}\n",
		".gitignore":             ".claude/\n",
	})
	gitRun(t, root, "init", "-q", "-b", "main")
	gitRun(t, root, "add", "-A")
	gitRun(t, root, "commit", "-q", "-m", "init")
	return root
}

type twinFixture struct {
	ctx          context.Context
	store        *sqlite.Store
	svc          *projectmemory.Service
	prov         *projectmemory.Provisioner
	rootA, rootB string
}

func newTwinFixture(t *testing.T) twinFixture {
	t.Helper()
	requireGit(t)
	ctx := context.Background()
	st := sqlitetest.MustOpen(t)
	rootA, rootB := twinRepo(t, "MARKER-ALPHA"), twinRepo(t, "MARKER-BRAVO")
	for id, root := range map[domain.ProjectID]string{projA: rootA, projB: rootB} {
		if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: string(id), Path: root, RegisteredAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	svc := projectmemory.NewService(st, projectmemory.WithCodeGraph(codegraph.NewIndex(st)))
	cfg := projectmemory.DefaultConfig()
	cfg.Mode = projectmemory.ModeAssisted
	return twinFixture{ctx: ctx, store: st, svc: svc, prov: projectmemory.NewProvisioner(svc, cfg), rootA: rootA, rootB: rootB}
}

func (f twinFixture) render(t *testing.T, project domain.ProjectID, root string, role projectmemory.PackRole) (string, projectmemory.Provisioned) {
	t.Helper()
	out := f.prov.Provision(f.ctx, projectmemory.ProvisionRequest{
		ProjectID: project, RepoPath: root, Role: role,
		Keywords:     []string{"login", "auth", "user", "service", "entry"},
		ChangedPaths: []string{"internal/auth/login.go"},
	})
	return out.Render(), out
}

var allRoles = []projectmemory.PackRole{
	projectmemory.RolePlanner, projectmemory.RoleWorker, projectmemory.RoleReviewer, projectmemory.RoleRepair,
}

func TestProjectsSharingNamesNeverSeeEachOthersMemoryOrGraph(t *testing.T) {
	f := newTwinFixture(t)
	for _, tc := range []struct {
		project       domain.ProjectID
		root          string
		mine, foreign string
	}{
		{projA, f.rootA, "MARKER-ALPHA", "MARKER-BRAVO"},
		{projB, f.rootB, "MARKER-BRAVO", "MARKER-ALPHA"},
	} {
		sawMine := false
		for _, role := range allRoles {
			rendered, out := f.render(t, tc.project, tc.root, role)
			if !out.Attached() {
				t.Fatalf("%s/%s: nothing attached: %s", tc.project, role, out.Metrics.FallbackReason)
			}
			if strings.Contains(rendered, tc.foreign) {
				t.Fatalf("%s/%s pack carries the other project's content:\n%s", tc.project, role, rendered)
			}
			if strings.Contains(rendered, tc.mine) {
				sawMine = true
			}
		}
		if !sawMine {
			t.Fatalf("%s: no role saw its own marker -- the isolation assertion would be vacuous", tc.project)
		}
	}

	// Stored rows: B's repository id under A's project yields nothing.
	_, outA := f.render(t, projA, f.rootA, projectmemory.RoleWorker)
	_, outB := f.render(t, projB, f.rootB, projectmemory.RoleWorker)
	if outA.Freshness.RepoID == "" || outB.Freshness.RepoID == "" {
		t.Fatal("fixture broken: no repo ids")
	}
	cross, err := f.store.ListProjectMemoryItems(f.ctx, projA, outB.Freshness.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cross) != 0 {
		t.Fatalf("project A read %d items filed under project B's repository", len(cross))
	}
	for _, it := range mustItems(t, f, projA, outA.Freshness.RepoID) {
		if strings.Contains(it.Summary+it.Content, "MARKER-BRAVO") {
			t.Fatalf("project A stores a fact with B's content: %s", it.Key.Key)
		}
	}
}

func mustItems(t *testing.T, f twinFixture, project domain.ProjectID, repoID string) []domain.ProjectMemoryItem {
	t.Helper()
	items, err := f.store.ListProjectMemoryItems(f.ctx, project, repoID)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

// A rename and a delete in A move A's memory and graph, and leave B's exactly
// as it was.
func TestChangesInOneProjectNeverTouchTheOther(t *testing.T) {
	f := newTwinFixture(t)
	_, beforeB := f.render(t, projB, f.rootB, projectmemory.RoleWorker)
	itemsB := len(mustItems(t, f, projB, beforeB.Freshness.RepoID))
	_, _ = f.render(t, projA, f.rootA, projectmemory.RoleWorker)

	gitRun(t, f.rootA, "mv", "internal/auth/login.go", "internal/auth/signin.go")
	if err := os.Remove(filepath.Join(f.rootA, "docs", "architecture.md")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, f.rootA, "add", "-A")
	gitRun(t, f.rootA, "commit", "-q", "-m", "rename and delete")

	renderedA, outA := f.render(t, projA, f.rootA, projectmemory.RoleWorker)
	if strings.Contains(renderedA, "docs/architecture.md") {
		t.Fatalf("A's pack still serves a deleted file:\n%s", renderedA)
	}
	if outA.Freshness.Kind == "" {
		t.Fatal("A did not sync after its commit moved")
	}
	for _, it := range mustItems(t, f, projA, outA.Freshness.RepoID) {
		if it.State == domain.MemoryStateValid {
			for _, p := range it.SourcePaths {
				if p == "docs/architecture.md" || p == "internal/auth/login.go" {
					t.Fatalf("A serves a valid fact sourced from a path that no longer exists: %s (%v)", it.Key.Key, it.SourcePaths)
				}
			}
		}
	}

	_, afterB := f.render(t, projB, f.rootB, projectmemory.RoleWorker)
	after := mustItems(t, f, projB, afterB.Freshness.RepoID)
	if len(after) != itemsB {
		t.Fatalf("project B's item count moved from %d to %d after a change in A", itemsB, len(after))
	}
	for _, it := range after {
		if it.State != domain.MemoryStateValid {
			t.Fatalf("project B's fact %s became %s after a change in A", it.Key.Key, it.State)
		}
	}
}

// A request that does not name a registered project fails closed: nothing is
// attached and nothing is written under an identity nobody registered.
func TestUnnamedOrUnknownProjectFailsClosed(t *testing.T) {
	f := newTwinFixture(t)
	for _, id := range []domain.ProjectID{"", "   ", "proj-ghost"} {
		out := f.prov.Provision(f.ctx, projectmemory.ProvisionRequest{
			ProjectID: id, RepoPath: f.rootA, Role: projectmemory.RoleWorker,
		})
		if out.Attached() {
			t.Fatalf("project id %q: memory attached for an unregistered/ambiguous project:\n%s", id, out.Render())
		}
		if strings.Contains(out.Render(), "MARKER-ALPHA") {
			t.Fatalf("project id %q: A's content served to an unregistered project", id)
		}
	}
}

// An archived project's memory is not served to anyone else, and the other
// project keeps working.
func TestArchivedProjectDoesNotLeakIntoTheOther(t *testing.T) {
	f := newTwinFixture(t)
	_, _ = f.render(t, projB, f.rootB, projectmemory.RoleWorker)
	if _, err := f.store.ArchiveProject(f.ctx, string(projB), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for _, role := range allRoles {
		rendered, out := f.render(t, projA, f.rootA, role)
		if !out.Attached() {
			t.Fatalf("A stopped working after B was archived: %s", out.Metrics.FallbackReason)
		}
		if strings.Contains(rendered, "MARKER-BRAVO") {
			t.Fatalf("A's %s pack carries an archived project's content", role)
		}
	}
}

// With the project scope the daemon always installs, a request that pairs
// project B with project A's checkout fails closed: nothing is synced under
// B's id and nothing of A's is served to B. (Without a scope the provisioner
// trusts its caller; every production caller resolves the path from the
// project record -- this closes the seam instead of relying on that.)
func TestScopedProvisionRefusesAMismatchedOrArchivedProject(t *testing.T) {
	f := newTwinFixture(t)
	scoped := projectmemory.NewProvisioner(f.svc, func() projectmemory.Config {
		c := projectmemory.DefaultConfig()
		c.Mode = projectmemory.ModeAssisted
		return c
	}()).WithProjectScope(f.store)

	mismatch := scoped.Provision(f.ctx, projectmemory.ProvisionRequest{
		ProjectID: projB, RepoPath: f.rootA, Role: projectmemory.RoleWorker, Keywords: []string{"login"},
	})
	if mismatch.Attached() || strings.Contains(mismatch.Render(), "MARKER-ALPHA") {
		t.Fatalf("project B was served project A's repository:\n%s", mismatch.Render())
	}
	if !strings.Contains(mismatch.Metrics.FallbackReason, "not one of project") {
		t.Fatalf("fallback reason = %q, want the scope refusal stated", mismatch.Metrics.FallbackReason)
	}
	if mismatch.Freshness.Kind != "" {
		t.Fatalf("a refused request still synced (%s)", mismatch.Freshness.Kind)
	}

	// The legitimate pairing still works through the same scoped provisioner.
	own := scoped.Provision(f.ctx, projectmemory.ProvisionRequest{ProjectID: projA, RepoPath: f.rootA, Role: projectmemory.RoleWorker})
	if !own.Attached() {
		t.Fatalf("the project's own repository was refused: %s", own.Metrics.FallbackReason)
	}
	// Nothing about A's repository was filed under B.
	if items := mustItems(t, f, projB, own.Freshness.RepoID); len(items) != 0 {
		t.Fatalf("%d facts about A's repository were filed under project B", len(items))
	}

	if _, err := f.store.ArchiveProject(f.ctx, string(projA), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	archived := scoped.Provision(f.ctx, projectmemory.ProvisionRequest{ProjectID: projA, RepoPath: f.rootA, Role: projectmemory.RoleWorker})
	if archived.Attached() || !strings.Contains(archived.Metrics.FallbackReason, "archived") {
		t.Fatalf("an archived project was served memory (reason %q)", archived.Metrics.FallbackReason)
	}
}
