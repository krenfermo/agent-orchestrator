package workflow

import (
	stdctx "context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/integration"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// These cover the production ready-for-integration transition: what happens
// when reconcileMasterTasks promotes a task whose target was overtaken while it
// worked, which is the case parallel dispatch created and the case the
// Integration Coordinator exists for.

// laneStub is a real in-memory mutual exclusion over lane names, standing in
// for integration.NewBranchLocker (which the daemon wires in production and
// internal/integration tests against the real branch_locks table).
type laneStub struct {
	mu         sync.Mutex
	held       map[string]bool
	taken      []string
	alwaysBusy bool
}

func newLaneStub() *laneStub { return &laneStub{held: map[string]bool{}} }

func (l *laneStub) Acquire(_ stdctx.Context, req integration.LockRequest) (integration.LockHandle, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := req.RepoPath + "#" + req.TargetBranch
	if l.alwaysBusy || l.held[key] {
		return integration.LockHandle{}, fmt.Errorf("%w: %s", integration.ErrLockBusy, key)
	}
	l.held[key] = true
	l.taken = append(l.taken, key)
	return integration.LockHandle{ID: "lane-1", LockKey: key}, nil
}

func (l *laneStub) Release(_ stdctx.Context, handle integration.LockHandle, _ string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.held, handle.LockKey)
	return nil
}

func (l *laneStub) lanes() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.taken...)
}

func laneGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANGUAGE=", "GIT_EDITOR=true")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// overtakenFixture is a master run whose integration ref has already advanced
// past the commit this task's worktree was cut from -- exactly what a sibling
// task landing first produces.
type overtakenFixture struct {
	store      *sqlite.Store
	coord      *Coordinator
	lanes      *laneStub
	mat        *fakeMaterializer
	ctx        stdctx.Context
	repo       string
	worktree   string
	refName    string
	baseSHA    string
	movedSHA   string
	master     domain.WorkflowRun
	task       domain.WorkflowTask
	child      RunDetail
	sessID     string
	workStepID string
	runner     *laneVerifyRunner
}

// laneVerifyRunner is the VerifyRunner the re-verification runs through, so the
// test can see that the task's own checks really were executed against the
// replayed worktree rather than assumed to still pass.
type laneVerifyRunner struct {
	mu       sync.Mutex
	calls    []VerifyCommandRequest
	exitCode int
}

func (r *laneVerifyRunner) Run(_ stdctx.Context, req VerifyCommandRequest) (VerifyCommandExecution, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, req)
	return VerifyCommandExecution{ExitCode: r.exitCode}, nil
}

func (r *laneVerifyRunner) ran() []VerifyCommandRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]VerifyCommandRequest(nil), r.calls...)
}

func newOvertakenFixture(t *testing.T, reviewState domain.WorkflowStepState, verifyState domain.WorkflowStepState) *overtakenFixture {
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
	master := domain.WorkflowRun{ID: "wf-master", ProjectID: "p", Objective: "master", State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}", CreatedAt: now, UpdatedAt: now}
	if _, _, err := store.CreateWorkflowRun(ctx, master, nil); err != nil {
		t.Fatal(err)
	}
	refName := masterIntegrationRefName(master.ID)

	// A sibling task landed first: the integration ref exists and points past
	// the commit this task's worktree was cut from.
	laneGit(t, repo, "checkout", "-q", "-b", "sibling", baseSHA)
	if err := os.WriteFile(filepath.Join(repo, "sibling.txt"), []byte("sibling work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	laneGit(t, repo, "add", "sibling.txt")
	laneGit(t, repo, "commit", "-m", "sibling task")
	movedSHA := laneGit(t, repo, "rev-parse", "HEAD")
	laneGit(t, repo, "update-ref", refName, movedSHA)
	laneGit(t, repo, "checkout", "-q", "main")
	if _, err := store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-sibling", WorkflowRunID: master.ID, ProjectID: "p",
		BaseSHA: baseSHA, HeadSHA: movedSHA,
		RetryState:   `{"taskId":"task-sibling","refName":"` + refName + `"}`,
		DurablePhase: masterIntegrationDurablePhase, PayloadVersion: masterIntegrationPayloadVersion, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// This task's own worktree, cut from the OLD base, with its work committed.
	worktree := filepath.Join(root, "worktrees", "task-1")
	laneGit(t, repo, "worktree", "add", "-q", "-b", "ao/task-1", worktree, baseSHA)
	if err := os.WriteFile(filepath.Join(worktree, "task.txt"), []byte("task work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	laneGit(t, worktree, "add", "task.txt")
	laneGit(t, worktree, "commit", "-m", "task 1")

	childID := "wf-exec-task-1"
	taskID := "task-1"
	// The task's own verification plan: one command, which is exactly what the
	// re-verification after a replay has to run again.
	artifactJSON, err := MarshalPlanArtifact(BuildPlanArtifact("p", "task", policyVersionV1, VerificationPlan{
		Commands: []VerificationCommandCheck{{Command: "go", Args: []string{"test", "./..."}, RequiredExitCode: 0, RetrySafe: true}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	steps := []domain.WorkflowStep{
		{ID: "wfs-plan", WorkflowRunID: childID, Kind: domain.WorkflowStepPlan, Ordinal: 0, State: domain.WorkflowStepCompleted, ArtifactJSON: artifactJSON, CreatedAt: now, UpdatedAt: now},
		{ID: "wfs-work", WorkflowRunID: childID, Kind: domain.WorkflowStepWork, Ordinal: 1, State: domain.WorkflowStepCompleted, ArtifactJSON: "{}", CreatedAt: now, UpdatedAt: now},
		{ID: "wfs-review", WorkflowRunID: childID, Kind: domain.WorkflowStepReview, Ordinal: 2, State: reviewState, ArtifactJSON: "{}", CreatedAt: now, UpdatedAt: now},
		{ID: "wfs-verify", WorkflowRunID: childID, Kind: domain.WorkflowStepVerify, Ordinal: 3, State: verifyState, ArtifactJSON: "{}", CreatedAt: now, UpdatedAt: now},
	}
	childRun := domain.WorkflowRun{ID: childID, ProjectID: "p", Objective: "task", State: domain.WorkflowRunCompleted, PolicyVersion: policyVersionV1, PolicySnapshot: "{}", CreatedAt: now, UpdatedAt: now, ParentWorkflowID: &master.ID, PlannedTaskID: &taskID}
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
		// The commit this worktree was cut from: now behind the ref's head.
		BaseSHA:      baseSHA,
		DurablePhase: "work_completed", PayloadVersion: "v1", RetryState: "{}", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	detail := RunDetail{Run: createdRun, Steps: []StepDetail{}}
	for _, s := range createdSteps {
		sd := StepDetail{Step: s}
		if s.Kind == domain.WorkflowStepReview && s.State == domain.WorkflowStepCompleted {
			sd.Review = &ReviewSummary{Verdict: domain.VerdictApproved}
		}
		if s.Kind == domain.WorkflowStepReview && s.State == domain.WorkflowStepFailed {
			sd.Review = &ReviewSummary{Verdict: domain.VerdictChangesRequested}
		}
		detail.Steps = append(detail.Steps, sd)
	}

	lanes := newLaneStub()
	mat := &fakeMaterializer{commitSHA: "should-not-be-used"}
	runner := &laneVerifyRunner{}
	coord := New(Deps{
		Store: store, Projects: store, WorkspaceFacts: mat, IntegrationLocks: lanes,
		Verifier: runner,
		Clock:    func() time.Time { return time.Now().UTC() },
	})
	return &overtakenFixture{
		store: store, coord: coord, lanes: lanes, mat: mat, ctx: ctx,
		repo: repo, worktree: worktree, refName: refName,
		baseSHA: baseSHA, movedSHA: movedSHA,
		master: master, task: seedPlanTask(t, ctx, store, master.ID, taskID, 1, domain.WorkflowTaskRunning, nil),
		child: detail, sessID: sessID, workStepID: workStepID, runner: runner,
	}
}

// The wiring itself: a task whose base was overtaken is integrated by the
// Coordinator -- replayed onto the ref's real head, then landed -- instead of
// having its worktree content materialized over the sibling that got there
// first.
func TestOvertakenTaskIsIntegratedThroughTheCoordinator(t *testing.T) {
	f := newOvertakenFixture(t, domain.WorkflowStepCompleted, domain.WorkflowStepCompleted)

	if err := f.coord.promoteTaskToIntegration(f.ctx, f.master, f.task, f.child); err != nil {
		t.Fatalf("promote: %v", err)
	}

	// The lane was entered, keyed on the ref (an AO ref keeps its full name).
	if got := f.lanes.lanes(); len(got) != 1 || got[0] != f.repo+"#"+f.refName {
		t.Fatalf("lanes taken = %v", got)
	}
	// The old content-materialization path was NOT used: using it here is
	// exactly the bug -- this task's tree has no sibling.txt in it.
	if f.mat.calls != 0 {
		t.Fatalf("the materializer ran %d times for an overtaken task", f.mat.calls)
	}
	// The ref moved, and it still contains the sibling's work as well as ours.
	head := laneGit(t, f.repo, "rev-parse", f.refName)
	if head == f.movedSHA {
		t.Fatal("the integration ref did not move")
	}
	for _, name := range []string{"sibling.txt", "task.txt"} {
		if laneGit(t, f.repo, "cat-file", "-e", head+":"+name) != "" {
			t.Fatalf("%s is missing from the integrated ref", name)
		}
	}

	// The integration is durably recorded, with the strategy and all three SHAs.
	records, err := f.coord.ListTaskIntegrations(f.ctx, f.master.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("ledger has %d rows, want an intent and a result: %+v", len(records), records)
	}
	if records[0].Outcome != string(integration.OutcomeAttempting) || records[1].Outcome != string(integration.OutcomeIntegrated) {
		t.Fatalf("ledger outcomes = %q, %q", records[0].Outcome, records[1].Outcome)
	}
	landed := records[1]
	if landed.Strategy != string(integration.StrategyRebaseFastForward) {
		t.Fatalf("strategy = %q", landed.Strategy)
	}
	if landed.TargetBeforeSHA != f.movedSHA || landed.TargetAfterSHA != head || landed.BaseSHA != f.baseSHA {
		t.Fatalf("recorded SHAs = %+v (want before %s, after %s, base %s)", landed, f.movedSHA, head, f.baseSHA)
	}
	if !landed.Replayed || !landed.VerificationRan || !landed.VerificationOK {
		t.Fatalf("the record does not show a replay that was re-verified: %+v", landed)
	}
	// The re-verification went through the run's OWN verify infrastructure,
	// running the task's own command in the task's own worktree.
	ran := f.runner.ran()
	// Compared through EvalSymlinks because secureWorktreePath resolves the
	// root before running anything — on macOS a t.TempDir() under /var is
	// really /private/var, so a raw string comparison asserts the platform's
	// symlink layout rather than the directory the command ran in.
	wantDir, err := filepath.EvalSymlinks(f.worktree)
	if err != nil {
		t.Fatalf("resolve worktree: %v", err)
	}
	if len(ran) != 1 || ran[0].Command != "go" || ran[0].Directory != wantDir {
		t.Fatalf("verification commands run = %+v, want the task's own command in %s", ran, wantDir)
	}

	// And the master's own integration state advanced to the new head, so the
	// next task is based on it exactly as in the serial path.
	state, err := f.coord.getMasterIntegrationState(f.ctx, f.master.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.CurrentSHA != head {
		t.Fatalf("master integration state = %s, want %s", state.CurrentSHA, head)
	}
}

// The gate, on the production path: a task whose review did not approve cannot
// reach the target, and the refusal is recorded rather than silent.
func TestOvertakenTaskWithAFailedGateNeverReachesTheTarget(t *testing.T) {
	tests := []struct {
		name           string
		review, verify domain.WorkflowStepState
	}{
		{"review requested changes", domain.WorkflowStepFailed, domain.WorkflowStepCompleted},
		{"review never finished", domain.WorkflowStepRunning, domain.WorkflowStepCompleted},
		{"verification failed", domain.WorkflowStepCompleted, domain.WorkflowStepFailed},
		{"verification never finished", domain.WorkflowStepCompleted, domain.WorkflowStepPending},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newOvertakenFixture(t, tc.review, tc.verify)
			before := laneGit(t, f.repo, "rev-parse", f.refName)

			err := f.coord.promoteTaskToIntegration(f.ctx, f.master, f.task, f.child)
			if err == nil {
				t.Fatal("a task that failed its gate was promoted")
			}
			if !errors.Is(err, errIntegrationFailed) {
				t.Fatalf("err = %v, want an integration failure", err)
			}
			if got := laneGit(t, f.repo, "rev-parse", f.refName); got != before {
				t.Fatalf("the target moved to %s despite a failed gate", got)
			}
			// No integration was recorded, because none was attempted: the gate
			// refuses before the lane is entered.
			records, rerr := f.coord.ListTaskIntegrations(f.ctx, f.master.ID)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if len(records) != 0 {
				t.Fatalf("an ungated task produced integration records: %+v", records)
			}
			// The refusal itself is on the master's failure ledger.
			state, serr := f.coord.getMasterIntegrationState(f.ctx, f.master.ID)
			if serr != nil {
				t.Fatal(serr)
			}
			if !strings.Contains(state.LastErrorReason, "integration_not_ready") {
				t.Fatalf("recorded reason = %q", state.LastErrorReason)
			}
		})
	}
}

// A busy lane is the single-lane property working, not a failure: the task
// stays where it is and the next reconcile pass retries. It must never park the
// master run for a human.
func TestBusyIntegrationLaneRetriesInsteadOfStoppingTheRun(t *testing.T) {
	f := newOvertakenFixture(t, domain.WorkflowStepCompleted, domain.WorkflowStepCompleted)
	f.lanes.alwaysBusy = true
	before := laneGit(t, f.repo, "rev-parse", f.refName)

	err := f.coord.promoteTaskToIntegration(f.ctx, f.master, f.task, f.child)
	if !errors.Is(err, errIntegrationBusy) {
		t.Fatalf("err = %v, want errIntegrationBusy", err)
	}
	if errors.Is(err, errIntegrationFailed) {
		t.Fatal("a busy lane was reported as an integration failure")
	}
	if got := laneGit(t, f.repo, "rev-parse", f.refName); got != before {
		t.Fatalf("the target moved to %s while the lane was busy", got)
	}
	// Nothing durable was written: a busy lane is not an event, it is a retry.
	state, err := f.coord.getMasterIntegrationState(f.ctx, f.master.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastErrorReason != "" {
		t.Fatalf("a busy lane recorded a failure: %q", state.LastErrorReason)
	}
}

// A task whose base still matches the ref's head keeps the pre-existing
// promotion exactly: the split is by that condition alone, so nothing about
// serial execution changes.
// Task 5's authoritative rule: EVERY task that reaches ready_for_integration
// goes through the Integration Coordinator — including the one whose base is
// still the target's head, which used to take a second promotion route with no
// lane, no gate and no audit record.
//
// This test replaces TestTaskWhoseBaseIsCurrentStillUsesTheExistingPromotion,
// which asserted exactly that legacy route. "The target did not move" is not a
// shortcut past the coordinator; it is the fast-forward strategy, decided from
// a head read under the lock rather than from what the dispatcher remembered.
func TestTaskWhoseBaseIsCurrentStillGoesThroughTheCoordinator(t *testing.T) {
	f := newOvertakenFixture(t, domain.WorkflowStepCompleted, domain.WorkflowStepCompleted)
	// Put the target back where this task's branch was cut from, so the target
	// genuinely has NOT moved: the source contains it, and a fast-forward is
	// the honest strategy. (The fixture's default is the overtaken case.)
	laneGit(t, f.repo, "update-ref", f.refName, f.baseSHA)
	now := time.Now().UTC()
	sessID := f.sessID
	if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
		ID: "wfc-work-2", WorkflowRunID: f.child.Run.ID, WorkflowStepID: &f.workStepID,
		ProjectID: "p", SessionID: &sessID, Branch: "ao/task-1", WorktreePath: f.worktree,
		BaseSHA: f.baseSHA, DurablePhase: "work_completed", PayloadVersion: "v1", RetryState: "{}", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	if err := f.coord.promoteTaskToIntegration(f.ctx, f.master, f.task, f.child); err != nil {
		t.Fatalf("promote: %v", err)
	}

	// The lane was taken, which is the guarantee the legacy route skipped.
	if lanes := f.lanes.lanes(); len(lanes) != 1 {
		t.Fatalf("integration lanes taken = %v, want exactly one", lanes)
	}
	// The materializer — the legacy promotion's only mechanism — was not used.
	if f.mat.calls != 0 {
		t.Fatalf("materializer ran %d times; there is no second promotion route any more", f.mat.calls)
	}
	// And the audit record exists, naming the strategy and bracketing the ref.
	records, err := f.coord.ListTaskIntegrations(f.ctx, f.master.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 {
		t.Fatal("the promotion recorded no integration audit row at all")
	}
	landed := records[len(records)-1]
	if landed.Outcome != string(integration.OutcomeIntegrated) {
		t.Fatalf("final outcome = %q, want integrated", landed.Outcome)
	}
	if landed.Strategy != string(integration.StrategyFastForward) {
		t.Fatalf("strategy = %q, want fast_forward for an unmoved target", landed.Strategy)
	}
	if landed.TargetBeforeSHA == "" || landed.TargetAfterSHA == "" {
		t.Fatalf("the record does not bracket the ref update: %+v", landed)
	}
	if landed.Replayed {
		t.Fatalf("an unmoved target was replayed: %+v", landed)
	}
}
func TestTaskBaseWasOvertaken(t *testing.T) {
	c := &Coordinator{}
	tests := []struct {
		name          string
		base, current string
		want          bool
	}{
		{"the ref moved past this task's base", "aaa", "bbb", true},
		{"the task was cut from the ref's head", "aaa", "aaa", false},
		{"nothing has been integrated yet", "aaa", "", false},
		{"the worktree's base could not be observed", "", "bbb", false},
		{"neither is known", "", "", false},
		{"whitespace is not a SHA", "  ", "bbb", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.taskBaseWasOvertaken(tc.base, tc.current); got != tc.want {
				t.Fatalf("taskBaseWasOvertaken(%q, %q) = %v, want %v", tc.base, tc.current, got, tc.want)
			}
		})
	}
}

// Work that passed against the base it was written on and fails against the
// target it is being replayed onto must not land. This is the reason the
// re-verification exists at all, on the production path.
func TestReplayedWorkThatFailsReverificationDoesNotLand(t *testing.T) {
	f := newOvertakenFixture(t, domain.WorkflowStepCompleted, domain.WorkflowStepCompleted)
	f.runner.exitCode = 1 // the task's own command now fails on the moved target
	before := laneGit(t, f.repo, "rev-parse", f.refName)

	err := f.coord.promoteTaskToIntegration(f.ctx, f.master, f.task, f.child)
	if err == nil {
		t.Fatal("work that failed re-verification was promoted")
	}
	if got := laneGit(t, f.repo, "rev-parse", f.refName); got != before {
		t.Fatalf("the target moved to %s despite a failed re-verification", got)
	}
	records, rerr := f.coord.ListTaskIntegrations(f.ctx, f.master.ID)
	if rerr != nil {
		t.Fatal(rerr)
	}
	// One row, and it stops for a person: the intent row is only written once
	// the ref is actually about to move, which never happened here.
	if len(records) != 1 || records[0].Outcome != string(integration.OutcomeNeedsAttention) {
		t.Fatalf("ledger = %+v, want a single needs-attention row", records)
	}
	if records[0].AttentionReason != string(integration.ReasonVerificationFailed) {
		t.Fatalf("attention reason = %q", records[0].AttentionReason)
	}
	// The stop is recorded against the TASK, not the objective, and names
	// everything a person needs to act on it. The master is deliberately NOT
	// carrying this error any more: one task's conflict must not park an
	// objective whose other tasks can still move.
	conflict := f.taskConflictRecord(t)
	for _, want := range []string{"base ", "target ", "source "} {
		if !strings.Contains(conflict.NextAction, want) {
			t.Fatalf("task conflict record %q does not name %q", conflict.NextAction, want)
		}
	}
	for _, want := range []string{`"strategyAttempted"`, `"recommendedAction"`, `"sourceSha"`, `"targetBeforeSha"`, `"baseSha"`} {
		if !strings.Contains(conflict.RetryState, want) {
			t.Fatalf("task conflict payload is missing %s: %s", want, conflict.RetryState)
		}
	}
	state, serr := f.coord.getMasterIntegrationState(f.ctx, f.master.ID)
	if serr != nil {
		t.Fatal(serr)
	}
	if state.LastErrorReason != "" {
		t.Fatalf("the objective recorded a task's conflict as its own failure: %q", state.LastErrorReason)
	}
}

// The TOCTOU the reviewer found, as a test.
//
// The old routing decided "has this task's base been overtaken?" from CACHED
// master state, BEFORE any lock. A target that moved between that decision and
// the promotion was integrated against a head nobody had read: the fast-forward
// route called MaterializeIntegrationCommit parented on the stale head and
// could silently revert whatever had landed in between.
//
// Here the master's cached state says the target is exactly where this task was
// cut from — the condition that used to select the unlocked route — and the ref
// has ACTUALLY moved on since. The Coordinator must notice, because it reads
// the head inside the lane rather than trusting what it was handed, and must
// therefore replay and re-verify instead of fast-forwarding onto a commit that
// is no longer there.
func TestTargetThatMovesAfterReadinessIsSeenInsideTheLane(t *testing.T) {
	f := newOvertakenFixture(t, domain.WorkflowStepCompleted, domain.WorkflowStepCompleted)

	// The cached view every caller had: base == the target's head. Under the
	// old routing this alone selected the unlocked, unaudited promotion.
	staleHead := f.baseSHA
	now := time.Now().UTC()
	sessID := f.sessID
	if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
		ID: "wfc-work-toctou", WorkflowRunID: f.child.Run.ID, WorkflowStepID: &f.workStepID,
		ProjectID: "p", SessionID: &sessID, Branch: "ao/task-1", WorktreePath: f.worktree,
		BaseSHA: staleHead, DurablePhase: "work_completed", PayloadVersion: "v1", RetryState: "{}", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// …and the truth on disk: a sibling landed after that view was taken. The
	// fixture already moved the ref to movedSHA, so the cached answer is stale
	// exactly the way a real race makes it stale.
	if got := laneGit(t, f.repo, "rev-parse", f.refName); got != f.movedSHA {
		t.Fatalf("fixture precondition: ref is at %s, want the moved head %s", got, f.movedSHA)
	}

	if err := f.coord.promoteTaskToIntegration(f.ctx, f.master, f.task, f.child); err != nil {
		t.Fatalf("promote: %v", err)
	}

	records, err := f.coord.ListTaskIntegrations(f.ctx, f.master.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 {
		t.Fatal("nothing was recorded: the promotion took a route with no audit")
	}
	landed := records[len(records)-1]

	// The head the coordinator acted on is the REAL one, not the cached one.
	if landed.TargetBeforeSHA != f.movedSHA {
		t.Fatalf("target-before = %s, want the head as it actually was (%s); a stale head was used",
			landed.TargetBeforeSHA, f.movedSHA)
	}
	if landed.TargetBeforeSHA == staleHead {
		t.Fatal("the coordinator integrated against the cached head, not the one it read under the lane")
	}
	// Having seen the move, it replayed and re-verified rather than
	// fast-forwarding onto a commit that was no longer the head.
	if !landed.Replayed {
		t.Fatalf("a moved target was not replayed: %+v", landed)
	}
	if !landed.VerificationRan || !landed.VerificationOK {
		t.Fatalf("the replayed work was not re-verified before landing: %+v", landed)
	}
	// And it did all of it holding the lane.
	if lanes := f.lanes.lanes(); len(lanes) != 1 {
		t.Fatalf("integration lanes taken = %v, want exactly one", lanes)
	}
	if f.mat.calls != 0 {
		t.Fatalf("the legacy materializer ran %d times; that route is gone", f.mat.calls)
	}
}

// newInternalTestRepo creates the real repository the internal integration
// fixtures need, using the same shape newOvertakenFixture already relies on.
//
// It is real git because the behaviour under test depends on git: which commit
// a ref names, whether one commit contains another, and whether a
// compare-and-set still sees the value it read.
func newInternalTestRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := filepath.Join(t.TempDir(), "repo")
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
	return repo
}

// taskConflictRecord reads the task-scoped conflict row.
func (f *overtakenFixture) taskConflictRecord(t *testing.T) domain.WorkflowCheckpoint {
	t.Helper()
	cps, err := f.store.ListWorkflowCheckpoints(f.ctx, f.master.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, cp := range cps {
		if cp.DurablePhase == taskIntegrationConflictPhase {
			return cp
		}
	}
	t.Fatal("no task_integration_conflict record was written")
	return domain.WorkflowCheckpoint{}
}
