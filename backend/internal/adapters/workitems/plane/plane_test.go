package plane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// plane_test.go — the adapter's contract with a real HTTP surface.
//
// Every test here drives an httptest server shaped like Plane's documented
// responses. That is the honest form of a contract test when no credentials
// exist: it cannot prove Plane behaves this way, and it does prove AO behaves
// correctly GIVEN that it does — including on the paths that only appear when
// something goes wrong, which is where an integration actually lives.

type route struct {
	method string
	path   string
	status int
	body   string
}

func serverFor(t *testing.T, routes ...route) (*Client, *[]*http.Request) {
	t.Helper()
	var seen []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Clone(context.Background()))
		for _, rt := range routes {
			if rt.method != "" && rt.method != r.Method {
				continue
			}
			if !strings.HasPrefix(r.URL.Path, rt.path) || r.URL.Path != rt.path {
				continue
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rt.status)
			_, _ = w.Write([]byte(rt.body))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no route in the fake"}`))
	}))
	t.Cleanup(srv.Close)

	client, err := New(Options{BaseURL: srv.URL, Workspace: "acme", Token: StaticToken("tok")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client, &seen
}

func page(results string) string {
	return `{"next_cursor":"","prev_cursor":"","next_page_results":false,"count":1,"total_pages":1,"results":` + results + `}`
}

const statesJSON = `[
  {"id":"st-backlog","name":"Backlog","group":"backlog","default":true,"sequence":15000},
  {"id":"st-todo","name":"Todo","group":"unstarted","sequence":25000},
  {"id":"st-doing","name":"In Progress","group":"started","sequence":35000},
  {"id":"st-review","name":"In Review","group":"started","sequence":36000},
  {"id":"st-done","name":"Done","group":"completed","sequence":45000},
  {"id":"st-cancelled","name":"Cancelled","group":"cancelled","sequence":55000}
]`

const projectsJSON = `[{"id":"proj-uuid","name":"Acme Web","identifier":"ACME","description":"the web app"}]`

func issueJSON(state string) string {
	return `{"id":"item-uuid","name":"Fix the login redirect","description_stripped":"It loops.",` +
		`"state":"` + state + `","priority":"high","sequence_id":123,"project":"proj-uuid",` +
		`"workspace":"ws-uuid","external_source":"agent-orchestrator","external_id":"run:r1",` +
		`"updated_at":"2026-09-05T10:00:00Z"}`
}

// The credential goes in a header and nowhere else. A token in a query string
// would reach every access log between AO and the provider.
func TestTokenTravelsAsAHeaderAndNeverInTheURL(t *testing.T) {
	client, seen := serverFor(t, route{http.MethodGet, "/api/v1/workspaces/acme/projects/", 200, page(projectsJSON)})
	if _, err := client.ListProjects(t.Context()); err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(*seen) == 0 {
		t.Fatal("no request was made")
	}
	req := (*seen)[0]
	if got := req.Header.Get("X-API-Key"); got != "tok" {
		t.Errorf("X-API-Key = %q, want the configured token", got)
	}
	if strings.Contains(req.URL.String(), "tok") {
		t.Errorf("the token appeared in the URL: %s", req.URL)
	}
}

// Section 4: failures are classified where the status is still in scope, so
// the sync worker can decide retryability without matching on message text.
func TestErrorsAreClassifiedForRetry(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantKind  ports.WorkItemsErrorKind
		wantRetry bool
	}{
		{"unauthorized", 401, `{"error":"Invalid API key"}`, ports.WorkItemsErrAuth, false},
		{"forbidden", 403, `{"error":"no access"}`, ports.WorkItemsErrAuth, false},
		{"missing", 404, `{"error":"not found"}`, ports.WorkItemsErrNotFound, false},
		{"rejected", 400, `{"name":["This field is required."]}`, ports.WorkItemsErrInvalid, false},
		{"rate limited", 429, `{"error":"slow down"}`, ports.WorkItemsErrRateLimited, true},
		{"server error", 503, `{"error":"upstream"}`, ports.WorkItemsErrUnavailable, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := serverFor(t, route{http.MethodGet, "/api/v1/workspaces/acme/projects/", tc.status, tc.body})
			_, err := client.ListProjects(t.Context())
			var wErr *ports.WorkItemsError
			if !errors.As(err, &wErr) {
				t.Fatalf("error = %v, want a *ports.WorkItemsError", err)
			}
			if wErr.Kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", wErr.Kind, tc.wantKind)
			}
			if wErr.Retryable() != tc.wantRetry {
				t.Errorf("retryable = %v, want %v", wErr.Retryable(), tc.wantRetry)
			}
			// The provider's own explanation survives, so an operator sees why.
			if wErr.Message == "" {
				t.Error("the provider's explanation was dropped")
			}
			// And the credential does not.
			if strings.Contains(wErr.Error(), "tok") {
				t.Errorf("the token reached an error message: %s", wErr.Error())
			}
		})
	}
}

// Cursor pagination: AO follows next_cursor while the provider says there is a
// next page, and stops when it says there is not.
func TestPaginationFollowsTheCursor(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			_, _ = w.Write([]byte(`{"next_cursor":"100:1:0","next_page_results":true,"count":2,"total_pages":2,` +
				`"results":[{"id":"p1","name":"One","identifier":"ONE"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"next_cursor":"","next_page_results":false,"count":2,"total_pages":2,` +
			`"results":[{"id":"p2","name":"Two","identifier":"TWO"}]}`))
	}))
	defer srv.Close()

	client, err := New(Options{BaseURL: srv.URL, Workspace: "acme", Token: StaticToken("tok")})
	if err != nil {
		t.Fatal(err)
	}
	projects, err := client.ListProjects(t.Context())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("got %d projects across pages, want 2", len(projects))
	}
	if calls != 2 {
		t.Errorf("made %d requests, want 2 — one per page", calls)
	}
}

// A work item's state arrives as a bare UUID; the portable thing is its group,
// which lives on the state. Resolving it is what makes status mapping possible.
func TestGetResolvesTheStateGroup(t *testing.T) {
	client, _ := serverFor(t,
		route{http.MethodGet, "/api/v1/workspaces/acme/projects/proj-uuid/work-items/item-uuid/", 200, issueJSON("st-doing")},
		route{http.MethodGet, "/api/v1/workspaces/acme/projects/proj-uuid/states/", 200, page(statesJSON)},
		route{http.MethodGet, "/api/v1/workspaces/acme/projects/", 200, page(projectsJSON)},
	)
	item, err := client.Get(t.Context(), domain.WorkItemRef{
		Provider: domain.WorkItemProviderPlane, Workspace: "acme", Project: "proj-uuid", ID: "item-uuid",
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if item.StateGroup != domain.WorkItemStateStarted {
		t.Errorf("state group = %q, want started", item.StateGroup)
	}
	if item.StateName != "In Progress" {
		t.Errorf("state name = %q, want the provider's own name", item.StateName)
	}
	// And mapped onto AO's existing cross-provider vocabulary.
	if item.State != domain.IssueInProgress {
		t.Errorf("normalized state = %q, want in_progress", item.State)
	}
	// The human key is built from the project prefix and the sequence.
	if item.Ref.Key != "ACME-123" {
		t.Errorf("key = %q, want ACME-123", item.Ref.Key)
	}
}

// A state the adapter cannot resolve must leave the group EMPTY rather than
// guess. An unknown planning state rendering as "done" is the one mistake here
// that would actually mislead somebody.
func TestAnUnresolvableStateIsNotGuessed(t *testing.T) {
	client, _ := serverFor(t,
		route{http.MethodGet, "/api/v1/workspaces/acme/projects/proj-uuid/work-items/item-uuid/", 200, issueJSON("st-unknown")},
		route{http.MethodGet, "/api/v1/workspaces/acme/projects/proj-uuid/states/", 200, page(statesJSON)},
		route{http.MethodGet, "/api/v1/workspaces/acme/projects/", 200, page(projectsJSON)},
	)
	item, err := client.Get(t.Context(), domain.WorkItemRef{
		Provider: domain.WorkItemProviderPlane, Workspace: "acme", Project: "proj-uuid", ID: "item-uuid",
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if item.StateGroup != "" || item.State != "" {
		t.Errorf("an unresolvable state produced group=%q state=%q, want both empty",
			item.StateGroup, item.State)
	}
}

// Resolve accepts what a person actually has in their hand.
func TestResolveAcceptsAHumanKey(t *testing.T) {
	client, seen := serverFor(t,
		route{http.MethodGet, "/api/v1/workspaces/acme/work-items/ACME-123/", 200, issueJSON("st-doing")},
		route{http.MethodGet, "/api/v1/workspaces/acme/projects/proj-uuid/states/", 200, page(statesJSON)},
		route{http.MethodGet, "/api/v1/workspaces/acme/projects/", 200, page(projectsJSON)},
	)
	// Lowercase, because that is what somebody types.
	item, err := client.Resolve(t.Context(), "acme-123")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if item.Ref.ID != "item-uuid" {
		t.Errorf("resolved to %q", item.Ref.ID)
	}
	if !strings.Contains((*seen)[0].URL.Path, "/work-items/ACME-123/") {
		t.Errorf("looked up %s; the by-identifier route takes the upper-case key", (*seen)[0].URL.Path)
	}
}

func TestResolveRejectsNonsense(t *testing.T) {
	client, _ := serverFor(t)
	for _, reference := range []string{"", "just some words", "https://example.com/nothing"} {
		if _, err := client.Resolve(t.Context(), reference); err == nil {
			t.Errorf("Resolve(%q) succeeded; a reference AO cannot parse must be refused", reference)
		}
	}
}

// A pasted browser URL is the other thing people have.
func TestResolveAcceptsAPastedURL(t *testing.T) {
	client, _ := serverFor(t,
		route{http.MethodGet, "/api/v1/workspaces/acme/projects/proj-uuid/work-items/item-uuid/", 200, issueJSON("st-doing")},
		route{http.MethodGet, "/api/v1/workspaces/acme/projects/proj-uuid/states/", 200, page(statesJSON)},
		route{http.MethodGet, "/api/v1/workspaces/acme/projects/", 200, page(projectsJSON)},
	)
	item, err := client.Resolve(t.Context(), "https://app.plane.so/acme/projects/proj-uuid/issues/item-uuid")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if item.Ref.ID != "item-uuid" {
		t.Errorf("resolved to %q", item.Ref.ID)
	}
}

// A URL naming a different workspace is refused rather than silently read
// against the configured one.
func TestResolveRefusesAnotherWorkspacesURL(t *testing.T) {
	client, _ := serverFor(t)
	_, err := client.Resolve(t.Context(), "https://app.plane.so/other/projects/p/issues/i")
	if err == nil {
		t.Fatal("a URL from another workspace was accepted")
	}
	if !strings.Contains(err.Error(), "workspace") {
		t.Errorf("error = %v; it should say which workspace mismatched", err)
	}
}

// Section 6: creating an item is idempotent. Plane answers a duplicate
// (external_source, external_id) with 409, and AO reads the existing item back
// rather than reporting a failure or creating a second one.
func TestCreateIsIdempotentAgainstADuplicate(t *testing.T) {
	client, _ := serverFor(t,
		route{http.MethodPost, "/api/v1/workspaces/acme/projects/proj-uuid/work-items/", 409,
			`{"error":"Issue with the same external id and external source already exists","id":"item-uuid"}`},
		route{http.MethodGet, "/api/v1/workspaces/acme/projects/proj-uuid/work-items/", 200, page("[" + issueJSON("st-doing") + "]")},
		route{http.MethodGet, "/api/v1/workspaces/acme/projects/proj-uuid/states/", 200, page(statesJSON)},
		route{http.MethodGet, "/api/v1/workspaces/acme/projects/", 200, page(projectsJSON)},
	)
	item, err := client.Create(t.Context(), ports.WorkItemCreateRequest{
		ProjectID: "proj-uuid", Title: "Fix the login redirect", ExternalID: "run:r1",
	})
	if err != nil {
		t.Fatalf("a duplicate create should resolve to the existing item, got %v", err)
	}
	if item.Ref.ID != "item-uuid" {
		t.Errorf("resolved to %q, want the existing item", item.Ref.ID)
	}
}

// A create without an external id is refused: an item AO cannot find again is
// one AO will duplicate on the next attempt.
func TestCreateRequiresAnExternalID(t *testing.T) {
	client, _ := serverFor(t)
	_, err := client.Create(t.Context(), ports.WorkItemCreateRequest{ProjectID: "proj-uuid", Title: "x"})
	if err == nil {
		t.Fatal("a create with no external id was accepted")
	}
}

// Section 7: a transition already in the target group does nothing. Plane
// records every state write as an activity a person sees, so a sync that fires
// twice must not churn the item's history.
func TestTransitionIsANoOpWhenAlreadyInTheGroup(t *testing.T) {
	var patches int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patches++
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/states/"):
			_, _ = w.Write([]byte(page(statesJSON)))
		case strings.HasSuffix(r.URL.Path, "/projects/"):
			_, _ = w.Write([]byte(page(projectsJSON)))
		default:
			_, _ = w.Write([]byte(issueJSON("st-doing")))
		}
	}))
	defer srv.Close()

	client, err := New(Options{BaseURL: srv.URL, Workspace: "acme", Token: StaticToken("tok")})
	if err != nil {
		t.Fatal(err)
	}
	ref := domain.WorkItemRef{
		Provider: domain.WorkItemProviderPlane, Workspace: "acme", Project: "proj-uuid", ID: "item-uuid",
	}
	// st-doing is already `started`.
	if err := client.Transition(t.Context(), ref, domain.WorkItemStateStarted); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if patches != 0 {
		t.Errorf("made %d state writes for an item already in that group, want 0", patches)
	}
	if err := client.Transition(t.Context(), ref, domain.WorkItemStateCompleted); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if patches != 1 {
		t.Errorf("made %d state writes for a real move, want 1", patches)
	}
}

// Which concrete state AO writes within a group must be deterministic, or two
// AO instances driving one project would disagree and a person could not
// predict where work lands. "In Progress" (sequence 35000) beats "In Review"
// (36000); both are `started`.
func TestTransitionPicksTheStateDeterministically(t *testing.T) {
	var patched map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			_ = json.NewDecoder(r.Body).Decode(&patched)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/states/"):
			_, _ = w.Write([]byte(page(statesJSON)))
		case strings.HasSuffix(r.URL.Path, "/projects/"):
			_, _ = w.Write([]byte(page(projectsJSON)))
		default:
			_, _ = w.Write([]byte(issueJSON("st-todo")))
		}
	}))
	defer srv.Close()

	client, _ := New(Options{BaseURL: srv.URL, Workspace: "acme", Token: StaticToken("tok")})
	err := client.Transition(t.Context(), domain.WorkItemRef{
		Provider: domain.WorkItemProviderPlane, Workspace: "acme", Project: "proj-uuid", ID: "item-uuid",
	}, domain.WorkItemStateStarted)
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if patched["state"] != "st-doing" {
		t.Errorf("wrote state %q, want st-doing — the lowest-sequence state in the group", patched["state"])
	}
}

// A project with no state in the target group is a configuration AO cannot
// satisfy, and it says so rather than writing something approximate.
func TestTransitionRefusesAGroupTheProjectDoesNotDefine(t *testing.T) {
	client, _ := serverFor(t,
		route{http.MethodGet, "/api/v1/workspaces/acme/projects/proj-uuid/work-items/item-uuid/", 200, issueJSON("st-doing")},
		route{http.MethodGet, "/api/v1/workspaces/acme/projects/proj-uuid/states/", 200,
			page(`[{"id":"st-doing","name":"In Progress","group":"started","sequence":1}]`)},
		route{http.MethodGet, "/api/v1/workspaces/acme/projects/", 200, page(projectsJSON)},
	)
	err := client.Transition(t.Context(), domain.WorkItemRef{
		Provider: domain.WorkItemProviderPlane, Workspace: "acme", Project: "proj-uuid", ID: "item-uuid",
	}, domain.WorkItemStateCompleted)
	if err == nil {
		t.Fatal("transitioned into a group the project does not define")
	}
}

// Section 8: a comment carries the dedupe key in the provider's own external-id
// fields, and a 409 from a duplicate is success — the note AO wanted posted is
// already there.
func TestCommentCarriesTheDedupeKeyAndToleratesADuplicate(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"already exists"}`))
	}))
	defer srv.Close()

	client, _ := New(Options{BaseURL: srv.URL, Workspace: "acme", Token: StaticToken("tok")})
	err := client.Comment(t.Context(), domain.WorkItemRef{
		Provider: domain.WorkItemProviderPlane, Workspace: "acme", Project: "proj-uuid", ID: "item-uuid",
	}, "Agent Orchestrator started working on this.", "run:r1:started")
	if err != nil {
		t.Fatalf("a duplicate comment should be success, got %v", err)
	}
	if body["external_id"] != "run:r1:started" {
		t.Errorf("external_id = %v, want the dedupe key", body["external_id"])
	}
	if body["external_source"] != domain.WorkItemExternalSource {
		t.Errorf("external_source = %v, want AO's own marker", body["external_source"])
	}
}

// Anything AO writes into a body is escaped. Commit subjects and stop reasons
// routinely contain characters that would otherwise close a tag.
func TestCommentBodiesAreEscaped(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	client, _ := New(Options{BaseURL: srv.URL, Workspace: "acme", Token: StaticToken("tok")})
	err := client.Comment(t.Context(), domain.WorkItemRef{
		Provider: domain.WorkItemProviderPlane, Workspace: "acme", Project: "proj-uuid", ID: "item-uuid",
	}, `fix: <script>alert("x")</script> & more`, "k")
	if err != nil {
		t.Fatalf("Comment: %v", err)
	}
	html, _ := body["comment_html"].(string)
	if strings.Contains(html, "<script>") {
		t.Errorf("markup reached the provider unescaped: %s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Errorf("the text was not escaped: %s", html)
	}
}

// Section 3: a half-configured integration fails at construction, clearly,
// rather than at the first request with a confusing provider error.
func TestConstructionRefusesAPartialConfiguration(t *testing.T) {
	if _, err := New(Options{Workspace: "", Token: StaticToken("t")}); err == nil {
		t.Error("built a client with no workspace")
	}
	if _, err := New(Options{Workspace: "acme", Token: nil}); err == nil {
		t.Error("built a client with no token source")
	}
	if _, err := New(Options{Workspace: "acme", Token: StaticToken(""), HTTPClient: http.DefaultClient}); err != nil {
		t.Errorf("an empty token should fail at USE, not at construction: %v", err)
	}
}

// The likeliest configuration mistake is pasting the documented URL including
// its version prefix. Producing /api/v1/api/v1 would fail with a 404 that looks
// like a permissions problem.
func TestBaseURLNormalisation(t *testing.T) {
	cases := map[string]string{
		"":                               DefaultBaseURL,
		"https://plane.acme.test":        "https://plane.acme.test",
		"https://plane.acme.test/":       "https://plane.acme.test",
		"https://plane.acme.test/api/v1": "https://plane.acme.test",
	}
	for in, want := range cases {
		got, err := NormalizeBaseURL(in)
		if err != nil {
			t.Errorf("NormalizeBaseURL(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"ftp://plane.acme.test", "not a url at all", "/relative"} {
		if _, err := NormalizeBaseURL(bad); err == nil {
			t.Errorf("NormalizeBaseURL(%q) was accepted", bad)
		}
	}
}

// Section 4: Plane's own rate-limit hint is honoured rather than guessed at.
func TestRetryAfterReadsTheProvidersHint(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	h := http.Header{}
	// Derived from now rather than hardcoded, so the expectation cannot drift
	// from the fixture the way a pasted epoch quietly can.
	h.Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(45*time.Second).Unix(), 10))
	if d, ok := RetryAfter(h, now); !ok || d != 45*time.Second {
		t.Errorf("RetryAfter = %v/%v, want 45s", d, ok)
	}

	h = http.Header{}
	h.Set("Retry-After", "30")
	if d, ok := RetryAfter(h, now); !ok || d != 30*time.Second {
		t.Errorf("RetryAfter = %v/%v, want 30s", d, ok)
	}

	// A reset in the past yields false, so a caller falls back to its own
	// backoff rather than to zero — which would turn a rate limit into a hot
	// loop.
	h = http.Header{}
	h.Set("X-RateLimit-Reset", "1000")
	if _, ok := RetryAfter(h, now); ok {
		t.Error("a reset in the past should not produce a delay")
	}
	if _, ok := RetryAfter(http.Header{}, now); ok {
		t.Error("no headers should not produce a delay")
	}
}

// A context deadline is reported as unavailable-and-retryable rather than as
// a provider refusal, so a slow Plane defers the sync instead of failing it.
func TestATimeoutIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, _ := New(Options{BaseURL: srv.URL, Workspace: "acme", Token: StaticToken("tok")})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()

	_, err := client.ListProjects(ctx)
	var wErr *ports.WorkItemsError
	if !errors.As(err, &wErr) {
		t.Fatalf("error = %v, want a classified error", err)
	}
	if !wErr.Retryable() {
		t.Errorf("a timeout classified as %q is not retryable; a slow provider should defer, not fail", wErr.Kind)
	}
}
