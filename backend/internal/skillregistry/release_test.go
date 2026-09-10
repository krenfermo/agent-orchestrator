package skillregistry

import (
	"errors"
	"fmt"
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

func TestAssessInstalled_LadderNeverSkipsAStep(t *testing.T) {
	r := validRelease()
	// Phase 10/11 provenance is an uninterpreted CLAIM, and a claim never
	// moves the state. Only a verification AO performed does.
	r.Provenance = Provenance{SignatureFormat: "cosign", Signature: "MEUCIQ...", KeyID: "abc"}
	if got := AssessInstalled(r, true, false); got != TrustVerified {
		t.Fatalf("AssessInstalled(matched, unsigned) = %q, want verified", got)
	}
	if got := AssessInstalled(r, false, false); got != TrustUnverified {
		t.Fatalf("AssessInstalled(unmatched) = %q, want unverified", got)
	}
	if got := AssessInstalled(r, true, true); got != TrustTrusted {
		t.Fatalf("AssessInstalled(matched, verified signature) = %q, want trusted", got)
	}
	// The step that must never be skipped: a signature over a description of
	// bytes AO does not hold says nothing about the bytes AO does hold.
	if got := AssessInstalled(r, false, true); got != TrustUnverified {
		t.Fatalf("a verified signature over UNMATCHED bytes assessed as %q; "+
			"a signature is not a substitute for the hash", got)
	}
	r.Revoked, r.RevocationReason = true, "withdrawn"
	if got := AssessInstalled(r, true, true); got != TrustRevoked {
		t.Fatalf("a revoked release assessed as %q even with a verified signature", got)
	}
}

// TestTrustedIsReachableOnlyFromAssessInstalled is the guard ADR 0006 asked
// for, inverted by ADR 0008.
//
// Phase 11 held that NOTHING in this package returns TrustTrusted, because
// nothing could verify a signature. Phase 12 can, so the claim changes shape
// rather than disappearing: TrustTrusted may be returned from exactly ONE
// place, AssessInstalled in release.go, which is the one function that takes
// both the integrity result and the signature result and can therefore refuse
// to promote one without the other.
//
// It reads the package's own source rather than exercising a code path,
// because the claim is about where a value may originate and no number of
// passing calls proves that. A second site appearing -- a helper that returns
// trusted because a release "looks signed", a cache path that carries the
// state through -- is exactly the regression this catches, and it is the kind
// that no ordinary test notices.
func TestTrustedIsReachableOnlyFromAssessInstalled(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	var offenders []string
	scanned, permitted := 0, 0
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
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			inAssessInstalled := name == "release.go" && fn.Name.Name == "AssessInstalled"
			ast.Inspect(fn, func(n ast.Node) bool {
				ret, ok := n.(*ast.ReturnStmt)
				if !ok {
					return true
				}
				for _, res := range ret.Results {
					ident, ok := res.(*ast.Ident)
					if !ok || ident.Name != "TrustTrusted" {
						continue
					}
					if inAssessInstalled {
						permitted++
						continue
					}
					offenders = append(offenders, fmt.Sprintf("%s (in %s)",
						fset.Position(ident.Pos()), fn.Name.Name))
				}
				return true
			})
		}
	}
	// A guard that scanned nothing would pass for the wrong reason.
	if scanned == 0 {
		t.Fatal("no source files were scanned; this guard would pass vacuously")
	}
	if permitted == 0 {
		t.Fatal("AssessInstalled never returns TrustTrusted, so trusted is unreachable again. " +
			"If that is deliberate, this test is the thing to change -- do not delete it.")
	}
	if len(offenders) > 0 {
		t.Fatalf("TrustTrusted is returned outside AssessInstalled, by %v.\n"+
			"Trusted must be reachable from exactly one place: the function that holds BOTH the\n"+
			"integrity result and the signature result, and can refuse to promote one without the\n"+
			"other. A second site is how a signature over bytes AO never hashed comes to read as\n"+
			"trusted. See ADR 0008.", offenders)
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
