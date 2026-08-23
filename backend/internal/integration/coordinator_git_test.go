package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These are the tests that matter most, and they run against the real git
// binary rather than a fake for one reason: every interesting property of this
// package -- what a rebase does to a moved target, which files git calls
// conflicted, whether a compare-and-set actually refuses -- is a property of
// git, and a fake would only assert that this file's author and this package's
// author agreed with each other.

func integRequireGit(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	return path
}

func integGit(t *testing.T, binary, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(binary, append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANGUAGE=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func integWrite(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func integRead(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// integFixture is one project: a repository with a target branch, and an
// AO-owned worktree on a task branch cut from it. It mirrors what the worktree
// lifecycle manager produces for an isolated_worktree task.
type integFixture struct {
	binary   string
	repo     string
	worktree string
	target   string
	source   string
	baseSHA  string
}

func newIntegFixture(t *testing.T) *integFixture {
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
	integWrite(t, repo, "CHANGELOG.md", "# changelog\n")
	integGit(t, binary, repo, "add", ".")
	integGit(t, binary, repo, "commit", "-m", "seed")
	base := integGit(t, binary, repo, "rev-parse", "HEAD")

	// AO never works in the user's checkout, so the task gets its own worktree
	// on its own ao/* branch, cut from the target at the moment it started.
	worktree := filepath.Join(root, "worktrees", "task-1")
	integGit(t, binary, repo, "worktree", "add", "-b", "ao/task-1", worktree, base)

	return &integFixture{binary: binary, repo: repo, worktree: worktree, target: "main", source: "ao/task-1", baseSHA: base}
}

// commitInWorktree adds the task's own work.
func (f *integFixture) commitInWorktree(t *testing.T, name, content, message string) string {
	t.Helper()
	integWrite(t, f.worktree, name, content)
	integGit(t, f.binary, f.worktree, "add", name)
	integGit(t, f.binary, f.worktree, "commit", "-m", message)
	return integGit(t, f.binary, f.worktree, "rev-parse", "HEAD")
}

// advanceTarget is another task's integration landing first: the target moves
// while this task was working, which is the whole reason the coordinator reads
// the head inside the lane instead of trusting the task's base.
func (f *integFixture) advanceTarget(t *testing.T, name, content, message string) string {
	t.Helper()
	integWrite(t, f.repo, name, content)
	integGit(t, f.binary, f.repo, "add", name)
	integGit(t, f.binary, f.repo, "commit", "-m", message)
	return integGit(t, f.binary, f.repo, "rev-parse", "HEAD")
}

func (f *integFixture) head(t *testing.T, rev string) string {
	t.Helper()
	return integGit(t, f.binary, f.repo, "rev-parse", rev)
}

func (f *integFixture) request() Request {
	return Request{
		ProjectID:     "prj-1",
		WorkflowRunID: "wf-1",
		TaskID:        "task-1",
		RepoPath:      f.repo,
		WorktreePath:  f.worktree,
		TargetBranch:  f.target,
		SourceBranch:  f.source,
		BaseSHA:       f.baseSHA,
		Readiness:     Readiness{Review: ReviewApproved, Verify: VerifyPassed},
	}
}

func (f *integFixture) coordinator(t *testing.T, verifier Verifier, rec Recorder) *Coordinator {
	t.Helper()
	c, err := New(Deps{Git: NewExecGit(f.binary), Locks: newCoordFakeLocker(), Verifier: verifier, Recorder: rec})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Nothing moved the target, so the task's commits already contain it and the
// ref can simply be moved forward. No history is rewritten and the
// verification the task already passed still describes this exact content.
func TestIntegrateFastForwardsWhenTheTargetDidNotMove(t *testing.T) {
	t.Parallel()
	f := newIntegFixture(t)
	source := f.commitInWorktree(t, "feature.txt", "task work\n", "task 1")
	verifier := &coordVerifier{result: Verification{Passed: true}}
	rec := &coordRecorder{}

	outcome, err := f.coordinator(t, verifier, rec).Integrate(context.Background(), f.request())
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Integrated {
		t.Fatalf("outcome = %+v", outcome)
	}
	if outcome.Record.Strategy != StrategyFastForward {
		t.Fatalf("strategy = %q, want fast_forward", outcome.Record.Strategy)
	}
	if got := f.head(t, "main"); got != source {
		t.Fatalf("main = %s, want the task tip %s", got, source)
	}
	if outcome.Record.TargetBeforeSHA != f.baseSHA || outcome.Record.TargetAfterSHA != source {
		t.Fatalf("recorded SHAs = %+v", outcome.Record)
	}
	if verifier.calls != 0 {
		t.Fatalf("verification re-ran %d times for content it had already seen", verifier.calls)
	}
	if outcome.Record.Replayed {
		t.Fatal("a fast-forward reported a replay")
	}
}

// The case the whole package exists for: the target advanced while the task was
// working, so the task's work is rebased onto where the target actually is,
// re-verified against that, and only then fast-forwarded on.
func TestTargetMovedForcesRebaseThenVerifyThenFastForward(t *testing.T) {
	t.Parallel()
	f := newIntegFixture(t)
	f.commitInWorktree(t, "feature.txt", "task work\n", "task 1")
	movedTarget := f.advanceTarget(t, "other.txt", "another task landed first\n", "task 0")

	verifier := &coordVerifier{result: Verification{Passed: true, Summary: "go test ./... ok"}}
	rec := &coordRecorder{}
	outcome, err := f.coordinator(t, verifier, rec).Integrate(context.Background(), f.request())
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Integrated {
		t.Fatalf("outcome = %+v", outcome)
	}
	got := outcome.Record
	if got.Strategy != StrategyRebaseFastForward {
		t.Fatalf("strategy = %q, want rebase_fast_forward", got.Strategy)
	}
	if !got.Replayed {
		t.Fatal("the record does not say the work was replayed")
	}
	// The verification that was re-run is the one that describes what landed.
	if verifier.calls != 1 {
		t.Fatalf("verification ran %d times, want exactly once", verifier.calls)
	}
	if !got.Verification.Ran || !got.Verification.Passed {
		t.Fatalf("recorded verification = %+v", got.Verification)
	}
	// Every SHA in the record is one a later reader could not re-derive.
	if got.BaseSHA != f.baseSHA {
		t.Fatalf("base = %s, want the target the task was cut from %s", got.BaseSHA, f.baseSHA)
	}
	if got.TargetBeforeSHA != movedTarget {
		t.Fatalf("target-before = %s, want the head read inside the lane %s", got.TargetBeforeSHA, movedTarget)
	}
	if got.TargetAfterSHA != f.head(t, "main") {
		t.Fatalf("target-after = %s, but main is at %s", got.TargetAfterSHA, f.head(t, "main"))
	}
	if got.SourceSHA != got.TargetAfterSHA {
		t.Fatalf("source %s did not become the new target %s", got.SourceSHA, got.TargetAfterSHA)
	}
	// The point of rebasing rather than merging: main's history stays linear
	// and holds both tasks' files.
	if out := integGit(t, f.binary, f.repo, "cat-file", "-p", "main^{commit}"); strings.Count(out, "parent ") != 1 {
		t.Fatalf("the new target head is not a single-parent commit:\n%s", out)
	}
	for _, name := range []string{"feature.txt", "other.txt"} {
		if integGit(t, f.binary, f.repo, "cat-file", "-e", "main:"+name) != "" {
			t.Fatalf("%s is missing from the integrated target", name)
		}
	}
	// The verifier was asked about the rebased commit, not the original one.
	if verifier.lastRequest.HeadSHA != got.SourceSHA || verifier.lastRequest.TargetSHA != movedTarget {
		t.Fatalf("verify request = %+v", verifier.lastRequest)
	}
}

// Work that passes against the base it was written on and fails against the
// moved target is a fact only its author can act on. The target must not move.
func TestVerificationFailingAfterRebaseStopsForAPersonAndLeavesTheTargetAlone(t *testing.T) {
	t.Parallel()
	f := newIntegFixture(t)
	f.commitInWorktree(t, "feature.txt", "task work\n", "task 1")
	movedTarget := f.advanceTarget(t, "other.txt", "another task landed first\n", "task 0")

	verifier := &coordVerifier{result: Verification{Passed: false, Summary: "go test ./...: 2 failures"}}
	rec := &coordRecorder{}
	outcome, err := f.coordinator(t, verifier, rec).Integrate(context.Background(), f.request())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Integrated {
		t.Fatal("work that failed verification against the target was integrated")
	}
	att := outcome.Record.Attention
	if att == nil || att.Reason != ReasonVerificationFailed {
		t.Fatalf("attention = %+v, want a verification failure", att)
	}
	if att.TargetSHA != movedTarget || att.BaseSHA != f.baseSHA || att.SourceSHA == "" {
		t.Fatalf("attention SHAs = %+v", att)
	}
	if !strings.Contains(att.Detail, "2 failures") {
		t.Fatalf("attention detail lost the verification output: %q", att.Detail)
	}
	if got := f.head(t, "main"); got != movedTarget {
		t.Fatalf("main moved to %s despite a failed verification", got)
	}
	if records := rec.all(); len(records) != 1 || records[0].Outcome != OutcomeNeedsAttention {
		t.Fatalf("recorder = %+v", records)
	}
}

// The deterministic low-risk case: two independent tasks each appended a line
// to the same append-only file. There is exactly one result that loses
// nothing, so the coordinator produces it rather than asking a person.
func TestNonOverlappingAppendConflictResolvesAutomatically(t *testing.T) {
	t.Parallel()
	f := newIntegFixture(t)
	f.commitInWorktree(t, "CHANGELOG.md", "# changelog\n- task one\n", "task 1 changelog")
	f.advanceTarget(t, "CHANGELOG.md", "# changelog\n- task zero\n", "task 0 changelog")

	verifier := &coordVerifier{result: Verification{Passed: true}}
	rec := &coordRecorder{}
	outcome, err := f.coordinator(t, verifier, rec).Integrate(context.Background(), f.request())
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Integrated {
		t.Fatalf("outcome = %+v, want an automatic resolution", outcome)
	}
	if got := outcome.Record.AutoResolvedPaths; len(got) != 1 || got[0] != "CHANGELOG.md" {
		t.Fatalf("auto-resolved paths = %v", got)
	}
	integrated := integGit(t, f.binary, f.repo, "show", "main:CHANGELOG.md")
	for _, line := range []string{"- task zero", "- task one"} {
		if !strings.Contains(integrated, line) {
			t.Fatalf("the resolution dropped %q:\n%s", line, integrated)
		}
	}
	if strings.Contains(integrated, "<<<<<<<") {
		t.Fatalf("conflict markers reached the target:\n%s", integrated)
	}
	// The resolved content is what was verified, not the pre-resolution one.
	if verifier.calls != 1 {
		t.Fatalf("verification ran %d times", verifier.calls)
	}
}

// The case that must NOT be resolved automatically: both sides changed the same
// line, so any resolution is a choice between two people's intentions. The task
// stops with the exact files and the exact SHAs, the target does not move, and
// the task's worktree is handed back the way its author left it.
func TestOverlappingConflictStopsWithExactFilesAndSHAs(t *testing.T) {
	t.Parallel()
	f := newIntegFixture(t)
	// Both sides rewrote the SAME existing line. Nothing can merge that without
	// picking one of them, which is precisely what the automatic rule refuses.
	sourceSHA := f.commitInWorktree(t, "README.md", "task one's seed\n", "task 1 readme")
	movedTarget := f.advanceTarget(t, "README.md", "task zero's seed\n", "task 0 readme")

	verifier := &coordVerifier{result: Verification{Passed: true}}
	rec := &coordRecorder{}
	outcome, err := f.coordinator(t, verifier, rec).Integrate(context.Background(), f.request())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Integrated {
		t.Fatal("an overlapping conflict was resolved and integrated")
	}
	att := outcome.Record.Attention
	if att == nil || att.Reason != ReasonMergeConflict {
		t.Fatalf("attention = %+v, want a merge conflict", att)
	}
	if outcome.Attention != att {
		t.Fatalf("Outcome.Attention (%+v) disagrees with Record.Attention (%+v)", outcome.Attention, att)
	}
	if len(att.ConflictFiles) != 1 || att.ConflictFiles[0] != "README.md" {
		t.Fatalf("conflict files = %v, want the exact conflicting path", att.ConflictFiles)
	}
	if att.BaseSHA != f.baseSHA || att.TargetSHA != movedTarget || att.SourceSHA != sourceSHA {
		t.Fatalf("attention SHAs = base %s / target %s / source %s; want %s / %s / %s",
			att.BaseSHA, att.TargetSHA, att.SourceSHA, f.baseSHA, movedTarget, sourceSHA)
	}
	if got := f.head(t, "main"); got != movedTarget {
		t.Fatalf("main moved to %s despite an unresolved conflict", got)
	}
	// Nothing may be left half-rebased in the task's own worktree.
	if got := f.head(t, "ao/task-1"); got != sourceSHA {
		t.Fatalf("the task branch was left at %s, want %s", got, sourceSHA)
	}
	if status := integGit(t, f.binary, f.worktree, "status", "--porcelain"); status != "" {
		t.Fatalf("the task worktree was left dirty:\n%s", status)
	}
	if strings.Contains(integRead(t, f.worktree, "README.md"), "<<<<<<<") {
		t.Fatal("conflict markers were left in the task's worktree")
	}
	// Verification never runs for work that could not be replayed at all.
	if verifier.calls != 0 {
		t.Fatalf("verification ran %d times for an unresolved conflict", verifier.calls)
	}
	if records := rec.all(); len(records) != 1 || records[0].Outcome != OutcomeNeedsAttention {
		t.Fatalf("recorder = %+v", records)
	}
}

// A project that would rather see every conflict itself gets exactly that: the
// same append-only conflict that resolves by default becomes a Needs attention.
func TestAutoResolutionCanBeDisabledByPolicy(t *testing.T) {
	t.Parallel()
	f := newIntegFixture(t)
	f.commitInWorktree(t, "CHANGELOG.md", "# changelog\n- task one\n", "task 1 changelog")
	f.advanceTarget(t, "CHANGELOG.md", "# changelog\n- task zero\n", "task 0 changelog")

	req := f.request()
	req.Policy.DisableAutoResolve = true
	outcome, err := f.coordinator(t, &coordVerifier{result: Verification{Passed: true}}, &coordRecorder{}).
		Integrate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Integrated || outcome.Record.Attention == nil {
		t.Fatalf("outcome = %+v, want a needs-attention", outcome)
	}
	if got := outcome.Record.Attention.ConflictFiles; len(got) != 1 || got[0] != "CHANGELOG.md" {
		t.Fatalf("conflict files = %v", got)
	}
}

// A task whose history contains a merge cannot be rebased without silently
// flattening it, so it is cherry-picked onto the moved target instead -- and
// the task's own branch is left exactly where it was.
func TestMergeHistoryIsCherryPickedRatherThanFlattened(t *testing.T) {
	t.Parallel()
	f := newIntegFixture(t)
	// Build a task branch whose history contains a merge commit.
	f.commitInWorktree(t, "feature.txt", "task work\n", "task 1")
	integGit(t, f.binary, f.worktree, "checkout", "-b", "ao/task-1-side", f.baseSHA)
	integWrite(t, f.worktree, "side.txt", "side work\n")
	integGit(t, f.binary, f.worktree, "add", "side.txt")
	integGit(t, f.binary, f.worktree, "commit", "-m", "side")
	integGit(t, f.binary, f.worktree, "checkout", "ao/task-1")
	integGit(t, f.binary, f.worktree, "merge", "--no-ff", "--no-edit", "-m", "merge side", "ao/task-1-side")
	sourceSHA := integGit(t, f.binary, f.worktree, "rev-parse", "HEAD")
	movedTarget := f.advanceTarget(t, "other.txt", "another task landed first\n", "task 0")

	verifier := &coordVerifier{result: Verification{Passed: true}}
	outcome, err := f.coordinator(t, verifier, &coordRecorder{}).Integrate(context.Background(), f.request())
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Integrated {
		t.Fatalf("outcome = %+v", outcome)
	}
	if outcome.Record.Strategy != StrategyCherryPick {
		t.Fatalf("strategy = %q, want cherry_pick", outcome.Record.Strategy)
	}
	if got := f.head(t, "ao/task-1"); got != sourceSHA {
		t.Fatalf("cherry-pick rewrote the task branch to %s, want %s left alone", got, sourceSHA)
	}
	if f.head(t, "main") == movedTarget {
		t.Fatal("the target did not move")
	}
	for _, name := range []string{"feature.txt", "side.txt", "other.txt"} {
		if integGit(t, f.binary, f.repo, "cat-file", "-e", "main:"+name) != "" {
			t.Fatalf("%s is missing from the integrated target", name)
		}
	}
	// The worktree is handed back on the task's own branch, not on the
	// detached HEAD the cherry-pick was staged on.
	if got := integGit(t, f.binary, f.worktree, "rev-parse", "--abbrev-ref", "HEAD"); got != "ao/task-1" {
		t.Fatalf("the task worktree was left on %q", got)
	}
}

// A task that stops for a person must stop only itself. This is the property
// that makes the single lane acceptable at all: if an unresolvable conflict
// held the lane, one task nobody looked at would stall every other task's
// integration behind it.
func TestAConflictedTaskDoesNotBlockTheNextOne(t *testing.T) {
	t.Parallel()
	f := newIntegFixture(t)
	// One shared lane for both tasks, exactly as a real project has.
	locks := newCoordFakeLocker()
	newCoordinator := func() *Coordinator {
		c, err := New(Deps{Git: NewExecGit(f.binary), Locks: locks,
			Verifier: &coordVerifier{result: Verification{Passed: true}}, Recorder: &coordRecorder{}})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	// A second AO task worktree, cut from the same base, whose work does not
	// conflict with anything.
	secondWorktree := filepath.Join(filepath.Dir(f.worktree), "task-2")
	integGit(t, f.binary, f.repo, "worktree", "add", "-b", "ao/task-2", secondWorktree, f.baseSHA)
	integWrite(t, secondWorktree, "independent.txt", "unrelated work\n")
	integGit(t, f.binary, secondWorktree, "add", "independent.txt")
	integGit(t, f.binary, secondWorktree, "commit", "-m", "task 2")
	secondSHA := integGit(t, f.binary, secondWorktree, "rev-parse", "HEAD")

	// Task 1 collides with a target that moved under it.
	f.commitInWorktree(t, "README.md", "task one's seed\n", "task 1 readme")
	f.advanceTarget(t, "README.md", "task zero's seed\n", "task 0 readme")

	conflicted, err := newCoordinator().Integrate(context.Background(), f.request())
	if err != nil {
		t.Fatal(err)
	}
	if conflicted.Integrated || conflicted.Record.Attention == nil {
		t.Fatalf("task 1 outcome = %+v, want a needs-attention", conflicted)
	}

	// Task 2 integrates immediately afterwards: the lane was given back.
	req := f.request()
	req.TaskID, req.WorktreePath, req.SourceBranch = "task-2", secondWorktree, "ao/task-2"
	second, err := newCoordinator().Integrate(context.Background(), req)
	if err != nil {
		t.Fatalf("the next task was blocked by a conflicted one: %v", err)
	}
	if !second.Integrated {
		t.Fatalf("task 2 outcome = %+v", second)
	}
	if second.Record.SourceSHA == secondSHA {
		t.Fatal("task 2 should have been rebased onto the moved target")
	}
	if integGit(t, f.binary, f.repo, "cat-file", "-e", "main:independent.txt") != "" {
		t.Fatal("task 2's work is missing from the target")
	}
	// And nothing is left holding the lane.
	if held := locks.stillHeld(); held != 0 {
		t.Fatalf("%d locks are still held after both integrations", held)
	}
}

// Every strategy here replays a change relative to a common base, so a source
// that shares no ancestor with the target has nothing any of them can apply.
// It stops for a person rather than failing, and the target does not move.
func TestUnrelatedHistoriesStopForAPersonRatherThanFailing(t *testing.T) {
	t.Parallel()
	f := newIntegFixture(t)
	target := f.head(t, "main")

	// An orphan branch: no commit in it is reachable from main, and no commit
	// in main is reachable from it.
	integGit(t, f.binary, f.worktree, "checkout", "--orphan", "ao/task-orphan")
	integGit(t, f.binary, f.worktree, "rm", "-rf", "--quiet", ".")
	integWrite(t, f.worktree, "orphan.txt", "unrelated work\n")
	integGit(t, f.binary, f.worktree, "add", "orphan.txt")
	integGit(t, f.binary, f.worktree, "commit", "-m", "orphan")
	orphanSHA := integGit(t, f.binary, f.worktree, "rev-parse", "HEAD")

	req := f.request()
	req.SourceBranch = "ao/task-orphan"
	outcome, err := f.coordinator(t, &coordVerifier{result: Verification{Passed: true}}, &coordRecorder{}).
		Integrate(context.Background(), req)
	if err != nil {
		t.Fatalf("unrelated histories were reported as a failure: %v", err)
	}
	att := outcome.Record.Attention
	if att == nil || att.Reason != ReasonNoApplicableStrategy {
		t.Fatalf("attention = %+v, want no-applicable-strategy", att)
	}
	if att.TargetSHA != target || att.SourceSHA != orphanSHA {
		t.Fatalf("attention SHAs = %+v, want target %s / source %s", att, target, orphanSHA)
	}
	if got := f.head(t, "main"); got != target {
		t.Fatalf("main moved to %s", got)
	}
}
