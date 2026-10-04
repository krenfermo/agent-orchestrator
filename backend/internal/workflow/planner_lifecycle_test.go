package workflow_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// documentContext is the ContextBuilder shape production actually has: real
// repository documents with real bodies, each bounded by the builder's own
// per-document cap.
type documentContext struct {
	docs []workflowcore.PlannerDocument
}

func (b documentContext) Build(_ context.Context, p domain.ProjectRecord) (workflowcore.PlannerContext, error) {
	docs := make([]workflowcore.PlannerDocument, len(b.docs))
	copy(docs, b.docs)
	return workflowcore.PlannerContext{Version: "v1", ProjectID: p.ID, ProjectPath: p.Path, Documents: docs}, nil
}

// recordingPlanner captures the request and context it was handed, and can
// block until its context ends so a test can assert who is allowed to end it.
type recordingPlanner struct {
	plan     workflowcore.MasterPlan
	block    bool
	entered  chan struct{}
	gotReq   chan workflowcore.PlannerRequest
	ctxErr   chan error
	deadline chan bool
	failWith error
}

func newRecordingPlanner(plan workflowcore.MasterPlan) *recordingPlanner {
	return &recordingPlanner{
		plan:     plan,
		entered:  make(chan struct{}, 1),
		gotReq:   make(chan workflowcore.PlannerRequest, 1),
		ctxErr:   make(chan error, 1),
		deadline: make(chan bool, 1),
	}
}

func (p *recordingPlanner) Generate(ctx context.Context, req workflowcore.PlannerRequest) (workflowcore.PlannerResponse, error) {
	_, hasDeadline := ctx.Deadline()
	select {
	case p.deadline <- hasDeadline:
	default:
	}
	select {
	case p.gotReq <- req:
	default:
	}
	select {
	case p.entered <- struct{}{}:
	default:
	}
	if p.block {
		<-ctx.Done()
		p.ctxErr <- ctx.Err()
		return workflowcore.PlannerResponse{}, fmt.Errorf("planner stopped: %w: %w", ports.ErrPlannerTimeout, ctx.Err())
	}
	select {
	case p.ctxErr <- ctx.Err():
	default:
	}
	if p.failWith != nil {
		return workflowcore.PlannerResponse{}, p.failWith
	}
	return workflowcore.PlannerResponse{Plan: p.plan, Provider: "fake", Model: "fake-v1"}, nil
}

func (p *recordingPlanner) Descriptor() (string, string) { return "fake", "fake-v1" }

func newPlannerFixture(t *testing.T, planner workflowcore.Planner, builder workflowcore.PlannerContextBuilder) (*workflowcore.Coordinator, *sqlite.Store, string) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	project := domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}
	if err := store.UpsertProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	c := workflowcore.New(workflowcore.Deps{Store: store, Projects: store, Planner: planner, PlannerContextBuilder: builder})
	created, err := c.CreateObjectiveRun(ctx, "p", "Fix the autonomous runtime", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatal(err)
	}
	return c, store, created.Run.ID
}

// The wf-80dc9f12 root cause, as a test: the content-free manifest used to be
// carved out of the SAME slice the planner request carried, so every document
// reached the planner with an empty body -- both blinding the planner and
// shrinking the payload its timeout budget is computed from.
func TestGeneratePlan_PlannerKeepsDocumentBodiesTheManifestDrops(t *testing.T) {
	docs := []workflowcore.PlannerDocument{
		{Path: "AGENTS.md", SHA256: "a", Content: strings.Repeat("repository conventions\n", 40)},
		{Path: "docs/architecture.md", SHA256: "b", Content: strings.Repeat("architecture notes\n", 40)},
	}
	planner := newRecordingPlanner(validMasterPlan())
	c, store, runID := newPlannerFixture(t, planner, documentContext{docs: docs})
	if _, err := c.GeneratePlan(context.Background(), runID); err != nil {
		t.Fatal(err)
	}

	req := <-planner.gotReq
	if len(req.Context.Documents) != len(docs) {
		t.Fatalf("planner got %d documents, want %d", len(req.Context.Documents), len(docs))
	}
	for i, doc := range req.Context.Documents {
		if doc.Content != docs[i].Content {
			t.Fatalf("document %q reached the planner with %d bytes of content, want %d", doc.Path, len(doc.Content), len(docs[i].Content))
		}
	}

	plan, ok, err := store.GetWorkflowPlan(context.Background(), runID)
	if err != nil || !ok {
		t.Fatalf("plan lookup: ok=%v err=%v", ok, err)
	}
	var manifest workflowcore.PlannerContext
	if err := json.Unmarshal([]byte(plan.ContextManifestJSON), &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Documents) != len(docs) {
		t.Fatalf("manifest lists %d documents, want %d", len(manifest.Documents), len(docs))
	}
	for _, doc := range manifest.Documents {
		if doc.Content != "" {
			t.Fatalf("manifest %q stored a document body durably", doc.Path)
		}
		if doc.SHA256 == "" {
			t.Fatalf("manifest %q lost its hash", doc.Path)
		}
	}
}

// The planner context stays bounded: what the coordinator sends is exactly
// what the (already bounded) builder produced -- it neither expands it nor,
// as the aliasing bug did, empties it.
func TestGeneratePlan_PlannerContextStaysWithinTheBuildersBudget(t *testing.T) {
	docs := []workflowcore.PlannerDocument{
		{Path: "AGENTS.md", SHA256: "a", Content: strings.Repeat("x", 4096)},
		{Path: "README.md", SHA256: "b", Content: strings.Repeat("y", 2048)},
	}
	planner := newRecordingPlanner(validMasterPlan())
	c, _, runID := newPlannerFixture(t, planner, documentContext{docs: docs})
	if _, err := c.GeneratePlan(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	req := <-planner.gotReq
	total := 0
	for _, doc := range req.Context.Documents {
		total += len(doc.Content)
	}
	if want := 4096 + 2048; total != want {
		t.Fatalf("planner context carried %d bytes, want exactly the builder's %d", total, want)
	}
}

// An autonomous plan generation is entered from a wake poller cycle and a
// manual one from an HTTP handler; neither of those contexts' lifetimes may
// become the planner's.
func TestGeneratePlan_PlannerDoesNotInheritTheCallersShortLivedDeadline(t *testing.T) {
	planner := newRecordingPlanner(validMasterPlan())
	c, _, runID := newPlannerFixture(t, planner, documentContext{})

	// A transport-shaped caller: an HTTP request context, or a poller cycle,
	// whose own deadline is far shorter than a plan generation takes. It is
	// still alive here -- what must not survive into the planner is its
	// LIFETIME, not merely its cancelled state.
	caller, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := c.GeneratePlan(caller, runID); err != nil {
		t.Fatal(err)
	}
	<-planner.gotReq
	if hasDeadline := <-planner.deadline; hasDeadline {
		t.Fatal("planner inherited its caller's deadline; a long plan generation would die with the request")
	}
	if err := <-planner.ctxErr; err != nil {
		t.Fatalf("planner ran on an already-dead context: %v", err)
	}
	// The caller's cancellation must not reach it either.
	cancel()
	select {
	case err := <-planner.ctxErr:
		t.Fatalf("planner context ended with the caller: %v", err)
	default:
	}
}

func TestGeneratePlan_CancellingTheRunStopsAnInFlightPlanner(t *testing.T) {
	planner := newRecordingPlanner(validMasterPlan())
	planner.block = true
	c, _, runID := newPlannerFixture(t, planner, documentContext{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.GeneratePlan(context.Background(), runID)
	}()
	<-planner.entered

	if _, err := c.CancelRun(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-planner.ctxErr:
		if err == nil {
			t.Fatal("planner context ended without an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the run did not stop the in-flight planner")
	}
	<-done
}

func TestGeneratePlan_DaemonShutdownStopsAnInFlightPlanner(t *testing.T) {
	planner := newRecordingPlanner(validMasterPlan())
	planner.block = true
	c, _, runID := newPlannerFixture(t, planner, documentContext{})

	lifetime, shutdown := context.WithCancel(context.Background())
	c.SetExecutionLifetime(lifetime)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.GeneratePlan(context.Background(), runID)
	}()
	<-planner.entered

	shutdown()
	select {
	case err := <-planner.ctxErr:
		if err == nil {
			t.Fatal("planner context ended without an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon shutdown did not stop the in-flight planner")
	}
	<-done
}

// A planner timeout must leave behind enough to diagnose the NEXT one without
// re-running the daemon: the budget it was given, what it sent, and how it
// ended -- durably, on the same checkpoint that records the stop.
func TestGeneratePlan_TimeoutRecordsDurableAttemptEvidence(t *testing.T) {
	evidence := workflowcore.PlannerAttemptEvidence{
		CalculatedTimeoutMS: (9 * time.Minute).Milliseconds(),
		EffectiveTimeoutMS:  (9 * time.Minute).Milliseconds(),
		ObjectiveBytes:      6432,
		ContextBytes:        92000,
		PayloadBytes:        193000,
		DocumentCount:       5,
		MaxSteps:            12,
		DurationMS:          (9 * time.Minute).Milliseconds(),
		Classification:      workflowcore.PlannerAttemptTimeout,
	}
	planner := newRecordingPlanner(validMasterPlan())
	planner.failWith = &workflowcore.PlannerAttemptError{
		Evidence: evidence,
		Err:      fmt.Errorf("planner timeout: %w: context deadline exceeded", ports.ErrPlannerTimeout),
	}
	c, store, runID := newPlannerFixture(t, planner, documentContext{})
	if _, err := c.GeneratePlan(context.Background(), runID); err != nil {
		t.Fatal(err)
	}

	checkpoints, err := store.ListWorkflowCheckpoints(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var state string
	for _, cp := range checkpoints {
		if cp.DurablePhase == workflowcore.ReasonPlannerRetryScheduled {
			state = cp.RetryState
		}
	}
	if state == "" {
		t.Fatalf("no planner retry checkpoint among %d", len(checkpoints))
	}
	var recorded struct {
		Planner workflowcore.PlannerAttemptEvidence `json:"plannerAttempt"`
	}
	if err := json.Unmarshal([]byte(state), &recorded); err != nil {
		t.Fatalf("retry_state is not the evidence envelope: %v (%s)", err, state)
	}
	if recorded.Planner != evidence {
		t.Fatalf("recorded evidence=%+v want %+v", recorded.Planner, evidence)
	}
}
