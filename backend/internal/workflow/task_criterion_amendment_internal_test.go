package workflow

// Plan / Acceptance Criteria Amendment.
//
// The mechanism's whole risk is that it becomes a way to talk a reviewer out of
// a real finding, so most of these tests are about what it REFUSES. The two
// that are about what it does check the properties that make an amendment
// trustworthy afterwards: the original text survives, and the work is judged
// again rather than inheriting a verdict reached under a criterion that no
// longer exists.

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type amendFixture struct {
	coord  *Coordinator
	store  *sqlite.Store
	ctx    stdctx.Context
	master domain.WorkflowRun
	task   domain.WorkflowTask
	child  RunDetail
}

func newAmendFixture(t *testing.T, criteria []string) *amendFixture {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	now := time.Now().UTC()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	master := domain.WorkflowRun{ID: "wf-amend", ProjectID: "p", Objective: "objective",
		State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}",
		CreatedAt: now, UpdatedAt: now}
	if _, _, err := store.CreateWorkflowRun(ctx, master, nil); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(criteria)
	task := domain.WorkflowTask{
		ID: "task-a", WorkflowRunID: master.ID, PlanStepID: "step-a", Ordinal: 1,
		Title: "Task A", Description: "does a thing",
		AcceptanceCriteriaJSON: string(raw), VerifyJSON: "{}", ScopeJSON: "{}",
		State: domain.WorkflowTaskRunning, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.InsertWorkflowTasks(ctx, []domain.WorkflowTask{task}); err != nil {
		t.Fatal(err)
	}

	// A child that has already been reviewed and blocked, which is the state an
	// amendment actually has to deal with.
	artifact, err := MarshalPlanArtifact(BuildPlanArtifact("p", "objective", policyVersionV1, VerificationPlan{}))
	if err != nil {
		t.Fatal(err)
	}
	childID, taskID := "wf-exec-a", task.ID
	steps := []domain.WorkflowStep{
		{ID: "wfs-plan", WorkflowRunID: childID, Kind: domain.WorkflowStepPlan, Ordinal: 0, State: domain.WorkflowStepCompleted, ArtifactJSON: artifact, CreatedAt: now, UpdatedAt: now},
		{ID: "wfs-work", WorkflowRunID: childID, Kind: domain.WorkflowStepWork, Ordinal: 1, State: domain.WorkflowStepCompleted, ArtifactJSON: "{}", CreatedAt: now, UpdatedAt: now},
		{ID: "wfs-review", WorkflowRunID: childID, Kind: domain.WorkflowStepReview, Ordinal: 2, State: domain.WorkflowStepWaiting, ArtifactJSON: "{}", CreatedAt: now, UpdatedAt: now},
		{ID: "wfs-fix", WorkflowRunID: childID, Kind: domain.WorkflowStepFix, Ordinal: 3, State: domain.WorkflowStepWaiting, ArtifactJSON: "{}", CreatedAt: now, UpdatedAt: now},
	}
	childRun := domain.WorkflowRun{ID: childID, ProjectID: "p", Objective: "objective",
		State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}",
		CreatedAt: now, UpdatedAt: now, ParentWorkflowID: &master.ID, PlannedTaskID: &taskID}
	created, createdSteps, err := store.CreateWorkflowRun(ctx, childRun, steps)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateWorkflowRunState(ctx, childID, domain.WorkflowRunRunning, domain.WorkflowRunNeedsAttention, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetWorkflowTaskExecutionRun(ctx, task.ID, childID, now); err != nil {
		t.Fatal(err)
	}
	task.ExecutionRunID = &childID

	detail := RunDetail{Run: created}
	for _, s := range createdSteps {
		detail.Steps = append(detail.Steps, StepDetail{Step: s})
	}
	coord := New(Deps{Store: store, Projects: store, Clock: func() time.Time { return time.Now().UTC() }})
	return &amendFixture{coord: coord, store: store, ctx: ctx, master: master, task: task, child: detail}
}

func baseRequest() TaskCriterionAmendmentRequest {
	return TaskCriterionAmendmentRequest{
		RunID: "wf-amend", TaskID: "task-a", CriterionIndex: 1,
		Reason:     "the state it describes was committed in 70296042b",
		Evidence:   []string{"70296042b feat(postrunqa): add QA finding attribution"},
		ApprovedBy: "joaquin",
	}
}

// The refusal that matters most: an agent cannot amend the bar it is judged
// against. Without a named human this is not a governance mechanism at all.
func TestAmendmentWithoutAHumanApproverIsRefused(t *testing.T) {
	f := newAmendFixture(t, []string{"a", "b"})
	req := baseRequest()
	req.ApprovedBy = "   "
	if _, err := f.coord.AmendTaskAcceptanceCriterion(f.ctx, req); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	// And nothing was written.
	if got := f.currentCriteria(t); len(got) != 2 {
		t.Fatalf("criteria = %v, want them untouched", got)
	}
	if a, _ := f.coord.ListTaskCriterionAmendments(f.ctx, f.master.ID); len(a) != 0 {
		t.Fatalf("amendments = %d, want 0", len(a))
	}
}

// A reason and checkable evidence are what make the record auditable later.
func TestAmendmentWithoutReasonOrEvidenceIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mutil func(*TaskCriterionAmendmentRequest)
	}{
		{"no reason", func(r *TaskCriterionAmendmentRequest) { r.Reason = "" }},
		{"no evidence", func(r *TaskCriterionAmendmentRequest) { r.Evidence = nil }},
		{"blank evidence", func(r *TaskCriterionAmendmentRequest) { r.Evidence = []string{"  "} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAmendFixture(t, []string{"a", "b"})
			req := baseRequest()
			tc.mutil(&req)
			if _, err := f.coord.AmendTaskAcceptanceCriterion(f.ctx, req); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

// A completed task's criteria are history. Amending them would rewrite the
// standard something was already judged against.
func TestAmendingATerminalTaskIsRefused(t *testing.T) {
	f := newAmendFixture(t, []string{"a", "b"})
	if _, err := f.store.UpdateWorkflowTaskState(f.ctx, "task-a",
		domain.WorkflowTaskRunning, domain.WorkflowTaskCompleted, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.coord.AmendTaskAcceptanceCriterion(f.ctx, baseRequest()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

// Amending the criterion you think you are amending: the text is the identity,
// not the index.
func TestAmendmentRefusesWhenTheTextDoesNotMatch(t *testing.T) {
	f := newAmendFixture(t, []string{"a", "b"})
	req := baseRequest()
	req.OriginalCriterion = "something else entirely"
	if _, err := f.coord.AmendTaskAcceptanceCriterion(f.ctx, req); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	req.CriterionIndex = 9
	req.OriginalCriterion = ""
	if _, err := f.coord.AmendTaskAcceptanceCriterion(f.ctx, req); !errors.Is(err, ErrInvalid) {
		t.Fatalf("out-of-range index: err = %v, want ErrInvalid", err)
	}
}

// Declaring a criterion obsolete removes it, keeps the original forever, and
// re-opens the review. Nothing about it approves the work.
func TestDeclaringACriterionObsoleteRemovesItAndReopensReview(t *testing.T) {
	f := newAmendFixture(t, []string{"keep me", "the stale one", "keep me too"})
	req := baseRequest()
	req.OriginalCriterion = "the stale one"

	amendment, err := f.coord.AmendTaskAcceptanceCriterion(f.ctx, req)
	if err != nil {
		t.Fatalf("amend: %v", err)
	}
	if amendment.Disposition != domain.WorkflowTaskCriterionObsolete {
		t.Fatalf("disposition = %q, want declared_obsolete", amendment.Disposition)
	}

	// Applied to the task…
	got := f.currentCriteria(t)
	if len(got) != 2 || got[0] != "keep me" || got[1] != "keep me too" {
		t.Fatalf("criteria = %v, want the stale one removed and the others untouched", got)
	}
	// …and the original survives, with who approved it and why.
	all, err := f.coord.ListTaskCriterionAmendments(f.ctx, f.master.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("amendments = %d, want 1", len(all))
	}
	rec := all[0]
	if rec.OriginalCriterion != "the stale one" || rec.ApprovedBy != "joaquin" ||
		rec.Reason == "" || len(rec.Evidence) != 1 || rec.CreatedAt.IsZero() {
		t.Fatalf("the amendment does not preserve what it must: %+v", rec)
	}

	// The reviewer will be handed the amended criteria, not the old ones.
	artifact, err := f.coord.planArtifactForRun(f.ctx, f.child.Run)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.AcceptanceCriteria) != 2 {
		t.Fatalf("the child's plan artifact still carries %d criteria: %v",
			len(artifact.AcceptanceCriteria), artifact.AcceptanceCriteria)
	}

	// And the work is judged again rather than inheriting the old verdict.
	f.assertReviewReopened(t)
}

// An amendment replaces the criterion with one that must still be met — the
// bar moves, it does not disappear.
func TestAmendingACriterionReplacesItAndStillRequiresIt(t *testing.T) {
	f := newAmendFixture(t, []string{"a", "postrunqa stays uncommitted"})
	req := baseRequest()
	req.AmendedCriterion = "the postrunqa changes are present in the branch history (70296042b)"

	amendment, err := f.coord.AmendTaskAcceptanceCriterion(f.ctx, req)
	if err != nil {
		t.Fatalf("amend: %v", err)
	}
	if amendment.Disposition != domain.WorkflowTaskCriterionAmended {
		t.Fatalf("disposition = %q, want amended", amendment.Disposition)
	}
	got := f.currentCriteria(t)
	if len(got) != 2 || got[1] != req.AmendedCriterion {
		t.Fatalf("criteria = %v, want the replacement in place", got)
	}
	f.assertReviewReopened(t)
}

func (f *amendFixture) currentCriteria(t *testing.T) []string {
	t.Helper()
	tasks, err := f.store.ListWorkflowTasks(f.ctx, f.master.ID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	if err := json.Unmarshal([]byte(tasks[0].AcceptanceCriteriaJSON), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// assertReviewReopened is the property that keeps an amendment from being an
// approval: the previous verdict is superseded, the review step is back to
// pending, and the child is no longer parked.
func (f *amendFixture) assertReviewReopened(t *testing.T) {
	t.Helper()
	steps, err := f.store.ListWorkflowSteps(f.ctx, f.child.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.Kind == domain.WorkflowStepReview && s.State != domain.WorkflowStepPending {
			t.Fatalf("review step = %q, want pending so a fresh independent review runs", s.State)
		}
	}
	run, ok, err := f.store.GetWorkflowRun(f.ctx, f.child.Run.ID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if run.State == domain.WorkflowRunNeedsAttention {
		t.Fatal("the child is still parked; the amendment did not re-open the question")
	}
	cps, err := f.store.ListWorkflowCheckpoints(f.ctx, f.child.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, cp := range cps {
		if cp.DurablePhase == taskCriterionAmendedPhase {
			found = true
		}
	}
	if !found {
		t.Fatal("no durable record of the amendment on the run whose verdict it superseded")
	}
}
