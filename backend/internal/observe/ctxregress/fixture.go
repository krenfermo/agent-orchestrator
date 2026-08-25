package ctxregress

import (
	stdctx "context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	plannercommand "github.com/aoagents/agent-orchestrator/backend/internal/adapters/planner/command"
	"github.com/aoagents/agent-orchestrator/backend/internal/codegraph"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	memory "github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// fixtureAgent stands in for every provider the run would otherwise call. It
// implements the workflow ports the dispatch wrappers wrap, so the wrappers
// under test are the real ones and only the thing on the far side of them is
// simulated.
//
// Its judgement is deliberately mechanical: it reads the context it was handed
// and asks whether the facts the fixture task depends on are still in it. An
// agent whose verdict depended on anything else would make the outcome gate
// unable to distinguish "routing dropped the evidence" from "the model felt
// differently this time", which is the one distinction the gate exists to make.
type fixtureAgent struct {
	fixture Fixture

	mu           sync.Mutex
	missing      []string
	dispatched   bool
	reads        int64
	tools        int64
	plannerBuild plannercommand.ContextBuilder
}

func newFixtureAgent(fixture Fixture) *fixtureAgent {
	return &fixtureAgent{fixture: fixture}
}

// plannerContext builds the planner's context document with the same builder
// the daemon wires, so what the router budgets here is what it would budget in
// production.
func (a *fixtureAgent) plannerContext(ctx stdctx.Context, project domain.ProjectRecord) (workflowcore.PlannerContext, error) {
	return a.plannerBuild.Build(ctx, project)
}

// spawnConfig is the worker dispatch the fixture task produces: the prompt the
// real builder makes, plus the pre-fetched tracker context that carries the
// task's required facts. The router budgets the second and never the first.
func (a *fixtureAgent) spawnConfig(fixture Fixture, artifact workflowcore.PlanArtifact) ports.SpawnConfig {
	return ports.SpawnConfig{
		ProjectID:    domain.ProjectID(fixture.ProjectID),
		IssueID:      domain.IssueID(fixture.TaskID),
		IssueContext: fixture.IssueContext,
		Kind:         domain.KindWorker,
		Harness:      domain.HarnessClaudeCode,
		Prompt:       workflowcore.BuildWorkStepPromptWithSpec(artifact, ""),
	}
}

// GetProject resolves the fixture project for the worker-routing path, which
// needs the checkout root the spawn config does not carry.
func (a *fixtureAgent) GetProject(_ stdctx.Context, id string) (domain.ProjectRecord, bool, error) {
	if id != a.fixture.ProjectID {
		return domain.ProjectRecord{}, false, nil
	}
	return domain.ProjectRecord{ID: a.fixture.ProjectID, Path: a.fixture.Repo, DisplayName: filepath.Base(a.fixture.Repo)}, true, nil
}

// Generate stands in for plan generation. The plan is fixed: the fixture task
// is one task, and a planner that invented a different one per run would make
// the two runs incomparable.
func (a *fixtureAgent) Generate(_ stdctx.Context, request workflowcore.PlannerRequest) (workflowcore.PlannerResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tools++
	return workflowcore.PlannerResponse{
		Plan:     workflowcore.MasterPlan{Version: "v1", Objective: request.Objective},
		Provider: "ctxregress",
		Model:    "fixture",
	}, nil
}

// Spawn is where the outcome is decided. The agent looks for the facts the
// task depends on in everything it was handed, and counts one repository read
// per fact it has to go looking for.
//
// A fact it cannot find is not recoverable by reading: the fixture's required
// facts exist only in the tracker context, exactly like a decision recorded in
// an issue and nowhere in the checkout. That is why a dropped fact blocks the
// task instead of merely making it more expensive.
func (a *fixtureAgent) Spawn(_ stdctx.Context, cfg ports.SpawnConfig) (domain.SessionRecord, int, int, error) {
	received := cfg.Prompt + "\n" + cfg.IssueContext
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dispatched = true
	a.missing = nil
	for _, fact := range a.fixture.RequiredFacts {
		if strings.Contains(received, fact) {
			continue
		}
		a.missing = append(a.missing, fact)
		// The agent searches the checkout for what it was not given: one read
		// and one tool call per fact it goes looking for.
		a.reads++
		a.tools++
	}
	// The edit it makes (or attempts) is one more tool call.
	a.tools++
	return domain.SessionRecord{
		ID:        domain.SessionID("ctxregress-" + a.fixture.TaskID),
		ProjectID: cfg.ProjectID,
		IssueID:   cfg.IssueID,
		Kind:      cfg.Kind,
		Harness:   cfg.Harness,
	}, 1, 0, nil
}

// Preflight always succeeds: the fixture reviewer needs no binary.
func (a *fixtureAgent) Preflight(_ stdctx.Context, _ domain.ReviewerHarness, _ string) error {
	return nil
}

// Launch stands in for the reviewer. It approves the work the worker was able
// to do and asks for changes when the worker was blocked.
func (a *fixtureAgent) Launch(_ stdctx.Context, req workflowcore.ReviewerLaunchRequest) (workflowcore.ReviewerLaunchResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tools++
	return workflowcore.ReviewerLaunchResult{
		HandleID:       req.RunID + "-handle",
		AgentSessionID: req.RunID + "-reviewer",
	}, nil
}

// Send stands in for fix delivery. The fix prompt carries the review findings,
// not the facts the worker was never given, so it does not change the outcome
// — which is the honest simulation of a fix cycle that cannot recover context
// that was budgeted away.
func (a *fixtureAgent) Send(_ stdctx.Context, _ domain.SessionID, _ string, _ *ports.SpawnAttachment) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tools++
	return nil
}

// Run stands in for verification: it passes when the task completed and fails
// when it did not.
func (a *fixtureAgent) Run(_ stdctx.Context, _ workflowcore.VerifyCommandRequest) (workflowcore.VerifyCommandExecution, error) {
	exit := 0
	if a.verifyStatus() != StatusPassed {
		exit = 1
	}
	return workflowcore.VerifyCommandExecution{ExitCode: exit, DurationMS: 0}, nil
}

func (a *fixtureAgent) taskStatus() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.dispatched {
		return StatusFailed
	}
	if len(a.missing) > 0 {
		return StatusBlocked
	}
	return StatusCompleted
}

func (a *fixtureAgent) reviewStatus() Status {
	if a.taskStatus() != StatusCompleted {
		return StatusChangesRequested
	}
	return StatusApproved
}

func (a *fixtureAgent) verifyStatus() Status {
	if a.taskStatus() != StatusCompleted {
		return StatusFailed
	}
	return StatusPassed
}

func (a *fixtureAgent) findings() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.missing) == 0 {
		return "No findings."
	}
	return "The task could not be completed: the dispatched context did not carry " + strings.Join(a.missing, "; ") + "."
}

func (a *fixtureAgent) missingFacts() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.missing))
	copy(out, a.missing)
	return out
}

func (a *fixtureAgent) toolCalls() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tools
}

func (a *fixtureAgent) fileReads() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reads
}

// The facts the default fixture task cannot be completed without. They are
// deliberately unguessable strings that appear only in the tracker context, so
// "the agent found them" can only mean "the dispatched context still carried
// them".
const (
	factDecision = "AO-FIXTURE-DECISION-7419: retries are capped at 3 attempts and the fourth escalates to a human"
	factOwner    = "AO-FIXTURE-OWNER: the platform-runtime group owns the retry budget and must approve any change to it"
)

// DefaultFixture is the task the shipped regression gate runs. Repo must be an
// existing checkout — WriteFixtureRepo builds a self-contained one.
func DefaultFixture(repo string) Fixture {
	return Fixture{
		ProjectID:     "ctxregress-fixture",
		Repo:          repo,
		TaskID:        "ctxregress-task-1",
		Objective:     "Bound the dispatch retry loop and escalate once the cap is reached, following the decision recorded on the tracker issue.",
		IssueContext:  fixtureIssueContext(),
		RequiredFacts: []string{factDecision, factOwner},
		VerifyCommand: "go",
		VerifyArgs:    []string{"build", "./..."},
	}
}

// fixtureIssueContext is the pre-fetched tracker context a worker spawn sends
// in full today: the decision at the top, then the discussion that produced it.
//
// It is long on purpose. A tracker context that already fit inside the worker
// budget would make the routed and unrouted payloads identical, and a
// regression harness whose two runs send the same bytes measures nothing.
func fixtureIssueContext() string {
	var b strings.Builder
	b.WriteString("# Issue AO-7419: dispatch retry loop is unbounded\n\n")
	b.WriteString("## Decision\n\n")
	b.WriteString(factDecision + "\n\n")
	b.WriteString(factOwner + "\n\n")
	b.WriteString("## Discussion\n\n")
	// Roughly 40 KB of prior discussion, which is ordinary for a long-running
	// tracker issue and several times the worker role's document budget.
	for i := 1; i <= 220; i++ {
		fmt.Fprintf(&b, "- comment %d: the retry loop was observed spinning against a provider that was down; "+
			"several people proposed different caps before the decision above was recorded, and the transcript of "+
			"that argument is preserved here because the tracker keeps everything.\n", i)
	}
	return b.String()
}

// WriteFixtureRepo materialises a small, self-contained git checkout for the
// fixture task: a few committed source files, a documents file, and an
// uncommitted change so the router's diff source has something real to report.
//
// It is a purpose-built repository rather than the caller's own checkout so
// the comparison does not depend on whatever happens to be uncommitted on the
// machine running it — the harness must measure the router, not the working
// tree it was run in.
func WriteFixtureRepo(dir string) (Fixture, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Fixture{}, fmt.Errorf("resolve fixture dir: %w", err)
	}
	if _, err := exec.LookPath("git"); err != nil {
		return Fixture{}, fmt.Errorf("ctxregress: git is required to build the fixture checkout: %w", err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return Fixture{}, fmt.Errorf("create fixture dir: %w", err)
	}
	write := func(name, content string) error {
		path := filepath.Join(abs, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return err
		}
		return os.WriteFile(path, []byte(content), 0o600)
	}
	git := func(args ...string) error {
		cmd := exec.Command("git", append([]string{"-C", abs}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=ao", "GIT_AUTHOR_EMAIL=ao@example.test",
			"GIT_COMMITTER_NAME=ao", "GIT_COMMITTER_EMAIL=ao@example.test",
		)
		if out, cmdErr := cmd.CombinedOutput(); cmdErr != nil {
			return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), cmdErr, out)
		}
		return nil
	}

	files := map[string]string{
		"AGENTS.md":   "# Fixture project\n\nRetries live in retry.go. The retry budget is a product decision, not a code detail.\n",
		"README.md":   "# ctxregress fixture\n\nA minimal checkout the context-router regression harness routes against.\n",
		"retry.go":    "package fixture\n\n// Retry runs attempt until it succeeds.\nfunc Retry(attempt func() error) error {\n\tfor {\n\t\tif err := attempt(); err == nil {\n\t\t\treturn nil\n\t\t}\n\t}\n}\n",
		"dispatch.go": "package fixture\n\n// Dispatch hands work to a provider.\nfunc Dispatch(work string) error {\n\treturn Retry(func() error { return send(work) })\n}\n\nfunc send(string) error { return nil }\n",
	}
	for name, content := range files {
		if err := write(name, content); err != nil {
			return Fixture{}, fmt.Errorf("write %s: %w", name, err)
		}
	}
	if err := git("init", "--initial-branch=main"); err != nil {
		return Fixture{}, err
	}
	if err := git("add", "."); err != nil {
		return Fixture{}, err
	}
	if err := git("commit", "-m", "fixture base"); err != nil {
		return Fixture{}, err
	}
	// The uncommitted change: what the diff source reports, and what a routed
	// worker payload is assembled around.
	if err := write("retry.go", "package fixture\n\n// Retry runs attempt until it succeeds or the budget is spent.\nfunc Retry(attempt func() error) error {\n\tvar last error\n\tfor i := 0; i < 3; i++ {\n\t\tif last = attempt(); last == nil {\n\t\t\treturn nil\n\t\t}\n\t}\n\treturn last\n}\n"); err != nil {
		return Fixture{}, fmt.Errorf("write retry.go: %w", err)
	}
	return DefaultFixture(abs), nil
}

// IndexFixtureEvidence populates the two stores the router reuses instead of
// reading — the code graph and durable project memory — for a fixture
// checkout.
//
// It exists so the reused-versus-newly-read split the routing metrics record
// is exercised by something other than zero. Without it the comparison still
// runs and still gates outcomes, but every routed byte is content read for the
// dispatch, and a metric that is structurally always zero proves nothing about
// the code that computes it.
//
// Both stores live under AO's data dir (AO_DATA_DIR, else ~/.ao/data), so a
// caller that wants this kept out of a real installation points that at a
// temporary directory first.
func IndexFixtureEvidence(ctx stdctx.Context, fixture Fixture) error {
	graphStore, err := codegraph.NewDefaultStore()
	if err != nil {
		return fmt.Errorf("open the code graph store: %w", err)
	}
	indexer, err := codegraph.NewNativeIndexer(graphStore)
	if err != nil {
		return fmt.Errorf("build the native indexer: %w", err)
	}
	if _, err := indexer.Index(ctx, codegraph.IndexRequest{ProjectRoot: fixture.Repo}); err != nil {
		return fmt.Errorf("index the fixture checkout: %w", err)
	}
	memoryStore, err := memory.NewDefaultStore()
	if err != nil {
		return fmt.Errorf("open the project memory store: %w", err)
	}
	if _, err := memoryStore.Upsert(ctx, fixtureMemory(fixture)...); err != nil {
		return fmt.Errorf("seed fixture project memory: %w", err)
	}
	return nil
}

// fixtureMemory is the durable project memory the fixture project carries:
// facts about the checkout that AO derived once and can hand over without
// re-reading anything. They are notes about the files the fixture task
// touches, so the router's memory ranking has something relevant to find.
func fixtureMemory(fixture Fixture) []memory.MemoryItem {
	return []memory.MemoryItem{
		{
			Project:    fixture.ProjectID,
			Scope:      "retry.go",
			Type:       memory.TypeNote,
			Content:    "retry.go holds the dispatch retry loop; every retry-policy change in this project lands there.",
			Source:     memory.Source{Kind: memory.SourceManual, Path: "retry.go", Detail: "ctxregress fixture"},
			Confidence: 0.9,
		},
		{
			Project:    fixture.ProjectID,
			Scope:      "dispatch.go",
			Type:       memory.TypeNote,
			Content:    "dispatch.go calls Retry and is the only caller; a change to the retry signature has exactly one call site.",
			Source:     memory.Source{Kind: memory.SourceManual, Path: "dispatch.go", Detail: "ctxregress fixture"},
			Confidence: 0.9,
		},
	}
}
