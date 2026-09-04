package controllers

import (
	"context"
	"errors"
	"net/http"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/authz"
)

// Authorizer is service/authz.Service as the HTTP layer needs it. Narrow on
// purpose: a controller may ASK whether something is permitted, and may resolve
// a subject to filter a list, and can do nothing else with authorization.
type Authorizer interface {
	Authorize(ctx context.Context, p domain.Principal, perm domain.Permission, res domain.AuthzResource) error
	Resolve(ctx context.Context, p domain.Principal) (authz.Subject, error)
	InstallationUnclaimed(ctx context.Context) (bool, error)
}

// ProjectScope resolves the project a session or a workflow run belongs to.
// Authorization for either is a question about its project: a person who may
// operate a project may operate the work inside it, and one who may not,
// may not.
type ProjectScope interface {
	GetSessionProjectID(ctx context.Context, id domain.SessionID) (domain.ProjectID, bool, error)
	GetWorkflowRunProjectID(ctx context.Context, id string) (domain.ProjectID, bool, error)
}

// Guard is the controllers' single entry point into authorization. Every
// project-, session- and run-scoped gate in this package goes through it, so
// there is one implementation of "deny" and one implementation of "what does
// a denial look like on the wire".
//
// A zero Guard (Authz nil) is DISABLED and allows everything. That is what
// keeps every pre-P4-B wiring -- the headless configurations and the many
// tests that construct a controller with no identity layer at all -- behaving
// exactly as it did; those setups fall back to the 8P-A ownership checks that
// were there before.
type Guard struct {
	Authz Authorizer
	Scope ProjectScope
}

// Enabled reports whether authorization is wired.
func (g Guard) Enabled() bool { return g.Authz != nil }

// Subject resolves the request's authority once, for callers that need to
// filter a list rather than ask a single question. ok is false when
// authorization is disabled or no principal resolved.
func (g Guard) Subject(r *http.Request) (authz.Subject, bool) {
	if !g.Enabled() {
		return authz.Subject{}, false
	}
	p, err := identity.RequirePrincipal(r)
	if err != nil {
		return authz.Subject{}, false
	}
	sub, err := g.Authz.Resolve(r.Context(), p)
	if err != nil {
		return authz.Subject{}, false
	}
	return sub, true
}

// AllowGlobal gates an installation-wide operation. A denial is a plain 403:
// there is no resource whose existence could leak, and telling an
// authenticated person "you may not manage users" is strictly more useful than
// pretending the route does not exist.
func (g Guard) AllowGlobal(w http.ResponseWriter, r *http.Request, perm domain.Permission) bool {
	if !g.Enabled() {
		return true
	}
	if g.unclaimed(r.Context()) {
		return true
	}
	p, err := identity.RequirePrincipal(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return false
	}
	if err := g.Authz.Authorize(r.Context(), p, perm, domain.GlobalResource()); err != nil {
		envelope.WriteError(w, r, err)
		return false
	}
	return true
}

// AllowProject gates an operation on one project.
//
// A denial reports 404, never 403 -- the convention every ownership check in
// this package has followed since 8P-A. A project id the caller cannot reach
// must be indistinguishable from one that does not exist, or the API becomes
// an oracle for "does this installation have a project with this id".
// Unauthenticated is still 401: that is a statement about the caller, not
// about the resource.
func (g Guard) AllowProject(w http.ResponseWriter, r *http.Request, perm domain.Permission, id domain.ProjectID, code, message string) bool {
	if !g.Enabled() {
		return true
	}
	if g.unclaimed(r.Context()) {
		return true
	}
	p, err := identity.RequirePrincipal(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return false
	}
	if err := g.Authz.Authorize(r.Context(), p, perm, domain.ProjectResource(id)); err != nil {
		if isUnauthorizedErr(err) {
			envelope.WriteError(w, r, err)
			return false
		}
		// Mask as 404 only when the caller cannot SEE the project at all.
		// Once they can, the project's existence is not a secret from them and
		// answering 404 to "you may not cancel this run" would be a lie that
		// costs a person an hour of debugging. A caller who can read is told
		// what they actually lack.
		if readErr := g.Authz.Authorize(r.Context(), p, domain.PermProjectRead, domain.ProjectResource(id)); readErr == nil {
			envelope.WriteError(w, r, err)
			return false
		}
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", code, message, nil)
		return false
	}
	return true
}

// AllowSession gates an operation on one session, by way of the project it
// belongs to. A session whose project cannot be resolved is denied: an
// un-attributable session fails closed, matching the rule 8P-B.2 established
// for an un-owned one.
func (g Guard) AllowSession(w http.ResponseWriter, r *http.Request, perm domain.Permission, id domain.SessionID) bool {
	if !g.Enabled() {
		return true
	}
	if g.unclaimed(r.Context()) {
		return true
	}
	if g.Scope == nil {
		return true
	}
	project, ok, err := g.Scope.GetSessionProjectID(r.Context(), id)
	if err != nil || !ok || project == "" {
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "SESSION_NOT_FOUND", "session not found", nil)
		return false
	}
	return g.AllowProject(w, r, perm, project, "SESSION_NOT_FOUND", "session not found")
}

// AllowWorkflowRun gates an operation on one workflow run, by way of its
// project.
func (g Guard) AllowWorkflowRun(w http.ResponseWriter, r *http.Request, perm domain.Permission, id string) bool {
	if !g.Enabled() {
		return true
	}
	if g.unclaimed(r.Context()) {
		return true
	}
	if g.Scope == nil {
		return true
	}
	project, ok, err := g.Scope.GetWorkflowRunProjectID(r.Context(), id)
	if err != nil || !ok || project == "" {
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "WORKFLOW_NOT_FOUND", "workflow run not found", nil)
		return false
	}
	return g.AllowProject(w, r, perm, project, "WORKFLOW_NOT_FOUND", "workflow run not found")
}

// CanProject answers the same question as AllowProject without writing a
// response, for list filtering where a denial means "omit the row", not
// "fail the request".
func (g Guard) CanProject(ctx context.Context, sub authz.Subject, perm domain.Permission, id domain.ProjectID) bool {
	return sub.Allows(perm, domain.ProjectResource(id))
}

// CanSessionRead reports whether a subject may read one session, resolving its
// project. Used by list filters; a session whose project cannot be resolved is
// omitted rather than surfaced.
func (g Guard) CanSessionRead(ctx context.Context, sub authz.Subject, perm domain.Permission, id domain.SessionID) bool {
	if g.Scope == nil {
		return true
	}
	project, ok, err := g.Scope.GetSessionProjectID(ctx, id)
	if err != nil || !ok || project == "" {
		return false
	}
	return sub.Allows(perm, domain.ProjectResource(project))
}

// CanRun reports whether a subject may act on one workflow run, resolving its
// project. Used by list filters, where a denial means "omit the row". With no
// scope resolver wired it allows, matching AllowWorkflowRun -- a daemon that
// cannot resolve a run's project has no authorization opinion about it, and
// silently emptying a list would be a worse answer than the pre-P4-B one.
func (g Guard) CanRun(ctx context.Context, sub authz.Subject, perm domain.Permission, id string) bool {
	if g.Scope == nil {
		return true
	}
	project, ok, err := g.Scope.GetWorkflowRunProjectID(ctx, id)
	if err != nil || !ok || project == "" {
		return false
	}
	return sub.Allows(perm, domain.ProjectResource(project))
}

// unclaimed reports whether the installation has no accounts yet. Before the
// first account exists there is nobody to authorize: a fresh desktop install
// must reach its first-run screen with every route behaving as it did before
// P4-B. A lookup failure answers false, so an unreadable database fails closed.
func (g Guard) unclaimed(ctx context.Context) bool {
	unclaimed, err := g.Authz.InstallationUnclaimed(ctx)
	return err == nil && unclaimed
}

// isUnauthorizedErr distinguishes "never authenticated" from "authenticated
// but not permitted". Only the latter is masked as a 404: masking a 401 would
// tell an anonymous caller that a project exists whenever it does not.
func isUnauthorizedErr(err error) bool {
	var e *apierr.Error
	return errors.As(err, &e) && e.Kind == apierr.KindUnauthorized
}
