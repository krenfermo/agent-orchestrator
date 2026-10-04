package workflow_test

import (
	"testing"

	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

func hasReason(reasons []workflowcore.ReviewReason, want workflowcore.ReviewReason) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}

// Scenario 1: exact-content single-file task => review skipped.
func TestReviewPolicyExactContentSingleFileSkipped(t *testing.T) {
	facts := workflowcore.ReviewRiskFacts{
		ChangedFilePaths:                       []string{"config/generated/build-metadata.json"},
		ChangedFileCount:                       1,
		VerifyFileCheckCount:                   1,
		HasExactContentCheckForSoleChangedFile: true,
	}
	decision := workflowcore.EvaluateReviewPolicy(facts)
	if decision.Decision != workflowcore.ReviewSkipped {
		t.Fatalf("decision = %s, want skipped (reasons=%v)", decision.Decision, decision.Reasons)
	}
	if !hasReason(decision.Reasons, workflowcore.ReasonExactContentSingleFile) {
		t.Fatalf("reasons = %v, want %s", decision.Reasons, workflowcore.ReasonExactContentSingleFile)
	}
	if decision.PolicyVersion != workflowcore.ReviewPolicyVersion {
		t.Fatalf("policy version = %s", decision.PolicyVersion)
	}
}

// Scenario 2: docs-only trivial task => skipped if deterministic coverage sufficient.
func TestReviewPolicyDocsOnlySkippedWhenVerified(t *testing.T) {
	facts := workflowcore.ReviewRiskFacts{
		ChangedFilePaths:     []string{"docs/guide.md", "README.md"},
		ChangedFileCount:     2,
		VerifyFileCheckCount: 2,
	}
	decision := workflowcore.EvaluateReviewPolicy(facts)
	if decision.Decision != workflowcore.ReviewSkipped {
		t.Fatalf("decision = %s, want skipped (reasons=%v)", decision.Decision, decision.Reasons)
	}
	if !hasReason(decision.Reasons, workflowcore.ReasonDocsOnlyChange) {
		t.Fatalf("reasons = %v, want %s", decision.Reasons, workflowcore.ReasonDocsOnlyChange)
	}

	// Docs-only WITHOUT any deterministic verify coverage must not be skipped.
	unverified := facts
	unverified.VerifyFileCheckCount = 0
	decision2 := workflowcore.EvaluateReviewPolicy(unverified)
	if decision2.Decision != workflowcore.ReviewRequired {
		t.Fatalf("unverified docs-only decision = %s, want required", decision2.Decision)
	}
	if !hasReason(decision2.Reasons, workflowcore.ReasonInsufficientVerify) {
		t.Fatalf("reasons = %v, want %s", decision2.Reasons, workflowcore.ReasonInsufficientVerify)
	}
}

// Scenario 3: auth path => required.
func TestReviewPolicyAuthPathRequired(t *testing.T) {
	facts := workflowcore.ReviewRiskFacts{
		ChangedFilePaths:   []string{"backend/internal/service/auth/login.go"},
		ChangedFileCount:   1,
		VerifyCommandCount: 1,
	}
	decision := workflowcore.EvaluateReviewPolicy(facts)
	if decision.Decision != workflowcore.ReviewRequired {
		t.Fatalf("decision = %s, want required", decision.Decision)
	}
	if !hasReason(decision.Reasons, workflowcore.ReasonAuthOrSecurityPath) {
		t.Fatalf("reasons = %v, want %s", decision.Reasons, workflowcore.ReasonAuthOrSecurityPath)
	}
}

// Scenario 4: migration => required.
func TestReviewPolicyMigrationPathRequired(t *testing.T) {
	facts := workflowcore.ReviewRiskFacts{
		ChangedFilePaths:   []string{"backend/internal/storage/sqlite/migrations/0102_add_thing.sql"},
		ChangedFileCount:   1,
		VerifyCommandCount: 1,
	}
	decision := workflowcore.EvaluateReviewPolicy(facts)
	if decision.Decision != workflowcore.ReviewRequired || !hasReason(decision.Reasons, workflowcore.ReasonMigrationOrSchemaPath) {
		t.Fatalf("decision = %s reasons=%v, want required/%s", decision.Decision, decision.Reasons, workflowcore.ReasonMigrationOrSchemaPath)
	}
}

// Scenario 5: concurrency-sensitive => required.
func TestReviewPolicyConcurrencyPathRequired(t *testing.T) {
	facts := workflowcore.ReviewRiskFacts{
		ChangedFilePaths:   []string{"backend/internal/session_manager/agent_switching.go"},
		ChangedFileCount:   1,
		VerifyCommandCount: 1,
	}
	decision := workflowcore.EvaluateReviewPolicy(facts)
	if decision.Decision != workflowcore.ReviewRequired || !hasReason(decision.Reasons, workflowcore.ReasonConcurrencyPath) {
		t.Fatalf("decision = %s reasons=%v, want required/%s", decision.Decision, decision.Reasons, workflowcore.ReasonConcurrencyPath)
	}
}

// Scenario 6: public API => required.
func TestReviewPolicyPublicAPIPathRequired(t *testing.T) {
	facts := workflowcore.ReviewRiskFacts{
		ChangedFilePaths:   []string{"backend/internal/httpd/apispec/openapi.yaml"},
		ChangedFileCount:   1,
		VerifyCommandCount: 1,
	}
	decision := workflowcore.EvaluateReviewPolicy(facts)
	if decision.Decision != workflowcore.ReviewRequired || !hasReason(decision.Reasons, workflowcore.ReasonPublicAPIPath) {
		t.Fatalf("decision = %s reasons=%v, want required/%s", decision.Decision, decision.Reasons, workflowcore.ReasonPublicAPIPath)
	}
}

// Scenario 7: large/multi-module => required.
func TestReviewPolicyLargeChangeRequired(t *testing.T) {
	var paths []string
	for i := 0; i < 8; i++ {
		paths = append(paths, "backend/internal/foo/file.go")
	}
	facts := workflowcore.ReviewRiskFacts{ChangedFilePaths: paths, ChangedFileCount: len(paths), VerifyCommandCount: 1}
	decision := workflowcore.EvaluateReviewPolicy(facts)
	if decision.Decision != workflowcore.ReviewRequired || !hasReason(decision.Reasons, workflowcore.ReasonLargeOrMultiModule) {
		t.Fatalf("decision = %s reasons=%v, want required/%s", decision.Decision, decision.Reasons, workflowcore.ReasonLargeOrMultiModule)
	}

	multiModule := workflowcore.ReviewRiskFacts{
		ChangedFilePaths:   []string{"backend/internal/foo/a.go", "frontend/src/renderer/b.tsx"},
		ChangedFileCount:   2,
		VerifyCommandCount: 1,
	}
	decision2 := workflowcore.EvaluateReviewPolicy(multiModule)
	if decision2.Decision != workflowcore.ReviewRequired || !hasReason(decision2.Reasons, workflowcore.ReasonLargeOrMultiModule) {
		t.Fatalf("multi-module decision = %s reasons=%v, want required/%s", decision2.Decision, decision2.Reasons, workflowcore.ReasonLargeOrMultiModule)
	}
}

// Scenario 8: previous fix/retry => required where policy says so.
func TestReviewPolicyPriorProviderAttemptRequired(t *testing.T) {
	facts := workflowcore.ReviewRiskFacts{
		ChangedFilePaths:          []string{"docs/guide.md"},
		ChangedFileCount:          1,
		VerifyFileCheckCount:      1,
		PriorWorkProviderAttempts: 2,
	}
	decision := workflowcore.EvaluateReviewPolicy(facts)
	if decision.Decision != workflowcore.ReviewRequired || !hasReason(decision.Reasons, workflowcore.ReasonPriorProviderAttempts) {
		t.Fatalf("decision = %s reasons=%v, want required/%s", decision.Decision, decision.Reasons, workflowcore.ReasonPriorProviderAttempts)
	}
}

// Scenario 9: insufficient Verify coverage => required.
func TestReviewPolicyInsufficientVerifyRequired(t *testing.T) {
	facts := workflowcore.ReviewRiskFacts{
		ChangedFilePaths: []string{"backend/internal/foo/bar.go"},
		ChangedFileCount: 1,
	}
	decision := workflowcore.EvaluateReviewPolicy(facts)
	if decision.Decision != workflowcore.ReviewRequired || !hasReason(decision.Reasons, workflowcore.ReasonInsufficientVerify) {
		t.Fatalf("decision = %s reasons=%v, want required/%s", decision.Decision, decision.Reasons, workflowcore.ReasonInsufficientVerify)
	}
}

// Ambiguous acceptance criteria => required (facts-level unit coverage,
// complementing the destructive-intent and dependency-config paths tested
// via EvaluateReviewPolicy's textBlob/path matching).
func TestReviewPolicyAmbiguousAcceptanceRequired(t *testing.T) {
	facts := workflowcore.ReviewRiskFacts{
		ChangedFilePaths:        []string{"docs/guide.md"},
		ChangedFileCount:        1,
		VerifyFileCheckCount:    1,
		AcceptanceCriteriaEmpty: true,
	}
	decision := workflowcore.EvaluateReviewPolicy(facts)
	if decision.Decision != workflowcore.ReviewRequired || !hasReason(decision.Reasons, workflowcore.ReasonAmbiguousAcceptance) {
		t.Fatalf("decision = %s reasons=%v, want required/%s", decision.Decision, decision.Reasons, workflowcore.ReasonAmbiguousAcceptance)
	}
}

func TestReviewPolicyDestructiveIntentRequired(t *testing.T) {
	facts := workflowcore.ReviewRiskFacts{
		ChangedFilePaths:   []string{"backend/internal/foo/bar.go"},
		ChangedFileCount:   1,
		VerifyCommandCount: 1,
		ObjectiveText:      "force push the release branch after rewriting history",
	}
	decision := workflowcore.EvaluateReviewPolicy(facts)
	if decision.Decision != workflowcore.ReviewRequired || !hasReason(decision.Reasons, workflowcore.ReasonDestructiveIntent) {
		t.Fatalf("decision = %s reasons=%v, want required/%s", decision.Decision, decision.Reasons, workflowcore.ReasonDestructiveIntent)
	}
}

func TestReviewPolicyDependencyConfigRequired(t *testing.T) {
	facts := workflowcore.ReviewRiskFacts{
		ChangedFilePaths:   []string{"backend/go.mod"},
		ChangedFileCount:   1,
		VerifyCommandCount: 1,
	}
	decision := workflowcore.EvaluateReviewPolicy(facts)
	if decision.Decision != workflowcore.ReviewRequired || !hasReason(decision.Reasons, workflowcore.ReasonDependencyConfigChange) {
		t.Fatalf("decision = %s reasons=%v, want required/%s", decision.Decision, decision.Reasons, workflowcore.ReasonDependencyConfigChange)
	}
}

// A normal, non-sensitive, multi-line code change with verify coverage but
// no exact-content/docs-only shape defaults conservatively to REQUIRED
// (OPTIONAL resolves to REQUIRED in v1).
func TestReviewPolicyDefaultConservativeForOrdinaryCodeChange(t *testing.T) {
	facts := workflowcore.ReviewRiskFacts{
		ChangedFilePaths:   []string{"backend/internal/service/widget/widget.go"},
		ChangedFileCount:   1,
		VerifyCommandCount: 1,
	}
	decision := workflowcore.EvaluateReviewPolicy(facts)
	if decision.Decision != workflowcore.ReviewRequired || !hasReason(decision.Reasons, workflowcore.ReasonDefaultConservative) {
		t.Fatalf("decision = %s reasons=%v, want required/%s", decision.Decision, decision.Reasons, workflowcore.ReasonDefaultConservative)
	}
	if decision.Complexity != workflowcore.ComplexityNormal {
		t.Fatalf("complexity = %s, want normal", decision.Complexity)
	}
}

func TestTaskComplexityClassification(t *testing.T) {
	trivial := workflowcore.EvaluateReviewPolicy(workflowcore.ReviewRiskFacts{
		ChangedFilePaths: []string{"docs/x.md"}, ChangedFileCount: 1, VerifyFileCheckCount: 1,
	})
	if trivial.Complexity != workflowcore.ComplexityTrivial {
		t.Fatalf("docs-only complexity = %s, want trivial", trivial.Complexity)
	}
	highRisk := workflowcore.EvaluateReviewPolicy(workflowcore.ReviewRiskFacts{
		ChangedFilePaths: []string{"backend/internal/service/auth/login.go"}, ChangedFileCount: 1, VerifyCommandCount: 1,
	})
	if highRisk.Complexity != workflowcore.ComplexityHighRisk {
		t.Fatalf("auth-path complexity = %s, want high_risk", highRisk.Complexity)
	}
}

// --- VerifyScopePolicy -------------------------------------------------

// Scenario 15/16: targeted and module verify selection.
func TestVerifyScopeTargetedSinglePackage(t *testing.T) {
	decision := workflowcore.ComputeVerifyScope(nil, false, []string{"backend/internal/foo/bar.go", "backend/internal/foo/bar_test.go"})
	if decision.Scope != workflowcore.VerifyScopeTargeted || decision.PackageDir != "backend/internal/foo" {
		t.Fatalf("decision = %+v, want targeted/backend/internal/foo", decision)
	}
}

func TestVerifyScopeModuleSharedAncestor(t *testing.T) {
	decision := workflowcore.ComputeVerifyScope(nil, false, []string{
		"backend/internal/foo/bar.go",
		"backend/internal/foo/sub/baz.go",
	})
	if decision.Scope != workflowcore.VerifyScopeModule || decision.PackageDir != "backend/internal/foo" {
		t.Fatalf("decision = %+v, want module/backend/internal/foo", decision)
	}
}

// Scenario 17: repo-wide sensitive task forces repository scope even though
// the changed-file set alone would otherwise look narrowly targetable.
func TestVerifyScopeRepositoryForcedBySensitiveReview(t *testing.T) {
	decision := workflowcore.ComputeVerifyScope(
		[]workflowcore.ReviewReason{workflowcore.ReasonAuthOrSecurityPath},
		false,
		[]string{"backend/internal/foo/bar.go"},
	)
	if decision.Scope != workflowcore.VerifyScopeRepository {
		t.Fatalf("decision = %+v, want repository", decision)
	}
	if !hasVerifyScopeReason(decision.Reasons, workflowcore.VerifyScopeReasonSensitiveRequiredReview) {
		t.Fatalf("reasons = %v", decision.Reasons)
	}
}

// Scenario 18: final integration gate forces repository scope regardless of
// how narrow the change looks.
func TestVerifyScopeFinalIntegrationTaskForcesRepository(t *testing.T) {
	decision := workflowcore.ComputeVerifyScope(nil, true, []string{"backend/internal/foo/bar.go"})
	if decision.Scope != workflowcore.VerifyScopeRepository {
		t.Fatalf("decision = %+v, want repository", decision)
	}
	if !hasVerifyScopeReason(decision.Reasons, workflowcore.VerifyScopeReasonFinalIntegrationTask) {
		t.Fatalf("reasons = %v", decision.Reasons)
	}
}

func TestVerifyScopeNoGoChangesStaysRepository(t *testing.T) {
	decision := workflowcore.ComputeVerifyScope(nil, false, []string{"docs/guide.md"})
	if decision.Scope != workflowcore.VerifyScopeRepository {
		t.Fatalf("decision = %+v, want repository", decision)
	}
}

// NarrowVerificationPlan must only rewrite the exact recognizable
// `go build|vet|test ./...` shape from the repo root, leaving every other
// command (already-scoped, non-Go, or repository-scope decisions) byte-
// identical.
func TestNarrowVerificationPlanOnlyRewritesRecognizableGoWildcard(t *testing.T) {
	plan := workflowcore.VerificationPlan{Commands: []workflowcore.VerificationCommandCheck{
		{Command: "go", Args: []string{"test", "./..."}, RequiredExitCode: 0},
		{Command: "go", Args: []string{"vet", "./..."}, RequiredExitCode: 0},
		{Command: "go", Args: []string{"build", "./internal/already-scoped/..."}, RequiredExitCode: 0},
		{Command: "npm", Args: []string{"run", "typecheck"}, RequiredExitCode: 0},
		{Command: "go", Args: []string{"test", "./..."}, WorkingDirectory: "backend", RequiredExitCode: 0},
	}}
	decision := workflowcore.VerifyScopeDecision{Scope: workflowcore.VerifyScopeTargeted, PackageDir: "internal/foo"}
	narrowed, applied := workflowcore.NarrowVerificationPlan(plan, decision)
	if narrowed.Commands[0].Args[1] != "./internal/foo/..." {
		t.Fatalf("command 0 = %v, want narrowed", narrowed.Commands[0].Args)
	}
	if narrowed.Commands[1].Args[1] != "./internal/foo/..." {
		t.Fatalf("command 1 = %v, want narrowed", narrowed.Commands[1].Args)
	}
	if narrowed.Commands[2].Args[1] != "./internal/already-scoped/..." {
		t.Fatalf("command 2 changed, want untouched: %v", narrowed.Commands[2].Args)
	}
	if narrowed.Commands[3].Command != "npm" {
		t.Fatalf("command 3 changed, want untouched: %+v", narrowed.Commands[3])
	}
	if narrowed.Commands[4].WorkingDirectory != "backend" || narrowed.Commands[4].Args[1] != "./..." {
		t.Fatalf("command 4 changed, want untouched (already scoped to a subdirectory): %+v", narrowed.Commands[4])
	}
	if len(applied) != 2 {
		t.Fatalf("applied = %v, want exactly 2 transforms recorded", applied)
	}
}

func TestNarrowVerificationPlanNoOpForRepositoryScope(t *testing.T) {
	plan := workflowcore.VerificationPlan{Commands: []workflowcore.VerificationCommandCheck{
		{Command: "go", Args: []string{"test", "./..."}, RequiredExitCode: 0},
	}}
	decision := workflowcore.VerifyScopeDecision{Scope: workflowcore.VerifyScopeRepository}
	narrowed, applied := workflowcore.NarrowVerificationPlan(plan, decision)
	if narrowed.Commands[0].Args[1] != "./..." {
		t.Fatalf("command changed under repository scope: %v", narrowed.Commands[0].Args)
	}
	if len(applied) != 0 {
		t.Fatalf("applied = %v, want none under repository scope", applied)
	}
}

func hasVerifyScopeReason(reasons []workflowcore.VerifyScopeReason, want workflowcore.VerifyScopeReason) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}
