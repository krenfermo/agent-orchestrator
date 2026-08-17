package domain

import "time"

// WorkflowQuestionResolutionID identifies one resolution attempt for a
// workflow_question classified auto_resolvable (Checkpoint 8K-B). Distinct
// from WorkflowQuestionID: a question may accumulate more than one
// resolution attempt (provider unavailable, resolver crash, staleness
// sweep) before it resolves, so the attempt gets its own identity.
type WorkflowQuestionResolutionID string

// ResolutionStatus is the durable lifecycle state of one Decision Resolver
// attempt, mirroring review_run's status shape (0012_add_review_tables.sql).
// Distinct from QuestionState: ResolutionStatus tracks one attempt; the
// question's resolving_run_id points at whichever attempt is (or was most
// recently) current.
type ResolutionStatus string

const (
	// ResolutionStatusPending means the resolution row was created but the
	// resolver session has not yet been launched (pass 2's dispatch step).
	ResolutionStatusPending ResolutionStatus = "pending"
	// ResolutionStatusRunning means a read-only resolver session is in
	// flight for this attempt. At most one row per question may hold this
	// status at a time (enforced by the partial unique index on
	// workflow_question_resolutions in 0105, or by the store's CAS
	// transition if that index is unavailable — see the store package doc
	// comment).
	ResolutionStatusRunning ResolutionStatus = "running"
	// ResolutionStatusComplete means the resolver produced an answer
	// (possibly requires_human=true if it could not determine one
	// confidently).
	ResolutionStatusComplete ResolutionStatus = "complete"
	// ResolutionStatusFailed means the resolver session errored or could
	// not be launched at all (e.g. no available opposite-provider harness).
	ResolutionStatusFailed ResolutionStatus = "failed"
	// ResolutionStatusCancelled means the owning run or question was
	// cancelled while this attempt was still pending/running.
	ResolutionStatusCancelled ResolutionStatus = "cancelled"
)

// Valid reports whether a resolution status value is persistable.
func (s ResolutionStatus) Valid() bool {
	switch s {
	case ResolutionStatusPending, ResolutionStatusRunning, ResolutionStatusComplete,
		ResolutionStatusFailed, ResolutionStatusCancelled:
		return true
	default:
		return false
	}
}

// Terminal reports whether the status is a final state that will never
// transition again (used by the CAS-transition store method and, in pass 2,
// the staleness sweep).
func (s ResolutionStatus) Terminal() bool {
	return s == ResolutionStatusComplete || s == ResolutionStatusFailed || s == ResolutionStatusCancelled
}

// WorkflowQuestionResolution is one durable Decision Resolver attempt
// (Checkpoint 8K-B): a read-only opposite-provider session spawned to answer
// a question the classifier judged auto_resolvable. Never stores the
// resolver session's full transcript — only the final answer, a bounded
// evidence-reference list, and the certainty/requires_human verdict.
type WorkflowQuestionResolution struct {
	ID                 WorkflowQuestionResolutionID
	WorkflowQuestionID WorkflowQuestionID
	WorkflowRunID      WorkflowRunID
	AskingSessionID    *SessionID
	ResolverHarness    AgentHarness
	ResolverSessionID  *SessionID
	Status             ResolutionStatus
	Answer             string
	ReasonSummary      string
	EvidenceReferences []string
	Certainty          *QuestionCertainty
	RequiresHuman      bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
	CompletedAt        *time.Time
}
