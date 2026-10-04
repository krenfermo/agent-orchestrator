package controllers

import (
	"context"
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
func agentMayReachSession(w http.ResponseWriter, r *http.Request, id domain.SessionID, authorized AgentAuthorityChecker) bool {
	authority, ok := agentPrincipal(r)
	if !ok {
		return true
	}
	if !authority.MayReachSession(id) {
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "SESSION_NOT_FOUND", "session not found", nil)
		return false
	}
	// P5-A phase 2C: the binding says which session this credential was minted
	// for. It does NOT say whether the launch it was minted for is still the
	// authorized one, and for a worker those are different questions.
	//
	// A worker holds session WRITE -- /send, /kill, /rollback, /restore,
	// /resume-agent, /switch-agent, /reviewer, /auto-review -- over a session
	// Checkpoint 8D reuses for the step's whole loop. Revocation ends such a
	// credential eagerly and, failing that, on the reconciler's sweep; but an
	// authorization that trusted revocation to have ALREADY happened would leave
	// an interval in which a finished attempt still acts on the session its
	// successor is working in.
	//
	// So the question is asked here, per request, derived from durable rows, and
	// the answer cannot be stale. It only ever denies, and a checker that cannot
	// answer denies too: an authority AO cannot evaluate is not an authority.
	if authorized == nil {
		return true
	}
	ok, err := authorized.StillAuthorized(r.Context(), authority)
	if err != nil || !ok {
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "SESSION_NOT_FOUND", "session not found", nil)
		return false
	}
	return true
}

// AgentAuthorityChecker answers whether an agent's authority is still the
// current one, from durable state, at the moment of the request.
//
// It is separate from the bindings above because it answers a different
// question. A binding is fixed at mint time and says WHERE a credential may
// act; this says WHETHER the launch it belongs to is still the authorized one.
// Only the second can go stale while a credential sits in a pane.
//
// Nil disables it, which leaves every pre-P5-A deployment and every test that
// does not wire it byte-for-byte unchanged.
type AgentAuthorityChecker interface {
	StillAuthorized(ctx context.Context, authority domain.AgentAuthority) (bool, error)
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
