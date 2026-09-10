package skillregistry

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// github.go -- the wire contract AO speaks to a GitHub-compatible API, and the
// exact, small set of requests it will ever make.
//
// # The publishing contract, in one paragraph
//
// A repository publishes AO skills as GITHUB RELEASES. Each release is a tag.
// At the COMMIT that tag points at, the repository holds one file at a fixed
// path -- .ao/release.json -- which is the same releaseBody the private
// registry protocol already defines. AO resolves the tag to a commit, reads
// that file AT THAT COMMIT, and pins everything to the commit from then on.
//
// # Why the descriptor is a file in the repository and not a release asset
//
// A release asset can be replaced without the tag moving and without the
// commit changing. A file at a commit cannot: changing it produces a different
// commit, and AO is pinned to the one it resolved. The descriptor is the thing
// that names the digests AO will check the bytes against, so it is precisely
// the thing that must not be swappable underneath a resolved release.
//
// # Why the artifact is the repository archive at that commit
//
// Same reason, one step further. A tarball uploaded as a release asset was
// built from something, and nothing about it says what. The forge's own
// archive of a commit is generated FROM the commit, so "the bytes AO installed
// came from this commit" is a statement the transport supports rather than one
// the publisher asserts. AO therefore fetches /tarball/{sha} and never a
// publisher-supplied asset -- see UnpackArchive for what it does with it.
//
// # Every path is built here, never received
//
// Nothing in a GitHub response is followed as a URL. The API hands back
// url, html_url, tarball_url, browser_download_url and half a dozen more, and
// AO reads none of them: it builds /repos/{owner}/{repo}/... from the
// CONFIGURED origin and the CONFIGURED owner. That is the same rule
// protocol.go states for a private registry, and it matters more here, because
// the party writing those URLs is not the party that runs the installation.

// GitHub API paths. Every dynamic segment is escaped by the builders below.
const (
	githubAcceptJSON = "application/vnd.github+json"
	// githubAcceptRaw asks the contents endpoint for the file's bytes rather
	// than a JSON envelope with base64 in it. AO bounds the response either
	// way; the raw form is one less encoding to get wrong.
	githubAcceptRaw = "application/vnd.github.raw"
	// githubAPIVersion pins the REST contract. A forge that ships a breaking
	// change on a new version cannot reach this client by default.
	githubAPIVersion = "2022-11-28"
	// GitHubDescriptorPath is where a repository publishes its AO release
	// descriptor, relative to the repository root, at the tagged commit.
	GitHubDescriptorPath = ".ao/release.json"
)

// The scan budgets. They are constants for the same reason the size limits in
// artifact.go are: an installation that could raise them is an installation
// that can be configured into spending somebody else's rate limit, and the
// number that matters is "enough for a real publisher and small enough that a
// mistake is cheap".
const (
	// MaxGitHubReleaseScan is how many of a repository's most recent releases
	// AO will look at in one search.
	MaxGitHubReleaseScan = 20
	// MaxGitHubRepoScan is how many repositories AO will look at when a
	// registry names an owner and no repository.
	MaxGitHubRepoScan = 25
	// MaxGitHubRequests is the hard request budget for ONE provider operation.
	// It bounds the multiplication of the two numbers above: a search over an
	// owner is repos x releases, and neither of those is a number AO controls.
	MaxGitHubRequests = 120
	// MaxGitHubDescriptorBytes bounds one release descriptor. It is far
	// smaller than MaxMetadataBytes because this is one release, not a listing.
	MaxGitHubDescriptorBytes int64 = 256 << 10
	// MaxGitHubRetryAfter is the longest AO will wait and retry when a forge
	// asks it to back off. Past this, the operation reports the rate limit
	// rather than sleeping: a person is watching a settings screen, and a
	// client that quietly blocks for an hour is one nobody can tell apart from
	// a hang.
	MaxGitHubRetryAfter = 10 * time.Second
)

// ErrRateLimited means the forge refused because AO has asked too often.
//
// It is its own sentinel and it is NOT merged into ErrRegistryUnreachable,
// even though both mean "no answer right now": unreachable sends an operator
// to look at the network, and a rate limit is a thing that resolves by waiting
// and that a shared credential makes worse. They also differ in one way that
// matters for correctness -- an install may fall back to verified cached bytes
// when a registry is unreachable, and a rate limit is exactly the case where
// AO could ask again in a minute and get a live, authoritative answer.
var ErrRateLimited = errors.New("skillregistry: the forge rate-limited this installation")

// ErrBudgetExhausted means one operation hit MaxGitHubRequests.
//
// It is surfaced rather than swallowed: a search that stopped early has told
// the caller less than the registry offers, and returning a short list with no
// note would be a search that quietly lies about what is available.
var ErrBudgetExhausted = errors.New("skillregistry: the request budget for this operation ran out")

// RateLimit is what the forge said about how much room is left.
//
// It is carried so a settings screen can say "resets at 14:20" rather than
// "try again later", which is the difference between a person waiting and a
// person retrying in a loop.
type RateLimit struct {
	// Limit and Remaining are the forge's counters, -1 when it sent none.
	Limit     int `json:"limit"`
	Remaining int `json:"remaining"`
	// ResetAt is when the window rolls over, zero when unknown.
	ResetAt time.Time `json:"resetAt,omitzero"`
	// RetryAfter is an explicit backoff the forge asked for, zero when none.
	RetryAfter time.Duration `json:"retryAfter,omitzero"`
	// Observed reports whether any of this came from a real response. A zero
	// RateLimit with Observed false means AO has not been told anything, which
	// is different from having been told the limit is zero.
	Observed bool `json:"observed"`
}

// Exhausted reports whether the forge says there is nothing left.
func (r RateLimit) Exhausted() bool { return r.Observed && r.Remaining == 0 }

// Wait is how long to hold off, bounded by MaxGitHubRetryAfter.
//
// A forge that asks for an hour gets no sleep at all: the caller reports the
// rate limit and the person decides. Sleeping for an unbounded interval inside
// a request is how a settings screen becomes indistinguishable from a hang.
func (r RateLimit) Wait(now time.Time) (time.Duration, bool) {
	candidate := r.RetryAfter
	if candidate <= 0 && !r.ResetAt.IsZero() {
		candidate = r.ResetAt.Sub(now)
	}
	if candidate <= 0 || candidate > MaxGitHubRetryAfter {
		return 0, false
	}
	return candidate, true
}

// Describe renders the limit for a message a person reads.
func (r RateLimit) Describe() string {
	if !r.Observed {
		return "the forge sent no rate-limit information"
	}
	out := fmt.Sprintf("%d of %d requests left in this window", r.Remaining, r.Limit)
	if !r.ResetAt.IsZero() {
		out += "; it resets at " + r.ResetAt.UTC().Format(time.RFC3339)
	}
	return out
}

// readRateLimit pulls the forge's counters out of a response.
//
// Absent headers produce an unobserved zero rather than a fabricated one: a
// proxy that strips them must not make AO believe it has unlimited room, and
// must not make it believe it has none either.
func readRateLimit(resp *http.Response, now time.Time) RateLimit {
	out := RateLimit{Limit: -1, Remaining: -1}
	if resp == nil {
		return out
	}
	if raw := strings.TrimSpace(resp.Header.Get("X-RateLimit-Limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			out.Limit, out.Observed = n, true
		}
	}
	if raw := strings.TrimSpace(resp.Header.Get("X-RateLimit-Remaining")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			out.Remaining, out.Observed = n, true
		}
	}
	if raw := strings.TrimSpace(resp.Header.Get("X-RateLimit-Reset")); raw != "" {
		if secs, err := strconv.ParseInt(raw, 10, 64); err == nil && secs > 0 {
			out.ResetAt, out.Observed = time.Unix(secs, 0).UTC(), true
		}
	}
	if raw := strings.TrimSpace(resp.Header.Get("Retry-After")); raw != "" {
		if secs, err := strconv.Atoi(raw); err == nil && secs >= 0 {
			out.RetryAfter, out.Observed = time.Duration(secs)*time.Second, true
		} else if at, err := http.ParseTime(raw); err == nil {
			if d := at.Sub(now); d > 0 {
				out.RetryAfter, out.Observed = d, true
			}
		}
	}
	return out
}

// rateLimited reports whether this response is a refusal for rate reasons.
//
// GitHub answers 403 for BOTH "your credential is not allowed to see this" and
// "you have asked too many times", and telling them apart is the difference
// between sending an operator to rotate a token and sending them to wait ten
// minutes. The counter is what separates them: a rate-limited 403 carries
// remaining=0, and an authorization 403 does not.
func rateLimited(resp *http.Response, limit RateLimit) bool {
	if resp == nil {
		return false
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if resp.StatusCode != http.StatusForbidden {
		return false
	}
	if limit.Exhausted() {
		return true
	}
	// A secondary rate limit carries no counter and only says so in the body,
	// which AO does not parse. The header GitHub does send is Retry-After, and
	// a 403 asking to be retried is not a 403 about permissions.
	return limit.RetryAfter > 0
}

// ------------------------------------------------------------------ wire types

// githubRepo is the repository descriptor, reduced to what AO uses.
//
// Every field AO does not read is absent from this struct, and the decoder
// used for GitHub bodies deliberately does NOT disallow unknown fields -- see
// decodeGitHub. A forge's own API is not a contract AO gets to freeze.
type githubRepo struct {
	FullName string `json:"full_name"`
	Name     string `json:"name"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
	Private  bool `json:"private"`
	Archived bool `json:"archived"`
	Disabled bool `json:"disabled"`
}

// visibility renders the repository's visibility in the two words GitSource
// accepts.
func (r githubRepo) visibility() string {
	if r.Private {
		return "private"
	}
	return "public"
}

// githubRelease is one published release. Its tag is the only field AO treats
// as identifying, and even that is only a lookup key for the commit.
type githubRelease struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	CreatedAt   time.Time `json:"created_at"`
	PublishedAt time.Time `json:"published_at"`
}

// githubRef is a git reference. object.type distinguishes a lightweight tag,
// which points straight at a commit, from an annotated one, which points at a
// tag object that has to be peeled.
type githubRef struct {
	Ref    string `json:"ref"`
	Object struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
	} `json:"object"`
}

// githubTagObject is an annotated tag, one peel from its commit.
type githubTagObject struct {
	SHA    string `json:"sha"`
	Object struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
	} `json:"object"`
}

// ------------------------------------------------------------------ endpoints

// githubEndpoints builds every URL a GitHub provider will ever request.
type githubEndpoints struct {
	origin   Origin
	basePath string
}

func (e githubEndpoints) url(p string, query url.Values) string {
	u := url.URL{Scheme: e.origin.Scheme, Host: e.origin.Authority(), Path: e.basePath + p}
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}
	return u.String()
}

// seg escapes one path segment. Owner, repository and tag are all validated
// before they get here, so none of them can contain a slash today; escaping
// them anyway is what keeps that true when somebody widens one of the rules.
func seg(s string) string { return url.PathEscape(s) }

func (e githubEndpoints) repo(owner, repo string) string {
	return e.url("/repos/"+seg(owner)+"/"+seg(repo), nil)
}

func (e githubEndpoints) releases(owner, repo string, perPage int) string {
	return e.url("/repos/"+seg(owner)+"/"+seg(repo)+"/releases",
		url.Values{"per_page": []string{strconv.Itoa(perPage)}})
}

// tagRef asks for one tag by name. The tag is escaped SEGMENT BY SEGMENT
// because a tag may legally contain a slash ("audit/v1.2.3") and escaping the
// whole thing would turn the slash into %2F, which the forge answers 404 for.
func (e githubEndpoints) tagRef(owner, repo, tag string) string {
	parts := strings.Split(NormalizeTag(tag), "/")
	for i, p := range parts {
		parts[i] = seg(p)
	}
	return e.url("/repos/"+seg(owner)+"/"+seg(repo)+"/git/ref/tags/"+strings.Join(parts, "/"), nil)
}

func (e githubEndpoints) tagObject(owner, repo, sha string) string {
	return e.url("/repos/"+seg(owner)+"/"+seg(repo)+"/git/tags/"+seg(sha), nil)
}

// descriptor addresses the release descriptor AT ONE COMMIT. The ref is the
// commit SHA, never a tag and never a branch: this is the request that makes
// the whole scheme immutable, and a mutable ref here would undo it.
func (e githubEndpoints) descriptor(owner, repo, commit string) string {
	parts := strings.Split(GitHubDescriptorPath, "/")
	for i, p := range parts {
		parts[i] = seg(p)
	}
	return e.url("/repos/"+seg(owner)+"/"+seg(repo)+"/contents/"+strings.Join(parts, "/"),
		url.Values{"ref": []string{commit}})
}

// tarball addresses the archive of one commit.
func (e githubEndpoints) tarball(owner, repo, commit string) string {
	return e.url("/repos/"+seg(owner)+"/"+seg(repo)+"/tarball/"+seg(commit), nil)
}

// orgRepos and userRepos are the two ways an account's repositories are
// listed. AO tries the organization endpoint first and falls back, because a
// registry configuration says "owner" and the forge makes an account either an
// organization or a user with no way to tell from the name.
func (e githubEndpoints) orgRepos(owner string, perPage int) string {
	return e.url("/orgs/"+seg(owner)+"/repos",
		url.Values{"per_page": []string{strconv.Itoa(perPage)}, "sort": []string{"pushed"}})
}

func (e githubEndpoints) userRepos(owner string, perPage int) string {
	return e.url("/users/"+seg(owner)+"/repos",
		url.Values{"per_page": []string{strconv.Itoa(perPage)}, "sort": []string{"pushed"}})
}

// GitHubPublicAPI is the base URL of github.com's REST API.
//
// It is a constant rather than a default: a registry names its own location,
// so an installation pointed at GitHub Enterprise, at a mirror, or -- as every
// test in this repository is -- at a fixture, configures that instead. There
// is no code path that reaches github.com without somebody having typed it.
const GitHubPublicAPI = "https://api.github.com"
