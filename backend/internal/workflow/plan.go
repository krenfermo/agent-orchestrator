package workflow

import (
	"encoding/json"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// PlanArtifact is the plan step's deterministic, structured output. Building
// it is a pure template expansion, not a call to any LLM/planner service —
// Checkpoint 8B's plan step does no IO beyond persisting this JSON.
type PlanArtifact struct {
	Objective          string           `json:"objective"`
	TaskPrompt         string           `json:"taskPrompt"`
	AcceptanceCriteria []string         `json:"acceptanceCriteria"`
	ProjectID          string           `json:"projectId"`
	PolicyVersion      string           `json:"policyVersion"`
	Verification       VerificationPlan `json:"verification"`
}

type VerificationPlan struct {
	Commands []VerificationCommandCheck `json:"commands,omitempty"`
	Files    []VerificationFileCheck    `json:"files,omitempty"`
}

type VerificationCommandCheck struct {
	Command          string   `json:"command"`
	Args             []string `json:"args,omitempty"`
	WorkingDirectory string   `json:"workingDirectory,omitempty"`
	TimeoutSeconds   int      `json:"timeoutSeconds,omitempty"`
	RequiredExitCode int      `json:"requiredExitCode"`
	RetrySafe        bool     `json:"retrySafe"`
}

type VerificationFileCheck struct {
	Path         string  `json:"path"`
	Exists       bool    `json:"exists"`
	ExactContent *string `json:"exactContent,omitempty"`
	SHA256       string  `json:"sha256,omitempty"`
}

// BuildPlanArtifact deterministically derives a PlanArtifact from a run's
// objective. No IO, no randomness, no model call: same inputs always produce
// the same artifact.
func BuildPlanArtifact(projectID, objective, policyVersion string, verification ...VerificationPlan) PlanArtifact {
	artifact := PlanArtifact{
		Objective: objective,
		AcceptanceCriteria: []string{
			"Objective is addressed by a concrete code change in the worktree.",
			"No unrelated files are modified.",
			"Existing tests are not knowingly broken.",
		},
		ProjectID:     projectID,
		PolicyVersion: policyVersion,
	}
	if len(verification) > 0 {
		artifact.Verification = verification[0]
	}
	artifact.TaskPrompt = BuildWorkStepPrompt(artifact)
	return artifact
}

// MarshalPlanArtifact serializes a PlanArtifact for storage in
// workflow_steps.artifact_json.
func MarshalPlanArtifact(p PlanArtifact) (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("marshal plan artifact: %w", err)
	}
	return string(b), nil
}

// UnmarshalPlanArtifact parses a plan step's artifact_json column back into a
// PlanArtifact. An empty or "{}" input yields the zero value with no error.
func UnmarshalPlanArtifact(raw string) (PlanArtifact, error) {
	var artifact PlanArtifact
	if raw == "" {
		return artifact, nil
	}
	if err := json.Unmarshal([]byte(raw), &artifact); err != nil {
		return PlanArtifact{}, fmt.Errorf("unmarshal plan artifact: %w", err)
	}
	return artifact, nil
}

// BuildWorkStepPrompt combines the plan artifact's objective/acceptance
// criteria with fixed guardrail language into the actual prompt text handed
// to the Codex worker via ports.SpawnConfig.Prompt. Pure and deterministic:
// no IO, no model call.
func BuildWorkStepPrompt(artifact PlanArtifact) string {
	return BuildWorkStepPromptWithSpec(artifact, "")
}

// BuildWorkStepPromptWithSpec is BuildWorkStepPrompt plus the approved
// amendments that reconcile the objective with the criteria in force
// (RenderEffectiveSpecification). It is a separate entry point because the
// worker prompt is also built at PLAN time, before any amendment can exist,
// and that call must keep producing the byte-identical prompt it always did.
// An empty spec makes the two functions the same function.
func BuildWorkStepPromptWithSpec(artifact PlanArtifact, effectiveSpec string) string {
	var criteria string
	for _, c := range artifact.AcceptanceCriteria {
		criteria += "- " + c + "\n"
	}
	return fmt.Sprintf(`You are the worker agent for an AO-managed workflow run.

Objective: %s

Your task: implement the objective above as a concrete, reviewable code
change in this worktree.

Acceptance criteria:
%s%s
Guardrails (follow all of these):
- Work only inside the current worktree. Do not touch files outside it.
- Run any reasonable/available test suite for this project before
  considering the task done.
- Do NOT push, do NOT merge, and do NOT modify any branch other than the
  current one.
- Do NOT open, request, or interact with any pull request.

When you are done (or if you get stuck), report the outcome clearly in your
final message: what changed, what you tested, and whether it succeeded. This
report is informational only — AO verifies your work independently from the
actual state of the worktree, not from what you say here, so be honest about
partial progress or failures.`, artifact.Objective, criteria, effectiveSpec)
}

// promptForRun reconstructs the work step's task prompt from the plan step's
// already-persisted artifact JSON, used when re-entering dispatch from
// recovery (no in-memory PlanArtifact survives a daemon restart). Falls back
// to a deterministic rebuild from the run's objective if the plan step's
// artifact is somehow still empty — BuildPlanArtifact is pure, so the rebuilt
// prompt is byte-identical to what StartRun would have produced.
func promptForRun(run domain.WorkflowRun, steps []domain.WorkflowStep) string {
	for _, s := range steps {
		if s.Kind != domain.WorkflowStepPlan || s.ArtifactJSON == "" || s.ArtifactJSON == "{}" {
			continue
		}
		if artifact, err := UnmarshalPlanArtifact(s.ArtifactJSON); err == nil && artifact.TaskPrompt != "" {
			return artifact.TaskPrompt
		}
	}
	return BuildWorkStepPrompt(BuildPlanArtifact(run.ProjectID, run.Objective, run.PolicyVersion))
}
