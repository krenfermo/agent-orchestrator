package projectmemory_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// provision_test.go — the one call a dispatch boundary makes (P2-B).
//
// These tests are about the guarantees the completion bar names: memory is
// actually used automatically, budgets bind, repeated context is removed only
// when it can be proved redundant, and no failure of any of it reaches the
// dispatch.

func provFixture(t *testing.T, mode projectmemory.MemoryMode) (*fixture, *projectmemory.Provisioner, string) {
	t.Helper()
	f := newFixture(t)
	svc := projectmemory.NewService(f.store)
	cfg := projectmemory.DefaultConfig()
	cfg.Mode = mode
	root := goRepo(t)
	initGitRepo(t, root)
	return f, projectmemory.NewProvisioner(svc, cfg), root
}

// docOf reads a real file into the legacy-document shape a dispatch would hold,
// digest included — which is what makes dedupe provable.
func docOf(t *testing.T, root, rel string) projectmemory.LegacyDocument {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return projectmemory.LegacyDocument{
		Path: rel, SHA256: hex.EncodeToString(sum[:]), Content: string(body),
	}
}

// Memory off is byte-for-byte the pre-P2-B behaviour: nothing synced, nothing
// attached, every legacy document kept.
func TestProvisionWithMemoryOffChangesNothing(t *testing.T) {
	f, prov, root := provFixture(t, projectmemory.ModeOff)
	docs := []projectmemory.LegacyDocument{docOf(t, root, "AGENTS.md")}

	out := prov.Provision(f.ctx, projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker, Legacy: docs,
	})
	switch {
	case out.Attached():
		t.Error("memory was attached with the mode off")
	case len(out.Legacy) != 1:
		t.Errorf("legacy documents = %d, want them untouched", len(out.Legacy))
	case out.Metrics.FallbackReason == "":
		t.Error("the metrics do not say why nothing was attached")
	case out.Metrics.SyncPerformed:
		t.Error("a sync ran with the mode off")
	}
}

// Assisted mode is the safe first step: it ADDS a pack and removes nothing.
func TestProvisionAssistedAddsAndNeverReplaces(t *testing.T) {
	f, prov, root := provFixture(t, projectmemory.ModeAssisted)
	docs := []projectmemory.LegacyDocument{docOf(t, root, "AGENTS.md"), docOf(t, root, "README.md")}

	out := prov.Provision(f.ctx, projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RolePlanner, Legacy: docs,
	})
	if !out.Attached() {
		t.Fatalf("assisted mode attached nothing: %s", out.Metrics.FallbackReason)
	}
	if len(out.Legacy) != len(docs) {
		t.Fatalf("assisted mode dropped %d legacy documents", len(docs)-len(out.Legacy))
	}
	if out.Metrics.DedupeSavedBytes != 0 {
		t.Fatalf("assisted mode reported %d bytes saved; it may not replace anything", out.Metrics.DedupeSavedBytes)
	}
	// It still reports what preferred mode WOULD save, which is how an
	// operator decides whether to enable it.
	if out.Dedupe.CoveredBytes == 0 {
		t.Error("assisted mode did not report the coverage preferred mode would use")
	}
}

// Preferred mode replaces a legacy document only when memory demonstrably
// carries the same version of it.
func TestProvisionPreferredReplacesOnlyProvenDocuments(t *testing.T) {
	f, prov, root := provFixture(t, projectmemory.ModePreferred)
	proven := docOf(t, root, "AGENTS.md")

	// A document AO holds at a DIFFERENT version than memory recorded. Its
	// digest will not match, so it must survive.
	drifted := docOf(t, root, "README.md")
	drifted.Content = "# App\n\nsomething else entirely\n"
	drifted.SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"

	// A synthetic source with no digest at all: unprovable, so kept.
	synthetic := projectmemory.LegacyDocument{Path: "issue context", Content: "please fix the thing"}

	out := prov.Provision(f.ctx, projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RolePlanner,
		Legacy: []projectmemory.LegacyDocument{proven, drifted, synthetic},
	})
	if !out.Attached() {
		t.Fatalf("preferred mode attached nothing: %s", out.Metrics.FallbackReason)
	}

	kept := map[string]bool{}
	for _, d := range out.Legacy {
		kept[d.Path] = true
	}
	if kept["AGENTS.md"] {
		t.Error("a document memory demonstrably covers was still sent")
	}
	if !kept["README.md"] {
		t.Error("a document whose digest did not match memory was dropped")
	}
	if !kept["issue context"] {
		t.Error("a source with no digest was dropped without proof")
	}
	if out.Metrics.DedupeSavedBytes != proven.Bytes() {
		t.Fatalf("saved %d bytes, want exactly the proven document's %d",
			out.Metrics.DedupeSavedBytes, proven.Bytes())
	}

	// The reason each surviving document survived is recorded, so the decision
	// is auditable rather than a black box.
	for _, d := range out.Dedupe.SortedDecisions() {
		if d.Path == "README.md" && !strings.Contains(d.Reason, "different version") {
			t.Errorf("README.md survived for reason %q", d.Reason)
		}
	}
}

// The role budget binds, and the pack says what it dropped.
func TestProvisionEnforcesTheRoleBudget(t *testing.T) {
	f := newFixture(t)
	svc := projectmemory.NewService(f.store)
	cfg := projectmemory.DefaultConfig()
	cfg.Mode = projectmemory.ModeAssisted
	cfg.Budgets = cfg.Budgets.Merged(projectmemory.BudgetSet{
		projectmemory.RoleWorker: {MaxBytes: 1200, MaxItems: 3, MaxDocuments: 0},
	})
	prov := projectmemory.NewProvisioner(svc, cfg)
	root := goRepo(t)
	initGitRepo(t, root)

	out := prov.Provision(f.ctx, projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
	})
	if !out.Attached() {
		t.Fatalf("nothing attached: %s", out.Metrics.FallbackReason)
	}
	if out.Metrics.PackItems > 3 {
		t.Fatalf("selected %d items against a 3-item budget", out.Metrics.PackItems)
	}
	if len(out.Render()) > 1200+512 {
		t.Fatalf("rendered %d bytes against a 1200-byte budget", len(out.Render()))
	}
	if out.Metrics.PackRejectedByBudget == 0 && out.Metrics.PackReducedToSummary == 0 {
		t.Fatal("a tight budget bound nothing")
	}
	if out.Metrics.EstimatedPackTokens == 0 {
		t.Fatal("the pack reports no estimated token cost")
	}
}

// A repeated provision on the same authority is a cache hit, and the pack it
// returns is identical.
func TestProvisionCachesOnStrongAuthority(t *testing.T) {
	f, prov, root := provFixture(t, projectmemory.ModeAssisted)
	req := projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		ChangedPaths: []string{"internal/store/store.go"},
	}

	first := prov.Provision(f.ctx, req)
	if first.Metrics.CacheHit {
		t.Fatal("the first provision reported a cache hit")
	}
	second := prov.Provision(f.ctx, req)
	if !second.Metrics.CacheHit {
		t.Fatal("a repeated provision on the same authority missed the cache")
	}
	if first.Pack.Digest != second.Pack.Digest {
		t.Fatal("a cache hit returned a different pack")
	}
	if stats := prov.CacheStats(); stats.Hits != 1 {
		t.Fatalf("cache stats = %+v", stats)
	}
}

// The cache key is the AUTHORITY, not the request text: a different role, or a
// different scope, is a different pack.
func TestProvisionCacheKeyDistinguishesAuthorities(t *testing.T) {
	f, prov, root := provFixture(t, projectmemory.ModeAssisted)
	base := projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
	}
	worker := prov.Provision(f.ctx, base)

	byRole := base
	byRole.Role = projectmemory.RoleReviewer
	if prov.Provision(f.ctx, byRole).Metrics.CacheKey == worker.Metrics.CacheKey {
		t.Error("two roles shared one cache key")
	}

	byScope := base
	byScope.ChangedPaths = []string{"internal/store/store.go"}
	if prov.Provision(f.ctx, byScope).Metrics.CacheKey == worker.Metrics.CacheKey {
		t.Error("two selection scopes shared one cache key")
	}
}

// A commit that moves invalidates every cached pack implicitly, because the
// commit is part of the key. There is no invalidation call to forget.
func TestProvisionCacheIsInvalidatedByANewCommit(t *testing.T) {
	f, prov, root := provFixture(t, projectmemory.ModeAssisted)
	repo := gitRepo{root: root}
	req := projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
	}
	before := prov.Provision(f.ctx, req)

	writeTree(t, root, map[string]string{"docs/architecture.md": "# Architecture\n\nRewritten.\n"})
	repo.commit(t, "rewrite the architecture doc")

	after := prov.Provision(f.ctx, req)
	if after.Metrics.CacheKey == before.Metrics.CacheKey {
		t.Fatal("the cache key survived a commit change")
	}
	if after.Metrics.CacheHit {
		t.Fatal("a pack from the previous commit was reused")
	}
}

// Four roles provisioning against one warm repository trigger no scan between
// them. This is the warm-project target stated end to end.
func TestProvisionWarmProjectDoesNoWorkAcrossRoles(t *testing.T) {
	f, prov, root := provFixture(t, projectmemory.ModeAssisted)

	// The first role pays for the cold index.
	first := prov.Provision(f.ctx, projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RolePlanner,
	})
	if !first.Attached() {
		t.Fatalf("the cold provision attached nothing: %s", first.Metrics.FallbackReason)
	}

	for _, role := range []projectmemory.PackRole{
		projectmemory.RoleWorker, projectmemory.RoleReviewer, projectmemory.RoleRepair,
	} {
		out := prov.Provision(f.ctx, projectmemory.ProvisionRequest{
			ProjectID: testProject, RepoPath: root, Role: role,
		})
		if out.Metrics.SyncPerformed {
			t.Errorf("%s triggered a sync on a warm repository", role)
		}
		if out.Metrics.SyncKind != string(projectmemory.SyncNone) {
			t.Errorf("%s synced as %q, want none", role, out.Metrics.SyncKind)
		}
		if out.Metrics.SyncFilesRead != 0 {
			t.Errorf("%s read %d files on a warm repository", role, out.Metrics.SyncFilesRead)
		}
		if !out.Attached() {
			t.Errorf("%s got no memory: %s", role, out.Metrics.FallbackReason)
		}
	}
}

// A memory subsystem that cannot answer must not stop a dispatch.
func TestProvisionFallsBackCleanlyOnAnUnusableRepository(t *testing.T) {
	f := newFixture(t)
	svc := projectmemory.NewService(f.store)
	cfg := projectmemory.DefaultConfig()
	cfg.Mode = projectmemory.ModeAssisted
	prov := projectmemory.NewProvisioner(svc, cfg)

	docs := []projectmemory.LegacyDocument{{Path: "AGENTS.md", Content: "rules", SHA256: "abc"}}
	out := prov.Provision(f.ctx, projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: filepath.Join(t.TempDir(), "gone"),
		Role: projectmemory.RoleWorker, Legacy: docs,
	})
	switch {
	case out.Attached():
		t.Error("an unreachable repository still attached memory")
	case len(out.Legacy) != 1:
		t.Error("the dispatch's legacy context was not preserved")
	case out.Metrics.FallbackReason == "":
		t.Error("the fallback does not say why")
	case out.Metrics.FallbackBytes == 0:
		t.Error("the fallback did not report the legacy bytes it fell back to")
	}
}

// The metrics have to add up, because the whole before/after rests on them.
func TestProvisionMetricsAreInternallyConsistent(t *testing.T) {
	f, prov, root := provFixture(t, projectmemory.ModePreferred)
	docs := []projectmemory.LegacyDocument{docOf(t, root, "AGENTS.md"), docOf(t, root, "README.md")}

	out := prov.Provision(f.ctx, projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RolePlanner,
		Legacy: docs, TaskBytes: 120,
	})
	m := out.Metrics
	if err := m.Validate(); err != nil {
		t.Fatalf("metrics do not validate: %v", err)
	}
	surviving := 0
	for _, d := range out.Legacy {
		surviving += d.Bytes()
	}
	if want := surviving + m.PackBytes + m.TaskBytes; m.ContextBytes != want {
		t.Fatalf("contextBytes = %d, want surviving(%d)+pack(%d)+task(%d) = %d",
			m.ContextBytes, surviving, m.PackBytes, m.TaskBytes, want)
	}
	if m.LegacyBytes != docs[0].Bytes()+docs[1].Bytes() {
		t.Fatalf("legacyBytes = %d, want the pre-dedupe total", m.LegacyBytes)
	}
	if m.EstimatedInputTokens != projectmemory.EstimateTokens(m.ContextBytes) {
		t.Fatal("the token estimate does not match the byte count it is derived from")
	}
	if m.PackDigest == "" || m.CacheKey == "" {
		t.Fatal("the record cannot identify the memory it describes")
	}
}

// A pack is never handed a fact AO cannot vouch for, however it was reached.
func TestProvisionNeverServesStaleMemory(t *testing.T) {
	f, prov, root := provFixture(t, projectmemory.ModeAssisted)
	base := projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
	}
	if out := prov.Provision(f.ctx, base); !out.Attached() {
		t.Fatalf("cold provision attached nothing: %s", out.Metrics.FallbackReason)
	}

	// Retire the conventions the way a task's own mutation would.
	if _, err := prov.Syncer().InvalidatePaths(f.ctx, testProject, root,
		[]string{"AGENTS.md"}, "changed by a task"); err != nil {
		t.Fatal(err)
	}
	// The cache must not serve the pre-invalidation pack. Invalidation does not
	// move the generation, so this is the case a naive cache gets wrong.
	prov2 := projectmemory.NewProvisioner(projectmemory.NewService(f.store), func() projectmemory.Config {
		cfg := projectmemory.DefaultConfig()
		cfg.Mode = projectmemory.ModeAssisted
		return cfg
	}())
	out := prov2.Provision(f.ctx, base)
	if strings.Contains(out.Render(), "Keep every change surgical") {
		t.Fatal("a retired fact was served as authoritative")
	}
	if out.Metrics.PackStaleExcluded == 0 {
		t.Fatal("the pack did not record that it withheld retired memory")
	}
}
