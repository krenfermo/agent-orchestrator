package integration

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests run against real git for the same reason the rest of
// coordinator_git_test.go does: every property here -- what a rebase does to a
// commit the target already contains, which commits are reachable from a ref,
// what the resulting diff is -- is a property of git, and a fake would only
// assert that this file's author agreed with themselves.

// depFixture is a project with several tasks in flight at once: one repository,
// one target branch, and an AO-owned worktree per task, all cut from the same
// base. It is the shape parallel dispatch actually produces.
type depFixture struct {
	binary    string
	repo      string
	target    string
	baseSHA   string
	worktrees map[string]string
}

func newDepFixture(t *testing.T, tasks ...string) *depFixture {
	t.Helper()
	binary := integRequireGit(t)
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o750); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	integGit(t, binary, repo, "init", "--initial-branch=main")
	integGit(t, binary, repo, "config", "user.email", "ao@example.com")
	integGit(t, binary, repo, "config", "user.name", "Ao Agents")
	integWrite(t, repo, "README.md", "seed\n")
	integGit(t, binary, repo, "add", ".")
	integGit(t, binary, repo, "commit", "-m", "seed")
	base := integGit(t, binary, repo, "rev-parse", "HEAD")

	f := &depFixture{binary: binary, repo: repo, target: "main", baseSHA: base, worktrees: map[string]string{}}
	for _, task := range tasks {
		path := filepath.Join(root, "worktrees", task)
		integGit(t, binary, repo, "worktree", "add", "-q", "-b", "ao/"+task, path, base)
		f.worktrees[task] = path
	}
	return f
}

// commit adds one of a task's own commits in its own worktree.
func (f *depFixture) commit(t *testing.T, task, name, content, message string) string {
	t.Helper()
	wt := f.worktrees[task]
	integWrite(t, wt, name, content)
	integGit(t, f.binary, wt, "add", name)
	integGit(t, f.binary, wt, "commit", "-m", message)
	return integGit(t, f.binary, wt, "rev-parse", "HEAD")
}

func (f *depFixture) head(t *testing.T, rev string) string {
	t.Helper()
	return integGit(t, f.binary, f.repo, "rev-parse", rev)
}

// contains reports whether the target branch can reach commit. It shells out
// itself rather than through integGit because "no" is an answer here, not a
// test failure.
func (f *depFixture) contains(t *testing.T, commit string) bool {
	t.Helper()
	cmd := exec.Command(f.binary, "-C", f.repo, "merge-base", "--is-ancestor", commit, "refs/heads/"+f.target)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANGUAGE=")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false
	}
	t.Fatalf("merge-base --is-ancestor: %v: %s", err, out)
	return false
}

func (f *depFixture) request(task string) Request {
	return Request{
		ProjectID:     "prj-1",
		WorkflowRunID: "wf-1",
		TaskID:        task,
		RepoPath:      f.repo,
		WorktreePath:  f.worktrees[task],
		TargetBranch:  f.target,
		SourceBranch:  "ao/" + task,
		BaseSHA:       f.baseSHA,
		Readiness:     Readiness{Review: ReviewApproved, Verify: VerifyPassed},
	}
}

func (f *depFixture) coordinator(t *testing.T, verifier Verifier, rec Recorder) *Coordinator {
	t.Helper()
	c, err := New(Deps{Git: NewExecGit(f.binary), Locks: newCoordFakeLocker(), Verifier: verifier, Recorder: rec})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// integrate is the ordinary success path, used to LAND a dependency so a later
// assertion about a dependent task is made against a real target.
func (f *depFixture) integrate(t *testing.T, c *Coordinator, req Request) Outcome {
	t.Helper()
	outcome, err := c.Integrate(context.Background(), req)
	if err != nil {
		t.Fatalf("integrate %s: %v", req.TaskID, err)
	}
	if !outcome.Integrated {
		t.Fatalf("integrate %s did not land: %+v", req.TaskID, outcome)
	}
	return outcome
}

// The acceptance case: A and B run in parallel and C depends on both.
//
// C is speculatively started before either has landed, so it reaches the lane
// with a target that may be missing one of them. It must wait for each, and
// when it finally integrates it must do so onto a target that contains both --
// which is asserted against the ref, not against what C was told.
func TestDependentTaskIntegratesOnlyOntoATargetHoldingBothDependencies(t *testing.T) {
	t.Parallel()
	f := newDepFixture(t, "task-a", "task-b", "task-c")
	f.commit(t, "task-a", "a.txt", "from a\n", "task a")
	f.commit(t, "task-b", "b.txt", "from b\n", "task b")
	f.commit(t, "task-c", "c.txt", "from c\n", "task c")

	verifier := &coordVerifier{result: Verification{Passed: true}}
	rec := &coordRecorder{}
	c := f.coordinator(t, verifier, rec)
	ctx := context.Background()

	// Nothing has landed yet, so C is simply early. Not a failure, not a
	// conflict, and nothing recorded: an attempt that never started must not
	// leave an audit row claiming one did.
	pending := f.request("task-c")
	pending.Dependencies = []Dependency{{TaskID: "task-a"}, {TaskID: "task-b"}}
	if _, err := c.Integrate(ctx, pending); !errors.Is(err, ErrDependencyPending) {
		t.Fatalf("err = %v, want ErrDependencyPending", err)
	}
	if len(rec.all()) != 0 {
		t.Fatalf("a waiting task recorded %d integration attempts", len(rec.all()))
	}

	// A lands. C is still short of B, and still waits.
	afterA := f.integrate(t, c, f.request("task-a")).Record.TargetAfterSHA
	half := f.request("task-c")
	half.Dependencies = []Dependency{{TaskID: "task-a", IntegratedSHA: afterA}, {TaskID: "task-b"}}
	if _, err := c.Integrate(ctx, half); !errors.Is(err, ErrDependencyPending) {
		t.Fatalf("err = %v, want ErrDependencyPending while task-b is unintegrated", err)
	}

	// B lands. Its own base was overtaken by A, so it is rebased onto the
	// current target and re-verified -- the ordinary dependency-aware refresh.
	outcomeB := f.integrate(t, c, f.request("task-b"))
	if outcomeB.Record.Strategy != StrategyRebaseFastForward || !outcomeB.Record.Replayed {
		t.Fatalf("task-b record = %+v, want a replay onto the moved target", outcomeB.Record)
	}
	afterB := outcomeB.Record.TargetAfterSHA

	// Now C may land, and only now.
	ready := f.request("task-c")
	ready.Dependencies = []Dependency{{TaskID: "task-a", IntegratedSHA: afterA}, {TaskID: "task-b", IntegratedSHA: afterB}}
	outcomeC := f.integrate(t, c, ready)

	// The guarantee, read off the ref rather than off the record: the target C
	// landed on contains both dependencies AND C.
	for _, sha := range []string{afterA, afterB, outcomeC.Record.TargetAfterSHA} {
		if !f.contains(t, sha) {
			t.Fatalf("the target does not contain %s", sha)
		}
	}
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if _, err := os.Stat(filepath.Join(f.repo, name)); err == nil {
			continue
		}
		// The repository's working tree is not checked out at the ref this
		// package moves, so read the file out of the target commit instead.
		if got := integGit(t, f.binary, f.repo, "show", "refs/heads/main:"+name); got == "" {
			t.Fatalf("%s is missing from the integrated target", name)
		}
	}
	if got := outcomeC.Record.DependenciesRequired; len(got) != 2 {
		t.Fatalf("recorded dependencies = %v, want both", got)
	}
}

// A dependency that DID integrate and has since been rewritten off the target is
// the one dependency case a person has to look at: nothing is wrong with this
// task, and landing it would produce exactly the state the whole mechanism
// exists to prevent.
func TestDependencyRewrittenOffTheTargetStopsForAPerson(t *testing.T) {
	t.Parallel()
	f := newDepFixture(t, "task-a", "task-b")
	f.commit(t, "task-a", "a.txt", "from a\n", "task a")
	f.commit(t, "task-b", "b.txt", "from b\n", "task b")

	rec := &coordRecorder{}
	c := f.coordinator(t, &coordVerifier{result: Verification{Passed: true}}, rec)
	afterA := f.integrate(t, c, f.request("task-a")).Record.TargetAfterSHA

	// Something outside AO rewinds the target past A's work.
	integGit(t, f.binary, f.repo, "update-ref", "refs/heads/main", f.baseSHA)

	req := f.request("task-b")
	req.Dependencies = []Dependency{{TaskID: "task-a", IntegratedSHA: afterA}}
	outcome, err := c.Integrate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Integrated || outcome.Attention == nil {
		t.Fatalf("outcome = %+v, want an attention", outcome)
	}
	if outcome.Attention.Reason != ReasonDependencyMissingFromTarget {
		t.Fatalf("reason = %q, want %q", outcome.Attention.Reason, ReasonDependencyMissingFromTarget)
	}
	if !strings.Contains(outcome.Attention.Detail, "task-a") {
		t.Fatalf("the attention does not name the missing dependency: %q", outcome.Attention.Detail)
	}
	if f.head(t, "refs/heads/main") != f.baseSHA {
		t.Fatal("the target moved despite the refusal")
	}
}

// The stale-review case. A landed a change that B also makes, so replaying B
// onto the current target produces a different change from the one its reviewer
// approved. The prior approval is NOT reused: the integration stops with the
// reason that sends the task back for one fresh review, and the ref does not
// move.
func TestRebaseThatChangesWhatTheTaskContributesRequiresAFreshReview(t *testing.T) {
	t.Parallel()
	f := newDepFixture(t, "task-a", "task-b")
	// Both tasks add shared.txt with the same content; only B also adds
	// feature.txt. Once A lands, B's shared.txt commit contributes nothing.
	f.commit(t, "task-a", "shared.txt", "alpha\n", "task a: shared")
	f.commit(t, "task-b", "shared.txt", "alpha\n", "task b: shared")
	f.commit(t, "task-b", "feature.txt", "beta\n", "task b: feature")

	verifier := &coordVerifier{result: Verification{Passed: true}}
	rec := &coordRecorder{}
	c := f.coordinator(t, verifier, rec)
	afterA := f.integrate(t, c, f.request("task-a")).Record.TargetAfterSHA

	req := f.request("task-b")
	req.Dependencies = []Dependency{{TaskID: "task-a", IntegratedSHA: afterA}}
	outcome, err := c.Integrate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Integrated || outcome.Attention == nil {
		t.Fatalf("outcome = %+v, want a stale-review attention", outcome)
	}
	if outcome.Attention.Reason != ReasonStaleReviewAfterRebase {
		t.Fatalf("reason = %q, want %q", outcome.Attention.Reason, ReasonStaleReviewAfterRebase)
	}
	got := outcome.Record
	if got.EffectiveFingerprintBefore == "" || got.EffectiveFingerprintAfter == "" ||
		got.EffectiveFingerprintBefore == got.EffectiveFingerprintAfter {
		t.Fatalf("the record does not show the contribution changing: %+v", got)
	}
	if got.ReviewReused {
		t.Fatal("the record claims the stale approval was reused")
	}
	// The verification is not re-run: verifying work whose review has gone stale
	// answers the wrong question.
	if verifier.calls != 0 {
		t.Fatalf("verification ran %d times for work awaiting a fresh review", verifier.calls)
	}
	if f.head(t, "refs/heads/main") != afterA {
		t.Fatal("the target moved despite the stale approval")
	}
	// The refresh is still durable: B's own branch has been rebased onto the
	// current target, which is the state the fresh review has to read.
	if !f.contains(t, afterA) {
		t.Fatal("the target lost task-a")
	}
	rebased := integGit(t, f.binary, f.worktrees["task-b"], "rev-parse", "HEAD")
	if merged := integGit(t, f.binary, f.repo, "merge-base", rebased, afterA); merged != afterA {
		t.Fatalf("task-b was not left rebased onto the current target (merge-base %s, target %s)", merged, afterA)
	}
}

// The other half of the same decision: a replay that leaves the task's own
// contribution alone reuses the approval it already has, and re-verifies -- the
// content is new even though the change is not.
func TestRebaseThatLeavesTheContributionAloneReusesTheApproval(t *testing.T) {
	t.Parallel()
	f := newDepFixture(t, "task-a", "task-b")
	f.commit(t, "task-a", "a.txt", "from a\n", "task a")
	f.commit(t, "task-b", "b.txt", "from b\n", "task b")

	verifier := &coordVerifier{result: Verification{Passed: true, Summary: "checks passed"}}
	rec := &coordRecorder{}
	c := f.coordinator(t, verifier, rec)
	afterA := f.integrate(t, c, f.request("task-a")).Record.TargetAfterSHA

	req := f.request("task-b")
	req.Dependencies = []Dependency{{TaskID: "task-a", IntegratedSHA: afterA}}
	outcome := f.integrate(t, c, req)
	got := outcome.Record
	if !got.Replayed || got.Strategy != StrategyRebaseFastForward {
		t.Fatalf("record = %+v, want a rebase onto the moved target", got)
	}
	if got.EffectiveFingerprintBefore != got.EffectiveFingerprintAfter {
		t.Fatalf("the rebase changed the contribution: %s -> %s",
			got.EffectiveFingerprintBefore, got.EffectiveFingerprintAfter)
	}
	if !got.ReviewReused {
		t.Fatal("the record does not say the approval was reused")
	}
	// Reusing the approval is not reusing the verification: the content that
	// landed had never been verified, and was.
	if verifier.calls != 1 {
		t.Fatalf("verification ran %d times, want exactly one re-verification", verifier.calls)
	}
	if got.Verification.Source != SourcePostReplay || !got.Verification.Ran {
		t.Fatalf("verification evidence = %+v, want a post-replay run", got.Verification)
	}
}

// A review that was never an approval has nothing to go stale. A policy-skipped
// review must not be turned into a reviewer request by a rebase.
func TestSkippedReviewIsNotInvalidatedByARebase(t *testing.T) {
	t.Parallel()
	f := newDepFixture(t, "task-a", "task-b")
	f.commit(t, "task-a", "shared.txt", "alpha\n", "task a: shared")
	f.commit(t, "task-b", "shared.txt", "alpha\n", "task b: shared")
	f.commit(t, "task-b", "feature.txt", "beta\n", "task b: feature")

	verifier := &coordVerifier{result: Verification{Passed: true}}
	c := f.coordinator(t, verifier, &coordRecorder{})
	afterA := f.integrate(t, c, f.request("task-a")).Record.TargetAfterSHA

	req := f.request("task-b")
	req.Readiness.Review = ReviewSkipped
	req.Dependencies = []Dependency{{TaskID: "task-a", IntegratedSHA: afterA}}
	outcome := f.integrate(t, c, req)
	if outcome.Record.ReviewReused {
		t.Fatal("a skipped review was recorded as a reused approval")
	}
	if verifier.calls != 1 {
		t.Fatalf("verification ran %d times, want exactly one re-verification", verifier.calls)
	}
}
