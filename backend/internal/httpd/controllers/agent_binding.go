package controllers

import (
	"net/http"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
)

// P4-I: an agent credential's BINDINGS, enforced at the transport.
//
// authz.Subject.Allows already caps an agent to its own project and to a tiny
// permission set, and that cap is where the security of the mechanism lives.
// What it cannot express is the finer binding: a reviewer launched for session
// agent-orchestrator-59 must not be able to steer agent-orchestrator-58 merely
// because both sit in the same project. Project-scoped RBAC is the right model
// for a person -- who legitimately works across the project -- and the wrong one
// for a credential minted for a single launch.
//
// So the binding is checked here, at the same canonical boundary the session and
// run gates already are, and it is checked BEFORE the ownership/permission
// branches rather than beside them: an agent reaching for something it was not
// launched for is refused whatever the account behind it may do.
//
// A denial reports the same 404 the ownership gates report, for the same reason:
// a session or run id a caller may not reach must be indistinguishable from one
// that does not exist.

// agentPrincipal returns the request's agent authority, if this request is an
// agent's at all.
func agentPrincipal(r *http.Request) (domain.AgentAuthority, bool) {
	p, ok := identity.PrincipalFromContext(r.Context())
	if !ok || !p.IsAgent() {
		return domain.AgentAuthority{}, false
	}
	return *p.Agent, true
}

// agentMayReachSession enforces the session binding. It returns true for every
// non-agent request, so a human's path through the gate is byte-for-byte
// unchanged.
func agentMayReachSession(w http.ResponseWriter, r *http.Request, id domain.SessionID) bool {
	authority, ok := agentPrincipal(r)
	if !ok {
		return true
	}
	if authority.MayReachSession(id) {
		return true
	}
	envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "SESSION_NOT_FOUND", "session not found", nil)
	return false
}

// agentMayReachWorkflowRun enforces the run binding.
func agentMayReachWorkflowRun(w http.ResponseWriter, r *http.Request, id string) bool {
	authority, ok := agentPrincipal(r)
	if !ok {
		return true
	}
	if authority.MayReachWorkflowRun(id) {
		return true
	}
	envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "WORKFLOW_NOT_FOUND", "workflow run not found", nil)
	return false
}
