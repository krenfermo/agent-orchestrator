package integration

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// The restart tests, against real git.
//
// A daemon that dies mid-integration leaves the ref update and the two records
// that describe it (this package's audit row, and the caller's own promotion
// ledger) in some order, and no ordering of writes to three stores survives
// every crash. What has to hold instead is that RE-ENTERING the integration
// from any of those points is safe: the target must never gain a second copy of
// the same work, and the second attempt must say so rather than quietly
// pretending it did the first one.
//
// That safety is not a special recovery path. It is the ordinary path applied
// to a target that already contains the source: the head is re-read inside the
// lane, the source is found to contain it, and the compare-and-set is a no-op
// because next already equals the head. These tests pin exactly that.

// commitCount is the number of commits reachable from a ref -- the direct
// measure of "was this merged twice".
func (f *integFixture) commitCount(t *testing.T, rev string) int {
	t.Helper()
	n, err := strconv.Atoi(integGit(t, f.binary, f.repo, "rev-list", "--count", rev))
	if err != nil {
		t.Fatalf("count commits on %s: %v", rev, err)
	}
	return n
}

// A restart between the ref update and the caller's own record of it.
//
// The caller has no promotion checkpoint, so its next pass integrates this task
// again -- which is the whole point of recording the promotion after the ref
// moves rather than before. The second attempt must find the work already
// there and land nothing.
func TestReintegratingAfterARestartDoesNotMergeTwice(t *testing.T) {
	t.Parallel()
	f := newIntegFixture(t)
	f.commitInWorktree(t, "feature.txt", "task work\n", "task 1")
	// The target moved while the task worked, so the first integration takes
	// the interesting road: a rebase, a re-verification, then a fast-forward.
	f.advanceTarget(t, "other.txt", "another task landed first\n", "task 0")

	verifier := &coordVerifier{result: Verification{Passed: true}}
	rec := &coordRecorder{}
	coordinator := f.coordinator(t, verifier, rec)

	first, err := coordinator.Integrate(context.Background(), f.request())
	if err != nil {
		t.Fatal(err)
	}
	if !first.Integrated || first.Record.Strategy != StrategyRebaseFastForward {
		t.Fatalf("first integration = %+v", first.Record)
	}
	landed := f.head(t, "main")
	countAfterFirst := f.commitCount(t, "main")

	// The daemon restarts here. Nothing durable outside this package says the
	// task was promoted, so the next pass asks for the same integration again.
	second, err := coordinator.Integrate(context.Background(), f.request())
	if err != nil {
		t.Fatal(err)
	}

	if !second.Integrated {
		t.Fatalf("the re-attempt did not report the work as integrated: %+v", second)
	}
	if got := f.head(t, "main"); got != landed {
		t.Fatalf("main moved on the second attempt: %s -> %s", landed, got)
	}
	if got := f.commitCount(t, "main"); got != countAfterFirst {
		t.Fatalf("main gained %d commit(s) on the second attempt", got-countAfterFirst)
	}
	// The second attempt is a fast-forward onto the commit the ref is already
	// at, which is what "there was nothing left to do" looks like in the audit.
	if second.Record.Strategy != StrategyFastForward {
		t.Fatalf("second strategy = %q, want fast_forward", second.Record.Strategy)
	}
	if second.Record.TargetBeforeSHA != landed || second.Record.TargetAfterSHA != landed {
		t.Fatalf("second record brackets a ref update that did not happen: %+v", second.Record)
	}
	if second.Record.Replayed {
		t.Fatal("the second attempt replayed work that was already on the target")
	}
	// Nothing was verified a second time either: the content on the target is
	// the content the first attempt already verified.
	if verifier.calls != 1 {
		t.Fatalf("verifier ran %d times, want once", verifier.calls)
	}
	// Both attempts are on the ledger. The second one is not noise -- it is the
	// durable account of a pass that found the work already landed.
	records := rec.all()
	if len(records) < 2 {
		t.Fatalf("recorded %d attempts, want both", len(records))
	}
	if records[len(records)-1].Outcome != OutcomeIntegrated {
		t.Fatalf("last record outcome = %q, want integrated", records[len(records)-1].Outcome)
	}
}

// A restart between the ref update and THIS package's own audit row.
//
// The Recorder fails after the ref has moved, which is exactly the shape a
// crash there leaves: the physical side done, the account of it missing. The
// caller is told (an error alongside Integrated=true, which is what makes it
// refuse the promotion and record the outstanding audit), and the next pass
// must complete the audit without moving anything.
func TestRestartBetweenTheRefUpdateAndTheAuditDoesNotIntegrateAgain(t *testing.T) {
	t.Parallel()
	f := newIntegFixture(t)
	source := f.commitInWorktree(t, "feature.txt", "task work\n", "task 1")

	failing := &flakyRecorder{failAfterIntent: true}
	coordinator := f.coordinator(t, &coordVerifier{result: Verification{Passed: true}}, failing)

	first, err := coordinator.Integrate(context.Background(), f.request())
	if err == nil {
		t.Fatal("a failed audit was reported as a clean integration")
	}
	if !first.Integrated {
		t.Fatalf("the ref moved but the outcome does not say so: %+v", first)
	}
	if got := f.head(t, "main"); got != source {
		t.Fatalf("main = %s, want the task tip %s", got, source)
	}
	// The intent row survives the crash, and it names exactly which commit was
	// about to move which ref, from where. That is the whole reason it is
	// written before the ref moves.
	intents := failing.all()
	if len(intents) == 0 || intents[0].Outcome != OutcomeAttempting {
		t.Fatalf("no attempting row was written before the ref moved: %+v", intents)
	}
	if intents[0].TargetAfterSHA != source {
		t.Fatalf("the intent row names %s, want %s", intents[0].TargetAfterSHA, source)
	}
	countAfterFirst := f.commitCount(t, "main")

	// The restart: a working recorder, the same request.
	good := &coordRecorder{}
	second, err := coordinator2(t, f, good).Integrate(context.Background(), f.request())
	if err != nil {
		t.Fatal(err)
	}
	if !second.Integrated {
		t.Fatalf("the recovery pass did not report the work as integrated: %+v", second)
	}
	if got := f.commitCount(t, "main"); got != countAfterFirst {
		t.Fatalf("main gained %d commit(s) on the recovery pass", got-countAfterFirst)
	}
	if second.Record.TargetBeforeSHA != source || second.Record.TargetAfterSHA != source {
		t.Fatalf("recovery record brackets a ref update that did not happen: %+v", second.Record)
	}
	// The audit that was missing now exists, and it describes an integration
	// rather than an attempt.
	records := good.all()
	if len(records) != 1 || records[0].Outcome != OutcomeIntegrated {
		t.Fatalf("recovery records = %+v, want exactly one integrated row", records)
	}
}

// Ten reconcile passes over one already-integrated task move the target exactly
// zero times. The property has to hold for repetition, not just for the second
// call: a daemon in a restart loop, or simply a board being polled, re-enters
// this path over and over.
func TestRepeatedReconcilePassesNeverAdvanceAnAlreadyIntegratedTarget(t *testing.T) {
	t.Parallel()
	f := newIntegFixture(t)
	f.commitInWorktree(t, "feature.txt", "task work\n", "task 1")
	rec := &coordRecorder{}
	coordinator := f.coordinator(t, &coordVerifier{result: Verification{Passed: true}}, rec)

	if _, err := coordinator.Integrate(context.Background(), f.request()); err != nil {
		t.Fatal(err)
	}
	landed := f.head(t, "main")
	count := f.commitCount(t, "main")

	for pass := 0; pass < 10; pass++ {
		outcome, err := coordinator.Integrate(context.Background(), f.request())
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if !outcome.Integrated {
			t.Fatalf("pass %d reported the work as not integrated: %+v", pass, outcome)
		}
		if got := f.head(t, "main"); got != landed {
			t.Fatalf("pass %d moved main: %s -> %s", pass, landed, got)
		}
	}
	if got := f.commitCount(t, "main"); got != count {
		t.Fatalf("main gained %d commit(s) over ten reconcile passes", got-count)
	}
	// The ledger tells the same story: exactly ONE attempt ever declared an
	// intent to move the ref -- the first -- and every pass after it recorded an
	// integration it did not have to perform. An intent row from a later pass
	// would mean a second compare-and-set was attempted against a ref that was
	// already where it needed to be.
	intents, integrated := 0, 0
	for i, r := range rec.all() {
		switch r.Outcome {
		case OutcomeAttempting:
			intents++
		case OutcomeIntegrated:
			integrated++
		default:
			t.Fatalf("record %d outcome = %q, want attempting or integrated", i, r.Outcome)
		}
	}
	if intents != 1 {
		t.Fatalf("%d passes declared an intent to move the ref, want 1", intents)
	}
	if integrated != 11 {
		t.Fatalf("%d passes recorded an integration, want 11 (the first plus ten reconciles)", integrated)
	}
}

// A reconcile pass over a task whose work is on the target but whose ao/*
// branch has since been deleted by cleanup does not silently succeed: the
// source cannot be resolved, and that is an error the caller must see rather
// than a quiet no-op.
//
// It is what makes the ordering in the caller load-bearing. Cleanup may only
// run after the promotion is durable precisely because this is what happens if
// it runs earlier.
func TestIntegratingATaskWhoseBranchWasAlreadyCleanedUpFails(t *testing.T) {
	t.Parallel()
	f := newIntegFixture(t)
	f.commitInWorktree(t, "feature.txt", "task work\n", "task 1")
	coordinator := f.coordinator(t, &coordVerifier{result: Verification{Passed: true}}, &coordRecorder{})
	if _, err := coordinator.Integrate(context.Background(), f.request()); err != nil {
		t.Fatal(err)
	}
	landed := f.head(t, "main")

	// Cleanup removed the worktree and the branch, as it does once the
	// promotion is durably recorded.
	integGit(t, f.binary, f.repo, "worktree", "remove", f.worktree)
	integGit(t, f.binary, f.repo, "update-ref", "-d", "refs/heads/"+f.source, landed)

	_, err := coordinator.Integrate(context.Background(), f.request())
	if err == nil {
		t.Fatal("integrating a task with no source branch was reported as success")
	}
	if !strings.Contains(err.Error(), f.source) {
		t.Fatalf("err = %v, want it to name the missing source branch %s", err, f.source)
	}
	if got := f.head(t, "main"); got != landed {
		t.Fatalf("main moved despite the failure: %s -> %s", landed, got)
	}
}

// coordinator2 builds a second Coordinator over the same repository, which is
// what a restarted daemon has: the same git, a fresh everything else.
func coordinator2(t *testing.T, f *integFixture, rec Recorder) *Coordinator {
	t.Helper()
	c, err := New(Deps{
		Git:      NewExecGit(f.binary),
		Locks:    newCoordFakeLocker(),
		Verifier: &coordVerifier{result: Verification{Passed: true}},
		Recorder: rec,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// flakyRecorder writes the intent row and then fails, which is how a crash
// between the ref update and the audit is simulated without killing the
// process.
type flakyRecorder struct {
	failAfterIntent bool
	records         []Record
}

func (r *flakyRecorder) RecordIntegration(_ context.Context, rec Record) error {
	r.records = append(r.records, rec)
	if r.failAfterIntent && rec.Outcome != OutcomeAttempting {
		return errors.New("audit store unavailable")
	}
	return nil
}

func (r *flakyRecorder) all() []Record { return append([]Record(nil), r.records...) }
