package workflow

import (
	stdctx "context"
	"fmt"
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
func (c *Coordinator) reconcileQuestions(ctx stdctx.Context, run domain.WorkflowRun, steps []domain.WorkflowStep) error {
	if c.questionsStore == nil {
		return nil
	}
	now := c.clock()

	if c.messageSender != nil {
		if _, err := questions.DeliverAnswered(ctx, c.questionsStore, questionMessageSender{c.messageSender}, run.ID, now); err != nil {
			return err
		}
	}

	if run.State.Terminal() || c.sessionFacts == nil || c.paneReader == nil {
		return nil
	}

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
			return err
		}
	}
	return nil
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

	open, err := c.questionsStore.ListOpenWorkflowQuestionsByRun(ctx, run.ID)
	if err != nil {
		return err
	}
	for _, q := range open {
		if q.WorkflowStepID != nil && string(*q.WorkflowStepID) == step.ID {
			// Already has an open question for this step: don't re-scrape.
			return nil
		}
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

	var branch, worktreePath string
	if cp, hasCP, cerr := c.store.GetLatestWorkflowCheckpointByStep(ctx, step.ID); cerr == nil && hasCP {
		branch, worktreePath = cp.Branch, cp.WorktreePath
	}

	policy := policyForRun(run)
	stepID := domain.WorkflowStepID(step.ID)
	newID := c.newID

	res, err := questions.Detect(ctx, c.questionsStore, parser, questions.DetectInput{
		RunID:                  domain.WorkflowRunID(run.ID),
		StepID:                 &stepID,
		SessionID:              &sessionID,
		AskingHarness:          harness,
		AskingRole:             string(step.Kind),
		PaneText:               paneText,
		CaptureProvider:        "tmux",
		PolicyVersionAtCapture: run.PolicyVersion,
		Branch:                 branch,
		WorktreePath:           worktreePath,
		MaxAutoAnswered:        policy.MaxAutoAnsweredQuestionsPerStep,
		Now:                    now,
		NewID:                  func() string { return "wfq-" + newID() },
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

// hasOpenQuestion reports whether an unresolved (pending/human_required)
// question exists — scoped to one step when stepID is non-nil, or to the
// whole run when stepID is nil (used by the master-task dispatch guard,
// which dispatches at the parent-run level, not a single step).
func (c *Coordinator) hasOpenQuestion(ctx stdctx.Context, runID string, stepID *string) (bool, error) {
	if c.questionsStore == nil {
		return false, nil
	}
	open, err := c.questionsStore.ListOpenWorkflowQuestionsByRun(ctx, runID)
	if err != nil {
		return false, err
	}
	if stepID == nil {
		return len(open) > 0, nil
	}
	for _, q := range open {
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
