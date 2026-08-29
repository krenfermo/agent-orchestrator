package workflow

import (
	stdctx "context"
	"path"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// workspaceChangedPaths extracts the changed-file path list from a
// WorkspaceObservation — the single shared conversion both ReviewPolicy's
// fact-gathering (review_policy_dispatch.go) and VerifyScopePolicy use, so
// "what files changed" has exactly one implementation.
func workspaceChangedPaths(obs ports.WorkspaceObservation) []string {
	paths := make([]string, 0, len(obs.Changes))
	for _, ch := range obs.Changes {
		paths = append(paths, ch.Path)
	}
	return paths
}

// VerifyScopePolicyVersion is versioned independently from
// ReviewPolicyVersion — the two policies are evaluated at different points
// in the lifecycle (review decision at cycle 1, verify scope at every
// verify attempt) and may evolve on different schedules.
const VerifyScopePolicyVersion = "v1"

// VerifyScope is the granularity Checkpoint 8I selects for a step's
// deterministic Verify commands. Escalates only when policy/risk requires it
// (checkpoint brief §11-14): TARGETED (single Go package) is cheapest,
// MODULE narrows to the smallest common ancestor directory of every changed
// file, REPOSITORY runs the Planner's commands completely unmodified.
type VerifyScope string

const (
	VerifyScopeTargeted   VerifyScope = "targeted"
	VerifyScopeModule     VerifyScope = "module"
	VerifyScopeRepository VerifyScope = "repository"
)

// VerifyScopeReason is a stable, machine-checkable code explaining why a
// particular VerifyScope was chosen, persisted alongside the scope so a run
// created today remains explainable later (mirrors ReviewReason).
type VerifyScopeReason string

const (
	VerifyScopeReasonSensitiveRequiredReview     VerifyScopeReason = "sensitive_change_requires_repository_gate"
	VerifyScopeReasonFinalIntegrationTask        VerifyScopeReason = "final_or_integration_task"
	VerifyScopeReasonNoGoChanges                 VerifyScopeReason = "no_go_package_signal_to_narrow"
	VerifyScopeReasonSinglePackage               VerifyScopeReason = "single_go_package_changed"
	VerifyScopeReasonSharedModule                VerifyScopeReason = "shared_module_ancestor_directory"
	VerifyScopeReasonPlannerCommandNotNarrowable VerifyScopeReason = "planner_command_not_safely_narrowable"
)

// VerifyScopeDecision is the durable, explainable result of evaluating
// VerifyScopePolicy for one verify attempt.
type VerifyScopeDecision struct {
	PolicyVersion string              `json:"policyVersion"`
	Scope         VerifyScope         `json:"scope"`
	Reasons       []VerifyScopeReason `json:"reasons"`
	PackageDir    string              `json:"packageDir,omitempty"`
	ChangedFiles  []string            `json:"changedFiles"`
}

// requiredReasonsForcingRepositoryGate is the subset of ReviewReason values
// that, when present in a task's ReviewPolicyDecision, force a REPOSITORY
// verify gate regardless of how narrow the changed-file set looks —
// narrowing Verify would undermine exactly the sensitivity ReviewPolicy just
// flagged (checkpoint brief §12: "risk" is an explicit VerifyScopePolicy
// input).
var requiredReasonsForcingRepositoryGate = map[ReviewReason]bool{
	ReasonAuthOrSecurityPath:     true,
	ReasonMigrationOrSchemaPath:  true,
	ReasonConcurrencyPath:        true,
	ReasonInfraOrCICDPath:        true,
	ReasonPublicAPIPath:          true,
	ReasonDestructiveIntent:      true,
	ReasonDependencyConfigChange: true,
	ReasonLargeOrMultiModule:     true,
}

// ComputeVerifyScope is the pure, deterministic VerifyScopePolicy v1
// decision function. reviewReasons is whatever ReviewPolicy already computed
// for this task (nil/empty is fine — a SKIPPED-by-policy task carries no
// required reasons). isFinalIntegrationTask is a deterministic sink-node
// signal from the plan's dependency graph (see
// coordinatorIsFinalIntegrationTask), never derived from the Planner's free-
// text title (checkpoint brief §15).
func ComputeVerifyScope(reviewReasons []ReviewReason, isFinalIntegrationTask bool, changedFilePaths []string) VerifyScopeDecision {
	for _, r := range reviewReasons {
		if requiredReasonsForcingRepositoryGate[r] {
			return VerifyScopeDecision{
				PolicyVersion: VerifyScopePolicyVersion,
				Scope:         VerifyScopeRepository,
				Reasons:       []VerifyScopeReason{VerifyScopeReasonSensitiveRequiredReview},
				ChangedFiles:  changedFilePaths,
			}
		}
	}
	if isFinalIntegrationTask {
		return VerifyScopeDecision{
			PolicyVersion: VerifyScopePolicyVersion,
			Scope:         VerifyScopeRepository,
			Reasons:       []VerifyScopeReason{VerifyScopeReasonFinalIntegrationTask},
			ChangedFiles:  changedFilePaths,
		}
	}

	goDirs := goPackageDirs(changedFilePaths)
	if len(goDirs) == 0 {
		return VerifyScopeDecision{
			PolicyVersion: VerifyScopePolicyVersion,
			Scope:         VerifyScopeRepository,
			Reasons:       []VerifyScopeReason{VerifyScopeReasonNoGoChanges},
			ChangedFiles:  changedFilePaths,
		}
	}
	if len(goDirs) == 1 {
		return VerifyScopeDecision{
			PolicyVersion: VerifyScopePolicyVersion,
			Scope:         VerifyScopeTargeted,
			Reasons:       []VerifyScopeReason{VerifyScopeReasonSinglePackage},
			PackageDir:    goDirs[0],
			ChangedFiles:  changedFilePaths,
		}
	}
	ancestor := commonAncestorDir(goDirs)
	if ancestor == "" || ancestor == "." {
		return VerifyScopeDecision{
			PolicyVersion: VerifyScopePolicyVersion,
			Scope:         VerifyScopeRepository,
			Reasons:       []VerifyScopeReason{VerifyScopeReasonPlannerCommandNotNarrowable},
			ChangedFiles:  changedFilePaths,
		}
	}
	return VerifyScopeDecision{
		PolicyVersion: VerifyScopePolicyVersion,
		Scope:         VerifyScopeModule,
		Reasons:       []VerifyScopeReason{VerifyScopeReasonSharedModule},
		PackageDir:    ancestor,
		ChangedFiles:  changedFilePaths,
	}
}

// goPackageDirs returns the sorted, de-duplicated set of directories
// containing a changed .go file, using forward-slash paths as reported by
// git status (ObserveWorkspace's own path convention).
func goPackageDirs(changedFilePaths []string) []string {
	seen := map[string]bool{}
	var dirs []string
	for _, p := range changedFilePaths {
		if !strings.HasSuffix(p, ".go") {
			continue
		}
		dir := path.Dir(path.Clean(p))
		if dir == "." {
			continue // repo-root .go files: no safe subpackage to narrow to
		}
		if !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// commonAncestorDir returns the deepest directory that is a prefix of every
// given directory's path segments, or "" if the only common ancestor is the
// repository root (nothing safe to narrow to).
func commonAncestorDir(dirs []string) string {
	if len(dirs) == 0 {
		return ""
	}
	common := strings.Split(dirs[0], "/")
	for _, d := range dirs[1:] {
		segs := strings.Split(d, "/")
		common = commonPrefix(common, segs)
		if len(common) == 0 {
			return ""
		}
	}
	return strings.Join(common, "/")
}

func commonPrefix(a, b []string) []string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return a[:i]
}

// narrowGoWildcardCommand rewrites a single VerificationCommandCheck's
// `./...` target to `./<packageDir>/...` ONLY when the command is
// unambiguously a whole-repo Go invocation from the worktree root: binary
// "go", first arg one of build/vet/test, last arg exactly "./...", and
// WorkingDirectory empty (already repo root — never rewrites a command the
// Planner already scoped to a subdirectory, and never touches any non-Go
// command). Returns the original check unchanged, plus false, when the
// transform is not safely recognizable — Checkpoint 8I's brief is explicit
// that only DEMONSTRABLY safe transforms are applied ("no hagas parsing
// frágil para todos los lenguajes de golpe").
func narrowGoWildcardCommand(check VerificationCommandCheck, packageDir string) (VerificationCommandCheck, bool) {
	if check.Command != "go" || check.WorkingDirectory != "" || packageDir == "" {
		return check, false
	}
	if len(check.Args) < 2 {
		return check, false
	}
	verb := check.Args[0]
	if verb != "build" && verb != "vet" && verb != "test" {
		return check, false
	}
	last := check.Args[len(check.Args)-1]
	if last != "./..." {
		return check, false
	}
	narrowed := check
	narrowed.Args = append([]string{}, check.Args...)
	narrowed.Args[len(narrowed.Args)-1] = "./" + packageDir + "/..."
	return narrowed, true
}

// NarrowVerificationPlan applies decision's scope to plan's commands,
// returning a new VerificationPlan. TARGETED/MODULE only ever narrow
// recognizable `go build|vet|test ./...` commands from the repo root; every
// other command (any non-Go tool, any command the Planner already scoped to
// a subdirectory, any file check) passes through byte-identical. REPOSITORY
// always returns plan unchanged. This function never invents a command that
// was not already in the Planner's plan (checkpoint brief §13, translated from
// the Spanish: "do not invent arbitrary commands").
func NarrowVerificationPlan(plan VerificationPlan, decision VerifyScopeDecision) (VerificationPlan, []string) {
	if decision.Scope == VerifyScopeRepository || decision.PackageDir == "" {
		return plan, nil
	}
	narrowed := VerificationPlan{Files: plan.Files, Commands: make([]VerificationCommandCheck, len(plan.Commands))}
	var applied []string
	for i, check := range plan.Commands {
		if nc, ok := narrowGoWildcardCommand(check, decision.PackageDir); ok {
			narrowed.Commands[i] = nc
			applied = append(applied, strings.Join(append([]string{check.Command}, check.Args...), " ")+
				" -> "+strings.Join(append([]string{nc.Command}, nc.Args...), " "))
		} else {
			narrowed.Commands[i] = check
		}
	}
	return narrowed, applied
}

// reviewPolicyReasonsForStep re-reads the review step's own
// review_policy_decision checkpoint (persisted once, at cycle 1, by
// persistReviewPolicyDecision) so VerifyScopePolicy can consult the exact
// same risk reasons ReviewPolicy already computed — never a second,
// possibly-divergent risk evaluation. Returns (nil, false) when no decision
// checkpoint exists yet (defensive: every review step reaching Verify has
// one by construction) or is unreadable; callers must treat that as "cannot
// prove low risk" and default to the safest scope.
func (c *Coordinator) reviewPolicyReasonsForStep(ctx stdctx.Context, runID, reviewStepID string) ([]ReviewReason, bool) {
	decision, ok := c.reviewPolicyDecisionForStep(ctx, runID, reviewStepID)
	if !ok {
		return nil, false
	}
	return decision.Reasons, true
}

// reviewPolicyDecisionForStep re-reads a review step's own
// review_policy_decision checkpoint. Shared by reviewPolicyReasonsForStep
// (VerifyScopePolicy input) and maybeVerify's SKIPPED-review branch
// (verify.go), so both consult the exact same durable record rather than
// two independently-derived readings of "was this skipped."
func (c *Coordinator) reviewPolicyDecisionForStep(ctx stdctx.Context, runID, reviewStepID string) (ReviewPolicyDecision, bool) {
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return ReviewPolicyDecision{}, false
	}
	var latest *domain.WorkflowCheckpoint
	for i := range checkpoints {
		cp := &checkpoints[i]
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != reviewStepID || cp.DurablePhase != reviewPolicyDurablePhase {
			continue
		}
		if latest == nil || cp.CreatedAt.After(latest.CreatedAt) {
			latest = cp
		}
	}
	if latest == nil {
		return ReviewPolicyDecision{}, false
	}
	return decodeReviewPolicyDecision(latest.RetryState)
}

// isFinalIntegrationTask deterministically identifies a "last/integration"
// task as a sink node in the master plan's dependency graph — a task no
// other task depends on — never from the Planner's free-text title
// (checkpoint brief §15). A single-task run (no master plan parent) is
// always treated as final: narrowing Verify scope for a standalone
// objective would remove the only safety net it has, so the pre-8I
// repository-wide behavior is preserved unchanged for that case. Any lookup
// failure defaults to true (repository gate) for the same reason
// reviewPolicyReasonsForStep defaults conservatively.
func (c *Coordinator) isFinalIntegrationTask(ctx stdctx.Context, run domain.WorkflowRun) bool {
	if run.ParentWorkflowID == nil || run.PlannedTaskID == nil || c.planStore == nil {
		return true
	}
	tasks, err := c.planStore.ListWorkflowTasks(ctx, *run.ParentWorkflowID)
	if err != nil {
		return true
	}
	dependedOn := map[string]bool{}
	for _, t := range tasks {
		for _, dep := range t.Dependencies {
			dependedOn[dep] = true
		}
	}
	return !dependedOn[*run.PlannedTaskID]
}
