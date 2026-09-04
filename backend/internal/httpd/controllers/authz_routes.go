package controllers

import (
	"net/http"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// globalRule is one installation-wide route family and the permissions it
// requires. Read is checked for GET/HEAD; Write for everything else.
type globalRule struct {
	// segment is the first path segment under /api/v1.
	segment string
	read    domain.Permission
	write   domain.Permission
}

// globalRouteRules is the audited list of INSTALLATION-WIDE route families and
// what each one requires. It is a family table rather than a route table on
// purpose: a new /settings/... route inherits the settings rule instead of
// arriving unguarded because somebody forgot a line here, which is the failure
// mode a per-route list has and this one does not.
//
// Everything absent from this table is either project-scoped -- gated inside
// the controller that can resolve the project (see Guard.AllowProject /
// AllowSession / AllowWorkflowRun) -- or deliberately reachable without
// authorization: the auth and SSO routes themselves, the loopback hook
// callbacks a running agent posts to, and the health/OpenAPI surfaces.
// TestGlobalAuthzRulesCoverEveryRoute holds that classification honest.
var globalRouteRules = []globalRule{
	// Installation settings and routing/execution policy.
	{segment: "settings", read: domain.PermSettingsRead, write: domain.PermSettingsManage},
	{segment: "execution-policy", read: domain.PermSettingsRead, write: domain.PermSettingsManage},
	{segment: "environment", read: domain.PermSettingsRead, write: domain.PermSettingsManage},
	{segment: "runtime", read: domain.PermSettingsRead, write: domain.PermSettingsManage},

	// Provider profiles, the agent catalog, and the credentialed setup flows.
	// A probe or a model refresh spends a real credential, so it is a manage
	// operation even though it reads like a query.
	{segment: "provider-profiles", read: domain.PermProviderRead, write: domain.PermProviderManage},
	{segment: "providers", read: domain.PermProviderRead, write: domain.PermProviderManage},
	{segment: "agents", read: domain.PermProviderRead, write: domain.PermProviderManage},

	// P4-B's own administration surfaces.
	{segment: "users", read: domain.PermUsersRead, write: domain.PermUsersManage},
	{segment: "teams", read: domain.PermTeamsRead, write: domain.PermTeamsManage},

	// Bringing projects into the installation.
	{segment: "import", read: domain.PermSettingsRead, write: domain.PermProjectCreate},
	{segment: "dev", read: domain.PermSettingsRead, write: domain.PermProjectCreate},
}

// projectCreateRoutes are the exact /projects paths that create a project
// rather than acting on one. They are listed individually because everything
// else under /projects is project-scoped and must NOT be gated here -- gating
// the whole segment globally would deny a member their own projects.
var projectCreateRoutes = map[string]bool{
	"projects":            true,
	"projects/clone":      true,
	"projects/initialize": true,
}

// requiredGlobalPermission reports the installation-wide permission a request
// needs, if any. ok is false for a path this layer does not gate.
func requiredGlobalPermission(method, path string) (domain.Permission, bool) {
	rest, ok := strings.CutPrefix(path, "/api/v1/")
	if !ok {
		return "", false
	}
	rest = strings.Trim(rest, "/")
	if rest == "" {
		return "", false
	}
	read := method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions

	if projectCreateRoutes[rest] {
		if read {
			// GET /projects is the project LIST, scoped per project in the
			// controller; only the create verbs are installation-wide.
			return "", false
		}
		return domain.PermProjectCreate, true
	}

	segment := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		segment = rest[:i]
	}
	for _, rule := range globalRouteRules {
		if rule.segment != segment {
			continue
		}
		if read {
			return rule.read, true
		}
		return rule.write, true
	}
	return "", false
}

// GlobalAuthzMiddleware enforces the installation-wide permissions above. It
// is the single choke point every transport shares: the browser, the desktop
// renderer, the CLI and the LAN bridge all reach these routes through it, so a
// button hidden in React and a curl against the same path get the same answer.
//
// A disabled Guard (no identity layer wired) passes everything through, which
// is what keeps headless and test configurations behaving as they did.
func GlobalAuthzMiddleware(g Guard) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !g.Enabled() {
				next.ServeHTTP(w, r)
				return
			}
			perm, ok := requiredGlobalPermission(r.Method, r.URL.Path)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			if !g.AllowGlobal(w, r, perm) {
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// HasGlobalAuthzRule reports whether this layer gates the given route. Exported
// for the router-walking coverage test in package httpd, which is what keeps a
// newly mounted route from arriving without any authorization decision at all.
func HasGlobalAuthzRule(method, path string) bool {
	_, ok := requiredGlobalPermission(method, path)
	return ok
}
