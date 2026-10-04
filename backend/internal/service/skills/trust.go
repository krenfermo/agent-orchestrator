package skills

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// trust.go -- administering what AO will verify against, and reading it back.
//
// # The one thing this file must never let happen
//
// A TRUST ROOT MUST NOT ARRIVE FROM THE THING IT VOUCHES FOR. Nothing here
// takes a root, a key or a certificate out of a registry response, a release,
// or a package. Every write on this surface is an authenticated administrative
// act by a settings.manage holder, and every one of them lands in the audit
// trail. A registry cannot declare itself trusted, because there is no code
// path from a registry response to this file.
//
// # AO Official is not administrable
//
// The id "ao-official" and the publisher "ao" are RESERVED. No amount of
// settings.manage creates a root claiming either, and the built-in official
// roots are overlaid on top of the store so a database row could not shadow
// one even if somebody wrote it there directly. That is the point of compiling
// the anchor in: the attack it defends against is somebody with write access
// to ~/.ao, and a check that lived only in a form would not stop them.
//
// # Nobody ever pastes a private key
//
// There is no field for one, no parameter that would accept one, and the two
// write paths -- a public key with a certificate the root signed, and a public
// key an administrator vouches for -- both take 32 bytes of PUBLIC key. The
// request type below rejects anything that decodes to a private key length
// rather than storing it, because the failure mode worth designing against is
// somebody pasting the wrong half of a keypair into a settings form.

// TrustStore is the persistence the trust authority needs.
type TrustStore interface {
	UpsertSkillTrustRoot(ctx context.Context, root skillregistry.TrustRoot) (skillregistry.TrustRoot, error)
	GetSkillTrustRoot(ctx context.Context, id string) (skillregistry.TrustRoot, bool, error)
	ListSkillTrustRoots(ctx context.Context) ([]skillregistry.TrustRoot, error)
	RevokeSkillTrustRoot(ctx context.Context, id, reason string, at time.Time, actor string) (bool, error)

	UpsertSkillSigningKey(ctx context.Context, key skillregistry.SigningKey) (skillregistry.SigningKey, error)
	GetSkillSigningKey(ctx context.Context, keyID string) (skillregistry.SigningKey, bool, error)
	ListSkillSigningKeys(ctx context.Context) ([]skillregistry.SigningKey, error)
	ListSkillSigningKeysForRoot(ctx context.Context, rootID string) ([]skillregistry.SigningKey, error)
	RevokeSkillSigningKey(ctx context.Context, keyID, reason string, at time.Time, actor string) (bool, error)
	RetireSkillSigningKey(ctx context.Context, keyID string, until, at time.Time, actor string) (bool, error)

	UpsertSkillTrustRevocation(ctx context.Context, rev skillregistry.TrustRevocation) (skillregistry.TrustRevocation, error)
	GetSkillTrustRevocation(ctx context.Context, subject skillregistry.RevocationSubject, subjectID string) (skillregistry.TrustRevocation, bool, error)
	ListSkillTrustRevocations(ctx context.Context) ([]skillregistry.TrustRevocation, error)

	AppendSkillAudit(ctx context.Context, entry store.SkillAuditEntry) error
}

// TrustAuthority owns the trust store and is the skillregistry.TrustStore the
// verifier reads through.
//
// It is ONE type doing both jobs on purpose. The alternative -- an admin
// service and a separate read-only adapter -- would be two places that decide
// whether a built-in root shadows a database row, and the second copy is the
// one that eventually says no.
type TrustAuthority struct {
	store TrustStore
	// builtin is the compiled-in root set, keyed by root id. It is injected
	// rather than read from skillregistry directly so a test can exercise the
	// official path against a fixture root without a global to mutate -- and
	// so that publishing a real AO root later is a change to one constructor
	// argument.
	builtinRoots map[string]skillregistry.TrustRoot
	builtinKeys  map[string]skillregistry.SigningKey
	now          func() time.Time
	newID        func() string
}

// NewTrustAuthority builds the authority over a store and a built-in root set.
//
// A nil store makes every read answer "not found" and every write refuse,
// which is the fail-closed direction: an installation whose trust store failed
// to open must install nothing under a signed policy rather than everything.
func NewTrustAuthority(st TrustStore, builtin []skillregistry.BuiltinRoot) *TrustAuthority {
	a := &TrustAuthority{
		store:        st,
		builtinRoots: map[string]skillregistry.TrustRoot{},
		builtinKeys:  map[string]skillregistry.SigningKey{},
		now:          func() time.Time { return time.Now().UTC() },
		newID:        func() string { return "sktrust-" + randomHex() },
	}
	for _, b := range builtin {
		a.builtinRoots[b.Root.ID] = b.Root
		for _, k := range b.Keys {
			a.builtinKeys[k.KeyID] = k
		}
	}
	return a
}

// Available reports whether the authority can answer at all.
func (a *TrustAuthority) Available() bool { return a != nil && a.store != nil }

func (a *TrustAuthority) requireAvailable() error {
	if !a.Available() {
		return apierr.Invalid("SKILL_TRUST_UNAVAILABLE",
			"this installation has no skill trust store, so no signature can chain to anything", nil)
	}
	return nil
}

// ------------------------------------------------- skillregistry.TrustStore

// GetTrustRoot implements skillregistry.TrustStore.
//
// The BUILT-IN root wins over any stored row with the same id, and it wins
// unconditionally. A database row cannot shadow a compiled-in anchor, so an
// attacker with write access to ~/.ao cannot replace AO Official with their
// own -- which is the entire reason the anchor is compiled in.
func (a *TrustAuthority) GetTrustRoot(
	ctx context.Context, id string,
) (skillregistry.TrustRoot, bool, error) {
	if a == nil {
		return skillregistry.TrustRoot{}, false, nil
	}
	if root, ok := a.builtinRoots[id]; ok {
		return root, true, nil
	}
	if a.store == nil {
		return skillregistry.TrustRoot{}, false, nil
	}
	return a.store.GetSkillTrustRoot(ctx, id)
}

// GetSigningKey implements skillregistry.TrustStore, with the same overlay
// rule and for the same reason.
func (a *TrustAuthority) GetSigningKey(
	ctx context.Context, keyID string,
) (skillregistry.SigningKey, bool, error) {
	if a == nil {
		return skillregistry.SigningKey{}, false, nil
	}
	if key, ok := a.builtinKeys[keyID]; ok {
		return key, true, nil
	}
	if a.store == nil {
		return skillregistry.SigningKey{}, false, nil
	}
	return a.store.GetSkillSigningKey(ctx, keyID)
}

// PublisherRevocation implements skillregistry.TrustStore.
//
// Only ADMINISTRATIVE revocations reach here. A registry's claim that a
// publisher is revoked is recorded and scoped to that registry's own installs;
// letting a registry revoke a publisher globally would let one compromised
// registry switch off every trusted install on the machine.
func (a *TrustAuthority) PublisherRevocation(
	ctx context.Context, publisher string,
) (skillregistry.TrustRevocation, bool, error) {
	if a == nil || a.store == nil {
		return skillregistry.TrustRevocation{}, false, nil
	}
	return a.store.GetSkillTrustRevocation(ctx, skillregistry.SubjectPublisher, publisher)
}

// ------------------------------------------------------------------- reads

// TrustRootDetail is one root with its keys, for a settings screen.
type TrustRootDetail struct {
	Root skillregistry.TrustRoot
	Keys []skillregistry.SigningKey
	// BuiltIn reports that this root came with the AO build and cannot be
	// edited or removed here. It is a separate field from the tier because a
	// FUTURE enterprise root could in principle be built in too, and a screen
	// that inferred immutability from the tier would then be wrong.
	BuiltIn bool
}

// ListTrustRoots returns every root this installation will chain to, built-in
// ones first.
func (a *TrustAuthority) ListTrustRoots(
	ctx context.Context,
) ([]TrustRootDetail, error) {
	if err := a.requireAvailable(); err != nil {
		return nil, err
	}
	// Reading the trust store is settings.read at the route. The keys it
	// returns are public and their fingerprints are meant to be compared out
	// of band, which is the whole point of publishing one.
	stored, err := a.store.ListSkillTrustRoots(ctx)
	if err != nil {
		return nil, err
	}
	keys, err := a.store.ListSkillSigningKeys(ctx)
	if err != nil {
		return nil, err
	}
	byRoot := map[string][]skillregistry.SigningKey{}
	for _, k := range keys {
		byRoot[k.TrustRootID] = append(byRoot[k.TrustRootID], k)
	}

	out := make([]TrustRootDetail, 0, len(stored)+len(a.builtinRoots))
	for id, root := range a.builtinRoots {
		detail := TrustRootDetail{Root: root, BuiltIn: true}
		for _, k := range a.builtinKeys {
			if k.TrustRootID == id {
				detail.Keys = append(detail.Keys, k)
			}
		}
		sortKeys(detail.Keys)
		out = append(out, detail)
	}
	for _, root := range stored {
		// A stored row with a built-in id is shadowed, not shown twice. It
		// cannot be created through this API; if one exists it was written
		// directly to the database, and rendering it would show a person a
		// root that is not the one being used.
		if _, shadowed := a.builtinRoots[root.ID]; shadowed {
			continue
		}
		detail := TrustRootDetail{Root: root, Keys: byRoot[root.ID]}
		sortKeys(detail.Keys)
		out = append(out, detail)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].BuiltIn != out[j].BuiltIn {
			return out[i].BuiltIn
		}
		return out[i].Root.ID < out[j].Root.ID
	})
	return out, nil
}

func sortKeys(keys []skillregistry.SigningKey) {
	sort.SliceStable(keys, func(i, j int) bool {
		// Root keys first -- they are the anchor a person checks a fingerprint
		// against -- then newest window first, then by id.
		if keys[i].IsRootKey != keys[j].IsRootKey {
			return keys[i].IsRootKey
		}
		if !keys[i].ValidFrom.Equal(keys[j].ValidFrom) {
			return keys[i].ValidFrom.After(keys[j].ValidFrom)
		}
		return keys[i].KeyID < keys[j].KeyID
	})
}

// ListTrustRevocations returns every administrative withdrawal.
func (a *TrustAuthority) ListTrustRevocations(
	ctx context.Context,
) ([]skillregistry.TrustRevocation, error) {
	if err := a.requireAvailable(); err != nil {
		return nil, err
	}
	revs, err := a.store.ListSkillTrustRevocations(ctx)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(revs, func(i, j int) bool {
		if !revs[i].RevokedAt.Equal(revs[j].RevokedAt) {
			return revs[i].RevokedAt.After(revs[j].RevokedAt)
		}
		return revs[i].SubjectID < revs[j].SubjectID
	})
	return revs, nil
}

// ------------------------------------------------------------------ writes

// TrustRootRequest creates or updates an enterprise trust root.
type TrustRootRequest struct {
	ID          string
	DisplayName string
	Publisher   string
	ValidFrom   time.Time
	ValidUntil  *time.Time

	Actor            string
	ActorPermissions []domain.Permission
}

// SaveTrustRoot creates or updates an ENTERPRISE trust root.
//
// The tier is not a parameter. There is no request shape that produces an
// official root and none that produces an external one: official arrives with
// the build, and external is declared-and-refused because "who may publish to
// everyone" is a decision no phase has made. A tier field on this request
// would be a field somebody eventually sets to "official".
func (a *TrustAuthority) SaveTrustRoot(
	ctx context.Context, req TrustRootRequest,
) (skillregistry.TrustRoot, error) {
	if err := a.requireAvailable(); err != nil {
		return skillregistry.TrustRoot{}, err
	}
	if err := requireSettingsManage(req.ActorPermissions, "configuring a skill trust root"); err != nil {
		return skillregistry.TrustRoot{}, err
	}
	if strings.TrimSpace(req.Actor) == "" {
		return skillregistry.TrustRoot{}, apierr.Invalid("SKILL_TRUST_ROOT_ANONYMOUS",
			"a trust root records who configured it; an anchor nobody signed for is one nobody "+
				"can be asked about", nil)
	}
	id := strings.TrimSpace(req.ID)
	publisher := strings.TrimSpace(req.Publisher)
	if skillregistry.ReservedTrustRootID(id) {
		return skillregistry.TrustRoot{}, apierr.Forbidden("SKILL_TRUST_ROOT_RESERVED",
			fmt.Sprintf("%q is reserved for AO's own trust root, which arrives with the build and "+
				"cannot be configured here. A root that could name itself AO Official would be the "+
				"one name worth impersonating", id))
	}
	if skillregistry.ReservedPublisher(publisher) {
		return skillregistry.TrustRoot{}, apierr.Forbidden("SKILL_TRUST_PUBLISHER_RESERVED",
			fmt.Sprintf("publisher %q is reserved for AO Official releases", publisher))
	}

	now := a.now()
	existing, found, err := a.store.GetSkillTrustRoot(ctx, id)
	if err != nil {
		return skillregistry.TrustRoot{}, err
	}
	if found {
		if existing.Status == skillregistry.StatusRevoked {
			return skillregistry.TrustRoot{}, apierr.Conflict("SKILL_TRUST_ROOT_REVOKED",
				fmt.Sprintf("trust root %s was revoked on %s and does not come back because a form "+
					"was re-submitted. Configure a new root",
					id, existing.RevokedAt.UTC().Format(time.RFC3339)), nil)
		}
		if existing.Publisher != publisher {
			return skillregistry.TrustRoot{}, apierr.Conflict("SKILL_TRUST_ROOT_PUBLISHER_FIXED",
				fmt.Sprintf("trust root %s anchors publisher %q. Repointing an anchor that keys "+
					"already chain to would silently re-attribute every release they signed",
					id, existing.Publisher), nil)
		}
	}

	root := skillregistry.TrustRoot{
		ID:          id,
		Tier:        skillregistry.TierEnterprise,
		DisplayName: strings.TrimSpace(req.DisplayName),
		Publisher:   publisher,
		Status:      skillregistry.StatusActive,
		ValidFrom:   req.ValidFrom,
		ValidUntil:  req.ValidUntil,
		CreatedAt:   now,
		CreatedBy:   req.Actor,
		UpdatedAt:   now,
		UpdatedBy:   req.Actor,
	}
	if root.ValidFrom.IsZero() {
		root.ValidFrom = now
	}
	if found {
		root.CreatedAt = existing.CreatedAt
		root.CreatedBy = existing.CreatedBy
	}
	if err := root.Validate(); err != nil {
		return skillregistry.TrustRoot{}, apierr.Invalid("SKILL_TRUST_ROOT_INVALID", err.Error(), nil)
	}

	saved, err := a.store.UpsertSkillTrustRoot(ctx, root)
	if err != nil {
		return skillregistry.TrustRoot{}, err
	}
	action := store.SkillAuditTrustRootAdded
	if found {
		action = store.SkillAuditTrustRootUpdated
	}
	a.audit(ctx, store.SkillAuditEntry{
		Actor: req.Actor, Action: action, SkillID: "",
		Detail: fmt.Sprintf("trust root %s (%s tier) anchors publisher %q",
			saved.ID, saved.Tier, saved.Publisher),
	})
	return saved, nil
}

// SigningKeyRequest adds one PUBLIC signing key to a root.
//
// There is no private-key field, and PublicKey is validated to be exactly 32
// bytes of ed25519 public key before anything is stored -- so a paste of the
// wrong half of a keypair is refused rather than persisted and then regretted.
type SigningKeyRequest struct {
	KeyID       string
	TrustRootID string
	// PublicKey is base64, 32 bytes decoded.
	PublicKey string
	// IsRootKey marks a key that authorizes other keys and signs no releases.
	IsRootKey bool
	ValidFrom time.Time
	// ValidUntil is optional and is what makes an overlap window expressible:
	// the outgoing key keeps a window that ends, the incoming one starts
	// before it does, and both verify during the overlap.
	ValidUntil *time.Time
	// RotatedFromKeyID names the key this replaces, when it replaces one.
	RotatedFromKeyID string
	// Certificate, when present, is a statement the ROOT key signed
	// authorizing this key. Supplying one makes the chain cryptographic;
	// omitting it makes the key administrative, which is recorded as such and
	// never rendered as the same fact.
	Certificate *skillregistry.KeyCertificate

	Actor            string
	ActorPermissions []domain.Permission
}

// AddSigningKey registers one public key under a root.
//
// Two paths, and the difference is recorded rather than smoothed over:
//
//   - WITH a certificate, AO verifies a statement signed by a root key it
//     already holds. The chain is cryptographic end to end and the key's
//     origin is "certificate".
//   - WITHOUT one, an administrator is asserting that this key belongs to this
//     root. That is a real and necessary path -- a root that keeps its key
//     offline cannot sign a certificate on demand -- and its origin is
//     "administrative", which the trust screen shows as what it is.
func (a *TrustAuthority) AddSigningKey(
	ctx context.Context, req SigningKeyRequest,
) (skillregistry.SigningKey, error) {
	if err := a.requireAvailable(); err != nil {
		return skillregistry.SigningKey{}, err
	}
	if err := requireSettingsManage(req.ActorPermissions, "adding a skill signing key"); err != nil {
		return skillregistry.SigningKey{}, err
	}
	if strings.TrimSpace(req.Actor) == "" {
		return skillregistry.SigningKey{}, apierr.Invalid("SKILL_SIGNING_KEY_ANONYMOUS",
			"adding a signing key records who did it", nil)
	}

	rootID := strings.TrimSpace(req.TrustRootID)
	root, ok, err := a.GetTrustRoot(ctx, rootID)
	if err != nil {
		return skillregistry.SigningKey{}, err
	}
	if !ok {
		return skillregistry.SigningKey{}, apierr.NotFound("SKILL_TRUST_ROOT_NOT_FOUND",
			fmt.Sprintf("no trust root %q is configured", rootID))
	}
	if _, builtin := a.builtinRoots[rootID]; builtin {
		return skillregistry.SigningKey{}, apierr.Forbidden("SKILL_TRUST_ROOT_IMMUTABLE",
			fmt.Sprintf("trust root %s arrives with the AO build and its keys come with it. "+
				"A key added here would be a key an attacker with write access to this host could "+
				"add, which is exactly what compiling the anchor in defends against", rootID))
	}
	if root.Status == skillregistry.StatusRevoked {
		return skillregistry.SigningKey{}, apierr.Conflict("SKILL_TRUST_ROOT_REVOKED",
			fmt.Sprintf("trust root %s is revoked and anchors nothing further", rootID), nil)
	}

	now := a.now()
	key := skillregistry.SigningKey{
		KeyID:            strings.TrimSpace(req.KeyID),
		TrustRootID:      rootID,
		Publisher:        root.Publisher,
		IsRootKey:        req.IsRootKey,
		Algorithm:        skillregistry.SchemeEd25519V1.Algorithm(),
		PublicKey:        strings.TrimSpace(req.PublicKey),
		Origin:           skillregistry.OriginAdministrative,
		Status:           skillregistry.StatusActive,
		ValidFrom:        req.ValidFrom,
		ValidUntil:       req.ValidUntil,
		RotatedFromKeyID: strings.TrimSpace(req.RotatedFromKeyID),
		CreatedAt:        now,
		CreatedBy:        req.Actor,
		UpdatedAt:        now,
		UpdatedBy:        req.Actor,
	}
	if key.ValidFrom.IsZero() {
		key.ValidFrom = now
	}

	if req.Certificate != nil {
		cert := *req.Certificate
		if cert.KeyID != key.KeyID || cert.PublicKey != key.PublicKey {
			return skillregistry.SigningKey{}, apierr.Forbidden("SKILL_KEY_CERT_MISMATCH",
				"the certificate authorizes a different key id or a different public key than the "+
					"one being added; a certificate that does not cover what it is filed against "+
					"proves nothing about it")
		}
		rootKey, ok, err := a.store.GetSkillSigningKey(ctx, cert.RootKeyID)
		if err != nil {
			return skillregistry.SigningKey{}, err
		}
		if !ok {
			return skillregistry.SigningKey{}, apierr.Forbidden("SKILL_KEY_CERT_ROOT_UNKNOWN",
				fmt.Sprintf("the certificate is signed by root key %q and this installation holds "+
					"no such key. AO does not learn root keys from the certificates that use them",
					cert.RootKeyID))
		}
		if err := skillregistry.VerifyKeyCertificate(cert, rootKey, now); err != nil {
			return skillregistry.SigningKey{}, apierr.Forbidden("SKILL_KEY_CERT_INVALID", err.Error())
		}
		key.Origin = skillregistry.OriginCertificate
	}

	if err := key.Validate(); err != nil {
		return skillregistry.SigningKey{}, apierr.Invalid("SKILL_SIGNING_KEY_INVALID", err.Error(), nil)
	}
	if key.Publisher != root.Publisher {
		return skillregistry.SigningKey{}, apierr.Forbidden("SKILL_SIGNING_KEY_PUBLISHER",
			fmt.Sprintf("key %s signs for %q and trust root %s anchors %q",
				key.KeyID, key.Publisher, root.ID, root.Publisher))
	}

	saved, err := a.store.UpsertSkillSigningKey(ctx, key)
	if err != nil {
		if errors.Is(err, store.ErrSigningKeySubstitution) {
			return skillregistry.SigningKey{}, apierr.Conflict("SKILL_SIGNING_KEY_SUBSTITUTION",
				err.Error(), nil)
		}
		return skillregistry.SigningKey{}, err
	}

	action := store.SkillAuditSigningKeyAdded
	detail := fmt.Sprintf("key %s (%s, %s) added to trust root %s; fingerprint %s",
		saved.KeyID, saved.Algorithm, saved.Origin, saved.TrustRootID,
		skillregistry.ShortFingerprint(saved.Fingerprint))
	if saved.RotatedFromKeyID != "" {
		action = store.SkillAuditSigningKeyRotated
		detail += fmt.Sprintf("; rotated from %s", saved.RotatedFromKeyID)
	}
	a.audit(ctx, store.SkillAuditEntry{Actor: req.Actor, Action: action, Detail: detail})
	return saved, nil
}

// RevokeRequest withdraws a key, a publisher or a root.
type RevokeRequest struct {
	Subject   skillregistry.RevocationSubject
	SubjectID string
	Reason    string

	Actor            string
	ActorPermissions []domain.Permission
}

// Revoke withdraws one trust subject, globally and one-way.
//
// What it does NOT do, and this is the same promise the release revocation has
// carried since ADR 0006: it uninstalls nothing, deletes no files, disables no
// skill on any project and stops no run under way. It blocks NEW trusted
// installs under the revoked subject and marks the installs that are already
// here as affected. Deciding what to do about those is a human's call, one
// package at a time -- AO removing somebody's installed package because a key
// was revoked would be AO acting destructively on a security event, which is
// how a revocation notice becomes an outage.
func (a *TrustAuthority) Revoke(
	ctx context.Context, req RevokeRequest,
) (skillregistry.TrustRevocation, error) {
	if err := a.requireAvailable(); err != nil {
		return skillregistry.TrustRevocation{}, err
	}
	if err := requireSettingsManage(req.ActorPermissions, "revoking skill trust"); err != nil {
		return skillregistry.TrustRevocation{}, err
	}
	if strings.TrimSpace(req.Actor) == "" {
		return skillregistry.TrustRevocation{}, apierr.Invalid("SKILL_TRUST_REVOKE_ANONYMOUS",
			"a revocation records who made it", nil)
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		return skillregistry.TrustRevocation{}, apierr.Invalid("SKILL_TRUST_REVOKE_NO_REASON",
			"a revocation must say why; one that does not is indistinguishable from a mistake, "+
				"and it is about to block every install under it", nil)
	}
	subjectID := strings.TrimSpace(req.SubjectID)
	if subjectID == "" {
		return skillregistry.TrustRevocation{}, apierr.Invalid("SKILL_TRUST_REVOKE_NO_SUBJECT",
			"a revocation must name what it withdraws", nil)
	}

	now := a.now()
	switch req.Subject {
	case skillregistry.SubjectSigningKey:
		if _, builtin := a.builtinKeys[subjectID]; builtin {
			return skillregistry.TrustRevocation{}, apierr.Forbidden("SKILL_TRUST_KEY_IMMUTABLE",
				fmt.Sprintf("key %s arrives with the AO build. Withdrawing it is an AO release, "+
					"not a setting on this host: a revocation a local attacker could write would "+
					"be a way to turn off official trust", subjectID))
		}
		if _, err := a.store.RevokeSkillSigningKey(ctx, subjectID, reason, now, req.Actor); err != nil {
			return skillregistry.TrustRevocation{}, err
		}
	case skillregistry.SubjectTrustRoot:
		if _, builtin := a.builtinRoots[subjectID]; builtin {
			return skillregistry.TrustRevocation{}, apierr.Forbidden("SKILL_TRUST_ROOT_IMMUTABLE",
				fmt.Sprintf("trust root %s arrives with the AO build and is withdrawn by an AO "+
					"release, not from this screen", subjectID))
		}
		if _, err := a.store.RevokeSkillTrustRoot(ctx, subjectID, reason, now, req.Actor); err != nil {
			return skillregistry.TrustRevocation{}, err
		}
	case skillregistry.SubjectPublisher:
		if skillregistry.ReservedPublisher(subjectID) {
			return skillregistry.TrustRevocation{}, apierr.Forbidden("SKILL_TRUST_PUBLISHER_RESERVED",
				fmt.Sprintf("publisher %q is AO's own and is withdrawn by an AO release", subjectID))
		}
	default:
		return skillregistry.TrustRevocation{}, apierr.Invalid("SKILL_TRUST_REVOKE_SUBJECT",
			fmt.Sprintf("%q is not a revocable trust subject; a release is withdrawn by the "+
				"registry that published it", req.Subject), nil)
	}

	// The row is written for every subject INCLUDING key and root, whose
	// status columns were just set. Two records of one act, because the status
	// answers "may this be used" and the revocation row answers "what did an
	// administrator decide, when, and why" -- and only the second survives the
	// subject being deleted.
	rev, err := a.store.UpsertSkillTrustRevocation(ctx, skillregistry.TrustRevocation{
		Subject: req.Subject, SubjectID: subjectID, Reason: reason,
		RevokedAt: now, RevokedBy: req.Actor,
	})
	if err != nil {
		return skillregistry.TrustRevocation{}, err
	}

	action := store.SkillAuditTrustRootRevoked
	switch req.Subject {
	case skillregistry.SubjectSigningKey:
		action = store.SkillAuditSigningKeyRevoked
	case skillregistry.SubjectPublisher:
		action = store.SkillAuditPublisherRevoked
	}
	a.audit(ctx, store.SkillAuditEntry{
		Actor: req.Actor, Action: action,
		Detail: fmt.Sprintf("%s %s revoked: %s. Installs already on this host are marked affected "+
			"and are not removed", req.Subject, subjectID, reason),
	})
	return rev, nil
}

// RetireSigningKey closes a key's window without calling it compromised.
//
// It is a DIFFERENT verb from revoking, because the consequence for history is
// different: a retired key's earlier signatures keep verifying and a revoked
// key's do not. Merging them would mean every scheduled rotation invalidated
// the releases the outgoing key had legitimately signed, which is a system
// that punishes rotating on time.
func (a *TrustAuthority) RetireSigningKey(
	ctx context.Context, keyID string, until time.Time, actor string, perms []domain.Permission,
) error {
	if err := a.requireAvailable(); err != nil {
		return err
	}
	if err := requireSettingsManage(perms, "retiring a skill signing key"); err != nil {
		return err
	}
	if _, builtin := a.builtinKeys[keyID]; builtin {
		return apierr.Forbidden("SKILL_TRUST_KEY_IMMUTABLE",
			fmt.Sprintf("key %s arrives with the AO build", keyID))
	}
	now := a.now()
	if until.IsZero() {
		until = now
	}
	moved, err := a.store.RetireSkillSigningKey(ctx, keyID, until, now, actor)
	if err != nil {
		return err
	}
	if !moved {
		return apierr.Conflict("SKILL_SIGNING_KEY_NOT_ACTIVE",
			fmt.Sprintf("key %s is not active, so there is nothing to retire", keyID), nil)
	}
	a.audit(ctx, store.SkillAuditEntry{
		Actor: actor, Action: store.SkillAuditSigningKeyRotated,
		Detail: fmt.Sprintf("key %s retired with effect from %s; signatures it made inside its "+
			"window still verify", keyID, until.UTC().Format(time.RFC3339)),
	})
	return nil
}

// audit records one trust decision.
//
// The id and the timestamp are filled HERE rather than by callers, for the
// same reason the marketplace's audit fills them: nine call sites that each
// remember to set a primary key are nine chances for one of them to write a
// row that collides with the last one and vanish.
//
// A failure to write the trail does not fail the operation that succeeded --
// the same rule the marketplace follows.
func (a *TrustAuthority) audit(ctx context.Context, entry store.SkillAuditEntry) {
	if a == nil || a.store == nil {
		return
	}
	entry.ID = a.newID()
	entry.OccurredAt = a.now()
	_ = a.store.AppendSkillAudit(ctx, entry)
}

// HasOfficialRoot reports whether this installation holds an anchor in the
// OFFICIAL tier.
//
// It asks about the TIER rather than about the reserved id, because the tier
// is what the official policy actually requires and the id is a convention. A
// check on the id would also make this untestable: production cannot create an
// official root, so the only way to exercise the official path is a build-in
// set a test supplies, and that set is free to name its root anything.
func (a *TrustAuthority) HasOfficialRoot() bool {
	if a == nil {
		return false
	}
	for _, root := range a.builtinRoots {
		if root.Tier == skillregistry.TierOfficial && root.Status == skillregistry.StatusActive {
			return true
		}
	}
	return false
}
