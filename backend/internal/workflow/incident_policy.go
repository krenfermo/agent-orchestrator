package workflow

import "strings"

// incident_policy.go — Checkpoint 8P-E.18's authorization model.
//
// The Diagnostic Agent is an untrusted proposer. Everything it returns is text
// that arrived from a language model, and this file is the boundary that turns
// some of that text into permission to act. The rules it enforces:
//
//  1. AO's action vocabulary is CLOSED. An action whose kind is not in this
//     file cannot be proposed, approved or executed — an agent cannot widen
//     what AO is able to do by inventing a string.
//  2. No action is destructive. There is no reset, no stash, no discard, no
//     branch deletion, no force anything, no database write, no credential
//     access, and no "run this shell command". Those are absent by
//     construction rather than gated, because a gate on a capability that
//     exists is one bug away from being open.
//  3. The Diagnostic Agent cannot approve itself. Separation of duties is
//     structural: diagnosis and execution are different calls, with different
//     preconditions, and the only actions that skip a human are the ones whose
//     blast radius is provably nil — see autoAuthorized.
//  4. Anything that writes code goes to a Repair Agent, then to an INDEPENDENT
//     reviewer, then to deterministic verification. The diagnostician never
//     writes the fix, and the repairer never reviews it.

// IncidentActionKind is the closed set of things the Advisor can propose.
type IncidentActionKind string

const (
	// IncidentActionNone means nothing to do. The honest proposal for a
	// human_decision_required or unsafe_or_insufficient_evidence diagnosis,
	// and the default when an agent proposes something unrecognised.
	IncidentActionNone IncidentActionKind = "none"

	// IncidentActionContinueRun re-enters ContinueRun for this run.
	//
	// This is the ONLY state-advancing action the Advisor has, and that is
	// deliberate: it is the same button a person already has, it re-derives its
	// own evidence at call time, every resume rule inside it is individually
	// bounded, and it is a no-op for any run whose durable shape does not match
	// what those rules require. Proposing it can therefore never do more than a
	// person clicking Reanudar would, which is what makes it safe to authorize
	// without asking.
	IncidentActionContinueRun IncidentActionKind = "continue_run"

	// IncidentActionRepairAgent launches a Repair Agent against AO's own source
	// in an isolated workspace. It changes code, so it always needs a person.
	IncidentActionRepairAgent IncidentActionKind = "repair_agent"

	// IncidentActionCancelRun cancels the run. Not destructive to the worktree,
	// but it ends work, so it always needs a person.
	IncidentActionCancelRun IncidentActionKind = "cancel_run"

	// IncidentActionAwaitHuman means the remedy is something only the person can do
	// outside AO (install a tool, fix credentials, resolve their own dirty
	// worktree). AO records the advice and does nothing.
	IncidentActionAwaitHuman IncidentActionKind = "await_human"
)

// incidentActionRisk grades an action for the approval decision and for what
// the modal tells the user.
type incidentActionRisk string

const (
	riskNone     incidentActionRisk = "none"
	riskLow      incidentActionRisk = "low"
	riskModerate incidentActionRisk = "moderate"
	riskHigh     incidentActionRisk = "high"
)

// incidentActionPolicy is one action's complete authorization record.
type incidentActionPolicy struct {
	// Risk is what the user is shown.
	Risk incidentActionRisk
	// AutoAuthorized means AO may execute it without asking. True for exactly
	// one action, for the reason documented on IncidentActionContinueRun.
	AutoAuthorized bool
	// WritesCode routes the action through Repair Agent -> independent review
	// -> deterministic verify.
	WritesCode bool
	// EndsWork marks an action that stops work in progress, so the modal must
	// say what is lost before anyone confirms.
	EndsWork bool
	// Describe is AO's own words for what will happen. The agent's `Detail` is
	// shown as its rationale, never as the description of the mechanism — a
	// proposer does not get to narrate what AO will do.
	Describe string
}

// incidentActionPolicies is the allow-list. Membership is the entire definition
// of "an action AO is able to take about an incident".
var incidentActionPolicies = map[IncidentActionKind]incidentActionPolicy{
	IncidentActionNone: {
		Risk: riskNone, AutoAuthorized: true,
		Describe: "Nothing is executed.",
	},
	IncidentActionContinueRun: {
		Risk: riskLow, AutoAuthorized: true,
		Describe: "Re-enter this run's normal continue path. AO re-checks its own durable evidence first and does nothing unless that evidence still holds; it never edits the worktree.",
	},
	IncidentActionRepairAgent: {
		Risk: riskHigh, WritesCode: true,
		Describe: "Launch a separate Repair Agent against AO's own source in an isolated workspace. Its output is reviewed by an independent reviewer and verified by deterministic checks before anything is adopted.",
	},
	IncidentActionCancelRun: {
		Risk: riskModerate, EndsWork: true,
		Describe: "Cancel this run. Work already committed is kept; work in progress in its session is not resumed.",
	},
	IncidentActionAwaitHuman: {
		Risk: riskNone, AutoAuthorized: true,
		Describe: "Nothing is executed. AO records what you need to do outside it.",
	},
}

// lookupIncidentAction resolves a proposed action against the allow-list.
//
// An unrecognised kind is not an error and not a rejection of the whole
// diagnosis: it degrades to IncidentActionNone. A diagnosis whose prose is
// useful and whose proposed verb is nonsense is still worth showing a person —
// what must never happen is AO executing the nonsense.
func lookupIncidentAction(kind IncidentActionKind) (IncidentActionKind, incidentActionPolicy) {
	if policy, ok := incidentActionPolicies[kind]; ok {
		return kind, policy
	}
	return IncidentActionNone, incidentActionPolicies[IncidentActionNone]
}

// authorizeIncidentAction is the single gate every execution passes through.
//
// It answers two things at once, and both are needed before anything runs:
// whether this action is allowed for this classification at all, and whether a
// person must say yes first.
func authorizeIncidentAction(class IncidentClass, kind IncidentActionKind, approvedBy string) (bool, string) {
	resolved, policy := lookupIncidentAction(kind)
	if resolved != kind {
		return false, "AO does not recognise the proposed action, so it will not run it"
	}
	if resolved == IncidentActionNone || resolved == IncidentActionAwaitHuman {
		return false, "this diagnosis proposes no executable action"
	}

	// Class/action compatibility. The point is that a classification cannot be
	// used to smuggle in an action from another one: an agent that says
	// "auto_recoverable" and proposes a code change has contradicted itself,
	// and AO believes the action, not the label.
	switch class {
	case IncidentAutoRecoverable:
		if resolved != IncidentActionContinueRun {
			return false, "an auto-recoverable incident may only be resolved by continuing the run"
		}
	case IncidentRepairAO:
		if resolved != IncidentActionRepairAgent {
			return false, "a repair of AO itself may only be carried out by a Repair Agent"
		}
	case IncidentHumanDecision:
		if resolved == IncidentActionRepairAgent {
			return false, "a human decision may not launch a Repair Agent on its own"
		}
	case IncidentUnsafeOrInsufficient:
		return false, "AO refuses to act on a diagnosis that reported unsafe or insufficient evidence"
	default:
		return false, "unrecognised classification"
	}

	if policy.AutoAuthorized {
		return true, ""
	}
	if strings.TrimSpace(approvedBy) == "" {
		return false, "this action requires explicit human approval before it can run"
	}
	return true, ""
}

// incidentActionNeedsApproval reports whether the UI must collect a yes before
// offering to run this action. It is the read-side of authorizeIncidentAction
// and deliberately shares its allow-list, so the modal can never offer a
// one-click action the executor would then refuse.
func incidentActionNeedsApproval(class IncidentClass, kind IncidentActionKind) bool {
	resolved, policy := lookupIncidentAction(kind)
	if resolved == IncidentActionNone || resolved == IncidentActionAwaitHuman {
		return false
	}
	if class == IncidentUnsafeOrInsufficient {
		return false
	}
	return !policy.AutoAuthorized
}

// IncidentActionDescription is everything the UI needs to render one incident
// action: what it says it does, what it costs, and whether it may be offered.
type IncidentActionDescription struct {
	// Describe and Risk are the human-readable label and risk band.
	Describe string
	Risk     string
	// NeedsApproval reports whether a named human must approve before the
	// action may run.
	NeedsApproval bool
	// EndsWork and WritesCode say what the action does to the run: whether it
	// terminates the work, and whether it may change the workspace.
	EndsWork   bool
	WritesCode bool
	// Executable reports whether the action is offerable once approved --
	// asked of the real authorization gate, not re-derived here, so the button
	// and the permission cannot disagree.
	Executable bool
	// Refusal is why Executable is false, and empty when it is true.
	Refusal string
}

// DescribeIncidentAction is the API's single source for how an action must be
// presented and whether it may be offered as a one-click control.
//
// It exists so the frontend never re-derives any of this. A UI that inferred
// "does this need approval" from a classification string would eventually infer
// it differently from authorizeIncidentAction — and the executor is the one
// holding the permission, so the two disagreeing means either a button that
// errors or, far worse, a button that looks safe and is not.
func DescribeIncidentAction(class IncidentClass, kind IncidentActionKind) IncidentActionDescription {
	resolved, policy := lookupIncidentAction(kind)
	out := IncidentActionDescription{
		Describe:      policy.Describe,
		Risk:          string(policy.Risk),
		EndsWork:      policy.EndsWork,
		WritesCode:    policy.WritesCode,
		NeedsApproval: incidentActionNeedsApproval(class, resolved),
	}
	if resolved != kind {
		out.Refusal = "AO does not recognise the proposed action, so it will not run it"
		return out
	}
	// Ask the real gate, with an approval present, so "executable" means "this
	// is offerable once approved" rather than "this would run right now".
	out.Executable, out.Refusal = authorizeIncidentAction(class, resolved, "human")
	return out
}
