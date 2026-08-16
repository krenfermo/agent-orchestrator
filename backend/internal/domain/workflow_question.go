package domain

import "time"

// WorkflowQuestionID identifies one durable captured question a worker
// harness asked while paused on the user (Checkpoint 8K-A).
type WorkflowQuestionID string

// QuestionCertainty records how confidently the captured question_text
// reflects what the harness actually asked. A conservative pane-scrape
// fallback: no confident parse ever invents text, it instead persists
// certainty=unknown with an empty question_text.
type QuestionCertainty string

const (
	// QuestionCertaintyActual means the question text was extracted from a
	// harness-native structured signal (not yet produced by any 8K-A
	// detector, but reserved so a future capture path doesn't need a schema
	// change).
	QuestionCertaintyActual QuestionCertainty = "actual"
	// QuestionCertaintyInferred means the question text was reconstructed
	// from a bounded pane-text scrape matching a known interactive-prompt
	// marker for the asking harness.
	QuestionCertaintyInferred QuestionCertainty = "inferred"
	// QuestionCertaintyUnknown means the pane text could not be parsed
	// confidently; question_text is empty and classification is always
	// human_required.
	QuestionCertaintyUnknown QuestionCertainty = "unknown"
)

// Valid reports whether a certainty value is persistable.
func (c QuestionCertainty) Valid() bool {
	switch c {
	case QuestionCertaintyActual, QuestionCertaintyInferred, QuestionCertaintyUnknown:
		return true
	default:
		return false
	}
}

// QuestionClassification is the deterministic classifier's verdict for a
// captured question. Distinct from QuestionState: classification records
// what the classifier decided the question *is*; state records where it
// currently sits in the resolution lifecycle (an ambiguous classification
// still forces a human_required state in 8K-A, since there is no
// second-LLM resolver yet to escalate from).
type QuestionClassification string

const (
	// QuestionClassificationPolicyResolvable means the answer is fully
	// determined by data AO already persists (branch/worktree/policy
	// fields) — no LLM call needed.
	QuestionClassificationPolicyResolvable QuestionClassification = "policy_resolvable"
	// QuestionClassificationAutoResolvable is reserved for a future
	// checkpoint's second-LLM Decision Resolver. 8K-A's classifier never
	// emits this value; it is included in the persisted enum now so a
	// later checkpoint widening auto-resolution doesn't need another
	// migration.
	QuestionClassificationAutoResolvable QuestionClassification = "auto_resolvable"
	// QuestionClassificationHumanRequired means the question is sensitive
	// or otherwise requires a human decision (or the text/certainty was not
	// reliably captured at all).
	QuestionClassificationHumanRequired QuestionClassification = "human_required"
	// QuestionClassificationAmbiguous means the classifier found no
	// confident match either way. Kept distinct from human_required for
	// observability even though its *state* always resolves to
	// human_required in 8K-A.
	QuestionClassificationAmbiguous QuestionClassification = "ambiguous"
)

// Valid reports whether a classification value is persistable.
func (c QuestionClassification) Valid() bool {
	switch c {
	case QuestionClassificationPolicyResolvable, QuestionClassificationAutoResolvable,
		QuestionClassificationHumanRequired, QuestionClassificationAmbiguous:
		return true
	default:
		return false
	}
}

// QuestionState is the durable resolution-lifecycle state of a captured
// question, distinct from QuestionClassification (see its doc comment).
type QuestionState string

const (
	// QuestionStatePending means the question was captured and classified
	// policy_resolvable but the policy resolver has not yet computed an
	// answer (or is about to be attempted).
	QuestionStatePending QuestionState = "pending"
	// QuestionStateResolving means a resolution attempt is in flight.
	// Reserved for resolvers that need more than one synchronous step;
	// 8K-A's policy resolver is synchronous and does not currently produce
	// this state, but it is a valid persisted value for future resolvers.
	QuestionStateResolving QuestionState = "resolving"
	// QuestionStateAnswered means an answer (policy or human) has been
	// computed and persisted, but may not yet be delivered to the session
	// (see delivered/delivered_at).
	QuestionStateAnswered QuestionState = "answered"
	// QuestionStateHumanRequired means the question needs a human answer
	// via the answer API before the run can proceed.
	QuestionStateHumanRequired QuestionState = "human_required"
	// QuestionStateCancelled means the owning run was cancelled while the
	// question was still open; no resolver or delivery attempt follows.
	QuestionStateCancelled QuestionState = "cancelled"
)

// Valid reports whether a state value is persistable.
func (s QuestionState) Valid() bool {
	switch s {
	case QuestionStatePending, QuestionStateResolving, QuestionStateAnswered,
		QuestionStateHumanRequired, QuestionStateCancelled:
		return true
	default:
		return false
	}
}

// Open reports whether the question is still awaiting resolution (used by
// the dispatch-guard dedup check and the run-cancel bulk update).
func (s QuestionState) Open() bool {
	return s == QuestionStatePending || s == QuestionStateHumanRequired
}

// AnswerSource records who/what produced the answer_text for a question.
type AnswerSource string

const (
	// AnswerSourcePolicy means the answer was computed deterministically by
	// the policy resolver from already-stored facts.
	AnswerSourcePolicy AnswerSource = "policy"
	// AnswerSourceHuman means the answer was submitted by a human through
	// the answer API.
	AnswerSourceHuman AnswerSource = "human"
)

// Valid reports whether an answer source value is persistable.
func (s AnswerSource) Valid() bool {
	switch s {
	case AnswerSourcePolicy, AnswerSourceHuman:
		return true
	default:
		return false
	}
}

// QuestionChoice is one structured option a harness offered alongside its
// question (e.g. a numbered select-list entry). Never carries free-form
// transcript text beyond the option label itself.
type QuestionChoice struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// WorkflowQuestion is one durable, classified fact: a worker harness paused
// on a question, captured conservatively from a bounded pane-text window
// (Checkpoint 8K-A). Never stores the full transcript or chain-of-thought —
// only the reconstructed question text (possibly empty, see
// QuestionCertaintyUnknown) and bounded capture-evidence metadata.
type WorkflowQuestion struct {
	ID                   WorkflowQuestionID
	WorkflowRunID        WorkflowRunID
	WorkflowStepID       *WorkflowStepID
	WorkflowAttemptID    *string
	SessionID            *SessionID
	AskingHarness        AgentHarness
	AskingRole           string
	Fingerprint          string
	QuestionText         string
	StructuredChoices    []QuestionChoice
	CaptureProvider      string
	CaptureParserVersion string
	CaptureRangeLines    int
	Certainty            QuestionCertainty
	Classification       QuestionClassification
	ClassificationReason string
	State                QuestionState
	CreatedAt            time.Time
	AnsweredAt           *time.Time
	AnswerSource         *AnswerSource
	AnswerText           string
	AnswerReference      string
	Delivered            bool
	DeliveredAt          *time.Time
}
