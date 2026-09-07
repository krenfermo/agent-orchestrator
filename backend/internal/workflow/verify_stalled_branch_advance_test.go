package workflow_test

// The two halves of the wf-0aadfcde incident, composed.
//
// verify_stalled_attempt.go converts a failed verification whose attempt
// identity can never move into a durable stop named verify_attempt_unretryable.
// That is the whole of its job: it makes the dead end visible. What it must NOT
// do is make the dead end permanent — and the thing that would have made it
// permanent is a stop reason no recovery door accepts.
//
// The door that fits this run is the third of ContinueRun's four:
// resumeBranchAdvancedVerify. Its evidence is a Git ancestry proof — the branch
// grew commits ON TOP of the reviewed one, which still contains it, so nothing
// was lost and only the review went stale. wf-0aadfcde is exactly that shape:
//
//	approved fingerprint 0efec0e4…  read at commit b016b224a
//	current HEAD                    several commits later, b016b224a an ancestor
//	recorded failure                verify_workspace_changed
//	stop                            verify_attempt_unretryable
//
// These tests pin that the two mechanisms compose: the stop is reachable, and
// the reopen accepts it — and, just as importantly, that the reopen still
// refuses every shape where the ancestry cannot be proved. The refusals matter
// more than the acceptance: a stop reason that opened a door for a rewritten
// history would be worse than the loop it replaced.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// parkOnUnretryableVerifyAttempt puts the fixture into the exact durable shape
// wf-0aadfcde was found in, and the shape verify_stalled_attempt.go leaves
// behind: a verification that failed on a workspace change, an attempt nothing
// can re-ask, a verify step driven terminal, and the run parked under
// verify_attempt_unretryable.
//
// It is written directly rather than driven, because the route into it ran
// through two defects that no longer exist. The rows are what matter here: they
// are on disk in production today, and the recovery has to act on them.
func (fx *branchAdvancedFixture) parkOnUnretryableVerifyAttempt(currentFingerprint string) {
	fx.t.Helper()
	runID := fx.runID
	verifyStepID := "verify"
	targetKey := "target-key-" + fx.approvedFingerprint[:12]

	result := workflowcore.VerifyResult{
		Version:             "v1",
		Passed:              false,
		TargetKey:           targetKey,
		ReviewedFingerprint: fx.approvedFingerprint,
		PreFingerprint:      currentFingerprint,
		ErrorClass:          domain.WorkflowErrorVerifyWorkspaceChanged,
	}
	payload, err := json.Marshal(result)
	if err != nil {
		fx.t.Fatal(err)
	}
	fx.clk.Advance(time.Minute)
	fx.store.checkpoints[runID] = append(fx.store.checkpoints[runID], domain.WorkflowCheckpoint{
		ID: "cp-verify-result", WorkflowRunID: runID, WorkflowStepID: &verifyStepID,
		ProjectID: "project-1", DurablePhase: "verify_result", PayloadVersion: "v1",
		RetryState: string(payload), NextAction: "verify_failed", CreatedAt: fx.clk.Now(),
	})
	// The stop verify_stalled_attempt.go records, written last so it is the
	// reason the run resolves to.
	fx.clk.Advance(time.Minute)
	fx.store.checkpoints[runID] = append(fx.store.checkpoints[runID], domain.WorkflowCheckpoint{
		ID: "cp-stop-unretryable", WorkflowRunID: runID, WorkflowStepID: &verifyStepID,
		ProjectID: "project-1", DurablePhase: workflowcore.ReasonVerifyAttemptUnretryable,
		PayloadVersion: "v1", RetryState: "{}",
		NextAction: "verify attempt already failed (verify_workspace_changed) and nothing can ask that target again",
		CreatedAt:  fx.clk.Now(),
	})

	steps := fx.store.steps[runID]
	for i := range steps {
		if steps[i].Kind == domain.WorkflowStepVerify {
			steps[i].State = domain.WorkflowStepFailed
		}
	}
	fx.store.steps[runID] = steps
	run := fx.store.runs[runID]
	run.State = domain.WorkflowRunNeedsAttention
	fx.store.runs[runID] = run
}

// ---- the incident, recovered ------------------------------------------------

// THE COMPOSITION. A run parked under verify_attempt_unretryable, whose branch
// grew commits on top of the approved one, is reopened by exactly one Continue
// and ends in exactly one fresh independent review.
func TestUnretryableVerifyStopReopensAsABranchAdvanceOnContinue(t *testing.T) {
	fx := newBranchAdvancedFixture(t)
	current := fx.commitOnTop("later.txt")
	fx.parkOnUnretryableVerifyAttempt(current)

	// A poll is not an authorization: the terminal verify step stays terminal.
	fx.poll(5)
	if got := fx.recoveries(); got != 0 {
		t.Fatalf("branch-advance recoveries after 5 polls = %d, want 0", got)
	}
	if got := fx.runState(); got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want it still parked", got)
	}

	// A person presses Continue, once.
	fx.clk.Advance(time.Minute)
	if _, err := fx.coord.ContinueRun(context.Background(), fx.runID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}

	if got := fx.recoveries(); got != 1 {
		t.Fatalf("branch-advance recoveries = %d, want exactly 1: verify_attempt_unretryable must reach this door", got)
	}
	if got := fx.runState(); got == domain.WorkflowRunNeedsAttention {
		t.Fatal("the run is still parked after an authorized branch-advance recovery")
	}
	if got := fx.stepState(domain.WorkflowStepVerify); got.Terminal() {
		t.Fatalf("verify step = %q, want a non-terminal state so re-verification is possible", got)
	}

	// One reviewer, for the workspace as it stands — never the stale approval.
	fx.poll(2)
	if fx.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches = %d, want exactly 1", fx.launcher.launchCalls)
	}
	fresh := fx.freshReview()
	if fresh.ID == fx.priorReviewID {
		t.Fatal("the stale approval was reused instead of a fresh review")
	}
	if fresh.TargetSHA == fx.approvedFingerprint {
		t.Fatalf("the fresh review targets the stale fingerprint %s", fresh.TargetSHA)
	}

	// And the run completes on the fresh approval, through verification.
	fx.approveFreshReview()
	fx.poll(3)
	if got := fx.runState(); got != domain.WorkflowRunCompleted {
		t.Fatalf("run state = %q, want completed after the fresh review approved and verification passed", got)
	}
}

// A second Continue is not a second recovery, and repeated Continues do not
// spend budget or launch reviewers. The bound is the ledger, not the button.
func TestRepeatedContinuesOnAnUnretryableStopProduceOneRecovery(t *testing.T) {
	fx := newBranchAdvancedFixture(t)
	current := fx.commitOnTop("later.txt")
	fx.parkOnUnretryableVerifyAttempt(current)

	for i := 0; i < 4; i++ {
		fx.clk.Advance(time.Minute)
		if _, err := fx.coord.ContinueRun(context.Background(), fx.runID); err != nil {
			t.Fatalf("ContinueRun %d: %v", i, err)
		}
	}
	if got := fx.recoveries(); got != 1 {
		t.Fatalf("branch-advance recoveries after 4 Continues = %d, want exactly 1", got)
	}
	fx.poll(2)
	if fx.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches = %d, want exactly 1", fx.launcher.launchCalls)
	}
}

// ---- the refusals, which matter more ----------------------------------------

// A history that was REWRITTEN is not a branch advance, however parked the run
// is. The approved commit is no longer reachable from HEAD, so AO cannot say
// nothing was lost, and the stop reason buys no licence at all.
func TestUnretryableVerifyStopDoesNotReopenARewrittenHistory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rewrite func(fx *branchAdvancedFixture)
	}{
		{"amend", func(fx *branchAdvancedFixture) {
			if err := os.WriteFile(filepath.Join(fx.repo, "seed.txt"), []byte("amended\n"), 0o600); err != nil {
				fx.t.Fatal(err)
			}
			fx.git("add", ".")
			fx.git("commit", "--amend", "-m", "amended seed")
		}},
		{"orphan branch", func(fx *branchAdvancedFixture) {
			fx.commitOnTop("later.txt")
			fx.git("checkout", "--orphan", "rewritten")
			fx.git("add", ".")
			fx.git("commit", "-m", "rewritten history")
			fx.git("branch", "-M", "main")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newBranchAdvancedFixture(t)
			tc.rewrite(fx)
			current := fx.observeHead()
			fx.parkOnUnretryableVerifyAttempt(current)

			fx.clk.Advance(time.Minute)
			if _, err := fx.coord.ContinueRun(context.Background(), fx.runID); err != nil {
				t.Fatalf("ContinueRun: %v", err)
			}

			if got := fx.recoveries(); got != 0 {
				t.Fatalf("branch-advance recoveries = %d, want 0: a rewritten history is a person's decision", got)
			}
			if got := fx.runState(); got != domain.WorkflowRunNeedsAttention {
				t.Fatalf("run state = %q, want it still parked", got)
			}
			if got := fx.stepState(domain.WorkflowStepVerify); got != domain.WorkflowStepFailed {
				t.Fatalf("verify step = %q, want it still failed", got)
			}
			if fx.launcher.launchCalls != 0 {
				t.Fatalf("reviewer launches = %d, want 0", fx.launcher.launchCalls)
			}
		})
	}
}

// An approved head AO cannot name is an unprovable baseline, and an unprovable
// baseline is never evidence of innocence. Without it there is no ancestry
// question to ask, so the door stays shut.
func TestUnretryableVerifyStopDoesNotReopenWithoutAProvableApprovedHead(t *testing.T) {
	fx := newBranchAdvancedFixture(t)
	current := fx.commitOnTop("later.txt")

	// Erase every durable record of the commit the approval was read at: the
	// work step's completion commit is the last fallback approvedHeadSHA has.
	cps := fx.store.checkpoints[fx.runID]
	for i := range cps {
		cps[i].HeadSHA = ""
	}
	fx.store.checkpoints[fx.runID] = cps

	fx.parkOnUnretryableVerifyAttempt(current)
	fx.clk.Advance(time.Minute)
	if _, err := fx.coord.ContinueRun(context.Background(), fx.runID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}

	if got := fx.recoveries(); got != 0 {
		t.Fatalf("branch-advance recoveries = %d, want 0 without a provable approved head", got)
	}
	if got := fx.runState(); got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want it still parked", got)
	}
	if fx.launcher.launchCalls != 0 {
		t.Fatalf("reviewer launches = %d, want 0", fx.launcher.launchCalls)
	}
}

// The HEAD did not move at all: whatever changed is uncommitted, which is a
// different question with a different, separately authorized answer. This door
// must not become a second route to it.
func TestUnretryableVerifyStopDoesNotReopenWhenTheHeadNeverMoved(t *testing.T) {
	fx := newBranchAdvancedFixture(t)
	// No commit on top. The fingerprint moves because the tree is dirty, not
	// because the branch advanced.
	obs := fx.ws.obs
	obs.Dirty = true
	obs.Changes = []ports.WorkspaceChange{{Path: "wip.go", Status: " M"}}
	fx.ws.obs = obs
	current := workflowcore.WorkspaceFingerprint(obs)

	fx.parkOnUnretryableVerifyAttempt(current)
	fx.clk.Advance(time.Minute)
	if _, err := fx.coord.ContinueRun(context.Background(), fx.runID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}

	if got := fx.recoveries(); got != 0 {
		t.Fatalf("branch-advance recoveries = %d, want 0: an unmoved HEAD is not a branch advance", got)
	}
	if got := fx.runState(); got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want it still parked", got)
	}
}
