package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// execution_authority_retirement.go — a stop that is a person's is not a licence
// to keep holding the machine.
//
// THE REGRESSION (wf-f5025a7c, repair generation 2 of wf-724a1e97). The
// quiescence proof did its job: it refused to fold a repair that still owned
// execution authority, and refusing was right. What it exposed is that the
// authority was FOSSIL — every obligation that could have used it had finished
// hours earlier, and nothing existed to close it:
//
//	capacity  reviewer:held   cap:reviewer:...:wfs-79a90a65:cycle1:codex:gen1
//	          for review run 3bf56007, which CONCLUDED (changes_requested).
//	outbox    spawn_worker_session:dispatched
//	          workflow-repair:wf-f5025a7c:475174af:gen1 — a repair LAUNCH claim
//	          whose repair run wf-c4c84f52 demonstrably exists.
//	session   agent-orchestrator-54 idle, is_terminated=0, instance $58.
//
// So the run read as "still able to execute" while being, in fact, finished —
// and the origin stayed shut behind it. The answer is not to teach the proof to
// look away from those rows. It is to close them, on proof, where they are
// closed for every other run.
//
// # What "finished" is allowed to mean
//
// Each authority has exactly one durable fact that retires it, and none of them
// is "the run is parked":
//
//	A CAPACITY CLAIM is retired by the REVIEW IT PAID FOR having concluded. A
//	  reviewer slot is released today only when its step goes terminal or a
//	  newer cycle supersedes it (capacity_scheduler.go) — and a review step
//	  resting at `waiting` after changes_requested is neither, so cycle 1's
//	  reviewer keeps its slot for the life of the run. The review run's own
//	  durable verdict is the missing fact: the reviewer submitted, so nothing is
//	  running, so the slot pays for nothing. A review run still `running` is a
//	  live execution and is never touched.
//	AN OUTBOX CLAIM is retired by the EFFECT IT CLAIMED having durably happened.
//	  claimIncidentOutboxSlot moves its row to `dispatched` and, on the success
//	  path, nothing ever moves it again: it is a permanent single-flight latch
//	  that happens to be spelled with a status meaning "a launch may be in
//	  flight". It is acknowledged here if and ONLY if the ledger holds the
//	  record its launch writes after succeeding. Absent that record the daemon
//	  may have died between the claim and the launch, which is genuinely
//	  ambiguous, and the row is left exactly as it is. Nothing is ever
//	  acknowledged to tidy a row.
//	A SESSION is retired by nothing here at all, deliberately. See below.
//
// # Preserving evidence is not preserving authority
//
// A parked run keeps its runtime on purpose: a person is about to look at it,
// and terminal_runtime.go's policy for needs_attention is to preserve. That is
// EVIDENCE. It is a different thing from AO still being able to write through
// that session, which is what quiescence asks about — and that question is
// answered by the rows above, not by the process: with no step ready or
// running, no capacity claim, and no actionable dispatch, AO has no path to
// deliver anything into it. So this file retires authority and leaves the
// runtime alone. It never terminates a session, never destroys a runtime, and
// never addresses one by name.
//
// What it does record is the exact incarnation it observed — session, runtime
// handle, InstanceID, owner token, launch id — so the claim "this session's
// AO-side authority is retired" is durably about ONE incarnation and cannot be
// inherited by a stranger that later takes the same name. A record whose
// InstanceID no longer matches the session's is not written.

// executionAuthorityRetiredPhase is the durable summary of one retirement pass.
//
// It is written LAST and it is not a gate: every individual retirement is an
// idempotent compare-and-set that explains itself in its own row
// (capacity_claims.release_reason, workflow_outbox.status), so a crash before
// this record loses an audit line and nothing else — the next pass re-derives
// the same answers and finds the work already done.
const executionAuthorityRetiredPhase = "execution_authority_retired"

// retiredAuthority is one thing this pass closed, or declined to.
type retiredAuthority struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Detail string `json:"detail"`
}

// executionAuthorityRetirement is the durable record's payload.
type executionAuthorityRetirement struct {
	Retired  []retiredAuthority `json:"retired,omitempty"`
	Refused  []retiredAuthority `json:"refused,omitempty"`
	Observed []retiredAuthority `json:"observed,omitempty"`
}

// retireFinishedExecutionAuthorities closes the execution authority a parked run
// can no longer use.
//
// It acts only on a run resting exactly where a person owns it — parked, on a
// human-owned stop AO is not itself remediating, with no wake scheduled and
// nothing ready or running. Those are the same facts clauses 1-3 of the
// quiescence proof establish, and they are re-derived here rather than passed
// in, so the two can never disagree about whether a run is at rest.
//
// Every refusal is recorded on the durable summary rather than silently
// skipped: "AO could not prove this authority is finished" is the answer an
// operator needs when a fold does not happen. It returns nothing — what it did
// is readable from the rows it changed and from that summary, and a return
// value no caller can act on would be a second, divergent account of the same
// facts.
func (c *Coordinator) retireFinishedExecutionAuthorities(ctx stdctx.Context, run domain.WorkflowRun) {
	var out executionAuthorityRetirement
	if run.State != domain.WorkflowRunNeedsAttention {
		return
	}
	_, disp, known := c.stopReason(ctx, run)
	if !known || disp.HumanAction == "" || disp.SelfRemediable {
		return
	}
	if c.wakeScheduler != nil {
		next, err := c.wakeScheduler.NextForRun(ctx, domain.WorkflowRunID(run.ID))
		if err != nil || next != nil {
			return
		}
	}
	steps, err := c.store.ListWorkflowSteps(ctx, run.ID)
	if err != nil {
		return
	}
	for _, step := range steps {
		if step.State == domain.WorkflowStepReady || step.State == domain.WorkflowStepRunning {
			// Something of this run's is still authorized to execute. Its
			// authority is not fossil, and none of it may be closed.
			return
		}
	}

	c.retireConcludedReviewerClaims(ctx, run, steps, &out)
	c.retireProvenLaunchClaims(ctx, run, &out)
	// And the dispatch a finished review cycle leaves behind. A trigger_review
	// whose step has ENDED cannot launch a reviewer by any path — dispatch
	// refuses a terminal step on its first line — so leaving it pending means
	// leaving a fossil that reads as a queued launch, which is exactly what
	// held wf-c4c84f52's branch. See late_verdict_disposition.go.
	c.retireUnlaunchableReviewDispatches(ctx, run, steps, &out)
	c.observeRetiredSessionAuthority(ctx, run, steps, &out)

	if len(out.Retired) == 0 {
		return
	}
	payload, merr := json.Marshal(out)
	if merr != nil {
		return
	}
	summary := make([]string, 0, len(out.Retired))
	for _, r := range out.Retired {
		summary = append(summary, r.Kind+" "+r.ID)
	}
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:            "wfc-" + c.newID(),
		WorkflowRunID: run.ID,
		ProjectID:     run.ProjectID,
		DurablePhase:  executionAuthorityRetiredPhase,
		NextAction: fmt.Sprintf(
			"execution_authority_retired: this run is stopped for a person and %s could no longer produce work, so %s been closed; no runtime was ended and no session was terminated",
			plural(len(out.Retired), "an authority", "authorities"), plural(len(out.Retired), "it has", "they have")),
		PayloadVersion: "v1",
		RetryState:     string(payload),
		CreatedAt:      c.clock(),
	}); err != nil && c.log != nil {
		c.log.Warn("workflow: recording an execution-authority retirement failed", "run", run.ID, "err", err)
	}
	if c.log != nil {
		c.log.Info("workflow: retired finished execution authority",
			"run", run.ID, "retired", strings.Join(summary, ", "))
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// retireConcludedReviewerClaims releases a reviewer slot whose review has
// concluded.
//
// The proof is the review run's own durable verdict, read through the step that
// points at it. Three refusals, each of them a live-execution guard rather than
// a formality:
//
//   - the claim names a step this run does not have, or one that is not a review
//     step: nothing here can say what it is paying for;
//   - the step's review run cannot be read, or is still `running`: a reviewer
//     that has not concluded IS a live execution, and its slot is real;
//   - the claim's generation is not the review cycle the step is on: an older
//     one is capacity_scheduler.go's supersession case, a newer one describes a
//     dispatch this pass cannot see, and neither is this rule's business.
//
// The release is generation-conditioned by the store's own CAS, so a claim that
// moved on between the read and the write is refused rather than clobbered.
func (c *Coordinator) retireConcludedReviewerClaims(
	ctx stdctx.Context, run domain.WorkflowRun, steps []domain.WorkflowStep, out *executionAuthorityRetirement,
) {
	if c.capacity == nil || c.reviewRuns == nil {
		return
	}
	claims, err := c.capacity.ListCapacityClaimsForRun(ctx, run.ID)
	if err != nil {
		return
	}
	byID := map[string]domain.WorkflowStep{}
	for _, step := range steps {
		byID[step.ID] = step
	}
	for _, claim := range claims {
		if claim.State != domain.CapacityClaimHeld || claim.Kind != domain.ExecutionKindReviewer {
			continue
		}
		refuse := func(detail string) {
			out.Refused = append(out.Refused, retiredAuthority{
				Kind: "capacity_claim", ID: claim.DispatchKey, Detail: detail})
		}
		step, ok := byID[claim.WorkflowStepID]
		if !ok || step.Kind != domain.WorkflowStepReview {
			refuse("the claim does not name a review step of this run, so AO cannot say what it is paying for")
			continue
		}
		if step.ReviewRunID == nil {
			refuse("the review step names no review run, so AO cannot prove the reviewer concluded")
			continue
		}
		reviewRun, found, rerr := c.reviewRuns.GetReviewRun(ctx, *step.ReviewRunID)
		if rerr != nil || !found {
			refuse(fmt.Sprintf("AO could not read review run %s to prove the reviewer concluded", *step.ReviewRunID))
			continue
		}
		if !reviewRunConcluded(reviewRun) {
			refuse(fmt.Sprintf("review run %s is %s and has recorded no verdict, so a reviewer may still be running",
				reviewRun.ID, reviewRun.Status))
			continue
		}
		cycle, cerr := c.completedReviewCycles(ctx, reviewRun.SessionID, reviewRun.Harness)
		if cerr != nil {
			refuse("AO could not derive this review step's current cycle, so it cannot fence the release")
			continue
		}
		if claim.LifecycleGeneration != int64(cycle) {
			refuse(fmt.Sprintf("the claim is for review cycle %d and this step is on cycle %d",
				claim.LifecycleGeneration, cycle))
			continue
		}
		released, relErr := c.capacity.ReleaseCapacityClaim(ctx, claim.DispatchKey, claim.LifecycleGeneration,
			fmt.Sprintf("the review this slot paid for concluded (%s, verdict %s) and this run is stopped for a person",
				reviewRun.ID, reviewRun.EffectiveVerdict()), c.clock())
		switch {
		case relErr != nil:
			refuse("releasing the slot failed: " + relErr.Error())
		case released:
			out.Retired = append(out.Retired, retiredAuthority{
				Kind: "capacity_claim", ID: claim.DispatchKey,
				Detail: fmt.Sprintf("reviewer slot for cycle %d, released because review run %s concluded",
					claim.LifecycleGeneration, reviewRun.ID)})
		}
	}
}

// reviewRunConcluded reports whether a review run can no longer be a live
// reviewer: it recorded a verdict, or it ended without one.
//
// `running` with no verdict is the one answer that means "a reviewer may be
// working right now", and it is the one this returns false for.
func reviewRunConcluded(r domain.ReviewRun) bool {
	if r.HasDurableVerdict() || r.LateVerdict.Valid() {
		return true
	}
	switch r.Status {
	case domain.ReviewRunComplete, domain.ReviewRunDelivered,
		domain.ReviewRunCancelled, domain.ReviewRunFailed:
		return true
	default:
		return false
	}
}

// launchClaimKeyPrefixes are the idempotency-key prefixes of the single-flight
// LAUNCH claims — rows whose `dispatched` status is a latch rather than a
// dispatch in progress. Only these are eligible for the acknowledgement below;
// an ordinary worker/reviewer/fix dispatch left at `dispatched` is a real crash
// boundary that its own recovery owns, and this rule must never touch one.
var launchClaimKeyPrefixes = []string{"workflow-repair:", "incident-repair:", "incident-diagnose:"}

// retireProvenLaunchClaims acknowledges a launch claim whose effect is on the
// ledger.
//
// The proof for a repair launch is the repair intent record: it is written after
// CreateTaskRun returns, it carries the same generation and evidence digest the
// key does, and it names the run that was created. If that run exists, the
// launch this row claimed demonstrably happened, and the row is describing a
// finished event as an in-flight one.
//
// If the record is absent the daemon may have died between the claim and the
// launch — the task run might exist with nothing pointing at it — so the row is
// left exactly where it is. FAIL CLOSED, and never `failed`: marking a claim
// failed frees its key for a second launch, which is the one outcome worse than
// a stale row.
func (c *Coordinator) retireProvenLaunchClaims(
	ctx stdctx.Context, run domain.WorkflowRun, out *executionAuthorityRetirement,
) {
	entries, err := c.store.ListWorkflowOutboxByRun(ctx, run.ID)
	if err != nil {
		return
	}
	intents := c.repairIntents(ctx, run.ID)
	for _, entry := range entries {
		if entry.Status != domain.WorkflowOutboxDispatched || !isLaunchClaimKey(entry.IdempotencyKey) {
			continue
		}
		refuse := func(detail string) {
			out.Refused = append(out.Refused, retiredAuthority{
				Kind: "outbox_claim", ID: entry.IdempotencyKey, Detail: detail})
		}
		generation, ok := launchClaimGeneration(entry.IdempotencyKey)
		if !ok {
			refuse("the claim key carries no generation, so AO cannot match it to the launch it claimed")
			continue
		}
		var proof string
		if strings.HasPrefix(entry.IdempotencyKey, incidentDiagnoseClaimPrefix) {
			// A DIAGNOSIS launch's proof is the incident ledger's own started
			// row: it is written after the launch returns and it names the agent
			// session the launch produced. The REQUEST row is deliberately not
			// enough — it is written before the launch, so a daemon that died
			// between the two would leave one behind with nothing having run.
			proof = c.diagnosisLaunchProof(ctx, run.ID, entry.IdempotencyKey, generation)
		} else {
			for _, intent := range intents {
				if intent.Generation != generation || intent.RepairRunID == "" {
					continue
				}
				if _, found, gerr := c.store.GetWorkflowRun(ctx, intent.RepairRunID); gerr == nil && found {
					proof = intent.RepairRunID
				}
			}
		}
		if proof == "" {
			refuse(fmt.Sprintf(
				"no durable record shows generation %d's launch completed, so AO cannot tell a finished launch from one interrupted mid-flight",
				generation))
			continue
		}
		acked, aerr := c.store.UpdateWorkflowOutboxStatus(ctx, entry.ID,
			domain.WorkflowOutboxDispatched, domain.WorkflowOutboxAcknowledged, c.clock(), "")
		switch {
		case aerr != nil:
			refuse("acknowledging the claim failed: " + aerr.Error())
		case acked:
			out.Retired = append(out.Retired, retiredAuthority{
				Kind: "outbox_claim", ID: entry.IdempotencyKey,
				Detail: fmt.Sprintf("launch generation %d completed as run %s; the claim is acknowledged and its key stays taken",
					generation, proof)})
		}
	}
}

func isLaunchClaimKey(key string) bool {
	for _, prefix := range launchClaimKeyPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// launchClaimGeneration reads the `:genN` suffix a launch claim key ends with.
func launchClaimGeneration(key string) (int, bool) {
	idx := strings.LastIndex(key, ":gen")
	if idx < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(key[idx+len(":gen"):])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// observeRetiredSessionAuthority records, and does not act.
//
// The distinction this file exists to keep straight: a parked run's runtime is
// PRESERVED on purpose (terminal_runtime.go), because a person is about to look
// at it. That is evidence. Whether AO can still WRITE through that session is a
// different question, and after the retirements above it is answered by the rows
// rather than by the process — no step ready or running, no capacity claim, no
// actionable dispatch, therefore no path by which AO delivers anything into it.
//
// So this writes down which incarnation that statement is about — handle,
// InstanceID, owner token, launch id — and touches nothing. Recording the
// InstanceID is what stops the statement from being inherited by a stranger that
// later takes the same name; a session whose recorded incarnation no longer
// matches is reported as observed-but-unmatched rather than retired.
func (c *Coordinator) observeRetiredSessionAuthority(
	ctx stdctx.Context, run domain.WorkflowRun, steps []domain.WorkflowStep, out *executionAuthorityRetirement,
) {
	if c.sessionFacts == nil {
		return
	}
	for _, sessionID := range c.runtimeOwningSessionsForRun(ctx, run, steps) {
		rec, found, err := c.sessionFacts.GetSession(ctx, sessionID)
		if err != nil || !found {
			continue
		}
		detail := fmt.Sprintf(
			"handle %s, instance %s, owner %s, launch %s; runtime preserved for inspection, AO holds no write path to it",
			orNone(rec.Metadata.RuntimeHandleID), orNone(rec.Metadata.RuntimeInstanceID),
			orNone(rec.Metadata.RuntimeOwnerToken), orNone(rec.Metadata.RuntimeLaunchID))
		if rec.IsTerminated {
			detail = "session already terminated"
		} else if rec.Metadata.RuntimeInstanceID == "" {
			// No incarnation recorded, so nothing here can be said about ONE
			// runtime. Reported, never asserted.
			out.Observed = append(out.Observed, retiredAuthority{
				Kind: "session", ID: string(sessionID),
				Detail: "no runtime incarnation is recorded for this session, so AO makes no claim about which runtime it names"})
			continue
		}
		out.Observed = append(out.Observed, retiredAuthority{
			Kind: "session", ID: string(sessionID), Detail: detail})
	}
}

// incidentDiagnoseClaimPrefix is the single-flight key a diagnostic launch
// takes: `incident-diagnose:<incidentId>:gen<N>`.
const incidentDiagnoseClaimPrefix = "incident-diagnose:"

// diagnosisLaunchProof returns the agent session a diagnostic launch produced,
// or "" when AO cannot prove one was produced.
//
// The proof is incident_diagnosis_started, which recordIncidentDiagnosisStarted
// writes after the agent result comes back and which carries the session id in
// its payload. All three of the incident, the attempt and a non-empty session
// must match the claim, so a started row belonging to a different incident or a
// different attempt proves nothing about this claim.
func (c *Coordinator) diagnosisLaunchProof(ctx stdctx.Context, runID, key string, generation int) string {
	incidentID := incidentIDFromDiagnoseClaim(key)
	if incidentID == "" {
		return ""
	}
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return ""
	}
	for _, cp := range cps {
		if cp.DurablePhase != incidentDiagnosisStartedPhase {
			continue
		}
		var rec IncidentRecord
		if json.Unmarshal([]byte(cp.RetryState), &rec) != nil {
			continue
		}
		if rec.IncidentID == incidentID && rec.DiagnosisAttempt == generation && rec.AgentSessionID != "" {
			return rec.AgentSessionID
		}
	}
	return ""
}

// incidentIDFromDiagnoseClaim reads the incident id out of the claim key. An
// unparseable key yields "", which proves nothing and therefore retires nothing.
func incidentIDFromDiagnoseClaim(key string) string {
	rest := strings.TrimPrefix(key, incidentDiagnoseClaimPrefix)
	idx := strings.LastIndex(rest, ":gen")
	if idx <= 0 {
		return ""
	}
	return rest[:idx]
}
