package workflow_test

// Checkpoint 8P-E.14B regression suite for the wf-6528a538 incident.
//
// Verification resolved the COMMAND context correctly and both Go commands
// passed:
//
//	go vet ./...                        requested "." resolved "backend"  PASS
//	go test ./internal/postrunqa/...    requested "." resolved "backend"  PASS
//
// and then failed the whole verification on a file check, because the file half
// of the same spec was evaluated in a different namespace:
//
//	verification.files: internal/postrunqa/classify.go
//	stat .../agent-orchestrator/internal/postrunqa: no such file or directory
//	-> verify_environment_error -> verify_unrepairable
//
// The file was exactly where the plan said it was — in the module the commands
// had just been resolved into. These tests pin the one rule both halves now
// share, in every layout it has to hold for.

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// incidentWorktree is the real repository's shape: a Go module below the
// registered worktree root, with the changed package inside it.
func incidentWorktree(t *testing.T) string {
	t.Helper()
	return writeWorktree(t, map[string]string{
		"backend/go.mod":                         "module x\n",
		"backend/internal/postrunqa/classify.go": "package postrunqa\n",
		"backend/internal/postrunqa/evidence.go": "package postrunqa\n",
		"frontend/package.json":                  "{}",
		"README.md":                              "# repo\n",
	})
}

// postrunqaPlan is the incident's own verification spec: commands and files
// both authored in the module's namespace, working directory left at ".".
func postrunqaPlan(filePath string) workflowcore.VerificationPlan {
	return workflowcore.VerificationPlan{
		Commands: []workflowcore.VerificationCommandCheck{
			{Command: "go", Args: []string{"vet", "./..."}, WorkingDirectory: ".", RequiredExitCode: 0, RetrySafe: true},
			{Command: "go", Args: []string{"test", "./internal/postrunqa/..."}, WorkingDirectory: ".", RequiredExitCode: 0, RetrySafe: true},
		},
		Files: []workflowcore.VerificationFileCheck{{Path: filePath, Exists: true}},
	}
}

// ---- A: the exact incident --------------------------------------------------

func TestVerifyFileCheckResolvesInTheSameContextAsTheCommands(t *testing.T) {
	root := incidentWorktree(t)
	runner := goModuleRunner(root)
	c, store, _, sender, runID := incidentFixture(t, root, postrunqaPlan("internal/postrunqa/classify.go"), runner)

	detail, err := c.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Run.State != domain.WorkflowRunCompleted {
		t.Fatalf("run = %q, want completed (steps: %s)", detail.Run.State, stepStates(detail))
	}
	if sender.calls != 0 {
		t.Fatalf("fix worker dispatched %d times for a path AO resolved in the wrong namespace", sender.calls)
	}
	for _, dir := range runner.dirs(root) {
		if dir != "backend" {
			t.Fatalf("verify commands ran in %v, want every one in backend", runner.dirs(root))
		}
	}

	res := latestVerifyResult(t, store, runID)
	if !res.Passed {
		t.Fatalf("verify did not pass: errorClass=%q checks=%+v", res.ErrorClass, res.Checks)
	}
	if res.ErrorClass != "" {
		t.Fatalf("errorClass = %q, want empty (the incident recorded verify_environment_error)", res.ErrorClass)
	}
	if res.PathContext != "backend" {
		t.Fatalf("pathContext = %q, want backend: the file half did not adopt the commands' namespace", res.PathContext)
	}
	fileCheck := onlyFileCheck(t, res)
	if !fileCheck.Passed {
		t.Fatalf("file check failed: %+v", fileCheck)
	}
	if fileCheck.ResolvedPath != "backend/internal/postrunqa/classify.go" {
		t.Fatalf("resolved path = %q, want backend/internal/postrunqa/classify.go", fileCheck.ResolvedPath)
	}
	if fileCheck.Label != "internal/postrunqa/classify.go" {
		t.Fatalf("label = %q, want the path exactly as the plan declared it", fileCheck.Label)
	}
	assertNoFixCycleAndNoAttention(t, store, runID)
}

// ---- B: a path the plan already qualified from the repository root ----------

func TestVerifyFileCheckResolvesARootQualifiedPathExactlyOnce(t *testing.T) {
	root := incidentWorktree(t)
	runner := goModuleRunner(root)
	c, store, _, _, runID := incidentFixture(t, root, postrunqaPlan("backend/internal/postrunqa/classify.go"), runner)

	detail, err := c.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Run.State != domain.WorkflowRunCompleted {
		t.Fatalf("run = %q, want completed (steps: %s)", detail.Run.State, stepStates(detail))
	}
	res := latestVerifyResult(t, store, runID)
	got := onlyFileCheck(t, res).ResolvedPath
	if got != "backend/internal/postrunqa/classify.go" {
		t.Fatalf("resolved path = %q, want backend/internal/postrunqa/classify.go (never backend/backend/...)", got)
	}
}

// ---- C: a root Go module keeps resolving from the repository root ----------

func TestVerifyFileCheckKeepsRootModuleResolution(t *testing.T) {
	root := writeWorktree(t, map[string]string{
		"go.mod":              "module x\n",
		"internal/foo/foo.go": "package foo\n",
	})
	plan := workflowcore.VerificationPlan{
		Commands: []workflowcore.VerificationCommandCheck{
			{Command: "go", Args: []string{"test", "./..."}, WorkingDirectory: ".", RequiredExitCode: 0, RetrySafe: true},
		},
		Files: []workflowcore.VerificationFileCheck{{Path: "internal/foo/foo.go", Exists: true}},
	}
	runner := goModuleRunner(root)
	c, store, _, _, runID := incidentFixture(t, root, plan, runner)

	detail, err := c.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Run.State != domain.WorkflowRunCompleted {
		t.Fatalf("run = %q, want completed (steps: %s)", detail.Run.State, stepStates(detail))
	}
	res := latestVerifyResult(t, store, runID)
	if res.PathContext != "." {
		t.Fatalf("pathContext = %q, want . for a root module", res.PathContext)
	}
	if got := onlyFileCheck(t, res).ResolvedPath; got != "internal/foo/foo.go" {
		t.Fatalf("resolved path = %q, want it unchanged at the repository root", got)
	}
	if len(res.ContextResolutions) != 0 {
		t.Fatalf("context resolutions = %+v, want none: nothing needed moving", res.ContextResolutions)
	}
}

// ---- D: an explicit, valid working directory stays authoritative -----------

func TestVerifyFileCheckResolvesAgainstAnExplicitWorkingDirectory(t *testing.T) {
	root := writeWorktree(t, map[string]string{
		"services/api/go.mod":       "module api\n",
		"services/api/handler.go":   "package api\n",
		"services/worker/go.mod":    "module worker\n",
		"services/worker/worker.go": "package worker\n",
	})
	plan := workflowcore.VerificationPlan{
		Commands: []workflowcore.VerificationCommandCheck{
			{Command: "go", Args: []string{"test", "./..."}, WorkingDirectory: "services/api", RequiredExitCode: 0, RetrySafe: true},
		},
		Files: []workflowcore.VerificationFileCheck{{Path: "handler.go", Exists: true}},
	}
	runner := goModuleRunner(root)
	c, store, _, _, runID := incidentFixture(t, root, plan, runner)

	detail, err := c.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Run.State != domain.WorkflowRunCompleted {
		t.Fatalf("run = %q, want completed (steps: %s)", detail.Run.State, stepStates(detail))
	}
	if dirs := runner.dirs(root); len(dirs) != 1 || dirs[0] != "services/api" {
		t.Fatalf("verify ran in %v, want the explicitly configured services/api", dirs)
	}
	res := latestVerifyResult(t, store, runID)
	if res.PathContext != "services/api" {
		t.Fatalf("pathContext = %q, want services/api", res.PathContext)
	}
	if got := onlyFileCheck(t, res).ResolvedPath; got != "services/api/handler.go" {
		t.Fatalf("resolved path = %q, want services/api/handler.go", got)
	}
	if len(res.ContextResolutions) != 0 {
		t.Fatalf("context resolutions = %+v, want none: the configured directory was already valid", res.ContextResolutions)
	}
}

// ---- E: several modules and nothing authoritative to choose ----------------

func TestVerifyRefusesToGuessAmongSeveralModules(t *testing.T) {
	root := writeWorktree(t, map[string]string{
		"backend/go.mod":      "module backend\n",
		"services/api/go.mod": "module api\n",
		"internal/foo/foo.go": "package foo\n",
	})
	plan := workflowcore.VerificationPlan{
		Commands: []workflowcore.VerificationCommandCheck{
			{Command: "go", Args: []string{"test", "./..."}, WorkingDirectory: ".", RequiredExitCode: 0, RetrySafe: true},
		},
		Files: []workflowcore.VerificationFileCheck{{Path: "internal/foo/foo.go", Exists: true}},
	}
	runner := goModuleRunner(root)
	c, store, _, sender, runID := incidentFixture(t, root, plan, runner)

	if _, err := c.GetRun(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	res := latestVerifyResult(t, store, runID)
	if res.Passed {
		t.Fatal("verification passed by guessing a module root among several")
	}
	if res.InfraFailure == nil {
		t.Fatalf("no infrastructure failure recorded: %+v", res.Checks)
	}
	if res.InfraFailure.Kind != workflowcore.VerifyInfraConfigInvalid {
		t.Fatalf("infra kind = %q, want config_invalid for an unresolvable layout", res.InfraFailure.Kind)
	}
	if !strings.Contains(res.InfraFailure.Detail, "multiple Go module roots") {
		t.Fatalf("detail = %q, want it to name the ambiguity rather than the toolchain message alone", res.InfraFailure.Detail)
	}
	// The guess must not have leaked into the path half either: verification
	// stopped before any file check ran in an invented namespace.
	for _, check := range res.Checks {
		if check.Kind == "file" {
			t.Fatalf("a file check ran in a guessed namespace: %+v", check)
		}
	}
	if sender.calls != 0 {
		t.Fatalf("fix worker dispatched %d times for a project-layout problem", sender.calls)
	}
}

// ---- F: a genuinely missing artifact is a real verification failure --------

// The old code resolved the artifact's PARENT through the working-directory
// checker, which stats. A missing directory therefore became
// verify_environment_error — an infrastructure verdict — instead of the
// artifact-missing verdict a file check exists to deliver.
func TestMissingArtifactIsAVerificationFailureNotAnInfrastructureError(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{"missing file in an existing directory", "internal/postrunqa/absent.go"},
		{"missing file in a directory that does not exist", "internal/nowhere/absent.go"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := incidentWorktree(t)
			runner := goModuleRunner(root)
			c, store, _, _, runID := incidentFixture(t, root, postrunqaPlan(tc.path), runner)

			if _, err := c.GetRun(context.Background(), runID); err != nil {
				t.Fatal(err)
			}
			res := latestVerifyResult(t, store, runID)
			if res.Passed {
				t.Fatal("a missing required artifact passed verification")
			}
			if res.ErrorClass != domain.WorkflowErrorVerifyArtifactMissing {
				t.Fatalf("errorClass = %q, want %q: a missing file is the code's problem, not AO's",
					res.ErrorClass, domain.WorkflowErrorVerifyArtifactMissing)
			}
			if res.InfraFailure != nil {
				t.Fatalf("infra failure recorded for a genuinely missing artifact: %+v", res.InfraFailure)
			}
			if got := onlyFileCheck(t, res).FailureReason; got != "required artifact is missing" {
				t.Fatalf("failure reason = %q, want the artifact-missing verdict", got)
			}
		})
	}
}

// ---- G: exact-content checks share the rule --------------------------------

func TestExactContentCheckUsesTheSamePathResolution(t *testing.T) {
	t.Run("matching content passes in the resolved context", func(t *testing.T) {
		root := incidentWorktree(t)
		want := "package postrunqa\n"
		plan := postrunqaPlan("internal/postrunqa/classify.go")
		plan.Files[0].ExactContent = &want
		runner := goModuleRunner(root)
		c, store, _, _, runID := incidentFixture(t, root, plan, runner)

		detail, err := c.GetRun(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		if detail.Run.State != domain.WorkflowRunCompleted {
			t.Fatalf("run = %q, want completed (steps: %s)", detail.Run.State, stepStates(detail))
		}
		res := latestVerifyResult(t, store, runID)
		if got := onlyFileCheck(t, res).ResolvedPath; got != "backend/internal/postrunqa/classify.go" {
			t.Fatalf("resolved path = %q, want the same resolution an existence check gets", got)
		}
	})

	t.Run("mismatching content is an artifact mismatch, not a path problem", func(t *testing.T) {
		root := incidentWorktree(t)
		other := "package something_else\n"
		plan := postrunqaPlan("internal/postrunqa/classify.go")
		plan.Files[0].ExactContent = &other
		runner := goModuleRunner(root)
		c, store, _, _, runID := incidentFixture(t, root, plan, runner)

		if _, err := c.GetRun(context.Background(), runID); err != nil {
			t.Fatal(err)
		}
		res := latestVerifyResult(t, store, runID)
		if res.ErrorClass != domain.WorkflowErrorVerifyArtifactMismatch {
			t.Fatalf("errorClass = %q, want %q: the file was found and read in the resolved context",
				res.ErrorClass, domain.WorkflowErrorVerifyArtifactMismatch)
		}
		if got := onlyFileCheck(t, res).ResolvedPath; got != "backend/internal/postrunqa/classify.go" {
			t.Fatalf("resolved path = %q, want the resolved context", got)
		}
	})
}

// ---- H: restart and repeated reconcile -------------------------------------

// Verification is keyed by (step, target), so polling it and restarting into it
// must reach the same durable answer without executing the checks again or
// recording a second, differently-resolved result.
func TestVerifyPathResolutionIsIdempotentAcrossPollsAndRestart(t *testing.T) {
	root := incidentWorktree(t)
	runner := goModuleRunner(root)
	c, store, clk, sender, runID := incidentFixture(t, root, postrunqaPlan("internal/postrunqa/classify.go"), runner)
	ctx := context.Background()

	if _, err := c.GetRun(ctx, runID); err != nil {
		t.Fatal(err)
	}
	commandsAfterFirstPass := len(runner.calls)
	if commandsAfterFirstPass == 0 {
		t.Fatal("no verification commands ran at all")
	}
	first := latestVerifyResult(t, store, runID)

	for i := 0; i < 5; i++ {
		clk.Advance(2)
		if _, err := c.GetRun(ctx, runID); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}
	// A restarted daemon over the same durable rows.
	restarted := restartVerifyCoordinator(t, store, root, runner, sender, clk)
	for i := 0; i < 3; i++ {
		clk.Advance(2)
		if _, err := restarted.GetRun(ctx, runID); err != nil {
			t.Fatalf("post-restart poll %d: %v", i, err)
		}
	}

	if len(runner.calls) != commandsAfterFirstPass {
		t.Fatalf("verification commands executed %d times, want the original %d: polling/restart re-ran the checks",
			len(runner.calls), commandsAfterFirstPass)
	}
	results := verifyResultCount(store, runID)
	if results != 1 {
		t.Fatalf("verify_result checkpoints = %d, want exactly 1", results)
	}
	last := latestVerifyResult(t, store, runID)
	if last.PathContext != first.PathContext || last.Passed != first.Passed || last.ErrorClass != first.ErrorClass {
		t.Fatalf("durable result changed across restart: %+v then %+v", first, last)
	}
	if sender.calls != 0 {
		t.Fatalf("fix worker dispatched %d times across polling and restart", sender.calls)
	}
}

// ---- the invariant ---------------------------------------------------------

// The rule this whole file exists to enforce, stated once and checked over
// every layout: within ONE verification spec, every relative path check is
// evaluated in the same namespace the spec's commands ran in, unless the path
// was already qualified from the repository root.
func TestVerifyCommandsAndFileChecksNeverDivergeInNamespace(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		plan  workflowcore.VerificationPlan
	}{
		{
			name:  "module below the worktree root, context-relative file",
			files: map[string]string{"backend/go.mod": "module x\n", "backend/internal/a/a.go": "package a\n"},
			plan:  planWithFile(".", "internal/a/a.go"),
		},
		{
			name:  "module below the worktree root, root-qualified file",
			files: map[string]string{"backend/go.mod": "module x\n", "backend/internal/a/a.go": "package a\n"},
			plan:  planWithFile(".", "backend/internal/a/a.go"),
		},
		{
			name:  "root module",
			files: map[string]string{"go.mod": "module x\n", "internal/a/a.go": "package a\n"},
			plan:  planWithFile(".", "internal/a/a.go"),
		},
		{
			name:  "explicit working directory",
			files: map[string]string{"services/api/go.mod": "module api\n", "services/api/a.go": "package api\n"},
			plan:  planWithFile("services/api", "a.go"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := writeWorktree(t, tc.files)
			runner := goModuleRunner(root)
			c, store, _, _, runID := incidentFixture(t, root, tc.plan, runner)
			if _, err := c.GetRun(context.Background(), runID); err != nil {
				t.Fatal(err)
			}
			res := latestVerifyResult(t, store, runID)
			if !res.Passed {
				t.Fatalf("verification failed: errorClass=%q checks=%+v", res.ErrorClass, res.Checks)
			}
			if res.PathContext == "" {
				t.Fatal("no path context recorded: the result does not state the namespace it used")
			}
			// Every command ran in the recorded namespace...
			for _, dir := range runner.dirs(root) {
				if dir != res.PathContext {
					t.Fatalf("a command ran in %q while the recorded path context is %q", dir, res.PathContext)
				}
			}
			// ...and every file check was evaluated in it, or was already
			// qualified from the repository root.
			for _, check := range res.Checks {
				if check.Kind != "file" {
					continue
				}
				joined := path.Join(res.PathContext, check.Label)
				if check.ResolvedPath != joined && check.ResolvedPath != check.Label {
					t.Fatalf("file %q resolved to %q, which is neither %q (the spec's namespace) nor the declared root-qualified path",
						check.Label, check.ResolvedPath, joined)
				}
				// And it really was read from there.
				if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(check.ResolvedPath))); err != nil {
					t.Fatalf("resolved path %q does not exist on disk: %v", check.ResolvedPath, err)
				}
			}
		})
	}
}

// The resolver's own algebra, independent of any run: the rule is purely
// syntactic, so it must be idempotent and must never double-apply a prefix.
func TestVerifyPathContextResolveIsIdempotent(t *testing.T) {
	for _, tc := range []struct{ base, in, want string }{
		{".", "internal/a/a.go", "internal/a/a.go"},
		{"", "internal/a/a.go", "internal/a/a.go"},
		{"backend", "internal/a/a.go", "backend/internal/a/a.go"},
		{"backend", "backend/internal/a/a.go", "backend/internal/a/a.go"},
		{"backend", "backend", "backend"},
		{"services/api", "handler.go", "services/api/handler.go"},
		{"services/api", "services/api/handler.go", "services/api/handler.go"},
		{"backend", "./internal/a/a.go", "backend/internal/a/a.go"},
	} {
		ctx := workflowcore.VerifyPathContext{Base: tc.base}
		got := ctx.ResolvePath(tc.in)
		if got != tc.want {
			t.Fatalf("ResolvePath(base=%q, %q) = %q, want %q", tc.base, tc.in, got, tc.want)
		}
		if again := ctx.ResolvePath(got); again != got {
			t.Fatalf("not idempotent: base=%q %q -> %q -> %q", tc.base, tc.in, got, again)
		}
	}
}

// ---- helpers ---------------------------------------------------------------

func planWithFile(workingDir, filePath string) workflowcore.VerificationPlan {
	return workflowcore.VerificationPlan{
		Commands: []workflowcore.VerificationCommandCheck{
			{Command: "go", Args: []string{"test", "./..."}, WorkingDirectory: workingDir, RequiredExitCode: 0, RetrySafe: true},
		},
		Files: []workflowcore.VerificationFileCheck{{Path: filePath, Exists: true}},
	}
}

func onlyFileCheck(t *testing.T, res workflowcore.VerifyResult) workflowcore.VerifyCheckResult {
	t.Helper()
	var found []workflowcore.VerifyCheckResult
	for _, check := range res.Checks {
		if check.Kind == "file" {
			found = append(found, check)
		}
	}
	if len(found) != 1 {
		t.Fatalf("file checks = %d, want exactly 1 (%+v)", len(found), res.Checks)
	}
	return found[0]
}

func verifyResultCount(store *fakeStore, runID string) int {
	n := 0
	for _, cp := range store.checkpoints[runID] {
		if cp.DurablePhase == "verify_result" {
			n++
		}
	}
	return n
}

// assertNoFixCycleAndNoAttention pins the incident's downstream damage: the
// misresolved path drove a verify failure, which drove verify_unrepairable and
// needs_attention.
func assertNoFixCycleAndNoAttention(t *testing.T, store *fakeStore, runID string) {
	t.Helper()
	if run := store.runs[runID]; run.State == domain.WorkflowRunNeedsAttention {
		t.Fatal("run parked in needs_attention after a verification whose checks all passed")
	}
	for _, cp := range store.checkpoints[runID] {
		switch cp.DurablePhase {
		case workflowcore.ReasonVerifyFixReentry:
			t.Fatalf("a verify-driven fix cycle was opened: %+v", cp)
		case workflowcore.ReasonVerifyUnrepairable, workflowcore.ReasonVerifyInfraFailed,
			workflowcore.ReasonVerifyConfigInvalid, workflowcore.ReasonVerifyToolUnavailable:
			t.Fatalf("verification stopped with %q: %s", cp.DurablePhase, cp.NextAction)
		}
	}
}

// restartVerifyCoordinator rebuilds a Coordinator over the SAME durable store —
// the daemon coming back — with the same worktree, verifier and message sender.
// The review/session doubles are re-seeded exactly as incidentFixture seeds
// them, because they stand in for facts that live outside workflow's own tables
// and therefore survive a restart too.
func restartVerifyCoordinator(t *testing.T, store *fakeStore, root string, runner workflowcore.VerifyRunner, sender *fakeMessageSender, clk *fakeClock) *workflowcore.Coordinator {
	t.Helper()
	approved := cleanObservation(root)
	reviews := newFakeReviewRuns()
	reviews.runs["review-incident"] = domain.ReviewRun{
		ID: "review-incident", SessionID: "sess-incident", Harness: domain.ReviewerClaudeCode,
		Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved,
		TargetSHA: workflowcore.WorkspaceFingerprint(approved),
	}
	facts := newFakeSessionFacts()
	facts.put(domain.SessionRecord{
		ID: "sess-incident", ProjectID: "project-1",
		Activity: domain.Activity{State: domain.ActivityActive},
		Metadata: domain.SessionMetadata{Branch: "feature", WorkspacePath: root},
	})
	ids := 0
	return workflowcore.New(workflowcore.Deps{
		Store: store, ReviewRuns: reviews, WorkspaceFacts: &mutableWorkspaceFacts{obs: approved},
		SessionFacts: facts, Verifier: runner, MessageSender: sender,
		Clock: clk.Now, NewID: func() string { ids++; return fmt.Sprintf("rst%d", ids) },
	})
}

// ---- containment ------------------------------------------------------------

// secureVerifyArtifactPath replaced the old "resolve the parent through the
// working-directory checker" containment, so the containment itself is pinned
// here: relaxing the existence requirement must not have relaxed the boundary.
func TestVerifyFileCheckRefusesToReadOutsideTheWorktree(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		link func(t *testing.T, root string)
		path string
	}{
		{
			name: "file symlink pointing out of the worktree",
			link: func(t *testing.T, root string) {
				linkAt(t, filepath.Join(outside, "secret.txt"), filepath.Join(root, "backend/internal/a/leak.txt"))
			},
			path: "internal/a/leak.txt",
		},
		{
			name: "directory symlink pointing out of the worktree",
			link: func(t *testing.T, root string) {
				linkAt(t, outside, filepath.Join(root, "backend/internal/escape"))
			},
			path: "internal/escape/secret.txt",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := writeWorktree(t, map[string]string{
				"backend/go.mod":          "module x\n",
				"backend/internal/a/a.go": "package a\n",
			})
			tc.link(t, root)
			runner := goModuleRunner(root)
			plan := planWithFile(".", tc.path)
			c, store, _, _, runID := incidentFixture(t, root, plan, runner)

			if _, err := c.GetRun(context.Background(), runID); err != nil {
				t.Fatal(err)
			}
			res := latestVerifyResult(t, store, runID)
			if res.Passed {
				t.Fatal("verification read an artifact from outside the worktree")
			}
			if res.ErrorClass != domain.WorkflowErrorVerifyEnvironment {
				t.Fatalf("errorClass = %q, want %q for an escape attempt", res.ErrorClass, domain.WorkflowErrorVerifyEnvironment)
			}
			if got := onlyFileCheck(t, res).FailureReason; !strings.Contains(got, "escapes the worktree") {
				t.Fatalf("failure reason = %q, want it to name the escape", got)
			}
		})
	}
}

// A symlink that stays inside the worktree is ordinary content and must still
// be readable — containment is about leaving, not about links.
func TestVerifyFileCheckFollowsSymlinksInsideTheWorktree(t *testing.T) {
	root := writeWorktree(t, map[string]string{
		"backend/go.mod":          "module x\n",
		"backend/internal/a/a.go": "package a\n",
	})
	linkAt(t, filepath.Join(root, "backend/internal/a/a.go"), filepath.Join(root, "backend/internal/a/alias.go"))
	runner := goModuleRunner(root)
	c, store, _, _, runID := incidentFixture(t, root, planWithFile(".", "internal/a/alias.go"), runner)

	if _, err := c.GetRun(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	res := latestVerifyResult(t, store, runID)
	if !res.Passed {
		t.Fatalf("an in-worktree symlink was rejected: errorClass=%q checks=%+v", res.ErrorClass, res.Checks)
	}
}

func linkAt(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
