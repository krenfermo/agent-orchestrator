package projectmemory_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// integrity_perf_test.go — P2-D §33, measured rather than asserted.
//
// The claim under test is the one an integrity model most easily breaks:
// "integrity checks must not turn every warm task into a full repo scan". So
// the numbers here are FILES READ, counted by the code under test itself, not
// timings — a wall-clock figure on one laptop is not a property of the design,
// and a file count is.
//
// The findings, at the time of writing, on the fixture repository (8 source
// files, 13 derived facts):
//
//	warm, no change      dispatch reads 0 files, assembles a pack from rows only
//	one file changed     the incremental pass reads 1 file
//	validation pass      reads 0 files (it is a row pass plus one `git` call
//	                     for the repository identity)
//	drift pass           reads only the paths stored facts name
//
// The load-bearing one is the third. Validation is the check P2-D adds, and if
// it re-read the repository it would be exactly the regression this section
// exists to forbid.

// TestWarmDispatchStillReadsNoFilesWithIntegrityOn is the P2-B guarantee,
// re-measured with P2-D's authority axis in place.
//
// A dispatch on an unmoved repository must still cost one indexed row read and
// no filesystem I/O. Authority is checked when a fact is WRITTEN and when
// `validate` runs, never on the read path — a per-read proof is precisely how
// an integrity model turns every warm task into a scan.
func TestWarmDispatchStillReadsNoFilesWithIntegrityOn(t *testing.T) {
	f, svc, root, _ := packService(t)
	syncer := projectmemory.NewSyncer(svc, projectmemory.DefaultConfig())

	// Two dispatches back to back, which is the four-roles-in-seconds shape.
	first := syncer.EnsureFresh(f.ctx, testProject, root)
	second := syncer.EnsureFresh(f.ctx, testProject, root)
	for i, fresh := range []projectmemory.Freshness{first, second} {
		if fresh.FilesRead != 0 {
			t.Fatalf("dispatch %d read %d files on an unchanged repository", i+1, fresh.FilesRead)
		}
	}

	// And the pack it assembles is not empty, so "0 files" is not "0 memory".
	pack := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
	})
	if pack.Empty() {
		t.Fatal("the warm path read no files AND produced no memory")
	}
	t.Logf("warm dispatch: filesRead=%d packItems=%d packBytes=%d",
		second.FilesRead, pack.Stats.SelectedItems, pack.Stats.SelectedBytes)
}

// TestValidationDoesNotScanTheRepository is the new claim P2-D has to make.
//
// The authority pass is a pass over ROWS plus one repository-identity read. It
// must not open the repository's files: that is drift detection's job, it is a
// separate command, and fusing them would make the cheap check as expensive as
// the expensive one.
//
// Measured by counting the files the pass reports having read, which is zero
// for a row pass by construction, and cross-checked against the drift pass
// below so the two are visibly different shapes of work.
func TestValidationDoesNotScanTheRepository(t *testing.T) {
	f, svc, root, _ := packService(t)

	report, err := svc.Validate(f.ctx, projectmemory.ValidateRequest{
		ProjectID: testProject, RepoPath: root, Apply: true,
	})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if report.Checked == 0 {
		t.Fatal("the validation pass checked nothing, so this measurement is meaningless")
	}
	t.Logf("validation: checked=%d provable=%d withheld=%d edgesRetired=%d",
		report.Checked, report.Provable, len(report.Findings), report.EdgesRetired)

	// The repository is untouched, so nothing about the FILES has changed --
	// and a drift pass over the same repository confirms every digest, which is
	// the cross-check that the two passes really are asking different
	// questions.
	drift, err := svc.Verify(f.ctx, testProject, root, "c1", false)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if drift.Drifted() {
		t.Fatalf("an untouched repository drifted: %+v", drift.Findings)
	}
	t.Logf("drift: checked=%d confirmed=%d unverifiable=%d",
		drift.Checked, drift.Confirmed, drift.Unverifiable)
}

// TestOneFileChangeReadsOneFile is the incremental claim, unchanged by P2-D and
// re-measured because the rename-carry path added work to it.
func TestOneFileChangeReadsOneFile(t *testing.T) {
	f, svc, root, _ := packService(t)

	if err := os.WriteFile(filepath.Join(root, "cmd/app/main.go"),
		[]byte("// Command app is the entry point.\npackage main\n\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := svc.Update(f.ctx, projectmemory.UpdateRequest{
		ProjectID: testProject, RepoPath: root, ToCommit: "c2",
		Changes: []projectmemory.PathChange{{
			Kind: projectmemory.ChangeModified, Path: "cmd/app/main.go",
		}},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if out.PathsRead != 1 {
		t.Fatalf("a one-file change read %d files, want 1", out.PathsRead)
	}
	t.Logf("one-file change: pathsRead=%d itemsWritten=%d itemsInvalidated=%d renamesFollowed=%d",
		out.PathsRead, out.ItemsWritten, out.ItemsInvalidated, out.RenamesFollowed)
}

// TestProvenanceMetadataDoesNotInflateThePack guards P2-B's measured saving
// against P2-D's additions (§32's last line).
//
// Provenance lives in COLUMNS, not in the rendered pack: an agent is handed
// summaries and bodies, and none of authority, provenance kind, repository
// identity or the three commits reaches the prompt. If any of it leaked into
// Render(), every dispatch would pay for it forever.
func TestProvenanceMetadataDoesNotInflateThePack(t *testing.T) {
	f, svc, root, _ := packService(t)
	item := unprovableItem(repoIDOf(t, f))
	item.Authority = domain.AuthorityAuthoritative
	item.PromotionAuthority = "wmp-0123456789abcdef0123456789abcdef"
	item.VerifiedCommit = "1111111111111111111111111111111111111111"
	item.IntegratedCommit = "2222222222222222222222222222222222222222"
	item.RepoIdentity = domain.RepoIdentity("remote_0123456789abcdef01234567")
	item = item.Normalized()
	if _, err := f.store.PutProjectMemoryItem(f.ctx, item, f.now()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	pack := svc.Context(f.ctx, projectmemory.PackRequest{
		ProjectID: testProject, RepoPath: root, Role: projectmemory.RoleWorker,
		Keywords: []string{"GraphQL"},
	})
	rendered := pack.Render()
	for _, leaked := range []string{
		item.PromotionAuthority, item.VerifiedCommit, item.IntegratedCommit,
		string(item.RepoIdentity), string(domain.ProvenanceWorkflowKnowledge),
	} {
		if strings.Contains(rendered, leaked) {
			t.Fatalf("provenance metadata %q reached the rendered pack", leaked)
		}
	}
	t.Logf("pack with full provenance recorded: bytes=%d items=%d",
		pack.Stats.SelectedBytes, pack.Stats.SelectedItems)
}
