package controllers_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
)

// agent_authority_fence_test.go — P5-A phase 2C's central fence, through the
// REAL router.
//
// The store tests next door prove the predicate. These prove the fence is
// actually ON the path every PermSessionWrite route takes: the same
// AuthorizeSessionAccess boundary that gates /send, /kill, /rollback,
// /restore, /resume-agent, /switch-agent, /pr/claim, /reviewer and
// /auto-review.
//
// That is why they assert against a LIST of routes rather than one: the point
// of a central fence is that nobody has to remember to add it to the next
// endpoint, and a test that checked one route would not notice if it had been
// bolted on there rather than at the boundary.

// sensitiveWorkerRoutes are session writes a worker credential's ceiling
// permits. They are what the window would have been worth.
var sensitiveWorkerRoutes = []struct {
	name   string
	method string
	path   func(session string) string
	body   string
}{
	{"send: type into the session", http.MethodPost,
		func(s string) string { return "/api/v1/sessions/" + s + "/send" }, `{"text":"do something else"}`},
	{"kill: end the session", http.MethodPost,
		func(s string) string { return "/api/v1/sessions/" + s + "/kill" }, `{}`},
	{"auto-review: change its configuration", http.MethodPut,
		func(s string) string { return "/api/v1/sessions/" + s + "/auto-review" }, `{"enabled":false}`},
	{"work-report: the advisory declaration", http.MethodPost,
		func(s string) string { return "/api/v1/sessions/" + s + "/work-report" }, `{"summary":"x"}`},
}

// staticAuthorityChecker is the fence's answer, fixed for a test.
type staticAuthorityChecker struct {
	authorized bool
	err        error
	asked      int
}

func (c *staticAuthorityChecker) StillAuthorized(_ context.Context, _ domain.AgentAuthority) (bool, error) {
	c.asked++
	return c.authorized, c.err
}

func fencedServer(t *testing.T, resolver identity.AgentResolver, checker controllers.AgentAuthorityChecker) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(
		config.Config{TrustedLocalMode: false},
		log, nil,
		httpd.APIDeps{
			// The sessions service has to be wired: those handlers answer 501
			// before they reach authorization, so without it the routes would
			// never touch the fence and the test would prove nothing.
			Sessions:       newFakeSessionService(),
			Workflows:      &workReportService{receipt: workReportReceiptFor("wf-1")},
			AgentAuth:      resolver,
			AgentAuthority: checker,
			SessionOwnership: ownedSessions{
				"agent-orchestrator-59": "user-owner",
			},
		},
		httpd.ControlDeps{},
	))
	t.Cleanup(srv.Close)
	return srv
}

func callAs(t *testing.T, srv *httptest.Server, method, path, token, body string) int {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set(identity.AgentTokenHeader, token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	return resp.StatusCode
}

// A worker on the currently authorized attempt is NOT refused by the fence.
// The routes may answer anything else (501 without a service wired, 400 on a
// body they dislike) -- what matters is that the fence is not what stopped it.
func TestFencePassesTheCurrentWorkerOnEveryRoute(t *testing.T) {
	checker := &staticAuthorityChecker{authorized: true}
	resolver := staticAgentResolver{token: "tok-1", authority: boundWorkerAuthority("agent-orchestrator-59")}
	srv := fencedServer(t, resolver, checker)

	for _, rt := range sensitiveWorkerRoutes {
		t.Run(rt.name, func(t *testing.T) {
			status := callAs(t, srv, rt.method, rt.path("agent-orchestrator-59"), "tok-1", rt.body)
			if status == http.StatusNotFound {
				t.Fatalf("the fence refused the CURRENT worker on %s", rt.name)
			}
		})
	}
	if checker.asked == 0 {
		t.Fatal("the fence was never consulted; it is not on the path")
	}
}

// AND THE POINT. A worker whose attempt is over is refused on EVERY one of
// them, with the credential still live -- no revocation, no sweep, no interval.
func TestFenceRefusesAFinishedAttemptOnEverySensitiveRoute(t *testing.T) {
	checker := &staticAuthorityChecker{authorized: false}
	resolver := staticAgentResolver{token: "tok-1", authority: boundWorkerAuthority("agent-orchestrator-59")}
	srv := fencedServer(t, resolver, checker)

	for _, rt := range sensitiveWorkerRoutes {
		t.Run(rt.name, func(t *testing.T) {
			status := callAs(t, srv, rt.method, rt.path("agent-orchestrator-59"), "tok-1", rt.body)
			// 404, never 403: a caller that may not reach something must not be
			// able to tell it apart from something that is not there.
			if status != http.StatusNotFound {
				t.Fatalf("%s answered %d; a finished attempt must be refused", rt.name, status)
			}
		})
	}
}

// A fence that cannot answer refuses. An authority AO cannot evaluate is not an
// authority, and a database blip must not become an open door.
func TestFenceFailsClosed(t *testing.T) {
	checker := &staticAuthorityChecker{authorized: true, err: errNoSuchCredential}
	resolver := staticAgentResolver{token: "tok-1", authority: boundWorkerAuthority("agent-orchestrator-59")}
	srv := fencedServer(t, resolver, checker)

	for _, rt := range sensitiveWorkerRoutes {
		t.Run(rt.name, func(t *testing.T) {
			if status := callAs(t, srv, rt.method, rt.path("agent-orchestrator-59"), "tok-1", rt.body); status != http.StatusNotFound {
				t.Fatalf("%s answered %d while the fence was failing; it must fail closed", rt.name, status)
			}
		})
	}
}

// The fence is for AGENTS. A human's path through the same boundary is
// untouched -- it is never consulted for a request that carries no agent
// credential.
func TestFenceIsNeverConsultedForAHuman(t *testing.T) {
	checker := &staticAuthorityChecker{authorized: false}
	resolver := staticAgentResolver{token: "tok-1", authority: boundWorkerAuthority("agent-orchestrator-59")}
	srv := fencedServer(t, resolver, checker)

	// No agent token: this is a person (or, here, nobody).
	for _, rt := range sensitiveWorkerRoutes {
		callAs(t, srv, rt.method, rt.path("agent-orchestrator-59"), "", rt.body)
	}
	if checker.asked != 0 {
		t.Fatalf("the fence was consulted %d times for a non-agent request", checker.asked)
	}
}

// A REVIEWER's authority is not a worker's, and the fence must not start
// answering for it: its lifetime is its review run, which agentauth already
// handles. The service-level fence returns true for a non-worker; this asserts
// the transport does not invent a different answer.
func TestFenceLeavesAReviewerToItsOwnLifetime(t *testing.T) {
	checker := &staticAuthorityChecker{authorized: true}
	resolver := staticAgentResolver{token: "tok-1", authority: reviewerAuthorityFor("agent-orchestrator-59")}
	srv := fencedServer(t, resolver, checker)

	status := callAs(t, srv, http.MethodPost, "/api/v1/sessions/agent-orchestrator-59/send", "tok-1", `{"text":"a verdict"}`)
	if status == http.StatusNotFound {
		t.Fatal("a reviewer was refused on the session it was launched for")
	}
}

func reviewerAuthorityFor(sessionID domain.SessionID) *domain.AgentAuthority {
	return &domain.AgentAuthority{
		CredentialID: "cred-rev",
		Role:         domain.AgentRoleReviewer,
		ProjectID:    "proj-1",
		SessionID:    sessionID,
		ReviewRunID:  "rr-1",
		Permissions:  domain.AgentRoleCeiling(domain.AgentRoleReviewer),
	}
}
