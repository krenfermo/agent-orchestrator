package githubintel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	scmgithub "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/github"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// --- fakes -----------------------------------------------------------------

type fakeProjects map[string]domain.ProjectRecord

func (f fakeProjects) GetProject(_ context.Context, id string) (domain.ProjectRecord, bool, error) {
	rec, ok := f[id]
	return rec, ok, nil
}

type fakeGit struct{ state LocalState }

func (f fakeGit) Read(context.Context, string, int) LocalState { return f.state }

type fakeSCM struct {
	repo    ports.SCMRepo
	parseOK bool
	listed  []ports.SCMPRObservation
	listErr error
	fetched []ports.SCMObservation
	fetch   error
	refs    []ports.SCMPRRef
}

func (f *fakeSCM) ParseRepository(string) (ports.SCMRepo, bool) { return f.repo, f.parseOK }

func (f *fakeSCM) ListPRsByRepo(context.Context, ports.SCMRepo, time.Time) ([]ports.SCMPRObservation, error) {
	return f.listed, f.listErr
}

func (f *fakeSCM) FetchPullRequests(_ context.Context, refs []ports.SCMPRRef) ([]ports.SCMObservation, error) {
	f.refs = refs
	return f.fetched, f.fetch
}

type fakeGitHub struct {
	overview    scmgithub.RepoOverview
	overviewErr error
	issues      []scmgithub.IssueSummary
	issuesErr   error
	detail      scmgithub.IssueDetail
	detailErr   error
	creds       bool
	credsErr    error
	overviews   int
	branchSHA   string
}

func (f *fakeGitHub) RepositoryOverview(context.Context, ports.SCMRepo) (scmgithub.RepoOverview, error) {
	f.overviews++
	return f.overview, f.overviewErr
}

func (f *fakeGitHub) ListIssues(context.Context, ports.SCMRepo, int) ([]scmgithub.IssueSummary, error) {
	return f.issues, f.issuesErr
}

func (f *fakeGitHub) FetchIssue(context.Context, ports.SCMRepo, int) (scmgithub.IssueDetail, error) {
	return f.detail, f.detailErr
}

func (f *fakeGitHub) SCMCredentialsAvailable(context.Context) (bool, error) {
	return f.creds, f.credsErr
}

func (f *fakeGitHub) BranchSHA(context.Context, ports.SCMRepo, string) (string, error) {
	if f.branchSHA == "" {
		return "", ports.ErrSCMNotFound
	}
	return f.branchSHA, nil
}

const projectID = "proj-1"

func ghRepo() ports.SCMRepo {
	return ports.SCMRepo{Provider: "github", Host: "github.com", Owner: "DarkaMX", Name: "MEDUSASASBACK", Repo: "DarkaMX/MEDUSASASBACK"}
}

func healthyLocal() LocalState {
	return LocalState{
		Available: true, Branch: "feat/thing", HeadSHA: "aaa111",
		UpstreamRef: "origin/feat/thing", Ahead: 2, Behind: 1, Dirty: true,
		OriginURL:     "git@github-nuevo:DarkaMX/MEDUSASASBACK.git",
		RecentCommits: []Commit{{SHA: "aaa111", Subject: "do the thing"}},
	}
}

func newService(t *testing.T, gh *fakeGitHub, scm *fakeSCM, local LocalState) *Service {
	t.Helper()
	return New(
		fakeProjects{projectID: {ID: projectID, Path: "/tmp/repo", RepoOriginURL: "git@github-nuevo:DarkaMX/MEDUSASASBACK.git"}},
		WithGit(fakeGit{state: local}),
		WithGitHub(gh),
		WithSCM(scm),
	)
}

// --- tests -----------------------------------------------------------------

func TestSnapshotReadyReportsBothHalves(t *testing.T) {
	gh := &fakeGitHub{
		creds: true,
		overview: scmgithub.RepoOverview{
			DefaultBranch: "main", DefaultBranchSHA: "main999",
			HTMLURL: "https://github.com/DarkaMX/MEDUSASASBACK", Private: true,
		},
		branchSHA: "remote222",
		issues:    []scmgithub.IssueSummary{{Number: 7, Title: "Broken login", State: "open", Milestone: "v2"}},
	}
	scm := &fakeSCM{repo: ghRepo(), parseOK: true,
		listed: []ports.SCMPRObservation{{Number: 12, SourceBranch: "feat/thing", URL: "u"}},
		fetched: []ports.SCMObservation{{Fetched: true,
			PR:           ports.SCMPRObservation{Number: 12, Title: "Do the thing", SourceBranch: "feat/thing", TargetBranch: "main", State: "open", HTMLURL: "https://github.com/x/y/pull/12"},
			Review:       ports.SCMReviewObservation{Decision: "changes_requested", Reviews: []ports.SCMReviewSummaryObservation{{Author: "carla", State: "changes_requested", Body: "Needs a test.\nAlso rename it."}}},
			CI:           ports.SCMCIObservation{Summary: "failing", Checks: []ports.SCMCheckObservation{{Name: "build", Status: "success"}, {Name: "test", Status: "failure"}}, FailedChecks: []ports.SCMCheckObservation{{Name: "test"}}},
			Mergeability: ports.SCMMergeabilityObservation{State: "blocked", Blockers: []string{"failing checks"}},
		}},
	}
	snap := newService(t, gh, scm, healthyLocal()).Snapshot(context.Background(), Request{
		ProjectID: projectID, WithIssues: true, WithPullRequests: true,
	})

	if snap.Availability != AvailabilityReady {
		t.Fatalf("availability = %q (%s); want ready", snap.Availability, snap.Detail)
	}
	repo := snap.Repository
	// Local half.
	if repo.CurrentBranch != "feat/thing" || repo.HeadSHA != "aaa111" || repo.Ahead != 2 || repo.Behind != 1 || !repo.Dirty {
		t.Fatalf("local repository state not reported: %+v", repo)
	}
	// Remote half, kept separate from the local one.
	if repo.DefaultBranch != "main" || repo.RemoteHeadRef != "feat/thing" || repo.RemoteHeadSHA != "remote222" {
		t.Fatalf("remote repository state not reported: %+v", repo)
	}
	if !repo.AliasResolved {
		t.Fatal("an SSH-alias origin should be reported as alias-resolved")
	}
	if snap.CurrentPR == nil || snap.CurrentPR.Number != 12 {
		t.Fatalf("current PR not identified: %+v", snap.CurrentPR)
	}
	pr := *snap.CurrentPR
	if pr.ReviewDecision != "changes_requested" || pr.ChecksFailed != 1 || pr.ChecksPassed != 1 {
		t.Fatalf("PR review/check state wrong: %+v", pr)
	}
	if len(pr.FailingChecks) != 1 || pr.FailingChecks[0] != "test" {
		t.Fatalf("failing checks = %v", pr.FailingChecks)
	}
	if len(snap.Issues) != 1 || snap.Issues[0].Milestone != "v2" {
		t.Fatalf("issues = %+v", snap.Issues)
	}
}

func TestSnapshotWithoutTokenIsDegradedNotFailed(t *testing.T) {
	gh := &fakeGitHub{creds: false}
	scm := &fakeSCM{repo: ghRepo(), parseOK: true}
	snap := newService(t, gh, scm, healthyLocal()).Snapshot(context.Background(), Request{
		ProjectID: projectID, WithPullRequests: true,
	})

	if snap.Availability != AvailabilityDegraded {
		t.Fatalf("availability = %q; want degraded", snap.Availability)
	}
	if snap.Reason != ReasonNoCredentials {
		t.Fatalf("reason = %q; want %q", snap.Reason, ReasonNoCredentials)
	}
	if snap.Authenticated {
		t.Fatal("reported authenticated with no credentials")
	}
	// The local half must survive: it needs no token, and throwing it away
	// would make an unauthenticated AO less useful than plain git.
	if snap.Repository.CurrentBranch != "feat/thing" || snap.Repository.Ahead != 2 {
		t.Fatalf("local state lost in the degraded path: %+v", snap.Repository)
	}
	if gh.overviews != 0 {
		t.Fatal("called GitHub without credentials")
	}
}

func TestSnapshotNonGitHubOriginIsNotGuessed(t *testing.T) {
	scm := &fakeSCM{parseOK: false}
	snap := newService(t, &fakeGitHub{creds: true}, scm, healthyLocal()).Snapshot(
		context.Background(), Request{ProjectID: projectID})
	if snap.Reason != ReasonNotGitHub {
		t.Fatalf("reason = %q; want %q", snap.Reason, ReasonNotGitHub)
	}
	if snap.Repository.Repo != "" {
		t.Fatalf("named a repository for an unrecognized origin: %q", snap.Repository.Repo)
	}
}

func TestSnapshotUnknownProjectIsUnavailable(t *testing.T) {
	snap := newService(t, &fakeGitHub{creds: true}, &fakeSCM{}, healthyLocal()).Snapshot(
		context.Background(), Request{ProjectID: "nope"})
	if snap.Availability != AvailabilityUnavailable || snap.Reason != ReasonNoProject {
		t.Fatalf("snapshot = %q/%q; want unavailable/%s", snap.Availability, snap.Reason, ReasonNoProject)
	}
}

func TestSnapshotRateLimitIsClassifiedAndNeverEchoesProviderText(t *testing.T) {
	gh := &fakeGitHub{creds: true, overviewErr: &scmgithub.RateLimitError{Message: "secret-looking provider text"}}
	snap := newService(t, gh, &fakeSCM{repo: ghRepo(), parseOK: true}, healthyLocal()).Snapshot(
		context.Background(), Request{ProjectID: projectID})

	if snap.Reason != ReasonRateLimited {
		t.Fatalf("reason = %q; want %q", snap.Reason, ReasonRateLimited)
	}
	if strings.Contains(snap.Detail, "secret-looking") {
		t.Fatalf("provider message leaked into the detail: %q", snap.Detail)
	}
}

// A GitHub outage serves the last snapshot AO could fetch, marked stale, and
// does not empty the surface.
func TestSnapshotServesLastKnownDataThroughAnOutage(t *testing.T) {
	now := time.Now().UTC()
	gh := &fakeGitHub{creds: true, overview: scmgithub.RepoOverview{DefaultBranch: "main"}}
	svc := New(
		fakeProjects{projectID: {ID: projectID, Path: "/tmp/repo"}},
		WithGit(fakeGit{state: healthyLocal()}),
		WithGitHub(gh),
		WithSCM(&fakeSCM{repo: ghRepo(), parseOK: true}),
		WithClock(func() time.Time { return now }),
	)
	first := svc.Snapshot(context.Background(), Request{ProjectID: projectID})
	if first.Availability != AvailabilityReady {
		t.Fatalf("first snapshot = %q (%s)", first.Availability, first.Detail)
	}

	// GitHub goes away, and enough time passes that the fresh-cache window has
	// closed but the stale grace window has not.
	gh.overviewErr = errors.New("dial tcp: connection refused")
	now = now.Add(cacheTTL + time.Second)
	out := svc.Snapshot(context.Background(), Request{ProjectID: projectID})

	if out.Availability != AvailabilityDegraded || !out.Stale {
		t.Fatalf("outage snapshot = %q stale=%v; want degraded/stale", out.Availability, out.Stale)
	}
	if out.Repository.DefaultBranch != "main" {
		t.Fatalf("last-known data not served: %+v", out.Repository)
	}
	if out.Reason != ReasonUnreachable {
		t.Fatalf("reason = %q; want %q", out.Reason, ReasonUnreachable)
	}

	// Past the grace window the stale answer stops being served.
	now = now.Add(staleGraceTTL)
	out = svc.Snapshot(context.Background(), Request{ProjectID: projectID})
	if out.Stale {
		t.Fatal("served an expired snapshot as last-known data")
	}
}

func TestSnapshotCacheAvoidsRepeatedReads(t *testing.T) {
	now := time.Now().UTC()
	gh := &fakeGitHub{creds: true, overview: scmgithub.RepoOverview{DefaultBranch: "main"}}
	svc := New(
		fakeProjects{projectID: {ID: projectID, Path: "/tmp/repo"}},
		WithGit(fakeGit{state: healthyLocal()}), WithGitHub(gh),
		WithSCM(&fakeSCM{repo: ghRepo(), parseOK: true}),
		WithClock(func() time.Time { return now }),
	)
	for i := 0; i < 4; i++ {
		svc.Snapshot(context.Background(), Request{ProjectID: projectID})
	}
	if gh.overviews != 1 {
		t.Fatalf("GitHub read %d times inside the cache window; want 1", gh.overviews)
	}
	// An explicit refresh has to actually refresh.
	svc.Snapshot(context.Background(), Request{ProjectID: projectID, SkipCache: true})
	if gh.overviews != 2 {
		t.Fatalf("refresh did not re-read GitHub (reads=%d)", gh.overviews)
	}
}

// A partial failure is degraded, not unavailable: the repository half is real
// and must still be reported.
func TestSnapshotPartialFailureIsDegraded(t *testing.T) {
	gh := &fakeGitHub{creds: true, overview: scmgithub.RepoOverview{DefaultBranch: "main"},
		issuesErr: errors.New("boom")}
	scm := &fakeSCM{repo: ghRepo(), parseOK: true}
	snap := newService(t, gh, scm, healthyLocal()).Snapshot(context.Background(), Request{
		ProjectID: projectID, WithIssues: true, WithPullRequests: true,
	})
	if snap.Availability != AvailabilityDegraded {
		t.Fatalf("availability = %q; want degraded", snap.Availability)
	}
	if snap.Repository.DefaultBranch != "main" {
		t.Fatalf("repository half lost: %+v", snap.Repository)
	}
}

// The PR detail read failing must not lose the PR list: a number and a title
// with no review decision is still more than an empty panel.
func TestPullRequestsFallBackToShallowFacts(t *testing.T) {
	gh := &fakeGitHub{creds: true, overview: scmgithub.RepoOverview{DefaultBranch: "main"}}
	scm := &fakeSCM{repo: ghRepo(), parseOK: true,
		listed: []ports.SCMPRObservation{{Number: 12, Title: "Do it", SourceBranch: "feat/thing"}},
		fetch:  errors.New("graphql exploded"),
	}
	snap := newService(t, gh, scm, healthyLocal()).Snapshot(context.Background(), Request{
		ProjectID: projectID, WithPullRequests: true,
	})
	if len(snap.PullRequests) != 1 || snap.PullRequests[0].Number != 12 {
		t.Fatalf("shallow PR facts lost: %+v", snap.PullRequests)
	}
	if snap.CurrentPR == nil {
		t.Fatal("current PR not identified from shallow facts")
	}
	// ...but the answer must not be dressed up as complete. A surface that
	// read "ready" here would render "no failing checks" for a pull request
	// whose checks AO never actually read.
	if snap.Availability != AvailabilityDegraded {
		t.Fatalf("availability = %q; shallow PR facts are a partial answer", snap.Availability)
	}
	if !strings.Contains(snap.Detail, "review and check detail") {
		t.Fatalf("detail does not name what is missing: %q", snap.Detail)
	}
}

// The branch's own PR is always fetched, even when the recency window is full
// of other people's work.
func TestPullRequestsAlwaysIncludeTheBranchesOwn(t *testing.T) {
	var listed []ports.SCMPRObservation
	for i := 1; i <= 30; i++ {
		listed = append(listed, ports.SCMPRObservation{
			Number: i, SourceBranch: "other/" + itoa(i),
			UpdatedAtProvider: time.Now().Add(time.Duration(-i) * time.Minute),
		})
	}
	listed = append(listed, ports.SCMPRObservation{
		Number: 99, SourceBranch: "feat/thing",
		UpdatedAtProvider: time.Now().Add(-100 * time.Hour),
	})
	scm := &fakeSCM{repo: ghRepo(), parseOK: true, listed: listed}
	gh := &fakeGitHub{creds: true, overview: scmgithub.RepoOverview{DefaultBranch: "main"}}
	newService(t, gh, scm, healthyLocal()).Snapshot(context.Background(), Request{
		ProjectID: projectID, WithPullRequests: true,
	})

	if len(scm.refs) > defaultOpenPRs+1 {
		t.Fatalf("fetched %d PR refs; the batch must stay bounded", len(scm.refs))
	}
	found := false
	for _, ref := range scm.refs {
		if ref.Number == 99 {
			found = true
		}
	}
	if !found {
		t.Fatalf("the branch's own (stale) PR was dropped from the batch: %+v", scm.refs)
	}
}

func TestOriginHostDiffers(t *testing.T) {
	for _, tc := range []struct {
		origin, resolved string
		want             bool
	}{
		{"git@github-nuevo:o/r.git", "github.com", true},
		{"git@github.com:o/r.git", "github.com", false},
		{"ssh://git@github-nuevo:22/o/r.git", "github.com", true},
		{"https://github.com/o/r.git", "github.com", false},
		{"", "github.com", false},
	} {
		if got := originHostDiffers(tc.origin, tc.resolved); got != tc.want {
			t.Fatalf("originHostDiffers(%q,%q) = %v; want %v", tc.origin, tc.resolved, got, tc.want)
		}
	}
}

func TestDurableFactsCarryOnlyDurableThings(t *testing.T) {
	snap := Snapshot{Repository: Repository{
		Repo: "o/r", Host: "github.com", DefaultBranch: "main",
		HeadSHA: "abc", Ahead: 3,
	}, PullRequests: []PullRequest{{Number: 1, ChecksSummary: "failing"}}}

	facts := snap.DurableFacts()
	if facts["defaultBranch"] != "main" || facts["repo"] != "o/r" {
		t.Fatalf("durable facts missing repository identity: %v", facts)
	}
	// Transient state must never be promotable. A memory row saying "check X
	// is failing" has a half-life of minutes.
	for key := range facts {
		switch key {
		case "repo", "defaultBranch", "host":
		default:
			t.Fatalf("transient fact %q offered as durable", key)
		}
	}
}

// Having a token source is not the same as GitHub accepting what it produces.
// An expired `gh` login satisfies the credentials probe and is rejected on the
// first real call, and a snapshot that said "authenticated" beside "GitHub
// rejected AO's credentials" would be saying two incompatible things.
func TestSnapshotClaimsAuthenticatedOnlyAfterGitHubAcceptsACall(t *testing.T) {
	gh := &fakeGitHub{creds: true, overviewErr: errors.New("github scm: authentication failed: Bad credentials")}
	snap := newService(t, gh, &fakeSCM{repo: ghRepo(), parseOK: true}, healthyLocal()).Snapshot(
		context.Background(), Request{ProjectID: projectID})
	if snap.Authenticated {
		t.Fatal("claimed authenticated after GitHub rejected the call")
	}

	gh.overviewErr = nil
	gh.overview = scmgithub.RepoOverview{DefaultBranch: "main"}
	snap = newService(t, gh, &fakeSCM{repo: ghRepo(), parseOK: true}, healthyLocal()).Snapshot(
		context.Background(), Request{ProjectID: projectID})
	if !snap.Authenticated {
		t.Fatal("did not claim authenticated after a call GitHub accepted")
	}
}

// An unreachable GitHub must not make AO less useful than plain git.
func TestSnapshotKeepsTheLocalHalfWhenTheRemoteReadFails(t *testing.T) {
	gh := &fakeGitHub{creds: true, overviewErr: errors.New("dial tcp: connection refused")}
	snap := newService(t, gh, &fakeSCM{repo: ghRepo(), parseOK: true}, healthyLocal()).Snapshot(
		context.Background(), Request{ProjectID: projectID})

	if snap.Availability != AvailabilityDegraded {
		t.Fatalf("availability = %q; want degraded — the local half answered", snap.Availability)
	}
	if snap.Repository.CurrentBranch != "feat/thing" || snap.Repository.HeadSHA != "aaa111" ||
		snap.Repository.Ahead != 2 || len(snap.Repository.RecentCommits) == 0 {
		t.Fatalf("local state lost on a remote failure: %+v", snap.Repository)
	}
	// The repository was still identified: the origin parsed, which needs no
	// network at all.
	if snap.Repository.Repo != "DarkaMX/MEDUSASASBACK" {
		t.Fatalf("repository identity lost: %+v", snap.Repository)
	}
}

// A failed read must never become the last-known data it is supposed to fall
// back to.
func TestSnapshotDoesNotCacheAFailedRemoteRead(t *testing.T) {
	now := time.Now().UTC()
	gh := &fakeGitHub{creds: true, overview: scmgithub.RepoOverview{DefaultBranch: "main"}}
	svc := New(
		fakeProjects{projectID: {ID: projectID, Path: "/tmp/repo"}},
		WithGit(fakeGit{state: healthyLocal()}), WithGitHub(gh),
		WithSCM(&fakeSCM{repo: ghRepo(), parseOK: true}),
		WithClock(func() time.Time { return now }),
	)
	if got := svc.Snapshot(context.Background(), Request{ProjectID: projectID}); got.Repository.DefaultBranch != "main" {
		t.Fatalf("first read failed: %+v", got)
	}

	// Two failed reads in a row: the second must still fall back to the ONE
	// good snapshot, not to the first failure.
	gh.overviewErr = errors.New("dial tcp: connection refused")
	now = now.Add(cacheTTL + time.Second)
	_ = svc.Snapshot(context.Background(), Request{ProjectID: projectID})
	now = now.Add(cacheTTL + time.Second)
	out := svc.Snapshot(context.Background(), Request{ProjectID: projectID})

	if !out.Stale || out.Repository.DefaultBranch != "main" {
		t.Fatalf("the good snapshot was overwritten by a failed read: %+v", out)
	}
}

// Continuing to call an API that is already refusing requests is the one
// behaviour guaranteed to make a rate limit last longer.
func TestSnapshotStopsCallingGitHubWhileRateLimited(t *testing.T) {
	now := time.Now().UTC()
	gh := &fakeGitHub{creds: true, overviewErr: &scmgithub.RateLimitError{
		Message: "API rate limit exceeded", RetryAfter: 2 * time.Minute,
	}}
	svc := New(
		fakeProjects{projectID: {ID: projectID, Path: "/tmp/repo"}},
		WithGit(fakeGit{state: healthyLocal()}), WithGitHub(gh),
		WithSCM(&fakeSCM{repo: ghRepo(), parseOK: true}),
		WithClock(func() time.Time { return now }),
	)
	first := svc.Snapshot(context.Background(), Request{ProjectID: projectID})
	if first.Reason != ReasonRateLimited {
		t.Fatalf("reason = %q; want %q", first.Reason, ReasonRateLimited)
	}
	if gh.overviews != 1 {
		t.Fatalf("GitHub called %d times on the first read; want 1", gh.overviews)
	}

	// Past the snapshot cache but inside the cooldown: still rate-limited, and
	// still without spending a request to find that out.
	now = now.Add(cacheTTL + time.Second)
	out := svc.Snapshot(context.Background(), Request{ProjectID: projectID})
	if gh.overviews != 1 {
		t.Fatalf("GitHub called %d times while rate-limited; want 1", gh.overviews)
	}
	if out.Reason != ReasonRateLimited {
		t.Fatalf("reason = %q inside the cooldown; want %q", out.Reason, ReasonRateLimited)
	}
	// The local half stays useful throughout.
	if out.Repository.CurrentBranch != "feat/thing" {
		t.Fatalf("local state lost during a rate limit: %+v", out.Repository)
	}

	// Past GitHub's own Retry-After, AO tries again.
	now = now.Add(2 * time.Minute)
	gh.overviewErr = nil
	gh.overview = scmgithub.RepoOverview{DefaultBranch: "main"}
	out = svc.Snapshot(context.Background(), Request{ProjectID: projectID})
	if gh.overviews != 2 {
		t.Fatalf("GitHub called %d times after the cooldown lifted; want 2", gh.overviews)
	}
	if out.Availability != AvailabilityReady {
		t.Fatalf("availability = %q after recovery; want ready", out.Availability)
	}
}

// A server that suggests an absurd wait must not be able to switch the surface
// off for the rest of the day.
func TestRateLimitCooldownIsClamped(t *testing.T) {
	now := time.Now().UTC()
	svc := New(fakeProjects{}, WithClock(func() time.Time { return now }))
	svc.enterCooldown("o/r", &scmgithub.RateLimitError{RetryAfter: 72 * time.Hour})
	until, held := svc.cooldownUntil("o/r")
	if !held {
		t.Fatal("no cooldown recorded")
	}
	if until.After(now.Add(time.Hour)) {
		t.Fatalf("cooldown until %v; more than an hour after %v", until, now)
	}
}
