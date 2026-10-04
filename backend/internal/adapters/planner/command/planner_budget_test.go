package command

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// wf80dc9f12Payload is the measured shape of the planner call that failed in
// production (workflow wf-80dc9f12): a 6.4 KB objective, a context whose
// document bodies had been silently emptied before it was sent, and a 12-step
// ceiling. Reproduced outside AO with the identical arguments it took 5m36s --
// six provider turns and ~40k output tokens -- against a 3-minute budget.
const (
	wf80dc9f12PayloadBytes  = 8366
	wf80dc9f12MaxSteps      = 12
	wf80dc9f12MeasuredCost  = 5*time.Minute + 36*time.Second
	wf80dc9f12OldCalculated = 3 * time.Minute
)

func TestScaledTimeout_StepAllowanceCoversASmallPayloadWithManySteps(t *testing.T) {
	// The regression gate for wf-80dc9f12: this exact payload used to compute
	// the bare floor, because the only term in the budget was input size.
	if old := defaultTimeout + time.Duration(wf80dc9f12PayloadBytes/bytesPerExtraMinute)*time.Minute; old != wf80dc9f12OldCalculated {
		t.Fatalf("input-only budget for the production payload changed: %s", old)
	}
	got := scaledTimeout(defaultTimeout, defaultMaxTimeout, wf80dc9f12PayloadBytes, wf80dc9f12MaxSteps)
	if got <= wf80dc9f12MeasuredCost {
		t.Fatalf("budget %s does not cover the measured %s the identical call really took", got, wf80dc9f12MeasuredCost)
	}
	if got > defaultMaxTimeout {
		t.Fatalf("budget %s exceeded MaxTimeout %s", got, defaultMaxTimeout)
	}
}

func TestScaledTimeout_StepAllowanceStaysBounded(t *testing.T) {
	if got := scaledTimeout(defaultTimeout, defaultMaxTimeout, 0, 10_000); got != defaultMaxTimeout {
		t.Fatalf("an absurd step count must still cap at MaxTimeout, got %s", got)
	}
	if got := scaledTimeout(defaultTimeout, defaultMaxTimeout, 10*1024*1024, wf80dc9f12MaxSteps); got != defaultMaxTimeout {
		t.Fatalf("both terms together must still cap at MaxTimeout, got %s", got)
	}
}

func TestGenerate_SmallRequestStaysNearTheFloor(t *testing.T) {
	var deadline time.Time
	p := Planner{Binary: "claude", Timeout: defaultTimeout, MaxTimeout: defaultMaxTimeout}
	p.runCommand = func(ctx context.Context, _ string, _ []string, _ string, _ []string) ([]byte, error) {
		deadline, _ = ctx.Deadline()
		return validEnvelope(t), nil
	}
	if _, err := p.Generate(context.Background(), workflowcore.PlannerRequest{
		Objective: "Add a health endpoint", Project: domain.ProjectRecord{ID: "p"}, MaxSteps: 3,
	}); err != nil {
		t.Fatal(err)
	}
	remaining := time.Until(deadline)
	if remaining <= defaultTimeout || remaining > defaultTimeout+3*perStepAllowance {
		t.Fatalf("a small 3-step request should stay near the floor, got %s (base=%s)", remaining, defaultTimeout)
	}
}

func TestGenerate_TimeoutCarriesDurableAttemptEvidence(t *testing.T) {
	p := Planner{Binary: "claude", Timeout: 20 * time.Millisecond, MaxTimeout: 20 * time.Millisecond}
	p.runCommand = func(ctx context.Context, _ string, _ []string, _ string, _ []string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	req := workflowcore.PlannerRequest{
		Objective: "Fix the runtime",
		Project:   domain.ProjectRecord{ID: "p"},
		MaxSteps:  wf80dc9f12MaxSteps,
		Context: workflowcore.PlannerContext{Version: "v1", Documents: []workflowcore.PlannerDocument{
			{Path: "AGENTS.md", SHA256: "x", Content: "a repository document"},
			{Path: "README.md", SHA256: "y", Content: "another one"},
		}},
	}
	_, err := p.Generate(context.Background(), req)
	if err == nil || !errors.Is(err, ports.ErrPlannerTimeout) {
		t.Fatalf("want ErrPlannerTimeout, got %v", err)
	}
	evidence, ok := workflowcore.PlannerEvidenceFrom(err)
	if !ok {
		t.Fatal("a timed-out planner attempt carried no evidence")
	}
	if evidence.Classification != workflowcore.PlannerAttemptTimeout {
		t.Fatalf("classification=%q", evidence.Classification)
	}
	if evidence.HasParentDeadline {
		t.Fatal("caller context had no deadline, evidence claims one")
	}
	if evidence.CalculatedTimeoutMS != 20 || evidence.EffectiveTimeoutMS != 20 {
		t.Fatalf("timeout evidence=%+v", evidence)
	}
	if evidence.ObjectiveBytes != len(req.Objective) || evidence.DocumentCount != 2 || evidence.MaxSteps != wf80dc9f12MaxSteps {
		t.Fatalf("shape evidence=%+v", evidence)
	}
	if evidence.ContextBytes <= 0 || evidence.PayloadBytes <= evidence.ContextBytes {
		t.Fatalf("payload evidence=%+v", evidence)
	}
	// Sizes and durations only: no prompt, objective text, document body or
	// environment value may reach durable storage.
	if state := evidence.JSON(); containsAny(state, req.Objective, "a repository document", "another one") {
		t.Fatalf("evidence leaked payload content: %s", state)
	}
}

func TestGenerate_ShortLivedCallerDeadlineIsRecordedAndClassifiedApart(t *testing.T) {
	p := Planner{Binary: "claude", Timeout: defaultTimeout, MaxTimeout: defaultMaxTimeout}
	p.runCommand = func(ctx context.Context, _ string, _ []string, _ string, _ []string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := p.Generate(ctx, workflowcore.PlannerRequest{Objective: "x", Project: domain.ProjectRecord{ID: "p"}, MaxSteps: 4})
	if err == nil || !errors.Is(err, ports.ErrPlannerTimeout) {
		t.Fatalf("want a retryable timeout classification, got %v", err)
	}
	evidence, ok := workflowcore.PlannerEvidenceFrom(err)
	if !ok {
		t.Fatal("no evidence on a caller-cancelled attempt")
	}
	// The distinction the wf-80dc9f12 postmortem could not make: this attempt
	// did NOT exhaust the planner's own budget, something above it died first.
	if evidence.Classification != workflowcore.PlannerAttemptParentCancelled {
		t.Fatalf("classification=%q, want %q", evidence.Classification, workflowcore.PlannerAttemptParentCancelled)
	}
	if !evidence.HasParentDeadline {
		t.Fatal("caller deadline not recorded")
	}
	if evidence.EffectiveTimeoutMS >= evidence.CalculatedTimeoutMS {
		t.Fatalf("effective deadline should be the caller's, evidence=%+v", evidence)
	}
}

func TestGenerate_MaxTimeoutRemainsEnforcedWithoutAnyCallerDeadline(t *testing.T) {
	var deadline time.Time
	p := Planner{Binary: "claude", Timeout: defaultTimeout, MaxTimeout: 4 * time.Minute}
	p.runCommand = func(ctx context.Context, _ string, _ []string, _ string, _ []string) ([]byte, error) {
		deadline, _ = ctx.Deadline()
		return validEnvelope(t), nil
	}
	if _, err := p.Generate(context.Background(), medusaSizedRequest()); err != nil {
		t.Fatal(err)
	}
	if remaining := time.Until(deadline); remaining > 4*time.Minute {
		t.Fatalf("attempt deadline %s exceeded MaxTimeout", remaining)
	}
}

func containsAny(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if needle != "" && strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}
