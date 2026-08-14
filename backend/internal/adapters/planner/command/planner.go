package command

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

type Planner struct {
	Binary  string
	Model   string
	Timeout time.Duration
}

func (p Planner) Descriptor() (string, string) {
	model := p.Model
	if model == "" {
		model = "sonnet"
	}
	return "anthropic", model
}

func (p Planner) Generate(ctx context.Context, req workflowcore.PlannerRequest) (workflowcore.PlannerResponse, error) {
	if p.Binary == "" {
		return workflowcore.PlannerResponse{}, fmt.Errorf("planner binary is required")
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	contextJSON, _ := json.Marshal(req.Context)
	prompt := fmt.Sprintf(`Act only as a software master planner. You cannot use tools, edit files, run commands, commit, push, or claim work is complete.
Decompose the objective into small, independently verifiable implementation units. Preserve a simple dependency DAG and no more than %d steps; for a small objective prefer 2-4 substantial steps and do not plan work already present in the repository context. Every step must require a durable code, test, or documentation change; never create a verification-only step, and instead include final checks in the last implementation step. Every step needs concrete acceptance criteria and safe structured verification checks. Verification commands are executable plus argument arrays, never shell snippets. Do not use shells, destructive commands, deployment tools, git mutation commands, absolute paths, or paths outside the workspace.

Objective: %s

Conservative repository context:
%s`, req.MaxSteps, req.Objective, string(contextJSON))
	schema := fmt.Sprintf(`{"type":"object","additionalProperties":false,"required":["version","objective","summary","steps"],"properties":{"version":{"const":"v1"},"objective":{"type":"string"},"summary":{"type":"string"},"steps":{"type":"array","minItems":1,"maxItems":%d,"items":{"type":"object","additionalProperties":false,"required":["id","title","description","dependencies","acceptanceCriteria","verify"],"properties":{"id":{"type":"string"},"title":{"type":"string"},"description":{"type":"string"},"dependencies":{"type":"array","items":{"type":"string"}},"acceptanceCriteria":{"type":"array","minItems":1,"items":{"type":"string"}},"verify":{"type":"object","additionalProperties":false,"required":["commands","files"],"properties":{"commands":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["command","args","workingDirectory","timeoutSeconds","requiredExitCode","retrySafe"],"properties":{"command":{"type":"string"},"args":{"type":"array","items":{"type":"string"}},"workingDirectory":{"type":"string"},"timeoutSeconds":{"type":"integer","minimum":1,"maximum":3600},"requiredExitCode":{"type":"integer"},"retrySafe":{"type":"boolean"}}}},"files":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["path","exists"],"properties":{"path":{"type":"string"},"exists":{"type":"boolean"},"exactContent":{"type":"string"},"sha256":{"type":"string"}}}}}}}}}}}`, req.MaxSteps)
	model := p.Model
	if model == "" {
		model = "sonnet"
	}
	cmd := exec.CommandContext(callCtx, p.Binary, "--print", "--output-format", "json", "--json-schema", schema, "--tools", "", "--permission-mode", "plan", "--no-session-persistence", "--model", model, prompt)
	cmd.Dir = req.Project.Path
	b, err := cmd.CombinedOutput()
	if err != nil {
		if callCtx.Err() != nil {
			return workflowcore.PlannerResponse{}, fmt.Errorf("planner timeout: %w", callCtx.Err())
		}
		return workflowcore.PlannerResponse{}, fmt.Errorf("planner command: %w: %s", err, strings.TrimSpace(string(b)))
	}
	var envelope struct {
		StructuredOutput json.RawMessage `json:"structured_output"`
		Result           string          `json:"result"`
	}
	if err := json.Unmarshal(b, &envelope); err != nil {
		return workflowcore.PlannerResponse{}, fmt.Errorf("planner parse envelope: %w", err)
	}
	raw := envelope.StructuredOutput
	if len(raw) == 0 && envelope.Result != "" {
		raw = []byte(envelope.Result)
	}
	var plan workflowcore.MasterPlan
	if len(raw) == 0 {
		return workflowcore.PlannerResponse{}, fmt.Errorf("planner output missing structured plan")
	}
	if err := json.Unmarshal(raw, &plan); err != nil {
		return workflowcore.PlannerResponse{}, fmt.Errorf("planner parse plan: %w", err)
	}
	return workflowcore.PlannerResponse{Plan: plan, Provider: "anthropic", Model: model}, nil
}
