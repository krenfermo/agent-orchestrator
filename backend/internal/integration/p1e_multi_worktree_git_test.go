package integration

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// p1e_multi_worktree_git_test.go — P1-E §L and §M, against the real git binary.
//
// The existing real-git suites each prove one property of the lane. What P1-E
// asks for is the shape a master objective actually produces, driven all the way
// through in one test:
//
//	base
//	 |- worktree child A   (independent)
//	 |- worktree child B   (independent)
//	 `- dependent child C  (runs from the integrated result of A and B)
//
// and then the two ways that shape goes wrong in practice: a real merge
// conflict, and an external actor moving the target under a frozen placement.
//
// Every assertion here is made against the REPOSITORY, not against what the
// coordinator reported. "The target contains A, B and C" is a question for git.

// p1eReachable reports whether a ref can reach a commit. It shells out directly
// because "no" is an answer here rather than a test failure.
func p1eReachable(t *testing.T, binary, repo, commit, ref string) bool {
	t.Helper()
	cmd := exec.Command(binary, "-C", repo, "merge-base", "--is-ancestor", commit, ref)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANGUAGE=")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true
	}
	var exit *exec.ExitError
	if ok := asExitError(err, &exit); ok && exit.ExitCode() == 1 {
		return false
	}
	t.Fatalf("merge-base --is-ancestor %s %s: %v: %s", commit, ref, err, out)
	return false
}

func asExitError(err error, target **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok { //nolint:errorlint // exact type is what is wanted
		*target = e
		return true
	}
	return false
}

// §L: three children, two independent and one dependent, all through the real
// lane. The target ends holding all three, no commit is lost, and each child
// worked on its own branch from a base it recorded.
func TestP1E_MasterChildrenIntegrateIntoOneTargetHoldingAllThree(t *testing.T) {
	t.Parallel()
	f := newDepFixture(t, "child-a", "child-b", "child-c")
	ctx := context.Background()
	verifier := &coordVerifier{result: Verification{Passed: true, Summary: "checks ok"}}
	rec := &coordRecorder{}
	coord := f.coordinator(t, verifier, rec)

	// Every child was cut from the SAME base, which is what parallel dispatch
	// produces and what makes the frozen base a real claim rather than a label.
	for _, child := range []string{"child-a", "child-b", "child-c"} {
		if got := integGit(t, f.binary, f.worktrees[child], "rev-parse", "HEAD"); got != f.baseSHA {
			t.Fatalf("%s was cut from %s, want the frozen base %s", child, got, f.baseSHA)
		}
	}
	// And each writes to its own branch. Two children sharing one would make
	// "whose work is this" unanswerable at integration time.
	branches := map[string]bool{}
	for _, child := range []string{"child-a", "child-b", "child-c"} {
		branch := integGit(t, f.binary, f.worktrees[child], "rev-parse", "--abbrev-ref", "HEAD")
		if branches[branch] {
			t.Fatalf("two children share execution branch %q", branch)
		}
		branches[branch] = true
	}

	// A and B touch different files: independent by construction.
	shaA := f.commit(t, "child-a", "alpha.txt", "alpha\n", "child A")
	shaB := f.commit(t, "child-b", "beta.txt", "beta\n", "child B")

	landedA := f.integrate(t, coord, f.request("child-a"))
	landedB := f.integrate(t, coord, f.request("child-b"))

	// A landed first and was not rewritten, so its exact commit must still be
	// reachable. B was cut from the same base and landed second, so the lane
	// replayed it -- a replay is a NEW commit, which is why B's contribution is
	// asserted through the file it added rather than through its original SHA.
	if !p1eReachable(t, f.binary, f.repo, shaA, "refs/heads/main") {
		t.Fatalf("child A's commit %s is no longer reachable from the target", shaA)
	}
	if p1eReachable(t, f.binary, f.repo, shaB, "refs/heads/main") {
		t.Fatalf("child B's pre-replay commit %s is on the target; it should have been replayed", shaB)
	}
	for _, file := range []string{"alpha.txt", "beta.txt"} {
		if integGit(t, f.binary, f.repo, "cat-file", "-e", "main:"+file) != "" {
			t.Fatalf("%s is missing from the target after A and B integrated", file)
		}
	}

	// C depends on both, and may only land onto a target that provably holds
	// each of them -- asserted against the ref, never against what C was told.
	integratedA := landedA.Record.TargetAfterSHA
	integratedB := landedB.Record.TargetAfterSHA
	if !p1eReachable(t, f.binary, f.repo, integratedA, "refs/heads/main") ||
		!p1eReachable(t, f.binary, f.repo, integratedB, "refs/heads/main") {
		t.Fatal("the target does not hold both dependencies' approved commits")
	}

	f.commit(t, "child-c", "gamma.txt", "gamma\n", "child C")
	reqC := f.request("child-c")
	reqC.Dependencies = []Dependency{
		{TaskID: "child-a", IntegratedSHA: integratedA},
		{TaskID: "child-b", IntegratedSHA: integratedB},
	}
	landedC, err := coord.Integrate(ctx, reqC)
	if err != nil {
		t.Fatalf("integrate child-c: %v", err)
	}
	if !landedC.Integrated {
		t.Fatalf("child C did not land: %+v", landedC)
	}

	// §L's acceptance: the target's final content is A + B + C.
	for _, file := range []string{"alpha.txt", "beta.txt", "gamma.txt"} {
		if integGit(t, f.binary, f.repo, "cat-file", "-e", "main:"+file) != "" {
			t.Fatalf("%s is missing from the final target", file)
		}
	}
	if got := f.head(t, "main"); got != landedC.Record.TargetAfterSHA {
		t.Fatalf("main is at %s but the last record says %s", got, landedC.Record.TargetAfterSHA)
	}
	// Each integration recorded the commit its work landed at, which is what
	// authorizes cleaning that child's checkout up -- and the only thing that
	// does.
	for _, landed := range []Outcome{landedA, landedB, landedC} {
		if landed.Record.TargetAfterSHA == "" {
			t.Fatalf("integration of %s recorded no landing commit", landed.Record.TaskID)
		}
	}
}

// §L: a real conflict. It is named with the exact files, the evidence is
// preserved, the target does not move, and it is not retried forever.
func TestP1E_RealConflictIsNamedAndPreservedWithoutRetrying(t *testing.T) {
	t.Parallel()
	f := newDepFixture(t, "child-a", "child-b")
	ctx := context.Background()
	verifier := &coordVerifier{result: Verification{Passed: true}}
	rec := &coordRecorder{}
	coord := f.coordinator(t, verifier, rec)

	// Both children rewrite the SAME line of the same file. Nothing automatic
	// can decide which is right.
	f.commit(t, "child-a", "shared.txt", "written by A\n", "child A")
	f.commit(t, "child-b", "shared.txt", "written by B\n", "child B")

	landedA := f.integrate(t, coord, f.request("child-a"))
	targetAfterA := f.head(t, "main")

	outcome, err := coord.Integrate(ctx, f.request("child-b"))
	if err != nil {
		t.Fatalf("a conflict must be an outcome, not an error: %v", err)
	}
	if outcome.Integrated {
		t.Fatal("a conflicting child was allowed to land")
	}
	if outcome.Attention == nil {
		t.Fatal("a conflict produced no attention record")
	}
	if outcome.Attention.Reason != ReasonMergeConflict {
		t.Fatalf("attention reason = %s, want %s", outcome.Attention.Reason, ReasonMergeConflict)
	}
	// The conflict is NAMED: the exact repository-relative path, not a count.
	if len(outcome.Attention.ConflictFiles) != 1 || outcome.Attention.ConflictFiles[0] != "shared.txt" {
		t.Fatalf("conflict files = %v, want [shared.txt]", outcome.Attention.ConflictFiles)
	}
	// The three commits that describe the situation completely are all there;
	// none of them is re-derivable once the branch moves again.
	if outcome.Attention.BaseSHA == "" || outcome.Attention.TargetSHA == "" || outcome.Attention.SourceSHA == "" {
		t.Fatalf("conflict evidence is incomplete: %+v", outcome.Attention)
	}
	if outcome.Attention.TargetSHA != targetAfterA {
		t.Fatalf("conflict names target %s, but the target is at %s", outcome.Attention.TargetSHA, targetAfterA)
	}

	// The target did not move, and A's landing is untouched.
	if got := f.head(t, "main"); got != targetAfterA {
		t.Fatalf("the target moved to %s during a refused integration", got)
	}
	if !p1eReachable(t, f.binary, f.repo, landedA.Record.TargetAfterSHA, "refs/heads/main") {
		t.Fatal("child A's landed work is no longer reachable from the target")
	}

	// §L: the losing child's worktree is NOT thrown away -- its commits are the
	// only copy of that work, and resolving the conflict needs them.
	if _, err := os.Stat(f.worktrees["child-b"]); err != nil {
		t.Fatalf("the conflicted child's checkout was removed: %v", err)
	}
	if got := integGit(t, f.binary, f.worktrees["child-b"], "rev-parse", "--abbrev-ref", "HEAD"); got != "ao/child-b" {
		t.Fatalf("the conflicted child's branch is %q; its work is not where it was left", got)
	}
	// And it is left clean rather than mid-rebase: a checkout stuck with
	// conflict markers staged is one nobody can read or resume from.
	if status := integGit(t, f.binary, f.worktrees["child-b"], "status", "--porcelain"); status != "" {
		t.Fatalf("the conflicted checkout was left dirty:\n%s", status)
	}

	// §L: no infinite retry. Repeating the identical attempt produces the same
	// refusal and no additional target movement -- it does not spin, and it
	// does not eventually let the conflict through.
	for i := 0; i < 3; i++ {
		again, aerr := coord.Integrate(ctx, f.request("child-b"))
		if aerr != nil {
			t.Fatalf("retry %d errored: %v", i, aerr)
		}
		if again.Integrated {
			t.Fatalf("retry %d let the conflict through", i)
		}
		if again.Attention == nil || again.Attention.Reason != ReasonMergeConflict {
			t.Fatalf("retry %d changed the diagnosis: %+v", i, again.Attention)
		}
		if got := f.head(t, "main"); got != targetAfterA {
			t.Fatalf("retry %d moved the target to %s", i, got)
		}
	}
}

// §M: external drift. Somebody who is not AO advances the target between the
// freeze and the integration. AO must notice, must not reset or overwrite, and
// must land on top of the external work rather than through it.
func TestP1E_ExternalDriftIsDetectedAndNeverOverwritten(t *testing.T) {
	t.Parallel()
	f := newIntegFixture(t)
	ctx := context.Background()

	// The placement froze at base A. The task's work is built on it.
	frozenBase := f.baseSHA
	f.commitInWorktree(t, "feature.txt", "task work\n", "task 1")

	// An external actor -- a colleague pushing, a hotfix, anything not AO --
	// advances the target to B.
	externalB := f.advanceTarget(t, "hotfix.txt", "someone else's urgent fix\n", "external hotfix")
	if externalB == frozenBase {
		t.Fatal("the external commit did not move the target")
	}

	verifier := &coordVerifier{result: Verification{Passed: true, Summary: "revalidated against the moved target"}}
	rec := &coordRecorder{}
	outcome, err := f.coordinator(t, verifier, rec).Integrate(ctx, f.request())
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Integrated {
		t.Fatalf("integration onto a drifted target did not land: %+v", outcome)
	}

	// Drift was DETECTED, not ignored: the record distinguishes what the task
	// was cut from and where the target actually was.
	if outcome.Record.BaseSHA != frozenBase {
		t.Fatalf("record base = %s, want the frozen base %s", outcome.Record.BaseSHA, frozenBase)
	}
	if outcome.Record.TargetBeforeSHA != externalB {
		t.Fatalf("record target-before = %s, want the externally advanced head %s",
			outcome.Record.TargetBeforeSHA, externalB)
	}
	if !outcome.Record.Replayed {
		t.Fatal("drift was not answered with a replay; the work would have landed against a stale base")
	}
	// The work was REVALIDATED against where the target actually is, not
	// carried over on the strength of a verification against the old base.
	if verifier.calls != 1 || !outcome.Record.Verification.Passed {
		t.Fatalf("the drifted target was not revalidated: calls=%d verification=%+v",
			verifier.calls, outcome.Record.Verification)
	}

	// No reset, no force, no silent overwrite: the external commit is still
	// reachable from the target, and the target moved FORWARD from it.
	if !p1eReachable(t, f.binary, f.repo, externalB, "refs/heads/main") {
		t.Fatal("the external commit is no longer reachable; AO overwrote somebody else's work")
	}
	if !p1eReachable(t, f.binary, f.repo, frozenBase, "refs/heads/main") {
		t.Fatal("the frozen base is no longer reachable from the target")
	}
	if got := f.head(t, "main"); got == externalB {
		t.Fatal("the target did not move at all, yet the integration reported success")
	}
	// Both the external file and the task's own file are present. A reset would
	// have removed one of them, and that is exactly what this asserts against.
	for _, file := range []string{"hotfix.txt", "feature.txt"} {
		if integGit(t, f.binary, f.repo, "cat-file", "-e", "main:"+file) != "" {
			t.Fatalf("%s is missing after integrating over external drift", file)
		}
	}
	// The reflog is the other half of "no force push": the target advanced by
	// an update, and the history it had is still there to read.
	reflog := integGit(t, f.binary, f.repo, "reflog", "show", "main", "--format=%H")
	if !strings.Contains(reflog, externalB[:12]) {
		t.Fatalf("the externally advanced head is missing from main's reflog:\n%s", reflog)
	}
}

// §M's second half: drift AO cannot safely replay over stops for a person with
// the exact condition, and leaves the target where the external actor left it.
func TestP1E_UnreplayableExternalDriftStopsWithTheTargetIntact(t *testing.T) {
	t.Parallel()
	f := newIntegFixture(t)
	ctx := context.Background()

	// The task edits a file; the external actor edits the same lines.
	f.commitInWorktree(t, "CHANGELOG.md", "# changelog\n\ntask entry\n", "task 1")
	externalB := f.advanceTarget(t, "CHANGELOG.md", "# changelog\n\nsomeone else's entry\n", "external edit")

	verifier := &coordVerifier{result: Verification{Passed: true}}
	rec := &coordRecorder{}
	outcome, err := f.coordinator(t, verifier, rec).Integrate(ctx, f.request())
	if err != nil {
		t.Fatalf("unreplayable drift must be an outcome, not an error: %v", err)
	}
	if outcome.Integrated {
		t.Fatal("work that cannot be replayed over external drift was allowed to land")
	}
	if outcome.Attention == nil || outcome.Attention.Reason != ReasonMergeConflict {
		t.Fatalf("attention = %+v, want a named merge conflict", outcome.Attention)
	}
	if len(outcome.Attention.ConflictFiles) == 0 {
		t.Fatal("the refusal did not name the conflicted file")
	}
	// The external actor's commit is exactly where they left it.
	if got := f.head(t, "main"); got != externalB {
		t.Fatalf("main = %s, want the external head %s untouched", got, externalB)
	}
	if content := integRead(t, f.repo, "CHANGELOG.md"); !strings.Contains(content, "someone else's entry") {
		t.Fatalf("the external actor's content was overwritten:\n%s", content)
	}
}
