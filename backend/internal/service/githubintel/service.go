// Package githubintel turns AO's existing GitHub plumbing into project
// intelligence: what state a repository is in, which pull request a branch has,
// what its reviews and checks say, and which issues are worth a planner's
// attention.
//
// Three rules shape every line of it.
//
// **GitHub is never authoritative over AO.** AO's execution state comes from
// AO's own durable facts; what this package produces is EXTERNAL context, and
// a GitHub outage degrades the surface rather than the workflow. Nothing here
// can move a session into needs_attention, and nothing here is written into
// project memory as a durable fact -- see the note on Snapshot.
//
// **Everything is bounded.** Each read has a ceiling and a timeout, the whole
// snapshot has a deadline, and results are memoized for a short window. A
// dispatch that decorates itself with GitHub context must not be able to wait
// on GitHub.
//
// **Degraded is a first-class answer.** No token, no origin, an unrecognized
// host, a rate limit, an outage: each produces a Snapshot that says what is
// unavailable and why, with the last good data attached when there is some.
// There is no state in which this package raises an error that a caller has to
// invent a story for.
package githubintel

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	scmgithub "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/github"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Availability is what AO could actually find out this time.
type Availability string

// The three answers a GitHub intelligence read can give. "degraded" is not a
// failure: it means part of the picture is real and part could not be fetched,
// and it exists so a surface never has to choose between showing a stale number
// as if it were fresh and showing nothing at all.
const (
	// AvailabilityReady means every requested read succeeded.
	AvailabilityReady Availability = "ready"
	// AvailabilityDegraded means some reads succeeded and some did not.
	AvailabilityDegraded Availability = "degraded"
	// AvailabilityUnavailable means no GitHub data could be produced at all.
	AvailabilityUnavailable Availability = "unavailable"
)

// Reason codes. These are classifications AO assigns, never provider text:
// provider messages can echo request content, and a stable code is what a
// frontend can translate and an operator can search for.
const (
	ReasonNoProject     = "PROJECT_NOT_FOUND"
	ReasonNoOrigin      = "NO_ORIGIN"
	ReasonNotGitHub     = "ORIGIN_NOT_GITHUB"
	ReasonNoCredentials = "NO_CREDENTIALS"
	ReasonAuthFailed    = "AUTH_FAILED"
	ReasonRateLimited   = "RATE_LIMITED"
	ReasonNotFound      = "REPO_NOT_FOUND"
	ReasonUnreachable   = "GITHUB_UNREACHABLE"
	ReasonNoProvider    = "PROVIDER_NOT_CONFIGURED"
)

const (
	// snapshotTimeout bounds the whole remote half of one snapshot. It is
	// generous for a healthy API and short enough that a hung GitHub cannot
	// hold a UI request or a dispatch open.
	snapshotTimeout = 8 * time.Second
	// contextTimeout is the tighter budget used when a snapshot decorates an
	// agent dispatch. A dispatch waits for nobody.
	contextTimeout = 3 * time.Second
	// cacheTTL is how long a snapshot is served without re-reading GitHub. It
	// is short because PR and check state genuinely moves; it is non-zero
	// because a UI that polls and a dispatch that provisions must not each
	// cost a fresh round trip.
	cacheTTL = 30 * time.Second
	// staleGraceTTL is how long a snapshot that could NOT be refreshed keeps
	// being served, marked degraded. It is the cached-last-known-data rule:
	// during a GitHub outage the surface shows what it last knew, plainly
	// labelled as of when, rather than emptying itself.
	staleGraceTTL = 30 * time.Minute
	// defaultOpenPRs and defaultIssues bound the two list reads.
	defaultOpenPRs = 10
	defaultIssues  = 10
	// defaultCommits bounds the local history read.
	defaultCommits = 10
	// maxCacheEntries caps the memo. One entry per project.
	maxCacheEntries = 128
	// rateLimitCooldown is how long AO stops calling GitHub for a repository
	// after GitHub says it is rate-limited.
	//
	// Without it, a UI polling every minute and a dispatch provisioning
	// alongside it would keep spending requests on an API that is already
	// refusing them, which is the one behaviour guaranteed to make a rate limit
	// last longer. The cooldown is per repository rather than global: one busy
	// repository must not silence the others.
	rateLimitCooldown = 5 * time.Minute
)

// SCM is the provider-neutral slice this service needs. It is satisfied by the
// same *scmgithub.Provider the SCM observer already runs on, so PR discovery
// and PR facts come from one implementation rather than two.
type SCM interface {
	ParseRepository(remote string) (ports.SCMRepo, bool)
	ListPRsByRepo(ctx context.Context, repo ports.SCMRepo, updatedAfter time.Time) ([]ports.SCMPRObservation, error)
	FetchPullRequests(ctx context.Context, refs []ports.SCMPRRef) ([]ports.SCMObservation, error)
}

// GitHub is the GitHub-specific slice: the repository and issue reads that
// have no provider-neutral equivalent because no other provider is involved.
type GitHub interface {
	RepositoryOverview(ctx context.Context, repo ports.SCMRepo) (scmgithub.RepoOverview, error)
	ListIssues(ctx context.Context, repo ports.SCMRepo, limit int) ([]scmgithub.IssueSummary, error)
	FetchIssue(ctx context.Context, repo ports.SCMRepo, number int) (scmgithub.IssueDetail, error)
	SCMCredentialsAvailable(ctx context.Context) (bool, error)
}

// Projects is the project lookup. It is the STORE, not a tenancy-aware
// service: authorization happens once, at the HTTP boundary, through the same
// Guard every other project-scoped route uses. A second access decision here
// would be a second thing that can disagree with the first.
type Projects interface {
	GetProject(ctx context.Context, id string) (domain.ProjectRecord, bool, error)
}

// Repository is the repository half of a snapshot: the local checkout's state
// beside the server's.
type Repository struct {
	Provider string `json:"provider,omitempty"`
	Host     string `json:"host,omitempty"`
	// Repo is "owner/name".
	Repo string `json:"repo,omitempty"`
	URL  string `json:"url,omitempty"`
	// OriginURL is the remote as configured, which may be an SSH alias. It is
	// reported verbatim because "why did AO pick this repository" has to be
	// answerable from what the checkout actually says.
	OriginURL string `json:"originUrl,omitempty"`
	// AliasResolved is true when the configured origin used an SSH host alias
	// that AO resolved to a GitHub host.
	AliasResolved bool `json:"aliasResolved,omitempty"`

	DefaultBranch string `json:"defaultBranch,omitempty"`
	CurrentBranch string `json:"currentBranch,omitempty"`
	Detached      bool   `json:"detached,omitempty"`
	HeadSHA       string `json:"headSha,omitempty"`
	// RemoteHeadSHA is the tip of the CURRENT branch on the server when AO
	// could read it, otherwise the tip of the default branch. RemoteHeadRef
	// says which, so the number is never ambiguous.
	RemoteHeadSHA string `json:"remoteHeadSha,omitempty"`
	RemoteHeadRef string `json:"remoteHeadRef,omitempty"`
	// UpstreamRef, Ahead and Behind are the LOCAL comparison, computed against
	// the remote-tracking ref as of the last fetch. They answer "what have I
	// not pushed", which is a different question from RemoteHeadSHA's.
	UpstreamRef string `json:"upstreamRef,omitempty"`
	Ahead       int    `json:"ahead"`
	Behind      int    `json:"behind"`
	Dirty       bool   `json:"dirty,omitempty"`
	Private     bool   `json:"private,omitempty"`
	Archived    bool   `json:"archived,omitempty"`

	RecentCommits []Commit `json:"recentCommits,omitempty"`
}

// PullRequest is one PR as P4-F reports it.
type PullRequest struct {
	Number             int       `json:"number"`
	Title              string    `json:"title"`
	URL                string    `json:"url"`
	Author             string    `json:"author,omitempty"`
	State              string    `json:"state,omitempty"`
	Draft              bool      `json:"draft,omitempty"`
	BaseBranch         string    `json:"baseBranch,omitempty"`
	HeadBranch         string    `json:"headBranch,omitempty"`
	HeadRepo           string    `json:"headRepo,omitempty"`
	HeadSHA            string    `json:"headSha,omitempty"`
	ReviewDecision     string    `json:"reviewDecision,omitempty"`
	RequestedReviewers []string  `json:"requestedReviewers,omitempty"`
	ChecksSummary      string    `json:"checksSummary,omitempty"`
	ChecksPassed       int       `json:"checksPassed"`
	ChecksFailed       int       `json:"checksFailed"`
	ChecksPending      int       `json:"checksPending"`
	FailingChecks      []string  `json:"failingChecks,omitempty"`
	Mergeable          string    `json:"mergeable,omitempty"`
	MergeBlockers      []string  `json:"mergeBlockers,omitempty"`
	Additions          int       `json:"additions"`
	Deletions          int       `json:"deletions"`
	ChangedFiles       int       `json:"changedFiles"`
	UpdatedAt          time.Time `json:"updatedAt,omitempty"`
	// LatestReviews is a bounded summary of what reviewers actually said. It
	// is a summary on purpose: a PR's full review history is exactly the kind
	// of unbounded content this package refuses to hand anybody.
	LatestReviews []ReviewNote `json:"latestReviews,omitempty"`
}

// ReviewNote is one reviewer's most recent verdict, trimmed.
type ReviewNote struct {
	Author string `json:"author,omitempty"`
	State  string `json:"state,omitempty"`
	Body   string `json:"body,omitempty"`
	URL    string `json:"url,omitempty"`
}

// Issue is one issue as P4-F reports it. It reuses the tracker's vocabulary
// (title, state, labels, assignees) and adds the two things a planner uses
// that the tracker model has no room for.
type Issue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	State     string    `json:"state,omitempty"`
	URL       string    `json:"url,omitempty"`
	Labels    []string  `json:"labels,omitempty"`
	Assignees []string  `json:"assignees,omitempty"`
	Milestone string    `json:"milestone,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
	// LinkedPRs are the pull requests GITHUB connects to this issue, not ones
	// AO inferred from body text.
	LinkedPRs []LinkedPR `json:"linkedPrs,omitempty"`
	// Comments is a bounded tail of the conversation, and CommentCount is the
	// provider's total, so "there are 40 comments and you are seeing 5" is
	// visible rather than implied.
	Comments     []IssueComment `json:"comments,omitempty"`
	CommentCount int            `json:"commentCount,omitempty"`
	Body         string         `json:"body,omitempty"`
}

// LinkedPR is GitHub's own issue/PR connection.
type LinkedPR struct {
	Number int    `json:"number"`
	Title  string `json:"title,omitempty"`
	URL    string `json:"url,omitempty"`
	State  string `json:"state,omitempty"`
}

// IssueComment is one bounded comment.
type IssueComment struct {
	Author    string    `json:"author,omitempty"`
	Body      string    `json:"body,omitempty"`
	Truncated bool      `json:"truncated,omitempty"`
	CreatedAt time.Time `json:"createdAt,omitempty"`
}

// Snapshot is one bounded read of everything AO can currently say about a
// project's GitHub repository.
//
// It is deliberately NOT persisted. Transient PR and check state belongs in
// external intelligence, not in project memory: a memory row that says
// "check X is failing" is a fact with a half-life of minutes, and promoting it
// would poison a store whose whole value is that its contents stay true. The
// one genuinely durable thing here -- the default branch -- is available to
// the memory subsystem through DurableFacts below, which promotes nothing on
// its own.
type Snapshot struct {
	ProjectID string `json:"projectId"`
	// Availability and Reason are the degraded-state contract. Reason is a
	// stable code; Detail is a short AO-authored sentence, never provider text.
	Availability Availability `json:"availability" enum:"ready,degraded,unavailable"`
	Reason       string       `json:"reason,omitempty"`
	Detail       string       `json:"detail,omitempty"`
	// Authenticated reports whether AO had usable GitHub credentials. False is
	// a supported mode, not an error: the repository half still works from the
	// local checkout.
	Authenticated bool `json:"authenticated"`

	Repository   Repository    `json:"repository"`
	PullRequests []PullRequest `json:"pullRequests,omitempty"`
	// CurrentPR is the pull request for the checkout's own branch, when it has
	// one. It is repeated here rather than left to be found in the list
	// because "the PR for what I am working on" is the question this surface
	// is asked most.
	CurrentPR *PullRequest `json:"currentPr,omitempty"`
	Issues    []Issue      `json:"issues,omitempty"`

	ObservedAt time.Time `json:"observedAt"`
	// Stale reports that this snapshot was served from cache because a refresh
	// failed. With ObservedAt it is the whole of the "last known data" story.
	Stale bool `json:"stale,omitempty"`
}

// DurableFacts returns the parts of a snapshot that are genuinely durable
// project metadata rather than transient state -- today, the default branch
// and the canonical repository identity.
//
// It promotes nothing. It exists so that a future caller that wants to record
// durable GitHub metadata has one obvious, reviewed place to take it from, and
// so that the boundary between "durable" and "transient" is written down in
// code instead of being a convention somebody has to remember.
func (s Snapshot) DurableFacts() map[string]string {
	out := map[string]string{}
	if s.Repository.Repo != "" {
		out["repo"] = s.Repository.Repo
	}
	if s.Repository.DefaultBranch != "" {
		out["defaultBranch"] = s.Repository.DefaultBranch
	}
	if s.Repository.Host != "" {
		out["host"] = s.Repository.Host
	}
	return out
}

// Request is one snapshot ask.
type Request struct {
	ProjectID domain.ProjectID
	// WorkDir overrides the checkout to read local state from. A workflow task
	// runs in its own worktree, and the branch that matters is that worktree's,
	// not the project root's. Empty means the project root.
	WorkDir string
	// Branch overrides which branch's pull request to look for. Empty means
	// whatever the checkout is on.
	Branch string
	// IssueNumber focuses one issue, which is then returned with its bounded
	// comments and GitHub's own linked PRs. Zero means "list recent issues".
	IssueNumber int
	// WithIssues and WithPullRequests let a caller pay only for what it will
	// use. A context pack for a reviewer wants PRs and not the issue list.
	WithIssues       bool
	WithPullRequests bool
	// Timeout overrides the remote deadline. Zero means snapshotTimeout.
	Timeout time.Duration
	// SkipCache forces a fresh read. The UI's explicit refresh sets it.
	SkipCache bool
}

// Service assembles snapshots.
type Service struct {
	projects Projects
	scm      SCM
	github   GitHub
	git      Git
	logger   *slog.Logger
	now      func() time.Time

	mu sync.Mutex
	// cache holds the last snapshot whose remote half succeeded, per request
	// shape. cooldown holds, per repository, the time before which AO will not
	// call GitHub again after being rate-limited.
	cache    map[string]cacheEntry
	cooldown map[string]time.Time
}

type cacheEntry struct {
	snapshot Snapshot
	at       time.Time
}

// Option configures a Service.
type Option func(*Service)

// WithGitHub installs the GitHub-specific reads. Without it the service still
// reports the local repository half and degrades the rest.
func WithGitHub(gh GitHub) Option { return func(s *Service) { s.github = gh } }

// WithSCM installs the provider-neutral PR reads.
func WithSCM(scm SCM) Option { return func(s *Service) { s.scm = scm } }

// WithGit overrides the local git reader.
func WithGit(g Git) Option { return func(s *Service) { s.git = g } }

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithClock overrides the clock, for tests.
func WithClock(now func() time.Time) Option {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
	}
}

// New builds a Service. Every dependency except the project lookup is
// optional: a daemon with no GitHub token, or no SCM provider at all, still
// serves this surface in its degraded form rather than 501-ing.
func New(projects Projects, opts ...Option) *Service {
	s := &Service{
		projects: projects,
		git:      ExecGit{},
		logger:   slog.Default(),
		now:      func() time.Time { return time.Now().UTC() },
		cache:    map[string]cacheEntry{},
		cooldown: map[string]time.Time{},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Snapshot reads GitHub intelligence for one project. It never returns an
// error: every failure is a degraded snapshot with a reason, because a caller
// that has to decide what a GitHub error means is a caller that will decide
// differently from the next one.
func (s *Service) Snapshot(ctx context.Context, req Request) Snapshot {
	key := cacheKey(req)
	if !req.SkipCache {
		if cached, ok := s.cached(key, cacheTTL); ok {
			return cached
		}
	}

	out, remoteOK := s.build(ctx, req)

	// A read that could not reach GitHub falls back to the last one that
	// could, marked stale. This is the "cached last-known data" rule: during
	// an outage the surface keeps saying what it last knew, and says when.
	//
	// The choice is made on the REASON, not on the availability. A repository
	// whose remote half failed is still "degraded" rather than "unavailable"
	// when the local checkout answered -- branch, HEAD and dirtiness are
	// genuinely useful on their own -- so availability cannot be the signal
	// for "the remote read failed".
	if degradable(out.Reason) {
		if cached, ok := s.cached(key, staleGraceTTL); ok {
			cached.Stale = true
			cached.Availability = AvailabilityDegraded
			cached.Reason = out.Reason
			cached.Detail = out.Detail
			return cached
		}
	}
	// Only a snapshot whose remote half succeeded may become the last-known
	// data. Caching a failed read would let it overwrite the good answer it is
	// supposed to fall back to.
	if remoteOK {
		s.store(key, out)
	}
	return out
}

// degradable reports whether a reason describes a transient GitHub problem
// worth serving stale data through. A repository that is not GitHub at all, or
// a project that does not exist, is not going to be fixed by an old snapshot.
func degradable(reason string) bool {
	switch reason {
	case ReasonRateLimited, ReasonUnreachable, ReasonAuthFailed, ReasonNotFound:
		return true
	default:
		return false
	}
}

// build assembles one snapshot and reports whether the REMOTE half succeeded.
// The bool is what makes "this answer is worth caching as last-known data"
// separable from "this answer is useful", which are different questions.
func (s *Service) build(ctx context.Context, req Request) (Snapshot, bool) {
	now := s.now()
	out := Snapshot{ProjectID: string(req.ProjectID), ObservedAt: now, Availability: AvailabilityUnavailable}

	project, found, err := s.projects.GetProject(ctx, string(req.ProjectID))
	if err != nil || !found {
		out.Reason = ReasonNoProject
		out.Detail = "AO has no project with this id."
		return out, false
	}

	// 1. The local half. It needs no credentials and no network, so it is read
	//    first and survives every remote failure below.
	dir := strings.TrimSpace(req.WorkDir)
	if dir == "" {
		dir = project.Path
	}
	local := s.git.Read(ctx, dir, defaultCommits)
	out.Repository = Repository{
		CurrentBranch: local.Branch, Detached: local.Detached, HeadSHA: local.HeadSHA,
		UpstreamRef: local.UpstreamRef, Ahead: local.Ahead, Behind: local.Behind,
		Dirty: local.Dirty, RecentCommits: local.RecentCommits,
	}

	origin := strings.TrimSpace(local.OriginURL)
	if origin == "" {
		origin = strings.TrimSpace(project.RepoOriginURL)
	}
	out.Repository.OriginURL = origin
	if origin == "" {
		out.Reason = ReasonNoOrigin
		out.Detail = "This project's checkout has no origin remote."
		return s.finishLocalOnly(out), false
	}

	if s.scm == nil {
		out.Reason = ReasonNoProvider
		out.Detail = "No SCM provider is configured in this daemon."
		return s.finishLocalOnly(out), false
	}
	repo, ok := s.scm.ParseRepository(origin)
	if !ok || !strings.EqualFold(repo.Provider, "github") {
		out.Reason = ReasonNotGitHub
		out.Detail = "This project's origin is not a GitHub repository AO recognizes."
		return s.finishLocalOnly(out), false
	}
	out.Repository.Provider = repo.Provider
	out.Repository.Host = repo.Host
	out.Repository.Repo = repo.Repo
	out.Repository.URL = "https://" + repo.Host + "/" + repo.Repo
	// An origin whose literal host differs from the resolved one went through
	// SSH alias resolution. Saying so is what makes the alias fix visible
	// instead of merely working.
	out.Repository.AliasResolved = originHostDiffers(origin, repo.Host)

	if s.github == nil {
		out.Reason = ReasonNoProvider
		out.Detail = "This daemon has no GitHub reader configured."
		return s.finishLocalOnly(out), false
	}
	if ok, err := s.github.SCMCredentialsAvailable(ctx); err == nil && !ok {
		out.Reason = ReasonNoCredentials
		out.Detail = "No GitHub token is configured, so only local repository state is available."
		return s.finishLocalOnly(out), false
	}
	// Authenticated is NOT set here. Having a token source is not the same as
	// GitHub accepting what it produces -- an expired `gh` login satisfies the
	// probe above and is rejected on the first real call -- and a surface that
	// says "authenticated" beside "GitHub rejected AO's credentials" is a
	// surface saying two incompatible things. It is set below, once a call has
	// actually been accepted.

	// 2. The remote half, under one deadline for the whole thing.
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = snapshotTimeout
	}
	remoteCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var failures []string
	// A repository GitHub has already refused is not called again until the
	// cooldown expires. The answer is the same "rate limited" it would be
	// after another round trip, minus the round trip.
	if until, held := s.cooldownUntil(repo.Repo); held {
		out.Reason = ReasonRateLimited
		out.Detail = "GitHub is rate-limiting AO; it will try again after " +
			until.Format(time.Kitchen) + "."
		return s.finishLocalOnly(out), false
	}

	overview, err := s.github.RepositoryOverview(remoteCtx, repo)
	if err != nil {
		out.Reason, out.Detail = classify(err)
		if out.Reason == ReasonRateLimited {
			s.enterCooldown(repo.Repo, err)
		}
		// The local half is real and stays reported: an unreachable GitHub
		// must not make AO less useful than plain git.
		return s.finishLocalOnly(out), false
	}
	out.Authenticated = true
	out.Repository.DefaultBranch = overview.DefaultBranch
	out.Repository.Private = overview.Private
	out.Repository.Archived = overview.Archived
	if overview.HTMLURL != "" {
		out.Repository.URL = overview.HTMLURL
	}
	out.Repository.RemoteHeadRef = overview.DefaultBranch
	out.Repository.RemoteHeadSHA = overview.DefaultBranchSHA

	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		branch = local.Branch
	}
	// The tip of the branch the checkout is actually on is the more useful
	// remote head when it exists; the default branch's tip is the fallback.
	if branch != "" && !strings.EqualFold(branch, overview.DefaultBranch) {
		if sha, err := s.branchTip(remoteCtx, repo, branch); err == nil && sha != "" {
			out.Repository.RemoteHeadRef = branch
			out.Repository.RemoteHeadSHA = sha
		}
	}

	if req.WithPullRequests {
		prs, current, partial, err := s.pullRequests(remoteCtx, repo, branch)
		switch {
		case err != nil:
			failures = append(failures, "pull requests")
			s.logger.Debug("github intel: pull requests unavailable", "repo", repo.Repo, "err", err)
		default:
			out.PullRequests = prs
			out.CurrentPR = current
			// A PR list with no review decisions and no checks is not a
			// complete answer, and reporting it as one would let a surface
			// render "no failing checks" for a PR whose checks AO never read.
			if partial {
				failures = append(failures, "pull request review and check detail")
			}
		}
	}
	if req.WithIssues || req.IssueNumber > 0 {
		issues, err := s.issues(remoteCtx, repo, req.IssueNumber)
		if err != nil {
			failures = append(failures, "issues")
			s.logger.Debug("github intel: issues unavailable", "repo", repo.Repo, "err", err)
		} else {
			out.Issues = issues
		}
	}

	out.Availability = AvailabilityReady
	if len(failures) > 0 {
		out.Availability = AvailabilityDegraded
		out.Reason = ReasonUnreachable
		out.Detail = "GitHub did not answer for: " + strings.Join(failures, ", ") + "."
	}
	return out, true
}

// finishLocalOnly settles a snapshot that has real local state but no remote
// half. It is degraded rather than unavailable when the local read worked:
// branch, HEAD and dirtiness are genuinely useful on their own, and calling
// that "unavailable" would throw away information AO has in hand.
func (s *Service) finishLocalOnly(out Snapshot) Snapshot {
	if out.Repository.HeadSHA != "" || out.Repository.CurrentBranch != "" {
		out.Availability = AvailabilityDegraded
	}
	return out
}

func (s *Service) branchTip(ctx context.Context, repo ports.SCMRepo, branch string) (string, error) {
	reader, ok := s.github.(interface {
		BranchSHA(context.Context, ports.SCMRepo, string) (string, error)
	})
	if !ok {
		return "", nil
	}
	return reader.BranchSHA(ctx, repo, branch)
}

// pullRequests lists the repository's open PRs and, when the checkout is on a
// branch, identifies that branch's own PR. The list read is one page; the
// detail read is one batched GraphQL call over at most a handful of refs.
// pullRequests returns the repository's open PRs, the branch's own PR, and
// whether the answer is PARTIAL -- shallow facts because the detail read
// failed. The third value exists so a caller cannot mistake "AO did not read
// the checks" for "there are no failing checks".
func (s *Service) pullRequests(
	ctx context.Context, repo ports.SCMRepo, branch string,
) ([]PullRequest, *PullRequest, bool, error) {
	listed, err := s.scm.ListPRsByRepo(ctx, repo, time.Time{})
	if err != nil {
		return nil, nil, false, err
	}
	sort.SliceStable(listed, func(i, j int) bool {
		return listed[i].UpdatedAtProvider.After(listed[j].UpdatedAtProvider)
	})

	// The branch's own PR is always fetched in full, even when it has fallen
	// off the end of the recency window: it is the one the caller asked about.
	currentIdx := -1
	for i, pr := range listed {
		if branch != "" && strings.EqualFold(pr.SourceBranch, branch) {
			currentIdx = i
			break
		}
	}
	refs := make([]ports.SCMPRRef, 0, defaultOpenPRs+1)
	seen := map[int]bool{}
	addRef := func(pr ports.SCMPRObservation) {
		if pr.Number == 0 || seen[pr.Number] {
			return
		}
		seen[pr.Number] = true
		refs = append(refs, ports.SCMPRRef{Repo: repo, Number: pr.Number, URL: pr.URL})
	}
	if currentIdx >= 0 {
		addRef(listed[currentIdx])
	}
	for i := range listed {
		if len(refs) >= defaultOpenPRs {
			break
		}
		addRef(listed[i])
	}
	if len(refs) == 0 {
		return nil, nil, false, nil
	}

	observed, err := s.scm.FetchPullRequests(ctx, refs)
	if err != nil {
		// The detail read failed but the list did not. Report the shallow
		// facts rather than nothing: a PR number and title with no review
		// decision is still more than an empty panel.
		out := make([]PullRequest, 0, len(refs))
		for _, ref := range refs {
			for _, pr := range listed {
				if pr.Number == ref.Number {
					out = append(out, shallowPR(pr))
				}
			}
		}
		s.logger.Debug("github intel: PR detail unavailable, serving shallow facts",
			"repo", repo.Repo, "err", err)
		return out, currentOf(out, branch), true, nil
	}

	out := make([]PullRequest, 0, len(observed))
	for _, obs := range observed {
		if obs.Error != nil || !obs.Fetched {
			continue
		}
		out = append(out, fromObservation(obs))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	// Every ref AO asked about came back unusable: the batch read effectively
	// failed even though the call did not, and the list is shallow at best.
	return out, currentOf(out, branch), len(out) == 0 && len(refs) > 0, nil
}

func currentOf(prs []PullRequest, branch string) *PullRequest {
	if branch == "" {
		return nil
	}
	for i := range prs {
		if strings.EqualFold(prs[i].HeadBranch, branch) {
			pr := prs[i]
			return &pr
		}
	}
	return nil
}

func (s *Service) issues(ctx context.Context, repo ports.SCMRepo, focus int) ([]Issue, error) {
	if focus > 0 {
		detail, err := s.github.FetchIssue(ctx, repo, focus)
		if err != nil {
			return nil, err
		}
		return []Issue{fromIssueDetail(detail)}, nil
	}
	listed, err := s.github.ListIssues(ctx, repo, defaultIssues)
	if err != nil {
		return nil, err
	}
	out := make([]Issue, 0, len(listed))
	for _, issue := range listed {
		out = append(out, fromIssueSummary(issue))
	}
	return out, nil
}

// --- conversions -----------------------------------------------------------

func shallowPR(pr ports.SCMPRObservation) PullRequest {
	return PullRequest{
		Number: pr.Number, Title: pr.Title, URL: firstNonEmpty(pr.HTMLURL, pr.URL),
		Author: pr.Author, State: pr.State, Draft: pr.Draft,
		BaseBranch: pr.TargetBranch, HeadBranch: pr.SourceBranch, HeadRepo: pr.HeadRepo,
		HeadSHA: pr.HeadSHA, Additions: pr.Additions, Deletions: pr.Deletions,
		ChangedFiles: pr.ChangedFiles, UpdatedAt: pr.UpdatedAtProvider,
	}
}

// maxReviewNotes bounds the review summary on one PR, and maxReviewBody bounds
// one note. A review thread is a conversation; this is a signal.
const (
	maxReviewNotes    = 3
	maxReviewBody     = 400
	maxFailingChecks  = 8
	maxMergeBlockers  = 6
	maxRequestedNames = 8
)

func fromObservation(obs ports.SCMObservation) PullRequest {
	out := shallowPR(obs.PR)
	out.ReviewDecision = obs.Review.Decision
	out.ChecksSummary = obs.CI.Summary
	out.Mergeable = obs.Mergeability.State
	out.MergeBlockers = trimStrings(obs.Mergeability.Blockers, maxMergeBlockers)

	for _, check := range obs.CI.Checks {
		switch strings.ToLower(check.Status) {
		case "success", "passed", "passing":
			out.ChecksPassed++
		case "failure", "failed", "failing", "cancelled", "canceled", "timed_out", "action_required":
			out.ChecksFailed++
		default:
			out.ChecksPending++
		}
	}
	for _, check := range obs.CI.FailedChecks {
		if len(out.FailingChecks) >= maxFailingChecks {
			break
		}
		if name := strings.TrimSpace(check.Name); name != "" {
			out.FailingChecks = append(out.FailingChecks, name)
		}
	}
	// Requested reviewers are not a field on the neutral observation, so they
	// are derived from the reviews AO does have: a reviewer who has submitted
	// is named, and that is the honest subset rather than an invented one.
	for _, review := range obs.Review.Reviews {
		if len(out.LatestReviews) >= maxReviewNotes {
			break
		}
		if review.IsBot {
			continue
		}
		note := ReviewNote{Author: review.Author, State: review.State, URL: review.URL}
		body := strings.TrimSpace(review.Body)
		if len(body) > maxReviewBody {
			body = body[:maxReviewBody] + "..."
		}
		note.Body = body
		out.LatestReviews = append(out.LatestReviews, note)
		if review.Author != "" && len(out.RequestedReviewers) < maxRequestedNames &&
			!contains(out.RequestedReviewers, review.Author) {
			out.RequestedReviewers = append(out.RequestedReviewers, review.Author)
		}
	}
	return out
}

func fromIssueSummary(in scmgithub.IssueSummary) Issue {
	return Issue{
		Number: in.Number, Title: in.Title, State: in.State, URL: in.URL,
		Labels: in.Labels, Assignees: in.Assignees, Milestone: in.Milestone,
		UpdatedAt: in.UpdatedAt,
	}
}

func fromIssueDetail(in scmgithub.IssueDetail) Issue {
	out := fromIssueSummary(in.IssueSummary)
	out.Body = in.Body
	out.CommentCount = in.CommentCount
	for _, c := range in.Comments {
		out.Comments = append(out.Comments, IssueComment{
			Author: c.Author, Body: c.Body, Truncated: c.Truncated, CreatedAt: c.CreatedAt,
		})
	}
	for _, pr := range in.LinkedPRs {
		out.LinkedPRs = append(out.LinkedPRs, LinkedPR{
			Number: pr.Number, Title: pr.Title, URL: pr.URL, State: pr.State,
		})
	}
	return out
}

// classify maps a provider error onto a stable AO reason code. Provider
// message text is deliberately not propagated: it can echo request content,
// and AO's own sentence is the one a frontend can translate.
func classify(err error) (string, string) {
	switch {
	case errors.Is(err, scmgithub.ErrRateLimited):
		return ReasonRateLimited, "GitHub is rate-limiting AO; this will recover on its own."
	case errors.Is(err, scmgithub.ErrAuthFailed):
		return ReasonAuthFailed, "GitHub rejected AO's credentials for this repository."
	case errors.Is(err, ports.ErrSCMNotFound):
		return ReasonNotFound, "GitHub has no repository at this origin, or the token cannot see it."
	default:
		return ReasonUnreachable, "AO could not reach GitHub."
	}
}

// originHostDiffers reports whether the origin URL's literal host is not the
// host AO resolved it to, which is exactly the SSH-alias case.
func originHostDiffers(origin, resolved string) bool {
	raw := strings.TrimSpace(origin)
	var host string
	switch {
	case strings.HasPrefix(raw, "git@"):
		host, _, _ = strings.Cut(strings.TrimPrefix(raw, "git@"), ":")
	case strings.HasPrefix(raw, "ssh://"):
		rest := strings.TrimPrefix(raw, "ssh://")
		if _, after, ok := strings.Cut(rest, "@"); ok {
			rest = after
		}
		host, _, _ = strings.Cut(rest, "/")
		host, _, _ = strings.Cut(host, ":")
	default:
		return false
	}
	return host != "" && !strings.EqualFold(host, resolved)
}

// cooldownUntil reports whether a rate-limit cooldown is still in force for
// this repository, and when it lifts.
func (s *Service) cooldownUntil(repo string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.cooldown[repo]
	if !ok {
		return time.Time{}, false
	}
	if !s.now().Before(until) {
		delete(s.cooldown, repo)
		return time.Time{}, false
	}
	return until, true
}

// enterCooldown records a rate limit. GitHub's own reset hint wins when it has
// one and is sane; otherwise the bounded default applies. The hint is clamped
// because a misbehaving or misconfigured server must not be able to switch the
// surface off for hours.
func (s *Service) enterCooldown(repo string, err error) {
	now := s.now()
	until := now.Add(rateLimitCooldown)
	var rl *scmgithub.RateLimitError
	if errors.As(err, &rl) {
		switch {
		case rl.RetryAfter > 0:
			until = now.Add(rl.RetryAfter)
		case !rl.ResetAt.IsZero() && rl.ResetAt.After(now):
			until = rl.ResetAt
		}
	}
	if ceiling := now.Add(time.Hour); until.After(ceiling) {
		until = ceiling
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cooldown) >= maxCacheEntries {
		s.cooldown = map[string]time.Time{}
	}
	s.cooldown[repo] = until
}

func (s *Service) cached(key string, ttl time.Duration) (Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.cache[key]
	if !ok || s.now().Sub(entry.at) >= ttl {
		return Snapshot{}, false
	}
	return entry.snapshot, true
}

func (s *Service) store(key string, snap Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cache) >= maxCacheEntries {
		s.cache = map[string]cacheEntry{}
	}
	s.cache[key] = cacheEntry{snapshot: snap, at: s.now()}
}

func cacheKey(req Request) string {
	return strings.Join([]string{
		string(req.ProjectID), req.WorkDir, req.Branch,
		boolKey(req.WithIssues), boolKey(req.WithPullRequests), itoa(req.IssueNumber),
	}, "|")
}

func boolKey(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if strings.EqualFold(item, v) {
			return true
		}
	}
	return false
}

func trimStrings(in []string, limit int) []string {
	if len(in) <= limit {
		return in
	}
	return in[:limit]
}
