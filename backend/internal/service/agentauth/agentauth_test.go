package agentauth_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	agentauth "github.com/aoagents/agent-orchestrator/backend/internal/service/agentauth"
)

type fakeStore struct {
	creds map[string]domain.AgentCredential // by token hash
	users map[domain.UserID]domain.User
	// runs is the durable review-run status a credential derives its life from.
	// A run this map does not name is a run that no longer exists, which the
	// real predicate also treats as closed.
	runs map[string]domain.ReviewRunStatus
	seq  int
	// listErr and revokeErr make a pass fail the way a locked database would,
	// so the recovery behaviour can be tested rather than argued about.
	listErr   error
	revokeErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		creds: map[string]domain.AgentCredential{},
		users: map[domain.UserID]domain.User{
			"user-owner": {ID: "user-owner", Role: domain.UserRoleOwner, Status: domain.UserStatusActive},
		},
		runs: map[string]domain.ReviewRunStatus{},
	}
}

// closed mirrors the SQL predicate exactly: only a review run AO still
// considers RUNNING keeps its reviewer able to speak.
func (f *fakeStore) closed(reviewRunID string) bool {
	return f.runs[reviewRunID] != domain.ReviewRunRunning
}

func (f *fakeStore) ListRevocableAgentCredentials(_ context.Context) ([]domain.RevocableAgentCredential, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []domain.RevocableAgentCredential
	for _, c := range f.creds {
		if c.RevokedAt == nil && c.ReviewRunID != "" && f.closed(c.ReviewRunID) {
			out = append(out, domain.RevocableAgentCredential{
				CredentialID: c.ID, ReviewRunID: c.ReviewRunID, RuntimeHandle: c.RuntimeHandle,
			})
		}
	}
	return out, nil
}

func (f *fakeStore) RevokeClosedReviewRunAgentCredentials(_ context.Context, at time.Time) (int64, error) {
	if f.revokeErr != nil {
		return 0, f.revokeErr
	}
	var n int64
	for hash, c := range f.creds {
		if c.RevokedAt == nil && c.ReviewRunID != "" && f.closed(c.ReviewRunID) {
			revoked := at
			c.RevokedAt = &revoked
			f.creds[hash] = c
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) RevokeAgentCredentialsForClosedReviewRun(_ context.Context, id string, at time.Time) (int64, error) {
	if f.revokeErr != nil {
		return 0, f.revokeErr
	}
	if !f.closed(id) {
		return 0, nil
	}
	var n int64
	for hash, c := range f.creds {
		if c.ReviewRunID == id && c.RevokedAt == nil {
			revoked := at
			c.RevokedAt = &revoked
			f.creds[hash] = c
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) InsertAgentCredential(_ context.Context, cred domain.AgentCredential) (domain.AgentCredential, error) {
	f.seq++
	f.creds[cred.TokenHash] = cred
	return cred, nil
}

func (f *fakeStore) GetAgentCredentialByTokenHash(_ context.Context, hash string) (domain.AgentCredential, bool, error) {
	c, ok := f.creds[hash]
	return c, ok, nil
}

func (f *fakeStore) ListAgentCredentialsForReviewRun(_ context.Context, id string) ([]domain.AgentCredential, error) {
	var out []domain.AgentCredential
	for _, c := range f.creds {
		if c.ReviewRunID == id {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeStore) TouchAgentCredentialLastSeen(_ context.Context, id string, at time.Time) (bool, error) {
	for hash, c := range f.creds {
		if c.ID == id && c.RevokedAt == nil {
			c.LastSeenAt = at
			f.creds[hash] = c
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStore) RevokeAgentCredentialsForReviewRun(_ context.Context, id string, at time.Time) (int64, error) {
	var n int64
	for hash, c := range f.creds {
		if c.ReviewRunID == id && c.RevokedAt == nil {
			revoked := at
			c.RevokedAt = &revoked
			f.creds[hash] = c
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) RevokeAgentCredentialsForSession(_ context.Context, id domain.SessionID, at time.Time) (int64, error) {
	var n int64
	for hash, c := range f.creds {
		if c.SessionID == id && c.RevokedAt == nil {
			revoked := at
			c.RevokedAt = &revoked
			f.creds[hash] = c
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) GetUserByID(_ context.Context, id domain.UserID) (domain.User, bool, error) {
	u, ok := f.users[id]
	return u, ok, nil
}

func issueInput() agentauth.IssueInput {
	return agentauth.IssueInput{
		Role:           domain.AgentRoleReviewer,
		UserID:         "user-owner",
		ProjectID:      "proj-1",
		SessionID:      "agent-orchestrator-59",
		WorkflowRunID:  "wf-98ab416c",
		WorkflowStepID: "wfs-04b67615",
		ReviewRunID:    "bf660d26",
		RuntimeHandle:  "workflow-review-bf660d26",
		Generation:     1,
	}
}

func newService(store *fakeStore, now time.Time) *agentauth.Service {
	return agentauth.New(store, func() time.Time { return now }, time.Hour)
}

// The credential exists so a reviewer can record its verdict, and its shape is
// the whole point: bound to one launch, capped by the role, and acting FOR the
// run's owner rather than AS them.
func TestIssuedCredentialIsBoundAndCapped(t *testing.T) {
	store := newFakeStore()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	svc := newService(store, now)

	issued, err := svc.Issue(context.Background(), issueInput())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if issued.Token == "" {
		t.Fatalf("no raw token was returned; the agent has nothing to present")
	}
	// The raw token is never stored -- only its hash, exactly as a browser
	// session already does.
	if stored, ok := store.creds[agentauth.HashToken(issued.Token)]; !ok {
		t.Fatalf("the credential was not stored under the token hash")
	} else if stored.TokenHash == issued.Token {
		t.Fatalf("the raw token was persisted")
	}
	cred := issued.Credential
	if cred.SessionID != "agent-orchestrator-59" || cred.WorkflowRunID != "wf-98ab416c" || cred.ReviewRunID != "bf660d26" {
		t.Fatalf("credential is not bound to its launch: %+v", cred)
	}
	if !cred.ExpiresAt.After(now) {
		t.Fatalf("credential does not expire after now")
	}
	for _, p := range cred.Permissions {
		if domain.ScopeOf(p) != domain.AuthzScopeProject {
			t.Fatalf("an issued credential holds the non-project permission %q", p)
		}
	}
}

// Issue refuses every incomplete binding. A credential with no session, no
// project or no account behind it would be a credential with no ceiling.
func TestIssueRefusesAnUnboundCredential(t *testing.T) {
	store := newFakeStore()
	svc := newService(store, time.Now())
	for name, mutate := range map[string]func(*agentauth.IssueInput){
		"no role":     func(in *agentauth.IssueInput) { in.Role = "" },
		"bad role":    func(in *agentauth.IssueInput) { in.Role = "archivist" },
		"no account":  func(in *agentauth.IssueInput) { in.UserID = "" },
		"no project":  func(in *agentauth.IssueInput) { in.ProjectID = "" },
		"no session":  func(in *agentauth.IssueInput) { in.SessionID = "" },
		"ghost owner": func(in *agentauth.IssueInput) { in.UserID = "user-who-does-not-exist" },
	} {
		t.Run(name, func(t *testing.T) {
			in := issueInput()
			mutate(&in)
			if _, err := svc.Issue(context.Background(), in); err == nil {
				t.Fatalf("Issue accepted %s", name)
			}
		})
	}
}

// A disabled account takes its agents with it, and it does so IMMEDIATELY --
// at the next request, not at the credential's expiry.
func TestDisablingAnAccountStopsItsAgentsAtOnce(t *testing.T) {
	store := newFakeStore()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	svc := newService(store, now)
	issued, err := svc.Issue(context.Background(), issueInput())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := svc.ResolveAgentPrincipal(context.Background(), issued.Token); err != nil {
		t.Fatalf("a fresh credential did not resolve: %v", err)
	}
	u := store.users["user-owner"]
	u.Status = domain.UserStatusDisabled
	store.users["user-owner"] = u
	if _, err := svc.ResolveAgentPrincipal(context.Background(), issued.Token); err == nil {
		t.Fatalf("a disabled account's agent still resolved")
	}
	// And it cannot be re-issued either.
	if _, err := svc.Issue(context.Background(), issueInput()); err == nil {
		t.Fatalf("a credential was minted for a disabled account")
	}
}

// Resolution is refused for every credential that is finished, and refused the
// same way: an unknown token, an expired one and a revoked one are one answer.
func TestResolveRefusesEveryFinishedCredential(t *testing.T) {
	store := newFakeStore()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	svc := newService(store, now)
	issued, err := svc.Issue(context.Background(), issueInput())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if _, err := svc.ResolveAgentPrincipal(context.Background(), ""); err == nil {
		t.Fatalf("an empty token resolved")
	}
	if _, err := svc.ResolveAgentPrincipal(context.Background(), "not-a-real-token"); err == nil {
		t.Fatalf("an unknown token resolved")
	}
	// Expired: the same service, one tick past the TTL.
	expired := agentauth.New(store, func() time.Time { return now.Add(2 * time.Hour) }, time.Hour)
	if _, err := expired.ResolveAgentPrincipal(context.Background(), issued.Token); err == nil {
		t.Fatalf("an expired credential resolved")
	}
	// Revoked: the review run it was minted for has been closed out.
	if n, err := svc.RevokeForReviewRun(context.Background(), "bf660d26"); err != nil || n != 1 {
		t.Fatalf("RevokeForReviewRun = %d, %v; want 1, nil", n, err)
	}
	if _, err := svc.ResolveAgentPrincipal(context.Background(), issued.Token); err == nil {
		t.Fatalf("a revoked credential resolved")
	}
}

// The resolved principal is an AGENT: it carries the launch's bounded authority,
// and it is not a browser session under another name.
func TestResolvedPrincipalCarriesTheAgentAuthority(t *testing.T) {
	store := newFakeStore()
	svc := newService(store, time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC))
	issued, err := svc.Issue(context.Background(), issueInput())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	p, err := svc.ResolveAgentPrincipal(context.Background(), issued.Token)
	if err != nil {
		t.Fatalf("ResolveAgentPrincipal: %v", err)
	}
	if !p.IsAgent() || p.AuthMethod != domain.AuthMethodAgent {
		t.Fatalf("principal is not an agent: %+v", p)
	}
	if p.SessionID != "" {
		t.Fatalf("an agent principal claims a browser auth session (%q)", p.SessionID)
	}
	if p.User.ID != "user-owner" {
		t.Fatalf("the agent does not act for the run's owner: %q", p.User.ID)
	}
	if !p.Agent.MayReachSession("agent-orchestrator-59") || p.Agent.MayReachSession("agent-orchestrator-58") {
		t.Fatalf("the resolved authority is not bound to its launch: %+v", *p.Agent)
	}
}

// EverIssuedForReviewRun is the evidence the ambiguous-review recovery reads,
// so it has to answer about the LAUNCH RECORD -- revoked and expired credentials
// included. A revocation must not make a reviewer look like one that never had
// an identity, or closing a review out would license replacing its successor.
func TestEverIssuedSurvivesRevocation(t *testing.T) {
	store := newFakeStore()
	svc := newService(store, time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC))

	if ever, err := svc.EverIssuedForReviewRun(context.Background(), "bf660d26"); err != nil || ever {
		t.Fatalf("EverIssued before any launch = %v, %v; want false, nil", ever, err)
	}
	if _, err := svc.Issue(context.Background(), issueInput()); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := svc.RevokeForReviewRun(context.Background(), "bf660d26"); err != nil {
		t.Fatalf("RevokeForReviewRun: %v", err)
	}
	if ever, err := svc.EverIssuedForReviewRun(context.Background(), "bf660d26"); err != nil || !ever {
		t.Fatalf("EverIssued after revocation = %v, %v; want true, nil", ever, err)
	}
	if ever, err := svc.EverIssuedForReviewRun(context.Background(), ""); err != nil || ever {
		t.Fatalf("EverIssued for no run = %v, %v; want false, nil", ever, err)
	}
}
