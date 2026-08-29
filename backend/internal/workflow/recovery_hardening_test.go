package workflow_test

// recovery_hardening_test.go — the regressions for the two workflows that sat
// blocked overnight with finished work in their worktrees.
//
//	wf-00283521 / medusa-4               agent_start_failed, work committed by hand as 74f053a6
//	wf-cd5bad10 / agent-orchestrator-35  verify_unrepairable, fixes committed by hand as 1de0aa7e
//	wf-f0efac7e (Medusa parent)          child_needs_attention -> cleared -> re-raised, 8s apart
//
// Each test below is one of those failures, and each asserts BOTH halves: the
// dead end is gone, and the safety rule it was standing on is not.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// ---- requirement 4 ---------------------------------------------------------

// The wf-cd5bad10 shape: the workspace differs from the approved review target
// because THIS run's own authorized fix worker changed it. That is code no
// reviewer has read yet, not code AO has a reason to distrust, and the remedy
// is a review — not verify_unrepairable.
func TestAuthorizedFixChangeGetsAFreshReviewInsteadOfAnUnexplainedStop(t *testing.T) {
	ctx := context.Background()
	runner := &fakeVerifyRunner{result: workflowcore.VerifyCommandExecution{ExitCode: 0}}
	c, store, clk, sessionFacts, workspaceFacts, reviewRuns, _, dir := driftFixture(t, runner)
	runID := driftStartRun(t, c, store, clk, sessionFacts, workspaceFacts, dir)

	got, err := c.ContinueRun(ctx, runID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	review := reviewStepFrom(got)
	if review.Step.ReviewRunID == nil {
		t.Fatal("review step has no review_run_id after dispatch")
	}
	firstReviewRunID := *review.Step.ReviewRunID
	reviewRuns.setStatus(firstReviewRunID, domain.ReviewRunComplete, domain.VerdictApproved)

	// The fix worker delivers, and keeps working past the observation that
	// closed its cycle — which is exactly what agent-orchestrator-35 did, and
	// why the tree ends at a fingerprint AO never recorded as a target.
	if err := os.WriteFile(filepath.Join(dir, "frontend", "src", "Board.tsx"),
		[]byte("export const Board = () => 'fixed twice';\n"), 0o644); err != nil {
		t.Fatalf("rewrite Board.tsx: %v", err)
	}
	delivered := workflowcore.WorkspaceFingerprint(workspaceFacts.obs)
	seedAuthorizedFixObservation(t, store, runID, delivered, clk.Now())

	clk.Advance(2 * time.Minute)
	got, err = c.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun after the fix landed: %v", err)
	}

	// The dead end is gone: the run is not parked on an unexplained workspace
	// change.
	if got.Run.State == domain.WorkflowRunNeedsAttention {
		reason := latestStopReason(t, store, runID)
		t.Fatalf("run parked on %q with its own authorized fix in the worktree — this is the wf-cd5bad10 regression", reason)
	}
	if runner.calls != 0 {
		t.Fatalf("verification ran %d times against a tree no reviewer has read; it must wait for the fresh review", runner.calls)
	}

	// The safety rule is intact: what AO did instead is ask for a review, and
	// the provenance it attributed the change to is on the record.
	prov := checkpointWithPhase(t, store, runID, "workspace_provenance")
	if !strings.Contains(prov.NextAction, string(workflowcore.ProvenanceAuthorizedFix)) &&
		!strings.Contains(prov.NextAction, string(workflowcore.ProvenanceAuthorizedWork)) {
		t.Fatalf("provenance record does not name an authorized class: %q", prov.NextAction)
	}
	if prov.FingerprintAfter != delivered {
		t.Fatalf("provenance observed fingerprint = %s, want the delivered one %s", prov.FingerprintAfter, delivered)
	}
	if _, ok := findCheckpoint(store, runID, "verify_provenance_fresh_review"); !ok {
		t.Fatal("no fresh review was authorized for the attributable change")
	}
	// And the next pass actually dispatches that review — against the workspace
	// as it stands now, not against the fingerprint the stale approval names.
	clk.Advance(time.Minute)
	got, err = c.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun (fresh review dispatch): %v", err)
	}
	after := reviewStepFrom(got)
	if after.Step.ReviewRunID == nil || *after.Step.ReviewRunID == firstReviewRunID {
		t.Fatalf("the stale approval is still the review step's answer (step %q); the reviewer was never re-asked", after.Step.State)
	}
	if target := reviewRuns.runs[*after.Step.ReviewRunID].TargetSHA; target == delivered {
		return
	} else if target == "" {
		t.Fatal("the fresh review was dispatched with no target")
	}
}

// ---- requirement 5 ---------------------------------------------------------

// The other half of the same rule, and the one that must not move: a change AO
// cannot attribute to its own authorized agents still blocks. Nothing about
// provenance makes an unowned edit verifiable.
func TestUnattributableWorkspaceChangeStaysBlocked(t *testing.T) {
	ctx := context.Background()
	runner := &fakeVerifyRunner{result: workflowcore.VerifyCommandExecution{ExitCode: 0}}
	c, store, clk, sessionFacts, workspaceFacts, reviewRuns, _, dir := driftFixture(t, runner)
	runID := driftStartRun(t, c, store, clk, sessionFacts, workspaceFacts, dir)

	got, err := c.ContinueRun(ctx, runID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	review := reviewStepFrom(got)
	if review.Step.ReviewRunID == nil {
		t.Fatal("review step has no review_run_id after dispatch")
	}
	reviewRuns.setStatus(*review.Step.ReviewRunID, domain.ReviewRunComplete, domain.VerdictApproved)

	// Somebody else edits the tree. No fix cycle was dispatched, no observation
	// of AO's ever recorded this state, and AO holds nothing that attributes it.
	if err := os.WriteFile(filepath.Join(dir, "frontend", "src", "Board.tsx"),
		[]byte("export const Board = () => 'unreviewed';\n"), 0o644); err != nil {
		t.Fatalf("rewrite Board.tsx: %v", err)
	}
	clk.Advance(2 * time.Minute)
	got, err = c.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun after the foreign edit: %v", err)
	}

	if runner.calls != 0 {
		t.Fatalf("verification ran %d times over an unattributable change; the guard must fire before execution", runner.calls)
	}
	if got.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention: an unowned edit must still stop the run", got.Run.State)
	}
	if _, ok := findCheckpoint(store, runID, "verify_provenance_fresh_review"); ok {
		t.Fatal("an unattributable change was granted a fresh review; that is a route around review")
	}
	prov := checkpointWithPhase(t, store, runID, "workspace_provenance")
	if strings.Contains(prov.NextAction, string(workflowcore.ProvenanceAuthorizedWork)) ||
		strings.Contains(prov.NextAction, string(workflowcore.ProvenanceAuthorizedFix)) {
		t.Fatalf("a foreign edit was classified as AO's own work: %q", prov.NextAction)
	}
	// The stop is still readable, and still says what it always said.
	verify := driftVerifyStep(t, got)
	found := false
	for _, a := range verify.Attempts {
		if a.ErrorClass == domain.WorkflowErrorVerifyWorkspaceChanged {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected verify_workspace_changed, attempts = %+v", verify.Attempts)
	}
}

// ---- requirements 6 and 7 --------------------------------------------------

// The incident C shape, end to end over a REAL git repository: AO lost the
// worker, a person committed the finished work on the task's own branch, and
// Continue must adopt it — and then still put it through review.
func TestManuallyLandedTaskCommitIsAdoptedAndStillReviewed(t *testing.T) {
	ctx := context.Background()
	fx := newAdoptionFixture(t)

	// AO gave up on the worker: the work step is durably failed and the run is
	// parked, exactly as both incidents left it.
	fx.parkOnLostWorker(t)

	// A person commits the finished work on the task's own branch — 74f053a6 /
	// 1de0aa7e, in miniature.
	head := fx.landWorkByHand(t, "backend/internal/codegraph/native.go", "package codegraph\n")

	got, err := fx.c.ContinueRun(ctx, fx.runID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}

	// Requirement 6: the commit was adopted, on the record, with its proofs.
	adoption, ok := findCheckpoint(fx.store, fx.runID, "work_commit_adopted")
	if !ok {
		t.Fatalf("the attributable commit %s was not adopted; run state = %q, stop = %q",
			head[:8], got.Run.State, latestStopReason(t, fx.store, fx.runID))
	}
	if adoption.HeadSHA != head {
		t.Fatalf("adopted head = %q, want %q", adoption.HeadSHA, head)
	}
	if !strings.Contains(adoption.RetryState, "\"dispatchBaseSha\":\""+fx.baseSHA+"\"") {
		t.Fatalf("the adoption record does not pin the dispatch base it proved descent from: %s", adoption.RetryState)
	}
	if workStepFrom(got).Step.State != domain.WorkflowStepCompleted {
		t.Fatalf("work step = %q after adoption, want completed", workStepFrom(got).Step.State)
	}
	if fx.spawner.calls != 1 {
		t.Fatalf("spawner calls = %d, want 1: AO must never start a second writer over an existing attributable changeset", fx.spawner.calls)
	}

	// Requirement 7: adoption buys a review, never a pass. The run is not
	// completed, no verification has run, and a reviewer is being asked.
	if got.Run.State == domain.WorkflowRunCompleted {
		t.Fatal("adoption completed the run without a review; adoption must never be a verdict")
	}
	if fx.verifier.calls != 0 {
		t.Fatalf("verification ran %d times on adopted, unreviewed work", fx.verifier.calls)
	}
	review := reviewStepFrom(got)
	if review.Step.ReviewRunID == nil {
		t.Fatalf("no review was dispatched for the adopted work; review step = %q", review.Step.State)
	}
	if target := fx.reviewRuns.runs[*review.Step.ReviewRunID].TargetSHA; target == "" {
		t.Fatal("the review was dispatched with no target; it would be reviewing nothing")
	}
}

// Adoption refuses everything it cannot prove. A commit that does not descend
// from the base this task was dispatched at is somebody else's history, and no
// amount of "the worktree is clean" makes it this task's result.
func TestAdoptionRefusesACommitThatDoesNotDescendFromTheDispatchBase(t *testing.T) {
	ctx := context.Background()
	fx := newAdoptionFixture(t)
	fx.parkOnLostWorker(t)
	fx.landWorkByHand(t, "backend/internal/codegraph/native.go", "package codegraph\n")
	// The history is rewritten out from under the task: the branch is reset to
	// an unrelated root, so the dispatch base is no longer an ancestor.
	fx.rewriteHistoryOffTheDispatchBase(t)

	if _, err := fx.c.ContinueRun(ctx, fx.runID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if _, ok := findCheckpoint(fx.store, fx.runID, "work_commit_adopted"); ok {
		t.Fatal("AO adopted a commit that does not descend from the task's dispatch base")
	}
	if fx.verifier.calls != 0 {
		t.Fatalf("verification ran %d times on work AO refused to adopt", fx.verifier.calls)
	}
}

// ---- requirements 8 and 9 --------------------------------------------------

// The wf-f0efac7e flap. The parent's mirror is derived from the child's DURABLE
// state, so it cannot be cleared by a pass that merely failed to name the
// child's stop — which is what an unrelated checkpoint landing on the child does.
func TestParentAttentionCannotClearWhileTheChildIsStillStopped(t *testing.T) {
	fx, ctx, masterID := startAutonomousObjective(t, twoTaskDependentPlan())
	_, childID := dispatchedChild(t, fx, masterID)

	parkOnHumanDecision(t, fx, childID)
	driveUntil(t, fx, 6, func() bool { return mirroredChildStop(t, fx, masterID) })
	if !mirroredChildStop(t, fx, masterID) {
		t.Fatal("the parent never mirrored its stopped child; the fixture never reached the state under test")
	}

	// An ordinary, unrelated row lands on the CHILD — an observation, a wake
	// note, an incident record. It says nothing about the stop, and before this
	// fix it made the parent's "is my child's stop human-owned?" lookup come
	// back empty for one pass, which unparked the parent. The next pass
	// re-derived the stop and parked it again: child_needs_attention,
	// attention_cleared, child_needs_attention, eight seconds apart.
	child, ok, err := fx.store.GetWorkflowRun(ctx, childID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(child): %v (found=%v)", err, ok)
	}
	if _, err := fx.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-noise-1",
		WorkflowRunID:  childID,
		ProjectID:      child.ProjectID,
		NextAction:     "an unrelated observation landing after the stop",
		DurablePhase:   "worker_observed_worker_active",
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      time.Now().UTC().Add(time.Second),
	}); err != nil {
		t.Fatalf("CreateWorkflowCheckpoint(noise): %v", err)
	}

	// Several poller passes. The child never left needs_attention, so the
	// parent's mirror must not move at all.
	driveUntil(t, fx, 6, func() bool { return false })

	if !mirroredChildStop(t, fx, masterID) {
		t.Fatal("the parent cleared its child mirror while the child was still stopped — this is the wf-f0efac7e flap")
	}
	if got := countCheckpointPhase(t, fx, masterID, "attention_cleared"); got != 0 {
		t.Fatalf("attention_cleared checkpoints on the master = %d, want 0 while the child is still stopped", got)
	}
}

// The same condition, re-derived on every poll for as long as the child stays
// stopped, is ONE durable record — not one per pass.
func TestRepeatedIdenticalChildAttentionIsDeduplicated(t *testing.T) {
	fx, _, masterID := startAutonomousObjective(t, twoTaskDependentPlan())
	_, childID := dispatchedChild(t, fx, masterID)

	parkOnHumanDecision(t, fx, childID)
	driveUntil(t, fx, 6, func() bool { return mirroredChildStop(t, fx, masterID) })
	if !mirroredChildStop(t, fx, masterID) {
		t.Fatal("the parent never mirrored its stopped child")
	}
	// Many more passes over one unchanged condition.
	driveUntil(t, fx, 10, func() bool { return false })

	if got := countCheckpointPhase(t, fx, masterID, workflowcore.ReasonChildNeedsAttention); got != 1 {
		t.Fatalf("child_needs_attention checkpoints on the master = %d, want exactly 1 for one unchanged child stop", got)
	}
}

// ---- requirement 13 --------------------------------------------------------

// One durable notification per real incident: the child's, which names an
// actionable reason. The parent's mirror is the same incident seen one level
// up, and it must not produce a second message however many passes re-derive it.
func TestNeedsAttentionEmailIsSentExactlyOncePerIncident(t *testing.T) {
	fx, _, masterID := startAutonomousObjective(t, twoTaskDependentPlan())
	_, childID := dispatchedChild(t, fx, masterID)

	parkOnHumanDecision(t, fx, childID)
	driveUntil(t, fx, 6, func() bool { return mirroredChildStop(t, fx, masterID) })
	driveUntil(t, fx, 10, func() bool { return false })

	if got := fx.emails.countOfType(domain.NotificationTaskNeedsAttention); got != 1 {
		t.Fatalf("task needs-attention notifications = %d, want exactly 1 across every poll of one unchanged incident", got)
	}
	if got := fx.emails.countOfType(domain.NotificationWorkflowNeedsAttention); got != 0 {
		t.Fatalf("the parent's mirror produced %d objective-level notifications, want 0: it is the same incident the child already reported", got)
	}
}

// ---- helpers ---------------------------------------------------------------

// seedAuthorizedFixObservation writes the durable row AO's own fix observation
// writes when a fix cycle delivers: the phase and the FingerprintAfter are the
// whole of the attribution, and seeding them is how this test stands in for a
// full review->fix->review cycle without re-testing one.
func seedAuthorizedFixObservation(t *testing.T, store *fakeStore, runID, fingerprint string, at time.Time) {
	t.Helper()
	steps, err := store.ListWorkflowSteps(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListWorkflowSteps: %v", err)
	}
	var fixStepID string
	for _, s := range steps {
		if s.Kind == domain.WorkflowStepFix {
			fixStepID = s.ID
		}
	}
	if fixStepID == "" {
		t.Fatal("run has no fix step")
	}
	if _, err := store.CreateWorkflowCheckpoint(context.Background(), domain.WorkflowCheckpoint{
		ID:               "wfc-seeded-fix-observed",
		WorkflowRunID:    runID,
		WorkflowStepID:   &fixStepID,
		ProjectID:        "proj-1",
		FingerprintAfter: fingerprint,
		NextAction:       "fix delivered a new workspace fingerprint",
		DurablePhase:     "fix_observed_" + string(domain.WorkflowStepWaiting),
		PayloadVersion:   "v1",
		RetryState:       "{}",
		CreatedAt:        at,
	}); err != nil {
		t.Fatalf("CreateWorkflowCheckpoint(fix observation): %v", err)
	}
}

func findCheckpoint(store *fakeStore, runID, phase string) (domain.WorkflowCheckpoint, bool) {
	cps, err := store.ListWorkflowCheckpoints(context.Background(), runID)
	if err != nil {
		return domain.WorkflowCheckpoint{}, false
	}
	for i := len(cps) - 1; i >= 0; i-- {
		if cps[i].DurablePhase == phase {
			return cps[i], true
		}
	}
	return domain.WorkflowCheckpoint{}, false
}

func checkpointWithPhase(t *testing.T, store *fakeStore, runID, phase string) domain.WorkflowCheckpoint {
	t.Helper()
	cp, ok := findCheckpoint(store, runID, phase)
	if !ok {
		t.Fatalf("no %q checkpoint on run %s", phase, runID)
	}
	return cp
}

func latestStopReason(t *testing.T, store *fakeStore, runID string) string {
	t.Helper()
	cps, err := store.ListWorkflowCheckpoints(context.Background(), runID)
	if err != nil || len(cps) == 0 {
		return ""
	}
	return cps[len(cps)-1].DurablePhase
}

// adoptionFixture drives the real dispatch path over a REAL git repository,
// because every proof adoption rests on is a Git read and a stub that answered
// them would be asserting AO's opinion of the branch rather than the branch.
type adoptionFixture struct {
	c          *workflowcore.Coordinator
	store      *fakeStore
	clk        *fakeClock
	spawner    *fakeSpawner
	facts      *fakeSessionFacts
	workspace  *realWorkspaceFacts
	reviewRuns *fakeReviewRuns
	verifier   *fakeVerifyRunner
	runID      string
	stepID     string
	sessionID  domain.SessionID
	dir        string
	branch     string
	baseSHA    string
}

// realWorkspaceFacts observes an actual git worktree, so HEAD, cleanliness and
// the commit list are the repository's answers rather than the test's.
type realWorkspaceFacts struct {
	dir    string
	branch string
}

func (f *realWorkspaceFacts) ObserveWorkspace(_ context.Context, _ ports.WorkspaceInfo) (ports.WorkspaceObservation, error) {
	head := strings.TrimSpace(adoptionGit(nil, f.dir, "rev-parse", "HEAD"))
	status := strings.TrimSpace(adoptionGit(nil, f.dir, "status", "--porcelain=v1", "--untracked-files=all"))
	obs := ports.WorkspaceObservation{Path: f.dir, Branch: f.branch, HeadSHA: head}
	for _, line := range strings.Split(status, "\n") {
		if len(line) < 4 {
			continue
		}
		obs.Changes = append(obs.Changes, ports.WorkspaceChange{Status: line[:2], Path: strings.TrimSpace(line[3:])})
		obs.Dirty = true
	}
	for _, line := range strings.Split(strings.TrimSpace(adoptionGit(nil, f.dir, "log", "-n", "20", "--pretty=format:%H %s")), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		commit := ports.WorkspaceCommit{SHA: parts[0]}
		if len(parts) > 1 {
			commit.Subject = parts[1]
		}
		obs.Commits = append(obs.Commits, commit)
	}
	return obs, nil
}

func newAdoptionFixture(t *testing.T) *adoptionFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	branch := "ao/task-codegraph"
	adoptionGit(t, dir, "init", "-q", "-b", branch)
	adoptionGit(t, dir, "config", "user.email", "ao@example.test")
	adoptionGit(t, dir, "config", "user.name", "AO")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	adoptionGit(t, dir, "add", ".")
	adoptionGit(t, dir, "commit", "-q", "-m", "base")
	base := strings.TrimSpace(adoptionGit(t, dir, "rev-parse", "HEAD"))

	facts := newFakeSessionFacts()
	spawner := &fakeSpawner{
		rec:   domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: branch, WorkspacePath: dir}},
		facts: facts,
	}
	workspace := &realWorkspaceFacts{dir: dir, branch: branch}
	reviewRuns := newFakeReviewRuns()
	verifier := &fakeVerifyRunner{result: workflowcore.VerifyCommandExecution{ExitCode: 0}}
	store := newFakeStore()
	clk := &fakeClock{t: time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC)}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store:            store,
		Spawner:          spawner,
		SessionFacts:     facts,
		WorkspaceFacts:   workspace,
		ReviewRuns:       reviewRuns,
		ReviewerLauncher: &fakeReviewerLauncher{},
		Verifier:         verifier,
		Clock:            clk.Now,
		NewID:            func() string { idSeq++; return "adopt" + itoa(idSeq) },
	})

	created, err := c.CreateRun(ctx, "proj-1", "add a native code graph indexer")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	detail, err := c.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	work := workStepFrom(detail)
	sessionID := domain.SessionID(*work.Step.SessionID)
	facts.put(domain.SessionRecord{
		ID: sessionID, ProjectID: "proj-1",
		Activity: domain.Activity{State: domain.ActivityIdle},
		Metadata: domain.SessionMetadata{Branch: branch, WorkspacePath: dir},
	})
	return &adoptionFixture{
		c: c, store: store, clk: clk, spawner: spawner, facts: facts,
		workspace: workspace, reviewRuns: reviewRuns, verifier: verifier,
		runID: created.Run.ID, stepID: work.Step.ID, sessionID: sessionID,
		dir: dir, branch: branch, baseSHA: base,
	}
}

// parkOnLostWorker puts the run in the state both incidents actually left:
// the work step durably failed, the run parked, the attempt closed, and the
// worker session long silent.
func (f *adoptionFixture) parkOnLostWorker(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	now := f.clk.Now()
	if _, err := f.store.UpdateWorkflowStepState(ctx, f.stepID, domain.WorkflowStepRunning, domain.WorkflowStepFailed, now); err != nil {
		t.Fatalf("fail the work step: %v", err)
	}
	if latest, ok, err := f.store.GetLatestWorkflowAttempt(ctx, f.stepID); err == nil && ok {
		if err := f.store.UpdateWorkflowAttemptOutcome(ctx, latest.ID, now, domain.WorkflowAttemptFailed, domain.WorkflowErrorAgentStartFailed); err != nil {
			t.Fatalf("close the attempt: %v", err)
		}
	}
	run, _, err := f.store.GetWorkflowRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if _, err := f.store.UpdateWorkflowRunState(ctx, f.runID, run.State, domain.WorkflowRunNeedsAttention, now); err != nil {
		t.Fatalf("park the run: %v", err)
	}
	if _, err := f.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-lost-worker",
		WorkflowRunID:  f.runID,
		WorkflowStepID: &f.stepID,
		ProjectID:      "proj-1",
		NextAction:     "worker produced no first signal and AO lost track of it",
		DurablePhase:   workflowcore.ReasonWorkerDispatchAmbiguous,
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      now,
	}); err != nil {
		t.Fatalf("record the stop: %v", err)
	}
	// The agent has been silent for hours, which is what makes the tree
	// attributable at all.
	f.clk.Advance(4 * time.Hour)
	f.facts.put(domain.SessionRecord{
		ID: f.sessionID, ProjectID: "proj-1",
		Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-time.Hour)},
		Metadata: domain.SessionMetadata{Branch: f.branch, WorkspacePath: f.dir},
	})
}

// landWorkByHand is the person committing the finished work on the task's own
// branch, as happened for both incidents.
func (f *adoptionFixture) landWorkByHand(t *testing.T, path, content string) string {
	t.Helper()
	full := filepath.Join(f.dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	adoptionGit(t, f.dir, "add", ".")
	adoptionGit(t, f.dir, "commit", "-q", "-m", "feat(codegraph): native indexer")
	return strings.TrimSpace(adoptionGit(t, f.dir, "rev-parse", "HEAD"))
}

// rewriteHistoryOffTheDispatchBase drops the task's starting point out of the
// branch entirely — an amend, a reset or a rebase, in its most extreme form.
func (f *adoptionFixture) rewriteHistoryOffTheDispatchBase(t *testing.T) {
	t.Helper()
	adoptionGit(t, f.dir, "checkout", "-q", "--orphan", "rewritten")
	adoptionGit(t, f.dir, "commit", "-q", "-m", "unrelated history", "--allow-empty")
	adoptionGit(t, f.dir, "branch", "-q", "-M", f.branch)
}

// adoptionGit runs git in dir. It fails the test on error when t is non-nil,
// and returns "" otherwise (the observation path, which must not fail a test
// from inside a coordinator call).
func adoptionGit(t *testing.T, dir string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil && t != nil {
		t.Helper()
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func itoa(n int) string { return strconv.Itoa(n) }

// ---- requirement 11 --------------------------------------------------------

// A diagnosis is a durable background job. Closing the modal, dropping the
// connection and restarting the daemon change nothing about it — and, crucially,
// an investigation blocked on an interactive prompt reports THAT rather than
// "investigating", which is how one sat silent all night.
func TestDiagnosticJobSurvivesTheModalAndReportsWhatItIsBlockedOn(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonFixCycleNotStarted, "the worker never started this cycle")
	ctx := context.Background()

	inc, _, err := f.c.RequestIncidentDiagnosis(ctx, f.runID)
	if err != nil {
		t.Fatalf("RequestIncidentDiagnosis: %v", err)
	}
	if f.agents.diagnostics != 1 {
		t.Fatalf("diagnostic launches = %d, want 1", f.agents.diagnostics)
	}

	// The person closes the modal and the daemon restarts. Everything about the
	// job has to come back from the ledger.
	f.c = f.newCoordinator()
	reloaded, err := f.c.LoadIncident(ctx, f.runID, inc.ID)
	if err != nil {
		t.Fatalf("LoadIncident after restart: %v", err)
	}
	if reloaded.State != workflowcore.IncidentDiagnosing {
		t.Fatalf("incident state after restart = %q, want diagnosing: the job did not survive", reloaded.State)
	}
	job := f.c.DeriveIncidentDiagnosisJob(ctx, reloaded)
	if job.SessionID != "diag-session" {
		t.Fatalf("job session = %q, want the session the launch actually produced", job.SessionID)
	}
	if job.StartedAt.IsZero() {
		t.Fatal("the job has no start time, so nobody can tell how long it has been going")
	}
	if job.Attempt != 1 || job.Max != workflowcore.MaxIncidentDiagnoses {
		t.Fatalf("job attempt = %d of %d, want 1 of %d", job.Attempt, job.Max, workflowcore.MaxIncidentDiagnoses)
	}

	// The agent is alive and working: running.
	f.sessionFacts.put(domain.SessionRecord{
		ID: "diag-session", Harness: "claude-code",
		Activity:      domain.Activity{State: domain.ActivityActive, LastActivityAt: f.clk.Now()},
		FirstSignalAt: f.clk.Now(),
	})
	if job := f.c.DeriveIncidentDiagnosisJob(ctx, reloaded); job.State != workflowcore.DiagnosisRunning {
		t.Fatalf("job state = %q, want running while the agent is active", job.State)
	}

	// The agent hits "Yes, I trust this folder". The whole incident is that AO
	// reported this as progress; it must report it as a person being needed.
	f.sessionFacts.put(domain.SessionRecord{
		ID: "diag-session", Harness: "claude-code",
		Activity: domain.Activity{State: domain.ActivityBlocked, LastActivityAt: f.clk.Now()},
	})
	job = f.c.DeriveIncidentDiagnosisJob(ctx, reloaded)
	if job.State != workflowcore.DiagnosisWaitingForUser {
		t.Fatalf("job state = %q, want waiting_for_user: a blocked agent is not an investigation in progress", job.State)
	}
	if job.BlockingInteraction == "" {
		t.Fatal("the job says it is waiting for a person without saying what for")
	}
	if st := f.c.DeriveIncidentStatus(ctx, reloaded); st.Progress != workflowcore.IncidentProgressDiagnosisBlocked {
		t.Fatalf("incident progress = %q, want diagnosis_blocked: this is the 'Investigando' misreport", st.Progress)
	}

	// A silent session past the startup grace is the same conclusion reached
	// from the other direction — the shape a trust prompt has when the provider
	// fires no hook at all.
	f.sessionFacts.put(domain.SessionRecord{
		ID: "diag-session", Harness: "claude-code",
		Activity: domain.Activity{State: domain.ActivityIdle},
	})
	f.clk.Advance(20 * time.Minute)
	if job := f.c.DeriveIncidentDiagnosisJob(ctx, reloaded); job.State != workflowcore.DiagnosisWaitingForUser {
		t.Fatalf("job state = %q for an agent that has never signalled, want waiting_for_user", job.State)
	}
}

// ---- requirement 12 --------------------------------------------------------

// A parent stopped on a child must not answer "go and diagnose the child
// first". Its pack carries the child's own bounded evidence, because AO was
// holding the child's run id the whole time.
func TestParentIncidentPackContainsBoundedChildEvidence(t *testing.T) {
	fx, ctx, masterID := startAutonomousObjective(t, twoTaskDependentPlan())
	_, childID := dispatchedChild(t, fx, masterID)

	parkOnHumanDecision(t, fx, childID)
	driveUntil(t, fx, 6, func() bool { return mirroredChildStop(t, fx, masterID) })
	if !mirroredChildStop(t, fx, masterID) {
		t.Fatal("the parent never mirrored its stopped child")
	}

	_, pack, err := fx.coord.IncidentPackFor(ctx, masterID)
	if err != nil {
		t.Fatalf("IncidentPackFor(parent): %v", err)
	}
	rendered := pack.Render()
	if !strings.Contains(rendered, childID) {
		t.Fatalf("the parent's pack never names the child run it is stopped on:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Stopped child task") {
		t.Fatalf("the parent's pack has no child evidence section:\n%s", rendered)
	}
	// The child's own REASON — not just its id — is what makes the second
	// investigation unnecessary.
	childReason := latestHumanOwnedStopReason(t, fx, childID)
	if childReason != "" && !strings.Contains(rendered, childReason) {
		t.Fatalf("the parent's pack does not carry the child's stop reason %q:\n%s", childReason, rendered)
	}
	// Bounded, like every other section: the budget is not waived for it.
	if pack.Bytes > pack.MaxBytes {
		t.Fatalf("pack is %d bytes, over its own %d-byte budget", pack.Bytes, pack.MaxBytes)
	}
}

// ---- requirement 14 --------------------------------------------------------

// Nothing here buys a run a shortcut. The three rules the recovery work is most
// likely to have eroded are asserted directly, on the same vocabulary the
// production code decides from.
func TestExistingSafetyGuaranteesAreIntact(t *testing.T) {
	t.Run("adoption never marks a task complete", func(t *testing.T) {
		ctx := context.Background()
		fx := newAdoptionFixture(t)
		fx.parkOnLostWorker(t)
		fx.landWorkByHand(t, "backend/internal/codegraph/native.go", "package codegraph\n")
		got, err := fx.c.ContinueRun(ctx, fx.runID)
		if err != nil {
			t.Fatalf("ContinueRun: %v", err)
		}
		if got.Run.State.Terminal() {
			t.Fatalf("run reached %q on an adoption; adoption is never a verdict", got.Run.State)
		}
		for _, s := range got.Steps {
			if s.Step.Kind == domain.WorkflowStepVerify && s.Step.State == domain.WorkflowStepCompleted {
				t.Fatal("the verify step completed without verification ever running")
			}
		}
		if fx.verifier.calls != 0 {
			t.Fatalf("verification ran %d times on adopted, unreviewed work", fx.verifier.calls)
		}
	})

	t.Run("a destructive incident action still requires a person", func(t *testing.T) {
		// cancel_run ends work and repair_agent writes code. Neither may ever
		// become something AO does on its own say-so, and asserting it against
		// the most permissive class (auto_recoverable) is the strongest form of
		// the claim.
		for _, kind := range []workflowcore.IncidentActionKind{
			workflowcore.IncidentActionCancelRun,
			workflowcore.IncidentActionRepairAgent,
		} {
			desc := workflowcore.DescribeIncidentAction(workflowcore.IncidentAutoRecoverable, kind)
			needsApproval, endsWork, writesCode := desc.NeedsApproval, desc.EndsWork, desc.WritesCode
			if !needsApproval {
				t.Fatalf("%s no longer requires human approval", kind)
			}
			if kind == workflowcore.IncidentActionCancelRun && !endsWork {
				t.Fatal("cancel_run no longer declares that it ends work")
			}
			if kind == workflowcore.IncidentActionRepairAgent && !writesCode {
				t.Fatal("repair_agent no longer declares that it writes code")
			}
		}
	})

	t.Run("every human-owned stop still names an action", func(t *testing.T) {
		// The invariant ClassifyAttention rests on: a stop AO bills to the user
		// must say what the user does. The three provider-preflight reasons
		// added by this work are covered by the same sweep as every older one.
		for _, reason := range []string{
			workflowcore.ReasonProviderAuthRequired,
			workflowcore.ReasonProviderWorkspaceTrustRequired,
			workflowcore.ReasonProviderPreflightFailed,
			workflowcore.ReasonVerifyWorkspaceUnattributable,
			workflowcore.ReasonVerifyUnrepairable,
			workflowcore.ReasonWorkerDispatchAmbiguous,
		} {
			verdict := workflowcore.ClassifyAttention(
				runDetailStoppedOn(reason), nil, workflowcore.PhaseNeedsAttention)
			if verdict.Attention == workflowcore.AttentionHuman && verdict.Action == "" {
				t.Fatalf("%s reaches a person with nothing to do", reason)
			}
			if verdict.Reason != reason {
				t.Fatalf("stop %q classified as %q", reason, verdict.Reason)
			}
		}
	})
}

// runDetailStoppedOn is the minimal RunDetail ClassifyAttention needs: a run
// parked with one canonical reason recorded.
func runDetailStoppedOn(reason string) workflowcore.RunDetail {
	return workflowcore.RunDetail{
		Run:                   domain.WorkflowRun{ID: "wf-x", State: domain.WorkflowRunNeedsAttention},
		LatestCheckpointPhase: reason,
	}
}

// latestHumanOwnedStopReason reads the child's own recorded stop reason.
func latestHumanOwnedStopReason(t *testing.T, fx *autonomousFixture, runID string) string {
	t.Helper()
	cps, err := fx.store.ListWorkflowCheckpoints(context.Background(), runID)
	if err != nil {
		return ""
	}
	for i := len(cps) - 1; i >= 0; i-- {
		if cps[i].DurablePhase != "" && strings.Contains(cps[i].DurablePhase, "_") &&
			!strings.HasPrefix(cps[i].DurablePhase, "incident_") &&
			!strings.HasPrefix(cps[i].DurablePhase, "worker_observed_") &&
			!strings.HasPrefix(cps[i].DurablePhase, "routing_") {
			return cps[i].DurablePhase
		}
	}
	return ""
}
