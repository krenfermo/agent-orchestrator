package workflow

import (
	stdctx "context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// fix_generation.go — P0-B's last hole: fix-cycle dispatch had no durable
// identity of its own.
//
// THE RACE THIS CLOSES. Worker dispatch mints a token before it claims its
// outbox row (dispatch.go: ClaimWorkflowOutboxDispatch) and names that token in
// the predicate of every later transition, so a pass that lost its claim can no
// longer acknowledge, fail or release somebody else's launch. Fix dispatch did
// none of that. It moved its row pending -> dispatched with the plain status
// CAS, which stamps NO token and CLEARS any that was there, and then completed
// with another plain status CAS. So `id + status = dispatched` was satisfiable
// by any pass at all, and two concrete failures followed:
//
//   - DUPLICATE PROMPT. Pass A claims (status CAS wins), calls Send, and stalls
//     before recording success. A reconciler releases or re-derives the cycle;
//     pass B enqueues under the same idempotency key, finds the row and — with
//     nothing on it saying WHOSE dispatch it was — completes it as its own. The
//     receipt-based recovery in fix_delivery_recovery.go narrowed this, but its
//     evidence is about the SESSION ("did some prompt with this digest arrive"),
//     never about which dispatch owns the claim; two dispatches of the same
//     cycle are byte-identical prompts and therefore indistinguishable to it.
//   - STALE LIFECYCLE MUTATION. A fix cycle derived from review generation N
//     could open an attempt, write the fix_dispatched fingerprint checkpoint
//     that authorizes the NEXT review, and thereby advance a lifecycle that had
//     already moved to review generation N+1 (or to an approval). fixAuthority
//     re-read the verdict, which catches an approved or rebound review, but
//     nothing bound the in-flight dispatch to the exact review generation it was
//     derived from, so a re-derivation across a restart could not tell "still
//     the same authority" from "the same review run, re-reviewed since".
//
// THE IDENTITY. fixDispatchGeneration names one fix-cycle dispatch completely:
// run, task, fix step, review run, review generation, cycle number, transport
// attempt, re-delivery ordinal, target worker session, findings digest, and —
// once it exists — the attempt row it opened. It is minted BEFORE the claim,
// stamped onto the outbox row by the claim itself, and written into the durable
// pre-delivery record strictly before Send. Either side reconstructs the other.
//
// It extends the existing machinery rather than replacing it: fixAuthority
// still gates delivery, the findings digest and prompt receipt are still
// computed and recorded exactly as they were, fixDelivery still projects the
// ledger, and the >4 KB transport path is untouched. The generation is one more
// field on the same records and one more term in the same CAS predicates.

// fixDispatchGeneration is the durable identity of ONE fix-cycle dispatch.
//
// Every field except ID and FixAttemptID is part of the BINDING: the answer to
// "which logical dispatch is this?". Two generations with the same binding
// describe the same intended delivery and may be adopted for one another across
// a restart; two with different bindings never may, however similar they look.
type fixDispatchGeneration struct {
	// ID is the token stamped on the outbox row by the claim, and the value
	// every ownership-dependent transition names back. Empty means LEGACY: a
	// dispatch recorded before this file existed. Legacy is a state to be
	// resolved (see fixGenerationDisposition), never a wildcard.
	ID string `json:"id,omitempty"`

	// ---- what this dispatch is for -------------------------------------
	WorkflowRunID string `json:"workflowRunId,omitempty"`
	// TaskID is the planned task this run serves, when it serves one. A child
	// run of an objective carries it; a standalone run does not.
	TaskID    string `json:"taskId,omitempty"`
	FixStepID string `json:"fixStepId,omitempty"`

	// ---- the authority it was derived from ------------------------------
	ReviewRunID string `json:"reviewRunId,omitempty"`
	// ReviewGeneration is the review authority token: the review run's id, its
	// effective verdict and the commit it reviewed, folded together. A
	// re-review of the same run, a changed verdict or a moved target all
	// produce a different token, which is what makes "is this fix still
	// authorized by the review that asked for it?" an exact comparison rather
	// than a same-id check.
	ReviewGeneration string `json:"reviewGeneration,omitempty"`

	// ---- which cycle, which delivery ------------------------------------
	CycleNumber      int `json:"cycleNumber"`
	TransportAttempt int `json:"transportAttempt,omitempty"`
	// Redelivery is fix_cycle_resume.go's ordinal: 0 for the ordinary
	// dispatch, N for the Nth human-initiated re-delivery of the same cycle.
	Redelivery int `json:"redelivery,omitempty"`

	// ---- where it goes and what it carries -------------------------------
	SessionID      string `json:"sessionId,omitempty"`
	FindingsDigest string `json:"findingsDigest,omitempty"`

	// ---- what it produced ------------------------------------------------
	// FixAttemptID names the workflow_attempt row this generation opened. It is
	// NOT part of the binding: it does not exist yet when the generation is
	// minted, and it is the one field a recovery legitimately fills in later.
	FixAttemptID string `json:"fixAttemptId,omitempty"`
}

// legacy reports a generation-less dispatch: one recorded before fix dispatch
// had an identity. Never treated as "matches anything".
func (g fixDispatchGeneration) legacy() bool { return g.ID == "" }

// binding is the identity of the logical dispatch, excluding the token and the
// attempt row. Rendered rather than hashed so a mismatch is readable in a
// checkpoint and in a test failure.
func (g fixDispatchGeneration) binding() string {
	return strings.Join([]string{
		g.WorkflowRunID,
		g.TaskID,
		g.FixStepID,
		g.ReviewRunID,
		g.ReviewGeneration,
		strconv.Itoa(g.CycleNumber),
		strconv.Itoa(g.TransportAttempt),
		strconv.Itoa(g.Redelivery),
		g.SessionID,
		g.FindingsDigest,
	}, "|")
}

// sameDispatch reports whether two generations describe the same logical
// delivery. Deliberately ignores the token: a recovery adopts the token it
// finds on disk, and what it must prove is that the delivery that token names
// is the delivery it is about to complete.
func (g fixDispatchGeneration) sameDispatch(other fixDispatchGeneration) bool {
	return g.binding() == other.binding()
}

// describe renders the generation for a ledger line or a log field.
func (g fixDispatchGeneration) describe() string {
	id := g.ID
	if id == "" {
		id = "(legacy, no generation)"
	}
	return fmt.Sprintf("generation %s (run %s, task %s, fix step %s, review %s @ %s, cycle %d, transport %d, redelivery %d, session %s, findings %s)",
		id, g.WorkflowRunID, orNone(g.TaskID), g.FixStepID, g.ReviewRunID, shortDigest(g.ReviewGeneration),
		g.CycleNumber, g.TransportAttempt, g.Redelivery, g.SessionID, shortDigest(g.FindingsDigest))
}

// reviewGenerationToken folds a review run into the authority identity a fix
// cycle is bound to: which review, what it concluded, and what it concluded it
// about. Hashed because TargetSHA and the verdict are already durable next to
// it on every record that carries this — the token exists to be COMPARED, and a
// digest compares exactly without inviting anybody to parse it.
func reviewGenerationToken(rr domain.ReviewRun) string {
	sum := sha256.Sum256([]byte(rr.ID + "\x00" + string(rr.EffectiveVerdict()) + "\x00" + rr.TargetSHA))
	return hex.EncodeToString(sum[:])
}

// newFixDispatchGeneration mints the identity for a dispatch about to be
// claimed. The token is generated here and becomes durable only if the claim
// succeeds and the pre-delivery record is written; an unclaimed generation
// names nothing and authorizes nothing.
func (c *Coordinator) newFixDispatchGeneration(
	run domain.WorkflowRun,
	fixStep domain.WorkflowStep,
	reviewRun domain.ReviewRun,
	cycleNumber, transportAttempt, redelivery int,
	findings fixFindingsRef,
) fixDispatchGeneration {
	g := c.intendedFixDispatchGeneration(run, fixStep, reviewRun, cycleNumber, transportAttempt, redelivery, findings)
	g.ID = "wfg-" + c.newID()
	return g
}

// intendedFixDispatchGeneration is the same identity WITHOUT a token: what this
// pass believes the dispatch is, derived from what it can read right now. It is
// what a recovery compares the generation on disk against.
func (c *Coordinator) intendedFixDispatchGeneration(
	run domain.WorkflowRun,
	fixStep domain.WorkflowStep,
	reviewRun domain.ReviewRun,
	cycleNumber, transportAttempt, redelivery int,
	findings fixFindingsRef,
) fixDispatchGeneration {
	taskID := ""
	if run.PlannedTaskID != nil {
		taskID = *run.PlannedTaskID
	}
	// A verify-driven cycle under a policy-SKIPPED review has no review run,
	// and therefore no review generation to be superseded by. Minting a token
	// over the zero value would produce a fence with nothing on the other side
	// of it: fixGenerationStaleRefusal would go and read review run "", fail to
	// find it, and refuse the delivery forever — which is exactly what happened
	// once maybeDispatchVerifyFix started (correctly) dispatching these cycles.
	//
	// The empty token is the honest value and the branch that handles it already
	// exists: fixGenerationStaleRefusal treats "no authority token" as "there is
	// simply no token to compare", and says in the same breath that
	// fixAuthorityRefusal is not weakened by it. That is true here — the
	// authority for this cycle is the unanswered verify_fix_reentry checkpoint,
	// which fixAuthorityRefusal checks under the same one-fix-per-re-entry rule
	// it applies to an approved review. The session and findings fences below
	// are unaffected and still apply.
	reviewGeneration := ""
	if reviewRun.ID != "" {
		reviewGeneration = reviewGenerationToken(reviewRun)
	}
	return fixDispatchGeneration{
		WorkflowRunID:    run.ID,
		TaskID:           taskID,
		FixStepID:        fixStep.ID,
		ReviewRunID:      reviewRun.ID,
		ReviewGeneration: reviewGeneration,
		CycleNumber:      cycleNumber,
		TransportAttempt: transportAttempt,
		Redelivery:       redelivery,
		SessionID:        string(reviewRun.SessionID),
		FindingsDigest:   FindingsDigest(findings.Body),
	}
}

// fixGenerationStaleRefusal returns "" when this generation may still mutate
// the run, and a precise reason when it may not.
//
// It is the generation half of the delivery gate; fixAuthorityRefusal is the
// review half, and both run, in that order, immediately before Send. The
// division is deliberate: fixAuthority asks "does the CURRENT review authorize
// a fix cycle at all?", this asks "is the cycle in my hand still the cycle that
// review authorized?". A superseded review passes the first and fails the
// second, which is exactly the stale-fix-generation case.
//
// Its default is refusal, for the same reason fixAuthorityRefusal's is: a
// store it cannot read is not a licence to write into a worktree.
func (c *Coordinator) fixGenerationStaleRefusal(
	ctx stdctx.Context,
	gen fixDispatchGeneration,
	reviewRun domain.ReviewRun,
	findings fixFindingsRef,
) string {
	// The target session. A generation minted for one worker session must never
	// deliver into another, whatever the review run says now.
	if want := string(reviewRun.SessionID); gen.SessionID != "" && gen.SessionID != want {
		return fmt.Sprintf("this fix generation targets worker session %s but the review run now names %s",
			gen.SessionID, want)
	}
	// The findings payload. Same cycle, different findings, is a different
	// delivery — sending it under this generation would attribute one payload's
	// dispatch record to another's bytes.
	if want := FindingsDigest(findings.Body); gen.FindingsDigest != "" && gen.FindingsDigest != want {
		return fmt.Sprintf("this fix generation carries findings digest %s but the findings to be delivered digest to %s",
			shortDigest(gen.FindingsDigest), shortDigest(want))
	}
	if gen.ReviewGeneration == "" {
		// Legacy record with no authority token. fixAuthorityRefusal still
		// applies and is not weakened; there is simply no token to compare.
		return ""
	}
	if gen.ReviewRunID != "" && gen.ReviewRunID != reviewRun.ID {
		return fmt.Sprintf("this fix generation was derived from review run %s, but the cycle in hand is for %s",
			gen.ReviewRunID, reviewRun.ID)
	}
	if c.reviewRuns == nil {
		return ""
	}
	current, found, err := c.reviewRuns.GetReviewRun(ctx, gen.ReviewRunID)
	if err != nil || !found {
		return fmt.Sprintf("AO could not read review run %s to prove this fix generation's authority is still current", gen.ReviewRunID)
	}
	if token := reviewGenerationToken(current); token != gen.ReviewGeneration {
		return fmt.Sprintf("this fix generation was authorized by review generation %s, which has been superseded by %s (verdict %s, target %s)",
			shortDigest(gen.ReviewGeneration), shortDigest(token), current.EffectiveVerdict(), current.TargetSHA)
	}
	return ""
}

// ---------------------------------------------------------------------------
// Recovery: which generation owns the dispatch already on disk?
// ---------------------------------------------------------------------------

// fixGenerationDisposition is what recovery could establish about the dispatch
// it found. Exactly three answers, and the middle one is not a synonym for
// either neighbour.
type fixGenerationDisposition int

const (
	// fixGenerationOwned: the durable records name one generation, it matches
	// the dispatch this pass derived, and the outbox row agrees. Recovery may
	// complete it.
	fixGenerationOwned fixGenerationDisposition = iota
	// fixGenerationLegacyAdopted: no generation was ever recorded, and the
	// generation-less state maps deterministically onto exactly one delivery
	// whose session and findings match what is derived now. Recovery may
	// complete it, under the empty token — which is what the ownership CAS
	// requires for a row claimed before the token existed.
	fixGenerationLegacyAdopted
	// fixGenerationUnprovable: the records cannot be mapped to one dispatch.
	// FAIL CLOSED. No send, no attempt, no advance, no retry loop.
	fixGenerationUnprovable
)

// resolveOwningFixGeneration answers "whose dispatch is the one on disk?" for a
// cycle whose outbox entry is already past `pending`.
//
// The answer is assembled from two durable sources and they must agree:
//
//	the outbox row's claim token (cleared by acknowledge/fail, so its ABSENCE
//	is not evidence of anything on an acknowledged row), and
//	the pre-delivery records' embedded generations (never cleared, so they are
//	authoritative whenever they exist).
//
// A generation is adopted, never re-minted. Minting a fresh token here would
// create a second identity for a delivery that already happened, which is the
// precise mistake this whole mechanism exists to make impossible — and it is
// requirement 9's "do not fabricate generation" for the legacy rows too: a
// generation-less dispatch stays generation-less for the life of its outbox
// row, and is completed under the empty token the row actually holds.
func (c *Coordinator) resolveOwningFixGeneration(
	entry domain.WorkflowOutboxEntry,
	intended fixDispatchGeneration,
	intents []promptDeliveryRecord,
) (fixDispatchGeneration, fixGenerationDisposition, string) {
	rowToken := entry.DispatchGeneration

	// Distinct generations named by the pre-delivery records for this exact
	// (step, cycle, transport attempt). More than one is by construction a
	// state AO cannot map to a single dispatch.
	recorded := map[string]fixDispatchGeneration{}
	legacy := []promptDeliveryRecord{}
	for _, rec := range intents {
		if rec.Generation.legacy() {
			legacy = append(legacy, rec)
			continue
		}
		recorded[rec.Generation.ID] = rec.Generation
	}

	// The generation-less records must be deterministically mappable onto ONE
	// delivery before any branch below may lean on them: they carry no token,
	// so the only proof available is that they all describe the same thing and
	// that it is the thing this pass derived. FixFindingsEvidence has carried
	// the review run and the findings digest since before generations existed,
	// which is what makes this a proof rather than an assumption.
	if why := legacyFixDeliveriesAgree(legacy, intended); why != "" {
		return fixDispatchGeneration{}, fixGenerationUnprovable, why
	}

	switch {
	case len(recorded) > 1:
		ids := make([]string, 0, len(recorded))
		for id := range recorded {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return fixDispatchGeneration{}, fixGenerationUnprovable,
			fmt.Sprintf("fix cycle %d has %d different dispatch generations recorded against it (%s), so AO cannot say which one owns the delivery already on disk",
				intended.CycleNumber, len(recorded), strings.Join(ids, ", "))

	case len(recorded) == 1:
		var found fixDispatchGeneration
		for _, g := range recorded {
			found = g
		}
		if rowToken != "" && rowToken != found.ID {
			return fixDispatchGeneration{}, fixGenerationUnprovable,
				fmt.Sprintf("the outbox row for fix cycle %d is claimed by generation %s but its pre-delivery record was written by %s",
					intended.CycleNumber, rowToken, found.ID)
		}
		if !found.sameDispatch(intended) {
			return fixDispatchGeneration{}, fixGenerationUnprovable,
				fmt.Sprintf("the fix dispatch on disk is %s, which is not the dispatch this pass derived (%s)",
					found.describe(), intended.describe())
		}
		return found, fixGenerationOwned, ""

	case rowToken != "":
		if len(legacy) > 0 {
			// The row is claimed by a generation that never got as far as
			// writing its pre-delivery record, while an OLDER generation-less
			// delivery of the same cycle did reach Send. "Nothing was sent
			// under the claim" and "something was sent for this cycle" are both
			// true, and AO cannot tell whether they are the same event.
			return fixDispatchGeneration{}, fixGenerationUnprovable,
				fmt.Sprintf("the outbox row for fix cycle %d is claimed by generation %s while %d generation-less pre-delivery record(s) for the same cycle are also on the ledger",
					intended.CycleNumber, rowToken, len(legacy))
		}
		// Boundary B: the claim is durable and no pre-delivery record exists at
		// all, which is positive proof Send was never reached
		// (recordFixDispatchIntent is fatal-before-Send). The claim is this
		// delivery's; adopt its token and let classification decide, which it
		// will as "provably not sent".
		adopted := intended
		adopted.ID = rowToken
		return adopted, fixGenerationOwned, ""

	default:
		// No claim token. Either a generation-less delivery that agrees with
		// this pass (proven just above), or nothing written at all. Both are
		// completed under the empty token the row actually holds — cleared here
		// explicitly, because an unclaimed acknowledge is the ONLY transition
		// that matches an unclaimed row, and inventing a token would be both a
		// fabricated identity and a CAS that silently matches nothing.
		//
		// The binding still travels, so the NEXT recovery has more to work with
		// than this one did.
		adopted := intended
		adopted.ID = ""
		return adopted, fixGenerationLegacyAdopted, ""
	}
}

// legacyFixDeliveriesAgree returns "" when every generation-less pre-delivery
// record for one cycle describes the same delivery, and that delivery is the one
// this pass derived. Otherwise it returns the named, actionable condition that
// makes the generation-less state unprovable.
//
// Empty fields are skipped rather than treated as mismatches: a record written
// before a field existed says nothing about it, and missing evidence is never
// evidence (the standing rule in fix_delivery_recovery.go).
func legacyFixDeliveriesAgree(legacy []promptDeliveryRecord, intended fixDispatchGeneration) string {
	for _, rec := range legacy {
		if d := rec.Findings.Digest; d != "" && intended.FindingsDigest != "" && d != intended.FindingsDigest {
			return fmt.Sprintf("a generation-less fix delivery on disk for cycle %d carried findings %s, but this pass derived %s, so AO cannot prove they are the same delivery",
				intended.CycleNumber, shortDigest(d), shortDigest(intended.FindingsDigest))
		}
		if r := rec.Findings.ReviewRunID; r != "" && intended.ReviewRunID != "" && r != intended.ReviewRunID {
			return fmt.Sprintf("a generation-less fix delivery on disk for cycle %d was derived from review run %s, but this pass derived %s, so AO cannot prove they are the same delivery",
				intended.CycleNumber, r, intended.ReviewRunID)
		}
	}
	return ""
}
