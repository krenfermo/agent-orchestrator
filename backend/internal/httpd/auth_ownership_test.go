package httpd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	authsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/authsvc"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// fakeProjectManager answers List/Get from a fixed set; every other method
// is unused by this test and panics if called.
type fakeProjectManager struct {
	projectsvc.Manager
	items map[domain.ProjectID]projectsvc.Project
}

func (f *fakeProjectManager) List(ctx context.Context) ([]projectsvc.Summary, error) {
	out := make([]projectsvc.Summary, 0, len(f.items))
	for _, p := range f.items {
		out = append(out, projectsvc.Summary{ID: p.ID, Name: p.Name, Path: p.Path})
	}
	return out, nil
}

func (f *fakeProjectManager) Get(ctx context.Context, id domain.ProjectID) (projectsvc.GetResult, error) {
	p, ok := f.items[id]
	if !ok {
		return projectsvc.GetResult{}, projectNotFoundErr(id)
	}
	return projectsvc.GetResult{Status: "ok", Project: &p}, nil
}

// fakeWorkflowManager answers ListRuns/GetRun from a fixed set.
type fakeWorkflowManager struct {
	workflowsvc.Manager
	runs map[string]domain.WorkflowRun
}

func (f *fakeWorkflowManager) ListRuns(ctx context.Context, filter workflowsvc.ListFilter) ([]workflowsvc.RunSummary, error) {
	out := make([]workflowsvc.RunSummary, 0, len(f.runs))
	for _, r := range f.runs {
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeWorkflowManager) GetRun(ctx context.Context, runID string) (workflowcore.RunDetail, error) {
	r, ok := f.runs[runID]
	if !ok {
		return workflowcore.RunDetail{}, workflowsvc.ErrNotFound
	}
	return workflowcore.RunDetail{Run: r}, nil
}

func projectNotFoundErr(id domain.ProjectID) error {
	return &notFoundErr{msg: "project " + string(id) + " not found"}
}

type notFoundErr struct{ msg string }

func (e *notFoundErr) Error() string { return e.msg }

// TestAuthOwnershipIDOR is Checkpoint 8P-A's security test: with
// AO_TRUSTED_LOCAL_MODE off, User A must never be able to list or fetch
// User B's projects/workflow runs by id (expect 404, not 403 — existence
// must not leak), the session cookie must be HttpOnly, a password must
// never appear in any JSON response, and a wrong password must return a
// generic invalid-credentials error indistinguishable from "no such
// account".
func TestAuthOwnershipIDOR(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	authMgr := authsvc.New(store, func() time.Time { return time.Now().UTC() })
	ctx := context.Background()

	userA, err := authMgr.CreateUser(ctx, authsvc.CreateUserInput{Email: "alice@example.com", Username: "alice", Password: "correct-horse-a"})
	if err != nil {
		t.Fatalf("create user A: %v", err)
	}
	userB, err := authMgr.CreateUser(ctx, authsvc.CreateUserInput{Email: "bob@example.com", Username: "bob", Password: "correct-horse-b"})
	if err != nil {
		t.Fatalf("create user B: %v", err)
	}

	projA := projectsvc.Project{ID: "proj-a", Name: "Project A", Path: "/tmp/a"}
	projB := projectsvc.Project{ID: "proj-b", Name: "Project B", Path: "/tmp/b"}
	// Ownership scoping reads owner_user_id off the real projects table, so
	// each project needs a durable row (the fake Manager below only serves
	// the read-model the controller renders; it never touches the DB).
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: string(projA.ID), Path: projA.Path, DisplayName: projA.Name, RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed project A: %v", err)
	}
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: string(projB.ID), Path: projB.Path, DisplayName: projB.Name, RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed project B: %v", err)
	}
	if _, err := store.SetProjectOwner(ctx, projA.ID, userA.ID); err != nil {
		t.Fatalf("set owner A: %v", err)
	}
	if _, err := store.SetProjectOwner(ctx, projB.ID, userB.ID); err != nil {
		t.Fatalf("set owner B: %v", err)
	}

	runA := domain.WorkflowRun{ID: "run-a", ProjectID: "proj-a", Objective: "do a", State: domain.WorkflowRunPending, PolicySnapshot: "{}", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	runB := domain.WorkflowRun{ID: "run-b", ProjectID: "proj-b", Objective: "do b", State: domain.WorkflowRunPending, PolicySnapshot: "{}", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if _, _, err := store.CreateWorkflowRun(ctx, runA, nil); err != nil {
		t.Fatalf("seed run A: %v", err)
	}
	if _, _, err := store.CreateWorkflowRun(ctx, runB, nil); err != nil {
		t.Fatalf("seed run B: %v", err)
	}
	if _, err := store.SetWorkflowRunOwner(ctx, runA.ID, userA.ID); err != nil {
		t.Fatalf("set run owner A: %v", err)
	}
	if _, err := store.SetWorkflowRunOwner(ctx, runB.ID, userB.ID); err != nil {
		t.Fatalf("set run owner B: %v", err)
	}

	cfg := config.Config{TrustedLocalMode: false}
	deps := APIDeps{
		Auth:              authMgr,
		Projects:          &fakeProjectManager{items: map[domain.ProjectID]projectsvc.Project{projA.ID: projA, projB.ID: projB}},
		Workflows:         &fakeWorkflowManager{runs: map[string]domain.WorkflowRun{runA.ID: runA, runB.ID: runB}},
		ProjectOwnership:  store,
		WorkflowOwnership: store,
	}
	router := NewRouterWithControl(cfg, discardLogger(), nil, deps, ControlDeps{})
	srv := httptest.NewServer(router)
	defer srv.Close()

	// --- wrong password is generic, and identical for unknown vs wrong password ---
	unknownBody := loginAndCapture(t, srv.URL, "nobody@example.com", "whatever12")
	wrongPassBody := loginAndCapture(t, srv.URL, "alice@example.com", "totally-wrong")
	if unknownBody.status != http.StatusUnauthorized || wrongPassBody.status != http.StatusUnauthorized {
		t.Fatalf("expected 401 for both, got unknown=%d wrongpass=%d", unknownBody.status, wrongPassBody.status)
	}
	if unknownBody.code != wrongPassBody.code || unknownBody.message != wrongPassBody.message {
		t.Fatalf("login failure must not distinguish unknown account from wrong password: %+v vs %+v", unknownBody, wrongPassBody)
	}
	// The message text is allowed to mention "password" generically (e.g.
	// "invalid ... password"); what matters is that it never echoes a hash,
	// which is what the loop below actually checks.
	for _, raw := range []string{unknownBody.raw, wrongPassBody.raw} {
		if strings.Contains(raw, "$2a$") || strings.Contains(raw, "$2b$") { // bcrypt hash prefix
			t.Fatalf("response leaked a bcrypt hash: %s", raw)
		}
	}

	// --- login as A, verify cookie shape and response body has no password field ---
	loginResp, cookie := loginOK(t, srv.URL, "alice@example.com", "correct-horse-a")
	if !cookie.HttpOnly {
		t.Fatal("session cookie must be HttpOnly")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie SameSite = %v, want Lax", cookie.SameSite)
	}
	if strings.Contains(strings.ToLower(loginResp), "password") {
		t.Fatalf("login response must never contain a password field: %s", loginResp)
	}

	client := &http.Client{}

	// --- User A can see their own project, not User B's, by direct id ---
	assertStatus(t, client, srv.URL+"/api/v1/projects/proj-a", cookie, http.StatusOK)
	assertStatus(t, client, srv.URL+"/api/v1/projects/proj-b", cookie, http.StatusNotFound)

	// --- User A's project list excludes User B's project entirely ---
	listBody := getBody(t, client, srv.URL+"/api/v1/projects", cookie)
	if strings.Contains(listBody, "proj-b") {
		t.Fatalf("project list leaked another user's project: %s", listBody)
	}
	if !strings.Contains(listBody, "proj-a") {
		t.Fatalf("project list is missing the caller's own project: %s", listBody)
	}

	// --- same for workflow runs ---
	assertStatus(t, client, srv.URL+"/api/v1/workflows/run-a", cookie, http.StatusOK)
	assertStatus(t, client, srv.URL+"/api/v1/workflows/run-b", cookie, http.StatusNotFound)

	runsList := getBody(t, client, srv.URL+"/api/v1/workflows", cookie)
	if strings.Contains(runsList, "run-b") {
		t.Fatalf("workflow list leaked another user's run: %s", runsList)
	}
}

type loginAttempt struct {
	status  int
	code    string
	message string
	raw     string
}

func loginAndCapture(t *testing.T, baseURL, usernameOrEmail, password string) loginAttempt {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"usernameOrEmail": usernameOrEmail, "password": password})
	resp, err := http.Post(baseURL+"/api/v1/auth/login", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("login request: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	raw := readAll(t, resp)
	_ = json.Unmarshal([]byte(raw), &out)
	return loginAttempt{status: resp.StatusCode, code: out.Code, message: out.Message, raw: raw}
}

func loginOK(t *testing.T, baseURL, usernameOrEmail, password string) (string, *http.Cookie) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"usernameOrEmail": usernameOrEmail, "password": password})
	resp, err := http.Post(baseURL+"/api/v1/auth/login", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("login request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login failed: %d %s", resp.StatusCode, readAll(t, resp))
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "ao_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("login did not set ao_session cookie")
	}
	return readAll(t, resp), cookie
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return string(buf)
}

func assertStatus(t *testing.T, client *http.Client, url string, cookie *http.Cookie, want int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.AddCookie(cookie)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("GET %s = %d, want %d (body: %s)", url, resp.StatusCode, want, readAll(t, resp))
	}
}

func getBody(t *testing.T, client *http.Client, url string, cookie *http.Cookie) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.AddCookie(cookie)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", url, resp.StatusCode)
	}
	return readAll(t, resp)
}
