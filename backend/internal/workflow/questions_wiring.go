package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	claudecodeq "github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/claudecode"
	codexq "github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/codex"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/questions"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
)

// QuestionsStore is the narrow persistence contract Checkpoint 8K-A's
// reconcile-loop wiring needs. Satisfied by *store.Store (aliased as
// sqlite.Store), which already implements each of these directly against
// the workflow_questions table (see workflow_questions_store.go). Optional:
// a nil QuestionsStore means detection/delivery/dispatch-guards/cancel are
// all no-ops, the same convention every other optional Deps field uses.
type QuestionsStore interface {
	questions.Store
	questions.DeliveryStore
	ListOpenWorkflowQuestionsByRun(ctx stdctx.Context, runID string) ([]domain.WorkflowQuestion, error)
	CancelOpenWorkflowQuestionsByRun(ctx stdctx.Context, runID string) (int64, error)

	// ListWorkflowQuestionsByRun (all states, any time) backs Checkpoint
	// 8K-B pass 2's resolving-question scan (decision_resolver_wiring.go)
	// and the widened dispatch-guard check in hasOpenQuestion below — reused
	// rather than adding a second narrower query, since *store.Store already
	// exposes it for the human-answer API (service/questions/answer_service.go).
	ListWorkflowQuestionsByRun(ctx stdctx.Context, runID string) ([]domain.WorkflowQuestion, error)
	// SetWorkflowQuestionResolvingRunID and TransitionWorkflowQuestionState
	// back Checkpoint 8K-B pass 2's resolver dispatch/observe wiring
	// (decision_resolver_wiring.go).
	SetWorkflowQuestionResolvingRunID(ctx stdctx.Context, questionID string, runID *string) (bool, error)
	TransitionWorkflowQuestionState(ctx stdctx.Context, id string, expected, next domain.QuestionState, reason string, now time.Time) (bool, error)

	// Resolution-attempt CRUD (Checkpoint 8K-B, pass 1 store methods, wired
	// into the reconcile loop by pass 2): *store.Store already implements
	// every one of these against workflow_question_resolutions.
	InsertWorkflowQuestionResolution(ctx stdctx.Context, r domain.WorkflowQuestionResolution) (domain.WorkflowQuestionResolution, error)
	GetWorkflowQuestionResolution(ctx stdctx.Context, id string) (domain.WorkflowQuestionResolution, bool, error)
	GetCurrentResolutionForQuestion(ctx stdctx.Context, questionID string) (domain.WorkflowQuestionResolution, bool, error)
	TransitionResolutionStatus(ctx stdctx.Context, id string, expectedStatus, newStatus domain.ResolutionStatus, answer, reasonSummary string, evidenceReferences []string, certainty *domain.QuestionCertainty, requiresHuman bool, updatedAt time.Time, completedAt *time.Time) (bool, error)
	SetResolutionResolverSessionID(ctx stdctx.Context, id string, resolverSessionID string) (bool, error)
	ListRunningResolutions(ctx stdctx.Context) ([]domain.WorkflowQuestionResolution, error)
	CancelRunningResolutionsByQuestion(ctx stdctx.Context, questionID string, at time.Time) (int64, error)
}

// PaneReader is the bounded pane-text capture path Checkpoint 8K-A's
// detector needs — ports.Runtime.GetOutput, narrowed to the single method
// used here. Reused unmodified from the runtime adapter already wired for
// terminal/review/session-messaging use, never a second capture mechanism.
type PaneReader interface {
	GetOutput(ctx stdctx.Context, handle ports.RuntimeHandle, lines int) (string, error)
}

// questionMessageSender adapts workflow.MessageSender to
// questions.MessageSender. The two interfaces are structurally identical
// (both are exactly *session_manager.Manager.Send's shape) but declared
// independently to avoid an import cycle: workflow imports service/questions
// for the dispatch guards below, so service/questions cannot import
// workflow.MessageSender back. This wrapper is the "few lines" adapter
// rather than a redesign of either interface.
type questionMessageSender struct{ sender MessageSender }

func (a questionMessageSender) Send(ctx stdctx.Context, id domain.SessionID, message string, attachment *ports.SpawnAttachment) error {
	return a.sender.Send(ctx, id, message, attachment)
}

// questionHarnessParsers maps an asking harness to its
// ports.QuestionPaneParser. Only harnesses with an actual parser implemented
// (Checkpoint 8K-A: Codex and Claude Code) are present; an unrecognized
// harness simply never gets a detection attempt — no generic bare-"?"
// fallback.
var questionHarnessParsers = map[domain.AgentHarness]ports.QuestionPaneParser{
	domain.AgentHarness("codex"):       codexq.QuestionParser{},
	domain.AgentHarness("claude-code"): claudecodeq.QuestionParser{},
}

// dialogHarnessParsers and dialogHarnessResponders are P3-C's structured
// dialog-answering registries: which providers can have their interactive
// prompts READ exactly (cursor included) and ANSWERED by keystroke.
//
// Membership is the entire definition of "AO can answer this provider's
// prompts". A harness absent from either is reported honestly as unsupported
// (§15) and its questions go to a person; it is never answered by typing into
// it, which is the failure this whole capability replaces.
//
// Claude Code only, for now, and deliberately: it is the one provider whose
// select dialog AO has observed live, parsed from a real captured pane and
// answered against a real terminal. A provider added here without that evidence
// would be a guess about somebody else's UI.
var dialogHarnessParsers = map[domain.AgentHarness]ports.DialogPaneParser{
	domain.HarnessClaudeCode: claudecodeq.DialogParser{},
}

var dialogHarnessResponders = map[domain.AgentHarness]ports.DialogResponder{
	domain.HarnessClaudeCode: claudecodeq.DialogResponder{},
}

func dialogParserFor(h domain.AgentHarness) (ports.DialogPaneParser, bool) {
	p, ok := dialogHarnessParsers[h]
	return p, ok
}

func dialogResponderFor(h domain.AgentHarness) (ports.DialogResponder, bool) {
	r, ok := dialogHarnessResponders[h]
	return r, ok
}

// SupportsStructuredDialogResponse reports whether AO can answer this
// provider's interactive prompts without typing into them (§15).
//
// Exported because the Advisor needs it: a run whose autonomy policy decided a
// question AO cannot deliver must stop reading as "AO is handling this" and
// become a person's, honestly, rather than waiting forever.
func SupportsStructuredDialogResponse(h domain.AgentHarness) bool {
	_, hasParser := dialogHarnessParsers[h]
	_, hasResponder := dialogHarnessResponders[h]
	return hasParser && hasResponder
}

// reconcileQuestions is Checkpoint 8K-A's read-time detection + delivery
// pass, called once per GetRun/Reconcile near advanceReviewFixCycle —
// mirroring observeWorkStep's "derive facts at read time" convention, never
// a background poller.
//
// Delivery is swept unconditionally every call (both right after a fresh
// answer and on every subsequent GetRun) so a daemon restart between
// "answered" and "delivered" recovers on the very next read — the
// delivered flag makes redundant sweeps a safe no-op.
//
// Detection only fires for a harness-bearing step (work/fix/review) whose
// session is currently waiting_input/blocked AND has no open question yet
// for that step — never a continuous poll regardless of activity state,
// and never a re-scrape once an open question already exists.
// reconcileQuestions returns a non-empty nextAction only for Checkpoint
// 8K-B's read-time-derived "waiting_for_capacity" override (see
// reconcileDecisionResolvers); the pre-existing "waiting_for_decision"
// override for a pending/human_required question is still derived
// separately by the caller (GetRun) via ListOpenWorkflowQuestionsByRun,
// unchanged from 8K-A.
func (c *Coordinator) reconcileQuestions(ctx stdctx.Context, run domain.WorkflowRun, steps []domain.WorkflowStep) (string, error) {
	if c.questionsStore == nil {
		return "", nil
	}
	now := c.clock()

	// Best-effort, and the "best-effort" is the point (P3-C).
	//
	// Unconditional on a message sender: the STRUCTURED half of this sweep
	// answers a prompt by pressing keys, and gating it on the ability to type
	// would make the capability depend on the very mechanism it exists to
	// replace.
	//
	// This runs on the READ path -- every GetRun, so every board poll and
	// every advice request. A worker session that refuses the write (a
	// provider TUI guard suppressing input while it is blocked, a session
	// that has gone away) made this return an error, which propagated out of
	// GetRun as a 500: the run's detail page, its advice and its recovery
	// assessment ALL became unreadable because a message could not be
	// delivered. That is the opposite of what a delivery failure should
	// cost, and it is exactly the state in which a person most needs to read
	// the run.
	//
	// Nothing is lost by continuing: the answer is already durable, the row
	// stays delivered=0, and the very next sweep retries it. See
	// DeliverAnswered's own restart-recovery contract.
	c.deliverAnsweredQuestions(ctx, run, now)

	// Detection runs BEFORE resolver dispatch/observe below so a question
	// freshly captured this same call (auto_resolvable -> state=resolving)
	// is eligible for dispatch within the same GetRun/Reconcile pass,
	// matching the responsiveness convention the policy-resolvable path
	// already established (answered+delivered synchronously within one
	// call) — never a second poll cycle's worth of extra latency just to
	// notice a question that was just inserted.
	if !run.State.Terminal() && c.sessionFacts != nil && c.paneReader != nil {
		for _, step := range steps {
			if step.State.Terminal() || step.SessionID == nil || *step.SessionID == "" {
				continue
			}
			switch step.Kind {
			case domain.WorkflowStepWork, domain.WorkflowStepFix, domain.WorkflowStepReview:
			default:
				continue
			}
			if err := c.detectQuestionForStep(ctx, run, step, now); err != nil {
				return "", err
			}
		}
	}

	// Checkpoint 8K-B pass 2: dispatch/observe resolving-state questions
	// regardless of whether sessionFacts/paneReader are wired — resolver
	// dispatch/observation never needs a live pane capture, only already-
	// persisted question/resolution rows.
	waitingForCapacity, err := c.reconcileDecisionResolvers(ctx, run, now)
	if err != nil {
		return "", err
	}
	return waitingForCapacity, nil
}

// detectQuestionForStep captures, parses, classifies, and (if
// policy_resolvable) resolves+delivers a single step's stuck-on-a-question
// moment. Best-effort on pane-capture failure: a transient GetOutput error
// just means the next poll tries again, never a hard failure of the whole
// GetRun/Reconcile call.
func (c *Coordinator) detectQuestionForStep(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, now time.Time) error {
	sessionID := domain.SessionID(*step.SessionID)
	sess, found, err := c.sessionFacts.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if !found || !sess.Activity.State.NeedsInput() {
		return nil
	}

	// Widened (Checkpoint 8K-B) to also skip re-scraping while a
	// state=resolving question is already open for this step — mirrors
	// hasOpenQuestion's own widening, kept as a direct local check here
	// (rather than calling hasOpenQuestion) so the "don't re-scrape" and
	// "block dispatch" concerns stay independently readable, same as before.
	stepIDStrForGuard := step.ID
	if open, err := c.hasOpenQuestion(ctx, run.ID, &stepIDStrForGuard); err != nil {
		return err
	} else if open {
		return nil
	}

	handle := ports.RuntimeHandle{ID: sess.Metadata.RuntimeHandleID}
	if handle.ID == "" {
		return nil
	}
	paneText, err := c.paneReader.GetOutput(ctx, handle, questions.PaneCaptureRangeLines)
	if err != nil {
		// A pane AO cannot read yields no question. That is the same outcome as
		// a pane with no question in it, and both must leave the run alone.
		//nolint:nilerr // an unreadable pane observes nothing; it is not a failure.
		return nil
	}

	harness := sess.Harness
	parser := questionHarnessParsers[harness]

	var branch, worktreePath, workspaceFingerprint string
	if cp, hasCP, cerr := c.store.GetLatestWorkflowCheckpointByStep(ctx, step.ID); cerr == nil && hasCP {
		branch, worktreePath = cp.Branch, cp.WorktreePath
		// Checkpoint 8K-B: thread the workspace fingerprint into the
		// question fingerprint so the same question text under a genuinely
		// different diff is a NEW question, not a dedup no-op — reusing the
		// step's own latest checkpoint (already loaded above for
		// branch/worktreePath) rather than a fresh ObserveWorkspace shell-out,
		// which detectQuestionForStep must not add (this is a best-effort
		// poll-time hot path, not a dispatch decision). FingerprintAfter is
		// the freshest observed state; FingerprintBefore covers the window
		// before the step's own work has produced one yet.
		workspaceFingerprint = cp.FingerprintAfter
		if workspaceFingerprint == "" {
			workspaceFingerprint = cp.FingerprintBefore
		}
	}

	policy := policyForRun(run)
	stepID := domain.WorkflowStepID(step.ID)
	newID := c.newID

	res, err := questions.Detect(ctx, c.questionsStore, parser, questions.DetectInput{
		RunID:                         domain.WorkflowRunID(run.ID),
		StepID:                        &stepID,
		SessionID:                     &sessionID,
		AskingHarness:                 harness,
		AskingRole:                    string(step.Kind),
		PaneText:                      paneText,
		CaptureProvider:               "tmux",
		PolicyVersionAtCapture:        run.PolicyVersion,
		WorkspaceFingerprintAtCapture: workspaceFingerprint,
		Branch:                        branch,
		WorktreePath:                  worktreePath,
		MaxAutoAnswered:               policy.MaxAutoAnsweredQuestionsPerStep,
		// P3-C §20: the run's FROZEN autonomy policy, so a Settings change
		// mid-run cannot widen what an in-flight task decides for itself, and a
		// restart cannot change the answer. A run created before P3-C resolves
		// to ask_always, which is the behaviour it has always had.
		AutonomyMode: policy.EffectiveAutonomyPolicy().Mode,
		Now:          now,
		NewID:        func() string { return "wfq-" + newID() },
	})
	if err != nil {
		return err
	}

	if res.Inserted {
		// Best-effort for the same reason the sweep above is: this is the read
		// path, and a refused write must not make the run unreadable. Also
		// unconditional on a message sender, for the same reason.
		c.deliverAnsweredQuestions(ctx, run, now)
	}
	return nil
}

// questionCarriesEvidence reports whether a question row actually records
// something AO observed being asked, as opposed to a row that only records
// that a session's activity reading said "needs input".
//
// A row with no text and no structured choices is the second kind. Detect no
// longer produces them (see service/questions.Detect), but rows written before
// that fix are on disk and must not keep standing in for evidence.
func questionCarriesEvidence(q domain.WorkflowQuestion) bool {
	return strings.TrimSpace(q.QuestionText) != "" || len(q.StructuredChoices) > 0
}

// provenHumanInputRequest is the corroboration gate observeWorkStep applies
// before it will ever park a run on "the worker is waiting for you".
//
// It answers yes only when AO holds an open question for THIS step whose
// content it actually reconstructed from the pane — i.e. it saw a question
// being asked. A needs-input activity reading on its own is not that (a Codex
// PermissionRequest hook latches waiting_input for a whole working turn), and
// neither is an evidence-free question row left over from before Detect stopped
// writing them.
//
// state=resolving is deliberately excluded: a question the Decision Resolver is
// working on is AO's problem, not the user's, and must not read as a human
// stop.
func (c *Coordinator) provenHumanInputRequest(ctx stdctx.Context, runID, stepID string) (bool, error) {
	if c.questionsStore == nil {
		return false, nil
	}
	all, err := c.questionsStore.ListWorkflowQuestionsByRun(ctx, runID)
	if err != nil {
		return false, err
	}
	for _, q := range all {
		if !q.State.Open() {
			continue
		}
		if q.WorkflowStepID == nil || string(*q.WorkflowStepID) != stepID {
			continue
		}
		if !questionCarriesEvidence(q) {
			continue
		}
		return true, nil
	}
	return false, nil
}

// retireUnevidencedQuestions cancels every open question on a run that records
// no observed content, and reports how many it retired.
//
// These rows exist only because Detect used to manufacture one whenever a
// session read needs-input and the pane could not be parsed — turning "AO saw
// nothing" into a durable human_required claim. They are not evidence under the
// rule provenHumanInputRequest applies, they block dispatch through
// hasOpenQuestion, and nothing will ever answer them because there is no
// question to answer. Detect cannot write another, so this only ever touches
// history.
//
// Generic by construction: it keys off the row's own emptiness, not off any
// run, step, harness or error string. Human-answered and resolver-owned
// questions are untouched, as is any row that carries real text or choices.
func (c *Coordinator) retireUnevidencedQuestions(ctx stdctx.Context, runID string) int {
	if c.questionsStore == nil {
		return 0
	}
	all, err := c.questionsStore.ListWorkflowQuestionsByRun(ctx, runID)
	if err != nil {
		return 0
	}
	retired := 0
	for _, q := range all {
		if !q.State.Open() || questionCarriesEvidence(q) {
			continue
		}
		moved, terr := c.questionsStore.TransitionWorkflowQuestionState(ctx, string(q.ID), q.State,
			domain.QuestionStateCancelled,
			"retired: recorded no observed question text or choices, so it is not evidence that a person was asked anything",
			c.clock())
		if terr != nil {
			if c.log != nil {
				c.log.Warn("workflow: retiring an unevidenced question failed", "run", runID, "question", q.ID, "err", terr)
			}
			continue
		}
		if moved {
			retired++
		}
	}
	return retired
}

// hasOpenQuestion reports whether an unresolved question exists — open
// (pending/human_required) OR, as of Checkpoint 8K-B pass 2, still
// state=resolving (a Decision Resolver attempt in flight or awaiting
// provider capacity) — scoped to one step when stepID is non-nil, or to the
// whole run when stepID is nil (used by the master-task dispatch guard,
// which dispatches at the parent-run level, not a single step).
//
// Widened from ListOpenWorkflowQuestionsByRun (pending/human_required only)
// to ListWorkflowQuestionsByRun (all states, filtered here) rather than
// adding a second narrow SQL query for a third state value — every other
// dispatch call site already goes through this single centralized guard, so
// widening it here is enough (per this checkpoint's brief: "extend it there,
// do not touch each of the four/five individual dispatch call sites again").
func (c *Coordinator) hasOpenQuestion(ctx stdctx.Context, runID string, stepID *string) (bool, error) {
	if c.questionsStore == nil {
		return false, nil
	}
	all, err := c.questionsStore.ListWorkflowQuestionsByRun(ctx, runID)
	if err != nil {
		return false, err
	}
	for _, q := range all {
		if !q.State.Open() && q.State != domain.QuestionStateResolving {
			continue
		}
		if stepID == nil {
			return true, nil
		}
		if q.WorkflowStepID != nil && *q.WorkflowStepID == domain.WorkflowStepID(*stepID) {
			return true, nil
		}
	}
	return false, nil
}

// nextActionForOpenQuestion derives GetRun's "waiting_for_decision" prefix
// when a run has an open question, per Checkpoint 8K-A: no new
// WorkflowRunState value, just this read-time-derived NextAction string
// (cleared automatically the moment the question is answered+delivered and
// no longer appears in ListOpenWorkflowQuestionsByRun on the next call).
func nextActionForOpenQuestion(q domain.WorkflowQuestion) string {
	text := q.QuestionText
	if text == "" {
		text = q.ClassificationReason
	}
	return fmt.Sprintf("waiting_for_decision: %s — %s", q.Classification, text)
}

// deliverAnsweredQuestions sweeps this run's answered-but-undelivered questions
// and reports failures to the log rather than to the caller.
//
// Every caller is on the READ path. An answer is durable the moment it is
// recorded; delivering it is a separate, retried obligation, and a delivery
// that cannot happen right now is a fact about the worker's session rather than
// about the run's readability.
func (c *Coordinator) deliverAnsweredQuestions(ctx stdctx.Context, run domain.WorkflowRun, now time.Time) {
	if c.questionsStore == nil {
		return
	}
	// P3-C: a worker blocked on an interactive prompt is answered by SELECTING,
	// not by typing. This runs first and handles every question whose session is
	// sitting on a dialog AO can read; whatever it does not claim falls through
	// to the ordinary text path below, unchanged.
	//
	// Both halves are shared by human and autonomous answers (§7). The only
	// thing that differs between them anywhere in AO is the durable
	// answer_source -- a "safe path for people, blind path for the machine"
	// split would put the unattended answer on the mechanism nobody reviewed.
	c.deliverDialogAnswers(ctx, run, now)
	// The convergence half, for whichever path delivered.
	//
	// The structured path un-parks inline, because it holds the proof at the
	// moment it acts: it pressed the key and watched the prompt go. The text
	// path holds no such proof when it writes -- the agent has not read the
	// message yet -- so its un-park cannot happen inline and has to be a
	// separate observation on a later pass. Without one, a run whose answer was
	// delivered as text stayed parked on `worker_blocked` forever while its
	// worker finished the task and went idle: P3-D smoke B ended in exactly
	// that state, twice, with the change on disk and no review ever dispatched.
	//
	// Deferred, and above the sender check, because it is about answers that
	// are ALREADY delivered by any route: a deployment with no text path at all
	// still has to stop parking runs whose prompt was answered.
	defer c.resumeAfterDeliveredAnswers(ctx, run)
	if c.messageSender == nil {
		return
	}
	if _, err := questions.DeliverAnsweredWithState(ctx, c.questionsStore, questionMessageSender{c.messageSender},
		questionSessionInputState{c}, run.ID, now); err != nil && c.log != nil {
		c.log.Warn("workflow: delivering an answered question failed; it stays pending and the next sweep retries it",
			"run", run.ID, "err", err)
	}
}

// resumeAfterDeliveredAnswers un-parks a run whose blocking prompt has a
// delivered answer AND whose worker has visibly gone back to work.
//
// Both halves are the proof, and neither alone would be one. The delivery says
// the answer was handed over; the session's own activity says it was consumed —
// a worker still sitting in a needs-input state has not acted on anything, and
// un-parking it would be AO deciding a question was resolved because it had
// written something at a screen.
//
// Narrow in the same way clearBlockedOnAnsweredPrompt is: it clears only the
// two stops that mean "the agent is waiting on input inside its own session",
// only while the run's current stop is one of them, and only for a question
// whose own session it could read.
func (c *Coordinator) resumeAfterDeliveredAnswers(ctx stdctx.Context, run domain.WorkflowRun) {
	if c.questionsStore == nil || c.sessionFacts == nil || run.State != domain.WorkflowRunNeedsAttention {
		return
	}
	reason, _, ok := c.stopReason(ctx, run)
	if !ok || (reason != ReasonWorkerBlocked && reason != ReasonFixWorkerBlocked) {
		return
	}
	qs, err := c.questionsStore.ListWorkflowQuestionsByRun(ctx, run.ID)
	if err != nil {
		return
	}
	for _, q := range qs {
		if !q.Delivered || q.State != domain.QuestionStateAnswered || q.SessionID == nil {
			continue
		}
		sess, found, serr := c.sessionFacts.GetSession(ctx, *q.SessionID)
		if serr != nil || !found || sess.Activity.State.NeedsInput() {
			// Still waiting on something: the answer has not been consumed, or
			// a new prompt has taken its place. Either way this stop is still
			// true and AO has nothing to clear.
			continue
		}
		c.unparkRun(ctx, run, reason, fmt.Sprintf(
			"AO's answer to question %s was delivered and its worker has resumed", q.ID))
		c.resumeStepBlockedOnAnsweredPrompt(ctx, q)
		return
	}
}

// questionSessionInputState lets the text delivery path see what the structured
// one already sees: whether the target session is sitting on a prompt.
//
// It is the coordinator's own session facts, narrowed to one question, so the
// two delivery paths read the same rows and cannot disagree about whether a
// dialog is open. An unreadable session answers `known=false`, which the sweep
// treats as "carry on" rather than as "do not deliver" — the same
// cannot-tell-is-not-a-refusal rule the rest of this package obeys.
type questionSessionInputState struct{ c *Coordinator }

func (q questionSessionInputState) AwaitingInput(ctx stdctx.Context, id domain.SessionID) (bool, bool) {
	if q.c.sessionFacts == nil || id == "" {
		return false, false
	}
	rec, found, err := q.c.sessionFacts.GetSession(ctx, id)
	if err != nil || !found {
		return false, false
	}
	if !rec.Activity.State.NeedsInput() {
		return false, true
	}
	// Withholding is only correct when something else can carry the answer.
	//
	// The text path is refused here so the structured one can press the
	// selection key instead -- but if there is no structured one for this
	// session (a provider AO cannot answer structurally, a runtime that cannot
	// press keys, a deployment with no pane reader), refusing would not hand
	// the answer to a better mechanism, it would withhold it from the only
	// mechanism there is. A typed answer that may be swallowed is worse than a
	// delivered one and better than none, and this is the boundary between
	// those two cases.
	if q.c.paneReader == nil || q.c.dialogKeys == nil || !SupportsStructuredDialogResponse(rec.Harness) {
		return false, true
	}
	// And the same argument once more, one level finer: a structured path that
	// exists is not a structured path that can SEE anything.
	//
	// P3-D smoke B ran into exactly that. A real Claude select dialog rendered
	// in a layout AO's pane parser does not recognise makes the structured path
	// report "no prompt on screen" and answer nothing — and withholding the
	// text write on its behalf hands the answer to a mechanism that has already
	// declined it. So the withholding is conditioned on AO actually being able
	// to read a prompt it could then press a key into. When it cannot, the
	// answer goes back to the only route left, which is where it was before any
	// of this and which is observed to work.
	if q.c.observableDialogFor(ctx, rec).Absent() {
		return false, true
	}
	return true, true
}

// observableDialogFor reads this session's pane once, for the single question
// "does the structured path own this answer".
//
// Read-only: it presses nothing. A prompt that is PRESENT belongs to the
// structured path, and so does one AO could not read — writing text at a screen
// AO cannot interpret is exactly the blind write this whole area is about, and
// the bounded retry (P3-D §14) is what resolves it. Only a proven ABSENCE
// releases the answer to the text path, which is the shape of a genuine
// free-text prompt.
//
// A session with no parser, no pane reader or no runtime handle is reported
// absent: there is no structured path to defer to, so withholding would strand
// the answer rather than route it better.
func (c *Coordinator) observableDialogFor(
	ctx stdctx.Context, sess domain.SessionRecord,
) domain.DialogObservation {
	parser, ok := dialogParserFor(sess.Harness)
	if !ok || c.paneReader == nil || sess.Metadata.RuntimeHandleID == "" {
		return domain.NoDialog()
	}
	return c.observeDialog(ctx, parser, ports.RuntimeHandle{ID: sess.Metadata.RuntimeHandleID}, sess)
}

// deliverDialogAnswers answers every blocked prompt this run has a durable
// answer for.
//
// It marks a question delivered ONLY after re-observing that the prompt is
// gone. That is what makes the receipt honest and exactly-once safe across a
// crash (§12/§13): keys written and then a crash leaves the row undelivered,
// the next pass re-observes, finds no dialog, and records the delivery without
// pressing anything a second time.
func (c *Coordinator) deliverDialogAnswers(ctx stdctx.Context, run domain.WorkflowRun, now time.Time) {
	if c.sessionFacts == nil || c.paneReader == nil || c.dialogKeys == nil {
		return
	}
	pending, err := c.questionsStore.ListUndeliveredAnsweredWorkflowQuestions(ctx, run.ID)
	if err != nil {
		if c.log != nil {
			c.log.Warn("workflow: could not list undelivered answers", "run", run.ID, "err", err)
		}
		return
	}
	for _, q := range pending {
		if q.SessionID == nil || *q.SessionID == "" {
			continue
		}
		sess, found, serr := c.sessionFacts.GetSession(ctx, *q.SessionID)
		if serr != nil || !found {
			continue
		}
		if !SupportsStructuredDialogResponse(sess.Harness) {
			// Not a provider AO can answer structurally. Leave it for the text
			// path, which will either deliver it (a genuine composer) or be
			// refused by sessionguard (a dialog) -- and the refusal is the
			// truthful outcome, not a reason to type into a prompt.
			continue
		}
		outcome := c.deliverDialogResponse(ctx, sess, dialogResponseFor(q, sess))
		switch {
		case outcome.Delivered:
			if _, merr := c.questionsStore.MarkWorkflowQuestionDelivered(ctx, string(q.ID), now); merr != nil && c.log != nil {
				c.log.Warn("workflow: a dialog answer was delivered but could not be recorded; the next pass re-observes and records it",
					"run", run.ID, "question", q.ID, "err", merr)
			}
			if c.log != nil {
				c.log.Info("workflow: answered an agent's prompt by selection",
					"run", run.ID, "question", q.ID, "session", sess.ID,
					"option", outcome.SelectedOptionID, "detail", outcome.Detail)
			}
			run = c.clearBlockedOnAnsweredPrompt(ctx, run, q, outcome)
		case outcome.Refusal == domain.RefusalDialogGone && sess.Activity.State.NeedsInput():
			// The prompt is not there AND the session is still waiting on
			// something. That is the redelivery-after-a-crash shape: the keys
			// landed, the agent consumed them, and the receipt is the absence
			// of the question.
			//
			// The liveness half is not decoration. An absent dialog equally
			// describes a session that DIED holding the question, and recording
			// that as delivered would file an answer nobody ever received --
			// the same lie as marking delivery on the strength of having
			// written keys. Found by the P3-C closing smoke, where a worker
			// exited before its answer arrived and the run nonetheless reported
			// the decision delivered.
			//
			// P3-D smoke B found the OTHER side of this reading, and did not
			// resolve it: RefusalDialogGone does not actually mean "the prompt
			// went away", it means AO's parser recognised nothing on the pane.
			// A real Claude select dialog AO could not parse therefore read as
			// an absence, and a correct autonomous decision was recorded
			// delivered without ever reaching the worker. Narrowing this branch
			// on the session's own `blocked` reading fixes that case and breaks
			// the crash-redelivery contract this branch exists for (it is
			// exercised from `blocked` throughout), so the honest separation is
			// between "the parser saw nothing" and "there is nothing to see" --
			// a distinction the parser does not currently report. Left as a
			// named limitation rather than swapped for a different wrong answer.
			if _, merr := c.questionsStore.MarkWorkflowQuestionDelivered(ctx, string(q.ID), now); merr != nil && c.log != nil {
				c.log.Warn("workflow: could not record a delivery whose prompt is already gone",
					"run", run.ID, "question", q.ID, "err", merr)
			}
			// And un-park, exactly as the branch above does (P3-D smoke B).
			//
			// The two branches record the SAME durable fact -- this question's
			// answer has been handed over -- so a run left parked on
			// worker_blocked by one of them is parked on a stop that is no
			// longer true. The smoke made that concrete: the worker took the
			// autonomous decision, implemented it, ran both verifiers and
			// reported its turn finished, while the run still read "AO stopped
			// and needs a decision" and no review was ever dispatched. That is
			// the stranding clearBlockedOnAnsweredPrompt was written to
			// prevent, reached by the other route.
			//
			// It is not a weaker claim than the branch above, either. Both
			// stand on the same observation -- the prompt AO recorded this stop
			// for is no longer on the screen -- and this one additionally
			// required the session to be alive before it would record anything
			// at all. What differs is only whether AO pressed the key itself.
			run = c.clearBlockedOnAnsweredPrompt(ctx, run, q, outcome)
		case outcome.Refusal == domain.RefusalDialogUnreadable:
			// AO holds the answer and cannot read the screen to deliver it.
			// Nothing is marked, nothing is pressed, and the run is not parked
			// yet: a half-drawn repaint resolves itself within an observation
			// or two, and stopping on the first one would turn every redraw
			// into an incident. See noteDialogUnreadable for the bound.
			run = c.noteDialogUnreadable(ctx, run, q, outcome, now)
		default:
			if c.log != nil {
				c.log.Warn("workflow: an agent's prompt was not answered; the answer stays pending",
					"run", run.ID, "question", q.ID, "refusal", string(outcome.Refusal), "detail", outcome.Detail)
			}
		}
	}
}

// dialogUnreadablePhase is the durable marker for "AO first failed to read
// this question's prompt". One row per question, ever — it is a deadline, not a
// log, and writing one per poll would put a heartbeat on the ledger.
const dialogUnreadablePhase = "provider_dialog_unreadable_observed"

// dialogUnreadableWindow is how long AO keeps re-observing an unreadable prompt
// before it says so out loud.
//
// It is a bound on RETRIES, not a sleep: the observation happens on whatever
// pass comes next (the heartbeat, a Continue, the wake this schedules), and the
// window only decides when to stop hoping. Generous, because the thing it is
// waiting out is a terminal redraw and the cost of waiting is a run that keeps
// working while the cost of giving up early is a person summoned to answer a
// question AO had already answered.
const dialogUnreadableWindow = 3 * time.Minute

// dialogUnreadableRecord is the marker's payload: which question, and when AO
// first could not read its prompt.
type dialogUnreadableRecord struct {
	QuestionID string    `json:"questionId"`
	FirstSeen  time.Time `json:"firstSeen"`
	Detail     string    `json:"detail"`
}

// noteDialogUnreadable records the first unreadable observation for a question
// and, once the retry window has passed with no better one, parks the run on
// its own reason.
//
// The reason is its own, and that is the point of the whole exercise (P3-D §14):
// `dialog_gone` would be a claim about the agent's screen that AO has not
// earned, and `worker_blocked` would send a person to decide something AO
// already decided. This one says what is actually true — AO cannot read this
// provider's prompt — which is the only sentence that leads anyone to the fault.
func (c *Coordinator) noteDialogUnreadable(
	ctx stdctx.Context, run domain.WorkflowRun, q domain.WorkflowQuestion,
	outcome DialogDeliveryOutcome, now time.Time,
) domain.WorkflowRun {
	first, found := c.firstDialogUnreadableAt(ctx, run.ID, string(q.ID))
	if !found {
		stepID := stepIDStringOf(q)
		rec := dialogUnreadableRecord{QuestionID: string(q.ID), FirstSeen: now, Detail: outcome.Detail}
		payload, err := json.Marshal(rec)
		if err != nil {
			payload = []byte("{}")
		}
		if _, cerr := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
			ID:             "wfc-" + c.newID(),
			WorkflowRunID:  run.ID,
			WorkflowStepID: stepID,
			ProjectID:      run.ProjectID,
			NextAction:     "AO could not read the agent's prompt; it will re-observe before concluding anything",
			DurablePhase:   dialogUnreadablePhase,
			PayloadVersion: "v1",
			RetryState:     string(payload),
			CreatedAt:      now,
		}); cerr != nil && c.log != nil {
			c.log.Warn("workflow: could not record an unreadable prompt observation",
				"run", run.ID, "question", q.ID, "err", cerr)
		}
		// The retry itself rides on the existing durable wake, so a run nobody
		// is polling still comes back to look again.
		c.scheduleWake(ctx, run, nil, wake.ReasonTransientRetry, "")
		if c.log != nil {
			c.log.Warn("workflow: AO could not read an agent's prompt; the answer stays pending and AO will look again",
				"run", run.ID, "question", q.ID, "detail", outcome.Detail)
		}
		return run
	}
	if now.Sub(first) < dialogUnreadableWindow {
		c.scheduleWake(ctx, run, nil, wake.ReasonTransientRetry, "")
		return run
	}
	if run.State.Terminal() {
		return run
	}
	// A run already parked on worker_blocked is parked for THIS prompt, and
	// that reason is now the less accurate of the two: it says a person has to
	// decide something, when the decision exists and only AO's reading of the
	// screen is missing. Recording the specific stop over it is a correction,
	// not a second incident. Any OTHER stop is about something else and is left
	// exactly as it is.
	if run.State == domain.WorkflowRunNeedsAttention {
		reason, _, ok := c.stopReason(ctx, run)
		if !ok || (reason != ReasonWorkerBlocked && reason != ReasonFixWorkerBlocked) {
			return run
		}
	}
	detail := fmt.Sprintf(
		"AO decided question %s automatically and could not read the prompt to deliver it (%s)",
		q.ID, orValue(outcome.Detail, "the agent's screen could not be interpreted"))
	c.recordAttentionStop(ctx, run, stepIDStringOf(q), ReasonProviderDialogUnreadable, detail)
	if run.State != domain.WorkflowRunNeedsAttention {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State,
			domain.WorkflowRunNeedsAttention, now); err != nil {
			if c.log != nil {
				c.log.Warn("workflow: could not park a run on an unreadable prompt", "run", run.ID, "err", err)
			}
			return run
		}
	}
	if refreshed, ok, err := c.store.GetWorkflowRun(ctx, run.ID); err == nil && ok {
		return refreshed
	}
	return run
}

// firstDialogUnreadableAt reads back when AO first failed to read this
// question's prompt, if it ever recorded that.
func (c *Coordinator) firstDialogUnreadableAt(ctx stdctx.Context, runID, questionID string) (time.Time, bool) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		// Unreadable ledger: claim no marker exists rather than a stale one, so
		// the window restarts instead of expiring on evidence AO cannot see.
		return time.Time{}, false
	}
	for _, cp := range cps {
		if cp.DurablePhase != dialogUnreadablePhase {
			continue
		}
		var rec dialogUnreadableRecord
		if json.Unmarshal([]byte(cp.RetryState), &rec) == nil && rec.QuestionID == questionID {
			return rec.FirstSeen, true
		}
	}
	return time.Time{}, false
}

// stepIDStringOf renders a question's step id for the checkpoint writer.
func stepIDStringOf(q domain.WorkflowQuestion) *string {
	if q.WorkflowStepID == nil || *q.WorkflowStepID == "" {
		return nil
	}
	v := string(*q.WorkflowStepID)
	return &v
}

// clearBlockedOnAnsweredPrompt un-parks a run whose ONLY reason for stopping
// was the prompt AO has just answered.
//
// worker_blocked is a human-owned stop, and clearResolvedStop refuses to clear
// one -- correctly, because AO must not guess that somebody dealt with
// something. Here AO is not guessing: it answered that exact prompt, re-read the
// screen, and observed the question gone. That is the proof the stop was
// recorded for, and leaving the run parked on it afterwards would strand a
// worker that is already back at work -- which the P3-C closing smoke observed
// doing precisely that, with the file the worker went on to write sitting in
// the repository while the run still read "needs a decision".
//
// Deliberately narrow. It clears ONLY the two reasons that mean "the agent is
// waiting on input inside its own session", and only when the run's current stop
// is one of them: any other stop is about something AO did not just resolve.
func (c *Coordinator) clearBlockedOnAnsweredPrompt(
	ctx stdctx.Context, run domain.WorkflowRun, q domain.WorkflowQuestion, outcome DialogDeliveryOutcome,
) domain.WorkflowRun {
	if run.State != domain.WorkflowRunNeedsAttention {
		return run
	}
	reason, _, ok := c.stopReason(ctx, run)
	if !ok || (reason != ReasonWorkerBlocked && reason != ReasonFixWorkerBlocked) {
		return run
	}
	run = c.unparkRun(ctx, run, reason, fmt.Sprintf(
		"AO answered the prompt this run was blocked on (question %s, selected %q) and re-read the agent's screen to confirm it was gone",
		q.ID, outcome.SelectedOptionLabel))
	// And the STEP, not only the run (P3-D smoke B).
	//
	// blockedOnHumanDecision parks both: the run stops, and the step it was
	// working on leaves `running` for `waiting`. Un-parking only the run
	// therefore fixes what a person reads and not what AO does — observation
	// runs exclusively over a work step in `running`, so a step left at
	// `waiting` is one whose worker is never looked at again. The smoke ended
	// exactly there: the answer delivered, the run resumed, the worker's change
	// on disk, and the work step sitting at `waiting` where neither the
	// heartbeat nor an explicit Continue could reach it.
	//
	// The proof licensing it is the same one that licensed the un-park, plus
	// one more: the step still durably owns the session the question came from,
	// so what is being returned to `running` is the execution AO answered, not
	// some later one.
	c.resumeStepBlockedOnAnsweredPrompt(ctx, q)
	return run
}

// resumeStepBlockedOnAnsweredPrompt returns the question's own step to
// `running` so work observation can see the worker again.
//
// Best-effort and heavily fenced. It moves nothing unless the step is the one
// the question was asked from, is parked at `waiting`, and still holds the
// question's session; a step that has moved on since is a step this answer has
// no business touching. A failed transition is a benign race (something else
// moved the step first), not an error worth failing a read path over.
func (c *Coordinator) resumeStepBlockedOnAnsweredPrompt(ctx stdctx.Context, q domain.WorkflowQuestion) {
	if q.WorkflowStepID == nil || *q.WorkflowStepID == "" || q.SessionID == nil {
		return
	}
	steps, err := c.store.ListWorkflowSteps(ctx, string(q.WorkflowRunID))
	if err != nil {
		return
	}
	for _, step := range steps {
		if step.ID != string(*q.WorkflowStepID) {
			continue
		}
		if step.State != domain.WorkflowStepWaiting {
			return
		}
		if step.SessionID == nil || domain.SessionID(*step.SessionID) != *q.SessionID {
			return
		}
		if _, serr := c.store.UpdateWorkflowStepState(ctx, step.ID,
			domain.WorkflowStepWaiting, domain.WorkflowStepRunning, c.clock()); serr != nil && c.log != nil {
			c.log.Info("workflow: could not return an answered step to running (benign race)",
				"run", string(q.WorkflowRunID), "step", step.ID, "err", serr)
		}
		return
	}
}
