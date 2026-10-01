package controllers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
)

// human_approver_internal_test.go -- AR-1a / D-SEC-3: the approver of a
// human-approval route is the authenticated principal, never the request body.

func approverRequest(p *domain.Principal) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	if p != nil {
		r = r.WithContext(identity.WithPrincipal(r.Context(), *p))
	}
	return r
}

func person(method domain.AuthMethod) *domain.Principal {
	return &domain.Principal{User: domain.User{ID: "user-ada", Username: "ada"}, AuthMethod: method}
}

func TestRequireHumanApprover(t *testing.T) {
	agent := &domain.Principal{User: domain.User{ID: "user-ada", Username: "ada"}, AuthMethod: domain.AuthMethodAgent,
		Agent: &domain.AgentAuthority{Role: domain.AgentRoleWorker}}
	for _, tc := range []struct {
		name       string
		principal  *domain.Principal
		claimed    string
		wantOK     bool
		wantStatus int
		wantMethod domain.AuthMethod
	}{
		{"no principal", nil, "", false, http.StatusUnauthorized, ""},
		{"principal without a user", &domain.Principal{AuthMethod: domain.AuthMethodTrustedLocal}, "", false, http.StatusUnauthorized, ""},
		{"principal without an auth method", person(""), "", false, http.StatusUnauthorized, ""},
		{"agent", agent, "", false, http.StatusForbidden, ""},
		{"agent claiming a person", agent, "ada", false, http.StatusForbidden, ""},
		{"body names someone else", person(domain.AuthMethodPassword), "mallory", false, http.StatusUnprocessableEntity, ""},
		{"password, body omitted", person(domain.AuthMethodPassword), "", true, 0, domain.AuthMethodPassword},
		{"oidc, body names self by username", person(domain.AuthMethodOIDC), "ada", true, 0, domain.AuthMethodOIDC},
		{"trusted-local, body names self by id", person(domain.AuthMethodTrustedLocal), "user-ada", true, 0, domain.AuthMethodTrustedLocal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			got, ok := requireHumanApprover(w, approverRequest(tc.principal), tc.claimed)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (status %d)", ok, tc.wantOK, w.Code)
			}
			if !ok {
				if w.Code != tc.wantStatus {
					t.Fatalf("status = %d, want %d", w.Code, tc.wantStatus)
				}
				return
			}
			if got.Name != "ada" || got.UserID != "user-ada" || got.AuthMethod != tc.wantMethod {
				t.Fatalf("approver = %+v, want ada/user-ada/%s", got, tc.wantMethod)
			}
		})
	}
}
