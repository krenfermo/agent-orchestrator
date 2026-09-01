package projectmemory_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// integrity_test.go — P2-D §30 and §32.
//
// The claims, one test each:
//
//	An unprovable fact is never served, to any role.
//	A withheld fact is withheld with a REASON an operator can act on.
//	A canonical fact with no promotion authority fails closed.
//	A stale decision does not resurrect the one it superseded.
//	A conflict stays explicit rather than being silently ordered.
//	A rename carries knowledge; a delete retires it.
//	Legacy rows are withheld, not deleted, and not given invented provenance.
//	Withholding a fact retires the edges around it without erasing them.

// repoIDOf is the identity the SERVICE filed this repository's facts under.
//
// It is read back from the index row rather than recomputed from the path the
// test holds, and that is not a convenience: the service canonicalises the path
// (resolving symlinks) before hashing it, and on macOS a t.TempDir() under
// /var resolves to /private/var. A test that hashed its own path would address
// a repository that has no facts and would pass by finding nothing.
func repoIDOf(t *testing.T, f *fixture) string {
	t.Helper()
	states, err := f.store.ListProjectMemoryIndexStates(f.ctx, testProject)
	if err != nil {
		t.Fatalf("list index states: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("%d indexed repositories, want exactly 1", len(states))
	}
	return states[0].RepoID
}

// unprovableItem is a canonical task-derived fact whose promotion AO cannot
// prove: exactly what P2-C's promotion path could produce, and what P2-D must
// refuse to serve.
func unprovableItem(repoID string) domain.ProjectMemoryItem {
	return domain.ProjectMemoryItem{
		Key: domain.ProjectMemoryKey{
			ProjectID: testProject, RepoID: repoID,
			Type: domain.MemoryTypeDecision, Scope: domain.MemoryScopeRepository, Key: "transport",
		},
		Summary:        "the API speaks GraphQL",
		Origin:         domain.OriginCanonical,
		ProvenanceKind: domain.ProvenanceWorkflowKnowledge,
		SourceCommit:   "c1",
		Confidence:     0.9,
	}.Normalized()
}

// TestUnprovableFactsAreNeverServedToAnyRole is P2-D §32's first line and the
// whole fail-closed rule at the read side.
//
// The fact is perfectly valid on the DRIFT axis -- nothing about its sources
// has moved -- and it is still withheld, because its licence cannot be shown.
// If the two axes had been one column this test would be untestable.
func TestUnprovableFactsAreNeverServedToAnyRole(t *testing.T) {
	f, svc, root, _ := packService(t)
	item := unprovableItem(repoIDOf(t, f))

	if _, err := f.store.PutProjectMemoryItem(f.ctx, item, f.now()); err != nil {
		t.Fatalf("seed fact: %v", err)
	}
	served := func() bool {
		for _, role := range []projectmemory.PackRole{
			projectmemory.RolePlanner, projectmemory.RoleWorker,
			projectmemory.RoleReviewer, projectmemory.RoleRepair,
		} {
			pack := svc.Context(f.ctx, projectmemory.PackRequest{
				ProjectID: testProject, RepoPath: root, Role: role,
				Keywords: []string{"GraphQL", "transport"},
			})
			for _, section := range pack.Sections {
				for _, sel := range section.Items {
					if sel.Item.ID == item.ID {
						return true
					}
				}
			}
		}
		return false
	}
	if !served() {
		t.Fatal("the fixture fact was not selected even while authoritative, so the test below proves nothing")
	}

	if _, err := f.store.SetProjectMemoryItemAuthority(f.ctx, item.ID, item.Generation,
		domain.AuthorityUnprovable,
		domain.MemoryAuthorityReason(domain.ReasonPromotionUnprovable, "no integration on record"),
		f.now()); err != nil {
		t.Fatalf("withhold: %v", err)
	}
	if served() {
		t.Fatal("an unprovable fact was still handed to a role as current")
	}

	// And the exclusion is attributed to the right counter, so an operator
	// reading the pack stats can tell a licence problem from a drift problem.
	pack := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RolePlanner,
		Keywords: []string{"GraphQL"},
	})
	if pack.Stats.UnprovableExcluded == 0 {
		t.Fatalf("the withheld fact was not counted as unprovable: %+v", pack.Stats)
	}
}

// TestCanonicalKnowledgeWithNoPromotionAuthorityIsWithheld is the validator
// closing P2-C's hole.
//
// A canonical fact derived from a task claims the project HAS that work. With
// no mutation-provenance row behind it, AO cannot show that, so the validation
// pass withholds it and names why.
func TestCanonicalKnowledgeWithNoPromotionAuthorityIsWithheld(t *testing.T) {
	f, svc, root, _ := packService(t)
	item := unprovableItem(repoIDOf(t, f))
	if _, err := f.store.PutProjectMemoryItem(f.ctx, item, f.now()); err != nil {
		t.Fatalf("seed fact: %v", err)
	}

	dry, err := svc.Validate(f.ctx, projectmemory.ValidateRequest{ProjectID: testProject, RepoPath: root})
	if err != nil {
		t.Fatalf("validate (dry run): %v", err)
	}
	var found *projectmemory.ValidateFinding
	for i := range dry.Findings {
		if dry.Findings[i].ItemID == item.ID {
			found = &dry.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("a canonical fact with no promotion authority was reported as provable: %+v", dry)
	}
	if found.ReasonClass != domain.ReasonPromotionUnprovable {
		t.Fatalf("reason class = %q, want %q", found.ReasonClass, domain.ReasonPromotionUnprovable)
	}
	if found.Applied {
		t.Fatal("a dry run applied its findings")
	}
	stored, _, err := f.store.GetProjectMemoryItem(f.ctx, item.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !stored.Servable() {
		t.Fatal("a dry run withheld a fact")
	}

	applied, err := svc.Validate(f.ctx, projectmemory.ValidateRequest{
		ProjectID: testProject, RepoPath: root, Apply: true,
	})
	if err != nil {
		t.Fatalf("validate (apply): %v", err)
	}
	if !applied.Withheld() {
		t.Fatal("the applying pass withheld nothing")
	}
	after, _, err := f.store.GetProjectMemoryItem(f.ctx, item.ID)
	if err != nil {
		t.Fatalf("read back after apply: %v", err)
	}
	if after.Servable() {
		t.Fatal("the fact is still served after validation withheld it")
	}
	if after.State != domain.MemoryStateValid {
		t.Fatalf("state = %q: withholding a licence must not touch the drift axis", after.State)
	}
}

// TestStaleDecisionDoesNotResurrectItsPredecessor is P2-D §11, and it is the
// test that section calls mandatory.
//
// Decision B supersedes decision A. B then becomes unprovable. The project must
// end up with NO current answer on the subject -- not with A back in force. A
// was retired because the project changed its mind, and B losing its licence is
// not evidence that the project changed it back.
func TestStaleDecisionDoesNotResurrectItsPredecessor(t *testing.T) {
	f, svc, root, _ := packService(t)

	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-a", Title: "pick a transport",
		FilesChanged: []string{"internal/store/store.go"},
		Integrated:   true, Commit: "c1",
		Decisions: []projectmemory.TaskDecision{{
			Statement: "module X uses GraphQL",
			Topic:     "transport", Scope: domain.MemoryScopeModule, ScopeKey: "internal/store",
			Evidence: []string{"internal/store/store.go"},
		}},
	})
	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-b", Title: "switch the transport",
		FilesChanged: []string{"internal/store/store.go"},
		Integrated:   true, Commit: "c2",
		Decisions: []projectmemory.TaskDecision{{
			Statement: "module X uses gRPC",
			Topic:     "transport", Scope: domain.MemoryScopeModule, ScopeKey: "internal/store",
			Evidence: []string{"internal/store/store.go"},
		}},
	})

	current := func() []string {
		pack := svc.Context(f.ctx, projectmemory.PackRequest{
			ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
			TaskRef: "task-c", ChangedPaths: []string{"internal/store/store.go"},
			Modules: []string{"internal/store"},
		})
		var out []string
		for _, section := range pack.Sections {
			for _, sel := range section.Items {
				if sel.Item.Key.Type == domain.MemoryTypeDecision {
					out = append(out, sel.Item.Summary)
				}
			}
		}
		return out
	}

	before := current()
	if len(before) != 1 || !strings.Contains(before[0], "gRPC") {
		t.Fatalf("current decisions = %v, want only the gRPC one", before)
	}

	// B loses its licence.
	items, err := f.store.ListProjectMemoryItems(f.ctx, testProject, repoIDOf(t, f))
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	withheld := 0
	for _, item := range items {
		if item.Key.Type != domain.MemoryTypeDecision || !strings.Contains(item.Summary, "gRPC") {
			continue
		}
		ok, err := f.store.SetProjectMemoryItemAuthority(f.ctx, item.ID, item.Generation,
			domain.AuthorityUnprovable,
			domain.MemoryAuthorityReason(domain.ReasonSupersededSourceChanged,
				"the files supporting this decision changed and it has not been re-derived"),
			f.now())
		if err != nil {
			t.Fatalf("withhold gRPC decision: %v", err)
		}
		if ok {
			withheld++
		}
	}
	if withheld == 0 {
		t.Fatal("the superseding decision was never found to withhold")
	}

	after := current()
	for _, summary := range after {
		if strings.Contains(summary, "GraphQL") {
			t.Fatalf("the superseded decision came back when its successor went stale: %v", after)
		}
		if strings.Contains(summary, "gRPC") {
			t.Fatalf("an unprovable decision is still being served: %v", after)
		}
	}
}

// TestValidationRetiresEdgesWithoutErasingThem is P2-D §23.
//
// An edge naming a fact AO can no longer prove must stop being traversed as
// current. It must NOT be deleted: the record that two facts were once related
// is exactly what an operator reads when asking why a decision was made, and
// deleting it would make the audit trail depend on the facts still being
// current.
func TestValidationRetiresEdgesWithoutErasingThem(t *testing.T) {
	f, svc, root, _ := packService(t)
	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-a", Title: "harden the store",
		FilesChanged: []string{"internal/store/store.go"},
		Integrated:   true, Commit: "c1",
		Decisions: []projectmemory.TaskDecision{{
			Statement: "the store queue is a table",
			Topic:     "queue-storage", Scope: domain.MemoryScopeModule, ScopeKey: "internal/store",
			Evidence: []string{"internal/store/store.go"},
		}},
	})

	repoID := repoIDOf(t, f)
	before, err := f.store.ListProjectMemoryRelations(f.ctx, testProject, repoID)
	if err != nil {
		t.Fatalf("list relations: %v", err)
	}
	if len(before) == 0 {
		t.Fatal("the fixture produced no edges, so this test proves nothing")
	}

	report, err := svc.Validate(f.ctx, projectmemory.ValidateRequest{
		ProjectID: testProject, RepoPath: root, Apply: true,
	})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if report.EdgesRetired == 0 {
		t.Fatalf("withholding %d facts retired no edges: %+v", len(report.Findings), report)
	}

	after, err := f.store.ListProjectMemoryRelations(f.ctx, testProject, repoID)
	if err != nil {
		t.Fatalf("list relations after: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("%d edges after validation, %d before: edges were deleted rather than retired",
			len(after), len(before))
	}
	retired := 0
	for _, rel := range after {
		if !rel.Traversable() {
			retired++
			if rel.AuthorityReason == "" {
				t.Fatalf("edge %s was retired with no reason recorded", rel.ID)
			}
		}
	}
	if retired == 0 {
		t.Fatal("no edge was actually retired")
	}
}

// TestRenameCarriesKnowledgeAndDeleteRetiresIt is P2-D §10 and §30's
// rename/delete pair.
//
// A rename git PROVED moves a decision's evidence to the new path, because
// nothing will ever re-derive that decision and retiring it would silently
// delete the project's own reasoning. A delete retires it, because the subject
// really is gone.
func TestRenameCarriesKnowledgeAndDeleteRetiresIt(t *testing.T) {
	f, svc, root, _ := packService(t)
	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-a", Title: "decide the store shape",
		FilesChanged: []string{"internal/store/store.go"},
		Integrated:   true, Commit: "c1",
		Decisions: []projectmemory.TaskDecision{{
			Statement: "the store queue is a table",
			Topic:     "queue-storage", Scope: domain.MemoryScopeModule, ScopeKey: "internal/store",
			Evidence: []string{"internal/store/store.go"},
		}},
	})

	// The rename, on disk and in the change set.
	if err := os.Rename(
		filepath.Join(root, "internal/store/store.go"),
		filepath.Join(root, "internal/store/queue.go"),
	); err != nil {
		t.Fatalf("rename: %v", err)
	}
	out, err := svc.Update(f.ctx, projectmemory.UpdateRequest{
		ProjectID: testProject, RepoPath: root, ToCommit: "c2",
		Changes: []projectmemory.PathChange{{
			Kind: projectmemory.ChangeRenamed,
			Path: "internal/store/queue.go", PreviousPath: "internal/store/store.go",
		}},
	})
	if err != nil {
		t.Fatalf("apply rename: %v", err)
	}
	if out.RenamesFollowed == 0 {
		t.Fatalf("the rename retired the decision instead of carrying it: %+v", out)
	}

	decision := findDecision(t, f, root, "queue is a table")
	if decision.State != domain.MemoryStateValid {
		t.Fatalf("the decision went %q on a rename: %s", decision.State, decision.StateReason)
	}
	if !containsPath(decision.SourcePaths, "internal/store/queue.go") {
		t.Fatalf("the decision's evidence was not moved: %v", decision.SourcePaths)
	}
	if containsPath(decision.SourcePaths, "internal/store/store.go") {
		t.Fatalf("the decision still points at the old path: %v", decision.SourcePaths)
	}

	// Now delete the file the decision is about. It really is gone, so the
	// decision stops being current.
	if err := os.Remove(filepath.Join(root, "internal/store/queue.go")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := svc.Update(f.ctx, projectmemory.UpdateRequest{
		ProjectID: testProject, RepoPath: root, ToCommit: "c3",
		Changes: []projectmemory.PathChange{{
			Kind: projectmemory.ChangeDeleted, Path: "internal/store/queue.go",
		}},
	}); err != nil {
		t.Fatalf("apply delete: %v", err)
	}
	gone := findDecision(t, f, root, "queue is a table")
	if gone.State == domain.MemoryStateValid {
		t.Fatal("a decision whose only evidence was deleted is still current")
	}
}

func findDecision(t *testing.T, f *fixture, root, needle string) domain.ProjectMemoryItem {
	t.Helper()
	items, err := f.store.ListProjectMemoryItems(f.ctx, testProject, repoIDOf(t, f))
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	for _, item := range items {
		if item.Key.Type == domain.MemoryTypeDecision && strings.Contains(item.Summary, needle) {
			return item
		}
	}
	t.Fatalf("no decision matching %q", needle)
	return domain.ProjectMemoryItem{}
}

func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

// TestUnchangedSourcesStayValidAndOnlyTheChangedOneIsWithheld is P2-D §30's
// first two lines, together, because "only the dependent one" is the claim
// that matters: an integrity check that invalidated everything on any change
// would be safe and useless.
func TestUnchangedSourcesStayValidAndOnlyTheChangedOneIsWithheld(t *testing.T) {
	f, svc, root, _ := packService(t)

	clean, err := svc.Verify(f.ctx, testProject, root, "c1", true)
	if err != nil {
		t.Fatalf("verify unchanged: %v", err)
	}
	if clean.Drifted() {
		t.Fatalf("an untouched repository reported drift: %+v", clean.Findings)
	}
	if clean.Confirmed == 0 {
		t.Fatal("nothing was confirmed, so the clean result proves nothing")
	}

	// cmd/app/main.go rather than a store file: the fixture repository yields a
	// file_summary for it, so it is a path memory actually carries a digest
	// for. Changing a file nothing was derived from would produce no drift and
	// would make this test pass for the wrong reason.
	if err := os.WriteFile(filepath.Join(root, "cmd/app/main.go"),
		[]byte("// Command app is the entry point.\npackage main\n\nfunc main() { panic(\"changed\") }\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	drifted, err := svc.Verify(f.ctx, testProject, root, "c2", true)
	if err != nil {
		t.Fatalf("verify changed: %v", err)
	}
	if !drifted.Drifted() {
		t.Fatal("a changed source produced no drift finding")
	}
	for _, finding := range drifted.Findings {
		if !containsPath(sourcePathsOf(t, f, finding.ItemID), "cmd/app/main.go") {
			t.Fatalf("drift withheld a fact not derived from the changed file: %s", finding.ItemID)
		}
	}
}

func sourcePathsOf(t *testing.T, f *fixture, id string) []string {
	t.Helper()
	item, found, err := f.store.GetProjectMemoryItem(f.ctx, id)
	if err != nil || !found {
		t.Fatalf("read %s: found=%v err=%v", id, found, err)
	}
	return item.SourcePaths
}

// TestManifestRecordsItemVersionsAndIsReproducible is P2-D §18 and §32.
//
// Three claims in one test, because they are the same claim from three sides:
// a manifest names the exact VERSION of every fact it carried, re-provisioning
// the same unchanged state addresses the SAME manifest row, and a fact whose
// authority changes produces a NEW manifest rather than rewriting the old one.
//
// The third is what makes a historical manifest evidence: "what was this
// execution told" has to keep its answer even after the answer stops being
// true.
func TestManifestRecordsItemVersionsAndIsReproducible(t *testing.T) {
	f, prov, root := provFixture(t, projectmemory.ModeAssisted)
	svc := projectmemory.NewService(f.store)

	request := projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleReviewer,
		TaskRef: "task-a", WorkflowRunID: "run-1",
		ChangedPaths: []string{"cmd/app/main.go"},
		HeadSHA:      "abc1234567890abc1234567890abc1234567890a",
	}
	first := prov.Provision(f.ctx, request)
	if !first.Attached() {
		t.Fatalf("nothing was attached: %s", first.Metrics.FallbackReason)
	}

	manifests, err := svc.ContextManifests(f.ctx, testProject, "task-a", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 1 {
		t.Fatalf("%d manifests, want 1", len(manifests))
	}
	m := manifests[0]
	if len(m.ItemVersions) != len(m.ItemIDs) {
		t.Fatalf("%d versions for %d items: position no longer means anything",
			len(m.ItemVersions), len(m.ItemIDs))
	}
	if m.RoleHeadSHA != request.HeadSHA {
		t.Fatalf("role head = %q, want the reviewed SHA %q", m.RoleHeadSHA, request.HeadSHA)
	}
	for i, id := range m.ItemIDs {
		item, found, err := f.store.GetProjectMemoryItem(f.ctx, id)
		if err != nil || !found {
			t.Fatalf("manifest names an item that does not exist: %s", id)
		}
		if m.ItemVersions[i] != item.ContentHash {
			t.Fatalf("version %d = %q, want the item's content hash %q",
				i, m.ItemVersions[i], item.ContentHash)
		}
	}

	// Reproducible: nothing changed, so the same dispatch addresses the same
	// row rather than appending a second observation of the same answer.
	second := prov.Provision(f.ctx, request)
	if second.Pack.Digest != first.Pack.Digest {
		t.Fatalf("an unchanged state produced two different packs: %q then %q",
			first.Pack.Digest, second.Pack.Digest)
	}
	again, err := svc.ContextManifests(f.ctx, testProject, "task-a", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 {
		t.Fatalf("%d manifests after re-provisioning identical state, want 1", len(again))
	}

	// Now withhold one of the facts it carried. The pack changes, so a NEW
	// manifest appears -- and the original stays exactly as it was, because a
	// record of what an execution was told must not be rewritten by what
	// happened afterwards.
	target, _, err := f.store.GetProjectMemoryItem(f.ctx, m.ItemIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.SetProjectMemoryItemAuthority(f.ctx, target.ID, target.Generation,
		domain.AuthorityUnprovable,
		domain.MemoryAuthorityReason(domain.ReasonProvenanceMissing, "withheld by this test"),
		f.now()); err != nil {
		t.Fatalf("withhold: %v", err)
	}
	third := prov.Provision(f.ctx, request)
	if third.Pack.Digest == first.Pack.Digest {
		t.Fatal("withholding a fact did not change the pack the dispatch received")
	}
	after, err := svc.ContextManifests(f.ctx, testProject, "task-a", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Fatalf("%d manifests after the authority changed, want 2", len(after))
	}
	for _, got := range after {
		if got.ID != m.ID {
			continue
		}
		if got.PackDigest != m.PackDigest || len(got.ItemIDs) != len(m.ItemIDs) {
			t.Fatal("the historical manifest was rewritten by a later provisioning")
		}
	}
}

// TestParallelTasksOnTheSameModuleDoNotBothStayAuthoritative is P2-D §15.
//
// Two tasks integrate work on the same module, one after the other, and both
// record a decision on the same topic. The project must converge on ONE current
// answer -- the later one -- with the earlier retired rather than both left
// standing as current. Two authoritative decisions on one subject is the
// ambiguity this whole subsystem exists to prevent.
//
// The complementary case, two tasks on DIFFERENT modules, must keep both: a
// convergence rule that discarded unrelated work would be worse than the
// ambiguity it removes.
func TestParallelTasksOnTheSameModuleDoNotBothStayAuthoritative(t *testing.T) {
	f, svc, root, _ := packService(t)

	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-a", Title: "A",
		FilesChanged: []string{"internal/store/store.go"},
		Integrated:   true, Commit: "c1",
		Decisions: []projectmemory.TaskDecision{{
			Statement: "the store writes synchronously",
			Topic:     "store-writes", Scope: domain.MemoryScopeModule, ScopeKey: "internal/store",
			Evidence: []string{"internal/store/store.go"},
		}},
	})
	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-b", Title: "B",
		FilesChanged: []string{"internal/store/store.go"},
		Integrated:   true, Commit: "c2",
		Decisions: []projectmemory.TaskDecision{{
			Statement: "the store writes through a queue",
			Topic:     "store-writes", Scope: domain.MemoryScopeModule, ScopeKey: "internal/store",
			Evidence: []string{"internal/store/store.go"},
		}},
	})
	// A third task, elsewhere, whose decision must survive both of the above.
	recordTask(t, f, svc, root, projectmemory.TaskOutcome{
		TaskRef: "task-c", Title: "C",
		FilesChanged: []string{"cmd/app/main.go"},
		Integrated:   true, Commit: "c3",
		Decisions: []projectmemory.TaskDecision{{
			Statement: "the entry point takes no flags",
			Topic:     "cli-flags", Scope: domain.MemoryScopeModule, ScopeKey: "cmd/app",
			Evidence: []string{"cmd/app/main.go"},
		}},
	})

	items, err := f.store.ListProjectMemoryItems(f.ctx, testProject, repoIDOf(t, f))
	if err != nil {
		t.Fatal(err)
	}
	current := map[string][]string{}
	for _, item := range items {
		if item.Key.Type != domain.MemoryTypeDecision {
			continue
		}
		if !item.Servable() || !domain.KnowledgeStatusOf(item).Current() {
			continue
		}
		topic := item.Key.Key
		current[topic] = append(current[topic], item.Summary)
	}
	// Grouped by the DERIVED subject key rather than by the topic string: the
	// key is a hash of scope plus topic, and re-deriving that composition here
	// would make this test agree with the writer by construction instead of by
	// observation.
	if len(current) != 2 {
		t.Fatalf("%d subjects carry a current decision, want 2 (one per module): %v", len(current), current)
	}
	var storeWrites, cliFlags []string
	for _, summaries := range current {
		if len(summaries) != 1 {
			t.Fatalf("%d current decisions on one subject, want exactly 1: %v", len(summaries), summaries)
		}
		switch {
		case strings.Contains(summaries[0], "store writes"):
			storeWrites = summaries
		case strings.Contains(summaries[0], "entry point"):
			cliFlags = summaries
		}
	}
	if len(cliFlags) != 1 {
		t.Fatalf("the unrelated module's decision did not survive: %v", current)
	}
	if !strings.Contains(storeWrites[0], "queue") {
		t.Fatalf("the current decision on the contended module is %q, want the later one", storeWrites[0])
	}
}

// TestWithheldFactIsNotServedFromThePackCache is the hole P2-D made visible.
//
// The pack cache is keyed on the memory GENERATION and the indexed commit, and
// both move only when an indexing pass runs. Every out-of-band demotion --
// drift invalidation, `ao memory invalidate`, an authority pass, a promotion
// recording a refusal -- changes what a reader should be served without
// touching either, so before the change mark was added to the key a pack
// cached moments earlier stayed reachable and kept serving a fact AO had just
// withheld.
//
// Withholding a fact and immediately re-provisioning the SAME dispatch is
// exactly that window, and it must not contain the fact.
func TestWithheldFactIsNotServedFromThePackCache(t *testing.T) {
	f, prov, root := provFixture(t, projectmemory.ModeAssisted)
	request := projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		TaskRef: "task-a", ChangedPaths: []string{"cmd/app/main.go"},
	}

	first := prov.Provision(f.ctx, request)
	if !first.Attached() {
		t.Fatalf("nothing was attached: %s", first.Metrics.FallbackReason)
	}
	if first.Pack.Stats.SelectedItems == 0 {
		t.Fatal("the first pack carried nothing, so this test proves nothing")
	}

	// Prove the cache is actually live, or the assertion below would pass
	// simply because nothing was ever cached.
	warm := prov.Provision(f.ctx, request)
	if !warm.Metrics.CacheHit {
		t.Fatal("the second identical provision missed the cache, so the window under test does not exist here")
	}

	target := first.Pack.Sections[0].Items[0].Item
	if _, err := f.store.SetProjectMemoryItemAuthority(f.ctx, target.ID, target.Generation,
		domain.AuthorityUnprovable,
		domain.MemoryAuthorityReason(domain.ReasonPromotionUnprovable, "withheld by this test"),
		f.now()); err != nil {
		t.Fatalf("withhold: %v", err)
	}

	after := prov.Provision(f.ctx, request)
	for _, section := range after.Pack.Sections {
		for _, sel := range section.Items {
			if sel.Item.ID == target.ID {
				t.Fatal("a withheld fact was served from the pack cache")
			}
		}
	}
	if after.Metrics.CacheHit {
		t.Fatal("the provision after a demotion hit the cache, so the key is not fenced by it")
	}
}
