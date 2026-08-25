// Package ctxregress is the disabled-vs-enabled regression harness for AO's
// role-aware context router.
//
// The router's whole claim is that AO can send an agent less and still get the
// same result. The first half of that claim is easy to measure and the second
// half is the one that matters: a payload that is 60% smaller and produces a
// worse outcome is not an improvement, it is a regression with a flattering
// metric attached. This harness exists so the second half is checked as
// mechanically as the first.
//
// It runs ONE fixture task through the real dispatch wrappers twice — once
// with the router absent, once with it installed — and compares the two runs
// on outcome first and size second:
//
//   - Outcome is the gate. If the task, review, or verify status of the routed
//     run differs from the unrouted one, the comparison is a regression and
//     the harness fails, whatever the measured saving was. It is a difference
//     check rather than a "did it get worse" check on purpose: an outcome that
//     changed in either direction means routing changed what the pipeline
//     decided, which is exactly the thing this gate exists to notice.
//   - Size is the report. The measured context-reduction percentage is printed
//     for the run, alongside the tool-call and file-read counts of both, so a
//     saving is always read next to what it cost.
//
// What is real here and what is not:
//
//   - Real: the dispatch wrappers (contextrouter/wfrouter and
//     observe/projectmemory/wfdispatch), the router itself and its budgets,
//     the git diff and code-graph and memory evidence sources, the prompt
//     builders the daemon's own dispatchers call, and every byte count — those
//     are measured on the actual payloads that went out.
//   - Not real: the agent. No provider is called. The fixture agent is a
//     deterministic stand-in whose outcome depends on exactly one thing:
//     whether the context it was handed still carried the facts the fixture
//     task cannot be completed without. That is what makes the outcome gate a
//     test of routing rather than a test of a model's mood, and it is also why
//     a passing comparison means "routing preserved this task's evidence", not
//     "routing is safe for every task".
package ctxregress

import (
	stdctx "context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/contextrouter"
	"github.com/aoagents/agent-orchestrator/backend/internal/contextrouter/wfrouter"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory/wfdispatch"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// ErrFixture is the sentinel every rejected fixture wraps.
var ErrFixture = errors.New("ctxregress: invalid fixture")

// Status is one dispatch outcome the comparison gates on.
type Status string

// The statuses the fixture task can reach. They are the harness's own
// vocabulary: the point is that the two runs agree, not that they match any
// particular durable enum.
const (
	// StatusCompleted / StatusApproved / StatusPassed are the outcomes of a
	// fixture run whose context carried what the task needed.
	StatusCompleted Status = "completed"
	StatusApproved  Status = "approved"
	StatusPassed    Status = "passed"
	// StatusBlocked, StatusChangesRequested and StatusFailed are the outcomes
	// when it did not.
	StatusBlocked          Status = "blocked_missing_context"
	StatusChangesRequested Status = "changes_requested"
	StatusFailed           Status = "failed"
)

// Fixture is the task both runs execute. One task, run twice, is the whole
// design: any difference between the two runs is then a difference the router
// made.
type Fixture struct {
	// ProjectID is the AO project id the dispatches carry.
	ProjectID string
	// Repo is the absolute checkout root. It is what the diff source runs git
	// in and what the code graph is keyed by.
	Repo string
	// TaskID and Objective describe the work.
	TaskID    string
	Objective string
	// IssueContext is the pre-fetched tracker context AO sends with a worker
	// spawn today, in full. It is the payload the router budgets, so it is
	// also where a routing regression shows up first.
	IssueContext string
	// RequiredFacts are the strings the fixture task cannot be completed
	// without. They live in IssueContext and nowhere in the checkout, which is
	// what makes them a fair test: an agent that is not handed them cannot go
	// and read them, exactly like a tracker decision that exists only in the
	// issue.
	RequiredFacts []string
	// VerifyCommand and VerifyArgs are the verification the run ends with.
	VerifyCommand string
	VerifyArgs    []string
}

func (f Fixture) validate() error {
	if strings.TrimSpace(f.Repo) == "" || !filepath.IsAbs(f.Repo) {
		return fmt.Errorf("%w: repo must be an absolute checkout path (got %q)", ErrFixture, f.Repo)
	}
	if strings.TrimSpace(f.Objective) == "" {
		return fmt.Errorf("%w: objective is required", ErrFixture)
	}
	if len(f.RequiredFacts) == 0 {
		return fmt.Errorf("%w: at least one required fact is needed, or the outcome gate cannot fail", ErrFixture)
	}
	for _, fact := range f.RequiredFacts {
		if strings.TrimSpace(fact) == "" {
			return fmt.Errorf("%w: a required fact must not be empty", ErrFixture)
		}
		if !strings.Contains(f.IssueContext, fact) {
			return fmt.Errorf("%w: required fact %q is not in the issue context, so the unrouted run could not carry it either", ErrFixture, fact)
		}
	}
	return nil
}

// Options configure a comparison.
type Options struct {
	// Fixture is the task to run twice.
	Fixture Fixture
	// Router is the router the enabled run installs. Nil builds the one AO
	// itself ships (contextrouter.Default), which is the configuration a
	// regression gate should be measuring; a test that wants a deliberately
	// hostile budget passes its own.
	Router *contextrouter.Router
	// Sink persists the evidence records both runs produce. Nil keeps them in
	// memory only — the records are returned either way.
	Sink projectmemory.Sink
	// Log receives the wrappers' own warnings. Nil logs nothing.
	Log *slog.Logger
}

// RunOutcome is everything one of the two runs produced.
type RunOutcome struct {
	// RouterEnabled says which of the two runs this is.
	RouterEnabled bool `json:"routerEnabled"`
	// TaskStatus, ReviewStatus and VerifyStatus are what the comparison gates
	// on.
	TaskStatus   Status `json:"taskStatus"`
	ReviewStatus Status `json:"reviewStatus"`
	VerifyStatus Status `json:"verifyStatus"`
	// MissingFacts are the required facts the dispatched context did not carry.
	// Empty is the healthy case, and it is the reason the statuses above came
	// out as they did.
	MissingFacts []string `json:"missingFacts,omitempty"`
	// ToolCalls and FileReads are what the fixture agent had to do. A routed
	// run that saves bytes by making the agent search for what it was not
	// given shows up here rather than in the size figures.
	ToolCalls int64 `json:"toolCalls"`
	FileReads int64 `json:"fileReads"`
	// Dispatches is how many agent dispatches the run made.
	Dispatches int `json:"dispatches"`
	// ContextSentBytes is the measured total AO handed the fixture agent
	// across every dispatch of this run. It is the figure the reduction
	// percentage is computed from.
	ContextSentBytes int64 `json:"contextSentBytes"`
	// PotentialBytes is the measured total those dispatches could have sent,
	// summed from each record's routing block.
	PotentialBytes int64 `json:"potentialBytes"`
	// ReusedBytes is how much of what was sent came out of AO's own indexed
	// stores rather than being read for this dispatch.
	ReusedBytes int64 `json:"reusedBytes"`
	// Records are the evidence records the run wrote, in dispatch order.
	Records []projectmemory.EvidenceRecord `json:"records"`
}

// StatusLine renders the three gated statuses in one line.
func (o RunOutcome) StatusLine() string {
	return fmt.Sprintf("task=%s review=%s verify=%s", o.TaskStatus, o.ReviewStatus, o.VerifyStatus)
}

// Comparison is the finished disabled-vs-enabled result.
type Comparison struct {
	Disabled RunOutcome `json:"disabled"`
	Enabled  RunOutcome `json:"enabled"`
	// Regressions names every gated status the two runs disagreed on. Empty
	// means the routed run reached the same conclusions as the unrouted one.
	Regressions []string `json:"regressions,omitempty"`
}

// Regressed reports whether the routed run changed an outcome. It is what a
// caller exits non-zero on, and it is deliberately independent of any measured
// saving.
func (c Comparison) Regressed() bool { return len(c.Regressions) > 0 }

// ContextReductionPercent reports how much less context the routed run sent,
// as a percentage of the unrouted run's measured total, and whether that
// figure could be computed.
//
// It reports ok=false rather than 0 when the unrouted run sent nothing
// measurable: "no reduction" and "no basis for a reduction figure" are
// different findings, and a harness that printed 0% for the second would be
// reporting a measurement it never made.
func (c Comparison) ContextReductionPercent() (float64, bool) {
	baseBytes := c.Disabled.ContextSentBytes
	if baseBytes <= 0 {
		return 0, false
	}
	return float64(baseBytes-c.Enabled.ContextSentBytes) / float64(baseBytes) * 100, true
}

// Report writes the human-readable comparison: both runs' statuses and counts,
// the measured reduction, and the verdict.
func (c Comparison) Report(w io.Writer) error {
	var b strings.Builder
	b.WriteString("context router regression harness\n")
	b.WriteString("================================\n\n")
	for _, run := range []RunOutcome{c.Disabled, c.Enabled} {
		label := "router disabled"
		if run.RouterEnabled {
			label = "router enabled "
		}
		fmt.Fprintf(&b, "%s  %s\n", label, run.StatusLine())
		fmt.Fprintf(&b, "                  dispatches=%d contextSentBytes=%d potentialBytes=%d reusedBytes=%d\n",
			run.Dispatches, run.ContextSentBytes, run.PotentialBytes, run.ReusedBytes)
		fmt.Fprintf(&b, "                  toolCalls=%d fileReads=%d\n", run.ToolCalls, run.FileReads)
		if len(run.MissingFacts) > 0 {
			fmt.Fprintf(&b, "                  missing facts: %s\n", strings.Join(run.MissingFacts, "; "))
		}
		b.WriteString("\n")
	}
	if pct, ok := c.ContextReductionPercent(); ok {
		fmt.Fprintf(&b, "measured context reduction: %.1f%% (%d -> %d bytes handed to the agent)\n",
			pct, c.Disabled.ContextSentBytes, c.Enabled.ContextSentBytes)
	} else {
		b.WriteString("measured context reduction: unavailable (the unrouted run sent no measurable payload)\n")
	}
	if c.Regressed() {
		b.WriteString("\nVERDICT: QUALITY REGRESSION — the routed run reached a different outcome.\n")
		for _, reason := range c.Regressions {
			fmt.Fprintf(&b, "  - %s\n", reason)
		}
		b.WriteString("A measured context saving does not offset this; the gate is the outcome.\n")
	} else {
		b.WriteString("\nVERDICT: no quality regression — the routed run reached the same task, review, and verify outcomes.\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// Run executes the fixture task twice — router disabled, then enabled — and
// compares the two.
//
// The disabled run goes first on purpose: it is the reference, and running it
// first means a comparison that fails to build its router at all has already
// produced the baseline rather than nothing.
func Run(ctx stdctx.Context, opts Options) (Comparison, error) {
	if err := opts.Fixture.validate(); err != nil {
		return Comparison{}, err
	}
	disabled, err := runOnce(ctx, opts, nil)
	if err != nil {
		return Comparison{}, fmt.Errorf("router-disabled run: %w", err)
	}
	router := opts.Router
	if router == nil {
		if router, err = contextrouter.Default(opts.Log); err != nil {
			return Comparison{}, fmt.Errorf("build the router AO ships: %w", err)
		}
	}
	enabled, err := runOnce(ctx, opts, router)
	if err != nil {
		return Comparison{}, fmt.Errorf("router-enabled run: %w", err)
	}
	return Comparison{Disabled: disabled, Enabled: enabled, Regressions: regressions(disabled, enabled)}, nil
}

// regressions names every gated status the two runs disagreed on.
func regressions(disabled, enabled RunOutcome) []string {
	var out []string
	for _, gate := range []struct {
		name              string
		before, afterStat Status
	}{
		{"task", disabled.TaskStatus, enabled.TaskStatus},
		{"review", disabled.ReviewStatus, enabled.ReviewStatus},
		{"verify", disabled.VerifyStatus, enabled.VerifyStatus},
	} {
		if gate.before != gate.afterStat {
			out = append(out, fmt.Sprintf("%s outcome changed: %s (router disabled) -> %s (router enabled)", gate.name, gate.before, gate.afterStat))
		}
	}
	return out
}

// runOnce executes the fixture task through the real dispatch wrapper stack.
// A nil router is the disabled run: wfrouter.Instrument then hands the
// dependencies back untouched, which is exactly what the feature flag being
// off does in the daemon.
func runOnce(ctx stdctx.Context, opts Options, router *contextrouter.Router) (RunOutcome, error) {
	capture := &captureSink{next: opts.Sink}
	recorder := projectmemory.NewRecorder(capture)
	agent := newFixtureAgent(opts.Fixture)

	deps := workflowcore.Deps{
		Projects:         agent,
		Planner:          agent,
		Spawner:          agent,
		ReviewerLauncher: agent,
		MessageSender:    agent,
		Verifier:         agent,
	}
	// The same order the daemon's composition root wires them in: the recorder
	// first, so the router wrapper sits outside it and the routing decision
	// reaches the record of the payload it produced.
	deps = wfdispatch.Instrument(deps, recorder, opts.Log)
	deps = wfrouter.Instrument(deps, router, opts.Log)

	if err := dispatchFixture(ctx, deps, opts.Fixture, agent); err != nil {
		return RunOutcome{}, err
	}

	records := capture.taken()
	out := RunOutcome{
		RouterEnabled: router != nil,
		TaskStatus:    agent.taskStatus(),
		ReviewStatus:  agent.reviewStatus(),
		VerifyStatus:  agent.verifyStatus(),
		MissingFacts:  agent.missingFacts(),
		ToolCalls:     agent.toolCalls(),
		FileReads:     agent.fileReads(),
		Dispatches:    len(records),
		Records:       records,
	}
	for _, record := range records {
		out.ContextSentBytes += metricValue(record.Context.ContextSentBytes)
		if record.Routing != nil {
			out.PotentialBytes += metricValue(record.Routing.PotentialBytes)
			out.ReusedBytes += metricValue(record.Routing.ReusedBytes)
		}
	}
	return out, nil
}

// metricValue reads a metric's count, treating an unavailable metric as
// nothing to add rather than as a zero measurement. The distinction survives in
// the records themselves, which the caller still has.
func metricValue(m projectmemory.Metric) int64 {
	if m.Value == nil {
		return 0
	}
	return *m.Value
}

// dispatchFixture drives one fixture task through the instrumented surfaces in
// the order a real run uses them: plan, work, review, fix if the review asked
// for one, verify.
func dispatchFixture(ctx stdctx.Context, deps workflowcore.Deps, fixture Fixture, agent *fixtureAgent) error {
	project := domain.ProjectRecord{ID: fixture.ProjectID, Path: fixture.Repo, DisplayName: filepath.Base(fixture.Repo)}
	plannerContext, err := agent.plannerContext(ctx, project)
	if err != nil {
		return fmt.Errorf("build planner context: %w", err)
	}
	if _, err := deps.Planner.Generate(ctx, workflowcore.PlannerRequest{
		Objective: fixture.Objective,
		Project:   project,
		Context:   plannerContext,
		MaxSteps:  workflowcore.MaxPlanSteps,
	}); err != nil {
		return fmt.Errorf("planner dispatch: %w", err)
	}

	artifact := workflowcore.BuildPlanArtifact(fixture.ProjectID, fixture.Objective, "v1")
	session, _, _, err := deps.Spawner.Spawn(ctx, agent.spawnConfig(fixture, artifact))
	if err != nil {
		return fmt.Errorf("worker dispatch: %w", err)
	}

	if _, err := deps.ReviewerLauncher.Launch(ctx, workflowcore.ReviewerLaunchRequest{
		Harness:         domain.ReviewerClaudeCode,
		WorkerSessionID: session.ID,
		ProjectID:       domain.ProjectID(fixture.ProjectID),
		RunID:           fixture.TaskID + "-review-1",
		WorkspacePath:   fixture.Repo,
		Prompt: workflowcore.BuildReviewPrompt(workflowcore.ReviewPromptInput{
			Objective:          artifact.Objective,
			AcceptanceCriteria: artifact.AcceptanceCriteria,
			WorktreePath:       fixture.Repo,
		}),
	}); err != nil {
		return fmt.Errorf("reviewer dispatch: %w", err)
	}

	if agent.reviewStatus() == StatusChangesRequested {
		if err := deps.MessageSender.Send(ctx, session.ID, workflowcore.BuildFixPrompt(workflowcore.FixPromptInput{
			Objective:          artifact.Objective,
			AcceptanceCriteria: artifact.AcceptanceCriteria,
			Findings:           agent.findings(),
			CycleNumber:        1,
		}), nil); err != nil {
			return fmt.Errorf("fix dispatch: %w", err)
		}
	}

	if _, err := deps.Verifier.Run(ctx, workflowcore.VerifyCommandRequest{
		Command:   fixture.VerifyCommand,
		Args:      fixture.VerifyArgs,
		Directory: fixture.Repo,
	}); err != nil {
		return fmt.Errorf("verify dispatch: %w", err)
	}
	return nil
}

// captureSink keeps every evidence record the run produced and, when a durable
// sink is configured, writes it there too.
//
// It validates each record even when nothing is persisting it, so a routing
// block that violated the measured/estimated labeling rule fails the harness
// rather than being quietly compared.
type captureSink struct {
	next projectmemory.Sink

	mu      sync.Mutex
	records []projectmemory.EvidenceRecord
}

func (c *captureSink) Write(ctx stdctx.Context, record projectmemory.EvidenceRecord) (string, error) {
	if err := record.Validate(); err != nil {
		return "", err
	}
	c.mu.Lock()
	c.records = append(c.records, record)
	c.mu.Unlock()
	if c.next == nil {
		return "", nil
	}
	return c.next.Write(ctx, record)
}

func (c *captureSink) taken() []projectmemory.EvidenceRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]projectmemory.EvidenceRecord, len(c.records))
	copy(out, c.records)
	return out
}
