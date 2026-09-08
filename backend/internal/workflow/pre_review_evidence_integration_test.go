package workflow_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// pre_review_evidence_integration_test.go — P5-A phase 2, end to end.
//
// The property under test throughout is the one the phase exists for: AO may
// finish an ordinary low-risk Task without an independent reviewer WHEN IT RAN
// THE CHECKS ITSELF and watched them pass — and in no other circumstance. Every
// test below is either that sentence, or one of the ways it must fail closed.

// newCoordinatorWithReviewAndVerifier is newCoordinatorWithReview plus the
// verification runtime, which is what phase 2 needs: without a VerifyRunner
// there is no evidence to be had, and the coordinator says so rather than
// pretending the change was checked.
func newCoordinatorWithReviewAndVerifier(
	spawner workflowcore.Spawner, sessionFacts workflowcore.SessionFacts,
	workspaceFacts workflowcore.WorkspaceFacts, reviewRuns *fakeReviewRuns,
	launcher *fakeReviewerLauncher, runner workflowcore.VerifyRunner,
) (*workflowcore.Coordinator, *fakeStore, *fakeClock) {
	store := newFakeStore()
	store.reviewRuns = reviewRuns
	clk := &fakeClock{t: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store:            store,
		Spawner:          spawner,
		SessionFacts:     sessionFacts,
		WorkspaceFacts:   workspaceFacts,
		ReviewRuns:       reviewRuns,
		ReviewerLauncher: launcher,
		Verifier:         runner,
		Clock:            clk.Now,
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("id%d", idSeq)
		},
	})
	return c, store, clk
}

// passingRunner answers every command with a clean exit 0.
func passingRunner() *scriptedVerifyRunner {
	return &scriptedVerifyRunner{respond: func(workflowcore.VerifyCommandRequest) (workflowcore.VerifyCommandExecution, error) {
		return workflowcore.VerifyCommandExecution{ExitCode: 0, DurationMS: 12, StdoutTail: "ok"}, nil
	}}
}

// evidenceFor reads back the durable evidence record for a run.
func evidenceFor(t *testing.T, store *fakeStore, runID string) (domain.PreReviewEvidence, bool) {
	t.Helper()
	cps := checkpointsWithPhase(t, store, runID, "pre_review_evidence")
	if len(cps) == 0 {
		return domain.PreReviewEvidence{}, false
	}
	rec, ok := workflowcore.DecodePreReviewEvidenceForTest(cps[len(cps)-1].RetryState)
	return rec, ok
}

// ordinaryCodeChange is a single ordinary source file: standard risk, the tier
// phase 2 is about.
func ordinaryCodeChange() []ports.WorkspaceChange {
	return []ports.WorkspaceChange{{Path: "pkg/helper.go", Status: " M"}}
}

// completeWorkStepInDir is completeWorkStepWithChanges against a REAL directory.
//
// Phase 2 needs one: the evidence pass resolves each command's working
// directory through secureWorktreePath, which lstats it, so the shared
// fixture's synthetic "/ws/wf" makes every pass record `unavailable` before it
// can run anything. That is correct behaviour — AO must not run commands in a
// directory it cannot resolve — and it means these tests have to give it a
// worktree that exists.
func completeWorkStepInDir(
	t *testing.T, c *workflowcore.Coordinator, clk *fakeClock,
	sessionFacts *fakeSessionFacts, workspaceFacts *fakeWorkspaceFacts,
	runID, dir string, changes []ports.WorkspaceChange,
) workflowcore.RunDetail {
	t.Helper()
	ctx := context.Background()
	detail, err := c.StartRun(ctx, runID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	work := workStepFrom(detail)
	sessionFacts.put(domain.SessionRecord{
		ID: domain.SessionID(*work.Step.SessionID), ProjectID: "proj-1",
		Activity: domain.Activity{State: domain.ActivityIdle}, IsTerminated: false,
		Metadata: domain.SessionMetadata{WorkspacePath: dir, Branch: "ao/wf"},
	})
	workspaceFacts.obs = ports.WorkspaceObservation{Path: dir, Branch: "ao/wf", Dirty: true, Changes: changes}
	clk.Advance(10 * time.Second)
	got, err := c.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if workStepFrom(got).Step.State != domain.WorkflowStepCompleted {
		t.Fatalf("work step state = %q, want completed", workStepFrom(got).Step.State)
	}
	return got
}

// THE HEADLINE. A fast Task, an ordinary code change, a plan AO can run, and
// checks AO watched pass: no independent reviewer, and the ledger says exactly
// why nobody reviewed it.
func TestLowRiskTaskWithObservedEvidenceCompletesWithoutAReviewer(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	dir := t.TempDir()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: dir}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	runner := passingRunner()
	c, store, clk := newCoordinatorWithReviewAndVerifier(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher, runner)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "rename a helper", verifiablePlan())
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStepInDir(t, c, clk, sessionFacts, workspaceFacts, created.Run.ID, dir, ordinaryCodeChange())
	got, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}

	// No reviewer. Not a fabricated verdict — no review_run exists at all.
	review := reviewStepFrom(got)
	if review.Step.ReviewRunID != nil {
		t.Fatalf("a reviewer ran despite sufficient observed evidence (review run %q)", *review.Step.ReviewRunID)
	}
	if launcher.launchCalls != 0 {
		t.Fatalf("the reviewer was launched %d times", launcher.launchCalls)
	}
	if review.Step.State != domain.WorkflowStepCompleted {
		t.Fatalf("review step state = %q, want completed", review.Step.State)
	}

	// AO really did run the checks.
	if len(runner.calls) != 1 {
		t.Fatalf("AO executed %d commands as evidence, want 1", len(runner.calls))
	}
	rec, ok := evidenceFor(t, store, created.Run.ID)
	if !ok {
		t.Fatal("no durable pre-review evidence was recorded")
	}
	if rec.Status != domain.PreReviewEvidenceObserved {
		t.Fatalf("evidence status = %q, want observed", rec.Status)
	}
	if rec.ExecutedCommandCount != 1 || !rec.AllPassed() {
		t.Fatalf("evidence did not record a clean executed pass: %+v", rec)
	}

	// The decision is durable, and it names the evidence it stood on.
	skips := checkpointsWithPhase(t, store, created.Run.ID, "review_skipped_by_evidence")
	if len(skips) != 1 {
		t.Fatalf("recorded %d evidence skips, want exactly 1", len(skips))
	}
	if !strings.Contains(skips[0].RetryState, rec.TargetKey) {
		t.Fatalf("the skip does not name the evidence it stood on: %s", skips[0].RetryState)
	}
	// And it is NOT indexed as a ReviewPolicy skip: the two must stay tellable
	// apart forever.
	if n := len(checkpointsWithPhase(t, store, created.Run.ID, "review_policy_skipped")); n != 0 {
		t.Fatalf("the evidence skip was also recorded as a policy skip (%d rows)", n)
	}

	depths := depthDecisionsFor(t, store, created.Run.ID)
	if len(depths) != 1 {
		t.Fatalf("recorded %d depth decisions, want 1", len(depths))
	}
	if depths[0].Effective != domain.ReviewDepthNone {
		t.Fatalf("effective depth = %q, want none", depths[0].Effective)
	}
	if depths[0].Reason != domain.ReviewDepthReasonEvidenceRelieved {
		t.Fatalf("reason = %q, want the decision to name the evidence", depths[0].Reason)
	}
}

// The same change with NO verification runtime keeps phase 1's behaviour
// exactly: a bounded review, because AO could observe nothing.
func TestOrdinaryTaskWithoutEvidenceStillGetsABoundedReview(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	dir := t.TempDir()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: dir}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	// No verifier at all.
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "rename a helper", verifiablePlan())
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStepInDir(t, c, clk, sessionFacts, workspaceFacts, created.Run.ID, dir, ordinaryCodeChange())
	got, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if reviewStepFrom(got).Step.ReviewRunID == nil {
		t.Fatal("no reviewer ran for a change AO could not check itself")
	}
	rec, ok := evidenceFor(t, store, created.Run.ID)
	if !ok {
		t.Fatal("the absence of a runtime must still be recorded, not left blank")
	}
	if rec.Status != domain.PreReviewEvidenceUnavailable {
		t.Fatalf("evidence status = %q, want unavailable", rec.Status)
	}
	depths := depthDecisionsFor(t, store, created.Run.ID)
	if depths[0].Effective != domain.ReviewDepthLight {
		t.Fatalf("effective depth = %q, want light", depths[0].Effective)
	}
}

// A FAILING check is a measurement, not a licence. It must never skip the
// reviewer, and the failure must be on the record.
func TestFailingEvidenceNeverSkipsTheReviewer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runner *scriptedVerifyRunner
		want   domain.PreReviewEvidenceStatus
	}{
		{"a check failed", &scriptedVerifyRunner{respond: func(workflowcore.VerifyCommandRequest) (workflowcore.VerifyCommandExecution, error) {
			return workflowcore.VerifyCommandExecution{ExitCode: 1, StderrTail: "FAIL"}, nil
		}}, domain.PreReviewEvidenceFailed},
		{"a check timed out", &scriptedVerifyRunner{respond: func(workflowcore.VerifyCommandRequest) (workflowcore.VerifyCommandExecution, error) {
			return workflowcore.VerifyCommandExecution{TimedOut: true}, nil
		}}, domain.PreReviewEvidenceTimedOut},
		{"the runtime could not run it", &scriptedVerifyRunner{respond: func(workflowcore.VerifyCommandRequest) (workflowcore.VerifyCommandExecution, error) {
			return workflowcore.VerifyCommandExecution{}, fmt.Errorf("exec: \"go\": executable file not found in $PATH")
		}}, domain.PreReviewEvidenceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessionFacts := newFakeSessionFacts()
			dir := t.TempDir()
			spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: dir}}, facts: sessionFacts}
			workspaceFacts := &fakeWorkspaceFacts{}
			reviewRuns := newFakeReviewRuns()
			launcher := &fakeReviewerLauncher{}
			c, store, clk := newCoordinatorWithReviewAndVerifier(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher, tc.runner)
			ctx := context.Background()

			created, err := c.CreateRun(ctx, "proj-1", "rename a helper", verifiablePlan())
			if err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			completeWorkStepInDir(t, c, clk, sessionFacts, workspaceFacts, created.Run.ID, dir, ordinaryCodeChange())
			got, err := c.ContinueRun(ctx, created.Run.ID)
			if err != nil {
				t.Fatalf("ContinueRun: %v", err)
			}
			if reviewStepFrom(got).Step.ReviewRunID == nil {
				t.Fatal("the reviewer was skipped on evidence that did not pass")
			}
			if n := len(checkpointsWithPhase(t, store, created.Run.ID, "review_skipped_by_evidence")); n != 0 {
				t.Fatalf("%d evidence skips recorded for a failing pass", n)
			}
			rec, _ := evidenceFor(t, store, created.Run.ID)
			if rec.Status != tc.want {
				t.Fatalf("evidence status = %q, want %q", rec.Status, tc.want)
			}
			// The reviewer must be TOLD the checks did not pass.
			if !strings.Contains(launcher.lastPrompt, "Checks AO RAN ITSELF") {
				t.Error("the dispatched prompt does not carry AO's own checks")
			}
		})
	}
}

// A change touching an authentication path is high risk. Evidence — however
// green — must not move it, and the request must not either.
func TestHighRiskStillGetsADeepReviewWithPerfectEvidence(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	dir := t.TempDir()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: dir}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	runner := passingRunner()
	c, store, clk := newCoordinatorWithReviewAndVerifier(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher, runner)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "tidy the session helper", verifiablePlan())
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// Explicitly ask for no reviewer at all, the worst case for this test.
	if err := c.ApplyReviewDepthPolicy(ctx, created.Run.ID, domain.ReviewDepthNone); err != nil {
		t.Fatalf("ApplyReviewDepthPolicy: %v", err)
	}
	completeWorkStepInDir(t, c, clk, sessionFacts, workspaceFacts, created.Run.ID, dir,
		[]ports.WorkspaceChange{{Path: "internal/auth/session.go", Status: " M"}})
	got, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if reviewStepFrom(got).Step.ReviewRunID == nil {
		t.Fatal("an auth-path change skipped its reviewer")
	}
	depths := depthDecisionsFor(t, store, created.Run.ID)
	if depths[0].RiskTier != domain.ReviewRiskHigh {
		t.Fatalf("risk tier = %q, want high", depths[0].RiskTier)
	}
	if depths[0].Effective != domain.ReviewDepthDeep {
		t.Fatalf("effective depth = %q, want deep", depths[0].Effective)
	}
	// A deep review gets the unchanged full prompt, not the bounded one.
	if strings.Contains(launcher.lastPrompt, "This is a BOUNDED review") {
		t.Error("a high-risk change was given the bounded prompt")
	}
	relief, ok := workflowcore.DecodeReviewEvidenceReliefForTest(
		checkpointsWithPhase(t, store, created.Run.ID, "review_evidence_relief")[0].RetryState)
	if !ok || relief.Granted {
		t.Fatalf("relief was granted on a high-risk change: %+v", relief)
	}
	if relief.Reason != domain.ReliefDeniedTier {
		t.Fatalf("relief reason = %q, want the tier refusal", relief.Reason)
	}
}

// A master run keeps a full independent review whatever the evidence says.
func TestMasterKeepsADeepReviewUnderEvidence(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	dir := t.TempDir()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: dir}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReviewAndVerifier(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher, passingRunner())
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "rename a helper", verifiablePlan())
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// A master run's frozen request is deep. max(deep, anything) is deep.
	if err := c.ApplyReviewDepthPolicy(ctx, created.Run.ID, domain.ReviewDepthDeep); err != nil {
		t.Fatalf("ApplyReviewDepthPolicy: %v", err)
	}
	completeWorkStepInDir(t, c, clk, sessionFacts, workspaceFacts, created.Run.ID, dir, ordinaryCodeChange())
	got, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if reviewStepFrom(got).Step.ReviewRunID == nil {
		t.Fatal("a deep-requesting run skipped its reviewer on evidence")
	}
	if d := depthDecisionsFor(t, store, created.Run.ID)[0]; d.Effective != domain.ReviewDepthDeep {
		t.Fatalf("effective depth = %q, want deep", d.Effective)
	}
}

// A worker's glowing report is not evidence. With no runtime to observe
// anything, a perfect report must buy exactly nothing.
func TestAWorkerReportIsNotEvidence(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	dir := t.TempDir()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: dir}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, _, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "rename a helper", verifiablePlan())
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStepInDir(t, c, clk, sessionFacts, workspaceFacts, created.Run.ID, dir, ordinaryCodeChange())

	if _, err := c.SubmitWorkReport(ctx, created.Run.ID, domain.WorkReport{
		Summary: "Renamed the helper. I ran the full suite and everything passes.",
		TestsReported: []domain.WorkReportTestClaim{
			{Command: "go test ./pkg/...", ClaimedOutcome: domain.WorkReportOutcomeClaimedPassed},
		},
		Criteria: []domain.WorkReportCriterion{{Criterion: "Objective is addressed", Addressed: true}},
	}); err != nil {
		t.Fatalf("SubmitWorkReport: %v", err)
	}

	got, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if reviewStepFrom(got).Step.ReviewRunID == nil {
		t.Fatal("a worker's claim that the tests passed skipped the reviewer")
	}
	// The reviewer gets the report — labelled as a claim, next to the rule.
	for _, want := range []string{
		"What the WORKER SAYS it did",
		"NOT evidence",
		"CLAIMS (unverified) it passed",
	} {
		if !strings.Contains(launcher.lastPrompt, want) {
			t.Errorf("the prompt is missing %q", want)
		}
	}
}

// A worker that claims a pass AO watched FAIL is recorded as contradicting the
// evidence, and the contradiction independently blocks any skip.
func TestAContradictedReportIsRecordedAndBlocksTheSkip(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	dir := t.TempDir()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: dir}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	failing := &scriptedVerifyRunner{respond: func(workflowcore.VerifyCommandRequest) (workflowcore.VerifyCommandExecution, error) {
		return workflowcore.VerifyCommandExecution{ExitCode: 1, StderrTail: "FAIL pkg"}, nil
	}}
	c, store, clk := newCoordinatorWithReviewAndVerifier(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher, failing)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "rename a helper", verifiablePlan())
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStepInDir(t, c, clk, sessionFacts, workspaceFacts, created.Run.ID, dir, ordinaryCodeChange())
	if _, err := c.SubmitWorkReport(ctx, created.Run.ID, domain.WorkReport{
		TestsReported: []domain.WorkReportTestClaim{
			{Command: "go test ./pkg/...", ClaimedOutcome: domain.WorkReportOutcomeClaimedPassed},
		},
	}); err != nil {
		t.Fatalf("SubmitWorkReport: %v", err)
	}
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	rec, ok := evidenceFor(t, store, created.Run.ID)
	if !ok {
		t.Fatal("no evidence recorded")
	}
	if !rec.ReportContradicted {
		t.Fatal("the worker claimed a pass AO watched fail, and the contradiction was not recorded")
	}
	if n := len(checkpointsWithPhase(t, store, created.Run.ID, "review_skipped_by_evidence")); n != 0 {
		t.Fatalf("%d evidence skips recorded despite a contradiction", n)
	}
	if !strings.Contains(launcher.lastPrompt, "WARNING: the worker's report claims a command passed that AO watched FAIL") {
		t.Error("the reviewer was not told about the contradiction")
	}
}

// The evidence pass is decided ONCE per target. Repeated observation and a
// coordinator rebuilt over the same store must both re-read the recorded
// answer rather than run the suite again.
func TestEvidenceIsExecutedOnceAndSurvivesRestart(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	dir := t.TempDir()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: dir}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	runner := passingRunner()
	c, store, clk := newCoordinatorWithReviewAndVerifier(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher, runner)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "rename a helper", verifiablePlan())
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStepInDir(t, c, clk, sessionFacts, workspaceFacts, created.Run.ID, dir, ordinaryCodeChange())
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	afterFirst := len(runner.calls)

	for i := 0; i < 5; i++ {
		clk.Advance(30 * time.Second)
		if _, err := c.GetRun(ctx, created.Run.ID); err != nil {
			t.Fatalf("GetRun %d: %v", i, err)
		}
	}
	if len(runner.calls) != afterFirst {
		t.Fatalf("repeated observation re-ran the suite: %d -> %d executions", afterFirst, len(runner.calls))
	}
	if n := len(checkpointsWithPhase(t, store, created.Run.ID, "pre_review_evidence")); n != 1 {
		t.Fatalf("recorded %d evidence rows for one target, want 1", n)
	}
	if n := len(checkpointsWithPhase(t, store, created.Run.ID, "review_skipped_by_evidence")); n != 1 {
		t.Fatalf("recorded %d evidence skips, want exactly 1", n)
	}

	// A restart: a NEW coordinator over the SAME store.
	restarted := workflowcore.New(workflowcore.Deps{
		Store: store, Spawner: spawner, SessionFacts: sessionFacts, WorkspaceFacts: workspaceFacts,
		ReviewRuns: reviewRuns, ReviewerLauncher: launcher, Verifier: runner, Clock: clk.Now,
		NewID: func() string { return "restarted-id" },
	})
	clk.Advance(time.Minute)
	if _, err := restarted.GetRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("GetRun after restart: %v", err)
	}
	if len(runner.calls) != afterFirst {
		t.Fatalf("a restart re-ran the suite: %d -> %d executions", afterFirst, len(runner.calls))
	}
	if n := len(checkpointsWithPhase(t, store, created.Run.ID, "review_skipped_by_evidence")); n != 1 {
		t.Fatalf("a restart recorded a second skip decision (%d rows)", n)
	}
}

// A cancelled run must not leave a record behind: a measurement nobody finished
// taking is not a measurement.
func TestCancellingDuringEvidenceRecordsNothing(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	dir := t.TempDir()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: dir}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}

	ctx, cancel := context.WithCancel(context.Background())
	runner := &scriptedVerifyRunner{respond: func(workflowcore.VerifyCommandRequest) (workflowcore.VerifyCommandExecution, error) {
		cancel()
		return workflowcore.VerifyCommandExecution{}, context.Canceled
	}}
	c, store, clk := newCoordinatorWithReviewAndVerifier(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher, runner)

	created, err := c.CreateRun(context.Background(), "proj-1", "rename a helper", verifiablePlan())
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStepInDir(t, c, clk, sessionFacts, workspaceFacts, created.Run.ID, dir, ordinaryCodeChange())
	// The cancellation surfaces as an error; what matters is what it did NOT
	// write.
	_, _ = c.ContinueRun(ctx, created.Run.ID)

	if n := len(checkpointsWithPhase(t, store, created.Run.ID, "pre_review_evidence")); n != 0 {
		t.Fatalf("a cancelled evidence pass recorded %d rows; it must record none", n)
	}
	if n := len(checkpointsWithPhase(t, store, created.Run.ID, "review_skipped_by_evidence")); n != 0 {
		t.Fatalf("a cancelled run skipped a reviewer (%d rows)", n)
	}
}

// A task with nothing executable to run is not a task AO has checked. It gets
// a reviewer, and the record says `not_planned` rather than pretending a pass.
func TestATaskWithNoExecutableChecksIsNotTreatedAsChecked(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	dir := t.TempDir()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: dir}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	runner := passingRunner()
	c, store, clk := newCoordinatorWithReviewAndVerifier(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher, runner)
	ctx := context.Background()

	// A plan whose only check is a file assertion: nothing to execute.
	plan := workflowcore.VerificationPlan{
		Files: []workflowcore.VerificationFileCheck{{Path: "pkg/helper.go", Exists: true}},
	}
	created, err := c.CreateRun(ctx, "proj-1", "rename a helper", plan)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStepInDir(t, c, clk, sessionFacts, workspaceFacts, created.Run.ID, dir, ordinaryCodeChange())
	got, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("AO executed %d commands for a plan with none", len(runner.calls))
	}
	if reviewStepFrom(got).Step.ReviewRunID == nil {
		t.Fatal("a task AO cannot check itself skipped its reviewer")
	}
	rec, _ := evidenceFor(t, store, created.Run.ID)
	if rec.Status != domain.PreReviewEvidenceNotPlanned {
		t.Fatalf("evidence status = %q, want not_planned", rec.Status)
	}
}

// verifyResultsFor decodes every verify_result this run recorded, so a test can
// state what verification actually did rather than infer it.
func verifyResultsFor(t *testing.T, store *fakeStore, runID string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, cp := range checkpointsWithPhase(t, store, runID, "verify_result") {
		var m map[string]any
		if err := json.Unmarshal([]byte(cp.RetryState), &m); err != nil {
			t.Fatalf("verify_result is not decodable: %v", err)
		}
		out = append(out, m)
	}
	return out
}

func reusedChecks(m map[string]any) int {
	if v, ok := m["reusedCheckCount"].(float64); ok {
		return int(v)
	}
	return 0
}

// REUSE, end to end. The evidence pass ran the plan at tree A; verification of
// tree A runs the same plan; the second execution does not happen, and the
// verification record says so instead of quietly having fewer checks.
func TestVerifyReusesPreReviewEvidenceInsteadOfRunningItTwice(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	dir := t.TempDir()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: dir}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	runner := passingRunner()
	c, store, clk := newCoordinatorWithReviewAndVerifier(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher, runner)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "rename a helper", verifiablePlan())
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStepInDir(t, c, clk, sessionFacts, workspaceFacts, created.Run.ID, dir, ordinaryCodeChange())
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	// The review was skipped on evidence, so verification is next. Drive it.
	clk.Advance(time.Minute)
	got, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	// THE SAVING: the plan's one command ran exactly ONCE across the whole run,
	// even though both the evidence pass and verification asked for it.
	if len(runner.calls) != 1 {
		t.Fatalf("the plan's command executed %d times across evidence+verify, want 1", len(runner.calls))
	}
	results := verifyResultsFor(t, store, created.Run.ID)
	if len(results) == 0 {
		t.Fatal("verification never recorded a result")
	}
	last := results[len(results)-1]
	if n := reusedChecks(last); n != 1 {
		t.Fatalf("verification reused %d checks, want 1: %v", n, last)
	}
	// A reused check is still a check: the record must not have lost it.
	checks, _ := last["checks"].([]any)
	if len(checks) == 0 {
		t.Fatalf("the verification record kept no checks at all: %v", last)
	}
	// And the run really did finish, on a real verification.
	if verifyStepFrom(got).Step.State != domain.WorkflowStepCompleted {
		t.Fatalf("verify step state = %q, want completed", verifyStepFrom(got).Step.State)
	}
}

// shiftingWorkspaceFacts returns tree A until the run has durably recorded its
// pre-review evidence, and tree B from then on.
//
// The switch is keyed on the EVIDENCE EXISTING rather than on a call count,
// because the number of observations between work completion and verification
// is an implementation detail and a test that encodes it would pass for the
// wrong reason the moment that detail changed. What this fixture means is
// exactly "the worktree moved after AO measured it", and that is what it says.
type shiftingWorkspaceFacts struct {
	store  *fakeStore
	runID  string
	before ports.WorkspaceObservation
	after  ports.WorkspaceObservation
}

func (f *shiftingWorkspaceFacts) ObserveWorkspace(ctx context.Context, _ ports.WorkspaceInfo) (ports.WorkspaceObservation, error) {
	cps, err := f.store.ListWorkflowCheckpoints(ctx, f.runID)
	if err == nil {
		for _, cp := range cps {
			if cp.DurablePhase == "pre_review_evidence" {
				return f.after, nil
			}
		}
	}
	return f.before, nil
}

// A tree that moved after the evidence was taken must not have that evidence
// reused for it. The approval-authority guard fires first, so verification does
// not execute at all — and critically it does not reuse either: nothing in this
// run may certify tree B with measurements taken on tree A.
func TestEvidenceIsNeverReusedForATreeItWasNotTakenOn(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	dir := t.TempDir()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: dir}}, facts: sessionFacts}
	seed := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	runner := passingRunner()
	c, store, clk := newCoordinatorWithReviewAndVerifier(spawner, sessionFacts, seed, reviewRuns, launcher, runner)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "rename a helper", verifiablePlan())
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStepInDir(t, c, clk, sessionFacts, seed, created.Run.ID, dir, ordinaryCodeChange())

	// From here on the worktree moves the instant AO has finished measuring it.
	shifting := &shiftingWorkspaceFacts{
		store: store, runID: created.Run.ID,
		before: seed.obs,
		after: ports.WorkspaceObservation{
			Path: dir, Branch: "ao/wf", Dirty: true,
			Changes: []ports.WorkspaceChange{
				{Path: "pkg/helper.go", Status: " M"},
				{Path: "pkg/unreviewed.go", Status: "??"},
			},
		},
	}
	moved := workflowcore.New(workflowcore.Deps{
		Store: store, Spawner: spawner, SessionFacts: sessionFacts, WorkspaceFacts: shifting,
		ReviewRuns: reviewRuns, ReviewerLauncher: launcher, Verifier: runner, Clock: clk.Now,
		NewID: func() string { return "moved-id" },
	})
	if _, err := moved.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	clk.Advance(time.Minute)
	got, err := moved.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	evidence, ok := evidenceFor(t, store, created.Run.ID)
	if !ok {
		t.Fatal("no evidence recorded")
	}
	// No reuse onto the moved tree, in any verification this run recorded.
	for _, r := range verifyResultsFor(t, store, created.Run.ID) {
		if n := reusedChecks(r); n != 0 {
			t.Fatalf("verification reused %d checks against a tree the evidence (%s) was not taken on",
				n, evidence.Fingerprint)
		}
	}
	if verifyStepFrom(got).Step.State == domain.WorkflowStepCompleted {
		t.Fatal("the run certified a tree whose evidence belonged to a different one")
	}
}

// The reuse key is fingerprint-sensitive by construction: the same plan on two
// different trees produces two different keys, which is what makes the check
// above structural rather than incidental.
func TestEvidenceTargetKeyMovesWithTheTree(t *testing.T) {
	keys := map[string]bool{}
	for _, changes := range [][]ports.WorkspaceChange{
		{{Path: "pkg/helper.go", Status: " M"}},
		{{Path: "pkg/helper.go", Status: " M"}, {Path: "pkg/other.go", Status: " M"}},
	} {
		sessionFacts := newFakeSessionFacts()
		dir := t.TempDir()
		spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: dir}}, facts: sessionFacts}
		workspaceFacts := &fakeWorkspaceFacts{}
		reviewRuns := newFakeReviewRuns()
		c, store, clk := newCoordinatorWithReviewAndVerifier(spawner, sessionFacts, workspaceFacts, reviewRuns, &fakeReviewerLauncher{}, passingRunner())
		ctx := context.Background()
		created, err := c.CreateRun(ctx, "proj-1", "rename a helper", verifiablePlan())
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		completeWorkStepInDir(t, c, clk, sessionFacts, workspaceFacts, created.Run.ID, dir, changes)
		if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
			t.Fatalf("ContinueRun: %v", err)
		}
		rec, ok := evidenceFor(t, store, created.Run.ID)
		if !ok || rec.TargetKey == "" {
			t.Fatalf("no target key recorded: %+v", rec)
		}
		keys[rec.TargetKey] = true
	}
	if len(keys) != 2 {
		t.Fatalf("two different trees produced %d distinct reuse keys; the key is not tree-sensitive", len(keys))
	}
}
