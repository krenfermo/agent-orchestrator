package agentauth

import (
	"context"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// adoption.go -- the window between Spawn and bind, and what recovery is
// allowed to conclude about it.
//
// A worker credential is minted UNBOUND because the session it will speak for
// does not exist until the spawn that must already carry it returns. The daemon
// can die in that window. What it leaves behind is not an orphan in any
// harmless sense:
//
//   - a real worker, in a real pane, holding a real credential FILE;
//   - a credential row naming no session, so AgentAuthority.MayReachSession
//     refuses every session route it could use;
//   - a work step whose session recovery can find by natural key and adopt.
//
// Recovery adopted the SESSION and left the credential exactly as it was. So
// the worker came back, kept working, and could never report on its own work --
// permanently, because nothing in the system was going to bind that row later.
// `ao work report` from that pane answered "not authorized" for the rest of the
// run.
//
// # What adoption may conclude, and from what
//
// Not "there is a session and there is a credential, so they go together". The
// proof is narrower and it is made of durable rows:
//
//  1. the credential is a WORKER credential naming THIS work step, and this
//     step's session is the one being adopted (recovery found it by natural
//     key, which is what makes it this step's worker);
//  2. it has never been bound -- session_id is empty -- so nothing else has
//     ever spoken with it and no decision is being overwritten;
//  3. it has not been revoked, which is what makes this generation-safe without
//     inventing a second fence: a replacement launch revokes its predecessors
//     as it mints its own, so a superseded launch's row is already excluded and
//     an old attempt can never adopt its way back into authority;
//  4. it is the ONLY such row for the step. Two orphans are two panes AO cannot
//     tell apart, and there is no fact on either row that distinguishes them;
//  5. the session does not already have a live worker credential. Two live
//     identities for one pane is the state adoption exists to avoid.
//
// Any of those failing is a refusal, not a best guess. The caller escalates,
// because "AO could not prove which worker holds which token" is a thing a
// person can act on and a thing AO must not decide alone.
//
// # Why the attempt fence moves too
//
// A bind alone would not fix anything. IsWorkerCredentialAuthorized requires a
// worker credential to name its step's NEWEST attempt, and an adoption opens a
// new attempt for the launch it adopts -- so a credential bound but still
// fenced to the crashed launch's attempt is refused by the very next request.
// Re-pointing the fence is truthful rather than a loosening: the attempt being
// adopted onto describes the same physical worker the credential was minted
// for. Both writes happen in one guarded statement, so a crash cannot leave
// half of it.

// WorkerAdoptionOutcome is what an adoption decided, and why.
//
// The three values are deliberately distinct. "Nothing to adopt" is the
// ordinary case and is not a problem; "adopted" is the recovery working; and
// "unprovable" is a stop, never a degraded success.
type WorkerAdoptionOutcome string

const (
	// WorkerAdoptionNothingToDo means no orphaned credential exists for this
	// step. The overwhelmingly common answer: a launch that completed bound its
	// credential, and one that failed had it taken back.
	WorkerAdoptionNothingToDo WorkerAdoptionOutcome = "nothing_to_adopt"
	// WorkerAdoptionAdopted means the orphan was bound to the adopted session
	// and re-fenced to the adopting attempt.
	WorkerAdoptionAdopted WorkerAdoptionOutcome = "adopted"
	// WorkerAdoptionUnprovable means an orphan exists but AO cannot prove it
	// belongs to this session. Never resolved by guessing.
	WorkerAdoptionUnprovable WorkerAdoptionOutcome = "unprovable"
)

// WorkerAdoptionInput identifies the launch being adopted.
type WorkerAdoptionInput struct {
	// StepID is the work step whose launch is being adopted. Required.
	StepID string
	// RunID and ProjectID are checked against the credential's own, so a
	// candidate minted under different work is refused rather than adopted.
	RunID     string
	ProjectID domain.ProjectID
	// SessionID is the session recovery proved this step's launch produced.
	SessionID domain.SessionID
	// AttemptID is the attempt doing the adopting. It becomes the credential's
	// new fence; without it the adopted credential is refused by the next
	// authorization check.
	AttemptID string
}

// WorkerAdoptionResult is the outcome plus what a person needs to read.
type WorkerAdoptionResult struct {
	Outcome WorkerAdoptionOutcome
	// CredentialID is the row that was adopted. Empty unless Outcome is
	// adopted.
	CredentialID string
	// Detail explains an unprovable outcome in a sentence a person can act on.
	Detail string
}

// AdoptWorkerSession binds an orphaned worker credential to the session its
// launch really produced, or refuses and says why.
//
// See this file's header for the proof it requires. It never mints, never
// revokes, and never touches a credential that has already spoken.
func (s *Service) AdoptWorkerSession(ctx context.Context, in WorkerAdoptionInput) (WorkerAdoptionResult, error) {
	if strings.TrimSpace(in.StepID) == "" || strings.TrimSpace(string(in.SessionID)) == "" {
		// Without both, there is nothing to prove a credential against.
		return WorkerAdoptionResult{Outcome: WorkerAdoptionNothingToDo}, nil
	}
	if strings.TrimSpace(in.AttemptID) == "" {
		// Adopting without an attempt to fence to would produce a credential
		// that binds and is then refused by the next authorization check --
		// worse than not adopting, because it looks like it worked.
		return WorkerAdoptionResult{
			Outcome: WorkerAdoptionUnprovable,
			Detail:  "the adoption named no attempt to fence the credential to",
		}, nil
	}

	candidates, err := s.store.ListAdoptableWorkerAgentCredentials(ctx, in.StepID)
	if err != nil {
		return WorkerAdoptionResult{}, err
	}
	if len(candidates) == 0 {
		return WorkerAdoptionResult{Outcome: WorkerAdoptionNothingToDo}, nil
	}
	if len(candidates) > 1 {
		return WorkerAdoptionResult{
			Outcome: WorkerAdoptionUnprovable,
			Detail: fmt.Sprintf(
				"work step %s has %d worker credentials that were never bound to a session, and nothing on those rows says which pane holds which token",
				in.StepID, len(candidates)),
		}, nil
	}

	candidate := candidates[0]
	if in.RunID != "" && candidate.WorkflowRunID != "" && candidate.WorkflowRunID != in.RunID {
		return WorkerAdoptionResult{
			Outcome: WorkerAdoptionUnprovable,
			Detail: fmt.Sprintf("the unbound credential for work step %s was minted under run %s, not %s",
				in.StepID, candidate.WorkflowRunID, in.RunID),
		}, nil
	}
	if in.ProjectID != "" && candidate.ProjectID != "" && candidate.ProjectID != in.ProjectID {
		return WorkerAdoptionResult{
			Outcome: WorkerAdoptionUnprovable,
			Detail: fmt.Sprintf("the unbound credential for work step %s was minted for project %s, not %s",
				in.StepID, candidate.ProjectID, in.ProjectID),
		}, nil
	}

	live, err := s.store.CountLiveWorkerAgentCredentialsForSession(ctx, in.SessionID)
	if err != nil {
		return WorkerAdoptionResult{}, err
	}
	if live > 0 {
		return WorkerAdoptionResult{
			Outcome: WorkerAdoptionUnprovable,
			Detail: fmt.Sprintf("session %s already speaks with a live worker credential, so adopting another would give one pane two identities",
				in.SessionID),
		}, nil
	}

	adopted, err := s.store.AdoptWorkerAgentCredential(ctx, candidate.CredentialID, in.SessionID, in.AttemptID)
	if err != nil {
		return WorkerAdoptionResult{}, err
	}
	if !adopted {
		// The guarded write matched nothing: the row was bound, revoked or
		// adopted between the read and the write. Somebody else decided, and
		// this call defers to them rather than retrying into a race.
		return WorkerAdoptionResult{
			Outcome: WorkerAdoptionUnprovable,
			Detail: fmt.Sprintf("credential %s stopped being adoptable while it was being adopted (bound, revoked, or taken by another pass)",
				candidate.CredentialID),
		}, nil
	}
	return WorkerAdoptionResult{Outcome: WorkerAdoptionAdopted, CredentialID: candidate.CredentialID}, nil
}
