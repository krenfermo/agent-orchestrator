package workflow

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func obsOf(p domain.WorkerRuntimeProof) *domain.WorkerRuntimeObservation {
	return &domain.WorkerRuntimeObservation{SessionID: "sess-1", Proof: p, Detail: string(p)}
}

// P9 §26: the decision model is closed and table-tested. Every row names the
// invariant it holds.
func TestDecideWorkerAdoptionTable(t *testing.T) {
	claimed := time.Date(2026, 9, 13, 20, 0, 0, 0, time.UTC)
	base := workerAdoptionFacts{
		SessionID:        "sess-1",
		SessionLaunchID:  "launch-2",
		SessionWorktree:  "/wt/task-1",
		SessionBranch:    "ao/task-1",
		SessionCreatedAt: claimed.Add(300 * time.Millisecond),
		RecordedLaunchID: "launch-2",
		RecordedWorktree: "/wt/task-1/",
		RecordedBranch:   "ao/task-1",
		ClaimedAt:        claimed,
		Runtime:          obsOf(domain.WorkerRuntimeOwned),
	}
	with := func(mut func(*workerAdoptionFacts)) workerAdoptionFacts {
		f := base
		mut(&f)
		return f
	}
	cases := []struct {
		name   string
		facts  workerAdoptionFacts
		action WorkerRecoveryAction
		reason WorkerRecoveryReason
	}{
		{"C: matching runtime is adopted", base, WorkerRecoveryAdopt, WorkerReasonMatchingRuntime},
		{"owned but exited is still the same launch", with(func(f *workerAdoptionFacts) { f.Runtime = obsOf(domain.WorkerRuntimeOwnedExited) }), WorkerRecoveryAdopt, WorkerReasonRuntimeExited},
		{"I10: terminal run outranks everything", with(func(f *workerAdoptionFacts) { f.RunTerminal = true }), WorkerRecoveryNoop, WorkerReasonTerminalRun},
		{"I7: runtime proven absent is relaunchable, never adopted", with(func(f *workerAdoptionFacts) { f.Runtime = obsOf(domain.WorkerRuntimeAbsent) }), WorkerRecoveryRelaunch, WorkerReasonRuntimeMissing},
		{"I5/I16: another incarnation under the name", with(func(f *workerAdoptionFacts) { f.Runtime = obsOf(domain.WorkerRuntimeInstanceMismatch) }), WorkerRecoveryFailClosed, WorkerReasonInstanceMismatch},
		{"I2: token of another launch", with(func(f *workerAdoptionFacts) { f.Runtime = obsOf(domain.WorkerRuntimeOwnerMismatch) }), WorkerRecoveryFailClosed, WorkerReasonOwnerMismatch},
		{"I6: another installation", with(func(f *workerAdoptionFacts) { f.Runtime = obsOf(domain.WorkerRuntimeInstallationMismatch) }), WorkerRecoveryFailClosed, WorkerReasonInstallationMismatch},
		{"I17: legacy provenance", with(func(f *workerAdoptionFacts) { f.Runtime = obsOf(domain.WorkerRuntimeProvenanceMissing) }), WorkerRecoveryFailClosed, WorkerReasonLegacyProvenanceMissing},
		{"conpty: unsupported fails closed", with(func(f *workerAdoptionFacts) { f.Runtime = obsOf(domain.WorkerRuntimeUnsupported) }), WorkerRecoveryFailClosed, WorkerReasonRuntimeUnsupported},
		{"unavailable probe waits, concludes nothing", with(func(f *workerAdoptionFacts) {
			f.Runtime = obsOf(domain.WorkerRuntimeUnavailable)
			f.UnreadableSince = claimed
			f.Now = claimed.Add(time.Minute)
		}), WorkerRecoveryWait, WorkerReasonRuntimeUnavailable},
		{"an hours-old claim with a FIRST failed read still waits", with(func(f *workerAdoptionFacts) {
			f.Runtime = obsOf(domain.WorkerRuntimeUnavailable)
			f.Now = claimed.Add(6 * time.Hour)
			f.UnreadableSince = f.Now
		}), WorkerRecoveryWait, WorkerReasonRuntimeUnavailable},
		{"a runtime unreadable past the grace fails closed, never adopts", with(func(f *workerAdoptionFacts) {
			f.Runtime = obsOf(domain.WorkerRuntimeUnavailable)
			f.UnreadableSince = claimed
			f.Now = claimed.Add(workerRuntimeUnreadableGrace + time.Second)
		}), WorkerRecoveryFailClosed, WorkerReasonRuntimeUnavailable},
		{"out-of-vocabulary proof waits", with(func(f *workerAdoptionFacts) { f.Runtime = obsOf("bogus") }), WorkerRecoveryWait, WorkerReasonRuntimeUnavailable},
		{"I14: recorded launch disagrees", with(func(f *workerAdoptionFacts) { f.RecordedLaunchID = "launch-1" }), WorkerRecoveryFailClosed, WorkerReasonLaunchMismatch},
		{"I13: no recorded launch, session inside this claim", with(func(f *workerAdoptionFacts) { f.RecordedLaunchID = "" }), WorkerRecoveryAdopt, WorkerReasonMatchingRuntime},
		{"I9: no recorded launch, session older than the claim in force", with(func(f *workerAdoptionFacts) {
			f.RecordedLaunchID = ""
			f.SessionCreatedAt = claimed.Add(-10 * time.Minute)
		}), WorkerRecoveryFailClosed, WorkerReasonGenerationMismatch},
		{"I15: worktree mismatch", with(func(f *workerAdoptionFacts) { f.RecordedWorktree = "/wt/task-2" }), WorkerRecoveryFailClosed, WorkerReasonWorktreeMismatch},
		{"I15: branch mismatch", with(func(f *workerAdoptionFacts) { f.RecordedBranch = "ao/task-2" }), WorkerRecoveryFailClosed, WorkerReasonBranchMismatch},
		{"unrecorded workspace never contradicts", with(func(f *workerAdoptionFacts) { f.RecordedWorktree, f.RecordedBranch = "", "" }), WorkerRecoveryAdopt, WorkerReasonMatchingRuntime},
		{"a contradicting runtime outranks agreeing durable facts", with(func(f *workerAdoptionFacts) {
			f.Runtime = obsOf(domain.WorkerRuntimeOwnerMismatch)
			f.RecordedWorktree = "/wt/other"
		}), WorkerRecoveryFailClosed, WorkerReasonOwnerMismatch},
		{"pre-P9 fixture: no port wired keeps the launch fence", with(func(f *workerAdoptionFacts) { f.Runtime = nil }), WorkerRecoveryAdopt, WorkerReasonRuntimeProofNotWired},
		{"pre-P9 fixture: no port wired still refuses a stated launch disagreement", with(func(f *workerAdoptionFacts) {
			f.Runtime = nil
			f.RecordedLaunchID = "launch-1"
		}), WorkerRecoveryFailClosed, WorkerReasonLaunchMismatch},
		{"pre-P9 fixture: the generation fence needs a runtime proof to apply", with(func(f *workerAdoptionFacts) {
			f.Runtime = nil
			f.RecordedLaunchID = ""
			f.SessionCreatedAt = claimed.Add(-time.Hour)
		}), WorkerRecoveryAdopt, WorkerReasonRuntimeProofNotWired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideWorkerAdoption(tc.facts)
			if got.Action != tc.action || got.Reason != tc.reason {
				t.Fatalf("decision = %s/%s (%s), want %s/%s", got.Action, got.Reason, got.Detail, tc.action, tc.reason)
			}
		})
	}
}

// §26/I18: the decision is a pure function — the same facts give the same
// answer however often it is asked.
func TestDecideWorkerAdoptionIsDeterministic(t *testing.T) {
	f := workerAdoptionFacts{SessionID: "s", Runtime: obsOf(domain.WorkerRuntimeOwnerMismatch)}
	first := decideWorkerAdoption(f)
	for i := 0; i < 100; i++ {
		if got := decideWorkerAdoption(f); got != first {
			t.Fatalf("pass %d: %+v != %+v", i, got, first)
		}
	}
}

// The ownedExecution accessors under a runtime proof: only `owned` is live,
// only absent/owned_exited are gone, and every contradiction is Unprovable —
// the gap that stops a reconciler from relaunching over something it could not
// prove is dead, or adopting something it could not prove is its own.
func TestOwnedExecutionUnderRuntimeProof(t *testing.T) {
	live := ownedExecution{SessionID: "s", Evidence: SessionOwnershipEvidence{Observed: true}, RowFound: true,
		LivenessKnown: true, LivenessAlive: true}
	cases := []struct {
		proof                  domain.WorkerRuntimeProof
		wantLive, wantGone     bool
		wantUnprovableOwnerRsn bool
	}{
		{domain.WorkerRuntimeOwned, true, false, false},
		{domain.WorkerRuntimeOwnedExited, false, true, false},
		{domain.WorkerRuntimeAbsent, false, true, false},
		{domain.WorkerRuntimeInstanceMismatch, false, false, true},
		{domain.WorkerRuntimeOwnerMismatch, false, false, true},
		{domain.WorkerRuntimeInstallationMismatch, false, false, true},
		{domain.WorkerRuntimeProvenanceMissing, false, false, true},
		{domain.WorkerRuntimeUnsupported, false, false, true},
		{domain.WorkerRuntimeUnavailable, false, false, false},
	}
	for _, tc := range cases {
		o := live
		o.Runtime = obsOf(tc.proof)
		if o.Live() != tc.wantLive || o.ProvenGone() != tc.wantGone {
			t.Errorf("%s: live=%v gone=%v, want live=%v gone=%v", tc.proof, o.Live(), o.ProvenGone(), tc.wantLive, tc.wantGone)
		}
		if o.runtimeOwnershipUnproven() != tc.wantUnprovableOwnerRsn {
			t.Errorf("%s: runtimeOwnershipUnproven=%v", tc.proof, o.runtimeOwnershipUnproven())
		}
		if !tc.wantLive && !tc.wantGone && !o.Unprovable() {
			t.Errorf("%s: neither live nor gone must be Unprovable", tc.proof)
		}
	}
}
