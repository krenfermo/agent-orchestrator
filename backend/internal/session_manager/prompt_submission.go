package sessionmanager

import (
	"context"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// prompt_submission.go — Checkpoint 8P-E.17.
//
// Send used to answer exactly one question — "did the transport commands
// succeed" — and callers had no choice but to read that as "the agent has the
// prompt". Incident wf-57f90ff2 is the gap between those two: a fix prompt was
// pasted into a pane in tmux copy-mode, the submitting Enter was swallowed by
// the mode, every command exited 0, and the prompt sat in Codex's composer as
// an unsubmitted draft. AO recorded a delivered fix cycle, then stopped the run
// because the worker had not made the change it had never been asked to make.
//
// The transport-level cause is fixed where it belongs (the tmux adapter now
// refuses to write into a pane whose keys it cannot deliver — see
// ensurePaneAcceptsKeys). This file closes the reporting gap that let the
// mistake go unnoticed: after writing a prompt, AO checks whether the composer
// actually emptied, and says which of the three things it can prove.
//
// Why the composer and not the activity clock: an agent's submit hook is the
// authoritative "a turn started" signal, but it is asynchronous, harness
// specific and — for exactly the harnesses this incident involves — not
// something Send may wait on. The composer is the direct observable: the
// payload is either still in the draft or it is not. AO already has a
// per-harness reader for it (ports.EmptyComposerDetector), built for the
// adjacent safety question "is there a human draft here", and reusing it means
// no new terminal-scraping heuristic enters the codebase.
//
// The check never fails a send. The bytes are already written by the time it
// runs; its whole job is to tell the caller what happened so the caller can
// decide, and a caller that ignores it is exactly as correct as it was before.

const (
	// submissionProbeTimeout bounds the whole confirmation. It is small on
	// purpose: this is a question about the terminal's current contents, not
	// about how long the agent takes to think.
	submissionProbeTimeout = 3 * time.Second
	// submissionProbeInterval paces the re-reads within that budget. The poll
	// exists because a TUI redraws its composer a frame or two after receiving
	// the submit — it is waiting on an observable predicate, not sleeping for a
	// guessed duration, and it returns the moment the predicate holds.
	submissionProbeInterval = 150 * time.Millisecond
)

// SendReportingSubmission is Send plus the submission verdict.
//
// It exists as a separate entry point rather than a changed Send signature so
// the many callers that legitimately do not care (a human's `ao send`, an
// orchestrator relay) are untouched, and the ones that must not mistake a
// draft for a delivery — the workflow's fix dispatch above all — can ask.
func (m *Manager) SendReportingSubmission(ctx context.Context, id domain.SessionID, message string, attachment *ports.SpawnAttachment) (ports.PromptSubmission, error) {
	if err := m.Send(ctx, id, message, attachment); err != nil {
		return ports.PromptSubmissionUnset, err
	}
	if strings.TrimSpace(message) == "" {
		// A nudge carries no payload, so there is no draft to look for and
		// nothing this check could mean.
		return ports.PromptSubmissionUnset, nil
	}
	return m.confirmPromptSubmitted(ctx, id), nil
}

// SubmitPending re-issues ONLY the submit, for a prompt AO has already written
// into the session's composer and durably recorded as loaded_not_submitted.
//
// This is the safe half of a retry, and the distinction is the entire reason it
// exists as its own method. Re-sending the prompt would paste a second copy
// into a draft that already holds the first — which is exactly what happened
// to session agent-orchestrator-29, whose composer ended up carrying the same
// 15 KB fix prompt twice. Submitting what is already there delivers the
// instructions once, deletes nothing, and cannot touch a human's draft: the
// caller must have durable evidence that the pending content is AO's own.
func (m *Manager) SubmitPending(ctx context.Context, id domain.SessionID) (ports.PromptSubmission, error) {
	// The empty message is AO's existing nudge contract: Enter alone, no paste.
	if err := m.Send(ctx, id, "", nil); err != nil {
		return ports.PromptSubmissionUnset, err
	}
	return m.confirmPromptSubmitted(ctx, id), nil
}

// ComposerState reports what the session's composer currently holds, without
// writing anything at all.
//
// It is the pre-flight half of the duplicate-prompt guarantee: a caller about
// to deliver can ask whether a draft is already pending and, when its own
// durable record attributes that draft to itself, submit it instead of pasting
// a second copy over the top. PromptSubmitted here means "the composer is
// empty" — nothing is waiting.
func (m *Manager) ComposerState(ctx context.Context, id domain.SessionID) ports.PromptSubmission {
	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil || !ok {
		return ports.PromptSubmissionAmbiguous
	}
	detector, ok := m.composerDetectorFor(rec.Harness)
	if !ok {
		return ports.PromptSubmissionAmbiguous
	}
	handle := ports.RuntimeHandle{ID: strings.TrimSpace(rec.Metadata.RuntimeHandleID)}
	if handle.ID == "" {
		return ports.PromptSubmissionAmbiguous
	}
	empty, err := m.composerIsEmpty(ctx, handle, detector)
	if err != nil {
		return ports.PromptSubmissionAmbiguous
	}
	if empty {
		return ports.PromptSubmitted
	}
	return ports.PromptLoadedNotSubmitted
}

// confirmPromptSubmitted answers whether the composer emptied after a submit.
//
// Every branch that cannot see the composer returns Ambiguous rather than
// guessing. That is deliberate and asymmetric on purpose: reporting a false
// "submitted" reproduces the incident, while reporting a false "ambiguous"
// costs a caller one extra, harmless verification later.
func (m *Manager) confirmPromptSubmitted(ctx context.Context, id domain.SessionID) ports.PromptSubmission {
	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil || !ok {
		return ports.PromptSubmissionAmbiguous
	}
	detector, ok := m.composerDetectorFor(rec.Harness)
	if !ok {
		return ports.PromptSubmissionAmbiguous
	}
	handle := ports.RuntimeHandle{ID: strings.TrimSpace(rec.Metadata.RuntimeHandleID)}
	if handle.ID == "" {
		return ports.PromptSubmissionAmbiguous
	}

	deadline := m.clock().Add(submissionProbeTimeout)
	ticker := time.NewTicker(submissionProbeInterval)
	defer ticker.Stop()
	sawComposer := false
	for {
		empty, err := m.composerIsEmpty(ctx, handle, detector)
		if err == nil {
			sawComposer = true
			if empty {
				// The payload is out of the composer: the submit landed.
				return ports.PromptSubmitted
			}
		}
		if !m.clock().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ports.PromptSubmissionAmbiguous
		case <-ticker.C:
		}
	}
	if !sawComposer {
		// Never managed to read the composer at all — no evidence either way.
		return ports.PromptSubmissionAmbiguous
	}
	// Read it, repeatedly, and it kept holding content the submit should have
	// consumed. That is a fact about a draft, not an absence of information.
	return ports.PromptLoadedNotSubmitted
}

// composerDetectorFor resolves the harness's composer reader, if it has one.
func (m *Manager) composerDetectorFor(harness domain.AgentHarness) (ports.EmptyComposerDetector, bool) {
	if m.agents == nil {
		return nil, false
	}
	agent, ok := m.agents.Agent(harness)
	if !ok {
		return nil, false
	}
	detector, ok := agent.(ports.EmptyComposerDetector)
	return detector, ok
}
