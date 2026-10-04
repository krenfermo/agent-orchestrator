package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// review_depth.go — resolving, recording and escalating how deep one review
// step's review pass runs.
//
// The vocabulary, the floors and the clamp live in domain/review_depth.go and
// are pure. This file is the part that touches AO's durable state: it reads the
// risk decision ReviewPolicy already persisted, maps it to a risk tier, resolves
// the run's frozen depth request against that tier's floor, and writes the
// answer down as its own append-only checkpoint — exactly the way
// review_policy_dispatch.go writes the decision this one is derived from.
//
// Three properties are load-bearing:
//
//   - ONE risk evaluation. The tier is a reading of the ReviewPolicyDecision
//     that already exists; there is no second risk model here that could
//     disagree with the first (review_policy.go §6's rule, applied again).
//   - The decision is DURABLE and REPLAYABLE. Requested depth, effective depth,
//     tier, the reason codes the tier was read from and the policy version are
//     all persisted, so a decision taken today stays explainable after the
//     floors change.
//   - Escalation only ever DEEPENS, and only ever affects the NEXT cycle. It
//     writes a checkpoint and changes no state transition, which is what keeps
//     it out of the review/fix state machine entirely.

const (
	// reviewDepthDecisionPhase marks the checkpoint recording which depth a
	// review step resolved to and why. Written once per review step, at cycle 1,
	// immediately after the review_policy_decision it is derived from — and
	// written for a SKIPPED decision too, so a review that never ran still says
	// what depth it would have run at.
	reviewDepthDecisionPhase = "review_depth_decision"
	// reviewDepthEscalatedPhase marks a durable escalation: a later fact raised
	// this step's depth above what cycle 1 resolved. Append-only; the presence
	// of any such row for the step is what the next dispatch reads.
	reviewDepthEscalatedPhase = "review_depth_escalated"
)

// highRiskReviewReasons are the ReviewPolicy reasons that make a change
// non-degradable: no selection, default or configuration may review one of
// these with less than a full independent pass.
//
// It is a map rather than a slice so the mapping below is a lookup rather than
// a scan, and so adding a reason to the vocabulary without classifying it fails
// safe (see ReviewRiskTierFor: an unclassified reason is treated as high).
var highRiskReviewReasons = map[ReviewReason]struct{}{
	ReasonAuthOrSecurityPath:     {},
	ReasonPaymentsOrBillingPath:  {},
	ReasonMigrationOrSchemaPath:  {},
	ReasonConcurrencyPath:        {},
	ReasonInfraOrCICDPath:        {},
	ReasonPublicAPIPath:          {},
	ReasonDependencyConfigChange: {},
	ReasonDestructiveIntent:      {},
}

// standardRiskReviewReasons are the reasons that mean "this must be looked at,
// and a bounded look is enough unless something escalates it".
var standardRiskReviewReasons = map[ReviewReason]struct{}{
	ReasonLargeOrMultiModule:    {},
	ReasonPriorProviderAttempts: {},
	ReasonAmbiguousAcceptance:   {},
	ReasonInsufficientVerify:    {},
	ReasonDefaultConservative:   {},
	ReasonNoChangedFiles:        {},
	// P5-A: a change set AO could not establish. Standard rather than high --
	// it keeps a review without claiming the change is sensitive, which is a
	// claim AO has no basis for either way -- and it is relief-blocking, so it
	// can never buy a cheaper review.
	ReasonUnprovableChangeSet: {},
}

// lowRiskReviewReasons are the reasons AO can already prove safe
// deterministically. They are exactly the two reasons EvaluateReviewPolicy
// resolves to ReviewSkipped, which is what makes depth `none` incapable of
// skipping a review AO would otherwise have run. TestReviewRiskTierLowMatchesSkip
// pins that equivalence so a future edit to either table cannot break it
// silently.
var lowRiskReviewReasons = map[ReviewReason]struct{}{
	ReasonDocsOnlyChange:         {},
	ReasonExactContentSingleFile: {},
}

// ReviewRiskTierFor derives a change's risk tier from the ReviewPolicyDecision
// AO already computed. Pure: no IO, no clock, no model call.
//
// The tier is the MAXIMUM over every recorded reason, so one auth path among
// forty docs files is still high. Two fail-safe rules make it impossible for a
// gap in the tables to produce a cheaper review than the facts justify:
//
//   - a reason that is in none of the three tables is treated as HIGH, so a
//     reason added to the vocabulary without being classified here fails toward
//     scrutiny rather than away from it;
//   - a decision that is REQUIRED can never come back low, whatever its reasons
//     say. REQUIRED and low are contradictory by construction today, and if a
//     future edit makes them reachable together the contradiction must resolve
//     in favour of reviewing.
func ReviewRiskTierFor(decision ReviewPolicyDecision) (domain.ReviewRiskTier, []string) {
	reasons := make([]string, 0, len(decision.Reasons))
	tier := domain.ReviewRiskLow
	if len(decision.Reasons) == 0 {
		// A decision with no reasons explains nothing, and an unexplained
		// decision is not evidence that a change is cheap.
		return domain.ReviewRiskHigh, nil
	}
	for _, r := range decision.Reasons {
		reasons = append(reasons, string(r))
		switch {
		case inReasonSet(highRiskReviewReasons, r):
			tier = domain.DeeperTier(tier, domain.ReviewRiskHigh)
		case inReasonSet(standardRiskReviewReasons, r):
			tier = domain.DeeperTier(tier, domain.ReviewRiskStandard)
		case inReasonSet(lowRiskReviewReasons, r):
			tier = domain.DeeperTier(tier, domain.ReviewRiskLow)
		default:
			tier = domain.DeeperTier(tier, domain.ReviewRiskHigh)
		}
	}
	if decision.Decision == ReviewRequired {
		tier = domain.DeeperTier(tier, domain.ReviewRiskStandard)
	}
	return tier, reasons
}

func inReasonSet(set map[ReviewReason]struct{}, r ReviewReason) bool {
	_, ok := set[r]
	return ok
}

// resolveReviewDepthDecision resolves and durably records the depth this review
// step will run at. Called exactly once per review step, from cycle 1, right
// after the review_policy_decision it reads.
//
// A failure to persist is returned rather than swallowed: a review that ran at
// a depth AO cannot explain afterwards is the thing this checkpoint exists to
// make impossible.
func (c *Coordinator) resolveReviewDepthDecision(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	reviewStep domain.WorkflowStep,
	policyDecision ReviewPolicyDecision,
) (domain.ReviewDepthDecision, error) {
	tier, reasons := ReviewRiskTierFor(policyDecision)
	snapshot := policyForRun(run).EffectiveReviewDepthPolicy()
	decision := domain.ResolveReviewDepth(snapshot, tier, reasons, c.clock())
	if err := c.persistReviewDepthDecision(ctx, run, reviewStep, decision, reviewDepthDecisionPhase); err != nil {
		return decision, err
	}
	return decision, nil
}

// persistReviewDepthDecision writes one depth decision as its own checkpoint
// row, following persistReviewPolicyDecision exactly.
func (c *Coordinator) persistReviewDepthDecision(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	reviewStep domain.WorkflowStep,
	decision domain.ReviewDepthDecision,
	phase string,
) error {
	payload, err := json.Marshal(decision)
	if err != nil {
		return err
	}
	stepID := reviewStep.ID
	_, err = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		RetryState:     string(payload),
		NextAction:     "review_depth: " + string(decision.Effective),
		DurablePhase:   phase,
		PayloadVersion: domain.ReviewDepthPolicyVersion,
		CreatedAt:      c.clock(),
	})
	return err
}

// decodeReviewDepthDecision unmarshals a depth checkpoint's RetryState. Returns
// ok=false on any unmarshal error rather than guessing — an unreadable
// checkpoint must never be silently read as a particular depth.
func decodeReviewDepthDecision(retryState string) (domain.ReviewDepthDecision, bool) {
	var decision domain.ReviewDepthDecision
	if retryState == "" {
		return decision, false
	}
	if err := json.Unmarshal([]byte(retryState), &decision); err != nil {
		return decision, false
	}
	return decision, decision.Recorded()
}

// DecodeReviewDepthDecisionForTest exposes decodeReviewDepthDecision to the
// external workflow_test package, mirroring
// DecodeReviewPolicyDecisionForTest.
func DecodeReviewDepthDecisionForTest(retryState string) (domain.ReviewDepthDecision, bool) {
	return decodeReviewDepthDecision(retryState)
}

// reviewDepthDecisionForStep re-reads the newest depth checkpoint recorded for
// a review step — a cycle-1 decision or a later escalation, whichever is more
// recent. Returns ok=false when there is none, which every caller must treat as
// "cannot prove a cheaper depth was authorized".
func (c *Coordinator) reviewDepthDecisionForStep(ctx stdctx.Context, runID, reviewStepID string) (domain.ReviewDepthDecision, bool) {
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return domain.ReviewDepthDecision{}, false
	}
	var latest *domain.WorkflowCheckpoint
	for i := range checkpoints {
		cp := &checkpoints[i]
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != reviewStepID {
			continue
		}
		if cp.DurablePhase != reviewDepthDecisionPhase && cp.DurablePhase != reviewDepthEscalatedPhase {
			continue
		}
		if latest == nil || cp.CreatedAt.After(latest.CreatedAt) {
			latest = cp
		}
	}
	if latest == nil {
		return domain.ReviewDepthDecision{}, false
	}
	return decodeReviewDepthDecision(latest.RetryState)
}

// EffectiveReviewDepth is the depth the NEXT review dispatch for this step must
// run at: the recorded decision, deepened by any escalation the durable ledger
// justifies.
//
// Every failure mode resolves to deep. A ledger AO cannot read, a decision it
// cannot decode, a fix budget it cannot count — none of those is evidence that
// a bounded review is enough, and the cost of being wrong in that direction is
// one full review pass rather than an unreviewed change.
func (c *Coordinator) EffectiveReviewDepth(ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep) domain.ReviewDepthDecision {
	decision, ok := c.reviewDepthDecisionForStep(ctx, run.ID, reviewStep.ID)
	if !ok {
		return domain.ReviewDepthDecision{
			PolicyVersion: domain.ReviewDepthPolicyVersion,
			Requested:     domain.ReviewDepthDeep,
			Effective:     domain.ReviewDepthDeep,
			Source:        domain.ReviewDepthRecovered,
			Reason:        domain.ReviewDepthReasonLegacyDefault,
			RiskTier:      domain.ReviewRiskHigh,
			DecidedAt:     c.clock(),
		}
	}
	if decision.Effective.AtLeast(domain.ReviewDepthDeep) {
		return decision
	}
	// Non-convergence. A light review that has already produced its budget of
	// changes_requested cycles is being asked a question it cannot answer, so
	// the next cycle goes deep. Fix cycles are the right unit and they are
	// already counted, deduplicated and restart-safe (fix_budget.go); an
	// unreadable count escalates, because "not known" is not "not needed".
	budget := c.fixBudget(ctx, run)
	if !budget.Known || budget.Spent >= domain.ReviewDepthMaxLightCycles {
		return domain.EscalateReviewDepth(decision, domain.ReviewDepthReasonEscalatedByCycles, c.clock())
	}
	return decision
}

// recordReviewDepthEscalation durably raises a review step's depth to deep.
//
// Best-effort by design: it is called from verdict observation, where failing
// the whole observation because an advisory checkpoint could not be written
// would turn a scrutiny improvement into a lifecycle failure. A lost escalation
// costs one shallower review cycle, and the non-convergence rule in
// EffectiveReviewDepth catches the same run one cycle later anyway.
func (c *Coordinator) recordReviewDepthEscalation(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	reviewStep domain.WorkflowStep,
	reason domain.ReviewDepthReason,
) {
	current, ok := c.reviewDepthDecisionForStep(ctx, run.ID, reviewStep.ID)
	if !ok || current.Effective.AtLeast(domain.ReviewDepthDeep) {
		// Nothing to deepen, or already as deep as it goes. Writing another row
		// would only add noise to the ledger.
		return
	}
	escalated := domain.EscalateReviewDepth(current, reason, c.clock())
	if err := c.persistReviewDepthDecision(ctx, run, reviewStep, escalated, reviewDepthEscalatedPhase); err != nil && c.log != nil {
		c.log.Warn("workflow: could not record a review depth escalation",
			"run", run.ID, "step", reviewStep.ID, "reason", string(reason), "err", err)
	}
}

// reviewBodyRequestsEscalation reports whether a reviewer used the escalation
// channel: the marker on the first line of its findings body.
//
// Deliberately a prefix match on a fixed marker rather than any attempt to read
// intent out of prose. The reviewer supplies the judgement; AO supplies the
// rule, and the rule has to be one a test can pin.
func reviewBodyRequestsEscalation(body string) bool {
	trimmed := strings.TrimLeft(body, " \t\r\n")
	return strings.HasPrefix(trimmed, domain.ReviewEscalationMarker)
}

// ApplyReviewDepthPolicy freezes a just-created run's review-depth request,
// mirroring ApplyRepairPolicy exactly: it is only accepted while the run is
// still pending, because a depth that could be lowered mid-run would let a
// review that has already been dispatched under one contract be judged under
// another.
//
// It records the REQUEST. Nothing here can lower the effective depth of any
// actual review: that is resolved per review step against the change's own risk
// tier, and the clamp is applied there.
func (c *Coordinator) ApplyReviewDepthPolicy(ctx stdctx.Context, runID string, depth domain.ReviewDepth) error {
	if !depth.Valid() {
		return fmt.Errorf("%w: %q is not a review depth", ErrInvalid, depth)
	}
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	if run.State != domain.WorkflowRunPending {
		return fmt.Errorf("%w: workflow run %q is already %s; its review depth is frozen", ErrInvalid, runID, run.State)
	}
	return c.rewriteFrozenPolicy(ctx, run, func(p *domain.WorkflowPolicy) {
		frozen := p.EffectiveReviewDepthPolicy()
		frozen.Requested = depth
		frozen.Source = domain.ReviewDepthExplicit
		frozen.At = c.clock()
		p.ReviewDepth = frozen
	})
}

// RunReviewDepthPolicy reads back a run's frozen review-depth request.
func (c *Coordinator) RunReviewDepthPolicy(ctx stdctx.Context, runID string) (domain.ReviewDepthPolicySnapshot, error) {
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return domain.ReviewDepthPolicySnapshot{}, err
	}
	if !ok {
		return domain.ReviewDepthPolicySnapshot{}, fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	return policyForRun(run).EffectiveReviewDepthPolicy(), nil
}

// attachLightReviewEvidence fills in the evidence pack a bounded review is
// judged against. Every field is either a fact AO observed itself or a
// deterministic classification of one; nothing here comes from the worker's own
// account of its work, which is exactly the property the light prompt claims to
// the reviewer.
//
// Best-effort in the same way every other read on the dispatch path is: a fact
// AO cannot resolve is left absent, and the prompt renders the absence honestly
// ("AO could not observe a changed-file list") rather than rendering an empty
// list as "nothing changed". A reviewer told a change is empty when AO simply
// could not look is a reviewer AO misled.
func (c *Coordinator) attachLightReviewEvidence(
	ctx stdctx.Context,
	in *ReviewPromptInput,
	run domain.WorkflowRun,
	reviewStep domain.WorkflowStep,
	depth domain.ReviewDepthDecision,
) {
	in.RiskTier = depth.RiskTier
	in.RiskReasons = depth.RiskReasons

	// P5-A phase 2: the checks AO ran itself for this step, and the worker's
	// own declaration. Both come from durable rows written before this review
	// was dispatched, so a reviewer relaunched after a restart is judged
	// against exactly the facts the policy was.
	if evidence, ok := c.preReviewEvidenceForStep(ctx, run.ID, reviewStep.ID); ok {
		in.PreReviewEvidence = evidence
	}
	if report, ok := c.workReportForRun(ctx, run.ID); ok {
		in.WorkReport = report
	}

	// The changed-file list and the prior-attempt count come from the risk
	// facts already persisted at cycle 1 (ObserveWorkspace + workflow_attempts).
	// Re-observing here would be a second, possibly-divergent reading of a
	// workspace the review target is already pinned to.
	if policyDecision, ok := c.reviewPolicyDecisionForStep(ctx, run.ID, reviewStep.ID); ok {
		in.ChangedPaths = policyDecision.Facts.ChangedFilePaths
		in.ChangedFileCount = policyDecision.Facts.ChangedFileCount
		in.PriorWorkerAttempts = policyDecision.Facts.PriorWorkProviderAttempts
	}

	if artifact, err := c.planArtifactForRun(ctx, run); err == nil {
		for _, cmd := range artifact.Verification.Commands {
			in.VerifyCommands = append(in.VerifyCommands, strings.TrimSpace(cmd.Command+" "+strings.Join(cmd.Args, " ")))
		}
		for _, f := range artifact.Verification.Files {
			label := f.Path
			switch {
			case !f.Exists:
				label += " (must be absent)"
			case f.SHA256 != "":
				label += " (must match a recorded digest)"
			case f.ExactContent != nil:
				label += " (must match exact content)"
			default:
				label += " (must exist)"
			}
			in.VerifyFileChecks = append(in.VerifyFileChecks, label)
		}
	}
}
