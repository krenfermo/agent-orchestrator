package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// worker_credential_store_test.go — P5-A phase 2C, against the real database.
//
// The guarantees these exercise live in SQL, not in Go: a binding that can only
// happen once, and a lifetime derived from the work step rather than remembered.
// Testing them against a fake would test the fake, so they run on sqlite with
// the real guarded statements.

func workerCredential(owner domain.UserID, id, hash, stepID, attemptID string, now time.Time) domain.AgentCredential {
	return domain.AgentCredential{
		ID: id, TokenHash: hash, Role: domain.AgentRoleWorker, UserID: owner,
		ProjectID: "proj-1",
		// Unbound: the session this will speak for does not exist yet.
		SessionID:     "",
		WorkflowRunID: "wf-1", WorkflowStepID: stepID,
		RuntimeHandle: "workflow-worker-" + attemptID, RuntimeInstanceID: attemptID,
		Permissions: domain.AgentRoleCeiling(domain.AgentRoleWorker),
		CreatedAt:   now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now,
	}
}

// seedWorkerRun creates a run whose WORK step is in the given state, and
// returns that step's id. The state is what the credential's lifetime derives
// from, so it is the only interesting input.
func seedWorkerRun(t *testing.T, st *sqlite.Store, runID string, workState domain.WorkflowStepState, now time.Time) string {
	t.Helper()
	seedProject(t, st, "proj-1")
	steps := sampleWorkflowSteps(runID, now)
	var workStepID string
	for i := range steps {
		if steps[i].Kind == domain.WorkflowStepWork {
			steps[i].State = workState
			workStepID = steps[i].ID
		}
	}
	if _, _, err := st.CreateWorkflowRun(context.Background(), sampleWorkflowRun("proj-1", runID, now), steps); err != nil {
		t.Fatalf("CreateWorkflowRun: %v", err)
	}
	return workStepID
}

// The binding moves a credential from unbound to bound exactly once, and no
// second call — on any session — can re-point it.
func TestBindAgentCredentialSessionIsOnceOnly(t *testing.T) {
	st := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Now().UTC()
	owner := seedCredentialOwner(t, st)
	stepID := seedWorkerRun(t, st, "wf-1", domain.WorkflowStepRunning, now)

	if _, err := st.InsertAgentCredential(ctx, workerCredential(owner, "cred-1", "hash-1", stepID, "att-1", now)); err != nil {
		t.Fatalf("InsertAgentCredential: %v", err)
	}

	bound, err := st.BindAgentCredentialSession(ctx, "cred-1", "agent-orchestrator-59")
	if err != nil || !bound {
		t.Fatalf("first bind: bound=%v err=%v", bound, err)
	}
	bound, err = st.BindAgentCredentialSession(ctx, "cred-1", "agent-orchestrator-58")
	if err != nil {
		t.Fatalf("second bind errored: %v", err)
	}
	if bound {
		t.Fatal("a bound credential was re-bound to another session")
	}

	got, ok, err := st.GetAgentCredentialByTokenHash(ctx, "hash-1")
	if err != nil || !ok {
		t.Fatalf("read back: ok=%v err=%v", ok, err)
	}
	if got.SessionID != "agent-orchestrator-59" {
		t.Fatalf("session = %q; the first binding must stand", got.SessionID)
	}
}

// A revoked credential can never be resurrected into a binding.
func TestBindAgentCredentialSessionRefusesARevokedRow(t *testing.T) {
	st := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Now().UTC()
	owner := seedCredentialOwner(t, st)
	stepID := seedWorkerRun(t, st, "wf-1", domain.WorkflowStepRunning, now)

	if _, err := st.InsertAgentCredential(ctx, workerCredential(owner, "cred-1", "hash-1", stepID, "att-1", now)); err != nil {
		t.Fatalf("InsertAgentCredential: %v", err)
	}
	if _, err := st.RevokeAgentCredential(ctx, "cred-1", now); err != nil {
		t.Fatalf("RevokeAgentCredential: %v", err)
	}
	bound, err := st.BindAgentCredentialSession(ctx, "cred-1", "agent-orchestrator-59")
	if err != nil {
		t.Fatalf("bind after revoke errored: %v", err)
	}
	if bound {
		t.Fatal("a revoked credential was bound to a session")
	}
}

// CAS under real concurrency: several passes racing over one launch produce
// exactly one winner. This is the property the deferred binding rests on, and
// it is enforced by the statement rather than by the caller.
func TestBindAgentCredentialSessionHasOneWinnerUnderRace(t *testing.T) {
	st := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Now().UTC()
	owner := seedCredentialOwner(t, st)
	stepID := seedWorkerRun(t, st, "wf-1", domain.WorkflowStepRunning, now)

	if _, err := st.InsertAgentCredential(ctx, workerCredential(owner, "cred-1", "hash-1", stepID, "att-1", now)); err != nil {
		t.Fatalf("InsertAgentCredential: %v", err)
	}

	const racers = 8
	var wg sync.WaitGroup
	results := make([]bool, racers)
	errs := make([]error, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = st.BindAgentCredentialSession(ctx, "cred-1", "agent-orchestrator-59")
		}(i)
	}
	close(start)
	wg.Wait()

	wins := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("racer %d errored: %v", i, errs[i])
		}
		if results[i] {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("%d binds reported success, want exactly 1", wins)
	}
}

// The derived lifetime, against the real join: a worker credential lives while
// its work step runs and not one state longer.
func TestWorkerCredentialLifetimeFollowsItsStep(t *testing.T) {
	for _, tc := range []struct {
		state     domain.WorkflowStepState
		revocable bool
	}{
		{domain.WorkflowStepReady, false},
		{domain.WorkflowStepRunning, false},
		{domain.WorkflowStepWaiting, false},
		{domain.WorkflowStepCompleted, true},
		{domain.WorkflowStepFailed, true},
		{domain.WorkflowStepCancelled, true},
		// A step back at pending is not the launch this credential was minted
		// for, so the credential must not outlive it.
		{domain.WorkflowStepPending, true},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			st := sqlitetest.MustOpen(t)
			ctx := context.Background()
			now := time.Now().UTC()
			owner := seedCredentialOwner(t, st)
			stepID := seedWorkerRun(t, st, "wf-1", tc.state, now)

			if _, err := st.InsertAgentCredential(ctx, workerCredential(owner, "cred-1", "hash-1", stepID, "att-1", now)); err != nil {
				t.Fatalf("InsertAgentCredential: %v", err)
			}

			pending, err := st.ListRevocableWorkerAgentCredentials(ctx)
			if err != nil {
				t.Fatalf("ListRevocableWorkerAgentCredentials: %v", err)
			}
			if got := len(pending) == 1; got != tc.revocable {
				t.Fatalf("state %q: revocable=%v, want %v", tc.state, got, tc.revocable)
			}

			n, err := st.RevokeStaleWorkerAgentCredentials(ctx, now)
			if err != nil {
				t.Fatalf("RevokeStaleWorkerAgentCredentials: %v", err)
			}
			// The listing and the sweep ask the SAME predicate, so they cannot
			// disagree about what "finished" means.
			if (n == 1) != tc.revocable {
				t.Fatalf("state %q: sweep revoked %d, want revocable=%v", tc.state, n, tc.revocable)
			}
			// And the sweep is idempotent.
			again, err := st.RevokeStaleWorkerAgentCredentials(ctx, now)
			if err != nil {
				t.Fatalf("second sweep: %v", err)
			}
			if again != 0 {
				t.Fatalf("a second sweep revoked %d more", again)
			}
		})
	}
}

// A credential whose step row no longer exists is finished too: the predicate is
// NOT EXISTS, not a state comparison, so an orphan left by a deleted run is
// discharged on the next pass rather than living out its TTL.
func TestOrphanedWorkerCredentialIsSweptAfterRestart(t *testing.T) {
	st := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Now().UTC()
	owner := seedCredentialOwner(t, st)

	// A step id that names nothing — exactly what a credential stranded by a
	// daemon that died mid-launch looks like on the next boot.
	if _, err := st.InsertAgentCredential(ctx, workerCredential(owner, "cred-1", "hash-1", "wfs-gone", "att-1", now)); err != nil {
		t.Fatalf("InsertAgentCredential: %v", err)
	}
	pending, err := st.ListRevocableWorkerAgentCredentials(ctx)
	if err != nil {
		t.Fatalf("ListRevocableWorkerAgentCredentials: %v", err)
	}
	if len(pending) != 1 || pending[0].WorkflowStepID != "wfs-gone" {
		t.Fatalf("pending = %+v, want the orphan", pending)
	}
	if n, err := st.RevokeStaleWorkerAgentCredentials(ctx, now); err != nil || n != 1 {
		t.Fatalf("sweep: n=%d err=%v", n, err)
	}
	got, ok, err := st.GetAgentCredentialByTokenHash(ctx, "hash-1")
	if err != nil || !ok {
		t.Fatalf("read back: ok=%v err=%v", ok, err)
	}
	if got.Active(now) {
		t.Fatal("an orphaned credential is still active after the sweep")
	}
}

// Replacement: a new attempt on a still-running step ends its predecessors and
// leaves its own credential alone. The sweep cannot see this ending, which is
// why the statement exists.
func TestSupersededWorkerCredentialsAreRevokedByAttempt(t *testing.T) {
	st := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Now().UTC()
	owner := seedCredentialOwner(t, st)
	stepID := seedWorkerRun(t, st, "wf-1", domain.WorkflowStepRunning, now)

	for _, att := range []string{"att-1", "att-2", "att-3"} {
		if _, err := st.InsertAgentCredential(ctx, workerCredential(owner, "cred-"+att, "hash-"+att, stepID, att, now)); err != nil {
			t.Fatalf("InsertAgentCredential(%s): %v", att, err)
		}
	}
	// The step is still running, so the derived sweep would keep all three.
	if n, err := st.RevokeStaleWorkerAgentCredentials(ctx, now); err != nil || n != 0 {
		t.Fatalf("the sweep can see this ending after all: n=%d err=%v", n, err)
	}

	n, err := st.RevokeSupersededWorkerAgentCredentials(ctx, stepID, "att-3", now)
	if err != nil {
		t.Fatalf("RevokeSupersededWorkerAgentCredentials: %v", err)
	}
	if n != 2 {
		t.Fatalf("revoked %d predecessors, want 2", n)
	}
	for att, wantActive := range map[string]bool{"att-1": false, "att-2": false, "att-3": true} {
		got, ok, err := st.GetAgentCredentialByTokenHash(ctx, "hash-"+att)
		if err != nil || !ok {
			t.Fatalf("read back %s: ok=%v err=%v", att, ok, err)
		}
		if got.Active(now) != wantActive {
			t.Fatalf("%s active=%v, want %v", att, got.Active(now), wantActive)
		}
	}
}

// A reviewer credential is never touched by the worker sweep, and vice versa:
// the two obligations are separate and must stay so.
func TestTheWorkerSweepLeavesReviewerCredentialsAlone(t *testing.T) {
	st := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Now().UTC()
	owner := seedCredentialOwner(t, st)

	// A reviewer credential whose review run does not exist — revocable by the
	// REVIEW sweep, and none of the worker sweep's business.
	if _, err := st.InsertAgentCredential(ctx, credential(owner, "cred-rev", "hash-rev", "rr-1", now)); err != nil {
		t.Fatalf("InsertAgentCredential(reviewer): %v", err)
	}
	pending, err := st.ListRevocableWorkerAgentCredentials(ctx)
	if err != nil {
		t.Fatalf("ListRevocableWorkerAgentCredentials: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("the worker sweep claimed a reviewer credential: %+v", pending)
	}
	if n, err := st.RevokeStaleWorkerAgentCredentials(ctx, now); err != nil || n != 0 {
		t.Fatalf("the worker sweep revoked %d reviewer credentials (err=%v)", n, err)
	}
	got, ok, err := st.GetAgentCredentialByTokenHash(ctx, "hash-rev")
	if err != nil || !ok {
		t.Fatalf("read back: ok=%v err=%v", ok, err)
	}
	if !got.Active(now) {
		t.Fatal("the worker sweep ended a reviewer's credential")
	}
}
