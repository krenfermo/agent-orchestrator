package workflow_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/gitignoreprobe"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// pre_dispatch_refusal_sqlite_test.go drives the two pre-spawn refusals
// through the REAL SQLite store.
//
// Both refusals were proven only against fakeStore, which has no CHECK
// constraints. On a real database the attempt row they write was rejected by
// workflow_attempts.error_class's CHECK (fixed by migration 0171), so StartRun
// failed with a constraint error and the run was left `running` instead of
// parked on its readable stop. Found by the Frente 1 workflow-cycle E2E.

type deniedAuthPreflight struct{}

func (deniedAuthPreflight) Preflight(context.Context, workflowcore.WorkerPreflightRequest) (workflowcore.WorkerPreflightResult, error) {
	return workflowcore.WorkerPreflightResult{BinaryOK: true, AuthOK: false}, nil
}

func TestPreDispatchRefusalsParkTheRunOnRealSQLite(t *testing.T) {
	tests := []struct {
		name      string
		deps      func(repo string) workflowcore.Deps
		plan      []workflowcore.VerificationPlan
		wantClass domain.WorkflowErrorClass
	}{
		{
			name: "deliverable not observable",
			deps: func(string) workflowcore.Deps {
				return workflowcore.Deps{DeliverableIgnores: &gitignoreprobe.Probe{}}
			},
			plan: []workflowcore.VerificationPlan{{Files: []workflowcore.VerificationFileCheck{
				{Path: "src/main.go", Exists: true}, {Path: "out/report.pdf", Exists: true},
			}}},
			wantClass: workflowcore.WorkflowErrorDeliverableNotObservable,
		},
		{
			name: "provider credentials refused by preflight",
			deps: func(string) workflowcore.Deps {
				return workflowcore.Deps{WorkerPreflight: deniedAuthPreflight{}}
			},
			wantClass: workflowcore.WorkflowErrorProviderAuthRequired,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := ignoreRepo(t)
			store, _ := newRestartSimStore(t)
			ctx := context.Background()
			if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "proj-1", Path: repo, RegisteredAt: time.Now().UTC().Truncate(time.Second)}); err != nil {
				t.Fatal(err)
			}
			spawner := &fakeSpawner{}
			deps := tt.deps(repo)
			deps.Store, deps.Projects, deps.Spawner = store, store, spawner
			deps.SessionFacts, deps.WorkspaceFacts = newFakeSessionFacts(), &fakeWorkspaceFacts{}
			c := workflowcore.New(deps)

			created, err := c.CreateRun(ctx, "proj-1", "write the report", tt.plan...)
			if err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			detail, err := c.StartRun(ctx, created.Run.ID)
			if err != nil {
				t.Fatalf("StartRun failed instead of recording the refusal: %v", err)
			}
			if spawner.calls != 0 {
				t.Fatalf("spawner calls = %d, want 0", spawner.calls)
			}
			if detail.Run.State != domain.WorkflowRunNeedsAttention {
				t.Fatalf("run state = %q, want needs_attention", detail.Run.State)
			}
			attempts, err := store.ListWorkflowAttempts(ctx, workStepFrom(detail).Step.ID)
			if err != nil || len(attempts) == 0 {
				t.Fatalf("attempts = %v, err %v; want the refusal recorded", attempts, err)
			}
			if got := attempts[len(attempts)-1].ErrorClass; got != tt.wantClass {
				t.Fatalf("attempt error class = %q, want %q", got, tt.wantClass)
			}
		})
	}
}
