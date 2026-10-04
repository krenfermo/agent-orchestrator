package httpd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
	agentauth "github.com/aoagents/agent-orchestrator/backend/internal/service/agentauth"
	authsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/authsvc"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/authz"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/rbac"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	sqlite "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// agent_identity_e2e_test.go — what an AO-launched agent may do, over real HTTP
// against a real router, a real database and real authorization.
//
// The defect: on an SSO installation a reviewer pane held no cookie and could
// hold nothing else, so `ao review submit` was answered 401 by the one route a
// reviewer exists to call. Its verdict never landed, its review_run stayed
// 'running', and thirty minutes later the run parked on review_state_ambiguous
// (wf-98ab416c).
//
// The fix must not be an exemption and must not be the operator's session, so
// what is asserted here is the shape of the middle path: the agent IS somebody,
// it acts FOR the run's owner, and it is confined to its own launch even though
// that owner may do everything everywhere.

type agentWorld struct {
	t         *testing.T
	srv       *httptest.Server
	owner     domain.User
	token     string
	store     *sqlite.Store
	agentAuth *agentauth.Service
}

func newAgentWorld(t *testing.T) *agentWorld {
	t.Helper()
	ctx := context.Background()
	st := sqlitetest.MustOpen(t)
	now := func() time.Time { return time.Now().UTC() }
	authMgr := authsvc.New(st, now)
	agentAuth := agentauth.New(st, now, time.Hour)

	owner, err := authMgr.CreateUser(ctx, authsvc.CreateUserInput{
		DisplayName: "Ada", Email: "ada@example.com", Username: "ada",
		Password: "correct-horse-ada", Role: domain.UserRoleOwner,
	})
	if err != nil {
		t.Fatalf("seed owner: %v", err)
	}

	projects := map[domain.ProjectID]projectsvc.Project{}
	runs := map[string]domain.WorkflowRun{}
	for _, id := range []domain.ProjectID{"proj-review", "proj-other"} {
		if err := st.UpsertProject(ctx, domain.ProjectRecord{
			ID: string(id), Path: "/tmp/" + string(id), DisplayName: string(id), RegisteredAt: now(),
		}); err != nil {
			t.Fatalf("seed project %s: %v", id, err)
		}
		if _, err := st.SetProjectOwner(ctx, id, owner.ID); err != nil {
			t.Fatalf("set project owner: %v", err)
		}
		projects[id] = projectsvc.Project{ID: id, Name: string(id), Path: "/tmp/" + string(id)}
		runID := "run-" + string(id)
		runs[runID] = domain.WorkflowRun{
			ID: runID, ProjectID: string(id), Objective: "work", State: domain.WorkflowRunNeedsAttention,
			PolicySnapshot: "{}", CreatedAt: now(), UpdatedAt: now(),
		}
		if _, _, err := st.CreateWorkflowRun(ctx, runs[runID], nil); err != nil {
			t.Fatalf("seed run %s: %v", runID, err)
		}
	}

	// The credential AO mints when it launches the reviewer for run-proj-review.
	issued, err := agentAuth.Issue(ctx, agentauth.IssueInput{
		Role:           domain.AgentRoleReviewer,
		UserID:         owner.ID,
		ProjectID:      "proj-review",
		SessionID:      "agent-orchestrator-59",
		WorkflowRunID:  "run-proj-review",
		WorkflowStepID: "wfs-04b67615",
		ReviewRunID:    "bf660d26",
		RuntimeHandle:  "workflow-review-bf660d26",
		Generation:     1,
	})
	if err != nil {
		t.Fatalf("issue agent credential: %v", err)
	}

	deps := APIDeps{
		Auth:              authMgr,
		AgentAuth:         agentAuth,
		Projects:          &fakeProjectManager{items: projects},
		Workflows:         &fakeWorkflowManager{runs: runs},
		ProjectOwnership:  st,
		WorkflowOwnership: st,
		SessionOwnership:  st,
		Authz:             authz.New(st),
		ProjectScope:      st,
		RBAC:              rbac.New(st, nil, rbac.NoopAudit{}, nil),
	}
	cfg := config.Config{TrustedLocalMode: false, AuthMode: domain.AuthModeOIDC}
	srv := httptest.NewServer(NewRouterWithControl(cfg, discardLogger(), nil, deps, ControlDeps{}))
	t.Cleanup(srv.Close)
	return &agentWorld{t: t, srv: srv, owner: owner, token: issued.Token, store: st, agentAuth: agentAuth}
}

// get issues a GET, optionally presenting the agent credential.
func (w *agentWorld) get(path string, asAgent bool) int {
	w.t.Helper()
	req, err := http.NewRequest(http.MethodGet, w.srv.URL+path, nil)
	if err != nil {
		w.t.Fatalf("build request: %v", err)
	}
	if asAgent {
		req.Header.Set(identity.AgentTokenHeader, w.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// A cookie-less agent is still refused — the fix is not an exemption.
func TestAgentWithoutACredentialIsStillRefused(t *testing.T) {
	w := newAgentWorld(t)
	if code := w.get("/api/v1/workflows/run-proj-review", false); code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a request with no identity at all", code)
	}
}

// With its own credential the agent reaches the run it was launched for. This is
// the request that used to be answered 401 while the reviewer waited.
func TestAgentReachesItsOwnRun(t *testing.T) {
	w := newAgentWorld(t)
	if code := w.get("/api/v1/workflows/run-proj-review", true); code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for the agent's own run", code)
	}
}

// And nothing else. The account behind the credential is the installation
// OWNER, who may read every run in every project and manage every account --
// so every refusal below is the credential's ceiling doing the work, not the
// account's.
func TestAgentActingForAnOwnerIsConfinedToItsLaunch(t *testing.T) {
	w := newAgentWorld(t)

	// A run in another project: 404, never 403 — a run id the caller may not
	// reach must be indistinguishable from one that does not exist.
	if code := w.get("/api/v1/workflows/run-proj-other", true); code != http.StatusNotFound {
		t.Fatalf("foreign run status = %d, want 404", code)
	}
	// Installation-wide surfaces are refused outright, and refused as 403:
	// there is no resource whose existence could leak, and the agent IS
	// authenticated -- it simply may not do this.
	for _, path := range []string{"/api/v1/users", "/api/v1/teams"} {
		if code := w.get(path, true); code != http.StatusForbidden {
			t.Fatalf("GET %s = %d, want 403", path, code)
		}
	}
	// A project it was not launched for is invisible, not merely forbidden.
	if code := w.get("/api/v1/projects/proj-other", true); code != http.StatusNotFound {
		t.Fatalf("foreign project status = %d, want 404", code)
	}
}

// A revoked credential stops working immediately — the same instant its review
// run is closed out, not at its expiry.
func TestRevokedAgentCredentialStopsWorking(t *testing.T) {
	ctx := context.Background()
	st := sqlitetest.MustOpen(t)
	now := func() time.Time { return time.Now().UTC() }
	agentAuth := agentauth.New(st, now, time.Hour)
	authMgr := authsvc.New(st, now)
	owner, err := authMgr.CreateUser(ctx, authsvc.CreateUserInput{
		DisplayName: "Ada", Email: "ada@example.com", Username: "ada",
		Password: "correct-horse-ada", Role: domain.UserRoleOwner,
	})
	if err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	issued, err := agentAuth.Issue(ctx, agentauth.IssueInput{
		Role: domain.AgentRoleReviewer, UserID: owner.ID, ProjectID: "proj-review",
		SessionID: "agent-orchestrator-59", ReviewRunID: "bf660d26",
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := agentAuth.ResolveAgentPrincipal(ctx, issued.Token); err != nil {
		t.Fatalf("a fresh credential did not resolve: %v", err)
	}
	if _, err := agentAuth.RevokeForReviewRun(ctx, "bf660d26"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := agentAuth.ResolveAgentPrincipal(ctx, issued.Token); err == nil {
		t.Fatalf("a revoked credential still resolved")
	}
	// But the LAUNCH RECORD survives, because the ambiguous-review recovery
	// reads it as evidence that this reviewer was once able to answer.
	ever, err := agentAuth.EverIssuedForReviewRun(ctx, "bf660d26")
	if err != nil || !ever {
		t.Fatalf("EverIssued after revocation = %v, %v; want true, nil", ever, err)
	}
}

// seedRunningReviewRun makes the review run the world's credential is bound to
// durable, in the state a reviewer that is still working leaves it in.
func (w *agentWorld) seedRunningReviewRun() {
	w.t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	rec, err := w.store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "proj-review", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode,
		Activity:  domain.Activity{State: domain.ActivityActive, LastActivityAt: now},
		Metadata:  domain.SessionMetadata{Branch: "feat/x", WorkspacePath: "/ws"},
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		w.t.Fatalf("seed session: %v", err)
	}
	if err := w.store.UpsertReview(ctx, domain.Review{
		ID: "rev-1", SessionID: rec.ID, ProjectID: rec.ProjectID,
		Harness: domain.ReviewerClaudeCode, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		w.t.Fatalf("seed review: %v", err)
	}
	if err := w.store.InsertReviewRun(ctx, domain.ReviewRun{
		ID: "bf660d26", ReviewID: "rev-1", SessionID: rec.ID, Harness: domain.ReviewerClaudeCode,
		PRURL: "https://example/pr/1", TargetSHA: "sha1",
		Status: domain.ReviewRunRunning, Verdict: domain.VerdictNone, CreatedAt: now,
	}); err != nil {
		w.t.Fatalf("seed review run: %v", err)
	}
}

// The whole point of revocation, over real HTTP: a reviewer that has finished
// can no longer reach the API, and one that has not is untouched.
//
// This is the regression in its user-visible form. Before the fix nothing on
// the success path ever revoked, so the credential of a reviewer that had
// approved and exited still opened its run for the rest of its TTL -- which is
// the state agc-dfef17e6 and agc-f1850962 were found in.
func TestAFinishedReviewersCredentialStopsReachingTheAPI(t *testing.T) {
	w := newAgentWorld(t)
	w.seedRunningReviewRun()
	ctx := context.Background()
	reaper := agentauth.NewReconciler(w.agentAuth, agentauth.ReconcilerConfig{DataDir: t.TempDir()})

	// While the review is running the sweep must leave the reviewer alone: a
	// credential taken away here is a verdict that can never be recorded.
	if _, err := reaper.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce while running: %v", err)
	}
	if code := w.get("/api/v1/workflows/run-proj-review", true); code != http.StatusOK {
		t.Fatalf("a working reviewer was locked out: status = %d, want 200", code)
	}

	// The verdict lands and the run leaves `running`. That, and nothing else,
	// is the end of the reviewer's authority.
	if ok, err := w.store.UpdateReviewRunResult(ctx, "bf660d26",
		domain.ReviewRunComplete, domain.VerdictApproved, "", "", false); err != nil || !ok {
		t.Fatalf("record the verdict: %v (ok=%v)", err, ok)
	}
	if err := reaper.CloseReviewRun(ctx, "bf660d26"); err != nil {
		t.Fatalf("CloseReviewRun: %v", err)
	}

	if code := w.get("/api/v1/workflows/run-proj-review", true); code != http.StatusUnauthorized {
		t.Fatalf("a finished reviewer still reached its run: status = %d, want 401", code)
	}
	// And the launch record survives the revocation, because the
	// ambiguous-review recovery reads it as evidence.
	if ever, err := w.agentAuth.EverIssuedForReviewRun(ctx, "bf660d26"); err != nil || !ever {
		t.Fatalf("EverIssued after revocation = %v, %v; want true, nil", ever, err)
	}
}
