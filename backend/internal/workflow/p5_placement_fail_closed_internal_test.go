package workflow

import (
	stdctx "context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// p5_placement_fail_closed_internal_test.go — P5: a launch AO cannot place is a
// launch AO does not perform.
//
// P3-A made the frozen placement travel with the launch, so the workspace
// router honours the RUN's decision over the PROJECT's current execution mode.
// It travels as a value, and the EMPTY value means "no frozen placement, route
// by project". frozenPlacementTarget answered that empty value for three
// different things: no placement authority wired (true, and correct), a read
// that failed, and a record it could not understand.
//
// The last two are the defect, and they are silent by construction: a run
// frozen into isolated_worktree, in a project configured for direct_branch, has
// its worker launched into the operator's own checkout on the operator's own
// branch — no worktree, no lock, no record that anything unusual happened. One
// transient store error is enough.
//
// These tests pin all three answers apart.

// failingPlacements is stubPlacements with a read that fails, which is the
// shape of a store hiccup, a locked database, or a cancelled context.
type failingPlacements struct {
	stubPlacements
	err error
}

func (f *failingPlacements) GetLiveExecutionPlacement(stdctx.Context, string, string, string) (domain.ExecutionPlacement, bool, error) {
	return domain.ExecutionPlacement{}, false, f.err
}

func placementTargetFixture(t *testing.T, placements ExecutionPlacements) (*Coordinator, domain.WorkflowRun, domain.WorkflowStep) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	base := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	repo := t.TempDir()
	cfg := domain.ProjectConfig{}
	// Deliberately DIRECT-BRANCH: it is the project setting that makes an
	// empty placement dangerous rather than merely imprecise, because the
	// router's fallback then hands the launch the operator's own checkout.
	cfg.ExecutionMode = domain.ExecutionDirectBranch
	cfg.DefaultBranch = "main"
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
		State: domain.WorkflowStepRunning, ArtifactJSON: "{}", CreatedAt: base, UpdatedAt: base,
	}
	if _, _, err := store.CreateWorkflowRun(ctx, run, []domain.WorkflowStep{step}); err != nil {
		t.Fatal(err)
	}
	c := New(Deps{
		Store: store, Projects: store, Placements: placements,
		Clock: func() time.Time { return base },
	})
	return c, run, step
}

// A placement read that fails must not be reported as "no placement": that is
// the project's answer wearing the run's clothes.
func TestFrozenPlacementTargetFailsClosedWhenThePlacementCannotBeRead(t *testing.T) {
	boom := errors.New("database is locked")
	c, run, step := placementTargetFixture(t, &failingPlacements{err: boom})

	placement, branch, err := c.frozenPlacementTarget(stdctx.Background(), run, step)
	if err == nil {
		t.Fatal("frozenPlacementTarget returned no error for an unreadable placement; the launch would have been routed by project configuration")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the underlying cause preserved", err)
	}
	if placement != "" || branch != "" {
		t.Fatalf("got (%q, %q) alongside the error, want empty", placement, branch)
	}
}

// A record this build cannot read is not an invitation to use the project's.
func TestFrozenPlacementTargetFailsClosedOnAnUnknownPlacementType(t *testing.T) {
	c, run, step := placementTargetFixture(t, &stubPlacements{
		found: true,
		placement: domain.ExecutionPlacement{
			WorkflowRunID: "wf-1", Type: domain.ExecutionPlacementType("quantum_worktree"),
			PlacementGeneration: 7, State: domain.PlacementActive,
			RepoPath: t.TempDir(), ExecutionBranch: "ao/x", MergeTarget: "main",
		},
	})

	_, _, err := c.frozenPlacementTarget(stdctx.Background(), run, step)
	if !errors.Is(err, ErrPlacementNotEnforceable) {
		t.Fatalf("err = %v, want ErrPlacementNotEnforceable", err)
	}
	// The generation has to be nameable: it is what an operator inspects.
	if !strings.Contains(err.Error(), "generation 7") {
		t.Fatalf("err = %v, want the placement generation named", err)
	}
}

// The one legitimate empty answer, kept: a deployment with no placement
// authority wired never had a freeze to honour, and routing by project is its
// whole contract.
func TestFrozenPlacementTargetStillReportsEmptyWhenPlacementsAreNotWired(t *testing.T) {
	c, run, step := placementTargetFixture(t, nil)

	placement, branch, err := c.frozenPlacementTarget(stdctx.Background(), run, step)
	if err != nil {
		t.Fatalf("frozenPlacementTarget = %v, want no error when no placement authority is wired", err)
	}
	if placement != "" || branch != "" {
		t.Fatalf("got (%q, %q), want the empty placement", placement, branch)
	}
}

// The classification the dispatcher acts on. A placement AO cannot enforce is
// never retried into a launch and never failed over to another provider: where
// the work belongs does not depend on who performs it.
func TestPlacementFailuresAreClassifiedAsUnretryable(t *testing.T) {
	for name, err := range map[string]error{
		"not enforceable": ErrPlacementNotEnforceable,
		"unprovable":      ErrPlacementUnprovable,
	} {
		t.Run(name, func(t *testing.T) {
			cls := classifyWorkerLaunchFailure(err)
			if cls.Retryable {
				t.Fatal("a placement failure was classified retryable; waiting does not produce a placement")
			}
			if cls.Class != domain.WorkflowErrorInvalidPlacement {
				t.Fatalf("class = %s, want invalid_placement", cls.Class)
			}
			if cls.Reason != ReasonPlacementUnenforceable {
				t.Fatalf("reason = %s, want %s", cls.Reason, ReasonPlacementUnenforceable)
			}
			if cls.Certainty != CertaintyActual {
				t.Fatalf("certainty = %s, want actual: this is decided by a sentinel, not by parsing text", cls.Certainty)
			}
		})
	}
}

// Every stop AO can reach must have a human action registered for it, or the
// run parks with a reason nobody can act on.
func TestPlacementUnenforceableIsANamedStop(t *testing.T) {
	d, ok := attentionDispositions[ReasonPlacementUnenforceable]
	if !ok {
		t.Fatal("placement_unenforceable has no disposition; a run would park with an unexplained reason")
	}
	if d.SelfRemediable {
		t.Fatal("placement_unenforceable is not self-remediable: nothing about waiting establishes a placement")
	}
	if d.HumanAction == "" {
		t.Fatal("placement_unenforceable has no human action")
	}
}

// blindOverrides is the override authority with an unreadable outstanding
// request: a store hiccup, a locked database, a cancelled context.
type blindOverrides struct {
	PlacementOverrides
	err error
}

func (b *blindOverrides) GetOutstandingPlacementOverride(stdctx.Context, string, string, string) (domain.ExecutionPlacementOverride, bool, error) {
	return domain.ExecutionPlacementOverride{}, false, b.err
}

// An unreadable override store must not be reported as "no override".
//
// The soft read this replaces was defended on the grounds that a missed
// override produces the placement AO would have chosen anyway. That is exactly
// backwards: an operator writes an override precisely BECAUSE policy's answer
// is not the one they want, so a failed read does not degrade the answer, it
// inverts it -- and the freeze is written once, so there is no later pass that
// notices. Here the project says direct_branch, so a swallowed error would
// freeze the operator's own checkout for a task whose request is unread.
func TestFreezeFailsWhenTheOverrideRequestCannotBeRead(t *testing.T) {
	boom := errors.New("database is locked")
	store := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	base := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	cfg := domain.ProjectConfig{}
	cfg.ExecutionMode = domain.ExecutionDirectBranch
	cfg.DefaultBranch = "main"
	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID: "p", Path: t.TempDir(), RegisteredAt: base, Config: cfg,
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
		State: domain.WorkflowStepReady, ArtifactJSON: "{}", CreatedAt: base, UpdatedAt: base,
	}
	if _, _, err := store.CreateWorkflowRun(ctx, run, []domain.WorkflowStep{step}); err != nil {
		t.Fatal(err)
	}
	c := New(Deps{
		Store: store, Projects: store, Placements: store,
		PlacementOverrides: &blindOverrides{PlacementOverrides: store, err: boom},
		Clock:              func() time.Time { return base },
	})

	_, ok, err := c.EnsureExecutionPlacement(ctx, run, step)
	if err == nil {
		t.Fatal("the freeze succeeded with an unreadable override store; an operator's explicit request was silently replaced by project policy")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the underlying cause preserved", err)
	}
	if ok {
		t.Fatal("a failed freeze reported a usable placement")
	}
	// And nothing was frozen: the next pass gets to decide with a working store
	// rather than inheriting a record made without the request.
	if _, found, gerr := store.GetLiveExecutionPlacement(ctx, run.ID, "", ""); gerr != nil || found {
		t.Fatalf("a placement was frozen anyway: found=%v err=%v", found, gerr)
	}
}
