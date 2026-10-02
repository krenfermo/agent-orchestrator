package httpd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
	agentauth "github.com/aoagents/agent-orchestrator/backend/internal/service/agentauth"
	authsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/authsvc"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// AR-1a / D-SEC-1 end to end, on the DEFAULT desktop posture (trusted-local),
// with the real agent-credential resolver and a real database: a credential
// that is presented and fails -- expired, revoked, or never issued -- is
// answered 401 and never becomes the installation owner, while a request that
// presents no credential keeps today's trusted-local behaviour.

type tlClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *tlClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *tlClock) Add(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

func TestPresentedAgentCredentialsOnTheTrustedLocalDesktop(t *testing.T) {
	ctx := context.Background()
	st := sqlitetest.MustOpen(t)
	clock := &tlClock{now: time.Now().UTC()}
	authMgr := authsvc.New(st, clock.Now)
	agentAuth := agentauth.New(st, clock.Now, time.Hour)

	owner, err := authMgr.CreateUser(ctx, authsvc.CreateUserInput{
		DisplayName: "Ada", Email: "ada@example.com", Username: "ada",
		Password: "correct-horse-ada", Role: domain.UserRoleOwner,
	})
	if err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "proj-review", Path: "/tmp/proj-review", DisplayName: "proj-review", RegisteredAt: clock.Now()}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	issue := func(reviewRunID string) agentauth.Issued {
		issued, err := agentAuth.Issue(ctx, agentauth.IssueInput{
			Role: domain.AgentRoleReviewer, UserID: owner.ID, ProjectID: "proj-review",
			SessionID: "agent-orchestrator-59", WorkflowRunID: "run-proj-review", WorkflowStepID: "wfs-1",
			ReviewRunID: reviewRunID, RuntimeHandle: "workflow-review-" + reviewRunID, Generation: 1,
		})
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		return issued
	}
	valid := issue("rr-valid")
	revoked := issue("rr-revoked")
	if _, err := agentAuth.Revoke(ctx, revoked.Credential.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	deps := APIDeps{
		Auth:      authMgr,
		AgentAuth: agentAuth,
		Projects:  &fakeProjectManager{items: map[domain.ProjectID]projectsvc.Project{"proj-review": {ID: "proj-review", Name: "proj-review", Path: "/tmp/proj-review"}}},
	}
	cfg := config.Config{TrustedLocalMode: true, AuthMode: domain.AuthModeTrustedLocal}
	srv := httptest.NewServer(NewRouterWithControl(cfg, discardLogger(), nil, deps, ControlDeps{}))
	t.Cleanup(srv.Close)

	get := func(token string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/auth/me", nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set(identity.AgentTokenHeader, token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	if code := get(""); code != http.StatusOK {
		t.Fatalf("NO credential: /auth/me = %d, want 200 (trusted-local owner)", code)
	}
	if code := get(valid.Token); code == http.StatusUnauthorized {
		t.Fatalf("VALID credential refused with 401")
	}
	if code := get(revoked.Token); code != http.StatusUnauthorized {
		t.Fatalf("REVOKED credential: %d, want 401", code)
	}
	if code := get("ao_agent_never_issued"); code != http.StatusUnauthorized {
		t.Fatalf("NEVER-ISSUED credential: %d, want 401", code)
	}
	clock.Add(2 * time.Hour)
	if code := get(valid.Token); code != http.StatusUnauthorized {
		t.Fatalf("EXPIRED credential: %d, want 401", code)
	}
	// And the desktop owner, presenting nothing, is still exactly as before.
	if code := get(""); code != http.StatusOK {
		t.Fatalf("NO credential after the others: %d, want 200", code)
	}
}
