package workflow

import (
	stdctx "context"
	"fmt"
	"strings"
	"time"

	claudecodeq "github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/claudecode"
	codexq "github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/codex"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/questions"
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

	if c.messageSender != nil {
		if _, err := questions.DeliverAnswered(ctx, c.questionsStore, questionMessageSender{c.messageSender}, run.ID, now); err != nil {
			return "", err
		}
	}

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
		Now:                           now,
		NewID:                         func() string { return "wfq-" + newID() },
	})
	if err != nil {
		return err
	}

	if res.Inserted && c.messageSender != nil {
		if _, derr := questions.DeliverAnswered(ctx, c.questionsStore, questionMessageSender{c.messageSender}, run.ID, now); derr != nil {
			return derr
		}
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
