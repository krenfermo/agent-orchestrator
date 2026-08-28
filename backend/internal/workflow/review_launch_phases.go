package workflow

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// review_launch_phases.go — the durable phases of ONE reviewer launch, and what
// each of them is entitled to claim.
//
// Review dispatch performs five durable acts in order, with an external side
// effect in the middle of them:
//
//	1. claim      the outbox row moves pending -> dispatched (launch ownership)
//	2. identity   InsertReviewRun creates the review_run row
//	3. LAUNCH     a reviewer process is started            <-- external
//	4. confirm    the launch is recorded, with its handle
//	5. bind       the step's authority pointer moves to this run
//
// The bug this file exists to remove: recovery could not tell (2) from (4).
// `adoptReviewOrMarkAmbiguous` read the review_run row and adopted anything that
// was not `failed` as a successful dispatch — so a crash between (2) and (3)
// left a row saying `running`, and the next boot reported a reviewer that had
// never been started. A phantom reviewer, permanently: nothing would ever launch
// one, and the step waited on it forever.
//
// A review_run row is therefore NOT proof of a launch. These two markers are:
//
//	review_launch_intent      an identity exists and NOTHING has been launched
//	review_launch_confirmed   a reviewer process exists, and this is its handle
//
// Both are ordinary ledger checkpoints, so they carry review_run_id in the
// column that already exists for it and need no schema change. Between them sits
// the one window that cannot be closed by ordering alone — a launch that
// happened and was not yet recorded — and it is exactly one write wide, with the
// handle recorded first thing afterwards so a later pass can adopt rather than
// relaunch.

// errReviewerPresenceUnproven is a launch refused because AO could not prove
// what is at the deterministic identity. It is a control signal: the caller
// leaves the durable state exactly as it is and tries again, because an
// unprovable identity that is retried is recoverable while a second reviewer
// launched over a live one is not.
var errReviewerPresenceUnproven = errors.New(
	"workflow: the reviewer at this identity could not be proven present or absent")

// errReviewerCapabilityMissing is a launch refused because the boundary cannot
// support a RECOVERABLE launch at all — no launcher, or no deterministic
// identity to address one by.
//
// It is deliberately an error rather than a fallback to a bare Launch. A
// reviewer started without an identity cannot be probed, adopted, or terminated
// after a crash, so the "degraded" path was not a lesser guarantee but the
// absence of every guarantee, taken silently.
// errReviewerInstanceUnproven is a confirmation refused because the reviewer
// could not be pinned to an exact runtime incarnation.
//
// It is a control signal, not a failure of the review: the durable state is
// left exactly as it was, and the next pass probes again. A launch that stays
// unconfirmed is recoverable; a confirmation that names only a reusable session
// is a permanent invitation to adopt a stranger.
var errReviewerInstanceUnproven = errors.New(
	"workflow: this reviewer could not be pinned to an exact runtime instance")

var errReviewerCapabilityMissing = errors.New(
	"workflow: a recoverable reviewer launch is not available at this boundary")

const (
	// reviewDispatchAuthorizedPhase is written BEFORE the outbox claim, and it
	// is what makes the claim reconstructable.
	//
	// The CAS that acquires launch ownership is one storage operation; writing
	// the metadata after it leaves a window in which the outbox says `dispatched`
	// and nothing on disk says who owns it or what they intended to launch —
	// recovery could only escalate. Writing it FIRST inverts that: an
	// authorization with no claim is harmless (nobody acted on it), while a claim
	// always has its authorization behind it.
	//
	// It carries everything restart needs: the replacement key, the predecessor,
	// the pointer this dispatch expected, the review-run id it will create, and
	// the deterministic reviewer identity it will launch under.
	reviewDispatchAuthorizedPhase = "review_dispatch_authorized"
	// reviewLaunchClaimedPhase marks that THIS dispatch won the outbox claim.
	// It is written first, before anything is created, and it is what tells a
	// dispatched outbox row whose dispatch crashed apart from one whose
	// provenance AO does not know (a row seeded before these markers existed, or
	// by anything other than this path). Only the former may be resumed
	// automatically; the latter keeps the conservative ambiguity it always had.
	reviewLaunchClaimedPhase = "review_launch_claimed"
	// reviewLaunchIntentPhase marks a review identity that exists with nothing
	// launched for it. It is written BEFORE the preflight and the launch.
	reviewLaunchIntentPhase = "review_launch_intent"
	// reviewLaunchConfirmedPhase marks a reviewer that demonstrably exists. It
	// is the FIRST write after the launcher returns, before the supersession
	// and the pointer bind, so the unrecorded-launch window is one write wide.
	reviewLaunchConfirmedPhase = "review_launch_confirmed"
	// reviewCancelIntentPhase / reviewCancelConfirmedPhase are the same
	// intent-act-confirm shape applied to TERMINATION.
	//
	// Marking a review_run `cancelled` in SQLite says nothing about the reviewer
	// process: a replacement that loses authority after it has already launched
	// leaves a live orphan, and a row that claims otherwise is a lie AO tells
	// itself. So the intent is durable first, the external kill happens against
	// the deterministic identity, and only a probe that finds it gone writes the
	// confirmation. A crash anywhere in that sequence replays idempotently,
	// because cancelling something already gone is success.
	reviewCancelIntentPhase    = "review_cancel_intent"
	reviewCancelConfirmedPhase = "review_cancel_confirmed"
)

// reviewLaunchPhaseRecord is what the launch markers carry.
type reviewLaunchPhaseRecord struct {
	ReviewRunID string `json:"reviewRunId"`
	Harness     string `json:"harness"`
	TargetSHA   string `json:"targetSha"`
	// HandleID is the DETERMINISTIC reviewer identity. It is present from the
	// intent onwards — not only on the confirmation — because its whole purpose
	// is to be knowable before the launch, so an uncertain replay can probe for
	// it instead of guessing.
	HandleID string `json:"handleId,omitempty"`
	// InstanceID is the runtime's immutable identity for the launched
	// incarnation. It is written with the CONFIRMATION (nothing exists to
	// identify before the launch), and from then on it is what recovery
	// addresses — a name alone would let a replacement answer in its place.
	InstanceID string `json:"instanceId,omitempty"`
	// IdempotencyKey is the replacement/dispatch identity this launch serves.
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	// Predecessor is the review this launch replaces, when it replaces one, and
	// ExpectedPointer is what the step's authority pointer held when this
	// dispatch was authorized. Together they let recovery finish a bind.
	Predecessor     string `json:"predecessor,omitempty"`
	ExpectedPointer string `json:"expectedPointer,omitempty"`
}

// recordReviewDispatchAuthorized persists the whole identity of a dispatch about
// to claim launch ownership — before the claim itself.
// It returns the checkpoint id it wrote, which IS this dispatch generation's
// claim token: the authorization is durable, it is written before the claim, and
// it is already the artifact ownership is reconstructed from — so nothing else
// needs to be invented to name the claim in SQL.
func (c *Coordinator) recordReviewDispatchAuthorized(
	ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep,
	rec reviewLaunchPhaseRecord,
) (string, error) {
	stepID := reviewStep.ID
	payload, _ := json.Marshal(rec)
	cp := domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		HeadSHA:        rec.TargetSHA,
		NextAction: fmt.Sprintf(
			"review_dispatch_authorized: review run %s will launch reviewer %s for %s",
			rec.ReviewRunID, orValue(rec.HandleID, "(no deterministic identity)"), rec.IdempotencyKey),
		DurablePhase:   reviewDispatchAuthorizedPhase,
		PayloadVersion: "v1",
		RetryState:     string(payload),
		CreatedAt:      c.clock(),
	}
	// The review_run_id COLUMN is deliberately left unset: it is a foreign key,
	// and this record is written before the row it would name exists. The
	// planned id travels in the payload, which is where recovery reads it —
	// naming a not-yet-created run in the column would make the whole protocol
	// unwritable.
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, cp); err != nil {
		return "", err
	}
	return cp.ID, nil
}

// latestReviewDispatchAuthorization returns the newest authorization recorded
// for this step under this dispatch key.
func (c *Coordinator) latestReviewDispatchAuthorization(
	ctx stdctx.Context, runID, stepID, idempotencyKey string,
) (reviewLaunchPhaseRecord, bool) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return reviewLaunchPhaseRecord{}, false
	}
	var newest reviewLaunchPhaseRecord
	found := false
	for _, cp := range cps {
		if cp.DurablePhase != reviewDispatchAuthorizedPhase ||
			cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID {
			continue
		}
		var rec reviewLaunchPhaseRecord
		if json.Unmarshal([]byte(cp.RetryState), &rec) != nil {
			continue
		}
		if idempotencyKey != "" && rec.IdempotencyKey != idempotencyKey {
			continue
		}
		newest, found = rec, true
	}
	return newest, found
}

func (c *Coordinator) recordReviewLaunchPhase(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	reviewStep domain.WorkflowStep,
	reviewRun domain.ReviewRun,
	phase, handleID, instanceID, targetSHA, nextAction string,
) error {
	stepID := reviewStep.ID
	runID := reviewRun.ID
	payload, _ := json.Marshal(reviewLaunchPhaseRecord{
		ReviewRunID: reviewRun.ID,
		Harness:     string(reviewRun.Harness),
		TargetSHA:   targetSHA,
		HandleID:    handleID,
		InstanceID:  instanceID,
	})
	_, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		ReviewRunID:    &runID,
		HeadSHA:        targetSHA,
		NextAction:     nextAction,
		DurablePhase:   phase,
		PayloadVersion: "v1",
		RetryState:     string(payload),
		CreatedAt:      c.clock(),
	})
	return err
}

// recordReviewLaunchClaimed records that this dispatch owns the launch. It runs
// before any identity exists, so it names no review run — the step and the
// dispatch key are the whole of its identity.
func (c *Coordinator) recordReviewLaunchClaimed(
	ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry,
) error {
	stepID := reviewStep.ID
	_, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		NextAction: fmt.Sprintf(
			"review_launch_claimed: this dispatch owns the reviewer launch for %s", entry.IdempotencyKey),
		DurablePhase:   reviewLaunchClaimedPhase,
		PayloadVersion: "v1",
		RetryState:     `{"idempotencyKey":` + jsonQuote(entry.IdempotencyKey) + `}`,
		CreatedAt:      c.clock(),
	})
	return err
}

// jsonQuote is json-safe quoting for the one short string above.
func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// reviewLaunchWasClaimedBy reports whether THIS dispatch path claimed the launch
// for this step under this key. False means the dispatched outbox row did not
// come from a claim AO recorded, so its provenance is unknown and it must not be
// resumed automatically.
func (c *Coordinator) reviewLaunchWasClaimedBy(
	ctx stdctx.Context, runID, stepID, idempotencyKey string,
) bool {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return false
	}
	for _, cp := range cps {
		if cp.DurablePhase != reviewLaunchClaimedPhase ||
			cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID {
			continue
		}
		var rec struct {
			IdempotencyKey string `json:"idempotencyKey"`
		}
		if json.Unmarshal([]byte(cp.RetryState), &rec) == nil && rec.IdempotencyKey == idempotencyKey {
			return true
		}
	}
	return false
}

// pointerOf is the step's current authority pointer, "" when unset.
func pointerOf(step domain.WorkflowStep) string {
	if step.ReviewRunID == nil {
		return ""
	}
	return *step.ReviewRunID
}

// predecessorForDispatch is the review this dispatch replaces, recorded at
// authorization time so finalization can still find it after a crash.
//
// The step's own pointer answers it — except in the one case that matters most:
// review-authority reconciliation RELEASES the pointer in order to authorize a
// replacement, so by the time the replacement is authorized the step names
// nothing and only the rebind record still knows what is being replaced.
// Recording an empty predecessor there is how a resumed finalization silently
// skipped the supersession.
func (c *Coordinator) predecessorForDispatch(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep,
) string {
	if p := pointerOf(step); p != "" {
		return p
	}
	if rebind, pending := c.pendingReviewAuthorityRebind(ctx, run, step); pending {
		return rebind.AbandonedRunID
	}
	return ""
}

// recordReviewLaunchIntentPlanned records that AO is about to create a review
// identity and launch a reviewer under a deterministic handle it has already
// decided. It is written BEFORE either exists.
func (c *Coordinator) recordReviewLaunchIntentPlanned(
	ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep,
	rec reviewLaunchPhaseRecord,
) error {
	stepID := reviewStep.ID
	payload, _ := json.Marshal(rec)
	// review_run_id stays unset for the same reason as the authorization above:
	// this is written before the row exists, and the column is a foreign key.
	_, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		HeadSHA:        rec.TargetSHA,
		NextAction: fmt.Sprintf(
			"review_launch_intent: about to create review run %s and launch reviewer %s",
			rec.ReviewRunID, orValue(rec.HandleID, "(no deterministic identity)")),
		DurablePhase:   reviewLaunchIntentPhase,
		PayloadVersion: "v1",
		RetryState:     string(payload),
		CreatedAt:      c.clock(),
	})
	return err
}

// recordReviewLaunchIntent records that a review identity exists and nothing has
// been launched for it. A failure here aborts the dispatch BEFORE the launch,
// which is the safe direction: an unrecorded identity with no reviewer behind it
// is recoverable, an unrecordable launch is not.
func (c *Coordinator) recordReviewLaunchIntent(
	ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep,
	reviewRun domain.ReviewRun, targetSHA string,
) error {
	return c.recordReviewLaunchPhase(ctx, run, reviewStep, reviewRun,
		reviewLaunchIntentPhase, "", "", targetSHA,
		fmt.Sprintf("review_launch_intent: review run %s created; no reviewer launched yet", reviewRun.ID))
}

// recordReviewLaunchConfirmed records that a reviewer process demonstrably
// exists, with the handle that identifies it.
func (c *Coordinator) recordReviewLaunchConfirmed(
	ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep,
	reviewRun domain.ReviewRun, ref ReviewerRef, targetSHA string,
) error {
	// A CONFIRMATION WITHOUT AN EXACT INSTANCE IS REFUSED.
	//
	// This record is what every later pass trusts: seeing it, recovery stops
	// probing and starts adopting. If it carries only the reusable name, then
	// after the reviewer exits and a stranger takes that name, recovery adopts
	// the stranger — and does so believing it has proof. A confirmation that
	// cannot name its incarnation is therefore not a weaker confirmation, it is
	// a false one, and it never reaches the ledger.
	//
	// The caller treats this as an unresolved launch: no bind, no adoption, and
	// a retry that will probe again rather than assume.
	if ref.InstanceID == "" {
		return fmt.Errorf(
			"%w: refusing to confirm reviewer %s for review run %s without an exact runtime instance",
			errReviewerInstanceUnproven, orValue(ref.HandleID, "(unnamed)"), reviewRun.ID)
	}
	// The INSTANCE is written here and nowhere earlier: nothing exists to
	// identify until the launch returns. From this record onwards every probe,
	// termination and recovery pass addresses the instance rather than the name,
	// which is what stops a replacement under the same name from answering for
	// this reviewer after a restart.
	return c.recordReviewLaunchPhase(ctx, run, reviewStep, reviewRun,
		reviewLaunchConfirmedPhase, ref.HandleID, ref.InstanceID, targetSHA,
		fmt.Sprintf("review_launch_confirmed: reviewer for review run %s is running (%s)",
			reviewRun.ID, orValue(ref.String(), "unrecorded")))
}

// ReviewLaunchPhase is the derived launch phase of one review run, read from
// durable records alone.
type ReviewLaunchPhase string

const (
	// ReviewLaunchNone means no launch phase has been recorded for this run:
	// either it predates these markers, or the crash landed before the identity
	// boundary was written.
	ReviewLaunchNone ReviewLaunchPhase = "none"
	// ReviewLaunchIntended means an identity exists and no launch is proven.
	// The one phase from which a review_run row must NOT be read as a reviewer.
	ReviewLaunchIntended ReviewLaunchPhase = "intended"
	// ReviewLaunchConfirmed means a reviewer demonstrably exists. The only
	// phase that licenses adoption.
	ReviewLaunchConfirmed ReviewLaunchPhase = "confirmed"
	// ReviewLaunchUnprovenIdentity means a launch marker EXISTS but names no
	// exact runtime incarnation.
	//
	// Two things land here: records written before instance ids existed, and any
	// confirmation that somehow reached the ledger without one. Both look like
	// success and neither is usable as one — a marker that names only a reusable
	// session name is an invitation to adopt whatever holds that name today.
	//
	// It is deliberately NOT folded into `confirmed`: the whole point is that a
	// caller cannot accidentally treat it as proof. It must be upgraded by a
	// coherent probe that yields an exact instance, or refused.
	ReviewLaunchUnprovenIdentity ReviewLaunchPhase = "unproven_identity"
)

// reviewLaunchPhaseFor derives the launch phase of one review run for one step,
// and returns the recorded reviewer reference when there is one.
//
// The ref carries the durable INSTANCE id whenever a confirmation recorded it,
// which is what lets recovery address the exact incarnation AO launched instead
// of whatever now answers to its name.
func (c *Coordinator) reviewLaunchPhaseFor(
	ctx stdctx.Context, runID, stepID, reviewRunID string,
) (ReviewLaunchPhase, ReviewerRef) {
	if reviewRunID == "" {
		return ReviewLaunchNone, ReviewerRef{}
	}
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		// Unreadable is not "nothing was launched". Reporting `none` here would
		// let a caller relaunch over a live reviewer, so the honest answer is
		// the one that licenses the least: intended, which forbids adoption AND
		// forbids treating the row as proof.
		return ReviewLaunchIntended, ReviewerRef{}
	}
	phase, ref := ReviewLaunchNone, ReviewerRef{}
	for _, cp := range cps {
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID {
			continue
		}
		// The run is identified by the column when it is set, and by the payload
		// when it is not — the pre-creation records cannot use the column, since
		// it is a foreign key to a row that does not exist yet.
		var rec reviewLaunchPhaseRecord
		_ = json.Unmarshal([]byte(cp.RetryState), &rec)
		named := rec.ReviewRunID
		if cp.ReviewRunID != nil && *cp.ReviewRunID != "" {
			named = *cp.ReviewRunID
		}
		if named != reviewRunID {
			continue
		}
		switch cp.DurablePhase {
		case reviewLaunchIntentPhase:
			if phase == ReviewLaunchNone {
				phase = ReviewLaunchIntended
			}
			if rec.HandleID != "" && ref.HandleID == "" {
				ref.HandleID = rec.HandleID
			}
		case reviewLaunchConfirmedPhase, "review_dispatched":
			if rec.HandleID != "" {
				ref.HandleID = rec.HandleID
			}
			// The confirmation is the only record that can carry an instance,
			// and the latest one wins: an adoption that re-confirmed the same
			// reviewer records the instance it actually verified.
			if rec.InstanceID != "" {
				ref.InstanceID = rec.InstanceID
			}
			// CONFIRMED REQUIRES AN EXACT INSTANCE.
			//
			// A marker without one proves a launch happened, not WHICH session
			// it produced — and every use of `confirmed` is a use of that
			// session. Classifying it separately is what stops a legacy or
			// malformed record from conferring authority by name alone.
			if ref.InstanceID != "" {
				phase = ReviewLaunchConfirmed
			} else if phase != ReviewLaunchConfirmed {
				phase = ReviewLaunchUnprovenIdentity
			}
		}
	}
	return phase, ref
}

// upgradeLegacyLaunchIdentity is the ONE way a marker without an exact instance
// can become authoritative.
//
// It performs a single coherent probe and, only if that probe proves AO owns a
// live reviewer AND reports the incarnation, persists a confirmation carrying
// it. From then on the reviewer is addressed by instance like any other, and
// the legacy record is never consulted for authority again.
//
// Every other answer refuses: nothing is adopted, nothing is destroyed, and the
// caller decides between resuming the launch protocol (proven absent) and a
// bounded incident (anything AO cannot prove).
func (c *Coordinator) upgradeLegacyLaunchIdentity(
	ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep,
	reviewRun domain.ReviewRun, ref ReviewerRef,
) (ReviewerRef, ReviewerPresence, error) {
	ensurer, ok := c.reviewerEnsurer()
	if !ok || ref.HandleID == "" {
		// Nothing to ask, and nothing that may be assumed.
		return ReviewerRef{}, ReviewerPresenceUnknown, nil
	}
	// By NAME, deliberately: this record has no instance, so the name is the
	// only key there is. That is legitimate exactly once — here — because the
	// answer is used to LEARN the instance, never to act by name.
	obs, perr := ensurer.ProbeReviewer(ctx, ReviewerRef{HandleID: ref.HandleID})
	if perr != nil {
		return ReviewerRef{}, ReviewerPresenceUnknown, nil
	}
	if !obs.Presence.LicensesAdoption() || obs.InstanceID == "" {
		return ReviewerRef{}, obs.Presence, nil
	}
	upgraded := obs.Ref(ref.HandleID)
	if err := c.recordReviewLaunchConfirmed(ctx, run, reviewStep, reviewRun, upgraded, reviewRun.TargetSHA); err != nil {
		return ReviewerRef{}, obs.Presence, err
	}
	if c.log != nil {
		c.log.Info("workflow: upgraded a name-only reviewer record to an exact runtime instance",
			"run", run.ID, "step", reviewStep.ID, "reviewRun", reviewRun.ID, "instance", upgraded.InstanceID)
	}
	return upgraded, obs.Presence, nil
}

// releaseReviewDispatchClaim gives back a launch claim whose dispatch may not
// proceed, so the outbox is not left permanently `dispatched` over a launch that
// will never happen.
//
// It returns the entry to `pending` rather than failing it: nothing was created
// and nothing was launched, so this is not a failure of the review — it is a
// dispatch that stopped before it began, and the step is free to be dispatched
// again if it becomes eligible.
func (c *Coordinator) releaseReviewDispatchClaim(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	reviewStep domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry,
	why string,
) (domain.WorkflowStep, error) {
	// THE SINGLE CHOKE POINT FOR dispatched -> pending.
	//
	// Releasing a claim is what makes a step dispatchable again, so every
	// release spends from the retry budget — and gating only the paths that
	// obviously look like retries left the others as ways around it, one attempt
	// per crash. The gate lives here so no caller can forget it, and it is
	// permissive for claims that never consumed an attempt (a step that left its
	// state, a dispatch that stopped before it began), which have no history and
	// therefore nothing spent.
	if ok, reason, berr := c.reviewLaunchBudgetRemains(ctx, run, reviewStep, entry); berr != nil || !ok {
		if c.log != nil {
			c.log.Warn("workflow: refusing to make a review claim dispatchable again",
				"run", run.ID, "step", reviewStep.ID, "key", entry.IdempotencyKey,
				"why", why, "refused", reason, "err", berr)
		}
		return c.markReviewAmbiguous(ctx, run, reviewStep, reason)
	}
	// Ownership-conditioned. Releasing is a transition off `dispatched`, and the
	// row is reclaimable — so a release that names no generation could give away
	// a claim that a NEWER dispatch is currently holding and launching under.
	released, err := c.store.ReleaseDispatchedWorkflowOutboxGeneration(
		ctx, entry.ID, "", entry.DispatchGeneration)
	if err != nil {
		return reviewStep, err
	}
	if !released {
		if c.log != nil {
			c.log.Warn("workflow: not releasing a review claim this caller no longer owns",
				"run", run.ID, "step", reviewStep.ID, "key", entry.IdempotencyKey, "why", why)
		}
		return reviewStep, nil
	}
	if c.log != nil {
		c.log.Warn("workflow: released a review dispatch claim without launching",
			"run", run.ID, "step", reviewStep.ID, "key", entry.IdempotencyKey, "why", why)
	}
	if fresh, ok, ferr := c.getWorkflowStep(ctx, run.ID, reviewStep.ID); ferr == nil && ok {
		return fresh, nil
	}
	return reviewStep, nil
}

// reviewLaunchStillAuthorized re-reads every fact a launch depends on and
// reports whether the launch may still proceed.
//
// It reads from the store rather than from anything the caller is holding: the
// whole point is that the caller's values are old by the time the external act
// is about to happen. A cancellation that lands in that window used to be
// invisible, and the reviewer was launched into a step that no longer existed.
func (c *Coordinator) reviewLaunchStillAuthorized(
	ctx stdctx.Context,
	runID, stepID string,
	entry domain.WorkflowOutboxEntry,
	authorization reviewLaunchPhaseRecord,
) (bool, string) {
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil || !ok {
		return false, "the parent run could not be re-read before launching"
	}
	if run.State.Terminal() {
		return false, "the parent run reached a terminal state before the reviewer was launched"
	}
	step, found, serr := c.getWorkflowStep(ctx, runID, stepID)
	if serr != nil || !found {
		return false, "the review step could not be re-read before launching"
	}
	if step.State.Terminal() {
		return false, "the review step reached a terminal state before the reviewer was launched"
	}
	// This dispatch must still hold the launch claim it acquired.
	entries, eerr := c.store.ListWorkflowOutboxByRun(ctx, runID)
	if eerr != nil {
		return false, "the dispatch claim could not be re-read before launching"
	}
	held := false
	for _, e := range entries {
		if e.IdempotencyKey != entry.IdempotencyKey {
			continue
		}
		held = e.Status == domain.WorkflowOutboxDispatched
	}
	if !held {
		return false, "this dispatch no longer holds the launch claim"
	}
	// And the authority pointer must not have moved under it. An empty expected
	// pointer means the step was released for a replacement, which stays valid
	// until something else binds.
	current := pointerOf(step)
	if authorization.ExpectedPointer != "" && current != authorization.ExpectedPointer {
		return false, "the review authority moved before the reviewer was launched"
	}
	if authorization.ExpectedPointer == "" && current != "" && current != authorization.ReviewRunID {
		return false, "another review took this step before the reviewer was launched"
	}
	return true, ""
}

// ensureReviewerLaunched performs the external launch through the deterministic
// identity, probing first so an uncertain replay adopts rather than duplicates.
//
// This is the distributed-systems boundary of the whole protocol. Launch alone
// cannot be replayed safely — a crash between the side effect and its
// confirmation is indistinguishable from never having launched. Probing the
// identity AO decided and persisted before the act turns that ambiguity into a
// question with an answer.
//
// `known` false from the probe is uncertainty, not absence: the caller must
// escalate rather than launch, because launching on an unanswered probe is
// exactly how a second reviewer appears.
func (c *Coordinator) ensureReviewerLaunched(
	ctx stdctx.Context, req ReviewerLaunchRequest, identity string,
) (ReviewerLaunchResult, bool, error) {
	ensurer, ok := c.reviewerEnsurer()
	if !ok {
		return ReviewerLaunchResult{}, false, fmt.Errorf(
			"%w: no reviewer launcher is wired", errReviewerCapabilityMissing)
	}
	if identity == "" {
		// Every launch must be addressable after a crash. Launching without a
		// deterministic identity used to be tolerated as a degraded path, and it
		// is exactly the ambiguity this protocol exists to remove: nothing could
		// probe, adopt, or terminate what it produced. It is now refused, so the
		// dispatch rests and retries rather than creating something unrecoverable.
		return ReviewerLaunchResult{}, false, fmt.Errorf(
			"%w: no deterministic reviewer identity for this request", errReviewerCapabilityMissing)
	}
	// Pre-launch there is no instance to address: the name is the only key, and
	// this is the one point in the lifecycle where resolving by it is correct.
	// Pre-launch there is no instance to address: the name is the only key, and
	// this is the one point in the lifecycle where resolving by it is correct.
	ref := ReviewerRef{HandleID: identity}
	obs, perr := ensurer.ProbeReviewer(ctx, ref)
	if perr != nil {
		obs = ReviewerObservation{Presence: ReviewerPresenceUnknown}
	}
	if obs.Presence == ReviewerPresenceExited {
		// AO's own reviewer, proven finished, still holding its session name.
		// It cannot be adopted (there is nothing running to wait for) and the
		// name cannot be reused while it stands, so it is reclaimed here — a
		// destruction licensed by proof of ownership, and aimed at the exact
		// incarnation the probe just identified — and the identity is re-probed
		// rather than assumed free.
		if cerr := ensurer.CancelReviewer(ctx, obs.Ref(identity)); cerr != nil {
			return ReviewerLaunchResult{}, false, fmt.Errorf(
				"reclaim exited reviewer %s: %w", identity, cerr)
		}
		obs, perr = ensurer.ProbeReviewer(ctx, ref)
		if perr != nil {
			obs = ReviewerObservation{Presence: ReviewerPresenceUnknown}
		}
	}
	switch {
	case obs.Presence.LicensesAdoption():
		// Proven to be AO's own reviewer for this identity, and proven running.
		//
		// The instance comes from THIS observation — the same one that proved
		// ownership and liveness. Asking again would be a second moment, and a
		// replacement arriving in between would be persisted as the confirmation
		// for a reviewer it has nothing to do with.
		if obs.InstanceID == "" {
			// An adoption AO cannot pin to an exact incarnation is not an
			// adoption. Refusing here is what stops a name-only confirmation
			// from ever reaching the ledger.
			return ReviewerLaunchResult{}, false, fmt.Errorf(
				"%w: %s is owned but reported no runtime instance", errReviewerPresenceUnproven, identity)
		}
		return ReviewerLaunchResult{HandleID: identity, InstanceID: obs.InstanceID}, true, nil
	case !obs.Presence.LicensesLaunch():
		// `foreign`, `unknown`, or an `exited` session that resisted reclamation.
		// Launching here is the one move that can put a second reviewer on the
		// same work, and "I could not tell" is not a licence to do it. The
		// uncertainty is returned to the caller, which preserves the durable
		// state and retries rather than guessing.
		return ReviewerLaunchResult{}, false, fmt.Errorf(
			"%w: %s at %s", errReviewerPresenceUnproven, obs.Presence, identity)
	}
	res, err := c.reviewerLauncher.Launch(ctx, req)
	if err != nil {
		return res, false, err
	}
	// The deterministic identity is what probe and cancel address, so it is what
	// gets recorded — never whatever the launcher happened to echo back. A handle
	// that differs from the identity is a handle recovery cannot act on.
	res.HandleID = identity
	return res, false, nil
}

// cancelReviewerExternally drives one reviewer termination through
// intent -> external kill -> probe -> confirmation, idempotently.
//
// Every step is replayable. A crash after the intent leaves a durable record
// recovery can finish; a crash after the kill and before the confirmation
// replays into a probe that finds the reviewer gone and confirms. The row is
// never called cancelled on the strength of the kill having been *attempted*.
func (c *Coordinator) cancelReviewerExternally(
	ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep,
	reviewRunID string, ref ReviewerRef, why string,
) error {
	identity := ref.HandleID
	// One intent per reviewer, however many times this is entered. Re-opening it
	// would append a row on every pass for as long as the reviewer resists
	// termination, turning a bounded obligation into an unbounded ledger.
	if !c.reviewCancelIntentExists(ctx, run.ID, reviewStep.ID, reviewRunID) {
		if err := c.recordReviewCancelPhase(ctx, run, reviewStep, reviewRunID, ref,
			reviewCancelIntentPhase, "review_cancel_intent: "+why); err != nil {
			return err
		}
	}
	ensurer, ok := c.reviewerEnsurer()
	if !ok || identity == "" {
		// No external handle to act on. The intent stands as the durable record
		// that AO wanted this reviewer gone and could not prove it acted.
		return nil
	}
	// Terminate ONLY what AO can prove is its own. A name collision or a stale
	// shell must never be destroyed on AO's behalf, so an unprovable identity
	// leaves the intent open rather than killing something unrelated.
	//
	// The classification is made HERE, on one probe, and every unprovable answer
	// ends this pass without an error. Handing an `unknown` identity to
	// CancelReviewer instead used to surface as a hard failure ("refusing to
	// terminate a session AO cannot prove it owns") from a path that runs on
	// boot reconciliation, on every wake, and on an ordinary workflow READ — so
	// a single stale pane failed all three. The obligation is durable; it does
	// not need an error to survive, and the bounded unproven-reviewer ledger is
	// what carries it to a person.
	obs, perr := ensurer.ProbeReviewer(ctx, ref)
	if obs.InstanceID != "" {
		// Anything destructive below addresses the incarnation THIS probe
		// identified, never a re-resolution of the name.
		ref.InstanceID = obs.InstanceID
	}
	switch {
	case perr == nil && obs.Presence == ReviewerPresenceForeign:
		if c.log != nil {
			c.log.Warn("workflow: refusing to terminate a session AO cannot prove it owns",
				"run", run.ID, "step", reviewStep.ID, "identity", identity)
		}
		return c.recordUnprovenReviewer(ctx, run, reviewStep, reviewRunID, identity,
			obs.Presence, "a session AO can prove is not its own holds this reviewer identity")
	case perr == nil && obs.Presence == ReviewerPresenceAbsent:
		// Proven gone. The cancellation is discharged by evidence, with nothing
		// to kill -- and emphatically not whatever answers to its name now.
		return c.recordReviewCancelPhase(ctx, run, reviewStep, reviewRunID, ref,
			reviewCancelConfirmedPhase, "review_cancel_confirmed: reviewer "+ref.String()+" is gone")
	case perr != nil || !obs.Presence.LicensesTermination():
		// UNKNOWN, or the probe itself failed -- a runtime that will not name
		// the pane's process is the common case. AO cannot prove ownership, so
		// it destroys nothing, records the observation, and converges THIS run
		// (and only this run) to needs_attention once the probe budget is spent.
		return c.escalateUnprovenReviewer(ctx, run, reviewStep, reviewRunID, identity, obs.Presence, perr)
	}
	if err := ensurer.CancelReviewer(ctx, ref); err != nil {
		return err
	}
	// Confirm only on evidence. An unanswered probe leaves the intent open, and
	// the next pass tries again — an orphan that is still alive in three seconds
	// is still an orphan, while a confirmation AO cannot stand behind is
	// permanent.
	// Confirm only on PROOF of absence. `foreign` and `unknown` leave the intent
	// open so the next pass tries again — an orphan that is still alive in three
	// seconds is still an orphan, while a confirmation AO cannot stand behind is
	// permanent.
	if obs, perr := ensurer.ProbeReviewer(ctx, ref); perr != nil || !obs.Presence.LicensesLaunch() {
		return nil
	}
	return c.recordReviewCancelPhase(ctx, run, reviewStep, reviewRunID, ref,
		reviewCancelConfirmedPhase, "review_cancel_confirmed: reviewer "+ref.String()+" is gone")
}

func (c *Coordinator) recordReviewCancelPhase(
	ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep,
	reviewRunID string, ref ReviewerRef, phase, nextAction string,
) error {
	stepID := reviewStep.ID
	rid := reviewRunID
	// The cancel intent carries the INSTANCE too, so an interrupted termination
	// is resumed against the incarnation it was aimed at rather than re-resolved
	// from the name it happened to use.
	payload, _ := json.Marshal(reviewLaunchPhaseRecord{
		ReviewRunID: reviewRunID, HandleID: ref.HandleID, InstanceID: ref.InstanceID,
	})
	cp := domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		NextAction:     nextAction,
		DurablePhase:   phase,
		PayloadVersion: "v1",
		RetryState:     string(payload),
		CreatedAt:      c.clock(),
	}
	if reviewRunID != "" {
		cp.ReviewRunID = &rid
	}
	_, err := c.store.CreateWorkflowCheckpoint(ctx, cp)
	return err
}

// finishPendingReviewCancellations replays any cancellation whose intent is
// durable and whose confirmation is not. It is the recovery half of the
// intent-act-confirm pair above.
func (c *Coordinator) finishPendingReviewCancellations(
	ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep,
) error {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, run.ID)
	if err != nil {
		return err
	}
	intents := map[string]ReviewerRef{} // reviewRunID -> the reviewer it aimed at
	confirmed := map[string]bool{}
	for _, cp := range cps {
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != reviewStep.ID {
			continue
		}
		var rec reviewLaunchPhaseRecord
		if json.Unmarshal([]byte(cp.RetryState), &rec) != nil {
			continue
		}
		switch cp.DurablePhase {
		case reviewCancelIntentPhase:
			intents[rec.ReviewRunID] = ReviewerRef{HandleID: rec.HandleID, InstanceID: rec.InstanceID}
		case reviewCancelConfirmedPhase:
			confirmed[rec.ReviewRunID] = true
		}
	}
	for reviewRunID, ref := range intents {
		if confirmed[reviewRunID] {
			continue
		}
		if err := c.cancelReviewerExternally(ctx, run, reviewStep, reviewRunID, ref,
			"resuming an interrupted reviewer cancellation"); err != nil {
			return err
		}
	}
	return nil
}

// finalizeBoundReviewDispatch completes a replacement whose pointer is already
// bound but whose protocol tail never finished.
//
// The bind is the second-to-last act. After it come the predecessor's
// supersession, the outbox acknowledgement and the final phase record, and a
// crash anywhere in that tail leaves a step that LOOKS dispatched to every
// early return in this package while the durable protocol is incomplete: a
// predecessor still claiming authority it no longer has, or an outbox row stuck
// at `dispatched` forever.
//
// It launches nothing — every act here is bookkeeping about a reviewer that
// already exists — and every act is idempotent, so repeated boots and wakes
// converge on the same finished protocol.
func (c *Coordinator) finalizeBoundReviewDispatch(
	ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep,
) (domain.WorkflowStep, error) {
	bound := pointerOf(reviewStep)
	if bound == "" || c.reviewRuns == nil {
		return reviewStep, nil
	}
	auth, ok := c.latestReviewDispatchAuthorization(ctx, run.ID, reviewStep.ID, "")
	if !ok || auth.ReviewRunID != bound {
		// No authorization for the bound run: it did not come from this
		// protocol (an ordinary cycle, or a row older than these records), so
		// there is no tail of ours to finish.
		return reviewStep, nil
	}

	// 1. The predecessor must know it was replaced.
	if auth.Predecessor != "" && auth.Predecessor != bound {
		prev, found, perr := c.reviewRuns.GetReviewRun(ctx, auth.Predecessor)
		if perr != nil {
			return reviewStep, perr
		}
		if found && prev.SupersededBy == "" {
			if _, serr := c.reviewRuns.MarkReviewRunSupersededBy(ctx, auth.Predecessor, bound); serr != nil {
				return reviewStep, serr
			}
		}
	}

	// 2. The outbox must not stay `dispatched` over a completed dispatch.
	entries, eerr := c.store.ListWorkflowOutboxByRun(ctx, run.ID)
	if eerr != nil {
		return reviewStep, eerr
	}
	for _, e := range entries {
		if e.IdempotencyKey != auth.IdempotencyKey || e.Status != domain.WorkflowOutboxDispatched {
			continue
		}
		if _, uerr := c.store.UpdateWorkflowOutboxStatus(ctx, e.ID,
			domain.WorkflowOutboxDispatched, domain.WorkflowOutboxAcknowledged, c.clock(), ""); uerr != nil {
			return reviewStep, uerr
		}
	}

	// 3. The final phase record, written last so its presence means the whole
	// protocol finished.
	if phase, _ := c.reviewLaunchPhaseFor(ctx, run.ID, reviewStep.ID, bound); phase == ReviewLaunchConfirmed {
		if !c.reviewDispatchFinalized(ctx, run.ID, reviewStep.ID, bound) {
			sid := ""
			if rr, found, rerr := c.reviewRuns.GetReviewRun(ctx, bound); rerr == nil && found {
				sid = string(rr.SessionID)
			}
			if ferr := c.recordReviewDispatchFinalized(ctx, run, reviewStep, bound, sid, auth.TargetSHA); ferr != nil {
				return reviewStep, ferr
			}
		}
	}
	return reviewStep, nil
}

// reviewDispatchFinalized reports whether the final phase record exists for this
// bound run.
func (c *Coordinator) reviewDispatchFinalized(ctx stdctx.Context, runID, stepID, reviewRunID string) bool {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return false
	}
	for _, cp := range cps {
		if cp.DurablePhase != "review_dispatched" ||
			cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID {
			continue
		}
		if cp.ReviewRunID != nil && *cp.ReviewRunID == reviewRunID {
			return true
		}
	}
	return false
}

// recordReviewDispatchFinalized writes the terminal phase record of the
// protocol.
func (c *Coordinator) recordReviewDispatchFinalized(
	ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep,
	reviewRunID, sessionID, targetSHA string,
) error {
	stepID, rid := reviewStep.ID, reviewRunID
	cp := domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		ReviewRunID:    &rid,
		HeadSHA:        targetSHA,
		NextAction:     "review_dispatched: awaiting verdict from review run " + reviewRunID,
		DurablePhase:   "review_dispatched",
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      c.clock(),
	}
	if sessionID != "" {
		sid := sessionID
		cp.SessionID = &sid
	}
	_, err := c.store.CreateWorkflowCheckpoint(ctx, cp)
	return err
}

// inFlightReviewHarness returns the harness a still-unfinished replacement
// authorization already committed to.
//
// "In flight" means an authorization exists whose protocol has not finalized: a
// review identity has been created, or may have been, under that provider. Its
// deterministic reviewer identity is derived from that same authorization, so
// recovering under a different harness would probe for one reviewer while
// another is still running.
func (c *Coordinator) inFlightReviewHarness(
	ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep,
) (domain.ReviewerHarness, bool) {
	auth, ok := c.latestReviewDispatchAuthorization(ctx, run.ID, reviewStep.ID, "")
	if !ok || auth.Harness == "" || auth.ReviewRunID == "" {
		return "", false
	}
	if c.reviewDispatchFinalized(ctx, run.ID, reviewStep.ID, auth.ReviewRunID) {
		// That dispatch completed; the next one routes freshly.
		return "", false
	}
	// A review run that ended without ever launching is not in flight either —
	// it is a dead attempt, and the next dispatch is free to route again.
	if c.reviewRuns != nil {
		if rr, found, err := c.reviewRuns.GetReviewRun(ctx, auth.ReviewRunID); err == nil && found {
			if rr.Status == domain.ReviewRunFailed {
				return "", false
			}
		}
	}
	return domain.ReviewerHarness(auth.Harness), true
}

// reviewerIdentityFor resolves the deterministic reviewer identity of a review
// run from durable records: the launch phases first, then the authorization.
//
// It never recomputes the identity from today's launcher, because the identity
// is a property of the launch that happened, not of the process asking.
func (c *Coordinator) reviewerIdentityFor(
	ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep, reviewRunID string,
) (ReviewerRef, bool) {
	if reviewRunID == "" {
		return ReviewerRef{}, false
	}
	// The durable launch record first: it is the only source that can carry the
	// INSTANCE, and the instance is what a later pass must address.
	if _, ref := c.reviewLaunchPhaseFor(ctx, run.ID, reviewStep.ID, reviewRunID); ref.HandleID != "" {
		return ref, true
	}
	// Falling back to the authorization yields a name and no instance — correct,
	// because that record is written before anything exists to identify.
	if auth, ok := c.latestReviewDispatchAuthorization(ctx, run.ID, reviewStep.ID, ""); ok &&
		auth.ReviewRunID == reviewRunID && auth.HandleID != "" {
		return ReviewerRef{HandleID: auth.HandleID, InstanceID: auth.InstanceID}, true
	}
	return ReviewerRef{}, false
}

// ReconcileOrphanedReviewers finishes external-session obligations that outlive
// the workflow they belonged to.
//
// A terminal run is the end of AO's INTEREST in a review, never the end of its
// responsibility for a process it started. Cancellation can win the race against
// a launch that has already created a reviewer, and every ordinary recovery path
// returns early on a terminal run — so the reviewer would keep running with
// nothing left that would ever look for it.
//
// This runs regardless of run state and does exactly one thing: drive unresolved
// cancellations to completion, and open a cancellation for any reviewer this
// protocol launched into a run that has since gone terminal.
func (c *Coordinator) ReconcileOrphanedReviewers(
	ctx stdctx.Context, run domain.WorkflowRun, steps []domain.WorkflowStep,
) error {
	if c.reviewRuns == nil {
		return nil
	}
	for _, step := range steps {
		if step.Kind != domain.WorkflowStepReview {
			continue
		}
		// Finish any termination already decided on.
		if err := c.finishPendingReviewCancellations(ctx, run, step); err != nil {
			return err
		}
		if !run.State.Terminal() && !step.State.Terminal() {
			continue
		}
		// The run or step is over. Every LAUNCH INTENT recorded for this step is
		// a reviewer AO may have created — that is what an intent means — and
		// each one is an obligation until it is either finalized or proven gone.
		//
		// The intents are what this iterates, not the newest authorization: an
		// interrupted launch leaves an intent behind while the authorization the
		// step ends up with may belong to an entirely different, completed
		// dispatch.
		for reviewRunID, ref := range c.unresolvedReviewLaunchIntents(ctx, run.ID, step.ID) {
			// Probe BEFORE opening a cancellation. A reviewer that already
			// exited owes nothing, and writing an intent for it on every boot
			// would grow the ledger without end. Only a reviewer AO can prove is
			// present and its own is an obligation.
			ensurer, ok := c.reviewerEnsurer()
			if !ok {
				break
			}
			obs, perr := ensurer.ProbeReviewer(ctx, ref)
			presence := obs.Presence
			// The termination below addresses the incarnation THIS probe
			// identified, never a re-resolution of the name.
			if obs.InstanceID != "" {
				ref.InstanceID = obs.InstanceID
			}
			switch {
			case perr == nil && presence.LicensesTermination():
				// Proven AO's own — running or exited, both are obligations and
				// both are safe to reclaim.
				if err := c.cancelReviewerExternally(ctx, run, step, reviewRunID, ref,
					"the workflow it was reviewing reached a terminal state"); err != nil {
					return err
				}
			case perr == nil && presence == ReviewerPresenceAbsent:
				// Nothing there. The obligation is discharged by proof.
			case perr == nil && presence == ReviewerPresenceForeign:
				// Something AO can prove is NOT its own. Never touched — but
				// recorded, so a session sitting on a reviewer identity is a
				// visible fact rather than a silent one.
				if err := c.recordUnprovenReviewer(ctx, run, step, reviewRunID, ref.String(),
					presence, "a session AO can prove is not its own holds this reviewer identity"); err != nil {
					return err
				}
			default:
				// UNKNOWN, or the probe itself failed.
				//
				// This used to be a bare `continue`: an unresolved obligation AO
				// could not identify was skipped on every pass, forever, with
				// nothing written down and nobody told. That is how a live orphan
				// stays invisible. It now accumulates durable evidence and, once
				// the retry budget is spent, becomes a bounded incident a person
				// can act on. It still never launches and never destroys.
				if err := c.escalateUnprovenReviewer(ctx, run, step, reviewRunID, ref.String(), presence, perr); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// reconcileOrphanedReviewersForAllRuns sweeps every run AO knows about for
// unresolved reviewer obligations, terminal ones included.
//
// Best-effort by construction: this cleans up processes, and a failure to reach
// one of them must never stop boot recovery from repairing workflow state. Each
// unfinished obligation stays durable, so the next boot tries again.
func (c *Coordinator) reconcileOrphanedReviewersForAllRuns(ctx stdctx.Context) {
	if c.reviewRuns == nil {
		return
	}
	// Only runs that actually carry a protocol record are considered — the
	// sweep is targeted, not a scan of every workflow that ever existed.
	seen := map[string]bool{}
	for _, phase := range []string{reviewCancelIntentPhase, reviewDispatchAuthorizedPhase} {
		ids, err := c.store.ListWorkflowRunIDsByCheckpointPhase(ctx, phase)
		if err != nil {
			continue
		}
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			run, ok, rerr := c.store.GetWorkflowRun(ctx, id)
			if rerr != nil || !ok {
				continue
			}
			steps, serr := c.store.ListWorkflowSteps(ctx, id)
			if serr != nil {
				continue
			}
			if oerr := c.ReconcileOrphanedReviewers(ctx, run, steps); oerr != nil && c.log != nil {
				c.log.Warn("workflow: could not finish a reviewer obligation for this run",
					"run", id, "err", oerr)
			}
		}
	}
}

// reviewCancelIntentExists reports whether a termination has already been
// decided on for this reviewer.
func (c *Coordinator) reviewCancelIntentExists(ctx stdctx.Context, runID, stepID, reviewRunID string) bool {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return false
	}
	for _, cp := range cps {
		if cp.DurablePhase != reviewCancelIntentPhase ||
			cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID {
			continue
		}
		var rec reviewLaunchPhaseRecord
		if json.Unmarshal([]byte(cp.RetryState), &rec) == nil && rec.ReviewRunID == reviewRunID {
			return true
		}
	}
	return false
}

// unresolvedReviewLaunchIntents returns every reviewer identity this step has a
// launch intent for whose protocol never resolved — neither finalized as a
// dispatch nor confirmed terminated.
//
// Each one is a reviewer AO may have created and has no record of concluding.
// That is precisely the set a terminal workflow still owes something to.
func (c *Coordinator) unresolvedReviewLaunchIntents(
	ctx stdctx.Context, runID, stepID string,
) map[string]ReviewerRef {
	out := map[string]ReviewerRef{}
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return out
	}
	cancelled := map[string]bool{}
	finalized := map[string]bool{}
	for _, cp := range cps {
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID {
			continue
		}
		var rec reviewLaunchPhaseRecord
		_ = json.Unmarshal([]byte(cp.RetryState), &rec)
		id := rec.ReviewRunID
		if id == "" && cp.ReviewRunID != nil {
			id = *cp.ReviewRunID
		}
		if id == "" {
			continue
		}
		switch cp.DurablePhase {
		case reviewLaunchIntentPhase:
			if rec.HandleID != "" {
				out[id] = ReviewerRef{HandleID: rec.HandleID, InstanceID: rec.InstanceID}
			}
		case reviewLaunchConfirmedPhase:
			// A confirmation upgrades the obligation with the exact instance,
			// so the sweep addresses the incarnation AO launched.
			if rec.HandleID != "" {
				out[id] = ReviewerRef{HandleID: rec.HandleID, InstanceID: rec.InstanceID}
			}
		case reviewCancelConfirmedPhase:
			cancelled[id] = true
		case "review_dispatched":
			finalized[id] = true
		}
	}
	for id := range out {
		if cancelled[id] || finalized[id] {
			delete(out, id)
		}
	}
	return out
}

// reviewerUnprovenPhase is the durable evidence trail for a reviewer identity
// AO holds an unresolved obligation for and cannot classify.
//
// It exists because the alternative was silence. An unresolved launch or cancel
// intent whose identity probes as `unknown` used to be skipped on every pass:
// nothing was written, nothing was retried with a budget, and nobody was told —
// so a reviewer that AO could neither adopt nor terminate could stay alive
// indefinitely, consuming provider capacity, with no trace on the ledger.
const reviewerUnprovenPhase = "review_reviewer_unproven"

// maxUnprovenReviewerProbes is the retry budget before an unclassifiable
// reviewer becomes a person's problem.
//
// Bounded in both directions on purpose: uncertainty is usually transient (a
// tmux server restarting, a probe timing out), so retrying is right; but
// retrying forever is what made the original silence indistinguishable from
// correct behaviour.
const maxUnprovenReviewerProbes = 5

// reviewerUnprovenRecord is what each probe attempt writes down.
type reviewerUnprovenRecord struct {
	ReviewRunID string `json:"reviewRunId"`
	HandleID    string `json:"handleId"`
	Presence    string `json:"presence"`
	Attempt     int    `json:"attempt"`
	Err         string `json:"err,omitempty"`
}

// recordUnprovenReviewer appends one bounded observation about an identity AO
// could not act on.
func (c *Coordinator) recordUnprovenReviewer(
	ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep,
	reviewRunID, identity string, presence ReviewerPresence, why string,
) error {
	_, _, err := c.appendUnprovenReviewerProbe(ctx, run, reviewStep, reviewRunID, identity, presence, why)
	return err
}

// appendUnprovenReviewerProbe records one probe observation IF the budget still
// allows one, and reports the durable count either way.
//
// Counting before appending is the whole contract. Appending first and checking
// afterwards made the budget govern only the incident, not the ledger: every
// boot, wake, and reconcile added another identical row for an obligation that
// had already been escalated, so a single unprovable reviewer grew the
// checkpoint table without end. `appended` tells the caller whether anything new
// was actually written.
func (c *Coordinator) appendUnprovenReviewerProbe(
	ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep,
	reviewRunID, identity string, presence ReviewerPresence, why string,
) (attempt int, appended bool, err error) {
	prior := c.unprovenReviewerProbeCount(ctx, run.ID, reviewStep.ID, reviewRunID)
	if prior >= maxUnprovenReviewerProbes {
		// Budget spent. The obligation is already on the ledger exactly
		// maxUnprovenReviewerProbes times, and repeating it says nothing new.
		return prior, false, nil
	}
	attempt = prior + 1
	stepID := reviewStep.ID
	payload, _ := json.Marshal(reviewerUnprovenRecord{
		ReviewRunID: reviewRunID,
		HandleID:    identity,
		Presence:    string(presence),
		Attempt:     attempt,
		Err:         why,
	})
	if _, cerr := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		NextAction: fmt.Sprintf(
			"review_reviewer_unproven: reviewer %s for review run %s probed as %s (attempt %d/%d): %s",
			identity, reviewRunID, presence, attempt, maxUnprovenReviewerProbes, why),
		DurablePhase:   reviewerUnprovenPhase,
		PayloadVersion: "v1",
		RetryState:     string(payload),
		CreatedAt:      c.clock(),
	}); cerr != nil {
		return prior, false, cerr
	}
	return attempt, true, nil
}

// unprovenReviewerProbeCount counts prior unproven observations for one reviewer
// identity on one step.
//
// The correlation key is (step, review run id), which is durable and survives
// restart — so the budget is a property of the OBLIGATION rather than of the
// process that happens to be observing it. A genuinely different reviewer
// obligation gets its own budget, which is why the count is keyed by review run
// rather than by step alone.
func (c *Coordinator) unprovenReviewerProbeCount(
	ctx stdctx.Context, runID, stepID, reviewRunID string,
) int {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		// An unreadable ledger must not read as "budget still available", or a
		// storage failure becomes an unbounded write loop.
		return maxUnprovenReviewerProbes
	}
	n := 0
	for _, cp := range cps {
		if cp.DurablePhase != reviewerUnprovenPhase {
			continue
		}
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID {
			continue
		}
		var rec reviewerUnprovenRecord
		if json.Unmarshal([]byte(cp.RetryState), &rec) != nil {
			continue
		}
		if rec.ReviewRunID == reviewRunID {
			n++
		}
	}
	return n
}

// escalateUnprovenReviewer is what an unresolvable obligation costs: durable
// evidence on every pass, a bounded retry budget, and then a stop a person can
// read — never a launch, never a destruction, and never silence.
func (c *Coordinator) escalateUnprovenReviewer(
	ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep,
	reviewRunID, identity string, presence ReviewerPresence, probeErr error,
) error {
	why := "AO could not classify what is at this reviewer identity"
	if probeErr != nil {
		why = probeErr.Error()
	}
	// Already a person's problem for this same obligation: nothing further is
	// written, and reconciliation is a no-op from here on. Checked BEFORE the
	// append so an escalated obligation stops costing ledger rows entirely.
	if run.State == domain.WorkflowRunNeedsAttention &&
		c.unprovenReviewerProbeCount(ctx, run.ID, reviewStep.ID, reviewRunID) >= maxUnprovenReviewerProbes {
		return nil
	}
	attempt, appended, err := c.appendUnprovenReviewerProbe(
		ctx, run, reviewStep, reviewRunID, identity, presence, why)
	if err != nil {
		return err
	}
	if attempt < maxUnprovenReviewerProbes {
		// Still inside the budget. Uncertainty is usually transient, and an
		// orphan that is still unprovable in three seconds is still an orphan.
		return nil
	}
	if !appended && run.State == domain.WorkflowRunNeedsAttention {
		// Budget spent and already escalated.
		return nil
	}
	detail := fmt.Sprintf(
		"reviewer %s for review run %s has an unresolved obligation but could not be classified after %d probes (%s). "+
			"AO will not launch a replacement over it and will not terminate a session it cannot prove it owns. "+
			"Check whether that session is still running and close it out.",
		identity, reviewRunID, attempt, why)
	// Through the evidence gate, like every other ambiguity: the stop carries a
	// collected snapshot or it is not taken at all.
	if _, serr := c.stopReviewAmbiguous(ctx, run, reviewStep,
		ReasonReviewStateAmbiguous, detail, ""); serr != nil {
		if c.log != nil {
			c.log.Warn("workflow: could not raise the unproven-reviewer incident; it will be retried",
				"run", run.ID, "step", reviewStep.ID, "reviewer", identity, "err", serr)
		}
		return nil
	}
	return nil
}

// reviewLaunchAbandonedPhase is the durable INTENT to abandon a review identity
// that was never launched, written BEFORE the two writes that carry it out.
//
// Abandoning is two operations — close the review run out as `failed`, then give
// the outbox claim back — and a crash between them used to be unrecoverable in a
// particularly quiet way: the row said `failed`, the outbox still said
// `dispatched`, and the failed-row branch read that pair as ambiguity. No
// reviewer existed, nothing would ever launch one, and the run sat in
// needs_attention forever.
//
// The marker turns that pair into a recognisable state. It says "this specific
// failure came from the unlaunched/absent path, and its claim still needs
// releasing", which is exactly the proof recovery must have before it resumes a
// dispatch on behalf of a failed row — a failed row from any OTHER cause keeps
// the conservative ambiguity it always had.
const reviewLaunchAbandonedPhase = "review_launch_abandoned"

// reviewLaunchAbandonRecord names the identity being abandoned and the claim
// that must be released with it.
type reviewLaunchAbandonRecord struct {
	ReviewRunID    string `json:"reviewRunId"`
	IdempotencyKey string `json:"idempotencyKey"`
	Why            string `json:"why"`
	// Cycle and Attempt are set ONLY when this abandon is part of a launch
	// attempt, and they are what make the retry budget crash-durable: this
	// record is the first write of the failure sequence, so an attempt stamped
	// here is spent even if every later bookkeeping write is lost.
	//
	// Abandoning for a reason that is not a launch failure (a lost
	// authorization, a legacy record with no reviewer behind it) leaves them
	// zero and consumes no budget.
	Cycle   int `json:"cycle,omitempty"`
	Attempt int `json:"attempt,omitempty"`
}

// abandonUnlaunchedReviewRun closes out a review identity that has no reviewer
// behind it, in an order that converges after a crash at any point.
//
//  1. record the intent      (durable proof of WHY, and of which claim)
//  2. mark the run failed     (it can no longer be adopted)
//  3. release the outbox claim (so the launch protocol resumes)
//
// A crash after (1) leaves a marker that the next pass reads and finishes. A
// crash after (2) is the case this exists for. Replay is idempotent: marking an
// already-failed run failed is a no-op, and releasing an already-released claim
// CASes against a status it no longer holds.
func (c *Coordinator) abandonUnlaunchedReviewRun(
	ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry, reviewRunID, detail, why string,
) (domain.WorkflowStep, error) {
	if err := c.recordReviewLaunchAbandonIntent(ctx, run, reviewStep, entry, reviewRunID, why); err != nil {
		return reviewStep, err
	}
	if _, cerr := c.reviewRuns.UpdateReviewRunResult(ctx, reviewRunID,
		domain.ReviewRunFailed, "", detail, "", false); cerr != nil {
		return reviewStep, cerr
	}
	return c.releaseReviewDispatchClaim(ctx, run, reviewStep, entry, why)
}

// recordReviewLaunchAbandonIntent writes the durable proof that a specific
// unlaunched review identity is being closed out, naming the EXACT outbox claim
// that closure authorises releasing.
//
// It is idempotent: an intent already on disk for this (run, step, review run,
// claim) is not written twice.
func (c *Coordinator) recordReviewLaunchAbandonIntent(
	ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry, reviewRunID, why string,
) error {
	return c.recordReviewLaunchAttemptAbandon(ctx, run, reviewStep, entry, reviewRunID, 0, 0, why)
}

// recordReviewLaunchAttemptAbandon is the same intent, additionally stamping the
// retry attempt it consumes. See reviewLaunchAbandonRecord.
func (c *Coordinator) recordReviewLaunchAttemptAbandon(
	ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry, reviewRunID string, cycle, attempt int, why string,
) error {
	recorded, merr := c.reviewLaunchAbandonMarker(ctx, run.ID, reviewStep.ID, reviewRunID, entry.IdempotencyKey)
	if merr != nil {
		// The ledger could not be read, so AO cannot tell whether its intent is
		// already on disk. Proceeding would either duplicate the marker or —
		// far worse — perform the abandon with no durable intent behind it,
		// recreating the exact unattributable state this protocol removes.
		// Nothing changes and the next pass tries again.
		return merr
	}
	if !recorded {
		stepID := reviewStep.ID
		payload, _ := json.Marshal(reviewLaunchAbandonRecord{
			ReviewRunID: reviewRunID, IdempotencyKey: entry.IdempotencyKey, Why: why,
			Cycle: cycle, Attempt: attempt,
		})
		if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
			ID:             "wfc-" + c.newID(),
			WorkflowRunID:  run.ID,
			WorkflowStepID: &stepID,
			ProjectID:      run.ProjectID,
			NextAction: fmt.Sprintf(
				"review_launch_abandoned: review run %s had no reviewer behind it (%s); releasing claim %s",
				reviewRunID, why, entry.IdempotencyKey),
			DurablePhase:   reviewLaunchAbandonedPhase,
			PayloadVersion: "v1",
			RetryState:     string(payload),
			CreatedAt:      c.clock(),
		}); err != nil {
			// The intent could not be made durable, so the writes it protects
			// are not taken. Nothing changes and the next pass tries again —
			// exactly the direction every other refusal in this protocol takes.
			return err
		}
	}
	return nil
}

// reviewLaunchAbandonMarker reports whether a durable abandon intent exists for
// this EXACT claim, and whether that question could be answered at all.
//
// The correlation is (workflow run, step, review run, idempotency key) — not the
// first three alone. A step is dispatched many times across cycles and
// replacements, each with its own outbox claim, so an abandon marker from an
// earlier generation would otherwise authorise releasing a NEWER dispatch's
// claim: a live launch cancelled by the ghost of a dead one.
//
// The error is returned rather than folded into the bool because the two
// callers need opposite things from it, and both need it to fail CLOSED:
// a reader that cannot see proof must not act as though proof exists, and a
// writer that cannot tell whether it already recorded its intent must not go on
// to perform the writes that intent protects.
func (c *Coordinator) reviewLaunchAbandonMarker(
	ctx stdctx.Context, runID, stepID, reviewRunID, idempotencyKey string,
) (bool, error) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return false, err
	}
	for _, cp := range cps {
		if cp.DurablePhase != reviewLaunchAbandonedPhase {
			continue
		}
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID {
			continue
		}
		var rec reviewLaunchAbandonRecord
		if json.Unmarshal([]byte(cp.RetryState), &rec) != nil {
			continue
		}
		if rec.ReviewRunID != reviewRunID {
			continue
		}
		// The claim this marker authorises releasing. A marker written before
		// the key was recorded names nothing, and authorises nothing.
		if rec.IdempotencyKey == "" || rec.IdempotencyKey != idempotencyKey {
			continue
		}
		return true, nil
	}
	return false, nil
}
