package workflow_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/gitignoreprobe"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// deliverable_observability_dispatch_test.go drives the pre-dispatch
// deliverable check through the real dispatch path -- CreateRun, StartRun,
// attemptWorkHarness -- against a REAL git repository and the real
// gitignoreprobe adapter. The unit tests pin the policy; this pins that the
// policy runs BEFORE the spawn, which until now was guaranteed only by where
// the call sits in dispatch.go.

// ignoreRepo builds a repository whose .gitignore covers out/, *.ext except
// keep.ext, and a tracked.ext that is committed despite matching a rule.
func ignoreRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=ao", "GIT_AUTHOR_EMAIL=ao@example.invalid",
			"GIT_COMMITTER_NAME=ao", "GIT_COMMITTER_EMAIL=ao@example.invalid",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	write := func(rel, body string) {
		t.Helper()
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q")
	write(".gitignore", "out/\n*.ext\n!keep.ext\n")
	write("src/main.go", "package main\n")
	write("tracked.ext", "committed anyway\n")
	run("add", "-f", ".gitignore", "src/main.go", "tracked.ext")
	run("commit", "-qm", "init")
	return dir
}

type erroringIgnoreProbe struct{ calls int }

func (p *erroringIgnoreProbe) IgnoredPaths(context.Context, string, []string) ([]workflowcore.IgnoredDeliverable, error) {
	p.calls++
	return nil, errors.New("git check-ignore: exit status 128")
}

func newDeliverableCoordinator(t *testing.T, repo string, probe workflowcore.DeliverableIgnoreProbe) (*workflowcore.Coordinator, *fakeStore, *fakeSpawner) {
	t.Helper()
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: repo}}, facts: sessionFacts}
	store := newFakeStore()
	clk := &fakeClock{t: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store:              store,
		Spawner:            spawner,
		SessionFacts:       sessionFacts,
		WorkspaceFacts:     &fakeWorkspaceFacts{},
		Projects:           fakeProjects{"proj-1": {ID: "proj-1", Path: repo}},
		DeliverableIgnores: probe,
		Clock:              clk.Now,
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("id%d", idSeq)
		},
	})
	return c, store, spawner
}

func TestDeliverableObservabilityGatesTheSpawn(t *testing.T) {
	files := func(paths ...string) workflowcore.VerificationPlan {
		plan := workflowcore.VerificationPlan{}
		for _, p := range paths {
			plan.Files = append(plan.Files, workflowcore.VerificationFileCheck{Path: p, Exists: true})
		}
		return plan
	}

	tests := []struct {
		name      string
		plan      workflowcore.VerificationPlan
		wantSpawn bool
	}{
		{
			// The audited BLOCKER, end to end: src/main.go is observable, the
			// report is declared by Verification.Files and sits under out/.
			name:      "a contractual ignored deliverable beside an observable one is refused before spawn",
			plan:      files("src/main.go", "out/report.pdf"),
			wantSpawn: false,
		},
		{
			name:      "every contractual deliverable observable spawns",
			plan:      files("src/main.go", "docs/notes.md"),
			wantSpawn: true,
		},
		{
			name:      "every contractual deliverable ignored is refused before spawn",
			plan:      files("out/report.pdf", "out/summary.csv"),
			wantSpawn: false,
		},
		{
			// A must-be-absent check is not a deliverable.
			name: "an ignored path asserted ABSENT spawns",
			plan: workflowcore.VerificationPlan{Files: []workflowcore.VerificationFileCheck{
				{Path: "src/main.go", Exists: true},
				{Path: "out/stale.pdf", Exists: false},
			}},
			wantSpawn: true,
		},
		{
			// git sees changes to a tracked file whatever pattern matches it.
			name:      "a tracked file covered by an ignore pattern spawns",
			plan:      files("tracked.ext"),
			wantSpawn: true,
		},
		{
			name:      "a path re-included by a negated pattern spawns",
			plan:      files("keep.ext"),
			wantSpawn: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := ignoreRepo(t)
			c, store, spawner := newDeliverableCoordinator(t, repo, &gitignoreprobe.Probe{})
			ctx := context.Background()

			created, err := c.CreateRun(ctx, "proj-1", "update the service and write the report", tt.plan)
			if err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			detail, _ := c.StartRun(ctx, created.Run.ID)

			if tt.wantSpawn {
				if spawner.calls != 1 {
					t.Fatalf("spawner calls = %d, want 1", spawner.calls)
				}
				return
			}
			if spawner.calls != 0 {
				t.Fatalf("spawner calls = %d, want 0: the refusal must happen before spawn", spawner.calls)
			}
			attempts, err := store.ListWorkflowAttempts(ctx, workStepFrom(detail).Step.ID)
			if err != nil {
				t.Fatalf("ListWorkflowAttempts: %v", err)
			}
			if len(attempts) == 0 {
				t.Fatal("no attempt recorded for the refused dispatch")
			}
			last := attempts[len(attempts)-1]
			if last.ErrorClass != workflowcore.WorkflowErrorDeliverableNotObservable {
				t.Fatalf("attempt error class = %q, want %q", last.ErrorClass, workflowcore.WorkflowErrorDeliverableNotObservable)
			}
			// Not retried: a second pass over the run must not spawn either.
			// The run is parked on attention, so ContinueRun may itself refuse;
			// what matters is that it does not spawn.
			_, _ = c.ContinueRun(ctx, created.Run.ID)
			if spawner.calls != 0 {
				t.Fatalf("a refused dispatch was retried into a spawn (%d calls)", spawner.calls)
			}
		})
	}
}

// A probe that cannot answer is unknown, and unknown is never a refusal.
func TestDeliverableObservabilityProbeErrorStillSpawns(t *testing.T) {
	repo := ignoreRepo(t)
	probe := &erroringIgnoreProbe{}
	c, _, spawner := newDeliverableCoordinator(t, repo, probe)
	ctx := context.Background()

	plan := workflowcore.VerificationPlan{Files: []workflowcore.VerificationFileCheck{{Path: "out/report.pdf", Exists: true}}}
	created, err := c.CreateRun(ctx, "proj-1", "write the report", plan)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := c.StartRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if probe.calls == 0 {
		t.Fatal("the probe was never asked: this test would pass without the gate")
	}
	if spawner.calls != 1 {
		t.Fatalf("spawner calls = %d, want 1: an unanswerable probe must not refuse", spawner.calls)
	}
}
