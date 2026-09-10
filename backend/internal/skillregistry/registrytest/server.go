package registrytest

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
)

// server.go -- an AO private registry, in a test.
//
// It implements the protocol honestly by default and can be told to misbehave
// in each of the specific ways the supply-chain tests need: wrong digest, wrong
// publisher, wrong capabilities, an oversized body, a redirect somewhere else,
// a 401, a revocation appearing between a search and an install. Every one of
// those is a real attacker move, and a fixture that could only behave correctly
// would leave all of them untested.

// DNSName is the name the fixture's certificate is issued for, and the host a
// test configures its registry with. It is a .test name, which RFC 6761
// reserves for exactly this.
const DNSName = "registry.test"

// Server is a running private registry.
type Server struct {
	t  *testing.T
	ca *CA
	// UntrustedCA signed nothing here; it is the pool a test hands the client
	// to reproduce "this certificate chains to an authority AO was not given".
	UntrustedCA *CA

	http *httptest.Server

	mu sync.Mutex
	// releases is the catalogue, keyed by "<skillId>@<version>".
	releases map[string]*Entry
	// revocations is what /v1/revocations answers.
	revocations []skillregistry.Revocation
	// registryID is what the handshake reports. A test can change it to
	// reproduce "this endpoint is a different registry".
	registryID string
	// requireToken, when set, is the exact credential the server accepts.
	requireToken string
	// authHeader is where the credential is expected. Empty means
	// "Authorization: Bearer".
	authHeader string
	// failures are the deliberate misbehaviours.
	failures Failures
	// requests records what was asked for, so a test can prove that searching
	// downloaded nothing.
	requests []string
}

// Failures are the ways the fixture can misbehave on purpose.
type Failures struct {
	// RedirectArtifactTo sends the artifact request somewhere else. Used for
	// the cross-origin redirect and metadata-IP redirect tests.
	RedirectArtifactTo string
	// OversizedMetadata pads the search answer past the metadata ceiling.
	OversizedMetadata bool
	// OversizedArtifact streams a body past the artifact ceiling.
	OversizedArtifact bool
	// ArtifactBomb serves a small gzip that expands without bound.
	ArtifactBomb bool
	// ArtifactSymlink puts a symlink in the package.
	ArtifactSymlink bool
	// ArtifactTraversal names an entry outside the package root.
	ArtifactTraversal bool
	// WrongAPIVersion answers with a protocol version AO does not speak.
	WrongAPIVersion bool
	// HTMLContentType answers metadata as text/html.
	HTMLContentType bool
	// Unauthorized answers 401 to everything.
	Unauthorized bool
	// NoETag suppresses validators, so the no-conditional-request path runs.
	NoETag bool
	// ServerError answers 503 to everything.
	ServerError bool
}

// Entry is one release the fixture serves, and the bytes behind it.
type Entry struct {
	Release skillregistry.Release
	// Files is the package tree, by slash-separated relative path. It always
	// includes skill.yaml.
	Files map[string]string
}

// New starts a fixture registry with the given id.
func New(t *testing.T, registryID string) *Server {
	t.Helper()
	ca := NewCA(t)
	s := &Server{
		t: t, ca: ca, UntrustedCA: NewCA(t),
		releases:   map[string]*Entry{},
		registryID: registryID,
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	srv.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{ca.Issue(t, DNSName)},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	s.http = srv
	return s
}

// TrustPool is the CA pool a client must be given to reach this fixture.
func (s *Server) TrustPool() *x509.CertPool { return s.ca.Pool }

// Port is the port the fixture listens on.
func (s *Server) Port() int {
	addr := s.http.Listener.Addr().(*net.TCPAddr)
	return addr.Port
}

// BaseURL is what a registry row's location is set to. It is the reserved DNS
// name, not an address: AO refuses an IP literal in a registry configuration,
// and the test resolves the name with LoopbackResolver.
func (s *Server) BaseURL() string { return fmt.Sprintf("https://%s:%d", DNSName, s.Port()) }

// Options are the client options a test hands NewHTTPSProvider: this fixture's
// CA and a resolver that points its name at loopback.
func (s *Server) Options() skillregistry.HTTPSOptions {
	return skillregistry.HTTPSOptions{RootCAs: s.ca.Pool, Resolver: LoopbackResolver{}}
}

// Registry is a configuration row pointing at this fixture, with the loopback
// exception a private registry on this host genuinely needs.
func (s *Server) Registry(id string) skillregistry.Registry {
	return skillregistry.Registry{
		ID: id, DisplayName: "Fixture private registry",
		Type: skillregistry.RegistryHTTPS, Location: s.BaseURL(),
		Enabled: true, TrustPolicy: skillregistry.TrustPolicyDigest, Priority: 100,
		NetworkPolicy: skillregistry.NetworkPolicy{PermittedPrivateCIDRs: []string{"127.0.0.0/8"}},
	}
}

// RequireBearer makes the fixture demand a bearer token.
func (s *Server) RequireBearer(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requireToken, s.authHeader = token, ""
}

// RequireAPIKey makes the fixture demand a value in a named header.
func (s *Server) RequireAPIKey(header, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requireToken, s.authHeader = token, header
}

// SetRegistryID changes what the handshake reports.
func (s *Server) SetRegistryID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registryID = id
}

// Fail installs a set of deliberate misbehaviours.
func (s *Server) Fail(f Failures) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = f
}

// Stop closes the listener, which is how a test goes offline.
func (s *Server) Stop() { s.http.Close() }

// Requests returns every path the fixture was asked for, in order. It is what
// proves a search downloaded nothing.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// FetchedArtifact reports whether any artifact endpoint was hit.
func (s *Server) FetchedArtifact() bool {
	for _, p := range s.Requests() {
		if strings.HasSuffix(p, "/artifact") {
			return true
		}
	}
	return false
}

// Revoke publishes a revocation and marks the release, which is what a registry
// does when it withdraws something after people have installed it.
func (s *Server) Revoke(skillID, version, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.releases[skillID+"@"+version]; ok {
		entry.Release.Revoked = true
		entry.Release.RevocationReason = reason
	}
	s.revocations = append(s.revocations, skillregistry.Revocation{
		SkillID: skillID, Version: version, Reason: reason,
	})
}

// Remove deletes a release entirely, which is what "the registry disappears
// after a search" looks like from the client's side.
func (s *Server) Remove(skillID, version string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.releases, skillID+"@"+version)
}

// Mutate edits a stored release's metadata in place. It is how a test makes the
// registry declare one thing and serve another.
func (s *Server) Mutate(skillID, version string, edit func(*Entry)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.releases[skillID+"@"+version]; ok {
		edit(entry)
	}
}

func (s *Server) entries() []*Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Entry, 0, len(s.releases))
	for _, e := range s.releases {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Release.Ref() < out[j].Release.Ref() })
	return out
}

func (s *Server) lookup(skillID, version string) (*Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.releases[skillID+"@"+version]
	return e, ok
}

// PackageDir writes one entry's files into dir, so a test can compute the same
// digests the daemon will.
func PackageDir(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		target := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

// tarGz renders a package tree as the archive the artifact endpoint serves.
func tarGz(files map[string]string, symlink, traversal bool) []byte {
	var buf strings.Builder
	gz := gzip.NewWriter(&stringWriter{&buf})
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		body := files[name]
		_ = tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg,
		})
		_, _ = tw.Write([]byte(body))
	}
	if symlink {
		_ = tw.WriteHeader(&tar.Header{
			Name: "stolen", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd",
		})
	}
	if traversal {
		body := "owned"
		_ = tw.WriteHeader(&tar.Header{
			Name: "../../escaped.txt", Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg,
		})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return []byte(buf.String())
}

type stringWriter struct{ b *strings.Builder }

func (w *stringWriter) Write(p []byte) (int, error) { return w.b.Write(p) }

// Sha256 is the hex digest of a string, so a test can state a digest without
// importing crypto.
func Sha256(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func writeJSON(w http.ResponseWriter, body any, contentType string) {
	b, _ := json.Marshal(body)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", fmt.Sprint(len(b)))
	_, _ = w.Write(b)
}

// bombArchive is a gzip that expands far past the uncompressed budget from a
// body small enough to slip under every compressed-size check. It is streamed
// rather than built, so the fixture does not have to hold 400 MiB to prove the
// client refuses one.
func bombArchive() []byte {
	const total = int64(400) << 20
	var buf strings.Builder
	gz := gzip.NewWriter(&stringWriter{&buf})
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{
		Name: "skill.yaml", Mode: 0o600, Size: total, Typeflag: tar.TypeReg,
	})
	chunk := make([]byte, 1<<20)
	for written := int64(0); written < total; written += int64(len(chunk)) {
		if _, err := tw.Write(chunk); err != nil {
			break
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return []byte(buf.String())
}
