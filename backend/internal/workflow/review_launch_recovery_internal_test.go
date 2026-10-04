package workflow

import (
	"errors"
	"fmt"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// TestClassifyReviewerLaunchFailure pins the split the recovery lifecycle turns
// on: which reviewer-launch failures AO may retry by itself, and which are a
// person's to resolve. Getting this wrong in either direction is a real failure
// mode — retrying an auth error forever, or billing a momentary spawn failure to
// a human — so the table is the contract.
func TestClassifyReviewerLaunchFailure(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantClass  domain.WorkflowErrorClass
		wantRetry  bool
		wantReason string
	}{
		{
			name:      "typed auth failure is permanent",
			err:       fmt.Errorf("start reviewer: %w", ports.ErrChatAuthRequired),
			wantClass: domain.WorkflowErrorAuth, wantRetry: false, wantReason: ReasonReviewerAuthInvalid,
		},
		{
			name:      "missing provider profile is permanent (auth-shaped)",
			err:       fmt.Errorf("resolve runtime: %w", ports.ErrProviderProfileRequired),
			wantClass: domain.WorkflowErrorAuth, wantRetry: false, wantReason: ReasonReviewerAuthInvalid,
		},
		{
			name:      "missing binary is permanent",
			err:       fmt.Errorf("preflight: %w", ports.ErrAgentBinaryNotFound),
			wantClass: domain.WorkflowErrorBinaryMissing, wantRetry: false, wantReason: ReasonReviewerBinaryMissing,
		},
		{
			name:      "unsupported configuration is permanent",
			err:       errors.New("launch reviewer: unsupported configuration for harness"),
			wantClass: domain.WorkflowErrorReviewerLaunchFailed, wantRetry: false, wantReason: ReasonReviewerLaunchUnsupported,
		},
		{
			name:      "explicit policy violation is permanent",
			err:       errors.New("launch reviewer: policy violation: reviewer independence required"),
			wantClass: domain.WorkflowErrorReviewerLaunchFailed, wantRetry: false, wantReason: ReasonReviewerLaunchUnsupported,
		},
		{
			name:      "temporary spawn failure is retryable",
			err:       errors.New("launch reviewer: fork/exec: resource temporarily unavailable"),
			wantClass: domain.WorkflowErrorTransient, wantRetry: true,
		},
		{
			name:      "transport failure is retryable",
			err:       errors.New("launch reviewer: connection refused"),
			wantClass: domain.WorkflowErrorTransient, wantRetry: true,
		},
		{
			name:      "runtime unavailable is retryable",
			err:       errors.New("launch reviewer: tmux server not running"),
			wantClass: domain.WorkflowErrorRuntimeFailed, wantRetry: true,
		},
		{
			name:      "provider capacity is retryable",
			err:       errors.New("launch reviewer: usage limit reached"),
			wantClass: domain.WorkflowErrorCapacityExhausted, wantRetry: true,
		},
		{
			name:      "rate limit is retryable",
			err:       errors.New("launch reviewer: 429 too many requests"),
			wantClass: domain.WorkflowErrorRateLimited, wantRetry: true,
		},
		{
			// An unnameable failure retries, but under the same bounded budget —
			// so it still reaches a human quickly instead of looping.
			name:      "unclassified failure is retryable but bounded",
			err:       errors.New("launch reviewer: something nobody has seen before"),
			wantClass: domain.WorkflowErrorAgentStartFailed, wantRetry: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyReviewerLaunchFailure(tc.err)
			if got.Class != tc.wantClass {
				t.Fatalf("class = %q, want %q", got.Class, tc.wantClass)
			}
			if got.Retryable != tc.wantRetry {
				t.Fatalf("retryable = %v, want %v", got.Retryable, tc.wantRetry)
			}
			if tc.wantReason != "" && got.Reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", got.Reason, tc.wantReason)
			}
			// Every classification must name a reason that carries a real human
			// action, because any of them can become a stop once the automatic
			// budget runs out.
			disp, ok := attentionDispositions[got.Reason]
			if !ok || disp.HumanAction == "" {
				t.Fatalf("reason %q has no human action registered", got.Reason)
			}
			if !got.Class.Valid() {
				t.Fatalf("class %q is not persistable", got.Class)
			}
		})
	}
}

// TestReviewLaunchStopReasonsAreRegistered keeps the clear-stop list honest: a
// reason this file can park a run on, but that clearReviewLaunchStop does not
// know about, would leave a recovered run reporting "needs attention" forever.
func TestReviewLaunchStopReasonsAreRegistered(t *testing.T) {
	for reason := range reviewLaunchStopReasons {
		if _, ok := attentionDispositions[reason]; !ok {
			t.Fatalf("reviewer-launch stop reason %q is not in the canonical registry", reason)
		}
	}
}
