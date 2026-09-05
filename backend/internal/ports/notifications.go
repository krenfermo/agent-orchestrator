package ports

import (
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// NotificationIntent is the lifecycle-to-notification-producer contract. It is
// not an HTTP DTO; lifecycle fills it from facts it already has after the
// underlying session/PR state write succeeds.
type NotificationIntent struct {
	Type      domain.NotificationType
	SessionID domain.SessionID
	ProjectID domain.ProjectID
	PRURL     string
	CreatedAt time.Time

	// WorkflowRunID anchors a run-level intent (workflow_completed) that has no
	// session behind it.
	WorkflowRunID string
	// DedupeKey names the one real-world event this intent reports. Required for
	// the completion family, which must never announce the same finished work
	// twice; see domain.NotificationRecord.DedupeKey.
	DedupeKey string

	// Enrichment hints. These avoid storage reads on the hot path.
	SessionDisplayName string
	PRNumber           int
	PRTitle            string
	PRSourceBranch     string
	PRTargetBranch     string
	Provider           string
	Repo               string
	// WorkflowObjective is the human-written objective of a completed run, used
	// in place of the run id nobody recognizes.
	WorkflowObjective string
	// AttentionReason is the canonical stop reason for the attention family
	// (workflow.ReasonFixBudgetExhausted and friends). Carried as a plain string
	// so ports does not depend on the workflow package's vocabulary.
	AttentionReason string
	// Detail is the one-line, human-readable account of THIS occurrence — the
	// same sentence the Board shows next to the stop.
	Detail string

	// TaskID is the optional planned-task anchor carried onto the stored row.
	TaskID string
	// Source and SourceEventID are provenance: which producer raised this, and
	// the durable id of the observation it read. Both are stored so a row can
	// be traced back to the fact that caused it. Empty Source defaults to
	// lifecycle, which is where every intent came from before providers were
	// distinguished.
	Source        domain.NotificationSource
	SourceEventID string
}

// NotificationResolution is the lifecycle-to-notification-producer contract for
// closing notifications whose underlying issue went away. Lifecycle fills it
// after the durable fact that resolves the issue is written — the session left
// the needs-input family, the PR stopped waiting on a merge.
//
// PRURL wins when set (a PR-scoped notification can outlive its session's
// activity churn); otherwise the resolution is scoped to SessionID.
type NotificationResolution struct {
	Type       domain.NotificationType
	SessionID  domain.SessionID
	PRURL      string
	ResolvedAt time.Time
}
