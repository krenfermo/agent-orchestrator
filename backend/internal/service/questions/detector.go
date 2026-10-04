package questions

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// CaptureParserVersion is bumped whenever a QuestionPaneParser's marker
// heuristics change in a way that would affect stored evidence metadata.
const CaptureParserVersion = "8k-a.v1"

// PaneCaptureRangeLines is the bounded recent-window size passed to
// ports.Runtime.GetOutput by callers before invoking Detect. Recorded as
// evidence metadata (capture_range_lines), never used to store the pane
// text itself.
const PaneCaptureRangeLines = 120

// Store is the narrow persistence contract Detect needs. Satisfied by
// *store.Store (backend/internal/storage/sqlite/store), which implements
// each of these against the workflow_questions table.
type Store interface {
	InsertWorkflowQuestion(ctx context.Context, q domain.WorkflowQuestion) (domain.WorkflowQuestion, bool, error)
	ListOpenWorkflowQuestionsByRun(ctx context.Context, runID string) ([]domain.WorkflowQuestion, error)
	CountPolicyAnsweredWorkflowQuestionsByStep(ctx context.Context, stepID string) (int64, error)
	AnswerWorkflowQuestion(ctx context.Context, id string, expectedState, newState domain.QuestionState, source domain.AnswerSource, answerText, answerReference string, answeredAt time.Time) (bool, error)
}

// DetectInput bundles the already-resolved facts Detect needs: everything
// here is either already captured by the caller (bounded pane text) or
// already durably stored elsewhere (run/step/policy identity, checkpoint
// branch/worktree). Detect performs no I/O of its own beyond the Store and
// parser calls it is explicitly given.
type DetectInput struct {
	RunID                         domain.WorkflowRunID
	StepID                        *domain.WorkflowStepID
	AttemptID                     *string
	SessionID                     *domain.SessionID
	AskingHarness                 domain.AgentHarness
	AskingRole                    string
	PaneText                      string
	CaptureProvider               string
	PolicyVersionAtCapture        string
	WorkspaceFingerprintAtCapture string
	Branch                        string
	WorktreePath                  string
	MaxAutoAnswered               int
	// AutonomyMode is P3-C's frozen question-autonomy policy for the run. The
	// zero value is not a mode, and EvaluateAutonomy normalizes it to
	// ask_always -- so a caller that does not pass one gets exactly the
	// pre-P3-C behaviour rather than an accidental widening.
	AutonomyMode domain.QuestionAutonomyMode
	Now          time.Time
	NewID        func() string
}

// DetectResult is what Detect produced.
type DetectResult struct {
	Question domain.WorkflowQuestion
	// Inserted is false when the fingerprint already existed (idempotent
	// dedup no-op): the returned Question is the pre-existing row, and it
	// was NOT reclassified or re-persisted.
	Inserted bool
	// Unparsed is true when the session reported a needs-input state but the
	// pane carried no question this harness's parser could reconstruct. It is
	// the honest "AO does not know what, if anything, is being asked" answer,
	// and NOTHING is persisted for it.
	//
	// This used to persist a question with empty text, which Classify then
	// (correctly, for what it was given) called human_required — turning the
	// ABSENCE of evidence into a durable claim that a person was needed. In
	// incident wf-57f90ff2 that fabricated row was the only "proof" behind a
	// worker_blocked stop on a Codex worker that was, at that moment, running
	// tests and that finished its turn unattended two minutes later.
	Unparsed bool
}

// Detect is Checkpoint 8K-A's single synchronous orchestration step:
// capture (already done by the caller into in.PaneText) -> parse -> print
// -> classify -> persist, as one store transaction (the row is inserted
// with classification already computed). A crash before InsertWorkflowQuestion
// commits just means the next poll re-detects from scratch, safely deduped
// by fingerprint on retry.
func Detect(ctx context.Context, store Store, parser ports.QuestionPaneParser, in DetectInput) (DetectResult, error) {
	var (
		questionText string
		choices      []domain.QuestionChoice
		certainty    domain.QuestionCertainty
	)

	candidate, ok := ports.QuestionCandidate{}, false
	if parser != nil {
		candidate, ok = parser.ParseQuestion(in.PaneText)
	}
	if !ok {
		// No question could be reconstructed from the pane. Persist NOTHING.
		//
		// The old behaviour here was to insert a row with empty text, which
		// Classify then labelled human_required ("question text could not be
		// reconstructed reliably") and ResolveState parked in state
		// human_required. That reads as a conservative choice and is the
		// opposite: a needs-input activity reading is not proof a question was
		// asked (a Codex PermissionRequest hook, for one, latches waiting_input
		// for a whole working turn — see the codex adapter), so an unparseable
		// pane means AO has no evidence at all, and manufacturing a
		// human_required row out of it converts "we did not see anything" into
		// "a person must act". Callers get Unparsed and decide; a genuinely
		// stuck worker is still caught, because a real prompt is precisely what
		// the parsers DO reconstruct.
		return DetectResult{Unparsed: true}, nil
	}
	questionText = candidate.QuestionText
	choices = candidate.StructuredChoices
	certainty = candidate.Certainty

	stepIDStr := ""
	if in.StepID != nil {
		stepIDStr = string(*in.StepID)
	}
	fingerprint := Fingerprint(in.RunID, stepIDStr, questionText, choices, in.PolicyVersionAtCapture, in.WorkspaceFingerprintAtCapture)

	// P3-C §20: the run's frozen autonomy policy is consulted for AMBIGUOUS
	// questions only, and strictly after every refusal Classify already makes.
	// Under ask_always this is Classify unchanged.
	// The autonomy decision's own sentence travels inside `reason`, which is
	// persisted as workflow_questions.classification_reason -- so an
	// auto-decided question carries the policy and the justification it was
	// taken under, and an escalated one carries why AO refused to take it.
	//
	// The MODE travels separately, in its own column, because the answer this
	// question eventually gets has to be durably distinguishable from a human's
	// and from an ordinary discovery-shape resolver answer. Deciding that from
	// prose would make the distinction depend on wording; deciding it from the
	// column makes it a fact. See domain.AnswerSourceAutonomous.
	classification, reason, autonomy := ClassifyUnderAutonomy(questionText, certainty, in.AutonomyMode)
	autonomyMode := domain.QuestionAutonomyMode("")
	if autonomy.AutoDecidable {
		autonomyMode = autonomy.Mode
	}

	budgetCtx := ClassifyContext{
		Branch:          in.Branch,
		WorktreePath:    in.WorktreePath,
		MaxAutoAnswered: in.MaxAutoAnswered,
	}
	// Checkpoint 8K-B pass 2 budget-sharing decision: auto_resolvable now
	// also computes PolicyAnsweredCount (previously only policy_resolvable
	// did — a gap that left the auto_resolvable branch of ResolveState's
	// budget check permanently 0-vs-0, i.e. never enforced). The intended
	// design (per ClassifyContext.PolicyAnsweredCount's doc comment, and the
	// recommendation left there in pass 1) is a single shared count across
	// answer_source IN ('policy','resolver'); the backing query
	// (queries/workflow_questions.sql's CountPolicyAnsweredWorkflowQuestionsByStep)
	// still filters strictly answer_source='policy' because widening its
	// WHERE clause reproducibly triggered a sqlc v1.31.1 SQL-codegen
	// corruption bug specific to this query file (see the store-level note
	// on Store.TransitionWorkflowQuestionState for the same bug hit
	// elsewhere in this file). Until that is worked around, both
	// classifications draw against this policy-only count as a
	// conservative stand-in — strictly safer than the prior "auto_resolvable
	// budget never enforced" gap, though not yet a true shared count with
	// resolver-answered questions. Flagged for pass 3.
	if (classification == domain.QuestionClassificationPolicyResolvable || classification == domain.QuestionClassificationAutoResolvable) && stepIDStr != "" {
		count, err := store.CountPolicyAnsweredWorkflowQuestionsByStep(ctx, stepIDStr)
		if err != nil {
			return DetectResult{}, err
		}
		budgetCtx.PolicyAnsweredCount = int(count)
	}
	state := ResolveState(classification, budgetCtx)

	newID := string(in.RunID) + ":question:" + fingerprint[:12]
	if in.NewID != nil {
		newID = in.NewID()
	}

	row := domain.WorkflowQuestion{
		ID:                   domain.WorkflowQuestionID(newID),
		WorkflowRunID:        in.RunID,
		WorkflowStepID:       in.StepID,
		WorkflowAttemptID:    in.AttemptID,
		SessionID:            in.SessionID,
		AskingHarness:        in.AskingHarness,
		AskingRole:           in.AskingRole,
		Fingerprint:          fingerprint,
		QuestionText:         questionText,
		StructuredChoices:    choices,
		CaptureProvider:      in.CaptureProvider,
		CaptureParserVersion: CaptureParserVersion,
		CaptureRangeLines:    PaneCaptureRangeLines,
		Certainty:            certainty,
		Classification:       classification,
		ClassificationReason: reason,
		State:                state,
		CreatedAt:            in.Now,
		AutonomyMode:         autonomyMode,
	}

	saved, inserted, err := store.InsertWorkflowQuestion(ctx, row)
	if err != nil {
		return DetectResult{}, err
	}
	if !inserted {
		// Duplicate fingerprint: never reclassified, never re-persisted.
		return DetectResult{Question: saved, Inserted: false}, nil
	}

	// Policy resolution happens inline, synchronously, right after a fresh
	// policy_resolvable insert lands in "pending" — no second-LLM call, no
	// network call, purely a deterministic read of already-stored facts.
	if saved.State == domain.QuestionStatePending && saved.Classification == domain.QuestionClassificationPolicyResolvable {
		answerText, ok := ResolvePolicyAnswer(saved.QuestionText, budgetCtx)
		if ok {
			answeredOK, aerr := store.AnswerWorkflowQuestion(ctx, string(saved.ID), domain.QuestionStatePending, domain.QuestionStateAnswered, domain.AnswerSourcePolicy, answerText, "", in.Now)
			if aerr != nil {
				return DetectResult{}, aerr
			}
			if answeredOK {
				saved.State = domain.QuestionStateAnswered
				policySrc := domain.AnswerSourcePolicy
				saved.AnswerSource = &policySrc
				saved.AnswerText = answerText
			}
		}
	}

	return DetectResult{Question: saved, Inserted: true}, nil
}
