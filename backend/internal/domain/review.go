package domain

import (
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/pkg/contract"
)

// ErrDuplicateReviewRun is returned by InsertReviewRun when a run already exists
// for the same worker session and target commit (the partial unique index from
// migration 0013). It lets the review engine fall back to the recorded run
// instead of surfacing a raw storage error after a reviewer may have launched.
var ErrDuplicateReviewRun = errors.New("domain: review run already exists for session and target sha")

// Review is the per-worker, per-reviewer-harness code-review record. A repeat
// trigger for the same harness reuses this row; the per-pass facts live on
// ReviewRun.
type Review struct {
	ID        string          `json:"id"`
	SessionID SessionID       `json:"sessionId"`
	ProjectID ProjectID       `json:"projectId"`
	Harness   ReviewerHarness `json:"harness"`
	PRURL     string          `json:"prUrl"`
	// ReviewerHandleID is the runtime handle of the live reviewer pane, reused
	// across passes and exposed so the UI can attach its terminal.
	ReviewerHandleID string    `json:"reviewerHandleId"`
	AgentSessionID   string    `json:"agentSessionId"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

// ReviewRun is one review pass against a worker's PR.
type ReviewRun struct {
	ID        string    `json:"id"`
	ReviewID  string    `json:"reviewId"`
	SessionID SessionID `json:"sessionId"`
	// BatchID groups review runs created by one trigger so worker feedback can
	// be delivered once after the whole trigger batch is terminal. Empty marks
	// legacy/single-run delivery.
	BatchID string          `json:"batchId"`
	Harness ReviewerHarness `json:"harness"`
	// TriggerSource records whether this pass was requested by a user or by the
	// daemon auto-review coordinator.
	TriggerSource ReviewTriggerSource `json:"triggerSource" enum:"manual,auto"`
	PRURL         string              `json:"prUrl"`
	// TargetSHA is the PR head commit this pass reviewed.
	TargetSHA string          `json:"targetSha"`
	Status    ReviewRunStatus `json:"status"`
	Verdict   ReviewVerdict   `json:"verdict"`
	// Body is the review text the reviewer submitted. It is recorded for AO's
	// own tracking; the reviewer also posts the review to the PR itself.
	Body string `json:"body"`
	// GithubReviewID is the id of the GitHub PR review the reviewer posted for
	// this pass (the `gh api .../pulls/{n}/reviews` object id), recorded at
	// submit time. It is empty when the reviewer could not post to the provider.
	// When the pass requests changes, AO includes it in the message to the
	// worker so the worker knows exactly which review to address and reply to.
	GithubReviewID string     `json:"githubReviewId"`
	CreatedAt      time.Time  `json:"createdAt"`
	DeliveredAt    *time.Time `json:"deliveredAt,omitempty"`
	// AutoInjectReview snapshots the session policy when this result is first
	// recorded. Later toggle changes must not rewrite or deliver this run.
	AutoInjectReview bool `json:"autoInjectReview"`

	// LateVerdict is a verdict the reviewer produced AFTER AO had already
	// closed this run out (migration 0135). It is preserved in full and it is
	// never authority on its own: only the workflow step that still points at
	// this run may adopt it. See workflow/review_authority.go.
	LateVerdict ReviewVerdict `json:"lateVerdict,omitempty"`
	// LateVerdictBody is that verdict's review text.
	LateVerdictBody string `json:"lateVerdictBody,omitempty"`
	// LateVerdictAt is when the late verdict was recorded. Nil when there is
	// none.
	LateVerdictAt *time.Time `json:"lateVerdictAt,omitempty"`
	// SupersededBy names the review run that took authority over this one.
	// Empty when nothing replaced it.
	SupersededBy string `json:"supersededBy,omitempty"`
}

// EffectiveVerdict is THE verdict this review run produced, whichever column it
// landed in — and it is what every workflow decision must consume.
//
// AO records a verdict in one of two places. The normal submission path writes
// `verdict` while the run is still running. A reviewer that finished after AO
// had already closed its run out writes `late_verdict` instead, because
// migration 0135 deliberately does NOT promote a late arrival into the
// authoritative column (the partial UNIQUE index would collide with the
// replacement review of the same target, and a late arrival must never be able
// to fail).
//
// That storage split is correct and it is invisible to anything downstream:
// "the reviewer asked for changes" means the same thing and must drive the same
// fix cycle regardless of which column holds it. Reading `Verdict` directly is
// how an adopted late `changes_requested` became authoritative and then
// dispatched no fix at all — the review concluded, the findings existed, and the
// cascade saw an empty verdict and moved on.
//
// A late verdict counts only while nothing has SUPERSEDED this run. Once a
// replacement took authority, the old reviewer's answer is evidence and never a
// decision — which is what keeps a stale late verdict out of the cascade. A
// normal verdict is unconditional: it was authoritative when it was recorded,
// and that does not expire.
func (r ReviewRun) EffectiveVerdict() ReviewVerdict {
	if r.Verdict.Valid() {
		return r.Verdict
	}
	if r.SupersededBy != "" {
		return ""
	}
	return r.LateVerdict
}

// EffectiveBody is the review text belonging to EffectiveVerdict — the findings
// a fix worker is given. Same precedence, for the same reason: findings that
// travel with a late verdict are the reviewer's real output.
func (r ReviewRun) EffectiveBody() string {
	if r.Verdict.Valid() {
		return r.Body
	}
	if r.SupersededBy != "" {
		return ""
	}
	return r.LateVerdictBody
}

// EffectiveVerdictAt is when the effective verdict was recorded, or nil when AO
// holds no timestamp for it. The normal submission path has never recorded one
// (there is no such column), so it reports nil rather than inventing CreatedAt —
// which is when the review STARTED, not when it concluded.
func (r ReviewRun) EffectiveVerdictAt() *time.Time {
	if r.Verdict.Valid() {
		return nil
	}
	if r.SupersededBy != "" {
		return nil
	}
	return r.LateVerdictAt
}

// HasEffectiveVerdict reports whether this run concluded with a verdict that
// still speaks for the step it is attached to.
func (r ReviewRun) HasEffectiveVerdict() bool { return r.EffectiveVerdict().Valid() }

// HasDurableVerdict reports whether this run recorded a verdict while it was
// authoritative. It is the fact "has this review actually concluded" —
// deliberately NOT satisfied by a late verdict, which is a reviewer's answer to
// a question AO had already stopped asking.
func (r ReviewRun) HasDurableVerdict() bool { return r.Verdict.Valid() }

// TerminalWithoutVerdict is the shape AO's own bookkeeping writes when it gives
// up on a review: a closed-out run that never recorded a verdict. It is the
// entry condition for review-authority reconciliation.
func (r ReviewRun) TerminalWithoutVerdict() bool {
	switch r.Status {
	case ReviewRunCancelled, ReviewRunFailed:
		return !r.HasDurableVerdict()
	default:
		return false
	}
}

// ReviewTriggerSource identifies who initiated a review pass.
type ReviewTriggerSource string

const (
	// ReviewTriggerManual marks a user-initiated review pass.
	ReviewTriggerManual ReviewTriggerSource = "manual"
	// ReviewTriggerAuto marks a daemon-initiated review pass.
	ReviewTriggerAuto ReviewTriggerSource = "auto"
)

// ReviewRunStatus is the lifecycle state of a single review pass.
type ReviewRunStatus = contract.AOReviewRunStatus

// Review run statuses.
const (
	ReviewRunRunning   = contract.AOReviewRunRunning
	ReviewRunComplete  = contract.AOReviewRunComplete
	ReviewRunDelivered = contract.AOReviewRunDelivered
	ReviewRunFailed    = contract.AOReviewRunFailed
	ReviewRunCancelled = contract.AOReviewRunCancelled
)

// ReviewVerdict is the outcome a reviewer reports. The empty verdict marks a
// run that has not produced an outcome yet.
type ReviewVerdict = contract.AOReviewVerdict

// Review verdicts.
const (
	VerdictNone             = contract.AOReviewVerdictNone
	VerdictApproved         = contract.AOReviewVerdictApproved
	VerdictChangesRequested = contract.AOReviewVerdictChangesRequested
)
