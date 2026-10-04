package workflow

import (
	stdctx "context"
	"encoding/json"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// read_only_completion.go — the one place AO decides that "the worktree is
// unchanged" is the RESULT of a task rather than the absence of one.
//
// The incident this file exists for: a plan whose task was, in its own words,
// "Verify current repository state (build, tests, vet, git status)", and whose
// acceptance criteria said in as many words that nothing was to be edited, that
// the known dirty baseline was to be preserved, and that no additional modified
// or untracked file was to appear. The worker ran the checks and went idle.
// evaluateWorkStepProgress reached its ActivityIdle branch, found no commit and
// no new dirt, and stopped the run:
//
//	ambiguous_worker_state
//	worker idle with no verifiable change — needs human review
//
// Which is false. AO was not unable to prove what the worker did; the worker
// did precisely what the plan required, and the plan had said in advance what
// that would look like. The classifier simply had no way to hear it.
//
// The fix is NOT to soften the ambiguity rule. `ambiguous_worker_state` on an
// implementation task that produced nothing is correct and stays exactly as it
// was — that is the whole point of it. What changes is that AO now knows which
// tasks are implementation tasks, because the plan declares it
// (domain.WorkflowWriteIntent), the declaration is durable (the task's scope
// and the execution run's own plan artifact), and it is conservative by
// construction: a plan that declares nothing, a plan written before the field
// existed, and a standalone objective all resolve to "mutating", which is the
// behaviour that already existed.
//
// Two further properties are load-bearing:
//
//   - The read-only verdict is a COMPARISON, never a belief. AO compares the
//     workspace fingerprint recorded at dispatch against the one it observes
//     now (both computed by WorkspaceFingerprint, which hashes HEAD, the three
//     git-status flags, and the content of every path git names). "Nothing
//     changed" is therefore a git-verified fact — and the known dirty baseline
//     is inside the dispatch-time fingerprint, so preserving it compares equal
//     while adding one file to it does not.
//   - It does NOT end the run. A satisfied read-only task lands on exactly the
//     same "start_review" transition an implementation task lands on, so review
//     and the verification step still run, and the plan's declared commands,
//     exit codes and file checks are still executed and still authoritative.
//     Declaring a task read-only buys it the right to be judged; it never buys
//     it the right to be believed.

// readOnlyWorkspaceVerdict is the comparison's outcome. `unknown` is a real
// answer and the most important one: it is what AO returns when it could not
// obtain the dispatch-time baseline or a fresh observation, and it deliberately
// leaves every pre-existing decision in place.
type readOnlyWorkspaceVerdict string

const (
	readOnlyWorkspaceUnknown   readOnlyWorkspaceVerdict = "unknown"
	readOnlyWorkspaceUnchanged readOnlyWorkspaceVerdict = "unchanged"
	readOnlyWorkspaceMutated   readOnlyWorkspaceVerdict = "mutated"
)

// readOnlyCompletionEvidencePhase is the durable record of a work-step
// observation the read-only rule decided. It is written for BOTH outcomes —
// the accepted no-change completion and the mutation stop — because both are
// conclusions AO reached from a fingerprint comparison, and a comparison whose
// two sides were never written down is one nobody can check afterwards.
const readOnlyCompletionEvidencePhase = "read_only_completion_evidence"

// readOnlyExpectation is what the pure evaluator is told about the task's
// declared write intent, and about whether the workspace honoured it.
//
// It is a value, not a callback, for the same reason workerEvidence is: every
// branch of evaluateWorkStepProgress must stay testable without a store, a git
// repository or a runtime.
type readOnlyExpectation struct {
	// Declared is true ONLY when the plan durably declared this task read-only.
	// It is false for `mutating`, and false for every task whose intent nobody
	// stated — which is what keeps the fail-closed path untouched.
	Declared bool
	// Verdict is the fingerprint comparison. Meaningless unless Declared.
	Verdict readOnlyWorkspaceVerdict
	// Baseline and Observed are the two fingerprints compared, recorded so the
	// verdict can be audited rather than trusted. Empty when unavailable.
	Baseline string
	Observed string
	// Detail is the human sentence behind Verdict.
	Detail string
}

// Satisfied reports that the plan declared this task read-only AND the
// workspace is git-verified to be exactly as it was when the worker started.
func (e readOnlyExpectation) Satisfied() bool {
	return e.Declared && e.Verdict == readOnlyWorkspaceUnchanged
}

// Violated reports that the plan declared this task read-only AND the workspace
// changed underneath it anyway. That is a stop, and a different stop from the
// ambiguity: AO is not failing to prove something here, it is proving that the
// declared contract was broken.
func (e readOnlyExpectation) Violated() bool {
	return e.Declared && e.Verdict == readOnlyWorkspaceMutated
}

// declaredWriteIntent resolves the run's own durable write-intent declaration
// from its plan step artifact.
//
// The artifact is the right carrier rather than the master run's task row: it
// is written at dispatch and lives on the EXECUTION run, so it is readable
// without reaching across the parent/child boundary, and it survives a daemon
// restart intact — which is what makes the completion verdict converge to the
// same answer after a crash as before one.
//
// Every failure to read degrades to Unspecified, i.e. mutating. An intent AO
// cannot read is an intent AO does not have.
func (c *Coordinator) declaredWriteIntent(ctx stdctx.Context, run domain.WorkflowRun) domain.WorkflowWriteIntent {
	steps, err := c.store.ListWorkflowSteps(ctx, run.ID)
	if err != nil {
		return domain.WorkflowWriteIntentUnspecified
	}
	for _, s := range steps {
		if s.Kind != domain.WorkflowStepPlan || s.ArtifactJSON == "" || s.ArtifactJSON == "{}" {
			continue
		}
		artifact, err := UnmarshalPlanArtifact(s.ArtifactJSON)
		if err != nil {
			continue
		}
		return domain.NormalizeWorkflowWriteIntent(string(artifact.WriteIntent))
	}
	return domain.WorkflowWriteIntentUnspecified
}

// dispatchBaselineFingerprint is the workspace fingerprint recorded when this
// step's worker was confirmed — the state the worker was handed.
//
// It comes off the dispatch provenance records rather than being recomputed,
// because recomputing it now would compare the tree against itself and always
// report "unchanged". The newest record that actually carries a fingerprint
// wins: a later boundary row (an unconfirmed launch, a failure) may legitimately
// carry none, and taking the plain newest row would then lose a baseline AO
// does hold.
func (c *Coordinator) dispatchBaselineFingerprint(ctx stdctx.Context, stepID string) (string, bool) {
	ps, ok := c.provenanceStore()
	if !ok || stepID == "" {
		return "", false
	}
	records, err := ps.ListWorkflowDispatchCheckpointsByStep(ctx, stepID)
	if err != nil {
		return "", false
	}
	for i := len(records) - 1; i >= 0; i-- {
		if fp := records[i].WorkspaceFingerprint; fp != "" {
			return fp, true
		}
	}
	return "", false
}

// resolveReadOnlyExpectation answers, for one work-step observation, whether
// the read-only rule applies and what it concludes.
//
// It costs one step-list read (and, only for a task that IS declared read-only,
// one dispatch-record read). Callers must only invoke it on an observation that
// could actually be decided by it — see observeWorkStep — so a healthy polling
// run pays for neither.
func (c *Coordinator) resolveReadOnlyExpectation(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	workspaceAvailable bool,
	obs ports.WorkspaceObservation,
) readOnlyExpectation {
	if !c.declaredWriteIntent(ctx, run).ReadOnly() {
		return readOnlyExpectation{}
	}
	e := readOnlyExpectation{Declared: true, Verdict: readOnlyWorkspaceUnknown}
	baseline, hasBaseline := c.dispatchBaselineFingerprint(ctx, step.ID)
	e.Baseline = baseline
	if !workspaceAvailable {
		e.Detail = "the task is declared read-only, but no fresh workspace observation was available to compare against its dispatch-time state"
		return e
	}
	e.Observed = WorkspaceFingerprint(obs)
	if !hasBaseline {
		// No baseline means no comparison, and a read-only claim with no
		// comparison under it is exactly the unevidenced conclusion this whole
		// area exists to refuse. Every pre-existing decision stands.
		e.Detail = "the task is declared read-only, but AO holds no dispatch-time workspace fingerprint to compare the current worktree against"
		return e
	}
	if e.Observed == baseline {
		e.Verdict = readOnlyWorkspaceUnchanged
		e.Detail = "the task is declared read-only and the worktree is git-verified unchanged since dispatch (workspace fingerprint " +
			shortFingerprint(baseline) + "), including any pre-existing uncommitted baseline"
		return e
	}
	e.Verdict = readOnlyWorkspaceMutated
	e.Detail = "the task is declared read-only but the worktree changed since dispatch (workspace fingerprint " +
		shortFingerprint(baseline) + " -> " + shortFingerprint(e.Observed) + ")"
	return e
}

// recordReadOnlyCompletionEvidence appends the durable record of one read-only
// verdict: which intent was declared, which two fingerprints were compared, and
// what that comparison concluded.
//
// It carries the step's workspace identity forward from the checkpoint it
// supersedes for exactly the reason ambiguityCheckpoint does — a row written
// against a step BECOMES "the latest checkpoint for this step", and some twenty
// readers resolve the worktree, branch, base and session from it, so a row that
// dropped them would destroy already-correct work rather than merely fail to
// help. It reuses that same builder so the two can never drift apart.
//
// Best-effort by design: see the call site in observeWorkStep.
func (c *Coordinator) recordReadOnlyCompletionEvidence(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, e readOnlyExpectation,
) {
	var carry domain.WorkflowCheckpoint
	if step.ID != "" {
		if cp, ok, err := c.store.GetLatestWorkflowCheckpointByStep(ctx, step.ID); err == nil && ok {
			carry = cp
		}
	}
	payload, err := json.Marshal(struct {
		WriteIntent domain.WorkflowWriteIntent `json:"writeIntent"`
		Verdict     readOnlyWorkspaceVerdict   `json:"verdict"`
		Baseline    string                     `json:"baselineFingerprint"`
		Observed    string                     `json:"observedFingerprint"`
		Detail      string                     `json:"detail"`
	}{
		WriteIntent: domain.WorkflowWriteIntentReadOnly,
		Verdict:     e.Verdict,
		Baseline:    e.Baseline,
		Observed:    e.Observed,
		Detail:      e.Detail,
	})
	if err != nil {
		return
	}
	cp := c.ambiguityCheckpoint(run, step, carry, readOnlyCompletionEvidencePhase, e.Detail, string(payload))
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, cp); err != nil && c.log != nil {
		c.log.Warn("workflow: recording read-only completion evidence failed",
			"run", run.ID, "step", step.ID, "verdict", string(e.Verdict), "err", err)
	}
}
