package skillregistry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillegress"
)

// httpsprovider.go -- the client for a company-private registry.
//
// # The four properties this file exists to hold
//
//  1. SEARCHING NEVER DOWNLOADS. Search, Get, ListVersions and
//     ResolveExactRelease each call one metadata endpoint and parse JSON.
//     FetchArtifact is the only method that reads a package body, and it is
//     called from exactly one place: an install a person asked for.
//
//  2. THE REGISTRY CHOOSES NO DESTINATION. Every URL is built from the
//     configured origin; a redirect off that origin is refused rather than
//     followed; and no URL that appears in a release is ever fetched. That is
//     the difference between an HTTP client and an SSRF gadget.
//
//  3. THE CREDENTIAL GOES ONE PLACE. It is attached by Credentials.applyTo,
//     which re-checks the origin, and it is never logged, cached, persisted or
//     put in a URL.
//
//  4. NOTHING IS UNBOUNDED. Timeouts, response size, artifact size,
//     uncompressed size, entry count and redirect count all have a ceiling,
//     because "the registry is trusted to be reasonable" is the assumption
//     every one of those limits exists to remove.
//
// # What is deliberately absent
//
// A "skip TLS verification" option, an "allow http" option, and a per-registry
// timeout or size override. Each would be a per-registry way to turn off the
// control that makes the others meaningful, and an installation that needs one
// has a problem a checkbox should not solve.

// HTTPSOptions are the knobs a provider is built with. Everything here has a
// safe zero value, and the two fields a test uses are documented as such --
// there is no configuration path that reaches them.
type HTTPSOptions struct {
	// RootCAs replaces the system trust store. It is nil in production: the
	// daemon never sets it, and there is no configuration field that does.
	//
	// It exists for the HTTPS fixture in this package's tests, which stands a
	// real TLS server with a certificate it generated. A test that disabled
	// verification instead would be a test that proves the client works with
	// verification off.
	RootCAs *x509.CertPool
	// Resolver replaces DNS. Nil means the system resolver. Tests use it to
	// point a real DNS name at the fixture's loopback address and to simulate
	// a rebinding answer.
	Resolver Resolver
	// Cache is the metadata/artifact cache. Nil is a working "no cache".
	Cache *Cache
	// Now is the clock, for tests that need a deterministic staleness.
	Now func() time.Time
}

// HTTPSProvider reads one private registry over HTTPS.
type HTTPSProvider struct {
	registryID string
	endpoints  endpoints
	client     *http.Client
	creds      Credentials
	cache      *Cache
	metaTTL    time.Duration
	now        func() time.Time
	// lastFreshness records how the most recent metadata answer was obtained,
	// so the service can say OFFLINE/STALE on screen without every method
	// growing a second return value the Provider interface does not have.
	lastFreshness Freshness
}

// NewHTTPSProvider opens a private registry.
//
// It resolves the credential ONCE, here, for the life of the provider. The
// alternative -- resolving per request -- would read the sealed store on every
// keystroke of a search, and the provider is already short-lived: the service
// opens one per operation.
func NewHTTPSProvider(
	ctx context.Context, reg Registry, resolver SecretResolver, opts HTTPSOptions,
) (*HTTPSProvider, error) {
	if reg.Type != RegistryHTTPS {
		return nil, fmt.Errorf("%w: %s is not an https registry", ErrRegistryUnreadable, reg.ID)
	}
	if err := reg.Validate(); err != nil {
		return nil, err
	}
	origin, basePath, err := ParseBaseURL(reg.Location)
	if err != nil {
		return nil, err
	}
	// The same validator the skill egress proxy uses, so an exception that
	// would re-open link-local is refused here exactly as it is there.
	permitted, err := skillegress.ParsePermittedCIDRs(reg.NetworkPolicy.PermittedPrivateCIDRs)
	if err != nil {
		return nil, badConfigf("networkPolicy: %v", err)
	}
	creds, err := resolveCredentials(ctx, reg, origin, resolver)
	if err != nil {
		return nil, err
	}
	lookup := opts.Resolver
	if lookup == nil {
		lookup = systemResolver{}
	}
	guard := addressGuard{origin: origin, resolver: lookup, permitted: permitted}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	ttl := DefaultCacheLimits().MetadataTTL
	if opts.Cache != nil {
		ttl = opts.Cache.Limits().MetadataTTL
	}
	return &HTTPSProvider{
		registryID: reg.ID,
		endpoints:  endpoints{origin: origin, basePath: basePath},
		client:     newRegistryClient(guard, origin, opts.RootCAs),
		creds:      creds,
		cache:      opts.Cache,
		metaTTL:    ttl,
		now:        now,
	}, nil
}

// newRegistryClient builds the one HTTP client this registry may use.
func newRegistryClient(guard addressGuard, origin Origin, roots *x509.CertPool) *http.Client {
	transport := &http.Transport{
		DialContext: guard.dialContext,
		TLSClientConfig: &tls.Config{
			// Verification is ON, and there is no field anywhere that turns it
			// off. MinVersion is stated rather than left to the default so a
			// future Go default cannot quietly lower it.
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
			// The certificate must be valid for the configured host, not for
			// whatever the connection ended up at. The dialer connects to a
			// pinned IP, so without this the handshake would be verified
			// against an address.
			ServerName: origin.Host,
		},
		// A connection to one origin is all this client will ever make.
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: time.Second,
		// A proxy from the environment would be a destination nobody
		// configured, seeing every request AO makes to its private registry.
		Proxy: nil,
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > MaxRedirects {
				return fmt.Errorf("%w: more than %d redirects", ErrNetworkPolicy, MaxRedirects)
			}
			if !origin.Matches(req.URL) {
				// The refusal is the control. Following it and stripping the
				// credential would still fetch a body chosen by whoever
				// controls the registry, from a host nobody authorized.
				return fmt.Errorf("%w: the registry redirected to %s://%s, which is not %s",
					ErrNetworkPolicy, req.URL.Scheme, req.URL.Host, origin)
			}
			return nil
		},
	}
}

// RegistryID implements Provider.
func (p *HTTPSProvider) RegistryID() string { return p.registryID }

// String renders the provider without its credential.
//
// Credentials already redacts in every path fmt has, so this is belt to that
// braces -- but a provider is the object somebody logs when a request fails,
// and %+v on a struct is how a field that was careful about String ends up
// printed by reflection anyway.
func (p *HTTPSProvider) String() string {
	if p == nil {
		return "skillregistry.HTTPSProvider(nil)"
	}
	return fmt.Sprintf("skillregistry.HTTPSProvider{registry:%s origin:%s auth:%s ref:%s}",
		p.registryID, p.endpoints.origin, p.creds.AuthType(), p.creds.Ref())
}

// GoString redacts too, so %#v does not defeat String.
func (p *HTTPSProvider) GoString() string { return p.String() }

// Freshness reports how the last metadata answer was obtained. It is what the
// service turns into "OFFLINE / STALE METADATA" on screen.
func (p *HTTPSProvider) Freshness() Freshness {
	if p.lastFreshness == "" {
		return FreshnessLive
	}
	return p.lastFreshness
}

// ------------------------------------------------------------------ requests

// get performs one metadata request, with the cache in front of it.
//
// The order is the design: try the registry, fall back to cache when the
// registry cannot be reached, and mark the result. A cache-first client would
// be a client that answers from a copy while the registry is sitting there with
// a revocation.
func (p *HTTPSProvider) get(ctx context.Context, endpoint string) ([]byte, Freshness, error) {
	key := endpoint
	cached, cacheErr := p.cache.GetMetadata(p.registryID, key)
	hasCache := cacheErr == nil

	reqCtx, cancel := context.WithTimeout(ctx, MetadataTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrRegistryUnreadable, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if hasCache {
		// Conditional revalidation. A registry that supports it answers 304
		// and AO pays no body; one that does not answers 200 and nothing is
		// lost.
		if cached.ETag != "" {
			req.Header.Set("If-None-Match", cached.ETag)
		}
		if cached.LastModified != "" {
			req.Header.Set("If-Modified-Since", cached.LastModified)
		}
	}
	if err := p.creds.applyTo(req); err != nil {
		return nil, "", err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		if hasCache {
			// Offline. The answer is the one AO already verified it received,
			// and it is marked stale whatever its age -- AO could not ask, and
			// "could not ask" is not "unchanged".
			return cached.Body, FreshnessStale, nil
		}
		return nil, "", classifyTransportError(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotModified && hasCache {
		refreshed := cached
		refreshed.FetchedAt = p.now()
		p.cache.PutMetadata(p.registryID, key, refreshed)
		return cached.Body, FreshnessLive, nil
	}
	if err := checkStatus(resp, p.creds.Ref()); err != nil {
		return nil, "", err
	}
	if err := checkContentType(resp, "application/json"); err != nil {
		return nil, "", err
	}
	body, err := readBounded(resp.Body, MaxMetadataBytes)
	if err != nil {
		return nil, "", err
	}
	p.cache.PutMetadata(p.registryID, key, MetadataEntry{
		Body:         body,
		ETag:         strings.TrimSpace(resp.Header.Get("ETag")),
		LastModified: strings.TrimSpace(resp.Header.Get("Last-Modified")),
		FetchedAt:    p.now(),
	})
	return body, FreshnessLive, nil
}

const userAgent = "ao-skill-registry/1 (+https://github.com/aoagents/agent-orchestrator)"

// readBounded reads at most limit bytes and refuses a body that exceeds it.
// Reading limit+1 is what proves the body was too large rather than exactly at
// the edge.
func readBounded(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading the response failed: %v", ErrRegistryUnreachable, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%w: the response is larger than %d bytes", ErrRegistryResponse, limit)
	}
	return body, nil
}

// checkStatus maps a status onto the sentinel an operator can act on.
func checkStatus(resp *http.Response, secretRef string) error {
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: the registry has no such entry", ErrNoSuchSkill)
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		// The ref, never the value. There is no code path here that has the
		// value in a string.
		if secretRef == "" {
			return fmt.Errorf("%w: the registry answered %d and this registry is configured with no "+
				"credential", ErrRegistryAuth, resp.StatusCode)
		}
		return fmt.Errorf("%w: the registry answered %d for the credential %s",
			ErrRegistryAuth, resp.StatusCode, secretRef)
	case resp.StatusCode >= 500:
		return fmt.Errorf("%w: the registry answered %d", ErrRegistryUnreachable, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("%w: the registry answered %d", ErrRegistryResponse, resp.StatusCode)
	}
	return nil
}

// checkContentType refuses an answer whose type is not the one asked for.
//
// It matters more than it looks: an HTML error page that parses as "not JSON"
// produces a confusing message, and a captive portal or a misrouted proxy
// answering 200 with HTML is exactly the case where a clear refusal beats a
// parse failure.
func checkContentType(resp *http.Response, want string) error {
	raw := resp.Header.Get("Content-Type")
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("%w: the response declares no content type", ErrRegistryResponse)
	}
	got, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return fmt.Errorf("%w: content type %q is malformed", ErrRegistryResponse, raw)
	}
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("%w: the response is %s and this endpoint serves %s", ErrRegistryResponse, got, want)
	}
	return nil
}

// classifyTransportError splits a failed request into the four things an
// operator would do something different about.
func classifyTransportError(err error) error {
	if err == nil {
		return nil
	}
	// A policy refusal comes back wrapped in *url.Error; unwrapping to the
	// sentinel is what keeps "blocked by policy" from reading as "the network
	// is down".
	if errors.Is(err, ErrNetworkPolicy) {
		return unwrapPolicy(err)
	}
	if errors.Is(err, ErrRegistryUnreachable) {
		return fmt.Errorf("%w: %v", ErrRegistryUnreachable, redactURLError(err))
	}
	var certErr *tls.CertificateVerificationError
	var hostErr x509.HostnameError
	var authErr x509.UnknownAuthorityError
	var invalidErr x509.CertificateInvalidError
	if errors.As(err, &certErr) || errors.As(err, &hostErr) ||
		errors.As(err, &authErr) || errors.As(err, &invalidErr) {
		return fmt.Errorf("%w: %v", ErrRegistryTLS, redactURLError(err))
	}
	if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
		return fmt.Errorf("%w: the request timed out", ErrRegistryUnreachable)
	}
	return fmt.Errorf("%w: %v", ErrRegistryUnreachable, redactURLError(err))
}

func unwrapPolicy(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err
	}
	return err
}

// redactURLError drops the *url.Error wrapper, which repeats the URL.
//
// The URL holds no credential -- this package never puts one in a URL -- but
// the wrapper turns every message into "Get \"https://...\": ..." twice over,
// and an error an operator cannot read is an error they route around.
func redactURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err
	}
	return err
}

// ------------------------------------------------------------------ metadata

// Search implements Provider. It reads one metadata endpoint and moves no
// package bytes; there is nowhere in the response schema for a package to be.
func (p *HTTPSProvider) Search(ctx context.Context, q Query) ([]Release, error) {
	q = q.Normalized()
	body, freshness, err := p.get(ctx, p.endpoints.search(q))
	if err != nil {
		return nil, err
	}
	p.lastFreshness = freshness
	releases, err := p.decodeReleases(body)
	if err != nil {
		return nil, err
	}
	// The registry's filtering is a courtesy; AO's is the contract. A registry
	// that ignored includeRevoked would otherwise put a withdrawn release in
	// an ordinary listing.
	out := make([]Release, 0, len(releases))
	for _, rel := range releases {
		if rel.Revoked && !q.IncludeRevoked {
			continue
		}
		if rel.Deprecated && !q.IncludeDeprecated {
			continue
		}
		if q.Publisher != "" && rel.Publisher != q.Publisher {
			continue
		}
		if q.Capability != "" && !containsString(rel.RequestedCapabilities, q.Capability) {
			continue
		}
		out = append(out, rel)
	}
	SortReleases(out)
	if limit := q.EffectiveLimit(); len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Get implements Provider: the DISPLAY read.
func (p *HTTPSProvider) Get(ctx context.Context, skillID, version string) (Release, error) {
	body, freshness, err := p.get(ctx, p.endpoints.release(skillID, version))
	if err != nil {
		return Release{}, err
	}
	p.lastFreshness = freshness
	return p.decodeRelease(body, skillID, version)
}

// ListVersions implements Provider, including deprecated and revoked releases.
func (p *HTTPSProvider) ListVersions(ctx context.Context, skillID string) ([]Release, error) {
	body, freshness, err := p.get(ctx, p.endpoints.versions(skillID))
	if err != nil {
		if errors.Is(err, ErrNoSuchSkill) {
			return nil, fmt.Errorf("%w: %s", ErrNoSuchSkill, skillID)
		}
		return nil, err
	}
	p.lastFreshness = freshness
	releases, err := p.decodeReleases(body)
	if err != nil {
		return nil, err
	}
	out := make([]Release, 0, len(releases))
	for _, rel := range releases {
		if rel.SkillID != skillID {
			// A versions listing that answers about another skill is a
			// registry answering a different question than the one asked.
			return nil, fmt.Errorf("%w: the versions of %s include %s",
				ErrRegistryResponse, skillID, rel.Ref())
		}
		out = append(out, rel)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchSkill, skillID)
	}
	SortReleases(out)
	return out, nil
}

// ResolveExactRelease implements Provider: the PRE-INSTALL read.
//
// It requires one complete version and it does NOT accept a cached answer. An
// install acting on a copy is exactly what revocation has to prevent, so the
// only acceptable outcomes here are "the registry answered just now" and "the
// registry could not be reached". The second is not an error this method
// invents a value for; the caller decides whether an offline install is
// permitted, and it has the persisted revocation record to check.
func (p *HTTPSProvider) ResolveExactRelease(ctx context.Context, skillID, version string) (Release, error) {
	version = strings.TrimSpace(version)
	if version == "" {
		return Release{}, fmt.Errorf("%w: a version is required; there is no latest to install",
			ErrNoSuchRelease)
	}
	if _, err := skillcatalog.ParseVersion(version); err != nil {
		return Release{}, fmt.Errorf("%w: %q is not one exact MAJOR.MINOR.PATCH version",
			ErrNoSuchRelease, version)
	}
	body, err := p.getFresh(ctx, p.endpoints.release(skillID, version))
	if err != nil {
		if errors.Is(err, ErrNoSuchSkill) {
			return Release{}, fmt.Errorf("%w: %s@%s", ErrNoSuchRelease, skillID, version)
		}
		return Release{}, err
	}
	p.lastFreshness = FreshnessLive
	return p.decodeRelease(body, skillID, version)
}

// getFresh is get with the cache removed from both directions: no conditional
// request, no fallback, and nothing stored. It is what the install path and the
// connection test use.
func (p *HTTPSProvider) getFresh(ctx context.Context, endpoint string) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, MetadataTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRegistryUnreadable, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Cache-Control", "no-cache")
	if err := p.creds.applyTo(req); err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, classifyTransportError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := checkStatus(resp, p.creds.Ref()); err != nil {
		return nil, err
	}
	if err := checkContentType(resp, "application/json"); err != nil {
		return nil, err
	}
	return readBounded(resp.Body, MaxMetadataBytes)
}

// decodeReleases parses a many-release body and validates every entry.
//
// A malformed entry fails the WHOLE answer rather than being skipped. A search
// that silently dropped the release somebody was looking for would be a search
// that lies about what a registry offers, and the entry AO cannot parse is
// disproportionately likely to be the interesting one.
func (p *HTTPSProvider) decodeReleases(body []byte) ([]Release, error) {
	var parsed releaseListBody
	if err := decodeStrict(body, &parsed); err != nil {
		return nil, err
	}
	if err := checkAPIVersion(parsed.APIVersion); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]Release, 0, len(parsed.Releases))
	for i := range parsed.Releases {
		rel := parsed.Releases[i]
		// AO's configured id, always. A payload that carried its own would let
		// one registry claim to be another, and every install would record the
		// wrong origin.
		rel.RegistryID = p.registryID
		if err := rel.Validate(); err != nil {
			return nil, fmt.Errorf("%w: release %s: %w", ErrRegistryResponse, rel.Ref(), err)
		}
		if seen[rel.Ref()] {
			// Two rows under one identity, and whichever the reader hits first
			// wins: the mutable-version attack in its simplest form.
			return nil, fmt.Errorf("%w: %s is listed twice; a version is one immutable thing",
				ErrRegistryResponse, rel.Ref())
		}
		seen[rel.Ref()] = true
		out = append(out, rel)
	}
	return out, nil
}

func (p *HTTPSProvider) decodeRelease(body []byte, skillID, version string) (Release, error) {
	var parsed releaseBody
	if err := decodeStrict(body, &parsed); err != nil {
		return Release{}, err
	}
	if err := checkAPIVersion(parsed.APIVersion); err != nil {
		return Release{}, err
	}
	rel := parsed.Release
	rel.RegistryID = p.registryID
	if err := rel.Validate(); err != nil {
		return Release{}, fmt.Errorf("%w: release %s: %w", ErrRegistryResponse, rel.Ref(), err)
	}
	// The registry answering about a different release than the one asked for
	// is the substitution this check exists to catch: a request for 1.0.0 that
	// returns 1.0.1's metadata would install 1.0.1 under the name a person
	// approved.
	if rel.SkillID != skillID {
		return Release{}, fmt.Errorf("%w: asked for %s and the registry answered about %s",
			ErrRegistryResponse, skillID, rel.SkillID)
	}
	if version != "" && rel.Version != version {
		return Release{}, fmt.Errorf("%w: asked for %s@%s and the registry answered with %s",
			ErrRegistryResponse, skillID, version, rel.Version)
	}
	return rel, nil
}

// decodeStrict refuses unknown fields, exactly as the local index reader does.
// A field this build does not understand may be the one that mattered.
func decodeStrict(body []byte, into any) error {
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("%w: %v", ErrRegistryResponse, err)
	}
	return nil
}

// ------------------------------------------------------------------- bytes

// FetchArtifact implements Provider. It is the ONLY method here that moves
// package content, and it runs only during an install.
//
// It writes nothing outside destDir, refuses every entry that is not a regular
// file or a directory, and bounds the compressed body, the uncompressed total,
// each file and the entry count. Whether the bytes are the RIGHT bytes is the
// caller's check, made against the release it resolved.
func (p *HTTPSProvider) FetchArtifact(ctx context.Context, rel Release, destDir string) error {
	reqCtx, cancel := context.WithTimeout(ctx, ArtifactTimeout)
	defer cancel()
	endpoint := p.endpoints.artifact(rel.SkillID, rel.Version)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRegistryUnreadable, err)
	}
	req.Header.Set("Accept", ArtifactMediaType)
	req.Header.Set("User-Agent", userAgent)
	if err := p.creds.applyTo(req); err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return classifyTransportError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := checkStatus(resp, p.creds.Ref()); err != nil {
		return err
	}
	if err := checkArtifactContentType(resp); err != nil {
		return err
	}
	// A declared length over the ceiling is refused before a byte is read.
	// It is a courtesy check, not the control: the control is the limited
	// reader below, because Content-Length is whatever the registry said.
	if resp.ContentLength > MaxArtifactDownloadBytes {
		return artifactRefusedf("the registry declares %d bytes and the ceiling is %d",
			resp.ContentLength, MaxArtifactDownloadBytes)
	}
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return fmt.Errorf("%w: %v", ErrArtifactRefused, err)
	}
	if _, err := UnpackArtifact(resp.Body, destDir); err != nil {
		return err
	}
	return nil
}

// checkArtifactContentType accepts the AO package type and the two generic
// gzip types a plain file server sends. It refuses text/html and application/
// json outright, which is what a login page or an error body arrives as.
func checkArtifactContentType(resp *http.Response) error {
	raw := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if raw == "" {
		// A body with no declared type is the one case where refusing costs
		// more than it buys: the bytes are about to be gzip-parsed, digest-
		// checked and manifest-checked regardless.
		return nil
	}
	got, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return fmt.Errorf("%w: content type %q is malformed", ErrRegistryResponse, raw)
	}
	switch strings.ToLower(got) {
	case ArtifactMediaType, "application/gzip", "application/x-gzip",
		"application/x-tar", "application/octet-stream":
		return nil
	}
	return fmt.Errorf("%w: the artifact endpoint answered %s; a package is %s",
		ErrRegistryResponse, got, ArtifactMediaType)
}

// ----------------------------------------------------------------- revocation

// FetchRevocations reads the registry's withdrawal list.
//
// It is NOT cached and never falls back: a stale revocation list is the one
// piece of registry state where an old copy is actively dangerous, because the
// whole point of the endpoint is to learn something new. When the registry
// cannot be reached, the caller keeps what it has already persisted and says
// so, rather than being handed a list that looks current.
func (p *HTTPSProvider) FetchRevocations(ctx context.Context) ([]Revocation, error) {
	body, err := p.getFresh(ctx, p.endpoints.revocations())
	if err != nil {
		return nil, err
	}
	var parsed revocationListBody
	if err := decodeStrict(body, &parsed); err != nil {
		return nil, err
	}
	if err := checkAPIVersion(parsed.APIVersion); err != nil {
		return nil, err
	}
	out := make([]Revocation, 0, len(parsed.Revocations))
	for _, rev := range parsed.Revocations {
		if err := rev.Validate(); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrRegistryResponse, err)
		}
		out = append(out, rev)
	}
	return out, nil
}
