package controllers

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

// skill_external.go -- the surface for external (forge-backed) registries.
//
// It is SMALL on purpose. Discovery, resolution and installation all go
// through the marketplace routes that already exist, because an external
// registry is a registry and a second install route for "the risky kind" would
// be a second place for a check to be missing.
//
// What is here is the one thing an external registry needs and no other kind
// does: an ADMINISTRATIVE REVOCATION LIST. A forge publishes no revocation
// feed -- there is no endpoint AO could poll to learn a repository was
// compromised -- so this is where somebody here writes down what this
// installation will no longer install from, and where they can take it back
// when a repository is re-vetted.

// SkillExternal is the external-registry surface.
//
// A fifth port beside Catalog, Images, Marketplace and Trust, separate for the
// same reason those are separate from each other: an installation can have all
// four and never configure a forge, and a port it does not implement answers
// 501 rather than pretending.
type SkillExternal interface {
	ExternalRevocationsView(ctx context.Context) (SkillExternalRevocationListResponse, error)
	RevokeExternalView(ctx context.Context, in RevokeSkillExternalInput) (SkillExternalRevocationView, error)
	LiftExternalRevocationView(ctx context.Context, in LiftSkillExternalRevocationInput) error
	MovedTagsView(ctx context.Context, tenants []domain.TenantID) (SkillMovedTagListResponse, error)
}

// RevokeSkillExternalInput carries one administrative withdrawal.
type RevokeSkillExternalInput struct {
	Subject   string
	SubjectID string
	Reason    string

	Actor            string
	ActorPermissions []domain.Permission
}

// LiftSkillExternalRevocationInput removes one.
type LiftSkillExternalRevocationInput struct {
	Subject   string
	SubjectID string

	Actor            string
	ActorPermissions []domain.Permission
}

// SkillExternalRevocationView is one administrative withdrawal.
type SkillExternalRevocationView struct {
	// Subject is what was withdrawn: an account, a repository, or one exact
	// commit. Three different blast radii, and the narrowest is the most
	// useful during an incident -- a repository that shipped one bad release
	// stays installable at every other commit.
	Subject string `json:"subject" enum:"external_owner,external_repository,external_commit"`
	// SubjectID is the owner, "owner/repository", or "owner/repository@<sha>".
	SubjectID string    `json:"subjectId"`
	Reason    string    `json:"reason"`
	RevokedAt time.Time `json:"revokedAt"`
	RevokedBy string    `json:"revokedBy,omitempty"`
}

// SkillExternalRevocationListResponse is the body of
// GET /api/v1/skills/external/revocations.
type SkillExternalRevocationListResponse struct {
	Revocations []SkillExternalRevocationView `json:"revocations"`
	// Policy states, in the daemon's own words, what an external revocation
	// does and does not do. It is served rather than written in a UI because
	// the comfortable version of this sentence -- "revoked" -- reads as
	// "removed", and nothing here removes anything.
	Policy string `json:"policy"`
}

// RevokeSkillExternalRequest is the wire body for creating one.
type RevokeSkillExternalRequest struct {
	Subject   string `json:"subject" enum:"external_owner,external_repository,external_commit"`
	SubjectID string `json:"subjectId"`
	Reason    string `json:"reason"`
}

// LiftSkillExternalRevocationRequest is the wire body for removing one.
type LiftSkillExternalRevocationRequest struct {
	Subject   string `json:"subject" enum:"external_owner,external_repository,external_commit"`
	SubjectID string `json:"subjectId"`
}

// SkillMovedTagView is one tag AO has seen point somewhere else.
type SkillMovedTagView struct {
	RegistryID string `json:"registryId"`
	Owner      string `json:"owner"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	// Commit is where the tag points now; PreviousCommit is where AO recorded
	// it. Both in full: deciding two commits are different needs every
	// character, and this is exactly the moment somebody compares them against
	// a repository page.
	Commit         string     `json:"commit"`
	PreviousCommit string     `json:"previousCommit"`
	MovedAt        *time.Time `json:"movedAt,omitempty"`
	FirstSeenAt    time.Time  `json:"firstSeenAt"`
	// Explanation is the daemon's sentence, including the part that says
	// nothing already installed was changed.
	Explanation string `json:"explanation"`
}

// SkillMovedTagListResponse is the body of GET /api/v1/skills/external/tags.
type SkillMovedTagListResponse struct {
	Tags []SkillMovedTagView `json:"tags"`
	// Policy is what AO does about a moved tag, from the daemon.
	Policy string `json:"policy"`
}

// SkillExternalRevocationPolicy is the exact promise and non-promise.
const SkillExternalRevocationPolicy = "A forge publishes no revocation feed, so this list is " +
	"this installation's own decision rather than something AO was told. Withdrawing an account, " +
	"a repository or a commit blocks NEW installs from it. It does not uninstall anything, delete " +
	"any files, disable any skill on any project, or stop a run already under way -- deciding what " +
	"to do about a package that is already here is a person's call, one package at a time."

// SkillMovedTagPolicy is what AO does when a tag moves.
const SkillMovedTagPolicy = "A tag is a name somebody can re-point; the commit is the release. " +
	"When a tag AO recorded starts pointing at a different commit, AO says so and refuses to " +
	"install through it silently -- taking the new commit is an explicit decision. Nothing already " +
	"installed is changed: its provenance still records the commit its bytes actually came from, " +
	"because that is what happened."

// ExternalHostingNotice is shown before an install from a forge.
//
// The daemon owns the sentence because the comfortable version of it is "from
// GitHub", which reads to almost everybody as an endorsement.
const ExternalHostingNotice = "Source: GitHub. Hosting on GitHub does not mean AO trusts the " +
	"publisher. AO checks that the bytes it fetched are the bytes the release named, at one exact " +
	"commit; it does not know who wrote them unless a signature chains to a trust root you " +
	"configured. Installing changes nothing else: no project is enabled, no capability is granted, " +
	"no image is approved and nothing is executed."

// registerExternalRoutes mounts the external surface. Same /skills family and
// the same gate: settings.read to look, settings.manage to change.
func (c *SkillsController) registerExternalRoutes(r chi.Router) {
	r.Get("/skills/external/revocations", c.listExternalRevocations)
	r.Post("/skills/external/revocations", c.revokeExternal)
	r.Post("/skills/external/revocations/lift", c.liftExternalRevocation)
	r.Get("/skills/external/tags", c.listMovedTags)
}

func (c *SkillsController) listExternalRevocations(w http.ResponseWriter, r *http.Request) {
	if c.External == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/skills/external/revocations")
		return
	}
	out, err := c.External.ExternalRevocationsView(r.Context())
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

// revokeExternal is deliberately the same shape as revokeTrust, on a
// deliberately separate port. Merging them would couple "what an administrator
// withdrew about a key" to "what they withdrew about a place", which are the
// two halves this phase spent a migration keeping apart.
//
//nolint:dupl // two ports, one HTTP shape; sharing the handler would share the ports.
func (c *SkillsController) revokeExternal(w http.ResponseWriter, r *http.Request) {
	if c.External == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/skills/external/revocations")
		return
	}
	var in RevokeSkillExternalRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON",
			"Invalid JSON body", nil)
		return
	}
	view, err := c.External.RevokeExternalView(r.Context(), RevokeSkillExternalInput{
		Subject:          in.Subject,
		SubjectID:        in.SubjectID,
		Reason:           in.Reason,
		Actor:            c.actor(r),
		ActorPermissions: c.callerGlobalPermissions(r),
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, view)
}

func (c *SkillsController) liftExternalRevocation(w http.ResponseWriter, r *http.Request) {
	if c.External == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/skills/external/revocations/lift")
		return
	}
	var in LiftSkillExternalRevocationRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON",
			"Invalid JSON body", nil)
		return
	}
	if err := c.External.LiftExternalRevocationView(r.Context(), LiftSkillExternalRevocationInput{
		Subject:          in.Subject,
		SubjectID:        in.SubjectID,
		Actor:            c.actor(r),
		ActorPermissions: c.callerGlobalPermissions(r),
	}); err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, OKResponse{OK: true})
}

func (c *SkillsController) listMovedTags(w http.ResponseWriter, r *http.Request) {
	if c.External == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/skills/external/tags")
		return
	}
	out, err := c.External.MovedTagsView(r.Context(), c.callerTenants(r))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}
