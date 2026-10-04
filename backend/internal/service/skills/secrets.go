package skills

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillsecrets"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// secrets.go — the trusted authority for scoped secret delivery.
//
// This is the only layer that can turn a reference into bytes. A runner asks
// it, with a lease; a caller cannot. There is deliberately no parameter
// anywhere on this surface by which a request could widen its own scope: the
// scope comes from the lease, the lease comes from the store, and the store
// row was written by an administrator.
//
// Every refusal is a refusal to deliver, never a partial delivery. A run that
// asked for three secrets and may have two gets none, because a skill written
// against three and handed two behaves in ways nobody designed.

// ErrSecretsUnavailable means AO cannot deliver secrets at all — no sealing
// key, no store. It is distinct from "this scope may not have that secret".
var ErrSecretsUnavailable = errors.New("skills: the secret backend is unavailable")

// SecretSealer seals and opens values. internal/secretbox satisfies it. It is
// an interface so this service can be tested without touching a real key file,
// and so a future KMS is a new implementation rather than a rewrite.
type SecretSealer interface {
	Seal(plaintext string) (string, error)
	Open(ciphertext string) (string, error)
}

// SecretStore is the persistence this authority needs.
type SecretStore interface {
	UpsertSkillSecret(ctx context.Context, rec store.SealedSecret) (store.SealedSecret, error)
	GetSkillSecret(ctx context.Context, name string) (store.SealedSecret, bool, error)
	ListSkillSecretNames(ctx context.Context) ([]store.SecretSummary, error)
	DeleteSkillSecret(ctx context.Context, name string) (bool, error)

	UpsertSkillSecretGrant(ctx context.Context, g skillsecrets.Grant) (skillsecrets.Grant, error)
	ListSkillSecretGrantsForScope(ctx context.Context, scope skillsecrets.Scope) ([]skillsecrets.Grant, error)
	ListSkillSecretGrantsForSecret(ctx context.Context, name string) ([]skillsecrets.Grant, error)
	RevokeSkillSecretGrant(ctx context.Context, id string, at time.Time) (bool, error)
	RevokeSkillSecretGrantsForSecret(ctx context.Context, name string, at time.Time) (int64, error)

	InsertSkillSecretLease(ctx context.Context, l skillsecrets.Lease) (skillsecrets.Lease, error)
	GetSkillSecretLease(ctx context.Context, id string) (skillsecrets.Lease, bool, error)
	ConsumeSkillSecretLease(ctx context.Context, id string, now time.Time) (bool, error)
	DeleteExpiredSkillSecretLeases(ctx context.Context, before time.Time) (int64, error)
}

// SecretAuthority mints leases and redeems them for values.
type SecretAuthority struct {
	store  SecretStore
	sealer SecretSealer
	now    func() time.Time
	newID  func() string
}

// NewSecretAuthority builds the authority. A nil sealer or store makes every
// operation fail closed rather than produce a half-working secret path.
func NewSecretAuthority(st SecretStore, sealer SecretSealer) *SecretAuthority {
	return &SecretAuthority{
		store: st, sealer: sealer,
		now:   func() time.Time { return time.Now().UTC() },
		newID: func() string { return "sec-" + randomHex() },
	}
}

// Available reports whether secrets can be delivered at all.
func (a *SecretAuthority) Available() bool {
	return a != nil && a.store != nil && a.sealer != nil
}

func (a *SecretAuthority) requireAvailable() error {
	if !a.Available() {
		return fmt.Errorf("%w: no store or no sealing key is configured", ErrSecretsUnavailable)
	}
	return nil
}

// RegisterSecret stores a value, sealed. The plaintext is read from the
// SecretValue exactly once, here, and never held afterwards.
func (a *SecretAuthority) RegisterSecret(
	ctx context.Context, name string, description string, value skillsecrets.SecretValue, actor string,
) (store.SecretSummary, error) {
	if err := a.requireAvailable(); err != nil {
		return store.SecretSummary{}, err
	}
	ref, err := skillsecrets.ParseRef(name)
	if err != nil {
		return store.SecretSummary{}, apierr.Invalid("SECRET_NAME_INVALID", err.Error(), nil)
	}
	if value.IsZero() {
		return store.SecretSummary{}, apierr.Invalid("SECRET_VALUE_REQUIRED",
			"a secret with no value is a name that promises something it cannot deliver", nil)
	}
	sealed, err := a.sealer.Seal(value.Reveal())
	if err != nil {
		// The sealer's error is not wrapped with anything derived from the
		// plaintext, and value itself redacts, so this path cannot leak.
		return store.SecretSummary{}, fmt.Errorf("%w: seal failed", ErrSecretsUnavailable)
	}
	now := a.now()
	rec, err := a.store.UpsertSkillSecret(ctx, store.SealedSecret{
		Name: string(ref), Description: description, SealedValue: sealed,
		CreatedAt: now, CreatedBy: actor, UpdatedAt: now,
	})
	if err != nil {
		return store.SecretSummary{}, err
	}
	return store.SecretSummary{
		Name: rec.Name, Description: rec.Description,
		CreatedAt: rec.CreatedAt, CreatedBy: rec.CreatedBy, UpdatedAt: rec.UpdatedAt,
	}, nil
}

// ListSecrets returns names and metadata, never values.
func (a *SecretAuthority) ListSecrets(ctx context.Context) ([]store.SecretSummary, error) {
	if err := a.requireAvailable(); err != nil {
		return nil, err
	}
	out, err := a.store.ListSkillSecretNames(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DeleteSecret removes a secret and revokes its grants first, so a window
// cannot exist where a grant points at a name that is being deleted.
func (a *SecretAuthority) DeleteSecret(ctx context.Context, name string) error {
	if err := a.requireAvailable(); err != nil {
		return err
	}
	if _, err := a.store.RevokeSkillSecretGrantsForSecret(ctx, name, a.now()); err != nil {
		return err
	}
	removed, err := a.store.DeleteSkillSecret(ctx, name)
	if err != nil {
		return err
	}
	if !removed {
		return apierr.NotFound("SECRET_NOT_FOUND", fmt.Sprintf("no secret named %q", name))
	}
	return nil
}

// GrantRequest authorizes one scope to receive one secret.
type GrantRequest struct {
	Ref   skillsecrets.Ref
	Scope skillsecrets.Scope
	// TTL bounds the grant. It is required: a grant that never expires is one
	// nobody removes.
	TTL time.Duration
	// Actor and ActorPermissions are the approver and what they hold. Granting
	// a secret is gated on settings.manage, the same permission that gates the
	// capability itself, because handing a credential to a package is an
	// installation-level act however narrow the scope.
	Actor            string
	ActorPermissions []domain.Permission
}

// Grant authorizes a scope to receive a secret.
func (a *SecretAuthority) Grant(ctx context.Context, req GrantRequest) (skillsecrets.Grant, error) {
	if err := a.requireAvailable(); err != nil {
		return skillsecrets.Grant{}, err
	}
	if !holdsSettingsManage(req.ActorPermissions) {
		return skillsecrets.Grant{}, apierr.Forbidden("SECRET_GRANT_REFUSED",
			"granting a secret to a skill requires the settings.manage permission")
	}
	if req.TTL <= 0 {
		return skillsecrets.Grant{}, apierr.Invalid("SECRET_GRANT_TTL_REQUIRED",
			"a grant must expire; one that never does is one nobody removes", nil)
	}
	if _, ok, err := a.store.GetSkillSecret(ctx, string(req.Ref)); err != nil {
		return skillsecrets.Grant{}, err
	} else if !ok {
		return skillsecrets.Grant{}, apierr.NotFound("SECRET_NOT_FOUND",
			fmt.Sprintf("no secret named %q", req.Ref))
	}

	now := a.now()
	grant := skillsecrets.Grant{
		ID: a.newID(), Ref: req.Ref, Scope: req.Scope,
		GrantedBy: req.Actor, GrantedAt: now, ExpiresAt: now.Add(req.TTL),
	}
	if err := grant.Validate(); err != nil {
		return skillsecrets.Grant{}, apierr.Invalid("SECRET_GRANT_INVALID", err.Error(), nil)
	}
	return a.store.UpsertSkillSecretGrant(ctx, grant)
}

// RevokeGrant revokes one grant by id.
func (a *SecretAuthority) RevokeGrant(ctx context.Context, id string) error {
	if err := a.requireAvailable(); err != nil {
		return err
	}
	changed, err := a.store.RevokeSkillSecretGrant(ctx, id, a.now())
	if err != nil {
		return err
	}
	if !changed {
		return apierr.NotFound("SECRET_GRANT_NOT_FOUND",
			fmt.Sprintf("no active grant %q", id))
	}
	return nil
}

// LeaseRequest asks for one attempt's right to redeem grants.
type LeaseRequest struct {
	Scope skillsecrets.Scope
	// RunID and AttemptID pin the lease. A retried attempt gets its own lease
	// and cannot redeem the previous one's.
	RunID     string
	AttemptID string
	// Requested are the references the manifest declared for this mode. A name
	// with no active grant for this exact scope makes the whole lease fail —
	// there is no partial lease.
	Requested []skillsecrets.Ref
	// TTL bounds redemption. It should be short: a lease outlives nothing but
	// the launch it was minted for.
	TTL time.Duration
}

// MintLease checks every requested reference against the grants for this exact
// scope and records a single-use right.
//
// It fails closed and it fails WHOLE: one ungranted, expired or revoked
// reference refuses the lease, because a run written against three secrets and
// handed two behaves in ways nobody designed.
func (a *SecretAuthority) MintLease(ctx context.Context, req LeaseRequest) (skillsecrets.Lease, error) {
	if err := a.requireAvailable(); err != nil {
		return skillsecrets.Lease{}, err
	}
	if err := req.Scope.Validate(); err != nil {
		return skillsecrets.Lease{}, apierr.Invalid("SECRET_SCOPE_INVALID", err.Error(), nil)
	}
	if len(req.Requested) == 0 {
		return skillsecrets.Lease{}, apierr.Invalid("SECRET_LEASE_EMPTY",
			"a lease with no references delivers nothing", nil)
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}

	grants, err := a.store.ListSkillSecretGrantsForScope(ctx, req.Scope)
	if err != nil {
		return skillsecrets.Lease{}, err
	}
	now := a.now()
	active := map[skillsecrets.Ref]bool{}
	for _, g := range grants {
		// Belt and braces: the query filtered on the scope, and the grant is
		// re-checked against it here so a query change cannot silently widen
		// what a lease covers.
		if !g.Scope.Matches(req.Scope) || !g.Active(now) {
			continue
		}
		active[g.Ref] = true
	}

	var missing []string
	for _, ref := range req.Requested {
		if !active[ref] {
			missing = append(missing, string(ref))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return skillsecrets.Lease{}, apierr.Forbidden("SECRET_NOT_GRANTED",
			fmt.Sprintf("%s has no active grant for %s",
				req.Scope, strings.Join(missing, ", ")))
	}

	lease := skillsecrets.Lease{
		ID: "lease-" + randomHex(), Scope: req.Scope,
		RunID: req.RunID, AttemptID: req.AttemptID,
		Refs:     append([]skillsecrets.Ref(nil), req.Requested...),
		IssuedAt: now, ExpiresAt: now.Add(ttl),
	}
	if err := lease.Validate(); err != nil {
		return skillsecrets.Lease{}, apierr.Invalid("SECRET_LEASE_INVALID", err.Error(), nil)
	}
	return a.store.InsertSkillSecretLease(ctx, lease)
}

// Redeem exchanges a lease for values, exactly once.
//
// It re-checks the grants at redemption rather than trusting the lease alone:
// a grant revoked between minting and launch must stop the delivery, which is
// what "revocation is verifiable" has to mean.
func (a *SecretAuthority) Redeem(
	ctx context.Context, leaseID string, scope skillsecrets.Scope, runID, attemptID string,
) (map[skillsecrets.Ref]skillsecrets.SecretValue, error) {
	if err := a.requireAvailable(); err != nil {
		return nil, err
	}
	lease, ok, err := a.store.GetSkillSecretLease(ctx, leaseID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, apierr.NotFound("SECRET_LEASE_NOT_FOUND", "no such lease")
	}
	// The caller says who it is; the lease says who it must be. A mismatch is
	// a caller trying to redeem somebody else's right.
	if !lease.Scope.Matches(scope) {
		return nil, apierr.Forbidden("SECRET_LEASE_SCOPE_MISMATCH",
			"this lease belongs to a different scope")
	}
	if lease.RunID != runID || lease.AttemptID != attemptID {
		return nil, apierr.Forbidden("SECRET_LEASE_ATTEMPT_MISMATCH",
			fmt.Sprintf("this lease belongs to attempt %s of run %s", lease.AttemptID, lease.RunID))
	}
	now := a.now()
	if !lease.Usable(now) {
		return nil, apierr.Forbidden("SECRET_LEASE_SPENT",
			"this lease is expired or already redeemed")
	}

	// Re-check the grants NOW. Between minting and redemption somebody may
	// have revoked one, and a lease is a right to ask, not a right to receive.
	grants, err := a.store.ListSkillSecretGrantsForScope(ctx, scope)
	if err != nil {
		return nil, err
	}
	stillActive := map[skillsecrets.Ref]bool{}
	for _, g := range grants {
		if g.Scope.Matches(scope) && g.Active(now) {
			stillActive[g.Ref] = true
		}
	}
	var revoked []string
	for _, ref := range lease.Refs {
		if !stillActive[ref] {
			revoked = append(revoked, string(ref))
		}
	}
	if len(revoked) > 0 {
		sort.Strings(revoked)
		return nil, apierr.Forbidden("SECRET_GRANT_REVOKED",
			fmt.Sprintf("the grant for %s is no longer active", strings.Join(revoked, ", ")))
	}

	// Consume BEFORE opening anything. If two deliveries race, exactly one
	// sees the row change and the other gets nothing — rather than both
	// getting values and one losing the bookkeeping.
	consumed, err := a.store.ConsumeSkillSecretLease(ctx, leaseID, now)
	if err != nil {
		return nil, err
	}
	if !consumed {
		return nil, apierr.Forbidden("SECRET_LEASE_SPENT",
			"this lease is expired or already redeemed")
	}

	out := make(map[skillsecrets.Ref]skillsecrets.SecretValue, len(lease.Refs))
	for _, ref := range lease.Refs {
		rec, ok, err := a.store.GetSkillSecret(ctx, string(ref))
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, apierr.NotFound("SECRET_NOT_FOUND", fmt.Sprintf("no secret named %q", ref))
		}
		plain, err := a.sealer.Open(rec.SealedValue)
		if err != nil {
			// The sealer's own error may quote ciphertext; it is dropped.
			return nil, fmt.Errorf("%w: could not open %s", ErrSecretsUnavailable, ref)
		}
		out[ref] = skillsecrets.NewSecretValue(plain)
	}
	return out, nil
}

// PruneExpiredLeases removes leases nobody can redeem.
func (a *SecretAuthority) PruneExpiredLeases(ctx context.Context) (int64, error) {
	if err := a.requireAvailable(); err != nil {
		return 0, err
	}
	return a.store.DeleteExpiredSkillSecretLeases(ctx, a.now())
}

// holdsSettingsManage reports whether these permissions include settings.manage.
//
// It names the permission rather than taking it as a parameter because every
// administrative write in this package is gated on the same one: handing a
// secret to a package, approving an image for execution, configuring a registry
// and installing from one are all decisions about the INSTALLATION, not about
// any one project. A parameter that is always the same value would suggest
// those four could diverge, and they are deliberately one rule.
func holdsSettingsManage(held []domain.Permission) bool {
	for _, p := range held {
		if p == domain.PermSettingsManage {
			return true
		}
	}
	return false
}

// randomHex mints a 16-byte identifier. The length is fixed rather than a
// parameter: every id in this package is the same width, and a caller able to
// ask for a shorter one could ask for a guessable one.
func randomHex() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("skills: read random: %v", err))
	}
	return hex.EncodeToString(b)
}
