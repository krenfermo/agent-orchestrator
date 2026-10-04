package controllers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/authz"
)

// work_items_test.go — the route table's authorization, and what the wire
// carries (P4-E §11, §17).
//
// The permission each route demands is a claim about who may do what, and it is
// exactly the kind of claim that changes by accident when somebody moves a
// route. It is pinned here as a table rather than asserted in prose.

// recordingAuthz is a controllers.Authorizer that records what each route
// asked for and answers however the test tells it to.
//
// It is a real Guard rather than a substituted one because Guard is a concrete
// struct: the tests below therefore exercise the ACTUAL denial rendering — the
// 404-not-403 masking included — rather than a stand-in that could disagree
// with it.
type recordingAuthz struct {
	allow bool
	asked []domain.Permission
}

func (a *recordingAuthz) Authorize(
	_ context.Context, _ domain.Principal, perm domain.Permission, _ domain.AuthzResource,
) error {
	// The guard re-asks for project.read to decide whether to mask a denial as
	// 404. That second question is not the route's own demand, so it is not
	// recorded.
	if perm == domain.PermProjectRead {
		if a.allow {
			return nil
		}
		return apierr.Forbidden("FORBIDDEN", "denied")
	}
	a.asked = append(a.asked, perm)
	if a.allow {
		return nil
	}
	return apierr.Forbidden("FORBIDDEN", "denied")
}

func (a *recordingAuthz) Resolve(context.Context, domain.Principal) (authz.Subject, error) {
	return authz.Subject{}, nil
}

func (a *recordingAuthz) InstallationUnclaimed(context.Context) (bool, error) { return false, nil }

// recordingGuard bundles the recorder with a real Guard.
type recordingGuard struct {
	authz *recordingAuthz
	guard controllers.Guard
}

func newGuard(allow bool) *recordingGuard {
	a := &recordingAuthz{allow: allow}
	return &recordingGuard{authz: a, guard: controllers.Guard{Authz: a}}
}

func (g *recordingGuard) asked() []domain.Permission { return g.authz.asked }

// stubWorkItems answers every route with a fixed value, so the tests here can
// be about routing and authorization rather than about behaviour the service
// tests already cover.
type stubWorkItems struct {
	config controllers.WorkItemsConfigResponse
	links  controllers.WorkItemLinksResponse
	linked controllers.WorkItemLinkResponse
	health controllers.WorkItemsHealthResponse

	putUpdate  *controllers.WorkItemsConfigUpdate
	linkReq    *controllers.WorkItemLinkRequest
	syncCalled bool
}

func (s *stubWorkItems) WorkItemsConfig(context.Context, domain.ProjectID) (controllers.WorkItemsConfigResponse, error) {
	return s.config, nil
}

func (s *stubWorkItems) PutWorkItemsConfig(
	_ context.Context, _ domain.ProjectID, req controllers.WorkItemsConfigUpdate,
) (controllers.WorkItemsConfigResponse, error) {
	s.putUpdate = &req
	return s.config, nil
}

func (s *stubWorkItems) DeleteWorkItemsConfig(context.Context, domain.ProjectID) error { return nil }

func (s *stubWorkItems) TestWorkItemsConnection(context.Context, domain.ProjectID) (controllers.WorkItemsConnectionResponse, error) {
	return controllers.WorkItemsConnectionResponse{Provider: "plane", Workspace: "acme", Projects: 2}, nil
}

func (s *stubWorkItems) ListWorkItemsProviderProjects(context.Context, domain.ProjectID) (controllers.WorkItemsProviderProjectsResponse, error) {
	return controllers.WorkItemsProviderProjectsResponse{}, nil
}

func (s *stubWorkItems) ListWorkItemLinks(context.Context, domain.ProjectID, bool) (controllers.WorkItemLinksResponse, error) {
	return s.links, nil
}

func (s *stubWorkItems) CreateWorkItemLink(
	_ context.Context, req controllers.WorkItemLinkRequest,
) (controllers.WorkItemLinkResponse, error) {
	s.linkReq = &req
	return s.linked, nil
}

func (s *stubWorkItems) DeleteWorkItemLink(context.Context, domain.ProjectID, string) error {
	return nil
}

func (s *stubWorkItems) SetWorkItemLinkSync(context.Context, domain.ProjectID, string, bool) error {
	return nil
}

func (s *stubWorkItems) WorkItemsHealth(context.Context, domain.ProjectID) (controllers.WorkItemsHealthResponse, error) {
	return s.health, nil
}

func (s *stubWorkItems) SyncWorkItems(context.Context, domain.ProjectID) (controllers.WorkItemsSyncResponse, error) {
	s.syncCalled = true
	return controllers.WorkItemsSyncResponse{Claimed: 1, Delivered: 1}, nil
}

func (s *stubWorkItems) ListWorkItemsAudit(context.Context, domain.ProjectID, int) (controllers.WorkItemsAuditResponse, error) {
	return controllers.WorkItemsAuditResponse{}, nil
}

func workItemsRouter(svc controllers.WorkItemsService, guard *recordingGuard) http.Handler {
	r := chi.NewRouter()
	(&controllers.WorkItemsController{Svc: svc, Guard: guard.guard}).Register(r)
	return r
}

// authed attaches a principal, because a guard with authorization ON answers
// 401 to a request that carries none — which would mask the permission
// question these tests are about.
func authed(r *http.Request) *http.Request {
	return r.WithContext(identity.WithPrincipal(r.Context(),
		domain.Principal{User: domain.User{ID: "u1", Email: "ana@example.test"}}))
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := authed(httptest.NewRequest(method, path, reader))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The route table. Each row says who may do this, and the split between read,
// link and manage is the point: doing the work and owning the connection are
// different authorities.
func TestWorkItemRoutesDemandTheRightPermission(t *testing.T) {
	cases := []struct {
		method, path, body string
		want               domain.Permission
	}{
		{http.MethodGet, "/projects/p1/workitems", "", domain.PermWorkItemsRead},
		{http.MethodPut, "/projects/p1/workitems", `{}`, domain.PermWorkItemsManage},
		{http.MethodDelete, "/projects/p1/workitems", "", domain.PermWorkItemsManage},
		{http.MethodPost, "/projects/p1/workitems/test", "", domain.PermWorkItemsManage},
		// Enumerating an external organization's projects is the connection
		// owner's view, not a member's.
		{http.MethodGet, "/projects/p1/workitems/projects", "", domain.PermWorkItemsManage},

		{http.MethodGet, "/projects/p1/workitems/health", "", domain.PermWorkItemsRead},
		{http.MethodGet, "/projects/p1/workitems/audit", "", domain.PermWorkItemsRead},
		{http.MethodGet, "/projects/p1/workitems/links", "", domain.PermWorkItemsRead},

		{http.MethodPost, "/projects/p1/workitems/links", `{"scope":"run","scopeId":"r1"}`, domain.PermWorkItemsLink},
		{http.MethodDelete, "/projects/p1/workitems/links/l1", "", domain.PermWorkItemsLink},
		{http.MethodPost, "/projects/p1/workitems/links/l1/sync", `{"enabled":false}`, domain.PermWorkItemsLink},
		{http.MethodPost, "/projects/p1/workitems/sync", "", domain.PermWorkItemsLink},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			guard := newGuard(true)
			h := workItemsRouter(&stubWorkItems{}, guard)
			do(t, h, tc.method, tc.path, tc.body)
			if len(guard.asked()) != 1 {
				t.Fatalf("the route asked for %d permissions, want exactly 1", len(guard.asked()))
			}
			if guard.asked()[0] != tc.want {
				t.Errorf("asked for %q, want %q", guard.asked()[0], tc.want)
			}
		})
	}
}

// Section 10: a project the caller cannot reach answers 404 on every route,
// including the ones that would otherwise leak whether a connection exists.
func TestADeniedProjectIsIndistinguishableFromAMissingOne(t *testing.T) {
	svc := &stubWorkItems{config: controllers.WorkItemsConfigResponse{
		Workspace: "secret-workspace", TokenConfigured: true, Enabled: true,
	}}
	for _, path := range []string{
		"/projects/p1/workitems",
		"/projects/p1/workitems/health",
		"/projects/p1/workitems/links",
		"/projects/p1/workitems/audit",
	} {
		guard := newGuard(false)
		rec := do(t, workItemsRouter(svc, guard), http.MethodGet, path, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s answered %d, want 404", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "secret-workspace") {
			t.Errorf("%s leaked the connection to a denied caller: %s", path, rec.Body.String())
		}
	}
}

// A daemon built without the integration answers 501 rather than panicking or
// pretending the feature is off.
func TestRoutesReportNotImplementedWithoutAService(t *testing.T) {
	guard := newGuard(true)
	r := chi.NewRouter()
	(&controllers.WorkItemsController{Guard: guard.guard}).Register(r)

	rec := do(t, r, http.MethodGet, "/projects/p1/workitems", "")
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", rec.Code)
	}
}

// Section 3/12: the response never carries the credential, and says only
// whether one exists.
func TestTheConfigResponseCarriesNoCredential(t *testing.T) {
	guard := newGuard(true)
	svc := &stubWorkItems{config: controllers.WorkItemsConfigResponse{
		ProjectID: "p1", Provider: "plane", Workspace: "acme",
		ExternalProjectID: "plane-proj", TokenConfigured: true, Enabled: true,
	}}
	rec := do(t, workItemsRouter(svc, guard), http.MethodGet, "/projects/p1/workitems", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"apiToken", "token", "apiKey", "secret"} {
		if _, present := body[forbidden]; present {
			t.Errorf("the response carries a %q field", forbidden)
		}
	}
	if body["tokenConfigured"] != true {
		t.Error("tokenConfigured was not reported")
	}
}

// Omitting a field in the update body must reach the service as "leave this
// alone", which is what the pointer fields exist to express.
func TestOmittedConfigFieldsArriveAsNil(t *testing.T) {
	guard := newGuard(true)
	svc := &stubWorkItems{}
	do(t, workItemsRouter(svc, guard), http.MethodPut, "/projects/p1/workitems", `{"enabled":true}`)

	if svc.putUpdate == nil {
		t.Fatal("the update never reached the service")
	}
	if svc.putUpdate.APIToken != nil {
		t.Error("an omitted apiToken arrived as a value; it must arrive as nil so the stored one is kept")
	}
	if svc.putUpdate.Enabled == nil || !*svc.putUpdate.Enabled {
		t.Error("the field that WAS sent did not arrive")
	}
}

// A malformed body is a 400, not a 500.
func TestAMalformedBodyIsRejected(t *testing.T) {
	guard := newGuard(true)
	rec := do(t, workItemsRouter(&stubWorkItems{}, guard), http.MethodPut, "/projects/p1/workitems", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// The link request reaches the service with the project from the path, never
// from the body — a body-supplied project id would be a cross-project write
// the guard never saw.
func TestTheLinkRequestTakesItsProjectFromThePath(t *testing.T) {
	guard := newGuard(true)
	svc := &stubWorkItems{}
	do(t, workItemsRouter(svc, guard), http.MethodPost, "/projects/p1/workitems/links",
		`{"scope":"run","scopeId":"r1","reference":"ACME-7","projectId":"p2"}`)

	if svc.linkReq == nil {
		t.Fatal("the request never reached the service")
	}
	if svc.linkReq.ProjectID != domain.ProjectID("p1") {
		t.Errorf("project = %q, want the one from the path", svc.linkReq.ProjectID)
	}
}

// Section 13: a link the provider could not answer for is rendered from the
// cache and marked stale, rather than disappearing from the list.
func TestADegradedLinkIsRenderedAsStale(t *testing.T) {
	guard := newGuard(true)
	svc := &stubWorkItems{links: controllers.WorkItemLinksResponse{
		Links: []controllers.WorkItemLinkResponse{{
			ID: "l1", Scope: "run", ScopeID: "r1", ExternalItemKey: "ACME-7",
			Title: "Fix the login redirect", State: "started",
			Stale: true, LiveError: "Plane could not be reached",
		}},
	}}
	rec := do(t, workItemsRouter(svc, guard), http.MethodGet, "/projects/p1/workitems/links?live=true", "")

	var body controllers.WorkItemLinksResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Links) != 1 {
		t.Fatalf("links = %d; a degraded provider must not empty the list", len(body.Links))
	}
	if !body.Links[0].Stale || body.Links[0].LiveError == "" {
		t.Error("the degraded state was not reported")
	}
	if body.Links[0].Title == "" {
		t.Error("the cached title was not rendered")
	}
}
