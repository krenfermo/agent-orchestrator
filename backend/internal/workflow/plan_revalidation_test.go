package workflow_test

// plan_revalidation_test.go — when a plan that went stale BECAUSE IT RAN may
// clear its own staleness, and when it may not.
//
// wf-95d5bd82, the P2-A objective, reported `plan: stale_but_revalidatable`.
// The change that made it stale was its own: the manifest pins the repository
// HEAD, its task children committed on the branch during work/review/fix, and
// so every commit the plan authorized invalidated the plan authorizing it. That
// is the plan working, not a premise moving.
//
// The refusal that must survive intact is everything else. These tests pin both
// halves, because a revalidation rule that is only tested on its happy path is
// indistinguishable from `return true`.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// headMovingContext is a planner context builder whose DOCUMENTS never change —
// byte for byte the same digests — and whose repository head does. It is the
// exact drift AO's own commits produce.
type headMovingContext struct {
	head  string
	dirty bool
	docs  []workflowcore.PlannerDocument
}

// projectPath is fixed rather than read from the record so a test can vary ONE
// thing at a time; the structural case below varies it deliberately.
const revalidationProjectPath = "/repo"

func (c *headMovingContext) Build(_ context.Context, p domain.ProjectRecord) (workflowcore.PlannerContext, error) {
	return workflowcore.PlannerContext{
		Version: "v1", ProjectID: p.ID, ProjectPath: revalidationProjectPath, Branch: "feat/x",
		HeadSHA: c.head, Dirty: c.dirty, Documents: c.docs,
	}, nil
}

// recordManifest stamps the plan's recorded manifest, which is what the current
// build is compared against.
func recordManifest(t *testing.T, store *crashStore, ctx workflowcore.PlannerContext) {
	t.Helper()
	raw, err := json.Marshal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	store.mutatePlan = func(r domain.WorkflowPlanRecord) domain.WorkflowPlanRecord {
		r.ContextManifestJSON = string(raw)
		return r
	}
	t.Cleanup(func() { store.mutatePlan = nil })
}

// recordOwnHead writes the durable fact that AO produced a given commit for this
// objective — a checkpoint carrying that head_sha, which is the only evidence
// the revalidation rule accepts.
func recordOwnHead(t *testing.T, store *crashStore, runID, projectID, head string) {
	t.Helper()
	if _, err := store.CreateWorkflowCheckpoint(context.Background(), domain.WorkflowCheckpoint{
		ID: "wfc-own-head-" + head, WorkflowRunID: runID, ProjectID: projectID,
		HeadSHA: head, DurablePhase: "review_target_head_observed",
		PayloadVersion: "v1", RetryState: "{}", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

func planAssessment(t *testing.T, store *crashStore, runID string, builder workflowcore.PlannerContextBuilder) domain.PlanReusability {
	t.Helper()
	c := workflowcore.New(workflowcore.Deps{
		Store: store, Projects: store,
		Planner: &staticPlanner{plan: validMasterPlan()}, PlannerContextBuilder: builder,
	})
	assessment, err := c.AssessRecovery(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return assessment.PlanReusable
}

// The wf-95d5bd82 case: nothing the planner reads has changed, the tree is
// clean, and the head is one AO recorded as this objective's own work.
func TestPlanStaleOnlyByAOsOwnCommitIsRevalidatedAutomatically(t *testing.T) {
	store, _, runID := approvedObjective(t)
	docs := []workflowcore.PlannerDocument{{Path: "AGENTS.md", SHA256: "aaa"}}
	recordManifest(t, store, workflowcore.PlannerContext{
		Version: "v1", ProjectID: "p", ProjectPath: revalidationProjectPath, Branch: "feat/x", HeadSHA: "old-head", Documents: docs,
	})
	recordOwnHead(t, store, runID, "p", "ao-own-head")

	got := planAssessment(t, store, runID, &headMovingContext{head: "ao-own-head", docs: docs})
	if got != domain.PlanReuseExact {
		t.Fatalf("reusability = %q, want exact: the only difference is a commit AO itself recorded", got)
	}
}

// A head AO cannot recognise is somebody else's commit, and stays a person's.
func TestPlanStaleByAnUnrecognisedHeadStaysAHumanDecision(t *testing.T) {
	store, _, runID := approvedObjective(t)
	docs := []workflowcore.PlannerDocument{{Path: "AGENTS.md", SHA256: "aaa"}}
	recordManifest(t, store, workflowcore.PlannerContext{
		Version: "v1", ProjectID: "p", ProjectPath: revalidationProjectPath, Branch: "feat/x", HeadSHA: "old-head", Documents: docs,
	})
	// No checkpoint names this head.

	got := planAssessment(t, store, runID, &headMovingContext{head: "somebody-elses-head", docs: docs})
	if got != domain.PlanReuseStaleRevalidatable {
		t.Fatalf("reusability = %q, want stale_but_revalidatable: AO cannot speak for that commit", got)
	}
}

// A changed planning document IS a premise change, however the head got there.
func TestPlanStaleByAChangedPlanningDocumentStaysAHumanDecision(t *testing.T) {
	store, _, runID := approvedObjective(t)
	recordManifest(t, store, workflowcore.PlannerContext{
		Version: "v1", ProjectID: "p", ProjectPath: revalidationProjectPath, Branch: "feat/x", HeadSHA: "old-head",
		Documents: []workflowcore.PlannerDocument{{Path: "AGENTS.md", SHA256: "aaa"}},
	})
	recordOwnHead(t, store, runID, "p", "ao-own-head")

	got := planAssessment(t, store, runID, &headMovingContext{
		head: "ao-own-head",
		docs: []workflowcore.PlannerDocument{{Path: "AGENTS.md", SHA256: "CHANGED"}},
	})
	if got != domain.PlanReuseStaleRevalidatable {
		t.Fatalf("reusability = %q, want stale_but_revalidatable: the plan's own premises moved", got)
	}
}

// An added or removed planning document is equally a premise change.
func TestPlanStaleByADocumentSetChangeStaysAHumanDecision(t *testing.T) {
	store, _, runID := approvedObjective(t)
	recordManifest(t, store, workflowcore.PlannerContext{
		Version: "v1", ProjectID: "p", ProjectPath: revalidationProjectPath, Branch: "feat/x", HeadSHA: "old-head",
		Documents: []workflowcore.PlannerDocument{{Path: "AGENTS.md", SHA256: "aaa"}},
	})
	recordOwnHead(t, store, runID, "p", "ao-own-head")

	got := planAssessment(t, store, runID, &headMovingContext{
		head: "ao-own-head",
		docs: []workflowcore.PlannerDocument{
			{Path: "AGENTS.md", SHA256: "aaa"},
			{Path: "docs/architecture.md", SHA256: "bbb"},
		},
	})
	if got != domain.PlanReuseStaleRevalidatable {
		t.Fatalf("reusability = %q, want stale_but_revalidatable: a new planning document is a new premise", got)
	}
}

// An uncommitted working tree has no provenance at all: nothing durable says
// who wrote it, so it can never be attributed to AO's own authorized work.
func TestPlanStaleWithADirtyTreeStaysAHumanDecision(t *testing.T) {
	store, _, runID := approvedObjective(t)
	docs := []workflowcore.PlannerDocument{{Path: "AGENTS.md", SHA256: "aaa"}}
	recordManifest(t, store, workflowcore.PlannerContext{
		Version: "v1", ProjectID: "p", ProjectPath: revalidationProjectPath, Branch: "feat/x", HeadSHA: "old-head", Documents: docs,
	})
	recordOwnHead(t, store, runID, "p", "ao-own-head")

	got := planAssessment(t, store, runID, &headMovingContext{head: "ao-own-head", dirty: true, docs: docs})
	if got != domain.PlanReuseStaleRevalidatable {
		t.Fatalf("reusability = %q, want stale_but_revalidatable: an uncommitted tree is nobody's provable work", got)
	}
}

// A moved project path is a structural change and is never revalidated, even
// with a recognised head.
func TestPlanStaleByAStructuralChangeStaysAHumanDecision(t *testing.T) {
	store, _, runID := approvedObjective(t)
	docs := []workflowcore.PlannerDocument{{Path: "AGENTS.md", SHA256: "aaa"}}
	recordManifest(t, store, workflowcore.PlannerContext{
		Version: "v1", ProjectID: "p", ProjectPath: "/somewhere/else", Branch: "feat/x",
		HeadSHA: "old-head", Documents: docs,
	})
	recordOwnHead(t, store, runID, "p", "ao-own-head")

	got := planAssessment(t, store, runID, &headMovingContext{head: "ao-own-head", docs: docs})
	if got != domain.PlanReuseStaleRevalidatable {
		t.Fatalf("reusability = %q, want stale_but_revalidatable: the project itself moved", got)
	}
}
