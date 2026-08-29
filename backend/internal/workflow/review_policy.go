package workflow

import (
	"strings"
	"time"
)

// ReviewPolicyVersion is the fixed Checkpoint 8I policy version. A future
// checkpoint may introduce v2 with different thresholds; the version is
// always persisted alongside the decision so historical decisions remain
// explainable against the rules that actually produced them ("no
// recalcules historia con reglas futuras").
const ReviewPolicyVersion = "v1"

// largeChangeFileThreshold is the changed-file-count above which a task is
// treated as large/multi-file regardless of which paths changed. Chosen
// conservatively (favor REQUIRED) per the checkpoint brief: "queremos ahorro
// seguro, no agresivo."
const largeChangeFileThreshold = 6

// ReviewDecision is the deterministic outcome of evaluating a
// ReviewRiskFacts snapshot against the current ReviewPolicy.
type ReviewDecision string

// The three review outcomes. Optional and skipped are distinct: optional means
// a review may run and its verdict counts, skipped means none was warranted at
// all, and only the second lets a run proceed with no verdict on record.
const (
	ReviewRequired ReviewDecision = "required"
	ReviewOptional ReviewDecision = "optional"
	ReviewSkipped  ReviewDecision = "skipped"
)

// ReviewReason is a stable, machine-checkable code explaining why a decision
// was reached. Never free text: reasons must remain comparable across runs
// and testable.
type ReviewReason string

// The closed reason vocabulary. Each names the specific fact in the snapshot
// that forced the decision, so a decision taken today stays explainable against
// the rules that produced it.
const (
	ReasonAuthOrSecurityPath     ReviewReason = "auth_or_security_path"
	ReasonMigrationOrSchemaPath  ReviewReason = "migration_or_schema_path"
	ReasonConcurrencyPath        ReviewReason = "concurrency_sensitive_path"
	ReasonInfraOrCICDPath        ReviewReason = "infrastructure_or_cicd_path"
	ReasonPublicAPIPath          ReviewReason = "public_api_or_contract_path"
	ReasonDestructiveIntent      ReviewReason = "destructive_operation_intent"
	ReasonDependencyConfigChange ReviewReason = "dependency_or_security_config_change"
	ReasonLargeOrMultiModule     ReviewReason = "large_or_multi_module_change"
	ReasonPriorProviderAttempts  ReviewReason = "prior_work_provider_attempts"
	ReasonAmbiguousAcceptance    ReviewReason = "ambiguous_acceptance_criteria"
	ReasonInsufficientVerify     ReviewReason = "verify_coverage_insufficient"
	ReasonDocsOnlyChange         ReviewReason = "docs_only_change_fully_verified"
	ReasonExactContentSingleFile ReviewReason = "exact_content_single_file_change"
	ReasonDefaultConservative    ReviewReason = "default_conservative_required"
	ReasonNoChangedFiles         ReviewReason = "no_changed_files_observed"
)

// sensitive* pattern tables are the single centralized source of path-based
// risk signals. Every call site that needs "is this path sensitive" must
// consult these tables, never re-implement its own strings.Contains checks
// (checkpoint brief §6: "no pongas if strings.Contains(path, \"auth\") en
// cinco archivos diferentes").
var (
	authOrSecurityPathPatterns = []string{
		"auth", "session", "login", "logout", "credential", "secret",
		"token", "permission", "acl", "oauth", "jwt", "password",
	}
	migrationOrSchemaPathPatterns = []string{
		"migrations/", "/migrations/", "schema.sql", ".sql", "goose",
	}
	concurrencyPathPatterns = []string{
		"mutex", "lock", "concurrent", "atomic", "goroutine",
		"agent_switching", "session_manager/manager",
	}
	infraOrCICDPathPatterns = []string{
		".github/workflows/", "dockerfile", "docker-compose", "terraform",
		"/deploy/", "k8s/", "helm/",
	}
	publicAPIPathPatterns = []string{
		"openapi", "apispec", "schema.ts", ".proto", "/api/v1/", "api/v1/",
	}
	dependencyConfigPaths = []string{
		"go.mod", "go.sum", "package.json", "package-lock.json",
		"pnpm-lock.yaml", "yarn.lock", "requirements.txt", "gemfile.lock",
	}
	docsOnlyExtensions        = []string{".md", ".txt", ".rst"}
	destructiveIntentKeywords = []string{
		"drop table", "rm -rf", "force push", "force-push", "delete all",
		"destroy ", "truncate table", "delete database",
	}
)

// ReviewRiskFacts is every fact ReviewPolicy is allowed to consult. All
// fields must be derivable from data already durable in the workflow engine
// at the moment cycle 1's review decision is made — no new LLM call, no
// invented signal (checkpoint brief §4).
type ReviewRiskFacts struct {
	ChangedFilePaths []string `json:"changedFilePaths"`
	ChangedFileCount int      `json:"changedFileCount"`

	// ObjectiveText and AcceptanceCriteria feed keyword matching only
	// (destructive-intent phrasing); they are not stored back onto the
	// decision (kept out of the persisted facts JSON to avoid duplicating
	// the run's own objective text across every checkpoint row).
	ObjectiveText           string   `json:"-"`
	AcceptanceCriteria      []string `json:"-"`
	AcceptanceCriteriaEmpty bool     `json:"acceptanceCriteriaEmpty"`

	// PriorWorkProviderAttempts is the number of attempt rows already
	// recorded on the work step. A first, successful dispatch always has
	// exactly 1; more than 1 means a retry/failover happened before this
	// code ever reached review (dispatch.go: one attempt row per harness
	// tried).
	PriorWorkProviderAttempts int `json:"priorWorkProviderAttempts"`

	VerifyCommandCount   int `json:"verifyCommandCount"`
	VerifyFileCheckCount int `json:"verifyFileCheckCount"`

	// HasExactContentCheckForSoleChangedFile is true only when exactly one
	// file changed AND the plan's VerificationPlan contains a file check for
	// that exact path asserting either exact content or a sha256 digest.
	// Computed by the caller (it needs the VerificationPlan, which
	// ReviewRiskFacts deliberately does not embed, keeping this struct a
	// flat, JSON-serializable fact snapshot).
	HasExactContentCheckForSoleChangedFile bool `json:"hasExactContentCheckForSoleChangedFile"`
}

// ReviewPolicyDecision is the durable, explainable result of evaluating
// ReviewPolicy once for a review step's cycle 1. Persisted verbatim
// (policy_version + reasons + facts + timestamp), never reduced to a bare
// boolean (checkpoint brief §3/§7).
type ReviewPolicyDecision struct {
	PolicyVersion string          `json:"policyVersion"`
	Decision      ReviewDecision  `json:"decision"`
	Reasons       []ReviewReason  `json:"reasons"`
	Facts         ReviewRiskFacts `json:"facts"`
	Complexity    TaskComplexity  `json:"complexity"`
	EvaluatedAt   time.Time       `json:"evaluatedAt"`
}

// TaskComplexity is Checkpoint 8I's minimal, explainable classifier — no ML,
// no additional LLM call, derived from the exact same facts ReviewPolicy
// already computed. It is informational (surfaced in the persisted decision
// and to the frontend) and does not itself gate anything beyond what the
// REQUIRED-trigger checks already gate.
type TaskComplexity string

// The three complexity bands, cheapest first.
const (
	ComplexityTrivial  TaskComplexity = "trivial"
	ComplexityNormal   TaskComplexity = "normal"
	ComplexityHighRisk TaskComplexity = "high_risk"
)

// classifyTaskComplexity derives a TaskComplexity label from facts alone.
// Any REQUIRED-trigger reason implies high_risk; a docs-only or
// exact-content single-file change implies trivial; everything else is
// normal.
func classifyTaskComplexity(facts ReviewRiskFacts, requiredReasons []ReviewReason) TaskComplexity {
	if len(requiredReasons) > 0 {
		return ComplexityHighRisk
	}
	if facts.ChangedFileCount == 0 {
		return ComplexityNormal
	}
	if allPathsHaveAnySuffix(facts.ChangedFilePaths, docsOnlyExtensions) || facts.HasExactContentCheckForSoleChangedFile {
		return ComplexityTrivial
	}
	return ComplexityNormal
}

func matchesAny(haystack string, patterns []string) bool {
	for _, p := range patterns {
		if strings.Contains(haystack, p) {
			return true
		}
	}
	return false
}

func allPathsHaveAnySuffix(paths, suffixes []string) bool {
	if len(paths) == 0 {
		return false
	}
	for _, p := range paths {
		lower := strings.ToLower(p)
		matched := false
		for _, suf := range suffixes {
			if strings.HasSuffix(lower, suf) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func dedupeReasons(reasons []ReviewReason) []ReviewReason {
	seen := map[ReviewReason]bool{}
	out := make([]ReviewReason, 0, len(reasons))
	for _, r := range reasons {
		if seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}

// EvaluateReviewPolicy is the pure, deterministic ReviewPolicy v1 decision
// function (checkpoint brief §2: "ReviewPolicy debe ser DETERMINISTA. NO
// gastes otro LLM"). Given the same facts it always returns the same
// decision — no IO, no clock read (EvaluatedAt is stamped by the caller).
//
// Order of evaluation, per the brief's conservative-by-default instruction:
//  1. Any hard REQUIRED trigger (sensitive path, destructive intent, large/
//     multi-module change, prior provider retry, ambiguous acceptance
//     criteria) wins outright, regardless of how verifiable the change is.
//  2. Absent any hard trigger, a task with zero deterministic Verify checks
//     can never be SKIPPED — there would be nothing to prove the change
//     safe by (§9 of the brief: "Verify incapaz de demostrar suficientemente
//     el resultado" is itself a REQUIRED trigger).
//  3. Docs-only changes (every changed path ends in a docs extension) that
//     do have deterministic Verify coverage are SKIPPED.
//  4. A single changed file with an exact-content/sha256 Verify file check
//     covering that exact path is SKIPPED.
//  5. Everything else defaults to REQUIRED (checkpoint brief §5: "Para 8I
//     puedes resolver OPTIONAL conservadoramente como REQUIRED por
//     default").
func EvaluateReviewPolicy(facts ReviewRiskFacts) ReviewPolicyDecision {
	var required []ReviewReason
	lowerPaths := make([]string, len(facts.ChangedFilePaths))
	for i, p := range facts.ChangedFilePaths {
		lowerPaths[i] = strings.ToLower(p)
	}
	textBlob := strings.ToLower(facts.ObjectiveText + " " + strings.Join(facts.AcceptanceCriteria, " "))

	for _, p := range lowerPaths {
		if matchesAny(p, authOrSecurityPathPatterns) {
			required = append(required, ReasonAuthOrSecurityPath)
		}
		if matchesAny(p, migrationOrSchemaPathPatterns) {
			required = append(required, ReasonMigrationOrSchemaPath)
		}
		if matchesAny(p, concurrencyPathPatterns) {
			required = append(required, ReasonConcurrencyPath)
		}
		if matchesAny(p, infraOrCICDPathPatterns) {
			required = append(required, ReasonInfraOrCICDPath)
		}
		if matchesAny(p, publicAPIPathPatterns) {
			required = append(required, ReasonPublicAPIPath)
		}
		if matchesAny(p, dependencyConfigPaths) {
			required = append(required, ReasonDependencyConfigChange)
		}
	}
	if matchesAny(textBlob, destructiveIntentKeywords) {
		required = append(required, ReasonDestructiveIntent)
	}
	if facts.ChangedFileCount > largeChangeFileThreshold || distinctTopLevelDirs(facts.ChangedFilePaths) > 1 {
		required = append(required, ReasonLargeOrMultiModule)
	}
	if facts.PriorWorkProviderAttempts > 1 {
		required = append(required, ReasonPriorProviderAttempts)
	}
	if facts.AcceptanceCriteriaEmpty {
		required = append(required, ReasonAmbiguousAcceptance)
	}

	if len(required) > 0 {
		required = dedupeReasons(required)
		return ReviewPolicyDecision{
			PolicyVersion: ReviewPolicyVersion,
			Decision:      ReviewRequired,
			Reasons:       required,
			Facts:         facts,
			Complexity:    classifyTaskComplexity(facts, required),
		}
	}

	hasVerifyCoverage := facts.VerifyCommandCount > 0 || facts.VerifyFileCheckCount > 0
	if !hasVerifyCoverage {
		return ReviewPolicyDecision{
			PolicyVersion: ReviewPolicyVersion,
			Decision:      ReviewRequired,
			Reasons:       []ReviewReason{ReasonInsufficientVerify},
			Facts:         facts,
			Complexity:    classifyTaskComplexity(facts, nil),
		}
	}

	if facts.ChangedFileCount > 0 && allPathsHaveAnySuffix(facts.ChangedFilePaths, docsOnlyExtensions) {
		return ReviewPolicyDecision{
			PolicyVersion: ReviewPolicyVersion,
			Decision:      ReviewSkipped,
			Reasons:       []ReviewReason{ReasonDocsOnlyChange},
			Facts:         facts,
			Complexity:    classifyTaskComplexity(facts, nil),
		}
	}

	if facts.ChangedFileCount == 1 && facts.HasExactContentCheckForSoleChangedFile {
		return ReviewPolicyDecision{
			PolicyVersion: ReviewPolicyVersion,
			Decision:      ReviewSkipped,
			Reasons:       []ReviewReason{ReasonExactContentSingleFile},
			Facts:         facts,
			Complexity:    classifyTaskComplexity(facts, nil),
		}
	}

	reason := ReasonDefaultConservative
	if facts.ChangedFileCount == 0 {
		reason = ReasonNoChangedFiles
	}
	return ReviewPolicyDecision{
		PolicyVersion: ReviewPolicyVersion,
		Decision:      ReviewRequired,
		Reasons:       []ReviewReason{reason},
		Facts:         facts,
		Complexity:    classifyTaskComplexity(facts, nil),
	}
}

// distinctTopLevelDirs counts distinct top-level directories among paths
// that actually have one — a repo-root file (e.g. README.md) alongside a
// single subdirectory's changes is not a multi-module change, so root-level
// paths are deliberately excluded from this count rather than treated as
// their own "." module.
func distinctTopLevelDirs(paths []string) int {
	dirs := map[string]bool{}
	for _, p := range paths {
		p = strings.TrimPrefix(p, "./")
		if idx := strings.Index(p, "/"); idx >= 0 {
			dirs[p[:idx]] = true
		}
	}
	return len(dirs)
}
