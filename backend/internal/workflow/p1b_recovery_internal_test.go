package workflow

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// p1b_recovery_internal_test.go — the pure halves of P1-B: the obligation
// classifier, the repair eligibility rules, the evidence digest, and the plan
// revision identity. They are tested here rather than through a run because
// each is a total function over durable facts, and a total function deserves a
// table rather than a fixture.

func step(kind domain.WorkflowStepKind, state domain.WorkflowStepState) StepDetail {
	return StepDetail{Step: domain.WorkflowStep{ID: "wfs-" + string(kind), Kind: kind, State: state}}
}

func runWithSteps(state domain.WorkflowRunState, steps ...StepDetail) RunDetail {
	return RunDetail{Run: domain.WorkflowRun{ID: "wf-1", State: state}, Steps: steps}
}

// Matrix 1-6: every recoverable phase names exactly one obligation, and the
// obligation says whether resuming discharges it or a person must.
func TestResumeObligationNamesExactlyOneOutstandingObligation(t *testing.T) {
	tests := []struct {
		name      string
		detail    RunDetail
		want      ResumeObligationKind
		automatic bool
	}{
		{
			name: "pending work owes a dispatch",
			detail: runWithSteps(domain.WorkflowRunPending,
				step(domain.WorkflowStepWork, domain.WorkflowStepReady),
				step(domain.WorkflowStepReview, domain.WorkflowStepPending)),
			want: ResumeObligationWorkDispatch, automatic: true,
		},
		{
			name: "a running worker owes an OBSERVATION, never a second launch",
			detail: runWithSteps(domain.WorkflowRunRunning,
				step(domain.WorkflowStepWork, domain.WorkflowStepRunning),
				step(domain.WorkflowStepReview, domain.WorkflowStepPending)),
			want: ResumeObligationWorkObservation, automatic: true,
		},
		{
			name: "completed work with a pending review owes one reviewer",
			detail: runWithSteps(domain.WorkflowRunRunning,
				step(domain.WorkflowStepWork, domain.WorkflowStepCompleted),
				step(domain.WorkflowStepReview, domain.WorkflowStepPending)),
			want: ResumeObligationReviewDispatch, automatic: true,
		},
		{
			name: "a running review owes an observation, never a second reviewer",
			detail: runWithSteps(domain.WorkflowRunRunning,
				step(domain.WorkflowStepWork, domain.WorkflowStepCompleted),
				step(domain.WorkflowStepReview, domain.WorkflowStepRunning)),
			want: ResumeObligationReviewObservation, automatic: true,
		},
		{
			name: "a ready fix step owes one delivery, never a second prompt",
			detail: runWithSteps(domain.WorkflowRunRunning,
				step(domain.WorkflowStepWork, domain.WorkflowStepCompleted),
				step(domain.WorkflowStepReview, domain.WorkflowStepCompleted),
				step(domain.WorkflowStepFix, domain.WorkflowStepReady)),
			want: ResumeObligationFixDelivery, automatic: true,
		},
		{
			name: "a running fix owes an observation",
			detail: runWithSteps(domain.WorkflowRunRunning,
				step(domain.WorkflowStepWork, domain.WorkflowStepCompleted),
				step(domain.WorkflowStepReview, domain.WorkflowStepCompleted),
				step(domain.WorkflowStepFix, domain.WorkflowStepRunning)),
			want: ResumeObligationFixObservation, automatic: true,
		},
		{
			// The ordering defect TestResumeRunningFixNeverDuplicatesThePrompt
			// caught: mid-cycle the review step sits at `waiting` because it is
			// waiting FOR the fix. Reading that as an owed reviewer would send
			// an operator to launch one at the exact moment the outstanding
			// obligation is a fix prompt nobody must send twice.
			name: "a fix in flight outranks the review parked for it",
			detail: runWithSteps(domain.WorkflowRunRunning,
				step(domain.WorkflowStepWork, domain.WorkflowStepCompleted),
				step(domain.WorkflowStepReview, domain.WorkflowStepWaiting),
				step(domain.WorkflowStepFix, domain.WorkflowStepRunning)),
			want: ResumeObligationFixObservation, automatic: true,
		},
		{
			// ...and cycle 1 is unaffected, because its fix step is `pending`.
			name: "a pending fix step does not mask the first review dispatch",
			detail: runWithSteps(domain.WorkflowRunRunning,
				step(domain.WorkflowStepWork, domain.WorkflowStepCompleted),
				step(domain.WorkflowStepReview, domain.WorkflowStepPending),
				step(domain.WorkflowStepFix, domain.WorkflowStepPending)),
			want: ResumeObligationReviewDispatch, automatic: true,
		},
		{
			name: "an approved review with verification outstanding owes the verify",
			detail: runWithSteps(domain.WorkflowRunRunning,
				step(domain.WorkflowStepWork, domain.WorkflowStepCompleted),
				step(domain.WorkflowStepReview, domain.WorkflowStepCompleted),
				step(domain.WorkflowStepFix, domain.WorkflowStepCompleted),
				step(domain.WorkflowStepVerify, domain.WorkflowStepReady)),
			want: ResumeObligationVerify, automatic: true,
		},
		{
			name: "a finished chain owes nothing",
			detail: runWithSteps(domain.WorkflowRunRunning,
				step(domain.WorkflowStepWork, domain.WorkflowStepCompleted),
				step(domain.WorkflowStepReview, domain.WorkflowStepCompleted),
				step(domain.WorkflowStepFix, domain.WorkflowStepCompleted),
				step(domain.WorkflowStepVerify, domain.WorkflowStepCompleted)),
			want: ResumeObligationNone,
		},
		{
			name:   "a completed run is inert",
			detail: runWithSteps(domain.WorkflowRunCompleted, step(domain.WorkflowStepWork, domain.WorkflowStepCompleted)),
			want:   ResumeObligationTerminal,
		},
		{
			name:   "a cancelled run is inert",
			detail: runWithSteps(domain.WorkflowRunCancelled, step(domain.WorkflowStepWork, domain.WorkflowStepRunning)),
			want:   ResumeObligationTerminal,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveResumeObligation(tt.detail)
			if got.Kind != tt.want {
				t.Fatalf("obligation = %q, want %q", got.Kind, tt.want)
			}
			if got.Automatic != tt.automatic {
				t.Fatalf("automatic = %v, want %v", got.Automatic, tt.automatic)
			}
			if got.Explanation == "" {
				t.Fatal("an obligation with no explanation is one nobody can act on")
			}
			// Total and stable: the same durable facts always name the same
			// obligation, which is what makes a repeated resume idempotent
			// rather than merely usually harmless.
			if again := deriveResumeObligation(tt.detail); again.Kind != got.Kind {
				t.Fatalf("obligation is not deterministic: %q vs %q", got.Kind, again.Kind)
			}
		})
	}
}

// A planned objective's obligations come from its plan, never from work/review
// steps it does not have -- and a plan awaiting MANUAL approval is explicitly
// not AO's to discharge.
func TestPlanResumeObligationSeparatesApprovalFromDispatch(t *testing.T) {
	base := func(status domain.WorkflowPlanStatus, mode domain.WorkflowPlanApprovalMode, tasks ...domain.WorkflowTask) RunDetail {
		return RunDetail{
			Run:   domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunRunning},
			Steps: []StepDetail{step(domain.WorkflowStepPlan, domain.WorkflowStepRunning)},
			Plan:  &domain.WorkflowPlanRecord{Status: status, ApprovalMode: mode, Revision: 1},
			Tasks: tasks,
		}
	}
	tests := []struct {
		name      string
		detail    RunDetail
		want      ResumeObligationKind
		automatic bool
	}{
		{"pending plan owes generation", base(domain.WorkflowPlanPending, domain.WorkflowPlanApprovalAuto), ResumeObligationPlanGeneration, true},
		{"validated + manual is the operator's", base(domain.WorkflowPlanValidated, domain.WorkflowPlanApprovalManual), ResumeObligationPlanApproval, false},
		{"validated + auto is AO's", base(domain.WorkflowPlanValidated, domain.WorkflowPlanApprovalAuto), ResumeObligationPlanApproval, true},
		{
			"approved with live tasks owes dispatch",
			base(domain.WorkflowPlanApproved, domain.WorkflowPlanApprovalAuto,
				domain.WorkflowTask{ID: "wft-1", State: domain.WorkflowTaskEligible}),
			ResumeObligationPlanDispatch, true,
		},
		{
			"approved with every task finished owes convergence",
			base(domain.WorkflowPlanApproved, domain.WorkflowPlanApprovalAuto,
				domain.WorkflowTask{ID: "wft-1", State: domain.WorkflowTaskCompleted}),
			ResumeObligationConvergence, true,
		},
		{"a rejected plan owes no execution", base(domain.WorkflowPlanRejected, domain.WorkflowPlanApprovalManual), ResumeObligationNone, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveResumeObligation(tt.detail)
			if got.Kind != tt.want || got.Automatic != tt.automatic {
				t.Fatalf("obligation = %q automatic=%v, want %q automatic=%v", got.Kind, got.Automatic, tt.want, tt.automatic)
			}
		})
	}
}

// Matrix 14/15/16/18/21: eligibility is deterministic, and the CONDITION is
// checked before any policy or budget -- so no policy setting anywhere can make
// an unrepairable stop repairable.
func TestRepairEligibilityRefusesTheConditionBeforeThePolicy(t *testing.T) {
	repairable := AttentionDisposition{Repairable: true, HumanAction: "x"}
	notRepairable := AttentionDisposition{HumanAction: "x"}
	policy := func(mode domain.RepairMode, budget int) domain.RepairPolicySnapshot {
		return domain.RepairPolicySnapshot{Version: domain.RepairPolicyVersion, Mode: mode, MaxRepairCycles: budget}
	}
	tests := []struct {
		name   string
		disp   AttentionDisposition
		policy domain.RepairPolicySnapshot
		spent  int
		want   domain.RepairEligibility
	}{
		{"repairable, suggest, budget left", repairable, policy(domain.RepairModeSuggest, 2), 0, domain.RepairEligible},
		{"repairable, automatic, budget left", repairable, policy(domain.RepairModeAutomatic, 2), 1, domain.RepairEligible},
		{"repairable but budget spent", repairable, policy(domain.RepairModeAutomatic, 2), 2, domain.RepairBudgetExhausted},
		{"repairable but policy disabled", repairable, policy(domain.RepairModeDisabled, 2), 0, domain.RepairPolicyDisabled},
		// The safety property: an automatic policy and an unspent budget still
		// cannot repair a condition nobody marked repairable.
		{"unrepairable under automatic policy", notRepairable, policy(domain.RepairModeAutomatic, 9), 0, domain.RepairIneligible},
		{"unrepairable under disabled policy", notRepairable, policy(domain.RepairModeDisabled, 9), 0, domain.RepairIneligible},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := repairEligibility(tt.disp, tt.policy, tt.spent); got != tt.want {
				t.Fatalf("eligibility = %q, want %q", got, tt.want)
			}
		})
	}
}

// Matrix 15: the classes P1-B §E forbids repairing are unrepairable in the
// registry itself, which is where the guarantee has to live -- a deny-list
// checked elsewhere is one forgotten entry away from being wrong.
func TestForbiddenRepairClassesAreUnrepairableInTheRegistry(t *testing.T) {
	forbidden := []string{
		ReasonVerifyApprovedHeadUnprovable,
		ReasonVerifyWorkspaceUnattributable,
		ReasonFixGenerationUnprovable,
		ReasonReviewStateAmbiguous,
		ReasonWorkerDispatchAmbiguous,
		ReasonFixDispatchAmbiguous,
		ReasonRecoveryUnreconcilable,
		ReasonReviewerAuthInvalid,
		ReasonProviderAuthRequired,
		ReasonProviderWorkspaceTrustRequired,
		"dirty_worktree",
		ReasonReadOnlyWorkspaceMutated,
		masterIntegrationFailureDurablePhase,
		ReasonPlannerAmbiguous,
	}
	for _, reason := range forbidden {
		disp, ok := attentionDispositions[reason]
		if !ok {
			t.Fatalf("%q is not a registered attention reason", reason)
		}
		if disp.Repairable {
			t.Fatalf("%q is marked repairable; a code-writing agent must never be aimed at it", reason)
		}
	}
	// And the handful that ARE repairable are exactly the technical ones.
	for _, reason := range []string{ReasonFixBudgetExhausted, ReasonVerifyBudgetExhausted, ReasonFixNoVerifiableChange} {
		if !attentionDispositions[reason].Repairable {
			t.Fatalf("%q should be repairable: its remedy is a bounded code change", reason)
		}
	}
	// An unclassified stop can never be repaired, because it is not in the
	// registry at all -- eligibility fails closed on the zero disposition.
	if repairEligibility(AttentionDisposition{}, domain.DefaultRepairPolicy(time.Now()), 0) != domain.RepairIneligible {
		t.Fatal("the zero disposition must never be repairable")
	}
}

// Matrix 19/26: the evidence digest identifies the FAILURE, so the same failure
// re-observed is one repair and a different failure is a different one.
func TestEvidenceDigestIdentifiesTheFailureNotTheObservation(t *testing.T) {
	detail := runWithSteps(domain.WorkflowRunNeedsAttention,
		step(domain.WorkflowStepWork, domain.WorkflowStepCompleted),
		step(domain.WorkflowStepReview, domain.WorkflowStepCompleted),
		step(domain.WorkflowStepVerify, domain.WorkflowStepFailed))

	first, _, _ := evidenceDigestFor(detail, ReasonVerifyBudgetExhausted)
	again, _, _ := evidenceDigestFor(detail, ReasonVerifyBudgetExhausted)
	if first != again {
		t.Fatalf("the same failure produced two digests (%s, %s); a poll or a restart would buy a second repair", first, again)
	}
	other, _, _ := evidenceDigestFor(detail, ReasonFixBudgetExhausted)
	if other == first {
		t.Fatal("two different conditions produced the same digest; distinct failures must be distinct repairs")
	}
	// A digest is an identity, never a disclosure: it is a hex digest and
	// nothing else.
	if len(first) != 32 {
		t.Fatalf("digest = %q, want a 32-character hex digest", first)
	}
}

// Matrix 11/12: revision 1 keeps its exact historical identity (so migration
// 0139 stays a pure ADD COLUMN), and every later revision is distinct.
func TestPlanRevisionIdentityIsBackwardCompatibleAndDistinct(t *testing.T) {
	if got := planStepIDForRevision(1, "model"); got != "model" {
		t.Fatalf("revision 1 step id = %q, want the unchanged historical spelling", got)
	}
	if got := canonicalTaskIDAtRevision("wf-1", 1, "model"); got != canonicalTaskID("wf-1", "model") {
		t.Fatalf("revision 1 task id = %q, want CP9(b)'s existing identity %q", got, canonicalTaskID("wf-1", "model"))
	}
	if got := ordinalForRevision(1, 0); got != 1 {
		t.Fatalf("revision 1 first ordinal = %d, want 1", got)
	}

	if planStepIDForRevision(2, "model") == "model" {
		t.Fatal("revision 2 must not reuse revision 1's plan_step_id: UNIQUE(workflow_run_id, plan_step_id) would swallow it")
	}
	if canonicalTaskIDAtRevision("wf-1", 2, "model") == canonicalTaskIDAtRevision("wf-1", 1, "model") {
		t.Fatal("revision 2 must mint a distinct task identity")
	}
	// Ordinals must not collide either, or UNIQUE(workflow_run_id, ordinal)
	// silently drops the new revision's rows.
	seen := map[int64]struct{}{}
	for rev := int64(1); rev <= 3; rev++ {
		for i := 0; i < MaxPlanSteps; i++ {
			ord := ordinalForRevision(rev, i)
			if _, dup := seen[ord]; dup {
				t.Fatalf("ordinal %d collides across revisions", ord)
			}
			seen[ord] = struct{}{}
		}
	}
	// A store that predates the column reads as revision 1 rather than 0.
	if planRevisionOf(domain.WorkflowPlanRecord{}) != 1 || taskRevisionOf(domain.WorkflowTask{}) != 1 {
		t.Fatal("a pre-0139 row must read as revision 1")
	}
}

// Every registered stop reason must name a recovery action a person can act
// on. `unrecoverable` is reserved for stops AO cannot classify at all, and a
// registered reason reaching it would be AO admitting ignorance about
// something it has already named.
func TestEveryRegisteredStopReasonHasAnActionableRecovery(t *testing.T) {
	for reason, disp := range attentionDispositions {
		action := recoveryActionFor(disp)
		if !action.Valid() {
			t.Fatalf("%q maps to %q, which is not a recovery action", reason, action)
		}
		if action == domain.RecoveryUnrecoverable {
			t.Fatalf("%q is a registered reason but maps to unrecoverable; give it a HumanAction or a Recovery", reason)
		}
	}
}
