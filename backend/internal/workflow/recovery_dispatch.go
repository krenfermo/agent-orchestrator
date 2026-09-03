package workflow

import (
	stdctx "context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
)

// recovery_dispatch.go — P3-C §16: the half of the Advisor that is allowed to
// act, kept structurally apart from the half that decides.
//
// advice.go is a pure function. It can say "AO should launch a repair here" and
// it cannot launch one, which is what makes it safe to compute on every board
// poll and every page load. This file is the other side: it re-derives the same
// advice, checks that AO still has the authority the advice assumed, and only
// then performs the ONE automatic action the model allows it to perform.
//
// Why re-derive rather than take the advice as an argument: the advice a caller
// is holding was computed at some earlier moment, and between then and now the
// run may have been repaired, cancelled, continued by somebody else, or moved
// past the stop entirely. An action taken from a stale reading is exactly the
// duplicate side effect §15 forbids.

// ErrRecoveryActionSuperseded is the answer to a click that arrived after the
// situation moved: another actor already started the same remedy, or the stop
// the action was computed against is no longer the run's stop.
//
// It is deliberately a distinct sentinel rather than a generic conflict: the
// honest thing to tell a person is "AO is already repairing this", and a
// surface cannot say that from ErrInvalid.
var ErrRecoveryActionSuperseded = errors.New("workflow: the run moved past the state this action was computed against")

// AutomaticRecoveryOutcome is what one dispatch pass actually did. Every field
// is a fact rather than an intention, so a caller (the wake poller, a test, an
// operator's CLI) can report the truth without inferring it.
type AutomaticRecoveryOutcome struct {
	RunID string
	// Action is the automatic action the advice named. Empty means AO had
	// nothing of its own to do, which is a successful outcome, not a failure.
	Action AutomaticActionID
	// Dispatched reports that this pass performed something. False for every
	// action that is already running under its own machinery (a repair in
	// flight, a scheduled retry, a capacity wait) — those are reported, never
	// re-driven.
	Dispatched bool
	// RepairRunID names the repair generation this pass created, when it
	// created one.
	RepairRunID string
	// Detail is AO's own sentence about what happened, for the log and the CLI.
	Detail string
}

// DispatchAutomaticRecovery performs whatever AO is authorized to do about a
// run by itself, and nothing else.
//
// It is the ONLY caller of an automatic remedy outside boot reconciliation, and
// it is bounded three ways before it does anything: the run must still be
// stopped, the advice must still name an automatic action, and that action's
// own authority (the frozen repair policy, the repair budget, the single-flight
// claim) must still hold. Each of those is re-read here rather than trusted
// from the caller.
//
// Idempotent by construction: the only action it performs, LaunchRepair, is
// itself single-flighted over the evidence digest of the stop, so two
// concurrent dispatches converge on one repair generation instead of two.
func (c *Coordinator) DispatchAutomaticRecovery(ctx stdctx.Context, runID string) (AutomaticRecoveryOutcome, error) {
	out := AutomaticRecoveryOutcome{RunID: runID}
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return out, err
	}
	if !ok {
		return out, fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	if run.State.Terminal() {
		out.Detail = "the run has ended; there is nothing to recover"
		return out, nil
	}

	advice, err := c.AdviceFor(ctx, runID)
	if err != nil {
		return out, err
	}
	out.Action = advice.AutomaticAction
	if advice.AutomaticActionActive {
		// Already happening under its own machinery. Reporting it is the whole
		// job; re-driving it is how one incident buys two repairs.
		out.Detail = "an automatic action is already in flight"
		return out, nil
	}

	switch advice.AutomaticAction {
	case AutoActionLaunchRepair:
		intent, lerr := c.LaunchRepair(ctx, runID, "")
		if lerr != nil {
			// A refusal here is normal rather than exceptional: the budget may
			// have been spent, or another actor may have taken the same repair
			// between the advice and this call. Both are recorded and neither
			// is an error the caller has to handle.
			if errors.Is(lerr, ErrRepairIneligible) || errors.Is(lerr, ErrInvalid) {
				out.Detail = "automatic repair was no longer authorized: " + lerr.Error()
				return out, nil
			}
			return out, lerr
		}
		out.Dispatched = true
		out.RepairRunID = intent.RepairRunID
		out.Detail = fmt.Sprintf("automatic repair generation %d launched", intent.Generation)
		c.recordRecoveryDispatch(ctx, run, advice, out)
		return out, nil
	case AutoActionNone:
		out.Detail = "AO has no automatic action for this run"
		return out, nil
	default:
		// Every other automatic action names machinery that drives itself — a
		// wake already scheduled, a provider failover inside a live dispatch, a
		// Decision Resolver already running. Dispatching them a second time
		// from here would be a second driver of one obligation.
		out.Detail = "the automatic action for this run is driven by its own machinery"
		return out, nil
	}
}

// recordRecoveryDispatch writes the durable fact that AO selected and carried
// out an automatic recovery (§17). It rides on the checkpoint ledger the
// incident lifecycle already uses rather than a table of its own, so there is
// one durable event model and not two.
func (c *Coordinator) recordRecoveryDispatch(ctx stdctx.Context, run domain.WorkflowRun, advice Advice, out AutomaticRecoveryOutcome) {
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:            "wfc-" + c.newID(),
		WorkflowRunID: run.ID,
		ProjectID:     run.ProjectID,
		NextAction: fmt.Sprintf("%s: %s (condition %s)",
			autoRecoveryDispatchedPhase, out.Detail, advice.ReasonCode),
		DurablePhase:   autoRecoveryDispatchedPhase,
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      c.clock(),
	}); err != nil && c.log != nil {
		c.log.Warn("workflow: recording the automatic recovery dispatch failed", "run", run.ID, "err", err)
	}
}

// autoRecoveryDispatchedPhase is the durable phase of the record above.
//
// It is deliberately NOT in attentionDispositions: it is a note about something
// AO DID, not a stop, and registering it there would make a successful recovery
// read as a new reason to stop.
const autoRecoveryDispatchedPhase = "auto_recovery_dispatched"

// scheduleAutoRecoveryWake schedules the durable wake that makes automatic
// repair actually automatic.
//
// Before P3-C the ONLY thing that ever called maybeAutoRepair was boot
// reconciliation, so a run that stopped on a repairable condition while the
// daemon stayed up sat there under an `automatic` policy waiting for somebody
// to press Repair — or for a restart. That is precisely the "Repair Automatic
// requires que el usuario pulse Repair" failure the completion bar names.
//
// A wake is the right carrier rather than an inline launch for two reasons.
// The stop is recorded from inside whatever transaction-shaped work produced
// it (sometimes a read path), and launching a repair agent from there would
// make a GET have a side effect. And a wake is durable: a daemon that dies
// between the stop and the repair still repairs, on the next poll, without
// needing the restart path to notice.
//
// Best-effort, exactly like every other wake this package schedules: a run that
// stops AND fails to schedule its recovery is no worse off than it was before
// this existed.
func (c *Coordinator) scheduleAutoRecoveryWake(ctx stdctx.Context, run domain.WorkflowRun, reason string) {
	if c.wakeScheduler == nil {
		return
	}
	disp, ok := attentionDispositions[reason]
	if !ok || !disp.Repairable {
		return
	}
	if policyForRun(run).EffectiveRepairPolicy().Mode != domain.RepairModeAutomatic {
		return
	}
	if _, err := c.wakeScheduler.Schedule(ctx, domain.WorkflowRunID(run.ID), nil,
		wake.ReasonAutoRecovery, nil); err != nil && c.log != nil {
		c.log.Warn("workflow: scheduling the automatic recovery wake failed",
			"run", run.ID, "reason", reason, "err", err)
	}
}

// ActionAuthorityMismatch names WHICH fact moved under a stale action click.
type ActionAuthorityMismatch string

const (
	// AuthorityMismatchNone means the action may proceed.
	AuthorityMismatchNone ActionAuthorityMismatch = ""
	// AuthorityMismatchRepairActive is the case §15 states by name: the UI
	// showed Repair, another actor started it, and the click arrived late.
	AuthorityMismatchRepairActive ActionAuthorityMismatch = "repair_active"
	// AuthorityMismatchStopChanged means the run is no longer stopped on the
	// condition the action was computed against.
	AuthorityMismatchStopChanged ActionAuthorityMismatch = "stop_changed"
	// AuthorityMismatchGeneration means a placement or repair generation moved.
	AuthorityMismatchGeneration ActionAuthorityMismatch = "generation_superseded"
	// AuthorityMismatchTerminal means the run ended.
	AuthorityMismatchTerminal ActionAuthorityMismatch = "run_terminal"
)

// RevalidateActionAuthority answers "is this action still the action AO
// offered", for a caller that captured an Advice.Authority and is now asking to
// execute something from it (§15).
//
// It is deliberately advisory rather than a lock: the executing paths
// (LaunchRepair's single-flight claim, ContinueRun's evidence gates, the
// placement generation CAS) are each individually idempotent already, so the
// worst a missed check produces is a no-op. What this adds is the ability to
// tell the person WHY nothing happened, which a silent no-op cannot.
//
// An empty expected authority means the caller captured none, and every check
// that has nothing to compare against passes — a client that does not send
// proof gets the pre-P3-C behaviour, never a refusal it cannot act on.
func (c *Coordinator) RevalidateActionAuthority(ctx stdctx.Context, runID string, action ActionID, expected AdviceAuthority) (ActionAuthorityMismatch, error) {
	current, err := c.AdviceFor(ctx, runID)
	if err != nil {
		return AuthorityMismatchNone, err
	}
	if current.Authority.RunState.Terminal() {
		return AuthorityMismatchTerminal, nil
	}
	// A second remedy while AO is already running one is the exact duplicate
	// this exists to refuse, and it is refused regardless of what the caller
	// captured: it is a fact about NOW, not a comparison.
	if current.AutomaticActionActive && isMutatingRecoveryAction(action) {
		return AuthorityMismatchRepairActive, nil
	}
	if expected == (AdviceAuthority{}) {
		return AuthorityMismatchNone, nil
	}
	if expected.RepairGeneration != 0 && expected.RepairGeneration != current.Authority.RepairGeneration {
		return AuthorityMismatchGeneration, nil
	}
	if expected.PlacementGeneration != 0 && expected.PlacementGeneration != current.Authority.PlacementGeneration {
		return AuthorityMismatchGeneration, nil
	}
	if expected.StopPhase != "" && expected.StopPhase != current.Authority.StopPhase {
		return AuthorityMismatchStopChanged, nil
	}
	if !expected.StopAt.IsZero() && !expected.StopAt.Equal(current.Authority.StopAt) {
		return AuthorityMismatchStopChanged, nil
	}
	return AuthorityMismatchNone, nil
}

// isMutatingRecoveryAction reports whether an action would change the run,
// which is the whole set §15 asks to revalidate. Read-only offers (open a
// session, view the diff, acknowledge a wait) are excluded: refusing to open a
// session because a repair started would be a refusal with no purpose.
func isMutatingRecoveryAction(a ActionID) bool {
	switch a {
	case ActionContinue, ActionRepair, ActionCommitAndContinue, ActionIntegrate,
		ActionRevalidatePlan, ActionRegeneratePlan, ActionUseIsolatedWorktree:
		return true
	default:
		return false
	}
}

// Describe renders one mismatch as the sentence a person should read.
func (m ActionAuthorityMismatch) Describe() string {
	switch m {
	case AuthorityMismatchRepairActive:
		return "AO is already repairing this run. Nothing was started a second time."
	case AuthorityMismatchStopChanged:
		return "This run is no longer stopped on the condition that action was offered for."
	case AuthorityMismatchGeneration:
		return "The run moved to a newer generation after that action was offered."
	case AuthorityMismatchTerminal:
		return "This run has ended."
	default:
		return ""
	}
}

// RecoveryStatusLine is `ao recover status <run>`'s whole output, as one value
// so the CLI, the API and a test all read the same words (§25).
type RecoveryStatusLine struct {
	Advice Advice
	// Headline is the one sentence to print.
	Headline string
	// ActionRequired is the imperative line, empty when nobody is needed.
	ActionRequired string
	// WaitUntil is echoed when AO is on a timer.
	WaitUntil *time.Time
}

// RecoveryStatus builds the CLI-shaped answer for one run. It writes nothing.
func (c *Coordinator) RecoveryStatus(ctx stdctx.Context, runID string) (RecoveryStatusLine, error) {
	advice, err := c.AdviceFor(ctx, runID)
	if err != nil {
		return RecoveryStatusLine{}, err
	}
	line := RecoveryStatusLine{Advice: advice, Headline: advice.Summary, WaitUntil: advice.WaitUntil}
	if advice.RequiresHuman {
		line.ActionRequired = strings.TrimSpace(advice.Explanation)
	}
	return line, nil
}
