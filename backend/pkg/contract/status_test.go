package contract_test

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/pkg/contract"
)

var statusNow = time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

func session(activity contract.ActivityState) contract.SessionFacts {
	return contract.SessionFacts{
		Activity:       activity,
		LastActivityAt: statusNow,
		HasSignal:      true,
	}
}

func TestDeriveStatusPrecedence(t *testing.T) {
	tests := []struct {
		name    string
		session contract.SessionFacts
		prs     []contract.PRFacts
		want    contract.SessionStatus
	}{
		{"terminated", contract.SessionFacts{IsTerminated: true}, nil, contract.StatusTerminated},
		{"terminated merged", contract.SessionFacts{IsTerminated: true}, []contract.PRFacts{{Merged: true}}, contract.StatusMerged},
		{"active before PR", session(contract.ActivityActive), []contract.PRFacts{{CI: contract.CIFailing}}, contract.StatusWorking},
		{"exited before PR", session(contract.ActivityExited), []contract.PRFacts{{Mergeability: contract.MergeMergeable}}, contract.StatusExited},
		{"waiting before PR", session(contract.ActivityWaitingInput), []contract.PRFacts{{CI: contract.CIFailing}}, contract.StatusNeedsInput},
		{"blocked before PR", session(contract.ActivityBlocked), []contract.PRFacts{{CI: contract.CIFailing}}, contract.StatusNeedsInput},
		{"PR before idle", session(contract.ActivityIdle), []contract.PRFacts{{CI: contract.CIFailing}}, contract.StatusCIFailed},
		{"idle", session(contract.ActivityIdle), nil, contract.StatusIdle},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := contract.DeriveStatus(tt.session, tt.prs, statusNow, 90*time.Second)
			if got != tt.want {
				t.Fatalf("DeriveStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDeriveStatusNoSignalRules(t *testing.T) {
	const grace = 90 * time.Second
	silent := contract.SessionFacts{
		Activity:       contract.ActivityIdle,
		LastActivityAt: statusNow.Add(-2 * grace),
	}

	tests := []struct {
		name           string
		session        contract.SessionFacts
		signalExpected bool
		now            time.Time
		want           contract.SessionStatus
	}{
		{"past grace", silent, true, statusNow, contract.StatusNoSignal},
		{"signal not expected", silent, false, statusNow, contract.StatusIdle},
		{"signal received", withSignal(silent), true, statusNow, contract.StatusIdle},
		{"at boundary", silent, true, silent.LastActivityAt.Add(grace), contract.StatusIdle},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			facts := tt.session
			facts.SignalExpected = tt.signalExpected
			got := contract.DeriveStatus(facts, nil, tt.now, grace)
			if got != tt.want {
				t.Fatalf("DeriveStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDeriveSCMStatusPipelineAndWorstWins(t *testing.T) {
	tests := []struct {
		name string
		prs  []contract.PRFacts
		want contract.SessionStatus
	}{
		{"closed ignored", []contract.PRFacts{{Closed: true}}, ""},
		{"merged", []contract.PRFacts{{Merged: true}}, contract.StatusMerged},
		{"open", []contract.PRFacts{{}}, contract.StatusPROpen},
		{"review pending", []contract.PRFacts{{Review: contract.ReviewRequired}}, contract.StatusReviewPending},
		{"approved", []contract.PRFacts{{Review: contract.ReviewApproved}}, contract.StatusApproved},
		{"mergeable", []contract.PRFacts{{Mergeability: contract.MergeMergeable}}, contract.StatusMergeable},
		{"merge blocked", []contract.PRFacts{{Mergeability: contract.MergeBlocked}}, contract.StatusPROpen},
		{"merge blocked with approved review", []contract.PRFacts{{Mergeability: contract.MergeBlocked, Review: contract.ReviewApproved}}, contract.StatusPROpen},
		{"changes requested", []contract.PRFacts{{Review: contract.ReviewChangesRequest}}, contract.StatusChangesRequested},
		{"review comments", []contract.PRFacts{{ReviewComments: true}}, contract.StatusChangesRequested},
		{"draft", []contract.PRFacts{{Draft: true}}, contract.StatusDraft},
		{"CI failed", []contract.PRFacts{{CI: contract.CIFailing}}, contract.StatusCIFailed},
		{
			"worst wins",
			[]contract.PRFacts{
				{URL: "a", SourceBranch: "a", TargetBranch: "main", Mergeability: contract.MergeMergeable},
				{URL: "b", SourceBranch: "b", TargetBranch: "main", CI: contract.CIFailing},
			},
			contract.StatusCIFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := contract.DeriveSCMStatus(tt.prs); got != tt.want {
				t.Fatalf("DeriveSCMStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStackRules(t *testing.T) {
	parent := contract.PRFacts{
		URL:          "parent",
		SourceBranch: "feature",
		TargetBranch: "main",
		Mergeability: contract.MergeMergeable,
	}
	child := contract.PRFacts{
		URL:          "child",
		SourceBranch: "feature/child",
		TargetBranch: "feature",
	}

	positions := contract.BuildStacks([]contract.PRFacts{parent, child})
	if positions["parent"].Blocked || !positions["parent"].BottomOfStack {
		t.Fatalf("parent position = %+v", positions["parent"])
	}
	if !positions["child"].Blocked || positions["child"].BottomOfStack {
		t.Fatalf("child position = %+v", positions["child"])
	}

	if got := contract.DeriveSCMStatus([]contract.PRFacts{parent, child}); got != contract.StatusMergeable {
		t.Fatalf("blocked child readiness was not suppressed: got %q", got)
	}
	child.CI = contract.CIFailing
	if got := contract.DeriveSCMStatus([]contract.PRFacts{parent, child}); got != contract.StatusCIFailed {
		t.Fatalf("blocked child problem was suppressed: got %q", got)
	}

	parent.Merged = true
	positions = contract.BuildStacks([]contract.PRFacts{parent, child})
	if positions["child"].Blocked {
		t.Fatal("merged parent still blocks child")
	}
}

func withSignal(facts contract.SessionFacts) contract.SessionFacts {
	facts.HasSignal = true
	return facts
}

// completed returns a task that has no work in flight and a durable receipt
// saying its agent reported the work finished.
func completed() contract.SessionFacts {
	facts := session(contract.ActivityIdle)
	facts.TurnCompleted = true
	return facts
}

// TestDeriveStatusCompletedNeedsProof pins the difference the Completed status
// exists to draw: a quiet task is Idle, and only the durable completion receipt
// promotes it. Nothing here is inferred from time or from a stopped runtime.
func TestDeriveStatusCompletedNeedsProof(t *testing.T) {
	if got := contract.DeriveStatus(completed(), nil, statusNow, 90*time.Second); got != contract.StatusCompleted {
		t.Fatalf("task with a completion receipt = %q, want %q", got, contract.StatusCompleted)
	}
	if got := contract.DeriveStatus(session(contract.ActivityIdle), nil, statusNow, 90*time.Second); got != contract.StatusIdle {
		t.Fatalf("quiet task without a receipt = %q, want %q", got, contract.StatusIdle)
	}
}

// TestDeriveStatusCompletedSurvivesInactivity checks that the status does not
// decay: an untouched receipt still reads Completed a week later, and still
// reads Completed when the hook pipeline could not be re-proven after a restart
// (HasSignal false, which alone would mean no_signal).
func TestDeriveStatusCompletedSurvivesInactivity(t *testing.T) {
	const grace = 90 * time.Second
	facts := completed()
	facts.LastActivityAt = statusNow.Add(-7 * 24 * time.Hour)
	facts.SignalExpected = true

	if got := contract.DeriveStatus(facts, nil, statusNow, grace); got != contract.StatusCompleted {
		t.Fatalf("a week later = %q, want %q", got, contract.StatusCompleted)
	}

	restored := facts
	restored.HasSignal = false
	if got := contract.DeriveStatus(restored, nil, statusNow, grace); got != contract.StatusCompleted {
		t.Fatalf("after a restart cleared the hook receipt = %q, want %q", got, contract.StatusCompleted)
	}
	unproven := restored
	unproven.TurnCompleted = false
	if got := contract.DeriveStatus(unproven, nil, statusNow, grace); got != contract.StatusNoSignal {
		t.Fatalf("same session without the completion receipt = %q, want %q", got, contract.StatusNoSignal)
	}
}

// TestDeriveStatusCompletedNeverMasksTrouble is requirement five: nothing that
// failed, was cancelled, or wants the user may read Completed, receipt or not.
func TestDeriveStatusCompletedNeverMasksTrouble(t *testing.T) {
	withReceipt := func(activity contract.ActivityState) contract.SessionFacts {
		facts := session(activity)
		facts.TurnCompleted = true
		return facts
	}
	terminated := contract.SessionFacts{IsTerminated: true, TurnCompleted: true}

	tests := []struct {
		name    string
		session contract.SessionFacts
		prs     []contract.PRFacts
		want    contract.SessionStatus
	}{
		{"killed or cancelled", terminated, nil, contract.StatusTerminated},
		{"crashed agent", withReceipt(contract.ActivityExited), nil, contract.StatusExited},
		{"asking the user", withReceipt(contract.ActivityWaitingInput), nil, contract.StatusNeedsInput},
		{"blocked on a decision", withReceipt(contract.ActivityBlocked), nil, contract.StatusNeedsInput},
		{"given more work", withReceipt(contract.ActivityActive), nil, contract.StatusWorking},
		{"failing CI", completed(), []contract.PRFacts{{URL: "u", CI: contract.CIFailing}}, contract.StatusCIFailed},
		{
			"changes requested",
			completed(),
			[]contract.PRFacts{{URL: "u", Review: contract.ReviewChangesRequest}},
			contract.StatusChangesRequested,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := contract.DeriveStatus(tt.session, tt.prs, statusNow, 90*time.Second)
			if got != tt.want {
				t.Fatalf("DeriveStatus() = %q, want %q", got, tt.want)
			}
			if got == contract.StatusCompleted {
				t.Fatalf("DeriveStatus() reported a finished task for %q", tt.name)
			}
		})
	}
}
