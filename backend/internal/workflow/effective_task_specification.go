package workflow

import (
	stdctx "context"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// effective_task_specification.go — the one authoritative answer to "what is
// this task actually required to do right now".
//
// Three durable facts describe a task's requirements, and they can disagree:
//
//   - run.Objective is the ORIGINAL intent, written when the plan was made. It
//     is history. It is never rewritten, because rewriting it would destroy the
//     record of what was actually asked for and make every later audit a
//     reconstruction.
//   - the task's acceptance criteria are the requirements IN FORCE now.
//   - an approved criterion amendment is the AUTHORITY that says which original
//     requirement stopped applying, who approved that, and on what evidence.
//
// Until this existed, agents were handed the first two and never the third.
// That is not merely incomplete, it is contradictory: Task 8's objective ended
// with "confirm via git status that the pre-existing uncommitted postrunqa
// changes remain present", while its acceptance criteria — amended, approved by
// the repository owner, with five pieces of evidence — said the opposite. An
// agent receiving both has no way to know which one governs, and the honest
// readings of that prompt disagree. A reviewer that follows the objective
// blocks correct work forever; one that follows the criteria looks like it is
// ignoring the objective. Neither is the agent's fault.
//
// So the fix is not to change the objective and not to hide it. It is to hand
// every agent the objective, the criteria in force, and the approved amendments
// that reconcile them, with an explicit statement of which wins — the same
// specification, built by the same function, for every role. Five roles
// rendering this five different ways would be five chances for them to disagree
// about what the task requires, which is the failure this exists to end.
//
// The specification is derived, never stored: it is recomputed from the task's
// current criteria and its append-only amendment ledger every time it is built.
// That is what makes it idempotent and restart-safe without a cache to
// invalidate — there is no state here that a crash could leave half-written.

// effectiveSpecAuthorityNote is the sentence that resolves the conflict. It is
// a single constant because all five roles must be told the same thing in the
// same words; a role given a softer or stronger phrasing would apply a
// different standard to the same task.
const effectiveSpecAuthorityNote = "The original objective is preserved for audit/history. Where it conflicts " +
	"with an approved amendment, the approved amendment and current acceptance criteria are authoritative."

// EffectiveCriterionAmendment is one approved change, rendered for an agent.
// It carries the approval and the evidence, not just the outcome, so an agent
// (or a person reading the transcript) can see that a requirement was retired
// by a named human for stated reasons rather than quietly dropped.
type EffectiveCriterionAmendment struct {
	OriginalCriterion string
	AmendedCriterion  string
	Disposition       domain.WorkflowTaskCriterionDisposition
	Reason            string
	Evidence          []string
	ApprovedBy        string
}

// EffectiveTaskSpecification is what a task is required to do, as one coherent
// statement: the historical objective, the criteria in force, and the approved
// amendments that reconcile the two.
type EffectiveTaskSpecification struct {
	// Objective is run.Objective, verbatim and unmodified. It is carried here
	// so callers pass the specification rather than the objective plus some
	// extras, which is how the two drift apart.
	Objective string
	// AcceptanceCriteria are the requirements as they now stand.
	AcceptanceCriteria []string
	// Amendments are the approved changes, oldest first.
	Amendments []EffectiveCriterionAmendment
}

// HasAmendments reports whether anything reconciles the objective and the
// criteria. When nothing does, the specification adds nothing to a prompt and
// every builder renders exactly what it rendered before this existed.
func (s EffectiveTaskSpecification) HasAmendments() bool { return len(s.Amendments) > 0 }

// effectiveTaskSpecification builds the specification for one execution run.
//
// Amendments live on the MASTER run's ledger, keyed by task, because that is
// where the plan they amend lives. A run with no master parent — a standalone
// objective — has no planned task and therefore no amendments, and gets a
// specification that is exactly its objective and criteria.
//
// Every failure to read degrades to "no amendments", never to an error: a
// missing amendment makes a prompt no worse than it was before this file
// existed, while failing a dispatch over one would turn an audit-trail read
// into an outage.
func (c *Coordinator) effectiveTaskSpecification(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	acceptanceCriteria []string,
) EffectiveTaskSpecification {
	spec := EffectiveTaskSpecification{
		Objective:          run.Objective,
		AcceptanceCriteria: acceptanceCriteria,
	}
	if c.planStore == nil || run.ParentWorkflowID == nil || run.PlannedTaskID == nil {
		return spec
	}
	amendments, err := c.planStore.ListWorkflowTaskCriterionAmendments(ctx, *run.ParentWorkflowID)
	if err != nil {
		return spec
	}
	for _, a := range amendments {
		if a.TaskID != *run.PlannedTaskID {
			continue
		}
		spec.Amendments = append(spec.Amendments, EffectiveCriterionAmendment{
			OriginalCriterion: a.OriginalCriterion,
			AmendedCriterion:  a.AmendedCriterion,
			Disposition:       a.Disposition,
			Reason:            a.Reason,
			Evidence:          a.Evidence,
			ApprovedBy:        a.ApprovedBy,
		})
	}
	return spec
}

// RenderEffectiveSpecification renders the amendments section every prompt
// shares, or "" when there is nothing to reconcile.
//
// Returning "" for the unamended case is what keeps this invisible to the
// overwhelming majority of runs: the builders concatenate it, so a task with no
// amendments produces the identical prompt it produced before.
func RenderEffectiveSpecification(spec EffectiveTaskSpecification) string {
	if !spec.HasAmendments() {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nApproved amendments to this task's requirements:\n\n")
	b.WriteString(effectiveSpecAuthorityNote + "\n")
	for i, a := range spec.Amendments {
		b.WriteString("\nAmendment " + strconv.Itoa(i+1) + " of " + strconv.Itoa(len(spec.Amendments)) + "\n")
		b.WriteString("- Original objective/requirement: " + a.OriginalCriterion + "\n")
		b.WriteString("- Disposition: " + string(a.Disposition) + "\n")
		if a.ApprovedBy != "" {
			b.WriteString("- Approved by: " + a.ApprovedBy + "\n")
		}
		if a.Reason != "" {
			b.WriteString("- Reason: " + a.Reason + "\n")
		}
		if len(a.Evidence) > 0 {
			b.WriteString("- Evidence:\n")
			for _, e := range a.Evidence {
				b.WriteString("  - " + e + "\n")
			}
		}
		switch a.Disposition {
		case domain.WorkflowTaskCriterionObsolete:
			// The strongest statement this file makes, and the one that most
			// needs to be unambiguous: an obsolete requirement is not a
			// softened requirement. An agent that "partially" honours it is
			// doing exactly what the amendment exists to stop.
			b.WriteString("- This requirement is NO LONGER IN FORCE. Do not evaluate it, do not report it\n" +
				"  as unmet, and do not attempt to satisfy it or recreate the condition it described.\n")
		default:
			if a.AmendedCriterion != "" {
				b.WriteString("- Current criterion in force (this replaces the original above): " + a.AmendedCriterion + "\n")
			}
		}
	}
	return b.String()
}
