package review

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
)

// submit_authority_test.go -- AR-1a / D-SEC-2: who may record a review verdict.
//
// Before AR-1a the only question asked of a submission was whether the review
// run belonged to the addressed worker session. Every credential bound to that
// session satisfies that -- the worker's own included -- so a worker could
// approve its own review.

// ledgerStore is the production-shaped store: it also answers which reviewer
// credentials AO minted for a review run.
type ledgerStore struct {
	*fakeStore
	creds   []domain.AgentCredential
	credErr error
}

func (l *ledgerStore) ListAgentCredentialsForReviewRun(_ context.Context, reviewRunID string) ([]domain.AgentCredential, error) {
	if l.credErr != nil {
		return nil, l.credErr
	}
	var out []domain.AgentCredential
	for _, c := range l.creds {
		if c.ReviewRunID == reviewRunID {
			out = append(out, c)
		}
	}
	return out, nil
}

var authorityNow = time.Unix(10_000, 0).UTC()

func runningRun() domain.ReviewRun {
	return domain.ReviewRun{ID: "run-1", SessionID: "mer-1", BatchID: "batch-1", PRURL: "pr1", TargetSHA: "sha1", Status: domain.ReviewRunRunning}
}

func newAuthorityService(st Store) *Service {
	return New(nil, st,
		WithLifecycleReducer(&fakeReducer{outcome: lifecycle.ReviewDeliverySent}),
		WithClock(func() time.Time { return authorityNow }))
}

func liveReviewerCredential(reviewRunID string) domain.AgentCredential {
	return domain.AgentCredential{
		ID: "agc-1", Role: domain.AgentRoleReviewer, SessionID: "mer-1", ReviewRunID: reviewRunID,
		ExpiresAt: authorityNow.Add(time.Hour),
	}
}

func reviewer(reviewRunID string) Submitter {
	return Submitter{Agent: &domain.AgentAuthority{
		CredentialID: "agc-1", Role: domain.AgentRoleReviewer, SessionID: "mer-1", ReviewRunID: reviewRunID,
	}}
}

func submitAs(t *testing.T, svc *Service, who Submitter, verdict domain.ReviewVerdict, body string) (domain.ReviewRun, error) {
	t.Helper()
	runs, err := svc.SubmitMany(context.Background(), who, "mer-1", []SubmittedReview{{RunID: "run-1", Verdict: verdict, Body: body}})
	if err != nil {
		return domain.ReviewRun{}, err
	}
	return runs[0], nil
}

// The reviewer AO launched for exactly this run, over exactly this session,
// records its verdict.
func TestTheLaunchedReviewerRecordsItsVerdict(t *testing.T) {
	st := &ledgerStore{fakeStore: &fakeStore{ok: true, run: runningRun(), prs: []domain.PullRequest{{URL: "pr1", HeadSHA: "sha1"}}},
		creds: []domain.AgentCredential{liveReviewerCredential("run-1")}}
	run, err := submitAs(t, newAuthorityService(st), reviewer("run-1"), domain.VerdictApproved, "")
	if err != nil {
		t.Fatalf("the launched reviewer was refused: %v", err)
	}
	if run.Verdict != domain.VerdictApproved || st.updateCalls != 1 {
		t.Fatalf("verdict not recorded: %+v (updates=%d)", run, st.updateCalls)
	}
}

// THE DEFECT: a worker credential bound to the same session approving its own
// review.
func TestAWorkerCannotRecordAReviewVerdict(t *testing.T) {
	st := &ledgerStore{fakeStore: &fakeStore{ok: true, run: runningRun()}, creds: []domain.AgentCredential{liveReviewerCredential("run-1")}}
	worker := Submitter{Agent: &domain.AgentAuthority{CredentialID: "agc-w", Role: domain.AgentRoleWorker, SessionID: "mer-1", AttemptID: "att-1"}}
	_, err := submitAs(t, newAuthorityService(st), worker, domain.VerdictApproved, "")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("worker submission err = %v, want ErrForbidden", err)
	}
	if st.updateCalls != 0 {
		t.Fatalf("a worker's verdict reached the store")
	}
}

// A reviewer credential bound to another session cannot speak for this one,
// even with the right run id.
func TestAReviewerBoundToAnotherSessionCannotRecordTheVerdict(t *testing.T) {
	st := &ledgerStore{fakeStore: &fakeStore{ok: true, run: runningRun()}}
	other := reviewer("run-1")
	other.Agent.SessionID = "mer-2"
	if _, err := submitAs(t, newAuthorityService(st), other, domain.VerdictApproved, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other-session reviewer err = %v, want ErrForbidden", err)
	}
	if st.updateCalls != 0 {
		t.Fatalf("an other-session verdict reached the store")
	}
}

// A stale reviewer -- one minted for a previous (or later) review run of the
// same step -- cannot record the verdict of the current run.
func TestAStaleGenerationReviewerCannotRecordTheCurrentVerdict(t *testing.T) {
	st := &ledgerStore{fakeStore: &fakeStore{ok: true, run: runningRun()}, creds: []domain.AgentCredential{liveReviewerCredential("run-1")}}
	if _, err := submitAs(t, newAuthorityService(st), reviewer("run-0"), domain.VerdictApproved, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("stale reviewer err = %v, want ErrForbidden", err)
	}
	empty := reviewer("")
	if _, err := submitAs(t, newAuthorityService(st), empty, domain.VerdictApproved, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reviewer credential without a review run err = %v, want ErrForbidden", err)
	}
	if st.updateCalls != 0 {
		t.Fatalf("a stale verdict reached the store")
	}
}

// Replaying the reviewer's submission never produces a second verdict: the
// identical replay is idempotent and a different one never overwrites it.
func TestReplayingASubmissionNeverRecordsASecondVerdict(t *testing.T) {
	st := &ledgerStore{fakeStore: &fakeStore{ok: true, run: runningRun(), prs: []domain.PullRequest{{URL: "pr1", HeadSHA: "sha1"}}},
		creds: []domain.AgentCredential{liveReviewerCredential("run-1")}}
	svc := newAuthorityService(st)
	if _, err := submitAs(t, svc, reviewer("run-1"), domain.VerdictChangesRequested, "fix it"); err != nil {
		t.Fatalf("first submission: %v", err)
	}
	if _, err := submitAs(t, svc, reviewer("run-1"), domain.VerdictChangesRequested, "fix it"); err != nil {
		t.Fatalf("identical replay should be idempotent: %v", err)
	}
	// Once delivered, a replay with a different verdict returns the recorded run
	// unchanged; whatever it answers, the recorded verdict never moves.
	got, err := submitAs(t, svc, reviewer("run-1"), domain.VerdictApproved, "")
	if err == nil && got.Verdict != domain.VerdictChangesRequested {
		t.Fatalf("a replay changed the recorded verdict: %+v", got)
	}
	if st.run.Verdict != domain.VerdictChangesRequested {
		t.Fatalf("stored verdict = %q, want the first one", st.run.Verdict)
	}
	if st.updateCalls != 1 {
		t.Fatalf("result writes = %d; want exactly one verdict", st.updateCalls)
	}
}

// The header-less path a worker shell would take on a trusted-local install
// (it resolves to a person): refused while the run is running and its reviewer
// holds a live credential.
func TestAPersonCannotSpeakOverARunningReviewWhoseReviewerHoldsAnIdentity(t *testing.T) {
	st := &ledgerStore{fakeStore: &fakeStore{ok: true, run: runningRun()}, creds: []domain.AgentCredential{liveReviewerCredential("run-1")}}
	if _, err := submitAs(t, newAuthorityService(st), Submitter{}, domain.VerdictApproved, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("person over a credentialed running review err = %v, want ErrForbidden", err)
	}
	if st.updateCalls != 0 {
		t.Fatalf("a person's verdict reached the store")
	}
}

// Compatibility: a reviewer launched without a credential (trusted-local where
// minting failed, or a build without the identity layer) still records its
// verdict header-less, exactly as before.
func TestAPersonStillRecordsWhenTheReviewerHoldsNoLiveIdentity(t *testing.T) {
	revoked := liveReviewerCredential("run-1")
	at := authorityNow.Add(-time.Minute)
	revoked.RevokedAt = &at
	for name, creds := range map[string][]domain.AgentCredential{
		"none minted":       nil,
		"revoked at launch": {revoked},
	} {
		t.Run(name, func(t *testing.T) {
			st := &ledgerStore{fakeStore: &fakeStore{ok: true, run: runningRun(), prs: []domain.PullRequest{{URL: "pr1", HeadSHA: "sha1"}}}, creds: creds}
			if _, err := submitAs(t, newAuthorityService(st), Submitter{}, domain.VerdictApproved, ""); err != nil {
				t.Fatalf("header-less submission without a live reviewer identity refused: %v", err)
			}
		})
	}
}

// Compatibility: once AO has closed the run out (the stall path), its reviewer's
// credential is swept together with its file, so the reviewer's late verdict
// arrives header-less. It must still be preserved, as before AR-1a.
func TestALateVerdictArrivingHeaderlessIsStillPreserved(t *testing.T) {
	run := runningRun()
	run.Status = domain.ReviewRunCancelled
	st := &ledgerStore{fakeStore: &fakeStore{ok: true, run: run}, creds: []domain.AgentCredential{liveReviewerCredential("run-1")}}
	if _, err := submitAs(t, newAuthorityService(st), Submitter{}, domain.VerdictApproved, ""); err != nil {
		t.Fatalf("late verdict refused: %v", err)
	}
	if st.lateVerdictCalls != 1 {
		t.Fatalf("late verdict not preserved (calls=%d)", st.lateVerdictCalls)
	}
}

// An unreadable credential ledger refuses: a rule AO cannot evaluate is not
// satisfied.
func TestAnUnreadableCredentialLedgerRefusesAPersonsSubmission(t *testing.T) {
	st := &ledgerStore{fakeStore: &fakeStore{ok: true, run: runningRun()}, credErr: errors.New("database is locked")}
	if _, err := submitAs(t, newAuthorityService(st), Submitter{}, domain.VerdictApproved, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unreadable ledger err = %v, want ErrForbidden", err)
	}
}

// An unrevoked reviewer credential counts even once expired: the same
// conservative predicate the guarded SQL write evaluates (Codex AR1A-02).
func TestAnExpiredButUnrevokedReviewerCredentialStillSpeaksForTheRun(t *testing.T) {
	expired := liveReviewerCredential("run-1")
	expired.ExpiresAt = authorityNow.Add(-time.Second)
	st := &ledgerStore{fakeStore: &fakeStore{ok: true, run: runningRun()}, creds: []domain.AgentCredential{expired}}
	if _, err := submitAs(t, newAuthorityService(st), Submitter{}, domain.VerdictApproved, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
}

// THE PRE-MINT WINDOW (Codex AR1A-02): the run is already running but its
// reviewer's credential has not been minted yet. A run created for a launcher
// that issues reviewer identities refuses a header-less verdict anyway.
func TestAHeaderlessVerdictIsRefusedBeforeTheReviewerCredentialIsMinted(t *testing.T) {
	run := runningRun()
	run.ReviewerIdentityExpected = true
	st := &guardedStore{ledgerStore: &ledgerStore{fakeStore: &fakeStore{ok: true, run: run}}}
	if _, err := submitAs(t, newAuthorityService(st), Submitter{}, domain.VerdictApproved, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if st.updateCalls != 0 || st.guardedCalls != 0 {
		t.Fatalf("a verdict write was attempted (plain=%d guarded=%d)", st.updateCalls, st.guardedCalls)
	}
	// The reviewer itself, once minted, still records it.
	st.creds = []domain.AgentCredential{liveReviewerCredential("run-1")}
	if _, err := submitAs(t, newAuthorityService(st), reviewer("run-1"), domain.VerdictApproved, ""); err != nil {
		t.Fatalf("the launched reviewer was refused: %v", err)
	}
}

// guardedStore adds the production guarded write; refuse simulates a reviewer
// credential minted between the ledger read and the write.
type guardedStore struct {
	*ledgerStore
	refuse       bool
	cancelFirst  bool
	guardedCalls int
}

func (g *guardedStore) UpdateReviewRunResultWithoutReviewerIdentity(ctx context.Context, id string, status domain.ReviewRunStatus, verdict domain.ReviewVerdict, body, githubReviewID string, autoInjectReview bool) (bool, error) {
	g.guardedCalls++
	if g.cancelFirst {
		g.run.Status = domain.ReviewRunCancelled
		return false, nil
	}
	if g.refuse || g.run.ReviewerIdentityExpected {
		return false, nil
	}
	return g.UpdateReviewRunResult(ctx, id, status, verdict, body, githubReviewID, autoInjectReview)
}

// THE READ-THEN-WRITE RACE (Codex AR1A-02): the ledger said "no reviewer
// identity", then one was minted before the write. The guarded write refuses
// and, the run still running, the submission is forbidden -- never recorded.
func TestACredentialMintedBetweenCheckAndWriteRefusesTheHeaderlessVerdict(t *testing.T) {
	st := &guardedStore{ledgerStore: &ledgerStore{fakeStore: &fakeStore{ok: true, run: runningRun()}}, refuse: true}
	if _, err := submitAs(t, newAuthorityService(st), Submitter{}, domain.VerdictApproved, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if st.guardedCalls != 1 || st.updateCalls != 0 || st.lateVerdictCalls != 0 {
		t.Fatalf("guarded=%d plain=%d late=%d; want exactly one refused guarded write", st.guardedCalls, st.updateCalls, st.lateVerdictCalls)
	}
}

// If AO closed the run out between the read and the guarded write, the verdict
// is a late one and is preserved exactly as before.
func TestAGuardedWriteLosingToClosureStillPreservesTheLateVerdict(t *testing.T) {
	st := &guardedStore{ledgerStore: &ledgerStore{fakeStore: &fakeStore{ok: true, run: runningRun()}}, cancelFirst: true}
	if _, err := submitAs(t, newAuthorityService(st), Submitter{}, domain.VerdictApproved, ""); err != nil {
		t.Fatalf("late verdict refused: %v", err)
	}
	if st.lateVerdictCalls != 1 {
		t.Fatalf("late verdict not preserved (calls=%d)", st.lateVerdictCalls)
	}
}
