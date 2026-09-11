package sessionmanager

import (
	"context"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// compaction.go -- asking a session's agent to replace its own conversation
// with a summary of it.
//
// AO does not own the model loop. It launches an interactive harness in a pane
// and writes prompts into it, so the conversation that grows call after call
// is one AO can measure and cannot hold. Every other lever AO has on cost acts
// on what it SENDS; this is the only one that acts on what the harness has
// already accumulated, and it works the only way it can -- by asking, in the
// harness's own vocabulary, through the same guarded transport every other
// prompt uses.
//
// What that buys is arithmetic, not magic. A run's billable input is the sum
// of the context over its calls, so a conversation that resets partway through
// stops charging every later call for the exploration that preceded it. On the
// measured run (wf-1c2cb9bd) the three repair cycles made 75 of the 193 calls
// against a conversation of 197k-324k tokens, and they were re-reading a base
// exploration that had already produced the change under review.
//
// WHAT THIS IS NOT. It is not a replacement for AO's own durable facts. A
// harness summarizing itself keeps what it judges relevant; the objective, the
// acceptance criteria, the unresolved review findings and the approval state
// travel in the SessionContextPack, in full, in the message that follows. If
// the compaction loses something, the pack puts it back. That ordering -- ask
// to compact, then re-anchor with facts -- is what makes this safe to do at a
// repair boundary at all, and it is the same ordering Checkpoint 8M's COMPACT
// action always described and never actually performed.

// CompactConversation asks the agent behind a session to compact its own
// conversation, and reports whether the request was actually delivered.
//
// requested=false with a nil error is the ordinary, expected outcome for a
// harness with no compaction vocabulary. It is not a failure and callers must
// treat it as "carry on exactly as before".
//
// An error means the transport refused. The caller decides what that is worth;
// for the repair path it is worth a log line and nothing else, because the
// message that matters is the one that follows.
func (m *Manager) CompactConversation(ctx context.Context, id domain.SessionID, focus string) (bool, error) {
	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil {
		return false, fmt.Errorf("compact %s: session: %w", id, err)
	}
	if !ok {
		return false, fmt.Errorf("compact %s: %w", id, ErrNotFound)
	}
	directive, ok := m.compactionDirectiveFor(rec.Harness, focus)
	if !ok {
		return false, nil
	}
	// Through SendReportingSubmission rather than Send: a compaction directive
	// left sitting in a composer is worse than one never sent, because the
	// fix prompt that follows would land UNDERNEATH it in the same draft. A
	// verdict of loaded-not-submitted is reported as not-requested so the
	// caller's own guard is what decides, and the submit retry the workflow
	// path already owns is not duplicated here.
	submission, err := m.SendReportingSubmission(ctx, id, directive, nil)
	if err != nil {
		return false, fmt.Errorf("compact %s: %w", id, err)
	}
	if submission == ports.PromptLoadedNotSubmitted {
		return false, fmt.Errorf("compact %s: %w", id, ports.ErrPromptUndelivered)
	}
	return true, nil
}

// compactionDirectiveFor resolves the harness's own compaction vocabulary.
// ok=false for every harness that has none -- which is every harness that does
// not implement ports.ConversationCompactor, and that is the safe default: a
// directive a harness does not understand is a prompt that wastes a turn
// saying something meaningless.
func (m *Manager) compactionDirectiveFor(harness domain.AgentHarness, focus string) (string, bool) {
	if m.agents == nil {
		return "", false
	}
	agent, ok := m.agents.Agent(harness)
	if !ok {
		return "", false
	}
	compactor, ok := agent.(ports.ConversationCompactor)
	if !ok {
		return "", false
	}
	directive, ok := compactor.CompactionDirective(focus)
	if !ok {
		return "", false
	}
	if strings.TrimSpace(directive) == "" {
		return "", false
	}
	return directive, true
}
