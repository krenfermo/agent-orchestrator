package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// P9 review: refining a concluded attempt is a compare-and-swap on the outcome
// the caller read. A verdict recorded since then matches no row and is never
// overwritten; an open attempt is never "refined" into a conclusion.
func TestRefineConcludedWorkflowAttemptIsFencedByTheReadOutcome(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj-refine")
	now := time.Now().UTC().Truncate(time.Second)
	_, steps, err := s.CreateWorkflowRun(ctx, sampleWorkflowRun("proj-refine", "wf-refine", now), sampleWorkflowSteps("wf-refine", now))
	if err != nil {
		t.Fatal(err)
	}
	stepID := steps[0].ID
	if _, err := s.CreateWorkflowAttempt(ctx, "wfa-open", stepID, "claude-code", "sonnet", now); err != nil {
		t.Fatal(err)
	}

	// An OPEN attempt is not concluded: refinement must not close it.
	if ok, err := s.RefineConcludedWorkflowAttempt(ctx, "wfa-open", "", now, domain.WorkflowAttemptFailed, domain.WorkflowErrorTransient); err != nil || ok {
		t.Fatalf("refined an open attempt: ok=%v err=%v", ok, err)
	}

	if ok, err := s.ClaimWorkflowAttemptOutcome(ctx, "wfa-open", now, domain.WorkflowAttemptFailed, ""); err != nil || !ok {
		t.Fatalf("conclude: ok=%v err=%v", ok, err)
	}
	// A refinement expecting a different outcome (a stale read) writes nothing.
	if ok, err := s.RefineConcludedWorkflowAttempt(ctx, "wfa-open", domain.WorkflowAttemptSucceeded, now.Add(time.Minute), domain.WorkflowAttemptFailed, domain.WorkflowErrorTransient); err != nil || ok {
		t.Fatalf("stale refinement applied: ok=%v err=%v", ok, err)
	}

	// Two refiners that read the same outcome: exactly one wins.
	var wg sync.WaitGroup
	wins := make(chan bool, 2)
	for _, out := range []domain.WorkflowAttemptOutcome{domain.WorkflowAttemptSucceeded, domain.WorkflowAttemptCancelled} {
		wg.Add(1)
		go func(out domain.WorkflowAttemptOutcome) {
			defer wg.Done()
			ok, err := s.RefineConcludedWorkflowAttempt(ctx, "wfa-open", domain.WorkflowAttemptFailed, now.Add(time.Minute), out, "")
			if err != nil {
				t.Errorf("refine: %v", err)
			}
			wins <- ok
		}(out)
	}
	wg.Wait()
	close(wins)
	n := 0
	for ok := range wins {
		if ok {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("refiners that read the same outcome: %d won, want exactly 1", n)
	}
	got, ok, err := s.GetLatestWorkflowAttempt(ctx, stepID)
	if err != nil || !ok || got.FinishedAt == nil || got.Outcome == domain.WorkflowAttemptFailed {
		t.Fatalf("attempt after the race = %+v ok=%v err=%v", got, ok, err)
	}
}
