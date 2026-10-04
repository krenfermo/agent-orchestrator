package controllers

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

// project_github.go -- P4-F's read surface.
//
// One route, project-scoped, behind the same Guard.AllowProject every other
// project route uses. Since P4-C that guard resolves through the caller's
// organizations, so a project in another tenant is not merely forbidden here --
// it does not exist for this caller, and a guessed id answers 404 exactly as
// /projects/{id} does. There is deliberately no second tenancy check in this
// file: a second one is a second thing that can be wrong, and repository
// metadata is exactly the kind of thing that must not leak across tenants.
//
// project.read is the permission, not memory.read: this is external context
// about the project's repository, not AO's derived knowledge of it.

// ProjectGitHubService is the controller-facing contract.
type ProjectGitHubService interface {
	ProjectGitHub(ctx context.Context, req ProjectGitHubQuery) (ProjectGitHubIntelligence, error)
}

// ProjectGitHubQuery is one snapshot ask.
type ProjectGitHubQuery struct {
	ProjectID domain.ProjectID
	// Branch narrows the pull-request lookup to one branch. Empty means
	// whatever branch the project checkout is currently on.
	Branch string
	// Issue focuses one issue, returning it with bounded comments and GitHub's
	// own linked pull requests instead of a list.
	Issue int
	// Refresh bypasses the short-lived snapshot cache.
	Refresh bool
}

// ProjectGitHubCommit is one commit from the local history.
type ProjectGitHubCommit struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject,omitempty"`
	Author  string `json:"author,omitempty"`
	Date    string `json:"date,omitempty"`
}

// ProjectGitHubRepository is the repository's state: the local checkout's
// facts beside the server's, never merged into one.
type ProjectGitHubRepository struct {
	Provider string `json:"provider,omitempty"`
	Host     string `json:"host,omitempty"`
	Repo     string `json:"repo,omitempty"`
	URL      string `json:"url,omitempty"`
	// OriginURL is the remote exactly as the checkout configures it, which may
	// be an SSH host alias.
	OriginURL string `json:"originUrl,omitempty"`
	// AliasResolved reports that the origin named an SSH alias AO resolved to
	// a GitHub host through the user's ssh configuration.
	AliasResolved bool   `json:"aliasResolved,omitempty"`
	DefaultBranch string `json:"defaultBranch,omitempty"`
	CurrentBranch string `json:"currentBranch,omitempty"`
	Detached      bool   `json:"detached,omitempty"`
	HeadSHA       string `json:"headSha,omitempty"`
	// RemoteHeadSHA is the tip of RemoteHeadRef on the server. The ref is
	// named so the SHA is never ambiguous about what it is the head of.
	RemoteHeadSHA string `json:"remoteHeadSha,omitempty"`
	RemoteHeadRef string `json:"remoteHeadRef,omitempty"`
	// UpstreamRef, Ahead and Behind are the LOCAL comparison against the
	// remote-tracking ref as of the last fetch — "what have I not pushed",
	// which is a different question from RemoteHeadSHA's.
	UpstreamRef   string                `json:"upstreamRef,omitempty"`
	Ahead         int                   `json:"ahead"`
	Behind        int                   `json:"behind"`
	Dirty         bool                  `json:"dirty,omitempty"`
	Private       bool                  `json:"private,omitempty"`
	Archived      bool                  `json:"archived,omitempty"`
	RecentCommits []ProjectGitHubCommit `json:"recentCommits,omitempty"`
}

// ProjectGitHubReview is one reviewer's most recent verdict, trimmed.
type ProjectGitHubReview struct {
	Author string `json:"author,omitempty"`
	State  string `json:"state,omitempty"`
	Body   string `json:"body,omitempty"`
	URL    string `json:"url,omitempty"`
}

// ProjectGitHubPullRequest is one pull request's current state.
type ProjectGitHubPullRequest struct {
	Number             int                   `json:"number"`
	Title              string                `json:"title"`
	URL                string                `json:"url"`
	Author             string                `json:"author,omitempty"`
	State              string                `json:"state,omitempty"`
	Draft              bool                  `json:"draft,omitempty"`
	BaseBranch         string                `json:"baseBranch,omitempty"`
	HeadBranch         string                `json:"headBranch,omitempty"`
	HeadRepo           string                `json:"headRepo,omitempty"`
	HeadSHA            string                `json:"headSha,omitempty"`
	ReviewDecision     string                `json:"reviewDecision,omitempty"`
	RequestedReviewers []string              `json:"requestedReviewers,omitempty"`
	ChecksSummary      string                `json:"checksSummary,omitempty"`
	ChecksPassed       int                   `json:"checksPassed"`
	ChecksFailed       int                   `json:"checksFailed"`
	ChecksPending      int                   `json:"checksPending"`
	FailingChecks      []string              `json:"failingChecks,omitempty"`
	Mergeable          string                `json:"mergeable,omitempty"`
	MergeBlockers      []string              `json:"mergeBlockers,omitempty"`
	Additions          int                   `json:"additions"`
	Deletions          int                   `json:"deletions"`
	ChangedFiles       int                   `json:"changedFiles"`
	UpdatedAt          string                `json:"updatedAt,omitempty"`
	LatestReviews      []ProjectGitHubReview `json:"latestReviews,omitempty"`
}

// ProjectGitHubLinkedPR is a pull request GitHub itself connects to an issue.
type ProjectGitHubLinkedPR struct {
	Number int    `json:"number"`
	Title  string `json:"title,omitempty"`
	URL    string `json:"url,omitempty"`
	State  string `json:"state,omitempty"`
}

// ProjectGitHubComment is one bounded issue comment.
type ProjectGitHubComment struct {
	Author string `json:"author,omitempty"`
	Body   string `json:"body,omitempty"`
	// Truncated reports that Body is a prefix, so a cut-off comment is never
	// shown as if it were whole.
	Truncated bool   `json:"truncated,omitempty"`
	CreatedAt string `json:"createdAt,omitempty"`
}

// ProjectGitHubIssue is one issue in the tracker's own vocabulary plus the
// milestone and linked pull requests a planner uses.
type ProjectGitHubIssue struct {
	Number    int                     `json:"number"`
	Title     string                  `json:"title"`
	State     string                  `json:"state,omitempty"`
	URL       string                  `json:"url,omitempty"`
	Labels    []string                `json:"labels,omitempty"`
	Assignees []string                `json:"assignees,omitempty"`
	Milestone string                  `json:"milestone,omitempty"`
	UpdatedAt string                  `json:"updatedAt,omitempty"`
	LinkedPRs []ProjectGitHubLinkedPR `json:"linkedPrs,omitempty"`
	Comments  []ProjectGitHubComment  `json:"comments,omitempty"`
	// CommentCount is the provider's total, so a bounded comment list can say
	// honestly that it is bounded.
	CommentCount int    `json:"commentCount,omitempty"`
	Body         string `json:"body,omitempty"`
}

// ProjectGitHubIntelligence is the body of GET /projects/{id}/github.
//
// Availability is the contract that matters. GitHub being unreachable is a
// normal state of the world, not an error AO should raise: the route answers
// 200 with availability "degraded" or "unavailable" and a stable reason code,
// and it never turns a GitHub outage into a failed request the caller has to
// interpret. Nothing here is authoritative over AO's own execution state.
type ProjectGitHubIntelligence struct {
	ProjectID string `json:"projectId"`
	// Availability says how much of this snapshot is real: ready (all of it),
	// degraded (some reads failed or only local state was available), or
	// unavailable (nothing could be produced).
	Availability string `json:"availability" enum:"ready,degraded,unavailable"`
	// Reason is a stable AO code (NO_CREDENTIALS, RATE_LIMITED, ...), never
	// provider text — provider messages can echo request content.
	Reason string `json:"reason,omitempty"`
	// Detail is AO's own one-sentence explanation, safe to display.
	Detail string `json:"detail,omitempty"`
	// Authenticated reports whether AO had usable GitHub credentials. False is
	// a supported mode: local repository state still works without a token.
	Authenticated bool                       `json:"authenticated"`
	Repository    ProjectGitHubRepository    `json:"repository"`
	PullRequests  []ProjectGitHubPullRequest `json:"pullRequests,omitempty"`
	// CurrentPR is the pull request for the checkout's own branch, repeated
	// here because "the PR for what I am working on" is what this surface is
	// asked most.
	CurrentPR  *ProjectGitHubPullRequest `json:"currentPr,omitempty"`
	Issues     []ProjectGitHubIssue      `json:"issues,omitempty"`
	ObservedAt string                    `json:"observedAt,omitempty"`
	// Stale reports that this is the last snapshot AO could fetch, served
	// through a GitHub outage rather than emptying the surface.
	Stale bool `json:"stale,omitempty"`
}

// ProjectGitHubController owns the /projects/{id}/github route.
type ProjectGitHubController struct {
	Svc   ProjectGitHubService
	Guard Guard
}

// Register mounts the GitHub intelligence route.
func (c *ProjectGitHubController) Register(r chi.Router) {
	r.Get("/projects/{id}/github", c.scoped(domain.PermProjectRead, c.overview))
}

func (c *ProjectGitHubController) scoped(perm domain.Permission, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !c.Guard.AllowProject(w, r, perm, projectID(r), "PROJECT_NOT_FOUND", "project not found") {
			return
		}
		h(w, r)
	}
}

func (c *ProjectGitHubController) overview(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/github")
		return
	}
	q := r.URL.Query()
	issue, _ := strconv.Atoi(strings.TrimSpace(q.Get("issue")))
	out, err := c.Svc.ProjectGitHub(r.Context(), ProjectGitHubQuery{
		ProjectID: projectID(r),
		Branch:    strings.TrimSpace(q.Get("branch")),
		Issue:     issue,
		Refresh:   strings.EqualFold(strings.TrimSpace(q.Get("refresh")), "true"),
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

// GetProjectGitHubQuery documents the query string for the generated spec. It
// is never decoded from; the handler reads r.URL.Query() directly, as every
// other query-parameter route in this package does.
type GetProjectGitHubQuery struct {
	Branch  string `query:"branch,omitempty" description:"Branch whose pull request to identify. Defaults to whatever branch the project checkout is on."`
	Issue   int    `query:"issue,omitempty" description:"Focus one issue, returned with bounded comments and GitHub's own linked pull requests instead of a list of recent issues."`
	Refresh bool   `query:"refresh,omitempty" description:"Bypass the short-lived snapshot cache and read GitHub again."`
}
