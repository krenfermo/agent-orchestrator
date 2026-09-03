package workflow

import (
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// presentation.go — P3-A: the canonical HUMAN projection of a workflow run.
//
// AO already derives everything a person needs (lifecycle.go's Phase and
// Attention, attention.go's closed reason vocabulary and its per-reason human
// action, capacity_wait.go, branch_cession_chain.go, the frozen execution
// placement). What it did not have was ONE answer to the six questions a
// person actually asks while a run is executing:
//
//	what is AO doing now / what is done / what is next / is it waiting on me /
//	is it repairing something by itself / where is it working
//
// Before this file every surface answered those by re-reading the technical
// vocabulary — a run detail page printing `needs_attention` and
// `fix_budget_exhausted`, a board card printing a step kind, a task card
// printing a run state — and the three could disagree because each mapped the
// facts itself. DerivePresentation is that mapping, once, over durable facts.
//
// # It derives, it does not decide
//
// Nothing here changes execution. Every field is either copied from a durable
// row or computed by the rules below, and an unknown value stays at its zero
// value rather than being guessed. In particular there is NO percentage: a
// bounded set of stages with a known current one is a fact; "67%" would be a
// fabrication, and §2 of the checkpoint forbids it.
//
// # The technical vocabulary survives
//
// The reason codes are not removed and not renamed. They move from the TITLE to
// the DETAIL: Presentation.Technical carries them intact, so an operator
// diagnosing a stop still reads `fix_budget_exhausted`, and a person reading the
// page first reads a sentence about what happened.

// Stage is the human status vocabulary — the answer to "what is AO doing".
//
// It is a projection of Phase, never a second lifecycle: DerivePresentation
// maps every Phase onto exactly one Stage in stageForPhase, and that function is
// the only place the mapping exists. Two surfaces rendering different stages
// for one run is therefore not a thing that can happen without changing this
// file.
type Stage string

const (
	// StagePreparing is everything before an agent is working: the run is
	// queued, its placement is being frozen or materialised, capacity is being
	// claimed.
	StagePreparing Stage = "preparing"
	// StagePlanning is a planner producing or validating a plan.
	StagePlanning Stage = "planning"
	// StageWorking is an implementer writing code.
	StageWorking Stage = "working"
	// StageReviewing is a reviewer reading the work.
	StageReviewing Stage = "reviewing"
	// StageCorrecting is AO applying changes a review asked for — its own fix
	// cycle, and also an automatic repair. Both are "AO is correcting
	// something", and a person does not have to know which machinery is doing
	// it to understand the state.
	StageCorrecting Stage = "correcting"
	// StageVerifying is verification running.
	StageVerifying Stage = "verifying"
	// StageIntegrating is verified work being moved onto its merge target.
	StageIntegrating Stage = "integrating"
	// StageWaiting is AO parked on something that clears BY ITSELF: capacity, a
	// branch another run holds, a scheduled retry. It is deliberately not
	// StageNeedsAttention — telling a person it is their turn when it is not is
	// the misreport this vocabulary exists to end.
	StageWaiting Stage = "waiting"
	// StageNeedsAttention is a stop only a person can clear.
	StageNeedsAttention Stage = "needs_attention"
	// StageCompleted is work that finished and was verified.
	StageCompleted Stage = "completed"
	// StageCancelled is work a person stopped.
	StageCancelled Stage = "cancelled"
	// StageFailed is work that ended without finishing. It is kept apart from
	// cancelled because the next thing a person does differs.
	StageFailed Stage = "failed"
)

// Terminal reports whether a stage can never change again.
func (s Stage) Terminal() bool {
	return s == StageCompleted || s == StageCancelled || s == StageFailed
}

// stageForPhase is the ONE mapping from the derived lifecycle phase onto the
// human stage. Every surface that shows a stage goes through it.
func stageForPhase(p Phase) Stage {
	switch p {
	case PhaseCancelled:
		return StageCancelled
	case PhaseFailed:
		return StageFailed
	case PhaseCompleted:
		return StageCompleted
	case PhaseNeedsAttention:
		return StageNeedsAttention
	case PhasePlanning:
		return StagePlanning
	case PhaseReviewing:
		return StageReviewing
	case PhaseFixing:
		return StageCorrecting
	case PhaseVerifying:
		return StageVerifying
	case PhaseRunning:
		return StageWorking
	case PhaseQueued:
		return StagePreparing
	case PhaseWaiting, PhaseWaitingForCapacity, PhaseBlocked, PhaseRetrying:
		return StageWaiting
	default:
		return StageWaiting
	}
}

// StageForPhase is the exported mapping, for callers that have a Lifecycle and
// need its stage without building a whole Presentation (the Board's compact
// projection).
func StageForPhase(p Phase) Stage { return stageForPhase(p) }

// ProgressState is where one stage of the progression sits relative to now.
type ProgressState string

const (
	// ProgressDone is a stage that has provably happened.
	ProgressDone ProgressState = "completed"
	// ProgressCurrent is the stage happening right now. At most one.
	ProgressCurrent ProgressState = "current"
	// ProgressFuture is a stage that has not started.
	ProgressFuture ProgressState = "future"
	// ProgressBlocked is the current stage, stopped. It is separate from
	// current because "AO is reviewing" and "AO stopped during review" are
	// different things to look at.
	ProgressBlocked ProgressState = "blocked"
	// ProgressSkipped is a stage this run provably will not have — a review
	// policy that skipped, a direct-branch run with nothing to integrate.
	ProgressSkipped ProgressState = "skipped"
)

// ProgressStage is one entry of the visible progression.
type ProgressStage struct {
	Stage Stage
	State ProgressState
	// Optional marks a stage that only some runs have (correcting,
	// integrating). A future optional stage is rendered dimmer than a future
	// mandatory one: "this may not happen" is a different promise from "this
	// has not happened yet".
	Optional bool
}

// ActionID is the closed set of things a person may be offered. Closed for the
// same reason RecoveryAction is: an action AO cannot name is an action AO must
// not offer.
type ActionID string

const (
	// ActionContinue re-enters the ordinary resume path.
	ActionContinue ActionID = "continue"
	// ActionCancel cancels the run.
	ActionCancel ActionID = "cancel"
	// ActionRepair authorizes a bounded repair.
	ActionRepair ActionID = "repair"
	// ActionCommitAndContinue is the dirty-worktree flow: show the changes,
	// propose a message, commit, verify clean, resume. Never a silent commit
	// and never an invisible stash.
	ActionCommitAndContinue ActionID = "commit_and_continue"
	// ActionViewChanges opens the working tree's pending changes.
	ActionViewChanges ActionID = "view_changes"
	// ActionViewBlockingWorkflow opens the run that holds the branch.
	ActionViewBlockingWorkflow ActionID = "view_blocking_workflow"
	// ActionWait is the explicit "leave it queued" acknowledgement. It performs
	// nothing, which is the point: the honest answer to a branch queue is that
	// waiting is a valid choice.
	ActionWait ActionID = "wait"
	// ActionAuthenticate sends the user to the provider that demanded
	// credentials.
	ActionAuthenticate ActionID = "authenticate"
	// ActionRevalidatePlan / ActionRegeneratePlan are the two answers to a
	// stale plan.
	ActionRevalidatePlan ActionID = "revalidate_plan"
	// ActionRegeneratePlan discards a plan that can no longer be trusted and
	// asks for a new one.
	ActionRegeneratePlan ActionID = "regenerate_plan"
	// ActionOpenSession opens the agent session a stop points at.
	ActionOpenSession ActionID = "open_session"
	// ActionIntegrate lands a verified isolated placement on its merge target.
	ActionIntegrate ActionID = "integrate"
	// ActionUseIsolatedWorktree is offered ONLY when placement selection was
	// automatic. An explicit direct-branch choice never gets it: turning that
	// choice into a worktree is the user's decision to make, and AO offering it
	// as a remedy is not the same as AO taking it.
	ActionUseIsolatedWorktree ActionID = "use_isolated_worktree"
)

// Action is one offer, with its own answer to "may I press this".
type Action struct {
	ID ActionID
	// Primary marks the recommended one. At most one action is primary.
	Primary bool
	// Enabled reports whether pressing it would do anything. A disabled action
	// is SHOWN, with its reason, rather than hidden: "why can't I repair this"
	// is a question the UI should answer, and a missing button does not.
	Enabled bool
	// DisabledReason is a stable code the UI renders a sentence for
	// (repair_active, repair_exhausted, repair_disabled, run_terminal,
	// not_recoverable, placement_explicit).
	DisabledReason string
}

// Technical is everything an operator diagnosing the run needs and everything a
// person reading it does not. It is retained in full and rendered SECONDARY.
type Technical struct {
	Phase           Phase
	RunState        domain.WorkflowRunState
	Attention       Attention
	AttentionReason string
	// AttentionDetail is attention.go's own English sentence about the remedy.
	// It is the fallback the UI renders when it has no localized copy for
	// AttentionReason, so a reason added to the backend is never a blank page.
	AttentionDetail string
	WaitReason      string
	NextWakeAt      *time.Time
	// PlacementGeneration/LifecycleGeneration are the two generations. A person
	// never has to understand them; an operator reading a stale-writer refusal
	// cannot do without them.
	PlacementGeneration int64
	LifecycleGeneration int64
	// ErrorClass is the typed cause on the newest attempt that recorded one.
	ErrorClass domain.WorkflowErrorClass
	// RepairRunID names the repair generation's own run, when one exists.
	RepairRunID string
	// Execution is the technical projection of WHICH execution this run's
	// status is about: the attempt, its provider, the session it owns, what
	// authority AO grants it, and the last thing AO can prove happened to it
	// (P3-D §24).
	//
	// It is projected from rows the run detail already carries, so the Board's
	// poll pays nothing extra for it. The fix-cycle authority it classifies
	// against is deliberately left UNRESOLVED here — resolving it is a review
	// read, and doing one per card on the most-polled route in the product is
	// exactly the N+1 this section forbids. An unresolved authority classifies
	// an open row as `active`, which is the conservative reading; the run
	// detail's own /recovery route carries the fully resolved answer.
	Execution RecoveryExecution
}

// PlacementChoice is who decided where this run works.
type PlacementChoice string

const (
	// PlacementChosenByUser means an explicit override was applied. §7: a run
	// with this choice and a direct-branch placement may NEVER become an
	// isolated worktree without a new explicit decision.
	PlacementChosenByUser PlacementChoice = "user"
	// PlacementChosenAutomatically means selection policy decided.
	PlacementChosenAutomatically PlacementChoice = "automatic"
	// PlacementChoiceUnknown is a run with no placement record — a legacy run,
	// or one that has not been frozen yet. Never coerced to "automatic":
	// claiming AO chose something it has no record of choosing is the
	// fabrication this whole model refuses.
	PlacementChoiceUnknown PlacementChoice = "unknown"
)

// IntegrationState is the closed vocabulary of "has this run's work landed
// where it is meant to live" (P3-B §15).
//
// It exists because "merge pending" was a generic label a surface applied to
// anything unfinished, including direct-branch runs that have nothing to merge
// at all. Every member below is a distinct fact with a distinct next step, and
// IntegrationNotRequired is a real answer rather than the absence of one.
type IntegrationState string

const (
	// IntegrationNotRequired is every direct-branch placement, and any run with
	// no placement record to reason about. The work is already where the user
	// asked for it.
	IntegrationNotRequired IntegrationState = "not_required"
	// IntegrationPending is an isolated placement whose work has not moved onto
	// its merge target yet.
	IntegrationPending IntegrationState = "pending"
	// IntegrationInProgress is an integration actually running now.
	IntegrationInProgress IntegrationState = "in_progress"
	// IntegrationIntegrated is work durably on the merge target.
	IntegrationIntegrated IntegrationState = "integrated"
	// IntegrationFailed is an integration that could not complete — today, a
	// conflict. It is separate from pending because the next step is a
	// person's.
	IntegrationFailed IntegrationState = "failed"
)

// PlacementPresentation is "where is AO working", in the terms the user chose
// it in.
type PlacementPresentation struct {
	// Known reports that a frozen placement was read. False leaves every other
	// field at its zero value.
	Known bool
	Type  domain.ExecutionPlacementType
	State domain.ExecutionPlacementState
	// ChosenBy answers §10: an automatic choice must be visible as one.
	ChosenBy PlacementChoice
	// ChoiceReason is the stable code behind ChosenBy — the applied override's
	// recorded reason for a user choice, or the selection input for an
	// automatic one ("project_execution_mode", "task_scope_downgrade").
	ChoiceReason string
	RepoPath     string
	// ExecutionBranch is where the agent writes; MergeTarget is where the work
	// is meant to land. Equal for a direct-branch placement, which is exactly
	// why such a run must never be asked whether to merge.
	ExecutionBranch string
	BaseBranch      string
	MergeTarget     string
	WorktreePath    string
	IntegratedSHA   string
	// IntegrationRequired reports that finishing this run leaves work somewhere
	// it is not meant to stay. It is FALSE for every direct-branch placement,
	// on purpose: there is no worktree to integrate, so §9's "never ask a
	// direct-branch run to merge" is a property of this field rather than of
	// each screen remembering the rule.
	IntegrationRequired bool
	// Integration is the same answer with its five distinguishable values, so a
	// surface can say "nothing to integrate" and "integration failed" instead
	// of a single generic "merge pending". IntegrationRequired is kept because
	// it is the boolean §9 is stated in; the two are derived together and can
	// never disagree.
	Integration IntegrationState
	Generation  int64
}

// TimelineEventKind is the closed vocabulary of the human timeline.
type TimelineEventKind string

// The bounded timeline vocabulary. Every member names something a person would
// recognise as having happened; there is deliberately no member for a
// heartbeat, a reconcile pass or a wake retry.
const (
	TimelineStarted        TimelineEventKind = "started"
	TimelinePlanned        TimelineEventKind = "planned"
	TimelineWorkerLaunched TimelineEventKind = "worker_launched"
	TimelineWorkCompleted  TimelineEventKind = "work_completed"
	TimelineReviewStarted  TimelineEventKind = "review_started"
	TimelineReviewVerdict  TimelineEventKind = "review_verdict"
	TimelineFixStarted     TimelineEventKind = "fix_started"
	TimelineRepairStarted  TimelineEventKind = "repair_started"
	TimelineVerified       TimelineEventKind = "verified"
	TimelineIntegrated     TimelineEventKind = "integrated"
	TimelineProviderFailed TimelineEventKind = "provider_failed"
	TimelineStopped        TimelineEventKind = "stopped"
	TimelineCompleted      TimelineEventKind = "completed"
	TimelineCancelled      TimelineEventKind = "cancelled"
	TimelineFailed         TimelineEventKind = "failed"
)

// TimelineEvent is one line of the activity timeline.
//
// The timeline is BOUNDED and it is not the checkpoint ledger: heartbeats,
// wake retries and reconcile passes are deliberately absent, because a person
// asking "what has happened" is not asking to read a log. Detail carries the
// technical qualifier (a harness name, an error class, a verdict) so the line
// stays short and the specificity is still there.
type TimelineEvent struct {
	At     time.Time
	Kind   TimelineEventKind
	Detail string
}

// Presentation is the whole human projection of one run.
type Presentation struct {
	Stage Stage
	// RequiresHuman is the single flag every surface uses to decide whether to
	// interrupt somebody. It is Attention == AttentionHuman, never the run
	// state: a run durably parked in needs_attention that AO is still retrying
	// by itself must not read as the user's turn.
	RequiresHuman bool
	// AutomaticActionActive reports AO is doing something about a problem right
	// now — a repair generation in flight, a scheduled retry, a provider
	// failover. §5: while it is true the UI must not ask the user anything and
	// must not offer a second, duplicate remedy.
	AutomaticActionActive bool
	// SummaryCode is the stable key the UI renders its sentence from. It is the
	// canonical attention reason for a stop, the admission/wait reason for a
	// wait, and the stage itself otherwise — so a code the UI has no copy for
	// still names something real.
	SummaryCode string
	// RecommendedAction is the one thing AO suggests, or empty when the honest
	// answer is that nothing is required.
	RecommendedAction ActionID
	Actions           []Action
	Progress          []ProgressStage
	Placement         PlacementPresentation
	Timeline          []TimelineEvent
	// LastMeaningfulActivityAt is when this run last did something a person
	// would recognise as having happened (P3-B §11). It is the newest entry of
	// the bounded Timeline above and nothing else — which is exactly why it is
	// meaningful: heartbeats, reconcile passes and wake retries never enter
	// that vocabulary, so a run parked for six hours cannot read as "active 2
	// seconds ago" because a poller touched it.
	LastMeaningfulActivityAt time.Time
	Technical                Technical
}

// PresentationInput is everything DerivePresentation reads. It is a plain value
// so the projection stays a pure function testable without a store — the same
// property DeriveLifecycle has and for the same reason.
type PresentationInput struct {
	Detail    RunDetail
	Lifecycle Lifecycle
	// Placements is the run's placement records; only the current generation
	// for the run's own obligation is read. Empty is meaningful: no placement
	// authority is wired, or none is frozen yet.
	Placements []PlacementView
	// Overrides is the placement override audit trail. An `applied` row is what
	// makes a placement a USER choice rather than a policy one.
	Overrides []PlacementOverrideView
	// Admission is why the run has not launched, when it has not.
	Admission AdmissionStateView
	Now       time.Time
}

// DerivePresentation projects one run onto the human status model.
func DerivePresentation(in PresentationInput) Presentation {
	p := Presentation{
		Stage:         stageForPhase(in.Lifecycle.Phase),
		RequiresHuman: in.Lifecycle.Attention == AttentionHuman,
		Technical: Technical{
			Phase:           in.Lifecycle.Phase,
			RunState:        in.Detail.Run.State,
			Attention:       in.Lifecycle.Attention,
			AttentionReason: in.Lifecycle.AttentionReason,
			AttentionDetail: in.Lifecycle.AttentionAction,
			WaitReason:      in.Lifecycle.WaitReason,
			NextWakeAt:      in.Lifecycle.NextWakeAt,
			ErrorClass:      latestErrorClass(in.Detail),
			RepairRunID:     in.Detail.Repair.RunID,
			Execution:       deriveRecoveryExecution(in.Detail, "", FixAuthority{}),
		},
	}
	p.Placement = derivePlacementPresentation(in)
	p.Technical.PlacementGeneration = p.Placement.Generation
	p.Technical.LifecycleGeneration = lifecycleGenerationOf(in.Placements, in.Detail.Run)

	// §5: a repair in flight is AO acting, not a person's turn. It overrides
	// the stage as well as the flag, because a run parked in needs_attention
	// while its repair generation is working is "AO is correcting this", and
	// showing the parked stop as the headline is what made repairs unreadable.
	p.AutomaticActionActive = automaticActionActive(in)
	if in.Detail.Repair.Active && !p.Stage.Terminal() {
		p.Stage = StageCorrecting
		p.RequiresHuman = false
	}

	p.SummaryCode = summaryCode(in, p)
	p.Progress = deriveProgress(in, p)
	p.Actions, p.RecommendedAction = deriveActions(in, p)
	p.Timeline = deriveTimeline(in)
	p.LastMeaningfulActivityAt = lastMeaningfulActivity(in, p.Timeline)
	return p
}

// lastMeaningfulActivity is the newest bounded-timeline entry, falling back to
// the run's creation.
//
// The fallback is CreatedAt rather than UpdatedAt on purpose: updated_at moves
// for bookkeeping — a reconcile writing a checkpoint, a poller stamping a
// state it did not change — and a "last activity" that tracked it would report
// a stalled run as freshly active, which is the misreport §11 exists to end.
func lastMeaningfulActivity(in PresentationInput, timeline []TimelineEvent) time.Time {
	if n := len(timeline); n > 0 {
		return timeline[n-1].At
	}
	return in.Detail.Run.CreatedAt
}

// automaticActionActive reports that AO is DOING something about a problem
// right now, so no remedy should be asked of anybody.
//
// Three sources, and every one of them is a durable fact rather than an
// inference from a state name: a repair generation whose run exists and has not
// finished; a self-remediable stop (attention.go already classified it, and its
// classification is what "AO will handle this" means); and an admission wait
// that clears by itself.
func automaticActionActive(in PresentationInput) bool {
	if in.Detail.Repair.Active {
		return true
	}
	if in.Lifecycle.Attention == AttentionInternal {
		return true
	}
	if in.Admission.WaitingReason != "" && in.Admission.AutoResume {
		return true
	}
	return false
}

// summaryCode picks the stable key the UI renders its sentence from.
//
// Order matters and mirrors what a person needs first: a stop they must clear,
// then what AO is waiting on, then what AO is doing. A repair in flight
// deliberately outranks the stop it is repairing — that is the §5 requirement
// that AO not ask a question it is already answering.
func summaryCode(in PresentationInput, p Presentation) string {
	if in.Detail.Repair.Active {
		return "repair_active"
	}
	if p.RequiresHuman && in.Lifecycle.AttentionReason != "" {
		return in.Lifecycle.AttentionReason
	}
	if in.Detail.BranchWait != nil {
		return string(domain.AdmissionBranchWait)
	}
	if in.Detail.CapacityWait != nil {
		return string(domain.AdmissionCapacityWait)
	}
	if in.Admission.WaitingReason != "" {
		return string(in.Admission.WaitingReason)
	}
	if in.Lifecycle.AttentionReason != "" {
		return in.Lifecycle.AttentionReason
	}
	return string(p.Stage)
}

// currentPlacementFor returns the current placement record for the run's own
// obligation, if there is one.
func currentPlacementFor(views []PlacementView) (PlacementView, bool) {
	for _, v := range views {
		if v.Current {
			return v, true
		}
	}
	return PlacementView{}, false
}

func lifecycleGenerationOf(views []PlacementView, _ domain.WorkflowRun) int64 {
	if v, ok := currentPlacementFor(views); ok {
		return v.LifecycleGeneration
	}
	return 0
}

// derivePlacementPresentation answers "where is AO working, and who decided
// that".
//
// The user-choice test is an APPLIED override naming this placement's
// generation. A merely `requested` override is not a choice AO acted on, and
// reporting it as one would tell a person their decision took effect when
// P1-E's whole point is that it did not until a freeze or a transition consumed
// it.
func derivePlacementPresentation(in PresentationInput) PlacementPresentation {
	view, ok := currentPlacementFor(in.Placements)
	if !ok {
		return PlacementPresentation{ChosenBy: PlacementChoiceUnknown}
	}
	out := PlacementPresentation{
		Known:           true,
		Type:            view.Type,
		State:           view.State,
		ChosenBy:        PlacementChosenAutomatically,
		ChoiceReason:    string(view.Provenance),
		RepoPath:        view.RepoPath,
		ExecutionBranch: view.ExecutionBranch,
		BaseBranch:      view.BaseBranch,
		MergeTarget:     view.MergeTarget,
		WorktreePath:    view.WorktreePath,
		IntegratedSHA:   view.IntegratedSHA,
		Generation:      view.PlacementGeneration,
	}
	for _, ov := range in.Overrides {
		if ov.State == domain.PlacementOverrideApplied &&
			ov.AppliedGeneration == view.PlacementGeneration &&
			ov.Requested.Explicit() {
			out.ChosenBy = PlacementChosenByUser
			out.ChoiceReason = ov.Reason
			break
		}
	}
	// §9: a direct-branch placement has nothing to integrate, ever. The work is
	// already on the branch the user named.
	out.IntegrationRequired = view.Type.Isolated() && view.IntegratedSHA == "" &&
		view.State != domain.PlacementTerminal
	out.Integration = integrationStateFor(view)
	return out
}

// integrationStateFor answers §15 from the placement record alone.
//
// Order matters: a recorded integrated SHA is proof, and outranks whatever
// state the row retired into. A direct-branch placement short-circuits first,
// because there is no state it could ever be in that would make integration a
// thing it owes anybody.
func integrationStateFor(view PlacementView) IntegrationState {
	if !view.Type.Isolated() {
		return IntegrationNotRequired
	}
	if view.IntegratedSHA != "" || view.State == domain.PlacementIntegrated {
		return IntegrationIntegrated
	}
	switch view.State {
	case domain.PlacementIntegrating:
		return IntegrationInProgress
	case domain.PlacementConflict:
		return IntegrationFailed
	case domain.PlacementTerminal:
		// A placement retired with nothing recorded as integrated is finished
		// with, and AO has no evidence the work moved. Reporting "pending"
		// would promise an integration nothing is going to perform.
		return IntegrationNotRequired
	}
	return IntegrationPending
}

// deriveProgress builds the visible progression.
//
// It is built from the run's OWN steps, not from a fixed template, so a run
// whose review policy skipped a review does not show a review that will never
// happen, and a direct-branch run does not show an integration stage it has no
// use for. Optional stages that have not happened are still listed — "this may
// yet happen" is information — but marked Optional so the UI can render them
// as less certain than the mandatory ones.
func deriveProgress(in PresentationInput, p Presentation) []ProgressStage {
	stages := []ProgressStage{{Stage: StagePreparing}}
	if in.Detail.Plan != nil {
		stages = append(stages, ProgressStage{Stage: StagePlanning})
	}
	var hasWork, hasReview, hasFix, hasVerify bool
	stepState := map[domain.WorkflowStepKind]domain.WorkflowStepState{}
	for _, s := range in.Detail.Steps {
		switch s.Step.Kind {
		case domain.WorkflowStepWork:
			hasWork = true
		case domain.WorkflowStepReview:
			hasReview = true
		case domain.WorkflowStepFix:
			hasFix = true
		case domain.WorkflowStepVerify:
			hasVerify = true
		}
		stepState[s.Step.Kind] = s.Step.State
	}
	if hasWork {
		stages = append(stages, ProgressStage{Stage: StageWorking})
	}
	if hasReview {
		stages = append(stages, ProgressStage{Stage: StageReviewing})
	}
	if hasFix {
		stages = append(stages, ProgressStage{Stage: StageCorrecting, Optional: true})
	}
	if hasVerify {
		stages = append(stages, ProgressStage{Stage: StageVerifying})
	}
	if p.Placement.Type.Isolated() {
		stages = append(stages, ProgressStage{Stage: StageIntegrating, Optional: true})
	}
	stages = append(stages, ProgressStage{Stage: StageCompleted})

	current := p.Stage
	// A terminal-but-not-completed run has no "current" stage in the
	// progression: it stopped somewhere, and where it stopped is what the
	// blocked marker below says.
	blocked := p.Stage == StageNeedsAttention || p.Stage == StageFailed || p.Stage == StageWaiting
	if blocked {
		current = stageForStoppedRun(in)
	}
	currentIndex := -1
	for i, st := range stages {
		if st.Stage == current {
			currentIndex = i
			break
		}
	}
	for i := range stages {
		switch {
		case p.Stage == StageCompleted:
			stages[i].State = ProgressDone
		case currentIndex < 0:
			stages[i].State = ProgressFuture
		case i < currentIndex:
			stages[i].State = ProgressDone
		case i == currentIndex:
			if blocked {
				stages[i].State = ProgressBlocked
			} else {
				stages[i].State = ProgressCurrent
			}
		default:
			stages[i].State = ProgressFuture
		}
	}
	// A step that provably finished is Done even when the progression's current
	// pointer sits earlier — a review that concluded before a fix cycle
	// reopened it, for instance.
	for i := range stages {
		if stages[i].State != ProgressFuture {
			continue
		}
		kind, ok := stepKindForStage(stages[i].Stage)
		if ok && stepState[kind] == domain.WorkflowStepCompleted {
			stages[i].State = ProgressDone
		}
	}
	if p.Stage == StageCancelled {
		for i := range stages {
			if stages[i].State == ProgressFuture {
				stages[i].State = ProgressSkipped
			}
		}
	}
	return stages
}

func stepKindForStage(s Stage) (domain.WorkflowStepKind, bool) {
	switch s {
	case StageWorking:
		return domain.WorkflowStepWork, true
	case StageReviewing:
		return domain.WorkflowStepReview, true
	case StageCorrecting:
		return domain.WorkflowStepFix, true
	case StageVerifying:
		return domain.WorkflowStepVerify, true
	default:
		return "", false
	}
}

// stageForStoppedRun names WHERE a stopped run stopped, which the phase alone
// cannot say: `needs_attention` is a state, not a place. It reads the run's own
// steps — the running one, else the first unfinished one.
func stageForStoppedRun(in PresentationInput) Stage {
	if kind := activeStepKind(in.Detail); kind != "" {
		switch kind {
		case domain.WorkflowStepPlan:
			return StagePlanning
		case domain.WorkflowStepWork:
			return StageWorking
		case domain.WorkflowStepReview:
			return StageReviewing
		case domain.WorkflowStepFix:
			return StageCorrecting
		case domain.WorkflowStepVerify:
			return StageVerifying
		}
	}
	if in.Detail.Plan != nil {
		return StagePlanning
	}
	return StagePreparing
}

// deriveActions answers "what can I do now".
//
// Two rules run through all of it. First, an action is offered because a
// durable fact authorises it, never because a state name looked like it might:
// Continue comes from Lifecycle.CanContinue, Repair from the repair lifecycle
// and the run's frozen repair policy, the placement remedies from the placement
// record. Second, an action AO will not perform is SHOWN DISABLED with its
// reason rather than hidden, because "why is this greyed out" is answerable and
// "where did the button go" is not.
func deriveActions(in PresentationInput, p Presentation) ([]Action, ActionID) {
	var actions []Action
	recommended := ActionID("")
	add := func(a Action) { actions = append(actions, a) }

	terminal := p.Stage.Terminal()

	// §5: while AO is repairing, every remedy that could open a SECOND action
	// is disabled. Not hidden — a person watching a repair should be able to
	// see that Resume exists and why it is unavailable right now.
	if in.Detail.Repair.Active {
		add(Action{ID: ActionContinue, Enabled: false, DisabledReason: "repair_active"})
		add(Action{ID: ActionRepair, Enabled: false, DisabledReason: "repair_active"})
		add(Action{ID: ActionCancel, Enabled: !terminal})
		return actions, ""
	}

	switch p.SummaryCode {
	case "dirty_worktree":
		// §17. The commit flow is the recommendation; the escape hatches are
		// offered beside it, and nothing is stashed or committed silently.
		add(Action{ID: ActionCommitAndContinue, Primary: true, Enabled: true})
		add(Action{ID: ActionViewChanges, Enabled: true})
		add(Action{ID: ActionCancel, Enabled: !terminal})
		return actions, ActionCommitAndContinue
	case string(domain.AdmissionBranchWait), ReasonBranchQueued:
		// §18. Waiting is legitimate and is the recommendation. A worktree is
		// offered ONLY when the placement was chosen automatically — turning an
		// explicit "branch actual" into a worktree is the user's call, and AO
		// proposing it as the obvious remedy is how the expectation bug
		// happened in the first place.
		add(Action{ID: ActionWait, Primary: true, Enabled: true})
		add(Action{ID: ActionViewBlockingWorkflow, Enabled: in.Detail.BranchWait != nil &&
			in.Detail.BranchWait.HeldByWorkflowRunID != ""})
		if p.Placement.Known && p.Placement.ChosenBy != PlacementChosenByUser &&
			p.Placement.Type == domain.PlacementDirectBranch {
			add(Action{ID: ActionUseIsolatedWorktree, Enabled: true})
		} else if p.Placement.ChosenBy == PlacementChosenByUser {
			add(Action{ID: ActionUseIsolatedWorktree, Enabled: false, DisabledReason: "placement_explicit"})
		}
		add(Action{ID: ActionCancel, Enabled: !terminal})
		return actions, ActionWait
	case string(domain.AdmissionCapacityWait), ReasonPlannerCapacityWait, ReasonReviewCapacityRetry,
		ReasonIncidentDiagnosisCapacityWait:
		// §19. A capacity wait is not an error and gets no Repair button.
		add(Action{ID: ActionCancel, Enabled: !terminal})
		return actions, ""
	case ReasonPlannerAmbiguous, "plan_stale":
		// §22.
		add(Action{ID: ActionRevalidatePlan, Primary: true, Enabled: true})
		add(Action{ID: ActionRegeneratePlan, Enabled: true})
		add(Action{ID: ActionCancel, Enabled: !terminal})
		return actions, ActionRevalidatePlan
	}

	// Authentication is its own answer wherever the disposition names it.
	if disp, ok := attentionDispositions[in.Lifecycle.AttentionReason]; ok &&
		disp.Recovery == domain.RecoveryAuthenticate {
		add(Action{ID: ActionAuthenticate, Primary: true, Enabled: true})
		add(Action{ID: ActionContinue, Enabled: in.Lifecycle.CanContinue})
		add(Action{ID: ActionCancel, Enabled: !terminal})
		return actions, ActionAuthenticate
	}

	// §26: a finished isolated placement whose work has not landed is the one
	// completed state with something left to do.
	if p.Stage == StageCompleted && p.Placement.IntegrationRequired {
		add(Action{ID: ActionIntegrate, Primary: true, Enabled: true})
		add(Action{ID: ActionViewChanges, Enabled: true})
		return actions, ActionIntegrate
	}
	if terminal {
		return actions, ""
	}

	if in.Lifecycle.CanContinue {
		add(Action{ID: ActionContinue, Primary: true, Enabled: true})
		recommended = ActionContinue
	} else if p.RequiresHuman {
		add(Action{ID: ActionContinue, Enabled: false, DisabledReason: "not_recoverable"})
	}
	switch {
	case in.Detail.Repair.Exhausted:
		add(Action{ID: ActionRepair, Enabled: false, DisabledReason: "repair_exhausted"})
	case p.RequiresHuman:
		add(Action{ID: ActionRepair, Enabled: true})
	}
	if sessionOfStoppedStep(in.Detail) != "" {
		add(Action{ID: ActionOpenSession, Enabled: true})
	}
	add(Action{ID: ActionCancel, Enabled: true})
	return actions, recommended
}

// sessionOfStoppedStep names the agent session a stop points at, when one
// exists: the running step's, else the newest step that has a session.
func sessionOfStoppedStep(d RunDetail) string {
	latest := ""
	for _, s := range d.Steps {
		if s.Step.SessionID == nil || *s.Step.SessionID == "" {
			continue
		}
		latest = *s.Step.SessionID
		if s.Step.State == domain.WorkflowStepRunning {
			return latest
		}
	}
	return latest
}

// deriveTimeline builds the bounded human timeline.
//
// Every entry comes from a durable row that already exists on the RunDetail —
// no new reads and, deliberately, no checkpoint ledger: a timeline that
// included every heartbeat and reconcile pass would be the log a person opened
// this page to avoid. Provider failures are folded to one line per failed
// attempt because a failover is a thing that happened, not noise.
func deriveTimeline(in PresentationInput) []TimelineEvent {
	events := []TimelineEvent{{At: in.Detail.Run.CreatedAt, Kind: TimelineStarted}}
	if in.Detail.Plan != nil && in.Detail.Plan.Status == domain.WorkflowPlanApproved {
		events = append(events, TimelineEvent{At: in.Detail.Plan.UpdatedAt, Kind: TimelinePlanned})
	}
	for _, s := range in.Detail.Steps {
		if s.Step.Kind == domain.WorkflowStepAdvance {
			continue
		}
		for _, a := range s.Attempts {
			switch s.Step.Kind {
			case domain.WorkflowStepWork:
				events = append(events, TimelineEvent{At: a.StartedAt, Kind: TimelineWorkerLaunched, Detail: a.Harness})
			case domain.WorkflowStepReview:
				events = append(events, TimelineEvent{At: a.StartedAt, Kind: TimelineReviewStarted, Detail: a.Harness})
			case domain.WorkflowStepFix:
				events = append(events, TimelineEvent{At: a.StartedAt, Kind: TimelineFixStarted, Detail: a.Harness})
			}
			if a.Outcome == domain.WorkflowAttemptFailed && a.FinishedAt != nil {
				events = append(events, TimelineEvent{
					At: *a.FinishedAt, Kind: TimelineProviderFailed,
					Detail: strings.TrimSpace(a.Harness + " " + string(a.ErrorClass)),
				})
			}
		}
		if s.Step.State == domain.WorkflowStepCompleted && s.Step.CompletedAt != nil {
			switch s.Step.Kind {
			case domain.WorkflowStepWork:
				events = append(events, TimelineEvent{At: *s.Step.CompletedAt, Kind: TimelineWorkCompleted})
			case domain.WorkflowStepReview:
				verdict := ""
				if s.Review != nil {
					verdict = string(s.Review.Verdict)
				}
				events = append(events, TimelineEvent{At: *s.Step.CompletedAt, Kind: TimelineReviewVerdict, Detail: verdict})
			case domain.WorkflowStepVerify:
				events = append(events, TimelineEvent{At: *s.Step.CompletedAt, Kind: TimelineVerified})
			}
		}
	}
	if in.Detail.Repair.Attempt > 0 && in.Detail.Repair.RunID != "" {
		events = append(events, TimelineEvent{
			At: in.Detail.LatestCheckpointAt, Kind: TimelineRepairStarted, Detail: in.Detail.Repair.RunID,
		})
	}
	if p, ok := currentPlacementFor(in.Placements); ok && p.IntegratedSHA != "" {
		events = append(events, TimelineEvent{At: in.Detail.Run.UpdatedAt, Kind: TimelineIntegrated, Detail: p.IntegratedSHA})
	}
	if in.Lifecycle.Attention == AttentionHuman && in.Detail.StopAuthorityPhase != "" {
		events = append(events, TimelineEvent{
			At: in.Detail.StopAuthorityAt, Kind: TimelineStopped, Detail: in.Detail.StopAuthorityPhase,
		})
	}
	switch in.Detail.Run.State {
	case domain.WorkflowRunCompleted:
		if in.Detail.Run.CompletedAt != nil {
			events = append(events, TimelineEvent{At: *in.Detail.Run.CompletedAt, Kind: TimelineCompleted})
		}
	case domain.WorkflowRunCancelled:
		if in.Detail.Run.CancelledAt != nil {
			events = append(events, TimelineEvent{At: *in.Detail.Run.CancelledAt, Kind: TimelineCancelled})
		}
	case domain.WorkflowRunFailed:
		events = append(events, TimelineEvent{At: in.Detail.Run.UpdatedAt, Kind: TimelineFailed})
	}
	// Drop entries with no timestamp: a line whose "when" is the zero time
	// would sort to 1 January year 1 and claim a thing happened before the run
	// existed.
	kept := events[:0]
	for _, e := range events {
		if e.At.IsZero() {
			continue
		}
		kept = append(kept, e)
	}
	events = kept
	sort.SliceStable(events, func(i, j int) bool { return events[i].At.Before(events[j].At) })
	return events
}
