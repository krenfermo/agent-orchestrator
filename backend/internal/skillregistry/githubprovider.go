package skillregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillegress"
)

// githubprovider.go -- reading somebody else's repository, safely.
//
// # The five properties this file exists to hold
//
//  1. SEARCHING NEVER DOWNLOADS A PACKAGE. Search, Get, ListVersions and
//     ResolveExactRelease read JSON and one small descriptor file. The only
//     method that pulls an archive is FetchArtifact, and it runs during an
//     install a person asked for.
//
//  2. A RELEASE IS A COMMIT. Every path that produces a Release resolves the
//     tag to a full SHA first and reads the descriptor AT that SHA. There is
//     no code path here that takes a branch, and none that takes a tag as an
//     identity once resolution is done.
//
//  3. THE FORGE CHOOSES NO DESTINATION. Every URL is built from the configured
//     origin and the configured owner. GitHub's responses are full of URLs and
//     AO follows none of them; a cross-origin redirect is refused rather than
//     followed, exactly as it is for a private registry.
//
//  4. NOTHING IS UNBOUNDED. Requests per operation, repositories scanned,
//     releases scanned, descriptor size, archive size, entry count, backoff.
//     "The forge is well behaved" is the assumption every one of those exists
//     to remove -- and the repository is not the forge, it is a stranger.
//
//  5. NO SHELL, EVER. There is no git binary, no clone, no checkout, no hook
//     and no working tree. A clone runs code the repository controls before
//     anybody has looked at it, which is the opposite of what a quarantine is
//     for.
//
// # What is deliberately absent
//
// A revocation feed. GitHub does not publish one, and inventing an endpoint
// AO would poll would mean inventing semantics the forge does not have. What
// AO offers instead is administrative revocation, recorded here, of an owner,
// a repository, a commit or a key -- see RevocationSubject.

// GitHubProvider reads AO releases out of one owner's repositories.
type GitHubProvider struct {
	registryID string
	owner      string
	// repository narrows the provider to one repo. Empty means scan the
	// owner's repositories under the budget.
	repository string
	endpoints  githubEndpoints
	client     *http.Client
	creds      Credentials
	cache      *Cache
	metaTTL    time.Duration
	now        func() time.Time

	lastState MetadataState
	// lastNotes is what the last scan could not do. A partial answer that says
	// nothing about being partial is a search that lies, and Provider has no
	// second return value to say it in.
	lastNotes []error
	// lastArchive is what the last FetchArtifact unpacked, for the audit line.
	lastArchive ArchiveReport
	// lastRate is what the forge last said about how much room is left. It is
	// reported on a probe and in a rate-limit refusal, so "wait" has a time
	// attached to it rather than being advice.
	lastRate RateLimit
}

// NewGitHubProvider opens an external registry.
//
// It resolves the credential once, for the life of the provider, exactly as
// NewHTTPSProvider does. A private repository with no credential is not
// refused here: it is refused by the forge with a 404, which AO reports as
// "no such repository" -- because that is genuinely all an unauthenticated
// caller can know, and pretending otherwise would leak the existence of
// private repositories.
func NewGitHubProvider(
	ctx context.Context, reg Registry, resolver SecretResolver, opts HTTPSOptions,
) (*GitHubProvider, error) {
	if reg.Type != RegistryGitHub {
		return nil, fmt.Errorf("%w: %s is not a github registry", ErrRegistryUnreadable, reg.ID)
	}
	if err := reg.Validate(); err != nil {
		return nil, err
	}
	origin, basePath, err := ParseBaseURL(reg.Location)
	if err != nil {
		return nil, err
	}
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
	return &GitHubProvider{
		registryID: reg.ID,
		owner:      strings.TrimSpace(reg.Owner),
		repository: strings.TrimSpace(reg.Repository),
		endpoints:  githubEndpoints{origin: origin, basePath: basePath},
		// The SAME client construction a private registry gets: TLS
		// verification on with no field that turns it off, one origin, a
		// pinned dial address, no environment proxy, and a redirect off-origin
		// refused rather than followed.
		client:  newRegistryClient(guard, origin, opts.RootCAs),
		creds:   creds,
		cache:   opts.Cache,
		metaTTL: ttl,
		now:     now,
	}, nil
}

// RegistryID implements Provider.
func (p *GitHubProvider) RegistryID() string { return p.registryID }

// String renders the provider without its credential.
func (p *GitHubProvider) String() string {
	if p == nil {
		return "skillregistry.GitHubProvider(nil)"
	}
	scope := p.owner
	if p.repository != "" {
		scope = p.owner + "/" + p.repository
	}
	return fmt.Sprintf("skillregistry.GitHubProvider{registry:%s origin:%s scope:%s auth:%s ref:%s}",
		p.registryID, p.endpoints.origin, scope, p.creds.AuthType(), p.creds.Ref())
}

// GoString redacts too, so %#v does not defeat String.
func (p *GitHubProvider) GoString() string { return p.String() }

// MetadataState implements MetadataFresher.
func (p *GitHubProvider) MetadataState() MetadataState { return p.lastState.Normalized() }

// RateLimitState is what the forge last said about AO's remaining budget.
func (p *GitHubProvider) RateLimitState() RateLimit { return p.lastRate }

// Origin is what AO actually reaches. Shown on a probe so "it works in my
// browser" and "AO cannot reach it" can be compared.
func (p *GitHubProvider) Origin() string { return p.endpoints.origin.String() }

// Scope is the owner and, when narrowed, the repository.
func (p *GitHubProvider) Scope() string {
	if p.repository != "" {
		return p.owner + "/" + p.repository
	}
	return p.owner
}

// ------------------------------------------------------------------- requests

// budget is one operation's request allowance.
//
// It is a value passed down rather than a field on the provider because it is
// per OPERATION: a search may spend it all and the next search starts over. A
// counter on the provider would make the second search fail for the first
// one's reasons.
type budget struct {
	left int
}

func newBudget() *budget { return &budget{left: MaxGitHubRequests} }

func (b *budget) spend() error {
	if b == nil {
		return nil
	}
	if b.left <= 0 {
		return fmt.Errorf("%w: this operation was allowed %d requests to the forge and used them "+
			"all; what AO found is shown and is not everything there is",
			ErrBudgetExhausted, MaxGitHubRequests)
	}
	b.left--
	return nil
}

// get performs one metadata request with the cache in front of it.
//
// The order is the same as the private registry's and for the same reason: ask
// the forge, fall back to cache only when it could not be reached, and mark
// which happened. It never answers from cache while the forge is reachable and
// sitting there with a different answer.
func (p *GitHubProvider) get(
	ctx context.Context, b *budget, endpoint, accept string, limit int64,
) ([]byte, MetadataState, error) {
	if err := b.spend(); err != nil {
		return nil, MetadataState{}, err
	}
	key := endpoint
	cached, cacheErr := p.cache.GetMetadata(p.registryID, key)
	hasCache := cacheErr == nil

	body, state, err := p.roundTrip(ctx, endpoint, accept, limit, cached, hasCache)
	if err == nil {
		return body, state, nil
	}
	// A rate limit the forge asked us to wait out, briefly. One retry, never a
	// loop, and only when the wait is short enough that a person watching a
	// settings screen would not call it a hang.
	if errors.Is(err, ErrRateLimited) {
		if wait, ok := p.lastRate.Wait(p.now()); ok {
			if err := b.spend(); err != nil {
				return nil, UnreachableState(), err
			}
			select {
			case <-ctx.Done():
				return nil, UnreachableState(), ctx.Err()
			case <-time.After(wait):
			}
			return p.roundTrip(ctx, endpoint, accept, limit, cached, hasCache)
		}
	}
	return body, state, err
}

// roundTrip is one request, with conditional revalidation and the offline
// fallback. It is separate from get so the single rate-limit retry above
// re-runs exactly the request that was refused and nothing else.
func (p *GitHubProvider) roundTrip(
	ctx context.Context, endpoint, accept string, limit int64,
	cached MetadataEntry, hasCache bool,
) ([]byte, MetadataState, error) {
	reqCtx, cancel := context.WithTimeout(ctx, MetadataTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return nil, MetadataState{}, fmt.Errorf("%w: %w", ErrRegistryUnreadable, err)
	}
	p.applyHeaders(req, accept)
	if hasCache {
		if cached.ETag != "" {
			req.Header.Set("If-None-Match", cached.ETag)
		}
		if cached.LastModified != "" {
			req.Header.Set("If-Modified-Since", cached.LastModified)
		}
	}
	if err := p.creds.applyTo(req); err != nil {
		return nil, MetadataState{}, err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		if hasCache {
			return cached.Body, OfflineState(cached, p.now(), p.metaTTL), nil
		}
		return nil, UnreachableState(), classifyTransportError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	p.lastRate = readRateLimit(resp, p.now())

	if resp.StatusCode == http.StatusNotModified && hasCache {
		refreshed := cached
		refreshed.FetchedAt = p.now()
		p.cache.PutMetadata(p.registryID, endpoint, refreshed)
		return cached.Body, LiveState(p.now()), nil
	}
	if err := p.checkGitHubStatus(resp); err != nil {
		return nil, MetadataState{}, err
	}
	body, err := readBounded(resp.Body, limit)
	if err != nil {
		return nil, MetadataState{}, err
	}
	p.cache.PutMetadata(p.registryID, endpoint, MetadataEntry{
		Body:         body,
		ETag:         strings.TrimSpace(resp.Header.Get("ETag")),
		LastModified: strings.TrimSpace(resp.Header.Get("Last-Modified")),
		FetchedAt:    p.now(),
	})
	return body, LiveState(p.now()), nil
}

// getFresh is get with the cache removed from both directions. It is what
// ResolveExactRelease and the probe use: an install must act on what the forge
// says now, not on a copy.
func (p *GitHubProvider) getFresh(
	ctx context.Context, b *budget, endpoint, accept string,
) ([]byte, error) {
	if err := b.spend(); err != nil {
		return nil, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, MetadataTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRegistryUnreadable, err)
	}
	p.applyHeaders(req, accept)
	req.Header.Set("Cache-Control", "no-cache")
	if err := p.creds.applyTo(req); err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, classifyTransportError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	p.lastRate = readRateLimit(resp, p.now())
	if err := p.checkGitHubStatus(resp); err != nil {
		return nil, err
	}
	// Every no-cache read is a small one: a tag ref, a tag object, a
	// repository descriptor, or the release descriptor itself. The archive
	// does not come through here -- FetchArtifact has its own, far larger
	// ceiling -- so there is one bound and it is stated here rather than
	// passed in by four callers who would all pass the same thing.
	return readBounded(resp.Body, MaxGitHubDescriptorBytes)
}

// applyHeaders sets the headers every request carries. The credential is NOT
// among them -- it is attached by Credentials.applyTo, which re-checks the
// origin first.
func (p *GitHubProvider) applyHeaders(req *http.Request, accept string) {
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
}

// checkGitHubStatus maps a forge status onto a sentinel an operator can act on.
//
// The split that matters is 403: the forge uses it both for "your credential
// may not see this" and for "you have asked too often", and sending somebody
// to rotate a token when they need to wait ten minutes is the kind of error
// message that gets a control switched off.
func (p *GitHubProvider) checkGitHubStatus(resp *http.Response) error {
	if rateLimited(resp, p.lastRate) {
		detail := p.lastRate.Describe()
		return fmt.Errorf("%w: the forge answered %d; %s", ErrRateLimited, resp.StatusCode, detail)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		// 404 is what the forge answers for a private repository AO may not
		// see, and that is the correct thing to report: an unauthenticated
		// caller genuinely cannot tell "absent" from "not yours", and a client
		// that distinguished them would be a client that enumerates private
		// repositories.
		return fmt.Errorf("%w: the forge has no such repository, release or file, or this "+
			"installation may not see it", ErrNoSuchSkill)
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		if p.creds.Ref() == "" {
			return fmt.Errorf("%w: the forge answered %d and this registry is configured with no "+
				"credential", ErrRegistryAuth, resp.StatusCode)
		}
		return fmt.Errorf("%w: the forge answered %d for the credential %s",
			ErrRegistryAuth, resp.StatusCode, p.creds.Ref())
	case resp.StatusCode >= 500:
		return fmt.Errorf("%w: the forge answered %d", ErrRegistryUnreachable, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("%w: the forge answered %d", ErrRegistryResponse, resp.StatusCode)
	}
	return nil
}

// decodeGitHub parses a body from the FORGE's own API.
//
// It does NOT disallow unknown fields, which is the opposite of decodeStrict
// and is deliberate: the AO registry protocol is AO's contract and an
// unrecognised field there may be the one that mattered, but GitHub's REST API
// is somebody else's and grows fields constantly. Refusing to parse a repo
// descriptor because the forge added a boolean would make AO break on a
// Tuesday for no security benefit.
func decodeGitHub(body []byte, into any) error {
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("%w: the forge's answer did not parse: %w", ErrRegistryResponse, err)
	}
	return nil
}

// ------------------------------------------------------------------ resolving

// resolveTagCommit turns a tag into the full commit SHA it points at TODAY.
//
// A lightweight tag points straight at a commit. An annotated tag points at a
// tag object, which is peeled once. A tag that points at anything else -- a
// tree, a blob, another tag -- is refused rather than followed further: two
// peels is a chain, and a chain is a place for an attacker to hide a
// substitution.
func (p *GitHubProvider) resolveTagCommit(
	ctx context.Context, b *budget, owner, repo, tag string, fresh bool,
) (string, error) {
	if err := CheckImmutableRef(tag); err != nil {
		return "", err
	}
	endpoint := p.endpoints.tagRef(owner, repo, tag)
	var body []byte
	var err error
	if fresh {
		body, err = p.getFresh(ctx, b, endpoint, githubAcceptJSON)
	} else {
		var state MetadataState
		body, state, err = p.get(ctx, b, endpoint, githubAcceptJSON, MaxGitHubDescriptorBytes)
		p.lastState = state
	}
	if err != nil {
		return "", err
	}
	var ref githubRef
	if err := decodeGitHub(body, &ref); err != nil {
		return "", err
	}
	sha := strings.ToLower(strings.TrimSpace(ref.Object.SHA))
	switch strings.TrimSpace(ref.Object.Type) {
	case "commit":
		return checkCommitSHA(sha, owner, repo, tag)
	case "tag":
		// Annotated. One peel, and only one.
		if err := checkAnnotatedSHA(sha, owner, repo, tag); err != nil {
			return "", err
		}
		objEndpoint := p.endpoints.tagObject(owner, repo, sha)
		var objBody []byte
		if fresh {
			objBody, err = p.getFresh(ctx, b, objEndpoint, githubAcceptJSON)
		} else {
			var state MetadataState
			objBody, state, err = p.get(ctx, b, objEndpoint, githubAcceptJSON, MaxGitHubDescriptorBytes)
			p.lastState = state
		}
		if err != nil {
			return "", err
		}
		var obj githubTagObject
		if err := decodeGitHub(objBody, &obj); err != nil {
			return "", err
		}
		if strings.TrimSpace(obj.Object.Type) != "commit" {
			return "", fmt.Errorf("%w: tag %s in %s/%s peels to a %q and AO installs from commits",
				ErrRegistryResponse, tag, owner, repo, obj.Object.Type)
		}
		return checkCommitSHA(strings.ToLower(strings.TrimSpace(obj.Object.SHA)), owner, repo, tag)
	}
	return "", fmt.Errorf("%w: tag %s in %s/%s points at a %q, not at a commit",
		ErrRegistryResponse, tag, owner, repo, ref.Object.Type)
}

func checkCommitSHA(sha, owner, repo, tag string) (string, error) {
	if !commitRe.MatchString(sha) {
		return "", fmt.Errorf("%w: tag %s in %s/%s resolved to %q, which is not a full commit SHA",
			ErrRegistryResponse, tag, owner, repo, sha)
	}
	return sha, nil
}

func checkAnnotatedSHA(sha, owner, repo, tag string) error {
	if !commitRe.MatchString(sha) {
		return fmt.Errorf("%w: annotated tag %s in %s/%s names object %q, which is not a SHA",
			ErrRegistryResponse, tag, owner, repo, sha)
	}
	return nil
}

// readDescriptor reads .ao/release.json AT ONE COMMIT and returns the release
// it declares, with its source re-stamped from AO's own resolution.
//
// The re-stamp is the load-bearing part. A descriptor is a file a stranger
// wrote: if it were allowed to name its own owner, repository or commit, a
// repository could publish a descriptor claiming to be somebody else's code at
// somebody else's commit, and every provenance row AO wrote would record the
// lie. So the publisher may declare exactly one field of the source -- path --
// and AO fills in the rest from what it asked for.
func (p *GitHubProvider) readDescriptor(
	ctx context.Context, b *budget, owner, repo, tag, commit, visibility string, fresh bool,
) (Release, error) {
	endpoint := p.endpoints.descriptor(owner, repo, commit)
	var body []byte
	var err error
	if fresh {
		body, err = p.getFresh(ctx, b, endpoint, githubAcceptRaw)
	} else {
		var state MetadataState
		body, state, err = p.get(ctx, b, endpoint, githubAcceptRaw, MaxGitHubDescriptorBytes)
		p.lastState = state
	}
	if err != nil {
		if errors.Is(err, ErrNoSuchSkill) {
			return Release{}, fmt.Errorf("%w: %s/%s has no %s at commit %s",
				ErrNoSuchRelease, owner, repo, GitHubDescriptorPath, commit)
		}
		return Release{}, err
	}
	// AO's own contract, so the strict decoder: a field this build does not
	// understand in an AO descriptor may be the one that mattered.
	var parsed releaseBody
	if err := decodeStrict(body, &parsed); err != nil {
		return Release{}, err
	}
	if err := checkAPIVersion(parsed.APIVersion); err != nil {
		return Release{}, err
	}
	rel := parsed.Release
	rel.RegistryID = p.registryID
	declaredPath := strings.TrimSpace(rel.Source.Path)
	rel.Source = GitSource{
		Provider:   SourceGitHub,
		Owner:      owner,
		Repository: repo,
		Tag:        NormalizeTag(tag),
		Commit:     commit,
		Path:       declaredPath,
		Visibility: visibility,
	}
	if err := rel.Validate(); err != nil {
		return Release{}, fmt.Errorf("%w: %s at commit %s: %w",
			ErrRegistryResponse, rel.Ref(), commit, err)
	}
	return rel, nil
}

// ------------------------------------------------------------------- scanning

// repoRef is one repository in scope and what the forge said about it.
type repoRef struct {
	owner      string
	name       string
	visibility string
}

// scope returns the repositories this registry covers.
//
// A registry narrowed to one repository costs ONE request and describes it, so
// the visibility a provenance row records is the forge's word rather than a
// guess. A registry naming only an owner lists the account's repositories,
// organization endpoint first, capped at MaxGitHubRepoScan.
func (p *GitHubProvider) scope(ctx context.Context, b *budget) ([]repoRef, error) {
	if p.repository != "" {
		info, err := p.describeRepo(ctx, b, p.owner, p.repository)
		if err != nil {
			return nil, err
		}
		return []repoRef{info}, nil
	}
	body, state, err := p.get(ctx, b,
		p.endpoints.orgRepos(p.owner, MaxGitHubRepoScan), githubAcceptJSON, MaxMetadataBytes)
	p.lastState = state
	if err != nil {
		if !errors.Is(err, ErrNoSuchSkill) {
			return nil, err
		}
		// Not an organization. An account is one or the other and the name
		// does not say which, so the fallback is the contract rather than a
		// retry loop: exactly one alternative, tried once.
		body, state, err = p.get(ctx, b,
			p.endpoints.userRepos(p.owner, MaxGitHubRepoScan), githubAcceptJSON, MaxMetadataBytes)
		p.lastState = state
		if err != nil {
			return nil, err
		}
	}
	var repos []githubRepo
	if err := decodeGitHub(body, &repos); err != nil {
		return nil, err
	}
	out := make([]repoRef, 0, len(repos))
	for _, r := range repos {
		if r.Archived || r.Disabled {
			continue
		}
		name := strings.TrimSpace(r.Name)
		login := strings.TrimSpace(r.Owner.Login)
		if login == "" {
			login = p.owner
		}
		// The forge answering about an account AO did not ask about is a
		// substitution, and it is refused rather than filtered: a listing that
		// silently dropped it would hide that it happened.
		if !strings.EqualFold(login, p.owner) {
			return nil, fmt.Errorf("%w: asked for %s's repositories and the forge answered about %s",
				ErrRegistryResponse, p.owner, login)
		}
		if !repoRe.MatchString(name) {
			continue
		}
		out = append(out, repoRef{owner: p.owner, name: name, visibility: r.visibility()})
		if len(out) >= MaxGitHubRepoScan {
			break
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %s has no repository AO can read", ErrNoSuchSkill, p.owner)
	}
	return out, nil
}

func (p *GitHubProvider) describeRepo(
	ctx context.Context, b *budget, owner, repo string,
) (repoRef, error) {
	body, state, err := p.get(ctx, b, p.endpoints.repo(owner, repo),
		githubAcceptJSON, MaxGitHubDescriptorBytes)
	p.lastState = state
	if err != nil {
		return repoRef{}, err
	}
	var info githubRepo
	if err := decodeGitHub(body, &info); err != nil {
		return repoRef{}, err
	}
	if name := strings.TrimSpace(info.Name); name != "" && !strings.EqualFold(name, repo) {
		return repoRef{}, fmt.Errorf("%w: asked for %s/%s and the forge answered about %s",
			ErrRegistryResponse, owner, repo, info.FullName)
	}
	return repoRef{owner: owner, name: repo, visibility: info.visibility()}, nil
}

// releasesOf lists one repository's published releases, newest first, dropping
// drafts.
//
// A DRAFT is dropped because it is not published: its tag may not exist yet,
// and a listing that offered one would offer something that cannot be
// resolved. A PRERELEASE is kept and marked deprecated in the Release, because
// "there is a newer stable one" is exactly what deprecated already means on
// this surface and hiding it would answer "where is the beta" with silence.
func (p *GitHubProvider) releasesOf(
	ctx context.Context, b *budget, repo repoRef, limit int,
) ([]githubRelease, error) {
	if limit <= 0 || limit > MaxGitHubReleaseScan {
		limit = MaxGitHubReleaseScan
	}
	body, state, err := p.get(ctx, b, p.endpoints.releases(repo.owner, repo.name, limit),
		githubAcceptJSON, MaxMetadataBytes)
	p.lastState = state
	if err != nil {
		return nil, err
	}
	var releases []githubRelease
	if err := decodeGitHub(body, &releases); err != nil {
		return nil, err
	}
	out := make([]githubRelease, 0, len(releases))
	for _, r := range releases {
		if r.Draft {
			continue
		}
		if strings.TrimSpace(r.TagName) == "" {
			continue
		}
		// A release whose "tag" is a branch name is refused at the door. It is
		// the FASE A rule and it is enforced here as well as in GitSource,
		// because a listing that dropped it silently would make the refusal
		// invisible to the person who has to go and fix the publishing.
		if CheckImmutableRef(r.TagName) != nil {
			continue
		}
		out = append(out, r)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// collect resolves the releases of every repository in scope into Releases.
//
// It stops on a budget exhaustion and reports what it had, wrapped in
// ErrBudgetExhausted, so the caller can say "this is not everything" rather
// than presenting a truncated list as the whole answer. A single release that
// fails to resolve is SKIPPED rather than failing the scan: on a forge, a
// repository with one malformed descriptor among twenty tags is an ordinary
// Tuesday, and refusing the whole registry for it would make one publisher's
// mistake everybody's outage.
func (p *GitHubProvider) collect(
	ctx context.Context, b *budget, perRepo int,
) ([]Release, []error) {
	repos, err := p.scope(ctx, b)
	if err != nil {
		return nil, []error{err}
	}
	var out []Release
	var problems []error
	for _, repo := range repos {
		releases, err := p.releasesOf(ctx, b, repo, perRepo)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s/%s: %w", repo.owner, repo.name, err))
			if fatalScanError(err) {
				return out, problems
			}
			continue
		}
		for _, gr := range releases {
			rel, err := p.resolveRelease(ctx, b, repo, gr.TagName, false)
			if err != nil {
				problems = append(problems, fmt.Errorf("%s/%s tag %s: %w",
					repo.owner, repo.name, gr.TagName, err))
				if fatalScanError(err) {
					return out, problems
				}
				continue
			}
			if gr.Prerelease {
				rel.Deprecated = true
				if rel.DeprecationNote == "" {
					rel.DeprecationNote = "the forge marks this release as a prerelease"
				}
			}
			out = append(out, rel)
		}
	}
	return out, problems
}

// fatalScanError reports whether a failure means the scan cannot usefully
// continue. A rate limit and an exhausted budget both do: every further
// request would fail the same way, and hammering a forge that just said stop
// is the aggressive loop FASE H exists to forbid.
func fatalScanError(err error) bool {
	return errors.Is(err, ErrRateLimited) ||
		errors.Is(err, ErrBudgetExhausted) ||
		errors.Is(err, ErrRegistryAuth) ||
		errors.Is(err, ErrRegistryUnreachable) ||
		errors.Is(err, ErrNetworkPolicy)
}

// resolveRelease turns one repository and tag into a pinned Release.
func (p *GitHubProvider) resolveRelease(
	ctx context.Context, b *budget, repo repoRef, tag string, fresh bool,
) (Release, error) {
	commit, err := p.resolveTagCommit(ctx, b, repo.owner, repo.name, tag, fresh)
	if err != nil {
		return Release{}, err
	}
	return p.readDescriptor(ctx, b, repo.owner, repo.name, tag, commit, repo.visibility, fresh)
}

// ------------------------------------------------------------------- Provider

// Search implements Provider. It moves no package bytes.
func (p *GitHubProvider) Search(ctx context.Context, q Query) ([]Release, error) {
	q = q.Normalized()
	b := newBudget()
	perRepo := MaxGitHubReleaseScan
	if p.repository == "" {
		// Scanning an account multiplies repositories by releases, and the
		// budget is the only thing standing between a search and somebody
		// else's rate limit. Fewer releases per repository is the cheaper half
		// to give up: a person looking across an org wants breadth.
		perRepo = 5
	}
	releases, problems := p.collect(ctx, b, perRepo)
	if len(releases) == 0 && len(problems) > 0 {
		return nil, problems[0]
	}
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
		if q.Text != "" && !matchesExternalText(rel, q.Text) {
			continue
		}
		out = append(out, rel)
	}
	SortReleases(out)
	if limit := q.EffectiveLimit(); len(out) > limit {
		out = out[:limit]
	}
	// A partial answer that says nothing about being partial is a search that
	// lies. The budget refusal is returned ALONGSIDE nothing -- there is no
	// second return value on Provider -- so it is folded into the error only
	// when there is otherwise nothing to show, and reported as a note by the
	// service when there is. Callers that want both use ScanNotes.
	p.lastNotes = problems
	return out, nil
}

// matchesExternalText is the client-side text filter for an external scan.
//
// The forge has a code-search API and AO does not use it: search there is
// rate-limited far harder, requires authentication for anything useful, and
// would mean sending the person's query -- which can name an internal package
// or a vulnerability they are hunting -- to a third party. Filtering locally
// costs nothing on a bounded scan and sends nobody anything.
//
// It is a second function rather than a widening of matchesText because it
// matches one extra field, owner/repository, which is the thing a person
// actually types when they are looking for somebody else's skill.
func matchesExternalText(rel Release, needle string) bool {
	if matchesText(rel, needle) {
		return true
	}
	return strings.Contains(strings.ToLower(rel.Source.Slug()), needle)
}

// ScanNotes returns what the last scan could not do.
//
// It is a method rather than a return value because Provider's shape is fixed
// and shared with two other implementations, and widening it for the one that
// scans would make every fixture return an empty slice nobody reads.
func (p *GitHubProvider) ScanNotes() []error { return p.lastNotes }

// Get implements Provider: the DISPLAY read.
func (p *GitHubProvider) Get(ctx context.Context, skillID, version string) (Release, error) {
	versions, err := p.ListVersions(ctx, skillID)
	if err != nil {
		return Release{}, err
	}
	for _, rel := range versions {
		if version == "" || rel.Version == version {
			return rel, nil
		}
	}
	return Release{}, fmt.Errorf("%w: %s@%s", ErrNoSuchRelease, skillID, version)
}

// ListVersions implements Provider, including deprecated and revoked releases.
func (p *GitHubProvider) ListVersions(ctx context.Context, skillID string) ([]Release, error) {
	b := newBudget()
	perRepo := MaxGitHubReleaseScan
	if p.repository == "" {
		perRepo = 10
	}
	releases, problems := p.collect(ctx, b, perRepo)
	out := make([]Release, 0, len(releases))
	for _, rel := range releases {
		if rel.SkillID == skillID {
			out = append(out, rel)
		}
	}
	if len(out) == 0 {
		if len(problems) > 0 {
			return nil, problems[0]
		}
		return nil, fmt.Errorf("%w: %s", ErrNoSuchSkill, skillID)
	}
	SortReleases(out)
	p.lastNotes = problems
	return out, nil
}

// ResolveExactRelease implements Provider: the PRE-INSTALL read.
//
// It re-reads from the forge with no cache in either direction, and it
// re-resolves the TAG TO A COMMIT while doing so. That second part is what
// makes moved-tag detection possible at all: the commit this returns is the
// one the tag points at RIGHT NOW, and the caller compares it against what it
// recorded the last time it looked.
func (p *GitHubProvider) ResolveExactRelease(
	ctx context.Context, skillID, version string,
) (Release, error) {
	version = strings.TrimSpace(version)
	if version == "" {
		return Release{}, fmt.Errorf("%w: a version is required; there is no latest to install",
			ErrNoSuchRelease)
	}
	if _, err := skillcatalog.ParseVersion(version); err != nil {
		return Release{}, fmt.Errorf("%w: %q is not one exact MAJOR.MINOR.PATCH version",
			ErrNoSuchRelease, version)
	}
	b := newBudget()
	repos, err := p.scope(ctx, b)
	if err != nil {
		return Release{}, err
	}
	for _, repo := range repos {
		releases, err := p.releasesOf(ctx, b, repo, MaxGitHubReleaseScan)
		if err != nil {
			if fatalScanError(err) {
				return Release{}, err
			}
			continue
		}
		for _, gr := range releases {
			// The tag is resolved FRESH, every time, on the install path. A
			// cached tag-to-commit answer is exactly the thing a moved tag
			// would hide behind.
			rel, err := p.resolveRelease(ctx, b, repo, gr.TagName, true)
			if err != nil {
				if fatalScanError(err) {
					return Release{}, err
				}
				continue
			}
			if rel.SkillID == skillID && rel.Version == version {
				p.lastState = LiveState(p.now())
				return rel, nil
			}
		}
	}
	return Release{}, fmt.Errorf("%w: %s@%s is not published by %s",
		ErrNoSuchRelease, skillID, version, p.Scope())
}

// FetchArtifact implements Provider: the ONLY method that moves package bytes.
//
// It fetches the forge's archive OF THE PINNED COMMIT -- never of a tag, never
// of a branch, and never a publisher-uploaded asset. The commit comes from the
// release the caller resolved, so the bytes and the identity cannot come apart:
// there is no parameter here that could name a different tree.
func (p *GitHubProvider) FetchArtifact(ctx context.Context, rel Release, destDir string) error {
	src := rel.Source
	if !src.Declared() {
		return artifactRefusedf("%s names no source commit; an external release is installed from "+
			"one exact commit or not at all", rel.Ref())
	}
	if err := src.Validate(); err != nil {
		return artifactRefusedf("%s: %v", rel.Ref(), err)
	}
	if !strings.EqualFold(src.Owner, p.owner) {
		// The release AO is about to fetch belongs to an account this registry
		// is not configured for. It cannot happen through resolveRelease,
		// which stamps the owner itself; it is checked because FetchArtifact
		// takes a Release from a caller.
		return fmt.Errorf("%w: %s is owned by %s and this registry reads %s",
			ErrNetworkPolicy, src.Slug(), src.Owner, p.owner)
	}
	if p.repository != "" && !strings.EqualFold(src.Repository, p.repository) {
		return fmt.Errorf("%w: %s is not the repository this registry reads (%s/%s)",
			ErrNetworkPolicy, src.Slug(), p.owner, p.repository)
	}

	reqCtx, cancel := context.WithTimeout(ctx, ArtifactTimeout)
	defer cancel()
	endpoint := p.endpoints.tarball(src.Owner, src.Repository, src.Commit)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRegistryUnreadable, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	if err := p.creds.applyTo(req); err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return classifyTransportError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	p.lastRate = readRateLimit(resp, p.now())
	if err := p.checkGitHubStatus(resp); err != nil {
		return err
	}
	if err := checkArchiveContentType(resp); err != nil {
		return err
	}
	if resp.ContentLength > MaxArtifactDownloadBytes {
		return artifactRefusedf("the forge declares %d bytes and the ceiling is %d",
			resp.ContentLength, MaxArtifactDownloadBytes)
	}
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return fmt.Errorf("%w: %w", ErrArtifactRefused, err)
	}
	report, err := UnpackArchive(resp.Body, destDir, ArchiveOptionsFor(src))
	if err != nil {
		return err
	}
	p.lastArchive = report
	return nil
}

// LastArchive is what the most recent FetchArtifact unpacked. The service
// audits it: which archive root was stripped and how many entries fell outside
// the package are the two facts that explain a digest that does not match.
func (p *GitHubProvider) LastArchive() ArchiveReport { return p.lastArchive }

// checkArchiveContentType refuses an answer that is plainly not an archive.
//
// It is deliberately permissive about which gzip spelling arrives -- forges
// and their CDNs disagree -- and strict about the two that mean something went
// wrong: an HTML page is a login screen or an error, and JSON is the API
// answering a different question.
func checkArchiveContentType(resp *http.Response) error {
	raw := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if raw == "" {
		return nil
	}
	got, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return fmt.Errorf("%w: content type %q is malformed", ErrRegistryResponse, raw)
	}
	switch strings.ToLower(got) {
	case "text/html", "application/json", "application/vnd.github+json":
		return fmt.Errorf("%w: the archive endpoint answered %s; an archive is a gzipped tar",
			ErrRegistryResponse, got)
	}
	return nil
}

// --------------------------------------------------------------------- probe

// Probe implements ConnectionProbe.
//
// CONNECTED here means the same demanding thing it means for a private
// registry: not that a socket opened and not that a 200 came back, but that
// the forge answered about the EXACT repository or account this registry is
// configured for. A 200 from a proxy, a login page or a different account is
// INVALID_RESPONSE.
func (p *GitHubProvider) Probe(ctx context.Context, now func() time.Time) ProbeResult {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	result := ProbeResult{
		RegistryID: p.registryID,
		Origin:     p.endpoints.origin.String(),
		AuthType:   string(p.creds.AuthType()),
		SecretRef:  p.creds.Ref(),
		TestedAt:   now(),
	}
	probeCtx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	b := newBudget()
	started := now()
	endpoint := p.endpoints.repo(p.owner, p.repository)
	scope := p.owner + "/" + p.repository
	if p.repository == "" {
		endpoint = p.endpoints.orgRepos(p.owner, 1)
		scope = p.owner
	}
	body, err := p.getFresh(probeCtx, b, endpoint, githubAcceptJSON)
	result.Latency = now().Sub(started)
	if err != nil {
		if p.repository == "" && errors.Is(err, ErrNoSuchSkill) {
			body, err = p.getFresh(probeCtx, b, p.endpoints.userRepos(p.owner, 1), githubAcceptJSON)
			result.Latency = now().Sub(started)
		}
		if err != nil {
			result.State, result.Detail = classifyGitHubProbeError(err, p.creds, p.lastRate)
			return result
		}
	}
	result.ProtocolVersion = githubAPIVersion
	if p.repository != "" {
		var info githubRepo
		if err := decodeGitHub(body, &info); err != nil {
			result.State, result.Detail = ProbeInvalidResponse, err.Error()
			return result
		}
		full := strings.TrimSpace(info.FullName)
		if full != "" && !strings.EqualFold(full, scope) {
			result.State = ProbeInvalidResponse
			result.ReportedRegistryID = full
			result.Detail = fmt.Sprintf("this endpoint answered about %q and this registry is "+
				"configured for %q; AO will not treat one repository's answers as another's",
				full, scope)
			return result
		}
		result.ReportedRegistryID = full
		result.State = ProbeConnected
		result.Detail = fmt.Sprintf("%s answered about %s (%s) over verified TLS. %s. Metadata "+
			"only: no archive was downloaded and nothing was installed, enabled or approved. "+
			"Reaching this repository says nothing about who wrote the code in it.",
			p.endpoints.origin, scope, info.visibility(), p.lastRate.Describe())
		return result
	}
	var repos []githubRepo
	if err := decodeGitHub(body, &repos); err != nil {
		result.State, result.Detail = ProbeInvalidResponse, err.Error()
		return result
	}
	result.ReportedRegistryID = scope
	result.State = ProbeConnected
	result.Detail = fmt.Sprintf("%s answered for the account %s over verified TLS. %s. Metadata "+
		"only: no archive was downloaded and nothing was installed, enabled or approved. Reaching "+
		"this account says nothing about who wrote the code in it.",
		p.endpoints.origin, scope, p.lastRate.Describe())
	return result
}

// classifyGitHubProbeError maps a failed forge request onto one of the six
// probe states, with the rate limit split out from an authorization failure.
func classifyGitHubProbeError(err error, creds Credentials, limit RateLimit) (ProbeState, string) {
	if errors.Is(err, ErrRateLimited) {
		// Not AUTH_FAILED and not UNREACHABLE. The registry is fine, the
		// credential is fine, and AO has asked too often -- which is a third
		// thing, and the only one whose fix is a clock.
		return ProbeInvalidResponse, fmt.Sprintf("%v. This is not a configuration problem: %s",
			err, limit.Describe())
	}
	if errors.Is(err, ErrBudgetExhausted) {
		return ProbeInvalidResponse, err.Error()
	}
	if errors.Is(err, ErrNoSuchSkill) {
		return ProbeInvalidResponse, fmt.Sprintf("%v. On a forge, a repository that does not "+
			"exist and one this installation may not see answer identically", err)
	}
	return classifyProbeError(err, creds, Origin{})
}
