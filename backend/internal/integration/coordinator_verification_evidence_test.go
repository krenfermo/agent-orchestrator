package integration

import (
	"context"
	"testing"
)

// The reviewer's third finding, as tests.
//
// "verificationRan: false" on a fast-forward or a direct-branch proof described
// the coordinator's own activity and misdescribed the fact a reader needs:
// something DID authorize that ref update — the task's own verify step — and
// the audit row said nothing about it. These check the two halves of the fix:
// real evidence is recorded when it still applies, and is never recorded when
// it does not.

func evidenceCoordinator(t *testing.T, git *coordStubGit, verifier *coordVerifier) (*Coordinator, *coordRecorder) {
	t.Helper()
	rec := &coordRecorder{}
	deps := Deps{Git: git, Locks: newCoordFakeLocker(), Recorder: rec}
	if verifier != nil {
		deps.Verifier = verifier
	}
	c, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	return c, rec
}

func readyRequest() Request {
	return Request{
		WorkflowRunID: "wf-1", TaskID: "task-1",
		RepoPath: "/repo", WorktreePath: "/wt", TargetBranch: "main", SourceBranch: "ao/task-1",
		Readiness: Readiness{Review: ReviewApproved, Verify: VerifyPassed},
	}
}

// A fast-forward records the verification that actually authorized it, with the
// links into the durable evidence behind the verdict.
func TestFastForwardRecordsTheVerificationThatAuthorizedIt(t *testing.T) {
	git := &coordStubGit{target: "target00", source: "source00", contains: true}
	c, rec := evidenceCoordinator(t, git, nil)

	req := readyRequest()
	req.SourceFingerprint = "fp-verified"
	req.Verified = Verification{
		Ran: true, Passed: true, Fingerprint: "fp-verified",
		StepID: "wfs-verify-1", EvidenceID: "wfc-verify-1", Summary: "12 checks passed",
	}
	outcome, err := c.Integrate(context.Background(), req)
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if !outcome.Integrated {
		t.Fatalf("outcome = %+v, want integrated", outcome)
	}
	got := outcome.Record.Verification
	if !got.Ran || !got.Passed {
		t.Fatalf("verification = %+v; a fast-forward authorized by a passing verify must not report Ran=false", got)
	}
	if got.Source != SourceTaskVerification {
		t.Fatalf("source = %q, want %q", got.Source, SourceTaskVerification)
	}
	if got.StepID != "wfs-verify-1" || got.EvidenceID != "wfc-verify-1" || got.Fingerprint != "fp-verified" {
		t.Fatalf("evidence links lost: %+v", got)
	}
	// And it is on the durable record, not only in the returned value.
	all := rec.all()
	if len(all) == 0 || all[len(all)-1].Verification.EvidenceID != "wfc-verify-1" {
		t.Fatalf("the ledger row carries no verification evidence: %+v", all)
	}
}

// A caller with no durable verification claims none. Ran=false is the honest
// answer THERE, which is what makes it meaningful above.
func TestNoClaimedVerificationIsRecordedAsNoVerification(t *testing.T) {
	git := &coordStubGit{target: "target00", source: "source00", contains: true}
	c, _ := evidenceCoordinator(t, git, nil)

	outcome, err := c.Integrate(context.Background(), readyRequest())
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if outcome.Record.Verification.Ran {
		t.Fatalf("verification = %+v, want no claim at all", outcome.Record.Verification)
	}
}

// The other half: work that changed after it was verified must not be credited
// with the old verdict. It is re-verified, and the record says so.
func TestStaleVerificationIsRevalidatedRatherThanReused(t *testing.T) {
	git := &coordStubGit{target: "target00", source: "source00", contains: true}
	verifier := &coordVerifier{result: Verification{Passed: true, Summary: "re-ran and passed"}}
	c, _ := evidenceCoordinator(t, git, verifier)

	req := readyRequest()
	// The worktree has moved on since the verification.
	req.SourceFingerprint = "fp-now"
	req.Verified = Verification{Ran: true, Passed: true, Fingerprint: "fp-when-verified", EvidenceID: "wfc-verify-1"}

	outcome, err := c.Integrate(context.Background(), req)
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if !outcome.Integrated {
		t.Fatalf("outcome = %+v, want integrated after a successful re-verification", outcome)
	}
	if verifier.calls != 1 {
		t.Fatalf("verifier calls = %d, want exactly 1 — a stale verdict must be re-established, not reused", verifier.calls)
	}
	got := outcome.Record.Verification
	if got.Source != SourceRevalidated {
		t.Fatalf("source = %q, want %q", got.Source, SourceRevalidated)
	}
	if got.EvidenceID == "wfc-verify-1" {
		t.Fatal("the stale evidence id was carried onto a fresh verdict")
	}
	if got.Fingerprint != "fp-now" {
		t.Fatalf("fingerprint = %q, want the content actually verified", got.Fingerprint)
	}
}

// A stale verdict that cannot be re-established stops. Integrating anyway would
// put an authorization in the ledger that authorized different content.
func TestStaleVerificationWithNothingToRevalidateStops(t *testing.T) {
	git := &coordStubGit{target: "target00", source: "source00", contains: true}
	c, rec := evidenceCoordinator(t, git, nil) // no verifier at all

	req := readyRequest()
	req.SourceFingerprint = "fp-now"
	req.Verified = Verification{Ran: true, Passed: true, Fingerprint: "fp-when-verified"}

	outcome, err := c.Integrate(context.Background(), req)
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if outcome.Integrated || outcome.Attention == nil {
		t.Fatalf("outcome = %+v, want a stop", outcome)
	}
	if outcome.Attention.Reason != ReasonStaleVerification {
		t.Fatalf("reason = %q, want %q", outcome.Attention.Reason, ReasonStaleVerification)
	}
	// Nothing was written to the target.
	for _, call := range git.called() {
		if len(call) >= 3 && call[:3] == "cas" {
			t.Fatalf("the ref was moved despite a stale verification: %v", git.called())
		}
	}
	if all := rec.all(); len(all) != 1 || all[0].Outcome != OutcomeNeedsAttention {
		t.Fatalf("ledger = %+v, want exactly one needs-attention row", all)
	}
}

// A re-verification that FAILS is a task-level stop, not a landing.
func TestStaleVerificationThatFailsRevalidationStops(t *testing.T) {
	git := &coordStubGit{target: "target00", source: "source00", contains: true}
	verifier := &coordVerifier{result: Verification{Passed: false, Summary: "2 checks failed"}}
	c, _ := evidenceCoordinator(t, git, verifier)

	req := readyRequest()
	req.SourceFingerprint = "fp-now"
	req.Verified = Verification{Ran: true, Passed: true, Fingerprint: "fp-when-verified"}

	outcome, err := c.Integrate(context.Background(), req)
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if outcome.Attention == nil || outcome.Attention.Reason != ReasonVerificationFailed {
		t.Fatalf("outcome = %+v, want a verification-failed stop", outcome)
	}
	if !outcome.Record.Verification.Ran || outcome.Record.Verification.Passed {
		t.Fatalf("the record must state that a verification ran and failed: %+v", outcome.Record.Verification)
	}
}

// A no-replay (direct-branch) integration is a real integration through this
// coordinator, and it names the strategy honestly: nothing was forwarded.
func TestNoReplayIntegrationTakesTheCoordinatorAndRecordsANoOp(t *testing.T) {
	git := &coordStubGit{target: "head0000", source: "head0000", contains: true}
	c, rec := evidenceCoordinator(t, git, nil)

	preconditionRan := false
	outcome, err := c.Integrate(context.Background(), Request{
		WorkflowRunID: "wf-1", TaskID: "task-1",
		RepoPath: "/repo", TargetBranch: "main", SourceBranch: "main",
		NoReplay:  true,
		Readiness: Readiness{Review: ReviewApproved, Verify: VerifyPassed},
		Verified: Verification{Ran: true, Passed: true, Fingerprint: "head0000",
			EvidenceID: "wfc-verify-1"},
		Precondition: func(_ context.Context, targetSHA, _ string) (string, error) {
			preconditionRan = true
			return targetSHA, nil
		},
	})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if !preconditionRan {
		t.Fatal("the caller's in-lane freshness check never ran")
	}
	if !outcome.Integrated {
		t.Fatalf("outcome = %+v, want integrated", outcome)
	}
	if outcome.Record.Strategy != StrategyNoOp {
		t.Fatalf("strategy = %q, want %q", outcome.Record.Strategy, StrategyNoOp)
	}
	// It took the lane, gated on readiness, and left the same audit row every
	// other mode leaves — including the verification that authorized it.
	all := rec.all()
	if len(all) != 1 {
		t.Fatalf("ledger rows = %d, want exactly 1", len(all))
	}
	if all[0].Verification.EvidenceID != "wfc-verify-1" || all[0].Outcome != OutcomeIntegrated {
		t.Fatalf("row = %+v", all[0])
	}
	// And it never ran a mutating git command: there was nothing to move.
	for _, call := range git.called() {
		switch {
		case len(call) >= 3 && call[:3] == "cas",
			len(call) >= 6 && call[:6] == "rebase",
			len(call) >= 8 && call[:8] == "checkout":
			t.Fatalf("a no-op integration ran %q", call)
		}
	}
}

// The precondition is the caller's own freshness check, and its refusal is a
// stop with the caller's reason — never a silent landing.
func TestPreconditionRefusalStopsWithoutTouchingTheTarget(t *testing.T) {
	git := &coordStubGit{target: "head0000", source: "head0000", contains: true}
	c, _ := evidenceCoordinator(t, git, nil)

	outcome, err := c.Integrate(context.Background(), Request{
		WorkflowRunID: "wf-1", TaskID: "task-1",
		RepoPath: "/repo", TargetBranch: "main", SourceBranch: "main",
		NoReplay:  true,
		Readiness: Readiness{Review: ReviewApproved, Verify: VerifyPassed},
		Precondition: func(context.Context, string, string) (string, error) {
			return "", errPreconditionForTest
		},
	})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if outcome.Attention == nil || outcome.Attention.Reason != ReasonPreconditionFailed {
		t.Fatalf("outcome = %+v, want a precondition-failed stop", outcome)
	}
	for _, call := range git.called() {
		if len(call) >= 3 && call[:3] == "cas" {
			t.Fatalf("the ref moved despite a refused precondition: %v", git.called())
		}
	}
}

var errPreconditionForTest = errTest("the branch no longer holds the verified commit")

type errTest string

func (e errTest) Error() string { return string(e) }
