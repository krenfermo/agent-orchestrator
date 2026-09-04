package controllers

import (
	"net/http"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// TestRequiredGlobalPermission pins the installation-wide route table. Each
// row is a claim about what an operator can reach, so a change here should be
// a deliberate edit to this table and not a side effect of moving a route.
func TestRequiredGlobalPermission(t *testing.T) {
	cases := []struct {
		method, path string
		want         domain.Permission
		gated        bool
	}{
		{http.MethodGet, "/api/v1/settings", domain.PermSettingsRead, true},
		{http.MethodPatch, "/api/v1/settings/session-interface", domain.PermSettingsManage, true},
		{http.MethodPatch, "/api/v1/settings/email-notifications", domain.PermSettingsManage, true},
		{http.MethodGet, "/api/v1/execution-policy", domain.PermSettingsRead, true},
		{http.MethodPut, "/api/v1/execution-policy", domain.PermSettingsManage, true},
		{http.MethodGet, "/api/v1/environment/status", domain.PermSettingsRead, true},
		{http.MethodPost, "/api/v1/environment/github/test", domain.PermSettingsManage, true},
		{http.MethodPost, "/api/v1/runtime/gc", domain.PermSettingsManage, true},

		{http.MethodGet, "/api/v1/provider-profiles", domain.PermProviderRead, true},
		{http.MethodPost, "/api/v1/provider-profiles", domain.PermProviderManage, true},
		{http.MethodPost, "/api/v1/provider-profiles/p1/connect", domain.PermProviderManage, true},
		{http.MethodGet, "/api/v1/providers/registry", domain.PermProviderRead, true},
		{http.MethodGet, "/api/v1/agents", domain.PermProviderRead, true},
		// A probe spends a real credential, so it is a manage operation even
		// though it reads like a query.
		{http.MethodPost, "/api/v1/agents/codex/probe", domain.PermProviderManage, true},

		{http.MethodGet, "/api/v1/users", domain.PermUsersRead, true},
		{http.MethodPost, "/api/v1/users", domain.PermUsersManage, true},
		{http.MethodPatch, "/api/v1/users/u1/role", domain.PermUsersManage, true},
		{http.MethodGet, "/api/v1/teams", domain.PermTeamsRead, true},
		{http.MethodDelete, "/api/v1/teams/t1/members/u1", domain.PermTeamsManage, true},

		// Creating a project is installation-wide; everything else under
		// /projects is scoped to one project and decided in the controller.
		{http.MethodPost, "/api/v1/projects", domain.PermProjectCreate, true},
		{http.MethodPost, "/api/v1/projects/clone", domain.PermProjectCreate, true},
		{http.MethodPost, "/api/v1/projects/initialize", domain.PermProjectCreate, true},
		{http.MethodGet, "/api/v1/projects", "", false},
		{http.MethodGet, "/api/v1/projects/p1", "", false},
		{http.MethodDelete, "/api/v1/projects/p1", "", false},
		{http.MethodGet, "/api/v1/projects/p1/access", "", false},
		{http.MethodPut, "/api/v1/projects/p1/access", "", false},

		// Project-scoped and public surfaces are not this layer's business.
		{http.MethodGet, "/api/v1/sessions", "", false},
		{http.MethodPost, "/api/v1/workflows/r1/cancel", "", false},
		{http.MethodPost, "/api/v1/auth/login", "", false},
		{http.MethodGet, "/api/v1/auth/me", "", false},
		{http.MethodGet, "/api/v1/notifications", "", false},
		{http.MethodGet, "/healthz", "", false},
	}

	for _, tc := range cases {
		got, ok := requiredGlobalPermission(tc.method, tc.path)
		if ok != tc.gated {
			t.Errorf("%s %s: gated=%v want %v", tc.method, tc.path, ok, tc.gated)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("%s %s: permission=%q want %q", tc.method, tc.path, got, tc.want)
		}
	}
}

// TestGlobalRulesNameOnlyRealPermissions stops the table from referring to a
// permission that no role grants -- a rule nobody can satisfy is an outage
// wearing a security label.
func TestGlobalRulesNameOnlyRealPermissions(t *testing.T) {
	known := map[domain.Permission]bool{}
	for _, p := range domain.AllPermissions {
		known[p] = true
	}
	for _, rule := range globalRouteRules {
		for _, p := range []domain.Permission{rule.read, rule.write} {
			if !known[p] {
				t.Errorf("route family %q requires unknown permission %q", rule.segment, p)
			}
			if domain.ScopeOf(p) != domain.AuthzScopeGlobal {
				t.Errorf("route family %q requires the project-scoped %q at the global gate", rule.segment, p)
			}
		}
	}
}
