package workflow

import (
	stdctx "context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// incident_repair.go — Checkpoint 8P-E.20.
//
// A repair of AO by AO is the most dangerous thing in this feature, and the
// safest way to build it turned out to be not to build most of it.
//
// # A repair is a workflow run
//
// Everything the brief asks for after "the Repair Agent made a change" already
// exists, is already durable, and is already tested: an independent reviewer
// with cross-provider routing, deterministic verification with the
// infrastructure-vs-code failure split from verify_context.go, bounded fix
// cycles, stale-approval detection in verify_recovery.go, and boot
// reconciliation for all of it. That is what an ordinary workflow run IS.
//
// So a repair is not a second pipeline that resembles a run. It is a run. The
// incident holds a durable link to it and watches its terminal state; the run
// does the work. Concretely, each requirement is satisfied by delegation rather
// than by new code:
//
//	isolated workspace     the project's own isolated-worktree execution mode,
//	                       and a hard refusal if it is direct_branch
//	change -> review       the run's work -> review steps, cross-provider
//	-> verify -> resolved  its verify step, then and only then the incident
//	bounded repair cycles  the run's own maxFixCycles budget
//	infra vs code failure  verify_context.go's classification, which never
//	                       spends a fix cycle on AO's own broken verifier
//	stale approval         verify_recovery.go's fresh-review requirement
//	restart idempotency    the run's own recovery, plus one claim row here
//
// The one thing that is genuinely new is the boundary: who may create such a
// run, against which repository, and on whose authority.
//
// # The diagnostician never becomes the repairer
//
// They are different sessions in different processes with different mandates,
// and now also different KINDS of thing: the diagnostic agent is a read-only
// pane the Advisor launches directly, while the repairer is a worker inside a
// workflow run that AO dispatches through its ordinary routing. There is no
// code path that turns one into the other, because there is no shared object
// between them to mutate.

// ErrSelfRepairUnavailable means AO has no configured repository to repair
// itself in. It is a refusal, not a fallback: guessing which checkout is AO's
// own source is exactly the guess that would let a repair land in the user's
// working tree.
var ErrSelfRepairUnavailable = errors.New("workflow: no self-repair project is configured, so AO will not launch a repair agent")

// ErrSelfRepairUnsafeTarget means the configured self-repair project runs in
// direct-branch mode — the user's own checkout. A repair agent must never be
// pointed at it.
var ErrSelfRepairUnsafeTarget = errors.New("workflow: the self-repair project runs on the user's own checkout (direct_branch), so AO will not launch a repair agent into it")

// incidentRepairPhase records a launched repair and the authority for it.
const incidentRepairPhase = "incident_repair_dispatched"

// incidentRepairIdempotencyKey is the single-flight identity of one repair
// launch, keyed by incident and repair generation. It is what makes a double
// click, a poll and a restart mid-creation converge on one repair run rather
// than three.
func incidentRepairIdempotencyKey(incidentID string, generation int) string {
	return "incident-repair:" + incidentID + ":gen" + strconv.Itoa(generation)
}

// incidentRepairVerification is the deterministic check every self-repair must
// pass. It is fixed rather than proposed by the diagnosing agent, and that is
// deliberate: a repair whose success criteria were written by the same model
// that asked for the repair is not verified, it is asserted.
//
// The commands are the repository's own, the ones a person would run: the build
// and the full test suite. resolveVerifyCommandContext finds the module root,
// so `.` is correct here whatever the repository layout turns out to be.
func incidentRepairVerification() VerificationPlan {
	return VerificationPlan{
		Commands: []VerificationCommandCheck{
			{Command: "go", Args: []string{"build", "./..."}, WorkingDirectory: ".", TimeoutSeconds: 600, RequiredExitCode: 0, RetrySafe: true},
			{Command: "go", Args: []string{"test", "./..."}, WorkingDirectory: ".", TimeoutSeconds: 1800, RequiredExitCode: 0, RetrySafe: true},
		},
	}
}

// launchIncidentRepair creates and starts the repair run for an approved
// class-B diagnosis.
//
// It is reached only from ExecuteIncidentAction, only after
// authorizeIncidentAction has confirmed an explicit human approval, and only
// for a diagnosis whose class is repair_ao. Those three facts are the entire
// licence for what follows.
func (c *Coordinator) launchIncidentRepair(ctx stdctx.Context, run domain.WorkflowRun, inc Incident, approvedBy string) (string, error) {
	target := strings.TrimSpace(c.selfRepairProjectID)
	if target == "" {
		return "", ErrSelfRepairUnavailable
	}
	// A repair must never run in the user's own checkout. This is checked
	// against the project's live configuration rather than assumed from the
	// fact that someone configured it, because the mode can change after the
	// setting was made.
	if c.projectExecutionMode(ctx, target).DirectBranch() {
		return "", ErrSelfRepairUnsafeTarget
	}

	// One repair per incident, hard. A repair that did not work is a new
	// incident with new evidence, not a retry of a guess — and an unbounded
	// repair loop is the single most expensive mistake this feature could make.
	if inc.Repairs >= MaxIncidentRepairs {
		return "", fmt.Errorf("workflow: incident %s has already used its %d approved repair", inc.ID, MaxIncidentRepairs)
	}
	generation := inc.Repairs + 1
	entry, claimed, err := c.claimIncidentRepair(ctx, run, inc, generation)
	if err != nil {
		return "", err
	}
	if !claimed {
		// Another pass already created this generation's repair run. Report the
		// one that exists rather than making a second.
		if existing := c.linkedRepairRunID(ctx, run.ID, inc.ID, generation); existing != "" {
			return existing, nil
		}
		return "", fmt.Errorf("workflow: a repair for incident %s generation %d is already being created", inc.ID, generation)
	}

	created, err := c.CreateRun(ctx, target, buildIncidentRepairObjective(inc), incidentRepairVerification())
	if err != nil {
		c.releaseIncidentRepairClaim(ctx, entry, err)
		return "", err
	}

	// The audit row is written BEFORE the run is started, so a crash between
	// creation and start still leaves the link discoverable and the claim
	// spent — recovery finds a repair run, not an orphan.
	rec := IncidentRecord{
		IncidentID: inc.ID, Signature: inc.Signature, StopReason: inc.StopReason,
		Class: IncidentRepairAO, ApprovedBy: approvedBy,
		DiagnosisAttempt: inc.Diagnoses, RepairGeneration: generation,
		RepairRunID: created.Run.ID, RepairProjectID: target,
		Summary: incidentDiagnosisSummary(inc),
		Action:  &IncidentActionSpec{Kind: IncidentActionRepairAgent},
	}
	if err := c.writeIncidentRow(ctx, run, incidentRepairPhase,
		fmt.Sprintf("incident_repair_dispatched: %s approved repair generation %d; run %s in project %s",
			approvedBy, generation, created.Run.ID, target), rec); err != nil {
		c.releaseIncidentRepairClaim(ctx, entry, err)
		return "", err
	}

	// Checkpoint 8P-E.21: the repair run's own ledger says what it is and where
	// it came from, so the Board can label it as AO's automatic repair rather
	// than mixing it into the project's ordinary work. It lives on the repair
	// run (not the incident's) because it answers a question asked FROM that
	// run: "why does this exist?".
	c.markRepairRunOrigin(ctx, created.Run, inc, run.ID, approvedBy)

	if _, err := c.StartRun(ctx, created.Run.ID); err != nil {
		// The run exists and is linked; starting it again is what recovery and
		// the ordinary continue path already do, so this is reported rather
		// than rolled back — deleting a created run would lose the audit trail
		// of an approval that really happened.
		if c.log != nil {
			c.log.Warn("workflow: incident repair run created but not started",
				"incident", inc.ID, "repairRun", created.Run.ID, "err", err)
		}
	}
	if c.log != nil {
		c.log.Info("workflow: incident repair run launched",
			"incident", inc.ID, "generation", generation, "repairRun", created.Run.ID,
			"project", target, "approvedBy", approvedBy)
	}
	return created.Run.ID, nil
}

// buildIncidentRepairObjective renders the approved diagnosis as the repair
// run's objective.
//
// The repairer receives the diagnosis and nothing else — not the raw context
// pack, not the ledger, not the diagnostic session. It is acting on a
// conclusion that has already been reached and approved by a person, and
// re-litigating it is not its job.
func buildIncidentRepairObjective(inc Incident) string {
	var b strings.Builder
	b.WriteString("Repair an AO defect identified by an approved incident diagnosis.\n\n")
	fmt.Fprintf(&b, "Incident: %s\nStop reason: %s\n\n", inc.ID, inc.StopReason)
	if d := inc.Diagnosis; d != nil {
		fmt.Fprintf(&b, "Summary: %s\n\n", d.Summary)
		if d.WhatHappened != "" {
			fmt.Fprintf(&b, "What happened:\n%s\n\n", d.WhatHappened)
		}
		if d.WhyStopped != "" {
			fmt.Fprintf(&b, "Why AO stopped:\n%s\n\n", d.WhyStopped)
		}
		if len(d.Evidence) > 0 {
			b.WriteString("Evidence the diagnosis relied on:\n")
			for _, e := range d.Evidence {
				b.WriteString("- " + e + "\n")
			}
			b.WriteString("\n")
		}
		if d.Action != nil && d.Action.Reason != "" {
			fmt.Fprintf(&b, "Why a repair was approved: %s\n\n", d.Action.Reason)
		}
	}
	b.WriteString(`Implement the smallest correct fix for the cause named above, and add a regression test that fails without it.

Do not run any destructive git operation: no reset, no stash, no checkout that discards work, no force, no branch deletion, no history rewrite. Do not modify AO's database or data directory. Do not approve your own change — an independent reviewer reads it next and deterministic checks run after that. If you conclude the diagnosis is wrong, stop and say so rather than fixing something else.`)
	return b.String()
}

func incidentDiagnosisSummary(inc Incident) string {
	if inc.Diagnosis == nil {
		return ""
	}
	return inc.Diagnosis.Summary
}

// claimIncidentRepair takes the single-flight slot for one repair generation.
func (c *Coordinator) claimIncidentRepair(ctx stdctx.Context, run domain.WorkflowRun, inc Incident, generation int) (domain.WorkflowOutboxEntry, bool, error) {
	return c.claimIncidentOutboxSlot(ctx, run,
		incidentRepairIdempotencyKey(inc.ID, generation),
		fmt.Sprintf(`{"incidentId":%q,"repairGeneration":%d}`, inc.ID, generation))
}

func (c *Coordinator) releaseIncidentRepairClaim(ctx stdctx.Context, entry domain.WorkflowOutboxEntry, cause error) {
	_, _ = c.store.UpdateWorkflowOutboxStatus(ctx, entry.ID,
		domain.WorkflowOutboxDispatched, domain.WorkflowOutboxFailed, c.clock(), string(domain.WorkflowErrorAgentStartFailed))
	if c.log != nil {
		c.log.Warn("workflow: incident repair launch failed", "entry", entry.ID, "err", cause)
	}
}

// linkedRepairRunID reads back the repair run recorded for one generation.
func (c *Coordinator) linkedRepairRunID(ctx stdctx.Context, runID, incidentID string, generation int) string {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return ""
	}
	for _, cp := range cps {
		if cp.DurablePhase != incidentRepairPhase {
			continue
		}
		rec, ok := decodeIncidentRecord(cp)
		if !ok || rec.IncidentID != incidentID || rec.RepairGeneration != generation {
			continue
		}
		return rec.RepairRunID
	}
	return ""
}

// reconcileIncidentRepair advances an incident whose repair run has finished.
//
// This is where "only then IncidentResolved" is enforced, and it is enforced by
// asking the repair run rather than by trusting anything the repairer said: a
// run reaches `completed` only after its review step approved and its verify
// step passed, so the incident can read one durable fact and know that both
// happened.
//
// Idempotent by state: an incident already terminal is left alone, and the
// resolved/refused row is written once because the transition into it is only
// valid from a non-terminal state.
func (c *Coordinator) reconcileIncidentRepair(ctx stdctx.Context, run domain.WorkflowRun, inc Incident) Incident {
	if inc.State.Terminal() || inc.RepairRunID == "" {
		return inc
	}
	repair, ok, err := c.store.GetWorkflowRun(ctx, inc.RepairRunID)
	if err != nil || !ok {
		return inc
	}
	rec := IncidentRecord{
		IncidentID: inc.ID, Signature: inc.Signature, StopReason: inc.StopReason,
		Class: IncidentRepairAO, RepairRunID: inc.RepairRunID,
		RepairGeneration: inc.Repairs, ApprovedBy: inc.ApprovedBy,
	}
	switch repair.State {
	case domain.WorkflowRunCompleted:
		rec.Outcome = "repair verified"
		rec.FinalSHA = c.repairFinalSHA(ctx, inc.RepairRunID)
		rec.ReviewerHarness = c.repairReviewerHarness(ctx, inc.RepairRunID)
		rec.VerifyResult = c.repairVerifyResult(ctx, inc.RepairRunID)
		_ = c.writeIncidentRow(ctx, run, incidentResolvedPhase,
			fmt.Sprintf("incident_resolved: repair run %s was reviewed by %s, verified, and landed at %s",
				inc.RepairRunID, rec.ReviewerHarness, rec.FinalSHA), rec)
		inc.State = IncidentResolved
	case domain.WorkflowRunFailed, domain.WorkflowRunCancelled:
		rec.Outcome = "repair " + string(repair.State)
		rec.Note = "the approved repair did not produce a reviewed, verified change"
		_ = c.writeIncidentRow(ctx, run, incidentRefusedPhase,
			fmt.Sprintf("incident_refused: repair run %s ended %s without a verified change",
				inc.RepairRunID, repair.State), rec)
		inc.State = IncidentRefused
	}
	return inc
}

// repairFinalSHA reads the commit the repair actually landed at, from the
// repair run's own durable record.
func (c *Coordinator) repairFinalSHA(ctx stdctx.Context, repairRunID string) string {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, repairRunID)
	if err != nil {
		return ""
	}
	sha := ""
	for _, cp := range cps {
		if cp.HeadSHA != "" {
			sha = cp.HeadSHA
		}
		if cp.DurablePhase == autonomousLocalCommitPhase {
			if _, after, found := strings.Cut(cp.NextAction, "local_commit_created: "); found {
				sha = strings.Fields(after)[0]
			}
		}
	}
	return sha
}

// repairReviewerHarness reads which independent reviewer approved the repair.
//
// It reads the review_run rather than the step's workflow_attempts: a review is
// carried out by a reviewer run, and the attempt rows on a review step do not
// carry the reviewer's harness. A real E2E recorded an empty reviewer for a
// repair that had in fact been approved by codex, which is precisely the audit
// field that must not be guessable.
func (c *Coordinator) repairReviewerHarness(ctx stdctx.Context, repairRunID string) string {
	if c.reviewRuns == nil {
		return ""
	}
	steps, err := c.store.ListWorkflowSteps(ctx, repairRunID)
	if err != nil {
		return ""
	}
	for _, s := range steps {
		if s.Kind != domain.WorkflowStepReview || s.ReviewRunID == nil {
			continue
		}
		rr, ok, rerr := c.reviewRuns.GetReviewRun(ctx, *s.ReviewRunID)
		if rerr != nil || !ok {
			return ""
		}
		return string(rr.Harness)
	}
	return ""
}

// repairVerifyResult reads the deterministic verification's own verdict.
func (c *Coordinator) repairVerifyResult(ctx stdctx.Context, repairRunID string) string {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, repairRunID)
	if err != nil {
		return ""
	}
	result := ""
	for _, cp := range cps {
		if cp.DurablePhase == "verify_result" {
			result = strings.TrimSpace(cp.NextAction)
		}
	}
	return result
}

// incidentRepairOriginPhase marks a run as an Incident Advisor repair.
const incidentRepairOriginPhase = "incident_repair_origin"

// IncidentRepairOrigin is a repair run's provenance, derived at read time.
type IncidentRepairOrigin struct {
	// Origin is the constant "incident_repair", so a reader can branch on one
	// value rather than on the presence of fields.
	Origin string
	// IncidentID and SourceRunID are what this repair is for and where it came
	// from, so the Board can link back rather than stranding the operator in an
	// unexplained run.
	IncidentID  string
	SourceRunID string
	ApprovedBy  string
}

// markRepairRunOrigin records the provenance on the repair run itself.
// Best-effort: a repair that ran and was reviewed is not undone by a missing
// label, and the incident's own ledger still holds the link.
func (c *Coordinator) markRepairRunOrigin(ctx stdctx.Context, repairRun domain.WorkflowRun, inc Incident, sourceRunID, approvedBy string) {
	rec := IncidentRecord{
		IncidentID: inc.ID, StopReason: inc.StopReason,
		RepairRunID: repairRun.ID, ApprovedBy: approvedBy,
		Note: sourceRunID,
	}
	payload, err := marshalIncidentRecord(rec)
	if err != nil {
		return
	}
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:            "wfc-" + c.newID(),
		WorkflowRunID: repairRun.ID,
		ProjectID:     repairRun.ProjectID,
		NextAction: fmt.Sprintf("incident_repair_origin: automatic AO repair for incident %s (from run %s), approved by %s",
			inc.ID, sourceRunID, approvedBy),
		DurablePhase:   incidentRepairOriginPhase,
		PayloadVersion: "v1",
		RetryState:     payload,
		CreatedAt:      c.clock(),
	}); err != nil && c.log != nil {
		c.log.Warn("workflow: could not label a repair run's origin", "repairRun", repairRun.ID, "err", err)
	}
}

// RepairOriginFor reports whether a run is an Incident Advisor repair, and what
// it is a repair OF. Used by the Board and the run view to label and group it
// away from the project's ordinary work.
func (c *Coordinator) RepairOriginFor(ctx stdctx.Context, runID string) (IncidentRepairOrigin, bool) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return IncidentRepairOrigin{}, false
	}
	for _, cp := range cps {
		if cp.DurablePhase != incidentRepairOriginPhase {
			continue
		}
		rec, ok := decodeIncidentRecord(cp)
		if !ok {
			continue
		}
		return IncidentRepairOrigin{
			Origin: "incident_repair", IncidentID: rec.IncidentID,
			SourceRunID: rec.Note, ApprovedBy: rec.ApprovedBy,
		}, true
	}
	return IncidentRepairOrigin{}, false
}
