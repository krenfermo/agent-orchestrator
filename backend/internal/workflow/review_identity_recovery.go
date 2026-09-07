package workflow

import (
	stdctx "context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ReviewerIdentityLedger answers, about AO's OWN launch record, whether a
// reviewer was ever handed a credential to speak with.
//
// It is deliberately a question about AO's bookkeeping and not about the agent:
// nothing here observes what a reviewer is doing, thinking, or has decided.
// Optional, like every other dependency here; nil makes the recovery below
// inert, which is what a pre-P4-I wiring and every test double get.
type ReviewerIdentityLedger interface {
	EverIssuedForReviewRun(ctx stdctx.Context, reviewRunID string) (bool, error)
}

// reviewerCannotDeliverVerdict reports that AO's own reviewer -- whatever it is
// doing right now -- has no channel through which any verdict could ever reach
// AO, and therefore that waiting for one is waiting for nothing.
//
// THE STATE THIS DESCRIBES, and why the existing rule could not see it.
//
// reviewerRuntimeGone answers a different question: is the reviewer gone? For
// wf-98ab416c it was not. The reviewer was alive, owned, and sitting at its
// prompt, twenty-three hours after it had finished. It had reached a verdict, it
// had run `ao review submit`, and AO had answered 401 NOT_AUTHENTICATED --
// because on an SSO installation a pane holds no session cookie and, before
// P4-I, could not hold anything else either. So the run parked on
// review_state_ambiguous, and every recovery path refused: the reviewer was
// provably present, which is exactly the evidence that licenses doing nothing.
//
// The refusal was right. "Present" must never be read as "finished", and no
// amount of inspecting a pane's output could turn a guess about an agent's state
// into proof. What makes this rule sound is that it does not look at the agent
// at all. It looks at two durable facts AO owns:
//
//   - this installation resolves NO identity for a cookie-less request, so the
//     ONLY route by which a verdict reaches AO is an authenticated
//     reviews/submit; and
//   - AO never minted a credential for this review run -- the launch predates
//     P4-I, or its credential could not be issued -- so every submit this
//     reviewer makes is refused before it is read.
//
// Together those are proof, not inference: a reviewer that cannot authenticate
// cannot record a verdict, whatever it concluded. Waiting is then not caution,
// it is a deadlock AO built.
//
// WHY THIS CANNOT BECOME A RELAUNCH LOOP. It is reachable only from an explicit
// human resume (never a poll, a wake or a boot reconcile), and it is
// SELF-EXTINGUISHING: every reviewer launched by this build is handed a
// credential, so fact 2 is false for all of them and this answers false forever
// after. It fires for exactly the population it was written for -- reviewers
// stranded by the defect P4-I fixed -- and for nothing else.
//
// Every uncertain answer is false, on the same asymmetry the rest of this file
// obeys. A probe that errored, a reviewer whose launch was never confirmed, a
// presence AO cannot correlate to its own launch (`foreign`), one it could not
// read (`unknown`), and an installation where a cookie-less request resolves the
// bootstrap admin perfectly well (trusted-local) all answer false.
func (c *Coordinator) reviewerCannotDeliverVerdict(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, reviewRun domain.ReviewRun,
) bool {
	// Trusted-local: a cookie-less `ao review submit` resolves the bootstrap
	// admin and works. Nothing is blocked, so nothing is proven.
	if c.trustedLocal {
		return false
	}
	if c.reviewerIdentity == nil {
		return false
	}
	ensurer, ok := c.reviewerEnsurer()
	if !ok {
		return false
	}
	// Only a CONFIRMED launch carrying an exact incarnation. An intent that was
	// never confirmed is a different question with its own path, and a ref with
	// no instance identifies nothing this could honestly ask about -- the same
	// precondition reviewerRuntimeGone applies.
	phase, ref := c.reviewLaunchPhaseFor(ctx, run.ID, step.ID, reviewRun.ID)
	if phase != ReviewLaunchConfirmed || !ref.Known() {
		return false
	}
	obs, err := ensurer.ProbeReviewer(ctx, ref)
	if err != nil {
		return false
	}
	// AO's OWN reviewer, proven: `owned` (running) or `exited` (finished, its
	// session lingering). Both are the ownership proof LicensesTermination
	// already encodes, and it is the same proof handleReviewerCapacityStall
	// needs in order to terminate anything. `absent` is deliberately excluded:
	// that is reviewerRuntimeGone's case and it is already handled, and routing
	// it here as well would decide the same state twice by two rules.
	if !obs.Presence.LicensesTermination() {
		return false
	}
	issued, err := c.reviewerIdentity.EverIssuedForReviewRun(ctx, reviewRun.ID)
	if err != nil {
		// A ledger AO cannot read proves nothing. Failing closed here keeps a
		// transient database error from being read as "this reviewer was never
		// given an identity", which would terminate a live, working reviewer.
		return false
	}
	return !issued
}
