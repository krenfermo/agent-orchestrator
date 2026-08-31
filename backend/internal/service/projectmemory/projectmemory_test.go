package projectmemory_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	pm "github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
	svc "github.com/aoagents/agent-orchestrator/backend/internal/service/projectmemory"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// projectmemory_test.go — the one policy this service owns.
//
// Everything else here is delegation. What is genuinely decided at this layer
// is which repository path a request may operate on, and getting that wrong
// would let one project's memory be filled with facts about a different
// codebase — so it is what these tests are about.

const testProject = domain.ProjectID("proj-1")

type fixture struct {
	store   *sqlite.Store
	service *svc.Service
	root    string
	child   string
	ctx     context.Context
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	root := t.TempDir()
	child := filepath.Join(root, "packages", "web")
	if err := os.MkdirAll(child, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# Root\n\nThe project.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "package.json"), []byte(`{"name":"web"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertWorkspaceProject(ctx, domain.ProjectRecord{
		ID: string(testProject), Path: root, RegisteredAt: time.Now().UTC(),
		Kind: domain.ProjectKindWorkspace,
	}, []domain.WorkspaceRepoRecord{{
		ProjectID: testProject, Name: "web", RelativePath: "packages/web",
		RegisteredAt: time.Now().UTC(),
	}}); err != nil {
		t.Fatal(err)
	}
	return &fixture{
		store: store, root: root, child: child, ctx: ctx,
		service: svc.New(pm.NewService(store), store),
	}
}

// An empty repo path is the single-repo case and must resolve to the project's
// own root, or `ao memory status` would be unusable without pasting a path.
func TestRebuildWithNoRepoPathUsesTheProjectRoot(t *testing.T) {
	f := newFixture(t)
	out, err := f.service.Rebuild(f.ctx, testProject, "", false)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(f.root)
	if err != nil {
		t.Fatal(err)
	}
	if out.RepoID != domain.ProjectMemoryRepoID(resolved) {
		t.Fatalf("rebuilt %s, want the project root %s", out.RepoID, resolved)
	}
	if out.ItemsWritten == 0 {
		t.Fatal("the rebuild wrote nothing")
	}
}

// A registered workspace child is a repository of this project, so it is
// allowed.
func TestRebuildAcceptsARegisteredWorkspaceRepo(t *testing.T) {
	f := newFixture(t)
	if _, err := f.service.Rebuild(f.ctx, testProject, f.child, false); err != nil {
		t.Fatalf("a registered workspace repo was refused: %v", err)
	}
	statuses, err := f.service.Status(f.ctx, testProject)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 {
		t.Fatalf("status reports %d repositories, want the one that was indexed", len(statuses))
	}
}

// The rule that matters: a checkout that is not part of this project must not
// be indexed under its id. Doing so would put facts about one codebase into
// another project's memory, and every later pack would carry them.
func TestRebuildRefusesAnUnrelatedCheckout(t *testing.T) {
	f := newFixture(t)
	stranger := t.TempDir()
	_, err := f.service.Rebuild(f.ctx, testProject, stranger, false)
	if err == nil {
		t.Fatal("an unrelated checkout was accepted")
	}
	if !strings.Contains(err.Error(), "not a repository of project") {
		t.Fatalf("err = %v, want it to name the reason", err)
	}
	statuses, statusErr := f.service.Status(f.ctx, testProject)
	if statusErr != nil {
		t.Fatal(statusErr)
	}
	if len(statuses) != 0 {
		t.Fatalf("the refused checkout still registered %d repositories", len(statuses))
	}
}

func TestRequestsAgainstAnUnregisteredProjectAreRefused(t *testing.T) {
	f := newFixture(t)
	if _, err := f.service.Rebuild(f.ctx, domain.ProjectID("ghost"), "", false); err == nil {
		t.Fatal("a rebuild ran for a project that is not registered")
	}
}

// With no paths, an invalidate runs drift detection and applies what it finds.
// That is the "something moved and I cannot say what" repair, and it must
// demote only what actually drifted.
func TestInvalidateWithoutPathsRunsDriftDetection(t *testing.T) {
	f := newFixture(t)
	if _, err := f.service.Rebuild(f.ctx, testProject, "", false); err != nil {
		t.Fatal(err)
	}

	clean, err := f.service.Invalidate(f.ctx, testProject, "", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if clean.DriftFound != 0 || clean.ItemsInvalidated != 0 {
		t.Fatalf("drift reported on an unchanged repository: %+v", clean)
	}
	if clean.DriftChecked == 0 {
		t.Fatal("the check evaluated nothing at all")
	}

	if err := os.WriteFile(filepath.Join(f.root, "README.md"),
		[]byte("# Root\n\nSomething else entirely.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	drifted, err := f.service.Invalidate(f.ctx, testProject, "", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if drifted.DriftFound == 0 || drifted.ItemsInvalidated == 0 {
		t.Fatalf("an edit made outside AO was not detected: %+v", drifted)
	}
}

// With explicit paths, an invalidate retires exactly those paths' facts and
// does not run a drift check.
func TestInvalidateWithPathsRetiresOnlyThose(t *testing.T) {
	f := newFixture(t)
	if _, err := f.service.Rebuild(f.ctx, testProject, "", false); err != nil {
		t.Fatal(err)
	}
	out, err := f.service.Invalidate(f.ctx, testProject, "", []string{"README.md"}, "operator test")
	if err != nil {
		t.Fatal(err)
	}
	if out.ItemsInvalidated == 0 {
		t.Fatal("naming a path retired nothing")
	}
	if out.DriftChecked != 0 {
		t.Fatalf("an explicit path invalidation also ran a drift check (%d)", out.DriftChecked)
	}

	stale, err := f.service.Inspect(f.ctx, controllers.ProjectMemoryInspectQuery{
		ProjectID: testProject, State: domain.MemoryStateStale,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stale.Total == 0 {
		t.Fatal("nothing is readable as stale afterwards")
	}
	for _, item := range stale.Items {
		if item.StateReason != "operator test" {
			t.Fatalf("stale reason = %q, want the operator's own words", item.StateReason)
		}
	}
}

// Inspect shows the facts AO can no longer vouch for. That is the whole point
// of an inspect, and the difference between it and a context pack.
func TestInspectShowsNonAuthoritativeFacts(t *testing.T) {
	f := newFixture(t)
	if _, err := f.service.Rebuild(f.ctx, testProject, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Invalidate(f.ctx, testProject, "", []string{"README.md"}, "test"); err != nil {
		t.Fatal(err)
	}
	all, err := f.service.Inspect(f.ctx, controllers.ProjectMemoryInspectQuery{ProjectID: testProject})
	if err != nil {
		t.Fatal(err)
	}
	sawStale := false
	for _, item := range all.Items {
		if item.State == domain.MemoryStateStale {
			sawStale = true
		}
	}
	if !sawStale {
		t.Fatal("an unfiltered inspect hid the stale facts")
	}
}

// The service is reachable through the controller's own interface. The
// satisfaction itself is a compile-time assertion in the package under test;
// what this adds is that a call actually routes through it, so a method that
// compiles but panics on the interface path is caught here rather than at a 501.
func TestServiceIsUsableThroughTheControllerContract(t *testing.T) {
	f := newFixture(t)
	var iface controllers.ProjectMemoryService = f.service
	if _, err := iface.Status(f.ctx, testProject); err != nil {
		t.Fatalf("status through the interface: %v", err)
	}
	if _, err := iface.Inspect(f.ctx, controllers.ProjectMemoryInspectQuery{ProjectID: testProject}); err != nil {
		t.Fatalf("inspect through the interface: %v", err)
	}
}
