package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// coordStubGit answers from a tiny fixed graph and records every call, so a
// test can assert not only what the coordinator decided but that it did not
// touch git at all on the paths where it must not.
type coordStubGit struct {
	mu    sync.Mutex
	calls []string

	target   string // current refs/heads/<target>
	source   string
	contains bool // is target an ancestor of source
	casErr   error
}

func (g *coordStubGit) record(format string, args ...any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, fmt.Sprintf(format, args...))
}

func (g *coordStubGit) called() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...)
}

func (g *coordStubGit) ResolveCommit(_ context.Context, dir, rev string) (string, error) {
	g.record("resolve %s %s", dir, rev)
	if strings.HasPrefix(rev, "refs/heads/") && !strings.HasPrefix(rev, "refs/heads/ao/") {
		return g.target, nil
	}
	return g.source, nil
}

// ResolveCommitIfExists mirrors ResolveCommit for this stub: every ref the gate
// tests name does exist. The "target ref not created yet" case has its own
// coverage against a real repository, where absence is a real git answer rather
// than a flag on a stub.
func (g *coordStubGit) ResolveCommitIfExists(ctx context.Context, dir, rev string) (string, bool, error) {
	sha, err := g.ResolveCommit(ctx, dir, rev)
	if err != nil {
		return "", false, err
	}
	return sha, sha != "", nil
}

func (g *coordStubGit) IsAncestor(_ context.Context, _, _, _ string) (bool, error) {
	g.record("is-ancestor")
	return g.contains, nil
}

func (g *coordStubGit) MergeBase(_ context.Context, _, _, _ string) (string, error) {
	g.record("merge-base")
	return "base000", nil
}

func (g *coordStubGit) HasMergeCommits(_ context.Context, _, _, _ string) (bool, error) {
	g.record("has-merges")
	return false, nil
}

func (g *coordStubGit) Rebase(_ context.Context, worktree, onto string) error {
	g.record("rebase %s onto %s", worktree, onto)
	return nil
}

func (g *coordStubGit) CherryPick(_ context.Context, _, _ string) error {
	g.record("cherry-pick")
	return nil
}

func (g *coordStubGit) Commit(_ context.Context, _, _ string) error {
	g.record("commit")
	return nil
}

func (g *coordStubGit) Merge(_ context.Context, _, _, _ string) error {
	g.record("merge")
	return nil
}

func (g *coordStubGit) ContinueReplay(context.Context, string, ReplayOp) error { return nil }
func (g *coordStubGit) AbortReplay(context.Context, string, ReplayOp) error    { return nil }

func (g *coordStubGit) UnmergedPaths(context.Context, string) ([]string, error) { return nil, nil }

func (g *coordStubGit) StageBlob(context.Context, string, string, int) ([]byte, bool, error) {
	return nil, false, nil
}

func (g *coordStubGit) WriteResolution(context.Context, string, string, []byte) error { return nil }

func (g *coordStubGit) CheckoutDetached(_ context.Context, _, rev string) error {
	g.record("checkout-detached %s", rev)
	return nil
}

func (g *coordStubGit) CheckoutBranch(_ context.Context, _, branch string) error {
	g.record("checkout %s", branch)
	return nil
}

func (g *coordStubGit) CompareAndSetRef(_ context.Context, _, ref, next, expected string) error {
	g.record("cas %s %s<-%s", ref, expected, next)
	if g.casErr != nil {
		return g.casErr
	}
	g.target = next
	return nil
}

// coordFakeLocker is a real mutual exclusion over lock keys, in memory. It is
// deliberately not a mock: the property under test is that two integrations of
// one target cannot overlap, and a mock that merely records the call would
// prove nothing about it.
type coordFakeLocker struct {
	mu    sync.Mutex
	held  map[string]string // key -> owner
	next  int
	taken []string
}

func newCoordFakeLocker() *coordFakeLocker {
	return &coordFakeLocker{held: map[string]string{}}
}

func (l *coordFakeLocker) Acquire(_ context.Context, req LockRequest) (LockHandle, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := req.RepoPath + "#" + req.TargetBranch
	if owner, ok := l.held[key]; ok {
		return LockHandle{}, fmt.Errorf("%w: held by %s", ErrLockBusy, owner)
	}
	l.next++
	handle := LockHandle{ID: fmt.Sprintf("lock-%d", l.next), LockKey: key}
	l.held[key] = req.TaskID
	l.taken = append(l.taken, key)
	return handle, nil
}

func (l *coordFakeLocker) Release(_ context.Context, handle LockHandle, _ string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.held, handle.LockKey)
	return nil
}

func (l *coordFakeLocker) stillHeld() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.held)
}

func (l *coordFakeLocker) acquisitions() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.taken...)
}

type coordRecorder struct {
	mu      sync.Mutex
	records []Record
}

func (r *coordRecorder) RecordIntegration(_ context.Context, rec Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec)
	return nil
}

func (r *coordRecorder) all() []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Record(nil), r.records...)
}

type coordVerifier struct {
	calls       int
	lastRequest VerifyRequest
	result      Verification
	err         error
}

func (v *coordVerifier) Verify(_ context.Context, req VerifyRequest) (Verification, error) {
	v.calls++
	v.lastRequest = req
	if v.err != nil {
		return Verification{}, v.err
	}
	return v.result, nil
}

func coordRequest() Request {
	return Request{
		ProjectID:     "prj-1",
		WorkflowRunID: "wf-1",
		TaskID:        "task-1",
		RepoPath:      "/repo",
		WorktreePath:  "/worktrees/task-1",
		TargetBranch:  "main",
		SourceBranch:  "ao/task-1",
		BaseSHA:       "base000",
		Readiness:     Readiness{Review: ReviewApproved, Verify: VerifyPassed},
	}
}

// A task whose review or verification did not pass must never reach the target
// branch -- and, just as importantly, must not even enter the integration lane
// while being turned away, or one unintegratable task could hold the lane
// against every task that IS ready.
func TestUnreadyTaskNeverReachesIntegration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		readiness Readiness
	}{
		{"review requested changes", Readiness{Review: ReviewChangesRequested, Verify: VerifyPassed}},
		{"review failed", Readiness{Review: ReviewFailed, Verify: VerifyPassed}},
		{"review still pending", Readiness{Review: ReviewPending, Verify: VerifyPassed}},
		{"verification failed", Readiness{Review: ReviewApproved, Verify: VerifyFailed}},
		{"verification still pending", Readiness{Review: ReviewApproved, Verify: VerifyPending}},
		{"nothing was recorded at all", Readiness{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			git := &coordStubGit{target: "tgt111", source: "src222", contains: true}
			locks := newCoordFakeLocker()
			rec := &coordRecorder{}
			c, err := New(Deps{Git: git, Locks: locks, Recorder: rec})
			if err != nil {
				t.Fatal(err)
			}
			req := coordRequest()
			req.Readiness = tc.readiness

			outcome, err := c.Integrate(context.Background(), req)
			if !errors.Is(err, ErrNotReady) {
				t.Fatalf("err = %v, want ErrNotReady", err)
			}
			if outcome.Integrated {
				t.Fatal("an unready task reported an integration")
			}
			if got := locks.acquisitions(); len(got) != 0 {
				t.Fatalf("the integration lane was entered by an unready task: %v", got)
			}
			if got := git.called(); len(got) != 0 {
				t.Fatalf("git was touched by an unready task: %v", got)
			}
			if got := rec.all(); len(got) != 0 {
				t.Fatalf("an unready task was recorded as an integration: %v", got)
			}
			if git.target != "tgt111" {
				t.Fatalf("the target branch moved to %s", git.target)
			}
		})
	}
}

// The single-lane property, stated at the level a caller sees it: while one
// integration of a target is in flight, a second one is refused with
// ErrLockBusy, and an integration of a DIFFERENT target is not affected at all.
func TestIntegrationLaneIsSingleFilePerTargetAndOnlyPerTarget(t *testing.T) {
	t.Parallel()
	locks := newCoordFakeLocker()
	git := &coordStubGit{target: "tgt111", source: "src222", contains: true}

	release := make(chan struct{})
	entered := make(chan struct{})
	// The first integration parks inside the lane by blocking in its verifier's
	// place: IsAncestor is the first thing that happens after the lock is taken.
	blocking := &coordBlockingGit{coordStubGit: git, entered: entered, release: release}
	first, err := New(Deps{Git: blocking, Locks: locks, Recorder: &coordRecorder{}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := first.Integrate(context.Background(), coordRequest())
		done <- err
	}()
	<-entered

	second, err := New(Deps{Git: &coordStubGit{target: "tgt111", source: "src333", contains: true}, Locks: locks, Recorder: &coordRecorder{}})
	if err != nil {
		t.Fatal(err)
	}
	sameTarget := coordRequest()
	sameTarget.TaskID = "task-2"
	if _, err := second.Integrate(context.Background(), sameTarget); !errors.Is(err, ErrLockBusy) {
		t.Fatalf("second integration of the same target: err = %v, want ErrLockBusy", err)
	}

	// A different target branch shares nothing with the busy one.
	otherGit := &coordStubGit{target: "oth111", source: "oth222", contains: true}
	other, err := New(Deps{Git: otherGit, Locks: locks, Recorder: &coordRecorder{}})
	if err != nil {
		t.Fatal(err)
	}
	otherTarget := coordRequest()
	otherTarget.TaskID, otherTarget.TargetBranch = "task-3", "release"
	outcome, err := other.Integrate(context.Background(), otherTarget)
	if err != nil {
		t.Fatalf("an unrelated target was blocked by a busy lane: %v", err)
	}
	if !outcome.Integrated {
		t.Fatal("the unrelated target did not integrate")
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first integration: %v", err)
	}

	// And once the lane is free the refused task can take it.
	if _, err := second.Integrate(context.Background(), sameTarget); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

// coordBlockingGit parks the first integration inside the lane so a second one
// can be attempted while it is genuinely in flight.
type coordBlockingGit struct {
	*coordStubGit
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (g *coordBlockingGit) IsAncestor(ctx context.Context, dir, a, b string) (bool, error) {
	g.once.Do(func() {
		close(g.entered)
		<-g.release
	})
	return g.coordStubGit.IsAncestor(ctx, dir, a, b)
}

// Every integration has to leave behind the strategy it chose and the three
// SHAs that describe the ref update, because none of them can be re-derived
// once the branch moves again.
func TestFastForwardIntegrationIsRecordedWithItsStrategyAndSHAs(t *testing.T) {
	t.Parallel()
	git := &coordStubGit{target: "tgt111", source: "src222", contains: true}
	rec := &coordRecorder{}
	verifier := &coordVerifier{result: Verification{Passed: true}}
	c, err := New(Deps{Git: git, Locks: newCoordFakeLocker(), Recorder: rec, Verifier: verifier})
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := c.Integrate(context.Background(), coordRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Integrated {
		t.Fatalf("outcome = %+v, want an integration", outcome)
	}
	got := outcome.Record
	if got.Strategy != StrategyFastForward {
		t.Fatalf("strategy = %q, want %q", got.Strategy, StrategyFastForward)
	}
	if got.SourceSHA != "src222" || got.TargetBeforeSHA != "tgt111" || got.TargetAfterSHA != "src222" {
		t.Fatalf("SHAs = source %q, before %q, after %q", got.SourceSHA, got.TargetBeforeSHA, got.TargetAfterSHA)
	}
	if got.Outcome != OutcomeIntegrated {
		t.Fatalf("outcome = %q", got.Outcome)
	}
	// A fast-forward integrates exactly the content the task already verified,
	// so re-running verification would only re-answer the same question.
	if verifier.calls != 0 {
		t.Fatalf("verification ran %d times for a fast-forward", verifier.calls)
	}
	// Two rows, in this order: the intent to move the ref, then what happened.
	// The first is what makes a landed target impossible to leave unaccounted
	// for if the process dies mid-update.
	records := rec.all()
	if len(records) != 2 {
		t.Fatalf("recorder saw %d rows, want an intent and a result: %+v", len(records), records)
	}
	if records[0].Outcome != OutcomeAttempting || records[1].Outcome != OutcomeIntegrated {
		t.Fatalf("ledger outcomes = %q, %q; want attempting then integrated", records[0].Outcome, records[1].Outcome)
	}
	for i, r := range records {
		if r.Strategy != StrategyFastForward || r.TargetBeforeSHA != "tgt111" || r.TargetAfterSHA != "src222" {
			t.Fatalf("row %d does not name the update: %+v", i, r)
		}
	}
	// The ref update must be a compare-and-set against the head that was read.
	var sawCAS bool
	for _, call := range git.called() {
		if strings.HasPrefix(call, "cas ") {
			sawCAS = true
			if call != "cas refs/heads/main tgt111<-src222" {
				t.Fatalf("ref update = %q", call)
			}
		}
	}
	if !sawCAS {
		t.Fatal("the target branch was never updated")
	}
}

// A target that something outside AO moved while the lane was held must not be
// overwritten: the compare-and-set fails and the task stops for a person.
func TestTargetWrittenOutsideAOStopsForAPerson(t *testing.T) {
	t.Parallel()
	git := &coordStubGit{target: "tgt111", source: "src222", contains: true, casErr: errors.New("ref is not at tgt111")}
	rec := &coordRecorder{}
	c, err := New(Deps{Git: git, Locks: newCoordFakeLocker(), Recorder: rec})
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := c.Integrate(context.Background(), coordRequest())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Integrated || outcome.Record.Attention == nil {
		t.Fatalf("outcome = %+v, want a needs-attention", outcome)
	}
	att := outcome.Record.Attention
	if att.Reason != ReasonTargetMoved {
		t.Fatalf("reason = %q", att.Reason)
	}
	if att.TargetSHA != "tgt111" || att.BaseSHA != "base000" {
		t.Fatalf("attention SHAs = %+v", att)
	}
	// The intent was recorded before the ref update was attempted, and the
	// refusal after it -- so the ledger shows an update that was tried and did
	// not take, rather than nothing at all.
	records := rec.all()
	if len(records) != 2 {
		t.Fatalf("recorder saw %d rows, want an intent and a refusal: %+v", len(records), records)
	}
	if records[0].Outcome != OutcomeAttempting || records[1].Outcome != OutcomeNeedsAttention {
		t.Fatalf("ledger outcomes = %q, %q", records[0].Outcome, records[1].Outcome)
	}
	if records[1].TargetAfterSHA != "" {
		t.Fatalf("a refused update recorded a target-after of %q", records[1].TargetAfterSHA)
	}
}

// Replaying work onto a moved target without being able to re-verify it is the
// exact silent regression this package exists to prevent, so it is refused
// rather than assumed to pass.
func TestReplayWithoutAVerifierIsRefused(t *testing.T) {
	t.Parallel()
	git := &coordStubGit{target: "tgt111", source: "src222", contains: false}
	c, err := New(Deps{Git: git, Locks: newCoordFakeLocker(), Recorder: &coordRecorder{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Integrate(context.Background(), coordRequest()); err == nil ||
		!strings.Contains(err.Error(), "no Verifier is configured") {
		t.Fatalf("err = %v, want a refusal naming the missing verifier", err)
	}
	if git.target != "tgt111" {
		t.Fatalf("the target moved to %s", git.target)
	}
}

// The mutating half of Git may only ever run in AO's own task worktree.
func TestIntegrationRefusesToUseTheRepositoryAsItsWorktree(t *testing.T) {
	t.Parallel()
	c, err := New(Deps{Git: &coordStubGit{}, Locks: newCoordFakeLocker(), Recorder: &coordRecorder{}})
	if err != nil {
		t.Fatal(err)
	}
	req := coordRequest()
	req.WorktreePath = req.RepoPath
	if _, err := c.Integrate(context.Background(), req); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

// An integration nobody can account for afterwards is worse than one that did
// not happen, and it is undetectable once it has occurred -- so the only place
// a missing Recorder can be caught is before anything has been integrated.
func TestCoordinatorCannotBeBuiltWithoutARecorder(t *testing.T) {
	t.Parallel()
	if _, err := New(Deps{Git: &coordStubGit{}, Locks: newCoordFakeLocker()}); err == nil ||
		!strings.Contains(err.Error(), "Recorder is required") {
		t.Fatalf("err = %v, want a refusal naming the missing recorder", err)
	}
	if _, err := New(Deps{Git: &coordStubGit{}, Locks: newCoordFakeLocker(), Recorder: &coordRecorder{}}); err != nil {
		t.Fatalf("a fully configured coordinator was refused: %v", err)
	}
}

// One writable ref must mean exactly one lane, however the request spells it.
//
// The hazard this guards is specific: if the lane were keyed on a TargetBranch
// a caller could choose independently of TargetRef, two requests naming one ref
// under two labels would take two different locks and both start replaying onto
// it. The compare-and-set would still stop the second from overwriting the
// first -- but only after it had rebased and re-verified against a target that
// was already stale, and only by failing. So the name is derived from the ref,
// and these are the two halves of that: different spellings contend, and a
// spelling that contradicts its ref is refused outright.
func TestOneTargetRefIsOneLaneHoweverItIsSpelled(t *testing.T) {
	t.Parallel()
	locks := newCoordFakeLocker()
	entered, release := make(chan struct{}), make(chan struct{})

	// The first request names the target by branch alone.
	blocking := &coordBlockingGit{
		coordStubGit: &coordStubGit{target: "tgt111", source: "src222", contains: true},
		entered:      entered, release: release,
	}
	first, err := New(Deps{Git: blocking, Locks: locks, Recorder: &coordRecorder{}})
	if err != nil {
		t.Fatal(err)
	}
	byBranch := coordRequest()
	byBranch.TargetBranch, byBranch.TargetRef = "main", ""
	done := make(chan error, 1)
	go func() {
		_, err := first.Integrate(context.Background(), byBranch)
		done <- err
	}()
	<-entered

	// The second names the SAME ref by its full name and no branch at all.
	second, err := New(Deps{
		Git:      &coordStubGit{target: "tgt111", source: "src333", contains: true},
		Locks:    locks,
		Recorder: &coordRecorder{},
	})
	if err != nil {
		t.Fatal(err)
	}
	byRef := coordRequest()
	byRef.TaskID, byRef.TargetBranch, byRef.TargetRef = "task-2", "", "refs/heads/main"
	if _, err := second.Integrate(context.Background(), byRef); !errors.Is(err, ErrLockBusy) {
		t.Fatalf("a request naming the same ref by its full name got its own lane: err = %v", err)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first integration: %v", err)
	}
	// Exactly one lane was ever taken, under the canonical short-branch name --
	// which is the key a direct-branch writer of main uses too.
	if got := locks.acquisitions(); len(got) != 1 || got[0] != "/repo#main" {
		t.Fatalf("lanes taken = %v, want exactly [/repo#main]", got)
	}
}

// A ref and a branch label that contradict each other are refused rather than
// silently reconciled: a caller that believes it is integrating something other
// than what the ref says would have recorded that belief too.
func TestTargetBranchThatContradictsItsRefIsRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		branch, ref string
		wantLane    string
		wantRefusal bool
	}{
		{name: "branch alone derives its ref", branch: "main", ref: "", wantLane: "main"},
		{name: "ref alone derives its lane", branch: "", ref: "refs/heads/main", wantLane: "main"},
		{name: "matching pair is accepted", branch: "main", ref: "refs/heads/main", wantLane: "main"},
		{
			// An AO-owned ref is not a branch, so its lane is its full name --
			// which cannot collide with any branch's short name.
			name: "an AO ref keeps its full name as its lane",
			ref:  "refs/ao/workflows/wf-1/integration", wantLane: "refs/ao/workflows/wf-1/integration",
		},
		{name: "a label that names a different ref", branch: "release", ref: "refs/heads/main", wantRefusal: true},
		{name: "a label attached to a non-branch ref", branch: "main", ref: "refs/ao/workflows/wf-1/integration", wantRefusal: true},
		{name: "neither a ref nor a branch", wantRefusal: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := coordRequest()
			req.TargetBranch, req.TargetRef = tc.branch, tc.ref
			err := req.validate()
			if tc.wantRefusal {
				if !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("err = %v, want ErrInvalidRequest", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("validate: %v", err)
			}
			if req.TargetBranch != tc.wantLane {
				t.Fatalf("lane name = %q, want %q", req.TargetBranch, tc.wantLane)
			}
		})
	}
}
