package workflow

import (
	stdctx "context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// task_knowledge_internal_test.go — the workflow half of P2-C.
//
// Three decisions live in this package and nowhere else, and each is tested
// here because getting any of them wrong is a safety bug rather than a quality
// one:
//
//   - How far a finished task's knowledge may travel.
//   - Which run scopes workflow-local sharing.
//   - Which authority may promote an isolated task's knowledge to canonical,
//     and at which instant.

// fakeTaskMemory records the calls the coordinator makes, so a test can assert
// on WHICH authority acted rather than on a side effect two paths share.
type fakeTaskMemory struct {
	promoted    []promotedCall
	discarded   []string
	recorded    []TaskOutcomeFacts
	invalidated int
}

type promotedCall struct {
	taskRef string
	commit  string
}

func (f *fakeTaskMemory) InvalidatePaths(stdctx.Context, domain.ProjectID, string, []string, string) (int64, error) {
	f.invalidated++
	return 0, nil
}

func (f *fakeTaskMemory) RecordOutcome(_ stdctx.Context, out TaskOutcomeFacts) error {
	f.recorded = append(f.recorded, out)
	return nil
}

func (f *fakeTaskMemory) PromoteTask(_ stdctx.Context, _ domain.ProjectID, taskRef, commit string) (int, error) {
	f.promoted = append(f.promoted, promotedCall{taskRef: taskRef, commit: commit})
	return 1, nil
}

func (f *fakeTaskMemory) DiscardTask(_ stdctx.Context, _ domain.ProjectID, taskRef string) (int64, error) {
	f.discarded = append(f.discarded, taskRef)
	return 0, nil
}

// TestKnowledgeShareForFollowsVerificationAndIntegration pins the sharing
// policy table. Integration is the only route to canonical; verification is
// the only route out of task-local.
func TestKnowledgeShareForFollowsVerificationAndIntegration(t *testing.T) {
	for _, tc := range []struct {
		name       string
		state      domain.WorkflowStepState
		integrated bool
		want       domain.KnowledgeShare
	}{
		{"integrated work is the project's", domain.WorkflowStepCompleted, true, domain.ShareCanonical},
		{"integrated even from an odd step state", domain.WorkflowStepRunning, true, domain.ShareCanonical},
		{"verified but unintegrated reaches its dependents", domain.WorkflowStepCompleted, false, domain.ShareWorkflow},
		{"unverified reaches nobody", domain.WorkflowStepRunning, false, domain.ShareTask},
		{"failed reaches nobody", domain.WorkflowStepFailed, false, domain.ShareTask},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := knowledgeShareFor(domain.WorkflowStep{State: tc.state}, tc.integrated)
			if got != tc.want {
				t.Fatalf("share = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestKnowledgeRunIDScopesToTheParentWorkflow: a child run is one task's
// execution. Scoping workflow-local sharing to the child would make every task
// its own workflow and share nothing — the feature present and inert.
func TestKnowledgeRunIDScopesToTheParentWorkflow(t *testing.T) {
	parent := "wf-parent"
	child := domain.WorkflowRun{ID: "wf-child", ParentWorkflowID: &parent}
	if got := knowledgeRunIDFor(child); got != parent {
		t.Errorf("a child run scopes sharing to %q, want the parent %q", got, parent)
	}
	solo := domain.WorkflowRun{ID: "wf-solo"}
	if got := knowledgeRunIDFor(solo); got != "wf-solo" {
		t.Errorf("a run with no parent scopes sharing to %q, want itself", got)
	}
	empty := ""
	blankParent := domain.WorkflowRun{ID: "wf-child", ParentWorkflowID: &empty}
	if got := knowledgeRunIDFor(blankParent); got != "wf-child" {
		t.Errorf("a blank parent id scoped sharing to %q, want the run itself", got)
	}
}

// TestTaskAuthorityFailsTowardLessKnowledge is the safety property: no failure
// of authority assembly may widen what a dispatch sees.
func TestTaskAuthorityFailsTowardLessKnowledge(t *testing.T) {
	c := &Coordinator{}
	// No plan store, no planned task: the authority names the run and nothing
	// it may read.
	auth := c.taskAuthorityFor(stdctx.Background(), domain.WorkflowRun{ID: "wf-1"})
	if len(auth.UpstreamTaskRefs) != 0 {
		t.Fatalf("a coordinator with no plan store invented %d upstream tasks", len(auth.UpstreamTaskRefs))
	}
	if auth.WorkflowRunID != "wf-1" || auth.TaskRef != "wf-1" {
		t.Fatalf("authority = %+v, want the run named as both run and task ref", auth)
	}
}

// TestTaskAuthorityNeverNamesItself guards the one self-reference that would
// make a task its own upstream and defeat the dependency check.
func TestTaskAuthorityNeverNamesItself(t *testing.T) {
	auth := (&Coordinator{}).taskAuthorityFor(stdctx.Background(), domain.WorkflowRun{ID: "wf-1"})
	for _, ref := range auth.UpstreamTaskRefs {
		if ref == auth.TaskRef {
			t.Fatal("a task named itself as an upstream dependency")
		}
	}
}

// TestPromoteIntegratedTaskMemoryRequiresProof is P2-C §4 at the boundary: no
// integrated SHA, no promotion — which is what keeps an isolated worktree that
// never lands from reaching canonical memory.
func TestPromoteIntegratedTaskMemoryRequiresProof(t *testing.T) {
	mem := &fakeTaskMemory{}
	c := &Coordinator{taskMemory: mem, projects: nil}
	run := domain.WorkflowRun{ID: "wf-1", ProjectID: "proj-1"}

	c.promoteIntegratedTaskMemory(stdctx.Background(), run, "task-1", "")
	if len(mem.promoted) != 0 {
		t.Fatal("memory was promoted with no integrated SHA to prove the work landed")
	}
	c.promoteIntegratedTaskMemory(stdctx.Background(), run, "", "sha-1")
	if len(mem.promoted) != 0 {
		t.Fatal("memory was promoted for an unnamed task")
	}

	// And with memory switched off, nothing happens and nothing panics.
	(&Coordinator{}).promoteIntegratedTaskMemory(stdctx.Background(), run, "task-1", "sha-1")
}

// TestFinishTaskWorktreePromotesBeforeCleanup pins the ORDERING that makes the
// promotion survivable.
//
// Cleanup is best-effort and may fail and leave. If promotion ran after it, a
// failed housekeeping step would silently keep integrated knowledge task-local
// — memory that is wrong in a way nothing later corrects.
func TestFinishTaskWorktreePromotesBeforeCleanup(t *testing.T) {
	mem := &fakeTaskMemory{}
	c := &Coordinator{
		taskMemory: mem,
		projects:   fakeProjectsAt("proj-1", t.TempDir()),
		// taskWorkspaces deliberately nil: the cleanup half cannot run at all,
		// which is the strongest form of "cleanup did not happen".
	}
	parent := domain.WorkflowRun{ID: "wf-1", ProjectID: "proj-1"}
	c.finishTaskWorktree(stdctx.Background(), parent, domain.WorkflowTask{ID: "task-1"}, "sha-1")

	if len(mem.promoted) != 1 {
		t.Fatalf("%d promotions, want 1: promotion must not depend on cleanup succeeding", len(mem.promoted))
	}
	if mem.promoted[0].taskRef != "task-1" || mem.promoted[0].commit != "sha-1" {
		t.Fatalf("promoted %+v, want task-1 at sha-1", mem.promoted[0])
	}
}

// TestFinishTaskWorktreeIsIdempotent covers the duplicate completion callback
// and the restart mid-cleanup: both re-enter this path, and both must leave
// exactly one canonical promotion. The idempotency is PromoteTaskMemory's, and
// this test pins that the coordinator does not defeat it by, say, promoting
// under a different task ref the second time.
func TestFinishTaskWorktreeIsIdempotent(t *testing.T) {
	mem := &fakeTaskMemory{}
	c := &Coordinator{taskMemory: mem, projects: fakeProjectsAt("proj-1", t.TempDir())}
	parent := domain.WorkflowRun{ID: "wf-1", ProjectID: "proj-1"}
	task := domain.WorkflowTask{ID: "task-1"}

	c.finishTaskWorktree(stdctx.Background(), parent, task, "sha-1")
	c.finishTaskWorktree(stdctx.Background(), parent, task, "sha-1")

	if len(mem.promoted) != 2 {
		t.Fatalf("%d promotion calls, want 2 — the coordinator must re-enter idempotently rather than guard", len(mem.promoted))
	}
	for _, p := range mem.promoted {
		if p.taskRef != "task-1" || p.commit != "sha-1" {
			t.Fatalf("a repeat promotion addressed %+v, want the same task and SHA every time", p)
		}
	}
}

// fakeProjectsAt is the smallest Projects that resolves one project root.
func fakeProjectsAt(id, path string) Projects { return staticProjects{id: id, path: path} }

type staticProjects struct{ id, path string }

func (p staticProjects) GetProject(_ stdctx.Context, id string) (domain.ProjectRecord, bool, error) {
	if id != p.id {
		return domain.ProjectRecord{}, false, nil
	}
	return domain.ProjectRecord{ID: p.id, Path: p.path}, true, nil
}
