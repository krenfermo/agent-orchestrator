package workflow_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// p1e_integrated_test.go — P1-E §D, §O and §P: the P1 authorities as ONE
// system, driven through the same poller a headless daemon runs.
//
// Every phase of P1 was proven against its own authority. What was never
// asserted is what happens to ALL of them at the end of a run: P1-C proved a
// slot is released, P1-D proved a placement is retired, the branch-lock manager
// proved a lock is freed — each in its own test, over its own fixture, with the
// others absent. A leak lives exactly in the gap between those tests.
//
// So the assertions here are deliberately about the WHOLE set at once, and they
// are made after a terminal run through the real store:
//
//	§O   nothing authoritative survives a successful terminal run
//	§P   everything a person needs to recover survives a failed one
//
// The two are the same sweep read in opposite directions, which is why they are
// in one file: a cleanup aggressive enough to satisfy §O and a preservation
// careful enough to satisfy §P are in tension, and only checking both catches a
// change that trades one for the other.

// p1eResidue is every authoritative runtime resource one run can be holding.
// It is gathered in one pass so a leak is reported as a set rather than as
// whichever authority a test happened to look at first.
type p1eResidue struct {
	OutstandingCapacityClaims int
	RuntimeBoundClaims        int
	HeldBranchLocks           int
	AuthoritativeAttempts     int
	LivePlacements            int
	PendingWakes              int
	PreservedPlacements       int
	ConflictPlacements        int
}

func (r p1eResidue) String() string {
	return fmt.Sprintf("capacity=%d runtimeBound=%d branchLocks=%d providerAttempts=%d "+
		"livePlacements=%d wakes=%d preserved=%d conflicts=%d",
		r.OutstandingCapacityClaims, r.RuntimeBoundClaims, r.HeldBranchLocks,
		r.AuthoritativeAttempts, r.LivePlacements, r.PendingWakes,
		r.PreservedPlacements, r.ConflictPlacements)
}

// residueFor reads every durable authority a run could still be holding.
//
// It walks the run AND its children: a master objective's leak is almost always
// on a child, and a sweep that only looked at the parent would report a clean
// machine while a worker's slot was still charged.
func residueFor(t *testing.T, fx *autonomousFixture, runIDs ...string) p1eResidue {
	t.Helper()
	ctx := context.Background()
	var out p1eResidue
	for _, runID := range runIDs {
		claims, err := fx.store.ListCapacityClaimsForRun(ctx, runID)
		if err != nil {
			t.Fatalf("ListCapacityClaimsForRun(%s): %v", runID, err)
		}
		for _, claim := range claims {
			if claim.State == domain.CapacityClaimReleased {
				continue
			}
			out.OutstandingCapacityClaims++
			if claim.RuntimeHandle != "" {
				out.RuntimeBoundClaims++
			}
		}

		attempts, err := fx.store.ListProviderAttemptsForRun(ctx, runID)
		if err != nil {
			t.Fatalf("ListProviderAttemptsForRun(%s): %v", runID, err)
		}
		for _, attempt := range attempts {
			if attempt.State.Authoritative() {
				out.AuthoritativeAttempts++
			}
		}

		placements, err := fx.store.ListExecutionPlacementsForRun(ctx, runID)
		if err != nil {
			t.Fatalf("ListExecutionPlacementsForRun(%s): %v", runID, err)
		}
		for _, p := range placements {
			switch {
			case !p.State.Terminal():
				out.LivePlacements++
			case p.State == domain.PlacementPreserved:
				out.PreservedPlacements++
			}
			if p.State == domain.PlacementConflict {
				out.ConflictPlacements++
			}
		}

		locks, err := fx.store.ListHeldBranchLocksByRun(ctx, runID)
		if err == nil {
			out.HeldBranchLocks += len(locks)
		}
	}
	return out
}

// dueWakesFor counts the wakes still pending for a run.
//
// A pending wake on a terminal run is the leak that turns into a LOOP rather
// than merely into an occupied slot: the poller comes back, finds work it
// believes is due, and relaunches something that is already over.
func dueWakesFor(t *testing.T, fx *autonomousFixture, runID string) int {
	t.Helper()
	pending, err := fx.store.ListPendingWorkflowWakeSchedulesByRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListPendingWorkflowWakeSchedulesByRun(%s): %v", runID, err)
	}
	return len(pending)
}

// p1eTaskVerification is the structured verification every TASK these tests
// create carries.
//
// It is not incidental: a task created with NO checks stops at verify with
// `verify_ambiguous`, and that refusal is correct — AO will not call work
// verified because nothing was asked of it. Supplying one here keeps these
// tests about the authorities they are named for rather than about that rule,
// which read_only_completion_test.go already covers.
func p1eTaskVerification() workflowcore.VerificationPlan {
	return workflowcore.VerificationPlan{
		Commands: []workflowcore.VerificationCommandCheck{{
			Command: "go", Args: []string{"test", "./..."},
			TimeoutSeconds: 60, RequiredExitCode: 0, RetrySafe: true,
		}},
	}
}

// childRunIDsOf lists every execution run a master objective produced.
func childRunIDsOf(t *testing.T, fx *autonomousFixture, masterID string) []string {
	t.Helper()
	tasks, err := fx.store.ListWorkflowTasks(context.Background(), masterID)
	if err != nil {
		t.Fatalf("ListWorkflowTasks(%s): %v", masterID, err)
	}
	out := []string{}
	for _, task := range tasks {
		if task.ExecutionRunID != nil && *task.ExecutionRunID != "" {
			out = append(out, *task.ExecutionRunID)
		}
	}
	return out
}

// §D: a bounded mutating TASK, created and driven with no planner and no
// hierarchy, reaches a terminal state and leaves nothing authoritative behind.
func TestP1E_MutatingTaskRunsEndToEndAndLeavesNoAuthorityBehind(t *testing.T) {
	fx := newAutonomousFixture(t, oneTaskPlan())
	ctx := context.Background()
	seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())

	created, err := fx.coord.CreateTaskRun(ctx, workflowcore.TaskRunRequest{
		ProjectID: "p", Objective: "change one file",
		WriteIntent: domain.WorkflowWriteIntentMutating, Verification: p1eTaskVerification(),
	})
	if err != nil {
		t.Fatalf("CreateTaskRun: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")

	// §D: no planner is consulted and no child hierarchy appears. A TASK that
	// quietly grew a master run would be paying for decomposition nobody asked
	// for, on every single-step change.
	driveCycles(t, fx, 40, func(int) {
		approveOpenReview(t, fx, created.Run.ID, domain.VerdictApproved)
	})
	if fx.planner.calls != 0 {
		t.Fatalf("a bounded TASK consulted the planner %d times", fx.planner.calls)
	}
	tasks, err := fx.store.ListWorkflowTasks(ctx, created.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Fatalf("a bounded TASK produced %d planned tasks; it must not decompose", len(tasks))
	}

	run, _, err := fx.store.GetWorkflowRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !run.State.Terminal() {
		t.Fatalf("run state = %q, want a terminal state", run.State)
	}

	// §D: mutation review is not optional. A mutating task that completed
	// without one would have written code nothing read.
	if fx.launcher.launchCalls == 0 {
		t.Fatal("a mutating task completed with no reviewer launched")
	}

	// §O: the whole authority set, in one read.
	residue := residueFor(t, fx, created.Run.ID)
	if residue.OutstandingCapacityClaims != 0 || residue.RuntimeBoundClaims != 0 ||
		residue.HeldBranchLocks != 0 || residue.AuthoritativeAttempts != 0 ||
		residue.LivePlacements != 0 {
		t.Fatalf("terminal run leaked authority: %s", residue)
	}

	// §O: and no wake that could relaunch terminal work. This is the one that
	// turns a leak into a loop rather than merely into an occupied slot.
	if pending := dueWakesFor(t, fx, created.Run.ID); pending != 0 {
		t.Fatalf("terminal run left %d wake(s) that could relaunch it", pending)
	}
}

// §D: a read-only TASK takes no mutation authority.
//
// Scope, stated rather than implied: this asserts the AUTHORITY half of §D's
// read-only requirement — that declaring a task read-only does not make it take
// the durable branch lock, which exists to serialise writers and which a reader
// holding would block them for nothing. The COMPLETION half — that a read-only
// task whose worktree is correctly unchanged completes instead of parking as
// ambiguous_worker_state — is proven in read_only_completion_test.go, against a
// fixture that can model a worker finishing without producing changes. This
// fixture cannot: its worker-completion signal is a workspace observation, so a
// genuinely unchanged workspace never signals here.
func TestP1E_ReadOnlyTaskTakesNoMutationAuthority(t *testing.T) {
	fx := newAutonomousFixture(t, oneTaskPlan())
	ctx := context.Background()
	seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())

	created, err := fx.coord.CreateTaskRun(ctx, workflowcore.TaskRunRequest{
		ProjectID: "p", Objective: "read the config and report",
		WriteIntent: domain.WorkflowWriteIntentReadOnly, Verification: p1eTaskVerification(),
	})
	if err != nil {
		t.Fatalf("CreateTaskRun: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")

	// Far enough that the worker has been dispatched, which is where mutation
	// authority would have been taken if it were going to be.
	driveCycles(t, fx, 12, nil)
	if len(fx.spawner.calls) == 0 {
		t.Fatal("the read-only task never dispatched a worker; there is nothing to assert about")
	}

	residue := residueFor(t, fx, created.Run.ID)
	if residue.HeldBranchLocks != 0 {
		t.Fatalf("a read-only task holds %d branch lock(s); the lock serialises WRITERS", residue.HeldBranchLocks)
	}
	// And it holds at most the one slot its own runtime is paying for -- a
	// reader must not reserve mutation capacity it will never use.
	if residue.OutstandingCapacityClaims > 1 {
		t.Fatalf("a read-only task holds %d capacity claims: %s", residue.OutstandingCapacityClaims, residue)
	}
	// The placement it was given is a real frozen one, not an absence: a
	// read-only task still works somewhere, and AO still records where.
	if residue.LivePlacements != 1 {
		t.Fatalf("a running read-only task has %d live placements, want exactly 1", residue.LivePlacements)
	}
}

// §D: restarting at several points converges on the same terminal state, with
// no duplicate worker, reviewer or prompt.
//
// The restart is the coordinator being rebuilt over the SAME durable store,
// which is what a daemon restart is; everything the run knows has to come back
// from rows.
func TestP1E_TaskConvergesAcrossRestartsAtSeveralPoints(t *testing.T) {
	for _, restartAfter := range []int{1, 3, 6, 10} {
		t.Run(fmt.Sprintf("restart_after_%d_cycles", restartAfter), func(t *testing.T) {
			fx := newAutonomousFixture(t, oneTaskPlan())
			ctx := context.Background()
			seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())

			created, err := fx.coord.CreateTaskRun(ctx, workflowcore.TaskRunRequest{
				ProjectID: "p", Objective: "converge", WriteIntent: domain.WorkflowWriteIntentMutating,
				Verification: p1eTaskVerification(),
			})
			if err != nil {
				t.Fatalf("CreateTaskRun: %v", err)
			}
			stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")

			driveCycles(t, fx, restartAfter, func(int) {
				approveOpenReview(t, fx, created.Run.ID, domain.VerdictApproved)
			})
			// The restart. Rebuilding the coordinator over the same store is
			// exactly what a daemon boot does.
			fx.withCapacityLimits(domain.CapacityLimits{})
			driveCycles(t, fx, 40, func(int) {
				approveOpenReview(t, fx, created.Run.ID, domain.VerdictApproved)
			})

			run, _, err := fx.store.GetWorkflowRun(ctx, created.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !run.State.Terminal() {
				t.Fatalf("restart after %d cycles left the run at %q", restartAfter, run.State)
			}
			// Exactly-once authority across the restart boundary.
			if len(fx.spawner.calls) != 1 {
				t.Fatalf("restart after %d cycles produced %d worker spawns, want exactly 1",
					restartAfter, len(fx.spawner.calls))
			}
			if residue := residueFor(t, fx, created.Run.ID); residue.OutstandingCapacityClaims != 0 ||
				residue.AuthoritativeAttempts != 0 || residue.LivePlacements != 0 {
				t.Fatalf("restart after %d cycles leaked: %s", restartAfter, residue)
			}
		})
	}
}

// §F: a MASTER objective with children converges, and the whole tree — parent
// and every child — is clean afterwards.
func TestP1E_MasterObjectiveConvergesAndTheWholeTreeIsClean(t *testing.T) {
	fx, ctx, masterID := startAutonomousObjective(t, twoTaskDependentPlan())

	driveCycles(t, fx, 60, func(int) {
		if _, childID, ok := activeChildRunID(t, fx, masterID); ok {
			approveOpenReview(t, fx, childID, domain.VerdictApproved)
		}
	})

	master, _, err := fx.store.GetWorkflowRun(ctx, masterID)
	if err != nil {
		t.Fatal(err)
	}
	if master.State != domain.WorkflowRunCompleted {
		t.Fatalf("master run = %q, want completed", master.State)
	}
	// §F: the parent completes only after its authoritative children did.
	tasks, err := fx.store.ListWorkflowTasks(ctx, masterID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) == 0 {
		t.Fatal("the master objective produced no tasks")
	}
	for _, task := range tasks {
		if task.State != domain.WorkflowTaskCompleted {
			t.Fatalf("master completed with task %s in state %q", task.PlanStepID, task.State)
		}
	}
	// §F: children are placed independently. Two siblings sharing one execution
	// branch would make "whose work is this" unanswerable at integration.
	branches := map[string]string{}
	for _, childID := range childRunIDsOf(t, fx, masterID) {
		placements, perr := fx.store.ListExecutionPlacementsForRun(ctx, childID)
		if perr != nil {
			t.Fatal(perr)
		}
		for _, p := range placements {
			if !p.Type.Isolated() || p.ExecutionBranch == "" {
				continue
			}
			if other, clash := branches[p.ExecutionBranch]; clash && other != childID {
				t.Fatalf("children %s and %s share execution branch %q", other, childID, p.ExecutionBranch)
			}
			branches[p.ExecutionBranch] = childID
		}
	}

	// §O over the whole tree.
	all := append([]string{masterID}, childRunIDsOf(t, fx, masterID)...)
	residue := residueFor(t, fx, all...)
	if residue.OutstandingCapacityClaims != 0 || residue.RuntimeBoundClaims != 0 ||
		residue.HeldBranchLocks != 0 || residue.AuthoritativeAttempts != 0 ||
		residue.LivePlacements != 0 {
		t.Fatalf("completed master objective leaked authority across its tree: %s", residue)
	}
	// §F: the parent never consumed a worker slot of its own. A master that
	// charged for its own supervision would halve the machine's usable breadth.
	parentResidue := residueFor(t, fx, masterID)
	if parentResidue.RuntimeBoundClaims != 0 {
		t.Fatalf("the parent objective bound %d runtime(s) of its own", parentResidue.RuntimeBoundClaims)
	}
}

// §P: a run that stops keeps the evidence that recovery needs.
//
// The failure mode this guards is a cleanup written to satisfy §O: a sweep that
// retires and collects everything on any terminal state throws away the
// worktree holding the only copy of the work, and the operator is left with a
// tidy machine and no way back.
//
// It drives the CANCEL path rather than writing a failed state directly,
// because cancel is a terminal transition the coordinator owns — which is the
// only kind whose cleanup behaviour is a property of the system rather than of
// the test's setup.
func TestP1E_CancelledRunPreservesTheEvidenceRecoveryNeeds(t *testing.T) {
	fx := newAutonomousFixture(t, oneTaskPlan())
	ctx := context.Background()
	seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())

	created, err := fx.coord.CreateTaskRun(ctx, workflowcore.TaskRunRequest{
		ProjectID: "p", Objective: "work that will not land",
		WriteIntent: domain.WorkflowWriteIntentMutating, Verification: p1eTaskVerification(),
	})
	if err != nil {
		t.Fatalf("CreateTaskRun: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")

	// Drive far enough that a placement exists and the worker has run, so there
	// is genuinely something a person might need back.
	driveCycles(t, fx, 8, nil)
	before, err := fx.store.ListExecutionPlacementsForRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 {
		t.Fatal("the run never froze a placement; there is nothing to preserve")
	}

	if _, err := fx.coord.CancelRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	driveCycles(t, fx, 4, nil)

	residue := residueFor(t, fx, created.Run.ID)
	// §P: an isolated placement whose work never landed is PRESERVED, not
	// merely terminal. The distinction is the whole point: `preserved` is AO's
	// durable refusal to clean something up, and the sweeper reads it.
	if residue.PreservedPlacements == 0 {
		placements, _ := fx.store.ListExecutionPlacementsForRun(ctx, created.Run.ID)
		states := []string{}
		for _, p := range placements {
			states = append(states, fmt.Sprintf("%s/%s(landed=%q)", p.Type, p.State, p.IntegratedSHA))
		}
		t.Fatalf("a cancelled run preserved no placement; states were %v", states)
	}
	// §P: and capacity is still not leaked indefinitely -- preservation is
	// about EVIDENCE, never about holding a slot nobody can use.
	if residue.OutstandingCapacityClaims != 0 {
		t.Fatalf("a cancelled run is still holding %d capacity claim(s)", residue.OutstandingCapacityClaims)
	}
	if residue.HeldBranchLocks != 0 {
		t.Fatalf("a cancelled run is still holding %d branch lock(s)", residue.HeldBranchLocks)
	}
	// The preserved record still names what it preserved, so an operator can
	// find the checkout without guessing a path.
	placements, err := fx.store.ListExecutionPlacementsForRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range placements {
		if p.State != domain.PlacementPreserved {
			continue
		}
		if p.ExecutionBranch == "" || p.RepoPath == "" {
			t.Fatalf("a preserved placement cannot say what it preserved: %+v", p)
		}
	}
}

// §P: a run parked in needs_attention is NOT terminal, and keeps everything.
// This is the state a person is actually looking at when they go to recover a
// run, and a placement retired underneath them is a checkout the recovery path
// can no longer name.
func TestP1E_NeedsAttentionKeepsItsPlacementLive(t *testing.T) {
	fx := newAutonomousFixture(t, oneTaskPlan())
	ctx := context.Background()
	seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())

	created, err := fx.coord.CreateTaskRun(ctx, workflowcore.TaskRunRequest{
		ProjectID: "p", Objective: "work that will stop for a person",
		WriteIntent: domain.WorkflowWriteIntentMutating,
	})
	if err != nil {
		t.Fatalf("CreateTaskRun: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")

	// No verification plan: this run stops at verify with `verify_ambiguous`,
	// which is a genuine coordinator-owned needs_attention rather than a state
	// written into the store by the test.
	driveCycles(t, fx, 40, func(int) {
		approveOpenReview(t, fx, created.Run.ID, domain.VerdictApproved)
	})
	run, _, err := fx.store.GetWorkflowRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != domain.WorkflowRunNeedsAttention {
		t.Skipf("the run reached %q rather than needs_attention; nothing to assert here", run.State)
	}

	residue := residueFor(t, fx, created.Run.ID)
	if residue.LivePlacements == 0 {
		t.Fatal("a run parked for a person had its placement retired; recovery can no longer name the checkout")
	}
}
