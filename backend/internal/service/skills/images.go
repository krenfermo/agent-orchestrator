package skills

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillimage"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// images.go — the durable trust root, and the one door through which an image
// becomes runnable.
//
// # Who may open it
//
// settings.manage, the same permission that gates handing a secret to a package
// (phase 5). Deciding that this installation will execute somebody's bytes is
// an installation-level act however narrow the scope, and it is deliberately
// NOT something a project maintainer can do for their own project.
//
// # What cannot open it
//
// A worker, a manifest, or an execution request. There is no path from "a run
// asked for this image" to "this image is approved": the authority is written
// through Approve, which takes an actor and their permissions, and read through
// ApprovedImage, which takes neither and can only ever narrow. A run that could
// widen its own approval would make the whole thing decorative.

// ImageStore is the persistence the trust root needs.
type ImageStore interface {
	UpsertSkillImageApproval(ctx context.Context, a skillimage.Approval) (skillimage.Approval, error)
	GetSkillImageApprovalForScope(ctx context.Context, scope skillimage.Scope, tool string) (skillimage.Approval, bool, error)
	GetSkillImageApproval(ctx context.Context, id string) (skillimage.Approval, bool, error)
	ListSkillImageApprovals(ctx context.Context) ([]skillimage.Approval, error)
	ListSkillImageApprovalsForProject(ctx context.Context, projectID domain.ProjectID) ([]skillimage.Approval, error)
	RevokeSkillImageApproval(ctx context.Context, id string, at time.Time) (bool, error)
}

// ImageAuditSink records approval decisions into the catalog's audit trail. It
// is the same trail that answers "who installed this package", because it is
// the same reviewer asking the same kind of question.
type ImageAuditSink interface {
	AppendSkillAudit(ctx context.Context, entry store.SkillAuditEntry) error
}

// ImageInspector reports what the host actually holds under a digest.
//
// It is the difference between an approval about bytes and an approval about a
// string somebody typed. Implemented by *skillrunner.Runner; nil on an
// installation with no container runtime, which is why Approve refuses rather
// than skipping the check — see verifyPresence.
type ImageInspector interface {
	VerifyImagePresent(ctx context.Context, digest string) (string, error)
}

// ImageAuthority is AO's trust root for container images.
type ImageAuthority struct {
	store   ImageStore
	audit   ImageAuditSink
	inspect ImageInspector
	now     func() time.Time
	newID   func() string
}

// WithImageInspector wires the host check Approve performs before recording a
// decision. Without it Approve refuses every approval: an unverifiable digest
// is a claim, and the trust root does not record claims.
func (a *ImageAuthority) WithImageInspector(in ImageInspector) *ImageAuthority {
	if a != nil {
		a.inspect = in
	}
	return a
}

// NewImageAuthority builds the trust root over a store.
func NewImageAuthority(st ImageStore, audit ImageAuditSink) *ImageAuthority {
	return &ImageAuthority{
		store: st, audit: audit,
		now:   func() time.Time { return time.Now().UTC() },
		newID: func() string { return "skimg-" + randomHex() },
	}
}

// Available reports whether the trust root can answer at all. A nil store is an
// installation with no trust root, which authorizes nothing rather than
// everything.
func (a *ImageAuthority) Available() bool { return a != nil && a.store != nil }

// ApproveRequest is one administrator's decision.
type ApproveRequest struct {
	Scope skillimage.Scope
	// Tool is the AO tool contract the image backs.
	Tool string
	// Reference is the repository the digest came from, for a human reader.
	Reference string
	// Digest is what will be run. Immutable, and the only thing resolution uses.
	Digest string
	// ExpiresIn optionally bounds the approval. Zero means no expiry, which is
	// allowed here and not for a secret grant: a base image is a long-lived
	// artifact, and a forced date would be theatre rather than a control.
	ExpiresIn time.Duration
	// Note is what the administrator says they checked. Required.
	Note string
	// Confirm must be true. Approving an image is not a side effect of filling
	// in a form: this field is what a UI's confirmation step sets, and its
	// absence is a refusal rather than a default.
	Confirm bool
	// Actor and ActorPermissions are the approver and what they hold.
	Actor            string
	ActorPermissions []domain.Permission
}

// Approve records that one scope may execute one digest.
//
// It does NOT run anything, and it does not enable a skill. Approving an image
// and executing a skill are two decisions: this one says "these bytes are
// acceptable to this installation", and a run additionally needs the skill
// installed, activated on the project, its capabilities granted, and every
// control the mode requires actually attested.
func (a *ImageAuthority) Approve(ctx context.Context, req ApproveRequest) (skillimage.Approval, error) {
	if err := a.requireAvailable(); err != nil {
		return skillimage.Approval{}, err
	}
	if !holdsPermission(req.ActorPermissions, domain.PermSettingsManage) {
		return skillimage.Approval{}, apierr.Forbidden("SKILL_IMAGE_APPROVAL_REFUSED",
			"approving a container image for execution requires the settings.manage permission")
	}
	if !req.Confirm {
		return skillimage.Approval{}, apierr.Invalid("SKILL_IMAGE_APPROVAL_UNCONFIRMED",
			"approving an image authorizes this installation to execute those bytes; "+
				"confirm the decision explicitly", nil)
	}
	if strings.TrimSpace(req.Actor) == "" {
		return skillimage.Approval{}, apierr.Invalid("SKILL_IMAGE_APPROVAL_ANONYMOUS",
			"an image approval records who made it; one nobody signed is one nobody can be asked about", nil)
	}

	now := a.now()
	approval := skillimage.Approval{
		ID: a.newID(), Scope: req.Scope, Tool: strings.TrimSpace(req.Tool),
		Reference: strings.TrimSpace(req.Reference), Digest: strings.TrimSpace(req.Digest),
		ApprovedBy: req.Actor, ApprovedAt: now, Note: strings.TrimSpace(req.Note),
	}
	if req.ExpiresIn > 0 {
		expires := now.Add(req.ExpiresIn)
		approval.ExpiresAt = &expires
	}
	if err := approval.Validate(); err != nil {
		return skillimage.Approval{}, apierr.Invalid("SKILL_IMAGE_APPROVAL_INVALID", err.Error(), nil)
	}
	if err := a.verifyPresence(ctx, approval.Digest); err != nil {
		return skillimage.Approval{}, err
	}

	stored, err := a.store.UpsertSkillImageApproval(ctx, approval)
	if err != nil {
		return skillimage.Approval{}, err
	}
	a.record(ctx, store.SkillAuditImageApproved, stored,
		fmt.Sprintf("approved %s to back %s: %s", stored.Digest, stored.Tool, stored.Note))
	return stored, nil
}

// Revoke withdraws an approval.
//
// What this does and does not do is stated in skillrunner.RevocationPolicy and
// must not be summarized more optimistically anywhere: it stops new executions
// immediately, it does not kill a container already running, and it does not
// recall a secret already delivered.
func (a *ImageAuthority) Revoke(ctx context.Context, id, actor string, perms []domain.Permission) error {
	if err := a.requireAvailable(); err != nil {
		return err
	}
	if !holdsPermission(perms, domain.PermSettingsManage) {
		return apierr.Forbidden("SKILL_IMAGE_REVOKE_REFUSED",
			"revoking an image approval requires the settings.manage permission")
	}
	existing, ok, err := a.store.GetSkillImageApproval(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return apierr.NotFound("SKILL_IMAGE_APPROVAL_NOT_FOUND", fmt.Sprintf("no image approval %q", id))
	}
	changed, err := a.store.RevokeSkillImageApproval(ctx, id, a.now())
	if err != nil {
		return err
	}
	if !changed {
		return apierr.Conflict("SKILL_IMAGE_APPROVAL_ALREADY_REVOKED",
			fmt.Sprintf("image approval %q was already revoked", id), nil)
	}
	a.record(ctx, store.SkillAuditImageRevoked, existing,
		fmt.Sprintf("revoked %s; running containers are not stopped and delivered secrets are not recalled",
			existing.Digest))
	return nil
}

// ApprovedImage answers "which image may this scope run", applying expiry and
// revocation AT THE MOMENT OF THE CALL.
//
// It satisfies skillrunner.ImageAuthority. It takes no actor and no
// permissions: it can only ever narrow, and there is deliberately no argument
// through which a run could widen what it is allowed to execute.
func (a *ImageAuthority) ApprovedImage(
	ctx context.Context, scope skillimage.Scope, tool string,
) (skillimage.Approval, error) {
	if err := a.requireAvailable(); err != nil {
		return skillimage.Approval{}, err
	}
	if err := scope.Validate(); err != nil {
		return skillimage.Approval{}, apierr.Invalid("SKILL_IMAGE_SCOPE_INVALID", err.Error(), nil)
	}
	approval, ok, err := a.store.GetSkillImageApprovalForScope(ctx, scope, tool)
	if err != nil {
		return skillimage.Approval{}, err
	}
	if !ok {
		return skillimage.Approval{}, apierr.Forbidden("SKILL_IMAGE_NOT_APPROVED",
			fmt.Sprintf("no image is approved for %s to back %s; an administrator must approve "+
				"a digest before this can run", scope, tool))
	}
	// Expiry and revocation are decided here, now, rather than in SQL. A query
	// that filtered them out would make "revoked" and "never approved" the same
	// answer, and an operator needs to know which one they are looking at.
	if reason := approval.InactiveReason(a.now()); reason != "" {
		return skillimage.Approval{}, apierr.Forbidden("SKILL_IMAGE_APPROVAL_INACTIVE",
			fmt.Sprintf("the image approved for %s to back %s is not usable: %s", scope, tool, reason))
	}
	return approval, nil
}

// ListApprovals returns every approval, newest decision first. Revoked and
// expired ones are included: they are the history of what this installation
// once allowed, and an administrative view that hid them would answer
// "what did we approve" with "what is approved right now".
func (a *ImageAuthority) ListApprovals(ctx context.Context) ([]skillimage.Approval, error) {
	if err := a.requireAvailable(); err != nil {
		return nil, err
	}
	out, err := a.store.ListSkillImageApprovals(ctx)
	if err != nil {
		return nil, err
	}
	sortApprovals(out)
	return out, nil
}

// ListApprovalsForProject narrows the listing to one project, which is how a
// project-scoped reader stays inside its own tenant's data.
func (a *ImageAuthority) ListApprovalsForProject(
	ctx context.Context, projectID domain.ProjectID,
) ([]skillimage.Approval, error) {
	if err := a.requireAvailable(); err != nil {
		return nil, err
	}
	out, err := a.store.ListSkillImageApprovalsForProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	sortApprovals(out)
	return out, nil
}

func sortApprovals(in []skillimage.Approval) {
	sort.SliceStable(in, func(i, j int) bool {
		if !in[i].ApprovedAt.Equal(in[j].ApprovedAt) {
			return in[i].ApprovedAt.After(in[j].ApprovedAt)
		}
		return in[i].ID < in[j].ID
	})
}

// verifyPresence refuses an approval for bytes this host cannot show AO.
//
// A digest in an approval request is CLIENT-SUPPLIED: it arrives from whoever
// filled in the form, and until something looks at the host it is a claim about
// an artifact, not the artifact. Recording it unchecked is the one thing a
// trust root must not do -- it would attest on the strength of its own input,
// and the mismatch would surface much later as a refused run that names no
// cause.
//
// It is a read. `image inspect` does not pull and does not start anything, so
// approving an image cannot become a way to make AO fetch or run one.
//
// No inspector, or an unusable runtime, is a REFUSAL rather than a skip. The
// alternative -- record it now, find out at run time -- is precisely the
// "trust what you were told" this exists to prevent, and an installation that
// cannot see the bytes is not one that can vouch for them.
func (a *ImageAuthority) verifyPresence(ctx context.Context, digest string) error {
	if a.inspect == nil {
		return apierr.Conflict("SKILL_IMAGE_UNVERIFIABLE",
			"this installation has no container runtime to verify the image against, so it "+
				"cannot approve one; a digest nobody can look at is a claim, not an artifact", nil)
	}
	effective, err := a.inspect.VerifyImagePresent(ctx, digest)
	if err != nil {
		return apierr.Invalid("SKILL_IMAGE_NOT_PRESENT",
			fmt.Sprintf("%s is not on this host, and AO does not pull to find out: %v", digest, err), nil)
	}
	if effective != digest {
		// The host holds something else under that name. Approving would record
		// a decision about bytes nobody reviewed.
		return apierr.Invalid("SKILL_IMAGE_DIGEST_MISMATCH",
			fmt.Sprintf("this host resolves %s to %s; AO approves the digest it can see and nothing else",
				digest, effective), nil)
	}
	return nil
}

func (a *ImageAuthority) requireAvailable() error {
	if !a.Available() {
		return apierr.Conflict("SKILL_IMAGE_TRUST_ROOT_UNAVAILABLE",
			"this installation has no image trust root configured, so no image is approved to run", nil)
	}
	return nil
}

// record writes one audit row. A failure to audit is deliberately not a failure
// to act: the write already happened, and returning an error here would tell
// the caller their approval did not land when it did. It is not silent either --
// the entry carries everything a later reader needs, and a dropped one shows up
// as a gap next to a row that exists.
func (a *ImageAuthority) record(
	ctx context.Context, action store.SkillAuditAction, approval skillimage.Approval, detail string,
) {
	if a.audit == nil {
		return
	}
	projectID := approval.Scope.ProjectID
	_ = a.audit.AppendSkillAudit(ctx, store.SkillAuditEntry{
		ID:         "skaud-" + randomHex(),
		OccurredAt: a.now(),
		Actor:      approval.ApprovedBy,
		Action:     action,
		SkillID:    approval.Scope.SkillID,
		Version:    approval.Scope.Version,
		ProjectID:  &projectID,
		Digest:     approval.Digest,
		Detail:     detail,
	})
}
