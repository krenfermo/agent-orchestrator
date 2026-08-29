package workflow

import (
	stdctx "context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// terminal_runtime.go — ending the runtimes a finished workflow owns.
//
// # The gap this closes
//
// Until now the Coordinator had no way to end a session at all. When a run
// reached a terminal state it released its capacity claim and stopped, and the
// worker's tmux pane, shell and agent process were simply left running. They
// were not orphans in any sense Runtime GC could act on either: the session row
// still said `is_terminated = 0`, and GC correctly refuses to reclaim a session
// AO durably records as running.
//
// That is what produced the incident's second half — twenty-five live agent
// processes from workflows finished hours and days earlier, several from the
// previous day, every one of them attached to a run that had long since
// completed or been cancelled. Nothing was broken; nothing had ever been asked
// to end them.
//
// # Why the policy is stated in terms of the RUN and not the process
//
// The question "may this process be killed" has no safe answer. The question
// this file asks instead is "does any live lifecycle authority still require
// this runtime", which is answerable from durable facts, and it is answered
// conservatively at every step:
//
//	completed / failed / cancelled   the obligation is discharged. End the
//	                                 runtime once ownership and incarnation are
//	                                 proven.
//	needs_attention                  NEVER automatically. See below.
//	running / waiting / pending      not terminal; nothing to decide.
//
// # needs_attention is deliberately excluded, not incidentally
//
// A stopped run is not a finished one. Resume, Repair, a fresh review and every
// authority-recovery path in this package may legitimately re-use the SAME
// session, and several of them (fix re-delivery, resumeUnstartedFixCycle) do
// exactly that — they re-deliver into the session the run already holds. Ending
// its runtime would convert a recoverable stop into an unrecoverable one.
//
// AO does not currently carry, per attention reason, a durable statement of
// whether recovery from that reason reuses the session or opens a fresh
// generation. Rather than infer it — which is the heuristic §D forbids — the
// answer is the same for every reason: preserve. attentionReasonRequiresRuntime
// states that explicitly so the decision is visible, testable, and has one
// place to change when that information does exist.
//
// # Evidence is not the process
//
// Nothing here removes a worktree, a branch, a checkpoint, an attempt, a review
// finding or an integration record. Those are durable and outlive the runtime
// that produced them, which is exactly why a finished run does not need a live
// agent process to keep its evidence readable. The worktree sweep is a separate
// decision with a separate proof (runtimegc's worktreeCandidates), and it is
// not reached from here.
//
// # Ordering, and why Runtime GC is still the safety net
//
// The durable session terminality is written BEFORE the destroy, on purpose. It
// is the termination intent, and it is what makes every crash window converge:
//
//	crash after terminal, before intent    the next reconcile re-derives it
//	crash after intent, before destroy     GC sees a terminated session with a
//	                                       provable identity and finishes the job
//	crash after destroy, before ack        the runtime is already absent; the
//	                                       next pass records absent and stops
//
// So this path makes a finished session end promptly, and GC remains the thing
// that catches whatever this path did not — including its own interruptions.

// TerminalRuntimeReclaimer ends ONE session's runtime, addressed to one exact
// incarnation, after proving the runtime is AO's own.
//
// It is deliberately not a session-management interface. Kill() tears down the
// workspace and destroys by NAME, both of which are wrong here: the worktree is
// evidence a finished run must keep, and a name-addressed destroy is the
// ABA-unsafe primitive ownership-sensitive code is forbidden to use (see
// ports.TestOwnershipSensitivePathsNeverUseNameOnlyDestroy, which walks this
// package). Production wires this to runtimegc's own ReclaimSessionRuntime, so
// the terminal path and the periodic sweep share one implementation of "prove
// it is mine, then destroy that incarnation".
//
// Optional, like every other dependency here: nil means terminal runtimes are
// left to the periodic sweep, which is untidy and never unsafe.
type TerminalRuntimeReclaimer interface {
	// ReclaimSessionRuntime ends the runtime and records the session's
	// terminality. Reclaimed reports whether the runtime was actually
	// destroyed; reason always explains the outcome, destroyed or not.
	ReclaimSessionRuntime(ctx stdctx.Context, req TerminalRuntimeRequest) (reclaimed bool, reason string, err error)
}

// TerminalRuntimeRequest is one session's runtime and the durable identity that
// authorizes ending it. Every field is read from a row; none is derived here.
type TerminalRuntimeRequest struct {
	SessionID     domain.SessionID
	Handle        string
	InstanceID    string
	OwnerToken    string
	LaunchID      string
	WorkflowRunID string
	Reason        string
}

// attentionReasonRequiresRuntime reports whether a run parked at
// needs_attention may still need its runtime.
//
// It answers true for everything, and that is the decision rather than a
// placeholder: see the discussion above. It exists as a named function so the
// policy has one site, one test, and one place to become reason-specific if AO
// ever records which recoveries reuse a session.
func attentionReasonRequiresRuntime(reason string) bool {
	_ = reason
	return true
}

// runtimeReclaimableForRunState reports whether a run in this state has
// discharged every obligation that could require its runtime, and the reason
// to record either way.
func runtimeReclaimableForRunState(state domain.WorkflowRunState, attentionReason string) (bool, string) {
	switch state {
	case domain.WorkflowRunCompleted:
		return true, "the workflow completed, so no lifecycle authority still requires this runtime"
	case domain.WorkflowRunCancelled:
		return true, "the workflow was cancelled, so no lifecycle authority still requires this runtime"
	case domain.WorkflowRunFailed:
		// Terminal failure, which is distinct from needs_attention: a failed run
		// has no bounded recovery left to run. Repair opens a NEW child run with
		// its own generation and its own session, so nothing reuses this one.
		return true, "the workflow failed terminally and no bounded recovery reuses this runtime"
	case domain.WorkflowRunNeedsAttention:
		if attentionReasonRequiresRuntime(attentionReason) {
			return false, "the run is parked for attention and its runtime may still be required for resume, repair or inspection"
		}
		return true, "the run is parked for attention on a reason whose recovery uses a fresh runtime"
	default:
		return false, "the run is not terminal"
	}
}

// reclaimTerminalRuntimes ends the runtimes a terminal run still owns.
//
// Idempotent by construction and safe to call from every reconciliation pass:
// a session already recorded as terminated is skipped, a runtime already gone
// resolves to "absent", and everything unprovable is left alone. It never
// returns an error — a run whose runtimes cannot be reasoned about must not
// stop the caller from finishing what it was doing — and every refusal is
// logged with its reason.
func (c *Coordinator) reclaimTerminalRuntimes(ctx stdctx.Context, run domain.WorkflowRun, steps []domain.WorkflowStep) {
	if c.terminalRuntimes == nil || c.sessionFacts == nil {
		return
	}
	reclaimable, why := runtimeReclaimableForRunState(run.State, c.attentionReasonForRun(ctx, run))
	if !reclaimable {
		return
	}
	for _, sessionID := range c.runtimeOwningSessionsForRun(ctx, run, steps) {
		c.reclaimOneTerminalRuntime(ctx, run, sessionID, why)
	}
}

// reclaimTerminalRuntimesForRun is the by-id entry point the terminal
// transitions use, mirroring retirePlacementsForTerminalRun exactly.
//
// It takes the state the caller just CAS'd rather than trusting a re-read, for
// the same reason that function does: a reclamation must never be skipped
// because of a stale read of the very transition that triggered it.
func (c *Coordinator) reclaimTerminalRuntimesForRun(ctx stdctx.Context, runID string, state domain.WorkflowRunState) {
	if c.terminalRuntimes == nil || runID == "" || !state.Terminal() {
		return
	}
	run, found, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil || !found {
		if err != nil && c.log != nil {
			c.log.Warn("workflow: could not read a terminal run to end its runtimes", "run", runID, "err", err)
		}
		return
	}
	run.State = state
	steps, err := c.store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		if c.log != nil {
			c.log.Warn("workflow: could not read a terminal run's steps to end its runtimes", "run", runID, "err", err)
		}
		return
	}
	c.reclaimTerminalRuntimes(ctx, run, steps)
}

// runtimeOwningSessionsForRun is every session this run may still own a runtime
// through, in a stable order and without duplicates.
//
// Two sources, and both are durable:
//
//   - the run's own steps, which is where a worker, a fix worker and an
//     independently-owned reviewer each record the session they were given;
//   - the run's provider attempts, which is §H: a Claude or Codex attempt that
//     was superseded by a failover named a runtime of its own, and a stale
//     attempt must not be left holding one after the obligation it belonged to
//     is terminal. The attempt's own state is not consulted — the RUN is
//     terminal, so every attempt under it is finished by definition.
func (c *Coordinator) runtimeOwningSessionsForRun(ctx stdctx.Context, run domain.WorkflowRun, steps []domain.WorkflowStep) []domain.SessionID {
	seen := map[domain.SessionID]struct{}{}
	var out []domain.SessionID
	add := func(id string) {
		if id == "" {
			return
		}
		sid := domain.SessionID(id)
		if _, dup := seen[sid]; dup {
			return
		}
		seen[sid] = struct{}{}
		out = append(out, sid)
	}
	for _, step := range steps {
		if step.SessionID != nil {
			add(*step.SessionID)
		}
	}
	if c.providerAttempts != nil {
		attempts, err := c.providerAttempts.ListProviderAttemptsForRun(ctx, run.ID)
		if err != nil {
			if c.log != nil {
				c.log.Warn("workflow: could not read provider attempts while ending a terminal run's runtimes",
					"run", run.ID, "err", err)
			}
		}
		for _, a := range attempts {
			add(a.RuntimeSessionID)
		}
	}
	return out
}

// reclaimOneTerminalRuntime applies the policy to one session.
func (c *Coordinator) reclaimOneTerminalRuntime(ctx stdctx.Context, run domain.WorkflowRun, sessionID domain.SessionID, why string) {
	rec, found, err := c.sessionFacts.GetSession(ctx, sessionID)
	if err != nil {
		if c.log != nil {
			c.log.Warn("workflow: could not read a terminal run's session to end its runtime",
				"run", run.ID, "session", sessionID, "err", err)
		}
		return
	}
	if !found {
		// The row is gone. There is nothing to prove ownership with and
		// nothing to mark, so there is nothing to do — never a licence to go
		// looking for the name on the runtime.
		return
	}
	if rec.IsTerminated {
		// Already ended, or ended by somebody else. The runtime, if any
		// survived, is now Runtime GC's ordinary terminated-session candidate.
		return
	}
	reclaimed, reason, rerr := c.terminalRuntimes.ReclaimSessionRuntime(ctx, TerminalRuntimeRequest{
		SessionID:     rec.ID,
		Handle:        rec.Metadata.RuntimeHandleID,
		InstanceID:    rec.Metadata.RuntimeInstanceID,
		OwnerToken:    rec.Metadata.RuntimeOwnerToken,
		LaunchID:      rec.Metadata.RuntimeLaunchID,
		WorkflowRunID: run.ID,
		Reason:        why,
	})
	if rerr != nil {
		if c.log != nil {
			c.log.Warn("workflow: ending a terminal run's runtime failed; the periodic sweep will re-derive it",
				"run", run.ID, "session", sessionID, "err", rerr)
		}
		return
	}
	if c.log != nil {
		c.log.Info("workflow: terminal run runtime reclamation",
			"run", run.ID, "runState", string(run.State), "session", sessionID,
			"instance", rec.Metadata.RuntimeInstanceID, "reclaimed", reclaimed, "reason", reason)
	}
}

// attentionReasonForRun reads the stop reason a needs_attention run is parked
// on, or "" when there is none to read.
//
// Best-effort on purpose: the ONLY consumer is the needs_attention branch of
// runtimeReclaimableForRunState, whose answer is "preserve" both when a reason
// is found and when one is not, so an unreadable store can never turn into a
// destroy.
func (c *Coordinator) attentionReasonForRun(ctx stdctx.Context, run domain.WorkflowRun) string {
	if run.State != domain.WorkflowRunNeedsAttention {
		return ""
	}
	cps, err := c.store.ListWorkflowCheckpoints(ctx, run.ID)
	if err != nil {
		return ""
	}
	var newest domain.WorkflowCheckpoint
	found := false
	for _, cp := range cps {
		if cp.DurablePhase == "" {
			continue
		}
		if _, known := attentionDispositions[cp.DurablePhase]; !known {
			continue
		}
		if !found || cp.CreatedAt.After(newest.CreatedAt) {
			newest, found = cp, true
		}
	}
	if !found {
		return ""
	}
	return newest.DurablePhase
}
