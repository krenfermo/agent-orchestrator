package agentauth_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/agentcred"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	agentauth "github.com/aoagents/agent-orchestrator/backend/internal/service/agentauth"
)

var revocationNow = time.Date(2026, 9, 7, 15, 40, 0, 0, time.UTC)

// issueForRun mints a credential for one review run and leaves that run in the
// given durable status, returning the raw token so a test can check whether the
// credential still opens anything.
func issueForRun(t *testing.T, store *fakeStore, svc *agentauth.Service, runID string, status domain.ReviewRunStatus) string {
	t.Helper()
	in := issueInput()
	in.ReviewRunID = runID
	in.RuntimeHandle = "workflow-review-" + runID
	store.runs[runID] = status
	issued, err := svc.Issue(context.Background(), in)
	if err != nil {
		t.Fatalf("Issue for %s: %v", runID, err)
	}
	return issued.Token
}

// newReconciler builds the sweep with a data dir it may delete credential files
// from, and returns that dir.
func newReconciler(t *testing.T, svc *agentauth.Service) (*agentauth.Reconciler, string) {
	t.Helper()
	dir := t.TempDir()
	return agentauth.NewReconciler(svc, agentauth.ReconcilerConfig{DataDir: dir}), dir
}

// writeCredentialFile puts a credential file where the launcher would have,
// so a test can check the sweep takes the file back along with the row.
func writeCredentialFile(t *testing.T, dir, handle, token string) string {
	t.Helper()
	path := agentcred.Path(dir, handle)
	if err := agentcred.Write(path, agentcred.File{
		Token: token, CredentialID: "agc-x", Role: string(domain.AgentRoleReviewer),
		ExpiresAt: revocationNow.Add(time.Hour),
	}); err != nil {
		t.Fatalf("write credential file: %v", err)
	}
	return path
}

// THE REGRESSION, at the service boundary. A reviewer that finished normally
// left a live credential behind because every revocation path AO had was a
// failure path. Submitting the verdict is what ends the authority, and this is
// the call the submit path now makes.
func TestASucceededReviewEndsItsCredential(t *testing.T) {
	store := newFakeStore()
	svc := newService(store, revocationNow)
	token := issueForRun(t, store, svc, "run-done", domain.ReviewRunComplete)
	rec, dir := newReconciler(t, svc)
	path := writeCredentialFile(t, dir, "workflow-review-run-done", token)

	if err := rec.CloseReviewRun(context.Background(), "run-done"); err != nil {
		t.Fatalf("CloseReviewRun: %v", err)
	}
	if _, err := svc.ResolveAgentPrincipal(context.Background(), token); err == nil {
		t.Fatal("a revoked credential still resolves a principal")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the credential file outlived the authority it carried (%v)", err)
	}
}

// The eager call sits AT the durable transition, so it has to be safe to make
// while the reviewer is still working. The guard is in the store, not in the
// caller: a review that is still running keeps its identity, and the call that
// arrived early simply does nothing.
func TestClosingAStillRunningReviewTakesNothingAway(t *testing.T) {
	store := newFakeStore()
	svc := newService(store, revocationNow)
	token := issueForRun(t, store, svc, "run-live", domain.ReviewRunRunning)
	rec, dir := newReconciler(t, svc)
	path := writeCredentialFile(t, dir, "workflow-review-run-live", token)

	if err := rec.CloseReviewRun(context.Background(), "run-live"); err != nil {
		t.Fatalf("CloseReviewRun: %v", err)
	}
	if _, err := svc.ResolveAgentPrincipal(context.Background(), token); err != nil {
		t.Fatalf("a reviewer that is still working lost its identity: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a working reviewer lost the file it reads its credential from: %v", err)
	}
}

// Cancellation and success can arrive at the same row from two directions --
// AO terminating the pane while the reviewer is submitting is the race that
// already destroyed one approval. Whichever lands first, the credential ends up
// revoked exactly once and at one time, and the loser reports no error.
func TestSuccessAndCancellationConvergeOnOneRevocation(t *testing.T) {
	store := newFakeStore()
	svc := newService(store, revocationNow)
	token := issueForRun(t, store, svc, "run-race", domain.ReviewRunComplete)
	rec, _ := newReconciler(t, svc)
	ctx := context.Background()

	// The cancellation path, which revokes unconditionally because AO killed
	// the reviewer, gets there first.
	if _, err := svc.RevokeForReviewRun(ctx, "run-race"); err != nil {
		t.Fatalf("RevokeForReviewRun: %v", err)
	}
	first := revokedAt(t, store, token)
	// ...and the submit path follows, on the same row.
	if err := rec.CloseReviewRun(ctx, "run-race"); err != nil {
		t.Fatalf("CloseReviewRun after a cancellation: %v", err)
	}
	if got := revokedAt(t, store, token); !got.Equal(first) {
		t.Fatalf("the second path moved the revocation time from %v to %v", first, got)
	}
	if _, err := svc.ResolveAgentPrincipal(ctx, token); err == nil {
		t.Fatal("the credential survived both revocations")
	}
}

// Restart recovery. A daemon that died between the verdict and the cleanup --
// or an installation upgraded onto this build with credentials already stranded
// -- has nothing to replay: the obligation is re-derived from the same rows, so
// the first pass of the next boot discharges it.
func TestARestartRevokesWhatThePreviousProcessNeverDid(t *testing.T) {
	store := newFakeStore()
	svc := newService(store, revocationNow)
	stranded := issueForRun(t, store, svc, "run-stranded", domain.ReviewRunComplete)
	working := issueForRun(t, store, svc, "run-working", domain.ReviewRunRunning)
	rec, dir := newReconciler(t, svc)
	strandedFile := writeCredentialFile(t, dir, "workflow-review-run-stranded", stranded)
	workingFile := writeCredentialFile(t, dir, "workflow-review-run-working", working)
	ctx := context.Background()

	revoked, err := rec.ReconcileOnce(ctx)
	if err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if len(revoked) != 1 || revoked[0].ReviewRunID != "run-stranded" {
		t.Fatalf("the boot sweep revoked %+v; want only the stranded credential", revoked)
	}
	if _, err := svc.ResolveAgentPrincipal(ctx, stranded); err == nil {
		t.Fatal("the stranded credential survived the restart sweep")
	}
	if _, err := svc.ResolveAgentPrincipal(ctx, working); err != nil {
		t.Fatalf("the sweep revoked a reviewer that is still working: %v", err)
	}
	if _, err := os.Stat(strandedFile); !os.IsNotExist(err) {
		t.Fatalf("the stranded credential file survived (%v)", err)
	}
	if _, err := os.Stat(workingFile); err != nil {
		t.Fatalf("the working reviewer lost its credential file: %v", err)
	}

	// A second pass has nothing left to do, and says so.
	again, err := rec.ReconcileOnce(ctx)
	if err != nil || len(again) != 0 {
		t.Fatalf("a repeated sweep revoked %+v (%v); want nothing", again, err)
	}
}

// A revocation that cannot be written keeps its obligation. It is not lost, not
// retried in a tight loop, and not turned into a failure of the work it was
// cleaning up after: it is simply still derivable, and the next pass discharges
// it.
func TestAFailedRevocationSurvivesAsAnObligation(t *testing.T) {
	store := newFakeStore()
	svc := newService(store, revocationNow)
	token := issueForRun(t, store, svc, "run-flaky", domain.ReviewRunComplete)
	rec, _ := newReconciler(t, svc)
	ctx := context.Background()

	store.revokeErr = errors.New("database is locked")
	if err := rec.CloseReviewRun(ctx, "run-flaky"); err == nil {
		t.Fatal("a failed revocation was reported as success")
	}
	if _, err := rec.ReconcileOnce(ctx); err == nil {
		t.Fatal("a failed sweep was reported as success")
	}
	// The obligation is still there to be found, unchanged.
	pending, err := svc.ListPendingRevocations(ctx)
	if err != nil || len(pending) != 1 || pending[0].ReviewRunID != "run-flaky" {
		t.Fatalf("the obligation did not survive the failure: %+v (%v)", pending, err)
	}

	store.revokeErr = nil
	if _, err := rec.ReconcileOnce(ctx); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if _, err := svc.ResolveAgentPrincipal(ctx, token); err == nil {
		t.Fatal("the retry did not revoke the credential")
	}
}

// A revoked credential is refused the same way an unknown one is, and the
// refusal says nothing a holder could learn from -- no token, no hash, no
// reason, no file path.
func TestARevokedCredentialIsRefusedWithoutLeakingAnything(t *testing.T) {
	store := newFakeStore()
	svc := newService(store, revocationNow)
	token := issueForRun(t, store, svc, "run-refused", domain.ReviewRunComplete)
	rec, _ := newReconciler(t, svc)
	ctx := context.Background()
	if err := rec.CloseReviewRun(ctx, "run-refused"); err != nil {
		t.Fatalf("CloseReviewRun: %v", err)
	}

	_, revokedErr := svc.ResolveAgentPrincipal(ctx, token)
	if revokedErr == nil {
		t.Fatal("a revoked credential still authenticates")
	}
	_, unknownErr := svc.ResolveAgentPrincipal(ctx, "a-token-nobody-ever-issued")
	if unknownErr == nil {
		t.Fatal("an unknown credential authenticates")
	}
	if revokedErr.Error() != unknownErr.Error() {
		t.Fatalf("revoked and unknown answer differently:\n revoked: %s\n unknown: %s", revokedErr, unknownErr)
	}
	for _, secret := range []string{token, agentauth.HashToken(token)} {
		if strings.Contains(revokedErr.Error(), secret) {
			t.Fatalf("the refusal echoed a secret back")
		}
	}
}

// The revocation report an operator reads carries identifiers only. It exists
// to correlate a revocation with a review run; it must never be a second place
// a token can be found.
func TestTheRevocationReportCarriesNoSecrets(t *testing.T) {
	store := newFakeStore()
	svc := newService(store, revocationNow)
	token := issueForRun(t, store, svc, "run-report", domain.ReviewRunComplete)
	rec, _ := newReconciler(t, svc)

	revoked, err := rec.ReconcileOnce(context.Background())
	if err != nil || len(revoked) != 1 {
		t.Fatalf("ReconcileOnce = %+v, %v", revoked, err)
	}
	rendered := revoked[0].CredentialID + " " + revoked[0].ReviewRunID + " " + revoked[0].RuntimeHandle
	for _, secret := range []string{token, agentauth.HashToken(token)} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("the revocation report carried a secret")
		}
	}
	if revoked[0].ReviewRunID != "run-report" || revoked[0].RuntimeHandle != "workflow-review-run-report" {
		t.Fatalf("the report cannot be correlated with its launch: %+v", revoked[0])
	}
}

// A reconciler with nothing wired starts nothing and refuses nothing, so a
// build with no identity layer needs no guard at its call site.
func TestAnUnwiredReconcilerIsInert(t *testing.T) {
	var rec *agentauth.Reconciler
	ctx := context.Background()
	if err := rec.CloseReviewRun(ctx, "run-anything"); err != nil {
		t.Fatalf("CloseReviewRun on a nil reconciler: %v", err)
	}
	if revoked, err := rec.ReconcileOnce(ctx); err != nil || revoked != nil {
		t.Fatalf("ReconcileOnce on a nil reconciler = %+v, %v", revoked, err)
	}
	done := rec.Start(ctx)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a nil reconciler did not close its done channel")
	}
}

// revokedAt reads back when a token's credential was revoked, failing the test
// if it is still live.
func revokedAt(t *testing.T, store *fakeStore, token string) time.Time {
	t.Helper()
	cred, ok := store.creds[agentauth.HashToken(token)]
	if !ok {
		t.Fatalf("the credential is not in the store")
	}
	if cred.RevokedAt == nil {
		t.Fatalf("the credential is still live")
	}
	return *cred.RevokedAt
}
