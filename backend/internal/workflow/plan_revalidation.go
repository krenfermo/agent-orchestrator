package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// plan_revalidation.go — when AO's own authorized work is allowed to clear its
// own plan's staleness, and when it is not.
//
// THE OBSERVATION (wf-95d5bd82, the P2-A objective). The parent reported
//
//	plan: stale_but_revalidatable   (context_changed)
//
// and the change that made it stale was AO'S OWN. Its task children had
// committed on the branch during work/review/fix, the planner context manifest
// pins the repository HEAD, and so every commit AO itself authorized moved the
// manifest and invalidated the plan that authorized it. A plan that goes stale
// because the plan was executed is not a staleness signal; it is the plan
// working.
//
// THE DANGER, equally real: "AO's own commit" is exactly the story an unrelated
// change would also tell if AO guessed. plan_reuse.go refuses to execute a stale
// plan for a reason that has not weakened — a plan whose premises moved may no
// longer decompose the work correctly — and nothing here is allowed to turn that
// refusal into a shrug.
//
// THE RULE. Revalidation is automatic in exactly one shape, and every clause is
// a proof rather than a heuristic:
//
//  1. NOTHING THE PLANNER READS HAS CHANGED. The manifest's document set is
//     identical, path for path, and every document's content digest is
//     identical. The planner's inputs are byte-for-byte what they were, so the
//     plan's intent cannot have moved: there is no premise left for a
//     regeneration to reconsider. A single changed or added or removed document
//     ends this branch immediately — that is a real premise change, and it is a
//     person's.
//  2. THE ONLY DIFFERENCE IS THE COMMIT POINTER. Version, project id, project
//     path and branch are identical; the difference is HEAD, and optionally the
//     dirty flag going clean.
//  3. THE TREE IS CLEAN. An uncommitted working tree has no provenance at all —
//     nothing durable says who wrote it — so it can never be attributed.
//  4. THAT HEAD IS AO'S OWN. The repository's current HEAD is a commit AO
//     itself durably recorded as the output of THIS objective's own work: a
//     checkpoint on this run or on one of its plan's task children carrying that
//     head_sha. This is the mutation-provenance clause, and it is what separates
//     "the branch moved" from "the branch moved because this plan ran".
//
// Anything else stays `stale_but_revalidatable` and a person decides — an
// external commit, a changed AGENTS.md, a moved project path, a dirty tree, or
// a HEAD AO cannot recognise. Fail-closed is the default at every step: a fact
// that cannot be read is never read as permission.

// planContextDrift is the itemised comparison of a recorded planner-context
// manifest against the one AO would build now. It names WHAT moved, which is
// the whole difference between a staleness AO can discharge and one it cannot.
type planContextDrift struct {
	// Structural names the identity fields that differ (version, projectId,
	// projectPath, branch). Any entry here is disqualifying by itself.
	Structural []string
	// DocumentsChanged names planner documents whose path set or content digest
	// differs. Also disqualifying: these ARE the plan's premises.
	DocumentsChanged []string
	// HeadMoved / DirtyChanged are the two differences that can be attributable.
	HeadMoved    bool
	DirtyChanged bool

	RecordedHead string
	CurrentHead  string
	CurrentDirty bool
}

// Any reports whether the manifest differs at all.
func (d planContextDrift) Any() bool {
	return len(d.Structural) > 0 || len(d.DocumentsChanged) > 0 || d.HeadMoved || d.DirtyChanged
}

// OnlyTheCommitPointer reports clauses 1 and 2: nothing the planner reads has
// changed, and the difference is the commit pointer.
func (d planContextDrift) OnlyTheCommitPointer() bool {
	return len(d.Structural) == 0 && len(d.DocumentsChanged) == 0 && (d.HeadMoved || d.DirtyChanged)
}

// describePlanContextDrift compares the recorded manifest against the one AO
// would build now, item by item.
//
// known is false whenever the comparison could not be made at all — no builder,
// no recorded manifest, an unreadable project. It is never conflated with "no
// drift": plan_reuse.go's `unverifiable` classification depends on the
// difference.
func (c *Coordinator) describePlanContextDrift(
	ctx stdctx.Context, run domain.WorkflowRun, record domain.WorkflowPlanRecord,
) (planContextDrift, bool) {
	var out planContextDrift
	if c.projects == nil || c.plannerContextBuilder == nil {
		return out, false
	}
	if record.ContextManifestJSON == "" || record.ContextManifestJSON == "{}" {
		return out, false
	}
	var recorded PlannerContext
	if err := json.Unmarshal([]byte(record.ContextManifestJSON), &recorded); err != nil {
		return out, false
	}
	project, found, err := c.projects.GetProject(ctx, run.ProjectID)
	if err != nil || !found {
		return out, false
	}
	current, err := c.plannerContextBuilder.Build(ctx, project)
	if err != nil {
		return out, false
	}

	for _, f := range []struct{ name, was, now string }{
		{"version", recorded.Version, current.Version},
		{"projectId", recorded.ProjectID, current.ProjectID},
		{"projectPath", recorded.ProjectPath, current.ProjectPath},
		{"branch", recorded.Branch, current.Branch},
	} {
		if f.was != f.now {
			out.Structural = append(out.Structural, fmt.Sprintf("%s (%q -> %q)", f.name, f.was, f.now))
		}
	}

	// The documents, compared as a set of path -> digest. Both a missing path
	// and a moved digest are premise changes; naming which is what makes the
	// refusal explainable.
	was := map[string]string{}
	for _, doc := range recorded.Documents {
		was[doc.Path] = doc.SHA256
	}
	now := map[string]string{}
	for _, doc := range current.Documents {
		now[doc.Path] = doc.SHA256
	}
	for path, digest := range was {
		switch cur, present := now[path]; {
		case !present:
			out.DocumentsChanged = append(out.DocumentsChanged, path+" (removed)")
		case cur != digest:
			out.DocumentsChanged = append(out.DocumentsChanged, path+" (content changed)")
		}
	}
	for path := range now {
		if _, present := was[path]; !present {
			out.DocumentsChanged = append(out.DocumentsChanged, path+" (added)")
		}
	}
	sort.Strings(out.DocumentsChanged)

	out.RecordedHead = strings.TrimSpace(recorded.HeadSHA)
	out.CurrentHead = strings.TrimSpace(current.HeadSHA)
	out.CurrentDirty = current.Dirty
	out.HeadMoved = out.RecordedHead != out.CurrentHead
	out.DirtyChanged = recorded.Dirty != current.Dirty
	return out, true
}

// planStalenessIsOwnAuthorizedWork answers clause 3 and clause 4, and returns
// the sentence that goes on the assessment either way.
//
// Its default is "no". Every read failure, every unrecognised head and every
// missing fact resolves to a refusal, because the cost of a wrong "yes" is
// executing a plan whose premises moved and the cost of a wrong "no" is one
// human click.
func (c *Coordinator) planStalenessIsOwnAuthorizedWork(
	ctx stdctx.Context, run domain.WorkflowRun, drift planContextDrift,
) (bool, string) {
	if !drift.OnlyTheCommitPointer() {
		switch {
		case len(drift.Structural) > 0:
			return false, "the project context itself moved (" + strings.Join(drift.Structural, ", ") + ")"
		case len(drift.DocumentsChanged) > 0:
			return false, "the documents this plan was generated from changed (" + strings.Join(drift.DocumentsChanged, ", ") + ")"
		default:
			return false, "AO cannot name what moved in the planner context"
		}
	}
	if drift.CurrentDirty {
		return false, "the working tree has uncommitted changes, which nothing durable attributes to anybody"
	}
	if drift.CurrentHead == "" {
		return false, "AO could not read the repository's current head"
	}
	if !c.headWasProducedByThisObjective(ctx, run, drift.CurrentHead) {
		return false, fmt.Sprintf("head %s is not a commit AO recorded as this objective's own work", shortFingerprint(drift.CurrentHead))
	}
	return true, fmt.Sprintf(
		"nothing this plan was generated from has changed — the same documents, byte for byte — and the only difference is that the branch advanced to %s, which AO recorded as this objective's own authorized work",
		shortFingerprint(drift.CurrentHead))
}

// headWasProducedByThisObjective reports whether the given commit is one AO
// durably recorded for this objective: on the objective's own ledger, or on the
// ledger of one of its plan's task children.
//
// Checkpoints are the evidence because they are the only place AO writes a head
// SHA it OBSERVED at a boundary it authorized — a dispatch, a work result, a
// review target. A commit nobody recorded is, correctly, a commit AO cannot
// speak for.
func (c *Coordinator) headWasProducedByThisObjective(ctx stdctx.Context, run domain.WorkflowRun, head string) bool {
	head = strings.TrimSpace(head)
	if head == "" {
		return false
	}
	runIDs := []string{run.ID}
	if c.planStore != nil {
		tasks, err := c.planStore.ListWorkflowTasks(ctx, run.ID)
		if err != nil {
			// Cannot enumerate this objective's own children, so cannot prove
			// the head belongs to one of them.
			return false
		}
		for _, task := range tasks {
			if task.ExecutionRunID != nil && *task.ExecutionRunID != "" {
				runIDs = append(runIDs, *task.ExecutionRunID)
			}
		}
	}
	for _, id := range runIDs {
		cps, err := c.store.ListWorkflowCheckpoints(ctx, id)
		if err != nil {
			return false
		}
		for _, cp := range cps {
			if strings.TrimSpace(cp.HeadSHA) == head {
				return true
			}
		}
	}
	return false
}
