package workflow

import (
	stdctx "context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// task_promotion_proof_internal_test.go — P2-D §31.
//
// Every case here is a way P2-C's promotion could say "canonical" about work
// the repository does not have. They are grouped by the two placements,
// because the two have entirely different proofs and the completion bar names
// them separately:
//
//	WORKTREE   verified is not integrated. Only a durable integration record,
//	           whose target head AO can still reach, licenses canonical.
//	DIRECT     the commit is on the project's own branch, but only if AO
//	           recorded that it verified there AND the commit is still in the
//	           branch's history AND the repository is still the same one.
//
// The single most important assertion in this file is
// TestWorktreeVerifiedButNotIntegratedNeverPromotes: before P2-D, a non-empty
// SHA argument was the whole proof.

// --- fakes ------------------------------------------------------------------

// staticWorkspaceFacts observes one repository at one head, which is all the
// direct-branch proof needs from the workspace port.
type staticWorkspaceFacts struct {
	head string
	err  error
}

func (f staticWorkspaceFacts) ObserveWorkspace(_ stdctx.Context, _ ports.WorkspaceInfo) (ports.WorkspaceObservation, error) {
	if f.err != nil {
		return ports.WorkspaceObservation{}, f.err
	}
	return ports.WorkspaceObservation{HeadSHA: f.head}, nil
}

// gitRepoWithHistory builds a real repository with n commits and returns its
// path and every SHA, oldest first.
//
// Real git rather than a fake, because the two properties under test are git
// properties: "the verified commit is still an ancestor of HEAD" and "the
// history was rewritten". A fake ancestry oracle would let this file pass while
// the production check did the opposite.
func gitRepoWithHistory(t *testing.T, commits int) (string, []string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_EDITOR=true",
			"GIT_AUTHOR_NAME=ao", "GIT_AUTHOR_EMAIL=ao@example.test",
			"GIT_COMMITTER_NAME=ao", "GIT_COMMITTER_EMAIL=ao@example.test")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	var shas []string
	for i := range commits {
		name := filepath.Join(dir, "f"+string(rune('a'+i))+".txt")
		if err := os.WriteFile(name, []byte("v"+string(rune('0'+i))), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		run("add", ".")
		run("commit", "-q", "-m", "c"+string(rune('0'+i)))
		shas = append(shas, run("rev-parse", "HEAD"))
	}
	return dir, shas
}

// identityOf is the durable identity the promotion proofs compare against.
//
// The fixtures record it explicitly because that is what production does:
// every mutation-provenance row is stamped with the identity of the repository
// it was observed in. A fixture that left it blank would exercise the
// "unidentifiable" path instead of the one under test -- which is itself worth
// testing, and is TestPromotionRefusesADifferentRepositoryAtTheSamePath's job.
func identityOf(t *testing.T, dir string) domain.RepoIdentity {
	t.Helper()
	return projectmemory.RepoIdentityOf(stdctx.Background(), dir)
}

// directBranchProject is a Projects whose one project resolves to a real
// repository and is configured for direct-branch execution.
type directBranchProject struct{ id, path string }

func (p directBranchProject) GetProject(_ stdctx.Context, id string) (domain.ProjectRecord, bool, error) {
	if id != p.id {
		return domain.ProjectRecord{}, false, nil
	}
	return domain.ProjectRecord{
		ID: p.id, Path: p.path,
		Kind:   domain.ProjectKindSingleRepo,
		Config: domain.ProjectConfig{ExecutionMode: domain.ExecutionDirectBranch},
	}, true, nil
}

// --- worktree promotion -----------------------------------------------------

// TestWorktreeVerifiedButNotIntegratedNeverPromotes is the completion bar's
// "a worktree not integrated cannot demonstrate canonical authority".
//
// The caller passes a perfectly plausible SHA -- which is exactly what
// finishTaskWorktree used to accept as proof -- and AO holds no record of an
// integration. The refusal must be explicit and must name its reason, so the
// operator sees "AO holds no record that this task's worktree was ever
// integrated" instead of an unexplained absence.
func TestWorktreeVerifiedButNotIntegratedNeverPromotes(t *testing.T) {
	repo, shas := gitRepoWithHistory(t, 2)
	c := &Coordinator{
		mutationProvenance: &fakeMutationProvenance{rows: []domain.WorkflowMutationProvenance{{
			ID: "wmp-verified", TaskID: "task-1", Boundary: domain.BoundaryVerified, HeadSHA: shas[1],
			RepoIdentity: identityOf(t, repo),
		}}},
		projects: fakeProjectsAt("proj-1", repo),
	}
	proof := c.worktreePromotionProof(stdctx.Background(), "task-1", repo, shas[1])
	if proof.Provable {
		t.Fatal("a verified-but-unintegrated worktree proved canonical authority")
	}
	if proof.ReasonClass != domain.ReasonPromotionUnprovable {
		t.Fatalf("reason class = %q, want %q", proof.ReasonClass, domain.ReasonPromotionUnprovable)
	}
	if !strings.Contains(proof.Detail, "integrated") {
		t.Fatalf("the refusal does not say what was missing: %q", proof.Detail)
	}
}

// TestWorktreeMergeIntegrationPromotesOnlyWhileReachable covers the two halves
// of the ancestry proof at once.
//
// A recorded fast-forward whose source commit is still reachable from the
// target head is a real integration. The SAME record, once the branch has been
// rewritten so that commit is no longer reachable, is not: the work AO recorded
// as landed is no longer in the history, and knowledge pinned to it describes a
// repository state that no longer exists.
func TestWorktreeMergeIntegrationPromotesOnlyWhileReachable(t *testing.T) {
	repo, shas := gitRepoWithHistory(t, 3)
	rec := domain.WorkflowMutationProvenance{
		ID: "wmp-int", TaskID: "task-1", Boundary: domain.BoundaryIntegrated,
		Placement:                 domain.MutationPlacementIsolatedWorktree,
		HeadSHA:                   shas[1],
		IntegrationTargetRef:      "refs/heads/main",
		IntegrationTargetAfterSHA: shas[2],
		IntegrationMethod:         domain.IntegrationFastForward,
		RepoIdentity:              identityOf(t, repo),
	}
	c := &Coordinator{
		mutationProvenance: &fakeMutationProvenance{rows: []domain.WorkflowMutationProvenance{rec}},
		projects:           fakeProjectsAt("proj-1", repo),
	}
	proof := c.worktreePromotionProof(stdctx.Background(), "task-1", repo, shas[2])
	if !proof.Provable {
		t.Fatalf("a recorded, still-reachable integration was refused: [%s] %s", proof.ReasonClass, proof.Detail)
	}
	if proof.IntegratedCommit != shas[2] || proof.VerifiedCommit != shas[1] {
		t.Fatalf("proof pinned the wrong commits: %+v", proof)
	}
	if proof.MutationProvenanceID != "wmp-int" {
		t.Fatalf("proof names %q as its authority, want the integration record", proof.MutationProvenanceID)
	}

	// Now the same record against a rewritten history: the integrated source
	// is an orphan, so ancestry fails and the promotion is refused.
	orphan := rec
	orphan.HeadSHA = "0000000000000000000000000000000000000000"
	c.mutationProvenance = &fakeMutationProvenance{rows: []domain.WorkflowMutationProvenance{orphan}}
	rewritten := c.worktreePromotionProof(stdctx.Background(), "task-1", repo, shas[2])
	if rewritten.Provable {
		t.Fatal("an integration whose commit is no longer reachable still proved canonical authority")
	}
}

// TestWorktreeCherryPickPromotesOnRecordedTargetsAlone is the one method
// ancestry cannot prove.
//
// A cherry-pick produces DIFFERENT commit SHAs for the same content, so the
// source is legitimately unreachable from the target. Applying the ancestry
// check here would refuse every honest cherry-pick; not recording the method
// would mean the caller had to remember that. The method carries it.
func TestWorktreeCherryPickPromotesOnRecordedTargetsAlone(t *testing.T) {
	repo, shas := gitRepoWithHistory(t, 2)
	c := &Coordinator{
		mutationProvenance: &fakeMutationProvenance{rows: []domain.WorkflowMutationProvenance{{
			ID: "wmp-cp", TaskID: "task-1", Boundary: domain.BoundaryIntegrated,
			// A SHA that is deliberately NOT in this repository, exactly as a
			// cherry-picked source branch's tip would be after the pick.
			HeadSHA:                   "1111111111111111111111111111111111111111",
			IntegrationTargetAfterSHA: shas[1],
			IntegrationMethod:         domain.IntegrationCherryPick,
			RepoIdentity:              identityOf(t, repo),
		}}},
		projects: fakeProjectsAt("proj-1", repo),
	}
	proof := c.worktreePromotionProof(stdctx.Background(), "task-1", repo, shas[1])
	if !proof.Provable {
		t.Fatalf("a recorded cherry-pick was refused: [%s] %s", proof.ReasonClass, proof.Detail)
	}
	if proof.Method != domain.IntegrationCherryPick {
		t.Fatalf("method = %q, want cherry_pick recorded on the proof", proof.Method)
	}
}

// TestCallerAndRecordDisagreeingRefusesPromotion is the "do not trust the
// argument" rule stated as a test.
//
// When the caller's idea of where the work landed and AO's own record disagree,
// one of them describes an integration that is not this one. Preferring the row
// silently would hide that; refusing surfaces it.
func TestCallerAndRecordDisagreeingRefusesPromotion(t *testing.T) {
	repo, shas := gitRepoWithHistory(t, 2)
	c := &Coordinator{
		mutationProvenance: &fakeMutationProvenance{rows: []domain.WorkflowMutationProvenance{{
			ID: "wmp-int", TaskID: "task-1", Boundary: domain.BoundaryIntegrated,
			HeadSHA:                   shas[0],
			IntegrationTargetAfterSHA: shas[1],
			IntegrationMethod:         domain.IntegrationCherryPick,
			RepoIdentity:              identityOf(t, repo),
		}}},
		projects: fakeProjectsAt("proj-1", repo),
	}
	proof := c.worktreePromotionProof(stdctx.Background(), "task-1", repo,
		"2222222222222222222222222222222222222222")
	if proof.Provable {
		t.Fatal("a promotion was proven despite the caller and the record naming different heads")
	}
}

// --- direct-branch promotion ------------------------------------------------

// TestDirectBranchPromotionRequiresARecordedVerifiedBoundary is the completion
// bar's "direct branch promotion has proof".
//
// Before P2-D, direct-branch work was canonical because the project's execution
// MODE said direct branch. Configuration is not evidence, so with no recorded
// verified boundary the promotion is refused.
func TestDirectBranchPromotionRequiresARecordedVerifiedBoundary(t *testing.T) {
	repo, shas := gitRepoWithHistory(t, 1)
	c := &Coordinator{
		mutationProvenance: &fakeMutationProvenance{},
		projects:           directBranchProject{id: "proj-1", path: repo},
		workspaceFacts:     staticWorkspaceFacts{head: shas[0]},
	}
	proof := c.directBranchPromotionProof(stdctx.Background(),
		domain.WorkflowRun{ID: "wf-1", ProjectID: "proj-1"}, "task-1", repo)
	if proof.Provable {
		t.Fatal("direct-branch work was promoted on the strength of the project's execution mode alone")
	}
	if proof.ReasonClass != domain.ReasonProvenanceMissing {
		t.Fatalf("reason class = %q, want %q", proof.ReasonClass, domain.ReasonProvenanceMissing)
	}
}

// TestDirectBranchPromotionSurvivesALaterUnrelatedCommit is P2-D §14's
// "the branch advanced after verify, before promotion".
//
// A branch growing on top of the verified commit is normal and safe: another
// task landed, or a person committed, and neither makes what THIS task learned
// untrue. Requiring HEAD to still EQUAL the verified commit would refuse every
// healthy repository; requiring ancestry accepts growth and refuses rewrites.
func TestDirectBranchPromotionSurvivesALaterUnrelatedCommit(t *testing.T) {
	repo, shas := gitRepoWithHistory(t, 3)
	c := &Coordinator{
		mutationProvenance: &fakeMutationProvenance{rows: []domain.WorkflowMutationProvenance{{
			ID: "wmp-verified", TaskID: "task-1", Boundary: domain.BoundaryVerified,
			Placement: domain.MutationPlacementDirectBranch, HeadSHA: shas[0],
			RepoIdentity: identityOf(t, repo),
		}}},
		projects: directBranchProject{id: "proj-1", path: repo},
		// HEAD has moved two commits past the verified one.
		workspaceFacts: staticWorkspaceFacts{head: shas[2]},
	}
	proof := c.directBranchPromotionProof(stdctx.Background(),
		domain.WorkflowRun{ID: "wf-1", ProjectID: "proj-1"}, "task-1", repo)
	if !proof.Provable {
		t.Fatalf("a branch that merely advanced blocked promotion: [%s] %s", proof.ReasonClass, proof.Detail)
	}
	if proof.VerifiedCommit != shas[0] || proof.IntegratedCommit != shas[0] {
		t.Fatalf("the proof pinned knowledge to the wrong commit: %+v", proof)
	}
}

// TestDirectBranchPromotionRefusesARewrittenBranch is the other side: a
// verified commit that has been amended, reset or rebased away.
//
// The knowledge would describe work the repository no longer contains, so the
// promotion is refused and named as source drift rather than as a missing row.
func TestDirectBranchPromotionRefusesARewrittenBranch(t *testing.T) {
	repo, shas := gitRepoWithHistory(t, 2)
	c := &Coordinator{
		mutationProvenance: &fakeMutationProvenance{rows: []domain.WorkflowMutationProvenance{{
			ID: "wmp-verified", TaskID: "task-1", Boundary: domain.BoundaryVerified,
			Placement: domain.MutationPlacementDirectBranch,
			// Verified at a commit that is not in this history at all, which is
			// what a reset or an amend leaves behind.
			HeadSHA:      "3333333333333333333333333333333333333333",
			RepoIdentity: identityOf(t, repo),
		}}},
		projects:       directBranchProject{id: "proj-1", path: repo},
		workspaceFacts: staticWorkspaceFacts{head: shas[1]},
	}
	proof := c.directBranchPromotionProof(stdctx.Background(),
		domain.WorkflowRun{ID: "wf-1", ProjectID: "proj-1"}, "task-1", repo)
	if proof.Provable {
		t.Fatal("a verified commit no longer on the branch still licensed canonical knowledge")
	}
	if proof.ReasonClass != domain.ReasonSourceDrift {
		t.Fatalf("reason class = %q, want %q", proof.ReasonClass, domain.ReasonSourceDrift)
	}
}

// TestDirectBranchPromotionRefusesAWorktreePlacement keeps the two proofs from
// leaking into each other. An isolated-worktree task must never reach the
// direct-branch proof, whatever else is on record for it.
func TestDirectBranchPromotionRefusesAWorktreePlacement(t *testing.T) {
	repo, shas := gitRepoWithHistory(t, 1)
	c := &Coordinator{
		mutationProvenance: &fakeMutationProvenance{rows: []domain.WorkflowMutationProvenance{{
			ID: "wmp-verified", TaskID: "task-1", Boundary: domain.BoundaryVerified, HeadSHA: shas[0],
		}}},
		// fakeProjectsAt yields a project with no execution mode set, which
		// resolves to isolated worktree.
		projects:       fakeProjectsAt("proj-1", repo),
		workspaceFacts: staticWorkspaceFacts{head: shas[0]},
	}
	proof := c.directBranchPromotionProof(stdctx.Background(),
		domain.WorkflowRun{ID: "wf-1", ProjectID: "proj-1"}, "task-1", repo)
	if proof.Provable {
		t.Fatal("an isolated-worktree task was promoted through the direct-branch proof")
	}
}

// TestPromotionRefusesADifferentRepositoryAtTheSamePath is P2-D §9 at the
// promotion boundary.
//
// The mutation was recorded against one repository identity; the checkout now
// at that path identifies as another. Nothing about the SHAs has to be wrong
// for this to be the wrong project's knowledge.
func TestPromotionRefusesADifferentRepositoryAtTheSamePath(t *testing.T) {
	repo, shas := gitRepoWithHistory(t, 2)
	c := &Coordinator{
		mutationProvenance: &fakeMutationProvenance{rows: []domain.WorkflowMutationProvenance{{
			ID: "wmp-int", TaskID: "task-1", Boundary: domain.BoundaryIntegrated,
			HeadSHA:                   shas[0],
			IntegrationTargetAfterSHA: shas[1],
			IntegrationMethod:         domain.IntegrationCherryPick,
			// A repository AO can identify, and which is not this one.
			RepoIdentity: domain.RepoIdentity("remote_someotherproject"),
		}}},
		projects: fakeProjectsAt("proj-1", repo),
	}
	proof := c.worktreePromotionProof(stdctx.Background(), "task-1", repo, shas[1])
	if proof.Provable {
		t.Fatal("a different repository at the same path inherited another project's promotion")
	}
	if proof.ReasonClass != domain.ReasonRepoIdentityChanged {
		t.Fatalf("reason class = %q, want %q", proof.ReasonClass, domain.ReasonRepoIdentityChanged)
	}
}
