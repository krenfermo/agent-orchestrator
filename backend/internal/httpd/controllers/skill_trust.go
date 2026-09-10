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

// SkillTrust is the trust-root surface.
//
// A FOURTH port beside Catalog, Images and Marketplace, and separate for the
// same reason those three are separate from each other: it answers a different
// question -- "whose signatures will this installation accept" -- and an
// installation can have every other skills surface without configuring one.
type SkillTrust interface {
	TrustView(ctx context.Context) (SkillTrustListResponse, error)
	SaveTrustRootView(ctx context.Context, in SaveSkillTrustRootInput) (SkillTrustRootView, error)
	AddSigningKeyView(ctx context.Context, in AddSkillSigningKeyInput) (SkillSigningKeyView, error)
	RetireSigningKeyView(ctx context.Context, in RetireSkillSigningKeyInput) error
	RevokeTrustView(ctx context.Context, in RevokeSkillTrustInput) (SkillTrustRevocationView, error)
}

// SaveSkillTrustRootInput carries a root configuration to the service.
type SaveSkillTrustRootInput struct {
	ID          string
	DisplayName string
	Publisher   string
	ValidFrom   time.Time
	ValidUntil  *time.Time

	Actor            string
	ActorPermissions []domain.Permission
}

// AddSkillSigningKeyInput carries one PUBLIC key to the service.
type AddSkillSigningKeyInput struct {
	KeyID            string
	TrustRootID      string
	PublicKey        string
	IsRootKey        bool
	ValidFrom        time.Time
	ValidUntil       *time.Time
	RotatedFromKeyID string
	Certificate      *SkillKeyCertificateInput

	Actor            string
	ActorPermissions []domain.Permission
}

// RetireSkillSigningKeyInput closes one key's window.
type RetireSkillSigningKeyInput struct {
	KeyID      string
	ValidUntil time.Time

	Actor            string
	ActorPermissions []domain.Permission
}

// RevokeSkillTrustInput withdraws a key, a publisher or a root.
type RevokeSkillTrustInput struct {
	Subject   string
	SubjectID string
	Reason    string

	Actor            string
	ActorPermissions []domain.Permission
}

// skill_trust.go -- Settings -> Skills -> Trust, over HTTP.
//
// # There is no field on this surface a private key could enter
//
// Every write takes a PUBLIC key: 32 bytes, base64, validated on length before
// anything is stored. A 64-byte value -- somebody pasting the wrong half of a
// keypair -- is refused rather than persisted and then regretted. There is no
// "generate a key" route, because the daemon verifies and does not sign: a
// consumer that could sign would be a consumer that can mint its own trusted
// releases.
//
// # AO Official is readable and not writable
//
// Built-in roots appear in the listing with builtIn true. Every write path
// refuses them, and the reserved id and publisher are refused as input, so the
// one name worth impersonating is the one nobody can claim.

// SkillTrustRootView is one anchor and its keys.
type SkillTrustRootView struct {
	TrustRootID string `json:"trustRootId"`
	// Tier is WHO decided this root should be trusted, not how much it is
	// trusted. An enterprise root an administrator installed deliberately is
	// not weaker than AO's.
	Tier        string `json:"tier" enum:"official,enterprise,external"`
	DisplayName string `json:"displayName"`
	// Publisher is the identity this root vouches for. A release published
	// under a different name does not become trusted by this root however good
	// its signature.
	Publisher string `json:"publisher"`
	Status    string `json:"status" enum:"active,retired,revoked"`

	ValidFrom  time.Time  `json:"validFrom"`
	ValidUntil *time.Time `json:"validUntil,omitempty"`

	RevokedAt        *time.Time `json:"revokedAt,omitempty"`
	RevocationReason string     `json:"revocationReason,omitempty"`

	// BuiltIn reports that this root arrived with the AO build and cannot be
	// edited or revoked from here. It is a separate field from the tier
	// because immutability is the property a screen must act on, and inferring
	// it from the tier would break the day a built-in enterprise root exists.
	BuiltIn bool `json:"builtIn"`

	Keys []SkillSigningKeyView `json:"keys"`

	CreatedAt time.Time `json:"createdAt,omitzero"`
	CreatedBy string    `json:"createdBy,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
	UpdatedBy string    `json:"updatedBy,omitempty"`
}

// SkillSigningKeyView is one public key AO will check signatures against.
type SkillSigningKeyView struct {
	KeyID       string `json:"keyId"`
	TrustRootID string `json:"trustRootId"`
	Publisher   string `json:"publisher"`
	// IsRootKey marks a key that authorizes OTHER keys and signs no releases.
	// The separation is what stops one leaked release-signing key from being
	// used to mint more keys.
	IsRootKey bool   `json:"isRootKey"`
	Algorithm string `json:"algorithm"`
	// PublicKey is base64 and PUBLIC. It is served so an operator can compare
	// it against what the publisher published elsewhere.
	PublicKey string `json:"publicKey"`
	// Fingerprint is sha256 over the raw key bytes; FingerprintShort is the
	// same value grouped for a screen, and is for recognising a key you
	// already know rather than for deciding two keys are the same one.
	Fingerprint      string `json:"fingerprint"`
	FingerprintShort string `json:"fingerprintShort"`

	Origin string `json:"origin" enum:"certificate,administrative,built-in"`
	Status string `json:"status" enum:"active,retired,revoked"`

	ValidFrom  time.Time  `json:"validFrom"`
	ValidUntil *time.Time `json:"validUntil,omitempty"`

	RevokedAt        *time.Time `json:"revokedAt,omitempty"`
	RevocationReason string     `json:"revocationReason,omitempty"`
	// RotatedFromKeyID is what makes a rotation legible as a rotation rather
	// than as two unrelated keys that happened to overlap.
	RotatedFromKeyID string `json:"rotatedFromKeyId,omitempty"`

	CreatedAt time.Time `json:"createdAt,omitzero"`
	CreatedBy string    `json:"createdBy,omitempty"`
}

// SkillTrustRevocationView is one administrative withdrawal.
type SkillTrustRevocationView struct {
	Subject   string    `json:"subject" enum:"signing_key,publisher,trust_root"`
	SubjectID string    `json:"subjectId"`
	Reason    string    `json:"reason"`
	RevokedAt time.Time `json:"revokedAt"`
	RevokedBy string    `json:"revokedBy,omitempty"`
}

// SkillTrustListResponse is the Trust settings screen's whole read.
type SkillTrustListResponse struct {
	Roots       []SkillTrustRootView       `json:"roots"`
	Revocations []SkillTrustRevocationView `json:"revocations"`
	// TrustModel is the model in words, from the daemon.
	TrustModel string `json:"trustModel"`
	// OfficialRootAvailable reports whether this build carries an AO Official
	// anchor. It is false here, deliberately, and a client shows why rather
	// than presenting the official policy as if it worked.
	OfficialRootAvailable bool `json:"officialRootAvailable"`
	// OfficialRootNote explains that absence in words a person can act on.
	OfficialRootNote string `json:"officialRootNote,omitempty"`
}

// SaveSkillTrustRootRequest creates or updates an ENTERPRISE trust root.
//
// There is no tier field. Official arrives with the build and external is
// declared-and-refused, so a tier a client could set would be a tier somebody
// eventually sets to "official".
type SaveSkillTrustRootRequest struct {
	DisplayName string `json:"displayName"`
	Publisher   string `json:"publisher"`
	// ValidFrom and ValidUntil are POINTERS so that "absent" is expressible
	// rather than spelled as the zero instant. An omitted validFrom means
	// "now"; a zero time reaching the store would be a root valid since year 1.
	ValidFrom  *time.Time `json:"validFrom,omitempty"`
	ValidUntil *time.Time `json:"validUntil,omitempty"`
}

// AddSkillSigningKeyRequest registers one PUBLIC key under a root.
type AddSkillSigningKeyRequest struct {
	TrustRootID string `json:"trustRootId"`
	// PublicKey is base64, 32 bytes decoded. A longer value -- an ed25519
	// PRIVATE key is 64 -- is refused on length rather than stored.
	PublicKey string `json:"publicKey"`
	IsRootKey bool   `json:"isRootKey,omitempty"`

	ValidFrom  *time.Time `json:"validFrom,omitempty"`
	ValidUntil *time.Time `json:"validUntil,omitempty"`
	// RotatedFromKeyID names the key this replaces, when it replaces one.
	RotatedFromKeyID string `json:"rotatedFromKeyId,omitempty"`

	// Certificate, when present, is a statement the ROOT key signed
	// authorizing this key. Supplying one makes the chain cryptographic and
	// records the key's origin as "certificate"; omitting it records
	// "administrative", which is what an administrator vouching for a key
	// actually is.
	Certificate *SkillKeyCertificateInput `json:"certificate,omitempty"`
}

// SkillKeyCertificateInput is a root's signed authorization of a signing key.
type SkillKeyCertificateInput struct {
	Scheme      string     `json:"scheme"`
	TrustRootID string     `json:"trustRootId"`
	RootKeyID   string     `json:"rootKeyId"`
	KeyID       string     `json:"keyId"`
	PublicKey   string     `json:"publicKey"`
	Publisher   string     `json:"publisher"`
	ValidFrom   time.Time  `json:"validFrom"`
	ValidUntil  *time.Time `json:"validUntil,omitempty"`
	// Value is the root's signature over the certificate, base64.
	Value string `json:"value"`
}

// RevokeSkillTrustRequest withdraws a key, a publisher or a trust root.
//
// A RELEASE is not revocable here: a release is withdrawn by the registry that
// published it, and a second place to do it would be a second place to look.
type RevokeSkillTrustRequest struct {
	Subject   string `json:"subject" enum:"signing_key,publisher,trust_root"`
	SubjectID string `json:"subjectId"`
	Reason    string `json:"reason"`
}

// RetireSkillSigningKeyRequest closes a key's window without calling it
// compromised.
type RetireSkillSigningKeyRequest struct {
	// ValidUntil is when the window closes. Absent means now.
	ValidUntil *time.Time `json:"validUntil,omitempty"`
}

// SkillTrustModelStatement is what trusted means, and what it does not, served
// from the daemon so a client cannot describe it more optimistically than the
// thing enforcing it.
//
// The second half is the part that matters. A badge saying "trusted" beside a
// package nobody read is exactly the assurance a supply-chain attack wants to
// inherit, so the sentence that defines it also says what it is not.
const SkillTrustModelStatement = "A trust root is an anchor this installation configured: AO will accept a " +
	"signature that chains to it, and to nothing else. Roots never arrive from a registry or from a release -- " +
	"AO Official's is compiled into the build, and an enterprise root is added here, by an administrator, and " +
	"recorded in the audit trail. TRUSTED means AO verified the downloaded bytes AND a signature over exactly " +
	"those bytes, made by a key that chains to one of these roots and held by the publisher the release names. " +
	"It does not mean the code is safe, that it has no vulnerabilities, that its capabilities are benign, or " +
	"that anybody reviewed it: it says who signed, not what they signed off on."

// registerTrustRoutes mounts the Trust settings surface. Same /skills family
// and the same gate: settings.read to look, settings.manage to change.
func (c *SkillsController) registerTrustRoutes(r chi.Router) {
	r.Get("/skills/trust", c.listTrust)
	r.Put("/skills/trust/roots/{trustRootId}", c.saveTrustRoot)
	r.Put("/skills/trust/keys/{keyId}", c.addSigningKey)
	r.Post("/skills/trust/keys/{keyId}/retire", c.retireSigningKey)
	r.Post("/skills/trust/revocations", c.revokeTrust)
}

func (c *SkillsController) listTrust(w http.ResponseWriter, r *http.Request) {
	if c.Trust == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/skills/trust")
		return
	}
	out, err := c.Trust.TrustView(r.Context())
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *SkillsController) saveTrustRoot(w http.ResponseWriter, r *http.Request) {
	if c.Trust == nil {
		apispec.NotImplemented(w, r, http.MethodPut, "/api/v1/skills/trust/roots/{trustRootId}")
		return
	}
	var in SaveSkillTrustRootRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON",
			"Invalid JSON body", nil)
		return
	}
	view, err := c.Trust.SaveTrustRootView(r.Context(), SaveSkillTrustRootInput{
		ID:               chi.URLParam(r, "trustRootId"),
		DisplayName:      in.DisplayName,
		Publisher:        in.Publisher,
		ValidFrom:        timeOrZero(in.ValidFrom),
		ValidUntil:       in.ValidUntil,
		Actor:            c.actor(r),
		ActorPermissions: c.callerGlobalPermissions(r),
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, view)
}

func (c *SkillsController) addSigningKey(w http.ResponseWriter, r *http.Request) {
	if c.Trust == nil {
		apispec.NotImplemented(w, r, http.MethodPut, "/api/v1/skills/trust/keys/{keyId}")
		return
	}
	var in AddSkillSigningKeyRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON",
			"Invalid JSON body", nil)
		return
	}
	view, err := c.Trust.AddSigningKeyView(r.Context(), AddSkillSigningKeyInput{
		KeyID:            chi.URLParam(r, "keyId"),
		TrustRootID:      in.TrustRootID,
		PublicKey:        in.PublicKey,
		IsRootKey:        in.IsRootKey,
		ValidFrom:        timeOrZero(in.ValidFrom),
		ValidUntil:       in.ValidUntil,
		RotatedFromKeyID: in.RotatedFromKeyID,
		Certificate:      in.Certificate,
		Actor:            c.actor(r),
		ActorPermissions: c.callerGlobalPermissions(r),
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, view)
}

func (c *SkillsController) retireSigningKey(w http.ResponseWriter, r *http.Request) {
	if c.Trust == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/skills/trust/keys/{keyId}/retire")
		return
	}
	var in RetireSkillSigningKeyRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON",
			"Invalid JSON body", nil)
		return
	}
	if err := c.Trust.RetireSigningKeyView(r.Context(), RetireSkillSigningKeyInput{
		KeyID:            chi.URLParam(r, "keyId"),
		ValidUntil:       timeOrZero(in.ValidUntil),
		Actor:            c.actor(r),
		ActorPermissions: c.callerGlobalPermissions(r),
	}); err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, map[string]string{"status": "retired"})
}

func (c *SkillsController) revokeTrust(w http.ResponseWriter, r *http.Request) {
	if c.Trust == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/skills/trust/revocations")
		return
	}
	var in RevokeSkillTrustRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON",
			"Invalid JSON body", nil)
		return
	}
	view, err := c.Trust.RevokeTrustView(r.Context(), RevokeSkillTrustInput{
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

// SkillTrustRootParams identify one trust root.
type SkillTrustRootParams struct {
	TrustRootID string `path:"trustRootId" description:"Trust root identifier (kebab-case). 'ao-official' is reserved for the build-embedded root and is refused here."`
}

// SkillSigningKeyParams identify one signing key.
type SkillSigningKeyParams struct {
	KeyID string `path:"keyId" description:"Signing key identifier. Permanent: the public key under a given id never changes."`
}

// timeOrZero unwraps an optional instant. An absent one is the zero time, and
// every caller of these inputs reads that as "now" -- which is why the wire
// shape is a pointer: the zero instant and "unspecified" would otherwise be
// the same value, and one of them means a root valid since year 1.
func timeOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
