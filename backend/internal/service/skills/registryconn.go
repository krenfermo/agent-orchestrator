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
	"github.com/aoagents/agent-orchestrator/backend/internal/skillsecrets"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// registryconn.go — reaching a private registry over the network, and the
// three things that only exist once there is a socket involved: a credential,
// a verdict about whether the far end answered, and a list of what it has
// since withdrawn.
//
// # Why the credential resolves through the REGISTRY and not through a name
//
// RegistrySecrets.ResolveRegistrySecret takes a Registry and re-reads the
// stored row before it opens anything. A caller who could hand it a name could
// hand it somebody else's; a caller who hands it a Registry gets checked
// against what an administrator actually saved, and reaching that row already
// required passing the tenant visibility rule.
//
// That is the tenant boundary, stated honestly: it is drawn around the
// REGISTRY, not around the secret. internal/skillsecrets' store is
// installation-wide, so two tenants on one installation share a secret
// namespace; what they do not share is a registry that names one. Making the
// secret store itself tenant-scoped is a bigger change than this phase, and
// pretending the boundary is somewhere it is not would be worse than saying
// where it is.
//
// # Why a connection test is settings.manage and not settings.read
//
// It makes AO open a connection and present a stored credential. That is not a
// read of AO's own state, it is AO acting on the network with an authority
// somebody granted it, and the person who can do that is the person who can
// change it.

// RegistrySecrets resolves a registry's credential from the sealed store.
//
// It is a small, separate type rather than a method on SecretAuthority because
// the authority's whole shape is scope-and-lease: a runner asks with a lease it
// was minted. A registry credential has no run, no scope and no lease — it is
// the daemon authenticating as itself — and adding a "just give me this one"
// method to the lease authority would be adding a way to bypass leases.
type RegistrySecrets struct {
	store  RegistrySecretStore
	sealer SecretSealer
}

// RegistrySecretStore is the persistence a registry credential needs.
type RegistrySecretStore interface {
	GetSkillRegistry(ctx context.Context, id string) (skillregistry.Registry, bool, error)
	GetSkillSecret(ctx context.Context, name string) (store.SealedSecret, bool, error)
}

// NewRegistrySecrets builds the resolver. A nil store or sealer makes every
// resolution fail closed, which is what an installation with no sealing key
// should do: refuse to open a private registry rather than open it
// unauthenticated.
func NewRegistrySecrets(st RegistrySecretStore, sealer SecretSealer) *RegistrySecrets {
	return &RegistrySecrets{store: st, sealer: sealer}
}

// ResolveRegistrySecret implements skillregistry.SecretResolver.
//
// Every refusal below names the secretRef and never a value, and the plaintext
// is wrapped in a SecretValue the moment it exists — which redacts in String,
// GoString, MarshalJSON and therefore in every log line and error that could
// reach one.
func (r *RegistrySecrets) ResolveRegistrySecret(
	ctx context.Context, reg skillregistry.Registry,
) (skillsecrets.SecretValue, error) {
	if r == nil || r.store == nil || r.sealer == nil {
		return skillsecrets.SecretValue{}, fmt.Errorf(
			"no sealing key is configured, so the credential %s cannot be opened", reg.CredentialSecretName)
	}
	name := strings.TrimSpace(reg.CredentialSecretName)
	if name == "" {
		return skillsecrets.SecretValue{}, errors.New("this registry names no credential")
	}

	// Re-read the row. The Registry handed in came from a caller, and a caller
	// who could name a secretRef could name one attached to another tenant's
	// registry. The stored row is what an administrator actually saved, and it
	// is the only thing that can authorize reading a secret through it.
	stored, ok, err := r.store.GetSkillRegistry(ctx, reg.ID)
	if err != nil {
		return skillsecrets.SecretValue{}, err
	}
	switch {
	case !ok:
		// The registry is not stored YET. This is the first save: SaveRegistry
		// opens a registry before recording it, precisely so a configuration
		// AO cannot act on is refused rather than persisted, and opening one
		// with a credential means resolving it.
		//
		// There is no row to check against, so the check that remains is the
		// one the caller already passed: SaveRegistry requires settings.manage
		// AND membership of the tenant a tenant-scoped registry names. What
		// this leaves reachable is narrow and worth stating: somebody who
		// already holds settings.manage can learn whether an arbitrary secret
		// NAME exists, by watching whether a save is accepted. They could
		// equally attach that name to a registry and save it, so the leak is
		// existence, to somebody who could have had it anyway -- and never a
		// value.
	case stored.CredentialSecretName != name:
		return skillsecrets.SecretValue{}, fmt.Errorf(
			"registry %s is configured with a different credential than %s; "+
				"a secret is reachable only through the registry it was attached to", reg.ID, name)
	case stored.TenantID != reg.TenantID:
		return skillsecrets.SecretValue{}, fmt.Errorf(
			"registry %s belongs to a different organization than the request claims; "+
				"%s was not resolved", reg.ID, name)
	}

	rec, ok, err := r.store.GetSkillSecret(ctx, name)
	if err != nil {
		return skillsecrets.SecretValue{}, err
	}
	if !ok {
		return skillsecrets.SecretValue{}, fmt.Errorf("no secret named %s is registered", name)
	}
	plain, err := r.sealer.Open(rec.SealedValue)
	if err != nil {
		// The sealer's own error may quote ciphertext; it is dropped.
		return skillsecrets.SecretValue{}, fmt.Errorf("the secret %s could not be opened", name)
	}
	return skillsecrets.NewSecretValue(plain), nil
}

// ---------------------------------------------------------- connection test

// TestConnection runs one connection test against a configured registry.
//
// It reads one small metadata endpoint. It downloads no artifact, changes no
// catalog, installs nothing, enables nothing and approves no image — and the
// audit line says so, because a button people are afraid to press is a button
// they stop using.
func (m *Marketplace) TestConnection(
	ctx context.Context, id, actor string, perms []domain.Permission, tenants []domain.TenantID,
) (skillregistry.ProbeResult, error) {
	if err := m.requireAvailable(); err != nil {
		return skillregistry.ProbeResult{}, err
	}
	if err := requireSettingsManage(perms, "testing a skill registry connection"); err != nil {
		return skillregistry.ProbeResult{}, err
	}
	reg, err := m.visibleRegistry(ctx, id, tenants)
	if err != nil {
		return skillregistry.ProbeResult{}, err
	}

	// The provider comes from the factory, which is the one place a registry
	// type turns into a client. Probing with a client built here instead would
	// be probing a DIFFERENT client than the one every other operation uses --
	// a connection test that passes for a configuration installs then fail on.
	result := m.probe(ctx, reg)
	m.recordProbe(ctx, reg, result)

	m.audit(ctx, store.SkillAuditEntry{
		Actor:  actor,
		Action: store.SkillAuditRegistryConnectionTested,
		Detail: fmt.Sprintf("registry %s: %s — %s. Metadata only: nothing was downloaded, "+
			"installed, enabled or approved", reg.ID, result.State, result.Detail),
	})
	if result.State == skillregistry.ProbeAuthFailed {
		// Separate row, deliberately. It is the failure an operator can FIX,
		// and it means a credential expired rather than a package failing a
		// check. The detail names the ref; there is no path here that has the
		// value.
		m.audit(ctx, store.SkillAuditEntry{
			Actor:  actor,
			Action: store.SkillAuditRegistryAuthFailed,
			Detail: fmt.Sprintf("registry %s refused the credential %s (authType %s)",
				reg.ID, reg.CredentialSecretName, reg.EffectiveAuthType()),
		})
	}
	return result, nil
}

// probe opens the registry the ordinary way and asks it to identify itself.
//
// A provider that cannot be opened at all -- an unresolvable credential, a base
// URL the network policy refuses -- is POLICY_BLOCKED: AO refused, before any
// packet, and saying "unreachable" would send an operator to look at the
// network instead of at the configuration.
func (m *Marketplace) probe(
	ctx context.Context, reg skillregistry.Registry,
) skillregistry.ProbeResult {
	provider, err := m.openProvider(ctx, reg)
	if err != nil {
		return skillregistry.ProbeRefused(reg, err, m.now)
	}
	prober, ok := provider.(skillregistry.ConnectionProbe)
	if !ok {
		return skillregistry.ProbeRefused(reg,
			fmt.Errorf("a %s registry has no connection to test", reg.Type), m.now)
	}
	return prober.Probe(ctx, m.now)
}

// recordProbe stores the verdict. It is best-effort for the same reason audit
// is: the test already happened, and failing the caller now would leave the
// screen unable to show what it observed.
func (m *Marketplace) recordProbe(
	ctx context.Context, reg skillregistry.Registry, result skillregistry.ProbeResult,
) {
	if m.status == nil {
		return
	}
	at := result.TestedAt
	in := store.SkillRegistryStatus{
		RegistryID:       reg.ID,
		LastProbeState:   result.State,
		LastProbeDetail:  result.Detail,
		LastProbeAt:      &at,
		LastProbeLatency: result.Latency,
		UpdatedAt:        m.now(),
	}
	if result.State == skillregistry.ProbeConnected {
		// A successful test IS a successful metadata read, so it is honest to
		// move the sync clock. Nothing else about the catalogue changed.
		in.LastSyncAt = &at
	}
	_, _ = m.status.UpsertSkillRegistryStatus(ctx, in)
}

// ------------------------------------------------------------- revocation

// RevocationSyncResult is what one revocation sync found.
type RevocationSyncResult struct {
	RegistryID string `json:"registryId"`
	// Fetched is how many withdrawals the registry listed.
	Fetched int `json:"fetched"`
	// NewlyRecorded is how many AO had not seen before.
	NewlyRecorded int `json:"newlyRecorded"`
	// AffectedInstalls names the installed releases this sync marked revoked.
	// AO marked them; it uninstalled nothing and disabled nothing.
	AffectedInstalls []string `json:"affectedInstalls"`
	// Unreachable names why AO could not ask. What it already knows is
	// unchanged and stays in force: a registry AO cannot reach does not become
	// a registry with no revocations.
	Unreachable string    `json:"unreachable,omitempty"`
	SyncedAt    time.Time `json:"syncedAt"`
}

// SyncRevocations asks one registry what it has withdrawn and records it.
//
// It is a request a person made. AO polls nothing: a background job that
// fetched from configured registries would be a background job reaching the
// network on a schedule nobody approved.
//
// It NEVER uninstalls, never deletes files and never disables a skill on a
// project. It marks, it blocks new installs, and it puts the sentence on
// screen. Deciding what to do about an installed release that was withdrawn is
// a human's call, one package at a time.
func (m *Marketplace) SyncRevocations(
	ctx context.Context, id, actor string, perms []domain.Permission, tenants []domain.TenantID,
) (RevocationSyncResult, error) {
	if err := m.requireAvailable(); err != nil {
		return RevocationSyncResult{}, err
	}
	if err := requireSettingsManage(perms, "synchronizing registry revocations"); err != nil {
		return RevocationSyncResult{}, err
	}
	if m.status == nil {
		return RevocationSyncResult{}, apierr.Invalid("SKILL_REGISTRY_NO_REVOCATION_STORE",
			"this installation has no revocation store, so a sync would record nothing", nil)
	}
	reg, err := m.visibleRegistry(ctx, id, tenants)
	if err != nil {
		return RevocationSyncResult{}, err
	}
	now := m.now()
	result := RevocationSyncResult{RegistryID: reg.ID, AffectedInstalls: []string{}, SyncedAt: now}

	source, err := m.revocationSource(ctx, reg)
	if err != nil {
		// Deliberately a successful answer with Unreachable set, not an error.
		// "AO could not ask" is what the caller has to render, and returning an
		// error here would make it indistinguishable from "the sync failed" --
		// which would hide that what AO already recorded is still in force.
		result.Unreachable = err.Error()
		return result, nil //nolint:nilerr // see above: unreachable is an answer.
	}
	revocations, err := source.FetchRevocations(ctx)
	if err != nil {
		// What AO already recorded stays in force. A registry AO cannot reach
		// is not a registry with nothing withdrawn.
		result.Unreachable = fmt.Sprintf("registry %s could not be asked: %v", reg.ID, err)
		return result, nil
	}
	result.Fetched = len(revocations)

	for _, rev := range revocations {
		before, existed, err := m.status.GetSkillRegistryRevocation(ctx, reg.ID, rev.SkillID, rev.Version)
		if err != nil {
			return RevocationSyncResult{}, err
		}
		revokedAt := rev.RevokedAt
		row := store.SkillRegistryRevocation{
			RegistryID: reg.ID, SkillID: rev.SkillID, Version: rev.Version,
			Reason: rev.Reason, ObservedAt: now,
		}
		if !revokedAt.IsZero() {
			row.RevokedAt = &revokedAt
		}
		if _, err := m.status.UpsertSkillRegistryRevocation(ctx, row); err != nil {
			return RevocationSyncResult{}, err
		}
		if !existed {
			result.NewlyRecorded++
		}
		_ = before

		// Mark an INSTALLED copy, if there is one. The store reports whether
		// the row actually changed, so the audit fires once rather than on
		// every sync.
		marked, err := m.store.MarkSkillInstallOriginRevoked(ctx, rev.SkillID, rev.Version, rev.Reason, now)
		if err != nil {
			return RevocationSyncResult{}, err
		}
		if marked {
			result.AffectedInstalls = append(result.AffectedInstalls, rev.Ref())
			m.audit(ctx, store.SkillAuditEntry{
				Actor:   actor,
				Action:  store.SkillAuditReleaseRevokedSeen,
				SkillID: rev.SkillID,
				Version: rev.Version,
				Detail: fmt.Sprintf("registry %s revoked this release: %s. "+
					"AO did not uninstall it, did not disable it on any project and did not stop a run "+
					"already under way; that decision is yours", reg.ID, rev.Reason),
			})
		}
	}
	sort.Strings(result.AffectedInstalls)

	syncedAt := now
	_, _ = m.status.UpsertSkillRegistryStatus(ctx, store.SkillRegistryStatus{
		RegistryID: reg.ID, LastRevocationSyncAt: &syncedAt, UpdatedAt: now,
	})
	m.audit(ctx, store.SkillAuditEntry{
		Actor:  actor,
		Action: store.SkillAuditRevocationsSynced,
		Detail: fmt.Sprintf("registry %s listed %d revocation(s); %d were new to AO and %d installed "+
			"release(s) were marked. Nothing was uninstalled or disabled",
			reg.ID, result.Fetched, result.NewlyRecorded, len(result.AffectedInstalls)),
	})
	return result, nil
}

// RevocationSource is a provider that can list withdrawals. Only the HTTPS
// provider implements it: a local directory's index already carries the
// revoked flag on each release, so it needs no second endpoint.
type RevocationSource interface {
	FetchRevocations(ctx context.Context) ([]skillregistry.Revocation, error)
}

func (m *Marketplace) revocationSource(
	ctx context.Context, reg skillregistry.Registry,
) (RevocationSource, error) {
	if !reg.Enabled {
		return nil, fmt.Errorf("registry %s is disabled", reg.ID)
	}
	provider, err := m.providers.Open(ctx, reg)
	if err != nil {
		return nil, fmt.Errorf("registry %s could not be opened: %w", reg.ID, err)
	}
	source, ok := provider.(RevocationSource)
	if !ok {
		return nil, fmt.Errorf("registry %s serves no revocation list; "+
			"a local directory carries the revoked flag on each release instead", reg.ID)
	}
	return source, nil
}

// knownRevocation reports what AO has recorded about one exact release,
// whether or not the registry can be reached right now.
//
// It is the check that makes an offline install safe to refuse. A release AO
// knows was withdrawn stays withdrawn while the network is down: silence is not
// consent, and the one direction it is safe to be wrong in is this one.
func (m *Marketplace) knownRevocation(
	ctx context.Context, registryID, skillID, version string,
) (store.SkillRegistryRevocation, bool) {
	if m.status == nil {
		return store.SkillRegistryRevocation{}, false
	}
	row, ok, err := m.status.GetSkillRegistryRevocation(ctx, registryID, skillID, version)
	if err != nil || !ok {
		return store.SkillRegistryRevocation{}, false
	}
	return row, true
}
