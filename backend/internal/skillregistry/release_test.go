package skillregistry

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"
)

func validRelease() Release {
	return Release{
		RegistryID:            "fixture",
		SkillID:               "example-audit",
		Name:                  "Example Audit",
		Version:               "1.2.3",
		Publisher:             "tests",
		Description:           "An example skill.",
		RiskLevel:             "medium",
		ManifestDigest:        strings.Repeat("a", 64),
		ArtifactDigest:        strings.Repeat("b", 64),
		RequestedCapabilities: []string{"repo.read", "report.write"},
		ExecutionModes: []ReleaseMode{{
			ID: "quick", Name: "Quick", Description: "A quick pass.",
			RiskLevel: "low", Capabilities: []string{"repo.read"},
		}},
		Compatibility: Compatibility{AOMinVersion: "0.11.0"},
		PublishedAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestRelease_ValidateAcceptsAWellFormedRelease(t *testing.T) {
	if err := validRelease().Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestRelease_ValidateRejectsMalformedFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Release)
		want   string
	}{
		{"skill id not kebab", func(r *Release) { r.SkillID = "Example_Audit" }, "kebab-case"},
		{"version not semver", func(r *Release) { r.Version = "1.2" }, "MAJOR.MINOR.PATCH"},
		{"version is a range", func(r *Release) { r.Version = "^1.2.3" }, "MAJOR.MINOR.PATCH"},
		{"publisher empty", func(r *Release) { r.Publisher = "" }, "publisher"},
		{"risk unknown", func(r *Release) { r.RiskLevel = "spicy" }, "riskLevel"},
		{"manifest digest short", func(r *Release) { r.ManifestDigest = "abc" }, "manifestDigest"},
		{"artifact digest uppercase", func(r *Release) { r.ArtifactDigest = strings.Repeat("A", 64) }, "artifactDigest"},
		{"no capabilities", func(r *Release) { r.RequestedCapabilities = nil }, "requestedCapabilities"},
		{"unknown capability", func(r *Release) { r.RequestedCapabilities = []string{"repo.pillage"} }, "not an AO capability"},
		{"duplicate capability", func(r *Release) {
			r.RequestedCapabilities = []string{"repo.read", "repo.read"}
		}, "twice"},
		{"no modes", func(r *Release) { r.ExecutionModes = nil }, "executionModes"},
		{"mode wants undeclared capability", func(r *Release) {
			r.ExecutionModes[0].Capabilities = []string{"net.egress"}
		}, "does not declare"},
		{"no published date", func(r *Release) { r.PublishedAt = time.Time{} }, "publishedAt"},
		{"revoked with no reason", func(r *Release) { r.Revoked = true }, "say why"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validRelease()
			tc.mutate(&r)
			err := r.Validate()
			if !errors.Is(err, ErrInvalidRelease) {
				t.Fatalf("Validate = %v, want ErrInvalidRelease", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A listing has hashed nothing, so it must never look like it did.
func TestAssessAvailable_NeverClaimsVerification(t *testing.T) {
	if got := AssessAvailable(validRelease()); got != TrustUnverified {
		t.Fatalf("AssessAvailable = %q, want unverified", got)
	}
	r := validRelease()
	r.Provenance = Provenance{SignatureFormat: "cosign", Signature: "MEUCIQ..."}
	if got := AssessAvailable(r); got != TrustUnverified {
		t.Fatalf("a release that CLAIMS a signature assessed as %q; a claim is not a check", got)
	}
	r.Revoked, r.RevocationReason = true, "withdrawn"
	if got := AssessAvailable(r); got != TrustRevoked {
		t.Fatalf("AssessAvailable(revoked) = %q", got)
	}
}

func TestAssessInstalled_IsVerifiedNeverTrusted(t *testing.T) {
	r := validRelease()
	r.Provenance = Provenance{SignatureFormat: "cosign", Signature: "MEUCIQ...", KeyID: "abc"}
	if got := AssessInstalled(r, true); got != TrustVerified {
		t.Fatalf("AssessInstalled = %q, want verified", got)
	}
	if got := AssessInstalled(r, false); got != TrustUnverified {
		t.Fatalf("AssessInstalled(unmatched) = %q, want unverified", got)
	}
	r.Revoked, r.RevocationReason = true, "withdrawn"
	if got := AssessInstalled(r, true); got != TrustRevoked {
		t.Fatalf("a revoked release assessed as %q even with matching bytes", got)
	}
}

// TestTrustedIsUnreachable is the guard ADR 0006 depends on.
//
// It reads the package's own source rather than exercising a code path,
// because the claim is a NEGATIVE one -- "nothing returns this" -- and no
// number of passing calls proves it. AO verifies no signature; the day it
// does, this test is the thing that has to be deliberately changed, which is
// exactly when somebody should be forced to think about it.
func TestTrustedIsUnreachable(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	var offenders []string
	scanned := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		ast.Inspect(file, func(n ast.Node) bool {
			ret, ok := n.(*ast.ReturnStmt)
			if !ok {
				return true
			}
			for _, res := range ret.Results {
				if ident, ok := res.(*ast.Ident); ok && ident.Name == "TrustTrusted" {
					offenders = append(offenders, fset.Position(ident.Pos()).String())
				}
			}
			return true
		})
	}
	// A guard that scanned nothing would pass for the wrong reason.
	if scanned == 0 {
		t.Fatal("no source files were scanned; this guard would pass vacuously")
	}
	if len(offenders) > 0 {
		t.Fatalf("TrustTrusted is returned by %v.\n"+
			"AO verifies no signature. Calling something trusted because two hashes matched is the\n"+
			"overstatement ADR 0006 exists to prevent. If signature verification now exists, change\n"+
			"this test deliberately -- do not delete it.", offenders)
	}
}

func TestTrustState_IntegrityChecked(t *testing.T) {
	for state, want := range map[TrustState]bool{
		TrustRevoked:    false,
		TrustUnverified: false,
		TrustVerified:   true,
		TrustTrusted:    true,
	} {
		if got := state.IntegrityChecked(); got != want {
			t.Fatalf("%q.IntegrityChecked() = %v, want %v", state, got, want)
		}
	}
}

func TestCheckCompatibility(t *testing.T) {
	cases := []struct {
		name      string
		min, max  string
		aoVersion string
		want      CompatibilityVerdict
	}{
		{"in range", "0.11.0", "", "0.12.0", CompatibilityOK},
		{"at the floor", "0.11.0", "", "0.11.0", CompatibilityOK},
		{"below the floor", "0.13.0", "", "0.12.0", CompatibilityTooOld},
		{"above the ceiling", "0.11.0", "0.12.0", "0.13.0", CompatibilityTooNew},
		{"at the ceiling", "0.11.0", "0.12.0", "0.12.0", CompatibilityOK},
		// A source build reports "dev". The check DID NOT RUN, and saying so
		// is different from saying it passed.
		{"ao version unparseable", "0.11.0", "", "dev", CompatibilityUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validRelease()
			r.Compatibility = Compatibility{AOMinVersion: tc.min, AOMaxVersion: tc.max}
			if got := CheckCompatibility(r, tc.aoVersion); got != tc.want {
				t.Fatalf("CheckCompatibility = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSortReleases_IsNewestFirstWithinASkill(t *testing.T) {
	mk := func(id, version string) Release {
		r := validRelease()
		r.SkillID, r.Version = id, version
		return r
	}
	rels := []Release{mk("b-skill", "1.0.0"), mk("a-skill", "1.0.0"), mk("a-skill", "1.10.0"), mk("a-skill", "1.2.0")}
	SortReleases(rels)
	var got []string
	for _, r := range rels {
		got = append(got, r.Ref())
	}
	want := []string{"a-skill@1.10.0", "a-skill@1.2.0", "a-skill@1.0.0", "b-skill@1.0.0"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestNewerThan_RefusesADowngradeAsAnUpdate(t *testing.T) {
	if !NewerThan("1.3.0", "1.2.9") {
		t.Fatal("1.3.0 should be newer than 1.2.9")
	}
	if NewerThan("1.2.0", "1.2.0") {
		t.Fatal("a version is not newer than itself")
	}
	if NewerThan("1.1.0", "1.2.0") {
		t.Fatal("1.1.0 must not read as an update over 1.2.0")
	}
	// A prerelease sorts BELOW its release, so 2.0.0-rc1 is not an update
	// over 2.0.0.
	if NewerThan("2.0.0-rc1", "2.0.0") {
		t.Fatal("a prerelease must not read as an update over its release")
	}
}
