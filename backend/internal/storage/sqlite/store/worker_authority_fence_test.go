package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// worker_authority_fence_test.go — P5-A phase 2C's central fence, against the
// real database.
//
// The fence answers "is this credential still the authorized one" from durable
// rows, per request. It is what removes the interval that revocation alone
// cannot: revocation is something that must have HAPPENED, and an authorization
// taken before it happened would be taken on a launch that is over.
//
// Every case below is one way authority ends, asserted through the real SQL.

// fenceFixture seeds a run whose work step is dispatched into a session with
// one attempt, and a bound worker credential for that attempt.
type fenceFixture struct {
	st        *sqlite.Store
	stepID    string
	sessionID domain.SessionID
	credID    string
	now       time.Time
}

func newFenceFixture(t *testing.T) *fenceFixture {
	t.Helper()
	st := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Now().UTC()
	owner := seedCredentialOwner(t, st)
	stepID := seedWorkerRun(t, st, "wf-1", domain.WorkflowStepRunning, now)

	session := seedFenceSession(t, st, stepID, now)
	attemptID := seedFenceAttempt(t, st, stepID, 1, now)

	cred := workerCredential(owner, "cred-1", "hash-1", stepID, attemptID, now)
	cred.SessionID = session
	if _, err := st.InsertAgentCredential(ctx, cred); err != nil {
		t.Fatalf("InsertAgentCredential: %v", err)
	}
	return &fenceFixture{st: st, stepID: stepID, sessionID: session, credID: "cred-1", now: now}
}

func (f *fenceFixture) authorized(t *testing.T) bool {
	t.Helper()
	ok, err := f.st.IsWorkerCredentialAuthorized(context.Background(), f.credID)
	if err != nil {
		t.Fatalf("IsWorkerCredentialAuthorized: %v", err)
	}
	return ok
}

// A worker on a live step, in the session that step is dispatched into, on the
// step's latest attempt, is authorized. The control for everything below.
func TestFenceAuthorizesTheCurrentWorker(t *testing.T) {
	fx := newFenceFixture(t)
	if !fx.authorized(t) {
		t.Fatal("the current worker was refused; it could not do its job")
	}
}

// Every ending, refused with NO window -- before any sweep or eager close has
// run. The credential row is untouched in each case: the fence denies on the
// derived facts alone.
func TestFenceRefusesEveryEndedAttemptBeforeAnySweep(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(t *testing.T, fx *fenceFixture)
	}{
		{"the step completed", func(t *testing.T, fx *fenceFixture) {
			setFenceStepState(t, fx, domain.WorkflowStepCompleted)
		}},
		{"the step failed", func(t *testing.T, fx *fenceFixture) {
			setFenceStepState(t, fx, domain.WorkflowStepFailed)
		}},
		{"the run was cancelled", func(t *testing.T, fx *fenceFixture) {
			setFenceStepState(t, fx, domain.WorkflowStepCancelled)
		}},
		{"a replacement attempt was launched", func(t *testing.T, fx *fenceFixture) {
			// The step is STILL RUNNING, which is exactly why the sweep cannot
			// see this one and the fence must.
			seedFenceAttempt(t, fx.st, fx.stepID, 2, fx.now.Add(time.Minute))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFenceFixture(t)
			if !fx.authorized(t) {
				t.Fatal("fixture did not start authorized")
			}
			tc.end(t, fx)
			if fx.authorized(t) {
				t.Fatal("a credential whose launch is over is still authorized; the window is open")
			}
			// And the row itself has NOT been revoked -- proving the refusal
			// came from the fence rather than from revocation having run.
			cred, ok, err := fx.st.GetAgentCredentialByTokenHash(context.Background(), "hash-1")
			if err != nil || !ok {
				t.Fatalf("read back: ok=%v err=%v", ok, err)
			}
			if cred.RevokedAt != nil {
				t.Fatal("the credential was revoked, so this test proved revocation rather than the fence")
			}
		})
	}
}

// A revoked credential is refused too: the fence and revocation are defence in
// depth, not alternatives.
func TestFenceRefusesARevokedCredential(t *testing.T) {
	fx := newFenceFixture(t)
	if _, err := fx.st.RevokeAgentCredential(context.Background(), fx.credID, fx.now); err != nil {
		t.Fatalf("RevokeAgentCredential: %v", err)
	}
	if fx.authorized(t) {
		t.Fatal("a revoked credential is still authorized")
	}
}

// An UNBOUND credential -- one whose launch died between minting and binding --
// is refused. It reaches nothing through the bindings either; this is the
// second lock.
func TestFenceRefusesAnUnboundCredential(t *testing.T) {
	st := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Now().UTC()
	owner := seedCredentialOwner(t, st)
	stepID := seedWorkerRun(t, st, "wf-1", domain.WorkflowStepRunning, now)
	seedFenceSession(t, st, stepID, now)
	attemptID := seedFenceAttempt(t, st, stepID, 1, now)

	// SessionID left empty: never bound.
	if _, err := st.InsertAgentCredential(ctx, workerCredential(owner, "cred-1", "hash-1", stepID, attemptID, now)); err != nil {
		t.Fatalf("InsertAgentCredential: %v", err)
	}
	ok, err := st.IsWorkerCredentialAuthorized(ctx, "cred-1")
	if err != nil {
		t.Fatalf("IsWorkerCredentialAuthorized: %v", err)
	}
	if ok {
		t.Fatal("an unbound credential is authorized")
	}
}

// A credential with no attempt recorded is refused: the fence cannot prove it
// belongs to the current launch, and an authority AO cannot prove is not one.
func TestFenceRefusesACredentialWithNoAttempt(t *testing.T) {
	st := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Now().UTC()
	owner := seedCredentialOwner(t, st)
	stepID := seedWorkerRun(t, st, "wf-1", domain.WorkflowStepRunning, now)
	session := seedFenceSession(t, st, stepID, now)
	seedFenceAttempt(t, st, stepID, 1, now)

	cred := workerCredential(owner, "cred-1", "hash-1", stepID, "", now)
	cred.SessionID = session
	cred.RuntimeInstanceID = ""
	if _, err := st.InsertAgentCredential(ctx, cred); err != nil {
		t.Fatalf("InsertAgentCredential: %v", err)
	}
	ok, err := st.IsWorkerCredentialAuthorized(ctx, "cred-1")
	if err != nil {
		t.Fatalf("IsWorkerCredentialAuthorized: %v", err)
	}
	if ok {
		t.Fatal("a credential that cannot name its launch is authorized")
	}
}

// An unknown credential id is refused rather than erroring: a request carrying
// one has no authority to evaluate.
func TestFenceRefusesAnUnknownCredential(t *testing.T) {
	fx := newFenceFixture(t)
	ok, err := fx.st.IsWorkerCredentialAuthorized(context.Background(), "cred-from-another-life")
	if err != nil {
		t.Fatalf("IsWorkerCredentialAuthorized: %v", err)
	}
	if ok {
		t.Fatal("an unknown credential is authorized")
	}
}

// RESTART AND RECOVERY. A step reopened from failed into a NEW generation must
// not hand the old attempt's credential its authority back when the step
// becomes live again -- which is precisely what a liveness-only check would do.
func TestFenceRefusesAnOldAttemptAfterAReopen(t *testing.T) {
	fx := newFenceFixture(t)

	// The launch fails; the step goes terminal. Nothing has swept yet.
	setFenceStepState(t, fx, domain.WorkflowStepFailed)
	if fx.authorized(t) {
		t.Fatal("a failed step's credential is authorized")
	}

	// Recovery reopens it, and dispatch mints a new attempt.
	reopenFence(t, fx)
	seedFenceAttempt(t, fx.st, fx.stepID, 2, fx.now.Add(time.Minute))

	if fx.authorized(t) {
		t.Fatal("a reopened step handed the OLD attempt's credential its authority back")
	}
}

// --- fixture helpers, all through the store's own API -----------------------

// seedFenceSession creates a REAL session row and dispatches the step into it.
// workflow_steps.session_id is a foreign key, so the row has to exist -- which
// is also what makes the "the step moved to a different session" case below
// exercise a real move rather than a dangling id.
func seedFenceSession(t *testing.T, st *sqlite.Store, stepID string, now time.Time) domain.SessionID {
	t.Helper()
	rec, err := st.CreateSession(context.Background(), sampleRecord("proj-1"))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := st.UpdateWorkflowStepSession(context.Background(), stepID, string(rec.ID), now); err != nil {
		t.Fatalf("UpdateWorkflowStepSession: %v", err)
	}
	return rec.ID
}

// newFenceSession creates a second real session, for the "the step moved"
// case.
func newFenceSession(t *testing.T, st *sqlite.Store) domain.SessionID {
	t.Helper()
	rec, err := st.CreateSession(context.Background(), sampleRecord("proj-1"))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return rec.ID
}

func seedFenceAttempt(t *testing.T, st *sqlite.Store, stepID string, n int, now time.Time) string {
	t.Helper()
	id := "wfa-" + stepID + "-" + string(rune('0'+n))
	if _, err := st.CreateWorkflowAttempt(context.Background(), id, stepID, "claude-code", "sonnet", now); err != nil {
		t.Fatalf("CreateWorkflowAttempt(%d): %v", n, err)
	}
	return id
}

func setFenceStepState(t *testing.T, fx *fenceFixture, next domain.WorkflowStepState) {
	t.Helper()
	steps, err := fx.st.ListWorkflowSteps(context.Background(), "wf-1")
	if err != nil {
		t.Fatalf("ListWorkflowSteps: %v", err)
	}
	var current domain.WorkflowStepState
	for _, s := range steps {
		if s.ID == fx.stepID {
			current = s.State
		}
	}
	if current == next {
		return
	}
	// Through the store's own compare-and-swap, so the fixture can only reach
	// states the lifecycle actually allows.
	ok, err := fx.st.UpdateWorkflowStepState(context.Background(), fx.stepID, current, next, fx.now)
	if err != nil {
		t.Fatalf("set step state %q -> %q: %v", current, next, err)
	}
	if !ok {
		t.Fatalf("the lifecycle refuses %q -> %q; the fixture cannot reach that state", current, next)
	}
}

// reopenFence puts a failed step back to `ready`, the way recovery does.
func reopenFence(t *testing.T, fx *fenceFixture) {
	t.Helper()
	ok, err := fx.st.ReopenFailedWorkflowStep(context.Background(), fx.stepID, fx.now)
	if err != nil || !ok {
		t.Fatalf("ReopenFailedWorkflowStep: ok=%v err=%v", ok, err)
	}
}

// A work step's session is WRITE-ONCE: UpdateWorkflowStepSession is guarded on
// `session_id IS NULL`, so a step can never be re-pointed at another session.
//
// That is worth asserting rather than assuming, because it is what makes the
// ATTEMPT the right discriminator for the fence. If a step's session could
// move, a credential bound to the old one would need the session-equality
// clause to catch it; since it cannot, that clause is defence in depth and the
// attempt is what actually separates one generation from the next.
func TestAWorkStepSessionCannotMove(t *testing.T) {
	fx := newFenceFixture(t)
	other := newFenceSession(t, fx.st)

	moved, err := fx.st.UpdateWorkflowStepSession(context.Background(), fx.stepID, string(other), fx.now)
	if err != nil {
		t.Fatalf("UpdateWorkflowStepSession: %v", err)
	}
	if moved {
		t.Fatal("a work step's session was re-pointed; the fence's generation discriminator would be wrong")
	}
	if !fx.authorized(t) {
		t.Fatal("the current worker lost authority over a session that did not move")
	}
}
