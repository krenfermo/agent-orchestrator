package workflow_test

// repair_lifecycle_test.go — certification of AO's automatic repair cycle.
//
// WHY DETERMINISTICALLY. Four separate provider gate runs tried to make a real
// worker produce a real mistake for AO to repair, and every one of them was
// defeated: a competent worker reads the contract it is given and satisfies it.
// The repair cycle is not the worker's behaviour, though — it is AO's: a
// verification fails, AO decides on its own to re-enter fix, delivers the
// findings to the worker it already has, waits for the work, and verifies
// again. Every one of those steps is AO's own code, and all of it can be
// exercised exactly as production runs it without depending on a language model
// choosing to be wrong.
//
// WHAT MAKES IT REAL RATHER THAN A MOCK. This harness drives the SAME machinery
// the daemon does: a real sqlite store, the real wake.Scheduler, the real
// wakepoller.Poller, the real Coordinator, real git worktrees per task. Nothing
// about the lifecycle is simulated — the test never calls a repair entry point
// directly, never writes a workflow row, and never advances a step. It supplies
// exactly two things production gets from outside: the reviewer's verdict
// (which lands out of band via `ao review submit`), and the worker's edits.
//
// THE FAILURE CONTRACT, and why it is content-driven. A verifier that fails
// "the first time" would certify nothing: the second pass would succeed because
// it is the second pass, and a repair cycle that never repaired anything would
// look identical. This harness's verifier reads a file out of the worktree it
// was pointed at and passes only when that file carries the repaired content.
// The verdict is therefore a function of the repository and of nothing else —
// no attempt counter, no call count, no clock. TestRepair_FailureContract
// asserts that property directly before any lifecycle test relies on it.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// repairMarker is the file whose content the verification depends on.
const repairMarker = "verification_state.txt"

// repairedContent is what the marker must say for the verification to pass.
// Anything else — including the file's absence — is a failing verification.
const repairedContent = "repaired"

// repairContract is the deterministic failure contract: a verification whose
// verdict is a pure function of the tree it was pointed at.
type repairContract struct {
	// dirs is every directory a verification command was run in, in order. It
	// is what the harness uses to find the worktree AO is actually verifying,
	// rather than assuming one.
	dirs []string
	// verdicts is the pass/fail sequence, so a test can assert that the run
	// failed before the repair and passed after it.
	verdicts []bool
}

func (c *repairContract) decide(req workflowcore.VerifyCommandRequest) (workflowcore.VerifyCommandExecution, error) {
	c.dirs = append(c.dirs, req.Directory)
	data, err := os.ReadFile(filepath.Join(req.Directory, repairMarker))
	passed := err == nil && strings.TrimSpace(string(data)) == repairedContent
	c.verdicts = append(c.verdicts, passed)
	if passed {
		return workflowcore.VerifyCommandExecution{ExitCode: 0, StdoutTail: "ok"}, nil
	}
	return workflowcore.VerifyCommandExecution{ExitCode: 1, StderrTail: repairMarker + " does not say " + repairedContent}, nil
}

// lastDir is the worktree the most recent verification actually ran in.
func (c *repairContract) lastDir() string {
	if len(c.dirs) == 0 {
		return ""
	}
	return c.dirs[len(c.dirs)-1]
}

func (c *repairContract) passedAtLeastOnce() bool {
	for _, v := range c.verdicts {
		if v {
			return true
		}
	}
	return false
}

// applyRepair is the worker's half: it writes the repaired content into the
// worktree AO is verifying, exactly as an agent editing files would.
func applyRepair(t *testing.T, dir string) {
	t.Helper()
	if dir == "" {
		t.Fatal("no worktree to repair — the verification never named one")
	}
	if err := os.WriteFile(filepath.Join(dir, repairMarker), []byte(repairedContent+"\n"), 0o600); err != nil {
		t.Fatalf("apply repair in %s: %v", dir, err)
	}
}

// newRepairFixture is newAutonomousFixture with the deterministic failure
// contract wired in place of the canned verifier.
func newRepairFixture(t *testing.T) (*autonomousFixture, *repairContract) {
	t.Helper()
	fx := newAutonomousFixture(t, oneTaskPlan())
	contract := &repairContract{}
	fx.verifier.decide = contract.decide
	seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())
	return fx, contract
}

// startRepairRun kicks the objective off exactly as the daemon does.
func startRepairRun(t *testing.T, fx *autonomousFixture) string {
	t.Helper()
	created, err := fx.coord.CreateObjectiveRun(context.Background(), "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")
	return created.Run.ID
}

// repairPhases lists every durable phase a child run recorded.
func repairPhases(t *testing.T, fx *autonomousFixture, runID string) []string {
	t.Helper()
	cps, err := fx.store.ListWorkflowCheckpoints(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	var out []string
	for _, cp := range cps {
		out = append(out, cp.DurablePhase)
	}
	return out
}

func countPhase(phases []string, want string) int {
	n := 0
	for _, p := range phases {
		if p == want {
			n++
		}
	}
	return n
}

// B1 — THE FAILURE CONTRACT. The verdict must depend on repository content and
// on nothing else. A contract that flipped on attempt number would make every
// lifecycle test below vacuous.
func TestRepair_FailureContractDependsOnContentNotAttemptNumber(t *testing.T) {
	dir := t.TempDir()
	contract := &repairContract{}
	req := workflowcore.VerifyCommandRequest{Command: "go", Args: []string{"test"}, Directory: dir}

	// Any number of attempts on an unrepaired tree fail. The attempt counter is
	// not an input.
	for i := 0; i < 5; i++ {
		exec, err := contract.decide(req)
		if err != nil || exec.ExitCode == 0 {
			t.Fatalf("attempt %d passed on an unrepaired tree (exit=%d err=%v)", i, exec.ExitCode, err)
		}
	}
	applyRepair(t, dir)
	exec, err := contract.decide(req)
	if err != nil || exec.ExitCode != 0 {
		t.Fatalf("a repaired tree must pass, got exit=%d err=%v", exec.ExitCode, err)
	}
	// And it flips back: the verdict tracks the content in both directions,
	// which no attempt-number rule could do.
	if err := os.Remove(filepath.Join(dir, repairMarker)); err != nil {
		t.Fatal(err)
	}
	if exec, _ := contract.decide(req); exec.ExitCode == 0 {
		t.Fatal("removing the repair must fail the verification again")
	}
	if got := len(contract.verdicts); got != 7 {
		t.Fatalf("verdicts recorded = %d, want 7", got)
	}
}

// B2 — THE CYCLE. A verification fails on unrepaired content; AO re-enters fix
// on its own, delivers the findings to the worker it already has, and when the
// repository is repaired the re-verification passes and the run completes. No
// human action anywhere.
func TestRepair_FailedVerificationIsRepairedAndReverifiedAutonomously(t *testing.T) {
	fx, contract := newRepairFixture(t)
	ctx := context.Background()
	masterID := startRepairRun(t, fx)

	repaired := false
	sendsBeforeRepair := 0
	driveCycles(t, fx, 60, func(int) {
		_, childID, ok := activeChildRunID(t, fx, masterID)
		if !ok {
			return
		}
		approveOpenReview(t, fx, childID, domain.VerdictApproved)
		// The worker's half: once AO has delivered fix findings for a failed
		// verification, the repository gets repaired.
		if !repaired && fx.sender.calls > 0 && contract.lastDir() != "" {
			applyRepair(t, contract.lastDir())
			// The workspace genuinely changed, which is what resolves the fix
			// cycle — the same signal a real edit produces.
			fx.ws.obs.Changes = append(fx.ws.obs.Changes, ports.WorkspaceChange{Path: repairMarker, Status: " M"})
			sendsBeforeRepair = fx.sender.calls
			repaired = true
		}
	})

	if len(contract.verdicts) == 0 {
		t.Fatal("verification never ran")
	}
	if contract.verdicts[0] {
		t.Fatal("the first verification must fail — the contract's unrepaired tree passed")
	}
	if !repaired {
		t.Fatalf("AO never delivered fix findings for the failed verification (sender calls=%d)", fx.sender.calls)
	}
	if !contract.passedAtLeastOnce() {
		t.Fatalf("the repaired tree was never re-verified successfully; verdicts=%v", contract.verdicts)
	}
	if sendsBeforeRepair == 0 {
		t.Fatal("the fix findings must reach the worker before the repair, not after")
	}
	run, _, err := fx.store.GetWorkflowRun(ctx, masterID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != domain.WorkflowRunCompleted {
		t.Fatalf("master run state = %q, want completed after an automatic repair", run.State)
	}

	// The mechanism, not just the outcome: this must be the VERIFY-driven
	// repair path — AO's own decision to re-enter fix after a failed
	// verification — and not a reviewer asking for changes.
	phases := repairPhases(t, fx, repairChildRunID(t, fx, masterID))
	if n := countPhase(phases, "verify_fix_reentry"); n != 1 {
		t.Fatalf("verify_fix_reentry records = %d, want exactly 1; phases=%v", n, phases)
	}
	if n := countPhase(phases, "fix_dispatched"); n != 1 {
		t.Fatalf("fix_dispatched records = %d, want exactly 1; phases=%v", n, phases)
	}
	if n := countPhase(phases, "fix_observed_waiting"); n < 1 {
		t.Fatalf("the repair's work was never observed; phases=%v", phases)
	}
	for _, p := range phases {
		if strings.HasSuffix(p, "_failed") {
			t.Fatalf("an automatic repair recorded a failure phase %q; phases=%v", p, phases)
		}
	}
}

// repairChildRunID is the execution run of the objective's single task.
func repairChildRunID(t *testing.T, fx *autonomousFixture, masterID string) string {
	t.Helper()
	tasks, err := fx.store.ListWorkflowTasks(context.Background(), masterID)
	if err != nil {
		t.Fatalf("ListWorkflowTasks: %v", err)
	}
	for _, tk := range tasks {
		if tk.ExecutionRunID != nil {
			return *tk.ExecutionRunID
		}
	}
	t.Fatal("the objective never dispatched a child run")
	return ""
}

// driveRepairToCompletion runs the whole repair lifecycle, calling atCycle
// before each poller tick so a test can restart the daemon partway through.
// Returns the child run id and the repair contract's verdict sequence.
func driveRepairToCompletion(t *testing.T, fx *autonomousFixture, contract *repairContract, masterID string, cycles int, atCycle func(i int, repaired bool)) {
	t.Helper()
	repaired := false
	driveCycles(t, fx, cycles, func(i int) {
		if atCycle != nil {
			atCycle(i, repaired)
		}
		_, childID, ok := activeChildRunID(t, fx, masterID)
		if !ok {
			return
		}
		approveOpenReview(t, fx, childID, domain.VerdictApproved)
		if !repaired && fx.sender.calls > 0 && contract.lastDir() != "" {
			applyRepair(t, contract.lastDir())
			fx.ws.obs.Changes = append(fx.ws.obs.Changes, ports.WorkspaceChange{Path: repairMarker, Status: " M"})
			repaired = true
		}
	})
}

// restartDaemon rebuilds the Coordinator and the poller over the SAME durable
// store, which is what a daemon restart is: every in-memory decision is gone,
// and everything the run knows about itself has to come back off disk.
func restartDaemon(t *testing.T, fx *autonomousFixture) {
	t.Helper()
	fx.withCapacityLimits(domain.CapacityLimits{})
}

// B3 — REPAIR IDENTITY. Sixty poller ticks over one failed verification must
// produce ONE repair, not one per tick. The cycle's identity is durable, so
// re-entering it is a no-op rather than a second dispatch.
func TestRepair_IsDispatchedExactlyOncePerFailedVerification(t *testing.T) {
	fx, contract := newRepairFixture(t)
	masterID := startRepairRun(t, fx)
	driveRepairToCompletion(t, fx, contract, masterID, 60, nil)

	childID := repairChildRunID(t, fx, masterID)
	phases := repairPhases(t, fx, childID)
	if n := countPhase(phases, "verify_fix_reentry"); n != 1 {
		t.Fatalf("verify_fix_reentry = %d, want exactly 1 across 60 ticks", n)
	}
	if n := countPhase(phases, "fix_dispatch_intent"); n != 1 {
		t.Fatalf("fix_dispatch_intent = %d, want exactly 1 — the repair was dispatched more than once", n)
	}
	if fx.sender.calls != 1 {
		t.Fatalf("fix findings delivered %d times, want exactly 1", fx.sender.calls)
	}
	steps, err := fx.store.ListWorkflowSteps(context.Background(), childID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.Kind != domain.WorkflowStepFix {
			continue
		}
		attempts, aerr := fx.store.ListWorkflowAttempts(context.Background(), step.ID)
		if aerr != nil {
			t.Fatal(aerr)
		}
		if len(attempts) > 1 {
			t.Fatalf("fix attempts = %d, want at most 1 — the repair opened a second attempt for one failure", len(attempts))
		}
	}
}

// B4 — RESTART SAFETY. The daemon is restarted at each of the three points
// where a repair can be interrupted, and every one of them must converge to the
// same terminal state with exactly one repair.
func TestRepair_SurvivesDaemonRestartAtEveryStage(t *testing.T) {
	stages := []struct {
		name string
		// restartWhen decides, from what the run has done so far, whether this
		// tick is the one to restart on.
		restartWhen func(fx *autonomousFixture, contract *repairContract, repaired bool) bool
	}{
		{"after the failed verification, before the repair is dispatched", func(fx *autonomousFixture, c *repairContract, _ bool) bool {
			return len(c.verdicts) > 0 && !c.verdicts[0] && fx.sender.calls == 0
		}},
		{"after the repair is dispatched, before the work lands", func(fx *autonomousFixture, _ *repairContract, repaired bool) bool {
			return fx.sender.calls > 0 && !repaired
		}},
		{"after the work lands, before the re-verification", func(fx *autonomousFixture, c *repairContract, repaired bool) bool {
			return repaired && !c.passedAtLeastOnce()
		}},
	}
	for _, stage := range stages {
		t.Run(stage.name, func(t *testing.T) {
			fx, contract := newRepairFixture(t)
			masterID := startRepairRun(t, fx)
			restarted := false
			driveRepairToCompletion(t, fx, contract, masterID, 80, func(_ int, repaired bool) {
				if !restarted && stage.restartWhen(fx, contract, repaired) {
					restartDaemon(t, fx)
					restarted = true
				}
			})
			if !restarted {
				t.Fatalf("the run never reached this stage, so the restart was never exercised; verdicts=%v sends=%d", contract.verdicts, fx.sender.calls)
			}
			run, _, err := fx.store.GetWorkflowRun(context.Background(), masterID)
			if err != nil {
				t.Fatal(err)
			}
			if run.State != domain.WorkflowRunCompleted {
				t.Fatalf("run state = %q, want completed after a restart at this stage", run.State)
			}
			phases := repairPhases(t, fx, repairChildRunID(t, fx, masterID))
			if n := countPhase(phases, "fix_dispatch_intent"); n != 1 {
				t.Fatalf("a restart produced %d repair dispatches, want exactly 1", n)
			}
			if !contract.passedAtLeastOnce() {
				t.Fatalf("the repaired tree was never re-verified; verdicts=%v", contract.verdicts)
			}
		})
	}
}

// B5 — PRESENTATION. While AO is repairing, no surface may tell a person it is
// their turn, and none may offer them the remedy AO is already applying.
//
// The flag to assert is RequiresHuman, not AutomaticActionActive: a repair that
// is already running inside the ordinary fix step is AdviceNoActionRequired by
// design ("a run that is working, reviewing, verifying, integrating — or one AO
// has already started correcting. Nobody is needed."), and AutomaticAction
// names a remedy AO intends to START, which by then it already has. What
// matters to a person looking at the screen is that nothing asks them to act,
// and that is what this checks — at every point between the failed verification
// and the passing one.
func TestRepair_IsNeverPresentedAsAPersonsTurn(t *testing.T) {
	fx, contract := newRepairFixture(t)
	ctx := context.Background()
	masterID := startRepairRun(t, fx)

	// Remedies a person could be invited to apply. open_session and cancel are
	// not remedies — they are the escape hatches every live run carries.
	remedies := map[workflowcore.ActionID]bool{
		workflowcore.ActionRepair:   true,
		workflowcore.ActionContinue: true,
	}
	samples := 0
	driveRepairToCompletion(t, fx, contract, masterID, 60, func(_ int, _ bool) {
		// The whole window: from the failed verification until a verification
		// passes. Everything AO does to repair happens inside it.
		if len(contract.verdicts) == 0 || contract.verdicts[0] || contract.passedAtLeastOnce() {
			return
		}
		_, childID, ok := activeChildRunID(t, fx, masterID)
		if !ok {
			return
		}
		advice, err := fx.coord.AdviceFor(ctx, childID)
		if err != nil {
			t.Fatalf("AdviceFor: %v", err)
		}
		samples++
		if advice.RequiresHuman {
			t.Fatalf("a run AO is repairing was presented as needing a person: category=%q summary=%q", advice.Category, advice.Summary)
		}
		if advice.Category == workflowcore.AdviceHumanAction {
			t.Fatalf("advice category = %q while AO was repairing", advice.Category)
		}
		for _, id := range advice.AvailableActions {
			if remedies[id] {
				t.Fatalf("a person was offered %q while AO was already repairing", id)
			}
		}
		if advice.RecommendedAction != "" && advice.RecommendedAction != workflowcore.ActionWait {
			t.Fatalf("AO recommended %q to a person while repairing by itself", advice.RecommendedAction)
		}
	})
	if samples == 0 {
		t.Fatal("the repair window was never observed, so nothing was asserted")
	}
	// And the finished run is not left asking for anything either.
	advice, err := fx.coord.AdviceFor(ctx, repairChildRunID(t, fx, masterID))
	if err != nil {
		t.Fatal(err)
	}
	if advice.RequiresHuman {
		t.Fatalf("a repaired-and-completed run still asks for a person: %q", advice.Summary)
	}
}

// B6 — REPAIR MEETS THE COMMIT BOUNDARY. After a repair, the verification that
// governs what gets committed must be the POST-repair one. A run that held its
// pre-repair verdict would let the commit boundary compare the repaired tree
// against the state that failed.
func TestRepair_TheGoverningVerdictIsThePostRepairOne(t *testing.T) {
	fx, contract := newRepairFixture(t)
	ctx := context.Background()
	masterID := startRepairRun(t, fx)
	driveRepairToCompletion(t, fx, contract, masterID, 60, nil)

	childID := repairChildRunID(t, fx, masterID)
	cps, err := fx.store.ListWorkflowCheckpoints(ctx, childID)
	if err != nil {
		t.Fatal(err)
	}
	var passing []domain.WorkflowCheckpoint
	var failing []domain.WorkflowCheckpoint
	for _, cp := range cps {
		if cp.DurablePhase != "verify_result" {
			continue
		}
		if strings.Contains(cp.RetryState, `"passed":true`) {
			passing = append(passing, cp)
		} else {
			failing = append(failing, cp)
		}
	}
	if len(failing) == 0 {
		t.Fatal("no failed verification was recorded — the repair had nothing to repair")
	}
	if len(passing) == 0 {
		t.Fatal("no passing verification was recorded after the repair")
	}
	last := passing[len(passing)-1]
	if !last.CreatedAt.After(failing[0].CreatedAt) {
		t.Fatal("the governing verdict predates the failure it was supposed to have repaired")
	}
	// Part A's canonical state must be on the governing verdict, and it must
	// describe the repaired tree rather than the one that failed.
	if !strings.Contains(last.RetryState, `"verifiedState"`) {
		t.Fatalf("the governing verdict carries no canonical verified state: %s", last.RetryState)
	}
	if last.FingerprintAfter != "" && failing[0].FingerprintAfter == last.FingerprintAfter {
		t.Fatal("the post-repair verdict describes the same tree as the failure, so the repair changed nothing")
	}
}

// B7 — THE BOUND. A repair that never arrives must not loop forever. AO spends
// its fix budget and then stops, leaving a run a person can act on rather than
// a machine that keeps re-dispatching.
func TestRepair_UnrepairedWorkStopsInsteadOfLoopingForever(t *testing.T) {
	fx, contract := newRepairFixture(t)
	ctx := context.Background()
	masterID := startRepairRun(t, fx)

	// Same drive loop, except the worker never repairs anything.
	driveCycles(t, fx, 80, func(int) {
		if _, childID, ok := activeChildRunID(t, fx, masterID); ok {
			approveOpenReview(t, fx, childID, domain.VerdictApproved)
		}
	})

	if contract.passedAtLeastOnce() {
		t.Fatalf("the verification passed without a repair; verdicts=%v", contract.verdicts)
	}
	run, _, err := fx.store.GetWorkflowRun(ctx, masterID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State == domain.WorkflowRunCompleted {
		t.Fatal("a run whose verification never passed was reported completed")
	}
	phases := repairPhases(t, fx, repairChildRunID(t, fx, masterID))
	reentries := countPhase(phases, "verify_fix_reentry")
	if reentries == 0 {
		t.Fatal("AO never attempted a repair at all")
	}
	if reentries > 8 {
		t.Fatalf("verify_fix_reentry = %d over 80 ticks — the repair loop is unbounded", reentries)
	}
}
