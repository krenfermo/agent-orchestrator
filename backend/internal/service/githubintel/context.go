package githubintel

// context.go -- P4-F's contribution to an agent dispatch.
//
// The rule from the brief is "the worker should receive only what is
// relevant", and the honest reading of that is: each role gets the external
// facts that could change what it does, and nothing else.
//
//	planner  — the branch's state and, if it has one, its pull request's
//	           headline status. A planner is deciding what to do next; whether
//	           work is already open against this branch changes that.
//	reviewer — the pull request's review decision, its diff size, and the names
//	           of checks that are FAILING. A reviewer is judging a change, and
//	           a red pipeline is evidence about that change.
//	worker   — the least of all: branch, whether a PR exists, and failing
//	           checks. A worker is executing a task; a list of other people's
//	           open pull requests is noise it has to read past.
//	repair   — the same as a worker plus the failing checks' detail, which is
//	           usually the thing it was dispatched to fix.
//
// Nothing here is ever a reason to fail or delay a dispatch. The whole call
// runs under a short deadline, degrades to nothing, and says so.

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// contextSource is the source name recorded in the dispatch metrics.
const contextSource = "github"

var _ projectmemory.ExternalContextProvider = (*Service)(nil)

// ExternalContext assembles the role-scoped external block for one dispatch.
// It never fails: an outage, a missing token or an origin AO does not
// recognize all return evidence with a reason and no text.
func (s *Service) ExternalContext(
	ctx context.Context, req projectmemory.ExternalContextRequest,
) projectmemory.ExternalEvidence {
	out := projectmemory.ExternalEvidence{Source: contextSource}

	// A dispatch is decorated, not delayed. The budget here is deliberately
	// tighter than the UI surface's.
	workDir := strings.TrimSpace(req.Workspace)
	if workDir == "" {
		workDir = req.RepoPath
	}
	role := req.Role
	snap := s.Snapshot(ctx, Request{
		ProjectID: req.ProjectID,
		WorkDir:   workDir,
		// One named issue, never the backlog. A dispatch that is against an
		// issue gets that issue's milestone, GitHub-linked pull requests and
		// the tail of its conversation; a dispatch that is not gets no issues
		// at all, because a list of other people's open tickets is noise every
		// role has to read past.
		IssueNumber:      issueNumber(req.IssueRef),
		WithIssues:       false,
		WithPullRequests: true,
		Timeout:          contextTimeout,
	})

	switch snap.Availability {
	case AvailabilityUnavailable:
		out.Degraded = true
		out.Reason = snap.Detail
		return out
	case AvailabilityDegraded:
		out.Degraded = true
		out.Reason = snap.Detail
	}
	out.Rendered = renderForRole(role, snap)
	return out
}

func renderForRole(role projectmemory.PackRole, snap Snapshot) string {
	var b strings.Builder
	repo := snap.Repository

	if repo.Repo != "" {
		line("Repository", repo.Repo, &b)
	}
	branch := repo.CurrentBranch
	if repo.Detached {
		branch = "(detached HEAD)"
	}
	line("Branch", branch, &b)
	if repo.DefaultBranch != "" && !strings.EqualFold(repo.DefaultBranch, repo.CurrentBranch) {
		line("Default branch", repo.DefaultBranch, &b)
	}
	// Ahead/behind is the branch fact that most often explains a surprise, so
	// it is stated for every role — but only when it is non-zero, because
	// "0 ahead, 0 behind" is bytes spent to say nothing happened.
	if repo.Ahead > 0 || repo.Behind > 0 {
		line("Branch position", fmt.Sprintf("%d ahead, %d behind %s",
			repo.Ahead, repo.Behind, orDefault(repo.UpstreamRef, "upstream")), &b)
	}
	if repo.Dirty {
		line("Working tree", "has uncommitted changes", &b)
	}
	if repo.Archived {
		line("Repository state", "archived on GitHub — pushes will be refused", &b)
	}

	if pr := snap.CurrentPR; pr != nil {
		b.WriteString(renderPR(role, *pr))
	} else if role == projectmemory.RolePlanner && len(snap.PullRequests) > 0 {
		// A planner, and only a planner, is told what else is open: it is
		// choosing work, and an open PR against the same area is a reason to
		// choose differently. Bounded to a handful of titles.
		b.WriteString("Open pull requests on this repository:\n")
		for i, pr := range snap.PullRequests {
			if i >= plannerOpenPRLimit {
				fmt.Fprintf(&b, "- ... and %d more\n", len(snap.PullRequests)-plannerOpenPRLimit)
				break
			}
			fmt.Fprintf(&b, "- #%d %s (%s)\n", pr.Number, pr.Title, orDefault(pr.State, "open"))
		}
	}

	for _, issue := range snap.Issues {
		b.WriteString(renderIssue(issue))
	}
	return b.String()
}

// plannerOpenPRLimit bounds the "what else is open" list.
const plannerOpenPRLimit = 5

func renderPR(role projectmemory.PackRole, pr PullRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Pull request for this branch: #%d %s\n", pr.Number, pr.Title)
	line("  URL", pr.URL, &b)
	state := pr.State
	if pr.Draft {
		state = "draft"
	}
	line("  State", state, &b)
	line("  Base", pr.BaseBranch, &b)
	if pr.ReviewDecision != "" {
		line("  Review decision", pr.ReviewDecision, &b)
	}

	// Failing checks reach every role: they are the one external fact that is
	// almost always actionable by whoever is holding the branch.
	if len(pr.FailingChecks) > 0 {
		b.WriteString("  Failing checks: " + strings.Join(pr.FailingChecks, ", ") + "\n")
	} else if pr.ChecksSummary != "" {
		line("  Checks", pr.ChecksSummary, &b)
	}

	switch role {
	case projectmemory.RoleReviewer:
		// A reviewer is judging this change, so the shape of the change and
		// what other reviewers already said are evidence it should have.
		if pr.ChangedFiles > 0 || pr.Additions > 0 || pr.Deletions > 0 {
			line("  Diff", fmt.Sprintf("%d files, +%d/-%d", pr.ChangedFiles, pr.Additions, pr.Deletions), &b)
		}
		if pr.Mergeable != "" {
			line("  Mergeability", pr.Mergeable, &b)
		}
		if len(pr.MergeBlockers) > 0 {
			b.WriteString("  Merge blockers: " + strings.Join(pr.MergeBlockers, ", ") + "\n")
		}
		for _, review := range pr.LatestReviews {
			fmt.Fprintf(&b, "  Review by %s (%s): %s\n",
				orDefault(review.Author, "someone"), orDefault(review.State, "commented"),
				firstLine(review.Body))
		}
	case projectmemory.RolePlanner:
		if pr.Mergeable != "" {
			line("  Mergeability", pr.Mergeable, &b)
		}
	default:
		// Worker and repair get nothing further. They are executing, and the
		// PR's diff statistics do not change what they should type.
	}
	return b.String()
}

func renderIssue(issue Issue) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Issue #%d: %s (%s)\n", issue.Number, issue.Title, orDefault(issue.State, "open"))
	line("  URL", issue.URL, &b)
	if len(issue.Labels) > 0 {
		b.WriteString("  Labels: " + strings.Join(issue.Labels, ", ") + "\n")
	}
	if issue.Milestone != "" {
		line("  Milestone", issue.Milestone, &b)
	}
	for _, pr := range issue.LinkedPRs {
		fmt.Fprintf(&b, "  Linked PR #%d %s (%s)\n", pr.Number, pr.Title, orDefault(pr.State, "open"))
	}
	// The comment tail is the newest few, and the count says how much is not
	// shown, so nobody reads five comments as the whole conversation.
	for _, comment := range issue.Comments {
		fmt.Fprintf(&b, "  %s: %s\n", orDefault(comment.Author, "someone"), firstLine(comment.Body))
	}
	if issue.CommentCount > len(issue.Comments) {
		fmt.Fprintf(&b, "  (%d comments in total; the most recent %d are shown)\n",
			issue.CommentCount, len(issue.Comments))
	}
	return b.String()
}

// issueNumber pulls the issue number out of whatever a boundary had: a bare
// number, an "owner/repo#12" tracker id, or a GitHub issue URL. Anything it
// cannot read yields zero, which means "no focused issue" and is always safe.
func issueNumber(ref string) int {
	raw := strings.TrimSpace(ref)
	if raw == "" {
		return 0
	}
	if idx := strings.LastIndexByte(raw, '#'); idx >= 0 {
		raw = raw[idx+1:]
	} else if idx := strings.LastIndex(raw, "/issues/"); idx >= 0 {
		raw = raw[idx+len("/issues/"):]
	}
	raw = strings.TrimSpace(strings.Trim(raw, "/"))
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func line(label, value string, b *strings.Builder) {
	if strings.TrimSpace(value) == "" {
		return
	}
	b.WriteString(label)
	b.WriteString(": ")
	b.WriteString(value)
	b.WriteString("\n")
}

func orDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// firstLine keeps a summary a summary. A review body or a comment can be a
// page long; one line of it is a signal, and the rest is what the URL is for.
func firstLine(s string) string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return "(no text)"
	}
	if idx := strings.IndexByte(trimmed, '\n'); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	const limit = 160
	if len(trimmed) > limit {
		return trimmed[:limit] + "..."
	}
	return trimmed
}
