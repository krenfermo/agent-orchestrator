package review

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
)

// fakeCredentialCloser records which review runs were closed out, and can fail
// the way a locked database would.
type fakeCredentialCloser struct {
	closed []string
	err    error
}

func (f *fakeCredentialCloser) CloseReviewRun(_ context.Context, reviewRunID string) error {
	f.closed = append(f.closed, reviewRunID)
	return f.err
}

// THE REGRESSION, at the submit boundary. Recording the verdict is the end of
// the reviewer's authority, and before this nothing on that path said so: the
// credential was taken back only where AO killed the reviewer or failed to
// start one, so a reviewer that simply finished kept a live identity over a
// concluded review for the rest of its TTL.
func TestSubmitEndsTheReviewersCredential(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	st := &fakeStore{
		ok:  true,
		run: domain.ReviewRun{ID: "run-1", SessionID: "mer-1", BatchID: "batch-1", PRURL: "pr1", TargetSHA: "sha1", Status: domain.ReviewRunRunning},
		prs: []domain.PullRequest{{URL: "pr1", HeadSHA: "sha1"}},
	}
	creds := &fakeCredentialCloser{}
	svc := New(nil, st,
		WithLifecycleReducer(&fakeReducer{outcome: lifecycle.ReviewDeliverySent}),
		WithClock(func() time.Time { return now }),
		WithAgentCredentials(creds))

	if _, err := svc.Submit(context.Background(), "mer-1", "run-1", domain.VerdictApproved, "", ""); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if len(creds.closed) != 1 || creds.closed[0] != "run-1" {
		t.Fatalf("credentials closed for %v; want exactly the submitted run", creds.closed)
	}
}

// A submission carrying several runs must not end the reviewer's identity part
// way through: it keeps it until everything it came to record is durable.
func TestSubmitManyEndsEveryRunsCredentialAfterAllOfThemAreRecorded(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	st := &fakeStore{
		batchRuns: []domain.ReviewRun{
			{ID: "run-a", SessionID: "mer-1", BatchID: "batch-1", PRURL: "pr1", TargetSHA: "sha1", Status: domain.ReviewRunRunning},
			{ID: "run-b", SessionID: "mer-1", BatchID: "batch-1", PRURL: "pr2", TargetSHA: "sha2", Status: domain.ReviewRunRunning},
		},
		prs: []domain.PullRequest{{URL: "pr1", HeadSHA: "sha1"}, {URL: "pr2", HeadSHA: "sha2"}},
	}
	creds := &fakeCredentialCloser{}
	svc := New(nil, st,
		WithLifecycleReducer(&fakeReducer{outcome: lifecycle.ReviewDeliverySent}),
		WithClock(func() time.Time { return now }),
		WithAgentCredentials(creds))

	if _, err := svc.SubmitMany(context.Background(), "mer-1", []SubmittedReview{
		{RunID: "run-a", Verdict: domain.VerdictApproved},
		{RunID: "run-b", Verdict: domain.VerdictApproved},
	}); err != nil {
		t.Fatalf("SubmitMany: %v", err)
	}
	if len(creds.closed) != 2 || creds.closed[0] != "run-a" || creds.closed[1] != "run-b" {
		t.Fatalf("credentials closed for %v; want both runs", creds.closed)
	}
	if st.updateCalls != 2 {
		t.Fatalf("runs recorded = %d; the credentials were closed before the results were", st.updateCalls)
	}
}

// A cleanup that fails must never destroy the verdict it was cleaning up after.
// The review concluded; the revocation is a durable obligation that the sweep
// discharges, and turning a transient write failure into a lost review is the
// exact shape of failure this area already carries one scar from.
func TestAFailedRevocationDoesNotFailTheSubmission(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	st := &fakeStore{
		ok:  true,
		run: domain.ReviewRun{ID: "run-1", SessionID: "mer-1", BatchID: "batch-1", PRURL: "pr1", TargetSHA: "sha1", Status: domain.ReviewRunRunning},
		prs: []domain.PullRequest{{URL: "pr1", HeadSHA: "sha1"}},
	}
	creds := &fakeCredentialCloser{err: errors.New("database is locked")}
	svc := New(nil, st,
		WithLifecycleReducer(&fakeReducer{outcome: lifecycle.ReviewDeliverySent}),
		WithClock(func() time.Time { return now }),
		WithAgentCredentials(creds))

	run, err := svc.Submit(context.Background(), "mer-1", "run-1", domain.VerdictChangesRequested, "fix it", "")
	if err != nil {
		t.Fatalf("a failed credential revocation destroyed the verdict: %v", err)
	}
	if run.Status != domain.ReviewRunDelivered || run.Verdict != domain.VerdictChangesRequested {
		t.Fatalf("the verdict was not recorded and delivered: %+v", run)
	}
	if len(creds.closed) != 1 {
		t.Fatalf("the revocation was not attempted: %v", creds.closed)
	}
}

// With no identity layer wired -- every build and every test that predates it
// -- the submit path is byte for byte what it was.
func TestSubmitWithoutAnIdentityLayerIsUnchanged(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	st := &fakeStore{
		ok:  true,
		run: domain.ReviewRun{ID: "run-1", SessionID: "mer-1", BatchID: "batch-1", PRURL: "pr1", TargetSHA: "sha1", Status: domain.ReviewRunRunning},
		prs: []domain.PullRequest{{URL: "pr1", HeadSHA: "sha1"}},
	}
	svc := New(nil, st,
		WithLifecycleReducer(&fakeReducer{outcome: lifecycle.ReviewDeliverySent}),
		WithClock(func() time.Time { return now }))

	if _, err := svc.Submit(context.Background(), "mer-1", "run-1", domain.VerdictApproved, "", ""); err != nil {
		t.Fatalf("Submit: %v", err)
	}
}
