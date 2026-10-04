package githubintel

// api.go -- the projection onto the controller's wire types.
//
// Kept apart from the engine beside it for the reason the rest of this
// codebase keeps vocabularies apart: the service reasons in its own types and
// the HTTP shapes are a contract with the frontend that must be free to change
// independently. This file is the only place the two meet.

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
)

var _ controllers.ProjectGitHubService = (*Service)(nil)

// ProjectGitHub implements the controller's read. It returns no error in any
// GitHub-related case: an outage, a missing token and an unrecognized origin
// are all states of the snapshot, and turning them into HTTP errors would make
// every caller invent its own story about what they mean.
func (s *Service) ProjectGitHub(
	ctx context.Context, req controllers.ProjectGitHubQuery,
) (controllers.ProjectGitHubIntelligence, error) {
	snap := s.Snapshot(ctx, Request{
		ProjectID:   req.ProjectID,
		Branch:      req.Branch,
		IssueNumber: req.Issue,
		// The UI surface wants the whole picture; a context pack asks for less
		// through the engine directly.
		WithIssues:       true,
		WithPullRequests: true,
		SkipCache:        req.Refresh,
	})
	return toWire(snap), nil
}

func toWire(in Snapshot) controllers.ProjectGitHubIntelligence {
	out := controllers.ProjectGitHubIntelligence{
		ProjectID:     in.ProjectID,
		Availability:  string(in.Availability),
		Reason:        in.Reason,
		Detail:        in.Detail,
		Authenticated: in.Authenticated,
		Repository:    repositoryToWire(in.Repository),
		ObservedAt:    formatTime(in.ObservedAt),
		Stale:         in.Stale,
	}
	for _, pr := range in.PullRequests {
		out.PullRequests = append(out.PullRequests, prToWire(pr))
	}
	if in.CurrentPR != nil {
		current := prToWire(*in.CurrentPR)
		out.CurrentPR = &current
	}
	for _, issue := range in.Issues {
		out.Issues = append(out.Issues, issueToWire(issue))
	}
	return out
}

func repositoryToWire(in Repository) controllers.ProjectGitHubRepository {
	out := controllers.ProjectGitHubRepository{
		Provider: in.Provider, Host: in.Host, Repo: in.Repo, URL: in.URL,
		OriginURL: in.OriginURL, AliasResolved: in.AliasResolved,
		DefaultBranch: in.DefaultBranch, CurrentBranch: in.CurrentBranch,
		Detached: in.Detached, HeadSHA: in.HeadSHA,
		RemoteHeadSHA: in.RemoteHeadSHA, RemoteHeadRef: in.RemoteHeadRef,
		UpstreamRef: in.UpstreamRef, Ahead: in.Ahead, Behind: in.Behind,
		Dirty: in.Dirty, Private: in.Private, Archived: in.Archived,
	}
	for _, commit := range in.RecentCommits {
		out.RecentCommits = append(out.RecentCommits, controllers.ProjectGitHubCommit{
			SHA: commit.SHA, Subject: commit.Subject, Author: commit.Author,
			Date: formatTime(commit.Date),
		})
	}
	return out
}

func prToWire(in PullRequest) controllers.ProjectGitHubPullRequest {
	out := controllers.ProjectGitHubPullRequest{
		Number: in.Number, Title: in.Title, URL: in.URL, Author: in.Author,
		State: in.State, Draft: in.Draft, BaseBranch: in.BaseBranch,
		HeadBranch: in.HeadBranch, HeadRepo: in.HeadRepo, HeadSHA: in.HeadSHA,
		ReviewDecision: in.ReviewDecision, RequestedReviewers: in.RequestedReviewers,
		ChecksSummary: in.ChecksSummary, ChecksPassed: in.ChecksPassed,
		ChecksFailed: in.ChecksFailed, ChecksPending: in.ChecksPending,
		FailingChecks: in.FailingChecks, Mergeable: in.Mergeable,
		MergeBlockers: in.MergeBlockers, Additions: in.Additions,
		Deletions: in.Deletions, ChangedFiles: in.ChangedFiles,
		UpdatedAt: formatTime(in.UpdatedAt),
	}
	for _, review := range in.LatestReviews {
		out.LatestReviews = append(out.LatestReviews, controllers.ProjectGitHubReview(review))
	}
	return out
}

func issueToWire(in Issue) controllers.ProjectGitHubIssue {
	out := controllers.ProjectGitHubIssue{
		Number: in.Number, Title: in.Title, State: in.State, URL: in.URL,
		Labels: in.Labels, Assignees: in.Assignees, Milestone: in.Milestone,
		UpdatedAt: formatTime(in.UpdatedAt), CommentCount: in.CommentCount,
		Body: in.Body,
	}
	for _, pr := range in.LinkedPRs {
		out.LinkedPRs = append(out.LinkedPRs, controllers.ProjectGitHubLinkedPR(pr))
	}
	for _, comment := range in.Comments {
		out.Comments = append(out.Comments, controllers.ProjectGitHubComment{
			Author: comment.Author, Body: comment.Body, Truncated: comment.Truncated,
			CreatedAt: formatTime(comment.CreatedAt),
		})
	}
	return out
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
