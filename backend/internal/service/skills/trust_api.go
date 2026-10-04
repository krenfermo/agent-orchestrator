package skills

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// trust_api.go -- the API-shaped half of the trust authority.
//
// It is the same split marketplace_api.go makes and for the same reason: the
// controller owns the wire shapes and this file translates, so a DTO change
// does not reach into the code that decides whether a signature verifies.
//
// Every value that crosses here is PUBLIC. There is no field on any of these
// views that a private key or a credential could occupy, and no input that
// would accept one.

// TrustView is the whole Trust settings read.
func (a *TrustAuthority) TrustView(ctx context.Context) (controllers.SkillTrustListResponse, error) {
	roots, err := a.ListTrustRoots(ctx)
	if err != nil {
		return controllers.SkillTrustListResponse{}, err
	}
	revs, err := a.ListTrustRevocations(ctx)
	if err != nil {
		return controllers.SkillTrustListResponse{}, err
	}
	out := controllers.SkillTrustListResponse{
		Roots:                 make([]controllers.SkillTrustRootView, 0, len(roots)),
		Revocations:           make([]controllers.SkillTrustRevocationView, 0, len(revs)),
		TrustModel:            controllers.SkillTrustModelStatement,
		OfficialRootAvailable: a.HasOfficialRoot(),
	}
	if !out.OfficialRootAvailable {
		// Said in words rather than left as a false flag, because a screen
		// that just greyed out the official policy would leave a person
		// guessing whether it was broken or deliberate.
		out.OfficialRootNote = "This build carries no AO Official trust root. AO's official signing " +
			"key has not been published yet, and embedding a placeholder would mean anybody " +
			"reading AO's source could sign packages this screen would call official. A registry " +
			"on the \"official\" trust policy installs nothing until a real root ships; a registry " +
			"using an enterprise root configured here reaches trusted today."
	}
	for _, d := range roots {
		out.Roots = append(out.Roots, trustRootView(d))
	}
	for _, r := range revs {
		out.Revocations = append(out.Revocations, controllers.SkillTrustRevocationView{
			Subject:   string(r.Subject),
			SubjectID: r.SubjectID,
			Reason:    r.Reason,
			RevokedAt: r.RevokedAt,
			RevokedBy: r.RevokedBy,
		})
	}
	return out, nil
}

// SaveTrustRootView adapts the controller's input.
func (a *TrustAuthority) SaveTrustRootView(
	ctx context.Context, in controllers.SaveSkillTrustRootInput,
) (controllers.SkillTrustRootView, error) {
	root, err := a.SaveTrustRoot(ctx, TrustRootRequest{
		ID:               in.ID,
		DisplayName:      in.DisplayName,
		Publisher:        in.Publisher,
		ValidFrom:        in.ValidFrom,
		ValidUntil:       in.ValidUntil,
		Actor:            in.Actor,
		ActorPermissions: in.ActorPermissions,
	})
	if err != nil {
		return controllers.SkillTrustRootView{}, err
	}
	keys, err := a.store.ListSkillSigningKeysForRoot(ctx, root.ID)
	if err != nil {
		return controllers.SkillTrustRootView{}, err
	}
	sortKeys(keys)
	return trustRootView(TrustRootDetail{Root: root, Keys: keys}), nil
}

// AddSigningKeyView adapts the controller's input.
//
// The certificate is translated field by field rather than decoded from the
// request body directly, so a client cannot smuggle a field this build does
// not know into the thing whose signature is about to be checked.
func (a *TrustAuthority) AddSigningKeyView(
	ctx context.Context, in controllers.AddSkillSigningKeyInput,
) (controllers.SkillSigningKeyView, error) {
	req := SigningKeyRequest{
		KeyID:            in.KeyID,
		TrustRootID:      in.TrustRootID,
		PublicKey:        in.PublicKey,
		IsRootKey:        in.IsRootKey,
		ValidFrom:        in.ValidFrom,
		ValidUntil:       in.ValidUntil,
		RotatedFromKeyID: in.RotatedFromKeyID,
		Actor:            in.Actor,
		ActorPermissions: in.ActorPermissions,
	}
	if c := in.Certificate; c != nil {
		req.Certificate = &skillregistry.KeyCertificate{
			Scheme:      skillregistry.SignatureScheme(c.Scheme),
			TrustRootID: c.TrustRootID,
			RootKeyID:   c.RootKeyID,
			KeyID:       c.KeyID,
			PublicKey:   c.PublicKey,
			Publisher:   c.Publisher,
			ValidFrom:   c.ValidFrom,
			ValidUntil:  c.ValidUntil,
			Value:       c.Value,
		}
	}
	key, err := a.AddSigningKey(ctx, req)
	if err != nil {
		return controllers.SkillSigningKeyView{}, err
	}
	return signingKeyView(key), nil
}

// RetireSigningKeyView adapts the controller's input.
func (a *TrustAuthority) RetireSigningKeyView(
	ctx context.Context, in controllers.RetireSkillSigningKeyInput,
) error {
	return a.RetireSigningKey(ctx, in.KeyID, in.ValidUntil, in.Actor, in.ActorPermissions)
}

// RevokeTrustView adapts the controller's input.
func (a *TrustAuthority) RevokeTrustView(
	ctx context.Context, in controllers.RevokeSkillTrustInput,
) (controllers.SkillTrustRevocationView, error) {
	subject := skillregistry.RevocationSubject(in.Subject)
	if !subject.Valid() {
		return controllers.SkillTrustRevocationView{}, apierr.Invalid("SKILL_TRUST_REVOKE_SUBJECT",
			"subject must be one of signing_key, publisher, trust_root", nil)
	}
	rev, err := a.Revoke(ctx, RevokeRequest{
		Subject: subject, SubjectID: in.SubjectID, Reason: in.Reason,
		Actor: in.Actor, ActorPermissions: in.ActorPermissions,
	})
	if err != nil {
		return controllers.SkillTrustRevocationView{}, err
	}
	return controllers.SkillTrustRevocationView{
		Subject:   string(rev.Subject),
		SubjectID: rev.SubjectID,
		Reason:    rev.Reason,
		RevokedAt: rev.RevokedAt,
		RevokedBy: rev.RevokedBy,
	}, nil
}

func trustRootView(d TrustRootDetail) controllers.SkillTrustRootView {
	keys := make([]controllers.SkillSigningKeyView, 0, len(d.Keys))
	for _, k := range d.Keys {
		keys = append(keys, signingKeyView(k))
	}
	return controllers.SkillTrustRootView{
		TrustRootID:      d.Root.ID,
		Tier:             string(d.Root.Tier),
		DisplayName:      d.Root.DisplayName,
		Publisher:        d.Root.Publisher,
		Status:           string(d.Root.Status),
		ValidFrom:        d.Root.ValidFrom,
		ValidUntil:       d.Root.ValidUntil,
		RevokedAt:        d.Root.RevokedAt,
		RevocationReason: d.Root.RevocationReason,
		BuiltIn:          d.BuiltIn,
		Keys:             keys,
		CreatedAt:        d.Root.CreatedAt,
		CreatedBy:        d.Root.CreatedBy,
		UpdatedAt:        d.Root.UpdatedAt,
		UpdatedBy:        d.Root.UpdatedBy,
	}
}

func signingKeyView(k skillregistry.SigningKey) controllers.SkillSigningKeyView {
	return controllers.SkillSigningKeyView{
		KeyID:       k.KeyID,
		TrustRootID: k.TrustRootID,
		Publisher:   k.Publisher,
		IsRootKey:   k.IsRootKey,
		Algorithm:   k.Algorithm,
		// The PUBLIC key, served so an operator can compare it against what
		// the publisher published somewhere else. There is no private
		// counterpart in this process to serve by mistake.
		PublicKey:        k.PublicKey,
		Fingerprint:      k.Fingerprint,
		FingerprintShort: skillregistry.ShortFingerprint(k.Fingerprint),
		Origin:           string(k.Origin),
		Status:           string(k.Status),
		ValidFrom:        k.ValidFrom,
		ValidUntil:       k.ValidUntil,
		RevokedAt:        k.RevokedAt,
		RevocationReason: k.RevocationReason,
		RotatedFromKeyID: k.RotatedFromKeyID,
		CreatedAt:        k.CreatedAt,
		CreatedBy:        k.CreatedBy,
	}
}

// provenanceView renders a verification for the API, or nil when AO checked no
// signature at all.
//
// nil and "verified: false" are different answers and must stay that way: the
// first means there was nothing to check, the second means AO checked and said
// no, and a client that rendered them the same would let a refused signature
// read as an ordinary unsigned install.
func provenanceView(v skillregistry.Verification) *controllers.SkillProvenanceView {
	if v.Scheme == "" && v.RefusalCode == "" && v.KeyID == "" {
		return nil
	}
	return &controllers.SkillProvenanceView{
		Verified:            v.Verified,
		Scheme:              string(v.Scheme),
		Algorithm:           v.Algorithm,
		KeyID:               v.KeyID,
		KeyFingerprint:      v.KeyFingerprint,
		KeyFingerprintShort: skillregistry.ShortFingerprint(v.KeyFingerprint),
		KeyOrigin:           string(v.KeyOrigin),
		TrustRootID:         v.TrustRootID,
		TrustRootName:       v.TrustRootName,
		TrustRootTier:       string(v.TrustRootTier),
		Publisher:           v.Publisher,
		SignedAt:            v.SignedAt,
		VerifiedAt:          v.VerifiedAt,
		RefusalCode:         v.RefusalCode,
		Refusal:             v.Refusal,
	}
}

// asOfOrZero unwraps an optional instant for a view field.
func asOfOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// originViewPtr renders one provenance row for a response that carries it
// optionally.
//
// It shares originView with the marketplace rather than re-deriving the
// mapping: two renderings of the same row would eventually disagree about what
// "trusted" prints as, which is the one field that must not drift.
func originViewPtr(o store.SkillInstallOrigin) *controllers.SkillInstallOriginView {
	v := originView(o)
	return &v
}
