// Package agentauth mints and resolves the credential an AO-LAUNCHED AGENT uses
// to talk to the daemon's own API.
//
// Why it exists. Under AO_AUTH_MODE=oidc a request resolves its identity from
// the ao_session cookie, and an agent AO started itself -- a reviewer pane, a
// worker pane -- holds no cookie and cannot obtain one: the OIDC flow needs a
// browser and a person. Every permission-gated route therefore answered the
// agent with 401 NOT_AUTHENTICATED, including the one route a reviewer exists to
// call. A reviewer would finish, try to record its verdict, be refused, and idle
// forever; thirty minutes later the review-staleness threshold parked the run on
// review_state_ambiguous, and AO reported that it could not prove what the
// review had concluded -- about a review it had refused to listen to.
//
// What this is NOT:
//
//   - not an exemption. reviews/submit stays behind the same authorization gate
//     as every other session-scoped route; the agent simply arrives at it with
//     an identity instead of without one.
//   - not the operator's credential. An agent presenting a person's session
//     would inherit that person's authority over every project in every
//     organization, which is a much larger grant than "record this verdict".
//   - not a new authorization model. The credential names the account it acts
//     FOR, and authorization grants the INTERSECTION of that account's rights
//     and the agent's own bounded grant -- so RBAC, project grants and tenancy
//     all still decide, and the agent can only ever do less than its owner.
//
// The credential is bound to one project, one session, one workflow run, one
// step and one runtime incarnation, capped by the agent's role, expiring, and
// revoked when the work it was minted for is closed out.
package agentauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// DefaultTTL is how long a freshly minted agent credential stays valid.
//
// It is generous because the cost of getting it wrong is asymmetric: a
// credential that expires under a long-running reviewer recreates exactly the
// failure this package exists to remove, while one that outlives its work is
// revoked the moment that work is closed out anyway (RevokeForReviewRun) and is
// bounded to one session and one run in the meantime.
const DefaultTTL = 72 * time.Hour

// Store is the durable surface this service needs.
type Store interface {
	InsertAgentCredential(ctx context.Context, cred domain.AgentCredential) (domain.AgentCredential, error)
	GetAgentCredentialByTokenHash(ctx context.Context, tokenHash string) (domain.AgentCredential, bool, error)
	ListAgentCredentialsForReviewRun(ctx context.Context, reviewRunID string) ([]domain.AgentCredential, error)
	TouchAgentCredentialLastSeen(ctx context.Context, id string, at time.Time) (bool, error)
	RevokeAgentCredentialsForReviewRun(ctx context.Context, reviewRunID string, at time.Time) (int64, error)
	RevokeAgentCredentialsForClosedReviewRun(ctx context.Context, reviewRunID string, at time.Time) (int64, error)
	RevokeAgentCredentialsForSession(ctx context.Context, sessionID domain.SessionID, at time.Time) (int64, error)
	// P5-A phase 2C: the worker half.
	BindAgentCredentialSession(ctx context.Context, credentialID string, sessionID domain.SessionID) (bool, error)
	RevokeAgentCredential(ctx context.Context, credentialID string, at time.Time) (int64, error)
	RevokeSupersededWorkerAgentCredentials(ctx context.Context, stepID, keepAttemptID string, at time.Time) (int64, error)
	ListRevocableWorkerAgentCredentials(ctx context.Context) ([]domain.RevocableAgentCredential, error)
	RevokeStaleWorkerAgentCredentials(ctx context.Context, at time.Time) (int64, error)
	IsWorkerCredentialAuthorized(ctx context.Context, credentialID string) (bool, error)
	ListRevocableAgentCredentials(ctx context.Context) ([]domain.RevocableAgentCredential, error)
	RevokeClosedReviewRunAgentCredentials(ctx context.Context, at time.Time) (int64, error)
	GetUserByID(ctx context.Context, id domain.UserID) (domain.User, bool, error)
}

// Service is the default implementation.
type Service struct {
	store Store
	now   func() time.Time
	ttl   time.Duration
}

// New builds a Service. now defaults to time.Now and ttl to DefaultTTL.
func New(store Store, now func() time.Time, ttl time.Duration) *Service {
	if now == nil {
		now = time.Now
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Service{store: store, now: now, ttl: ttl}
}

// IssueInput describes the launch a credential is being minted for. Every field
// but Permissions is a binding the resolved credential will be confined to.
type IssueInput struct {
	Role domain.AgentRole
	// UserID is the account the agent acts for -- the workflow run's owner.
	// Required: an agent with no account behind it has nothing to intersect
	// against, and "no account" must not mean "unbounded".
	UserID    domain.UserID
	ProjectID domain.ProjectID
	// SessionID is the one session the agent may reach. For a reviewer this is
	// the WORKER's session, because that is the session its review_run belongs
	// to and therefore the one `ao review submit` addresses.
	SessionID         domain.SessionID
	WorkflowRunID     string
	WorkflowStepID    string
	ReviewRunID       string
	RuntimeHandle     string
	RuntimeInstanceID string
	Generation        int64
	// Permissions asks for LESS than the role's ceiling. Empty means the whole
	// ceiling, which is what every AO launch asks for.
	Permissions []domain.Permission
}

// Issued is a minted credential plus the raw token, which exists only in this
// return value: it is never stored and never logged.
type Issued struct {
	Token      string
	Credential domain.AgentCredential
}

// Issue mints a credential for one agent launch.
func (s *Service) Issue(ctx context.Context, in IssueInput) (Issued, error) {
	if !domain.ValidAgentRole(in.Role) {
		return Issued{}, fmt.Errorf("agent credential: unknown role %q", in.Role)
	}
	if strings.TrimSpace(string(in.UserID)) == "" {
		return Issued{}, fmt.Errorf("agent credential: an owning account is required")
	}
	if strings.TrimSpace(string(in.ProjectID)) == "" {
		return Issued{}, fmt.Errorf("agent credential: a project binding is required")
	}
	// A session binding is required for every role EXCEPT a worker's, and the
	// exception is an ordering fact rather than a relaxation.
	//
	// A reviewer is bound to the worker's session, which already exists when it
	// launches. A worker's session is created BY the spawn that must already
	// carry its credential in the environment, so at this moment there is no
	// session to name. It is therefore minted UNBOUND and bound exactly once,
	// by BindSession, when the launch reports which session it produced.
	//
	// An unbound credential is inert, not permissive: AgentAuthority.
	// MayReachSession refuses every session route while SessionID is empty, so
	// the window between minting and binding grants nothing at all.
	if strings.TrimSpace(string(in.SessionID)) == "" && in.Role != domain.AgentRoleWorker {
		return Issued{}, fmt.Errorf("agent credential: a session binding is required")
	}
	// The account has to exist and be active AT ISSUE TIME. Minting a
	// credential for a disabled account would create an authority that outlives
	// the decision to disable it; the resolver re-checks this on every request
	// as well, so a later disable also takes effect immediately.
	u, ok, err := s.store.GetUserByID(ctx, in.UserID)
	if err != nil {
		return Issued{}, fmt.Errorf("agent credential: look up owning account: %w", err)
	}
	if !ok || u.Status != domain.UserStatusActive {
		return Issued{}, fmt.Errorf("agent credential: owning account %s is not active", in.UserID)
	}

	raw, err := randomToken()
	if err != nil {
		return Issued{}, fmt.Errorf("agent credential: generate token: %w", err)
	}
	now := s.now().UTC()
	cred := domain.AgentCredential{
		ID:                "agc-" + uuid.NewString(),
		TokenHash:         HashToken(raw),
		Role:              in.Role,
		UserID:            in.UserID,
		ProjectID:         in.ProjectID,
		SessionID:         in.SessionID,
		WorkflowRunID:     in.WorkflowRunID,
		WorkflowStepID:    in.WorkflowStepID,
		ReviewRunID:       in.ReviewRunID,
		RuntimeHandle:     in.RuntimeHandle,
		RuntimeInstanceID: in.RuntimeInstanceID,
		Generation:        in.Generation,
		Permissions:       domain.CapAgentPermissions(in.Role, in.Permissions),
		CreatedAt:         now,
		ExpiresAt:         now.Add(s.ttl),
		LastSeenAt:        now,
	}
	stored, err := s.store.InsertAgentCredential(ctx, cred)
	if err != nil {
		return Issued{}, err
	}
	return Issued{Token: raw, Credential: stored}, nil
}

// ResolveAgentPrincipal turns a raw agent token into a principal, or returns a
// structured error the transport renders as 401.
//
// Every failure answers the same way a browser session's does: the caller is
// told the credential is not usable, never which of the reasons applied to a
// token they do not hold.
func (s *Service) ResolveAgentPrincipal(ctx context.Context, rawToken string) (domain.Principal, error) {
	notFound := apierr.Unauthorized("NOT_AUTHENTICATED", "agent credential not found, revoked, or expired")
	if strings.TrimSpace(rawToken) == "" {
		return domain.Principal{}, notFound
	}
	cred, ok, err := s.store.GetAgentCredentialByTokenHash(ctx, HashToken(rawToken))
	if err != nil {
		return domain.Principal{}, fmt.Errorf("lookup agent credential: %w", err)
	}
	now := s.now().UTC()
	if !ok || !cred.Active(now) {
		return domain.Principal{}, notFound
	}
	// The account is re-read on every request, not cached into the credential:
	// disabling somebody must stop their agents too, and it must stop them now
	// rather than at the credential's expiry.
	u, ok, err := s.store.GetUserByID(ctx, cred.UserID)
	if err != nil {
		return domain.Principal{}, fmt.Errorf("lookup agent credential account: %w", err)
	}
	if !ok || u.Status != domain.UserStatusActive {
		return domain.Principal{}, notFound
	}
	// Best-effort; a failure here must never fail resolution.
	_, _ = s.store.TouchAgentCredentialLastSeen(ctx, cred.ID, now)

	authority := cred.Authority()
	return domain.Principal{
		User:       u,
		AuthMethod: domain.AuthMethodAgent,
		Agent:      &authority,
	}, nil
}

// RevokeForReviewRun revokes every credential minted for one review run. It is
// called wherever a review run is closed out, so that a reviewer AO has stopped
// listening to also stops being able to speak.
func (s *Service) RevokeForReviewRun(ctx context.Context, reviewRunID string) (int64, error) {
	if strings.TrimSpace(reviewRunID) == "" {
		return 0, nil
	}
	return s.store.RevokeAgentCredentialsForReviewRun(ctx, reviewRunID, s.now().UTC())
}

// RevokeForClosedReviewRun ends one reviewer's authority at the moment that
// authority actually ends: when the review run it was minted for has durably
// stopped running.
//
// It is the SUCCESS path, and its absence is the defect this exists to close.
// RevokeForReviewRun above was only ever reached from failure paths -- a launch
// that never produced a pane, a credential file that could not be written, a
// reviewer AO proved it owned and terminated. A reviewer that simply finished,
// recorded its verdict and exited passed through none of them, so its
// credential stayed live for the rest of its 72-hour TTL over a review that had
// already concluded (agc-dfef17e6 and agc-f1850962 on wf-98ab416c).
//
// The guard is the whole design. Revocation follows the run into closure and
// never anticipates it: a reviewer whose run is still running keeps its
// identity however long the review takes, because taking it away early is
// exactly the failure agent credentials exist to prevent -- a real review that
// cannot be recorded. So this is safe to call from anywhere, including beside a
// live reviewer, and calling it early simply does nothing.
//
// Idempotent by construction: the statement only ever moves a NULL revoked_at
// to a time, so a retry, a duplicate event, or a race against a cancellation
// that already revoked converges on the same row and reports nothing revoked
// the second time.
//
// The credentials it took back come back with it, so the caller can also remove
// the files they were handed over in.
func (s *Service) RevokeForClosedReviewRun(ctx context.Context, reviewRunID string) ([]domain.RevocableAgentCredential, error) {
	if strings.TrimSpace(reviewRunID) == "" {
		return nil, nil
	}
	// Read through the SAME predicate the write applies, so what is reported as
	// revoked and what is actually revoked can never be two different answers.
	// The list is the installation-wide one because there is only ever one
	// predicate; it is a handful of rows at most, and normally zero.
	pending, err := s.ListPendingRevocations(ctx)
	if err != nil {
		return nil, err
	}
	matched := make([]domain.RevocableAgentCredential, 0, 1)
	for _, cred := range pending {
		if cred.ReviewRunID == reviewRunID {
			matched = append(matched, cred)
		}
	}
	if len(matched) == 0 {
		return nil, nil
	}
	if _, err := s.store.RevokeAgentCredentialsForClosedReviewRun(ctx, reviewRunID, s.now().UTC()); err != nil {
		return nil, err
	}
	return matched, nil
}

// ListPendingRevocations reports the credentials whose authority has ended and
// whose rows are still live, WITHOUT changing anything.
//
// This is the recoverable obligation in its readable form. It is not a queue
// and not a ledger: it is re-derived from durable rows every time it is asked,
// so nothing about it can be lost by a crash, and there is nothing to replay
// after one. The handles come back with it so the caller can also remove the
// files the tokens were handed over in.
func (s *Service) ListPendingRevocations(ctx context.Context) ([]domain.RevocableAgentCredential, error) {
	rows, err := s.store.ListRevocableAgentCredentials(ctx)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// ReconcileClosedReviewRuns revokes every credential whose review run has
// stopped running, in one statement, and reports what it took back.
//
// It is what makes revocation survive a failure. A revocation that could not be
// written -- a locked database, a daemon killed between the verdict and the
// cleanup, an installation upgraded onto this build with credentials already
// stranded -- leaves the obligation exactly where it was: derivable from the
// same rows, and discharged by the next pass. Nothing has to remember it.
func (s *Service) ReconcileClosedReviewRuns(ctx context.Context) ([]domain.RevocableAgentCredential, error) {
	pending, err := s.ListPendingRevocations(ctx)
	if err != nil {
		return nil, err
	}
	if len(pending) == 0 {
		return nil, nil
	}
	if _, err := s.store.RevokeClosedReviewRunAgentCredentials(ctx, s.now().UTC()); err != nil {
		return nil, err
	}
	return pending, nil
}

// RevokeForSession revokes every credential bound to one session.
func (s *Service) RevokeForSession(ctx context.Context, sessionID domain.SessionID) (int64, error) {
	if strings.TrimSpace(string(sessionID)) == "" {
		return 0, nil
	}
	return s.store.RevokeAgentCredentialsForSession(ctx, sessionID, s.now().UTC())
}

// EverIssuedForReviewRun reports whether AO ever minted a credential for this
// review run -- revoked and expired ones included.
//
// It answers a question about AO's OWN launch record, and that is what makes it
// usable as evidence: a reviewer for which no credential was ever minted was
// started by a build that could not give one, so on an installation that
// requires authentication it has no channel through which any verdict could
// reach AO. See workflow's ambiguous-review recovery.
func (s *Service) EverIssuedForReviewRun(ctx context.Context, reviewRunID string) (bool, error) {
	if strings.TrimSpace(reviewRunID) == "" {
		return false, nil
	}
	creds, err := s.store.ListAgentCredentialsForReviewRun(ctx, reviewRunID)
	if err != nil {
		return false, err
	}
	return len(creds) > 0, nil
}

// HashToken is the one-way form a credential is stored as.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// BindSession binds a worker credential to the session its launch produced.
//
// This is the second half of the deferred binding described in Issue, and it is
// deliberately one guarded statement rather than a read-then-write: two passes
// racing over the same launch cannot both succeed, and a credential can only
// ever travel from unbound to bound, once.
//
// It reports whether it bound anything. A false with no error means the
// credential was already bound, already revoked, or gone — three different
// histories that are one answer here, because in none of them may this call be
// the one that decides which session the credential speaks for. Callers treat
// that as a failed launch and take the credential back rather than proceeding
// with an authority they cannot account for.
func (s *Service) BindSession(ctx context.Context, credentialID string, sessionID domain.SessionID) (bool, error) {
	if strings.TrimSpace(credentialID) == "" {
		return false, fmt.Errorf("agent credential: a credential id is required to bind a session")
	}
	if strings.TrimSpace(string(sessionID)) == "" {
		// Binding to nothing would leave the credential unbound while telling
		// the caller it succeeded, which is the one outcome this call must not
		// produce.
		return false, fmt.Errorf("agent credential: refusing to bind %s to an empty session", credentialID)
	}
	return s.store.BindAgentCredentialSession(ctx, credentialID, sessionID)
}

// Revoke ends one credential by id. Idempotent: revoking an already-revoked
// credential is a no-op that reports zero, never an error, because the state
// the caller wanted is the state that already holds.
func (s *Service) Revoke(ctx context.Context, credentialID string) (int64, error) {
	if strings.TrimSpace(credentialID) == "" {
		return 0, nil
	}
	return s.store.RevokeAgentCredential(ctx, credentialID, s.now().UTC())
}

// RevokeSupersededWorkers ends every worker credential for one step whose
// attempt is not keepAttemptID.
//
// The one eager path a worker credential has, and it covers the one ending the
// derived sweep cannot see: a step being re-dispatched is still running, so a
// superseded launch's credential still satisfies the sweep's "may live" rule.
// It is called from exactly one place, as a replacement is minted, rather than
// from every transition a step can make — which is the distinction the
// reconciler's own design note draws between an obligation that is derived and
// one that has to be remembered.
func (s *Service) RevokeSupersededWorkers(ctx context.Context, stepID, keepAttemptID string) (int64, error) {
	if strings.TrimSpace(stepID) == "" || strings.TrimSpace(keepAttemptID) == "" {
		// Without both, this cannot tell a predecessor from the launch it is
		// about to authorize, and revoking on a guess would end the credential
		// of the worker that is starting.
		return 0, nil
	}
	return s.store.RevokeSupersededWorkerAgentCredentials(ctx, stepID, keepAttemptID, s.now().UTC())
}

// StillAuthorized reports whether an agent's authority is still current.
//
// This is P5-A phase 2C's CENTRAL FENCE, and it exists because revocation alone
// cannot close the window it needs to close. Revocation is eager and swept, but
// both are things that must have HAPPENED; an authorization taken in the
// interval before either ran would be taken on a credential whose launch is
// over. AgentRoleWorker holds session write -- /send, /kill, /rollback,
// /restore, /resume-agent, /switch-agent, /reviewer, /auto-review -- over a
// session that Checkpoint 8D reuses for the whole step, so that interval is not
// one anybody should have to reason about.
//
// So the question is answered from durable rows, per request, and the answer
// cannot be stale. Three rules:
//
//   - it only ever DENIES. It grants nothing the binding checks would refuse,
//     so it cannot widen an authority;
//   - it applies to WORKERS only. A reviewer's lifetime is its review run and is
//     already handled; asking this question of it would be asking about a step
//     it has no attempt on;
//   - it FAILS CLOSED. An authority AO cannot evaluate is not an authority.
func (s *Service) StillAuthorized(ctx context.Context, authority domain.AgentAuthority) (bool, error) {
	if authority.Role != domain.AgentRoleWorker {
		return true, nil
	}
	if strings.TrimSpace(authority.CredentialID) == "" {
		return false, nil
	}
	return s.store.IsWorkerCredentialAuthorized(ctx, authority.CredentialID)
}

// CloseFinishedWorkers takes back the authority of every worker whose step has
// stopped running, and returns what it took.
//
// It is the same guarded predicate the sweep applies -- deliberately the same,
// so eager and swept cannot disagree about what "finished" means -- which is
// what makes it safe to call from the coordinator's ordinary observation pass
// instead of from each of the twenty-three places a step can transition. While
// the step runs it is a no-op; the moment it stops, the credential is gone.
//
// It exists because the window it closes is not innocuous. AgentRoleWorker
// holds session WRITE, and AuthorizeSessionAccess gates /send, /kill,
// /rollback, /restore, /resume-agent, /switch-agent, /pr/claim, /reviewer and
// /auto-review on exactly that permission. A worker session is reused across a
// step's whole loop (Checkpoint 8D), so a credential that outlived its turn
// could type into the session a later agent is working in. Waiting a
// reconciliation interval for that is a wait with no upside.
//
// The sweep remains the guarantee: this is an optimization that shortens the
// window, not a replacement for an obligation that must survive a crash.
func (s *Service) CloseFinishedWorkers(ctx context.Context) ([]domain.RevocableAgentCredential, error) {
	return s.ReconcileStaleWorkerCredentials(ctx)
}

// ListPendingWorkerRevocations reports the worker credentials whose work step
// has stopped running. The read half of the sweep's own predicate, so a caller
// can see what it is about to discharge — and so a test can prove the sweep and
// the listing cannot disagree about what "finished" means.
func (s *Service) ListPendingWorkerRevocations(ctx context.Context) ([]domain.RevocableAgentCredential, error) {
	return s.store.ListRevocableWorkerAgentCredentials(ctx)
}

// ReconcileStaleWorkerCredentials revokes every worker credential whose work
// step is no longer running, in one set-based statement.
//
// Same derived-obligation design as ReconcileClosedReviewRuns: nothing is
// remembered, everything is re-derived, and a pass that failed is
// indistinguishable from one that never ran. It returns what it revoked so the
// caller can remove the files those credentials were handed over in.
func (s *Service) ReconcileStaleWorkerCredentials(ctx context.Context) ([]domain.RevocableAgentCredential, error) {
	pending, err := s.store.ListRevocableWorkerAgentCredentials(ctx)
	if err != nil {
		return nil, err
	}
	if len(pending) == 0 {
		return nil, nil
	}
	if _, err := s.store.RevokeStaleWorkerAgentCredentials(ctx, s.now().UTC()); err != nil {
		return nil, err
	}
	return pending, nil
}
