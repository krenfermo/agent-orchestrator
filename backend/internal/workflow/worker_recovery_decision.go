package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// P9 — the worker recovery decision.
//
// Every path that acts on a worker WITHOUT the launching process's memory — boot
// reconciliation, adoption of a dispatched command after a restart, a human
// reopen — asks one question: may AO treat THIS session as the worker of THIS
// dispatch? This file answers it once, as a pure function over durable facts and
// one runtime read-back, so the three callers cannot drift into three answers.
//
//	RECOVER WHEN PROVABLY SAFE. FAIL CLOSED WHEN OWNERSHIP CANNOT BE PROVEN.
//	NEVER GUESS OWNERSHIP. NEVER CREATE TWO OWNERS FOR THE SAME GENERATION.

// WorkerRuntimeOwnership is the optional port that reads a worker session's
// runtime identity back and classifies it (workerownership.Classify behind the
// session manager in the daemon). Nil keeps the pre-P9 behaviour, which is what
// every unit fixture without a runtime wires; the daemon always wires it.
type WorkerRuntimeOwnership interface {
	ObserveWorkerRuntime(ctx stdctx.Context, id domain.SessionID) domain.WorkerRuntimeObservation
}

// WorkerRecoveryAction is the closed set of things recovery may do.
type WorkerRecoveryAction string

// The actions recovery may take.
const (
	WorkerRecoveryAdopt      WorkerRecoveryAction = "adopt"
	WorkerRecoveryRelaunch   WorkerRecoveryAction = "relaunch"
	WorkerRecoveryWait       WorkerRecoveryAction = "wait"
	WorkerRecoveryNoop       WorkerRecoveryAction = "noop"
	WorkerRecoveryFailClosed WorkerRecoveryAction = "fail_closed"
)

// WorkerRecoveryReason is the closed reason code a decision carries. Decisions
// are never taken on free text; Detail is for people.
type WorkerRecoveryReason string

// The closed reason codes.
const (
	WorkerReasonMatchingRuntime         WorkerRecoveryReason = "matching_runtime"
	WorkerReasonRuntimeExited           WorkerRecoveryReason = "runtime_exited"
	WorkerReasonRuntimeMissing          WorkerRecoveryReason = "runtime_missing"
	WorkerReasonInstanceMismatch        WorkerRecoveryReason = "instance_mismatch"
	WorkerReasonOwnerMismatch           WorkerRecoveryReason = "owner_mismatch"
	WorkerReasonInstallationMismatch    WorkerRecoveryReason = "installation_mismatch"
	WorkerReasonLaunchMismatch          WorkerRecoveryReason = "launch_mismatch"
	WorkerReasonGenerationMismatch      WorkerRecoveryReason = "generation_mismatch"
	WorkerReasonWorktreeMismatch        WorkerRecoveryReason = "worktree_mismatch"
	WorkerReasonBranchMismatch          WorkerRecoveryReason = "branch_mismatch"
	WorkerReasonLegacyProvenanceMissing WorkerRecoveryReason = "legacy_provenance_missing"
	WorkerReasonRuntimeUnsupported      WorkerRecoveryReason = "runtime_unsupported"
	WorkerReasonRuntimeUnavailable      WorkerRecoveryReason = "runtime_unavailable"
	WorkerReasonTerminalRun             WorkerRecoveryReason = "terminal_run"
	WorkerReasonRuntimeProofNotWired    WorkerRecoveryReason = "runtime_proof_not_wired"
	WorkerReasonSettleWindow            WorkerRecoveryReason = "settle_window"
)

// WorkerRecoveryDecision is one decision and why.
type WorkerRecoveryDecision struct {
	Action WorkerRecoveryAction
	Reason WorkerRecoveryReason
	Detail string
}

// workerAdoptionFacts is everything the decision reads. All of it is durable or
// a single runtime read-back taken for this decision.
type workerAdoptionFacts struct {
	RunTerminal bool
	// Runtime is nil when no WorkerRuntimeOwnership port is wired.
	Runtime *domain.WorkerRuntimeObservation

	SessionID        domain.SessionID
	SessionLaunchID  string
	SessionWorktree  string
	SessionBranch    string
	SessionCreatedAt time.Time

	// RecordedLaunchID is the launch AO's own dispatch evidence names for this
	// step ("" = none was recorded, e.g. a crash between Spawn and the
	// confirmation).
	RecordedLaunchID string
	// RecordedWorktree / RecordedBranch come from the same evidence or from the
	// run's frozen placement; "" means not recorded, which never contradicts.
	RecordedWorktree string
	RecordedBranch   string
	// ClaimedAt is when the CURRENT generation claimed the dispatch. It fences a
	// session with no recorded launch: a session created before the claim that
	// is now in force belongs to an earlier generation.
	ClaimedAt time.Time
	// UnreadableFor is how long the CURRENT episode of failed runtime reads for
	// this session has lasted, measured on a monotonic clock in THIS process;
	// UnreadableKnown is false when no episode is open. The grace is measured
	// from it -- never from the age of a dispatch record (hours old after a
	// reboot), never across an earlier, closed episode, and never on the wall
	// clock a laptop sleep or an NTP step can jump.
	UnreadableFor   time.Duration
	UnreadableKnown bool
	// RecordedSessionID is the session the CURRENT generation's launch evidence
	// names ("" when it names none). With RecordedLaunchID it is what separates
	// AO restoring this generation's own session under a new launch from a
	// session of another launch entirely.
	RecordedSessionID string
	// SessionTerminated is the row's own verdict: AO recorded this session's
	// teardown. A terminated session is never adopted as a worker.
	SessionTerminated bool
}

// workerRuntimeUnreadableGrace is how long recovery waits out a runtime it
// cannot read at all before failing closed. A single failed tmux read must not
// park a live worker; a runtime that stays unreadable must not become a run that
// silently waits forever. Past the grace AO still adopts nothing and launches
// nothing -- it names the stop.
const workerRuntimeUnreadableGrace = 15 * time.Minute

// generationFenceTolerance absorbs the clock distance between the claim write
// and the session row creation inside ONE dispatch (two components stamping
// time.Now() milliseconds apart). It is far below the 30 s launch-retry floor,
// so no session of an earlier generation can hide inside it.
const generationFenceTolerance = 2 * time.Second

// decideWorkerAdoption is the whole decision. Order matters: terminal first
// (nothing is recovered into a closed run), then the runtime proof (a
// contradiction outranks any durable agreement), then the durable fences
// (launch, generation, workspace).
func decideWorkerAdoption(f workerAdoptionFacts) WorkerRecoveryDecision {
	if f.RunTerminal {
		return WorkerRecoveryDecision{Action: WorkerRecoveryNoop, Reason: WorkerReasonTerminalRun,
			Detail: "the run is terminal; nothing is recovered into it"}
	}
	if f.SessionTerminated {
		return WorkerRecoveryDecision{Action: WorkerRecoveryRelaunch, Reason: WorkerReasonRuntimeMissing,
			Detail: fmt.Sprintf("session %s is recorded as terminated; it is not a worker to adopt", f.SessionID)}
	}
	proofReason := WorkerReasonRuntimeProofNotWired
	if f.Runtime != nil {
		obs := *f.Runtime
		switch obs.Proof {
		case domain.WorkerRuntimeOwned:
			proofReason = WorkerReasonMatchingRuntime
		case domain.WorkerRuntimeOwnedExited:
			// Ownership is proven; only liveness is gone. Adopting it is binding
			// the SAME launch, never a second owner — what it produced is work
			// observation's question.
			proofReason = WorkerReasonRuntimeExited
		case domain.WorkerRuntimeAbsent:
			return WorkerRecoveryDecision{Action: WorkerRecoveryRelaunch, Reason: WorkerReasonRuntimeMissing, Detail: obs.Detail}
		case domain.WorkerRuntimeInstanceMismatch:
			return failClosed(WorkerReasonInstanceMismatch, obs.Detail)
		case domain.WorkerRuntimeOwnerMismatch:
			return failClosed(WorkerReasonOwnerMismatch, obs.Detail)
		case domain.WorkerRuntimeInstallationMismatch:
			return failClosed(WorkerReasonInstallationMismatch, obs.Detail)
		case domain.WorkerRuntimeProvenanceMissing:
			return failClosed(WorkerReasonLegacyProvenanceMissing, obs.Detail)
		case domain.WorkerRuntimeUnsupported:
			return failClosed(WorkerReasonRuntimeUnsupported, obs.Detail)
		default: // unavailable, or anything outside the vocabulary
			// A failed read concludes nothing: not adopted, not relaunched. The
			// next pass asks again -- for a bounded time, after which the wait
			// is the stop.
			detail := orValue(obs.Detail, "the runtime could not be read")
			if f.UnreadableKnown && f.UnreadableFor > workerRuntimeUnreadableGrace {
				return failClosed(WorkerReasonRuntimeUnavailable, fmt.Sprintf(
					"the runtime has stayed unreadable for more than %s: %s", workerRuntimeUnreadableGrace, detail))
			}
			return WorkerRecoveryDecision{Action: WorkerRecoveryWait, Reason: WorkerReasonRuntimeUnavailable, Detail: detail}
		}
	}

	// The generation fence applies whenever the runtime was read, and whether or
	// not any launch was recorded: a session created before the claim now in
	// force belongs to an EARLIER generation, whatever an older launch record
	// says. A claim whose instant AO cannot name cannot fence anything, and an
	// unfenceable adoption is not a provable one.
	if f.Runtime != nil {
		switch {
		case f.ClaimedAt.IsZero():
			return failClosed(WorkerReasonGenerationMismatch,
				"the dispatch generation now in force has no recorded claim instant, so no session can be proven to belong to it")
		case f.SessionCreatedAt.IsZero():
			return failClosed(WorkerReasonGenerationMismatch, fmt.Sprintf(
				"session %s has no recorded creation instant, so it cannot be fenced to the generation now in force", f.SessionID))
		case f.SessionCreatedAt.Before(f.ClaimedAt.Add(-generationFenceTolerance)):
			return failClosed(WorkerReasonGenerationMismatch, fmt.Sprintf(
				"session %s was created at %s, before the dispatch generation now in force claimed this step at %s",
				f.SessionID, f.SessionCreatedAt.UTC().Format(time.RFC3339), f.ClaimedAt.UTC().Format(time.RFC3339)))
		}
	}
	recorded := strings.TrimSpace(f.RecordedLaunchID)
	session := strings.TrimSpace(f.SessionLaunchID)
	if recorded != "" && session != "" && recorded != session {
		// AO itself relaunches a session under a NEW launch (restore, resume):
		// the same session, a fresh launch id, a fresh token. That is provably
		// this generation's worker when the runtime proves the session's current
		// launch AND this generation's own evidence names this very session.
		// Anything else is a launch nobody here recorded.
		sameSessionRestored := f.Runtime != nil && f.Runtime.Proof.OwnershipProven() &&
			f.RecordedSessionID != "" && f.RecordedSessionID == string(f.SessionID)
		if !sameSessionRestored {
			return failClosed(WorkerReasonLaunchMismatch, fmt.Sprintf(
				"session %s runs launch %s, but the launch AO recorded for this dispatch was %s",
				f.SessionID, shortFingerprint(session), shortFingerprint(recorded)))
		}
	}
	if f.RecordedWorktree != "" && f.SessionWorktree != "" && !sameWorkspacePath(f.RecordedWorktree, f.SessionWorktree) {
		return failClosed(WorkerReasonWorktreeMismatch, fmt.Sprintf(
			"session %s works in %s, but this dispatch's worktree is %s", f.SessionID, f.SessionWorktree, f.RecordedWorktree))
	}
	if f.RecordedBranch != "" && f.SessionBranch != "" && f.RecordedBranch != f.SessionBranch {
		return failClosed(WorkerReasonBranchMismatch, fmt.Sprintf(
			"session %s is on branch %s, but this dispatch's branch is %s", f.SessionID, f.SessionBranch, f.RecordedBranch))
	}
	detail := "the session's runtime is this dispatch's launch"
	if f.Runtime != nil {
		detail = f.Runtime.Detail
	}
	return WorkerRecoveryDecision{Action: WorkerRecoveryAdopt, Reason: proofReason, Detail: detail}
}

func failClosed(reason WorkerRecoveryReason, detail string) WorkerRecoveryDecision {
	return WorkerRecoveryDecision{Action: WorkerRecoveryFailClosed, Reason: reason, Detail: detail}
}

func sameWorkspacePath(a, b string) bool {
	return strings.TrimRight(strings.TrimSpace(a), "/") == strings.TrimRight(strings.TrimSpace(b), "/")
}

// observeWorkerRuntime reads the runtime proof for one session, or nil when no
// port is wired.
func (c *Coordinator) observeWorkerRuntime(ctx stdctx.Context, id domain.SessionID) *domain.WorkerRuntimeObservation {
	obs := c.readWorkerRuntime(ctx, id)
	if obs == nil {
		return nil
	}
	if obs.Proof == domain.WorkerRuntimeUnavailable {
		c.noteRuntimeUnreadable(id)
	} else {
		c.runtimeUnreadable.Delete(id)
	}
	return obs
}

// readWorkerRuntime is the read-back without any effect on recovery state: the
// readback uses it, so an operator asking "who owns this" can never start or
// extend the grace that decides a stop.
func (c *Coordinator) readWorkerRuntime(ctx stdctx.Context, id domain.SessionID) *domain.WorkerRuntimeObservation {
	if c.workerRuntimeOwnership == nil || id == "" {
		return nil
	}
	obs := c.workerRuntimeOwnership.ObserveWorkerRuntime(ctx, id)
	if !obs.Proof.Valid() {
		obs.Proof = domain.WorkerRuntimeUnavailable
	}
	return &obs
}

// unreadableEpisode is one continuous stretch of failed runtime reads.
type unreadableEpisode struct{ first, last time.Time }

// noteRuntimeUnreadable opens an episode at this process's first failed read of
// id, or extends the open one. Only a SUCCESSFUL read ends an episode (see
// observeWorkerRuntime): with no read in between, nothing is known to have
// recovered, so two failures an hour apart are one unreadable hour -- however
// sparsely recovery happens to look.
func (c *Coordinator) noteRuntimeUnreadable(id domain.SessionID) {
	now := c.monotonicNow()
	if v, ok := c.runtimeUnreadable.Load(id); ok {
		if ep, ok := v.(unreadableEpisode); ok {
			c.runtimeUnreadable.Store(id, unreadableEpisode{first: ep.first, last: now})
			return
		}
	}
	c.runtimeUnreadable.Store(id, unreadableEpisode{first: now, last: now})
}

// unreadableFor is how long the open episode for id has lasted, and whether one
// is open. In memory on purpose: a restarted daemon owes every runtime a fresh
// read before a failure to read it may become a stop.
func (c *Coordinator) unreadableFor(id domain.SessionID) (time.Duration, bool) {
	v, ok := c.runtimeUnreadable.Load(id)
	if !ok {
		return 0, false
	}
	ep, ok := v.(unreadableEpisode)
	if !ok {
		return 0, false
	}
	return c.monotonicNow().Sub(ep.first), true
}

// workerAdoptionFactsFor assembles the decision's inputs for adopting rec as the
// worker of step under the outbox entry currently in force. runtime is the
// caller's own read-back (nil when no port is wired), passed in so one decision
// never reads the runtime twice.
func (c *Coordinator) workerAdoptionFactsFor(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry, rec domain.SessionRecord, runtime *domain.WorkerRuntimeObservation,
) workerAdoptionFacts {
	f := workerAdoptionFacts{
		RunTerminal:       run.State.Terminal(),
		Runtime:           runtime,
		SessionID:         rec.ID,
		SessionLaunchID:   rec.Metadata.RuntimeLaunchID,
		SessionWorktree:   rec.Metadata.WorkspacePath,
		SessionBranch:     rec.Metadata.Branch,
		SessionCreatedAt:  rec.CreatedAt,
		SessionTerminated: rec.IsTerminated,
	}
	f.UnreadableFor, f.UnreadableKnown = c.unreadableFor(rec.ID)
	ev := c.currentGenerationLaunchEvidence(ctx, run.ID, step.ID, entry)
	f.RecordedLaunchID, f.RecordedSessionID = ev.launchID, ev.sessionID
	f.RecordedWorktree, f.RecordedBranch = ev.worktree, ev.branch
	if entry.DispatchedAt != nil {
		f.ClaimedAt = *entry.DispatchedAt
	}
	return f
}

// generationLaunchEvidence is what the CURRENT dispatch generation's own
// launch records say: the launch, the session, the worktree and the branch.
type generationLaunchEvidence struct{ launchID, sessionID, worktree, branch string }

// currentGenerationLaunchEvidence reads the launch evidence of the generation
// that holds the outbox claim NOW, and of no other.
//
// The generation token on the outbox row IS the id of that generation's
// launch-intent record, and every later record of the same launch carries the
// intent's attempt id. The attempt named by the intent is therefore the fence:
// a record left by an earlier generation -- a confirmed or unconfirmed launch a
// retry replaced -- is never read as this generation's. A row claimed before
// tokens existed names no intent and yields nothing: the runtime proof and the
// creation-instant fence then carry the whole decision.
func (c *Coordinator) currentGenerationLaunchEvidence(
	ctx stdctx.Context, runID, stepID string, entry domain.WorkflowOutboxEntry,
) generationLaunchEvidence {
	var ev generationLaunchEvidence
	generation := strings.TrimSpace(entry.DispatchGeneration)
	if generation == "" || stepID == "" {
		return ev
	}
	ps, ok := c.provenanceStore()
	if !ok {
		return ev
	}
	records, err := ps.ListWorkflowDispatchCheckpointsByStep(ctx, stepID)
	if err != nil {
		return ev
	}
	attemptID := ""
	for _, r := range records {
		if r.ID == generation && r.AttemptID != nil {
			attemptID = strings.TrimSpace(*r.AttemptID)
		}
	}
	if attemptID == "" {
		return ev
	}
	for i := len(records) - 1; i >= 0; i-- {
		r := records[i]
		if r.AttemptID == nil || strings.TrimSpace(*r.AttemptID) != attemptID {
			continue
		}
		if ev.launchID == "" && strings.TrimSpace(r.RuntimeLaunchID) != "" {
			ev.launchID = strings.TrimSpace(r.RuntimeLaunchID)
			if r.SessionID != nil {
				ev.sessionID = strings.TrimSpace(*r.SessionID)
			}
		}
		if ev.worktree == "" {
			ev.worktree = strings.TrimSpace(r.WorktreePath)
		}
		if ev.branch == "" {
			ev.branch = strings.TrimSpace(r.Branch)
		}
	}
	if ev.launchID == "" {
		// The confirmation write is what may have failed; the ledger's own
		// unconfirmed record then carries this attempt's launch.
		if rec, ok := c.latestUnconfirmedLaunchRecord(ctx, runID, stepID); ok && rec.AttemptID == attemptID {
			ev.launchID = strings.TrimSpace(rec.RuntimeLaunchID)
			ev.sessionID = strings.TrimSpace(rec.SessionID)
			if ev.worktree == "" {
				ev.worktree = strings.TrimSpace(rec.WorktreePath)
			}
			if ev.branch == "" {
				ev.branch = strings.TrimSpace(rec.Branch)
			}
		}
	}
	return ev
}

// workerOwnershipStopRecord is the machine-readable half of an ownership stop:
// identities and closed codes only, never a prompt, a command or a token.
type workerOwnershipStopRecord struct {
	SessionID          string `json:"sessionId"`
	Action             string `json:"action"`
	Reason             string `json:"reason"`
	Proof              string `json:"proof,omitempty"`
	RuntimeInstanceID  string `json:"runtimeInstanceId,omitempty"`
	ObservedInstanceID string `json:"observedInstanceId,omitempty"`
}

// stopWorkerOwnershipUnproven parks a dispatch whose candidate worker AO cannot
// prove it owns. Same shape as every other ambiguity stop -- evidence first
// through the gate, then the step and the run -- and deliberately the same
// non-action on the outbox: it stays `dispatched`, so nothing can launch over a
// runtime that might still be running.
//
// Idempotent (I18): a run already parked on this exact stop writes nothing.
func (c *Coordinator) stopWorkerOwnershipUnproven(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep,
	sessionID domain.SessionID, decision WorkerRecoveryDecision,
) (domain.WorkflowStep, error) {
	if c.runIsTerminalOnDisk(ctx, run) {
		// A cancel (or any close) that landed since this pass read the run wins:
		// no evidence, no stop and no notification on a closed run.
		return step, nil
	}
	if run.State == domain.WorkflowRunNeedsAttention {
		if reason, ok := c.latestCanonicalStopReason(ctx, run.ID); ok && reason == ReasonWorkerOwnershipUnproven {
			return step, nil
		}
	}
	detail := fmt.Sprintf("session %s may not be adopted (%s): %s", sessionID, decision.Reason, decision.Detail)
	if c.log != nil {
		c.log.Warn("workflow: refusing to adopt a worker whose ownership is unproven",
			"run", run.ID, "step", step.ID, "session", sessionID, "reason", decision.Reason)
	}
	if _, rerr := c.raiseAmbiguousWorkerState(ctx, run, step, ReasonWorkerOwnershipUnproven, detail,
		c.observedWorkerFactsFor(ctx, sessionID, nil)); rerr != nil {
		return step, rerr
	}
	now := c.clock()
	if step.State == domain.WorkflowStepRunning || step.State == domain.WorkflowStepReady {
		if _, err := c.store.UpdateWorkflowStepState(ctx, step.ID, step.State, domain.WorkflowStepWaiting, now); err != nil {
			return step, err
		}
		step.State = domain.WorkflowStepWaiting
	}
	if run.State == domain.WorkflowRunRunning || run.State == domain.WorkflowRunWaiting {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, now); err != nil {
			return step, err
		}
	}
	rec := workerOwnershipStopRecord{SessionID: string(sessionID), Action: string(decision.Action), Reason: string(decision.Reason)}
	if obs := c.observeWorkerRuntime(ctx, sessionID); obs != nil {
		rec.Proof, rec.RuntimeInstanceID, rec.ObservedInstanceID = string(obs.Proof), obs.RuntimeInstanceID, obs.ObservedInstanceID
	}
	state, _ := json.Marshal(rec)
	c.recordAttentionStopWithState(ctx, run, &step.ID, ReasonWorkerOwnershipUnproven, detail, string(state))
	return step, nil
}
