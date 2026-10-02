package controllers

import (
	"net/http"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
)

// humanApprover is the person a human-approval route records, derived from the
// request's authenticated principal.
type humanApprover struct {
	Name       string
	UserID     domain.UserID
	AuthMethod domain.AuthMethod
}

// requireHumanApprover derives the approver of a human-approval route
// (acceptance-criterion amendment, integration fresh-review exception) from the
// principal the identity middleware resolved.
//
// AR-1a (D-SEC-3): these routes used to record whatever approvedBy the request
// body carried, so any caller holding workflow.run could attribute an approval
// to anybody. The approver is now WHO the transport authenticated:
//
//   - no principal at all -> 401: there is nobody to attribute it to;
//   - an agent principal  -> 403: an agent never approves a change to the bar
//     it is judged against, nor widens a guard on its own work;
//   - a body approvedBy that names someone else -> 422: the field survives only
//     for compatibility, as an assertion that must agree with the principal
//     (its username or user id); it can no longer choose the approver.
//
// A trusted-local principal is accepted and recorded AS trusted_local, never
// upgraded: on a desktop install a header-less loopback call resolves the
// bootstrap owner, and the audit trail must say that is how it was identified.
func requireHumanApprover(w http.ResponseWriter, r *http.Request, claimed string) (humanApprover, bool) {
	p, ok := identity.PrincipalFromContext(r.Context())
	if !ok || p.User.ID == "" {
		envelope.WriteAPIError(w, r, http.StatusUnauthorized, "unauthorized", "NOT_AUTHENTICATED",
			"an approval must be made by an authenticated person", nil)
		return humanApprover{}, false
	}
	if p.IsAgent() || p.AuthMethod == domain.AuthMethodAgent {
		envelope.WriteAPIError(w, r, http.StatusForbidden, "forbidden", "APPROVER_IS_AGENT",
			"an AO-launched agent cannot approve this; a person must", nil)
		return humanApprover{}, false
	}
	name := strings.TrimSpace(p.User.Username)
	if name == "" {
		name = string(p.User.ID)
	}
	if claimed = strings.TrimSpace(claimed); claimed != "" && claimed != name && claimed != string(p.User.ID) {
		envelope.WriteAPIError(w, r, http.StatusUnprocessableEntity, "unprocessable", "APPROVER_MISMATCH",
			"approvedBy is derived from the authenticated principal and cannot name someone else; omit it", nil)
		return humanApprover{}, false
	}
	switch p.AuthMethod {
	case domain.AuthMethodPassword, domain.AuthMethodOIDC, domain.AuthMethodTrustedLocal:
	default:
		// How the approver was identified is part of the record; a principal
		// that does not say is not one an approval can be attributed to.
		envelope.WriteAPIError(w, r, http.StatusUnauthorized, "unauthorized", "NOT_AUTHENTICATED",
			"the approver's authentication method could not be determined", nil)
		return humanApprover{}, false
	}
	return humanApprover{Name: name, UserID: p.User.ID, AuthMethod: p.AuthMethod}, true
}
