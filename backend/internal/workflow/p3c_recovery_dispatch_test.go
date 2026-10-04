package workflow_test

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// p3c_recovery_dispatch_test.go — P3-C §3/§15/§16/§18 against a real store.
//
// The headline property, and the reason this file exists: before P3-C the ONLY
// thing that ever launched an automatic repair was boot reconciliation. A run
// that stopped on a repairable condition under an `automatic` policy while the
// daemon stayed up waited for somebody to press Repair — which is exactly the
// failure the completion bar names. DispatchAutomaticRecovery is the path that
// closes that, and every test below is about what it will and will not do.

// §3: automatic policy + repairable condition + budget = AO repairs it, with
// nobody asked and no restart involved.
func TestAutomaticRecoveryLaunchesARepairWithoutARestart(t *testing.T) {
	ctx := context.Background()
	store, reboot, runID := stoppedTaskRun(t, workflowcore.ReasonVerifyBudgetExhausted)
	setRepairMode(t, store, runID, domain.RepairModeAutomatic)

	coordinator := reboot()
	advice, err := coordinator.AdviceFor(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if advice.AutomaticAction != workflowcore.AutoActionLaunchRepair {
		t.Fatalf("advice automaticAction = %q, want launch_repair", advice.AutomaticAction)
	}
	if advice.RequiresHuman {
		t.Fatal("a run AO is authorized to repair by itself asked for a human")
	}

	out, err := coordinator.DispatchAutomaticRecovery(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Dispatched || out.Action != workflowcore.AutoActionLaunchRepair {
		t.Fatalf("dispatch = %+v, want a launched repair", out)
	}
	if launched := repairRunsFor(t, store, runID); len(launched) != 1 {
		t.Fatalf("%d repairs launched, want exactly 1", len(launched))
	}
}

// §18: recovery is bounded. Repeated dispatch passes converge on ONE repair
// generation rather than minting one per pass — the single most expensive
// mistake an automatic remedy could make.
func TestAutomaticRecoveryIsIdempotentAcrossRepeatedPasses(t *testing.T) {
	ctx := context.Background()
	store, reboot, runID := stoppedTaskRun(t, workflowcore.ReasonVerifyBudgetExhausted)
	setRepairMode(t, store, runID, domain.RepairModeAutomatic)

	for pass := 1; pass <= 4; pass++ {
		if _, err := reboot().DispatchAutomaticRecovery(ctx, runID); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	if launched := repairRunsFor(t, store, runID); len(launched) != 1 {
		t.Fatalf("%d repairs launched across 4 passes, want exactly 1", len(launched))
	}
}

// §3: the policy is the authority. Neither `suggest` nor `disabled` lets the
// dispatcher act, however often it is called.
func TestAutomaticRecoveryRespectsTheFrozenRepairPolicy(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []domain.RepairMode{domain.RepairModeDisabled, domain.RepairModeSuggest} {
		t.Run(string(mode), func(t *testing.T) {
			store, reboot, runID := stoppedTaskRun(t, workflowcore.ReasonVerifyBudgetExhausted)
			setRepairMode(t, store, runID, mode)

			out, err := reboot().DispatchAutomaticRecovery(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			if out.Dispatched {
				t.Fatalf("%s: the dispatcher acted anyway: %+v", mode, out)
			}
			if launched := repairRunsFor(t, store, runID); len(launched) != 0 {
				t.Fatalf("%s: %d repairs launched, want 0", mode, len(launched))
			}
		})
	}
}

// §3/§10: a condition that is not repairable is never repaired, whatever the
// policy says. The condition is judged before the policy, always.
func TestAutomaticRecoveryNeverRepairsANonRepairableCondition(t *testing.T) {
	ctx := context.Background()
	store, reboot, runID := stoppedTaskRun(t, workflowcore.ReasonProviderAuthRequired)
	setRepairMode(t, store, runID, domain.RepairModeAutomatic)

	advice, err := reboot().AdviceFor(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if advice.AutomaticAction != workflowcore.AutoActionNone {
		t.Fatalf("an auth stop produced automatic action %q", advice.AutomaticAction)
	}
	out, err := reboot().DispatchAutomaticRecovery(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if out.Dispatched {
		t.Fatalf("a credential problem was handed to a repair agent: %+v", out)
	}
	if launched := repairRunsFor(t, store, runID); len(launched) != 0 {
		t.Fatalf("%d repairs launched for an auth stop, want 0", len(launched))
	}
}

// §32: the read path does not mutate. Asking for advice — the thing a board
// poll does many times a minute — must never start a repair.
func TestAdviceIsAStrictReadAndNeverLaunchesAnything(t *testing.T) {
	ctx := context.Background()
	store, reboot, runID := stoppedTaskRun(t, workflowcore.ReasonVerifyBudgetExhausted)
	setRepairMode(t, store, runID, domain.RepairModeAutomatic)

	for i := 0; i < 5; i++ {
		if _, err := reboot().AdviceFor(ctx, runID); err != nil {
			t.Fatal(err)
		}
	}
	if launched := repairRunsFor(t, store, runID); len(launched) != 0 {
		t.Fatalf("reading advice launched %d repairs", len(launched))
	}
}

// §15: the click that arrives after another actor already started the same
// remedy is refused with the reason a person can read, not silently duplicated.
func TestStaleRepairClickIsRefusedWithARepairActiveReason(t *testing.T) {
	ctx := context.Background()
	store, reboot, runID := stoppedTaskRun(t, workflowcore.ReasonVerifyBudgetExhausted)
	setRepairMode(t, store, runID, domain.RepairModeAutomatic)

	coordinator := reboot()
	captured, err := coordinator.AdviceFor(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	// Another actor takes the repair between the reading and the click.
	if _, err := coordinator.DispatchAutomaticRecovery(ctx, runID); err != nil {
		t.Fatal(err)
	}

	mismatch, err := reboot().RevalidateActionAuthority(ctx, runID, workflowcore.ActionRepair, captured.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if mismatch != workflowcore.AuthorityMismatchRepairActive {
		t.Fatalf("mismatch = %q, want repair_active", mismatch)
	}
	if mismatch.Describe() == "" {
		t.Fatal("a refusal with no sentence a person can read")
	}
	// And nothing was started a second time.
	if launched := repairRunsFor(t, store, runID); len(launched) != 1 {
		t.Fatalf("%d repairs exist after the stale click, want 1", len(launched))
	}
}

// §15: a read-only offer is never refused just because a repair started.
// Refusing to open a session would be a refusal with no purpose.
func TestReadOnlyActionsAreNotRefusedByAnActiveRepair(t *testing.T) {
	ctx := context.Background()
	store, reboot, runID := stoppedTaskRun(t, workflowcore.ReasonVerifyBudgetExhausted)
	setRepairMode(t, store, runID, domain.RepairModeAutomatic)
	if _, err := reboot().DispatchAutomaticRecovery(ctx, runID); err != nil {
		t.Fatal(err)
	}

	mismatch, err := reboot().RevalidateActionAuthority(ctx, runID, workflowcore.ActionViewChanges, workflowcore.AdviceAuthority{})
	if err != nil {
		t.Fatal(err)
	}
	if mismatch != workflowcore.AuthorityMismatchNone {
		t.Fatalf("a read-only action was refused with %q", mismatch)
	}
}

// §15: a client that captured no authority is not refused. Sending no proof
// gets the pre-P3-C behaviour, never a refusal it cannot act on.
func TestAnEmptyAuthorityProofIsNotARefusal(t *testing.T) {
	ctx := context.Background()
	_, reboot, runID := stoppedTaskRun(t, workflowcore.ReasonVerifyBudgetExhausted)

	mismatch, err := reboot().RevalidateActionAuthority(ctx, runID, workflowcore.ActionContinue, workflowcore.AdviceAuthority{})
	if err != nil {
		t.Fatal(err)
	}
	if mismatch != workflowcore.AuthorityMismatchNone {
		t.Fatalf("an empty proof was refused with %q", mismatch)
	}
}

// §15: a click computed against a stop the run has moved past is refused as
// such — the situation changed, and the action was for the old one.
func TestAClickAgainstASupersededStopIsRefused(t *testing.T) {
	ctx := context.Background()
	store, reboot, runID := stoppedTaskRun(t, workflowcore.ReasonVerifyBudgetExhausted)
	captured, err := reboot().AdviceFor(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	// The run stops on something else entirely.
	parkRun(t, store, runID, "p", workflowcore.ReasonProviderAuthRequired)

	mismatch, err := reboot().RevalidateActionAuthority(ctx, runID, workflowcore.ActionRepair, captured.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if mismatch != workflowcore.AuthorityMismatchStopChanged {
		t.Fatalf("mismatch = %q, want stop_changed", mismatch)
	}
}

// §25: the CLI-shaped answer says the two things somebody typing `recover
// status` needs: is anyone needed, and what is AO doing.
func TestRecoveryStatusSaysWhetherAnyoneIsNeeded(t *testing.T) {
	ctx := context.Background()
	store, reboot, runID := stoppedTaskRun(t, workflowcore.ReasonVerifyBudgetExhausted)
	setRepairMode(t, store, runID, domain.RepairModeAutomatic)
	coordinator := reboot()
	if _, err := coordinator.DispatchAutomaticRecovery(ctx, runID); err != nil {
		t.Fatal(err)
	}

	line, err := reboot().RecoveryStatus(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if line.Advice.RequiresHuman {
		t.Fatal("a run being repaired reported that a human is required")
	}
	if line.ActionRequired != "" {
		t.Fatalf("a run being repaired printed an imperative: %q", line.ActionRequired)
	}
	if !line.Advice.AutomaticActionActive {
		t.Fatalf("the status did not say AO is acting: %+v", line.Advice)
	}
	if line.Headline == "" {
		t.Fatal("the status printed no headline")
	}
}

// §18: an exhausted budget stops the dispatcher, and the advice says the bound
// was reached rather than silently offering nothing.
func TestAutomaticRecoveryStopsAtTheBudget(t *testing.T) {
	ctx := context.Background()
	store, reboot, runID := stoppedTaskRun(t, workflowcore.ReasonVerifyBudgetExhausted)
	setRepairMode(t, store, runID, domain.RepairModeAutomatic)

	// Spend the whole budget, ending each generation before the next is minted.
	budget := 0
	for i := 0; i < 8; i++ {
		out, err := reboot().DispatchAutomaticRecovery(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		if out.Dispatched {
			budget++
			// A repair must actually END before the next generation can be
			// spent: while one is running it IS this failure's repair.
			// Cancelling is what a repair that fixed nothing looks like here.
			if _, cerr := reboot().CancelRun(ctx, out.RepairRunID); cerr != nil {
				t.Fatalf("end repair: %v", cerr)
			}
			continue
		}
		break
	}
	if budget == 0 {
		t.Fatal("no repair was ever launched, so the bound was never exercised")
	}
	advice, err := reboot().AdviceFor(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if advice.AutomaticAction == workflowcore.AutoActionLaunchRepair {
		t.Fatalf("the dispatcher still intended a repair past the budget: %+v", advice)
	}
	if advice.RepairBudget > 0 && budget > advice.RepairBudget {
		t.Fatalf("%d repairs launched against a budget of %d", budget, advice.RepairBudget)
	}
}
