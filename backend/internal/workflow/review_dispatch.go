package workflow

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// reviewTargetDurablePhase marks the checkpoint that records which workspace
// fingerprint a review cycle was actually dispatched against (Checkpoint
// 8P-E.13A.3). It is written once per review cycle, immediately before the
// outbox entry for that cycle, so every retry/recovery of the same cycle
// resolves the identical target.
const reviewTargetDurablePhase = "review_target_observed"

// reviewHarness was Checkpoint 8C's single hardcoded reviewer harness.
// Checkpoint 8L replaces it with a per-step dynamic choice from
// ExecutionRouter (see reviewerHarnessForStep in routing_dispatch.go):
// cross-provider from whichever harness actually implemented the work step,
// reused stably across all of that step's review/fix cycles.

// ReviewRuns is the narrow review-schema read/write surface workflow reuses
// (Checkpoints 8A/8C). *sqlite.Store satisfies it in production, backed by
// the exact same review/review_run tables and store methods
// internal/review.Engine's own Trigger uses (backend/internal/review/review.go
// and backend/internal/storage/sqlite/store/review_store.go) — workflow never
// writes to the review/review_run tables directly, and never reuses
// Engine.Trigger itself (see dispatchReviewFromPending's doc comment for why).
type ReviewRuns interface {
	// GetReviewRun resolves a review run by id. Used by Reconcile's
	// best-effort integrity check (recovery.go) and by observeReviewStep's
	// live verdict read (review_progress.go).
	GetReviewRun(ctx stdctx.Context, id string) (domain.ReviewRun, bool, error)

	// GetReviewBySessionAndHarness and UpsertReview together implement the
	// one-row-per-(session,harness) "review" parent get-or-create. There is
	// no separate GetOrCreateReview/ensureReview store method to call —
	// internal/review.Engine.Trigger inlines this exact same composition in
	// its own unexported upsertReview helper (review.go), and
	// ensureReview below mirrors it faithfully.
	GetReviewBySessionAndHarness(ctx stdctx.Context, id domain.SessionID, harness domain.ReviewerHarness) (domain.Review, bool, error)
	UpsertReview(ctx stdctx.Context, r domain.Review) error

	// InsertReviewRun and GetReviewRunBySessionPRSHAAndHarness are the exact
	// insert + unique-index-conflict dedupe fallback pair
	// internal/review.Engine.Trigger uses (review.go's InsertReviewRun call
	// and its domain.ErrDuplicateReviewRun handling).
	InsertReviewRun(ctx stdctx.Context, run domain.ReviewRun) error
	GetReviewRunBySessionPRSHAAndHarness(ctx stdctx.Context, id domain.SessionID, prURL, targetSHA string, harness domain.ReviewerHarness) (domain.ReviewRun, bool, error)

	// ListReviewRunsBySession backs Checkpoint 8D's cycle-number derivation:
	// the count of review_runs already created for this worker session's
	// Claude reviews IS the natural cycle counter (see reviewCycleNumber in
	// this file) — no new schema needed, reusing review_run row cardinality
	// as the iteration count. Same store method internal/review already
	// exposes (backend/internal/storage/sqlite/store/review_store.go),
	// reused unmodified through this same narrow port.
	ListReviewRunsBySession(ctx stdctx.Context, id domain.SessionID) ([]domain.ReviewRun, error)

	// CancelRunningReviewRunsBySessionAndHarness backs Checkpoint 8P-D.3's
	// mid-session reviewer-capacity stall recovery: when a reviewer's own
	// session goes idle/exits without ever calling `ao review submit` (no
	// verdict), the still-"running" review_run must be closed out durably
	// (never silently forgotten, never left to eventually time out via
	// reviewStalenessThreshold) so reviewerHarnessForStep can route a fresh
	// cycle — to a fallback provider if policy allows one, or back to the
	// same one once capacity recovers. CAS-guarded in SQL on
	// status='running' AND verdict='' (see review.sql), so it can never
	// clobber a verdict that actually landed in the same instant. Same
	// store method internal/review already exposes.
	CancelRunningReviewRunsBySessionAndHarness(ctx stdctx.Context, id domain.SessionID, harness domain.ReviewerHarness, body string) (int64, error)
}

// ReviewerLaunchRequest is workflow's request to actually start a reviewer
// process/pane over a worktree.
type ReviewerLaunchRequest struct {
	Harness         domain.ReviewerHarness
	WorkerSessionID domain.SessionID
	ProjectID       domain.ProjectID
	ReviewID        string // the parent domain.Review row id
	RunID           string // the domain.ReviewRun id
	WorkspacePath   string
	Prompt          string
	SystemPrompt    string
	// RuntimeEnv overrides subprocess env for the reviewer process
	// (Checkpoint 8P-B.1) -- the workflow run owner's isolated
	// runtime-home, resolved once by Coordinator.resolveRuntimeEnv. Nil
	// preserves pre-8P-B.1 behavior exactly.
	RuntimeEnv map[string]string
}

// ReviewerLaunchResult is the runtime handle created for a reviewer launch.
type ReviewerLaunchResult struct {
	HandleID       string
	AgentSessionID string
}

// ReviewerLauncher is workflow's own narrow reviewer-launch port
// (Checkpoint 8C). It deliberately does NOT reuse internal/review.Launcher
// (backend/internal/review/launcher.go) wholesale: that launcher's
// Spawn/Notify always build their prompt/system-prompt internally via the
// unexported reviewTexts() (review/prompt.go), which unconditionally
// instructs the reviewer to post a GitHub PR review via
// `gh api .../pulls/{number}/reviews` and diff against a PR's base branch —
// there is no override hook, and that is fundamentally incompatible with
// 8C's no-PR, uncommitted-worktree review (see review_prompt.go's doc
// comment for the full reasoning). What IS reused unmodified: the reviewer
// registry/resolver (ports.ReviewerResolver, resolving "claude-code" to the
// exact same adapter instance internal/review uses) and that adapter's own
// ReviewCommand method, which alone builds the read-only tool allowlist/
// denylist and permission mode (adapters/reviewer/claudecode/claudecode.go)
// from whatever ports.ReviewInvocation it is given — the adapter itself has
// no PR assumption baked in, only review/launcher.go's invocation-building
// wrapper does. The concrete implementation (wired from internal/daemon)
// builds its own ReviewInvocation carrying workflow's own prompt (see
// BuildReviewPrompt) and calls the adapter directly, then spawns the runtime
// pane through the exact same generic runtime port every other session pane
// in the daemon already uses — not a new or more permissive mechanism.
type ReviewerLauncher interface {
	// Preflight checks whether the reviewer harness can actually be launched
	// (binary on PATH, etc.) without starting a pane, mirroring
	// review.Launcher's own Preflight semantics.
	Preflight(ctx stdctx.Context, harness domain.ReviewerHarness, workspacePath string) error
	// Launch starts a fresh reviewer pane for one review run.
	Launch(ctx stdctx.Context, req ReviewerLaunchRequest) (ReviewerLaunchResult, error)
}

// reviewStepOutboxIdempotencyKey is the deterministic idempotency key for a
// review step's trigger-review command. Checkpoint 8D makes it cycle-
// specific so a second review cycle for the same step gets its own outbox
// row (and thus its own single-flight guard) instead of colliding with
// cycle 1's already-acknowledged entry.
func reviewStepOutboxIdempotencyKey(stepID string, cycleNumber int, harness domain.ReviewerHarness) string {
	// Checkpoint 8P-D.3: harness is part of the key (not just stepID+cycle)
	// because completedReviewCycles/cycleNumber is computed per-harness —
	// once a capacity-driven fallback can change harness mid-step
	// (reviewerHarnessForStep skipping a cancelled run), a fresh harness
	// restarts that harness's own cycle count at 1, which would otherwise
	// collide with cycle 1's original (different-harness) idempotency key
	// and silently re-adopt the old, already-terminal outbox entry instead
	// of dispatching the fallback. Harness never changes across cycles
	// outside that path, so this is a no-op for every other existing case.
	return "workflow-step-review:" + stepID + ":cycle" + strconv.Itoa(cycleNumber) + ":" + string(harness)
}

// completedReviewCycles returns the count of Claude-Code review_runs already
// created for a worker session — Checkpoint 8D's natural cycle counter,
// reusing review_run row cardinality as the iteration count rather than
// adding a new counter column ("usa attempts/review_runs/checkpoints para
// representar iteraciones, no columnas fix1/fix2/fix3"). Callers derive the
// actual next/current cycle number from this count per their own call site's
// semantics (see dispatchReviewStep and cascade.go's maybeDispatchFix).
func (c *Coordinator) completedReviewCycles(ctx stdctx.Context, sessionID domain.SessionID, harness domain.ReviewerHarness) (int, error) {
	if c.reviewRuns == nil {
		return 0, nil
	}
	runs, err := c.reviewRuns.ListReviewRunsBySession(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, r := range runs {
		if r.Harness == harness {
			count++
		}
	}
	return count, nil
}

func reviewPayloadJSON(workStepID, reviewStepID, workerSessionID, targetSHA string, harness domain.ReviewerHarness, cycleNumber int) string {
	b, _ := json.Marshal(map[string]any{
		"workStepId":      workStepID,
		"reviewStepId":    reviewStepID,
		"workerSessionId": workerSessionID,
		"targetSha":       targetSHA,
		"harness":         string(harness),
		"cycle":           cycleNumber,
	})
	return string(b)
}

// dispatchReviewStep is the single idempotent dispatch algorithm for a
// review step's real Claude reviewer launch. Checkpoint 8C's original shape
// only ever unblocked cycle 1 from "pending" once the work step completed.
// Checkpoint 8D extends the exact same function to also dispatch cycle N+1
// once the fix step has delivered and observed a genuinely new workspace
// fingerprint (reviewStep sitting at "waiting", the 8C->8D revision's
// non-terminal resting state for a changes_requested verdict). Safe to call
// repeatedly — from ContinueRun, from boot recovery, and opportunistically
// from GetRun — without ever launching a second reviewer for the same cycle
// across the union of all call sites: the outbox idempotency key is
// cycle-specific (reviewStepOutboxIdempotencyKey), so a second call for an
// already-dispatched cycle always resolves through the
// dispatched/acknowledged branch below, never re-enters "pending".
func (c *Coordinator) dispatchReviewStep(ctx stdctx.Context, run domain.WorkflowRun, workStep, fixStep, reviewStep domain.WorkflowStep) (domain.WorkflowStep, error) {
	if run.State.Terminal() || reviewStep.State.Terminal() {
		return reviewStep, nil
	}
	// Checkpoint 8K-A: never launch/re-launch a reviewer while this step has
	// an unresolved question open.
	if open, err := c.hasOpenQuestion(ctx, run.ID, &reviewStep.ID); err != nil {
		return reviewStep, err
	} else if open {
		return reviewStep, nil
	}
	if c.reviewerLauncher == nil || c.reviewRuns == nil {
		// No launcher/review-store wired (e.g. a unit test exercising only
		// the durable foundation or 8B's work-step dispatch). Nothing to do.
		return reviewStep, nil
	}

	// Checkpoint 8P-E.13A: work-completion evidence gates this entire
	// function, before any lookup that could be mistaken for evidence.
	//
	// This guard used to live inside the `pending` case, BELOW the work
	// checkpoint lookup, and that ordering produced a real dead end in
	// ~/.ao/data: a child run whose work step was still `ready` because the
	// repository+branch was locked by another workflow has, by construction,
	// no work session and no work checkpoint — so the "no session/checkpoint
	// to review" branch fired and parked the run in needs_attention with
	// review_dispatch_ambiguous, on a run whose review was simply not due yet.
	// Nothing was ambiguous: work had not started.
	//
	// The invariant, stated once, for every cycle: a review may only be
	// dispatched from evidence that the work step actually completed. A work
	// step that is pending/ready/running/waiting (including waiting on a
	// branch lock) leaves the review step exactly where it is. Only once work
	// is durably `completed` does a missing session/checkpoint below become
	// genuine ambiguity worth a human's time.
	if workStep.State != domain.WorkflowStepCompleted {
		return reviewStep, nil
	}

	// The worker session, branch, and worktree path never change across
	// cycles (Checkpoint 8D reuses the SAME Codex worker session throughout
	// the loop, never a new Spawn) — always resolved from the work step's
	// own latest checkpoint, which Checkpoint 8B always writes with a
	// session id once the work step completes.
	workCP, hasWorkCP, err := c.store.GetLatestWorkflowCheckpointByStep(ctx, workStep.ID)
	if err != nil {
		return reviewStep, err
	}
	if !hasWorkCP || workCP.SessionID == nil || *workCP.SessionID == "" {
		return c.markReviewAmbiguous(ctx, run, reviewStep,
			"ambiguous_review_state: work step has no recorded session/checkpoint to review")
	}
	sessionID := *workCP.SessionID
	branch, worktreePath, baseSHA := workCP.Branch, workCP.WorktreePath, workCP.BaseSHA

	var targetSHA string
	// firstCycleTarget marks the two branches whose target is derived from the
	// work step's completion fingerprint rather than from a fix cycle's own
	// delivered fingerprint. Only those branches re-observe the workspace
	// below (Checkpoint 8P-E.13A.3): a fix cycle's target is already the
	// fingerprint the fix step observed moments earlier for this same cycle.
	firstCycleTarget := false
	switch reviewStep.State {
	case domain.WorkflowStepPending:
		// Cycle 1: the one-off hardcoded "work just completed, unblock
		// review" edge, not a generic dependency-resolution engine — mirrors
		// StartRun's plan->work unblock exactly. "Work has completed" is
		// already proven by the evidence gate above.

		// Checkpoint 8I: ReviewPolicy evaluates exactly once, right here, at
		// cycle 1 — before any reviewer is ever launched. A later fix cycle
		// (the WorkflowStepWaiting case below) never re-evaluates: once a
		// cycle 1 decision is REQUIRED, every subsequent cycle for this same
		// review step stays REQUIRED (checkpoint brief §10 — "no reevaluar y
		// saltar reviewer a mitad de una corrección sensible"). SKIPPED is
		// therefore only ever reachable from this exact branch.
		policyArtifact, err := c.planArtifactForRun(ctx, run)
		if err != nil {
			return reviewStep, err
		}
		facts, err := c.computeReviewRiskFacts(ctx, run, workStep, policyArtifact, workCP)
		if err != nil {
			return reviewStep, err
		}
		decision := EvaluateReviewPolicy(facts)
		decision.EvaluatedAt = c.clock()
		if err := c.persistReviewPolicyDecision(ctx, run, reviewStep, decision); err != nil {
			return reviewStep, err
		}
		if decision.Decision == ReviewSkipped {
			return c.applyReviewPolicySkip(ctx, run, reviewStep)
		}

		// Checkpoint 8D: target_sha is the work-completion checkpoint's own
		// WorkspaceFingerprint (workflow.WorkspaceFingerprint, computed by
		// observeWorkStep and stored on its FingerprintAfter field) — not a
		// literal git SHA. This is deliberate reuse of the column's
		// semantics (design decision 3): for a PR-less workflow review,
		// target_sha's honest meaning is "the identity of the reviewed
		// state," and it must be a fingerprint hash, not a raw (often empty)
		// HeadSHA, so a later fix cycle's "did the workspace genuinely
		// change" comparison (fingerprintBefore == this same value) compares
		// like with like. Falls back to the raw HeadSHA/BaseSHA only when no
		// workspace observation was ever available (e.g. WorkspaceFacts not
		// wired), matching 8C's original honest-if-unchanged behavior.
		targetSHA = workCP.FingerprintAfter
		if targetSHA == "" {
			targetSHA = workCP.HeadSHA
		}
		if targetSHA == "" {
			targetSHA = baseSHA
		}
		firstCycleTarget = true
		if _, err := c.store.UpdateWorkflowStepState(ctx, reviewStep.ID, domain.WorkflowStepPending, domain.WorkflowStepReady, c.clock()); err != nil {
			return reviewStep, err
		}
		reviewStep.State = domain.WorkflowStepReady

	case domain.WorkflowStepReady, domain.WorkflowStepRunning:
		// Crash-recovery resume ONLY: a previous call already unblocked
		// pending->ready (or even ready->running) but the outbox dispatch
		// itself never completed durably (no review_run_id recorded yet).
		// The common, non-crash case of "running with a review_run already
		// dispatched, awaiting verdict" must NOT re-enter here — that guard
		// is exactly this ReviewRunID check.
		if reviewStep.ReviewRunID != nil {
			return reviewStep, nil
		}
		// Checkpoint 8I crash-recovery: if the most recently persisted
		// checkpoint for this step is a SKIPPED review_policy_decision, a
		// prior call recorded the decision but crashed before
		// applyReviewPolicySkip finished walking ready->running->completed.
		// Resuming here must complete that same skip, never fall through to
		// a real reviewer dispatch (which would silently turn a SKIPPED
		// decision into a REQUIRED one on retry).
		if latestCP, hasLatestCP, cpErr := c.store.GetLatestWorkflowCheckpointByStep(ctx, reviewStep.ID); cpErr != nil {
			return reviewStep, cpErr
		} else if hasLatestCP && latestCP.DurablePhase == reviewPolicyDurablePhase {
			if decision, ok := decodeReviewPolicyDecision(latestCP.RetryState); ok && decision.Decision == ReviewSkipped {
				return c.applyReviewPolicySkip(ctx, run, reviewStep)
			}
		}
		targetSHA = workCP.FingerprintAfter
		if targetSHA == "" {
			targetSHA = workCP.HeadSHA
		}
		if targetSHA == "" {
			targetSHA = baseSHA
		}
		firstCycleTarget = true

	case domain.WorkflowStepWaiting:
		if fixStep.State == domain.WorkflowStepWaiting {
			// Cycle N+1 (Checkpoint 8D): only eligible once the fix step has
			// delivered AND observed a genuinely new workspace fingerprint
			// for THIS review step's cycle. fixStep.State == waiting with a
			// non-empty FingerprintAfter on its latest checkpoint is that
			// fact.
			fixCP, hasFixCP, err := c.store.GetLatestWorkflowCheckpointByStep(ctx, fixStep.ID)
			if err != nil {
				return reviewStep, err
			}
			if !hasFixCP || fixCP.FingerprintAfter == "" {
				return reviewStep, nil
			}
			// Idempotency: never re-review a fingerprint already reviewed by
			// the step's current review_run (covers a repeated
			// GetRun/Reconcile call landing after this cycle's review_run
			// already exists, before the outbox row above resolves to
			// acknowledged).
			if reviewStep.ReviewRunID != nil {
				existing, ok, err := c.reviewRuns.GetReviewRun(ctx, *reviewStep.ReviewRunID)
				if err != nil {
					return reviewStep, err
				}
				if ok && existing.TargetSHA == fixCP.FingerprintAfter {
					return reviewStep, nil
				}
			}
			targetSHA = fixCP.FingerprintAfter
			baseSHA = fixCP.FingerprintBefore
		} else {
			// Checkpoint 8P-D.3: a review step can also rest at "waiting"
			// after handleReviewerCapacityStall closes out a reviewer
			// session that went idle mid-review with no verdict — no
			// workspace change happened, so there is nothing for the
			// fixStep-based gate above to observe. Recognize this distinct
			// resting state via reviewStep.ReviewRunID's own review_run
			// having been durably CANCELLED (written only by
			// handleReviewerCapacityStall/CancelRunningReviewRunsBySession-
			// AndHarness) rather than a checkpoint DurablePhase: a Waiting
			// decision from routeReviewerDispatch below (no eligible
			// fallback yet) persists its OWN "routing_decision" checkpoint
			// on every retry attempt, which would overwrite any
			// checkpoint-phase-based marker before the real recovery wake
			// ever fires — the review_run's own terminal status is the one
			// fact that survives every intermediate no-op retry unchanged.
			// Re-reviews the SAME target, unchanged — this is a provider
			// retry, not a new fix cycle.
			if reviewStep.ReviewRunID == nil {
				return reviewStep, nil
			}
			priorRun, ok, err := c.reviewRuns.GetReviewRun(ctx, *reviewStep.ReviewRunID)
			if err != nil {
				return reviewStep, err
			}
			if !ok || priorRun.Status != domain.ReviewRunCancelled {
				return reviewStep, nil
			}
			targetSHA = priorRun.TargetSHA
			if targetSHA == "" {
				targetSHA = workCP.FingerprintAfter
			}
			if targetSHA == "" {
				targetSHA = workCP.HeadSHA
			}
		}

	default:
		// terminal: nothing to dispatch from here.
		return reviewStep, nil
	}

	// Checkpoint 8L: resolve the reviewer harness before touching the outbox
	// — see reviewerHarnessForStep's doc comment for why this must be stable
	// across cycles and only routed fresh on cycle 1.
	harness, ok, err := c.reviewerHarnessForStep(ctx, run, workStep, reviewStep, domain.SessionID(sessionID), workCP)
	if err != nil {
		return reviewStep, err
	}
	if !ok {
		return c.markRunWaitingForCapacity(ctx, run, reviewStep)
	}

	completedCycles, err := c.completedReviewCycles(ctx, domain.SessionID(sessionID), harness)
	if err != nil {
		return reviewStep, err
	}
	// Pending/Waiting always start a genuinely new cycle (completedCycles
	// reflects only prior FINISHED cycles, none in flight): cycleNumber =
	// completedCycles + 1. Ready/Running is a crash-recovery resume of
	// whatever cycle was already in flight — by construction that state is
	// only reachable mid-dispatch of the LATEST attempted cycle, so its
	// review_run (if the crash landed after InsertReviewRun but before the
	// step link) is already included in completedCycles; using
	// completedCycles itself (not +1) here is what lets recovery recompute
	// the SAME cycle number, and thus the SAME outbox idempotency key, the
	// original crashed attempt used.
	cycleNumber := completedCycles + 1
	if reviewStep.State == domain.WorkflowStepReady || reviewStep.State == domain.WorkflowStepRunning {
		if completedCycles > 0 {
			cycleNumber = completedCycles
		} else {
			cycleNumber = 1
		}
	}

	// Checkpoint 8P-E.13A.3: the target must name the workspace the reviewer
	// is about to READ, not the workspace as it stood when the work step was
	// observed. Those are the same state only when review dispatch follows
	// work completion immediately — and it very often does not: a run can sit
	// in needs_attention, wait on a branch lock, or wait for reviewer capacity
	// for hours first (in ~/.ao/data, wf-507d9a93 waited 110 minutes). During
	// that window the repository can move, and the reviewer still reads
	// whatever is there when it launches. Recording the stale work-completion
	// fingerprint as target_sha therefore labels the verdict with a state
	// nobody reviewed, and Verify — which compares the live workspace against
	// that label — then fails with verify_workspace_changed even though
	// nothing changed after the reviewer approved.
	if firstCycleTarget {
		targetSHA = c.reviewTargetFingerprint(ctx, run, reviewStep, workCP, cycleNumber, targetSHA)
	}

	now := c.clock()
	entry, _, err := c.store.EnqueueWorkflowOutboxEntry(ctx, domain.WorkflowOutboxEntry{
		ID:             "wfo-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &reviewStep.ID,
		IdempotencyKey: reviewStepOutboxIdempotencyKey(reviewStep.ID, cycleNumber, harness),
		CommandType:    domain.WorkflowOutboxTriggerReview,
		Payload:        reviewPayloadJSON(workStep.ID, reviewStep.ID, sessionID, targetSHA, harness, cycleNumber),
		CreatedAt:      now,
	})
	if err != nil {
		return reviewStep, err
	}

	switch entry.Status {
	case domain.WorkflowOutboxPending:
		return c.dispatchReviewFromPending(ctx, run, reviewStep, entry, sessionID, branch, worktreePath, baseSHA, targetSHA, harness)
	case domain.WorkflowOutboxDispatched, domain.WorkflowOutboxAcknowledged:
		// Dispatched: a previous attempt got at least as far as "about to
		// launch," but we don't durably know if the launch itself completed.
		return c.adoptReviewOrMarkAmbiguous(ctx, run, reviewStep, entry, sessionID, targetSHA, harness)
	case domain.WorkflowOutboxFailed:
		// Already durably recorded as failed; no auto-retry.
		return reviewStep, nil
	default:
		return reviewStep, nil
	}
}

// reviewTargetFingerprint resolves the workspace fingerprint a review cycle is
// dispatched against: the live workspace as observed right now, recorded
// durably so every retry and every crash-recovery of the SAME cycle resolves
// the identical value.
//
// Stability across retries is not a nicety here — adoptReviewOrMarkAmbiguous
// looks the in-flight review_run up by (session, "", target_sha, harness), so a
// target that drifted between the first dispatch attempt and a recovery attempt
// would fail to adopt the reviewer already running and park the run in
// review_dispatch_ambiguous. Hence: observe once, checkpoint it, reuse it.
//
// Every failure path falls back to the caller's work-completion fingerprint —
// the pre-8P-E.13A.3 behavior — because a missing observation must not turn a
// dispatchable review into an error. It only means the target is as good as it
// used to be, never worse.
func (c *Coordinator) reviewTargetFingerprint(ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep, workCP domain.WorkflowCheckpoint, cycleNumber int, fallback string) string {
	if recorded, ok := c.recordedReviewTarget(ctx, run.ID, reviewStep.ID, cycleNumber); ok {
		return recorded
	}
	if c.workspaceFacts == nil || workCP.WorktreePath == "" || workCP.SessionID == nil || *workCP.SessionID == "" {
		return fallback
	}
	obs, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
		Path:      workCP.WorktreePath,
		Branch:    workCP.Branch,
		SessionID: domain.SessionID(*workCP.SessionID),
		ProjectID: domain.ProjectID(run.ProjectID),
	})
	if err != nil {
		return fallback
	}
	target := WorkspaceFingerprint(obs)
	if target == "" {
		return fallback
	}
	stepID := reviewStep.ID
	stateJSON, _ := json.Marshal(map[string]any{"cycle": cycleNumber})
	nextAction := "review_target_observed: reviewing the workspace as it stands now"
	if target != fallback {
		// Not a failure — the reviewer reads the live workspace either way —
		// but the drift is worth a durable trace, because "the repository moved
		// between work completion and review dispatch" is exactly the fact a
		// human debugging this run will want and could not otherwise recover.
		nextAction = "review_target_observed: workspace moved since work completed; reviewing its current state"
	}
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:                "wfc-" + c.newID(),
		WorkflowRunID:     run.ID,
		WorkflowStepID:    &stepID,
		ProjectID:         run.ProjectID,
		SessionID:         workCP.SessionID,
		Branch:            workCP.Branch,
		WorktreePath:      workCP.WorktreePath,
		HeadSHA:           obs.HeadSHA,
		FingerprintBefore: fallback,
		FingerprintAfter:  target,
		NextAction:        nextAction,
		DurablePhase:      reviewTargetDurablePhase,
		PayloadVersion:    "v1",
		RetryState:        string(stateJSON),
		CreatedAt:         c.clock(),
	}); err != nil {
		// The observation is sound but could not be made durable. Using it
		// anyway would mean a later retry of this same cycle re-observes and
		// possibly resolves a different target — the exact instability this
		// checkpoint exists to prevent. Fall back instead.
		return fallback
	}
	return target
}

// recordedReviewTarget returns the fingerprint already durably recorded for
// this review step's given cycle, if any.
func (c *Coordinator) recordedReviewTarget(ctx stdctx.Context, runID, reviewStepID string, cycleNumber int) (string, bool) {
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return "", false
	}
	var latest *domain.WorkflowCheckpoint
	for i := range checkpoints {
		cp := &checkpoints[i]
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != reviewStepID || cp.DurablePhase != reviewTargetDurablePhase {
			continue
		}
		var state struct {
			Cycle int `json:"cycle"`
		}
		if json.Unmarshal([]byte(cp.RetryState), &state) != nil || state.Cycle != cycleNumber {
			continue
		}
		if cp.FingerprintAfter == "" {
			continue
		}
		if latest == nil || cp.CreatedAt.After(latest.CreatedAt) {
			latest = cp
		}
	}
	if latest == nil {
		return "", false
	}
	return latest.FingerprintAfter, true
}

func (c *Coordinator) dispatchReviewFromPending(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	reviewStep domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry,
	workerSessionID, branch, worktreePath, baseSHA, targetSHA string,
	harness domain.ReviewerHarness,
) (domain.WorkflowStep, error) {
	now := c.clock()
	// Checkpoint 8P-E.13A.2: same fix as dispatchFromPending (dispatch.go) for
	// a run parked in needs_attention rather than Waiting — a reviewer that is
	// actually being launched is proof the run is moving again, so a stale,
	// non-human stop must not outlive it and report "needs attention" over a
	// running review.
	run = c.clearResolvedStop(ctx, run, "the review step dispatched successfully")
	// Checkpoint 8N.1: same fix as dispatchFromPending (dispatch.go) — a
	// successful (non-waiting) review dispatch decision means capacity is
	// genuinely back, so a run parked in Waiting must move to Running here.
	if run.State == domain.WorkflowRunWaiting {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, domain.WorkflowRunWaiting, domain.WorkflowRunRunning, now); err != nil {
			return reviewStep, err
		}
		run.State = domain.WorkflowRunRunning
	}
	if _, err := c.store.UpdateWorkflowOutboxStatus(ctx, entry.ID, domain.WorkflowOutboxPending, domain.WorkflowOutboxDispatched, now, ""); err != nil {
		return reviewStep, err
	}
	// Keep the in-memory entry in sync with the CAS update above — the same
	// fix 8B made after discovering the original bug (dispatch.go): the
	// success/failure recorders below use entry.Status as the *expected*
	// value for their own CAS call, and a stale "pending" here would
	// silently no-op against the DB row (already "dispatched"), leaving the
	// outbox permanently stuck instead of advancing to acknowledged/failed.
	entry.Status = domain.WorkflowOutboxDispatched
	// ready->running (cycle 1, after the pending->ready unblock above) and
	// waiting->running (cycle N+1, Checkpoint 8D) are both valid transitions.
	if reviewStep.State == domain.WorkflowStepReady || reviewStep.State == domain.WorkflowStepWaiting {
		from := reviewStep.State
		if _, err := c.store.UpdateWorkflowStepState(ctx, reviewStep.ID, from, domain.WorkflowStepRunning, now); err != nil {
			return reviewStep, err
		}
		reviewStep.State = domain.WorkflowStepRunning
	}

	sessionID := domain.SessionID(workerSessionID)
	reviewRow, err := c.ensureReviewRow(ctx, sessionID, domain.ProjectID(run.ProjectID), harness)
	if err != nil {
		return c.recordReviewDispatchFailure(ctx, run, reviewStep, entry, domain.WorkflowErrorReviewerLaunchFailed, err)
	}

	reviewRunID := c.newID()
	reviewRun := domain.ReviewRun{
		ID:            reviewRunID,
		ReviewID:      reviewRow.ID,
		SessionID:     sessionID,
		BatchID:       c.newID(),
		Harness:       harness,
		TriggerSource: domain.ReviewTriggerManual,
		PRURL:         "",
		TargetSHA:     targetSHA,
		Status:        domain.ReviewRunRunning,
		Verdict:       domain.VerdictNone,
		CreatedAt:     now,
		// AutoInjectReview is deliberately false, never the session's live
		// policy default. internal/service/review.Service's delivery path
		// (deliverSubmitted -> lifecycle.ApplyReviewBatch) auto-injects a
		// changes_requested finding back into the WORKER's agent session
		// when a review_run.AutoInjectReview snapshot is true — that is
		// exactly the "Codex receives a new prompt" side effect Checkpoint
		// 8C forbids by construction (§9: no fix loop). Engine.Trigger
		// snapshots the *session's* live AutoInjectReview flag (which
		// defaults to true at spawn, session_manager's seedRecord) onto
		// every ReviewRun it creates; workflow does not reuse that snapshot
		// because doing so would silently reopen the exact loop this
		// checkpoint must not build. Recording next_action: "fix" (see
		// review_progress.go) is the ONLY effect of a changes_requested
		// verdict in 8C.
		AutoInjectReview: false,
	}
	if err := c.reviewRuns.InsertReviewRun(ctx, reviewRun); err != nil {
		if errors.Is(err, domain.ErrDuplicateReviewRun) {
			existing, ok, getErr := c.reviewRuns.GetReviewRunBySessionPRSHAAndHarness(ctx, sessionID, "", targetSHA, harness)
			if getErr != nil {
				return c.recordReviewDispatchFailure(ctx, run, reviewStep, entry, domain.WorkflowErrorReviewerLaunchFailed, getErr)
			}
			if ok {
				return c.recordReviewDispatchSuccess(ctx, run, reviewStep, entry, existing, "")
			}
		}
		return c.recordReviewDispatchFailure(ctx, run, reviewStep, entry, domain.WorkflowErrorReviewerLaunchFailed, err)
	}

	// The reviewer is told exactly what this task owns and what it does not.
	// Before this, it received run.Objective under the heading "objective of the
	// overall run" and no acceptance criteria at all, and so rejected a
	// correctly-scoped child task for work the plan had assigned to later tasks
	// — every cycle, until the fix budget ran out. See ReviewTaskScope.
	scope := c.reviewScopeForRun(ctx, run)
	prompt := BuildReviewPrompt(ReviewPromptInput{
		Objective:             run.Objective,
		AcceptanceCriteria:    scope.AcceptanceCriteria,
		AvailableDependencies: scope.AvailableDependencies,
		FuturePlannedTasks:    scope.FuturePlannedTasks,
		WorkerSessionID:       workerSessionID,
		Branch:                branch,
		WorktreePath:          worktreePath,
		BaseSHA:               baseSHA,
		HeadSHA:               targetSHA,
		ReviewRunID:           reviewRunID,
	})

	if err := c.reviewerLauncher.Preflight(ctx, harness, worktreePath); err != nil {
		return c.recordReviewDispatchFailure(ctx, run, reviewStep, entry, domain.WorkflowErrorReviewerLaunchFailed, fmt.Errorf("reviewer preflight: %w", err))
	}
	runtimeEnv, _, _, err := c.resolveRuntimeEnv(ctx, run.ID, domain.AgentHarness(harness))
	if err != nil {
		return c.recordReviewDispatchFailure(ctx, run, reviewStep, entry, classifyProviderFailure(err).Class, err)
	}
	launch, err := c.reviewerLauncher.Launch(ctx, ReviewerLaunchRequest{
		Harness:         harness,
		WorkerSessionID: sessionID,
		ProjectID:       domain.ProjectID(run.ProjectID),
		ReviewID:        reviewRow.ID,
		RunID:           reviewRunID,
		WorkspacePath:   worktreePath,
		Prompt:          prompt,
		RuntimeEnv:      runtimeEnv,
	})
	if err != nil {
		return c.recordReviewDispatchFailure(ctx, run, reviewStep, entry, domain.WorkflowErrorReviewerLaunchFailed, fmt.Errorf("launch reviewer: %w", err))
	}

	return c.recordReviewDispatchSuccess(ctx, run, reviewStep, entry, reviewRun, launch.HandleID)
}

// ensureReviewRow mirrors internal/review.Engine's unexported upsertReview
// composition exactly (review.go): look up the existing per-(session,
// harness) review row; if found, reuse its id/CreatedAt; otherwise mint a
// fresh row. No reviewer handle/agent-session id is known yet at this point
// (workflow always launches a brand-new pane per review step; there is no
// resumable prior pane the way a repeat internal/review.Trigger call has).
func (c *Coordinator) ensureReviewRow(ctx stdctx.Context, sessionID domain.SessionID, projectID domain.ProjectID, harness domain.ReviewerHarness) (domain.Review, error) {
	now := c.clock()
	existing, ok, err := c.reviewRuns.GetReviewBySessionAndHarness(ctx, sessionID, harness)
	if err != nil {
		return domain.Review{}, err
	}
	row := domain.Review{
		ID:        c.newID(),
		SessionID: sessionID,
		ProjectID: projectID,
		Harness:   harness,
		PRURL:     "",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if ok {
		row.ID = existing.ID
		row.CreatedAt = existing.CreatedAt
		row.ReviewerHandleID = existing.ReviewerHandleID
		row.AgentSessionID = existing.AgentSessionID
	}
	if err := c.reviewRuns.UpsertReview(ctx, row); err != nil {
		return domain.Review{}, err
	}
	return row, nil
}

// adoptReviewOrMarkAmbiguous handles a retry/recovery call that found the
// outbox entry already dispatched (or, defensively, acknowledged). It never
// launches a second reviewer. A natural-key match (session+harness+empty
// PR+target SHA) is real evidence and is adopted; anything else is surfaced
// as ambiguous rather than silently resolved ("nunca asumir éxito").
func (c *Coordinator) adoptReviewOrMarkAmbiguous(ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep, entry domain.WorkflowOutboxEntry, workerSessionID, targetSHA string, harness domain.ReviewerHarness) (domain.WorkflowStep, error) {
	existing, ok, err := c.reviewRuns.GetReviewRunBySessionPRSHAAndHarness(ctx, domain.SessionID(workerSessionID), "", targetSHA, harness)
	if err != nil {
		return reviewStep, err
	}
	if ok {
		return c.recordReviewDispatchSuccess(ctx, run, reviewStep, entry, existing, "")
	}
	return c.markReviewAmbiguous(ctx, run, reviewStep, "ambiguous_review_state: no review run found for dispatched command")
}

func (c *Coordinator) markReviewAmbiguous(ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep, nextAction string) (domain.WorkflowStep, error) {
	now := c.clock()
	if reviewStep.State == domain.WorkflowStepRunning || reviewStep.State == domain.WorkflowStepReady {
		from := reviewStep.State
		if _, err := c.store.UpdateWorkflowStepState(ctx, reviewStep.ID, from, domain.WorkflowStepWaiting, now); err != nil {
			return reviewStep, err
		}
		reviewStep.State = domain.WorkflowStepWaiting
	}
	if run.State == domain.WorkflowRunRunning || run.State == domain.WorkflowRunWaiting {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, now); err != nil {
			return reviewStep, err
		}
	}
	stepID := reviewStep.ID
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		NextAction:     nextAction,
		DurablePhase:   "review_dispatch_ambiguous",
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      now,
	}); err != nil {
		return reviewStep, err
	}
	// Outbox intentionally stays wherever it was: a human/future checkpoint
	// decides whether to clean up and retry.
	return reviewStep, nil
}

func (c *Coordinator) recordReviewDispatchSuccess(ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep, entry domain.WorkflowOutboxEntry, reviewRun domain.ReviewRun, reviewerHandleID string) (domain.WorkflowStep, error) {
	now := c.clock()

	if _, err := c.store.SetWorkflowStepReviewRun(ctx, reviewStep.ID, reviewRun.ID, now); err != nil {
		return reviewStep, err
	}
	rid := reviewRun.ID
	reviewStep.ReviewRunID = &rid

	if entry.Status != domain.WorkflowOutboxAcknowledged {
		if _, err := c.store.UpdateWorkflowOutboxStatus(ctx, entry.ID, entry.Status, domain.WorkflowOutboxAcknowledged, now, ""); err != nil {
			return reviewStep, err
		}
	}

	sid := string(reviewRun.SessionID)
	stepID := reviewStep.ID
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		SessionID:      &sid,
		HeadSHA:        reviewRun.TargetSHA,
		ReviewRunID:    &rid,
		ReviewVerdict:  "",
		NextAction:     "review_dispatched: awaiting verdict from review run " + reviewRun.ID,
		DurablePhase:   "review_dispatched",
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      now,
	}); err != nil {
		return reviewStep, err
	}
	return reviewStep, nil
}

func (c *Coordinator) recordReviewDispatchFailure(ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep, entry domain.WorkflowOutboxEntry, errClass domain.WorkflowErrorClass, cause error) (domain.WorkflowStep, error) {
	now := c.clock()
	if _, err := c.store.UpdateWorkflowOutboxStatus(ctx, entry.ID, entry.Status, domain.WorkflowOutboxFailed, now, string(errClass)); err != nil {
		return reviewStep, err
	}
	if reviewStep.State == domain.WorkflowStepRunning {
		if _, err := c.store.UpdateWorkflowStepState(ctx, reviewStep.ID, domain.WorkflowStepRunning, domain.WorkflowStepFailed, now); err != nil {
			return reviewStep, err
		}
		reviewStep.State = domain.WorkflowStepFailed
	}
	if run.State == domain.WorkflowRunRunning || run.State == domain.WorkflowRunWaiting {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, now); err != nil {
			return reviewStep, err
		}
	}
	if c.log != nil {
		c.log.Warn("workflow: review step dispatch failed", "step", reviewStep.ID, "err", cause)
	}
	return reviewStep, nil
}
