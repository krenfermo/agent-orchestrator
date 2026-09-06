package githubintel

import (
	"context"
	"strings"
	"testing"

	scmgithub "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/github"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

func contextService(t *testing.T) *Service {
	t.Helper()
	gh := &fakeGitHub{
		creds:    true,
		overview: scmgithub.RepoOverview{DefaultBranch: "main", DefaultBranchSHA: "m1"},
		detail: scmgithub.IssueDetail{
			IssueSummary: scmgithub.IssueSummary{
				Number: 7, Title: "Login is broken", State: "open",
				Labels: []string{"bug"}, Milestone: "v2",
			},
			CommentCount: 40,
			Comments:     []scmgithub.IssueComment{{Author: "ana", Body: "Repro on staging"}},
			LinkedPRs:    []scmgithub.LinkedPR{{Number: 3, Title: "Fix login", State: "open"}},
		},
	}
	scm := &fakeSCM{repo: ghRepo(), parseOK: true,
		listed: []ports.SCMPRObservation{
			{Number: 12, SourceBranch: "feat/thing"},
			{Number: 44, Title: "Someone else's work", SourceBranch: "other/branch"},
		},
		fetched: []ports.SCMObservation{
			{Fetched: true,
				PR: ports.SCMPRObservation{Number: 12, Title: "Do the thing", SourceBranch: "feat/thing",
					TargetBranch: "main", State: "open", Additions: 40, Deletions: 5, ChangedFiles: 3},
				Review: ports.SCMReviewObservation{Decision: "changes_requested",
					Reviews: []ports.SCMReviewSummaryObservation{{Author: "carla", State: "changes_requested", Body: "Needs a test.\nAnd a rename."}}},
				CI:           ports.SCMCIObservation{Summary: "failing", FailedChecks: []ports.SCMCheckObservation{{Name: "backend-tests"}}},
				Mergeability: ports.SCMMergeabilityObservation{State: "blocked", Blockers: []string{"failing checks"}},
			},
			{Fetched: true, PR: ports.SCMPRObservation{Number: 44, Title: "Someone else's work", SourceBranch: "other/branch", State: "open"}},
		},
	}
	return newService(t, gh, scm, healthyLocal())
}

func TestExternalContextGivesEachRoleWhatItNeeds(t *testing.T) {
	svc := contextService(t)

	reviewer := svc.ExternalContext(context.Background(), projectmemory.ExternalContextRequest{
		ProjectID: projectID, RepoPath: "/tmp/repo", Role: projectmemory.RoleReviewer,
	})
	if reviewer.Rendered == "" {
		t.Fatalf("reviewer got no external context (reason=%q)", reviewer.Reason)
	}
	for _, want := range []string{"changes_requested", "backend-tests", "3 files, +40/-5", "carla"} {
		if !strings.Contains(reviewer.Rendered, want) {
			t.Fatalf("reviewer context missing %q:\n%s", want, reviewer.Rendered)
		}
	}
	// A reviewer's summary of a review is one line of it. The rest is what the
	// URL is for.
	if strings.Contains(reviewer.Rendered, "And a rename.") {
		t.Fatalf("reviewer context carried a whole review body:\n%s", reviewer.Rendered)
	}

	// A worker is executing, so the diff statistics and other people's open
	// pull requests are bytes it has to read past.
	worker := svc.ExternalContext(context.Background(), projectmemory.ExternalContextRequest{
		ProjectID: projectID, RepoPath: "/tmp/repo", Role: projectmemory.RoleWorker,
	})
	if !strings.Contains(worker.Rendered, "backend-tests") {
		t.Fatalf("worker context lost the failing check:\n%s", worker.Rendered)
	}
	for _, unwanted := range []string{"3 files, +40/-5", "Someone else's work"} {
		if strings.Contains(worker.Rendered, unwanted) {
			t.Fatalf("worker context carried %q, which cannot change what it does:\n%s", unwanted, worker.Rendered)
		}
	}
	if len(worker.Rendered) >= len(reviewer.Rendered) {
		t.Fatalf("worker context is not narrower than the reviewer's (%d vs %d bytes)",
			len(worker.Rendered), len(reviewer.Rendered))
	}

	// A planner is choosing work, so what else is already open matters.
	planner := svc.ExternalContext(context.Background(), projectmemory.ExternalContextRequest{
		ProjectID: projectID, RepoPath: "/tmp/repo", Role: projectmemory.RolePlanner,
	})
	if !strings.Contains(planner.Rendered, "2 ahead, 1 behind") {
		t.Fatalf("planner context lost the branch position:\n%s", planner.Rendered)
	}
}

// A dispatch against an issue gets that issue, with the things the tracker's
// pre-fetched body cannot carry: milestone, GitHub-linked PRs, and an honest
// statement of how much of the conversation is shown.
func TestExternalContextFocusesTheDispatchsIssue(t *testing.T) {
	out := contextService(t).ExternalContext(context.Background(), projectmemory.ExternalContextRequest{
		ProjectID: projectID, RepoPath: "/tmp/repo", Role: projectmemory.RoleWorker,
		IssueRef: "DarkaMX/MEDUSASASBACK#7",
	})
	for _, want := range []string{"Issue #7", "Login is broken", "Milestone: v2", "Linked PR #3", "40 comments in total"} {
		if !strings.Contains(out.Rendered, want) {
			t.Fatalf("issue context missing %q:\n%s", want, out.Rendered)
		}
	}
}

func TestIssueNumberParsesEveryFormABoundaryHas(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"12", 12},
		{"#12", 12},
		{"owner/repo#12", 12},
		{"https://github.com/owner/repo/issues/12", 12},
		{"", 0},
		{"not-a-number", 0},
		{"owner/repo#0", 0},
		{"owner/repo#-3", 0},
	} {
		if got := issueNumber(tc.in); got != tc.want {
			t.Fatalf("issueNumber(%q) = %d; want %d", tc.in, got, tc.want)
		}
	}
}

// The whole point of the degraded contract: GitHub being down produces a
// dispatch with no external context and a reason, never a failed dispatch.
func TestExternalContextDegradesSilently(t *testing.T) {
	svc := newService(t, &fakeGitHub{creds: false}, &fakeSCM{repo: ghRepo(), parseOK: true}, LocalState{})
	out := svc.ExternalContext(context.Background(), projectmemory.ExternalContextRequest{
		ProjectID: projectID, RepoPath: "/tmp/repo", Role: projectmemory.RoleWorker,
	})
	if !out.Degraded || out.Reason == "" {
		t.Fatalf("degraded state not reported: %+v", out)
	}
	if out.Rendered != "" {
		t.Fatalf("rendered context without a usable snapshot:\n%s", out.Rendered)
	}
	if out.Source != contextSource {
		t.Fatalf("source = %q; want %q", out.Source, contextSource)
	}
}
