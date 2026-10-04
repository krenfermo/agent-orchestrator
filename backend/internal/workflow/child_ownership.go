package workflow

import (
	stdctx "context"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// stampChildOwnership is Checkpoint 8P-C.1's single durable propagation
// path: a master task's child run always inherits its owner from the
// PARENT WorkflowRun's already-resolved owner -- never the current request
// identity, never a client-supplied user id, never guessed later from a
// ProviderProfile. Called from both branches of dispatchMasterTask (fresh
// creation AND the FindWorkflowRunByPlannedTask recovery branch), always
// before any StartRun/dispatch call, so it closes the durable window where
// a child could exist with a NULL owner after a crash: the very next
// reconcile pass re-enters dispatchMasterTask, finds the child by its
// natural key (planned_task_id), and re-stamps here -- idempotently, since
// SetWorkflowRunOwner is a plain overwrite-with-the-same-value on retry --
// before StartRun is ever called again.
//
// A parent with no owner (predates 8P-A ownership, or created while no
// identity was resolved) leaves the child unowned too -- exactly
// pre-8P-C.1 behavior, never invented.
func (c *Coordinator) stampChildOwnership(ctx stdctx.Context, childRunID string, parent domain.WorkflowRun) error {
	owner := c.runOwner(ctx, parent.ID)
	if owner == "" {
		return nil
	}
	ok, err := c.store.SetWorkflowRunOwner(ctx, childRunID, owner)
	if err != nil {
		return fmt.Errorf("stamp child run %s owner from parent %s: %w", childRunID, parent.ID, err)
	}
	if !ok {
		return fmt.Errorf("stamp child run %s owner: run not found", childRunID)
	}
	return nil
}

// requireChildOwnershipForDispatch is the hard multi-user-mode gate
// (checkpoint brief §3/§13): a master task's child run must NEVER dispatch
// a provider process while its owner is unresolved when the parent itself
// has one. In trusted-local mode (c.trustedLocal) this never blocks --
// matching every other trusted-local compatibility path in this package.
// This does not touch the general (non-master-task) unowned-run behavior
// elsewhere in the daemon; it only guards the one path 8P-C.1 introduces
// durable child ownership for.
func (c *Coordinator) requireChildOwnershipForDispatch(ctx stdctx.Context, childRunID string, parent domain.WorkflowRun) error {
	if c.trustedLocal {
		return nil
	}
	parentOwner := c.runOwner(ctx, parent.ID)
	if parentOwner == "" {
		// Parent itself predates ownership -- nothing to enforce, matches
		// trusted-local/pre-8P-A compatibility.
		return nil
	}
	childOwner := c.runOwner(ctx, childRunID)
	if childOwner == "" {
		return fmt.Errorf("%w: child run %s has no resolved owner (parent %s owned by %s)", ErrInvalid, childRunID, parent.ID, parentOwner)
	}
	if childOwner != parentOwner {
		return fmt.Errorf("%w: child run %s owner %s does not match parent %s owner %s", ErrInvalid, childRunID, childOwner, parent.ID, parentOwner)
	}
	return nil
}
