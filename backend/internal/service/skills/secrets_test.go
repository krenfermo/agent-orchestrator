package skills_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/secretbox"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/skills"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillsecrets"
)

// Every value here is synthetic. Nothing in this file reads a real credential.
const (
	syntheticDSN   = "SYNTHETIC-DSN-NEVER-REAL-0123456789"
	syntheticToken = "SYNTHETIC-TOKEN-NEVER-REAL-abcdef"
)

type secretFixture struct {
	fixture
	auth *skills.SecretAuthority
}

func newSecretFixture(t *testing.T) secretFixture {
	t.Helper()
	f := newFixture(t)
	return secretFixture{fixture: f, auth: skills.NewSecretAuthority(f.store, secretbox.New(f.dataDir))}
}

func scopeFor(project domain.ProjectID, mode string) skillsecrets.Scope {
	return skillsecrets.Scope{
		TenantID: "tenant-a", ProjectID: project,
		SkillID: "security-audit", Version: "0.1.0", ModeID: mode,
	}
}

func (f secretFixture) register(t *testing.T, name, value string) {
	t.Helper()
	if _, err := f.auth.RegisterSecret(context.Background(), name, "synthetic",
		skillsecrets.NewSecretValue(value), admin); err != nil {
		t.Fatalf("RegisterSecret %s: %v", name, err)
	}
}

func (f secretFixture) grant(t *testing.T, name string, scope skillsecrets.Scope, ttl time.Duration) skillsecrets.Grant {
	t.Helper()
	g, err := f.auth.Grant(context.Background(), skills.GrantRequest{
		Ref: skillsecrets.Ref(name), Scope: scope, TTL: ttl,
		Actor: admin, ActorPermissions: adminPerms(),
	})
	if err != nil {
		t.Fatalf("Grant %s: %v", name, err)
	}
	return g
}

func (f secretFixture) mint(t *testing.T, scope skillsecrets.Scope, run, attempt string, refs ...string) skillsecrets.Lease {
	t.Helper()
	parsed := make([]skillsecrets.Ref, 0, len(refs))
	for _, r := range refs {
		parsed = append(parsed, skillsecrets.Ref(r))
	}
	lease, err := f.auth.MintLease(context.Background(), skills.LeaseRequest{
		Scope: scope, RunID: run, AttemptID: attempt, Requested: parsed, TTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("MintLease: %v", err)
	}
	return lease
}

// The happy path, so every refusal below is a refusal of something that would
// otherwise have worked.
func TestSecrets_DeliversExactlyWhatWasGranted(t *testing.T) {
	f := newSecretFixture(t)
	ctx := context.Background()
	project := f.seedProject(t, "medusa")
	scope := scopeFor(project, "dependencies")

	f.register(t, "SENTRY_DSN", syntheticDSN)
	f.grant(t, "SENTRY_DSN", scope, time.Hour)
	lease := f.mint(t, scope, "run-1", "attempt-1", "SENTRY_DSN")

	values, err := f.auth.Redeem(ctx, lease.ID, scope, "run-1", "attempt-1")
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if len(values) != 1 {
		t.Fatalf("delivered %d values", len(values))
	}
	if got := values["SENTRY_DSN"].Reveal(); got != syntheticDSN {
		t.Fatalf("the delivered value is not the registered one")
	}
	// A listing never carries a value.
	list, err := f.auth.ListSecrets(ctx)
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	body, _ := json.Marshal(list)
	if strings.Contains(string(body), syntheticDSN) {
		t.Fatalf("the secret listing carried a value: %s", body)
	}
}

// NEGATIVE: another project, and another tenant. The grant exists; the scope
// does not match, so nothing is delivered.
func TestSecrets_RefusesAnotherProjectAndAnotherTenant(t *testing.T) {
	f := newSecretFixture(t)
	ctx := context.Background()
	medusa := f.seedProject(t, "medusa")
	poseidon := f.seedProject(t, "poseidon")

	f.register(t, "SENTRY_DSN", syntheticDSN)
	f.grant(t, "SENTRY_DSN", scopeFor(medusa, "dependencies"), time.Hour)

	// Another project in the same tenant.
	_, err := f.auth.MintLease(ctx, skills.LeaseRequest{
		Scope: scopeFor(poseidon, "dependencies"), RunID: "run-1", AttemptID: "a1",
		Requested: []skillsecrets.Ref{"SENTRY_DSN"}, TTL: time.Minute,
	})
	if err == nil || apiCode(t, err) != "SECRET_NOT_GRANTED" {
		t.Fatalf("another project was granted: %v", err)
	}

	// Another tenant, same project id.
	otherTenant := scopeFor(medusa, "dependencies")
	otherTenant.TenantID = "tenant-b"
	_, err = f.auth.MintLease(ctx, skills.LeaseRequest{
		Scope: otherTenant, RunID: "run-1", AttemptID: "a1",
		Requested: []skillsecrets.Ref{"SENTRY_DSN"}, TTL: time.Minute,
	})
	if err == nil || apiCode(t, err) != "SECRET_NOT_GRANTED" {
		t.Fatalf("another tenant was granted: %v", err)
	}
}

// NEGATIVE: an unauthorized mode and a different package version. Both are
// part of the scope, so both refuse.
func TestSecrets_RefusesAnotherModeAndAnotherVersion(t *testing.T) {
	f := newSecretFixture(t)
	ctx := context.Background()
	project := f.seedProject(t, "medusa")
	f.register(t, "SENTRY_DSN", syntheticDSN)
	f.grant(t, "SENTRY_DSN", scopeFor(project, "dependencies"), time.Hour)

	otherMode := scopeFor(project, "active-pentest")
	if _, err := f.auth.MintLease(ctx, skills.LeaseRequest{
		Scope: otherMode, RunID: "r", AttemptID: "a",
		Requested: []skillsecrets.Ref{"SENTRY_DSN"}, TTL: time.Minute,
	}); err == nil || apiCode(t, err) != "SECRET_NOT_GRANTED" {
		t.Fatalf("an unauthorized mode was granted: %v", err)
	}

	otherVersion := scopeFor(project, "dependencies")
	otherVersion.Version = "0.2.0"
	if _, err := f.auth.MintLease(ctx, skills.LeaseRequest{
		Scope: otherVersion, RunID: "r", AttemptID: "a",
		Requested: []skillsecrets.Ref{"SENTRY_DSN"}, TTL: time.Minute,
	}); err == nil || apiCode(t, err) != "SECRET_NOT_GRANTED" {
		t.Fatalf("another version was granted: %v", err)
	}
}

// NEGATIVE: expired and revoked grants. Revocation is checked at REDEMPTION as
// well as at minting, because a lease is a right to ask, not a right to receive.
func TestSecrets_RefusesExpiredAndRevokedGrants(t *testing.T) {
	f := newSecretFixture(t)
	ctx := context.Background()
	project := f.seedProject(t, "medusa")
	scope := scopeFor(project, "dependencies")
	f.register(t, "SENTRY_DSN", syntheticDSN)

	// Expired: a one-nanosecond grant is dead by the time it is used.
	f.grant(t, "SENTRY_DSN", scope, time.Nanosecond)
	time.Sleep(2 * time.Millisecond)
	if _, err := f.auth.MintLease(ctx, skills.LeaseRequest{
		Scope: scope, RunID: "r", AttemptID: "a",
		Requested: []skillsecrets.Ref{"SENTRY_DSN"}, TTL: time.Minute,
	}); err == nil || apiCode(t, err) != "SECRET_NOT_GRANTED" {
		t.Fatalf("an expired grant was used: %v", err)
	}

	// Revoked between minting and redemption: the lease exists and still fails.
	g := f.grant(t, "SENTRY_DSN", scope, time.Hour)
	lease := f.mint(t, scope, "run-2", "attempt-1", "SENTRY_DSN")
	if err := f.auth.RevokeGrant(ctx, g.ID); err != nil {
		t.Fatalf("RevokeGrant: %v", err)
	}
	_, err := f.auth.Redeem(ctx, lease.ID, scope, "run-2", "attempt-1")
	if err == nil || apiCode(t, err) != "SECRET_GRANT_REVOKED" {
		t.Fatalf("a revoked grant was redeemed: %v", err)
	}
}

// NEGATIVE: a previous attempt cannot use a later attempt's lease, and a lease
// cannot be redeemed twice.
func TestSecrets_RefusesReplayAcrossAttempts(t *testing.T) {
	f := newSecretFixture(t)
	ctx := context.Background()
	project := f.seedProject(t, "medusa")
	scope := scopeFor(project, "dependencies")
	f.register(t, "SENTRY_DSN", syntheticDSN)
	f.grant(t, "SENTRY_DSN", scope, time.Hour)

	first := f.mint(t, scope, "run-1", "attempt-1", "SENTRY_DSN")
	second := f.mint(t, scope, "run-1", "attempt-2", "SENTRY_DSN")

	// attempt-1 cannot redeem attempt-2's lease.
	if _, err := f.auth.Redeem(ctx, second.ID, scope, "run-1", "attempt-1"); err == nil ||
		apiCode(t, err) != "SECRET_LEASE_ATTEMPT_MISMATCH" {
		t.Fatalf("an earlier attempt redeemed a later lease: %v", err)
	}
	// A different run cannot either.
	if _, err := f.auth.Redeem(ctx, first.ID, scope, "run-9", "attempt-1"); err == nil ||
		apiCode(t, err) != "SECRET_LEASE_ATTEMPT_MISMATCH" {
		t.Fatalf("another run redeemed this lease: %v", err)
	}

	// One redemption works; the second is refused.
	if _, err := f.auth.Redeem(ctx, first.ID, scope, "run-1", "attempt-1"); err != nil {
		t.Fatalf("first redemption: %v", err)
	}
	if _, err := f.auth.Redeem(ctx, first.ID, scope, "run-1", "attempt-1"); err == nil ||
		apiCode(t, err) != "SECRET_LEASE_SPENT" {
		t.Fatalf("a lease was redeemed twice: %v", err)
	}
}

// NEGATIVE: a lease redeemed with a scope other than its own.
func TestSecrets_RefusesAScopeMismatchAtRedemption(t *testing.T) {
	f := newSecretFixture(t)
	ctx := context.Background()
	medusa := f.seedProject(t, "medusa")
	poseidon := f.seedProject(t, "poseidon")
	scope := scopeFor(medusa, "dependencies")
	f.register(t, "SENTRY_DSN", syntheticDSN)
	f.grant(t, "SENTRY_DSN", scope, time.Hour)
	lease := f.mint(t, scope, "run-1", "attempt-1", "SENTRY_DSN")

	if _, err := f.auth.Redeem(ctx, lease.ID, scopeFor(poseidon, "dependencies"),
		"run-1", "attempt-1"); err == nil || apiCode(t, err) != "SECRET_LEASE_SCOPE_MISMATCH" {
		t.Fatalf("a lease was redeemed under another scope: %v", err)
	}
}

// NEGATIVE: partial authorization delivers NOTHING. A run written against two
// secrets and handed one behaves in ways nobody designed.
func TestSecrets_RefusesWholeWhenOneReferenceIsUngranted(t *testing.T) {
	f := newSecretFixture(t)
	ctx := context.Background()
	project := f.seedProject(t, "medusa")
	scope := scopeFor(project, "dependencies")
	f.register(t, "SENTRY_DSN", syntheticDSN)
	f.register(t, "API_TOKEN", syntheticToken)
	f.grant(t, "SENTRY_DSN", scope, time.Hour)

	_, err := f.auth.MintLease(ctx, skills.LeaseRequest{
		Scope: scope, RunID: "r", AttemptID: "a",
		Requested: []skillsecrets.Ref{"SENTRY_DSN", "API_TOKEN"}, TTL: time.Minute,
	})
	if err == nil || apiCode(t, err) != "SECRET_NOT_GRANTED" {
		t.Fatalf("a partial lease was minted: %v", err)
	}
	if !strings.Contains(err.Error(), "API_TOKEN") {
		t.Fatalf("the refusal should name the ungranted reference: %v", err)
	}
	// The granted one is named nowhere as delivered, and no value appears.
	if strings.Contains(err.Error(), syntheticDSN) || strings.Contains(err.Error(), syntheticToken) {
		t.Fatalf("the refusal leaked a value: %v", err)
	}
}

// NEGATIVE: granting requires settings.manage. Handing a credential to a
// package is an installation-level act however narrow the scope.
func TestSecrets_GrantRequiresSettingsManage(t *testing.T) {
	f := newSecretFixture(t)
	ctx := context.Background()
	project := f.seedProject(t, "medusa")
	f.register(t, "SENTRY_DSN", syntheticDSN)

	_, err := f.auth.Grant(ctx, skills.GrantRequest{
		Ref: "SENTRY_DSN", Scope: scopeFor(project, "dependencies"), TTL: time.Hour,
		Actor: "member@example.com",
		// A project administrator: enough to activate a skill, not enough to
		// hand it a credential.
		ActorPermissions: []domain.Permission{domain.PermProjectRead, domain.PermProjectManage},
	})
	if err == nil || apiCode(t, err) != "SECRET_GRANT_REFUSED" {
		t.Fatalf("a project administrator granted a secret: %v", err)
	}
}

// NEGATIVE: a grant with no expiry, and a secret that does not exist.
func TestSecrets_GrantRefusals(t *testing.T) {
	f := newSecretFixture(t)
	ctx := context.Background()
	project := f.seedProject(t, "medusa")
	f.register(t, "SENTRY_DSN", syntheticDSN)
	scope := scopeFor(project, "dependencies")

	if _, err := f.auth.Grant(ctx, skills.GrantRequest{
		Ref: "SENTRY_DSN", Scope: scope, TTL: 0,
		Actor: admin, ActorPermissions: adminPerms(),
	}); err == nil || apiCode(t, err) != "SECRET_GRANT_TTL_REQUIRED" {
		t.Fatalf("a grant with no expiry was accepted: %v", err)
	}
	if _, err := f.auth.Grant(ctx, skills.GrantRequest{
		Ref: "NO_SUCH_SECRET", Scope: scope, TTL: time.Hour,
		Actor: admin, ActorPermissions: adminPerms(),
	}); err == nil || apiCode(t, err) != "SECRET_NOT_FOUND" {
		t.Fatalf("a grant on a nonexistent secret was accepted: %v", err)
	}
	// A partial scope is refused before it reaches the store.
	partial := scope
	partial.ModeID = ""
	if _, err := f.auth.Grant(ctx, skills.GrantRequest{
		Ref: "SENTRY_DSN", Scope: partial, TTL: time.Hour,
		Actor: admin, ActorPermissions: adminPerms(),
	}); err == nil || apiCode(t, err) != "SECRET_GRANT_INVALID" {
		t.Fatalf("a partial scope was granted: %v", err)
	}
}

// NEGATIVE: no secret backend. Every operation fails closed rather than
// producing a half-working path.
func TestSecrets_FailsClosedWithNoBackend(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	project := f.seedProject(t, "medusa")

	for name, auth := range map[string]*skills.SecretAuthority{
		"no sealer": skills.NewSecretAuthority(f.store, nil),
		"no store":  skills.NewSecretAuthority(nil, secretbox.New(f.dataDir)),
		"neither":   skills.NewSecretAuthority(nil, nil),
	} {
		t.Run(name, func(t *testing.T) {
			if auth.Available() {
				t.Fatal("an incomplete authority reported itself available")
			}
			if _, err := auth.RegisterSecret(ctx, "SENTRY_DSN", "",
				skillsecrets.NewSecretValue(syntheticDSN), admin); !errors.Is(err, skills.ErrSecretsUnavailable) {
				t.Fatalf("RegisterSecret = %v", err)
			}
			if _, err := auth.MintLease(ctx, skills.LeaseRequest{
				Scope: scopeFor(project, "dependencies"), RunID: "r", AttemptID: "a",
				Requested: []skillsecrets.Ref{"SENTRY_DSN"},
			}); !errors.Is(err, skills.ErrSecretsUnavailable) {
				t.Fatalf("MintLease = %v", err)
			}
			if _, err := auth.Redeem(ctx, "lease", scopeFor(project, "dependencies"),
				"r", "a"); !errors.Is(err, skills.ErrSecretsUnavailable) {
				t.Fatalf("Redeem = %v", err)
			}
		})
	}
}

// Deleting a secret revokes its grants first, so no window exists where a
// grant points at a name being removed.
func TestSecrets_DeleteRevokesGrantsFirst(t *testing.T) {
	f := newSecretFixture(t)
	ctx := context.Background()
	project := f.seedProject(t, "medusa")
	scope := scopeFor(project, "dependencies")
	f.register(t, "SENTRY_DSN", syntheticDSN)
	f.grant(t, "SENTRY_DSN", scope, time.Hour)
	lease := f.mint(t, scope, "run-1", "attempt-1", "SENTRY_DSN")

	if err := f.auth.DeleteSecret(ctx, "SENTRY_DSN"); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
	if _, err := f.auth.Redeem(ctx, lease.ID, scope, "run-1", "attempt-1"); err == nil {
		t.Fatal("a lease on a deleted secret was redeemed")
	}
	if err := f.auth.DeleteSecret(ctx, "SENTRY_DSN"); err == nil ||
		apiCode(t, err) != "SECRET_NOT_FOUND" {
		t.Fatalf("deleting a missing secret = %v", err)
	}
}

// The value is sealed at rest: the ciphertext must not contain the plaintext.
func TestSecrets_ValueIsSealedAtRest(t *testing.T) {
	f := newSecretFixture(t)
	ctx := context.Background()
	f.register(t, "SENTRY_DSN", syntheticDSN)

	rec, ok, err := f.store.GetSkillSecret(ctx, "SENTRY_DSN")
	if err != nil || !ok {
		t.Fatalf("GetSkillSecret: %v ok=%v", err, ok)
	}
	if strings.Contains(rec.SealedValue, syntheticDSN) {
		t.Fatal("the stored value contains the plaintext")
	}
	// Not a prefix either: a prefix is enough to confirm a guess.
	if strings.Contains(rec.SealedValue, syntheticDSN[:12]) {
		t.Fatal("the stored value contains a prefix of the plaintext")
	}
}

// The end of the chain: with a container AND a working authority,
// secrets.read becomes grantable. Without either half it does not — and
// nothing a caller sends can change that, because the dry-run API has no
// attestation field.
func TestSecrets_UnblocksSecretsReadOnlyWithBothHalves(t *testing.T) {
	confinement := []skillcatalog.Control{
		skillcatalog.ControlFilesystemIsolation,
		skillcatalog.ControlProcessIsolation,
		skillcatalog.ControlNoCredentialInheritance,
		skillcatalog.ControlResourceLimits,
		skillcatalog.ControlEgressDenyAll,
	}
	spec, ok := skillcatalog.CapSecretsRead.Spec()
	if !ok {
		t.Fatal("secrets.read has no spec")
	}

	// Container only: still blocked, on the delivery control.
	containerOnly := skillcatalog.RunnerAttestation{
		RunnerID: "container/docker", Controls: confinement,
	}
	missing, satisfied := firstMissing(spec.RequiresControls, containerOnly)
	if satisfied {
		t.Fatal("secrets.read was carried by confinement alone")
	}
	if missing != skillcatalog.ControlScopedSecretDelivery {
		t.Fatalf("missing = %q, want scoped_secret_delivery", missing)
	}

	// Delivery only, with no container: blocked on confinement, because a
	// value handed to an unconfined process is a value handed to the host.
	deliveryOnly := skillcatalog.RunnerAttestation{
		RunnerID: "none", Controls: []skillcatalog.Control{skillcatalog.ControlScopedSecretDelivery},
	}
	if _, satisfied := firstMissing(spec.RequiresControls, deliveryOnly); satisfied {
		t.Fatal("secrets.read was carried without confinement")
	}

	// Both halves: carried.
	both := skillcatalog.RunnerAttestation{
		RunnerID: "container/docker",
		Controls: append(append([]skillcatalog.Control{}, confinement...),
			skillcatalog.ControlScopedSecretDelivery),
	}
	if _, satisfied := firstMissing(spec.RequiresControls, both); !satisfied {
		t.Fatal("secrets.read was still refused with every control present")
	}

	// And the OTHER blocked capabilities are untouched: delivering secrets
	// does not unblock writing, executing or reaching the network.
	for _, stillBlocked := range []skillcatalog.Capability{
		skillcatalog.CapRepoWrite, skillcatalog.CapProcessExec,
		skillcatalog.CapNetEgress, skillcatalog.CapNetActiveScan,
	} {
		other, _ := stillBlocked.Spec()
		if _, satisfied := firstMissing(other.RequiresControls, both); satisfied {
			t.Fatalf("%s became grantable when secret delivery arrived", stillBlocked)
		}
	}
}

func firstMissing(needs []skillcatalog.Control, att skillcatalog.RunnerAttestation) (skillcatalog.Control, bool) {
	for _, need := range needs {
		if !att.Provides(need) {
			return need, false
		}
	}
	return "", true
}
