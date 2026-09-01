package projectmemory_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// pack_test.go — the four properties a MemoryContextPack must have, one test
// group each: bounded, deterministic, fail-closed, role-specific.

func packService(t *testing.T) (*fixture, *projectmemory.Service, string, projectmemory.IndexOutcome) {
	t.Helper()
	f := newFixture(t)
	svc := projectmemory.NewService(f.store)
	root := goRepo(t)
	out, err := svc.Index(f.ctx, projectmemory.IndexRequest{
		ProjectID: testProject, RepoPath: root, Commit: "c1", Branch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	return f, svc, root, out
}

func TestContextPackIsRoleSpecific(t *testing.T) {
	f, svc, root, _ := packService(t)

	seen := map[projectmemory.PackRole]map[domain.ProjectMemoryType]bool{}
	for _, role := range []projectmemory.PackRole{
		projectmemory.RolePlanner, projectmemory.RoleWorker,
		projectmemory.RoleReviewer, projectmemory.RoleRepair,
	} {
		pack := svc.Context(f.ctx, projectmemory.PackRequest{
			ProjectID: testProject, RepoPath: root, Role: role,
		})
		if pack.Empty() {
			t.Fatalf("%s pack is empty: %s", role, pack.Stats.FallbackReason)
		}
		types := map[domain.ProjectMemoryType]bool{}
		for _, s := range pack.Sections {
			types[s.Type] = true
		}
		seen[role] = types
		if pack.Stats.IndexedCommit != "c1" {
			t.Errorf("%s pack does not state its provenance commit", role)
		}
	}

	// A Planner is told what the system is; a Worker is not handed the
	// project overview in place of the conventions it has to obey.
	if !seen[projectmemory.RolePlanner][domain.MemoryTypeProjectOverview] {
		t.Error("the planner pack carries no project overview")
	}
	if seen[projectmemory.RoleWorker][domain.MemoryTypeProjectOverview] {
		t.Error("the worker pack carries the project overview, which is not in its section order")
	}
	if !seen[projectmemory.RoleWorker][domain.MemoryTypeConvention] {
		t.Error("the worker pack carries no conventions")
	}
	if !seen[projectmemory.RoleReviewer][domain.MemoryTypeConvention] {
		t.Error("the reviewer pack carries no conventions")
	}
}

func TestContextPackIsDeterministic(t *testing.T) {
	f, svc, root, _ := packService(t)
	req := projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		ChangedPaths: []string{"internal/store/store.go"},
	}
	first := svc.Context(f.ctx, req)
	second := svc.Context(f.ctx, req)
	if first.Digest != second.Digest {
		t.Fatalf("two packs from an unchanged store differ: %s vs %s", first.Digest, second.Digest)
	}
	if first.Render() != second.Render() {
		t.Fatal("two packs from an unchanged store rendered differently")
	}
	if first.Digest == "" {
		t.Fatal("the pack carries no digest")
	}
}

func TestContextPackEnforcesItsBudget(t *testing.T) {
	f, svc, root, _ := packService(t)

	tight := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RolePlanner,
		Budget: projectmemory.PackBudget{MaxBytes: 900, MaxItems: 3},
	})
	if tight.Stats.SelectedItems > 3 {
		t.Fatalf("selected %d items against a 3-item budget", tight.Stats.SelectedItems)
	}
	if len(tight.Render()) > 2500 {
		// The rendered pack includes a fixed header; the budget bounds the
		// facts, and the header is small and constant.
		t.Fatalf("rendered pack is %d bytes against a 900-byte fact budget", len(tight.Render()))
	}
	if tight.Stats.DroppedItems == 0 && tight.Stats.DroppedToSummary == 0 {
		t.Fatal("a tight budget dropped nothing, so it was not enforced")
	}
	if !strings.Contains(tight.Render(), "budget") {
		t.Error("the pack does not tell the agent that facts were omitted")
	}
}

func TestContextPackRanksTheChangedAreaFirst(t *testing.T) {
	f, svc, root, _ := packService(t)

	pack := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleReviewer,
		ChangedPaths: []string{"internal/store/store.go"},
	})
	found := false
	for _, section := range pack.Sections {
		for _, sel := range section.Items {
			if sel.Item.Key.Key == "internal/store" {
				found = true
				if sel.Reason != "covers a path this work changes" {
					t.Errorf("the changed module was selected for %q", sel.Reason)
				}
			}
		}
	}
	if !found {
		t.Fatal("the module containing the changed file did not make it into the pack")
	}
}

func TestContextPackExcludesStaleMemory(t *testing.T) {
	f, svc, root, out := packService(t)

	// Take the conventions out of circulation the way a source change would.
	if _, err := svc.Invalidate(f.ctx, testProject, root, []string{"AGENTS.md"}, "test"); err != nil {
		t.Fatal(err)
	}
	pack := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
	})
	for _, section := range pack.Sections {
		for _, sel := range section.Items {
			if strings.HasPrefix(sel.Item.Key.Key, "AGENTS.md") {
				t.Fatalf("a stale fact was served as authoritative: %s", sel.Item.Key)
			}
		}
	}
	if pack.Stats.StaleExcluded == 0 {
		t.Fatal("the pack did not record that it withheld stale memory")
	}
	_ = out
}

func TestContextPackFailsClosedWithoutAnIndex(t *testing.T) {
	f := newFixture(t)
	svc := projectmemory.NewService(f.store)
	root := goRepo(t)

	pack := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
	})
	if !pack.Empty() {
		t.Fatal("a repository with no index produced a non-empty pack")
	}
	if pack.Stats.FallbackReason == "" {
		t.Fatal("the empty pack does not say why it is empty")
	}
	if !strings.Contains(pack.Render(), "No project memory is attached") {
		t.Fatalf("the rendered empty pack does not say so:\n%s", pack.Render())
	}
}

func TestContextPackFailsClosedAfterAFailedIndex(t *testing.T) {
	f, svc, root, out := packService(t)

	claimed, ok, err := f.store.ClaimProjectMemoryIndexPass(f.ctx, testProject, out.RepoID, "c2", "main", f.now())
	if err != nil || !ok {
		t.Fatal(err)
	}
	if _, err := f.store.FailProjectMemoryIndexPass(f.ctx, testProject, out.RepoID, claimed.Generation, "disk full", f.now()); err != nil {
		t.Fatal(err)
	}

	pack := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
	})
	if !pack.Empty() {
		t.Fatal("memory from a failed index was still served")
	}
	if !strings.Contains(pack.Stats.FallbackReason, "disk full") {
		t.Fatalf("fallback reason = %q, want it to name the failure", pack.Stats.FallbackReason)
	}
}

func TestContextPackKeepsTaskMemoryToItsOwnTask(t *testing.T) {
	f, svc, root, _ := packService(t)

	if err := svc.RecordTaskOutcome(f.ctx, projectmemory.TaskOutcome{
		ProjectID: testProject, RepoPath: root, TaskRef: "t1",
		Title: "add the queue", WhatChanged: "added a durable queue",
		Why: "the scheduler needed one", FilesChanged: []string{"internal/store/store.go"},
		Risks: []projectmemory.TaskRisk{{Statement: "the queue is unbounded under load"}},
	}); err != nil {
		t.Fatal(err)
	}

	mine := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleRepair, TaskRef: "t1",
	})
	if !strings.Contains(mine.Render(), "add the queue") {
		t.Fatalf("a task cannot see its own memory:\n%s", mine.Render())
	}

	theirs := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleRepair, TaskRef: "t2",
	})
	if strings.Contains(theirs.Render(), "add the queue") {
		t.Fatal("one task's unintegrated memory leaked into another task's pack")
	}
	none := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
	})
	if strings.Contains(none.Render(), "add the queue") {
		t.Fatal("unintegrated task memory leaked into a pack with no task ref at all")
	}
}

func TestPlannerPackSpansEveryRepositoryOfTheProject(t *testing.T) {
	f := newFixture(t)
	svc := projectmemory.NewService(f.store)
	backend := goRepo(t)
	frontend := t.TempDir()
	writeTree(t, frontend, map[string]string{
		"package.json": `{"name":"web","scripts":{"build":"vite build"}}`,
		"README.md":    "# Web\n\nThe browser client.\n",
		"src/app.tsx":  "export const App = () => null\n",
	})
	for _, r := range []string{backend, frontend} {
		if _, err := svc.Index(f.ctx, projectmemory.IndexRequest{
			ProjectID: testProject, RepoPath: r, Commit: "c1",
		}); err != nil {
			t.Fatal(err)
		}
	}

	pack := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, Role: projectmemory.RolePlanner,
		Budget: projectmemory.PackBudget{MaxBytes: 64 * 1024, MaxItems: 100},
	})
	rendered := pack.Render()
	if !strings.Contains(rendered, "A small service that does one thing well") {
		t.Error("the planner pack is missing the backend repository")
	}
	if !strings.Contains(rendered, "The browser client") {
		t.Error("the planner pack is missing the frontend repository")
	}

	// A worker pack, by contrast, is scoped to the repository it works in.
	worker := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: frontend, Role: projectmemory.RoleWorker,
	})
	if strings.Contains(worker.Render(), "A small service that does one thing well") {
		t.Error("a worker pack for the frontend carries the backend's memory")
	}
}

func TestContextPackReportsMeasurableStats(t *testing.T) {
	f, svc, root, _ := packService(t)
	pack := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		ChangedPaths: []string{"internal/store/store.go"},
	})
	s := pack.Stats
	switch {
	case s.CandidateItems == 0:
		t.Error("no candidates were counted")
	case s.SelectedItems == 0:
		t.Error("no items were selected")
	case s.SelectedBytes == 0 || s.SelectedTokens == 0:
		t.Error("the pack did not measure its own size")
	case s.SelectedItems > s.CandidateItems:
		t.Error("more items were selected than were candidates")
	case len(s.SourcesReused) == 0:
		t.Error("the pack named no source paths behind its facts")
	}
}

func TestTaskMemoryIsCompactedAndNeverCarriesATranscript(t *testing.T) {
	f := newFixture(t)
	svc := projectmemory.NewService(f.store)
	root := goRepo(t)
	if _, err := svc.Index(f.ctx, projectmemory.IndexRequest{
		ProjectID: testProject, RepoPath: root, Commit: "c1",
	}); err != nil {
		t.Fatal(err)
	}

	huge := strings.Repeat("transcript line that should never reach memory\n", 5000)
	if err := svc.RecordTaskOutcome(f.ctx, projectmemory.TaskOutcome{
		ProjectID: testProject, RepoPath: root, TaskRef: "t1",
		Title: "big task", WhatChanged: huge, Why: huge,
		Integrated: true, Commit: "c1",
	}); err != nil {
		t.Fatal(err)
	}
	items, err := f.store.ListProjectMemoryItems(f.ctx, testProject, domain.ProjectMemoryRepoID(mustEval(t, root)))
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if len(it.Content) > domain.MaxProjectMemoryContent {
			t.Fatalf("item %s carries %d bytes, over the %d cap",
				it.Key, len(it.Content), domain.MaxProjectMemoryContent)
		}
		if len(it.Summary) > domain.MaxProjectMemorySummary {
			t.Fatalf("item %s has a %d-byte summary", it.Key, len(it.Summary))
		}
	}
}

func TestPromoteTaskMemoryMakesItCanonical(t *testing.T) {
	f, svc, root, _ := packService(t)

	if err := svc.RecordTaskOutcome(f.ctx, projectmemory.TaskOutcome{
		ProjectID: testProject, RepoPath: root, TaskRef: "t1",
		Title: "add the queue", WhatChanged: "added a durable queue",
		Decisions: []projectmemory.TaskDecision{{Statement: "the queue is a table, not a JSON blob"}},
	}); err != nil {
		t.Fatal(err)
	}
	// Before promotion, no other task sees the decision.
	before := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker, TaskRef: "t2",
	})
	if strings.Contains(before.Render(), "not a JSON blob") {
		t.Fatal("an unintegrated decision was visible to another task")
	}

	promoted, err := svc.PromoteTaskMemory(f.ctx, testProject, "t1", provenPromotion("c2"))
	if err != nil {
		t.Fatal(err)
	}
	if promoted == 0 {
		t.Fatal("promotion moved nothing")
	}
	after := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker, TaskRef: "t2",
	})
	if !strings.Contains(after.Render(), "not a JSON blob") {
		t.Fatalf("a promoted decision is still invisible:\n%s", after.Render())
	}
	if left, err := f.store.ListProjectMemoryItemsForTask(f.ctx, testProject, "t1"); err != nil {
		t.Fatal(err)
	} else if len(left) != 0 {
		t.Fatalf("%d task-local rows survived promotion", len(left))
	}
}

func TestDiscardTaskMemoryLeavesNoParallelMemory(t *testing.T) {
	f, svc, root, _ := packService(t)
	if err := svc.RecordTaskOutcome(f.ctx, projectmemory.TaskOutcome{
		ProjectID: testProject, RepoPath: root, TaskRef: "t1",
		Title: "abandoned", WhatChanged: "half a refactor",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DiscardTaskMemory(f.ctx, testProject, "t1"); err != nil {
		t.Fatal(err)
	}
	left, err := f.store.ListProjectMemoryItemsForTask(f.ctx, testProject, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("%d task-local rows survived the task", len(left))
	}
}

func TestVerifyDetectsDriftEditedOutsideAO(t *testing.T) {
	f, svc, root, _ := packService(t)

	// Edit a source file behind AO's back — no pass, no change set.
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"),
		[]byte("# AGENTS.md\n\nCompletely different rules now.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	dry, err := svc.Verify(f.ctx, testProject, root, "c1", false)
	if err != nil {
		t.Fatal(err)
	}
	if !dry.Drifted() {
		t.Fatal("drift detection missed a file edited outside AO")
	}
	for _, fnd := range dry.Findings {
		if fnd.Applied {
			t.Fatal("a dry run applied a demotion")
		}
	}
	status, _, err := f.store.GetProjectMemoryStatus(f.ctx, testProject, dry.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Counts.Stale != 0 {
		t.Fatal("a dry run changed stored state")
	}

	applied, err := svc.Verify(f.ctx, testProject, root, "c1", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.Findings) != len(dry.Findings) {
		t.Fatalf("apply found %d findings, dry run found %d", len(applied.Findings), len(dry.Findings))
	}
	for _, fnd := range applied.Findings {
		if !fnd.Applied {
			t.Fatalf("finding %s was not applied", fnd.ItemID)
		}
	}
	pack := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
	})
	if strings.Contains(pack.Render(), "Keep every change surgical") {
		t.Fatal("a fact whose source drifted is still being served")
	}
}

func TestVerifyDetectsADeletedSource(t *testing.T) {
	f, svc, root, _ := packService(t)
	if err := os.Remove(filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	report, err := svc.Verify(f.ctx, testProject, root, "c1", true)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Drifted() {
		t.Fatal("a deleted source did not register as drift")
	}
	for _, fnd := range report.Findings {
		if fnd.To != domain.MemoryStateInvalidated {
			t.Fatalf("a deleted source produced %s, want invalidated", fnd.To)
		}
	}
}

func TestVerifyOnAnUnchangedRepositoryFindsNothing(t *testing.T) {
	f, svc, root, _ := packService(t)
	report, err := svc.Verify(f.ctx, testProject, root, "c1", true)
	if err != nil {
		t.Fatal(err)
	}
	if report.Drifted() {
		t.Fatalf("drift reported on an unchanged repository: %+v", report.Findings)
	}
	if report.Checked == 0 {
		t.Fatal("the check evaluated nothing at all")
	}
	if report.Confirmed != report.Checked {
		t.Fatalf("checked %d, confirmed %d", report.Checked, report.Confirmed)
	}
}

func TestStatusAndInspectAreOperatorReadable(t *testing.T) {
	f, svc, root, out := packService(t)

	status, ok, err := svc.Status(f.ctx, testProject, root)
	if err != nil || !ok {
		t.Fatalf("status: ok=%v err=%v", ok, err)
	}
	if status.Index.Generation != out.Generation || status.Index.IndexedCommit != "c1" {
		t.Fatalf("status = %+v", status.Index)
	}
	if !status.Healthy() {
		t.Fatal("a freshly indexed repository is not reported healthy")
	}

	all, err := svc.StatusAll(f.ctx, testProject)
	if err != nil || len(all) != 1 {
		t.Fatalf("statusAll = %d entries, err=%v", len(all), err)
	}

	inspect, err := svc.Inspect(f.ctx, projectmemory.InspectRequest{
		ProjectID: testProject, RepoPath: root, Type: domain.MemoryTypeModule,
	})
	if err != nil {
		t.Fatal(err)
	}
	if inspect.Total == 0 {
		t.Fatal("inspect found no module facts")
	}
	for _, it := range inspect.Items {
		if it.Key.Type != domain.MemoryTypeModule {
			t.Fatalf("inspect --type module returned a %s", it.Key.Type)
		}
	}

	scoped, err := svc.Inspect(f.ctx, projectmemory.InspectRequest{
		ProjectID: testProject, RepoPath: root, PathPrefix: "internal/store",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range scoped.Items {
		if !strings.HasPrefix(it.Key.Key, "internal/store") && !hasPrefixInSources(it, "internal/store") {
			t.Fatalf("inspect --path internal/store returned %s", it.Key)
		}
	}
}

func hasPrefixInSources(it domain.ProjectMemoryItem, prefix string) bool {
	for _, p := range it.SourcePaths {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

func TestRebuildPurgeStartsFromNothing(t *testing.T) {
	f, svc, root, out := packService(t)

	if err := svc.RecordTaskOutcome(f.ctx, projectmemory.TaskOutcome{
		ProjectID: testProject, RepoPath: root, TaskRef: "t1",
		Title: "work", WhatChanged: "something", Integrated: true,
	}); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := svc.Rebuild(f.ctx, testProject, root, "c2", "main", true)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Generation <= out.Generation {
		t.Fatalf("rebuild generation %d did not advance past %d", rebuilt.Generation, out.Generation)
	}
	items, err := f.store.ListProjectMemoryItems(f.ctx, testProject, rebuilt.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Key.Type == domain.MemoryTypeTaskResult {
			t.Fatal("a purge rebuild left task memory behind")
		}
		if it.SourceCommit != "c2" && it.SourceCommit != "" {
			t.Fatalf("item %s still carries the pre-rebuild commit %q", it.Key, it.SourceCommit)
		}
	}
}

func TestSyncIsANoOpWhenAlreadyAtTheCommit(t *testing.T) {
	f, svc, root, _ := packService(t)
	out, err := svc.Sync(f.ctx, testProject, root, "c1", "main")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Skipped {
		t.Fatalf("sync at the indexed commit did work: %+v", out)
	}
	if out.PathsRead != 0 {
		t.Fatalf("sync read %d paths when already up to date", out.PathsRead)
	}
}

func TestProviderNeutralityOfTheRenderedPack(t *testing.T) {
	f, svc, root, _ := packService(t)
	pack := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
	})
	rendered := strings.ToLower(pack.Render())
	for _, provider := range []string{"claude", "codex", "anthropic", "openai", "copilot", "gpt"} {
		if strings.Contains(rendered, provider) {
			t.Fatalf("the rendered pack names the provider %q; packs must be provider-neutral", provider)
		}
	}
	if !strings.Contains(rendered, "not the repository itself") {
		t.Error("the pack does not state that it is a derived cache rather than the source of truth")
	}
}

func TestServiceSurvivesAnUnavailableOptionalGraph(t *testing.T) {
	f := newFixture(t)
	var reported []error
	svc := projectmemory.NewService(f.store, projectmemory.WithGraph(&projectmemory.TeeGraph{
		Canonical:       projectmemory.NewLocalGraph(f.store),
		Optional:        projectmemory.UnavailableGraph{Backend: "grae"},
		OnOptionalError: func(err error) { reported = append(reported, err) },
	}))
	root := goRepo(t)

	out, err := svc.Index(f.ctx, projectmemory.IndexRequest{
		ProjectID: testProject, RepoPath: root, Commit: "c1",
	})
	if err != nil {
		t.Fatalf("indexing failed because an OPTIONAL graph backend was down: %v", err)
	}
	if out.RelationsWritten == 0 {
		t.Fatal("no relations were written to the canonical backend")
	}
	if len(reported) == 0 {
		t.Fatal("the optional backend's outage was never reported")
	}

	// Traversal still answers, from the canonical backend.
	edges, err := svc.Graph().Neighbors(f.ctx, projectmemory.GraphQuery{
		ProjectID: testProject, RepoID: out.RepoID,
		Node: domain.ProjectMemoryNode{Kind: domain.NodeModule, Key: "internal/server"},
	})
	if err != nil {
		t.Fatalf("traversal failed with the optional backend down: %v", err)
	}
	if len(edges) == 0 {
		t.Fatal("the canonical backend answered nothing")
	}
	if !strings.Contains(svc.Graph().Name(), "local") {
		t.Errorf("graph name %q does not name the canonical backend", svc.Graph().Name())
	}
}

func TestPackClockIsInjectable(t *testing.T) {
	f := newFixture(t)
	fixed := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	svc := projectmemory.NewService(f.store, projectmemory.WithServiceClock(func() time.Time { return fixed }))
	root := goRepo(t)
	if _, err := svc.Index(f.ctx, projectmemory.IndexRequest{
		ProjectID: testProject, RepoPath: root, Commit: "c1",
	}); err != nil {
		t.Fatal(err)
	}
	pack := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
	})
	if !pack.BuiltAt.Equal(fixed) {
		t.Fatalf("pack built at %v, want the injected %v", pack.BuiltAt, fixed)
	}
}

// Selection ranks by relevance first and confidence second, and the ordering is
// total: with equal relevance and equal confidence it falls through to
// freshness and then to the derived id, so no two orderings of the same set are
// possible. This is what makes the pack digest meaningful.
func TestContextPackOrdersByRelevanceThenConfidence(t *testing.T) {
	f, svc, root, _ := packService(t)

	pack := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		Budget: projectmemory.PackBudget{MaxBytes: 64 * 1024, MaxItems: 100},
	})

	// Within a section — where every fact has the same relevance because none
	// matches a changed path — confidence must be non-increasing.
	for _, section := range pack.Sections {
		for i := 1; i < len(section.Items); i++ {
			prev, cur := section.Items[i-1], section.Items[i]
			if cur.Score > prev.Score {
				t.Fatalf("section %s is not in score order: %v after %v",
					section.Title, cur.Score, prev.Score)
			}
			if cur.Score == prev.Score && cur.Item.Confidence > prev.Item.Confidence {
				t.Fatalf("section %s: confidence %v ranked after %v at equal relevance",
					section.Title, cur.Item.Confidence, prev.Item.Confidence)
			}
		}
	}

	// A changed path outranks confidence: the module covering it is selected
	// ahead of a higher-confidence fact that has nothing to do with the work.
	targeted := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		ChangedPaths: []string{"internal/store/store.go"},
		Budget:       projectmemory.PackBudget{MaxBytes: 64 * 1024, MaxItems: 100},
	})
	var best projectmemory.SelectedItem
	for _, section := range targeted.Sections {
		for _, sel := range section.Items {
			if sel.Score > best.Score {
				best = sel
			}
		}
	}
	if best.Reason != "covers a path this work changes" {
		t.Fatalf("the top-ranked fact was selected for %q, not for covering the changed path", best.Reason)
	}
	if best.Item.Confidence >= 0.95 {
		t.Log("note: the top fact also happens to be the most confident; the reason above is what pins the ordering")
	}
}

// Task memory is the one part of project memory that grows with time rather
// than with the repository, so it is the one part with an explicit retention
// bound. Beyond it the oldest outcomes are retired, not deleted.
func TestTaskMemoryIsCompactedBeyondItsRetentionBound(t *testing.T) {
	f := newFixture(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tick := 0
	svc := projectmemory.NewService(f.store, projectmemory.WithServiceClock(func() time.Time {
		tick++
		return base.Add(time.Duration(tick) * time.Minute)
	}))
	root := goRepo(t)
	if _, err := svc.Index(f.ctx, projectmemory.IndexRequest{
		ProjectID: testProject, RepoPath: root, Commit: "c1",
	}); err != nil {
		t.Fatal(err)
	}

	const over = projectmemory.MaxRetainedTaskResults + 5
	for i := range over {
		if err := svc.RecordTaskOutcome(f.ctx, projectmemory.TaskOutcome{
			ProjectID: testProject, RepoPath: root,
			TaskRef:     fmt.Sprintf("t%03d", i),
			Title:       fmt.Sprintf("task %d", i),
			WhatChanged: "something", Integrated: true, Commit: "c1",
		}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}

	repoID := domain.ProjectMemoryRepoID(mustEval(t, root))
	valid, err := f.store.ListProjectMemoryItemsByState(f.ctx, testProject, repoID, domain.MemoryStateValid)
	if err != nil {
		t.Fatal(err)
	}
	live := 0
	for _, it := range valid {
		if it.Key.Type == domain.MemoryTypeTaskResult {
			live++
		}
	}
	if live > projectmemory.MaxRetainedTaskResults {
		t.Fatalf("%d live task outcomes, over the %d retention bound",
			live, projectmemory.MaxRetainedTaskResults)
	}

	// Retired, not deleted: the aged-out outcomes are still readable, and they
	// say why they aged out.
	all, err := f.store.ListProjectMemoryItems(f.ctx, testProject, repoID)
	if err != nil {
		t.Fatal(err)
	}
	compacted := 0
	for _, it := range all {
		if it.Key.Type == domain.MemoryTypeTaskResult && it.State == domain.MemoryStateInvalidated {
			compacted++
			if !strings.Contains(it.StateReason, "compacted") {
				t.Fatalf("an aged-out outcome says %q", it.StateReason)
			}
		}
	}
	if compacted == 0 {
		t.Fatal("nothing was compacted, so the retention bound is not enforced")
	}
	if compacted+live != over {
		t.Fatalf("%d live + %d compacted != %d recorded", live, compacted, over)
	}
}
