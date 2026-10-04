package workflow

import (
	stdctx "context"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// incident_child_pack.go — the child's evidence, in the parent's pack.
//
// The incident: a Medusa parent stopped on `child_needs_attention`, a person
// asked "what do I do", and the best AO could answer was "diagnose the child
// first" — while holding the child's run id the whole time. Every fact the
// answer needed was one query away, and the operator was sent to run a second
// investigation to discover something AO already knew.
//
// So when a parent's stop is ABOUT a child, the parent's pack carries a bounded
// pack of that child's own evidence. Bounded is the operative word: it is the
// same budget discipline as the rest of the pack (whole sections, dropped from
// the bottom, never half a diff), and it deliberately carries the child's
// REASONS and RESULTS rather than the child's whole history — the parent's
// diagnostician is being told why its task stopped, not being handed a second
// run to reason about from scratch.

const (
	// incidentChildPackMaxCheckpoints bounds how much of the child's ledger
	// travels. The newest rows explain a stop; the child's whole life does not.
	incidentChildPackMaxCheckpoints = 12
	// incidentChildPackMaxSteps bounds the step table, which is small by
	// construction but is bounded anyway so a malformed run cannot blow the
	// budget.
	incidentChildPackMaxSteps = 12
)

// childEvidence is everything the parent's pack says about one stopped child.
// Every field is read from the child's own durable state; nothing is inferred
// from the parent's.
type childEvidence struct {
	RunID  string
	TaskID string
	Title  string
	State  string

	StopReason string
	StopDetail string
	// Attempt is the child's newest attempt: which step, which outcome, which
	// error class. It is the single most common thing a person needs and the
	// thing they currently have to open a second run to see.
	Attempt string

	Steps       []string
	Checkpoints []string

	// SessionState is the child's worker session as AO recorded it, and
	// Provider the harness running it.
	SessionState string
	Provider     string

	ReviewVerdict  string
	ReviewFindings string
	VerifyResult   string

	// WorkspaceStatus is the child's worktree — branch, HEAD, and whether it is
	// clean — and Provenance the newest attribution AO recorded for it. The two
	// together answer "is the child's work actually there, and whose is it".
	WorkspaceStatus string
	Provenance      string
}

// attachIncidentChildFacts adds the child evidence pack when the parent's stop
// is about a child, and does nothing at all otherwise.
//
// It is deliberately driven by the STOP REASON rather than by "this run has
// children": a master run stopped on its own dirty worktree does not need its
// children's ledgers, and shipping them would spend the pack budget on evidence
// nobody asked for.
func (c *Coordinator) attachIncidentChildFacts(ctx stdctx.Context, detail RunDetail, in *IncidentPackInput) {
	if in.StopReason != ReasonChildNeedsAttention && in.StopReason != ReasonChildFailed {
		return
	}
	if c.planStore == nil {
		return
	}
	tasks, err := c.planStore.ListWorkflowTasks(ctx, detail.Run.ID)
	if err != nil {
		return
	}
	var blocks []string
	for _, task := range tasks {
		if task.ExecutionRunID == nil {
			continue
		}
		child, ok, cerr := c.store.GetWorkflowRun(ctx, *task.ExecutionRunID)
		if cerr != nil || !ok {
			continue
		}
		// Only the children that are actually stopped. A sibling running
		// perfectly well is not evidence about why the parent stopped.
		if child.State != domain.WorkflowRunNeedsAttention && child.State != domain.WorkflowRunFailed {
			continue
		}
		ev := c.collectChildEvidence(ctx, task, child)
		blocks = append(blocks, renderChildEvidence(ev))
		if len(blocks) >= 3 {
			// Three stopped children is already more than a diagnosis can act
			// on at once, and the bound is what keeps a wide plan from
			// crowding out the parent's own evidence.
			break
		}
	}
	if len(blocks) == 0 {
		return
	}
	in.ChildEvidence = strings.Join(blocks, "\n\n")
}

// collectChildEvidence reads one stopped child's durable state.
func (c *Coordinator) collectChildEvidence(ctx stdctx.Context, task domain.WorkflowTask, child domain.WorkflowRun) childEvidence {
	ev := childEvidence{
		RunID: child.ID, TaskID: task.ID, Title: task.Title, State: string(child.State),
	}
	if reason, _, ok := c.stopReason(ctx, child); ok {
		ev.StopReason = reason
		ev.StopDetail = c.stopDetailFor(ctx, child, reason)
	}

	steps, err := c.store.ListWorkflowSteps(ctx, child.ID)
	if err == nil {
		for i, s := range steps {
			if i >= incidentChildPackMaxSteps {
				break
			}
			ev.Steps = append(ev.Steps, fmt.Sprintf("%s: %s", s.Kind, s.State))
			attempts, aerr := c.store.ListWorkflowAttempts(ctx, s.ID)
			if aerr != nil || len(attempts) == 0 {
				continue
			}
			a := attempts[len(attempts)-1]
			outcome := string(a.Outcome)
			if outcome == "" {
				outcome = "(still in flight)"
			}
			line := fmt.Sprintf("%s attempt %d: %s", s.Kind, a.AttemptNumber, outcome)
			if a.ErrorClass != "" {
				line += " (" + string(a.ErrorClass) + ")"
			}
			ev.Attempt = line
		}
	}

	cps, err := c.store.ListWorkflowCheckpoints(ctx, child.ID)
	if err == nil {
		start := len(cps) - incidentChildPackMaxCheckpoints
		if start < 0 {
			start = 0
		}
		for _, cp := range cps[start:] {
			ev.Checkpoints = append(ev.Checkpoints,
				fmt.Sprintf("%s  %s  %s", cp.CreatedAt.UTC().Format("15:04:05"), cp.DurablePhase, oneLine(cp.NextAction)))
			if cp.DurablePhase == workspaceProvenancePhase {
				ev.Provenance = oneLine(cp.NextAction)
			}
		}
	}

	// The child's worker session and its worktree, from the same facts ports the
	// parent's own pack reads — never from the filesystem directly.
	if c.sessionFacts != nil {
		for _, s := range steps {
			if s.Kind != domain.WorkflowStepWork || s.SessionID == nil {
				continue
			}
			sess, found, serr := c.sessionFacts.GetSession(ctx, domain.SessionID(*s.SessionID))
			if serr != nil || !found {
				continue
			}
			ev.Provider = string(sess.Harness)
			ev.SessionState = fmt.Sprintf("%s (terminated=%v, lastActivity=%s, firstSignal=%s)",
				sess.Activity.State, sess.IsTerminated,
				stampOrNever(sess.Activity.LastActivityAt.IsZero(), sess.Activity.LastActivityAt.UTC().Format("15:04:05")),
				stampOrNever(sess.FirstSignalAt.IsZero(), sess.FirstSignalAt.UTC().Format("15:04:05")))
			if c.workspaceFacts != nil {
				if obs, ok := c.observeFixWorkspace(ctx, sess); ok {
					ev.WorkspaceStatus = fmt.Sprintf("branch=%s head=%s dirty=%v staged=%v untracked=%v changes=%d",
						obs.Branch, shortFingerprint(obs.HeadSHA), obs.Dirty, obs.Staged, obs.Untracked, len(obs.Changes))
				}
			}
		}
	}

	if c.reviewRuns != nil {
		for _, s := range steps {
			if s.Kind != domain.WorkflowStepReview || s.ReviewRunID == nil {
				continue
			}
			if rr, ok, rerr := c.reviewRuns.GetReviewRun(ctx, *s.ReviewRunID); rerr == nil && ok {
				// Effective, for the same reason the parent advisor pack uses it:
				// a child whose review concluded via an adopted late verdict must
				// hand the Incident Advisor that verdict and those findings, not
				// two empty strings.
				ev.ReviewVerdict = string(rr.EffectiveVerdict())
				ev.ReviewFindings = rr.EffectiveBody()
			}
		}
	}
	if result, ok, verr := c.latestVerifyResult(ctx, child.ID); verr == nil && ok {
		ev.VerifyResult = fmt.Sprintf("passed=%v class=%s reviewed=%s pre=%s",
			result.Passed, result.ErrorClass,
			shortFingerprint(result.ReviewedFingerprint), shortFingerprint(result.PreFingerprint))
	}
	return ev
}

func stampOrNever(zero bool, formatted string) string {
	if zero {
		return "never"
	}
	return formatted
}

// renderChildEvidence lays one child's evidence out as the pack's plain text.
// Ordered the way a person reads it: what stopped, why, then the state that
// explains it.
func renderChildEvidence(ev childEvidence) string {
	var b strings.Builder
	fmt.Fprintf(&b, "task %s (%s)\nchild run: %s\nstate: %s\n", ev.TaskID, ev.Title, ev.RunID, ev.State)
	if ev.StopReason != "" {
		fmt.Fprintf(&b, "reason: %s\ndetail: %s\n", ev.StopReason, ev.StopDetail)
	}
	if ev.Attempt != "" {
		fmt.Fprintf(&b, "newest attempt: %s\n", ev.Attempt)
	}
	if len(ev.Steps) > 0 {
		fmt.Fprintf(&b, "steps: %s\n", strings.Join(ev.Steps, ", "))
	}
	if ev.Provider != "" || ev.SessionState != "" {
		fmt.Fprintf(&b, "worker session: %s on %s\n", ev.SessionState, ev.Provider)
	}
	if ev.WorkspaceStatus != "" {
		fmt.Fprintf(&b, "workspace: %s\n", ev.WorkspaceStatus)
	}
	if ev.Provenance != "" {
		fmt.Fprintf(&b, "workspace provenance: %s\n", ev.Provenance)
	}
	if ev.ReviewVerdict != "" {
		fmt.Fprintf(&b, "review verdict: %s\n", ev.ReviewVerdict)
	}
	if ev.VerifyResult != "" {
		fmt.Fprintf(&b, "verify: %s\n", ev.VerifyResult)
	}
	if len(ev.Checkpoints) > 0 {
		fmt.Fprintf(&b, "recent checkpoints:\n  %s\n", strings.Join(ev.Checkpoints, "\n  "))
	}
	if ev.ReviewFindings != "" {
		fmt.Fprintf(&b, "latest review findings:\n%s\n", ev.ReviewFindings)
	}
	return strings.TrimSpace(b.String())
}
