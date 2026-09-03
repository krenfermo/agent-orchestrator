package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// advice.go — P3-C: the single deterministic answer to "what do I do now?".
//
// P3-A gave AO one human projection of a run (presentation.go) and P1-B gave it
// one operator recommendation (recovery_assessment.go). Both are correct and
// neither answers the question a person actually has in front of a stopped run,
// because that question has three halves:
//
//	is anyone needed at all / what is AO already doing about it / if I am
//	needed, what exactly do I press
//
// DeriveAdvice answers all three from the SAME durable facts the other two read
// — attention.go's closed reason vocabulary and its per-reason disposition, the
// frozen repair policy and its budget, the placement record, the admission/wait
// state — and it introduces no fourth registry of stops. Every reason code it
// emits comes from attention.go; every action it offers comes from
// presentation.go's closed ActionID set; every repair decision comes from
// PlanRepair. This file only composes them and states the consequence.
//
// # It derives, it does not act
//
// Nothing in this file writes, launches, dispatches or schedules. That is §16
// of the checkpoint and it is a structural property rather than a convention:
// DeriveAdvice is a pure function of a plain input value, so a GET that renders
// advice CANNOT have a side effect. The AutomaticAction field says what AO
// intends; recovery_dispatch.go is the only thing that carries it out, and it
// re-derives its own authority when it does.

// AdviceVersion versions the rules below, so advice shown to a person stays
// explainable after they change.
const AdviceVersion = "v1"

// AdviceCategory is the four-way classification §1 of the checkpoint asks for,
// plus the honest fifth answer that a run which is simply working has.
type AdviceCategory string

const (
	// AdviceNoActionRequired is a run that is working, reviewing, verifying,
	// integrating — or one AO has already started correcting. Nobody is needed.
	AdviceNoActionRequired AdviceCategory = "no_action_required"
	// AdviceAutoRecoverable is a stop AO can and will address by itself: a
	// repair it is authorized to launch, a provider failover, a scheduled
	// retry. Distinct from wait-only because AO is DOING something, not
	// waiting for something.
	AdviceAutoRecoverable AdviceCategory = "auto_recoverable"
	// AdviceWaitOnly is a block that clears without anyone acting: capacity, a
	// branch another run holds, a queued wake. There is no remedy to offer and
	// offering one would be inviting an intervention that helps nothing.
	AdviceWaitOnly AdviceCategory = "wait_only"
	// AdviceHumanAction is a stop only a person can clear, and AO knows what
	// they have to do.
	AdviceHumanAction AdviceCategory = "human_action"
	// AdviceTerminal is a run that has ended. Nothing is recoverable; the only
	// thing that may remain is an integration.
	AdviceTerminal AdviceCategory = "terminal"
)

// AutomaticActionID is the closed set of things AO may do about a stop WITHOUT
// asking. Closed for the same reason ActionID and RecoveryAction are: an
// automatic action AO cannot name is one it must never take.
type AutomaticActionID string

const (
	// AutoActionNone means AO has nothing of its own to do.
	AutoActionNone AutomaticActionID = ""
	// AutoActionLaunchRepair is a repairable stop under an `automatic` repair
	// policy with budget left. It is the ONE automatic action that is
	// dispatched from advice (recovery_dispatch.go); every other member below
	// names machinery that is already running on its own.
	AutoActionLaunchRepair AutomaticActionID = "launch_repair"
	// AutoActionRepairInFlight is a repair generation that already exists and
	// has not finished. Nothing to dispatch — the answer to "what is AO doing"
	// is "this".
	AutoActionRepairInFlight AutomaticActionID = "repair_in_flight"
	// AutoActionScheduledRetry is a self-remediable stop with a durable wake:
	// a planner retry, a reviewer launch retry, a prompt transport retry.
	AutoActionScheduledRetry AutomaticActionID = "scheduled_retry"
	// AutoActionProviderFailover is a work/review dispatch whose provider
	// failed and whose budget still allows another provider.
	AutoActionProviderFailover AutomaticActionID = "provider_failover"
	// AutoActionAwaitCapacity is a provider/capacity wait AO resumes from by
	// itself. It is an automatic action rather than "nothing" because a person
	// reading the card must be told AO will come back to it.
	AutoActionAwaitCapacity AutomaticActionID = "await_capacity"
	// AutoActionAwaitBranch is the same answer for a branch another run holds.
	AutoActionAwaitBranch AutomaticActionID = "await_branch"
	// AutoActionResolveQuestion is a captured question AO is resolving with a
	// Decision Resolver instead of interrupting anybody (§20-22).
	AutoActionResolveQuestion AutomaticActionID = "resolve_question"
	// AutoActionDeliverQuestionResponse is a decision AO has already taken and
	// is now delivering to the agent that asked (P3-C §17).
	//
	// It is a separate action from resolving because they are different waits
	// with different failure modes: resolving can end in "ask a person", while
	// delivery can end in "this provider cannot be answered by AO at all". A
	// person watching either one should be told which is happening.
	AutoActionDeliverQuestionResponse AutomaticActionID = "deliver_question_response"
	// AutoActionFreshReview is verify_recovery.go's one authorized re-review.
	AutoActionFreshReview AutomaticActionID = "fresh_review"
)

// BlockedAction is one action AO will NOT perform, with the stable code that
// says why. §2 asks for blocked actions and blocked reasons as first-class
// output rather than as the absence of an offer.
type BlockedAction struct {
	ID     ActionID
	Reason string
}

// AdviceAuthority is the proof summary §2 asks for: the generations and the
// stop record any mutating action must be revalidated against. A UI holding an
// Advice and clicking an action minutes later sends these back, and
// recovery_dispatch.go refuses a click computed against a state the run has
// moved past.
type AdviceAuthority struct {
	// PlacementGeneration / LifecycleGeneration are the two generations the
	// placement model already versions every mutation by.
	PlacementGeneration int64
	LifecycleGeneration int64
	// RepairGeneration is the newest repair generation (0 when none), so a
	// second click on Repair cannot open a second generation.
	RepairGeneration int
	// StopPhase / StopAt identify the checkpoint that actually stopped the run.
	// They are the cheapest complete answer to "is this still the same stop?".
	StopPhase string
	StopAt    time.Time
	// RunState is the state the advice was computed against.
	RunState domain.WorkflowRunState
}

// Advice is the whole answer for one run.
type Advice struct {
	RunID string
	// TargetRunID is the run somebody should actually act on: it differs from
	// RunID exactly when this run's stop mirrors a child's.
	TargetRunID string
	// ReasonCode is attention.go's canonical code, never a new one.
	ReasonCode string
	Category   AdviceCategory
	Stage      Stage
	// SummaryCode is the stable key a UI renders its headline from — the same
	// value Presentation.SummaryCode carries, so the card and this contract can
	// never disagree.
	SummaryCode string
	// Summary and Explanation are AO's own English. Summary is one line;
	// Explanation says why it is stopped and what happens next. Both are
	// FALLBACKS for a UI with no localized copy for SummaryCode — never the
	// primary contract, and never a place where a fingerprint or a generation
	// is the headline (§9).
	Summary     string
	Explanation string
	// RequiresHuman is the single flag any surface uses to decide whether to
	// interrupt somebody. It is false for every category except human_action.
	RequiresHuman bool
	// AutomaticAction is what AO intends to do by itself, and
	// AutomaticActionActive reports that it is already happening. A non-empty
	// AutomaticAction with AutomaticActionActive false is a dispatchable
	// intent; recovery_dispatch.go is the only thing that acts on it.
	AutomaticAction       AutomaticActionID
	AutomaticActionActive bool
	// AutomaticActionBlockedReason names why an automatic action AO would
	// otherwise take is not on offer (repair_disabled, repair_exhausted,
	// repair_active, not_repairable). Empty when nothing is blocked.
	AutomaticActionBlockedReason string
	// RecommendedAction is the ONE thing AO suggests a person do, empty when
	// the honest answer is "nothing".
	RecommendedAction ActionID
	AvailableActions  []ActionID
	BlockedActions    []BlockedAction
	// ExpectedNextStage is where this run goes next if nothing else changes.
	// Empty when AO genuinely cannot say — never guessed.
	ExpectedNextStage Stage
	// Retryable reports that re-entering the ordinary resume path would do
	// something. It is Lifecycle.CanContinue, not a second derivation.
	Retryable bool
	// Repairable reports that the STOP CLASS is repairable, independently of
	// policy and budget. RepairEligibility is the full answer including why not.
	Repairable        bool
	RepairEligibility domain.RepairEligibility
	RepairSpent       int
	RepairBudget      int
	// WaitUntil / WaitReason are the run's soonest open wake, when there is
	// one. They are the only clock-dependent values here, and they are real
	// durable rows rather than an estimate (§2).
	WaitUntil  *time.Time
	WaitReason string
	Authority  AdviceAuthority
	// Technical is presentation.go's operator block, carried verbatim so a CLI
	// or an operator never needs a second call to see the reason codes.
	Technical Technical
	Version   string
}

// AdviceInput is everything DeriveAdvice reads. A plain value, so the whole
// projection stays a pure function testable without a store.
type AdviceInput struct {
	Detail       RunDetail
	Lifecycle    Lifecycle
	Presentation Presentation
	// Repair is PlanRepair's answer for this run. Its zero value means "no
	// repair authority was consulted", which is treated as ineligible rather
	// than as permission.
	Repair RepairPlan
	// Admission is why the run has not launched, when it has not.
	Admission AdmissionStateView
	Now       time.Time
}

// DeriveAdvice projects one run onto the advice model.
//
// Order matters and is the whole safety argument. Terminal first, because a
// finished run has no remedy. Then what AO is ALREADY doing, because a run
// being repaired must never be presented as a person's turn (§19). Then the
// waits, which clear by themselves (§5, §6). Then the automatic action AO is
// authorized to start (§3). Only what survives all of that can be a human
// decision.
func DeriveAdvice(in AdviceInput) Advice {
	p := in.Presentation
	disp, dispKnown := attentionDispositions[in.Lifecycle.AttentionReason]

	a := Advice{
		RunID:             in.Detail.Run.ID,
		TargetRunID:       in.Detail.Run.ID,
		ReasonCode:        in.Lifecycle.AttentionReason,
		Stage:             p.Stage,
		SummaryCode:       p.SummaryCode,
		RecommendedAction: p.RecommendedAction,
		Retryable:         in.Lifecycle.CanContinue,
		Repairable:        dispKnown && disp.Repairable,
		RepairEligibility: in.Repair.Eligibility,
		RepairSpent:       in.Repair.Spent,
		RepairBudget:      in.Repair.Budget,
		WaitUntil:         in.Lifecycle.NextWakeAt,
		WaitReason:        in.Lifecycle.WaitReason,
		Technical:         p.Technical,
		Version:           AdviceVersion,
		Authority: AdviceAuthority{
			PlacementGeneration: p.Technical.PlacementGeneration,
			LifecycleGeneration: p.Technical.LifecycleGeneration,
			RepairGeneration:    in.Detail.Repair.Attempt,
			StopPhase:           in.Detail.StopAuthorityPhase,
			StopAt:              in.Detail.StopAuthorityAt,
			RunState:            in.Detail.Run.State,
		},
	}
	if in.Lifecycle.AttentionWorkflowID != "" {
		a.TargetRunID = in.Lifecycle.AttentionWorkflowID
	}
	if a.RepairEligibility == "" {
		a.RepairEligibility = domain.RepairIneligible
	}
	a.AvailableActions, a.BlockedActions = splitActions(p.Actions)

	category, auto, active, blocked := classifyAdvice(in, disp, dispKnown)
	a.Category = category
	a.AutomaticAction = auto
	a.AutomaticActionActive = active
	a.AutomaticActionBlockedReason = blocked
	a.RequiresHuman = category == AdviceHumanAction
	a.ExpectedNextStage = expectedNextStage(in, category, auto)
	a.Summary, a.Explanation = adviceProse(in, a, disp)

	// §13/§19: while AO is acting, it does not also ask. A recommendation
	// alongside an active automatic action is exactly the "second, duplicate
	// remedy" the presentation model already refuses to render, and repeating
	// it here would let a caller reading advice reintroduce it.
	if a.Category == AdviceAutoRecoverable || a.Category == AdviceNoActionRequired ||
		a.Category == AdviceWaitOnly {
		if a.RecommendedAction != ActionWait && a.RecommendedAction != ActionIntegrate {
			a.RecommendedAction = ""
		}
	}
	// §13/§14: and a repair AO is about to start by itself is not also a button.
	// presentation.go offers Repair on the strength of the stop being a person's
	// -- which it is not, under an automatic policy -- and leaving the offer
	// standing would put a person one click away from authorizing the identical
	// repair AO already intends. It is REFUSED with its reason rather than
	// hidden, so "why can't I press this" stays answerable.
	if a.AutomaticAction == AutoActionLaunchRepair {
		a.AvailableActions, a.BlockedActions = refuseAction(
			a.AvailableActions, a.BlockedActions, ActionRepair, "automatic_repair_pending")
	}
	return a
}

// refuseAction moves one offer from the available list to the blocked list with
// a stated reason. It never ADDS an action that was not offered: an action AO
// was not going to allow anyway has no business appearing as a refusal.
func refuseAction(available []ActionID, blocked []BlockedAction, id ActionID, reason string) ([]ActionID, []BlockedAction) {
	kept := available[:0]
	found := false
	for _, got := range available {
		if got == id {
			found = true
			continue
		}
		kept = append(kept, got)
	}
	if !found {
		return available, blocked
	}
	return kept, append(blocked, BlockedAction{ID: id, Reason: reason})
}

// splitActions separates presentation.go's offers into the two lists §2 asks
// for. It reuses the SAME Action values the run detail page renders, so an
// action available in advice and unavailable on the page is not representable.
func splitActions(actions []Action) ([]ActionID, []BlockedAction) {
	var available []ActionID
	var blockedList []BlockedAction
	for _, act := range actions {
		if act.Enabled {
			available = append(available, act.ID)
			continue
		}
		reason := act.DisabledReason
		if reason == "" {
			reason = "unavailable"
		}
		blockedList = append(blockedList, BlockedAction{ID: act.ID, Reason: reason})
	}
	return available, blockedList
}

// classifyAdvice is the ordered decision described on DeriveAdvice.
func classifyAdvice(in AdviceInput, disp AttentionDisposition, dispKnown bool) (
	AdviceCategory, AutomaticActionID, bool, string,
) {
	p := in.Presentation
	if p.Stage.Terminal() {
		return AdviceTerminal, AutoActionNone, false, ""
	}

	// 1. A repair generation that exists and is not finished. It outranks
	//    everything, including the stop it is repairing: the origin's advice
	//    while its repair lives is "AO found a problem and is fixing it".
	if in.Detail.Repair.Active {
		return AdviceAutoRecoverable, AutoActionRepairInFlight, true, ""
	}
	// 2. verify_recovery.go's one authorized fresh review is AO re-asking a
	//    question, not a person's turn.
	if in.Detail.Repair.WaitingForFreshReview {
		return AdviceAutoRecoverable, AutoActionFreshReview, true, ""
	}

	// 3. A question AO is resolving by itself (§20-22). It is checked before
	//    the waits because a resolving question is AO working, not AO blocked.
	if resolving, _ := resolvingQuestion(in.Detail.Questions); resolving {
		return AdviceAutoRecoverable, AutoActionResolveQuestion, true, ""
	}
	// §17: the decision is taken and is on its way to the agent. It is still
	// AO's work, not a person's -- but only while AO can actually deliver it.
	// A provider whose prompts AO cannot answer structurally would otherwise
	// read as "AO is handling this" forever, which is the one thing a stuck run
	// must never say.
	//
	// And only while the run is not parked on a stop of its own. A pending
	// delivery to a worker that has since died would otherwise MASK the real
	// reason the run stopped, reporting "AO is sending the decision" about a
	// session there is nobody left to send it to. The run's own stop is the
	// truthful headline whenever it has one.
	parkedOnItsOwnStop := p.Stage == StageNeedsAttention && p.RequiresHuman
	if pending, deliverable := undeliveredAnswer(in.Detail.Questions); pending && !parkedOnItsOwnStop {
		if deliverable {
			return AdviceAutoRecoverable, AutoActionDeliverQuestionResponse, true, ""
		}
		return AdviceHumanAction, AutoActionNone, false, "provider_cannot_be_answered"
	}

	// 4. The waits. Nothing to repair, nothing to ask, and — §6 — no error.
	switch p.SummaryCode {
	case string(domain.AdmissionBranchWait), ReasonBranchQueued:
		return AdviceWaitOnly, AutoActionAwaitBranch, true, ""
	case string(domain.AdmissionCapacityWait), ReasonPlannerCapacityWait, ReasonReviewCapacityRetry,
		ReasonIncidentDiagnosisCapacityWait, string(domain.AdmissionProviderWait):
		return AdviceWaitOnly, AutoActionAwaitCapacity, true, ""
	}
	if in.Detail.BranchWait != nil {
		return AdviceWaitOnly, AutoActionAwaitBranch, true, ""
	}
	if in.Detail.CapacityWait != nil {
		return AdviceWaitOnly, AutoActionAwaitCapacity, true, ""
	}

	// 5. A self-remediable stop: attention.go already classified it, and that
	//    classification IS "AO will handle this". A provider failure with
	//    another attempt still budgeted is the failover case (§7).
	if dispKnown && disp.SelfRemediable {
		return AdviceAutoRecoverable, AutoActionScheduledRetry, true, ""
	}
	// A failover is a LIVE dispatch still trying providers. A run that has
	// PARKED still carries the failed attempt rows that produced its stop, so
	// reading them without this guard would report "AO is trying the next
	// provider" about a run that has stopped trying — the exact fabrication §7
	// forbids. The stop's own reason is the authority once the run is parked.
	if p.Stage != StageNeedsAttention && providerFailoverActive(in.Detail) {
		return AdviceAutoRecoverable, AutoActionProviderFailover, true, ""
	}

	// 6. §3: a repairable stop AO is authorized to repair itself. The intent is
	//    reported as NOT active — it has not been dispatched yet, and claiming
	//    otherwise would make an undispatched repair look like a running one.
	if in.Repair.Eligibility == domain.RepairEligible && in.Repair.AutomaticAllowed {
		return AdviceAutoRecoverable, AutoActionLaunchRepair, false, ""
	}

	// 7. Everything left is either a person's, or a run that is simply working.
	if p.Stage != StageNeedsAttention && !p.RequiresHuman {
		return AdviceNoActionRequired, AutoActionNone, false, ""
	}
	if !p.RequiresHuman {
		// Parked, but attention.go refused to call it a human decision (an
		// unclassified stop, or a named stop with no remedy). Saying "your
		// turn" here is exactly the dead end that vocabulary exists to remove.
		return AdviceWaitOnly, AutoActionNone, false, automaticBlockedReason(in, disp, dispKnown)
	}
	return AdviceHumanAction, AutoActionNone, false, automaticBlockedReason(in, disp, dispKnown)
}

// automaticBlockedReason explains why AO is NOT repairing a stop a person is
// now being asked about. Empty when repair was never on the table at all —
// "AO cannot repair a credential problem" is not a blocked automatic action,
// it is a condition with a different remedy.
func automaticBlockedReason(in AdviceInput, disp AttentionDisposition, dispKnown bool) string {
	if !dispKnown || !disp.Repairable {
		return ""
	}
	switch in.Repair.Eligibility {
	case domain.RepairPolicyDisabled:
		return "repair_disabled"
	case domain.RepairBudgetExhausted:
		return "repair_exhausted"
	case domain.RepairEligible:
		if !in.Repair.AutomaticAllowed {
			return "repair_requires_authorization"
		}
	}
	if in.Detail.Repair.Exhausted {
		return "repair_exhausted"
	}
	return ""
}

// resolvingQuestion reports a captured question AO is currently answering with
// a Decision Resolver rather than with a person, and whether an AUTONOMY POLICY
// is what routed it there.
//
// The second answer is not decoration. "AO is looking up which helper the repo
// already has" and "AO is taking a low-risk decision on your behalf, because
// you told it to" are different things to be told, and only the second one has
// a policy a person may want to change. Both are read off the durable question
// row; neither is inferred from prose.
func resolvingQuestion(qs []domain.WorkflowQuestion) (resolving, autonomous bool) {
	for _, q := range qs {
		if q.State != domain.QuestionStateResolving {
			continue
		}
		resolving = true
		if q.AutonomyMode.Valid() {
			return true, true
		}
	}
	return resolving, false
}

// providerFailoverActive reports a live dispatch whose provider failed and
// whose step has not stopped: AO is trying the next provider.
//
// It reads only durable attempt rows. A step that has genuinely run out of
// providers parks with its own canonical reason and never reaches here, which
// is what keeps "AO is trying Codex" apart from "every provider failed".
func providerFailoverActive(d RunDetail) bool {
	for _, s := range d.Steps {
		if s.Step.State.Terminal() || len(s.Attempts) == 0 {
			continue
		}
		newest := s.Attempts[len(s.Attempts)-1]
		if newest.Outcome != domain.WorkflowAttemptFailed {
			continue
		}
		if !failoverEligibleClass(newest.ErrorClass) {
			continue
		}
		return true
	}
	return false
}

// failoverEligibleClass names the attempt error classes another provider could
// plausibly succeed at. It is deliberately narrow: an auth failure or a missing
// binary is a configuration fact, and reporting "AO is trying the other
// provider" about one would be a fabrication.
func failoverEligibleClass(cls domain.WorkflowErrorClass) bool {
	switch cls {
	case domain.WorkflowErrorTransient, domain.WorkflowErrorRateLimited,
		domain.WorkflowErrorCapacityExhausted, domain.WorkflowErrorAgentStartFailed:
		return true
	default:
		return false
	}
}

// expectedNextStage answers "and then what". It is a statement about the
// PROGRESSION the run already has, never a prediction: a stage that is not in
// the run's own progress list is never named.
func expectedNextStage(in AdviceInput, category AdviceCategory, auto AutomaticActionID) Stage {
	if category == AdviceTerminal {
		return ""
	}
	switch auto {
	case AutoActionLaunchRepair, AutoActionRepairInFlight:
		return StageCorrecting
	case AutoActionDeliverQuestionResponse, AutoActionResolveQuestion:
		return StageWorking
	case AutoActionFreshReview:
		return StageReviewing
	}
	// The next FUTURE stage in the run's own progression, if there is one.
	current := -1
	for i, st := range in.Presentation.Progress {
		if st.State == ProgressCurrent || st.State == ProgressBlocked {
			current = i
			break
		}
	}
	if current < 0 {
		return ""
	}
	for _, st := range in.Presentation.Progress[current+1:] {
		if st.State == ProgressFuture && !st.Optional {
			return st.Stage
		}
	}
	return ""
}

// adviceProse writes AO's own two sentences. They are a FALLBACK for a UI with
// no copy for SummaryCode, and they are deliberately built from the disposition
// AO already wrote rather than from a second vocabulary — a person must never
// be told two different things about one stop.
func adviceProse(in AdviceInput, a Advice, disp AttentionDisposition) (summary, explanation string) {
	switch a.Category {
	case AdviceTerminal:
		return fmt.Sprintf("This run is %s.", in.Detail.Run.State),
			"There is nothing to recover. Anything still outstanding is named in the actions above."
	case AdviceNoActionRequired:
		return "AO is working. You do not need to do anything.",
			fmt.Sprintf("The run is at the %q stage and nothing is blocking it.", a.Stage)
	case AdviceAutoRecoverable:
		switch a.AutomaticAction {
		case AutoActionRepairInFlight:
			return "AO found a problem and is repairing it automatically.",
				"A bounded repair agent is working on the condition that stopped this run. Nothing is asked of you while it runs."
		case AutoActionLaunchRepair:
			return "AO found a problem it is allowed to repair by itself.",
				"This run's repair policy is automatic and its repair budget is not spent, so AO will start a bounded repair agent for this condition without asking."
		case AutoActionProviderFailover:
			return "The provider could not start. AO is trying the next one.",
				"A provider attempt failed with a class another provider can plausibly succeed at, and this obligation still has provider attempts budgeted."
		case AutoActionScheduledRetry:
			return "AO is retrying this by itself.",
				strings.TrimSpace(fmt.Sprintf("The stop is one AO handles automatically (%s). A durable wake is scheduled; nothing is asked of you.", a.ReasonCode))
		case AutoActionResolveQuestion:
			if _, autonomous := resolvingQuestion(in.Detail.Questions); autonomous {
				return "AO found a low-risk decision and is taking it automatically.",
					"This run's autonomy policy authorizes AO to settle technical, reversible choices from repository evidence instead of interrupting you. The decision it takes is recorded."
			}
			return "AO is answering a question the worker raised.",
				"A read-only Decision Resolver is determining the answer from repository evidence. You are only asked if it cannot."
		case AutoActionDeliverQuestionResponse:
			return "AO took a low-risk decision and is sending it to the agent.",
				"The decision is recorded. AO is selecting it in the agent's own prompt rather than typing into it, and re-reads the screen to confirm the right option before confirming."
		case AutoActionFreshReview:
			return "AO is re-reviewing the work.",
				"The approval AO held no longer describes the workspace, so it is asking for exactly one fresh review of what is there now."
		}
		return "AO is handling this by itself.", ""
	case AdviceWaitOnly:
		switch a.AutomaticAction {
		case AutoActionAwaitBranch:
			return "The branch is being used by another task.",
				"AO is queued for it and resumes by itself once the branch is free. Waiting is a valid choice here."
		case AutoActionAwaitCapacity:
			role := ""
			if in.Detail.CapacityWait != nil && in.Detail.CapacityWait.Role != "" {
				role = " to start the " + string(in.Detail.CapacityWait.Role)
			}
			return fmt.Sprintf("AO is waiting for available capacity%s.", role),
				"This is not an error and there is nothing to repair. AO retries on its own schedule."
		}
		if a.ReasonCode == unclassifiedStop || a.ReasonCode == "" {
			return "AO stopped and cannot say why.",
				"No canonical stop reason was recorded for this run, so AO will not tell you it is your turn when it does not know what happened. Inspect the run, or cancel it."
		}
		return fmt.Sprintf("AO stopped on %q and has no remedy to offer.", a.ReasonCode),
			"The stop is named but AO holds no action for it, so it is not presented as your decision."
	case AdviceHumanAction:
		s := humanSummaryFor(a.SummaryCode, a.ReasonCode)
		e := strings.TrimSpace(disp.HumanAction)
		if e == "" {
			e = strings.TrimSpace(in.Lifecycle.AttentionAction)
		}
		return s, e
	}
	return "", ""
}

// humanSummaryFor is the one-line headline for a human-owned stop. The cases
// named here are the ones the checkpoint states explicitly (§4, §8); every
// other reason falls back to the canonical code, which is honest and which the
// UI already has copy for.
func humanSummaryFor(summaryCode, reasonCode string) string {
	switch summaryCode {
	case "dirty_worktree":
		return "There are uncommitted changes in Git. AO cannot continue until they are saved."
	case ReasonProviderAuthRequired, ReasonReviewerAuthInvalid, string(domain.WorkflowErrorAuth):
		return "A provider needs you to sign in before AO can continue."
	case ReasonProviderWorkspaceTrustRequired:
		return "The provider has no recorded trust for this folder, so it cannot run unattended."
	case ReasonPlannerAmbiguous:
		return "The plan can no longer be trusted and has to be revalidated or regenerated."
	case ReasonFixBudgetExhausted, ReasonVerifyBudgetExhausted, repairEscalatedPhase:
		return "AO has used every automatic attempt it is allowed. The next step is yours."
	case ReasonQuestionHumanRequired:
		return "A worker asked a question AO will not answer for you."
	case ReasonProviderDialogUnreadable:
		// P3-D §13. The honest sentence, and a deliberately different one from
		// the worker-blocked headline: the decision is already made, so telling
		// somebody a worker is "waiting for you to decide" would send them to
		// repeat work AO has done. What they actually have to do is pass an
		// answer AO holds through a screen AO cannot drive.
		return "AO already decided this question and cannot read the agent's prompt to send the answer."
	}
	if reasonCode != "" {
		return fmt.Sprintf("AO stopped and needs a decision (%s).", reasonCode)
	}
	return "AO stopped and needs a decision."
}

// AdviceFor is the coordinator's read-only entry point: "what do I do about
// this run". It writes nothing, so a poll, a page load, a Board render and an
// operator's terminal can all ask freely (§32: no read endpoint mutates).
func (c *Coordinator) AdviceFor(ctx stdctx.Context, runID string) (Advice, error) {
	detail, err := c.GetRun(ctx, runID)
	if err != nil {
		return Advice{}, err
	}
	return c.adviceFromDetail(ctx, detail)
}

// AdviceForDetail is AdviceFor for a caller that has already loaded the run
// detail — the run detail page and the Board both have one in hand, and reading
// it twice to answer "what do I do now" would double the most expensive query
// on the most-polled route in the product.
func (c *Coordinator) AdviceForDetail(ctx stdctx.Context, detail RunDetail) (Advice, error) {
	return c.adviceFromDetail(ctx, detail)
}

// adviceFromDetail is AdviceFor's body for a caller that already loaded the
// detail, so a controller rendering a run page does not read it twice.
func (c *Coordinator) adviceFromDetail(ctx stdctx.Context, detail RunDetail) (Advice, error) {
	life := DeriveLifecycle(LifecycleInput{Detail: detail, Questions: detail.Questions})
	placements, overrides, admission := c.advicePresentationInputs(ctx, detail.Run.ID)
	now := c.clock()
	presentation := DerivePresentation(PresentationInput{
		Detail: detail, Lifecycle: life, Placements: placements,
		Overrides: overrides, Admission: admission, Now: now,
	})
	// PlanRepair writes nothing (see its doc comment), which is what makes it
	// safe to consult from a read path. A failure to compute it degrades to
	// "no repair authority", never to an error: advice about a stopped run is
	// more useful than a 500 about the repair planner.
	plan, perr := c.planRepairFor(ctx, detail)
	if perr != nil {
		if c.log != nil {
			c.log.Debug("workflow: advice could not plan a repair", "run", detail.Run.ID, "err", perr)
		}
		plan = RepairPlan{Eligibility: domain.RepairUnknownCondition}
	}
	return DeriveAdvice(AdviceInput{
		Detail: detail, Lifecycle: life, Presentation: presentation,
		Repair: plan, Admission: admission, Now: now,
	}), nil
}

// advicePresentationInputs reads the placement authority for one run.
//
// It is the coordinator-side twin of the controller's own presentationInputs,
// and it exists so a CLI or an internal caller gets the SAME projection the
// HTTP surface renders rather than a thinner one derived from fewer facts. A
// reader that fails degrades to its zero value: an absent placement record is
// already a meaningful input (PlacementChoiceUnknown), and refusing to give
// advice because a placement query failed would withhold the one answer the
// caller asked for.
func (c *Coordinator) advicePresentationInputs(ctx stdctx.Context, runID string) (
	[]PlacementView, []PlacementOverrideView, AdmissionStateView,
) {
	var placements []PlacementView
	var overrides []PlacementOverrideView
	var admission AdmissionStateView
	if got, err := c.ListPlacements(ctx, runID); err == nil {
		placements = got
	}
	if got, err := c.ListPlacementOverrides(ctx, runID); err == nil {
		overrides = got
	}
	if got, err := c.AdmissionState(ctx, runID); err == nil {
		admission = got
	}
	return placements, overrides, admission
}

// ApplyAutonomyPolicy freezes a run's question-autonomy mode.
//
// It mirrors ApplyRepairPolicy exactly, for the same reasons: run creation
// always stamps the safe default (ask_always), and this is the explicit
// post-creation step a caller takes when the create request named something
// else — which is what keeps CreateRun's signature and its call sites
// unchanged.
//
// It refuses once the run has left `pending`. An autonomy mode is a statement
// about what AO may decide unattended, and widening that for a run already in
// flight would change the terms under which work already started. Restarts
// cannot change it either: they never re-enter this call.
func (c *Coordinator) ApplyAutonomyPolicy(ctx stdctx.Context, runID string, mode domain.QuestionAutonomyMode) error {
	if !mode.Valid() {
		return fmt.Errorf("%w: %q is not a question autonomy mode", ErrInvalid, mode)
	}
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	if run.State != domain.WorkflowRunPending {
		return fmt.Errorf("%w: workflow run %q is already %s; its autonomy policy is frozen", ErrInvalid, runID, run.State)
	}
	return c.rewriteFrozenPolicy(ctx, run, func(p *domain.WorkflowPolicy) {
		frozen := p.EffectiveAutonomyPolicy()
		frozen.Mode = mode
		frozen.At = c.clock()
		p.Autonomy = frozen
	})
}

// rewriteFrozenPolicy applies one edit to a run's policy snapshot and persists
// it. It is the shared body of ApplyRepairPolicy and ApplyAutonomyPolicy: both
// freeze one knob of the same snapshot under the same precondition, and having
// each marshal it separately was two places for the snapshot round-trip to
// diverge.
//
// The `pending` precondition lives in the callers rather than here, because
// each of them owns the sentence explaining WHICH policy is frozen and why —
// and a shared error saying "some policy is frozen" would be worse than the
// duplication it removed.
func (c *Coordinator) rewriteFrozenPolicy(ctx stdctx.Context, run domain.WorkflowRun, edit func(*domain.WorkflowPolicy)) error {
	policy := policyForRun(run)
	edit(&policy)
	snapshot, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	_, err = c.store.UpdateWorkflowRunPolicySnapshot(ctx, run.ID, string(snapshot), c.clock())
	return err
}

// RunAutonomyPolicy reads back a run's frozen question-autonomy policy.
func (c *Coordinator) RunAutonomyPolicy(ctx stdctx.Context, runID string) (domain.QuestionAutonomySnapshot, error) {
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return domain.QuestionAutonomySnapshot{}, err
	}
	if !ok {
		return domain.QuestionAutonomySnapshot{}, fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	return policyForRun(run).EffectiveAutonomyPolicy(), nil
}

// undeliveredAnswer reports a question AO has answered but not yet handed to
// the agent, and whether AO is still able to hand it over.
//
// The second answer is what stops a stuck run from reading as a working one.
// Delivery goes through the provider's own prompt, and a provider AO cannot
// answer structurally (§15) will never receive it — so the honest advice is
// that a person is needed, not that AO is busy. Reporting "AO is handling this"
// about work AO cannot finish is exactly the misreport the whole Advisor exists
// to end.
func undeliveredAnswer(qs []domain.WorkflowQuestion) (pending, deliverable bool) {
	for _, q := range qs {
		if q.State != domain.QuestionStateAnswered || q.Delivered {
			continue
		}
		pending = true
		if SupportsStructuredDialogResponse(q.AskingHarness) {
			return true, true
		}
	}
	return pending, false
}
