package httpd

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
)

// projectScopedRoutePrefixes are the /api/v1 route families whose
// authorization is decided INSIDE the controller, because it needs a project,
// session or run id the middleware cannot resolve from the path alone.
var projectScopedRoutePrefixes = []string{
	// GET /api/v1/projects is the list, filtered per row against the caller's
	// project access; everything under it is scoped to one project id.
	"/api/v1/projects",
	"/api/v1/sessions",
	"/api/v1/orchestrators",
	"/api/v1/workflows",
	"/api/v1/questions/",
	"/api/v1/reviews/",
	"/api/v1/prs/",
	"/api/v1/usage/",
	// P4-C: every /tenants route is decided per organization, which the
	// middleware cannot resolve from the path alone -- the same reason
	// /projects is here. POST /tenants asks AllowGlobal inside the handler.
	"/api/v1/tenants",
}

// unauthorizedByDesignRoutes are the /api/v1 families deliberately reachable
// without an authorization decision, each for a stated reason:
//
//   - /auth and /auth/oidc ARE the authentication surface; gating them on being
//     authenticated is a locked door with the key inside.
//   - /events, /notifications, /capacity, /import (read), /browser, /decisions,
//     /push, /shell-terminals, /mobile and /dev are the loopback/desktop
//     surfaces AO has always served unauthenticated on 127.0.0.1, and P4-B
//     deliberately does not change the primary listener's trust boundary (see
//     AGENTS.md). Narrowing them is a later slice with its own migration of
//     desktop behavior, not a silent change here.
//
// A new route that matches neither this list nor projectScopedRoutePrefixes
// nor the global rule table fails TestEveryAPIRouteHasAnAuthorizationDecision,
// which is the point: an unguarded endpoint should be a decision somebody
// wrote down, never an omission.
var unauthorizedByDesignRoutes = []string{
	"/api/v1/auth",
	"/api/v1/openapi.yaml",
	"/api/v1/events",
	"/api/v1/notifications",
	"/api/v1/capacity",
	"/api/v1/browser",
	"/api/v1/decisions",
	"/api/v1/push",
	"/api/v1/shell-terminals",
	"/api/v1/mobile",
	"/api/v1/scheduler",
	"/api/v1/incidents",
}

// TestEveryAPIRouteHasAnAuthorizationDecision walks the real router and
// asserts every mounted /api/v1 route is classified: gated globally, gated
// per-resource in its controller, or unauthorized on purpose. It is the
// standing guard on the family table -- a per-route list would go stale, and a
// prefix table without this test would silently fail open.
func TestEveryAPIRouteHasAnAuthorizationDecision(t *testing.T) {
	router := NewRouterWithControl(config.Config{}, discardLogger(), nil, APIDeps{}, ControlDeps{})

	var unclassified []string
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/api/v1/") {
			return nil
		}
		if controllers.HasGlobalAuthzRule(method, route) {
			return nil
		}
		for _, prefix := range projectScopedRoutePrefixes {
			if strings.HasPrefix(route, prefix) {
				return nil
			}
		}
		for _, prefix := range unauthorizedByDesignRoutes {
			if strings.HasPrefix(route, prefix) {
				return nil
			}
		}
		unclassified = append(unclassified, method+" "+route)
		return nil
	})
	if err != nil {
		t.Fatalf("walk routes: %v", err)
	}
	if len(unclassified) > 0 {
		t.Fatalf("these routes have no authorization decision — add a global rule, "+
			"gate them in their controller, or list them as unauthorized by design:\n  %s",
			strings.Join(unclassified, "\n  "))
	}
}
