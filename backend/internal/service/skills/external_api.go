package skills

import (
	"context"
	"fmt"
	"sort"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// external_api.go -- the projection of the external surface onto the
// controller's DTOs.
//
// # What this layer decides not to send
//
// Nothing about a credential, as everywhere else on this family. And nothing
// that would let a client compute a trust state of its own: the sentences
// below are the daemon's, because the comfortable rewrite of every one of them
// is the dangerous one. "Revoked" reads as "removed" and nothing here removes
// anything; "moved" reads as "updated" and an updated tag is precisely the
// substitution; "from GitHub" reads as an endorsement.

// ExternalAuthority is the controller port for the external surface.
//
// It is a small type over the two things that own the state -- the trust
// authority holds the administrative revocations, and the marketplace holds
// the tag ledger -- rather than a sixth service. Two owners, one port, because
// a screen asks both questions at once and a client stitching two responses
// together is a client that renders half an answer.
type ExternalAuthority struct {
	trust       *TrustAuthority
	marketplace *Marketplace
}

// NewExternalAuthority wires the port.
func NewExternalAuthority(trust *TrustAuthority, marketplace *Marketplace) *ExternalAuthority {
	return &ExternalAuthority{trust: trust, marketplace: marketplace}
}

// Available reports whether the surface can answer at all.
func (e *ExternalAuthority) Available() bool {
	return e != nil && e.trust.Available()
}

func (e *ExternalAuthority) requireAvailable() error {
	if !e.Available() {
		return apierr.Invalid("SKILL_EXTERNAL_UNAVAILABLE",
			"this installation has no skill trust store, so it cannot record what it will refuse "+
				"to install from", nil)
	}
	return nil
}

// ExternalRevocationsView implements the controller's revocation list.
func (e *ExternalAuthority) ExternalRevocationsView(
	ctx context.Context,
) (controllers.SkillExternalRevocationListResponse, error) {
	out := controllers.SkillExternalRevocationListResponse{
		Revocations: []controllers.SkillExternalRevocationView{},
		Policy:      controllers.SkillExternalRevocationPolicy,
	}
	if err := e.requireAvailable(); err != nil {
		return out, err
	}
	rows, err := e.trust.ListExternalRevocations(ctx)
	if err != nil {
		return controllers.SkillExternalRevocationListResponse{}, err
	}
	for _, rev := range rows {
		out.Revocations = append(out.Revocations, externalRevocationView(rev))
	}
	return out, nil
}

// RevokeExternalView implements the controller's revoke.
func (e *ExternalAuthority) RevokeExternalView(
	ctx context.Context, in controllers.RevokeSkillExternalInput,
) (controllers.SkillExternalRevocationView, error) {
	if err := e.requireAvailable(); err != nil {
		return controllers.SkillExternalRevocationView{}, err
	}
	rev, err := e.trust.RevokeExternal(ctx, ExternalRevokeRequest{
		Subject:          skillregistry.RevocationSubject(in.Subject),
		SubjectID:        in.SubjectID,
		Reason:           in.Reason,
		Actor:            in.Actor,
		ActorPermissions: in.ActorPermissions,
	})
	if err != nil {
		return controllers.SkillExternalRevocationView{}, err
	}
	return externalRevocationView(rev), nil
}

// LiftExternalRevocationView implements the controller's lift.
func (e *ExternalAuthority) LiftExternalRevocationView(
	ctx context.Context, in controllers.LiftSkillExternalRevocationInput,
) error {
	if err := e.requireAvailable(); err != nil {
		return err
	}
	return e.trust.LiftExternalRevocation(ctx,
		skillregistry.RevocationSubject(in.Subject), in.SubjectID,
		in.Actor, in.ActorPermissions)
}

// MovedTagsView implements the controller's moved-tag list.
//
// It is filtered to the registries the CALLER can see, for the same reason
// every other registry read is: a tenant-scoped registry's existence is itself
// information, and a moved-tag row naming a repository would leak which
// organizations are reading which forges.
func (e *ExternalAuthority) MovedTagsView(
	ctx context.Context, tenants []domain.TenantID,
) (controllers.SkillMovedTagListResponse, error) {
	out := controllers.SkillMovedTagListResponse{
		Tags:   []controllers.SkillMovedTagView{},
		Policy: controllers.SkillMovedTagPolicy,
	}
	if e == nil || e.marketplace == nil || !e.marketplace.externalAvailable() {
		return out, nil
	}
	visible, err := e.marketplace.ListRegistries(ctx, tenants)
	if err != nil {
		return controllers.SkillMovedTagListResponse{}, err
	}
	allowed := map[string]bool{}
	for _, reg := range visible {
		allowed[reg.ID] = true
	}
	rows, err := e.marketplace.external.ListMovedSkillExternalTags(ctx)
	if err != nil {
		return controllers.SkillMovedTagListResponse{}, err
	}
	for _, row := range rows {
		if !allowed[row.RegistryID] {
			continue
		}
		out.Tags = append(out.Tags, movedTagView(row))
	}
	sort.SliceStable(out.Tags, func(i, j int) bool {
		left, right := out.Tags[i], out.Tags[j]
		if left.MovedAt != nil && right.MovedAt != nil && !left.MovedAt.Equal(*right.MovedAt) {
			return left.MovedAt.After(*right.MovedAt)
		}
		return left.Owner+"/"+left.Repository+"#"+left.Tag <
			right.Owner+"/"+right.Repository+"#"+right.Tag
	})
	return out, nil
}

func externalRevocationView(
	rev skillregistry.TrustRevocation,
) controllers.SkillExternalRevocationView {
	return controllers.SkillExternalRevocationView{
		Subject:   string(rev.Subject),
		SubjectID: rev.SubjectID,
		Reason:    rev.Reason,
		RevokedAt: rev.RevokedAt,
		RevokedBy: rev.RevokedBy,
	}
}

func movedTagView(row store.SkillExternalTag) controllers.SkillMovedTagView {
	return controllers.SkillMovedTagView{
		RegistryID:     row.RegistryID,
		Owner:          row.Owner,
		Repository:     row.Repository,
		Tag:            row.Tag,
		Commit:         row.Commit,
		PreviousCommit: row.MovedFromCommit,
		MovedAt:        row.MovedAt,
		FirstSeenAt:    row.FirstObservedAt,
		// The daemon's sentence, in full, with both SHAs.
		Explanation: skillregistry.DescribeTagMove(row),
	}
}

// ExternalIdentityNotice renders the identity sentence for one release, for a
// client that is about to offer an install.
//
// It exists so the pre-install screen can say WHO, in the same words the
// installed row will use afterwards. A screen that phrased it one way before
// and another way after would be a screen where the install appeared to
// establish something.
func ExternalIdentityNotice(rel skillregistry.Release, reg skillregistry.Registry) string {
	if !rel.Source.Declared() {
		return ""
	}
	return fmt.Sprintf("%s %s", controllers.ExternalHostingNotice,
		skillregistry.ExternalIdentityOf(rel, reg).Describe())
}
