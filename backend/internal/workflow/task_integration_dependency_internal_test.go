package workflow

// Dependency-aware re-integration and stale-review recovery, at the workflow
// seam: does the plan's own dependency data actually reach the Integration
// Coordinator, and does the Coordinator's "this approval went stale" answer
// actually re-open the task's review instead of parking a person on it.
//
// The git-level properties (what a rebase does to a diff, what a ref reaches)
// are covered against real git in internal/integration. What is covered here is
// the wiring: the ledger read, the two sentinels that must not park anything,
// and the three durable state changes a fresh review rests on.

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/integration"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	sqlitetest "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// depTaskFixture is one master run with a sibling already on the integration
// ref and one task of its own, in an AO-owned worktree cut from before that
// sibling landed. siblingFile/siblingBody describe what the sibling put on the
// ref, which is what decides whether replaying this task changes what it
// contributes.
type depTaskFixture struct {
	store               *sqlite.Store
	coord               *Coordinator
	ctx                 stdctx.Context
	repo                string
	worktree            string
	refName             string
	baseSHA             string
	movedSHA            string
	master              domain.WorkflowRun
	task                domain.WorkflowTask
	child               RunDetail
	reviewRunID         string
	approvedFingerprint string
}

type depTaskWork struct {
	name, body string
}

func newDepTaskFixture(t *testing.T, siblingWork depTaskWork, taskWork []depTaskWork) *depTaskFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := stdctx.Background()
	store := sqlitetest.MustOpen(t)
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o750); err != nil {
		t.Fatal(err)
	}
	laneGit(t, repo, "init", "--initial-branch=main")
	laneGit(t, repo, "config", "user.email", "ao@example.com")
	laneGit(t, repo, "config", "user.name", "Ao Agents")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	laneGit(t, repo, "add", ".")
	laneGit(t, repo, "commit", "-m", "seed")
	baseSHA := laneGit(t, repo, "rev-parse", "HEAD")

	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: repo, RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	sess, err := store.CreateSession(ctx, domain.SessionRecord{ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessCodex, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	master := domain.WorkflowRun{ID: "wf-master", ProjectID: "p", Objective: "master", State: domain.WorkflowRunRunning,
		PolicyVersion: policyVersionV1, PolicySnapshot: "{}", CreatedAt: now, UpdatedAt: now}
	if _, _, err := store.CreateWorkflowRun(ctx, master, nil); err != nil {
		t.Fatal(err)
	}
	refName := masterIntegrationRefName(master.ID)

	// The dependency landed first: the integration ref is past the commit this
	// task's worktree was cut from.
	laneGit(t, repo, "checkout", "-q", "-b", "sibling", baseSHA)
	if err := os.WriteFile(filepath.Join(repo, siblingWork.name), []byte(siblingWork.body), 0o600); err != nil {
		t.Fatal(err)
	}
	laneGit(t, repo, "add", siblingWork.name)
	laneGit(t, repo, "commit", "-m", "sibling task")
	movedSHA := laneGit(t, repo, "rev-parse", "HEAD")
	laneGit(t, repo, "update-ref", refName, movedSHA)
	laneGit(t, repo, "checkout", "-q", "main")

	worktree := filepath.Join(root, "worktrees", "task-1")
	laneGit(t, repo, "worktree", "add", "-q", "-b", "ao/task-1", worktree, baseSHA)
	for _, work := range taskWork {
		if err := os.WriteFile(filepath.Join(worktree, work.name), []byte(work.body), 0o600); err != nil {
			t.Fatal(err)
		}
		laneGit(t, worktree, "add", work.name)
		laneGit(t, worktree, "commit", "-m", "task work "+work.name)
	}

	childID, taskID := "wf-exec-task-1", "task-1"
	artifactJSON, err := MarshalPlanArtifact(BuildPlanArtifact("p", "task", policyVersionV1, VerificationPlan{
		Commands: []VerificationCommandCheck{{Command: "go", Args: []string{"test", "./..."}, RequiredExitCode: 0, RetrySafe: true}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	reviewRunID, approvedFingerprint := "rr-approved", "fingerprint-approved"
	steps := []domain.WorkflowStep{
		{ID: "wfs-plan", WorkflowRunID: childID, Kind: domain.WorkflowStepPlan, Ordinal: 0, State: domain.WorkflowStepCompleted, ArtifactJSON: artifactJSON, CreatedAt: now, UpdatedAt: now},
		{ID: "wfs-work", WorkflowRunID: childID, Kind: domain.WorkflowStepWork, Ordinal: 1, State: domain.WorkflowStepCompleted, ArtifactJSON: "{}", CreatedAt: now, UpdatedAt: now},
		{ID: "wfs-review", WorkflowRunID: childID, Kind: domain.WorkflowStepReview, Ordinal: 2, State: domain.WorkflowStepCompleted, ArtifactJSON: "{}", ReviewRunID: &reviewRunID, CreatedAt: now, UpdatedAt: now},
		{ID: "wfs-verify", WorkflowRunID: childID, Kind: domain.WorkflowStepVerify, Ordinal: 3, State: domain.WorkflowStepCompleted, ArtifactJSON: "{}", CreatedAt: now, UpdatedAt: now},
	}
	childRun := domain.WorkflowRun{ID: childID, ProjectID: "p", Objective: "task", State: domain.WorkflowRunCompleted,
		PolicyVersion: policyVersionV1, PolicySnapshot: "{}", CreatedAt: now, UpdatedAt: now,
		ParentWorkflowID: &master.ID, PlannedTaskID: &taskID}
	createdRun, createdSteps, err := store.CreateWorkflowRun(ctx, childRun, steps)
	if err != nil {
		t.Fatal(err)
	}
	sessID := string(sess.ID)
	workStepID := ""
	for _, step := range createdSteps {
		if step.Kind == domain.WorkflowStepWork {
			workStepID = step.ID
		}
	}
	if _, err := store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-work", WorkflowRunID: childID, WorkflowStepID: &workStepID,
		ProjectID: "p", SessionID: &sessID, Branch: "ao/task-1", WorktreePath: worktree,
		BaseSHA: baseSHA, DurablePhase: "work_completed", PayloadVersion: "v1", RetryState: "{}", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// The approval that will go stale, as a real row: the review step's
	// review_run_id has a foreign key, so a fixture that only pretended a review
	// run existed could not exercise this path at all.
	if err := store.UpsertReview(ctx, domain.Review{
		ID: "rev-1", SessionID: sess.ID, ProjectID: "p", Harness: domain.ReviewerClaudeCode,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertReviewRun(ctx, domain.ReviewRun{
		ID: reviewRunID, ReviewID: "rev-1", SessionID: sess.ID, Harness: domain.ReviewerClaudeCode,
		TriggerSource: domain.ReviewTriggerAuto, TargetSHA: approvedFingerprint,
		Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// The review step's own pointer at that run. Set through the store rather
	// than in the CreateWorkflowRun literal because review_run_id is written by
	// dispatch, not by run creation.
	for _, step := range createdSteps {
		if step.Kind != domain.WorkflowStepReview {
			continue
		}
		if _, err := store.SetWorkflowStepReviewRun(ctx, step.ID, reviewRunID, now); err != nil {
			t.Fatal(err)
		}
	}
	refreshed, err := store.ListWorkflowSteps(ctx, childID)
	if err != nil {
		t.Fatal(err)
	}
	createdSteps = refreshed

	detail := RunDetail{Run: createdRun}
	for _, s := range createdSteps {
		sd := StepDetail{Step: s}
		if s.Kind == domain.WorkflowStepReview {
			sd.Review = &ReviewSummary{Verdict: domain.VerdictApproved}
		}
		detail.Steps = append(detail.Steps, sd)
	}

	coord := New(Deps{
		Store: store, Projects: store, WorkspaceFacts: &fakeMaterializer{}, IntegrationLocks: newLaneStub(),
		Verifier: &laneVerifyRunner{}, ReviewRuns: store, ReviewerLauncher: depReviewerLauncher{},
		Clock: func() time.Time { return time.Now().UTC() },
	})
	return &depTaskFixture{
		store: store, coord: coord, ctx: ctx, repo: repo, worktree: worktree, refName: refName,
		baseSHA: baseSHA, movedSHA: movedSHA, master: master,
		task:                seedPlanTask(t, ctx, store, master.ID, taskID, 1, domain.WorkflowTaskRunning, nil),
		child:               detail,
		reviewRunID:         reviewRunID,
		approvedFingerprint: approvedFingerprint,
	}
}

// dependsOnSibling makes task-1's plan say what it actually depends on. The
// scope is the plan's own field (s1's classifier writes it); nothing else about
// the task changes.
func (f *depTaskFixture) dependsOnSibling(t *testing.T, siblingTaskID string) {
	t.Helper()
	scope, err := json.Marshal(domain.WorkflowTaskScope{IntegrationDependencies: []string{siblingTaskID}})
	if err != nil {
		t.Fatal(err)
	}
	f.task.ScopeJSON = string(scope)
}

// siblingIntegratedAt writes the ledger row the dependency's own integration
// would have written.
func (f *depTaskFixture) siblingIntegratedAt(t *testing.T, siblingTaskID, targetAfter string) {
	t.Helper()
	payload, err := json.Marshal(taskIntegrationPayload{
		TaskID: siblingTaskID, Outcome: string(integration.OutcomeIntegrated),
		Strategy: string(integration.StrategyFastForward), TargetRef: f.refName,
		TargetBeforeSHA: f.baseSHA, TargetAfterSHA: targetAfter,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
		ID: "wfc-ledger-" + siblingTaskID, WorkflowRunID: f.master.ID, ProjectID: "p",
		BaseSHA: f.baseSHA, HeadSHA: targetAfter, RetryState: string(payload),
		DurablePhase: taskIntegrationDurablePhase, PayloadVersion: taskIntegrationPayloadVersion,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *depTaskFixture) refHead(t *testing.T) string {
	t.Helper()
	return laneGit(t, f.repo, "rev-parse", f.refName)
}

func (f *depTaskFixture) taskState(t *testing.T) domain.WorkflowTask {
	t.Helper()
	tasks, err := f.store.ListWorkflowTasks(f.ctx, f.master.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.ID == f.task.ID {
			return task
		}
	}
	t.Fatalf("task %s disappeared", f.task.ID)
	return domain.WorkflowTask{}
}

func (f *depTaskFixture) countPhase(t *testing.T, runID, phase string) int {
	t.Helper()
	cps, err := f.store.ListWorkflowCheckpoints(f.ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, cp := range cps {
		if cp.DurablePhase == phase {
			n++
		}
	}
	return n
}

// A task whose required dependency has not integrated is early, not blocked and
// not broken: it must not move the ref, must not be parked, and must not record
// an attempt that never happened.
func TestTaskWaitsUntilItsIntegrationDependencyHasLanded(t *testing.T) {
	f := newDepTaskFixture(t, depTaskWork{"sibling.txt", "sibling work\n"}, []depTaskWork{{"task.txt", "task work\n"}})
	f.dependsOnSibling(t, "task-sibling")

	err := f.coord.promoteTaskToIntegration(f.ctx, f.master, f.task, f.child)
	if !errors.Is(err, errIntegrationWaitingOnDependency) {
		t.Fatalf("err = %v, want errIntegrationWaitingOnDependency", err)
	}
	if got := f.refHead(t); got != f.movedSHA {
		t.Fatalf("the integration ref moved for a task whose dependency has not landed: %s", got)
	}
	if state := f.taskState(t); state.State != domain.WorkflowTaskRunning {
		t.Fatalf("task state = %q, want it left running (waiting is not a stop)", state.State)
	}
	if n := f.countPhase(t, f.master.ID, taskIntegrationDurablePhase); n != 0 {
		t.Fatalf("%d integration attempts recorded for an integration that never started", n)
	}

	// The dependency lands. The same task, unchanged, now integrates.
	f.siblingIntegratedAt(t, "task-sibling", f.movedSHA)
	if err := f.coord.promoteTaskToIntegration(f.ctx, f.master, f.task, f.child); err != nil {
		t.Fatalf("promote after the dependency landed: %v", err)
	}
	after := f.refHead(t)
	if after == f.movedSHA {
		t.Fatal("the integration ref did not move once the dependency had landed")
	}
	// And it landed on top of the dependency, not instead of it.
	if merged := laneGit(t, f.repo, "merge-base", after, f.movedSHA); merged != f.movedSHA {
		t.Fatalf("the task integrated onto a target that excludes its dependency (merge-base %s)", merged)
	}

	// The other half of the stale-review decision: this rebase left the task's
	// own contribution alone, so its approval was reused rather than re-asked --
	// and the content, which IS new, was verified again before it landed.
	attempts, err := f.coord.ListTaskIntegrations(f.ctx, f.master.ID)
	if err != nil {
		t.Fatal(err)
	}
	var landed *TaskIntegrationRecord
	for i := range attempts {
		if attempts[i].TaskID == f.task.ID && attempts[i].Outcome == string(integration.OutcomeIntegrated) {
			landed = &attempts[i]
		}
	}
	if landed == nil {
		t.Fatal("the integration ledger has no row for the landed task")
	}
	if !landed.Replayed || landed.Strategy != string(integration.StrategyRebaseFastForward) {
		t.Fatalf("ledger row = %+v, want a replay onto the moved target", landed)
	}
	if landed.EffectiveBefore == "" || landed.EffectiveBefore != landed.EffectiveAfter {
		t.Fatalf("the rebase changed the task's contribution: %+v", landed)
	}
	if !landed.ReviewReused {
		t.Fatal("the ledger does not say the prior approval was reused")
	}
	if !landed.VerificationRan || !landed.VerificationOK ||
		landed.VerificationSource != string(integration.SourcePostReplay) {
		t.Fatalf("verification evidence = %+v, want a post-replay run", landed)
	}
	if len(landed.Dependencies) != 1 || landed.Dependencies[0] != "task-sibling" {
		t.Fatalf("recorded dependencies = %v, want the sibling this task required", landed.Dependencies)
	}
	if n := f.countPhase(t, f.child.Run.ID, integrationFreshReviewRequiredPhase); n != 0 {
		t.Fatalf("%d fresh reviews were asked for a rebase that changed nothing", n)
	}
}

// A dependency that DID integrate and has since been rewritten off the ref is
// the one dependency case a person owns.
func TestDependencyRewrittenOffTheIntegrationRefParksTheTask(t *testing.T) {
	f := newDepTaskFixture(t, depTaskWork{"sibling.txt", "sibling work\n"}, []depTaskWork{{"task.txt", "task work\n"}})
	f.dependsOnSibling(t, "task-sibling")
	f.siblingIntegratedAt(t, "task-sibling", f.movedSHA)
	// Something outside AO rewinds the ref past the dependency's work.
	laneGit(t, f.repo, "update-ref", f.refName, f.baseSHA)

	err := f.coord.promoteTaskToIntegration(f.ctx, f.master, f.task, f.child)
	if !errors.Is(err, errIntegrationTaskConflict) {
		t.Fatalf("err = %v, want errIntegrationTaskConflict", err)
	}
	state := f.taskState(t)
	if !state.State.Parked() {
		t.Fatalf("task state = %q, want it parked for a person", state.State)
	}
	if state.AttentionReason != string(integration.ReasonDependencyMissingFromTarget) {
		t.Fatalf("attention reason = %q, want %q", state.AttentionReason, integration.ReasonDependencyMissingFromTarget)
	}
	if got := f.refHead(t); got != f.baseSHA {
		t.Fatalf("the ref moved despite the refusal: %s", got)
	}
}

// The acceptance case for stale reviews. The dependency landed a change this
// task also makes, so replaying the task onto the current ref produces a
// different change from the one its reviewer approved. The prior approval is NOT
// reused: the task's own review and verify steps are re-opened for exactly one
// more cycle, its child run is running again, and nothing is parked.
func TestRebaseThatChangesTheContributionReopensTheReviewInsteadOfReusingIt(t *testing.T) {
	f := newDepTaskFixture(t,
		depTaskWork{"shared.txt", "alpha\n"},
		[]depTaskWork{{"shared.txt", "alpha\n"}, {"feature.txt", "beta\n"}})
	f.dependsOnSibling(t, "task-sibling")
	f.siblingIntegratedAt(t, "task-sibling", f.movedSHA)

	err := f.coord.promoteTaskToIntegration(f.ctx, f.master, f.task, f.child)
	if !errors.Is(err, errIntegrationFreshReview) {
		t.Fatalf("err = %v, want errIntegrationFreshReview", err)
	}
	if got := f.refHead(t); got != f.movedSHA {
		t.Fatalf("the ref moved on a stale approval: %s", got)
	}
	// Not a person's problem: the task keeps running and is not parked.
	if state := f.taskState(t); state.State != domain.WorkflowTaskRunning {
		t.Fatalf("task state = %q, want it still running while its review is redone", state.State)
	}
	if n := f.countPhase(t, f.master.ID, taskIntegrationConflictPhase); n != 0 {
		t.Fatalf("%d task conflicts recorded for a stale approval AO can answer itself", n)
	}

	// The decision is durable, and it is bounded by being counted.
	if n := f.countPhase(t, f.child.Run.ID, integrationFreshReviewRequiredPhase); n != 1 {
		t.Fatalf("integration_fresh_review_required checkpoints = %d, want exactly 1", n)
	}
	record, outstanding, rerr := f.coord.outstandingIntegrationFreshReview(f.ctx, f.child.Run.ID)
	if rerr != nil || !outstanding {
		t.Fatalf("outstanding fresh review = %v, %v", outstanding, rerr)
	}
	if record.ApprovedEffectiveChange == "" || record.CurrentEffectiveChange == "" ||
		record.ApprovedEffectiveChange == record.CurrentEffectiveChange {
		t.Fatalf("the record does not show the contribution changing: %+v", record)
	}
	// They are CHANGE identities, not commits. A record that quietly carried the
	// attention's SHAs instead would look right and mean nothing, so the two are
	// cross-checked against the integration ledger's own row for this attempt.
	attempts, lerr := f.coord.ListTaskIntegrations(f.ctx, f.master.ID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	var stale *TaskIntegrationRecord
	for i := range attempts {
		if attempts[i].TaskID == f.task.ID && attempts[i].AttentionReason == string(integration.ReasonStaleReviewAfterRebase) {
			stale = &attempts[i]
		}
	}
	if stale == nil {
		t.Fatal("the integration ledger has no row for the stale-approval attempt")
	}
	if stale.EffectiveBefore != record.ApprovedEffectiveChange || stale.EffectiveAfter != record.CurrentEffectiveChange {
		t.Fatalf("the fresh-review record and the ledger disagree about the change: %+v vs %+v", record, stale)
	}
	if stale.ReviewReused {
		t.Fatal("the ledger claims the stale approval was reused")
	}
	if got := laneGit(t, f.repo, "rev-parse", "ao/task-1"); got == stale.EffectiveBefore || got == stale.EffectiveAfter {
		t.Fatal("the recorded change identities are commit SHAs, not change identities")
	}
	// The dispatcher recognises a fresh review by the stale run's own target;
	// a record that named anything else would be silently ignored.
	if record.ApprovedFingerprint != f.approvedFingerprint {
		t.Fatalf("approved fingerprint = %q, want the stale review run's target", record.ApprovedFingerprint)
	}
	if record.PriorReviewRunID != f.reviewRunID {
		t.Fatalf("prior review run = %q, want %q", record.PriorReviewRunID, f.reviewRunID)
	}

	// The three state changes the next review cycle rests on.
	child, err := f.store.ListWorkflowSteps(f.ctx, f.child.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range child {
		switch step.Kind {
		case domain.WorkflowStepReview, domain.WorkflowStepVerify:
			if step.State != domain.WorkflowStepWaiting {
				t.Fatalf("%s step state = %q, want waiting", step.Kind, step.State)
			}
		}
	}
	run, ok, err := f.store.GetWorkflowRun(f.ctx, f.child.Run.ID)
	if err != nil || !ok {
		t.Fatalf("read child run: %v %v", ok, err)
	}
	if run.State != domain.WorkflowRunRunning {
		t.Fatalf("child run state = %q, want running again", run.State)
	}

	// And the review dispatcher sees the request, in its own vocabulary.
	pending, want := f.coord.pendingFreshReview(f.ctx, f.child.Run.ID, "wfs-review")
	if !want {
		t.Fatal("dispatchReviewStep would not see a fresh review it has to run")
	}
	if pending.ApprovedFingerprint != f.approvedFingerprint {
		t.Fatalf("pending fresh review = %+v", pending)
	}
}

// The bound. A target that keeps moving under one task faster than it can be
// reviewed is a person's problem, and the second re-review is the last one.
func TestIntegrationFreshReviewIsBounded(t *testing.T) {
	f := newDepTaskFixture(t,
		depTaskWork{"shared.txt", "alpha\n"},
		[]depTaskWork{{"shared.txt", "alpha\n"}, {"feature.txt", "beta\n"}})
	f.dependsOnSibling(t, "task-sibling")
	f.siblingIntegratedAt(t, "task-sibling", f.movedSHA)

	rec := integration.Record{
		TaskID:                     f.task.ID,
		Strategy:                   integration.StrategyRebaseFastForward,
		EffectiveFingerprintBefore: "change-a",
		EffectiveFingerprintAfter:  "change-b",
		Attention: &integration.Attention{
			Reason: integration.ReasonStaleReviewAfterRebase, TargetSHA: f.movedSHA,
			Strategy: integration.StrategyRebaseFastForward, Detail: "stale",
		},
	}
	workCP, _, err := f.store.GetLatestWorkflowCheckpointByStep(f.ctx, "wfs-work")
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= maxIntegrationFreshReviews; attempt++ {
		if rerr := f.coord.requestIntegrationFreshReview(f.ctx, f.master, f.task, f.child, workCP, rec); !errors.Is(rerr, errIntegrationFreshReview) {
			t.Fatalf("attempt %d: err = %v, want errIntegrationFreshReview", attempt, rerr)
		}
		// Put the child back where a finished cycle leaves it, so the next
		// request sees the same starting state a real one would.
		f.resetChildToCompleted(t)
	}
	if rerr := f.coord.requestIntegrationFreshReview(f.ctx, f.master, f.task, f.child, workCP, rec); !errors.Is(rerr, errIntegrationTaskConflict) {
		t.Fatalf("beyond the bound: err = %v, want the task parked for a person", rerr)
	}
	if state := f.taskState(t); !state.State.Parked() {
		t.Fatalf("task state = %q, want parked once the bound was reached", state.State)
	}
}

// resetChildToCompleted puts the child run and its review/verify steps back to
// the state a finished cycle leaves them in.
func (f *depTaskFixture) resetChildToCompleted(t *testing.T) {
	t.Helper()
	now := time.Now().UTC()
	for _, id := range []string{"wfs-review", "wfs-verify"} {
		if _, err := f.store.UpdateWorkflowStepState(f.ctx, id, domain.WorkflowStepWaiting, domain.WorkflowStepCompleted, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.UpdateWorkflowRunState(f.ctx, f.child.Run.ID, domain.WorkflowRunRunning, domain.WorkflowRunCompleted, now); err != nil {
		t.Fatal(err)
	}
}

// depReviewerLauncher exists so "a reviewer is available" is true. Nothing in
// these tests reaches a launch: the transition ends by resting the review step
// at `waiting`, and the dispatch that follows is review_dispatch.go's own,
// covered by its own tests.
type depReviewerLauncher struct{}

func (depReviewerLauncher) Preflight(stdctx.Context, domain.ReviewerHarness, string) error {
	return nil
}

// A launch must report an exact instance: a confirmation without one is
// refused, because nothing downstream could address it.
func (depReviewerLauncher) Launch(_ stdctx.Context, req ReviewerLaunchRequest) (ReviewerLaunchResult, error) {
	return ReviewerLaunchResult{
		HandleID:   "workflow-review-" + req.RunID,
		InstanceID: "inst-" + req.RunID,
	}, nil
}

// No external session is ever created here, so `absent` is the honest answer.
func (depReviewerLauncher) ReviewerIdentity(req ReviewerLaunchRequest) string {
	return "workflow-review-" + req.RunID
}

func (depReviewerLauncher) ProbeReviewer(stdctx.Context, ReviewerRef) (ReviewerObservation, error) {
	return ReviewerObservation{Presence: ReviewerPresenceAbsent}, nil
}

func (depReviewerLauncher) CancelReviewer(stdctx.Context, ReviewerRef) error { return nil }
