package skillimage

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func fullScope() Scope {
	return Scope{
		TenantID: domain.TenantID("tenant-1"), ProjectID: domain.ProjectID("proj-1"),
		SkillID: "security-audit", Version: "1.2.0", ModeID: "static-code",
	}
}

func validApproval() Approval {
	return Approval{
		ID: "img-1", Scope: fullScope(), Tool: "ao.static-scan/v1",
		Reference: "alpine", Digest: "sha256:" + strings.Repeat("a", 64),
		ApprovedBy: "ada", ApprovedAt: time.Now().UTC().Add(-time.Hour),
		Note: "pulled by hand, diffed against the upstream manifest",
	}
}

// A tag is a mutable pointer. Approving one would mean approving whatever it
// points at next, which is the opposite of what an approval is for.
func TestParseDigest_RefusesEverythingThatIsNotOne(t *testing.T) {
	good := "sha256:" + strings.Repeat("0123456789abcdef", 4)
	if got, err := ParseDigest("  " + good + "  "); err != nil || got != good {
		t.Fatalf("ParseDigest(%q) = %q, %v", good, got, err)
	}
	for _, bad := range []string{
		"", "   ", "latest", "alpine:3.19", "alpine@sha256:short",
		"sha256:" + strings.Repeat("a", 63),
		"sha256:" + strings.Repeat("a", 65),
		"sha256:" + strings.Repeat("A", 64), // uppercase is a typo, not a digest
		"sha512:" + strings.Repeat("a", 64),
		strings.Repeat("a", 64),
	} {
		if _, err := ParseDigest(bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("ParseDigest(%q) was accepted", bad)
		}
	}
}

// The reference is documentation. It must not be something anybody could be
// tempted to resolve, because then the digest stops being the identity.
func TestParseReference_RefusesAnythingResolvable(t *testing.T) {
	for _, good := range []string{"alpine", "library/alpine", "ghcr.io/aoagents/tools"} {
		if _, err := ParseReference(good); err != nil {
			t.Fatalf("ParseReference(%q): %v", good, err)
		}
	}
	for _, bad := range []string{
		"", "alpine:3.19", "alpine:latest", "latest", "repo/latest",
		"alpine@sha256:" + strings.Repeat("a", 64),
		"Alpine", "alpine!", "alpine 3.19",
	} {
		if _, err := ParseReference(bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("ParseReference(%q) was accepted", bad)
		}
	}
}

// Requirement: reject ambiguous scopes and wildcards. An approval is a
// statement somebody has to be able to evaluate, and "all versions" is not one.
func TestApproval_RefusesAnAmbiguousScope(t *testing.T) {
	full := fullScope()
	cases := map[string]Scope{
		"no tenant":        {ProjectID: full.ProjectID, SkillID: full.SkillID, Version: full.Version, ModeID: full.ModeID},
		"no project":       {TenantID: full.TenantID, SkillID: full.SkillID, Version: full.Version, ModeID: full.ModeID},
		"no skill":         {TenantID: full.TenantID, ProjectID: full.ProjectID, Version: full.Version, ModeID: full.ModeID},
		"no version":       {TenantID: full.TenantID, ProjectID: full.ProjectID, SkillID: full.SkillID, ModeID: full.ModeID},
		"no mode":          {TenantID: full.TenantID, ProjectID: full.ProjectID, SkillID: full.SkillID, Version: full.Version},
		"wildcard version": {TenantID: full.TenantID, ProjectID: full.ProjectID, SkillID: full.SkillID, Version: "*", ModeID: full.ModeID},
		"glob version":     {TenantID: full.TenantID, ProjectID: full.ProjectID, SkillID: full.SkillID, Version: "1.2.*", ModeID: full.ModeID},
		"wildcard mode":    {TenantID: full.TenantID, ProjectID: full.ProjectID, SkillID: full.SkillID, Version: full.Version, ModeID: "*"},
		"single-char glob": {TenantID: full.TenantID, ProjectID: full.ProjectID, SkillID: "security-audi?", Version: full.Version, ModeID: full.ModeID},
	}
	for name, scope := range cases {
		t.Run(name, func(t *testing.T) {
			a := validApproval()
			a.Scope = scope
			if err := a.Validate(); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

// Every other field an approval cannot do without.
func TestApproval_RefusesWhatItCannotEnforceOrExplain(t *testing.T) {
	later := time.Now().UTC().Add(time.Hour)
	earlier := time.Now().UTC().Add(-2 * time.Hour)
	cases := map[string]func(*Approval){
		"no tool":            func(a *Approval) { a.Tool = "  " },
		"no reference":       func(a *Approval) { a.Reference = "" },
		"a tagged reference": func(a *Approval) { a.Reference = "alpine:3.19" },
		"no digest":          func(a *Approval) { a.Digest = "" },
		"a tag as digest":    func(a *Approval) { a.Digest = "latest" },
		"nobody approved it": func(a *Approval) { a.ApprovedBy = " " },
		"no timestamp":       func(a *Approval) { a.ApprovedAt = time.Time{} },
		"no stated reason":   func(a *Approval) { a.Note = "" },
		"expires before it was made": func(a *Approval) {
			a.ExpiresAt = &earlier
		},
		"revoked before it was made": func(a *Approval) {
			a.RevokedAt = &earlier
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			a := validApproval()
			mutate(&a)
			if err := a.Validate(); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
	// The valid one still validates, so the table above is testing the
	// mutations and not a broken fixture.
	if err := validApproval().Validate(); err != nil {
		t.Fatalf("a valid approval was refused: %v", err)
	}
	// An optional expiry in the future is fine: unlike a secret grant, a base
	// image is a long-lived artifact and a forced date would be theatre.
	ok := validApproval()
	ok.ExpiresAt = &later
	if err := ok.Validate(); err != nil {
		t.Fatalf("an approval with a future expiry was refused: %v", err)
	}
}

// Active and Covers are the two questions asked at run time, and they must
// agree with each other in every state.
func TestApproval_ActiveAndCoversAgree(t *testing.T) {
	now := time.Now().UTC()
	scope := fullScope()
	digest := "sha256:" + strings.Repeat("a", 64)

	live := validApproval()
	if !live.Active(now) || !live.Covers(scope, "ao.static-scan/v1", digest, now) {
		t.Fatal("a live approval did not cover its own scope")
	}
	if live.InactiveReason(now) != "" {
		t.Fatalf("a live approval explained itself as inactive: %q", live.InactiveReason(now))
	}

	past := now.Add(-time.Minute)
	expired := validApproval()
	expired.ExpiresAt = &past
	if expired.Active(now) || expired.Covers(scope, "ao.static-scan/v1", digest, now) {
		t.Fatal("an expired approval still covered")
	}
	if !strings.Contains(expired.InactiveReason(now), "expired") {
		t.Fatalf("reason = %q", expired.InactiveReason(now))
	}

	revoked := validApproval()
	revoked.RevokedAt = &past
	if revoked.Active(now) || revoked.Covers(scope, "ao.static-scan/v1", digest, now) {
		t.Fatal("a revoked approval still covered")
	}
	if !strings.Contains(revoked.InactiveReason(now), "revoked") {
		t.Fatalf("reason = %q", revoked.InactiveReason(now))
	}

	// A future revocation has not happened yet. The comparison is deliberately
	// "not after now" rather than "is set", so a scheduled revocation does not
	// take effect early.
	future := now.Add(time.Hour)
	scheduled := validApproval()
	scheduled.RevokedAt = &future
	if !scheduled.Active(now) {
		t.Fatal("a revocation scheduled for later took effect now")
	}

	// Covers is exact in every dimension.
	if live.Covers(scope, "ao.dependency-audit/v1", digest, now) {
		t.Fatal("an approval covered another tool")
	}
	if live.Covers(scope, "ao.static-scan/v1", "sha256:"+strings.Repeat("b", 64), now) {
		t.Fatal("an approval covered another digest")
	}
	other := scope
	other.ModeID = "pentest"
	if live.Covers(other, "ao.static-scan/v1", digest, now) {
		t.Fatal("an approval covered another mode")
	}
}

// Describe goes into an audit line and an operator view. It must name the
// approver and the scope, and it must not be where somebody learns a path or a
// credential.
func TestApproval_DescribeNamesTheDecisionAndNothingElse(t *testing.T) {
	a := validApproval()
	got := a.Describe()
	for _, want := range []string{"ao.static-scan/v1", "alpine", a.Digest, "ada", "no expiry"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Describe() = %q, missing %q", got, want)
		}
	}
	past := time.Now().UTC().Add(-time.Minute)
	a.RevokedAt = &past
	if !strings.Contains(a.Describe(), "revoked") {
		t.Fatalf("a revoked approval describes itself as %q", a.Describe())
	}
}
