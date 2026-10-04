package workflow_test

import (
	"context"
	"os"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// p1e_soak_test.go — P1-E §X: a compressed deterministic soak over the
// INTEGRATED P1 system.
//
// P0-D already soaks the durable lifecycle. What it cannot show is the thing
// P1 added: capacity claims, frozen placements, provider attempts, branch
// authority and wakes all being taken and returned, hundreds of times, by runs
// that restart in the middle. A leak in any one of them is a function of
// iteration count, and iteration count is exactly what this buys.
//
// What it can and cannot show, stated as plainly as P0-D's file states it:
//
//   - It CAN find an authority that accumulates: a claim never released, a
//     placement never retired, a provider attempt left authoritative, a wake
//     scheduled on a finished run, a lock nobody frees. Those show up as a
//     non-zero residue on an iteration, naming the iteration they started on.
//   - It CANNOT show anything that is a function of WALL-CLOCK time. The clock
//     is fake, the runtime is a fake, the provider is a fake. A real tmux
//     server degrading over hours, an fd leak, a provider session expiring:
//     none of that is here, and none of it is implied.
//
// This is NOT a wall-clock soak and must never be reported as one.

// The §X counts, and the reduced counts the ordinary suite runs.
//
// The full soak drives a real sqlite store and a real git repository per
// iteration, which under `-race` costs about five minutes on its own — enough
// to push internal/workflow past `go test`'s ten-minute per-package default and
// turn a required gate red on time rather than on correctness.
//
// So the ordinary suite runs a fifth of it, and AO_P1E_SOAK_FULL=1 runs the
// counts §X asks for. That is a deliberate trade and worth being exact about:
// the reduced run still asserts every invariant on every iteration — one spawn,
// one reviewer, one placement, zero residue — so a leak that is a function of
// iteration count is caught the moment it appears, just with less headroom
// above it. The full counts are run explicitly and reported as evidence.
var (
	p1eSoakTaskRuns       = p1eSoakCount(100, 20)
	p1eSoakAutonomousRuns = p1eSoakCount(100, 20)
	p1eSoakMasterRuns     = p1eSoakCount(50, 10)
)

// p1eSoakCount picks the full or the reduced count for one lane.
func p1eSoakCount(full, reduced int) int {
	if os.Getenv("AO_P1E_SOAK_FULL") == "1" {
		return full
	}
	return reduced
}

// p1eSoakTotals is what the soak counts. Every field is either an expected
// outcome or a leak; there is no field whose meaning depends on interpretation.
type p1eSoakTotals struct {
	runs      int
	completed int
	// attentionUnexpected counts runs that stopped for a person when nothing
	// in the soak asked them to. It is a separate counter rather than a share
	// of a total because "some runs needed attention" is only meaningful when
	// you know which ones were supposed to.
	attentionUnexpected int

	workerSpawns    int
	reviewerLaunche int
	fixPrompts      int

	leakedCapacity   int
	leakedRuntimes   int
	leakedLocks      int
	leakedPlacements int
	leakedAttempts   int
	leakedWakes      int

	duplicateSpawns  int
	preservedRecords int
}

// assertNoResidue folds one finished run's authority residue into the totals,
// and fails immediately naming the iteration rather than letting a leak show up
// as a wrong grand total nobody can locate.
func (tot *p1eSoakTotals) assertNoResidue(t *testing.T, label string, iteration int, residue p1eResidue) {
	t.Helper()
	tot.leakedCapacity += residue.OutstandingCapacityClaims
	tot.leakedRuntimes += residue.RuntimeBoundClaims
	tot.leakedLocks += residue.HeldBranchLocks
	tot.leakedPlacements += residue.LivePlacements
	tot.leakedAttempts += residue.AuthoritativeAttempts
	tot.preservedRecords += residue.PreservedPlacements
	if residue.OutstandingCapacityClaims != 0 || residue.RuntimeBoundClaims != 0 ||
		residue.HeldBranchLocks != 0 || residue.LivePlacements != 0 ||
		residue.AuthoritativeAttempts != 0 {
		t.Fatalf("%s iteration %d leaked authority: %s", label, iteration, residue)
	}
}

// TestP1E_SoakTaskRuns drives bounded TASK runs: the shape a single-step change
// takes, with a restart folded into every iteration.
func TestP1E_SoakTaskRuns(t *testing.T) {
	ctx := context.Background()
	var totals p1eSoakTotals

	for i := 0; i < p1eSoakTaskRuns; i++ {
		fx := newAutonomousFixture(t, oneTaskPlan())
		seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())
		created, err := fx.coord.CreateTaskRun(ctx, workflowcore.TaskRunRequest{
			ProjectID: "p", Objective: "soak task", Verification: p1eTaskVerification(),
			WriteIntent: domain.WorkflowWriteIntentMutating,
		})
		if err != nil {
			t.Fatalf("iteration %d: CreateTaskRun: %v", i, err)
		}
		stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")

		// Half the iterations restart mid-flight. Alternating rather than
		// randomising keeps the soak deterministic, which is the whole point:
		// a failure has to be reproducible from its iteration number alone.
		approve := func(int) { approveOpenReview(t, fx, created.Run.ID, domain.VerdictApproved) }
		if i%2 == 0 {
			driveCycles(t, fx, 4, approve)
			fx.withCapacityLimits(domain.CapacityLimits{})
		}
		driveCycles(t, fx, 40, approve)

		run, _, err := fx.store.GetWorkflowRun(ctx, created.Run.ID)
		if err != nil {
			t.Fatalf("iteration %d: GetWorkflowRun: %v", i, err)
		}
		totals.runs++
		switch run.State {
		case domain.WorkflowRunCompleted:
			totals.completed++
		case domain.WorkflowRunNeedsAttention:
			totals.attentionUnexpected++
			t.Fatalf("iteration %d: an approved bounded task stopped for a person", i)
		default:
			t.Fatalf("iteration %d: run state = %q, want a terminal state", i, run.State)
		}

		// Exactly-once authority, per iteration, across the restart.
		if len(fx.spawner.calls) != 1 {
			totals.duplicateSpawns += len(fx.spawner.calls) - 1
			t.Fatalf("iteration %d: %d worker spawns, want exactly 1", i, len(fx.spawner.calls))
		}
		if fx.launcher.launchCalls != 1 {
			t.Fatalf("iteration %d: %d reviewer launches, want exactly 1", i, fx.launcher.launchCalls)
		}
		totals.workerSpawns += len(fx.spawner.calls)
		totals.reviewerLaunche += fx.launcher.launchCalls
		totals.fixPrompts += fx.sender.calls
		totals.leakedWakes += dueWakesFor(t, fx, created.Run.ID)

		totals.assertNoResidue(t, "task", i, residueFor(t, fx, created.Run.ID))
	}

	if totals.completed != p1eSoakTaskRuns {
		t.Fatalf("%d of %d task runs completed", totals.completed, p1eSoakTaskRuns)
	}
	if totals.leakedWakes != 0 {
		t.Fatalf("%d wake(s) survived terminal task runs", totals.leakedWakes)
	}
	t.Logf("P1-E soak (TASK, full=%t): %d runs, %d completed, %d spawns, %d reviewer launches, %d fix prompts; "+
		"leaks: capacity=%d runtimes=%d locks=%d placements=%d attempts=%d wakes=%d; duplicates=%d",
		os.Getenv("AO_P1E_SOAK_FULL") == "1",
		totals.runs, totals.completed, totals.workerSpawns, totals.reviewerLaunche, totals.fixPrompts,
		totals.leakedCapacity, totals.leakedRuntimes, totals.leakedLocks, totals.leakedPlacements,
		totals.leakedAttempts, totals.leakedWakes, totals.duplicateSpawns)
}

// TestP1E_SoakAutonomousRuns drives single-task AUTONOMOUS objectives, a third
// of them through a full changes-requested -> fix -> approve cycle.
//
// The fix lane matters most here for the reason P0-D gives: a duplicate there
// is not a wasted launch but a second copy of the reviewer's findings pasted
// into a live composer.
func TestP1E_SoakAutonomousRuns(t *testing.T) {
	ctx := context.Background()
	var totals p1eSoakTotals

	for i := 0; i < p1eSoakAutonomousRuns; i++ {
		fx, _, masterID := startAutonomousObjective(t, oneTaskPlan())
		wantFixCycle := i%3 == 0
		requested := false
		fixSeen := false

		before := func(int) {
			_, childID, ok := activeChildRunID(t, fx, masterID)
			if !ok {
				return
			}
			if wantFixCycle && !requested {
				if approveOpenReview(t, fx, childID, domain.VerdictChangesRequested) {
					requested = true
				}
				return
			}
			if wantFixCycle && requested && !fixSeen && fx.sender.calls > 0 {
				// The worker genuinely responds to the findings, so the fix
				// cycle can resolve rather than parking.
				fx.ws.obs.Changes = append(fx.ws.obs.Changes, workspaceChangeForSoak())
				fixSeen = true
				return
			}
			approveOpenReview(t, fx, childID, domain.VerdictApproved)
		}

		// Every fourth objective restarts mid-flight.
		if i%4 == 0 {
			driveCycles(t, fx, 6, before)
			fx.withCapacityLimits(domain.CapacityLimits{})
		}
		driveCycles(t, fx, 60, before)

		run, _, err := fx.store.GetWorkflowRun(ctx, masterID)
		if err != nil {
			t.Fatalf("iteration %d: GetWorkflowRun: %v", i, err)
		}
		totals.runs++
		switch run.State {
		case domain.WorkflowRunCompleted:
			totals.completed++
		case domain.WorkflowRunNeedsAttention:
			totals.attentionUnexpected++
			t.Fatalf("iteration %d: an autonomous objective stopped for a person", i)
		default:
			t.Fatalf("iteration %d: run state = %q, want completed", i, run.State)
		}
		if wantFixCycle && fx.sender.calls == 0 {
			t.Fatalf("iteration %d: a changes_requested cycle delivered no fix prompt", i)
		}
		if !wantFixCycle && fx.sender.calls != 0 {
			t.Fatalf("iteration %d: an approved-first-time run delivered %d fix prompt(s)", i, fx.sender.calls)
		}
		totals.workerSpawns += len(fx.spawner.calls)
		totals.reviewerLaunche += fx.launcher.launchCalls
		totals.fixPrompts += fx.sender.calls

		all := append([]string{masterID}, childRunIDsOf(t, fx, masterID)...)
		for _, runID := range all {
			totals.leakedWakes += dueWakesFor(t, fx, runID)
		}
		totals.assertNoResidue(t, "autonomous", i, residueFor(t, fx, all...))
	}

	if totals.completed != p1eSoakAutonomousRuns {
		t.Fatalf("%d of %d autonomous runs completed", totals.completed, p1eSoakAutonomousRuns)
	}
	t.Logf("P1-E soak (AUTONOMOUS, full=%t): %d objectives, %d completed, %d spawns, %d reviewer launches, "+
		"%d fix prompts; leaks: capacity=%d runtimes=%d locks=%d placements=%d attempts=%d wakes=%d",
		os.Getenv("AO_P1E_SOAK_FULL") == "1",
		totals.runs, totals.completed, totals.workerSpawns, totals.reviewerLaunche, totals.fixPrompts,
		totals.leakedCapacity, totals.leakedRuntimes, totals.leakedLocks, totals.leakedPlacements,
		totals.leakedAttempts, totals.leakedWakes)
}

// TestP1E_SoakMasterObjectives drives multi-child MASTER objectives with a real
// dependency between the children, half of them across a restart.
//
// The property this adds over the autonomous soak is parent/child convergence:
// the parent must complete only after its authoritative children did, and the
// whole tree must be clean afterwards -- not just the parent, which is where a
// child's leaked slot would otherwise hide.
func TestP1E_SoakMasterObjectives(t *testing.T) {
	ctx := context.Background()
	var totals p1eSoakTotals

	for i := 0; i < p1eSoakMasterRuns; i++ {
		fx, _, masterID := startAutonomousObjective(t, twoTaskDependentPlan())
		before := func(int) {
			if _, childID, ok := activeChildRunID(t, fx, masterID); ok {
				approveOpenReview(t, fx, childID, domain.VerdictApproved)
			}
		}
		if i%2 == 0 {
			driveCycles(t, fx, 8, before)
			fx.withCapacityLimits(domain.CapacityLimits{})
		}
		driveCycles(t, fx, 70, before)

		run, _, err := fx.store.GetWorkflowRun(ctx, masterID)
		if err != nil {
			t.Fatalf("iteration %d: GetWorkflowRun: %v", i, err)
		}
		totals.runs++
		if run.State != domain.WorkflowRunCompleted {
			t.Fatalf("iteration %d: master run = %q, want completed", i, run.State)
		}
		totals.completed++

		// Parent/child convergence: no parent completes over an unfinished
		// child, and no child is duplicated across the restart.
		tasks, err := fx.store.ListWorkflowTasks(ctx, masterID)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 2 {
			t.Fatalf("iteration %d: %d tasks, want 2", i, len(tasks))
		}
		seen := map[string]bool{}
		for _, task := range tasks {
			if task.State != domain.WorkflowTaskCompleted {
				t.Fatalf("iteration %d: master completed with task %s in %q", i, task.PlanStepID, task.State)
			}
			if task.ExecutionRunID == nil {
				continue
			}
			if seen[*task.ExecutionRunID] {
				t.Fatalf("iteration %d: two tasks share child run %s", i, *task.ExecutionRunID)
			}
			seen[*task.ExecutionRunID] = true
		}
		if fx.planner.calls != 1 {
			t.Fatalf("iteration %d: planner ran %d times across the restart, want exactly 1", i, fx.planner.calls)
		}
		totals.workerSpawns += len(fx.spawner.calls)
		totals.reviewerLaunche += fx.launcher.launchCalls

		all := append([]string{masterID}, childRunIDsOf(t, fx, masterID)...)
		for _, runID := range all {
			totals.leakedWakes += dueWakesFor(t, fx, runID)
		}
		totals.assertNoResidue(t, "master", i, residueFor(t, fx, all...))
	}

	if totals.completed != p1eSoakMasterRuns {
		t.Fatalf("%d of %d master objectives completed", totals.completed, p1eSoakMasterRuns)
	}
	t.Logf("P1-E soak (MASTER, full=%t): %d objectives, %d converged, %d spawns, %d reviewer launches; "+
		"leaks: capacity=%d runtimes=%d locks=%d placements=%d attempts=%d wakes=%d",
		os.Getenv("AO_P1E_SOAK_FULL") == "1",
		totals.runs, totals.completed, totals.workerSpawns, totals.reviewerLaunche,
		totals.leakedCapacity, totals.leakedRuntimes, totals.leakedLocks, totals.leakedPlacements,
		totals.leakedAttempts, totals.leakedWakes)
}

// workspaceChangeForSoak is the observation that stands in for a worker
// actually acting on the reviewer's findings, so a fix cycle can resolve rather
// than parking as "the fix produced nothing".
func workspaceChangeForSoak() ports.WorkspaceChange {
	return ports.WorkspaceChange{Path: "fix.go", Status: " M"}
}
