// Command aobaseline records Phase 0 project-memory baseline evidence: for
// each of several representative AO agent tasks it runs the CURRENT,
// unmodified context-assembly pipeline over a real repository and writes one
// structured evidence file per task under AO's data dir.
//
// It changes nothing about the pipeline. Every prompt and context document it
// measures is produced by the same exported builders the daemon's own
// dispatchers call (workflow.BuildWorkStepPromptWithSpec, BuildReviewPrompt,
// BuildFixPrompt, and the planner's ContextBuilder), and the planner record is
// assembled by the very function the live planner decorator uses, so a
// baseline file and a live-run file have the same shape.
//
// What it deliberately does NOT do is call a provider. The supply side (files
// inspected, source reachable, context assembled and sent) is measured here;
// the consumption side (prompt/output/cached tokens, the agent's own tool
// calls) is recorded as unavailable, with the reason stated in the file. Those
// numbers come from a live run with AO_PROJECT_MEMORY_BASELINE=1 set on the
// daemon, which wraps the same dispatch surfaces in the same recorder.
//
// Usage:
//
//	go run ./cmd/aobaseline [-repo PATH] [-evidence-dir PATH] [-run-id ID]
//
// See docs/project-memory-baseline.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	plannercommand "github.com/aoagents/agent-orchestrator/backend/internal/adapters/planner/command"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory/wfdispatch"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// harnessUnavailableReason is stamped on every metric this harness cannot
// obtain. It names the actual limitation rather than borrowing the live
// dispatch surfaces' wording, so a reader of a baseline file is never left
// thinking a provider was called and reported nothing.
const harnessUnavailableReason = "the baseline harness measures the context AO assembles and dispatches no provider call, so no agent-side signal (provider telemetry, tool calls, the agent's own file reads) exists for this record"

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "aobaseline:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("aobaseline", flag.ContinueOnError)
	fs.SetOutput(out)
	repo := fs.String("repo", "", "repository to measure (default: the git repository containing the working directory)")
	evidenceDir := fs.String("evidence-dir", "", "where evidence is written (default: <AO_DATA_DIR>/project-memory/baseline)")
	runID := fs.String("run-id", "", "identifier this baseline's records are filed under (default: baseline-<timestamp>)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	repoPath, err := resolveRepo(*repo)
	if err != nil {
		return err
	}
	dir := *evidenceDir
	if dir == "" {
		if dir, err = projectmemory.EvidenceDir(); err != nil {
			return err
		}
	}
	sink, err := projectmemory.NewDirSink(dir)
	if err != nil {
		return err
	}
	id := *runID
	if id == "" {
		id = "baseline-" + time.Now().UTC().Format("20060102T150405Z")
	}

	recorder := projectmemory.NewRecorder(sink)
	// The summary is progress reporting, not the deliverable: a closed pipe on
	// stdout must not fail a baseline whose evidence files are already written.
	report := func(format string, args ...any) { _, _ = fmt.Fprintf(out, format, args...) }
	report("repository: %s\nevidence:   %s\nrun id:     %s\n\n", repoPath, sink.Root(), id)

	ctx := context.Background()
	var failures int
	for _, task := range tasks() {
		path, err := task.record(ctx, recorder, repoPath, id)
		if err != nil {
			failures++
			report("  %-10s FAILED  %v\n", task.id, err)
			continue
		}
		report("  %-10s %s\n", task.id, path)
	}
	if failures > 0 {
		return fmt.Errorf("%d of %d baseline tasks failed", failures, len(tasks()))
	}
	report("\n%d baseline evidence records written.\n", len(tasks()))
	return nil
}

// resolveRepo picks the repository to measure: an explicit path, or the git
// repository containing the working directory. It refuses to guess.
func resolveRepo(explicit string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return "", fmt.Errorf("resolve -repo: %w", err)
		}
		if _, err := os.Stat(abs); err != nil {
			return "", fmt.Errorf("resolve -repo: %w", err)
		}
		return abs, nil
	}
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	raw, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("no -repo given and the working directory is not a git repository: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// task is one representative agent task the baseline exercises.
type task struct {
	// id names the task and becomes its taskId in the evidence record.
	id string
	// role is the agent role this task stands in for.
	role domain.WorkflowRole
	// scope is the source the role's work would reach, relative to the repo.
	// It is what "source tokens available" is measured over.
	scope []string
	// observable declares what the harness can report for this task, mirroring
	// the capabilities the live decorator for the same role declares.
	observable projectmemory.Capabilities
	// exercise runs the real, unmodified assembly path and records it.
	exercise func(ctx context.Context, span *projectmemory.Span, repo string) error
}

// tasks are the representative tasks the Phase 0 baseline covers: the four
// roles whose context AO itself assembles. Verify is absent because it
// assembles no context at all -- it runs a command -- and a record of it would
// be a row of "not applicable" rather than a baseline.
func tasks() []task {
	return []task{
		{
			id:         "planner-context",
			role:       domain.WorkflowRolePlanner,
			scope:      []string{"."},
			observable: projectmemory.Capabilities{FileReads: true, ContextPayload: true, SourceScope: true},
			exercise:   exercisePlanner,
		},
		{
			id:         "worker-prompt",
			role:       domain.WorkflowRoleWorker,
			scope:      []string{filepath.Join("backend", "internal", "workflow")},
			observable: projectmemory.Capabilities{ContextPayload: true, SourceScope: true},
			exercise:   exerciseWorker,
		},
		{
			id:         "reviewer-prompt",
			role:       domain.WorkflowRoleReviewer,
			scope:      []string{filepath.Join("backend", "internal", "observe")},
			observable: projectmemory.Capabilities{ContextPayload: true, SourceScope: true},
			exercise:   exerciseReviewer,
		},
		{
			id:         "fix-prompt",
			role:       domain.WorkflowRoleFixWorker,
			scope:      []string{filepath.Join("backend", "internal", "cli")},
			observable: projectmemory.Capabilities{ContextPayload: true, SourceScope: true},
			exercise:   exerciseFix,
		},
	}
}

// record opens a span for the task, runs the real assembly path, measures the
// task's source scope, and writes the evidence file.
func (t task) record(ctx context.Context, recorder *projectmemory.Recorder, repo, runID string) (string, error) {
	span := recorder.Begin(projectmemory.Dispatch{
		Role:              t.role,
		WorkflowRunID:     runID,
		TaskID:            t.id,
		ProjectID:         filepath.Base(repo),
		Observable:        t.observable,
		UnavailableReason: harnessUnavailableReason,
	})
	span.ObserveSourceScope(projectmemory.ScanSource(repo, t.scope))
	span.Note("baseline harness record: the context measured here was produced by the unmodified builders the daemon's own dispatchers call, over this harness's fixed objective (and, for the fix role, a placeholder findings body) rather than a live run's real inputs -- the sizes characterise the builders, not any one task")
	exerciseErr := t.exercise(ctx, span, repo)
	_, path, writeErr := span.Finish(ctx, exerciseErr)
	if writeErr != nil {
		return "", writeErr
	}
	if exerciseErr != nil {
		return path, exerciseErr
	}
	return path, nil
}

// baselineObjective is the objective every prompt-building task is measured
// against. One shared, realistic objective keeps the four records comparable:
// a difference between them is then a difference between the roles' context,
// not between the tasks they were asked to do.
const baselineObjective = "Add a provider-agnostic instrumentation layer that records per-run context and token metrics for AO agent dispatch."

func exercisePlanner(ctx context.Context, span *projectmemory.Span, repo string) error {
	project := domain.ProjectRecord{ID: filepath.Base(repo), Path: repo, DisplayName: filepath.Base(repo)}
	plannerContext, err := plannercommand.ContextBuilder{}.Build(ctx, project)
	if err != nil {
		return fmt.Errorf("build planner context: %w", err)
	}
	wfdispatch.RecordPlannerContext(span, workflowcore.PlannerRequest{
		Objective: baselineObjective,
		Project:   project,
		Context:   plannerContext,
		MaxSteps:  workflowcore.MaxPlanSteps,
	})
	return nil
}

func exerciseWorker(_ context.Context, span *projectmemory.Span, repo string) error {
	artifact := workflowcore.BuildPlanArtifact(filepath.Base(repo), baselineObjective, "v1")
	span.ObserveContextSent(workflowcore.BuildWorkStepPromptWithSpec(artifact, ""))
	return nil
}

func exerciseReviewer(_ context.Context, span *projectmemory.Span, repo string) error {
	artifact := workflowcore.BuildPlanArtifact(filepath.Base(repo), baselineObjective, "v1")
	span.ObserveContextSent(workflowcore.BuildReviewPrompt(workflowcore.ReviewPromptInput{
		Objective:          artifact.Objective,
		AcceptanceCriteria: artifact.AcceptanceCriteria,
		WorktreePath:       repo,
	}))
	return nil
}

func exerciseFix(_ context.Context, span *projectmemory.Span, repo string) error {
	artifact := workflowcore.BuildPlanArtifact(filepath.Base(repo), baselineObjective, "v1")
	span.ObserveContextSent(workflowcore.BuildFixPrompt(workflowcore.FixPromptInput{
		Objective:          artifact.Objective,
		AcceptanceCriteria: artifact.AcceptanceCriteria,
		Findings:           "Baseline harness placeholder: no review findings exist for a measurement run.",
		CycleNumber:        1,
	}))
	return nil
}
