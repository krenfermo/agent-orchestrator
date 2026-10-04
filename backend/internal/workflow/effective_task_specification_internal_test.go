package workflow

// The authoritative task specification every agent role receives
// (effective_task_specification.go).
//
// The failure this ends is a prompt that contradicts itself: a historical
// objective demanding one thing and an approved, owner-signed amendment
// demanding the opposite, with nothing telling the agent which governs. Task 8
// shipped exactly that prompt for hours — a reviewer following the objective
// blocks correct work forever, one following the criteria looks like it is
// ignoring the objective, and neither reading is the agent's fault.
//
// These run against a real store and the real amendment API, because the whole
// claim is about what the durable ledger produces. A fake would assert the
// fixture's opinion of an amendment rather than an amendment.

import (
	stdctx "context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

const (
	specHistoricalObjective = "Ship the planner surface. Finish by confirming via git status that the " +
		"pre-existing uncommitted backend/internal/postrunqa/*.go changes remain present and unmodified."
	specOriginalCriterion = "git status still shows backend/internal/postrunqa/*.go as modified-but-uncommitted"
	specAmendedCriterion  = "no unrelated files were staged or committed, and the postrunqa changes are " +
		"preserved in the branch history rather than in the working tree"
	specOtherCriterion = "Task API responses include execution strategy and integration state"
)

type specFixture struct {
	t     *testing.T
	coord *Coordinator
	store *sqlite.Store
	ctx   stdctx.Context
	child domain.WorkflowRun
}

// newSpecFixture builds one child execution run under a master plan, with the
// contradictory objective Task 8 actually carried.
func newSpecFixture(t *testing.T) *specFixture {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	now := time.Now().UTC()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	master := domain.WorkflowRun{ID: "wf-spec-master", ProjectID: "p", Objective: specHistoricalObjective,
		State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}",
		CreatedAt: now, UpdatedAt: now}
	if _, _, err := store.CreateWorkflowRun(ctx, master, nil); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal([]string{specOtherCriterion, specOriginalCriterion})
	task := domain.WorkflowTask{
		ID: "task-spec", WorkflowRunID: master.ID, PlanStepID: "step-spec", Ordinal: 1,
		Title: "Planner surface", Description: "does a thing",
		AcceptanceCriteriaJSON: string(raw), VerifyJSON: "{}", ScopeJSON: "{}",
		State: domain.WorkflowTaskRunning, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.InsertWorkflowTasks(ctx, []domain.WorkflowTask{task}); err != nil {
		t.Fatal(err)
	}
	artifact, err := MarshalPlanArtifact(BuildPlanArtifact("p", specHistoricalObjective, policyVersionV1, VerificationPlan{}))
	if err != nil {
		t.Fatal(err)
	}
	childID, taskID := "wf-spec-child", task.ID
	steps := []domain.WorkflowStep{
		{ID: "wfs-plan", WorkflowRunID: childID, Kind: domain.WorkflowStepPlan, Ordinal: 0, State: domain.WorkflowStepCompleted, ArtifactJSON: artifact, CreatedAt: now, UpdatedAt: now},
		{ID: "wfs-work", WorkflowRunID: childID, Kind: domain.WorkflowStepWork, Ordinal: 1, State: domain.WorkflowStepCompleted, ArtifactJSON: "{}", CreatedAt: now, UpdatedAt: now},
		{ID: "wfs-review", WorkflowRunID: childID, Kind: domain.WorkflowStepReview, Ordinal: 2, State: domain.WorkflowStepWaiting, ArtifactJSON: "{}", CreatedAt: now, UpdatedAt: now},
	}
	childRun := domain.WorkflowRun{ID: childID, ProjectID: "p", Objective: specHistoricalObjective,
		State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}",
		CreatedAt: now, UpdatedAt: now, ParentWorkflowID: &master.ID, PlannedTaskID: &taskID}
	created, _, err := store.CreateWorkflowRun(ctx, childRun, steps)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetWorkflowTaskExecutionRun(ctx, task.ID, childID, now); err != nil {
		t.Fatal(err)
	}
	coord := New(Deps{Store: store, Projects: store, Clock: func() time.Time { return time.Now().UTC() }})
	return &specFixture{t: t, coord: coord, store: store, ctx: ctx, child: created}
}

// amend runs the real, human-approved amendment API. An empty replacement
// declares the criterion obsolete.
func (fx *specFixture) amend(replacement string) {
	fx.t.Helper()
	if _, err := fx.coord.AmendTaskAcceptanceCriterion(fx.ctx, TaskCriterionAmendmentRequest{
		RunID: "wf-spec-master", TaskID: "task-spec", CriterionIndex: 1,
		OriginalCriterion: specOriginalCriterion,
		AmendedCriterion:  replacement,
		Reason:            "The criterion asserted a precondition of the environment, not a property of the work.",
		Evidence:          []string{"those changes were committed in full as 70296042b"},
		ApprovedBy:        "joaquin (repository owner)",
	}); err != nil {
		fx.t.Fatalf("AmendTaskAcceptanceCriterion: %v", err)
	}
}

// currentCriteria reads the criteria as they now stand.
func (fx *specFixture) currentCriteria() []string {
	fx.t.Helper()
	tasks, err := fx.store.ListWorkflowTasks(fx.ctx, "wf-spec-master")
	if err != nil || len(tasks) != 1 {
		fx.t.Fatalf("ListWorkflowTasks: %v", err)
	}
	var out []string
	if err := json.Unmarshal([]byte(tasks[0].AcceptanceCriteriaJSON), &out); err != nil {
		fx.t.Fatal(err)
	}
	return out
}

func (fx *specFixture) rendered() string {
	fx.t.Helper()
	return RenderEffectiveSpecification(fx.coord.effectiveTaskSpecification(fx.ctx, fx.child, fx.currentCriteria()))
}

// ---- 1. objective contradicts an amended criterion: the agent is told which wins

func TestAgentIsToldWhichRequirementGovernsWhenTheyConflict(t *testing.T) {
	fx := newSpecFixture(t)
	fx.amend(specAmendedCriterion)
	rendered := fx.rendered()

	if rendered == "" {
		t.Fatal("an amended task rendered no specification, so the prompt still contradicts itself")
	}
	// The authority statement — without it the agent has two requirements and
	// no rule for choosing.
	for _, want := range []string{
		"preserved for audit/history",
		"the approved amendment and current acceptance criteria are authoritative",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("the specification never says which requirement governs (missing %q):\n%s", want, rendered)
		}
	}
	// The original must remain visible: retiring a requirement silently is the
	// abuse the amendment ledger exists to prevent.
	if !strings.Contains(rendered, specOriginalCriterion) {
		t.Fatal("the original requirement was dropped rather than visibly superseded")
	}
	// And the approval and evidence travel with it, so the agent can see the
	// amendment was earned rather than asserted.
	for _, want := range []string{"joaquin (repository owner)", "70296042b", "Reason:"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("the specification omits %q:\n%s", want, rendered)
		}
	}
	if !strings.Contains(rendered, specAmendedCriterion) {
		t.Fatal("the criterion actually in force is missing")
	}
}

// ---- 2. declared_obsolete retires the requirement outright -----------------

func TestObsoleteRequirementIsRetiredNotSoftened(t *testing.T) {
	fx := newSpecFixture(t)
	fx.amend("") // empty replacement declares it obsolete
	rendered := fx.rendered()

	if !strings.Contains(rendered, string(domain.WorkflowTaskCriterionObsolete)) {
		t.Fatalf("the disposition is not stated:\n%s", rendered)
	}
	// "Do not evaluate it" is the operative instruction. A merely-weakened
	// phrasing still lets an agent report the requirement as unmet, which is
	// the behaviour that kept Task 8 blocked.
	for _, want := range []string{"NO LONGER IN FORCE", "Do not evaluate it", "do not attempt to satisfy it"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("an obsolete requirement is not clearly retired (missing %q):\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "this replaces the original above") {
		t.Fatal("an obsolete requirement was rendered as though it had a replacement")
	}
	// It is also genuinely gone from the criteria in force.
	for _, c := range fx.currentCriteria() {
		if c == specOriginalCriterion {
			t.Fatal("an obsolete criterion is still listed as in force")
		}
	}
}

// ---- 3. every role receives the same semantics -----------------------------

// The point of one function is that no role can disagree with another about
// what the task requires.
func TestEveryRoleReceivesTheSameEffectiveSemantics(t *testing.T) {
	fx := newSpecFixture(t)
	fx.amend(specAmendedCriterion)
	spec := fx.rendered()
	if spec == "" {
		t.Fatal("precondition: the fixture should render a specification")
	}
	artifact := BuildPlanArtifact("p", specHistoricalObjective, policyVersionV1, VerificationPlan{})
	artifact.AcceptanceCriteria = fx.currentCriteria()

	prompts := map[string]string{
		"worker": BuildWorkStepPromptWithSpec(artifact, spec),
		"review": BuildReviewPrompt(ReviewPromptInput{
			Objective: specHistoricalObjective, AcceptanceCriteria: artifact.AcceptanceCriteria, EffectiveSpec: spec,
		}),
		"fix": BuildFixPrompt(FixPromptInput{
			Objective: specHistoricalObjective, AcceptanceCriteria: artifact.AcceptanceCriteria, EffectiveSpec: spec,
		}),
		"decisionResolver": BuildDecisionResolverPrompt(DecisionResolverPromptInput{
			Objective: specHistoricalObjective, AcceptanceCriteria: artifact.AcceptanceCriteria, EffectiveSpec: spec,
		}),
		"incidentAdvisor": BuildIncidentDiagnosticPrompt(BuildIncidentContextPack(IncidentPackInput{
			Detail:     RunDetail{Run: fx.child},
			StopReason: "verify_unrepairable", EffectiveSpec: spec,
		})),
	}
	for role, prompt := range prompts {
		if !strings.Contains(prompt, "the approved amendment and current acceptance criteria are authoritative") {
			t.Errorf("%s prompt lacks the authority statement, so it can disagree with the other roles", role)
		}
		if !strings.Contains(prompt, specOriginalCriterion) {
			t.Errorf("%s prompt does not show the original requirement being superseded", role)
		}
		if !strings.Contains(prompt, specAmendedCriterion) {
			t.Errorf("%s prompt does not show the criterion in force", role)
		}
	}
}

// ---- 4. no amendments: every prompt is byte-for-byte what it always was ----

// The regression that matters for the overwhelming majority of runs, which are
// never amended.
func TestWithoutAmendmentsEveryPromptIsByteForByteUnchanged(t *testing.T) {
	fx := newSpecFixture(t) // no amendment recorded
	spec := fx.coord.effectiveTaskSpecification(fx.ctx, fx.child, fx.currentCriteria())
	if spec.HasAmendments() {
		t.Fatal("a task with no amendments reported some")
	}
	rendered := RenderEffectiveSpecification(spec)
	if rendered != "" {
		t.Fatalf("an unamended task rendered %q, want empty", rendered)
	}
	// This layer reads; it never rewrites.
	if spec.Objective != specHistoricalObjective {
		t.Fatalf("the historical objective was altered: %q", spec.Objective)
	}

	artifact := BuildPlanArtifact("p", specHistoricalObjective, policyVersionV1, VerificationPlan{})
	artifact.AcceptanceCriteria = fx.currentCriteria()

	if BuildWorkStepPromptWithSpec(artifact, rendered) != BuildWorkStepPrompt(artifact) {
		t.Error("worker prompt changed for a task with no amendments")
	}
	reviewIn := ReviewPromptInput{Objective: specHistoricalObjective, AcceptanceCriteria: artifact.AcceptanceCriteria}
	reviewWithSpec := reviewIn
	reviewWithSpec.EffectiveSpec = rendered
	if BuildReviewPrompt(reviewWithSpec) != BuildReviewPrompt(reviewIn) {
		t.Error("review prompt changed for a task with no amendments")
	}
	fixIn := FixPromptInput{Objective: specHistoricalObjective, AcceptanceCriteria: artifact.AcceptanceCriteria}
	fixWithSpec := fixIn
	fixWithSpec.EffectiveSpec = rendered
	if BuildFixPrompt(fixWithSpec) != BuildFixPrompt(fixIn) {
		t.Error("fix prompt changed for a task with no amendments")
	}
	drIn := DecisionResolverPromptInput{Objective: specHistoricalObjective, AcceptanceCriteria: artifact.AcceptanceCriteria}
	drWithSpec := drIn
	drWithSpec.EffectiveSpec = rendered
	if BuildDecisionResolverPrompt(drWithSpec) != BuildDecisionResolverPrompt(drIn) {
		t.Error("decision resolver prompt changed for a task with no amendments")
	}
}

// ---- 5. derived, therefore idempotent and restart-safe ---------------------

func TestSpecificationIsDerivedIdempotentAndRestartSafe(t *testing.T) {
	fx := newSpecFixture(t)
	fx.amend(specAmendedCriterion)
	first := fx.rendered()

	for i := 0; i < 20; i++ {
		if got := fx.rendered(); got != first {
			t.Fatal("the specification is not stable across repeated builds")
		}
	}
	// A second Coordinator over the same durable store IS a daemon restart.
	fx.coord = New(Deps{Store: fx.store, Projects: fx.store, Clock: func() time.Time { return time.Now().UTC() }})
	if got := fx.rendered(); got != first {
		t.Fatal("the specification changed across a restart")
	}
	// And building it must write nothing: it is a pure read of durable state.
	cps, err := fx.store.ListWorkflowCheckpoints(fx.ctx, fx.child.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := len(cps)
	_ = fx.rendered()
	cps, err = fx.store.ListWorkflowCheckpoints(fx.ctx, fx.child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cps) != before {
		t.Fatalf("building the specification wrote %d checkpoints; it must be a pure read", len(cps)-before)
	}
}

// ---- 6. the audit keeps the original objective and criterion ---------------

func TestAuditKeepsTheOriginalObjectiveAndCriterion(t *testing.T) {
	fx := newSpecFixture(t)
	fx.amend(specAmendedCriterion)

	// The persisted objective is untouched, including the sentence the
	// amendment superseded.
	run, ok, err := fx.store.GetWorkflowRun(fx.ctx, fx.child.ID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if run.Objective != specHistoricalObjective {
		t.Fatalf("run.Objective was rewritten:\n got: %q\nwant: %q", run.Objective, specHistoricalObjective)
	}
	if !strings.Contains(run.Objective, "uncommitted") {
		t.Fatal("the historical objective no longer states the requirement it originally carried")
	}
	// The amendment ledger still carries the original criterion verbatim,
	// with its approval, reason and evidence.
	amendments, err := fx.store.ListWorkflowTaskCriterionAmendments(fx.ctx, "wf-spec-master")
	if err != nil || len(amendments) != 1 {
		t.Fatalf("amendments = %d, err = %v, want exactly 1", len(amendments), err)
	}
	a := amendments[0]
	if a.OriginalCriterion != specOriginalCriterion {
		t.Fatalf("the original criterion was not preserved: %q", a.OriginalCriterion)
	}
	if a.ApprovedBy == "" || a.Reason == "" || len(a.Evidence) == 0 {
		t.Fatal("the amendment lost its approval, reason or evidence")
	}
}
