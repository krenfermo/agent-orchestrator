package workflow

// The durable path a write-intent declaration travels, asserted at every hop:
//
//	planner JSON -> PlannedStep.WriteIntent      (plan text, validated)
//	             -> WorkflowTaskScope.WriteIntent (task row, scope_json)
//	             -> PlanArtifact.WriteIntent      (child run, artifact_json)
//	             -> the completion classifier
//
// and the two compatibility properties that make it safe to add: a plan that
// declares nothing hashes exactly as it did before the field existed, and a
// scope row written before it existed deserializes to Unspecified rather than
// to a read-only claim nobody made.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func planWithIntent(intent domain.WorkflowWriteIntent) MasterPlan {
	return MasterPlan{
		Version:   MasterPlanVersion,
		Objective: "Verify current repository state",
		Steps: []PlannedStep{{
			ID:                 "s1",
			Title:              "Verify current repository state (build, tests, vet, git status)",
			Description:        "Run the existing build/test/vet commands and inspect git status. Make no edits.",
			AcceptanceCriteria: []string{"No source, test or documentation file is modified."},
			Verify: VerificationPlan{Commands: []VerificationCommandCheck{
				{Command: "go", Args: []string{"build", "./..."}, WorkingDirectory: "backend", TimeoutSeconds: 600, RequiredExitCode: 0, RetrySafe: true},
			}},
			WriteIntent: intent,
		}},
	}
}

func TestPlanValidationAcceptsDeclaredWriteIntentsAndRejectsUnreadableOnes(t *testing.T) {
	cases := []struct {
		name      string
		intent    domain.WorkflowWriteIntent
		wantValid bool
		wantKept  domain.WorkflowWriteIntent
	}{
		{"absent is legal and stays unspecified", "", true, domain.WorkflowWriteIntentUnspecified},
		{"mutating", domain.WorkflowWriteIntentMutating, true, domain.WorkflowWriteIntentMutating},
		{"read_only", domain.WorkflowWriteIntentReadOnly, true, domain.WorkflowWriteIntentReadOnly},
		{"normalized from loose casing", "Read_Only", true, domain.WorkflowWriteIntentReadOnly},
		// An intent AO cannot read must NOT silently become "mutating": that
		// would make a mistyped read_only declaration vanish, and the plan
		// would look like it never made one.
		{"unreadable is a plan error", "readonly", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, validation, _ := NormalizeAndValidatePlan(planWithIntent(tc.intent), "Verify current repository state", MaxPlanSteps)
			if validation.Valid != tc.wantValid {
				t.Fatalf("valid = %v (%v), want %v", validation.Valid, validation.Errors, tc.wantValid)
			}
			if !tc.wantValid {
				return
			}
			if got := plan.Steps[0].WriteIntent; got != tc.wantKept {
				t.Fatalf("WriteIntent = %q, want %q", got, tc.wantKept)
			}
		})
	}
}

// Legacy compatibility, stated as the property that actually matters: a plan
// that declares nothing serializes -- and therefore hashes -- exactly as it did
// before the field existed, so no stored plan's identity moves.
func TestUndeclaredWriteIntentDoesNotChangeThePlanHash(t *testing.T) {
	_, _, withField := NormalizeAndValidatePlan(planWithIntent(""), "Verify current repository state", MaxPlanSteps)

	raw, err := json.Marshal(planWithIntent(""))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "writeIntent") {
		t.Fatalf("an undeclared write intent was serialized into the plan JSON: %s", raw)
	}
	declared, _, withIntent := NormalizeAndValidatePlan(planWithIntent(domain.WorkflowWriteIntentReadOnly),
		"Verify current repository state", MaxPlanSteps)
	if declared.Steps[0].WriteIntent != domain.WorkflowWriteIntentReadOnly {
		t.Fatal("the declared intent did not survive normalization")
	}
	if withField == withIntent {
		t.Fatal("declaring a write intent must change the plan hash; it is part of the plan")
	}
}

func TestWriteIntentReachesTheTaskScopeAndSurvivesARoundTrip(t *testing.T) {
	graph := ClassifyTaskGraph(TaskGraphInput{
		WorkflowRunID: "wf-1",
		Objective:     "Verify current repository state",
		Tasks: []TaskScopeInput{{
			TaskID: "wft-1", PlanStepID: "s1", Ordinal: 1,
			Title:              "Verify current repository state",
			Description:        "Run the existing checks. Make no edits.",
			AcceptanceCriteria: []string{"No file is modified."},
			WriteIntent:        domain.WorkflowWriteIntentReadOnly,
		}},
	})
	scope, ok := graph.Scopes["wft-1"]
	if !ok {
		t.Fatal("no scope was classified")
	}
	if scope.WriteIntent != domain.WorkflowWriteIntentReadOnly {
		t.Fatalf("scope WriteIntent = %q, want read_only", scope.WriteIntent)
	}
	raw, err := MarshalTaskScope(scope)
	if err != nil {
		t.Fatalf("MarshalTaskScope: %v", err)
	}
	back, err := UnmarshalTaskScope(raw)
	if err != nil {
		t.Fatalf("UnmarshalTaskScope: %v", err)
	}
	if back.WriteIntent != domain.WorkflowWriteIntentReadOnly {
		t.Fatalf("round-tripped WriteIntent = %q, want read_only", back.WriteIntent)
	}

	// A scope row written before the field existed carries no claim at all.
	legacy, err := UnmarshalTaskScope(`{"version":"v1","source":"estimated","readPaths":[],"writePaths":[]}`)
	if err != nil {
		t.Fatalf("legacy scope: %v", err)
	}
	if legacy.WriteIntent.ReadOnly() {
		t.Fatal("a legacy scope must never resolve to a read-only declaration")
	}
	legacyRaw, err := MarshalTaskScope(legacy)
	if err != nil {
		t.Fatalf("marshal legacy scope: %v", err)
	}
	if strings.Contains(legacyRaw, "writeIntent") {
		t.Fatalf("an undeclared intent was written into a legacy scope: %s", legacyRaw)
	}
}

// The child run's plan artifact is the hop the classifier actually reads, and
// it is the one that has to survive a restart. Assert both the round-trip and
// the legacy default.
func TestPlanArtifactCarriesWriteIntentAndDefaultsConservatively(t *testing.T) {
	artifact := BuildPlanArtifact("p", "Verify current repository state", "v1")
	if artifact.WriteIntent.ReadOnly() {
		t.Fatal("a standalone objective must never default to read-only")
	}
	artifact.WriteIntent = domain.WorkflowWriteIntentReadOnly
	raw, err := MarshalPlanArtifact(artifact)
	if err != nil {
		t.Fatalf("MarshalPlanArtifact: %v", err)
	}
	back, err := UnmarshalPlanArtifact(raw)
	if err != nil {
		t.Fatalf("UnmarshalPlanArtifact: %v", err)
	}
	if !back.WriteIntent.ReadOnly() {
		t.Fatalf("round-tripped WriteIntent = %q, want read_only", back.WriteIntent)
	}
	legacy, err := UnmarshalPlanArtifact(`{"objective":"o","taskPrompt":"p","acceptanceCriteria":[]}`)
	if err != nil {
		t.Fatalf("legacy artifact: %v", err)
	}
	if legacy.WriteIntent.ReadOnly() {
		t.Fatal("a legacy plan artifact must never resolve to a read-only declaration")
	}
}

// The worker prompt: byte-identical for everything that declares nothing, and
// explicitly read-only for what does. Handing a verification task both the
// "implement a concrete code change" instruction and a no-edit criterion is the
// contradiction this replaces.
func TestReadOnlyWorkerPromptReplacesTheImplementInstruction(t *testing.T) {
	base := BuildPlanArtifact("p", "Verify current repository state", "v1")

	mutating := BuildWorkStepPromptWithSpec(base, "")
	explicit := base
	explicit.WriteIntent = domain.WorkflowWriteIntentMutating
	if got := BuildWorkStepPromptWithSpec(explicit, ""); got != mutating {
		t.Fatal("declaring `mutating` changed the worker prompt; it must be byte-identical to the pre-existing one")
	}

	ro := base
	ro.WriteIntent = domain.WorkflowWriteIntentReadOnly
	prompt := BuildWorkStepPromptWithSpec(ro, "")
	if strings.Contains(prompt, "implement the objective above as a concrete, reviewable code") {
		t.Fatalf("a read-only task was still told to implement a code change:\n%s", prompt)
	}
	for _, want := range []string{"READ-ONLY", "Do NOT create, edit or delete any file"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("read-only prompt is missing %q:\n%s", want, prompt)
		}
	}
}
