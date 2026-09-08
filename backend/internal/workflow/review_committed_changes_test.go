package workflow_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// review_committed_changes_test.go -- the P5-A phase 2 defect, and every way
// the fix has to fail closed.
//
// THE DEFECT. A real smoke (wf-a8dffdf6) ran a worker that did its job and
// COMMITTED it. That left the worktree clean, ReviewPolicy read its changed
// files from the dirty tree alone, saw none, and recorded
// `no_changed_files_observed` -- so a change that added a function and a test
// was classified as a change AO could not see, and the fast Task path was
// unreachable for any worker that commits.
//
// These tests run against REAL git repositories, through the real
// `git diff --name-only base..head`, because the whole claim of the fix is that
// the set is derived from verifiable git evidence rather than asserted. A fake
// git port would let the fix pass by agreeing with itself.

// gitRepo is a real repository with one commit, standing in for the branch a
// worker is handed.
func gitRepo(t *testing.T) (dir, head string) {
	t.Helper()
	dir = t.TempDir()
	for _, args := range [][]string{
		{"init", "--initial-branch=ao/wf"},
		{"config", "user.email", "ao@example.test"},
		{"config", "user.name", "AO Fixture"},
		{"config", "commit.gpgsign", "false"},
	} {
		run := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := run.CombinedOutput(); err != nil {
			t.Skipf("git unavailable for %v: %v (%s)", args, err, out)
		}
	}
	return dir, gitCommit(t, dir, "README.md", "the branch before the task\n", "base")
}

// gitWrite puts a file in the worktree without committing it: the dirty half.
func gitWrite(t *testing.T, dir, path, content string) {
	t.Helper()
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// gitCommit is the committed half: what a worker that finishes its work
// properly leaves behind, and what the old change-file source could not see.
func gitCommit(t *testing.T, dir, path, content, message string) string {
	t.Helper()
	gitWrite(t, dir, path, content)
	for _, args := range [][]string{
		{"add", "-A"},
		{"commit", "-q", "-m", message},
	} {
		run := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := run.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	return gitHead(t, dir)
}

// gitRun runs an arbitrary git command, for the tests that have to rewrite
// history to prove the fix refuses to diff against it.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
}

// realRepoRun drives one run to the point where ReviewPolicy has decided,
// against a real repository.
//
// baseHead is the worktree HEAD when the work is dispatched -- the base the
// task starts from. deliveredHead is the HEAD when AO observes the delivery,
// which is a LATER commit whenever the worker committed. dirty is whatever the
// worktree is still holding uncommitted at that moment. Splitting the two
// heads is the whole point: the fixture that could not express it is the one
// that made the defect invisible.
type realRepoRun struct {
	c      *workflowcore.Coordinator
	store  *fakeStore
	runID  string
	detail workflowcore.RunDetail
}

func runAgainstRealRepo(
	t *testing.T, dir, objective, baseHead, deliveredHead string,
	dirty []ports.WorkspaceChange, runner workflowcore.VerifyRunner,
) realRepoRun {
	t.Helper()
	ctx := context.Background()
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{
		rec:   domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: dir}},
		facts: sessionFacts,
	}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReviewAndVerifier(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher, runner)

	created, err := c.CreateRun(ctx, "proj-1", objective, verifiablePlan())
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Dispatch sees the branch as it was handed over: this is the base.
	workspaceFacts.obs = ports.WorkspaceObservation{Path: dir, Branch: "ao/wf", HeadSHA: baseHead}
	detail, err := c.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	work := workStepFrom(detail)
	sessionFacts.put(domain.SessionRecord{
		ID: domain.SessionID(*work.Step.SessionID), ProjectID: "proj-1",
		Activity: domain.Activity{State: domain.ActivityIdle}, IsTerminated: false,
		Metadata: domain.SessionMetadata{WorkspacePath: dir, Branch: "ao/wf"},
	})

	// Delivery sees whatever the worker actually left: commits, dirt, or both.
	workspaceFacts.obs = ports.WorkspaceObservation{
		Path: dir, Branch: "ao/wf", HeadSHA: deliveredHead,
		Dirty: len(dirty) > 0, Changes: dirty,
	}
	clk.Advance(10 * time.Second)
	if _, err := c.GetRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	got, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	return realRepoRun{c: c, store: store, runID: created.Run.ID, detail: got}
}

// policyFor reads back the single durable review_policy_decision.
func (r realRepoRun) policyFor(t *testing.T) workflowcore.ReviewPolicyDecision {
	t.Helper()
	cps := checkpointsWithPhase(t, r.store, r.runID, "review_policy_decision")
	if len(cps) != 1 {
		t.Fatalf("recorded %d policy decisions, want exactly 1", len(cps))
	}
	decision, ok := workflowcore.DecodeReviewPolicyDecisionForTest(cps[0].RetryState)
	if !ok {
		t.Fatalf("undecodable policy decision: %s", cps[0].RetryState)
	}
	return decision
}

// THE HEADLINE, and the exact shape of the smoke that found the defect: the
// worker committed its work and left a clean tree. AO must see two changed
// files, not none, and the fast Task path must therefore be reachable.
func TestAWorkerThatCommittedItsWorkIsNotSeenAsAWorkerThatDidNone(t *testing.T) {
	dir, base := gitRepo(t)
	gitCommit(t, dir, "pkg/helper.go", "package pkg\n\nfunc Helper() {}\n", "add helper")
	head := gitCommit(t, dir, "pkg/helper_test.go", "package pkg\n", "test the helper")

	run := runAgainstRealRepo(t, dir, "rename a helper", base, head, nil, passingRunner())
	decision := run.policyFor(t)

	if decision.Facts.ChangedFileCount != 2 {
		t.Fatalf("changed files = %d (%v), want the two committed files",
			decision.Facts.ChangedFileCount, decision.Facts.ChangedFilePaths)
	}
	if decision.Facts.CommittedChangedFileCount != 2 {
		t.Fatalf("committed count = %d, want 2 -- the set must say where each path came from",
			decision.Facts.CommittedChangedFileCount)
	}
	if decision.Facts.ChangedFilesSource != "base_diff_and_worktree" {
		t.Fatalf("source = %q, want the base..head diff", decision.Facts.ChangedFilesSource)
	}
	if decision.Facts.ChangeSetBaseSHA != base || decision.Facts.ChangeSetHeadSHA != head {
		t.Fatalf("the decision does not name a re-derivable diff: %s..%s",
			decision.Facts.ChangeSetBaseSHA, decision.Facts.ChangeSetHeadSHA)
	}
	if decision.Facts.ChangeSetBaseFrom != "worker_dispatched" {
		t.Fatalf("base provenance = %q, want the dispatch checkpoint", decision.Facts.ChangeSetBaseFrom)
	}
	// The defect, named: this is the reason that must NOT appear.
	if hasReason(decision.Reasons, workflowcore.ReasonNoChangedFiles) {
		t.Fatalf("committed work was still classified as no changed files: %v", decision.Reasons)
	}
	if hasReason(decision.Reasons, workflowcore.ReasonUnprovableChangeSet) {
		t.Fatalf("a provable change set was called unprovable: %v", decision.Reasons)
	}
	// And the consequence the phase exists for: no independent reviewer.
	if id := reviewStepFrom(run.detail).Step.ReviewRunID; id != nil {
		t.Fatalf("a reviewer ran for committed low-risk work with observed evidence (review run %q)", *id)
	}
}

// The other half must not regress: a worker that delivered WITHOUT committing
// is still read from the dirty tree, and that answer is proven, not a fallback.
func TestUncommittedWorkIsStillTheWholeChangeSet(t *testing.T) {
	dir, base := gitRepo(t)
	gitWrite(t, dir, "pkg/helper.go", "package pkg\n")

	run := runAgainstRealRepo(t, dir, "rename a helper", base, base,
		[]ports.WorkspaceChange{{Path: "pkg/helper.go", Status: " M"}}, passingRunner())
	decision := run.policyFor(t)

	if decision.Facts.ChangedFilesSource != "worktree_only" {
		t.Fatalf("source = %q, want worktree_only when nothing was committed", decision.Facts.ChangedFilesSource)
	}
	if decision.Facts.ChangedFileCount != 1 || decision.Facts.ChangedFilePaths[0] != "pkg/helper.go" {
		t.Fatalf("changed files = %v, want the one dirty file", decision.Facts.ChangedFilePaths)
	}
	if decision.Facts.CommittedChangedFileCount != 0 {
		t.Fatalf("committed count = %d, want 0", decision.Facts.CommittedChangedFileCount)
	}
	if hasReason(decision.Reasons, workflowcore.ReasonUnprovableChangeSet) {
		t.Fatalf("an uncommitted change was called unprovable: %v", decision.Reasons)
	}
}

// Both halves at once, which is what a worker that commits and then keeps
// editing actually leaves. The union is deduplicated: a file that was
// committed AND is dirty again is one changed file, not two.
func TestCommittedAndUncommittedWorkAreOneDeduplicatedSet(t *testing.T) {
	dir, base := gitRepo(t)
	head := gitCommit(t, dir, "pkg/helper.go", "package pkg\n\nfunc Helper() {}\n", "add helper")
	gitWrite(t, dir, "pkg/helper.go", "package pkg\n\nfunc Helper() int { return 1 }\n")
	gitWrite(t, dir, "pkg/extra.go", "package pkg\n")

	run := runAgainstRealRepo(t, dir, "rename a helper", base, head, []ports.WorkspaceChange{
		{Path: "pkg/helper.go", Status: " M"},
		{Path: "pkg/extra.go", Status: "??"},
	}, passingRunner())
	decision := run.policyFor(t)

	want := []string{"pkg/extra.go", "pkg/helper.go"}
	if strings.Join(decision.Facts.ChangedFilePaths, ",") != strings.Join(want, ",") {
		t.Fatalf("changed files = %v, want %v (sorted, deduplicated)", decision.Facts.ChangedFilePaths, want)
	}
	if decision.Facts.CommittedChangedFileCount != 1 {
		t.Fatalf("committed count = %d, want 1", decision.Facts.CommittedChangedFileCount)
	}
}

// Sensitive-path classification must run over the COMPLETE set, committed
// included. This is the case where the defect was dangerous rather than merely
// wasteful: a committed migration that no longer reached the classifier would
// have looked like ordinary work.
func TestASensitiveFileThatWasCommittedStillForcesAReview(t *testing.T) {
	dir, base := gitRepo(t)
	head := gitCommit(t, dir, "backend/migrations/0200_add_column.sql",
		"ALTER TABLE sessions ADD COLUMN note TEXT;\n", "add a column")

	run := runAgainstRealRepo(t, dir, "add a column", base, head, nil, passingRunner())
	decision := run.policyFor(t)

	if decision.Decision != workflowcore.ReviewRequired {
		t.Fatalf("decision = %q, want required for a committed migration", decision.Decision)
	}
	if !hasReason(decision.Reasons, workflowcore.ReasonMigrationOrSchemaPath) {
		t.Fatalf("reasons = %v, want the committed migration to be classified", decision.Reasons)
	}
	if id := reviewStepFrom(run.detail).Step.ReviewRunID; id == nil {
		t.Fatal("no reviewer ran for a committed schema migration")
	}
	depths := depthDecisionsFor(t, run.store, run.runID)
	if len(depths) != 1 || depths[0].Effective != domain.ReviewDepthDeep {
		t.Fatalf("depth = %+v, want deep for a high-risk committed path", depths)
	}
}

// A base AO cannot find is a base AO must not diff against. The set becomes
// unprovable -- NOT empty -- the review is preserved, and the decision says
// which base it could not resolve.
func TestABaseThatIsNotInTheCheckoutFailsClosed(t *testing.T) {
	dir, _ := gitRepo(t)
	head := gitCommit(t, dir, "pkg/helper.go", "package pkg\n", "add helper")
	// A well-formed SHA that names nothing: the shape of a base whose commit
	// was garbage-collected or never existed in this checkout.
	missing := "0123456789abcdef0123456789abcdef01234567"

	run := runAgainstRealRepo(t, dir, "rename a helper", missing, head, nil, passingRunner())
	decision := run.policyFor(t)

	if !decision.Facts.ChangedFilesUnprovable {
		t.Fatalf("an unresolvable base produced a provable set: %+v", decision.Facts)
	}
	if !hasReason(decision.Reasons, workflowcore.ReasonUnprovableChangeSet) {
		t.Fatalf("reasons = %v, want unprovable_change_set", decision.Reasons)
	}
	// Not the empty-set reason: "nothing changed" and "AO cannot tell" must
	// stay distinguishable forever.
	if hasReason(decision.Reasons, workflowcore.ReasonNoChangedFiles) {
		t.Fatalf("an unprovable set was reported as an empty one: %v", decision.Reasons)
	}
	if decision.Decision != workflowcore.ReviewRequired {
		t.Fatalf("decision = %q, want required", decision.Decision)
	}
	if id := reviewStepFrom(run.detail).Step.ReviewRunID; id == nil {
		t.Fatal("no reviewer ran for a change set AO could not prove")
	}
	if !strings.Contains(decision.Facts.UnprovableChangeSetReason, "not present in this checkout") {
		t.Fatalf("the decision does not explain itself: %q", decision.Facts.UnprovableChangeSetReason)
	}
}

// A base that IS a commit but is no longer an ancestor of the head -- a rebase,
// a reset, a force-push -- describes a change that never happened. Refused.
func TestARewrittenHistoryFailsClosed(t *testing.T) {
	dir, base := gitRepo(t)
	// An unrelated line of history: a real commit that this branch does not
	// contain, exactly as a rebased base would be.
	gitRun(t, dir, "checkout", "-q", "--orphan", "rewritten")
	orphanBase := gitCommit(t, dir, "OTHER.md", "another history\n", "orphan base")
	gitRun(t, dir, "checkout", "-q", "ao/wf")
	if gitHead(t, dir) != base {
		t.Fatalf("fixture did not return to the task branch")
	}
	head := gitCommit(t, dir, "pkg/helper.go", "package pkg\n", "add helper")

	run := runAgainstRealRepo(t, dir, "rename a helper", orphanBase, head, nil, passingRunner())
	decision := run.policyFor(t)

	if !decision.Facts.ChangedFilesUnprovable {
		t.Fatalf("a rewritten history produced a provable set: %+v", decision.Facts)
	}
	if !strings.Contains(decision.Facts.UnprovableChangeSetReason, "no longer an ancestor") {
		t.Fatalf("reason = %q, want the ancestry failure named", decision.Facts.UnprovableChangeSetReason)
	}
	if id := reviewStepFrom(run.detail).Step.ReviewRunID; id == nil {
		t.Fatal("no reviewer ran despite a base that is not in this branch's history")
	}
}

// Commits that were already on the branch when the task started are not the
// task's. The diff begins at the base, so they are never attributed to the
// worker -- and the count of committed paths says exactly how many were.
func TestCommitsMadeBeforeTheTaskAreNotAttributedToIt(t *testing.T) {
	dir, _ := gitRepo(t)
	gitCommit(t, dir, "legacy/one.go", "package legacy\n", "somebody else's work")
	base := gitCommit(t, dir, "legacy/two.go", "package legacy\n", "somebody else's work again")
	head := gitCommit(t, dir, "pkg/helper.go", "package pkg\n", "the task's own commit")

	run := runAgainstRealRepo(t, dir, "rename a helper", base, head, nil, passingRunner())
	decision := run.policyFor(t)

	want := []string{"pkg/helper.go"}
	if strings.Join(decision.Facts.ChangedFilePaths, ",") != strings.Join(want, ",") {
		t.Fatalf("changed files = %v, want only the task's own commit", decision.Facts.ChangedFilePaths)
	}
}

// And the fix must not manufacture a change where there is none: a run whose
// worker committed nothing and left nothing dirty still records the empty set,
// with the reason it always had.
func TestARunThatChangedNothingStillSaysSo(t *testing.T) {
	dir, base := gitRepo(t)

	run := runAgainstRealRepo(t, dir, "rename a helper", base, base, nil, passingRunner())
	decision := run.policyFor(t)

	if decision.Facts.ChangedFileCount != 0 {
		t.Fatalf("changed files = %v, want none", decision.Facts.ChangedFilePaths)
	}
	if !hasReason(decision.Reasons, workflowcore.ReasonNoChangedFiles) {
		t.Fatalf("reasons = %v, want no_changed_files_observed", decision.Reasons)
	}
	if decision.Decision != workflowcore.ReviewRequired {
		t.Fatalf("decision = %q, want required", decision.Decision)
	}
}
