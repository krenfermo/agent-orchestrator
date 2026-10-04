package daemon

import (
	"context"
	"log/slog"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/runtimegc"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// terminal_runtime_wiring.go — binding the workflow coordinator's terminal
// runtime reclamation to the two components that already own the halves of it.
//
// The coordinator decides WHICH sessions may be ended and under what run
// states (workflow/terminal_runtime.go). It deliberately owns neither of the
// mechanisms:
//
//   - runtimegc.Sweeper owns "prove this runtime is AO's own, addressed to its
//     exact incarnation, and destroy it". Reusing it is what keeps the
//     immediate terminal path and the fifteen-minute sweep from being two
//     implementations of the same proofs that could disagree.
//   - lifecycle.Manager owns the durable session record. It is the only
//     component allowed to write terminality, and routing through it is what
//     keeps this from becoming a second session lifecycle.
//
// Neither is a new abstraction, and nothing here decides anything.
type terminalRuntimeReclaimer struct {
	sweeper *runtimegc.Sweeper
	// sessions writes the durable terminality. lifecycle.Manager satisfies it.
	sessions sessionTerminalWriter
	log      *slog.Logger
}

// sessionTerminalWriter is the one lifecycle capability this needs.
type sessionTerminalWriter interface {
	MarkTerminated(ctx context.Context, id domain.SessionID) error
}

// newTerminalRuntimeReclaimer returns nil when either half is missing, which
// the coordinator reads as "no immediate reclamation" and which leaves the
// periodic sweep to do the same work later. A half-wired reclaimer would be
// worse than none: destroying a runtime without recording the session's
// terminality leaves a row that says a dead agent is running.
func newTerminalRuntimeReclaimer(sweeper *runtimegc.Sweeper, sessions sessionTerminalWriter, log *slog.Logger) workflowcore.TerminalRuntimeReclaimer {
	if sweeper == nil || sessions == nil {
		return nil
	}
	return terminalRuntimeReclaimer{sweeper: sweeper, sessions: sessions, log: log}
}

// ReclaimSessionRuntime records the termination INTENT, then ends the runtime.
//
// The order is the crash-safety argument, and it only works this way round:
//
//	intent then destroy   a crash in between leaves a terminated session whose
//	                      runtime identity is still recorded, which is exactly
//	                      runtimegc's ordinary OrphanTerminatedSession candidate.
//	                      The next sweep finishes the job. Converges.
//	destroy then intent   a crash in between leaves a row claiming a live agent
//	                      that no longer exists. Nothing is unsafe, but the
//	                      liveness guard now protects a corpse from GC and the
//	                      row stays wrong until the next terminal reconcile.
//
// So the intent is written first and the destroy is allowed to fail. A failed
// destroy is reported, not retried here: the sweeper re-derives the candidate
// from durable facts on its next pass, and a retry loop in this path would be a
// second scheduler.
//
// The workspace is untouched. See workflow/terminal_runtime.go on why a
// finished run keeps its worktree and every durable record while its process
// ends.
func (r terminalRuntimeReclaimer) ReclaimSessionRuntime(
	ctx context.Context, req workflowcore.TerminalRuntimeRequest,
) (bool, string, error) {
	if err := r.sessions.MarkTerminated(ctx, req.SessionID); err != nil {
		// Without the durable intent there is nothing to converge on, so
		// nothing is destroyed. The next reconcile tries again.
		return false, "AO could not record the session's terminality, so its runtime was left alone", err
	}
	finding, err := r.sweeper.ReclaimSessionRuntime(ctx, runtimegc.ReclaimRequest{
		SessionID:     string(req.SessionID),
		LaunchID:      req.LaunchID,
		Handle:        req.Handle,
		InstanceID:    req.InstanceID,
		OwnerToken:    req.OwnerToken,
		WorkflowRunID: req.WorkflowRunID,
		Reason:        req.Reason,
	})
	if err != nil {
		return false, "AO could not read the durable state needed to prove this runtime is its own", err
	}
	return finding.Disposition == runtimegc.DispositionCleaned, finding.Reason, nil
}
