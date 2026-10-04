package workflow

import (
	stdctx "context"
	"fmt"
	"sort"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// recovery_status.go — "how is AO trying to recover this run".
//
// The recovery ASSESSMENT (recovery_assessment.go) answers what an operator
// should do. The PRESENTATION (presentation.go) answers what a person reading
// the page sees. Neither answers the question an engineer has when a run is not
// finishing: which of AO's own recovery mechanisms is in play right now, and
// what is it waiting for.
//
// That question has a bounded set of answers and they are NOT all "running".
// A run waiting for a capacity slot, a run queued behind somebody else's branch
// lock, a run whose reviewer is being retried on a second provider, a run whose
// autonomous answer cannot be delivered, and a run rebuilding its state after a
// restart are five completely different situations with five different next
// steps — and before this file every one of them read as "running" or as a flat
// "needs attention". §7 names them; RecoveryState is that vocabulary.
//
// Two rules hold the file together:
//
//   - It DERIVES, it does not decide. Every field is read off durable rows the
//     caller already loaded, or off one narrow extra read the caller explicitly
//     opted into (capacity claims, on the run-detail route only). Nothing here
//     writes, so a poll, a page load and an operator's terminal can all ask.
//   - It does not duplicate Advice. Advice says what a person should do;
//     RecoveryStatus says what AO is doing. They are checked against each other
//     by invariant (recovery_status_invariants_test.go) rather than by one
//     being derived from the other, because a projection that agreed with
//     Advice by construction could not detect Advice being wrong.

// RecoveryStatusVersion versions the derivation, so a status shown to somebody
// stays explainable after the rules change.
const RecoveryStatusVersion = "v1"

// RecoveryState is the closed vocabulary of "what is AO doing about this run".
type RecoveryState string

const (
	// RecoveryHealthyRunning is work in flight with nothing blocking it.
	RecoveryHealthyRunning RecoveryState = "healthy_running"
	// RecoveryWaitingCapacity is a launch that cannot start until a runtime
	// slot frees. A legitimate wait, never a fault.
	RecoveryWaitingCapacity RecoveryState = "waiting_capacity"
	// RecoveryWaitingBranch is a direct-branch run queued behind another run's
	// branch lock. Also legitimate, and it names the holder.
	RecoveryWaitingBranch RecoveryState = "waiting_branch"
	// RecoveryWaitingProvider is a bounded provider retry: the launch failed
	// transiently and AO has scheduled its own next attempt.
	RecoveryWaitingProvider RecoveryState = "waiting_provider"
	// RecoveryWaitingDialogDelivery is an answer AO holds and has not been able
	// to hand to the agent yet.
	RecoveryWaitingDialogDelivery RecoveryState = "waiting_dialog_delivery"
	// RecoveryVerifyingResult is a worker whose turn has finished and whose
	// result AO is judging — review or verification.
	RecoveryVerifyingResult RecoveryState = "verifying_result"
	// RecoveryAutomaticPending is a stop AO is entitled to clear itself and has
	// scheduled a wake for. The person is not the next actor.
	RecoveryAutomaticPending RecoveryState = "automatic_recovery_pending"
	// RecoveryRepairRunning is a Repair Agent generation in flight.
	RecoveryRepairRunning RecoveryState = "repair_running"
	// RecoveryFailoverRunning is a second provider attempt after a first one
	// failed. Not a user failure while the fallback is alive.
	RecoveryFailoverRunning RecoveryState = "failover_running"
	// RecoveryRestartRecovery is a run whose durable state is being rebuilt
	// after a daemon restart: an attempt is open, its step is in flight, and
	// nothing has moved since boot.
	RecoveryRestartRecovery RecoveryState = "restart_recovery"
	// RecoveryNeedsHuman is a stop AO cannot clear.
	RecoveryNeedsHuman RecoveryState = "needs_human"
	// RecoveryTerminal is a run that ended.
	RecoveryTerminal RecoveryState = "terminal"
)

// Waiting reports a state that is a legitimate wait rather than a fault. It is
// what keeps "queued behind a branch" out of every "stuck run" count.
func (s RecoveryState) Waiting() bool {
	switch s {
	case RecoveryWaitingCapacity, RecoveryWaitingBranch, RecoveryWaitingProvider,
		RecoveryWaitingDialogDelivery:
		return true
	default:
		return false
	}
}

// AOIsActing reports a state in which AO itself is the next actor. Its
// complement is exactly RecoveryNeedsHuman plus RecoveryTerminal, and the
// Advice invariant is stated against it.
func (s RecoveryState) AOIsActing() bool {
	return s != RecoveryNeedsHuman && s != RecoveryTerminal
}

// RecoveryAttemptChain is one step's provider attempts in order, which is what
// makes a failover readable: attempt 1 on one provider with its failure class,
// attempt 2 on another, running.
type RecoveryAttemptChain struct {
	AttemptID     string
	AttemptNumber int64
	Provider      string
	Outcome       string
	ErrorClass    string
	StartedAt     time.Time
	FinishedAt    *time.Time
}

// RecoveryRepairView is the automatic repair, in the terms a person reading a
// run asks about: which attempt of how many, on what provider, why it started,
// and whether another one remains.
type RecoveryRepairView struct {
	Active bool
	// Attempt/Budget are the N and M of "attempt N of M".
	Attempt int
	Budget  int
	RunID   string
	// Exhausted reports every attempt spent; NextRetryPossible is its inverse
	// stated positively, because "will AO try again" is the actual question.
	Exhausted         bool
	NextRetryPossible bool
	// WhyStarted is the condition the repair was opened for, and Detail is AO's
	// own sentence about why the newest generation reads as live or quiescent.
	WhyStarted string
	Detail     string
	// Quiescent is a generation that exists, is unfinished, and has been proven
	// unable to write. It is neither active nor exhausted, and conflating it
	// with either is what made repairs unreadable.
	Quiescent bool
}

// RecoveryCapacityView is the admission answer for this run's launches.
//
// Filled only by the caller that opted into the claim read. Read=false means
// "nobody looked", which is never rendered as "no claims".
type RecoveryCapacityView struct {
	Read bool
	// Waiting/Held count this run's queued and held claims.
	Waiting int
	Held    int
	// Kinds names the roles waiting, so "waiting for capacity" says for what.
	Kinds []string
	// ClaimID/DispatchKey identify the newest claim, for an operator chasing it.
	ClaimID     string
	DispatchKey string
	// FossilSuspected marks a HELD claim whose lifecycle generation is older
	// than its step's current one — a slot paid for by a launch the lifecycle
	// has moved past. It is a suspicion, deliberately: only the scheduler holds
	// the authority to release one, and a read may not.
	FossilSuspected bool
}

// RecoveryBranchView is who owns the branch this run needs. Read-only: nothing
// in this file acquires or releases a lock (§12).
type RecoveryBranchView struct {
	Branch string
	// HeldByRunID/HeldBySessionID name the current owner when this run is
	// WAITING for it; they are empty when this run owns it or when no wait is
	// recorded.
	HeldByRunID     string
	HeldBySessionID string
	Waiting         bool
}

// RecoveryDialogView is the question pipeline's state, and nothing else about
// it (§13): no pane contents, no keystrokes, no prompt text.
type RecoveryDialogView struct {
	// State is captured / resolving / resolved / delivery_pending / delivered /
	// unreadable / "" when this run has no question in flight.
	State string
	// Source is how the answer was reached, when there is one.
	Source string
	// Unreadable marks the bounded provider_dialog_unreadable condition.
	Unreadable bool
}

// RecoveryEvent is one bounded, significant thing that happened (§14).
type RecoveryEvent struct {
	Kind   string
	Phase  string
	At     time.Time
	StepID string
	Detail string
}

// RecoveryStatus is the whole answer.
type RecoveryStatus struct {
	RunID  string
	TaskID string
	// Execution is which attempt this is about; see recovery_execution.go.
	Execution RecoveryExecution
	State     RecoveryState
	// StopReason is the canonical attention reason when the run is stopped.
	StopReason string
	// RecommendedAction mirrors the assessment's one recommendation, so a
	// caller holding a status never has to resolve it a second way.
	RecommendedAction domain.RecoveryAction
	Repair            RecoveryRepairView
	// Failover is the step's provider attempt chain, in order.
	Failover []RecoveryAttemptChain
	Capacity RecoveryCapacityView
	Branch   RecoveryBranchView
	Dialog   RecoveryDialogView
	// NextWakeAt/RetryCount are the durable wake this run is resting on.
	NextWakeAt *time.Time
	RetryCount int64
	// Timeline is the bounded significant history, newest last.
	Timeline []RecoveryEvent
	Version  string
}

// RecoveryStatusFor is the read-only entry point.
//
// It performs exactly ONE read beyond what the assessment already needed: this
// run's capacity claims. That read is here rather than in the presentation
// because the Board renders presentations by the hundred and a per-card claim
// query is precisely the N+1 §17 forbids; the run-detail route renders one.
func (c *Coordinator) RecoveryStatusFor(ctx stdctx.Context, runID string) (RecoveryStatus, error) {
	detail, err := c.GetRun(ctx, runID)
	if err != nil {
		return RecoveryStatus{}, err
	}
	assessment, err := c.AssessRecovery(ctx, runID)
	if err != nil {
		return RecoveryStatus{}, err
	}
	status := c.deriveRecoveryStatus(detail, assessment)
	status.Capacity = c.readCapacityView(ctx, detail)
	// The capacity read can only ever make the state MORE specific, never less:
	// a run whose only obstacle is an unstarted claim was previously reported as
	// whatever its step state suggested.
	if status.State == RecoveryHealthyRunning && status.Capacity.Waiting > 0 {
		status.State = RecoveryWaitingCapacity
	}
	return status, nil
}

// deriveRecoveryStatus is the pure core: a function of the durable facts the
// caller already holds, so it can be exercised directly and so a caller that
// has a detail in hand does not load it twice.
func (c *Coordinator) deriveRecoveryStatus(d RunDetail, a RecoveryAssessment) RecoveryStatus {
	s := RecoveryStatus{
		RunID:             d.Run.ID,
		TaskID:            a.TaskID,
		Execution:         deriveRecoveryExecution(d, a.StepID, FixAuthority{}),
		StopReason:        a.ReasonCode,
		RecommendedAction: a.RecommendedAction,
		NextWakeAt:        d.NextWakeAt,
		RetryCount:        d.WakeAttemptCount,
		Version:           RecoveryStatusVersion,
	}
	s.Repair = deriveRepairView(d, a)
	s.Failover = deriveFailoverChain(d, s.Execution.StepID)
	s.Branch = deriveBranchView(d)
	s.Dialog = deriveDialogView(d)
	s.Timeline = deriveRecoveryTimeline(d)
	s.State = deriveRecoveryState(d, a, s)
	return s
}

// deriveRecoveryState is the classification, in the order the answers exclude
// each other. Terminal first, because a finished run is not a recovery
// question; then the things AO is actively doing; then the legitimate waits;
// and only at the end the human stop, so a run AO is still working on is never
// reported as somebody's turn.
func deriveRecoveryState(d RunDetail, a RecoveryAssessment, s RecoveryStatus) RecoveryState {
	if d.Run.State.Terminal() {
		return RecoveryTerminal
	}
	if s.Repair.Active {
		return RecoveryRepairRunning
	}
	if s.Dialog.Unreadable {
		// The bounded retry is still AO's to make until the stop is raised; once
		// it is raised the reason lands in the human branch below.
		if a.ReasonCode != ReasonProviderDialogUnreadable {
			return RecoveryWaitingDialogDelivery
		}
	} else if s.Dialog.State == dialogStateDeliveryPending {
		return RecoveryWaitingDialogDelivery
	}
	if s.Branch.Waiting {
		return RecoveryWaitingBranch
	}
	switch a.ReasonCode {
	case ReasonCapacityRetryExhausted:
		// Named separately from the wait: the budget is spent, so this is not a
		// queue any more.
		return RecoveryNeedsHuman
	case ReasonWorkerLaunchRetry, ReasonReviewerLaunchRetry, ReasonPromptTransportRetry:
		return RecoveryWaitingProvider
	}
	if reason := a.ReasonCode; reason != "" {
		if disp, ok := attentionDispositions[reason]; ok && disp.SelfRemediable {
			if stopIsACapacitySlotWait(reason) {
				return RecoveryWaitingCapacity
			}
			return RecoveryAutomaticPending
		}
	}
	if isFailoverInFlight(s.Failover) {
		return RecoveryFailoverRunning
	}
	if d.Run.State == domain.WorkflowRunNeedsAttention {
		return RecoveryNeedsHuman
	}
	if looksLikeRestartRecovery(s.Execution) {
		return RecoveryRestartRecovery
	}
	if judgingResult(d) {
		return RecoveryVerifyingResult
	}
	return RecoveryHealthyRunning
}

// stopIsACapacitySlotWait recognises the self-remediable stops whose obstacle is
// a runtime slot, from the dispositions table's own phase rather than from a
// hand-kept list of reason strings.
//
// Named distinctly from lifecycle.go's isCapacityWaitReason, which answers a
// different question (whether a WAKE reason counts as a capacity wait); two
// functions with one name would invite reading either answer as the other.
func stopIsACapacitySlotWait(reason string) bool {
	disp, ok := attentionDispositions[reason]
	return ok && disp.Phase == PhaseWaitingForCapacity
}

// isFailoverInFlight reports a chain whose earlier attempts failed and whose
// newest one is still open — the shape that must not read as a user failure.
func isFailoverInFlight(chain []RecoveryAttemptChain) bool {
	if len(chain) < 2 {
		return false
	}
	newest := chain[len(chain)-1]
	if newest.Outcome != "" {
		return false
	}
	for _, a := range chain[:len(chain)-1] {
		if a.Outcome == string(domain.WorkflowAttemptFailed) {
			return true
		}
	}
	return false
}

// judgingResult reports a run whose worker has finished and whose result AO is
// now judging. It is deliberately about the STEPS rather than about the run
// state: "reviewing" and "verifying" are both AO judging a result, and a person
// asking "why is this not done" is owed that answer rather than "running".
func judgingResult(d RunDetail) bool {
	for _, s := range d.Steps {
		switch s.Step.Kind {
		case domain.WorkflowStepReview, domain.WorkflowStepVerify:
			if s.Step.State == domain.WorkflowStepRunning || s.Step.State == domain.WorkflowStepReady {
				return true
			}
		}
	}
	return false
}

// looksLikeRestartRecovery reports a run whose newest durable act is
// reconciliation itself: the boundary dispatch reconciliation writes when it
// resolves what a crash left behind.
//
// That row IS the restart, which is why it is the signal rather than a clock
// comparison or a process start time. A run whose latest event is a
// reconciliation and whose attempt is still open is one AO is rebuilding.
//
// Deliberately conservative. Reporting "restart recovery" over a run that is
// simply working would be the worse lie of the two, because it invites somebody
// to wait for a rebuild that is not happening.
func looksLikeRestartRecovery(e RecoveryExecution) bool {
	if e.AttemptID == "" || e.Outcome != "" {
		return false
	}
	if e.LifecycleState != string(domain.WorkflowStepRunning) {
		return false
	}
	return e.LastEventPhase == string(domain.DispatchPhaseWorkerDispatchReconciled)
}

// deriveRepairView projects the repair lifecycle the run detail already folded.
func deriveRepairView(d RunDetail, a RecoveryAssessment) RecoveryRepairView {
	r := d.Repair
	v := RecoveryRepairView{
		Active:            r.Active,
		Attempt:           r.Attempt,
		Budget:            r.Budget,
		RunID:             r.RunID,
		Exhausted:         r.Exhausted,
		NextRetryPossible: !r.Exhausted && r.Budget > r.Attempt,
		Detail:            r.QuiescenceReason,
		Quiescent:         r.Quiescent,
	}
	if r.Attempt > 0 {
		// Why it started is the condition AO judged repairable, which is the
		// run's own stop reason at the time. The assessment carries the current
		// one, and for a repair in flight that is the one it was opened for.
		v.WhyStarted = a.ReasonCode
	}
	return v
}

// deriveFailoverChain returns the step's attempts in order.
func deriveFailoverChain(d RunDetail, stepID string) []RecoveryAttemptChain {
	if stepID == "" {
		return nil
	}
	for _, s := range d.Steps {
		if s.Step.ID != stepID {
			continue
		}
		out := make([]RecoveryAttemptChain, 0, len(s.Attempts))
		for _, a := range s.Attempts {
			out = append(out, RecoveryAttemptChain{
				AttemptID:     a.ID,
				AttemptNumber: a.AttemptNumber,
				Provider:      a.Harness,
				Outcome:       string(a.Outcome),
				ErrorClass:    string(a.ErrorClass),
				StartedAt:     a.StartedAt,
				FinishedAt:    a.FinishedAt,
			})
		}
		return out
	}
	return nil
}

// deriveBranchView reads the structured branch wait the run detail already
// enriched. Read-only by construction: there is nothing here that could take a
// lock even by accident.
func deriveBranchView(d RunDetail) RecoveryBranchView {
	if d.BranchWait == nil {
		return RecoveryBranchView{}
	}
	return RecoveryBranchView{
		Branch:          d.BranchWait.Branch,
		HeldByRunID:     d.BranchWait.HeldByWorkflowRunID,
		HeldBySessionID: d.BranchWait.HeldBySessionID,
		Waiting:         true,
	}
}

// The dialog vocabulary, as the states a person may be shown (§13).
const (
	dialogStateCaptured        = "captured"
	dialogStateResolving       = "resolving"
	dialogStateDeliveryPending = "delivery_pending"
	dialogStateDelivered       = "delivered"
	dialogStateUnreadable      = "unreadable"
	dialogStateHumanRequired   = "human_required"
)

// deriveDialogView projects the newest question's pipeline position, and only
// that. No prompt text, no options, no pane contents.
//
// `delivery_pending` and `resolving` are deliberately separate: one means AO has
// not decided yet, the other means it has and the agent has not received it.
// Conflating them hides exactly the failure P3-D spent its smoke B on.
func deriveDialogView(d RunDetail) RecoveryDialogView {
	var newest *domain.WorkflowQuestion
	for i := range d.Questions {
		q := &d.Questions[i]
		if newest == nil || q.CreatedAt.After(newest.CreatedAt) {
			newest = q
		}
	}
	if newest == nil {
		return RecoveryDialogView{}
	}
	v := RecoveryDialogView{}
	if newest.AnswerSource != nil {
		v.Source = string(*newest.AnswerSource)
	}
	switch newest.State {
	case domain.QuestionStatePending:
		v.State = dialogStateCaptured
	case domain.QuestionStateResolving:
		v.State = dialogStateResolving
	case domain.QuestionStateAnswered:
		if newest.Delivered {
			v.State = dialogStateDelivered
		} else {
			v.State = dialogStateDeliveryPending
		}
	case domain.QuestionStateHumanRequired:
		v.State = dialogStateHumanRequired
	default:
		v.State = string(newest.State)
	}
	// Unreadable is a fact about DELIVERY, not about the question's own state,
	// so it is read off the ledger rather than off the row.
	if v.State == dialogStateDeliveryPending && hasPhase(d, dialogUnreadablePhase) {
		v.State = dialogStateUnreadable
		v.Unreadable = true
	}
	return v
}

// hasPhase reports whether the run's newest checkpoint per step carries a phase.
// It reads what the detail already folded rather than re-querying the ledger.
func hasPhase(d RunDetail, phase string) bool {
	if d.LatestCheckpointPhase == phase || d.StopAuthorityPhase == phase {
		return true
	}
	for _, s := range d.Steps {
		if s.LatestCheckpoint != nil && s.LatestCheckpoint.DurablePhase == phase {
			return true
		}
	}
	return false
}

// recoveryTimelineKinds is the WHITELIST (§14). A phase absent from it is not
// on the timeline, which is what keeps a heartbeat out of it: the ledger
// carries observation rows on every poll, and a timeline that showed them would
// be a log rather than a history.
var recoveryTimelineKinds = map[string]string{
	workerDispatchedDurablePhase: "dispatch_accepted",
	"worker_launch_intent":       "dispatch_requested",
	"work_completed":             "work_result_observed",
	"review_dispatched":          "review_started",
	"review_verdict":             "review_verdict",
	fixDispatchedPhase:           "fix_dispatched",
	fixAttemptSupersededPhase:    "attempt_superseded",
	"repair_intent_created":      "repair_scheduled",
	"repair_launched":            "repair_launched",
	"repair_completed":           "repair_completed",
	"verify_completed":           "verified",
	"autonomous_local_commit":    "completed",
	dialogUnreadablePhase:        "dialog_unreadable",
	"question_answer_delivered":  "dialog_delivered",
	"waiting_for_branch":         "branch_wait",
	"worker_capacity_wait":       "capacity_wait",
	string(domain.DispatchPhaseWorkerDispatchReconciled): "restart_recovery",
	"attempt_reaped_orphaned":                            "attempt_reaped",
	ReasonProviderDialogUnreadable:                       "dialog_unreadable_stop",
	ReasonWorkerDispatchAmbiguous:                        "worker_ambiguous",
}

// deriveRecoveryTimeline projects the whitelisted phases from the per-step
// checkpoints the detail already carries, newest last and bounded.
//
// It does NOT re-read the ledger. The full history lives in the checkpoints
// table and an operator who needs all of it has the ledger; what this owes is
// the handful of transitions that explain how the run got where it is.
func deriveRecoveryTimeline(d RunDetail) []RecoveryEvent {
	out := make([]RecoveryEvent, 0, len(d.Steps)+2)
	add := func(cp *domain.WorkflowCheckpoint) {
		if cp == nil {
			return
		}
		kind, ok := recoveryTimelineKinds[cp.DurablePhase]
		if !ok {
			return
		}
		out = append(out, RecoveryEvent{
			Kind: kind, Phase: cp.DurablePhase, At: cp.CreatedAt,
			StepID: derefString(cp.WorkflowStepID), Detail: cp.NextAction,
		})
	}
	for _, s := range d.Steps {
		add(s.LatestCheckpoint)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	const maxRecoveryTimelineEvents = 24
	if len(out) > maxRecoveryTimelineEvents {
		out = out[len(out)-maxRecoveryTimelineEvents:]
	}
	return out
}

// readCapacityView is the one opted-in extra read.
func (c *Coordinator) readCapacityView(ctx stdctx.Context, d RunDetail) RecoveryCapacityView {
	if c.capacity == nil {
		return RecoveryCapacityView{}
	}
	claims, err := c.capacity.ListCapacityClaimsForRun(ctx, d.Run.ID)
	if err != nil {
		return RecoveryCapacityView{}
	}
	v := RecoveryCapacityView{Read: true}
	generation := map[string]int64{}
	for _, s := range d.Steps {
		generation[s.Step.ID] = int64(len(s.Attempts))
	}
	for _, claim := range claims {
		switch claim.State {
		case domain.CapacityClaimQueued:
			v.Waiting++
			v.Kinds = appendUnique(v.Kinds, string(claim.Kind))
		case domain.CapacityClaimHeld:
			v.Held++
			// A held claim whose generation is older than its step's current one
			// is paying for a launch the lifecycle has moved past. Reported as a
			// SUSPICION: only the scheduler may release one, and this is a read.
			if cur, ok := generation[claim.WorkflowStepID]; ok && claim.LifecycleGeneration < cur {
				v.FossilSuspected = true
			}
		}
		if claim.State != domain.CapacityClaimReleased {
			v.ClaimID, v.DispatchKey = claim.ID, claim.DispatchKey
		}
	}
	return v
}

func appendUnique(xs []string, x string) []string {
	for _, existing := range xs {
		if existing == x {
			return xs
		}
	}
	return append(xs, x)
}

// Describe renders the status as the one sentence `ao workflow recover status`
// leads with (§16). It is here rather than in the CLI so every surface says the
// same thing about the same state.
func (s RecoveryStatus) Describe() string {
	switch s.State {
	case RecoveryTerminal:
		return "This run has ended. No action required."
	case RecoveryHealthyRunning:
		return "Working normally. No action required."
	case RecoveryVerifyingResult:
		return "The worker finished its turn. AO is judging the result. No action required."
	case RecoveryWaitingCapacity:
		if len(s.Capacity.Kinds) > 0 {
			return fmt.Sprintf("Waiting for a runtime slot for: %v. No action required.", s.Capacity.Kinds)
		}
		return "Waiting for a runtime slot. No action required."
	case RecoveryWaitingBranch:
		if s.Branch.HeldByRunID != "" {
			return fmt.Sprintf("Waiting for branch %s, held by workflow %s. No action required.",
				s.Branch.Branch, s.Branch.HeldByRunID)
		}
		return fmt.Sprintf("Waiting for branch %s. No action required.", s.Branch.Branch)
	case RecoveryWaitingProvider:
		return "A provider launch failed and AO has scheduled its own retry. No action required."
	case RecoveryWaitingDialogDelivery:
		return "AO decided the agent's question and is delivering the answer. No action required."
	case RecoveryAutomaticPending:
		return "AO is recovering this run by itself. No action required."
	case RecoveryRepairRunning:
		return fmt.Sprintf("Repair attempt %d/%d is running%s.",
			s.Repair.Attempt, s.Repair.Budget, providerSuffix(s.Execution.Provider))
	case RecoveryFailoverRunning:
		return fmt.Sprintf("A provider failed and AO is trying another one%s.",
			providerSuffix(newestProvider(s.Failover)))
	case RecoveryRestartRecovery:
		return "AO is rebuilding this run's state after a restart. No action required."
	case RecoveryNeedsHuman:
		if s.StopReason != "" {
			return fmt.Sprintf("Human action required (%s).", s.StopReason)
		}
		return "Human action required."
	}
	return "AO cannot describe this run's recovery state."
}

func providerSuffix(provider string) string {
	if provider == "" {
		return ""
	}
	return " with " + provider
}

func newestProvider(chain []RecoveryAttemptChain) string {
	if len(chain) == 0 {
		return ""
	}
	return chain[len(chain)-1].Provider
}
