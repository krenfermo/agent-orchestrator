package command

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

func validEnvelope(t *testing.T) []byte {
	t.Helper()
	plan := workflowcore.MasterPlan{
		Version: "v1", Objective: "Build users", Summary: "one step",
		Steps: []workflowcore.PlannedStep{{
			ID: "s1", Title: "Step", Description: "Do it",
			Dependencies: []string{}, AcceptanceCriteria: []string{"done"},
			Verify: workflowcore.VerificationPlan{Commands: []workflowcore.VerificationCommandCheck{}, Files: []workflowcore.VerificationFileCheck{}},
		}},
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	env, err := json.Marshal(struct {
		StructuredOutput json.RawMessage `json:"structured_output"`
	}{StructuredOutput: raw})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// medusaSizedRequest builds a request whose prompt+context payload is
// comparable in size to the real MEDUSA workflow prompts that motivated
// Checkpoint 8P-E.10 (tens of KB of objective text plus repository
// context) -- large enough that scaledTimeout must grow past defaultTimeout.
func medusaSizedRequest() workflowcore.PlannerRequest {
	objective := strings.Repeat("CHECKPOINT 8P-E.10 long objective line describing required behavior in detail. ", 800) // ~65KB
	docs := make([]workflowcore.PlannerDocument, 0, 20)
	for i := 0; i < 20; i++ {
		docs = append(docs, workflowcore.PlannerDocument{Path: "file.go", SHA256: "x", Content: strings.Repeat("package x\nfunc F() {}\n", 200)})
	}
	return workflowcore.PlannerRequest{
		Objective: objective,
		Project:   domain.ProjectRecord{ID: "p", Path: "."},
		Context:   workflowcore.PlannerContext{Version: "v1", ProjectID: "p", Documents: docs},
		MaxSteps:  12,
	}
}

// The step-allowance term is passed as 0 here on purpose: these two cases
// pin the INPUT half of the budget, which must keep behaving exactly as
// Checkpoint 8P-E.10 defined it. The output half has its own tests below.
func TestScaledTimeout_SmallPayloadStaysAtBase(t *testing.T) {
	got := scaledTimeout(defaultTimeout, defaultMaxTimeout, 100, 0)
	if got != defaultTimeout {
		t.Fatalf("small payload should not be scaled up, got %s", got)
	}
}

func TestScaledTimeout_LargePayloadScalesButStaysBounded(t *testing.T) {
	got := scaledTimeout(defaultTimeout, defaultMaxTimeout, 10*1024*1024, 0) // 10MB, way past any real prompt
	if got != defaultMaxTimeout {
		t.Fatalf("oversized payload must be capped at MaxTimeout, got %s", got)
	}
	mid := scaledTimeout(defaultTimeout, defaultMaxTimeout, 400*1024, 0) // 400KB
	if mid <= defaultTimeout || mid > defaultMaxTimeout {
		t.Fatalf("mid-size payload should scale strictly between base and max, got %s", mid)
	}
}

func TestGenerate_MedusaSizedPromptGetsMoreThanDefaultTimeout(t *testing.T) {
	req := medusaSizedRequest()
	var gotDeadline time.Time
	p := Planner{Binary: "claude", Timeout: defaultTimeout, MaxTimeout: defaultMaxTimeout}
	p.runCommand = func(ctx context.Context, binary string, args []string, dir string, env []string) ([]byte, error) {
		gotDeadline, _ = ctx.Deadline()
		return validEnvelope(t), nil
	}
	if _, err := p.Generate(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if remaining := time.Until(gotDeadline); remaining <= defaultTimeout {
		t.Fatalf("MEDUSA-sized prompt did not get a scaled-up deadline: %s remaining (base=%s)", remaining, defaultTimeout)
	}
}

func TestGenerate_TimeoutClassifiesAsErrPlannerTimeout(t *testing.T) {
	p := Planner{Binary: "claude", Timeout: 10 * time.Millisecond, MaxTimeout: 10 * time.Millisecond}
	p.runCommand = func(ctx context.Context, binary string, args []string, dir string, env []string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	_, err := p.Generate(context.Background(), workflowcore.PlannerRequest{Objective: "x", Project: domain.ProjectRecord{ID: "p"}, MaxSteps: 4})
	if err == nil || !errors.Is(err, ports.ErrPlannerTimeout) {
		t.Fatalf("want ErrPlannerTimeout, got %v", err)
	}
}

func TestGenerate_ToleratesHarmlessLeadingAndTrailingProse(t *testing.T) {
	p := Planner{Binary: "claude", Timeout: defaultTimeout}
	p.runCommand = func(ctx context.Context, binary string, args []string, dir string, env []string) ([]byte, error) {
		var b []byte
		b = append(b, []byte("Loading model, please wait...\n")...)
		b = append(b, validEnvelope(t)...)
		b = append(b, []byte("\n(session ended)\n")...)
		return b, nil
	}
	resp, err := p.Generate(context.Background(), workflowcore.PlannerRequest{Objective: "x", Project: domain.ProjectRecord{ID: "p"}, MaxSteps: 4})
	if err != nil {
		t.Fatalf("harmless surrounding prose should not fail parsing: %v", err)
	}
	if resp.Plan.Version != "v1" || len(resp.Plan.Steps) != 1 {
		t.Fatalf("plan not extracted correctly: %+v", resp.Plan)
	}
}

func TestGenerate_RejectsGenuinelyMalformedOutput(t *testing.T) {
	var calls int32
	p := Planner{Binary: "claude", Timeout: defaultTimeout}
	p.runCommand = func(ctx context.Context, binary string, args []string, dir string, env []string) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		return []byte("Internal error: the assistant could not comply with this request."), nil
	}
	_, err := p.Generate(context.Background(), workflowcore.PlannerRequest{Objective: "x", Project: domain.ProjectRecord{ID: "p"}, MaxSteps: 4})
	if err == nil || !errors.Is(err, ports.ErrPlannerOutputMalformed) {
		t.Fatalf("want ErrPlannerOutputMalformed, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != int32(maxParseRetries+1) {
		t.Fatalf("retry must be bounded: want %d calls, got %d", maxParseRetries+1, got)
	}
}

func TestGenerate_RetriesOnceOnTransientParseFailureThenSucceeds(t *testing.T) {
	var calls int32
	p := Planner{Binary: "claude", Timeout: defaultTimeout}
	p.runCommand = func(ctx context.Context, binary string, args []string, dir string, env []string) ([]byte, error) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			return []byte("not json at all"), nil
		}
		return validEnvelope(t), nil
	}
	resp, err := p.Generate(context.Background(), workflowcore.PlannerRequest{Objective: "x", Project: domain.ProjectRecord{ID: "p"}, MaxSteps: 4})
	if err != nil {
		t.Fatalf("expected retry to recover, got %v", err)
	}
	if resp.Plan.Version != "v1" {
		t.Fatalf("unexpected plan: %+v", resp.Plan)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("want exactly 2 attempts, got %d", got)
	}
}

func TestGenerate_DoesNotRetryCommandOrTimeoutFailures(t *testing.T) {
	var calls int32
	p := Planner{Binary: "claude", Timeout: defaultTimeout}
	p.runCommand = func(ctx context.Context, binary string, args []string, dir string, env []string) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		return []byte("permission denied"), errors.New("exit status 1")
	}
	_, err := p.Generate(context.Background(), workflowcore.PlannerRequest{Objective: "x", Project: domain.ProjectRecord{ID: "p"}, MaxSteps: 4})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("a real subprocess/exec failure must not be retried, got %d calls", got)
	}
}

func TestGenerate_MalformedOutputErrorCarriesRawTextForUpstreamClassification(t *testing.T) {
	p := Planner{Binary: "claude", Timeout: defaultTimeout}
	p.runCommand = func(ctx context.Context, binary string, args []string, dir string, env []string) ([]byte, error) {
		return []byte("Error: rate limit exceeded, please retry later"), nil
	}
	_, err := p.Generate(context.Background(), workflowcore.PlannerRequest{Objective: "x", Project: domain.ProjectRecord{ID: "p"}, MaxSteps: 4})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "rate limit exceeded") {
		t.Fatalf("provider's plain-text error text must survive in the wrapped error so classifyProviderFailure can see it, got: %v", err)
	}
}
