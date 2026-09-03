package workflow_test

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// p3c_advice_test.go — P3-C §29's matrix, asserted on the projection rather
// than on a screen.
//
// Every case below states one property of the answer to "what do I do now":
// which category the situation falls in, whether a person is interrupted, what
// AO says it will do by itself, and which actions are offered or refused. They
// are deliberately assertions about the CONTRACT and not about wording — the
// prose is a fallback for a client with no copy, and a test that pinned it
// would make translating the product a test failure.

func adviceFor(detail workflowcore.RunDetail, placements []workflowcore.PlacementView, repair workflowcore.RepairPlan) workflowcore.Advice {
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail, Questions: detail.Questions})
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	p := workflowcore.DerivePresentation(workflowcore.PresentationInput{
		Detail: detail, Lifecycle: life, Placements: placements, Now: now,
	})
	return workflowcore.DeriveAdvice(workflowcore.AdviceInput{
		Detail: detail, Lifecycle: life, Presentation: p, Repair: repair, Now: now,
	})
}

// stoppedOn builds a run durably parked on one canonical reason.
func stoppedOn(reason string) workflowcore.RunDetail {
	return workflowcore.RunDetail{
		Run: domain.WorkflowRun{
			ID: "wf-1", State: domain.WorkflowRunNeedsAttention,
			CreatedAt: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC),
		},
		Steps: singleTaskSteps(domain.WorkflowStepCompleted, domain.WorkflowStepCompleted,
			domain.WorkflowStepPending, domain.WorkflowStepReady),
		StopAuthorityPhase: reason,
		StopAuthorityAt:    time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC),
		CheckpointsFolded:  true,
	}
}

func hasAction(a workflowcore.Advice, id workflowcore.ActionID) bool {
	for _, got := range a.AvailableActions {
		if got == id {
			return true
		}
	}
	return false
}

func blockedReason(a workflowcore.Advice, id workflowcore.ActionID) (string, bool) {
	for _, b := range a.BlockedActions {
		if b.ID == id {
			return b.Reason, true
		}
	}
	return "", false
}

// §13: a run that is simply working answers "you do not need to do anything",
// and offers nobody a remedy for a problem that does not exist.
func TestWorkingRunRequiresNoAction(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run: domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunRunning, CreatedAt: time.Now().Add(-time.Hour)},
		Steps: singleTaskSteps(domain.WorkflowStepRunning, domain.WorkflowStepPending,
			domain.WorkflowStepPending, domain.WorkflowStepPending),
	}
	a := adviceFor(detail, directBranchPlacement(), workflowcore.RepairPlan{})
	if a.Category != workflowcore.AdviceNoActionRequired {
		t.Fatalf("category = %q, want no_action_required", a.Category)
	}
	if a.RequiresHuman {
		t.Fatal("a working run asked for a human")
	}
	if a.RecommendedAction != "" {
		t.Fatalf("a working run recommended %q", a.RecommendedAction)
	}
}

// A review in flight is the same answer: AO is doing the work.
func TestReviewingRunRequiresNoAction(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run: domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunRunning, CreatedAt: time.Now().Add(-time.Hour)},
		Steps: singleTaskSteps(domain.WorkflowStepCompleted, domain.WorkflowStepRunning,
			domain.WorkflowStepPending, domain.WorkflowStepPending),
	}
	a := adviceFor(detail, directBranchPlacement(), workflowcore.RepairPlan{})
	if a.Category != workflowcore.AdviceNoActionRequired || a.RequiresHuman {
		t.Fatalf("a review in flight asked for a human: %+v", a)
	}
}

// §3/§19: while a repair generation is alive the origin is AO's problem, not a
// person's — and the second remedy that would duplicate it is refused with a
// reason rather than silently withheld.
func TestActiveRepairRequiresNoHumanAndBlocksDuplicateRemedies(t *testing.T) {
	detail := stoppedOn(workflowcore.ReasonFixBudgetExhausted)
	detail.Repair = workflowcore.RepairLifecycle{Active: true, Attempt: 1, Budget: 2, RunID: "wf-repair-1"}
	a := adviceFor(detail, directBranchPlacement(), workflowcore.RepairPlan{
		Eligibility: domain.RepairEligible, Mode: domain.RepairModeAutomatic, Budget: 2,
	})
	if a.Category != workflowcore.AdviceAutoRecoverable {
		t.Fatalf("category = %q, want auto_recoverable", a.Category)
	}
	if a.RequiresHuman {
		t.Fatal("a run being repaired asked for a human")
	}
	if a.AutomaticAction != workflowcore.AutoActionRepairInFlight || !a.AutomaticActionActive {
		t.Fatalf("automatic action = %q active=%v, want repair_in_flight active", a.AutomaticAction, a.AutomaticActionActive)
	}
	if reason, ok := blockedReason(a, workflowcore.ActionRepair); !ok || reason != "repair_active" {
		t.Fatalf("Repair was not refused with repair_active: %+v", a.BlockedActions)
	}
	if a.RecommendedAction != "" {
		t.Fatalf("a repairing run still recommended %q", a.RecommendedAction)
	}
	if a.ExpectedNextStage != workflowcore.StageCorrecting {
		t.Fatalf("expected next stage = %q, want correcting", a.ExpectedNextStage)
	}
}

// §3: automatic policy + repairable condition + budget = AO says it will do it
// itself, and reports the intent as NOT yet active — an undispatched repair
// must never read as a running one.
func TestAutomaticRepairPolicyProducesAnAutomaticIntent(t *testing.T) {
	a := adviceFor(stoppedOn(workflowcore.ReasonFixNoVerifiableChange), directBranchPlacement(),
		workflowcore.RepairPlan{
			Eligibility: domain.RepairEligible, Mode: domain.RepairModeAutomatic,
			AutomaticAllowed: true, Budget: 2,
		})
	if a.Category != workflowcore.AdviceAutoRecoverable {
		t.Fatalf("category = %q, want auto_recoverable", a.Category)
	}
	if a.AutomaticAction != workflowcore.AutoActionLaunchRepair {
		t.Fatalf("automatic action = %q, want launch_repair", a.AutomaticAction)
	}
	if a.AutomaticActionActive {
		t.Fatal("an undispatched repair reported itself as already running")
	}
	if a.RequiresHuman {
		t.Fatal("a run AO is authorized to repair asked for a human")
	}
}

// §3: suggest offers the button and does NOT claim AO will act.
func TestSuggestRepairPolicyOffersRepairAndAsksTheHuman(t *testing.T) {
	a := adviceFor(stoppedOn(workflowcore.ReasonFixBudgetExhausted), directBranchPlacement(),
		workflowcore.RepairPlan{
			Eligibility: domain.RepairEligible, Mode: domain.RepairModeSuggest,
			AutomaticAllowed: false, Budget: 2,
		})
	if a.Category != workflowcore.AdviceHumanAction || !a.RequiresHuman {
		t.Fatalf("suggest policy did not leave the decision with a person: %+v", a)
	}
	if a.AutomaticAction != workflowcore.AutoActionNone {
		t.Fatalf("suggest policy claimed an automatic action %q", a.AutomaticAction)
	}
	if !hasAction(a, workflowcore.ActionRepair) {
		t.Fatalf("suggest policy did not offer Repair: %+v", a.AvailableActions)
	}
	if a.AutomaticActionBlockedReason != "repair_requires_authorization" {
		t.Fatalf("blocked reason = %q, want repair_requires_authorization", a.AutomaticActionBlockedReason)
	}
}

// §3: disabled never claims an automatic action, and says so.
func TestDisabledRepairPolicyNamesWhyAOIsNotRepairing(t *testing.T) {
	a := adviceFor(stoppedOn(workflowcore.ReasonFixBudgetExhausted), directBranchPlacement(),
		workflowcore.RepairPlan{Eligibility: domain.RepairPolicyDisabled, Mode: domain.RepairModeDisabled, Budget: 2})
	if a.AutomaticAction != workflowcore.AutoActionNone {
		t.Fatalf("disabled policy claimed %q", a.AutomaticAction)
	}
	if a.AutomaticActionBlockedReason != "repair_disabled" {
		t.Fatalf("blocked reason = %q, want repair_disabled", a.AutomaticActionBlockedReason)
	}
}

// §18: an exhausted budget is a bound reached, reported as one.
func TestExhaustedRepairBudgetIsReportedAsABound(t *testing.T) {
	detail := stoppedOn(workflowcore.ReasonFixBudgetExhausted)
	detail.Repair = workflowcore.RepairLifecycle{Attempt: 2, Budget: 2, Exhausted: true}
	a := adviceFor(detail, directBranchPlacement(), workflowcore.RepairPlan{
		Eligibility: domain.RepairBudgetExhausted, Mode: domain.RepairModeAutomatic, Spent: 2, Budget: 2,
	})
	if a.AutomaticAction != workflowcore.AutoActionNone {
		t.Fatalf("an exhausted budget still claimed %q", a.AutomaticAction)
	}
	if a.AutomaticActionBlockedReason != "repair_exhausted" {
		t.Fatalf("blocked reason = %q, want repair_exhausted", a.AutomaticActionBlockedReason)
	}
	if reason, ok := blockedReason(a, workflowcore.ActionRepair); !ok || reason != "repair_exhausted" {
		t.Fatalf("Repair was not refused with repair_exhausted: %+v", a.BlockedActions)
	}
	if !a.RequiresHuman {
		t.Fatal("an exhausted repair budget is a person's and must say so")
	}
}

// §4: a dirty worktree is a person's, the remedy is the commit flow, and no
// automatic action is claimed for it.
func TestDirtyWorktreeAdviceIsCommitAndContinue(t *testing.T) {
	a := adviceFor(stoppedOn("dirty_worktree"), directBranchPlacement(), workflowcore.RepairPlan{})
	if a.Category != workflowcore.AdviceHumanAction || !a.RequiresHuman {
		t.Fatalf("a dirty worktree did not read as a human action: %+v", a)
	}
	if a.RecommendedAction != workflowcore.ActionCommitAndContinue {
		t.Fatalf("recommended = %q, want commit_and_continue", a.RecommendedAction)
	}
	if !hasAction(a, workflowcore.ActionViewChanges) {
		t.Fatalf("the dirty-worktree advice did not offer the diff: %+v", a.AvailableActions)
	}
	if a.AutomaticAction != workflowcore.AutoActionNone {
		t.Fatalf("a dirty worktree claimed automatic action %q", a.AutomaticAction)
	}
}

// §5: a branch another run holds is a wait, not an error and not a question.
// Waiting is the recommendation, and it is the ONE recommendation a wait keeps.
func TestBranchWaitIsWaitOnly(t *testing.T) {
	detail := stoppedOn(workflowcore.ReasonBranchQueued)
	detail.Run.State = domain.WorkflowRunWaiting
	detail.BranchWait = &workflowcore.BranchWait{Branch: "feat/x", HeldByWorkflowRunID: "wf-2"}
	a := adviceFor(detail, directBranchPlacement(), workflowcore.RepairPlan{})
	if a.Category != workflowcore.AdviceWaitOnly {
		t.Fatalf("category = %q, want wait_only", a.Category)
	}
	if a.RequiresHuman {
		t.Fatal("a branch queue asked for a human")
	}
	if a.AutomaticAction != workflowcore.AutoActionAwaitBranch {
		t.Fatalf("automatic action = %q, want await_branch", a.AutomaticAction)
	}
	if a.RecommendedAction != workflowcore.ActionWait {
		t.Fatalf("recommended = %q, want wait", a.RecommendedAction)
	}
	if !hasAction(a, workflowcore.ActionViewBlockingWorkflow) {
		t.Fatalf("the branch wait did not offer the blocking run: %+v", a.AvailableActions)
	}
}

// §5: an EXPLICIT direct-branch choice is never offered a worktree as a way out
// of a branch queue. Turning that choice into a worktree is the user's, and AO
// proposing it is not the same as AO taking it — the refusal is visible, with
// its reason.
func TestExplicitDirectBranchIsNeverOfferedAWorktreeFallback(t *testing.T) {
	detail := stoppedOn(workflowcore.ReasonBranchQueued)
	detail.Run.State = domain.WorkflowRunWaiting
	detail.BranchWait = &workflowcore.BranchWait{Branch: "feat/x", HeldByWorkflowRunID: "wf-2"}
	placements := directBranchPlacement()
	overrides := []workflowcore.PlacementOverrideView{{
		State: domain.PlacementOverrideApplied, AppliedGeneration: 1,
		Requested: domain.PlacementOverrideDirectBranch, Reason: "user_choice",
	}}
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail, Questions: detail.Questions})
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	p := workflowcore.DerivePresentation(workflowcore.PresentationInput{
		Detail: detail, Lifecycle: life, Placements: placements, Overrides: overrides, Now: now,
	})
	a := workflowcore.DeriveAdvice(workflowcore.AdviceInput{
		Detail: detail, Lifecycle: life, Presentation: p, Now: now,
	})
	if hasAction(a, workflowcore.ActionUseIsolatedWorktree) {
		t.Fatal("an explicit direct-branch run was offered an isolated worktree")
	}
	reason, ok := blockedReason(a, workflowcore.ActionUseIsolatedWorktree)
	if !ok || reason != "placement_explicit" {
		t.Fatalf("the worktree fallback was hidden rather than refused with a reason: %+v", a.BlockedActions)
	}
}

// §5: an AUTOMATIC placement choice may be offered the worktree, because
// nobody chose the branch.
func TestAutomaticPlacementMayBeOfferedAWorktreeFallback(t *testing.T) {
	detail := stoppedOn(workflowcore.ReasonBranchQueued)
	detail.Run.State = domain.WorkflowRunWaiting
	detail.BranchWait = &workflowcore.BranchWait{Branch: "feat/x", HeldByWorkflowRunID: "wf-2"}
	a := adviceFor(detail, directBranchPlacement(), workflowcore.RepairPlan{})
	if !hasAction(a, workflowcore.ActionUseIsolatedWorktree) {
		t.Fatalf("an automatic placement was not offered the worktree: %+v", a.AvailableActions)
	}
}

// §6: a capacity wait is not an error, gets no Repair, and asks nobody.
func TestCapacityWaitIsWaitOnlyAndOffersNoRepair(t *testing.T) {
	detail := stoppedOn(workflowcore.ReasonPlannerCapacityWait)
	detail.Run.State = domain.WorkflowRunWaiting
	detail.CapacityWait = &workflowcore.CapacityWait{Role: domain.WorkflowRole("worker")}
	a := adviceFor(detail, directBranchPlacement(), workflowcore.RepairPlan{})
	if a.Category != workflowcore.AdviceWaitOnly {
		t.Fatalf("category = %q, want wait_only", a.Category)
	}
	if a.RequiresHuman {
		t.Fatal("a capacity wait asked for a human")
	}
	if a.AutomaticAction != workflowcore.AutoActionAwaitCapacity {
		t.Fatalf("automatic action = %q, want await_capacity", a.AutomaticAction)
	}
	if hasAction(a, workflowcore.ActionRepair) {
		t.Fatal("a capacity wait offered Repair")
	}
	if a.RecommendedAction != "" {
		t.Fatalf("a capacity wait recommended %q", a.RecommendedAction)
	}
}

// §7: a provider attempt that failed on a class another provider could succeed
// at reads as a failover in progress, not as a stop.
func TestProviderFailoverReadsAsAutomaticNotAsAStop(t *testing.T) {
	steps := singleTaskSteps(domain.WorkflowStepReady, domain.WorkflowStepPending,
		domain.WorkflowStepPending, domain.WorkflowStepPending)
	for i := range steps {
		if steps[i].Step.Kind != domain.WorkflowStepWork {
			continue
		}
		steps[i].Attempts = []domain.WorkflowAttempt{{
			ID: "wfa-1", Harness: "claude-code", Outcome: domain.WorkflowAttemptFailed,
			ErrorClass: domain.WorkflowErrorTransient,
			StartedAt:  time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC),
		}}
	}
	detail := workflowcore.RunDetail{
		Run:   domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunRunning, CreatedAt: time.Now().Add(-time.Hour)},
		Steps: steps,
	}
	a := adviceFor(detail, directBranchPlacement(), workflowcore.RepairPlan{})
	if a.RequiresHuman {
		t.Fatal("a provider failover asked for a human")
	}
	if a.Category != workflowcore.AdviceAutoRecoverable {
		t.Fatalf("category = %q, want auto_recoverable", a.Category)
	}
	if a.AutomaticAction != workflowcore.AutoActionProviderFailover {
		t.Fatalf("automatic action = %q, want provider_failover", a.AutomaticAction)
	}
}

// §7: an auth failure is NOT a failover. Another provider does not fix a
// credential, and claiming AO is "trying the other one" would be a fabrication.
func TestAuthFailureIsNotReportedAsAFailover(t *testing.T) {
	a := adviceFor(stoppedOn(workflowcore.ReasonProviderAuthRequired), directBranchPlacement(),
		workflowcore.RepairPlan{Eligibility: domain.RepairIneligible})
	if a.AutomaticAction != workflowcore.AutoActionNone {
		t.Fatalf("an auth stop claimed automatic action %q", a.AutomaticAction)
	}
	if !a.RequiresHuman {
		t.Fatal("an auth stop must be a person's")
	}
	if a.RecommendedAction != workflowcore.ActionAuthenticate {
		t.Fatalf("recommended = %q, want authenticate", a.RecommendedAction)
	}
	if hasAction(a, workflowcore.ActionRepair) {
		t.Fatal("an auth stop offered Repair")
	}
}

// §9: an ambiguous plan gets revalidate/regenerate, and the headline is never a
// fingerprint.
func TestPlannerAmbiguousOffersRevalidateAndRegenerate(t *testing.T) {
	a := adviceFor(stoppedOn(workflowcore.ReasonPlannerAmbiguous), directBranchPlacement(), workflowcore.RepairPlan{})
	if a.RecommendedAction != workflowcore.ActionRevalidatePlan {
		t.Fatalf("recommended = %q, want revalidate_plan", a.RecommendedAction)
	}
	if !hasAction(a, workflowcore.ActionRegeneratePlan) {
		t.Fatalf("regenerate was not offered: %+v", a.AvailableActions)
	}
	if !a.RequiresHuman {
		t.Fatal("an ambiguous plan is a person's")
	}
}

// §10: the fix/review incidents do not all map onto "Repair". Each one's
// category and repairability is asserted from the SAME registry the stop sites
// write, so a new reason cannot silently inherit somebody else's remedy.
func TestFixAndReviewIncidentsAreDistinguished(t *testing.T) {
	for _, tc := range []struct {
		reason     string
		repairable bool
		human      bool
	}{
		{workflowcore.ReasonReviewerLaunchFailed, false, true},
		{workflowcore.ReasonFixNoVerifiableChange, true, true},
		{workflowcore.ReasonFixBudgetExhausted, true, true},
		{workflowcore.ReasonReviewerAuthInvalid, false, true},
		{workflowcore.ReasonVerifyFixUnavailable, true, true},
	} {
		a := adviceFor(stoppedOn(tc.reason), directBranchPlacement(), workflowcore.RepairPlan{})
		if a.Repairable != tc.repairable {
			t.Errorf("%s: repairable = %v, want %v", tc.reason, a.Repairable, tc.repairable)
		}
		if a.RequiresHuman != tc.human {
			t.Errorf("%s: requiresHuman = %v, want %v", tc.reason, a.RequiresHuman, tc.human)
		}
		if a.ReasonCode != tc.reason {
			t.Errorf("%s: reasonCode = %q — the advice invented a vocabulary", tc.reason, a.ReasonCode)
		}
	}
}

// §11: a completed isolated placement whose work has not landed recommends the
// integration and says nobody is otherwise needed.
func TestIntegrationPendingRecommendsIntegrate(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run: domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunCompleted, CreatedAt: time.Now().Add(-time.Hour)},
		Steps: singleTaskSteps(domain.WorkflowStepCompleted, domain.WorkflowStepCompleted,
			domain.WorkflowStepPending, domain.WorkflowStepCompleted),
	}
	a := adviceFor(detail, isolatedPlacement(domain.PlacementActive, ""), workflowcore.RepairPlan{})
	if a.Category != workflowcore.AdviceTerminal {
		t.Fatalf("category = %q, want terminal", a.Category)
	}
	if a.RecommendedAction != workflowcore.ActionIntegrate {
		t.Fatalf("recommended = %q, want integrate", a.RecommendedAction)
	}
}

// §11: a direct-branch run never gets integration advice, because it has
// nothing to integrate.
func TestDirectBranchNeverGetsIntegrationAdvice(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run: domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunCompleted, CreatedAt: time.Now().Add(-time.Hour)},
		Steps: singleTaskSteps(domain.WorkflowStepCompleted, domain.WorkflowStepCompleted,
			domain.WorkflowStepPending, domain.WorkflowStepCompleted),
	}
	a := adviceFor(detail, directBranchPlacement(), workflowcore.RepairPlan{})
	if a.RecommendedAction == workflowcore.ActionIntegrate || hasAction(a, workflowcore.ActionIntegrate) {
		t.Fatalf("a direct-branch run was advised to integrate: %+v", a)
	}
}

// §12: a Project Memory failure with a safe fallback is not a stop and must
// never reach the Advisor as one. The property is stated where it can be
// enforced: no memory-shaped reason is in the disposition registry, so a
// run cannot be parked on one, and an unclassified stop is never billed to a
// person.
func TestUnclassifiedStopIsNeverBilledToAHuman(t *testing.T) {
	a := adviceFor(stoppedOn("project_memory_unavailable"), directBranchPlacement(), workflowcore.RepairPlan{})
	if a.RequiresHuman {
		t.Fatal("a stop AO cannot name was billed to a person")
	}
	if a.Category != workflowcore.AdviceWaitOnly {
		t.Fatalf("category = %q, want wait_only for an unnameable stop", a.Category)
	}
}

// §20-22: a question AO is resolving by itself is AO working, not a person's
// turn — that is the whole difference between an autonomous task and an
// interview.
func TestResolvingQuestionRequiresNoHuman(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run: domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunRunning, CreatedAt: time.Now().Add(-time.Hour)},
		Steps: singleTaskSteps(domain.WorkflowStepRunning, domain.WorkflowStepPending,
			domain.WorkflowStepPending, domain.WorkflowStepPending),
		Questions: []domain.WorkflowQuestion{{
			ID: "wfq-1", State: domain.QuestionStateResolving, QuestionText: "which helper should I use?",
		}},
	}
	a := adviceFor(detail, directBranchPlacement(), workflowcore.RepairPlan{})
	if a.RequiresHuman {
		t.Fatal("a question AO is resolving itself asked for a human")
	}
	if a.AutomaticAction != workflowcore.AutoActionResolveQuestion {
		t.Fatalf("automatic action = %q, want resolve_question", a.AutomaticAction)
	}
}

// A question genuinely addressed to the user still interrupts them. Autonomy is
// not silence.
func TestHumanRequiredQuestionStillInterrupts(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run: domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunRunning, CreatedAt: time.Now().Add(-time.Hour)},
		Steps: singleTaskSteps(domain.WorkflowStepRunning, domain.WorkflowStepPending,
			domain.WorkflowStepPending, domain.WorkflowStepPending),
		Questions: []domain.WorkflowQuestion{{
			ID: "wfq-1", State: domain.QuestionStateHumanRequired,
			QuestionText: "should I drop the production table?",
		}},
	}
	a := adviceFor(detail, directBranchPlacement(), workflowcore.RepairPlan{})
	if !a.RequiresHuman || a.Category != workflowcore.AdviceHumanAction {
		t.Fatalf("a human_required question did not interrupt: %+v", a)
	}
	if a.ReasonCode != workflowcore.ReasonQuestionHumanRequired {
		t.Fatalf("reasonCode = %q, want question_human_required", a.ReasonCode)
	}
}

// §2: the authority a mutating action is revalidated against is carried in the
// advice, so a client can send it back. An advice with no proof is an advice no
// stale click can be refused from.
func TestAdviceCarriesItsAuthorityProof(t *testing.T) {
	detail := stoppedOn(workflowcore.ReasonFixBudgetExhausted)
	detail.Repair = workflowcore.RepairLifecycle{Attempt: 1, Budget: 2}
	a := adviceFor(detail, directBranchPlacement(), workflowcore.RepairPlan{})
	if a.Authority.StopPhase != workflowcore.ReasonFixBudgetExhausted {
		t.Fatalf("authority lost the stop phase: %+v", a.Authority)
	}
	if a.Authority.StopAt.IsZero() {
		t.Fatal("authority lost the stop timestamp")
	}
	if a.Authority.RepairGeneration != 1 {
		t.Fatalf("authority repair generation = %d, want 1", a.Authority.RepairGeneration)
	}
	if a.Authority.PlacementGeneration != 1 {
		t.Fatalf("authority placement generation = %d, want 1", a.Authority.PlacementGeneration)
	}
	if a.Authority.RunState != domain.WorkflowRunNeedsAttention {
		t.Fatalf("authority run state = %q", a.Authority.RunState)
	}
}

// A terminal run is not a recovery question, and nothing about it may invite a
// second execution of work that already ended.
func TestTerminalRunOffersNothingToRecover(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run: domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunCancelled, CreatedAt: time.Now().Add(-time.Hour)},
		Steps: singleTaskSteps(domain.WorkflowStepCompleted, domain.WorkflowStepPending,
			domain.WorkflowStepPending, domain.WorkflowStepPending),
	}
	a := adviceFor(detail, directBranchPlacement(), workflowcore.RepairPlan{})
	if a.Category != workflowcore.AdviceTerminal {
		t.Fatalf("category = %q, want terminal", a.Category)
	}
	if a.RequiresHuman || a.AutomaticAction != workflowcore.AutoActionNone {
		t.Fatalf("a cancelled run asked for something: %+v", a)
	}
	if a.ExpectedNextStage != "" {
		t.Fatalf("a terminal run predicted a next stage %q", a.ExpectedNextStage)
	}
}

// The Advisor never invents a reason vocabulary: every code it emits for a
// classified stop is the canonical one attention.go recorded.
func TestAdviceNeverInventsAReasonCode(t *testing.T) {
	for _, reason := range []string{
		workflowcore.ReasonFixBudgetExhausted, workflowcore.ReasonVerifyBudgetExhausted,
		workflowcore.ReasonPlannerAmbiguous, workflowcore.ReasonProviderAuthRequired,
		workflowcore.ReasonReviewerLaunchFailed, workflowcore.ReasonWorkerBlocked,
		"dirty_worktree",
	} {
		a := adviceFor(stoppedOn(reason), directBranchPlacement(), workflowcore.RepairPlan{})
		if a.ReasonCode != reason {
			t.Errorf("stop %q produced reasonCode %q", reason, a.ReasonCode)
		}
		if a.Summary == "" || a.Explanation == "" {
			t.Errorf("stop %q produced advice with no prose at all: %+v", reason, a)
		}
	}
}

// §13/§14: a repair AO is about to start by itself is not also a button. The
// offer is REFUSED with its reason rather than hidden — one click away from
// authorizing the identical repair is not a choice worth offering.
func TestAPendingAutomaticRepairRefusesTheManualRepairButton(t *testing.T) {
	a := adviceFor(stoppedOn(workflowcore.ReasonVerifyBudgetExhausted), directBranchPlacement(),
		workflowcore.RepairPlan{
			Eligibility: domain.RepairEligible, Mode: domain.RepairModeAutomatic,
			AutomaticAllowed: true, Budget: 2,
		})
	if hasAction(a, workflowcore.ActionRepair) {
		t.Fatalf("Repair was still offered while AO intends to launch it: %+v", a.AvailableActions)
	}
	reason, ok := blockedReason(a, workflowcore.ActionRepair)
	if !ok || reason != "automatic_repair_pending" {
		t.Fatalf("Repair was hidden rather than refused with a reason: %+v", a.BlockedActions)
	}
}

// The converse: under `suggest` the button is the whole point and must survive.
func TestSuggestPolicyKeepsTheRepairButton(t *testing.T) {
	a := adviceFor(stoppedOn(workflowcore.ReasonVerifyBudgetExhausted), directBranchPlacement(),
		workflowcore.RepairPlan{Eligibility: domain.RepairEligible, Mode: domain.RepairModeSuggest, Budget: 2})
	if !hasAction(a, workflowcore.ActionRepair) {
		t.Fatalf("suggest policy lost the Repair button: %+v", a.AvailableActions)
	}
}

// §7, the other half: a run that has PARKED still carries the failed attempt
// rows that produced its stop. Reading them as a failover in progress would
// report "AO is trying the next provider" about a run that has stopped trying.
func TestAParkedRunIsNeverReportedAsAFailoverInProgress(t *testing.T) {
	detail := stoppedOn(workflowcore.ReasonDispatchFailed)
	for i := range detail.Steps {
		if detail.Steps[i].Step.Kind != domain.WorkflowStepWork {
			continue
		}
		detail.Steps[i].Step.State = domain.WorkflowStepReady
		detail.Steps[i].Attempts = []domain.WorkflowAttempt{{
			ID: "wfa-1", Harness: "codex", Outcome: domain.WorkflowAttemptFailed,
			ErrorClass: domain.WorkflowErrorAgentStartFailed,
			StartedAt:  time.Date(2026, 9, 1, 10, 30, 0, 0, time.UTC),
		}}
	}
	a := adviceFor(detail, directBranchPlacement(), workflowcore.RepairPlan{})
	if a.AutomaticAction == workflowcore.AutoActionProviderFailover {
		t.Fatal("a parked run was reported as a failover in progress")
	}
	if !a.RequiresHuman {
		t.Fatalf("a dispatch_failed stop did not read as a person's: %+v", a)
	}
}
