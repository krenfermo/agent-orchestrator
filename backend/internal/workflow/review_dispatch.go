package workflow

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
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

	// UpdateReviewRunResult closes out ONE review run by id, CAS-guarded in SQL
	// on status='running' (review.sql). review_launch_recovery.go uses it for
	// the one case that has no other honest ending: a review_run inserted for a
	// reviewer whose launch then failed before any reviewer session existed.
	// Same store method internal/review's own submit path already uses; the
	// status written here is "failed", which migration 0014 deliberately
	// excludes from the (session_id, target_sha) unique index so a retry of the
	// same target can insert a fresh run instead of adopting the dead one.
	UpdateReviewRunResult(ctx stdctx.Context, id string, status domain.ReviewRunStatus, verdict domain.ReviewVerdict, body, githubReviewID string, autoInjectReview bool) (bool, error)

	// MarkReviewRunSupersededBy names the replacement that took authority over
	// a closed-out run (migration 0135). It is what makes "which review speaks
	// for this step" answerable from durable state after a restart, and it is
	// what stops a late verdict on a replaced run from ever being adopted.
	MarkReviewRunSupersededBy(ctx stdctx.Context, id, supersededBy string) (bool, error)
}

// ReviewerPresence is what a probe of a deterministic reviewer identity found.
//
// The distinction that matters is not present/absent but PROVEN. Only
// ReviewerPresenceOwned licenses adoption or termination; everything else is
// either a licence to launch (proven absent) or a reason to do nothing at all.
type ReviewerPresence string

const (
	// ReviewerPresenceAbsent is proof that nothing is there. The only answer
	// that licenses a launch.
	ReviewerPresenceAbsent ReviewerPresence = "absent"
	// ReviewerPresenceOwned is proof that the reviewer AO launched for this
	// identity is there. The only answer that licenses adoption or termination.
	ReviewerPresenceOwned ReviewerPresence = "owned"
	// ReviewerPresenceForeign is something at that identity which AO cannot
	// correlate to its own launch — a name collision, a stale shell, a session
	// from a previous life. Never adopted, never destroyed.
	ReviewerPresenceForeign ReviewerPresence = "foreign"
	// ReviewerPresenceUnknown is AO admitting it could not tell. It is not
	// absence, and converting it into one is precisely how a second reviewer
	// gets launched over a live one.
	ReviewerPresenceUnknown ReviewerPresence = "unknown"
	// ReviewerPresenceExited is AO's own reviewer, proven finished: the
	// ownership token matches, and the process behind it is gone while the
	// session lingers (a dead pane kept by remain-on-exit, a shell that outlived
	// its command).
	//
	// It is deliberately NOT `owned`: adopting it would record a confirmed
	// reviewer over a corpse and wait for a verdict that can never arrive. It is
	// deliberately NOT `absent` either: the name is still taken, so a launch
	// would collide. What it licenses is exactly one thing — cleanup — after
	// which the identity becomes genuinely absent and reusable.
	ReviewerPresenceExited ReviewerPresence = "exited"
)

// LicensesLaunch reports whether this verdict permits creating the reviewer.
func (p ReviewerPresence) LicensesLaunch() bool { return p == ReviewerPresenceAbsent }

// LicensesAdoption reports whether this verdict permits adopting the reviewer at
// that identity as a live one.
func (p ReviewerPresence) LicensesAdoption() bool { return p == ReviewerPresenceOwned }

// LicensesTermination reports whether this verdict permits destroying what is at
// that identity. Both answers that permit it are proofs of AO's own ownership;
// `foreign` and `unknown` never are.
func (p ReviewerPresence) LicensesTermination() bool {
	return p == ReviewerPresenceOwned || p == ReviewerPresenceExited
}

// ReviewerObservation is ONE coherent answer about a reviewer: what is there,
// and WHICH INCARNATION that answer is about.
//
// The two travel together because splitting them reopens the race they exist to
// close. A probe that returned only a presence, followed by a separate call to
// learn the instance, describes two moments: the session can exit and a stranger
// take its name in between, and AO would then persist the stranger's identity as
// the confirmation for its own reviewer. There is no ordering of two name-keyed
// calls that avoids this — only one call can.
type ReviewerObservation struct {
	Presence ReviewerPresence
	// InstanceID is the exact incarnation Presence describes. It is set whenever
	// the probe found something it could identify, and it is the ONLY value a
	// caller may persist for this observation.
	InstanceID string
}

// Ref rebuilds an exact reference from this observation.
func (o ReviewerObservation) Ref(handleID string) ReviewerRef {
	return ReviewerRef{HandleID: handleID, InstanceID: o.InstanceID}
}

// ReviewerRef addresses one reviewer with BOTH of its keys.
//
// HandleID is the deterministic name AO computes before any side effect. It is a
// DISCOVERY key and nothing more: a runtime session name is reusable the instant
// its holder exits, so a name that once meant AO's reviewer can later mean a
// stranger's session.
//
// InstanceID is the runtime's immutable identity for the exact incarnation AO
// launched, learned from the creation call and persisted with the launch
// confirmation. Once it exists it is the AUTHORITY key: every probe, every
// termination and every recovery pass must address it, because it is the only
// thing a replacement cannot answer to.
//
// A ref with an empty InstanceID is therefore a pre-launch ref — the only state
// in which resolving by name is legitimate.
type ReviewerRef struct {
	HandleID   string
	InstanceID string
}

// Known reports whether this ref carries the authority key.
func (r ReviewerRef) Known() bool { return r.InstanceID != "" }

// String renders the ref for logs and durable next-action sentences.
func (r ReviewerRef) String() string {
	if r.InstanceID == "" {
		return r.HandleID
	}
	return r.HandleID + " (" + r.InstanceID + ")"
}

// ReviewerEnsurer is the optional half of the reviewer launch boundary that
// makes a launch RECOVERABLE across process death.
//
// Launch on its own cannot be: it performs an external side effect and then
// returns, and a crash in between leaves AO unable to tell "the reviewer was
// never started" from "it was started and the confirmation was lost". Retrying
// on that uncertainty starts a second reviewer; refusing to retry strands the
// step. Neither is acceptable, and no amount of write-ordering in the caller
// fixes it — the ambiguity is in the interface.
//
// What removes it is a DETERMINISTIC external identity that AO can compute and
// persist BEFORE the side effect, and can then ask about afterwards:
//
//	ReviewerIdentity(req)   the handle Launch WILL use — pure, no side effect
//	ProbeReviewer(handle)   does that reviewer exist right now?
//	CancelReviewer(handle)  terminate it; idempotent
//
// With those, an uncertain replay probes instead of guessing: an existing
// reviewer is adopted, a missing one is launched under the same identity, and
// repeated calls converge on exactly one. The protocol becomes
// EnsureReviewer(identity) rather than CreateNewReviewer().
//
// Optional by type assertion, following this package's narrow-optional
// convention: a launcher that does not implement it keeps today's behaviour,
// and the protocol degrades to a BOUNDED incident on genuine uncertainty rather
// than to a guess.
type ReviewerEnsurer interface {
	// ReviewerIdentity returns the deterministic external handle this request
	// will launch under. It must be pure and stable: the same request must
	// always yield the same identity, on every process and after every restart.
	ReviewerIdentity(req ReviewerLaunchRequest) string
	// ProbeReviewer classifies what is at that identity right now.
	//
	// Four answers, not two, because adoption and destruction are only ever
	// licensed by ONE of them. A session that merely bears the right name proves
	// nothing: it may be a collision or a stale shell, and adopting it would
	// hand the step a reviewer that is not reviewing while destroying it would
	// kill something AO does not own.
	ProbeReviewer(ctx stdctx.Context, ref ReviewerRef) (ReviewerObservation, error)
	// CancelReviewer terminates the reviewer with that identity. It is
	// idempotent: cancelling one that is already gone is success, which is what
	// makes crash-interrupted cancellation safe to replay.
	CancelReviewer(ctx stdctx.Context, ref ReviewerRef) error
}

// reviewerEnsurer returns the launcher's ensure capability when it has one.
// reviewerEnsurer returns the launch boundary's recovery half.
//
// It is no longer a type assertion that can quietly fail: ReviewerLauncher
// requires ReviewerEnsurer, so any launcher the coordinator holds satisfies it
// by construction. The only false answer left is "no launcher is wired at all",
// which every caller already handles.
func (c *Coordinator) reviewerEnsurer() (ReviewerEnsurer, bool) {
	if c.reviewerLauncher == nil {
		return nil, false
	}
	return c.reviewerLauncher, true
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
	HandleID string
	// InstanceID is the runtime's immutable identity for the incarnation this
	// launch created. It is what makes the launch addressable after a restart,
	// so it is recorded with the confirmation and never re-derived from the name.
	InstanceID     string
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
	// ReviewerEnsurer is REQUIRED, not optional.
	//
	// It was optional-by-type-assertion, and that was a latent hazard rather
	// than flexibility: when the production launcher stopped satisfying it after
	// a signature change, everything still compiled, every test still passed
	// against a fake that did satisfy it, and deterministic crash recovery was
	// silently disabled in production alone. Requiring it here turns that entire
	// class of regression into a compile error.
	ReviewerEnsurer
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

// reviewCycleOf recovers the cycle number a dispatch claim belongs to, from the
// claim's own idempotency key.
//
// Recovery paths hold the outbox entry but not the cycle they were dispatched
// for, and the retry budget is per cycle — so reading it back out of the key is
// what lets a resumed recovery consult the same budget the original attempt did.
// An unparseable key yields 0, which matches no cycle's records and therefore
// grants no budget.
func reviewCycleOf(entry domain.WorkflowOutboxEntry) int {
	const marker = ":cycle"
	i := strings.Index(entry.IdempotencyKey, marker)
	if i < 0 {
		return 0
	}
	rest := entry.IdempotencyKey[i+len(marker):]
	end := strings.IndexByte(rest, ':')
	if end < 0 {
		end = len(rest)
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0
	}
	return n
}

// reviewDispatchIdempotencyKey is the durable identity of ONE authorized review
// question: which step is asking, which provider is answering, and — when the
// question was authorized by a fresh-review generation — which generation.
//
// The cycle number alone was not an identity. It is derived from how many
// review runs have COMPLETED for this session and harness, so it only advances
// when a review finishes. A newly authorized fresh-review generation therefore
// computed the same cycle as the one before it, found that cycle's outbox row
// already acknowledged, and adopted the review run it belonged to — answering a
// new question with an old answer.
//
// wf-04e8309d is the case: six completed cycles, so cycleNumber 7, and an
// acknowledged cycle7 row from an earlier dispatch. An exceptional third
// fresh-review generation was granted, consumed, and then silently served by
// the stale approval it existed to replace, re-emitting review_dispatched on
// every poll without ever launching a reviewer.
//
// So the generation joins the key, tagged with the PURPOSE that authorized it.
// The purpose matters because generations from different mechanisms are counted
// independently — an integration replay's attempt 1 and a verify recovery's
// generation 1 are different questions, and a bare number would collide them.
//
// Backward compatibility is exact: with no fresh-review generation in force the
// key is byte-for-byte what it always was, so every ordinary review cycle keeps
// its identity and every acknowledged row already on disk stays adoptable by
// the path that wrote it. Only a fresh generation — which could not previously
// get an identity of its own at all — gets the suffix.
// reviewReplacementIdempotencyKey is the single-flight identity of ONE
// replacement review, and it is deliberately independent of harness.
//
// The ordinary key is scoped by (cycle, harness). For a replacement that is
// wrong twice over: two concurrent dispatchers routing to different providers
// get two different keys, so both pass the outbox's single-flight guard, both
// launch a reviewer, and both try to take the step. Keying on the run being
// replaced instead gives one row per replacement whatever provider each
// dispatcher happens to pick — and it stays stable across retries, so a
// capacity wait re-enters the same row rather than minting a second one (which
// is why this is not simply the cycle key with the harness dropped: the cycle
// number itself restarts per harness).
func reviewReplacementIdempotencyKey(stepID, replacedRunID string) string {
	return "workflow-step-review-replacement:" + stepID + ":" + replacedRunID
}

func reviewDispatchIdempotencyKey(stepID string, cycleNumber int, harness domain.ReviewerHarness, purpose string, generation int) string {
	base := reviewStepOutboxIdempotencyKey(stepID, cycleNumber, harness)
	if generation <= 0 || strings.TrimSpace(purpose) == "" {
		return base
	}
	return base + ":fresh-" + purpose + strconv.Itoa(generation)
}

// completedReviewCycles returns the count of Claude-Code review_runs already
// created for a worker session — Checkpoint 8D's natural cycle counter,
// reusing review_run row cardinality as the iteration count rather than
// adding a new counter column (checkpoint brief, translated from the Spanish:
// "use attempts/review_runs/checkpoints to represent iterations, not fix1/
// fix2/fix3 columns"). Callers derive the
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
		if r.Harness != harness {
			continue
		}
		// A run closed out as "failed" is a reviewer that never produced
		// anything — today, only review_launch_recovery.go writes that status,
		// for a launch that failed before any reviewer session existed. Counting
		// it as a completed cycle would make every retry of the SAME cycle
		// compute a higher cycle number, hence a different outbox idempotency
		// key, hence a second outbox entry dispatching the same review. A failed
		// launch is not a review cycle; it is an attempt at one.
		if r.Status == domain.ReviewRunFailed {
			continue
		}
		count++
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
// humanResume marks the one call site a person (or the API's Continue) drives
// explicitly, as opposed to a poll or a boot reconcile. It is the licence to
// re-open a review cycle whose launch stopped permanently: AO never retries
// those by itself (an auth failure retried on every 2s poll is a loop, not a
// recovery), but a human who has just fixed the cause is entitled to one.
func (c *Coordinator) dispatchReviewStep(ctx stdctx.Context, run domain.WorkflowRun, workStep, fixStep, reviewStep domain.WorkflowStep, humanResume bool) (domain.WorkflowStep, error) {
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
	// freshPurpose/freshGeneration identify the authorized fresh-review
	// generation this dispatch serves, when there is one. They stay empty for an
	// ordinary cycle, which is what keeps its key unchanged.
	freshPurpose, freshGeneration := "", 0
	// replacementOf names the review this dispatch replaces, when it is one. It
	// is what makes the dispatch identity below harness-independent.
	replacementOf := ""
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
			// A bound pointer is not a finished protocol. The bind is the
			// SECOND-to-last act: the predecessor's supersession, the outbox
			// acknowledgement and the final phase record all come after it, and
			// a crash in that tail used to be invisible here — this early return
			// saw a pointer, concluded the dispatch was done, and left the
			// protocol permanently half-finished.
			//
			// Finalization is resumed from whichever durable phase it actually
			// reached, and launches nothing: every remaining act is bookkeeping
			// about a reviewer that already exists.
			return c.finalizeBoundReviewDispatch(ctx, run, reviewStep)
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
		// A launch failure AO has already declared permanent (or out of
		// automatic budget) owns this step until a person acts: without this
		// guard the fix-cycle branch below would re-attempt the same doomed
		// launch on every 2s GetRun poll.
		if rec, ok := c.latestReviewLaunchRecord(ctx, run.ID, reviewStep.ID); ok && !rec.dueForRetry(c.clock()) && !humanResume {
			return reviewStep, nil
		}
		// Checkpoint 8P-E.14D: a review step reopened by an authorized verify
		// recovery rests at "waiting" with an APPROVED review_run underneath it —
		// a resting state none of the branches below can recognize, because none
		// of them is looking for "the approval is fine, it is just older than the
		// code". The durable fresh-review request is the only fact that describes
		// it, and it is checked FIRST so a fix step left resting at "waiting" by an
		// earlier cycle cannot claim a dispatch that belongs to the recovery.
		//
		// Once served, this falls through: the ordinary fix-driven cycles below
		// stay reachable, which is exactly what makes a changes_requested verdict
		// on the fresh review continue into the existing loop instead of into a
		// second mechanism.
		freshReview, wantFreshReview := c.pendingFreshReview(ctx, run.ID, reviewStep.ID)
		if wantFreshReview && reviewStep.ReviewRunID != nil {
			existing, found, err := c.reviewRuns.GetReviewRun(ctx, *reviewStep.ReviewRunID)
			if err != nil {
				return reviewStep, err
			}
			// The step already points at a run other than the stale approval: this
			// generation's fresh review has been dispatched, and dispatching again
			// would be a second reviewer for one authorization.
			if found && existing.TargetSHA != freshReview.ApprovedFingerprint {
				wantFreshReview = false
			}
		}

		if wantFreshReview {
			// This generation's own dispatch identity. Without it a newly
			// authorized generation reuses the previous cycle's outbox row and
			// adopts the very review it was authorized to replace.
			freshPurpose, freshGeneration = freshReview.Purpose, freshReview.Generation
			// Start from the fingerprint verification actually found, and let
			// reviewTargetFingerprint re-observe and pin the live workspace for
			// this cycle exactly as every other first-target dispatch does — the
			// reviewer must read what is there, not what was there.
			targetSHA = freshReview.CurrentFingerprint
			baseSHA = freshReview.ApprovedFingerprint
			firstCycleTarget = true
		} else if fixStep.State == domain.WorkflowStepWaiting {
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
				// "Already reviewed by the step's current run" requires that
				// run to still SPEAK for this fingerprint. A run AO closed out
				// without a verdict does not, and treating it as if it did is
				// what stranded wf-756988ae: its review run was cancelled by the
				// stall path, the fix step happened to be resting at `waiting`
				// with the very fingerprint that run had been dispatched
				// against, and so this guard matched and returned — on every
				// poll, every wake and every boot, forever. The capacity-retry
				// branch written for exactly that state sits below this one and
				// was never reached.
				if ok && existing.TargetSHA == fixCP.FingerprintAfter && reviewRunStillSpeaks(existing) {
					return reviewStep, nil
				}
			}
			targetSHA = fixCP.FingerprintAfter
			baseSHA = fixCP.FingerprintBefore
		} else if rebind, pending := c.pendingReviewAuthorityRebind(ctx, run, reviewStep); pending {
			replacementOf = rebind.AbandonedRunID
			// Review-authority reconciliation released this step: its previous
			// review was closed out with no verdict, the pointer to it has been
			// cleared, and exactly one replacement is authorized
			// (review_authority.go). The authorization checkpoint is the only
			// fact that describes this resting state — the run it replaced is no
			// longer reachable from the step — and it carries the target so the
			// replacement reviews the same thing rather than drifting.
			//
			// Checked before the fix-cycle gate below because an authorized
			// rebind is about a review that never concluded, not about a new
			// fingerprint to review; letting the fix branch claim it would
			// silently turn a retry into a cycle.
			targetSHA = rebind.TargetSHA
			if targetSHA == "" {
				targetSHA = workCP.FingerprintAfter
			}
			if targetSHA == "" {
				targetSHA = workCP.HeadSHA
			}
			if targetSHA == "" {
				targetSHA = baseSHA
			}
		} else if rec, isLaunchRetry := c.latestReviewLaunchRecord(ctx, run.ID, reviewStep.ID); isLaunchRetry {
			// A reviewer launch failed before any reviewer session existed and
			// rested this step at "waiting" (review_launch_recovery.go). No
			// workspace change happened and no verdict exists, so neither the
			// fix-cycle gate above nor the cancelled-run gate below can see it —
			// the durable launch-failure record is the only fact that describes
			// this resting state, and it survives every intermediate routing/
			// target checkpoint a retry pass writes.
			//
			// A failure AO classified as retryable resumes automatically here
			// (the wake it scheduled lands in exactly this branch). One it
			// classified as permanent, or one that used up its automatic budget,
			// resumes only when a person asks for it.
			if !rec.dueForRetry(c.clock()) && !humanResume {
				return reviewStep, nil
			}
			// Re-review the SAME target the failed attempt was dispatched
			// against — a launch failure changed nothing about the workspace,
			// and a drifting target would break adoption of an in-flight
			// reviewer (see reviewTargetFingerprint).
			targetSHA = rec.TargetSHA
			if targetSHA == "" {
				targetSHA = workCP.FingerprintAfter
			}
			if targetSHA == "" {
				targetSHA = workCP.HeadSHA
			}
			if targetSHA == "" {
				targetSHA = baseSHA
			}
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
			// Cancelled AND silent. A cancelled run that produced a verdict
			// after the fact is not an unresolved capacity stall — its verdict
			// has been adopted and is already driving the cascade (a fix, or
			// verify). Retrying on status alone dispatched a second reviewer
			// over the SAME target while that fix was still in flight: a review
			// of work nobody had changed yet.
			if !ok || priorRun.Status != domain.ReviewRunCancelled || priorRun.HasEffectiveVerdict() {
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
	// An in-flight replacement is NEVER rerouted. Once an authorization has
	// persisted a harness, a review identity exists (or may exist) under it, and
	// routing again on today's capacity would recover the wrong provider —
	// creating a second review under harness B while harness A's reviewer is
	// still out there. Fresh routing is for dispatches that have not yet
	// established a durable identity; this is not one of them.
	if durable, found := c.inFlightReviewHarness(ctx, run, reviewStep); found && durable != harness {
		if c.log != nil {
			c.log.Info("workflow: recovering an in-flight replacement with its durable harness, not today's routing",
				"run", run.ID, "step", reviewStep.ID, "durable", durable, "routed", harness)
		}
		harness, ok = durable, true
	}
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
	// The dispatch identity, resolved once and used for BOTH the target pin and
	// the outbox key, so the two can never disagree about which question this is.
	dispatchKey := reviewDispatchIdempotencyKey(reviewStep.ID, cycleNumber, harness, freshPurpose, freshGeneration)
	if replacementOf != "" {
		dispatchKey = reviewReplacementIdempotencyKey(reviewStep.ID, replacementOf)
	}
	if firstCycleTarget {
		targetSHA = c.reviewTargetFingerprint(ctx, run, reviewStep, workCP, dispatchKey, cycleNumber, targetSHA)
	} else {
		// Every OTHER cycle pins the head too. The target itself must not be
		// re-observed here (a fix-driven cycle's target is the fingerprint the
		// fix step delivered, and adoption looks the in-flight review_run up by
		// it), but the commit that fingerprint names still has to be written
		// down — see pinReviewTargetHead.
		c.pinReviewTargetHead(ctx, run, reviewStep, workCP, dispatchKey, cycleNumber, targetSHA)
	}

	now := c.clock()
	entry, _, err := c.store.EnqueueWorkflowOutboxEntry(ctx, domain.WorkflowOutboxEntry{
		ID:             "wfo-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &reviewStep.ID,
		IdempotencyKey: dispatchKey,
		CommandType:    domain.WorkflowOutboxTriggerReview,
		Payload:        reviewPayloadJSON(workStep.ID, reviewStep.ID, sessionID, targetSHA, harness, cycleNumber),
		CreatedAt:      now,
	})
	if err != nil {
		return reviewStep, err
	}

	switch entry.Status {
	case domain.WorkflowOutboxPending:
		return c.dispatchReviewFromPending(ctx, run, reviewStep, entry, sessionID, branch, worktreePath, baseSHA, targetSHA, harness, cycleNumber)
	case domain.WorkflowOutboxDispatched, domain.WorkflowOutboxAcknowledged:
		// Dispatched: a previous attempt got at least as far as "about to
		// launch," but we don't durably know if the launch itself completed.
		return c.adoptReviewOrMarkAmbiguous(ctx, run, reviewStep, entry, sessionID, targetSHA, harness)
	case domain.WorkflowOutboxFailed:
		// Durably failed. Still no auto-retry — but a human-driven Continue on a
		// run stopped by a reviewer-launch failure re-opens this exact entry
		// (same idempotency key, no second row) rather than leaving the person
		// with a button that does nothing, which is what the original incident
		// left behind.
		//
		// The generation is observed HERE, with the snapshot this pass is acting
		// on, and carried into the resume — which may then act on that
		// generation and no other. A pass delayed after this point finds the
		// failure it saw superseded and no-ops, instead of promoting itself to
		// whatever failure is newest by then.
		if humanResume {
			if observed, ok := c.observeFailedReviewLaunchGeneration(ctx, run.ID, reviewStep.ID, entry); ok &&
				c.resumeReviewLaunchAfterFailure(ctx, run, reviewStep, entry, observed) {
				entry.Status = domain.WorkflowOutboxPending
				return c.dispatchReviewFromPending(ctx, run, reviewStep, entry, sessionID, branch, worktreePath, baseSHA, targetSHA, harness, cycleNumber)
			}
		}
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
func (c *Coordinator) reviewTargetFingerprint(ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep, workCP domain.WorkflowCheckpoint, dispatchKey string, cycleNumber int, fallback string) string {
	if recorded, ok := c.recordedReviewTarget(ctx, run.ID, reviewStep.ID, dispatchKey, cycleNumber); ok {
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
	stateJSON, _ := json.Marshal(map[string]any{"cycle": cycleNumber, "dispatchKey": dispatchKey})
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

// reviewTargetHeadDurablePhase records the commit a review cycle's target
// fingerprint names, for the cycles that do not pin the target themselves.
//
// reviewTargetFingerprint writes review_target_observed only for a FIRST-cycle
// dispatch — the one that is allowed to re-observe and pin the live workspace.
// Every later cycle inherits its target from the fix step's delivery
// observation, so before this phase existed no row anywhere said which commit
// that fingerprint was read at. approvedHeadSHA then fell back to the WORK
// step's completion commit, which is stale the moment any fix cycle commits.
//
// That is exactly what parked wf-a21d98aa: its third review cycle approved a
// fingerprint whose head was 095bf89f, AO resolved the approved commit to the
// first cycle's 77aad8d6, and verification concluded the branch had advanced
// past an approval when the branch had not moved since the approval at all.
//
// A separate phase rather than a second review_target_observed row: the latter
// is a target PIN that reviewTargetFingerprint/recordedReviewTarget arbitrate
// per dispatch identity, and adding rows to it from here would let a head
// observation answer a question about which fingerprint to review.
const reviewTargetHeadDurablePhase = "review_target_head_observed"

// pinReviewTargetHead durably binds this cycle's already-resolved target
// fingerprint to the commit it names, and writes NOTHING when it cannot prove
// the binding.
//
// The proof is the fingerprint itself: WorkspaceFingerprint hashes head_sha
// among its inputs, so a live observation whose fingerprint equals the target
// is an observation of exactly the state the target names, and its HeadSHA is
// that state's commit. An observation that hashes to anything else is a
// workspace that has already moved on, and it says nothing about the target —
// so no row is written, approvedHeadSHA answers "unknown", and every consumer
// of that answer refuses rather than guesses.
func (c *Coordinator) pinReviewTargetHead(ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep, workCP domain.WorkflowCheckpoint, dispatchKey string, cycleNumber int, targetSHA string) {
	if targetSHA == "" || c.workspaceFacts == nil || workCP.WorktreePath == "" || workCP.SessionID == nil || *workCP.SessionID == "" {
		return
	}
	if _, already := c.recordedReviewTargetHead(ctx, run.ID, reviewStep.ID, targetSHA); already {
		return
	}
	obs, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
		Path:      workCP.WorktreePath,
		Branch:    workCP.Branch,
		SessionID: domain.SessionID(*workCP.SessionID),
		ProjectID: domain.ProjectID(run.ProjectID),
	})
	if err != nil || obs.HeadSHA == "" || WorkspaceFingerprint(obs) != targetSHA {
		return
	}
	stepID := reviewStep.ID
	stateJSON, _ := json.Marshal(map[string]any{"cycle": cycleNumber, "dispatchKey": dispatchKey})
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:                "wfc-" + c.newID(),
		WorkflowRunID:     run.ID,
		WorkflowStepID:    &stepID,
		ProjectID:         run.ProjectID,
		SessionID:         workCP.SessionID,
		Branch:            workCP.Branch,
		WorktreePath:      workCP.WorktreePath,
		HeadSHA:           obs.HeadSHA,
		FingerprintBefore: targetSHA,
		FingerprintAfter:  targetSHA,
		NextAction: fmt.Sprintf("review_target_head_observed: review cycle %d reads %s, which is commit %s",
			cycleNumber, shortFingerprint(targetSHA), shortFingerprint(obs.HeadSHA)),
		DurablePhase:   reviewTargetHeadDurablePhase,
		PayloadVersion: "v1",
		RetryState:     string(stateJSON),
		CreatedAt:      c.clock(),
	}); err != nil && c.log != nil {
		c.log.Warn("workflow: pinning the review target's head failed", "run", run.ID, "step", reviewStep.ID, "err", err)
	}
}

// recordedReviewTargetHead reports the commit already durably bound to this
// review step's given target fingerprint, if any.
func (c *Coordinator) recordedReviewTargetHead(ctx stdctx.Context, runID, reviewStepID, targetSHA string) (string, bool) {
	if targetSHA == "" {
		return "", false
	}
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return "", false
	}
	for _, cp := range cps {
		if cp.DurablePhase != reviewTargetHeadDurablePhase || cp.HeadSHA == "" {
			continue
		}
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != reviewStepID {
			continue
		}
		if cp.FingerprintAfter == targetSHA {
			return cp.HeadSHA, true
		}
	}
	return "", false
}

// recordedReviewTarget returns the fingerprint already durably recorded for
// this review step's given cycle, if any.
func (c *Coordinator) recordedReviewTarget(ctx stdctx.Context, runID, reviewStepID, dispatchKey string, cycleNumber int) (string, bool) {
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
			Cycle       int    `json:"cycle"`
			DispatchKey string `json:"dispatchKey"`
		}
		if json.Unmarshal([]byte(cp.RetryState), &state) != nil {
			continue
		}
		// The target is pinned per DISPATCH IDENTITY, not per cycle. The cycle
		// number does not advance across fresh-review generations, so matching
		// on it alone handed a newly authorized generation the target pinned by
		// the previous one -- and adoption by (session, target, harness) then
		// bound the new question to the old review run. That is the second half
		// of the wf-04e8309d collision, one layer below the outbox key.
		//
		// Rows written before dispatch keys existed carry none, and are still
		// matched by cycle so historical runs keep resolving their own target.
		if state.DispatchKey != "" || dispatchKey != "" {
			if state.DispatchKey != dispatchKey {
				continue
			}
		} else if state.Cycle != cycleNumber {
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
	cycleNumber int,
) (domain.WorkflowStep, error) {
	// P1-C: runtime admission, in the PENDING branch only.
	//
	// The placement matters as much as the check. dispatchReviewStep is
	// re-entered on every pass over a run, including passes where this cycle's
	// launch already happened and its outbox row is `dispatched`. Asking for
	// capacity there would mint a fresh claim per pass -- the step's attempt
	// count grows, so the generation and therefore the claim key change -- and
	// a single review would silently accumulate slots until the run hit its own
	// per-workflow bound and parked itself. Here it is asked exactly once per
	// launch that is actually about to happen, which is the same discipline
	// dispatchFromPending follows.
	//
	// A refusal parks the run in Waiting under the reviewer-capacity wake and
	// leaves the outbox entry Pending, so the next pass re-derives the
	// identical dispatch key and tries again.
	capReq := c.reviewerCapacityRequest(run, reviewStep, entry.IdempotencyKey, cycleNumber)
	if admitted, cerr := c.acquireCapacity(ctx, capReq); cerr != nil {
		return reviewStep, cerr
	} else if !admitted {
		return c.markRunWaitingForCapacity(ctx, run, reviewStep)
	}

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
	// PHASE: AUTHORIZED. Everything this dispatch intends, durable BEFORE it
	// claims ownership — so a claim can always be reconstructed, and a crash
	// immediately after the CAS is recoverable rather than an escalation.
	//
	// The review-run id and the deterministic reviewer identity are both decided
	// HERE, before any of them exists, which is what lets an uncertain replay
	// probe for the reviewer instead of guessing whether one was started.
	plannedReviewRunID := c.newID()
	plannedIdentity := ""
	if ensurer, ok := c.reviewerEnsurer(); ok {
		plannedIdentity = ensurer.ReviewerIdentity(ReviewerLaunchRequest{
			Harness: harness, WorkerSessionID: domain.SessionID(workerSessionID),
			RunID: plannedReviewRunID,
		})
	}
	authorization := reviewLaunchPhaseRecord{
		ReviewRunID:     plannedReviewRunID,
		Harness:         string(harness),
		TargetSHA:       targetSHA,
		HandleID:        plannedIdentity,
		IdempotencyKey:  entry.IdempotencyKey,
		Predecessor:     c.predecessorForDispatch(ctx, run, reviewStep),
		ExpectedPointer: pointerOf(reviewStep),
	}
	// The authorization's id is this dispatch's CLAIM TOKEN. It is written
	// before the claim (an authorization with no claim is harmless; a claim with
	// no authorization is not reconstructable), and the claim below stamps it
	// onto the row — so every later transition can prove it still owns it.
	dispatchGeneration, aerr := c.recordReviewDispatchAuthorized(ctx, run, reviewStep, authorization)
	if aerr != nil {
		return reviewStep, aerr
	}

	// THE LAUNCH-OWNERSHIP GATE, and the only one there is.
	//
	// This CAS is what decides which caller may bring a reviewer into existence.
	// Its result used to be discarded, so a caller that LOST it went on to create
	// its own review run and launch its own reviewer with whatever harness it had
	// locally routed to — two reviewers over one authorization, which the
	// harness-independent key makes possible to detect and this makes impossible
	// to do.
	//
	// A loser launches nothing. It re-reads the step so the winner's binding is
	// what it returns, and leaves observation to pick the review up: the winner
	// inserts its review run (harness and all) BEFORE it launches, so by the time
	// anything can be launched the identity is already durable and adoptable.
	// The claim STAMPS ITS OWNER. Taking the row and recording who took it are
	// one statement, so a dispatched row can never be owned by nobody — and a
	// dispatch that is later released and reclaimed is a genuinely different
	// generation, which the stale holder can no longer act on.
	claimed, err := c.store.ClaimWorkflowOutboxDispatch(ctx, entry.ID, now, dispatchGeneration)
	if err != nil {
		return reviewStep, err
	}
	if !claimed {
		if c.log != nil {
			c.log.Warn("workflow: lost the review dispatch claim; not launching a second reviewer",
				"run", run.ID, "step", reviewStep.ID, "key", entry.IdempotencyKey, "harness", harness)
		}
		if fresh, ok, ferr := c.getWorkflowStep(ctx, run.ID, reviewStep.ID); ferr == nil && ok {
			return fresh, nil
		}
		return reviewStep, nil
	}
	// PHASE: LAUNCH CLAIMED. Written before anything is created, so a dispatched
	// outbox row whose dispatch crashed is distinguishable from one whose
	// provenance AO does not know. See review_launch_phases.go.
	// PHASE: ATTEMPT ALLOCATED — BEFORE the claim record, not after.
	//
	// A claim that is durable without an attempt behind it is a free retry: the
	// next pass sees a claimed generation, releases it, and nothing was spent.
	// Allocating first makes that state unreachable — every durable claim has an
	// attempt already on the ledger, and a crash before the claim costs an
	// attempt, which is the safe direction.
	//
	// It is also where the budget is enforced: a cycle with no attempts left is
	// never claimed at all.
	if _, aerr := c.allocateReviewLaunchAttempt(ctx, run, reviewStep, entry, cycleNumber); aerr != nil {
		if errors.Is(aerr, errReviewLaunchBudgetExhausted) {
			return c.markReviewAmbiguous(ctx, run, reviewStep, fmt.Sprintf(
				"ambiguous_review_state: this review cycle has spent all %d reviewer launch attempts; "+
					"a person needs to look before another is started", maxReviewerLaunchAttempts))
		}
		// The allocation could not be made durable, so nothing that depends on
		// it happens. The claim is left held; the next pass retries allocation.
		return reviewStep, aerr
	}
	if cerr := c.recordReviewLaunchClaimed(ctx, run, reviewStep, entry); cerr != nil {
		return reviewStep, cerr
	}
	// Keep the in-memory entry in sync with the CAS update above — the same
	// fix 8B made after discovering the original bug (dispatch.go): the
	// success/failure recorders below use entry.Status as the *expected*
	// value for their own CAS call, and a stale "pending" here would
	// silently no-op against the DB row (already "dispatched"), leaving the
	// outbox permanently stuck instead of advancing to acknowledged/failed.
	entry.Status = domain.WorkflowOutboxDispatched
	// ...and with the claim token it now holds, which every ownership-dependent
	// transition below (fail, release) must name back to be allowed to act.
	entry.DispatchGeneration = dispatchGeneration
	// ready->running (cycle 1, after the pending->ready unblock above),
	// waiting->running (cycle N+1, Checkpoint 8D) and waiting->running (a
	// launch-failure retry resting at waiting) are all valid transitions.
	if reviewStep.State == domain.WorkflowStepReady || reviewStep.State == domain.WorkflowStepWaiting {
		from := reviewStep.State
		moved, serr := c.store.UpdateWorkflowStepState(ctx, reviewStep.ID, from, domain.WorkflowStepRunning, now)
		if serr != nil {
			return reviewStep, serr
		}
		if !moved {
			// The step left the state this dispatch was authorized against —
			// cancelled, completed, or moved by another valid transition. Its
			// result used to be discarded and the cached step mutated to
			// `running` regardless, so a cancelled step still had a review row
			// created and a reviewer launched into it.
			//
			// Nothing is created and nothing is launched. The claim is released
			// so the outbox is not left permanently `dispatched` over work that
			// will never happen; a step that is genuinely still dispatchable will
			// be picked up by the next pass.
			return c.releaseReviewDispatchClaim(ctx, run, reviewStep, entry,
				"the review step left the state this dispatch was authorized against")
		}
		reviewStep.State = domain.WorkflowStepRunning
	}

	// PHASE: LAUNCH INTENT — written BEFORE the review identity is created.
	//
	// The reverse order leaves a review_run row saying `running` with no phase
	// record behind it, which recovery could only read as a legacy successful
	// dispatch: a reviewer nobody started, adopted forever. Recording the intent
	// first means the row can never exist without the marker that says what it
	// is, and the marker already names the deterministic reviewer identity.
	if ierr := c.recordReviewLaunchIntentPlanned(ctx, run, reviewStep, authorization); ierr != nil {
		return reviewStep, ierr
	}

	sessionID := domain.SessionID(workerSessionID)
	reviewRow, err := c.ensureReviewRow(ctx, sessionID, domain.ProjectID(run.ProjectID), harness)
	if err != nil {
		return c.recordReviewLaunchFailure(ctx, run, reviewStep, entry, harness, "", targetSHA, cycleNumber, reviewLaunchStageReviewRow, err)
	}

	reviewRunID := plannedReviewRunID
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
				return c.recordReviewLaunchFailure(ctx, run, reviewStep, entry, harness, "", targetSHA, cycleNumber, reviewLaunchStageReviewRun, getErr)
			}
			// A duplicate that is itself a dead launch attempt (failed, no
			// verdict) is not evidence of a running reviewer and must never be
			// adopted as one. In production migration 0014 already excludes
			// failed rows from the unique index, so this is belt-and-braces
			// against any other store that does not.
			if ok && existing.Status != domain.ReviewRunFailed {
				// Another dispatch already created this identity. Whether it
				// also LAUNCHED anything is a different question, and binding
				// here used to answer it with "yes" unconditionally — the same
				// defect as binding on a row with no launch record. The shared
				// gate decides.
				return c.adoptExistingReviewRun(ctx, run, reviewStep, entry, existing)
			}
		}
		return c.recordReviewLaunchFailure(ctx, run, reviewStep, entry, harness, "", targetSHA, cycleNumber, reviewLaunchStageReviewRun, err)
	}

	// PHASE: REVIEW IDENTITY DURABLE, NOTHING LAUNCHED.
	//
	// Recorded before the preflight and the launch below, and it is what makes
	// the next crash window recoverable. A review_run row with status=running is
	// NOT proof that a reviewer exists: this dispatch creates the row first and
	// launches second, so between those two writes the row describes an intent
	// and nothing more. Recovery that reads the row alone adopts a reviewer
	// nobody started; recovery that reads THIS boundary knows the difference.
	// The reviewer is told exactly what this task owns and what it does not.
	// Before this, it received run.Objective under the heading "objective of the
	// overall run" and no acceptance criteria at all, and so rejected a
	// correctly-scoped child task for work the plan had assigned to later tasks
	// — every cycle, until the fix budget ran out. See ReviewTaskScope.
	scope := c.reviewScopeForRun(ctx, run)
	prompt := BuildReviewPrompt(ReviewPromptInput{
		Objective:             run.Objective,
		AcceptanceCriteria:    scope.AcceptanceCriteria,
		EffectiveSpec:         RenderEffectiveSpecification(c.effectiveTaskSpecification(ctx, run, scope.AcceptanceCriteria)),
		AvailableDependencies: scope.AvailableDependencies,
		FuturePlannedTasks:    scope.FuturePlannedTasks,
		WorkerSessionID:       workerSessionID,
		Branch:                branch,
		WorktreePath:          worktreePath,
		BaseSHA:               baseSHA,
		HeadSHA:               targetSHA,
		ReviewRunID:           reviewRunID,
	})

	// Every failure from here on has already inserted a review_run: reviewRunID
	// is handed to the recorder so that partial durable state is closed out
	// rather than left "running" with no reviewer behind it.
	// PHASE: READY TO LAUNCH — the last durable check before the irreversible act.
	//
	// Everything above ran on state read some calls ago. A cancellation, a
	// terminal transition or another owner can have won since, and the external
	// launch is the one act AO cannot take back. So the facts are re-read from
	// the store, never from the cached values, and a launch is refused unless all
	// of them still hold.
	if ok, why := c.reviewLaunchStillAuthorized(ctx, run.ID, reviewStep.ID, entry, authorization); !ok {
		if c.log != nil {
			c.log.Warn("workflow: refusing to launch a reviewer whose authorization no longer holds",
				"run", run.ID, "step", reviewStep.ID, "reviewRun", reviewRunID, "why", why)
		}
		// Through the SAME protocol as every other unlaunched cleanup. This
		// path used to mark the row failed and release the claim directly, and
		// a crash between those two writes left the one state nothing could
		// attribute: failed row, dispatched outbox, no marker. There is exactly
		// one way to close out an unlaunched review run.
		return c.abandonUnlaunchedReviewRun(ctx, run, reviewStep, entry, reviewRunID,
			"review_dispatch: "+why, why)
	}

	if err := c.reviewerLauncher.Preflight(ctx, harness, worktreePath); err != nil {
		return c.recordReviewLaunchFailure(ctx, run, reviewStep, entry, harness, reviewRunID, targetSHA, cycleNumber, reviewLaunchStagePreflight, fmt.Errorf("reviewer preflight: %w", err))
	}
	runtimeEnv, _, _, err := c.resolveRuntimeEnv(ctx, run.ID, domain.AgentHarness(harness))
	if err != nil {
		return c.recordReviewLaunchFailure(ctx, run, reviewStep, entry, harness, reviewRunID, targetSHA, cycleNumber, reviewLaunchStageRuntimeEnv, err)
	}
	launchReq := ReviewerLaunchRequest{
		Harness:         harness,
		WorkerSessionID: sessionID,
		ProjectID:       domain.ProjectID(run.ProjectID),
		ReviewID:        reviewRow.ID,
		RunID:           reviewRunID,
		WorkspacePath:   worktreePath,
		Prompt:          prompt,
		RuntimeEnv:      runtimeEnv,
	}
	// P2-C §7: a Reviewer is entitled to exactly what the Worker it reviews was
	// entitled to. Reviewing a change against knowledge the author did not have
	// is how a review reports a "regression" that is actually a decision the
	// project made and the reviewer was never told about.
	//
	// P2-D section 17: and the commit it is about to judge travels with it, so
	// the manifest recording what this reviewer was told can be compared
	// against what it actually reviewed.
	launch, adopted, err := c.ensureReviewerLaunched(
		projectmemory.WithRoleHead(c.withTaskAuthority(ctx, run), targetSHA),
		launchReq, authorization.HandleID)
	if err != nil {
		return c.recordReviewLaunchFailure(ctx, run, reviewStep, entry, harness, reviewRunID, targetSHA, cycleNumber, reviewLaunchStageLaunch, fmt.Errorf("launch reviewer: %w", err))
	}

	// PHASE: EXTERNAL LAUNCH CONFIRMED.
	//
	// The very first write after Launch returns, before the supersession and the
	// pointer bind below, so the window in which a live reviewer exists with no
	// durable trace of it is exactly one write wide. It carries the runtime
	// handle, which is what lets a later pass ADOPT this reviewer instead of
	// starting a second one.
	if adopted && c.log != nil {
		c.log.Info("workflow: adopted an existing reviewer under its deterministic identity instead of launching a second one",
			"run", run.ID, "step", reviewStep.ID, "reviewRun", reviewRun.ID, "handle", launch.HandleID)
	}
	launchedRef := ReviewerRef{HandleID: launch.HandleID, InstanceID: launch.InstanceID}
	if rerr := c.recordReviewLaunchConfirmed(ctx, run, reviewStep, reviewRun, launchedRef, targetSHA); rerr != nil {
		if errors.Is(rerr, errReviewerInstanceUnproven) {
			// A reviewer was started and the launcher could not say WHICH
			// session it is. Recording that would be a confirmation a stranger
			// could later answer for, so it is refused — and the reviewer is a
			// live thing AO cannot address, which is a launch failure with an
			// external obligation attached, not a clean retry. The bounded
			// launch-failure path closes out the review run and carries this to
			// a person rather than silently trying again.
			return c.recordReviewLaunchFailure(ctx, run, reviewStep, entry, harness, reviewRunID, targetSHA,
				cycleNumber, reviewLaunchStageLaunch, rerr)
		}
		return reviewStep, rerr
	}
	return c.recordReviewDispatchSuccess(ctx, run, reviewStep, entry, reviewRun, launchedRef)
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
	if !ok {
		// A dispatched command with no review run at all. If AO recorded the
		// claim, this is its own dispatch crashing between claiming the launch
		// and creating the identity: nothing exists, nothing was launched, and
		// giving the claim back is the one recovery that produces neither a
		// phantom nor a permanently dispatched outbox.
		//
		// Without that record the row's provenance is unknown, and the
		// conservative reading this path has always taken still stands.
		if c.reviewLaunchWasClaimedBy(ctx, run.ID, reviewStep.ID, entry.IdempotencyKey) {
			// releaseReviewDispatchClaim is the single choke point for
			// dispatched -> pending and applies the retry budget itself.
			return c.releaseReviewDispatchClaim(ctx, run, reviewStep, entry,
				"the dispatched command created no review run before crashing")
		}
		return c.markReviewAmbiguous(ctx, run, reviewStep,
			"ambiguous_review_state: no review run found for dispatched command")
	}
	// A run closed out as "failed" is the durable record of a reviewer that
	// never launched (review_launch_recovery.go), so it is not evidence that
	// this dispatched command succeeded — adopting it would report a reviewer
	// nobody started ("nunca asumir éxito").
	if existing.Status == domain.ReviewRunFailed {
		// AN INTERRUPTED ABANDON IS NOT AN AMBIGUITY.
		//
		// Abandoning an unlaunched identity marks the run failed and then gives
		// the claim back. A crash in between leaves exactly this pair — failed
		// row, dispatched outbox — and reading it as ambiguity stranded the run
		// forever: no reviewer existed, and nothing would ever launch one.
		//
		// The abandon marker is what tells the two apart. It is durable proof
		// that THIS failure came from the unlaunched/absent path and that its
		// claim still needs releasing; a failed row from any other cause has no
		// such marker and keeps the conservative ambiguity it always had.
		abandoned, merr := c.reviewLaunchAbandonMarker(
			ctx, run.ID, reviewStep.ID, existing.ID, entry.IdempotencyKey)
		if merr != nil {
			// FAIL CLOSED. AO could not read the ledger, so it has no proof —
			// and "cannot read proof" must never become "proof exists". Nothing
			// is released, nothing is relaunched, and the uncertainty is what
			// gets recorded.
			return c.markReviewAmbiguous(ctx, run, reviewStep,
				"ambiguous_review_state: a failed review run's recovery evidence could not be read")
		}
		if abandoned {
			// The marker names THIS claim, so finishing the interrupted abandon
			// releases only what that abandon was authorised to release.
			//
			// But finishing it means the step becomes dispatchable again, and a
			// crash that recurs in this window would otherwise walk past the
			// retry budget one attempt at a time — the interrupted attempt is
			// durably spent, yet the release hands out another launch. So the
			// budget is consulted here too: it is the same gate as on the
			// ordinary failure path, applied to the same durable history.
			return c.releaseReviewDispatchClaim(ctx, run, reviewStep, entry,
				"finishing an interrupted abandon of a review run that never had a reviewer")
		}
		// Either the failure came from somewhere else entirely, or the only
		// marker belongs to a DIFFERENT dispatch generation — a stale abandon
		// must not authorise releasing a newer claim.
		return c.markReviewAmbiguous(ctx, run, reviewStep,
			"ambiguous_review_state: the only review run for this dispatched command failed to launch")
	}

	// AND the row is still not proof on its own — see adoptExistingReviewRun.
	return c.adoptExistingReviewRun(ctx, run, reviewStep, entry, existing)
}

// adoptExistingReviewRun decides what an already-existing review_run is worth.
//
// It is the single gate between "a review_run row exists" and "this step has a
// reviewer", and every caller that finds a row goes through it. The row itself
// is never the answer: dispatch creates it BEFORE launching anything, so its
// existence is an intent, and only a durable launch record naming an exact
// runtime instance makes it authoritative.
// reviewLaunchBudgetRemains reports whether this claim may be released for
// another launch attempt.
//
// Every recovery that gives a claim back makes the step dispatchable again, so
// each of them spends from the same budget an ordinary failure does. Gating only
// the obvious path leaves the others as ways around it, one attempt per crash.
//
// The cycle comes from the durable attempt records for THIS claim, not from
// parsing the claim's key: a replacement key need not carry a cycle at all, and
// a cycle that parses to zero matches no history — so a spent budget would read
// as untouched. It FAILS CLOSED on history AO cannot read or decode.
func (c *Coordinator) reviewLaunchBudgetRemains(
	ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry,
) (bool, string, error) {
	history, err := c.reviewLaunchAttempts(ctx, run.ID, reviewStep.ID)
	if err != nil {
		return false, "ambiguous_review_state: the reviewer retry history could not be read", err
	}
	cycle, known := history.cycleOfClaim[entry.IdempotencyKey]
	if !known {
		// No attempt was ever recorded against this claim. Fall back to the
		// cycle the key names, which for an ordinary dispatch is the same value
		// and for a replacement grants nothing it should not.
		cycle = reviewCycleOf(entry)
	}
	// A CLAIMED GENERATION WITH NO ATTEMPT BEHIND IT IS NOT FREE BUDGET.
	//
	// Allocation now precedes the claim, so this state cannot be produced going
	// forward — but a ledger written by an older build can hold it, and reading
	// it as "nothing spent" is exactly the leak that ordering closed.
	if history.claimGeneration[entry.IdempotencyKey] > 0 &&
		history.attemptOfClaim[entry.IdempotencyKey] == 0 {
		return false, "ambiguous_review_state: this dispatch claim has no durable retry attempt behind it, " +
			"so AO cannot tell how much of its budget is already spent", nil
	}
	// Exhaustion is measured by the HIGHEST attempt as well as the count: a
	// gapped history reports fewer surviving records than the limit while the
	// highest number already reached it, and releasing on that hands out an
	// attempt past the budget. Gaps fail toward exhaustion.
	if history.highestIn(cycle) >= maxReviewerLaunchAttempts ||
		history.spentIn(cycle) >= maxReviewerLaunchAttempts {
		return false, fmt.Sprintf(
			"ambiguous_review_state: this review cycle has spent all %d reviewer launch attempts; "+
				"a person needs to look before another is started", maxReviewerLaunchAttempts), nil
	}
	return true, "", nil
}

func (c *Coordinator) adoptExistingReviewRun(
	ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry, existing domain.ReviewRun,
) (domain.WorkflowStep, error) {
	switch phase, ref := c.reviewLaunchPhaseFor(ctx, run.ID, reviewStep.ID, existing.ID); phase {
	case ReviewLaunchConfirmed:
		// A reviewer demonstrably exists. Adopt it — bind and acknowledge, never
		// launch a second one. The ref carries the durable INSTANCE, so this
		// adopts the incarnation AO launched rather than the name it used.
		return c.recordReviewDispatchSuccess(ctx, run, reviewStep, entry, existing, ref)
	case ReviewLaunchIntended:
		// An identity exists and no launch was ever confirmed. This is the one
		// state AO cannot resolve by reading its own records: the launch may
		// have happened and its confirmation been lost. So it ASKS — the intent
		// recorded the deterministic reviewer identity precisely so that this
		// question has an answer.
		if ensurer, ok := c.reviewerEnsurer(); ok && ref.HandleID != "" {
			obs, perr := ensurer.ProbeReviewer(ctx, ref)
			if perr != nil {
				obs = ReviewerObservation{Presence: ReviewerPresenceUnknown}
			}
			presence := obs.Presence
			switch {
			case presence.LicensesAdoption():
				// It was launched; only the confirmation was lost. Record the
				// confirmation now and adopt it — never a second reviewer.
				// The instance comes from THIS observation — the one that proved
				// ownership and liveness. A second look-up would be a second
				// moment, and a replacement arriving in between would be
				// persisted as this reviewer's confirmation.
				ref = obs.Ref(ref.HandleID)
				if rerr := c.recordReviewLaunchConfirmed(ctx, run, reviewStep, existing, ref, existing.TargetSHA); rerr != nil {
					if errors.Is(rerr, errReviewerInstanceUnproven) {
						// The reviewer is there, but AO cannot pin it to an
						// incarnation. Adopting on that would write a
						// confirmation a stranger could later answer for, so
						// nothing is written and nothing is bound: the durable
						// state stays exactly as it was and the next pass probes
						// again. If it stays unprovable, the step's own bounded
						// retries carry it to a person.
						return c.markReviewAmbiguous(ctx, run, reviewStep,
							"ambiguous_review_state: the reviewer at "+ref.String()+
								" is running but could not be pinned to an exact runtime instance")
					}
					return reviewStep, rerr
				}
				return c.recordReviewDispatchSuccess(ctx, run, reviewStep, entry, existing, ref)
			case presence == ReviewerPresenceExited:
				// Aimed at the incarnation this observation identified.
				ref = obs.Ref(ref.HandleID)
				// AO's OWN reviewer, proven finished, its session still holding
				// the identity. This is not ambiguity — it is a determined
				// outcome — so it must not become a person's problem. The
				// session is reclaimed (a destruction licensed by proof of
				// ownership) and the identity is closed out below exactly as a
				// proven-absent one is, freeing the name for a clean retry.
				if cerr := ensurer.CancelReviewer(ctx, ref); cerr != nil {
					return c.markReviewAmbiguous(ctx, run, reviewStep,
						"ambiguous_review_state: the finished reviewer at "+ref.String()+
							" could not be reclaimed, so its identity cannot be reused")
				}
			case presence.LicensesLaunch():
				// Proven absent: nothing was launched, so closing this identity
				// out and retrying cannot duplicate anything.
			default:
				// AO could not tell. Launching would risk a second reviewer and
				// closing out would risk orphaning a live one, so this is the
				// bounded incident the protocol reserves for genuinely
				// undeterminable external truth.
				return c.markReviewAmbiguous(ctx, run, reviewStep,
					"ambiguous_review_state: the reviewer at "+ref.String()+" is "+string(presence)+
						", which licenses neither adoption nor a relaunch")
			}
		}
		// No reviewer exists (or no probe is available). Close the identity out
		// as `failed` — precisely what that status means here, and what
		// migration 0014 excludes from the unique index — then give the claim
		// back so exactly one launch results from the retry.
		return c.abandonUnlaunchedReviewRun(ctx, run, reviewStep, entry, existing.ID,
			"review_launch_intent: no reviewer launch was ever recorded for this review run",
			"the review run was created but no reviewer launch was ever recorded")
	case ReviewLaunchUnprovenIdentity:
		// A launch marker exists but names no incarnation — a legacy
		// `review_dispatched`, or a confirmation from before instance ids.
		//
		// This used to be read as success, and that is the whole defect: after
		// the reviewer exits and a stranger takes its name, "adopt by name"
		// adopts the stranger while believing it holds proof. The record is
		// upgraded by a coherent probe or it confers nothing.
		upgraded, presence, uerr := c.upgradeLegacyLaunchIdentity(ctx, run, reviewStep, existing, ref)
		if uerr != nil {
			return reviewStep, uerr
		}
		if upgraded.Known() {
			// Proven live and pinned. From here it is an ordinary confirmed
			// reviewer, addressed by instance.
			return c.recordReviewDispatchSuccess(ctx, run, reviewStep, entry, existing, upgraded)
		}
		if presence.LicensesLaunch() {
			// Proven absent: whatever this record described is gone, so closing
			// the identity out and retrying cannot duplicate anything.
			return c.abandonUnlaunchedReviewRun(ctx, run, reviewStep, entry, existing.ID,
				"review_launch_legacy: the reviewer this record described is gone",
				"a name-only launch record described a reviewer that no longer exists")
		}
		// foreign / unknown / exited-but-unpinnable: no adoption, no
		// destruction, no relaunch over something that might be live.
		return c.markReviewAmbiguous(ctx, run, reviewStep,
			"ambiguous_review_state: the reviewer recorded for "+ref.String()+
				" could not be pinned to an exact runtime instance ("+string(presence)+")")
	default:
		// ReviewLaunchNone: NOTHING was ever recorded for this run.
		//
		// A review_run row is created BEFORE the launch, so its existence is an
		// intent and never a proof — and this branch used to bind on it anyway,
		// making a reviewer that may never have started this step's authority
		// forever. The row is closed out and the claim released, so the launch
		// protocol resumes and produces exactly one reviewer.
		//
		// A legacy row whose deterministic identity is still recoverable is
		// handled above; one with no trace at all cannot be probed, so relaunch
		// is the only move that ends with a reviewer, and closing the row out
		// first is what keeps it to one.
		return c.abandonUnlaunchedReviewRun(ctx, run, reviewStep, entry, existing.ID,
			"review_launch_none: no launch was ever recorded for this review run",
			"a review run exists with no record that any reviewer was ever launched")
	}
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

func (c *Coordinator) recordReviewDispatchSuccess(ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep, entry domain.WorkflowOutboxEntry, reviewRun domain.ReviewRun, reviewerRef ReviewerRef) (domain.WorkflowStep, error) {
	now := c.clock()

	// A reviewer is genuinely attached now, so any stop this run was parked on
	// because a previous launch failed is stale by proof, not by assumption.
	// Only reviewer-launch reasons are cleared (review_launch_recovery.go).
	run = c.clearReviewLaunchStop(ctx, run)

	// The replacement takes authority here, and the run it replaces is told so
	// durably BEFORE the step is rebound. Order matters: a crash between the two
	// leaves a run marked superseded by a replacement the step has not adopted
	// yet, which reconciliation reads as "not authoritative" and retries — the
	// safe direction. The reverse order would leave a replaced run looking
	// authoritative with the step already pointing elsewhere.
	// Two different facts, and conflating them is a bug: what the pointer
	// CURRENTLY holds (the CAS's expected value) and which run this replacement
	// SUPERSEDES. They diverge exactly when review-authority reconciliation has
	// already released the pointer — it is then unset, while the run being
	// replaced is named only by the rebind authorization.
	expectedPointer := ""
	if reviewStep.ReviewRunID != nil {
		expectedPointer = *reviewStep.ReviewRunID
	}
	predecessor := expectedPointer
	if predecessor == "" {
		// Review-authority reconciliation clears the pointer when it releases a
		// step (review_authority.go), so by the time a replacement binds, the
		// step no longer names what it replaced. The rebind authorization does,
		// and consulting it here is what keeps the durable authority chain
		// complete: every replaced run ends up naming its replacement, whichever
		// route the replacement arrived by.
		if rebind, pending := c.pendingReviewAuthorityRebind(ctx, run, reviewStep); pending {
			predecessor = rebind.AbandonedRunID
		}
	}
	// ORDER: BIND FIRST, SUPERSEDE ONLY IF THE BIND WON.
	//
	// The reverse order creates a state nothing can recover from: the
	// predecessor marked superseded (so its late verdict is stale and
	// unusable) while the rebind is refused (because that same late verdict
	// landed first) — leaving the replacement live, unbound, and the only valid
	// verdict in the run permanently ineffective.
	//
	// Binding first makes the authority transfer the single decision point. If
	// it loses, the predecessor keeps its authority and its verdict, and this
	// replacement is closed out below.
	bound, berr := c.store.RebindWorkflowStepReviewRunFrom(
		ctx, reviewStep.ID, expectedPointer, predecessor, reviewRun.ID, now)
	if berr != nil {
		return reviewStep, berr
	}
	if !bound {
		// Losing the swap is not automatically losing authority. Two recovery
		// callers can adopt the SAME confirmed reviewer, and the second one's
		// CAS fails against a pointer that already holds exactly what it was
		// trying to write. Treating that as a loss used to CANCEL the reviewer
		// the first caller had just successfully bound.
		if fresh, ok, ferr := c.getWorkflowStep(ctx, run.ID, reviewStep.ID); ferr == nil && ok {
			if pointerOf(fresh) == reviewRun.ID && !fresh.State.Terminal() {
				// The identical bind already happened. Continue finalizing —
				// idempotently, since every remaining act checks its own state.
				return c.finalizeBoundReviewDispatch(ctx, run, fresh)
			}
		}
		// The predecessor won — in practice by producing a late verdict while
		// this replacement was being launched. It keeps its authority and its
		// verdict must stay ADOPTABLE, which means the pointer has to name it
		// again: reconciliation released it in order to authorize this
		// replacement, and a refused replacement that left the pointer unset
		// would strand the very verdict that refused it.
		//
		// Restoring is a CAS from "unset", so it can only ever put back the
		// authority this dispatch was authorized to take; if someone else has
		// since claimed the step, it does nothing.
		if predecessor != "" && expectedPointer == "" {
			if _, rerr := c.store.RebindWorkflowStepReviewRunFrom(
				ctx, reviewStep.ID, "", "", predecessor, now); rerr != nil {
				return reviewStep, rerr
			}
		}
		// And this reviewer must not be left live and unowned. The EXTERNAL
		// reviewer is terminated, not merely the row: a row that says cancelled
		// while its process is still running is the orphan this protocol exists
		// to prevent.
		if xerr := c.cancelReviewerExternally(ctx, run, reviewStep, reviewRun.ID, reviewerRef,
			"the review it would have replaced concluded first"); xerr != nil {
			return reviewStep, xerr
		}
		if _, cerr := c.reviewRuns.UpdateReviewRunResult(ctx, reviewRun.ID,
			domain.ReviewRunCancelled, "", "review_authority: the review it would have replaced concluded first", "", false); cerr != nil {
			return reviewStep, cerr
		}
		if c.log != nil {
			c.log.Warn("workflow: replacement lost the authority bind; its reviewer is closed out",
				"run", run.ID, "step", reviewStep.ID, "replacement", reviewRun.ID, "predecessor", predecessor)
		}
		return reviewStep, nil
	}
	if predecessor != "" && predecessor != reviewRun.ID {
		superseded, serr := c.reviewRuns.MarkReviewRunSupersededBy(ctx, predecessor, reviewRun.ID)
		if serr != nil {
			return reviewStep, serr
		}
		if !superseded {
			// The CAS is write-once. Losing it means another replacement already
			// named itself the successor of this run — so this dispatcher is not
			// the owner, and must not go on to rebind the step or acknowledge the
			// command as though it were. Ignoring the result here is how two
			// replacements could both believe they had taken over.
			if c.log != nil {
				c.log.Warn("workflow: another replacement already superseded this review; abandoning this bind",
					"run", run.ID, "step", reviewStep.ID,
					"predecessor", predecessor, "attempted", reviewRun.ID)
			}
			return reviewStep, nil
		}
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

// recordReviewDispatchFailure used to be the single ending for every reviewer
// launch failure: outbox failed with the flat "reviewer_launch_failed" class,
// review step -> failed (terminal, unresumable), run -> needs_attention, the
// real error only in a log line, and any review_run this attempt had already
// inserted left at "running" forever. Every one of those is a bug the
// wf-6d290889 incident hit at once. recordReviewLaunchFailure
// (review_launch_recovery.go) replaces it for all launch-stage failures.
