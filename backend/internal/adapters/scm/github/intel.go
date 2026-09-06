package github

// intel.go -- the bounded repository and issue reads P4-F needs.
//
// Everything here is a fixed-cost query. There is no pagination loop, no
// "fetch until done", and no HTML anywhere: each call is one or two REST/GraphQL
// requests with an explicit ceiling, because this data is read to DECORATE a
// planning or review dispatch and a decoration that can cost an unbounded
// number of round trips is a decoration that will one day stall a dispatch.
//
// The PR side is not here. PR discovery and PR facts already exist on this
// provider as ListPRsByRepo and FetchPullRequests, built for the SCM observer
// and normalized into ports.SCMObservation; P4-F reuses them rather than
// growing a second, subtly different set of PR reads.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const (
	// intelIssueListLimit is the hard ceiling on issues one repository read
	// returns, regardless of what the caller asks for.
	intelIssueListLimit = 25
	// intelIssueCommentLimit bounds the comments fetched for ONE focused
	// issue. The newest few carry the current state of the conversation; the
	// rest is history a dispatch does not need and cannot afford.
	intelIssueCommentLimit = 5
	// intelLinkedPRLimit bounds the linked-PR scan on a focused issue.
	intelLinkedPRLimit = 10
	// intelCommentBodyLimit truncates one comment body. A single 40KB comment
	// must not be able to dominate a context pack.
	intelCommentBodyLimit = 600
)

// RepoOverview is the repository's own metadata, as GitHub has it.
type RepoOverview struct {
	// DefaultBranch is the branch GitHub opens PRs against by default.
	DefaultBranch string
	// DefaultBranchSHA is the tip of that branch on the server right now. It
	// is what makes "is my checkout behind?" answerable without a fetch.
	DefaultBranchSHA string
	// HTMLURL is the browser URL for the repository.
	HTMLURL string
	// Private reports the repository's visibility.
	Private bool
	// Archived reports a repository nobody can push to any more, which is
	// worth saying out loud before an agent plans work in it.
	Archived bool
	// PushedAt is the last push GitHub saw, in any branch.
	PushedAt time.Time
	// OpenIssues is GitHub's own count. It includes open pull requests, which
	// is GitHub's definition and not a bug here -- it is reported as the
	// provider's number, never recomputed.
	OpenIssues int
}

// RepositoryOverview reads one repository's metadata plus the tip of its
// default branch. Two REST calls, both cheap and both ETag-cached by the
// client, so a repeat read on an unchanged repository costs two 304s.
func (p *Provider) RepositoryOverview(ctx context.Context, repo ports.SCMRepo) (RepoOverview, error) {
	resp, err := p.client.doREST(ctx, "GET", repoPath(repo.Owner, repo.Name), nil, nil)
	if err != nil {
		return RepoOverview{}, err
	}
	var body struct {
		DefaultBranch string `json:"default_branch"`
		HTMLURL       string `json:"html_url"`
		Private       bool   `json:"private"`
		Archived      bool   `json:"archived"`
		PushedAt      string `json:"pushed_at"`
		OpenIssues    int    `json:"open_issues_count"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		return RepoOverview{}, fmt.Errorf("github scm: decode repository: %w", err)
	}
	out := RepoOverview{
		DefaultBranch: strings.TrimSpace(body.DefaultBranch),
		HTMLURL:       strings.TrimSpace(body.HTMLURL),
		Private:       body.Private,
		Archived:      body.Archived,
		PushedAt:      parseGitHubTime(body.PushedAt),
		OpenIssues:    body.OpenIssues,
	}
	if out.DefaultBranch == "" {
		return out, nil
	}
	// The branch tip is a separate, equally cheap call. A failure here is not
	// fatal: knowing the default branch without its SHA is still most of the
	// answer, and refusing the whole overview would be the worse trade.
	sha, err := p.branchSHA(ctx, repo, out.DefaultBranch)
	if err != nil {
		p.logger.Debug("github intel: default branch tip unavailable",
			"repo", repo.Repo, "branch", out.DefaultBranch, "err", err)
		return out, nil
	}
	out.DefaultBranchSHA = sha
	return out, nil
}

// BranchSHA returns the tip commit of one branch on the server, or
// ports.ErrSCMNotFound when the branch does not exist there.
func (p *Provider) BranchSHA(ctx context.Context, repo ports.SCMRepo, branch string) (string, error) {
	return p.branchSHA(ctx, repo, branch)
}

func (p *Provider) branchSHA(ctx context.Context, repo ports.SCMRepo, branch string) (string, error) {
	if strings.TrimSpace(branch) == "" {
		return "", ports.ErrSCMNotFound
	}
	resp, err := p.client.doREST(ctx, "GET",
		repoPath(repo.Owner, repo.Name, "branches", branch), nil, nil)
	if err != nil {
		return "", err
	}
	var body struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		return "", fmt.Errorf("github scm: decode branch: %w", err)
	}
	return strings.TrimSpace(body.Commit.SHA), nil
}

// IssueSummary is one issue as a planning surface needs it. It carries the
// same vocabulary as the tracker adapter's domain.Issue plus the two fields
// that model has no room for and a planner genuinely uses.
type IssueSummary struct {
	Number    int
	Title     string
	State     string
	URL       string
	Labels    []string
	Assignees []string
	Milestone string
	UpdatedAt time.Time
}

// ListIssues returns up to limit open issues, newest activity first. Pull
// requests are excluded: GitHub serves them from the issues endpoint, and a
// list that mixed them would double-count every open PR.
func (p *Provider) ListIssues(ctx context.Context, repo ports.SCMRepo, limit int) ([]IssueSummary, error) {
	if limit <= 0 || limit > intelIssueListLimit {
		limit = intelIssueListLimit
	}
	q := url.Values{}
	q.Set("state", "open")
	q.Set("sort", "updated")
	q.Set("direction", "desc")
	// One page, over-fetched a little so filtering PRs out does not leave the
	// caller short. Deliberately never paginated: this is a summary surface.
	q.Set("per_page", strconv.Itoa(min(limit*2, 100)))
	resp, err := p.client.doREST(ctx, "GET", repoPath(repo.Owner, repo.Name, "issues"), q, nil)
	if err != nil {
		return nil, err
	}
	var body []struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		State       string `json:"state"`
		HTMLURL     string `json:"html_url"`
		UpdatedAt   string `json:"updated_at"`
		PullRequest *struct {
			URL string `json:"url"`
		} `json:"pull_request"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		Assignees []struct {
			Login string `json:"login"`
		} `json:"assignees"`
		Milestone *struct {
			Title string `json:"title"`
		} `json:"milestone"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		return nil, fmt.Errorf("github scm: decode issues: %w", err)
	}
	out := make([]IssueSummary, 0, limit)
	for _, row := range body {
		if row.PullRequest != nil {
			continue
		}
		if len(out) >= limit {
			break
		}
		issue := IssueSummary{
			Number: row.Number, Title: strings.TrimSpace(row.Title),
			State: strings.ToLower(strings.TrimSpace(row.State)),
			URL:   strings.TrimSpace(row.HTMLURL), UpdatedAt: parseGitHubTime(row.UpdatedAt),
		}
		for _, l := range row.Labels {
			if name := strings.TrimSpace(l.Name); name != "" {
				issue.Labels = append(issue.Labels, name)
			}
		}
		for _, a := range row.Assignees {
			if login := strings.TrimSpace(a.Login); login != "" {
				issue.Assignees = append(issue.Assignees, login)
			}
		}
		if row.Milestone != nil {
			issue.Milestone = strings.TrimSpace(row.Milestone.Title)
		}
		out = append(out, issue)
	}
	return out, nil
}

// IssueComment is one bounded comment on a focused issue.
type IssueComment struct {
	Author string
	Body   string
	// Truncated reports that Body is a prefix, so a reader is never shown a
	// cut-off comment as if it were the whole one.
	Truncated bool
	CreatedAt time.Time
}

// LinkedPR is a pull request GitHub itself has connected to an issue.
type LinkedPR struct {
	Number int
	Title  string
	URL    string
	State  string
}

// IssueDetail is one issue with the context a dispatch actually reasons about.
type IssueDetail struct {
	IssueSummary
	Body      string
	BodyBytes int
	Comments  []IssueComment
	LinkedPRs []LinkedPR
	// CommentCount is the provider's total, which is how a bounded comment
	// list can say honestly that it is bounded.
	CommentCount int
}

// issueDetailQuery reads one issue and everything P4-F wants about it in a
// single GraphQL round trip, with every connection explicitly bounded.
//
// The timeline is where "linked PRs" actually lives: a branch that says
// "fixes #12" produces a CONNECTED_EVENT, and a PR that merely mentions the
// issue produces a CROSS_REFERENCED_EVENT. Both are GitHub's own links, which
// is the difference between reading a relationship and inferring one from
// text.
const issueDetailQuery = `query($owner:String!,$name:String!,$number:Int!,$comments:Int!,$links:Int!){
  repository(owner:$owner,name:$name){
    issue(number:$number){
      number title url state bodyText updatedAt
      milestone{ title }
      labels(first:10){ nodes{ name } }
      assignees(first:10){ nodes{ login } }
      comments(last:$comments){
        totalCount
        nodes{ bodyText createdAt author{ login } }
      }
      timelineItems(last:$links, itemTypes:[CONNECTED_EVENT,CROSS_REFERENCED_EVENT]){
        nodes{
          ... on ConnectedEvent{ subject{ ... on PullRequest{ number title url state } } }
          ... on CrossReferencedEvent{ source{ ... on PullRequest{ number title url state } } }
        }
      }
    }
  }
}`

// FetchIssue reads one issue with bounded comments and GitHub's own linked
// pull requests.
func (p *Provider) FetchIssue(ctx context.Context, repo ports.SCMRepo, number int) (IssueDetail, error) {
	if number <= 0 {
		return IssueDetail{}, ports.ErrSCMNotFound
	}
	data, err := p.client.doGraphQL(ctx, issueDetailQuery, map[string]any{
		"owner": repo.Owner, "name": repo.Name, "number": number,
		"comments": intelIssueCommentLimit, "links": intelLinkedPRLimit,
	})
	if err != nil {
		return IssueDetail{}, err
	}
	issue, ok := mapAt(data, "repository", "issue")
	if !ok {
		return IssueDetail{}, ports.ErrSCMNotFound
	}
	out := IssueDetail{IssueSummary: IssueSummary{
		Number:    intOf(issue["number"]),
		Title:     strings.TrimSpace(strOf(issue["title"])),
		URL:       strings.TrimSpace(strOf(issue["url"])),
		State:     strings.ToLower(strings.TrimSpace(strOf(issue["state"]))),
		UpdatedAt: parseGitHubTime(strOf(issue["updatedAt"])),
	}}
	if ms, ok := issue["milestone"].(map[string]any); ok {
		out.Milestone = strings.TrimSpace(strOf(ms["title"]))
	}
	for _, node := range nodesOf(issue, "labels") {
		if name := strings.TrimSpace(strOf(node["name"])); name != "" {
			out.Labels = append(out.Labels, name)
		}
	}
	for _, node := range nodesOf(issue, "assignees") {
		if login := strings.TrimSpace(strOf(node["login"])); login != "" {
			out.Assignees = append(out.Assignees, login)
		}
	}
	body := strings.TrimSpace(strOf(issue["bodyText"]))
	out.BodyBytes = len(body)
	out.Body = body

	if comments, ok := issue["comments"].(map[string]any); ok {
		out.CommentCount = intOf(comments["totalCount"])
	}
	for _, node := range nodesOf(issue, "comments") {
		text := strings.TrimSpace(strOf(node["bodyText"]))
		if text == "" {
			continue
		}
		comment := IssueComment{CreatedAt: parseGitHubTime(strOf(node["createdAt"]))}
		if author, ok := node["author"].(map[string]any); ok {
			comment.Author = strings.TrimSpace(strOf(author["login"]))
		}
		if len(text) > intelCommentBodyLimit {
			text, comment.Truncated = text[:intelCommentBodyLimit], true
		}
		comment.Body = text
		out.Comments = append(out.Comments, comment)
	}

	seen := map[int]bool{}
	for _, node := range nodesOf(issue, "timelineItems") {
		for _, key := range []string{"subject", "source"} {
			pr, ok := node[key].(map[string]any)
			if !ok {
				continue
			}
			number := intOf(pr["number"])
			if number == 0 || seen[number] {
				continue
			}
			seen[number] = true
			out.LinkedPRs = append(out.LinkedPRs, LinkedPR{
				Number: number,
				Title:  strings.TrimSpace(strOf(pr["title"])),
				URL:    strings.TrimSpace(strOf(pr["url"])),
				State:  strings.ToLower(strings.TrimSpace(strOf(pr["state"]))),
			})
		}
	}
	return out, nil
}

// --- small GraphQL readers -------------------------------------------------
//
// The batch PR query has its own decoders in observer_provider.go; these are
// the two-or-three shapes this file needs and nothing more.

func mapAt(data map[string]any, keys ...string) (map[string]any, bool) {
	cur := data
	for _, key := range keys {
		next, ok := cur[key].(map[string]any)
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

func nodesOf(parent map[string]any, key string) []map[string]any {
	conn, ok := parent[key].(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := conn["nodes"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if node, ok := item.(map[string]any); ok {
			out = append(out, node)
		}
	}
	return out
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}

func intOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}
