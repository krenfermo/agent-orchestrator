package workflow

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// dispatch_state_machine.go — the phased worker dispatch: intent, launch,
// confirmation, RUNNING. In that order, each phase durable before the next one
// begins.
//
// What it replaces. Dispatch used to be one straight line: mark the outbox
// dispatched, move the step to RUNNING, call Spawn, and write a checkpoint if
// that returned. The step was therefore RUNNING at a moment when nothing had
// been launched, nothing had been proven, and the only durable trace of the
// launch was written after it. Every question anybody later asked of that state
// — is this worker alive, did it ever start, may this be retried — was being
// asked of a row that said "running" because AO had *intended* to launch, and
// an attempt row that existed because AO had intended to create one.
//
// The four phases, and what each one is allowed to claim:
//
//  1. INTENT (LaunchOutcomeIntended). Written before any launcher or session
//     call. It claims only that AO is about to launch: this run, this step,
//     this attempt, under this outbox key, aimed at this branch/worktree/base.
//     If it cannot be written, nothing is launched at all — a launch AO cannot
//     record is a launch AO cannot later reconcile, and the whole point of the
//     record is that it exists before the thing it describes.
//  2. LAUNCH. The launcher is invoked through WorkerLauncher, and the process/
//     session ownership evidence is read back through SessionOwnership. Both
//     are interfaces so that success, failure, and a launch that returns no
//     evidence at all are each reachable in a test without a process, a
//     terminal, or a timer.
//  3. CONFIRMATION (LaunchOutcomeDispatched). Written only once launch evidence
//     was actually observed — at minimum a session identity, plus whatever
//     ownership proof the runtime exposes. This is the record that licenses
//     RUNNING.
//  4. RUNNING. The step moves ready -> running, and the attempt becomes the
//     open attempt of a launched worker, strictly after (3) is durable.
//
// And the two failure shapes, which are deliberately different states:
//
//   - The launcher failed after intent was persisted: a `failed` boundary
//     carrying the evidence gathered so far, then the pre-existing bounded
//     retry-per-policy path (worker_launch_recovery.go) — retry when the cause
//     is transient and the budget allows, needs_attention when it does not.
//     Never a step left RUNNING over a worker that does not exist.
//   - The launcher succeeded and the confirmation could not be persisted: a
//     `worker_launch_unconfirmed` record, the outbox left `dispatched`, the
//     step left OUT of running and without a session. It is distinguishable
//     from full success (no confirmation, no RUNNING) and from full failure
//     (the evidence says an agent was launched), which is exactly what a later
//     reconciler needs in order to adopt rather than relaunch.

// WorkerLaunchRequest is everything a launcher needs to start one worker, and
// nothing about how it should do it. It is the coordinator's side of the launch
// boundary: workflow decides WHAT is launched and against which workspace,
// the launcher decides how a process comes to exist.
type WorkerLaunchRequest struct {
	RunID     string
	StepID    string
	AttemptID string
	ProjectID domain.ProjectID
	Harness   domain.AgentHarness
	IssueID   domain.IssueID
	Prompt    string
	// DisplayName is the human-facing session name.
	DisplayName string
	// BaseRef is the ref the workspace is cut from.
	BaseRef string
	// RuntimeEnv is the owner-scoped provider subprocess env resolved by
	// RuntimeIsolation. Nil when no isolation is wired.
	RuntimeEnv map[string]string
	Owner      domain.UserID
	// WorkflowRunID tells the session it belongs to a run that already holds
	// this repository+branch pair, so it does not queue behind its own run.
	WorkflowRunID string
}

// WorkerLaunchResult is what a launcher can prove about a launch it performed.
//
// A launcher returning a nil error and an empty Session.ID has NOT proven a
// launch: it has reported one. That case is treated as ambiguous rather than
// successful, because "something may be running and AO cannot name it" is the
// one state from which a relaunch puts two agents on one worktree.
type WorkerLaunchResult struct {
	Session domain.SessionRecord
	// LaunchedAt is when the launch itself happened. Zero when the launcher
	// cannot honestly say; it is never defaulted to the coordinator's clock at
	// record time, which would turn "we wrote this then" into "it started then".
	LaunchedAt time.Time
}

// WorkerLauncher is the injectable launch surface the dispatch state machine
// calls, and the ONLY place in this file where anything outside AO's own
// storage is invoked.
//
// Production wiring is the pre-existing Spawner (session_manager), adapted
// below. Tests inject a fake, which is what makes launch success, launch
// failure, and evidence-free success independently reachable without spawning
// a process or waiting for one.
type WorkerLauncher interface {
	LaunchWorker(ctx stdctx.Context, req WorkerLaunchRequest) (WorkerLaunchResult, error)
}

// SessionOwnershipEvidence is the process/session ownership proof for one
// launch: which runtime instance holds it, which launch generation fences it,
// and which native agent session it is.
//
// The launch generation is the field that decides a retry. A session id
// survives a daemon restart while the process behind it does not, so "a session
// row exists" is not "the process AO started is alive"; only the generation
// separates those.
type SessionOwnershipEvidence struct {
	SessionID       domain.SessionID
	RuntimeHandleID string
	RuntimeLaunchID string
	AgentSessionID  string
	Branch          string
	WorktreePath    string
	BaseSHA         string
	// Observed reports whether AO could actually read the session back. False
	// is never "the session is not ours" — it is "AO could not tell", and
	// Unavailable says why.
	Observed bool
	// Unavailable names why the proof could not be read. Empty when Observed.
	Unavailable string
}

// SessionOwnership is the injectable read-back of a launched session's
// ownership evidence.
//
// It is separate from WorkerLauncher because the two answer different
// questions — "did a launch happen" and "who owns what it produced" — and a
// test needs to be able to fail one without the other. It never returns an
// error: a proof AO could not read is one of the facts the boundary records,
// exactly as in the evidence snapshot (see evidence_snapshot.go).
type SessionOwnership interface {
	ObserveSessionOwnership(ctx stdctx.Context, id domain.SessionID) SessionOwnershipEvidence
}

// DispatchRecorder is the write side of migration 0133/0134's dispatch
// checkpoints, type-asserted off the coordinator's Store following this
// package's narrow-optional convention (see ProvenanceStore).
//
// Unlike the read side, absence here is NOT tolerated at dispatch time: the
// intent record is the precondition for launching at all, so a store that
// cannot write one cannot dispatch. See beginWorkerDispatch.
type DispatchRecorder interface {
	CreateWorkflowDispatchCheckpoint(ctx stdctx.Context, cp domain.WorkflowDispatchCheckpoint) (domain.WorkflowDispatchCheckpoint, error)
}

func (c *Coordinator) dispatchRecorder() (DispatchRecorder, bool) {
	rec, ok := c.store.(DispatchRecorder)
	return rec, ok
}

// errNoDispatchRecorder is the refusal to launch something AO cannot record.
var errNoDispatchRecorder = errors.New(
	"workflow: this store cannot record dispatch boundaries, so no launch may be attempted")

// errLaunchWithoutEvidence is a launcher that reported success and named no
// session. Nothing is retried on it here: the outbox stays `dispatched`, which
// routes the next pass through adoptOrMarkAmbiguous — adopt on evidence,
// escalate otherwise, never spawn again.
var errLaunchWithoutEvidence = errors.New(
	"workflow: launcher reported success without a session identity")

// workerLauncherOrDefault returns the injected launcher, or the default adapter
// over the pre-existing Spawner. Nil only when neither is wired, which is the
// pre-8B "durable foundation only" configuration dispatchWorkStep already
// guards for.
func (c *Coordinator) workerLauncherOrDefault() WorkerLauncher {
	if c.workerLauncher != nil {
		return c.workerLauncher
	}
	if c.spawner == nil {
		return nil
	}
	return spawnerWorkerLauncher{spawner: c.spawner, clock: c.clock}
}

// sessionOwnershipOrDefault returns the injected ownership prober, or the
// default adapter over SessionFacts. Never nil: with nothing wired it reports
// the proof unavailable, which is the honest answer and not a blocker.
func (c *Coordinator) sessionOwnershipOrDefault() SessionOwnership {
	if c.sessionOwnership != nil {
		return c.sessionOwnership
	}
	return sessionFactsOwnership{facts: c.sessionFacts}
}

// spawnerWorkerLauncher adapts the pre-existing Spawner to WorkerLauncher. It
// adds nothing: same SpawnConfig, same call, same transactional property that
// Spawn either returns a session record or returns an error having created
// none.
type spawnerWorkerLauncher struct {
	spawner Spawner
	clock   func() time.Time
}

func (l spawnerWorkerLauncher) LaunchWorker(ctx stdctx.Context, req WorkerLaunchRequest) (WorkerLaunchResult, error) {
	rec, _, _, err := l.spawner.Spawn(ctx, ports.SpawnConfig{
		ProjectID:     req.ProjectID,
		Kind:          domain.KindWorker,
		Harness:       req.Harness,
		IssueID:       req.IssueID,
		Prompt:        req.Prompt,
		DisplayName:   req.DisplayName,
		BaseRef:       req.BaseRef,
		RuntimeEnv:    req.RuntimeEnv,
		Owner:         req.Owner,
		WorkflowRunID: req.WorkflowRunID,
	})
	if err != nil {
		return WorkerLaunchResult{}, err
	}
	return WorkerLaunchResult{Session: rec, LaunchedAt: l.clock()}, nil
}

// sessionFactsOwnership reads ownership evidence back off the session row the
// launch created. A session it cannot read is reported unavailable, never
// absent: the two are different, and only one of them is a fact.
type sessionFactsOwnership struct{ facts SessionFacts }

func (o sessionFactsOwnership) ObserveSessionOwnership(ctx stdctx.Context, id domain.SessionID) SessionOwnershipEvidence {
	ev := SessionOwnershipEvidence{SessionID: id}
	if id == "" {
		ev.Unavailable = "the launch named no session, so no ownership proof can be read"
		return ev
	}
	if o.facts == nil {
		ev.Unavailable = "no session read path is wired into this coordinator"
		return ev
	}
	rec, found, err := o.facts.GetSession(ctx, id)
	switch {
	case err != nil:
		ev.Unavailable = "reading the session failed: " + err.Error()
		return ev
	case !found:
		ev.Unavailable = "the session row could not be found"
		return ev
	}
	ev.Observed = true
	ev.RuntimeHandleID = rec.Metadata.RuntimeHandleID
	ev.RuntimeLaunchID = rec.Metadata.RuntimeLaunchID
	ev.AgentSessionID = rec.Metadata.AgentSessionID
	ev.Branch = rec.Metadata.Branch
	ev.WorktreePath = rec.Metadata.WorkspacePath
	ev.BaseSHA = rec.Metadata.DiffBaseSHA
	return ev
}

// ---- the durable boundary record -------------------------------------------

// dispatchBoundary is one row of the dispatch state machine's own history. It
// exists as a struct rather than a long argument list because every phase
// writes the same shape and the phases differ only in what they are entitled to
// claim.
type dispatchBoundary struct {
	run     domain.WorkflowRun
	step    domain.WorkflowStep
	entry   domain.WorkflowOutboxEntry
	attempt string
	harness domain.AgentHarness
	phase   domain.WorkflowDispatchPhase
	stage   domain.WorkflowLaunchStage
	outcome domain.WorkflowLaunchOutcome

	sessionID  string
	errorClass domain.WorkflowErrorClass
	detail     string

	branch       string
	worktreePath string
	baseSHA      string
	fingerprint  string

	runtimeHandleID string
	runtimeLaunchID string
	agentSessionID  string

	launchedAt *time.Time
	evidence   map[string]string
}

// recordDispatchBoundary appends one dispatch record. The error is returned
// rather than swallowed at every call site because the intent and confirmation
// phases both treat a failed write as a decision point, not as a lost log line.
func (c *Coordinator) recordDispatchBoundary(ctx stdctx.Context, b dispatchBoundary) error {
	recorder, ok := c.dispatchRecorder()
	if !ok {
		return errNoDispatchRecorder
	}
	stepID := b.step.ID
	cp := domain.WorkflowDispatchCheckpoint{
		ID:             "wfd-" + c.newID(),
		WorkflowRunID:  b.run.ID,
		WorkflowStepID: &stepID,
		Phase:          b.phase,
		IdempotencyKey: b.entry.IdempotencyKey,
		Harness:        string(b.harness),
		LaunchStage:    b.stage,
		LaunchOutcome:  b.outcome,
		ErrorClass:     b.errorClass,
		EvidenceJSON:   encodeDispatchEvidence(b.evidence),
		Detail:         boundDispatchDetail(b.detail),

		Branch:               b.branch,
		WorktreePath:         b.worktreePath,
		BaseSHA:              b.baseSHA,
		WorkspaceFingerprint: b.fingerprint,

		RuntimeHandleID: b.runtimeHandleID,
		RuntimeLaunchID: b.runtimeLaunchID,
		AgentSessionID:  b.agentSessionID,

		LaunchedAt: b.launchedAt,
		CreatedAt:  c.clock(),
	}
	if b.attempt != "" {
		attemptID := b.attempt
		cp.AttemptID = &attemptID
	}
	if b.sessionID != "" {
		sessionID := b.sessionID
		cp.SessionID = &sessionID
	}
	_, err := recorder.CreateWorkflowDispatchCheckpoint(ctx, cp)
	return err
}

// dispatchDetailMaxLen bounds the persisted detail. Same reasoning as
// workerLaunchErrorMaxLen: launch details are short in practice, and this only
// stops a runtime dumping a process log into one row.
const dispatchDetailMaxLen = 4000

func boundDispatchDetail(detail string) string {
	if len(detail) > dispatchDetailMaxLen {
		return detail[:dispatchDetailMaxLen]
	}
	return detail
}

// encodeDispatchEvidence renders the boundary's extra observations. The column
// is CHECK (json_valid(...)), so this never returns anything that is not an
// object — an unencodable map degrades to "{}" rather than to a rejected write.
func encodeDispatchEvidence(evidence map[string]string) string {
	if len(evidence) == 0 {
		return "{}"
	}
	raw, err := json.Marshal(evidence)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// ---- phase 1: intent --------------------------------------------------------

// workerDispatchIntent is what phase 1 produced and the later phases carry: the
// attempt this launch belongs to, and the workspace facts the launch was aimed
// at, recorded before it was attempted.
type workerDispatchIntent struct {
	attempt      domain.WorkflowAttempt
	harness      domain.AgentHarness
	branch       string
	worktreePath string
	baseSHA      string
	fingerprint  string
}

// beginWorkerDispatch opens the attempt and persists the dispatch intent,
// strictly before any launcher or session call.
//
// The attempt row is created here rather than on success precisely because
// "an attempt row exists" must stop meaning "an attempt is running". From this
// point on, an attempt exists whose step is NOT running and whose dispatch has
// no confirmation, which is the state the whole file is built to make readable.
func (c *Coordinator) beginWorkerDispatch(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry,
	harness domain.AgentHarness,
) (workerDispatchIntent, error) {
	if _, ok := c.dispatchRecorder(); !ok {
		return workerDispatchIntent{}, errNoDispatchRecorder
	}
	now := c.clock()
	attempt, err := c.openWorkerAttempt(ctx, step.ID, harness, now)
	if err != nil {
		return workerDispatchIntent{}, err
	}
	// From here on the intent is returned even when this function fails, so the
	// caller can conclude the attempt row it just opened. An attempt left open
	// over a dispatch that never launched would be the same "a row exists,
	// therefore something is running" reading this whole file removes.
	intent := workerDispatchIntent{attempt: attempt, harness: harness}
	// The workspace facts a launch is aimed at, "when available": before a
	// session exists AO knows the project checkout it is launching into and
	// nothing about the worktree that launch will be given. Whatever it can
	// read now is recorded now, because a confirmation that disagrees with its
	// own intent is itself evidence.
	intent.branch, intent.worktreePath, intent.baseSHA, intent.fingerprint =
		c.observedLaunchWorkspace(ctx, run, "", "", c.projectPathFor(ctx, run.ProjectID))

	if err := c.recordDispatchBoundary(ctx, dispatchBoundary{
		run: run, step: step, entry: entry, attempt: attempt.ID, harness: harness,
		phase:        domain.DispatchPhaseWorkerLaunchIntent,
		stage:        domain.LaunchStageIntent,
		outcome:      domain.LaunchOutcomeIntended,
		detail:       fmt.Sprintf("worker launch intent recorded for attempt %s on %s", attempt.ID, harness),
		branch:       intent.branch,
		worktreePath: intent.worktreePath,
		baseSHA:      intent.baseSHA,
		fingerprint:  intent.fingerprint,
		evidence:     map[string]string{"outboxStatus": string(entry.Status)},
	}); err != nil {
		return intent, err
	}
	return intent, nil
}

// openWorkerAttempt returns the step's open attempt, creating one when the step
// has none or when the latest one is already terminal (Checkpoint 8H: a prior
// provider's failed attempt is never overwritten by the fallback's).
func (c *Coordinator) openWorkerAttempt(
	ctx stdctx.Context,
	stepID string,
	harness domain.AgentHarness,
	now time.Time,
) (domain.WorkflowAttempt, error) {
	attempts, err := c.store.ListWorkflowAttempts(ctx, stepID)
	if err != nil {
		return domain.WorkflowAttempt{}, err
	}
	if len(attempts) > 0 && attempts[len(attempts)-1].Outcome == "" {
		return attempts[len(attempts)-1], nil
	}
	return c.store.CreateWorkflowAttempt(ctx, "wfa-"+c.newID(), stepID, string(harness), "", now)
}

// observedLaunchWorkspace reads whatever workspace facts are available for a
// launch: branch, worktree, the base commit it is authorized against, and the
// fingerprint of the tree as it stands. Every one is best-effort and every one
// is empty rather than guessed when it cannot be read.
func (c *Coordinator) observedLaunchWorkspace(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	branch, worktree, fallbackPath string,
) (string, string, string, string) {
	path := worktree
	if path == "" {
		path = fallbackPath
	}
	if c.workspaceFacts == nil || path == "" {
		return branch, worktree, "", ""
	}
	obs, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
		Path: path, Branch: branch, ProjectID: domain.ProjectID(run.ProjectID),
	})
	if err != nil {
		return branch, worktree, "", ""
	}
	if branch == "" {
		branch = obs.Branch
	}
	return branch, worktree, obs.HeadSHA, WorkspaceFingerprint(obs)
}

// ---- phase 2/3: launch, then confirmation -----------------------------------

// launchWorker performs phase 2 and gathers the evidence phase 3 records. It
// never writes state: the caller owns the failure routing, because a launch
// failure's answer (retry per policy, or stop with the evidence) is
// worker_launch_recovery.go's decision and not this function's.
func (c *Coordinator) launchWorker(
	ctx stdctx.Context,
	req WorkerLaunchRequest,
) (WorkerLaunchResult, SessionOwnershipEvidence, error) {
	launcher := c.workerLauncherOrDefault()
	if launcher == nil {
		return WorkerLaunchResult{}, SessionOwnershipEvidence{}, errors.New("workflow: no worker launcher is wired")
	}
	result, err := launcher.LaunchWorker(ctx, req)
	if err != nil {
		return WorkerLaunchResult{}, SessionOwnershipEvidence{}, err
	}
	// A launcher that reports success and names no session has proven nothing.
	// Treated as ambiguous, never as a launch to confirm and never as a failure
	// to retry: see errLaunchWithoutEvidence.
	if result.Session.ID == "" {
		return result, SessionOwnershipEvidence{}, errLaunchWithoutEvidence
	}
	return result, c.sessionOwnershipOrDefault().ObserveSessionOwnership(ctx, result.Session.ID), nil
}

// recordLaunchFailureBoundary appends the `failed` dispatch record for a launch
// that did not complete, carrying the evidence gathered up to that point. It is
// best-effort by construction: the caller is already on its way to a durable
// failure through worker_launch_recovery.go, and losing the boundary row must
// never turn a recorded failure into an unrecorded one.
func (c *Coordinator) recordLaunchFailureBoundary(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry,
	intent workerDispatchIntent,
	stage domain.WorkflowLaunchStage,
	errClass domain.WorkflowErrorClass,
	cause error,
) {
	detail := ""
	if cause != nil {
		detail = cause.Error()
	}
	if err := c.recordDispatchBoundary(ctx, dispatchBoundary{
		run: run, step: step, entry: entry, attempt: intent.attempt.ID, harness: intent.harness,
		phase:        domain.DispatchPhaseWorkerLaunchError,
		stage:        stage,
		outcome:      domain.LaunchOutcomeFailed,
		errorClass:   errClass,
		detail:       detail,
		branch:       intent.branch,
		worktreePath: intent.worktreePath,
		baseSHA:      intent.baseSHA,
		fingerprint:  intent.fingerprint,
		evidence:     map[string]string{"attemptId": intent.attempt.ID},
	}); err != nil && c.log != nil {
		c.log.Warn("workflow: could not record the worker launch failure boundary",
			"step", step.ID, "stage", stage, "err", err)
	}
}

// recordAmbiguousLaunchBoundary appends the `ambiguous` record for a launcher
// that reported success and named nothing. Best-effort for the same reason as
// above; the state it leaves behind (outbox `dispatched`, no session on the
// step) is what actually routes the next pass through adoptOrMarkAmbiguous.
func (c *Coordinator) recordAmbiguousLaunchBoundary(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry,
	intent workerDispatchIntent,
) {
	if err := c.recordDispatchBoundary(ctx, dispatchBoundary{
		run: run, step: step, entry: entry, attempt: intent.attempt.ID, harness: intent.harness,
		phase:        domain.DispatchPhaseWorkerLaunchUnconfirmed,
		stage:        domain.LaunchStageConfirm,
		outcome:      domain.LaunchOutcomeAmbiguous,
		detail:       errLaunchWithoutEvidence.Error(),
		branch:       intent.branch,
		worktreePath: intent.worktreePath,
		baseSHA:      intent.baseSHA,
		fingerprint:  intent.fingerprint,
	}); err != nil && c.log != nil {
		c.log.Warn("workflow: could not record the ambiguous worker launch boundary",
			"step", step.ID, "err", err)
	}
}

// unconfirmedLaunchRecordPhase is the ledger phase for phase 3's own failure:
// the launcher succeeded and AO could not durably confirm it.
//
// It is written to workflow_checkpoints rather than to the dispatch table for
// the plain reason that the dispatch table is the thing that just refused the
// write. A second home is what makes this state durable at all, and durable is
// the entire requirement: a reconciler that never runs on it is the same as no
// record.
const unconfirmedLaunchRecordPhase = "worker_launch_unconfirmed"

// unconfirmedLaunchRecord is the decoded form of that checkpoint's RetryState:
// enough to adopt the launched session later without relaunching it.
type unconfirmedLaunchRecord struct {
	AttemptID       string `json:"attemptId"`
	Harness         string `json:"harness"`
	SessionID       string `json:"sessionId"`
	Branch          string `json:"branch"`
	WorktreePath    string `json:"worktreePath"`
	BaseSHA         string `json:"baseSha"`
	RuntimeHandleID string `json:"runtimeHandleId"`
	RuntimeLaunchID string `json:"runtimeLaunchId"`
	AgentSessionID  string `json:"agentSessionId"`
	// Cause is why the confirmation could not be persisted, verbatim.
	Cause string `json:"cause"`
	// OwnershipUnavailable names why the ownership proof could not be read, if
	// it could not. Empty when it was observed.
	OwnershipUnavailable string `json:"ownershipUnavailable,omitempty"`
}

// recordUnconfirmedLaunch persists the distinct "launched, not confirmed"
// state. It deliberately does NOT set the step's session, advance the outbox,
// or move the step to running: every one of those would collapse this state
// into full success, and the outbox left at `dispatched` with no session on the
// step is exactly the shape adoptOrMarkAmbiguous already knows how to resolve
// from evidence rather than by launching a second worker.
func (c *Coordinator) recordUnconfirmedLaunch(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	intent workerDispatchIntent,
	result WorkerLaunchResult,
	ownership SessionOwnershipEvidence,
	cause error,
) {
	causeText := ""
	if cause != nil {
		causeText = cause.Error()
	}
	if len(causeText) > workerLaunchErrorMaxLen {
		causeText = causeText[:workerLaunchErrorMaxLen]
	}
	branch, worktree, baseSHA := launchWorkspaceFacts(result, ownership)
	state, _ := json.Marshal(unconfirmedLaunchRecord{
		AttemptID:            intent.attempt.ID,
		Harness:              string(intent.harness),
		SessionID:            string(result.Session.ID),
		Branch:               branch,
		WorktreePath:         worktree,
		BaseSHA:              baseSHA,
		RuntimeHandleID:      ownership.RuntimeHandleID,
		RuntimeLaunchID:      ownership.RuntimeLaunchID,
		AgentSessionID:       ownership.AgentSessionID,
		Cause:                causeText,
		OwnershipUnavailable: ownership.Unavailable,
	})
	stepID := step.ID
	sessionID := string(result.Session.ID)
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		SessionID:      &sessionID,
		Branch:         branch,
		WorktreePath:   worktree,
		BaseSHA:        baseSHA,
		NextAction: fmt.Sprintf(
			"worker session %s launched but its dispatch confirmation could not be persisted: %s",
			sessionID, causeText),
		DurablePhase:   unconfirmedLaunchRecordPhase,
		PayloadVersion: "v1",
		RetryState:     string(state),
		CreatedAt:      c.clock(),
	}); err != nil && c.log != nil {
		// Both durable homes refused. The outbox is still `dispatched` and the
		// step still has no session, so the next pass reaches
		// adoptOrMarkAmbiguous and resolves this from the session row itself —
		// which is why this is logged rather than escalated into a stop.
		c.log.Error("workflow: worker launch could not be confirmed OR recorded as unconfirmed",
			"step", step.ID, "session", sessionID, "err", err)
	}
	if c.log != nil {
		c.log.Warn("workflow: worker launched but its confirmation could not be persisted",
			"step", step.ID, "session", sessionID, "err", cause)
	}
}

// launchWorkspaceFacts folds the two sources of a LAUNCHED session's workspace
// facts in priority order: what the ownership read-back observed, and what the
// launcher's own returned record carried. Nothing is invented; an unreadable
// fact stays empty.
//
// The intent's own workspace facts are deliberately NOT folded in here. They
// describe the project checkout the launch was aimed at, which is a different
// place from the worktree the launch was actually given, and quietly
// substituting one for the other is how a dispatch record ends up naming a tree
// no worker ever touched.
func launchWorkspaceFacts(
	result WorkerLaunchResult,
	ownership SessionOwnershipEvidence,
) (branch, worktree, baseSHA string) {
	pick := func(values ...string) string {
		for _, v := range values {
			if v != "" {
				return v
			}
		}
		return ""
	}
	branch = pick(ownership.Branch, result.Session.Metadata.Branch)
	worktree = pick(ownership.WorktreePath, result.Session.Metadata.WorkspacePath)
	baseSHA = pick(ownership.BaseSHA, result.Session.Metadata.DiffBaseSHA)
	return branch, worktree, baseSHA
}

// ---- reading the state back -------------------------------------------------

// WorkerDispatchPhase is one work step's dispatch phase, derived from durable
// records alone. It is what a reconciler reads, and it is the reason "an
// attempt row exists" is no longer an answer to any question.
type WorkerDispatchPhase string

const (
	// WorkerDispatchNone means no dispatch boundary has been recorded.
	WorkerDispatchNone WorkerDispatchPhase = "none"
	// WorkerDispatchIntended means an intent was recorded and nothing has
	// concluded it. Either the launch is in flight right now, or the process
	// died holding it.
	WorkerDispatchIntended WorkerDispatchPhase = "intended"
	// WorkerDispatchFailed means the launch is proven not to have completed.
	WorkerDispatchFailed WorkerDispatchPhase = "failed"
	// WorkerDispatchUnconfirmed means a launch was reported and not durably
	// confirmed. The one phase from which a relaunch is forbidden and an
	// adoption is owed.
	WorkerDispatchUnconfirmed WorkerDispatchPhase = "unconfirmed"
	// WorkerDispatchConfirmed means the launch evidence was observed AND
	// durably recorded. The only phase that licenses RUNNING.
	WorkerDispatchConfirmed WorkerDispatchPhase = "confirmed"
)

// WorkerDispatchStatus is the derived dispatch state of one work step.
type WorkerDispatchStatus struct {
	Phase WorkerDispatchPhase
	// AttemptID and SessionID are whatever the newest record named; empty when
	// it named nothing.
	AttemptID string
	SessionID string
	// Record is the newest dispatch record itself, zero-valued when Phase is
	// WorkerDispatchNone or when it was derived from the ledger fallback.
	Record domain.WorkflowDispatchCheckpoint
	// Readable reports whether the dispatch records could be read at all. False
	// means unknown — never "nothing was dispatched".
	Readable bool
}

// LicensesRunning reports whether this dispatch state permits the step and its
// attempt to be treated as RUNNING.
func (s WorkerDispatchStatus) LicensesRunning() bool {
	return s.Phase == WorkerDispatchConfirmed
}

// WorkerDispatchStatusForStep derives one work step's dispatch phase from its
// durable records: the newest dispatch boundary wins, and the ledger's
// unconfirmed record is consulted for the one state that can only exist there
// (the dispatch table itself refused the write).
func (c *Coordinator) WorkerDispatchStatusForStep(ctx stdctx.Context, runID, stepID string) WorkerDispatchStatus {
	status := WorkerDispatchStatus{Phase: WorkerDispatchNone}
	ps, ok := c.provenanceStore()
	if !ok || stepID == "" {
		return status
	}
	records, err := ps.ListWorkflowDispatchCheckpointsByStep(ctx, stepID)
	if err != nil {
		return status
	}
	status.Readable = true
	if len(records) > 0 {
		latest := records[len(records)-1]
		status.Record = latest
		status.AttemptID = deref(latest.AttemptID)
		status.SessionID = deref(latest.SessionID)
		status.Phase = dispatchPhaseFromOutcome(latest.LaunchOutcome)
	}
	// The ledger's unconfirmed record only ever exists when the dispatch table
	// could not take the confirmation, so it can only ever be NEWER than the
	// intent that is the newest dispatch row. It never overrides a confirmation.
	if status.Phase == WorkerDispatchIntended || status.Phase == WorkerDispatchNone {
		if rec, ok := c.latestUnconfirmedLaunchRecord(ctx, runID, stepID); ok {
			status.Phase = WorkerDispatchUnconfirmed
			status.SessionID = rec.SessionID
			if rec.AttemptID != "" {
				status.AttemptID = rec.AttemptID
			}
		}
	}
	return status
}

func dispatchPhaseFromOutcome(outcome domain.WorkflowLaunchOutcome) WorkerDispatchPhase {
	switch outcome {
	case domain.LaunchOutcomeDispatched:
		return WorkerDispatchConfirmed
	case domain.LaunchOutcomeFailed:
		return WorkerDispatchFailed
	case domain.LaunchOutcomeUnconfirmed, domain.LaunchOutcomeAmbiguous:
		return WorkerDispatchUnconfirmed
	case domain.LaunchOutcomeIntended:
		return WorkerDispatchIntended
	default:
		// An outcome this build does not know is never folded into a
		// neighbouring one: unknown is treated as "not confirmed", which is the
		// only reading that cannot claim a worker is running.
		return WorkerDispatchIntended
	}
}

// latestUnconfirmedLaunchRecord returns the newest unconfirmed-launch record
// for a step, and whether it is still operative — i.e. no confirmed dispatch
// has been recorded in the ledger since.
func (c *Coordinator) latestUnconfirmedLaunchRecord(ctx stdctx.Context, runID, stepID string) (unconfirmedLaunchRecord, bool) {
	if runID == "" || stepID == "" {
		return unconfirmedLaunchRecord{}, false
	}
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return unconfirmedLaunchRecord{}, false
	}
	// Oldest first (created_at, id), so the last match wins on index order —
	// several checkpoints of one dispatch share a clock reading.
	recordIdx, dispatchedIdx := -1, -1
	for i, cp := range cps {
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID {
			continue
		}
		switch cp.DurablePhase {
		case unconfirmedLaunchRecordPhase:
			recordIdx = i
		case workerDispatchedDurablePhase:
			dispatchedIdx = i
		}
	}
	if recordIdx < 0 || dispatchedIdx > recordIdx {
		return unconfirmedLaunchRecord{}, false
	}
	var rec unconfirmedLaunchRecord
	if json.Unmarshal([]byte(cps[recordIdx].RetryState), &rec) != nil {
		return unconfirmedLaunchRecord{}, false
	}
	return rec, true
}

// WorkerAttemptRunning is THE answer to "is this attempt running", and the
// reason the question can no longer be answered by the existence of a row.
//
// All three must hold, and each one rules out a state AO used to call running:
//
//   - the attempt has not concluded (a terminal attempt is not running);
//   - the step is durably RUNNING (a step still at `ready` never started);
//   - the step's newest dispatch boundary is a CONFIRMATION, for this attempt
//     (an intent, an ambiguous launch, or a confirmation belonging to some
//     earlier attempt proves nothing about this one).
func (c *Coordinator) WorkerAttemptRunning(
	ctx stdctx.Context,
	runID string,
	step domain.WorkflowStep,
	attempt domain.WorkflowAttempt,
) bool {
	if attempt.Outcome != "" || step.State != domain.WorkflowStepRunning {
		return false
	}
	status := c.WorkerDispatchStatusForStep(ctx, runID, step.ID)
	if !status.LicensesRunning() {
		return false
	}
	// An attempt id recorded on the confirmation must match; a confirmation
	// that recorded none is not evidence about any particular attempt.
	return status.AttemptID != "" && status.AttemptID == attempt.ID
}
