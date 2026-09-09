package controllers

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

// skill_images.go — the administrative surface for the image trust root.
//
// It sits under /skills, which the global rule table already gates on
// settings.read for reads and settings.manage for writes. That is the correct
// gate and it needed no change: deciding that this installation will execute
// somebody's bytes is an installation-level act, and it is deliberately NOT
// something a project administrator can do for their own project.
//
// # What this surface is not
//
// It is not a way to run anything. Approving an image and running a skill are
// two separate decisions on two separate routes: an approval says "these bytes
// are acceptable here", and a run additionally needs the skill installed,
// activated on the project, its capabilities granted, and every control the
// mode requires actually attested. There is deliberately no route here that
// approves-and-runs, because a single click that did both would collapse the
// two decisions a reviewer is supposed to make separately.
//
// # What a client must send to approve
//
// A confirmation flag and a note. Neither is decoration: the flag is what a
// UI's confirmation step sets, and its absence is a refusal rather than a
// default; the note is what the administrator says they checked, and an
// approval with no stated reason is indistinguishable from a mistake six
// months later.

// SkillImageTrust is the service surface this controller needs.
type SkillImageTrust interface {
	ListImageApprovals(ctx context.Context) ([]SkillImageApprovalView, error)
	ApproveImage(ctx context.Context, in ApproveSkillImageInput) (SkillImageApprovalView, error)
	RevokeImageApproval(ctx context.Context, id string, in RevokeSkillImageInput) error
	RevocationPolicy() string
}

// ApproveSkillImageInput carries an approval from the controller to the
// service, including the caller's resolved INSTALLATION permissions. The
// service checks them; it does not re-derive authority.
type ApproveSkillImageInput struct {
	TenantID  domain.TenantID
	ProjectID domain.ProjectID
	SkillID   string
	Version   string
	ModeID    string
	Tool      string
	Reference string
	Digest    string
	// ExpiresInSeconds is optional. Zero means no expiry, which is allowed for
	// an image and not for a secret grant: a base image is a long-lived
	// artifact, and a forced date would be theatre rather than a control.
	ExpiresInSeconds int64
	Note             string
	Confirm          bool
	Actor            string
	ActorPermissions []domain.Permission
}

// RevokeSkillImageInput carries a revocation and the caller's authority.
type RevokeSkillImageInput struct {
	Actor            string
	ActorPermissions []domain.Permission
}

// ApproveSkillImageRequest is the wire body for an approval.
type ApproveSkillImageRequest struct {
	TenantID  string `json:"tenantId"`
	ProjectID string `json:"projectId"`
	SkillID   string `json:"skillId"`
	Version   string `json:"version"`
	ModeID    string `json:"modeId"`
	// Tool is the AO tool contract the image backs. An image approved to back
	// the static scan is not thereby approved to back a different tool.
	Tool string `json:"tool"`
	// Reference is the repository the digest came from, recorded for a human.
	// Nothing resolves through it.
	Reference string `json:"reference"`
	// Digest is sha256:<64 hex>. A tag is refused: it is a mutable pointer and
	// "the image we approved" has to mean one thing forever.
	Digest string `json:"digest"`
	// ExpiresInSeconds optionally bounds the approval. Omit for no expiry.
	ExpiresInSeconds int64 `json:"expiresInSeconds,omitempty"`
	// Note is what was checked. Required.
	Note string `json:"note"`
	// Confirm must be true. Approving an image is not a side effect of filling
	// in a form.
	Confirm bool `json:"confirm"`
}

// SkillImageApprovalView is one recorded decision.
type SkillImageApprovalView struct {
	ID        string `json:"id"`
	TenantID  string `json:"tenantId"`
	ProjectID string `json:"projectId"`
	SkillID   string `json:"skillId"`
	Version   string `json:"version"`
	ModeID    string `json:"modeId"`
	Tool      string `json:"tool"`
	Reference string `json:"reference"`
	Digest    string `json:"digest"`
	// ApprovedBy is the administrator. An approval nobody signed is one nobody
	// can be asked about, so this is never empty.
	ApprovedBy string `json:"approvedBy"`
	ApprovedAt string `json:"approvedAt"`
	ExpiresAt  string `json:"expiresAt,omitempty"`
	RevokedAt  string `json:"revokedAt,omitempty"`
	Note       string `json:"note"`
	// Active is the answer to "would this authorize a run right now", computed
	// rather than stored, so a client cannot read a stale row as permission.
	Active bool `json:"active"`
	// InactiveReason says which of expired and revoked happened. A view that
	// showed only Active:false would make them the same fact.
	InactiveReason string `json:"inactiveReason,omitempty"`
}

// SkillImageApprovalListResponse is the administrative listing.
type SkillImageApprovalListResponse struct {
	// Approvals includes revoked and expired entries: they are the history of
	// what this installation once allowed, and a list that hid them would
	// answer "what did we approve" with "what is approved right now".
	Approvals []SkillImageApprovalView `json:"approvals"`
	// TrustModel states, in the response itself, what an approval does and does
	// not mean. It is here rather than only in documentation because the client
	// rendering this list is where somebody will decide whether to trust it.
	TrustModel string `json:"trustModel"`
	// RevocationPolicy is the exact promise and non-promise of revoking.
	RevocationPolicy string `json:"revocationPolicy"`
}

// trustModelStatement is the one-paragraph version of ADR 0005 section 4.
const trustModelStatement = "An approval records that a named administrator of this installation " +
	"inspected these exact bytes and allowed this scope to execute them. It is NOT a publisher " +
	"signature: AO verifies none, contacts no registry and pulls nothing. An administrator who " +
	"approves a malicious digest has approved a malicious image."

// registerImageRoutes adds the trust-root surface. It is called from Register
// so the routes live beside the rest of the skills family and inherit its gate.
func (c *SkillsController) registerImageRoutes(r chi.Router) {
	r.Get("/skills/images", c.listImageApprovals)
	r.Post("/skills/images", c.approveImage)
	r.Delete("/skills/images/{approvalId}", c.revokeImageApproval)
}

func (c *SkillsController) listImageApprovals(w http.ResponseWriter, r *http.Request) {
	if c.Images == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/skills/images")
		return
	}
	approvals, err := c.Images.ListImageApprovals(r.Context())
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, SkillImageApprovalListResponse{
		Approvals:        approvals,
		TrustModel:       trustModelStatement,
		RevocationPolicy: c.Images.RevocationPolicy(),
	})
}

func (c *SkillsController) approveImage(w http.ResponseWriter, r *http.Request) {
	if c.Images == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/skills/images")
		return
	}
	var in ApproveSkillImageRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	view, err := c.Images.ApproveImage(r.Context(), ApproveSkillImageInput{
		TenantID:         domain.TenantID(in.TenantID),
		ProjectID:        domain.ProjectID(in.ProjectID),
		SkillID:          in.SkillID,
		Version:          in.Version,
		ModeID:           in.ModeID,
		Tool:             in.Tool,
		Reference:        in.Reference,
		Digest:           in.Digest,
		ExpiresInSeconds: in.ExpiresInSeconds,
		Note:             in.Note,
		Confirm:          in.Confirm,
		Actor:            c.actor(r),
		ActorPermissions: c.callerGlobalPermissions(r),
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusCreated, view)
}

func (c *SkillsController) revokeImageApproval(w http.ResponseWriter, r *http.Request) {
	if c.Images == nil {
		apispec.NotImplemented(w, r, http.MethodDelete, "/api/v1/skills/images/{approvalId}")
		return
	}
	if err := c.Images.RevokeImageApproval(r.Context(), chi.URLParam(r, "approvalId"),
		RevokeSkillImageInput{
			Actor:            c.actor(r),
			ActorPermissions: c.callerGlobalPermissions(r),
		}); err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, OKResponse{OK: true})
}

// callerGlobalPermissions resolves what the caller may do installation-wide.
//
// It follows callerProjectPermissions exactly, for the reason recorded there: a
// disabled guard is the default single-user desktop on the unauthenticated
// loopback listener, where returning an empty set would invent a new silent
// trust boundary on the one listener AGENTS.md says not to gate. When the guard
// IS enabled, an unresolvable subject yields nothing, which is the fail-closed
// answer for a multi-user install.
func (c *SkillsController) callerGlobalPermissions(r *http.Request) []domain.Permission {
	if !c.Guard.Enabled() {
		return domain.AllPermissions
	}
	sub, ok := c.Guard.Subject(r)
	if !ok {
		return nil
	}
	return sub.GlobalPermissions()
}

// SkillImageApprovalParams identify one recorded approval.
type SkillImageApprovalParams struct {
	ApprovalID string `path:"approvalId" description:"Image approval identifier."`
}
