package workflow_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// reviewer_identity_expected_test.go -- AR-1a / D-SEC-2 (Codex AR1A-02): a
// review run created for a launcher that hands its reviewer AO's own
// credential is marked as expecting that identity in the SAME insert that
// makes it visible as running, so a header-less verdict cannot be recorded in
// the window before the credential is minted.

type identityIssuingLauncher struct {
	*fakeReviewerLauncher
	issues bool
}

func (l identityIssuingLauncher) IssuesReviewerIdentity() bool { return l.issues }

type staticRunOwners struct {
	owner *domain.UserID
	err   error
}

func (s staticRunOwners) GetWorkflowRunOwner(context.Context, string) (*domain.UserID, error) {
	return s.owner, s.err
}

func dispatchReviewWithIdentity(t *testing.T, issues bool, owner *domain.UserID) (domain.ReviewRun, workflowcore.ReviewerLaunchRequest) {
	t.Helper()
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	fake := &fakeReviewerLauncher{}
	store := newFakeStore()
	store.reviewRuns = reviewRuns
	clk := &fakeClock{t: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store: store, Spawner: spawner, SessionFacts: sessionFacts, WorkspaceFacts: workspaceFacts,
		ReviewRuns: reviewRuns, ReviewerLauncher: identityIssuingLauncher{fakeReviewerLauncher: fake, issues: issues},
		RunOwners:              staticRunOwners{owner: owner},
		ReviewerIdentityIssuer: identityIssuingLauncher{fakeReviewerLauncher: fake, issues: issues},
		Clock:                  clk.Now,
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("id%d", idSeq)
		},
	})
	ctx := context.Background()
	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)
	got, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	review := reviewStepFrom(got)
	if review.Step.ReviewRunID == nil || fake.launchCalls != 1 {
		t.Fatalf("review not dispatched (run id %v, launches %d)", review.Step.ReviewRunID, fake.launchCalls)
	}
	return reviewRuns.runs[*review.Step.ReviewRunID], fake.lastReq
}

func TestAnOwnedRunsReviewIsCreatedExpectingTheReviewersIdentity(t *testing.T) {
	owner := domain.UserID("user-ada")
	run, req := dispatchReviewWithIdentity(t, true, &owner)
	if !run.ReviewerIdentityExpected {
		t.Fatalf("review run inserted without ReviewerIdentityExpected")
	}
	if !req.ReviewerIdentityExpected || req.OwnerUserID != owner {
		t.Fatalf("launch request = expected:%v owner:%q, want the marker and the run owner", req.ReviewerIdentityExpected, req.OwnerUserID)
	}
}

func TestAnUnownedLegacyRunKeepsItsPreviousReviewBehaviour(t *testing.T) {
	run, req := dispatchReviewWithIdentity(t, true, nil)
	if run.ReviewerIdentityExpected || req.ReviewerIdentityExpected {
		t.Fatalf("an unowned run was marked as expecting a reviewer identity it cannot be minted")
	}
}

func TestALauncherWithoutAnIdentityLayerNeverMarksTheRun(t *testing.T) {
	owner := domain.UserID("user-ada")
	run, req := dispatchReviewWithIdentity(t, false, &owner)
	if run.ReviewerIdentityExpected || req.ReviewerIdentityExpected {
		t.Fatalf("a launcher that issues no identity marked the run")
	}
}

// Codex AR1A-R2-01: an owner lookup that FAILS is not "no owner". The review
// run must not be created without the identity marker (that would reopen the
// pre-mint window); the dispatch refuses before inserting anything.
func TestAFailedOwnerLookupNeverCreatesAnUnmarkedReview(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	fake := &fakeReviewerLauncher{}
	store := newFakeStore()
	store.reviewRuns = reviewRuns
	clk := &fakeClock{t: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store: store, Spawner: spawner, SessionFacts: sessionFacts, WorkspaceFacts: workspaceFacts,
		ReviewRuns: reviewRuns, ReviewerLauncher: identityIssuingLauncher{fakeReviewerLauncher: fake, issues: true},
		RunOwners:              staticRunOwners{err: errors.New("database is locked")},
		ReviewerIdentityIssuer: identityIssuingLauncher{fakeReviewerLauncher: fake, issues: true},
		Clock:                  clk.Now,
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("id%d", idSeq)
		},
	})
	ctx := context.Background()
	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)
	_, _ = c.ContinueRun(ctx, created.Run.ID)
	if reviewRuns.insertCalls != 0 || fake.launchCalls != 0 {
		t.Fatalf("a review run was inserted (%d) or launched (%d) after a failed owner lookup", reviewRuns.insertCalls, fake.launchCalls)
	}
	for id, r := range reviewRuns.runs {
		if r.Status == domain.ReviewRunRunning && !r.ReviewerIdentityExpected {
			t.Fatalf("running review %s exists without the identity marker", id)
		}
	}
}
