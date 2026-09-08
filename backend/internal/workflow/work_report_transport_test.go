package workflow_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// work_report_transport_test.go — P5-A phase 2B: the session-addressed entry
// point a worker actually calls.
//
// The interesting cases are all refusals, and each one exists so that the
// ledger cannot end up holding a declaration in a place that implies something
// untrue about it.

// reportFor is a small, valid declaration.
func reportFor(summary string) domain.WorkReport {
	return domain.WorkReport{
		Summary:       summary,
		TestsReported: []domain.WorkReportTestClaim{{Command: "go test ./pkg/...", ClaimedOutcome: domain.WorkReportOutcomeClaimedPassed}},
	}
}

// workSessionOf returns the session AO dispatched a run's work step into.
func workSessionOf(t *testing.T, detail workflowcore.RunDetail) string {
	t.Helper()
	step := workStepFrom(detail).Step
	if step.SessionID == nil || *step.SessionID == "" {
		t.Fatal("the work step has no session")
	}
	return *step.SessionID
}

// A worker names its own session; AO resolves the run from durable state and
// records the declaration against it.
func TestAWorkerReportsAgainstItsOwnSession(t *testing.T) {
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
	detail := completeWorkStepInDir(t, c, clk, sessionFacts, workspaceFacts, created.Run.ID, dir, ordinaryCodeChange())
	session := workSessionOf(t, detail)

	receipt, err := c.SubmitWorkReportForSession(ctx, session, reportFor("renamed the helper"))
	if err != nil {
		t.Fatalf("SubmitWorkReportForSession: %v", err)
	}
	if receipt.WorkflowRunID != created.Run.ID {
		t.Fatalf("report landed on run %q, want %q", receipt.WorkflowRunID, created.Run.ID)
	}
	if receipt.Superseded {
		t.Error("the first report claims to have replaced one")
	}
	if receipt.Report.Version != domain.WorkReportVersion {
		t.Errorf("stored version = %q", receipt.Report.Version)
	}
	// AO stamps the fingerprint itself; the worker never supplies it.
	if receipt.Report.Reference.FingerprintAtSubmission == "" {
		t.Error("AO did not stamp its own fingerprint onto the report")
	}
	if n := len(checkpointsWithPhase(t, store, created.Run.ID, "work_report")); n != 1 {
		t.Fatalf("recorded %d work_report rows, want 1", n)
	}
}

// A second report replaces the first as the one policy reads, and BOTH stay on
// the ledger: the write is append-only, so a worker that changes its story
// leaves a history rather than an edit.
func TestASecondReportSupersedesWithoutErasing(t *testing.T) {
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
	detail := completeWorkStepInDir(t, c, clk, sessionFacts, workspaceFacts, created.Run.ID, dir, ordinaryCodeChange())
	session := workSessionOf(t, detail)

	if _, err := c.SubmitWorkReportForSession(ctx, session, reportFor("first attempt")); err != nil {
		t.Fatalf("first report: %v", err)
	}
	clk.Advance(time.Second)
	second := reportFor("second attempt")
	second.Limitations = []string{"did not cover the windows path"}
	receipt, err := c.SubmitWorkReportForSession(ctx, session, second)
	if err != nil {
		t.Fatalf("second report: %v", err)
	}
	if !receipt.Superseded {
		t.Error("the second report does not say it replaced the first")
	}
	if n := len(checkpointsWithPhase(t, store, created.Run.ID, "work_report")); n != 2 {
		t.Fatalf("recorded %d work_report rows, want both kept", n)
	}

	// And the NEWEST is the one the policy acts on: this one admits a
	// limitation, so the reviewer must run.
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	got, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if reviewStepFrom(got).Step.ReviewRunID == nil {
		t.Fatal("the newest report admitted a limitation and the reviewer was still skipped")
	}
	relief, ok := workflowcore.DecodeReviewEvidenceReliefForTest(
		checkpointsWithPhase(t, store, created.Run.ID, "review_evidence_relief")[0].RetryState)
	if !ok || relief.Reason != domain.ReliefDeniedWorkerAdmission {
		t.Fatalf("relief reason = %q, want the worker admission to be why", relief.Reason)
	}
}

// A report that arrives after AO has already decided how deeply to review is
// refused. Accepting it would leave an undated claim sitting next to a decision
// it could not have informed.
func TestAReportAfterTheDepthDecisionIsRefused(t *testing.T) {
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
	// Ask for a bounded review, so the run is still live after the depth
	// decision — a run that skipped its reviewer and finished would be refused
	// for the different and less interesting reason that it is terminal.
	if err := c.ApplyReviewDepthPolicy(ctx, created.Run.ID, domain.ReviewDepthLight); err != nil {
		t.Fatalf("ApplyReviewDepthPolicy: %v", err)
	}
	detail := completeWorkStepInDir(t, c, clk, sessionFacts, workspaceFacts, created.Run.ID, dir, ordinaryCodeChange())
	session := workSessionOf(t, detail)

	// The depth decision happens here.
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if n := len(checkpointsWithPhase(t, store, created.Run.ID, "review_depth_decision")); n != 1 {
		t.Fatalf("fixture did not reach the depth decision (%d rows)", n)
	}

	_, err = c.SubmitWorkReportForSession(ctx, session, reportFor("too late"))
	if !errors.Is(err, workflowcore.ErrWorkReportWindowClosed) {
		t.Fatalf("err = %v, want ErrWorkReportWindowClosed", err)
	}
	if n := len(checkpointsWithPhase(t, store, created.Run.ID, "work_report")); n != 0 {
		t.Fatalf("a refused report was still written (%d rows)", n)
	}
}

// A session that is not running any work step resolves to nothing. That covers
// a made-up session, a pane from a superseded launch generation whose step now
// carries a different session, and a run that has gone terminal.
func TestAReportFromAStaleOrUnknownSessionIsRefused(t *testing.T) {
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
	detail := completeWorkStepInDir(t, c, clk, sessionFacts, workspaceFacts, created.Run.ID, dir, ordinaryCodeChange())
	session := workSessionOf(t, detail)

	t.Run("a session running nothing", func(t *testing.T) {
		_, err := c.SubmitWorkReportForSession(ctx, "sess-from-another-life", reportFor("hello"))
		if !errors.Is(err, workflowcore.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("a blank session", func(t *testing.T) {
		if _, err := c.SubmitWorkReportForSession(ctx, "   ", reportFor("hello")); !errors.Is(err, workflowcore.ErrInvalid) {
			t.Fatalf("err = %v, want ErrInvalid", err)
		}
	})

	t.Run("a cancelled run", func(t *testing.T) {
		if _, err := c.CancelRun(ctx, created.Run.ID); err != nil {
			t.Fatalf("CancelRun: %v", err)
		}
		_, err := c.SubmitWorkReportForSession(ctx, session, reportFor("after the cancel"))
		if !errors.Is(err, workflowcore.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound: a terminal run has nothing left to inform", err)
		}
		if n := len(checkpointsWithPhase(t, store, created.Run.ID, "work_report")); n != 0 {
			t.Fatalf("a report landed on a cancelled run (%d rows)", n)
		}
	})
}

// A report survives a restart and is read by the coordinator that comes after
// it — the report the policy reads is the one on disk, not one held in memory.
func TestAReportSurvivesARestartAndReachesTheReviewer(t *testing.T) {
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
	// Ask for a bounded review, so a reviewer actually launches and can be
	// inspected for what it was handed.
	if err := c.ApplyReviewDepthPolicy(ctx, created.Run.ID, domain.ReviewDepthLight); err != nil {
		t.Fatalf("ApplyReviewDepthPolicy: %v", err)
	}
	detail := completeWorkStepInDir(t, c, clk, sessionFacts, workspaceFacts, created.Run.ID, dir, ordinaryCodeChange())
	session := workSessionOf(t, detail)

	if _, err := c.SubmitWorkReportForSession(ctx, session, reportFor("renamed the helper and its call sites")); err != nil {
		t.Fatalf("SubmitWorkReportForSession: %v", err)
	}

	// The daemon restarts: a NEW coordinator over the SAME store.
	restarted := workflowcore.New(workflowcore.Deps{
		Store: store, Spawner: spawner, SessionFacts: sessionFacts, WorkspaceFacts: workspaceFacts,
		ReviewRuns: reviewRuns, ReviewerLauncher: launcher, Verifier: runner, Clock: clk.Now,
		NewID: func() string { return "restarted-id" },
	})
	clk.Advance(time.Minute)
	got, err := restarted.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun after restart: %v", err)
	}
	if reviewStepFrom(got).Step.ReviewRunID == nil {
		t.Fatal("no reviewer launched")
	}
	if !strings.Contains(launcher.lastPrompt, "renamed the helper and its call sites") {
		t.Error("the reviewer did not receive the report recorded before the restart")
	}
	if !strings.Contains(launcher.lastPrompt, "What the WORKER SAYS it did") {
		t.Error("the report was not labelled as a claim in the prompt")
	}
}

// The whole phase, in one run: the worker declares, AO checks the change
// itself, the policy decides on what AO observed, nobody reviews it, and Verify
// still certifies it — reusing the evidence rather than running it twice.
func TestReportToDeliveryEndToEnd(t *testing.T) {
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
	detail := completeWorkStepInDir(t, c, clk, sessionFacts, workspaceFacts, created.Run.ID, dir, ordinaryCodeChange())
	session := workSessionOf(t, detail)

	// 1. The worker declares, honestly and with nothing to confess.
	if _, err := c.SubmitWorkReportForSession(ctx, session, domain.WorkReport{
		Summary:  "Renamed helper and updated both call sites.",
		Criteria: []domain.WorkReportCriterion{{Criterion: "Objective is addressed", Addressed: true}},
		TestsReported: []domain.WorkReportTestClaim{
			{Command: "go test ./pkg/...", ClaimedOutcome: domain.WorkReportOutcomeClaimedPassed},
		},
	}); err != nil {
		t.Fatalf("SubmitWorkReportForSession: %v", err)
	}

	// 2-4. AO runs the checks itself, decides, and skips the reviewer.
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	clk.Advance(time.Minute)
	got, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	if launcher.launchCalls != 0 {
		t.Fatalf("a reviewer launched %d times", launcher.launchCalls)
	}
	evidence, ok := evidenceFor(t, store, created.Run.ID)
	if !ok || evidence.Status != domain.PreReviewEvidenceObserved {
		t.Fatalf("evidence = %+v, want observed", evidence)
	}
	if !evidence.ReportRecorded {
		t.Error("the evidence record does not note that a report existed")
	}
	if evidence.ReportContradicted {
		t.Error("an honest report was recorded as contradicting AO")
	}
	if n := len(checkpointsWithPhase(t, store, created.Run.ID, "review_skipped_by_evidence")); n != 1 {
		t.Fatalf("recorded %d evidence skips, want 1", n)
	}

	// 5. Verify still ran, on its own authority, and reused rather than
	//    re-executed: one execution of the plan across the whole run.
	if len(runner.calls) != 1 {
		t.Fatalf("the plan's command ran %d times end to end, want 1", len(runner.calls))
	}
	results := verifyResultsFor(t, store, created.Run.ID)
	if len(results) == 0 || reusedChecks(results[len(results)-1]) != 1 {
		t.Fatalf("verification did not reuse the evidence: %v", results)
	}

	// 6. Delivered.
	if verifyStepFrom(got).Step.State != domain.WorkflowStepCompleted {
		t.Fatalf("verify step = %q, want completed", verifyStepFrom(got).Step.State)
	}
	if got.Run.State != domain.WorkflowRunCompleted {
		t.Fatalf("run state = %q, want completed", got.Run.State)
	}
}

// REGRESSION. A report must not displace the work step's own latest checkpoint.
//
// dispatchReviewStep recovers the session, worktree, branch and completion
// fingerprint from GetLatestWorkflowCheckpointByStep(workStep). When the report
// was written with the work step's id it became that row the moment it was
// newer — which is every real run, because a clock advances between finishing
// and reporting — and the review dispatch then found a checkpoint with no
// session on it and stopped the run as ambiguous.
//
// The clock advance below is the whole test: without it the two rows share a
// timestamp and the bug hides.
func TestAWorkReportDoesNotDisplaceTheWorkStepCheckpoint(t *testing.T) {
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
	detail := completeWorkStepInDir(t, c, clk, sessionFacts, workspaceFacts, created.Run.ID, dir, ordinaryCodeChange())
	session := workSessionOf(t, detail)
	workStepID := workStepFrom(detail).Step.ID

	clk.Advance(30 * time.Second)
	if _, err := c.SubmitWorkReportForSession(ctx, session, reportFor("done")); err != nil {
		t.Fatalf("SubmitWorkReportForSession: %v", err)
	}

	// The work step's latest checkpoint is still the one carrying its facts.
	latest, ok, err := store.GetLatestWorkflowCheckpointByStep(ctx, workStepID)
	if err != nil || !ok {
		t.Fatalf("GetLatestWorkflowCheckpointByStep: %v (ok=%v)", err, ok)
	}
	if latest.DurablePhase == "work_report" {
		t.Fatal("the report became the work step's latest checkpoint; review dispatch reads that row")
	}
	if latest.SessionID == nil || latest.WorktreePath == "" {
		t.Fatalf("the work step's latest checkpoint lost its facts: %+v", latest)
	}

	// And the run proceeds rather than stopping as ambiguous.
	got, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if got.Run.State == domain.WorkflowRunNeedsAttention {
		t.Fatalf("the run stopped for attention after a report: %+v",
			checkpointsWithPhase(t, store, created.Run.ID, "review_dispatch_ambiguous"))
	}
	if n := len(checkpointsWithPhase(t, store, created.Run.ID, "review_dispatch_ambiguous")); n != 0 {
		t.Fatalf("the review dispatch went ambiguous (%d rows)", n)
	}
}

// fakeWorkerCredentialCloser records that the coordinator asked for a finished
// worker's identity to be taken back.
type fakeWorkerCredentialCloser struct {
	calls int
	err   error
}

func (f *fakeWorkerCredentialCloser) CloseFinishedWorkers(_ context.Context) error {
	f.calls++
	return f.err
}

// P5-A phase 2C audit: a worker's identity ends when its turn does, at the
// transition rather than a reconciliation interval later.
//
// The window matters because AgentRoleWorker holds session WRITE, which gates
// /send, /kill, /rollback, /switch-agent and the rest — and a worker session is
// reused across a step's whole loop, so a credential that outlived its turn
// could steer the session a later agent works in.
func TestAFinishedWorkStepEndsItsWorkerCredential(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	dir := t.TempDir()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: dir}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	closer := &fakeWorkerCredentialCloser{}

	store := newFakeStore()
	store.reviewRuns = reviewRuns
	clk := &fakeClock{t: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store: store, Spawner: spawner, SessionFacts: sessionFacts, WorkspaceFacts: workspaceFacts,
		ReviewRuns: reviewRuns, ReviewerLauncher: &fakeReviewerLauncher{}, Verifier: passingRunner(),
		WorkerCredentials: closer, Clock: clk.Now,
		NewID: func() string { idSeq++; return fmt.Sprintf("id%d", idSeq) },
	})
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "rename a helper", verifiablePlan())
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStepInDir(t, c, clk, sessionFacts, workspaceFacts, created.Run.ID, dir, ordinaryCodeChange())

	if closer.calls == 0 {
		t.Fatal("the work step finished and nothing asked for its worker's credential back")
	}
}

// A closer that fails must not fail the run: work that finished finished, and
// the derived sweep re-derives the obligation on its next pass.
func TestAFailedCredentialCloseDoesNotFailTheRun(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	dir := t.TempDir()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: dir}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	closer := &fakeWorkerCredentialCloser{err: errors.New("database is locked")}

	store := newFakeStore()
	store.reviewRuns = reviewRuns
	clk := &fakeClock{t: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store: store, Spawner: spawner, SessionFacts: sessionFacts, WorkspaceFacts: workspaceFacts,
		ReviewRuns: reviewRuns, ReviewerLauncher: &fakeReviewerLauncher{}, Verifier: passingRunner(),
		WorkerCredentials: closer, Clock: clk.Now,
		NewID: func() string { idSeq++; return fmt.Sprintf("id%d", idSeq) },
	})
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "rename a helper", verifiablePlan())
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// completeWorkStepInDir fails the test if the run does not reach completed,
	// so reaching the end of it IS the assertion.
	completeWorkStepInDir(t, c, clk, sessionFacts, workspaceFacts, created.Run.ID, dir, ordinaryCodeChange())
	if closer.calls == 0 {
		t.Fatal("the closer was never called")
	}
}

// A coordinator with no closer wired behaves exactly as it did before phase 2C:
// the derived sweep remains the only path, and it still converges.
func TestACoordinatorWithoutACloserIsUnchanged(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	dir := t.TempDir()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: dir}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	c, _, clk := newCoordinatorWithReviewAndVerifier(spawner, sessionFacts, workspaceFacts, reviewRuns, &fakeReviewerLauncher{}, passingRunner())
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "rename a helper", verifiablePlan())
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStepInDir(t, c, clk, sessionFacts, workspaceFacts, created.Run.ID, dir, ordinaryCodeChange())
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
}
