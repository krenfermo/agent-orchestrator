package domain

import "time"

// AgentRole is the kind of AO-launched agent a credential belongs to.
//
// It exists because "what may an agent do" is a property of the JOB, not of the
// person the agent acts for: a reviewer needs to record a verdict on the one
// session it was launched over, and needs nothing else, whoever owns the run.
// Deriving the ceiling from the role rather than from the account is what keeps
// an owner's agent from inheriting an owner's authority.
type AgentRole string

const (
	// AgentRoleReviewer is a reviewer pane AO launched for one review run.
	AgentRoleReviewer AgentRole = "reviewer"
	// AgentRoleWorker is a worker/fix pane AO launched for one workflow step.
	AgentRoleWorker AgentRole = "worker"
)

// ValidAgentRole reports whether r is a role this build issues credentials for.
func ValidAgentRole(r AgentRole) bool {
	switch r {
	case AgentRoleReviewer, AgentRoleWorker:
		return true
	default:
		return false
	}
}

// agentRoleCeiling is the MOST a credential of each role may ever carry. It is
// a closed table for the same reason the RBAC role tables are: a permission an
// agent can hold has to be a decision somebody made once, in one place, and not
// the accidental sum of whatever the issuing call site happened to pass.
//
// Both roles are deliberately tiny. An agent reads and writes the ONE session it
// was launched into (that is how a reviewer records its verdict and how a worker
// reports progress) and reads the run it belongs to. It holds no installation
// permission, no tenant permission, and no authority over any other project --
// and, because AgentAuthority.Allows denies every non-project scope outright,
// adding one to this table could not grant it either.
var agentRoleCeiling = map[AgentRole][]Permission{
	AgentRoleReviewer: {PermSessionRead, PermSessionWrite, PermWorkflowRead},
	AgentRoleWorker:   {PermSessionRead, PermSessionWrite, PermWorkflowRead},
}

// AgentRoleCeiling returns the permissions a role may hold, in a stable order.
// An unknown role holds nothing -- a credential issued for a role this build
// does not know must be inert, never unbounded.
func AgentRoleCeiling(r AgentRole) []Permission {
	ceiling, ok := agentRoleCeiling[r]
	if !ok {
		return nil
	}
	out := make([]Permission, len(ceiling))
	copy(out, ceiling)
	return out
}

// CapAgentPermissions intersects a requested permission set with its role's
// ceiling, preserving the ceiling's order and dropping duplicates. An empty
// request means "the whole ceiling", which is what every AO launch asks for;
// passing an explicit set is for a caller that wants LESS.
func CapAgentPermissions(role AgentRole, requested []Permission) []Permission {
	ceiling := agentRoleCeiling[role]
	if len(requested) == 0 {
		return AgentRoleCeiling(role)
	}
	want := make(map[Permission]bool, len(requested))
	for _, p := range requested {
		want[p] = true
	}
	out := make([]Permission, 0, len(ceiling))
	for _, p := range ceiling {
		if want[p] {
			out = append(out, p)
		}
	}
	return out
}

// AgentAuthority is the non-secret shape of an agent credential: everything a
// request needs in order to decide what this agent may do, and nothing that
// could be replayed. It travels on the Principal exactly like the user does.
//
// Every field except Permissions is a BINDING, not a hint. The credential was
// minted for one project, one session, one workflow run and one step, and an
// authorization that ignored any of them would turn a reviewer's credential into
// a key to the whole installation the moment it leaked out of its pane.
type AgentAuthority struct {
	// CredentialID is the agent_credentials row, for audit correlation.
	CredentialID string
	// Role is the ceiling the permissions below were capped against.
	Role AgentRole
	// ProjectID is the ONLY project this credential has any authority in.
	ProjectID ProjectID
	// SessionID is the ONLY session it may read or write. Empty means none --
	// which denies every session route rather than allowing all of them.
	SessionID SessionID
	// WorkflowRunID is the ONLY run it may reach. Empty means none.
	WorkflowRunID string
	// WorkflowStepID is the step the launch belonged to. Carried for audit and
	// for the recovery that has to tell one generation's reviewer from the next.
	WorkflowStepID string
	// ReviewRunID is the review_run a reviewer credential was minted for, empty
	// for a worker.
	ReviewRunID string
	// AttemptID is the launch this credential was minted for -- the work step's
	// attempt row, for a worker. It is what separates one generation of a step
	// from the next, and it is projected here (rather than left on the row)
	// because the authorization fence has to be able to ask "is this still the
	// authorized launch?" from the request's own principal.
	AttemptID string
	// Permissions is the durable grant, already capped by Role at issue time.
	Permissions []Permission
}

// Allows reports whether this agent's own grant permits perm on res. It is the
// AGENT half of the decision only: the account the agent acts for still has to
// permit the same thing, which is why authz.Subject.Allows evaluates both and
// grants only their intersection.
//
// Two rules, and both are refusals:
//
//   - a non-project scope is always denied. An agent has no business managing
//     users, settings, providers or an organization, and expressing that as a
//     missing table entry would make it a typo away from being granted.
//   - a project other than the bound one is always denied, whatever the account
//     behind the credential may do elsewhere.
func (a AgentAuthority) Allows(perm Permission, res AuthzResource) bool {
	if ScopeOf(perm) != AuthzScopeProject {
		return false
	}
	if res.Scope != AuthzScopeProject || res.Project == "" {
		return false
	}
	if a.ProjectID == "" || res.Project != a.ProjectID {
		return false
	}
	for _, held := range a.Permissions {
		if held == perm {
			return true
		}
	}
	return false
}

// MayReachSession reports whether id is the one session this credential was
// bound to. An unbound credential reaches no session at all.
func (a AgentAuthority) MayReachSession(id SessionID) bool {
	return a.SessionID != "" && id == a.SessionID
}

// MayReachWorkflowRun reports whether id is the one run this credential was
// bound to. An unbound credential reaches no run at all.
func (a AgentAuthority) MayReachWorkflowRun(id string) bool {
	return a.WorkflowRunID != "" && id == a.WorkflowRunID
}

// AgentCredential is one durable agent-credential row. The raw token is only
// ever returned by the issuing call and never stored: TokenHash is its SHA-256,
// exactly as auth_sessions already does for a browser session.
type AgentCredential struct {
	ID        string
	TokenHash string
	Role      AgentRole
	// UserID is the account the agent acts FOR. It is never widened: the
	// credential can only ever do less than this account can.
	UserID         UserID
	ProjectID      ProjectID
	SessionID      SessionID
	WorkflowRunID  string
	WorkflowStepID string
	ReviewRunID    string
	// RuntimeHandle and RuntimeInstanceID name the exact incarnation this
	// credential was minted for, so a revocation can be aimed at one launch
	// rather than at a reusable name.
	RuntimeHandle     string
	RuntimeInstanceID string
	// Generation is the launch generation, so a replacement reviewer's
	// credential is distinguishable from its predecessor's.
	Generation  int64
	Permissions []Permission
	CreatedAt   time.Time
	ExpiresAt   time.Time
	LastSeenAt  time.Time
	RevokedAt   *time.Time
}

// RevocableAgentCredential names one live credential whose authority has ended
// -- the review run it was minted for is no longer running.
//
// It is a projection rather than the whole row on purpose: a revocation pass
// needs the row to revoke, the run that ended in order to explain itself, and
// the runtime handle in order to remove the file the token was handed over in.
// It never needs the token hash, so it never carries it.
type RevocableAgentCredential struct {
	CredentialID string
	// ReviewRunID names the review run that ended, for a reviewer credential.
	// Empty for a worker's.
	ReviewRunID string
	// WorkflowStepID names the work step that stopped running, for a worker
	// credential. Empty for a reviewer's.
	//
	// The two are separate fields rather than one "reason" string because a
	// revocation has to be able to say WHICH obligation it discharged, and a
	// reader that cannot tell a finished review from a finished work step
	// cannot check the sweep did the right thing.
	WorkflowStepID string
	RuntimeHandle  string
}

// Authority projects the row onto the non-secret shape a request carries.
func (c AgentCredential) Authority() AgentAuthority {
	return AgentAuthority{
		CredentialID:   c.ID,
		Role:           c.Role,
		ProjectID:      c.ProjectID,
		SessionID:      c.SessionID,
		WorkflowRunID:  c.WorkflowRunID,
		WorkflowStepID: c.WorkflowStepID,
		ReviewRunID:    c.ReviewRunID,
		AttemptID:      c.RuntimeInstanceID,
		Permissions:    c.Permissions,
	}
}

// Active reports whether the credential may still authenticate at now.
func (c AgentCredential) Active(now time.Time) bool {
	return c.RevokedAt == nil && c.ExpiresAt.After(now)
}
