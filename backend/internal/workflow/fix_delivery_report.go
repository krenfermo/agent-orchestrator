package workflow

import (
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// fix_delivery_report.go — the read-time projection that makes a fix attempt
// diagnosable without grepping ~/.ao.
//
// Everything below already existed on the ledger; none of it was reachable. The
// dispatch records live in workflow_checkpoints.retry_state, which GetRun
// decodes for exactly one phase (verify_result) and passes over for every
// other. So an operator facing "fix worker idle with no verifiable new change"
// could see the stop sentence and nothing about the delivery that preceded it:
// not the review run that authorized it, not whether the findings were in the
// prompt, not whether the agent was ever given the turn.
//
// This is a projection, not a new source of truth. It reads back what dispatch
// already wrote, verbatim, and invents nothing — an unknown field stays empty
// rather than being estimated.

// FixDeliveryState is the durable phase of the newest delivery record for a fix
// step, in the vocabulary an operator needs rather than the storage one.
type FixDeliveryState string

const (
	// FixDeliveryStateIntent: AO wrote the pre-delivery record and has not yet
	// recorded an outcome. After a restart this is the window recovery reasons
	// about; steady-state it means the send is in flight.
	FixDeliveryStateIntent FixDeliveryState = "intent_recorded"
	// FixDeliveryStateDispatched: the prompt was delivered and the cycle's
	// attempt row exists.
	FixDeliveryStateDispatched FixDeliveryState = "dispatched"
	// FixDeliveryStateTransportRetry: the transport refused the prompt before
	// any of it reached the agent; AO is re-sending on its own.
	FixDeliveryStateTransportRetry FixDeliveryState = "transport_retry"
	// FixDeliveryStateNotSubmitted: the prompt is in the agent's composer and
	// was never submitted.
	FixDeliveryStateNotSubmitted FixDeliveryState = "loaded_not_submitted"
)

// FixDeliveryReport is everything AO can say, from durable state alone, about
// the most recent fix cycle dispatched for one fix step.
//
// It deliberately carries no prompt text. The reviewer's findings are already
// durable on the review run and already surfaced (truncated) as the review
// step's FindingsSummary; what is added here is the binding — digest, count,
// size, and whether those exact bytes were embedded in the delivered prompt.
type FixDeliveryReport struct {
	// State is the newest recorded delivery phase for this step.
	State FixDeliveryState
	// DispatchedAt is when that record was written.
	DispatchedAt time.Time

	// --- what authorized this fix cycle ---

	// ReviewRunID is the review run the cycle is bound to; ReviewVerdict is the
	// effective verdict that triggered it and ReviewTargetSHA the commit it was
	// reviewed against. Together they are the review generation.
	ReviewRunID     string
	ReviewVerdict   string
	ReviewTargetSHA string

	// --- which findings travelled ---

	// FindingsSource is "review_run" or "verification".
	FindingsSource string
	// FindingsCount, FindingsBytes and FindingsDigest identify the payload.
	FindingsCount  int
	FindingsBytes  int
	FindingsDigest string
	// FindingsEmbedded is the answer to "did the worker actually receive the
	// complete reviewer finding": whether the exact findings bytes appear
	// verbatim in the prompt that was delivered.
	FindingsEmbedded bool
	// FindingsSnippet is a bounded head of those findings.
	FindingsSnippet string

	// --- the fix attempt itself ---

	// CycleNumber is the fix generation; FixAttemptID names the workflow_attempt
	// row this delivery produced.
	CycleNumber      int
	FixAttemptID     string
	TransportAttempt int
	// SessionID is the worker session the prompt was written into.
	SessionID string

	// --- what the transport could prove ---

	PromptBytes   int
	Transport     ports.PromptTransport
	ContextPack   bool
	PromptReceipt string
	// Submission is whether the payload left the agent's composer. Empty means
	// the transport in use could not report it, never that it failed.
	Submission ports.PromptSubmission
	// Acknowledged is the plain reading of Submission: AO can prove the agent
	// was given the turn. False includes "could not be checked".
	Acknowledged bool
	// ReceiptMatch compares the prompt AO DELIVERED against the prompt the
	// worker session actually recorded receiving: "match", "other", "none"
	// (the session recorded no prompt), or "" (not checked).
	//
	// This is the field incident wf-a816d7fe needed and no surface had. Every
	// other signal there was green — the transport exited 0 and Submission was
	// `submitted` — while the agent had in fact received the last 510 bytes of
	// a 4600-byte prompt, because tmux was pasting without bracketed-paste
	// markers and the agent's TUI read each CR as Enter. "other" is exactly
	// that condition, stated as a fact: AO and the agent do not hold the same
	// bytes.
	//
	// It is observability, not authority: nothing branches on it. A delivery
	// whose receipt cannot be read reports "" and every existing decision is
	// left precisely as it was.
	ReceiptMatch string

	// --- how it ended ---

	// TerminalErrorClass is the fix step's latest attempt's error class (e.g.
	// ambiguous_worker_state), empty while the attempt is still open.
	TerminalErrorClass domain.WorkflowErrorClass
	// TerminalOutcome is that attempt's outcome, empty while it is open.
	TerminalOutcome domain.WorkflowAttemptOutcome
	// NextAction is the newest recorded next_action for this step — the stop
	// sentence, when the step is stopped.
	NextAction string
	// Reason is the delivery record's own note, when one was recorded (a
	// transport refusal's cause, or "delivery proven after restart").
	Reason string
}

// fixDeliveryPhases are the durable phases that represent a delivery of some
// fix cycle. All four carry a promptDeliveryRecord.
func isFixDeliveryPhase(phase string) (FixDeliveryState, bool) {
	switch phase {
	case fixDispatchIntentPhase:
		return FixDeliveryStateIntent, true
	case fixDispatchedPhase:
		return FixDeliveryStateDispatched, true
	case fixTransportRetryPhase:
		return FixDeliveryStateTransportRetry, true
	case fixPromptNotSubmittedPhase:
		return FixDeliveryStateNotSubmitted, true
	}
	return "", false
}

// BuildFixDeliveryReport projects the newest delivery record for a fix step out
// of the run's checkpoints, plus the terminal facts from its latest attempt.
//
// Pure: it takes the rows and returns the projection, so the derivation is
// testable without a store and cannot itself write anything. Returns nil when
// the step has never had a delivery recorded, which is the honest answer for a
// fix step that was never dispatched.
func BuildFixDeliveryReport(step domain.WorkflowStep, attempts []domain.WorkflowAttempt, cps []domain.WorkflowCheckpoint) *FixDeliveryReport {
	if step.Kind != domain.WorkflowStepFix {
		return nil
	}
	var (
		newest domain.WorkflowCheckpoint
		state  FixDeliveryState
		found  bool
	)
	// The newest next_action for this step, from any phase — the stop sentence
	// usually lives on a later observation row than the delivery it describes.
	var (
		actionAt time.Time
		action   string
	)
	for _, cp := range cps {
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != step.ID {
			continue
		}
		if cp.NextAction != "" && (action == "" || !cp.CreatedAt.Before(actionAt)) {
			action, actionAt = cp.NextAction, cp.CreatedAt
		}
		phase, ok := isFixDeliveryPhase(cp.DurablePhase)
		if !ok {
			continue
		}
		if !found || !cp.CreatedAt.Before(newest.CreatedAt) {
			newest, state, found = cp, phase, true
		}
	}
	if !found {
		return nil
	}

	rec := promptDeliveryRecordFromJSON(newest.RetryState)
	report := &FixDeliveryReport{
		State:            state,
		DispatchedAt:     newest.CreatedAt,
		ReviewRunID:      rec.Findings.ReviewRunID,
		ReviewVerdict:    rec.Findings.ReviewVerdict,
		ReviewTargetSHA:  rec.Findings.ReviewTargetSHA,
		FindingsSource:   rec.Findings.Source,
		FindingsCount:    rec.Findings.Count,
		FindingsBytes:    rec.Findings.Bytes,
		FindingsDigest:   rec.Findings.Digest,
		FindingsEmbedded: rec.Findings.Embedded,
		FindingsSnippet:  rec.Findings.Snippet,
		CycleNumber:      rec.CycleNumber,
		FixAttemptID:     rec.FixAttemptID,
		TransportAttempt: rec.TransportAttempt,
		PromptBytes:      rec.PromptBytes,
		Transport:        rec.Transport,
		ContextPack:      rec.ContextPack,
		PromptReceipt:    rec.PromptReceipt,
		Submission:       rec.Submission,
		Acknowledged:     rec.Submission == ports.PromptSubmitted,
		NextAction:       action,
		Reason:           rec.Reason,
	}
	// The checkpoint columns are the fallback, not the source: a record written
	// before these fields existed still names its review run and verdict there.
	if report.ReviewRunID == "" && newest.ReviewRunID != nil {
		report.ReviewRunID = *newest.ReviewRunID
	}
	if report.ReviewVerdict == "" {
		report.ReviewVerdict = newest.ReviewVerdict
	}
	if report.ReviewTargetSHA == "" {
		report.ReviewTargetSHA = newest.FingerprintBefore
	}
	if newest.SessionID != nil {
		report.SessionID = *newest.SessionID
	}
	if len(attempts) > 0 {
		latest := attempts[len(attempts)-1]
		report.TerminalErrorClass = latest.ErrorClass
		report.TerminalOutcome = latest.Outcome
		if report.FixAttemptID == "" {
			report.FixAttemptID = latest.ID
		}
	}
	return report
}
