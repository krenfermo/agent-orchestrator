package workflow

import (
	stdctx "context"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// session_compaction.go -- Checkpoint P7. Making COMPACT compact.
//
// Checkpoint 8M gave the lifecycle policy three actions and described the
// middle one as reusing the current session "but with a fresh, fact-only
// SessionContextPack instead of relying purely on accumulated conversation
// state". Only the second half was ever built. applyFixLifecycleDecision
// prepends the pack and says so in its own doc comment -- "Never changes which
// session receives the prompt" -- so the accumulated conversation the action
// was defined against went on accumulating. On wf-1c2cb9bd the third repair
// cycle received its fact pack at call 175, against a conversation of 294,234
// tokens, and the 19 calls that followed each paid for all of it.
//
// Two things were missing and both are here.
//
// THE SIGNAL. SessionLifecycleRequest.ContextPressure has carried a comment
// since 8M saying no production call site sets it because "AO has no
// per-session live token/context signal yet (confirmed by the 8L audit)". The
// usage pipeline has placed calls in time since P3-E and P5 turned that into a
// trajectory; domain.SessionContextReading is the same rows folded to the two
// figures a decision needs. The socket was left deliberately; this plugs it.
//
// THE ACT. Compaction itself is a request AO makes of the harness, because AO
// does not hold the conversation -- see ports.ConversationCompactor. It is
// delivered through the ordinary guarded transport, durably recorded BEFORE it
// is attempted so a crash cannot repeat it, and it is entirely best-effort: a
// harness that cannot compact, a transport that refuses, a directive that
// leaves the composer unsubmitted all land in the same place, which is the
// behaviour this file replaced. The fix prompt and its fact pack follow either
// way, unchanged and complete.

// SessionContextFacts is the narrow read that answers "how big is this
// session's conversation right now". Optional: a nil one means every lifecycle
// decision sees an unobservable reading, which is exactly the state the policy
// already handled before the signal existed.
type SessionContextFacts interface {
	GetSessionContextReading(ctx stdctx.Context, sessionID string) domain.SessionContextReading
}

// ConversationCompactingSender is the OPTIONAL capability a MessageSender may
// carry: asking the agent behind a session to compact its own conversation.
// Satisfied by *session_manager.Manager.
//
// Discovered by type assertion, the same way SubmissionReportingSender is, so
// a sender that does not implement it behaves exactly as it always did and the
// workflow package never learns which harnesses have a compaction vocabulary.
type ConversationCompactingSender interface {
	MessageSender
	CompactConversation(ctx stdctx.Context, id domain.SessionID, focus string) (bool, error)
}

// sessionCompactionDurablePhase is the checkpoint phase recording that AO
// asked a session to compact. It is written STRICTLY BEFORE the request is
// attempted, so a crash between the two loses the compaction rather than
// repeating it on recovery -- the safe direction, because a lost compaction
// costs money and a repeated one costs a turn and a conversation nobody can
// reconstruct.
const sessionCompactionDurablePhase = "session_compaction_requested"

// compactionFocus is the plain-language hint handed to the harness's own
// summarizer. Deliberately short and deliberately not the facts themselves:
// the facts travel in the SessionContextPack, in full, in the message that
// follows, so this text never has to be trusted to preserve anything.
const compactionFocus = "Keep the code you have already changed in this session, " +
	"the tests you have run and their results, the decisions you made and why, " +
	"and anything still unresolved. Drop the exploration that led to them."

// maybeCompactBeforeFix asks the session to compact when the lifecycle decision
// says COMPACT and everything needed to ask is present. It returns whether a
// compaction was actually requested, for the audit record.
//
// Every failure mode returns false and no error. This is an optimization
// wrapped around a delivery that must happen regardless: a compaction AO could
// not perform must never become a fix cycle AO did not deliver.
func (c *Coordinator) maybeCompactBeforeFix(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	fixStep domain.WorkflowStep,
	sessionID domain.SessionID,
	decision domain.SessionLifecycleDecision,
	cycleNumber int,
) bool {
	if decision.Action != domain.LifecycleCompact || sessionID == "" {
		return false
	}
	if !policyForRun(run).SessionCompactionEnabled {
		return false
	}
	sender, ok := c.messageSender.(ConversationCompactingSender)
	if !ok {
		return false
	}
	if c.sessionCompactionAlreadyRequested(ctx, run.ID, fixStep.ID, cycleNumber) {
		return false
	}
	// Durable first. A record written for a request that then failed costs one
	// skipped optimization; a request made without a record could be made
	// again by recovery, against a conversation that no longer looks like the
	// one the decision was taken on.
	if err := c.recordSessionCompactionRequest(ctx, run, fixStep, sessionID, cycleNumber); err != nil {
		if c.log != nil {
			c.log.Info("workflow: skipping compaction, its record could not be written",
				"run", run.ID, "step", fixStep.ID, "cycle", cycleNumber, "error", err)
		}
		return false
	}
	requested, err := sender.CompactConversation(ctx, sessionID, compactionFocus)
	if err != nil {
		if c.log != nil {
			c.log.Info("workflow: compaction request was refused; delivering the fix cycle unchanged",
				"run", run.ID, "step", fixStep.ID, "cycle", cycleNumber, "error", err)
		}
		return false
	}
	if !requested && c.log != nil {
		c.log.Debug("workflow: this harness has no compaction vocabulary",
			"run", run.ID, "step", fixStep.ID, "cycle", cycleNumber)
	}
	return requested
}

// sessionCompactionAlreadyRequested reports whether this exact cycle already
// has a compaction record. A read failure returns true -- refusing to compact
// on no information, rather than risking a second directive into a
// conversation whose state AO cannot currently read.
func (c *Coordinator) sessionCompactionAlreadyRequested(ctx stdctx.Context, runID, stepID string, cycleNumber int) bool {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return true
	}
	want := compactionRecordMarker(stepID, cycleNumber)
	for _, cp := range cps {
		if cp.DurablePhase == sessionCompactionDurablePhase && strings.Contains(cp.RetryState, want) {
			return true
		}
	}
	return false
}

// compactionRecordMarker identifies one cycle's compaction record inside the
// checkpoint payload. Derived from the step and the cycle, never from a clock,
// so recovery reconstructs the same marker rather than writing a second one.
//
// The TRAILING COMMA is load-bearing. Without it the marker for cycle 1 is a
// prefix of the payload for cycle 10, so a run whose policy allows ten repair
// cycles would find cycle 10's record while looking for cycle 1's and skip a
// compaction it had never performed. The payload writes the session field
// immediately after the cycle, so the comma is always there to match.
func compactionRecordMarker(stepID string, cycleNumber int) string {
	return `"step":"` + stepID + `","cycle":` + itoaInt(cycleNumber) + `,`
}

func (c *Coordinator) recordSessionCompactionRequest(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	fixStep domain.WorkflowStep,
	sessionID domain.SessionID,
	cycleNumber int,
) error {
	sid := string(sessionID)
	payload := `{` + compactionRecordMarker(fixStep.ID, cycleNumber) +
		`"session":"` + sid + `","policyVersion":"` + domain.SessionLifecyclePolicyVersion + `"}`
	_, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		ProjectID:      run.ProjectID,
		SessionID:      &sid,
		RetryState:     payload,
		DurablePhase:   sessionCompactionDurablePhase,
		PayloadVersion: domain.SessionLifecyclePolicyVersion,
		CreatedAt:      c.clock(),
	})
	return err
}

// itoaInt is strconv.Itoa without the import, matching the tiny local helpers
// this package already uses for checkpoint payload assembly.
func itoaInt(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// CompactionRecordMarkerForTest exposes compactionRecordMarker to the external
// workflow_test package, matching this package's existing ...ForTest
// convention (see DecodeSessionLifecycleDecisionForTest).
func CompactionRecordMarkerForTest(stepID string, cycleNumber int) string {
	return compactionRecordMarker(stepID, cycleNumber)
}

// --- the opt-in -------------------------------------------------------------
//
// Everything above is the ACT. What follows is the only way a production run
// can ever ask for it.
//
// Both knobs are frozen through the same `pending` precondition every other
// per-run policy uses (ApplyRepairPolicy, ApplyAutonomyPolicy,
// ApplyReviewDepthPolicy), and for a sharper reason than those have. A
// compaction flag flipped mid-run would apply to a conversation that has
// already grown under the opposite contract, and the context-pressure
// threshold is the input to the lifecycle decision that reads it -- moving
// either one after a fix cycle has been dispatched would make the run's own
// audit record a description of a rule that was no longer in force when it
// mattered.

// ApplySessionCompactionPolicy freezes a just-created run's explicit
// session-compaction choice and records who made it.
//
// An explicit FALSE is written exactly as an explicit true is, and that is the
// point rather than an edge case: it is what makes "this run was told not to
// compact" a fact the run carries, distinguishable from "nobody said", and it
// is what a controlled A/B needs so the control arm is a recorded decision
// instead of an absence. It is also what inheritance keys on -- see
// domain.InheritWorkflowPolicy.
func (c *Coordinator) ApplySessionCompactionPolicy(ctx stdctx.Context, runID string, enabled bool, requestedBy string) error {
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	if run.State != domain.WorkflowRunPending {
		return fmt.Errorf("%w: workflow run %q is already %s; its session-compaction choice is frozen", ErrInvalid, runID, run.State)
	}
	return c.rewriteFrozenPolicy(ctx, run, func(p *domain.WorkflowPolicy) {
		p.SessionCompactionEnabled = enabled
		p.CompactionProvenance = domain.SessionCompactionProvenance{
			Version:     domain.SessionCompactionPolicyVersion,
			Source:      domain.SessionCompactionExplicit,
			RequestedBy: strings.TrimSpace(requestedBy),
			At:          c.clock(),
		}
	})
}

// ApplyContextPerCallWarnTokens freezes a per-run override of the
// context-per-call threshold, leaving every other budget field alone.
//
// The value is validated HERE as well as at the API edge and again when it is
// read back, because this is the one advisory override a lifecycle decision
// acts on: sessionContextPressure compares a live conversation against it, and
// the COMPACT branch is what it gates. An out-of-range value is refused rather
// than clamped -- silently widening 1 to 20,000 would hand back a run whose
// threshold is not the one the caller named, which is exactly the kind of
// substitution a lab experiment cannot afford.
func (c *Coordinator) ApplyContextPerCallWarnTokens(ctx stdctx.Context, runID string, tokens int64) error {
	if !domain.ValidContextPerCallWarnTokens(tokens) {
		return fmt.Errorf("%w: contextPerCallWarnTokens %d is outside [%d, %d]",
			ErrInvalid, tokens, domain.MinContextPerCallWarnTokens, domain.MaxContextPerCallWarnTokens)
	}
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	if run.State != domain.WorkflowRunPending {
		return fmt.Errorf("%w: workflow run %q is already %s; its context-pressure threshold is frozen", ErrInvalid, runID, run.State)
	}
	return c.rewriteFrozenPolicy(ctx, run, func(p *domain.WorkflowPolicy) {
		// EffectiveUsageBudgetPolicy, not p.Usage: it fills in Version and the
		// ParentScope default, so a run whose budget nobody had configured
		// ends up with a snapshot inheritance recognises as recorded rather
		// than one carrying a lone advisory number and no version.
		usage := p.EffectiveUsageBudgetPolicy()
		usage.WorkflowContextPerCallWarnTokens = tokens
		p.Usage = usage
	})
}
