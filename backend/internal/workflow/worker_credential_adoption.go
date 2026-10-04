package workflow

import (
	stdctx "context"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// worker_credential_adoption.go -- the launch-side half of the Spawn/bind
// window.
//
// A worker credential is minted before the spawn (the environment is fixed at
// spawn) and bound after it (the session does not exist before it). A daemon
// that dies between the two leaves a running worker holding a token AO refuses
// every session route for, and nothing that was ever going to bind it later.
// Recovery adopted the SESSION -- correctly, on natural-key evidence -- and left
// the credential exactly as it was, so the worker came back and could never
// report on its own work for the rest of the run.
//
// So an adoption now adopts BOTH, and it is the same rule twice: act on proof,
// escalate otherwise, never guess. service/agentauth/adoption.go carries the
// proof; this file carries what the coordinator does with each answer.

// WorkerCredentialAdopter re-attaches an orphaned worker credential to the
// session its launch really produced.
//
// Optional, like every other dependency here. A nil adopter means the behaviour
// this package had before the interface existed, which is also the correct
// behaviour on an installation where a cookie-less call already resolves the
// operator: there is no identity to re-attach, so there is nothing to recover.
type WorkerCredentialAdopter interface {
	// AdoptWorkerCredential returns whether the adoption could be completed,
	// and a sentence explaining any refusal. A refusal is a REAL stop on an
	// installation that requires an identity: the worker is running and
	// provably cannot report. It is not a stop where an identity is optional,
	// and the implementation -- which is the thing that knows which
	// installation this is -- decides that, exactly as the launcher does when
	// it mints.
	AdoptWorkerCredential(ctx stdctx.Context, req WorkerCredentialAdoptionRequest) (WorkerCredentialAdoption, error)
}

// WorkerCredentialAdoptionRequest identifies the launch being adopted.
type WorkerCredentialAdoptionRequest struct {
	RunID     string
	StepID    string
	ProjectID domain.ProjectID
	// SessionID is the session recovery proved this step's launch produced.
	SessionID domain.SessionID
	// AttemptID is the attempt doing the adopting, which becomes the adopted
	// credential's fence.
	AttemptID string
}

// WorkerCredentialAdoption is the answer.
type WorkerCredentialAdoption struct {
	// Blocked is true only when an orphaned credential exists, AO could not
	// prove it belongs to this session, and this installation requires an
	// identity for a worker to be able to report at all.
	Blocked bool
	// Detail explains a Blocked answer in a sentence a person can act on.
	Detail string
	// CredentialID names an adopted credential, for the log and the ledger.
	CredentialID string
}

// adoptWorkerCredential re-attaches the orphaned credential, if there is one.
//
// It returns an error only for a refusal that must stop the adoption. A store
// failure is deliberately NOT one: the worker session is real and adopting it
// is right whatever happened to the credential bookkeeping, and the sweep plus
// the next recovery pass both re-derive this obligation from durable rows. What
// must never happen is a confirmed dispatch over a credential AO cannot account
// for -- which is the Blocked case, and only that.
func (c *Coordinator) adoptWorkerCredential(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, sessionID domain.SessionID, attemptID string,
) error {
	if c.workerCredentialAdopter == nil {
		return nil
	}
	res, err := c.workerCredentialAdopter.AdoptWorkerCredential(ctx, WorkerCredentialAdoptionRequest{
		RunID: run.ID, StepID: step.ID, ProjectID: domain.ProjectID(run.ProjectID),
		SessionID: sessionID, AttemptID: attemptID,
	})
	if err != nil {
		if c.log != nil {
			c.log.Warn("workflow: could not re-attach an adopted worker's credential",
				"step", step.ID, "session", sessionID, "err", err)
		}
		return nil
	}
	if res.Blocked {
		return fmt.Errorf("%w: %s", errWorkerCredentialUnadoptable, res.Detail)
	}
	if res.CredentialID != "" && c.log != nil {
		c.log.Info("workflow: re-attached an adopted worker's credential",
			"step", step.ID, "session", sessionID, "credential", res.CredentialID, "attempt", attemptID)
	}
	return nil
}

// errWorkerCredentialUnadoptable is the internal sentinel for "a worker is
// running, it holds a credential AO cannot account for, and this installation
// needs it to have one". It never leaves the package: recordDispatchSuccess
// converts it into the ordinary ambiguity stop, which is the vocabulary a
// person already reads for "AO could not prove what happened to a launch".
var errWorkerCredentialUnadoptable = fmt.Errorf("%w: an adopted worker's credential could not be accounted for", ErrInvalid)
