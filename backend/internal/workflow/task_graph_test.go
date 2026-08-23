package workflow_test

import (
	"encoding/json"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

func scopeInput(id string, ordinal int64, title, description string, criteria []string, deps []string) workflowcore.TaskScopeInput {
	return workflowcore.TaskScopeInput{
		TaskID: id, PlanStepID: id, Ordinal: ordinal, Title: title,
		Description: description, AcceptanceCriteria: criteria, Dependencies: deps,
	}
}

func relationFor(t *testing.T, rels []domain.WorkflowTaskRelationship, a, b string) domain.WorkflowTaskRelationship {
	t.Helper()
	if a > b {
		a, b = b, a
	}
	for _, rel := range rels {
		if rel.TaskID == a && rel.RelatedTaskID == b {
			return rel
		}
	}
	t.Fatalf("no relationship stored for pair (%s,%s) in %+v", a, b, rels)
	return domain.WorkflowTaskRelationship{}
}

// A pair the DAG orders is a functional dependency even when both tasks write
// the same file: the ordering already removed the collision, and reporting it
// again as a conflict would serialize something already serialized.
func TestClassifyTaskGraphPrefersDependencyOverOverlappingWrites(t *testing.T) {
	graph := workflowcore.ClassifyTaskGraph(workflowcore.TaskGraphInput{
		WorkflowRunID: "wf-1",
		Objective:     "Extend the durable task model",
		Tasks: []workflowcore.TaskScopeInput{
			scopeInput("wft-a", 1, "Add the scope column",
				"Add scope_json to backend/internal/domain/workflow_plan.go.",
				[]string{"backend/internal/domain/workflow_plan.go declares the new field."}, nil),
			scopeInput("wft-b", 2, "Wire the scope column",
				"Update backend/internal/domain/workflow_plan.go so readers see the field.",
				[]string{"backend/internal/domain/workflow_plan.go compiles."}, []string{"wft-a"}),
		},
	})
	rel := relationFor(t, graph.Relationships, "wft-a", "wft-b")
	if rel.Relation != domain.WorkflowTaskRelationDependency {
		t.Fatalf("relation=%q reason=%q, want functional_dependency", rel.Relation, rel.Reason)
	}
	if rel.Reason != string(workflowcore.TaskRelationReasonDirectDependency) {
		t.Fatalf("reason=%q, want %q", rel.Reason, workflowcore.TaskRelationReasonDirectDependency)
	}
	if len(rel.Overlap) != 0 {
		t.Fatalf("dependency pair carried an overlap: %+v", rel.Overlap)
	}
	if got := graph.Scopes["wft-b"].ExecutionStrategy; got != domain.WorkflowTaskExecutionSequential {
		t.Fatalf("wft-b strategy=%q, want sequential", got)
	}
	if got := graph.Scopes["wft-b"].IntegrationDependencies; len(got) != 1 || got[0] != "wft-a" {
		t.Fatalf("wft-b integration deps=%+v, want [wft-a]", got)
	}
}

// A dependency that runs through an intermediate task still orders the pair.
func TestClassifyTaskGraphRecognizesTransitiveDependency(t *testing.T) {
	graph := workflowcore.ClassifyTaskGraph(workflowcore.TaskGraphInput{
		WorkflowRunID: "wf-1",
		Tasks: []workflowcore.TaskScopeInput{
			scopeInput("wft-a", 1, "Schema", "Add backend/internal/storage/sqlite/migrations/0127_x.sql.", []string{"Migration exists."}, nil),
			scopeInput("wft-b", 2, "Store", "Add backend/internal/storage/sqlite/store/x_store.go.", []string{"Store compiles."}, []string{"wft-a"}),
			scopeInput("wft-c", 3, "Service", "Add backend/internal/service/x/x.go.", []string{"Service compiles."}, []string{"wft-b"}),
		},
	})
	rel := relationFor(t, graph.Relationships, "wft-a", "wft-c")
	if rel.Relation != domain.WorkflowTaskRelationDependency || rel.Reason != string(workflowcore.TaskRelationReasonTransitiveDependency) {
		t.Fatalf("relation=%q reason=%q, want a transitive functional_dependency", rel.Relation, rel.Reason)
	}
}

// Two unordered tasks that both write the same file conflict, and the decision
// names the file that made it one.
func TestClassifyTaskGraphFlagsSharedFileWriteAsProbableConflict(t *testing.T) {
	graph := workflowcore.ClassifyTaskGraph(workflowcore.TaskGraphInput{
		WorkflowRunID: "wf-1",
		Tasks: []workflowcore.TaskScopeInput{
			scopeInput("wft-a", 1, "Add a field",
				"Add a field to backend/internal/domain/workflow_plan.go.",
				[]string{"The field is persisted."}, nil),
			scopeInput("wft-b", 2, "Add a constant",
				"Add a constant to backend/internal/domain/workflow_plan.go.",
				[]string{"The constant is exported."}, nil),
		},
	})
	rel := relationFor(t, graph.Relationships, "wft-a", "wft-b")
	if rel.Relation != domain.WorkflowTaskRelationWriteConflict {
		t.Fatalf("relation=%q, want probable_write_conflict", rel.Relation)
	}
	if rel.Reason != string(workflowcore.TaskRelationReasonSharedFileWrite) {
		t.Fatalf("reason=%q, want %q", rel.Reason, workflowcore.TaskRelationReasonSharedFileWrite)
	}
	if len(rel.Overlap) != 1 || rel.Overlap[0] != "backend/internal/domain/workflow_plan.go" {
		t.Fatalf("overlap=%+v, want the shared file", rel.Overlap)
	}
	for _, id := range []string{"wft-a", "wft-b"} {
		if got := graph.Scopes[id].ExecutionStrategy; got != domain.WorkflowTaskExecutionSerialized {
			t.Fatalf("%s strategy=%q, want serialized", id, got)
		}
	}
	// Only the earlier task is an integration dependency of the later one --
	// taking both directions would be a cycle.
	if got := graph.Scopes["wft-b"].IntegrationDependencies; len(got) != 1 || got[0] != "wft-a" {
		t.Fatalf("wft-b integration deps=%+v, want [wft-a]", got)
	}
	if got := graph.Scopes["wft-a"].IntegrationDependencies; len(got) != 0 {
		t.Fatalf("wft-a integration deps=%+v, want none", got)
	}
}

// Different files in the same package are the ordinary case in any codebase.
// Calling that a conflict would serialize a whole plan for nothing.
func TestClassifyTaskGraphTreatsDifferentFilesInOnePackageAsIndependent(t *testing.T) {
	graph := workflowcore.ClassifyTaskGraph(workflowcore.TaskGraphInput{
		WorkflowRunID: "wf-1",
		Tasks: []workflowcore.TaskScopeInput{
			scopeInput("wft-a", 1, "Classifier", "Add backend/internal/workflow/task_graph.go.", []string{"Classifier is pure."}, nil),
			scopeInput("wft-b", 2, "Board", "Update backend/internal/workflow/board.go.", []string{"Board renders."}, nil),
		},
	})
	rel := relationFor(t, graph.Relationships, "wft-a", "wft-b")
	if rel.Relation != domain.WorkflowTaskRelationIndependent || rel.Reason != string(workflowcore.TaskRelationReasonDisjointWriteSets) {
		t.Fatalf("relation=%q reason=%q, want independent work", rel.Relation, rel.Reason)
	}
	for _, id := range []string{"wft-a", "wft-b"} {
		if got := graph.Scopes[id].ExecutionStrategy; got != domain.WorkflowTaskExecutionParallel {
			t.Fatalf("%s strategy=%q, want parallel", id, got)
		}
	}
}

// A task whose write scope is only a directory (the file is not knowable yet)
// conflicts with a task writing a specific file inside it.
func TestClassifyTaskGraphFlagsDirectoryContainingAnotherTasksFile(t *testing.T) {
	graph := workflowcore.ClassifyTaskGraph(workflowcore.TaskGraphInput{
		WorkflowRunID: "wf-1",
		Tasks: []workflowcore.TaskScopeInput{
			{TaskID: "wft-a", Ordinal: 1, Title: "Rework the package",
				Description:      "Refactor the whole package.",
				DeclaredPackages: []string{"backend/internal/workflow"}},
			{TaskID: "wft-b", Ordinal: 2, Title: "Touch one file",
				Description:   "Update one file.",
				DeclaredFiles: []string{"backend/internal/workflow/board.go"}},
		},
	})
	rel := relationFor(t, graph.Relationships, "wft-a", "wft-b")
	if rel.Relation != domain.WorkflowTaskRelationWriteConflict || rel.Reason != string(workflowcore.TaskRelationReasonSharedPackageWrite) {
		t.Fatalf("relation=%q reason=%q, want a shared-package write conflict", rel.Relation, rel.Reason)
	}
	if len(rel.Overlap) != 1 || rel.Overlap[0] != "backend/internal/workflow/board.go" {
		t.Fatalf("overlap=%+v, want the contained file", rel.Overlap)
	}
}

// A path named without any write verb beside it is read scope, not write
// scope, so it can never manufacture a conflict.
func TestEstimatedScopeSeparatesReadsFromWrites(t *testing.T) {
	graph := workflowcore.ClassifyTaskGraph(workflowcore.TaskGraphInput{
		WorkflowRunID: "wf-1",
		RepoRoots:     []string{"backend", "docs"},
		Tasks: []workflowcore.TaskScopeInput{
			{
				TaskID: "wft-a", Ordinal: 1,
				Title:       "Add a store method",
				Description: "Follow the conventions in docs/architecture.md. Add the method to backend/internal/storage/sqlite/store/workflow_plan_store.go.",
				AcceptanceCriteria: []string{
					"backend/internal/storage/sqlite/store/workflow_plan_store.go gains UpdateWorkflowTaskScope().",
				},
				Verify: workflowcore.VerificationPlan{
					Commands: []workflowcore.VerificationCommandCheck{{Command: "go", Args: []string{"test", "./internal/storage/..."}, WorkingDirectory: "backend", RequiredExitCode: 0}},
					Files:    []workflowcore.VerificationFileCheck{{Path: "internal/storage/sqlite/migrations/0127_x.sql", Exists: true}},
				},
			},
		},
	})
	scope := graph.Scopes["wft-a"]
	if !hasPath(scope.WritePaths, "backend/internal/storage/sqlite/store/workflow_plan_store.go") {
		t.Fatalf("write paths=%+v, want the file named beside \"Add\"", scope.WritePaths)
	}
	if !hasPath(scope.ReadPaths, "docs/architecture.md") {
		t.Fatalf("read paths=%+v, want the merely-referenced document", scope.ReadPaths)
	}
	if hasPath(scope.WritePaths, "docs/architecture.md") {
		t.Fatalf("a referenced document became a write: %+v", scope.WritePaths)
	}
	// A file check is resolved in the spec's own namespace, not read literally:
	// workingDirectory "backend" plus "internal/..." is backend/internal/...
	if !hasPath(scope.WritePaths, "backend/internal/storage/sqlite/migrations/0127_x.sql") {
		t.Fatalf("write paths=%+v, want the verify file check resolved into backend/", scope.WritePaths)
	}
	if !hasPath(scope.Components, "backend") || !hasPath(scope.Components, "docs") {
		t.Fatalf("components=%+v, want backend and docs", scope.Components)
	}
	if !hasPath(scope.Packages, "backend/internal/storage/sqlite/store") {
		t.Fatalf("packages=%+v, want the store package", scope.Packages)
	}
	if !hasPath(scope.Symbols, "UpdateWorkflowTaskScope") {
		t.Fatalf("symbols=%+v, want the named symbol", scope.Symbols)
	}
	if scope.Source != domain.WorkflowTaskScopeEstimated {
		t.Fatalf("source=%q, want estimated", scope.Source)
	}
	if scope.Version != workflowcore.TaskGraphPolicyVersion {
		t.Fatalf("version=%q, want %q", scope.Version, workflowcore.TaskGraphPolicyVersion)
	}
}

// Observed output outranks every estimate: a task that has run is classified
// against what it actually wrote.
func TestObservedWriteSetOverridesTheEstimate(t *testing.T) {
	tasks := []workflowcore.TaskScopeInput{
		scopeInput("wft-a", 1, "Add the classifier", "Add backend/internal/workflow/task_graph.go.", []string{"It is pure."}, nil),
		scopeInput("wft-b", 2, "Add the board", "Add backend/internal/workflow/board.go.", []string{"It renders."}, nil),
	}
	before := workflowcore.ClassifyTaskGraph(workflowcore.TaskGraphInput{WorkflowRunID: "wf-1", Tasks: tasks})
	if rel := relationFor(t, before.Relationships, "wft-a", "wft-b"); rel.Relation != domain.WorkflowTaskRelationIndependent {
		t.Fatalf("estimated relation=%q, want independent", rel.Relation)
	}

	// wft-a turns out to have edited board.go too.
	tasks[0].ObservedWritePaths = []string{"backend/internal/workflow/task_graph.go", "backend/internal/workflow/board.go"}
	after := workflowcore.ClassifyTaskGraph(workflowcore.TaskGraphInput{WorkflowRunID: "wf-1", Tasks: tasks})
	rel := relationFor(t, after.Relationships, "wft-a", "wft-b")
	if rel.Relation != domain.WorkflowTaskRelationWriteConflict {
		t.Fatalf("relation=%q, want probable_write_conflict once the writes are observed", rel.Relation)
	}
	if got := after.Scopes["wft-a"].Source; got != domain.WorkflowTaskScopeObserved {
		t.Fatalf("source=%q, want observed", got)
	}
	if got := after.Scopes["wft-a"].ObservedWritePaths; len(got) != 2 {
		t.Fatalf("observed=%+v, want both paths", got)
	}
}

// The classifier is a pure function: the same plan must always produce the same
// graph, byte for byte, or the persisted decision is not reproducible.
func TestClassifyTaskGraphIsDeterministic(t *testing.T) {
	in := workflowcore.TaskGraphInput{
		WorkflowRunID: "wf-1",
		Objective:     "Extend the model",
		Tasks: []workflowcore.TaskScopeInput{
			scopeInput("wft-c", 3, "Third", "Update backend/internal/workflow/board.go.", []string{"ok"}, []string{"wft-a"}),
			scopeInput("wft-a", 1, "First", "Add backend/internal/workflow/task_graph.go.", []string{"ok"}, nil),
			scopeInput("wft-b", 2, "Second", "Add backend/internal/workflow/task_graph.go tests.", []string{"ok"}, nil),
		},
	}
	first := workflowcore.ClassifyTaskGraph(in)
	second := workflowcore.ClassifyTaskGraph(in)
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Fatalf("classification is not deterministic:\n%s\n%s", a, b)
	}
	if len(first.Relationships) != 3 {
		t.Fatalf("relationships=%d, want one per unordered pair", len(first.Relationships))
	}
	for _, rel := range first.Relationships {
		if rel.TaskID >= rel.RelatedTaskID {
			t.Fatalf("pair %+v is not canonically ordered", rel)
		}
	}
}

// A single-segment directory must never enter a write set: admitting "backend"
// would make every task in the repository conflict with every other.
func TestClassifyTaskGraphIgnoresRepositoryRootAsWriteScope(t *testing.T) {
	graph := workflowcore.ClassifyTaskGraph(workflowcore.TaskGraphInput{
		WorkflowRunID: "wf-1",
		RepoRoots:     []string{"backend"},
		Tasks: []workflowcore.TaskScopeInput{
			scopeInput("wft-a", 1, "One", "Update everything in backend and read/write docs.", []string{"ok"}, nil),
			scopeInput("wft-b", 2, "Two", "Update everything in backend too.", []string{"ok"}, nil),
		},
	})
	for id, scope := range graph.Scopes {
		if hasPath(scope.WritePaths, "backend") || hasPath(scope.WritePaths, "read/write") {
			t.Fatalf("%s write paths=%+v, want no repository root and no English phrase", id, scope.WritePaths)
		}
	}
	if rel := relationFor(t, graph.Relationships, "wft-a", "wft-b"); rel.Relation != domain.WorkflowTaskRelationIndependent {
		t.Fatalf("relation=%q, want independent", rel.Relation)
	}
}

func TestMarshalTaskScopeRoundTripsAndNeverEmitsNullSlices(t *testing.T) {
	raw, err := workflowcore.MarshalTaskScope(domain.WorkflowTaskScope{})
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"readPaths", "writePaths", "packages", "components", "files", "symbols", "integrationDependencies", "observedWritePaths"} {
		if probe[key] == nil {
			t.Fatalf("%s serialized as null in %s", key, raw)
		}
	}
	if probe["version"] != workflowcore.TaskGraphPolicyVersion {
		t.Fatalf("version=%v, want %q", probe["version"], workflowcore.TaskGraphPolicyVersion)
	}
	back, err := workflowcore.UnmarshalTaskScope(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.Version != workflowcore.TaskGraphPolicyVersion || back.Source != domain.WorkflowTaskScopeEstimated {
		t.Fatalf("round trip lost fields: %+v", back)
	}
	// A task planned before the scope model existed must read back clean.
	empty, err := workflowcore.UnmarshalTaskScope("")
	if err != nil || empty.Version != "" {
		t.Fatalf("empty scope_json: %+v err=%v", empty, err)
	}
}

func hasPath(in []string, want string) bool {
	for _, s := range in {
		if s == want {
			return true
		}
	}
	return false
}
