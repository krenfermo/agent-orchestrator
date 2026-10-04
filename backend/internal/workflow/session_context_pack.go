package workflow

import (
	stdctx "context"
	"encoding/json"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// BuildSessionContextPack wraps an already-computed TaskCheckpointSummary
// into Checkpoint 8M's versioned handoff format (checkpoint brief §7). It
// performs no IO and adds no new facts — callers build the summary once
// (BuildTaskCheckpointSummary) and reuse it both for telemetry (8J) and for
// a context pack (8M), never two independent computations of "what does
// this task look like right now."
func BuildSessionContextPack(role domain.WorkflowRole, facts domain.TaskCheckpointSummary) domain.SessionContextPack {
	return domain.SessionContextPack{Version: domain.SessionContextPackVersion, Role: role, Facts: facts}
}

// RenderContextPackForRole renders a compact, role-scoped plain-text block
// from a SessionContextPack (checkpoint brief §9) — every role gets a
// trimmed VIEW of the same underlying facts, never a second fetch/builder
// and never chain-of-thought or transcript content. Kept intentionally
// short: this is meant to replace re-sending full history, not to become a
// new verbose prompt section.
func RenderContextPackForRole(pack domain.SessionContextPack) string {
	return RenderContextPackForRoleExcluding(pack, nil)
}

// ContextPackField names one renderable section of a context pack, so a caller
// composing the pack with a prompt that ALREADY carries a section verbatim can
// omit the duplicate instead of sending the same bytes twice (Checkpoint
// 8P-E.13C). Omission is only ever legitimate when the excluded content is
// present in full elsewhere in the same message — this is a de-duplication
// mechanism, never a truncation one.
type ContextPackField string

// The fields a context pack may declare it omitted, each legitimate only while
// the same content is present in full elsewhere in the message.
const (
	ContextPackObjective          ContextPackField = "objective"
	ContextPackAcceptanceCriteria ContextPackField = "acceptanceCriteria"
	ContextPackReviewFindings     ContextPackField = "latestReviewFindings"
)

// fixPromptDuplicateFields are the sections BuildFixPrompt already includes
// verbatim in every fix message.
var fixPromptDuplicateFields = []ContextPackField{
	ContextPackObjective, ContextPackAcceptanceCriteria, ContextPackReviewFindings,
}

// RenderContextPackForRoleExcluding is RenderContextPackForRole with the named
// sections omitted. Every other fact is rendered unchanged.
func RenderContextPackForRoleExcluding(pack domain.SessionContextPack, exclude []ContextPackField) string {
	f := pack.Facts
	excluded := make(map[ContextPackField]bool, len(exclude))
	for _, field := range exclude {
		excluded[field] = true
	}
	if excluded[ContextPackObjective] {
		f.Objective = ""
	}
	if excluded[ContextPackAcceptanceCriteria] {
		f.AcceptanceCriteria = nil
	}
	if excluded[ContextPackReviewFindings] {
		f.LatestReviewFindings = ""
	}
	return renderContextPackFacts(pack, f)
}

func renderContextPackFacts(pack domain.SessionContextPack, f domain.TaskCheckpointSummary) string {
	var b strings.Builder
	writeLine := func(label string, v string) {
		if v == "" {
			return
		}
		b.WriteString(label)
		b.WriteString(": ")
		b.WriteString(v)
		b.WriteString("\n")
	}
	writeList := func(label string, vs []string) {
		if len(vs) == 0 {
			return
		}
		b.WriteString(label)
		b.WriteString(":\n")
		for _, v := range vs {
			b.WriteString("- ")
			b.WriteString(v)
			b.WriteString("\n")
		}
	}

	b.WriteString("SessionContextPack " + pack.Version + " (facts only, no transcript)\n")
	writeLine("Objective", f.Objective)
	writeLine("Task", f.Task)

	switch pack.Role {
	case domain.WorkflowRoleReviewer:
		writeList("Acceptance criteria", f.AcceptanceCriteria)
		writeList("Files changed", f.FilesChanged)
		writeList("Tests", f.Tests)
		writeLine("Fingerprint", f.CurrentFingerprint)
	case domain.WorkflowRoleDecisionResolver:
		writeLine("Next action", f.NextAction)
		writeList("Prior decisions", f.Decisions)
		writeLine("Fingerprint", f.CurrentFingerprint)
	case domain.WorkflowRoleFixWorker:
		writeLine("Latest review findings", f.LatestReviewFindings)
		writeList("Failed tests / active errors", f.ActiveErrors)
		writeList("Acceptance criteria", f.AcceptanceCriteria)
		writeList("Files changed", f.FilesChanged)
	default: // worker, planner, and any future role
		writeList("Acceptance criteria", f.AcceptanceCriteria)
		writeList("Relevant files", f.RelevantFiles)
		writeList("Decisions", f.Decisions)
		writeList("Active errors", f.ActiveErrors)
		writeList("Tests", f.Tests)
	}
	writeLine("Next action", f.NextAction)
	return strings.TrimRight(b.String(), "\n")
}

// sessionLifecycleDurablePhase is the checkpoint DurablePhase Checkpoint 8M
// adds. A lifecycle decision (and its context pack, when one was built) is
// persisted the same checkpoint-row way 8I's ReviewPolicyDecision and 8L's
// RoutingDecision already are — no new migration.
const sessionLifecycleDurablePhase = "session_lifecycle_decision"

// sessionLifecycleRecord is the durable payload for one lifecycle
// checkpoint row: the decision plus the context pack it produced, if any
// (REUSE never produces one — nothing to hand off).
type sessionLifecycleRecord struct {
	Decision    domain.SessionLifecycleDecision `json:"decision"`
	ContextPack *domain.SessionContextPack      `json:"contextPack,omitempty"`
}

// persistSessionLifecycleDecision durably records a lifecycle decision (and
// its context pack, if the action produced one) so a session switch/compact
// can always be explained later: which policy version, which reasons, and
// exactly what facts were handed to the next session — never a bare action.
func (c *Coordinator) persistSessionLifecycleDecision(ctx stdctx.Context, run domain.WorkflowRun, stepID *string, decision domain.SessionLifecycleDecision, pack *domain.SessionContextPack) error {
	payload, err := json.Marshal(sessionLifecycleRecord{Decision: decision, ContextPack: pack})
	if err != nil {
		return err
	}
	sid := decision.ToSessionID
	if sid == "" {
		sid = decision.FromSessionID
	}
	var sessionIDPtr *string
	if sid != "" {
		sessionIDPtr = &sid
	}
	// NextAction is deliberately left empty: RunDetail.NextAction mirrors
	// whichever checkpoint's NextAction was set most recently across the
	// WHOLE run (workflow.go), and this is a supplementary audit record,
	// not the workflow's real next directive — setting one here would
	// silently shadow the actual next_action another checkpoint in the same
	// call already recorded (e.g. "fix").
	_, err = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: stepID,
		ProjectID:      run.ProjectID,
		SessionID:      sessionIDPtr,
		RetryState:     string(payload),
		DurablePhase:   sessionLifecycleDurablePhase,
		PayloadVersion: domain.SessionLifecyclePolicyVersion,
		CreatedAt:      c.clock(),
	})
	return err
}

// decodeSessionLifecycleRecord unmarshals a session_lifecycle_decision
// checkpoint's RetryState. ok=false on any unmarshal error, mirroring every
// other decode-for-test helper in this package.
func decodeSessionLifecycleRecord(retryState string) (sessionLifecycleRecord, bool) {
	var rec sessionLifecycleRecord
	if retryState == "" {
		return rec, false
	}
	if err := json.Unmarshal([]byte(retryState), &rec); err != nil {
		return rec, false
	}
	return rec, rec.Decision.Action != ""
}

// DecodeSessionLifecycleDecisionForTest exposes decodeSessionLifecycleRecord
// to the external workflow_test package.
func DecodeSessionLifecycleDecisionForTest(retryState string) (domain.SessionLifecycleDecision, *domain.SessionContextPack, bool) {
	rec, ok := decodeSessionLifecycleRecord(retryState)
	return rec.Decision, rec.ContextPack, ok
}
