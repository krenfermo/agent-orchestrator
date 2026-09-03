package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
)

// p3c_placement_semantics_internal_test.go — P3-C §28.
//
// THE DEFECT. A run may be placed on the user's own branch explicitly inside a
// project whose DEFAULT execution mode is isolated. Admission already takes the
// branch lock for it, because it reads the run's placement — so the run really
// does hold a lock and really does write on somebody's real branch. Everything
// after the launch, though, asked the PROJECT: autonomousLocalCommit returned
// early for any project not in direct-branch mode, so that run finished with
// its work uncommitted on a real branch and — worse — with nothing on the
// ledger saying so.
//
// These tests state the rule as a property: direct-branch semantics follow the
// RUN's placement, and the project's default is consulted only when the run has
// no placement record of its own.

// stubPlacements answers with one frozen placement, or with none.
type stubPlacements struct {
	placement domain.ExecutionPlacement
	found     bool
}

func (s *stubPlacements) FreezeExecutionPlacement(stdctx.Context, domain.ExecutionPlacement) (bool, error) {
	return false, nil
}

func (s *stubPlacements) GetLiveExecutionPlacement(stdctx.Context, string, string, string) (domain.ExecutionPlacement, bool, error) {
	return s.placement, s.found, nil
}

func (s *stubPlacements) GetExecutionPlacement(stdctx.Context, string, string, string, int64) (domain.ExecutionPlacement, bool, error) {
	return s.placement, s.found, nil
}

func (s *stubPlacements) MaxExecutionPlacementGeneration(stdctx.Context, string, string, string) (int64, error) {
	return s.placement.PlacementGeneration, nil
}

func (s *stubPlacements) TransitionExecutionPlacement(stdctx.Context, string, string, string, int64, domain.ExecutionPlacementState, domain.ExecutionPlacementState, string, string, time.Time) (bool, error) {
	return false, nil
}

func (s *stubPlacements) RecordExecutionPlacementPreparation(stdctx.Context, string, string, string, int64, string, string, string, time.Time) (bool, error) {
	return false, nil
}

func (s *stubPlacements) MarkExecutionPlacementIntegrated(stdctx.Context, string, string, string, int64, string, time.Time) (bool, error) {
	return false, nil
}

func (s *stubPlacements) RetireSupersededExecutionPlacements(stdctx.Context, string, string, string, int64, string, time.Time) (int64, error) {
	return 0, nil
}

func (s *stubPlacements) ListExecutionPlacementsForRun(stdctx.Context, string) ([]domain.ExecutionPlacement, error) {
	if !s.found {
		return nil, nil
	}
	return []domain.ExecutionPlacement{s.placement}, nil
}

// stubBranchLocks reports the locks a run holds and records nothing else.
type stubBranchLocks struct{ held []domain.BranchLock }

func (s *stubBranchLocks) Acquire(stdctx.Context, BranchLockRequest) ([]domain.BranchLock, error) {
	return s.held, nil
}
func (s *stubBranchLocks) ReleaseRun(stdctx.Context, string, string) (int64, error) { return 0, nil }
func (s *stubBranchLocks) HeldByRun(stdctx.Context, string) ([]domain.BranchLock, error) {
	return s.held, nil
}
func (s *stubBranchLocks) Renew(stdctx.Context, string, string, string) {}
func (s *stubBranchLocks) RecoverStale(stdctx.Context, string) (int64, error) {
	return 0, nil
}

// recordingCommitter remembers every repository it was asked to commit.
type recordingCommitter struct{ committed []string }

func (r *recordingCommitter) CommitAll(_ stdctx.Context, info ports.WorkspaceInfo, _ string) (string, bool, error) {
	r.committed = append(r.committed, info.RepoPath)
	return "sha-" + info.RepoPath, true, nil
}

func placementSemanticsFixture(t *testing.T, projectMode domain.ExecutionMode, placementType domain.ExecutionPlacementType, hasPlacement bool) (
	*Coordinator, domain.WorkflowRun, domain.WorkflowStep, *recordingCommitter,
) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	repo := t.TempDir()
	cfg := domain.ProjectConfig{}
	cfg.ExecutionMode = projectMode
	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID: "p", Path: repo, RegisteredAt: base, Config: cfg,
	}); err != nil {
		t.Fatal(err)
	}
	run := domain.WorkflowRun{
		ID: "wf-1", ProjectID: "p", Objective: "do the thing",
		State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}",
		CreatedAt: base, UpdatedAt: base,
	}
	step := domain.WorkflowStep{
		ID: "wfs-work", WorkflowRunID: run.ID, Kind: domain.WorkflowStepWork, Ordinal: 1,
		State: domain.WorkflowStepCompleted, ArtifactJSON: "{}", CreatedAt: base, UpdatedAt: base,
	}
	if _, _, err := store.CreateWorkflowRun(ctx, run, []domain.WorkflowStep{step}); err != nil {
		t.Fatal(err)
	}
	committer := &recordingCommitter{}
	c := New(Deps{
		Store: store, Projects: store,
		Placements: &stubPlacements{
			found: hasPlacement,
			placement: domain.ExecutionPlacement{
				WorkflowRunID: run.ID, Type: placementType, PlacementGeneration: 1,
				State: domain.PlacementActive, RepoPath: repo, BaseBranch: "feat/x",
				ExecutionBranch: "feat/x", MergeTarget: "feat/x",
			},
		},
		BranchLocks: &stubBranchLocks{held: []domain.BranchLock{{
			ID: "bl-1", ProjectID: "p", RepoPath: repo, Branch: "feat/x", WorkflowRunID: run.ID,
		}}},
		WorkspaceCommitter: committer,
		Clock:              func() time.Time { return base },
	})
	return c, run, step, committer
}

// The headline: an EXPLICIT direct-branch placement inside an isolated-default
// project commits its work. Before P3-C it silently did not.
func TestExplicitDirectBranchCommitsInsideAnIsolatedDefaultProject(t *testing.T) {
	c, run, step, committer := placementSemanticsFixture(t,
		domain.ExecutionIsolatedWorktree, domain.PlacementDirectBranch, true)
	if err := c.autonomousLocalCommit(stdctx.Background(), run, step); err != nil {
		t.Fatalf("autonomous commit: %v", err)
	}
	if len(committer.committed) != 1 {
		t.Fatalf("%d repositories committed, want 1 — the run's own placement was ignored in favour of the project default",
			len(committer.committed))
	}
}

// And the converse, which is the same rule seen from the other side: an
// ISOLATED placement inside a direct-branch project does not commit the user's
// branch, however the project is configured.
func TestIsolatedPlacementDoesNotCommitInsideADirectBranchProject(t *testing.T) {
	c, run, step, committer := placementSemanticsFixture(t,
		domain.ExecutionDirectBranch, domain.PlacementIsolatedWorktree, true)
	if err := c.autonomousLocalCommit(stdctx.Background(), run, step); err != nil {
		t.Fatalf("autonomous commit: %v", err)
	}
	if len(committer.committed) != 0 {
		t.Fatalf("an isolated placement committed %d repositories on the project's default", len(committer.committed))
	}
}

// The fallback, and its ordering: a run with NO placement record has nothing
// of its own to derive from, and only then does the project's mode answer.
func TestProjectModeIsOnlyTheFallbackForARunWithNoPlacement(t *testing.T) {
	c, run, step, committer := placementSemanticsFixture(t,
		domain.ExecutionDirectBranch, domain.PlacementDirectBranch, false)
	if !c.runPlacementIsDirectBranch(stdctx.Background(), run) {
		t.Fatal("a legacy run in a direct-branch project did not fall back to the project mode")
	}
	if err := c.autonomousLocalCommit(stdctx.Background(), run, step); err != nil {
		t.Fatalf("autonomous commit: %v", err)
	}
	if len(committer.committed) != 1 {
		t.Fatalf("%d repositories committed for a legacy direct-branch run, want 1", len(committer.committed))
	}

	c2, run2, _, _ := placementSemanticsFixture(t,
		domain.ExecutionIsolatedWorktree, domain.PlacementDirectBranch, false)
	if c2.runPlacementIsDirectBranch(stdctx.Background(), run2) {
		t.Fatal("a legacy run in an isolated project was read as direct-branch")
	}
}

// The placement answer is a property of the RUN, asserted directly, so a future
// caller that needs it cannot reach for the project by accident.
func TestRunPlacementIsDirectBranchDerivesFromThePlacement(t *testing.T) {
	for _, tc := range []struct {
		name        string
		projectMode domain.ExecutionMode
		placement   domain.ExecutionPlacementType
		want        bool
	}{
		{"explicit direct in isolated project", domain.ExecutionIsolatedWorktree, domain.PlacementDirectBranch, true},
		{"explicit isolated in direct project", domain.ExecutionDirectBranch, domain.PlacementIsolatedWorktree, false},
		{"direct in direct project", domain.ExecutionDirectBranch, domain.PlacementDirectBranch, true},
		{"isolated in isolated project", domain.ExecutionIsolatedWorktree, domain.PlacementIsolatedWorktree, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, run, _, _ := placementSemanticsFixture(t, tc.projectMode, tc.placement, true)
			if got := c.runPlacementIsDirectBranch(stdctx.Background(), run); got != tc.want {
				t.Fatalf("runPlacementIsDirectBranch = %v, want %v", got, tc.want)
			}
		})
	}
}

// autoRecoveryWakeRecorder records every wake reason a stop scheduled.
type autoRecoveryWakeRecorder struct{ reasons []wake.Reason }

func (r *autoRecoveryWakeRecorder) Schedule(_ stdctx.Context, runID domain.WorkflowRunID, _ *domain.WorkflowStepID, reason wake.Reason, _ *time.Time) (wake.Schedule, error) {
	r.reasons = append(r.reasons, reason)
	return wake.Schedule{ID: "wfwk-1", WorkflowRunID: runID, Reason: reason}, nil
}

func (r *autoRecoveryWakeRecorder) WakeNow(_ stdctx.Context, runID domain.WorkflowRunID, _ *domain.WorkflowStepID, reason wake.Reason) (wake.Schedule, error) {
	r.reasons = append(r.reasons, reason)
	return wake.Schedule{ID: "wfwk-1", WorkflowRunID: runID, Reason: reason}, nil
}

func (r *autoRecoveryWakeRecorder) CancelAllForRun(stdctx.Context, domain.WorkflowRunID) (int, error) {
	return 0, nil
}

func (r *autoRecoveryWakeRecorder) NextForRun(stdctx.Context, domain.WorkflowRunID) (*wake.Schedule, error) {
	return nil, nil
}

func (r *autoRecoveryWakeRecorder) scheduledAutoRecovery() bool {
	for _, reason := range r.reasons {
		if reason == wake.ReasonAutoRecovery {
			return true
		}
	}
	return false
}

// P3-C §3: the moment a repairable stop is recorded under an `automatic`
// policy, AO schedules its own recovery — which is what makes automatic repair
// happen while the daemon stays up, instead of at the next restart.
func TestARepairableStopUnderAutomaticPolicySchedulesItsOwnRecovery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		mode   domain.RepairMode
		want   bool
	}{
		{"repairable + automatic", ReasonVerifyBudgetExhausted, domain.RepairModeAutomatic, true},
		{"repairable + suggest", ReasonVerifyBudgetExhausted, domain.RepairModeSuggest, false},
		{"repairable + disabled", ReasonVerifyBudgetExhausted, domain.RepairModeDisabled, false},
		{"not repairable + automatic", ReasonProviderAuthRequired, domain.RepairModeAutomatic, false},
		{"unnameable stop + automatic", "something_nobody_registered", domain.RepairModeAutomatic, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := sqlitetest.MustOpen(t)
			ctx := stdctx.Background()
			base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
			if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: base}); err != nil {
				t.Fatal(err)
			}
			policy := domain.DefaultWorkflowPolicy()
			policy.Repair = domain.DefaultRepairPolicy(base)
			policy.Repair.Mode = tc.mode
			snapshot, err := json.Marshal(policy)
			if err != nil {
				t.Fatal(err)
			}
			run := domain.WorkflowRun{
				ID: "wf-1", ProjectID: "p", Objective: "do the thing",
				State: domain.WorkflowRunNeedsAttention, PolicyVersion: policyVersionV1,
				PolicySnapshot: string(snapshot), CreatedAt: base, UpdatedAt: base,
			}
			step := domain.WorkflowStep{
				ID: "wfs-verify", WorkflowRunID: run.ID, Kind: domain.WorkflowStepVerify, Ordinal: 1,
				State: domain.WorkflowStepFailed, ArtifactJSON: "{}", CreatedAt: base, UpdatedAt: base,
			}
			if _, _, err := store.CreateWorkflowRun(ctx, run, []domain.WorkflowStep{step}); err != nil {
				t.Fatal(err)
			}
			wakes := &autoRecoveryWakeRecorder{}
			var seq int
			c := New(Deps{
				Store: store, Projects: store, WakeScheduler: wakes,
				Clock: func() time.Time { return base },
				NewID: func() string { seq++; return fmt.Sprintf("p3c%d", seq) },
			})
			c.recordAttentionStop(ctx, run, &step.ID, tc.reason, "the stop detail")

			if got := wakes.scheduledAutoRecovery(); got != tc.want {
				t.Fatalf("auto-recovery wake scheduled = %v, want %v (reasons: %v)", got, tc.want, wakes.reasons)
			}
		})
	}
}

// P3-C §17/§26: the automatic-recovery note is BOOKKEEPING, never a stop.
//
// It is written on a run that is parked for its own reason, at the exact moment
// AO starts repairing that reason. If it counted as a stop it would displace
// `verify_budget_exhausted` — and the repair's own eligibility, quiescence and
// convergence rules all read that reason back, so a recovery that worked would
// erase the record of what it was recovering from.
func TestTheAutomaticRecoveryNoteNeverDisplacesTheStopItIsRecovering(t *testing.T) {
	if !isBookkeepingPhase(autoRecoveryDispatchedPhase) {
		t.Fatal("the auto-recovery dispatch note is not classified as bookkeeping")
	}
	if _, registered := attentionDispositions[autoRecoveryDispatchedPhase]; registered {
		t.Fatal("the auto-recovery dispatch note is registered as a stop reason")
	}
}
