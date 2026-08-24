package workflow

// One more fresh review, when a person says so
// (task_integration_fresh_review_exception.go).
//
// This widens a guard, so most of these tests are about what it REFUSES. The
// property that matters most is that it is bounded: one grant, for one task,
// for one workspace state, and a repeat request is the same decision arriving
// twice rather than a second decision.

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/integration"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type exceptionFixture struct {
	t       *testing.T
	coord   *Coordinator
	store   *sqlite.Store
	ctx     stdctx.Context
	ws      *exceptionWorkspace
	masterD domain.WorkflowRun
	taskID  string
	childID string
}

// exceptionWorkspace lets a test move the observed fingerprint, which is the
// key the grant is bounded by.
type exceptionWorkspace struct{ obs ports.WorkspaceObservation }

func (w *exceptionWorkspace) ObserveWorkspace(_ stdctx.Context, _ ports.WorkspaceInfo) (ports.WorkspaceObservation, error) {
	return w.obs, nil
}

// newExceptionFixture reaches the exact dead end this mechanism exists for: a
// task parked on integration_stale_review_after_rebase with its ordinary
// fresh-review budget fully spent.
func newExceptionFixture(t *testing.T, attemptsUsed int) *exceptionFixture {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	now := time.Now().UTC()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	master := domain.WorkflowRun{ID: "wf-exc-master", ProjectID: "p", Objective: "objective",
		State: domain.WorkflowRunNeedsAttention, PolicyVersion: policyVersionV1, PolicySnapshot: "{}",
		CreatedAt: now, UpdatedAt: now}
	if _, _, err := store.CreateWorkflowRun(ctx, master, nil); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal([]string{"it works"})
	task := domain.WorkflowTask{
		ID: "task-exc", WorkflowRunID: master.ID, PlanStepID: "step-exc", Ordinal: 8,
		Title: "Planner surface", Description: "does a thing",
		AcceptanceCriteriaJSON: string(raw), VerifyJSON: "{}", ScopeJSON: "{}",
		State: domain.WorkflowTaskEligible, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.InsertWorkflowTasks(ctx, []domain.WorkflowTask{task}); err != nil {
		t.Fatal(err)
	}
	childID := "wf-exc-child"
	sid := "sess-exc"
	_ = sid
	steps := []domain.WorkflowStep{
		{ID: "wfs-work", WorkflowRunID: childID, Kind: domain.WorkflowStepWork, Ordinal: 1, State: domain.WorkflowStepCompleted, ArtifactJSON: "{}", SessionID: &sid, CreatedAt: now, UpdatedAt: now},
		{ID: "wfs-review", WorkflowRunID: childID, Kind: domain.WorkflowStepReview, Ordinal: 2, State: domain.WorkflowStepCompleted, ArtifactJSON: "{}", CreatedAt: now, UpdatedAt: now},
	}
	childRun := domain.WorkflowRun{ID: childID, ProjectID: "p", Objective: "objective",
		State: domain.WorkflowRunCompleted, PolicyVersion: policyVersionV1, PolicySnapshot: "{}",
		CreatedAt: now, UpdatedAt: now, ParentWorkflowID: &master.ID, PlannedTaskID: &task.ID}
	if _, _, err := store.CreateWorkflowRun(ctx, childRun, steps); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.SetWorkflowTaskExecutionRun(ctx, task.ID, childID, now); err != nil || !ok {
		t.Fatalf("SetWorkflowTaskExecutionRun: ok=%v err=%v", ok, err)
	}
	// The work checkpoint the fingerprint is observed against.
	workStepID := "wfs-work"
	if _, err := store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-work", WorkflowRunID: childID, WorkflowStepID: &workStepID, ProjectID: "p",
		Branch: "main", WorktreePath: t.TempDir(),
		RetryState: "{}", DurablePhase: "worker_observed", PayloadVersion: "v1", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// The fresh reviews already spent.
	for i := 1; i <= attemptsUsed; i++ {
		payload, _ := json.Marshal(IntegrationFreshReviewRecord{TaskID: task.ID, Attempt: i})
		if _, err := store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
			ID: "wfc-req-" + string(rune('a'+i)), WorkflowRunID: childID, ProjectID: "p",
			RetryState: string(payload), DurablePhase: integrationFreshReviewRequiredPhase,
			PayloadVersion: "v1", CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Parked, on the one reason this mechanism answers.
	if _, err := store.ParkWorkflowTaskForAttention(ctx, task.ID, domain.WorkflowTaskRunning,
		string(integration.ReasonStaleReviewAfterRebase), domain.WorkflowTaskAttention{Attempt: 2}, now); err != nil {
		t.Fatal(err)
	}

	ws := &exceptionWorkspace{obs: ports.WorkspaceObservation{Path: "/tmp/x", Branch: "main", HeadSHA: "aaaa1111"}}
	coord := New(Deps{Store: store, Projects: store, WorkspaceFacts: ws,
		Clock: func() time.Time { return time.Now().UTC() }})
	return &exceptionFixture{t: t, coord: coord, store: store, ctx: ctx, ws: ws,
		masterD: master, taskID: task.ID, childID: childID}
}

func (fx *exceptionFixture) authorize(by, reason string) (IntegrationFreshReviewException, error) {
	fx.t.Helper()
	return fx.coord.AuthorizeIntegrationFreshReviewException(fx.ctx, IntegrationFreshReviewExceptionRequest{
		MasterRunID: "wf-exc-master", TaskID: fx.taskID, ApprovedBy: by, Reason: reason,
	})
}

func (fx *exceptionFixture) grants() []IntegrationFreshReviewException {
	fx.t.Helper()
	got, err := fx.coord.integrationFreshReviewExceptions(fx.ctx, fx.childID)
	if err != nil {
		fx.t.Fatal(err)
	}
	return got
}

// ---- 1. the grant, and everything it records ------------------------------

func TestExceptionalFreshReviewIsGrantedAndFullyAudited(t *testing.T) {
	fx := newExceptionFixture(t, maxIntegrationFreshReviews)

	got, err := fx.authorize("joaquin (repository owner)", "the branch was stabilised; the work is unchanged and needs one review against it")
	if err != nil {
		t.Fatalf("AuthorizeIntegrationFreshReviewException: %v", err)
	}

	if got.ApprovedBy == "" || got.Reason == "" {
		t.Fatal("the grant lost its approver or reason")
	}
	if got.TaskID != fx.taskID || got.MasterRunID != "wf-exc-master" || got.ChildRunID != fx.childID {
		t.Fatalf("the grant does not identify the task/run it belongs to: %+v", got)
	}
	if got.Fingerprint == "" {
		t.Fatal("the grant records no workspace state, so it could not be bounded to one")
	}
	if got.PriorAttempts != maxIntegrationFreshReviews {
		t.Fatalf("priorAttempts = %d, want %d: the record must say what was exhausted", got.PriorAttempts, maxIntegrationFreshReviews)
	}
	if got.Generation != 1 {
		t.Fatalf("generation = %d, want 1", got.Generation)
	}
	if got.GrantedAt.IsZero() {
		t.Fatal("the grant has no timestamp")
	}
	// And the budget it widens is exactly one wider, for this task only.
	if b := fx.coord.integrationFreshReviewBudget(fx.ctx, fx.childID); b != maxIntegrationFreshReviews+1 {
		t.Fatalf("budget = %d, want %d", b, maxIntegrationFreshReviews+1)
	}
	if maxIntegrationFreshReviews != 2 {
		t.Fatal("the global bound was changed; it must stay exactly where it was")
	}
}

// The previous requests are evidence and must survive untouched.
func TestExceptionKeepsEveryPreviousCheckpoint(t *testing.T) {
	fx := newExceptionFixture(t, maxIntegrationFreshReviews)
	before, err := fx.coord.integrationFreshReviewAttempts(fx.ctx, fx.childID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.authorize("joaquin", "reason"); err != nil {
		t.Fatal(err)
	}
	after, err := fx.coord.integrationFreshReviewAttempts(fx.ctx, fx.childID)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("prior fresh-review requests changed from %d to %d — the grant must add, never rewrite", before, after)
	}
}

// ---- 2. bounded: one grant per workspace state ----------------------------

func TestRepeatedAuthorizationForTheSameWorkspaceGrantsNothingNew(t *testing.T) {
	fx := newExceptionFixture(t, maxIntegrationFreshReviews)
	first, err := fx.authorize("joaquin", "stabilised the branch")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		again, err := fx.authorize("joaquin", "stabilised the branch")
		if err != nil {
			t.Fatalf("a repeat request should be idempotent, not an error: %v", err)
		}
		if again.Generation != first.Generation {
			t.Fatalf("generation moved to %d on repeat: a poll or a double-click is not a second decision", again.Generation)
		}
	}
	if n := len(fx.grants()); n != 1 {
		t.Fatalf("grants = %d after 11 requests, want exactly 1", n)
	}
	if b := fx.coord.integrationFreshReviewBudget(fx.ctx, fx.childID); b != maxIntegrationFreshReviews+1 {
		t.Fatalf("budget = %d, want %d: repeats must not accumulate headroom", b, maxIntegrationFreshReviews+1)
	}
}

// A genuinely different workspace is a different decision, and may be granted
// again — that is the "salvo nueva decisión humana explícita" half.
func TestANewWorkspaceStateMayBeAuthorizedAgain(t *testing.T) {
	fx := newExceptionFixture(t, maxIntegrationFreshReviews)
	if _, err := fx.authorize("joaquin", "first"); err != nil {
		t.Fatal(err)
	}
	// The tree genuinely moved, and the budget is spent again.
	fx.ws.obs.HeadSHA = "bbbb2222"
	payload, _ := json.Marshal(IntegrationFreshReviewRecord{TaskID: fx.taskID, Attempt: 3})
	if _, err := fx.store.CreateWorkflowCheckpoint(fx.ctx, domain.WorkflowCheckpoint{
		ID: "wfc-req-3", WorkflowRunID: fx.childID, ProjectID: "p",
		RetryState: string(payload), DurablePhase: integrationFreshReviewRequiredPhase,
		PayloadVersion: "v1", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	second, err := fx.authorize("joaquin", "the branch moved again and was re-stabilised")
	if err != nil {
		t.Fatalf("a new workspace state should be authorizable: %v", err)
	}
	if second.Generation != 2 {
		t.Fatalf("generation = %d, want 2", second.Generation)
	}
	if n := len(fx.grants()); n != 2 {
		t.Fatalf("grants = %d, want 2", n)
	}
}

// ---- 3. the refusals ------------------------------------------------------

func TestExceptionRefusesWithoutAnApproverOrReason(t *testing.T) {
	fx := newExceptionFixture(t, maxIntegrationFreshReviews)
	for _, tc := range []struct{ by, reason string }{
		{"", "a reason"},
		{"joaquin", ""},
		{"   ", "a reason"},
	} {
		if _, err := fx.authorize(tc.by, tc.reason); !errors.Is(err, ErrInvalid) {
			t.Fatalf("authorize(%q,%q) = %v, want ErrInvalid: an unattributable widening is not an authorization", tc.by, tc.reason, err)
		}
	}
	if n := len(fx.grants()); n != 0 {
		t.Fatalf("grants = %d, want 0", n)
	}
}

// While the ordinary budget still has room, the ordinary path is the answer.
func TestExceptionRefusesWhileTheOrdinaryBudgetRemains(t *testing.T) {
	fx := newExceptionFixture(t, maxIntegrationFreshReviews-1)
	_, err := fx.authorize("joaquin", "just in case")
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid: headroom nobody needs is headroom everybody has to reason about later", err)
	}
	if !strings.Contains(err.Error(), "not exhausted") {
		t.Fatalf("the refusal does not explain itself: %v", err)
	}
}

// A task parked for a different reason has a different remedy.
func TestExceptionRefusesATaskParkedForAnotherReason(t *testing.T) {
	fx := newExceptionFixture(t, maxIntegrationFreshReviews)
	if _, err := fx.store.ResumeWorkflowTaskFromAttention(fx.ctx, fx.taskID, domain.WorkflowTaskRunning, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.store.ParkWorkflowTaskForAttention(fx.ctx, fx.taskID, domain.WorkflowTaskRunning,
		string(integration.ReasonVerificationFailed), domain.WorkflowTaskAttention{}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.authorize("joaquin", "please"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid: one more review does not answer a failing verification", err)
	}
}

// A task that is not parked has nothing to unblock.
func TestExceptionRefusesATaskThatIsNotParked(t *testing.T) {
	fx := newExceptionFixture(t, maxIntegrationFreshReviews)
	if _, err := fx.store.ResumeWorkflowTaskFromAttention(fx.ctx, fx.taskID, domain.WorkflowTaskRunning, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.authorize("joaquin", "please"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

// ---- 4. restart-safe, because it is derived -------------------------------

func TestGrantsSurviveARestartAndAreNotDuplicated(t *testing.T) {
	fx := newExceptionFixture(t, maxIntegrationFreshReviews)
	if _, err := fx.authorize("joaquin", "stabilised"); err != nil {
		t.Fatal(err)
	}
	// A second Coordinator over the same durable store IS a daemon restart.
	fx.coord = New(Deps{Store: fx.store, Projects: fx.store, WorkspaceFacts: fx.ws,
		Clock: func() time.Time { return time.Now().UTC() }})

	if b := fx.coord.integrationFreshReviewBudget(fx.ctx, fx.childID); b != maxIntegrationFreshReviews+1 {
		t.Fatalf("budget after restart = %d, want %d", b, maxIntegrationFreshReviews+1)
	}
	if _, err := fx.authorize("joaquin", "stabilised"); err != nil {
		t.Fatal(err)
	}
	if n := len(fx.grants()); n != 1 {
		t.Fatalf("grants = %d after a restart and a repeat, want exactly 1", n)
	}
}

// A run with no grant is governed by exactly the bound it always was.
func TestAnUngrantedRunKeepsTheOriginalBudget(t *testing.T) {
	fx := newExceptionFixture(t, 0)
	if b := fx.coord.integrationFreshReviewBudget(fx.ctx, fx.childID); b != maxIntegrationFreshReviews {
		t.Fatalf("budget = %d, want %d: the global bound must be untouched for everyone else", b, maxIntegrationFreshReviews)
	}
}

// ---- 5. the explicit second decision --------------------------------------

// Without it, a repeat for an already-granted state is idempotent. With it, a
// person is deliberately authorizing again for that same state -- the only way
// a second generation exists for an unchanged workspace, and it says so.
func TestReauthorizeGrantsASecondGenerationForTheSameWorkspace(t *testing.T) {
	fx := newExceptionFixture(t, maxIntegrationFreshReviews)
	first, err := fx.authorize("joaquin", "first")
	if err != nil {
		t.Fatal(err)
	}
	// The budget must genuinely be spent again before a second grant is due.
	payload, _ := json.Marshal(IntegrationFreshReviewRecord{TaskID: fx.taskID, Attempt: 3})
	if _, err := fx.store.CreateWorkflowCheckpoint(fx.ctx, domain.WorkflowCheckpoint{
		ID: "wfc-req-x", WorkflowRunID: fx.childID, ProjectID: "p",
		RetryState: string(payload), DurablePhase: integrationFreshReviewRequiredPhase,
		PayloadVersion: "v1", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	second, err := fx.coord.AuthorizeIntegrationFreshReviewException(fx.ctx, IntegrationFreshReviewExceptionRequest{
		MasterRunID: "wf-exc-master", TaskID: fx.taskID,
		ApprovedBy: "joaquin", Reason: "the first generation was consumed by a dispatch defect, since fixed",
		Reauthorize: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Generation != first.Generation+1 {
		t.Fatalf("generation = %d, want %d", second.Generation, first.Generation+1)
	}
	if !second.Reauthorized {
		t.Fatal("a second grant for an already-authorized workspace must record that it was a deliberate re-authorization")
	}
	if n := len(fx.grants()); n != 2 {
		t.Fatalf("grants = %d, want 2", n)
	}
}

// Re-authorization is still an authorization: it needs an approver and a
// reason, and it still refuses when the budget is not spent.
func TestReauthorizeStillRefusesAnUnjustifiedGrant(t *testing.T) {
	fx := newExceptionFixture(t, maxIntegrationFreshReviews)
	if _, err := fx.authorize("joaquin", "first"); err != nil {
		t.Fatal(err)
	}
	// Budget not spent again: 2 attempts, budget now 3.
	_, err := fx.coord.AuthorizeIntegrationFreshReviewException(fx.ctx, IntegrationFreshReviewExceptionRequest{
		MasterRunID: "wf-exc-master", TaskID: fx.taskID,
		ApprovedBy: "joaquin", Reason: "again", Reauthorize: true,
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid: re-authorizing does not bypass the budget check", err)
	}
	if _, err := fx.coord.AuthorizeIntegrationFreshReviewException(fx.ctx, IntegrationFreshReviewExceptionRequest{
		MasterRunID: "wf-exc-master", TaskID: fx.taskID, ApprovedBy: "", Reason: "x", Reauthorize: true,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid: re-authorizing still needs a named approver", err)
	}
}
