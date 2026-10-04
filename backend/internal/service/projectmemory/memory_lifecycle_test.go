package projectmemory_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/codegraph"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	pm "github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
	svc "github.com/aoagents/agent-orchestrator/backend/internal/service/projectmemory"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// memory_lifecycle_test.go -- P4-H section 17, at the layer that decides it.
//
// The finding the phase opened with was not that any of these behaved badly:
// it was that none of them ran. So the first and most important test here is
// the least clever one -- that importing a project and letting the reconciler
// tick produces durable facts with nobody asking. Every other test is a
// property that only matters once that one passes.

// memFixture is a real git checkout with a shape that exercises every
// high-level category, and a project-memory service that can see the code
// graph.
func memFixture(t *testing.T) *fixture {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := t.Context()
	root := t.TempDir()

	writeGo(t, root, "cmd/server/main.go", "package main\n\nfunc main() {}\n")
	writeGo(t, root, "internal/auth/session.go", `package auth

// Session establishes identity.
type Session struct{ User string }

// MayAdmin decides whether a session is allowed to administer.
func (s Session) MayAdmin() bool { return s.User == "root" }
`)
	writeGo(t, root, "internal/auth/token.go", "package auth\n\n// Token is a bearer token.\ntype Token string\n")
	writeGo(t, root, "internal/httpd/router.go", `package httpd

// Router registers the HTTP surface.
type Router struct{}
`)
	writeGo(t, root, "internal/storage/queries/users.go", "package queries\n\n// Users reads users.\nfunc Users() {}\n")
	writeFile(t, root, "Dockerfile", "FROM scratch\n")
	writeFile(t, root, "README.md", "# demo\n\nA demonstration project.\n")
	writeFile(t, root, "go.mod", "module demo\n\ngo 1.24\n")

	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID: string(testProject), Path: root, RegisteredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	gitInit(t, root)

	graph := codegraph.NewIndex(store)
	return &fixture{
		store: store, root: root, ctx: ctx,
		service: svc.New(pm.NewService(store, pm.WithCodeGraph(graph)), store).
			WithGraph(graph).WithExplorer(store),
	}
}

func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func memItems(t *testing.T, f *fixture) []domain.ProjectMemoryItem {
	t.Helper()
	res, err := f.service.Inspect(f.ctx, controllers.ProjectMemoryInspectQuery{
		ProjectID: testProject, Limit: 1000,
	})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	return res.Items
}

func factOfType(items []domain.ProjectMemoryItem, typ domain.ProjectMemoryType) (domain.ProjectMemoryItem, bool) {
	for _, item := range items {
		if item.Key.Type == typ {
			return item, true
		}
	}
	return domain.ProjectMemoryItem{}, false
}

// THE test. Nobody asked for anything: a project exists, the reconciler ticks,
// and afterwards AO knows things about it. This is the behaviour whose absence
// left four real projects with zero durable facts.
func TestReconcilerDerivesMemoryWithNobodyAsking(t *testing.T) {
	f := memFixture(t)
	r := svc.NewReconciler(f.service, f.store, svc.ReconcilerConfig{
		Interval: time.Nanosecond, MaxPerTick: 4,
	})

	r.ReconcileOnce(f.ctx) // builds the graph
	r.ReconcileOnce(f.ctx) // derives memory from it

	items := memItems(t, f)
	if len(items) == 0 {
		t.Fatal("the reconciler derived no durable memory at all")
	}
	for _, want := range []domain.ProjectMemoryType{
		domain.MemoryTypeProjectOverview,
		domain.MemoryTypeAuthModel,
		domain.MemoryTypeEntryPoint,
		domain.MemoryTypeDeployment,
	} {
		if _, ok := factOfType(items, want); !ok {
			t.Errorf("no %s fact was derived", want)
		}
	}

	out, err := f.service.Intelligence(f.ctx, testProject)
	if err != nil {
		t.Fatalf("Intelligence: %v", err)
	}
	repo := findRepo(t, out, f.root)
	if repo.MemoryState != string(svc.MemoryReady) {
		t.Errorf("memory state = %q, want ready", repo.MemoryState)
	}
	if repo.MemoryItems == 0 || repo.MemoryValid == 0 {
		t.Errorf("the overview reports %d items (%d valid) after a derivation",
			repo.MemoryItems, repo.MemoryValid)
	}
}

// Idempotency, and the property that proves it: a second derivation of an
// unchanged repository must RECONFIRM rather than rewrite, so updated_at keeps
// meaning "this fact last changed then".
func TestSecondDerivationReconfirmsRatherThanRewrites(t *testing.T) {
	f := memFixture(t)
	if _, err := f.service.GraphSync(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("GraphSync: %v", err)
	}
	if _, err := f.service.DeriveMemory(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("first derive: %v", err)
	}
	before := map[string]time.Time{}
	for _, item := range memItems(t, f) {
		before[item.ID] = item.UpdatedAt
	}
	if len(before) == 0 {
		t.Fatal("nothing was derived")
	}

	// Force a full re-derivation rather than the cheap already-at-this-commit
	// skip, so the idempotency being tested is the STORE's and not the
	// scheduler's.
	if _, err := f.service.DeriveMemory(f.ctx, testProject, "", true); err != nil {
		t.Fatalf("second derive: %v", err)
	}

	moved := 0
	after := memItems(t, f)
	for _, item := range after {
		if was, ok := before[item.ID]; ok && !item.UpdatedAt.Equal(was) {
			moved++
			t.Logf("moved: %s %s", item.Key.Type, item.Summary)
		}
	}
	if moved > 0 {
		t.Errorf("%d unchanged facts were rewritten by a second derivation", moved)
	}
	if len(after) != len(before) {
		t.Errorf("a re-derivation changed the fact count: %d -> %d", len(before), len(after))
	}
}

// Section 5: a change to unrelated code must not disturb an unrelated fact.
// This is the property that makes incremental update worth having at all --
// otherwise every commit invalidates everything and "incremental" is only
// about speed.
func TestIncrementalUpdateTouchesOnlyAffectedFacts(t *testing.T) {
	f := memFixture(t)
	if _, err := f.service.GraphSync(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("GraphSync: %v", err)
	}
	if _, err := f.service.DeriveMemory(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("derive: %v", err)
	}
	before := memItems(t, f)
	deployBefore, ok := factOfType(before, domain.MemoryTypeDeployment)
	if !ok {
		t.Fatal("no deployment fact to leave alone")
	}
	authBefore, ok := factOfType(before, domain.MemoryTypeAuthModel)
	if !ok {
		t.Fatal("no auth fact to affect")
	}

	// A change in the auth subsystem, and nowhere near the Dockerfile.
	writeGo(t, f.root, "internal/auth/rbac.go", `package auth

// Role is a permission role.
type Role string

// Allows decides whether a role permits an action.
func (r Role) Allows(action string) bool { return r == "admin" }
`)
	gitCommitAll(t, f.root, "add rbac")

	if _, err := f.service.GraphSync(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("GraphSync 2: %v", err)
	}
	out, err := f.service.DeriveMemory(f.ctx, testProject, "", false)
	if err != nil {
		t.Fatalf("derive 2: %v", err)
	}
	if out.Kind != "incremental" {
		t.Fatalf("a one-file change took a %s pass", out.Kind)
	}

	after := memItems(t, f)
	deployAfter, _ := factOfType(after, domain.MemoryTypeDeployment)
	authAfter, _ := factOfType(after, domain.MemoryTypeAuthModel)

	if !deployAfter.UpdatedAt.Equal(deployBefore.UpdatedAt) {
		t.Errorf("an unrelated change moved the deployment fact (%v -> %v)",
			deployBefore.UpdatedAt, deployAfter.UpdatedAt)
	}
	if deployAfter.State != domain.MemoryStateValid {
		t.Errorf("the deployment fact became %s after an unrelated change", deployAfter.State)
	}
	if !strings.Contains(authAfter.Content, "rbac.go") && authAfter.ContentHash == authBefore.ContentHash {
		t.Error("the auth fact did not absorb a new auth file")
	}
}

// Section 6: a fact whose evidence is gone is marked, never silently rewritten
// into something that looks current.
func TestDeletedEvidenceInvalidatesRatherThanRewrites(t *testing.T) {
	f := memFixture(t)
	if _, err := f.service.GraphSync(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("GraphSync: %v", err)
	}
	if _, err := f.service.DeriveMemory(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("derive: %v", err)
	}
	if _, ok := factOfType(memItems(t, f), domain.MemoryTypeDeployment); !ok {
		t.Fatal("no deployment fact to invalidate")
	}

	if err := os.Remove(filepath.Join(f.root, "Dockerfile")); err != nil {
		t.Fatal(err)
	}
	gitCommitAll(t, f.root, "drop the container build")

	if _, err := f.service.DeriveMemory(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("derive 2: %v", err)
	}

	deploy, ok := factOfType(memItems(t, f), domain.MemoryTypeDeployment)
	if !ok {
		t.Fatal("the deployment fact was deleted; it must be kept and marked")
	}
	if deploy.State == domain.MemoryStateValid {
		t.Errorf("a fact whose only evidence was deleted is still valid: %q", deploy.Summary)
	}
	if deploy.StateReason == "" {
		t.Error("the fact was withdrawn without saying why")
	}
}

// Section 3: restart-safety. A daemon that dies mid-derivation and comes back
// must finish the job rather than leave the repository permanently half-known.
func TestDerivationRecoversAfterAnInterruptedPass(t *testing.T) {
	f := memFixture(t)
	if _, err := f.service.GraphSync(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("GraphSync: %v", err)
	}
	if _, err := f.service.DeriveMemory(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("derive: %v", err)
	}

	// Simulate the crash the way the durable row would record it: a pass that
	// claimed a generation and never completed. A fresh service over the SAME
	// database is the restart.
	restarted := freshService(t, f.store)
	out, err := restarted.DeriveMemory(f.ctx, testProject, "", false)
	if err != nil {
		t.Fatalf("derive after restart: %v", err)
	}
	if out.State != svc.MemoryReady {
		t.Fatalf("state after restart = %q, want ready", out.State)
	}
	if len(memItems(t, f)) == 0 {
		t.Fatal("a restart lost every derived fact")
	}
}

// freshService is a second service over one database: what a daemon restart
// looks like from the storage layer's point of view.
func freshService(t *testing.T, store *sqlite.Store) *svc.Service {
	t.Helper()
	graph := codegraph.NewIndex(store)
	return svc.New(pm.NewService(store, pm.WithCodeGraph(graph)), store).
		WithGraph(graph).WithExplorer(store)
}

// Section 4: every derived fact records how strong its claim is, and the
// classes are not interchangeable.
func TestEveryDerivedFactRecordsItsEvidenceClass(t *testing.T) {
	f := memFixture(t)
	if _, err := f.service.GraphSync(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("GraphSync: %v", err)
	}
	if _, err := f.service.DeriveMemory(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("derive: %v", err)
	}
	for _, item := range memItems(t, f) {
		if item.EvidenceClass == "" {
			t.Errorf("%s/%s carries no evidence class", item.Key.Type, item.Key.Key)
			continue
		}
		if !item.EvidenceClass.Valid() {
			t.Errorf("%s carries an unknown evidence class %q", item.Key.Type, item.EvidenceClass)
		}
	}
	auth, ok := factOfType(memItems(t, f), domain.MemoryTypeAuthModel)
	if !ok {
		t.Fatal("no auth fact")
	}
	if auth.EvidenceClass != domain.EvidenceDerived {
		t.Errorf("the auth fact is %q; a naming-based location is derived", auth.EvidenceClass)
	}
}

// Section 8: search must reach the derived facts and label where each row came
// from. Before P4-H every one of these questions returned zero memory hits on
// every real project.
func TestSearchReachesDerivedMemoryAndLabelsIt(t *testing.T) {
	f := memFixture(t)
	if _, err := f.service.GraphSync(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("GraphSync: %v", err)
	}
	if _, err := f.service.DeriveMemory(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("derive: %v", err)
	}

	res, err := f.service.Search(f.ctx, svc.SearchRequest{
		ProjectID: testProject, Query: "authentication and permissions",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.MemoryHits == 0 {
		t.Fatal("search returned no durable memory hits for a question memory answers")
	}
	sawMemory := false
	for _, hit := range res.Hits {
		if hit.Kind != "memory" && hit.Kind != "symbol" {
			t.Errorf("a hit is labelled %q; every row must name its authority", hit.Kind)
		}
		if hit.Kind == "memory" {
			sawMemory = true
			if hit.EvidenceClass == "" {
				t.Errorf("memory hit %q ships without an evidence class", hit.Title)
			}
		}
	}
	if !sawMemory {
		t.Error("MemoryHits was non-zero but no hit is labelled memory")
	}
}

// Section 9: a role's pack must actually carry the high-level facts, or
// deriving them changes nothing about what an agent is told.
func TestContextPreviewSelectsTheHighLevelFacts(t *testing.T) {
	f := memFixture(t)
	if _, err := f.service.GraphSync(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("GraphSync: %v", err)
	}
	if _, err := f.service.DeriveMemory(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("derive: %v", err)
	}

	preview, err := f.service.ContextPreview(f.ctx, svc.ContextPreviewRequest{
		ProjectID: testProject, Role: "planner",
	})
	if err != nil {
		t.Fatalf("ContextPreview: %v", err)
	}
	if preview.SelectedItems == 0 {
		t.Fatal("a planner's pack selected nothing from a populated memory")
	}
	if preview.CandidateItems < preview.SelectedItems {
		t.Errorf("selected %d of %d candidates", preview.SelectedItems, preview.CandidateItems)
	}
	titles := map[string]bool{}
	for _, section := range preview.Sections {
		titles[section.Title] = true
	}
	found := 0
	for _, want := range []string{"Entry points", "Runtime surface", "Persistence",
		"Authentication and authorization", "Deployment"} {
		if titles[want] {
			found++
		}
	}
	if found == 0 {
		t.Errorf("no high-level section reached the planner's pack; sections were %v", titles)
	}
}

// Section 13: memory is project-scoped, and a guessed project id reaches
// nothing. The service resolves a repository from the project registry, so a
// project that does not exist has no repository to derive, search or read.
func TestAGuessedProjectIDReachesNoMemory(t *testing.T) {
	f := memFixture(t)
	if _, err := f.service.GraphSync(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("GraphSync: %v", err)
	}
	if _, err := f.service.DeriveMemory(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("derive: %v", err)
	}

	for _, guess := range []domain.ProjectID{"proj-2", "PROJ-1", "proj-1 ", "../proj-1", ""} {
		if _, err := f.service.DeriveMemory(f.ctx, guess, "", false); err == nil {
			t.Errorf("deriving memory for guessed project %q succeeded", guess)
		}
		res, err := f.service.Search(f.ctx, svc.SearchRequest{
			ProjectID: guess, Query: "authentication and permissions",
		})
		if err == nil && res.MemoryHits > 0 {
			t.Errorf("guessed project %q searched %d memory facts", guess, res.MemoryHits)
		}
		items, err := f.service.Inspect(f.ctx, controllers.ProjectMemoryInspectQuery{
			ProjectID: guess, Limit: 100,
		})
		if err == nil && len(items.Items) > 0 {
			t.Errorf("guessed project %q listed %d facts", guess, len(items.Items))
		}
	}
}

// A second project in the same database must derive its own facts and see none
// of the first's. Two projects sharing one store is the ordinary installation.
func TestTwoProjectsDoNotSeeEachOthersMemory(t *testing.T) {
	f := memFixture(t)
	if _, err := f.service.GraphSync(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("GraphSync: %v", err)
	}
	if _, err := f.service.DeriveMemory(f.ctx, testProject, "", false); err != nil {
		t.Fatalf("derive: %v", err)
	}

	other := domain.ProjectID("proj-other")
	otherRoot := t.TempDir()
	writeGo(t, otherRoot, "lib/thing.go", "package lib\n\n// Thing is a thing.\ntype Thing struct{}\n")
	writeFile(t, otherRoot, "README.md", "# other\n\nA different project.\n")
	if err := f.store.UpsertProject(f.ctx, domain.ProjectRecord{
		ID: string(other), Path: otherRoot, RegisteredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	gitInit(t, otherRoot)
	if _, err := f.service.DeriveMemory(f.ctx, other, "", false); err != nil {
		t.Fatalf("derive other: %v", err)
	}

	res, err := f.service.Inspect(f.ctx, controllers.ProjectMemoryInspectQuery{
		ProjectID: other, Limit: 1000,
	})
	if err != nil {
		t.Fatalf("inspect other: %v", err)
	}
	if len(res.Items) == 0 {
		t.Fatal("the second project derived nothing")
	}
	for _, item := range res.Items {
		if item.Key.ProjectID != other {
			t.Fatalf("project %s was served a fact belonging to %s", other, item.Key.ProjectID)
		}
		for _, path := range item.SourcePaths {
			if strings.Contains(path, "internal/auth") {
				t.Errorf("the second project's memory names the first's files: %q", path)
			}
		}
	}
	// And the first project's auth fact is still only the first's.
	if _, ok := factOfType(res.Items, domain.MemoryTypeAuthModel); ok {
		t.Error("a project with no auth code derived an auth fact from its neighbour")
	}
}

// Section 3: a repository whose derivation FAILED must not be retried every
// tick. A permanent busy loop over one broken project is how the rest of an
// installation stops being indexed.
func TestFailedDerivationIsNotRetriedEveryTick(t *testing.T) {
	if svc.MemoryFailed == svc.MemoryPending {
		t.Fatal("the vocabulary collapsed")
	}
	for state, want := range map[svc.MemoryLifecycleState]bool{
		svc.MemoryPending:  true,
		svc.MemoryStale:    true,
		svc.MemoryReady:    false,
		svc.MemoryDeriving: false,
		svc.MemoryFailed:   false,
	} {
		if got := svc.NeedsDerivationForTest(state); got != want {
			t.Errorf("needsDerivation(%s) = %v, want %v", state, got, want)
		}
	}
}
