package workflow

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// provider_auth_interactive_test.go pins the verdict policy for the keychain
// incident's own state.
//
// "The provider has no usable credential" and "reaching the credential needs a
// person" are different stops with different remedies, and only the second one
// used to present as a hang rather than a failure. Before this they were the
// same field, so a run that stopped because macOS wanted a keychain password
// told its operator to go and sign in again -- which fixed nothing.

func TestEvaluateWorkerPreflight_InteractiveAuthIsItsOwnVerdict(t *testing.T) {
	verdict := evaluateWorkerPreflight(domain.HarnessClaudeCode, "/tmp/ws", WorkerPreflightResult{
		BinaryOK:                true,
		AuthRequiresInteraction: true,
		TrustOK:                 true, TrustUnknown: true,
		PermissionModeOK: true, PermissionModeUnknown: true,
		Detail: "keychain at /x/login.keychain-db cannot be opened",
	})
	if verdict.Ready {
		t.Fatal("a launch that would open an OS prompt was allowed to dispatch")
	}
	if verdict.Class != WorkflowErrorProviderAuthInteractive {
		t.Fatalf("class = %q, want %q", verdict.Class, WorkflowErrorProviderAuthInteractive)
	}
	if verdict.Reason != ReasonProviderAuthInteractive {
		t.Fatalf("reason = %q, want %q", verdict.Reason, ReasonProviderAuthInteractive)
	}
	if !strings.Contains(verdict.Detail, "hang") {
		t.Fatalf("detail does not say the launch would hang rather than fail: %q", verdict.Detail)
	}
}

// Ordering matters: an interactive credential store is the more specific claim
// and must not be reported as a plain "not authenticated", which would send a
// person to a login that is not the problem.
func TestEvaluateWorkerPreflight_InteractiveOutranksUnauthenticated(t *testing.T) {
	verdict := evaluateWorkerPreflight(domain.HarnessClaudeCode, "/tmp/ws", WorkerPreflightResult{
		BinaryOK:                true,
		AuthOK:                  false,
		AuthUnknown:             false,
		AuthRequiresInteraction: true,
		TrustOK:                 true, TrustUnknown: true,
		PermissionModeOK: true, PermissionModeUnknown: true,
	})
	if verdict.Class != WorkflowErrorProviderAuthInteractive {
		t.Fatalf("class = %q, want the interactive class to win", verdict.Class)
	}
}

// A missing binary still outranks everything: there is no point telling a
// person about credentials for a CLI that is not installed.
func TestEvaluateWorkerPreflight_BinaryStillOutranksInteractiveAuth(t *testing.T) {
	verdict := evaluateWorkerPreflight(domain.HarnessClaudeCode, "/tmp/ws", WorkerPreflightResult{
		BinaryOK:                false,
		AuthRequiresInteraction: true,
		TrustOK:                 true, TrustUnknown: true,
		PermissionModeOK: true, PermissionModeUnknown: true,
	})
	if verdict.Class != domain.WorkflowErrorBinaryMissing {
		t.Fatalf("class = %q, want the missing binary to win", verdict.Class)
	}
}

// Every stop AO can reach must carry an action a person can take. A reason
// with no registered disposition renders as a dead end on the Board.
func TestAttentionDisposition_CoversTheInteractiveAuthReasons(t *testing.T) {
	for _, reason := range []string{ReasonProviderAuthInteractive, ReasonPlannerAuthInteractive} {
		disposition, ok := attentionDispositions[reason]
		if !ok {
			t.Fatalf("reason %q has no registered attention disposition", reason)
		}
		if strings.TrimSpace(disposition.HumanAction) == "" {
			t.Fatalf("reason %q has no human action", reason)
		}
		// The remedy must name at least one of the two real ways out, or it
		// repeats the mistake of telling people to sign in again.
		action := disposition.HumanAction
		if !strings.Contains(action, "ANTHROPIC_API_KEY") && !strings.Contains(action, "apiKeyHelper") {
			t.Fatalf("reason %q does not name an unattended credential remedy: %q", reason, action)
		}
	}
}

// The error class must map to the same disposition, so a stop recorded by
// class and one recorded by reason say the same thing.
func TestAttentionDisposition_InteractiveAuthClassMapsToItsReason(t *testing.T) {
	byClass, ok := attentionErrorClasses[WorkflowErrorProviderAuthInteractive]
	if !ok {
		t.Fatal("provider_auth_interactive has no disposition by error class")
	}
	if byClass.HumanAction != attentionDispositions[ReasonProviderAuthInteractive].HumanAction {
		t.Fatal("the class and reason dispositions for interactive auth disagree")
	}
}

// A reviewer is as unattended as a worker, so a refusal for the same reason
// must stop it the same way -- and must never be retried, because waiting does
// not unlock a keychain. Fixing planner and worker while the reviewer kept
// launching into the dialog is the shape of half-fix this guards against.
func TestClassifyReviewerLaunchFailure_InteractiveAuthIsPermanent(t *testing.T) {
	refusal := &ErrProviderPreflight{
		Class:  WorkflowErrorProviderAuthInteractive,
		Reason: ReasonProviderAuthInteractive,
		Detail: "provider preflight: reaching claude-code's credentials would require answering an interactive prompt",
	}
	cls := classifyReviewerLaunchFailure(fmt.Errorf("launch reviewer: %w", refusal))
	if cls.Retryable {
		t.Fatal("a reviewer refused for needing an interactive prompt was marked retryable; waiting does not unlock a keychain")
	}
	if cls.Class != WorkflowErrorProviderAuthInteractive {
		t.Fatalf("class = %q, want %q", cls.Class, WorkflowErrorProviderAuthInteractive)
	}
	if cls.Reason != ReasonProviderAuthInteractive {
		t.Fatalf("reason = %q, want %q", cls.Reason, ReasonProviderAuthInteractive)
	}
}

// The worker and the reviewer must reach the same verdict from the same
// refusal. Two classifiers that disagree is how one role gets fixed and
// another quietly does not.
func TestPreflightRefusal_ClassifiesIdenticallyForWorkerAndReviewer(t *testing.T) {
	for _, class := range []domain.WorkflowErrorClass{
		WorkflowErrorProviderAuthInteractive,
		WorkflowErrorProviderAuthRequired,
		WorkflowErrorProviderWorkspaceTrustRequired,
	} {
		refusal := &ErrProviderPreflight{Class: class, Reason: string(class), Detail: "refused"}
		worker, ok := classifyPreflightRefusal(refusal)
		if !ok {
			t.Fatalf("the worker classifier did not recognise a preflight refusal for %q", class)
		}
		reviewer := classifyReviewerLaunchFailure(refusal)
		if worker.Class != reviewer.Class || worker.Reason != reviewer.Reason || worker.Retryable != reviewer.Retryable {
			t.Fatalf("worker and reviewer disagree on %q: worker=%+v reviewer=%+v", class, worker, reviewer)
		}
	}
}

// An error that is not a preflight refusal must be untouched by the new branch.
func TestClassifyReviewerLaunchFailure_LeavesOtherFailuresAlone(t *testing.T) {
	cls := classifyReviewerLaunchFailure(errors.New("connection refused"))
	if !cls.Retryable {
		t.Fatal("an ordinary transient launch failure stopped being retryable")
	}
}
