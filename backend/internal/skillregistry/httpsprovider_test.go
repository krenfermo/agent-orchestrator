package skillregistry_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry/registrytest"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillsecrets"
)

// httpsprovider_test.go -- the private-registry client against a real HTTPS
// server, over real TLS, with the real address policy in the path.
//
// Every test here points AO's production client at the fixture. There is no
// mock Provider, no disabled verification and no injected transport: the
// controls this phase adds are transport controls, and a stub that satisfies
// the interface exercises none of them.

func openProvider(t *testing.T, srv *registrytest.Server, reg skillregistry.Registry) skillregistry.Provider {
	t.Helper()
	p, err := skillregistry.NewHTTPSProvider(context.Background(), reg, nil, srv.Options())
	if err != nil {
		t.Fatalf("NewHTTPSProvider: %v", err)
	}
	return p
}

func publishOne(t *testing.T, srv *registrytest.Server) skillregistry.Release {
	t.Helper()
	return srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.2.0"})
}

// TestSearchNeverDownloadsAnArtifact is the property the whole protocol shape
// exists to hold. It is asserted against what the SERVER was asked for, not
// against what the client returned: an absence is only provable if something
// was counting.
func TestSearchNeverDownloadsAnArtifact(t *testing.T) {
	srv := registrytest.New(t, "corp")
	publishOne(t, srv)
	p := openProvider(t, srv, srv.Registry("corp"))

	rels, err := p.Search(context.Background(), skillregistry.Query{Text: "security"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(rels) != 1 || rels[0].SkillID != "security-audit" {
		t.Fatalf("search returned %+v", rels)
	}
	if _, err := p.Get(context.Background(), "security-audit", "1.2.0"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := p.ListVersions(context.Background(), "security-audit"); err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if _, err := p.ResolveExactRelease(context.Background(), "security-audit", "1.2.0"); err != nil {
		t.Fatalf("ResolveExactRelease: %v", err)
	}
	if srv.FetchedArtifact() {
		t.Fatalf("a metadata read fetched an artifact; requests were %v", srv.Requests())
	}
}

// TestFetchArtifactLandsVerifiableBytes proves the fetch path produces exactly
// the tree the release describes -- the same two digests the install compares.
func TestFetchArtifactLandsVerifiableBytes(t *testing.T) {
	srv := registrytest.New(t, "corp")
	rel := publishOne(t, srv)
	p := openProvider(t, srv, srv.Registry("corp"))

	dest := filepath.Join(t.TempDir(), "quarantine")
	if err := p.FetchArtifact(context.Background(), rel, dest); err != nil {
		t.Fatalf("FetchArtifact: %v", err)
	}
	got, err := skillcatalog.ComputePackageDigest(dest)
	if err != nil {
		t.Fatalf("ComputePackageDigest: %v", err)
	}
	if got != rel.ArtifactDigest {
		t.Fatalf("artifact digest %s, want %s", got, rel.ArtifactDigest)
	}
	manifest, err := skillregistry.FileDigest(filepath.Join(dest, skillcatalog.ManifestFileName))
	if err != nil {
		t.Fatalf("FileDigest: %v", err)
	}
	if manifest != rel.ManifestDigest {
		t.Fatalf("manifest digest %s, want %s", manifest, rel.ManifestDigest)
	}
	// The package must load as a package: the fetch produced a real tree, not
	// a pile of files that happens to hash correctly.
	if _, err := skillcatalog.LoadPackage(dest); err != nil {
		t.Fatalf("LoadPackage: %v", err)
	}
}

// TestBearerTokenReachesTheRegistry proves the credential is actually
// presented, and that a wrong one is a classified refusal rather than a
// generic failure.
func TestBearerTokenReachesTheRegistry(t *testing.T) {
	srv := registrytest.New(t, "corp")
	publishOne(t, srv)
	srv.RequireBearer("s3cret-token")

	reg := srv.Registry("corp")
	reg.AuthType = skillregistry.AuthBearer
	reg.CredentialSecretName = "CORP_REGISTRY_TOKEN"

	good := skillregistry.SecretResolverFunc(
		func(context.Context, skillregistry.Registry) (skillsecrets.SecretValue, error) {
			return skillsecrets.NewSecretValue("s3cret-token"), nil
		})
	p, err := skillregistry.NewHTTPSProvider(context.Background(), reg, good, srv.Options())
	if err != nil {
		t.Fatalf("NewHTTPSProvider: %v", err)
	}
	if _, err := p.Search(context.Background(), skillregistry.Query{}); err != nil {
		t.Fatalf("authenticated search: %v", err)
	}

	bad := skillregistry.SecretResolverFunc(
		func(context.Context, skillregistry.Registry) (skillsecrets.SecretValue, error) {
			return skillsecrets.NewSecretValue("wrong-token"), nil
		})
	p2, err := skillregistry.NewHTTPSProvider(context.Background(), reg, bad, srv.Options())
	if err != nil {
		t.Fatalf("NewHTTPSProvider: %v", err)
	}
	_, err = p2.Search(context.Background(), skillregistry.Query{})
	if !errors.Is(err, skillregistry.ErrRegistryAuth) {
		t.Fatalf("a wrong token gave %v, want ErrRegistryAuth", err)
	}
	// The refusal names the ref and never the value.
	if strings.Contains(err.Error(), "wrong-token") {
		t.Fatalf("the auth error quotes the credential: %v", err)
	}
	if !strings.Contains(err.Error(), "CORP_REGISTRY_TOKEN") {
		t.Fatalf("the auth error does not name the secretRef: %v", err)
	}
}

// TestAPIKeyHeaderIsUsedWhenConfigured proves the second auth type is real and
// not a value the configuration accepts and ignores.
func TestAPIKeyHeaderIsUsedWhenConfigured(t *testing.T) {
	srv := registrytest.New(t, "corp")
	publishOne(t, srv)
	srv.RequireAPIKey("X-Corp-Key", "key-value")

	reg := srv.Registry("corp")
	reg.AuthType = skillregistry.AuthAPIKeyHeader
	reg.APIKeyHeader = "X-Corp-Key"
	reg.CredentialSecretName = "CORP_KEY"

	p, err := skillregistry.NewHTTPSProvider(context.Background(), reg,
		skillregistry.SecretResolverFunc(
			func(context.Context, skillregistry.Registry) (skillsecrets.SecretValue, error) {
				return skillsecrets.NewSecretValue("key-value"), nil
			}), srv.Options())
	if err != nil {
		t.Fatalf("NewHTTPSProvider: %v", err)
	}
	if _, err := p.Search(context.Background(), skillregistry.Query{}); err != nil {
		t.Fatalf("api-key search: %v", err)
	}
}

// TestUntrustedCertificateIsRefused proves verification is on. The fixture's
// server is real; the client is simply not given its authority.
func TestUntrustedCertificateIsRefused(t *testing.T) {
	srv := registrytest.New(t, "corp")
	publishOne(t, srv)

	opts := srv.Options()
	// The pool of an authority that signed nothing here.
	opts.RootCAs = srv.UntrustedCA.Pool
	p, err := skillregistry.NewHTTPSProvider(context.Background(), srv.Registry("corp"), nil, opts)
	if err != nil {
		t.Fatalf("NewHTTPSProvider: %v", err)
	}
	_, err = p.Search(context.Background(), skillregistry.Query{})
	if !errors.Is(err, skillregistry.ErrRegistryTLS) {
		t.Fatalf("an unknown authority gave %v, want ErrRegistryTLS", err)
	}
}

// TestRedirectOffOriginIsRefused covers the cross-origin redirect and the
// redirect to a metadata address in one place: both are the same refusal, made
// before the request is sent rather than after the body arrives.
func TestRedirectOffOriginIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, target string }{
		{"another host", "https://evil.example.com/steal"},
		{"cloud metadata", "http://169.254.169.254/latest/meta-data/"},
		{"downgraded to http", "http://registry.test/v1/skills"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := registrytest.New(t, "corp")
			rel := publishOne(t, srv)
			srv.Fail(registrytest.Failures{RedirectArtifactTo: tc.target})
			p := openProvider(t, srv, srv.Registry("corp"))

			err := p.FetchArtifact(context.Background(), rel, filepath.Join(t.TempDir(), "q"))
			if !errors.Is(err, skillregistry.ErrNetworkPolicy) {
				t.Fatalf("redirect to %s gave %v, want ErrNetworkPolicy", tc.target, err)
			}
		})
	}
}

// TestDNSRebindingIsRefused makes a permitted name resolve to the cloud
// metadata address. The origin allowlist cannot catch this -- the name IS the
// configured one -- so the address check at dial time is the only thing that
// can, which is why it exists.
func TestDNSRebindingIsRefused(t *testing.T) {
	srv := registrytest.New(t, "corp")
	publishOne(t, srv)

	opts := srv.Options()
	opts.Resolver = registrytest.FixedResolver{IPs: []net.IP{net.ParseIP("169.254.169.254")}}
	p, err := skillregistry.NewHTTPSProvider(context.Background(), srv.Registry("corp"), nil, opts)
	if err != nil {
		t.Fatalf("NewHTTPSProvider: %v", err)
	}
	_, err = p.Search(context.Background(), skillregistry.Query{})
	if !errors.Is(err, skillregistry.ErrNetworkPolicy) {
		t.Fatalf("a rebinding answer gave %v, want ErrNetworkPolicy", err)
	}
	if !strings.Contains(err.Error(), "169.254.169.254") {
		t.Fatalf("the refusal does not name the address it refused: %v", err)
	}
}

// TestOneGoodAnswerDoesNotExcuseABadOne holds the rule that EVERY resolved
// address must be acceptable. A name answering with both a public address and
// the metadata address is refused: allowing it would leave which one gets used
// to chance.
func TestOneGoodAnswerDoesNotExcuseABadOne(t *testing.T) {
	srv := registrytest.New(t, "corp")
	publishOne(t, srv)

	opts := srv.Options()
	opts.Resolver = registrytest.FixedResolver{IPs: []net.IP{
		net.ParseIP("127.0.0.1"), net.ParseIP("169.254.169.254"),
	}}
	p, err := skillregistry.NewHTTPSProvider(context.Background(), srv.Registry("corp"), nil, opts)
	if err != nil {
		t.Fatalf("NewHTTPSProvider: %v", err)
	}
	if _, err := p.Search(context.Background(), skillregistry.Query{}); !errors.Is(err, skillregistry.ErrNetworkPolicy) {
		t.Fatalf("a mixed answer gave %v, want ErrNetworkPolicy", err)
	}
}

// TestLoopbackNeedsAnExplicitPolicy proves the private-range exception is a
// real control and not decoration: without it the fixture is unreachable, and
// the fixture lives on loopback.
func TestLoopbackNeedsAnExplicitPolicy(t *testing.T) {
	srv := registrytest.New(t, "corp")
	publishOne(t, srv)

	reg := srv.Registry("corp")
	reg.NetworkPolicy = skillregistry.NetworkPolicy{}
	p, err := skillregistry.NewHTTPSProvider(context.Background(), reg, nil, srv.Options())
	if err != nil {
		t.Fatalf("NewHTTPSProvider: %v", err)
	}
	if _, err := p.Search(context.Background(), skillregistry.Query{}); !errors.Is(err, skillregistry.ErrNetworkPolicy) {
		t.Fatalf("loopback with no exception gave %v, want ErrNetworkPolicy", err)
	}
}

// TestNoPolicyMayReopenLinkLocal holds the one range an exception can never
// re-open -- the same rule, and the same validator, the skill egress proxy
// uses.
func TestNoPolicyMayReopenLinkLocal(t *testing.T) {
	srv := registrytest.New(t, "corp")
	reg := srv.Registry("corp")
	reg.NetworkPolicy = skillregistry.NetworkPolicy{PermittedPrivateCIDRs: []string{"169.254.0.0/16"}}
	if err := reg.Validate(); err == nil {
		t.Fatal("a registry permitting link-local validated")
	}
	if _, err := skillregistry.NewHTTPSProvider(context.Background(), reg, nil, srv.Options()); err == nil {
		t.Fatal("a provider opened with a link-local exception")
	}
}

// TestOversizedMetadataIsRefused covers the response ceiling.
func TestOversizedMetadataIsRefused(t *testing.T) {
	srv := registrytest.New(t, "corp")
	publishOne(t, srv)
	srv.Fail(registrytest.Failures{OversizedMetadata: true})
	p := openProvider(t, srv, srv.Registry("corp"))

	if _, err := p.Search(context.Background(), skillregistry.Query{}); !errors.Is(err, skillregistry.ErrRegistryResponse) {
		t.Fatalf("an oversized body gave %v, want ErrRegistryResponse", err)
	}
}

// TestOversizedArtifactIsRefused covers the compressed ceiling, and
// TestDecompressionBombIsRefused covers the one a compressed ceiling cannot
// catch.
func TestOversizedArtifactIsRefused(t *testing.T) {
	srv := registrytest.New(t, "corp")
	rel := publishOne(t, srv)
	srv.Fail(registrytest.Failures{OversizedArtifact: true})
	p := openProvider(t, srv, srv.Registry("corp"))

	err := p.FetchArtifact(context.Background(), rel, filepath.Join(t.TempDir(), "q"))
	if err == nil {
		t.Fatal("an oversized artifact was accepted")
	}
}

func TestDecompressionBombIsRefused(t *testing.T) {
	srv := registrytest.New(t, "corp")
	rel := publishOne(t, srv)
	srv.Fail(registrytest.Failures{ArtifactBomb: true})
	p := openProvider(t, srv, srv.Registry("corp"))

	dest := filepath.Join(t.TempDir(), "q")
	err := p.FetchArtifact(context.Background(), rel, dest)
	if !errors.Is(err, skillregistry.ErrArtifactRefused) {
		t.Fatalf("a decompression bomb gave %v, want ErrArtifactRefused", err)
	}
	// And it did not write 400 MiB while failing.
	var written int64
	_ = filepath.Walk(dest, func(_ string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() {
			written += info.Size()
		}
		return nil
	})
	if written > skillregistry.MaxArtifactFileBytes+1 {
		t.Fatalf("the refused bomb still wrote %d bytes", written)
	}
}

// TestArtifactLinksAndTraversalAreRefused covers the two archive entries that
// would reach outside the quarantine.
func TestArtifactLinksAndTraversalAreRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail registrytest.Failures
	}{
		{"a symlink out of the package", registrytest.Failures{ArtifactSymlink: true}},
		{"an entry above the package root", registrytest.Failures{ArtifactTraversal: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := registrytest.New(t, "corp")
			rel := publishOne(t, srv)
			srv.Fail(tc.fail)
			p := openProvider(t, srv, srv.Registry("corp"))

			base := t.TempDir()
			err := p.FetchArtifact(context.Background(), rel, filepath.Join(base, "q"))
			if !errors.Is(err, skillregistry.ErrArtifactRefused) {
				t.Fatalf("%s gave %v, want ErrArtifactRefused", tc.name, err)
			}
			if _, statErr := os.Stat(filepath.Join(base, "escaped.txt")); statErr == nil {
				t.Fatal("a traversal entry landed outside the quarantine")
			}
		})
	}
}

// TestUnknownProtocolVersionIsRefused holds the same rule skillcatalog holds
// for an unknown manifest apiVersion: a reader that guesses at an unknown
// contract is a reader that installs something it did not understand.
func TestUnknownProtocolVersionIsRefused(t *testing.T) {
	srv := registrytest.New(t, "corp")
	publishOne(t, srv)
	srv.Fail(registrytest.Failures{WrongAPIVersion: true})
	p := openProvider(t, srv, srv.Registry("corp"))

	if _, err := p.Search(context.Background(), skillregistry.Query{}); !errors.Is(err, skillregistry.ErrRegistryResponse) {
		t.Fatalf("an unknown apiVersion gave %v, want ErrRegistryResponse", err)
	}
}

// TestHTMLIsNotJSON is the captive-portal case: something answered 200 and it
// is not the registry.
func TestHTMLIsNotJSON(t *testing.T) {
	srv := registrytest.New(t, "corp")
	publishOne(t, srv)
	srv.Fail(registrytest.Failures{HTMLContentType: true})
	p := openProvider(t, srv, srv.Registry("corp"))

	if _, err := p.Search(context.Background(), skillregistry.Query{}); !errors.Is(err, skillregistry.ErrRegistryResponse) {
		t.Fatalf("an HTML answer gave %v, want ErrRegistryResponse", err)
	}
}

// TestRegistryCannotAnswerAboutAnotherRelease covers substitution: a request
// for one version that comes back as another would install something other
// than what a person approved.
func TestRegistryCannotAnswerAboutAnotherRelease(t *testing.T) {
	srv := registrytest.New(t, "corp")
	srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.2.0"})
	srv.Mutate("security-audit", "1.2.0", func(e *registrytest.Entry) {
		e.Release.Version = "9.9.9"
	})
	p := openProvider(t, srv, srv.Registry("corp"))

	_, err := p.ResolveExactRelease(context.Background(), "security-audit", "1.2.0")
	if !errors.Is(err, skillregistry.ErrRegistryResponse) {
		t.Fatalf("a substituted version gave %v, want ErrRegistryResponse", err)
	}
}

// TestResolveExactReleaseRefusesARange holds that there is no "latest" to
// install.
func TestResolveExactReleaseRefusesARange(t *testing.T) {
	srv := registrytest.New(t, "corp")
	publishOne(t, srv)
	p := openProvider(t, srv, srv.Registry("corp"))

	for _, version := range []string{"", "latest", "1.2", "^1.0.0", "1.2.x"} {
		if _, err := p.ResolveExactRelease(context.Background(), "security-audit", version); !errors.Is(err, skillregistry.ErrNoSuchRelease) {
			t.Fatalf("version %q gave %v, want ErrNoSuchRelease", version, err)
		}
	}
}

// TestRegistryGoneAfterSearch is the ordinary operational case: the answer is
// classified as unreachable, not as "the registry has nothing".
func TestRegistryGoneAfterSearch(t *testing.T) {
	srv := registrytest.New(t, "corp")
	publishOne(t, srv)
	p := openProvider(t, srv, srv.Registry("corp"))
	if _, err := p.Search(context.Background(), skillregistry.Query{}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	srv.Stop()

	if _, err := p.ResolveExactRelease(context.Background(), "security-audit", "1.2.0"); !errors.Is(err, skillregistry.ErrRegistryUnreachable) {
		t.Fatalf("a stopped registry gave %v, want ErrRegistryUnreachable", err)
	}
}
