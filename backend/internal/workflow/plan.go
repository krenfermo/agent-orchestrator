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
	// WriteIntent is the planned task's declared write intent, carried into the
	// EXECUTION run's own plan step so the completion classifier can read it
	// from a durable row rather than from the master run it may not be able to
	// reach.
	//
	// This is the propagation hop that makes the semantic restart-safe: the
	// artifact is persisted in workflow_steps.artifact_json at dispatch, so a
	// daemon that comes back up mid-run resolves the same intent, and therefore
	// the same completion verdict, that it would have resolved before the
	// crash. Empty for every standalone objective and every legacy plan, which
	// is Unspecified, which is treated as mutating.
	WriteIntent domain.WorkflowWriteIntent `json:"writeIntent,omitempty"`
	// Strategy is the run's frozen execution strategy, bound at create time so
	// the work prompt can carry Checkpoint P7's turn-economy section for a
	// TASK and not for an open-ended run.
	//
	// Empty for every artifact written before P7 and for any caller that does
	// not bind one, which is what keeps every previously-built prompt
	// byte-identical: the section is appended only when this field says TASK.
	Strategy domain.ExecutionStrategy `json:"strategy,omitempty"`
}

// VerificationPlan is how a planned task says it can be checked: commands that
// must succeed, and files that must (or must not) be there afterwards.
type VerificationPlan struct {
	Commands []VerificationCommandCheck `json:"commands,omitempty"`
	Files    []VerificationFileCheck    `json:"files,omitempty"`
}

// VerificationCommandCheck is one command a verification runs. RetrySafe marks
// a command AO may run more than once -- a verification that is not retry-safe
// must not be re-run to resolve an ambiguous result.
type VerificationCommandCheck struct {
	Command          string   `json:"command"`
	Args             []string `json:"args,omitempty"`
	WorkingDirectory string   `json:"workingDirectory,omitempty"`
	TimeoutSeconds   int      `json:"timeoutSeconds,omitempty"`
	RequiredExitCode int      `json:"requiredExitCode"`
	RetrySafe        bool     `json:"retrySafe"`
}

// VerificationFileCheck is one artifact assertion. Exists false asserts the
// path is absent; ExactContent and SHA256 assert what is in it when it is
// present.
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
	// A task the plan declared read-only is told so, and told it in place of
	// the "implement a concrete code change" instruction rather than after it:
	// handing a verification task both sentences is handing it a contradiction,
	// and the honest readings of that prompt disagree about what to do. The
	// mutating text is byte-identical to what it always was, so every task that
	// declares nothing produces exactly the prompt it produced before.
	task := `Your task: implement the objective above as a concrete, reviewable code
change in this worktree.`
	extraGuardrail := ""
	if artifact.WriteIntent.ReadOnly() {
		task = `Your task: carry out the objective above as a READ-ONLY task. Its accepted
outcome is that this worktree is left exactly as you found it: run the checks,
inspect what you were asked to inspect, and report what you found.`
		extraGuardrail = `
- This task is READ-ONLY. Do NOT create, edit or delete any file, and do NOT
  commit or stage anything. Any pre-existing uncommitted changes in this
  worktree must be left exactly as they are. AO verifies this independently by
  comparing the worktree against the state you were handed, so an unexpected
  change stops the run for a person to look at.`
	}
	return fmt.Sprintf(`You are the worker agent for an AO-managed workflow run.

Objective: %s

%s

Acceptance criteria:
%s%s
Guardrails (follow all of these):
- Work only inside the current worktree. Do not touch files outside it.
- Run any reasonable/available test suite for this project before
  considering the task done.
- Do NOT push, do NOT merge, and do NOT modify any branch other than the
  current one.
- Do NOT open, request, or interact with any pull request.%s

When you are done (or if you get stuck), report the outcome clearly in your
final message: what changed, what you tested, and whether it succeeded. This
report is informational only — AO verifies your work independently from the
actual state of the worktree, not from what you say here, so be honest about
partial progress or failures.

Before you finish, ALSO record that report in a form AO keeps and hands to the
reviewer:

  ao work report --json -

reading a JSON object on stdin with any of: summary, criteria
[{criterion, addressed, note}], testsReported [{command, claimedOutcome:
claimed_passed|claimed_failed|claimed_skipped, note}], limitations, risks,
followUp, claimedChangedPaths, commit. The short form works too, e.g.
%[6]sao work report --summary "..." --test "go test ./...=passed" --limitation "..."%[6]s.

Two things about it. It is a DECLARATION, not proof: AO runs this task's own
checks itself before anyone reviews the change, and claiming a test passed
gains you nothing. And declaring a limitation, a risk, or a criterion you did
NOT address makes AO review the change more carefully rather than less — so
saying what you did not finish is the most useful thing you can put in it.%[7]s`, artifact.Objective, task, criteria, effectiveSpec, extraGuardrail, "`", turnEconomySection(artifact.Strategy))
}

// turnEconomySection is Checkpoint P7's turn-economy guidance, and it is
// appended to a TASK's work prompt only.
//
// WHY ONLY A TASK. Every line below trades breadth for turns, and that trade
// is right for a bounded change with a stated objective and wrong for an
// open-ended one. An autonomous run that is asked not to explore the
// repository has been asked not to do its job, so it gets none of this and its
// wider advisory profile stays exactly as it was.
//
// WHY IT IS PHRASED AS HABITS AND NOT AS LIMITS. AO cannot enforce any of it:
// it does not own the model loop, it cannot cap a turn count, and a hard
// instruction like "use at most N commands" would buy a cheap run that stops
// before it is finished. These are the habits the measured run's own shape
// argues for, stated as preferences a competent agent can weigh against the
// work in front of it.
//
// WHAT IT IS NOT. It is NOT a measured saving. The two levers this checkpoint
// quantified -- the call count and the size of the conversation each call
// re-reads -- were quantified by replaying a recorded run
// (internal/observe/turnbench). The effect of this text cannot be replayed,
// because changing it changes which calls the agent makes, and there is no
// recording of the run it would have produced. It is here because it is cheap,
// because it is true, and because the alternative is saying nothing; it is
// deliberately not counted in any before/after figure.
func turnEconomySection(strategy domain.ExecutionStrategy) string {
	if strategy != domain.ExecutionStrategyTask {
		return ""
	}
	return `

How to spend your turns on this task (it is a bounded change, not an
exploration):
- Every message you send re-reads this whole conversation, so the cost of a
  turn grows with what you have already said. Prefer one command that answers
  several related questions to several commands that each answer one.
- Read what the task points at. Do not survey the repository first; widen only
  when something you actually read sends you somewhere else.
- Wait for long work OUTSIDE a turn. Run it in the background and let its
  completion wake you, or block on it inside the same command. Do not spend a
  turn asking whether it has finished.
- Verify once. Re-running a check whose inputs have not changed since it passed
  tells you nothing you did not already know.
- Keep the messages between actions short. The report that matters is the final
  one and the ` + "`ao work report`" + ` you record beside it; a running commentary in
  between is re-read by every turn that follows it.
- If a well-bounded question would take several turns of reading to answer --
  where something is defined, why one test fails -- delegating it to a subagent
  keeps that reading out of this conversation. Delegate the QUESTION only:
  the change itself, the commits and the report stay in this session, because
  they are what AO tracks and reviews.`
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
