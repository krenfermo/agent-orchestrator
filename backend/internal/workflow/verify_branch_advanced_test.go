package workflow_test

// Bounded recovery of ONE shape of verify_workspace_changed: the branch grew
// commits on top of the reviewed one (verify_branch_advanced.go).
//
// The fixture below uses a REAL git repository, because every claim the
// mechanism makes is a claim about git: whether the approved commit still
// exists, whether it is still reachable from the head, and whether the head is
// the tip of the branch AO was authorized to work on. A stub answering those
// questions would assert the fixture's opinion rather than the system's.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

type branchAdvancedFixture struct {
	t        *testing.T
	coord    *workflowcore.Coordinator
	store    *fakeStore
	clk      *fakeClock
	reviews  *fakeReviewRuns
	launcher *fakeReviewerLauncher
	ws       *mutableWorkspaceFacts
	facts    *fakeSessionFacts
	runID    string
	repo     string
	sid      string
	// approvedFingerprint (F1) is what the review approved, and approvedHead
	// the commit it was read at.
	approvedFingerprint string
	approvedHead        string
	priorReviewID       string
}

// newBranchAdvancedFixture reaches the exact resting state the recovery starts
// from: work completed, a real reviewer approved F1 at commit H1, and the
// verify step has not run yet. Nothing is parked and no recovery generation
// exists — this run has never been reopened by anybody, which is the point:
// the mechanism under test must work without one.
func newBranchAdvancedFixture(t *testing.T) *branchAdvancedFixture {
	t.Helper()
	repo := newAutoTestRepo(t)
	fx := &branchAdvancedFixture{t: t, repo: repo, priorReviewID: "review-approved-h1", sid: "sess-branch-advanced"}
	fx.approvedHead = fx.head()

	store := newFakeStore()
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	runID := "wf-branch-advanced"
	fx.runID = runID

	plan := workflowcore.VerificationPlan{
		Files: []workflowcore.VerificationFileCheck{{Path: "seed.txt", Exists: true}},
	}
	artifactJSON, err := workflowcore.MarshalPlanArtifact(
		workflowcore.BuildPlanArtifact("project-1", "branch advance objective", "v1", plan))
	if err != nil {
		t.Fatal(err)
	}
	store.steps[runID] = []domain.WorkflowStep{
		{ID: "plan", WorkflowRunID: runID, Kind: domain.WorkflowStepPlan, Ordinal: 1, State: domain.WorkflowStepCompleted, ArtifactJSON: artifactJSON},
		{ID: "work", WorkflowRunID: runID, Kind: domain.WorkflowStepWork, Ordinal: 2, State: domain.WorkflowStepCompleted, SessionID: &fx.sid},
		{ID: "review", WorkflowRunID: runID, Kind: domain.WorkflowStepReview, Ordinal: 3, State: domain.WorkflowStepCompleted, ReviewRunID: &fx.priorReviewID},
		{ID: "fix", WorkflowRunID: runID, Kind: domain.WorkflowStepFix, Ordinal: 4, State: domain.WorkflowStepWaiting},
		{ID: "verify", WorkflowRunID: runID, Kind: domain.WorkflowStepVerify, Ordinal: 5, State: domain.WorkflowStepPending},
		{ID: "advance", WorkflowRunID: runID, Kind: domain.WorkflowStepAdvance, Ordinal: 6, State: domain.WorkflowStepPending},
	}
	store.runs[runID] = domain.WorkflowRun{
		ID: runID, ProjectID: "project-1", Objective: "branch advance objective", State: domain.WorkflowRunWaiting,
		PolicyVersion: "v1", PolicySnapshot: `{"maxFixCycles":3}`, CreatedAt: now, UpdatedAt: now,
	}

	approved := ports.WorkspaceObservation{Path: repo, Branch: "main", HeadSHA: fx.approvedHead}
	fx.approvedFingerprint = workflowcore.WorkspaceFingerprint(approved)
	workStepID := "work"
	store.checkpoints[runID] = []domain.WorkflowCheckpoint{{
		ID: "work-cp", WorkflowRunID: runID, WorkflowStepID: &workStepID, ProjectID: "project-1",
		SessionID: &fx.sid, Branch: "main", WorktreePath: repo, HeadSHA: fx.approvedHead,
		FingerprintAfter: fx.approvedFingerprint, CreatedAt: now,
	}}

	fx.reviews = newFakeReviewRuns()
	fx.reviews.runs[fx.priorReviewID] = domain.ReviewRun{
		ID: fx.priorReviewID, SessionID: domain.SessionID(fx.sid), Harness: domain.ReviewerClaudeCode,
		Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved,
		TargetSHA: fx.approvedFingerprint, CreatedAt: now,
	}
	// The worker is done and has been quiet for an hour: nothing of AO's can
	// still be landing in this tree.
	fx.facts = newFakeSessionFacts()
	fx.facts.put(domain.SessionRecord{
		ID: domain.SessionID(fx.sid), ProjectID: "project-1",
		Activity:        domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-time.Hour)},
		TurnCompletedAt: now.Add(-time.Hour),
		Metadata:        domain.SessionMetadata{Branch: "main", WorkspacePath: repo},
	})

	fx.store, fx.clk = store, &fakeClock{t: now}
	fx.ws = &mutableWorkspaceFacts{obs: approved}
	fx.launcher = &fakeReviewerLauncher{}
	fx.coord = fx.newCoordinator()
	return fx
}

// newCoordinator builds a Coordinator over the fixture's durable state. A
// SECOND one built from the same store is exactly what a daemon restart is,
// which is how the restart test below is written.
func (fx *branchAdvancedFixture) newCoordinator() *workflowcore.Coordinator {
	ids := 0
	return workflowcore.New(workflowcore.Deps{
		Store: fx.store, ReviewRuns: fx.reviews, WorkspaceFacts: fx.ws,
		SessionFacts: fx.facts, Verifier: &scriptedVerifyRunner{
			respond: func(workflowcore.VerifyCommandRequest) (workflowcore.VerifyCommandExecution, error) {
				return workflowcore.VerifyCommandExecution{ExitCode: 0}, nil
			},
		},
		MessageSender:    &fakeMessageSender{},
		ReviewerLauncher: fx.launcher,
		Clock:            fx.clk.Now,
		NewID:            func() string { ids++; return fmt.Sprintf("ba%d-%d", fx.launcher.launchCalls, ids) },
	})
}

func (fx *branchAdvancedFixture) git(args ...string) string {
	fx.t.Helper()
	out, err := autoGit(fx.repo, args...)
	if err != nil {
		fx.t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(out)
}

func (fx *branchAdvancedFixture) head() string {
	fx.t.Helper()
	return fx.git("rev-parse", "HEAD")
}

// commitOnTop is the situation the recovery exists for: the branch grows one
// more commit while this task waits. Nothing the task did is touched.
func (fx *branchAdvancedFixture) commitOnTop(name string) string {
	fx.t.Helper()
	if err := os.WriteFile(filepath.Join(fx.repo, name), []byte("later work\n"), 0o600); err != nil {
		fx.t.Fatal(err)
	}
	fx.git("add", ".")
	fx.git("commit", "-m", "later: "+name)
	return fx.observeHead()
}

// observeHead points the workspace observation at the repository's real head,
// which is what moves the fingerprint away from the approval.
func (fx *branchAdvancedFixture) observeHead() string {
	fx.t.Helper()
	obs := fx.ws.obs
	obs.HeadSHA = fx.head()
	fx.ws.obs = obs
	fp := workflowcore.WorkspaceFingerprint(obs)
	if fp == fx.approvedFingerprint {
		fx.t.Fatal("the fixture did not move the workspace fingerprint")
	}
	return fp
}

func (fx *branchAdvancedFixture) poll(times int) {
	fx.t.Helper()
	for i := 0; i < times; i++ {
		fx.clk.Advance(10 * time.Second)
		if _, err := fx.coord.GetRun(context.Background(), fx.runID); err != nil {
			fx.t.Fatalf("GetRun: %v", err)
		}
	}
}

func (fx *branchAdvancedFixture) recoveries() int {
	return len(checkpointsByPhase(fx.store, fx.runID, "verify_branch_advanced_fresh_review"))
}

func (fx *branchAdvancedFixture) runState() domain.WorkflowRunState {
	return fx.store.runs[fx.runID].State
}

func (fx *branchAdvancedFixture) stepState(kind domain.WorkflowStepKind) domain.WorkflowStepState {
	fx.t.Helper()
	for _, s := range fx.store.steps[fx.runID] {
		if s.Kind == kind {
			return s.State
		}
	}
	fx.t.Fatalf("no %s step", kind)
	return ""
}

// freshReview returns the review run that is not the stale approval.
func (fx *branchAdvancedFixture) freshReview() domain.ReviewRun {
	fx.t.Helper()
	var found []domain.ReviewRun
	for id, r := range fx.reviews.runs {
		if id != fx.priorReviewID && r.Status != domain.ReviewRunFailed {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		fx.t.Fatalf("fresh review runs = %d, want exactly 1: %+v", len(found), found)
	}
	return found[0]
}

func (fx *branchAdvancedFixture) approveFreshReview() {
	fx.t.Helper()
	rr := fx.freshReview()
	rr.Status, rr.Verdict, rr.Body = domain.ReviewRunComplete, domain.VerdictApproved, "approved: the branch as it stands"
	fx.reviews.runs[rr.ID] = rr
}

func (fx *branchAdvancedFixture) verifyResults() []workflowcore.VerifyResult {
	fx.t.Helper()
	var out []workflowcore.VerifyResult
	for _, cp := range checkpointsByPhase(fx.store, fx.runID, "verify_result") {
		var res workflowcore.VerifyResult
		if err := json.Unmarshal([]byte(cp.RetryState), &res); err != nil {
			fx.t.Fatalf("verify_result checkpoint is unreadable: %v", err)
		}
		out = append(out, res)
	}
	return out
}

// ---- 1. the branch advanced by commits on top: recoverable -----------------

func TestBranchAdvancedByCommitsOnTopIsRecoverable(t *testing.T) {
	fx := newBranchAdvancedFixture(t)
	advanced := fx.commitOnTop("later.txt")

	// The verification that discovers the change must not park the run.
	fx.poll(1)
	if got := fx.recoveries(); got != 1 {
		t.Fatalf("branch-advance recoveries = %d, want exactly 1", got)
	}
	if fx.runState() == domain.WorkflowRunNeedsAttention {
		t.Fatal("the run parked instead of re-reviewing the branch as it stands")
	}
	if got := fx.stepState(domain.WorkflowStepVerify); got.Terminal() {
		t.Fatalf("verify step = %q, want a non-terminal state a re-verification can run from", got)
	}
	// The failing verification is recorded honestly, on the class it failed on,
	// and marked as the question it is rather than the answer.
	results := fx.verifyResults()
	last := results[len(results)-1]
	if last.Passed || last.ErrorClass != domain.WorkflowErrorVerifyWorkspaceChanged || !last.SupersededByFreshReview {
		t.Fatalf("stale verification recorded as %+v, want a superseded verify_workspace_changed failure", last)
	}

	// One fresh, independent review — of the CURRENT head, not the approval.
	fx.poll(1)
	if fx.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches = %d, want exactly 1", fx.launcher.launchCalls)
	}
	fresh := fx.freshReview()
	if fresh.ID == fx.priorReviewID {
		t.Fatal("the stale approval was reused instead of a new review run")
	}
	if fresh.TargetSHA != advanced {
		t.Fatalf("the fresh review targets %q, want the CURRENT workspace %q", fresh.TargetSHA, advanced)
	}

	// A fresh verification of what that review approved, and only then done.
	fx.approveFreshReview()
	fx.poll(2)
	if got := fx.runState(); got != domain.WorkflowRunCompleted {
		t.Fatalf("run state = %q, want completed", got)
	}
	results = fx.verifyResults()
	final := results[len(results)-1]
	if !final.Passed {
		t.Fatalf("final verification = %+v, want a pass", final)
	}
	if final.ReviewedFingerprint != advanced {
		t.Fatalf("the passing verification verified %q, want the freshly approved %q", final.ReviewedFingerprint, advanced)
	}
}

// ---- 2. the verified commit is gone: NOT recoverable -----------------------

// An amend, a reset or a rebase leaves a head that does not contain the commit
// the reviewer read. The work AO certified is not on the branch any more, and
// no ancestry proof can be made — so it parks, exactly as it always did.
func TestRewrittenHistoryIsNotRecoverable(t *testing.T) {
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
		{"reset", func(fx *branchAdvancedFixture) {
			fx.commitOnTop("later.txt")
			fx.git("reset", "--hard", "HEAD~1")
			// And then a different commit takes that place, so the head moved
			// to something that never contained the approved commit.
			fx.git("checkout", "--orphan", "rewritten")
			fx.git("add", ".")
			fx.git("commit", "-m", "rewritten history")
			fx.git("branch", "-M", "main")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newBranchAdvancedFixture(t)
			tc.rewrite(fx)
			fx.observeHead()

			fx.poll(2)
			if got := fx.recoveries(); got != 0 {
				t.Fatalf("branch-advance recoveries = %d, want 0: a rewritten history is a person's decision", got)
			}
			if got := fx.runState(); got != domain.WorkflowRunNeedsAttention {
				t.Fatalf("run state = %q, want needs_attention", got)
			}
			if fx.launcher.launchCalls != 0 {
				t.Fatalf("reviewer launches = %d, want 0", fx.launcher.launchCalls)
			}
			results := fx.verifyResults()
			last := results[len(results)-1]
			if last.Passed || last.ErrorClass != domain.WorkflowErrorVerifyWorkspaceChanged || last.SupersededByFreshReview {
				t.Fatalf("verification recorded as %+v, want an unrecovered verify_workspace_changed failure", last)
			}
		})
	}
}

// ---- 3. git cannot answer: NOT recoverable ---------------------------------

// Failing to read the repository is never evidence that nothing was lost. A
// worktree git cannot answer for parks, with no recovery and no reviewer asked.
func TestUnreadableRepositoryIsNotRecoverable(t *testing.T) {
	fx := newBranchAdvancedFixture(t)
	// The branch really did advance; the repository just cannot be read any
	// more (here: it is not a repository at all).
	advanced := fx.commitOnTop("later.txt")
	notARepo := t.TempDir()
	if err := os.WriteFile(filepath.Join(notARepo, "seed.txt"), []byte("seed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	obs := fx.ws.obs
	obs.Path = notARepo
	fx.ws.obs = obs
	for i, cp := range fx.store.checkpoints[fx.runID] {
		if cp.ID == "work-cp" {
			cp.WorktreePath = notARepo
			fx.store.checkpoints[fx.runID][i] = cp
		}
	}
	if fp := workflowcore.WorkspaceFingerprint(fx.ws.obs); fp != advanced || fp == fx.approvedFingerprint {
		t.Fatal("the fixture must present the SAME advanced workspace, only in a place git cannot answer for")
	}

	fx.poll(2)
	if got := fx.recoveries(); got != 0 {
		t.Fatalf("branch-advance recoveries = %d, want 0: an unreadable repository proves nothing", got)
	}
	if got := fx.runState(); got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", got)
	}
	if fx.launcher.launchCalls != 0 {
		t.Fatalf("reviewer launches = %d, want 0", fx.launcher.launchCalls)
	}
}

// ---- 4. a restart in the middle of the recovery ----------------------------

// The decision is durable before anything moves, so a daemon that dies between
// recording it and dispatching the review resumes the SAME decision — it never
// re-decides it against a branch that may have moved again, and it never asks
// for a second review.
func TestRestartDuringBranchAdvancedRecoveryResumesOneReview(t *testing.T) {
	fx := newBranchAdvancedFixture(t)
	advanced := fx.commitOnTop("later.txt")

	// The pass that records the decision, and then the daemon dies.
	fx.poll(1)
	if got := fx.recoveries(); got != 1 {
		t.Fatalf("branch-advance recoveries = %d, want exactly 1", got)
	}
	if fx.launcher.launchCalls != 0 {
		t.Fatalf("reviewer launches before the restart = %d, want 0", fx.launcher.launchCalls)
	}

	// A new Coordinator over the same durable state IS the restart.
	fx.coord = fx.newCoordinator()
	fx.poll(3)

	if got := fx.recoveries(); got != 1 {
		t.Fatalf("branch-advance recoveries after the restart = %d, want still exactly 1", got)
	}
	if fx.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches after the restart = %d, want exactly 1", fx.launcher.launchCalls)
	}
	if got := fx.freshReview().TargetSHA; got != advanced {
		t.Fatalf("the resumed review targets %q, want %q", got, advanced)
	}

	// And it still finishes the way it would have without the restart.
	fx.approveFreshReview()
	fx.poll(2)
	if got := fx.runState(); got != domain.WorkflowRunCompleted {
		t.Fatalf("run state = %q, want completed", got)
	}
}

// ---- 5. fifty polls, one fresh review --------------------------------------

func TestFiftyPollsProduceExactlyOneFreshReview(t *testing.T) {
	fx := newBranchAdvancedFixture(t)
	fx.commitOnTop("later.txt")

	fx.poll(50)

	if got := fx.recoveries(); got != 1 {
		t.Fatalf("branch-advance recoveries after 50 polls = %d, want exactly 1", got)
	}
	if fx.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches after 50 polls = %d, want exactly 1", fx.launcher.launchCalls)
	}
	if got := len(fx.reviews.runs); got != 2 {
		t.Fatalf("review runs = %d, want 2 (the stale approval and one fresh review)", got)
	}
}

// ---- 6. the stale review is never reused -----------------------------------

// Nothing earlier is carried across the recovery: not the approval, not the
// verification it authorized. Every verification that runs after the advance
// runs against a fingerprint a reviewer actually read.
func TestStaleReviewIsNeverReusedAcrossABranchAdvance(t *testing.T) {
	fx := newBranchAdvancedFixture(t)
	advanced := fx.commitOnTop("later.txt")

	fx.poll(2)
	// Before the fresh verdict lands, no verification may run at all: the
	// review step is reopened, so there is nothing approved to verify.
	fx.poll(5)
	for _, res := range fx.verifyResults() {
		if res.Passed {
			t.Fatalf("a verification passed before the fresh review answered: %+v", res)
		}
	}

	fx.approveFreshReview()
	fx.poll(2)

	sawFreshPass := false
	for _, res := range fx.verifyResults() {
		if res.Passed && res.ReviewedFingerprint == fx.approvedFingerprint {
			t.Fatalf("a verification passed against the STALE approval %q", fx.approvedFingerprint)
		}
		if res.Passed && res.ReviewedFingerprint == advanced {
			sawFreshPass = true
		}
	}
	if !sawFreshPass {
		t.Fatal("no verification ever passed against the freshly approved workspace")
	}
	if fresh := fx.freshReview(); fresh.ID == fx.priorReviewID || fresh.TargetSHA == fx.approvedFingerprint {
		t.Fatalf("the fresh review is not independent of the stale approval: %+v", fresh)
	}
}

// ---- 7. a run already parked on the change ---------------------------------

// A workspace change decided and persisted before the proofs held — or by a
// daemon that had no such mechanism at all — is not a dead end. Its verify step
// is terminal, so only an explicit Continue may reopen it: a Board poll
// re-deriving the same state a hundred times is not a person, and must change
// nothing.
func TestParkedBranchAdvanceReopensOnlyOnContinue(t *testing.T) {
	fx := newBranchAdvancedFixture(t)
	fx.commitOnTop("later.txt")
	// Park it for real, through the ordinary path: while the agent may still be
	// writing, the change is not attributable and the run stops.
	sess, _, _ := fx.facts.GetSession(context.Background(), domain.SessionID(fx.sid))
	active := sess
	active.Activity = domain.Activity{State: domain.ActivityActive, LastActivityAt: fx.clk.Now()}
	fx.facts.put(active)
	fx.poll(2)
	if got := fx.runState(); got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want the parked state this test starts from", got)
	}
	if got := fx.stepState(domain.WorkflowStepVerify); got != domain.WorkflowStepFailed {
		t.Fatalf("verify step = %q, want failed", got)
	}

	// The agent finishes and goes quiet: every proof now holds. Polling still
	// changes nothing, because a poll is not an authorization.
	fx.facts.put(sess)
	fx.poll(10)
	if got := fx.recoveries(); got != 0 {
		t.Fatalf("branch-advance recoveries after 10 polls = %d, want 0: a terminal step is reopened only by Continue", got)
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
		t.Fatalf("branch-advance recoveries = %d, want exactly 1", got)
	}
	if got := fx.runState(); got == domain.WorkflowRunNeedsAttention {
		t.Fatal("the run is still parked after an authorized branch-advance recovery")
	}
	if got := fx.stepState(domain.WorkflowStepVerify); got.Terminal() {
		t.Fatalf("verify step = %q, want a non-terminal state", got)
	}

	// A second Continue is not a second recovery.
	fx.clk.Advance(time.Minute)
	if _, err := fx.coord.ContinueRun(context.Background(), fx.runID); err != nil {
		t.Fatalf("second ContinueRun: %v", err)
	}
	if got := fx.recoveries(); got != 1 {
		t.Fatalf("branch-advance recoveries after a second Continue = %d, want still 1", got)
	}

	fx.poll(2)
	if fx.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches = %d, want exactly 1", fx.launcher.launchCalls)
	}
	fx.approveFreshReview()
	fx.poll(2)
	if got := fx.runState(); got != domain.WorkflowRunCompleted {
		t.Fatalf("run state = %q, want completed", got)
	}
}

// ---- 8. outside its own shape, nothing changes -----------------------------

// A worker that could still be delivering is not a settled tree, and an
// unsettled tree is not recoverable, however clean the ancestry looks.
func TestBranchAdvanceRefusesWhileTheAgentMayStillBeWriting(t *testing.T) {
	fx := newBranchAdvancedFixture(t)
	fx.commitOnTop("later.txt")
	sess, _, _ := fx.facts.GetSession(context.Background(), domain.SessionID(fx.sid))
	sess.Activity = domain.Activity{State: domain.ActivityActive, LastActivityAt: fx.clk.Now()}
	fx.facts.put(sess)

	fx.poll(2)

	if got := fx.recoveries(); got != 0 {
		t.Fatalf("branch-advance recoveries = %d, want 0 while the agent may still be writing", got)
	}
	if got := fx.runState(); got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", got)
	}
}
