package github

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func intelRepo() ports.SCMRepo {
	return ports.SCMRepo{Provider: "github", Host: "github.com", Owner: "o", Name: "r", Repo: "o/r"}
}

func TestRepositoryOverviewReadsDefaultBranchAndTip(t *testing.T) {
	f := newFakeGH(t)
	f.on(http.MethodGet, "/repos/o/r", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"default_branch": "main", "html_url": "https://github.com/o/r",
			"private": true, "archived": false, "pushed_at": "2026-01-02T03:04:05Z",
			"open_issues_count": 9,
		})
	})
	f.on(http.MethodGet, "/repos/o/r/branches/main", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]any{"sha": "tip123"}})
	})

	got, err := newProviderForTest(t, f).RepositoryOverview(ctx(), intelRepo())
	if err != nil {
		t.Fatalf("RepositoryOverview: %v", err)
	}
	if got.DefaultBranch != "main" || got.DefaultBranchSHA != "tip123" || !got.Private || got.OpenIssues != 9 {
		t.Fatalf("overview = %+v", got)
	}
	if got.PushedAt.IsZero() {
		t.Fatal("pushed_at not parsed")
	}
}

// Knowing the default branch without its tip is still most of the answer;
// refusing the whole overview would be the worse trade.
func TestRepositoryOverviewSurvivesAMissingBranchTip(t *testing.T) {
	f := newFakeGH(t)
	f.on(http.MethodGet, "/repos/o/r", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"default_branch": "main"})
	})
	f.on(http.MethodGet, "/repos/o/r/branches/main", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Branch not found"}`, http.StatusNotFound)
	})
	got, err := newProviderForTest(t, f).RepositoryOverview(ctx(), intelRepo())
	if err != nil {
		t.Fatalf("RepositoryOverview: %v", err)
	}
	if got.DefaultBranch != "main" || got.DefaultBranchSHA != "" {
		t.Fatalf("overview = %+v", got)
	}
}

func TestRepositoryOverviewClassifiesRateLimit(t *testing.T) {
	f := newFakeGH(t)
	f.on(http.MethodGet, "/repos/o/r", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "1800000000")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	})
	_, err := newProviderForTest(t, f).RepositoryOverview(ctx(), intelRepo())
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v; want a rate-limit classification", err)
	}
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.ResetAt.IsZero() {
		t.Fatalf("rate-limit error carries no reset hint: %v", err)
	}
}

func TestRepositoryOverviewClassifiesAuthFailure(t *testing.T) {
	f := newFakeGH(t)
	f.on(http.MethodGet, "/repos/o/r", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
	})
	_, err := newProviderForTest(t, f).RepositoryOverview(ctx(), intelRepo())
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("err = %v; want an auth classification", err)
	}
}

// The issues endpoint serves pull requests too; a list that mixed them would
// double-count every open PR.
func TestListIssuesExcludesPullRequestsAndStaysOnOnePage(t *testing.T) {
	f := newFakeGH(t)
	f.on(http.MethodGet, "/repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		if page := r.URL.Query().Get("page"); page != "" && page != "1" {
			t.Errorf("issue listing paginated to page %s; it must not", page)
		}
		// A Link header inviting pagination: this endpoint must decline it.
		w.Header().Set("Link", `<https://api.github.com/repos/o/r/issues?page=2>; rel="next"`)
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"number": 1, "title": "A bug", "state": "open", "html_url": "https://github.com/o/r/issues/1",
				"updated_at": "2026-01-02T03:04:05Z",
				"labels":     []map[string]any{{"name": "bug"}},
				"assignees":  []map[string]any{{"login": "ana"}},
				"milestone":  map[string]any{"title": "v2"}},
			{"number": 2, "title": "A pull request", "state": "open",
				"pull_request": map[string]any{"url": "https://api.github.com/repos/o/r/pulls/2"}},
			{"number": 3, "title": "Another bug", "state": "open"},
		})
	})

	got, err := newProviderForTest(t, f).ListIssues(ctx(), intelRepo(), 10)
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d issues; want 2 (the pull request must be excluded): %+v", len(got), got)
	}
	first := got[0]
	if first.Number != 1 || first.Milestone != "v2" || len(first.Labels) != 1 || first.Assignees[0] != "ana" {
		t.Fatalf("issue not parsed: %+v", first)
	}
	if f.callsTo(http.MethodGet, "/repos/o/r/issues") != 1 {
		t.Fatalf("issue listing made %d requests; it is a one-page read",
			f.callsTo(http.MethodGet, "/repos/o/r/issues"))
	}
}

// The caller's limit is a request, not a promise: the adapter's own ceiling
// wins, and the page size never exceeds GitHub's maximum.
func TestListIssuesClampsTheCallersLimit(t *testing.T) {
	f := newFakeGH(t)
	var perPage int
	f.on(http.MethodGet, "/repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		perPage, _ = strconv.Atoi(r.URL.Query().Get("per_page"))
		rows := make([]map[string]any, 0, 100)
		for i := 1; i <= 100; i++ {
			rows = append(rows, map[string]any{"number": i, "title": "issue", "state": "open"})
		}
		_ = json.NewEncoder(w).Encode(rows)
	})
	got, err := newProviderForTest(t, f).ListIssues(ctx(), intelRepo(), 10000)
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(got) != intelIssueListLimit {
		t.Fatalf("returned %d issues; the ceiling is %d", len(got), intelIssueListLimit)
	}
	if perPage > 100 || perPage <= 0 {
		t.Fatalf("per_page = %d; GitHub's maximum is 100", perPage)
	}
}

func TestFetchIssueReadsCommentsAndGitHubsOwnLinks(t *testing.T) {
	f := newFakeGH(t)
	var query string
	f.on(http.MethodPost, "/graphql", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		query = body.Query
		if got := int(body.Variables["comments"].(float64)); got != intelIssueCommentLimit {
			t.Errorf("comment window = %d; want the bounded %d", got, intelIssueCommentLimit)
		}
		_, _ = w.Write([]byte(`{"data":{"repository":{"issue":{
			"number":7,"title":"Login is broken","url":"https://github.com/o/r/issues/7",
			"state":"OPEN","bodyText":"It fails","updatedAt":"2026-01-02T03:04:05Z",
			"milestone":{"title":"v2"},
			"labels":{"nodes":[{"name":"bug"}]},
			"assignees":{"nodes":[{"login":"ana"}]},
			"comments":{"totalCount":40,"nodes":[
				{"bodyText":"Repro on staging","createdAt":"2026-01-02T03:04:05Z","author":{"login":"bea"}}
			]},
			"timelineItems":{"nodes":[
				{"subject":{"number":3,"title":"Fix login","url":"https://github.com/o/r/pull/3","state":"OPEN"}},
				{"source":{"number":3,"title":"Fix login","url":"https://github.com/o/r/pull/3","state":"OPEN"}},
				{"source":{"number":4,"title":"Related","url":"https://github.com/o/r/pull/4","state":"MERGED"}}
			]}
		}}}}`))
	})

	got, err := newProviderForTest(t, f).FetchIssue(ctx(), intelRepo(), 7)
	if err != nil {
		t.Fatalf("FetchIssue: %v", err)
	}
	if got.Number != 7 || got.Milestone != "v2" || got.State != "open" {
		t.Fatalf("issue = %+v", got)
	}
	// The provider's total is kept, so a bounded list can say it is bounded.
	if got.CommentCount != 40 || len(got.Comments) != 1 || got.Comments[0].Author != "bea" {
		t.Fatalf("comments = %d/%d %+v", len(got.Comments), got.CommentCount, got.Comments)
	}
	// The same PR reached through two timeline event types is one link.
	if len(got.LinkedPRs) != 2 {
		t.Fatalf("linked PRs = %+v; want the duplicate deduped", got.LinkedPRs)
	}
	if !strings.Contains(query, "timelineItems") {
		t.Fatal("linked PRs were not read from GitHub's own timeline")
	}
}

func TestFetchIssueTruncatesAPathologicalComment(t *testing.T) {
	f := newFakeGH(t)
	huge := strings.Repeat("x", 40000)
	f.on(http.MethodPost, "/graphql", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"repository":{"issue":{"number":7,"state":"OPEN",
			"comments":{"totalCount":1,"nodes":[{"bodyText":"` + huge + `"}]}}}}}`))
	})
	got, err := newProviderForTest(t, f).FetchIssue(ctx(), intelRepo(), 7)
	if err != nil {
		t.Fatalf("FetchIssue: %v", err)
	}
	if len(got.Comments) != 1 || !got.Comments[0].Truncated {
		t.Fatalf("comment not truncated: %+v", got.Comments)
	}
	if len(got.Comments[0].Body) > intelCommentBodyLimit {
		t.Fatalf("comment body is %d bytes; the ceiling is %d",
			len(got.Comments[0].Body), intelCommentBodyLimit)
	}
}

func TestFetchIssueMissingIssueIsNotFound(t *testing.T) {
	f := newFakeGH(t)
	f.on(http.MethodPost, "/graphql", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"repository":{"issue":null}}}`))
	})
	if _, err := newProviderForTest(t, f).FetchIssue(ctx(), intelRepo(), 7); !errors.Is(err, ports.ErrSCMNotFound) {
		t.Fatalf("err = %v; want ErrSCMNotFound", err)
	}
	// A nonsense number never reaches the network at all.
	if _, err := newProviderForTest(t, f).FetchIssue(ctx(), intelRepo(), 0); !errors.Is(err, ports.ErrSCMNotFound) {
		t.Fatalf("err = %v; want ErrSCMNotFound", err)
	}
}
