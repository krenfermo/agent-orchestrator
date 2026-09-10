// Package githubtest is a GitHub-compatible forge, in a test.
//
// # Why this exists and github.com does not
//
// Nothing in this repository's test suite may reach the internet. A test that
// hit github.com would be a test that fails when somebody's wifi does, that
// leaks the fact that AO is being tested to a third party, that cannot
// reproduce a rate limit on demand, and -- the one that actually matters --
// that cannot move a tag. Every interesting property of an external registry
// is a property of a forge MISBEHAVING, and a fixture is the only place a
// forge misbehaves on request.
//
// # What it implements
//
// Exactly the endpoints GitHubProvider calls, and nothing else: the repository
// descriptor, the releases list, a tag ref, an annotated tag object, the
// contents endpoint at a ref, the tarball endpoint, and the two
// repository-listing endpoints. An endpoint AO does not call is an endpoint
// this fixture would be describing rather than testing.
//
// # What it can do wrong, on purpose
//
// Move a tag between two calls; point a tag at a branch; serve an archive of a
// different commit; lie about a digest; strip the descriptor; answer 403 with
// and without rate-limit counters; answer 429 with a Retry-After; refuse a
// credential; go away entirely; return an archive with a symlink, a hard link,
// a traversal, a device node, too many entries, or one that expands without
// bound. Each of those is a real thing that has happened to a real supply
// chain, and a fixture that could only behave correctly would leave all of
// them untested.
package githubtest

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry/registrytest"
)

// DNSName is the name the fixture's certificate is issued for, and the host a
// registry configuration points at. A .test name, which RFC 6761 reserves for
// exactly this.
const DNSName = "forge.test"

// Server is a running fixture forge.
type Server struct {
	t  *testing.T
	ca *registrytest.CA

	http *httptest.Server
	port int

	mu sync.Mutex
	// repos is every repository this forge hosts, keyed "owner/name".
	repos map[string]*Repo
	// requireToken, when set, is the exact bearer credential accepted.
	requireToken string
	// failures are the deliberate misbehaviours.
	failures Failures
	// rate is what the forge reports about AO's remaining budget.
	rate rateState
	// requests records every path asked for, in order, so a test can prove a
	// search downloaded no archive.
	requests []string
}

// Repo is one repository and everything in it.
type Repo struct {
	Owner   string
	Name    string
	Private bool
	// Archived and Disabled are dropped from an owner scan.
	Archived bool
	Disabled bool
	// tags maps a tag name to the commit it points at. Moving a tag is
	// re-pointing an entry here, which is exactly what a force-push is.
	tags map[string]string
	// annotated marks a tag as an annotated tag object, which AO has to peel.
	annotated map[string]bool
	// commits maps a commit SHA to the tree at that commit.
	commits map[string]map[string]string
	// releases is the published release list, newest first.
	releases []Release
}

// Release is one entry in the forge's releases listing.
type Release struct {
	Tag         string
	Name        string
	Draft       bool
	Prerelease  bool
	PublishedAt time.Time
}

// Failures are the ways this forge can misbehave on purpose.
type Failures struct {
	// Unauthorized answers 401 to everything.
	Unauthorized bool
	// Forbidden answers 403 with NO rate-limit counters, which is how a forge
	// says "your credential may not see this".
	Forbidden bool
	// RateLimited answers 403 WITH remaining=0, which is how the same forge
	// says "you have asked too often". Telling the two apart is the whole
	// point of having both.
	RateLimited bool
	// TooManyRequests answers 429 with the Retry-After below.
	TooManyRequests bool
	// RetryAfter is the header value sent with a 429 or a rate-limited 403.
	RetryAfter time.Duration
	// ServerError answers 503 to everything.
	ServerError bool
	// ArchiveOfCommit serves this commit's tree whatever commit was asked for.
	// It is the "wrong commit archive" case: the forge answers 200 with real,
	// well-formed bytes that are not the ones AO pinned.
	ArchiveOfCommit string
	// ArchiveSymlink, ArchiveHardlink, ArchiveTraversal, ArchiveDevice add one
	// hostile entry to every archive.
	ArchiveSymlink   bool
	ArchiveHardlink  bool
	ArchiveTraversal bool
	ArchiveDevice    bool
	// ArchiveBomb serves a small gzip that expands past the budget.
	ArchiveBomb bool
	// ArchiveEntryFlood serves more entries than the ceiling allows.
	ArchiveEntryFlood bool
	// ArchiveTwoRoots serves an archive with two top-level directories, which
	// a repository archive never has and a merge would resolve by luck.
	ArchiveTwoRoots bool
	// ArchiveNoRoot serves an archive with no synthetic root at all.
	ArchiveNoRoot bool
	// OversizedArchive streams a body past the compressed ceiling.
	OversizedArchive bool
	// HTMLArchive answers the archive endpoint with a login page.
	HTMLArchive bool
	// OversizedDescriptor pads the descriptor past its ceiling.
	OversizedDescriptor bool
	// NoETag suppresses validators so the no-conditional-request path runs.
	NoETag bool
	// TagPointsAtTree makes every tag ref answer with a tree object rather
	// than a commit.
	TagPointsAtTree bool
}

type rateState struct {
	limit     int
	remaining int
	resetAt   time.Time
	set       bool
}

// New starts a fixture forge.
func New(t *testing.T) *Server {
	t.Helper()
	ca := registrytest.NewCA(t)
	s := &Server{t: t, ca: ca, repos: map[string]*Repo{}}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	srv.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{ca.Issue(t, DNSName)},
	}
	srv.StartTLS()
	s.http = srv
	if addr, ok := srv.Listener.Addr().(*net.TCPAddr); ok {
		s.port = addr.Port
	}
	t.Cleanup(func() { s.current().Close() })
	return s
}

func (s *Server) current() *httptest.Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.http
}

// Port is the port the fixture listens on, fixed across Pause and Resume.
func (s *Server) Port() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.port
}

// BaseURL is what a registry row's location is set to: the reserved DNS name,
// because AO refuses an IP literal in a registry configuration.
func (s *Server) BaseURL() string { return fmt.Sprintf("https://%s:%d", DNSName, s.Port()) }

// TrustPool is the CA a client must be given to reach this fixture.
func (s *Server) TrustPool() *x509.CertPool { return s.ca.Pool }

// Options are the client options a test hands NewGitHubProvider.
func (s *Server) Options() skillregistry.HTTPSOptions {
	return skillregistry.HTTPSOptions{
		RootCAs:  s.ca.Pool,
		Resolver: registrytest.LoopbackResolver{},
	}
}

// Registry is a configuration row pointing at this fixture.
//
// The loopback exception is real and necessary: the fixture is on this host,
// and AO refuses a private address without one. It is the same exception a
// private registry fixture needs, and it is written down rather than assumed.
func (s *Server) Registry(id, owner, repository string) skillregistry.Registry {
	return skillregistry.Registry{
		ID:          id,
		DisplayName: "Fixture forge",
		Type:        skillregistry.RegistryGitHub,
		Location:    s.BaseURL(),
		Owner:       owner,
		Repository:  repository,
		Enabled:     true,
		TrustPolicy: skillregistry.TrustPolicyExternalIntegrity,
		Priority:    100,
		NetworkPolicy: skillregistry.NetworkPolicy{
			PermittedPrivateCIDRs: []string{"127.0.0.0/8"},
		},
	}
}

// RequireBearer makes the fixture demand a bearer token.
func (s *Server) RequireBearer(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requireToken = token
}

// Fail installs a set of deliberate misbehaviours.
func (s *Server) Fail(f Failures) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = f
}

// SetRateLimit makes the forge report a remaining budget.
func (s *Server) SetRateLimit(limit, remaining int, resetAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rate = rateState{limit: limit, remaining: remaining, resetAt: resetAt, set: true}
}

// Stop closes the listener, which is how a test takes the forge offline.
func (s *Server) Stop() { s.current().Close() }

// Pause is Stop with the intent named: a test that pauses will Resume.
func (s *Server) Pause() { s.Stop() }

// Resume brings the forge back at the same host and port with the same
// contents, which is what proves offline is a STATE and not a latch.
func (s *Server) Resume() {
	s.t.Helper()
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", s.Port()))
	if err != nil {
		s.t.Fatalf("githubtest: resuming on port %d: %v", s.Port(), err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{s.ca.Issue(s.t, DNSName)},
	}
	srv.StartTLS()
	s.mu.Lock()
	s.http = srv
	s.mu.Unlock()
}

// Requests returns every path the fixture was asked for, in order.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// FetchedArchive reports whether any tarball endpoint was hit. It is what
// proves a search downloaded nothing.
func (s *Server) FetchedArchive() bool {
	for _, p := range s.Requests() {
		if strings.Contains(p, "/tarball/") {
			return true
		}
	}
	return false
}

// RequestCount is how many requests the fixture has served.
func (s *Server) RequestCount() int { return len(s.Requests()) }

// ResetRequests clears the request log, so a test can count one operation.
func (s *Server) ResetRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = nil
}

// ------------------------------------------------------------------ contents

// AddRepo registers a repository.
func (s *Server) AddRepo(owner, name string, private bool) *Repo {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := &Repo{
		Owner: owner, Name: name, Private: private,
		tags:      map[string]string{},
		annotated: map[string]bool{},
		commits:   map[string]map[string]string{},
	}
	s.repos[strings.ToLower(owner+"/"+name)] = r
	return r
}

func (s *Server) repo(owner, name string) (*Repo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[strings.ToLower(owner+"/"+name)]
	return r, ok
}

// Commit adds a tree at a commit SHA.
func (r *Repo) Commit(sha string, files map[string]string) {
	copied := map[string]string{}
	for k, v := range files {
		copied[k] = v
	}
	r.commits[sha] = copied
}

// Tag points a tag at a commit, publishing a release for it.
//
// Calling it twice for one tag with different commits is a FORCE-PUSH, which
// is the whole of the moved-tag scenario.
func (r *Repo) Tag(tag, commit string) {
	r.tags[tag] = commit
	for _, rel := range r.releases {
		if rel.Tag == tag {
			return
		}
	}
	r.releases = append([]Release{{
		Tag: tag, Name: tag,
		PublishedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}}, r.releases...)
}

// MoveTag re-points an existing tag without touching the release listing. It
// is the attack: the name is unchanged and the bytes behind it are not.
func (r *Repo) MoveTag(tag, commit string) { r.tags[tag] = commit }

// Annotate marks a tag as annotated, so AO has to peel it.
func (r *Repo) Annotate(tag string) { r.annotated[tag] = true }

// PublishRaw appends a release entry directly, for drafts, prereleases and
// releases whose "tag" is a branch name.
func (r *Repo) PublishRaw(rel Release) {
	r.releases = append([]Release{rel}, r.releases...)
}

// TreeOf returns the files at a commit.
func (r *Repo) TreeOf(sha string) map[string]string { return r.commits[sha] }

// CommitOf returns the commit a tag points at.
func (r *Repo) CommitOf(tag string) string { return r.tags[tag] }

// ------------------------------------------------------------------- serving

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.URL.Path)
	fail := s.failures
	token := s.requireToken
	rate := s.rate
	s.mu.Unlock()

	s.writeRateHeaders(w, rate, fail)

	if fail.ServerError {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if fail.TooManyRequests {
		if fail.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(fail.RetryAfter.Seconds())))
		}
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	if fail.RateLimited {
		// A 403 that carries remaining=0 is the forge saying "too often", and
		// it must not be readable as "your token is wrong".
		w.Header().Set("X-RateLimit-Limit", "60")
		w.Header().Set("X-RateLimit-Remaining", "0")
		if fail.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(fail.RetryAfter.Seconds())))
		}
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if fail.Unauthorized {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if fail.Forbidden {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if token != "" {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got != token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}

	path := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(path, "/")
	switch {
	case len(parts) == 3 && parts[0] == "orgs" && parts[2] == "repos":
		s.serveOwnerRepos(w, parts[1], true)
	case len(parts) == 3 && parts[0] == "users" && parts[2] == "repos":
		s.serveOwnerRepos(w, parts[1], false)
	case len(parts) == 3 && parts[0] == "repos":
		s.serveRepo(w, parts[1], parts[2])
	case len(parts) == 4 && parts[0] == "repos" && parts[3] == "releases":
		s.serveReleases(w, parts[1], parts[2], fail)
	case len(parts) >= 6 && parts[0] == "repos" && parts[3] == "git" && parts[4] == "ref" &&
		parts[5] == "tags":
		s.serveTagRef(w, parts[1], parts[2], strings.Join(parts[6:], "/"), fail)
	case len(parts) == 6 && parts[0] == "repos" && parts[3] == "git" && parts[4] == "tags":
		s.serveTagObject(w, parts[1], parts[2], parts[5])
	case len(parts) >= 5 && parts[0] == "repos" && parts[3] == "contents":
		s.serveContents(w, r, parts[1], parts[2], strings.Join(parts[4:], "/"), fail)
	case len(parts) == 5 && parts[0] == "repos" && parts[3] == "tarball":
		s.serveTarball(w, parts[1], parts[2], parts[4], fail)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *Server) writeRateHeaders(w http.ResponseWriter, rate rateState, fail Failures) {
	if !rate.set {
		return
	}
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(rate.limit))
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(rate.remaining))
	if !rate.resetAt.IsZero() {
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(rate.resetAt.Unix(), 10))
	}
	_ = fail
}

func (s *Server) serveOwnerRepos(w http.ResponseWriter, owner string, org bool) {
	s.mu.Lock()
	var out []map[string]any
	keys := make([]string, 0, len(s.repos))
	for k := range s.repos {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	found := false
	for _, k := range keys {
		r := s.repos[k]
		if !strings.EqualFold(r.Owner, owner) {
			continue
		}
		found = true
		out = append(out, repoJSON(r))
	}
	s.mu.Unlock()
	if !found {
		// An owner with no repositories is indistinguishable from an owner
		// that does not exist, and on the org endpoint it is also how "this
		// account is a user, not an organization" arrives.
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_ = org
	writeJSON(w, out)
}

func (s *Server) serveRepo(w http.ResponseWriter, owner, name string) {
	r, ok := s.repo(owner, name)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	writeJSON(w, repoJSON(r))
}

func repoJSON(r *Repo) map[string]any {
	return map[string]any{
		"full_name": r.Owner + "/" + r.Name,
		"name":      r.Name,
		"owner":     map[string]any{"login": r.Owner},
		"private":   r.Private,
		"archived":  r.Archived,
		"disabled":  r.Disabled,
	}
}

func (s *Server) serveReleases(w http.ResponseWriter, owner, name string, fail Failures) {
	r, ok := s.repo(owner, name)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	out := make([]map[string]any, 0, len(r.releases))
	for _, rel := range r.releases {
		out = append(out, map[string]any{
			"tag_name":     rel.Tag,
			"name":         rel.Name,
			"draft":        rel.Draft,
			"prerelease":   rel.Prerelease,
			"created_at":   rel.PublishedAt,
			"published_at": rel.PublishedAt,
			// Fields AO does not read, present on purpose: the lenient decoder
			// for forge bodies has to stay lenient, and a fixture that served
			// only the fields AO reads would never prove it.
			"html_url":    "https://example.invalid/ignored",
			"tarball_url": "https://example.invalid/ignored",
			"id":          1,
		})
	}
	if !fail.NoETag {
		w.Header().Set("ETag", `"releases-`+strconv.Itoa(len(out))+`"`)
	}
	writeJSON(w, out)
}

func (s *Server) serveTagRef(w http.ResponseWriter, owner, name, tag string, fail Failures) {
	r, ok := s.repo(owner, name)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	commit, ok := r.tags[tag]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	objType := "commit"
	sha := commit
	if fail.TagPointsAtTree {
		objType = "tree"
	} else if r.annotated[tag] {
		objType = "tag"
		sha = annotatedSHA(commit)
	}
	writeJSON(w, map[string]any{
		"ref":    "refs/tags/" + tag,
		"object": map[string]any{"sha": sha, "type": objType},
	})
}

// annotatedSHA is the tag object's own SHA, deterministically derived so a
// test can predict it. A real forge's would be a hash of the tag object.
func annotatedSHA(commit string) string {
	return strings.Repeat("a", 4) + commit[4:]
}

func (s *Server) serveTagObject(w http.ResponseWriter, owner, name, sha string) {
	r, ok := s.repo(owner, name)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	for tag, commit := range r.tags {
		if r.annotated[tag] && annotatedSHA(commit) == sha {
			writeJSON(w, map[string]any{
				"sha":    sha,
				"object": map[string]any{"sha": commit, "type": "commit"},
			})
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
}

func (s *Server) serveContents(
	w http.ResponseWriter, r *http.Request, owner, name, filePath string, fail Failures,
) {
	repo, ok := s.repo(owner, name)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	ref := r.URL.Query().Get("ref")
	tree, ok := repo.commits[ref]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	body, ok := tree[filePath]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if fail.OversizedDescriptor {
		body += strings.Repeat(" ", int(skillregistry.MaxGitHubDescriptorBytes)+1)
	}
	if !fail.NoETag {
		w.Header().Set("ETag", `"`+registrytest.Sha256(body)[:16]+`"`)
	}
	w.Header().Set("Content-Type", "application/vnd.github.raw; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write([]byte(body))
}

func (s *Server) serveTarball(w http.ResponseWriter, owner, name, commit string, fail Failures) {
	repo, ok := s.repo(owner, name)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if fail.HTMLArchive {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body>sign in</body></html>"))
		return
	}
	served := commit
	if fail.ArchiveOfCommit != "" {
		// The forge answers 200 with a real, well-formed archive of a
		// DIFFERENT commit. Nothing about the transport is wrong; the bytes
		// are simply not the ones AO pinned, and only the digest check catches
		// it.
		served = fail.ArchiveOfCommit
	}
	tree, ok := repo.commits[served]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/x-gzip")
	if fail.ArchiveBomb {
		_, _ = w.Write(bombArchive(archiveRoot(owner, name, served)))
		return
	}
	if fail.OversizedArchive {
		_, _ = w.Write(oversizedArchive(archiveRoot(owner, name, served)))
		return
	}
	_, _ = w.Write(tarGz(archiveRoot(owner, name, served), tree, fail))
}

// archiveRoot is the synthetic top-level directory a forge adds. AO strips
// exactly one level, and the fixture produces one so that stripping is
// exercised rather than assumed.
func archiveRoot(owner, name, commit string) string {
	short := commit
	if len(short) > 7 {
		short = short[:7]
	}
	return owner + "-" + name + "-" + short
}

func writeJSON(w http.ResponseWriter, body any) {
	b, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	_, _ = w.Write(b)
}

// tarGz renders a repository tree as the archive the tarball endpoint serves.
func tarGz(root string, files map[string]string, fail Failures) []byte {
	var buf strings.Builder
	gz := gzip.NewWriter(&stringWriter{&buf})
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: root + "/", Mode: 0o755, Typeflag: tar.TypeDir})
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	prefix := root + "/"
	if fail.ArchiveNoRoot {
		prefix = ""
	}
	for _, name := range names {
		body := files[name]
		_ = tw.WriteHeader(&tar.Header{
			Name: prefix + name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		})
		_, _ = tw.Write([]byte(body))
	}
	if fail.ArchiveSymlink {
		_ = tw.WriteHeader(&tar.Header{
			Name: prefix + "stolen", Mode: 0o777,
			Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd",
		})
	}
	if fail.ArchiveHardlink {
		_ = tw.WriteHeader(&tar.Header{
			Name: prefix + "hard-link-entry", Mode: 0o644,
			Typeflag: tar.TypeLink, Linkname: "/etc/passwd",
		})
	}
	if fail.ArchiveTraversal {
		body := "owned"
		_ = tw.WriteHeader(&tar.Header{
			Name: prefix + "../../escaped.txt", Mode: 0o644,
			Size: int64(len(body)), Typeflag: tar.TypeReg,
		})
		_, _ = tw.Write([]byte(body))
	}
	if fail.ArchiveDevice {
		_ = tw.WriteHeader(&tar.Header{
			Name: prefix + "dev/null", Mode: 0o666, Typeflag: tar.TypeChar,
			Devmajor: 1, Devminor: 3,
		})
	}
	if fail.ArchiveTwoRoots {
		body := "second root"
		_ = tw.WriteHeader(&tar.Header{
			Name: "other-root/skill.yaml", Mode: 0o644,
			Size: int64(len(body)), Typeflag: tar.TypeReg,
		})
		_, _ = tw.Write([]byte(body))
	}
	if fail.ArchiveEntryFlood {
		for i := 0; i <= skillregistry.MaxArtifactEntries; i++ {
			name := fmt.Sprintf("%sfiller/%06d.txt", prefix, i)
			_ = tw.WriteHeader(&tar.Header{
				Name: name, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg,
			})
			_, _ = tw.Write([]byte("x"))
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return []byte(buf.String())
}

// bombArchive is a gzip that expands far past the uncompressed budget from a
// body small enough to slip under every compressed-size check.
func bombArchive(root string) []byte {
	const total = int64(400) << 20
	var buf strings.Builder
	gz := gzip.NewWriter(&stringWriter{&buf})
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{
		Name: root + "/skill.yaml", Mode: 0o644, Size: total, Typeflag: tar.TypeReg,
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

// oversizedArchive is a body past the COMPRESSED ceiling, which is a different
// refusal from the bomb: one is caught by the download limit and the other by
// the expansion budget, and a system that only had one of them would be missing
// half the problem.
func oversizedArchive(root string) []byte {
	var buf strings.Builder
	gz := gzip.NewWriter(&stringWriter{&buf})
	tw := tar.NewWriter(gz)
	// MANY files, each comfortably under the per-file ceiling, adding up past
	// the COMPRESSED download ceiling. One enormous file would be caught by
	// the per-file budget instead, which is a different control -- and a test
	// that hit the wrong one would prove the wrong thing.
	const each = int64(8) << 20
	count := int(skillregistry.MaxArtifactDownloadBytes/each) + 2
	chunk := make([]byte, 1<<20)
	for f := 0; f < count; f++ {
		_ = tw.WriteHeader(&tar.Header{
			Name: fmt.Sprintf("%s/blob-%02d.bin", root, f), Mode: 0o644,
			Size: each, Typeflag: tar.TypeReg,
		})
		for written := int64(0); written < each; written += int64(len(chunk)) {
			// Refilled from crypto/rand every megabyte. A repeating pattern
			// would compress to almost nothing and the fixture would quietly
			// stop testing the ceiling it exists to test.
			if _, err := rand.Read(chunk); err != nil {
				break
			}
			if _, err := tw.Write(chunk); err != nil {
				break
			}
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return []byte(buf.String())
}

type stringWriter struct{ b *strings.Builder }

func (w *stringWriter) Write(p []byte) (int, error) { return w.b.Write(p) }
