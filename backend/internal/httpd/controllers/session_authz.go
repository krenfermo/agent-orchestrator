package controllers

import (
	"context"
	"net/http"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
)

// SessionOwnershipStore backs Checkpoint 8P-B.1/8P-B.2's session ownership
// scoping, narrow like ProjectsController's OwnershipStore. Any controller
// that reads/writes content or control for one specific session (not just
// session-id metadata) must be wired with this and gate every such handler
// through AuthorizeSessionAccess -- see session_authz.go's doc comment.
type SessionOwnershipStore interface {
	GetSessionOwner(ctx context.Context, id domain.SessionID) (*domain.UserID, error)
}

// SessionScoping bundles the two knobs every controller that touches
// session content/control needs, so each controller struct embeds one
// field instead of two, and every call site passes the exact same pair to
// AuthorizeSessionAccess. Zero value (nil Ownership) is scoping-disabled,
// matching every other optional-dependency convention in this package.
type SessionScoping struct {
	Ownership    SessionOwnershipStore
	TrustedLocal bool
}

func (s SessionScoping) enforced() bool {
	return !s.TrustedLocal && s.Ownership != nil
}

// AuthorizeSessionAccess is Checkpoint 8P-B.2's single canonical session
// authorization boundary. Every controller/handler that reads or mutates
// one specific session's content or control (get, send, attach/stream,
// preview, workspace files, switch-agent, restore/resume/kill/relaunch,
// conversation turns, reviews, per-session usage, ...) must call this
// before doing anything else, and never re-derive the ownership check
// inline.
//
// It writes the locked 404 envelope (never 403 -- existence must not leak)
// and returns false when access is denied. Denial happens for:
//   - scoping disabled (trusted-local mode, or Ownership not wired): never
//     denies -- returns true immediately, exactly like every pre-8P-B.1/
//     8P-B.2 configuration behaved.
//   - no session cookie/identity resolved in multi-user mode: 401 via
//     identity.Require's own error, not a 404 (never authenticated at all,
//     vs. authenticated-but-forbidden).
//   - a lookup error: fails closed (denied), never serves a resource whose
//     ownership couldn't be verified.
//   - a session with NO owner (nil owner_user_id -- a pre-0111 legacy
//     session, or one spawned while no owner could be resolved) in
//     multi-user mode: DENIED. This is deliberately stricter than
//     projects/workflow_runs' "unowned is visible to everyone" convention
//     (8P-A) -- a session carries live provider credentials/content, so an
//     un-attributable session must fail closed rather than fail open once
//     multi-user mode is active (Checkpoint 8P-B.2 §18/§24). Trusted-local
//     mode is completely unaffected (scoping is disabled there regardless
//     of owner), so no legacy desktop session becomes unreachable.
//   - a session owned by a DIFFERENT user: denied.
func AuthorizeSessionAccess(w http.ResponseWriter, r *http.Request, scoping SessionScoping, id domain.SessionID) bool {
	if !scoping.enforced() {
		return true
	}
	user, err := identity.Require(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return false
	}
	owner, err := scoping.Ownership.GetSessionOwner(r.Context(), id)
	if err != nil || owner == nil || *owner != user.ID {
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "SESSION_NOT_FOUND", "session not found", nil)
		return false
	}
	return true
}
