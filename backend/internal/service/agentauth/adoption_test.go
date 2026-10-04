package agentauth_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentauth"
)

// adoption_test.go -- the Spawn/bind window, and the proof adoption requires.
//
// The defect: a daemon dies after Spawn and before the bind that follows it. The
// worker is real, running, and holding a credential file; its row names no
// session, so MayReachSession refuses every session route. Recovery adopted the
// SESSION and left the credential alone, so that worker could never report on
// its own work -- for the rest of the run, with nothing that was going to fix it.
//
// These tests pin both halves of the rule: adopt when the launch can be proven,
// refuse when it cannot, and never guess in between.

// adoptionFixture is a live worker credential minted for one step and never
// bound -- exactly the row a crash between Spawn and bind leaves behind.
func adoptionFixture(t *testing.T) (*agentauth.Service, *fakeStore, domain.AgentCredential) {
	t.Helper()
	store := newFakeStore()
	svc := agentauth.New(store, nil, 0)
	issued, err := svc.Issue(context.Background(), agentauth.IssueInput{
		Role:      domain.AgentRoleWorker,
		UserID:    "user-owner",
		ProjectID: "p1",
		// Deliberately empty: this is what the launcher mints, because the
		// session it will speak for does not exist yet.
		SessionID:         "",
		WorkflowRunID:     "wf-1",
		WorkflowStepID:    "wfs-work",
		RuntimeHandle:     "workflow-worker-at-1",
		RuntimeInstanceID: "at-1",
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if issued.Credential.SessionID != "" {
		t.Fatal("the fixture minted a BOUND credential; it must be unbound to model the window")
	}
	return svc, store, issued.Credential
}

func TestAdoptionBindsTheOrphanedCredentialAndMovesItsAttemptFence(t *testing.T) {
	svc, store, cred := adoptionFixture(t)

	res, err := svc.AdoptWorkerSession(context.Background(), agentauth.WorkerAdoptionInput{
		StepID: "wfs-work", RunID: "wf-1", ProjectID: "p1",
		SessionID: "sess-9", AttemptID: "at-2",
	})
	if err != nil {
		t.Fatalf("AdoptWorkerSession: %v", err)
	}
	if res.Outcome != agentauth.WorkerAdoptionAdopted {
		t.Fatalf("outcome = %s (%s), want adopted", res.Outcome, res.Detail)
	}
	if res.CredentialID != cred.ID {
		t.Fatalf("adopted %s, want %s", res.CredentialID, cred.ID)
	}

	got := store.credentialByID(cred.ID)
	if got.SessionID != "sess-9" {
		t.Fatalf("session = %q, want sess-9: the credential is still bound to nothing", got.SessionID)
	}
	// The fence has to move too. Without it the credential binds and is then
	// refused by the very next authorization check, because that check requires
	// a worker credential to name its step's NEWEST attempt -- and an adoption
	// opens a new one.
	if got.RuntimeInstanceID != "at-2" {
		t.Fatalf("attempt fence = %q, want the adopting attempt at-2", got.RuntimeInstanceID)
	}
}

// The ordinary case, and it must stay silent: a launch that completed bound its
// own credential, so recovery finds nothing to adopt and reports so.
func TestAdoptionIsANoOpWhenNothingWasOrphaned(t *testing.T) {
	svc, _, cred := adoptionFixture(t)
	ctx := context.Background()
	if bound, err := svc.BindSession(ctx, cred.ID, "sess-1"); err != nil || !bound {
		t.Fatalf("BindSession: bound=%v err=%v", bound, err)
	}

	res, err := svc.AdoptWorkerSession(ctx, agentauth.WorkerAdoptionInput{
		StepID: "wfs-work", RunID: "wf-1", ProjectID: "p1", SessionID: "sess-1", AttemptID: "at-2",
	})
	if err != nil {
		t.Fatalf("AdoptWorkerSession: %v", err)
	}
	if res.Outcome != agentauth.WorkerAdoptionNothingToDo {
		t.Fatalf("outcome = %s, want nothing_to_adopt", res.Outcome)
	}
}

// Two orphans is two panes AO cannot tell apart, and nothing on either row
// distinguishes them. Refusing is the whole point: binding one at random gives
// a token to a worker that may not be holding it.
func TestAdoptionRefusesWhenTwoLaunchesLeftAnOrphan(t *testing.T) {
	svc, _, _ := adoptionFixture(t)
	ctx := context.Background()
	if _, err := svc.Issue(ctx, agentauth.IssueInput{
		Role: domain.AgentRoleWorker, UserID: "user-owner", ProjectID: "p1",
		WorkflowRunID: "wf-1", WorkflowStepID: "wfs-work",
		RuntimeHandle: "workflow-worker-at-9", RuntimeInstanceID: "at-9",
	}); err != nil {
		t.Fatalf("issue second orphan: %v", err)
	}

	res, err := svc.AdoptWorkerSession(ctx, agentauth.WorkerAdoptionInput{
		StepID: "wfs-work", RunID: "wf-1", ProjectID: "p1", SessionID: "sess-9", AttemptID: "at-2",
	})
	if err != nil {
		t.Fatalf("AdoptWorkerSession: %v", err)
	}
	if res.Outcome != agentauth.WorkerAdoptionUnprovable {
		t.Fatalf("outcome = %s, want unprovable", res.Outcome)
	}
	if !strings.Contains(res.Detail, "wfs-work") {
		t.Fatalf("detail = %q, want the step named so a person can look at it", res.Detail)
	}
}

// A superseded launch can never adopt its way back into authority. The
// replacement revoked its predecessor as it minted its own, so the predecessor
// is not adoptable -- which is the generation fence doing its job without a
// second mechanism being invented for it.
func TestARevokedPredecessorIsNotAdoptable(t *testing.T) {
	svc, store, cred := adoptionFixture(t)
	ctx := context.Background()
	// The replacement launch: a new attempt, which revokes every other
	// attempt's credential for this step.
	if _, err := svc.RevokeSupersededWorkers(ctx, "wfs-work", "at-2"); err != nil {
		t.Fatalf("RevokeSupersededWorkers: %v", err)
	}

	res, err := svc.AdoptWorkerSession(ctx, agentauth.WorkerAdoptionInput{
		StepID: "wfs-work", RunID: "wf-1", ProjectID: "p1", SessionID: "sess-9", AttemptID: "at-3",
	})
	if err != nil {
		t.Fatalf("AdoptWorkerSession: %v", err)
	}
	if res.Outcome != agentauth.WorkerAdoptionNothingToDo {
		t.Fatalf("outcome = %s, want nothing_to_adopt for a revoked predecessor", res.Outcome)
	}
	if got := store.credentialByID(cred.ID); got.SessionID != "" {
		t.Fatalf("a revoked predecessor was bound to %q", got.SessionID)
	}
}

// A session that already speaks with a live worker credential must not be given
// a second one: two identities for one pane is the state adoption exists to
// avoid, not to create.
func TestAdoptionRefusesToGiveOnePaneTwoIdentities(t *testing.T) {
	svc, _, _ := adoptionFixture(t)
	ctx := context.Background()
	// A different step's worker, already bound to the session in question.
	other, err := svc.Issue(ctx, agentauth.IssueInput{
		Role: domain.AgentRoleWorker, UserID: "user-owner", ProjectID: "p1",
		WorkflowRunID: "wf-1", WorkflowStepID: "wfs-other",
		RuntimeHandle: "workflow-worker-at-7", RuntimeInstanceID: "at-7",
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if bound, err := svc.BindSession(ctx, other.Credential.ID, "sess-9"); err != nil || !bound {
		t.Fatalf("BindSession: bound=%v err=%v", bound, err)
	}

	res, err := svc.AdoptWorkerSession(ctx, agentauth.WorkerAdoptionInput{
		StepID: "wfs-work", RunID: "wf-1", ProjectID: "p1", SessionID: "sess-9", AttemptID: "at-2",
	})
	if err != nil {
		t.Fatalf("AdoptWorkerSession: %v", err)
	}
	if res.Outcome != agentauth.WorkerAdoptionUnprovable {
		t.Fatalf("outcome = %s, want unprovable", res.Outcome)
	}
}

// A candidate minted under different work is refused rather than adopted, even
// though it is the only orphan for the step id it was asked about.
func TestAdoptionRefusesACredentialFromAnotherRun(t *testing.T) {
	svc, _, _ := adoptionFixture(t)

	res, err := svc.AdoptWorkerSession(context.Background(), agentauth.WorkerAdoptionInput{
		StepID: "wfs-work", RunID: "wf-OTHER", ProjectID: "p1", SessionID: "sess-9", AttemptID: "at-2",
	})
	if err != nil {
		t.Fatalf("AdoptWorkerSession: %v", err)
	}
	if res.Outcome != agentauth.WorkerAdoptionUnprovable {
		t.Fatalf("outcome = %s, want unprovable for a credential minted under another run", res.Outcome)
	}
}

// Adopting with no attempt to fence to would bind a credential the next
// authorization check refuses -- worse than not adopting, because it looks like
// it worked.
func TestAdoptionRefusesWithoutAnAttemptToFenceTo(t *testing.T) {
	svc, _, _ := adoptionFixture(t)

	res, err := svc.AdoptWorkerSession(context.Background(), agentauth.WorkerAdoptionInput{
		StepID: "wfs-work", RunID: "wf-1", ProjectID: "p1", SessionID: "sess-9",
	})
	if err != nil {
		t.Fatalf("AdoptWorkerSession: %v", err)
	}
	if res.Outcome != agentauth.WorkerAdoptionUnprovable {
		t.Fatalf("outcome = %s, want unprovable", res.Outcome)
	}
}

// Idempotence: a second adoption pass over an already-adopted launch changes
// nothing and reports the ordinary answer, so repeated recovery is safe.
func TestAdoptionIsIdempotent(t *testing.T) {
	svc, store, cred := adoptionFixture(t)
	ctx := context.Background()
	in := agentauth.WorkerAdoptionInput{
		StepID: "wfs-work", RunID: "wf-1", ProjectID: "p1", SessionID: "sess-9", AttemptID: "at-2",
	}
	if res, err := svc.AdoptWorkerSession(ctx, in); err != nil || res.Outcome != agentauth.WorkerAdoptionAdopted {
		t.Fatalf("first adoption: outcome=%v err=%v", res.Outcome, err)
	}
	res, err := svc.AdoptWorkerSession(ctx, in)
	if err != nil {
		t.Fatalf("second adoption: %v", err)
	}
	if res.Outcome != agentauth.WorkerAdoptionNothingToDo {
		t.Fatalf("second adoption outcome = %s, want nothing_to_adopt", res.Outcome)
	}
	if got := store.credentialByID(cred.ID); got.SessionID != "sess-9" || got.RuntimeInstanceID != "at-2" {
		t.Fatalf("the second pass moved the credential: session=%q attempt=%q", got.SessionID, got.RuntimeInstanceID)
	}
}
