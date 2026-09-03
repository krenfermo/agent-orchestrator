package workflow_test

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// presentation_test.go — P3-A. Every test below asserts a property of the
// HUMAN projection, not of a string: what stage a person is shown, whether the
// UI is told to interrupt them, and which actions AO authorises. The technical
// vocabulary is asserted only where the point is that it survived into the
// secondary detail.

func presentationFor(detail workflowcore.RunDetail, placements []workflowcore.PlacementView, overrides []workflowcore.PlacementOverrideView) workflowcore.Presentation {
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail, Questions: detail.Questions})
	return workflowcore.DerivePresentation(workflowcore.PresentationInput{
		Detail: detail, Lifecycle: life, Placements: placements, Overrides: overrides,
		Now: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
	})
}

func directBranchPlacement() []workflowcore.PlacementView {
	return []workflowcore.PlacementView{{
		Type: domain.PlacementDirectBranch, PlacementGeneration: 1, Current: true,
		State: domain.PlacementActive, Provenance: domain.PlacementFrozenAtSelection,
		RepoPath: "/repo", BaseBranch: "feat/x", ExecutionBranch: "feat/x", MergeTarget: "feat/x",
	}}
}

func isolatedPlacement(state domain.ExecutionPlacementState, integratedSHA string) []workflowcore.PlacementView {
	return []workflowcore.PlacementView{{
		Type: domain.PlacementIsolatedWorktree, PlacementGeneration: 1, Current: true,
		State: state, Provenance: domain.PlacementFrozenAtSelection,
		RepoPath: "/repo", BaseBranch: "main", ExecutionBranch: "ao/wf-1/t1",
		WorktreePath: "/data/ao/worktrees/wf-1", MergeTarget: "main", IntegratedSHA: integratedSHA,
	}}
}

// The stage vocabulary is a projection of the phase, and the two surfaces that
// render it must therefore agree by construction rather than by convention.
func TestStageProjectsEveryPhase(t *testing.T) {
	for phase, want := range map[workflowcore.Phase]workflowcore.Stage{
		workflowcore.PhaseQueued:             workflowcore.StagePreparing,
		workflowcore.PhasePlanning:           workflowcore.StagePlanning,
		workflowcore.PhaseRunning:            workflowcore.StageWorking,
		workflowcore.PhaseReviewing:          workflowcore.StageReviewing,
		workflowcore.PhaseFixing:             workflowcore.StageCorrecting,
		workflowcore.PhaseVerifying:          workflowcore.StageVerifying,
		workflowcore.PhaseWaiting:            workflowcore.StageWaiting,
		workflowcore.PhaseWaitingForCapacity: workflowcore.StageWaiting,
		workflowcore.PhaseBlocked:            workflowcore.StageWaiting,
		workflowcore.PhaseRetrying:           workflowcore.StageWaiting,
		workflowcore.PhaseNeedsAttention:     workflowcore.StageNeedsAttention,
		workflowcore.PhaseCompleted:          workflowcore.StageCompleted,
		workflowcore.PhaseFailed:             workflowcore.StageFailed,
		workflowcore.PhaseCancelled:          workflowcore.StageCancelled,
	} {
		if got := workflowcore.StageForPhase(phase); got != want {
			t.Errorf("StageForPhase(%q) = %q, want %q", phase, got, want)
		}
	}
}

// §9: a direct-branch run has no worktree, so nothing about it may ever ask a
// person whether to integrate or merge. The property is asserted on the
// projection rather than on a screen, because a screen that forgot would be a
// second place to remember it.
func TestDirectBranchNeverRequiresIntegration(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run:   domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunCompleted, CreatedAt: time.Now().Add(-time.Hour)},
		Steps: singleTaskSteps(domain.WorkflowStepCompleted, domain.WorkflowStepCompleted, domain.WorkflowStepPending, domain.WorkflowStepCompleted),
	}
	p := presentationFor(detail, directBranchPlacement(), nil)
	if p.Placement.IntegrationRequired {
		t.Fatal("a completed direct-branch run reported integration as required")
	}
	for _, a := range p.Actions {
		if a.ID == workflowcore.ActionIntegrate {
			t.Fatalf("a direct-branch run offered %q", a.ID)
		}
	}
	for _, st := range p.Progress {
		if st.Stage == workflowcore.StageIntegrating {
			t.Fatal("a direct-branch progression showed an integrating stage")
		}
	}
	if p.Placement.ExecutionBranch != "feat/x" || p.Placement.WorktreePath != "" {
		t.Fatalf("execution location = %+v, want the branch itself and no worktree", p.Placement)
	}
}

// The other half of §26: a verified isolated placement whose work has not
// landed is the one completed state with something still to do.
func TestIsolatedWorktreeCompletionOffersIntegration(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run:   domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunCompleted, CreatedAt: time.Now().Add(-time.Hour)},
		Steps: singleTaskSteps(domain.WorkflowStepCompleted, domain.WorkflowStepCompleted, domain.WorkflowStepPending, domain.WorkflowStepCompleted),
	}
	p := presentationFor(detail, isolatedPlacement(domain.PlacementReviewing, ""), nil)
	if !p.Placement.IntegrationRequired {
		t.Fatal("a verified worktree with nothing integrated reported no integration needed")
	}
	if p.RecommendedAction != workflowcore.ActionIntegrate {
		t.Fatalf("recommended = %q, want integrate", p.RecommendedAction)
	}
	// Already integrated: nothing left to ask for.
	p = presentationFor(detail, isolatedPlacement(domain.PlacementIntegrated, "abc123"), nil)
	if p.Placement.IntegrationRequired {
		t.Fatal("an integrated placement still asked to be integrated")
	}
}

// §10 / §18: AO may offer a worktree as a remedy for a branch queue only when
// it chose the placement itself. An explicit direct-branch choice is a strong
// order, and the offer is shown disabled with its reason rather than removed,
// so a person can see the rule instead of guessing at it.
func TestExplicitDirectBranchIsNotOfferedAWorktreeRemedy(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run:        domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunWaiting, CreatedAt: time.Now().Add(-time.Hour)},
		Steps:      singleTaskSteps(domain.WorkflowStepReady, domain.WorkflowStepPending, domain.WorkflowStepPending, domain.WorkflowStepPending),
		BranchWait: &workflowcore.BranchWait{Branch: "feat/x", RepoPath: "/repo", HeldByWorkflowRunID: "wf-2"},
		WaitReason: "branch_lock",
	}
	explicit := []workflowcore.PlacementOverrideView{{
		Requested: domain.PlacementOverrideDirectBranch, State: domain.PlacementOverrideApplied,
		AppliedGeneration: 1, RequestedBy: "user", Reason: "user_selected_current_branch",
	}}
	p := presentationFor(detail, directBranchPlacement(), explicit)
	if p.Placement.ChosenBy != workflowcore.PlacementChosenByUser {
		t.Fatalf("chosenBy = %q, want user", p.Placement.ChosenBy)
	}
	var found bool
	for _, a := range p.Actions {
		if a.ID != workflowcore.ActionUseIsolatedWorktree {
			continue
		}
		found = true
		if a.Enabled {
			t.Fatal("an explicit direct-branch run was offered an enabled worktree fallback")
		}
		if a.DisabledReason != "placement_explicit" {
			t.Fatalf("disabledReason = %q, want placement_explicit", a.DisabledReason)
		}
	}
	if !found {
		t.Fatal("the worktree remedy was hidden rather than shown disabled with its reason")
	}
	if p.RecommendedAction != workflowcore.ActionWait {
		t.Fatalf("recommended = %q, want wait", p.RecommendedAction)
	}

	// The same wait with an AUTOMATIC placement: now the offer is a real one,
	// because nobody has decided anything a worktree would override.
	p = presentationFor(detail, directBranchPlacement(), nil)
	if p.Placement.ChosenBy != workflowcore.PlacementChosenAutomatically {
		t.Fatalf("chosenBy = %q, want automatic", p.Placement.ChosenBy)
	}
	for _, a := range p.Actions {
		if a.ID == workflowcore.ActionUseIsolatedWorktree && !a.Enabled {
			t.Fatal("an automatic placement refused the worktree remedy it is allowed to offer")
		}
	}
}

// A run with no placement record reports `unknown`, never `automatic`: claiming
// AO chose something it has no record of choosing is the fabrication the whole
// placement model refuses.
func TestUnknownPlacementIsNotReportedAsAutomatic(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run:   domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunRunning, CreatedAt: time.Now()},
		Steps: singleTaskSteps(domain.WorkflowStepRunning, domain.WorkflowStepPending, domain.WorkflowStepPending, domain.WorkflowStepPending),
	}
	p := presentationFor(detail, nil, nil)
	if p.Placement.Known {
		t.Fatal("a run with no placement record reported a known placement")
	}
	if p.Placement.ChosenBy != workflowcore.PlacementChoiceUnknown {
		t.Fatalf("chosenBy = %q, want unknown", p.Placement.ChosenBy)
	}
}

// §5: while a repair is in flight AO is acting, so the projection says
// "correcting", does not claim it is the user's turn, and offers no second
// remedy that could open a duplicate action.
func TestActiveRepairSuppressesAttentionAndDuplicateRemedies(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run:   domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunNeedsAttention, CreatedAt: time.Now().Add(-time.Hour)},
		Steps: singleTaskSteps(domain.WorkflowStepCompleted, domain.WorkflowStepWaiting, domain.WorkflowStepPending, domain.WorkflowStepPending),
		Repair: workflowcore.RepairLifecycle{
			Active: true, Attempt: 1, Budget: 3, RunID: "wf-repair-1",
		},
	}
	p := presentationFor(detail, directBranchPlacement(), nil)
	if p.Stage != workflowcore.StageCorrecting {
		t.Fatalf("stage = %q, want correcting while a repair is in flight", p.Stage)
	}
	if p.RequiresHuman {
		t.Fatal("a run AO is actively repairing claimed it needs a person")
	}
	if !p.AutomaticActionActive {
		t.Fatal("an active repair did not report an automatic action in progress")
	}
	if p.SummaryCode != "repair_active" {
		t.Fatalf("summaryCode = %q, want repair_active", p.SummaryCode)
	}
	if p.RecommendedAction != "" {
		t.Fatalf("recommended = %q, want nothing asked of anyone", p.RecommendedAction)
	}
	for _, a := range p.Actions {
		switch a.ID {
		case workflowcore.ActionContinue, workflowcore.ActionRepair:
			if a.Enabled {
				t.Fatalf("%q was enabled during an active repair", a.ID)
			}
			if a.DisabledReason != "repair_active" {
				t.Fatalf("%q disabledReason = %q, want repair_active", a.ID, a.DisabledReason)
			}
		}
	}
	// The technical account survives underneath.
	if p.Technical.RepairRunID != "wf-repair-1" || p.Technical.RunState != domain.WorkflowRunNeedsAttention {
		t.Fatalf("technical detail lost the durable facts: %+v", p.Technical)
	}
}

// §17: a dirty worktree gets the commit-and-continue flow as its recommendation
// and never a silent stash. The technical reason stays available underneath.
func TestDirtyWorktreeOffersCommitAndContinue(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run:                   domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunNeedsAttention, CreatedAt: time.Now().Add(-time.Hour)},
		Steps:                 singleTaskSteps(domain.WorkflowStepReady, domain.WorkflowStepPending, domain.WorkflowStepPending, domain.WorkflowStepPending),
		StopAuthorityPhase:    "dirty_worktree",
		StopAuthorityAt:       time.Now().Add(-time.Minute),
		LatestCheckpointPhase: "dirty_worktree",
		CheckpointsFolded:     true,
	}
	p := presentationFor(detail, directBranchPlacement(), nil)
	if p.SummaryCode != "dirty_worktree" {
		t.Fatalf("summaryCode = %q, want dirty_worktree", p.SummaryCode)
	}
	if !p.RequiresHuman {
		t.Fatal("a dirty worktree is a person's to clear and must say so")
	}
	if p.RecommendedAction != workflowcore.ActionCommitAndContinue {
		t.Fatalf("recommended = %q, want commit_and_continue", p.RecommendedAction)
	}
	var sawView bool
	for _, a := range p.Actions {
		if a.ID == workflowcore.ActionViewChanges && a.Enabled {
			sawView = true
		}
	}
	if !sawView {
		t.Fatal("the dirty-worktree flow did not offer to show the changes first")
	}
	if p.Technical.AttentionReason != "dirty_worktree" {
		t.Fatalf("technical.attentionReason = %q, want the code preserved", p.Technical.AttentionReason)
	}
}

// §19: a capacity wait is AO queuing, not an error. It gets no Repair button
// and does not claim a person is needed.
func TestCapacityWaitIsNotAnErrorAndOffersNoRepair(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run:        domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunWaiting, CreatedAt: time.Now().Add(-time.Hour)},
		Steps:      singleTaskSteps(domain.WorkflowStepReady, domain.WorkflowStepPending, domain.WorkflowStepPending, domain.WorkflowStepPending),
		WaitReason: "provider_capacity",
		CapacityWait: &workflowcore.CapacityWait{
			Role: domain.WorkflowRoleWorker, Reason: workflowcore.CapacityWaitProviderCooldown,
		},
	}
	p := presentationFor(detail, directBranchPlacement(), nil)
	if p.Stage != workflowcore.StageWaiting {
		t.Fatalf("stage = %q, want waiting", p.Stage)
	}
	if p.RequiresHuman {
		t.Fatal("a capacity wait claimed a person was needed")
	}
	for _, a := range p.Actions {
		if a.ID == workflowcore.ActionRepair {
			t.Fatal("a capacity wait offered Repair")
		}
	}
}

// §2: the progression names a current stage and never fabricates a percentage.
// A stopped run marks WHERE it stopped rather than reporting a bare state.
func TestProgressionMarksCurrentDoneAndBlocked(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run:   domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunRunning, CreatedAt: time.Now().Add(-time.Hour)},
		Steps: singleTaskSteps(domain.WorkflowStepCompleted, domain.WorkflowStepRunning, domain.WorkflowStepPending, domain.WorkflowStepPending),
	}
	p := presentationFor(detail, isolatedPlacement(domain.PlacementActive, ""), nil)
	states := map[workflowcore.Stage]workflowcore.ProgressState{}
	for _, st := range p.Progress {
		states[st.Stage] = st.State
	}
	if states[workflowcore.StageWorking] != workflowcore.ProgressDone {
		t.Fatalf("working = %q, want completed", states[workflowcore.StageWorking])
	}
	if states[workflowcore.StageReviewing] != workflowcore.ProgressCurrent {
		t.Fatalf("reviewing = %q, want current", states[workflowcore.StageReviewing])
	}
	if states[workflowcore.StageVerifying] != workflowcore.ProgressFuture {
		t.Fatalf("verifying = %q, want future", states[workflowcore.StageVerifying])
	}
	if _, ok := states[workflowcore.StageIntegrating]; !ok {
		t.Fatal("an isolated placement's progression omitted the integrating stage")
	}

	stopped := detail
	stopped.Run.State = domain.WorkflowRunNeedsAttention
	stopped.StopAuthorityPhase = "reviewer_launch_failed"
	stopped.CheckpointsFolded = true
	p = presentationFor(stopped, isolatedPlacement(domain.PlacementActive, ""), nil)
	for _, st := range p.Progress {
		if st.Stage == workflowcore.StageReviewing && st.State != workflowcore.ProgressBlocked {
			t.Fatalf("a run stopped during review marked reviewing %q, want blocked", st.State)
		}
	}
}

// §15: the timeline is chronological, bounded and human. It reports what
// happened, with the technical qualifier alongside rather than as the line.
func TestTimelineIsChronologicalAndBounded(t *testing.T) {
	start := time.Date(2026, 9, 1, 10, 32, 0, 0, time.UTC)
	launched := start.Add(time.Minute)
	workDone := start.Add(9 * time.Minute)
	completed := start.Add(20 * time.Minute)
	detail := workflowcore.RunDetail{
		Run: domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunCompleted, CreatedAt: start, CompletedAt: &completed},
		Steps: []workflowcore.StepDetail{
			{
				Step:     domain.WorkflowStep{Kind: domain.WorkflowStepWork, State: domain.WorkflowStepCompleted, CompletedAt: &workDone},
				Attempts: []domain.WorkflowAttempt{{AttemptNumber: 1, Harness: "codex", StartedAt: launched}},
			},
		},
	}
	p := presentationFor(detail, directBranchPlacement(), nil)
	if len(p.Timeline) < 3 {
		t.Fatalf("timeline = %+v, want at least started/launched/completed", p.Timeline)
	}
	for i := 1; i < len(p.Timeline); i++ {
		if p.Timeline[i].At.Before(p.Timeline[i-1].At) {
			t.Fatalf("timeline is out of order at %d: %+v", i, p.Timeline)
		}
		if p.Timeline[i].At.IsZero() {
			t.Fatalf("timeline entry %d has no timestamp: %+v", i, p.Timeline[i])
		}
	}
	if p.Timeline[0].Kind != workflowcore.TimelineStarted {
		t.Fatalf("first entry = %q, want started", p.Timeline[0].Kind)
	}
	if p.Timeline[len(p.Timeline)-1].Kind != workflowcore.TimelineCompleted {
		t.Fatalf("last entry = %q, want completed", p.Timeline[len(p.Timeline)-1].Kind)
	}
}

// §30-H: a spent repair budget is the one repair state that IS a person's, and
// it says so with an explanation rather than an exhausted Repair button.
func TestRepairExhaustedIsActionableAndOffersNoRepair(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run:   domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunNeedsAttention, CreatedAt: time.Now().Add(-time.Hour)},
		Steps: singleTaskSteps(domain.WorkflowStepCompleted, domain.WorkflowStepWaiting, domain.WorkflowStepPending, domain.WorkflowStepPending),
		Repair: workflowcore.RepairLifecycle{
			Active: false, Attempt: 3, Budget: 3, Exhausted: true, RunID: "wf-repair-3",
		},
		StopAuthorityPhase: "workflow_repair_escalated",
		CheckpointsFolded:  true,
	}
	p := presentationFor(detail, directBranchPlacement(), nil)
	if !p.RequiresHuman {
		t.Fatal("an exhausted repair budget did not report that a person is needed")
	}
	if p.AutomaticActionActive {
		t.Fatal("an exhausted repair still claimed AO was acting on it")
	}
	for _, a := range p.Actions {
		if a.ID == workflowcore.ActionRepair {
			if a.Enabled {
				t.Fatal("Repair was offered after the budget was spent")
			}
			if a.DisabledReason != "repair_exhausted" {
				t.Fatalf("Repair disabledReason = %q, want repair_exhausted", a.DisabledReason)
			}
		}
	}
	// The explanation a person acts on is the daemon's own sentence, carried
	// into the technical detail so the UI has copy even for a reason it has
	// never seen.
	if p.Technical.AttentionDetail == "" {
		t.Fatal("an exhausted repair gave the user no explanation to act on")
	}
}

// §30-K: a provider that rejected its credentials is a sign-in, not a retry and
// not a repair.
func TestAuthRequiredRecommendsSigningIn(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run:                domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunNeedsAttention, CreatedAt: time.Now().Add(-time.Hour)},
		Steps:              singleTaskSteps(domain.WorkflowStepReady, domain.WorkflowStepPending, domain.WorkflowStepPending, domain.WorkflowStepPending),
		StopAuthorityPhase: workflowcore.ReasonProviderAuthRequired,
		CheckpointsFolded:  true,
	}
	p := presentationFor(detail, directBranchPlacement(), nil)
	if p.RecommendedAction != workflowcore.ActionAuthenticate {
		t.Fatalf("recommended = %q, want authenticate (reason %q)", p.RecommendedAction, p.SummaryCode)
	}
	for _, a := range p.Actions {
		if a.ID == workflowcore.ActionRepair {
			t.Fatal("an authentication stop offered Repair")
		}
	}
}

// §23 / §30-M: restart durability. DerivePresentation is a pure function of
// durable rows, so a daemon that has just booted and re-read them must produce
// exactly the projection the previous process did.
//
// The test asserts the property that MAKES that true rather than simulating a
// restart: two independent derivations from the same durable facts are equal,
// and nothing in the projection is carried in process memory between them.
func TestPresentationIsReproducibleFromDurableFactsAlone(t *testing.T) {
	stop := time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC)
	detail := workflowcore.RunDetail{
		Run: domain.WorkflowRun{
			ID: "wf-1", ProjectID: "p1", State: domain.WorkflowRunNeedsAttention,
			CreatedAt: stop.Add(-time.Hour), UpdatedAt: stop,
		},
		Steps: []workflowcore.StepDetail{
			{
				Step: domain.WorkflowStep{
					Kind: domain.WorkflowStepWork, State: domain.WorkflowStepCompleted,
					CompletedAt: &stop,
				},
				Attempts: []domain.WorkflowAttempt{{AttemptNumber: 1, Harness: "codex", StartedAt: stop.Add(-30 * time.Minute)}},
			},
			step(domain.WorkflowStepReview, domain.WorkflowStepWaiting),
			step(domain.WorkflowStepFix, domain.WorkflowStepPending),
			step(domain.WorkflowStepVerify, domain.WorkflowStepPending),
		},
		StopAuthorityPhase: "dirty_worktree",
		StopAuthorityAt:    stop,
		CheckpointsFolded:  true,
	}
	placements := directBranchPlacement()
	overrides := []workflowcore.PlacementOverrideView{{
		Requested: domain.PlacementOverrideDirectBranch, State: domain.PlacementOverrideApplied,
		AppliedGeneration: 1, RequestedBy: "user", Reason: "user_selected_current_branch",
	}}

	first := presentationFor(detail, placements, overrides)
	second := presentationFor(detail, placements, overrides)

	if first.Stage != second.Stage || first.SummaryCode != second.SummaryCode ||
		first.RequiresHuman != second.RequiresHuman || first.RecommendedAction != second.RecommendedAction {
		t.Fatalf("the projection changed between two reads of the same rows:\n%+v\n%+v", first, second)
	}
	if len(first.Progress) != len(second.Progress) || len(first.Timeline) != len(second.Timeline) ||
		len(first.Actions) != len(second.Actions) {
		t.Fatal("the progression, timeline or action list changed between two reads of the same rows")
	}
	for i := range first.Progress {
		if first.Progress[i] != second.Progress[i] {
			t.Fatalf("progression entry %d differs between reads: %+v vs %+v", i, first.Progress[i], second.Progress[i])
		}
	}
	if first.Placement != second.Placement {
		t.Fatalf("the execution location differs between reads: %+v vs %+v", first.Placement, second.Placement)
	}
	// And the user's explicit choice survives the re-read, which is the half of
	// restart durability that matters most for §7: a reconstructed screen must
	// not forget that the branch was chosen rather than inferred.
	if second.Placement.ChosenBy != workflowcore.PlacementChosenByUser {
		t.Fatalf("chosenBy after re-read = %q, want user", second.Placement.ChosenBy)
	}
}
