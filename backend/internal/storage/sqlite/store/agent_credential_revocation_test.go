package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	sqlite "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// seedReviewRun makes one review run durable in the state a test needs it in,
// and returns the session it belongs to so a credential can be bound to it.
func seedReviewRun(t *testing.T, s *sqlite.Store, runID string, status domain.ReviewRunStatus) domain.SessionID {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	seedProject(t, s, "proj-cred")
	rec, err := s.CreateSession(ctx, sampleRecord("proj-cred"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := s.UpsertReview(ctx, domain.Review{
		ID: "rev-" + runID, SessionID: rec.ID, ProjectID: rec.ProjectID,
		Harness: domain.ReviewerClaudeCode, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("upsert review: %v", err)
	}
	if err := s.InsertReviewRun(ctx, domain.ReviewRun{
		ID: runID, ReviewID: "rev-" + runID, SessionID: rec.ID, Harness: domain.ReviewerClaudeCode,
		PRURL: "https://example/pr/" + runID, TargetSHA: "sha-" + runID,
		Status: domain.ReviewRunRunning, Verdict: domain.VerdictNone, CreatedAt: now,
	}); err != nil {
		t.Fatalf("insert review run: %v", err)
	}
	if status != domain.ReviewRunRunning {
		if ok, err := s.UpdateReviewRunResult(ctx, runID, status, domain.VerdictNone, "", "", false); err != nil || !ok {
			t.Fatalf("move review run to %s: %v (ok=%v)", status, err, ok)
		}
	}
	return rec.ID
}

func liveCredentialIDs(t *testing.T, s *sqlite.Store, runID string) []string {
	t.Helper()
	creds, err := s.ListAgentCredentialsForReviewRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("list agent credentials: %v", err)
	}
	var live []string
	for _, c := range creds {
		if c.RevokedAt == nil {
			live = append(live, c.ID)
		}
	}
	return live
}

// THE REGRESSION. A reviewer that simply finished -- submitted its verdict and
// exited -- passed through none of the failure paths that took a credential
// back, so its identity stayed live for the rest of its 72-hour TTL over a
// review that had already concluded. On the base commit nothing revoked it;
// here the run reaching a terminal status is itself what ends the credential.
func TestACompletedReviewRunRevokesItsAgentCredential(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	owner := seedCredentialOwner(t, s)
	now := time.Now().UTC().Truncate(time.Second)
	seedReviewRun(t, s, "run-complete", domain.ReviewRunComplete)

	if _, err := s.InsertAgentCredential(ctx, credential(owner, "agc-1", "hash-1", "run-complete", now)); err != nil {
		t.Fatalf("InsertAgentCredential: %v", err)
	}
	pending, err := s.ListRevocableAgentCredentials(ctx)
	if err != nil {
		t.Fatalf("ListRevocableAgentCredentials: %v", err)
	}
	if len(pending) != 1 || pending[0].CredentialID != "agc-1" ||
		pending[0].ReviewRunID != "run-complete" ||
		pending[0].RuntimeHandle != "workflow-review-run-complete" {
		t.Fatalf("the finished review did not surface its live credential: %+v", pending)
	}
	n, err := s.RevokeClosedReviewRunAgentCredentials(ctx, now)
	if err != nil || n != 1 {
		t.Fatalf("RevokeClosedReviewRunAgentCredentials = %d, %v; want 1, nil", n, err)
	}
	if live := liveCredentialIDs(t, s, "run-complete"); len(live) != 0 {
		t.Fatalf("a completed review left a live credential: %v", live)
	}
	// The launch record itself must survive. The ambiguous-review recovery reads
	// it as evidence that a reviewer was once able to answer.
	all, err := s.ListAgentCredentialsForReviewRun(ctx, "run-complete")
	if err != nil || len(all) != 1 {
		t.Fatalf("revocation erased the launch record: %d rows, %v", len(all), err)
	}
}

// The reviewer must keep its identity for exactly as long as AO is still
// listening. Revoking a running review is the failure the credential exists to
// prevent -- a real review that cannot be recorded -- so the predicate refuses
// to anticipate a closure, however sure anything else may be.
func TestARunningReviewKeepsItsAgentCredential(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	owner := seedCredentialOwner(t, s)
	now := time.Now().UTC().Truncate(time.Second)
	seedReviewRun(t, s, "run-running", domain.ReviewRunRunning)

	if _, err := s.InsertAgentCredential(ctx, credential(owner, "agc-1", "hash-1", "run-running", now)); err != nil {
		t.Fatalf("InsertAgentCredential: %v", err)
	}
	pending, err := s.ListRevocableAgentCredentials(ctx)
	if err != nil {
		t.Fatalf("ListRevocableAgentCredentials: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("a running review was treated as finished: %+v", pending)
	}
	if n, err := s.RevokeClosedReviewRunAgentCredentials(ctx, now); err != nil || n != 0 {
		t.Fatalf("the sweep revoked a live reviewer: %d, %v", n, err)
	}
	if n, err := s.RevokeAgentCredentialsForClosedReviewRun(ctx, "run-running", now); err != nil || n != 0 {
		t.Fatalf("the scoped revocation revoked a live reviewer: %d, %v", n, err)
	}
	if live := liveCredentialIDs(t, s, "run-running"); len(live) != 1 {
		t.Fatalf("the running reviewer lost its identity: %v", live)
	}
}

// Failure, cancellation and delivery are all the same fact to a credential:
// this review run is no longer running, so the reviewer that held it has
// nothing left it may say.
func TestEveryEndingOfAReviewRunEndsItsCredential(t *testing.T) {
	for _, status := range []domain.ReviewRunStatus{
		domain.ReviewRunComplete, domain.ReviewRunFailed, domain.ReviewRunCancelled,
	} {
		t.Run(string(status), func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()
			owner := seedCredentialOwner(t, s)
			now := time.Now().UTC().Truncate(time.Second)
			seedReviewRun(t, s, "run-x", status)
			if _, err := s.InsertAgentCredential(ctx, credential(owner, "agc-1", "hash-1", "run-x", now)); err != nil {
				t.Fatalf("InsertAgentCredential: %v", err)
			}
			if n, err := s.RevokeAgentCredentialsForClosedReviewRun(ctx, "run-x", now); err != nil || n != 1 {
				t.Fatalf("a %s review run did not end its credential: %d, %v", status, n, err)
			}
			if live := liveCredentialIDs(t, s, "run-x"); len(live) != 0 {
				t.Fatalf("a %s review run left a live credential: %v", status, live)
			}
		})
	}
}

// A reviewer that was REPLACED loses its identity to the same rule as one that
// finished: the replacement dispatch closes the predecessor run out, and a
// closed run is a closed credential. The replacement keeps its own -- revocation
// is per review run, so one generation can never take another generation down
// with it, in either direction.
func TestAReplacedReviewerLosesOnlyItsOwnGenerationsCredential(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	owner := seedCredentialOwner(t, s)
	now := time.Now().UTC().Truncate(time.Second)
	seedReviewRun(t, s, "run-gen1", domain.ReviewRunCancelled)
	seedReviewRun(t, s, "run-gen2", domain.ReviewRunRunning)
	if _, err := s.MarkReviewRunSupersededBy(ctx, "run-gen1", "run-gen2"); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	gen1 := credential(owner, "agc-gen1", "hash-gen1", "run-gen1", now)
	gen2 := credential(owner, "agc-gen2", "hash-gen2", "run-gen2", now)
	gen2.Generation = 2
	for _, c := range []domain.AgentCredential{gen1, gen2} {
		if _, err := s.InsertAgentCredential(ctx, c); err != nil {
			t.Fatalf("InsertAgentCredential %s: %v", c.ID, err)
		}
	}

	if n, err := s.RevokeClosedReviewRunAgentCredentials(ctx, now); err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v; want exactly the superseded generation", n, err)
	}
	if live := liveCredentialIDs(t, s, "run-gen1"); len(live) != 0 {
		t.Fatalf("the replaced reviewer kept its credential: %v", live)
	}
	if live := liveCredentialIDs(t, s, "run-gen2"); len(live) != 1 {
		t.Fatalf("the replacement lost its credential to its predecessor: %v", live)
	}
}

// Revocation is a one-way write and repeating it changes nothing: a retry after
// a transient failure, a duplicate event, and a sweep racing the eager path all
// converge on the same row with the same revocation time.
func TestRevokingAnAlreadyRevokedCredentialIsANoOp(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	owner := seedCredentialOwner(t, s)
	now := time.Now().UTC().Truncate(time.Second)
	seedReviewRun(t, s, "run-idem", domain.ReviewRunComplete)
	if _, err := s.InsertAgentCredential(ctx, credential(owner, "agc-1", "hash-1", "run-idem", now)); err != nil {
		t.Fatalf("InsertAgentCredential: %v", err)
	}

	if n, err := s.RevokeAgentCredentialsForClosedReviewRun(ctx, "run-idem", now); err != nil || n != 1 {
		t.Fatalf("first revocation = %d, %v", n, err)
	}
	later := now.Add(time.Hour)
	if n, err := s.RevokeAgentCredentialsForClosedReviewRun(ctx, "run-idem", later); err != nil || n != 0 {
		t.Fatalf("second revocation = %d, %v; want 0 rows and no error", n, err)
	}
	if n, err := s.RevokeClosedReviewRunAgentCredentials(ctx, later); err != nil || n != 0 {
		t.Fatalf("sweep over an already-revoked credential = %d, %v", n, err)
	}
	creds, err := s.ListAgentCredentialsForReviewRun(ctx, "run-idem")
	if err != nil || len(creds) != 1 {
		t.Fatalf("list: %d rows, %v", len(creds), err)
	}
	if creds[0].RevokedAt == nil || !creds[0].RevokedAt.Equal(now) {
		t.Fatalf("a retry moved the revocation time: %v", creds[0].RevokedAt)
	}
	if pending, err := s.ListRevocableAgentCredentials(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("a revoked credential is still pending revocation: %+v (%v)", pending, err)
	}
}

// A credential naming a review run that no longer exists has no authority to
// derive from, so it counts as closed. The predicate is written as "only a
// RUNNING run keeps a credential alive" rather than "a terminal run ends one"
// precisely so that absence is covered without a second rule.
func TestACredentialWhoseReviewRunIsGoneIsRevocable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	owner := seedCredentialOwner(t, s)
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := s.InsertAgentCredential(ctx, credential(owner, "agc-1", "hash-1", "run-that-never-existed", now)); err != nil {
		t.Fatalf("InsertAgentCredential: %v", err)
	}
	if n, err := s.RevokeClosedReviewRunAgentCredentials(ctx, now); err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v; want the orphaned credential revoked", n, err)
	}
	if live := liveCredentialIDs(t, s, "run-that-never-existed"); len(live) != 0 {
		t.Fatalf("an orphaned credential stayed live: %v", live)
	}
}
