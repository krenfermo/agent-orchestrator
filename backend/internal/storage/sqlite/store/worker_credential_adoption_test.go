package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// worker_credential_adoption_test.go -- the Spawn/bind window, against the real
// database.
//
// The adoption's safety is the guard in the SQL, not anything in Go: a row that
// has never been bound and has not been revoked, moved exactly once, by exactly
// one caller. A fake would test the fake, so these run on sqlite.

func TestAdoptableListsOnlyTheOrphansOfOneStep(t *testing.T) {
	st := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Now().UTC()
	owner := seedCredentialOwner(t, st)
	stepID := seedWorkerRun(t, st, "wf-1", domain.WorkflowStepRunning, now)

	orphan := workerCredential(owner, "cred-orphan", "hash-orphan", stepID, "at-1", now)
	if _, err := st.InsertAgentCredential(ctx, orphan); err != nil {
		t.Fatalf("insert orphan: %v", err)
	}
	// A bound credential for the same step: this launch completed, so there is
	// nothing to adopt about it.
	bound := workerCredential(owner, "cred-bound", "hash-bound", stepID, "at-2", now)
	bound.SessionID = "sess-live"
	if _, err := st.InsertAgentCredential(ctx, bound); err != nil {
		t.Fatalf("insert bound: %v", err)
	}
	// A revoked orphan: its launch was superseded, and a superseded launch must
	// never adopt its way back into authority.
	revoked := workerCredential(owner, "cred-revoked", "hash-revoked", stepID, "at-3", now)
	if _, err := st.InsertAgentCredential(ctx, revoked); err != nil {
		t.Fatalf("insert revoked: %v", err)
	}
	if _, err := st.RevokeAgentCredential(ctx, "cred-revoked", now); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// Another step's orphan, which this step's adoption must never see.
	otherStep := seedWorkerRun(t, st, "wf-2", domain.WorkflowStepRunning, now)
	elsewhere := workerCredential(owner, "cred-elsewhere", "hash-elsewhere", otherStep, "at-4", now)
	if _, err := st.InsertAgentCredential(ctx, elsewhere); err != nil {
		t.Fatalf("insert elsewhere: %v", err)
	}

	got, err := st.ListAdoptableWorkerAgentCredentials(ctx, stepID)
	if err != nil {
		t.Fatalf("ListAdoptableWorkerAgentCredentials: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("adoptable = %d rows, want exactly the one orphan: %+v", len(got), got)
	}
	if got[0].CredentialID != "cred-orphan" {
		t.Fatalf("adoptable = %s, want cred-orphan", got[0].CredentialID)
	}
	if got[0].WorkflowRunID != "wf-1" || got[0].RuntimeInstanceID != "at-1" {
		t.Fatalf("projection lost identity: %+v", got[0])
	}
}

// The adoption binds the session AND moves the attempt fence, in one write.
// Either alone leaves a credential the next request refuses.
func TestAdoptionBindsAndRefencesInOneWrite(t *testing.T) {
	st := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Now().UTC()
	owner := seedCredentialOwner(t, st)
	stepID := seedWorkerRun(t, st, "wf-1", domain.WorkflowStepRunning, now)
	if _, err := st.InsertAgentCredential(ctx, workerCredential(owner, "cred-1", "hash-1", stepID, "at-1", now)); err != nil {
		t.Fatalf("insert: %v", err)
	}

	adopted, err := st.AdoptWorkerAgentCredential(ctx, "cred-1", "sess-9", "at-2")
	if err != nil || !adopted {
		t.Fatalf("AdoptWorkerAgentCredential: adopted=%v err=%v", adopted, err)
	}
	got, found, err := st.GetAgentCredentialByTokenHash(ctx, "hash-1")
	if err != nil || !found {
		t.Fatalf("read back: found=%v err=%v", found, err)
	}
	if got.SessionID != "sess-9" {
		t.Fatalf("session = %q, want sess-9", got.SessionID)
	}
	if got.RuntimeInstanceID != "at-2" {
		t.Fatalf("attempt fence = %q, want at-2", got.RuntimeInstanceID)
	}

	// Once only, on any session: the guard is session_id = '', so a second
	// adoption matches nothing rather than re-pointing a live identity.
	again, err := st.AdoptWorkerAgentCredential(ctx, "cred-1", "sess-OTHER", "at-3")
	if err != nil {
		t.Fatalf("second adoption: %v", err)
	}
	if again {
		t.Fatal("a second adoption re-pointed a credential that was already speaking")
	}
	got, _, _ = st.GetAgentCredentialByTokenHash(ctx, "hash-1")
	if got.SessionID != "sess-9" || got.RuntimeInstanceID != "at-2" {
		t.Fatalf("the refused adoption still moved the row: session=%q attempt=%q", got.SessionID, got.RuntimeInstanceID)
	}
}

// A revoked credential can never be resurrected into a binding.
func TestAdoptionRefusesARevokedCredential(t *testing.T) {
	st := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Now().UTC()
	owner := seedCredentialOwner(t, st)
	stepID := seedWorkerRun(t, st, "wf-1", domain.WorkflowStepRunning, now)
	if _, err := st.InsertAgentCredential(ctx, workerCredential(owner, "cred-1", "hash-1", stepID, "at-1", now)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := st.RevokeAgentCredential(ctx, "cred-1", now); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	adopted, err := st.AdoptWorkerAgentCredential(ctx, "cred-1", "sess-9", "at-2")
	if err != nil {
		t.Fatalf("AdoptWorkerAgentCredential: %v", err)
	}
	if adopted {
		t.Fatal("a revoked credential was adopted back into service")
	}
}

// Concurrency: two recovery passes racing on one orphan. Exactly one may win,
// and the loser must be told it lost rather than believing it adopted.
func TestConcurrentAdoptionsProduceExactlyOneWinner(t *testing.T) {
	st := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Now().UTC()
	owner := seedCredentialOwner(t, st)
	stepID := seedWorkerRun(t, st, "wf-1", domain.WorkflowStepRunning, now)
	if _, err := st.InsertAgentCredential(ctx, workerCredential(owner, "cred-1", "hash-1", stepID, "at-1", now)); err != nil {
		t.Fatalf("insert: %v", err)
	}

	const passes = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
	)
	for i := 0; i < passes; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			adopted, err := st.AdoptWorkerAgentCredential(ctx, "cred-1", domain.SessionID("sess-9"), "at-2")
			if err != nil {
				t.Errorf("pass %d: %v", n, err)
				return
			}
			if adopted {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("%d passes reported adopting one credential, want exactly 1", winners)
	}
}

// The other half of the proof: a session that already speaks with a live worker
// credential is counted, so adoption can refuse to give one pane two
// identities. Revoked rows do not count -- they cannot speak.
func TestLiveWorkerCredentialsForSessionCountsOnlyLiveOnes(t *testing.T) {
	st := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Now().UTC()
	owner := seedCredentialOwner(t, st)
	stepID := seedWorkerRun(t, st, "wf-1", domain.WorkflowStepRunning, now)

	live := workerCredential(owner, "cred-live", "hash-live", stepID, "at-1", now)
	live.SessionID = "sess-9"
	if _, err := st.InsertAgentCredential(ctx, live); err != nil {
		t.Fatalf("insert live: %v", err)
	}
	dead := workerCredential(owner, "cred-dead", "hash-dead", stepID, "at-2", now)
	dead.SessionID = "sess-9"
	if _, err := st.InsertAgentCredential(ctx, dead); err != nil {
		t.Fatalf("insert dead: %v", err)
	}
	if _, err := st.RevokeAgentCredential(ctx, "cred-dead", now); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	n, err := st.CountLiveWorkerAgentCredentialsForSession(ctx, "sess-9")
	if err != nil {
		t.Fatalf("CountLiveWorkerAgentCredentialsForSession: %v", err)
	}
	if n != 1 {
		t.Fatalf("live worker credentials = %d, want 1 (the revoked one cannot speak)", n)
	}
	if n, err = st.CountLiveWorkerAgentCredentialsForSession(ctx, "sess-nobody"); err != nil || n != 0 {
		t.Fatalf("live for an unused session = %d (err=%v), want 0", n, err)
	}
}

// An adopted credential must be AUTHORIZED, which is the whole point: the
// binding and the fence together are what IsWorkerCredentialAuthorized checks.
func TestAnAdoptedCredentialIsAuthorized(t *testing.T) {
	st := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Now().UTC()
	owner := seedCredentialOwner(t, st)
	stepID := seedWorkerRun(t, st, "wf-1", domain.WorkflowStepRunning, now)
	if _, err := st.InsertAgentCredential(ctx, workerCredential(owner, "cred-1", "hash-1", stepID, "at-crashed", now)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// The recovery: a REAL session on the step, and a NEW attempt doing the
	// adopting. Both are what the authority check reads.
	session, err := st.CreateSession(ctx, sampleRecord("proj-1"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := st.UpdateWorkflowStepSession(ctx, stepID, string(session.ID), now); err != nil {
		t.Fatalf("UpdateWorkflowStepSession: %v", err)
	}
	if _, err := st.CreateWorkflowAttempt(ctx, "at-adopting", stepID, "claude-code", "opus", now); err != nil {
		t.Fatalf("CreateWorkflowAttempt: %v", err)
	}

	// Before adoption the worker provably cannot speak: bound to nothing.
	if ok, err := st.IsWorkerCredentialAuthorized(ctx, "cred-1"); err != nil || ok {
		t.Fatalf("an unbound credential was authorized: ok=%v err=%v", ok, err)
	}

	if adopted, err := st.AdoptWorkerAgentCredential(ctx, "cred-1", session.ID, "at-adopting"); err != nil || !adopted {
		t.Fatalf("adopt: adopted=%v err=%v", adopted, err)
	}
	ok, err := st.IsWorkerCredentialAuthorized(ctx, "cred-1")
	if err != nil {
		t.Fatalf("IsWorkerCredentialAuthorized: %v", err)
	}
	if !ok {
		t.Fatal("the adopted credential is still refused; the adoption did not restore the worker's ability to report")
	}
}
